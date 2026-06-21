package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestAdminRolesEnumeration proves AC1: GET /v1/admin/roles enumerates the three
// assignable roles with the exact inference actions, model scope, and admin-scope
// grant each provides, plus the full admin-scope vocabulary — so a GUI can render
// a permissions editor without reverse-engineering the authorization engine.
func TestAdminRolesEnumeration(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/roles", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("roles status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Roles []struct {
			Name                 string   `json:"name"`
			Description          string   `json:"description"`
			InferenceActions     []string `json:"inference_actions"`
			ModelScope           string   `json:"model_scope"`
			GrantsAllAdminScopes bool     `json:"grants_all_admin_scopes"`
		} `json:"roles"`
		Scopes []string `json:"scopes"`
	}
	decode(t, rec, &out)

	// The three built-in roles, in privilege order admin → user → read-only.
	if len(out.Roles) != 3 {
		t.Fatalf("roles len = %d, want 3: %+v", len(out.Roles), out.Roles)
	}
	if out.Roles[0].Name != "admin" || out.Roles[1].Name != "user" || out.Roles[2].Name != "read-only" {
		t.Fatalf("role order = [%s %s %s], want [admin user read-only]",
			out.Roles[0].Name, out.Roles[1].Name, out.Roles[2].Name)
	}

	byName := map[string]struct {
		actions    []string
		modelScope string
		superuser  bool
		desc       string
	}{}
	for _, r := range out.Roles {
		byName[r.Name] = struct {
			actions    []string
			modelScope string
			superuser  bool
			desc       string
		}{r.InferenceActions, r.ModelScope, r.GrantsAllAdminScopes, r.Description}
	}

	// admin: pull/load/infer on ALL models, grants every admin scope.
	if a := byName["admin"]; !equalStr(a.actions, []string{"pull", "load", "infer"}) ||
		a.modelScope != "all" || !a.superuser || a.desc == "" {
		t.Errorf("admin role wrong: %+v", a)
	}
	// user: pull/load/infer on allow-listed models, no implicit admin scopes.
	if u := byName["user"]; !equalStr(u.actions, []string{"pull", "load", "infer"}) ||
		u.modelScope != "allow-listed" || u.superuser {
		t.Errorf("user role wrong: %+v", u)
	}
	// read-only: infer only, allow-listed, no implicit admin scopes.
	if ro := byName["read-only"]; !equalStr(ro.actions, []string{"infer"}) ||
		ro.modelScope != "allow-listed" || ro.superuser {
		t.Errorf("read-only role wrong: %+v", ro)
	}

	// The full admin-scope vocabulary (14 scopes) is returned for the scope picker,
	// and it matches authz.AllScopes exactly (so the editor can never drift).
	if !equalStr(out.Scopes, authz.AllScopes()) {
		t.Fatalf("scopes = %v, want %v", out.Scopes, authz.AllScopes())
	}
	if len(out.Scopes) != 14 {
		t.Fatalf("scopes len = %d, want 14", len(out.Scopes))
	}
}

