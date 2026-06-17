package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// StatsSource is the read-only slice of the control-plane server the live
// collector scrapes: the point-in-time fleet snapshot plus the queue-depth,
// time-in-queue, and affinity counters the server already exposes for
// observability (#10/#34). Narrowing to an interface keeps the collector
// unit-testable with a fake source and documents the only coupling between this
// package and the control plane. *server.Server satisfies it.
type StatsSource interface {
	Fleet() []types.Worker
	QueueStats() queue.Stats
	WaitTimeStats() server.WaitTimeStats
	AffinityStats() server.AffinityStats
}

// queuePriorities is the fixed enumeration of scheduling priorities, with the
// stable label each maps to. QueueStats.ByPriority only carries the priorities
// that currently have queued jobs, so the collector iterates this fixed set and
// emits an explicit 0 for the empty ones — the series for every priority is then
// always present, which a dashboard/alert can rely on rather than seeing a
// label vanish when its queue drains.
var queuePriorities = []struct {
	p     queue.Priority
	label string
}{
	{queue.PriorityLow, "low"},
	{queue.PriorityNormal, "normal"},
	{queue.PriorityHigh, "high"},
}

// serverCollector is a prometheus.Collector that reads the control-plane server's
// in-memory snapshots at scrape time and emits them as gauges (and a const
// histogram for the time-in-queue distribution). Collecting at scrape time — as
// opposed to mirroring into long-lived gauge objects from a background poller —
// keeps the exported values exactly consistent with the server's own
// QueueStats/Fleet/WaitTimeStats/AffinityStats and needs no extra goroutine; the
// reads are cheap in-memory snapshots.
type serverCollector struct {
	src StatsSource

	queueDepth        *prometheus.Desc
	queueWaitSeconds  *prometheus.Desc
	workerGPUUtil     *prometheus.Desc
	workerVRAMTotal   *prometheus.Desc
	workerVRAMFree    *prometheus.Desc
	workerActiveJobs  *prometheus.Desc
	workerStartTime   *prometheus.Desc
	affinityTotal     *prometheus.Desc
	fleetWorkersTotal *prometheus.Desc
}

// newServerCollector builds the collector and its metric descriptors. All names
// are namespaced agentgpu_* to match the request-path collectors.
func newServerCollector(src StatsSource) *serverCollector {
	fq := func(name string) string { return namespace + "_" + name }
	return &serverCollector{
		src: src,
		queueDepth: prometheus.NewDesc(
			fq("queue_depth"),
			"Number of jobs currently waiting in the scheduling queue, by priority.",
			[]string{"priority"}, nil,
		),
		queueWaitSeconds: prometheus.NewDesc(
			fq("queue_wait_seconds"),
			"Time jobs spent queued before being placed on a worker (placement path only), in seconds.",
			nil, nil,
		),
		workerGPUUtil: prometheus.NewDesc(
			fq("worker_gpu_utilization"),
			"Reported mean GPU utilization of a worker, 0-100.",
			[]string{"worker", "gpu_type"}, nil,
		),
		workerVRAMTotal: prometheus.NewDesc(
			fq("worker_vram_bytes"),
			"Total video memory reported by a worker, in bytes.",
			[]string{"worker"}, nil,
		),
		workerVRAMFree: prometheus.NewDesc(
			fq("worker_vram_free_bytes"),
			"Currently-free video memory reported by a worker, in bytes.",
			[]string{"worker"}, nil,
		),
		workerActiveJobs: prometheus.NewDesc(
			fq("worker_active_jobs"),
			"Number of jobs currently in flight on a worker.",
			[]string{"worker"}, nil,
		),
		workerStartTime: prometheus.NewDesc(
			fq("worker_start_time_seconds"),
			"Unix timestamp at which a worker registered with the server. Compute uptime as time() - this (resets on reconnect).",
			[]string{"worker"}, nil,
		),
		affinityTotal: prometheus.NewDesc(
			fq("affinity_total"),
			"Session-affinity routing outcomes since startup, by result (hit|miss).",
			[]string{"result"}, nil,
		),
		fleetWorkersTotal: prometheus.NewDesc(
			fq("fleet_workers"),
			"Number of workers currently connected to the fleet, by status (online|draining|stale).",
			[]string{"status"}, nil,
		),
	}
}

// Describe sends every descriptor this collector may emit. It is implemented
// (rather than relying on unchecked collection) so the registry can detect a
// descriptor clash at registration time.
func (c *serverCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queueDepth
	ch <- c.queueWaitSeconds
	ch <- c.workerGPUUtil
	ch <- c.workerVRAMTotal
	ch <- c.workerVRAMFree
	ch <- c.workerActiveJobs
	ch <- c.workerStartTime
	ch <- c.affinityTotal
	ch <- c.fleetWorkersTotal
}

// Collect reads the server's current snapshots and emits one metric family per
// descriptor. It is invoked by the registry on every scrape, so the values are
// always live; a nil source yields no metrics (the collector contributes nothing
// rather than erroring the whole scrape).
func (c *serverCollector) Collect(ch chan<- prometheus.Metric) {
	if c.src == nil {
		return
	}

	c.collectQueue(ch)
	c.collectWait(ch)
	c.collectFleet(ch)
	c.collectAffinity(ch)
}

