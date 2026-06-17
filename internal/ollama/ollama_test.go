package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// ptr returns a pointer to v, for constructing the optional ChatRequest.KeepAlive.
func ptr[T any](v T) *T { return &v }

// jobErr extracts a *types.JobError from err, failing the test if err is not one.
func jobErr(t *testing.T, err error) *types.JobError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var je *types.JobError
	if !errors.As(err, &je) {
		t.Fatalf("error is not *types.JobError: %T %v", err, err)
	}
	return je
}

func TestVersion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"version":"0.5.7"}`)
	}))
	defer srv.Close()

	v, err := New(srv.URL).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "0.5.7" {
		t.Fatalf("version = %q, want 0.5.7", v)
	}
}

func TestVersionUnreachable(t *testing.T) {
	t.Parallel()
	// Point at a closed server so Do fails at the transport layer.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := New(url).Version(context.Background())
	je := jobErr(t, err)
	if je.Code != CodeUnreachable {
		t.Fatalf("code = %q, want %q", je.Code, CodeUnreachable)
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"models":[{"name":"llama3:latest","digest":"abc"},{"name":"mistral","digest":"def"}]}`)
	}))
	defer srv.Close()

	models, err := New(srv.URL).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].Name != "llama3:latest" || models[0].Digest != "abc" || models[1].Name != "mistral" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestPullSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// NDJSON progress stream ending in success.
		fmt.Fprint(w, `{"status":"pulling manifest"}`+"\n")
		fmt.Fprint(w, `{"status":"downloading","completed":10,"total":100}`+"\n")
		fmt.Fprint(w, `{"status":"success"}`+"\n")
	}))
	defer srv.Close()

	if err := New(srv.URL).Pull(context.Background(), "llama3"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
}

func TestPullErrorInStream(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"pulling manifest"}`+"\n")
		fmt.Fprint(w, `{"error":"model 'nope' not found, try pulling it first"}`+"\n")
	}))
	defer srv.Close()

	err := New(srv.URL).Pull(context.Background(), "nope")
	je := jobErr(t, err)
	if je.Code != CodeModelNotFound {
		t.Fatalf("code = %q, want %q (msg %q)", je.Code, CodeModelNotFound, je.Message)
	}
}

func TestChatStreamsTokensAndCounts(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		flusher, _ := w.(http.Flusher)
		for _, tok := range []string{"Hello", ", ", "world"} {
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"done":false}`+"\n", tok)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, `{"done":true,"eval_count":3,"prompt_eval_count":5}`+"\n")
	}))
	defer srv.Close()

	var got strings.Builder
	tokens, err := New(srv.URL).Chat(context.Background(), "llama3", "hi", func(delta string) {
		got.WriteString(delta)
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got.String() != "Hello, world" {
		t.Fatalf("accumulated = %q, want %q", got.String(), "Hello, world")
	}
	// eval_count + prompt_eval_count.
	if tokens != 8 {
		t.Fatalf("tokens = %d, want 8", tokens)
	}
}

func TestChatModelNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model 'ghost' not found"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Chat(context.Background(), "ghost", "hi", nil)
	je := jobErr(t, err)
	if je.Code != CodeModelNotFound {
		t.Fatalf("code = %q, want %q", je.Code, CodeModelNotFound)
	}
}

func TestChatInvalidRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Chat(context.Background(), "m", "hi", nil)
	je := jobErr(t, err)
	if je.Code != CodeInvalidRequest {
		t.Fatalf("code = %q, want %q", je.Code, CodeInvalidRequest)
	}
}

