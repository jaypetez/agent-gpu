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
	EnvServerListen = "AGENTGPU_SERVER_LISTEN"
	EnvHTTPListen   = "AGENTGPU_HTTP_LISTEN"
	// EnvMetricsListen is the bind address of the dedicated Prometheus metrics
	// listener (#24), which serves only /metrics and is intentionally separate
	// from the public API port (so scraping needs no API auth). Set it empty to
	// disable the metrics listener entirely.
	EnvMetricsListen     = "AGENTGPU_METRICS_LISTEN"
	EnvWorkerServer      = "AGENTGPU_SERVER_ADDR"
	EnvWorkerID          = "AGENTGPU_WORKER_ID"
	EnvStorePath         = "AGENTGPU_STORE_PATH"
	EnvQuotaPath         = "AGENTGPU_QUOTA_PATH"
	EnvHeartbeatInterval = "AGENTGPU_HEARTBEAT_INTERVAL"
	EnvHeartbeatTimeout  = "AGENTGPU_HEARTBEAT_TIMEOUT"
	EnvOllamaURL         = "AGENTGPU_OLLAMA_URL"
	EnvSessionPath       = "AGENTGPU_SESSION_PATH"
	EnvSessionTTL        = "AGENTGPU_SESSION_TTL"
	// EnvMaxSessionsPerKey caps the number of concurrent active sessions a single
	// owner key may hold (#37). 0 = unlimited.
	EnvMaxSessionsPerKey = "AGENTGPU_MAX_SESSIONS_PER_KEY"
	// EnvSessionMaxTurns / EnvSessionMaxContextTokens override the per-session
	// conversation turn cap and the cumulative context-token cap (#37). 0 =
	// unlimited for that dimension.
	EnvSessionMaxTurns         = "AGENTGPU_MAX_SESSION_TURNS"
	EnvSessionMaxContextTokens = "AGENTGPU_MAX_SESSION_CONTEXT_TOKENS"
	// EnvSessionOverflowPolicy selects what happens when a per-session history cap
	// is exceeded: "trim" (drop oldest turns — the default, non-breaking) or
	// "reject" (refuse the append). See internal/session.OverflowPolicy.
	EnvSessionOverflowPolicy = "AGENTGPU_SESSION_OVERFLOW_POLICY"
	// EnvModelWarmMax caps the model-warmth keep_alive window (#35): the longest a
	// session-bound model is kept resident on its worker after a turn. The server
	// sends keep_alive = min(session TTL, this cap) so an idle/abandoned session's
	// model unloads within a bounded window and never pins VRAM indefinitely. A
	// session with no idle TTL falls back to exactly this cap.
	EnvModelWarmMax = "AGENTGPU_MODEL_WARM_MAX"
	// EnvGPUDetect toggles automatic GPU detection on the worker (#16). When
	// false, detection is skipped and the manual EnvGPUType/EnvTotalVRAM (or their
	// flags) describe the worker's capacity instead.
	EnvGPUDetect = "AGENTGPU_GPU_DETECT"
	// EnvGPUType / EnvTotalVRAM are manual capacity overrides used when detection
	// is disabled or the vendor CLI is not on PATH. EnvTotalVRAM is in bytes.
	EnvGPUType   = "AGENTGPU_GPU_TYPE"
	EnvTotalVRAM = "AGENTGPU_TOTAL_VRAM"
	// EnvGlobalRPM / EnvGlobalTPM configure the server-wide (global) rate limits
	// enforced at the HTTP request boundary (#6), independent of per-key quota.
	EnvGlobalRPM = "AGENTGPU_GLOBAL_RPM"
	EnvGlobalTPM = "AGENTGPU_GLOBAL_TPM"
	// EnvHTTPAddr is the base URL of the public HTTP API the `agentgpu` CLI
	// targets when managing a RUNNING server (key/quota/models commands), e.g.
	// http://127.0.0.1:8080. It is the client-side counterpart of EnvHTTPListen
	// (the server's bind address): a full URL with scheme, not a bare host:port,
	// and deliberately distinct from EnvWorkerServer (the worker→server gRPC addr)
	// so the HTTP client never targets the gRPC control plane.
	EnvHTTPAddr = "AGENTGPU_HTTP_ADDR"
	// EnvToken is the admin Bearer token the CLI authenticates with against the
	// running server's admin API (agpu_<id>_<secret>).
	EnvToken = "AGENTGPU_TOKEN"
	// EnvLogLevel / EnvLogFormat / EnvLogOutput configure structured logging (#23)
	// for both the server and worker processes. Level is debug|info|warn|error;
	// format is json|text (json by default so logs are aggregator-ready, text for
	// local dev); output is stderr|stdout|<file path>. They are resolved once at
	// startup (flag > env > default) so the log level is configurable without a
	// code change.
	EnvLogLevel  = "AGENTGPU_LOG_LEVEL"
	EnvLogFormat = "AGENTGPU_LOG_FORMAT"
	EnvLogOutput = "AGENTGPU_LOG_OUTPUT"
)

