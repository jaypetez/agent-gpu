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
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	defer ts.Close()

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Start a worker that advertises llama3 and heartbeats quickly.
	wctx, wcancel := context.WithCancel(context.Background())
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          "worker-1",
		Models:            []types.Model{{Name: "llama3", Digest: "sha256:abc"}},
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
