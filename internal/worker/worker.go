// Package worker implements the agent-gpu worker: the client side of the
// server<->worker control stream. It registers with the server, sends periodic
// heartbeats, executes dispatched jobs, and — crucially — reconnects with
// exponential backoff when the stream drops.
//
// Job execution is a stub for issue #1: it echoes the prompt back. Real Ollama
// integration is out of scope and isolated behind the Executor seam.
package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/jaypetez/agent-gpu/internal/gpu"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// Backoff configures exponential reconnect backoff with full jitter.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Factor float64
}

// DefaultBackoff is the standard reconnect policy.
var DefaultBackoff = Backoff{Base: 500 * time.Millisecond, Max: 30 * time.Second, Factor: 2.0}

// Delay returns the backoff delay for the given attempt (0-based), capped at
// Max, with full jitter applied.
func (b Backoff) Delay(attempt int, rng *rand.Rand) time.Duration {
	base := b.Base
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	factor := b.Factor
	if factor < 1 {
		factor = 2.0
	}
	max := b.Max
	if max <= 0 {
		max = 30 * time.Second
	}
	d := float64(base) * math.Pow(factor, float64(attempt))
	if d > float64(max) || math.IsInf(d, 1) {
		d = float64(max)
	}
	// Full jitter: random in [0, d].
	if rng != nil {
		d = rng.Float64() * d
	}
	return time.Duration(d)
}

// Config configures a Worker.
type Config struct {
	// ServerAddr is the gRPC server address (host:port).
	ServerAddr string
	// WorkerID is this worker's stable identifier.
	WorkerID string
	// Models advertised at registration. They seed the registration message and
	// serve as a fallback/override; once running, heartbeats report the models
	// the Executor lists (sourced from Ollama's /api/tags for the real worker).
	Models []types.Model
	// HeartbeatInterval between heartbeats. Defaults to 15s.
	HeartbeatInterval time.Duration
	// Capacity fields reported in heartbeats. When Detector is set these are
	// overwritten by live GPU detection (static identity once at startup, dynamic
	// free VRAM + load each heartbeat); when Detector is nil they are used as-is
	// as manual overrides (or zero, the CPU profile). TotalVRAM/FreeVRAM are
	// bytes, GPUType is a human-readable description, Load is 0-100.
	TotalVRAM uint64
	FreeVRAM  uint64
	GPUType   string
	Load      uint32
	// Detector, if set, supplies real GPU capacity (#16): NVIDIA/AMD/Apple via
	// their vendor CLIs, with a clean CPU fallback when no GPU is present. It is
	// optional and nil-safe — a nil Detector leaves the static TotalVRAM/GPUType
	// (etc.) fields above as the reported capacity, so existing callers and tests
	// that set those directly keep working unchanged. The real CLI wiring in
	// cmd/agentgpu constructs a gpu.Detector and passes it here.
	Detector CapacityDetector
	// Backoff policy for reconnects.
	Backoff Backoff
	// Executor runs jobs. Defaults to EchoExecutor.
	Executor Executor
	// Logger. Defaults to slog.Default().
	Logger *slog.Logger
	// DialOptions are extra gRPC dial options (tests inject in-process dialers).
	DialOptions []grpc.DialOption
}

// CapacityDetector probes the host's GPU(s) and returns an aggregated capacity
// snapshot. *gpu.Detector satisfies it; tests inject a fake. It must never error
// or panic — it returns a usable Capacity (the CPU fallback) on any trouble — so
// detection can run inline on the heartbeat path without endangering liveness.
type CapacityDetector interface {
	Detect(ctx context.Context) gpu.Capacity
}

