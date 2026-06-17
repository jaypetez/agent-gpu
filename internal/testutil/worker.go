package testutil

import (
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// modelsFromNames maps bare model names to types.Model values (no digest). It
// backs the WithWorkerModels / WithHeartbeatModels name-only convenience.
func modelsFromNames(names []string) []types.Model {
	if len(names) == 0 {
		return nil
	}
	out := make([]types.Model, 0, len(names))
	for _, n := range names {
		out = append(out, types.Model{Name: n})
	}
	return out
}

// WorkerOption mutates a types.Worker during construction. Options are applied in
// the order given, so a later option overrides an earlier one.
type WorkerOption func(*types.Worker)

// Worker builds an Online types.Worker with a default id ("worker-1") and no
// models, then applies opts in order. It unifies the assorted online-worker
// constructors (scheduler.online, httpapi.onlineWorker, and hand-built fleet
// snapshots) behind one builder.
//
// The default is deliberately Online with zero capacity so a snapshot-only test
// (e.g. model aggregation) states just its models; scheduling tests layer on
// WithFreeVRAM / WithLoad / WithActiveJobs to drive Pick.
func Worker(opts ...WorkerOption) types.Worker {
	w := types.Worker{
		ID:     "worker-1",
		Status: types.WorkerOnline,
	}
	for _, opt := range opts {
		opt(&w)
	}
	return w
}

// WithWorkerID sets the worker id.
func WithWorkerID(id string) WorkerOption {
	return func(w *types.Worker) { w.ID = id }
}

// WithWorkerModels sets the worker's served models from bare names (no digest).
// For digests, use WithWorkerModelObjects.
func WithWorkerModels(names ...string) WorkerOption {
	return func(w *types.Worker) { w.Models = modelsFromNames(names) }
}

// WithWorkerModelObjects sets the worker's served models from full types.Model
// values (name + digest), for tests that assert on the digest.
func WithWorkerModelObjects(models ...types.Model) WorkerOption {
	return func(w *types.Worker) { w.Models = models }
}

// WithFreeVRAM sets the worker's reported free VRAM in bytes.
func WithFreeVRAM(free uint64) WorkerOption {
	return func(w *types.Worker) { w.FreeVRAM = free }
}

// WithTotalVRAM sets the worker's reported total VRAM in bytes.
func WithTotalVRAM(total uint64) WorkerOption {
	return func(w *types.Worker) { w.TotalVRAM = total }
}

// WithLoad sets the worker's reported load percentage.
func WithLoad(load uint32) WorkerOption {
	return func(w *types.Worker) { w.Load = load }
}

// WithActiveJobs sets the worker's reported in-flight job count.
func WithActiveJobs(active uint32) WorkerOption {
	return func(w *types.Worker) { w.ActiveJobs = active }
}

// WithGPUType sets the worker's reported GPU type string.
func WithGPUType(gpuType string) WorkerOption {
	return func(w *types.Worker) { w.GPUType = gpuType }
}

// WithStatus sets the worker's lifecycle status (Online / Draining / Stale).
func WithStatus(s types.WorkerStatus) WorkerOption {
	return func(w *types.Worker) { w.Status = s }
}

// WithLastSeen sets the worker's last-seen timestamp.
func WithLastSeen(t time.Time) WorkerOption {
	return func(w *types.Worker) { w.LastSeen = t }
}

// HeartbeatOption mutates a types.Heartbeat during construction. Options are
// applied in the order given, so a later option overrides an earlier one.
type HeartbeatOption func(*types.Heartbeat)

// Heartbeat builds a types.Heartbeat with a default worker id ("worker-1") and no
// capacity, then applies opts in order. It unifies the hand-built heartbeat
// literals scattered across the control-plane tests.
func Heartbeat(opts ...HeartbeatOption) types.Heartbeat {
	h := types.Heartbeat{WorkerID: "worker-1"}
	for _, opt := range opts {
		opt(&h)
	}
	return h
}

// WithHeartbeatWorkerID sets the heartbeat's worker id.
func WithHeartbeatWorkerID(id string) HeartbeatOption {
	return func(h *types.Heartbeat) { h.WorkerID = id }
}

// WithHeartbeatModels sets the heartbeat's advertised models from bare names.
func WithHeartbeatModels(names ...string) HeartbeatOption {
	return func(h *types.Heartbeat) { h.AvailableModels = modelsFromNames(names) }
}

// WithHeartbeatModelObjects sets the heartbeat's advertised models from full
// types.Model values (name + digest).
func WithHeartbeatModelObjects(models ...types.Model) HeartbeatOption {
	return func(h *types.Heartbeat) { h.AvailableModels = models }
}

// WithHeartbeatVRAM sets the heartbeat's total and free VRAM in bytes.
func WithHeartbeatVRAM(total, free uint64) HeartbeatOption {
	return func(h *types.Heartbeat) {
		h.TotalVRAM = total
		h.FreeVRAM = free
	}
}

// WithHeartbeatLoad sets the heartbeat's reported load percentage.
func WithHeartbeatLoad(load uint32) HeartbeatOption {
	return func(h *types.Heartbeat) { h.Load = load }
}

// WithHeartbeatActiveJobs sets the heartbeat's reported in-flight job count.
func WithHeartbeatActiveJobs(active uint32) HeartbeatOption {
	return func(h *types.Heartbeat) { h.ActiveJobs = active }
}

// WithHeartbeatGPUType sets the heartbeat's reported GPU type string.
func WithHeartbeatGPUType(gpuType string) HeartbeatOption {
	return func(h *types.Heartbeat) { h.GPUType = gpuType }
}
