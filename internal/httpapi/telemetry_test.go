package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// telemetryResponse mirrors the GET /v1/admin/telemetry wire shape for decoding in
// tests. It is local to the test so a drift in the handler's field tags is caught
// here.
type telemetryResponse struct {
	Requests struct {
		Count   uint64 `json:"count"`
		Latency struct {
			SumMs   uint64 `json:"sum_ms"`
			MaxMs   uint64 `json:"max_ms"`
			MeanMs  uint64 `json:"mean_ms"`
			Buckets []struct {
				LeMs  uint64 `json:"le_ms"`
				Count uint64 `json:"count"`
			} `json:"buckets"`
		} `json:"latency"`
	} `json:"requests"`
	Throttles struct {
		Global uint64 `json:"global"`
		Key    uint64 `json:"key"`
	} `json:"throttles"`
	Fleet struct {
		WorkerCount int            `json:"worker_count"`
		ByStatus    map[string]int `json:"by_status"`
		Queue       struct {
			Total      int            `json:"total"`
			ByPriority map[string]int `json:"by_priority"`
		} `json:"queue"`
		WaitTime struct {
			Count   uint64 `json:"count"`
			SumMs   uint64 `json:"sum_ms"`
			MaxMs   uint64 `json:"max_ms"`
			MeanMs  uint64 `json:"mean_ms"`
			Buckets []struct {
				LeMs  uint64 `json:"le_ms"`
				Count uint64 `json:"count"`
			} `json:"buckets"`
		} `json:"wait_time"`
	} `json:"fleet"`
	Sessions struct {
		Active int `json:"active"`
	} `json:"sessions"`
	Affinity struct {
		Hits    uint64 `json:"hits"`
		Misses  uint64 `json:"misses"`
		Rebinds uint64 `json:"rebinds"`
	} `json:"affinity"`
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// TestRequestStatsAccumulator unit-tests the lock-free accumulator directly:
// count/sum/max/mean and the cumulative le-bucketed histogram (with its +Inf
// trailing bucket) over a set of recorded latencies, plus the empty-accumulator
// zero case (mean must not divide by zero).
func TestRequestStatsAccumulator(t *testing.T) {
	t.Parallel()

	// Empty: every field zero, mean guarded, +Inf bucket present with count 0.
	empty := (&requestStats{}).snapshot()
	if empty.Count != 0 || empty.SumMs != 0 || empty.MaxMs != 0 || empty.MeanMs != 0 {
		t.Fatalf("empty snapshot = %+v, want all zero", empty)
	}
	if n := len(empty.Buckets); n != len(requestLatencyBucketBounds)+1 {
		t.Fatalf("empty buckets len = %d, want %d", n, len(requestLatencyBucketBounds)+1)
	}
	if last := empty.Buckets[len(empty.Buckets)-1]; last.LeMs != 0 || last.Count != 0 {
		t.Errorf("empty +Inf bucket = %+v, want {le_ms:0 count:0}", last)
	}

	// Record latencies spanning the bounds (10/100/1000/10000 ms). Durations chosen
	// so each lands in a distinct bucket: 5ms, 50ms, 500ms, 5000ms, and 50000ms
	// (the last only in +Inf).
	rs := &requestStats{}
	for _, d := range []time.Duration{
		5 * time.Millisecond,
		50 * time.Millisecond,
		500 * time.Millisecond,
		5000 * time.Millisecond,
		50000 * time.Millisecond,
	} {
		rs.record(d)
	}
	got := rs.snapshot()

	if got.Count != 5 {
		t.Errorf("count = %d, want 5", got.Count)
	}
	// Sum = 5 + 50 + 500 + 5000 + 50000 = 55555 ms.
	if got.SumMs != 55555 {
		t.Errorf("sum_ms = %d, want 55555", got.SumMs)
	}
	if got.MaxMs != 50000 {
		t.Errorf("max_ms = %d, want 50000", got.MaxMs)
	}
	// Mean = 55555 / 5 = 11111 ms (integer division).
	if got.MeanMs != 11111 {
		t.Errorf("mean_ms = %d, want 11111", got.MeanMs)
	}

	// Cumulative buckets: <=10 -> {5ms}=1; <=100 -> {5,50}=2; <=1000 -> {5,50,500}=3;
	// <=10000 -> {5,50,500,5000}=4; +Inf -> all 5.
	wantCum := []struct {
		leMs  uint64
		count uint64
	}{
		{10, 1},
		{100, 2},
		{1000, 3},
		{10000, 4},
		{0, 5},
	}
	if len(got.Buckets) != len(wantCum) {
		t.Fatalf("buckets len = %d, want %d (%+v)", len(got.Buckets), len(wantCum), got.Buckets)
	}
	for i, w := range wantCum {
		if got.Buckets[i].LeMs != w.leMs || got.Buckets[i].Count != w.count {
			t.Errorf("bucket[%d] = %+v, want {le_ms:%d count:%d}", i, got.Buckets[i], w.leMs, w.count)
		}
	}

	// A negative duration (clock skew) is clamped to 0, lands in every bucket, and
	// does not lower the max.
	rs.record(-1 * time.Second)
	skewed := rs.snapshot()
	if skewed.Count != 6 || skewed.MaxMs != 50000 {
		t.Errorf("after skew: count=%d max=%d, want 6 and 50000", skewed.Count, skewed.MaxMs)
	}
	if skewed.Buckets[0].Count != 2 { // <=10ms now holds the 5ms and the clamped-0.
		t.Errorf("after skew bucket[0].count = %d, want 2", skewed.Buckets[0].Count)
	}
}

// TestRequestStatsConcurrent drives the accumulator from many goroutines so the
// atomics (count add, sum add, max CAS, bucket adds) are exercised under contention.
// It is meaningful under `go test -race`: the test fails the race detector if any
// update is not atomic. The total count and sum are exact (Add is exact); the max
// must equal the largest latency recorded.
func TestRequestStatsConcurrent(t *testing.T) {
	t.Parallel()

	rs := &requestStats{}
	const goroutines = 16
	const perG = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				// All in the 1ms bucket so the per-bucket counter is also contended;
				// one goroutine additionally records a large latency to drive the max.
				rs.record(time.Millisecond)
			}
			if g == 0 {
				rs.record(9 * time.Second) // 9000ms, the global max.
			}
		}(g)
	}
	wg.Wait()

	got := rs.snapshot()
	wantCount := uint64(goroutines*perG) + 1
	if got.Count != wantCount {
		t.Errorf("count = %d, want %d (lost updates under contention)", got.Count, wantCount)
	}
	// Sum = goroutines*perG*1ms + 9000ms.
	wantSum := uint64(goroutines*perG) + 9000
	if got.SumMs != wantSum {
		t.Errorf("sum_ms = %d, want %d", got.SumMs, wantSum)
	}
	if got.MaxMs != 9000 {
		t.Errorf("max_ms = %d, want 9000 (CAS lost the max)", got.MaxMs)
	}
	// The +Inf bucket must hold every record.
	if last := got.Buckets[len(got.Buckets)-1]; last.Count != wantCount {
		t.Errorf("+Inf bucket count = %d, want %d", last.Count, wantCount)
	}
}

