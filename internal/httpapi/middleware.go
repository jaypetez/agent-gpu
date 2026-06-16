package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// ctxKey is an unexported context-key type so the authenticated key stashed by
// the middleware cannot collide with keys from other packages.
type ctxKey int

const apiKeyContextKey ctxKey = iota

// keyFromContext returns the authenticated API key the auth middleware stashed
// on the request context, and whether one was present. Handlers behind
// authMiddleware always find one; it is the seam #13's chat/completions handler
// reads to authorize and meter per key without re-authenticating.
func keyFromContext(ctx context.Context) (store.APIKey, bool) {
	k, ok := ctx.Value(apiKeyContextKey).(store.APIKey)
	return k, ok
}

// authMiddleware authenticates the request's Bearer token and, on success,
// stashes the resolved store.APIKey on the request context before calling next.
// It is the single shared entry point every authenticated HTTP route wraps —
// model discovery today, chat/completions (#13) next — so authentication
// behaves identically across the API.
//
// It maps failures deterministically:
//
//   - missing or malformed Authorization header  → 401 (no Authenticate call)
//   - auth.ErrUnauthenticated                     → 401
//   - any other Authenticate error                → 500
//
// A 401 never reveals whether the key was unknown, revoked, or malformed, and
// no downstream handler runs, so the catalog is never leaked to an
// unauthenticated caller.
//
// Note: a successful Authenticate bumps the key's UsageCount/LastUsedAt, so
// discovery calls to /v1/models and /models count as key usage. When HTTP
// rate-limiting / usage metrics land (#6), they must not double-count these
// discovery requests against inference usage.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
			return
		}
		key, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid api key")
				return
			}
			// An unexpected error (e.g. store failure) is the server's fault, not
			// the caller's; log it but do not echo internals to the client.
			s.log.Error("authentication failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "authentication failed")
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// adminMiddleware gates a route to admin-role keys (#4). It runs INSIDE
// authMiddleware (wrap as s.authMiddleware(s.adminMiddleware(h))), so the key is
// already authenticated and stashed on the context: it reads keyFromContext and
// requires authz.RoleAdmin among the key's roles, otherwise 403. An
// unauthenticated request never reaches here (authMiddleware already returned
// 401), so the two responses are cleanly separated: 401 for "who are you" and
// 403 for "you may not". The 403 message is deliberately generic and never
// echoes the key's id or roles.
func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := keyFromContext(r.Context())
		if !ok {
			// Defensive: authMiddleware always stashes a key before us. If it is
			// somehow absent, fail closed as unauthenticated rather than admitting.
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
			return
		}
		if !hasRole(key.Roles, authz.RoleAdmin) {
			writeError(w, http.StatusForbidden, "forbidden", "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hasRole reports whether role appears in roles. Role lists are tiny, so a
// linear scan is the right tool.
func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
// It returns false when the header is absent, not a Bearer scheme, or carries an
// empty token. The scheme match is case-insensitive per RFC 7235.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
