package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// fakeFleet is a deterministic stand-in for *server.Server's Fleet snapshot,
// letting the handler tests assert aggregation/dedup/filtering without standing
// up a gRPC control plane. snapshot is swapped between sub-tests to model worker
// drain/eviction.
type fakeFleet struct{ snapshot []types.Worker }

func (f *fakeFleet) Fleet() []types.Worker { return f.snapshot }

// testServer builds an httpapi.Server wired to the fake fleet and a real auth
// service + authorizer over an in-memory store, plus a discarding logger.
func testServer(t *testing.T, fleet *fakeFleet) (*Server, *auth.Service) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	s := &Server{
		fleet: fleet,
		auth:  authSvc,
		authz: az,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s, authSvc
}

// mustKey creates an API key with the given permissions and returns its token.
func mustKey(t *testing.T, authSvc *auth.Service, perms auth.Permissions) string {
	t.Helper()
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "test", perms)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return token
}

// do issues an authenticated GET to path through the routed handler and returns
// the recorder. An empty token sends no Authorization header.
func do(t *testing.T, s *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func onlineWorker(id string, models ...types.Model) types.Worker {
	return types.Worker{ID: id, Models: models, Status: types.WorkerOnline}
}

// adminPerms grants visibility of every model (admin bypasses allow/deny).
func adminPerms() auth.Permissions { return auth.Permissions{Roles: []string{authz.RoleAdmin}} }

func decodeModels(t *testing.T, rec *httptest.ResponseRecorder) modelList {
	t.Helper()
	var got modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /models body %q: %v", rec.Body.String(), err)
	}
	return got
}

// TestModelsReflectsFleet covers AC1: /models and /v1/models reflect the models
// currently available across the fleet.
func TestModelsReflectsFleet(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		onlineWorker("w1", types.Model{Name: "llama3", Digest: "sha256:aaa"}),
		onlineWorker("w2", types.Model{Name: "mistral", Digest: "sha256:bbb"}),
	}}
	s, authSvc := testServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())

	rec := do(t, s, "/models", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeModels(t, rec)
	if len(got.Models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got.Models), got.Models)
	}
	// Deterministic sort by name: llama3 before mistral.
	if got.Models[0].Name != "llama3" || got.Models[1].Name != "mistral" {
		t.Fatalf("models not sorted by name: %+v", got.Models)
	}
	if got.Models[0].Digest != "sha256:aaa" {
		t.Fatalf("digest mismatch: %+v", got.Models[0])
	}

	// /v1/models OpenAI shape.
	rec = do(t, s, "/v1/models", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d, want 200", rec.Code)
	}
	var oa openAIModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &oa); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	if oa.Object != "list" || len(oa.Data) != 2 {
		t.Fatalf("openai shape wrong: %+v", oa)
	}
	if oa.Data[0].ID != "llama3" || oa.Data[0].Object != "model" || oa.Data[0].OwnedBy != "agent-gpu" {
		t.Fatalf("openai model fields wrong: %+v", oa.Data[0])
	}
	if oa.Data[0].Created != openAICreated {
		t.Fatalf("created not stable: got %d want %d", oa.Data[0].Created, openAICreated)
	}
}

// TestModelDedupAcrossWorkers covers AC2: a model present on multiple workers
// appears once with correct availability.
func TestModelDedupAcrossWorkers(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		onlineWorker("w1", types.Model{Name: "llama3", Digest: "sha256:aaa"}),
		onlineWorker("w2", types.Model{Name: "llama3", Digest: "sha256:aaa"}),
		onlineWorker("w3", types.Model{Name: "mistral", Digest: "sha256:bbb"}),
	}}
	s, authSvc := testServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())

	got := decodeModels(t, do(t, s, "/models", token))
	if len(got.Models) != 2 {
		t.Fatalf("got %d models, want 2 (dedup): %+v", len(got.Models), got.Models)
	}
	llama := got.Models[0]
	if llama.Name != "llama3" {
		t.Fatalf("first model = %q, want llama3", llama.Name)
	}
	if llama.WorkerCount != 2 {
		t.Fatalf("worker_count = %d, want 2", llama.WorkerCount)
	}
	if len(llama.Workers) != 2 || llama.Workers[0] != "w1" || llama.Workers[1] != "w2" {
		t.Fatalf("workers wrong/unsorted: %+v", llama.Workers)
	}
}

