package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// ctxKey is an unexported context-key type so values stashed by the middleware
// cannot collide with keys from other packages.
type ctxKey int

const (
	apiKeyContextKey ctxKey = iota
	requestIDContextKey
	requestLoggerContextKey
)

// requestIDHeader is the HTTP header carrying the request/job correlation id
// (#23). An inbound value is honored so a client (or an upstream proxy) can
// supply its own trace id; otherwise the server mints one. It is always echoed
// on the response so a caller can correlate its request with the server-side
// logs, and the same id rides the dispatched job to the worker (it becomes
// Job.ID), giving one id end-to-end: HTTP request → server submit/placement →
// worker job execution.
const requestIDHeader = "X-Request-Id"

// keyFromContext returns the authenticated API key the auth middleware stashed
// on the request context, and whether one was present. Handlers behind
// authMiddleware always find one; it is the seam #13's chat/completions handler
// reads to authorize and meter per key without re-authenticating.
func keyFromContext(ctx context.Context) (store.APIKey, bool) {
	k, ok := ctx.Value(apiKeyContextKey).(store.APIKey)
	return k, ok
}

// requestIDFromContext returns the correlation id the requestID middleware
// stashed on the request context, and whether one was present. Every route is
// behind that middleware so a handler always finds one; it is the id echoed in
// the X-Request-Id response header and used as the dispatched Job.ID.
func requestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDContextKey).(string)
	return id, ok
}

// reqLog returns the request-scoped logger stashed by the requestID middleware:
// the server logger pre-bound with request_id, so any line a handler logs is
// automatically correlated. If no request-scoped logger is present (a handler
// invoked outside the middleware chain, e.g. some unit tests), it falls back to
// the server's base logger so logging never panics.
func (s *Server) reqLog(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(requestLoggerContextKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return s.log
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
			// the caller's; log it but do not echo internals to the client. The
			// request-scoped logger carries the request_id even though auth has not
			// yet stashed a key (requestIDMiddleware ran first, outermost).
			s.reqLog(r.Context()).Error("authentication failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "authentication failed")
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestIDMiddleware is the OUTERMOST middleware on every route (#23): it
// establishes the request/job correlation id before any other middleware runs,
// so even an unauthenticated 401 carries a request_id and an X-Request-Id
// response header. It:
//
//   - honors an inbound X-Request-Id when the client supplies a sane one, else
//     mints a fresh unguessable id (newID with a "req-" prefix);
//   - stashes the id and a request-scoped logger (s.log pre-bound with
//     request_id) on the context for downstream middleware/handlers;
//   - echoes the id in the X-Request-Id response header before next runs (so it
//     is set even on an error response, whose status line is written by a
//     handler/inner middleware).
//
// It deliberately logs nothing itself — never the Authorization header or any
// body — and the response header is the client's hook to correlate its call
// with the server-side logs and the worker's job-execution line.
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newID("req-")
		}
		// Echo before next runs so the header is present on every response,
		// including ones an inner layer short-circuits (401/403/429/…).
		w.Header().Set(requestIDHeader, id)

		ctx := context.WithValue(r.Context(), requestIDContextKey, id)
		ctx = context.WithValue(ctx, requestLoggerContextKey, s.log.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// maxRequestIDLen bounds an accepted inbound correlation id so a client cannot
// bloat logs or response headers with an arbitrarily long value. A minted id is
// 4 + 32 hex chars, so this is comfortably above any legitimate inbound id.
const maxRequestIDLen = 128

// sanitizeRequestID validates a client-supplied X-Request-Id, returning it
// unchanged when acceptable or "" when it must be rejected (so the caller mints
// a fresh id). It accepts only a bounded run of unreserved/safe characters
// (alphanumerics and -_.) so an inbound value can never inject a newline, a
// header-splitting control character, or a log-forging payload into the logs or
// the echoed response header. An over-long or empty value is rejected.
func sanitizeRequestID(v string) string {
	if v == "" || len(v) > maxRequestIDLen {
		return ""
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return ""
		}
	}
	return v
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
