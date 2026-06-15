// Package server implements the agent-gpu control-plane gRPC server: the
// authoritative side of the server<->worker bidirectional stream.
//
// Scope for issue #1 is deliberately narrow: accept worker registrations,
// track connected workers in an in-memory registry, and dispatch a single job
// to a connected worker, collecting its result. Real capacity-aware
// scheduling, queueing, auth, and quotas are out of scope and left as seams.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// ErrNoWorkers is returned when a job is submitted but no worker is connected.
var ErrNoWorkers = errors.New("server: no workers connected")

// ErrShuttingDown is returned when a job is submitted during shutdown.
var ErrShuttingDown = errors.New("server: shutting down")

// worker is the server's view of one connected worker stream.
type worker struct {
	id     string
	models []types.Model

	// send serializes writes to the worker's stream (a gRPC stream must not be
	// written from multiple goroutines concurrently).
	send chan *agentgpuv1.ServerMessage

	mu      sync.Mutex
	pending map[string]chan types.JobResult // job id -> result waiter
}

func (w *worker) addPending(jobID string) chan types.JobResult {
	ch := make(chan types.JobResult, 1)
	w.mu.Lock()
	w.pending[jobID] = ch
	w.mu.Unlock()
	return ch
}

func (w *worker) resolve(res types.JobResult) {
	w.mu.Lock()
	ch, ok := w.pending[res.JobID]
	if ok {
		delete(w.pending, res.JobID)
	}
	w.mu.Unlock()
	if ok {
		ch <- res
	}
}

// failAllPending unblocks any outstanding job waiters when the stream drops.
func (w *worker) failAllPending(err *types.JobError) {
	w.mu.Lock()
	pending := w.pending
	w.pending = make(map[string]chan types.JobResult)
	w.mu.Unlock()
	for id, ch := range pending {
		ch <- types.JobResult{JobID: id, Err: err}
	}
}

// Server is the control-plane gRPC service implementation.
type Server struct {
	agentgpuv1.UnimplementedControlPlaneServer

	log   *slog.Logger
	store store.Store
	authz *authz.Authorizer

	mu      sync.RWMutex
	workers map[string]*worker // by worker id
	nextSes uint64
}

// Option configures a Server.
type Option func(*Server)

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.log = l
		}
	}
}

// WithStore sets the persistence backend. Defaults to an in-memory store.
func WithStore(st store.Store) Option {
	return func(s *Server) {
		if st != nil {
			s.store = st
		}
	}
}

// WithAuthorizer sets the authorization engine used to gate job dispatch.
// Defaults to an Authorizer logging through the server's logger.
func WithAuthorizer(a *authz.Authorizer) Option {
	return func(s *Server) {
		if a != nil {
			s.authz = a
		}
	}
}

// New constructs a Server.
func New(opts ...Option) *Server {
	s := &Server{
		log:     slog.Default(),
		store:   store.NewMemory(),
		workers: make(map[string]*worker),
	}
	for _, o := range opts {
		o(s)
	}
	// Default the authorizer to one auditing through the (possibly overridden)
	// server logger, after options have run.
	if s.authz == nil {
		s.authz = authz.NewAuthorizer(authz.WithLogger(s.log))
	}
	return s
}

// Register wires the Server into a gRPC server.
func (s *Server) Register(gs *grpc.Server) {
	agentgpuv1.RegisterControlPlaneServer(gs, s)
}

// Store exposes the backing store (a seam for later auth/quota epics).
func (s *Server) Store() store.Store { return s.store }

// WorkerCount returns the number of currently connected workers.
func (s *Server) WorkerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.workers)
}

// Connect implements the ControlPlane bidirectional stream. One call per
// connected worker; it runs until the worker disconnects or the server's
// context is cancelled.
func (s *Server) Connect(stream agentgpuv1.ControlPlane_ConnectServer) error {
	ctx := stream.Context()

	// The first message must be a registration.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first message must be Register")
	}
	if reg.GetWorkerId() == "" {
		return status.Error(codes.InvalidArgument, "register: worker_id is required")
	}

	w := &worker{
		id:      reg.GetWorkerId(),
		models:  types.ModelsFromProto(reg.GetModels()),
		send:    make(chan *agentgpuv1.ServerMessage, 16),
		pending: make(map[string]chan types.JobResult),
	}

	ses := s.addWorker(w)
	defer s.removeWorker(w)

	sessionID := fmt.Sprintf("%s-%d", w.id, ses)
	w.send <- &agentgpuv1.ServerMessage{
		Payload: &agentgpuv1.ServerMessage_RegisterAck{
			RegisterAck: &agentgpuv1.RegisterAck{SessionId: sessionID},
		},
	}
	s.log.Info("worker registered", "worker", w.id, "session", sessionID, "models", len(w.models))

	// Writer goroutine: the single owner of stream.Send.
	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				sendErr <- ctx.Err()
				return
			case msg := <-w.send:
				if err := stream.Send(msg); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()

	// Reader loop: receive worker -> server messages.
	for {
		select {
		case err := <-sendErr:
			return err
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				s.log.Info("worker disconnected", "worker", w.id)
				return nil
			}
			return err
		}

		switch p := msg.Payload.(type) {
		case *agentgpuv1.WorkerMessage_Heartbeat:
			s.log.Debug("heartbeat", "worker", w.id, "active_jobs", p.Heartbeat.GetActiveJobs())
		case *agentgpuv1.WorkerMessage_Result:
			w.resolve(types.JobResultFromProto(p.Result))
		case *agentgpuv1.WorkerMessage_Register:
			// Re-registration on an established stream is a protocol error.
			return status.Error(codes.FailedPrecondition, "duplicate Register on established stream")
		default:
			s.log.Warn("unknown worker message", "worker", w.id)
		}
	}
}

