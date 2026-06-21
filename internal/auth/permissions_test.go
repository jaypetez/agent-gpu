package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// TestCreateWithPermissions verifies roles and allow/deny lists are persisted
// at creation and survive a fresh read from the store.
func TestCreateWithPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	perms := Permissions{
		Roles:       []string{"user"},
		AllowModels: []string{"llama3"},
		DenyModels:  []string{"badmodel"},
	}
	_, key, err := svc.CreateWithPermissions(ctx, "perm-agent", perms)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.store.GetAPIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "user" {
		t.Fatalf("roles = %v", got.Roles)
	}
	if len(got.AllowModels) != 1 || got.AllowModels[0] != "llama3" {
		t.Fatalf("allow = %v", got.AllowModels)
	}
	if len(got.DenyModels) != 1 || got.DenyModels[0] != "badmodel" {
		t.Fatalf("deny = %v", got.DenyModels)
	}
}

// TestSetPermissionsReplaces verifies SetPermissions replaces (not merges) the
// lists, preserves identity, and that a subsequent Authenticate returns the new
// permissions with no restart (the fresh-read property authz relies on, AC3).
func TestSetPermissionsReplaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	token, key, err := svc.CreateWithPermissions(ctx, "agent", Permissions{Roles: []string{"user"}, AllowModels: []string{"llama3"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Replace with a read-only role and a different allow-list.
	updated, err := svc.SetPermissions(ctx, key.ID, Permissions{Roles: []string{"read-only"}, AllowModels: []string{"mistral"}})
	if err != nil {
		t.Fatalf("set permissions: %v", err)
	}
	if len(updated.Roles) != 1 || updated.Roles[0] != "read-only" {
		t.Fatalf("roles = %v", updated.Roles)
	}
	if len(updated.AllowModels) != 1 || updated.AllowModels[0] != "mistral" {
		t.Fatalf("allow = %v", updated.AllowModels)
	}

	// Authenticate returns the freshly-stored permissions — no restart needed.
	authed, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if len(authed.Roles) != 1 || authed.Roles[0] != "read-only" {
		t.Fatalf("authenticated roles = %v, want [read-only]", authed.Roles)
	}
	if len(authed.AllowModels) != 1 || authed.AllowModels[0] != "mistral" {
		t.Fatalf("authenticated allow = %v, want [mistral]", authed.AllowModels)
	}
}

// TestSetPermissionsClears verifies passing nil lists clears all access.
func TestSetPermissionsClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	_, key, err := svc.CreateWithPermissions(ctx, "agent", Permissions{Roles: []string{"admin"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.SetPermissions(ctx, key.ID, Permissions{})
	if err != nil {
		t.Fatalf("set permissions: %v", err)
	}
	if len(got.Roles) != 0 || len(got.AllowModels) != 0 || len(got.DenyModels) != 0 {
		t.Fatalf("expected cleared lists, got %+v", got)
	}
}

// TestSetPermissionsUnknownKey verifies an unknown id returns ErrNotFound.
func TestSetPermissionsUnknownKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)
	if _, err := svc.SetPermissions(ctx, "deadbeef", Permissions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestAdminScopesPersist verifies admin scopes (#90) round-trip through create
// and SetPermissions and survive a fresh read from the store, alongside roles.
func TestAdminScopesPersist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	_, key, err := svc.CreateWithPermissions(ctx, "scoped", Permissions{AdminScopes: []string{"keys:read", "workers:write"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.store.GetAPIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.AdminScopes) != 2 || got.AdminScopes[0] != "keys:read" || got.AdminScopes[1] != "workers:write" {
		t.Fatalf("created admin_scopes = %v", got.AdminScopes)
	}

	// SetPermissions replaces the scope set.
	updated, err := svc.SetPermissions(ctx, key.ID, Permissions{AdminScopes: []string{"audit:read"}})
	if err != nil {
		t.Fatalf("set permissions: %v", err)
	}
	if len(updated.AdminScopes) != 1 || updated.AdminScopes[0] != "audit:read" {
		t.Fatalf("replaced admin_scopes = %v", updated.AdminScopes)
	}

	// Clearing drops the scope set entirely.
	cleared, err := svc.SetPermissions(ctx, key.ID, Permissions{Roles: []string{"user"}})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(cleared.AdminScopes) != 0 {
		t.Fatalf("admin_scopes not cleared: %v", cleared.AdminScopes)
	}
}
