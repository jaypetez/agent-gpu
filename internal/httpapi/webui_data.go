package httpapi

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// webui_data.go maps the server's live, in-process state onto the console's
// view-models for the dashboard (#100). It reads the SAME accessors the JSON admin
// endpoints use — the fleet snapshot, queue stats, the request-stats mirror, the
// throttle counters, and the log ring — so the dashboard's numbers match
// /v1/admin/telemetry, /stats, /gpus, and /logs by construction. It performs no
// new probing and starts no pollers; every value is read on demand.

// overviewData bundles everything the Overview partial renders: the KPI row and
// the three named panels (queue depth, worker health, event stream).
type overviewData struct {
	kpis    []webui.KPI
	queue   webui.QueueDepth
	workers []webui.WorkerRow
	events  []webui.EventRow
}

// maxEventRows bounds how many recent log lines the event-stream panel shows. The
// dashboard is a glance surface, not the full log viewer (that is the Logs
// section); a short tail keeps the panel readable and the render cheap.
const maxEventRows = 8

// collectOverview reads the live fleet/telemetry/log state once and folds it into
// the dashboard view-models. It is the single data pull behind the Overview
// partial, so the KPI row and all three panels reflect one consistent instant.
//
// The queue/worker/throttle data is telemetry (the route is gated on
// telemetry:read), but the event-stream panel surfaces LOG lines, which are
// logs:read territory. So the events are fetched ONLY when the viewer also holds
// logs:read — mirroring the per-section Visible gating in buildShell. A viewer
// with telemetry:read but not logs:read gets every panel except a populated event
// stream (it renders its calm empty state), and crucially we never even read the
// log ring for them, so no log line can leak through this surface.
func (s *Server) collectOverview(r *http.Request) overviewData {
	fleet := s.fleet.Fleet()
	qs := s.fleet.QueueStats()
	reqs := s.RequestStats()
	rl := s.RateLimitStats()

	var events []webui.EventRow
	if key, ok := keyFromContext(r.Context()); ok && authz.HasScope(key, authz.ScopeLogsRead) {
		events = s.buildEvents()
	}

	return overviewData{
		kpis:    buildKPIs(fleet, qs, reqs, rl),
		queue:   buildQueueDepth(qs),
		workers: buildWorkerRows(fleet),
		events:  events,
	}
}

// buildKPIs assembles the three headline metrics an operator scans first: queue
// depth, worker health, and throttle pressure. Each carries a status tone AND a
// caption, so its health is conveyed by color and words together (AC3). The tones
// are derived from simple, explained thresholds — this is a glance view, not an
// alerting engine.
func buildKPIs(fleet []types.Worker, qs queue.Stats, reqs RequestStats, rl RateLimitStats) []webui.KPI {
	online := 0
	for _, w := range fleet {
		if w.Status == types.WorkerOnline {
			online++
		}
	}
	total := len(fleet)

	// Queue depth: idle when empty, watch as it grows, alert when it is deep
	// relative to the online capacity (a backlog with nowhere to drain).
	queueTone := webui.ToneOK
	queueCaption := "Jobs dispatch without waiting"
	switch {
	case qs.Total == 0:
		queueTone = webui.ToneIdle
		queueCaption = "No jobs waiting"
	case online == 0:
		queueTone = webui.ToneDanger
		queueCaption = "Backlog with no online workers"
	case qs.Total > online*4:
		queueTone = webui.ToneWarn
		queueCaption = "Backlog building — watch capacity"
	}

	// Worker health: ok when all online, watch when some are draining/stale,
	// alert when none are online.
	workerTone := webui.ToneOK
	workerCaption := "All workers online"
	switch {
	case total == 0:
		workerTone = webui.ToneIdle
		workerCaption = "No workers connected"
	case online == 0:
		workerTone = webui.ToneDanger
		workerCaption = "No workers online"
	case online < total:
		workerTone = webui.ToneWarn
		workerCaption = strconv.Itoa(total-online) + " not taking jobs"
	}

	// Throttles: ok at zero, watch once requests are being rejected (the fleet is
	// at a limit). Cumulative since start, so it is a pressure signal, not a rate.
	throttled := rl.GlobalThrottled + rl.KeyThrottled
	throttleTone := webui.ToneOK
	throttleCaption := "No requests throttled"
	if throttled > 0 {
		throttleTone = webui.ToneWarn
		throttleCaption = "Requests hit a rate limit"
	}

	return []webui.KPI{
		{
			Label:   "Queue depth",
			Value:   strconv.Itoa(qs.Total),
			Unit:    "jobs",
			Tone:    queueTone,
			Caption: queueCaption,
		},
		{
			Label:   "Workers online",
			Value:   strconv.Itoa(online) + "/" + strconv.Itoa(total),
			Tone:    workerTone,
			Caption: workerCaption,
		},
		{
			Label:   "Throttled",
			Value:   formatCount(throttled),
			Unit:    "total",
			Tone:    throttleTone,
			Caption: throttleCaption,
		},
	}
}

