package server

import (
	"sync"
	"testing"
	"time"

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

func TestWorkerSnapshotAndStatus(t *testing.T) {
	base := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	timeout := 30 * time.Second

	w := &worker{
		id:            "w",
		models:        []types.Model{{Name: "llama3"}},
		registeredAt:  base,
		lastHeartbeat: base,
		pending:       map[string]chan types.JobResult{},
	}

	// Heartbeat folds capacity in and re-stamps lastHeartbeat.
	w.applyHeartbeat(types.Heartbeat{
		ActiveJobs:      4,
		TotalVRAM:       16,
		FreeVRAM:        8,
		Load:            33,
		GPUType:         "gpu",
		AvailableModels: []types.Model{{Name: "mistral"}},
	}, base.Add(time.Second))

	snap := w.snapshot(base.Add(2*time.Second), timeout)
	if snap.Status != types.WorkerOnline {
		t.Fatalf("fresh heartbeat status = %v, want online", snap.Status)
	}
	if snap.ActiveJobs != 4 || snap.Load != 33 || snap.GPUType != "gpu" {
		t.Fatalf("snapshot capacity not applied: %+v", snap)
	}
	// RegisteredAt is surfaced from the worker (the worker-uptime metric base,
	// #24) and is independent of the heartbeat that re-stamped LastSeen.
	if !snap.RegisteredAt.Equal(base) {
		t.Fatalf("snapshot RegisteredAt = %v, want %v", snap.RegisteredAt, base)
	}
	if len(snap.Models) != 1 || snap.Models[0].Name != "mistral" {
		t.Fatalf("snapshot should reflect available_models: %+v", snap.Models)
	}

	// Past the timeout: stale, unavailable.
	stalePoint := base.Add(time.Second + timeout + time.Nanosecond)
	if !w.isStale(stalePoint, timeout) {
		t.Fatal("worker should be stale past the timeout")
	}
	if w.available(stalePoint, timeout) {
		t.Fatal("stale worker must not be available for routing")
	}
	if got := w.snapshot(stalePoint, timeout).Status; got != types.WorkerStale {
		t.Fatalf("stale snapshot status = %v, want stale", got)
	}

	// Draining trumps liveness for routing and status.
	w.markDraining()
	if w.available(base.Add(2*time.Second), timeout) {
		t.Fatal("draining worker must not be available")
	}
	if got := w.snapshot(base.Add(2*time.Second), timeout).Status; got != types.WorkerDraining {
		t.Fatalf("draining snapshot status = %v, want draining", got)
	}
}

func TestWorkerNeverHeartbeatedNotStale(t *testing.T) {
	// A zero lastHeartbeat (never seeded) is graced, not treated as ancient.
	w := &worker{id: "w", pending: map[string]chan types.JobResult{}}
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	if w.isStale(now, time.Second) {
		t.Fatal("never-heartbeated worker (zero lastHeartbeat) must not be stale")
	}
	if !w.available(now, time.Second) {
		t.Fatal("never-heartbeated worker should still be available")
	}
}