func (c *Config) withDefaults() {
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.Backoff == (Backoff{}) {
		c.Backoff = DefaultBackoff
	}
	if c.Executor == nil {
		c.Executor = EchoExecutor{Models: c.Models}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Worker is the control-plane client.
type Worker struct {
	cfg Config
	rng *rand.Rand

	// onConnect, if set, is invoked after each successful registration ack with
	// the session ID returned by the server. Used by tests to observe
	// (re)connection events and assert per-session invariants.
	onConnect func(sessionID string)

	// modelsMu guards models, the most recent successful Executor.ListModels
	// result, refreshed before each heartbeat and after a pull. It is reported as
	// available_models so the server's fleet view tracks model availability
	// (including newly pulled models) without re-registration. It is seeded from
	// the configured Models so a worker advertises something before its first
	// refresh.
	modelsMu sync.Mutex
	models   []types.Model

	// capMu guards capacity, the most recent GPU-detection result reported in
	// heartbeats. When a Detector is configured, the static identity (type +
	// total VRAM) is captured once at startup and the dynamic signals (free VRAM
	// + load) are refreshed before each heartbeat; a refresh failure keeps the
	// last-known value rather than flapping. When no Detector is configured it is
	// seeded once from the static cfg fields (manual override / CPU zero) and left
	// untouched. Guarded like the model cache because the heartbeat reader and the
	// detection refresh touch it from different goroutines.
	capMu    sync.Mutex
	capacity gpu.Capacity
}

// New constructs a Worker from config.
func New(cfg Config) *Worker {
	cfg.withDefaults()
	return &Worker{
		cfg:    cfg,
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
		models: cfg.Models,
		// Seed capacity from the static cfg fields so the worker reports something
		// sensible before its first detection (and forever, when no Detector is
		// set): a manual override, or the all-zero CPU profile.
		capacity: gpu.Capacity{
			Type:      cfg.GPUType,
			TotalVRAM: cfg.TotalVRAM,
			FreeVRAM:  cfg.FreeVRAM,
			Load:      cfg.Load,
		},
	}
}

// currentModels returns a copy of the most recently cached model list.
func (w *Worker) currentModels() []types.Model {
	w.modelsMu.Lock()
	defer w.modelsMu.Unlock()
	return append([]types.Model(nil), w.models...)
}

// refreshModels asks the Executor for the current model list and caches it.
// A failure (e.g. Ollama unreachable) leaves the previous cache intact and is
// logged at debug level; the worker keeps advertising what it last knew rather
// than flapping to empty on a transient blip.
func (w *Worker) refreshModels(ctx context.Context) {
	models, err := w.cfg.Executor.ListModels(ctx)
	if err != nil {
		w.cfg.Logger.Debug("list models failed; keeping cached set", "worker", w.cfg.WorkerID, "err", err)
		return
	}
	w.modelsMu.Lock()
	w.models = models
	w.modelsMu.Unlock()
}

// currentCapacity returns the most recently cached GPU capacity snapshot.
func (w *Worker) currentCapacity() gpu.Capacity {
	w.capMu.Lock()
	defer w.capMu.Unlock()
	return w.capacity
}

// detectCapacity runs the configured Detector and caches the result. full
// controls how much of the snapshot is adopted:
//
//   - full=true (startup): adopt the entire snapshot, including the static
//     identity (Type + TotalVRAM) which is immutable hardware identity.
//   - full=false (per-heartbeat): refresh only the dynamic signals the scheduler
//     reads (FreeVRAM + Load), preserving the startup Type/TotalVRAM so a
//     momentary probe hiccup cannot blank out the hardware identity.
//
// It is a no-op when no Detector is configured (the seeded static capacity
// stands). Detection is bounded by a short timeout and never propagates an
// error: gpu.Detector.Detect already degrades to the CPU fallback on any
// trouble, so the worst case here is reporting the CPU profile, never a failed
// heartbeat or wedged startup.
func (w *Worker) detectCapacity(ctx context.Context, full bool) {
	if w.cfg.Detector == nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cap := w.cfg.Detector.Detect(probeCtx)

	w.capMu.Lock()
	defer w.capMu.Unlock()
	if full {
		w.capacity = cap
		return
	}
	// Dynamic-only refresh: keep the startup-detected identity.
	w.capacity.FreeVRAM = cap.FreeVRAM
	w.capacity.Load = cap.Load
}

// OnConnect registers a callback invoked after each successful registration
// ack with the assigned session ID. Primarily for tests observing
// (re)connection events.
func (w *Worker) OnConnect(fn func(sessionID string)) { w.onConnect = fn }

// Run connects to the server and serves the control stream, reconnecting with
// exponential backoff until ctx is cancelled. It returns ctx.Err() on
// cancellation and only returns a non-context error if it cannot proceed.
func (w *Worker) Run(ctx context.Context) error {
	// Detect the local inference backend on startup. A reachable backend logs its
	// version and seeds the model cache; an unreachable one logs a clear warning
	// and the worker continues DEGRADED — it still registers and heartbeats (with
	// whatever models it last knew, possibly none) so the fleet sees it. This is
	// best-effort and bounded so a slow/hung backend cannot wedge startup.
	w.detectBackend(ctx)

	// Detect local GPU capacity once at startup to capture the hardware identity
	// (type + total VRAM, which are immutable). Free VRAM and load are refreshed
	// per heartbeat below. Like the backend probe this is best-effort, bounded,
	// and degrades to the CPU fallback rather than failing startup. A nil Detector
	// makes this a no-op and the static cfg fields stand.
	w.detectCapacity(ctx, true)

	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// resetBackoff is invoked once runOnce successfully registers, so a
		// long-lived worker that hits a transient drop reconnects promptly
		// instead of inheriting a near-max backoff from earlier failures.
		resetBackoff := func() { attempt = 0 }
		err := w.runOnce(ctx, resetBackoff)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			w.cfg.Logger.Warn("control stream ended; will reconnect", "worker", w.cfg.WorkerID, "err", err)
		}

		delay := w.cfg.Backoff.Delay(attempt, w.rng)
		attempt++
		w.cfg.Logger.Debug("reconnect backoff", "attempt", attempt, "delay", delay.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// versionReporter is an optional Executor capability: a backend that can report
// its version (Ollama does; the echo stub does not). detectBackend uses it for
// the startup probe.
type versionReporter interface {
	Version(ctx context.Context) (string, error)
}

// detectBackend performs the startup backend probe: it reports the backend
// version (if the Executor supports it) and refreshes the model cache. A
// version probe failure is logged as a clear DEGRADED warning and is
// non-fatal — the worker still registers and heartbeats. The probe is bounded
// by a short timeout so an unresponsive backend cannot wedge startup.
func (w *Worker) detectBackend(ctx context.Context) {
	vr, ok := w.cfg.Executor.(versionReporter)
	if !ok {
		// Stub/no-version executor: just seed the model cache best-effort.
		w.refreshModels(ctx)
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	version, err := vr.Version(probeCtx)
	if err != nil {
		w.cfg.Logger.Warn("ollama not reachable at startup; running degraded (will still register/heartbeat)",
			"worker", w.cfg.WorkerID, "err", err)
		return
	}
	w.cfg.Logger.Info("detected ollama", "worker", w.cfg.WorkerID, "version", version)
	w.refreshModels(probeCtx)
}

// dial opens a gRPC connection to the server.
func (w *Worker) dial(ctx context.Context) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	opts = append(opts, w.cfg.DialOptions...)
	return grpc.DialContext(ctx, w.cfg.ServerAddr, opts...)
}

// runOnce performs one full connect → register → serve cycle. It returns when
// the stream drops or ctx is cancelled. resetBackoff, if non-nil, is invoked
// once registration succeeds so the caller can reset reconnect backoff.
func (w *Worker) runOnce(ctx context.Context, resetBackoff func()) error {
	// Scope a cancellable context to this connection so the stream and any
	// goroutines spawned in serve are torn down when this cycle ends — without
	// this, every reconnect would leak the previous cycle's goroutines because
	// they would only observe the long-lived Run context.
	//
	// Crucially this context is NOT a child of ctx (the long-lived Run context).
	// If it were, a graceful shutdown (ctx cancelled) would synchronously abort
	// the gRPC stream, so the Deregister serve tries to send would race a
	// half-dead transport and usually never reach the server. Decoupling lets
	// serve send the Deregister on a still-live stream first and only then
	// return, at which point the deferred cancel tears everything down. ctx is
	// still honored: dial respects it for connect-time abort, and serve selects
	// on it to trigger the graceful Deregister.
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := w.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := agentgpuv1.NewControlPlaneClient(conn)
	stream, err := client.Connect(cctx)
	if err != nil {
		return err
	}

	// Registration handshake.
	if err := stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Register{
			Register: &agentgpuv1.Register{
				WorkerId: w.cfg.WorkerID,
				Models:   types.ModelsToProto(w.currentModels()),
			},
		},
	}); err != nil {
		return err
	}
	ack, err := stream.Recv()
	if err != nil {
		return err
	}
	if ack.GetRegisterAck() == nil {
		return errors.New("worker: expected RegisterAck after Register")
	}
	w.cfg.Logger.Info("registered with server", "worker", w.cfg.WorkerID, "session", ack.GetRegisterAck().GetSessionId())
	if resetBackoff != nil {
		resetBackoff()
	}
	if w.onConnect != nil {
		w.onConnect(ack.GetRegisterAck().GetSessionId())
	}

	return w.serve(ctx, cctx, stream)
}

// serve runs the heartbeat loop and processes dispatched jobs until the stream
// drops. The receive loop runs in its own goroutine; this method owns all
// Sends (heartbeats, job results, deregister) to keep stream writes
// single-threaded.
//
// runCtx is the long-lived Run context. connCtx is the per-connection context
// owned by runOnce, cancelled by its deferred cancel only when this cycle ends;
// it is passed to the spawned goroutines so they are torn down (rather than
// leaked) on every reconnect. connCtx is deliberately NOT derived from runCtx
// (see runOnce) and is NOT selected on here: while serve runs it is never Done,
// so it cannot compete with runCtx.Done() for the shutdown branch.
//
// The two exit triggers are therefore distinct and unambiguous: a graceful
// shutdown surfaces via runCtx.Done() (and sends a Deregister), while a
// transient stream drop (reconnect) surfaces via recvErr and does NOT — a
// recovering worker must not be wrongly drained.
func (w *Worker) serve(runCtx, connCtx context.Context, stream agentgpuv1.ControlPlane_ConnectClient) error {
	// chunks carries every streaming JobChunk (per-token deltas and terminal
	// chunks) from the job worker to this loop, the single owner of stream.Send.
	chunks := make(chan types.JobChunk, 64)
	recvErr := make(chan error, 1)
	jobs := make(chan types.Job, 8)
	pulls := make(chan string, 8)

	// activeJobs counts jobs currently executing, reported in heartbeats. It is
	// touched by the job-worker goroutine and read by this loop, so it must be
	// accessed atomically.
	var activeJobs int32

	// Receive loop.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			switch p := msg.Payload.(type) {
			case *agentgpuv1.ServerMessage_Job:
				jobs <- types.JobFromProto(p.Job)
			case *agentgpuv1.ServerMessage_PullModel:
				if pm := p.PullModel; pm != nil {
					pulls <- pm.GetModel()
				}
			}
		}
	}()

	// emit forwards a streaming chunk to the send loop. It is the callback the
	// Executor calls per token; it drops on connCtx so a dropped connection does
	// not wedge the executor on a full channel.
	emit := func(c types.JobChunk) {
		select {
		case chunks <- c:
		case <-connCtx.Done():
		}
	}

	// Job worker: execute jobs, streaming each output delta as a JobChunk and a
	// terminal JobChunk{Done:true} carrying tokens or error. active-job accounting
	// brackets execution: increment when a job is dequeued, decrement once its
	// terminal chunk is queued (or the connection ends).
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case job := <-jobs:
				atomic.AddInt32(&activeJobs, 1)
				res := w.cfg.Executor.Execute(connCtx, job, emit)
				// Always send a terminal chunk so the server's accumulator resolves the
				// waiter exactly once — even on failure, so the waiter never hangs.
				emit(types.JobChunk{
					JobID:            job.ID,
					Done:             true,
					Err:              res.Err,
					Tokens:           res.Tokens,
					ToolCalls:        res.ToolCalls,
					FinishReason:     res.FinishReason,
					PromptTokens:     res.PromptTokens,
					CompletionTokens: res.CompletionTokens,
				})
				atomic.AddInt32(&activeJobs, -1)
			}
		}
	}()

	// Pull worker: handle PullModel control messages off the receive path so a
	// long pull does not block job dispatch. On success it refreshes the model
	// cache so the next heartbeat advertises the newly pulled model.
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case model := <-pulls:
				if err := w.cfg.Executor.Pull(connCtx, model); err != nil {
					w.cfg.Logger.Warn("model pull failed", "worker", w.cfg.WorkerID, "model", model, "err", err)
					continue
				}
				w.cfg.Logger.Info("model pulled", "worker", w.cfg.WorkerID, "model", model)
				w.refreshModels(connCtx)
			}
		}
	}()

	sendHeartbeat := func() error {
		// Refresh the model cache so the heartbeat advertises the live set
		// (sourced from Ollama for the real worker); a refresh failure keeps the
		// last-known set.
		w.refreshModels(connCtx)
		// Refresh the dynamic GPU signals (free VRAM + load) so each heartbeat
		// carries fresh scheduler-relevant capacity; the immutable identity from
		// startup is preserved. A detection failure degrades to CPU/last-known
		// inside detectCapacity and never blocks the heartbeat.
		w.detectCapacity(connCtx, false)
		return stream.Send(&agentgpuv1.WorkerMessage{
			Payload: &agentgpuv1.WorkerMessage_Heartbeat{
				Heartbeat: w.heartbeat(uint32(atomic.LoadInt32(&activeJobs))),
			},
		})
	}

	ticker := time.NewTicker(w.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			// Graceful shutdown: the long-lived Run context was cancelled.
			// Announce departure so the server marks us draining and stops routing
			// new jobs immediately rather than waiting for the stale timeout.
			//
			// This is the sole context case in the select. An earlier version also
			// watched connCtx.Done(); because connCtx was a child of runCtx, a
			// graceful cancel made both ready at once and Go's select picked
			// pseudo-randomly, dropping the Deregister ~50% of the time. The
			// per-connection teardown that connCtx drives is now handled purely by
			// the goroutines and runOnce's deferred cancel, so graceful shutdown is
			// unambiguous here. Transient drops never reach this case — they surface
			// via recvErr.
			w.deregister(stream, recvErr)
			return runCtx.Err()
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-ticker.C:
			if err := sendHeartbeat(); err != nil {
				return err
			}
		case c := <-chunks:
			if err := stream.Send(&agentgpuv1.WorkerMessage{
				Payload: &agentgpuv1.WorkerMessage_Chunk{Chunk: c.Proto()},
			}); err != nil {
				return err
			}
		}
	}
}

