package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/server"
)

// webui_workers.go is the HTTP wiring for the console's Workers + GPU management
// screens (#101). It mirrors the dashboard handlers in webui.go exactly: page
// handlers authenticate, build the role-gated shell, and render a full templ in
// @Shell; partial handlers render just their fragment for HTMX to swap. The write
// handlers (drain / force-evict / pull / unload) are the console's first
// state-changing surface beyond login/logout, so each one:
//
//   - calls s.csrfOK(r) FIRST and refuses with 403 on failure (the double-submit
//     CSRF defense, same as handleUILogout / handleUILoginSubmit); HTMX requests
//     carry the token in the X-CSRF-Token header inherited from the body's
//     hx-headers, so a legitimate console action always passes;
//   - calls the SAME in-process fleet method the JSON admin endpoint uses
//     (DrainWorkerWithDeadline / AdminPullModel / AdminUnloadModel), so the
//     console and the API drive the control plane identically;
//   - records exactly one audit entry via s.recordAudit, reusing the SAME op
//     constants as the JSON handlers (worker.drain / model.pull / model.unload;
//     worker.evict for the forced eviction), so a console-initiated change is
//     indistinguishable in the audit trail from an API-initiated one; and
//   - returns a toast fragment (WorkerActionToast) HTMX appends to #toasts, with
//     status conveyed by tone AND text.
//
// The routes themselves are registered in registerUIRoutes (webui.go), gated by
// s.uiScopeAuth on workers:read (reads), workers:write (drain/evict), or
// models:write (pull/unload) — the same scopes the JSON admin routes require — so
// an authenticated-but-unscoped key gets a 403 HTML page, never a redirect loop.

// handleUIWorkers serves GET /admin/workers: the fleet screen. It authenticates
// inline (an unauthenticated hit redirects to login) and renders the Workers page
// for the resolved key. The live heatmap + worker list load via HTMX after first
// paint from their partials.
func (s *Server) handleUIWorkers(w http.ResponseWriter, r *http.Request) {
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
	shell := s.buildShell(r, key, webui.SectionWorkers, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Workers"},
	})
	setUIPageHeaders(w)
	_ = webui.Workers(webui.WorkersData{Shell: shell}).Render(r.Context(), w)
}

// handleUIWorkerDetail serves GET /admin/workers/{id}: one worker's detail screen.
// It resolves the worker from the in-process fleet (WorkerByID, the same accessor
// GET /v1/admin/workers/{id} uses); a worker that is not connected renders the
// not-found body inside the shell with a 404 status (a browser expects HTML, not
// the API's JSON envelope).
func (s *Server) handleUIWorkerDetail(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	detail, found := s.collectWorkerDetail(id)
	if !found {
		shell := s.buildShell(r, key, webui.SectionWorkers, []webui.Crumb{
			{Label: "Console", Href: uiBasePath},
			{Label: "Workers", Href: uiBasePath + "workers"},
			{Label: "Not found"},
		})
		setUIPageHeaders(w)
		w.WriteHeader(http.StatusNotFound)
		_ = webui.WorkerNotFound(shell).Render(r.Context(), w)
		return
	}
	shell := s.buildShell(r, key, webui.SectionWorkers, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Workers", Href: uiBasePath + "workers"},
		{Label: shortIDForCrumb(id)},
	})
	setUIPageHeaders(w)
	_ = webui.WorkerDetailPage(detail, shell).Render(r.Context(), w)
}

// handleUIHeatmapPartial renders the GPU heatmap fragment from one live fleet
// snapshot (the same aggregateGPUs reducer behind GET /v1/admin/gpus). It is the
// HTMX partial behind #gpu-heatmap, gated by uiScopeAuth on workers:read.
func (s *Server) handleUIHeatmapPartial(w http.ResponseWriter, r *http.Request) {
	setUIPageHeaders(w)
	_ = webui.Heatmap(assetPath(), s.collectHeatmap()).Render(r.Context(), w)
}

