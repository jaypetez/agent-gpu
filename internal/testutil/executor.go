package testutil

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// FakeExecutor is a single, configurable worker.Executor for tests. It folds the
// behaviours that were previously implemented as three separate fakes:
//
//   - scripted: emit a fixed sequence of deltas, then a result with a configured
//     prompt/completion token split (the old scriptedExecutor).
//   - echo / reply: emit a reply as one delta or character-by-character, with a
//     1/1/2 token split (the old recordingExecutor).
//   - blocking: emit some deltas, signal Emitted, then block until released or the
//     context is cancelled, never sending a terminal chunk (the old
//     blockingExecutor) — for mid-stream disconnect / in-flight-active tests.
//
// It also records, race-safely, every job it executed (Calls / LastJob) and every
// model Pull was asked for (Pulls), and exposes a handled count.
//
// All recording is mutex- or atomic-guarded so a test goroutine may read it while
// the worker's Execute goroutine writes it, clean under -race.
//
// Construct it with NewFakeExecutor(opts...); the zero-argument form is an echo
// executor advertising no models.
type FakeExecutor struct {
	models []types.Model
	// reply, when set, is the assistant content. With replyPerRune it is emitted
	// rune-by-rune; otherwise it is emitted as a single delta. Ignored when deltas
	// or echo is set.
	reply        string
	replyPerRune bool
	// deltas, when non-empty, are emitted one per JobChunk before the result.
	deltas []string
	// echo, when true, emits "echo: "+prompt as a single delta (the EchoExecutor
	// behaviour) and reports its whitespace token count.
	echo bool

	// toolCall, when non-nil, is added to the result with finish_reason
	// "tool_calls" and emitted as a tool-call delta.
	toolCall *types.ToolCall

	// promptTokens/completionTokens override the reported token split. When both
	// are zero a reply/echo executor derives a sensible default (see Execute).
	promptTokens     uint64
	completionTokens uint64

	// emitted, when non-nil, is closed once the executor has emitted its deltas,
	// so a test can deterministically wait for tokens to be in flight.
	emitted chan struct{}
	// block, when true, makes Execute wait on release (or ctx cancellation) after
	// emitting, modelling an in-flight job that never completes on its own.
	block bool
	// release, when non-nil, unblocks a blocking Execute; ctx cancellation also
	// unblocks it.
	release chan struct{}

	// pullErr is returned by Pull (nil = success). modelsErr is returned by
	// ListModels (nil = success). unloadErr is returned by Unload (nil = success).
	pullErr   error
	modelsErr error
	unloadErr error

	count   atomic.Int64
	mu      sync.Mutex
	lastJob *types.Job
	pulls   []string
	unloads []string
}

// Compile-time assertion that *FakeExecutor satisfies worker.Executor.
var _ worker.Executor = (*FakeExecutor)(nil)

// ExecutorOption configures a FakeExecutor.
type ExecutorOption func(*FakeExecutor)

// NewFakeExecutor builds a FakeExecutor with the given options. With no options it
// echoes the prompt (like worker.EchoExecutor) and advertises no models.
func NewFakeExecutor(opts ...ExecutorOption) *FakeExecutor {
	e := &FakeExecutor{}
	for _, opt := range opts {
		opt(e)
	}
	if !e.echo && len(e.deltas) == 0 && e.reply == "" && e.toolCall == nil {
		// Default to echo behaviour so a bare executor still produces output.
		e.echo = true
	}
	return e
}

// WithExecModels sets the models ListModels returns (and that the worker
// advertises) from bare names.
func WithExecModels(names ...string) ExecutorOption {
	return func(e *FakeExecutor) { e.models = modelsFromNames(names) }
}

// WithExecModelObjects sets ListModels' models from full types.Model values.
func WithExecModelObjects(models ...types.Model) ExecutorOption {
	return func(e *FakeExecutor) { e.models = models }
}

// WithReply makes Execute emit reply as a single assistant delta.
func WithReply(reply string) ExecutorOption {
	return func(e *FakeExecutor) {
		e.reply = reply
		e.echo = false
	}
}

// WithReplyPerRune makes Execute emit reply one rune per delta (so tests can
// observe streamed tokens). Implies WithReply(reply).
func WithReplyPerRune(reply string) ExecutorOption {
	return func(e *FakeExecutor) {
		e.reply = reply
		e.replyPerRune = true
		e.echo = false
	}
}

// WithDeltas makes Execute emit the given deltas, one per JobChunk, then return a
// result whose output is their concatenation.
func WithDeltas(deltas ...string) ExecutorOption {
	return func(e *FakeExecutor) {
		e.deltas = deltas
		e.echo = false
	}
}

// WithEcho forces the echo behaviour ("echo: "+prompt) explicitly.
func WithEcho() ExecutorOption {
	return func(e *FakeExecutor) {
		e.echo = true
		e.reply = ""
		e.deltas = nil
	}
}

// WithToolCall makes the result carry tc with finish_reason "tool_calls" and
// emits it as a tool-call delta (modelling a model that called a function).
func WithToolCall(tc types.ToolCall) ExecutorOption {
	return func(e *FakeExecutor) {
		c := tc
		e.toolCall = &c
	}
}

// WithTokens overrides the reported prompt/completion token split.
func WithTokens(prompt, completion uint64) ExecutorOption {
	return func(e *FakeExecutor) {
		e.promptTokens = prompt
		e.completionTokens = completion
	}
}

