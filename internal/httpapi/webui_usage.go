package httpapi

import (
	"net/http"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_usage.go is the HTTP wiring for the console's Usage screen (#103). It
// mirrors the dashboard/workers handlers exactly: the page handler authenticates,
// builds the role-gated shell, and renders a full templ in @Shell; the partial
// handler renders just the usage board fragment for HTMX to swap. Both reads are
// gated on telemetry:read by the route (uiScopeAuth), the same scope the JSON GET
// /v1/admin/usage requires, so an authenticated-but-unscoped key gets a 403 HTML
// page and the "Usage" sidebar entry is hidden for a key without it (buildShell).
// The data is read IN-PROCESS via collectUsage, which reuses the same per-key
// projection the JSON endpoint builds — never an internal HTTP call.

// handleUIUsage serves GET /admin/usage: the usage screen. It authenticates inline
// (an unauthenticated hit redirects to login) and renders the Usage page for the
// resolved key. The per-key meters + sparklines load via HTMX after first paint
// from the usage partial.
func (s *Server) handleUIUsage(w http.ResponseWriter, r *http.Request) {
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
	shell := s.buildShell(r, key, webui.SectionUsage, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Usage"},
	})
	setUIPageHeaders(w)
	_ = webui.Usage(webui.UsageData{Shell: shell}).Render(r.Context(), w)
}

// handleUIUsagePartial renders the usage board fragment from one in-process pull of
// the live quota snapshots (the same projection GET /v1/admin/usage builds). It is
// the HTMX partial behind #usage-board, gated on telemetry:read by the route. When
// the quota engine is not wired the board renders a clear "usage reporting is off"
// notice rather than empty bars.
func (s *Server) handleUIUsagePartial(w http.ResponseWriter, r *http.Request) {
	setUIPageHeaders(w)
	_ = webui.UsageBoardPartial(assetPath(), s.collectUsage(r.Context())).Render(r.Context(), w)
}
