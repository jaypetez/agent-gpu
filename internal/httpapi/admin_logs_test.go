package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// ---- test log source ----

// fakeLogSource is an in-memory LogSource for the admin log tests. It mirrors the
// real ring's contract: Snapshot returns the buffered records oldest-first, Count
// is the monotonic total ever appended (so the stream's new-since-cursor math is
// exercised), and both are mutex-guarded so the stream poll loop is race-clean
// while a test appends concurrently. It is NOT bounded (a test never seeds enough
// to need eviction); the bounded-buffer behavior is the ring's own concern, tested
// in cmd.
type fakeLogSource struct {
	mu    sync.Mutex
	recs  []LogRecord
	count uint64
}

func (f *fakeLogSource) Snapshot() []LogRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]LogRecord, len(f.recs))
	copy(out, f.recs)
	return out
}

func (f *fakeLogSource) Count() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// SnapshotCount returns the records and the count atomically under one lock, like
// the real ring, so the stream's cursor math is exercised against a consistent
// pair.
func (f *fakeLogSource) SnapshotCount() ([]LogRecord, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]LogRecord, len(f.recs))
	copy(out, f.recs)
	return out, f.count
}

// add appends one record, advancing the monotonic count (like the ring's append).
func (f *fakeLogSource) add(rec LogRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, rec)
	f.count++
}

// logTestServer builds an httpapi.Server wired to a fakeLogSource (the seam under
// test), a real auth service + authorizer, and a discarding logger. It mirrors
// testServer but attaches the log source so the GET /v1/admin/logs and stream
// endpoints are exercised. The source is returned so a test can seed records.
func logTestServer(t *testing.T) (*Server, *auth.Service, *fakeLogSource) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	src := &fakeLogSource{}
	s := &Server{
		auth:  authSvc,
		authz: az,
		log:   discard,
		logs:  src,
	}
	return s, authSvc, src
}

// logAt builds a fixed UTC instant offset by sec seconds from a stable base, so
// seeded records have deterministic, well-separated timestamps for ordering and
// time-window assertions.
func logAt(sec int) time.Time {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return base.Add(time.Duration(sec) * time.Second)
}

func logUnix(tm time.Time) string { return strconv.FormatInt(tm.Unix(), 10) }

// seedLogRecords appends a fixed, deterministic set of records spanning every
// level and the filterable attributes (request_id/session_id/worker) with
// well-separated timestamps, so the filter and ordering assertions are exact.
func seedLogRecords(src *fakeLogSource) {
	src.add(LogRecord{Time: logAt(10), Level: "DEBUG", Message: "trace tick", Attrs: map[string]any{"request_id": "r1"}})
	src.add(LogRecord{Time: logAt(20), Level: "INFO", Message: "served request", Attrs: map[string]any{"request_id": "r2", "session_id": "s1"}})
	src.add(LogRecord{Time: logAt(30), Level: "WARN", Message: "slow worker", Attrs: map[string]any{"worker": "w1"}})
	src.add(LogRecord{Time: logAt(40), Level: "ERROR", Message: "dispatch failed", Attrs: map[string]any{"request_id": "r2", "worker": "w2"}})
}

