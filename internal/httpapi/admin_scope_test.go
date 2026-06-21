package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestScopedKeyMatrix is the table-driven proof of AC1 at the HTTP layer: a key
// holding a SPECIFIC admin scope passes exactly its scope-gated routes and is
// 403 on the others, while a RoleAdmin key passes every route (superuser). It
// exercises the real routed handler so the scope middleware wiring on each route
// is what is under test.
func TestScopedKeyMatrix(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		{ID: "w1", Status: types.WorkerOnline},
	}}
	s, authSvc := adminTestServer(t, fleet)

	// One key per scope, plus the admin superuser. Each non-admin key holds ONLY
	// the named scope so the matrix is exact.
	keysReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	keysWriter := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysWrite}})
	workersReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	workersWriter := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersWrite}})
	telemetryReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeTelemetryRead}})
	auditReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeAuditRead}})
	adminToken := mustKey(t, authSvc, adminPerms())

	// Create a target key so the {id} routes act on a real key.
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"target"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	id := created.ID

	// Each route names the scope that should grant it. A key holding that scope
	// must pass (non-403); every other non-admin key must be 403.
	type routeCase struct {
		method, path, body, scope string
	}
	routes := []routeCase{
		{http.MethodPost, "/v1/admin/keys", `{"name":"x"}`, authz.ScopeKeysWrite},
		{http.MethodGet, "/v1/admin/keys", "", authz.ScopeKeysRead},
		{http.MethodGet, "/v1/admin/keys/" + id, "", authz.ScopeKeysRead},
		{http.MethodPost, "/v1/admin/keys/" + id + "/rotate", "", authz.ScopeKeysWrite},
		{http.MethodPut, "/v1/admin/keys/" + id + "/permissions", `{"roles":["user"]}`, authz.ScopeKeysWrite},
		{http.MethodPut, "/v1/admin/keys/" + id + "/quota", `{"rpm":1}`, authz.ScopeKeysWrite},
		{http.MethodGet, "/v1/admin/keys/" + id + "/quota", "", authz.ScopeKeysRead},
		{http.MethodGet, "/v1/admin/workers", "", authz.ScopeWorkersRead},
		{http.MethodPost, "/v1/admin/workers/w1/drain", "", authz.ScopeWorkersWrite},
		{http.MethodGet, "/v1/admin/stats", "", authz.ScopeTelemetryRead},
		{http.MethodGet, "/v1/admin/audit", "", authz.ScopeAuditRead},
	}

	holders := map[string]string{
		authz.ScopeKeysRead:      keysReader,
		authz.ScopeKeysWrite:     keysWriter,
		authz.ScopeWorkersRead:   workersReader,
		authz.ScopeWorkersWrite:  workersWriter,
		authz.ScopeTelemetryRead: telemetryReader,
		authz.ScopeAuditRead:     auditReader,
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// The matching-scope key passes (anything but 403).
			rec := req(t, s, rt.method, rt.path, holders[rt.scope], rt.body)
			if rec.Code == http.StatusForbidden {
				t.Errorf("key with %s was 403 on its own route %s %s", rt.scope, rt.method, rt.path)
			}

			// Every other non-admin scope key is 403 on this route.
			for scope, token := range holders {
				if scope == rt.scope {
					continue
				}
				rec := req(t, s, rt.method, rt.path, token, rt.body)
				if rec.Code != http.StatusForbidden {
					t.Errorf("key with %s should be 403 on %s %s (needs %s), got %d",
						scope, rt.method, rt.path, rt.scope, rec.Code)
				}
				if rec.Code == http.StatusForbidden {
					if code := errorCode(t, rec); code != "forbidden" {
						t.Errorf("403 error code = %q, want forbidden", code)
					}
				}
			}

			// The admin superuser always passes (never 403).
			rec = req(t, s, rt.method, rt.path, adminToken, rt.body)
			if rec.Code == http.StatusForbidden {
				t.Errorf("admin superuser was 403 on %s %s", rt.method, rt.path)
			}
		})
	}
}

// TestNonScopedKeyForbidden proves a valid key with NO admin scope (and not
// admin) is 403 on every admin route (AC1).
func TestNonScopedKeyForbidden(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})

	for _, rt := range allAdminRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, userToken, rt.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("non-scoped key status = %d, want 403", rec.Code)
			}
		})
	}
}

// TestCreateKeyWithScopes proves a key can be created with admin scopes, that the
// scopes round-trip in the view, and that an unknown scope is rejected 400.
func TestCreateKeyWithScopes(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"scoped","admin_scopes":["keys:read","workers:write"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created struct {
		ID          string   `json:"id"`
		AdminScopes []string `json:"admin_scopes"`
	}
	decode(t, rec, &created)
	if len(created.AdminScopes) != 2 {
		t.Fatalf("admin_scopes round-trip wrong: %+v", created.AdminScopes)
	}

	// The scopes are visible in the metadata view too.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+created.ID, adminToken, "")
	if !strings.Contains(rec.Body.String(), "workers:write") {
		t.Errorf("get view missing admin_scopes: %s", rec.Body.String())
	}

	// An unknown scope is rejected.
	rec = req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"bad","admin_scopes":["keys:delete"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown scope status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_request_error" {
		t.Errorf("unknown scope code = %q, want invalid_request_error", code)
	}
}

// TestSetPermissionsRejectsUnknownScope proves the permissions editor validates
// scopes too (a write surface that accepts admin_scopes per #90).
func TestSetPermissionsRejectsUnknownScope(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"t"}`)
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)

	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+created.ID+"/permissions", adminToken,
		`{"roles":["user"],"admin_scopes":["bogus:read"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown scope on set-permissions status = %d, want 400", rec.Code)
	}
}
