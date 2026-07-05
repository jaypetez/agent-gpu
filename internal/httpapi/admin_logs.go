package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Log streaming/viewing API (#99): a filterable, cursor-paginated query over the
// in-memory log ring (#90) and an SSE live-tail of new matching lines. Both read
// the ring through the LogSource seam injected by cmd (WithLogSource), so this
// package never depends on the concrete logging setup — the #90 ring is left
// untouched. Every record the ring holds was REDACTED at capture (a secret-named
// attribute is masked to "[REDACTED]" before it ever reaches the buffer), so the
// Attrs map exposed here can never carry secret material; this layer passes it
// through verbatim and never re-inspects it.
//
// Structured fields are exposed as discrete fields (the attrs map), not embedded
// in the message text: a log viewer filters/colorizes on attrs[request_id],
// attrs[session_id], attrs[worker], etc. without parsing the human message.

// LogRecord is one structured log line exposed over the admin log API. It is the
// exported, transport-shaped projection of the cmd-private ring record: Time is a
// real time.Time (the ring stores unix nanos), Level is the slog level name
// (DEBUG/INFO/WARN/ERROR — the adapter calls slog.Level.String()), Message is the
// human message, and Attrs is the resolved, already-redacted attribute map (all
// the slog attributes for the line — request_id/session_id/worker land here when
// present). The adapter in cmd builds these; this package only reads them.
type LogRecord struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs"`
}

// LogSource is the read seam over the in-memory log ring (#90) the admin log
// endpoints (#99) consume. The HTTP layer reads through it rather than depending
// on cmd's concrete ring; the adapter wrapping the ring lives in cmd (mirroring
// the config-applier injection in #92). The records it returns are bounded by the
// ring capacity (drop-oldest), so memory is bounded by construction, and are
// already redacted at capture.
//
//   - Snapshot returns the records currently buffered, oldest-first. The query
//     endpoint uses it.
//   - Count is the monotonic total ever appended (NOT the capped buffer length).
//   - SnapshotCount returns BOTH in one consistent read: the records together with
//     the count that matches them at that instant. The live tail needs this
//     atomicity — deriving "new since my cursor" from a separate Snapshot and Count
//     would race a concurrent append and duplicate or skip lines at the boundary.
//     The count it returns is exactly the total appended when the snapshot was
//     taken, so the cursor math is exact.
//
// Implementations must be safe for concurrent use (the ring is mutex-guarded).
type LogSource interface {
	Snapshot() []LogRecord
	Count() uint64
	SnapshotCount() ([]LogRecord, uint64)
}

// logEntryView is the wire shape of one row in the GET /v1/admin/logs list and of
// each SSE data frame in the live tail. It mirrors LogRecord's fields; the
// explicit projection (rather than serializing LogRecord directly) keeps the wire
// contract local to the handler and matches the projection discipline of the rest
// of the admin API. attrs is always present (an empty map, never null, when the
// line had no structured fields) so a client can index it without a nil guard.
type logEntryView struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs"`
}

// logFilter is the parsed, validated set of GET /v1/admin/logs (and stream) query
// filters. A record passes only when it satisfies EVERY set filter (ANDed): its
// level is at or above minLevel; each non-empty attr filter matches the record's
// corresponding attribute by exact (string-rendered) value; and its timestamp
// falls in the half-open [since, until) window. An attr filter for a key the
// record does not carry simply does not match it (honest and generic — a session
// filter naturally excludes non-session lines).
type logFilter struct {
	minLevel  int               // minimum slog level rank (see levelRank); records below are excluded.
	attrMatch map[string]string // attribute key→required exact value (request_id/session_id/worker).
	since     time.Time         // inclusive lower bound; zero = unbounded.
	until     time.Time         // exclusive upper bound; zero = unbounded.
}

