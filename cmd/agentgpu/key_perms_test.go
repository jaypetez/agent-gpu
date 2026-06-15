package main

import (
	"bytes"
	"context"
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
		full := append(args, "--store", path)
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

// TestKeyPermsSubcommand verifies `key perms <id>` replaces a key's permissions,
// including the trailing-positional flag ordering.
func TestKeyPermsSubcommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		full := append(args, "--store", path)
		if err := runKeyCmd(ctx, &out, full); err != nil {
			t.Fatalf("runKeyCmd %v: %v", args, err)
		}
		return out.String()
	}

	created := run("create", "--name", "agent", "--role", "admin")
	id := strings.SplitN(extractToken(t, created), "_", 3)[1]

	// id positional BEFORE the flags exercises reorderFlagsFirst.
	out := run("perms", id, "--role", "read-only", "--allow-model", "llama3")
	if !strings.Contains(out, "Updated permissions for key "+id) {
		t.Fatalf("perms output: %q", out)
	}
	if !strings.Contains(out, "Roles: read-only") || !strings.Contains(out, "Allow: llama3") {
		t.Fatalf("perms did not echo new perms: %q", out)
	}

	list := run("list")
	if strings.Contains(list, "admin") {
		t.Fatalf("perms should have replaced admin role: %q", list)
	}
	if !strings.Contains(list, "read-only") {
		t.Fatalf("list missing replaced role: %q", list)
	}
}

// TestKeyPermsRequiresID verifies the perms subcommand rejects a missing id.
func TestKeyPermsRequiresID(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runKeyCmd(context.Background(), &out, []string{"perms", "--role", "user", "--store", filepath.Join(t.TempDir(), "k.json")})
	if err == nil {
		t.Fatal("expected error when id omitted")
	}
}
