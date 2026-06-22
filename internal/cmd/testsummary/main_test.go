package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes content to a temp file and returns an opened *os.File for
// reading, so run() (which takes *os.File) can be exercised end-to-end.
func writeTemp(t *testing.T, content string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open temp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// runOn pipes the given stdin content through run() and captures stdout + the
// exit code, so a test asserts on both the summary text and the gate result.
func runOn(t *testing.T, stdin string) (string, int) {
	t.Helper()
	in := writeTemp(t, stdin)
	outPath := filepath.Join(t.TempDir(), "out.txt")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	code := run(in, out)
	if err := out.Close(); err != nil {
		t.Fatalf("close out: %v", err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	return string(b), code
}

func TestRunAllPass(t *testing.T) {
	// Two passing tests in one package; the run/output events are noise the
	// summary ignores. Exit code must be 0.
	stream := strings.Join([]string{
		`{"Action":"run","Package":"pkg/a","Test":"TestOne"}`,
		`{"Action":"output","Package":"pkg/a","Test":"TestOne","Output":"=== RUN TestOne\n"}`,
		`{"Action":"pass","Package":"pkg/a","Test":"TestOne"}`,
		`{"Action":"run","Package":"pkg/a","Test":"TestTwo"}`,
		`{"Action":"pass","Package":"pkg/a","Test":"TestTwo"}`,
		`{"Action":"pass","Package":"pkg/a"}`,
	}, "\n")
	out, code := runOn(t, stream)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "2 passed, 0 failed, 0 skipped") {
		t.Errorf("summary line missing/incorrect; output:\n%s", out)
	}
}

func TestRunWithFailureSurfacesOutput(t *testing.T) {
	// A failing test must flip the exit code to 1, be named in the FAILED TESTS
	// block, and have its assertion output echoed so an agent can read it.
	stream := strings.Join([]string{
		`{"Action":"run","Package":"pkg/b","Test":"TestBoom"}`,
		`{"Action":"output","Package":"pkg/b","Test":"TestBoom","Output":"    main_test.go:10: got 1 want 2\n"}`,
		`{"Action":"fail","Package":"pkg/b","Test":"TestBoom"}`,
		`{"Action":"pass","Package":"pkg/b","Test":"TestOK"}`,
		`{"Action":"fail","Package":"pkg/b"}`,
	}, "\n")
	out, code := runOn(t, stream)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL: pkg/b.TestBoom") {
		t.Errorf("failed test not named; output:\n%s", out)
	}
	if !strings.Contains(out, "got 1 want 2") {
		t.Errorf("assertion output not surfaced; output:\n%s", out)
	}
	if !strings.Contains(out, "1 passed, 1 failed, 0 skipped") {
		t.Errorf("summary counts wrong; output:\n%s", out)
	}
}

func TestRunBuildErrorFails(t *testing.T) {
	// A leading non-JSON build error (what `go test -json` prints before any
	// event when a package fails to compile) must fail the gate.
	stream := strings.Join([]string{
		`# pkg/c`,
		`pkg/c/x.go:3:1: syntax error: unexpected }`,
		`{"Action":"fail","Package":"pkg/c"}`,
	}, "\n")
	out, code := runOn(t, stream)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "syntax error") {
		t.Errorf("build error not surfaced; output:\n%s", out)
	}
}

func TestRunSkipCounts(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"skip","Package":"pkg/d","Test":"TestSkipped"}`,
		`{"Action":"pass","Package":"pkg/d","Test":"TestRan"}`,
		`{"Action":"pass","Package":"pkg/d"}`,
	}, "\n")
	out, code := runOn(t, stream)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "1 passed, 0 failed, 1 skipped") {
		t.Errorf("skip count wrong; output:\n%s", out)
	}
}
