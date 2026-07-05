package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
)

// webui_settings_test.go exercises the console's Settings screen (#103): the read
// page (gated config:read) and — most importantly — the live-apply PUT (gated
// config:write), proving it is refused without CSRF AND without config:write (no
// mutation), applies a valid tunable (the runtime value changes + one config.update
// audit entry), rejects an invalid value (400, nothing applied), and rejects a
// boot-only field (400, nothing applied). It reuses the config test recorder
// (recordingAppliers) so a test can assert what was pushed into the (fake)
// subsystems, and drives requests through the fully-routed s.Handler().

// uiPut issues a urlencoded PUT through the routed handler the way HTMX does (cookie
// auth + the double-submit CSRF token as cookie + header), so the settings write
// gate is exercised. A caller can omit the csrf to drive the CSRF-failure path.
func uiPut(t *testing.T, s *Server, path, session, csrf string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return uiWrite(t, s, http.MethodPut, path, session, csrf, form)
}

// configWriterToken mints a key holding config:read + config:write so one session
// drives the read page and the write.
func configWriterToken(t *testing.T, authSvc *auth.Service) string {
	t.Helper()
	return mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigRead, authz.ScopeConfigWrite}})
}

// TestUIConfigPage covers the settings screen: an authenticated config:read viewer
// gets 200 with the tabbed editor, a representative tunable, and the read-only boot
// section.
func TestUIConfigPage(t *testing.T) {
	s, authSvc, _ := settingsTestServer(t, &recordingAppliers{})
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/config", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Settings", "General", "Quotas", "Sessions", "Advanced", "log_level", "Boot-only", "Apply changes"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

// TestUIConfigPageScopeGated proves the config:read gate (a config:write-only key is
// 403 on the READ page, mirroring the JSON GET), the unauthenticated redirect, and
// that a config:read-only viewer sees the editor read-only (no interactive submit).
func TestUIConfigPageScopeGated(t *testing.T) {
	s, authSvc, _ := settingsTestServer(t, &recordingAppliers{})

	// config:write-only → 403 on the read page (read needs config:read).
	writeOnly := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigWrite}})
	rec := uiGet(t, s, "/admin/config", map[string]string{sessionCookieName: writeOnly})
	if rec.Code != http.StatusForbidden {
		t.Errorf("config page for config:write-only key = %d, want 403", rec.Code)
	}

	// config:read-only → 200 but read-only (no "Apply changes" submit).
	readSession, _ := loginAndGetSession(t, s, mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigRead}}))
	rec = uiGet(t, s, "/admin/config", map[string]string{sessionCookieName: readSession})
	if rec.Code != http.StatusOK {
		t.Fatalf("config page for config:read = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Apply changes") {
		t.Error("a config:read-only viewer should not get the interactive Apply control")
	}
	if !strings.Contains(rec.Body.String(), "needs config:write") {
		t.Error("a config:read-only viewer should see the read-only note")
	}

	// Unauthenticated → redirect.
	rec = uiGet(t, s, "/admin/config", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated config page = %d, want 303", rec.Code)
	}
}

// TestUIConfigUpdateCSRFAndScope proves the write gate: missing CSRF is refused 403
// with NO mutation and NO audit; a key without config:write is 403 with no mutation.
func TestUIConfigUpdateCSRFAndScope(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc, auditLog := settingsTestServer(t, rec)
	writer := configWriterToken(t, authSvc)
	session, _ := loginAndGetSession(t, s, writer)

	form := url.Values{"log_level": {"debug"}}

	// Missing CSRF → 403, applier never called, no audit.
	out := uiPut(t, s, "/admin/config", session, "", form)
	if out.Code != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", out.Code)
	}
	if rec.logLevelSet {
		t.Error("PUT without CSRF still applied a change")
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpConfigUpdate}, 0)); n != 0 {
		t.Errorf("PUT without CSRF recorded %d audit entries, want 0", n)
	}

	// Without config:write → 403 (route gate), nothing applied.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	out = uiPut(t, s, "/admin/config", roSession, roCSRF, form)
	if out.Code != http.StatusForbidden {
		t.Errorf("PUT without config:write = %d, want 403", out.Code)
	}
	if rec.logLevelSet {
		t.Error("PUT without config:write still applied a change")
	}
}

