package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/metrics"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
)

// quotaCheckpointInterval is how often the server flushes the quota counters to
// the checkpoint file while running, bounding how much usage a crash can lose.
const quotaCheckpointInterval = 30 * time.Second

// sessionCheckpointInterval is how often the server flushes sessions and their
// history to disk while running, bounding how much conversation state a crash
// can lose (mirrors the quota checkpoint cadence).
const sessionCheckpointInterval = 30 * time.Second

// httpShutdownTimeout bounds how long the HTTP server is given to drain
// in-flight requests on graceful shutdown before the process proceeds to stop
// the gRPC server.
const httpShutdownTimeout = 10 * time.Second

func runServerCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return usagef("usage: agentgpu server start [--listen host:port]")
	}

	fs := flag.NewFlagSet("server start", flag.ContinueOnError)
	listen := fs.String("listen", "", "gRPC listen address (host:port)")
	httpListen := fs.String("http-listen", "", "public HTTP API listen address (host:port) (default 127.0.0.1:8080 or $AGENTGPU_HTTP_LISTEN)")
	metricsListen := fs.String("metrics-listen", "", "Prometheus /metrics listen address (host:port); empty disables it (default 127.0.0.1:9090 or $AGENTGPU_METRICS_LISTEN)")
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	quotaPath := fs.String("quota-path", "", "path to the quota counter checkpoint (default $AGENTGPU_QUOTA_PATH or ~/.agentgpu/quota.json)")
	rpm := fs.Uint64("default-rpm", 0, "global default requests per minute (0 = unlimited)")
	tpm := fs.Uint64("default-tpm", 0, "global default tokens per minute (0 = unlimited)")
	daily := fs.Uint64("default-daily-tokens", 0, "global default daily token budget (0 = unlimited)")
	monthly := fs.Uint64("default-monthly-tokens", 0, "global default monthly token budget (0 = unlimited)")
	globalRPM := fs.Uint64("global-rpm", 0, "server-wide requests-per-minute cap across the whole fleet (0 = unlimited or $AGENTGPU_GLOBAL_RPM)")
	globalTPM := fs.Uint64("global-tpm", 0, "server-wide tokens-per-minute cap across the whole fleet (0 = unlimited or $AGENTGPU_GLOBAL_TPM)")
	hbTimeout := fs.Duration("heartbeat-timeout", 0, "evict a worker after this long without a heartbeat (default 45s or $AGENTGPU_HEARTBEAT_TIMEOUT)")
	sessionPath := fs.String("session-path", "", "path to the session+history checkpoint (default $AGENTGPU_SESSION_PATH or ~/.agentgpu/sessions.json)")
	sessionTTL := fs.Duration("session-ttl", 0, "per-session idle timeout (default 30m or $AGENTGPU_SESSION_TTL)")
	modelWarmMax := fs.Duration("model-warm-max", 0, "max model-warmth keep_alive window for session-bound jobs; keep_alive = min(session TTL, this) (default 1h or $AGENTGPU_MODEL_WARM_MAX)")
	setUsage(fs, "Usage: agentgpu server start [--listen host:port] [--http-listen host:port] [flags]")
	// The server/worker commands have no caller-injected writer; their help goes to
	// stdout (a success), matching the informational top-level help.
	if err := parseFlags(fs, os.Stdout, args[1:]); err != nil {
		return err
	}

	// Translate an explicit "off" for the metrics listener (an empty
	// --metrics-listen passed on the command line, or AGENTGPU_METRICS_LISTEN set
	// to empty) into the disable sentinel so the decision survives env/default
	// resolution; an unset flag/env defaults the listener on. flagPassed reports
	// whether --metrics-listen appeared on the command line so an explicit empty
	// flag is distinguishable from the (empty) zero value of an unset flag.
	metricsFlag := *metricsListen
	if metricsListenOff(fs, metricsFlag) {
		metricsFlag = config.MetricsListenDisabled
	}
	cfg := config.ResolveServer(config.ServerConfig{Listen: *listen, HTTPListen: *httpListen, MetricsListen: metricsFlag}, nil)
	heartbeatTimeout := config.ResolveHeartbeatTimeout(*hbTimeout, nil)
	qcfg := config.ResolveQuota(config.QuotaConfig{
		Path:                 *quotaPath,
		DefaultRPM:           *rpm,
		DefaultTPM:           *tpm,
		DefaultDailyTokens:   *daily,
		DefaultMonthlyTokens: *monthly,
		GlobalRPM:            *globalRPM,
		GlobalTPM:            *globalTPM,
	}, nil, nil)
	scfg := config.ResolveSession(config.SessionConfig{
		Path:         *sessionPath,
		TTL:          *sessionTTL,
		ModelWarmMax: *modelWarmMax,
	}, nil, nil)
	return serveControlPlane(ctx, logger, cfg, *storeFlag, qcfg, scfg, heartbeatTimeout)
}

