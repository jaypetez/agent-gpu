package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// The in-memory log ring buffer (#90 foundation for #92/#99). It is a bounded,
// fixed-capacity, drop-oldest ring of recently-emitted log records, fed by a
// slog.Handler that wraps the real (JSON/text) handler: every record the process
// logs is BOTH written to the configured sink AND appended to the ring. A later
// issue (#92/#99) exposes the ring over an admin endpoint and flips the dynamic
// log level; here we only build the plumbing — there is NO HTTP endpoint yet.
//
// Redaction is preserved: the ring stores the already-redacted attributes (the
// wrapped handler's ReplaceAttr runs on the record before we snapshot it is not
// possible — ReplaceAttr runs inside the terminal handler — so the ring captures
// attrs directly and applies the SAME redactAttr to each, guaranteeing no secret
// reaches the buffer even though it bypasses the terminal handler's ReplaceAttr).

// logRecord is a flattened, retained copy of a slog.Record for the ring. slog
// forbids retaining a Record across the Handle call (its attrs may be reused), so
// the ring copies the message, level, time, and resolved attributes out.
type logRecord struct {
	Time    int64          // Unix nanoseconds.
	Level   slog.Level     //
	Message string         //
	Attrs   map[string]any // Resolved, redacted attribute key→value.
}

// logRing is a bounded ring buffer of the most recent log records, safe for
// concurrent use. On overflow the oldest record is overwritten (a rolling
// window). A non-positive capacity disables buffering (Snapshot returns nil).
type logRing struct {
	mu    sync.Mutex
	buf   []logRecord
	next  int  // index to write the next record.
	full  bool // whether the ring has wrapped at least once.
	cap   int
	count int // total records ever appended (for tests/metrics).
}

// newLogRing returns a ring holding the most recent capacity records (a
// non-positive capacity disables buffering).
func newLogRing(capacity int) *logRing {
	if capacity <= 0 {
		return &logRing{cap: 0}
	}
	return &logRing{buf: make([]logRecord, capacity), cap: capacity}
}

// append adds a record, overwriting the oldest when the ring is full.
func (r *logRing) append(rec logRecord) {
	if r.cap == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = rec
	r.next = (r.next + 1) % r.cap
	if r.next == 0 {
		r.full = true
	}
	r.count++
}

