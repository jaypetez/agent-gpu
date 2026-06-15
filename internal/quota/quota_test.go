package quota

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// mockClock is a settable, concurrency-safe clock for window-reset tests.
type mockClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *mockClock { return &mockClock{t: t} }

func (c *mockClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mockClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// keyWith returns an APIKey whose per-key limits are lim.
func keyWith(id string, lim Limits) store.APIKey {
	l := lim
	return store.APIKey{ID: id, Limits: &l}
}

func newEngine(clk *mockClock) (*Engine, *MemoryCounterStore) {
	cs := NewMemoryCounterStore()
	return NewEngine(cs, WithClock(clk.now)), cs
}

func TestCheckAndReserve_RPMBlocksUntilReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 15, 0, time.UTC))
	eng, _ := newEngine(clk)
	key := keyWith("k1", Limits{RPM: 3})

	for i := 0; i < 3; i++ {
		if err := eng.CheckAndReserve(ctx, key); err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
	}
	if err := eng.CheckAndReserve(ctx, key); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("4th request: want ErrQuotaExceeded, got %v", err)
	}

	// Cross the minute boundary: the window resets and requests are allowed again.
	clk.set(time.Date(2026, 6, 14, 10, 31, 0, 0, time.UTC))
	if err := eng.CheckAndReserve(ctx, key); err != nil {
		t.Fatalf("after minute reset: unexpected error: %v", err)
	}
}

func TestCheckAndReserve_ZeroRPMUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC))
	eng, _ := newEngine(clk)
	key := keyWith("k1", Limits{}) // all zero == unlimited

	for i := 0; i < 1000; i++ {
		if err := eng.CheckAndReserve(ctx, key); err != nil {
			t.Fatalf("request %d under unlimited: %v", i, err)
		}
	}
}

func TestTokenBudgets_TPMDailyMonthly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		lim   Limits
		check func(s Snapshot) uint64 // returns the usage for the limited dimension
	}{
		{"tpm", Limits{TPM: 10}, func(s Snapshot) uint64 { return s.TokensThisMinute }},
		{"daily", Limits{DailyTokens: 10}, func(s Snapshot) uint64 { return s.TokensToday }},
		{"monthly", Limits{MonthlyTokens: 10}, func(s Snapshot) uint64 { return s.TokensThisMonth }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			clk := newClock(time.Date(2026, 6, 14, 10, 30, 15, 0, time.UTC))
			eng, _ := newEngine(clk)
			key := keyWith("k1", tc.lim)

			// First request: budget not yet exhausted, reserve succeeds.
			if err := eng.CheckAndReserve(ctx, key); err != nil {
				t.Fatalf("first reserve: %v", err)
			}
			// Record tokens up to the limit.
			eng.RecordTokens(ctx, key.ID, 10)

			snap, err := eng.UsageForKey(ctx, key)
			if err != nil {
				t.Fatalf("usage: %v", err)
			}
			if got := tc.check(snap); got != 10 {
				t.Fatalf("usage = %d, want 10", got)
			}

			// Budget now exhausted: the next reserve is denied.
			if err := eng.CheckAndReserve(ctx, key); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("reserve with exhausted %s budget: want ErrQuotaExceeded, got %v", tc.name, err)
			}
		})
	}
}