// Defaults.
const (
	DefaultServerListen = "127.0.0.1:50051"
	// DefaultHTTPListen is the address the public HTTP API binds by default.
	DefaultHTTPListen = "127.0.0.1:8080"
	// DefaultMetricsListen is the address the Prometheus metrics listener binds by
	// default (#24): loopback only, so /metrics is reachable for local scraping
	// but not exposed off-box without an explicit override. Set the flag/env to
	// empty to disable the listener.
	DefaultMetricsListen = "127.0.0.1:9090"
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
	// DefaultMaxSessionsPerKey is the default concurrent-session cap per owner key
	// (#37): 0 = unlimited, preserving prior behavior. Operators set a positive cap
	// to bound how many simultaneous conversations a single key can hold.
	DefaultMaxSessionsPerKey = 0
	// DefaultSessionMaxContextTokens is the default per-session cumulative
	// context-token cap (#37): 0 = unlimited. The byte cap already bounds memory; a
	// token cap is opt-in for operators who want to bound conversation context size
	// by an (estimated) token count instead of raw bytes.
	DefaultSessionMaxContextTokens = 0
	// DefaultSessionOverflowPolicy is the default over-cap behavior for per-session
	// history (#37): trim oldest turns, preserving the pre-#37 non-breaking
	// behavior. "reject" is the opt-in hard-ceiling alternative.
	DefaultSessionOverflowPolicy = "trim"
	// DefaultModelWarmMax is the default cap on the model-warmth keep_alive window
	// (#35): a session-bound model is kept resident on its worker for at most this
	// long after a turn. One hour gives generous headroom over the 30-minute
	// default session TTL (so the common case is never clipped) while still
	// bounding how long an idle/abandoned session can pin VRAM. The keep_alive sent
	// to Ollama is min(session TTL, this cap); a never-idle session uses this cap.
	DefaultModelWarmMax = time.Hour
	// DefaultGPUDetect is the default for the worker's automatic GPU detection
	// (#16): on, so a worker advertises real hardware capacity out of the box.
	DefaultGPUDetect = true
	// DefaultHTTPAddr is the base URL the `agentgpu` CLI targets when no
	// --server/--url flag or AGENTGPU_HTTP_ADDR is set: the loopback HTTP API on
	// the default port (the http:// counterpart of DefaultHTTPListen).
	DefaultHTTPAddr = "http://127.0.0.1:8080"
	// DefaultLogLevel is the default structured-logging level (#23): info, so
	// routine lifecycle/decision lines are emitted while debug-only verbosity is
	// filtered out by default.
	DefaultLogLevel = "info"
	// DefaultLogFormat is the default log encoding (#23): json, so logs are
	// structured and parseable for aggregators out of the box. "text" is selectable
	// for human-friendly local development.
	DefaultLogFormat = "json"
	// DefaultLogOutput is the default log sink (#23): stderr, so logs do not
	// intermix with any stdout protocol output and follow the twelve-factor
	// convention. "stdout" or a file path are selectable alternatives.
	DefaultLogOutput = "stderr"
)