// TestTelemetryAllSections proves AC1/AC3: GET /v1/admin/telemetry returns every
// section in one call, with values sourced from the existing in-process collectors.
// It drives real requests through the routed handler so requests.count increments
// from the metricsMiddleware accumulator (not a fabricated value), and seeds the
// fake fleet's queue/wait/affinity stats plus the server's throttle counters.
func TestTelemetryAllSections(t *testing.T) {
	fleet := &fakeFleet{
		snapshot: []types.Worker{
			{ID: "w1", Status: types.WorkerOnline},
			{ID: "w2", Status: types.WorkerDraining},
			{ID: "w3", Status: types.WorkerOnline},
		},
		queueStats: queue.Stats{
			Total:      3,
			ByPriority: map[queue.Priority]int{queue.PriorityNormal: 2, queue.PriorityHigh: 1},
		},
		waitStats: server.WaitTimeStats{
			Count: 4, SumMs: 800, MaxMs: 500,
			Buckets: []server.WaitBucket{
				{LeMs: 100, Count: 1},
				{LeMs: 1000, Count: 4},
				{LeMs: 0, Count: 4},
			},
		},
		affinityStats: server.AffinityStats{Hits: 42, Misses: 3, Rebinds: 3},
	}
	s, authSvc := adminTestServer(t, fleet)
	// Seed throttle counters via the public increment seam (mirrors the rate-limit
	// rejection sites) so the throttles section has non-zero values to surface.
	s.incGlobalThrottled()
	s.incGlobalThrottled()
	s.incKeyThrottled()

	token := mustKey(t, authSvc, adminPerms())

	// Drive a few requests through the routed handler so the in-process request
	// accumulator increments. Each authenticated GET below flows through
	// metricsMiddleware (the outermost wrapper), which calls recordRequest.
	const priming = 3
	for i := 0; i < priming; i++ {
		if rec := do(t, s, "/v1/admin/stats", token); rec.Code != http.StatusOK {
			t.Fatalf("priming request %d status = %d", i, rec.Code)
		}
	}

	rec := do(t, s, "/v1/admin/telemetry", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry status = %d, body %q", rec.Code, rec.Body.String())
	}
	var got telemetryResponse
	decode(t, rec, &got)

	// requests.count: the 3 priming requests plus the telemetry request itself is in
	// flight (recorded after the handler returns), so at least the 3 priming ones are
	// counted. Assert it reflects real traffic (>= priming) rather than a fixed value.
	if got.Requests.Count < priming {
		t.Errorf("requests.count = %d, want >= %d (accumulator did not see priming traffic)",
			got.Requests.Count, priming)
	}
	// The +Inf latency bucket count must equal the total request count.
	if last := got.Requests.Latency.Buckets[len(got.Requests.Latency.Buckets)-1]; last.LeMs != 0 || last.Count != got.Requests.Count {
		t.Errorf("latency +Inf bucket = %+v, want {le_ms:0 count:%d}", last, got.Requests.Count)
	}

	// throttles: from the seeded counters.
	if got.Throttles.Global != 2 || got.Throttles.Key != 1 {
		t.Errorf("throttles = %+v, want {global:2 key:1}", got.Throttles)
	}

	// fleet.by_status: 2 online, 1 draining.
	if got.Fleet.WorkerCount != 3 {
		t.Errorf("fleet.worker_count = %d, want 3", got.Fleet.WorkerCount)
	}
	if got.Fleet.ByStatus["online"] != 2 || got.Fleet.ByStatus["draining"] != 1 {
		t.Errorf("fleet.by_status = %+v, want online:2 draining:1", got.Fleet.ByStatus)
	}
	if _, ok := got.Fleet.ByStatus["stale"]; ok {
		t.Errorf("fleet.by_status should omit zero statuses, got %+v", got.Fleet.ByStatus)
	}

	// fleet.queue: total + by_priority names.
	if got.Fleet.Queue.Total != 3 || got.Fleet.Queue.ByPriority["normal"] != 2 || got.Fleet.Queue.ByPriority["high"] != 1 {
		t.Errorf("fleet.queue = %+v, want total:3 normal:2 high:1", got.Fleet.Queue)
	}

	// fleet.wait_time: count/sum/max plus derived mean (800/4 = 200) and buckets.
	if got.Fleet.WaitTime.Count != 4 || got.Fleet.WaitTime.SumMs != 800 || got.Fleet.WaitTime.MaxMs != 500 {
		t.Errorf("fleet.wait_time core = %+v, want count:4 sum:800 max:500", got.Fleet.WaitTime)
	}
	if got.Fleet.WaitTime.MeanMs != 200 {
		t.Errorf("fleet.wait_time.mean_ms = %d, want 200", got.Fleet.WaitTime.MeanMs)
	}
	if n := len(got.Fleet.WaitTime.Buckets); n != 3 {
		t.Errorf("fleet.wait_time buckets len = %d, want 3", n)
	}

	// affinity: surfaced verbatim.
	if got.Affinity.Hits != 42 || got.Affinity.Misses != 3 || got.Affinity.Rebinds != 3 {
		t.Errorf("affinity = %+v, want hits:42 misses:3 rebinds:3", got.Affinity)
	}

	// sessions: 0 here (adminTestServer wires no session manager).
	if got.Sessions.Active != 0 {
		t.Errorf("sessions.active = %d, want 0 (no manager wired)", got.Sessions.Active)
	}
}