// handleUIWorkerListPartial renders the live worker-list fragment from the fleet
// snapshot. It is the HTMX partial behind #worker-list, gated on workers:read.
func (s *Server) handleUIWorkerListPartial(w http.ResponseWriter, r *http.Request) {
	setUIPageHeaders(w)
	_ = webui.WorkerListPartial(assetPath(), s.collectWorkerList()).Render(r.Context(), w)
}

// handleUIWorkerDrain serves POST /admin/workers/{id}/drain: a soft drain (no new
// jobs; in-flight jobs finish). CSRF-checked first, then it calls the same
// DrainWorkerWithDeadline(id, 0) the JSON drain uses for a pure soft drain, audits
// under worker.drain, and returns a toast. Gated on workers:write by the route.
func (s *Server) handleUIWorkerDrain(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.fleet.DrainWorkerWithDeadline(id, 0); err != nil {
		s.recordAudit(r, auditOpWorkerDrain, id, audit.OutcomeFailure, nil, nil)
		s.renderWorkerActionError(w, r, "drain", id, err)
		return
	}
	s.recordAudit(r, auditOpWorkerDrain, id, audit.OutcomeSuccess, nil,
		audit.RedactedValues{"status": "draining"})
	s.renderWorkerActionToast(w, r, webui.WorkerActionResult{
		Tone:    webui.ToneOK,
		Title:   "Draining " + shortIDForCrumb(id),
		Message: "No new jobs will be dispatched; in-flight jobs finish first.",
	})
}

// handleUIWorkerEvict serves POST /admin/workers/{id}/evict: a forced eviction.
// It reuses the timed-drain path with the smallest positive deadline, so the
// worker is evicted as soon as its in-flight jobs reach zero (immediately if it is
// idle) — the same DrainWorkerWithDeadline the JSON forced-drain uses, just with
// the deadline pinned to "now". The typed-name confirm that gates this in the UI
// is a client concern (Alpine); the handler still enforces CSRF + workers:write.
// Audited under worker.evict so a forced eviction is distinguishable from a soft
// drain in the trail.
func (s *Server) handleUIWorkerEvict(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	// A 1ns deadline means "evict the moment in-flight jobs clear" — the forced
	// eviction the UI's high-friction control promises, vs. the soft drain above.
	if err := s.fleet.DrainWorkerWithDeadline(id, time.Nanosecond); err != nil {
		s.recordAudit(r, auditOpWorkerEvict, id, audit.OutcomeFailure, nil, nil)
		s.renderWorkerActionError(w, r, "evict", id, err)
		return
	}
	s.recordAudit(r, auditOpWorkerEvict, id, audit.OutcomeSuccess, nil,
		audit.RedactedValues{"status": "draining", "forced": true})
	s.renderWorkerActionToast(w, r, webui.WorkerActionResult{
		Tone:    webui.ToneWarn,
		Title:   "Evicting " + shortIDForCrumb(id),
		Message: "The worker is being disconnected. It will drop from the fleet shortly.",
	})
}

// handleUIWorkerPull serves POST /admin/workers/{id}/models: dispatch a model pull
// to the worker. CSRF-checked first; the model name comes from the posted form.
// It calls the same AdminPullModel the JSON endpoint uses, audits under model.pull
// with the worker/model target, and returns a toast. Gated on models:write.
func (s *Server) handleUIWorkerPull(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.renderWorkerActionToastStatus(w, r, http.StatusBadRequest, webui.WorkerActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Couldn't read that",
			Message: "The form was malformed. Reload the page and try again.",
		})
		return
	}
	model := r.PostFormValue("model")
	if model == "" {
		s.renderWorkerActionToastStatus(w, r, http.StatusBadRequest, webui.WorkerActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Model is required",
			Message: "Enter a model name to pull, e.g. llama3.",
		})
		return
	}
	target := id + "/" + model
	if err := s.fleet.AdminPullModel(r.Context(), id, model); err != nil {
		s.recordAudit(r, auditOpModelPull, target, audit.OutcomeFailure, nil, nil)
		s.renderWorkerActionError(w, r, "pull", id, err)
		return
	}
	s.recordAudit(r, auditOpModelPull, target, audit.OutcomeSuccess, nil,
		audit.RedactedValues{"worker": id, "model": model})
	s.renderWorkerActionToast(w, r, webui.WorkerActionResult{
		Tone:    webui.ToneInfo,
		Title:   "Pulling " + model,
		Message: "The model is downloading on the worker; it appears after the next heartbeat.",
	})
}

