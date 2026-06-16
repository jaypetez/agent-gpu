package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// adminTestServer builds an httpapi.Server wired to the fake fleet, a real auth
// service + authorizer + (unlimited) quota engine over an in-memory store, and a
// discarding logger. It mirrors testServer but also sets the quota engine so the
// GET .../quota usage endpoint is exercised, and returns the fleet so a test can
// configure DrainWorker behaviour.
func adminTestServer(t *testing.T, fleet *fakeFleet) (*Server, *auth.Service) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	s := &Server{
		fleet: fleet,
		auth:  authSvc,
		authz: az,
		quota: quota.NewEngine(quota.NewMemoryCounterStore()),
		log:   discard,
	}
	return s, authSvc
}

// req issues an authenticated request with an optional JSON body through the
// routed handler and returns the recorder. An empty token sends no
// Authorization header.
func req(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// adminRoute names one admin endpoint for the table-driven authz tests.
type adminRoute struct {
	method string
	path   string
	body   string
}

// allAdminRoutes is every admin endpoint, used to prove the authz gate is
// applied uniformly (non-admin → 403, unauth → 401 on every one). Paths use a
// placeholder id; the gate runs before any lookup, so the id need not exist.
func allAdminRoutes() []adminRoute {
	return []adminRoute{
		{http.MethodPost, "/v1/admin/keys", `{"name":"x"}`},
		{http.MethodGet, "/v1/admin/keys", ""},
		{http.MethodGet, "/v1/admin/keys/abc", ""},
		{http.MethodDelete, "/v1/admin/keys/abc", ""},
		{http.MethodPost, "/v1/admin/keys/abc/rotate", ""},
		{http.MethodPut, "/v1/admin/keys/abc/permissions", `{"roles":["user"]}`},
		{http.MethodPut, "/v1/admin/keys/abc/quota", `{"rpm":1}`},
		{http.MethodGet, "/v1/admin/keys/abc/quota", ""},
		{http.MethodGet, "/v1/admin/workers", ""},
		{http.MethodPost, "/v1/admin/workers/abc/drain", ""},
		{http.MethodGet, "/v1/admin/stats", ""},
	}
}

// TestAdminAuthzNonAdminForbidden proves AC1: a valid but non-admin key receives
// 403 on EVERY admin endpoint (and the error envelope carries code "forbidden").
func TestAdminAuthzNonAdminForbidden(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	// A key with the "user" role only — authenticated, but not admin.
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})

	for _, rt := range allAdminRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, userToken, rt.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if code := errorCode(t, rec); code != "forbidden" {
				t.Errorf("error code = %q, want forbidden", code)
			}
		})
	}
}

