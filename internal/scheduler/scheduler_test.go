package scheduler

import (
	"testing"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/types"
)

func online(id string, free uint64, load uint32, active uint32, models ...string) types.Worker {
	ms := make([]types.Model, 0, len(models))
	for _, m := range models {
		ms = append(ms, types.Model{Name: m})
	}
	return types.Worker{
		ID:         id,
		Models:     ms,
		FreeVRAM:   free,
		Load:       load,
		ActiveJobs: active,
		Status:     types.WorkerOnline,
	}
}

const gib = 1 << 30

// TestPickModelLoadedDominates asserts a worker that already has the model
// loaded beats one with far more free VRAM and lower load.
func TestPickModelLoadedDominates(t *testing.T) {
	loaded := online("a", 1*gib, 90, 5, "llama3")       // model loaded, busy, little VRAM
	idleBig := online("b", 64*gib, 0, 0 /* no model */) // tons of headroom, idle
	got, ok := Pick([]types.Worker{idleBig, loaded}, "llama3")
	if !ok || got != "a" {
		t.Fatalf("Pick = %q, %v; want a (model-loaded dominates)", got, ok)
	}
}

// TestPickFreeVRAMBeatsLoad asserts that among workers without the model loaded,
// more free VRAM wins over lower load.
func TestPickFreeVRAMBeatsLoad(t *testing.T) {
	more := online("a", 32*gib, 80, 0)
	idle := online("b", 8*gib, 0, 0)
	got, ok := Pick([]types.Worker{idle, more}, "llama3")
	if !ok || got != "a" {
		t.Fatalf("Pick = %q, %v; want a (more free VRAM beats lower load)", got, ok)
	}
}

// TestPickLoadBeatsActiveJobs asserts that with equal VRAM and no model loaded,
// lower load wins; and with equal VRAM and load, fewer active jobs wins.
func TestPickLoadThenActiveJobs(t *testing.T) {
	// Equal VRAM, differing load.
	hi := online("a", 16*gib, 60, 0)
	lo := online("b", 16*gib, 10, 0)
	if got, ok := Pick([]types.Worker{hi, lo}, "llama3"); !ok || got != "b" {
		t.Fatalf("load tie-break: Pick = %q, %v; want b (lower load)", got, ok)
	}
	// Equal VRAM and load, differing active jobs.
	busy := online("a", 16*gib, 20, 7)
	free := online("b", 16*gib, 20, 1)
	if got, ok := Pick([]types.Worker{busy, free}, "llama3"); !ok || got != "b" {
		t.Fatalf("active-job tie-break: Pick = %q, %v; want b (fewer active)", got, ok)
	}
}

// TestPickTieBreakByID asserts that fully-equal workers are broken by ascending
// ID, deterministically, regardless of input order.
func TestPickTieBreakByID(t *testing.T) {
	w1 := online("zeta", 16*gib, 10, 2)
	w2 := online("alpha", 16*gib, 10, 2)
	w3 := online("mid", 16*gib, 10, 2)
	for _, order := range [][]types.Worker{
		{w1, w2, w3},
		{w3, w2, w1},
		{w2, w3, w1},
	} {
		if got, ok := Pick(order, "llama3"); !ok || got != "alpha" {
			t.Fatalf("tie-break: Pick = %q, %v; want alpha (lowest id)", got, ok)
		}
	}
}

// TestPickSkipsDrainingAndStale asserts non-Online workers are never selected,
// even when they would otherwise score best.
func TestPickSkipsDrainingAndStale(t *testing.T) {
	draining := online("drain", 64*gib, 0, 0, "llama3")
	draining.Status = types.WorkerDraining
	stale := online("stale", 64*gib, 0, 0, "llama3")
	stale.Status = types.WorkerStale
	ok1 := online("ok", 8*gib, 50, 3)

	got, ok := Pick([]types.Worker{draining, stale, ok1}, "llama3")
	if !ok || got != "ok" {
		t.Fatalf("Pick = %q, %v; want ok (draining/stale skipped)", got, ok)
	}
}

// TestPickRequiresFitOrLoaded asserts a worker with no free VRAM and without the
// model loaded is not runnable, while one that already has the model loaded is
// runnable even at zero free VRAM.
func TestPickRequiresFitOrLoaded(t *testing.T) {
	noFit := online("full", 0, 10, 0)               // no VRAM, no model => not runnable
	loadedNoVRAM := online("a", 0, 10, 0, "llama3") // model loaded, runnable even at 0 VRAM

	if got, ok := Pick([]types.Worker{noFit}, "llama3"); ok {
		t.Fatalf("Pick = %q, %v; want no runnable worker", got, ok)
	}
	if got, ok := Pick([]types.Worker{noFit, loadedNoVRAM}, "llama3"); !ok || got != "a" {
		t.Fatalf("Pick = %q, %v; want a (loaded model runnable at 0 VRAM)", got, ok)
	}
}

// TestPickEmptyFleet asserts ok=false for an empty / all-unrunnable fleet.
func TestPickEmptyFleet(t *testing.T) {
	if _, ok := Pick(nil, "llama3"); ok {
		t.Fatal("Pick(nil) ok = true; want false")
	}
	if _, ok := Pick([]types.Worker{}, "llama3"); ok {
		t.Fatal("Pick(empty) ok = true; want false")
	}
}

// TestPickDeterministic asserts repeated calls on the same fleet always return
// the same worker (purity / no map-iteration-order dependence).
func TestPickDeterministic(t *testing.T) {
	fleet := []types.Worker{
		online("a", 16*gib, 10, 1, "llama3"),
		online("b", 16*gib, 10, 1, "llama3"),
		online("c", 16*gib, 10, 1, "llama3"),
		online("d", 16*gib, 10, 1),
	}
	first, ok := Pick(fleet, "llama3")
	if !ok {
		t.Fatal("Pick ok = false")
	}
	for i := 0; i < 100; i++ {
		got, ok := Pick(fleet, "llama3")
		if !ok || got != first {
			t.Fatalf("Pick non-deterministic: iter %d got %q, want %q", i, got, first)
		}
	}
}

// TestScoreModelLoadedAddsWeight asserts Score directly: loading the model adds
// the dominating weight.
func TestScoreModelLoaded(t *testing.T) {
	with := Score(online("a", 8*gib, 0, 0, "llama3"), "llama3")
	without := Score(online("a", 8*gib, 0, 0), "llama3")
	if with <= without {
		t.Fatalf("Score loaded=%d not-loaded=%d; loaded must dominate", with, without)
	}
	if with-without < weightModelLoaded {
		t.Fatalf("model-loaded delta = %d, want >= %d", with-without, int64(weightModelLoaded))
	}
}

func TestPriorityForRoles(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  queue.Priority
	}{
		{"admin high", []string{authz.RoleAdmin}, queue.PriorityHigh},
		{"user normal", []string{authz.RoleUser}, queue.PriorityNormal},
		{"read-only low", []string{authz.RoleReadOnly}, queue.PriorityLow},
		{"no roles low", nil, queue.PriorityLow},
		{"empty low", []string{}, queue.PriorityLow},
		{"highest wins", []string{authz.RoleReadOnly, authz.RoleUser, authz.RoleAdmin}, queue.PriorityHigh},
		{"user+readonly normal", []string{authz.RoleReadOnly, authz.RoleUser}, queue.PriorityNormal},
		{"unknown role low", []string{"someone-else"}, queue.PriorityLow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PriorityForRoles(tc.roles); got != tc.want {
				t.Fatalf("PriorityForRoles(%v) = %v, want %v", tc.roles, got, tc.want)
			}
		})
	}
}