// WithEmitSignal wires a channel that Execute closes once it has emitted its
// deltas, so a test can wait for tokens to be in flight without sleeping. The
// channel must be unbuffered/freshly made; Execute closes it exactly once.
func WithEmitSignal(ch chan struct{}) ExecutorOption {
	return func(e *FakeExecutor) { e.emitted = ch }
}

// WithBlock makes Execute block after emitting until release is signalled or the
// context is cancelled, never sending a terminal chunk on its own. Pass a release
// channel to unblock deterministically; pass nil to rely on context cancellation
// (the mid-stream-disconnect case).
func WithBlock(release chan struct{}) ExecutorOption {
	return func(e *FakeExecutor) {
		e.block = true
		e.release = release
	}
}

// WithPullErr makes Pull return err.
func WithPullErr(err error) ExecutorOption {
	return func(e *FakeExecutor) { e.pullErr = err }
}

// WithListModelsErr makes ListModels return err.
func WithListModelsErr(err error) ExecutorOption {
	return func(e *FakeExecutor) { e.modelsErr = err }
}

// WithUnloadErr makes Unload return err.
func WithUnloadErr(err error) ExecutorOption {
	return func(e *FakeExecutor) { e.unloadErr = err }
}

// Execute implements worker.Executor.
func (e *FakeExecutor) Execute(ctx context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	e.count.Add(1)
	j := job
	e.mu.Lock()
	e.lastJob = &j
	e.mu.Unlock()

	output := e.emitOutput(job, emit)

	if e.emitted != nil {
		close(e.emitted)
	}
	if e.block {
		// Model an in-flight job: wait to be released or for the client to
		// disconnect (ctx cancellation). Never emit a terminal chunk; the worker
		// derives Done from the returned result only after Execute returns, so
		// returning here without the client seeing Done models a genuine abort.
		select {
		case <-ctx.Done():
		case <-e.releaseChan():
		}
	}

	prompt, completion := e.tokenSplit(output)
	res := types.JobResult{
		JobID:            job.ID,
		Output:           output,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		Tokens:           prompt + completion,
		FinishReason:     "stop",
	}
	if e.toolCall != nil {
		res.ToolCalls = []types.ToolCall{*e.toolCall}
		res.FinishReason = "tool_calls"
		if emit != nil {
			emit(types.JobChunk{JobID: job.ID, ToolCalls: res.ToolCalls})
		}
	}
	return res
}

// emitOutput streams the configured output and returns the accumulated string.
func (e *FakeExecutor) emitOutput(job types.Job, emit func(types.JobChunk)) string {
	switch {
	case len(e.deltas) > 0:
		var sb strings.Builder
		for _, d := range e.deltas {
			sb.WriteString(d)
			if emit != nil {
				emit(types.JobChunk{JobID: job.ID, Delta: d})
			}
		}
		return sb.String()
	case e.echo:
		out := "echo: " + job.Prompt
		if emit != nil {
			emit(types.JobChunk{JobID: job.ID, Delta: out})
		}
		return out
	case e.replyPerRune:
		for _, r := range e.reply {
			if emit != nil {
				emit(types.JobChunk{JobID: job.ID, Delta: string(r)})
			}
		}
		return e.reply
	default: // single-delta reply (possibly empty, e.g. a tool-call-only turn)
		if e.reply != "" && emit != nil {
			emit(types.JobChunk{JobID: job.ID, Delta: e.reply})
		}
		return e.reply
	}
}

// tokenSplit returns the reported prompt/completion tokens. An explicit override
// (WithTokens) wins; otherwise echo reports its whitespace token count as
// completion tokens and a reply reports a 1/1 split, matching the fakes this
// replaces.
func (e *FakeExecutor) tokenSplit(output string) (prompt, completion uint64) {
	if e.promptTokens != 0 || e.completionTokens != 0 {
		return e.promptTokens, e.completionTokens
	}
	if e.echo {
		return 0, uint64(len(strings.Fields(output)))
	}
	return 1, 1
}

// releaseChan returns a channel that is never ready when no release was wired, so
// a blocking Execute then relies solely on context cancellation.
func (e *FakeExecutor) releaseChan() <-chan struct{} {
	if e.release != nil {
		return e.release
	}
	return nil
}

// ListModels implements worker.Executor.
func (e *FakeExecutor) ListModels(context.Context) ([]types.Model, error) {
	if e.modelsErr != nil {
		return nil, e.modelsErr
	}
	return e.models, nil
}

// Pull implements worker.Executor: it records the requested model and returns the
// configured error (nil by default).
func (e *FakeExecutor) Pull(_ context.Context, model string) error {
	e.mu.Lock()
	e.pulls = append(e.pulls, model)
	e.mu.Unlock()
	return e.pullErr
}

// Unload implements worker.Executor: it records the requested model and returns
// the configured error (nil by default).
func (e *FakeExecutor) Unload(_ context.Context, model string) error {
	e.mu.Lock()
	e.unloads = append(e.unloads, model)
	e.mu.Unlock()
	return e.unloadErr
}

// Handled returns how many jobs Execute has run.
func (e *FakeExecutor) Handled() int64 { return e.count.Load() }

// LastJob returns a copy of the most recent job Execute ran, or nil if none.
func (e *FakeExecutor) LastJob() *types.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastJob == nil {
		return nil
	}
	j := *e.lastJob
	return &j
}

// Pulls returns the models Pull was called with, in order.
func (e *FakeExecutor) Pulls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.pulls...)
}

// Unloads returns the models Unload was called with, in order.
func (e *FakeExecutor) Unloads() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.unloads...)
}
