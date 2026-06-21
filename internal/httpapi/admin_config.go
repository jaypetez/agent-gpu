package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
)

// Settings/config management API (#92): GET/PUT /v1/admin/config. GET returns the
// effective resolved settings — the runtime-tunable values plus the boot-only
// values flagged read-only — gated by config:read. PUT validates a partial update
// and applies it LIVE (no restart), rejecting invalid values BEFORE applying
// anything, gated by config:write.
//
// The handlers operate over a runtimeConfig holder injected via WithRuntimeConfig.
// The holder keeps the current tunable values behind an RWMutex and a set of
// applier functions that push a change into the live subsystems (the quota engine,
// session manager + history store, the control-plane server, and the logger). The
// appliers are injected (rather than the subsystems themselves) so this package
// stays unit-testable without standing up a live control plane: a test wires fake
// appliers and asserts they were called with the right values. cmd wires the real
// setters (quota.SetDefaults/SetGlobalLimits, session SetTTL/SetMaxSessionsPerKey,
// MemoryHistoryStore.SetCaps, server SetModelWarmMax/SetHeartbeatTimeout, and
// logHandle.SetLevel).
//
// Precedence/persistence: only the fields changed via PUT are checkpointed (an
// override map), not the whole config. At boot cmd resolves config normally
// (flag > env > default) and constructs the subsystems, THEN loads the config
// checkpoint and re-applies each persisted override via the same appliers — so a
// prior PUT survives a restart, and a persisted PUT override wins over the boot
// flags for that tunable field.

// ConfigAppliers are the live-apply hooks the runtime-config holder calls when a
// PUT changes a tunable field (#92). Each is injected by cmd and wired to the
// matching thread-safe subsystem setter; nil is tolerated (the corresponding
// field becomes a no-op apply, which is what unit tests that only exercise some
// fields rely on). The SetLogLevel and SetSessionCaps appliers return an error so
// a value the applier itself rejects (an unknown log level; an unparseable
// overflow policy) surfaces as a 400 rather than a silent no-op — though the
// handler validates those up front, so the applier error is a defensive backstop.
type ConfigAppliers struct {
	SetLogLevel         func(level string) error
	SetQuotaDefaults    func(rpm, tpm, daily, monthly uint64)
	SetQuotaGlobal      func(rpm, tpm uint64)
	SetSessionTTL       func(d time.Duration)
	SetSessionCaps      func(maxTurns, maxBytes, maxCtxTokens, maxPerKey int, overflow string) error
	SetModelWarmMax     func(d time.Duration)
	SetHeartbeatTimeout func(d time.Duration)
}

// ConfigSettings is the full set of runtime-tunable settings (#92). It is the GET
// "settings" projection, the holder's current-state snapshot, and the exported
// shape cmd seeds the holder with via WithRuntimeConfig. All duration fields are
// serialized as Go duration strings (e.g. "30m0s") so a human-readable value
// round-trips through PUT; counts/limits are plain integers.
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

// ConfigReadOnly is the set of boot-only settings surfaced read-only in GET (#92).
// PUT rejects any attempt to change them with 400. Durations/addresses are
// strings; they are informational so an operator can see the effective values
// without consulting flags/env.
type ConfigReadOnly struct {
	ServerListen        string `json:"server_listen"`
	ServerHTTPListen    string `json:"server_http_listen"`
	ServerMetricsListen string `json:"server_metrics_listen"`
	QuotaPath           string `json:"quota_path"`
	SessionPath         string `json:"session_path"`
	LogFormat           string `json:"log_format"`
	LogOutput           string `json:"log_output"`
}

// configResponse is the GET /v1/admin/config body: the effective tunable settings
// plus the boot-only values flagged read-only. read_only is also surfaced as an
// explicit list of the read-only field keys so a client can render them disabled
// without hard-coding the classification.
type configResponse struct {
	Settings       ConfigSettings `json:"settings"`
	ReadOnly       ConfigReadOnly `json:"read_only"`
	ReadOnlyFields []string       `json:"read_only_fields"`
}

