package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
)

// webui_logs_test.go exercises the console's Logs viewer (#103): the page (with the
// filter bar + CSV export link + live-tail panel), the filtered line-table partial
// (proving a tightened filter reduces the rows and that structured fields render as
// discrete badges, never embedded in the message), the logs:read scope gate, and the
// SSE live-tail proxy (reachable with logs:read; reusing the shared stream). It
// drives requests through the fully-routed s.Handler().

// TestUILogsPage covers the logs screen: an authenticated logs:read viewer gets 200
// with the filter bar, the CSV export link, the live-tail panel, and the HTMX line
// table region.
func TestUILogsPage(t *testing.T) {
	s, authSvc, _ := logUITestServer(t)
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/logs", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/logs = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Logs", `id="log-filter"`, `id="log-table"`, "Live tail", "Export CSV", "format=csv", "Resume"} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page missing %q", want)
		}
	}
}

// TestUILogsPageScopeGated proves the logs:read gate and the unauthenticated
// redirect, plus the sidebar entry tracking the scope.
func TestUILogsPageScopeGated(t *testing.T) {
	s, authSvc, _ := logUITestServer(t)

	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	rec := uiGet(t, s, "/admin/logs", map[string]string{sessionCookieName: userToken})
	if rec.Code != http.StatusForbidden {
		t.Errorf("logs page for unscoped key = %d, want 403", rec.Code)
	}

	logsSession, _ := loginAndGetSession(t, s, mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}}))
	dash := uiGet(t, s, "/admin/", map[string]string{sessionCookieName: logsSession})
	if !strings.Contains(dash.Body.String(), `href="/admin/logs"`) {
		t.Error("logs:read viewer's sidebar should link to /admin/logs")
	}

	rec = uiGet(t, s, "/admin/logs", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated logs page = %d, want 303", rec.Code)
	}
}

// TestUILogsPartialFilterReducesVolume proves the filtered line table: structured
// fields render as DISCRETE badges (key=value, never in the message), the default
// (no level) view excludes debug/info noise, and a worker filter reduces the rows to
// just the matching lines — the AC's "filters that reduce volume".
func TestUILogsPartialFilterReducesVolume(t *testing.T) {
	s, authSvc, src := logUITestServer(t)
	seedLogRecords(src) // 4 lines: DEBUG/INFO/WARN/ERROR across request_id/session_id/worker
	logs := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})
	session, _ := loginAndGetSession(t, s, logs)

	// Default (no level): the warn floor excludes DEBUG/INFO, leaving WARN + ERROR (2).
	rec := uiGet(t, s, "/admin/partials/logs", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("logs partial = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "slow worker") || !strings.Contains(body, "dispatch failed") {
		t.Error("default logs view should include the WARN + ERROR lines")
	}
	if strings.Contains(body, "trace tick") || strings.Contains(body, "served request") {
		t.Error("default logs view should exclude DEBUG/INFO under the warn floor")
	}
	// Structured fields are discrete badges, not embedded in the message: the ERROR
	// line carries worker=w2 as a badge.
	if !strings.Contains(body, "worker=w2") {
		t.Error("logs partial should render structured fields as discrete key=value badges")
	}

	// A worker=w1 filter at level=debug reduces to the single WARN line for w1.
	rec = uiGet(t, s, "/admin/partials/logs?level=debug&worker=w1", map[string]string{sessionCookieName: session})
	body = rec.Body.String()
	if !strings.Contains(body, "slow worker") {
		t.Error("worker=w1 filter should include the w1 WARN line")
	}
	for _, excluded := range []string{"dispatch failed", "trace tick", "served request"} {
		if strings.Contains(body, excluded) {
			t.Errorf("worker=w1 filter should exclude %q", excluded)
		}
	}
}

// TestUILogsStreamReachable proves the SSE proxy is gated on logs:read and, with the
// scope, opens an event stream (the shared SSE writer's content type), delivering a
// NEW line appended after the connection opens. The request context is cancelled to
// stop the infinite tail.
func TestUILogsStreamReachable(t *testing.T) {
	s, authSvc, src := logUITestServer(t)
	// Drive the tail fast so the test does not wait on the real interval.
	s.logStreamPoll = 5 * time.Millisecond
	logs := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})
	session, _ := loginAndGetSession(t, s, logs)

	// Without logs:read → 403 (a non-streaming refusal before any frame).
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	denied := uiGet(t, s, "/admin/logs/stream", map[string]string{sessionCookieName: userToken})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("stream without logs:read = %d, want 403", denied.Code)
	}

	// With logs:read: open the stream with a short-lived context, append a line, and
	// assert it is delivered as an SSE data frame.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/admin/logs/stream?level=debug", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		// Append a matching line shortly after the stream opens so the tail emits it.
		time.Sleep(20 * time.Millisecond)
		src.add(LogRecord{Time: logAt(100), Level: "ERROR", Message: "tail-line-xyz", Attrs: map[string]any{"worker": "w9"}})
		<-ctx.Done()
		close(done)
	}()

	s.Handler().ServeHTTP(rec, r)
	<-done

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Errorf("stream Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "tail-line-xyz") {
		t.Errorf("stream did not deliver the appended line; body: %s", rec.Body.String())
	}
}

// TestUILogsStreamDisabledWhenNoSource proves the stream returns a non-200 (501)
// when no log source is wired, rather than hanging.
func TestUILogsStreamDisabledWhenNoSource(t *testing.T) {
	s, authSvc := adminTestServerNoQuota(t) // no log source wired
	logs := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})
	session, _ := loginAndGetSession(t, s, logs)

	rec := uiGet(t, s, "/admin/logs/stream", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("stream with no log source = %d, want 501", rec.Code)
	}
}