// TestOnlyOnlineWorkers covers AC3: a drained/stale worker's models disappear
// from the catalog after the fleet snapshot changes (simulating eviction).
func TestOnlyOnlineWorkers(t *testing.T) {
	w1 := onlineWorker("w1", types.Model{Name: "llama3"})
	w2 := onlineWorker("w2", types.Model{Name: "mistral"})
	fleet := &fakeFleet{snapshot: []types.Worker{w1, w2}}
	s, authSvc := testServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())

	if n := len(decodeModels(t, do(t, s, "/models", token)).Models); n != 2 {
		t.Fatalf("initial models = %d, want 2", n)
	}

	// w2 drains: its model must vanish.
	w2.Status = types.WorkerDraining
	fleet.snapshot = []types.Worker{w1, w2}
	got := decodeModels(t, do(t, s, "/models", token))
	if len(got.Models) != 1 || got.Models[0].Name != "llama3" {
		t.Fatalf("after drain got %+v, want only llama3", got.Models)
	}

	// w1 stale, w2 fully evicted (removed from snapshot): catalog empties.
	w1.Status = types.WorkerStale
	fleet.snapshot = []types.Worker{w1}
	if n := len(decodeModels(t, do(t, s, "/models", token)).Models); n != 0 {
		t.Fatalf("after eviction models = %d, want 0", n)
	}
}

// TestPermissionFiltering covers AC4: a key sees only models it may run
// inference against; deny-list, no-granting-role, and deny-by-default all hide.
func TestPermissionFiltering(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		onlineWorker("w1",
			types.Model{Name: "llama3"},
			types.Model{Name: "mistral"},
			types.Model{Name: "secret"},
		),
	}}
	s, authSvc := testServer(t, fleet)

	// user role, allow-listed llama3 only -> sees llama3, not mistral/secret.
	allowed := mustKey(t, authSvc, auth.Permissions{
		Roles:       []string{authz.RoleUser},
		AllowModels: []string{"llama3"},
	})
	got := decodeModels(t, do(t, s, "/models", allowed))
	if len(got.Models) != 1 || got.Models[0].Name != "llama3" {
		t.Fatalf("allowed key saw %+v, want only llama3", got.Models)
	}

	// Deny-list wins even when allow-listed.
	denied := mustKey(t, authSvc, auth.Permissions{
		Roles:       []string{authz.RoleUser},
		AllowModels: []string{"llama3", "secret"},
		DenyModels:  []string{"secret"},
	})
	got = decodeModels(t, do(t, s, "/models", denied))
	for _, m := range got.Models {
		if m.Name == "secret" {
			t.Fatalf("denied key saw deny-listed model: %+v", got.Models)
		}
	}
	if len(got.Models) != 1 || got.Models[0].Name != "llama3" {
		t.Fatalf("denied key saw %+v, want only llama3", got.Models)
	}

	// No granting role -> deny by default, empty catalog.
	noRole := mustKey(t, authSvc, auth.Permissions{AllowModels: []string{"llama3"}})
	if n := len(decodeModels(t, do(t, s, "/models", noRole)).Models); n != 0 {
		t.Fatalf("no-role key saw %d models, want 0", n)
	}

	// Same filter on /v1/models.
	rec := do(t, s, "/v1/models", allowed)
	var oa openAIModelList
	_ = json.Unmarshal(rec.Body.Bytes(), &oa)
	if len(oa.Data) != 1 || oa.Data[0].ID != "llama3" {
		t.Fatalf("/v1/models filter wrong: %+v", oa.Data)
	}
}

// TestUnauthenticated covers AC5 (401 path): missing, malformed, and invalid
// bearer tokens are rejected with 401 and never leak the catalog.
func TestUnauthenticated(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{onlineWorker("w1", types.Model{Name: "llama3"})}}
	s, _ := testServer(t, fleet)

	cases := []struct {
		name   string
		header string // "" means no header set
	}{
		{"no header", ""},
		{"malformed scheme", "Token abc"},
		{"empty bearer", "Bearer "},
		{"unknown key", "Bearer agpu_unknownid_secretpart"},
	}
	for _, tc := range cases {
		for _, path := range []string{"/models", "/v1/models"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", rec.Code)
				}
				// The 401 body must not contain any model name (no catalog leak).
				if body := rec.Body.String(); contains(body, "llama3") {
					t.Fatalf("401 body leaked catalog: %s", body)
				}
			})
		}
	}
}

// contains is a tiny substring helper to keep the leak assertion dependency-free.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestMethodNotAllowed verifies non-GET requests are rejected once authenticated.
func TestMethodNotAllowed(t *testing.T) {
	fleet := &fakeFleet{snapshot: nil}
	s, authSvc := testServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())

	req := httptest.NewRequest(http.MethodPost, "/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
