package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// recordingAppliers captures every applier call so a PUT test can assert the
// holder pushed the right value into the (fake) subsystem. Each field records the
// last value it was called with and whether it was called at all.
type recordingAppliers struct {
	mu sync.Mutex

	logLevel    string
	logLevelSet bool
	logLevelErr error // when non-nil, SetLogLevel returns it (forces an applier failure)

	qDefRPM, qDefTPM, qDefDaily, qDefMonthly uint64
	qDefSet                                  bool

	qGlobalRPM, qGlobalTPM uint64
	qGlobalSet             bool

	sessionTTL    time.Duration
	sessionTTLSet bool

	capTurns, capBytes, capCtx, capPerKey int
	capOverflow                           string
	capSet                                bool
	capErr                                error

	warm    time.Duration
	warmSet bool

	hb    time.Duration
	hbSet bool
}

// appliers builds the ConfigAppliers backed by this recorder.
func (r *recordingAppliers) appliers() ConfigAppliers {
	return ConfigAppliers{
		SetLogLevel: func(level string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.logLevelErr != nil {
				return r.logLevelErr
			}
			r.logLevel, r.logLevelSet = level, true
			return nil
		},
		SetQuotaDefaults: func(rpm, tpm, daily, monthly uint64) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.qDefRPM, r.qDefTPM, r.qDefDaily, r.qDefMonthly = rpm, tpm, daily, monthly
			r.qDefSet = true
		},
		SetQuotaGlobal: func(rpm, tpm uint64) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.qGlobalRPM, r.qGlobalTPM = rpm, tpm
			r.qGlobalSet = true
		},
		SetSessionTTL: func(d time.Duration) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.sessionTTL, r.sessionTTLSet = d, true
		},
		SetSessionCaps: func(maxTurns, maxBytes, maxCtxTokens, maxPerKey int, overflow string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.capErr != nil {
				return r.capErr
			}
			r.capTurns, r.capBytes, r.capCtx, r.capPerKey, r.capOverflow = maxTurns, maxBytes, maxCtxTokens, maxPerKey, overflow
			r.capSet = true
			return nil
		},
		SetModelWarmMax: func(d time.Duration) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.warm, r.warmSet = d, true
		},
		SetHeartbeatTimeout: func(d time.Duration) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.hb, r.hbSet = d, true
		},
	}
}

// defaultConfigSettings is a representative resolved tunable set the config tests
// seed the holder with (mirrors the package defaults).
func defaultConfigSettings() ConfigSettings {
	return ConfigSettings{
		LogLevel:                  "info",
		QuotaDefaultRPM:           0,
		QuotaDefaultTPM:           0,
		QuotaDefaultDailyTokens:   0,
		QuotaDefaultMonthlyTokens: 0,
		QuotaGlobalRPM:            0,
		QuotaGlobalTPM:            0,
		SessionTTL:                (30 * time.Minute).String(),
		SessionMaxTurns:           200,
		SessionMaxBytes:           1 << 20,
		SessionMaxContextTokens:   0,
		SessionMaxSessionsPerKey:  0,
		SessionOverflowPolicy:     "trim",
		ModelWarmMax:              time.Hour.String(),
		HeartbeatTimeout:          (45 * time.Second).String(),
	}
}

// defaultConfigBoot is the boot-only set the config tests surface read-only.
func defaultConfigBoot() ConfigReadOnly {
	return ConfigReadOnly{
		ServerListen:        "127.0.0.1:50051",
		ServerHTTPListen:    "127.0.0.1:8080",
		ServerMetricsListen: "127.0.0.1:9090",
		QuotaPath:           "/var/lib/agentgpu/quota.json",
		SessionPath:         "/var/lib/agentgpu/sessions.json",
		LogFormat:           "json",
		LogOutput:           "stderr",
	}
}

// configTestServer builds a routed Server with the runtime-config holder wired to
// the supplied recorder + checkpoint path, plus the auth/authz stack so the
// config:read / config:write gates are exercised through the real middleware.
func configTestServer(t *testing.T, rec *recordingAppliers, checkpointPath string) (*Server, *auth.Service) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	auditLog := audit.NewMemoryStore(0)
	s := &Server{
		fleet:    &fakeFleet{},
		auth:     authSvc,
		authz:    az,
		quota:    quota.NewEngine(quota.NewMemoryCounterStore()),
		log:      discard,
		auditLog: auditLog,
	}
	WithRuntimeConfig(defaultConfigSettings(), defaultConfigBoot(), rec.appliers(), checkpointPath)(s)
	return s, authSvc
}

