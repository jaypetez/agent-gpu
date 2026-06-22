package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// webui.go is the HTTP wiring for the embedded admin console (issue #100). It is
// kept OUT of httpapi.go on purpose: the OpenAPI route-sync test parses
// mux.Handle("...") literals in httpapi.go ONLY and pins the public-API route
// count, and the console's HTML/static routes are not part of the OpenAPI API
// contract. Registering them here, behind a single s.registerUIRoutes(mux) call in
// Handler(), keeps the API contract test green while still mounting the console on
// the same mux and middleware chain.
//
// Auth reuses the existing engine exactly: a login form posts an admin API token,
// the server validates it with auth.Authenticate, and on success sets an HttpOnly
// session cookie whose value IS the token (so JavaScript can never read it). The
// console pages and the /v1/admin API then accept either that cookie or the
// unchanged Authorization: Bearer header (see tokenFromRequest, used by
// authMiddleware). The console's sidebar is gated by the viewer's admin scopes, so
// a key sees only the sections it may read.

const (
	// uiBasePath is the console mount point. Everything the GUI serves lives under
	// it; the public OpenAPI API lives under /v1 and is untouched.
	uiBasePath = "/admin/"

	// sessionCookieName carries the admin API token as an HttpOnly cookie after a
	// successful console login. Its value is the token itself; HttpOnly keeps it
	// out of reach of any script, and tokenFromRequest reads it server-side.
	sessionCookieName = "agpu_admin_session"

	// csrfCookieName carries the double-submit CSRF token. It is deliberately NOT
	// HttpOnly: the server reads it server-side and the template echoes the value
	// into the form field / HTMX header, then the server compares the submitted
	// value against this cookie on every unsafe-method UI request (the double
	// submit). It carries no secret — it is a per-session, unguessable nonce — so it
	// being script-readable is by design and harmless.
	csrfCookieName = "agpu_csrf"

	// sessionTTL bounds how long a console session cookie is valid before the
	// operator must sign in again. One hour is a sane default for an admin surface;
	// the underlying token's own expiry (if any) is still enforced by Authenticate
	// on every request, so this only caps the cookie, never extends a key's life.
	sessionTTL = time.Hour
)

