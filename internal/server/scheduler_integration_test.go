package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// TestStreamFailsFastOnUnservedModel covers the streaming fail-fast contract
// (#13 client-readiness): SubmitAuthorizedJobStream for a model NO worker serves
// returns ErrModelUnavailable (mapped to a 503 "unavailable" by the HTTP layer),
// distinct from queue.ErrQueueFull, which is returned only when a worker serves
// the model but cannot take the stream right now (busy). A drained worker serving
// the model exercises the latter case.
func TestStreamFailsFastOnUnservedModel(t *testing.T) {
	az := authz.NewAuthorizer()
	h := newHarnessWith(t, server.WithAuthorizer(az), server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k", Roles: []string{authz.RoleAdmin}}
	ctx := context.Background()

	// No worker serves any model: the stream submit fails fast.
	if _, err := h.srv.SubmitAuthorizedJobStream(ctx, key,
		types.Job{ID: "s1", Model: "ghost", Prompt: "x"}); !errors.Is(err, server.ErrModelUnavailable) {
		t.Fatalf("unserved-model stream err = %v, want ErrModelUnavailable", err)
	}

	// A drained worker serves "llama3": the model IS served, but no worker can take
	// the stream, so backpressure is surfaced as ErrQueueFull (NOT
	// ErrModelUnavailable), matching the non-streaming submit's diagnosis split.
	busy := dialRaw(t, h, "busy-worker", []types.Model{{Name: "llama3"}})
	defer busy.close()
	waitFor(t, 2*time.Second, "busy worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})
	if err := h.srv.DrainWorker("busy-worker"); err != nil {
		t.Fatalf("drain busy-worker: %v", err)
	}
	if _, err := h.srv.SubmitAuthorizedJobStream(ctx, key,
		types.Job{ID: "s2", Model: "llama3", Prompt: "x"}); !errors.Is(err, queue.ErrQueueFull) {
		t.Fatalf("served-but-busy stream err = %v, want ErrQueueFull", err)
	}
}

// reply sends a successful result for jobID on the raw client's stream.
func (r *rawClient) reply(t *testing.T, jobID, output string) {
	t.Helper()
	if err := r.stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Result{
			Result: types.JobResult{JobID: jobID, Output: output}.Proto(),
		},
	}); err != nil {
		t.Fatalf("send result: %v", err)
	}
}