// ServerConfig configures the server process.
type ServerConfig struct {
	// Listen is the gRPC listen address (host:port).
	Listen string
	// HTTPListen is the public HTTP API listen address (host:port). It fronts the
	// OpenAI-compatible API (model discovery #12, chat/completions #13).
	HTTPListen string
	// MetricsListen is the Prometheus metrics listener address (host:port), a
	// dedicated port serving only /metrics, separate from the API (#24). An empty
	// resolved value disables the metrics listener; see ResolveServer for how an
	// explicitly-set-empty flag/env disables it vs. an unset one defaulting on.
	MetricsListen string
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
	// limiting off). The boot value comes from here; it is also runtime-tunable via
	// PUT /v1/admin/config (#92), which calls quota.Engine.SetGlobalLimits to change
	// it live with no restart.
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
	// MaxContextTokens is the per-session cumulative estimated context-token cap
	// (#37; 0 = unbounded). The estimate is a whitespace-token count, matching the
	// rest of the project's token accounting (there is no real tokenizer).
	MaxContextTokens int
	// MaxSessionsPerKey caps the concurrent active sessions one owner key may hold
	// (#37; 0 = unlimited).
	MaxSessionsPerKey int
	// OverflowPolicy is the over-cap behavior for history (#37): "trim" (drop
	// oldest, default) or "reject" (refuse the append). An empty value resolves to
	// DefaultSessionOverflowPolicy; an unrecognized value falls back to "trim" when
	// parsed by internal/session.ParseOverflowPolicy.
	OverflowPolicy string
	// ModelWarmMax caps the model-warmth keep_alive window (#35): the longest a
	// session-bound model is kept resident on its worker after a turn. 0 selects
	// DefaultModelWarmMax via ResolveSession. The server sends keep_alive =
	// min(session TTL, this cap), so an idle/abandoned session's model unloads
	// within a bounded window; a never-idle session falls back to this cap.
	ModelWarmMax time.Duration
}

