package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient returns a Client pointed at srv, reusing the httptest server's
// own *http.Client so requests stay in-process. No real server or model needed.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(srv.URL+"/v1", "agpu_test_secret", srv.Client())
}

// readJSON decodes a request body into a generic map so a handler can assert on
// fields the client sent (e.g. the stream flag), failing the test on bad JSON.
func readJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	m := map[string]any{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("decode request body %q: %v", b, err)
		}
	}
	return m
}

// TestListModelsParsesCatalog verifies ListModels decodes the OpenAI list
// envelope and sends the Bearer token.
func TestListModelsParsesCatalog(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"qwen2:0.5b","object":"model","created":0,"owned_by":"agent-gpu"},
			{"id":"llama3","object":"model","created":0,"owned_by":"agent-gpu"}
		]}`))
	}))
	defer srv.Close()

	models, err := newTestClient(t, srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer agpu_test_secret" {
		t.Errorf("auth = %q, want Bearer agpu_test_secret", gotAuth)
	}
	if len(models) != 2 || models[0].ID != "qwen2:0.5b" || models[1].ID != "llama3" {
		t.Fatalf("models = %+v, want [qwen2:0.5b llama3]", models)
	}
	if models[0].OwnedBy != "agent-gpu" {
		t.Errorf("owned_by = %q, want agent-gpu", models[0].OwnedBy)
	}
}

// TestChatParsesCompletion verifies a non-streaming chat completion is decoded,
// including the assistant content and usage, and that stream=false is sent.
func TestChatParsesCompletion(t *testing.T) {
	var gotStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		// Reflect the request's stream flag back via a marker so we can assert it.
		body := readJSON(t, r)
		gotStream, _ = body["stream"].(bool)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"qwen2:0.5b",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello there."},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}
		}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:    "qwen2:0.5b",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   true, // Chat must force this to false
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotStream {
		t.Error("Chat sent stream=true, want stream=false")
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello there." {
		t.Fatalf("choices = %+v, want one with content 'Hello there.'", resp.Choices)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 10 || resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v, want prompt=7 completion=3 total=10", resp.Usage)
	}
}

// TestChatStreamAccumulatesDeltas verifies the SSE parser forwards each content
// delta in order, skips the role-only first frame, tolerates an empty-choices
// frame, and stops at [DONE].
func TestChatStreamAccumulatesDeltas(t *testing.T) {
	var gotStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		gotStream, _ = body["stream"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		// role-only first frame, two content frames, an empty-choices keep-alive,
		// a terminal frame with finish_reason, then [DONE]. Interleaved blank
		// separator lines and a ":" comment must be ignored.
		_, _ = w.Write([]byte(strings.Join([]string{
			`: keep-alive comment`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2:0.5b","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			``,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2:0.5b","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			``,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2:0.5b","choices":[]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2:0.5b","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			``,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen2:0.5b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")))
	}))
	defer srv.Close()

	var sb strings.Builder
	err := newTestClient(t, srv).ChatStream(context.Background(),
		ChatRequest{Model: "qwen2:0.5b", Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d string) { sb.WriteString(d) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if !gotStream {
		t.Error("ChatStream sent stream=false, want stream=true")
	}
	if got := sb.String(); got != "Hello world" {
		t.Errorf("accumulated = %q, want %q", got, "Hello world")
	}
}

// TestChatStreamSurfacesMidStreamError verifies that a terminal SSE error frame
// is returned as an *APIError (not treated as content), after any earlier
// content was already delivered.
func TestChatStreamSurfacesMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`,
			``,
			`data: {"error":{"message":"inference failed","code":"internal_error","type":"server_error"}}`,
			`data: [DONE]`,
			``,
		}, "\n")))
	}))
	defer srv.Close()

	var sb strings.Builder
	err := newTestClient(t, srv).ChatStream(context.Background(),
		ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d string) { sb.WriteString(d) })
	if err == nil {
		t.Fatal("ChatStream returned nil, want a mid-stream error")
	}
	apiErr, ok := asAPIError(err)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Code != "internal_error" || apiErr.Type != "server_error" {
		t.Errorf("error = %+v, want code=internal_error type=server_error", apiErr)
	}
	// The content delivered before the failure must still have reached onDelta.
	if got := sb.String(); got != "partial" {
		t.Errorf("delivered content = %q, want %q", got, "partial")
	}
}

// TestChatErrorEnvelope verifies a non-2xx JSON error body is decoded into an
// *APIError carrying the server's code, message, and OpenAI type.
func TestChatErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"model not permitted","code":"forbidden","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Chat(context.Background(),
		ChatRequest{Model: "secret", Messages: []Message{{Role: "user", Content: "hi"}}})
	apiErr, ok := asAPIError(err)
	if !ok {
		t.Fatalf("error type = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", apiErr.Status)
	}
	if apiErr.Code != "forbidden" || apiErr.Type != "invalid_request_error" {
		t.Errorf("error = %+v, want code=forbidden type=invalid_request_error", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "model not permitted") {
		t.Errorf("Error() = %q, want it to contain the message", apiErr.Error())
	}
}

// TestRetryAfterIsReadOn429 verifies the Retry-After header is parsed off a 429
// into APIError.RetryAfter.
func TestRetryAfterIsReadOn429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":"rate_limit_exceeded","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListModels(context.Background())
	apiErr, ok := asAPIError(err)
	if !ok {
		t.Fatalf("error type = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.Status)
	}
	if apiErr.RetryAfter != 12 {
		t.Errorf("RetryAfter = %d, want 12", apiErr.RetryAfter)
	}
	if apiErr.Code != "rate_limit_exceeded" {
		t.Errorf("code = %q, want rate_limit_exceeded", apiErr.Code)
	}
}

// TestUnavailableModelReturns503 verifies the 503 "unavailable" path (a model no
// worker serves) is surfaced as an *APIError, with a Retry-After when present.
func TestUnavailableModelReturns503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"no worker available for the requested model","code":"unavailable","type":"server_error"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Chat(context.Background(),
		ChatRequest{Model: "ghost", Messages: []Message{{Role: "user", Content: "hi"}}})
	apiErr, ok := asAPIError(err)
	if !ok {
		t.Fatalf("error type = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "unavailable" {
		t.Errorf("error = %+v, want status=503 code=unavailable", apiErr)
	}
	if apiErr.RetryAfter != 5 {
		t.Errorf("RetryAfter = %d, want 5", apiErr.RetryAfter)
	}
}

// TestParseSSESkipsNonDataAndUnknownFields exercises the bare parser directly:
// it must ignore "event:" lines and blank lines, tolerate unknown JSON fields,
// and stop at [DONE].
func TestParseSSESkipsNonDataAndUnknownFields(t *testing.T) {
	stream := strings.Join([]string{
		`event: message`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"},"logprobs":null}]}`,
		``,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"b"}}]}`,
		`data: [DONE]`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"IGNORED"}}]}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := parseSSE(strings.NewReader(stream), func(d string) { sb.WriteString(d) }); err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if got := sb.String(); got != "ab" {
		t.Errorf("accumulated = %q, want %q (content after [DONE] must be ignored)", got, "ab")
	}
}

// TestNoAPIErrorForTransport confirms asAPIError reports false for a non-API
// error (e.g. a dial failure), so callers print it verbatim instead.
func TestNoAPIErrorForTransport(t *testing.T) {
	// Point at a closed server to force a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(url+"/v1", "agpu_x", &http.Client{})
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected a transport error from a closed server")
	}
	if _, ok := asAPIError(err); ok {
		t.Errorf("asAPIError = true for a transport error %v, want false", err)
	}
}
