package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
)

// dispatch routes the non-informational subcommands. It is split from run so the
// signal-handling and logging setup (which the server/worker need) is established
// once here, after the version/help fast paths have been handled.
func dispatch(args []string) error {
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
	case "quota":
		return runQuotaCmd(ctx, os.Stdout, args[1:])
	case "models":
		return runModelsCmd(ctx, os.Stdout, args[1:])
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
