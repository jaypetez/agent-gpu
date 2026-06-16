package quota

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newGlobalEngine builds an Engine with the given global limits and a settable
// clock, mirroring newEngine for the per-key tests.
func newGlobalEngine(clk *mockClock, rpm, tpm uint64) (*Engine, *MemoryCounterStore) {
	cs := NewMemoryCounterStore()
	return NewEngine(cs, WithClock(clk.now), WithGlobalLimits(rpm, tpm)), cs
}

// TestCheckAndReserveGlobal_RPMReservesAndDenies proves the global limiter
// admits exactly GlobalRPM requests in a minute window and denies the next with
// ErrQuotaExceeded, regardless of which (or whether any) key is calling — the
// reservation is against the reserved global counter, not a per-key one.
func TestCheckAndReserveGlobal_RPMReservesAndDenies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 30, 0, time.UTC))
	eng, _ := newGlobalEngine(clk, 3, 0)

	for i := 0; i < 3; i++ {
		if err := eng.CheckAndReserveGlobal(ctx); err != nil {
			t.Fatalf("global request %d: unexpected error: %v", i, err)
		}
	}
	if err := eng.CheckAndReserveGlobal(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("4th global request: want ErrQuotaExceeded, got %v", err)
	}
}

// TestCheckAndReserveGlobal_ResetsOnMinuteBoundary proves the global limit is a
// fixed minute window: after the clock crosses the boundary the allowance fully
// resets and requests are admitted again.
func TestCheckAndReserveGlobal_ResetsOnMinuteBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 30, 0, time.UTC))
	eng, _ := newGlobalEngine(clk, 2, 0)

	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if err := eng.CheckAndReserveGlobal(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("request 3: want ErrQuotaExceeded, got %v", err)
	}

	clk.set(time.Date(2026, 6, 16, 10, 1, 0, 0, time.UTC))
	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("after minute reset: unexpected error: %v", err)
	}
}

// TestCheckAndReserveGlobal_Unlimited proves that with no global limit
// configured (rpm==0 && tpm==0) the limiter short-circuits to nil and never
// denies, leaving the pre-#6 behavior intact. It also asserts the global
// counter is untouched (Get returns zero requests) so the short-circuit really
// avoids the store.
func TestCheckAndReserveGlobal_Unlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	eng, cs := newGlobalEngine(clk, 0, 0)

	for i := 0; i < 1000; i++ {
		if err := eng.CheckAndReserveGlobal(ctx); err != nil {
			t.Fatalf("global request %d under unlimited: %v", i, err)
		}
	}
	c, err := cs.Get(ctx, globalKeyID, clk.now())
	if err != nil {
		t.Fatalf("get global counter: %v", err)
	}
	if c.MinuteRequests != 0 {
		t.Errorf("global MinuteRequests = %d, want 0 (short-circuit must not reserve)", c.MinuteRequests)
	}
}

// TestCheckAndReserveGlobal_DoesNotTouchPerKeyCounters proves global reservation
// accounts entirely separately from per-key counters: after exhausting the
// global limit, an arbitrary real key's counter is still zero, so a global
// denial never consumes a key's RPM and an allowed request is still
// independently subject to its per-key CheckAndReserve.
func TestCheckAndReserveGlobal_DoesNotTouchPerKeyCounters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	eng, cs := newGlobalEngine(clk, 1, 0)

	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("first global request: %v", err)
	}
	if err := eng.CheckAndReserveGlobal(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second global request: want ErrQuotaExceeded, got %v", err)
	}

	c, err := cs.Get(ctx, "agpu_realkey", clk.now())
	if err != nil {
		t.Fatalf("get key counter: %v", err)
	}
	if c.MinuteRequests != 0 {
		t.Errorf("per-key MinuteRequests = %d, want 0 (global must not touch a key)", c.MinuteRequests)
	}
}

// TestCheckAndReserveGlobal_TPMBudget proves the global token budget denies only
// once already exhausted (>= limit), mirroring the per-key TPM semantics:
// CheckAndReserveGlobal admits the request whose tokens are not yet known and
// denies the next once recorded tokens reach the limit. It drives the real
// RecordGlobalTokens path (the request path's post-dispatch accounting), not a
// raw cs.AddTokens, so the test exercises the seam production actually uses.
func TestCheckAndReserveGlobal_TPMBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	eng, _ := newGlobalEngine(clk, 0, 10)

	// Under budget: admitted (RPM is unlimited here, only TPM is set).
	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("under token budget: %v", err)
	}
	// Record the global tokens via the engine, exactly as the server does after a
	// job completes; this drives the global minute-token counter to the limit.
	eng.RecordGlobalTokens(ctx, 10)
	if err := eng.CheckAndReserveGlobal(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over token budget: want ErrQuotaExceeded, got %v", err)
	}
}

