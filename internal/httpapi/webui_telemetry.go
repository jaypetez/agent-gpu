package httpapi

import (
	"net/http"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_telemetry.go is the HTTP wiring for the console's Telemetry dashboard
// (#103). It mirrors the dashboard handlers: the page handler authenticates, builds
// the role-gated shell, and renders a full templ in @Shell; the partial handler
// renders just the telemetry board fragment for HTMX to swap. Both reads are gated
// on telemetry:read by the route (uiScopeAuth), the same scope the JSON GET
// /v1/admin/telemetry requires. The data is read IN-PROCESS via collectTelemetry,
// which reads the same collectors the JSON endpoint reads — never an internal HTTP
// call. The board refreshes on a calm cadence (slower than the Overview health
// strip, since these are roll-ups), so only the data region updates.

// handleUITelemetry serves GET /admin/telemetry: the telemetry dashboard. It
// authenticates inline (an unauthenticated hit redirects to login) and renders the
// Telemetry page for the resolved key. The live metrics load via HTMX after first
// paint from the telemetry partial.
func (s *Server) handleUITelemetry(w http.ResponseWriter, r *http.Request) {
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
	shell := s.buildShell(r, key, webui.SectionTelemetry, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Telemetry"},
	})
	setUIPageHeaders(w)
	_ = webui.Telemetry(webui.TelemetryData{Shell: shell}).Render(r.Context(), w)
}

// handleUITelemetryPartial renders the telemetry board fragment from one in-process
// pull of the live collectors (the same data GET /v1/admin/telemetry serves). It is
// the HTMX partial behind #telemetry-board, gated on telemetry:read by the route.
func (s *Server) handleUITelemetryPartial(w http.ResponseWriter, r *http.Request) {
	setUIPageHeaders(w)
	_ = webui.TelemetryBoardPartial(assetPath(), s.collectTelemetry()).Render(r.Context(), w)
}
