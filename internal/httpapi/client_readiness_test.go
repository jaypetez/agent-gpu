package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// These tests cover the OpenAI-compatibility & robustness fixes shipped for the
// published example client (#13 client-readiness): fail-fast on an unserved model
// (Fix 1, HTTP mapping), mid-stream SSE error frames (Fix 2), the OpenAI `type`
// field on the error envelope (Fix 3), GET /v1/models/{model} (Fix 4),
// panic-recovery middleware (Fix 5), and the small OpenAI-compat fixes — 405
// Allow header and array prompt (Fix 6).

// ---- shared fakes ----

// errEngine is an inferenceEngine that fails the non-streaming submit with a
// fixed error and, for streams, emits any pre-deltas then a terminal chunk
// carrying streamErr (so the mid-stream error path is exercised). A nil
// streamErr makes the stream a clean single Done chunk.
type errEngine struct {
	submitErr error
	deltas    []string
	streamErr *types.JobError
}

func (e *errEngine) SubmitAuthorizedJob(context.Context, store.APIKey, types.Job) (types.JobResult, error) {
	return types.JobResult{}, e.submitErr
}

func (e *errEngine) SubmitAuthorizedJobStream(_ context.Context, _ store.APIKey, job types.Job) (<-chan types.JobChunk, error) {
	if e.submitErr != nil {
		return nil, e.submitErr
	}
	ch := make(chan types.JobChunk, len(e.deltas)+1)
	for _, d := range e.deltas {
		ch <- types.JobChunk{JobID: job.ID, Delta: d}
	}
	if e.streamErr != nil {
		// A mid-stream failure: the terminal chunk carries the error, no clean
		// finish_reason.
		ch <- types.JobChunk{JobID: job.ID, Done: true, Err: e.streamErr}
	} else {
		ch <- types.JobChunk{JobID: job.ID, Done: true, FinishReason: "stop"}
	}
	close(ch)
	return ch, nil
}

// readinessServer builds an internal Server wired to the given engine and a fleet
// serving the named models, with a real auth service over an in-memory store and
// a discarding logger. It returns the server and an admin token (admin sees every
// model). models seeds a single online worker's catalog.
func readinessServer(t *testing.T, eng inferenceEngine, models ...string) (*Server, string) {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	ms := make([]types.Model, 0, len(models))
	for _, m := range models {
		ms = append(ms, types.Model{Name: m})
	}
	s := &Server{
		fleet:  &fakeFleet{snapshot: []types.Worker{onlineWorker("w1", ms...)}},
		engine: eng,
		auth:   authSvc,
		authz:  az,
		log:    discard,
	}
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return s, token
}

// postJSON issues an authenticated POST with a JSON body through the routed
// handler and returns the recorder.
func postJSON(t *testing.T, s *Server, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeErr decodes an error envelope from a recorder.
func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return got
}

// parseSSE splits an SSE body into its data frames (excluding the [DONE]
// sentinel) and reports whether the [DONE] sentinel was seen.
func parseSSE(t *testing.T, body string) (frames []string, sawDone bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		frames = append(frames, payload)
	}
	return frames, sawDone
}

// ---- Fix 1 (HTTP mapping): fail fast on an unserved model ----