// metricsListenOff reports whether the operator explicitly turned the metrics
// listener off: either --metrics-listen was passed with an empty value, or
// AGENTGPU_METRICS_LISTEN is set to empty. An unset flag (empty zero value, not
// visited) and an unset env both return false so the listener defaults on. It is
// the only place that needs to distinguish "set to empty" from "unset", which a
// plain string flag cannot, so it consults fs.Visit and os.LookupEnv directly.
func metricsListenOff(fs *flag.FlagSet, flagValue string) bool {
	passed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "metrics-listen" {
			passed = true
		}
	})
	if passed && flagValue == "" {
		return true
	}
	if !passed {
		if v, ok := os.LookupEnv(config.EnvMetricsListen); ok && v == "" {
			return true
		}
	}
	return false
}

// serveControlPlane starts the gRPC control-plane server and blocks until ctx
// is cancelled (SIGINT/SIGTERM), then shuts down gracefully, checkpointing the
// quota counters on the way out.
func serveControlPlane(ctx context.Context, logger *slog.Logger, cfg config.ServerConfig, storeFlag string, qcfg config.QuotaConfig, scfg config.SessionConfig, heartbeatTimeout time.Duration) error {
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}

	// Shared, persistent key store so per-key Limits are visible to dispatch.
	st, err := openStore(storeFlag)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Quota engine: load any existing counter checkpoint, then enforce with the
	// configured global defaults. Counters are checkpointed periodically and on
	// graceful shutdown.
	cs := quota.NewMemoryCounterStore()
	if err := cs.LoadCheckpoint(qcfg.Path); err != nil {
		return err
	}
	eng := quota.NewEngine(cs,
		quota.WithLogger(logger),
		quota.WithDefaults(quota.Limits{
			RPM:           qcfg.DefaultRPM,
			TPM:           qcfg.DefaultTPM,
			DailyTokens:   qcfg.DefaultDailyTokens,
			MonthlyTokens: qcfg.DefaultMonthlyTokens,
		}),
		// Server-wide (global) rate limits enforced at the HTTP boundary (#6),
		// independent of per-key quota. Load-time only — there is no hot-reload.
		quota.WithGlobalLimits(qcfg.GlobalRPM, qcfg.GlobalTPM),
	)

	// Session subsystem (#36): in-memory session + history stores, restored from
	// their checkpoint at boot. Sessions already idled out while the process was
	// down are dropped on load (roll-expire), and any history orphaned by a
	// dropped/expired session is purged so it is not resurrected. Mirrors the
	// quota wiring: periodic + shutdown checkpoints bound how much a crash loses.
	sessPath := scfg.Path
	histPath := sessionHistoryPath(sessPath)
	sessStore := session.NewMemorySessionStore()
	histStore := session.NewMemoryHistoryStore(scfg.MaxTurns, scfg.MaxBytes)
	now := time.Now()
	dropped, err := sessStore.LoadCheckpoint(sessPath, now)
	if err != nil {
		return fmt.Errorf("load session checkpoint: %w", err)
	}
	keep := sessionKeepSet(sessStore)
	_ = dropped // the dropped ids are excluded from keep below, so they are purged on history load.
	if err := histStore.LoadCheckpoint(histPath, keep); err != nil {
		return fmt.Errorf("load history checkpoint: %w", err)
	}
	mgr := session.NewManager(sessStore, histStore,
		session.WithLogger(logger),
		session.WithTTL(scfg.TTL),
	)
	mgr.Start()
	defer func() { _ = mgr.Close() }()

	gs := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    server.DefaultKeepaliveTime,
			Timeout: server.DefaultKeepaliveTimeout,
		}),
	)
	// Shared authorizer: the same instance gates job dispatch in the gRPC server
	// AND permission-filters the HTTP model catalog, so a model a key sees in the
	// catalog is exactly a model it may invoke (no drift between the two paths).
	az := authz.NewAuthorizer(authz.WithLogger(logger))
	srv := server.New(
		server.WithLogger(logger),
		server.WithStore(st),
		server.WithQuota(eng),
		server.WithHeartbeatTimeout(heartbeatTimeout),
		server.WithAuthorizer(az),
		// The same manager backs affinity routing in the dispatcher and the session
		// API + stateful history in the HTTP layer — one source of truth.
		server.WithSessionManager(mgr),
		// Model warmth (#35): cap the keep_alive window the dispatcher sends for
		// session-bound jobs at min(session TTL, this), so a conversation's model
		// stays resident across active turns and unloads within a bounded window
		// once the session goes idle.
		server.WithModelWarmMax(scfg.ModelWarmMax),
	)
	srv.Register(gs)
	// Start the stale-worker eviction loop; Close stops it on shutdown.
	srv.Start()
	defer func() { _ = srv.Close() }()

	// Prometheus instrument (#24): one registry shared by the request-path
	// collectors (updated inline by the HTTP layer) and the live server collector
	// (read at scrape time). Building it here, before the HTTP server, lets the
	// request path record into it; the server collector is registered just below
	// once srv exists. metricsAddr is the resolved metrics listener address; the
	// MetricsListenDisabled sentinel maps to "" which disables the listener.
	metricsAddr := config.MetricsListenAddr(cfg.MetricsListen)
	m := metrics.New()
	// The live collector reads queue depth, the fleet, the time-in-queue
	// distribution, and affinity counters from the control-plane server at every
	// scrape. A registration failure (a descriptor clash) is a programming error,
	// so surface it as a startup error rather than silently dropping the metrics.
	if err := m.RegisterServerCollector(srv); err != nil {
		return fmt.Errorf("register metrics collector: %w", err)
	}

	// Public HTTP API: authenticates Bearer tokens via the same key store and
	// permission-filters the catalog with the shared authorizer above. The same
	// Prometheus instrument is threaded in so the request path is metered.
	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(srv, authSvc, az, mgr, m, logger, cfg.HTTPListen)

	// Metrics listener (#24): a second HTTP server serving only /metrics on a
	// dedicated port, unauthenticated (it is an operational port, not the public
	// API), kept off the API mux so scraping needs no API auth and the OpenAPI
	// route set is unaffected. An empty address disables it.
	var metricsSrv *http.Server
	if metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", m.Handler())
		metricsSrv = &http.Server{
			Addr:              metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	logger.Info("control-plane server listening",
		"addr", lis.Addr().String(), "http_addr", cfg.HTTPListen, "quota_path", qcfg.Path,
		"global_rpm", qcfg.GlobalRPM, "global_tpm", qcfg.GlobalTPM,
		"session_path", sessPath, "session_ttl", scfg.TTL.String(),
		"metrics_addr", metricsAddr, "heartbeat_timeout", heartbeatTimeout.String())

	// Periodic checkpoint so a crash loses at most quotaCheckpointInterval of usage.
	ticker := time.NewTicker(quotaCheckpointInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := cs.Checkpoint(qcfg.Path, time.Now()); err != nil {
					logger.Warn("quota checkpoint failed", "err", err)
				}
			}
		}
	}()

	// Periodic session+history checkpoint, bounding how much conversation state a
	// crash can lose to one interval (mirrors the quota checkpoint above).
	sessTicker := time.NewTicker(sessionCheckpointInterval)
	defer sessTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sessTicker.C:
				checkpointSessions(logger, sessStore, histStore, sessPath, histPath)
			}
		}
	}()

	errCh := make(chan error, 3)
	go func() { errCh <- gs.Serve(lis) }()
	go func() {
		// ListenAndServe returns http.ErrServerClosed on graceful Shutdown; treat
		// that as a clean stop so it does not race the ctx.Done() path to errCh.
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http api: %w", err)
		}
	}()
	if metricsSrv != nil {
		go func() {
			// Same clean-stop contract as the API server: ErrServerClosed on a
			// graceful Shutdown is not an error.
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics listener: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received; stopping gracefully")
		// Drain HTTP first so no in-flight request races the control plane down.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http api shutdown failed", "err", err)
		}
		// Stop the metrics listener too (best-effort; a scrape in flight is
		// short-lived and losing it on shutdown is harmless).
		if metricsSrv != nil {
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
				logger.Warn("metrics listener shutdown failed", "err", err)
			}
		}
		gs.GracefulStop()
		if err := cs.Checkpoint(qcfg.Path, time.Now()); err != nil {
			logger.Warn("quota checkpoint on shutdown failed", "err", err)
		}
		checkpointSessions(logger, sessStore, histStore, sessPath, histPath)
		return nil
	case err := <-errCh:
		return err
	}
}

