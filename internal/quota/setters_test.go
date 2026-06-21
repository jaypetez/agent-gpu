package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// TestSetDefaultsTakesEffectLive proves SetDefaults changes the limits a key with
// no per-key override is enforced against, observed live on the check path (#92).
func TestSetDefaultsTakesEffectLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	eng, _ := newEngine(clk)

	// No defaults: a key with nil Limits is unlimited — three requests all pass.
	key := store.APIKey{ID: "k1"} // nil Limits → falls back to defaults
	for i := 0; i < 3; i++ {
		if err := eng.CheckAndReserve(ctx, key); err != nil {
			t.Fatalf("pre-set reserve %d: unexpected denial %v", i, err)
		}
	}

	// Tighten the default to RPM=1 live. The minute window has already seen 3
	// requests, so the next reserve must now be denied — the new default is in force.
	eng.SetDefaults(Limits{RPM: 1})
	if got := eng.Defaults(); got.RPM != 1 {
		t.Fatalf("Defaults() = %+v, want RPM=1", got)
	}
	if err := eng.CheckAndReserve(ctx, key); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("after SetDefaults(RPM=1): err = %v, want ErrQuotaExceeded", err)
	}

	// A fresh minute window resets the counter; with RPM=1 exactly one passes then denies.
	clk.set(clk.t.Add(time.Minute))
	if err := eng.CheckAndReserve(ctx, key); err != nil {
		t.Fatalf("new window first reserve: unexpected denial %v", err)
	}
	if err := eng.CheckAndReserve(ctx, key); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("new window second reserve: err = %v, want ErrQuotaExceeded", err)
	}
}

// TestSetDefaultsDoesNotOverridePerKey proves a per-key override still wins after
// SetDefaults (the default only applies to keys with nil Limits).
func TestSetDefaultsDoesNotOverridePerKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	eng, _ := newEngine(clk)

	eng.SetDefaults(Limits{RPM: 1})
	// A key WITH its own (generous) limit is unaffected by the tight default.
	key := keyWith("over", Limits{RPM: 100})
	for i := 0; i < 5; i++ {
		if err := eng.CheckAndReserve(ctx, key); err != nil {
			t.Fatalf("per-key override reserve %d: unexpected denial %v", i, err)
		}
	}
}

// TestSetGlobalLimitsTakesEffectLive proves SetGlobalLimits switches the global
// limiter on/off and changes its cap live, observed on CheckAndReserveGlobal (#92).
func TestSetGlobalLimitsTakesEffectLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := newClock(time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC))
	eng, _ := newEngine(clk)

	// Global limiting starts off (0/0): unlimited, and — by design — the off path
	// never touches the counter store, so the global counter stays at zero.
	for i := 0; i < 10; i++ {
		if err := eng.CheckAndReserveGlobal(ctx); err != nil {
			t.Fatalf("global limiting off: unexpected denial %v", i)
		}
	}

	// Turn it on with RPM=2 live. Because the off path never incremented the global
	// counter, the limiter starts fresh: exactly two reserves pass, the third denies.
	eng.SetGlobalLimits(2, 0)
	if got := eng.GlobalLimits(); got.RPM != 2 {
		t.Fatalf("GlobalLimits() = %+v, want RPM=2", got)
	}
	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("after SetGlobalLimits(2), reserve 1: %v", err)
	}
	if err := eng.CheckAndReserveGlobal(ctx); err != nil {
		t.Fatalf("after SetGlobalLimits(2), reserve 2: %v", err)
	}
	if err := eng.CheckAndReserveGlobal(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("after SetGlobalLimits(2), reserve 3: err = %v, want ErrQuotaExceeded", err)
	}

	// Turn it back off live; subsequent global reserves are unlimited again.
	eng.SetGlobalLimits(0, 0)
	for i := 0; i < 5; i++ {
		if err := eng.CheckAndReserveGlobal(ctx); err != nil {
			t.Fatalf("global limiting re-disabled: unexpected denial %v", i)
		}
	}
}