// TestChatUnservedModelReturns503 proves a non-streaming chat request for a model
// no worker serves returns 503 with code "unavailable" and the model-specific
// message — promptly, not after a hang (the engine returns ErrModelUnavailable
// synchronously, so the handler must not block).
func TestChatUnservedModelReturns503(t *testing.T) {
	s, token := readinessServer(t, &errEngine{submitErr: server.ErrModelUnavailable}, "llama3")

	rec := postJSON(t, s, "/v1/chat/completions", token,
		`{"model":"ghost","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeErr(t, rec)
	if got.Error.Code != "unavailable" {
		t.Errorf("code = %q, want unavailable", got.Error.Code)
	}
	if got.Error.Message != "no worker available for the requested model" {
		t.Errorf("message = %q, want the model-specific unavailable message", got.Error.Message)
	}
	// Distinct from the queue-full message so a client can tell the two apart.
	if got.Error.Message == "no capacity available" {
		t.Errorf("message must differ from the queue-full message")
	}
}

// TestCompletionUnservedModelReturns503 is the /v1/completions counterpart.
func TestCompletionUnservedModelReturns503(t *testing.T) {
	s, token := readinessServer(t, &errEngine{submitErr: server.ErrModelUnavailable}, "llama3")

	rec := postJSON(t, s, "/v1/completions", token, `{"model":"ghost","prompt":"hi"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec); got.Error.Code != "unavailable" {
		t.Errorf("code = %q, want unavailable", got.Error.Code)
	}
}

// ---- Fix 2: mid-stream errors surface as an SSE error frame, not a fake reason ----

// TestChatStreamMidErrorEmitsErrorFrame proves a mid-stream worker failure is
// surfaced to the client as a `data: {"error":...}` frame (with a code) followed
// by [DONE], and that NO chunk carries finish_reason "error" (a truncated answer
// must not look like a clean completion).
func TestChatStreamMidErrorEmitsErrorFrame(t *testing.T) {
	eng := &errEngine{
		deltas:    []string{"par", "tial"},
		streamErr: &types.JobError{Code: "worker_disconnected", Message: "secret internal detail"},
	}
	s, token := readinessServer(t, eng, "llama3")

	rec := postJSON(t, s, "/v1/chat/completions", token,
		`{"model":"llama3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream began); body=%s", rec.Code, rec.Body.String())
	}

	frames, sawDone := parseSSE(t, rec.Body.String())
	if !sawDone {
		t.Fatalf("stream did not end with [DONE]; body=%s", rec.Body.String())
	}

	var sawErrorFrame bool
	for _, f := range frames {
		// No frame may carry finish_reason "error".
		if strings.Contains(f, `"finish_reason":"error"`) {
			t.Errorf("a chunk carried finish_reason \"error\": %s", f)
		}
		var env errorBody
		if err := json.Unmarshal([]byte(f), &env); err == nil && env.Error.Code != "" {
			sawErrorFrame = true
			if env.Error.Code != "worker_disconnected" {
				t.Errorf("error frame code = %q, want worker_disconnected", env.Error.Code)
			}
			if env.Error.Type != "server_error" {
				t.Errorf("error frame type = %q, want server_error", env.Error.Type)
			}
			// The worker's internal message must NOT leak to the client.
			if strings.Contains(env.Error.Message, "secret internal detail") {
				t.Errorf("error frame leaked internal detail: %q", env.Error.Message)
			}
		}
	}
	if !sawErrorFrame {
		t.Fatalf("no SSE error frame found in stream; frames=%v", frames)
	}
}

// TestCompletionStreamMidErrorEmitsErrorFrame is the /v1/completions counterpart.
func TestCompletionStreamMidErrorEmitsErrorFrame(t *testing.T) {
	eng := &errEngine{
		deltas:    []string{"oops"},
		streamErr: &types.JobError{Code: "internal_error", Message: "boom"},
	}
	s, token := readinessServer(t, eng, "llama3")

	rec := postJSON(t, s, "/v1/completions", token, `{"model":"llama3","stream":true,"prompt":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	frames, sawDone := parseSSE(t, rec.Body.String())
	if !sawDone {
		t.Fatalf("stream did not end with [DONE]; body=%s", rec.Body.String())
	}
	var sawErrorFrame bool
	for _, f := range frames {
		if strings.Contains(f, `"finish_reason":"error"`) {
			t.Errorf("a chunk carried finish_reason \"error\": %s", f)
		}
		var env errorBody
		if err := json.Unmarshal([]byte(f), &env); err == nil && env.Error.Code != "" {
			sawErrorFrame = true
		}
	}
	if !sawErrorFrame {
		t.Fatalf("no SSE error frame found in completion stream; frames=%v", frames)
	}
}

// TestStreamErrorBodyCodeFallback proves a terminal error with no code falls back
// to internal_error (and never panics on a nil JobError).
func TestStreamErrorBodyCodeFallback(t *testing.T) {
	if got := streamErrorBody(&types.JobError{}).Error.Code; got != "internal_error" {
		t.Errorf("empty-code fallback = %q, want internal_error", got)
	}
	if got := streamErrorBody(nil).Error.Code; got != "internal_error" {
		t.Errorf("nil JobError fallback = %q, want internal_error", got)
	}
	if got := streamErrorBody(nil).Error.Type; got != "server_error" {
		t.Errorf("type = %q, want server_error", got)
	}
}

// ---- Fix 3: error envelope carries the OpenAI `type` (superset) ----

// TestErrorEnvelopeCarriesType proves a representative set of errors now carry the
// OpenAI `type` while code/message are unchanged (additive superset).
func TestErrorEnvelopeCarriesType(t *testing.T) {
	// 401 (no token): authentication_error.
	s, token := readinessServer(t, &fakeEngine{}, "llama3")
	rec := do(t, s, "/v1/models", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", rec.Code)
	}
	got := decodeErr(t, rec)
	if got.Error.Type != "authentication_error" {
		t.Errorf("401 type = %q, want authentication_error", got.Error.Type)
	}
	if got.Error.Code != "unauthorized" {
		t.Errorf("401 code = %q, want unauthorized (unchanged)", got.Error.Code)
	}

	// 400 (malformed body): invalid_request_error.
	rec = postJSON(t, s, "/v1/chat/completions", token, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	got = decodeErr(t, rec)
	if got.Error.Type != "invalid_request_error" {
		t.Errorf("400 type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Code != "invalid_request_error" {
		t.Errorf("400 code = %q, want invalid_request_error (unchanged)", got.Error.Code)
	}

	// 503 (unserved model): server_error.
	s503, token503 := readinessServer(t, &errEngine{submitErr: server.ErrModelUnavailable}, "llama3")
	rec = postJSON(t, s503, "/v1/chat/completions", token503,
		`{"model":"ghost","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("503 status = %d, want 503", rec.Code)
	}
	if got := decodeErr(t, rec); got.Error.Type != "server_error" {
		t.Errorf("503 type = %q, want server_error", got.Error.Type)
	}
}

// TestOpenAIErrorTypeMapping unit-tests the status→type mapping directly.
func TestOpenAIErrorTypeMapping(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:          "invalid_request_error",
		http.StatusUnauthorized:        "authentication_error",
		http.StatusForbidden:           "invalid_request_error",
		http.StatusNotFound:            "invalid_request_error",
		http.StatusMethodNotAllowed:    "invalid_request_error",
		http.StatusConflict:            "invalid_request_error",
		http.StatusUnprocessableEntity: "invalid_request_error",
		http.StatusTooManyRequests:     "rate_limit_error",
		http.StatusServiceUnavailable:  "server_error",
		http.StatusInternalServerError: "server_error",
	}
	for status, want := range cases {
		if got := openAIErrorType(status); got != want {
			t.Errorf("openAIErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

// ---- Fix 4: GET /v1/models/{model} (retrieve) ----

// TestRetrieveModelVisible proves a visible model returns 200 with the single
// OpenAI model object.
func TestRetrieveModelVisible(t *testing.T) {
	s, token := readinessServer(t, &fakeEngine{}, "qwen2:0.5b")

	rec := do(t, s, "/v1/models/qwen2:0.5b", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m openAIModel
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode model %q: %v", rec.Body.String(), err)
	}
	if m.ID != "qwen2:0.5b" || m.Object != "model" || m.OwnedBy != "agent-gpu" {
		t.Errorf("model fields wrong: %+v", m)
	}
	if m.Created != openAICreated {
		t.Errorf("created = %d, want %d", m.Created, openAICreated)
	}
}

// TestRetrieveModelNamespacedTag proves the {model...} wildcard captures a
// slash-namespaced tag whole.
func TestRetrieveModelNamespacedTag(t *testing.T) {
	s, token := readinessServer(t, &fakeEngine{}, "library/llama3:8b")

	rec := do(t, s, "/v1/models/library/llama3:8b", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m openAIModel
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.ID != "library/llama3:8b" {
		t.Errorf("id = %q, want library/llama3:8b (whole path captured)", m.ID)
	}
}

// TestRetrieveUnknownModel404 proves an unknown/forbidden model returns 404 with
// code model_not_found and type invalid_request_error.
func TestRetrieveUnknownModel404(t *testing.T) {
	s, token := readinessServer(t, &fakeEngine{}, "llama3")

	rec := do(t, s, "/v1/models/does-not-exist", token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeErr(t, rec)
	if got.Error.Code != "model_not_found" {
		t.Errorf("code = %q, want model_not_found", got.Error.Code)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", got.Error.Type)
	}
}

// TestRetrieveModelUnauth proves the retrieve endpoint requires auth (401) and
// never confirms a model's existence to an unauthenticated caller.
func TestRetrieveModelUnauth(t *testing.T) {
	s, _ := readinessServer(t, &fakeEngine{}, "llama3")

	rec := do(t, s, "/v1/models/llama3", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListModelsBarePathStillWins proves registering the retrieve wildcard did not
// shadow the bare /v1/models list route.
func TestListModelsBarePathStillWins(t *testing.T) {
	s, token := readinessServer(t, &fakeEngine{}, "llama3")

	rec := do(t, s, "/v1/models", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list openAIModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("bare /v1/models did not return the list object: %+v", list)
	}
}

// ---- Fix 5: panic-recovery middleware ----

// TestRecoverMiddlewareReturns500 proves a handler panic becomes a clean 500 JSON
// error envelope (not a dropped connection), with the X-Request-Id header set.
func TestRecoverMiddlewareReturns500(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{log: discard}

	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	// Mirror the production chain so request_id is on the context and the
	// statusRecorder tracks whether the response started.
	h := s.metricsMiddleware(s.requestIDMiddleware(s.recoverMiddleware(panicky)))

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	got := decodeErr(t, rec)
	if got.Error.Code != "internal_error" {
		t.Errorf("code = %q, want internal_error", got.Error.Code)
	}
	if got.Error.Type != "server_error" {
		t.Errorf("type = %q, want server_error", got.Error.Type)
	}
	if rec.Header().Get(requestIDHeader) == "" {
		t.Error("X-Request-Id header not set on a recovered panic response")
	}
}

// TestRecoverMiddlewareAfterResponseStarted proves that when the response has
// already started (e.g. a streaming handler that panics mid-stream), the recover
// middleware does NOT attempt a second WriteHeader — the status stays what the
// handler set, and the panic is contained (no crash).
func TestRecoverMiddlewareAfterResponseStarted(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{log: discard}

	panicky := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("mid-stream boom")
	})
	h := s.metricsMiddleware(s.requestIDMiddleware(s.recoverMiddleware(panicky)))

	req := httptest.NewRequest(http.MethodGet, "/panic-mid", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not panic out of ServeHTTP

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (response already started, recover must not rewrite)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("body = %q, want the partial bytes preserved", rec.Body.String())
	}
}

// TestRecoverMiddlewarePassesThrough proves a non-panicking handler is unaffected.
func TestRecoverMiddlewarePassesThrough(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{log: discard}

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("fine"))
	})
	h := s.recoverMiddleware(ok)

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || rec.Body.String() != "fine" {
		t.Errorf("pass-through altered the response: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// ---- Fix 6: 405 Allow header + array prompt ----

// TestMethodNotAllowedSetsAllow proves a wrong method on the discovery and
// inference routes returns 405 with an Allow header advertising the right verb.
func TestMethodNotAllowedSetsAllow(t *testing.T) {
	s, token := readinessServer(t, &fakeEngine{}, "llama3")

	// POST to a GET-only route.
	for _, path := range []string{"/v1/models", "/models"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s POST status = %d, want 405; body=%s", path, rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s 405 Allow = %q, want GET", path, allow)
		}
	}

	// GET to a POST-only route (decodePost guards the method).
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		rec := do(t, s, path, token) // do issues a GET
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET status = %d, want 405; body=%s", path, rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s 405 Allow = %q, want POST", path, allow)
		}
	}
}

// TestCompletionPromptStringForm proves a plain string prompt still decodes.
func TestCompletionPromptStringForm(t *testing.T) {
	cap := &capturingEngine{}
	s, token := readinessServer(t, cap, "llama3")

	rec := postJSON(t, s, "/v1/completions", token, `{"model":"llama3","prompt":"hello world"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastPrompt != "hello world" {
		t.Errorf("prompt = %q, want \"hello world\"", cap.lastPrompt)
	}
}

// TestCompletionPromptArrayForm proves an array-of-strings prompt decodes, joined
// with newlines onto the single Job.Prompt.
func TestCompletionPromptArrayForm(t *testing.T) {
	cap := &capturingEngine{}
	s, token := readinessServer(t, cap, "llama3")

	rec := postJSON(t, s, "/v1/completions", token, `{"model":"llama3","prompt":["line one","line two"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastPrompt != "line one\nline two" {
		t.Errorf("prompt = %q, want the two lines joined with a newline", cap.lastPrompt)
	}
}

// TestCompletionPromptNonStringArrayRejected proves a non-string (integer-token)
// prompt array is rejected with 400 invalid_request_error (agent-gpu has no
// tokenizer for token ids).
func TestCompletionPromptNonStringArrayRejected(t *testing.T) {
	s, token := readinessServer(t, &capturingEngine{}, "llama3")

	rec := postJSON(t, s, "/v1/completions", token, `{"model":"llama3","prompt":[1,2,3]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec); got.Error.Code != "invalid_request_error" {
		t.Errorf("code = %q, want invalid_request_error", got.Error.Code)
	}
}

// capturingEngine records the Job.Prompt the handler threaded through, so the
// prompt-decoding tests can assert the mapping onto Job.Prompt.
type capturingEngine struct {
	lastPrompt string
}

func (e *capturingEngine) SubmitAuthorizedJob(_ context.Context, _ store.APIKey, job types.Job) (types.JobResult, error) {
	e.lastPrompt = job.Prompt
	return types.JobResult{JobID: job.ID, Output: "ok", FinishReason: "stop"}, nil
}

func (e *capturingEngine) SubmitAuthorizedJobStream(_ context.Context, _ store.APIKey, job types.Job) (<-chan types.JobChunk, error) {
	e.lastPrompt = job.Prompt
	ch := make(chan types.JobChunk, 1)
	ch <- types.JobChunk{JobID: job.ID, Done: true, FinishReason: "stop"}
	close(ch)
	return ch, nil
}
