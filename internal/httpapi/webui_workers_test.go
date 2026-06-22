package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// webui_workers_test.go exercises the console's Workers + GPU management surface
// (#101): the fleet page, the worker detail (and its 404), the live heatmap +
// worker-list partials, the workers:read scope gate (and the unauthenticated
// redirect), and — most importantly — every state-changing handler's CSRF + write-
// scope gate plus its in-process fleet call and audit entry. It reuses the #100
// rig (adminTestServerWithAudit, mustKey, loginAndGetSession, uiGet, the recording
// fakeFleet) and drives requests through the fully-routed s.Handler().

// uiWrite issues a state-changing UI request (POST/DELETE) through the routed
// handler the way HTMX does: the session cookie authenticates, and the CSRF token
// is sent BOTH as the agpu_csrf cookie and the X-CSRF-Token header (the double
// submit), with Sec-Fetch-Site: same-origin. A caller can omit the csrf to
// exercise the CSRF-failure path. form may be nil.
func uiWrite(t *testing.T, s *Server, method, path, session, csrf string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	}
	if csrf != "" {
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
		r.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// workersWriterToken mints a key holding both workers:write and models:write, so a
// single session can drive all four write actions in a happy-path test.
func workersWriterToken(t *testing.T, authSvc *auth.Service) string {
	t.Helper()
	return mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{
		authz.ScopeWorkersRead, authz.ScopeWorkersWrite, authz.ScopeModelsWrite,
	}})
}

// TestUIWorkersPage covers the fleet page: an authenticated workers:read viewer
// gets 200 with the page chrome and the two HTMX-polled regions wired to their
// partials.
func TestUIWorkersPage(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111"), detailWorker("worker-bbbb2222")}}
	s, authSvc := adminTestServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/workers", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/workers = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The page header and both polled regions (by id + partial URL) are present.
	for _, want := range []string{"Workers", `id="gpu-heatmap"`, `id="worker-list"`, "partials/gpu-heatmap", "partials/worker-list"} {
		if !strings.Contains(body, want) {
			t.Errorf("workers page missing %q", want)
		}
	}
}

// TestUIWorkersPageScopeGated proves the workers:read gate: a valid key without
// workers:read gets 403 (not a redirect — it is authenticated), and an
// unauthenticated request is redirected to login.
func TestUIWorkersPageScopeGated(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	// Authenticated but unscoped: a user key (inference only, no admin scope) → 403.
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	rec := uiGet(t, s, "/admin/workers", map[string]string{sessionCookieName: userToken})
	if rec.Code != http.StatusForbidden {
		t.Errorf("workers page for unscoped key = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="worker-list"`) {
		t.Error("403 workers page leaked the fleet region")
	}

	// Unauthenticated → redirect to login.
	rec = uiGet(t, s, "/admin/workers", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated workers page = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/admin/login") {
		t.Errorf("unauthenticated redirect = %q, want /admin/login", loc)
	}
}

// TestUIWorkerDetail covers the detail screen: a known worker renders 200 with the
// detail + a "View logs" deep link to /admin/logs?worker=<id> (AC1, the ~3-click
// path to logs), and an unknown worker renders the not-found body with a 404.
func TestUIWorkerDetail(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}}
	s, authSvc := adminTestServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/workers/worker-aaaa1111", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET worker detail = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The View logs affordance points at the worker's logs (AC1).
	if !strings.Contains(body, "/admin/logs?worker=worker-aaaa1111") {
		t.Error("worker detail missing the View logs deep link to /admin/logs?worker=<id>")
	}
	if !strings.Contains(body, "View logs") {
		t.Error("worker detail missing the 'View logs' affordance text")
	}
	// The management controls are present (drain, force-evict, pull).
	for _, want := range []string{"Force-evict", "Drain", "Pull", "Models", "llama3"} {
		if !strings.Contains(body, want) {
			t.Errorf("worker detail missing %q control/section", want)
		}
	}

	// Unknown worker → 404 with the not-found body.
	rec = uiGet(t, s, "/admin/workers/ghost", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worker detail = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Worker not connected") {
		t.Error("unknown worker should render the not-found body")
	}
}

