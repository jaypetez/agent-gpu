// Package scheduler is the capacity-aware placement core for agent-gpu: given a
// fleet snapshot and a target model, it selects the best-fit worker via a pure,
// deterministic scoring function, and it derives a job's queue priority from the
// owning API key's roles.
//
// # Purity & determinism
//
// Pick and Score are pure: same inputs always produce the same output. They read
// no clock, take no locks, and never depend on map-iteration order. Ties are
// broken by worker ID (ascending) so the selected worker is stable across calls.
// This is what makes the placement decision reproducible and unit-testable in
// isolation from the server's concurrency.
//
// # Runnability
//
// A worker is a candidate for a model only when it is Online (not draining, not
// stale) AND it can plausibly run the model: either it already has the model
// loaded, or it reports free VRAM to load it. Draining/stale workers are never
// selected — the server's liveness state is carried in the Worker snapshot's
// Status, computed by the fleet view.
//
// # Scoring weights (highest influence first)
//
//  1. model-already-loaded  — dominates everything else. Reusing a loaded model
//     avoids a cold load/reload, the single biggest
//     latency win available to the scheduler today.
//  2. more free VRAM        — headroom to load and run without thrashing.
//  3. lower load            — the worker's reported 0-100 utilization; prefer the
//     least-busy GPU.
//  4. fewer active jobs     — final tie-break on raw concurrency.
//
// Because real model-size data does not exist yet (#16), "fit" is approximated
// as FreeVRAM > 0; once per-model VRAM requirements land, the runnability filter
// and the VRAM term should compare against the model's actual footprint. See
// docs/architecture.md (Scheduling) for the FUTURE-work notes on anti-starvation
// aging and real VRAM-fit.
//
// # Priority
//
// PriorityForRoles centralizes the (interim) mapping from an API key's roles to
// a queue priority, until an explicit per-key priority field exists. It is a
// single small function precisely so it is trivially swapped later.
//
// # Scope
//
// This package owns the placement math only. Holding a worker handle, dispatching
// over the stream, the queue, and waking on capacity changes are the server's
// job; the scheduler is the deterministic decision it consults.
package scheduler

