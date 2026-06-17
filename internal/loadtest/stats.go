package loadtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// StatsSnapshot is the subset of the admin stats endpoint the saturation poller
// tracks: the current total queue depth and the time-in-queue summary. It mirrors
// the shape of httpapi's GET /v1/admin/stats response (the queue + wait_time
// sections) so a remote poll decodes directly into it.
type StatsSnapshot struct {
	QueueTotal int
	WaitCount  uint64
	WaitMaxMs  uint64
	WaitMeanMs uint64
}

// StatsSource yields a StatsSnapshot on demand. Two implementations exist: an
// HTTP poller against GET /v1/admin/stats (remote mode) and a direct in-process
// reader over the server's accessors (inproc mode). Narrowing to an interface
// lets the poller work identically in both modes and keeps it unit-testable.
type StatsSource interface {
	Snapshot(ctx context.Context) (StatsSnapshot, error)
}

// SaturationPoller samples a StatsSource on an interval over a run and records
// the worst-case it sees: the peak queue depth and the final time-in-queue
// summary. It is started in a goroutine alongside the load and Stopped when the
// run ends; Observation returns the accumulated SaturationObs for the report.
//
// It is how queueing is made observable beyond the client-side 503 count: even
// when the queue never overflows (no 503s), a non-zero peak depth or queue-wait
// time shows requests were backing up under load.
type SaturationPoller struct {
	src      StatsSource
	interval time.Duration

	peakDepth  int
	waitCount  uint64
	waitMaxMs  uint64
	waitMeanMs uint64
	samples    int

	done chan struct{}
}

// NewSaturationPoller returns a poller over src sampling every interval. A
// non-positive interval defaults to 250ms.
func NewSaturationPoller(src StatsSource, interval time.Duration) *SaturationPoller {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	return &SaturationPoller{src: src, interval: interval, done: make(chan struct{})}
}

// Run samples the source until ctx is cancelled (the run ended), folding each
// snapshot into the running peak/summary. It takes one sample immediately so even
// a very short run records something, then samples on the interval. It returns
// when ctx is done; Observation is then safe to read. Run is meant to be invoked
// in its own goroutine.
func (p *SaturationPoller) Run(ctx context.Context) {
	defer close(p.done)
	p.sample(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// One final sample so the summary reflects end-of-run state.
			p.sample(context.Background())
			return
		case <-ticker.C:
			p.sample(ctx)
		}
	}
}

// sample takes one snapshot and folds it into the accumulators. A poll error is
// ignored (best-effort observability must never fail the run); the next tick
// retries.
func (p *SaturationPoller) sample(ctx context.Context) {
	snap, err := p.src.Snapshot(ctx)
	if err != nil {
		return
	}
	p.samples++
	if snap.QueueTotal > p.peakDepth {
		p.peakDepth = snap.QueueTotal
	}
	// The wait-time counters are cumulative on the server, so the latest snapshot
	// carries the run-to-date totals; keep the most recent (largest) values.
	if snap.WaitCount >= p.waitCount {
		p.waitCount = snap.WaitCount
		p.waitMeanMs = snap.WaitMeanMs
	}
	if snap.WaitMaxMs > p.waitMaxMs {
		p.waitMaxMs = snap.WaitMaxMs
	}
}

// Wait blocks until Run has returned (after ctx cancellation), so the caller can
// safely read Observation without racing the poller goroutine.
func (p *SaturationPoller) Wait() { <-p.done }

// Observation returns the accumulated saturation snapshot for the report. Call it
// after Wait so the read happens-after the poller goroutine finished.
func (p *SaturationPoller) Observation() *SaturationObs {
	return &SaturationObs{
		PeakQueueDepth: p.peakDepth,
		WaitCount:      p.waitCount,
		WaitMaxMs:      p.waitMaxMs,
		WaitMeanMs:     p.waitMeanMs,
		Samples:        p.samples,
	}
}

// HTTPStatsSource polls GET /v1/admin/stats with an admin Bearer token. It is the
// remote-mode StatsSource: the load runs against a deployment and this watches
// its queue/wait-time from outside.
type HTTPStatsSource struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// adminStatsWire is the subset of the admin stats response the poller decodes —
// the queue total and the wait_time summary. It matches the JSON field names in
// httpapi/admin.go (adminStatsResponse).
type adminStatsWire struct {
	Queue struct {
		Total int `json:"total"`
	} `json:"queue"`
	WaitTime struct {
		Count  uint64 `json:"count"`
		MaxMs  uint64 `json:"max_ms"`
		MeanMs uint64 `json:"mean_ms"`
	} `json:"wait_time"`
}

// Snapshot fetches and decodes one admin-stats response. A non-2xx response or a
// decode failure returns an error (the poller ignores it and retries), so a
// transient blip never disturbs the load.
func (h *HTTPStatsSource) Snapshot(ctx context.Context) (StatsSnapshot, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/v1/admin/stats", nil)
	if err != nil {
		return StatsSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return StatsSnapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return StatsSnapshot{}, &statsHTTPError{status: resp.StatusCode}
	}
	var wire adminStatsWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return StatsSnapshot{}, err
	}
	return StatsSnapshot{
		QueueTotal: wire.Queue.Total,
		WaitCount:  wire.WaitTime.Count,
		WaitMaxMs:  wire.WaitTime.MaxMs,
		WaitMeanMs: wire.WaitTime.MeanMs,
	}, nil
}

// statsHTTPError marks a non-2xx admin-stats poll (e.g. 403 from a non-admin
// token) so the caller can distinguish it from a transport error if needed. The
// poller treats both the same (ignore + retry).
type statsHTTPError struct{ status int }

func (e *statsHTTPError) Error() string { return http.StatusText(e.status) }
