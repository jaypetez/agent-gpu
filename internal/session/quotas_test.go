package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestConcurrentSessionCapRejectsAndDecrements proves the per-owner concurrent
// cap (#37): the N+1 Create is rejected with ErrSessionLimitExceeded, a DIFFERENT
// owner is unaffected (the cap is per key), and deleting a session frees a slot
// so a subsequent Create succeeds.
func TestConcurrentSessionCapRejectsAndDecrements(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk, WithMaxSessionsPerKey(2))
	ctx := context.Background()

	s1, err := m.Create(ctx, "k1", "m")
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := m.Create(ctx, "k1", "m"); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if m.activeCount("k1") != 2 {
		t.Fatalf("active count = %d, want 2", m.activeCount("k1"))
	}

	// The 3rd create for k1 is over the cap.
	if _, err := m.Create(ctx, "k1", "m"); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("create 3 = %v, want ErrSessionLimitExceeded", err)
	}

	// A different owner key is unaffected by k1's usage.
	if _, err := m.Create(ctx, "k2", "m"); err != nil {
		t.Fatalf("create for k2 should be allowed (per-key cap): %v", err)
	}

	// Deleting one of k1's sessions frees a slot; the next create succeeds.
	if err := m.Delete(ctx, s1.ID, "k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if m.activeCount("k1") != 1 {
		t.Fatalf("active count after delete = %d, want 1", m.activeCount("k1"))
	}
	if _, err := m.Create(ctx, "k1", "m"); err != nil {
		t.Fatalf("create after freeing a slot: %v", err)
	}
}

// TestConcurrentSessionCapUnlimitedByDefault proves the default (no cap option,
// or 0) imposes no concurrent-session limit — the non-breaking default.
func TestConcurrentSessionCapUnlimitedByDefault(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk) // no WithMaxSessionsPerKey
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := m.Create(ctx, "k", "m"); err != nil {
			t.Fatalf("create %d under unlimited cap: %v", i, err)
		}
	}
}

// TestConcurrentSessionCapDecrementsOnExpiry proves the sweeper's idle-expiry
// reap frees the owner's concurrency slot, so an idled-out session does not
// permanently consume cap headroom.
func TestConcurrentSessionCapDecrementsOnExpiry(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk, WithMaxSessionsPerKey(1)) // TTL 1h, sweep 1ms
	ctx := context.Background()

	if _, err := m.Create(ctx, "k", "m"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// At the cap now: a second create is rejected.
	if _, err := m.Create(ctx, "k", "m"); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("create at cap = %v, want ErrSessionLimitExceeded", err)
	}

	m.Start()
	defer m.Close()
	// Idle the session out; the sweeper reaps it and frees the slot.
	clk.advance(2 * time.Hour)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.activeCount("k") == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if m.activeCount("k") != 0 {
		t.Fatalf("expiry did not free the slot: count = %d", m.activeCount("k"))
	}
	// With the slot freed, a fresh create succeeds.
	if _, err := m.Create(ctx, "k", "m"); err != nil {
		t.Fatalf("create after expiry freed a slot: %v", err)
	}
}

// TestConcurrentCapReconciledFromStore proves a Manager built over a store that
// already holds sessions (e.g. checkpoint-restored at boot) counts them against
// the cap rather than starting from zero.
func TestConcurrentCapReconciledFromStore(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ss := NewMemorySessionStore()
	// Two pre-existing live sessions for k, as if restored from a checkpoint.
	for _, id := range []string{"sess_a", "sess_b"} {
		ss.Put(Session{ID: id, OwnerKeyID: "k", Model: "m", TTL: time.Hour,
			CreatedAt: clk.now(), LastActiveAt: clk.now(), Status: StatusActive})
	}
	m := NewManager(ss, NewMemoryHistoryStore(100, 1<<20),
		WithClock(clk.now), WithTTL(time.Hour), WithSweepInterval(time.Hour),
		WithMaxSessionsPerKey(2))
	if m.activeCount("k") != 2 {
		t.Fatalf("reconciled count = %d, want 2", m.activeCount("k"))
	}
	// Already at the cap from restored sessions: a new create is rejected.
	if _, err := m.Create(context.Background(), "k", "m"); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("create over reconciled cap = %v, want ErrSessionLimitExceeded", err)
	}
}

