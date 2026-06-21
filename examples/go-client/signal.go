package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// signalContext returns a context that is cancelled on SIGINT (Ctrl-C) or
// SIGTERM, plus a stop function the caller must defer to release the signal
// handler. Cancelling the context — rather than imposing a deadline — is how a
// long streaming generation is stopped cleanly: the in-flight HTTP request is
// aborted via the context, the partial output already printed is kept, and the
// process exits with the conventional interrupt code.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
