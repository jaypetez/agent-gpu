package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// endRecorder collects SessionEndStats reported by a Manager's session observer.
// It is concurrency-safe because the idle-expiry sweeper fires the observer from
// its own goroutine while the test goroutine reads.
type endRecorder struct {
	mu   sync.Mutex
	ends []SessionEndStats
}

func (r *endRecorder) observe(end SessionEndStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, end)
}

func (r *endRecorder) snapshot() []SessionEndStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SessionEndStats(nil), r.ends...)
}

func (r *endRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ends)
}

// waitFor polls cond until it is true or the timeout elapses, mirroring the
// inline poll loops the other session tests use to wait on the sweeper without a
// fixed sleep. It fails the test with desc on timeout.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, desc)
}

// TestActiveSessionsReflectsCreatesAndDeletes proves ActiveSessions tracks the
// live session count as sessions are created and deleted — the live read backing
// the agentgpu_active_sessions gauge (#38).
func TestActiveSessionsReflectsCreatesAndDeletes(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	ctx := context.Background()

	if got := m.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions at start = %d, want 0", got)
	}

	s1, _ := m.Create(ctx, "alice", "llama")
	s2, _ := m.Create(ctx, "alice", "llama")
	s3, _ := m.Create(ctx, "bob", "llama")
	if got := m.ActiveSessions(); got != 3 {
		t.Fatalf("ActiveSessions after 3 creates = %d, want 3 (across owners)", got)
	}

	if err := m.Delete(ctx, s1.ID, "alice"); err != nil {
		t.Fatalf("Delete s1: %v", err)
	}
	if got := m.ActiveSessions(); got != 2 {
		t.Fatalf("ActiveSessions after 1 delete = %d, want 2", got)
	}

	_ = m.Delete(ctx, s2.ID, "alice")
	_ = m.Delete(ctx, s3.ID, "bob")
	if got := m.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions after all deletes = %d, want 0", got)
	}
}

// TestSessionObserverFiresOnDeleteWithStats proves the end observer fires once on
// an explicit Delete, carrying the session's final turn count, its lifetime
// duration, and the "deleted" reason (#38).
func TestSessionObserverFiresOnDeleteWithStats(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &endRecorder{}
	m := newTestManager(t, clk, WithSessionObserver(rec.observe))
	ctx := context.Background()

	s, err := m.Create(ctx, "alice", "llama")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Three stored turns.
	for _, c := range []string{"a", "b", "c"} {
		if err := m.AppendTurn(ctx, s.ID, "alice", types.Message{Role: "user", Content: c}); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}
	// Advance the clock so the lifetime duration is observable and exact.
	clk.advance(90 * time.Second)

	if err := m.Delete(ctx, s.ID, "alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ends := rec.snapshot()
	if len(ends) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1: %+v", len(ends), ends)
	}
	got := ends[0]
	if got.Turns != 3 {
		t.Errorf("end turns = %d, want 3", got.Turns)
	}
	if got.Duration != 90*time.Second {
		t.Errorf("end duration = %v, want 90s", got.Duration)
	}
	if got.Reason != EndReasonDeleted {
		t.Errorf("end reason = %q, want %q", got.Reason, EndReasonDeleted)
	}
}

// TestSessionObserverFiresOnExpiry proves the sweeper fires the end observer when
// a session idles out, with the "expired" reason — so an abandoned conversation's
// lifetime is still recorded (#38).
func TestSessionObserverFiresOnExpiry(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &endRecorder{}
	m := newTestManager(t, clk, WithTTL(time.Minute), WithSessionObserver(rec.observe))
	m.Start()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	s, err := m.Create(ctx, "alice", "llama")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.AppendTurn(ctx, s.ID, "alice", types.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	// Idle past the TTL; the sweeper (1ms interval) reaps it against the fake clock.
	clk.advance(2 * time.Minute)
	waitFor(t, 2*time.Second, "session expired and observed", func() bool {
		return rec.len() == 1
	})

	ends := rec.snapshot()
	if len(ends) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1: %+v", len(ends), ends)
	}
	if ends[0].Reason != EndReasonExpired {
		t.Errorf("end reason = %q, want %q", ends[0].Reason, EndReasonExpired)
	}
	if ends[0].Turns != 1 {
		t.Errorf("end turns = %d, want 1", ends[0].Turns)
	}
	// LastActiveAt was the append at t0; the session expired sometime after t0+1m.
	// Duration is end-CreatedAt, so it is at least the TTL.
	if ends[0].Duration < time.Minute {
		t.Errorf("end duration = %v, want >= 1m", ends[0].Duration)
	}
}

// TestSessionObserverNotFiredForMissingOrNotOwned proves a delete that does not
// actually remove a session (missing id, or another owner's id) emits no end
// observation — the observation tracks real session ends only.
func TestSessionObserverNotFiredForMissingOrNotOwned(t *testing.T) {
	clk := newClock(time.Now())
	rec := &endRecorder{}
	m := newTestManager(t, clk, WithSessionObserver(rec.observe))
	ctx := context.Background()

	s, _ := m.Create(ctx, "alice", "llama")

	// Missing id: no-op delete, no observation.
	_ = m.Delete(ctx, "sess_nope", "alice")
	// Another owner: ErrSessionNotFound, no observation, session survives.
	_ = m.Delete(ctx, s.ID, "bob")

	if n := rec.len(); n != 0 {
		t.Fatalf("observer fired %d times for non-deletes, want 0", n)
	}
	if got := m.ActiveSessions(); got != 1 {
		t.Fatalf("ActiveSessions = %d after rejected deletes, want 1", got)
	}
}

// TestSessionObserverFiresOncePerSessionUnderConcurrentDelete proves that when an
// explicit Delete races the idle-expiry sweeper for the SAME session, the end
// observation is emitted exactly once (the gate that prevents double-counting the
// per-owner tally also prevents double-counting the lifetime metric, #38).
func TestSessionObserverFiresOncePerSessionUnderConcurrentDelete(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &endRecorder{}
	// Long sweep interval and a not-yet-expired session so the only ends are the
	// concurrent explicit Deletes we drive below (no sweeper interference).
	m := NewManager(NewMemorySessionStore(), NewMemoryHistoryStore(100, 1<<20),
		WithClock(clk.now), WithTTL(time.Hour), WithSweepInterval(time.Hour),
		WithSessionObserver(rec.observe))
	ctx := context.Background()

	s, _ := m.Create(ctx, "alice", "llama")

	// Fire several concurrent Deletes of the same id. Exactly one removes the row;
	// the others see it gone. The observation must fire once.
	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_ = m.Delete(ctx, s.ID, "alice")
		}()
	}
	wg.Wait()

	if n := rec.len(); n != 1 {
		t.Fatalf("observer fired %d times under concurrent delete, want exactly 1", n)
	}
	if got := m.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions = %d after delete, want 0", got)
	}
}

// TestNoObserverIsNoOp proves a Manager built WITHOUT an observer ends sessions
// normally and never panics on the nil hook (the default, non-opted-in path).
func TestNoObserverIsNoOp(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk) // no WithSessionObserver
	ctx := context.Background()

	s, _ := m.Create(ctx, "alice", "llama")
	if err := m.Delete(ctx, s.ID, "alice"); err != nil {
		t.Fatalf("Delete without observer: %v", err)
	}
	if got := m.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions = %d, want 0", got)
	}
}
