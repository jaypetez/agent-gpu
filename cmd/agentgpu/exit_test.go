package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
)

// TestExitCodeMapping covers the error-to-exit-code contract documented in
// usage(): help is success, a usageError is 2, the apiclient status sentinels map
// to 3/4, a server APIError is 1, and a plain error is 1.
func TestExitCodeMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitOK},
		{"help", flag.ErrHelp, exitOK},
		{"usage", usagef("bad"), exitUsage},
		{"wrapped usage", fmt.Errorf("ctx: %w", usagef("bad")), exitUsage},
		{"unauthorized", apiclient.ErrUnauthorized, exitAuth},
		{"forbidden", apiclient.ErrForbidden, exitAuth},
		{"not found", apiclient.ErrNotFound, exitNotFound},
		{"rate limited", apiclient.ErrRateLimited, exitError},
		{"server sentinel", apiclient.ErrServer, exitError},
		{"api error 400", &apiclient.APIError{Status: 400, Message: "bad"}, exitError},
		{"plain", errors.New("boom"), exitError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCode(tc.err); got != tc.want {
				t.Fatalf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestExitCodeSentinelless404FallsBackToGeneral proves a 404 *APIError that has
// NOT been tagged with the ErrNotFound sentinel falls back to the general error
// code. The exported APIError does not let a test set its unexported class, so
// this only covers the fallback; the real 404→exitNotFound path (where the client
// attaches the sentinel) is exercised end-to-end in http_test.go's
// TestHTTPErrorExitCodes.
func TestExitCodeSentinelless404FallsBackToGeneral(t *testing.T) {
	t.Parallel()
	err := &apiclient.APIError{Status: 404, Message: "key not found"}
	if got := exitCode(err); got != exitError {
		t.Fatalf("a 404 APIError without sentinel maps to %d (documented fallback is general error)", got)
	}
}

// TestTopLevelHelpExitsZero proves `--help`/`-h`/`help` print usage and are a
// success (exit 0), not an error.
func TestTopLevelHelpExitsZero(t *testing.T) {
	// Not parallel: run() writes the help to os.Stdout for these args.
	for _, arg := range []string{"--help", "-h", "help"} {
		t.Run(arg, func(t *testing.T) {
			out := captureStdout(t, func() {
				if err := run([]string{arg}); err != nil {
					t.Fatalf("run([%q]) = %v, want nil", arg, err)
				}
			})
			if !strings.Contains(out, "Usage:") || !strings.Contains(out, "agentgpu") {
				t.Fatalf("help output missing usage: %q", out)
			}
			if !strings.Contains(out, "Exit codes:") {
				t.Fatalf("help should document exit codes: %q", out)
			}
		})
	}
}

// TestSubcommandHelpExitsZero proves a per-command `--help` prints that command's
// usage (with its flags) and returns flag.ErrHelp, which maps to exit 0. Help is
// captured via the handler's injected writer (a buffer), so the subtests run in
// parallel without racing on the process-global os.Stdout.
func TestSubcommandHelpExitsZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args      []string
		wantFlag  string // a flag name that must appear in the command's help
		wantUsage string
	}{
		{[]string{"key", "create", "--help"}, "-name", "key create"},
		{[]string{"key", "revoke", "-h"}, "-token", "key revoke"},
		{[]string{"quota", "set", "--help"}, "-rpm", "quota set"},
		{[]string{"models", "list", "--help"}, "-json", "models list"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			out, err := dispatchForTest(tc.args)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("err = %v, want flag.ErrHelp", err)
			}
			if exitCode(err) != exitOK {
				t.Fatalf("help should exit 0, got %d", exitCode(err))
			}
			if !strings.Contains(out, tc.wantUsage) {
				t.Fatalf("help missing usage %q: %q", tc.wantUsage, out)
			}
			if !strings.Contains(out, tc.wantFlag) {
				t.Fatalf("help missing flag %q: %q", tc.wantFlag, out)
			}
		})
	}
}

// dispatchForTest routes a subcommand to its handler with an injected buffer,
// mirroring dispatch() but without the signal/logging setup so help/usage paths
// can be exercised directly. It returns what the handler wrote to that buffer so
// help text is asserted without touching the process-global os.Stdout. Test-only
// glue.
func dispatchForTest(args []string) (string, error) {
	var out bytes.Buffer
	var err error
	switch args[0] {
	case "key":
		err = runKeyCmd(context.Background(), &out, args[1:])
	case "quota":
		err = runQuotaCmd(context.Background(), &out, args[1:])
	case "models":
		err = runModelsCmd(context.Background(), &out, args[1:])
	default:
		err = usagef("unknown %q", args[0])
	}
	return out.String(), err
}

// TestUnknownSubcommandIsUsageError proves an unknown subcommand is a usage error
// (exit 2), not a generic error.
func TestUnknownSubcommandIsUsageError(t *testing.T) {
	t.Parallel()
	err := run([]string{"definitely-not-a-command"})
	if err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("exit code = %d, want %d", exitCode(err), exitUsage)
	}
}

// TestUnknownFlagIsUsageError proves an unknown flag on a subcommand is a usage
// error (exit 2).
func TestUnknownFlagIsUsageError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runModelsCmd(context.Background(), &out, []string{"list", "--bogus", "--token", "agpu_x_y", "--server", "http://127.0.0.1:1"})
	if err == nil {
		t.Fatal("expected a usage error for an unknown flag")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("exit code = %d, want %d", exitCode(err), exitUsage)
	}
}
