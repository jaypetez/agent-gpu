package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestQueueDepthReflectsBacklog covers AC3/AC4 for queue depth: a growing backlog
// (jobs submitted while no worker is connected) is reflected live in QueueStats()
// — the total grows to N and the per-priority breakdown attributes every queued
// job to its lane. Keyless SubmitJob queues at PriorityNormal, so this asserts a
// known queue state of N normal-lane jobs. (The distinct-priority ordering of the
// breakdown is exercised directly in the queue package's priority tests.)
func TestQueueDepthReflectsBacklog(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	const n = 4
	for i := 0; i < n; i++ {
		id := "job-" + string(rune('A'+i))
		// Each blocks (queued, no worker) until Close releases it.
		go func(id string) {
			_, _ = h.srv.SubmitJob(context.Background(), types.Job{ID: id, Model: "m"})
		}(id)
	}

	waitFor(t, 2*time.Second, "backlog of N queued", func() bool {
		return h.srv.QueueStats().Total == n
	})
	st := h.srv.QueueStats()
	if st.Total != n {
		t.Fatalf("queue total = %d, want %d", st.Total, n)
	}
	if st.ByPriority[queue.PriorityNormal] != n {
		t.Fatalf("by-priority normal = %d, want %d (%v)", st.ByPriority[queue.PriorityNormal], n, st.ByPriority)
	}
}

// TestWaitTimeRecordedOnPlacement is the injected-clock wait-time test (AC2/AC4):
// a job that QUEUES (no runnable worker at submit) and is later PLACED records a
// time-in-queue equal to the clock advance between its enqueue and its placement.
// A second job with a larger wait updates the running sum/max and lands the
// cumulative buckets correctly. Because the queue stamps EnqueuedAt and placeItem
// reads the wait off the SAME injected clock, the recorded wait is deterministic.
func TestWaitTimeRecordedOnPlacement(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	// placing receives each job id the instant the placement loop has dequeued it
	// for placement: a deterministic edge to gate the clock advance on (the item
	// leaves the queue immediately, so QueueStats().Total cannot be polled for it).
	placing := make(chan string, 4)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Hour), // never evict on the advanced clock
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
		server.WithPlacementObserver(func(jobID string) { placing <- jobID }),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()
	// Start the placement loop now so it drains the queue (and fires the placement
	// observer) before any worker connects; otherwise the loop would not run until
	// the first Connect and awaitPlacing below would deadlock.
	h.srv.Start()

	awaitPlacing := func(want string) {
		t.Helper()
		select {
		case got := <-placing:
			if got != want {
				t.Fatalf("placement observer fired for %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("placement loop never dequeued %q", want)
		}
	}

	// Baseline: nothing recorded yet.
	if got := h.srv.WaitTimeStats().Count; got != 0 {
		t.Fatalf("initial wait count = %d, want 0", got)
	}

	// ---- Job 1: queued at t0, placed at t0+50ms → wait ≈ 50ms (bucket <=100). ----
	type result struct {
		res types.JobResult
		err error
	}
	done1 := make(chan result, 1)
	go func() {
		res, err := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "w1", Model: "llama3", Prompt: "hi"})
		done1 <- result{res, err}
	}()
	// The job is enqueued (stamped EnqueuedAt at t0) and the placement loop has it
	// in hand, parked for a worker. Now advance the clock so the dispatch measures
	// the full wait.
	awaitPlacing("w1")
	clk.advance(50 * time.Millisecond)

	rc := dialRaw(t, h, "worker-1", []types.Model{{Name: "llama3"}})
	defer rc.close()
	job := rc.awaitJob(t)
	rc.reply(t, job.GetId(), "ok")
	if r := <-done1; r.err != nil {
		t.Fatalf("job1 submit err = %v", r.err)
	}

	waitFor(t, 2*time.Second, "wait recorded for job1", func() bool {
		return h.srv.WaitTimeStats().Count == 1
	})
	ws := h.srv.WaitTimeStats()
	if ws.SumMs != 50 || ws.MaxMs != 50 {
		t.Fatalf("after job1: sum=%d max=%d, want sum=50 max=50", ws.SumMs, ws.MaxMs)
	}
	// 50ms falls in the <=100, <=1000, <=10000, and +Inf buckets but NOT <=10.
	assertBucket(t, ws, 10, 0)
	assertBucket(t, ws, 100, 1)
	assertBucket(t, ws, 1000, 1)
	assertBucket(t, ws, 10000, 1)
	assertInfBucket(t, ws, 1)

	// ---- Job 2: queued at t1, placed at t1+5000ms → wait ≈ 5000ms (bucket <=10000). ----
	// The worker is connected now, so to force a SECOND queueing we drain it first
	// (drained workers are not selected), submit, advance, then connect a fresh
	// worker.
	if err := h.srv.DrainWorker("worker-1"); err != nil {
		t.Fatalf("drain worker-1: %v", err)
	}
	done2 := make(chan result, 1)
	go func() {
		res, err := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "w2", Model: "llama3", Prompt: "hi"})
		done2 <- result{res, err}
	}()
	awaitPlacing("w2")
	clk.advance(5000 * time.Millisecond)

	rc2 := dialRaw(t, h, "worker-2", []types.Model{{Name: "llama3"}})
	defer rc2.close()
	job2 := rc2.awaitJob(t)
	rc2.reply(t, job2.GetId(), "ok")
	if r := <-done2; r.err != nil {
		t.Fatalf("job2 submit err = %v", r.err)
	}

	waitFor(t, 2*time.Second, "wait recorded for job2", func() bool {
		return h.srv.WaitTimeStats().Count == 2
	})
	ws = h.srv.WaitTimeStats()
	if ws.Count != 2 {
		t.Fatalf("count = %d, want 2", ws.Count)
	}
	if ws.SumMs != 5050 { // 50 + 5000
		t.Fatalf("sum = %d, want 5050", ws.SumMs)
	}
	if ws.MaxMs != 5000 {
		t.Fatalf("max = %d, want 5000", ws.MaxMs)
	}
	// Cumulative buckets: only the 50ms wait is <=100; both are <=10000.
	assertBucket(t, ws, 10, 0)
	assertBucket(t, ws, 100, 1)
	assertBucket(t, ws, 1000, 1)
	assertBucket(t, ws, 10000, 2)
	assertInfBucket(t, ws, 2)
}

