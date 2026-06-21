package httpapi

import (
	"net/http"
	"sync/atomic"
	"time"
)

// Telemetry/metrics view API (#98): GET /v1/admin/telemetry returns, in ONE call,
// the structured stats a monitoring dashboard wants — request rate/latency,
// throttles, fleet health by status, active sessions, and affinity hit/miss — as a
// read-only JSON facade over the SAME in-process collectors the rest of the server
// already maintains. It scrapes/parses nothing from Prometheus: the Prometheus
// /metrics listener and its collectors are entirely unchanged, and GET
// /v1/admin/stats keeps its own (different) shape. This endpoint is the human/JSON
// roll-up the GUI dashboard (#103/#14) renders against.
//
// Every section derives from an existing accessor:
//
//   - requests        — the in-process requestStats accumulator updated inline by
//     metricsMiddleware (the same place ObserveRequest is called); see below.
//   - throttles        — s.RateLimitStats() (the global/per-key throttle counters).
//   - fleet.by_status  — s.fleet.Fleet() folded by Status.String().
//   - fleet.queue      — s.fleet.QueueStats().
//   - fleet.wait_time  — s.fleet.WaitTimeStats().
//   - sessions.active  — s.sessionMgr.ActiveSessions() (0 when sessions disabled).
//   - affinity         — s.fleet.AffinityStats().
//   - uptime_seconds   — wall time since the Server's startedAt.
//
// It is gated to the telemetry:read scope (the same scope as /v1/admin/stats and
// /v1/admin/usage), is a pure read, and is not audited (matching the other admin
// read endpoints).

// requestStats is the in-process request-rate/latency accumulator (#98): a
// lock-free mirror of the Prometheus request_duration histogram, updated inline on
// the hot path by metricsMiddleware and read on demand by the telemetry endpoint.
// It exists because the Prometheus collectors are write-only in-process (there is
// no read API over a CounterVec/HistogramVec), so the JSON dashboard reads this
// rather than scraping Gather().
//
// Every field is updated with sync/atomic (NOT a mutex) so the hot path stays
// lock-free and adds negligible overhead: count and latencySumMs via atomic.Add,
// latencyMaxMs via a compare-and-swap loop, and each latency bucket via an atomic
// counter. A snapshot read (RequestStats) is eventually-consistent — it may observe
// the fields of concurrent updates slightly out of step — which is exactly what a
// monitoring gauge wants.
type requestStats struct {
	count        atomic.Uint64
	latencySumMs atomic.Uint64
	latencyMaxMs atomic.Uint64
	// buckets are cumulative-on-read fixed counters parallel to
	// requestLatencyBucketBounds: bucket i counts requests whose latency was <=
	// requestLatencyBucketBounds[i]. They are stored as per-bound (non-cumulative)
	// counters and made cumulative in the snapshot, mirroring the server's
	// time-in-queue histogram shape so the section reuses the same bucket type. The
	// array length is the const requestLatencyBucketCount (Go requires a constant).
	buckets [requestLatencyBucketCount]atomic.Uint64
}

// requestLatencyBucketCount is the number of finite latency buckets. It pins the
// length of both requestStats.buckets (an array length must be a compile-time
// constant) and requestLatencyBucketBounds below, so the per-bound counter array and
// the bounds it parallels are tied together by construction — a mismatched bounds
// literal is a compile error rather than a silent drift.
const requestLatencyBucketCount = 4

// requestLatencyBucketBounds are the fixed cumulative upper bounds (milliseconds)
// of the request-latency histogram. They reuse the control plane's time-in-queue
// bucket boundaries (server.waitBucketBounds: 10ms/100ms/1s/10s) so the latency and
// wait_time sections of the telemetry response share one bucket vocabulary and one
// bucket type. The implicit +Inf bucket (emitted with le_ms == 0) catches anything
// slower.
var requestLatencyBucketBounds = [requestLatencyBucketCount]uint64{10, 100, 1000, 10000}