// LogConfig configures structured logging (#23) for the server and worker
// processes: the minimum level emitted, the encoding, and the sink. It is a
// plain value struct (no slog dependency) so the config package stays leaf,
// mirroring QuotaConfig/SessionConfig; the cmd layer turns it into an
// *slog.Logger (see cmd/agentgpu/logging.go). The same resolved config seeds the
// single root logger both subcommands inherit, so logging is configured in one
// place with no per-subsystem duplication.
type LogConfig struct {
	// Level is the minimum level emitted: debug|info|warn|error. An unrecognized
	// value falls back to DefaultLogLevel when the logger is built.
	Level string
	// Format is the encoding: json (structured/aggregator-ready) or text
	// (human-friendly). An unrecognized value falls back to DefaultLogFormat.
	Format string
	// Output is the sink: stderr, stdout, or a writable file path. A file path
	// that cannot be opened surfaces as a startup error rather than a silent
	// fallback, so a misconfigured sink is loud.
	Output string
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

// intEnvOr returns the non-negative integer parsed from the env value if set and
// parseable, else the fallback. A negative or unparseable value falls back
// silently so a typo cannot wedge startup (mirroring uintEnvOr); it is used for
// the count-style session caps (turns, context tokens, sessions-per-key) which
// are plain ints elsewhere.
func intEnvOr(look EnvLookup, key string, fallback int) int {
	if v, ok := look(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

// boolEnvOr returns the boolean parsed from the env value if set and parseable
// (via strconv.ParseBool, so "1"/"true"/"0"/"false"/etc.), else the fallback. An
// unparseable value falls back silently so a typo cannot wedge startup.
func boolEnvOr(look EnvLookup, key string, fallback bool) bool {
	if v, ok := look(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// MetricsListenDisabled is the sentinel a caller stores in
// ServerConfig.MetricsListen to disable the metrics listener explicitly, so an
// intentional "off" survives ResolveServer rather than being refilled from the
// environment/default. The CLI maps an explicitly-passed empty --metrics-listen
// (or empty AGENTGPU_METRICS_LISTEN) to this so "off" is sticky; the resolved
// value is then mapped back to "" (disabled) for the lifecycle layer.
const MetricsListenDisabled = "-"

// ResolveServer applies env-then-default resolution to a ServerConfig whose
// fields hold flag values (empty meaning "unset"). The CLI layer passes flag
// values in; this fills the gaps from the environment and defaults.
//
// MetricsListen (#24) mirrors HTTPListen's flag > env > default precedence with
// one addition: the MetricsListenDisabled sentinel ("-") is preserved verbatim so
// an operator can turn the metrics listener off and have that decision stick
// (the cmd layer translates an explicit empty flag/env into the sentinel, then
// maps the resolved sentinel back to "" = disabled). An unset MetricsListen
// resolves to DefaultMetricsListen (the listener is on by default).
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
	if out.MetricsListen == "" {
		out.MetricsListen = envOr(look, EnvMetricsListen, DefaultMetricsListen)
	}
	return out
}

// MetricsListenAddr maps a resolved ServerConfig.MetricsListen to the effective
// bind address for the lifecycle layer: the MetricsListenDisabled sentinel ("-")
// becomes "" (the listener is disabled), and any other value is returned as-is.
// Centralizing the mapping keeps the cmd wiring a single readable check.
func MetricsListenAddr(resolved string) string {
	if resolved == MetricsListenDisabled {
		return ""
	}
	return resolved
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

// ResolveHTTPAddr resolves the base URL the CLI targets for the public HTTP API
// (the admin + catalog endpoints) with flag > env > default precedence. An empty
// flag value means "unset". The result is a full URL (scheme + host:port), e.g.
// http://127.0.0.1:8080 — it is consumed by internal/apiclient, never used as a
// gRPC dial target, so it must not be confused with EnvWorkerServer.
func ResolveHTTPAddr(flagValue string, look EnvLookup) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvHTTPAddr, DefaultHTTPAddr)
}

// ResolveToken resolves the admin Bearer token the CLI authenticates with, using
// flag > env > default("") precedence. An empty flag value means "unset"; an
// empty result means "no token configured" (the CLI then errors with guidance to
// either provide --token or use --local for offline store management).
func ResolveToken(flagValue string, look EnvLookup) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvToken, "")
}

// ResolveGPUDetect resolves the worker's automatic-GPU-detection toggle (#16)
// with flag > env > default precedence. Because the flag is a bool whose zero
// value (false) is indistinguishable from "unset", the caller passes flagSet to
// signal whether the user actually provided --gpu-detect; only then does the
// flag win. Otherwise AGENTGPU_GPU_DETECT, then DefaultGPUDetect (true), apply.
func ResolveGPUDetect(flagValue, flagSet bool, look EnvLookup) bool {
	if look == nil {
		look = os.LookupEnv
	}
	if flagSet {
		return flagValue
	}
	return boolEnvOr(look, EnvGPUDetect, DefaultGPUDetect)
}

// ResolveGPUType resolves the manual GPU-type override (#16) with flag > env >
// default("") precedence. An empty flag value means "unset". It is used as the
// reported GPU type when detection is disabled or the vendor CLI is unavailable;
// empty means "no manual override".
func ResolveGPUType(flagValue string, look EnvLookup) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvGPUType, "")
}