// TestUIConfigUpdateAppliesValid proves the happy path: a valid tunable is applied
// LIVE (the applier is called with the new value and the runtime holder updates),
// and exactly one config.update audit entry is recorded.
func TestUIConfigUpdateAppliesValid(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc, auditLog := settingsTestServer(t, rec)
	writer := configWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	out := uiPut(t, s, "/admin/config", session, csrf, url.Values{
		"log_level":       {"debug"},
		"quota_global_rpm": {"500"},
	})
	if out.Code != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200; body: %s", out.Code, out.Body.String())
	}
	if !rec.logLevelSet || rec.logLevel != "debug" {
		t.Errorf("log level applier = (%q, set=%v), want debug applied", rec.logLevel, rec.logLevelSet)
	}
	if !rec.qGlobalSet || rec.qGlobalRPM != 500 {
		t.Errorf("global rpm applier = (%d, set=%v), want 500 applied", rec.qGlobalRPM, rec.qGlobalSet)
	}
	// The runtime holder's current value reflects the change.
	s.config.mu.RLock()
	cur := s.config.cur
	s.config.mu.RUnlock()
	if cur.LogLevel != "debug" || cur.QuotaGlobalRPM != 500 {
		t.Errorf("runtime config after PUT = log_level %q, global_rpm %d; want debug/500", cur.LogLevel, cur.QuotaGlobalRPM)
	}
	// Exactly one config.update success audit entry.
	entries := auditLog.List(audit.Filter{Op: auditOpConfigUpdate}, 0)
	if len(entries) != 1 {
		t.Fatalf("PUT valid recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Outcome != audit.OutcomeSuccess || e.Target != "config" {
		t.Errorf("config audit entry = %+v, want target=config success", e)
	}
	// The response re-renders the editor with a success toast.
	if !strings.Contains(out.Body.String(), "Settings applied") {
		t.Error("PUT valid should render a success toast")
	}
}

// TestUIConfigUpdateRejectsInvalid proves an invalid value is rejected 400 with
// NOTHING applied and no success audit — the value is validated by the SAME
// applyConfigPatch the JSON PUT uses, before any apply.
func TestUIConfigUpdateRejectsInvalid(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc, auditLog := settingsTestServer(t, rec)
	writer := configWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	// An unknown log level.
	out := uiPut(t, s, "/admin/config", session, csrf, url.Values{"log_level": {"loud"}})
	if out.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid log level = %d, want 400; body: %s", out.Code, out.Body.String())
	}
	if rec.logLevelSet {
		t.Error("an invalid value must not be applied")
	}

	// A non-numeric value for a numeric field.
	out = uiPut(t, s, "/admin/config", session, csrf, url.Values{"quota_global_rpm": {"lots"}})
	if out.Code != http.StatusBadRequest {
		t.Errorf("PUT non-numeric rpm = %d, want 400", out.Code)
	}
	if rec.qGlobalSet {
		t.Error("a non-numeric numeric field must not be applied")
	}

	// An invalid duration.
	out = uiPut(t, s, "/admin/config", session, csrf, url.Values{"session_ttl": {"soon"}})
	if out.Code != http.StatusBadRequest {
		t.Errorf("PUT invalid duration = %d, want 400", out.Code)
	}

	if n := len(auditLog.List(audit.Filter{Op: auditOpConfigUpdate}, 0)); n != 0 {
		t.Errorf("rejected PUTs recorded %d audit entries, want 0", n)
	}
}

// TestUIConfigUpdateRejectsBootOnly proves a boot-only field present in the form is
// rejected 400 with NOTHING applied, mirroring the JSON PUT's read-only guard.
func TestUIConfigUpdateRejectsBootOnly(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc, auditLog := settingsTestServer(t, rec)
	writer := configWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	out := uiPut(t, s, "/admin/config", session, csrf, url.Values{
		"server_listen": {"127.0.0.1:9999"},
		"log_level":     {"debug"}, // a valid tunable alongside — still nothing applies
	})
	if out.Code != http.StatusBadRequest {
		t.Fatalf("PUT boot-only field = %d, want 400; body: %s", out.Code, out.Body.String())
	}
	if rec.logLevelSet {
		t.Error("a request touching a boot-only field must apply NOTHING (even valid sibling fields)")
	}
	if !strings.Contains(out.Body.String(), "read-only") {
		t.Error("the boot-only rejection should explain the field is read-only")
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpConfigUpdate}, 0)); n != 0 {
		t.Errorf("boot-only PUT recorded %d audit entries, want 0", n)
	}
}
