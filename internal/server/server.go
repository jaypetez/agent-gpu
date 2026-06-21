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
	"github.com/jaypetez/agent-gpu/internal/session"
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

// defaultModelWarmMax is the fallback cap on the model-warmth keep_alive window
// (#35) when WithModelWarmMax is not set: a session-bound model is kept resident
// for at most this long after a turn. It mirrors config.DefaultModelWarmMax (one
// hour); the server keeps a local constant rather than importing config so the
// control plane stays free of the config package (matching how it takes plain
// durations like the heartbeat timeout). The cmd layer passes the resolved value
// in via WithModelWarmMax.
const defaultModelWarmMax = time.Hour

// ErrShuttingDown is returned when a job is submitted during shutdown.
var ErrShuttingDown = errors.New("server: shutting down")

// ErrModelUnavailable is returned by the dispatch path when no live (connected,
// non-stale) worker advertises the requested model at submit time. It is the
// fail-fast counterpart to legitimate backpressure: a model that simply does not
// exist on the fleet can never be served by waiting, so the request is rejected
// immediately rather than queued behind a (potentially unbounded) waiter that
// would only resolve if a matching worker happened to appear. The queue-and-block
// path is reserved for the case where a worker that HAS the model is connected
// but cannot take the job right now (busy/draining). The request path (#13) maps
// it to 503 "unavailable". See modelServed for the precise liveness rule.
var ErrModelUnavailable = errors.New("server: no worker serves the requested model")

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
	// observers maps a job id to a channel the streaming request path
	// (SubmitAuthorizedJobStream) subscribes to. When set, accumulate forwards a
	// copy of every JobChunk (per-token deltas AND the terminal chunk) to the
	// observer so the HTTP layer can emit SSE as tokens arrive, in addition to
	// the synchronous accumulate-and-resolve path. The non-streaming pending
	// waiter is independent and unaffected. Guarded by mu.
	observers map[string]chan types.JobChunk

	// registeredAt is the server clock at registration, set once when the worker
	// connects and never mutated thereafter, so it needs no lock for reads. It
	// backs the worker-uptime metric (#24): the collector emits it as
	// worker_start_time_seconds and dashboards derive uptime as time() - it.
	// Uptime resets on reconnect because a re-registering worker is a fresh struct.
	registeredAt time.Time

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
		ID:           w.id,
		Models:       append([]types.Model(nil), models...),
		LastSeen:     w.lastHeartbeat,
		ActiveJobs:   w.activeJobs,
		TotalVRAM:    w.totalVRAM,
		FreeVRAM:     w.freeVRAM,
		Load:         w.load,
		GPUType:      w.gpuType,
		Status:       status,
		RegisteredAt: w.registeredAt,
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

// addObserver registers a streaming observer for jobID. accumulate forwards a
// copy of every chunk for the job to the returned channel (buffered so a brief
// consumer stall does not block the worker reader). It is the seam the
// streaming request path uses to emit SSE as tokens arrive.
func (w *worker) addObserver(jobID string) chan types.JobChunk {
	ch := make(chan types.JobChunk, 64)
	w.mu.Lock()
	if w.observers == nil {
		w.observers = make(map[string]chan types.JobChunk)
	}
	w.observers[jobID] = ch
	w.mu.Unlock()
	return ch
}

// removeObserver detaches the observer for jobID so a client that disconnected
// (or a completed stream) leaves no per-job state behind. It is idempotent.
func (w *worker) removeObserver(jobID string) {
	w.mu.Lock()
	delete(w.observers, jobID)
	w.mu.Unlock()
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
		obs := w.observers[chunk.JobID]
		w.mu.Unlock()
		// Fan a copy of the per-token chunk out to any streaming observer so the
		// HTTP layer can emit SSE as tokens arrive. Non-blocking: a slow/gone
		// consumer must not wedge the worker reader (the synchronous accumulate
		// path below still resolves the waiter regardless).
		forwardChunk(obs, chunk)
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
	obs := w.observers[chunk.JobID]
	delete(w.observers, chunk.JobID)
	w.mu.Unlock()

	// Forward the terminal chunk to the streaming observer and close its channel
	// so the SSE writer flushes the final delta/finish_reason and then ends.
	if obs != nil {
		forwardChunk(obs, chunk)
		close(obs)
	}

	res := types.JobResult{
		JobID:            chunk.JobID,
		Output:           output,
		Err:              chunk.Err,
		Tokens:           chunk.Tokens,
		ToolCalls:        chunk.ToolCalls,
		FinishReason:     chunk.FinishReason,
		PromptTokens:     chunk.PromptTokens,
		CompletionTokens: chunk.CompletionTokens,
	}
	if chunk.Err != nil {
		// On failure the partial output is discarded in favor of the error so the
		// caller does not mistake a truncated stream for a complete answer.
		res.Output = ""
	}
	w.resolve(res)
}

