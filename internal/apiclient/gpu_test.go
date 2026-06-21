package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFleetCapacity proves the client decodes the GET /v1/admin/gpus aggregate
// into the typed FleetCapacity and sends the request to the right path. It uses
// the shared recordingHandler so the exact request the client makes is asserted
// alongside the decoded response.
func TestFleetCapacity(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, `{
		"fleet":{"worker_count":2,"total_vram":128,"free_vram":70,"mean_load":36,"max_load":60},
		"by_type":[
			{"gpu_type":"2x NVIDIA GeForce RTX 4090","worker_count":1,"total_vram":100,"free_vram":60},
			{"gpu_type":"cpu","worker_count":1,"total_vram":28,"free_vram":10}
		],
		"workers":[
			{"id":"worker-a","gpu_type":"2x NVIDIA GeForce RTX 4090","total_vram":100,"free_vram":60,"load":60,"status":"online","active_jobs":3},
			{"id":"worker-b","gpu_type":"cpu","total_vram":28,"free_vram":10,"load":12,"status":"draining","active_jobs":0}
		]
	}`))
	defer srv.Close()

	got, err := newTestClient(t, srv).FleetCapacity(context.Background())
	if err != nil {
		t.Fatalf("FleetCapacity: %v", err)
	}

	if cap.method != http.MethodGet || cap.path != "/v1/admin/gpus" {
		t.Fatalf("sent %s %s, want GET /v1/admin/gpus", cap.method, cap.path)
	}

	if got.Fleet.WorkerCount != 2 || got.Fleet.TotalVRAM != 128 || got.Fleet.FreeVRAM != 70 ||
		got.Fleet.MeanLoad != 36 || got.Fleet.MaxLoad != 60 {
		t.Errorf("fleet roll-up wrong: %+v", got.Fleet)
	}

	if len(got.ByType) != 2 {
		t.Fatalf("by_type len = %d, want 2", len(got.ByType))
	}
	if got.ByType[0].GPUType != "2x NVIDIA GeForce RTX 4090" || got.ByType[0].WorkerCount != 1 ||
		got.ByType[0].TotalVRAM != 100 || got.ByType[0].FreeVRAM != 60 {
		t.Errorf("by_type[0] wrong: %+v", got.ByType[0])
	}
	if got.ByType[1].GPUType != "cpu" || got.ByType[1].WorkerCount != 1 {
		t.Errorf("by_type[1] wrong: %+v", got.ByType[1])
	}

	if len(got.Workers) != 2 {
		t.Fatalf("workers len = %d, want 2", len(got.Workers))
	}
	cell := got.Workers[0]
	if cell.ID != "worker-a" || cell.GPUType != "2x NVIDIA GeForce RTX 4090" || cell.TotalVRAM != 100 ||
		cell.FreeVRAM != 60 || cell.Load != 60 || cell.Status != "online" || cell.ActiveJobs != 3 {
		t.Errorf("workers[0] cell wrong: %+v", cell)
	}
}

// TestFleetCapacityEmpty proves an empty fleet decodes cleanly: zero aggregates
// and empty (non-nil) slices, so a caller can range over them without a guard.
func TestFleetCapacityEmpty(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK,
		`{"fleet":{"worker_count":0,"total_vram":0,"free_vram":0,"mean_load":0,"max_load":0},"by_type":[],"workers":[]}`))
	defer srv.Close()

	got, err := newTestClient(t, srv).FleetCapacity(context.Background())
	if err != nil {
		t.Fatalf("FleetCapacity: %v", err)
	}
	if got.Fleet.WorkerCount != 0 {
		t.Errorf("worker_count = %d, want 0", got.Fleet.WorkerCount)
	}
	if got.ByType == nil || len(got.ByType) != 0 {
		t.Errorf("by_type = %+v, want empty non-nil slice", got.ByType)
	}
	if got.Workers == nil || len(got.Workers) != 0 {
		t.Errorf("workers = %+v, want empty non-nil slice", got.Workers)
	}
}

// TestFleetCapacityForbidden proves the client maps a 403 (a token lacking
// workers:read) to the typed ErrForbidden sentinel.
func TestFleetCapacityForbidden(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient scope","code":"forbidden"}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).FleetCapacity(context.Background()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("FleetCapacity err = %v, want ErrForbidden", err)
	}
}
