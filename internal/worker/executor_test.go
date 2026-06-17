package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/ollama"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestOllamaExecutorStreamsAndAccumulates verifies the executor emits a delta
// per token and returns the accumulated output plus Ollama's token count.
func TestOllamaExecutorStreamsAndAccumulates(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			for _, tok := range []string{"a", "b", "c"} {
				fmt.Fprintf(w, `{"message":{"content":%q},"done":false}`+"\n", tok)
			}
			fmt.Fprint(w, `{"done":true,"eval_count":4,"prompt_eval_count":2}`+"\n")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	var deltas []string
	res := exec.Execute(context.Background(), types.Job{ID: "j1", Model: "llama3", Prompt: "hi"},
		func(c types.JobChunk) {
			if c.JobID != "j1" {
				t.Errorf("chunk job id = %q, want j1", c.JobID)
			}
			deltas = append(deltas, c.Delta)
		})
	if res.Err != nil {
		t.Fatalf("unexpected err: %v", res.Err)
	}
	if res.Output != "abc" {
		t.Fatalf("output = %q, want abc", res.Output)
	}
	if res.Tokens != 6 {
		t.Fatalf("tokens = %d, want 6", res.Tokens)
	}
	if len(deltas) != 3 || deltas[0] != "a" || deltas[2] != "c" {
		t.Fatalf("deltas = %v, want [a b c]", deltas)
	}
}

// captureChatKeepAlive runs a one-shot /api/chat stub that decodes the request
// body and returns the keep_alive field exactly as sent (present flag + value),
// so a test can assert how the executor threaded a job's warmth (#35).
func captureChatKeepAlive(t *testing.T) (*httptest.Server, func() (present bool, value float64)) {
	t.Helper()
	type ka struct {
		present bool
		value   float64
	}
	ch := make(chan ka, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode chat body %q: %v", raw, err)
		}
		v, ok := m["keep_alive"]
		f, _ := v.(float64)
		ch <- ka{present: ok, value: f}
		fmt.Fprint(w, `{"done":true,"eval_count":1,"prompt_eval_count":1}`+"\n")
	}))
	return srv, func() (bool, float64) {
		got := <-ch
		return got.present, got.value
	}
}

// TestOllamaExecutorThreadsKeepAlive verifies a job's KeepAliveSeconds is threaded
// to Ollama's /api/chat as the keep_alive field, so a session-bound turn keeps the
// model warm (#35).
func TestOllamaExecutorThreadsKeepAlive(t *testing.T) {
	t.Parallel()
	srv, keepAliveOf := captureChatKeepAlive(t)
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	res := exec.Execute(context.Background(),
		types.Job{ID: "j1", Model: "llama3", Prompt: "hi", KeepAliveSeconds: 900}, nil)
	if res.Err != nil {
		t.Fatalf("unexpected err: %v", res.Err)
	}
	present, value := keepAliveOf()
	if !present {
		t.Fatal("keep_alive not sent for a warm job")
	}
	if int64(value) != 900 {
		t.Fatalf("keep_alive = %v, want 900", value)
	}
}

// TestOllamaExecutorOmitsKeepAliveWhenZero verifies a job with KeepAliveSeconds 0
// (every session-less job, and every pre-#35 job) sends NO keep_alive, so Ollama's
// own default unload window applies — exact back-compat.
func TestOllamaExecutorOmitsKeepAliveWhenZero(t *testing.T) {
	t.Parallel()
	srv, keepAliveOf := captureChatKeepAlive(t)
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	res := exec.Execute(context.Background(),
		types.Job{ID: "j1", Model: "llama3", Prompt: "hi"}, nil) // KeepAliveSeconds 0
	if res.Err != nil {
		t.Fatalf("unexpected err: %v", res.Err)
	}
	if present, _ := keepAliveOf(); present {
		t.Fatal("keep_alive sent for a session-less job; want omitted")
	}
}

// TestKeepAliveHelper unit-tests the seconds→optional-pointer mapping the executor
// uses: 0 means "unset" (nil → omitted), any non-zero value (a warm window, or a
// negative keep-forever sentinel) is passed through as a pointer.
func TestKeepAliveHelper(t *testing.T) {
	t.Parallel()
	if got := keepAlive(0); got != nil {
		t.Fatalf("keepAlive(0) = %v, want nil", *got)
	}
	if got := keepAlive(600); got == nil || *got != 600 {
		t.Fatalf("keepAlive(600) = %v, want 600", got)
	}
	if got := keepAlive(-1); got == nil || *got != -1 {
		t.Fatalf("keepAlive(-1) = %v, want -1", got)
	}
}

// TestOllamaExecutorUnload verifies the executor's Unload drives Ollama's
// keep_alive=0 generate eviction path (#35).
func TestOllamaExecutorUnload(t *testing.T) {
	t.Parallel()
	var gotKeepAlive float64
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		called = true
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode unload body: %v", err)
		}
		gotKeepAlive, _ = m["keep_alive"].(float64)
		fmt.Fprint(w, `{"model":"llama3","done":true,"done_reason":"unload"}`)
	}))
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	if err := exec.Unload(context.Background(), "llama3"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if !called {
		t.Fatal("generate (unload) endpoint was not called")
	}
	if int64(gotKeepAlive) != 0 {
		t.Fatalf("keep_alive = %v, want 0", gotKeepAlive)
	}
}

// TestOllamaExecutorErrorMapsToJobError verifies an Ollama failure becomes a
// JobResult carrying a stable error code (never a hang).
func TestOllamaExecutorErrorMapsToJobError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model 'x' not found"}`)
	}))
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	res := exec.Execute(context.Background(), types.Job{ID: "j1", Model: "x", Prompt: "hi"}, nil)
	if res.Err == nil {
		t.Fatal("expected error result")
	}
	if res.Err.Code != ollama.CodeModelNotFound {
		t.Fatalf("code = %q, want %q", res.Err.Code, ollama.CodeModelNotFound)
	}
}

// TestOllamaExecutorListModels verifies model listing flows through /api/tags.
func TestOllamaExecutorListModels(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"models":[{"name":"llama3"}]}`)
	}))
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	models, err := exec.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Name != "llama3" {
		t.Fatalf("models = %+v, want [llama3]", models)
	}
}

// TestOllamaExecutorPull verifies the pull path drives /api/pull to completion.
func TestOllamaExecutorPull(t *testing.T) {
	t.Parallel()
	var pulled string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		pulled = "called"
		fmt.Fprint(w, `{"status":"success"}`+"\n")
	}))
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	if err := exec.Pull(context.Background(), "llama3"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulled != "called" {
		t.Fatal("pull endpoint was not called")
	}
}

// TestOllamaExecutorVersion verifies the version probe capability.
func TestOllamaExecutorVersion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"1.2.3"}`)
	}))
	defer srv.Close()

	exec := NewOllamaExecutor(ollama.New(srv.URL))
	v, err := exec.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", v)
	}
}