// TestConcurrentCapRaceSafe stresses Create under concurrency with a per-owner
// cap so the race detector (CI amd64) flags any data race on the tally, and so
// the cap is never exceeded even under contended check-and-increment. Exactly
// `cap` creates must succeed; the rest must be rejected.
func TestConcurrentCapRaceSafe(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	const limit = 10
	m := NewManager(NewMemorySessionStore(), NewMemoryHistoryStore(100, 1<<20),
		WithClock(clk.now), WithTTL(time.Hour), WithSweepInterval(time.Hour),
		WithMaxSessionsPerKey(limit))
	ctx := context.Background()

	var ok, rejected int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Create(ctx, "k", "m"); err == nil {
				atomic.AddInt64(&ok, 1)
			} else if errors.Is(err, ErrSessionLimitExceeded) {
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}
	wg.Wait()
	if ok != limit {
		t.Fatalf("succeeded %d creates, want exactly limit %d", ok, limit)
	}
	if rejected != 100-limit {
		t.Fatalf("rejected %d, want %d", rejected, 100-limit)
	}
	if m.activeCount("k") != limit {
		t.Fatalf("final active count = %d, want %d", m.activeCount("k"), limit)
	}
}

// TestDeleteVsSweepDecrementsOnce proves the concurrency tally is decremented
// EXACTLY ONCE when an explicit Delete races the idle-expiry sweeper for the same
// session: the owner must not be under-counted (which would let it exceed its cap
// later). It idles a session out (so the sweeper wants to reap it) and concurrently
// issues an owner Delete, then asserts the tally lands at 0, never negative-then-
// wrapped or stuck.
func TestDeleteVsSweepDecrementsOnce(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk, WithMaxSessionsPerKey(5)) // TTL 1h, sweep 1ms
	ctx := context.Background()
	s, err := m.Create(ctx, "k", "m")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.activeCount("k") != 1 {
		t.Fatalf("count after create = %d, want 1", m.activeCount("k"))
	}

	m.Start()
	defer m.Close()

	// Idle the session out and concurrently Delete it: the sweeper and the explicit
	// Delete both target the same id.
	clk.advance(2 * time.Hour)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = m.Delete(ctx, s.ID, "k") // may win or lose the race with the sweeper
	}()
	wg.Wait()

	// Whoever removed it released the slot once; the tally settles at 0.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.activeCount("k") == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := m.activeCount("k"); got != 0 {
		t.Fatalf("count after Delete/sweep race = %d, want exactly 0", got)
	}
	// And the cap headroom is fully restored: 5 fresh creates all succeed.
	for i := 0; i < 5; i++ {
		if _, err := m.Create(ctx, "k", "m"); err != nil {
			t.Fatalf("post-race create %d: %v (tally leaked?)", i, err)
		}
	}
}

// TestContextTokenCapTrims proves the cumulative context-token cap trims oldest
// turns under the default trim policy (the token estimate is whitespace-token
// count, matching the echo executor).
func TestContextTokenCapTrims(t *testing.T) {
	// Each turn "one two" -> 2 tokens. Cap 5 holds at most 2 turns (4 tokens); a
	// 3rd (6) trims the oldest back to 2 turns.
	hs := NewMemoryHistoryStoreWithPolicy(0, 0, 5, OverflowTrim)
	hs.Append("s", turn("user", "a1 a2"))
	hs.Append("s", turn("user", "b1 b2"))
	hs.Append("s", turn("user", "c1 c2"))
	got, _ := hs.Get("s")
	if len(got) != 2 {
		t.Fatalf("token-cap trim len = %d, want 2: %v", len(got), got)
	}
	if got[0].Content != "b1 b2" || got[1].Content != "c1 c2" {
		t.Fatalf("token cap trimmed wrong turns: %v", got)
	}
}

