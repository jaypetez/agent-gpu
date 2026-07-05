package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/metrics"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// configIntegrationFixture wires the REAL subsystems and the REAL config appliers
// (via buildConfigAppliers) behind a routed httpapi server, exactly as
// serveControlPlane does — so a PUT /v1/admin/config drives the same live-apply
// path an operator would. It returns the routed server, an admin bearer token, and
// the live subsystems so the test can observe that a PUT actually took effect.
type configIntegrationFixture struct {
	httpSrv   *httpapi.Server
	token     string
	lh        *logHandle
	eng       *quota.Engine
	mgr       *session.Manager
	histStore *session.MemoryHistoryStore
	srv       *server.Server
}

func newConfigIntegrationFixture(t *testing.T) *configIntegrationFixture {
	t.Helper()

	// Real log handle (the dynamic-level seam #92 flips).
	lh, err := newLoggerHandle(config.LogConfig{Level: "info", Format: "json", Output: "stderr"})
	if err != nil {
		t.Fatalf("newLoggerHandle: %v", err)
	}
	t.Cleanup(func() { _ = lh.Close() })
	logger := lh.Logger

	// Real key store + auth + authz.
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(logger))

	// Real quota engine (defaults/global both off at boot).
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithLogger(logger))

	// Real session manager + history store, with a 30m boot TTL.
	histStore := session.NewMemoryHistoryStoreWithPolicy(200, 1<<20, 0, session.OverflowTrim)
	mgr := session.NewManager(session.NewMemorySessionStore(), histStore,
		session.WithLogger(logger), session.WithTTL(30*time.Minute))

	// Real control-plane server with a 45s boot heartbeat timeout and 1h warm-max.
	srv := server.New(
		server.WithLogger(logger),
		server.WithStore(st),
		server.WithQuota(eng),
		server.WithAuthorizer(az),
		server.WithSessionManager(mgr),
		server.WithHeartbeatTimeout(45*time.Second),
		server.WithModelWarmMax(time.Hour),
	)

	// The SAME applier wiring serveControlPlane uses.
	appliers := buildConfigAppliers(lh, eng, mgr, histStore, srv)
	initial := httpapi.ConfigSettings{
		LogLevel:                 "info",
		SessionTTL:               (30 * time.Minute).String(),
		SessionMaxTurns:          200,
		SessionMaxBytes:          1 << 20,
		SessionMaxContextTokens:  0,
		SessionMaxSessionsPerKey: 0,
		SessionOverflowPolicy:    "trim",
		ModelWarmMax:             time.Hour.String(),
		HeartbeatTimeout:         (45 * time.Second).String(),
	}
	boot := httpapi.ConfigReadOnly{
		ServerListen:     "127.0.0.1:50051",
		ServerHTTPListen: "127.0.0.1:8080",
		QuotaPath:        "/var/lib/agentgpu/quota.json",
		SessionPath:      "/var/lib/agentgpu/sessions.json",
		LogFormat:        "json",
		LogOutput:        "stderr",
	}

	m := metrics.New()
	httpSrv := httpapi.NewServer(srv, authSvc, az, mgr, m, logger, "127.0.0.1:0",
		httpapi.WithRuntimeConfig(initial, boot, appliers, ""))

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin", auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}

	return &configIntegrationFixture{
		httpSrv: httpSrv, token: token, lh: lh, eng: eng, mgr: mgr, histStore: histStore, srv: srv,
	}
}

// put drives a PUT /v1/admin/config through the routed handler and returns the
// response recorder.
func (f *configIntegrationFixture) put(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/v1/admin/config", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.httpSrv.Handler().ServeHTTP(rec, r)
	return rec
}

