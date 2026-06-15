// Command agentgpu is the unified single binary for agent-gpu. It exposes the
// server and worker as subcommands, matching the README quickstart:
//
//	agentgpu server start
//	agentgpu worker start --server host:port
//
// Only the internal gRPC control plane is wired up in this milestone; the
// public OpenAI-compatible HTTP API and the `key`/scheduling commands are
// added by later epics.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentgpu:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 {
		usage()
		return fmt.Errorf("expected a subcommand")
	}

	// Root context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	switch args[0] {
	case "server":
		return runServerCmd(ctx, logger, args[1:])
	case "worker":
		return runWorkerCmd(ctx, logger, args[1:])
	case "key":
		return runKeyCmd(ctx, os.Stdout, args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentgpu — distributed inference layer for Ollama

Usage:
  agentgpu server start [--listen host:port]
  agentgpu worker start --server host:port [--id worker-id]
  agentgpu key create --name <name> [--role r ...] [--allow-model m ...] [--deny-model m ...] [--store path]
  agentgpu key list [--store path]
  agentgpu key revoke <id> [--store path]
  agentgpu key rotate <id> [--store path]
  agentgpu key perms <id> [--role r ...] [--allow-model m ...] [--deny-model m ...] [--store path]

Configuration may also be supplied via environment variables:
  AGENTGPU_SERVER_LISTEN, AGENTGPU_SERVER_ADDR, AGENTGPU_WORKER_ID,
  AGENTGPU_STORE_PATH
`)
}