// --- GET /v1/admin/config ---

// TestConfigGetShape proves GET returns the effective tunable settings, the
// boot-only values flagged read-only, and the explicit read-only field list.
func TestConfigGetShape(t *testing.T) {
	s, authSvc := configTestServer(t, &recordingAppliers{}, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/config", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var resp configResponse
	decode(t, rec, &resp)
	if resp.Settings.LogLevel != "info" {
		t.Errorf("settings.log_level = %q, want info", resp.Settings.LogLevel)
	}
	if resp.Settings.SessionTTL != (30 * time.Minute).String() {
		t.Errorf("settings.session_ttl = %q", resp.Settings.SessionTTL)
	}
	if resp.ReadOnly.ServerListen != "127.0.0.1:50051" {
		t.Errorf("read_only.server_listen = %q", resp.ReadOnly.ServerListen)
	}
	// The read-only field list must enumerate exactly the boot-only keys.
	if len(resp.ReadOnlyFields) != len(readOnlyFields) {
		t.Fatalf("read_only_fields = %v, want %v", resp.ReadOnlyFields, readOnlyFields)
	}
	for i, f := range readOnlyFields {
		if resp.ReadOnlyFields[i] != f {
			t.Errorf("read_only_fields[%d] = %q, want %q", i, resp.ReadOnlyFields[i], f)
		}
	}
}

// TestConfigGetRequiresReadScope proves GET is gated by config:read: an admin and
// a config:read holder pass, a config:write-only key and an unrelated-scope key
// are 403, and an unauthenticated request is 401.
func TestConfigGetRequiresReadScope(t *testing.T) {
	s, authSvc := configTestServer(t, &recordingAppliers{}, "")

	readToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigRead}})
	writeToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigWrite}})
	otherToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	adminToken := mustKey(t, authSvc, adminPerms())

	if rec := req(t, s, http.MethodGet, "/v1/admin/config", readToken, ""); rec.Code != http.StatusOK {
		t.Errorf("config:read GET status = %d, want 200", rec.Code)
	}
	if rec := req(t, s, http.MethodGet, "/v1/admin/config", adminToken, ""); rec.Code != http.StatusOK {
		t.Errorf("admin GET status = %d, want 200", rec.Code)
	}
	if rec := req(t, s, http.MethodGet, "/v1/admin/config", writeToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("config:write-only GET status = %d, want 403", rec.Code)
	}
	if rec := req(t, s, http.MethodGet, "/v1/admin/config", otherToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("unrelated-scope GET status = %d, want 403", rec.Code)
	}
	if rec := req(t, s, http.MethodGet, "/v1/admin/config", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET status = %d, want 401", rec.Code)
	}
}

