package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// webui_keys_test.go exercises the console's keys / users / permissions surface
// (#102): the keys page and the masked-table partial, the keys:read scope gate (and
// the unauthenticated redirect), and — most importantly — every state-changing
// handler's CSRF + keys:write gate, its in-process auth.Service call, its single
// audit entry, the one-time-token reveal (and that the table only ever shows a
// mask), the CreatedBy=viewer provenance, and the unknown-role/scope rejection. It
// reuses the #100/#101 rig (adminTestServerWithAudit, mustKey, loginAndGetSession,
// uiGet, uiWrite) and drives requests through the fully-routed s.Handler().

// keysWriterToken mints a key holding keys:read + keys:write so a single session can
// drive the read screens and all four write actions.
func keysWriterToken(t *testing.T, authSvc *auth.Service) string {
	t.Helper()
	return mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{
		authz.ScopeKeysRead, authz.ScopeKeysWrite,
	}})
}

// findKeyByName returns the stored key with the given name, failing the test if it
// is absent. It is how the write tests assert what was actually persisted (masked),
// independent of the HTTP response.
func findKeyByName(t *testing.T, authSvc *auth.Service, name string) store.APIKey {
	t.Helper()
	keys, err := authSvc.List(context.Background())
	if err != nil {
		t.Fatalf("List keys: %v", err)
	}
	for _, k := range keys {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("no stored key named %q", name)
	return store.APIKey{}
}

// keyIDFromToken extracts the key id from a plaintext token (agpu_<id>_<secret>),
// so a test can learn the viewer's own key id from the token mustKey returns.
func keyIDFromToken(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, "_")
	if len(parts) < 3 {
		t.Fatalf("malformed token %q", token)
	}
	return parts[1]
}

// TestUIKeysPage covers the keys screen: an authenticated keys:read viewer gets 200
// with the page chrome, the HTMX-polled table region, and the role/scope catalog the
// editors render their pickers from (so a picker can't offer an unknown grant).
func TestUIKeysPage(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := keysWriterToken(t, authSvc)
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/keys", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/keys = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The page header, the polled region (by id + partial URL), the create action,
	// and the catalog-sourced pickers (a known role + a known scope) are present.
	for _, want := range []string{"API keys", `id="key-list"`, "partials/keys", "New key", authz.RoleAdmin, authz.ScopeKeysRead} {
		if !strings.Contains(body, want) {
			t.Errorf("keys page missing %q", want)
		}
	}
}

// TestUIKeysPageScopeGated proves the keys:read gate: a valid key without keys:read
// gets 403 (it is authenticated, so not a redirect), and an unauthenticated request
// is redirected to login. It also proves the sidebar entry is hidden for a viewer
// without keys:read (role-based IA) yet shown for one with it.
func TestUIKeysPageScopeGated(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	// Authenticated but unscoped: a user key (inference only, no admin scope) → 403,
	// and the table region must not leak.
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	rec := uiGet(t, s, "/admin/keys", map[string]string{sessionCookieName: userToken})
	if rec.Code != http.StatusForbidden {
		t.Errorf("keys page for unscoped key = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="key-list"`) {
		t.Error("403 keys page leaked the key-list region")
	}

	// A viewer who can sign in (holds an admin scope) but LACKS keys:read must not see
	// the API keys sidebar entry (role-based IA, AC4), while a keys:read viewer must.
	// (A pure-inference key with no admin scope can't sign in to the console at all,
	// so the sidebar-visibility contrast is drawn between two console-capable keys.)
	noKeysSession, _ := loginAndGetSession(t, s, mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}}))
	dash := uiGet(t, s, "/admin/", map[string]string{sessionCookieName: noKeysSession})
	if strings.Contains(dash.Body.String(), `href="/admin/keys"`) {
		t.Error("a viewer without keys:read should not have the API keys sidebar entry")
	}
	readerSession, _ := loginAndGetSession(t, s, mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}}))
	dash = uiGet(t, s, "/admin/", map[string]string{sessionCookieName: readerSession})
	if !strings.Contains(dash.Body.String(), `href="/admin/keys"`) {
		t.Error("keys:read viewer's sidebar should link to /admin/keys")
	}

	// Unauthenticated → redirect to login.
	rec = uiGet(t, s, "/admin/keys", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated keys page = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/admin/login") {
		t.Errorf("unauthenticated redirect = %q, want /admin/login", loc)
	}
}