// sessionHistoryPath derives the history checkpoint path from the session
// checkpoint path: history.json alongside the session file. The two stores
// checkpoint to separate files (matching the session package's split between a
// SessionStore and a HistoryStore) but live in the same directory and share a
// lifecycle, so a single --session-path flag configures both.
func sessionHistoryPath(sessionPath string) string {
	return filepath.Join(filepath.Dir(sessionPath), "history.json")
}

// sessionKeepSet returns the ids of the sessions currently in the store, used as
// the keep-set when loading history so history orphaned by a session that was
// dropped on load (idle-expired while the process was down) is not resurrected.
func sessionKeepSet(store *session.MemorySessionStore) map[string]struct{} {
	sessions, err := store.List()
	if err != nil {
		// The in-memory store never errors on List; on the off chance a future
		// backend does, fall back to keeping nothing rather than resurrecting orphans.
		return map[string]struct{}{}
	}
	keep := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		keep[s.ID] = struct{}{}
	}
	return keep
}

// checkpointSessions flushes both the session and history stores, logging (but
// not failing on) a write error — a checkpoint failure must not crash the
// running server; the next interval retries.
func checkpointSessions(logger *slog.Logger, sessStore *session.MemorySessionStore, histStore *session.MemoryHistoryStore, sessPath, histPath string) {
	if err := sessStore.Checkpoint(sessPath, time.Now()); err != nil {
		logger.Warn("session checkpoint failed", "err", err)
	}
	if err := histStore.Checkpoint(histPath); err != nil {
		logger.Warn("history checkpoint failed", "err", err)
	}
}