// TestContextTokenCapRejects proves OverflowReject refuses the turn that would
// exceed the token cap and leaves stored history unchanged.
func TestContextTokenCapRejects(t *testing.T) {
	hs := NewMemoryHistoryStoreWithPolicy(0, 0, 4, OverflowReject)
	if err := hs.Append("s", turn("user", "a1 a2")); err != nil { // 2 tokens, fits
		t.Fatalf("append 1: %v", err)
	}
	if err := hs.Append("s", turn("user", "b1 b2")); err != nil { // total 4, fits exactly
		t.Fatalf("append 2: %v", err)
	}
	// Total would be 6 > 4: rejected, history unchanged.
	if err := hs.Append("s", turn("user", "c1 c2")); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("append 3 = %v, want ErrSessionLimitExceeded", err)
	}
	got, _ := hs.Get("s")
	if len(got) != 2 || got[0].Content != "a1 a2" || got[1].Content != "b1 b2" {
		t.Fatalf("rejected append mutated history: %v", got)
	}
}

// TestTurnCapTrimDefault proves the turn cap still trims oldest under the default
// policy (the pre-#37 behavior is preserved).
func TestTurnCapTrimDefault(t *testing.T) {
	hs := NewMemoryHistoryStoreWithPolicy(2, 0, 0, OverflowTrim)
	for _, c := range []string{"a", "b", "c"} {
		if err := hs.Append("s", turn("user", c)); err != nil {
			t.Fatalf("append %q: %v", c, err)
		}
	}
	got, _ := hs.Get("s")
	if len(got) != 2 || got[0].Content != "b" || got[1].Content != "c" {
		t.Fatalf("turn-cap trim wrong: %v", got)
	}
}

// TestTurnCapRejects proves OverflowReject refuses the turn that would exceed the
// turn cap and leaves stored history unchanged.
func TestTurnCapRejects(t *testing.T) {
	hs := NewMemoryHistoryStoreWithPolicy(2, 0, 0, OverflowReject)
	if err := hs.Append("s", turn("user", "a")); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if err := hs.Append("s", turn("user", "b")); err != nil {
		t.Fatalf("append b: %v", err)
	}
	if err := hs.Append("s", turn("user", "c")); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("append c = %v, want ErrSessionLimitExceeded", err)
	}
	got, _ := hs.Get("s")
	if len(got) != 2 || got[0].Content != "a" || got[1].Content != "b" {
		t.Fatalf("rejected turn mutated history: %v", got)
	}
}

// TestByteCapRejectsFirstOversizeTurn proves that under OverflowReject a FIRST
// turn that alone exceeds the byte cap is refused (the hard ceiling applies),
// unlike trim mode which retains an oversize sole turn.
func TestByteCapRejectsFirstOversizeTurn(t *testing.T) {
	hs := NewMemoryHistoryStoreWithPolicy(0, 8, 0, OverflowReject)
	big := turn("user", strings.Repeat("x", 100))
	if err := hs.Append("s", big); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("oversize first turn under reject = %v, want ErrSessionLimitExceeded", err)
	}
	if got, _ := hs.Get("s"); len(got) != 0 {
		t.Fatalf("rejected oversize turn was stored: %v", got)
	}
}

// TestWouldRejectMatchesAppend proves WouldReject is a faithful read-only predicate
// for the reject policy and is always false under trim.
func TestWouldRejectMatchesAppend(t *testing.T) {
	reject := NewMemoryHistoryStoreWithPolicy(2, 0, 0, OverflowReject)
	reject.Append("s", turn("user", "a"))
	reject.Append("s", turn("user", "b"))
	if !reject.WouldReject("s", turn("user", "c")) {
		t.Fatalf("WouldReject = false, want true at cap under reject")
	}
	// Trim store never rejects.
	trim := NewMemoryHistoryStoreWithPolicy(2, 0, 0, OverflowTrim)
	trim.Append("s", turn("user", "a"))
	trim.Append("s", turn("user", "b"))
	if trim.WouldReject("s", turn("user", "c")) {
		t.Fatalf("WouldReject = true under trim, want always false")
	}
	// No turns to add is never a rejection.
	if reject.WouldReject("s") {
		t.Fatalf("WouldReject with no turns should be false")
	}
}

