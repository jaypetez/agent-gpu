// Command go-client is a small, dependency-free example client for the
// agent-gpu OpenAI-compatible HTTP API. It lists the models a key may use,
// then runs a chat completion (buffered or streamed) end to end, surfacing the
// server's typed error envelope and Retry-After hints exactly the way a real
// client should.
//
// It is deliberately stdlib-only (net/http, encoding/json, bufio, …) so it can
// be copied out of the repo and `go run` on its own — see the package doc on
// go.mod. The HTTP/SSE plumbing lives here in client.go; flag parsing and the
// run flow live in main.go.
//
// # The agent-gpu API in one paragraph
//
// agent-gpu speaks the OpenAI REST surface under a /v1 base path. Every request
// carries an `Authorization: Bearer agpu_<keyid>_<secret>` token; the server
// authenticates it, applies the key's per-model permissions and quotas, and
// schedules the job onto a worker. GET /v1/models returns the (permission- and
// availability-filtered) catalog; POST /v1/chat/completions returns a single
// chat.completion or, with "stream":true, a Server-Sent Events stream of
// chat.completion.chunk frames terminated by the literal `data: [DONE]`. A
// non-2xx response is a JSON envelope {"error":{"message","code","type"}}; a
// failure that happens AFTER a stream has started arrives as a terminal SSE
// frame {"error":{…}} (then [DONE]) instead, which this client detects and
// surfaces rather than treating as content.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---- wire shapes (mirror internal/httpapi/{models,chat,response}.go) ----
//
// These structs decode the JSON the server emits field-for-field. Only the
// fields this example reads are modeled; the server tolerates and ignores
// unknown request fields, and json.Unmarshal ignores unknown response fields,
// so the client stays forward-compatible as the API grows.

// Model is one entry of GET /v1/models: a model id the key may use and that a
// worker currently serves. owned_by is always "agent-gpu"; created is a stable
// sentinel (0) the server reports because the OpenAI schema requires the field.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelList is the GET /v1/models envelope: {"object":"list","data":[…]}.
type modelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Message is one chat message in a request or response: a role
// ("system"/"user"/"assistant") and its text content.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the subset of the OpenAI chat/completions request this example
// sends. Other OpenAI fields (temperature, top_p, …) are accepted and ignored
// by the server, so they are simply omitted here.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// ChatResponse is a non-streaming chat.completion response. The assistant reply
// is Choices[0].Message.Content; Usage carries the token accounting.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// ChatChoice is one completion alternative; agent-gpu always returns exactly one
// (index 0). FinishReason is "stop" on a normal completion.
type ChatChoice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage is the token accounting returned with a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatChunk is one streaming SSE frame (object "chat.completion.chunk"). Each
// frame carries a delta, not the full message so far: the first frame's delta
// announces the role, later frames carry incremental content, and the terminal
// frame sets finish_reason (a JSON null on every non-terminal frame, hence the
// pointer). The choices array may be empty on a keep-alive-style frame, which
// the parser tolerates.
type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// errorEnvelope is the server's error object, returned as the whole body of a
// non-2xx response AND as a terminal SSE frame on a mid-stream failure. It is a
// strict superset of OpenAI's error object: Message is a generic human string,
// Code is a stable machine code to branch on (e.g. "unauthorized", "forbidden",
// "rate_limit_exceeded", "model_not_found", "unavailable"), and Type is the
// OpenAI error class.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ---- typed error ----

// APIError is what every non-2xx response (and every mid-stream SSE error frame)
// becomes. It carries the HTTP status, the server's machine Code and human
// Message, the OpenAI Type, and — when the server sent one on a 429/503 — the
// RetryAfter hint in seconds (0 when absent). Inspect it with errors.As.
type APIError struct {
	Status     int
	Code       string
	Message    string
	Type       string
	RetryAfter int // seconds; 0 when no Retry-After header was present
}

// Error renders the agent-gpu contract line: the message plus the machine code
// and OpenAI type, so a caller that just prints err gets an actionable string.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s (code=%s, type=%s)", msg, e.Code, e.Type)
}

// ---- client ----

// Client talks to the agent-gpu HTTP API. Construct it with NewClient; the zero
// value is not usable. It is safe for sequential use by one command-line run;
// it is not intended as a connection pool.
type Client struct {
	// baseURL is the OpenAI surface root, i.e. the ".../v1" prefix (like the
	// base_url an OpenAI SDK takes). Paths are joined onto it directly.
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient returns a Client for baseURL (the ".../v1" prefix) authenticating
// with apiKey. A trailing slash on baseURL is trimmed so path joining is
// unambiguous.
//
// The supplied *http.Client governs the non-streaming requests. Do NOT give it
// a short Timeout: a generation can legitimately run for many seconds and that
// timeout would also cap the streaming request (the #1 streaming foot-gun).
// Bound a single request with a context deadline instead — ListModels and Chat
// take a context; ChatStream deliberately does not impose one (see its doc).
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

// newRequest builds an authenticated request. body is nil for GETs; for POSTs
// it is the already-marshalled JSON payload.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// ListModels returns the catalog visible to the key: GET /v1/models. The list is
// already filtered to models the key may use AND a worker currently serves, so a
// model present here is one the key can actually call. Use it to discover or
// validate a model before sending a completion.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, apiErrorFromResponse(resp)
	}
	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	return list.Data, nil
}

