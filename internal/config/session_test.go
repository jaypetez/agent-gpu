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
	})
	t.Run("explicit caps preserved", func(t *testing.T) {
		t.Parallel()
		got := ResolveSession(SessionConfig{MaxTurns: 10, MaxBytes: 4096, TTL: time.Minute}, env(nil), home)
		if got.MaxTurns != 10 || got.MaxBytes != 4096 || got.TTL != time.Minute {
			t.Fatalf("explicit values not preserved: %+v", got)
		}
	})
}
