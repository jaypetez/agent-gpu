package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// newTestAuthorizer returns an Authorizer that captures its audit output into
// buf so tests can assert on the structured records.
func newTestAuthorizer(t *testing.T) (*Authorizer, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return NewAuthorizer(WithLogger(log)), &buf
}

// TestAuthorizeMatrix exercises the full role × action × allow/deny precedence
// ladder, covering allow, deny, and role-based paths (AC2, AC5).
func TestAuthorizeMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		key     store.APIKey
		model   string
		action  Action
		allowed bool
	}{
		{
			name:    "deny-list wins over admin",
			key:     store.APIKey{Roles: []string{RoleAdmin}, DenyModels: []string{"llama3"}},
			model:   "llama3",
			action:  Infer,
			allowed: false,
		},
		{
			name:    "deny-list wins over allow-list",
			key:     store.APIKey{Roles: []string{RoleUser}, AllowModels: []string{"llama3"}, DenyModels: []string{"llama3"}},
			model:   "llama3",
			action:  Infer,
			allowed: false,
		},
		{
			name:    "admin allowed on any model and action",
			key:     store.APIKey{Roles: []string{RoleAdmin}},
			model:   "anything",
			action:  Pull,
			allowed: true,
		},
		{
			name:    "admin allowed infer with no lists",
			key:     store.APIKey{Roles: []string{RoleAdmin}},
			model:   "anything",
			action:  Infer,
			allowed: true,
		},
		{
			name:    "read-only cannot pull even on allowed model",
			key:     store.APIKey{Roles: []string{RoleReadOnly}, AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Pull,
			allowed: false,
		},
		{
			name:    "read-only cannot load even on allowed model",
			key:     store.APIKey{Roles: []string{RoleReadOnly}, AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Load,
			allowed: false,
		},
		{
			name:    "read-only may infer on allowed model",
			key:     store.APIKey{Roles: []string{RoleReadOnly}, AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Infer,
			allowed: true,
		},
		{
			name:    "user may pull allowed model",
			key:     store.APIKey{Roles: []string{RoleUser}, AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Pull,
			allowed: true,
		},
		{
			name:    "user may load allowed model",
			key:     store.APIKey{Roles: []string{RoleUser}, AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Load,
			allowed: true,
		},
		{
			name:    "user may infer allowed model",
			key:     store.APIKey{Roles: []string{RoleUser}, AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Infer,
			allowed: true,
		},
		{
			name:    "user denied model not on allow-list",
			key:     store.APIKey{Roles: []string{RoleUser}, AllowModels: []string{"llama3"}},
			model:   "mistral",
			action:  Infer,
			allowed: false,
		},
		{
			name:    "allow-list with no granting role is denied",
			key:     store.APIKey{AllowModels: []string{"llama3"}},
			model:   "llama3",
			action:  Infer,
			allowed: false,
		},
		{
			name:    "empty key denied by default",
			key:     store.APIKey{},
			model:   "llama3",
			action:  Infer,
			allowed: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newTestAuthorizer(t)
			err := a.Authorize(context.Background(), tc.key, tc.model, tc.action)
			if tc.allowed && err != nil {
				t.Fatalf("expected allow, got %v", err)
			}
			if !tc.allowed {
				if err == nil {
					t.Fatal("expected ErrForbidden, got nil")
				}
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("expected ErrForbidden, got %v", err)
				}
			}
		})
	}
}

// TestAuthorizeDeniesByDefault covers AC1: a key with no access is denied with
// ErrForbidden on every action.
func TestAuthorizeDeniesByDefault(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthorizer(t)
	key := store.APIKey{ID: "k1"}
	for _, action := range []Action{Pull, Load, Infer} {
		if err := a.Authorize(context.Background(), key, "llama3", action); !errors.Is(err, ErrForbidden) {
			t.Fatalf("action %v: expected ErrForbidden, got %v", action, err)
		}
	}
}

// auditRecord is the subset of fields we assert on in the audit log.
type auditRecord struct {
	Result string `json:"result"`
	KeyID  string `json:"key_id"`
	Model  string `json:"model"`
	Op     string `json:"op"`
	Reason string `json:"reason"`
	Role   string `json:"role"`
	Level  string `json:"level"`
}

func lastRecord(t *testing.T, buf *bytes.Buffer) auditRecord {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no audit records written")
	}
	var rec auditRecord
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("parse audit record %q: %v", lines[len(lines)-1], err)
	}
	return rec
}

// TestAuditGranted covers AC4: a granted decision is logged with the expected
// fields at Info, and no secret material leaks.
func TestAuditGranted(t *testing.T) {
	t.Parallel()
	a, buf := newTestAuthorizer(t)
	key := store.APIKey{
		ID:          "key-123",
		SecretHash:  []byte("super-secret-hash"),
		Salt:        []byte("salty"),
		Roles:       []string{RoleUser},
		AllowModels: []string{"llama3"},
	}
	if err := a.Authorize(context.Background(), key, "llama3", Infer); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	rec := lastRecord(t, buf)
	if rec.Result != "granted" {
		t.Fatalf("result = %q, want granted", rec.Result)
	}
	if rec.Level != "INFO" {
		t.Fatalf("level = %q, want INFO", rec.Level)
	}
	if rec.KeyID != "key-123" || rec.Model != "llama3" || rec.Op != "infer" {
		t.Fatalf("unexpected fields: %+v", rec)
	}
	if rec.Role != RoleUser {
		t.Fatalf("role = %q, want %q", rec.Role, RoleUser)
	}
	if out := buf.String(); strings.Contains(out, "super-secret-hash") || strings.Contains(out, "salty") {
		t.Fatalf("audit log leaked secret material: %s", out)
	}
}

// TestAuditDenied covers AC4: a denied decision is logged at Warn with a reason.
func TestAuditDenied(t *testing.T) {
	t.Parallel()
	a, buf := newTestAuthorizer(t)
	key := store.APIKey{ID: "key-999"}
	if err := a.Authorize(context.Background(), key, "mistral", Pull); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}

	rec := lastRecord(t, buf)
	if rec.Result != "denied" {
		t.Fatalf("result = %q, want denied", rec.Result)
	}
	if rec.Level != "WARN" {
		t.Fatalf("level = %q, want WARN", rec.Level)
	}
	if rec.KeyID != "key-999" || rec.Model != "mistral" || rec.Op != "pull" {
		t.Fatalf("unexpected fields: %+v", rec)
	}
	if rec.Reason == "" {
		t.Fatal("denied record missing reason")
	}
}

