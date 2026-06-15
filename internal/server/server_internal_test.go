package server

import (
	"sync"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// TestAddWorkerSessionsUnique exercises the nextSes counter under concurrency.
// addWorker increments s.nextSes; before the fix this was an unsynchronized
// read-modify-write shared across concurrent Connect goroutines, so concurrent
// registrations could observe the same session number. This test fans out many
// concurrent addWorker calls and asserts every returned session number is
// unique. Run with -race to catch the underlying data race directly.
func TestAddWorkerSessionsUnique(t *testing.T) {
	const n = 200
	s := New()

	var wg sync.WaitGroup
	wg.Add(n)
	results := make([]uint64, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			w := &worker{
				id:      "w",
				send:    make(chan *agentgpuv1.ServerMessage, 1),
				pending: make(map[string]chan types.JobResult),
			}
			<-start // maximize contention
			results[i] = s.addWorker(w)
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, ses := range results {
		if ses == 0 {
			t.Fatalf("session number must be non-zero")
		}
		if seen[ses] {
			t.Fatalf("duplicate session number %d handed out by addWorker", ses)
		}
		seen[ses] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique session numbers, got %d", n, len(seen))
	}
}
