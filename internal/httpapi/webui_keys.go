package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// webui_keys.go is the HTTP wiring for the console's API-keys, users, and
// permissions screens (#102). It mirrors the workers handlers (webui_workers.go)
// exactly: the page handler authenticates, builds the role-gated shell, and renders
// a full templ in @Shell; the partial handler renders just the table fragment for
// HTMX to swap; and each write handler:
//
//   - calls s.uiWriteGuard(w, r) FIRST (the double-submit CSRF check) and refuses
//     with 403 BEFORE any side effect, exactly like the worker write handlers —
//     HTMX carries the token in the X-CSRF-Token header inherited from the body's
//     hx-headers, so a legitimate console action always passes;
//   - calls the SAME in-process auth.Service method the JSON admin endpoint uses
//     (CreateWithPermissions / Rotate / Revoke / SetPermissions), so the console and
//     the API drive key management identically;
//   - records exactly one audit entry via s.recordAudit, reusing the SAME op
//     constants as the JSON handlers (key.create / key.rotate / key.revoke /
//     key.permissions), so a console-initiated change is indistinguishable in the
//     trail from an API-initiated one — and the plaintext token is NEVER logged or
//     audited; and
//   - returns either the one-time token reveal fragment (create / rotate) or a toast
//     (revoke / permissions) HTMX swaps in, with status conveyed by tone AND text.
//
// The routes are registered in registerUIRoutes (webui.go), gated by s.uiScopeAuth
// on keys:read (the page + the list partial) or keys:write (create / rotate /
// revoke / permissions) — the same scopes the JSON admin routes require — so an
// authenticated-but-unscoped key gets a 403 HTML page, and the "API keys" sidebar
// entry is hidden for a key without keys:read (buildShell gates it). The masked
// table NEVER shows a token; the one-time plaintext is shown once in the reveal and
// never stored or re-shown.

// handleUIKeys serves GET /admin/keys: the keys screen. It authenticates inline (an
// unauthenticated hit redirects to login) and renders the Keys page for the
// resolved key, seeding the role + admin-scope catalog the create/permissions
// editors populate their pickers from. The masked key table loads via HTMX after
// first paint from its partial.
func (s *Server) handleUIKeys(w http.ResponseWriter, r *http.Request) {
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
	shell := s.buildShell(r, key, webui.SectionKeys, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "API keys"},
	})
	setUIPageHeaders(w)
	_ = webui.Keys(webui.KeysData{
		Shell:       shell,
		Roles:       keyRoleOptions(),
		AdminScopes: authz.AllScopes(),
	}).Render(r.Context(), w)
}

// handleUIKeyListPartial renders the masked keys-table fragment from the in-process
// key store (the same s.auth.List behind GET /v1/admin/keys). It is the HTMX
// partial behind #key-list, gated on keys:read by the route. The fragment carries
// no secret — every row shows a fixed mask, never a token.
func (s *Server) handleUIKeyListPartial(w http.ResponseWriter, r *http.Request) {
	rows, err := s.collectKeys(r.Context())
	if err != nil {
		s.reqLog(r.Context()).Error("ui key list failed", "err", err)
		setUIPageHeaders(w)
		_ = webui.KeyListError("Couldn't load API keys. Try again in a moment.").Render(r.Context(), w)
		return
	}
	setUIPageHeaders(w)
	_ = webui.KeyListPartial(rows, keyRoleOptions(), authz.AllScopes()).Render(r.Context(), w)
}

// handleUIKeyCreate serves POST /admin/keys: mint a new key. CSRF-checked first;
// the name, owner/team labels, roles, admin scopes, and allow/deny model lists come
// from the posted form. Unknown roles/scopes are rejected 400 BEFORE the key is
// created (client- and server-side validation, AC2). It calls the same
// CreateWithPermissions the JSON endpoint uses, stamping CreatedBy from the viewer's
// key id (provenance), audits under key.create with the MASKED key values (never the
// token), and returns the one-time token reveal fragment. Gated on keys:write.
func (s *Server) handleUIKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderKeyActionToastStatus(w, r, http.StatusBadRequest, webui.KeyActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Couldn't read that",
			Message: "The form was malformed. Reload the page and try again.",
		})
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.renderKeyActionToastStatus(w, r, http.StatusBadRequest, webui.KeyActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Name is required",
			Message: "Give the key a name so it can be told apart later.",
		})
		return
	}
	roles := r.PostForm["roles"]
	scopes := r.PostForm["admin_scopes"]
	if !s.validateKeyGrants(w, r, roles, scopes) {
		return
	}
	perms := auth.Permissions{
		Roles:       roles,
		AdminScopes: scopes,
		AllowModels: parseModelList(r.PostFormValue("allow_models")),
		DenyModels:  parseModelList(r.PostFormValue("deny_models")),
		Owner:       strings.TrimSpace(r.PostFormValue("owner")),
		Team:        strings.TrimSpace(r.PostFormValue("team")),
		CreatedBy:   viewerKeyID(r),
	}
	token, key, err := s.auth.CreateWithPermissions(r.Context(), name, perms)
	if err != nil {
		s.recordAudit(r, auditOpKeyCreate, "", audit.OutcomeFailure, nil, nil)
		s.reqLog(r.Context()).Error("ui key create failed", "err", err)
		s.renderKeyActionToastStatus(w, r, http.StatusInternalServerError, webui.KeyActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Couldn't create the key",
			Message: "Something went wrong on our end. Try again in a moment.",
		})
		return
	}
	// Audit the MASKED key values only — never the plaintext token.
	s.recordAudit(r, auditOpKeyCreate, key.ID, audit.OutcomeSuccess, nil, auditKeyValues(key))
	s.renderKeyReveal(w, r, http.StatusCreated, webui.KeyReveal{
		KeyID:   key.ID,
		Name:    key.Name,
		Token:   token,
		Title:   "Key created",
		Message: "Copy this token now — it is shown once and can't be retrieved again.",
	})
}