// TestConfigEndpointsUnavailableWhenUnwired proves the routes are registered but
// return 503 when no runtime config is wired (mirroring the nil-subsystem
// convention), so the route count stays static.
func TestConfigEndpointsUnavailableWhenUnwired(t *testing.T) {
	// adminTestServer wires NO runtime config (s.config is nil).
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	if rec := req(t, s, http.MethodGet, "/v1/admin/config", adminToken, ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET with no runtime config = %d, want 503", rec.Code)
	}
	if rec := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `{"log_level":"debug"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT with no runtime config = %d, want 503", rec.Code)
	}
}

// --- PUT /v1/admin/config ---

// TestConfigPutAppliesEachTunable proves every runtime-tunable field, when PUT,
// is applied via its injected applier with the right value AND reflected back in
// the GET response.
func TestConfigPutAppliesEachTunable(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc := configTestServer(t, rec, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	body := `{
		"log_level":"debug",
		"quota_default_rpm":10,"quota_default_tpm":20,"quota_default_daily_tokens":30,"quota_default_monthly_tokens":40,
		"quota_global_rpm":600,"quota_global_tpm":7000,
		"session_ttl":"1h0m0s","session_max_turns":50,"session_max_bytes":2048,
		"session_max_context_tokens":99,"session_max_sessions_per_key":3,"session_overflow_policy":"reject",
		"model_warm_max":"2h0m0s","heartbeat_timeout":"90s"
	}`
	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logLevel != "debug" || !rec.logLevelSet {
		t.Errorf("log level applier: got %q set=%v", rec.logLevel, rec.logLevelSet)
	}
	if !rec.qDefSet || rec.qDefRPM != 10 || rec.qDefTPM != 20 || rec.qDefDaily != 30 || rec.qDefMonthly != 40 {
		t.Errorf("quota defaults applier: %+v", rec)
	}
	if !rec.qGlobalSet || rec.qGlobalRPM != 600 || rec.qGlobalTPM != 7000 {
		t.Errorf("quota global applier: rpm=%d tpm=%d", rec.qGlobalRPM, rec.qGlobalTPM)
	}
	if !rec.sessionTTLSet || rec.sessionTTL != time.Hour {
		t.Errorf("session ttl applier: %v set=%v", rec.sessionTTL, rec.sessionTTLSet)
	}
	if !rec.capSet || rec.capTurns != 50 || rec.capBytes != 2048 || rec.capCtx != 99 || rec.capPerKey != 3 || rec.capOverflow != "reject" {
		t.Errorf("session caps applier: turns=%d bytes=%d ctx=%d perKey=%d overflow=%q",
			rec.capTurns, rec.capBytes, rec.capCtx, rec.capPerKey, rec.capOverflow)
	}
	if !rec.warmSet || rec.warm != 2*time.Hour {
		t.Errorf("model warm max applier: %v", rec.warm)
	}
	if !rec.hbSet || rec.hb != 90*time.Second {
		t.Errorf("heartbeat timeout applier: %v", rec.hb)
	}

	// GET reflects the new values.
	getRec := req(t, s, http.MethodGet, "/v1/admin/config", adminToken, "")
	var got configResponse
	decode(t, getRec, &got)
	if got.Settings.LogLevel != "debug" || got.Settings.QuotaGlobalRPM != 600 ||
		got.Settings.SessionOverflowPolicy != "reject" || got.Settings.HeartbeatTimeout != (90*time.Second).String() {
		t.Errorf("GET after PUT did not reflect change: %+v", got.Settings)
	}
}

// TestConfigPutPartialSessionCapsPreservesUnchanged proves a PUT that changes only
// ONE session-cap field still calls the unit SetSessionCaps applier with the other
// caps' CURRENT values (so an un-named cap is preserved, not reset to zero). This
// guards the apply-as-a-unit wiring for the history caps.
func TestConfigPutPartialSessionCapsPreservesUnchanged(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc := configTestServer(t, rec, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	// Boot caps: turns=200, bytes=1<<20, ctx=0, perKey=0. Change ONLY the turn cap.
	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `{"session_max_turns":7}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.capSet {
		t.Fatal("session caps applier should have been called")
	}
	if rec.capTurns != 7 {
		t.Errorf("capTurns = %d, want 7", rec.capTurns)
	}
	// The OTHER caps carry their current (boot) values, not zero.
	if rec.capBytes != 1<<20 {
		t.Errorf("capBytes = %d, want preserved 1<<20", rec.capBytes)
	}
	if rec.capOverflow != "trim" {
		t.Errorf("capOverflow = %q, want preserved trim", rec.capOverflow)
	}
}

// TestConfigPutPartialLeavesOthers proves a partial PUT changes only the present
// fields and leaves the others — and their appliers — untouched.
func TestConfigPutPartialLeavesOthers(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc := configTestServer(t, rec, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `{"log_level":"warn"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.Code)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.logLevelSet || rec.logLevel != "warn" {
		t.Errorf("log level not applied: %q set=%v", rec.logLevel, rec.logLevelSet)
	}
	// No other applier should have fired.
	if rec.qDefSet || rec.qGlobalSet || rec.sessionTTLSet || rec.capSet || rec.warmSet || rec.hbSet {
		t.Errorf("a partial PUT touched an un-named applier: %+v", rec)
	}
}

// TestConfigPutInvalidValueAppliesNothing proves an out-of-range/invalid value is
// rejected with 400 and NOTHING is applied — the change is all-or-nothing, even
// when the invalid field is preceded (alphabetically) by valid ones.
func TestConfigPutInvalidValueAppliesNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad log level", `{"log_level":"loud"}`},
		{"zero session ttl", `{"session_ttl":"0s"}`},
		{"negative session ttl", `{"session_ttl":"-5m"}`},
		{"unparseable duration", `{"heartbeat_timeout":"soon"}`},
		{"zero model warm max", `{"model_warm_max":"0s"}`},
		{"bad overflow policy", `{"session_overflow_policy":"squish"}`},
		{"negative count", `{"session_max_turns":-1}`},
		{"wrong type", `{"quota_default_rpm":"lots"}`},
		// A valid field present alongside an invalid one must NOT be applied
		// (validation is total + up front; "log_level" sorts before "session_ttl").
		{"valid+invalid mix", `{"log_level":"debug","session_ttl":"0s"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingAppliers{}
			s, authSvc := configTestServer(t, rec, "")
			adminToken := mustKey(t, authSvc, adminPerms())

			resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, tc.body)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s status = %d, want 400; body=%s", tc.name, resp.Code, resp.Body.String())
			}
			if code := errorCode(t, resp); code != "invalid_request_error" {
				t.Errorf("error code = %q, want invalid_request_error", code)
			}
			rec.mu.Lock()
			applied := rec.logLevelSet || rec.qDefSet || rec.qGlobalSet || rec.sessionTTLSet || rec.capSet || rec.warmSet || rec.hbSet
			rec.mu.Unlock()
			if applied {
				t.Errorf("an invalid PUT applied something: %+v", rec)
			}
		})
	}
}

