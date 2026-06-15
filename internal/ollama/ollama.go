// Package ollama is the worker's client for a local Ollama instance. It speaks
// Ollama's REST API (https://github.com/ollama/ollama/blob/main/docs/api.md)
// over the standard library's net/http — no third-party HTTP client — and maps
// Ollama failures onto the agent-gpu structured-error contract
// (types.JobError) with stable, machine-readable codes.
//
// The client is intentionally small: it covers exactly the operations the
// worker needs for issue #11 — detect the server (Version), list local models
// (ListModels), pull a model on demand (Pull), and run streaming chat inference
// (Chat). Streaming is first-class: Chat decodes Ollama's NDJSON response
// line-by-line and invokes an emit callback per token so the worker can forward
// each delta to the server as it is produced, rather than buffering a full
// response. Long-running inference is bounded by the caller's context, not by a
// short global HTTP timeout, so a slow generation is not spuriously killed;
// cancelling the context aborts the in-flight request and stops emitting.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// Stable error codes mapped onto types.JobError.Code. They are part of the
// system error contract: the request path (#13) maps them to HTTP statuses, so
// keep them stable.
const (
	// CodeModelNotFound is reported when Ollama does not have the requested model
	// (e.g. inference or load against a model that was never pulled).
	CodeModelNotFound = "model_not_found"
	// CodeUnreachable is reported when the Ollama server cannot be contacted
	// (connection refused, DNS failure, etc.).
	CodeUnreachable = "ollama_unreachable"
	// CodeOllamaError is the catch-all for an Ollama-reported failure that does
	// not map to a more specific code.
	CodeOllamaError = "ollama_error"
	// CodeTimeout is reported when the context deadline is exceeded.
	CodeTimeout = "timeout"
	// CodeInvalidRequest is reported when Ollama rejects the request as malformed
	// (HTTP 400).
	CodeInvalidRequest = "invalid_request"
)

// DefaultBaseURL is the address a local Ollama listens on by default.
const DefaultBaseURL = "http://localhost:11434"

// Client talks to a single Ollama instance over HTTP. It is safe for concurrent
// use. Construct it with New.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client. A nil client is
// ignored. Primarily a test seam; the default client has no global timeout so
// long inference relies on the caller's context for cancellation.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New constructs a Client for the Ollama instance at baseURL (e.g.
// "http://localhost:11434"). An empty baseURL falls back to DefaultBaseURL. The
// trailing slash, if any, is trimmed so path joining is unambiguous.
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// No global Timeout: inference can legitimately run for a long time and is
		// bounded by the per-call context instead. A connect-level safety net could
		// be added via a Transport.DialContext deadline, but the request context
		// already covers abort-on-cancel.
		http: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// BaseURL returns the resolved base URL (for logging).
func (c *Client) BaseURL() string { return c.baseURL }

// versionResponse models GET /api/version.
type versionResponse struct {
	Version string `json:"version"`
}

// Version returns the running Ollama server version. It is the startup
// detection probe: a non-nil error (typically CodeUnreachable) means Ollama is
// not reachable and the worker should run degraded.
func (c *Client) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/version", nil)
	if err != nil {
		return "", c.transportError(err, "build version request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", c.transportError(err, "contact ollama")
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", c.statusError(resp, "version")
	}
	var vr versionResponse
	if err := json.NewDecoder(resp.Body).Decode(&vr); err != nil {
		return "", &types.JobError{Code: CodeOllamaError, Message: "decode version response: " + err.Error()}
	}
	return vr.Version, nil
}

// tagsResponse models GET /api/tags.
type tagsResponse struct {
	Models []tagModel `json:"models"`
}

type tagModel struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// ListModels returns the models Ollama currently has available locally,
// mapped to the domain Model type. The Ollama digest (when present) is carried
// through.
func (c *Client) ListModels(ctx context.Context) ([]types.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, c.transportError(err, "build tags request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(err, "contact ollama")
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(resp, "tags")
	}
	var tr tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, &types.JobError{Code: CodeOllamaError, Message: "decode tags response: " + err.Error()}
	}
	out := make([]types.Model, 0, len(tr.Models))
	for _, m := range tr.Models {
		if m.Name == "" {
			continue
		}
		out = append(out, types.Model{Name: m.Name, Digest: m.Digest})
	}
	return out, nil
}

