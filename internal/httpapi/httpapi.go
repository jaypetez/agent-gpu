// Package httpapi is the public HTTP surface of agent-gpu: the OpenAI-compatible
// API the server fronts for clients. It is the first HTTP server in the project
// (the control plane between server and workers is gRPC-only) and is built to be
// reused: the request-scoped Bearer auth middleware and the JSON/error helpers
// here are shared by the chat/completions path (#13) as it lands.
//
// # Scope
//
// This package ships the OpenAI-compatible API surface:
//
//   - GET  /v1/models           — the OpenAI-canonical model list (#12).
//   - GET  /models              — a richer internal catalog (digest + per-model
//     worker availability) (#12).
//   - POST /v1/chat/completions — chat completions with messages + function/tool
//     calling, non-streaming and SSE streaming (#13).
//   - POST /v1/completions      — legacy text completions, non-streaming and SSE
//     streaming (#13).
//   - POST   /v1/sessions       — create an owner-scoped conversation session (#36).
//   - GET    /v1/sessions/{id}  — session metadata + stored history (#36).
//   - DELETE /v1/sessions/{id}  — end a session and purge its history (#36).
//   - /v1/admin/...             — admin-only key/quota/permission/worker
//     management, gated to admin-role keys (#4). See admin.go.
//
// The chat endpoint also supports two session-aware conversation modes (#36):
// AFFINITY (X-Session-Id header — stateless, server only routes to the warm
// worker) and STATEFUL (session_id body field — server stores history and
// reconstructs the full context each turn). See chat.go and docs/architecture.md.
//
// Every endpoint requires a valid API key (Bearer token). Model discovery is
// permission-filtered per key — a model appears only if the key may run
// inference against it (authz.Infer), so a model a key sees in the catalog is
// exactly a model it may invoke. The inference endpoints gate through the
// control-plane server's SubmitAuthorizedJob / SubmitAuthorizedJobStream, which
// enforce the same authorization plus quota before any worker is touched.
//
// The inference routes are additionally fronted by a server-wide (global) rate
// limiter (rateLimitMiddleware, #6): it reserves against the quota engine's
// global counter before dispatch and returns 429 with a Retry-After header when
// the fleet-wide limit is exceeded, independent of per-key quota. Per-key quota
// 429s (from the submit path) also carry a Retry-After, and both scopes feed the
// throttle metrics exposed by RateLimitStats. See ratelimit.go.
//
// The formal OpenAPI 3.1 spec (#14) is out of scope here; per-key quota is
// enforced (and 429-mapped) because the submit paths reserve against it. The
// auth middleware and the JSON/error/SSE helpers are designed so those plug in
// without rework.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/metrics"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// fleetSource is the subset of *server.Server the HTTP layer needs: a
// point-in-time snapshot of the fleet, the operator-initiated drain used by the
// admin API (#4), and the queue-depth and time-in-queue snapshots the admin
// stats endpoint reports (#10). Narrowing to an interface keeps the handlers
// testable without standing up a full gRPC server and documents the only
// coupling between this package and the control plane. *server.Server satisfies
// it.
type fleetSource interface {
	Fleet() []types.Worker
	DrainWorker(id string) error
	QueueStats() queue.Stats
	WaitTimeStats() server.WaitTimeStats
}

// inferenceEngine is the subset of *server.Server the chat/completions handlers
// need: the gated synchronous and streaming submit paths. Narrowing to an
// interface keeps the handlers unit-testable with a fake engine and documents
// exactly what the HTTP layer asks of the control plane. *server.Server
// satisfies it (SubmitAuthorizedJob and SubmitAuthorizedJobStream).
type inferenceEngine interface {
	SubmitAuthorizedJob(ctx context.Context, key store.APIKey, job types.Job) (types.JobResult, error)
	SubmitAuthorizedJobStream(ctx context.Context, key store.APIKey, job types.Job) (<-chan types.JobChunk, error)
}

