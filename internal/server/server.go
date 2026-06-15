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
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/scheduler"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// ErrNoWorkers is the typed "no worker available" seam. With the capacity-aware
// scheduler (#9) the normal dispatch path no longer returns it — a job that fits
// no worker is queued and waits for capacity rather than failing fast — but it
// is retained for callers (and the request path, #13) that may want a fail-fast,
// no-queue variant in future.
var ErrNoWorkers = errors.New("server: no workers connected")

// ErrShuttingDown is returned when a job is submitted during shutdown.
var ErrShuttingDown = errors.New("server: shutting down")

// ErrWorkerNotFound is returned by DrainWorker when no worker has the given id.
var ErrWorkerNotFound = errors.New("server: worker not found")

// Heartbeat lifecycle defaults. The timeout is the window after a worker's last
// heartbeat before it is considered stale and evicted; it defaults to three
// missed intervals so a single dropped heartbeat does not evict a live worker.
const (
	// DefaultHeartbeatInterval is the expected gap between worker heartbeats.
	DefaultHeartbeatInterval = 15 * time.Second
	// DefaultHeartbeatTimeout is how long a worker may go without a heartbeat
	// before it is marked stale and evicted (3x the interval).
	DefaultHeartbeatTimeout = 45 * time.Second
)

// worker is the server's view of one connected worker stream.
type worker struct {
	id string

	// send serializes writes to the worker's stream (a gRPC stream must not be
	// written from multiple goroutines concurrently).
	send chan *agentgpuv1.ServerMessage

	mu      sync.Mutex
	pending map[string]chan types.JobResult // job id -> result waiter
	// streams accumulates the output deltas of in-flight streaming jobs, keyed by
	// job id. The worker sends per-token JobChunks (#11); the server appends each
	// delta here and, on the terminal chunk, resolves the pending waiter with the
	// fully accumulated JobResult. This keeps SubmitJob synchronous (it still
	// returns one final result) while the wire carries a true token stream that
	// the request path (#13) will forward as SSE. Guarded by mu.
	streams map[string]*strings.Builder

	// Capacity/liveness fields reported via heartbeats, guarded by mu. models is
	// seeded from the registration advertisement and refreshed from each
	// heartbeat's available_models.
	models          []types.Model
	lastHeartbeat   time.Time
	activeJobs      uint32
	totalVRAM       uint64
	freeVRAM        uint64
	load            uint32
	gpuType         string
	availableModels []types.Model
	draining        bool
}

// applyHeartbeat folds a heartbeat's capacity report into the worker's view and
// stamps lastHeartbeat with the server clock. All under w.mu.
func (w *worker) applyHeartbeat(hb types.Heartbeat, now time.Time) {
	w.mu.Lock()
	w.lastHeartbeat = now
	w.activeJobs = hb.ActiveJobs
	w.totalVRAM = hb.TotalVRAM
	w.freeVRAM = hb.FreeVRAM
	w.load = hb.Load
	w.gpuType = hb.GPUType
	if hb.AvailableModels != nil {
		w.availableModels = hb.AvailableModels
	}
	w.mu.Unlock()
}

// markDraining flags the worker as draining so pickWorker stops selecting it.
func (w *worker) markDraining() {
	w.mu.Lock()
	w.draining = true
	w.mu.Unlock()
}

// snapshot builds a fleet-view snapshot of the worker. now and timeout classify
// the worker as stale when it has missed heartbeats past the timeout.
func (w *worker) snapshot(now time.Time, timeout time.Duration) types.Worker {
	w.mu.Lock()
	defer w.mu.Unlock()
	models := w.availableModels
	if models == nil {
		models = w.models
	}
	status := types.WorkerOnline
	switch {
	case w.draining:
		status = types.WorkerDraining
	case !w.lastHeartbeat.IsZero() && now.Sub(w.lastHeartbeat) > timeout:
		status = types.WorkerStale
	}
	return types.Worker{
		ID:         w.id,
		Models:     append([]types.Model(nil), models...),
		LastSeen:   w.lastHeartbeat,
		ActiveJobs: w.activeJobs,
		TotalVRAM:  w.totalVRAM,
		FreeVRAM:   w.freeVRAM,
		Load:       w.load,
		GPUType:    w.gpuType,
		Status:     status,
	}
}

