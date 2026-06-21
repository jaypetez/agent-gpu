package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// gpusResponse mirrors the GET /v1/admin/gpus wire shape for decoding in tests.
// It is local to the test so a drift in the handler's field tags is caught here.
type gpusResponse struct {
	Fleet struct {
		WorkerCount int    `json:"worker_count"`
		TotalVRAM   uint64 `json:"total_vram"`
		FreeVRAM    uint64 `json:"free_vram"`
		MeanLoad    uint32 `json:"mean_load"`
		MaxLoad     uint32 `json:"max_load"`
	} `json:"fleet"`
	ByType []struct {
		GPUType     string `json:"gpu_type"`
		WorkerCount int    `json:"worker_count"`
		TotalVRAM   uint64 `json:"total_vram"`
		FreeVRAM    uint64 `json:"free_vram"`
	} `json:"by_type"`
	Workers []struct {
		ID         string `json:"id"`
		GPUType    string `json:"gpu_type"`
		TotalVRAM  uint64 `json:"total_vram"`
		FreeVRAM   uint64 `json:"free_vram"`
		Load       uint32 `json:"load"`
		Status     string `json:"status"`
		ActiveJobs uint32 `json:"active_jobs"`
	} `json:"workers"`
}

// TestAdminGPUsAggregation is the table-driven proof of the aggregation (#94 AC1,
// AC2): for a range of fleet snapshots, GET /v1/admin/gpus returns the correct
// fleet roll-up (summed VRAM, mean/max load with the empty-fleet divide-by-zero
// guard), the correct by-type grouping (sorted by gpu_type, CPU-only workers
// included under "cpu"), and the per-worker cells (sorted by id). All values come
// only from the heartbeat capacity fields the fake fleet carries.
func TestAdminGPUsAggregation(t *testing.T) {
	const gb = uint64(1) << 30

	cases := []struct {
		name    string
		workers []types.Worker
		// wantFleet is the expected roll-up.
		wantWorkerCount     int
		wantTotalVRAM       uint64
		wantFreeVRAM        uint64
		wantMeanLoad        uint32
		wantMaxLoad         uint32
		wantByType          []string // expected gpu_type values, in the order returned
		wantByTypeCounts    []int    // worker_count per by_type row, aligned with wantByType
		wantByTypeTotalVRAM []uint64 // total_vram per by_type row, aligned with wantByType
		wantWorkerOrder     []string // expected cell ids, in the order returned
	}{
		{
			name:    "empty fleet yields zeros and empty arrays",
			workers: nil,
			// All aggregates zero; arrays must be present and empty (asserted via
			// the raw body below, not just len()).
			wantWorkerCount:  0,
			wantByType:       nil,
			wantByTypeCounts: nil,
			wantWorkerOrder:  nil,
		},
		{
			name: "multi-worker fleet sums, means, and groups by type",
			// Two RTX-4090 workers and one A100 worker. Loads 60/20/40 -> mean 40,
			// max 60. Deliberately listed out of id order to prove the cell sort.
			workers: []types.Worker{
				{ID: "w-c", GPUType: "1x NVIDIA A100", TotalVRAM: 80 * gb, FreeVRAM: 40 * gb, Load: 40, Status: types.WorkerOnline, ActiveJobs: 2},
				{ID: "w-a", GPUType: "2x NVIDIA GeForce RTX 4090", TotalVRAM: 48 * gb, FreeVRAM: 30 * gb, Load: 60, Status: types.WorkerOnline, ActiveJobs: 3},
				{ID: "w-b", GPUType: "2x NVIDIA GeForce RTX 4090", TotalVRAM: 48 * gb, FreeVRAM: 24 * gb, Load: 20, Status: types.WorkerDraining, ActiveJobs: 1},
			},
			wantWorkerCount: 3,
			wantTotalVRAM:   (80 + 48 + 48) * gb,
			wantFreeVRAM:    (40 + 30 + 24) * gb,
			wantMeanLoad:    40, // (40+60+20)/3
			wantMaxLoad:     60,
			// Sorted by gpu_type: "1x NVIDIA A100" < "2x NVIDIA GeForce RTX 4090".
			wantByType:          []string{"1x NVIDIA A100", "2x NVIDIA GeForce RTX 4090"},
			wantByTypeCounts:    []int{1, 2},
			wantByTypeTotalVRAM: []uint64{80 * gb, 96 * gb},
			wantWorkerOrder:     []string{"w-a", "w-b", "w-c"},
		},
		{
			name: "cpu-only worker is included with zero VRAM",
			workers: []types.Worker{
				{ID: "gpu-1", GPUType: "1x NVIDIA A100", TotalVRAM: 80 * gb, FreeVRAM: 80 * gb, Load: 0, Status: types.WorkerOnline},
				{ID: "cpu-1", GPUType: "cpu", TotalVRAM: 0, FreeVRAM: 0, Load: 10, Status: types.WorkerOnline},
			},
			wantWorkerCount: 2,
			wantTotalVRAM:   80 * gb,
			wantFreeVRAM:    80 * gb,
			wantMeanLoad:    5, // (0+10)/2
			wantMaxLoad:     10,
			// "1x NVIDIA A100" sorts before "cpu" (uppercase '1' < lowercase 'c').
			wantByType:          []string{"1x NVIDIA A100", "cpu"},
			wantByTypeCounts:    []int{1, 1},
			wantByTypeTotalVRAM: []uint64{80 * gb, 0},
			wantWorkerOrder:     []string{"cpu-1", "gpu-1"},
		},
		{
			name: "integer mean truncates",
			// Loads 10 and 25 -> mean 17 (35/2 truncated), max 25.
			workers: []types.Worker{
				{ID: "a", GPUType: "gpu", TotalVRAM: gb, FreeVRAM: gb, Load: 10, Status: types.WorkerOnline},
				{ID: "b", GPUType: "gpu", TotalVRAM: gb, FreeVRAM: gb, Load: 25, Status: types.WorkerOnline},
			},
			wantWorkerCount:     2,
			wantTotalVRAM:       2 * gb,
			wantFreeVRAM:        2 * gb,
			wantMeanLoad:        17,
			wantMaxLoad:         25,
			wantByType:          []string{"gpu"},
			wantByTypeCounts:    []int{2},
			wantByTypeTotalVRAM: []uint64{2 * gb},
			wantWorkerOrder:     []string{"a", "b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, authSvc := adminTestServer(t, &fakeFleet{snapshot: tc.workers})
			token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})

			rec := req(t, s, http.MethodGet, "/v1/admin/gpus", token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}

			// Empty arrays must serialize as [] (never null) so clients can iterate.
			body := rec.Body.String()
			if tc.wantWorkerCount == 0 {
				for _, field := range []string{`"by_type":[]`, `"workers":[]`} {
					if !strings.Contains(body, field) {
						t.Errorf("empty-fleet body missing %s: %s", field, body)
					}
				}
			}

			var got gpusResponse
			decode(t, rec, &got)

			if got.Fleet.WorkerCount != tc.wantWorkerCount {
				t.Errorf("worker_count = %d, want %d", got.Fleet.WorkerCount, tc.wantWorkerCount)
			}
			if got.Fleet.TotalVRAM != tc.wantTotalVRAM {
				t.Errorf("total_vram = %d, want %d", got.Fleet.TotalVRAM, tc.wantTotalVRAM)
			}
			if got.Fleet.FreeVRAM != tc.wantFreeVRAM {
				t.Errorf("free_vram = %d, want %d", got.Fleet.FreeVRAM, tc.wantFreeVRAM)
			}
			if got.Fleet.MeanLoad != tc.wantMeanLoad {
				t.Errorf("mean_load = %d, want %d", got.Fleet.MeanLoad, tc.wantMeanLoad)
			}
			if got.Fleet.MaxLoad != tc.wantMaxLoad {
				t.Errorf("max_load = %d, want %d", got.Fleet.MaxLoad, tc.wantMaxLoad)
			}

			// by_type: count, order, and per-row aggregates.
			if len(got.ByType) != len(tc.wantByType) {
				t.Fatalf("by_type len = %d, want %d: %+v", len(got.ByType), len(tc.wantByType), got.ByType)
			}
			for i, wantType := range tc.wantByType {
				if got.ByType[i].GPUType != wantType {
					t.Errorf("by_type[%d].gpu_type = %q, want %q (order must be sorted)", i, got.ByType[i].GPUType, wantType)
				}
				if got.ByType[i].WorkerCount != tc.wantByTypeCounts[i] {
					t.Errorf("by_type[%d].worker_count = %d, want %d", i, got.ByType[i].WorkerCount, tc.wantByTypeCounts[i])
				}
				if got.ByType[i].TotalVRAM != tc.wantByTypeTotalVRAM[i] {
					t.Errorf("by_type[%d].total_vram = %d, want %d", i, got.ByType[i].TotalVRAM, tc.wantByTypeTotalVRAM[i])
				}
			}

			// workers (cells): id order must be sorted, and the cell fields must echo
			// the source worker's capacity fields.
			if len(got.Workers) != len(tc.wantWorkerOrder) {
				t.Fatalf("workers len = %d, want %d: %+v", len(got.Workers), len(tc.wantWorkerOrder), got.Workers)
			}
			for i, wantID := range tc.wantWorkerOrder {
				if got.Workers[i].ID != wantID {
					t.Errorf("workers[%d].id = %q, want %q (order must be sorted by id)", i, got.Workers[i].ID, wantID)
				}
			}
		})
	}
}