// TestDefaultLogger ensures NewAuthorizer works without WithLogger (falls back
// to slog.Default()) and a nil logger is ignored.
func TestDefaultLogger(t *testing.T) {
	t.Parallel()
	a := NewAuthorizer(WithLogger(nil))
	if a.log == nil {
		t.Fatal("nil WithLogger should leave default logger intact")
	}
	if err := a.Authorize(context.Background(), store.APIKey{Roles: []string{RoleAdmin}}, "m", Infer); err != nil {
		t.Fatalf("admin should be allowed: %v", err)
	}
}

// TestAllRoles proves the role enumeration describes exactly the three built-in
// roles, in privilege order, with the inference actions, model scope, and
// admin-scope grant each role's decide() behavior implies — the contract a
// permissions editor GUI renders against.
func TestAllRoles(t *testing.T) {
	t.Parallel()
	roles := AllRoles()
	if len(roles) != 3 {
		t.Fatalf("AllRoles len = %d, want 3", len(roles))
	}

	// Deterministic privilege order: admin → user → read-only.
	if roles[0].Name != RoleAdmin || roles[1].Name != RoleUser || roles[2].Name != RoleReadOnly {
		t.Fatalf("AllRoles order = [%s %s %s], want [admin user read-only]",
			roles[0].Name, roles[1].Name, roles[2].Name)
	}

	byName := map[string]RoleInfo{}
	for _, r := range roles {
		byName[r.Name] = r
		if r.Description == "" {
			t.Errorf("role %q has empty description", r.Name)
		}
	}

	// admin: pull/load/infer on ALL models, and every admin scope.
	admin := byName[RoleAdmin]
	if got, want := admin.InferenceActions, []string{"pull", "load", "infer"}; !equalStrings(got, want) {
		t.Errorf("admin actions = %v, want %v", got, want)
	}
	if admin.ModelScope != ModelScopeAll {
		t.Errorf("admin model_scope = %q, want %q", admin.ModelScope, ModelScopeAll)
	}
	if !admin.GrantsAllAdminScopes {
		t.Error("admin should grant all admin scopes")
	}

	// user: pull/load/infer on allow-listed models, no implicit admin scopes.
	user := byName[RoleUser]
	if got, want := user.InferenceActions, []string{"pull", "load", "infer"}; !equalStrings(got, want) {
		t.Errorf("user actions = %v, want %v", got, want)
	}
	if user.ModelScope != ModelScopeAllowListed {
		t.Errorf("user model_scope = %q, want %q", user.ModelScope, ModelScopeAllowListed)
	}
	if user.GrantsAllAdminScopes {
		t.Error("user should NOT grant all admin scopes")
	}

	// read-only: infer ONLY, on allow-listed models, no implicit admin scopes.
	ro := byName[RoleReadOnly]
	if got, want := ro.InferenceActions, []string{"infer"}; !equalStrings(got, want) {
		t.Errorf("read-only actions = %v, want %v", got, want)
	}
	if ro.ModelScope != ModelScopeAllowListed {
		t.Errorf("read-only model_scope = %q, want %q", ro.ModelScope, ModelScopeAllowListed)
	}
	if ro.GrantsAllAdminScopes {
		t.Error("read-only should NOT grant all admin scopes")
	}
}

