package server_test

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// harness wires one Server to an in-process bufconn listener. It can "drop" the
// live connection (hard-stop and restart the gRPC server on a fresh listener)
// to simulate a transient network failure and exercise worker reconnect.
type harness struct {
	t   *testing.T
	srv *server.Server

	mu  sync.Mutex
	lis *bufconn.Listener
	gs  *grpc.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t}
	h.srv = server.New()
	h.start()
	return h
}

func (h *harness) start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lis = bufconn.Listen(1 << 20)
	h.gs = grpc.NewServer()
	h.srv.Register(h.gs)
	lis := h.lis
	gs := h.gs
	go func() { _ = gs.Serve(lis) }()
}

// dropConnection severs all live streams by hard-stopping the server, then
// starts a fresh server so the worker's reconnect attempts succeed.
func (h *harness) dropConnection() {
	h.mu.Lock()
	gs := h.gs
	h.mu.Unlock()
	gs.Stop()
	h.start()
}

// dialOption returns a gRPC dialer that always reaches the *current* listener,
// so reconnects after dropConnection land on the new server.
func (h *harness) dialOption() grpc.DialOption {
	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		h.mu.Lock()
		lis := h.lis
		h.mu.Unlock()
		return lis.DialContext(ctx)
	})
}

func (h *harness) close() {
	h.mu.Lock()
	gs := h.gs
	h.mu.Unlock()
	gs.Stop()
}

func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

func newWorker(h *harness, onConnect func(sessionID string)) *worker.Worker {
	return newWorkerID(h, "worker-1", onConnect)
}

func newWorkerID(h *harness, id string, onConnect func(sessionID string)) *worker.Worker {
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          id,
		Models:            []types.Model{{Name: "llama3"}},
		HeartbeatInterval: 20 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			h.dialOption(),
		},
	})
	if onConnect != nil {
		w.OnConnect(onConnect)
	}
	return w
}

// TestControlPlaneRoundTrip covers the core acceptance criterion: a worker
// registers, the server dispatches a trivial job over the bidi stream, and the
// worker's result comes back.
func TestControlPlaneRoundTrip(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newWorker(h, nil)
	go func() { _ = w.Run(ctx) }()

	// Wait for the worker to register.
	waitFor(t, 2*time.Second, "worker to register", func() bool {
		return h.srv.WorkerCount() == 1
	})

	res, err := h.srv.SubmitJob(ctx, types.Job{ID: "job-1", Model: "llama3", Prompt: "ping"})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if res.JobID != "job-1" {
		t.Fatalf("result job id = %q", res.JobID)
	}
	if res.Output != "echo: ping" {
		t.Fatalf("result output = %q, want echo of prompt", res.Output)
	}
}

// TestNoWorkersIsError verifies SubmitJob fails cleanly with no workers.
func TestNoWorkersIsError(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	_, err := h.srv.SubmitJob(context.Background(), types.Job{ID: "j", Model: "m"})
	if err != server.ErrNoWorkers {
		t.Fatalf("err = %v, want ErrNoWorkers", err)
	}
}

// TestReconnectAfterDrop verifies the connection survives a transient drop:
// after the stream is severed, the worker reconnects with backoff and the
// server can dispatch a job again.
func TestReconnectAfterDrop(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var connects int32
	w := newWorker(h, func(string) { atomic.AddInt32(&connects, 1) })
	go func() { _ = w.Run(ctx) }()

	// First connection.
	waitFor(t, 2*time.Second, "initial registration", func() bool {
		return atomic.LoadInt32(&connects) >= 1 && h.srv.WorkerCount() == 1
	})

	// Sanity: a job works before the drop.
	if _, err := h.srv.SubmitJob(ctx, types.Job{ID: "before", Model: "llama3", Prompt: "x"}); err != nil {
		t.Fatalf("pre-drop job: %v", err)
	}

	// Simulate a transient network failure.
	h.dropConnection()

	// The worker must reconnect (second registration) and the registry must
	// hold exactly one worker again.
	waitFor(t, 5*time.Second, "reconnection after drop", func() bool {
		return atomic.LoadInt32(&connects) >= 2 && h.srv.WorkerCount() == 1
	})

	// A job dispatched after the reconnect must succeed end-to-end.
	res, err := h.srv.SubmitJob(ctx, types.Job{ID: "after", Model: "llama3", Prompt: "pong"})
	if err != nil {
		t.Fatalf("post-reconnect job: %v", err)
	}
	if res.Output != "echo: pong" {
		t.Fatalf("post-reconnect output = %q", res.Output)
	}
}

// TestConcurrentWorkersUniqueSessions connects many workers concurrently and
// asserts each receives a distinct session ID. This is the end-to-end guard
// for the nextSes data race: concurrent Connect goroutines must not hand out
// duplicate session numbers. Run with -race to catch the underlying race.
func TestConcurrentWorkersUniqueSessions(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 16

	var mu sync.Mutex
	sessions := make(map[string]bool)
	var connects int32

	for i := 0; i < n; i++ {
		w := newWorkerID(h, fmt.Sprintf("worker-%d", i), func(sessionID string) {
			mu.Lock()
			if sessions[sessionID] {
				mu.Unlock()
				t.Errorf("duplicate session id handed out: %q", sessionID)
				return
			}
			sessions[sessionID] = true
			mu.Unlock()
			atomic.AddInt32(&connects, 1)
		})
		go func() { _ = w.Run(ctx) }()
	}

	waitFor(t, 5*time.Second, "all workers to register", func() bool {
		return atomic.LoadInt32(&connects) == n && h.srv.WorkerCount() == n
	})

	mu.Lock()
	got := len(sessions)
	mu.Unlock()
	if got != n {
		t.Fatalf("expected %d unique session ids, got %d", n, got)
	}
}

// TestNoGoroutineLeakAcrossReconnects guards the per-connection goroutine
// teardown: every reconnect cycle spawns receive/job-worker/writer goroutines,
// and before the fix the job-worker goroutine only observed the long-lived Run
// context, leaking one goroutine per reconnect. After many forced reconnects
// the live goroutine count must return near its baseline.
func TestNoGoroutineLeakAcrossReconnects(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	ctx, cancel := context.WithCancel(context.Background())

	var connects int32
	w := newWorkerID(h, "leak-worker", func(string) { atomic.AddInt32(&connects, 1) })
	go func() { _ = w.Run(ctx) }()

	waitFor(t, 2*time.Second, "initial registration", func() bool {
		return atomic.LoadInt32(&connects) >= 1 && h.srv.WorkerCount() == 1
	})

	// Let startup goroutines settle, then snapshot the baseline.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const cycles = 50
	for i := 0; i < cycles; i++ {
		prev := atomic.LoadInt32(&connects)
		h.dropConnection()
		waitFor(t, 5*time.Second, fmt.Sprintf("reconnect %d", i), func() bool {
			return atomic.LoadInt32(&connects) > prev && h.srv.WorkerCount() == 1
		})
	}

	// Allow the final cycle's torn-down goroutines to exit.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// Tolerance covers scheduler jitter and transient gRPC goroutines; a real
	// leak grows ~1 (or more) per cycle and would blow far past this.
	const tolerance = 15
	if after > baseline+tolerance {
		t.Fatalf("goroutine leak across %d reconnects: baseline=%d after=%d (delta=%d, tolerance=%d)",
			cycles, baseline, after, after-baseline, tolerance)
	}

	// Stop the worker and ensure Run unwinds cleanly.
	cancel()
	waitFor(t, 2*time.Second, "worker registry to drain", func() bool {
		return h.srv.WorkerCount() == 0
	})
}