// TestManagerCheckAppendable proves the Manager's pre-dispatch gate: owner-scoped
// (404-equivalent for a stranger), ErrSessionLimitExceeded at the cap under
// reject, and nil when the turn fits.
func TestManagerCheckAppendable(t *testing.T) {
	clk := newClock(time.Now())
	m := NewManager(NewMemorySessionStore(),
		NewMemoryHistoryStoreWithPolicy(1, 0, 0, OverflowReject),
		WithClock(clk.now), WithSweepInterval(time.Hour))
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")

	// Empty history + cap 1: a single new turn fits.
	if err := m.CheckAppendable(ctx, s.ID, "k", turn("user", "hi")); err != nil {
		t.Fatalf("CheckAppendable on empty = %v, want nil", err)
	}
	// Fill to the cap, then a further turn would be rejected.
	if err := m.AppendTurn(ctx, s.ID, "k", turn("user", "hi")); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if err := m.CheckAppendable(ctx, s.ID, "k", turn("user", "more")); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("CheckAppendable at cap = %v, want ErrSessionLimitExceeded", err)
	}
	// A stranger gets the not-found result (no existence leak), not a limit error.
	if err := m.CheckAppendable(ctx, s.ID, "stranger", turn("user", "x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("CheckAppendable as stranger = %v, want ErrSessionNotFound", err)
	}
}

// TestAppendTurnRejectModeThroughManager proves AppendTurn surfaces
// ErrSessionLimitExceeded through the Manager in reject mode.
func TestAppendTurnRejectModeThroughManager(t *testing.T) {
	clk := newClock(time.Now())
	m := NewManager(NewMemorySessionStore(),
		NewMemoryHistoryStoreWithPolicy(1, 0, 0, OverflowReject),
		WithClock(clk.now), WithSweepInterval(time.Hour))
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")
	if err := m.AppendTurn(ctx, s.ID, "k", turn("user", "a")); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := m.AppendTurn(ctx, s.ID, "k", turn("user", "b")); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("append 2 = %v, want ErrSessionLimitExceeded", err)
	}
}

// TestParseOverflowPolicy covers the config-string mapping incl. the bad-value
// fallback.
func TestParseOverflowPolicy(t *testing.T) {
	cases := []struct {
		in     string
		want   OverflowPolicy
		wantOK bool
	}{
		{"", OverflowTrim, true},
		{"trim", OverflowTrim, true},
		{"TRIM", OverflowTrim, true},
		{" reject ", OverflowReject, true},
		{"reject", OverflowReject, true},
		{"nonsense", OverflowTrim, false},
	}
	for _, c := range cases {
		got, ok := ParseOverflowPolicy(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseOverflowPolicy(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
	if OverflowReject.String() != "reject" || OverflowTrim.String() != "trim" {
		t.Fatalf("String() mismatch: %q/%q", OverflowTrim.String(), OverflowReject.String())
	}
}

// tokensHelperSanity guards the token estimate against accidental drift from the
// whitespace-token heuristic the rest of the project uses.
func TestTurnTokensHeuristic(t *testing.T) {
	if got := turnTokens(types.Message{Content: "one two three"}); got != 3 {
		t.Fatalf("turnTokens content = %d, want 3", got)
	}
	m := types.Message{
		Role:      "assistant",
		ToolCalls: []types.ToolCall{{FunctionName: "get weather", Arguments: `{"city":"paris"}`}},
	}
	// "get weather" -> 2, arguments `{"city":"paris"}` -> 1 (no whitespace) = 3.
	if got := turnTokens(m); got != 3 {
		t.Fatalf("turnTokens tool call = %d, want 3", got)
	}
}