// TestAdminAuthzUnauthenticated proves AC1: a request with no/invalid bearer
// token receives 401 on EVERY admin endpoint.
func TestAdminAuthzUnauthenticated(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})

	for _, rt := range allAdminRoutes() {
		t.Run("missing "+rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, "", rt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("no-token status = %d, want 401", rec.Code)
			}
		})
		t.Run("invalid "+rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, "agpu_bogus_token", rt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("bad-token status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestAdminKeyLifecycle proves AC2: an admin key can create, list, inspect,
// rotate, set permissions/quota on, and revoke a key, with consistent shapes and
// the plaintext token returned ONLY on create and rotate.
func TestAdminKeyLifecycle(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	// Create.
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"svc","roles":["user"],"allow_models":["llama3"],"deny_models":["secret"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Token       string   `json:"token"`
		Roles       []string `json:"roles"`
		AllowModels []string `json:"allow_models"`
		DenyModels  []string `json:"deny_models"`
		Created     int64    `json:"created"`
	}
	decode(t, rec, &created)
	if created.ID == "" || created.Name != "svc" {
		t.Fatalf("create response wrong: %+v", created)
	}
	if !strings.HasPrefix(created.Token, "agpu_") {
		t.Fatalf("create did not return a plaintext token: %q", created.Token)
	}
	if len(created.Roles) != 1 || created.Roles[0] != "user" || created.AllowModels[0] != "llama3" {
		t.Errorf("create permissions wrong: %+v", created)
	}
	id := created.ID

	// List: the new key appears, never with a secret.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	assertNoSecret(t, rec.Body.String())
	var list struct {
		Keys []map[string]any `json:"keys"`
	}
	decode(t, rec, &list)
	if !containsKeyID(list.Keys, id) {
		t.Fatalf("list does not contain created key %s: %+v", id, list.Keys)
	}

	// Inspect one.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+id, adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	assertNoSecret(t, rec.Body.String())
	var got map[string]any
	decode(t, rec, &got)
	if got["id"] != id || got["revoked"] != false {
		t.Errorf("get response wrong: %+v", got)
	}

	// Set permissions (full replace): clear allow-list, set deny.
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/permissions", adminToken,
		`{"roles":["read-only"],"deny_models":["llama3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set permissions status = %d, want 200", rec.Code)
	}
	updated, err := authSvc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get updated key: %v", err)
	}
	if len(updated.Roles) != 1 || updated.Roles[0] != "read-only" ||
		len(updated.AllowModels) != 0 || len(updated.DenyModels) != 1 || updated.DenyModels[0] != "llama3" {
		t.Errorf("permissions not replaced: %+v", updated)
	}

	// Set quota: RPM=5, others default to 0 (unlimited).
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/quota", adminToken, `{"rpm":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set quota status = %d, want 200", rec.Code)
	}
	updated, _ = authSvc.Get(context.Background(), id)
	if updated.Limits == nil || updated.Limits.RPM != 5 {
		t.Fatalf("limits not set: %+v", updated.Limits)
	}

	// Clearing the override (empty body) drops Limits back to nil (global default).
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/quota", adminToken, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear quota status = %d, want 200", rec.Code)
	}
	updated, _ = authSvc.Get(context.Background(), id)
	if updated.Limits != nil {
		t.Errorf("limits not cleared: %+v", updated.Limits)
	}

	// Usage snapshot.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+id+"/quota", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota status = %d, want 200", rec.Code)
	}
	var usage struct {
		KeyID  string `json:"key_id"`
		Limits struct {
			RPM uint64 `json:"rpm"`
		} `json:"limits"`
	}
	decode(t, rec, &usage)
	if usage.KeyID != id {
		t.Errorf("usage key_id = %q, want %q", usage.KeyID, id)
	}

	// Rotate: returns a fresh one-time token, distinct from create's.
	rec = req(t, s, http.MethodPost, "/v1/admin/keys/"+id+"/rotate", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200", rec.Code)
	}
	var rotated struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decode(t, rec, &rotated)
	if rotated.ID != id || !strings.HasPrefix(rotated.Token, "agpu_") || rotated.Token == created.Token {
		t.Fatalf("rotate response wrong: %+v", rotated)
	}

	// Revoke: 204, and the key now reads back revoked.
	rec = req(t, s, http.MethodDelete, "/v1/admin/keys/"+id, adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", rec.Code)
	}
	revoked, _ := authSvc.Get(context.Background(), id)
	if !revoked.Revoked() {
		t.Errorf("key not revoked after DELETE")
	}
}

// TestAdminUnknownKey404 proves AC2 error consistency: every {id} operation on a
// key that does not exist returns 404 with code "not_found".
func TestAdminUnknownKey404(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	cases := []adminRoute{
		{http.MethodGet, "/v1/admin/keys/nope", ""},
		{http.MethodDelete, "/v1/admin/keys/nope", ""},
		{http.MethodPost, "/v1/admin/keys/nope/rotate", ""},
		{http.MethodPut, "/v1/admin/keys/nope/permissions", `{"roles":["user"]}`},
		{http.MethodPut, "/v1/admin/keys/nope/quota", `{"rpm":1}`},
		{http.MethodGet, "/v1/admin/keys/nope/quota", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := req(t, s, c.method, c.path, adminToken, c.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if code := errorCode(t, rec); code != "not_found" {
				t.Errorf("error code = %q, want not_found", code)
			}
		})
	}
}

