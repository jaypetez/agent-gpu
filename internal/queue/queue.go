// Package queue implements the global job queue for agent-gpu: a standalone,
// concurrency-safe FIFO-within-priority queue that the capacity-aware scheduler
// (#9) draws from when it places jobs on workers.
//
// # Ordering
//
// Jobs are served highest-priority-first; within a single priority level they
// are served strictly first-in-first-out. FIFO-within-level is provided by a
// monotonic per-queue sequence number assigned at Enqueue time, so two items of
// equal priority always come out in the order they went in regardless of
// goroutine scheduling.
//
// # Backpressure
//
// A queue may be bounded with WithMaxDepth(n). When full, Enqueue does NOT block
// the caller; it returns ErrQueueFull immediately so the request path can map
// the rejection to an explicit 503/429 rather than stalling. An unbounded queue
// (n <= 0) never returns ErrQueueFull.
//
// # Blocking dequeue
//
// Dequeue is non-blocking and reports whether an item was available. DequeueWait
// blocks (via sync.Cond) until an item is available, the supplied context is
// done, or the queue is closed — the seam the scheduler loop will park on.
//
// # Scope
//
// This package owns the queue data structure only. Choosing which worker runs a
// dequeued job, wiring the queue into the server's dispatch path, persistence,
// and metrics export (#24) are all out of scope; the scheduler (#9) consumes
// this queue. The queue is in-memory only and starts empty on every restart.
package queue

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// Priority is a job's scheduling priority. Higher values are served first; ties
// are broken first-in-first-out within the level.
type Priority int

const (
	// PriorityLow is below the default; served only when nothing higher is
	// queued.
	PriorityLow Priority = 0
	// PriorityNormal is the default priority for ordinary jobs.
	PriorityNormal Priority = 1
	// PriorityHigh is served ahead of normal and low priority jobs.
	PriorityHigh Priority = 2
)

// ErrQueueFull is returned by Enqueue when a bounded queue is at its max depth.
// It is the typed seam the request path maps to backpressure (503/429). Match it
// with errors.Is.
var ErrQueueFull = errors.New("queue: full")

// ErrClosed is returned by DequeueWait when the queue is closed while a caller
// is blocked (or already closed on entry). Match it with errors.Is.
var ErrClosed = errors.New("queue: closed")

// Item is one enqueued job together with the metadata captured at Enqueue time.
// The model lives on Job; the owning API key and the priority are carried
// alongside so the scheduler can account and order without mutating Job.
type Item struct {
	Job        types.Job
	Key        string
	Priority   Priority
	Seq        uint64
	EnqueuedAt time.Time
}

// Stats is an observable snapshot of queue depth: the total plus a per-priority
// breakdown.
type Stats struct {
	Total      int
	ByPriority map[Priority]int
}

// Option configures a Queue at construction.
type Option func(*Queue)

// WithMaxDepth bounds the queue to at most n pending items; an Enqueue past that
// returns ErrQueueFull. A value of n <= 0 means unbounded (the default).
func WithMaxDepth(n int) Option {
	return func(q *Queue) {
		if n < 0 {
			n = 0
		}
		q.maxDepth = n
	}
}

// Queue is a concurrency-safe, priority-ordered, FIFO-within-priority job queue.
// All state is guarded by a single mutex; a paired condition variable wakes
// callers blocked in DequeueWait. The zero value is not usable; construct with
// New.
type Queue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    itemHeap
	seq      uint64 // monotonic sequence assigned at enqueue, gives FIFO-within-level
	maxDepth int    // 0 == unbounded
	closed   bool
}

// New constructs an empty Queue with the supplied options.
func New(opts ...Option) *Queue {
	q := &Queue{}
	q.cond = sync.NewCond(&q.mu)
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// Enqueue appends job (owned by key, at priority p) to the queue, stamping it
// with the next sequence number and the current time. It returns ErrQueueFull
// immediately — without blocking — if the queue is bounded and already at max
// depth, and ErrClosed if the queue has been closed.
func (q *Queue) Enqueue(job types.Job, key string, p Priority) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}
	if q.maxDepth > 0 && q.items.Len() >= q.maxDepth {
		return ErrQueueFull
	}

	it := Item{
		Job:        job,
		Key:        key,
		Priority:   p,
		Seq:        q.seq,
		EnqueuedAt: time.Now(),
	}
	q.seq++
	heap.Push(&q.items, it)
	// Wake one blocked DequeueWait caller; one Enqueue makes exactly one item
	// available. Signal (not Broadcast) is sufficient and avoids a thundering
	// herd; Close uses Broadcast to wake everyone.
	q.cond.Signal()
	return nil
}

// Dequeue removes and returns the next item — highest priority, then oldest
// within that priority — without blocking. The bool is false (and the Item zero)
// when the queue is empty.
func (q *Queue) Dequeue() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.popLocked()
}

// DequeueWait blocks until an item is available, then returns it. It returns
// ctx.Err() if ctx is done first, and ErrClosed if the queue is closed while
// waiting (or is already closed and empty on entry).
func (q *Queue) DequeueWait(ctx context.Context) (Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// A goroutine bridges context cancellation to the condition variable:
	// without it a parked cond.Wait would never observe ctx being done. The
	// goroutine exits when this call returns (stop is closed via defer).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-stop:
		}
	}()

	for {
		if it, ok := q.popLocked(); ok {
			return it, nil
		}
		if q.closed {
			return Item{}, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return Item{}, err
		}
		q.cond.Wait()
	}
}

// popLocked removes the next item. The caller must hold q.mu.
func (q *Queue) popLocked() (Item, bool) {
	if q.items.Len() == 0 {
		return Item{}, false
	}
	return heap.Pop(&q.items).(Item), true
}

// Len reports the number of pending items.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items.Len()
}

// Stats returns an observable snapshot of queue depth: the total and a
// per-priority breakdown. The breakdown map only contains priorities that have
// at least one pending item.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()

	byPriority := make(map[Priority]int, len(q.items))
	for _, it := range q.items {
		byPriority[it.Priority]++
	}
	return Stats{Total: q.items.Len(), ByPriority: byPriority}
}

// Close marks the queue closed and wakes every blocked DequeueWait caller (each
// returns ErrClosed once the queue drains). Subsequent Enqueue calls return
// ErrClosed. Already-queued items remain dequeuable until drained. Close is
// idempotent and never leaks the blocked goroutines.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// itemHeap orders items by (priority descending, seq ascending): the highest
// priority is served first, and within a priority the lowest sequence number
// (oldest enqueue) is served first, giving strict FIFO-within-level. It
// implements heap.Interface; do not call its methods directly — go through the
// container/heap functions under q.mu.
type itemHeap []Item

func (h itemHeap) Len() int { return len(h) }

func (h itemHeap) Less(i, j int) bool {
	if h[i].Priority != h[j].Priority {
		return h[i].Priority > h[j].Priority // higher priority first
	}
	return h[i].Seq < h[j].Seq // older (smaller seq) first within a priority
}

func (h itemHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *itemHeap) Push(x any) { *h = append(*h, x.(Item)) }

func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = Item{} // avoid retaining the popped item's memory
	*h = old[:n-1]
	return it
}