// registerUIRoutes mounts the embedded admin console on mux. It is called from
// Handler() with a single function call (NOT a route-registration string literal in
// httpapi.go), so the OpenAPI route-sync test — which parses those literals in
// httpapi.go — never counts these UI routes against the public-API contract. The
// console is
// thus served on the same mux and wrapped by the same metrics→requestID→recover
// chain as the API, but stays cleanly separate from the documented API surface.
//
// Routes (all under /admin/):
//
//   - GET  /admin/                  the console (dashboard); redirects to login when
//     unauthenticated.
//   - GET  /admin/login             the login page.
//   - POST /admin/login             validate the token, set the session + CSRF
//     cookies, redirect into the console.
//   - POST /admin/logout            clear the cookies, redirect to login.
//   - GET  /admin/partials/overview the HTMX dashboard partial (live telemetry).
//   - GET  /admin/assets/...        static assets (embedded or --ui-path).
//
// Static and page routes set Cache-Control appropriately; the asset route strips
// the /admin/assets prefix before serving from the asset FS.
func (s *Server) registerUIRoutes(mux *http.ServeMux) {
	mux.Handle("GET "+uiBasePath, http.HandlerFunc(s.handleUIIndex))
	mux.Handle("GET /admin/login", http.HandlerFunc(s.handleUILoginPage))
	mux.Handle("POST /admin/login", http.HandlerFunc(s.handleUILoginSubmit))
	mux.Handle("POST /admin/logout", http.HandlerFunc(s.handleUILogout))
	// The overview partial is data-bearing (queue depth, worker health, throttle
	// counters — the same telemetry GET /v1/admin/telemetry serves), so it is gated
	// on telemetry:read, NOT bare authentication. A plain user key (no admin scope)
	// must get 403 here exactly as it does on the JSON telemetry route; the
	// per-viewer events/log sub-panel additionally requires logs:read (enforced in
	// collectOverview).
	mux.Handle("GET /admin/partials/overview", s.uiScopeAuth(authz.ScopeTelemetryRead, http.HandlerFunc(s.handleUIOverviewPartial)))

	// Workers + GPU management (#101). Reads are gated on workers:read; the live
	// heatmap + worker-list partials refresh under the same scope. Writes mirror the
	// JSON admin scopes exactly: drain/evict on workers:write, pull/unload on
	// models:write. Every write handler additionally enforces the double-submit CSRF
	// check (s.csrfOK via uiWriteGuard) before touching the control plane.
	mux.Handle("GET /admin/workers", s.uiScopeAuth(authz.ScopeWorkersRead, http.HandlerFunc(s.handleUIWorkers)))
	mux.Handle("GET /admin/workers/{id}", s.uiScopeAuth(authz.ScopeWorkersRead, http.HandlerFunc(s.handleUIWorkerDetail)))
	mux.Handle("GET /admin/partials/gpu-heatmap", s.uiScopeAuth(authz.ScopeWorkersRead, http.HandlerFunc(s.handleUIHeatmapPartial)))
	mux.Handle("GET /admin/partials/worker-list", s.uiScopeAuth(authz.ScopeWorkersRead, http.HandlerFunc(s.handleUIWorkerListPartial)))
	mux.Handle("POST /admin/workers/{id}/drain", s.uiScopeAuth(authz.ScopeWorkersWrite, http.HandlerFunc(s.handleUIWorkerDrain)))
	mux.Handle("POST /admin/workers/{id}/evict", s.uiScopeAuth(authz.ScopeWorkersWrite, http.HandlerFunc(s.handleUIWorkerEvict)))
	mux.Handle("POST /admin/workers/{id}/models", s.uiScopeAuth(authz.ScopeModelsWrite, http.HandlerFunc(s.handleUIWorkerPull)))
	// {model...} is a trailing wildcard (matching the JSON unload route) because a
	// model ref can contain slashes (e.g. "library/llama3:8b"); r.PathValue("model")
	// yields the full remainder.
	mux.Handle("DELETE /admin/workers/{id}/models/{model...}", s.uiScopeAuth(authz.ScopeModelsWrite, http.HandlerFunc(s.handleUIWorkerUnload)))

	assetFS := s.uiAssets
	if assetFS == nil {
		assetFS = webui.Assets()
	}
	fileServer := http.FileServer(http.FS(assetFS))
	mux.Handle("GET /admin/assets/", s.uiAssetCache(http.StripPrefix("/admin/assets/", fileServer)))
}

// uiAuth gates a console route to an authenticated viewer, resolving the API key
// from the session cookie or Bearer header (tokenFromRequest). Unlike the API's
// authMiddleware (which returns a 401 JSON envelope), an unauthenticated console
// request is redirected to the login page with a ?next= back-link, because the
// caller is a browser, not an API client. On success it stashes the key on the
// context exactly like authMiddleware, so downstream handlers read keyFromContext.
func (s *Server) uiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := tokenFromRequest(r)
		if !ok {
			s.redirectToLogin(w, r, "")
			return
		}
		key, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				// Stale/invalid cookie: clear it and bounce to login rather than loop.
				s.clearSessionCookies(w, r)
				s.redirectToLogin(w, r, "Your session has expired. Sign in again.")
				return
			}
			s.reqLog(r.Context()).Error("ui authentication failed", "err", err)
			s.renderUIError(w, r, http.StatusInternalServerError, "Something went wrong on our end. Try again.")
			return
		}
		next.ServeHTTP(w, r.WithContext(withKey(r.Context(), key)))
	})
}

