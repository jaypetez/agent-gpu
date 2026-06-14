package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/server"
)

func runServerCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return fmt.Errorf("usage: agentgpu server start [--listen host:port]")
	}

	fs := flag.NewFlagSet("server start", flag.ContinueOnError)
	listen := fs.String("listen", "", "gRPC listen address (host:port)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg := config.ResolveServer(config.ServerConfig{Listen: *listen}, nil)
	return serveControlPlane(ctx, logger, cfg)
}

// serveControlPlane starts the gRPC control-plane server and blocks until ctx
// is cancelled (SIGINT/SIGTERM), then shuts down gracefully.
func serveControlPlane(ctx context.Context, logger *slog.Logger, cfg config.ServerConfig) error {
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}

	gs := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    server.DefaultKeepaliveTime,
			Timeout: server.DefaultKeepaliveTimeout,
		}),
	)
	srv := server.New(server.WithLogger(logger))
	srv.Register(gs)

	logger.Info("control-plane server listening", "addr", lis.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- gs.Serve(lis) }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received; stopping gracefully")
		gs.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
