package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
)

// webui_test.go exercises the embedded admin console's HTTP surface (issue #100):
// the token login flow and the HttpOnly session cookie it sets, cookie-based auth
// of both the console and the /v1/admin API, the double-submit CSRF defense,
// logout, the role-gated sidebar, and the tokenFromRequest cookie/Bearer
// precedence. It reuses the package's existing test rig (adminTestServer, mustKey)
// and drives requests through the fully-routed s.Handler() so the real middleware
// chain runs.

// uiGet issues a GET through the routed handler with optional cookies, returning
// the recorder. cookies maps cookie name->value; an empty map sends none.
func uiGet(t *testing.T, s *Server, path string, cookies map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// uiPostForm issues a urlencoded POST through the routed handler with optional
// cookies and headers, returning the recorder. The form is built from values.
func uiPostForm(t *testing.T, s *Server, path string, values url.Values, cookies, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// setCookieValue returns the value the response sets for the named cookie, and
// whether it was set. It parses the Set-Cookie headers on the recorder.
func setCookieValue(rec *httptest.ResponseRecorder, name string) (*http.Cookie, bool) {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}

// loginAndGetSession runs the full login handshake (GET login for a CSRF token,
// then POST the token) and returns the session + csrf cookies for the established
// session. It fails the test if login does not succeed.
func loginAndGetSession(t *testing.T, s *Server, token string) (session, csrf string) {
	t.Helper()
	// GET the login page to obtain a CSRF cookie.
	rec := uiGet(t, s, "/admin/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/login = %d, want 200", rec.Code)
	}
	csrfCookie, ok := setCookieValue(rec, csrfCookieName)
	if !ok {
		t.Fatal("login page did not set a CSRF cookie")
	}
	// POST the token with the matching CSRF token (cookie + form field).
	form := url.Values{"token": {token}, "csrf_token": {csrfCookie.Value}}
	rec = uiPostForm(t, s, "/admin/login", form,
		map[string]string{csrfCookieName: csrfCookie.Value},
		map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/login = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	sess, ok := setCookieValue(rec, sessionCookieName)
	if !ok {
		t.Fatal("successful login did not set a session cookie")
	}
	newCSRF, ok := setCookieValue(rec, csrfCookieName)
	if !ok {
		t.Fatal("successful login did not rotate the CSRF cookie")
	}
	return sess.Value, newCSRF.Value
}

// TestUILoginValidTokenSetsHttpOnlySession proves AC2: a valid admin token at the
// login form yields a 303 into the console and an HttpOnly session cookie whose
// value is the token, plus a fresh CSRF cookie. (AC2: login → HttpOnly cookie.)
func TestUILoginValidTokenSetsHttpOnlySession(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())

	rec := uiGet(t, s, "/admin/login", nil)
	csrfCookie, ok := setCookieValue(rec, csrfCookieName)
	if !ok {
		t.Fatal("login page did not set a CSRF cookie")
	}
	form := url.Values{"token": {token}, "csrf_token": {csrfCookie.Value}}
	rec = uiPostForm(t, s, "/admin/login", form,
		map[string]string{csrfCookieName: csrfCookie.Value}, nil)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", rec.Code)
	}
	sess, ok := setCookieValue(rec, sessionCookieName)
	if !ok {
		t.Fatal("login did not set the session cookie")
	}
	if !sess.HttpOnly {
		t.Error("session cookie is not HttpOnly — the token would be readable by JavaScript")
	}
	if sess.Value != token {
		t.Error("session cookie value should be the token itself")
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", sess.SameSite)
	}
	if sess.MaxAge <= 0 {
		t.Errorf("session cookie MaxAge = %d, want a positive TTL", sess.MaxAge)
	}
	// Plain HTTP test request → Secure must be false (a Secure cookie is dropped
	// over plain HTTP, which would break local login).
	if sess.Secure {
		t.Error("session cookie should not be Secure over plain HTTP")
	}
}

