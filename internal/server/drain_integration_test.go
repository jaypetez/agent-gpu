package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// Tests for the timed/forced drain added in #93 (DrainWorkerWithDeadline). They
// drive a real control-plane stream via the bufconn harness and rawClient so the
// forced-eviction goroutine, the in-flight wait, and the stream teardown are all
// exercised end-to-end. The forced-drain deadline uses real wall-clock time (the
// reaper's timer is not on the injected clock), so deadlines here are kept small.

// TestForcedDrainEvictsAfterJobsFinish covers AC4: a timed drain force-evicts the
// worker once its in-flight jobs reach zero, BEFORE the deadline. The in-flight
// job completes normally (it is not failed), and the worker is removed without
// waiting out the deadline.
func TestForcedDrainEvictsAfterJobsFinish(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "drain-finish", []types.Model{{Name: "llama3"}})
	defer rc.close()
	rc.heartbeat(t, types.Heartbeat{WorkerID: "drain-finish"})
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

	// Drain with a generous deadline: the forced eviction must come from in-flight
	// reaching zero, not from the deadline. A 30s deadline would fail the test if
	// the reaper waited for it.
	if err := h.srv.DrainWorkerWithDeadline("drain-finish", 30*time.Second); err != nil {
		t.Fatalf("DrainWorkerWithDeadline: %v", err)
	}

	// The worker is draining but NOT yet evicted (its job is still in flight).
	waitFor(t, 2*time.Second, "worker draining", func() bool {
		w, ok := fleetByID(h.srv, "drain-finish")
		return ok && w.Status == types.WorkerDraining
	})

	// Finish the in-flight job: in-flight drops to zero, so the reaper force-evicts
	// well within the (much longer) deadline.
	if err := rc.stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Result{
			Result: types.JobResult{JobID: job.GetId(), Output: "done"}.Proto(),
		},
	}); err != nil {
		t.Fatalf("send result: %v", err)
	}

	// The original job completes normally (NOT failed by the drain).
	select {
	case res := <-resCh:
		if res.Err != nil || res.Output != "done" {
			t.Fatalf("in-flight result = %+v, want output=done no err", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight job did not complete")
	}

	// And the worker is force-evicted once idle, without waiting out the deadline.
	start := time.Now()
	waitFor(t, 2*time.Second, "worker evicted after jobs finished", func() bool {
		return h.srv.WorkerCount() == 0
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("eviction took %v; reaper appears to have waited for the deadline", elapsed)
	}
}

// TestForcedDrainEvictsAfterDeadline covers AC4: a timed drain force-evicts the
// worker once the deadline elapses EVEN IF jobs are still in flight, and the
// stuck pending job is failed with worker_evicted. The soft drain (no deadline)
// would leave such a worker pinned indefinitely; the deadline is the difference.
func TestForcedDrainEvictsAfterDeadline(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "drain-deadline", []types.Model{{Name: "llama3"}})
	defer rc.close()
	rc.heartbeat(t, types.Heartbeat{WorkerID: "drain-deadline"})
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Dispatch a job the worker never finishes, so it stays pending in-flight.
	jobErr := make(chan error, 1)
	go func() {
		_, err := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "stuck", Model: "llama3", Prompt: "x"})
		jobErr <- err
	}()
	rc.awaitJob(t)

	// Drain with a short real-time deadline; in-flight never reaches zero, so the
	// eviction must come from the deadline elapsing.
	if err := h.srv.DrainWorkerWithDeadline("drain-deadline", 150*time.Millisecond); err != nil {
		t.Fatalf("DrainWorkerWithDeadline: %v", err)
	}

	// The deadline elapses → the worker is force-evicted despite the in-flight job.
	waitFor(t, 2*time.Second, "worker evicted after deadline", func() bool {
		return h.srv.WorkerCount() == 0
	})

	// The stuck pending job is failed with worker_evicted (distinct from the
	// worker_stale used by heartbeat eviction and worker_disconnected used by a
	// bare stream close).
	select {
	case err := <-jobErr:
		var je *types.JobError
		if !errors.As(err, &je) || je.Code != "worker_evicted" {
			t.Fatalf("stuck job err = %v, want worker_evicted JobError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stuck job did not fail after forced eviction")
	}

	// The forced evict must actually TEAR DOWN the RPC, not merely deregister the
	// worker: the client's stream is closed, so its background reader observes the
	// error and closes recvd. This proves the Connect reader/recv goroutines exit
	// (no stream leak) rather than parking in stream.Recv() for an evicted worker.
	select {
	case _, open := <-rc.recvd:
		if open {
			// A buffered message may arrive first; drain until the channel closes.
			for range rc.recvd {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forced evict did not close the worker stream (RPC left open)")
	}
}

// TestForcedDrainReaperStopsOnDisconnect proves the forced-drain reaper does not
// leak or misfire when the worker disconnects on its own before the deadline: the
// reaper observes the stream teardown (w.ctx.Done) and exits. Removal here comes
// from the natural stream-close path, and a -race run over this test exercises the
// reaper/cleanup interleaving.
func TestForcedDrainReaperStopsOnDisconnect(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "drain-disconnect", []types.Model{{Name: "llama3"}})
	rc.heartbeat(t, types.Heartbeat{WorkerID: "drain-disconnect"})
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Drain with a long deadline so the reaper is still waiting when the worker
	// disconnects.
	if err := h.srv.DrainWorkerWithDeadline("drain-disconnect", 30*time.Second); err != nil {
		t.Fatalf("DrainWorkerWithDeadline: %v", err)
	}
	waitFor(t, 2*time.Second, "worker draining", func() bool {
		w, ok := fleetByID(h.srv, "drain-disconnect")
		return ok && w.Status == types.WorkerDraining
	})

	// Worker drops its connection: the stream-close cleanup removes it, and the
	// reaper exits via w.ctx.Done rather than waiting out the 30s deadline.
	rc.close()
	waitFor(t, 2*time.Second, "worker removed on disconnect", func() bool {
		return h.srv.WorkerCount() == 0
	})
}

// TestDrainWorkerWithDeadlineUnknown proves DrainWorkerWithDeadline reports
// ErrWorkerNotFound for an unknown id (so the HTTP layer can 404), for both a
// soft (0) and a timed deadline.
func TestDrainWorkerWithDeadlineUnknown(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	if err := h.srv.DrainWorkerWithDeadline("missing", 0); !errors.Is(err, server.ErrWorkerNotFound) {
		t.Fatalf("soft drain unknown = %v, want ErrWorkerNotFound", err)
	}
	if err := h.srv.DrainWorkerWithDeadline("missing", 10*time.Second); !errors.Is(err, server.ErrWorkerNotFound) {
		t.Fatalf("timed drain unknown = %v, want ErrWorkerNotFound", err)
	}
}
