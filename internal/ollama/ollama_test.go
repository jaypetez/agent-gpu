package ollama

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

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