// TestTelemetryFleetByStatus is the table-driven proof of the fleet-by-status
// aggregation: for a range of fleet snapshots, by_status carries the correct count
// per lifecycle status (online/draining/stale) and omits statuses with no workers.
func TestTelemetryFleetByStatus(t *testing.T) {
	cases := []struct {
		name    string
		workers []types.Worker
		want    map[string]int
	}{
		{name: "empty fleet", workers: nil, want: map[string]int{}},
		{
			name: "mixed statuses",
			workers: []types.Worker{
				{ID: "a", Status: types.WorkerOnline},
				{ID: "b", Status: types.WorkerOnline},
				{ID: "c", Status: types.WorkerDraining},
				{ID: "d", Status: types.WorkerStale},
				{ID: "e", Status: types.WorkerStale},
				{ID: "f", Status: types.WorkerStale},
			},
			want: map[string]int{"online": 2, "draining": 1, "stale": 3},
		},
		{
			name:    "all online",
			workers: []types.Worker{{ID: "a", Status: types.WorkerOnline}, {ID: "b", Status: types.WorkerOnline}},
			want:    map[string]int{"online": 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fleet := &fakeFleet{snapshot: tc.workers}
			s, authSvc := adminTestServer(t, fleet)
			token := mustKey(t, authSvc, adminPerms())

			rec := do(t, s, "/v1/admin/telemetry", token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			var got telemetryResponse
			decode(t, rec, &got)

			if got.Fleet.WorkerCount != len(tc.workers) {
				t.Errorf("worker_count = %d, want %d", got.Fleet.WorkerCount, len(tc.workers))
			}
			if len(got.Fleet.ByStatus) != len(tc.want) {
				t.Fatalf("by_status = %+v, want %+v", got.Fleet.ByStatus, tc.want)
			}
			for k, v := range tc.want {
				if got.Fleet.ByStatus[k] != v {
					t.Errorf("by_status[%q] = %d, want %d", k, got.Fleet.ByStatus[k], v)
				}
			}
		})
	}
}

// TestTelemetrySessionsActive proves the sessions section reflects the live session
// count when a manager is wired (and the nil-guard 0 is proven by
// TestTelemetryAllSections, which wires no manager).
func TestTelemetrySessionsActive(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(logger))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(0, 0),
		session.WithLogger(logger),
		session.WithTTL(time.Hour),
	)
	t.Cleanup(func() { _ = mgr.Close() })

	s := &Server{
		fleet:      &fakeFleet{},
		auth:       authSvc,
		authz:      az,
		sessionMgr: mgr,
		log:        logger,
	}
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin", adminPerms())
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Create two live sessions for two owners.
	if _, err := mgr.Create(context.Background(), "owner-a", "llama3"); err != nil {
		t.Fatalf("create session a: %v", err)
	}
	if _, err := mgr.Create(context.Background(), "owner-b", "llama3"); err != nil {
		t.Fatalf("create session b: %v", err)
	}

	rec := do(t, s, "/v1/admin/telemetry", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var got telemetryResponse
	decode(t, rec, &got)
	if got.Sessions.Active != 2 {
		t.Errorf("sessions.active = %d, want 2", got.Sessions.Active)
	}
}