// ResolveTotalVRAM resolves the manual total-VRAM override in bytes (#16) with
// flag > env > default(0) precedence. A zero flag value means "unset". It pairs
// with ResolveGPUType for hosts where automatic detection is off or impossible.
func ResolveTotalVRAM(flagValue uint64, look EnvLookup) uint64 {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != 0 {
		return flagValue
	}
	return uintEnvOr(look, EnvTotalVRAM, 0)
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

// ResolveModelWarmMax resolves the model-warmth keep_alive cap (#35) with flag >
// env > default precedence. A non-positive flag value means "unset"; the result
// is always positive (DefaultModelWarmMax when nothing else is configured) so the
// derived warm window is always bounded.
func ResolveModelWarmMax(flagValue time.Duration, look EnvLookup) time.Duration {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return durationEnvOr(look, EnvModelWarmMax, DefaultModelWarmMax)
}

// ResolveSessionMaxTurns resolves the per-session turn cap with flag > env >
// default precedence (#37). A non-positive flag value means "unset"; the result
// is the env value, then DefaultSessionMaxTurns. (Unlike the unlimited-by-default
// caps below, the turn cap keeps its long-standing 200 default so history is
// always bounded.)
func ResolveSessionMaxTurns(flagValue int, look EnvLookup) int {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return intEnvOr(look, EnvSessionMaxTurns, DefaultSessionMaxTurns)
}

// ResolveMaxSessionsPerKey resolves the concurrent-session-per-key cap with flag
// > env > default(0=unlimited) precedence (#37). A non-positive flag value means
// "unset"; the env is then consulted, defaulting to unlimited.
func ResolveMaxSessionsPerKey(flagValue int, look EnvLookup) int {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return intEnvOr(look, EnvMaxSessionsPerKey, DefaultMaxSessionsPerKey)
}

// ResolveSessionMaxContextTokens resolves the per-session context-token cap with
// flag > env > default(0=unlimited) precedence (#37). A non-positive flag value
// means "unset".
func ResolveSessionMaxContextTokens(flagValue int, look EnvLookup) int {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue > 0 {
		return flagValue
	}
	return intEnvOr(look, EnvSessionMaxContextTokens, DefaultSessionMaxContextTokens)
}

// ResolveSessionOverflowPolicy resolves the history over-cap policy string with
// flag > env > default("trim") precedence (#37). An empty flag value means
// "unset". The returned string is validated/parsed by
// internal/session.ParseOverflowPolicy at wiring time (an unrecognized value
// falls back to trim there), mirroring how ResolveLog leaves level/format
// spelling validation to the logger builder.
func ResolveSessionOverflowPolicy(flagValue string, look EnvLookup) string {
	if look == nil {
		look = os.LookupEnv
	}
	if flagValue != "" {
		return flagValue
	}
	return envOr(look, EnvSessionOverflowPolicy, DefaultSessionOverflowPolicy)
}

// ResolveSession fills a SessionConfig with flag > env > default precedence for
// the path, TTL, model-warmth cap, history caps (turns/bytes/context-tokens),
// the concurrent-session-per-key cap, and the overflow policy (#37). MaxBytes
// keeps its passed-through-then-defaulted handling (it changes rarely and has no
// env override); the other caps resolve flag > env > default via their helpers.
func ResolveSession(flags SessionConfig, look EnvLookup, homeDir func() (string, error)) SessionConfig {
	out := flags
	out.Path = ResolveSessionPath(flags.Path, look, homeDir)
	out.TTL = ResolveSessionTTL(flags.TTL, look)
	out.ModelWarmMax = ResolveModelWarmMax(flags.ModelWarmMax, look)
	out.MaxTurns = ResolveSessionMaxTurns(flags.MaxTurns, look)
	out.MaxContextTokens = ResolveSessionMaxContextTokens(flags.MaxContextTokens, look)
	out.MaxSessionsPerKey = ResolveMaxSessionsPerKey(flags.MaxSessionsPerKey, look)
	out.OverflowPolicy = ResolveSessionOverflowPolicy(flags.OverflowPolicy, look)
	if out.MaxBytes <= 0 {
		out.MaxBytes = DefaultSessionMaxBytes
	}
	return out
}

// ResolveLog fills a LogConfig with flag > env > default precedence for each of
// the level, format, and output fields (#23). An empty flag field means "unset"
// (consult the env, then the default), so a process that passes no logging flags
// is configured entirely from the environment and defaults — the log level is
// thus changeable without a code change. Validation of the resolved values
// (level/format spelling, opening a file sink) happens when the logger is built;
// this only resolves the string precedence, mirroring ResolveServer/ResolveSession.
func ResolveLog(flags LogConfig, look EnvLookup) LogConfig {
	if look == nil {
		look = os.LookupEnv
	}
	out := flags
	if out.Level == "" {
		out.Level = envOr(look, EnvLogLevel, DefaultLogLevel)
	}
	if out.Format == "" {
		out.Format = envOr(look, EnvLogFormat, DefaultLogFormat)
	}
	if out.Output == "" {
		out.Output = envOr(look, EnvLogOutput, DefaultLogOutput)
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
