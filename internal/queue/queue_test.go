package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// job is a tiny helper building a Job with a given id (model is required by
// types.Job.Validate but the queue does not validate, so any value is fine).
func job(id string) types.Job {
	return types.Job{ID: id, Model: "echo", Prompt: "hi"}
}

// TestPriorityThenFIFO enqueues interleaved priorities and asserts the exact
// dequeue order: all high before all normal before all low, and FIFO (enqueue
// order) within each level.
func TestPriorityThenFIFO(t *testing.T) {
	t.Parallel()
	q := New()

	// Enqueue interleaved so neither priority nor insertion order alone explains
	// the result — only (priority desc, seq asc) does.
	enqueue := []struct {
		id string
		p  Priority
	}{
		{"n1", PriorityNormal},
		{"l1", PriorityLow},
		{"h1", PriorityHigh},
		{"n2", PriorityNormal},
		{"h2", PriorityHigh},
		{"l2", PriorityLow},
		{"h3", PriorityHigh},
		{"n3", PriorityNormal},
	}
	for _, e := range enqueue {
		if err := q.Enqueue(job(e.id), "k", e.p); err != nil {
			t.Fatalf("enqueue %s: %v", e.id, err)
		}
	}

	// High (FIFO), then normal (FIFO), then low (FIFO).
	want := []string{"h1", "h2", "h3", "n1", "n2", "n3", "l1", "l2"}
	for i, wantID := range want {
		it, ok := q.Dequeue()
		if !ok {
			t.Fatalf("dequeue %d: queue empty, want %s", i, wantID)
		}
		if it.Job.ID != wantID {
			t.Fatalf("dequeue %d: got %s, want %s", i, it.Job.ID, wantID)
		}
	}
	if _, ok := q.Dequeue(); ok {
		t.Fatalf("dequeue after draining: want empty")
	}
}

// TestEnqueueMetadata checks the per-item metadata captured at enqueue time:
// key, priority, a monotonic sequence, and a non-zero timestamp.
func TestEnqueueMetadata(t *testing.T) {
	t.Parallel()
	q := New()
	before := time.Now()
	if err := q.Enqueue(job("a"), "key-1", PriorityHigh); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if err := q.Enqueue(job("b"), "key-2", PriorityHigh); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	first, _ := q.Dequeue()
	second, _ := q.Dequeue()

	if first.Job.ID != "a" || first.Key != "key-1" || first.Priority != PriorityHigh {
		t.Fatalf("first item metadata wrong: %+v", first)
	}
	if first.EnqueuedAt.Before(before) {
		t.Fatalf("EnqueuedAt %v predates enqueue start %v", first.EnqueuedAt, before)
	}
	if !(first.Seq < second.Seq) {
		t.Fatalf("seq not monotonic: first=%d second=%d", first.Seq, second.Seq)
	}
}

// TestDequeueEmpty: a non-blocking Dequeue on an empty queue reports false.
func TestDequeueEmpty(t *testing.T) {
	t.Parallel()
	q := New()
	if it, ok := q.Dequeue(); ok {
		t.Fatalf("empty Dequeue: ok=true item=%+v, want false", it)
	}
}

// TestBackpressure: a bounded queue rejects the (k+1)-th enqueue with
// ErrQueueFull, and accepts again after a dequeue frees a slot.
func TestBackpressure(t *testing.T) {
	t.Parallel()
	const k = 3
	q := New(WithMaxDepth(k))

	for i := 0; i < k; i++ {
		if err := q.Enqueue(job(fmt.Sprintf("j%d", i)), "k", PriorityNormal); err != nil {
			t.Fatalf("enqueue %d within depth: %v", i, err)
		}
	}
	if err := q.Enqueue(job("overflow"), "k", PriorityNormal); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("enqueue past depth: got %v, want ErrQueueFull", err)
	}
	if q.Len() != k {
		t.Fatalf("len after rejected enqueue = %d, want %d", q.Len(), k)
	}

	// Free a slot, then a fresh enqueue must succeed.
	if _, ok := q.Dequeue(); !ok {
		t.Fatalf("dequeue to free a slot: empty")
	}
	if err := q.Enqueue(job("after-free"), "k", PriorityNormal); err != nil {
		t.Fatalf("enqueue after freeing a slot: %v", err)
	}
}

// TestUnboundedNoFull: the default queue never returns ErrQueueFull.
func TestUnboundedNoFull(t *testing.T) {
	t.Parallel()
	q := New()
	for i := 0; i < 1000; i++ {
		if err := q.Enqueue(job(fmt.Sprintf("j%d", i)), "k", PriorityNormal); err != nil {
			t.Fatalf("unbounded enqueue %d: %v", i, err)
		}
	}
	if q.Len() != 1000 {
		t.Fatalf("len = %d, want 1000", q.Len())
	}
}

