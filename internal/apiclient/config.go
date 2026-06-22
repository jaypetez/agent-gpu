package apiclient

import (
	"context"
	"net/http"
)

// Settings/config admin API client (#92, surfaced to the CLI in #104): GET/PUT
// /v1/admin/config. These are the only admin endpoints the typed client did not
// already wrap. The wire shapes mirror internal/httpapi/admin_config.go
// field-for-field (see configResponse/ConfigSettings/ConfigReadOnly there);
// duration fields are Go duration STRINGS (e.g. "30m0s") so a human-readable value
// round-trips, and counts/limits are plain integers.

// ConfigSettings is the runtime-tunable settings projection (the GET response's
// "settings" object and the round-trip shape). It mirrors httpapi.ConfigSettings.
// PUT does not take this struct — it takes a partial field patch — because the
// server distinguishes "field present" from "field omitted" by the keys in the
// request body; see PutConfig.
type ConfigSettings struct {
	LogLevel string `json:"log_level"`

	QuotaDefaultRPM           uint64 `json:"quota_default_rpm"`
	QuotaDefaultTPM           uint64 `json:"quota_default_tpm"`
	QuotaDefaultDailyTokens   uint64 `json:"quota_default_daily_tokens"`
	QuotaDefaultMonthlyTokens uint64 `json:"quota_default_monthly_tokens"`
	QuotaGlobalRPM            uint64 `json:"quota_global_rpm"`
	QuotaGlobalTPM            uint64 `json:"quota_global_tpm"`

	SessionTTL               string `json:"session_ttl"`
	SessionMaxTurns          int    `json:"session_max_turns"`
	SessionMaxBytes          int    `json:"session_max_bytes"`
	SessionMaxContextTokens  int    `json:"session_max_context_tokens"`
	SessionMaxSessionsPerKey int    `json:"session_max_sessions_per_key"`
	SessionOverflowPolicy    string `json:"session_overflow_policy"`
	ModelWarmMax             string `json:"model_warm_max"`

	HeartbeatTimeout string `json:"heartbeat_timeout"`
}

// ConfigReadOnly is the set of boot-only settings surfaced read-only in GET. PUT
// rejects any attempt to change them with a 400 (an *APIError, not a sentinel
// class). It mirrors httpapi.ConfigReadOnly.
type ConfigReadOnly struct {
	ServerListen        string `json:"server_listen"`
	ServerHTTPListen    string `json:"server_http_listen"`
	ServerMetricsListen string `json:"server_metrics_listen"`
	QuotaPath           string `json:"quota_path"`
	SessionPath         string `json:"session_path"`
	LogFormat           string `json:"log_format"`
	LogOutput           string `json:"log_output"`
}

// ConfigResponse is the GET/PUT /v1/admin/config body: the effective tunable
// settings, the boot-only values flagged read-only, and the explicit list of
// read-only field keys (so a client can render them disabled without hard-coding
// the classification). It mirrors httpapi.configResponse.
type ConfigResponse struct {
	Settings       ConfigSettings `json:"settings"`
	ReadOnly       ConfigReadOnly `json:"read_only"`
	ReadOnlyFields []string       `json:"read_only_fields"`
}

// GetConfig returns the effective runtime configuration (GET /v1/admin/config):
// the tunable settings plus the boot-only values flagged read-only. Requires the
// config:read scope (the admin role grants it); a token lacking it gets the typed
// ErrForbidden, and a server without runtime config wired returns a 503 (a plain
// *APIError).
func (c *Client) GetConfig(ctx context.Context) (ConfigResponse, error) {
	var out ConfigResponse
	err := c.do(ctx, http.MethodGet, "/v1/admin/config", nil, &out)
	return out, err
}

// PutConfig applies a partial update of the runtime-tunable settings (PUT
// /v1/admin/config) and returns the resulting effective configuration. patch is a
// map of config field key to value — only the keys present are changed; an empty
// patch is a no-op success that echoes the current config. The server validates
// every present field BEFORE applying anything and rejects an invalid value, an
// unknown key, or a boot-only (read-only) key with a 400 — surfaced here as an
// *APIError carrying the server's message, so the CLI can print the validation
// detail. Requires the config:write scope (the admin role grants it).
//
// The patch is sent as a raw field map (not a typed struct) so the server can tell
// "field present" from "field omitted" by the keys in the body, matching its
// generic-map decode; values use the same wire encoding as ConfigSettings (counts
// as integers, durations as strings like "30m").
func (c *Client) PutConfig(ctx context.Context, patch map[string]any) (ConfigResponse, error) {
	var out ConfigResponse
	err := c.do(ctx, http.MethodPut, "/v1/admin/config", patch, &out)
	return out, err
}