// record folds one completed request's latency into the accumulator. It is called
// inline by metricsMiddleware after the handler chain returns, on the same value
// (seconds) already observed by Prometheus, so the two never disagree. A negative
// duration (clock skew) is clamped to zero. All updates are atomic; the max uses a
// CAS loop so a concurrent larger value is never lost.
func (rs *requestStats) record(d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	latMs := uint64(ms)

	rs.count.Add(1)
	rs.latencySumMs.Add(latMs)

	// Raise the running max with a compare-and-swap loop: re-read and retry until we
	// either install our value or observe one already >= ours.
	for {
		cur := rs.latencyMaxMs.Load()
		if latMs <= cur {
			break
		}
		if rs.latencyMaxMs.CompareAndSwap(cur, latMs) {
			break
		}
	}

	// Increment every bucket whose bound is >= this latency (cumulative-on-read is
	// reconstructed in the snapshot from these per-bound counts).
	for i, bound := range requestLatencyBucketBounds {
		if latMs <= bound {
			rs.buckets[i].Add(1)
		}
	}
}

// snapshot returns a point-in-time RequestStats: count, sum/max/mean (ms), and the
// cumulative le-bucketed histogram (trailing +Inf bucket, le_ms == 0). Mean is the
// integer sum/count, 0 when no request has been recorded. Reads are atomic but not
// a single consistent transaction, so a snapshot taken during concurrent updates is
// eventually consistent — acceptable for a monitoring gauge.
func (rs *requestStats) snapshot() RequestStats {
	count := rs.count.Load()
	sumMs := rs.latencySumMs.Load()
	var meanMs uint64
	if count > 0 {
		meanMs = sumMs / count
	}
	buckets := make([]RequestStatsBucket, 0, len(requestLatencyBucketBounds)+1)
	for i, bound := range requestLatencyBucketBounds {
		buckets = append(buckets, RequestStatsBucket{LeMs: bound, Count: rs.buckets[i].Load()})
	}
	// The +Inf bucket holds every recorded request (le_ms == 0 is the sentinel).
	buckets = append(buckets, RequestStatsBucket{LeMs: 0, Count: count})
	return RequestStats{
		Count:   count,
		SumMs:   sumMs,
		MaxMs:   rs.latencyMaxMs.Load(),
		MeanMs:  meanMs,
		Buckets: buckets,
	}
}

// RequestStats is an observable snapshot of HTTP request rate and latency (#98): a
// lock-free in-process mirror of the Prometheus request_duration histogram, exposed
// so the telemetry dashboard can read it without scraping Prometheus. Count is the
// total requests observed; SumMs/MaxMs/MeanMs summarize the latency distribution
// (milliseconds); Buckets is the cumulative le-bucketed histogram (the trailing
// entry, LeMs == 0, is the +Inf bucket). It mirrors server.WaitTimeStats /
// RateLimitStats as the metrics seam, so an operator derives request rate by
// sampling Count against uptime_seconds.
type RequestStats struct {
	Count   uint64
	SumMs   uint64
	MaxMs   uint64
	MeanMs  uint64
	Buckets []RequestStatsBucket
}

// RequestStatsBucket is one cumulative bucket of the request-latency histogram:
// Count is the number of requests whose latency was <= LeMs. A LeMs of 0 is the
// sentinel for the +Inf bucket (it holds every recorded request, so its Count
// equals RequestStats.Count). It mirrors server.WaitBucket.
type RequestStatsBucket struct {
	LeMs  uint64
	Count uint64
}

// RequestStats returns a point-in-time snapshot of the in-process request-rate and
// latency accumulator. It is the read counterpart of the inline updates
// metricsMiddleware performs, and it mirrors the RateLimitStats / AffinityStats /
// WaitTimeStats snapshot idiom. On a Server built via a struct literal without the
// accumulator wired (some unit tests), reqStats() lazily provides a zero one, so
// this always returns a valid (empty) snapshot rather than panicking.
func (s *Server) RequestStats() RequestStats {
	return s.reqStats().snapshot()
}

// recordRequest folds one completed request's latency into the in-process
// accumulator. It is called inline by metricsMiddleware alongside (and additive to)
// s.metrics.ObserveRequest, on the same measured duration, so this mirror never
// disagrees with the Prometheus histogram. It is lock-free (atomics only); the
// accumulator is lazily constructed via reqStats() so a struct-literal Server is
// safe.
func (s *Server) recordRequest(d time.Duration) {
	s.reqStats().record(d)
}