// TestStats reports the total and a per-priority breakdown.
func TestStats(t *testing.T) {
	t.Parallel()
	q := New()
	mustEnqueue(t, q, "h", PriorityHigh)
	mustEnqueue(t, q, "n1", PriorityNormal)
	mustEnqueue(t, q, "n2", PriorityNormal)

	s := q.Stats()
	if s.Total != 3 {
		t.Fatalf("Stats.Total = %d, want 3", s.Total)
	}
	if s.ByPriority[PriorityHigh] != 1 || s.ByPriority[PriorityNormal] != 2 {
		t.Fatalf("Stats.ByPriority = %v, want high=1 normal=2", s.ByPriority)
	}
	if _, present := s.ByPriority[PriorityLow]; present {
		t.Fatalf("Stats.ByPriority should omit priorities with no items, got %v", s.ByPriority)
	}
}

// TestDequeueWaitWakesOnEnqueue: a blocked DequeueWait returns the item once an
// Enqueue makes one available — driven by sync, not sleeps.
func TestDequeueWaitWakesOnEnqueue(t *testing.T) {
	t.Parallel()
	q := New()

	type result struct {
		it  Item
		err error
	}
	done := make(chan result, 1)
	go func() {
		it, err := q.DequeueWait(context.Background())
		done <- result{it, err}
	}()

	// The waiter is blocked (queue empty); enqueue should wake it. We cannot
	// observe "is blocked" directly without a sleep, so we just enqueue and
	// require the waiter to receive the item within a bounded time.
	if err := q.Enqueue(job("wakeme"), "k", PriorityNormal); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("DequeueWait returned error: %v", r.err)
		}
		if r.it.Job.ID != "wakeme" {
			t.Fatalf("DequeueWait returned %s, want wakeme", r.it.Job.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DequeueWait did not wake on enqueue within 2s")
	}
}

// TestDequeueWaitReturnsImmediatelyWhenNonEmpty: no blocking when an item is
// already present.
func TestDequeueWaitReturnsImmediatelyWhenNonEmpty(t *testing.T) {
	t.Parallel()
	q := New()
	mustEnqueue(t, q, "ready", PriorityNormal)

	it, err := q.DequeueWait(context.Background())
	if err != nil {
		t.Fatalf("DequeueWait: %v", err)
	}
	if it.Job.ID != "ready" {
		t.Fatalf("got %s, want ready", it.Job.ID)
	}
}

// TestDequeueWaitContextCancel: a blocked DequeueWait returns ctx.Err() when the
// context is cancelled.
func TestDequeueWaitContextCancel(t *testing.T) {
	t.Parallel()
	q := New()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := q.DequeueWait(ctx)
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DequeueWait after cancel: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DequeueWait did not return on context cancel within 2s")
	}
}

// TestDequeueWaitContextDeadline: a deadline that elapses while blocked yields
// context.DeadlineExceeded.
func TestDequeueWaitContextDeadline(t *testing.T) {
	t.Parallel()
	q := New()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := q.DequeueWait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DequeueWait with elapsed deadline: got %v, want DeadlineExceeded", err)
	}
}

// TestDequeueWaitUnblocksOnClose: Close wakes every blocked waiter with
// ErrClosed and does not leak goroutines.
func TestDequeueWaitUnblocksOnClose(t *testing.T) {
	t.Parallel()
	q := New()

	const waiters = 5
	errs := make(chan error, waiters)
	var ready sync.WaitGroup
	ready.Add(waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			ready.Done()
			_, err := q.DequeueWait(context.Background())
			errs <- err
		}()
	}
	ready.Wait() // all goroutines started

	q.Close()

	for i := 0; i < waiters; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("waiter %d: got %v, want ErrClosed", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d did not unblock on Close within 2s", i)
		}
	}
}

// TestCloseIdempotentAndRejectsEnqueue: Close can be called repeatedly, and a
// closed queue rejects Enqueue with ErrClosed while still draining queued items.
func TestCloseIdempotentAndRejectsEnqueue(t *testing.T) {
	t.Parallel()
	q := New()
	mustEnqueue(t, q, "pre", PriorityNormal)

	q.Close()
	q.Close() // idempotent: must not panic

	if err := q.Enqueue(job("post"), "k", PriorityNormal); !errors.Is(err, ErrClosed) {
		t.Fatalf("enqueue after close: got %v, want ErrClosed", err)
	}

	// Items enqueued before Close are still dequeuable.
	if it, ok := q.Dequeue(); !ok || it.Job.ID != "pre" {
		t.Fatalf("dequeue after close: ok=%v id=%q, want ok=true id=pre", ok, it.Job.ID)
	}
	// Once drained, DequeueWait on a closed queue returns ErrClosed.
	if _, err := q.DequeueWait(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("DequeueWait on drained closed queue: got %v, want ErrClosed", err)
	}
}