// TestUIKeyListPartialMasksSecret covers the masked-table partial: 200 with a row
// per key showing the key id, name, roles, and status — and a FIXED secret mask,
// NEVER a token (the table renders no secret).
func TestUIKeyListPartialMasksSecret(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	viewer := keysWriterToken(t, authSvc)
	// Seed a second key whose plaintext token must not appear in the table.
	seedTok := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleReadOnly}, Owner: "ops@", Team: "platform"})
	seedID := keyIDFromToken(t, seedTok)
	session, _ := loginAndGetSession(t, s, viewer)

	rec := uiGet(t, s, "/admin/partials/keys", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("key-list partial = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{seedID[:8], "Keys", "Status", "active", maskedSecret} {
		if !strings.Contains(body, want) {
			t.Errorf("key-list partial missing %q", want)
		}
	}
	// The plaintext seed token (its full secret) must NOT appear anywhere in the table.
	if strings.Contains(body, seedTok) {
		t.Error("key-list partial leaked a plaintext token — the table must show only a mask")
	}
}

// TestUIKeyListPartialRendersViewerKey proves a non-empty store renders rows with
// per-row controls (the viewer's own key always exists). The empty-state copy is a
// pure render branch; the data projection is unit-checked in TestUIKeyDataProjection.
func TestUIKeyListPartialRendersViewerKey(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	viewer := keysWriterToken(t, authSvc)
	session, _ := loginAndGetSession(t, s, viewer)

	rec := uiGet(t, s, "/admin/partials/keys", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("key-list partial = %d, want 200", rec.Code)
	}
	// At least the viewer's own key renders as a row with a Permissions control.
	if !strings.Contains(rec.Body.String(), "Permissions") {
		t.Error("key-list should render per-row Permissions control")
	}
}

// TestUIKeyCreate covers the create write end-to-end: missing CSRF is refused 403
// with NO key persisted and NO audit; a key without keys:write is 403; the happy
// path persists a MASKED key, stamps CreatedBy = the viewer's key id, returns the
// one-time plaintext token in the reveal (and never stores it), and records exactly
// one key.create audit entry that does NOT contain the token.
func TestUIKeyCreate(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	writer := keysWriterToken(t, authSvc)
	viewerID := keyIDFromToken(t, writer)
	session, csrf := loginAndGetSession(t, s, writer)

	form := url.Values{
		"name":         {"ci-pipeline"},
		"roles":        {authz.RoleUser},
		"admin_scopes": {authz.ScopeKeysRead},
		"owner":        {"ci@"},
	}

	// Missing CSRF → 403, nothing persisted, nothing audited.
	rec := uiWrite(t, s, http.MethodPost, "/admin/keys", session, "", form)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d, want 403", rec.Code)
	}
	if keysNamed(t, authSvc, "ci-pipeline") != 0 {
		t.Error("create without CSRF still persisted a key")
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpKeyCreate}, 0)); n != 0 {
		t.Errorf("create without CSRF recorded %d audit entries, want 0", n)
	}

	// Happy path: CSRF + keys:write → 201, a masked key persisted, the token revealed
	// once, CreatedBy = viewer, one audit entry without the token.
	rec = uiWrite(t, s, http.MethodPost, "/admin/keys", session, csrf, form)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create happy path = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	key := findKeyByName(t, authSvc, "ci-pipeline")
	if key.CreatedBy != viewerID {
		t.Errorf("created key CreatedBy = %q, want viewer id %q", key.CreatedBy, viewerID)
	}
	if len(key.SecretHash) == 0 {
		t.Error("created key should have a secret hash stored (the secret itself is never stored)")
	}
	// The reveal contains a freshly-minted plaintext token for THIS key id; the table
	// (a separate fragment) never would.
	body := rec.Body.String()
	wantTokenPrefix := "agpu_" + key.ID + "_"
	if !strings.Contains(body, wantTokenPrefix) {
		t.Errorf("create response did not reveal the one-time token (prefix %q)\nbody: %s", wantTokenPrefix, body)
	}
	if !strings.Contains(body, "shown once") {
		t.Error("create reveal should warn the token is shown once")
	}
	entries := auditLog.List(audit.Filter{Op: auditOpKeyCreate}, 0)
	if len(entries) != 1 {
		t.Fatalf("create recorded %d audit entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Target != key.ID || e.Outcome != audit.OutcomeSuccess || e.Actor == "" {
		t.Errorf("create audit entry = %+v, want target=%s success with an actor", e, key.ID)
	}
	// The audit entry must never carry the plaintext token.
	if strings.Contains(auditString(e), wantTokenPrefix) {
		t.Error("create audit entry leaked the plaintext token")
	}

	// A key without keys:write is refused 403 (keys:read only), nothing persisted.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, "/admin/keys", roSession, roCSRF, url.Values{"name": {"nope"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("create without keys:write = %d, want 403", rec.Code)
	}
	if keysNamed(t, authSvc, "nope") != 0 {
		t.Error("create without keys:write still persisted a key")
	}
}

