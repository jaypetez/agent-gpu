package main

import (
	"time"

	"github.com/jaypetez/agent-gpu/internal/httpapi"
)

// logRingSource adapts the in-memory log ring (#90) to the httpapi.LogSource seam
// the admin log endpoints (#99) read through. It lives in cmd — not in httpapi —
// so the HTTP layer never depends on the concrete logging setup (the cmd-private
// logRecord), mirroring how the runtime-config appliers are wired here (#92). It
// only ADDS a read adapter over the existing ring; it does not touch the ring's
// capture, redaction, or level behavior.
//
// The ring already stores REDACTED records (a secret-named attribute is masked to
// "[REDACTED]" before it is appended), so Snapshot copies Attrs through verbatim —
// no secret can reach the HTTP layer.
type logRingSource struct {
	ring *logRing
}

// newLogRingSource wraps ring as an httpapi.LogSource. A nil ring (logging
// disabled / zero-capacity) still yields a usable source: Snapshot returns nil and
// Count returns 0, so the admin endpoints behave as an empty, never-growing tail.
func newLogRingSource(ring *logRing) *logRingSource {
	return &logRingSource{ring: ring}
}

// Snapshot returns the buffered records (oldest-first) projected onto
// httpapi.LogRecord. The query endpoint uses it. The slice and each map are the
// ring's own snapshot copies, so the HTTP layer never aliases live ring state.
func (s *logRingSource) Snapshot() []httpapi.LogRecord {
	if s.ring == nil {
		return nil
	}
	return mapLogRecords(s.ring.Snapshot())
}

// Count returns the ring's monotonic total-appended counter (the cursor the live
// tail tracks). A nil ring reports 0.
func (s *logRingSource) Count() uint64 {
	if s.ring == nil {
		return 0
	}
	return s.ring.Count()
}

// SnapshotCount returns the buffered records (projected onto httpapi.LogRecord)
// AND the matching monotonic count in one consistent read, so the live tail's
// new-since-cursor math cannot race a concurrent append. A nil ring yields an
// empty slice and 0.
func (s *logRingSource) SnapshotCount() ([]httpapi.LogRecord, uint64) {
	if s.ring == nil {
		return nil, 0
	}
	recs, count := s.ring.SnapshotCount()
	return mapLogRecords(recs), count
}

// mapLogRecords projects the cmd-private ring records onto the exported
// httpapi.LogRecord shape: the unix-nano Time becomes a real time.Time, the slog
// level becomes its name string (Level.String(): DEBUG/INFO/WARN/ERROR), and the
// already-redacted Attrs map is carried through unchanged (the ring redacts at
// capture, so no secret can reach the HTTP layer).
func mapLogRecords(recs []logRecord) []httpapi.LogRecord {
	out := make([]httpapi.LogRecord, 0, len(recs))
	for _, rec := range recs {
		out = append(out, httpapi.LogRecord{
			Time:    time.Unix(0, rec.Time),
			Level:   rec.Level.String(),
			Message: rec.Message,
			Attrs:   rec.Attrs,
		})
	}
	return out
}

// Compile-time assertion that *logRingSource satisfies the HTTP layer's seam.
var _ httpapi.LogSource = (*logRingSource)(nil)