// Chat runs a non-streaming chat completion: POST /v1/chat/completions with
// stream=false. It returns the decoded chat.completion, or an *APIError carrying
// the server's code/message (and Retry-After, on a 429/503) for any non-2xx.
//
// Bound the call with the context — a generation can take a while, so the caller
// supplies a generous deadline (this example uses 120s) rather than relying on a
// short http.Client.Timeout.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, apiErrorFromResponse(resp)
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	return &out, nil
}

// ChatStream runs a streaming chat completion: POST /v1/chat/completions with
// stream=true. It invokes onDelta for each incremental content delta as it
// arrives (the first frame's role-only delta carries no content and is skipped),
// and returns once the server sends the [DONE] sentinel.
//
// A failure that happens BEFORE the stream starts (auth, permissions, no worker
// for the model, …) arrives as an ordinary non-2xx JSON body and is returned as
// an *APIError. A failure AFTER the stream has started arrives as a terminal SSE
// error frame ({"error":{…}} then [DONE]); ChatStream detects that frame and
// returns it as an *APIError too, so a truncated answer is never mistaken for a
// clean completion. Any content already delivered to onDelta stays delivered.
//
// Timeouts: ChatStream takes a context but the caller should NOT give it a short
// deadline — a long generation streaming token-by-token must not be cut off
// mid-flight. Use cancellation (e.g. on SIGINT) to stop early instead. The
// underlying *http.Client must likewise not have a short overall Timeout, which
// would abort the read regardless of the context.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) error {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal chat request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return err
	}
	// The streaming endpoint replies with text/event-stream; advertise it.
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)

	// A pre-stream failure is a normal JSON error with a non-2xx status.
	if resp.StatusCode != http.StatusOK {
		return apiErrorFromResponse(resp)
	}
	return parseSSE(resp.Body, onDelta)
}

// parseSSE reads an OpenAI-style Server-Sent Events stream from r and drives
// onDelta with each content delta. It is split out from ChatStream so the SSE
// state machine can be unit-tested directly.
//
// The parser is deliberately defensive, matching the framing the server emits:
//   - it reads line by line and acts ONLY on lines beginning with the literal
//     "data: " prefix, stripping exactly that prefix;
//   - "data: [DONE]" terminates the stream cleanly (returns nil);
//   - blank separator lines and any non-data line (e.g. a ":" comment / keep-
//     alive, or an "event:" line) are skipped;
//   - each data payload is JSON-unmarshalled and disambiguated: a payload with a
//     top-level "error" is a mid-stream failure (returned as an *APIError); any
//     other payload is a chat.completion.chunk whose first choice's delta content
//     (if any) is forwarded. An empty choices array is tolerated.
//
// The scanner's buffer is enlarged because a single SSE data line can exceed
// bufio.Scanner's 64 KiB default (e.g. a chunk carrying tool-call JSON).
func parseSSE(r io.Reader, onDelta func(string)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	const dataPrefix = "data: "
	for scanner.Scan() {
		line := scanner.Text()
		// Only data lines carry payloads; skip blanks, ":" comments, "event:" etc.
		if !strings.HasPrefix(line, dataPrefix) {
			continue
		}
		payload := strings.TrimPrefix(line, dataPrefix)
		if payload == "[DONE]" {
			return nil
		}

		// A mid-stream failure is delivered as a frame with a top-level "error".
		// Try that shape first; a chunk has no non-empty top-level error, so a
		// successful decode with a populated message means this is the error frame.
		var env errorEnvelope
		if err := json.Unmarshal([]byte(payload), &env); err == nil && env.Error.Message != "" {
			return &APIError{
				// The mid-stream frame carries no HTTP status of its own; record the
				// transport status that applies (the stream itself was a 200).
				Status:  http.StatusOK,
				Code:    env.Error.Code,
				Message: env.Error.Message,
				Type:    env.Error.Type,
			}
		}

		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("decode stream chunk: %w", err)
		}
		if len(chunk.Choices) == 0 {
			continue // tolerate a delta-less keep-alive-style frame
		}
		if delta := chunk.Choices[0].Delta.Content; delta != "" {
			onDelta(delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	// The stream ended without an explicit [DONE]. Treat it as a clean EOF: any
	// content was already delivered, and the server normally always sends [DONE].
	return nil
}

// ---- helpers ----

// apiErrorFromResponse builds an *APIError from a non-2xx response: it decodes
// the {"error":{…}} envelope for the code/message/type and reads any Retry-After
// header (present on a 429, sometimes a 503). A body that is missing or not the
// expected envelope still yields a usable error keyed off the status.
func apiErrorFromResponse(resp *http.Response) *APIError {
	e := &APIError{
		Status:     resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
	body, _ := io.ReadAll(resp.Body)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
		e.Type = env.Error.Type
	} else {
		// No (or unparseable) envelope: fall back to the status text so the error
		// is still meaningful.
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// parseRetryAfter parses the integer-seconds form of the Retry-After header the
// server emits. It returns 0 for an empty or non-integer value (the server only
// ever sends the delta-seconds form, never an HTTP-date).
func parseRetryAfter(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// drainClose drains and closes a response body so the underlying connection can
// be reused, and is safe to defer immediately after a successful Do.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

// asAPIError reports the *APIError in err's chain, if any, so callers (and tests)
// can branch on the server's code / Retry-After. It is a thin errors.As wrapper.
func asAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}
