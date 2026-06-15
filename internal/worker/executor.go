package worker

import (
	"context"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/ollama"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// Executor runs inference jobs and exposes the worker's local model operations.
// Inference is streaming: Execute calls emit with a per-token JobChunk as
// output is produced, then returns the final JobResult; the worker forwards each
// chunk to the server over the control stream. ListModels and Pull back the
// worker's model advertisement and the permission-gated pull path respectively.
//
// EchoExecutor is the default stub; OllamaExecutor is the real implementation
// over a local Ollama instance.
type Executor interface {
	// Execute runs job and streams its output. emit is called once per output
	// delta with a JobChunk{Delta: ...} (Done false); after Execute returns the
	// worker sends a terminal JobChunk{Done: true} carrying the result's tokens or
	// error. Implementations need not emit a terminal chunk themselves — they
	// return the final JobResult and the worker derives the terminal chunk from
	// it. emit may be nil-safe at the call site but is always non-nil here.
	Execute(ctx context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult
	// ListModels returns the models currently available to serve, for heartbeat
	// advertisement.
	ListModels(ctx context.Context) ([]types.Model, error)
	// Pull fetches a model onto the worker. It is invoked only after the server
	// has authorized the requesting key for the Pull action.
	Pull(ctx context.Context, model string) error
}

// EchoExecutor is the stub executor: it echoes the prompt back as output. It
// implements the streaming Executor by emitting the echoed output as a single
// delta chunk so the server accumulates exactly the same final string the
// foundational round-trip tests expect.
type EchoExecutor struct {
	// Models is returned by ListModels (the worker's configured advertisement).
	Models []types.Model
}

// Execute implements Executor. It emits the echoed output as one delta chunk
// and returns a final result whose token count is the number of
// whitespace-separated tokens in the output, so quota accounting (#5) is
// testable now; real token counts come from OllamaExecutor.
func (e EchoExecutor) Execute(_ context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	output := "echo: " + job.Prompt
	tokens := uint64(len(strings.Fields(output)))
	if emit != nil {
		emit(types.JobChunk{JobID: job.ID, Delta: output})
	}
	return types.JobResult{
		JobID:  job.ID,
		Output: output,
		Tokens: tokens,
	}
}

// ListModels returns the configured model list (empty if none).
func (e EchoExecutor) ListModels(_ context.Context) ([]types.Model, error) {
	return e.Models, nil
}

// Pull is a no-op for the echo stub: it advertises no real backend to pull onto.
func (e EchoExecutor) Pull(_ context.Context, _ string) error { return nil }

// OllamaExecutor implements Executor over a local Ollama instance. Execute
// streams /api/chat token-by-token; ListModels reads /api/tags; Pull drives
// /api/pull.
type OllamaExecutor struct {
	client *ollama.Client
}

// NewOllamaExecutor constructs an OllamaExecutor over the given client.
func NewOllamaExecutor(client *ollama.Client) *OllamaExecutor {
	return &OllamaExecutor{client: client}
}

// Execute streams chat inference for the job against Ollama, emitting a delta
// chunk per produced token. It returns the accumulated output plus the token
// count Ollama reports (eval_count + prompt_eval_count). On failure it returns a
// JobResult carrying the mapped *types.JobError; the worker turns that into a
// terminal error chunk so the waiter never hangs.
func (e *OllamaExecutor) Execute(ctx context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	var sb strings.Builder
	tokens, err := e.client.Chat(ctx, job.Model, job.Prompt, func(delta string) {
		sb.WriteString(delta)
		if emit != nil {
			emit(types.JobChunk{JobID: job.ID, Delta: delta})
		}
	})
	if err != nil {
		return types.JobResult{JobID: job.ID, Output: sb.String(), Err: jobError(err), Tokens: tokens}
	}
	return types.JobResult{JobID: job.ID, Output: sb.String(), Tokens: tokens}
}

// Version reports the running Ollama version. It satisfies the worker's
// optional version-probe capability used for startup backend detection.
func (e *OllamaExecutor) Version(ctx context.Context) (string, error) {
	return e.client.Version(ctx)
}

// ListModels lists the models Ollama has available locally.
func (e *OllamaExecutor) ListModels(ctx context.Context) ([]types.Model, error) {
	return e.client.ListModels(ctx)
}

// Pull fetches a model onto Ollama.
func (e *OllamaExecutor) Pull(ctx context.Context, model string) error {
	return e.client.Pull(ctx, model)
}

// jobError coerces an error returned by the ollama client into a *types.JobError.
// The client returns *types.JobError directly, but guard the general case so a
// nil-typed error never slips through.
func jobError(err error) *types.JobError {
	if err == nil {
		return nil
	}
	if je, ok := err.(*types.JobError); ok && je != nil {
		return je
	}
	return &types.JobError{Code: ollama.CodeOllamaError, Message: err.Error()}
}
