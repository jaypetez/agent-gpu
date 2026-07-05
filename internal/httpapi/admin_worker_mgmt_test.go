package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// Tests for the per-worker management surface added in #93: GET
// /v1/admin/workers/{id} (detail), POST .../models (pull), DELETE
// .../models/{model} (unload), and the timed/forced-drain extension of
// POST .../drain. Authz/unauth gating is proven uniformly by the table-driven
// tests in admin_test.go / admin_scope_test.go (which now include these routes);
// these tests focus on behavior, error mapping, the audit trail, and the wiring
// to the control plane.

// detailWorker is a fleet snapshot worker with the detail fields populated.
func detailWorker(id string) types.Worker {
	reg := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	return types.Worker{
		ID:           id,
		Models:       []types.Model{{Name: "llama3"}, {Name: "mistral"}},
		Status:       types.WorkerOnline,
		ActiveJobs:   2,
		TotalVRAM:    24 << 30,
		FreeVRAM:     16 << 30,
		Load:         42,
		GPUType:      "NVIDIA RTX 4090",
		RegisteredAt: reg,
		LastSeen:     reg.Add(90 * time.Second),
	}
}

// TestAdminGetWorkerDetail covers AC1: detail returns the rich per-worker
// projection (models, status, GPU/VRAM, load, active jobs, last_seen, uptime via
// registered_at, draining), 404 for an unknown worker, and the workers:read gate.
func TestAdminGetWorkerDetail(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("w1")}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())
	readerToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})

	// Happy path: the full projection, including the derived uptime.
	rec := req(t, s, http.MethodGet, "/v1/admin/workers/w1", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", rec.Code)
	}
	var got struct {
		ID            string   `json:"id"`
		Models        []string `json:"models"`
		Status        string   `json:"status"`
		Draining      bool     `json:"draining"`
		ActiveJobs    uint32   `json:"active_jobs"`
		TotalVRAM     uint64   `json:"total_vram"`
		FreeVRAM      uint64   `json:"free_vram"`
		Load          uint32   `json:"load"`
		GPUType       string   `json:"gpu_type"`
		LastSeen      int64    `json:"last_seen"`
		RegisteredAt  int64    `json:"registered_at"`
		UptimeSeconds int64    `json:"uptime_seconds"`
	}
	decode(t, rec, &got)
	if got.ID != "w1" || got.Status != "online" || got.Draining {
		t.Errorf("detail identity wrong: %+v", got)
	}
	if len(got.Models) != 2 || got.Models[0] != "llama3" || got.Models[1] != "mistral" {
		t.Errorf("detail models wrong: %+v", got.Models)
	}
	if got.ActiveJobs != 2 || got.Load != 42 || got.GPUType != "NVIDIA RTX 4090" ||
		got.TotalVRAM != 24<<30 || got.FreeVRAM != 16<<30 {
		t.Errorf("detail capacity wrong: %+v", got)
	}
	if got.UptimeSeconds != 90 { // last_seen - registered_at
		t.Errorf("uptime_seconds = %d, want 90", got.UptimeSeconds)
	}
	if got.RegisteredAt == 0 {
		t.Errorf("registered_at should be populated")
	}

	// A workers:read key (not admin) is granted the detail route.
	rec = req(t, s, http.MethodGet, "/v1/admin/workers/w1", readerToken, "")
	if rec.Code != http.StatusOK {
		t.Errorf("workers:read detail status = %d, want 200", rec.Code)
	}

	// Unknown worker → 404 not_found.
	rec = req(t, s, http.MethodGet, "/v1/admin/workers/ghost", adminToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worker status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_found" {
		t.Errorf("unknown worker code = %q, want not_found", code)
	}

	// A no-token request is 401 before any lookup.
	rec = req(t, s, http.MethodGet, "/v1/admin/workers/w1", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth detail status = %d, want 401", rec.Code)
	}
}