// reqStats returns the request-stats accumulator, lazily constructing it once for a
// Server built via a struct literal (some unit tests) so neither the hot-path
// update nor a snapshot read ever dereferences a nil pointer. NewServer sets the
// field up front, so the sync.Once is uncontended in production.
func (s *Server) reqStats() *requestStats {
	s.requestStatsOnce.Do(func() {
		if s.requestStats == nil {
			s.requestStats = &requestStats{}
		}
	})
	return s.requestStats
}

// adminTelemetryResponse is the GET /v1/admin/telemetry response (#98): the
// dashboard summary in one document — request rate/latency, throttles, fleet health
// (status breakdown, queue depth, wait-time distribution), active session count,
// affinity hit/miss/rebind, and process uptime. It is a live read (no caching) and
// contains no secrets. It is DISTINCT from /v1/admin/stats (which keeps its own
// queue/worker/wait_time shape, unchanged); this one rolls up every dashboard
// signal so a GUI fetches it in a single call.
type adminTelemetryResponse struct {
	Requests      telemetryRequests  `json:"requests"`
	Throttles     telemetryThrottles `json:"throttles"`
	Fleet         telemetryFleet     `json:"fleet"`
	Sessions      telemetrySessions  `json:"sessions"`
	Affinity      telemetryAffinity  `json:"affinity"`
	UptimeSeconds int64              `json:"uptime_seconds"`
}

// telemetryRequests is the request-rate/latency section: the total request count
// and the latency distribution (the in-process mirror of the Prometheus request
// histogram). A dashboard derives request rate by sampling count against
// uptime_seconds across two polls.
type telemetryRequests struct {
	Count   uint64           `json:"count"`
	Latency telemetryLatency `json:"latency"`
}

// telemetryLatency is the request-latency distribution: sum/max/mean (milliseconds)
// plus the cumulative le-bucketed histogram. It reuses adminWaitBucket as the bucket
// type so the latency and wait_time sections share one shape.
type telemetryLatency struct {
	SumMs   uint64            `json:"sum_ms"`
	MaxMs   uint64            `json:"max_ms"`
	MeanMs  uint64            `json:"mean_ms"`
	Buckets []adminWaitBucket `json:"buckets"`
}

// telemetryThrottles is the throttle section: the cumulative count of requests
// rejected by the server-wide (global) limiter and by per-key quota. It is the same
// pair RateLimitStats exposes and the usage summary reports.
type telemetryThrottles struct {
	Global uint64 `json:"global"`
	Key    uint64 `json:"key"`
}

// telemetryFleet is the fleet-health section: the worker count, a breakdown of
// workers by lifecycle status (online/draining/stale), the queue depth (total plus
// per-priority), and the time-in-queue distribution. The by_status map carries only
// statuses with at least one worker, keyed by the status string.
type telemetryFleet struct {
	WorkerCount int             `json:"worker_count"`
	ByStatus    map[string]int  `json:"by_status"`
	Queue       adminQueueStats `json:"queue"`
	WaitTime    adminWaitTime   `json:"wait_time"`
}

// telemetrySessions is the sessions section: the count of live (not-yet-expired)
// sessions. It is 0 when sessions are disabled (no session manager wired).
type telemetrySessions struct {
	Active int `json:"active"`
}

// telemetryAffinity is the session-affinity section: hits (turns that reused a
// session's warm worker), misses (turns that rebound), and rebinds. It is the same
// triple server.AffinityStats exposes.
type telemetryAffinity struct {
	Hits    uint64 `json:"hits"`
	Misses  uint64 `json:"misses"`
	Rebinds uint64 `json:"rebinds"`
}