// TestConcurrentEnqueueDequeue is the headline atomicity test: many concurrent
// enqueuers and dequeuers over a fixed set of jobs. Every enqueued ID must be
// dequeued EXACTLY once — no loss, no double. Uses the gate+atomic pattern so
// the race detector (run by CI on amd64) exercises the lock.
func TestConcurrentEnqueueDequeue(t *testing.T) {
	t.Parallel()

	const (
		enqueuers = 10
		dequeuers = 5
		perEnq    = 20
		total     = enqueuers * perEnq // 200 jobs
	)
	q := New()

	start := make(chan struct{})

	// Producers: each enqueues perEnq jobs with deterministic, unique IDs.
	var prodWG sync.WaitGroup
	for p := 0; p < enqueuers; p++ {
		prodWG.Add(1)
		go func(p int) {
			defer prodWG.Done()
			<-start
			for j := 0; j < perEnq; j++ {
				id := fmt.Sprintf("p%d-j%d", p, j)
				// Spread priorities so the heap path is exercised concurrently.
				prio := Priority(j % 3)
				if err := q.Enqueue(job(id), "k", prio); err != nil {
					t.Errorf("enqueue %s: %v", id, err)
				}
			}
		}(p)
	}

	// Consumers: dequeue until the total count is reached. Record each ID seen.
	var (
		seenMu sync.Mutex
		seen   = make(map[string]int, total)
	)
	var dequeued int64
	var consWG sync.WaitGroup
	for c := 0; c < dequeuers; c++ {
		consWG.Add(1)
		go func() {
			defer consWG.Done()
			<-start
			for atomic.LoadInt64(&dequeued) < total {
				it, ok := q.Dequeue()
				if !ok {
					continue // producers may not have caught up yet
				}
				atomic.AddInt64(&dequeued, 1)
				seenMu.Lock()
				seen[it.Job.ID]++
				seenMu.Unlock()
			}
		}()
	}

	close(start)
	prodWG.Wait()
	consWG.Wait()

	if got := atomic.LoadInt64(&dequeued); got != total {
		t.Fatalf("dequeued %d jobs, want %d", got, total)
	}
	if len(seen) != total {
		t.Fatalf("saw %d distinct IDs, want %d", len(seen), total)
	}
	for p := 0; p < enqueuers; p++ {
		for j := 0; j < perEnq; j++ {
			id := fmt.Sprintf("p%d-j%d", p, j)
			if seen[id] != 1 {
				t.Fatalf("job %s dequeued %d times, want exactly 1", id, seen[id])
			}
		}
	}
	if q.Len() != 0 {
		t.Fatalf("queue not empty after draining: len=%d", q.Len())
	}
}

// TestConcurrentEnqueueBackpressureExactCount: under concurrency on a bounded
// queue, the number of successful enqueues never exceeds the depth while the
// queue is never drained, and the rest fail with ErrQueueFull.
func TestConcurrentEnqueueBackpressureExactCount(t *testing.T) {
	t.Parallel()
	const depth = 50
	const goroutines = 500
	q := New(WithMaxDepth(depth))

	var ok, full int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := q.Enqueue(job(fmt.Sprintf("j%d", i)), "k", PriorityNormal)
			switch {
			case err == nil:
				atomic.AddInt64(&ok, 1)
			case errors.Is(err, ErrQueueFull):
				atomic.AddInt64(&full, 1)
			default:
				t.Errorf("unexpected enqueue error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&ok); got != depth {
		t.Fatalf("successful enqueues = %d, want exactly %d", got, depth)
	}
	if got := atomic.LoadInt64(&full); got != goroutines-depth {
		t.Fatalf("ErrQueueFull rejections = %d, want %d", got, goroutines-depth)
	}
	if q.Len() != depth {
		t.Fatalf("len = %d, want %d", q.Len(), depth)
	}
}

func mustEnqueue(t *testing.T, q *Queue, id string, p Priority) {
	t.Helper()
	if err := q.Enqueue(job(id), "k", p); err != nil {
		t.Fatalf("enqueue %s: %v", id, err)
	}
}
