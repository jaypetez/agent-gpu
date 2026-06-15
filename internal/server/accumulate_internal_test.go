package server

import (
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/types"
)

func newTestWorker() *worker {
	return &worker{
		id:      "w",
		pending: make(map[string]chan types.JobResult),
		streams: make(map[string]*strings.Builder),
	}
}

// TestAccumulateResolvesOnDone verifies the server appends per-token deltas and
// resolves the waiter exactly once, with the fully accumulated output and the
// terminal chunk's token count, on the done chunk.
func TestAccumulateResolvesOnDone(t *testing.T) {
	t.Parallel()
	w := newTestWorker()
	ch := w.addPending("j1")

	w.accumulate(types.JobChunk{JobID: "j1", Delta: "Hello"})
	w.accumulate(types.JobChunk{JobID: "j1", Delta: ", "})
	w.accumulate(types.JobChunk{JobID: "j1", Delta: "world"})

	select {
	case <-ch:
		t.Fatal("waiter resolved before terminal chunk")
	default:
	}

	w.accumulate(types.JobChunk{JobID: "j1", Done: true, Tokens: 7})

	res := <-ch
	if res.Output != "Hello, world" {
		t.Fatalf("output = %q, want %q", res.Output, "Hello, world")
	}
	if res.Tokens != 7 {
		t.Fatalf("tokens = %d, want 7", res.Tokens)
	}
	if res.Err != nil {
		t.Fatalf("unexpected err: %v", res.Err)
	}
	// The per-job buffer is cleaned up.
	w.mu.Lock()
	_, ok := w.streams["j1"]
	w.mu.Unlock()
	if ok {
		t.Fatal("stream buffer was not cleaned up")
	}
}

// TestAccumulateTerminalErrorResolvesWaiter verifies that an Ollama failure
// arriving as a terminal error chunk resolves the waiter with the error (and
// discards partial output) rather than hanging it.
func TestAccumulateTerminalErrorResolvesWaiter(t *testing.T) {
	t.Parallel()
	w := newTestWorker()
	ch := w.addPending("j2")

	w.accumulate(types.JobChunk{JobID: "j2", Delta: "partial"})
	w.accumulate(types.JobChunk{
		JobID: "j2",
		Done:  true,
		Err:   &types.JobError{Code: "ollama_error", Message: "boom"},
	})

	res := <-ch
	if res.Err == nil || res.Err.Code != "ollama_error" {
		t.Fatalf("err = %v, want ollama_error", res.Err)
	}
	if res.Output != "" {
		t.Fatalf("output = %q, want empty on error", res.Output)
	}
}

// TestAccumulateResolvesExactlyOneWaiter verifies a second terminal chunk for
// the same job (or one with no waiter) is a harmless no-op.
func TestAccumulateResolvesExactlyOneWaiter(t *testing.T) {
	t.Parallel()
	w := newTestWorker()
	ch := w.addPending("j3")

	w.accumulate(types.JobChunk{JobID: "j3", Delta: "x"})
	w.accumulate(types.JobChunk{JobID: "j3", Done: true, Tokens: 1})
	<-ch // first resolution

	// A duplicate terminal chunk must not panic or block; the waiter is gone.
	w.accumulate(types.JobChunk{JobID: "j3", Done: true, Tokens: 1})
}
