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

// newLogger builds the single root *slog.Logger both the server and worker
// subcommands inherit (#23). It is the one place logging is configured, so the
// level, encoding, sink, and secret redaction are uniform across the whole
// process with no per-subsystem duplication.
//
// It returns the logger, a closer that flushes/closes a file sink (a no-op for
// stderr/stdout), and an error only when a file sink cannot be opened. The
// resolved config comes from config.ResolveLog (flag > env > default), so the
// level is configurable without a code change. An unrecognized level or format
// degrades to the info/json defaults rather than erroring, so a typo in an env
// var cannot stop the process from starting.
//
// Redaction is installed here via redactAttr (the stringly-typed backstop); the
// type-level guarantee lives on store.APIKey.LogValue. Both together ensure no
// secret material reaches the logs.
func newLogger(cfg config.LogConfig) (*slog.Logger, func() error, error) {
	level, _ := parseLevel(cfg.Level)

	out, closeFn, err := openLogOutput(cfg.Output)
	if err != nil {
		return nil, nil, err
	}

	opts := &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactAttr,
	}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "text":
		handler = slog.NewTextHandler(out, opts)
	default:
		// json is the default and the fallback for any unrecognized format, so logs
		// stay structured and parseable unless text is explicitly requested.
		handler = slog.NewJSONHandler(out, opts)
	}

	return slog.New(handler), closeFn, nil
}
