package config

import (
	"errors"
	"path/filepath"
	"testing"
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
