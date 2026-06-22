package httpapi

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// webui_observability_test.go holds the shared test rig for the #103 console screens
// (Usage / Telemetry / Logs / Settings) that need a subsystem the base
// adminTestServer does not wire: a server with NO quota engine (to exercise the
// usage disabled-notice), and a server with a log source + runtime-config holder +
// audit store (for the logs and settings screens). They mirror the existing per-
// subsystem test servers (usageTestServer / logTestServer / configTestServer) but
// also stand up the full UI routing via s.Handler() and the auth/authz stack so the
// scope gates are exercised through the real middleware.

// adminTestServerNoQuota builds a routed Server with the auth/authz stack but NO
// quota engine, so the Usage screen's disabled-notice path is exercised.
func adminTestServerNoQuota(t *testing.T) (*Server, *auth.Service) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	s := &Server{
		fleet: &fakeFleet{},
		auth:  authSvc,
		authz: az,
		log:   discard,
	}
	return s, authSvc
}

// settingsTestServer builds a routed Server with the runtime-config holder wired (so
// the Settings read page + the live-apply PUT are exercised through the real
// config:read / config:write gates and CSRF), plus an audit store so a test can
// assert the config.update entry. It reuses the config test's recorder/appliers and
// representative defaults.
func settingsTestServer(t *testing.T, rec *recordingAppliers) (*Server, *auth.Service, *audit.MemoryStore) {
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
		log:      discard,
		auditLog: auditLog,
	}
	WithRuntimeConfig(defaultConfigSettings(), defaultConfigBoot(), rec.appliers(), "")(s)
	return s, authSvc, auditLog
}

// logUITestServer builds a routed Server with a fakeLogSource wired (so the Logs
// page, the filtered partial, and the SSE proxy are exercised), the auth/authz
// stack, and a fast stream poll so a live-tail test does not sleep on the real
// interval. The source is returned so a test can seed records.
func logUITestServer(t *testing.T) (*Server, *auth.Service, *fakeLogSource) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	src := &fakeLogSource{}
	s := &Server{
		auth:          authSvc,
		authz:         az,
		log:           discard,
		logs:          src,
		fleet:         &fakeFleet{},
	}
	return s, authSvc, src
}