// TestAdminMalformedBody400 proves AC2 error consistency: a malformed JSON body
// on a write endpoint yields 400 with code "invalid_request_error".
func TestAdminMalformedBody400(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	for _, path := range []string{
		"/v1/admin/keys",
		"/v1/admin/keys/abc/permissions",
		"/v1/admin/keys/abc/quota",
	} {
		method := http.MethodPost
		if strings.Contains(path, "/abc/") {
			method = http.MethodPut
		}
		t.Run(path, func(t *testing.T) {
			rec := req(t, s, method, path, adminToken, `{not json`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if code := errorCode(t, rec); code != "invalid_request_error" {
				t.Errorf("error code = %q, want invalid_request_error", code)
			}
		})
	}
}

// TestAdminWorkers proves AC2 for the worker resource: list returns the fleet
// snapshot projection, drain returns 204 and forwards the id, and a drain on an
// unknown worker maps ErrWorkerNotFound → 404.
func TestAdminWorkers(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		{ID: "w1", Models: []types.Model{{Name: "llama3"}}, Status: types.WorkerOnline, ActiveJobs: 2, Load: 50, GPUType: "a100"},
	}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	// List.
	rec := req(t, s, http.MethodGet, "/v1/admin/workers", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list workers status = %d, want 200", rec.Code)
	}
	var out struct {
		Workers []struct {
			ID         string   `json:"id"`
			Models     []string `json:"models"`
			Status     string   `json:"status"`
			ActiveJobs uint32   `json:"active_jobs"`
			GPUType    string   `json:"gpu_type"`
		} `json:"workers"`
	}
	decode(t, rec, &out)
	if len(out.Workers) != 1 || out.Workers[0].ID != "w1" || out.Workers[0].Status != "online" ||
		out.Workers[0].ActiveJobs != 2 || out.Workers[0].GPUType != "a100" ||
		len(out.Workers[0].Models) != 1 || out.Workers[0].Models[0] != "llama3" {
		t.Fatalf("worker projection wrong: %+v", out.Workers)
	}

	// Drain a known worker → 204, id forwarded to the control plane.
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("drain status = %d, want 204", rec.Code)
	}
	if fleet.drained != "w1" {
		t.Errorf("DrainWorker called with %q, want w1", fleet.drained)
	}

	// Drain an unknown worker → 404.
	fleet.drainErr = server.ErrWorkerNotFound
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/ghost/drain", adminToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("drain unknown status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_found" {
		t.Errorf("error code = %q, want not_found", code)
	}
}

// TestAdminStats proves AC1/AC4 for the consolidated monitoring endpoint (#10):
// GET /v1/admin/stats returns the documented shape — queue depth (total +
// per-priority), per-worker load, and the time-in-queue distribution
// (count/sum/max/mean + cumulative buckets) — built live from QueueStats(),
// Fleet(), and WaitTimeStats(). Admin gating (non-admin 403, unauth 401) is
// proven for this route by the table-driven authz tests above.
func TestAdminStats(t *testing.T) {
	fleet := &fakeFleet{
		snapshot: []types.Worker{
			{ID: "w1", Status: types.WorkerOnline, ActiveJobs: 2, Load: 50},
			{ID: "w2", Status: types.WorkerDraining, ActiveJobs: 0, Load: 10},
		},
		queueStats: queue.Stats{
			Total:      3,
			ByPriority: map[queue.Priority]int{queue.PriorityHigh: 1, queue.PriorityNormal: 2},
		},
		waitStats: server.WaitTimeStats{
			Count: 2,
			SumMs: 150,
			MaxMs: 120,
			Buckets: []server.WaitBucket{
				{LeMs: 10, Count: 0},
				{LeMs: 100, Count: 1},
				{LeMs: 1000, Count: 2},
				{LeMs: 10000, Count: 2},
				{LeMs: 0, Count: 2}, // +Inf
			},
		},
	}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/stats", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}

	var out struct {
		Queue struct {
			Total      int            `json:"total"`
			ByPriority map[string]int `json:"by_priority"`
		} `json:"queue"`
		Workers []struct {
			ID         string `json:"id"`
			ActiveJobs uint32 `json:"active_jobs"`
			Load       uint32 `json:"load"`
			Status     string `json:"status"`
		} `json:"workers"`
		WaitTime struct {
			Count   uint64 `json:"count"`
			SumMs   uint64 `json:"sum_ms"`
			MaxMs   uint64 `json:"max_ms"`
			MeanMs  uint64 `json:"mean_ms"`
			Buckets []struct {
				LeMs  uint64 `json:"le_ms"`
				Count uint64 `json:"count"`
			} `json:"buckets"`
		} `json:"wait_time"`
	}
	decode(t, rec, &out)

	// Queue depth: total plus the per-priority breakdown keyed by name.
	if out.Queue.Total != 3 {
		t.Errorf("queue total = %d, want 3", out.Queue.Total)
	}
	if out.Queue.ByPriority["high"] != 1 || out.Queue.ByPriority["normal"] != 2 {
		t.Errorf("by_priority = %v, want high:1 normal:2", out.Queue.ByPriority)
	}

	// Per-worker load projection (id/active_jobs/load/status), order-preserving.
	if len(out.Workers) != 2 {
		t.Fatalf("workers len = %d, want 2", len(out.Workers))
	}
	if out.Workers[0].ID != "w1" || out.Workers[0].ActiveJobs != 2 ||
		out.Workers[0].Load != 50 || out.Workers[0].Status != "online" {
		t.Errorf("worker[0] = %+v, want w1 active=2 load=50 online", out.Workers[0])
	}
	if out.Workers[1].ID != "w2" || out.Workers[1].Status != "draining" {
		t.Errorf("worker[1] = %+v, want w2 draining", out.Workers[1])
	}

	// Wait-time: summary + computed mean + cumulative buckets (incl. +Inf).
	if out.WaitTime.Count != 2 || out.WaitTime.SumMs != 150 || out.WaitTime.MaxMs != 120 {
		t.Errorf("wait_time summary = %+v, want count=2 sum=150 max=120", out.WaitTime)
	}
	if out.WaitTime.MeanMs != 75 { // 150 / 2
		t.Errorf("wait_time mean = %d, want 75", out.WaitTime.MeanMs)
	}
	if len(out.WaitTime.Buckets) != 5 {
		t.Fatalf("buckets len = %d, want 5 (4 bounds + Inf)", len(out.WaitTime.Buckets))
	}
	last := out.WaitTime.Buckets[len(out.WaitTime.Buckets)-1]
	if last.LeMs != 0 || last.Count != 2 {
		t.Errorf("+Inf bucket = %+v, want le_ms=0 count=2", last)
	}
}