// collectQueue emits queue_depth for every priority (an explicit 0 for the
// empty ones — see queuePriorities).
func (c *serverCollector) collectQueue(ch chan<- prometheus.Metric) {
	stats := c.src.QueueStats()
	for _, qp := range queuePriorities {
		ch <- prometheus.MustNewConstMetric(
			c.queueDepth, prometheus.GaugeValue, float64(stats.ByPriority[qp.p]), qp.label,
		)
	}
}

// collectWait emits the time-in-queue distribution as a const histogram built
// from the server's cumulative ms buckets, converted to seconds (Prometheus
// convention). The server's WaitTimeStats carries cumulative le-buckets with a
// trailing +Inf sentinel (LeMs == 0); MustNewConstHistogram wants the finite
// buckets keyed by their upper bound, so the +Inf entry is dropped here (it is
// implicit in the histogram's sample count).
func (c *serverCollector) collectWait(ch chan<- prometheus.Metric) {
	w := c.src.WaitTimeStats()
	buckets := make(map[float64]uint64, len(w.Buckets))
	for _, b := range w.Buckets {
		if b.LeMs == 0 {
			// The +Inf sentinel; its count is the total sample count, supplied
			// separately to MustNewConstHistogram.
			continue
		}
		buckets[msToSeconds(b.LeMs)] = b.Count
	}
	ch <- prometheus.MustNewConstHistogram(
		c.queueWaitSeconds,
		w.Count,
		msToSeconds(w.SumMs),
		buckets,
	)
}

// collectFleet emits the per-worker gauges and the per-status worker counts from
// the fleet snapshot. GPU utilization is the worker's reported Load (0-100, the
// mean device utilization per gpu.Capacity). worker_start_time_seconds is the
// registration timestamp so dashboards derive uptime as time() - start_time.
func (c *serverCollector) collectFleet(ch chan<- prometheus.Metric) {
	fleet := c.src.Fleet()
	statusCounts := map[string]int{}
	for _, w := range fleet {
		statusCounts[w.Status.String()]++

		ch <- prometheus.MustNewConstMetric(
			c.workerGPUUtil, prometheus.GaugeValue, float64(w.Load), w.ID, w.GPUType,
		)
		ch <- prometheus.MustNewConstMetric(
			c.workerVRAMTotal, prometheus.GaugeValue, float64(w.TotalVRAM), w.ID,
		)
		ch <- prometheus.MustNewConstMetric(
			c.workerVRAMFree, prometheus.GaugeValue, float64(w.FreeVRAM), w.ID,
		)
		ch <- prometheus.MustNewConstMetric(
			c.workerActiveJobs, prometheus.GaugeValue, float64(w.ActiveJobs), w.ID,
		)
		// Only emit a start time once the worker has a registration timestamp; a
		// zero time (a snapshot from a path that did not stamp it) is skipped so a
		// bogus 1970 epoch never shows up as a worker that has been up for decades.
		if !w.RegisteredAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				c.workerStartTime, prometheus.GaugeValue, float64(w.RegisteredAt.Unix()), w.ID,
			)
		}
	}
	// Emit a count for every known status so the series are always present, even
	// at zero, mirroring the fixed-priority queue_depth treatment.
	for _, st := range []types.WorkerStatus{types.WorkerOnline, types.WorkerDraining, types.WorkerStale} {
		ch <- prometheus.MustNewConstMetric(
			c.fleetWorkersTotal, prometheus.GaugeValue, float64(statusCounts[st.String()]), st.String(),
		)
	}
}

// collectAffinity emits the affinity hit/miss counters. They are monotonic
// since startup, so they are exported as counter values keyed by result; a
// dashboard computes the hit ratio with rate() over both series.
func (c *serverCollector) collectAffinity(ch chan<- prometheus.Metric) {
	a := c.src.AffinityStats()
	ch <- prometheus.MustNewConstMetric(c.affinityTotal, prometheus.CounterValue, float64(a.Hits), "hit")
	ch <- prometheus.MustNewConstMetric(c.affinityTotal, prometheus.CounterValue, float64(a.Misses), "miss")
}

// msToSeconds converts a millisecond count to seconds for the Prometheus-native
// seconds unit.
func msToSeconds(ms uint64) float64 { return float64(ms) / 1000.0 }

// RegisterServerCollector registers a live collector over src on this
// instrument's registry, so the next scrape reflects the server's current queue
// depth, fleet, time-in-queue distribution, and affinity counters. It is called
// once at startup after the control-plane server is built (the server is passed
// as the StatsSource). It returns an error if registration fails (e.g. a
// descriptor clash) and is a no-op returning nil for a nil *Metrics or nil src.
func (m *Metrics) RegisterServerCollector(src StatsSource) error {
	if m == nil || src == nil {
		return nil
	}
	return m.reg.Register(newServerCollector(src))
}
