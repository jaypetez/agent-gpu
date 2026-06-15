package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// TestSetLimitsPersists verifies per-key quota limits are persisted and visible
// on a fresh authenticated read (no restart needed).
func TestSetLimitsPersists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	token, key, err := svc.Create(ctx, "agent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.Limits != nil {
		t.Fatalf("new key should have nil Limits, got %+v", key.Limits)
	}

	updated, err := svc.SetLimits(ctx, key.ID, &store.Limits{RPM: 60, TPM: 1000, DailyTokens: 100000, MonthlyTokens: 3000000})
	if err != nil {
		t.Fatalf("set limits: %v", err)
	}
	if updated.Limits == nil || updated.Limits.RPM != 60 || updated.Limits.TPM != 1000 {
		t.Fatalf("limits not applied: %+v", updated.Limits)
	}

	authed, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authed.Limits == nil || authed.Limits.DailyTokens != 100000 || authed.Limits.MonthlyTokens != 3000000 {
		t.Fatalf("authenticated limits = %+v, want the stored override", authed.Limits)
	}
}

// TestSetLimitsClears verifies passing nil clears the per-key override so the
// key falls back to global defaults.
func TestSetLimitsClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	_, key, err := svc.Create(ctx, "agent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.SetLimits(ctx, key.ID, &store.Limits{RPM: 10}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := svc.SetLimits(ctx, key.ID, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got.Limits != nil {
		t.Fatalf("expected nil Limits after clear, got %+v", got.Limits)
	}
}

// TestSetLimitsUnknownKey verifies an unknown id returns ErrNotFound.
func TestSetLimitsUnknownKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)
	if _, err := svc.SetLimits(ctx, "deadbeef", &store.Limits{RPM: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSetLimitsPreservesPermissions verifies setting limits does not disturb
// the key's roles/allow/deny lists.
func TestSetLimitsPreservesPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	_, key, err := svc.CreateWithPermissions(ctx, "agent", Permissions{Roles: []string{"user"}, AllowModels: []string{"llama3"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.SetLimits(ctx, key.ID, &store.Limits{RPM: 5})
	if err != nil {
		t.Fatalf("set limits: %v", err)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "user" {
		t.Fatalf("roles disturbed: %v", got.Roles)
	}
	if len(got.AllowModels) != 1 || got.AllowModels[0] != "llama3" {
		t.Fatalf("allow disturbed: %v", got.AllowModels)
	}
}
