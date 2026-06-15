package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

func runWorkerCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return fmt.Errorf("usage: agentgpu worker start --server host:port [--id worker-id]")
	}

	fs := flag.NewFlagSet("worker start", flag.ContinueOnError)
	srvAddr := fs.String("server", "", "gRPC server address (host:port)")
	id := fs.String("id", "", "worker id (defaults to hostname)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg := config.ResolveWorker(config.WorkerConfig{ServerAddr: *srvAddr, WorkerID: *id}, nil, nil)
	if cfg.ServerAddr == "" {
		return fmt.Errorf("--server is required (or set %s)", config.EnvWorkerServer)
	}

	w := worker.New(worker.Config{
		ServerAddr: cfg.ServerAddr,
		WorkerID:   cfg.WorkerID,
		Logger:     logger,
	})

	logger.Info("starting worker", "id", cfg.WorkerID, "server", cfg.ServerAddr)
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	logger.Info("worker stopped")
	return nil
}
