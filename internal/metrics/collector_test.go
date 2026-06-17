package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// fakeStatsSource is a deterministic StatsSource for the collector tests: it
// returns fixed fleet/queue/wait-time/affinity snapshots so the emitted metrics
// can be asserted exactly against known state, with no gRPC server in the loop.
type fakeStatsSource struct {
	fleet    []types.Worker
	queue    queue.Stats
	wait     server.WaitTimeStats
	affinity server.AffinityStats
}

func (f *fakeStatsSource) Fleet() []types.Worker               { return f.fleet }
func (f *fakeStatsSource) QueueStats() queue.Stats             { return f.queue }
func (f *fakeStatsSource) WaitTimeStats() server.WaitTimeStats { return f.wait }
func (f *fakeStatsSource) AffinityStats() server.AffinityStats { return f.affinity }

// collectInto gathers a one-off registry containing only the server collector
// over src, returning the gathered exposition as text for substring assertions
// and the registry for testutil helpers.
func registerCollector(t *testing.T, src StatsSource) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(newServerCollector(src)); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	return reg
}

// TestCollectorQueueDepth asserts queue_depth is emitted for every priority,
// reflecting the fake's per-priority counts and an explicit 0 for empties.
func TestCollectorQueueDepth(t *testing.T) {
	src := &fakeStatsSource{
		queue: queue.Stats{
			Total: 5,
			ByPriority: map[queue.Priority]int{
				queue.PriorityHigh:   2,
				queue.PriorityNormal: 3,
				// PriorityLow intentionally absent -> must surface as 0.
			},
		},
	}
	reg := registerCollector(t, src)

	want := `
# HELP agentgpu_queue_depth Number of jobs currently waiting in the scheduling queue, by priority.
# TYPE agentgpu_queue_depth gauge
agentgpu_queue_depth{priority="high"} 2
agentgpu_queue_depth{priority="low"} 0
agentgpu_queue_depth{priority="normal"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentgpu_queue_depth"); err != nil {
		t.Fatalf("queue_depth mismatch:\n%v", err)
	}
}

// TestCollectorFleetGauges asserts the per-worker gauges (GPU utilization from
// Load, VRAM total/free, active jobs, and the start time from RegisteredAt) are
// emitted from the fleet snapshot, and that the per-status worker count is
// present for every status.
func TestCollectorFleetGauges(t *testing.T) {
	registered := time.Unix(1_700_000_000, 0)
	src := &fakeStatsSource{
		fleet: []types.Worker{
			{
				ID:           "w1",
				Load:         42,
				GPUType:      "NVIDIA RTX 4090",
				TotalVRAM:    24 << 30,
				FreeVRAM:     12 << 30,
				ActiveJobs:   3,
				Status:       types.WorkerOnline,
				RegisteredAt: registered,
			},
		},
	}
	reg := registerCollector(t, src)

	// GPU utilization mirrors Load, labeled by worker + gpu_type.
	wantUtil := `
# HELP agentgpu_worker_gpu_utilization Reported mean GPU utilization of a worker, 0-100.
# TYPE agentgpu_worker_gpu_utilization gauge
agentgpu_worker_gpu_utilization{gpu_type="NVIDIA RTX 4090",worker="w1"} 42
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantUtil), "agentgpu_worker_gpu_utilization"); err != nil {
		t.Fatalf("worker_gpu_utilization mismatch:\n%v", err)
	}

	wantStart := `
# HELP agentgpu_worker_start_time_seconds Unix timestamp at which a worker registered with the server. Compute uptime as time() - this (resets on reconnect).
# TYPE agentgpu_worker_start_time_seconds gauge
agentgpu_worker_start_time_seconds{worker="w1"} 1.7e+09
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantStart), "agentgpu_worker_start_time_seconds"); err != nil {
		t.Fatalf("worker_start_time_seconds mismatch:\n%v", err)
	}

	wantVRAM := `
# HELP agentgpu_worker_vram_bytes Total video memory reported by a worker, in bytes.
# TYPE agentgpu_worker_vram_bytes gauge
agentgpu_worker_vram_bytes{worker="w1"} 2.5769803776e+10
# HELP agentgpu_worker_vram_free_bytes Currently-free video memory reported by a worker, in bytes.
# TYPE agentgpu_worker_vram_free_bytes gauge
agentgpu_worker_vram_free_bytes{worker="w1"} 1.2884901888e+10
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantVRAM), "agentgpu_worker_vram_bytes", "agentgpu_worker_vram_free_bytes"); err != nil {
		t.Fatalf("worker_vram mismatch:\n%v", err)
	}

	// Active jobs gauge reflects the snapshot.
	wantActive := `
# HELP agentgpu_worker_active_jobs Number of jobs currently in flight on a worker.
# TYPE agentgpu_worker_active_jobs gauge
agentgpu_worker_active_jobs{worker="w1"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantActive), "agentgpu_worker_active_jobs"); err != nil {
		t.Fatalf("worker_active_jobs mismatch:\n%v", err)
	}

	// Every status series is present, even at zero.
	wantFleet := `
