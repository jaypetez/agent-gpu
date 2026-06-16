// Package config resolves agent-gpu process configuration from flags and
// environment variables. A file-based source is intentionally left as a seam
// for a later epic; the precedence today is: flag > environment > default.
package config

import (
	"os"
	"path/filepath"
	"time"
)

// Environment variable names.
const (
	EnvServerListen      = "AGENTGPU_SERVER_LISTEN"
	EnvHTTPListen        = "AGENTGPU_HTTP_LISTEN"
	EnvWorkerServer      = "AGENTGPU_SERVER_ADDR"
	EnvWorkerID          = "AGENTGPU_WORKER_ID"
	EnvStorePath         = "AGENTGPU_STORE_PATH"
	EnvQuotaPath         = "AGENTGPU_QUOTA_PATH"
	EnvHeartbeatInterval = "AGENTGPU_HEARTBEAT_INTERVAL"
	EnvHeartbeatTimeout  = "AGENTGPU_HEARTBEAT_TIMEOUT"
	EnvOllamaURL         = "AGENTGPU_OLLAMA_URL"
)

// Defaults.
const (
	DefaultServerListen = "127.0.0.1:50051"
	// DefaultHTTPListen is the address the public HTTP API binds by default.
	DefaultHTTPListen = "127.0.0.1:8080"
	// DefaultHeartbeatInterval is the worker's heartbeat cadence.
	DefaultHeartbeatInterval = 15 * time.Second
	// DefaultHeartbeatTimeout is the server's stale-eviction window (3x the
	// interval, so a single dropped heartbeat does not evict a live worker).
	DefaultHeartbeatTimeout = 45 * time.Second
	// DefaultOllamaURL is the address a local Ollama listens on by default.
	DefaultOllamaURL = "http://localhost:11434"
)

// ServerConfig configures the server process.
type ServerConfig struct {
	// Listen is the gRPC listen address (host:port).
	Listen string
	// HTTPListen is the public HTTP API listen address (host:port). It fronts the
	// OpenAI-compatible API (model discovery #12, chat/completions #13).
	HTTPListen string
}

// WorkerConfig configures the worker process.
type WorkerConfig struct {
	// ServerAddr is the gRPC server address to connect to.
	ServerAddr string
	// WorkerID is this worker's stable identifier; defaults to the hostname.
	WorkerID string
}

// QuotaConfig configures the quota engine's global default limits and the
// counter checkpoint file location. The limit fields mirror store.Limits /
// quota.Limits but are kept as plain values here so the config package stays
// free of a quota dependency (matching how ServerConfig avoids importing
// server). A zero limit field means "unlimited" for that dimension.
type QuotaConfig struct {
	// Path is the counter checkpoint file location.
	Path string
	// DefaultRPM is the global default requests-per-minute (0 = unlimited).
	DefaultRPM uint64
	// DefaultTPM is the global default tokens-per-minute (0 = unlimited).
	DefaultTPM uint64
	// DefaultDailyTokens is the global default daily token budget (0 = unlimited).
	DefaultDailyTokens uint64
	// DefaultMonthlyTokens is the global default monthly token budget (0 = unlimited).
	DefaultMonthlyTokens uint64
}

// EnvLookup is the signature of os.LookupEnv, injectable for tests.
type EnvLookup func(string) (string, bool)

// envOr returns the env value if set and non-empty, else the fallback.
func envOr(look EnvLookup, key, fallback string) string {
	if v, ok := look(key); ok && v != "" {
		return v
	}
	return fallback
}