// handleUIWorkerUnload serves DELETE /admin/workers/{id}/models/{model}: unload a
// model from the worker. CSRF-checked first; it calls the same AdminUnloadModel
// the JSON endpoint uses, audits under model.unload, and returns a toast. Gated on
// models:write.
func (s *Server) handleUIWorkerUnload(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	model := r.PathValue("model")
	target := id + "/" + model
	if err := s.fleet.AdminUnloadModel(r.Context(), id, model); err != nil {
		s.recordAudit(r, auditOpModelUnload, target, audit.OutcomeFailure, nil, nil)
		s.renderWorkerActionError(w, r, "unload", id, err)
		return
	}
	s.recordAudit(r, auditOpModelUnload, target, audit.OutcomeSuccess, nil,
		audit.RedactedValues{"worker": id, "model": model})
	s.renderWorkerActionToast(w, r, webui.WorkerActionResult{
		Tone:    webui.ToneOK,
		Title:   "Unloaded " + model,
		Message: "The model was unloaded from the worker.",
	})
}

// --- write-handler helpers --------------------------------------------------

// uiWriteGuard is the shared front-gate for every state-changing console handler:
// it enforces the double-submit CSRF check and, on failure, writes a 403 with an
// error toast and reports false so the caller returns immediately. The route's
// uiScopeAuth has already enforced authentication + the write scope before this
// runs; this adds the CSRF layer (the SameSite=Lax cookie is the first line, this
// is the second). No write path skips it.
func (s *Server) uiWriteGuard(w http.ResponseWriter, r *http.Request) bool {
	if !s.csrfOK(r) {
		s.renderWorkerActionToastStatus(w, r, http.StatusForbidden, webui.WorkerActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Couldn't verify that request",
			Message: "Your session token didn't match. Reload the page and try again.",
		})
		return false
	}
	return true
}

// renderWorkerActionError maps a fleet write error to a toast: a not-connected
// worker is a clear 404-flavored message, anything else a generic failure (the
// detail is logged, never surfaced). The action verb personalizes the message.
func (s *Server) renderWorkerActionError(w http.ResponseWriter, r *http.Request, action, id string, err error) {
	if errors.Is(err, server.ErrWorkerNotFound) {
		s.renderWorkerActionToastStatus(w, r, http.StatusNotFound, webui.WorkerActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Worker not connected",
			Message: "Couldn't " + action + " " + shortIDForCrumb(id) + " — it isn't in the fleet right now.",
		})
		return
	}
	s.reqLog(r.Context()).Error("ui worker action failed", "action", action, "worker", id, "err", err)
	s.renderWorkerActionToastStatus(w, r, http.StatusInternalServerError, webui.WorkerActionResult{
		Tone:    webui.ToneDanger,
		Title:   "That didn't work",
		Message: "Couldn't " + action + " the worker. Try again in a moment.",
	})
}

// renderWorkerActionToast writes a success toast (HTTP 200) HTMX appends to
// #toasts.
func (s *Server) renderWorkerActionToast(w http.ResponseWriter, r *http.Request, res webui.WorkerActionResult) {
	s.renderWorkerActionToastStatus(w, r, http.StatusOK, res)
}

// renderWorkerActionToastStatus writes the toast fragment with an explicit status
// code, setting the console page headers first (the fragment is HTML, no-store).
func (s *Server) renderWorkerActionToastStatus(w http.ResponseWriter, r *http.Request, status int, res webui.WorkerActionResult) {
	setUIPageHeaders(w)
	w.WriteHeader(status)
	_ = webui.WorkerActionToast(res).Render(r.Context(), w)
}

// shortIDForCrumb renders a worker id compactly for a breadcrumb/message, mirroring
// the webui.shortID treatment (first 8 chars + ellipsis) so the chrome and the
// console body abbreviate ids identically.
func shortIDForCrumb(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}
