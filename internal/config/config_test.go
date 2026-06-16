package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var errHome = errors.New("no home")

func env(m map[string]string) EnvLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestResolveServer(t *testing.T) {
	t.Parallel()
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		got := ResolveServer(ServerConfig{}, env(nil))
		if got.Listen != DefaultServerListen {
			t.Fatalf("Listen = %q, want default %q", got.Listen, DefaultServerListen)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveServer(ServerConfig{}, env(map[string]string{EnvServerListen: "0.0.0.0:9000"}))
		if got.Listen != "0.0.0.0:9000" {
			t.Fatalf("Listen = %q, want env value", got.Listen)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveServer(ServerConfig{Listen: "1.2.3.4:1"}, env(map[string]string{EnvServerListen: "0.0.0.0:9000"}))
		if got.Listen != "1.2.3.4:1" {
			t.Fatalf("Listen = %q, want flag value", got.Listen)
		}
	})
	t.Run("http listen default", func(t *testing.T) {
		t.Parallel()
		got := ResolveServer(ServerConfig{}, env(nil))
		if got.HTTPListen != DefaultHTTPListen {
			t.Fatalf("HTTPListen = %q, want default %q", got.HTTPListen, DefaultHTTPListen)
		}
	})
	t.Run("http listen env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveServer(ServerConfig{}, env(map[string]string{EnvHTTPListen: "0.0.0.0:8443"}))
		if got.HTTPListen != "0.0.0.0:8443" {
			t.Fatalf("HTTPListen = %q, want env value", got.HTTPListen)
		}
	})
	t.Run("http listen flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveServer(ServerConfig{HTTPListen: "1.2.3.4:80"}, env(map[string]string{EnvHTTPListen: "0.0.0.0:8443"}))
		if got.HTTPListen != "1.2.3.4:80" {
			t.Fatalf("HTTPListen = %q, want flag value", got.HTTPListen)
		}
	})
}

