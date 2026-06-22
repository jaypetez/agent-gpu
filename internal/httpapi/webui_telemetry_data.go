package httpapi

import (
	"sort"
	"strconv"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_telemetry_data.go maps the server's in-process telemetry collectors onto
// the console's Telemetry view-models (#103). It reads the SAME accessors GET
// /v1/admin/telemetry reads — the request-stats mirror (RequestStats), the throttle
// counters (RateLimitStats), the fleet snapshot folded by status, the queue and
// wait-time stats, the affinity counters, and the live session count — so the
// dashboard's numbers match the JSON endpoint by construction. It performs no new
// probing and starts no pollers; every value is read on demand for one consistent
// instant.

// collectTelemetry assembles the telemetry board in one pass from the existing
// in-process collectors, mirroring handleAdminTelemetry. It is the single data pull
// behind the telemetry partial, so every panel reflects one consistent instant.
func (s *Server) collectTelemetry() webui.TelemetryBoard {
	reqs := s.RequestStats()
	rl := s.RateLimitStats()
	fleet := s.fleet.Fleet()
	qs := s.fleet.QueueStats()
	wt := s.fleet.WaitTimeStats()
	aff := s.fleet.AffinityStats()

	active := 0
	if s.sessionMgr != nil {
		active = s.sessionMgr.ActiveSessions()
	}

	online := 0
	byStatus := map[string]int{}
	for _, w := range fleet {
		byStatus[w.Status.String()]++
	}

	// KPI strip: the headline rates an operator scans first. Request count is
	// cumulative; uptime turns it into an approximate rate. Each carries a tone +
	// word so meaning never rests on color alone.
	uptime := s.uptimeSeconds()
	rpsTone := webui.ToneOK
	rpsCaption := "Cumulative since start"
	throttled := rl.GlobalThrottled + rl.KeyThrottled
	throttleTone := webui.ToneOK
	throttleCaption := "No requests throttled"
	if throttled > 0 {
		throttleTone = webui.ToneWarn
		throttleCaption = "Requests hit a rate limit"
	}
	latencyTone := webui.ToneOK
	if reqs.MeanMs >= 1000 {
		latencyTone = webui.ToneWarn
	}

	kpis := []webui.KPI{
		{
			Label:   "Requests",
			Value:   formatCount(reqs.Count),
			Unit:    "total",
			Tone:    rpsTone,
			Caption: rpsCaption,
		},
		{
			Label:   "Mean latency",
			Value:   strconv.FormatUint(reqs.MeanMs, 10),
			Unit:    "ms",
			Tone:    latencyTone,
			Caption: "p-mean over all requests; max " + strconv.FormatUint(reqs.MaxMs, 10) + "ms",
		},
		{
			Label:   "Throttled",
			Value:   formatCount(throttled),
			Unit:    "total",
			Tone:    throttleTone,
			Caption: throttleCaption,
		},
		{
			Label:   "Active sessions",
			Value:   strconv.Itoa(active),
			Tone:    webui.ToneInfo,
			Caption: "Live conversation sessions",
		},
		{
			Label:   "Queue depth",
			Value:   strconv.Itoa(qs.Total),
			Unit:    "jobs",
			Tone:    queueTelemetryTone(qs.Total, online),
			Caption: "Jobs waiting for a worker",
		},
		{
			Label:   "Uptime",
			Value:   formatUptime(uptime),
			Tone:    webui.ToneIdle,
			Caption: "Time since the server started",
		},
	}

	return webui.TelemetryBoard{
		KPIs: kpis,
		Latency: histogramView(reqs.MeanMs, reqs.MaxMs, reqs.Count, func() []webui.HistogramBar {
			bars := make([]webui.HistogramBar, len(reqs.Buckets))
			max := maxBucketCount(toLeCounts(reqs.Buckets))
			for i, b := range reqs.Buckets {
				bars[i] = histBar(b.LeMs, b.Count, max)
			}
			return bars
		}()),
		WaitTime: histogramView(waitMean(wt.SumMs, wt.Count), wt.MaxMs, wt.Count, func() []webui.HistogramBar {
			bars := make([]webui.HistogramBar, len(wt.Buckets))
			counts := make([]uint64, len(wt.Buckets))
			for i, b := range wt.Buckets {
				counts[i] = b.Count
			}
			max := maxBucketCount(counts)
			for i, b := range wt.Buckets {
				bars[i] = histBar(b.LeMs, b.Count, max)
			}
			return bars
		}()),
		Fleet:    fleetStatusView(byStatus),
		Affinity: affinityView(aff.Hits, aff.Misses, aff.Rebinds),
	}
}

// queueTelemetryTone tones the queue-depth KPI: idle empty, alert when there is a
// backlog with no online workers, watch when it builds past the online capacity.
func queueTelemetryTone(total, online int) string {
	switch {
	case total == 0:
		return webui.ToneIdle
	case online == 0:
		return webui.ToneDanger
	case total > online*4:
		return webui.ToneWarn
	default:
		return webui.ToneOK
	}
}

// histogramView assembles a Histogram model from its summary + bars.
func histogramView(meanMs, maxMs uint64, count uint64, bars []webui.HistogramBar) webui.Histogram {
	return webui.Histogram{Count: count, MeanMs: meanMs, MaxMs: maxMs, Bars: bars}
}

// histBar builds one histogram bar: a human bound label (the +Inf sentinel LeMs==0
// renders as "slower"), the count, and the width as a percentage of the largest
// bucket so the distribution's shape reads at a glance.
func histBar(leMs, count, max uint64) webui.HistogramBar {
	pct := 0
	if max > 0 {
		pct = int((count * 100) / max)
	}
	return webui.HistogramBar{Label: bucketLabel(leMs), Count: count, Pct: pct}
}

// bucketLabel renders a cumulative bucket's upper bound for display. The 0 sentinel
// is the +Inf bucket (everything slower than the last finite bound).
func bucketLabel(leMs uint64) string {
	if leMs == 0 {
		return "slower"
	}
	if leMs >= 1000 {
		return "≤ " + strconv.FormatUint(leMs/1000, 10) + "s"
	}
	return "≤ " + strconv.FormatUint(leMs, 10) + "ms"
}

// toLeCounts copies a request-stats bucket slice's counts for the max computation.
func toLeCounts(buckets []RequestStatsBucket) []uint64 {
	out := make([]uint64, len(buckets))
	for i, b := range buckets {
		out[i] = b.Count
	}
	return out
}

// maxBucketCount returns the largest count in a bucket slice (the denominator for
// the bar widths), 0 for an empty slice.
func maxBucketCount(counts []uint64) uint64 {
	var max uint64
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	return max
}

// waitMean computes the integer mean wait (ms), guarding a zero count.
func waitMean(sumMs, count uint64) uint64 {
	if count == 0 {
		return 0
	}
	return sumMs / count
}

// fleetStatusView projects the by-status worker counts into sorted rows with a tone
// per status (online ok, draining watch, anything else alert), so the breakdown
// reads in text + color.
func fleetStatusView(byStatus map[string]int) []webui.TelemetryStatus {
	rows := make([]webui.TelemetryStatus, 0, len(byStatus))
	for status, n := range byStatus {
		rows = append(rows, webui.TelemetryStatus{
			Status: status,
			Tone:   statusWordTone(status),
			Count:  n,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Status < rows[j].Status })
	return rows
}

// statusWordTone maps a worker-status WORD (already stringified) to a tone, since
// the telemetry fold works on status strings rather than the typed enum.
func statusWordTone(status string) string {
	switch status {
	case "online":
		return webui.ToneOK
	case "draining":
		return webui.ToneWarn
	default:
		return webui.ToneDanger
	}
}

// affinityView derives the session-affinity panel: the raw counts plus a hit-rate
// percentage (hits / (hits+misses)) and its tone. HasData is false when no turns
// have been routed (the panel then shows its calm empty state).
func affinityView(hits, misses, rebinds uint64) webui.TelemetryAffinity {
	total := hits + misses
	a := webui.TelemetryAffinity{
		Hits:    formatCount(hits),
		Misses:  formatCount(misses),
		Rebinds: formatCount(rebinds),
		HasData: total > 0,
	}
	if total > 0 {
		a.HitRate = int((hits * 100) / total)
		switch {
		case a.HitRate >= 80:
			a.RateTone = webui.ToneOK
		case a.HitRate >= 50:
			a.RateTone = webui.ToneWarn
		default:
			a.RateTone = webui.ToneDanger
		}
	}
	return a
}

// formatUptime renders the process uptime seconds as a compact "Nd Nh" / "Nh Nm" /
// "Nm" / "Ns" string for the KPI, mirroring the worker uptime formatting.
func formatUptime(sec int64) string {
	if sec <= 0 {
		return "0s"
	}
	days := sec / 86400
	hours := (sec % 86400) / 3600
	mins := (sec % 3600) / 60
	secs := sec % 60
	switch {
	case days > 0:
		return strconv.FormatInt(days, 10) + "d " + strconv.FormatInt(hours, 10) + "h"
	case hours > 0:
		return strconv.FormatInt(hours, 10) + "h " + strconv.FormatInt(mins, 10) + "m"
	case mins > 0:
		return strconv.FormatInt(mins, 10) + "m"
	default:
		return strconv.FormatInt(secs, 10) + "s"
	}
}