// TestUIHeatmapPartial covers the GPU heatmap fragment: 200 with the per-worker
// cells, each carrying a load band WORD (color + text, AC2) and a one-click link
// to that worker's detail.
func TestUIHeatmapPartial(t *testing.T) {
	w1 := detailWorker("worker-aaaa1111")
	w1.Load = 10 // ok band
	w2 := detailWorker("worker-bbbb2222")
	w2.Load = 95 // hot band
	fleet := &fakeFleet{snapshot: []types.Worker{w1, w2}}
	s, authSvc := adminTestServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/partials/gpu-heatmap", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("heatmap partial = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Band words are rendered (text, not color alone) and cells link to detail.
	for _, want := range []string{"ok", "hot", "/admin/workers/worker-aaaa1111", "/admin/workers/worker-bbbb2222", "GPU utilization"} {
		if !strings.Contains(body, want) {
			t.Errorf("heatmap partial missing %q", want)
		}
	}
	// A cell's accessible label states the load in words (color is never the sole signal).
	if !strings.Contains(body, "load 95 percent, hot") {
		t.Error("heatmap cell missing a text aria-label stating load + band")
	}
}

// TestUIWorkerListPartial covers the live worker-list fragment: 200 with a row per
// worker, the status as a text-labeled badge, and a link to detail.
func TestUIWorkerListPartial(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}}
	s, authSvc := adminTestServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/partials/worker-list", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("worker-list partial = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/admin/workers/worker-aaaa1111", "online", "Last seen", "GPU"} {
		if !strings.Contains(body, want) {
			t.Errorf("worker-list partial missing %q", want)
		}
	}
}

// TestUIWorkerListPartialEmpty proves the designed empty state (AC4): a fleet with
// no workers renders the calm "No workers connected" invitation, not an empty grid.
func TestUIWorkerListPartialEmpty(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/partials/worker-list", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty worker-list partial = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No workers connected") {
		t.Error("empty fleet should render the 'No workers connected' empty state")
	}
}

// TestUIWorkerDrain covers the drain write end-to-end: the happy path calls
// DrainWorkerWithDeadline(id, 0) (the soft drain) and records a worker.drain audit
// entry; a missing CSRF token is refused 403 with NO fleet call and NO audit; a key
// without workers:write is refused 403.
func TestUIWorkerDrain(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	writer := workersWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	// Missing CSRF → 403, and the control plane is NOT touched and nothing audited.
	rec := uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/drain", session, "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("drain without CSRF = %d, want 403", rec.Code)
	}
	if fleet.drained != "" {
		t.Errorf("drain without CSRF still called the control plane (drained=%q)", fleet.drained)
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpWorkerDrain}, 0)); n != 0 {
		t.Errorf("drain without CSRF recorded %d audit entries, want 0", n)
	}

	// Happy path: CSRF + workers:write → 200, soft drain dispatched, one audit entry.
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/drain", session, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drain happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if fleet.drained != "worker-aaaa1111" || fleet.drainDeadline != 0 {
		t.Errorf("drain called with (%q,%v), want (worker-aaaa1111, 0) — a soft drain", fleet.drained, fleet.drainDeadline)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpWorkerDrain}, 0)
	if len(entries) != 1 {
		t.Fatalf("drain recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != "worker-aaaa1111" || e.Outcome != audit.OutcomeSuccess || e.Actor == "" {
		t.Errorf("drain audit entry = %+v, want target=worker-aaaa1111 success with an actor", e)
	}

	// A key without workers:write is refused 403 (workers:read only).
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/drain", roSession, roCSRF, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("drain without workers:write = %d, want 403", rec.Code)
	}
}

