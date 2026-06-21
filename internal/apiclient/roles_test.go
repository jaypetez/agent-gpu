package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestListRoles proves the client decodes the GET /v1/admin/roles catalog into
// the typed RolesCatalog and sends the request to the right path. It uses the
// shared recordingHandler so the exact request the client makes is asserted
// alongside the decoded response.
func TestListRoles(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, `{
		"roles":[
			{"name":"admin","description":"Superuser.","inference_actions":["pull","load","infer"],"model_scope":"all","grants_all_admin_scopes":true},
			{"name":"user","description":"Pull/load/infer on allow-listed models.","inference_actions":["pull","load","infer"],"model_scope":"allow-listed","grants_all_admin_scopes":false},
			{"name":"read-only","description":"Infer only.","inference_actions":["infer"],"model_scope":"allow-listed","grants_all_admin_scopes":false}
		],
		"scopes":["audit:read","audit:write","config:read","config:write","keys:read","keys:write","logs:read","logs:write","models:read","models:write","telemetry:read","telemetry:write","workers:read","workers:write"]
	}`))
	defer srv.Close()

	got, err := newTestClient(t, srv).ListRoles(context.Background())
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}

	if cap.method != http.MethodGet || cap.path != "/v1/admin/roles" {
		t.Fatalf("sent %s %s, want GET /v1/admin/roles", cap.method, cap.path)
	}

	if len(got.Roles) != 3 {
		t.Fatalf("roles len = %d, want 3", len(got.Roles))
	}
	admin := got.Roles[0]
	if admin.Name != "admin" || admin.ModelScope != "all" || !admin.GrantsAllAdminScopes ||
		len(admin.InferenceActions) != 3 || admin.InferenceActions[0] != "pull" {
		t.Errorf("admin role decoded wrong: %+v", admin)
	}
	ro := got.Roles[2]
	if ro.Name != "read-only" || ro.ModelScope != "allow-listed" || ro.GrantsAllAdminScopes ||
		len(ro.InferenceActions) != 1 || ro.InferenceActions[0] != "infer" {
		t.Errorf("read-only role decoded wrong: %+v", ro)
	}

	if len(got.Scopes) != 14 {
		t.Fatalf("scopes len = %d, want 14", len(got.Scopes))
	}
	if got.Scopes[0] != "audit:read" {
		t.Errorf("scopes[0] = %q, want audit:read", got.Scopes[0])
	}
}

// TestListRolesForbidden proves the client maps a 403 (a token lacking keys:read)
// to the typed ErrForbidden sentinel.
func TestListRolesForbidden(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient scope","code":"forbidden"}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).ListRoles(context.Background()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListRoles err = %v, want ErrForbidden", err)
	}
}