// TestQueuesThenPlacesWhenWorkerAppears covers the core queue-on-miss behavior:
// a job submitted while no worker is connected is QUEUED (not dropped), and once
// a worker registers and heartbeats the placement loop dispatches it and the
// original caller receives the result. The clock is injected and fast-forwarded
// so no real time approaches the heartbeat timeout.
func TestQueuesThenPlacesWhenWorkerAppears(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	// placing fires the instant the placement loop pulls a job for placement, a
	// deterministic "the job queued and is being placed" edge (QueueStats().Total
	// cannot be polled for it, as the loop dequeues it the moment it can).
	placing := make(chan string, 4)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
		server.WithPlacementObserver(func(jobID string) { placing <- jobID }),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// A worker that serves the model is connected but DRAINED, so the model is
	// "served" (a job for it queues rather than fast-failing with
	// ErrModelUnavailable) yet no worker can take the job right now — the
	// legitimate-backpressure case the queue exists for. Drained workers are not
	// selectable, so the submit below queues instead of dispatching.
	busy := dialRaw(t, h, "busy-worker", []types.Model{{Name: "llama3"}})
	defer busy.close()
	waitFor(t, 2*time.Second, "busy worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})
	if err := h.srv.DrainWorker("busy-worker"); err != nil {
		t.Fatalf("drain busy-worker: %v", err)
	}

	// Submit blocks, the job lands in the queue (model served, no worker free); the
	// placement loop then pulls it and parks waiting for a runnable worker.
	type result struct {
		res types.JobResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "queued-1", Model: "llama3", Prompt: "hi"})
		done <- result{res, err}
	}()

	select {
	case got := <-placing:
		if got != "queued-1" {
			t.Fatalf("placement observer fired for %q, want queued-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job was not queued/pulled for placement")
	}

	// A fresh worker registers AFTER the job is queued, advertising the model so it
	// is runnable immediately (a registered worker reports zero free VRAM until its
	// first heartbeat, but an already-loaded model is runnable regardless).
	rc := dialRaw(t, h, "late-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()

	// The placement loop dispatches the queued job to the new worker; reply so the
	// blocked caller's waiter resolves.
	job := rc.awaitJob(t)
	if job.GetId() != "queued-1" {
		t.Fatalf("dispatched job id = %q, want queued-1", job.GetId())
	}
	rc.reply(t, job.GetId(), "placed")

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("queued submit err = %v", r.err)
		}
		if r.res.Output != "placed" {
			t.Fatalf("queued submit output = %q, want placed", r.res.Output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued job never placed after worker appeared")
	}

	waitFor(t, 2*time.Second, "queue drained", func() bool {
		return h.srv.QueueStats().Total == 0
	})
}

// TestPriorityUnderContention covers priority ordering: several jobs are queued
// while no worker can take them, then ONE worker is made available; the
// highest-priority job must be dispatched first. Priority is derived from each
// key's roles via SubmitAuthorizedJob. Determinism comes from enqueuing all jobs
// (confirmed via QueueStats) BEFORE any runnable worker exists, so the queue's
// priority ordering — not goroutine scheduling — decides who goes first.
//
// freezeQueue serves the model via a drained worker and parks the placement loop
// on a sentinel, so the three submits below form a static, priority-ordered queue
// (a job for an unserved model would now fast-fail). The sentinel itself is
// dispatched FIRST when the runnable worker appears (it was pulled first), so the
// test consumes that dispatch before asserting the high/normal/low order.
func TestPriorityUnderContention(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	placing := make(chan string, 8)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
		server.WithPlacementObserver(func(jobID string) { placing <- jobID }),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	busy := freezeQueue(t, h, placing, "llama3")
	defer busy.close()

	// Three keys at distinct priority tiers (admin=high, user=normal,
	// read-only=low). Allow-list the model so user/read-only are authorized.
	lowKey := store.APIKey{ID: "k-low", Roles: []string{"read-only"}, AllowModels: []string{"llama3"}}
	normKey := store.APIKey{ID: "k-norm", Roles: []string{"user"}, AllowModels: []string{"llama3"}}
	highKey := store.APIKey{ID: "k-high", Roles: []string{"admin"}}

	type result struct {
		res types.JobResult
		err error
	}
	submit := func(key store.APIKey, jobID string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			res, err := h.srv.SubmitAuthorizedJob(context.Background(), key,
				types.Job{ID: jobID, Model: "llama3", Prompt: "x"})
			ch <- result{res, err}
		}()
		return ch
	}

	// Enqueue low first, then normal, then high — deliberately the reverse of the
	// order we expect them served, so a FIFO bug would surface. The loop is parked
	// on the sentinel, so these accumulate statically (Total tracks each submit).
	lowCh := submit(lowKey, "job-low")
	waitFor(t, 2*time.Second, "low queued", func() bool { return h.srv.QueueStats().Total == 1 })
	normCh := submit(normKey, "job-norm")
	waitFor(t, 2*time.Second, "norm queued", func() bool { return h.srv.QueueStats().Total == 2 })
	highCh := submit(highKey, "job-high")
	waitFor(t, 2*time.Second, "high queued", func() bool { return h.srv.QueueStats().Total == 3 })

	// Confirm the per-priority breakdown is what we intend.
	stats := h.srv.QueueStats()
	if stats.ByPriority[queue.PriorityHigh] != 1 ||
		stats.ByPriority[queue.PriorityNormal] != 1 ||
		stats.ByPriority[queue.PriorityLow] != 1 {
		t.Fatalf("queue breakdown = %#v, want one each of high/normal/low", stats.ByPriority)
	}

	// Make exactly ONE worker available. Serve jobs one at a time (reply only
	// after receiving). The parked sentinel is dispatched first; consume it, then
	// assert the remaining dispatch order is high, normal, low.
	rc := dialRaw(t, h, "single-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()

	if job := rc.awaitJob(t); job.GetId() != "sentinel" {
		t.Fatalf("first dispatch = %q, want the parked sentinel", job.GetId())
	} else {
		rc.reply(t, job.GetId(), "ok")
	}

	wantOrder := []string{"job-high", "job-norm", "job-low"}
	for i, want := range wantOrder {
		job := rc.awaitJob(t)
		if job.GetId() != want {
			t.Fatalf("dispatch %d = %q, want %q (priority order)", i, job.GetId(), want)
		}
		rc.reply(t, job.GetId(), "ok")
	}

	for _, ch := range []<-chan result{highCh, normCh, lowCh} {
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("submit err = %v", r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a queued submit never completed")
		}
	}
}

// TestQueueFullRejected covers backpressure: a bounded queue at depth rejects a
// further submit with queue.ErrQueueFull rather than blocking the caller.
//
// freezeQueue serves the model (via a drained worker) and parks the placement
// loop on a sentinel, so jobs queue rather than fast-failing AND the loop does
// not drain the bounded queue out from under the test. The sentinel is dequeued
// (it leaves the depth-1 queue, which then holds "first"), so the second submit
// finds the queue full.
func TestQueueFullRejected(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	placing := make(chan string, 8)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
		server.WithQueue(queue.New(queue.WithMaxDepth(1))),
		server.WithPlacementObserver(func(jobID string) { placing <- jobID }),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	busy := freezeQueue(t, h, placing, "m")
	defer busy.close()

	// The first job queues (depth 1) and blocks its caller.
	first := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := h.srv.SubmitJob(ctx, types.Job{ID: "first", Model: "m"})
		first <- err
	}()
	waitFor(t, 2*time.Second, "first queued", func() bool {
		return h.srv.QueueStats().Total == 1
	})

	// The second submit cannot queue (max depth 1) and must reject immediately.
	if _, err := h.srv.SubmitJob(context.Background(),
		types.Job{ID: "second", Model: "m"}); err != queue.ErrQueueFull {
		t.Fatalf("second submit err = %v, want ErrQueueFull", err)
	}

	// Cancelling the first caller unblocks it cleanly (no leak).
	cancel()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled first submit did not unblock")
	}
}