// logListView is the decoded list envelope returned by GET /v1/admin/logs.
type logListView struct {
	Data       []logEntryView `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
}

func decodeLogList(t *testing.T, rec *httptest.ResponseRecorder) logListView {
	t.Helper()
	var out logListView
	decode(t, rec, &out)
	return out
}

func messagesOf(views []logEntryView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Message
	}
	return out
}

// ---- GET /v1/admin/logs ----

// TestAdminLogsDefaultExcludesDebugInfo proves AC3: with no ?level= the default
// view is warn+error only — debug and info lines are excluded.
func TestAdminLogsDefaultExcludesDebugInfo(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	seedLogRecords(src)
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	rec := req(t, s, http.MethodGet, "/v1/admin/logs", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out := decodeLogList(t, rec)
	// Newest-first: ERROR(40) then WARN(30). DEBUG(10)/INFO(20) excluded by default.
	if got := messagesOf(out.Data); len(got) != 2 || got[0] != "dispatch failed" || got[1] != "slow worker" {
		t.Fatalf("default view = %v, want [dispatch failed, slow worker] (warn+error, newest-first)", got)
	}
	for _, v := range out.Data {
		if v.Level == "DEBUG" || v.Level == "INFO" {
			t.Errorf("default view leaked a %s line: %+v", v.Level, v)
		}
	}
}

// TestAdminLogsLevelWidens proves AC1/AC3: an explicit level floor widens the view
// — level=debug returns every line, level=info excludes only debug, level=error
// keeps only errors.
func TestAdminLogsLevelWidens(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	seedLogRecords(src)
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	cases := []struct {
		level string
		want  []string // newest-first
	}{
		{"debug", []string{"dispatch failed", "slow worker", "served request", "trace tick"}},
		{"info", []string{"dispatch failed", "slow worker", "served request"}},
		{"warn", []string{"dispatch failed", "slow worker"}},
		{"error", []string{"dispatch failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			rec := req(t, s, http.MethodGet, "/v1/admin/logs?level="+tc.level, token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			out := decodeLogList(t, rec)
			if got := messagesOf(out.Data); !equalStrings(got, tc.want) {
				t.Errorf("level=%s view = %v, want %v", tc.level, got, tc.want)
			}
		})
	}
}

// TestAdminLogsAttrFilters proves AC1: each structured-field filter
// (request_id/session_id/worker) narrows the result by exact attribute match, and
// a line lacking the attribute does not match. level=debug is set so the floor
// does not hide the seeded debug/info lines.
func TestAdminLogsAttrFilters(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	seedLogRecords(src)
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	cases := []struct {
		name  string
		query string
		want  []string // newest-first
	}{
		{"request_id matches two", "?level=debug&request_id=r2", []string{"dispatch failed", "served request"}},
		{"request_id matches one", "?level=debug&request_id=r1", []string{"trace tick"}},
		{"session_id excludes non-session", "?level=debug&session_id=s1", []string{"served request"}},
		{"worker matches one", "?level=debug&worker=w1", []string{"slow worker"}},
		{"combined request_id and worker", "?level=debug&request_id=r2&worker=w2", []string{"dispatch failed"}},
		{"no match", "?level=debug&worker=nobody", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := req(t, s, http.MethodGet, "/v1/admin/logs"+tc.query, token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			out := decodeLogList(t, rec)
			if got := messagesOf(out.Data); !equalStrings(got, tc.want) {
				t.Errorf("query %q = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestAdminLogsTimeWindow proves AC1: since/until bound a half-open [since,until)
// window, matching the audit endpoint's convention, and a garbage bound is treated
// as unbounded rather than an error.
func TestAdminLogsTimeWindow(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	seedLogRecords(src)
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	cases := []struct {
		name  string
		query string
		want  []string // newest-first
	}{
		// since is inclusive: logAt(20) and later (with level=debug to see them all).
		{"since inclusive", "?level=debug&since=" + logUnix(logAt(20)), []string{"dispatch failed", "slow worker", "served request"}},
		// until is exclusive: strictly before logAt(30).
		{"until exclusive", "?level=debug&until=" + logUnix(logAt(30)), []string{"served request", "trace tick"}},
		// Half-open window [20,40): keeps logAt(20),(30).
		{"window", "?level=debug&since=" + logUnix(logAt(20)) + "&until=" + logUnix(logAt(40)), []string{"slow worker", "served request"}},
		// Garbage bound is unbounded, not an error: all four (level=debug).
		{"garbage since unbounded", "?level=debug&since=not-a-number", []string{"dispatch failed", "slow worker", "served request", "trace tick"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := req(t, s, http.MethodGet, "/v1/admin/logs"+tc.query, token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			out := decodeLogList(t, rec)
			if got := messagesOf(out.Data); !equalStrings(got, tc.want) {
				t.Errorf("query %q = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestAdminLogsPagination proves AC1: ?limit= bounds the page and the returned
// cursor follows to the next page, with the newest-first order preserved across
// the boundary. level=debug so all four seeded lines are in play.
func TestAdminLogsPagination(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	seedLogRecords(src)
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	rec := req(t, s, http.MethodGet, "/v1/admin/logs?level=debug&limit=2", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", rec.Code)
	}
	p1 := decodeLogList(t, rec)
	if got := messagesOf(p1.Data); len(got) != 2 || got[0] != "dispatch failed" || got[1] != "slow worker" {
		t.Fatalf("page1 = %v, want [dispatch failed, slow worker]", got)
	}
	if !p1.Pagination.HasMore || p1.Pagination.NextCursor == nil {
		t.Fatalf("page1 should have a next cursor: %+v", p1.Pagination)
	}

	rec = req(t, s, http.MethodGet, "/v1/admin/logs?level=debug&limit=2&cursor="+*p1.Pagination.NextCursor, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page2 status = %d, want 200", rec.Code)
	}
	p2 := decodeLogList(t, rec)
	if got := messagesOf(p2.Data); len(got) != 2 || got[0] != "served request" || got[1] != "trace tick" {
		t.Fatalf("page2 = %v, want [served request, trace tick]", got)
	}
	if p2.Pagination.HasMore || p2.Pagination.NextCursor != nil {
		t.Errorf("page2 should be the last page: %+v", p2.Pagination)
	}
}

// TestAdminLogsAttrsAreDiscreteFields proves AC3: the structured fields are exposed
// as a discrete attrs map (not embedded in the message), and an attrs map is always
// present (never null) even for a line with no attributes.
func TestAdminLogsAttrsAreDiscreteFields(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	src.add(LogRecord{Time: logAt(10), Level: "ERROR", Message: "boom", Attrs: map[string]any{"request_id": "rid-123", "worker": "w9"}})
	src.add(LogRecord{Time: logAt(20), Level: "ERROR", Message: "no attrs line", Attrs: nil})
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	rec := req(t, s, http.MethodGet, "/v1/admin/logs?level=error", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out := decodeLogList(t, rec)
	if len(out.Data) != 2 {
		t.Fatalf("entries = %d, want 2", len(out.Data))
	}
	// Newest-first: index 0 is the no-attrs line, index 1 is the attrs line.
	noAttrs, withAttrs := out.Data[0], out.Data[1]
	if withAttrs.Attrs["request_id"] != "rid-123" || withAttrs.Attrs["worker"] != "w9" {
		t.Errorf("attrs not exposed as discrete fields: %+v", withAttrs.Attrs)
	}
	// The discrete fields are NOT embedded in the message text.
	if strings.Contains(withAttrs.Message, "rid-123") || strings.Contains(withAttrs.Message, "w9") {
		t.Errorf("structured fields leaked into the message: %q", withAttrs.Message)
	}
	// attrs is present as {} (never null) for a line with no attributes.
	if noAttrs.Attrs == nil {
		t.Errorf("attrs should be an empty map, not nil, for an attribute-less line")
	}
	if !strings.Contains(rec.Body.String(), `"attrs":{}`) {
		t.Errorf("attribute-less line should marshal attrs as {}: %s", rec.Body.String())
	}
}

// TestAdminLogsRedactionPassThrough proves AC4 at the HTTP layer: a record whose
// Attrs already carry a redacted secret (the ring redacts at capture) is served
// with the value still "[REDACTED]" and the raw secret never appears in the body.
// The layer neither strips the redaction nor re-introduces the cleartext.
func TestAdminLogsRedactionPassThrough(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	// Simulate what the ring stores: the secret-named attribute is already masked.
	src.add(LogRecord{Time: logAt(10), Level: "ERROR", Message: "auth attempt", Attrs: map[string]any{
		"token":  "[REDACTED]",
		"secret": "[REDACTED]",
		"user":   "alice",
	}})
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	rec := req(t, s, http.MethodGet, "/v1/admin/logs?level=error", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("redacted marker missing from response: %s", body)
	}
	// A raw secret value must never appear (the ring would have masked it; assert the
	// layer does not somehow surface cleartext).
	for _, banned := range []string{"sk-", "supersecret", "agpu_"} {
		if strings.Contains(body, banned) {
			t.Fatalf("response leaked secret material (%q): %s", banned, body)
		}
	}
	out := decodeLogList(t, rec)
	if out.Data[0].Attrs["token"] != "[REDACTED]" || out.Data[0].Attrs["secret"] != "[REDACTED]" {
		t.Errorf("redacted attrs not preserved: %+v", out.Data[0].Attrs)
	}
}

// TestAdminLogsNilSource proves AC: a server without a log source returns 501
// (mirroring the nil-quota usage endpoint), not 500 or a crash.
func TestAdminLogsNilSource(t *testing.T) {
	s, authSvc, _ := logTestServer(t)
	s.logs = nil // simulate an embedder without the log source wired.
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	rec := req(t, s, http.MethodGet, "/v1/admin/logs", token, "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil source status = %d, want 501", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_implemented" {
		t.Errorf("error code = %q, want not_implemented", code)
	}
}

// TestAdminLogsScopeGate proves AC1: a key WITHOUT logs:read (and not admin) is
// 403, a key holding ONLY logs:read passes 200, and an unauthenticated request is
// 401 — the gate runs before the handler.
func TestAdminLogsScopeGate(t *testing.T) {
	s, authSvc, src := logTestServer(t)
	seedLogRecords(src)

	// Unauthenticated → 401.
	rec := req(t, s, http.MethodGet, "/v1/admin/logs", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", rec.Code)
	}

	// Wrong scope → 403.
	otherToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	rec = req(t, s, http.MethodGet, "/v1/admin/logs", otherToken, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("keys:read key status = %d, want 403", rec.Code)
	}
	if code := errorCode(t, rec); code != "forbidden" {
		t.Errorf("error code = %q, want forbidden", code)
	}

	// Exactly logs:read → 200.
	logsToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})
	rec = req(t, s, http.MethodGet, "/v1/admin/logs", logsToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs:read key status = %d, want 200", rec.Code)
	}
}

// TestLogFilterEdgeCases proves the filter helpers handle the awkward inputs:
// a non-string attribute value matched by its rendered form, an offset slog level
// name ("WARN+1"), an unrecognized level name, and an unrecognized query level
// falling back to the warn default.
func TestLogFilterEdgeCases(t *testing.T) {
	// recordLevelRank: an offset level keeps its base rank; an unknown name ranks as
	// error so it survives the default warn floor.
	if got := recordLevelRank("WARN+1"); got != levelRank("warn") {
		t.Errorf("recordLevelRank(WARN+1) = %d, want warn rank %d", got, levelRank("warn"))
	}
	if got := recordLevelRank("CUSTOM"); got != levelRank("error") {
		t.Errorf("recordLevelRank(CUSTOM) = %d, want error rank %d", got, levelRank("error"))
	}

	// A non-string attribute (logged as an int) matches a filter by its rendered
	// form.
	f := logFilter{minLevel: levelRank("debug"), attrMatch: map[string]string{"code": "42"}}
	if !f.matches(LogRecord{Level: "INFO", Attrs: map[string]any{"code": 42}}) {
		t.Error("non-string attr 42 should match filter code=42")
	}
	if f.matches(LogRecord{Level: "INFO", Attrs: map[string]any{"code": 7}}) {
		t.Error("non-string attr 7 should NOT match filter code=42")
	}

	// An unrecognized ?level= falls back to the warn default, not "show everything".
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/logs?level=bogus", nil)
	if got := parseLogFilter(r).minLevel; got != defaultMinLevelRank {
		t.Errorf("bogus level minLevel = %d, want default %d", got, defaultMinLevelRank)
	}

	// The stream poll interval defaults to the const in production (field unset) and
	// honors an injected override (the test path).
	prod := &Server{}
	if got := prod.logStreamPollOrDefault(); got != logStreamPollInterval {
		t.Errorf("default poll = %v, want %v", got, logStreamPollInterval)
	}
	overridden := &Server{logStreamPoll: 5 * time.Millisecond}
	if got := overridden.logStreamPollOrDefault(); got != 5*time.Millisecond {
		t.Errorf("overridden poll = %v, want 5ms", got)
	}
}

// ---- GET /v1/admin/logs/stream (SSE live tail) ----

// flushableRecorder is an httptest.ResponseRecorder that also implements
// http.Flusher (the embedded recorder does not, so beginSSE would otherwise refuse
// to stream). Writes are guarded by a mutex so the test goroutine can read Body
// while the handler goroutine writes, keeping -race clean.
type flushableRecorder struct {
	mu   sync.Mutex
	rec  *httptest.ResponseRecorder
	code int
}

func newFlushableRecorder() *flushableRecorder {
	return &flushableRecorder{rec: httptest.NewRecorder()}
}

func (f *flushableRecorder) Header() http.Header { return f.rec.Header() }

func (f *flushableRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.Write(b)
}

func (f *flushableRecorder) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
	f.rec.WriteHeader(code)
}

func (f *flushableRecorder) Flush() {}

func (f *flushableRecorder) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.Body.String()
}

func (f *flushableRecorder) status() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code
}

// parseSSEData extracts the JSON payloads of each `data:` frame from an SSE body,
// skipping comment (`:`) and blank lines.
func parseSSEData(body string) [][]byte {
	var frames [][]byte
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		frames = append(frames, append([]byte(nil), payload...))
	}
	return frames
}

// TestAdminLogsStreamTailsNewLines proves AC2: the SSE tail delivers NEW matching
// lines emitted after the connection opens, applies the same filters, and — when
// the client disconnects (ctx cancel) — the handler returns promptly with no
// goroutine leak. It seeds a pre-existing line (which must NOT be replayed),
// appends new lines while the handler runs, asserts they arrive as data frames,
// then cancels the context and asserts the handler returns.
func TestAdminLogsStreamTailsNewLines(t *testing.T) {
	s, _, src := logTestServer(t)
	// Drive the tail fast and deterministically (production polls every 500ms).
	s.logStreamPoll = 10 * time.Millisecond
	// A pre-existing line: the tail starts at the current count, so this must NOT
	// be replayed.
	src.add(LogRecord{Time: logAt(1), Level: "ERROR", Message: "old line", Attrs: map[string]any{"worker": "w1"}})

	// Drive the handler directly with a cancelable context. It is called directly
	// (bypassing the auth middleware, whose gate is proven separately in the scope
	// test) so the test controls the request context for the disconnect assertion.
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/logs/stream?level=warn&worker=w1", nil).WithContext(ctx)
	w := newFlushableRecorder()

	done := make(chan struct{})
	go func() {
		s.handleAdminLogsStream(w, r)
		close(done)
	}()

	// Wait for the first keepalive comment frame. It is written only AFTER the loop
	// has started — i.e. after the handler captured its starting cursor at the
	// pre-existing count — so appending now is race-free: the new lines are
	// guaranteed to count as "new" relative to that cursor.
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.body(), ": keepalive")
	})

	// Append new lines after the stream has started. Two match the filter
	// (level>=warn AND worker=w1); one is filtered out (info) and one by worker.
	src.add(LogRecord{Time: logAt(10), Level: "WARN", Message: "new warn one", Attrs: map[string]any{"worker": "w1"}})
	src.add(LogRecord{Time: logAt(11), Level: "INFO", Message: "filtered by level", Attrs: map[string]any{"worker": "w1"}})
	src.add(LogRecord{Time: logAt(12), Level: "ERROR", Message: "filtered by worker", Attrs: map[string]any{"worker": "w2"}})
	src.add(LogRecord{Time: logAt(13), Level: "ERROR", Message: "new error two", Attrs: map[string]any{"worker": "w1"}})

	// Wait until both matching frames have arrived (poll the body across ticks),
	// bounded so a regression fails fast rather than hanging.
	waitFor(t, 5*time.Second, func() bool {
		return len(parseSSEData(w.body())) >= 2
	})

	frames := parseSSEData(w.body())
	got := make([]string, 0, len(frames))
	for _, f := range frames {
		var v logEntryView
		if err := json.Unmarshal(f, &v); err != nil {
			t.Fatalf("decode frame %q: %v", f, err)
		}
		got = append(got, v.Message)
	}
	// Chronological order within the tail; only the two matching lines, never the
	// pre-existing "old line" (not replayed) nor the filtered-out lines.
	if !equalStrings(got, []string{"new warn one", "new error two"}) {
		t.Fatalf("streamed messages = %v, want [new warn one, new error two]", got)
	}
	body := w.body()
	if strings.Contains(body, "old line") {
		t.Errorf("pre-existing line was replayed (tail must start at the live position): %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("infinite tail must not emit a [DONE] sentinel: %s", body)
	}

	// Disconnect: cancel the context and assert the handler returns promptly.
	cancel()
	select {
	case <-done:
		// handler returned — no goroutine leak.
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after context cancel (goroutine leak)")
	}

	// The SSE headers were written.
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
}

// TestAdminLogsStreamNilSource proves the stream endpoint 501s when no log source
// is wired (before any SSE header is written), mirroring the query endpoint.
func TestAdminLogsStreamNilSource(t *testing.T) {
	s, authSvc, _ := logTestServer(t)
	s.logs = nil
	token := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeLogsRead}})

	// httptest.NewRecorder supports Flush, so the 501 here is the nil-source guard,
	// not a missing-flusher fallback.
	rec := req(t, s, http.MethodGet, "/v1/admin/logs/stream", token, "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil source stream status = %d, want 501", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_implemented" {
		t.Errorf("error code = %q, want not_implemented", code)
	}
}

// TestAdminLogsStreamUnsupportedFlush proves the stream endpoint fails closed with
// 500 when the ResponseWriter cannot flush (no http.Flusher), rather than
// buffering. It calls the handler with a non-flushing writer directly.
func TestAdminLogsStreamUnsupportedFlush(t *testing.T) {
	s, _, src := logTestServer(t)
	seedLogRecords(src)

	r := httptest.NewRequest(http.MethodGet, "/v1/admin/logs/stream", nil)
	w := &nonFlushingWriter{header: http.Header{}}
	s.handleAdminLogsStream(w, r)
	if w.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no flusher)", w.status)
	}
}

// nonFlushingWriter is an http.ResponseWriter WITHOUT a Flush method, so beginSSE
// reports the writer cannot stream.
type nonFlushingWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (w *nonFlushingWriter) Header() http.Header         { return w.header }
func (w *nonFlushingWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *nonFlushingWriter) WriteHeader(code int)        { w.status = code }

// boundedLogSource models the real ring's bounded, drop-oldest buffer: Snapshot
// returns at most cap records (the newest), while Count keeps growing. It lets the
// emitNewLogs overflow branch (more new records than the buffer holds) be tested
// deterministically.
type boundedLogSource struct {
	mu    sync.Mutex
	recs  []LogRecord
	count uint64
	cap   int
}

func (b *boundedLogSource) Snapshot() []LogRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogRecord, len(b.recs))
	copy(out, b.recs)
	return out
}

func (b *boundedLogSource) Count() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

func (b *boundedLogSource) SnapshotCount() ([]LogRecord, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogRecord, len(b.recs))
	copy(out, b.recs)
	return out, b.count
}

func (b *boundedLogSource) add(rec LogRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recs = append(b.recs, rec)
	if len(b.recs) > b.cap {
		b.recs = b.recs[len(b.recs)-b.cap:]
	}
	b.count++
}

// TestEmitNewLogsOverflowEmitsCurrentBuffer proves the documented overflow branch:
// when more records were appended since the cursor than the buffer can hold (the
// producer outran the consumer), emitNewLogs emits the whole current buffer (the
// best a bounded tail can do) and advances the cursor to the current count so the
// dropped records are not re-counted.
func TestEmitNewLogsOverflowEmitsCurrentBuffer(t *testing.T) {
	s, _, _ := logTestServer(t)
	src := &boundedLogSource{cap: 2}
	s.logs = src
	// Append 5 records into a 2-slot buffer: 3 scrolled off. The buffer now holds
	// the two newest.
	for i := 1; i <= 5; i++ {
		src.add(LogRecord{Time: logAt(i), Level: "ERROR", Message: "m" + strconv.Itoa(i)})
	}

	w := newFlushableRecorder()
	// Cursor at 0 (as if the stream started before any append); delta is 5 but the
	// buffer holds 2, so only the two newest are emitted.
	next := s.emitNewLogs(w, w, logFilter{minLevel: levelRank("debug")}, 0)
	if next != 5 {
		t.Errorf("cursor advanced to %d, want 5 (current count)", next)
	}
	got := make([]string, 0)
	for _, f := range parseSSEData(w.body()) {
		var v logEntryView
		if err := json.Unmarshal(f, &v); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		got = append(got, v.Message)
	}
	if !equalStrings(got, []string{"m4", "m5"}) {
		t.Errorf("emitted = %v, want [m4 m5] (the current buffer, oldest-first)", got)
	}
}

// ---- small test helpers local to this file ----

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitFor polls cond until it is true or the timeout elapses, failing the test on
// timeout. The short sleep keeps the spin cheap without a wall-clock dependency
// beyond the handler's own poll tick.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}