// TestCheckAndReserveGlobal_TPMResetsOnMinuteBoundary proves the global token
// budget is a fixed minute window: after recording enough tokens to exhaust it
// (denying the next request), crossing the minute boundary fully resets the
// allowance and requests are admitted again — the same window discipline as RPM.
func TestCheckAndReserveGlobal_TPMResetsOnMinuteBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 30, 0, time.UTC))
	eng, _ := newGlobalEngine(clk, 0, 10)

	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("under token budget: %v", err)
	}
	eng.RecordGlobalTokens(ctx, 10)
	if err := eng.CheckAndReserveGlobal(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over token budget: want ErrQuotaExceeded, got %v", err)
	}

	clk.set(time.Date(2026, 6, 16, 10, 1, 0, 0, time.UTC))
	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("after minute reset: unexpected error: %v", err)
	}
}

// TestRecordGlobalTokens_ShortCircuitsWhenUnlimited proves RecordGlobalTokens
// never touches the counter store when no global limits are configured
// (RPM==0 && TPM==0 — the default), so the default install has zero global
// counter growth, consistent with CheckAndReserveGlobal's short-circuit.
func TestRecordGlobalTokens_ShortCircuitsWhenUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	eng, cs := newGlobalEngine(clk, 0, 0)

	eng.RecordGlobalTokens(ctx, 1000)

	c, err := cs.Get(ctx, globalKeyID, clk.now())
	if err != nil {
		t.Fatalf("get global counter: %v", err)
	}
	if c.MinuteTokens != 0 {
		t.Errorf("global MinuteTokens = %d, want 0 (short-circuit must not record)", c.MinuteTokens)
	}
}

// TestRecordGlobalTokens_RecordsWhenLimited proves RecordGlobalTokens does drive
// the global minute-token counter when a global limit is configured, so the TPM
// budget is actually enforced end-to-end (the bug this fixes: nothing recorded
// onto the global counter, making global TPM silently non-functional).
func TestRecordGlobalTokens_RecordsWhenLimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	eng, cs := newGlobalEngine(clk, 0, 100)

	eng.RecordGlobalTokens(ctx, 7)
	eng.RecordGlobalTokens(ctx, 0) // n==0 is a no-op.

	c, err := cs.Get(ctx, globalKeyID, clk.now())
	if err != nil {
		t.Fatalf("get global counter: %v", err)
	}
	if c.MinuteTokens != 7 {
		t.Errorf("global MinuteTokens = %d, want 7", c.MinuteTokens)
	}
}

// TestGlobalMinuteReset_TracksClock proves GlobalMinuteReset returns the next
// minute boundary from the engine clock, the seam the request path uses for the
// Retry-After hint on a global 429.
func TestGlobalMinuteReset_TracksClock(t *testing.T) {
	t.Parallel()
	clk := newClock(time.Date(2026, 6, 16, 10, 0, 30, 0, time.UTC))
	eng, _ := newGlobalEngine(clk, 1, 0)

	want := time.Date(2026, 6, 16, 10, 1, 0, 0, time.UTC)
	if got := eng.GlobalMinuteReset(); !got.Equal(want) {
		t.Errorf("GlobalMinuteReset() = %v, want %v", got, want)
	}
}

// TestSnapshotRetryAfter picks the soonest reset among exhausted dimensions, and
// falls back to the soonest forward-looking window reset when none is reported
// at/over its limit. It is the per-key Retry-After computation.
func TestSnapshotRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 16, 10, 0, 30, 0, time.UTC)
	minuteReset := time.Date(2026, 6, 16, 10, 1, 0, 0, time.UTC)
	dayReset := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)

	// RPM at limit -> minute reset is the hint.
	s := Snapshot{
		Limits:             Limits{RPM: 2, DailyTokens: 100},
		RequestsThisMinute: 2,
		TokensToday:        10,
		MinuteResetsAt:     minuteReset,
		DayResetsAt:        dayReset,
	}
	got, ok := s.RetryAfter(now)
	if !ok || !got.Equal(minuteReset) {
		t.Errorf("RPM-at-limit RetryAfter() = (%v,%v), want (%v,true)", got, ok, minuteReset)
	}

	// Only the daily budget is exhausted -> day reset is the hint (not the sooner
	// minute reset, which is not at limit).
	s = Snapshot{
		Limits:             Limits{RPM: 2, DailyTokens: 100},
		RequestsThisMinute: 1,
		TokensToday:        100,
		MinuteResetsAt:     minuteReset,
		DayResetsAt:        dayReset,
	}
	got, ok = s.RetryAfter(now)
	if !ok || !got.Equal(dayReset) {
		t.Errorf("daily-exhausted RetryAfter() = (%v,%v), want (%v,true)", got, ok, dayReset)
	}

	// Nothing at limit -> fall back to the soonest forward-looking reset.
	s = Snapshot{
		Limits:         Limits{RPM: 2},
		MinuteResetsAt: minuteReset,
		DayResetsAt:    dayReset,
		MonthResetsAt:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	got, ok = s.RetryAfter(now)
	if !ok || !got.Equal(minuteReset) {
		t.Errorf("fallback RetryAfter() = (%v,%v), want (%v,true)", got, ok, minuteReset)
	}

	// No windows after now -> not ok (omit Retry-After).
	if _, ok := (Snapshot{}).RetryAfter(now); ok {
		t.Errorf("empty snapshot RetryAfter() ok = true, want false")
	}
}
