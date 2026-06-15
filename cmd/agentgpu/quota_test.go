package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyQuotaCLIFlow exercises `key quota set` and `key quota <id>` against a
// temp store: set limits, then show them, asserting the values round-trip and
// no secret leaks.
func TestKeyQuotaCLIFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "keys.json")
	quotaPath := filepath.Join(t.TempDir(), "quota.json")

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runKeyCmd(ctx, &out, args); err != nil {
			t.Fatalf("runKeyCmd %v: %v", args, err)
		}
		return out.String()
	}

	created := run("create", "--name", "q-agent", "--store", storePath)
	token := extractToken(t, created)
	id := strings.SplitN(token, "_", 3)[1]
	secret := strings.SplitN(token, "_", 3)[2]

	// Set per-key limits.
	set := run("quota", "set", id, "--rpm", "60", "--tpm", "1000", "--daily-tokens", "100000", "--store", storePath)
	if !strings.Contains(set, "Updated quota for key "+id) {
		t.Fatalf("set output: %q", set)
	}
	if !strings.Contains(set, "RPM: 60") || !strings.Contains(set, "TPM: 1000") {
		t.Fatalf("set did not echo limits: %q", set)
	}

	// Show usage vs limits.
	show := run("quota", id, "--store", storePath, "--quota-path", quotaPath)
	if !strings.Contains(show, "Quota for key "+id) {
		t.Fatalf("show output: %q", show)
	}
	if !strings.Contains(show, "per-key override") {
		t.Fatalf("show should report per-key override: %q", show)
	}
	for _, want := range []string{"requests/min", "tokens/min", "tokens/day", "tokens/month", "60", "1000", "100000"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show missing %q: %q", want, show)
		}
	}
	// monthly-tokens was left at 0 -> "unlimited".
	if !strings.Contains(show, "unlimited") {
		t.Fatalf("show should mark monthly as unlimited: %q", show)
	}
	if strings.Contains(show, secret) || strings.Contains(set, secret) {
		t.Fatal("quota CLI leaked the secret")
	}
}

// TestKeyQuotaClear verifies --clear removes the per-key override.
func TestKeyQuotaClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "keys.json")

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runKeyCmd(ctx, &out, args); err != nil {
			t.Fatalf("runKeyCmd %v: %v", args, err)
		}
		return out.String()
	}

	created := run("create", "--name", "q-agent", "--store", storePath)
	id := strings.SplitN(extractToken(t, created), "_", 3)[1]

	run("quota", "set", id, "--rpm", "5", "--store", storePath)
	cleared := run("quota", "set", id, "--clear", "--store", storePath)
	if !strings.Contains(cleared, "Cleared quota override") {
		t.Fatalf("clear output: %q", cleared)
	}

	show := run("quota", id, "--store", storePath)
	if !strings.Contains(show, "global defaults") {
		t.Fatalf("after clear, show should report global defaults: %q", show)
	}
}
