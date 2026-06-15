package server_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// authzHarness wires a server (with a captured-audit authorizer) to a live
// worker, plus an auth.Service over a shared in-memory store, so a test can run
// the full authenticate -> authorize -> dispatch flow end to end.
type authzHarness struct {
	h     *harness
	auth  *auth.Service
	audit *bytes.Buffer
}

func newAuthzHarness(t *testing.T) *authzHarness {
	t.Helper()
	var audit bytes.Buffer
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewJSONHandler(&audit, &slog.HandlerOptions{Level: slog.LevelInfo}))
	az := authz.NewAuthorizer(authz.WithLogger(log))

	h := &harness{t: t}
	h.srv = server.New(server.WithAuthorizer(az))
	h.start()
	t.Cleanup(h.close)

	// Bring up a worker serving "llama3" and wait for it to register.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := newWorker(h, nil)
	go func() { _ = w.Run(ctx) }()
	waitFor(t, 2*time.Second, "worker to register", func() bool { return h.srv.WorkerCount() == 1 })

	return &authzHarness{h: h, auth: auth.NewService(st), audit: &audit}
}

// authedKey creates a key with the given permissions and returns it freshly
// authenticated (mirroring the request-path: Authenticate -> Authorize).
func (a *authzHarness) authedKey(t *testing.T, perms auth.Permissions) store.APIKey {
	t.Helper()
	ctx := context.Background()
	token, _, err := a.auth.CreateWithPermissions(ctx, "agent", perms)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key, err := a.auth.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return key
}

// TestAuthzDispatchAllow covers AC5: an authenticated, permitted key reaches the
// worker and gets a result, and the decision is audited as granted.
func TestAuthzDispatchAllow(t *testing.T) {
	h := newAuthzHarness(t)
	key := h.authedKey(t, auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{"llama3"}})

	res, err := h.h.srv.SubmitAuthorizedJob(context.Background(), key, types.Job{ID: "j1", Model: "llama3", Prompt: "ping"})
	if err != nil {
		t.Fatalf("SubmitAuthorizedJob: %v", err)
	}
	if res.Output != "echo: ping" {
		t.Fatalf("output = %q", res.Output)
	}
	if !strings.Contains(h.audit.String(), `"result":"granted"`) {
		t.Fatalf("missing granted audit record: %s", h.audit.String())
	}
}

// TestAuthzDispatchDeny covers AC1/AC5: a key without access is rejected with
// ErrForbidden before any job reaches a worker, and the denial is audited.
func TestAuthzDispatchDeny(t *testing.T) {
	h := newAuthzHarness(t)
	key := h.authedKey(t, auth.Permissions{}) // no roles, no models

	_, err := h.h.srv.SubmitAuthorizedJob(context.Background(), key, types.Job{ID: "j2", Model: "llama3", Prompt: "ping"})
	if !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if !strings.Contains(h.audit.String(), `"result":"denied"`) {
		t.Fatalf("missing denied audit record: %s", h.audit.String())
	}
}

// TestAuthzFreshReadNoRestart covers AC3: changing a key's permissions takes
// effect on the next request with no server restart. We grant access, then
// revoke it via SetPermissions, re-authenticate, and confirm the next dispatch
// is now forbidden.
func TestAuthzFreshReadNoRestart(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()

	token, created, err := h.auth.CreateWithPermissions(ctx, "agent",
		auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{"llama3"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// First request: allowed.
	key, err := h.auth.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := h.h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "ok", Model: "llama3", Prompt: "p"}); err != nil {
		t.Fatalf("expected allow before permission change, got %v", err)
	}

	// Revoke model access in place (no restart).
	if _, err := h.auth.SetPermissions(ctx, created.ID, auth.Permissions{Roles: []string{authz.RoleUser}}); err != nil {
		t.Fatalf("set permissions: %v", err)
	}

	// Next request re-reads the key and must now be forbidden.
	key2, err := h.auth.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("re-authenticate: %v", err)
	}
	if _, err := h.h.srv.SubmitAuthorizedJob(ctx, key2, types.Job{ID: "no", Model: "llama3", Prompt: "p"}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("expected ErrForbidden after permission change, got %v", err)
	}
}