func TestResolveStorePath(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	wantDefault := filepath.Join("/home/u", ".agentgpu", "keys.json")

	t.Run("default uses home dir", func(t *testing.T) {
		t.Parallel()
		got := ResolveStorePath("", env(nil), home)
		if got != wantDefault {
			t.Fatalf("got %q, want default %q", got, wantDefault)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveStorePath("", env(map[string]string{EnvStorePath: "/tmp/k.json"}), home)
		if got != "/tmp/k.json" {
			t.Fatalf("got %q, want env value", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveStorePath("/flag/k.json", env(map[string]string{EnvStorePath: "/tmp/k.json"}), home)
		if got != "/flag/k.json" {
			t.Fatalf("got %q, want flag value", got)
		}
	})
	t.Run("home error falls back to relative path", func(t *testing.T) {
		t.Parallel()
		badHome := func() (string, error) { return "", errHome }
		got := ResolveStorePath("", env(nil), badHome)
		if got != filepath.Join(".agentgpu", "keys.json") {
			t.Fatalf("got %q, want relative fallback", got)
		}
	})
}

func TestResolveQuotaPath(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	wantDefault := filepath.Join("/home/u", ".agentgpu", "quota.json")

	t.Run("default uses home dir", func(t *testing.T) {
		t.Parallel()
		if got := ResolveQuotaPath("", env(nil), home); got != wantDefault {
			t.Fatalf("got %q, want %q", got, wantDefault)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Parallel()
		got := ResolveQuotaPath("", env(map[string]string{EnvQuotaPath: "/tmp/q.json"}), home)
		if got != "/tmp/q.json" {
			t.Fatalf("got %q, want env value", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveQuotaPath("/flag/q.json", env(map[string]string{EnvQuotaPath: "/tmp/q.json"}), home)
		if got != "/flag/q.json" {
			t.Fatalf("got %q, want flag value", got)
		}
	})
	t.Run("home error falls back to relative path", func(t *testing.T) {
		t.Parallel()
		badHome := func() (string, error) { return "", errHome }
		if got := ResolveQuotaPath("", env(nil), badHome); got != filepath.Join(".agentgpu", "quota.json") {
			t.Fatalf("got %q, want relative fallback", got)
		}
	})
}

func TestResolveQuota(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	got := ResolveQuota(QuotaConfig{DefaultRPM: 60, DefaultTPM: 1000}, env(nil), home)
	if got.Path != filepath.Join("/home/u", ".agentgpu", "quota.json") {
		t.Fatalf("path = %q", got.Path)
	}
	// Limit defaults pass through unchanged.
	if got.DefaultRPM != 60 || got.DefaultTPM != 1000 {
		t.Fatalf("limit defaults not preserved: %+v", got)
	}
}

func TestResolveWorker(t *testing.T) {
	t.Parallel()
	host := func() (string, error) { return "test-host", nil }

	t.Run("worker id falls back to hostname", func(t *testing.T) {
		t.Parallel()
		got := ResolveWorker(WorkerConfig{ServerAddr: "x:1"}, env(nil), host)
		if got.WorkerID != "test-host" {
			t.Fatalf("WorkerID = %q, want hostname fallback", got.WorkerID)
		}
	})
	t.Run("env provides server and id", func(t *testing.T) {
		t.Parallel()
		got := ResolveWorker(WorkerConfig{}, env(map[string]string{
			EnvWorkerServer: "srv:50051",
			EnvWorkerID:     "w-env",
		}), host)
		if got.ServerAddr != "srv:50051" {
			t.Fatalf("ServerAddr = %q", got.ServerAddr)
		}
		if got.WorkerID != "w-env" {
			t.Fatalf("WorkerID = %q", got.WorkerID)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Parallel()
		got := ResolveWorker(WorkerConfig{ServerAddr: "flag:1", WorkerID: "w-flag"}, env(map[string]string{
			EnvWorkerServer: "srv:50051",
			EnvWorkerID:     "w-env",
		}), host)
		if got.ServerAddr != "flag:1" || got.WorkerID != "w-flag" {
			t.Fatalf("flags should win: %+v", got)
		}
	})
}

func TestResolveHeartbeatInterval(t *testing.T) {
	t.Parallel()
	// Flag wins.
	if got := ResolveHeartbeatInterval(5*time.Second, env(map[string]string{EnvHeartbeatInterval: "9s"})); got != 5*time.Second {
		t.Fatalf("flag precedence: got %v", got)
	}
	// Env when no flag.
	if got := ResolveHeartbeatInterval(0, env(map[string]string{EnvHeartbeatInterval: "9s"})); got != 9*time.Second {
		t.Fatalf("env precedence: got %v", got)
	}
	// Default when neither.
	if got := ResolveHeartbeatInterval(0, env(nil)); got != DefaultHeartbeatInterval {
		t.Fatalf("default: got %v", got)
	}
	// Unparseable env falls back to default.
	if got := ResolveHeartbeatInterval(0, env(map[string]string{EnvHeartbeatInterval: "not-a-duration"})); got != DefaultHeartbeatInterval {
		t.Fatalf("bad env should fall back to default: got %v", got)
	}
}

func TestResolveOllamaURL(t *testing.T) {
	t.Parallel()
	// Flag wins over env.
	if got := ResolveOllamaURL("http://flag:1", env(map[string]string{EnvOllamaURL: "http://env:2"})); got != "http://flag:1" {
		t.Fatalf("flag precedence: got %q", got)
	}
	// Env when no flag.
	if got := ResolveOllamaURL("", env(map[string]string{EnvOllamaURL: "http://env:2"})); got != "http://env:2" {
		t.Fatalf("env precedence: got %q", got)
	}
	// Default when neither.
	if got := ResolveOllamaURL("", env(nil)); got != DefaultOllamaURL {
		t.Fatalf("default: got %q", got)
	}
}

func TestResolveHeartbeatTimeout(t *testing.T) {
	t.Parallel()
	if got := ResolveHeartbeatTimeout(90*time.Second, env(map[string]string{EnvHeartbeatTimeout: "120s"})); got != 90*time.Second {
		t.Fatalf("flag precedence: got %v", got)
	}
	if got := ResolveHeartbeatTimeout(0, env(map[string]string{EnvHeartbeatTimeout: "120s"})); got != 120*time.Second {
		t.Fatalf("env precedence: got %v", got)
	}
	if got := ResolveHeartbeatTimeout(0, env(nil)); got != DefaultHeartbeatTimeout {
		t.Fatalf("default: got %v", got)
	}
}