// readOnlyFields is the explicit list of boot-only field keys, returned in the GET
// response so a client knows exactly which keys PUT will reject. It is also the
// set the PUT validator rejects (a boot-only key present in the request body → a
// 400 before anything is applied). Kept sorted for a stable response/rejection.
var readOnlyFields = []string{
	"log_format",
	"log_output",
	"quota_path",
	"server_http_listen",
	"server_listen",
	"server_metrics_listen",
	"session_path",
}

// runtimeConfig is the live holder behind the admin config endpoints (#92). It
// keeps the current tunable values and the boot-only values, the injected
// appliers, and the checkpoint path. cur is guarded by mu so a GET sees a
// consistent snapshot and a PUT updates it atomically after applying the change to
// the subsystems. It is nil when no runtime config is wired (the handlers then
// return 503), mirroring how the session endpoints gate on a nil manager.
type runtimeConfig struct {
	// writeMu serializes the whole PUT read-modify-apply-commit so two concurrent
	// updates cannot interleave (the snapshot a PUT validates against, the apply, and
	// the commit are one critical section). It is the OUTERMOST lock: an applier it
	// calls acquires the relevant subsystem lock, and no subsystem ever acquires
	// writeMu, so there is no lock cycle. The GET read path does not take it (it uses
	// the mu RLock below), so reads never block behind a write.
	writeMu sync.Mutex

	mu       sync.RWMutex
	cur      ConfigSettings
	boot     ConfigReadOnly
	appliers ConfigAppliers
	// checkpointPath is the config-override checkpoint file (config.json alongside
	// the quota/session checkpoints). Empty disables persistence (the change still
	// applies live; it just does not survive a restart) so unit tests need not touch
	// the filesystem.
	checkpointPath string
	// persisted is the set of override keys changed via PUT, persisted to the
	// checkpoint so a prior PUT survives restart. Only changed fields are stored
	// (not the whole config). It is updated under mu alongside cur.
	persisted map[string]struct{}
}

// WithRuntimeConfig wires the runtime settings/config holder backing GET/PUT
// /v1/admin/config (#92). initial is the effective resolved tunable values at
// boot, boot is the read-only values, appliers push a PUT change into the live
// subsystems, and checkpointPath is where changed-field overrides are persisted
// (empty disables persistence). When this option is not supplied the holder is
// nil and the config endpoints return 503 (the routes are always registered and
// documented; the static route count is unaffected).
func WithRuntimeConfig(initial ConfigSettings, boot ConfigReadOnly, appliers ConfigAppliers, checkpointPath string) Option {
	return func(s *Server) {
		s.config = &runtimeConfig{
			cur:            initial,
			boot:           boot,
			appliers:       appliers,
			checkpointPath: checkpointPath,
			persisted:      make(map[string]struct{}),
		}
	}
}