import (
	"sort"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// Scoring weights. They are ordered by orders of magnitude so a higher-priority
// term can never be outweighed by the sum of every lower term within its
// expected range: model-locality dominates VRAM headroom, which dominates load,
// which dominates active-job count. Documented on the package.
const (
	// weightModelLoaded is added when the worker already has the target model
	// loaded. It dwarfs every capacity term so locality always wins.
	weightModelLoaded = 1_000_000_000

	// freeVRAMUnit scales free VRAM into the score. VRAM is reported in bytes;
	// dividing by a gibibyte keeps the term in a sane range while still ordering
	// workers by available headroom (whole-GiB granularity).
	freeVRAMUnit = 1 << 30 // 1 GiB

	// weightFreeVRAMPerGiB weights each free GiB. With load capped at 100 and the
	// load weight below, free-VRAM differences dominate load differences.
	weightFreeVRAMPerGiB = 10_000

	// weightLoadPenalty is subtracted per unit of reported load (0-100), so a
	// less-loaded worker scores higher. Bounded by 100*weightLoadPenalty, which
	// stays below one GiB of VRAM weight.
	weightLoadPenalty = 50

	// weightActiveJobPenalty is the final tie-break: fewer active jobs scores
	// higher. Small so it only separates workers that are otherwise equal.
	weightActiveJobPenalty = 1

	// weightBoundWorkerBonus is added to a session's affinity-bound worker when it
	// is among the runnable candidates (#34). It is large enough to outrank every
	// capacity term — so a warm bound worker reliably wins over a fresher peer —
	// yet strictly BELOW weightModelLoaded, so a different worker that already has
	// the model loaded still dominates a bound worker that does not. It only ranks
	// among already-runnable candidates and can never make a draining/stale or
	// VRAM-unfit worker selectable: affinity is a strong preference, not a pin.
	weightBoundWorkerBonus = 100_000_000
)

// runnable reports whether w can be considered for the given model: it must be
// Online (draining/stale workers are never picked) and either already have the
// model loaded or report free VRAM to load it.
func runnable(w types.Worker, model string) bool {
	if w.Status != types.WorkerOnline {
		return false
	}
	if hasModel(w, model) {
		return true
	}
	return w.FreeVRAM > 0
}

// hasModel reports whether the worker snapshot lists model among its loaded /
// available models.
func hasModel(w types.Worker, model string) bool {
	for _, m := range w.Models {
		if m.Name == model {
			return true
		}
	}
	return false
}

// Score computes the placement desirability of running model on w. Higher is
// better. It is pure and deterministic; it does NOT check runnability (callers
// filter first via Pick). The weights are documented on the package: model
// locality dominates, then free VRAM, then lower load, then fewer active jobs.
func Score(w types.Worker, model string) int64 {
	var score int64
	if hasModel(w, model) {
		score += weightModelLoaded
	}
	score += int64(w.FreeVRAM/freeVRAMUnit) * weightFreeVRAMPerGiB
	score -= int64(w.Load) * weightLoadPenalty
	score -= int64(w.ActiveJobs) * weightActiveJobPenalty
	return score
}

// Pick selects the best-fit worker for model from a fleet snapshot, returning
// its ID and ok=true. It filters to runnable candidates, scores each, and
// returns the highest-scoring one; ties are broken by worker ID (ascending) so
// the result is stable. ok is false when no worker is runnable for the model.
//
// Pick is pure and deterministic: it copies and sorts candidate IDs rather than
// relying on the input slice order or any map iteration, so identical fleets
// always yield the same choice.
//
// Pick applies no session affinity; it is PickPreferring with no preferred
// worker. The existing keyless/internal callers use it unchanged.
func Pick(workers []types.Worker, model string) (workerID string, ok bool) {
	return PickPreferring(workers, model, "")
}

// PickPreferring is Pick with session affinity (#34): preferredWorkerID names
// the worker a conversation is bound to (its warm KV cache). When that worker is
// among the runnable candidates, it receives weightBoundWorkerBonus so it wins
// over fresher peers — but the bonus sits strictly below the model-loaded weight,
// so a different worker that already has the model loaded still wins, and the
// bonus never lets a non-runnable (draining/stale/VRAM-unfit) worker be chosen.
// Net: a warm bound worker is reused; a gone/draining/stale bound worker simply
// is not a candidate, so the best-fit fresh worker is picked instead (rebind).
//
// An empty preferredWorkerID (or one not in the candidate set) makes PickPreferring
// behave exactly like Pick, so the no-affinity path is byte-identical. It remains
// pure and deterministic.
func PickPreferring(workers []types.Worker, model, preferredWorkerID string) (workerID string, ok bool) {
	// Index runnable candidates by ID so the decision does not depend on the
	// input slice's order. Sorting the IDs gives the deterministic tie-break.
	byID := make(map[string]types.Worker, len(workers))
	ids := make([]string, 0, len(workers))
	for _, w := range workers {
		if !runnable(w, model) {
			continue
		}
		byID[w.ID] = w
		ids = append(ids, w.ID)
	}
	if len(ids) == 0 {
		return "", false
	}
	sort.Strings(ids)

	// scoreFor applies the affinity bonus only when id is the preferred worker AND
	// that worker is in the runnable candidate set (it is, by construction of byID).
	// A non-runnable preferred worker never reaches here, so the bonus can never
	// override the runnability filter.
	scoreFor := func(id string) int64 {
		s := Score(byID[id], model)
		if preferredWorkerID != "" && id == preferredWorkerID {
			s += weightBoundWorkerBonus
		}
		return s
	}

	bestID := ids[0]
	bestScore := scoreFor(bestID)
	for _, id := range ids[1:] {
		if s := scoreFor(id); s > bestScore {
			bestScore = s
			bestID = id
		}
	}
	return bestID, true
}

// PriorityForRoles maps an API key's roles to a queue priority. This is the
// interim policy until an explicit per-key priority field exists; keeping it a
// single small function makes it trivial to swap later.
//
//	admin                 → PriorityHigh
//	user                  → PriorityNormal
//	read-only / no roles  → PriorityLow
//
// When a key holds several roles, the highest-implied priority wins.
func PriorityForRoles(roles []string) queue.Priority {
	best := queue.PriorityLow
	for _, r := range roles {
		switch r {
		case authz.RoleAdmin:
			if queue.PriorityHigh > best {
				best = queue.PriorityHigh
			}
		case authz.RoleUser:
			if queue.PriorityNormal > best {
				best = queue.PriorityNormal
			}
		case authz.RoleReadOnly:
			// read-only stays at PriorityLow.
		}
	}
	return best
}