// uiScopeAuth gates a data-bearing console route to a viewer that is BOTH
// authenticated AND holds a specific admin scope. It authenticates exactly like
// uiAuth (session cookie or Bearer → auth.Authenticate, stashing the key on the
// context), then requires authz.HasScope(key, scope) — the same scope check the
// JSON admin routes enforce via scopeMiddleware, and the same HasScope the
// sidebar's per-section Visible gating uses. The two failure modes are kept
// distinct, mirroring the 401/403 split: an UNAUTHENTICATED request is redirected
// to login (it is a browser, like uiAuth), but an AUTHENTICATED-but-unscoped key
// is refused with a 403 HTML page rather than a redirect — bouncing it to login
// would loop forever (the key is valid, so it would just re-authenticate into the
// same 403). This closes the hole where a plain user key, by setting its own token
// as the session cookie, could read the telemetry board that GET
// /v1/admin/telemetry correctly denies it.
func (s *Server) uiScopeAuth(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := tokenFromRequest(r)
		if !ok {
			s.redirectToLogin(w, r, "")
			return
		}
		key, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				// Stale/invalid cookie: clear it and bounce to login rather than loop.
				s.clearSessionCookies(w, r)
				s.redirectToLogin(w, r, "Your session has expired. Sign in again.")
				return
			}
			s.reqLog(r.Context()).Error("ui authentication failed", "err", err)
			s.renderUIError(w, r, http.StatusInternalServerError, "Something went wrong on our end. Try again.")
			return
		}
		if !authz.HasScope(key, scope) {
			// Authenticated but unscoped: a 403, NOT a redirect (a valid key would
			// loop straight back through login into the same refusal). The message is
			// generic and never echoes the key's id, roles, or scopes.
			s.renderUIError(w, r, http.StatusForbidden, "You don't have access to this. Ask for a key with the right admin scope.")
			return
		}
		next.ServeHTTP(w, r.WithContext(withKey(r.Context(), key)))
	})
}

// handleUIIndex serves the console root. It authenticates inline (so an
// unauthenticated hit redirects to login rather than 401-ing) and renders the
// dashboard for the resolved key.
func (s *Server) handleUIIndex(w http.ResponseWriter, r *http.Request) {
	// Only the exact base path is the dashboard; anything else under /admin/ that
	// is not a more specific registered route is a 404 (not a silent dashboard).
	if r.URL.Path != uiBasePath {
		s.renderUIError(w, r, http.StatusNotFound, "That console page doesn't exist.")
		return
	}
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
	shell := s.buildShell(r, key, webui.SectionOverview, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Overview"},
	})
	setUIPageHeaders(w)
	_ = webui.Dashboard(webui.DashboardData{Shell: shell}).Render(r.Context(), w)
}

// handleUILoginPage serves the sign-in form. An already-authenticated visitor is
// sent straight into the console (so logging in twice is a no-op redirect).
func (s *Server) handleUILoginPage(w http.ResponseWriter, r *http.Request) {
	if token, ok := tokenFromRequest(r); ok {
		if _, err := s.auth.Authenticate(r.Context(), token); err == nil {
			http.Redirect(w, r, s.safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
	}
	csrf := s.ensureCSRFCookie(w, r)
	setUIPageHeaders(w)
	_ = webui.Login(webui.LoginData{
		AssetPath: assetPath(),
		CSRFToken: csrf,
		Error:     r.URL.Query().Get("error"),
		Next:      s.safeNext(r.URL.Query().Get("next")),
	}).Render(r.Context(), w)
}

// handleUILoginSubmit validates the posted admin token and, on success, sets the
// HttpOnly session cookie (value = token) plus a fresh CSRF cookie, then redirects
// into the console. A failed validation re-renders the form with an in-voice error
// and NO cookie set (so a bad token never establishes a session). The form is
// itself CSRF-checked against the pre-login CSRF cookie the login page set.
func (s *Server) handleUILoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLoginError(w, r, "We couldn't read that form. Try again.")
		return
	}
	if !s.csrfOK(r) {
		s.renderLoginError(w, r, "Your session token didn't match. Reload the page and try again.")
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		s.renderLoginError(w, r, "Enter an admin API token to sign in.")
		return
	}
	key, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		if !errors.Is(err, auth.ErrUnauthenticated) {
			s.reqLog(r.Context()).Error("ui login authentication failed", "err", err)
		}
		// Same generic message for unknown/revoked/expired/wrong — no enumeration.
		s.renderLoginError(w, r, "That token isn't valid. Check it and try again.")
		return
	}
	// Only an admin-capable key (the admin role or at least one admin scope) can do
	// anything in the console; a key with neither would log in to an empty shell.
	// Reject it with a clear message rather than presenting a dead console.
	if !hasAnyAdminAccess(key) {
		s.renderLoginError(w, r, "That token has no admin access. Ask for a key with an admin role or scope.")
		return
	}
	secure := requestIsHTTPS(r)
	s.setSessionCookie(w, token, secure)
	s.rotateCSRFCookie(w, secure)
	http.Redirect(w, r, s.safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// handleUILogout clears the session and CSRF cookies and returns to the login
// page. It is CSRF-checked (it is a state change) so a cross-site forced logout is
// rejected; an unauthenticated logout is a harmless no-op redirect.
func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if !s.csrfOK(r) {
		// A logout that fails CSRF still shouldn't strand the user; send them to
		// login without clearing (they can sign in fresh). This avoids a CSRF-driven
		// logout while not surfacing a scary error for a benign double-submit.
		s.redirectToLogin(w, r, "")
		return
	}
	s.clearSessionCookies(w, r)
	s.redirectToLogin(w, r, "")
}