// TestConfigPutRejectsBootOnlyFields proves every boot-only field is rejected with
// 400 (read-only; restart to change) and nothing is applied.
func TestConfigPutRejectsBootOnlyFields(t *testing.T) {
	for _, field := range readOnlyFields {
		t.Run(field, func(t *testing.T) {
			rec := &recordingAppliers{}
			s, authSvc := configTestServer(t, rec, "")
			adminToken := mustKey(t, authSvc, adminPerms())

			body := `{"` + field + `":"127.0.0.1:1234"}`
			resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, body)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("PUT boot-only %s status = %d, want 400", field, resp.Code)
			}
			if !contains(resp.Body.String(), "read-only") {
				t.Errorf("error message should mention read-only: %s", resp.Body.String())
			}
			rec.mu.Lock()
			applied := rec.logLevelSet || rec.qDefSet || rec.qGlobalSet || rec.sessionTTLSet || rec.capSet || rec.warmSet || rec.hbSet
			rec.mu.Unlock()
			if applied {
				t.Errorf("a boot-only PUT applied something")
			}
		})
	}
}

// TestConfigPutRejectsUnknownField proves an unknown field key is rejected 400.
func TestConfigPutRejectsUnknownField(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc := configTestServer(t, rec, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `{"nonsense":1}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("PUT unknown field status = %d, want 400", resp.Code)
	}
	if !contains(resp.Body.String(), "unknown") {
		t.Errorf("error message should mention unknown field: %s", resp.Body.String())
	}
}

// TestConfigPutRequiresWriteScope proves PUT is gated by config:write: a
// config:read-only key is 403, a config:write key passes, and an unauthenticated
// request is 401.
func TestConfigPutRequiresWriteScope(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc := configTestServer(t, rec, "")

	readToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigRead}})
	writeToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeConfigWrite}})

	if resp := req(t, s, http.MethodPut, "/v1/admin/config", readToken, `{"log_level":"debug"}`); resp.Code != http.StatusForbidden {
		t.Errorf("config:read-only PUT status = %d, want 403", resp.Code)
	}
	if resp := req(t, s, http.MethodPut, "/v1/admin/config", writeToken, `{"log_level":"debug"}`); resp.Code != http.StatusOK {
		t.Errorf("config:write PUT status = %d, want 200", resp.Code)
	}
	if resp := req(t, s, http.MethodPut, "/v1/admin/config", "", `{"log_level":"debug"}`); resp.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated PUT status = %d, want 401", resp.Code)
	}
}

// TestConfigPutMalformedBody proves a non-JSON / non-object body is a 400.
func TestConfigPutMalformedBody(t *testing.T) {
	s, authSvc := configTestServer(t, &recordingAppliers{}, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `not json`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("malformed PUT status = %d, want 400", resp.Code)
	}
}

// TestConfigPutAudited proves a successful PUT records exactly one audit entry
// (op config.update) whose before/after carry ONLY the changed field.
func TestConfigPutAudited(t *testing.T) {
	rec := &recordingAppliers{}
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	auditLog := audit.NewMemoryStore(0)
	s := &Server{
		fleet:    &fakeFleet{},
		auth:     authSvc,
		authz:    az,
		quota:    quota.NewEngine(quota.NewMemoryCounterStore()),
		log:      discard,
		auditLog: auditLog,
	}
	WithRuntimeConfig(defaultConfigSettings(), defaultConfigBoot(), rec.appliers(), "")(s)
	adminToken := mustKey(t, authSvc, adminPerms())

	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `{"log_level":"debug"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.Code)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpConfigUpdate}, 0)
	if len(entries) != 1 {
		t.Fatalf("audit entries for config.update = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Target != "config" || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("audit entry target/outcome wrong: %+v", e)
	}
	// before/after carry only the changed field.
	if _, ok := e.After["log_level"]; !ok {
		t.Errorf("audit after missing changed field log_level: %+v", e.After)
	}
	if len(e.After) != 1 {
		t.Errorf("audit after should carry only the changed field, got %+v", e.After)
	}
	if e.Before["log_level"] != "info" {
		t.Errorf("audit before should capture the prior value (info), got %+v", e.Before)
	}
}

