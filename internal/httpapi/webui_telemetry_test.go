package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// webui_telemetry_test.go exercises the console's Telemetry dashboard (#103): the
// page + the HTMX telemetry-board partial, the telemetry:read scope gate (and the
// unauthenticated redirect), and the in-process projection (the KPI strip + the
// latency/wait histograms + the fleet-by-status + affinity panels) read from the
// SAME collectors GET /v1/admin/telemetry reads. It drives requests through the
// fully-routed s.Handler().

// TestUITelemetryPage covers the dashboard screen: an authenticated telemetry:read
// viewer gets 200 with the page chrome and the HTMX-polled board region.
func TestUITelemetryPage(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/telemetry", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/telemetry = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Telemetry", `id="telemetry-board"`, "partials/telemetry"} {
		if !strings.Contains(body, want) {
			t.Errorf("telemetry page missing %q", want)
		}
	}
}

// TestUITelemetryPageScopeGated proves the telemetry:read gate and the redirect for
// an unauthenticated request, plus that the sidebar entry tracks the scope.
func TestUITelemetryPageScopeGated(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	rec := uiGet(t, s, "/admin/telemetry", map[string]string{sessionCookieName: userToken})
	if rec.Code != http.StatusForbidden {
		t.Errorf("telemetry page for unscoped key = %d, want 403", rec.Code)
	}

	telemSession, _ := loginAndGetSession(t, s, mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeTelemetryRead}}))
	dash := uiGet(t, s, "/admin/", map[string]string{sessionCookieName: telemSession})
	if !strings.Contains(dash.Body.String(), `href="/admin/telemetry"`) {
		t.Error("telemetry:read viewer's sidebar should link to /admin/telemetry")
	}

	rec = uiGet(t, s, "/admin/telemetry", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated telemetry page = %d, want 303", rec.Code)
	}
}

// TestUITelemetryPartialRenders proves the board partial renders the KPI strip and
// the named panels from the live collectors, with a seeded fleet so the
// fleet-by-status breakdown has content.
func TestUITelemetryPartialRenders(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		{ID: "w1", Status: types.WorkerOnline},
		{ID: "w2", Status: types.WorkerDraining},
	}}
	s, authSvc := adminTestServer(t, fleet)
	telem := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeTelemetryRead}})
	session, _ := loginAndGetSession(t, s, telem)

	rec := uiGet(t, s, "/admin/partials/telemetry", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry partial = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Requests", "Mean latency", "Throttled", "Active sessions", "Request latency", "Time in queue", "Fleet by status", "Session affinity", "online", "draining"} {
		if !strings.Contains(body, want) {
			t.Errorf("telemetry partial missing %q", want)
		}
	}
}

// TestTelemetryProjectionAffinity unit-checks the affinity projection: the hit rate
// is hits/(hits+misses) with a tone, and zero traffic reports HasData=false.
func TestTelemetryProjectionAffinity(t *testing.T) {
	a := affinityView(80, 20, 5)
	if !a.HasData || a.HitRate != 80 {
		t.Errorf("affinity = %+v, want HasData with HitRate=80", a)
	}
	if a.RateTone == "" {
		t.Error("affinity should carry a rate tone")
	}
	if z := affinityView(0, 0, 0); z.HasData {
		t.Errorf("zero-traffic affinity = %+v, want HasData=false", z)
	}
}

// TestTelemetryProjectionFleetStatus unit-checks the fleet-status fold: statuses are
// sorted and toned (online ok, draining warn, else danger).
func TestTelemetryProjectionFleetStatus(t *testing.T) {
	rows := fleetStatusView(map[string]int{"online": 3, "stale": 1, "draining": 2})
	if len(rows) != 3 {
		t.Fatalf("fleet status rows = %d, want 3", len(rows))
	}
	// Sorted alphabetically: draining, online, stale.
	if rows[0].Status != "draining" || rows[1].Status != "online" || rows[2].Status != "stale" {
		t.Errorf("fleet status order = %v", []string{rows[0].Status, rows[1].Status, rows[2].Status})
	}
	if rows[1].Tone == "" {
		t.Error("online status should carry a tone")
	}
}
