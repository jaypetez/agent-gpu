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
