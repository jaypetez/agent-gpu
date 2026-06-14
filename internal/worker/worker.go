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

// Execute implements Executor.
func (EchoExecutor) Execute(_ context.Context, job types.Job) types.JobResult {
	return types.JobResult{JobID: job.ID, Output: "echo: " + job.Prompt}
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
	// Models advertised at registration.
	Models []types.Model
	// HeartbeatInterval between heartbeats. Defaults to 15s.
	HeartbeatInterval time.Duration
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

	// onConnect, if set, is invoked after each successful registration ack.
	// Used by tests to observe (re)connection events.
	onConnect func()
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
// ack. Primarily for tests observing (re)connection events.
func (w *Worker) OnConnect(fn func()) { w.onConnect = fn }

// Run connects to the server and serves the control stream, reconnecting with
// exponential backoff until ctx is cancelled. It returns ctx.Err() on
// cancellation and only returns a non-context error if it cannot proceed.
func (w *Worker) Run(ctx context.Context) error {
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := w.runOnce(ctx)
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
// the stream drops or ctx is cancelled.
func (w *Worker) runOnce(ctx context.Context) error {
	conn, err := w.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := agentgpuv1.NewControlPlaneClient(conn)
	stream, err := client.Connect(ctx)
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
	if w.onConnect != nil {
		w.onConnect()
	}

	return w.serve(ctx, stream)
}

// serve runs the heartbeat loop and processes dispatched jobs until the stream
// drops. The receive loop runs in its own goroutine; this method owns all
// Sends (heartbeats and job results) to keep stream writes single-threaded.
func (w *Worker) serve(ctx context.Context, stream agentgpuv1.ControlPlane_ConnectClient) error {
	results := make(chan types.JobResult, 8)
	recvErr := make(chan error, 1)
	jobs := make(chan types.Job, 8)

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

	// Job worker: execute jobs (stub) and queue results for sending.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-jobs:
				res := w.cfg.Executor.Execute(ctx, job)
				select {
				case results <- res:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	ticker := time.NewTicker(w.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-ticker.C:
			if err := stream.Send(&agentgpuv1.WorkerMessage{
				Payload: &agentgpuv1.WorkerMessage_Heartbeat{
					Heartbeat: &agentgpuv1.Heartbeat{WorkerId: w.cfg.WorkerID},
				},
			}); err != nil {
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
