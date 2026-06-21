package server_test

import (
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestServerSettersReflectLiveValues proves SetHeartbeatTimeout / SetModelWarmMax
// replace the live values (observed via the getters that read them under the same
// locks the consumers use) and that a non-positive value is rejected (#92).
func TestServerSettersReflectLiveValues(t *testing.T) {
	t.Parallel()
	srv := server.New(
		server.WithHeartbeatTimeout(45*time.Second),
		server.WithModelWarmMax(time.Hour),
	)

	if got := srv.HeartbeatTimeout(); got != 45*time.Second {
		t.Fatalf("initial HeartbeatTimeout = %v, want 45s", got)
	}
	if got := srv.ModelWarmMax(); got != time.Hour {
		t.Fatalf("initial ModelWarmMax = %v, want 1h", got)
	}

	srv.SetHeartbeatTimeout(90 * time.Second)
	srv.SetModelWarmMax(2 * time.Hour)
	if got := srv.HeartbeatTimeout(); got != 90*time.Second {
		t.Errorf("after SetHeartbeatTimeout: %v, want 90s", got)
	}
	if got := srv.ModelWarmMax(); got != 2*time.Hour {
		t.Errorf("after SetModelWarmMax: %v, want 2h", got)
	}

	// Non-positive values are rejected (the live value is kept).
	srv.SetHeartbeatTimeout(0)
	srv.SetHeartbeatTimeout(-time.Second)
	srv.SetModelWarmMax(0)
	if got := srv.HeartbeatTimeout(); got != 90*time.Second {
		t.Errorf("non-positive SetHeartbeatTimeout changed value to %v", got)
	}
	if got := srv.ModelWarmMax(); got != 2*time.Hour {
		t.Errorf("non-positive SetModelWarmMax changed value to %v", got)
	}
}

// TestSetHeartbeatTimeoutEvictsLive proves SetHeartbeatTimeout takes effect on the
// eviction read path with no restart (#92): a worker that is NOT stale under the
// long boot timeout becomes stale — and is evicted — once the timeout is lowered at
// runtime, with the clock fast-forwarded (no real sleep).
func TestSetHeartbeatTimeoutEvictsLive(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithHeartbeatTimeout(time.Hour), // generous boot timeout
		server.WithEvictScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	rc := dialRaw(t, h, "live-timeout-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// Advance well past a short timeout but FAR under the 1h boot timeout. The worker
	// must still be present — eviction has not fired.
	clk.advance(2 * time.Minute)
	if got := h.srv.WorkerCount(); got != 1 {
		t.Fatalf("worker evicted under generous timeout (count=%d)", got)
	}

	// Lower the timeout at runtime to 30s. The worker has now been silent for 2m, so
	// the very next eviction scan must reap it — proving the new timeout is in force
	// live with no restart.
	h.srv.SetHeartbeatTimeout(30 * time.Second)
	waitFor(t, 2*time.Second, "worker evicted after live timeout change", func() bool {
		return h.srv.WorkerCount() == 0
	})
}
