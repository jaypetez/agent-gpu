package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyCLIFlow exercises the key subcommand end-to-end against a temp store:
// create -> list -> rotate -> revoke. It also asserts the one-time token notice
// is printed and that list never prints a secret.
func TestKeyCLIFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		// --local exercises the on-disk store (offline bootstrap) path; the default
		// mode targets a running server over HTTP (covered in http_test.go).
		full := append(args, "--local", "--store", path)
		if err := runKeyCmd(ctx, &out, full); err != nil {
			t.Fatalf("runKeyCmd %v: %v", args, err)
		}
		return out.String()
	}

	created := run("create", "--name", "cli-agent")
	if !strings.Contains(created, "Token: agpu_") {
		t.Fatalf("create did not print a token: %q", created)
	}
	if !strings.Contains(created, "will not be shown again") {
		t.Fatalf("create missing one-time warning: %q", created)
	}

	// Pull the id out of the printed token.
	token := extractToken(t, created)
	id := strings.SplitN(token, "_", 3)[1]

	list := run("list")
	if !strings.Contains(list, id) || !strings.Contains(list, "cli-agent") {
		t.Fatalf("list missing key: %q", list)
	}
	secret := strings.SplitN(token, "_", 3)[2]
	if strings.Contains(list, secret) {
		t.Fatal("list leaked the secret")
	}

	rotated := run("rotate", id)
	if !strings.Contains(rotated, "Token: agpu_") {
		t.Fatalf("rotate did not print a token: %q", rotated)
	}
	if extractToken(t, rotated) == token {
		t.Fatal("rotate returned the same token")
	}

	revoked := run("revoke", id)
	if !strings.Contains(revoked, "Revoked key "+id) {
		t.Fatalf("revoke output: %q", revoked)
	}
	if after := run("list"); !strings.Contains(after, "true") {
		t.Fatalf("list should show revoked=true: %q", after)
	}
}

func TestKeyCreateRequiresName(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runKeyCmd(context.Background(), &out, []string{"create", "--local", "--store", filepath.Join(t.TempDir(), "k.json")})
	if err == nil {
		t.Fatal("expected error when --name omitted")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("missing --name should be a usage error, got exit %d", exitCode(err))
	}
}

func extractToken(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Token: ") {
			return strings.TrimPrefix(line, "Token: ")
		}
	}
	t.Fatalf("no token line in output: %q", output)
	return ""
}
