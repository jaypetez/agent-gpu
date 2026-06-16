package server_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/gpu"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// fakeDetector is a worker.CapacityDetector that returns a scripted Capacity.
// detectN counts how many times Detect ran so a test can assert detection
// happens both at startup (static identity) and per heartbeat (dynamic refresh).
// The free VRAM and load it returns can change over time via a swappable
// function so a test can prove the per-heartbeat refresh actually re-reads them.
type fakeDetector struct {
	detectN atomic.Int64
	fn      func(n int64) gpu.Capacity
}

func (f *fakeDetector) Detect(context.Context) gpu.Capacity {
	n := f.detectN.Add(1)
	return f.fn(n)
}

// TestWorkerReportsDetectedGPUCapacity covers the issue #16 end-to-end wiring:
// a worker configured with a GPU detector folds the detected type/total/free
// VRAM and load into its heartbeats, and the server's fleet view reflects them.
// This is the GPU analog of TestWorkerReportsCapacityAndActiveJobs, which feeds
// static cfg fields; here the values come from the injected detector.
func TestWorkerReportsDetectedGPUCapacity(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		totalVRAM = uint64(24564) * 1024 * 1024 // 24564 MiB, as nvidia-smi would report
		freeFirst = uint64(24000) * 1024 * 1024
	)
	det := &fakeDetector{fn: func(int64) gpu.Capacity {
		return gpu.Capacity{
			Type:      "NVIDIA GeForce RTX 4090",
			TotalVRAM: totalVRAM,
			FreeVRAM:  freeFirst,
			Load:      37,
		}
	}}

	w := newWorkerWithCapacity(h, "gpu-worker", worker.Config{
		Models:   []types.Model{{Name: "llama3"}},
		Detector: det,
	})
	go func() { _ = w.Run(ctx) }()

	waitFor(t, 2*time.Second, "worker to register", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Detected capacity surfaces in the fleet view via heartbeats.
	waitFor(t, 2*time.Second, "detected capacity heartbeat", func() bool {
		fw, ok := fleetByID(h.srv, "gpu-worker")
		return ok &&
			fw.GPUType == "NVIDIA GeForce RTX 4090" &&
			fw.TotalVRAM == totalVRAM &&
			fw.FreeVRAM == freeFirst &&
			fw.Load == 37
	})

	// Detection must have run (startup + at least one heartbeat refresh).
	if det.detectN.Load() < 2 {
		t.Fatalf("detector ran %d times, want >= 2 (startup + per-heartbeat)", det.detectN.Load())
	}
}

// TestWorkerHeartbeatRefreshesDynamicCapacity verifies that the per-heartbeat
// refresh actually re-reads the dynamic signals (free VRAM + load) while keeping
// the startup-captured identity. The detector reports plummeting free VRAM and
// rising load over successive calls; the fleet view must move to the later
// values while the type/total stay put.
func TestWorkerHeartbeatRefreshesDynamicCapacity(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const totalVRAM = uint64(16) * 1024 * 1024 * 1024
	det := &fakeDetector{fn: func(n int64) gpu.Capacity {
		// First call (startup) reports full and idle; later calls report busier.
		// The identity (type/total) is constant; free/load change.
		if n <= 1 {
			return gpu.Capacity{Type: "Detected GPU", TotalVRAM: totalVRAM, FreeVRAM: totalVRAM, Load: 0}
		}
		return gpu.Capacity{Type: "Detected GPU", TotalVRAM: totalVRAM, FreeVRAM: 1 << 30, Load: 88}
	}}

	w := newWorkerWithCapacity(h, "dyn-worker", worker.Config{
		Models:   []types.Model{{Name: "llama3"}},
		Detector: det,
	})
	go func() { _ = w.Run(ctx) }()

	waitFor(t, 2*time.Second, "worker to register", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Eventually the heartbeat reports the busier dynamic values...
	waitFor(t, 2*time.Second, "dynamic capacity refresh", func() bool {
		fw, ok := fleetByID(h.srv, "dyn-worker")
		return ok && fw.FreeVRAM == 1<<30 && fw.Load == 88
	})
	// ...while the startup-captured identity is preserved.
	fw, _ := fleetByID(h.srv, "dyn-worker")
	if fw.GPUType != "Detected GPU" || fw.TotalVRAM != totalVRAM {
		t.Fatalf("identity changed: type=%q total=%d, want %q/%d", fw.GPUType, fw.TotalVRAM, "Detected GPU", totalVRAM)
	}
}

// TestWorkerCPUFallbackHeartbeat verifies a GPU-less worker (detector returns
// the CPU fallback) registers and heartbeats cleanly, reporting cpu/zero VRAM —
// the CPU-mode AC, exercised through the real worker→server path.
func TestWorkerCPUFallbackHeartbeat(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	det := &fakeDetector{fn: func(int64) gpu.Capacity {
		return gpu.Capacity{Type: gpu.CPUType} // zero VRAM, zero load
	}}

	w := newWorkerWithCapacity(h, "cpu-worker", worker.Config{
		Models:   []types.Model{{Name: "llama3"}},
		Detector: det,
	})
	go func() { _ = w.Run(ctx) }()

	waitFor(t, 2*time.Second, "worker to register", func() bool {
		return h.srv.WorkerCount() == 1
	})
	waitFor(t, 2*time.Second, "cpu-mode heartbeat", func() bool {
		fw, ok := fleetByID(h.srv, "cpu-worker")
		return ok && fw.GPUType == gpu.CPUType && fw.TotalVRAM == 0 && fw.FreeVRAM == 0 && fw.Load == 0
	})
}
