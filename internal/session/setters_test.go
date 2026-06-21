package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestSetTTLStampsNewSessions proves SetTTL changes the TTL stamped onto sessions
// created AFTER the change, while sessions created before keep their original TTL
// (#92).
func TestSetTTLStampsNewSessions(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk) // WithTTL(time.Hour)

	before, err := m.Create(context.Background(), "k1", "llama")
	if err != nil {
		t.Fatalf("create before: %v", err)
	}
	if before.TTL != time.Hour {
		t.Fatalf("before TTL = %v, want 1h", before.TTL)
	}

	m.SetTTL(15 * time.Minute)
	if got := m.TTL(); got != 15*time.Minute {
		t.Fatalf("TTL() = %v, want 15m", got)
	}

	after, err := m.Create(context.Background(), "k1", "llama")
	if err != nil {
		t.Fatalf("create after: %v", err)
	}
	if after.TTL != 15*time.Minute {
		t.Errorf("after TTL = %v, want 15m (live SetTTL)", after.TTL)
	}

	// The earlier session is unchanged.
	got, err := m.Get(context.Background(), before.ID, "k1")
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if got.TTL != time.Hour {
		t.Errorf("pre-existing session TTL changed to %v, want 1h", got.TTL)
	}
}

// TestSetTTLRejectsNonPositive proves a non-positive SetTTL is ignored (the
// existing TTL stands) so a session always idles out within a bounded window.
func TestSetTTLRejectsNonPositive(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	m.SetTTL(0)
	m.SetTTL(-time.Minute)
	if got := m.TTL(); got != time.Hour {
		t.Fatalf("TTL() = %v after non-positive sets, want unchanged 1h", got)
	}
}

// TestSetMaxSessionsPerKeyTakesEffectLive proves SetMaxSessionsPerKey changes the
// concurrent-session cap live: lowering it refuses new creates for an over-cap
// owner; raising it lets them through again (#92).
func TestSetMaxSessionsPerKeyTakesEffectLive(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk) // unlimited by default

	// Two sessions while unlimited.
	if _, err := m.Create(context.Background(), "k1", "m"); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := m.Create(context.Background(), "k1", "m"); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	// Lower the cap below the current tally (2): a third create is refused.
	m.SetMaxSessionsPerKey(2)
	if got := m.MaxSessionsPerKey(); got != 2 {
		t.Fatalf("MaxSessionsPerKey() = %d, want 2", got)
	}
	if _, err := m.Create(context.Background(), "k1", "m"); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("over-cap create err = %v, want ErrSessionLimitExceeded", err)
	}

	// Raise the cap live: a new create now succeeds.
	m.SetMaxSessionsPerKey(5)
	if _, err := m.Create(context.Background(), "k1", "m"); err != nil {
		t.Fatalf("create after raising cap: %v", err)
	}

	// Back to unlimited (0): creates always succeed.
	m.SetMaxSessionsPerKey(0)
	for i := 0; i < 3; i++ {
		if _, err := m.Create(context.Background(), "k1", "m"); err != nil {
			t.Fatalf("create %d after un-capping: %v", i, err)
		}
	}
}

// TestHistoryStoreSetCapsTakesEffectLive proves MemoryHistoryStore.SetCaps changes
// the turn cap and overflow policy live, observed on the append path: under reject
// the next over-cap append is refused; switching to trim drops oldest instead (#92).
func TestHistoryStoreSetCapsTakesEffectLive(t *testing.T) {
	hs := NewMemoryHistoryStore(0, 0) // unbounded, trim

	msg := func(s string) types.Message { return types.Message{Role: "user", Content: s} }

	// Unbounded: three appends all succeed.
	for i, c := range []string{"a", "b", "c"} {
		if err := hs.Append("s1", msg(c)); err != nil {
			t.Fatalf("unbounded append %d: %v", i, err)
		}
	}

	// Tighten to maxTurns=2 under reject. The stored history already has 3 turns; a
	// further append would exceed the cap, so reject refuses it (stored history is
	// not retroactively trimmed).
	hs.SetCaps(HistoryCaps{MaxTurns: 2, Policy: OverflowReject})
	if got := hs.Caps(); got.MaxTurns != 2 || got.Policy != OverflowReject {
		t.Fatalf("Caps() = %+v, want maxTurns=2 reject", got)
	}
	if err := hs.Append("s1", msg("d")); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("reject append over cap: err = %v, want ErrSessionLimitExceeded", err)
	}

	// Switch to trim live: the append now succeeds. The "d" append above was
	// rejected (never stored), so the stored history is still [a b c]; appending "e"
	// under maxTurns=2 trims to the 2 most-recent turns, [c e].
	hs.SetCaps(HistoryCaps{MaxTurns: 2, Policy: OverflowTrim})
	if err := hs.Append("s1", msg("e")); err != nil {
		t.Fatalf("trim append: %v", err)
	}
	hist, _ := hs.Get("s1")
	if len(hist) != 2 {
		t.Fatalf("trimmed history len = %d, want 2", len(hist))
	}
	if hist[0].Content != "c" || hist[1].Content != "e" {
		t.Errorf("trimmed history = %v, want [c e]", []string{hist[0].Content, hist[1].Content})
	}
}
