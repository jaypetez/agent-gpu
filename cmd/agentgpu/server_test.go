package main

import (
	"flag"
	"os"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/config"
)

// parseMetricsFlag builds a FlagSet with the --metrics-listen flag (mirroring
// runServerCmd), parses args, and reports the flag value plus the FlagSet so a
// test can drive metricsListenOff exactly as the command does.
func parseMetricsFlag(t *testing.T, args []string) (*flag.FlagSet, string) {
	t.Helper()
	fs := flag.NewFlagSet("server start", flag.ContinueOnError)
	v := fs.String("metrics-listen", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return fs, *v
}

// TestMetricsListenOff covers the only place that must distinguish "set to empty"
// (disable) from "unset" (default on): an explicit empty --metrics-listen or an
// empty AGENTGPU_METRICS_LISTEN disables; an unset flag/env, or a non-empty
// value, does not. It is not parallel because it mutates the process environment.
func TestMetricsListenOff(t *testing.T) {
	t.Run("unset flag and env: not off (default on)", func(t *testing.T) {
		// Guarantee the env is truly absent for this case, restoring afterward.
		prev, had := os.LookupEnv(config.EnvMetricsListen)
		_ = os.Unsetenv(config.EnvMetricsListen)
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(config.EnvMetricsListen, prev)
			}
		})
		fs, v := parseMetricsFlag(t, nil)
		if metricsListenOff(fs, v) {
			t.Fatal("unset flag and unset env must not disable (listener defaults on)")
		}
	})
	t.Run("empty env: off", func(t *testing.T) {
		t.Setenv(config.EnvMetricsListen, "")
		fs, v := parseMetricsFlag(t, nil)
		if !metricsListenOff(fs, v) {
			t.Fatal("an explicitly empty AGENTGPU_METRICS_LISTEN must disable")
		}
	})
	t.Run("non-empty env: not off", func(t *testing.T) {
		t.Setenv(config.EnvMetricsListen, "127.0.0.1:9090")
		fs, v := parseMetricsFlag(t, nil)
		if metricsListenOff(fs, v) {
			t.Fatal("a non-empty env must not disable")
		}
	})
	t.Run("explicit empty flag: off, overriding env", func(t *testing.T) {
		t.Setenv(config.EnvMetricsListen, "127.0.0.1:9090") // env says on...
		fs, v := parseMetricsFlag(t, []string{"--metrics-listen="})
		if !metricsListenOff(fs, v) { // ...but an explicit empty flag wins to disable
			t.Fatal("explicit empty --metrics-listen must disable, overriding env")
		}
	})
	t.Run("non-empty flag: not off", func(t *testing.T) {
		t.Setenv(config.EnvMetricsListen, "")
		fs, v := parseMetricsFlag(t, []string{"--metrics-listen=0.0.0.0:9100"})
		if metricsListenOff(fs, v) {
			t.Fatal("a non-empty flag must not disable")
		}
	})
}