// handleUIKeyRotate serves POST /admin/keys/{id}/rotate: replace a key's secret,
// preserving its id and all attached identity/permissions. CSRF-checked first; it
// calls the same Rotate the JSON endpoint uses, audits under key.rotate (never the
// token), and returns the one-time reveal of the NEW token. A revoked key can't be
// rotated (surfaced as a toast). Gated on keys:write.
func (s *Server) handleUIKeyRotate(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	token, err := s.auth.Rotate(r.Context(), id)
	if err != nil {
		s.recordAudit(r, auditOpKeyRotate, id, audit.OutcomeFailure, nil, nil)
		s.renderKeyWriteError(w, r, "rotate", id, err)
		return
	}
	s.recordAudit(r, auditOpKeyRotate, id, audit.OutcomeSuccess, nil, nil)
	s.renderKeyReveal(w, r, http.StatusOK, webui.KeyReveal{
		KeyID:   id,
		Token:   token,
		Rotated: true,
		Title:   "Key rotated",
		Message: "The old token stops working immediately. Copy the new token now — it won't be shown again.",
	})
}

// handleUIKeyRevoke serves POST /admin/keys/{id}/revoke: permanently disable a key.
// CSRF-checked first; it reads the key before and after (for the audit before/after)
// and calls the same Revoke the JSON endpoint uses, auditing under key.revoke. The
// typed-name HIGH-friction confirm that gates this in the UI is a client concern
// (Alpine); the handler still enforces CSRF + keys:write. Gated on keys:write.
func (s *Server) handleUIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	before, _ := s.auth.Get(r.Context(), id)
	if err := s.auth.Revoke(r.Context(), id); err != nil {
		s.recordAudit(r, auditOpKeyRevoke, id, audit.OutcomeFailure, auditKeyValues(before), nil)
		s.renderKeyWriteError(w, r, "revoke", id, err)
		return
	}
	after, _ := s.auth.Get(r.Context(), id)
	s.recordAudit(r, auditOpKeyRevoke, id, audit.OutcomeSuccess, auditKeyValues(before), auditKeyValues(after))
	s.renderKeyActionToast(w, r, webui.KeyActionResult{
		Tone:    webui.ToneWarn,
		Title:   "Key revoked",
		Message: "Key " + shortIDForCrumb(id) + " can no longer authenticate.",
	})
}

// handleUIKeyPermissions serves POST /admin/keys/{id}/permissions: replace a key's
// roles, admin scopes, and allow/deny model lists (FULL replace, not a merge —
// AC2). CSRF-checked first; unknown roles/scopes are rejected 400 BEFORE the
// permissions are written (client- and server-side, AC2). It calls the same
// SetPermissions the JSON endpoint uses, auditing under key.permissions with the
// before/after masked values. Gated on keys:write.
func (s *Server) handleUIKeyPermissions(w http.ResponseWriter, r *http.Request) {
	if !s.uiWriteGuard(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderKeyActionToastStatus(w, r, http.StatusBadRequest, webui.KeyActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Couldn't read that",
			Message: "The form was malformed. Reload the page and try again.",
		})
		return
	}
	id := r.PathValue("id")
	roles := r.PostForm["roles"]
	scopes := r.PostForm["admin_scopes"]
	if !s.validateKeyGrants(w, r, roles, scopes) {
		return
	}
	before, _ := s.auth.Get(r.Context(), id)
	key, err := s.auth.SetPermissions(r.Context(), id, auth.Permissions{
		Roles:       roles,
		AdminScopes: scopes,
		AllowModels: parseModelList(r.PostFormValue("allow_models")),
		DenyModels:  parseModelList(r.PostFormValue("deny_models")),
	})
	if err != nil {
		s.recordAudit(r, auditOpKeyPermissions, id, audit.OutcomeFailure, auditKeyValues(before), nil)
		s.renderKeyWriteError(w, r, "update", id, err)
		return
	}
	s.recordAudit(r, auditOpKeyPermissions, id, audit.OutcomeSuccess, auditKeyValues(before), auditKeyValues(key))
	s.renderKeyActionToast(w, r, webui.KeyActionResult{
		Tone:    webui.ToneOK,
		Title:   "Permissions updated",
		Message: "Key " + shortIDForCrumb(id) + " now has the roles and scopes you set.",
	})
}

