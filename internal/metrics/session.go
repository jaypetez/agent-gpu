package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Session observability (#38). This file holds the session-specific metrics,
// built on top of the existing instrument (#24) and kept deliberately
// bounded-cardinality (no session id or key id ever appears as a label):
//
//   - agentgpu_active_sessions (gauge): the live count of sessions the session
//     Manager currently holds, read at scrape time via a small collector over a
//     SessionStatsSource — the same scrape-time pattern the server collector uses,
//     so the gauge needs no background poller and never goes stale.
//   - agentgpu_session_turns (histogram) and agentgpu_session_duration_seconds
//     (histogram): the per-session lifetime distributions, recorded by pushing one
//     observation when a session ENDS (explicit delete or idle expiry). The
//     Manager surfaces session-end events through a nil-safe observer hook that the
//     cmd layer wires to ObserveSessionEnd; a build that does not wire it records
//     nothing (the histograms simply stay empty).

// SessionStatsSource is the read-only slice of the session Manager the live
// session collector scrapes: the current count of active sessions. Narrowing to
// an interface keeps the collector unit-testable with a fake source and documents
// the only coupling between this package and the session subsystem.
// *session.Manager satisfies it (ActiveSessions).
type SessionStatsSource interface {
	// ActiveSessions returns the total number of live sessions across all owners.
	ActiveSessions() int
}

// sessionCollector is a prometheus.Collector that reads the session Manager's
// current active-session count at scrape time and emits it as a gauge. Like the
// server collector, collecting at scrape time keeps the exported value exactly
// consistent with the Manager's own count with no extra goroutine; the read is a
// cheap in-memory tally.
type sessionCollector struct {
	src            SessionStatsSource
	activeSessions *prometheus.Desc
}

// newSessionCollector builds the collector and its descriptor. The name is
// namespaced agentgpu_* to match the rest of the instrument.
func newSessionCollector(src SessionStatsSource) *sessionCollector {
	return &sessionCollector{
		src: src,
		activeSessions: prometheus.NewDesc(
			namespace+"_active_sessions",
			"Number of conversation sessions currently live (created and not yet ended by delete or idle expiry).",
			nil, nil,
		),
	}
}

// Describe sends the one descriptor this collector emits so the registry can
// detect a clash at registration time.
func (c *sessionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.activeSessions
}

// Collect reads the Manager's current active-session count and emits it as a
// gauge. A nil source yields no metric (the collector contributes nothing rather
// than erroring the whole scrape), mirroring the server collector.
func (c *sessionCollector) Collect(ch chan<- prometheus.Metric) {
	if c.src == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(
		c.activeSessions, prometheus.GaugeValue, float64(c.src.ActiveSessions()),
	)
}

// RegisterSessionCollector registers a live collector over src on this
// instrument's registry, so the next scrape reflects the session Manager's
// current active-session count. It is called once at startup after the Manager is
// built. It returns an error if registration fails (e.g. a descriptor clash) and
// is a no-op returning nil for a nil *Metrics or nil src — matching
// RegisterServerCollector's nil-safety so a disabled build (or a test without
// sessions) behaves unchanged.
func (m *Metrics) RegisterSessionCollector(src SessionStatsSource) error {
	if m == nil || src == nil {
		return nil
	}
	return m.reg.Register(newSessionCollector(src))
}

// ObserveSessionEnd records one ended session's lifetime against the session
// histograms (#38): its turn count in agentgpu_session_turns and its wall-clock
// duration (seconds) in agentgpu_session_duration_seconds. It is the push entry
// point the cmd layer wires to the session Manager's end observer, so exactly one
// observation is recorded per session end. A negative duration (clock skew) is
// clamped to zero. No-op on a nil *Metrics, so a disabled build records nothing.
func (m *Metrics) ObserveSessionEnd(turns int, dur time.Duration) {
	if m == nil {
		return
	}
	if turns < 0 {
		turns = 0
	}
	seconds := dur.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.sessionTurns.Observe(float64(turns))
	m.sessionDuration.Observe(seconds)
}
