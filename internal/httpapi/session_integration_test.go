package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/jaypetez/agent-gpu/internal/testutil"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// recordingExecutor is a worker.Executor that echoes a configurable reply and
// records, per worker, the jobs it handled and how many. It backs the
// session-aware integration tests: the affinity test reads handled() to learn
// which worker served a turn, and the stateful test reads lastJob() to assert
// the reconstructed history the worker actually received.
type recordingExecutor struct {
	workerID string
	models   []types.Model
	// reply is the assistant content echoed for a normal turn.
	reply string
	// toolCall, when non-nil, is returned with finish_reason "tool_calls".
	toolCall *types.ToolCall

	count   atomic.Int64
	mu      sync.Mutex
	lastJob *types.Job
}

func (e *recordingExecutor) Execute(_ context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	e.count.Add(1)
	j := job
	e.mu.Lock()
	e.lastJob = &j
	e.mu.Unlock()

	// Emit the reply as deltas: the server accumulates streamed deltas into the
	// JobResult output for BOTH the streaming and non-streaming submit paths, so a
	// reply only set on JobResult.Output (and never emitted) would be discarded.
	if emit != nil {
		for _, r := range e.reply {
			emit(types.JobChunk{JobID: job.ID, Delta: string(r)})
		}
	}

	res := types.JobResult{
		JobID:            job.ID,
		Output:           e.reply,
		PromptTokens:     1,
		CompletionTokens: 1,
		Tokens:           2,
		FinishReason:     "stop",
	}
	if e.toolCall != nil {
		res.ToolCalls = []types.ToolCall{*e.toolCall}
		res.FinishReason = "tool_calls"
		if emit != nil {
			emit(types.JobChunk{JobID: job.ID, ToolCalls: res.ToolCalls})
		}
	}
	return res
}

func (e *recordingExecutor) ListModels(context.Context) ([]types.Model, error) { return e.models, nil }
func (e *recordingExecutor) Pull(context.Context, string) error                { return nil }

func (e *recordingExecutor) handled() int64 { return e.count.Load() }

func (e *recordingExecutor) job() *types.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastJob
}

// sessionHarness wires a real control-plane gRPC server (with a session.Manager
// shared between the dispatcher's affinity routing and the HTTP session API),
// one or more workers over bufconn, and the HTTP API together. It exposes the
// manager so a test can pre-bind/inspect sessions and a clock-free TTL so
// sessions never idle out mid-test.
type sessionHarness struct {
	url     string
	token   string
	authSvc *auth.Service
	mgr     *session.Manager
	execs   map[string]*recordingExecutor
}

// newSessionHarness builds the harness with one worker per executor (keyed by
// the executor's workerID), all advertising model. The session manager uses a
// long TTL so the sweeper never reaps a session during the test.
func newSessionHarness(t *testing.T, model string, execs ...*recordingExecutor) sessionHarness {
	t.Helper()
	named := make([]namedExecutor, len(execs))
	for i, e := range execs {
		e.models = []types.Model{{Name: model, Digest: "sha256:test"}}
		named[i] = namedExecutor{id: e.workerID, exec: e}
	}
	h := buildSessionHarness(t, model, named)
	byID := make(map[string]*recordingExecutor, len(execs))
	for _, e := range execs {
		byID[e.workerID] = e
	}
	h.execs = byID
	return h
}

// namedExecutor pairs a worker.Executor with the worker id it registers under,
// so buildSessionHarness can wire heterogeneous executor types (recording or
// blocking) onto the same control plane.
type namedExecutor struct {
	id   string
	exec worker.Executor
}

