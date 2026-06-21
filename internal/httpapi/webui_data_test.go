package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// webui_data_test.go unit-tests the dashboard data mapping (the live fleet/telemetry
// state → view-model logic), including the KPI status-tone thresholds, so the
// "status by color AND text" health derivation is verified independently of a render.

func TestBuildKPIsThresholds(t *testing.T) {
	tests := []struct {
		name        string
		fleet       []types.Worker
		qs          queue.Stats
		rl          RateLimitStats
		wantQueue   string // tone of the queue KPI
		wantWorkers string // tone of the workers KPI
		wantThr     string // tone of the throttle KPI
	}{
		{
			name:        "idle fleet, empty queue, no throttles",
			fleet:       nil,
			qs:          queue.Stats{Total: 0},
			wantQueue:   webui.ToneIdle,
			wantWorkers: webui.ToneIdle,
			wantThr:     webui.ToneOK,
		},
		{
			name:        "healthy: all online, small queue",
			fleet:       []types.Worker{{ID: "a", Status: types.WorkerOnline}, {ID: "b", Status: types.WorkerOnline}},
			qs:          queue.Stats{Total: 1},
			wantQueue:   webui.ToneOK,
			wantWorkers: webui.ToneOK,
			wantThr:     webui.ToneOK,
		},
		{
			name:        "backlog with workers online -> queue watch",
			fleet:       []types.Worker{{ID: "a", Status: types.WorkerOnline}},
			qs:          queue.Stats{Total: 10}, // > online*4
			wantQueue:   webui.ToneWarn,
			wantWorkers: webui.ToneOK,
			wantThr:     webui.ToneOK,
		},
		{
			name:        "backlog with NO online workers -> queue alert, workers alert",
			fleet:       []types.Worker{{ID: "a", Status: types.WorkerStale}},
			qs:          queue.Stats{Total: 3},
			wantQueue:   webui.ToneDanger,
			wantWorkers: webui.ToneDanger,
			wantThr:     webui.ToneOK,
		},
		{
			name:        "some draining -> workers watch",
			fleet:       []types.Worker{{ID: "a", Status: types.WorkerOnline}, {ID: "b", Status: types.WorkerDraining}},
			qs:          queue.Stats{Total: 0},
			wantQueue:   webui.ToneIdle,
			wantWorkers: webui.ToneWarn,
			wantThr:     webui.ToneOK,
		},
		{
			name:        "throttles seen -> throttle watch",
			fleet:       []types.Worker{{ID: "a", Status: types.WorkerOnline}},
			qs:          queue.Stats{Total: 0},
			rl:          RateLimitStats{GlobalThrottled: 5},
			wantQueue:   webui.ToneIdle,
			wantWorkers: webui.ToneOK,
			wantThr:     webui.ToneWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kpis := buildKPIs(tc.fleet, tc.qs, RequestStats{}, tc.rl)
			if len(kpis) != 3 {
				t.Fatalf("want 3 KPIs, got %d", len(kpis))
			}
			if kpis[0].Tone != tc.wantQueue {
				t.Errorf("queue KPI tone = %q, want %q", kpis[0].Tone, tc.wantQueue)
			}
			if kpis[1].Tone != tc.wantWorkers {
				t.Errorf("workers KPI tone = %q, want %q", kpis[1].Tone, tc.wantWorkers)
			}
			if kpis[2].Tone != tc.wantThr {
				t.Errorf("throttle KPI tone = %q, want %q", kpis[2].Tone, tc.wantThr)
			}
			// Every KPI carries a non-empty caption (status stated in words, AC3).
			for i, k := range kpis {
				if k.Caption == "" {
					t.Errorf("KPI[%d] %q has no caption", i, k.Label)
				}
			}
		})
	}
}

func TestBuildWorkerRowsSortedWithTones(t *testing.T) {
	fleet := []types.Worker{
		{ID: "z", Status: types.WorkerStale, ActiveJobs: 0, Load: 0},
		{ID: "a", Status: types.WorkerOnline, ActiveJobs: 2, Load: 40},
		{ID: "m", Status: types.WorkerDraining, ActiveJobs: 1, Load: 10},
	}
	rows := buildWorkerRows(fleet)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	// Sorted by id.
	if rows[0].ID != "a" || rows[1].ID != "m" || rows[2].ID != "z" {
		t.Errorf("rows not sorted by id: %q %q %q", rows[0].ID, rows[1].ID, rows[2].ID)
	}
	// Tone + status text mapping.
	if rows[0].Tone != webui.ToneOK || rows[0].Status != "online" {
		t.Errorf("online worker row wrong: %+v", rows[0])
	}
	if rows[1].Tone != webui.ToneWarn || rows[1].Status != "draining" {
		t.Errorf("draining worker row wrong: %+v", rows[1])
	}
	if rows[2].Tone != webui.ToneDanger || rows[2].Status != "stale" {
		t.Errorf("stale worker row wrong: %+v", rows[2])
	}
}

