package server_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// blockingExecutor holds each job until released, so a test can observe a job
// counted as active in the worker's heartbeats while it is in flight.
type blockingExecutor struct{ release chan struct{} }

func (b blockingExecutor) Execute(ctx context.Context, job types.Job) types.JobResult {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return types.JobResult{JobID: job.ID, Output: "done"}
}

// testClock is a mutable, mutex-guarded clock so the eviction loop's staleness
// decisions can be fast-forwarded without real sleeps.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(start time.Time) *testClock { return &testClock{t: start} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// rawClient is a low-level control-plane stream a test can drive directly: it
// registers, then sends exactly the heartbeats the test chooses (so a worker
// can be made to "go silent" while keeping its stream open, isolating stale
// eviction from stream-close cleanup).
type rawClient struct {
	conn   *grpc.ClientConn
	stream agentgpuv1.ControlPlane_ConnectClient
	// recvd carries server->worker messages so tests can await a dispatch
	// without blocking a waitFor cond on a bare stream.Recv.
	recvd chan *agentgpuv1.ServerMessage
}

func dialRaw(t *testing.T, h *harness, id string, models []types.Model) *rawClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		h.dialOption(),
	)
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	stream, err := agentgpuv1.NewControlPlaneClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatalf("connect raw: %v", err)
	}
	if err := stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Register{
			Register: &agentgpuv1.Register{WorkerId: id, Models: types.ModelsToProto(models)},
		},
	}); err != nil {
		t.Fatalf("register raw: %v", err)
	}
	if _, err := stream.Recv(); err != nil { // RegisterAck
		t.Fatalf("recv ack: %v", err)
	}
	rc := &rawClient{conn: conn, stream: stream, recvd: make(chan *agentgpuv1.ServerMessage, 8)}
	// Background reader: forwards post-ack server messages (job dispatches) to
	// recvd. Exits when the stream closes.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				close(rc.recvd)
				return
			}
			rc.recvd <- msg
		}
	}()
	return rc
}

// awaitJob blocks until the worker receives a job dispatch (or fails the test).
func (r *rawClient) awaitJob(t *testing.T) *agentgpuv1.Job {
	t.Helper()
	for {
		select {
		case msg, ok := <-r.recvd:
			if !ok {
				t.Fatal("stream closed before job dispatch")
			}
			if job := msg.GetJob(); job != nil {
				return job
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for job dispatch")
		}
	}
}

func (r *rawClient) heartbeat(t *testing.T, hb types.Heartbeat) {
	t.Helper()
	if err := r.stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Heartbeat{Heartbeat: hb.Proto()},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
}

func (r *rawClient) close() { _ = r.conn.Close() }

// fleetByID returns the snapshot for id and whether it was found.
func fleetByID(srv *server.Server, id string) (types.Worker, bool) {
	for _, w := range srv.Fleet() {
		if w.ID == id {
			return w, true
		}
	}
	return types.Worker{}, false
}

// TestFleetReflectsRegistrationAndHeartbeat covers AC1 and AC2: a registered
// worker appears in Fleet(), and a heartbeat's capacity fields (VRAM, load,
// active jobs, models) become visible in the fleet snapshot.
func TestFleetReflectsRegistrationAndHeartbeat(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "cap-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()

	// AC1: appears in the fleet after registration.
	waitFor(t, 2*time.Second, "worker in fleet", func() bool {
		_, ok := fleetByID(h.srv, "cap-worker")
		return ok
	})
	w, _ := fleetByID(h.srv, "cap-worker")
	if w.Status != types.WorkerOnline {
		t.Fatalf("status = %v, want online", w.Status)
	}

	// AC2: a heartbeat updates capacity fields visibly.
	rc.heartbeat(t, types.Heartbeat{
		WorkerID:        "cap-worker",
		ActiveJobs:      2,
		TotalVRAM:       24 << 30,
		FreeVRAM:        16 << 30,
		Load:            42,
		GPUType:         "NVIDIA RTX 4090",
		AvailableModels: []types.Model{{Name: "llama3"}, {Name: "mistral"}},
	})

	waitFor(t, 2*time.Second, "heartbeat capacity to surface", func() bool {
		w, ok := fleetByID(h.srv, "cap-worker")
		return ok && w.Load == 42 && w.ActiveJobs == 2
	})
	w, _ = fleetByID(h.srv, "cap-worker")
	if w.TotalVRAM != 24<<30 || w.FreeVRAM != 16<<30 {
		t.Fatalf("vram total=%d free=%d, want 24Gi/16Gi", w.TotalVRAM, w.FreeVRAM)
	}
	if w.GPUType != "NVIDIA RTX 4090" {
		t.Fatalf("gpu type = %q", w.GPUType)
	}
	if len(w.Models) != 2 {
		t.Fatalf("available models = %d, want 2", len(w.Models))
	}
	if !w.LastSeen.Equal(clk.now()) {
		t.Fatalf("last seen = %v, want clock %v", w.LastSeen, clk.now())
	}
}

// TestStaleWorkerEvictedAndPendingFails covers AC3: a worker that stops
// heartbeating is evicted within the configurable timeout, and its in-flight
// pending job fails with worker_stale. The injected clock is fast-forwarded so
// no real time passes.
func TestStaleWorkerEvictedAndPendingFails(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	const timeout = 30 * time.Second
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(timeout),
		server.WithEvictScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "stale-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()

	// Wait for registration. lastHeartbeat is seeded at the current (pre-advance)
	// clock at registration, so we deliberately send no heartbeat here: an
	// in-flight heartbeat processed after clk.advance would re-stamp lastHeartbeat
	// with the advanced time and reset staleness.
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Submit a job; the raw client never sends a result, so it stays pending.
	jobErr := make(chan error, 1)
	go func() {
		_, err := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "stuck", Model: "llama3", Prompt: "x"})
		jobErr <- err
	}()

	// Ensure the job is actually pending on the worker before we evict.
	rc.awaitJob(t)

	// Fast-forward past the timeout; the eviction loop must remove the worker.
	clk.advance(timeout + time.Second)

	waitFor(t, 2*time.Second, "stale worker evicted", func() bool {
		return h.srv.WorkerCount() == 0
	})

	// AC3: the pending job fails with a worker_stale error.
	select {
	case err := <-jobErr:
		var je *types.JobError
		if !errors.As(err, &je) || je.Code != "worker_stale" {
			t.Fatalf("pending job err = %v, want worker_stale JobError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending job did not fail after eviction")
	}
}

