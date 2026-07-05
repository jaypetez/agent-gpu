package httpapi

import (
	"net/http"
	"sort"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// GPU/fleet capacity inventory (#94): an aggregated, read-only view over the
// existing fleet heartbeat capacity fields. It performs NO new GPU probing — it
// reduces the same per-worker snapshot the admin worker endpoints expose
// (GPUType + TotalVRAM/FreeVRAM/Load aggregates) into three sections an operator
// dashboard wants: a fleet roll-up, a by-GPU-type grouping, and the per-worker
// cells suitable for a utilization heatmap. Because the control plane tracks GPU
// capacity only at the aggregate per-worker level (there is no per-device
// breakdown in the snapshot), a heatmap "cell" is one WORKER, and GPUType is the
// worker's reported human string (e.g. "2x NVIDIA GeForce RTX 4090", or "cpu"
// for a GPU-less worker), grouped as-is rather than parsed for device counts.

// adminGPUsResponse is the GET /v1/admin/gpus response: a fleet-wide GPU capacity
// inventory derived live from the heartbeat snapshot (no caching, no probing).
// All arrays are emitted as [] (never null) so a client can iterate without a nil
// guard, including for an empty fleet (which yields zero aggregates and empty
// arrays).
type adminGPUsResponse struct {
	Fleet   adminGPUFleet    `json:"fleet"`
	ByType  []adminGPUByType `json:"by_type"`
	Workers []adminGPUCell   `json:"workers"`
}

// adminGPUFleet is the fleet roll-up section: how many workers were observed, the
// summed total/free VRAM (bytes) across them, and the load distribution
// (mean/max of the coarse 0-100 per-worker load). MeanLoad is the integer mean
// over WorkerCount, 0 when the fleet is empty (no divide-by-zero); MaxLoad is the
// single busiest worker's load, 0 when the fleet is empty.
type adminGPUFleet struct {
	WorkerCount int    `json:"worker_count"`
	TotalVRAM   uint64 `json:"total_vram"`
	FreeVRAM    uint64 `json:"free_vram"`
	MeanLoad    uint32 `json:"mean_load"`
	MaxLoad     uint32 `json:"max_load"`
}

// adminGPUByType is one row of the by-GPU-type grouping: every worker reporting
// the same GPUType string is folded into one row carrying the worker count and
// the summed total/free VRAM for that type. The grouping is on the worker's
// reported GPUType verbatim (a CPU-only worker groups under "cpu"); the string is
// not parsed into per-device counts because the snapshot carries no per-device
// data.
type adminGPUByType struct {
	GPUType     string `json:"gpu_type"`
	WorkerCount int    `json:"worker_count"`
	TotalVRAM   uint64 `json:"total_vram"`
	FreeVRAM    uint64 `json:"free_vram"`
}

// adminGPUCell is one heatmap cell: a single worker's capacity/utilization. A
// cell is per WORKER (not per physical GPU) because the fleet snapshot tracks GPU
// capacity only at the aggregate per-worker level. It mirrors the capacity fields
// of the worker list view so a dashboard can render a worker×metric heatmap
// directly from this section.
type adminGPUCell struct {
	ID         string `json:"id"`
	GPUType    string `json:"gpu_type"`
	TotalVRAM  uint64 `json:"total_vram"`
	FreeVRAM   uint64 `json:"free_vram"`
	Load       uint32 `json:"load"`
	Status     string `json:"status"`
	ActiveJobs uint32 `json:"active_jobs"`
}

// handleAdminGPUs serves GET /v1/admin/gpus (#94). It reads the fleet snapshot
// ONCE and reduces it into the aggregated GPU capacity inventory: a fleet roll-up
// (worker count, summed total/free VRAM, mean/max load), a per-GPU-type grouping
// (sorted by gpu_type), and the per-worker heatmap cells (sorted by id). Both
// orderings are deterministic so the response is stable across calls over the
// same snapshot. The values derive solely from the existing heartbeat capacity
// fields — no new GPU probing is performed. An empty fleet yields zero aggregates
// and empty (non-null) arrays with a 200. Gated to the workers:read scope
// (s.requireScope), so a key lacking it gets 403 and an unauthenticated request
// 401 before this runs. This is a pure read and is not audited (matching the
// other admin read endpoints).
func (s *Server) handleAdminGPUs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, aggregateGPUs(s.fleet.Fleet()))
}

// aggregateGPUs reduces a fleet snapshot into the GPU capacity inventory: a fleet
// roll-up (worker count, summed total/free VRAM, mean/max load), a per-GPU-type
// grouping (sorted by gpu_type), and the per-worker heatmap cells (sorted by id).
// It performs NO probing — it folds the existing heartbeat capacity fields — and
// is deterministic over a given snapshot. Extracted so BOTH the JSON endpoint
// (#94) and the console's GPU heatmap (#101) reduce the fleet identically and can
// never disagree. An empty fleet yields zero aggregates and empty (non-null)
// arrays.
func aggregateGPUs(fleet []types.Worker) adminGPUsResponse {
	// Fleet roll-up: sum VRAM and load, track the max load, over a single pass.
	var totalVRAM, freeVRAM uint64
	var loadSum uint64
	var maxLoad uint32
	// byType accumulates per-GPU-type aggregates keyed by the reported GPUType
	// string; the keys are materialized into a sorted slice afterwards so the
	// output ordering is deterministic.
	byType := make(map[string]*adminGPUByType)
	cells := make([]adminGPUCell, 0, len(fleet))

	for _, wk := range fleet {
		totalVRAM += wk.TotalVRAM
		freeVRAM += wk.FreeVRAM
		loadSum += uint64(wk.Load)
		if wk.Load > maxLoad {
			maxLoad = wk.Load
		}

		row, ok := byType[wk.GPUType]
		if !ok {
			row = &adminGPUByType{GPUType: wk.GPUType}
			byType[wk.GPUType] = row
		}
		row.WorkerCount++
		row.TotalVRAM += wk.TotalVRAM
		row.FreeVRAM += wk.FreeVRAM

		cells = append(cells, adminGPUCell{
			ID:         wk.ID,
			GPUType:    wk.GPUType,
			TotalVRAM:  wk.TotalVRAM,
			FreeVRAM:   wk.FreeVRAM,
			Load:       wk.Load,
			Status:     wk.Status.String(),
			ActiveJobs: wk.ActiveJobs,
		})
	}

	var meanLoad uint32
	if len(fleet) > 0 {
		// Integer mean over the worker count; guarded so an empty fleet stays 0.
		meanLoad = uint32(loadSum / uint64(len(fleet)))
	}

	// Materialize the by-type rows in a deterministic order (sorted by gpu_type).
	byTypeRows := make([]adminGPUByType, 0, len(byType))
	for _, row := range byType {
		byTypeRows = append(byTypeRows, *row)
	}
	sort.Slice(byTypeRows, func(i, j int) bool { return byTypeRows[i].GPUType < byTypeRows[j].GPUType })

	// Cells sorted by worker id for a stable heatmap layout across calls.
	sort.Slice(cells, func(i, j int) bool { return cells[i].ID < cells[j].ID })

	return adminGPUsResponse{
		Fleet: adminGPUFleet{
			WorkerCount: len(fleet),
			TotalVRAM:   totalVRAM,
			FreeVRAM:    freeVRAM,
			MeanLoad:    meanLoad,
			MaxLoad:     maxLoad,
		},
		ByType:  byTypeRows,
		Workers: cells,
	}
}