// isStale reports whether the worker has missed heartbeats past timeout. A
// worker that has never sent a heartbeat (zero lastHeartbeat) is graced until
// its first one; the writer seeds lastHeartbeat at registration so this holds.
func (w *worker) isStale(now time.Time, timeout time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastHeartbeat.IsZero() {
		return false
	}
	return now.Sub(w.lastHeartbeat) > timeout
}

// available reports whether the worker may receive new jobs: it must be neither
// draining nor stale.
func (w *worker) available(now time.Time, timeout time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.draining {
		return false
	}
	if !w.lastHeartbeat.IsZero() && now.Sub(w.lastHeartbeat) > timeout {
		return false
	}
	return true
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

// accumulate folds one streaming JobChunk into the worker's per-job output
// buffer. Non-terminal chunks append their delta. On the terminal chunk
// (Done = true) it resolves the pending waiter exactly once with the final
// JobResult — the accumulated output plus the reported tokens, or the error if
// the chunk carries one — and discards the buffer. This is the single point
// that turns the worker's token stream back into the synchronous JobResult that
// SubmitJob returns. The request path (#13) will tap the same chunks for SSE.
func (w *worker) accumulate(chunk types.JobChunk) {
	if !chunk.Done {
		w.mu.Lock()
		b := w.streams[chunk.JobID]
		if b == nil {
			b = &strings.Builder{}
			w.streams[chunk.JobID] = b
		}
		b.WriteString(chunk.Delta)
		w.mu.Unlock()
		return
	}

	// Terminal chunk: assemble the final result, drop the buffer, and resolve the
	// waiter (resolve already removes the pending entry and is a no-op if the
	// caller has gone).
	w.mu.Lock()
	var output string
	if b := w.streams[chunk.JobID]; b != nil {
		output = b.String()
		delete(w.streams, chunk.JobID)
	}
	// A late delta on the terminal chunk (some backends include trailing content)
	// is honored.
	output += chunk.Delta
	w.mu.Unlock()

	res := types.JobResult{
		JobID:  chunk.JobID,
		Output: output,
		Err:    chunk.Err,
		Tokens: chunk.Tokens,
	}
	if chunk.Err != nil {
		// On failure the partial output is discarded in favor of the error so the
		// caller does not mistake a truncated stream for a complete answer.
		res.Output = ""
	}
	w.resolve(res)
}

// failAllPending unblocks any outstanding job waiters when the stream drops. It
// also discards any partial stream buffers so a dropped connection leaks no
// per-job state.
func (w *worker) failAllPending(err *types.JobError) {
	w.mu.Lock()
	pending := w.pending
	w.pending = make(map[string]chan types.JobResult)
	w.streams = make(map[string]*strings.Builder)
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
	quota *quota.Engine

	// now is the injectable clock used for heartbeat stamping and eviction
	// (defaults to time.Now). Tests fast-forward it instead of sleeping.
	now func() time.Time
	// heartbeatTimeout is the window after a worker's last heartbeat before it is
	// evicted as stale.
	heartbeatTimeout time.Duration
	// evictScan is the wall-clock cadence at which the eviction loop wakes to
	// re-evaluate staleness against the (possibly injected) clock. Defaults to
	// heartbeatTimeout/2; tests set it small so the loop reacts promptly to a
	// fast-forwarded clock without real sleeps approaching the timeout.
	evictScan time.Duration
	// onDrain, if set, is invoked with a worker's id the moment the server marks
	// it draining in response to a Deregister message — before the subsequent
	// stream-close removeWorker. nil by default; set via WithDrainObserver. It
	// gives operators (and tests) a synchronization point on the graceful-drain
	// transition that does not depend on observing the brief draining→removed
	// window through a polling race.
	onDrain func(workerID string)

	mu      sync.RWMutex
	workers map[string]*worker // by worker id
	nextSes uint64

	// evictDone is closed when the background eviction loop exits; nil until Run
	// (started lazily on first Connect) launches it.
	evictOnce sync.Once
	evictStop chan struct{}
	evictDone chan struct{}

	// queue holds jobs that fit no worker at submit time; the placement loop
	// drains it as capacity frees. Constructed in New.
	queue *queue.Queue

	// waiters maps a queued job's ID to the channel its blocked caller is parked
	// on. The placement loop dispatches the job, then resolves the SAME channel so
	// the original SubmitJob caller receives the result. Guarded by waitersMu.
	waitersMu sync.Mutex
	waiters   map[string]chan types.JobResult

	// capacity is a coalescing signal: a non-blocking send (capacitySignal) wakes
	// the placement loop to re-attempt placement when capacity may have changed (a
	// heartbeat applied, a job completed, a worker registered). A buffered size-1
	// channel coalesces bursts into a single wakeup.
	capacity chan struct{}

	// placeScan bounds how long the placement loop parks between capacity signals
	// before re-checking on its own, so a fast-forwarded clock or a missed signal
	// cannot wedge a queued job. Defaults to evictScan.
	placeScan time.Duration

	// placement loop lifecycle, mirroring the eviction loop.
	placeOnce sync.Once
	placeStop chan struct{}
	placeDone chan struct{}
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

// WithQuota sets the quota engine used to enforce per-key consumption limits on
// the dispatch path. Defaults to an unlimited (no-op) engine so dispatch paths
// that do not configure quotas behave exactly as before.
func WithQuota(q *quota.Engine) Option {
	return func(s *Server) {
		if q != nil {
			s.quota = q
		}
	}
}

// WithClock overrides the time source used for heartbeat stamping and stale
// eviction (for tests). A nil clock is ignored. Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

// WithHeartbeatTimeout sets how long a worker may go without a heartbeat before
// it is evicted as stale. A non-positive value is ignored. Defaults to
// DefaultHeartbeatTimeout.
func WithHeartbeatTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.heartbeatTimeout = d
		}
	}
}