// --- write-handler helpers --------------------------------------------------

// validateKeyGrants rejects an unknown role or admin scope BEFORE any mutation,
// returning false (and writing a 400 toast) so the caller returns immediately.
// This is the server side of the AC2 "reject unknown role/scope" guard; the editor
// only offers catalog values, so a legitimate submit always passes. It validates
// with the SAME authz.ValidRole / authz.ValidScope predicates the JSON
// validateRoles / validateScopes helpers use, but renders an HTML toast (not a JSON
// envelope) since this is the console surface.
func (s *Server) validateKeyGrants(w http.ResponseWriter, r *http.Request, roles, scopes []string) bool {
	for _, role := range roles {
		if !authz.ValidRole(role) {
			s.renderKeyActionToastStatus(w, r, http.StatusBadRequest, webui.KeyActionResult{
				Tone:    webui.ToneDanger,
				Title:   "Unknown role",
				Message: "The role " + role + " isn't recognized. Pick from the listed roles.",
			})
			return false
		}
	}
	for _, sc := range scopes {
		if !authz.ValidScope(sc) {
			s.renderKeyActionToastStatus(w, r, http.StatusBadRequest, webui.KeyActionResult{
				Tone:    webui.ToneDanger,
				Title:   "Unknown scope",
				Message: "The admin scope " + sc + " isn't recognized. Pick from the listed scopes.",
			})
			return false
		}
	}
	return true
}

// renderKeyWriteError maps an auth-service write error to a toast: an unknown key
// is a clear 404-flavored message; a rotate of a revoked key is a 409-flavored
// "can't rotate a revoked key"; anything else a generic failure (the detail is
// logged, never surfaced). The action verb personalizes the message.
func (s *Server) renderKeyWriteError(w http.ResponseWriter, r *http.Request, action, id string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.renderKeyActionToastStatus(w, r, http.StatusNotFound, webui.KeyActionResult{
			Tone:    webui.ToneDanger,
			Title:   "Key not found",
			Message: "Couldn't " + action + " " + shortIDForCrumb(id) + " — no such key.",
		})
		return
	}
	if errors.Is(err, auth.ErrUnauthenticated) {
		// Rotate of a revoked key: the secret can't be replaced on a dead key.
		s.renderKeyActionToastStatus(w, r, http.StatusConflict, webui.KeyActionResult{
			Tone:    webui.ToneWarn,
			Title:   "Key is revoked",
			Message: "Couldn't " + action + " " + shortIDForCrumb(id) + " — a revoked key can't be rotated.",
		})
		return
	}
	s.reqLog(r.Context()).Error("ui key action failed", "action", action, "key", id, "err", err)
	s.renderKeyActionToastStatus(w, r, http.StatusInternalServerError, webui.KeyActionResult{
		Tone:    webui.ToneDanger,
		Title:   "That didn't work",
		Message: "Couldn't " + action + " the key. Try again in a moment.",
	})
}

// renderKeyReveal writes the one-time token reveal fragment with an explicit status
// (201 for create, 200 for rotate). The reveal is the ONLY place the plaintext
// token appears; it is never stored, logged, or shown again. The console page
// headers (no-store) are set first.
func (s *Server) renderKeyReveal(w http.ResponseWriter, r *http.Request, status int, rev webui.KeyReveal) {
	setUIPageHeaders(w)
	w.WriteHeader(status)
	_ = webui.KeyReveal_(rev).Render(r.Context(), w)
}

// renderKeyActionToast writes a success toast (HTTP 200) HTMX appends to #toasts.
func (s *Server) renderKeyActionToast(w http.ResponseWriter, r *http.Request, res webui.KeyActionResult) {
	s.renderKeyActionToastStatus(w, r, http.StatusOK, res)
}

// renderKeyActionToastStatus writes the key-action toast fragment with an explicit
// status code, setting the console page headers first (the fragment is HTML,
// no-store).
func (s *Server) renderKeyActionToastStatus(w http.ResponseWriter, r *http.Request, status int, res webui.KeyActionResult) {
	setUIPageHeaders(w)
	w.WriteHeader(status)
	_ = webui.KeyActionToast(res).Render(r.Context(), w)
}

// viewerKeyID returns the authenticated viewer's key id from the request context
// (injected by uiScopeAuth via withKey), used to stamp CreatedBy on a new key for
// provenance. It is empty only if the key is somehow absent — never the token.
func viewerKeyID(r *http.Request) string {
	if k, ok := keyFromContext(r.Context()); ok {
		return k.ID
	}
	return ""
}

// parseModelList splits a textarea's content into a clean model-pattern list: it
// accepts newline- or comma-separated entries, trims whitespace, and drops blanks.
// An empty/whitespace-only input yields nil, which CreateWithPermissions /
// SetPermissions interpret as "no list" (clearing it on a full replace).
func parseModelList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
