package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// syncBuffer is a concurrency-safe bytes.Buffer: the worker logs from its own
// goroutine while the test goroutine reads, so writes/reads must be mutex-guarded
// to be race-free under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// correlationHarness wires a real control-plane gRPC server, a worker over
// bufconn, and the HTTP API together, capturing the SERVER and WORKER logs into
// separate JSON buffers so a test can assert the same correlation id appears on
// both sides (end-to-end traceability, #23).
type correlationHarness struct {
	url       string
	token     string
	serverLog *syncBuffer
	workerLog *syncBuffer
}

func newCorrelationHarness(t *testing.T, exec *scriptedExecutor, model string) correlationHarness {
	t.Helper()
	serverLog := &syncBuffer{}
	workerLog := &syncBuffer{}
	// JSON handlers at Info so "executing job" (Info) and placement lines are
	// captured and parseable. The redaction ReplaceAttr is a cmd-layer concern; the
	// correlation story is independent of it, so a plain JSON handler suffices here.
	srvLogger := slog.New(slog.NewJSONHandler(serverLog, &slog.HandlerOptions{Level: slog.LevelInfo}))
	wrkLogger := slog.New(slog.NewJSONHandler(workerLog, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(srvLogger))
	grpcSrv := server.New(
		server.WithLogger(srvLogger),
		server.WithStore(st),
		server.WithAuthorizer(az),
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
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, nil, srvLogger, "")
	ts := httptest.NewServer(httpSrv.Handler())
	t.Cleanup(ts.Close)

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	exec.models = []types.Model{{Name: model, Digest: "sha256:test"}}
	wctx, wcancel := context.WithCancel(context.Background())
	t.Cleanup(wcancel)
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          "worker-1",
		Models:            exec.models,
		Executor:          exec,
		Logger:            wrkLogger,
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

	h := correlationHarness{url: ts.URL, token: token, serverLog: serverLog, workerLog: workerLog}
	waitFor(t, 2*time.Second, "model in catalog", func() bool {
		return len(fetchModels(t, h.url, h.token)) == 1
	})
	return h
}

// logLineWithField scans newline-delimited JSON log output for a line whose
// named field equals want, returning the decoded record and whether it was
// found. It is the primitive the correlation assertions build on.
func logLineWithField(t *testing.T, out, field, want string) (map[string]any, bool) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if v, ok := rec[field].(string); ok && v == want {
			return rec, true
		}
	}
	return nil, false
}

// TestCorrelationEndToEnd is the headline correlation AC test: a single chat
// request is traceable end-to-end by one id. It proves:
//
//   - the response carries an X-Request-Id header;
//   - the worker logged an "executing job" line whose job_id equals that id;
//   - the SAME id therefore links the HTTP boundary → the worker's execution.
//
// Server-side request logging is request-scoped (request_id == job_id), and the
// worker line is the far end of the trace. (AC: a single request traceable
// end-to-end via its correlation id.)
func TestCorrelationEndToEnd(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"hi"}, promptTokens: 1, completionTokens: 1}
	h := newCorrelationHarness(t, exec, "llama3")

	resp := h.postChat(t, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	id := resp.Header.Get("X-Request-Id")
	if id == "" {
		t.Fatal("response missing X-Request-Id header")
	}

	// The worker logs "executing job" asynchronously; give it a moment to flush.
	waitFor(t, 2*time.Second, "worker executing-job log line", func() bool {
		_, ok := logLineWithField(t, h.workerLog.String(), "job_id", id)
		return ok
	})

	rec, ok := logLineWithField(t, h.workerLog.String(), "job_id", id)
	if !ok {
		t.Fatalf("worker log has no job_id == %q\nworker log:\n%s", id, h.workerLog.String())
	}
	if rec["msg"] != "executing job" {
		t.Errorf("worker job_id line msg = %v, want 'executing job'", rec["msg"])
	}
	if rec["job_id"] != id {
		t.Errorf("worker job_id = %v, want %q (request_id == job_id end-to-end)", rec["job_id"], id)
	}
}

// TestCorrelationHonorsInboundRequestID proves a client-supplied X-Request-Id is
// honored: it is echoed on the response AND becomes the job_id the worker logs,
// so a caller can pin its own trace id end-to-end.
func TestCorrelationHonorsInboundRequestID(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"hi"}, promptTokens: 1, completionTokens: 1}
	h := newCorrelationHarness(t, exec, "llama3")

	const inbound = "trace-abc123"
	resp := h.postChat(t, inbound)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Request-Id"); got != inbound {
		t.Fatalf("echoed X-Request-Id = %q, want inbound %q", got, inbound)
	}

	waitFor(t, 2*time.Second, "worker line for inbound id", func() bool {
		_, ok := logLineWithField(t, h.workerLog.String(), "job_id", inbound)
		return ok
	})
}

// TestCorrelationUnauthenticatedGetsRequestID proves the correlation middleware
// is OUTERMOST: even a 401 (no Authorization header, short-circuited by the auth
// middleware before any handler) carries an X-Request-Id header.
func TestCorrelationUnauthenticatedGetsRequestID(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"x"}}
	h := newCorrelationHarness(t, exec, "llama3")

	req, err := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions",
		strings.NewReader(`{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// No Authorization header → 401 before any handler runs.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("401 response missing X-Request-Id header (middleware not outermost)")
	}
}

// TestCorrelationRejectsForgedRequestID proves a forged inbound X-Request-Id
// carrying characters outside the safe allow-list (here a space and shell
// metacharacters — a stand-in for a log-injection payload that a non-Go client
// or upstream proxy could send) is NOT honored: the server mints a fresh, clean
// id instead, so an attacker-controlled value can never reach the logs or the
// echoed response header verbatim. (Go's own HTTP client blocks CR/LF before
// send, so the allow-list is the defense for everything it lets through.)
func TestCorrelationRejectsForgedRequestID(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"hi"}, promptTokens: 1, completionTokens: 1}
	h := newCorrelationHarness(t, exec, "llama3")

	forged := "abc def; rm -rf /"
	resp := h.postChat(t, forged)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := resp.Header.Get("X-Request-Id")
	if got == forged {
		t.Fatalf("forged id was honored verbatim: %q", got)
	}
	if strings.ContainsAny(got, " ;/") {
		t.Fatalf("echoed id contains disallowed characters: %q", got)
	}
	if got == "" {
		t.Fatal("server did not mint a replacement id for the rejected forged value")
	}
	// The minted replacement must drive the trace, and the forged string must not
	// appear anywhere in the server logs.
	if strings.Contains(h.serverLog.String(), forged) {
		t.Errorf("forged id leaked into server logs:\n%s", h.serverLog.String())
	}
}

// postChat issues a non-streaming chat request, optionally setting an inbound
// X-Request-Id header when reqID is non-empty.
func (h correlationHarness) postChat(t *testing.T, reqID string) *http.Response {
	t.Helper()
	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	if reqID != "" {
		req.Header.Set("X-Request-Id", reqID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}