// handleAdminGetConfig serves GET /v1/admin/config (#92): the effective resolved
// settings — the runtime-tunable values plus the boot-only values flagged
// read-only. Gated to config:read (s.requireScope), so a key lacking it gets 403
// and an unauthenticated request 401 before this runs. Returns 503 when no runtime
// config is wired. These settings carry no secrets; the response still goes
// through an explicit projection (no struct is serialized directly) for hygiene
// parity with the rest of the admin API.
func (s *Server) handleAdminGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime config is not enabled")
		return
	}
	s.config.mu.RLock()
	resp := configResponse{
		Settings:       s.config.cur,
		ReadOnly:       s.config.boot,
		ReadOnlyFields: readOnlyFields,
	}
	s.config.mu.RUnlock()
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminPutConfig serves PUT /v1/admin/config (#92): a partial update of the
// runtime-tunable settings, applied LIVE with no restart. Only fields present in
// the request body change. The handler validates EVERY present field first
// (enum/range, and rejects any boot-only or unknown key) and returns 400 with a
// clear message on the FIRST problem, applying NOTHING; only once the whole update
// is valid does it apply each present field via its applier, update the in-memory
// holder, checkpoint the changed-field overrides, and record one audit entry. It
// is gated to config:write (registered via requireScopeWrite, so it also layers
// idempotency). Returns 503 when no runtime config is wired.
func (s *Server) handleAdminPutConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime config is not enabled")
		return
	}

	// Decode into a generic map first so we can (a) reject boot-only/unknown keys by
	// name and (b) tell "field present" from "field omitted" without a pointer for
	// every field. A malformed body is a 400.
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
		return
	}

	// Reject boot-only and unknown keys BEFORE applying anything, so a request that
	// tries to change a read-only field (or fat-fingers a key) fails loudly rather
	// than silently ignoring it.
	for _, key := range sortedKeys(raw) {
		if isReadOnlyField(key) {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("field %q is read-only; restart to change it", key))
			return
		}
		if !isTunableField(key) {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("unknown config field %q", key))
			return
		}
	}

	// Serialize the whole read-modify-apply-commit so two concurrent PUTs cannot
	// interleave: the snapshot we validate against, the apply, and the commit are one
	// critical section. writeMu is the outermost lock (no subsystem ever takes it), so
	// holding it across the appliers introduces no lock cycle; the GET read path does
	// not take it, so reads never block behind a write.
	s.config.writeMu.Lock()
	defer s.config.writeMu.Unlock()

	// Parse + validate every present field into a pending change set. Validation is
	// total (all present fields) and happens BEFORE any apply, so the FIRST invalid
	// value aborts the whole update with a clear message and nothing is changed.
	s.config.mu.RLock()
	pending := s.config.cur
	s.config.mu.RUnlock()

	changed, err := applyConfigPatch(&pending, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(changed) == 0 {
		// A well-formed but empty (or no-tunable-field) body is a no-op success: the
		// effective config is unchanged. Return the current config inline (mirrors a
		// GET) — we cannot delegate to handleAdminGetConfig here because it would be a
		// nested handler write; build the response directly under the held writeMu.
		s.config.mu.RLock()
		resp := configResponse{Settings: s.config.cur, ReadOnly: s.config.boot, ReadOnlyFields: readOnlyFields}
		s.config.mu.RUnlock()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Capture the before/after projection of ONLY the changed fields for the audit
	// entry. These settings carry no secrets, but the projection style matches the
	// rest of the admin audit surface.
	before := s.config.snapshotFields(changed)

	// Apply each changed field to the live subsystems via the injected appliers, then
	// commit the new values to the holder and persist the changed-field overrides.
	// Applier errors here are a defensive backstop (the values were validated above);
	// on one, abort with a 400 and record a failure — nothing partially committed,
	// because we apply BEFORE committing cur and the appliers themselves are atomic.
	if err := s.config.apply(pending, changed); err != nil {
		s.recordAudit(r, auditOpConfigUpdate, "config", audit.OutcomeFailure, before, nil)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	after := s.config.snapshotFields(changed)
	s.recordAudit(r, auditOpConfigUpdate, "config", audit.OutcomeSuccess, before, after)

	if err := s.config.checkpoint(); err != nil {
		// A checkpoint-write failure must not fail the request — the change already
		// took effect live. Log it; the change simply may not survive a restart.
		s.reqLog(r.Context()).Warn("config checkpoint failed", "err", err)
	}

	s.config.mu.RLock()
	resp := configResponse{
		Settings:       s.config.cur,
		ReadOnly:       s.config.boot,
		ReadOnlyFields: readOnlyFields,
	}
	s.config.mu.RUnlock()
	writeJSON(w, http.StatusOK, resp)
}

// apply pushes each changed field's new value (from next) into the live
// subsystems via the appliers, then — only if every apply succeeded — commits
// next as the holder's current values and marks the changed keys persisted. The
// quota defaults/global are applied as a unit (a single applier call) because the
// underlying setter replaces the whole limit set; likewise the session history
// caps. An applier error leaves cur unchanged and is returned so the handler maps
// it to a 400. Callers must not hold mu (apply takes it to commit).
func (rc *runtimeConfig) apply(next ConfigSettings, changed map[string]struct{}) error {
	a := rc.appliers

	if _, ok := changed["log_level"]; ok && a.SetLogLevel != nil {
		if err := a.SetLogLevel(next.LogLevel); err != nil {
			return err
		}
	}
	if quotaDefaultsChanged(changed) && a.SetQuotaDefaults != nil {
		a.SetQuotaDefaults(next.QuotaDefaultRPM, next.QuotaDefaultTPM, next.QuotaDefaultDailyTokens, next.QuotaDefaultMonthlyTokens)
	}
	if quotaGlobalChanged(changed) && a.SetQuotaGlobal != nil {
		a.SetQuotaGlobal(next.QuotaGlobalRPM, next.QuotaGlobalTPM)
	}
	if _, ok := changed["session_ttl"]; ok && a.SetSessionTTL != nil {
		d, _ := time.ParseDuration(next.SessionTTL) // validated already
		a.SetSessionTTL(d)
	}
	if sessionCapsChanged(changed) && a.SetSessionCaps != nil {
		if err := a.SetSessionCaps(next.SessionMaxTurns, next.SessionMaxBytes, next.SessionMaxContextTokens, next.SessionMaxSessionsPerKey, next.SessionOverflowPolicy); err != nil {
			return err
		}
	}
	if _, ok := changed["model_warm_max"]; ok && a.SetModelWarmMax != nil {
		d, _ := time.ParseDuration(next.ModelWarmMax) // validated already
		a.SetModelWarmMax(d)
	}
	if _, ok := changed["heartbeat_timeout"]; ok && a.SetHeartbeatTimeout != nil {
		d, _ := time.ParseDuration(next.HeartbeatTimeout) // validated already
		a.SetHeartbeatTimeout(d)
	}

	rc.mu.Lock()
	rc.cur = next
	for k := range changed {
		rc.persisted[k] = struct{}{}
	}
	rc.mu.Unlock()
	return nil
}

// snapshotFields projects the named fields of the current config into a redacted
// audit value map (#92), so the audit before/after carries only the fields the
// PUT touched. Callers pass the changed-field set; the projection reads cur under
// mu. There are no secret config fields, but the projection style mirrors the rest
// of the admin audit surface (no struct serialized directly).
func (rc *runtimeConfig) snapshotFields(fields map[string]struct{}) audit.RedactedValues {
	if len(fields) == 0 {
		return nil
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	v := audit.RedactedValues{}
	for f := range fields {
		v[f] = fieldValue(rc.cur, f)
	}
	return v
}

// fieldValue returns the current value of the named tunable field for the audit
// projection. An unknown field (not possible from the validated change set) yields
// nil.
func fieldValue(c ConfigSettings, field string) any {
	switch field {
	case "log_level":
		return c.LogLevel
	case "quota_default_rpm":
		return c.QuotaDefaultRPM
	case "quota_default_tpm":
		return c.QuotaDefaultTPM
	case "quota_default_daily_tokens":
		return c.QuotaDefaultDailyTokens
	case "quota_default_monthly_tokens":
		return c.QuotaDefaultMonthlyTokens
	case "quota_global_rpm":
		return c.QuotaGlobalRPM
	case "quota_global_tpm":
		return c.QuotaGlobalTPM
	case "session_ttl":
		return c.SessionTTL
	case "session_max_turns":
		return c.SessionMaxTurns
	case "session_max_bytes":
		return c.SessionMaxBytes
	case "session_max_context_tokens":
		return c.SessionMaxContextTokens
	case "session_max_sessions_per_key":
		return c.SessionMaxSessionsPerKey
	case "session_overflow_policy":
		return c.SessionOverflowPolicy
	case "model_warm_max":
		return c.ModelWarmMax
	case "heartbeat_timeout":
		return c.HeartbeatTimeout
	default:
		return nil
	}
}

// quotaDefaultsChanged / quotaGlobalChanged / sessionCapsChanged report whether
// any field in the corresponding apply-as-a-unit group changed, so the unit
// applier (which replaces the whole limit/cap set) runs exactly when at least one
// of its fields was touched.
func quotaDefaultsChanged(changed map[string]struct{}) bool {
	for _, k := range []string{"quota_default_rpm", "quota_default_tpm", "quota_default_daily_tokens", "quota_default_monthly_tokens"} {
		if _, ok := changed[k]; ok {
			return true
		}
	}
	return false
}

func quotaGlobalChanged(changed map[string]struct{}) bool {
	_, rpm := changed["quota_global_rpm"]
	_, tpm := changed["quota_global_tpm"]
	return rpm || tpm
}

func sessionCapsChanged(changed map[string]struct{}) bool {
	for _, k := range []string{"session_max_turns", "session_max_bytes", "session_max_context_tokens", "session_max_sessions_per_key", "session_overflow_policy"} {
		if _, ok := changed[k]; ok {
			return true
		}
	}
	return false
}

// sortedKeys returns the keys of m sorted, so boot-only/unknown-key rejection and
// the apply order are deterministic (a stable first-error message under tests).
func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isReadOnlyField reports whether key names a boot-only field (rejected by PUT).
func isReadOnlyField(key string) bool {
	for _, f := range readOnlyFields {
		if f == key {
			return true
		}
	}
	return false
}

// isTunableField reports whether key names a known runtime-tunable field.
func isTunableField(key string) bool {
	switch key {
	case "log_level",
		"quota_default_rpm", "quota_default_tpm", "quota_default_daily_tokens", "quota_default_monthly_tokens",
		"quota_global_rpm", "quota_global_tpm",
		"session_ttl", "session_max_turns", "session_max_bytes", "session_max_context_tokens",
		"session_max_sessions_per_key", "session_overflow_policy", "model_warm_max",
		"heartbeat_timeout":
		return true
	default:
		return false
	}
}

// applyConfigPatch parses every present field in raw into dst, validating
// enum/range as it goes, and returns the set of changed field keys. It returns the
// FIRST validation error (with a clear, field-named message) without mutating dst
// further, so the caller applies NOTHING on any invalid field. dst starts as a
// copy of the current config; only present fields are overwritten.
func applyConfigPatch(dst *ConfigSettings, raw map[string]json.RawMessage) (map[string]struct{}, error) {
	changed := make(map[string]struct{}, len(raw))
	// Iterate in sorted key order so the first-error message is deterministic.
	for _, key := range sortedKeys(raw) {
		msg := raw[key]
		switch key {
		case "log_level":
			v, err := decodeString(key, msg)
			if err != nil {
				return nil, err
			}
			if !validLogLevel(v) {
				return nil, fmt.Errorf("invalid %s %q: must be one of debug, info, warn, error", key, v)
			}
			dst.LogLevel = strings.ToLower(strings.TrimSpace(v))
		case "quota_default_rpm":
			v, err := decodeUint(key, msg)
			if err != nil {
				return nil, err
			}
			dst.QuotaDefaultRPM = v
		case "quota_default_tpm":
			v, err := decodeUint(key, msg)
			if err != nil {
				return nil, err
			}
			dst.QuotaDefaultTPM = v
		case "quota_default_daily_tokens":
			v, err := decodeUint(key, msg)
			if err != nil {
				return nil, err
			}
			dst.QuotaDefaultDailyTokens = v
		case "quota_default_monthly_tokens":
			v, err := decodeUint(key, msg)
			if err != nil {
				return nil, err
			}
			dst.QuotaDefaultMonthlyTokens = v
		case "quota_global_rpm":
			v, err := decodeUint(key, msg)
			if err != nil {
				return nil, err
			}
			dst.QuotaGlobalRPM = v
		case "quota_global_tpm":
			v, err := decodeUint(key, msg)
			if err != nil {
				return nil, err
			}
			dst.QuotaGlobalTPM = v
		case "session_ttl":
			d, err := decodeDuration(key, msg)
			if err != nil {
				return nil, err
			}
			if d <= 0 {
				return nil, fmt.Errorf("invalid %s %q: must be a positive duration", key, d)
			}
			dst.SessionTTL = d.String()
		case "session_max_turns":
			v, err := decodeNonNegInt(key, msg)
			if err != nil {
				return nil, err
			}
			dst.SessionMaxTurns = v
		case "session_max_bytes":
			v, err := decodeNonNegInt(key, msg)
			if err != nil {
				return nil, err
			}
			dst.SessionMaxBytes = v
		case "session_max_context_tokens":
			v, err := decodeNonNegInt(key, msg)
			if err != nil {
				return nil, err
			}
			dst.SessionMaxContextTokens = v
		case "session_max_sessions_per_key":
			v, err := decodeNonNegInt(key, msg)
			if err != nil {
				return nil, err
			}
			dst.SessionMaxSessionsPerKey = v
		case "session_overflow_policy":
			v, err := decodeString(key, msg)
			if err != nil {
				return nil, err
			}
			norm := strings.ToLower(strings.TrimSpace(v))
			if norm != "trim" && norm != "reject" {
				return nil, fmt.Errorf("invalid %s %q: must be trim or reject", key, v)
			}
			dst.SessionOverflowPolicy = norm
		case "model_warm_max":
			d, err := decodeDuration(key, msg)
			if err != nil {
				return nil, err
			}
			if d <= 0 {
				return nil, fmt.Errorf("invalid %s %q: must be a positive duration", key, d)
			}
			dst.ModelWarmMax = d.String()
		case "heartbeat_timeout":
			d, err := decodeDuration(key, msg)
			if err != nil {
				return nil, err
			}
			if d <= 0 {
				return nil, fmt.Errorf("invalid %s %q: must be a positive duration", key, d)
			}
			dst.HeartbeatTimeout = d.String()
		default:
			// Unreachable: unknown keys are rejected before applyConfigPatch runs.
			return nil, fmt.Errorf("unknown config field %q", key)
		}
		changed[key] = struct{}{}
	}
	return changed, nil
}

// decodeString decodes a JSON string field, returning a field-named 400 message
// on a type mismatch.
func decodeString(field string, msg json.RawMessage) (string, error) {
	var v string
	if err := json.Unmarshal(msg, &v); err != nil {
		return "", fmt.Errorf("invalid %s: must be a string", field)
	}
	return v, nil
}

// decodeUint decodes a JSON non-negative integer field into a uint64, rejecting a
// negative or non-integer value with a field-named 400 message.
func decodeUint(field string, msg json.RawMessage) (uint64, error) {
	var v uint64
	if err := json.Unmarshal(msg, &v); err != nil {
		return 0, fmt.Errorf("invalid %s: must be a non-negative integer", field)
	}
	return v, nil
}

// decodeNonNegInt decodes a JSON non-negative integer field into an int (the
// count-style session caps), rejecting a negative or non-integer value with a
// field-named 400 message.
func decodeNonNegInt(field string, msg json.RawMessage) (int, error) {
	var v int
	if err := json.Unmarshal(msg, &v); err != nil {
		return 0, fmt.Errorf("invalid %s: must be a non-negative integer", field)
	}
	if v < 0 {
		return 0, fmt.Errorf("invalid %s %d: must be non-negative", field, v)
	}
	return v, nil
}

// decodeDuration decodes a JSON duration field. It accepts a Go duration STRING
// (e.g. "30m", "45s") — the human-readable form GET emits — and rejects anything
// else with a field-named 400 message. The range (positive) is checked by the
// caller so the message can be field-specific.
func decodeDuration(field string, msg json.RawMessage) (time.Duration, error) {
	var s string
	if err := json.Unmarshal(msg, &s); err != nil {
		return 0, fmt.Errorf("invalid %s: must be a duration string (e.g. \"30m\")", field)
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be a duration string (e.g. \"30m\")", field, s)
	}
	return d, nil
}

// validLogLevel reports whether name is one of the recognized slog level names
// (debug|info|warn|error, case-insensitive). It mirrors cmd's parseLevel set but
// is duplicated here (a tiny, stable enum) so the httpapi package validates the
// PUT without importing cmd. "warning" is accepted as an alias for warn, matching
// parseLevel.
func validLogLevel(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug", "info", "warn", "warning", "error":
		return true
	default:
		return false
	}
}