// Snapshot returns a copy of the buffered records, oldest first. It is the read
// seam the admin log query endpoint (#99) uses; here it backs the unit tests.
func (r *logRing) Snapshot() []logRecord {
	if r.cap == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// SnapshotCount returns the buffered records (oldest first) AND the monotonic
// total-appended count in ONE locked read, so the two are mutually consistent: the
// records are exactly the newest min(count, cap) ever appended at the instant of
// the call. The live-tail stream (#99) needs this atomicity — computing "new since
// my cursor" from a SEPARATE Snapshot and Count call would race a concurrent
// append (the snapshot and the count could reflect different totals), which would
// duplicate or skip lines at the page boundary. Snapshot and Count remain
// available for callers that need only one. Safe for concurrent use.
func (r *logRing) SnapshotCount() ([]logRecord, uint64) {
	if r.cap == 0 {
		return nil, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked(), uint64(r.count)
}

// snapshotLocked copies the buffered records oldest-first. The caller must hold mu.
func (r *logRing) snapshotLocked() []logRecord {
	n := r.next
	out := make([]logRecord, 0, r.cap)
	if r.full {
		// Oldest is at r.next; read from there wrapping around.
		for i := 0; i < r.cap; i++ {
			out = append(out, r.buf[(n+i)%r.cap])
		}
	} else {
		out = append(out, r.buf[:n]...)
	}
	return out
}

// Len returns the number of records currently buffered (<= capacity).
func (r *logRing) Len() int {
	if r.cap == 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return r.cap
	}
	return r.next
}

// Count returns the total number of records ever appended to the ring — a
// monotonically increasing counter that never resets, NOT the (capped) number
// currently buffered (that is Len). It is the cursor the live-tail stream
// endpoint (#99) tracks: by remembering a prior Count and re-reading it each
// poll, the stream learns how many new records arrived (and, when the delta
// exceeds the buffer capacity, that some scrolled off). A disabled ring (zero
// capacity) never appends, so its Count is always 0. Safe for concurrent use
// (read under the same mutex append takes).
func (r *logRing) Count() uint64 {
	if r.cap == 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return uint64(r.count)
}

// ringHandler is a slog.Handler that forwards every record to a wrapped terminal
// handler AND appends a redacted copy to a logRing. It composes transparently:
// WithAttrs/WithGroup return a ringHandler over the wrapped handler's derived
// handler, carrying the accumulated attrs/groups so a record handled through a
// logger built with .With(...) captures those attrs in the ring too.
type ringHandler struct {
	wrapped slog.Handler
	ring    *logRing
	// attrs are the attributes accumulated via WithAttrs (already redacted), and
	// groups the open group names via WithGroup, so the ring copy mirrors what the
	// terminal handler would emit.
	attrs  []slog.Attr
	groups []string
}

// newRingHandler wraps terminal so every record it handles is also appended to
// ring. Redaction is applied to each captured attribute via redactAttr so the
// ring can never hold secret material even though it does not run the terminal
// handler's ReplaceAttr.
func newRingHandler(terminal slog.Handler, ring *logRing) *ringHandler {
	return &ringHandler{wrapped: terminal, ring: ring}
}

// Enabled defers to the wrapped handler (so the dynamic LevelVar still governs
// what is emitted AND buffered — a record filtered out is never rung).
func (h *ringHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.wrapped.Enabled(ctx, level)
}

// Handle forwards to the wrapped handler, then appends a redacted copy to the
// ring. A wrapped-handler error is returned as-is (the ring append is
// best-effort and never errors).
func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	err := h.wrapped.Handle(ctx, rec)

	if h.ring != nil && h.ring.cap > 0 {
		attrs := make(map[string]any, rec.NumAttrs()+len(h.attrs))
		// Pre-bound attrs (via WithAttrs) belong under any currently-open group;
		// record attrs are emitted under the same open group, matching slog's own
		// nesting so the ring's keys mirror the terminal handler's output.
		groupPrefix := strings.Join(h.groups, ".")
		for _, a := range h.attrs {
			h.collectPrefixed(attrs, groupPrefix, a)
		}
		rec.Attrs(func(a slog.Attr) bool {
			h.collectPrefixed(attrs, groupPrefix, a)
			return true
		})
		h.ring.append(logRecord{
			Time:    rec.Time.UnixNano(),
			Level:   rec.Level,
			Message: rec.Message,
			Attrs:   attrs,
		})
	}
	return err
}

// collectPrefixed resolves one attribute into the map under a dotted prefix,
// redacting any secret by its LEAF key name (matching the terminal handler's
// ReplaceAttr, which slog calls with the leaf key regardless of group nesting).
// A group attribute recurses with the group name appended to the prefix, so a
// nested credential group's secret leaf is still redacted by name.
func (h *ringHandler) collectPrefixed(dst map[string]any, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		next := prefix
		if a.Key != "" {
			if next == "" {
				next = a.Key
			} else {
				next += "." + a.Key
			}
		}
		for _, ga := range a.Value.Group() {
			h.collectPrefixed(dst, next, ga)
		}
		return
	}
	if a.Key == "" {
		return
	}
	// Redact by leaf key (a.Key), exactly as the terminal handler's ReplaceAttr
	// does, so the ring can never hold a secret value even though it bypasses the
	// handler's ReplaceAttr.
	redacted := redactAttr(nil, a)
	key := a.Key
	if prefix != "" {
		key = prefix + "." + a.Key
	}
	dst[key] = redacted.Value.Any()
}

// WithAttrs returns a handler that adds attrs to both the wrapped handler and the
// ring's captured attrs. The attrs are retained raw and redacted at capture time
// in collect (by leaf key), so the ring never holds a secret value.
func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ringHandler{
		wrapped: h.wrapped.WithAttrs(attrs),
		ring:    h.ring,
		attrs:   append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups:  h.groups,
	}
}

// WithGroup returns a handler opening the named group on both the wrapped handler
// and the ring's group stack.
func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{
		wrapped: h.wrapped.WithGroup(name),
		ring:    h.ring,
		attrs:   h.attrs,
		groups:  append(append([]string(nil), h.groups...), name),
	}
}

// Compile-time assertion that ringHandler satisfies slog.Handler.
var _ slog.Handler = (*ringHandler)(nil)