# HELP agentgpu_fleet_workers Number of workers currently connected to the fleet, by status (online|draining|stale).
# TYPE agentgpu_fleet_workers gauge
agentgpu_fleet_workers{status="draining"} 0
agentgpu_fleet_workers{status="online"} 1
agentgpu_fleet_workers{status="stale"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantFleet), "agentgpu_fleet_workers"); err != nil {
		t.Fatalf("fleet_workers mismatch:\n%v", err)
	}
}

// TestCollectorStartTimeOmittedWhenZero proves a worker snapshot without a
// registration timestamp emits no start-time series (rather than a bogus 1970).
func TestCollectorStartTimeOmittedWhenZero(t *testing.T) {
	src := &fakeStatsSource{
		fleet: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}, // RegisteredAt zero
	}
	reg := registerCollector(t, src)
	if n := testutil.CollectAndCount(newServerCollector(src), "agentgpu_worker_start_time_seconds"); n != 0 {
		t.Fatalf("worker_start_time_seconds emitted %d series for a zero RegisteredAt, want 0", n)
	}
	// Sanity: the registry still gathers (other metrics present).
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}
}

// TestCollectorWaitHistogram asserts the time-in-queue const histogram reflects
// the server's cumulative ms buckets converted to seconds, with the +Inf
// sentinel dropped and the sample count/sum preserved.
func TestCollectorWaitHistogram(t *testing.T) {
	// 3 samples: cumulative counts at <=100ms:1, <=500ms:2, <=1000ms:3, +Inf:3.
	src := &fakeStatsSource{
		wait: server.WaitTimeStats{
			Count: 3,
			SumMs: 1600, // 0.1 + 0.5 + 1.0 = 1.6s
			MaxMs: 1000,
			Buckets: []server.WaitBucket{
				{LeMs: 100, Count: 1},
				{LeMs: 500, Count: 2},
				{LeMs: 1000, Count: 3},
				{LeMs: 0, Count: 3}, // +Inf sentinel
			},
		},
	}
	reg := registerCollector(t, src)

	want := `
# HELP agentgpu_queue_wait_seconds Time jobs spent queued before being placed on a worker (placement path only), in seconds.
# TYPE agentgpu_queue_wait_seconds histogram
agentgpu_queue_wait_seconds_bucket{le="0.1"} 1
agentgpu_queue_wait_seconds_bucket{le="0.5"} 2
agentgpu_queue_wait_seconds_bucket{le="1"} 3
agentgpu_queue_wait_seconds_bucket{le="+Inf"} 3
agentgpu_queue_wait_seconds_sum 1.6
agentgpu_queue_wait_seconds_count 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentgpu_queue_wait_seconds"); err != nil {
		t.Fatalf("queue_wait_seconds mismatch:\n%v", err)
	}
}

// TestCollectorAffinity asserts affinity hit/miss surface as the affinity_total
// counter keyed by result.
func TestCollectorAffinity(t *testing.T) {
	src := &fakeStatsSource{affinity: server.AffinityStats{Hits: 7, Misses: 2}}
	reg := registerCollector(t, src)

	want := `
# HELP agentgpu_affinity_total Session-affinity routing outcomes since startup, by result (hit|miss).
# TYPE agentgpu_affinity_total counter
agentgpu_affinity_total{result="hit"} 7
agentgpu_affinity_total{result="miss"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentgpu_affinity_total"); err != nil {
		t.Fatalf("affinity_total mismatch:\n%v", err)
	}
}

// TestCollectorNilSourceEmitsNothing proves a collector over a nil source
// contributes no metrics (rather than erroring the whole scrape).
func TestCollectorNilSourceEmitsNothing(t *testing.T) {
	if n := testutil.CollectAndCount(newServerCollector(nil)); n != 0 {
		t.Fatalf("nil-source collector emitted %d metrics, want 0", n)
	}
}

// TestRegisterServerCollectorNilSafe proves the registration helper is a no-op
// (no error, nothing registered) for a nil *Metrics or nil source.
func TestRegisterServerCollectorNilSafe(t *testing.T) {
	var m *Metrics
	if err := m.RegisterServerCollector(&fakeStatsSource{}); err != nil {
		t.Fatalf("nil *Metrics RegisterServerCollector = %v, want nil", err)
	}
	real := New()
	if err := real.RegisterServerCollector(nil); err != nil {
		t.Fatalf("nil source RegisterServerCollector = %v, want nil", err)
	}
}