// TestUILoginInvalidTokenNoCookie proves AC2/the dos-list: an invalid token
// re-renders the login form with 401 and sets NO session cookie.
func TestUILoginInvalidTokenNoCookie(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})

	rec := uiGet(t, s, "/admin/login", nil)
	csrfCookie, _ := setCookieValue(rec, csrfCookieName)

	form := url.Values{"token": {"agpu_bogus_token"}, "csrf_token": {csrfCookie.Value}}
	rec = uiPostForm(t, s, "/admin/login", form,
		map[string]string{csrfCookieName: csrfCookie.Value}, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid-token login status = %d, want 401", rec.Code)
	}
	if _, ok := setCookieValue(rec, sessionCookieName); ok {
		t.Error("invalid token must not establish a session cookie")
	}
	// The error message is shown in an alert region. templ HTML-escapes the
	// apostrophe in "isn't", so match the unambiguous unescaped fragment.
	if !strings.Contains(rec.Body.String(), "valid. Check it and try again") {
		t.Error("invalid-token login should re-render the form with an error message")
	}
}

// TestUILoginNonAdminRejected proves the dos-list: a valid but non-admin key (no
// admin role and no admin scope) cannot sign in to the console — it would see an
// empty shell — and gets a clear message with no cookie.
func TestUILoginNonAdminRejected(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	// A "user" role grants inference but no admin role/scope.
	token := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})

	rec := uiGet(t, s, "/admin/login", nil)
	csrfCookie, _ := setCookieValue(rec, csrfCookieName)
	form := url.Values{"token": {token}, "csrf_token": {csrfCookie.Value}}
	rec = uiPostForm(t, s, "/admin/login", form,
		map[string]string{csrfCookieName: csrfCookie.Value}, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-admin login status = %d, want 401", rec.Code)
	}
	if _, ok := setCookieValue(rec, sessionCookieName); ok {
		t.Error("a non-admin key must not establish a console session")
	}
	if !strings.Contains(rec.Body.String(), "no admin access") {
		t.Error("non-admin login should explain the key lacks admin access")
	}
}

// TestUILoginMissingCSRFRejected proves the CSRF defense on the login POST: a
// request whose CSRF cookie and submitted token do not match is rejected before
// any authentication, with no session established.
func TestUILoginMissingCSRFRejected(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())

	// No CSRF cookie at all.
	form := url.Values{"token": {token}}
	rec := uiPostForm(t, s, "/admin/login", form, nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login without CSRF status = %d, want 401", rec.Code)
	}
	if _, ok := setCookieValue(rec, sessionCookieName); ok {
		t.Error("a login failing CSRF must not establish a session")
	}

	// Mismatched cookie vs submitted token.
	form = url.Values{"token": {token}, "csrf_token": {"wrong"}}
	rec = uiPostForm(t, s, "/admin/login", form,
		map[string]string{csrfCookieName: "different"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with mismatched CSRF status = %d, want 401", rec.Code)
	}
}

// TestUILoginCrossSiteRejected proves the Sec-Fetch-Site defense-in-depth: a POST
// the browser labels cross-site is rejected even if it carries a matching CSRF
// token (e.g. a leaked one).
func TestUILoginCrossSiteRejected(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())

	form := url.Values{"token": {token}, "csrf_token": {"tok"}}
	rec := uiPostForm(t, s, "/admin/login", form,
		map[string]string{csrfCookieName: "tok"},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-site login status = %d, want 401 (rejected)", rec.Code)
	}
}

// TestUISessionCookieAuthenticatesConsole proves AC2: a request carrying ONLY the
// session cookie (no Authorization header) reaches the authenticated dashboard.
func TestUISessionCookieAuthenticatesConsole(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /admin/ = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Fleet overview") {
		t.Error("dashboard did not render the overview heading")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("dashboard Content-Type = %q, want text/html", ct)
	}
}

// TestUISessionCookieAuthenticatesAdminAPI proves AC2: the SAME session cookie
// authenticates the JSON /v1/admin API, so the console's HTMX calls work without a
// Bearer header — while API clients using Bearer are unaffected (covered by the
// existing admin tests).
func TestUISessionCookieAuthenticatesAdminAPI(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/v1/admin/stats", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/admin/stats with session cookie = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "queue") {
		t.Error("admin stats response did not render via the cookie-authenticated request")
	}
}

