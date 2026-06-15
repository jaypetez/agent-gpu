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
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// Executor runs an inference job and returns its result. The default echo
// executor is a stub; the Ollama epic provides the real implementation.
type Executor interface {
	Execute(ctx context.Context, job types.Job) types.JobResult
}

// EchoExecutor is the stub executor: it echoes the prompt back as output.
type EchoExecutor struct{}

// Execute implements Executor. It reports a token count equal to the number of
// whitespace-separated tokens in the output so quota accounting (#5) is
// testable now; real token counts arrive with the Ollama integration (#11).
func (EchoExecutor) Execute(_ context.Context, job types.Job) types.JobResult {
	output := "echo: " + job.Prompt
	return types.JobResult{
		JobID:  job.ID,
		Output: output,
		Tokens: uint64(len(strings.Fields(output))),
	}
}

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
	// Models advertised at registration and reported as available_models in
	// heartbeats.
	Models []types.Model
	// HeartbeatInterval between heartbeats. Defaults to 15s.
	HeartbeatInterval time.Duration
	// Capacity stub fields reported in heartbeats. Real GPU detection (#16)
	// replaces these; until then they are configured/zero. TotalVRAM/FreeVRAM are
	// bytes, GPUType is a human-readable description, Load is 0-100.
	TotalVRAM uint64
	FreeVRAM  uint64
	GPUType   string
	Load      uint32
	// Backoff policy for reconnects.
	Backoff Backoff
	// Executor runs jobs. Defaults to EchoExecutor.
	Executor Executor
	// Logger. Defaults to slog.Default().
	Logger *slog.Logger
	// DialOptions are extra gRPC dial options (tests inject in-process dialers).
	DialOptions []grpc.DialOption
}

func (c *Config) withDefaults() {
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.Backoff == (Backoff{}) {
		c.Backoff = DefaultBackoff
	}
	if c.Executor == nil {
		c.Executor = EchoExecutor{}
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
}

// New constructs a Worker from config.
func New(cfg Config) *Worker {
	cfg.withDefaults()
	return &Worker{
		cfg: cfg,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// OnConnect registers a callback invoked after each successful registration
// ack with the assigned session ID. Primarily for tests observing
// (re)connection events.
func (w *Worker) OnConnect(fn func(sessionID string)) { w.onConnect = fn }

// Run connects to the server and serves the control stream, reconnecting with
// exponential backoff until ctx is cancelled. It returns ctx.Err() on
// cancellation and only returns a non-context error if it cannot proceed.
func (w *Worker) Run(ctx context.Context) error {
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
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := w.dial(cctx)
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
				Models:   types.ModelsToProto(w.cfg.Models),
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
// runCtx is the long-lived Run context; connCtx is the per-connection context
// (cancelled by runOnce on return) so the spawned goroutines are torn down when
// this connection ends rather than leaking. The two are distinguished so a
// graceful shutdown (runCtx cancelled) can send a Deregister before closing,
// while a transient drop (only connCtx cancelled) does not.
func (w *Worker) serve(runCtx, connCtx context.Context, stream agentgpuv1.ControlPlane_ConnectClient) error {
	results := make(chan types.JobResult, 8)
	recvErr := make(chan error, 1)
	jobs := make(chan types.Job, 8)

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
			if job := msg.GetJob(); job != nil {
				jobs <- types.JobFromProto(job)
			}
		}
	}()

	// Job worker: execute jobs (stub) and queue results for sending. active-job
	// accounting brackets execution: increment when a job is dequeued, decrement
	// once its result is queued (or the connection ends).
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case job := <-jobs:
				atomic.AddInt32(&activeJobs, 1)
				res := w.cfg.Executor.Execute(connCtx, job)
				select {
				case results <- res:
					atomic.AddInt32(&activeJobs, -1)
				case <-connCtx.Done():
					atomic.AddInt32(&activeJobs, -1)
					return
				}
			}
		}
	}()

	sendHeartbeat := func() error {
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
			// new jobs immediately rather than waiting for the stale timeout. We
			// send while the stream is still live — runOnce cancels connCtx (which
			// tears down the stream) only after serve returns. Best-effort: ignore
			// the send error.
			_ = stream.Send(&agentgpuv1.WorkerMessage{
				Payload: &agentgpuv1.WorkerMessage_Deregister{
					Deregister: &agentgpuv1.Deregister{WorkerId: w.cfg.WorkerID},
				},
			})
			return runCtx.Err()
		case <-connCtx.Done():
			// Transient teardown (e.g. reconnect): no graceful announcement.
			return connCtx.Err()
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-ticker.C:
			if err := sendHeartbeat(); err != nil {
				return err
			}
		case res := <-results:
			if err := stream.Send(&agentgpuv1.WorkerMessage{
				Payload: &agentgpuv1.WorkerMessage_Result{Result: res.Proto()},
			}); err != nil {
				return err
			}
		}
	}
}

// heartbeat builds the worker's periodic liveness/capacity report. Capacity
// fields are stub/configured values until real GPU detection (#16) lands.
func (w *Worker) heartbeat(activeJobs uint32) *agentgpuv1.Heartbeat {
	return types.Heartbeat{
		WorkerID:        w.cfg.WorkerID,
		ActiveJobs:      activeJobs,
		TotalVRAM:       w.cfg.TotalVRAM,
		FreeVRAM:        w.cfg.FreeVRAM,
		Load:            w.cfg.Load,
		GPUType:         w.cfg.GPUType,
		AvailableModels: w.cfg.Models,
	}.Proto()
}