// deregister announces a graceful departure and ensures it is actually
// delivered before the connection is torn down. Sending alone is not enough:
// runOnce's deferred conn.Close() races the in-flight Deregister and can
// truncate it before the server reads it. So after sending we half-close the
// send direction (CloseSend) — a clean end-of-stream the server reads only
// after it has processed the Deregister — then wait for the receive goroutine
// to observe the server tearing its side down (recvErr). At that point the
// Deregister has provably been received and the connection can be closed
// safely. A bounded timeout keeps shutdown from hanging if the server is
// unresponsive. All steps are best-effort: a send/close error just means the
// stream is already gone, which a stale timeout would have reaped anyway.
func (w *Worker) deregister(stream agentgpuv1.ControlPlane_ConnectClient, recvErr <-chan error) {
	if err := stream.Send(&agentgpuv1.WorkerMessage{
		Payload: &agentgpuv1.WorkerMessage_Deregister{
			Deregister: &agentgpuv1.Deregister{WorkerId: w.cfg.WorkerID},
		},
	}); err != nil {
		return
	}
	if err := stream.CloseSend(); err != nil {
		return
	}
	// Wait until the server has read through the Deregister and closed its side
	// (surfacing here as recvErr), so the deferred conn.Close() cannot truncate
	// the message in flight. Bounded so an unresponsive server can't wedge us.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-recvErr:
	case <-timer.C:
	}
}

// heartbeat builds the worker's periodic liveness/capacity report. The capacity
// fields come from the cached GPU-detection snapshot (#16): real
// NVIDIA/AMD/Apple capacity when a Detector is configured (free VRAM + load
// refreshed each heartbeat, type + total VRAM captured once at startup), or the
// static cfg fields / CPU profile when no Detector is set.
func (w *Worker) heartbeat(activeJobs uint32) *agentgpuv1.Heartbeat {
	cap := w.currentCapacity()
	return types.Heartbeat{
		WorkerID:        w.cfg.WorkerID,
		ActiveJobs:      activeJobs,
		TotalVRAM:       cap.TotalVRAM,
		FreeVRAM:        cap.FreeVRAM,
		Load:            cap.Load,
		GPUType:         cap.Type,
		AvailableModels: w.currentModels(),
	}.Proto()
}
