package httpapi

import (
	"net/http"
	"time"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_logs.go is the HTTP wiring for the console's Logs viewer (#103): the page,
// the filtered line-table partial, and a live-tail SSE proxy. All three are gated on
// logs:read by the route (uiScopeAuth), the same scope the JSON GET /v1/admin/logs
// requires, so an authenticated-but-unscoped key gets a 403 HTML page and the "Logs"
// sidebar entry is hidden for a key without it (buildShell). The data is read
// IN-PROCESS via collectLogsTable (the same Snapshot+filter the JSON endpoint uses);
// the live tail reuses the shared SSE machinery (emitNewLogs). Structured fields are
// shown as discrete badges, never embedded in the message. The records are already
// redacted at capture, so no secret can reach the screen.

// handleUILogs serves GET /admin/logs: the logs viewer. It authenticates inline (an
// unauthenticated hit redirects to login) and renders the Logs page for the resolved
// key, reflecting the request's filters into the form (so a deep link like
// ?worker=w1 pre-populates). The filtered line table loads via HTMX after first
// paint; the live tail connects over SSE.
func (s *Server) handleUILogs(w http.ResponseWriter, r *http.Request) {
	token, ok := tokenFromRequest(r)
	if !ok {
		s.redirectToLogin(w, r, "")
		return
	}
	key, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		s.clearSessionCookies(w, r)
		s.redirectToLogin(w, r, "")
		return
	}
	shell := s.buildShell(r, key, webui.SectionLogs, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Logs"},
	})

	query := logQueryString(r)
	streamURL := uiBasePath + "logs/stream"
	csvHref := "/v1/admin/logs?format=csv"
	if query != "" {
		streamURL += "?" + query
		csvHref += "&" + query
	}
	setUIPageHeaders(w)
	_ = webui.Logs(webui.LogsData{
		Shell:     shell,
		Enabled:   s.logs != nil,
		Filter:    logFilterState(r),
		Levels:    []string{"debug", "info", "warn", "error"},
		StreamURL: streamURL,
		CSVHref:   csvHref,
	}).Render(r.Context(), w)
}

// handleUILogsPartial renders the filtered, newest-first line table from one
// in-process snapshot of the ring (the same Snapshot+filter GET /v1/admin/logs
// uses). It is the HTMX partial behind #log-table, gated on logs:read by the route.
// A tightened filter visibly reduces the rows (AC3). The structured fields render as
// discrete badges, never folded into the message.
func (s *Server) handleUILogsPartial(w http.ResponseWriter, r *http.Request) {
	setUIPageHeaders(w)
	_ = webui.LogTablePartial(s.collectLogsTable(r)).Render(r.Context(), w)
}

// handleUILogsStream serves GET /admin/logs/stream: a live SSE tail of NEW log lines
// matching the request's filters, for the viewer's pause/resume live tail. It is a
// thin console-side proxy over the SAME stream machinery the JSON
// /v1/admin/logs/stream uses (emitNewLogs over a consistent snapshot+count read),
// reusing the shared SSE writer. The cookie authenticates it (the route gates it on
// logs:read); it is a GET, so no CSRF token is required. Pause/resume is entirely
// client-side: the page closes and reopens the EventSource, and because the cursor
// is the ring's monotonic Count carried implicitly by the stream position, a
// reconnect simply resumes tailing new lines (there is no server-held per-client
// state to lose). The loop selects on r.Context().Done() so a client disconnect
// returns the handler promptly (no goroutine leak). 501 when no log source is wired.
//
// Each frame is the JSON line shape (logEntryView) the shared writeSSEData emits;
// the page parses it and renders the discrete-field row client-side. Keeping the
// frame identical to the JSON endpoint means the console and an API client tail the
// same wire contract.
func (s *Server) handleUILogsStream(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		s.renderUIError(w, r, http.StatusNotImplemented, "Live log streaming isn't enabled on this server.")
		return
	}

	filter := parseLogFilter(r)

	flusher, ok := beginSSE(w)
	if !ok {
		s.renderUIError(w, r, http.StatusInternalServerError, "Streaming isn't supported here. Try the filtered view instead.")
		return
	}

	// Start at the current count so the tail shows only NEW lines from here on (it
	// tails forward; the initial buffer is the server-rendered table the page already
	// loaded). Reuse the exact poll machinery the JSON stream uses.
	lastCount := s.logs.Count()
	ticker := time.NewTicker(s.logStreamPollOrDefault())
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			lastCount = s.emitNewLogs(w, flusher, filter, lastCount)
			s.writeSSEComment(w, flusher)
		}
	}
}
