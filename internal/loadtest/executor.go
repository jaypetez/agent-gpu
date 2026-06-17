package loadtest

import (
	"context"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// slowEchoExecutor wraps worker.EchoExecutor with an artificial per-request
// processing delay. It is a load-test-only Executor used by the in-process stack
// to make queueing observable model-free: the plain echo backend returns
// instantly, so a worker is never busy long enough for a backlog to form. With a
// small think delay each job occupies its worker for that long, so under enough
// concurrency jobs queue (non-zero depth + wait-time) and, with a bounded queue,
// excess requests are rejected with 503 — exactly the saturation the harness
// needs to exercise without a real model.
//
// It embeds worker.EchoExecutor so ListModels and Pull (and any future Executor
// methods) are inherited unchanged; only Execute is overridden to sleep first.
type slowEchoExecutor struct {
	worker.EchoExecutor
	think time.Duration
}

// newSlowEchoExecutor returns an Executor that echoes after sleeping for think.
// A non-positive think yields the plain instant echo behavior.
func newSlowEchoExecutor(models []types.Model, think time.Duration) worker.Executor {
	return slowEchoExecutor{EchoExecutor: worker.EchoExecutor{Models: models}, think: think}
}

// Execute sleeps for the configured think delay (honoring ctx cancellation so a
// disconnect/shutdown aborts promptly) and then delegates to the embedded echo
// executor for the actual (instant) echo result. The sleep is what makes the
// worker "busy", building a queue backlog under load.
func (e slowEchoExecutor) Execute(ctx context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	if e.think > 0 {
		t := time.NewTimer(e.think)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			// Aborted mid-think: return a terminal result carrying the context error
			// so the worker resolves the waiter rather than hanging.
			return types.JobResult{JobID: job.ID, Err: &types.JobError{Code: "aborted", Message: ctx.Err().Error()}}
		}
	}
	return e.EchoExecutor.Execute(ctx, job, emit)
}
