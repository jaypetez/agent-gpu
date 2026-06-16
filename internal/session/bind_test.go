package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBindSetsBoundWorker asserts Bind records the worker on the owner's session,
// touches LastActiveAt, and persists so a later Get observes the binding.
func TestBindSetsBoundWorker(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	ctx := context.Background()

	s, err := m.Create(ctx, "k1", "llama")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.BoundWorkerID != "" {
		t.Fatalf("new session BoundWorkerID = %q, want empty", s.BoundWorkerID)
	}

	// Advance the clock so we can assert Bind touches LastActiveAt.
	clk.advance(5 * time.Minute)
	bound, err := m.Bind(ctx, s.ID, "k1", "worker-1")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if bound.BoundWorkerID != "worker-1" {
		t.Fatalf("Bind BoundWorkerID = %q, want worker-1", bound.BoundWorkerID)
	}
	if !bound.LastActiveAt.Equal(clk.now()) {
		t.Fatalf("Bind LastActiveAt = %v, want %v (touched)", bound.LastActiveAt, clk.now())
	}

	// The binding is persisted: a fresh Get sees it.
	got, err := m.Get(ctx, s.ID, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BoundWorkerID != "worker-1" {
		t.Fatalf("Get BoundWorkerID = %q, want worker-1", got.BoundWorkerID)
	}
}

// TestBindRebindUpdatesWorker asserts Bind overwrites a prior binding (rebind).
func TestBindRebindUpdatesWorker(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	ctx := context.Background()

	s, _ := m.Create(ctx, "k1", "llama")
	if _, err := m.Bind(ctx, s.ID, "k1", "worker-1"); err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	bound, err := m.Bind(ctx, s.ID, "k1", "worker-2")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if bound.BoundWorkerID != "worker-2" {
		t.Fatalf("rebound BoundWorkerID = %q, want worker-2", bound.BoundWorkerID)
	}
}

// TestBindOwnerChecked asserts Bind is owner-scoped: another key cannot bind (or
// probe the existence of) a session it does not own.
func TestBindOwnerChecked(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	ctx := context.Background()

	s, _ := m.Create(ctx, "alice", "llama")
	if _, err := m.Bind(ctx, s.ID, "bob", "worker-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Bind as bob = %v, want ErrSessionNotFound", err)
	}
	// Alice's session was not mutated by the rejected bind.
	got, err := m.Get(ctx, s.ID, "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BoundWorkerID != "" {
		t.Fatalf("BoundWorkerID = %q after rejected bind, want empty", got.BoundWorkerID)
	}
}

// TestBindMissingReturnsNotFound asserts binding a non-existent session returns
// ErrSessionNotFound.
func TestBindMissingReturnsNotFound(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	if _, err := m.Bind(context.Background(), "sess_nope", "k1", "worker-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Bind missing = %v, want ErrSessionNotFound", err)
	}
}

// TestBindExpiredReturnsNotFound asserts an idled-out session is not bindable.
func TestBindExpiredReturnsNotFound(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk, WithTTL(time.Minute))
	ctx := context.Background()

	s, _ := m.Create(ctx, "k1", "llama")
	clk.advance(2 * time.Minute) // idle past TTL
	if _, err := m.Bind(ctx, s.ID, "k1", "worker-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Bind expired = %v, want ErrSessionNotFound", err)
	}
}
