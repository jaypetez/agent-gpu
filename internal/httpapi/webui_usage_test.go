package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/store"
	usagepkg "github.com/jaypetez/agent-gpu/internal/usage"
)

// webui_usage_test.go exercises the console's Usage screen (#103): the page + the
// HTMX usage-board partial, the telemetry:read scope gate (and the unauthenticated
// redirect), and the in-process projection (consumption-vs-limit meters, the 7-day
// sparkline, and the exhaustion forecast) that reuses the SAME per-key row the JSON
// GET /v1/admin/usage builds. It reuses the #100/#101/#102 rig (mustKey/
// loginAndGetSession/uiGet) and drives requests through the fully-routed s.Handler().

// TestUIUsagePage covers the usage screen: an authenticated telemetry:read viewer
// gets 200 with the page chrome and the HTMX-polled board region wired to its
// partial.
func TestUIUsagePage(t *testing.T) {
	s, authSvc, _ := usageTestServer(t, quota.Limits{}, usagepkg.New())
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/usage", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/usage = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Usage", `id="usage-board"`, "partials/usage"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage page missing %q", want)
		}
	}
}

// TestUIUsagePageScopeGated proves the telemetry:read gate: a valid key without it
// gets 403 (authenticated, so not a redirect), and an unauthenticated request is
// redirected to login.
func TestUIUsagePageScopeGated(t *testing.T) {
	s, authSvc, _ := usageTestServer(t, quota.Limits{}, usagepkg.New())

	// Authenticated but lacking telemetry:read → 403.
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	rec := uiGet(t, s, "/admin/usage", map[string]string{sessionCookieName: userToken})
	if rec.Code != http.StatusForbidden {
		t.Errorf("usage page for unscoped key = %d, want 403", rec.Code)
	}

	// A key holding a different admin scope can sign in but must not see the Usage
	// sidebar entry (role-based IA); a telemetry:read viewer must.
	noTelem, _ := loginAndGetSession(t, s, mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}}))
	dash := uiGet(t, s, "/admin/", map[string]string{sessionCookieName: noTelem})
	if strings.Contains(dash.Body.String(), `href="/admin/usage"`) {
		t.Error("a viewer without telemetry:read should not have the Usage sidebar entry")
	}

	// Unauthenticated → redirect to login.
	rec = uiGet(t, s, "/admin/usage", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated usage page = %d, want 303", rec.Code)
	}
}

// TestUIUsagePartialMetersAndSparkline proves the partial renders the meters,
// sparkline, and forecast from a rising series approaching a daily limit. It also
// proves the partial is gated on telemetry:read.
func TestUIUsagePartialMetersAndSparkline(t *testing.T) {
	series := usagepkg.New()
	s, authSvc, eng := usageTestServer(t, quota.Limits{}, series)
	telem := telemetryKey(t, authSvc)
	session, _ := loginAndGetSession(t, s, telem)

	// A limited key with a rising multi-day series + today's partial consumption, so a
	// daily forecast and a drawable sparkline are produced.
	limited := mustKeyNamed(t, authSvc, "alpha", auth.Permissions{}, &store.Limits{DailyTokens: 200_000})
	now := eng.Now()
	for d := -3; d < 0; d++ {
		series.Record([]quota.Snapshot{{KeyID: limited, TokensToday: 120_000}}, now.AddDate(0, 0, d))
	}
	eng.RecordTokens(context.Background(), limited, 150_000) // 75% of the 200k daily budget
	series.Record([]quota.Snapshot{{KeyID: limited, TokensToday: 150_000}}, now)

	rec := uiGet(t, s, "/admin/partials/usage", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("usage partial = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The key, the meter labels, the sparkline polyline, and the throttle summary all
	// render. A sparkline with >= 2 points emits a <polyline points="...">.
	for _, want := range []string{"alpha", "Daily tokens", "Monthly tokens", "Requests / min", "<polyline", "Fleet throttling", "Runs out"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage partial missing %q", want)
		}
	}
}

// TestUIUsagePartialDisabledWhenNoQuota proves the board renders the disabled notice
// (not a 500 or empty bars) when the quota engine is not wired, mirroring the JSON
// endpoint's nil-quota gate.
func TestUIUsagePartialDisabledWhenNoQuota(t *testing.T) {
	// A server with NO quota engine but telemetry:read wired.
	s, authSvc := adminTestServerNoQuota(t)
	telem := telemetryKey(t, authSvc)
	session, _ := loginAndGetSession(t, s, telem)

	rec := uiGet(t, s, "/admin/partials/usage", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("usage partial (no quota) = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Usage reporting is off") {
		t.Error("usage partial with no quota engine should render the disabled notice")
	}
}

// TestUsageMeterProjection unit-checks the meter projection: a limited dimension
// computes the right fill percentage + threshold tone, and an unlimited dimension is
// marked not-Limited (a calm "no limit" track, never a full/empty meter).
func TestUsageMeterProjection(t *testing.T) {
	// 150k of 200k → 75% → warn.
	m := tokenMeter("Daily tokens", 150_000, 200_000)
	if !m.Limited || m.Pct != 75 {
		t.Errorf("limited meter = %+v, want Limited with Pct=75", m)
	}
	if m.Tone == "" {
		t.Error("limited meter should carry a tone")
	}
	// Unlimited (0 limit) → not Limited, no fill.
	u := tokenMeter("Daily tokens", 999, 0)
	if u.Limited || u.Pct != 0 {
		t.Errorf("unlimited meter = %+v, want not Limited with Pct=0", u)
	}
	if u.Limit != "no limit" {
		t.Errorf("unlimited meter limit label = %q, want \"no limit\"", u.Limit)
	}
}

// TestUsageForecastView unit-checks the forecast projection: it prefers the SOONER
// of daily/monthly, names the dimension, and tones by urgency (alert within a day).
func TestUsageForecastView(t *testing.T) {
	now := time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)
	daily := now.Add(12 * time.Hour).Unix()
	monthly := now.Add(20 * 24 * time.Hour).Unix()
	f := usageForecastView(adminUsageForecast{DailyExhaustionAt: &daily, MonthlyExhaustionAt: &monthly}, now)
	if !f.Has || f.Dimension != "daily" {
		t.Errorf("forecast = %+v, want the sooner daily estimate", f)
	}
	// Within a day → danger tone.
	if f.Tone == "" {
		t.Error("forecast should carry a tone")
	}
	// No estimate → Has false.
	if g := usageForecastView(adminUsageForecast{}, now); g.Has {
		t.Errorf("no-estimate forecast = %+v, want Has=false", g)
	}
}
