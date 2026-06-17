package loadtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// InProcConfig configures the in-process stack the inproc load mode runs
// against. It mirrors the production wiring (server + workers over bufconn + HTTP
// API) but with the dependency-free echo backend, so a baseline is reproducible
// on any machine with no Ollama or GPU. The knobs make saturation observable
// model-free: Workers exercises multi-worker routing, GlobalRPM/TPM elicits
// throttling (429), and Think makes client-observed latency climb under load.
type InProcConfig struct {
	// Model is the model name the echo workers advertise (and that load requests
	// target). Defaults to "echo-model".
	Model string
	// Workers is the number of echo workers to spin up over bufconn. Use >= 2 to
	// exercise multi-worker routing. Defaults to 2.
	Workers int
	// QueueMaxDepth bounds the server's job queue; a request that finds NO runnable
	// worker for its model and a full queue is rejected with 503
	// (queue.ErrQueueFull). Note the server only enqueues when no worker is
	// runnable (Online + can serve the model) — NOT merely when workers are busy —
	// so with always-on echo workers the queue stays empty and this knob does not
	// fire. It bounds the queue for the no-runnable-worker case (the realistic
	// remote-deployment saturation), and is recorded in the report for provenance.
	// Zero leaves the queue unbounded.
	QueueMaxDepth int
	// GlobalRPM / GlobalTPM set the server-wide rate limit; exceeding it returns
	// 429. Zero (the default) disables the global limiter.
	GlobalRPM uint64
	GlobalTPM uint64
	// Think is an artificial per-request processing delay applied by the in-process
	// echo backend. The default echo executor returns instantly, so under load
	// nothing backs up and latency stays sub-millisecond. A small Think (e.g. 5ms)
	// makes each job occupy its worker for that long, so under enough concurrency
	// requests wait for a free worker and the CLIENT-OBSERVED latency climbs
	// steeply — the saturation signal the harness exposes model-free. (It surfaces
	// as latency growth, not 503s: the server queues only when no worker is
	// runnable, and busy-but-Online echo workers stay runnable, so jobs back up in
	// the worker's own intake rather than the server queue.) Zero (the default)
	// keeps the instant echo for a pure throughput baseline.
	Think time.Duration
	// Logger receives the stack's logs; defaults to a discard logger so a load run
	// is not drowned in server logs. Pass a real logger to debug.
	Logger *slog.Logger
}

// InProcStack is a fully-wired in-process agent-gpu deployment for load testing:
// a control-plane gRPC server, Workers echo workers connected over bufconn, and
// the public HTTP API served on a local httptest.Server. It exposes the base URL
// and a user + admin token so the standard Driver runs against it exactly as it
// would a remote deployment, and a StatsSource over the live server so the
// saturation poller works without HTTP. Close tears the whole stack down.
type InProcStack struct {
	// BaseURL is the HTTP API base (httptest server URL) the Driver targets.
	BaseURL string
	// UserToken is a user-role key permitted for the model, for the load requests.
	UserToken string
	// AdminToken is an admin-role key, for the saturation poller / admin stats.
	AdminToken string

	srv   *server.Server
	stats StatsSource
	close func()
}

// DefaultInProcModel is the default model name the in-process echo workers
// advertise (and that load requests target when --model is not set). It is
// exported so the cmd layer can default the model to it.
const DefaultInProcModel = "echo-model"

