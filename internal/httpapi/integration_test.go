package httpapi_test

import (
	"context"
	"encoding/json"
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

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// TestEndToEndCatalogReflectsLiveFleet wires the real control-plane gRPC server,
// a worker over bufconn, and the HTTP API together. It proves the catalog
// reflects a live fleet (AC1) and that evicting the worker empties the catalog
// (AC3) — end-to-end, not through a fake fleet.
func TestEndToEndCatalogReflectsLiveFleet(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A short heartbeat timeout + fast scan so a worker going silent is evicted
	// quickly; the worker heartbeats faster still so it stays Online until told
	// to stop.
	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	grpcSrv := server.New(
		server.WithLogger(discard),
		server.WithStore(st),
		server.WithAuthorizer(az),
		server.WithHeartbeatTimeout(60*time.Millisecond),
		server.WithEvictScanInterval(10*time.Millisecond),
	)
	grpcSrv.Start()
	defer func() { _ = grpcSrv.Close() }()

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	grpcSrv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, nil, nil, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	defer ts.Close()

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Start a worker that advertises llama3 and heartbeats quickly.
	wctx, wcancel := context.WithCancel(context.Background())
	w := newCatalogWorker(lis, "worker-1", []types.Model{{Name: "llama3", Digest: "sha256:abc"}})
	go func() { _ = w.Run(wctx) }()

	// AC1: catalog reflects the live fleet once the worker registers.
	waitFor(t, 2*time.Second, "model to appear in catalog", func() bool {
		return len(fetchModels(t, ts.URL, token)) == 1
	})
	models := fetchModels(t, ts.URL, token)
	if models[0].Name != "llama3" || models[0].WorkerCount != 1 {
		t.Fatalf("catalog entry wrong: %+v", models[0])
	}

	// AC3: stop the worker; once it is evicted the catalog empties.
	wcancel()
	waitFor(t, 2*time.Second, "model to disappear after eviction", func() bool {
		return len(fetchModels(t, ts.URL, token)) == 0
	})
}

// newCatalogWorker builds a worker that advertises the given models and
// heartbeats quickly against the supplied bufconn listener. It is the shared
// builder for the catalog/aggregation end-to-end tests.
func newCatalogWorker(lis *bufconn.Listener, id string, models []types.Model) *worker.Worker {
	return worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          id,
		Models:            models,
		HeartbeatInterval: 15 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})
}

// TestModelAggregationMultiWorker proves the catalog deduplicates a model
// advertised by several workers into one entry whose worker_count and worker
// ids reflect the whole fleet, while a model unique to one worker stays a
// single-worker entry — end-to-end through GET /v1/models and GET /models.
// (AC2 — the multi-worker model-aggregation flow.)
func TestModelAggregationMultiWorker(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	grpcSrv := server.New(
		server.WithLogger(discard),
		server.WithStore(st),
		server.WithAuthorizer(az),
		server.WithHeartbeatTimeout(2*time.Second),
		server.WithEvictScanInterval(50*time.Millisecond),
	)
	grpcSrv.Start()
	defer func() { _ = grpcSrv.Close() }()

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	grpcSrv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, nil, nil, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	defer ts.Close()

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Two workers share "llama3"; a third advertises only "mistral". The shared
	// model must collapse to one catalog entry across both serving workers.
	workers := []struct {
		id     string
		models []types.Model
	}{
		{"worker-a", []types.Model{{Name: "llama3", Digest: "sha256:abc"}}},
		{"worker-b", []types.Model{{Name: "llama3", Digest: "sha256:abc"}}},
		{"worker-c", []types.Model{{Name: "mistral", Digest: "sha256:def"}}},
	}
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	for _, spec := range workers {
		w := newCatalogWorker(lis, spec.id, spec.models)
		go func() { _ = w.Run(wctx) }()
	}

	// Wait until the whole fleet has converged: the shared model (llama3) must be
	// served by BOTH expected workers and the distinct model (mistral) by its one
	// worker. Polling only on len(models) == 2 would proceed as soon as a single
	// llama3 worker plus mistral are Online — before worker-b has necessarily
	// registered — leaving the worker_count assertion to win a registration race.
	// Waiting on the full expected state removes that timing dependence entirely.
	waitFor(t, 2*time.Second, "fleet to converge: llama3 on worker-a+worker-b, mistral on worker-c", func() bool {
		models := fetchModels(t, ts.URL, token)
		byName := make(map[string]modelEntry, len(models))
		for _, m := range models {
			byName[m.Name] = m
		}
		llama, ok := byName["llama3"]
		if !ok || llama.WorkerCount != 2 || !equalStrings(llama.Workers, []string{"worker-a", "worker-b"}) {
			return false
		}
		mistral, ok := byName["mistral"]
		if !ok || mistral.WorkerCount != 1 || !equalStrings(mistral.Workers, []string{"worker-c"}) {
			return false
		}
		return true
	})

	models := fetchModels(t, ts.URL, token)
	byName := make(map[string]modelEntry, len(models))
	for _, m := range models {
		if _, dup := byName[m.Name]; dup {
			t.Fatalf("model %q appears more than once in catalog: %+v", m.Name, models)
		}
		byName[m.Name] = m
	}

	// The shared model collapses to one entry serving both workers.
	llama, ok := byName["llama3"]
	if !ok {
		t.Fatalf("llama3 missing from catalog: %+v", models)
	}
	if llama.WorkerCount != 2 {
		t.Errorf("llama3 worker_count = %d, want 2", llama.WorkerCount)
	}
	if want := []string{"worker-a", "worker-b"}; !equalStrings(llama.Workers, want) {
		t.Errorf("llama3 workers = %v, want %v", llama.Workers, want)
	}

	// The distinct model is a single-worker entry.
	mistral, ok := byName["mistral"]
	if !ok {
		t.Fatalf("mistral missing from catalog: %+v", models)
	}
	if mistral.WorkerCount != 1 {
		t.Errorf("mistral worker_count = %d, want 1", mistral.WorkerCount)
	}
	if want := []string{"worker-c"}; !equalStrings(mistral.Workers, want) {
		t.Errorf("mistral workers = %v, want %v", mistral.Workers, want)
	}

	// The OpenAI /v1/models surface dedupes to the same two model ids.
	ids := fetchOpenAIModelIDs(t, ts.URL, token)
	if want := []string{"llama3", "mistral"}; !equalStrings(ids, want) {
		t.Errorf("/v1/models ids = %v, want %v", ids, want)
	}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchOpenAIModelIDs queries GET /v1/models and returns the model ids in
// response order (the catalog is sorted by name, so they are deterministic).
func fetchOpenAIModelIDs(t *testing.T, baseURL, token string) []string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// modelEntry mirrors the /models response shape for decoding in the external
// test package.
type modelEntry struct {
	Name        string   `json:"name"`
	Digest      string   `json:"digest"`
	WorkerCount int      `json:"worker_count"`
	Workers     []string `json:"workers"`
}

func fetchModels(t *testing.T, baseURL, token string) []modelEntry {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Models []modelEntry `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Models
}

func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}
