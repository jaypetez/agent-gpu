// Package authz is the authorization engine for agent-gpu: given an
// already-authenticated API key, it decides whether that key may perform an
// action (pull / load / infer) against a named model, and audits every
// decision.
//
// Authentication (who you are) lives in internal/auth; authorization (what you
// may do) lives here. Keeping them in separate packages mirrors the
// ErrUnauthenticated (HTTP 401) / ErrForbidden (HTTP 403) split: auth answers
// "is this a valid key?", authz answers "is this key allowed to do X?".
//
// # Roles
//
// Three built-in roles ship today. The matrix of (role × action) is:
//
//	role         pull   load   infer
//	admin         yes    yes    yes     (and on ALL models, ignoring allow/deny)
//	user          yes*   yes*   yes*    (* only on permitted models)
//	read-only     no     no     yes*    (* only on permitted models)
//
// "permitted" means the model passes the per-key allow/deny precedence below.
//
// # Precedence (deny-wins, deterministic)
//
// Authorize evaluates a fixed order and returns at the first rule that fires:
//
//  1. model in key.DenyModels                      → DENY
//  2. role admin                                    → ALLOW (any model/action)
//  3. role forbids the action (read-only + pull/load) → DENY
//  4. model in key.AllowModels                      → ALLOW
//  5. otherwise                                     → DENY (deny-by-default)
//
// A key with no roles and no allow-list can do nothing: access is opt-in.
//
// # Audit
//
// Every decision is logged via slog with the fields: result
// ("granted"|"denied"), key_id, model, op ("pull"|"load"|"infer"), reason, and
// (when relevant) role. Denials log at Warn, grants at Info. Secrets, tokens,
// and hashes are NEVER logged — only the opaque key_id.
//
// Permissions are read from the live store.APIKey passed in on each call, so
// changes take effect immediately without a restart; this package caches
// nothing.
package authz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// ErrForbidden is returned by Authorize when an authenticated key is not
// permitted to perform the requested action on the requested model. It is the
// typed seam the request path (#13) maps to HTTP 403, mirroring
// auth.ErrUnauthenticated → 401. Match it with errors.Is.
var ErrForbidden = errors.New("authz: forbidden")

// Action is an operation an API key may attempt against a model.
type Action int

const (
	// Pull fetches a model onto a worker (Ollama pull). Out of scope to wire up
	// in #3; the primitive is enforced wherever the pull path lands (#11).
	Pull Action = iota
	// Load loads a model into a worker's memory ready to serve.
	Load
	// Infer runs inference against an already-available model. This is the
	// action enforced on the job-dispatch path today.
	Infer
)

// op returns the audit/op string for an action.
func (a Action) op() string {
	switch a {
	case Pull:
		return "pull"
	case Load:
		return "load"
	case Infer:
		return "infer"
	default:
		return "unknown"
	}
}

// String implements fmt.Stringer for readable error/debug output.
func (a Action) String() string { return a.op() }

// Built-in role names. The (role × action) matrix is documented on the package
// and in docs/architecture.md.
const (
	// RoleAdmin grants every action on every model, bypassing allow/deny lists.
	RoleAdmin = "admin"
	// RoleUser grants pull/load/infer, but only on permitted models.
	RoleUser = "user"
	// RoleReadOnly grants infer only (never pull/load), on permitted models.
	RoleReadOnly = "read-only"
)

// Authorizer decides whether an authenticated key may perform an action on a
// model, auditing each decision. It is stateless beyond its logger and safe for
// concurrent use; permissions come from the store.APIKey passed to Authorize,
// never cached, so updates take effect without a restart.
type Authorizer struct {
	log *slog.Logger
}

// Option configures an Authorizer.
type Option func(*Authorizer)

// WithLogger sets the structured audit logger. A nil logger is ignored
// (the default slog.Default() is kept).
func WithLogger(l *slog.Logger) Option {
	return func(a *Authorizer) {
		if l != nil {
			a.log = l
		}
	}
}

// NewAuthorizer constructs an Authorizer. Without WithLogger it audits to
// slog.Default().
func NewAuthorizer(opts ...Option) *Authorizer {
	a := &Authorizer{log: slog.Default()}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Authorize reports whether key may perform action on model. It returns nil
// when the action is permitted and ErrForbidden when it is not, auditing the
// decision either way. The decision follows the deterministic deny-wins
// precedence documented on the package.
func (a *Authorizer) Authorize(ctx context.Context, key store.APIKey, model string, action Action) error {
	role, reason, allowed := decide(key, model, action)
	if allowed {
		a.audit(ctx, slog.LevelInfo, "granted", key.ID, model, action, reason, role)
		return nil
	}
	a.audit(ctx, slog.LevelWarn, "denied", key.ID, model, action, reason, role)
	return ErrForbidden
}

// decide runs the precedence ladder and returns the deciding role (empty if
// none), a human-readable reason for the audit log, and whether the action is
// allowed. It performs no logging and no I/O so it is trivially testable.
func decide(key store.APIKey, model string, action Action) (role, reason string, allowed bool) {
	// 1. Explicit deny always wins.
	if contains(key.DenyModels, model) {
		return "", "model in deny-list", false
	}
	// 2. admin is permitted everything.
	if contains(key.Roles, RoleAdmin) {
		return RoleAdmin, "role admin", true
	}
	// 3. A role that forbids this action denies regardless of allow-list.
	//    read-only may never pull or load.
	if contains(key.Roles, RoleReadOnly) && (action == Pull || action == Load) {
		return RoleReadOnly, "role read-only forbids " + action.op(), false
	}
	// 4. Model on the allow-list is permitted, provided a granting role is held.
	//    user grants all three actions; read-only grants infer only (and the
	//    pull/load case was already denied in step 3).
	if contains(key.AllowModels, model) {
		switch {
		case contains(key.Roles, RoleUser):
			return RoleUser, "model in allow-list (role user)", true
		case contains(key.Roles, RoleReadOnly):
			// Reaching here means action == Infer (pull/load denied above).
			return RoleReadOnly, "model in allow-list (role read-only)", true
		}
		// Model is allow-listed but the key holds no role that grants the action.
		return "", "model in allow-list but no granting role", false
	}
	// 5. Deny by default.
	return "", "deny by default", false
}

// audit writes one structured decision record. It deliberately logs only the
// opaque key_id — never the secret, hash, token, or model contents beyond the
// model name — so audit output is safe to retain.
func (a *Authorizer) audit(ctx context.Context, level slog.Level, result, keyID, model string, action Action, reason, role string) {
	attrs := []any{
		"result", result,
		"key_id", keyID,
		"model", model,
		"op", action.op(),
		"reason", reason,
	}
	if role != "" {
		attrs = append(attrs, "role", role)
	}
	a.log.Log(ctx, level, "authorization decision", attrs...)
}

// contains reports whether s is present in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
