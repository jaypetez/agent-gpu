package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalBootstrapGuard proves the #104 API-first invariant: --local is locked to
// one-time bootstrap (minting the first admin key on an EMPTY store). Once the store
// has any key, every mutating --local operation is rejected with a usage error
// (exit 2) directing the operator to the --server HTTP path. Read-only --local
// inspection (key list) is unaffected.
func TestLocalBootstrapGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// seedStore creates a single key in a fresh temp store via the legal bootstrap
	// path and returns the store path (now non-empty) plus the created key id.
	seedStore := func(t *testing.T) (string, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "keys.json")
		var out bytes.Buffer
		if err := runKeyCmd(ctx, &out, []string{"create", "--name", "bootstrap", "--role", "admin", "--local", "--store", path}); err != nil {
			t.Fatalf("bootstrap create: %v", err)
		}
		token := extractToken(t, out.String())
		id := strings.SplitN(token, "_", 3)[1]
		return path, id
	}

	t.Run("bootstrap create on empty store succeeds", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "keys.json")
		var out bytes.Buffer
		if err := runKeyCmd(ctx, &out, []string{"create", "--name", "first", "--role", "admin", "--local", "--store", path}); err != nil {
			t.Fatalf("first create on empty store should succeed, got %v", err)
		}
		if !strings.Contains(out.String(), "Token: agpu_") {
			t.Fatalf("create did not print the one-time token: %q", out.String())
		}
	})

	// Each mutating --local op below targets an already-populated store and must be
	// rejected with the directing usage error — including a SECOND create, which is
	// the case that most directly enforces "first key only".
	cases := []struct {
		name string
		// args are the subcommand + its positional args; --local --store <path> is
		// appended by the runner. The id placeholder is filled with the seeded key id.
		args func(id string) []string
		op   string
	}{
		{"second create", func(string) []string { return []string{"create", "--name", "second", "--role", "user"} }, "key create"},
		{"revoke", func(id string) []string { return []string{"revoke", id} }, "key revoke"},
		{"rotate", func(id string) []string { return []string{"rotate", id} }, "key rotate"},
		{"perms", func(id string) []string { return []string{"perms", id, "--role", "user"} }, "key perms"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+" rejected on non-empty store", func(t *testing.T) {
			t.Parallel()
			path, id := seedStore(t)
			var out bytes.Buffer
			full := append(tc.args(id), "--local", "--store", path)
			err := runKeyCmd(ctx, &out, full)
			assertLocalRejected(t, err, tc.op)
		})
	}

	t.Run("quota set rejected on non-empty store", func(t *testing.T) {
		t.Parallel()
		path, id := seedStore(t)
		var out bytes.Buffer
		err := runQuotaCmd(ctx, &out, []string{"set", id, "--rpm", "60", "--local", "--store", path})
		assertLocalRejected(t, err, "quota set")
	})

	t.Run("read-only list is allowed on non-empty store", func(t *testing.T) {
		t.Parallel()
		path, id := seedStore(t)
		var out bytes.Buffer
		if err := runKeyCmd(ctx, &out, []string{"list", "--local", "--store", path}); err != nil {
			t.Fatalf("key list --local on a populated store should be allowed, got %v", err)
		}
		if !strings.Contains(out.String(), id) {
			t.Fatalf("list missing the seeded key %q: %q", id, out.String())
		}
	})
}

// assertLocalRejected checks err is the bootstrap-guard rejection: a usage error
// (exit 2) whose message names the operation, states --local is bootstrap-only, and
// directs the operator to --server.
func assertLocalRejected(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a usage error rejecting the --local mutation on a non-empty store")
	}
	if got := exitCode(err); got != exitUsage {
		t.Fatalf("exit code = %d, want %d (err: %v)", got, exitUsage, err)
	}
	msg := err.Error()
	for _, want := range []string{op, "--local", "empty store", "--server"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("rejection message %q missing %q", msg, want)
		}
	}
}
