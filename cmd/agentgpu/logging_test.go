package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// readLogFile builds a logger over a temp file, runs fn against it, closes the
// sink, and returns the file's contents. It is the harness for asserting on the
// actual bytes newLogger emits (format, level filtering, redaction).
func readLogFile(t *testing.T, cfg config.LogConfig, fn func(*slog.Logger)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentgpu.log")
	cfg.Output = path
	logger, closeFn, err := newLogger(cfg)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	fn(logger)
	if err := closeFn(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(b)
}

// TestNewLoggerJSONFormat proves the default (json) format emits structured,
// parseable lines: each line is a JSON object carrying the message and fields.
// (AC: logs structured & parseable.)
func TestNewLoggerJSONFormat(t *testing.T) {
	out := readLogFile(t, config.LogConfig{Level: "info", Format: "json"}, func(l *slog.Logger) {
		l.Info("hello", "model", "llama3", "n", 3)
	})
	line := strings.TrimSpace(out)
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("log line is not valid JSON (%q): %v", line, err)
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["model"] != "llama3" {
		t.Errorf("model = %v, want llama3", rec["model"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
}

// TestNewLoggerTextFormat proves selecting text yields the slog text encoding
// (key=value, not JSON) for human-friendly local development.
func TestNewLoggerTextFormat(t *testing.T) {
	out := readLogFile(t, config.LogConfig{Level: "info", Format: "text"}, func(l *slog.Logger) {
		l.Info("hello", "model", "llama3")
	})
	if !strings.Contains(out, "model=llama3") {
		t.Errorf("text output missing key=value field: %q", out)
	}
	if strings.Contains(out, `"msg"`) {
		t.Errorf("text output looks like JSON: %q", out)
	}
}

// TestNewLoggerLevelFiltering proves the configured level gates output: at info,
// a Debug line is suppressed; at debug, it is emitted. This is the lever that
// makes verbose logging opt-in (and the documented sampling alternative). (AC:
// configurable levels.)
func TestNewLoggerLevelFiltering(t *testing.T) {
	atInfo := readLogFile(t, config.LogConfig{Level: "info", Format: "json"}, func(l *slog.Logger) {
		l.Debug("verbose detail")
		l.Info("routine line")
	})
	if strings.Contains(atInfo, "verbose detail") {
		t.Errorf("Debug line emitted at info level: %q", atInfo)
	}
	if !strings.Contains(atInfo, "routine line") {
		t.Errorf("Info line missing at info level: %q", atInfo)
	}

	atDebug := readLogFile(t, config.LogConfig{Level: "debug", Format: "json"}, func(l *slog.Logger) {
		l.Debug("verbose detail")
	})
	if !strings.Contains(atDebug, "verbose detail") {
		t.Errorf("Debug line suppressed at debug level: %q", atDebug)
	}
}

// TestNewLoggerInvalidLevelFallsBackToInfo proves an unrecognized level does not
// wedge startup: it degrades to info (Debug suppressed, Info emitted) rather than
// erroring.
func TestNewLoggerInvalidLevelFallsBackToInfo(t *testing.T) {
	out := readLogFile(t, config.LogConfig{Level: "loud", Format: "json"}, func(l *slog.Logger) {
		l.Debug("verbose")
		l.Info("normal")
	})
	if strings.Contains(out, "verbose") {
		t.Errorf("Debug emitted under invalid level (should fall back to info): %q", out)
	}
	if !strings.Contains(out, "normal") {
		t.Errorf("Info missing under invalid level fallback: %q", out)
	}
}

// TestNewLoggerInvalidFormatFallsBackToJSON proves an unrecognized format
// degrades to json (structured) rather than erroring.
func TestNewLoggerInvalidFormatFallsBackToJSON(t *testing.T) {
	out := readLogFile(t, config.LogConfig{Level: "info", Format: "yaml"}, func(l *slog.Logger) {
		l.Info("hello", "k", "v")
	})
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("invalid format did not fall back to JSON (%q): %v", out, err)
	}
}

// TestNewLoggerBadFileOutputErrors proves a file sink that cannot be opened
// surfaces a loud startup error rather than silently dropping logs.
func TestNewLoggerBadFileOutputErrors(t *testing.T) {
	// A path whose parent directory does not exist cannot be opened.
	bad := filepath.Join(t.TempDir(), "missing-dir", "agentgpu.log")
	_, _, err := newLogger(config.LogConfig{Level: "info", Format: "json", Output: bad})
	if err == nil {
		t.Fatalf("expected an error for an unopenable log path, got nil")
	}
}

// TestNewLoggerStdStreamsCloserIsNoop proves stderr/stdout map to a no-op closer
// (the process must never close the standard streams).
func TestNewLoggerStdStreamsCloserIsNoop(t *testing.T) {
	for _, out := range []string{"stderr", "stdout", "STDERR"} {
		_, closeFn, err := newLogger(config.LogConfig{Level: "info", Format: "json", Output: out})
		if err != nil {
			t.Fatalf("newLogger(%q): %v", out, err)
		}
		if err := closeFn(); err != nil {
			t.Errorf("closer for %q returned %v, want nil no-op", out, err)
		}
	}
}

// TestParseLevel covers the level-name mapping including case-insensitivity and
// the invalid-value fallback signaled via ok.
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in     string
		want   slog.Level
		wantOK bool
	}{
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"  Error  ", slog.LevelError, true},
		{"nonsense", slog.LevelInfo, false},
	}
	for _, tc := range cases {
		got, ok := parseLevel(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseLevel(%q) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestRedactionNoSecretMaterial is the headline AC test: with the production
// redaction installed (newLogger's ReplaceAttr + APIKey.LogValue), logging a
// whole APIKey, a "token" attribute, and a "secret_hash" attribute leaks NONE of
// the secret material — not the token string, not the base64/hex of the
// SecretHash or Salt — and the sensitive keys are masked. (AC: no secret
// material in logs (tested).)
func TestRedactionNoSecretMaterial(t *testing.T) {
	secretHash := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	salt := []byte{0xCA, 0xFE, 0xBA, 0xBE, 0x05, 0x06, 0x07, 0x08}
	const tokenStr = "agpu_keyid_supersecretvalue"

	key := store.APIKey{
		ID:         "key-123",
		Name:       "ci-key",
		Prefix:     "agpu",
		SecretHash: secretHash,
		Salt:       salt,
		Roles:      []string{"admin"},
	}

	out := readLogFile(t, config.LogConfig{Level: "info", Format: "json"}, func(l *slog.Logger) {
		// 1) A whole APIKey (auto-redacted by LogValue — SecretHash/Salt never serialized).
		l.Info("auth ok", "key", key)
		// 2) A stringly-typed token attr (caught by the ReplaceAttr backstop).
		l.Info("inbound", "token", tokenStr, "authorization", "Bearer "+tokenStr)
		// 3) A raw hash/salt under sensitive key names (backstop).
		l.Info("dump", "secret_hash", secretHash, "salt", salt, "password", "hunter2")
	})

	// The literal secret material must not appear in any encoding.
	forbidden := []string{
		tokenStr,
		"hunter2",
		base64.StdEncoding.EncodeToString(secretHash),
		base64.StdEncoding.EncodeToString(salt),
		hex.EncodeToString(secretHash),
		hex.EncodeToString(salt),
		// slog's default []byte rendering is base64 via the StdEncoding; also guard
		// the raw bytes appearing verbatim.
		string(secretHash),
		string(salt),
	}
	for _, f := range forbidden {
		if f == "" {
			continue
		}
		if strings.Contains(out, f) {
			t.Errorf("log output leaked secret material %q\nfull output:\n%s", f, out)
		}
	}

	// The sensitive keys are present but redacted (proving the mask fired, not that
	// the field was merely absent).
	for _, key := range []string{"token", "authorization", "secret_hash", "salt", "password"} {
		if !strings.Contains(out, `"`+key+`":"`+redactedPlaceholder+`"`) {
			t.Errorf("expected %q to be redacted to %q in output:\n%s", key, redactedPlaceholder, out)
		}
	}

	// The APIKey's safe identifying fields survived (so auth logging is still useful).
	if !strings.Contains(out, `"id":"key-123"`) {
		t.Errorf("APIKey.LogValue dropped the safe id field:\n%s", out)
	}
	if !strings.Contains(out, `"name":"ci-key"`) {
		t.Errorf("APIKey.LogValue dropped the safe name field:\n%s", out)
	}
	// And it never serialized the secret-bearing field names at all.
	if strings.Contains(out, "SecretHash") || strings.Contains(out, "Salt") {
		t.Errorf("APIKey.LogValue serialized a secret field name:\n%s", out)
	}
}

// TestRedactAttrCaseInsensitive proves the key-name match is case-insensitive,
// so a leak under "Authorization" or "API_KEY" is masked just like the lowercase
// form.
func TestRedactAttrCaseInsensitive(t *testing.T) {
	for _, k := range []string{"Authorization", "API_KEY", "Secret", "TOKEN"} {
		got := redactAttr(nil, slog.String(k, "leak-me"))
		if got.Value.String() != redactedPlaceholder {
			t.Errorf("redactAttr(%q) = %q, want %q", k, got.Value.String(), redactedPlaceholder)
		}
	}
	// A non-sensitive key passes through untouched.
	if got := redactAttr(nil, slog.String("model", "llama3")); got.Value.String() != "llama3" {
		t.Errorf("redactAttr masked a non-sensitive key: %q", got.Value.String())
	}
}

// TestAPIKeyLogValue is a focused unit test of the type-level redaction: the
// group carries only safe fields and omits SecretHash/Salt entirely. Asserted
// independently of the handler so the guarantee holds for any slog handler.
func TestAPIKeyLogValue(t *testing.T) {
	revoked := store.APIKey{
		ID:         "k1",
		Name:       "n1",
		SecretHash: []byte("hash"),
		Salt:       []byte("salt"),
		Roles:      []string{"user"},
	}
	v := revoked.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want group", v.Kind())
	}
	seen := map[string]bool{}
	for _, a := range v.Group() {
		seen[a.Key] = true
	}
	for _, want := range []string{"id", "name", "roles", "revoked"} {
		if !seen[want] {
			t.Errorf("LogValue group missing safe field %q", want)
		}
	}
	for _, forbidden := range []string{"SecretHash", "Salt", "secret_hash", "salt"} {
		if seen[forbidden] {
			t.Errorf("LogValue group exposed sensitive field %q", forbidden)
		}
	}
}

// ensure the redaction is exercised through a context-bearing logger call too,
// so a `.With` chain (as the request-scoped logger uses) does not bypass it.
func TestRedactionThroughWith(t *testing.T) {
	out := readLogFile(t, config.LogConfig{Level: "info", Format: "json"}, func(l *slog.Logger) {
		l.With("request_id", "req-abc").Info("scoped", "token", "leak")
	})
	if strings.Contains(out, `"leak"`) || !strings.Contains(out, redactedPlaceholder) {
		t.Errorf("redaction bypassed through .With chain:\n%s", out)
	}
	if !strings.Contains(out, `"request_id":"req-abc"`) {
		t.Errorf("request_id not carried through .With chain:\n%s", out)
	}
	_ = context.Background()
}