// TestUIKeyCreateRejectsUnknownGrant proves the AC2 server-side validation: an
// unknown role (and, separately, an unknown scope) is rejected 400 BEFORE any key is
// created — nothing is persisted and nothing is audited.
func TestUIKeyCreateRejectsUnknownGrant(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	writer := keysWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)

	bad := []url.Values{
		{"name": {"bad-role"}, "roles": {"superuser"}},
		{"name": {"bad-scope"}, "admin_scopes": {"keys:destroy"}},
	}
	for _, form := range bad {
		rec := uiWrite(t, s, http.MethodPost, "/admin/keys", session, csrf, form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create with %v = %d, want 400", form, rec.Code)
		}
		if keysNamed(t, authSvc, form.Get("name")) != 0 {
			t.Errorf("create with %v still persisted a key", form)
		}
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpKeyCreate}, 0)); n != 0 {
		t.Errorf("rejected creates recorded %d audit entries, want 0", n)
	}
}

// TestUIKeyRotate covers rotate: missing CSRF is 403 with no rotation and no audit;
// the happy path reveals a NEW one-time token (different from the original), keeps
// the key id, and records one key.rotate audit entry without the token; a key without
// keys:write is 403.
func TestUIKeyRotate(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	writer := keysWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)
	// A target key to rotate.
	targetTok := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	targetID := keyIDFromToken(t, targetTok)
	hashBefore := append([]byte(nil), findKeyByID(t, authSvc, targetID).SecretHash...)

	path := "/admin/keys/" + targetID + "/rotate"

	// Missing CSRF → 403, secret unchanged, no audit.
	rec := uiWrite(t, s, http.MethodPost, path, session, "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rotate without CSRF = %d, want 403", rec.Code)
	}
	if !bytesEqual(findKeyByID(t, authSvc, targetID).SecretHash, hashBefore) {
		t.Error("rotate without CSRF still changed the secret")
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpKeyRotate}, 0)); n != 0 {
		t.Errorf("rotate without CSRF recorded %d audit entries, want 0", n)
	}

	// Happy path: a new token is revealed, the id is preserved, the secret changed.
	rec = uiWrite(t, s, http.MethodPost, path, session, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "agpu_"+targetID+"_") {
		t.Errorf("rotate did not reveal a new token for the same id %q\nbody: %s", targetID, body)
	}
	if strings.Contains(body, targetTok) {
		t.Error("rotate revealed the OLD token; it must mint a new one")
	}
	if bytesEqual(findKeyByID(t, authSvc, targetID).SecretHash, hashBefore) {
		t.Error("rotate did not change the stored secret")
	}
	entries := auditLog.List(audit.Filter{Op: auditOpKeyRotate}, 0)
	if len(entries) != 1 {
		t.Fatalf("rotate recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != targetID || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("rotate audit entry = %+v, want target=%s success", e, targetID)
	}
	if strings.Contains(auditString(entries[0]), "agpu_"+targetID+"_") {
		t.Error("rotate audit entry leaked the plaintext token")
	}

	// Without keys:write → 403.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, path, roSession, roCSRF, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("rotate without keys:write = %d, want 403", rec.Code)
	}
}