// handleAdminTelemetry serves GET /v1/admin/telemetry (#98). It assembles the
// dashboard summary in one pass from the existing in-process collectors — the
// request-stats accumulator, the throttle counters, the fleet snapshot (folded by
// status), the queue and wait-time stats, the affinity counters, and the live
// session count — plus the process uptime. It performs NO new probing and starts NO
// background poller: every value is read on demand from state the server already
// maintains, and the Prometheus /metrics surface is untouched. The session count is
// nil-guarded (0 when sessions are disabled). Gated to the telemetry:read scope
// (s.requireScope), so a key lacking it gets 403 and an unauthenticated request 401
// before this runs. This is a pure read and is not audited.
func (s *Server) handleAdminTelemetry(w http.ResponseWriter, r *http.Request) {
	// Requests: the in-process latency mirror. Reuse adminWaitBucket for the buckets.
	rstat := s.RequestStats()
	latBuckets := make([]adminWaitBucket, len(rstat.Buckets))
	for i, b := range rstat.Buckets {
		latBuckets[i] = adminWaitBucket{LeMs: b.LeMs, Count: b.Count}
	}

	// Throttles: the same global/per-key counters RateLimitStats exposes.
	rl := s.RateLimitStats()

	// Fleet: fold the snapshot by status, and read the queue + wait-time stats.
	fleet := s.fleet.Fleet()
	byStatus := make(map[string]int, 3)
	for _, wk := range fleet {
		byStatus[wk.Status.String()]++
	}

	qs := s.fleet.QueueStats()
	byPriority := make(map[string]int, len(qs.ByPriority))
	for p, n := range qs.ByPriority {
		byPriority[priorityName(p)] += n
	}

	wt := s.fleet.WaitTimeStats()
	var waitMeanMs uint64
	if wt.Count > 0 {
		waitMeanMs = wt.SumMs / wt.Count
	}
	waitBuckets := make([]adminWaitBucket, len(wt.Buckets))
	for i, b := range wt.Buckets {
		waitBuckets[i] = adminWaitBucket{LeMs: b.LeMs, Count: b.Count}
	}

	// Affinity: the same hit/miss/rebind triple AffinityStats exposes.
	aff := s.fleet.AffinityStats()

	// Sessions: live count, nil-guarded so a Server with sessions disabled reports 0
	// rather than panicking on a nil manager.
	activeSessions := 0
	if s.sessionMgr != nil {
		activeSessions = s.sessionMgr.ActiveSessions()
	}

	writeJSON(w, http.StatusOK, adminTelemetryResponse{
		Requests: telemetryRequests{
			Count: rstat.Count,
			Latency: telemetryLatency{
				SumMs:   rstat.SumMs,
				MaxMs:   rstat.MaxMs,
				MeanMs:  rstat.MeanMs,
				Buckets: latBuckets,
			},
		},
		Throttles: telemetryThrottles{
			Global: rl.GlobalThrottled,
			Key:    rl.KeyThrottled,
		},
		Fleet: telemetryFleet{
			WorkerCount: len(fleet),
			ByStatus:    byStatus,
			Queue:       adminQueueStats{Total: qs.Total, ByPriority: byPriority},
			WaitTime: adminWaitTime{
				Count:   wt.Count,
				SumMs:   wt.SumMs,
				MaxMs:   wt.MaxMs,
				MeanMs:  waitMeanMs,
				Buckets: waitBuckets,
			},
		},
		Sessions: telemetrySessions{Active: activeSessions},
		Affinity: telemetryAffinity{
			Hits:    aff.Hits,
			Misses:  aff.Misses,
			Rebinds: aff.Rebinds,
		},
		UptimeSeconds: s.uptimeSeconds(),
	})
}

// uptimeSeconds returns the whole seconds the Server has been running, computed
// against its injectable clock (nowFunc) so a test can drive it deterministically.
// It is clamped at 0 (a zero/unset startedAt, e.g. a struct-literal Server in a
// unit test, yields 0 rather than a garbage or negative value). The dashboard uses
// it to turn the monotonic requests.count into a rate.
func (s *Server) uptimeSeconds() int64 {
	if s.startedAt.IsZero() {
		return 0
	}
	now := time.Now
	if s.nowFunc != nil {
		now = s.nowFunc
	}
	d := now().Sub(s.startedAt)
	if d <= 0 {
		return 0
	}
	return int64(d.Seconds())
}