// Server is the agent-gpu HTTP API server. It is constructed with the
// control-plane server (for the fleet snapshot), the auth service (to
// authenticate Bearer tokens), and the authorizer (to permission-filter the
// catalog). The same authorizer instance should be shared with the gRPC server
// so catalog visibility matches dispatch-time authorization exactly.
type Server struct {
	fleet  fleetSource
	engine inferenceEngine
	auth   *auth.Service
	authz  *authz.Authorizer
	log    *slog.Logger
	listen string

	// quota backs the admin per-key usage snapshot (GET /v1/admin/keys/{id}/quota).
	// It is the same *quota.Engine the control-plane server reserves against, so
	// the usage the admin API reports is exactly what enforcement sees. It is
	// sourced from grpcSrv.Quota() in NewServer (may be nil if the server was
	// built without a quota engine, e.g. in some unit tests); the handler guards
	// against nil.
	quota *quota.Engine

	// sessionMgr backs the session CRUD endpoints and stateful chat mode (#36).
	// When nil, sessions are disabled: the /v1/sessions endpoints return 501 and
	// a chat request carrying a body session_id is rejected. In cmd it is always
	// set; it is left injectable so unit tests can drive it without standing up
	// the full session subsystem. The same *session.Manager instance is shared
	// with the control-plane server (server.WithSessionManager) so the affinity
	// binding the dispatcher writes and the history this layer reads/writes refer
	// to one source of truth.
	sessionMgr *session.Manager

	// metrics is the Prometheus instrument the request-path middleware/handlers
	// update (request count + latency, tokens generated, throttles) (#24). It is
	// nil-safe: a nil *metrics.Metrics disables all recording, so callers and
	// tests that do not wire metrics in behave exactly as before. The exposition
	// /metrics endpoint and the live server collector live on a separate listener
	// owned by cmd, not on this API mux (so scraping needs no API auth and the
	// OpenAPI route set is unaffected).
	metrics *metrics.Metrics

	// auditLog is the append-only admin audit trail (#90): every admin WRITE
	// records one redacted entry (actor, op, target, before/after, request_id,
	// outcome). It is nil-safe — a nil store makes the recording calls no-ops, so
	// unit tests that do not wire it behave exactly as before — and is set in
	// NewServer when an audit store is supplied. Secrets are never recorded (the
	// before/after projection omits SecretHash/Salt; see admin_audit.go).
	auditLog *audit.MemoryStore

	// idempotency caches the response of an admin WRITE keyed by its
	// Idempotency-Key header so a duplicate request within the TTL replays the
	// prior response instead of re-running the mutation (#90). It is constructed
	// in NewServer; idempotencyOnce lazily constructs it for a Server built via a
	// struct literal (some unit tests) so the middleware never dereferences nil.
	idempotency     *idempotencyCache
	idempotencyOnce sync.Once

	// rlMu guards the rate-limit throttle counters below. A small dedicated mutex
	// (rather than reusing a broader lock) keeps the throttle accounting cheap and
	// off the request hot path's critical sections, mirroring server.affinityMu.
	rlMu sync.Mutex
	// globalThrottled counts requests rejected by the global rate limiter
	// (rateLimitMiddleware); keyThrottled counts per-key quota 429s observed at
	// writeSubmitError. They are the throttle-metrics seam for #24 (Prometheus).
	globalThrottled uint64
	keyThrottled    uint64

	// httpSrv is constructed in NewServer (not in ListenAndServe) so the pointer
	// is non-nil and stable before any goroutine starts or any Shutdown call
	// races startup. This gives a happens-before edge between construction and
	// both ListenAndServe and Shutdown, so there is no data race on the field and
	// no window where Shutdown sees nil and silently no-ops while the listener is
	// still being brought up.
	httpSrv *http.Server
}

// Option configures optional dependencies on an HTTP API Server that are not
// part of the core positional constructor signature. It keeps NewServer
// backward-compatible as cross-cutting subsystems (the admin audit log, #90) are
// threaded in without churning every call site.
type Option func(*Server)

// WithAuditLog wires the append-only admin audit store (#90) so every admin
// WRITE records a redacted entry. A nil store leaves auditing disabled (the
// recording calls become no-ops), so tests and embedders that do not need it are
// unaffected.
func WithAuditLog(a *audit.MemoryStore) Option {
	return func(s *Server) { s.auditLog = a }
}