// defaultMinLevelRank is the level floor applied when the client supplies no
// ?level= : WARN. So the default view excludes debug/info noise (AC3) — an
// operator sees warnings and errors unless they explicitly widen the window.
var defaultMinLevelRank = levelRank("warn")

// levelRank maps a level NAME (case-insensitive) onto slog's numeric severity so
// records can be compared by "at least this severe". It mirrors slog's own
// constants (DEBUG=-4, INFO=0, WARN=4, ERROR=8) without importing parseLevel from
// cmd (this package must not depend on cmd). An unrecognized name ranks below
// DEBUG so it is never accidentally filtered OUT by a min-level comparison (a
// record with an odd custom level still shows unless the floor is explicitly set
// above it). slog renders intermediate levels as "WARN+1" etc.; we match on the
// base name, which is sufficient for the debug/info/warn/error vocabulary the
// project emits.
func levelRank(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return -4
	case "info":
		return 0
	case "warn", "warning":
		return 4
	case "error":
		return 8
	default:
		return -8
	}
}

// recordLevelRank ranks a record's level string for the min-level comparison. A
// record's Level is the slog name ("INFO", "WARN", ...); slog can also render an
// offset level ("WARN+1"), so the leading base name is taken before ranking. An
// unrecognized value ranks as ERROR so an oddly-named level is shown under the
// default warn floor rather than silently dropped.
func recordLevelRank(level string) int {
	base := level
	for _, sep := range []string{"+", "-"} {
		if i := strings.Index(base, sep); i > 0 {
			base = base[:i]
			break
		}
	}
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "debug":
		return -4
	case "info":
		return 0
	case "warn", "warning":
		return 4
	case "error":
		return 8
	default:
		return 8
	}
}

// parseLogFilter reads the shared log query filters from the request:
//
//   - level       — minimum level (debug|info|warn|error); default warn, so the
//     default view excludes debug/info noise (AC3). An unrecognized value falls
//     back to the warn default rather than erroring (graceful degradation, like
//     the audit endpoint's bad-bound rule).
//   - request_id  — exact match against attrs[request_id] (every HTTP-handler line).
//   - session_id  — exact match against attrs[session_id] (session-aware lines).
//   - worker      — exact match against attrs[worker] (server-process worker lines).
//   - since/until — unix-seconds half-open window [since, until); inclusive lower,
//     exclusive upper, matching the audit endpoint. An absent/unparseable bound is
//     unbounded on that side.
func parseLogFilter(r *http.Request) logFilter {
	q := r.URL.Query()
	minLevel := defaultMinLevelRank
	if lvl := q.Get("level"); lvl != "" {
		// An unrecognized name ranks as -8 here; treat that as "use the default"
		// rather than "show everything", so a typo does not silently disable the
		// noise floor.
		if r := levelRank(lvl); r >= -4 {
			minLevel = r
		}
	}
	attrMatch := map[string]string{}
	for _, key := range logFilterAttrKeys {
		if v := q.Get(key); v != "" {
			attrMatch[key] = v
		}
	}
	return logFilter{
		minLevel:  minLevel,
		attrMatch: attrMatch,
		since:     parseUnixSeconds(q.Get("since")),
		until:     parseUnixSeconds(q.Get("until")),
	}
}

// logFilterAttrKeys are the attribute keys exposed as discrete query filters. They
// match by exact value against the record's Attrs (a key absent from a record
// never matches, so e.g. a session_id filter excludes non-session lines). Keeping
// the set explicit (rather than accepting an arbitrary attr=... ) bounds the
// surface and documents which structured fields are first-class filters.
var logFilterAttrKeys = []string{"request_id", "session_id", "worker"}

// matches reports whether a record passes every set filter (ANDed): level floor,
// each attr exact-match, and the half-open time window.
func (f logFilter) matches(rec LogRecord) bool {
	if recordLevelRank(rec.Level) < f.minLevel {
		return false
	}
	for key, want := range f.attrMatch {
		got, ok := rec.Attrs[key]
		if !ok || attrString(got) != want {
			return false
		}
	}
	if !f.since.IsZero() && rec.Time.Before(f.since) {
		return false
	}
	if !f.until.IsZero() && !rec.Time.Before(f.until) {
		return false
	}
	return true
}

