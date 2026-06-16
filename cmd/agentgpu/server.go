package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
)

// quotaCheckpointInterval is how often the server flushes the quota counters to
// the checkpoint file while running, bounding how much usage a crash can lose.
const quotaCheckpointInterval = 30 * time.Second

// httpShutdownTimeout bounds how long the HTTP server is given to drain
// in-flight requests on graceful shutdown before the process proceeds to stop
// the gRPC server.
const httpShutdownTimeout = 10 * time.Second

func runServerCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return fmt.Errorf("usage: agentgpu server start [--listen host:port]")
	}

	fs := flag.NewFlagSet("server start", flag.ContinueOnError)
	listen := fs.String("listen", "", "gRPC listen address (host:port)")
	httpListen := fs.String("http-listen", "", "public HTTP API listen address (host:port) (default 127.0.0.1:8080 or $AGENTGPU_HTTP_LISTEN)")
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	quotaPath := fs.String("quota-path", "", "path to the quota counter checkpoint (default $AGENTGPU_QUOTA_PATH or ~/.agentgpu/quota.json)")
	rpm := fs.Uint64("default-rpm", 0, "global default requests per minute (0 = unlimited)")
	tpm := fs.Uint64("default-tpm", 0, "global default tokens per minute (0 = unlimited)")
	daily := fs.Uint64("default-daily-tokens", 0, "global default daily token budget (0 = unlimited)")
	monthly := fs.Uint64("default-monthly-tokens", 0, "global default monthly token budget (0 = unlimited)")
	hbTimeout := fs.Duration("heartbeat-timeout", 0, "evict a worker after this long without a heartbeat (default 45s or $AGENTGPU_HEARTBEAT_TIMEOUT)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg := config.ResolveServer(config.ServerConfig{Listen: *listen, HTTPListen: *httpListen}, nil)
	heartbeatTimeout := config.ResolveHeartbeatTimeout(*hbTimeout, nil)
	qcfg := config.ResolveQuota(config.QuotaConfig{
		Path:                 *quotaPath,
		DefaultRPM:           *rpm,
		DefaultTPM:           *tpm,
		DefaultDailyTokens:   *daily,
		DefaultMonthlyTokens: *monthly,
	}, nil, nil)
	return serveControlPlane(ctx, logger, cfg, *storeFlag, qcfg, heartbeatTimeout)
}

// serveControlPlane starts the gRPC control-plane server and blocks until ctx
// is cancelled (SIGINT/SIGTERM), then shuts down gracefully, checkpointing the
// quota counters on the way out.
func serveControlPlane(ctx context.Context, logger *slog.Logger, cfg config.ServerConfig, storeFlag string, qcfg config.QuotaConfig, heartbeatTimeout time.Duration) error {
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
	)

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
	)
	srv.Register(gs)
	// Start the stale-worker eviction loop; Close stops it on shutdown.
	srv.Start()
	defer func() { _ = srv.Close() }()

	// Public HTTP API: authenticates Bearer tokens via the same key store and
	// permission-filters the catalog with the shared authorizer above.
	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(srv, authSvc, az, logger, cfg.HTTPListen)

	logger.Info("control-plane server listening",
		"addr", lis.Addr().String(), "http_addr", cfg.HTTPListen, "quota_path", qcfg.Path,
		"heartbeat_timeout", heartbeatTimeout.String())

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

	errCh := make(chan error, 2)
	go func() { errCh <- gs.Serve(lis) }()
	go func() {
		// ListenAndServe returns http.ErrServerClosed on graceful Shutdown; treat
		// that as a clean stop so it does not race the ctx.Done() path to errCh.
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http api: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received; stopping gracefully")
		// Drain HTTP first so no in-flight request races the control plane down.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http api shutdown failed", "err", err)
		}
		gs.GracefulStop()
		if err := cs.Checkpoint(qcfg.Path, time.Now()); err != nil {
			logger.Warn("quota checkpoint on shutdown failed", "err", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}
