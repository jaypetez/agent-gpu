package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/version"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was
// written. The version command prints directly to os.Stdout (like main), so the
// test swaps the file descriptor rather than injecting a writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()
	_ = w.Close()
	return <-done
}

func TestVersionCommand(t *testing.T) {
	// Not parallel: it temporarily redirects the process-wide os.Stdout.
	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			var runErr error
			out := captureStdout(t, func() { runErr = run([]string{arg}) })
			if runErr != nil {
				t.Fatalf("run([%q]) returned error: %v", arg, runErr)
			}
			want := version.String()
			if got := strings.TrimSpace(out); got != want {
				t.Fatalf("run([%q]) printed %q, want %q", arg, got, want)
			}
			if !strings.Contains(out, "agentgpu") {
				t.Fatalf("run([%q]) output missing program name: %q", arg, out)
			}
		})
	}
}

func TestNoArgsReturnsError(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("run(nil) = nil, want an error for missing subcommand")
	}
}

func TestUnknownSubcommandReturnsError(t *testing.T) {
	if err := run([]string{"definitely-not-a-command"}); err == nil {
		t.Fatal("run with unknown subcommand = nil, want an error")
	}
}