// durationEnvOr returns the duration parsed from the env value if set and
// parseable, else the fallback. An unparseable value falls back silently so a
// typo cannot wedge startup.
func durationEnvOr(look EnvLookup, key string, fallback time.Duration) time.Duration {
	if v, ok := look(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// ResolveServer applies env-then-default resolution to a ServerConfig whose
// fields hold flag values (empty meaning "unset"). The CLI layer passes flag
// values in; this fills the gaps from the environment and defaults.
func ResolveServer(flags ServerConfig, look EnvLookup) ServerConfig {
	if look == nil {
		look = os.LookupEnv
	}
	out := flags
	if out.Listen == "" {
		out.Listen = envOr(look, EnvServerListen, DefaultServerListen)
	}
	if out.HTTPListen == "" {
		out.HTTPListen = envOr(look, EnvHTTPListen, DefaultHTTPListen)
	}
	return out
}

// ResolveHeartbeatInterval resolves the worker heartbeat cadence with flag >
// env > default precedence. A non-positive flag value means "unset".
func ResolveHeartbeatInterval(flagValue time.Duration, look EnvLookup) time.Duration {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return durationEnvOr(look, EnvHeartbeatInterval, DefaultHeartbeatInterval)
}

// ResolveHeartbeatTimeout resolves the server stale-eviction window with flag >
// env > default precedence. A non-positive flag value means "unset".
func ResolveHeartbeatTimeout(flagValue time.Duration, look EnvLookup) time.Duration {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return durationEnvOr(look, EnvHeartbeatTimeout, DefaultHeartbeatTimeout)
}

// ResolveOllamaURL resolves the worker's local Ollama base URL with flag > env
// > default precedence. An empty flag value means "unset".
func ResolveOllamaURL(flagValue string, look EnvLookup) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvOllamaURL, DefaultOllamaURL)
}

// DefaultStorePath returns the default keys-file location, ~/.agentgpu/keys.json,
// using homeDir (os.UserHomeDir if nil) to resolve the home directory. If the
// home directory cannot be determined it falls back to a relative path so the
// CLI still works in constrained environments.
func DefaultStorePath(homeDir func() (string, error)) string {
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil || home == "" {
		return filepath.Join(".agentgpu", "keys.json")
	}
	return filepath.Join(home, ".agentgpu", "keys.json")
}

// ResolveStorePath resolves the keys-file path with flag > env > default
// precedence. An empty flag value means "unset".
func ResolveStorePath(flagValue string, look EnvLookup, homeDir func() (string, error)) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvStorePath, DefaultStorePath(homeDir))
}

// DefaultQuotaPath returns the default counter-checkpoint location,
// ~/.agentgpu/quota.json, falling back to a relative path when the home
// directory cannot be determined (mirroring DefaultStorePath).
func DefaultQuotaPath(homeDir func() (string, error)) string {
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil || home == "" {
		return filepath.Join(".agentgpu", "quota.json")
	}
	return filepath.Join(home, ".agentgpu", "quota.json")
}

// ResolveQuotaPath resolves the counter-checkpoint path with flag > env >
// default precedence. An empty flag value means "unset".
func ResolveQuotaPath(flagValue string, look EnvLookup, homeDir func() (string, error)) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvQuotaPath, DefaultQuotaPath(homeDir))
}

// ResolveQuota fills a QuotaConfig's Path with flag > env > default precedence
// (limit defaults are passed through as-is; they have no env override today).
func ResolveQuota(flags QuotaConfig, look EnvLookup, homeDir func() (string, error)) QuotaConfig {
	out := flags
	out.Path = ResolveQuotaPath(flags.Path, look, homeDir)
	return out
}

// ResolveWorker applies env-then-default resolution to a WorkerConfig.
func ResolveWorker(flags WorkerConfig, look EnvLookup, hostname func() (string, error)) WorkerConfig {
	if look == nil {
		look = os.LookupEnv
	}
	if hostname == nil {
		hostname = os.Hostname
	}
	out := flags
	if out.ServerAddr == "" {
		out.ServerAddr = envOr(look, EnvWorkerServer, "")
	}
	if out.WorkerID == "" {
		out.WorkerID = envOr(look, EnvWorkerID, "")
	}
	if out.WorkerID == "" {
		if h, err := hostname(); err == nil {
			out.WorkerID = h
		}
	}
	return out
}