// TestAdminRolesScopeGate proves AC1's gating for the new route: a key holding
// keys:read passes (200), a key holding only an unrelated scope is 403, and an
// unauthenticated request is 401. (The broader matrix is covered by
// TestScopedKeyMatrix; this pins the specific 200/403/401 contract.)
func TestAdminRolesScopeGate(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	// keys:read holder → 200.
	reader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	if rec := req(t, s, http.MethodGet, "/v1/admin/roles", reader, ""); rec.Code != http.StatusOK {
		t.Errorf("keys:read holder status = %d, want 200", rec.Code)
	}

	// A holder of an unrelated scope (workers:read) → 403 with code "forbidden".
	other := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	rec := req(t, s, http.MethodGet, "/v1/admin/roles", other, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("workers:read holder status = %d, want 403", rec.Code)
	}
	if code := errorCode(t, rec); code != "forbidden" {
		t.Errorf("403 code = %q, want forbidden", code)
	}

	// Unauthenticated → 401.
	if rec := req(t, s, http.MethodGet, "/v1/admin/roles", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
}

// TestSetPermissionsRejectsUnknownRole proves AC2: an invalid role name on the
// permissions PUT is rejected with 400 and code "invalid_request_error", and
// because validation runs before any mutation NOTHING is applied — the key's
// permissions are unchanged after the rejected call.
func TestSetPermissionsRejectsUnknownRole(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	// Seed a key with a known starting permission set.
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"t","roles":["user"],"allow_models":["llama3"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d, want 201", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)

	before, err := authSvc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get seeded key: %v", err)
	}

	// A full-replace PUT carrying an unknown role is rejected 400.
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+created.ID+"/permissions", adminToken,
		`{"roles":["user","superuser"],"allow_models":["mistral"],"deny_models":["secret"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_request_error" {
		t.Errorf("unknown role code = %q, want invalid_request_error", code)
	}

	// Nothing was applied: roles/allow/deny are exactly as before the rejected call.
	after, err := authSvc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get key after rejected PUT: %v", err)
	}
	if !equalStr(after.Roles, before.Roles) || !equalStr(after.AllowModels, before.AllowModels) ||
		!equalStr(after.DenyModels, before.DenyModels) {
		t.Fatalf("rejected PUT mutated the key: before roles=%v allow=%v deny=%v, after roles=%v allow=%v deny=%v",
			before.Roles, before.AllowModels, before.DenyModels,
			after.Roles, after.AllowModels, after.DenyModels)
	}
}

// TestCreateKeyRejectsUnknownRole proves the create path validates roles too: an
// unknown role on POST /v1/admin/keys is rejected 400 and no key is created.
func TestCreateKeyRejectsUnknownRole(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"bad","roles":["owner"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role on create status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_request_error" {
		t.Errorf("code = %q, want invalid_request_error", code)
	}

	// No key was created (only the admin key the test minted exists).
	keys, err := authSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("a key was created despite the invalid role: %d keys exist", len(keys))
	}
}

// TestSetPermissionsValidReplaceSucceeds proves AC2's success path: a PUT carrying
// valid roles + admin scopes + allow/deny lists is accepted (200) and full-replaces
// every dimension on the key.
func TestSetPermissionsValidReplaceSucceeds(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"t","roles":["user"]}`)
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)

	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+created.ID+"/permissions", adminToken,
		`{"roles":["read-only","user"],"admin_scopes":["keys:read","workers:write"],"allow_models":["llama3","mistral"],"deny_models":["secret"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid replace status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := authSvc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get updated key: %v", err)
	}
	if !equalStr(got.Roles, []string{"read-only", "user"}) ||
		!equalStr(got.AdminScopes, []string{"keys:read", "workers:write"}) ||
		!equalStr(got.AllowModels, []string{"llama3", "mistral"}) ||
		!equalStr(got.DenyModels, []string{"secret"}) {
		t.Fatalf("permissions not fully replaced: %+v", got)
	}
}

// TestSetPermissionsTakesEffectImmediately proves AC3: a permissions change made
// via the admin PUT is reflected by dispatch-time authorization and the
// permission-filtered catalog on the very next request — no restart — because the
// authorizer reads the key fresh from the store on every check. It drives the
// change through the real routed handler and observes the effect two ways: a
// direct authz.Authorize decision and the permission-filtered /models catalog.
func TestSetPermissionsTakesEffectImmediately(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		onlineWorker("w1", types.Model{Name: "llama3"}, types.Model{Name: "mistral"}),
	}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	// A real authorizer sharing the same auth store the handler mutates, so a fresh
	// Get inside Authorize observes the change. A discarding logger keeps it quiet.
	az := authz.NewAuthorizer(authz.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	// Create a target key that, initially, may infer llama3 (user + allow-list).
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"svc","roles":["user"],"allow_models":["llama3"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)

	key, err := authSvc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	// Baseline: llama3 is permitted for inference.
	if err := az.Authorize(context.Background(), key, "llama3", authz.Infer); err != nil {
		t.Fatalf("baseline: llama3 should be allowed, got %v", err)
	}

	// Flip permissions via the admin PUT: deny llama3 (deny-wins), allow mistral.
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+created.ID+"/permissions", adminToken,
		`{"roles":["user"],"allow_models":["mistral"],"deny_models":["llama3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set permissions status = %d, want 200", rec.Code)
	}

	// Immediately re-read the key and re-authorize: the change is in effect.
	key, err = authSvc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get key after PUT: %v", err)
	}
	if err := az.Authorize(context.Background(), key, "llama3", authz.Infer); err == nil {
		t.Fatal("after PUT: llama3 should now be DENIED, but Authorize allowed it")
	}
	if err := az.Authorize(context.Background(), key, "mistral", authz.Infer); err != nil {
		t.Fatalf("after PUT: mistral should now be allowed, got %v", err)
	}

	// And the permission-filtered catalog reflects it on the next request too: the
	// key now sees only mistral (llama3 is deny-listed). This exercises the same
	// fresh-read path the dispatch authz uses, end to end through the routed handler.
	keyToken := mintToken(t, s, authSvc, adminToken, created.ID)
	got := decodeModels(t, do(t, s, "/models", keyToken))
	if len(got.Models) != 1 || got.Models[0].Name != "mistral" {
		t.Fatalf("after PUT the catalog should show only mistral, got %+v", got.Models)
	}
}

// mintToken rotates the target key so the test obtains a usable bearer token for
// it (CreateWithPermissions returns the token only at create time, and the target
// key above was created through the HTTP handler). Rotating yields a fresh token
// without changing the key's permissions, which is exactly what the catalog check
// needs.
func mintToken(t *testing.T, s *Server, _ *auth.Service, adminToken, id string) string {
	t.Helper()
	rec := req(t, s, http.MethodPost, "/v1/admin/keys/"+id+"/rotate", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate to mint token status = %d, want 200", rec.Code)
	}
	var rotated struct {
		Token string `json:"token"`
	}
	decode(t, rec, &rotated)
	return rotated.Token
}

// equalStr reports whether two string slices are equal element-for-element. It is
// a local helper so the role/permission assertions stay dependency-free.
func equalStr(a, b []string) bool {
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