// WithEvictScanInterval overrides the wall-clock cadence at which the eviction
// loop wakes to re-check staleness. A non-positive value is ignored. Defaults
// to heartbeatTimeout/2. Primarily a test seam so the loop reacts to a
// fast-forwarded clock without real sleeps.
func WithEvictScanInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.evictScan = d
		}
	}
}

// WithQueue sets the job queue the scheduler draws from when a submitted job
// fits no worker. A nil queue is ignored. Defaults to an unbounded queue.
// Bounding it (queue.WithMaxDepth) makes a full queue reject with
// queue.ErrQueueFull, which SubmitJob surfaces to the caller for backpressure.
func WithQueue(q *queue.Queue) Option {
	return func(s *Server) {
		if q != nil {
			s.queue = q
		}
	}
}

// WithPlaceScanInterval overrides the wall-clock cadence at which the placement
// loop re-checks for an available worker between capacity signals. A
// non-positive value is ignored. Defaults to evictScan. Primarily a test seam so
// the loop reacts promptly to a fast-forwarded clock.
func WithPlaceScanInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.placeScan = d
		}
	}
}

// WithDrainObserver registers a callback invoked with a worker's id the moment
// the server marks it draining in response to a graceful Deregister, before the
// subsequent stream-close cleanup. A nil callback is ignored. Primarily an
// observability seam (e.g. operator notifications, tests asserting the drain
// transition without racing the brief draining→removed window).
func WithDrainObserver(fn func(workerID string)) Option {
	return func(s *Server) {
		if fn != nil {
			s.onDrain = fn
		}
	}
}

