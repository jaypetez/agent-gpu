package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
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

// withSessionLog binds session_id onto the request-scoped logger and returns a
// context carrying the enriched logger (#38). After this, every line a handler
// logs through reqLog for the rest of the request carries BOTH request_id (per
// turn, from requestIDMiddleware) and session_id (across turns), so a multi-turn
// conversation is traceable end-to-end: filter logs by session_id to see the whole
// conversation, by request_id to see one turn. An empty id is a no-op (the logger
// is returned unchanged) so a stateless request never gains an empty session_id
// attribute. It is the session-aware counterpart of the request-id binding and is
// used by the stateful chat path and the session CRUD handlers.
func (s *Server) withSessionLog(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestLoggerContextKey, s.reqLog(ctx).With("session_id", sessionID))
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

// responseStarter is the optional capability recoverMiddleware probes to learn
// whether the response has already started. statusRecorder (the writer wrapper
// the metrics middleware installs outermost) implements it; if the writer in the
// chain does not, recoverMiddleware conservatively assumes the response has NOT
// started and attempts to write the 500 envelope (a no-op double-WriteHeader is
// harmless and the standard library only logs a warning).
type responseStarter interface {
	responseStarted() bool
}

// recoverMiddleware is the safety net that turns a handler panic into a clean
// 500 JSON error envelope instead of a dropped connection (a panicking handler
// otherwise crashes the goroutine and the client sees the connection close with
// no response). It runs INSIDE requestIDMiddleware (so the recovered panic is
// logged with the request_id) but OUTSIDE the mux (so it covers every route and
// every other inner middleware). On a recovered panic it:
//
//   - logs the recovered value and a full stack trace through the request-scoped
//     logger at Error (so the failure is attributable and debuggable), and
//   - writes a 500 internal_error envelope IFF the response has not already
//     started. For a response that has begun (e.g. a streaming handler that
//     panicked after the first SSE frame) the status line is already on the wire,
//     so only logging is possible — writing again would corrupt the response.
//
// It deliberately re-raises nothing: a recovered request fails in isolation
// without taking the server down.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the standard library's sentinel for an
			// intentional abort (e.g. a handler giving up on a hijacked/closed
			// connection); net/http suppresses it and expects it to propagate, so
			// re-panic rather than masking it as a 500.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			s.reqLog(r.Context()).Error("recovered from panic in handler",
				"panic", rec, "stack", string(debug.Stack()))
			// Only write a response if none has started yet. If the chain did not
			// install a response-state tracker, assume not-started and try the write
			// (a double WriteHeader is a logged no-op, never a corruption risk here).
			started := false
			if rs, ok := w.(responseStarter); ok {
				started = rs.responseStarted()
			}
			if !started {
				writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
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

// scopeMiddleware gates a route to keys holding a specific admin scope (#90). It
// runs INSIDE authMiddleware (wrap as s.authMiddleware(s.scopeMiddleware(scope,
// h))), so the key is already authenticated and stashed on the context: it reads
// keyFromContext and requires authz.HasScope(key, scope), otherwise 403. The
// RoleAdmin superuser holds every scope (so existing admin keys pass every
// route, preserving backward compatibility — AC2), while a key granted only a
// specific scope passes exactly its scope-gated routes and a key with neither
// gets 403. As with adminMiddleware, an unauthenticated request never reaches
// here (authMiddleware already returned 401), and the 403 message is generic and
// never echoes the key's id, roles, or scopes.
func (s *Server) scopeMiddleware(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := keyFromContext(r.Context())
		if !ok {
			// Defensive: authMiddleware always stashes a key before us. If it is
			// somehow absent, fail closed as unauthenticated rather than admitting.
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
			return
		}
		if !authz.HasScope(key, scope) {
			writeError(w, http.StatusForbidden, "forbidden", "insufficient scope")
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
