package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Tests for the per-worker management client methods added in #93: WorkerDetail,
// DrainWorker (with the optional deadline), PullModel, and UnloadModel. They use
// the shared recordingHandler so each asserts the exact request the client sends
// and the response/error mapping, against an in-process httptest server.

func TestWorkerDetail(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK,
		`{"id":"w1","models":["llama3","mistral"],"status":"online","draining":false,"active_jobs":2,"total_vram":100,"free_vram":40,"load":42,"gpu_type":"a100","last_seen":1000,"registered_at":900,"uptime_seconds":100}`))
	defer srv.Close()

	det, err := newTestClient(t, srv).WorkerDetail(context.Background(), "w1")
	if err != nil {
		t.Fatalf("WorkerDetail: %v", err)
	}
	if det.ID != "w1" || det.Status != "online" || det.Load != 42 || det.GPUType != "a100" {
		t.Fatalf("unexpected detail: %+v", det)
	}
	if len(det.Models) != 2 || det.Models[1] != "mistral" {
		t.Fatalf("detail models = %+v", det.Models)
	}
	if det.UptimeSeconds != 100 || det.RegisteredAt != 900 {
		t.Fatalf("detail uptime/registered = %d/%d, want 100/900", det.UptimeSeconds, det.RegisteredAt)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/admin/workers/w1" {
		t.Fatalf("sent %s %s, want GET /v1/admin/workers/w1", cap.method, cap.path)
	}
}

func TestWorkerDetailNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"worker not found","code":"not_found"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).WorkerDetail(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("WorkerDetail(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestDrainWorkerSoft(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	defer srv.Close()

	// A zero deadline is the pure soft drain: no body is sent.
	if err := newTestClient(t, srv).DrainWorker(context.Background(), "w1", 0); err != nil {
		t.Fatalf("DrainWorker soft: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/admin/workers/w1/drain" {
		t.Fatalf("sent %s %s, want POST /v1/admin/workers/w1/drain", cap.method, cap.path)
	}
	if cap.body != nil {
		t.Fatalf("soft drain should send no body, got %+v", cap.body)
	}
}

func TestDrainWorkerTimed(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	defer srv.Close()

	if err := newTestClient(t, srv).DrainWorker(context.Background(), "w1", 30*time.Second); err != nil {
		t.Fatalf("DrainWorker timed: %v", err)
	}
	// The deadline is sent as whole seconds in the body.
	if cap.body == nil || toFloat(cap.body["deadline_seconds"]) != 30 {
		t.Fatalf("timed drain body = %+v, want deadline_seconds=30", cap.body)
	}

	// A sub-second positive deadline rounds up to 1s so it still requests a forced
	// drain rather than degrading to a soft one.
	cap = capture{}
	if err := newTestClient(t, srv).DrainWorker(context.Background(), "w1", 200*time.Millisecond); err != nil {
		t.Fatalf("DrainWorker sub-second: %v", err)
	}
	if cap.body == nil || toFloat(cap.body["deadline_seconds"]) != 1 {
		t.Fatalf("sub-second drain body = %+v, want deadline_seconds=1", cap.body)
	}
}

func TestDrainWorkerNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"worker not found","code":"not_found"}}`)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv).DrainWorker(context.Background(), "ghost", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DrainWorker(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestPullModel(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusAccepted, ""))
	defer srv.Close()

	if err := newTestClient(t, srv).PullModel(context.Background(), "w1", "llama3"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/admin/workers/w1/models" {
		t.Fatalf("sent %s %s, want POST /v1/admin/workers/w1/models", cap.method, cap.path)
	}
	if cap.body["model"] != "llama3" {
		t.Fatalf("pull body model = %v, want llama3", cap.body["model"])
	}
}

func TestPullModelNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"worker not found","code":"not_found"}}`)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv).PullModel(context.Background(), "ghost", "llama3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PullModel(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestUnloadModel(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	defer srv.Close()

	if err := newTestClient(t, srv).UnloadModel(context.Background(), "w1", "llama3"); err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != "/v1/admin/workers/w1/models/llama3" {
		t.Fatalf("sent %s %s, want DELETE /v1/admin/workers/w1/models/llama3", cap.method, cap.path)
	}
}

// TestUnloadModelNamespacedPath proves a namespaced model id keeps its embedded
// slash as a literal path separator (so the server's multi-segment wildcard
// captures it whole) while a colon tag is preserved.
func TestUnloadModelNamespacedPath(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	defer srv.Close()

	if err := newTestClient(t, srv).UnloadModel(context.Background(), "w1", "library/qwen2:0.5b"); err != nil {
		t.Fatalf("UnloadModel namespaced: %v", err)
	}
	if cap.path != "/v1/admin/workers/w1/models/library/qwen2:0.5b" {
		t.Fatalf("namespaced path = %q, want .../models/library/qwen2:0.5b", cap.path)
	}
}

// toFloat coerces a JSON-decoded number into float64 for assertions.
func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return -1
}
