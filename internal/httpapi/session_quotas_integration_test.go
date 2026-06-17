package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// quotaHarnessOpts parameterizes the session limits under test (#37).
type quotaHarnessOpts struct {
	maxPerKey int
	maxTurns  int
	maxTokens int
	policy    session.OverflowPolicy
}

// buildQuotaHarness wires a control-plane server, an HTTP API, and one worker
// over bufconn with a session manager configured for the given limits. It mirrors
// buildSessionHarness but exposes the #37 knobs (concurrent cap + history caps +
// overflow policy) so the limit paths can be exercised end to end.
func buildQuotaHarness(t *testing.T, model string, exec *recordingExecutor, o quotaHarnessOpts) sessionHarness {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))

	exec.models = []types.Model{{Name: model, Digest: "sha256:test"}}
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStoreWithPolicy(o.maxTurns, 0, o.maxTokens, o.policy),
		session.WithLogger(discard),
		session.WithTTL(time.Hour),
		session.WithMaxSessionsPerKey(o.maxPerKey),
	)
	mgr.Start()
	t.Cleanup(func() { _ = mgr.Close() })

	grpcSrv := server.New(
		server.WithLogger(discard),
		server.WithStore(st),
		server.WithAuthorizer(az),
		server.WithSessionManager(mgr),
		server.WithHeartbeatTimeout(2*time.Second),
		server.WithEvictScanInterval(50*time.Millisecond),
	)
	grpcSrv.Start()
	t.Cleanup(func() { _ = grpcSrv.Close() })

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	grpcSrv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, mgr, nil, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	t.Cleanup(ts.Close)

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	models := []types.Model{{Name: model, Digest: "sha256:test"}}
	wctx, wcancel := context.WithCancel(context.Background())
	t.Cleanup(wcancel)
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          exec.workerID,
		Models:            models,
		Executor:          exec,
		Logger:            discard,
		HeartbeatInterval: 15 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})
	go func() { _ = w.Run(wctx) }()

	h := sessionHarness{url: ts.URL, token: token, authSvc: authSvc, mgr: mgr,
		execs: map[string]*recordingExecutor{exec.workerID: exec}}
	waitFor(t, 3*time.Second, "worker in fleet", func() bool {
		return len(grpcSrv.Fleet()) == 1
	})
	return h
}

// errorEnvelope decodes the {"error":{message,code}} body for assertions.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeErr(t *testing.T, resp *http.Response) errorEnvelope {
	t.Helper()
	var e errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return e
}

// TestSessionConcurrentCapHTTP proves AC1 over HTTP: with a per-key cap of 2, the
// 3rd POST /v1/sessions is rejected with 429 + code session_limit_exceeded; after
// DELETEing one, a new create succeeds again.
func TestSessionConcurrentCapHTTP(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "ok"}
	h := buildQuotaHarness(t, "llama3", exec, quotaHarnessOpts{maxPerKey: 2})

	id1 := h.createSession(t, "llama3")
	_ = h.createSession(t, "llama3") // 2nd, still under the cap

	// 3rd create exceeds the cap.
	resp := h.post(t, "/v1/sessions", `{"model":"llama3"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		_ = resp.Body.Close()
		t.Fatalf("over-cap create status = %d, want 429", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "session_limit_exceeded" {
		_ = resp.Body.Close()
		t.Fatalf("over-cap create code = %q, want session_limit_exceeded", code)
	}
	_ = resp.Body.Close()

	// Free a slot; the next create is accepted.
	dresp := h.delete(t, h.token, "/v1/sessions/"+id1)
	if dresp.StatusCode != http.StatusNoContent {
		_ = dresp.Body.Close()
		t.Fatalf("delete status = %d, want 204", dresp.StatusCode)
	}
	_ = dresp.Body.Close()
	_ = h.createSession(t, "llama3") // succeeds (201) or the helper fails the test
}

// TestSessionTurnCapRejectHTTP proves AC over HTTP for reject mode: a stateful
// chat turn that would exceed the per-session turn cap is rejected with 409 +
// code session_limit_exceeded BEFORE any dispatch (the executor is not invoked
// for the rejected turn), and the stored history is unchanged.
func TestSessionTurnCapRejectHTTP(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "r"}
	// Turn cap 2 under reject. Each accepted turn persists 2 messages (user +
	// assistant), so the very first turn fills the cap; the second turn's
	// pre-dispatch check sees that appending would exceed 2 and rejects.
	h := buildQuotaHarness(t, "llama3", exec, quotaHarnessOpts{maxTurns: 2, policy: session.OverflowReject})

	sid := h.createSession(t, "llama3")

	// Turn 1 is accepted (history empty: user+assistant = 2 == cap).
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"one"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("turn 1 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	handledAfterTurn1 := exec.handled()

	// Turn 2 would push history past the cap → 409, rejected before dispatch.
	resp = h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"two"}]}`, nil)
	if resp.StatusCode != http.StatusConflict {
		_ = resp.Body.Close()
		t.Fatalf("over-cap turn status = %d, want 409", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "session_limit_exceeded" {
		_ = resp.Body.Close()
		t.Fatalf("over-cap turn code = %q, want session_limit_exceeded", code)
	}
	_ = resp.Body.Close()

	// The rejected turn never reached the worker.
	if exec.handled() != handledAfterTurn1 {
		t.Errorf("rejected turn dispatched: handled %d -> %d", handledAfterTurn1, exec.handled())
	}
	// History still holds exactly turn 1's two messages.
	if hist := h.history(t, sid); len(hist) != 2 {
		t.Fatalf("history after reject = %d turns, want 2: %+v", len(hist), hist)
	}
}

// TestSessionTokenCapRejectHTTP proves AC over HTTP for the context-token cap in
// reject mode: a turn whose content pushes the cumulative token estimate past the
// cap is rejected with 409.
func TestSessionTokenCapRejectHTTP(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "ok"} // reply "ok" = 1 token
	// Token cap 3. Turn 1: user "a b" (2) + assistant "ok" (1) = 3 == cap, accepted.
	// Turn 2: any further user token would exceed 3 → rejected.
	h := buildQuotaHarness(t, "llama3", exec, quotaHarnessOpts{maxTokens: 3, policy: session.OverflowReject})

	sid := h.createSession(t, "llama3")

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"a b"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("turn 1 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"more"}]}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("over-token-cap turn status = %d, want 409", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "session_limit_exceeded" {
		t.Fatalf("over-token-cap turn code = %q, want session_limit_exceeded", code)
	}
}

// TestSessionTrimModeStillWorksHTTP proves the DEFAULT trim policy keeps working
// over HTTP: with a tiny turn cap and trim, repeated stateful turns never error
// (they trim oldest), and the worker keeps being invoked.
func TestSessionTrimModeStillWorksHTTP(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "r"}
	h := buildQuotaHarness(t, "llama3", exec, quotaHarnessOpts{maxTurns: 2, policy: session.OverflowTrim})

	sid := h.createSession(t, "llama3")
	for _, msg := range []string{"one", "two", "three"} {
		resp := h.post(t, "/v1/chat/completions",
			`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"`+msg+`"}]}`, nil)
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("trim-mode turn %q status = %d, want 200", msg, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	// Trim keeps at most the cap (2) most-recent turns.
	if hist := h.history(t, sid); len(hist) != 2 {
		t.Fatalf("trim-mode history = %d, want cap 2: %+v", len(hist), hist)
	}
	if exec.handled() != 3 {
		t.Errorf("trim mode handled %d turns, want 3", exec.handled())
	}
}