// forwardChunk delivers chunk to a streaming observer without blocking: a slow
// or gone consumer drops the chunk rather than wedging the worker reader. A nil
// channel is a no-op. The channel is closed by the terminal-chunk path in
// accumulate (or failAllPending on a drop), never here.
func forwardChunk(obs chan types.JobChunk, chunk types.JobChunk) {
	if obs == nil {
		return
	}
	select {
	case obs <- chunk:
	default:
	}
}

// failAllPending unblocks any outstanding job waiters when the stream drops. It
// also discards any partial stream buffers and closes any streaming observers
// so a dropped connection leaks no per-job state and the SSE writer unblocks.
func (w *worker) failAllPending(err *types.JobError) {
	w.mu.Lock()
	pending := w.pending
	w.pending = make(map[string]chan types.JobResult)
	w.streams = make(map[string]*strings.Builder)
	observers := w.observers
	w.observers = make(map[string]chan types.JobChunk)
	w.mu.Unlock()
	for id, ch := range pending {
		ch <- types.JobResult{JobID: id, Err: err}
	}
	for id, obs := range observers {
		// Deliver a terminal error chunk then close so the SSE writer emits an
		// error event and ends rather than hanging on a now-dead worker.
		forwardChunk(obs, types.JobChunk{JobID: id, Done: true, Err: err})
		close(obs)
	}
}

