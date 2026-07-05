package httpapi

import (
	"strconv"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_settings_data.go maps the runtime-config holder onto the console's Settings
// view-models (#103). It reads the SAME holder GET/PUT /v1/admin/config operate over
// (s.config) under its read lock, so the effective values the editor shows are
// exactly what the API reports. The tunable fields are grouped into tabs (General /
// Quotas / Sessions / Advanced) for legibility; the boot-only fields are surfaced
// read-only with a "restart to change" note (the PUT rejects them, mirroring the
// JSON handler). Behavior-changing fields carry a consequence warning. No secret is
// involved — config carries none — but the projection style matches the rest of the
// console.

// collectSettings builds the Settings model from the live config holder. Enabled is
// false when no runtime config is wired (the screen renders the unavailable notice).
// canWrite gates whether the editor is interactive (the viewer holds config:write)
// or read-only with a note.
func (s *Server) collectSettings(canWrite bool) webui.SettingsData {
	if s.config == nil {
		return webui.SettingsData{Enabled: false, CanWrite: canWrite}
	}
	s.config.mu.RLock()
	cur := s.config.cur
	boot := s.config.boot
	s.config.mu.RUnlock()

	tabs := []webui.SettingsTab{
		{
			ID:    "general",
			Title: "General",
			Fields: []webui.SettingsField{
				{
					Name: "log_level", Label: "Log level", Kind: webui.FieldSelect,
					Options: []string{"debug", "info", "warn", "error"}, Value: cur.LogLevel,
					Help:    "Minimum severity written to the server log.",
					Warning: "Setting debug greatly increases log volume.",
				},
			},
		},
		{
			ID:    "quotas",
			Title: "Quotas",
			Fields: []webui.SettingsField{
				numField("quota_default_rpm", "Default RPM", "Per-key requests/minute for keys without an override (0 = unlimited).", cur.QuotaDefaultRPM),
				numField("quota_default_tpm", "Default TPM", "Per-key tokens/minute for keys without an override (0 = unlimited).", cur.QuotaDefaultTPM),
				numField("quota_default_daily_tokens", "Default daily tokens", "Per-key daily token budget default (0 = unlimited).", cur.QuotaDefaultDailyTokens),
				numField("quota_default_monthly_tokens", "Default monthly tokens", "Per-key monthly token budget default (0 = unlimited).", cur.QuotaDefaultMonthlyTokens),
				warnField(numField("quota_global_rpm", "Global RPM", "Fleet-wide requests/minute ceiling across all keys (0 = unlimited).", cur.QuotaGlobalRPM),
					"Lowering this throttles the whole fleet immediately."),
				warnField(numField("quota_global_tpm", "Global TPM", "Fleet-wide tokens/minute ceiling across all keys (0 = unlimited).", cur.QuotaGlobalTPM),
					"Lowering this throttles the whole fleet immediately."),
			},
		},
		{
			ID:    "sessions",
			Title: "Sessions",
			Fields: []webui.SettingsField{
				durField("session_ttl", "Session TTL", "How long an idle conversation session is retained before it expires.", cur.SessionTTL),
				numField("session_max_turns", "Max turns", "Maximum turns kept per session (0 = unlimited).", uint64(cur.SessionMaxTurns)),
				numField("session_max_bytes", "Max bytes", "Maximum stored bytes per session (0 = unlimited).", uint64(cur.SessionMaxBytes)),
				numField("session_max_context_tokens", "Max context tokens", "Maximum context tokens retained per session (0 = unlimited).", uint64(cur.SessionMaxContextTokens)),
				warnField(numField("session_max_sessions_per_key", "Max sessions / key", "Maximum live sessions a single key may hold (0 = unlimited).", uint64(cur.SessionMaxSessionsPerKey)),
					"Lowering this may evict a key's oldest live sessions."),
				{
					Name: "session_overflow_policy", Label: "Overflow policy", Kind: webui.FieldSelect,
					Options: []string{"trim", "reject"}, Value: cur.SessionOverflowPolicy,
					Help:    "What happens when a session exceeds its caps: trim oldest turns, or reject the request.",
				},
			},
		},
		{
			ID:    "advanced",
			Title: "Advanced",
			Fields: []webui.SettingsField{
				durField("model_warm_max", "Model warm window", "How long a model stays warm on a worker after its last use.", cur.ModelWarmMax),
				warnField(durField("heartbeat_timeout", "Heartbeat timeout", "How long a worker may miss heartbeats before it is marked stale.", cur.HeartbeatTimeout),
					"Too short a timeout can drop healthy-but-slow workers from the fleet."),
			},
		},
	}

	return webui.SettingsData{
		Enabled:  true,
		CanWrite: canWrite,
		Tabs:     tabs,
		ReadOnly: bootFields(boot),
	}
}

// numField builds a non-negative integer field with a 0 lower bound.
func numField(name, label, help string, value uint64) webui.SettingsField {
	return webui.SettingsField{
		Name: name, Label: label, Help: help, Kind: webui.FieldNumber,
		Value: strconv.FormatUint(value, 10), Min: "0",
	}
}

// durField builds a Go-duration text field (the value is the holder's duration
// string, e.g. "30m0s").
func durField(name, label, help, value string) webui.SettingsField {
	return webui.SettingsField{
		Name: name, Label: label, Help: help, Kind: webui.FieldDuration, Value: value,
	}
}

// warnField attaches a consequence warning to a field (a behavior-changing tunable),
// returning the field so it composes inline in the tab definitions.
func warnField(f webui.SettingsField, warning string) webui.SettingsField {
	f.Warning = warning
	return f
}

// bootFields projects the boot-only read-only settings into display fields, marked
// ReadOnly so the editor renders them disabled with a "restart to change" note. The
// order matches readOnlyFields for stability.
func bootFields(boot ConfigReadOnly) []webui.SettingsField {
	return []webui.SettingsField{
		roField("server_listen", "gRPC listen", boot.ServerListen),
		roField("server_http_listen", "HTTP listen", boot.ServerHTTPListen),
		roField("server_metrics_listen", "Metrics listen", boot.ServerMetricsListen),
		roField("quota_path", "Quota checkpoint", boot.QuotaPath),
		roField("session_path", "Session checkpoint", boot.SessionPath),
		roField("log_format", "Log format", boot.LogFormat),
		roField("log_output", "Log output", boot.LogOutput),
	}
}

// roField builds a read-only boot field.
func roField(name, label, value string) webui.SettingsField {
	return webui.SettingsField{
		Name: name, Label: label, Value: value, Kind: webui.FieldText, ReadOnly: true,
	}
}
