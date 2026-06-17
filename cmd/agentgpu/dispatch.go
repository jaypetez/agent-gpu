package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/jaypetez/agent-gpu/internal/config"
)

// dispatch routes the non-informational subcommands. It is split from run so the
// signal-handling and logging setup (which the server/worker need) is established
// once here, after the version/help fast paths have been handled.
func dispatch(args []string) error {
	// Root context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Build the single root logger both the server and worker inherit (#23):
	// level/format/output resolved from env + defaults (flag > env > default), so
	// the log level is configurable without a code change. The redaction
	// ReplaceAttr and JSON-by-default encoding live in newLogger so they apply
	// uniformly to every subsystem that takes this logger. A file sink (if any) is
	// closed on the way out so its buffer is flushed.
	logger, closeLog, err := newLogger(config.ResolveLog(config.LogConfig{}, nil))
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	slog.SetDefault(logger)

	switch args[0] {
	case "server":
		return runServerCmd(ctx, logger, args[1:])
	case "worker":
		return runWorkerCmd(ctx, logger, args[1:])
	case "key":
		return runKeyCmd(ctx, os.Stdout, args[1:])
	case "quota":
		return runQuotaCmd(ctx, os.Stdout, args[1:])
	case "models":
		return runModelsCmd(ctx, os.Stdout, args[1:])
	case "loadtest":
		return runLoadtestCmd(ctx, logger, os.Stdout, args[1:])
	default:
		usage(os.Stderr)
		return usagef("unknown subcommand %q", args[0])
	}
}

// isNetworkError reports whether err is a transport-level failure (the server is
// unreachable, the connection was refused, DNS failed, or the request timed out)
// rather than an HTTP response. It is used to map such failures to exitNetwork.
// An *apiclient.APIError is a real HTTP response and is deliberately NOT a
// network error here.
func isNetworkError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// context deadline from the client's own timeout is a transport failure too.
	return errors.Is(err, context.DeadlineExceeded)
}
