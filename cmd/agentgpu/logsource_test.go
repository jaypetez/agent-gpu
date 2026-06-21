package main

import (
	"path/filepath"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/config"
)

// TestLogRingCountMonotonic proves the Count() method added for #99 returns the
// total number of records ever appended (a monotonic cursor), distinct from Len()
// (the capped buffer size). It never resets when the ring wraps.
func TestLogRingCountMonotonic(t *testing.T) {
	ring := newLogRing(2) // tiny so it wraps quickly.
	if ring.Count() != 0 {
		t.Fatalf("fresh ring Count = %d, want 0", ring.Count())
	}
	for i := 0; i < 5; i++ {
		ring.append(logRecord{Message: "x"})
	}
	if ring.Count() != 5 {
		t.Errorf("Count after 5 appends = %d, want 5 (monotonic, not the capped Len)", ring.Count())
	}
	if ring.Len() != 2 {
		t.Errorf("Len = %d, want 2 (the buffer cap)", ring.Len())
	}
}

// TestLogRingCountDisabled proves a zero-capacity ring (logging buffering
// disabled) reports Count 0 and never grows.
func TestLogRingCountDisabled(t *testing.T) {
	ring := newLogRing(0)
	ring.append(logRecord{Message: "dropped"})
	if ring.Count() != 0 {
		t.Errorf("disabled ring Count = %d, want 0", ring.Count())
	}
}

// TestLogRingSnapshotCountConsistent proves the atomic (records, count) read the
// live tail relies on: SnapshotCount returns the buffered records together with
// the matching monotonic count, equal to Snapshot+Count taken without an
// intervening append, and the count keeps growing past the buffer cap.
func TestLogRingSnapshotCountConsistent(t *testing.T) {
	ring := newLogRing(2) // tiny so it wraps.
	for i := 1; i <= 4; i++ {
		ring.append(logRecord{Message: "m"})
	}
	recs, count := ring.SnapshotCount()
	if count != 4 {
		t.Errorf("SnapshotCount count = %d, want 4 (monotonic total)", count)
	}
	if len(recs) != 2 {
		t.Errorf("SnapshotCount records = %d, want 2 (buffer cap)", len(recs))
	}
	// Consistent with the separate reads (no intervening append).
	if got := len(ring.Snapshot()); got != len(recs) {
		t.Errorf("Snapshot len = %d, SnapshotCount len = %d; should agree", got, len(recs))
	}
	if ring.Count() != count {
		t.Errorf("Count = %d, SnapshotCount count = %d; should agree", ring.Count(), count)
	}

	// Disabled ring yields the empty pair.
	disabled := newLogRing(0)
	r2, c2 := disabled.SnapshotCount()
	if r2 != nil || c2 != 0 {
		t.Errorf("disabled SnapshotCount = (%v, %d), want (nil, 0)", r2, c2)
	}
}

// TestLogRingSourceSnapshotCount proves the cmd adapter forwards SnapshotCount,
// projecting the records and carrying the count through, and is nil-safe.
func TestLogRingSourceSnapshotCount(t *testing.T) {
	ring := newLogRing(4)
	ring.append(logRecord{Message: "first"})
	ring.append(logRecord{Message: "second"})

	src := newLogRingSource(ring)
	recs, count := src.SnapshotCount()
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if len(recs) != 2 || recs[0].Message != "first" || recs[1].Message != "second" {
		t.Errorf("records projected wrong: %+v", recs)
	}

	// Nil-safe.
	nilRecs, nilCount := newLogRingSource(nil).SnapshotCount()
	if nilRecs != nil || nilCount != 0 {
		t.Errorf("nil-ring SnapshotCount = (%v, %d), want (nil, 0)", nilRecs, nilCount)
	}
}

// TestLogRingSourceNil proves the adapter is nil-safe: a nil ring yields an empty,
// never-growing source rather than panicking.
func TestLogRingSourceNil(t *testing.T) {
	src := newLogRingSource(nil)
	if src.Snapshot() != nil {
		t.Errorf("nil-ring Snapshot = %v, want nil", src.Snapshot())
	}
	if src.Count() != 0 {
		t.Errorf("nil-ring Count = %d, want 0", src.Count())
	}
}

// TestLogRingSourceMapsRecords proves the adapter maps a cmd logRecord onto the
// httpapi.LogRecord shape: the unix-nano time becomes a real time.Time, the slog
// level becomes its name, the message and attrs carry through. It uses the real
// logger so the level/attrs match what the ring captures in production.
func TestLogRingSourceMapsRecords(t *testing.T) {
	h, err := newLoggerHandle(config.LogConfig{Level: "debug", Format: "json", Output: filepath.Join(t.TempDir(), "log")})
	if err != nil {
		t.Fatalf("newLoggerHandle: %v", err)
	}
	defer func() { _ = h.Close() }()

	h.Logger.Warn("served", "request_id", "rid-1", "worker", "w7")
	src := newLogRingSource(h.Ring)

	if src.Count() == 0 {
		t.Fatal("Count should reflect the appended record")
	}
	snap := src.Snapshot()
	if len(snap) == 0 {
		t.Fatal("Snapshot should contain the appended record")
	}
	rec := snap[len(snap)-1]
	if rec.Level != "WARN" {
		t.Errorf("level = %q, want WARN (slog.Level.String())", rec.Level)
	}
	if rec.Message != "served" {
		t.Errorf("message = %q, want served", rec.Message)
	}
	if rec.Attrs["request_id"] != "rid-1" || rec.Attrs["worker"] != "w7" {
		t.Errorf("attrs not carried through: %+v", rec.Attrs)
	}
	if rec.Time.IsZero() {
		t.Error("time should be a real instant, not zero")
	}
}

// TestLogRingSourceRedactsSecrets proves AC4 end-to-end: a secret-named attribute
// logged through the real handler is REDACTED in the ring at capture, so the
// adapter (and therefore the admin endpoint that reads it) never sees the
// cleartext — only "[REDACTED]". This is the honest redaction proof: it exercises
// the real capture path, not a pre-masked test value.
func TestLogRingSourceRedactsSecrets(t *testing.T) {
	h, err := newLoggerHandle(config.LogConfig{Level: "debug", Format: "json", Output: filepath.Join(t.TempDir(), "log")})
	if err != nil {
		t.Fatalf("newLoggerHandle: %v", err)
	}
	defer func() { _ = h.Close() }()

	const cleartext = "agpu_supersecret_value"
	h.Logger.Error("auth failure", "token", cleartext, "secret", cleartext, "user", "alice")

	src := newLogRingSource(h.Ring)
	snap := src.Snapshot()
	if len(snap) == 0 {
		t.Fatal("expected a captured record")
	}
	rec := snap[len(snap)-1]
	if rec.Attrs["token"] != redactedPlaceholder {
		t.Errorf("token attr = %v, want %q (redacted at capture)", rec.Attrs["token"], redactedPlaceholder)
	}
	if rec.Attrs["secret"] != redactedPlaceholder {
		t.Errorf("secret attr = %v, want %q (redacted at capture)", rec.Attrs["secret"], redactedPlaceholder)
	}
	// A non-secret attribute is untouched.
	if rec.Attrs["user"] != "alice" {
		t.Errorf("user attr = %v, want alice (non-secret, untouched)", rec.Attrs["user"])
	}
	// The cleartext value is nowhere in the captured attrs.
	for k, v := range rec.Attrs {
		if s, ok := v.(string); ok && s == cleartext {
			t.Fatalf("cleartext secret leaked into ring under attr %q", k)
		}
	}
}