// handleUIOverviewPartial renders the dashboard's polled region from one live
// telemetry pull plus the worker snapshot and recent events. It is the HTMX
// partial behind #overview. On a data error it returns the board's error partial
// (HTTP 200 with the error markup, so HTMX swaps it in — an HTTP error would make
// HTMX show nothing). It is gated by uiScopeAuth on telemetry:read (the panels are
// telemetry data); the event-stream sub-panel additionally requires logs:read,
// enforced in collectOverview, so a telemetry-only viewer never sees log lines.
func (s *Server) handleUIOverviewPartial(w http.ResponseWriter, r *http.Request) {
	setUIPageHeaders(w)
	data := s.collectOverview(r)
	_ = webui.Overview(assetPath(), data.kpis, data.queue, data.workers, data.events).Render(r.Context(), w)
}

// --- cookie + token helpers -------------------------------------------------

// tokenFromRequest resolves the API token for a request, preferring the HttpOnly
// session cookie the console sets and falling back to the Authorization: Bearer
// header. This is the single seam that lets the SAME middleware authenticate both
// a browser (cookie) and an API client (Bearer) without changing either: API
// clients keep sending Bearer exactly as before, and the cookie path is additive.
// It returns false when neither a non-empty cookie nor a Bearer token is present.
func tokenFromRequest(r *http.Request) (string, bool) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if v := strings.TrimSpace(c.Value); v != "" {
			return v, true
		}
	}
	return bearerToken(r)
}

// setSessionCookie writes the HttpOnly session cookie carrying the token. SameSite
// is Lax (so a top-level navigation from another site cannot ride the session into
// an unsafe POST, while normal same-site use works); Secure is set only on HTTPS
// (requestIsHTTPS) because the in-process server speaks plain HTTP today — see the
// package note: production MUST terminate TLS in front of the server, at which
// point the cookie is also marked Secure.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// clearSessionCookies expires both the session and CSRF cookies. It mirrors the
// attributes used to set them (Path=/) so the browser actually removes them.
func (s *Server) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := requestIsHTTPS(r)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: "", Path: "/",
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ensureCSRFCookie returns the request's CSRF token, minting and setting a fresh
// one if none is present. The login page calls it so a pre-auth form already
// carries a token to double-submit. The value is server-minted, unguessable hex.
func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	tok := newID("csrf-")
	s.writeCSRFCookie(w, tok, requestIsHTTPS(r))
	return tok
}

// rotateCSRFCookie mints and sets a brand-new CSRF token. It is called on login
// success so the authenticated session gets a token distinct from any pre-login
// one (defense against fixation). Returns nothing; the value rides the cookie and
// is read back on the next page render.
func (s *Server) rotateCSRFCookie(w http.ResponseWriter, secure bool) {
	s.writeCSRFCookie(w, newID("csrf-"), secure)
}

