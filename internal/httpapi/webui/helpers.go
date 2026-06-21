package webui

import (
	"encoding/json"
	"strings"
)

// pageTitle composes the document <title>. A screen supplies a short suffix
// ("Overview"); the product name is appended so every tab reads
// "Overview · agent-gpu console". An empty suffix yields just the product name.
func pageTitle(suffix string) string {
	if suffix == "" {
		return "agent-gpu console"
	}
	return suffix + " · agent-gpu console"
}

// csrfHeaderJSON builds the hx-headers value that makes every HTMX request carry
// the double-submit CSRF token (header X-CSRF-Token), matching the cookie the
// server set. It is attached once on <body> so all descendant HTMX requests
// inherit it. The value is a tiny JSON object; json.Marshal guarantees it is
// safely quoted (the token is server-minted hex, but encoding it defensively
// keeps the attribute well-formed regardless).
func csrfHeaderJSON(token string) string {
	b, err := json.Marshal(map[string]string{"X-CSRF-Token": token})
	if err != nil {
		// The input is a flat string map; marshalling cannot fail in practice. Fall
		// back to an empty object so the attribute is still valid JSON.
		return "{}"
	}
	return string(b)
}

// shortID renders a key id compactly for the topbar/sidebar: the first 8
// characters, so a long opaque id does not dominate the chrome. The full id is
// shown in the element's title attribute by the caller. Short ids are returned
// unchanged.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// roleLabel summarizes the viewer's privilege for the sidebar footer: "admin" for
// the superuser, otherwise the joined role list, or "restricted" when a key holds
// only individual scopes and no role. It never reveals scopes or the token.
func roleLabel(v Viewer) string {
	if v.IsAdmin {
		return "admin"
	}
	if len(v.Roles) > 0 {
		return strings.Join(v.Roles, ", ")
	}
	return "restricted"
}