// TestUILogoutClearsSession proves AC2: logout clears the session cookie (expires
// it) so a subsequent console request is unauthenticated and redirects to login.
func TestUILogoutClearsSession(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())
	session, csrf := loginAndGetSession(t, s, token)

	// Logout (CSRF-checked, same-site).
	rec := uiPostForm(t, s, "/admin/logout", url.Values{"csrf_token": {csrf}},
		map[string]string{sessionCookieName: session, csrfCookieName: csrf},
		map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", rec.Code)
	}
	cleared, ok := setCookieValue(rec, sessionCookieName)
	if !ok || cleared.MaxAge >= 0 && cleared.Value != "" {
		t.Error("logout did not expire the session cookie")
	}

	// The dashboard now redirects to login (cookie is gone).
	rec = uiGet(t, s, "/admin/", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("post-logout GET /admin/ = %d, want 303 redirect to login", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/admin/login") {
		t.Errorf("post-logout redirect = %q, want /admin/login", loc)
	}
}

// TestUIUnauthenticatedRedirdirectsToLogin proves a browser hitting the console
// without a session is redirected to the login page (not 401-ed like the API),
// preserving where it was headed via ?next=.
func TestUIUnauthenticatedRedirectsToLogin(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})

	rec := uiGet(t, s, "/admin/", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /admin/ = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/admin/login") {
		t.Fatalf("redirect = %q, want /admin/login", loc)
	}
	if !strings.Contains(loc, "next=") {
		t.Error("login redirect should carry a ?next= back-link")
	}
}

// TestUISidebarRoleGating proves AC2/AC5: the sidebar shows only the sections the
// viewer holds the read-scope for. An admin sees every section; a key scoped to
// workers:read sees Workers but NOT API keys or Settings.
func TestUISidebarRoleGating(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	// Admin: sees all sections.
	adminToken := mustKey(t, authSvc, adminPerms())
	adminSession, _ := loginAndGetSession(t, s, adminToken)
	rec := uiGet(t, s, "/admin/", map[string]string{sessionCookieName: adminSession})
	body := rec.Body.String()
	for _, label := range []string{"Workers", "API keys", "Usage", "Logs", "Audit", "Settings"} {
		if !strings.Contains(body, ">"+label+"<") {
			t.Errorf("admin sidebar is missing section %q", label)
		}
	}

	// Scoped: workers:read only. Sees Workers; not API keys / Settings / Audit.
	scopedToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeWorkersRead}})
	scopedSession, _ := loginAndGetSession(t, s, scopedToken)
	rec = uiGet(t, s, "/admin/", map[string]string{sessionCookieName: scopedSession})
	body = rec.Body.String()
	if !strings.Contains(body, ">Workers<") {
		t.Error("workers:read viewer should see the Workers section")
	}
	for _, hidden := range []string{">API keys<", ">Settings<", ">Audit<"} {
		if strings.Contains(body, hidden) {
			t.Errorf("workers:read viewer should NOT see %q", strings.Trim(hidden, "><"))
		}
	}
}

// TestTokenFromRequestPrecedence proves the cookie-over-Bearer precedence of the
// shared token resolver: the session cookie wins when present, the Bearer header
// is the fallback, and neither yields ok=false.
func TestTokenFromRequestPrecedence(t *testing.T) {
	t.Run("cookie present wins over bearer", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "cookie-token"})
		r.Header.Set("Authorization", "Bearer header-token")
		got, ok := tokenFromRequest(r)
		if !ok || got != "cookie-token" {
			t.Fatalf("tokenFromRequest = (%q,%v), want (cookie-token,true)", got, ok)
		}
	})
	t.Run("falls back to bearer when no cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/admin/stats", nil)
		r.Header.Set("Authorization", "Bearer header-token")
		got, ok := tokenFromRequest(r)
		if !ok || got != "header-token" {
			t.Fatalf("tokenFromRequest = (%q,%v), want (header-token,true)", got, ok)
		}
	})
	t.Run("empty cookie falls back to bearer", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ""})
		r.Header.Set("Authorization", "Bearer header-token")
		got, ok := tokenFromRequest(r)
		if !ok || got != "header-token" {
			t.Fatalf("tokenFromRequest = (%q,%v), want (header-token,true)", got, ok)
		}
	})
	t.Run("neither present", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
		if _, ok := tokenFromRequest(r); ok {
			t.Fatal("tokenFromRequest should return ok=false with no cookie and no bearer")
		}
	})
}

