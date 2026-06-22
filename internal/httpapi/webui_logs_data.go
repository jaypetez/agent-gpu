package httpapi

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_logs_data.go maps the in-memory log ring onto the console's Logs
// view-models (#103). It reuses the SAME read seam + filter the JSON GET
// /v1/admin/logs uses — parseLogFilter for the query, s.logs.Snapshot() for the
// records, and filter.matches for the predicate — so the console and the API filter
// identically. Structured fields are exposed as DISCRETE badges (the attrs map),
// never embedded in the human message. The records are already redacted at capture
// (the ring masks secret-named attributes before buffering), so they are projected
// straight through and no secret can reach the screen.

// primaryLogFields are the first-class filter attributes, emphasized as primary
// badges on a line (the rest of a line's attrs render as secondary badges). It
// mirrors logFilterAttrKeys so the badges and the filters name the same fields.
var primaryLogFields = map[string]bool{"request_id": true, "session_id": true, "worker": true}

// collectLogsTable builds the filtered, newest-first line table for the logs
// partial from one consistent snapshot of the ring, applying the request's filters
// (the same parseLogFilter the JSON endpoint uses). The count is the number of lines
// AFTER filtering, so a tightened filter visibly reduces the table (AC3). When no
// log source is wired the table is empty (the handler/page gate Enabled separately).
func (s *Server) collectLogsTable(r *http.Request) webui.LogsTable {
	if s.logs == nil {
		return webui.LogsTable{}
	}
	filter := parseLogFilter(r)
	snap := s.logs.Snapshot()
	lines := make([]webui.LogLine, 0, len(snap))
	for _, rec := range snap {
		if filter.matches(rec) {
			lines = append(lines, newLogLine(rec))
		}
	}
	// Newest first, stable so same-timestamp lines keep ring order.
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time > lines[j].Time })
	return webui.LogsTable{Lines: lines, Count: len(lines)}
}

// newLogLine projects one log record into a viewer row: the time, the level + tone,
// the human message, and the structured fields as discrete sorted badges (the
// first-class filter fields emphasized as primary). The attrs are rendered as
// key=value, never folded into the message.
func newLogLine(rec LogRecord) webui.LogLine {
	fields := make([]webui.LogField, 0, len(rec.Attrs))
	for k, v := range rec.Attrs {
		fields = append(fields, webui.LogField{
			Key:     k,
			Value:   attrString(v),
			Primary: primaryLogFields[k],
		})
	}
	// Primary fields first, then alphabetical within each group, for a stable,
	// scannable order.
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].Primary != fields[j].Primary {
			return fields[i].Primary
		}
		return fields[i].Key < fields[j].Key
	})
	return webui.LogLine{
		Time:    rec.Time.UTC().Format("15:04:05"),
		Level:   rec.Level,
		Tone:    levelTone(rec.Level),
		Message: rec.Message,
		Fields:  fields,
	}
}

// logFilterState reflects the request's filters back into the form model (#103) so
// the viewer shows what is being filtered (and a deep link like ?worker=w1
// pre-populates). The level defaults to "warn" to match the JSON endpoint's noise
// floor; the time bounds are rendered as datetime-local strings for the inputs.
func logFilterState(r *http.Request) webui.LogFilterState {
	q := r.URL.Query()
	level := strings.ToLower(strings.TrimSpace(q.Get("level")))
	if level == "" {
		level = "warn"
	}
	return webui.LogFilterState{
		Level:     level,
		RequestID: q.Get("request_id"),
		SessionID: q.Get("session_id"),
		Worker:    q.Get("worker"),
		Since:     toDatetimeLocal(parseUnixSeconds(q.Get("since"))),
		Until:     toDatetimeLocal(parseUnixSeconds(q.Get("until"))),
	}
}

// logQueryString builds the canonical query string for the active filters, used to
// thread them onto the SSE stream URL and the CSV export link so the live tail and
// the download honor exactly what the viewer filtered. Empty fields are omitted.
func logQueryString(r *http.Request) string {
	src := r.URL.Query()
	out := url.Values{}
	for _, key := range []string{"level", "request_id", "session_id", "worker", "since", "until"} {
		if v := strings.TrimSpace(src.Get(key)); v != "" {
			out.Set(key, v)
		}
	}
	return out.Encode()
}

// toDatetimeLocal renders a time as the "2006-01-02T15:04" form an <input
// type=datetime-local> expects, or "" for a zero time (an unset bound). The ring's
// timestamps are UTC; the input is shown in UTC for determinism (the operator filters
// against the same clock the server stamps).
func toDatetimeLocal(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04")
}