// TestUIKeyRevoke covers revoke: missing CSRF is 403 with the key still active and no
// audit; the happy path marks the key revoked and records one key.revoke audit entry;
// a key without keys:write is 403.
func TestUIKeyRevoke(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	writer := keysWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)
	targetTok := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	targetID := keyIDFromToken(t, targetTok)
	path := "/admin/keys/" + targetID + "/revoke"

	// Missing CSRF → 403, still active, no audit.
	rec := uiWrite(t, s, http.MethodPost, path, session, "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke without CSRF = %d, want 403", rec.Code)
	}
	if findKeyByID(t, authSvc, targetID).Revoked() {
		t.Error("revoke without CSRF still revoked the key")
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpKeyRevoke}, 0)); n != 0 {
		t.Errorf("revoke without CSRF recorded %d audit entries, want 0", n)
	}

	// Happy path: revoked + one audit entry.
	rec = uiWrite(t, s, http.MethodPost, path, session, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !findKeyByID(t, authSvc, targetID).Revoked() {
		t.Error("revoke happy path did not revoke the key")
	}
	entries := auditLog.List(audit.Filter{Op: auditOpKeyRevoke}, 0)
	if len(entries) != 1 {
		t.Fatalf("revoke recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != targetID || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("revoke audit entry = %+v, want target=%s success", e, targetID)
	}

	// Without keys:write → 403.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, "/admin/keys/"+targetID+"/revoke", roSession, roCSRF, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("revoke without keys:write = %d, want 403", rec.Code)
	}
}

// TestUIKeyPermissions covers the full-replace permissions editor: missing CSRF is
// 403 with permissions unchanged and no audit; an unknown role/scope is 400 with
// nothing applied; the happy path REPLACES the key's roles/scopes/model-lists
// wholesale and records one key.permissions audit entry; a key without keys:write is
// 403.
func TestUIKeyPermissions(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	writer := keysWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)
	// A target key that starts with user role + a deny list, to prove full replace.
	targetTok := mustKey(t, authSvc, auth.Permissions{
		Roles:      []string{authz.RoleUser},
		DenyModels: []string{"gpt-4"},
	})
	targetID := keyIDFromToken(t, targetTok)
	path := "/admin/keys/" + targetID + "/permissions"

	newPerms := url.Values{
		"roles":        {authz.RoleReadOnly},
		"admin_scopes": {authz.ScopeWorkersRead, authz.ScopeLogsRead},
		"allow_models": {"llama3\nmistral"},
		// deny_models intentionally omitted: a full replace must clear the old deny.
	}

	// Missing CSRF → 403, unchanged, no audit.
	rec := uiWrite(t, s, http.MethodPost, path, session, "", newPerms)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("permissions without CSRF = %d, want 403", rec.Code)
	}
	if k := findKeyByID(t, authSvc, targetID); !bytesEqualStrs(k.Roles, []string{authz.RoleUser}) {
		t.Errorf("permissions without CSRF changed roles to %v", k.Roles)
	}
	if n := len(auditLog.List(audit.Filter{Op: auditOpKeyPermissions}, 0)); n != 0 {
		t.Errorf("permissions without CSRF recorded %d audit entries, want 0", n)
	}

	// Unknown scope → 400, nothing applied.
	rec = uiWrite(t, s, http.MethodPost, path, session, csrf, url.Values{"admin_scopes": {"keys:destroy"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("permissions with unknown scope = %d, want 400", rec.Code)
	}
	if k := findKeyByID(t, authSvc, targetID); !bytesEqualStrs(k.Roles, []string{authz.RoleUser}) {
		t.Errorf("rejected permissions update still changed roles to %v", k.Roles)
	}

	// Happy path: a full replace of roles, scopes, and model lists + one audit entry.
	rec = uiWrite(t, s, http.MethodPost, path, session, csrf, newPerms)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions happy path = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	k := findKeyByID(t, authSvc, targetID)
	if !bytesEqualStrs(k.Roles, []string{authz.RoleReadOnly}) {
		t.Errorf("roles after replace = %v, want [read-only]", k.Roles)
	}
	if !bytesEqualStrs(k.AdminScopes, []string{authz.ScopeWorkersRead, authz.ScopeLogsRead}) {
		t.Errorf("scopes after replace = %v, want [workers:read logs:read]", k.AdminScopes)
	}
	if !bytesEqualStrs(k.AllowModels, []string{"llama3", "mistral"}) {
		t.Errorf("allow models after replace = %v, want [llama3 mistral]", k.AllowModels)
	}
	if len(k.DenyModels) != 0 {
		t.Errorf("deny models after replace = %v, want cleared (full replace)", k.DenyModels)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpKeyPermissions}, 0)
	if len(entries) != 1 {
		t.Fatalf("permissions recorded %d audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != targetID || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("permissions audit entry = %+v, want target=%s success", e, targetID)
	}

	// Without keys:write → 403.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	roSession, roCSRF := loginAndGetSession(t, s, reader)
	rec = uiWrite(t, s, http.MethodPost, path, roSession, roCSRF, newPerms)
	if rec.Code != http.StatusForbidden {
		t.Errorf("permissions without keys:write = %d, want 403", rec.Code)
	}
}

// TestUIKeyRotateRevokedConflict proves a rotate of a revoked key is surfaced
// cleanly (409 with an in-voice toast) and records a failure audit entry, rather
// than 500-ing.
func TestUIKeyRotateRevokedConflict(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	writer := keysWriterToken(t, authSvc)
	session, csrf := loginAndGetSession(t, s, writer)
	targetTok := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	targetID := keyIDFromToken(t, targetTok)
	if err := authSvc.Revoke(context.Background(), targetID); err != nil {
		t.Fatalf("seed revoke: %v", err)
	}

	rec := uiWrite(t, s, http.MethodPost, "/admin/keys/"+targetID+"/rotate", session, csrf, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rotate of revoked key = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "revoked") {
		t.Error("rotate of revoked key should render a 'revoked' toast")
	}
	entries := auditLog.List(audit.Filter{Op: auditOpKeyRotate}, 0)
	if len(entries) != 1 || entries[0].Outcome != audit.OutcomeFailure {
		t.Errorf("rotate of revoked key should record one failure audit entry, got %+v", entries)
	}
}

// TestUIKeyDataProjection is a thin unit check on the data layer behind the screen:
// the masked projection reflects the store, derives the right status word/tone for
// active/revoked/expired keys, and never carries a secret.
func TestUIKeyDataProjection(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	// Active, revoked, and (separately) the masked-secret invariant.
	activeTok := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	activeID := keyIDFromToken(t, activeTok)
	revokedTok := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})
	revokedID := keyIDFromToken(t, revokedTok)
	if err := authSvc.Revoke(context.Background(), revokedID); err != nil {
		t.Fatalf("seed revoke: %v", err)
	}

	rows, err := s.collectKeys(context.Background())
	if err != nil {
		t.Fatalf("collectKeys: %v", err)
	}
	byID := map[string]struct {
		status, tone string
		masked       string
	}{}
	for _, r := range rows {
		byID[r.ID] = struct {
			status, tone string
			masked       string
		}{r.Status, r.Tone, r.MaskedSecret}
		if strings.Contains(r.MaskedSecret, activeTok) || strings.Contains(r.MaskedSecret, revokedTok) {
			t.Error("a row's masked secret contained a real token")
		}
	}
	if got := byID[activeID]; got.status != "active" || got.tone == "" {
		t.Errorf("active key projection = %+v, want status=active with a tone", got)
	}
	if got := byID[revokedID]; got.status != "revoked" {
		t.Errorf("revoked key projection = %+v, want status=revoked", got)
	}
}