// TestUIOverviewPartialRenders proves the dashboard's HTMX partial renders the
// three named panels (queue depth, worker health, event stream) from the live
// telemetry, authenticated by the session cookie.
func TestUIOverviewPartialRenders(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)

	rec := uiGet(t, s, "/admin/partials/overview", map[string]string{sessionCookieName: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("overview partial = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, panel := range []string{"Queue depth", "Worker health", "Event stream", "Workers online"} {
		if !strings.Contains(body, panel) {
			t.Errorf("overview partial missing the %q panel", panel)
		}
	}
}

// TestUIAssetsServed proves the embedded static assets are served under the asset
// path with the right content type and a cache header, with no auth required (they
// carry no secrets and the login page needs the CSS before any session exists).
func TestUIAssetsServed(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})

	rec := uiGet(t, s, "/admin/assets/css/app.css", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET app.css = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Errorf("app.css Content-Type = %q, want text/css", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("app.css Cache-Control = %q, want a max-age", cc)
	}

	// The vendored JS is served too (no CDN).
	for _, asset := range []string{"/admin/assets/js/htmx.min.js", "/admin/assets/js/alpine.min.js"} {
		rec := uiGet(t, s, asset, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", asset, rec.Code)
		}
	}
}

// TestWithUIAssetsServesFromProvidedFS proves the --ui-path seam: when a custom
// asset FS is wired via WithUIAssets (cmd does this for --ui-path), the console
// serves assets from it instead of the embedded copy. A struct-literal Server sets
// uiAssets directly to a MapFS standing in for the disk FS.
func TestWithUIAssetsServesFromProvidedFS(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})
	s.uiAssets = fstest.MapFS{
		"css/app.css": &fstest.MapFile{Data: []byte("/* dev-mode marker */")},
	}

	rec := uiGet(t, s, "/admin/assets/css/app.css", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET app.css from --ui-path FS = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dev-mode marker") {
		t.Error("asset was not served from the WithUIAssets FS")
	}
}

// TestUIPagesSetSecurityHeaders proves the console HTML responses carry the strict
// CSP (default-src 'self' — no external host/CDN, AC7 at the browser) plus the
// hardening headers, on both the login page and the authenticated dashboard.
func TestUIPagesSetSecurityHeaders(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})

	// Login page (unauthenticated).
	rec := uiGet(t, s, "/admin/login", nil)
	assertSecurityHeaders(t, rec)

	// Dashboard (authenticated).
	token := mustKey(t, authSvc, adminPerms())
	session, _ := loginAndGetSession(t, s, token)
	rec = uiGet(t, s, "/admin/", map[string]string{sessionCookieName: session})
	assertSecurityHeaders(t, rec)
}

func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP missing default-src 'self' (no-CDN posture): %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors 'none': %q", csp)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on an HTML page", got)
	}
}

// TestUILoginNoStoreNoCache reinforces that a credential-bearing page is never
// cached (no-store), so a shared proxy or the back button cannot resurface it.
func TestUILoginNoStoreNoCache(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})
	rec := uiGet(t, s, "/admin/login", nil)
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("login Cache-Control = %q, want no-store", cc)
	}
}

// TestUIRoutesNotInOpenAPISpec is a focused guard reinforcing the verified anchor:
// the console routes are registered via s.registerUIRoutes (a function call), NOT
// mux.Handle string literals in httpapi.go, so the OpenAPI route-sync test
// (TestOpenAPISpecMatchesRegisteredRoutes) still counts exactly the public-API
// routes and passes. This test asserts the console is reachable (proving the routes
// ARE registered) while TestOpenAPISpecMatchesRegisteredRoutes independently
// asserts the API count is unchanged — together they prove the separation holds.
func TestUIRoutesRegisteredButOutsideAPIContract(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})
	// The login route is reachable (registered)…
	rec := uiGet(t, s, "/admin/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("console login route not registered: GET /admin/login = %d", rec.Code)
	}
	// …and a bogus console sub-path 404s through the console (not the API), proving
	// the console owns the /admin/ space.
	rec = uiGet(t, s, "/admin/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown console path = %d, want 404", rec.Code)
	}
}