// TestConfigPutApplierErrorIs400 proves an applier that itself rejects the value
// (a defensive backstop) surfaces as a 400 and records a failure audit.
func TestConfigPutApplierErrorIs400(t *testing.T) {
	rec := &recordingAppliers{capErr: errFakeApplier}
	s, authSvc := configTestServer(t, rec, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	// A valid value that passes handler validation but the applier rejects.
	resp := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, `{"session_max_turns":5}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("applier-error PUT status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

// errFakeApplier is the sentinel a recordingAppliers returns to force an applier
// failure in TestConfigPutApplierErrorIs400.
var errFakeApplier = &applierError{}

type applierError struct{}

func (*applierError) Error() string { return "fake applier rejected the value" }

// TestConfigCheckpointRoundTripAndReapply proves a PUT is persisted to the
// checkpoint and re-applied to a fresh server on boot (so a prior change survives
// a restart and wins over the boot config).
func TestConfigCheckpointRoundTripAndReapply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// First server: PUT a couple of overrides; they are checkpointed.
	rec1 := &recordingAppliers{}
	s1, authSvc1 := configTestServer(t, rec1, path)
	adminToken1 := mustKey(t, authSvc1, adminPerms())
	resp := req(t, s1, http.MethodPut, "/v1/admin/config", adminToken1,
		`{"log_level":"debug","quota_global_rpm":600,"session_ttl":"1h0m0s"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}

	// Second server: fresh holder seeded with the BOOT defaults + the same checkpoint
	// path; loading the checkpoint must re-apply the persisted overrides on top.
	rec2 := &recordingAppliers{}
	s2, _ := configTestServer(t, rec2, path)
	if err := s2.LoadConfigCheckpoint(path); err != nil {
		t.Fatalf("LoadConfigCheckpoint: %v", err)
	}

	// The appliers on the second server were called with the persisted values.
	rec2.mu.Lock()
	defer rec2.mu.Unlock()
	if !rec2.logLevelSet || rec2.logLevel != "debug" {
		t.Errorf("reapply log level: %q set=%v", rec2.logLevel, rec2.logLevelSet)
	}
	if !rec2.qGlobalSet || rec2.qGlobalRPM != 600 {
		t.Errorf("reapply quota global rpm: %d set=%v", rec2.qGlobalRPM, rec2.qGlobalSet)
	}
	if !rec2.sessionTTLSet || rec2.sessionTTL != time.Hour {
		t.Errorf("reapply session ttl: %v set=%v", rec2.sessionTTL, rec2.sessionTTLSet)
	}

	// And the holder's current values reflect the re-applied overrides, while an
	// untouched field keeps its boot default.
	s2.config.mu.RLock()
	cur := s2.config.cur
	s2.config.mu.RUnlock()
	if cur.LogLevel != "debug" || cur.QuotaGlobalRPM != 600 || cur.SessionTTL != time.Hour.String() {
		t.Errorf("reapplied current config wrong: %+v", cur)
	}
	if cur.SessionMaxTurns != 200 {
		t.Errorf("untouched field should keep boot default 200, got %d", cur.SessionMaxTurns)
	}
}

// TestLoadConfigCheckpointMissingFileOK proves a missing checkpoint is not an
// error (a fresh boot with no overrides), and the holder keeps its boot values.
func TestLoadConfigCheckpointMissingFileOK(t *testing.T) {
	rec := &recordingAppliers{}
	s, _ := configTestServer(t, rec, filepath.Join(t.TempDir(), "absent.json"))
	if err := s.LoadConfigCheckpoint(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("missing checkpoint should be tolerated, got %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logLevelSet || rec.qGlobalSet {
		t.Errorf("no override should have been applied from a missing checkpoint")
	}
}

// TestLoadConfigCheckpointSkipsNonTunableKeys proves a hand-edited checkpoint
// carrying a boot-only or unknown key is tolerated: the bad keys are skipped and
// the genuine tunable override still applies (so a corrupted file cannot wedge boot).
func TestLoadConfigCheckpointSkipsNonTunableKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// log_level is genuine; server_listen is boot-only; bogus is unknown.
	writeFile(t, path, `{"log_level":"warn","server_listen":"0.0.0.0:1","bogus":1}`)

	rec := &recordingAppliers{}
	s, _ := configTestServer(t, rec, path)
	if err := s.LoadConfigCheckpoint(path); err != nil {
		t.Fatalf("LoadConfigCheckpoint with stray keys: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.logLevelSet || rec.logLevel != "warn" {
		t.Errorf("genuine tunable override not applied: %q set=%v", rec.logLevel, rec.logLevelSet)
	}
}

// TestLoadConfigCheckpointToleratesBadValue proves an invalid value in the
// persisted file is not fatal: the override is skipped and the boot value stands.
func TestLoadConfigCheckpointToleratesBadValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"session_ttl":"0s"}`) // 0s is invalid (must be positive)

	rec := &recordingAppliers{}
	s, _ := configTestServer(t, rec, path)
	if err := s.LoadConfigCheckpoint(path); err != nil {
		t.Fatalf("bad value should be tolerated, got %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.sessionTTLSet {
		t.Errorf("an invalid persisted value should not be applied")
	}
	// The holder keeps its boot value.
	s.config.mu.RLock()
	defer s.config.mu.RUnlock()
	if s.config.cur.SessionTTL != (30 * time.Minute).String() {
		t.Errorf("session TTL changed from boot default after a bad checkpoint: %q", s.config.cur.SessionTTL)
	}
}

// TestLoadConfigCheckpointMalformedJSONErrors proves a malformed (non-JSON)
// checkpoint file surfaces as an error (a hard, visible failure — distinct from a
// missing file), so a truncated write is noticed rather than silently ignored.
func TestLoadConfigCheckpointMalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{not json`)

	rec := &recordingAppliers{}
	s, _ := configTestServer(t, rec, path)
	if err := s.LoadConfigCheckpoint(path); err == nil {
		t.Fatal("malformed checkpoint JSON should return an error")
	}
}

