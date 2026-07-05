package main

import (
	"net/http"
	"strings"
	"testing"
)

// usersKeysBody is a canned GET /v1/admin/keys list with mixed owner/team labels
// and a revoked key, used to exercise the client-side grouping.
const usersKeysBody = `{"data":[
	{"id":"k1","name":"a","owner":"alice","team":"platform","roles":["admin"],"admin_scopes":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":1,"created":1},
	{"id":"k2","name":"b","owner":"alice","team":"data","roles":["user"],"admin_scopes":[],"allow_models":[],"deny_models":[],"revoked":true,"usage_count":2,"created":2},
	{"id":"k3","name":"c","owner":"bob","team":"platform","roles":["user","read-only"],"admin_scopes":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":3,"created":3},
	{"id":"k4","name":"d","roles":["user"],"admin_scopes":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":4,"created":4}
],"pagination":{"next_cursor":null,"has_more":false}}`

// TestUsersByOwner proves `users` (default --by owner) groups keys by owner label,
// counts total/active, unions roles, and buckets the unlabeled key under
// "(unassigned)".
func TestUsersByOwner(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/keys": {http.StatusOK, usersKeysBody},
	})

	out, err := runHTTP(t, a, runUsersCmd)
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	for _, want := range []string{"OWNER", "KEYS", "ACTIVE", "ROLES", "alice", "bob", "(unassigned)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("users missing %q: %q", want, out)
		}
	}
	// alice has 2 keys but only 1 active (k2 is revoked); her roles are admin+user.
	aliceLine := lineContaining(t, out, "alice")
	if !strings.Contains(aliceLine, "admin,user") {
		t.Fatalf("alice roles should union admin,user: %q", aliceLine)
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/v1/admin/keys" {
		t.Fatalf("sent %s %s, want GET /v1/admin/keys", a.lastReq.method, a.lastReq.path)
	}
}

// TestUsersByTeam proves --by team groups by the team label instead.
func TestUsersByTeam(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/keys": {http.StatusOK, usersKeysBody},
	})

	out, err := runHTTP(t, a, runUsersCmd, "--by", "team")
	if err != nil {
		t.Fatalf("users --by team: %v", err)
	}
	for _, want := range []string{"TEAM", "platform", "data", "(unassigned)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("users --by team missing %q: %q", want, out)
		}
	}
}

// TestUsersBadDimension proves an unsupported --by value is a usage error before any
// request.
func TestUsersBadDimension(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	_, err := runHTTP(t, a, runUsersCmd, "--by", "department")
	if err == nil {
		t.Fatal("expected a usage error for an unknown --by dimension")
	}
	if got := exitCode(err); got != exitUsage {
		t.Fatalf("exit = %d, want %d (err: %v)", got, exitUsage, err)
	}
}

// TestUsersEmpty proves a fleet with no keys prints the no-keys notice.
func TestUsersEmpty(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/keys": {http.StatusOK, `{"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})
	out, err := runHTTP(t, a, runUsersCmd)
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	if !strings.Contains(out, "No keys.") {
		t.Fatalf("empty notice missing: %q", out)
	}
}

// TestUsersForbidden proves a 403 (a token lacking keys:read) maps to the auth exit
// code.
func TestUsersForbidden(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/keys": {http.StatusForbidden, `{"error":{"message":"insufficient scope: keys:read","code":"forbidden"}}`},
	})
	_, err := runHTTP(t, a, runUsersCmd)
	if exitCode(err) != exitAuth {
		t.Fatalf("forbidden exit = %d, want %d (err: %v)", exitCode(err), exitAuth, err)
	}
}

// lineContaining returns the first output line containing sub, failing if none
// does. It lets a table test assert per-row content without parsing columns.
func lineContaining(t *testing.T, out, sub string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", sub, out)
	return ""
}