// attrString renders an attribute value for an exact-match filter comparison.
// The filtered attributes (request_id/session_id/worker) are strings in practice;
// fmt.Sprint gives a stable, allocation-cheap rendering that also matches a
// non-string value formatted the obvious way, so a filter is robust to an
// attribute that was logged as a non-string.
func attrString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// toLogEntryView projects a LogRecord onto its wire shape, normalizing a nil Attrs
// to an empty (non-nil) map so the JSON field is always {} rather than null.
func toLogEntryView(rec LogRecord) logEntryView {
	attrs := rec.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	return logEntryView{
		Time:    rec.Time,
		Level:   rec.Level,
		Message: rec.Message,
		Attrs:   attrs,
	}
}

// handleAdminLogs serves GET /v1/admin/logs (#99): a filterable, cursor-paginated
// page of structured log lines from the in-memory ring (#90), NEWEST FIRST (the
// same ordering as the audit endpoint), in the shared {data,pagination} envelope.
// Filters (level, request_id, session_id, worker, since/until) are ANDed; see
// parseLogFilter. By default — no ?level= — debug/info are excluded (the floor is
// warn), so the default view is warnings and errors only (AC3). Each row exposes
// the structured fields as a discrete attrs map (AC3), never embedded in the
// message.
//
// The records are already redacted at capture (the ring masks secret-named
// attributes before buffering), so they are projected straight through — no
// secret can reach the response (AC4). Memory is bounded by the ring's fixed
// capacity (AC4). Gated to logs:read (s.requireScope): a key lacking it gets 403
// and an unauthenticated request 401 before this runs. When no log source is wired
// (WithLogSource not supplied) the endpoint returns 501, mirroring the nil-quota
// usage endpoint.
func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "log streaming is not enabled")
		return
	}

	filter := parseLogFilter(r)

	// Snapshot is oldest-first; filter, then reverse to newest-first so the page
	// boundary and ordering match the audit endpoint's contract.
	snap := s.logs.Snapshot()
	views := make([]logEntryView, 0, len(snap))
	for _, rec := range snap {
		if filter.matches(rec) {
			views = append(views, toLogEntryView(rec))
		}
	}
	// Newest first. A stable reverse preserves the relative order of records that
	// share a timestamp (the ring is already in append order).
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].Time.After(views[j].Time)
	})

	limit, offset := parsePageParams(r)
	writeList(w, views, limit, offset)
}

// logStreamPollInterval is how often the live-tail handler re-reads the ring for
// new matching records. It trades latency (a new line is delivered within at most
// one interval) against the cost of a snapshot+filter pass; 500ms is responsive
// for an operator watching logs without busy-spinning on the mutex-guarded ring.
const logStreamPollInterval = 500 * time.Millisecond

// logStreamPollOrDefault returns the configured stream poll interval, falling back
// to logStreamPollInterval when unset. The field is injectable ONLY so a test can
// drive the tail fast and deterministically (no real-time sleeps); production never
// sets it, so the live tail always polls at the const interval.
func (s *Server) logStreamPollOrDefault() time.Duration {
	if s.logStreamPoll > 0 {
		return s.logStreamPoll
	}
	return logStreamPollInterval
}

