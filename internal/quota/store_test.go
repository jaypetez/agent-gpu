package quota

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "quota.json")

	// Original store: record usage, then checkpoint.
	cs1 := NewMemoryCounterStore()
	eng1 := NewEngine(cs1, WithClock(clk.now))
	key := keyWith("k1", Limits{RPM: 100, DailyTokens: 1000})

	if err := eng1.CheckAndReserve(ctx, key); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	eng1.RecordTokens(ctx, key.ID, 42)
	if err := cs1.Checkpoint(path, clk.now()); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// New store loaded from the checkpoint: counts survive, windows intact.
	cs2 := NewMemoryCounterStore()
	if err := cs2.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	eng2 := NewEngine(cs2, WithClock(clk.now))
	snap, err := eng2.UsageForKey(ctx, key)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if snap.RequestsThisMinute != 1 {
		t.Fatalf("RequestsThisMinute after restart = %d, want 1", snap.RequestsThisMinute)
	}
	if snap.TokensToday != 42 {
		t.Fatalf("TokensToday after restart = %d, want 42", snap.TokensToday)
	}
}

func TestCheckpointRollsExpiredWindowsOnRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "quota.json")

	cs1 := NewMemoryCounterStore()
	eng1 := NewEngine(cs1, WithClock(clk.now))
	key := keyWith("k1", Limits{RPM: 100, DailyTokens: 1000})
	_ = eng1.CheckAndReserve(ctx, key)
	eng1.RecordTokens(ctx, key.ID, 42)
	if err := cs1.Checkpoint(path, clk.now()); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Restart on a LATER day: the minute and day windows have expired and roll
	// to zero on first access; the month window persists.
	laterClk := newClock(time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC))
	cs2 := NewMemoryCounterStore()
	if err := cs2.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	eng2 := NewEngine(cs2, WithClock(laterClk.now))
	snap, _ := eng2.UsageForKey(ctx, key)
	if snap.RequestsThisMinute != 0 {
		t.Fatalf("RequestsThisMinute after expiry = %d, want 0", snap.RequestsThisMinute)
	}
	if snap.TokensToday != 0 {
		t.Fatalf("TokensToday after day expiry = %d, want 0", snap.TokensToday)
	}
	if snap.TokensThisMonth != 42 {
		t.Fatalf("TokensThisMonth (same month) = %d, want 42", snap.TokensThisMonth)
	}
}

func TestLoadCheckpointMissingFile(t *testing.T) {
	t.Parallel()
	cs := NewMemoryCounterStore()
	if err := cs.LoadCheckpoint(filepath.Join(t.TempDir(), "does-not-exist.json")); err != nil {
		t.Fatalf("missing checkpoint should not error, got %v", err)
	}
}

func TestCheckpointEmptyPath(t *testing.T) {
	t.Parallel()
	cs := NewMemoryCounterStore()
	if err := cs.Checkpoint("", time.Now()); err == nil {
		t.Fatal("empty checkpoint path should error")
	}
}

func TestReserveDeniedPersistsRolledState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	cs := NewMemoryCounterStore()
	_ = cs

	// Sanity: a denied reserve does not increment the request counter.
	eng := NewEngine(cs, WithClock(clk.now))
	key := keyWith("k1", Limits{RPM: 1})
	if err := eng.CheckAndReserve(ctx, key); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := eng.CheckAndReserve(ctx, key); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second reserve: want ErrQuotaExceeded, got %v", err)
	}
	snap, _ := eng.UsageForKey(ctx, key)
	if snap.RequestsThisMinute != 1 {
		t.Fatalf("denied reserve must not increment: RequestsThisMinute = %d, want 1", snap.RequestsThisMinute)
	}
}

// errStore is a CounterStore stub whose AddTokens always fails, to exercise the
// engine's error-logging path without panicking.
type errStore struct{ *MemoryCounterStore }

func (e *errStore) AddTokens(context.Context, string, time.Time, uint64) error {
	return errors.New("boom")
}

func TestRecordTokensSwallowsStoreError(t *testing.T) {
	t.Parallel()
	var cs CounterStore = &errStore{MemoryCounterStore: NewMemoryCounterStore()}
	eng := NewEngine(cs)
	// Must not panic; the error is logged and swallowed.
	eng.RecordTokens(context.Background(), "k1", 5)
}