// TestAllRolesIsACopy proves AllRoles hands back a fresh slice each call so a
// caller mutating the result cannot corrupt the shared role definitions.
func TestAllRolesIsACopy(t *testing.T) {
	t.Parallel()
	first := AllRoles()
	first[0].Name = "tampered"
	first[0].InferenceActions[0] = "tampered"
	second := AllRoles()
	if second[0].Name != RoleAdmin {
		t.Fatalf("mutating the returned slice leaked into AllRoles: %q", second[0].Name)
	}
}

// TestValidRole proves every built-in role name validates and an unknown string
// (including a near-miss and the empty string) does not — the seam the
// permissions API rejects unknown roles at.
func TestValidRole(t *testing.T) {
	t.Parallel()
	for _, name := range []string{RoleAdmin, RoleUser, RoleReadOnly} {
		if !ValidRole(name) {
			t.Errorf("ValidRole(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "superuser", "Admin", "readonly", "read_only", "owner"} {
		if ValidRole(name) {
			t.Errorf("ValidRole(%q) = true, want false", name)
		}
	}
}

// TestAllRolesValidateAndCoverDecide cross-checks the enumeration against the
// live decide() ladder: every enumerated role validates, and for each its
// declared InferenceActions are exactly the actions decide() grants an
// allow-listed model (so the GUI's matrix can never drift from enforcement).
func TestAllRolesValidateAndCoverDecide(t *testing.T) {
	t.Parallel()
	for _, ri := range AllRoles() {
		if !ValidRole(ri.Name) {
			t.Errorf("enumerated role %q does not pass ValidRole", ri.Name)
		}
		// For admin, decide() allows everything regardless of the allow-list; for
		// user/read-only the model must be allow-listed. Use an allow-listed model so
		// the comparison is apples-to-apples across roles.
		key := store.APIKey{Roles: []string{ri.Name}, AllowModels: []string{"m"}}
		var granted []string
		for _, act := range []Action{Pull, Load, Infer} {
			if _, _, ok := decide(key, "m", act); ok {
				granted = append(granted, act.op())
			}
		}
		if !equalStrings(granted, ri.InferenceActions) {
			t.Errorf("role %q: decide() grants %v but RoleInfo declares %v", ri.Name, granted, ri.InferenceActions)
		}
	}
}

// equalStrings reports whether two string slices are equal element-for-element.
func equalStrings(a, b []string) bool {
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