// buildSessionHarness wires the control-plane gRPC server, the shared session
// manager, the HTTP API, and one worker per namedExecutor, then waits for the
// whole fleet to register. It is the common backbone for the session integration
// tests; callers that need typed access to their executors set h.execs.
func buildSessionHarness(t *testing.T, model string, execs []namedExecutor) sessionHarness {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))

	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(0, 0),
		session.WithLogger(discard),
		session.WithTTL(time.Hour),
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
	for _, ne := range execs {
		w := worker.New(worker.Config{
			ServerAddr:        "bufconn",
			WorkerID:          ne.id,
			Models:            models,
			Executor:          ne.exec,
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
	}

	h := sessionHarness{url: ts.URL, token: token, authSvc: authSvc, mgr: mgr}
	// Wait until every worker has registered so dispatch reaches the whole fleet.
	waitFor(t, 3*time.Second, "all workers in fleet", func() bool {
		return len(grpcSrv.Fleet()) == len(execs)
	})
	return h
}

func (h sessionHarness) post(t *testing.T, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (h sessionHarness) get(t *testing.T, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.url+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (h sessionHarness) delete(t *testing.T, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, h.url+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// createSession POSTs /v1/sessions and returns the new session id.
func (h sessionHarness) createSession(t *testing.T, model string) string {
	t.Helper()
	resp := h.post(t, "/v1/sessions", `{"model":"`+model+`"}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session status = %d, want 201", resp.StatusCode)
	}
	var out struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if out.Object != "session" || out.Model != model || !strings.HasPrefix(out.ID, "sess_") {
		t.Fatalf("create session response wrong: %+v", out)
	}
	return out.ID
}

// TestSessionAffinityMode proves AC1: a stateless chat request carrying a
// session id in the X-Session-Id header is pinned to the same worker across
// turns (warm-cache affinity), while the full history the client supplied still
// reaches the worker (the server stores nothing in affinity mode).
func TestSessionAffinityMode(t *testing.T) {
	ea := &recordingExecutor{workerID: "worker-a", reply: "a"}
	eb := &recordingExecutor{workerID: "worker-b", reply: "b"}
	h := newSessionHarness(t, "llama3", ea, eb)

	sid := h.createSession(t, "llama3")

	// First affinity turn: the server binds the session to whichever worker wins;
	// we do not care which, only that the SECOND turn pins to the same one.
	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`
	resp := h.post(t, "/v1/chat/completions", body, map[string]string{"X-Session-Id": sid})
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("turn 1 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	first := boundWorker(t, ea, eb)

	// Second affinity turn with full history again: must land on the same worker.
	body2 := `{"model":"llama3","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x"},{"role":"user","content":"again"}]}`
	resp = h.post(t, "/v1/chat/completions", body2, map[string]string{"X-Session-Id": sid})
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("turn 2 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	second := boundWorker(t, ea, eb)
	if first != second {
		t.Fatalf("affinity broke: turn 1 -> %s, turn 2 -> %s", first, second)
	}
	if ea.handled()+eb.handled() != 2 {
		t.Fatalf("total handled = %d, want 2", ea.handled()+eb.handled())
	}

	// Affinity mode stores NOTHING: GET shows an empty history (the client owns it).
	gresp := h.get(t, h.token, "/v1/sessions/"+sid)
	defer func() { _ = gresp.Body.Close() }()
	var detail sessionDetailWire
	if err := json.NewDecoder(gresp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if len(detail.Messages) != 0 {
		t.Errorf("affinity mode persisted history: %+v", detail.Messages)
	}

	// The full client-supplied history reached the bound worker on turn 2.
	be := h.execs[second]
	job := be.job()
	if job == nil || len(job.Messages) != 3 {
		t.Fatalf("bound worker messages = %+v, want the 3 client-supplied messages", job)
	}
}

// boundWorker returns the workerID of whichever executor most recently handled a
// job. Exactly one of the two should have handled the latest turn.
func boundWorker(t *testing.T, ea, eb *recordingExecutor) string {
	t.Helper()
	switch {
	case ea.handled() > 0 && eb.handled() == 0:
		return ea.workerID
	case eb.handled() > 0 && ea.handled() == 0:
		return eb.workerID
	default:
		// Both handled at least once: the bound one is whichever just incremented.
		// The affinity test compares the bound worker across two turns, so resolve
		// to the worker with the higher count (the pinned one accrues both turns).
		if ea.handled() >= eb.handled() {
			return ea.workerID
		}
		return eb.workerID
	}
}

// sessionDetailWire mirrors the GET /v1/sessions/{id} response for decoding in
// the external test package.
type sessionDetailWire struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Model      string `json:"model"`
	Created    int64  `json:"created"`
	LastActive int64  `json:"last_active"`
	Messages   []struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

// TestSessionStatefulMode proves AC2 + AC3: a stateful session accepts
// new-message-only turns; the worker receives the FULL reconstructed history
// (history + new turn); the new user turn and assistant reply are persisted and
// retrievable via GET; a second turn sees turn 1's context; DELETE ends the
// session (204) and a subsequent GET is 404.
func TestSessionStatefulMode(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "hello there"}
	h := newSessionHarness(t, "llama3", exec)

	sid := h.createSession(t, "llama3")

	// Turn 1: only the new user message + session_id in the body.
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"turn-one"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("turn 1 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The worker saw exactly the one new message (no prior history yet).
	if job := exec.job(); job == nil || len(job.Messages) != 1 || job.Messages[0].Content != "turn-one" {
		t.Fatalf("turn 1 worker messages = %+v, want [turn-one]", exec.job())
	}

	// Turn 2: again only the new message; the worker must now receive the full
	// reconstructed context: user turn-one, assistant hello there, user turn-two.
	resp = h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"turn-two"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("turn 2 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	job := exec.job()
	if job == nil || len(job.Messages) != 3 {
		t.Fatalf("turn 2 reconstructed messages = %+v, want 3", job)
	}
	if job.Messages[0].Content != "turn-one" || job.Messages[0].Role != "user" {
		t.Errorf("reconstructed[0] = %+v, want user/turn-one", job.Messages[0])
	}
	if job.Messages[1].Content != "hello there" || job.Messages[1].Role != "assistant" {
		t.Errorf("reconstructed[1] = %+v, want assistant/hello there", job.Messages[1])
	}
	if job.Messages[2].Content != "turn-two" || job.Messages[2].Role != "user" {
		t.Errorf("reconstructed[2] = %+v, want user/turn-two", job.Messages[2])
	}

	// GET shows all four persisted turns in order.
	gresp := h.get(t, h.token, "/v1/sessions/"+sid)
	var detail sessionDetailWire
	if err := json.NewDecoder(gresp.Body).Decode(&detail); err != nil {
		_ = gresp.Body.Close()
		t.Fatalf("decode get: %v", err)
	}
	_ = gresp.Body.Close()
	wantHistory := []struct{ role, content string }{
		{"user", "turn-one"},
		{"assistant", "hello there"},
		{"user", "turn-two"},
		{"assistant", "hello there"},
	}
	if len(detail.Messages) != len(wantHistory) {
		t.Fatalf("GET history = %d turns, want %d: %+v", len(detail.Messages), len(wantHistory), detail.Messages)
	}
	for i, want := range wantHistory {
		if detail.Messages[i].Role != want.role || detail.Messages[i].Content != want.content {
			t.Errorf("history[%d] = %s/%q, want %s/%q", i,
				detail.Messages[i].Role, detail.Messages[i].Content, want.role, want.content)
		}
	}

	// DELETE ends the session (204) and purges history.
	dresp := h.delete(t, h.token, "/v1/sessions/"+sid)
	if dresp.StatusCode != http.StatusNoContent {
		_ = dresp.Body.Close()
		t.Fatalf("delete status = %d, want 204", dresp.StatusCode)
	}
	_ = dresp.Body.Close()

	// A subsequent GET is 404.
	gresp = h.get(t, h.token, "/v1/sessions/"+sid)
	if gresp.StatusCode != http.StatusNotFound {
		_ = gresp.Body.Close()
		t.Fatalf("get after delete status = %d, want 404", gresp.StatusCode)
	}
	_ = gresp.Body.Close()
}

// TestSessionStatefulStreaming proves AC4 (streaming): a stateful streaming turn
// reconstructs context, streams SSE frames, and persists the assistant reply
// after the terminal chunk so a subsequent turn sees it.
func TestSessionStatefulStreaming(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "stream-reply"}
	h := newSessionHarness(t, "llama3", exec)

	sid := h.createSession(t, "llama3")

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","stream":true,"session_id":"`+sid+`","messages":[{"role":"user","content":"go"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	frames, done := readSSE(t, resp.Body)
	_ = resp.Body.Close()
	if !done {
		t.Fatalf("stream did not end with [DONE]")
	}
	var content strings.Builder
	for _, f := range frames {
		var fr struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(f, &fr); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if len(fr.Choices) > 0 {
			content.WriteString(fr.Choices[0].Delta.Content)
		}
	}
	if content.String() != "stream-reply" {
		t.Errorf("streamed content = %q, want stream-reply", content.String())
	}

	// The streamed assistant reply was persisted after the terminal chunk. Poll:
	// the SSE body is drained, but the detached persistence write may race the
	// next read by a hair.
	waitFor(t, 2*time.Second, "streamed turn persisted", func() bool {
		hist := h.history(t, sid)
		return len(hist) == 2 && hist[1].Role == "assistant" && hist[1].Content == "stream-reply"
	})

	// A follow-up streaming turn must see turn 1 in the reconstructed context.
	resp = h.post(t, "/v1/chat/completions",
		`{"model":"llama3","stream":true,"session_id":"`+sid+`","messages":[{"role":"user","content":"more"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("stream turn 2 status = %d, want 200", resp.StatusCode)
	}
	_, _ = readSSE(t, resp.Body)
	_ = resp.Body.Close()

	job := exec.job()
	if job == nil || len(job.Messages) != 3 {
		t.Fatalf("stream turn 2 reconstructed = %+v, want 3 messages", job)
	}
	if job.Messages[0].Content != "go" || job.Messages[1].Content != "stream-reply" || job.Messages[2].Content != "more" {
		t.Errorf("stream turn 2 context wrong: %+v", job.Messages)
	}
}

// TestSessionStatefulStreamingDisconnect proves the blocking-defect fix: when a
// client disconnects mid-stream (after deltas land but BEFORE any terminal
// chunk), NEITHER the new user turn NOR a partial assistant turn is persisted, so
// the session history stays consistent and the turn can be retried. The blocking
// executor emits two deltas then waits on its context; the test cancels the
// client request once the deltas are in flight, modelling a genuine abort with no
// error frame and no Done chunk.
func TestSessionStatefulStreamingDisconnect(t *testing.T) {
	emitted := make(chan struct{})
	// A blocking fake: it emits two deltas, signals via emitted, then waits on the
	// context (no release channel) — never sending a terminal chunk, so a client
	// disconnect mid-stream is the only thing that unblocks it.
	exec := testutil.NewFakeExecutor(
		testutil.WithExecModelObjects(types.Model{Name: "llama3", Digest: "sha256:test"}),
		testutil.WithDeltas("par", "tial"),
		testutil.WithEmitSignal(emitted),
		testutil.WithBlock(nil),
	)
	const workerID = "worker-1"
	h := buildSessionHarness(t, "llama3", []namedExecutor{{id: workerID, exec: exec}})

	sid := h.createSession(t, "llama3")

	// History is empty before the turn; it must be unchanged after the disconnect.
	if hist := h.history(t, sid); len(hist) != 0 {
		t.Fatalf("pre-turn history = %d, want 0", len(hist))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+"/v1/chat/completions",
		strings.NewReader(`{"model":"llama3","stream":true,"session_id":"`+sid+`","messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	// Wait until the executor has emitted its deltas (tokens are in flight), then
	// abort the client request mid-stream — before any terminal/Done chunk.
	select {
	case <-emitted:
	case <-time.After(3 * time.Second):
		_ = resp.Body.Close()
		t.Fatal("executor never emitted deltas")
	}
	cancel()
	_ = resp.Body.Close()

	// The executor unblocks once its context is cancelled (client disconnect tears
	// down the upstream job). Wait for that so any erroneous persistence would have
	// had its chance to run.
	waitFor(t, 3*time.Second, "executor observed disconnect", func() bool {
		return exec.Handled() == 1
	})

	// The disconnected turn persisted NOTHING: no orphaned user turn, no partial
	// assistant turn. History stays empty so the client can retry cleanly. Hold
	// the assertion across a short window to catch a late detached write.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if hist := h.history(t, sid); len(hist) != 0 {
			t.Fatalf("disconnect persisted a turn: %+v", hist)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSessionStatefulCrossOwner proves a stateful chat naming a session owned by
// a DIFFERENT key is rejected with 404 BEFORE any dispatch (no existence leak, no
// wasted inference): the executor handles zero jobs.
func TestSessionStatefulCrossOwner(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "ok"}
	h := newSessionHarness(t, "llama3", exec)

	// The session belongs to the harness's admin token.
	sid := h.createSession(t, "llama3")

	// A second, distinct key tries to drive a stateful turn against it.
	otherToken, _, err := h.authSvc.CreateWithPermissions(context.Background(), "intruder",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"intrude"}]}`,
		map[string]string{"Authorization": "Bearer " + otherToken})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner stateful chat status = %d, want 404", resp.StatusCode)
	}
	if exec.handled() != 0 {
		t.Errorf("worker handled %d jobs, want 0 (no dispatch for cross-owner session)", exec.handled())
	}
}

// history reads a session's stored turns via the manager (admin owns it under
// the harness's admin token). It is a test convenience for polling persistence.
func (h sessionHarness) history(t *testing.T, sid string) []struct{ Role, Content string } {
	t.Helper()
	resp := h.get(t, h.token, "/v1/sessions/"+sid)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var detail sessionDetailWire
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	out := make([]struct{ Role, Content string }, len(detail.Messages))
	for i, m := range detail.Messages {
		out[i] = struct{ Role, Content string }{m.Role, m.Content}
	}
	return out
}

// TestSessionStatefulToolCalling proves AC4 (function calling across turns): a
// stateful turn whose assistant reply is a tool call persists the tool_calls,
// the client sends the tool result as the next turn, and the worker sees the
// full reconstructed context including the prior tool call + result.
func TestSessionStatefulToolCalling(t *testing.T) {
	exec := &recordingExecutor{
		workerID: "worker-1",
		toolCall: &types.ToolCall{ID: "call_1", Type: "function", FunctionName: "get_weather", Arguments: `{"city":"paris"}`},
	}
	h := newSessionHarness(t, "llama3", exec)

	sid := h.createSession(t, "llama3")

	// Turn 1: the assistant decides to call a tool.
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"user","content":"weather in paris?"}],
		  "tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("tool turn 1 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The persisted assistant turn carries the tool call.
	hist := h.historyDetail(t, sid)
	if len(hist.Messages) != 2 {
		t.Fatalf("after tool turn 1, history = %d, want 2 (user, assistant tool_call)", len(hist.Messages))
	}
	asst := hist.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("persisted assistant tool call wrong: %+v", asst)
	}

	// Turn 2: the client returns the tool result as a tool message. The worker
	// must now see user, assistant tool_call, tool result.
	exec.toolCall = nil // this turn the model answers normally
	exec.reply = "it is sunny"
	resp = h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"`+sid+`","messages":[{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"sunny"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("tool turn 2 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	job := exec.job()
	if job == nil || len(job.Messages) != 3 {
		t.Fatalf("tool turn 2 reconstructed = %+v, want 3 messages", job)
	}
	if job.Messages[1].Role != "assistant" || len(job.Messages[1].ToolCalls) != 1 {
		t.Errorf("reconstructed assistant tool call lost: %+v", job.Messages[1])
	}
	if job.Messages[2].Role != "tool" || job.Messages[2].ToolCallID != "call_1" {
		t.Errorf("reconstructed tool result wrong: %+v", job.Messages[2])
	}
}

func (h sessionHarness) historyDetail(t *testing.T, sid string) sessionDetailWire {
	t.Helper()
	resp := h.get(t, h.token, "/v1/sessions/"+sid)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var detail sessionDetailWire
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return detail
}

// TestSessionOwnerScoping proves a session is invisible to a different key: GET
// and DELETE on another owner's session both return 404 (no existence leak).
func TestSessionOwnerScoping(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "ok"}
	h := newSessionHarness(t, "llama3", exec)

	sid := h.createSession(t, "llama3")

	// A second, distinct key.
	otherToken, _, err := h.authSvc.CreateWithPermissions(context.Background(), "intruder",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}

	gresp := h.get(t, otherToken, "/v1/sessions/"+sid)
	if gresp.StatusCode != http.StatusNotFound {
		_ = gresp.Body.Close()
		t.Fatalf("cross-owner GET status = %d, want 404", gresp.StatusCode)
	}
	_ = gresp.Body.Close()

	dresp := h.delete(t, otherToken, "/v1/sessions/"+sid)
	if dresp.StatusCode != http.StatusNotFound {
		_ = dresp.Body.Close()
		t.Fatalf("cross-owner DELETE status = %d, want 404", dresp.StatusCode)
	}
	_ = dresp.Body.Close()

	// The real owner still sees it.
	gresp = h.get(t, h.token, "/v1/sessions/"+sid)
	if gresp.StatusCode != http.StatusOK {
		_ = gresp.Body.Close()
		t.Fatalf("owner GET status = %d, want 200", gresp.StatusCode)
	}
	_ = gresp.Body.Close()
}

// TestSessionStatefulUnknownSession proves a chat request referencing a session
// the key does not own (or that does not exist) is rejected with 404 before any
// dispatch.
func TestSessionStatefulUnknownSession(t *testing.T) {
	exec := &recordingExecutor{workerID: "worker-1", reply: "ok"}
	h := newSessionHarness(t, "llama3", exec)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"llama3","session_id":"sess_doesnotexist","messages":[{"role":"user","content":"hi"}]}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if exec.handled() != 0 {
		t.Errorf("worker handled %d jobs, want 0 (no dispatch for unknown session)", exec.handled())
	}
}