func TestTokenWindowResets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name    string
		lim     Limits
		advance time.Time // a time that crosses the relevant boundary
		usage   func(s Snapshot) uint64
	}{
		{
			name:    "minute",
			lim:     Limits{TPM: 5},
			advance: time.Date(2026, 6, 14, 10, 31, 0, 0, time.UTC),
			usage:   func(s Snapshot) uint64 { return s.TokensThisMinute },
		},
		{
			name:    "day",
			lim:     Limits{DailyTokens: 5},
			advance: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			usage:   func(s Snapshot) uint64 { return s.TokensToday },
		},
		{
			name:    "month",
			lim:     Limits{MonthlyTokens: 5},
			advance: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			usage:   func(s Snapshot) uint64 { return s.TokensThisMonth },
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := newClock(time.Date(2026, 6, 14, 10, 30, 15, 0, time.UTC))
			eng, _ := newEngine(clk)
			key := keyWith("k1", tc.lim)

			eng.RecordTokens(ctx, key.ID, 5)
			snap, _ := eng.UsageForKey(ctx, key)
			if got := tc.usage(snap); got != 5 {
				t.Fatalf("before reset: usage = %d, want 5", got)
			}
			// Exhausted: reserve denied.
			if err := eng.CheckAndReserve(ctx, key); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("before reset: want ErrQuotaExceeded, got %v", err)
			}

			// Cross the boundary: the window resets.
			clk.set(tc.advance)
			snap, _ = eng.UsageForKey(ctx, key)
			if got := tc.usage(snap); got != 0 {
				t.Fatalf("after %s reset: usage = %d, want 0", tc.name, got)
			}
			if err := eng.CheckAndReserve(ctx, key); err != nil {
				t.Fatalf("after %s reset: reserve should succeed, got %v", tc.name, err)
			}
		})
	}
}

func TestFailedJobConsumesRPMNotTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	eng, _ := newEngine(clk)
	key := keyWith("k1", Limits{RPM: 5, DailyTokens: 100})

	// A request that produces no tokens (e.g. failed job): RecordTokens(0) is a no-op.
	if err := eng.CheckAndReserve(ctx, key); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	eng.RecordTokens(ctx, key.ID, 0)

	snap, _ := eng.UsageForKey(ctx, key)
	if snap.RequestsThisMinute != 1 {
		t.Fatalf("RequestsThisMinute = %d, want 1", snap.RequestsThisMinute)
	}
	if snap.TokensToday != 0 {
		t.Fatalf("TokensToday = %d, want 0 (failed job consumes no token budget)", snap.TokensToday)
	}
}

func TestConcurrentReserve_ExactCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	eng, _ := newEngine(clk)
	const limit = 50
	const goroutines = 500
	key := keyWith("k1", Limits{RPM: limit})

	var succeeded int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := eng.CheckAndReserve(ctx, key); err == nil {
				atomic.AddInt64(&succeeded, 1)
			} else if !errors.Is(err, ErrQuotaExceeded) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&succeeded); got != limit {
		t.Fatalf("exactly %d requests should succeed, got %d", limit, got)
	}
}

func TestPerKeyLimitsOverrideDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	cs := NewMemoryCounterStore()
	eng := NewEngine(cs, WithClock(clk.now), WithDefaults(Limits{RPM: 2}))

	// Key with nil Limits falls back to the global default of RPM=2.
	def := store.APIKey{ID: "def"}
	if err := eng.CheckAndReserve(ctx, def); err != nil {
		t.Fatalf("default 1: %v", err)
	}
	if err := eng.CheckAndReserve(ctx, def); err != nil {
		t.Fatalf("default 2: %v", err)
	}
	if err := eng.CheckAndReserve(ctx, def); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("default 3: want ErrQuotaExceeded, got %v", err)
	}

	// Key with its own Limits overrides the default.
	over := keyWith("over", Limits{RPM: 5})
	for i := 0; i < 5; i++ {
		if err := eng.CheckAndReserve(ctx, over); err != nil {
			t.Fatalf("override %d: %v", i, err)
		}
	}
	if err := eng.CheckAndReserve(ctx, over); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("override 6: want ErrQuotaExceeded, got %v", err)
	}
}

func TestWindowStartBoundaries(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 14, 10, 30, 45, 123, time.UTC)
	if got := windowStart(windowMinute, at); !got.Equal(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("minute start = %v", got)
	}
	if got := windowStart(windowDay, at); !got.Equal(time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day start = %v", got)
	}
	if got := windowStart(windowMonth, at); !got.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month start = %v", got)
	}
	// Non-UTC input is normalized to UTC boundaries.
	loc := time.FixedZone("UTC+5", 5*3600)
	atLoc := time.Date(2026, 6, 14, 2, 30, 0, 0, loc) // 21:30 UTC on the 13th
	if got := windowStart(windowDay, atLoc); !got.Equal(time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("non-UTC day start = %v, want 2026-06-13 UTC", got)
	}
}