// pullProgress models one NDJSON line of POST /api/pull. Ollama streams
// progress objects and signals failure with an "error" field rather than a
// non-200 status, so the whole stream must be drained and inspected.
type pullProgress struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// Pull fetches a model onto the Ollama instance, draining the NDJSON progress
// stream to completion. It returns an error if Ollama reports a failure
// (either an HTTP error status or an "error" field in the stream). Pull is
// gated by the authorization layer at the server before it is ever requested.
func (c *Client) Pull(ctx context.Context, model string) error {
	body, err := json.Marshal(map[string]any{"model": model, "stream": true})
	if err != nil {
		return &types.JobError{Code: CodeInvalidRequest, Message: "marshal pull request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return c.transportError(err, "build pull request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(err, "contact ollama")
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return c.statusError(resp, "pull")
	}
	// Drain the NDJSON progress stream; surface the first reported error.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var p pullProgress
		if err := json.Unmarshal(line, &p); err != nil {
			// A malformed progress line is not fatal on its own; keep draining.
			continue
		}
		if p.Error != "" {
			return c.errorFromMessage(p.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		return c.transportError(err, "read pull stream")
	}
	return nil
}

// chatRequest models POST /api/chat. The schema is intentionally minimal for
// the foundational stub (#11): one user message and streaming on. Richer chat
// params arrive with the OpenAI-API epic (#13).
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse models one NDJSON object of the /api/chat stream. Per-token
// objects carry Message.Content with Done false; the final object carries Done
// true plus the eval counts. An "error" field signals an in-stream failure.
type chatResponse struct {
	Message         chatMessage `json:"message"`
	Done            bool        `json:"done"`
	EvalCount       uint64      `json:"eval_count"`
	PromptEvalCount uint64      `json:"prompt_eval_count"`
	Error           string      `json:"error"`
}

// Chat runs streaming chat inference for prompt against model. It calls emit
// once per produced token delta as the NDJSON stream arrives, and returns the
// total token count (eval_count + prompt_eval_count) reported on the terminal
// object. The caller's context bounds the call: cancelling it aborts the
// in-flight HTTP request and stops emitting. A non-nil error is a *types.JobError
// with a stable code.
func (c *Client) Chat(ctx context.Context, model, prompt string, emit func(delta string)) (uint64, error) {
	reqBody := chatRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		Stream:   true,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return 0, &types.JobError{Code: CodeInvalidRequest, Message: "marshal chat request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return 0, c.transportError(err, "build chat request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, c.transportError(err, "contact ollama")
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, c.statusError(resp, "chat")
	}

	var tokens uint64
	scanner := bufio.NewScanner(resp.Body)
	// Allow long single-line tokens / large objects without erroring on the
	// default 64KiB limit; partial lines across reads are handled by Scan.
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		// Honor cancellation between lines so a cancelled job stops emitting
		// promptly even if the server keeps sending.
		if ctx.Err() != nil {
			return tokens, c.transportError(ctx.Err(), "chat cancelled")
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var cr chatResponse
		if err := json.Unmarshal(line, &cr); err != nil {
			return tokens, &types.JobError{Code: CodeOllamaError, Message: "decode chat chunk: " + err.Error()}
		}
		if cr.Error != "" {
			return tokens, c.errorFromMessage(cr.Error)
		}
		if cr.Message.Content != "" && emit != nil {
			emit(cr.Message.Content)
		}
		if cr.Done {
			tokens = cr.EvalCount + cr.PromptEvalCount
			return tokens, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return tokens, c.transportError(err, "read chat stream")
	}
	// Stream ended without a terminal done object.
	return tokens, &types.JobError{Code: CodeOllamaError, Message: "chat stream ended without completion"}
}

// transportError maps a transport/context failure to a *types.JobError with a
// stable code. Context cancellation/deadline map to CodeTimeout; everything
// else is treated as the Ollama server being unreachable.
func (c *Client) transportError(err error, op string) *types.JobError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &types.JobError{Code: CodeTimeout, Message: op + ": " + err.Error()}
	}
	if errors.Is(err, context.Canceled) {
		// A cancelled inference is not a server fault; report it as a timeout-class
		// abort so the waiter resolves with a stable code rather than hanging.
		return &types.JobError{Code: CodeTimeout, Message: op + ": " + err.Error()}
	}
	return &types.JobError{Code: CodeUnreachable, Message: op + ": " + err.Error()}
}

// statusError maps a non-200 HTTP response to a *types.JobError. It reads
// (a bounded amount of) the body so Ollama's error message is preserved, then
// classifies by status code.
func (c *Client) statusError(resp *http.Response, op string) *types.JobError {
	msg := readErrorBody(resp.Body)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return &types.JobError{Code: CodeModelNotFound, Message: errMessage(op, msg, resp.StatusCode)}
	case http.StatusBadRequest:
		return &types.JobError{Code: CodeInvalidRequest, Message: errMessage(op, msg, resp.StatusCode)}
	default:
		// A model-not-found may also surface as a generic error body; classify on
		// content as a fallback.
		if isModelNotFound(msg) {
			return &types.JobError{Code: CodeModelNotFound, Message: errMessage(op, msg, resp.StatusCode)}
		}
		return &types.JobError{Code: CodeOllamaError, Message: errMessage(op, msg, resp.StatusCode)}
	}
}

// errorFromMessage classifies an in-stream Ollama "error" string. Ollama
// reports a missing model in the body of an otherwise-200 stream, so inspect
// the text.
func (c *Client) errorFromMessage(msg string) *types.JobError {
	if isModelNotFound(msg) {
		return &types.JobError{Code: CodeModelNotFound, Message: msg}
	}
	return &types.JobError{Code: CodeOllamaError, Message: msg}
}

// errorBody is the shape Ollama uses for JSON error responses: {"error":"..."}.
type errorBody struct {
	Error string `json:"error"`
}

// readErrorBody reads up to 8KiB of an error response body and extracts
// Ollama's "error" field when the body is JSON, falling back to the raw text.
func readErrorBody(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 8<<10))
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var eb errorBody
	if err := json.Unmarshal(trimmed, &eb); err == nil && eb.Error != "" {
		return eb.Error
	}
	return string(trimmed)
}

func errMessage(op, msg string, status int) string {
	if msg != "" {
		return fmt.Sprintf("ollama %s: %s", op, msg)
	}
	return fmt.Sprintf("ollama %s: status %d", op, status)
}

// isModelNotFound reports whether an Ollama error string indicates a missing
// model.
func isModelNotFound(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "not found") || strings.Contains(m, "no such") || strings.Contains(m, "try pulling")
}

// drainClose drains and closes a response body so the underlying connection can
// be reused, ignoring errors.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}