// TestConfigHotReloadChangesRealBehavior is the cmd-level integration proof that a
// PUT /v1/admin/config takes effect LIVE on the real subsystems with no restart
// (#92, AC2/AC3): it asserts the dynamic log level, the session TTL stamped onto a
// new session, the quota global limits, and the server heartbeat timeout / warm-max
// all change as a direct result of one PUT through the real applier wiring.
func TestConfigHotReloadChangesRealBehavior(t *testing.T) {
	f := newConfigIntegrationFixture(t)

	// Baselines BEFORE the PUT.
	if got := f.lh.Level.Level(); got != slog.LevelInfo {
		t.Fatalf("boot log level = %v, want info", got)
	}
	if got := f.srv.HeartbeatTimeout(); got != 45*time.Second {
		t.Fatalf("boot heartbeat timeout = %v, want 45s", got)
	}
	if got := f.srv.ModelWarmMax(); got != time.Hour {
		t.Fatalf("boot model warm max = %v, want 1h", got)
	}
	if got := f.eng.GlobalLimits(); got.RPM != 0 || got.TPM != 0 {
		t.Fatalf("boot global limits = %+v, want 0/0", got)
	}
	if got := f.mgr.TTL(); got != 30*time.Minute {
		t.Fatalf("boot session TTL = %v, want 30m", got)
	}

	// One PUT flips every observable subsystem live.
	body := `{
		"log_level":"debug",
		"quota_global_rpm":600,"quota_global_tpm":7000,
		"session_ttl":"15m0s",
		"model_warm_max":"2h0m0s",
		"heartbeat_timeout":"90s"
	}`
	if rec := f.put(t, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Log level: the dynamic level var was flipped (debug now emits).
	if got := f.lh.Level.Level(); got != slog.LevelDebug {
		t.Errorf("after PUT log level = %v, want debug", got)
	}
	// Quota global limits: observed live on the engine.
	if got := f.eng.GlobalLimits(); got.RPM != 600 || got.TPM != 7000 {
		t.Errorf("after PUT global limits = %+v, want 600/7000", got)
	}
	// Server heartbeat timeout + warm-max: observed live on the server.
	if got := f.srv.HeartbeatTimeout(); got != 90*time.Second {
		t.Errorf("after PUT heartbeat timeout = %v, want 90s", got)
	}
	if got := f.srv.ModelWarmMax(); got != 2*time.Hour {
		t.Errorf("after PUT model warm max = %v, want 2h", got)
	}
	// Session TTL: the manager's TTL changed AND a NEW session is stamped with it.
	if got := f.mgr.TTL(); got != 15*time.Minute {
		t.Errorf("after PUT session TTL = %v, want 15m", got)
	}
	sess, err := f.mgr.Create(context.Background(), "owner-key", "llama3")
	if err != nil {
		t.Fatalf("create session after PUT: %v", err)
	}
	if sess.TTL != 15*time.Minute {
		t.Errorf("new session stamped TTL = %v, want 15m (live hot-reload)", sess.TTL)
	}
}

// TestConfigHotReloadSessionCapsRealBehavior proves a PUT changing the session
// history caps + overflow policy takes effect on the real history store: after
// switching to a turn cap of 1 under reject, the manager refuses a second turn
// append (#92).
func TestConfigHotReloadSessionCapsRealBehavior(t *testing.T) {
	f := newConfigIntegrationFixture(t)

	sess, err := f.mgr.Create(context.Background(), "owner-key", "llama3")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ctx := context.Background()

	// Under the boot caps (200 turns, trim) the first append succeeds.
	if err := f.mgr.AppendTurn(ctx, sess.ID, "owner-key", types.Message{Role: "user", Content: "a"}); err != nil {
		t.Fatalf("append 1 under boot caps: %v", err)
	}

	// PUT a turn cap of 1 under reject.
	body := `{"session_max_turns":1,"session_overflow_policy":"reject"}`
	if rec := f.put(t, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT caps status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The store now holds 1 turn and the cap is 1 under reject, so a second turn is
	// refused — proving the new caps are in force live.
	if err := f.mgr.AppendTurn(ctx, sess.ID, "owner-key", types.Message{Role: "user", Content: "b"}); err == nil {
		t.Fatalf("append over the live turn cap should be refused")
	}
}

// TestConfigPersistedOverrideReappliedOnBoot proves a PUT persisted to the config
// checkpoint is re-applied to a freshly-built server on boot, winning over the boot
// config for that tunable field (#92, AC5).
func TestConfigPersistedOverrideReappliedOnBoot(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	// First server: PUT an override that is checkpointed to path.
	lh1, err := newLoggerHandle(config.LogConfig{Level: "info", Format: "json", Output: "stderr"})
	if err != nil {
		t.Fatalf("newLoggerHandle: %v", err)
	}
	t.Cleanup(func() { _ = lh1.Close() })
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(lh1.Logger))
	eng1 := quota.NewEngine(quota.NewMemoryCounterStore())
	hist1 := session.NewMemoryHistoryStore(200, 1<<20)
	mgr1 := session.NewManager(session.NewMemorySessionStore(), hist1, session.WithTTL(30*time.Minute))
	srv1 := server.New(server.WithStore(st), server.WithAuthorizer(az), server.WithSessionManager(mgr1),
		server.WithHeartbeatTimeout(45*time.Second), server.WithModelWarmMax(time.Hour))
	httpSrv1 := httpapi.NewServer(srv1, authSvc, az, mgr1, metrics.New(), lh1.Logger, "127.0.0.1:0",
		httpapi.WithRuntimeConfig(httpapi.ConfigSettings{
			LogLevel: "info", SessionTTL: (30 * time.Minute).String(), SessionMaxTurns: 200,
			SessionMaxBytes: 1 << 20, SessionOverflowPolicy: "trim", ModelWarmMax: time.Hour.String(),
			HeartbeatTimeout: (45 * time.Second).String(),
		}, httpapi.ConfigReadOnly{}, buildConfigAppliers(lh1, eng1, mgr1, hist1, srv1), path))
	token, _, _ := authSvc.CreateWithPermissions(context.Background(), "admin", auth.Permissions{Roles: []string{authz.RoleAdmin}})

	r := httptest.NewRequest(http.MethodPut, "/v1/admin/config", strings.NewReader(`{"heartbeat_timeout":"90s","session_ttl":"15m0s"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	httpSrv1.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Second server: fresh subsystems at the BOOT defaults; loading the checkpoint
	// must re-apply the persisted overrides on top.
	lh2, err := newLoggerHandle(config.LogConfig{Level: "info", Format: "json", Output: "stderr"})
	if err != nil {
		t.Fatalf("newLoggerHandle 2: %v", err)
	}
	t.Cleanup(func() { _ = lh2.Close() })
	eng2 := quota.NewEngine(quota.NewMemoryCounterStore())
	hist2 := session.NewMemoryHistoryStore(200, 1<<20)
	mgr2 := session.NewManager(session.NewMemorySessionStore(), hist2, session.WithTTL(30*time.Minute))
	srv2 := server.New(server.WithStore(store.NewMemory()), server.WithSessionManager(mgr2),
		server.WithHeartbeatTimeout(45*time.Second), server.WithModelWarmMax(time.Hour))
	authSvc2 := auth.NewService(store.NewMemory())
	az2 := authz.NewAuthorizer()
	httpSrv2 := httpapi.NewServer(srv2, authSvc2, az2, mgr2, metrics.New(), lh2.Logger, "127.0.0.1:0",
		httpapi.WithRuntimeConfig(httpapi.ConfigSettings{
			LogLevel: "info", SessionTTL: (30 * time.Minute).String(), SessionMaxTurns: 200,
			SessionMaxBytes: 1 << 20, SessionOverflowPolicy: "trim", ModelWarmMax: time.Hour.String(),
			HeartbeatTimeout: (45 * time.Second).String(),
		}, httpapi.ConfigReadOnly{}, buildConfigAppliers(lh2, eng2, mgr2, hist2, srv2), path))
	if err := httpSrv2.LoadConfigCheckpoint(path); err != nil {
		t.Fatalf("LoadConfigCheckpoint: %v", err)
	}

	// The persisted overrides won over the boot config for those fields.
	if got := srv2.HeartbeatTimeout(); got != 90*time.Second {
		t.Errorf("reboot heartbeat timeout = %v, want 90s (persisted PUT)", got)
	}
	if got := mgr2.TTL(); got != 15*time.Minute {
		t.Errorf("reboot session TTL = %v, want 15m (persisted PUT)", got)
	}
	// An untouched field keeps its boot default.
	if got := srv2.ModelWarmMax(); got != time.Hour {
		t.Errorf("reboot model warm max = %v, want 1h (untouched boot default)", got)
	}
}