// TestConcurrentQueuedJobsEachDispatchedOnce stresses the placement path: many
// jobs are submitted concurrently while a single worker (echo-replying via a
// background reader) is connected. Every job must complete exactly once with no
// loss and no double-dispatch. Designed to be meaningful under -race on CI
// (amd64); the arm64 dev host cannot run ThreadSanitizer.
func TestConcurrentQueuedJobsEachDispatchedOnce(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// One worker that echoes every dispatched job back as a result, so jobs that
	// queue (the worker reports zero free VRAM until a heartbeat, but the model is
	// advertised so it is runnable) are placed and resolved.
	rc := dialRaw(t, h, "echo-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()
	stopReplies := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopReplies:
				return
			case msg, ok := <-rc.recvd:
				if !ok {
					return
				}
				if job := msg.GetJob(); job != nil {
					rc.reply(t, job.GetId(), "ok:"+job.GetId())
				}
			}
		}
	}()
	defer close(stopReplies)

	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	const n = 40
	results := make(chan types.JobResult, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		id := "job-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := h.srv.SubmitJob(ctx, types.Job{ID: id, Model: "llama3", Prompt: "x"})
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}(id)
	}

	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		select {
		case res := <-results:
			seen[res.JobID]++
		case err := <-errs:
			t.Fatalf("submit failed: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d jobs completed", i, n)
		}
	}
	if len(seen) != n {
		t.Fatalf("distinct completed jobs = %d, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %s completed %d times, want exactly once", id, c)
		}
	}
}

// TestCloseReleasesQueuedCaller covers clean shutdown: a caller blocked on a
// queued job that never places is released (not hung) when the server closes.
func TestCloseReleasesQueuedCaller(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	// placing fires when the loop pulls the job for placement — a deterministic
	// "the job is queued and being placed (parked on the drained worker)" edge.
	placing := make(chan string, 4)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
		server.WithPlacementObserver(func(jobID string) { placing <- jobID }),
	)
	defer h.close()

	// A drained worker serves the model so the job queues (model served, no worker
	// free) rather than fast-failing; the loop then parks on it until Close.
	busy := dialRaw(t, h, "busy-worker", []types.Model{{Name: "m"}})
	defer busy.close()
	waitFor(t, 2*time.Second, "busy worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})
	if err := h.srv.DrainWorker("busy-worker"); err != nil {
		t.Fatalf("drain busy-worker: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.srv.SubmitJob(context.Background(), types.Job{ID: "stuck", Model: "m"})
		done <- err
	}()
	select {
	case got := <-placing:
		if got != "stuck" {
			t.Fatalf("placement observer fired for %q, want stuck", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job was not queued/pulled for placement")
	}

	// Close must release the blocked caller and stop the placement loop.
	if err := h.srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("queued caller returned nil err on shutdown, want shutdown error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued caller not released by Close")
	}
}
