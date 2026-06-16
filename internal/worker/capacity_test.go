package worker

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/gpu"
)

// stubDetector is a CapacityDetector returning a scripted Capacity and counting
// calls, for direct unit tests of the worker's capacity caching.
type stubDetector struct {
	calls atomic.Int64
	fn    func(n int64) gpu.Capacity
}

func (s *stubDetector) Detect(context.Context) gpu.Capacity {
	n := s.calls.Add(1)
	return s.fn(n)
}

// TestHeartbeatUsesStaticCapacityWhenNoDetector verifies the nil-Detector path:
// the worker reports the static cfg capacity fields unchanged (preserving the
// behavior existing callers/tests rely on).
func TestHeartbeatUsesStaticCapacityWhenNoDetector(t *testing.T) {
	t.Parallel()
	w := New(Config{
		WorkerID:  "w1",
		GPUType:   "manual-gpu",
		TotalVRAM: 8 << 30,
		FreeVRAM:  6 << 30,
		Load:      11,
		// Detector nil.
	})
	// detectCapacity is a no-op with no detector; capacity stays the seeded static.
	w.detectCapacity(context.Background(), true)
	w.detectCapacity(context.Background(), false)

	hb := w.heartbeat(3)
	if hb.GetGpuType() != "manual-gpu" {
		t.Fatalf("gpu type = %q, want manual-gpu", hb.GetGpuType())
	}
	if hb.GetTotalVramBytes() != 8<<30 || hb.GetFreeVramBytes() != 6<<30 {
		t.Fatalf("vram total=%d free=%d, want 8Gi/6Gi", hb.GetTotalVramBytes(), hb.GetFreeVramBytes())
	}
	if hb.GetLoad() != 11 {
		t.Fatalf("load = %d, want 11", hb.GetLoad())
	}
	if hb.GetActiveJobs() != 3 {
		t.Fatalf("active jobs = %d, want 3", hb.GetActiveJobs())
	}
}

// TestDetectCapacityStaticThenDynamic verifies the two-phase capacity refresh:
// a full (startup) detection adopts the whole snapshot, and a subsequent dynamic
// (per-heartbeat) refresh updates only free VRAM + load while preserving the
// startup type + total VRAM, even when the detector reports a different identity.
func TestDetectCapacityStaticThenDynamic(t *testing.T) {
	t.Parallel()
	det := &stubDetector{fn: func(n int64) gpu.Capacity {
		if n == 1 {
			return gpu.Capacity{Type: "GPU-A", TotalVRAM: 24 << 30, FreeVRAM: 24 << 30, Load: 0}
		}
		// A later probe that (wrongly) reports a different identity must NOT change
		// the cached type/total; only free/load are taken from it.
		return gpu.Capacity{Type: "GPU-B", TotalVRAM: 1 << 30, FreeVRAM: 5 << 30, Load: 60}
	}}
	w := New(Config{WorkerID: "w1", Detector: det})

	// Startup: adopt the full snapshot.
	w.detectCapacity(context.Background(), true)
	if got := w.currentCapacity(); got.Type != "GPU-A" || got.TotalVRAM != 24<<30 || got.FreeVRAM != 24<<30 || got.Load != 0 {
		t.Fatalf("after startup: %+v", got)
	}

	// Per-heartbeat: refresh only the dynamic signals.
	w.detectCapacity(context.Background(), false)
	got := w.currentCapacity()
	if got.Type != "GPU-A" || got.TotalVRAM != 24<<30 {
		t.Fatalf("dynamic refresh changed identity: %+v", got)
	}
	if got.FreeVRAM != 5<<30 || got.Load != 60 {
		t.Fatalf("dynamic refresh did not update free/load: %+v", got)
	}
}

// TestNewSeedsCapacityFromStaticConfig verifies the worker reports the static
// config capacity before any detection has run (and forever, with no detector).
func TestNewSeedsCapacityFromStaticConfig(t *testing.T) {
	t.Parallel()
	w := New(Config{WorkerID: "w1", GPUType: "seed", TotalVRAM: 1 << 30, FreeVRAM: 1 << 29, Load: 5})
	got := w.currentCapacity()
	want := gpu.Capacity{Type: "seed", TotalVRAM: 1 << 30, FreeVRAM: 1 << 29, Load: 5}
	if got != want {
		t.Fatalf("seeded capacity = %+v, want %+v", got, want)
	}
}