// TestAdminStatsEmpty proves the endpoint is well-formed against a fresh server:
// an empty queue, no workers, and no recorded waits yield zeroed sections with a
// zero mean (no divide-by-zero) and a non-null buckets array.
func TestAdminStatsEmpty(t *testing.T) {
	// A real server.New() exercises the genuine WaitTimeStats()/QueueStats()
	// zero state (the +Inf bucket present, mean guarded at Count==0).
	grpcSrv := server.New()
	defer func() { _ = grpcSrv.Close() }()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		fleet: grpcSrv,
		auth:  authSvc,
		authz: authz.NewAuthorizer(authz.WithLogger(discard)),
		quota: quota.NewEngine(quota.NewMemoryCounterStore()),
		log:   discard,
	}
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/stats", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}
	var out struct {
		Queue struct {
			Total int `json:"total"`
		} `json:"queue"`
		Workers  []any `json:"workers"`
		WaitTime struct {
			Count   uint64 `json:"count"`
			MeanMs  uint64 `json:"mean_ms"`
			Buckets []struct {
				LeMs uint64 `json:"le_ms"`
			} `json:"buckets"`
		} `json:"wait_time"`
	}
	decode(t, rec, &out)
	if out.Queue.Total != 0 || out.WaitTime.Count != 0 || out.WaitTime.MeanMs != 0 {
		t.Errorf("empty stats not zeroed: %+v", out)
	}
	if len(out.WaitTime.Buckets) != 5 {
		t.Errorf("buckets len = %d, want 5 even when empty", len(out.WaitTime.Buckets))
	}
}

// TestAdminCreateKeyNeverLeaksSecret proves no response (create, list, get)
// includes the stored secret hash or salt fields.
func TestAdminCreateKeyNeverLeaksSecret(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"svc"}`)
	assertNoSecret(t, rec.Body.String())

	rec = req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	assertNoSecret(t, rec.Body.String())
}

// ---- helpers ----

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, rec, &body)
	return body.Error.Code
}

// assertNoSecret fails if the response body carries any field that would expose
// the stored secret material. SecretHash/Salt are []byte and would marshal as
// base64 under those JSON keys if the record were serialized directly.
func assertNoSecret(t *testing.T, body string) {
	t.Helper()
	for _, banned := range []string{"SecretHash", "secret_hash", "secrethash", "Salt", "\"salt\""} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(banned)) {
			t.Fatalf("response leaks secret material (%q): %s", banned, body)
		}
	}
}

func containsKeyID(keys []map[string]any, id string) bool {
	for _, k := range keys {
		if k["id"] == id {
			return true
		}
	}
	return false
}
