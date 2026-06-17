package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResolveSessionPath(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	wantDefault := filepath.Join("/home/u", ".agentgpu", "sessions.json")

	t.Run("default uses home dir", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionPath("", env(nil), home); got != wantDefault {
			t.Fatalf("got %q, want %q", got, wantDefault)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveSessionPath("", env(map[string]string{EnvSessionPath: "/tmp/s.json"}), home)
		if got != "/tmp/s.json" {
			t.Fatalf("got %q, want env value", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveSessionPath("/flag/s.json", env(map[string]string{EnvSessionPath: "/tmp/s.json"}), home)
		if got != "/flag/s.json" {
			t.Fatalf("got %q, want flag value", got)
		}
	})
	t.Run("home error falls back to relative path", func(t *testing.T) {
		t.Parallel()
		badHome := func() (string, error) { return "", errHome }
		if got := ResolveSessionPath("", env(nil), badHome); got != filepath.Join(".agentgpu", "sessions.json") {
			t.Fatalf("got %q, want relative fallback", got)
		}
	})
}

func TestResolveSessionTTL(t *testing.T) {
	t.Parallel()
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionTTL(0, env(nil)); got != DefaultSessionTTL {
			t.Fatalf("got %v, want default %v", got, DefaultSessionTTL)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveSessionTTL(0, env(map[string]string{EnvSessionTTL: "5m"}))
		if got != 5*time.Minute {
			t.Fatalf("got %v, want 5m", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveSessionTTL(2*time.Minute, env(map[string]string{EnvSessionTTL: "5m"}))
		if got != 2*time.Minute {
			t.Fatalf("got %v, want flag 2m", got)
		}
	})
	t.Run("unparseable env falls back to default", func(t *testing.T) {
		t.Parallel()
		got := ResolveSessionTTL(0, env(map[string]string{EnvSessionTTL: "nonsense"}))
		if got != DefaultSessionTTL {
			t.Fatalf("got %v, want default on bad env", got)
		}
	})
}

func TestResolveModelWarmMax(t *testing.T) {
	t.Parallel()
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		if got := ResolveModelWarmMax(0, env(nil)); got != DefaultModelWarmMax {
			t.Fatalf("got %v, want default %v", got, DefaultModelWarmMax)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveModelWarmMax(0, env(map[string]string{EnvModelWarmMax: "20m"}))
		if got != 20*time.Minute {
			t.Fatalf("got %v, want 20m", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveModelWarmMax(45*time.Minute, env(map[string]string{EnvModelWarmMax: "20m"}))
		if got != 45*time.Minute {
			t.Fatalf("got %v, want flag 45m", got)
		}
	})
	t.Run("unparseable env falls back to default", func(t *testing.T) {
		t.Parallel()
		got := ResolveModelWarmMax(0, env(map[string]string{EnvModelWarmMax: "nonsense"}))
		if got != DefaultModelWarmMax {
			t.Fatalf("got %v, want default on bad env", got)
		}
	})
}

func TestResolveMaxSessionsPerKey(t *testing.T) {
	t.Parallel()
	t.Run("default unlimited", func(t *testing.T) {
		t.Parallel()
		if got := ResolveMaxSessionsPerKey(0, env(nil)); got != DefaultMaxSessionsPerKey {
			t.Fatalf("got %d, want default %d", got, DefaultMaxSessionsPerKey)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		if got := ResolveMaxSessionsPerKey(0, env(map[string]string{EnvMaxSessionsPerKey: "5"})); got != 5 {
			t.Fatalf("got %d, want 5", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveMaxSessionsPerKey(3, env(map[string]string{EnvMaxSessionsPerKey: "5"}))
		if got != 3 {
			t.Fatalf("got %d, want flag 3", got)
		}
	})
	t.Run("bad/negative env falls back to default", func(t *testing.T) {
		t.Parallel()
		if got := ResolveMaxSessionsPerKey(0, env(map[string]string{EnvMaxSessionsPerKey: "-4"})); got != DefaultMaxSessionsPerKey {
			t.Fatalf("got %d, want default on negative env", got)
		}
		if got := ResolveMaxSessionsPerKey(0, env(map[string]string{EnvMaxSessionsPerKey: "x"})); got != DefaultMaxSessionsPerKey {
			t.Fatalf("got %d, want default on unparseable env", got)
		}
	})
}

func TestResolveSessionMaxTurns(t *testing.T) {
	t.Parallel()
	t.Run("default 200", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionMaxTurns(0, env(nil)); got != DefaultSessionMaxTurns {
			t.Fatalf("got %d, want %d", got, DefaultSessionMaxTurns)
		}
	})
	t.Run("env overrides; flag wins", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionMaxTurns(0, env(map[string]string{EnvSessionMaxTurns: "10"})); got != 10 {
			t.Fatalf("env got %d, want 10", got)
		}
		if got := ResolveSessionMaxTurns(7, env(map[string]string{EnvSessionMaxTurns: "10"})); got != 7 {
			t.Fatalf("flag got %d, want 7", got)
		}
	})
}

func TestResolveSessionMaxContextTokens(t *testing.T) {
	t.Parallel()
	t.Run("default unlimited", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionMaxContextTokens(0, env(nil)); got != DefaultSessionMaxContextTokens {
			t.Fatalf("got %d, want default %d", got, DefaultSessionMaxContextTokens)
		}
	})
	t.Run("env overrides; flag wins; bad env falls back", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionMaxContextTokens(0, env(map[string]string{EnvSessionMaxContextTokens: "4096"})); got != 4096 {
			t.Fatalf("env got %d, want 4096", got)
		}
		if got := ResolveSessionMaxContextTokens(2048, env(map[string]string{EnvSessionMaxContextTokens: "4096"})); got != 2048 {
			t.Fatalf("flag got %d, want 2048", got)
		}
		if got := ResolveSessionMaxContextTokens(0, env(map[string]string{EnvSessionMaxContextTokens: "nope"})); got != DefaultSessionMaxContextTokens {
			t.Fatalf("got %d, want default on bad env", got)
		}
	})
}

func TestResolveSessionOverflowPolicy(t *testing.T) {
	t.Parallel()
	t.Run("default trim", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionOverflowPolicy("", env(nil)); got != DefaultSessionOverflowPolicy {
			t.Fatalf("got %q, want default %q", got, DefaultSessionOverflowPolicy)
		}
	})
	t.Run("env overrides; flag wins", func(t *testing.T) {
		t.Parallel()
		if got := ResolveSessionOverflowPolicy("", env(map[string]string{EnvSessionOverflowPolicy: "reject"})); got != "reject" {
			t.Fatalf("env got %q, want reject", got)
		}
		if got := ResolveSessionOverflowPolicy("trim", env(map[string]string{EnvSessionOverflowPolicy: "reject"})); got != "trim" {
			t.Fatalf("flag got %q, want trim", got)
		}
	})
}

func TestResolveSession(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }

	t.Run("defaults fill empty config", func(t *testing.T) {
		t.Parallel()
		got := ResolveSession(SessionConfig{}, env(nil), home)
		if got.Path != filepath.Join("/home/u", ".agentgpu", "sessions.json") {
			t.Fatalf("path = %q", got.Path)
		}
		if got.TTL != DefaultSessionTTL {
			t.Fatalf("ttl = %v", got.TTL)
		}
		if got.MaxTurns != DefaultSessionMaxTurns || got.MaxBytes != DefaultSessionMaxBytes {
			t.Fatalf("caps = %d/%d, want defaults", got.MaxTurns, got.MaxBytes)
		}
		if got.MaxContextTokens != DefaultSessionMaxContextTokens || got.MaxSessionsPerKey != DefaultMaxSessionsPerKey {
			t.Fatalf("tokens/per-key = %d/%d, want defaults", got.MaxContextTokens, got.MaxSessionsPerKey)
		}
		if got.OverflowPolicy != DefaultSessionOverflowPolicy {
			t.Fatalf("overflow = %q, want default %q", got.OverflowPolicy, DefaultSessionOverflowPolicy)
		}
		if got.ModelWarmMax != DefaultModelWarmMax {
			t.Fatalf("model warm max = %v, want default %v", got.ModelWarmMax, DefaultModelWarmMax)
		}
	})
	t.Run("explicit caps preserved", func(t *testing.T) {
		t.Parallel()
		got := ResolveSession(SessionConfig{
			MaxTurns: 10, MaxBytes: 4096, MaxContextTokens: 512, MaxSessionsPerKey: 9,
			OverflowPolicy: "reject", TTL: time.Minute, ModelWarmMax: 90 * time.Second,
		}, env(nil), home)
		if got.MaxTurns != 10 || got.MaxBytes != 4096 || got.TTL != time.Minute {
			t.Fatalf("explicit values not preserved: %+v", got)
		}
		if got.MaxContextTokens != 512 || got.MaxSessionsPerKey != 9 || got.OverflowPolicy != "reject" {
			t.Fatalf("explicit #37 caps not preserved: %+v", got)
		}
		if got.ModelWarmMax != 90*time.Second {
			t.Fatalf("explicit model warm max not preserved: %v", got.ModelWarmMax)
		}
	})
	t.Run("env fills the #37 caps", func(t *testing.T) {
		t.Parallel()
		got := ResolveSession(SessionConfig{}, env(map[string]string{
			EnvMaxSessionsPerKey:       "4",
			EnvSessionMaxTurns:         "20",
			EnvSessionMaxContextTokens: "1000",
			EnvSessionOverflowPolicy:   "reject",
		}), home)
		if got.MaxSessionsPerKey != 4 || got.MaxTurns != 20 || got.MaxContextTokens != 1000 || got.OverflowPolicy != "reject" {
			t.Fatalf("env did not fill #37 caps: %+v", got)
		}
	})
}