// buildQueueDepth folds the per-priority queue stats into the panel's model. The
// queue.Stats.ByPriority map is keyed by queue.Priority; missing levels are zero.
func buildQueueDepth(qs queue.Stats) webui.QueueDepth {
	return webui.QueueDepth{
		Total:  qs.Total,
		High:   qs.ByPriority[queue.PriorityHigh],
		Normal: qs.ByPriority[queue.PriorityNormal],
		Low:    qs.ByPriority[queue.PriorityLow],
	}
}

// buildWorkerRows projects the fleet snapshot into the worker-health table,
// sorted by id for a stable render. Each row's tone maps the lifecycle status to
// the status language; the status string is always shown as text beside the badge.
func buildWorkerRows(fleet []types.Worker) []webui.WorkerRow {
	rows := make([]webui.WorkerRow, 0, len(fleet))
	for _, w := range fleet {
		rows = append(rows, webui.WorkerRow{
			ID:         w.ID,
			Status:     w.Status.String(),
			Tone:       workerTone(w.Status),
			ActiveJobs: w.ActiveJobs,
			Load:       w.Load,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// workerTone maps a worker lifecycle status to a status tone: online is healthy,
// draining is a transient watch state, stale/unknown is an alert.
func workerTone(st types.WorkerStatus) string {
	switch st {
	case types.WorkerOnline:
		return webui.ToneOK
	case types.WorkerDraining:
		return webui.ToneWarn
	default:
		return webui.ToneDanger
	}
}

// buildEvents reads the most recent log lines from the injected log source and
// projects them into the event-stream panel, newest-first. When no log source is
// wired (WithLogSource not supplied, e.g. most unit tests), it returns an empty
// slice so the panel shows its calm empty state rather than erroring.
func (s *Server) buildEvents() []webui.EventRow {
	if s.logs == nil {
		return nil
	}
	recs := s.logs.Snapshot() // oldest-first, bounded by the ring capacity
	// Take the last maxEventRows and reverse to newest-first.
	start := 0
	if len(recs) > maxEventRows {
		start = len(recs) - maxEventRows
	}
	tail := recs[start:]
	rows := make([]webui.EventRow, 0, len(tail))
	for i := len(tail) - 1; i >= 0; i-- {
		r := tail[i]
		rows = append(rows, webui.EventRow{
			Time:    r.Time.Format("15:04:05"),
			Level:   r.Level,
			Tone:    levelTone(r.Level),
			Message: r.Message,
		})
	}
	return rows
}

// levelTone maps a slog level name to a status tone for the event stream: ERROR is
// an alert, WARN a watch, everything else informational.
func levelTone(level string) string {
	switch level {
	case "ERROR":
		return webui.ToneDanger
	case "WARN":
		return webui.ToneWarn
	default:
		return webui.ToneInfo
	}
}

// formatCount renders a cumulative counter compactly: plain below 1000, then
// k/M-suffixed so a large throttle count stays one glanceable token (1.2k, 3.4M)
// rather than a long run of digits in a KPI card.
func formatCount(n uint64) string {
	switch {
	case n < 1000:
		return strconv.FormatUint(n, 10)
	case n < 1_000_000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	}
}