// StartInProc builds and starts the in-process stack per cfg. It returns once at
// least one worker has registered and the model is visible in the catalog (so
// the first load request does not queue waiting for a worker), or an error if
// the stack cannot be brought up within a short readiness window. The caller MUST
// call Close on the returned stack to release the gRPC server, workers, and HTTP
// listener.
//
// The wiring mirrors cmd/agentgpu's production serveControlPlane: a server.New
// with the same authorizer shared into the HTTP API, an auth.Service over an
// in-memory store, and workers dialed over bufconn. The differences are all
// load-test affordances: the backend is worker.EchoExecutor (no Ollama), the
// queue/quota are configured from the saturation knobs, and heartbeats are fast
// so readiness is near-instant.
func StartInProc(ctx context.Context, cfg InProcConfig) (*InProcStack, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultInProcModel
	}
	if cfg.Workers < 1 {
		cfg.Workers = 2
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(logger))

	// Quota engine carrying any global limits (the throttling knob). With both
	// zero it is an unlimited no-op, exactly like a server started without limits.
	eng := quota.NewEngine(quota.NewMemoryCounterStore(),
		quota.WithLogger(logger),
		quota.WithGlobalLimits(cfg.GlobalRPM, cfg.GlobalTPM),
	)

	opts := []server.Option{
		server.WithLogger(logger),
		server.WithStore(st),
		server.WithAuthorizer(az),
		server.WithQuota(eng),
		// Short heartbeat timeout + scan so a worker is considered live quickly and
		// a load run does not wait on production-scale timers.
		server.WithHeartbeatTimeout(2 * time.Second),
		server.WithEvictScanInterval(50 * time.Millisecond),
	}
	if cfg.QueueMaxDepth > 0 {
		// Bounding the queue makes a full queue reject with 503 (the saturation knob
		// for queueing). Unbounded otherwise.
		opts = append(opts, server.WithQueue(queue.New(queue.WithMaxDepth(cfg.QueueMaxDepth))))
	}

	srv := server.New(opts...)
	srv.Start()

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(srv, authSvc, az, nil, logger, "")
	ts := httptest.NewServer(httpSrv.Handler())

	// Mint a user key (permitted for the model) for the load, and an admin key for
	// the stats poller. CreateWithPermissions is the same path the admin API uses.
	userToken, _, err := authSvc.CreateWithPermissions(ctx, "loadtest-user",
		auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{cfg.Model}})
	if err != nil {
		ts.Close()
		gs.Stop()
		_ = srv.Close()
		return nil, fmt.Errorf("mint user key: %w", err)
	}
	adminToken, _, err := authSvc.CreateWithPermissions(ctx, "loadtest-admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		ts.Close()
		gs.Stop()
		_ = srv.Close()
		return nil, fmt.Errorf("mint admin key: %w", err)
	}

	// Spin up the echo workers over bufconn. Each advertises the model so the
	// scheduler can route to any of them (multi-worker routing).
	models := []types.Model{{Name: cfg.Model, Digest: "sha256:echo"}}
	wctx, wcancel := context.WithCancel(context.Background())
	for i := 0; i < cfg.Workers; i++ {
		w := worker.New(worker.Config{
			ServerAddr:        "bufconn",
			WorkerID:          fmt.Sprintf("loadtest-worker-%d", i),
			Models:            models,
			Executor:          newSlowEchoExecutor(models, cfg.Think),
			Logger:            logger,
			HeartbeatInterval: 15 * time.Millisecond,
			Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
			DialOptions: []grpc.DialOption{
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
					return lis.DialContext(ctx)
				}),
			},
		})
		go func() { _ = w.Run(wctx) }()
	}

	stack := &InProcStack{
		BaseURL:    ts.URL,
		UserToken:  userToken,
		AdminToken: adminToken,
		srv:        srv,
		stats:      &inProcStatsSource{srv: srv},
		close: func() {
			wcancel()
			ts.Close()
			gs.Stop()
			_ = srv.Close()
		},
	}

	// Wait until at least one worker has registered and is advertising the model,
	// so the first load request finds a runnable worker rather than queuing. This
	// mirrors the compose-e2e model-wait. The readiness signal is the live fleet
	// reporting an Online worker for the model.
	if err := waitForWorkers(ctx, srv, cfg.Workers, 5*time.Second); err != nil {
		stack.Close()
		return nil, err
	}
	return stack, nil
}

// StatsSource returns a StatsSource that reads the live server's queue and
// time-in-queue accessors directly (no HTTP), for the in-process saturation
// poller.
func (s *InProcStack) StatsSource() StatsSource { return s.stats }

// Close tears down the workers, HTTP server, and gRPC/control-plane server. It is
// idempotent-safe to call once.
func (s *InProcStack) Close() {
	if s.close != nil {
		s.close()
		s.close = nil
	}
}

// waitForWorkers blocks until the server's fleet reports at least `want` Online
// workers (capped at want so a partial fleet still proceeds once enough are up),
// or the timeout elapses. It polls on a short interval — the workers register
// asynchronously over bufconn, so there is no synchronous "ready" signal to wait
// on. Returns an error if no worker becomes Online within the window.
func waitForWorkers(ctx context.Context, srv *server.Server, want int, timeout time.Duration) error {
	if want < 1 {
		want = 1
	}
	deadline := time.Now().Add(timeout)
	for {
		online := 0
		for _, w := range srv.Fleet() {
			if w.Status == types.WorkerOnline {
				online++
			}
		}
		if online >= want {
			return nil
		}
		if time.Now().After(deadline) {
			if online > 0 {
				// At least one worker is up: good enough to run (the rest will join).
				return nil
			}
			return fmt.Errorf("no workers became ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// inProcStatsSource reads saturation signals straight off the live server's
// accessors (QueueStats + WaitTimeStats), so the in-process mode observes
// queueing without going through the admin HTTP endpoint. It mirrors what
// HTTPStatsSource decodes from GET /v1/admin/stats.
type inProcStatsSource struct{ srv *server.Server }

func (s *inProcStatsSource) Snapshot(context.Context) (StatsSnapshot, error) {
	qs := s.srv.QueueStats()
	wt := s.srv.WaitTimeStats()
	var meanMs uint64
	if wt.Count > 0 {
		meanMs = wt.SumMs / wt.Count
	}
	return StatsSnapshot{
		QueueTotal: qs.Total,
		WaitCount:  wt.Count,
		WaitMaxMs:  wt.MaxMs,
		WaitMeanMs: meanMs,
	}, nil
}
