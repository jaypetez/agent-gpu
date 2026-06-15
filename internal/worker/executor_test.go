package worker

import (
	"context"
	"fmt"
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
