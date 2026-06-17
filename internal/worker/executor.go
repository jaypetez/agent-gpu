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
	// Unload evicts a model from the backend's memory, freeing its VRAM. It backs
	// the model-warmth feature's explicit-release path (#35): the server asks the
	// session's bound worker to unload the conversation's model when the session is
	// ended, rather than waiting out the keep_alive window. It is best-effort — a
	// missing/already-unloaded model is not an error — so it never fails a caller.
	Unload(ctx context.Context, model string) error
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
		JobID:            job.ID,
		Output:           output,
		Tokens:           tokens,
		CompletionTokens: tokens,
		FinishReason:     "stop",
	}
}

// ListModels returns the configured model list (empty if none).
func (e EchoExecutor) ListModels(_ context.Context) ([]types.Model, error) {
	return e.Models, nil
}

// Pull is a no-op for the echo stub: it advertises no real backend to pull onto.
func (e EchoExecutor) Pull(_ context.Context, _ string) error { return nil }

// Unload is a no-op for the echo stub: it holds no model in memory to evict.
func (e EchoExecutor) Unload(_ context.Context, _ string) error { return nil }

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
// chunk per produced token (and per tool-call delta). A chat job (job.Messages
// non-empty) threads the full conversation plus tools to Ollama /api/chat; a
// prompt-only job (the /v1/completions and #11 path) wraps job.Prompt as a
// single user message. It returns the accumulated output, the token split, any
// tool calls the model emitted, and an OpenAI finish_reason derived from
// Ollama's done_reason. On failure it returns a JobResult carrying the mapped
// *types.JobError; the worker turns that into a terminal error chunk so the
// waiter never hangs.
func (e *OllamaExecutor) Execute(ctx context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	messages := job.Messages
	if len(messages) == 0 {
		// Prompt-only job: present it to Ollama as a single user turn.
		messages = []types.Message{{Role: "user", Content: job.Prompt}}
	}

	var sb strings.Builder
	res, err := e.client.ChatStream(ctx, ollama.ChatRequest{
		Model:     job.Model,
		Messages:  messages,
		Tools:     job.Tools,
		KeepAlive: keepAlive(job.KeepAliveSeconds),
	}, func(delta string, calls []types.ToolCall) {
		sb.WriteString(delta)
		if emit != nil && (delta != "" || len(calls) > 0) {
			emit(types.JobChunk{JobID: job.ID, Delta: delta, ToolCalls: calls})
		}
	})
	if err != nil {
		return types.JobResult{JobID: job.ID, Output: sb.String(), Err: jobError(err), Tokens: res.Tokens}
	}
	return types.JobResult{
		JobID:            job.ID,
		Output:           sb.String(),
		Tokens:           res.Tokens,
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		ToolCalls:        res.ToolCalls,
		FinishReason:     finishReason(res),
	}
}

// keepAlive maps a job's KeepAliveSeconds (#35) to the optional keep_alive value
// the ollama client sends. A zero value means "unset" — return nil so the client
// omits keep_alive and Ollama's default unload window applies (back-compat with
// every pre-#35 job, which carries 0). A non-zero value (a session's warm window,
// or a negative "keep loaded" sentinel) is passed through as a pointer so it is
// sent verbatim.
func keepAlive(seconds int64) *int64 {
	if seconds == 0 {
		return nil
	}
	return &seconds
}

// finishReason derives the OpenAI finish_reason from a chat result. A tool call
// always takes precedence (OpenAI reports "tool_calls" even when Ollama's
// done_reason is "stop"); otherwise Ollama's done_reason maps through, with
// "length" preserved and an empty/"stop" reason normalized to "stop".
func finishReason(res ollama.ChatResult) string {
	if len(res.ToolCalls) > 0 {
		return "tool_calls"
	}
	switch res.DoneReason {
	case "length":
		return "length"
	case "", "stop":
		return "stop"
	default:
		return res.DoneReason
	}
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

// Unload evicts a model from Ollama's memory (keep_alive=0), freeing its VRAM.
func (e *OllamaExecutor) Unload(ctx context.Context, model string) error {
	return e.client.Unload(ctx, model)
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
