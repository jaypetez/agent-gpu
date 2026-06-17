package server_test

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/jaypetez/agent-gpu/internal/metrics"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestAffinityMetricsRiseUnderWorkerLoss is the metrics-facing AC test for #38:
// it stands up the live Prometheus collector over a real control-plane server,
// drives a session through an affinity HIT and then a forced rebind after the
// bound worker is lost, and asserts the SCRAPED exposition reflects the change —
// agentgpu_affinity_total{result="miss"} and agentgpu_session_rebinds_total both
// go from 0 to 1, while the hit series stays at 1. This proves the affinity
// hit-rate and the rebind signal are visible on /metrics and change as expected
// under worker loss, end to end through the collector (not just AffinityStats).
func TestAffinityMetricsRiseUnderWorkerLoss(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(time.Hour),
		session.WithSweepInterval(time.Hour), // no sweeping during the test
	)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithSessionManager(mgr),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// Register the live server collector over the real server on a real instrument,
	// exactly as cmd does, and scrape its registry to read the exposition.
	m := metrics.New()
	if err := m.RegisterServerCollector(h.srv); err != nil {
		t.Fatalf("register server collector: %v", err)
	}
	reg := m.Registry()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}

	// worker-b reports more free VRAM, so absent affinity it is the better fit and
	// would win every turn; binding to worker-a makes the HIT observable.
	wa := dialRaw(t, h, "worker-a", []types.Model{{Name: "llama3"}})
	defer wa.close()
	wb := dialRaw(t, h, "worker-b", []types.Model{{Name: "llama3"}})
	defer wb.close()

	stop := make(chan struct{})
	defer close(stop)
	dispatched := make(chan string, 16)
	wa.autoReply(t, "worker-a", stop, dispatched)
	wb.autoReply(t, "worker-b", stop, dispatched)

	wa.heartbeatCapacity(t, "worker-a", 8<<30)
	wb.heartbeatCapacity(t, "worker-b", 64<<30)
	waitFor(t, 2*time.Second, "both workers in fleet with capacity", func() bool {
		a, okA := fleetByID(h.srv, "worker-a")
		b, okB := fleetByID(h.srv, "worker-b")
		return okA && okB && a.FreeVRAM == 8<<30 && b.FreeVRAM == 64<<30
	})

	ctx := context.Background()
	sess, err := mgr.Create(ctx, key.ID, "llama3")
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if _, err := mgr.Bind(ctx, sess.ID, key.ID, "worker-a"); err != nil {
		t.Fatalf("Bind to worker-a: %v", err)
	}

	submit := func(jobID string) {
		if _, err := h.srv.SubmitAuthorizedJob(ctx, key,
			types.Job{ID: jobID, Model: "llama3", Prompt: "hi", SessionID: sess.ID}); err != nil {
			t.Fatalf("submit %s: %v", jobID, err)
		}
	}

	// Baseline: before any session turn, both miss and rebind series read 0.
	assertCounter(t, reg, "agentgpu_session_rebinds_total", nil, 0)
	assertCounter(t, reg, "agentgpu_affinity_total", map[string]string{"result": "miss"}, 0)

	// Turn 1: routes to the bound worker-a — an affinity HIT. Miss/rebind stay 0.
	submit("turn-1")
	if first := <-dispatched; first != "worker-a" {
		t.Fatalf("turn-1 routed to %q, want bound worker-a (affinity hit)", first)
	}
	assertCounter(t, reg, "agentgpu_affinity_total", map[string]string{"result": "hit"}, 1)
	assertCounter(t, reg, "agentgpu_affinity_total", map[string]string{"result": "miss"}, 0)
	assertCounter(t, reg, "agentgpu_session_rebinds_total", nil, 0)

	// Lose the bound worker-a (close its stream); worker-b stays online.
	wa.close()
	waitFor(t, 2*time.Second, "worker-a gone, worker-b remains online", func() bool {
		_, goneOK := fleetByID(h.srv, "worker-a")
		b, survOK := fleetByID(h.srv, "worker-b")
		return !goneOK && survOK && b.Status == types.WorkerOnline
	})

	// Turn 2: the bound worker is gone, so the turn rebinds to worker-b — an
	// affinity MISS and a rebind. Both scraped counters must rise to 1; the hit
	// series is unchanged.
	submit("turn-2")
	if second := <-dispatched; second != "worker-b" {
		t.Fatalf("turn-2 routed to %q, want worker-b (rebind after loss)", second)
	}
	assertCounter(t, reg, "agentgpu_affinity_total", map[string]string{"result": "hit"}, 1)
	assertCounter(t, reg, "agentgpu_affinity_total", map[string]string{"result": "miss"}, 1)
	assertCounter(t, reg, "agentgpu_session_rebinds_total", nil, 1)
}

// TestActiveSessionsGaugeScrapeable proves agentgpu_active_sessions is scrapeable
// from the live session collector and tracks creates/deletes through the real
// Manager — the active-sessions visibility AC for #38.
func TestActiveSessionsGaugeScrapeable(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(time.Hour),
		session.WithSweepInterval(time.Hour),
	)
	m := metrics.New()
	if err := m.RegisterSessionCollector(mgr); err != nil {
		t.Fatalf("register session collector: %v", err)
	}
	reg := m.Registry()

	ctx := context.Background()
	assertGauge(t, reg, "agentgpu_active_sessions", 0)

	s1, _ := mgr.Create(ctx, "alice", "llama3")
	_, _ = mgr.Create(ctx, "bob", "llama3")
	assertGauge(t, reg, "agentgpu_active_sessions", 2)

	if err := mgr.Delete(ctx, s1.ID, "alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertGauge(t, reg, "agentgpu_active_sessions", 1)
}

// assertCounter gathers reg and asserts the named counter/gauge (optionally
// matching the given labels) equals want. It scrapes through the registry exactly
// as a Prometheus server would, so it proves the value is on the exposition — not
// just in the server's in-memory snapshot.
func assertCounter(t *testing.T, reg interface {
	Gather() ([]*dto.MetricFamily, error)
}, name string, labels map[string]string, want float64) {
	t.Helper()
	got, ok := gatherValue(t, reg, name, labels)
	if !ok {
		t.Fatalf("metric %s%v not present in exposition", name, labels)
	}
	if got != want {
		t.Fatalf("%s%v = %v, want %v", name, labels, got, want)
	}
}

// assertGauge is assertCounter for an unlabeled gauge.
func assertGauge(t *testing.T, reg interface {
	Gather() ([]*dto.MetricFamily, error)
}, name string, want float64) {
	t.Helper()
	assertCounter(t, reg, name, nil, want)
}

// gatherValue scrapes reg and returns the value of the metric family named name
// whose labels are a superset of the given labels (nil matches the first series).
// It reports ok=false when no such series is present.
func gatherValue(t *testing.T, reg interface {
	Gather() ([]*dto.MetricFamily, error)
}, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, mt := range fam.GetMetric() {
			if !labelsMatch(mt.GetLabel(), labels) {
				continue
			}
			switch {
			case mt.Counter != nil:
				return mt.Counter.GetValue(), true
			case mt.Gauge != nil:
				return mt.Gauge.GetValue(), true
			}
		}
	}
	return 0, false
}

// labelsMatch reports whether the metric's label pairs contain every wanted
// label=value. A nil/empty want matches any series.
func labelsMatch(have []*dto.LabelPair, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, lp := range have {
			if lp.GetName() == k && lp.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