// handleAdminLogsStream serves GET /v1/admin/logs/stream (#99): a live SSE tail of
// NEW log lines matching the same filters as the query endpoint (parseLogFilter).
// It reuses the shared SSE writer (beginSSE/writeSSEData). Tail semantics: the
// cursor starts at the ring's CURRENT Count, so the stream delivers only lines
// emitted AFTER the connection opens (it tails forward; it does not replay the
// existing buffer). Each poll takes ONE consistent (snapshot, count) read
// (emitNewLogs) to learn how many new records arrived since the last tick, takes
// exactly those newest records from the snapshot, emits the ones matching the
// filters as `data:` frames (oldest-first within the tick, so the client sees
// chronological order), and advances the cursor.
//
// Pause/resume without losing position: the cursor is the monotonic Count, which
// the client receives implicitly as the stream position — a client that
// disconnects and reconnects simply resumes tailing new lines from that point
// (there is no server-held per-client state to lose). A client that wants the
// recent buffer first fetches GET /v1/admin/logs, then opens the stream.
//
// Clean disconnect: the loop selects on r.Context().Done(), so when the client
// closes the connection (or the server shuts down) the handler returns promptly —
// no goroutine leak. There is no [DONE] sentinel: the tail is infinite and ends
// only on disconnect. A periodic comment frame keeps the connection alive through
// idle-timeout proxies.
//
// Gated to logs:read; 501 when no log source is wired; 500 if the ResponseWriter
// does not support flushing (should not happen with net/http). The ring's records
// are already redacted (AC4), so frames carry no secret material.
func (s *Server) handleAdminLogsStream(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "log streaming is not enabled")
		return
	}

	filter := parseLogFilter(r)

	flusher, ok := beginSSE(w)
	if !ok {
		// No streaming support behind the ResponseWriter (should not happen with
		// net/http). Fail closed rather than buffering. Headers were not written by
		// beginSSE in this case, so a JSON error + status is still possible.
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	// Start at the current count so the tail shows only NEW lines from here on.
	lastCount := s.logs.Count()

	ticker := time.NewTicker(s.logStreamPollOrDefault())
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected or server shutting down: return so the handler
			// goroutine exits (no leak).
			return
		case <-ticker.C:
			lastCount = s.emitNewLogs(w, flusher, filter, lastCount)
			// A bare comment frame is a no-op for an SSE client but keeps the
			// connection warm through idle proxies; flushed so it actually leaves.
			s.writeSSEComment(w, flusher)
		}
	}
}

// emitNewLogs writes every NEW matching record since lastCount as an SSE data
// frame (oldest-first, so the client sees chronological order) and returns the
// advanced cursor. It is the poll step of the live tail, factored out so it is
// unit-testable against a fake source.
//
// It takes a single CONSISTENT (snapshot, count) read so the records and the count
// that bounds them cannot disagree under a concurrent append. The number of new
// records since the cursor is count-lastCount; it takes that many from the TAIL of
// the (oldest-first) snapshot. When that delta exceeds the buffer length some
// records scrolled off between polls (the producer outran the consumer): the whole
// current buffer is emitted — the best a bounded tail can do — and the cursor still
// advances to the current count so those dropped records are neither re-emitted nor
// re-counted. A delta of zero (no new lines) writes nothing. Because the snapshot
// and count are atomically consistent, the cursor advances to exactly the count
// matching the emitted records, so a line is never duplicated or skipped at the
// boundary.
func (s *Server) emitNewLogs(w http.ResponseWriter, flusher http.Flusher, filter logFilter, lastCount uint64) uint64 {
	snap, count := s.logs.SnapshotCount()
	if count <= lastCount {
		return count
	}
	newN := int(count - lastCount)
	if newN > len(snap) {
		newN = len(snap)
	}
	// The newest newN records are the tail of the oldest-first snapshot.
	for _, rec := range snap[len(snap)-newN:] {
		if filter.matches(rec) {
			writeSSEData(w, flusher, toLogEntryView(rec))
		}
	}
	return count
}

// writeSSEComment writes a single SSE comment line (": \n\n") and flushes it. A
// comment is ignored by every SSE client but proves the connection is alive, so
// an idle tail is not closed by an intermediary's read timeout.
func (s *Server) writeSSEComment(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = w.Write([]byte(": keepalive\n\n"))
	flusher.Flush()
}