func TestBuildQueueDepth(t *testing.T) {
	qs := queue.Stats{
		Total: 6,
		ByPriority: map[queue.Priority]int{
			queue.PriorityHigh:   3,
			queue.PriorityNormal: 2,
			queue.PriorityLow:    1,
		},
	}
	q := buildQueueDepth(qs)
	if q.Total != 6 || q.High != 3 || q.Normal != 2 || q.Low != 1 {
		t.Errorf("queue depth = %+v", q)
	}
}

func TestLevelTone(t *testing.T) {
	if levelTone("ERROR") != webui.ToneDanger {
		t.Error("ERROR -> danger")
	}
	if levelTone("WARN") != webui.ToneWarn {
		t.Error("WARN -> warn")
	}
	if levelTone("INFO") != webui.ToneInfo || levelTone("DEBUG") != webui.ToneInfo {
		t.Error("INFO/DEBUG -> info")
	}
}

func TestFormatCount(t *testing.T) {
	cases := map[uint64]string{
		0:         "0",
		999:       "999",
		1000:      "1.0k",
		1500:      "1.5k",
		1_000_000: "1.0M",
		2_500_000: "2.5M",
	}
	for n, want := range cases {
		if got := formatCount(n); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestCollectOverviewWithFakeFleet proves the end-to-end data pull wires the fleet
// snapshot into the view-models. With no log source wired it yields empty events
// even for a viewer holding logs:read (the admin key on the context), and never
// panics. The request carries an admin key on its context, matching how the
// uiScopeAuth wrapper invokes collectOverview in production.
func TestCollectOverviewWithFakeFleet(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{
		snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline, ActiveJobs: 1, Load: 20}},
	})
	r := httptest.NewRequest(http.MethodGet, "/admin/partials/overview", nil)
	r = r.WithContext(withKey(r.Context(), store.APIKey{Roles: []string{authz.RoleAdmin}}))
	data := s.collectOverview(r)
	if len(data.kpis) != 3 {
		t.Fatalf("want 3 KPIs, got %d", len(data.kpis))
	}
	if len(data.workers) != 1 || data.workers[0].ID != "w1" {
		t.Errorf("worker rows = %+v", data.workers)
	}
	if data.events != nil {
		t.Errorf("expected nil events with no log source, got %+v", data.events)
	}
}

// TestCollectOverviewOmitsEventsWithoutLogsRead proves the per-viewer log gating at
// the data layer: a viewer holding telemetry:read but NOT logs:read never has the
// log ring read for them, so events are empty even when a log source IS wired with
// content — the telemetry panels are unaffected.
func TestCollectOverviewOmitsEventsWithoutLogsRead(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{
		snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}},
	})
	src := &fakeLogSource{}
	src.add(LogRecord{Time: logAt(10), Level: "ERROR", Message: "leak-me", Attrs: nil})
	s.logs = src

	// telemetry:read only — must NOT pull events.
	r := httptest.NewRequest(http.MethodGet, "/admin/partials/overview", nil)
	r = r.WithContext(withKey(r.Context(), store.APIKey{AdminScopes: []string{authz.ScopeTelemetryRead}}))
	if data := s.collectOverview(r); len(data.events) != 0 {
		t.Errorf("telemetry-only viewer should get no events, got %+v", data.events)
	}

	// telemetry:read + logs:read — events are pulled.
	r = httptest.NewRequest(http.MethodGet, "/admin/partials/overview", nil)
	r = r.WithContext(withKey(r.Context(), store.APIKey{AdminScopes: []string{authz.ScopeTelemetryRead, authz.ScopeLogsRead}}))
	if data := s.collectOverview(r); len(data.events) != 1 || data.events[0].Message != "leak-me" {
		t.Errorf("logs:read viewer should get the seeded event, got %+v", data.events)
	}
}
