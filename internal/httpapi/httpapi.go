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
//
// Every endpoint requires a valid API key (Bearer token). Model discovery is
// permission-filtered per key — a model appears only if the key may run
// inference against it (authz.Infer), so a model a key sees in the catalog is
// exactly a model it may invoke. The inference endpoints gate through the
// control-plane server's SubmitAuthorizedJob / SubmitAuthorizedJobStream, which
// enforce the same authorization plus quota before any worker is touched.
//
// The formal OpenAPI 3.1 spec (#14), admin endpoints (#4), and dedicated HTTP
// rate-limit middleware (#6) are out of scope here; quota is already enforced
// (and 429-mapped) because the submit paths reserve against it. The auth
// middleware and the JSON/error/SSE helpers are designed so those plug in
// without rework.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// fleetSource is the subset of *server.Server the HTTP layer needs: a
// point-in-time snapshot of the fleet. Narrowing to an interface keeps the
// handlers testable without standing up a full gRPC server and documents the
// only coupling between this package and the control plane.
type fleetSource interface {
	Fleet() []types.Worker
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

	// httpSrv is constructed in NewServer (not in ListenAndServe) so the pointer
	// is non-nil and stable before any goroutine starts or any Shutdown call
	// races startup. This gives a happens-before edge between construction and
	// both ListenAndServe and Shutdown, so there is no data race on the field and
	// no window where Shutdown sees nil and silently no-ops while the listener is
	// still being brought up.
	httpSrv *http.Server
}

// NewServer constructs an HTTP API Server. grpcSrv supplies the fleet snapshot,
// authSvc authenticates Bearer tokens, az permission-filters the catalog, log
// receives structured logs (defaulting to slog.Default() when nil), and listen
// is the host:port to bind.
func NewServer(grpcSrv *server.Server, authSvc *auth.Service, az *authz.Authorizer, log *slog.Logger, listen string) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		fleet:  grpcSrv,
		engine: grpcSrv,
		auth:   authSvc,
		authz:  az,
		log:    log,
		listen: listen,
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
// handler (and never leaks the catalog). It is exported so tests can exercise
// routing through net/http/httptest without binding a socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/models", s.authMiddleware(http.HandlerFunc(s.handleOpenAIModels)))
	mux.Handle("/models", s.authMiddleware(http.HandlerFunc(s.handleModels)))
	mux.Handle("/v1/chat/completions", s.authMiddleware(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("/v1/completions", s.authMiddleware(http.HandlerFunc(s.handleCompletions)))
	return mux
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
