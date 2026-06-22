package httpapi

import (
	"context"
	"sort"
	"time"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// webui_keys_data.go maps the server's in-process key store and the static
// role/scope catalog onto the console's keys/users/permissions view-models (#102).
// Like webui_workers_data.go it reads the SAME in-process seams the JSON admin
// endpoints use — s.auth.List for the rows (the masked projection; the secret is
// never stored and so can never be read) and authz.AllRoles / authz.AllScopes for
// the editor pickers — so the console and the API never disagree, and a picker can
// never offer a role/scope authorization doesn't recognize. It performs no new
// work and starts no pollers; every value is read on demand for one instant.

// maskedSecret is the fixed placeholder the keys table shows in the secret column.
// It is identical for every row: the table NEVER renders a token (the plaintext is
// shown once, at creation/rotation, in the reveal partial). Showing a uniform mask
// makes it explicit to the operator that no secret is on the screen.
const maskedSecret = "agpu_••••••••••••"

// collectKeys projects the key store into the masked table rows, sorted by name
// (then id) for a stable render. Each row carries a lifecycle status word + tone
// (active / revoked / expired) so status reads in grayscale and to a screen
// reader, never color alone. No secret is included — the rows mirror the masked
// adminKeyView the JSON list returns.
func (s *Server) collectKeys(ctx context.Context) ([]webui.KeyRow, error) {
	keys, err := s.auth.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rows := make([]webui.KeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, newKeyRow(k, now))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

// newKeyRow builds one masked table row from a stored key. The secret column is a
// fixed mask; status is derived from the key's revoked/expired state with both a
// word and a tone. Roles/scopes are copied as-is for display.
func newKeyRow(k store.APIKey, now time.Time) webui.KeyRow {
	status, tone := keyStatus(k, now)
	return webui.KeyRow{
		ID:           k.ID,
		Name:         k.Name,
		Owner:        k.Owner,
		Team:         k.Team,
		Roles:        k.Roles,
		AdminScopes:  k.AdminScopes,
		AllowModels:  k.AllowModels,
		DenyModels:   k.DenyModels,
		MaskedSecret: maskedSecret,
		Created:      formatDate(k.CreatedAt),
		LastUsed:     relativeSince(k.LastUsedAt),
		Expiry:       formatExpiry(k.ExpiresAt),
		Status:       status,
		Tone:         tone,
		Revoked:      k.Revoked(),
		Expired:      k.Expired(now),
	}
}

// keyStatus maps a key's lifecycle to a status word + tone, conveyed by BOTH in
// the UI (AC1). Revoked wins over expired (a revoked key is dead regardless of its
// TTL); an unexpired, unrevoked key is "active".
func keyStatus(k store.APIKey, now time.Time) (word, tone string) {
	switch {
	case k.Revoked():
		return "revoked", webui.ToneDanger
	case k.Expired(now):
		return "expired", webui.ToneWarn
	default:
		return "active", webui.ToneOK
	}
}

// keyRoleOptions projects the assignable-role catalog (authz.AllRoles) into the
// editor's picker options: the role name (the submitted value) plus its short
// description. It is the SAME catalog GET /v1/admin/roles serves, so the GUI's
// pickers track the authorization ladder exactly.
func keyRoleOptions() []webui.RoleOption {
	roles := authz.AllRoles()
	opts := make([]webui.RoleOption, len(roles))
	for i, r := range roles {
		opts[i] = webui.RoleOption{Name: r.Name, Description: r.Description}
	}
	return opts
}

// formatDate renders a creation timestamp as a compact absolute date
// (2006-01-02), the operator's at-a-glance "when was this minted" without a noisy
// clock. A zero time renders an em dash.
func formatDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02")
}

// formatExpiry renders a key's TTL: an absolute date for a set expiry, or the
// word "never" when the key does not expire (a nil ExpiresAt). It is deliberately
// a word, not a blank, so the column never reads as missing data.
func formatExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2006-01-02")
}
