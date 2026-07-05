package httpapi

import (
	"net/http"

	"github.com/jaypetez/agent-gpu/internal/authz"
)

// Role/scope enumeration (#95): a read-only catalog of the assignable roles and
// the inference actions + admin scopes each grants, plus the full admin-scope
// vocabulary. It exists so a permissions-editor GUI can render the role and
// scope pickers (and the role→capability matrix) directly from the API instead
// of reverse-engineering the authorization engine (authz.decide). The role
// definitions and the scope set are sourced verbatim from internal/authz
// (authz.AllRoles / authz.AllScopes), so this endpoint can never drift from what
// dispatch-time authorization actually enforces.

// adminRolesResponse is the GET /v1/admin/roles response: the assignable roles
// (each with its inference actions, model scope, and admin-scope grant) and the
// complete admin-scope vocabulary. Both arrays are always present (never null).
// Roles are returned in authz's deterministic privilege order (admin → user →
// read-only); Scopes is the sorted scope vocabulary. The document is the source
// of truth a GUI renders its role/scope editor against; it carries no per-key
// state and no secrets.
type adminRolesResponse struct {
	Roles  []adminRoleView `json:"roles"`
	Scopes []string        `json:"scopes"`
}

// adminRoleView is the wire projection of one assignable role for the editor GUI.
// It mirrors authz.RoleInfo field-for-field (name, description, the granted
// inference actions, the breadth those actions apply to, and whether the role is
// the admin-scope superuser); InferenceActions is emitted as [] (never null).
type adminRoleView struct {
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	InferenceActions     []string `json:"inference_actions"`
	ModelScope           string   `json:"model_scope"`
	GrantsAllAdminScopes bool     `json:"grants_all_admin_scopes"`
}

// handleAdminRoles serves GET /v1/admin/roles (#95). It returns the assignable
// roles and their grants (from authz.AllRoles) plus the full admin-scope
// vocabulary (from authz.AllScopes), so a permissions editor can render its
// pickers without reverse-engineering the authorization ladder. It is a pure
// read of static role/scope metadata: no per-key state, no caching, and it is not
// audited (matching the other admin read endpoints). Gated to the keys:read scope
// (s.requireScope, which the RoleAdmin superuser always satisfies), so a key
// lacking it gets 403 and an unauthenticated request 401 before this runs.
func (s *Server) handleAdminRoles(w http.ResponseWriter, _ *http.Request) {
	roles := authz.AllRoles()
	views := make([]adminRoleView, len(roles))
	for i, r := range roles {
		views[i] = adminRoleView{
			Name:                 r.Name,
			Description:          r.Description,
			InferenceActions:     orEmpty(r.InferenceActions),
			ModelScope:           r.ModelScope,
			GrantsAllAdminScopes: r.GrantsAllAdminScopes,
		}
	}
	writeJSON(w, http.StatusOK, adminRolesResponse{
		Roles:  views,
		Scopes: authz.AllScopes(),
	})
}
