package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/ollama"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// parseModels turns a comma-separated --models flag value into the model list a
// worker advertises at registration and in heartbeats. Entries are trimmed and
// blanks dropped, so "llama3, mistral ,," yields two models. Real model/version
// discovery from the local Ollama is issue #11/#16; until then operators name
// the models their worker serves so the server can route by model.
func parseModels(s string) []types.Model {
	var models []types.Model
	for _, name := range strings.Split(s, ",") {
		if name = strings.TrimSpace(name); name != "" {
			models = append(models, types.Model{Name: name})
		}
	}
	return models
}

func runWorkerCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return fmt.Errorf("usage: agentgpu worker start --server host:port [--id worker-id] [--models name,name]")
	}

	fs := flag.NewFlagSet("worker start", flag.ContinueOnError)
	srvAddr := fs.String("server", "", "gRPC server address (host:port)")
	id := fs.String("id", "", "worker id (defaults to hostname)")
	hbInterval := fs.Duration("heartbeat-interval", 0, "heartbeat cadence (default 15s or $AGENTGPU_HEARTBEAT_INTERVAL)")
	modelsFlag := fs.String("models", "", "comma-separated models this worker serves (fallback/override; live set is sourced from Ollama)")
	ollamaURL := fs.String("ollama-url", "", "local Ollama base URL (default http://localhost:11434 or $AGENTGPU_OLLAMA_URL)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg := config.ResolveWorker(config.WorkerConfig{ServerAddr: *srvAddr, WorkerID: *id}, nil, nil)
	if cfg.ServerAddr == "" {
		return fmt.Errorf("--server is required (or set %s)", config.EnvWorkerServer)
	}
	heartbeatInterval := config.ResolveHeartbeatInterval(*hbInterval, nil)
	models := parseModels(*modelsFlag)
	resolvedOllamaURL := config.ResolveOllamaURL(*ollamaURL, nil)

	// The real worker runs inference against the local Ollama instance. Model
	// detection, listing, streaming chat, and permission-gated pull all flow
	// through this executor; --models is a fallback/override the worker seeds with
	// until Ollama's /api/tags is reachable.
	executor := worker.NewOllamaExecutor(ollama.New(resolvedOllamaURL))

	w := worker.New(worker.Config{
		ServerAddr:        cfg.ServerAddr,
		WorkerID:          cfg.WorkerID,
		Models:            models,
		HeartbeatInterval: heartbeatInterval,
		Executor:          executor,
		Logger:            logger,
	})

	logger.Info("starting worker", "id", cfg.WorkerID, "server", cfg.ServerAddr,
		"models", len(models), "ollama_url", resolvedOllamaURL)
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	logger.Info("worker stopped")
	return nil
}