// TestAdminGetWorkerDetailDraining proves the draining boolean and status reflect
// a draining worker, and that a worker with no registration time omits uptime.
func TestAdminGetWorkerDetailDraining(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		{ID: "d1", Status: types.WorkerDraining, Models: []types.Model{{Name: "llama3"}}},
	}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/workers/d1", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", rec.Code)
	}
	var got struct {
		Status        string `json:"status"`
		Draining      bool   `json:"draining"`
		RegisteredAt  int64  `json:"registered_at"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	}
	decode(t, rec, &got)
	if got.Status != "draining" || !got.Draining {
		t.Errorf("draining detail wrong: status=%q draining=%v", got.Status, got.Draining)
	}
	// No RegisteredAt set → both registered_at and uptime_seconds omitted (0).
	if got.RegisteredAt != 0 || got.UptimeSeconds != 0 {
		t.Errorf("zero registration should omit registered_at/uptime: %+v", got)
	}
}

// TestAdminPullModel covers AC2: pull dispatches PullModel to the worker (202),
// 404 for an unknown worker, 400 for an empty model, the models:write gate, and
// an audit entry.
func TestAdminPullModel(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())
	writerToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeModelsWrite}})

	// Happy path: 202 Accepted, dispatch reaches the control plane.
	rec := req(t, s, http.MethodPost, "/v1/admin/workers/w1/models", adminToken, `{"model":"llama3"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pull status = %d, want 202", rec.Code)
	}
	if fleet.pulledWorker != "w1" || fleet.pulledModel != "llama3" {
		t.Errorf("AdminPullModel called with (%q,%q), want (w1,llama3)", fleet.pulledWorker, fleet.pulledModel)
	}

	// A models:write key (not admin) is granted the route.
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/models", writerToken, `{"model":"mistral"}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("models:write pull status = %d, want 202", rec.Code)
	}

	// Empty model → 400.
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/models", adminToken, `{"model":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty model status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_request_error" {
		t.Errorf("empty model code = %q, want invalid_request_error", code)
	}

	// Unknown worker → 404.
	fleet.pullErr = server.ErrWorkerNotFound
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/ghost/models", adminToken, `{"model":"llama3"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worker pull status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_found" {
		t.Errorf("unknown worker pull code = %q, want not_found", code)
	}

	// Audit: a successful pull and a failed pull are both recorded, and the empty
	// model 400 (rejected before dispatch) is NOT.
	entries := auditLog.List(audit.Filter{Op: auditOpModelPull}, 0)
	if len(entries) != 3 { // 2 success (admin, writer) + 1 failure (ghost)
		t.Fatalf("recorded %d model.pull audit entries, want 3", len(entries))
	}
	var success, failure int
	for _, e := range entries {
		switch e.Outcome {
		case audit.OutcomeSuccess:
			success++
			if e.Target != "w1/llama3" && e.Target != "w1/mistral" {
				t.Errorf("pull audit target = %q, want w1/<model>", e.Target)
			}
		case audit.OutcomeFailure:
			failure++
			if e.Target != "ghost/llama3" {
				t.Errorf("failed pull audit target = %q, want ghost/llama3", e.Target)
			}
		}
		if e.Actor == "" || e.RequestID == "" {
			t.Errorf("pull audit missing actor/request_id: %+v", e)
		}
	}
	if success != 2 || failure != 1 {
		t.Errorf("pull audit outcomes: success=%d failure=%d, want 2/1", success, failure)
	}
}