// NewServer constructs an HTTP API Server. grpcSrv supplies the fleet snapshot,
// authSvc authenticates Bearer tokens, az permission-filters the catalog, mgr
// backs the session API + stateful chat (nil disables sessions), m is the
// Prometheus instrument the request path updates (nil disables metrics, #24),
// log receives structured logs (defaulting to slog.Default() when nil), and
// listen is the host:port to bind. Optional cross-cutting dependencies (e.g. the
// admin audit log) are supplied via Option.
func NewServer(grpcSrv *server.Server, authSvc *auth.Service, az *authz.Authorizer, mgr *session.Manager, m *metrics.Metrics, log *slog.Logger, listen string, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		fleet:       grpcSrv,
		engine:      grpcSrv,
		auth:        authSvc,
		authz:       az,
		quota:       grpcSrv.Quota(),
		sessionMgr:  mgr,
		metrics:     m,
		log:         log,
		listen:      listen,
		idempotency: newIdempotencyCache(defaultIdempotencyTTL, idempotencyCacheMax),
	}
	for _, o := range opts {
		o(s)
	}
	// Build the *http.Server up front so the pointer is stable before any
	// ListenAndServe goroutine or Shutdown call observes it (see field doc).
	s.httpSrv = &http.Server{
		Addr:              listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Handler returns the routed http.Handler for the API. Every route is wrapped
// in the Bearer auth middleware, so an unauthenticated request never reaches a
// handler (and never leaks the catalog). The whole mux is then wrapped in the
// correlation-id middleware (#23), which runs OUTERMOST — before auth — so even
// an unauthenticated 401 carries a request_id and an X-Request-Id response
// header. It is exported so tests can exercise routing through
// net/http/httptest without binding a socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/models", s.authMiddleware(http.HandlerFunc(s.handleOpenAIModels)))
	// OpenAI's retrieve-model endpoint. The {model...} multi-segment wildcard so a
	// namespaced tag (library/x:tag) or a colon tag (qwen2:0.5b) is captured whole
	// via r.PathValue("model"). The bare-path registration above is more specific
	// for "/v1/models" exactly, so it still wins for the list endpoint; this route
	// only matches "/v1/models/<something>".
	mux.Handle("GET /v1/models/{model...}", s.authMiddleware(http.HandlerFunc(s.handleOpenAIModelRetrieve)))
	mux.Handle("/models", s.authMiddleware(http.HandlerFunc(s.handleModels)))
	// The inference surface is the only one fronted by the global rate limiter:
	// rateLimitMiddleware runs INSIDE authMiddleware (so the authenticated key is
	// on the context for the throttle log) but BEFORE the handler, so a global
	// 429 short-circuits before any dispatch. Discovery, session, and admin routes
	// are intentionally NOT rate-limited (they do not consume inference capacity).
	mux.Handle("/v1/chat/completions", s.authMiddleware(s.rateLimitMiddleware(http.HandlerFunc(s.handleChatCompletions))))
	mux.Handle("/v1/completions", s.authMiddleware(s.rateLimitMiddleware(http.HandlerFunc(s.handleCompletions))))
	// Session CRUD (#36). Go 1.22+ method+path patterns let the same path host
	// distinct verbs and capture the id segment via r.PathValue("id").
	mux.Handle("POST /v1/sessions", s.authMiddleware(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("GET /v1/sessions/{id}", s.authMiddleware(http.HandlerFunc(s.handleGetSession)))
	mux.Handle("DELETE /v1/sessions/{id}", s.authMiddleware(http.HandlerFunc(s.handleDeleteSession)))
	s.registerAdminRoutes(mux)
	// Middleware order (outermost first): metrics → requestID → recover → mux.
	// Correlation id is established before auth so every response — including
	// unauthenticated 401s short-circuited by authMiddleware — gets a request_id
	// and X-Request-Id header. The metrics middleware wraps it OUTERMOST so it
	// times the whole handler chain and records the final status of even a
	// short-circuited response (#24); it is a no-op when metrics are disabled, and
	// it installs the statusRecorder the recover middleware reads to learn whether a
	// response has already started. The recover middleware sits INSIDE requestID
	// (so a recovered panic logs with the request_id) but OUTSIDE the mux and the
	// per-route middleware (so it covers every handler), turning a handler panic
	// into a clean 500 rather than a dropped connection.
	return s.metricsMiddleware(s.requestIDMiddleware(s.recoverMiddleware(mux)))
}

// admin wraps an admin handler in the auth + admin-role gates. It is retained as
// the superuser gate (authenticate then require the admin role) for any route
// that is not scope-decomposed; scope-gated routes use requireScope instead. The
// RoleAdmin superuser passes both, so existing admin keys are unaffected (AC2).
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return s.authMiddleware(s.adminMiddleware(h))
}

// requireScope wraps a read (non-mutating) admin handler in the auth + scope
// gates: authenticate, then require the given admin scope (authz.HasScope, which
// the RoleAdmin superuser always satisfies). It is the read counterpart of
// requireScopeWrite (which additionally layers idempotency). Using it on the
// existing admin routes makes the scope matrix real — a key with only
// keys:read passes GET /v1/admin/keys but not the writes — while a RoleAdmin key
// still passes everything.
func (s *Server) requireScope(scope string, h http.HandlerFunc) http.Handler {
	return s.authMiddleware(s.scopeMiddleware(scope, h))
}

// requireScopeWrite wraps an admin WRITE handler in the full admin write stack:
// authenticate → require the scope → idempotency replay → audit-recorded
// handler. The idempotency middleware sits inside the scope gate (so only an
// authorized write is ever cached/replayed) and outside the handler (so it
// captures the handler's response). Audit recording happens inside the handler
// itself (the handler calls s.recordAudit around its mutation), since only the
// handler knows the before/after of the specific resource.
func (s *Server) requireScopeWrite(scope string, h http.HandlerFunc) http.Handler {
	return s.authMiddleware(s.scopeMiddleware(scope, s.idempotent(h)))
}

// registerAdminRoutes mounts the admin API (#4, scoped in #90) on mux. Each
// route is gated by the matching resource×operation scope via requireScope
// (reads) or requireScopeWrite (writes, which additionally layer idempotency and
// audit). The RoleAdmin superuser holds every scope, so an admin key passes every
// route exactly as before. Go 1.22+ method+path patterns let the collection,
// {id}, and {id}/sub routes coexist without collision and pin each verb. See
// admin.go for the handlers and the response shapes.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/admin/keys", s.requireScopeWrite(authz.ScopeKeysWrite, s.handleAdminCreateKey))
	mux.Handle("GET /v1/admin/keys", s.requireScope(authz.ScopeKeysRead, s.handleAdminListKeys))
	mux.Handle("GET /v1/admin/keys/{id}", s.requireScope(authz.ScopeKeysRead, s.handleAdminGetKey))
	mux.Handle("DELETE /v1/admin/keys/{id}", s.requireScopeWrite(authz.ScopeKeysWrite, s.handleAdminRevokeKey))
	mux.Handle("POST /v1/admin/keys/{id}/rotate", s.requireScopeWrite(authz.ScopeKeysWrite, s.handleAdminRotateKey))
	mux.Handle("PUT /v1/admin/keys/{id}/permissions", s.requireScopeWrite(authz.ScopeKeysWrite, s.handleAdminSetPermissions))
	mux.Handle("PUT /v1/admin/keys/{id}/quota", s.requireScopeWrite(authz.ScopeKeysWrite, s.handleAdminSetQuota))
	mux.Handle("GET /v1/admin/keys/{id}/quota", s.requireScope(authz.ScopeKeysRead, s.handleAdminGetQuota))
	mux.Handle("GET /v1/admin/workers", s.requireScope(authz.ScopeWorkersRead, s.handleAdminListWorkers))
	mux.Handle("POST /v1/admin/workers/{id}/drain", s.requireScopeWrite(authz.ScopeWorkersWrite, s.handleAdminDrainWorker))
	mux.Handle("GET /v1/admin/stats", s.requireScope(authz.ScopeTelemetryRead, s.handleAdminStats))
}

// ListenAndServe binds s.listen and serves until the listener is closed or
// Shutdown is called. It returns http.ErrServerClosed on a graceful shutdown,
// which the caller treats as a clean stop.
func (s *Server) ListenAndServe() error {
	s.log.Info("http api listening", "addr", s.listen)
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully drains in-flight requests and stops the server, bounded by
// ctx. The underlying *http.Server is constructed in NewServer, so this always
// acts on a stable, non-nil pointer regardless of whether ListenAndServe has
// started yet: calling Shutdown before (or concurrently with) ListenAndServe is
// race-free and correctly prevents the listener from ever serving (a subsequent
// ListenAndServe returns http.ErrServerClosed).
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}
