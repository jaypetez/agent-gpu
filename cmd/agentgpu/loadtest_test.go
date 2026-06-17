package main

import (
	"bytes"
	"context"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/loadtest"
)

// TestParseLoadtestFlagsDefaults proves a bare invocation gets sensible defaults:
// remote mode, concurrency 16, chat endpoint, and a default request budget when
// neither --duration nor --requests is given.
func TestParseLoadtestFlagsDefaults(t *testing.T) {
	var out bytes.Buffer
	f, err := parseLoadtestFlags(&out, nil)
	if err != nil {
		t.Fatalf("parseLoadtestFlags: %v", err)
	}
	if f.mode != "remote" {
		t.Errorf("mode = %q, want remote", f.mode)
	}
	if f.concurrency != 16 {
		t.Errorf("concurrency = %d, want 16", f.concurrency)
	}
	if f.endpoint != "chat" {
		t.Errorf("endpoint = %q, want chat", f.endpoint)
	}
	if f.requests != 1000 {
		t.Errorf("requests = %d, want default 1000", f.requests)
	}
	if f.duration != 0 {
		t.Errorf("duration = %v, want 0", f.duration)
	}
}

// TestParseLoadtestFlagsExplicit proves explicit flags are parsed across the
// remote target, load shape, and in-process knobs.
func TestParseLoadtestFlagsExplicit(t *testing.T) {
	var out bytes.Buffer
	args := []string{
		"--mode", "inproc",
		"--concurrency", "32",
		"--duration", "10s",
		"--rate", "500",
		"--endpoint", "completions",
		"--model", "llama3",
		"--prompt", "hi there",
		"--workers", "4",
		"--queue-max-depth", "16",
		"--global-rpm", "100",
		"--global-tpm", "5000",
		"--think", "5ms",
		"--stats-interval", "2s",
		"--json",
	}
	f, err := parseLoadtestFlags(&out, args)
	if err != nil {
		t.Fatalf("parseLoadtestFlags: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"mode", f.mode, "inproc"},
		{"concurrency", f.concurrency, 32},
		{"duration", f.duration, 10 * time.Second},
		{"rate", f.rate, 500.0},
		{"endpoint", f.endpoint, "completions"},
		{"model", f.model, "llama3"},
		{"prompt", f.prompt, "hi there"},
		{"workers", f.workers, 4},
		{"queueMaxDepth", f.queueMaxDepth, 16},
		{"globalRPM", f.globalRPM, uint64(100)},
		{"globalTPM", f.globalTPM, uint64(5000)},
		{"think", f.think, 5 * time.Millisecond},
		{"statsInterval", f.statsInterval, 2 * time.Second},
		{"asJSON", f.asJSON, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// An explicit --duration must NOT trigger the default request budget.
	if f.requests != 0 {
		t.Errorf("requests = %d, want 0 when --duration is set", f.requests)
	}
}

// TestParseLoadtestFlagsHelp proves -h returns flag.ErrHelp (clean exit 0).
func TestParseLoadtestFlagsHelp(t *testing.T) {
	var out bytes.Buffer
	_, err := parseLoadtestFlags(&out, []string{"-h"})
	if err != flag.ErrHelp {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

// TestResolveMixAndModelEndpoint proves the single --endpoint path builds a
// single-endpoint mix and validates the endpoint name.
func TestResolveMixAndModelEndpoint(t *testing.T) {
	f := &loadtestFlags{endpoint: "models"}
	mix, model, err := f.resolveMixAndModel("default-model")
	if err != nil {
		t.Fatalf("resolveMixAndModel: %v", err)
	}
	if mix.Pick(0) != loadtest.EndpointModels {
		t.Errorf("mix did not resolve to models endpoint")
	}
	if model != "default-model" {
		t.Errorf("model = %q, want the default", model)
	}

	// An unknown endpoint is a usage error.
	bad := &loadtestFlags{endpoint: "frob"}
	if _, _, err := bad.resolveMixAndModel(""); err == nil {
		t.Errorf("unknown endpoint = nil error, want usage error")
	}
}

// TestResolveMixAndModelMix proves a --mix spec is parsed and overrides the
// endpoint default, and that a user --model wins over the default.
func TestResolveMixAndModelMix(t *testing.T) {
	f := &loadtestFlags{endpoint: "chat", mix: "chat=80,models=20", model: "user-model"}
	mix, model, err := f.resolveMixAndModel("default-model")
	if err != nil {
		t.Fatalf("resolveMixAndModel: %v", err)
	}
	if got := mix.String(); got != "chat=80,models=20" {
		t.Errorf("mix = %q, want chat=80,models=20", got)
	}
	if model != "user-model" {
		t.Errorf("model = %q, want user-model", model)
	}

	// A non-default --endpoint together with --mix is a conflict.
	conflict := &loadtestFlags{endpoint: "models", mix: "chat=80,models=20"}
	if _, _, err := conflict.resolveMixAndModel(""); err == nil {
		t.Errorf("endpoint+mix conflict = nil error, want usage error")
	}

	// An invalid mix spec is a usage error.
	badMix := &loadtestFlags{mix: "chat=oops"}
	if _, _, err := badMix.resolveMixAndModel(""); err == nil {
		t.Errorf("invalid mix = nil error, want usage error")
	}
}

// TestRunLoadtestCmdUnknownMode proves an unknown --mode is a usage error.
func TestRunLoadtestCmdUnknownMode(t *testing.T) {
	var out bytes.Buffer
	err := runLoadtestCmd(context.Background(), nil, &out, []string{"--mode", "bogus", "--requests", "1"})
	if err == nil {
		t.Fatalf("unknown mode = nil error, want usage error")
	}
	var ue *usageError
	if !asUsageError(err, &ue) {
		t.Errorf("err = %T, want *usageError", err)
	}
}

// TestRunLoadtestCmdHelp proves `loadtest --help` prints usage and returns
// flag.ErrHelp (clean exit).
func TestRunLoadtestCmdHelp(t *testing.T) {
	var out bytes.Buffer
	err := runLoadtestCmd(context.Background(), nil, &out, []string{"--help"})
	if err != flag.ErrHelp {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(out.String(), "loadtest") {
		t.Errorf("help output missing 'loadtest': %q", out.String())
	}
}

// TestRunLoadtestInProcEndToEnd drives the inproc mode through the cmd layer at
// low concurrency, asserting it runs the full stack and prints a report with the
// expected sections. This is the cmd-level functional smoke (complements the
// package-level loadtest tests) and rides the -race job.
func TestRunLoadtestInProcEndToEnd(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--mode", "inproc", "--workers", "2", "--concurrency", "4", "--requests", "100"}
	if err := runLoadtestCmd(context.Background(), nil, &out, args); err != nil {
		t.Fatalf("runLoadtestCmd: %v", err)
	}
	got := out.String()
	for _, want := range []string{"agent-gpu load test", "throughput", "status breakdown", "latency", "2xx (ok):          100"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\n--- full output ---\n%s", want, got)
		}
	}
}

// TestRunLoadtestInProcJSON proves --json emits a parseable JSON report with the
// run's counts.
func TestRunLoadtestInProcJSON(t *testing.T) {
	var out bytes.Buffer
	args := []string{"--mode", "inproc", "--workers", "2", "--concurrency", "4", "--requests", "50", "--json"}
	if err := runLoadtestCmd(context.Background(), nil, &out, args); err != nil {
		t.Fatalf("runLoadtestCmd: %v", err)
	}
	// The JSON object should be present; a crude structural check keeps the test
	// free of a full decode while proving it is JSON, not the text report.
	s := out.String()
	if !strings.Contains(s, `"summary"`) || !strings.Contains(s, `"total": 50`) {
		t.Errorf("json report missing expected fields:\n%s", s)
	}
	if strings.Contains(s, "agent-gpu load test") {
		t.Errorf("--json emitted the text report too:\n%s", s)
	}
}

// asUsageError is a tiny errors.As shim so the test does not import errors just
// for one call; it mirrors how main.go inspects the usageError type.
func asUsageError(err error, target **usageError) bool {
	for err != nil {
		if ue, ok := err.(*usageError); ok {
			*target = ue
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