// TestTelemetryUptime proves uptime_seconds derives from the Server start time via
// the injectable clock, so it is a positive, deterministic value rather than a
// hardcoded zero. A struct-literal Server (no startedAt) reports 0 (the guard).
func TestTelemetryUptime(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())

	// adminTestServer builds via a struct literal (no startedAt), so uptime is 0.
	rec := do(t, s, "/v1/admin/telemetry", token)
	var zero telemetryResponse
	decode(t, rec, &zero)
	if zero.UptimeSeconds != 0 {
		t.Errorf("struct-literal uptime_seconds = %d, want 0", zero.UptimeSeconds)
	}

	// With a startedAt 90s in the past and a fixed clock, uptime is exactly 90.
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.startedAt = base
	s.nowFunc = func() time.Time { return base.Add(90 * time.Second) }
	rec = do(t, s, "/v1/admin/telemetry", token)
	var got telemetryResponse
	decode(t, rec, &got)
	if got.UptimeSeconds != 90 {
		t.Errorf("uptime_seconds = %d, want 90", got.UptimeSeconds)
	}
}

// TestTelemetryScopeGate pins the 200/403/401 contract for the route specifically
// (the matrix in TestScopedKeyMatrix covers it alongside the others; this isolates
// the gate): a telemetry:read key gets 200, a different-scope key 403, and an
// unauthenticated request 401.
func TestTelemetryScopeGate(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	telemetryReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeTelemetryRead}})
	keysReader := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})

	if rec := do(t, s, "/v1/admin/telemetry", telemetryReader); rec.Code != http.StatusOK {
		t.Errorf("telemetry:read key status = %d, want 200", rec.Code)
	}
	rec := do(t, s, "/v1/admin/telemetry", keysReader)
	if rec.Code != http.StatusForbidden {
		t.Errorf("keys:read key status = %d, want 403", rec.Code)
	}
	if code := errorCode(t, rec); code != "forbidden" {
		t.Errorf("403 error code = %q, want forbidden", code)
	}
	if rec := do(t, s, "/v1/admin/telemetry", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
}

