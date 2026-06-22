package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestQuotaShowLocalReadOnly exercises the read-only --local quota inspection path
// (runQuotaShowLocal) over a temp store: bootstrap a key, then `quota show --local`
// reports its effective limits (global defaults, no per-key override) without
// leaking a secret. Setting a quota in --local mode is forbidden by the #104
// bootstrap guard (a populated store must be managed over the API) and is asserted
// here too; the HTTP set/show round-trip lives in http_test.go.
func TestQuotaShowLocalReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "keys.json")
	quotaPath := filepath.Join(t.TempDir(), "quota.json")

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		full := append(args, "--local")
		if err := runKeyCmd(ctx, &out, full); err != nil {
			t.Fatalf("runKeyCmd %v: %v", args, err)
		}
		return out.String()
	}

	created := run("create", "--name", "q-agent", "--store", storePath)
	token := extractToken(t, created)
	id := strings.SplitN(token, "_", 3)[1]
	secret := strings.SplitN(token, "_", 3)[2]

	// quota set --local against the now-populated store is rejected by the guard.
	var setOut bytes.Buffer
	setErr := runKeyCmd(ctx, &setOut, []string{"quota", "set", id, "--rpm", "60", "--local", "--store", storePath})
	if exitCode(setErr) != exitUsage {
		t.Fatalf("quota set --local on a populated store should be a usage error, got exit %d (err: %v)", exitCode(setErr), setErr)
	}

	// The read-only show path still works and reports the effective (default) limits.
	show := run("quota", id, "--store", storePath, "--quota-path", quotaPath)
	if !strings.Contains(show, "Quota for key "+id) {
		t.Fatalf("show output: %q", show)
	}
	if !strings.Contains(show, "global defaults") {
		t.Fatalf("show should report global defaults (no per-key override): %q", show)
	}
	for _, want := range []string{"requests/min", "tokens/min", "tokens/day", "tokens/month", "unlimited"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show missing %q: %q", want, show)
		}
	}
	if strings.Contains(show, secret) {
		t.Fatal("quota show --local leaked the secret")
	}
}

// TestQuotaSetClearWithNumericIsUsageError proves combining --clear with a numeric
// dimension is rejected as a usage error (exit 2) rather than silently letting
// --clear win. The guard fires during flag validation, before any HTTP/store
// access, so it needs neither a server nor a token.
func TestQuotaSetClearWithNumericIsUsageError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runQuotaCmd(context.Background(), &out, []string{"set", "k1", "--clear", "--rpm", "99"})
	if err == nil {
		t.Fatal("expected a usage error for --clear combined with --rpm")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("exit code = %d, want %d", exitCode(err), exitUsage)
	}
	if !strings.Contains(err.Error(), "--clear cannot be combined") {
		t.Fatalf("error should explain the conflict: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("no output expected on the usage-error path, got %q", out.String())
	}
}

// TestKeyQuotaClearHTTP verifies `key quota set <id> --clear` routes through to the
// quota endpoint with an empty body (the clear signal) and reports the cleared
// override. It goes through runKeyCmd (the `key quota` alias) against the admin
// stub, since clearing a per-key override is a runtime mutation that the #104
// invariant requires to go over the API rather than --local.
func TestKeyQuotaClearHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/keys/k1/quota": {http.StatusOK,
			`{"id":"k1","name":"app","roles":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":0,"created":1}`},
	})

	out, err := runHTTP(t, a, runKeyCmd, "quota", "set", "k1", "--clear")
	if err != nil {
		t.Fatalf("key quota set --clear: %v", err)
	}
	if !strings.Contains(out, "Cleared quota override for key k1") {
		t.Fatalf("clear output: %q", out)
	}
	if a.lastReq.method != http.MethodPut || a.lastReq.path != "/v1/admin/keys/k1/quota" {
		t.Fatalf("sent %s %s, want PUT /v1/admin/keys/k1/quota", a.lastReq.method, a.lastReq.path)
	}
	if len(a.lastReq.body) != 0 {
		t.Fatalf("clear should send an empty body, got %v", a.lastReq.body)
	}
}
