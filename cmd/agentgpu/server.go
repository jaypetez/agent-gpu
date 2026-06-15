package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
)

// quotaCheckpointInterval is how often the server flushes the quota counters to
// the checkpoint file while running, bounding how much usage a crash can lose.
const quotaCheckpointInterval = 30 * time.Second

func runServerCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return fmt.Errorf("usage: agentgpu server start [--listen host:port]")
	}

	fs := flag.NewFlagSet("server start", flag.ContinueOnError)
	listen := fs.String("listen", "", "gRPC listen address (host:port)")
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	quotaPath := fs.String("quota-path", "", "path to the quota counter checkpoint (default $AGENTGPU_QUOTA_PATH or ~/.agentgpu/quota.json)")
	rpm := fs.Uint64("default-rpm", 0, "global default requests per minute (0 = unlimited)")
	tpm := fs.Uint64("default-tpm", 0, "global default tokens per minute (0 = unlimited)")
	daily := fs.Uint64("default-daily-tokens", 0, "global default daily token budget (0 = unlimited)")
	monthly := fs.Uint64("default-monthly-tokens", 0, "global default monthly token budget (0 = unlimited)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg := config.ResolveServer(config.ServerConfig{Listen: *listen}, nil)
	qcfg := config.ResolveQuota(config.QuotaConfig{
		Path:                 *quotaPath,
		DefaultRPM:           *rpm,
		DefaultTPM:           *tpm,
		DefaultDailyTokens:   *daily,
		DefaultMonthlyTokens: *monthly,
	}, nil, nil)
	return serveControlPlane(ctx, logger, cfg, *storeFlag, qcfg)
}

// serveControlPlane starts the gRPC control-plane server and blocks until ctx
// is cancelled (SIGINT/SIGTERM), then shuts down gracefully, checkpointing the
// quota counters on the way out.
func serveControlPlane(ctx context.Context, logger *slog.Logger, cfg config.ServerConfig, storeFlag string, qcfg config.QuotaConfig) error {
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
	srv := server.New(server.WithLogger(logger), server.WithStore(st), server.WithQuota(eng))
	srv.Register(gs)

	logger.Info("control-plane server listening", "addr", lis.Addr().String(), "quota_path", qcfg.Path)

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

	errCh := make(chan error, 1)
	go func() { errCh <- gs.Serve(lis) }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received; stopping gracefully")
		gs.GracefulStop()
		if err := cs.Checkpoint(qcfg.Path, time.Now()); err != nil {
			logger.Warn("quota checkpoint on shutdown failed", "err", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}