// New constructs a Server.
func New(opts ...Option) *Server {
	s := &Server{
		log:              slog.Default(),
		store:            store.NewMemory(),
		now:              time.Now,
		heartbeatTimeout: DefaultHeartbeatTimeout,
		workers:          make(map[string]*worker),
		evictStop:        make(chan struct{}),
		evictDone:        make(chan struct{}),
		waiters:          make(map[string]chan types.JobResult),
		capacity:         make(chan struct{}, 1),
		placeStop:        make(chan struct{}),
		placeDone:        make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if s.evictScan <= 0 {
		s.evictScan = s.heartbeatTimeout / 2
		if s.evictScan <= 0 {
			s.evictScan = DefaultHeartbeatTimeout / 2
		}
	}
	if s.placeScan <= 0 {
		s.placeScan = s.evictScan
	}
	if s.queue == nil {
		s.queue = queue.New()
	}
	// Default the authorizer to one auditing through the (possibly overridden)
	// server logger, after options have run.
	if s.authz == nil {
		s.authz = authz.NewAuthorizer(authz.WithLogger(s.log))
	}
	// Default the quota engine to an unlimited (no-op) engine over a fresh
	// in-memory counter store, so dispatch behaves unchanged when no quotas are
	// configured. Zero default Limits == unlimited on every dimension.
	if s.quota == nil {
		s.quota = quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithLogger(s.log))
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

// Start launches the background eviction loop that marks workers stale and
// evicts them after the configured heartbeat timeout. It is idempotent and
// safe to call before serving begins; Close stops the loop. Calling Start is
// optional — Connect lazily starts the loop on the first worker — but the cmd
// layer calls it explicitly so the lifecycle is tied to process shutdown.
func (s *Server) Start() {
	s.evictOnce.Do(func() {
		go s.evictLoop()
	})
	s.placeOnce.Do(func() {
		go s.placeLoop()
	})
}

// Close stops the eviction loop and waits for it to exit. Safe to call once;
// further calls are no-ops. It does not tear down active worker streams (the
// gRPC server's GracefulStop owns that).
func (s *Server) Close() error {
	// Ensure the loops were started so the waits below cannot block forever.
	s.Start()
	select {
	case <-s.evictStop:
		// already closed
	default:
		close(s.evictStop)
	}
	<-s.evictDone

	// Stop the placement loop. Closing the queue unblocks its parked DequeueWait;
	// closing placeStop unblocks it if it is parked on a capacity signal. Any
	// callers still blocked on a waiter are released with a shutdown error so they
	// do not hang forever past Close.
	select {
	case <-s.placeStop:
	default:
		close(s.placeStop)
	}
	s.queue.Close()
	<-s.placeDone
	s.failAllWaiters(&types.JobError{Code: "shutting_down", Message: "server shutting down"})
	return nil
}

// evictLoop periodically evicts workers that have missed heartbeats past the
// timeout. It is driven by the injected clock for staleness decisions but wakes
// on a wall-clock ticker so it makes progress regardless of the clock source.
func (s *Server) evictLoop() {
	defer close(s.evictDone)
	ticker := time.NewTicker(s.evictScan)
	defer ticker.Stop()
	for {
		select {
		case <-s.evictStop:
			return
		case <-ticker.C:
			s.evictStale()
		}
	}
}

// evictStale removes every worker whose last heartbeat is older than the
// timeout, failing its pending jobs with a worker_stale error. Stale ids are
// collected under s.mu first, then removed in a second pass so removeWorker can
// take the lock itself (no reentrancy).
func (s *Server) evictStale() {
	now := s.now()
	var stale []*worker
	s.mu.RLock()
	for _, w := range s.workers {
		if w.isStale(now, s.heartbeatTimeout) {
			stale = append(stale, w)
		}
	}
	s.mu.RUnlock()

	for _, w := range stale {
		s.log.Warn("evicting stale worker", "worker", w.id, "timeout", s.heartbeatTimeout.String())
		s.evictWorker(w, &types.JobError{Code: "worker_stale", Message: "worker missed heartbeats"})
	}
}

// evictWorker removes w from the registry (only if it is still the registered
// stream for its id) and fails its pending jobs with err.
func (s *Server) evictWorker(w *worker, err *types.JobError) {
	s.mu.Lock()
	if cur, ok := s.workers[w.id]; ok && cur == w {
		delete(s.workers, w.id)
	}
	s.mu.Unlock()
	w.failAllPending(err)
}

// signalCapacity wakes the placement loop to re-attempt placement of queued
// jobs. It is a non-blocking, coalescing send: if a wakeup is already pending the
// extra signal is dropped (one re-check covers all the capacity that freed since
// the last). Call it whenever capacity may have increased — a heartbeat applied,
// a job completed, or a worker registered.
func (s *Server) signalCapacity() {
	select {
	case s.capacity <- struct{}{}:
	default:
	}
}

// addWaiter registers a result channel for a queued job's ID so the placement
// loop can resolve it once it dispatches the job. The channel is buffered so the
// placement loop never blocks delivering a result whose caller has since gone.
func (s *Server) addWaiter(jobID string) chan types.JobResult {
	ch := make(chan types.JobResult, 1)
	s.waitersMu.Lock()
	s.waiters[jobID] = ch
	s.waitersMu.Unlock()
	return ch
}

// removeWaiter drops a queued job's waiter (caller cancelled / timed out). It
// must be paired with addWaiter so a cancelled caller leaks no entry.
func (s *Server) removeWaiter(jobID string) {
	s.waitersMu.Lock()
	delete(s.waiters, jobID)
	s.waitersMu.Unlock()
}

// resolveWaiter delivers res to the waiter for res.JobID, if one is still
// registered, and removes it. It reports whether a waiter was found.
func (s *Server) resolveWaiter(res types.JobResult) bool {
	s.waitersMu.Lock()
	ch, ok := s.waiters[res.JobID]
	if ok {
		delete(s.waiters, res.JobID)
	}
	s.waitersMu.Unlock()
	if ok {
		ch <- res
	}
	return ok
}

// failAllWaiters releases every outstanding queued-job waiter with err. Used on
// Close so callers blocked on never-placed jobs do not hang past shutdown.
func (s *Server) failAllWaiters(err *types.JobError) {
	s.waitersMu.Lock()
	waiters := s.waiters
	s.waiters = make(map[string]chan types.JobResult)
	s.waitersMu.Unlock()
	for id, ch := range waiters {
		ch <- types.JobResult{JobID: id, Err: err}
	}
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

	// Ensure the eviction loop is running. Start is idempotent, so workers that
	// connect before an explicit Start (e.g. in tests) still get evicted.
	s.Start()

	w := &worker{
		id:      reg.GetWorkerId(),
		models:  types.ModelsFromProto(reg.GetModels()),
		send:    make(chan *agentgpuv1.ServerMessage, 16),
		pending: make(map[string]chan types.JobResult),
		streams: make(map[string]*strings.Builder),
		// Seed lastHeartbeat at registration so a worker that registers but has
		// not yet sent its first heartbeat is graced for one full timeout window
		// rather than being treated as never-seen.
		lastHeartbeat: s.now(),
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
	// A new worker may be able to run queued jobs (e.g. one that advertises a
	// queued job's model at registration): wake the placement loop.
	s.signalCapacity()

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
			hb := types.HeartbeatFromProto(p.Heartbeat)
			w.applyHeartbeat(hb, s.now())
			s.log.Debug("heartbeat", "worker", w.id,
				"active_jobs", hb.ActiveJobs, "load", hb.Load,
				"free_vram", hb.FreeVRAM, "total_vram", hb.TotalVRAM)
			// A heartbeat can free capacity (lower load, more VRAM, a newly loaded
			// model): wake the placement loop to re-attempt queued jobs.
			s.signalCapacity()
		case *agentgpuv1.WorkerMessage_Result:
			w.resolve(types.JobResultFromProto(p.Result))
			// A completed job frees a worker slot: re-attempt queued jobs.
			s.signalCapacity()
		case *agentgpuv1.WorkerMessage_Chunk:
			chunk := types.JobChunkFromProto(p.Chunk)
			w.accumulate(chunk)
			// The terminal chunk frees a worker slot: re-attempt queued jobs.
			if chunk.Done {
				s.signalCapacity()
			}
		case *agentgpuv1.WorkerMessage_Deregister:
			// Graceful drain: stop routing new jobs to this worker, but let its
			// in-flight pending jobs finish. The worker closes the stream once it
			// has drained, at which point removeWorker (deferred) cleans it up.
			w.markDraining()
			s.log.Info("worker draining", "worker", w.id)
			if s.onDrain != nil {
				s.onDrain(w.id)
			}
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

// pickWorker selects the best-fit worker for the model via the capacity-aware
// scheduler (#9): it scores the current fleet snapshot and returns the live
// worker handle for the winning id. The scheduler already filters out
// draining/stale workers (Status != Online), so the selection respects the
// liveness rules. Returns false when no worker is runnable for the model.
//
// There is an inherent snapshot-vs-handle window: the scheduler picks from a
// point-in-time Fleet snapshot, and the chosen worker may have disconnected or
// begun draining by the time we resolve its handle. We re-check the live handle
// with available() under the registry lock to close the obvious cases; a job
// dispatched into a worker that drops immediately after still fails cleanly via
// the worker's failAllPending.
func (s *Server) pickWorker(model string) (*worker, bool) {
	id, ok := scheduler.Pick(s.Fleet(), model)
	if !ok {
		return nil, false
	}
	now := s.now()
	s.mu.RLock()
	w, ok := s.workers[id]
	if ok && !w.available(now, s.heartbeatTimeout) {
		ok = false
	}
	s.mu.RUnlock()
	return w, ok
}

// dispatchTo hands job to worker w and blocks until the worker returns a result,
// the context is done, or the worker's stream drops (failAllPending). It is the
// shared dispatch primitive used by both the synchronous SubmitJob fast path and
// the background placement loop. On ctx cancellation it cleans up the worker's
// pending entry so no waiter leaks.
func (s *Server) dispatchTo(ctx context.Context, w *worker, job types.Job) (types.JobResult, error) {
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

// placeLoop is the background placement loop: it dequeues the highest-priority
// queued job, waits for a worker that can run it (woken promptly by a capacity
// signal, with a bounded periodic re-check as a backstop), dispatches it, and
// resolves the waiter the blocked SubmitJob caller holds. It mirrors the
// eviction loop's Start/Close lifecycle. Each dequeued job is dispatched exactly
// once: it leaves the queue on DequeueWait and is handed to exactly one worker.
func (s *Server) placeLoop() {
	defer close(s.placeDone)

	// The loop's lifetime context is cancelled on Close so a parked DequeueWait
	// and an in-flight placement both unwind.
	ctx, cancel := s.placeContext()
	defer cancel()

	for {
		item, err := s.queue.DequeueWait(ctx)
		if err != nil {
			// ErrClosed (queue closed on shutdown) or ctx cancelled: stop.
			return
		}
		s.placeItem(ctx, item)
	}
}

// placeContext returns a context cancelled when the placement loop is asked to
// stop (placeStop closed).
func (s *Server) placeContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.placeStop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// placeItem waits for a runnable worker for the dequeued item and dispatches it,
// resolving the caller's waiter with the result. If the loop is stopping before
// a worker appears, the item's waiter is failed so the caller does not hang.
func (s *Server) placeItem(ctx context.Context, item queue.Item) {
	ticker := time.NewTicker(s.placeScan)
	defer ticker.Stop()

	for {
		if w, ok := s.pickWorker(item.Job.Model); ok {
			s.log.Info("placing queued job", "key_id", item.Key, "model", item.Job.Model,
				"priority", int(item.Priority), "worker", w.id)
			// Dispatch and forward the result to the original caller's waiter. A
			// fresh context bounds the dispatch to the loop's lifetime (cancelled
			// on Close); the caller's own ctx cancellation is handled separately by
			// SubmitJob, which drops its waiter on cancel.
			res, err := s.dispatchTo(ctx, w, item.Job)
			if err != nil {
				res = types.JobResult{JobID: item.Job.ID, Err: jobErr(err)}
			}
			s.resolveWaiter(res)
			return
		}
		// No worker yet: park until capacity changes, a periodic re-check, or the
		// loop stops.
		select {
		case <-ctx.Done():
			s.resolveWaiter(types.JobResult{
				JobID: item.Job.ID,
				Err:   &types.JobError{Code: "shutting_down", Message: "server shutting down"},
			})
			return
		case <-s.capacity:
		case <-ticker.C:
		}
	}
}

// jobErr coerces an error into a *types.JobError for delivery through a waiter.
func jobErr(err error) *types.JobError {
	var je *types.JobError
	if errors.As(err, &je) {
		return je
	}
	return &types.JobError{Code: "dispatch_failed", Message: err.Error()}
}

// QueueStats returns an observable snapshot of the job queue depth (total and a
// per-priority breakdown). It delegates to the queue and is the metrics seam for
// #24 (Prometheus) until that lands.
func (s *Server) QueueStats() queue.Stats { return s.queue.Stats() }

// Fleet returns a point-in-time snapshot of every connected worker, including
// its reported capacity and computed status (online/draining/stale). It is the
// observability seam for admin endpoints (#4) and the scheduler (#9).
func (s *Server) Fleet() []types.Worker {
	now := s.now()
	s.mu.RLock()
	ws := make([]*worker, 0, len(s.workers))
	for _, w := range s.workers {
		ws = append(ws, w)
	}
	s.mu.RUnlock()

	out := make([]types.Worker, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.snapshot(now, s.heartbeatTimeout))
	}
	return out
}

// DrainWorker marks the worker with the given id as draining: pickWorker stops
// selecting it while its in-flight jobs finish. It is the admin seam for #4
// (operator-initiated drain), distinct from a worker self-deregistering on
// graceful shutdown. Returns ErrWorkerNotFound if no such worker is connected.
func (s *Server) DrainWorker(id string) error {
	s.mu.RLock()
	w, ok := s.workers[id]
	s.mu.RUnlock()
	if !ok {
		return ErrWorkerNotFound
	}
	w.markDraining()
	s.log.Info("worker drain requested", "worker", id)
	return nil
}

// SubmitAuthorizedJob authorizes an already-authenticated key for inference on
// job.Model, enforces its quota, and — if permitted — dispatches the job. It is
// the enforcement seam for the request path (#13), whose flow is:
//
//	key, err := auth.Authenticate(ctx, token)          // 401 on err
//	res, err := srv.SubmitAuthorizedJob(ctx, key, job) // 403 authz.ErrForbidden / 429 quota.ErrQuotaExceeded
//
// The order is authenticate (by the caller) → authorize → quota reserve →
// dispatch → record tokens:
//
//   - authz.Authorize gates the model/action (authz.ErrForbidden → 403).
//   - quota.CheckAndReserve reserves one request against the key's RPM and
//     rejects an already-exhausted token budget (quota.ErrQuotaExceeded → 429,
//     mapped by #6). It runs BEFORE dispatch so a denied request never reaches a
//     worker.
//   - after the job returns, quota.RecordTokens records the tokens the job
//     actually produced (result.Tokens). A failed/zero-token job thus consumes
//     an RPM unit but no token budget.
//
// Authorization and quota both read the key passed in (freshly read by
// Authenticate), so changes take effect without a restart.
//
// NOTE: the pull path is now gated the same way — see PullModel, which calls
// authz.Authorize with authz.Pull before sending a worker any PullModel
// message. The load action (authz.Load) still has no dispatch path; gate it the
// same way when it lands.
func (s *Server) SubmitAuthorizedJob(ctx context.Context, key store.APIKey, job types.Job) (types.JobResult, error) {
	if err := job.Validate(); err != nil {
		return types.JobResult{}, err
	}
	if err := s.authz.Authorize(ctx, key, job.Model, authz.Infer); err != nil {
		return types.JobResult{}, err
	}
	if err := s.quota.CheckAndReserve(ctx, key); err != nil {
		return types.JobResult{}, err
	}
	// Derive the queue priority from the key's roles so that, under contention,
	// higher-privilege keys are placed ahead of lower ones. The priority only
	// matters if the job has to queue (no worker fits now); the fast path is
	// unaffected.
	prio := scheduler.PriorityForRoles(key.Roles)
	res, err := s.submit(ctx, job, key.ID, prio)
	// Record whatever tokens the job produced (zero on dispatch failure). The
	// request itself was already reserved against RPM above.
	s.quota.RecordTokens(ctx, key.ID, res.Tokens)
	if err != nil {
		return res, err
	}
	return res, nil
}

// Quota exposes the configured quota engine (a seam for usage inspection and
// graceful-shutdown checkpointing by the cmd layer).
func (s *Server) Quota() *quota.Engine { return s.quota }

// PullModel authorizes an already-authenticated key for the Pull action on
// model and, if permitted, instructs the named worker to fetch it onto its
// local Ollama. It mirrors SubmitAuthorizedJob's authorize-before-act ordering:
// a denied key returns authz.ErrForbidden and NO PullModel message is sent to
// the worker, so an unauthorized caller never reaches Ollama. The pull itself
// is fire-and-forget: the worker pulls asynchronously and advertises the new
// model on its next heartbeat (auto-pull-on-dispatch-miss is intentionally not
// built here — it is a documented seam for a later epic). Returns
// ErrWorkerNotFound if no such worker is connected.
func (s *Server) PullModel(ctx context.Context, key store.APIKey, workerID, model string) error {
	if err := s.authz.Authorize(ctx, key, model, authz.Pull); err != nil {
		return err
	}
	s.mu.RLock()
	w, ok := s.workers[workerID]
	s.mu.RUnlock()
	if !ok {
		return ErrWorkerNotFound
	}
	select {
	case w.send <- &agentgpuv1.ServerMessage{
		Payload: &agentgpuv1.ServerMessage_PullModel{
			PullModel: &agentgpuv1.PullModel{Model: model},
		},
	}:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.log.Info("pull requested", "key_id", key.ID, "worker", workerID, "model", model)
	return nil
}

// SubmitJob dispatches a job to a worker and waits for the result. This is the
// minimal foundational dispatch primitive with no authorization; callers on the
// public request path must use SubmitAuthorizedJob so inference is gated.
//
// As of #9 the selection is capacity-aware (scheduler.Pick) and a job that fits
// no worker right now is QUEUED (not dropped): the call blocks until the
// background placement loop finds a worker and resolves the result, or ctx is
// done. Keyless internal callers queue at PriorityNormal so the priority lane is
// well-defined; the public path derives priority from the key's roles via
// SubmitAuthorizedJob.
func (s *Server) SubmitJob(ctx context.Context, job types.Job) (types.JobResult, error) {
	return s.submit(ctx, job, "", queue.PriorityNormal)
}

// submit is the shared dispatch core. It validates the job, tries an immediate
// capacity-aware placement, and — on a miss — enqueues the job at the given
// priority and blocks the caller on a server-level waiter until the placement
// loop dispatches it. A queue.ErrQueueFull (bounded queue at depth) is returned
// to the caller for backpressure; ctx cancellation returns ctx.Err() and cleans
// up the waiter so nothing leaks.
func (s *Server) submit(ctx context.Context, job types.Job, keyID string, prio queue.Priority) (types.JobResult, error) {
	if err := job.Validate(); err != nil {
		return types.JobResult{}, err
	}

	// Fast path: a worker fits right now. Dispatch synchronously.
	if w, ok := s.pickWorker(job.Model); ok {
		return s.dispatchTo(ctx, w, job)
	}

	// No worker fits: queue the job and block on a waiter the placement loop will
	// resolve once it places the job on a worker. Register the waiter BEFORE
	// enqueueing so the loop cannot dispatch-and-resolve before we are listening.
	resCh := s.addWaiter(job.ID)
	if err := s.queue.Enqueue(job, keyID, prio); err != nil {
		// Could not queue (full or closed): drop the waiter and surface the error.
		s.removeWaiter(job.ID)
		if errors.Is(err, queue.ErrClosed) {
			return types.JobResult{}, ErrShuttingDown
		}
		s.log.Warn("job rejected: queue full", "key_id", keyID, "model", job.Model,
			"priority", int(prio), "reason", "queue_full")
		return types.JobResult{}, err
	}
	s.log.Info("job queued: no worker available", "key_id", keyID, "model", job.Model,
		"priority", int(prio), "reason", "no_runnable_worker")
	// A worker may appear concurrently; nudge the placement loop in case the
	// capacity signal that would have woken it landed before we enqueued.
	s.signalCapacity()

	select {
	case <-ctx.Done():
		// Caller gave up. Drop the waiter so the placement loop's later resolve is a
		// harmless no-op (the job, if still queued, is dispatched but its result is
		// discarded — at-least-once dispatch, never lost mid-flight).
		s.removeWaiter(job.ID)
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