// TestChatContextCancellationAborts verifies that cancelling the context stops
// the stream and emitting, mapping to a timeout-class code, and that the
// request is aborted (the server observes the closed connection).
func TestChatContextCancellationAborts(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		aborted bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		// Emit one token, then block until the client cancels (r.Context() done).
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"first"},"done":false}`+"\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
		mu.Lock()
		aborted = true
		mu.Unlock()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var emitted int
	go func() {
		// Cancel shortly after the first token is emitted.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := New(srv.URL).Chat(ctx, "m", "hi", func(string) { emitted++ })
	je := jobErr(t, err)
	if je.Code != CodeTimeout {
		t.Fatalf("code = %q, want %q", je.Code, CodeTimeout)
	}
	waitTrue(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return aborted
	})
}

func waitTrue(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// decodeChatBody reads and JSON-decodes a captured /api/chat request body into a
// generic map so a test can assert exactly which fields were (or were not) sent
// on the wire — in particular whether keep_alive is present and its value (#35).
func decodeChatBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
	return m
}

// chatStreamServer is a minimal /api/chat stub that captures the decoded request
// body and replies with a terminal done object, for asserting what the client
// sent. The captured body is delivered on the returned channel exactly once.
func chatStreamServer(t *testing.T) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		bodies <- decodeChatBody(t, r)
		fmt.Fprint(w, `{"done":true,"eval_count":1,"prompt_eval_count":1}`+"\n")
	}))
	return srv, bodies
}

// TestChatStreamSendsKeepAlive asserts that a non-nil ChatRequest.KeepAlive is
// sent to Ollama as a numeric keep_alive field of the same value (in seconds) —
// the model-warmth wire contract (#35).
func TestChatStreamSendsKeepAlive(t *testing.T) {
	t.Parallel()
	srv, bodies := chatStreamServer(t)
	defer srv.Close()

	_, err := New(srv.URL).ChatStream(context.Background(), ChatRequest{
		Model:     "llama3",
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
		KeepAlive: ptr(int64(600)),
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	body := <-bodies
	ka, ok := body["keep_alive"]
	if !ok {
		t.Fatalf("keep_alive missing from request body %v", body)
	}
	// JSON numbers decode to float64; 600 seconds must round-trip exactly.
	if f, ok := ka.(float64); !ok || int64(f) != 600 {
		t.Fatalf("keep_alive = %v (%T), want 600", ka, ka)
	}
}

// TestChatStreamUnloadKeepAliveZero asserts that a KeepAlive of 0 is sent
// verbatim (not omitted): a zero keep_alive tells Ollama to unload the model
// immediately after the turn. (The worker maps a job's 0 to nil/omitted; this
// guards the client's own pass-through semantics for an explicit 0.)
func TestChatStreamUnloadKeepAliveZero(t *testing.T) {
	t.Parallel()
	srv, bodies := chatStreamServer(t)
	defer srv.Close()

	_, err := New(srv.URL).ChatStream(context.Background(), ChatRequest{
		Model:     "llama3",
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
		KeepAlive: ptr(int64(0)),
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	body := <-bodies
	ka, ok := body["keep_alive"]
	if !ok {
		t.Fatalf("keep_alive missing for explicit 0 in body %v", body)
	}
	if f, ok := ka.(float64); !ok || int64(f) != 0 {
		t.Fatalf("keep_alive = %v (%T), want 0", ka, ka)
	}
}

// TestChatStreamOmitsKeepAliveWhenUnset asserts that a nil KeepAlive omits the
// field entirely, so Ollama applies its own default unload window — back-compat
// with every pre-#35 chat request (which carries no keep_alive).
func TestChatStreamOmitsKeepAliveWhenUnset(t *testing.T) {
	t.Parallel()
	srv, bodies := chatStreamServer(t)
	defer srv.Close()

	_, err := New(srv.URL).ChatStream(context.Background(), ChatRequest{
		Model:    "llama3",
		Messages: []types.Message{{Role: "user", Content: "hi"}},
		// KeepAlive left nil.
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	body := <-bodies
	if _, ok := body["keep_alive"]; ok {
		t.Fatalf("keep_alive present when unset; body %v", body)
	}
}

// TestUnloadSendsKeepAliveZero asserts Unload issues a keep_alive=0 generate for
// the model (Ollama's documented eviction path), with no prompt (#35).
func TestUnloadSendsKeepAliveZero(t *testing.T) {
	t.Parallel()
	bodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode unload body %q: %v", raw, err)
		}
		bodies <- m
		fmt.Fprint(w, `{"model":"llama3","done":true,"done_reason":"unload"}`)
	}))
	defer srv.Close()

	if err := New(srv.URL).Unload(context.Background(), "llama3"); err != nil {
		t.Fatalf("Unload: %v", err)
	}

	body := <-bodies
	if body["model"] != "llama3" {
		t.Fatalf("model = %v, want llama3", body["model"])
	}
	ka, ok := body["keep_alive"]
	if !ok {
		t.Fatalf("keep_alive missing from unload body %v", body)
	}
	if f, ok := ka.(float64); !ok || int64(f) != 0 {
		t.Fatalf("keep_alive = %v (%T), want 0", ka, ka)
	}
	if _, hasPrompt := body["prompt"]; hasPrompt {
		t.Fatalf("unload sent a prompt; body %v", body)
	}
}

// TestUnloadModelNotFoundIsNoError asserts that unloading a model Ollama does not
// have is treated as success — there is nothing to evict, so the desired end
// state already holds (the unload path is best-effort).
func TestUnloadModelNotFoundIsNoError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model 'ghost' not found"}`)
	}))
	defer srv.Close()

	if err := New(srv.URL).Unload(context.Background(), "ghost"); err != nil {
		t.Fatalf("Unload of missing model = %v, want nil", err)
	}
}

// TestUnloadServerErrorSurfaces asserts a genuine Ollama failure on the unload
// path is surfaced as a *types.JobError (only model-not-found is swallowed).
func TestUnloadServerErrorSurfaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	err := New(srv.URL).Unload(context.Background(), "llama3")
	je := jobErr(t, err)
	if je.Code != CodeOllamaError {
		t.Fatalf("code = %q, want %q", je.Code, CodeOllamaError)
	}
}