// TestLoadConfigCheckpointNilHolderNoop proves loading is a no-op when no runtime
// config is wired (the server has no config holder).
func TestLoadConfigCheckpointNilHolderNoop(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{}) // no runtime config
	if err := s.LoadConfigCheckpoint(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("nil-holder load should be a no-op, got %v", err)
	}
}

// TestConfigConcurrentPutGet hammers the endpoint with concurrent PUTs and GETs so
// the race detector (CI runs go test -race) exercises the holder's locking: the
// read-modify-apply-commit under writeMu, the GET snapshot under mu, and the
// applier calls. It asserts only that every request returns a non-5xx status; the
// point is the race-clean concurrency, not a specific final value.
func TestConfigConcurrentPutGet(t *testing.T) {
	rec := &recordingAppliers{}
	s, authSvc := configTestServer(t, rec, "")
	adminToken := mustKey(t, authSvc, adminPerms())

	const workers = 8
	const iters = 25
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if (g+i)%2 == 0 {
					body := `{"quota_global_rpm":` + itoa(g*100+i) + `,"log_level":"debug"}`
					r := req(t, s, http.MethodPut, "/v1/admin/config", adminToken, body)
					if r.Code >= 500 {
						t.Errorf("concurrent PUT 5xx: %d", r.Code)
					}
				} else {
					r := req(t, s, http.MethodGet, "/v1/admin/config", adminToken, "")
					if r.Code >= 500 {
						t.Errorf("concurrent GET 5xx: %d", r.Code)
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

// itoa is a tiny base-10 int formatter for the concurrency test body (avoids a
// strconv import just for the test).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// writeFile writes content to path for the checkpoint-loading tests, failing the
// test on a write error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