func (s *Server) writeCSRFCookie(w http.ResponseWriter, tok string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: false, // read server-side and echoed into the form/header for the double-submit
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// csrfOK enforces the double-submit CSRF check for an unsafe-method UI request:
// the token in the agpu_csrf cookie must match the one submitted in the
// csrf_token form field OR the X-CSRF-Token header (HTMX requests send the
// header). As defense-in-depth it also accepts the request when the browser's
// Sec-Fetch-Site is same-origin/none and rejects an explicit cross-site value.
// SameSite=Lax on the cookie is the first line of defense; this is the second.
func (s *Server) csrfOK(r *http.Request) bool {
	// A cross-site fetch is rejected outright when the browser tells us so.
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	submitted := r.Header.Get("X-CSRF-Token")
	if submitted == "" {
		submitted = r.PostFormValue("csrf_token")
	}
	return submitted != "" && submitted == c.Value
}

// --- rendering helpers ------------------------------------------------------

// buildShell assembles the layout view-model for an authenticated viewer: the
// role-gated navigation (each section visible only when the key holds its read
// scope), the viewer summary, the active section, and the breadcrumb trail. This
// is where AC2/AC5's role-based IA is computed.
func (s *Server) buildShell(r *http.Request, key store.APIKey, active webui.Section, crumbs []webui.Crumb) webui.ShellData {
	csrf := ""
	if c, err := r.Cookie(csrfCookieName); err == nil {
		csrf = c.Value
	}
	isAdmin := hasRole(key.Roles, authz.RoleAdmin)
	nav := []webui.NavEntry{
		{Section: webui.SectionOverview, Label: "Overview", Href: uiBasePath, Scope: authz.ScopeTelemetryRead, Icon: "overview"},
		{Section: webui.SectionWorkers, Label: "Workers", Href: uiBasePath + "workers", Scope: authz.ScopeWorkersRead, Icon: "workers"},
		{Section: webui.SectionKeys, Label: "API keys", Href: uiBasePath + "keys", Scope: authz.ScopeKeysRead, Icon: "keys"},
		{Section: webui.SectionUsage, Label: "Usage", Href: uiBasePath + "usage", Scope: authz.ScopeTelemetryRead, Icon: "usage"},
		{Section: webui.SectionLogs, Label: "Logs", Href: uiBasePath + "logs", Scope: authz.ScopeLogsRead, Icon: "logs"},
		{Section: webui.SectionAudit, Label: "Audit", Href: uiBasePath + "audit", Scope: authz.ScopeAuditRead, Icon: "audit"},
		{Section: webui.SectionConfig, Label: "Settings", Href: uiBasePath + "config", Scope: authz.ScopeConfigRead, Icon: "config"},
	}
	for i := range nav {
		nav[i].Visible = authz.HasScope(key, nav[i].Scope)
	}
	return webui.ShellData{
		Viewer: webui.Viewer{
			KeyID:   key.ID,
			Name:    key.Name,
			Roles:   key.Roles,
			IsAdmin: isAdmin,
		},
		Active:     active,
		Nav:        nav,
		Crumbs:     crumbs,
		Title:      sectionTitle(active),
		AssetPath:  assetPath(),
		CSRFToken:  csrf,
		LiveStream: true,
	}
}

// renderLoginError re-renders the login page with an in-voice error and a 401
// status. It re-mints/uses the CSRF token so the retry form is valid. No session
// cookie is set on this path.
func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, msg string) {
	csrf := s.ensureCSRFCookie(w, r)
	setUIPageHeaders(w)
	w.WriteHeader(http.StatusUnauthorized)
	_ = webui.Login(webui.LoginData{
		AssetPath: assetPath(),
		CSRFToken: csrf,
		Error:     msg,
		Next:      s.safeNext(r.PostFormValue("next")),
	}).Render(r.Context(), w)
}

// renderUIError renders a minimal HTML error page for a console request (a browser
// expects HTML, not the API's JSON envelope). It reuses the login shell's ground
// styling via a tiny inline page so it never depends on an authenticated shell.
func (s *Server) renderUIError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	setUIPageHeaders(w)
	w.WriteHeader(status)
	_ = webui.ErrorPage(assetPath(), http.StatusText(status), msg, uiBasePath).Render(r.Context(), w)
}

// redirectToLogin sends a browser to the login page, preserving where it was
// headed (?next=) so a successful sign-in returns there, and optionally an error
// message to show. It uses 303 See Other so a POST that triggers it becomes a GET.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request, msg string) {
	q := url.Values{}
	// Preserve where the browser was headed so a successful sign-in returns there —
	// but never the auth endpoints themselves (logging in shouldn't bounce back to
	// the login or logout path).
	if next := currentPath(r); next != "" && next != "/admin/login" && next != "/admin/logout" {
		q.Set("next", next)
	}
	if msg != "" {
		q.Set("error", msg)
	}
	target := "/admin/login"
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// uiAssetCache wraps the static asset handler with a long cache lifetime. Assets
// are content-stable per build (the CSS is rebuilt by `make ui`, the vendored JS
// is version-pinned), so a generous immutable cache is safe and keeps the console
// snappy. The HTML pages themselves are always no-store (set per-handler).
func (s *Server) uiAssetCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// --- small pure helpers -----------------------------------------------------

