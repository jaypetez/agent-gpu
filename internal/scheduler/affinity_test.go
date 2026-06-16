package scheduler

import (
	"testing"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestPickPreferringEmptyMatchesPick asserts that with no preferred worker (or a
// preferred id not in the fleet), PickPreferring is byte-identical to Pick — the
// no-affinity path must be unchanged.
func TestPickPreferringEmptyMatchesPick(t *testing.T) {
	fleet := []types.Worker{
		online("a", 32*gib, 80, 0),
		online("b", 8*gib, 0, 0),
		online("c", 16*gib, 10, 1, "llama3"),
	}
	want, ok := Pick(fleet, "llama3")
	if !ok {
		t.Fatal("Pick ok = false")
	}
	if got, ok := PickPreferring(fleet, "llama3", ""); !ok || got != want {
		t.Fatalf("PickPreferring(empty) = %q, %v; want %q (== Pick)", got, ok, want)
	}
	// A preferred id that is not a candidate is ignored (no preference applied).
	if got, ok := PickPreferring(fleet, "llama3", "ghost"); !ok || got != want {
		t.Fatalf("PickPreferring(absent) = %q, %v; want %q (== Pick)", got, ok, want)
	}
}

// TestPickPreferringBoundWins asserts the affinity bonus lets a bound worker win
// over a peer with more free VRAM / lower load (neither has the model loaded), so
// a conversation's warm worker is reused.
func TestPickPreferringBoundWins(t *testing.T) {
	bound := online("a", 8*gib, 50, 3)   // bound but otherwise worse
	fresher := online("b", 64*gib, 0, 0) // more VRAM, idle
	// Without affinity, the fresher worker wins.
	if got, _ := Pick([]types.Worker{bound, fresher}, "llama3"); got != "b" {
		t.Fatalf("baseline Pick = %q; want b", got)
	}
	// With affinity to "a", the bound worker wins.
	got, ok := PickPreferring([]types.Worker{bound, fresher}, "llama3", "a")
	if !ok || got != "a" {
		t.Fatalf("PickPreferring = %q, %v; want a (bound bonus wins)", got, ok)
	}
}

// TestPickPreferringModelLoadedDominatesBonus asserts the model-loaded weight
// still dominates the affinity bonus: a different worker that already has the
// model loaded beats a bound worker that does not. The bonus sits below the
// model-loaded weight by design.
func TestPickPreferringModelLoadedDominatesBonus(t *testing.T) {
	boundNoModel := online("a", 64*gib, 0, 0)     // bound, idle, but model NOT loaded
	loaded := online("b", 1*gib, 90, 5, "llama3") // model loaded, busy, little VRAM
	got, ok := PickPreferring([]types.Worker{boundNoModel, loaded}, "llama3", "a")
	if !ok || got != "b" {
		t.Fatalf("PickPreferring = %q, %v; want b (model-loaded dominates affinity bonus)", got, ok)
	}
}

// TestPickPreferringBoundLoadedBeatsLoadedPeer asserts that when both the bound
// worker and a peer have the model loaded, the affinity bonus breaks the tie in
// favor of the bound worker even if the peer has somewhat more headroom.
func TestPickPreferringBoundLoadedBeatsLoadedPeer(t *testing.T) {
	boundLoaded := online("a", 8*gib, 40, 2, "llama3")
	peerLoaded := online("b", 16*gib, 10, 0, "llama3") // more VRAM, lower load
	// Without affinity, capacity terms pick the roomier peer.
	if got, _ := Pick([]types.Worker{boundLoaded, peerLoaded}, "llama3"); got != "b" {
		t.Fatalf("baseline Pick = %q; want b", got)
	}
	// With affinity, the warm bound worker is reused.
	got, ok := PickPreferring([]types.Worker{boundLoaded, peerLoaded}, "llama3", "a")
	if !ok || got != "a" {
		t.Fatalf("PickPreferring = %q, %v; want a (bound+loaded reused)", got, ok)
	}
}

// TestPickPreferringDrainingBoundRebinds asserts a draining bound worker is not a
// candidate (the runnability filter still applies), so a healthy worker is chosen
// instead — the rebind case.
func TestPickPreferringDrainingBoundRebinds(t *testing.T) {
	bound := online("a", 64*gib, 0, 0, "llama3")
	bound.Status = types.WorkerDraining
	healthy := online("b", 8*gib, 50, 3)
	got, ok := PickPreferring([]types.Worker{bound, healthy}, "llama3", "a")
	if !ok || got != "b" {
		t.Fatalf("PickPreferring = %q, %v; want b (draining bound not selectable → rebind)", got, ok)
	}
}

// TestPickPreferringStaleBoundRebinds asserts a stale bound worker is likewise
// not selectable; the bonus must never override the health filter.
func TestPickPreferringStaleBoundRebinds(t *testing.T) {
	bound := online("a", 64*gib, 0, 0, "llama3")
	bound.Status = types.WorkerStale
	healthy := online("b", 8*gib, 50, 3)
	got, ok := PickPreferring([]types.Worker{bound, healthy}, "llama3", "a")
	if !ok || got != "b" {
		t.Fatalf("PickPreferring = %q, %v; want b (stale bound not selectable → rebind)", got, ok)
	}
}

// TestPickPreferringUnfitBoundRebinds asserts the bonus cannot make a VRAM-unfit
// bound worker (no free VRAM, model not loaded) selectable: it is not runnable,
// so a fitting worker is chosen.
func TestPickPreferringUnfitBoundRebinds(t *testing.T) {
	bound := online("a", 0, 10, 0) // no VRAM, model not loaded → not runnable
	fits := online("b", 8*gib, 50, 3)
	got, ok := PickPreferring([]types.Worker{bound, fits}, "llama3", "a")
	if !ok || got != "b" {
		t.Fatalf("PickPreferring = %q, %v; want b (unfit bound not selectable → rebind)", got, ok)
	}
}

// TestPickPreferringGoneBoundRebinds asserts that a bound worker absent from the
// fleet (disconnected) yields a pick of the remaining best-fit worker.
func TestPickPreferringGoneBoundRebinds(t *testing.T) {
	remaining := online("b", 16*gib, 10, 1, "llama3")
	got, ok := PickPreferring([]types.Worker{remaining}, "llama3", "a")
	if !ok || got != "b" {
		t.Fatalf("PickPreferring = %q, %v; want b (gone bound → rebind)", got, ok)
	}
}

// TestPickPreferringDeterministic asserts affinity-aware selection is pure: the
// same inputs always yield the same worker.
func TestPickPreferringDeterministic(t *testing.T) {
	fleet := []types.Worker{
		online("a", 16*gib, 10, 1, "llama3"),
		online("b", 16*gib, 10, 1, "llama3"),
		online("c", 16*gib, 10, 1, "llama3"),
	}
	first, ok := PickPreferring(fleet, "llama3", "b")
	if !ok || first != "b" {
		t.Fatalf("PickPreferring = %q, %v; want b", first, ok)
	}
	for i := 0; i < 100; i++ {
		if got, ok := PickPreferring(fleet, "llama3", "b"); !ok || got != first {
			t.Fatalf("PickPreferring not deterministic: %q vs %q", got, first)
		}
	}
}
