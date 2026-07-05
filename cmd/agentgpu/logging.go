package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/config"
)

// redactedPlaceholder replaces the value of any attribute whose key names a
// known secret. It is a constant, non-empty string so a redacted field is
// visibly present (proving redaction happened) without revealing the value.
const redactedPlaceholder = "[REDACTED]"

// sensitiveKeys is the case-insensitive set of attribute key names whose values
// must never appear in logs (#23). It is the stringly-typed backstop to the
// type-level redaction (store.APIKey.LogValue): even if a raw secret/token/hash
// is logged under one of these keys by mistake, the ReplaceAttr below masks it.
// Keys are stored lowercase and matched case-insensitively.
var sensitiveKeys = map[string]struct{}{
	"secret":        {},
	"secret_hash":   {},
	"secrethash":    {},
	"salt":          {},
	"token":         {},
	"authorization": {},
	"password":      {},
	"api_key":       {},
	"apikey":        {},
	"bearer":        {},
}

// redactAttr is the slog ReplaceAttr that masks the value of any attribute whose
// key names a secret (case-insensitive, see sensitiveKeys). It is applied at
// every nesting level by slog, so a sensitive key inside a group (e.g. an
// accidentally-logged credential struct) is masked too. Non-sensitive attributes
// pass through unchanged, so the redaction adds no cost to ordinary fields beyond
// a map lookup. groups is unused (matching is by leaf key name, which is
// sufficient for the known-sensitive set) but required by the ReplaceAttr
// signature.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		a.Value = slog.StringValue(redactedPlaceholder)
	}
	return a
}

// parseLevel maps a configured level name onto an slog.Level. Recognized names
// are debug|info|warn|error (case-insensitive). An unrecognized value falls back
// to info rather than erroring, so a typo cannot wedge startup; the chosen level
// is reported via ok so the caller can note the fallback if desired.
func parseLevel(name string) (level slog.Level, ok bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// openLogOutput resolves the configured output name to a writer and a closer.
// "stderr"/"stdout" (case-insensitive) map to the corresponding standard stream
// with a no-op closer (the process must not close those). Any other value is
// treated as a file path: it is opened append-or-create, and the returned closer
// closes that file so the caller can flush it on shutdown. A path that cannot be
// opened returns an error so a misconfigured sink is loud rather than silently
// dropping logs.
func openLogOutput(name string) (io.Writer, func() error, error) {
	noop := func() error { return nil }
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "stderr", "":
		return os.Stderr, noop, nil
	case "stdout":
		return os.Stdout, noop, nil
	default:
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log output %q: %w", name, err)
		}
		return f, f.Close, nil
	}
}

// logRingCapacity is the number of most-recent log records the in-memory ring
// retains (#90 foundation for the log-stream/level admin endpoints in #92/#99).
// It is a fixed, modest cap: enough for an operator to inspect recent activity
// without unbounded memory growth.
const logRingCapacity = 1024

// logHandle bundles the root logger with the two control seams a later admin
// endpoint (#92/#99) needs but which are wired here, at the single
// logging-configuration site: a *slog.LevelVar to flip the verbosity at runtime,
// and the in-memory ring buffer of recent records. It also carries the file-sink
// closer. The plain newLogger wrapper discards the extra seams for callers that
// do not need them (so existing call sites and tests are unchanged).
type logHandle struct {
	Logger *slog.Logger
	// Level is the dynamic log level: SetLevel changes what is emitted (and rung)
	// process-wide with no restart. It is the seam #92 flips from an admin route.
	Level *slog.LevelVar
	// Ring is the bounded in-memory buffer of recent log records (redacted). It is
	// the seam #92/#99 reads to stream recent logs from an admin route.
	Ring *logRing
	// Close flushes/closes a file sink (a no-op for stderr/stdout).
	Close func() error
}

// SetLevel changes the dynamic log level at runtime (the seam #92 exposes over
// an admin route). It is safe for concurrent use (slog.LevelVar is).
func (h *logHandle) SetLevel(level slog.Level) { h.Level.Set(level) }

// newLoggerHandle builds the root logger plus its runtime-control seams (the
// dynamic level var and the in-memory ring), at the single place logging is
// configured (#23/#90). The terminal handler (JSON by default, text on request)
// is wrapped in a ringHandler so every emitted record is also appended to the
// ring; the level var governs both what is emitted and what is buffered.
//
// Redaction is installed via redactAttr (the stringly-typed backstop) on the
// terminal handler AND re-applied by the ring handler to its captured copy; the
// type-level guarantee lives on store.APIKey.LogValue. Together they ensure no
// secret material reaches either the logs or the ring.
//
// It returns an error only when a file sink cannot be opened. An unrecognized
// level or format degrades to the info/json defaults rather than erroring, so a
// typo in an env var cannot stop the process from starting.
func newLoggerHandle(cfg config.LogConfig) (*logHandle, error) {
	initial, _ := parseLevel(cfg.Level)
	levelVar := new(slog.LevelVar)
	levelVar.Set(initial)

	out, closeFn, err := openLogOutput(cfg.Output)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{
		Level:       levelVar,
		ReplaceAttr: redactAttr,
	}

	var terminal slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "text":
		terminal = slog.NewTextHandler(out, opts)
	default:
		// json is the default and the fallback for any unrecognized format, so logs
		// stay structured and parseable unless text is explicitly requested.
		terminal = slog.NewJSONHandler(out, opts)
	}

	ring := newLogRing(logRingCapacity)
	handler := newRingHandler(terminal, ring)

	return &logHandle{
		Logger: slog.New(handler),
		Level:  levelVar,
		Ring:   ring,
		Close:  closeFn,
	}, nil
}

// newLogger builds the single root *slog.Logger both the server and worker
// subcommands inherit (#23). It is the thin, backward-compatible wrapper over
// newLoggerHandle for callers that only need the logger + sink closer (most
// callers and the logging tests); the dynamic level var and in-memory ring it
// also builds are reachable via newLoggerHandle when a caller needs them (#92).
//
// It returns the logger, a closer that flushes/closes a file sink (a no-op for
// stderr/stdout), and an error only when a file sink cannot be opened. The
// resolved config comes from config.ResolveLog (flag > env > default).
func newLogger(cfg config.LogConfig) (*slog.Logger, func() error, error) {
	h, err := newLoggerHandle(cfg)
	if err != nil {
		return nil, nil, err
	}
	return h.Logger, h.Close, nil
}
