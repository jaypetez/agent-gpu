package config

import "testing"

func TestResolveLog(t *testing.T) {
	t.Parallel()

	t.Run("defaults fill empty config", func(t *testing.T) {
		t.Parallel()
		got := ResolveLog(LogConfig{}, env(nil))
		if got.Level != DefaultLogLevel {
			t.Errorf("Level = %q, want default %q", got.Level, DefaultLogLevel)
		}
		if got.Format != DefaultLogFormat {
			t.Errorf("Format = %q, want default %q", got.Format, DefaultLogFormat)
		}
		if got.Output != DefaultLogOutput {
			t.Errorf("Output = %q, want default %q", got.Output, DefaultLogOutput)
		}
	})

	t.Run("env overrides defaults", func(t *testing.T) {
		t.Parallel()
		got := ResolveLog(LogConfig{}, env(map[string]string{
			EnvLogLevel:  "debug",
			EnvLogFormat: "text",
			EnvLogOutput: "stdout",
		}))
		if got.Level != "debug" || got.Format != "text" || got.Output != "stdout" {
			t.Fatalf("env not applied: %+v", got)
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveLog(
			LogConfig{Level: "warn", Format: "json", Output: "/var/log/agentgpu.log"},
			env(map[string]string{
				EnvLogLevel:  "debug",
				EnvLogFormat: "text",
				EnvLogOutput: "stdout",
			}),
		)
		if got.Level != "warn" || got.Format != "json" || got.Output != "/var/log/agentgpu.log" {
			t.Fatalf("flag did not win: %+v", got)
		}
	})

	t.Run("each field resolves independently", func(t *testing.T) {
		t.Parallel()
		// Level from flag, format from env, output from default.
		got := ResolveLog(LogConfig{Level: "error"}, env(map[string]string{EnvLogFormat: "text"}))
		if got.Level != "error" {
			t.Errorf("Level = %q, want flag error", got.Level)
		}
		if got.Format != "text" {
			t.Errorf("Format = %q, want env text", got.Format)
		}
		if got.Output != DefaultLogOutput {
			t.Errorf("Output = %q, want default %q", got.Output, DefaultLogOutput)
		}
	})

	t.Run("empty env value falls back to default", func(t *testing.T) {
		t.Parallel()
		// envOr treats a set-but-empty env var as unset, so the default applies.
		got := ResolveLog(LogConfig{}, env(map[string]string{EnvLogLevel: ""}))
		if got.Level != DefaultLogLevel {
			t.Fatalf("Level = %q, want default on empty env", got.Level)
		}
	})
}

// TestResolveLogNilLookup proves a nil EnvLookup falls back to os.LookupEnv: in a
// clean environment (the three log vars unset) the resolver yields the defaults.
// It is a top-level (non-parallel) test because it uses t.Setenv, which forbids a
// parallel test, to neutralize any ambient log env vars.
func TestResolveLogNilLookup(t *testing.T) {
	t.Setenv(EnvLogLevel, "")
	t.Setenv(EnvLogFormat, "")
	t.Setenv(EnvLogOutput, "")
	got := ResolveLog(LogConfig{}, nil)
	if got.Level != DefaultLogLevel || got.Format != DefaultLogFormat || got.Output != DefaultLogOutput {
		t.Fatalf("nil-lookup resolve = %+v, want all defaults", got)
	}
}