// TestWaitTimeExcludesFastPath proves the instrumentation records ONLY queued
// jobs: a job dispatched on the synchronous fast path (a worker fits at submit)
// never queued, so it must not appear in the time-in-queue distribution.
func TestWaitTimeExcludesFastPath(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Hour),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// A worker is connected and runnable BEFORE the submit, so the job takes the
	// fast path (dispatched synchronously, never enqueued).
	rc := dialRaw(t, h, "worker-1", []types.Model{{Name: "llama3"}})
	defer rc.close()
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	done := make(chan error, 1)
	go func() {
		_, err := h.srv.SubmitJob(context.Background(),
			types.Job{ID: "fast", Model: "llama3", Prompt: "hi"})
		done <- err
	}()
	job := rc.awaitJob(t)
	rc.reply(t, job.GetId(), "ok")
	if err := <-done; err != nil {
		t.Fatalf("fast-path submit err = %v", err)
	}

	if got := h.srv.WaitTimeStats().Count; got != 0 {
		t.Fatalf("fast-path job recorded a wait (count=%d), want 0", got)
	}
}

// assertBucket checks the cumulative count of the bucket with upper bound leMs.
func assertBucket(t *testing.T, ws server.WaitTimeStats, leMs, want uint64) {
	t.Helper()
	for _, b := range ws.Buckets {
		if b.LeMs == leMs {
			if b.Count != want {
				t.Fatalf("bucket le=%d count=%d, want %d", leMs, b.Count, want)
			}
			return
		}
	}
	t.Fatalf("bucket le=%d not present in %+v", leMs, ws.Buckets)
}

// assertInfBucket checks the +Inf bucket (LeMs == 0), which must equal Count.
func assertInfBucket(t *testing.T, ws server.WaitTimeStats, want uint64) {
	t.Helper()
	last := ws.Buckets[len(ws.Buckets)-1]
	if last.LeMs != 0 {
		t.Fatalf("trailing bucket le=%d, want 0 (+Inf)", last.LeMs)
	}
	if last.Count != want {
		t.Fatalf("+Inf bucket count = %d, want %d", last.Count, want)
	}
}
