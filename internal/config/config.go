// Package config resolves agent-gpu process configuration from flags and
// environment variables. A file-based source is intentionally left as a seam
// for a later epic; the precedence today is: flag > environment > default.
package config

import (
	"os"
	"path/filepath"
	"strconv"
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
	EnvSessionPath       = "AGENTGPU_SESSION_PATH"
	EnvSessionTTL        = "AGENTGPU_SESSION_TTL"
	// EnvGlobalRPM / EnvGlobalTPM configure the server-wide (global) rate limits
	// enforced at the HTTP request boundary (#6), independent of per-key quota.
	EnvGlobalRPM = "AGENTGPU_GLOBAL_RPM"
	EnvGlobalTPM = "AGENTGPU_GLOBAL_TPM"
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
	// DefaultSessionTTL is the default per-session idle timeout (#33). A session
	// untouched for this long is reaped by the sweeper.
	DefaultSessionTTL = 30 * time.Minute
	// DefaultSessionMaxTurns is the default per-session conversation turn cap.
	DefaultSessionMaxTurns = 200
	// DefaultSessionMaxBytes is the default per-session cumulative-content byte
	// cap (1 MiB), bounding history memory growth from a long conversation.
	DefaultSessionMaxBytes = 1 << 20
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

	// GlobalRPM is the server-wide requests-per-minute cap enforced at the HTTP
	// boundary across the whole fleet, independent of per-key quota (#6). It is
	// resolved with flag > env > default precedence (0 = unlimited; global rate
	// limiting off). It is applied at load time; there is no hot-reload.
	GlobalRPM uint64
	// GlobalTPM is the server-wide tokens-per-minute cap, the token-budget analog
	// of GlobalRPM (0 = unlimited).
	GlobalTPM uint64
}

// SessionConfig configures the conversation-session subsystem (#33): where
// sessions and history are checkpointed, the per-session idle TTL, and the
// per-session history caps. It is a plain value struct (no session-package
// dependency) so the config package stays leaf, mirroring QuotaConfig. The
// cmd/HTTP wiring that consumes it lands with #36.
type SessionConfig struct {
	// Path is the session+history checkpoint file location.
	Path string
	// TTL is the per-session idle timeout (0 selects DefaultSessionTTL via
	// ResolveSession). A session untouched for TTL is reaped by the sweeper.
	TTL time.Duration
	// MaxTurns is the per-session conversation turn cap (0 = unbounded).
	MaxTurns int
	// MaxBytes is the per-session cumulative-content byte cap (0 = unbounded).
	MaxBytes int
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

// uintEnvOr returns the unsigned integer parsed from the env value if set and
// parseable, else the fallback. An unparseable value falls back silently so a
// typo cannot wedge startup (mirroring durationEnvOr).
func uintEnvOr(look EnvLookup, key string, fallback uint64) uint64 {
	if v, ok := look(key); ok && v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
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

// ResolveQuota fills a QuotaConfig with flag > env > default precedence. The
// per-key default limits are passed through as-is (no env override today), but
// the server-wide global limits (#6) resolve flag > env > 0 (unlimited): a zero
// flag value means "unset" so the AGENTGPU_GLOBAL_RPM/TPM env is consulted.
func ResolveQuota(flags QuotaConfig, look EnvLookup, homeDir func() (string, error)) QuotaConfig {
	if look == nil {
		look = os.LookupEnv
	}
	out := flags
	out.Path = ResolveQuotaPath(flags.Path, look, homeDir)
	if out.GlobalRPM == 0 {
		out.GlobalRPM = uintEnvOr(look, EnvGlobalRPM, 0)
	}
	if out.GlobalTPM == 0 {
		out.GlobalTPM = uintEnvOr(look, EnvGlobalTPM, 0)
	}
	return out
}

// DefaultSessionPath returns the default session-checkpoint location,
// ~/.agentgpu/sessions.json, falling back to a relative path when the home
// directory cannot be determined (mirroring DefaultStorePath/DefaultQuotaPath).
func DefaultSessionPath(homeDir func() (string, error)) string {
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil || home == "" {
		return filepath.Join(".agentgpu", "sessions.json")
	}
	return filepath.Join(home, ".agentgpu", "sessions.json")
}

// ResolveSessionPath resolves the session-checkpoint path with flag > env >
// default precedence. An empty flag value means "unset".
func ResolveSessionPath(flagValue string, look EnvLookup, homeDir func() (string, error)) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvSessionPath, DefaultSessionPath(homeDir))
}

// ResolveSessionTTL resolves the per-session idle TTL with flag > env > default
// precedence. A non-positive flag value means "unset".
func ResolveSessionTTL(flagValue time.Duration, look EnvLookup) time.Duration {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return durationEnvOr(look, EnvSessionTTL, DefaultSessionTTL)
}

// ResolveSession fills a SessionConfig with flag > env > default precedence for
// the path and TTL, and applies the cap defaults when a cap is left at zero
// ("unset"). The cap fields have no env override today (they change rarely),
// matching how QuotaConfig's limits are passed through.
func ResolveSession(flags SessionConfig, look EnvLookup, homeDir func() (string, error)) SessionConfig {
	out := flags
	out.Path = ResolveSessionPath(flags.Path, look, homeDir)
	out.TTL = ResolveSessionTTL(flags.TTL, look)
	if out.MaxTurns <= 0 {
		out.MaxTurns = DefaultSessionMaxTurns
	}
	if out.MaxBytes <= 0 {
		out.MaxBytes = DefaultSessionMaxBytes
	}
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