// addWorker registers the worker and returns its monotonic session number.
// The increment happens under the same lock that guards the registry, so
// concurrent Connect goroutines cannot race on nextSes or hand out duplicate
// session IDs.
func (s *Server) addWorker(w *worker) uint64 {
	s.mu.Lock()
	s.nextSes++
	ses := s.nextSes
	s.workers[w.id] = w
	s.mu.Unlock()
	return ses
}

func (s *Server) removeWorker(w *worker) {
	s.mu.Lock()
	// Only remove if this exact worker is still registered (avoid evicting a
	// reconnected stream that reused the same id).
	if cur, ok := s.workers[w.id]; ok && cur == w {
		delete(s.workers, w.id)
	}
	s.mu.Unlock()
	w.failAllPending(&types.JobError{Code: "worker_disconnected", Message: "worker stream closed"})
}

// pickWorker returns any currently connected worker. This is a placeholder for
// the capacity-aware scheduler introduced by a later epic.
func (s *Server) pickWorker() (*worker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, w := range s.workers {
		return w, true
	}
	return nil, false
}

// SubmitAuthorizedJob authorizes an already-authenticated key for inference on
// job.Model and, if permitted, dispatches the job. It is the enforcement seam
// for the request path (#13), whose flow is:
//
//	key, err := auth.Authenticate(ctx, token)   // 401 on err
//	res, err := srv.SubmitAuthorizedJob(ctx, key, job) // 403 on authz.ErrForbidden
//
// Authorization reads the key's current roles/lists (the caller passes the key
// freshly read by Authenticate), so permission changes take effect without a
// restart. A forbidden job is never handed to a worker; the error is
// authz.ErrForbidden.
//
// NOTE (#11): the pull/load operations have no dispatch path yet. When they
// land, gate them the same way with authz.Pull / authz.Load before touching a
// worker.
func (s *Server) SubmitAuthorizedJob(ctx context.Context, key store.APIKey, job types.Job) (types.JobResult, error) {
	if err := job.Validate(); err != nil {
		return types.JobResult{}, err
	}
	if err := s.authz.Authorize(ctx, key, job.Model, authz.Infer); err != nil {
		return types.JobResult{}, err
	}
	return s.SubmitJob(ctx, job)
}

// SubmitJob dispatches a job to a connected worker and waits for the result.
// This is the minimal foundational dispatch primitive with no authorization;
// callers on the public request path must use SubmitAuthorizedJob so inference
// is gated. Queueing and capacity-aware scheduling remain out of scope.
func (s *Server) SubmitJob(ctx context.Context, job types.Job) (types.JobResult, error) {
	if err := job.Validate(); err != nil {
		return types.JobResult{}, err
	}

	w, ok := s.pickWorker()
	if !ok {
		return types.JobResult{}, ErrNoWorkers
	}

	resCh := w.addPending(job.ID)
	// Hand the job to the worker's writer goroutine. Use select so a cancelled
	// caller (or a writer goroutine that has already exited) does not block us
	// forever on a full or unread send channel.
	select {
	case w.send <- &agentgpuv1.ServerMessage{
		Payload: &agentgpuv1.ServerMessage_Job{Job: job.Proto()},
	}:
	case <-ctx.Done():
		w.mu.Lock()
		delete(w.pending, job.ID)
		w.mu.Unlock()
		return types.JobResult{}, ctx.Err()
	}

	select {
	case <-ctx.Done():
		w.mu.Lock()
		delete(w.pending, job.ID)
		w.mu.Unlock()
		return types.JobResult{}, ctx.Err()
	case res := <-resCh:
		if res.Err != nil {
			return res, res.Err
		}
		return res, nil
	}
}

// keepalive interval/timeout knobs surfaced for the cmd layer to configure.
const (
	// DefaultKeepaliveTime is how often the server pings idle worker streams.
	DefaultKeepaliveTime = 30 * time.Second
	// DefaultKeepaliveTimeout is how long the server waits for a ping ack.
	DefaultKeepaliveTimeout = 10 * time.Second
)
