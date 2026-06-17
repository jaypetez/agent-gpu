package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/ollama"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/testutil"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// newOllamaWorker builds a worker whose Executor is a real OllamaExecutor
// pointed at the given stub base URL, wired to the harness's in-process dialer.
func newOllamaWorker(h *harness, id, baseURL string) *worker.Worker {
	return worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          id,
		HeartbeatInterval: 20 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		Executor:          worker.NewOllamaExecutor(ollama.New(baseURL)),
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			h.dialOption(),
		},
	})
}

// TestOllamaStreamingRoundTrip covers the true-streaming contract end to end: a
// worker runs streaming chat inference against a stub Ollama, emits a JobChunk
// per token over the gRPC stream, and the server accumulates them and resolves
// the synchronous SubmitJob caller with the full output and the eval_count
// token total. It also asserts the heartbeat-sourced model list reflects
// /api/tags.
func TestOllamaStreamingRoundTrip(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.5.0"}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"llama3"}]}`)
		case "/api/chat":
			for _, tok := range []string{"po", "ng", "!"} {
				fmt.Fprintf(w, `{"message":{"content":%q},"done":false}`+"\n", tok)
			}
			fmt.Fprint(w, `{"done":true,"eval_count":3,"prompt_eval_count":2}`+"\n")
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer ollamaSrv.Close()

	h := newHarness(t)
	defer h.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newOllamaWorker(h, "ollama-worker", ollamaSrv.URL)
	go func() { _ = w.Run(ctx) }()

	// Wait for registration and for the heartbeat-sourced model list (from
	// /api/tags) to surface in the fleet view.
	waitFor(t, 2*time.Second, "worker to register with model from /api/tags", func() bool {
		for _, fw := range h.srv.Fleet() {
			for _, m := range fw.Models {
				if m.Name == "llama3" {
					return true
				}
			}
		}
		return false
	})

	res, err := h.srv.SubmitJob(ctx, types.Job{ID: "job-stream", Model: "llama3", Prompt: "ping"})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if res.Output != "pong!" {
		t.Fatalf("accumulated output = %q, want %q", res.Output, "pong!")
	}
	// eval_count + prompt_eval_count from the terminal Ollama object.
	if res.Tokens != 5 {
		t.Fatalf("tokens = %d, want 5", res.Tokens)
	}
}

// TestOllamaInferenceErrorResolvesWaiter covers the failure path: an Ollama
// error must produce a terminal error chunk that resolves the waiter with a
// stable error code, never hang it.
func TestOllamaInferenceErrorResolvesWaiter(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.5.0"}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"ghost"}]}`)
		case "/api/chat":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"model 'ghost' not found"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer ollamaSrv.Close()

	h := newHarness(t)
	defer h.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newOllamaWorker(h, "ollama-worker", ollamaSrv.URL)
	go func() { _ = w.Run(ctx) }()
	waitFor(t, 2*time.Second, "worker to register", func() bool { return h.srv.WorkerCount() == 1 })

	subCtx, subCancel := context.WithTimeout(ctx, 2*time.Second)
	defer subCancel()
	res, err := h.srv.SubmitJob(subCtx, types.Job{ID: "job-fail", Model: "ghost", Prompt: "ping"})
	if err == nil {
		t.Fatalf("expected error, got result %+v", res)
	}
	var je *types.JobError
	if !errors.As(err, &je) || je.Code != ollama.CodeModelNotFound {
		t.Fatalf("err = %v, want model_not_found JobError", err)
	}
}

// pullHarness wires a server (with an authorizer) to a worker whose executor
// records pulls (a testutil.FakeExecutor advertising llama3), plus an
// auth.Service, so the permission-gated pull path can be exercised end to end.
type pullHarness struct {
	h    *harness
	auth *auth.Service
	exec *testutil.FakeExecutor
}

func newPullHarness(t *testing.T) *pullHarness {
	t.Helper()
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })

	h := &harness{t: t}
	h.srv = server.New(server.WithAuthorizer(authz.NewAuthorizer()))
	h.start()
	t.Cleanup(h.close)

	exec := testutil.NewFakeExecutor(testutil.WithExecModels("llama3"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          "puller",
		HeartbeatInterval: 20 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		Executor:          exec,
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			h.dialOption(),
		},
	})
	go func() { _ = w.Run(ctx) }()
	waitFor(t, 2*time.Second, "puller to register", func() bool { return h.srv.WorkerCount() == 1 })

	return &pullHarness{h: h, auth: auth.NewService(st), exec: exec}
}

func (p *pullHarness) authedKey(t *testing.T, perms auth.Permissions) store.APIKey {
	t.Helper()
	ctx := context.Background()
	token, _, err := p.auth.CreateWithPermissions(ctx, "agent", perms)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key, err := p.auth.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return key
}

// TestPullModelPermitted covers the permission-gated pull happy path: a key
// permitted to Pull the model causes the server to send a PullModel control
// message and the worker's executor to actually pull it.
func TestPullModelPermitted(t *testing.T) {
	p := newPullHarness(t)
	key := p.authedKey(t, auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{"llama3"}})

	if err := p.h.srv.PullModel(context.Background(), key, "puller", "llama3"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	waitFor(t, 2*time.Second, "worker to pull the model", func() bool {
		for _, m := range p.exec.Pulls() {
			if m == "llama3" {
				return true
			}
		}
		return false
	})
}

// TestPullModelDenied covers the gate: a key NOT permitted to Pull is rejected
// with authz.ErrForbidden and NO PullModel message reaches the worker (so the
// worker's Ollama /api/pull is never called).
func TestPullModelDenied(t *testing.T) {
	p := newPullHarness(t)
	// read-only role may never pull, even on an allow-listed model.
	key := p.authedKey(t, auth.Permissions{Roles: []string{authz.RoleReadOnly}, AllowModels: []string{"llama3"}})

	err := p.h.srv.PullModel(context.Background(), key, "puller", "llama3")
	if !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	// Give any (erroneously-sent) message time to reach the worker, then assert
	// the executor was never asked to pull.
	time.Sleep(100 * time.Millisecond)
	if pulls := p.exec.Pulls(); len(pulls) != 0 {
		t.Fatalf("denied pull reached the worker: %v", pulls)
	}
}