// TestAdminUnloadModel covers AC3: unload dispatches UnloadModel best-effort
// (204), a missing model is success (the control plane no-ops), 404 only when the
// worker is not connected, the models:write gate, and an audit entry.
func TestAdminUnloadModel(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())
	writerToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeModelsWrite}})

	// Happy path: 204, dispatch reaches the control plane.
	rec := req(t, s, http.MethodDelete, "/v1/admin/workers/w1/models/llama3", adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unload status = %d, want 204", rec.Code)
	}
	if fleet.unloadedWorker != "w1" || fleet.unloadedModel != "llama3" {
		t.Errorf("AdminUnloadModel called with (%q,%q), want (w1,llama3)", fleet.unloadedWorker, fleet.unloadedModel)
	}

	// A model that is not loaded is a worker-side no-op → still 204 (success). The
	// control plane returns nil for a connected worker regardless of residency.
	rec = req(t, s, http.MethodDelete, "/v1/admin/workers/w1/models/never-loaded", adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unload missing-model status = %d, want 204 (success)", rec.Code)
	}

	// A colon-tagged model id is captured whole by the multi-segment wildcard.
	rec = req(t, s, http.MethodDelete, "/v1/admin/workers/w1/models/qwen2:0.5b", writerToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unload colon-tag status = %d, want 204", rec.Code)
	}
	if fleet.unloadedModel != "qwen2:0.5b" {
		t.Errorf("colon-tag model = %q, want qwen2:0.5b", fleet.unloadedModel)
	}

	// Unknown worker → 404.
	fleet.unloadErr = server.ErrWorkerNotFound
	rec = req(t, s, http.MethodDelete, "/v1/admin/workers/ghost/models/llama3", adminToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worker unload status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_found" {
		t.Errorf("unknown worker unload code = %q, want not_found", code)
	}

	// Audit: 3 successes (llama3, never-loaded, qwen2:0.5b) + 1 failure (ghost).
	entries := auditLog.List(audit.Filter{Op: auditOpModelUnload}, 0)
	if len(entries) != 4 {
		t.Fatalf("recorded %d model.unload audit entries, want 4", len(entries))
	}
}

// TestAdminDrainSoftDefault proves the timed-drain extension preserves the
// original behavior: a drain with no body (and one with deadline_seconds:0) is a
// pure soft drain — DrainWorkerWithDeadline is called with a zero deadline.
func TestAdminDrainSoftDefault(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	// Empty body → soft drain (deadline 0).
	rec := req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("soft drain status = %d, want 204", rec.Code)
	}
	if fleet.drained != "w1" || fleet.drainDeadline != 0 {
		t.Errorf("soft drain: drained=%q deadline=%v, want w1/0", fleet.drained, fleet.drainDeadline)
	}

	// Explicit deadline_seconds:0 is also a soft drain.
	fleet.drained, fleet.drainDeadline = "", 99
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, `{"deadline_seconds":0}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("explicit-zero drain status = %d, want 204", rec.Code)
	}
	if fleet.drainDeadline != 0 {
		t.Errorf("explicit-zero deadline = %v, want 0", fleet.drainDeadline)
	}
}

// TestAdminDrainTimed proves a positive deadline_seconds is forwarded to the
// control plane as a duration (the timed/forced drain), is recorded in the audit
// after-snapshot, and that a negative value is rejected 400.
func TestAdminDrainTimed(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, `{"deadline_seconds":30}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("timed drain status = %d, want 204", rec.Code)
	}
	if fleet.drained != "w1" || fleet.drainDeadline != 30*time.Second {
		t.Errorf("timed drain: drained=%q deadline=%v, want w1/30s", fleet.drained, fleet.drainDeadline)
	}

	// The audit after-snapshot carries the deadline so a forced drain is
	// distinguishable from a soft one.
	entries := auditLog.List(audit.Filter{Op: auditOpWorkerDrain}, 0)
	if len(entries) != 1 {
		t.Fatalf("recorded %d drain audit entries, want 1", len(entries))
	}
	after := entries[0].After
	if after == nil || after["status"] != "draining" {
		t.Errorf("drain audit after = %+v, want status=draining", after)
	}
	// JSON round-trips the int64 deadline as a float64 in the redacted map.
	if dl, ok := after["deadline_seconds"]; !ok || toInt(dl) != 30 {
		t.Errorf("drain audit after deadline_seconds = %v, want 30", after["deadline_seconds"])
	}

	// Negative deadline → 400 before any control-plane call.
	fleet.drained = ""
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, `{"deadline_seconds":-5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative deadline status = %d, want 400", rec.Code)
	}
	if fleet.drained != "" {
		t.Errorf("negative deadline still called the control plane (drained=%q)", fleet.drained)
	}

	// Malformed body → 400.
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed drain body status = %d, want 400", rec.Code)
	}
}

// toInt coerces a JSON-decoded number (float64) or an int64 to int64.
func toInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return -1
	}
}
