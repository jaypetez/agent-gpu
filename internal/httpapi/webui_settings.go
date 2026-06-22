package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_settings.go is the HTTP wiring for the console's Settings screen (#103): a
// read page (GET, gated config:read) and a live-apply write (PUT, gated
// config:write). The page renders the effective resolved settings — the runtime-
// tunable fields grouped into tabs, the boot-only fields read-only — exactly what
// GET /v1/admin/config reports. The write is the console's settings mutation and so,
// like every state-changing console handler:
//
//   - calls s.uiWriteGuard(w, r) FIRST (the double-submit CSRF check) and refuses
//     with 403 BEFORE any mutation — the route's uiScopeAuth has already enforced
//     authentication + config:write, this adds the CSRF layer;
//   - validates EVERY present field via the SAME applyConfigPatch the JSON PUT uses
//     (rejecting an invalid value or a boot-only/unknown key with a 400 and applying
//     NOTHING), then applies the change LIVE via the SAME s.config.apply + checkpoint
//     the JSON handler uses, so the console and the API drive config identically; and
//   - records exactly one audit entry under the SAME config.update op the JSON
//     handler uses, so a console-initiated change is indistinguishable in the trail
//     from an API-initiated one.
//
// On success it re-renders the tabbed editor with the new effective values plus a
// success toast; on a validation error it returns an inline field error (HTTP 400)
// with nothing applied.

// handleUIConfig serves GET /admin/config: the settings screen. It authenticates
// inline (an unauthenticated hit redirects to login) and renders the editor for the
// resolved key, marking it interactive only when the viewer also holds config:write
// (a config:read-only viewer sees the values read-only). The route gates the page on
// config:read.
func (s *Server) handleUIConfig(w http.ResponseWriter, r *http.Request) {
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
	shell := s.buildShell(r, key, webui.SectionConfig, []webui.Crumb{
		{Label: "Console", Href: uiBasePath},
		{Label: "Settings"},
	})
	canWrite := authz.HasScope(key, authz.ScopeConfigWrite)
	setUIPageHeaders(w)
	_ = webui.Settings(s.settingsData(shell, canWrite)).Render(r.Context(), w)
}

// settingsData assembles the SettingsData for a render, threading the shell through
// the in-process projection (collectSettings reads the live config holder).
func (s *Server) settingsData(shell webui.ShellData, canWrite bool) webui.SettingsData {
	d := s.collectSettings(canWrite)
	d.Shell = shell
	return d
}