// TestTelemetryDoesNotTouchMetrics is a guard for AC2: recording into the
// in-process request accumulator (what metricsMiddleware now also does) is additive
// and independent of the Prometheus instrument — a Server with metrics disabled
// (nil *metrics.Metrics) still records into the accumulator and serves telemetry,
// and a Server with metrics enabled records into both without the accumulator
// disturbing the Prometheus path. Here we assert the accumulator works with metrics
// nil (the struct-literal adminTestServer leaves s.metrics nil).
func TestTelemetryDoesNotTouchMetrics(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	if s.metrics != nil {
		t.Fatal("precondition: adminTestServer should leave metrics nil")
	}
	token := mustKey(t, authSvc, adminPerms())

	// Drive a request; the accumulator must increment even though Prometheus is off.
	if rec := do(t, s, "/v1/admin/stats", token); rec.Code != http.StatusOK {
		t.Fatalf("priming status = %d", rec.Code)
	}
	if got := s.RequestStats(); got.Count == 0 {
		t.Error("request accumulator did not record with metrics disabled")
	}

	rec := do(t, s, "/v1/admin/telemetry", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry status = %d", rec.Code)
	}
}

// recorderUnwrap is a tiny sanity check that the statusRecorder still forwards Flush
// after the metrics-middleware change (the accumulator addition must not have
// altered the SSE-critical wrapper). It is a compile+behavior guard kept next to the
// change.
func TestStatusRecorderStillFlushes(t *testing.T) {
	var flushed bool
	rec := &statusRecorder{ResponseWriter: flusherWriter{rw: httptest.NewRecorder(), onFlush: func() { flushed = true }}}
	rec.Flush()
	if !flushed {
		t.Error("statusRecorder.Flush did not reach the underlying Flusher")
	}
}

// flusherWriter is an http.ResponseWriter+Flusher whose Flush records that it ran.
type flusherWriter struct {
	rw      http.ResponseWriter
	onFlush func()
}

func (f flusherWriter) Header() http.Header         { return f.rw.Header() }
func (f flusherWriter) Write(b []byte) (int, error) { return f.rw.Write(b) }
func (f flusherWriter) WriteHeader(code int)        { f.rw.WriteHeader(code) }
func (f flusherWriter) Flush()                      { f.onFlush() }