// --- small test helpers -----------------------------------------------------

// keysNamed counts stored keys with the given name (0 means "not persisted").
func keysNamed(t *testing.T, authSvc *auth.Service, name string) int {
	t.Helper()
	keys, err := authSvc.List(context.Background())
	if err != nil {
		t.Fatalf("List keys: %v", err)
	}
	n := 0
	for _, k := range keys {
		if k.Name == name {
			n++
		}
	}
	return n
}

// findKeyByID returns the stored key with the given id, failing if absent.
func findKeyByID(t *testing.T, authSvc *auth.Service, id string) store.APIKey {
	t.Helper()
	k, err := authSvc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get key %q: %v", id, err)
	}
	return k
}

// auditString renders an audit entry's redacted before/after maps to a string so a
// test can assert the plaintext token never appears in either.
func auditString(e audit.Entry) string {
	var b strings.Builder
	for k, v := range e.Before {
		b.WriteString(k)
		b.WriteString(toStr(v))
	}
	for k, v := range e.After {
		b.WriteString(k)
		b.WriteString(toStr(v))
	}
	return b.String()
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []string:
		return strings.Join(x, ",")
	default:
		return ""
	}
}

// bytesEqual reports byte-slice equality.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// bytesEqualStrs reports string-slice equality (order-sensitive), treating nil and
// empty as equal.
func bytesEqualStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