// handleUIConfigUpdate serves PUT /admin/config: apply a partial settings change
// LIVE. It is the console's settings WRITE — gated on config:write by the route and
// CSRF-checked here FIRST — and reuses the JSON PUT's validate→apply→checkpoint→
// audit pipeline exactly. Only fields present (and non-empty) in the posted form are
// changed; an invalid value, a boot-only field, or an unknown key is rejected with a
// 400 inline error and NOTHING is applied. On success the editor re-renders with the
// new values and a success toast.
func (s *Server) handleUIConfigUpdate(w http.ResponseWriter, r *http.Request) {
	// CSRF FIRST, before any mutation — the route already enforced auth + config:write.
	if !s.uiWriteGuard(w, r) {
		return
	}
	if s.config == nil {
		s.renderSettingsToastStatus(w, r, http.StatusServiceUnavailable, webui.SettingsResult{
			Tone:    webui.ToneDanger,
			Title:   "Settings unavailable",
			Message: "Runtime configuration isn't enabled on this server.",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderSettingsToastStatus(w, r, http.StatusBadRequest, webui.SettingsResult{
			Tone:    webui.ToneDanger,
			Title:   "Couldn't read that",
			Message: "The form was malformed. Reload the page and try again.",
		})
		return
	}

	// Build a partial JSON patch from the posted form, mapping each present tunable
	// field to its typed JSON value so the SAME applyConfigPatch validator the JSON
	// PUT uses runs unchanged. A boot-only field present in the form is rejected
	// before anything is applied (mirroring the JSON handler's read-only guard).
	raw, ferr := settingsFormToPatch(r)
	if ferr != nil {
		s.renderSettingsFieldError(w, r, ferr.Error())
		return
	}

	// Serialize the whole read-modify-apply-commit, exactly as the JSON handler does.
	s.config.writeMu.Lock()
	defer s.config.writeMu.Unlock()

	s.config.mu.RLock()
	pending := s.config.cur
	s.config.mu.RUnlock()

	changed, err := applyConfigPatch(&pending, raw)
	if err != nil {
		// A validation failure: nothing applied. Surface the field-named message inline.
		s.renderSettingsFieldError(w, r, err.Error())
		return
	}
	if len(changed) == 0 {
		// A no-op (no field actually changed): re-render the editor with a calm note.
		s.renderSettingsOK(w, r, "No changes", "Every setting already had that value.")
		return
	}

	before := s.config.snapshotFields(changed)
	if err := s.config.apply(pending, changed); err != nil {
		s.recordAudit(r, auditOpConfigUpdate, "config", audit.OutcomeFailure, before, nil)
		s.renderSettingsFieldError(w, r, err.Error())
		return
	}
	after := s.config.snapshotFields(changed)
	s.recordAudit(r, auditOpConfigUpdate, "config", audit.OutcomeSuccess, before, after)

	if err := s.config.checkpoint(); err != nil {
		// A checkpoint write failure must not fail the request — the change took effect
		// live. Log it; the change simply may not survive a restart.
		s.reqLog(r.Context()).Warn("ui config checkpoint failed", "err", err)
	}

	s.renderSettingsOK(w, r, "Settings applied", "Your changes took effect immediately.")
}

// settingsFormError carries a field-named validation message for the inline error
// (it implements error so the apply pipeline's first-error contract composes).
type settingsFormError struct{ msg string }

func (e settingsFormError) Error() string { return e.msg }

// settingsFormToPatch builds the partial JSON patch from the posted form: each
// present, non-empty tunable field becomes a typed JSON value (numbers as JSON
// numbers, durations/text/selects as JSON strings) keyed by its config key, so
// applyConfigPatch validates the console PUT byte-for-byte like the JSON PUT. A
// boot-only field present in the form is rejected here (mirroring the JSON handler's
// read-only guard). An empty field is OMITTED (a blank input means "leave unchanged",
// not "set to empty"). An unknown form field is ignored (the form's hidden CSRF and
// the tab markers are not config keys).
func settingsFormToPatch(r *http.Request) (map[string]json.RawMessage, error) {
	raw := map[string]json.RawMessage{}
	for key := range r.PostForm {
		if key == "csrf_token" {
			continue
		}
		if isReadOnlyField(key) {
			return nil, settingsFormError{"field \"" + key + "\" is read-only; restart to change it"}
		}
		if !isTunableField(key) {
			// Not a config key (a tab marker or stray field) — ignore it.
			continue
		}
		val := strings.TrimSpace(r.PostFormValue(key))
		if val == "" {
			// Blank = leave unchanged.
			continue
		}
		if numericConfigField(key) {
			// A numeric tunable: emit a JSON number (unquoted) so decodeUint/
			// decodeNonNegInt accept it. A non-numeric value is rejected with a clear,
			// field-named message rather than producing invalid JSON.
			if !isNonNegInteger(val) {
				return nil, settingsFormError{"invalid " + key + " " + val + ": must be a non-negative integer"}
			}
			raw[key] = json.RawMessage(val)
			continue
		}
		// A string-valued tunable (duration / log level / overflow policy): emit a JSON
		// string. json.Marshal guarantees correct quoting/escaping.
		b, _ := json.Marshal(val)
		raw[key] = json.RawMessage(b)
	}
	return raw, nil
}

// numericConfigField reports whether a tunable key takes a JSON number (the
// counts/limits), vs. a JSON string (durations, the enums). It mirrors the
// decodeUint/decodeNonNegInt fields of applyConfigPatch.
func numericConfigField(key string) bool {
	switch key {
	case "quota_default_rpm", "quota_default_tpm", "quota_default_daily_tokens", "quota_default_monthly_tokens",
		"quota_global_rpm", "quota_global_tpm",
		"session_max_turns", "session_max_bytes", "session_max_context_tokens", "session_max_sessions_per_key":
		return true
	default:
		return false
	}
}

// isNonNegInteger reports whether s is a base-10 non-negative integer (digits only,
// non-empty). It guards the numeric-field JSON emission so a bad value yields a
// field-named 400 rather than malformed JSON.
func isNonNegInteger(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// renderSettingsOK re-renders the tabbed editor (with the new effective values) and
// appends a success toast, so the screen reflects the applied change in place. The
// viewer reached this path through config:write, so the editor stays interactive.
func (s *Server) renderSettingsOK(w http.ResponseWriter, r *http.Request, title, msg string) {
	setUIPageHeaders(w)
	_ = webui.SettingsApplied(s.collectSettings(true), webui.SettingsResult{
		Tone:    webui.ToneOK,
		Title:   title,
		Message: msg,
	}).Render(r.Context(), w)
}

// renderSettingsFieldError returns the inline validation error fragment (HTTP 400)
// with nothing applied, so the operator sees exactly which field was rejected and
// why. The message is the field-named applyConfigPatch error.
func (s *Server) renderSettingsFieldError(w http.ResponseWriter, r *http.Request, msg string) {
	setUIPageHeaders(w)
	w.WriteHeader(http.StatusBadRequest)
	_ = webui.SettingsError(msg).Render(r.Context(), w)
}

// renderSettingsToastStatus writes a settings toast fragment with an explicit status
// code (used for the 403/503 framing failures, distinct from the inline field error).
func (s *Server) renderSettingsToastStatus(w http.ResponseWriter, r *http.Request, status int, res webui.SettingsResult) {
	setUIPageHeaders(w)
	w.WriteHeader(status)
	_ = webui.SettingsToast(res).Render(r.Context(), w)
}
