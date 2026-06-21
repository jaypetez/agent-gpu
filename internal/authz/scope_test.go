package authz

import (
	"testing"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// TestHasScopeMatrix is the table-driven proof of the scoped-RBAC decision (AC1):
// RoleAdmin is a superuser that holds every scope; a key with a specific scope
// holds exactly that scope and no other; a key with neither the admin role nor
// the scope is denied.
func TestHasScopeMatrix(t *testing.T) {
	admin := store.APIKey{Roles: []string{RoleAdmin}}
	keysReader := store.APIKey{AdminScopes: []string{ScopeKeysRead}}
	workersWriter := store.APIKey{AdminScopes: []string{ScopeWorkersWrite}}
	multi := store.APIKey{AdminScopes: []string{ScopeKeysRead, ScopeTelemetryRead}}
	none := store.APIKey{Roles: []string{RoleUser}} // a valid non-admin key, no scopes
	adminPlusScope := store.APIKey{Roles: []string{RoleAdmin}, AdminScopes: []string{ScopeKeysRead}}

	cases := []struct {
		name  string
		key   store.APIKey
		scope string
		want  bool
	}{
		// Superuser: admin holds every scope.
		{"admin holds keys:read", admin, ScopeKeysRead, true},
		{"admin holds keys:write", admin, ScopeKeysWrite, true},
		{"admin holds workers:write", admin, ScopeWorkersWrite, true},
		{"admin holds audit:read", admin, ScopeAuditRead, true},
		{"admin holds telemetry:read", admin, ScopeTelemetryRead, true},

		// Specific scope: holds exactly its scope.
		{"keys-reader holds keys:read", keysReader, ScopeKeysRead, true},
		{"keys-reader lacks keys:write", keysReader, ScopeKeysWrite, false},
		{"keys-reader lacks workers:read", keysReader, ScopeWorkersRead, false},
		{"workers-writer holds workers:write", workersWriter, ScopeWorkersWrite, true},
		{"workers-writer lacks workers:read", workersWriter, ScopeWorkersRead, false},
		{"workers-writer lacks keys:write", workersWriter, ScopeKeysWrite, false},

		// Multiple scopes.
		{"multi holds keys:read", multi, ScopeKeysRead, true},
		{"multi holds telemetry:read", multi, ScopeTelemetryRead, true},
		{"multi lacks keys:write", multi, ScopeKeysWrite, false},

		// No scope at all → denied everywhere.
		{"none lacks keys:read", none, ScopeKeysRead, false},
		{"none lacks workers:write", none, ScopeWorkersWrite, false},

		// Admin role still wins even with an unrelated explicit scope present.
		{"admin+scope holds unrelated scope", adminPlusScope, ScopeWorkersWrite, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasScope(c.key, c.scope); got != c.want {
				t.Errorf("HasScope(%+v, %q) = %v, want %v", c.key, c.scope, got, c.want)
			}
		})
	}
}

// TestValidScope proves the scope vocabulary is enforced: every defined scope is
// valid, and an unknown string is not.
func TestValidScope(t *testing.T) {
	for _, s := range AllScopes() {
		if !ValidScope(s) {
			t.Errorf("AllScopes returned %q but ValidScope says it is invalid", s)
		}
	}
	for _, bad := range []string{"", "keys", "keys:delete", "config:admin", "bogus:read"} {
		if ValidScope(bad) {
			t.Errorf("ValidScope(%q) = true, want false", bad)
		}
	}
}

// TestAllScopesComplete proves the resource×operation matrix is fully populated
// (each of the seven resources has both a read and a write scope), so a route
// can always find a matching scope.
func TestAllScopesComplete(t *testing.T) {
	want := []string{
		ScopeConfigRead, ScopeConfigWrite,
		ScopeWorkersRead, ScopeWorkersWrite,
		ScopeModelsRead, ScopeModelsWrite,
		ScopeKeysRead, ScopeKeysWrite,
		ScopeLogsRead, ScopeLogsWrite,
		ScopeTelemetryRead, ScopeTelemetryWrite,
		ScopeAuditRead, ScopeAuditWrite,
	}
	got := AllScopes()
	if len(got) != len(want) {
		t.Fatalf("AllScopes len = %d, want %d: %v", len(got), len(want), got)
	}
	set := make(map[string]bool, len(got))
	for _, s := range got {
		set[s] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("AllScopes missing %q", w)
		}
	}
}