// TestDrainStopsRoutingButFinishesInFlight covers AC4: draining a worker stops
// new routing (pickWorker skips it) while an already-pending job is allowed to
// finish; once the worker closes the stream it is removed.
func TestDrainStopsRoutingButFinishesInFlight(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "drain-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()
	rc.heartbeat(t, types.Heartbeat{WorkerID: "drain-worker"})
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Dispatch a job and read it on the worker so it is pending in-flight.
	resCh := make(chan types.JobResult, 1)
	go func() {
		res, _ := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "inflight", Model: "llama3", Prompt: "x"})
		resCh <- res
	}()
	job := rc.awaitJob(t)

	// Worker requests graceful drain.
	if err := rc.stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Deregister{
			Deregister: &agentgpuv1.Deregister{WorkerId: "drain-worker"},
		},
	}); err != nil {
		t.Fatalf("send deregister: %v", err)
	}

	// Wait for the server to observe the drain (fleet reports draining, still
	// present and not evicted).
	waitFor(t, 2*time.Second, "worker marked draining", func() bool {
		w, ok := fleetByID(h.srv, "drain-worker")
		return ok && w.Status == types.WorkerDraining
	})

	// AC4: while draining, no new routing — pickWorker skips it. Use a bounded
	// context so this never blocks even if a stray job were enqueued.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), time.Second)
	defer cancelProbe()
	if _, err := h.srv.SubmitJob(probeCtx,
		types.Job{ID: "new", Model: "llama3", Prompt: "y"}); !errors.Is(err, server.ErrNoWorkers) {
		t.Fatalf("dispatch to draining worker = %v, want ErrNoWorkers", err)
	}

	// The in-flight job must still be allowed to finish: send its result.
	rc.heartbeat(t, types.Heartbeat{WorkerID: "drain-worker"}) // still alive
	if err := rc.stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Result{
			Result: types.JobResult{JobID: job.GetId(), Output: "done"}.Proto(),
		},
	}); err != nil {
		t.Fatalf("send result: %v", err)
	}
	select {
	case res := <-resCh:
		if res.Output != "done" {
			t.Fatalf("in-flight result output = %q, want done", res.Output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight job did not complete during drain")
	}

	// Closing the stream removes the worker entirely.
	rc.close()
	waitFor(t, 2*time.Second, "drained worker removed", func() bool {
		return h.srv.WorkerCount() == 0
	})
}

// TestDrainWorkerAdminSeam covers the DrainWorker admin method (#4 seam):
// an operator-initiated drain marks the worker draining so it is skipped by
// the router, and an unknown id returns ErrWorkerNotFound.
func TestDrainWorkerAdminSeam(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	if err := h.srv.DrainWorker("missing"); !errors.Is(err, server.ErrWorkerNotFound) {
		t.Fatalf("DrainWorker(missing) = %v, want ErrWorkerNotFound", err)
	}

	rc := dialRaw(t, h, "admin-drain", []types.Model{{Name: "llama3"}})
	defer rc.close()
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	if err := h.srv.DrainWorker("admin-drain"); err != nil {
		t.Fatalf("DrainWorker: %v", err)
	}
	w, ok := fleetByID(h.srv, "admin-drain")
	if !ok || w.Status != types.WorkerDraining {
		t.Fatalf("fleet status = %v ok=%v, want draining", w.Status, ok)
	}
	if _, err := h.srv.SubmitJob(context.Background(),
		types.Job{ID: "j", Model: "llama3", Prompt: "x"}); !errors.Is(err, server.ErrNoWorkers) {
		t.Fatalf("dispatch to drained worker = %v, want ErrNoWorkers", err)
	}
}

// TestWorkerReportsCapacityAndActiveJobs uses the real worker.Worker to verify
// the end-to-end heartbeat path: configured capacity stub fields and the live
// active-job count surface in the server's fleet view (AC2). It exercises the
// worker's heartbeat construction and active-job accounting, not just a raw
// stream.
func TestWorkerReportsCapacityAndActiveJobs(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	w := newWorkerWithCapacity(h, "real-worker", worker.Config{
		Models:    []types.Model{{Name: "llama3"}},
		TotalVRAM: 8 << 30,
		FreeVRAM:  6 << 30,
		GPUType:   "test-gpu",
		Load:      7,
		Executor:  blockingExecutor{release: release},
	})
	go func() { _ = w.Run(ctx) }()

	waitFor(t, 2*time.Second, "worker to register", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Capacity stub fields surface in the fleet view via heartbeats.
	waitFor(t, 2*time.Second, "capacity heartbeat", func() bool {
		fw, ok := fleetByID(h.srv, "real-worker")
		return ok && fw.GPUType == "test-gpu" && fw.TotalVRAM == 8<<30 && fw.Load == 7
	})

	// Dispatch a job that blocks in the executor; the worker must report it as
	// active in a subsequent heartbeat.
	done := make(chan struct{})
	go func() {
		_, _ = h.srv.SubmitJob(context.Background(),
			types.Job{ID: "blocking", Model: "llama3", Prompt: "x"})
		close(done)
	}()
	waitFor(t, 2*time.Second, "active job reported", func() bool {
		fw, ok := fleetByID(h.srv, "real-worker")
		return ok && fw.ActiveJobs == 1
	})

	// Release the job; active count returns to zero.
	close(release)
	<-done
	waitFor(t, 2*time.Second, "active job cleared", func() bool {
		fw, ok := fleetByID(h.srv, "real-worker")
		return ok && fw.ActiveJobs == 0
	})
}

// TestWorkerGracefulDeregister verifies the real worker reliably sends a
// Deregister on graceful shutdown (Run context cancelled), so the server marks
// it draining before the stream tears down (AC4, worker side).
//
// Asserting only that the worker is eventually gone is insufficient: the
// stream-close removeWorker path drops the worker regardless of whether a
// Deregister was ever sent, so WorkerCount()==0 cannot distinguish a graceful
// drain from a bare disconnect. We instead assert the server observes the
// Deregister-driven draining transition. The server's drain observer fires
// synchronously in the reader loop the instant markDraining runs — strictly
// before the deferred removeWorker on stream close — giving a race-free
// synchronization point that does not depend on catching the brief
// draining→removed window through a polling race.
func TestWorkerGracefulDeregister(t *testing.T) {
	// drainStatus carries the fleet status of the draining worker, captured from
	// inside the drain observer. The observer runs synchronously on the server's
	// reader goroutine the instant the worker is marked draining — after
	// markDraining but strictly before the deferred removeWorker (which only runs
	// once the reader loop sees the post-Deregister stream close). So a Fleet()
	// snapshot taken here is guaranteed to still contain the worker and to report
	// WorkerDraining, giving a race-free assertion that needs no polling window.
	type drainEvent struct {
		id     string
		status types.WorkerStatus
		found  bool
	}
	drained := make(chan drainEvent, 1)
	var h *harness
	h = newHarnessWith(t,
		server.WithHeartbeatTimeout(time.Minute),
		server.WithDrainObserver(func(id string) {
			w, ok := fleetByID(h.srv, id)
			select {
			case drained <- drainEvent{id: id, status: w.Status, found: ok}:
			default:
			}
		}),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())

	w := newWorkerWithCapacity(h, "graceful-worker", worker.Config{
		Models: []types.Model{{Name: "llama3"}},
	})
	go func() { _ = w.Run(ctx) }()

	waitFor(t, 2*time.Second, "worker to register", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Cancel Run: the worker must emit Deregister before closing the stream. The
	// server processes the Deregister (marks draining) and only then removeWorker
	// on stream close.
	cancel()

	// The graceful Deregister must reach the server and drive the draining
	// transition. Against the racy worker (where the graceful-shutdown branch was
	// stolen ~50% of the time by a competing select case, or the Deregister was
	// truncated when the stream's context was cancelled out from under it) this
	// observer frequently never fires and the test fails.
	select {
	case ev := <-drained:
		if ev.id != "graceful-worker" {
			t.Fatalf("drain observer fired for %q, want graceful-worker", ev.id)
		}
		if !ev.found {
			t.Fatal("draining worker absent from fleet at drain time")
		}
		if ev.status != types.WorkerDraining {
			t.Fatalf("worker status at drain = %v, want WorkerDraining", ev.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed a graceful Deregister/draining transition")
	}

	// And it is eventually removed once the stream tears down.
	waitFor(t, 2*time.Second, "worker removed after graceful shutdown", func() bool {
		return h.srv.WorkerCount() == 0
	})
}
