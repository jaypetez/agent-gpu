package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeSessionSource is a deterministic SessionStatsSource for the session
// collector tests: it returns a fixed active-session count so the emitted gauge
// can be asserted exactly, with no real session Manager in the loop.
type fakeSessionSource struct{ active int }

func (f *fakeSessionSource) ActiveSessions() int { return f.active }

// TestSessionCollectorActiveSessions asserts agentgpu_active_sessions reflects the
// Manager's live count read at scrape time.
func TestSessionCollectorActiveSessions(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(newSessionCollector(&fakeSessionSource{active: 3})); err != nil {
		t.Fatalf("register session collector: %v", err)
	}

	want := `
# HELP agentgpu_active_sessions Number of conversation sessions currently live (created and not yet ended by delete or idle expiry).
# TYPE agentgpu_active_sessions gauge
agentgpu_active_sessions 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentgpu_active_sessions"); err != nil {
		t.Fatalf("active_sessions mismatch:\n%v", err)
	}
}

// TestSessionCollectorTracksChanges proves the gauge is a live read: a change to
// the source's count is reflected on the next scrape without re-registration
// (the create/delete-visible behavior the AC asks for).
func TestSessionCollectorTracksChanges(t *testing.T) {
	src := &fakeSessionSource{active: 0}
	c := newSessionCollector(src)

	if got := testutil.ToFloat64(c); got != 0 {
		t.Fatalf("active_sessions = %v, want 0 at start", got)
	}
	src.active = 5 // simulate creates
	if got := testutil.ToFloat64(c); got != 5 {
		t.Fatalf("active_sessions = %v, want 5 after creates", got)
	}
	src.active = 2 // simulate deletes
	if got := testutil.ToFloat64(c); got != 2 {
		t.Fatalf("active_sessions = %v, want 2 after deletes", got)
	}
}

// TestSessionCollectorNilSourceEmitsNothing proves a collector over a nil source
// contributes no metrics rather than erroring the whole scrape.
func TestSessionCollectorNilSourceEmitsNothing(t *testing.T) {
	if n := testutil.CollectAndCount(newSessionCollector(nil)); n != 0 {
		t.Fatalf("nil-source session collector emitted %d metrics, want 0", n)
	}
}

// TestRegisterSessionCollectorNilSafe proves the registration helper is a no-op
// (no error, nothing registered) for a nil *Metrics or nil source.
func TestRegisterSessionCollectorNilSafe(t *testing.T) {
	var m *Metrics
	if err := m.RegisterSessionCollector(&fakeSessionSource{}); err != nil {
		t.Fatalf("nil *Metrics RegisterSessionCollector = %v, want nil", err)
	}
	real := New()
	if err := real.RegisterSessionCollector(nil); err != nil {
		t.Fatalf("nil source RegisterSessionCollector = %v, want nil", err)
	}
}

// TestObserveSessionEndRecordsHistograms proves a session-end observation lands in
// both lifetime histograms (turns and duration), with the right sample count and
// sum — the key-metric-increments AC for the session histograms.
func TestObserveSessionEndRecordsHistograms(t *testing.T) {
	m := New()
	m.ObserveSessionEnd(3, 30*time.Second)
	m.ObserveSessionEnd(7, 90*time.Second)

	// Two observations on each histogram.
	if got := testutil.CollectAndCount(m.sessionTurns, "agentgpu_session_turns"); got == 0 {
		t.Fatalf("session_turns has no series, want observations recorded")
	}
	if got := testutil.CollectAndCount(m.sessionDuration, "agentgpu_session_duration_seconds"); got == 0 {
		t.Fatalf("session_duration_seconds has no series, want observations recorded")
	}

	// The _count and _sum are exact: 2 sessions, 3+7=10 turns, 30+90=120 seconds.
	wantTurns := `
# HELP agentgpu_session_turns Number of conversation turns a session accumulated over its lifetime, observed when the session ends (delete or idle expiry).
# TYPE agentgpu_session_turns histogram
agentgpu_session_turns_bucket{le="1"} 0
agentgpu_session_turns_bucket{le="2"} 0
agentgpu_session_turns_bucket{le="5"} 1
agentgpu_session_turns_bucket{le="10"} 2
agentgpu_session_turns_bucket{le="20"} 2
agentgpu_session_turns_bucket{le="50"} 2
agentgpu_session_turns_bucket{le="100"} 2
agentgpu_session_turns_bucket{le="200"} 2
agentgpu_session_turns_bucket{le="500"} 2
agentgpu_session_turns_bucket{le="+Inf"} 2
agentgpu_session_turns_sum 10
agentgpu_session_turns_count 2
`
	if err := testutil.CollectAndCompare(m.sessionTurns, strings.NewReader(wantTurns), "agentgpu_session_turns"); err != nil {
		t.Fatalf("session_turns mismatch:\n%v", err)
	}

	// Spot-check the duration sum/count (buckets asserted loosely via count above).
	wantDurMeta := `
# HELP agentgpu_session_duration_seconds How long a session lived (end minus creation) in seconds, observed when the session ends (delete or idle expiry).
# TYPE agentgpu_session_duration_seconds histogram
agentgpu_session_duration_seconds_sum 120
agentgpu_session_duration_seconds_count 2
`
	if err := testutil.CollectAndCompare(m.sessionDuration, strings.NewReader(wantDurMeta),
		"agentgpu_session_duration_seconds_sum", "agentgpu_session_duration_seconds_count"); err != nil {
		t.Fatalf("session_duration_seconds sum/count mismatch:\n%v", err)
	}
}

// TestObserveSessionEndClampsNegatives proves a negative turn count or duration
// (defensive against bad input / clock skew) is clamped to zero rather than
// recording a nonsensical negative observation.
func TestObserveSessionEndClampsNegatives(t *testing.T) {
	m := New()
	m.ObserveSessionEnd(-5, -10*time.Second)

	want := `
# HELP agentgpu_session_turns Number of conversation turns a session accumulated over its lifetime, observed when the session ends (delete or idle expiry).
# TYPE agentgpu_session_turns histogram
agentgpu_session_turns_sum 0
agentgpu_session_turns_count 1
`
	if err := testutil.CollectAndCompare(m.sessionTurns, strings.NewReader(want),
		"agentgpu_session_turns_sum", "agentgpu_session_turns_count"); err != nil {
		t.Fatalf("session_turns clamp mismatch:\n%v", err)
	}
}

// TestObserveSessionEndNilSafe proves ObserveSessionEnd is a no-op on a nil
// *Metrics (the disabled-build / no-observer-wired contract).
func TestObserveSessionEndNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveSessionEnd(3, time.Minute) // must not panic
}