// TestAdminGPUsCellFidelity proves each per-worker cell carries the exact capacity
// fields of its source worker (status string, active_jobs, VRAM, load, gpu_type),
// so a heatmap renders the real per-worker data — not just that the counts line up.
func TestAdminGPUsCellFidelity(t *testing.T) {
	const gb = uint64(1) << 30
	w := types.Worker{
		ID: "w1", GPUType: "1x NVIDIA A100", TotalVRAM: 80 * gb, FreeVRAM: 12 * gb,
		Load: 85, Status: types.WorkerDraining, ActiveJobs: 7,
	}
	s, authSvc := adminTestServer(t, &fakeFleet{snapshot: []types.Worker{w}})
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})

	rec := req(t, s, http.MethodGet, "/v1/admin/gpus", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got gpusResponse
	decode(t, rec, &got)

	if len(got.Workers) != 1 {
		t.Fatalf("workers len = %d, want 1", len(got.Workers))
	}
	cell := got.Workers[0]
	if cell.ID != "w1" || cell.GPUType != "1x NVIDIA A100" || cell.TotalVRAM != 80*gb ||
		cell.FreeVRAM != 12*gb || cell.Load != 85 || cell.Status != "draining" || cell.ActiveJobs != 7 {
		t.Errorf("cell does not echo source worker: %+v", cell)
	}
}

// TestAdminGPUsScopeGate proves the workers:read gate (#94 AC3): a key holding
// only workers:read gets 200, a key with a different scope (and no admin role)
// gets 403, and an unauthenticated request gets 401. The broad scope matrix lives
// in TestScopedKeyMatrix (which now includes this route); this focuses the gate.
func TestAdminGPUsScopeGate(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc := adminTestServer(t, fleet)

	workersReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	otherScope := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})

	// workers:read passes.
	if rec := req(t, s, http.MethodGet, "/v1/admin/gpus", workersReader, ""); rec.Code != http.StatusOK {
		t.Errorf("workers:read status = %d, want 200", rec.Code)
	}
	// A different (insufficient) scope is 403.
	rec := req(t, s, http.MethodGet, "/v1/admin/gpus", otherScope, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("keys:read-only status = %d, want 403", rec.Code)
	} else if code := errorCode(t, rec); code != "forbidden" {
		t.Errorf("403 error code = %q, want forbidden", code)
	}
	// Unauthenticated is 401.
	if rec := req(t, s, http.MethodGet, "/v1/admin/gpus", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
}