// Server is the control-plane gRPC service implementation.
type Server struct {
	agentgpuv1.UnimplementedControlPlaneServer

	log   *slog.Logger
	store store.Store
	authz *authz.Authorizer
	quota *quota.Engine

	// sessionMgr enables session-affinity routing (#34) when set. nil by default,
	// so affinity is disabled and dispatch behaves exactly as before — every
	// existing scheduler/dispatch/quota/authz path is unchanged. Wired by #36.
	sessionMgr *session.Manager

	// warmMu guards modelWarmMax, which is runtime-tunable (#92): SetModelWarmMax
	// replaces it live with no restart, and sessionDispatchHints reads it under
	// warmMu so a concurrent dispatch observes the change atomically. It is a small
	// dedicated mutex separate from s.mu so a warm-window update never contends with
	// the fleet read path.
	warmMu sync.RWMutex
	// modelWarmMax caps the model-warmth keep_alive window (#35): for a
	// session-bound job the dispatcher sets Job.KeepAliveSeconds to
	// min(session TTL, modelWarmMax) so the model stays resident across the
	// conversation and unloads within a BOUNDED window once the session goes idle —
	// an abandoned session can never pin VRAM indefinitely. A session with no idle
	// TTL falls back to exactly this cap. It is only consulted when sessionMgr is
	// set; defaults to config.DefaultModelWarmMax. Session-less jobs are never
	// warmed (KeepAliveSeconds stays 0 → Ollama's own default applies).
	modelWarmMax time.Duration

	// affinityMu guards the affinity hit/miss/rebind counters below. A hit is
	// counted when a turn routes to the worker its session was already bound to; a
	// miss when the session had a binding but a different worker was chosen (a
	// rebind). No counting happens when there is no session or no prior binding.
	//
	// affinityRebinds counts the same events as affinityMisses today — a turn whose
	// session re-bound to a different worker because the bound one was
	// gone/draining/stale/unfit — and is surfaced as the dedicated
	// agentgpu_session_rebinds_total metric (#38). It is tracked as its own counter
	// (rather than reusing misses) so "a session moved workers" stays a distinct,
	// clearly-named signal even if the miss accounting later grows cases that are
	// not rebinds.
	affinityMu      sync.Mutex
	affinityHits    uint64
	affinityMisses  uint64
	affinityRebinds uint64

	// waitMu guards the time-in-queue distribution below: the count of placed-
	// from-queue jobs, the running sum and max of their queue wait (ms), and the
	// cumulative per-bucket counts (parallel to waitBucketBounds). Only jobs that
	// actually queued and were later placed by the placement loop are recorded —
	// the synchronous fast path (a worker fit immediately, wait ≈ 0) is excluded so
	// it does not swamp the distribution. It is the observability seam for #24.
	waitMu      sync.Mutex
	waitCount   uint64
	waitSumMs   uint64
	waitMaxMs   uint64
	waitBuckets []uint64 // cumulative counts, parallel to waitBucketBounds

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
	// onDequeueForPlacement, if set, is invoked with a job's id the moment the
	// placement loop pops it from the queue to attempt placement — before it parks
	// waiting for a runnable worker. nil by default; set via WithPlacementObserver.
	// It is a synchronization seam (chiefly for tests of the time-in-queue
	// distribution): a queued item leaves the queue the instant the placement loop
	// dequeues it, so QueueStats().Total drops to zero before a poller can observe
	// it; this hook gives a deterministic "the job is now queued-and-being-placed"
	// edge to gate a clock advance on, without racing that window.
	onDequeueForPlacement func(jobID string)

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

// WithSessionManager enables session-affinity routing (#34): when a dispatched
// job carries a SessionID, the dispatcher prefers the worker the session is
// bound to (warm KV cache) and records/updates the binding after dispatch. A nil
// manager (the default) disables affinity entirely, leaving dispatch byte-
// identical to the no-session behavior. The cmd layer (#36) wires the manager.
func WithSessionManager(m *session.Manager) Option {
	return func(s *Server) {
		if m != nil {
			s.sessionMgr = m
		}
	}
}

// WithModelWarmMax caps the model-warmth keep_alive window (#35): a session-bound
// job's keep_alive is set to min(session TTL, d) so the conversation's model
// stays resident across active turns and unloads within a bounded window once the
// session goes idle. A non-positive value is ignored (the default applies). It
// only has effect alongside WithSessionManager; the cmd layer wires it from the
// resolved session config (config.ResolveModelWarmMax).
func WithModelWarmMax(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.modelWarmMax = d
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

// waitBucketBounds are the fixed cumulative upper bounds (in milliseconds) of the
// time-in-queue histogram, smallest-first. A job's wait is counted in every
// bucket whose bound is >= the wait, plus a conceptual +Inf bucket (WaitTimeStats
// appends it explicitly, see WaitBucket.LeMs == 0). Mirrors a Prometheus
// le-bucketed histogram so #24 can scrape it directly.
var waitBucketBounds = []uint64{10, 100, 1000, 10000}

// WithPlacementObserver registers a callback invoked with a job's id the instant
// the placement loop dequeues it to attempt placement (before it parks waiting
// for a runnable worker). A nil callback is ignored. Primarily a test seam: it
// gives a deterministic synchronization edge on "the job has been queued and is
// now being placed", which a QueueStats().Total poll cannot reliably catch
// because the item leaves the queue the moment it is dequeued.
func WithPlacementObserver(fn func(jobID string)) Option {
	return func(s *Server) {
		if fn != nil {
			s.onDequeueForPlacement = fn
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
		waitBuckets:      make([]uint64, len(waitBucketBounds)),
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
	if s.modelWarmMax <= 0 {
		s.modelWarmMax = defaultModelWarmMax
	}
	if s.queue == nil {
		// Stamp enqueue times against the server's (possibly injected) clock so the
		// wait-time distribution recorded in placeItem is measured on one clock and
		// tests can advance it deterministically.
		s.queue = queue.New(queue.WithClock(s.now))
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

	// One clock read seeds both the registration timestamp (worker uptime, #24)
	// and the first heartbeat grace, so the two are consistent for this worker.
	registeredAt := s.now()
	w := &worker{
		id:        reg.GetWorkerId(),
		models:    types.ModelsFromProto(reg.GetModels()),
		send:      make(chan *agentgpuv1.ServerMessage, 16),
		pending:   make(map[string]chan types.JobResult),
		streams:   make(map[string]*strings.Builder),
		observers: make(map[string]chan types.JobChunk),
		// registeredAt backs the worker_start_time_seconds metric; set once here
		// and never mutated.
		registeredAt: registeredAt,
		// Seed lastHeartbeat at registration so a worker that registers but has
		// not yet sent its first heartbeat is graced for one full timeout window
		// rather than being treated as never-seen.
		lastHeartbeat: registeredAt,
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
//
// preferredWorkerID, when non-empty, names the session's affinity-bound worker;
// the scheduler gives it a weighted bonus among the runnable candidates (#34).
func (s *Server) pickWorker(model, preferredWorkerID string) (*worker, bool) {
	id, ok := scheduler.PickPreferring(s.Fleet(), model, preferredWorkerID)
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

// modelServed reports whether at least one LIVE worker currently advertises a
// model named model. It is the fail-fast gate the dispatch path consults before
// queueing: a model no connected worker serves can never be satisfied by
// waiting, so submit returns ErrModelUnavailable instead of enqueueing the job
// behind a waiter that would block until the client's own timeout.
//
// "Live" here means present and not stale (Online OR Draining), which is BROADER
// than the scheduler's runnability filter (Online-and-fit) on purpose. The
// distinction is exactly the queue's reason to exist (#9): a model is "served"
// — and a job for it legitimately queues for capacity — whenever a worker that
// has the model is connected, even if no such worker can take the job right now
// (it is draining its in-flight work, or, once capacity gating lands, busy). A
// stale worker (missed heartbeats, about to be evicted) does not count: it is no
// longer a credible home for the model. This keeps the queue-and-block path
// reachable for genuine backpressure while still failing fast when the model is
// simply not on the fleet at all (the headline case: no connected worker has it).
func (s *Server) modelServed(model string) bool {
	for _, wk := range s.Fleet() {
		if wk.Status == types.WorkerStale {
			continue
		}
		for _, m := range wk.Models {
			if m.Name == model {
				return true
			}
		}
	}
	return false
}

// affinityFor resolves the worker a job's session is bound to, for affinity
// routing (#34). It returns "" — meaning "no preference" — unless a session
// manager is configured AND the job carries a SessionID AND the (owner-checked)
// session has a non-empty BoundWorkerID. A missing/not-owned/expired session is
// treated as "no preference" (the lookup error is non-fatal: affinity is only a
// hint). keyID is the owning API key, required to look the session up.
func (s *Server) affinityFor(ctx context.Context, job types.Job, keyID string) string {
	if s.sessionMgr == nil || job.SessionID == "" || keyID == "" {
		return ""
	}
	sess, err := s.sessionMgr.Get(ctx, job.SessionID, keyID)
	if err != nil {
		s.log.Debug("affinity: session lookup failed",
			"session", job.SessionID, "key_id", keyID, "err", err)
		return ""
	}
	return sess.BoundWorkerID
}

// sessionDispatchHints resolves both per-session dispatch hints in a single
// owner-checked session lookup, for the dispatch entry points (submit /
// SubmitAuthorizedJobStream): the affinity-bound worker (#34) and the model-warmth
// keep_alive window in seconds (#35). It returns ("", 0) — no preference, no
// warmth — unless a session manager is configured AND the job carries a SessionID
// AND the session resolves; a missing/not-owned/expired session is non-fatal
// (both are best-effort hints). Folding the two into one Get keeps the hot path to
// a single session read. placeItem still uses affinityFor alone, because a queued
// job already had its keep_alive stamped before it was enqueued.
func (s *Server) sessionDispatchHints(ctx context.Context, job types.Job, keyID string) (preferred string, keepAliveSeconds int64) {
	if s.sessionMgr == nil || job.SessionID == "" || keyID == "" {
		return "", 0
	}
	sess, err := s.sessionMgr.Get(ctx, job.SessionID, keyID)
	if err != nil {
		s.log.Debug("session dispatch hints: lookup failed",
			"session", job.SessionID, "key_id", keyID, "err", err)
		return "", 0
	}
	s.warmMu.RLock()
	warm := s.modelWarmMax
	s.warmMu.RUnlock()
	return sess.BoundWorkerID, warmWindowSeconds(sess.TTL, warm)
}

// warmWindowSeconds derives the keep_alive window (in whole seconds) for a
// session-bound job from the session's idle TTL and the configured cap (#35):
//
//   - TTL > 0: min(TTL, max) — re-sent each turn this resets Ollama's unload
//     timer so the model stays warm across the conversation, and once the session
//     goes idle the model unloads within at most this window.
//   - TTL <= 0 (a session that never idles out): the cap, so even a never-idle
//     session's model is released within a BOUNDED window if the conversation is
//     abandoned — it is never kept "forever".
//
// The result is always >= 1 second (a sub-second window rounds up to 1 rather
// than to 0, which would mean "unload immediately"). max is always positive (the
// server defaults it), so the window is always bounded and positive.
func warmWindowSeconds(ttl, max time.Duration) int64 {
	window := ttl
	if window <= 0 || (max > 0 && window > max) {
		window = max
	}
	secs := int64(window / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// recordAffinity updates the session→worker binding after a successful dispatch
// and counts the affinity hit/miss (#34). preferred is the worker the session
// was bound to before this turn (from affinityFor); chosen is the worker the
// scheduler actually selected. It is a no-op when there is no session.
//
//   - First-turn binding: preferred == "" → bind the session to chosen (no hit/miss
//     counted; there was no prior affinity to honor or break).
//   - Hit: chosen == preferred → the warm worker was reused; count a hit only.
//   - Miss (rebind): chosen != preferred (preferred non-empty) → the bound worker
//     was gone/draining/stale/unfit, so a fresh worker was picked; re-bind the
//     session to chosen and count a miss.
//
// Bind failures are logged and swallowed: a bookkeeping error must never fail an
// inference turn that already succeeded.
func (s *Server) recordAffinity(ctx context.Context, job types.Job, keyID, chosen, preferred string) {
	if s.sessionMgr == nil || job.SessionID == "" || keyID == "" {
		return
	}
	switch {
	case preferred == "":
		// First turn for this session: record the binding.
		if _, err := s.sessionMgr.Bind(ctx, job.SessionID, keyID, chosen); err != nil {
			s.log.Debug("affinity: first-turn bind failed",
				"session", job.SessionID, "key_id", keyID, "worker", chosen, "err", err)
		}
	case chosen == preferred:
		// Routed back to the bound worker: affinity hit, binding already current.
		s.affinityMu.Lock()
		s.affinityHits++
		s.affinityMu.Unlock()
	default:
		// Bound worker was not chosen (rebind): affinity miss, update the binding.
		// A miss here is exactly a rebind (the session moves to a different worker),
		// so bump both counters under the one lock; rebinds surfaces as the dedicated
		// agentgpu_session_rebinds_total metric (#38).
		s.affinityMu.Lock()
		s.affinityMisses++
		s.affinityRebinds++
		s.affinityMu.Unlock()
		s.log.Debug("affinity: rebinding session",
			"session", job.SessionID, "key_id", keyID, "old_worker", preferred, "new_worker", chosen)
		if _, err := s.sessionMgr.Bind(ctx, job.SessionID, keyID, chosen); err != nil {
			s.log.Debug("affinity: rebind failed",
				"session", job.SessionID, "key_id", keyID, "worker", chosen, "err", err)
		}
	}
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
		// Announce the dequeue-for-placement edge (test sync seam): the item has
		// left the queue and a worker is about to be sought. Fired before placeItem
		// parks so a test can gate a clock advance on it without racing the window
		// where QueueStats().Total has already dropped to zero.
		if s.onDequeueForPlacement != nil {
			s.onDequeueForPlacement(item.Job.ID)
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
		// Resolve affinity each attempt: the session's bound worker may appear,
		// drain, or go stale while the job is queued, so re-read it per placement try.
		preferred := s.affinityFor(ctx, item.Job, item.Key)
		if w, ok := s.pickWorker(item.Job.Model, preferred); ok {
			s.log.Info("placing queued job", "key_id", item.Key, "job_id", item.Job.ID, "model", item.Job.Model,
				"priority", int(item.Priority), "worker", w.id)
			// Dispatch and forward the result to the original caller's waiter. A
			// fresh context bounds the dispatch to the loop's lifetime (cancelled
			// on Close); the caller's own ctx cancellation is handled separately by
			// SubmitJob, which drops its waiter on cancel.
			res, err := s.dispatchTo(ctx, w, item.Job)
			if err != nil {
				res = types.JobResult{JobID: item.Job.ID, Err: jobErr(err)}
			} else {
				// Record/update the session binding only after a successful turn.
				s.recordAffinity(ctx, item.Job, item.Key, w.id, preferred)
				// Record the time this job spent queued before being placed, measured
				// on the same (injected) clock that stamped EnqueuedAt. Only this
				// dequeue→dispatch path records: the synchronous fast path never
				// queued, so its near-zero wait is excluded from the distribution.
				s.recordWait(s.now().Sub(item.EnqueuedAt))
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

// AffinityStats is an observable snapshot of session-affinity routing (#34):
// Hits counts turns that routed back to the worker their session was already
// bound to (warm KV cache reused); Misses counts turns that rebound to a
// different worker because the bound one was gone/draining/stale/unfit. Neither
// counts a session's first turn (no prior binding) nor any non-session job.
//
// Rebinds (#38) counts the session→worker rebindings — a turn whose session moved
// to a different worker. It equals Misses today (every miss is a rebind) but is
// reported separately so the rebind signal stays distinct and clearly named; the
// metrics layer exports it as agentgpu_session_rebinds_total.
type AffinityStats struct {
	Hits    uint64
	Misses  uint64
	Rebinds uint64
}

// AffinityStats returns a point-in-time snapshot of the affinity hit/miss/rebind
// counters. It mirrors QueueStats as the metrics seam for #24/#38 (Prometheus).
func (s *Server) AffinityStats() AffinityStats {
	s.affinityMu.Lock()
	defer s.affinityMu.Unlock()
	return AffinityStats{Hits: s.affinityHits, Misses: s.affinityMisses, Rebinds: s.affinityRebinds}
}

// WaitBucket is one cumulative bucket of the time-in-queue histogram: Count is
// the number of placed-from-queue jobs whose queue wait was <= LeMs. A LeMs of 0
// is the sentinel for the +Inf bucket (every recorded job falls in it), so its
// Count always equals WaitTimeStats.Count.
type WaitBucket struct {
	LeMs  uint64 // bucket upper bound in ms; 0 means +Inf
	Count uint64 // cumulative count of waits <= LeMs (or all, for +Inf)
}

// WaitTimeStats is an observable snapshot of the time a job spent queued before
// it was placed on a worker (the dequeue→dispatch path only — the synchronous
// fast path, which never queues, is excluded). Count/SumMs/MaxMs summarize the
// distribution; Buckets are the cumulative le-bucketed histogram (the trailing
// entry, LeMs == 0, is the +Inf bucket). It mirrors AffinityStats as the metrics
// seam for #24 (Prometheus).
type WaitTimeStats struct {
	Count   uint64
	SumMs   uint64
	MaxMs   uint64
	Buckets []WaitBucket
}

// recordWait folds one placed-from-queue job's queue wait into the time-in-queue
// distribution: it bumps the count, adds the wait (in ms) to the running sum,
// raises the max, and increments every cumulative bucket whose bound is >= the
// wait. Only the placement (was-queued) path calls it; the fast path is excluded
// so a flood of near-zero waits does not swamp the distribution. A negative wait
// (clock skew) is clamped to zero. The critical section is a handful of integer
// updates so the hot path lock is held only briefly.
func (s *Server) recordWait(d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	waitMs := uint64(ms)

	s.waitMu.Lock()
	s.waitCount++
	s.waitSumMs += waitMs
	if waitMs > s.waitMaxMs {
		s.waitMaxMs = waitMs
	}
	for i, bound := range waitBucketBounds {
		if waitMs <= bound {
			s.waitBuckets[i]++
		}
	}
	s.waitMu.Unlock()
}

// WaitTimeStats returns a point-in-time copy of the time-in-queue distribution.
// Buckets are cumulative and ordered smallest-bound-first, with a trailing +Inf
// bucket (LeMs == 0) whose count equals Count.
func (s *Server) WaitTimeStats() WaitTimeStats {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	buckets := make([]WaitBucket, 0, len(waitBucketBounds)+1)
	for i, bound := range waitBucketBounds {
		buckets = append(buckets, WaitBucket{LeMs: bound, Count: s.waitBuckets[i]})
	}
	// The +Inf bucket holds every recorded wait (LeMs == 0 is the sentinel).
	buckets = append(buckets, WaitBucket{LeMs: 0, Count: s.waitCount})
	return WaitTimeStats{
		Count:   s.waitCount,
		SumMs:   s.waitSumMs,
		MaxMs:   s.waitMaxMs,
		Buckets: buckets,
	}
}

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
	// request itself was already reserved against RPM above. Mirror the tokens
	// onto the global minute-token counter so a configured global TPM reflects
	// fleet-wide usage (no-op when no global limits are set).
	s.quota.RecordTokens(ctx, key.ID, res.Tokens)
	s.quota.RecordGlobalTokens(ctx, res.Tokens)
	if err != nil {
		return res, err
	}
	return res, nil
}

// SubmitAuthorizedJobStream is the streaming counterpart of SubmitAuthorizedJob
// for /v1/chat/completions and /v1/completions with stream=true. It gates the
// key exactly as the non-streaming path (authz.Infer → quota.CheckAndReserve)
// BEFORE any worker is touched, then dispatches the job and returns a channel
// the caller drains for live JobChunks (per-token deltas plus the terminal
// chunk), which the HTTP layer forwards as Server-Sent Events.
//
// Ordering and cleanup:
//
//   - Validate → Authorize (authz.ErrForbidden) → CheckAndReserve
//     (quota.ErrQuotaExceeded) → pick a worker. A streaming request needs a live
//     worker to attach its observer to, so if no worker can run the model right
//     now it returns queue.ErrQueueFull (the HTTP layer maps that to 503); unlike
//     the non-streaming path it does not queue-and-block, since there is no
//     stream to attach to until a worker exists.
//   - The returned channel is closed by the worker's accumulate path on the
//     terminal chunk (or by failAllPending if the worker drops), so the caller's
//     range loop ends cleanly.
//   - Tokens are recorded against the key when the terminal chunk arrives, in a
//     goroutine that also drives dispatch and ctx-cancel cleanup.
//   - Cancelling ctx (client disconnect) aborts the upstream dispatch: the
//     dispatch goroutine's dispatchTo returns on ctx.Done, removing the worker's
//     pending entry; the observer is detached so no per-job state leaks.
//
// The returned error is non-nil only when gating/dispatch-setup fails before any
// chunk could flow; once a non-nil channel is returned, all outcomes (including
// errors) surface as a terminal JobChunk on it.
func (s *Server) SubmitAuthorizedJobStream(ctx context.Context, key store.APIKey, job types.Job) (<-chan types.JobChunk, error) {
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if err := s.authz.Authorize(ctx, key, job.Model, authz.Infer); err != nil {
		return nil, err
	}
	if err := s.quota.CheckAndReserve(ctx, key); err != nil {
		return nil, err
	}
	// Resolve session affinity (#34) and model warmth (#35) in one lookup: prefer
	// the bound worker and stamp the keep_alive window so the worker keeps the
	// model resident across the conversation. Both are zero-valued without a
	// session/manager, leaving the stream path unchanged.
	preferred, keepAlive := s.sessionDispatchHints(ctx, job, key.ID)
	job.KeepAliveSeconds = keepAlive
	w, ok := s.pickWorker(job.Model, preferred)
	if !ok {
		// No live worker for the model: there is nothing to attach a stream to.
		// Distinguish "no worker serves this model at all" (a fail-fast 503 with a
		// clear "unavailable" message) from "a capable worker exists but is busy"
		// (legitimate backpressure, surfaced like a full queue) so the streaming
		// path matches the non-streaming submit's diagnosis.
		if !s.modelServed(job.Model) {
			return nil, ErrModelUnavailable
		}
		// A capable worker exists but none can take the stream right now. Surface
		// backpressure the same way the queue would, mapped to 503.
		return nil, queue.ErrQueueFull
	}
	// Record/update the binding now that a worker is committed for this turn. The
	// stream still flows the same chunks; affinity is bookkeeping only and a Bind
	// error never blocks the stream.
	s.recordAffinity(ctx, job, key.ID, w.id, preferred)

	// Register the observer BEFORE dispatching so no chunk can be produced and
	// folded by accumulate before the observer is listening.
	obs := w.addObserver(job.ID)

	out := make(chan types.JobChunk, 64)
	go func() {
		defer close(out)
		// Drive dispatch in a sibling goroutine: dispatchTo blocks until the
		// terminal result (which also resolves the pending waiter accumulate uses),
		// returns its token count for quota accounting, and — crucially — removes
		// the worker's pending entry on ctx cancellation so a client disconnect
		// aborts the upstream job.
		done := make(chan types.JobResult, 1)
		go func() {
			res, _ := s.dispatchTo(ctx, w, job)
			done <- res
		}()

		for {
			select {
			case <-ctx.Done():
				// Client disconnected: detach the observer and stop forwarding. The
				// dispatch goroutine observes ctx.Done too and tears down the pending
				// entry; tokens for an aborted stream are not recorded.
				w.removeObserver(job.ID)
				return
			case chunk, alive := <-obs:
				if !alive {
					// Observer closed by accumulate/failAllPending: stream complete.
					// Drain the dispatch result for quota accounting.
					res := <-done
					s.quota.RecordTokens(ctx, key.ID, res.Tokens)
					s.quota.RecordGlobalTokens(ctx, res.Tokens)
					return
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					w.removeObserver(job.ID)
					return
				}
			}
		}
	}()

	return out, nil
}

// Quota exposes the configured quota engine (a seam for usage inspection and
// graceful-shutdown checkpointing by the cmd layer).
func (s *Server) Quota() *quota.Engine { return s.quota }

// SetHeartbeatTimeout replaces the stale-eviction window at runtime (#92): the
// length of time a worker may go without a heartbeat before the eviction loop
// reaps it. A non-positive value is rejected (the existing timeout is kept) so
// eviction is never disabled by a bad value; the runtime-config layer validates
// d > 0 before calling, so a rejection here is a defensive backstop. It takes
// effect immediately with no restart — evictStale, pickWorker, and Fleet all read
// heartbeatTimeout under s.mu, and this writes it under s.mu.Lock() — and is safe
// for concurrent use. The eviction-loop scan cadence (evictScan) is unchanged; the
// next scan simply uses the new timeout.
func (s *Server) SetHeartbeatTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatTimeout = d
}

// HeartbeatTimeout returns the current stale-eviction window (#92), read under
// s.mu so it reflects any live SetHeartbeatTimeout. It backs the admin config GET
// projection.
func (s *Server) HeartbeatTimeout() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.heartbeatTimeout
}

// SetModelWarmMax replaces the model-warmth keep_alive cap at runtime (#92): the
// longest a session-bound model is kept resident on its worker after a turn. A
// non-positive value is rejected (the existing cap is kept) so the derived warm
// window stays bounded; the runtime-config layer validates d > 0 before calling.
// It takes effect on the next session-bound dispatch with no restart
// (sessionDispatchHints reads it under warmMu) and is safe for concurrent use.
func (s *Server) SetModelWarmMax(d time.Duration) {
	if d <= 0 {
		return
	}
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	s.modelWarmMax = d
}

// ModelWarmMax returns the current model-warmth keep_alive cap (#92), read under
// warmMu so it reflects any live SetModelWarmMax. It backs the admin config GET
// projection.
func (s *Server) ModelWarmMax() time.Duration {
	s.warmMu.RLock()
	defer s.warmMu.RUnlock()
	return s.modelWarmMax
}

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

// UnloadSessionModel best-effort asks workerID to evict model from its Ollama,
// freeing the VRAM (#35). It is the explicit-release path for model warmth: the
// HTTP layer calls it after an authorized session is ended (DELETE /v1/sessions)
// so the conversation's model is released promptly on the worker it was bound to,
// rather than lingering for the remainder of its keep_alive window. It is
// deliberately unauthenticated at this layer — the caller has already
// owner-checked and deleted the session — and entirely best-effort:
//
//   - An empty workerID or model is a no-op (an unbound or model-less session has
//     nothing to release).
//   - A worker that is not connected (drained/stale/never-bound) is a no-op, not
//     an error: its model is already gone or will be reaped by Ollama's idle
//     timer, the backstop release path.
//   - A full/again-closed worker send channel is dropped rather than blocking the
//     delete response; the keep_alive timer still releases the model.
//
// It never returns an error to the caller, so a release attempt can never fail a
// session-delete the client already succeeded at; failures are logged at debug.
func (s *Server) UnloadSessionModel(ctx context.Context, workerID, model string) {
	if workerID == "" || model == "" {
		return
	}
	s.mu.RLock()
	w, ok := s.workers[workerID]
	s.mu.RUnlock()
	if !ok {
		s.log.Debug("unload skipped: worker not connected", "worker", workerID, "model", model)
		return
	}
	select {
	case w.send <- &agentgpuv1.ServerMessage{
		Payload: &agentgpuv1.ServerMessage_UnloadModel{
			UnloadModel: &agentgpuv1.UnloadModel{Model: model},
		},
	}:
		s.log.Info("session model unload requested", "worker", workerID, "model", model)
	case <-ctx.Done():
	default:
		// Worker writer is not keeping up (or has exited); do not block the caller.
		// Ollama's idle keep_alive timer will release the model regardless.
		s.log.Debug("unload dropped: worker send not ready", "worker", workerID, "model", model)
	}
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

	// Resolve session affinity (#34) and model warmth (#35) in one lookup: prefer
	// the worker this conversation is bound to, and stamp the keep_alive window so
	// the worker keeps the model resident across the session. Both are zero-valued
	// when there is no session/manager, leaving placement and the worker request
	// byte-identical to the no-session path. Stamp keep_alive on the job BEFORE the
	// fast-path dispatch and before enqueue so both the synchronous and queued
	// dispatch paths carry it.
	preferred, keepAlive := s.sessionDispatchHints(ctx, job, keyID)
	job.KeepAliveSeconds = keepAlive

	// Fail fast when no live worker serves the model at all: there is nothing to
	// wait for, so reject immediately rather than enqueueing behind a waiter that
	// would block until the caller's own timeout. The queue-and-block path below is
	// reserved for the case where a worker that HAS the model is connected but
	// cannot take the job right now (busy/draining — legitimate backpressure).
	// Checked after affinity/keep-alive resolution but before the fast-path
	// pick/enqueue so neither path can park on an unserved model.
	if !s.modelServed(job.Model) {
		return types.JobResult{}, ErrModelUnavailable
	}

	// Fast path: a worker fits right now. Dispatch synchronously.
	if w, ok := s.pickWorker(job.Model, preferred); ok {
		res, err := s.dispatchTo(ctx, w, job)
		if err == nil {
			// Record/update the binding only after a successful turn.
			s.recordAffinity(ctx, job, keyID, w.id, preferred)
		}
		return res, err
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
		s.log.Warn("job rejected: queue full", "key_id", keyID, "job_id", job.ID, "model", job.Model,
			"priority", int(prio), "reason", "queue_full")
		return types.JobResult{}, err
	}
	s.log.Info("job queued: no worker available", "key_id", keyID, "job_id", job.ID, "model", job.Model,
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
