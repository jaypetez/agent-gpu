package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	dto "github.com/prometheus/client_model/go"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/metrics"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/testutil"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// metricsHarness is the inference harness wired with a real *metrics.Metrics:
// the request path is metered and the live server collector is registered, so an
// end-to-end test can scrape /metrics through the registry and assert request
// counts, tokens, throttles, and the live fleet/queue gauges reflect real state.
type metricsHarness struct {
	url     string
	token   string
	authSvc *auth.Service
	metrics *metrics.Metrics
}

// newMetricsHarness mirrors newInferenceHarnessWith but threads a fresh metrics
// instrument into NewServer and registers the server collector over the
// control-plane server, returning the instrument for assertions.
func newMetricsHarness(t *testing.T, exec *scriptedExecutor, model string, opts ...server.Option) metricsHarness {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	base := []server.Option{
		server.WithLogger(discard),
		server.WithStore(st),
		server.WithAuthorizer(az),
		server.WithHeartbeatTimeout(2 * time.Second),
		server.WithEvictScanInterval(50 * time.Millisecond),
	}
	grpcSrv := server.New(append(base, opts...)...)
	grpcSrv.Start()
	t.Cleanup(func() { _ = grpcSrv.Close() })

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	grpcSrv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	m := metrics.New()
	if err := m.RegisterServerCollector(grpcSrv); err != nil {
		t.Fatalf("register server collector: %v", err)
	}

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, nil, m, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	t.Cleanup(ts.Close)

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	exec.models = []types.Model{{Name: model, Digest: "sha256:test"}}
	wctx, wcancel := context.WithCancel(context.Background())
	t.Cleanup(wcancel)
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          "worker-1",
		Models:            exec.models,
		Executor:          exec,
		Logger:            discard,
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

	h := metricsHarness{url: ts.URL, token: token, authSvc: authSvc, metrics: m}
	waitFor(t, 2*time.Second, "model in catalog", func() bool {
		return len(fetchModels(t, h.url, h.token)) == 1
	})
	return h
}

func (h metricsHarness) postAs(t *testing.T, token, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+path, readerOfStr(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestMetricsEndToEndRequestAndTokens proves driving a real chat completion
// through the full stack increments agentgpu_requests_total for the bounded
// chat endpoint label and meters the token split — the AC "tests assert key
// metrics increment" at the integration level.
func TestMetricsEndToEndRequestAndTokens(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"Hello", " world"}, promptTokens: 7, completionTokens: 3}
	h := newMetricsHarness(t, exec, "llama3")

	resp := h.postAs(t, h.token, "/v1/chat/completions",
		`{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if got := gatheredValue(t, h.metrics, "agentgpu_requests_total",
		map[string]string{"endpoint": "/v1/chat/completions", "method": "POST", "code": "200"}); got != 1 {
		t.Errorf("requests_total{chat,POST,200} = %v, want 1", got)
	}
	if got := gatheredValue(t, h.metrics, "agentgpu_tokens_generated_total",
		map[string]string{"model": "llama3", "kind": "prompt"}); got != 7 {
		t.Errorf("tokens{llama3,prompt} = %v, want 7", got)
	}
	if got := gatheredValue(t, h.metrics, "agentgpu_tokens_generated_total",
		map[string]string{"model": "llama3", "kind": "completion"}); got != 3 {
		t.Errorf("tokens{llama3,completion} = %v, want 3", got)
	}
}

// TestMetricsEndToEndThrottle proves a per-key 429 increments
// agentgpu_throttled_total{scope="key"} at the rejection site — the throttle
// metric is a true counter fed end-to-end.
func TestMetricsEndToEndThrottle(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithClock(clk.nowFn))
	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newMetricsHarness(t, exec, "llama3", server.WithQuota(eng))

	token := testutil.MintToken(t, h.authSvc,
		testutil.WithKeyName("user"),
		testutil.WithRoles(authz.RoleUser),
		testutil.WithAllowModels("llama3"),
		testutil.WithRPM(1),
	)
	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// First fits RPM=1; second trips the per-key limit (429).
	resp := h.postAs(t, token, "/v1/chat/completions", body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", resp.StatusCode)
	}
	resp = h.postAs(t, token, "/v1/chat/completions", body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request 2 status = %d, want 429", resp.StatusCode)
	}

	if got := gatheredValue(t, h.metrics, "agentgpu_throttled_total",
		map[string]string{"scope": "key"}); got != 1 {
		t.Errorf("throttled_total{key} = %v, want 1", got)
	}
	// The 429 is also counted as a request with code 429.
	if got := gatheredValue(t, h.metrics, "agentgpu_requests_total",
		map[string]string{"endpoint": "/v1/chat/completions", "method": "POST", "code": "429"}); got != 1 {
		t.Errorf("requests_total{chat,POST,429} = %v, want 1", got)
	}
}

// TestMetricsLiveCollectorReflectsFleet proves the live collector reports the
// connected worker: a worker_start_time_seconds series is present (uptime base)
// and the fleet online count is 1 — the collector reads real server state.
func TestMetricsLiveCollectorReflectsFleet(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"ok"}}
	h := newMetricsHarness(t, exec, "llama3")

	// The worker registered during harness setup, so its start time and the
	// online count are live in the next scrape.
	if got := gatheredValue(t, h.metrics, "agentgpu_fleet_workers",
		map[string]string{"status": "online"}); got != 1 {
		t.Errorf("fleet_workers{online} = %v, want 1", got)
	}
	start := gatheredValue(t, h.metrics, "agentgpu_worker_start_time_seconds",
		map[string]string{"worker": "worker-1"})
	if start <= 0 {
		t.Errorf("worker_start_time_seconds{worker-1} = %v, want a positive unix timestamp", start)
	}
	// Queue depth series are present for every priority even when empty.
	if got := gatheredValue(t, h.metrics, "agentgpu_queue_depth",
		map[string]string{"priority": "normal"}); got != 0 {
		t.Errorf("queue_depth{normal} = %v, want 0 (idle)", got)
	}
}

// ---- helpers ----

// gatheredValue gathers m's registry and returns the value of the sample named
// `name` matching `labels` exactly, failing if absent. Counters, gauges, and a
// histogram's sample count are all readable through it.
func gatheredValue(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if !labelPairsEqual(met.GetLabel(), labels) {
				continue
			}
			switch {
			case met.GetCounter() != nil:
				return met.GetCounter().GetValue()
			case met.GetGauge() != nil:
				return met.GetGauge().GetValue()
			case met.GetHistogram() != nil:
				return float64(met.GetHistogram().GetSampleCount())
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found in exposition", name, labels)
	return 0
}

func labelPairsEqual(have []*dto.LabelPair, want map[string]string) bool {
	if len(have) != len(want) {
		return false
	}
	for _, l := range have {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

// readerOfStr is a tiny io.Reader over a string for request bodies (kept local to
// avoid widening this file's imports).
func readerOfStr(s string) io.Reader { return &strReader{s: s} }

type strReader struct {
	s string
	i int
}

func (r *strReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