// TestUIWorkerEvict covers force-evict: the happy path reuses the timed-drain path
// with a positive (immediate) deadline and records a worker.evict audit entry;
// missing CSRF and missing workers:write are both 403. (The typed-name confirm that
// gates this in the UI is a client concern, covered by the E2E spec; the server
// still enforces CSRF + scope.)
func TestUIWorkerEvict(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	writer := workersWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	// Missing CSRF → 403, no control-plane call, no audit.
	rec := uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/evict", session, "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("evict without CSRF = %d, want 403", rec.Code)
	}
	if fleet.drained != "" {
		t.Errorf("evict without CSRF still called the control plane (drained=%q)", fleet.drained)
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpWorkerEvict}, 0)); n != 0 {
		t.Errorf("evict without CSRF recorded %d audit entries, want 0", n)
	}

	// Happy path: a forced (immediate, positive deadline) eviction + audit entry.
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/evict", session, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("evict happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if fleet.drained != "worker-aaaa1111" || fleet.drainDeadline <= 0 {
		t.Errorf("evict called with (%q,%v), want (worker-aaaa1111, >0) — a forced eviction", fleet.drained, fleet.drainDeadline)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpWorkerEvict}, 0)
	if len(entries) != 1 {
		t.Fatalf("evict recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != "worker-aaaa1111" || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("evict audit entry = %+v, want target=worker-aaaa1111 success", e)
	}

	// Without workers:write → 403.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/evict", roSession, roCSRF, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("evict without workers:write = %d, want 403", rec.Code)
	}
}

// TestUIWorkerPull covers the model pull: the happy path calls AdminPullModel and
// records a model.pull audit entry with the worker/model target; missing CSRF is
// 403 with no dispatch; a key without models:write is 403; an empty model is 400.
func TestUIWorkerPull(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	writer := workersWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	// Missing CSRF → 403, no dispatch, no audit.
	rec := uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/models", session, "", url.Values{"model": {"llama3"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("pull without CSRF = %d, want 403", rec.Code)
	}
	if fleet.pulledModel != "" {
		t.Errorf("pull without CSRF still dispatched (model=%q)", fleet.pulledModel)
	}

	// Happy path: dispatch + audit.
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/models", session, csrf, url.Values{"model": {"llama3"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("pull happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if fleet.pulledWorker != "worker-aaaa1111" || fleet.pulledModel != "llama3" {
		t.Errorf("AdminPullModel called with (%q,%q), want (worker-aaaa1111, llama3)", fleet.pulledWorker, fleet.pulledModel)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpModelPull}, 0)
	if len(entries) != 1 {
		t.Fatalf("pull recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != "worker-aaaa1111/llama3" || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("pull audit entry = %+v, want target=worker-aaaa1111/llama3 success", e)
	}

	// Empty model → 400, no dispatch.
	fleet.pulledModel = ""
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/models", session, csrf, url.Values{"model": {""}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("pull with empty model = %d, want 400", rec.Code)
	}
	if fleet.pulledModel != "" {
		t.Errorf("empty-model pull still dispatched (model=%q)", fleet.pulledModel)
	}

	// Without models:write → 403.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, "/admin/workers/worker-aaaa1111/models", roSession, roCSRF, url.Values{"model": {"llama3"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("pull without models:write = %d, want 403", rec.Code)
	}
}

