package main

import (
	"fmt"

	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
)

// buildConfigAppliers wires the admin settings/config endpoint's live-apply hooks
// (#92) to the real, thread-safe subsystem setters: the log handle's dynamic level,
// the quota engine's defaults/global limits, the session manager's TTL and per-key
// cap, the history store's caps + overflow policy, and the control-plane server's
// model-warm-max and heartbeat timeout. It is factored out of serveControlPlane so
// the same wiring an operator's PUT drives through is exercised end-to-end by a
// cmd-level integration test (a PUT changes real behavior), with no duplicated
// applier logic.
//
// The log-level and overflow-policy appliers reject an unrecognized value (a 400
// at the HTTP layer) rather than silently falling back, so a bad PUT is loud; the
// handler validates both up front, so these are defensive backstops. The quota
// defaults/global and the session history caps are each applied as a unit because
// the underlying setter replaces the whole limit/cap set.
func buildConfigAppliers(lh *logHandle, eng *quota.Engine, mgr *session.Manager, histStore *session.MemoryHistoryStore, srv *server.Server) httpapi.ConfigAppliers {
	return httpapi.ConfigAppliers{
		SetLogLevel: func(level string) error {
			lvl, ok := parseLevel(level)
			if !ok {
				return fmt.Errorf("unrecognized log level %q", level)
			}
			lh.SetLevel(lvl)
			return nil
		},
		SetQuotaDefaults: func(rpm, tpm, daily, monthly uint64) {
			eng.SetDefaults(quota.Limits{RPM: rpm, TPM: tpm, DailyTokens: daily, MonthlyTokens: monthly})
		},
		SetQuotaGlobal: eng.SetGlobalLimits,
		SetSessionTTL:  mgr.SetTTL,
		SetSessionCaps: func(maxTurns, maxBytes, maxCtxTokens, maxPerKey int, overflowPolicy string) error {
			pol, ok := session.ParseOverflowPolicy(overflowPolicy)
			if !ok {
				return fmt.Errorf("unrecognized session overflow policy %q", overflowPolicy)
			}
			histStore.SetCaps(session.HistoryCaps{
				MaxTurns:  maxTurns,
				MaxBytes:  maxBytes,
				MaxTokens: maxCtxTokens,
				Policy:    pol,
			})
			mgr.SetMaxSessionsPerKey(maxPerKey)
			return nil
		},
		SetModelWarmMax:     srv.SetModelWarmMax,
		SetHeartbeatTimeout: srv.SetHeartbeatTimeout,
	}
}
