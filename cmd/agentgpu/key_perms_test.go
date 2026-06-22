package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyCreateWithPerms verifies create accepts repeatable role/allow/deny
// flags and that list renders them without leaking the secret.
func TestKeyCreateWithPerms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		full := append(args, "--local", "--store", path)
		if err := runKeyCmd(ctx, &out, full); err != nil {
			t.Fatalf("runKeyCmd %v: %v", args, err)
		}
		return out.String()
	}

	created := run("create", "--name", "perm-agent",
		"--role", "user", "--allow-model", "llama3", "--allow-model", "mistral", "--deny-model", "secret-model")
	token := extractToken(t, created)
	id := strings.SplitN(token, "_", 3)[1]
	if !strings.Contains(created, "Roles: user") {
		t.Fatalf("create did not echo roles: %q", created)
	}

	list := run("list")
	if !strings.Contains(list, "user") || !strings.Contains(list, "llama3") ||
		!strings.Contains(list, "mistral") || !strings.Contains(list, "secret-model") {
		t.Fatalf("list missing perms: %q", list)
	}
	if secret := strings.SplitN(token, "_", 3)[2]; strings.Contains(list, secret) {
		t.Fatal("list leaked the secret")
	}
	_ = id
}

// TestKeyPermsSubcommandHTTP verifies `key perms <id>` replaces a key's permissions
// (a full replace — the old admin role is gone), including the id-before-flags
// ordering that exercises reorderFlagsFirst. Permission changes are a runtime
// mutation, so under the #104 invariant they go over the admin API; the request is
// asserted against the stub.
func TestKeyPermsSubcommandHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/keys/k1/permissions": {http.StatusOK,
			`{"id":"k1","name":"agent","roles":["read-only"],"allow_models":["llama3"],"deny_models":[],"revoked":false,"usage_count":0,"created":1}`},
	})

	// id positional BEFORE the flags exercises reorderFlagsFirst.
	out, err := runHTTP(t, a, runKeyCmd, "perms", "k1", "--role", "read-only", "--allow-model", "llama3")
	if err != nil {
		t.Fatalf("key perms: %v", err)
	}
	if !strings.Contains(out, "Updated permissions for key k1") {
		t.Fatalf("perms output: %q", out)
	}
	if !strings.Contains(out, "Roles: read-only") || !strings.Contains(out, "Allow: llama3") {
		t.Fatalf("perms did not echo new perms: %q", out)
	}
	if strings.Contains(out, "admin") {
		t.Fatalf("perms is a full replace; the admin role should be gone: %q", out)
	}
	// The replace was sent as a PUT carrying exactly the new role and allow list.
	if a.lastReq.method != http.MethodPut || a.lastReq.path != "/v1/admin/keys/k1/permissions" {
		t.Fatalf("sent %s %s", a.lastReq.method, a.lastReq.path)
	}
	roles, _ := a.lastReq.body["roles"].([]any)
	if len(roles) != 1 || roles[0] != "read-only" {
		t.Fatalf("body roles = %v, want [read-only]", a.lastReq.body["roles"])
	}
}

// TestKeyPermsRequiresID verifies the perms subcommand rejects a missing id.
func TestKeyPermsRequiresID(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runKeyCmd(context.Background(), &out, []string{"perms", "--role", "user", "--local", "--store", filepath.Join(t.TempDir(), "k.json")})
	if err == nil {
		t.Fatal("expected error when id omitted")
	}
}