// assetPath is the URL prefix under which the console's static assets are served.
// Templates build asset URLs from it (assetPath()+"/css/app.css"), and several
// page actions are addressed relative to it (assetPath()+"/../login"); keeping it
// in one place means a future remount only changes here.
func assetPath() string { return "/admin/assets" }

// uiContentSecurityPolicy is the console's CSP: default-src 'self' confines every
// resource to the server's own origin, so NO external host — no CDN, no third-party
// script/style/font/image, no off-origin fetch/XHR/WebSocket — can be loaded,
// matching the "vendored, no CDN" requirement (#100, AC7) at the browser. img-src
// adds data: for the inline SVG favicon; style-src and script-src allow 'unsafe-
// inline'/'unsafe-eval' because the queue bars use inline width styles (live data,
// not a token) and Alpine evaluates its directives at runtime — both same-origin
// code we ship, never third-party. frame-ancestors 'none' blocks click-jacking.
const uiContentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self' 'unsafe-eval'; " +
	"connect-src 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// setUIPageHeaders sets the security + caching headers common to every console HTML
// response: the CSP above, nosniff, a referrer policy, frame-deny (belt with the
// CSP frame-ancestors), and no-store (pages are per-session and must never be
// cached by a shared proxy). Static assets set their own (cacheable) headers.
func setUIPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", uiContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}

// requestIsHTTPS reports whether the request reached us over TLS, either
// in-process (r.TLS set) or via a TLS-terminating proxy that set
// X-Forwarded-Proto: https. It decides the cookie Secure flag: the in-process
// server speaks plain HTTP today, so Secure must be conditional (a Secure cookie
// is dropped by the browser over plain HTTP, which would break local login). In
// production behind TLS termination the proxy header (or direct TLS) flips Secure
// on. See the package doc note on terminating TLS in production.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// hasAnyAdminAccess reports whether a key can do anything in the console: it holds
// the admin role (superuser) or at least one admin scope. A key with neither would
// see an empty shell, so login rejects it with a clear message instead.
func hasAnyAdminAccess(key store.APIKey) bool {
	if hasRole(key.Roles, authz.RoleAdmin) {
		return true
	}
	return len(key.AdminScopes) > 0
}

// safeNext sanitizes a ?next=/form next destination to a local, absolute path so a
// crafted next= can never become an open redirect to another origin. It accepts
// only a value beginning with a single "/" (not "//", which a browser treats as a
// protocol-relative URL to another host) and falling under the console base; any
// other value falls back to the console root.
func (s *Server) safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return uiBasePath
	}
	// Reject control characters / backslashes that could confuse a redirect.
	if strings.ContainsAny(next, "\\\r\n") {
		return uiBasePath
	}
	if !strings.HasPrefix(next, uiBasePath) && next != strings.TrimSuffix(uiBasePath, "/") {
		return uiBasePath
	}
	return next
}

// currentPath returns the request path (with query) for use as a ?next= back-link,
// or "" if it is not under the console.
func currentPath(r *http.Request) string {
	if !strings.HasPrefix(r.URL.Path, "/admin") {
		return ""
	}
	if r.URL.RawQuery != "" {
		return r.URL.Path + "?" + r.URL.RawQuery
	}
	return r.URL.Path
}

// sectionTitle maps a section to the document <title> suffix the shell renders.
func sectionTitle(sec webui.Section) string {
	switch sec {
	case webui.SectionOverview:
		return "Overview"
	case webui.SectionWorkers:
		return "Workers"
	case webui.SectionKeys:
		return "API keys"
	case webui.SectionUsage:
		return "Usage"
	case webui.SectionLogs:
		return "Logs"
	case webui.SectionAudit:
		return "Audit"
	case webui.SectionConfig:
		return "Settings"
	default:
		return ""
	}
}