// TestUIWorkerUnload covers the model unload: the happy path calls AdminUnloadModel
// (capturing a colon-tagged ref whole via the trailing wildcard) and records a
// model.unload audit entry; missing CSRF is 403 with no dispatch; a key without
// models:write is 403.
func TestUIWorkerUnload(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	writer := workersWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	// Missing CSRF → 403, no dispatch.
	rec := uiWrite(t, s, http.MethodDelete, "/admin/workers/worker-aaaa1111/models/llama3", session, "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unload without CSRF = %d, want 403", rec.Code)
	}
	if fleet.unloadedModel != "" {
		t.Errorf("unload without CSRF still dispatched (model=%q)", fleet.unloadedModel)
	}

	// Happy path: a colon-tagged model id is captured whole by the wildcard.
	rec = uiWrite(t, s, http.MethodDelete, "/admin/workers/worker-aaaa1111/models/qwen2:0.5b", session, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unload happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if fleet.unloadedWorker != "worker-aaaa1111" || fleet.unloadedModel != "qwen2:0.5b" {
		t.Errorf("AdminUnloadModel called with (%q,%q), want (worker-aaaa1111, qwen2:0.5b)", fleet.unloadedWorker, fleet.unloadedModel)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpModelUnload}, 0)
	if len(entries) != 1 {
		t.Fatalf("unload recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != "worker-aaaa1111/qwen2:0.5b" || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("unload audit entry = %+v, want target=worker-aaaa1111/qwen2:0.5b success", e)
	}

	// Without models:write → 403.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodDelete, "/admin/workers/worker-aaaa1111/models/llama3", roSession, roCSRF, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unload without models:write = %d, want 403", rec.Code)
	}
}

// TestUIWorkerActionNotFound proves a write against a worker the control plane no
// longer knows surfaces a clean not-found toast (404) and records a failure audit
// entry, rather than 500-ing.
func TestUIWorkerActionNotFound(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{detailWorker("worker-aaaa1111")}, drainErr: server.ErrWorkerNotFound}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	writer := workersWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	rec := uiWrite(t, s, http.MethodPost, "/admin/workers/ghost/drain", session, csrf, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("drain of missing worker = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not connected") {
		t.Error("missing-worker drain should render a 'not connected' toast")
	}
	entries := auditLog.List(audit.Filter{Op: auditOpWorkerDrain}, 0)
	if len(entries) != 1 || entries[0].Outcome != audit.OutcomeFailure {
		t.Errorf("missing-worker drain should record one failure audit entry, got %+v", entries)
	}
}

// TestUIWorkerDataProjection is a thin unit check on the data layer that backs the
// screens: the in-process projection matches the fleet snapshot (so the console's
// numbers track the API by construction), independent of the HTTP layer.
func TestUIWorkerDataProjection(t *testing.T) {
	reg := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	w := types.Worker{
		ID: "w-1", Status: types.WorkerOnline, ActiveJobs: 3, Load: 70,
		GPUType: "NVIDIA RTX 4090", TotalVRAM: 24 << 30, FreeVRAM: 6 << 30,
		Models: []types.Model{{Name: "mistral"}, {Name: "llama3"}}, RegisteredAt: reg, LastSeen: reg.Add(time.Hour),
	}
	s, _ := adminTestServer(t, &fakeFleet{snapshot: []types.Worker{w}})

	detail, ok := s.collectWorkerDetail("w-1")
	if !ok {
		t.Fatal("collectWorkerDetail(w-1) not found")
	}
	if detail.LoadTone == "" || detail.UsedPct != 75 { // (24-6)/24 = 75%
		t.Errorf("detail UsedPct=%d tone=%q, want 75%% with a tone", detail.UsedPct, detail.LoadTone)
	}
	if detail.LogsHref != "/admin/logs?worker=w-1" {
		t.Errorf("detail LogsHref=%q, want /admin/logs?worker=w-1", detail.LogsHref)
	}
	if len(detail.Models) != 2 || detail.Models[0] != "llama3" {
		t.Errorf("detail models not sorted: %+v", detail.Models)
	}

	// The heatmap reduces the same snapshot; the busy worker (load 70) bands warn/busy.
	hm := s.collectHeatmap()
	if hm.WorkerCount != 1 || len(hm.Cells) != 1 {
		t.Fatalf("heatmap WorkerCount=%d cells=%d, want 1/1", hm.WorkerCount, len(hm.Cells))
	}
	if hm.Cells[0].BandWord != "busy" {
		t.Errorf("heatmap cell band word = %q for load 70, want busy", hm.Cells[0].BandWord)
	}
}
