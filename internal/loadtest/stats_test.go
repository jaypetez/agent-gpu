package loadtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStatsSource is an in-memory StatsSource whose snapshot the test mutates to
// simulate a queue building up and draining.
type fakeStatsSource struct {
	mu   sync.Mutex
	snap StatsSnapshot
	err  error
	hits int64
}

func (f *fakeStatsSource) set(s StatsSnapshot) {
	f.mu.Lock()
	f.snap = s
	f.mu.Unlock()
}

func (f *fakeStatsSource) Snapshot(context.Context) (StatsSnapshot, error) {
	atomic.AddInt64(&f.hits, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.err
}

// TestSaturationPollerPeakAndSummary proves the poller records the peak queue
// depth across samples and keeps the latest cumulative wait-time summary.
func TestSaturationPollerPeakAndSummary(t *testing.T) {
	src := &fakeStatsSource{}
	src.set(StatsSnapshot{QueueTotal: 0})

	p := NewSaturationPoller(src, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)

	// Let it take a few samples, then raise the depth, then drain it.
	time.Sleep(20 * time.Millisecond)
	src.set(StatsSnapshot{QueueTotal: 7, WaitCount: 3, WaitMaxMs: 42, WaitMeanMs: 21})
	time.Sleep(20 * time.Millisecond)
	src.set(StatsSnapshot{QueueTotal: 0, WaitCount: 5, WaitMaxMs: 50, WaitMeanMs: 18})
	time.Sleep(20 * time.Millisecond)

	cancel()
	p.Wait()
	obs := p.Observation()

	if obs.PeakQueueDepth != 7 {
		t.Errorf("PeakQueueDepth = %d, want 7", obs.PeakQueueDepth)
	}
	if obs.WaitCount != 5 {
		t.Errorf("WaitCount = %d, want 5 (latest cumulative)", obs.WaitCount)
	}
	if obs.WaitMaxMs != 50 {
		t.Errorf("WaitMaxMs = %d, want 50 (max over samples)", obs.WaitMaxMs)
	}
	if obs.WaitMeanMs != 18 {
		t.Errorf("WaitMeanMs = %d, want 18 (latest)", obs.WaitMeanMs)
	}
	if obs.Samples == 0 {
		t.Errorf("Samples = 0, want > 0")
	}
}

// TestSaturationPollerImmediateSample proves Run takes a sample immediately so a
// run shorter than the interval still records something.
func TestSaturationPollerImmediateSample(t *testing.T) {
	src := &fakeStatsSource{}
	src.set(StatsSnapshot{QueueTotal: 3})
	p := NewSaturationPoller(src, time.Hour) // interval far longer than the run

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	time.Sleep(15 * time.Millisecond)
	cancel()
	p.Wait()

	if p.Observation().PeakQueueDepth != 3 {
		t.Errorf("PeakQueueDepth = %d, want 3 from the immediate sample", p.Observation().PeakQueueDepth)
	}
	if atomic.LoadInt64(&src.hits) == 0 {
		t.Errorf("source was never sampled")
	}
}

// TestHTTPStatsSourceDecodes proves the HTTP source decodes the admin stats
// shape and sends the Bearer token.
func TestHTTPStatsSourceDecodes(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"queue":{"total":9,"by_priority":{"normal":9}},
			"workers":[],
			"wait_time":{"count":4,"sum_ms":80,"max_ms":40,"mean_ms":20,"buckets":[]}
		}`))
	}))
	defer ts.Close()

	src := &HTTPStatsSource{BaseURL: ts.URL, Token: "admintoken", Client: ts.Client()}
	snap, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if gotAuth != "Bearer admintoken" {
		t.Errorf("Authorization = %q, want Bearer admintoken", gotAuth)
	}
	if snap.QueueTotal != 9 {
		t.Errorf("QueueTotal = %d, want 9", snap.QueueTotal)
	}
	if snap.WaitCount != 4 || snap.WaitMaxMs != 40 || snap.WaitMeanMs != 20 {
		t.Errorf("wait summary = %+v, want 4/40/20", snap)
	}
}

// TestHTTPStatsSourceNon2xx proves a non-2xx poll (e.g. a non-admin token → 403)
// returns an error so the poller ignores it rather than recording garbage.
func TestHTTPStatsSourceNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	src := &HTTPStatsSource{BaseURL: ts.URL, Token: "nonadmin", Client: ts.Client()}
	if _, err := src.Snapshot(context.Background()); err == nil {
		t.Fatalf("Snapshot on 403 = nil error, want error")
	}
}
