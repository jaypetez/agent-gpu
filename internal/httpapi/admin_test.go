package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// adminTestServer builds an httpapi.Server wired to the fake fleet, a real auth
// service + authorizer + (unlimited) quota engine over an in-memory store, and a
// discarding logger. It mirrors testServer but also sets the quota engine so the
// GET .../quota usage endpoint is exercised, and returns the fleet so a test can
// configure DrainWorker behaviour.
func adminTestServer(t *testing.T, fleet *fakeFleet) (*Server, *auth.Service) {
	s, authSvc, _ := adminTestServerWithAudit(t, fleet)
	return s, authSvc
}

// adminTestServerWithAudit is adminTestServer plus an attached audit store, so a
// test can assert on the recorded audit entries (#90).
func adminTestServerWithAudit(t *testing.T, fleet *fakeFleet) (*Server, *auth.Service, *audit.MemoryStore) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	auditLog := audit.NewMemoryStore(0)
	s := &Server{
		fleet:    fleet,
		auth:     authSvc,
		authz:    az,
		quota:    quota.NewEngine(quota.NewMemoryCounterStore()),
		log:      discard,
		auditLog: auditLog,
	}
	return s, authSvc, auditLog
}

// req issues an authenticated request with an optional JSON body through the
// routed handler and returns the recorder. An empty token sends no
// Authorization header.
func req(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return reqWithHeaders(t, s, method, path, token, body, nil)
}

// reqWithHeaders is req with extra request headers (e.g. Idempotency-Key) so the
// header-driven middleware can be exercised through the routed handler.
func reqWithHeaders(t *testing.T, s *Server, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// adminRoute names one admin endpoint for the table-driven authz tests.
type adminRoute struct {
	method string
	path   string
	body   string
}

// allAdminRoutes is every admin endpoint, used to prove the authz gate is
// applied uniformly (non-admin → 403, unauth → 401 on every one). Paths use a
// placeholder id; the gate runs before any lookup, so the id need not exist.
func allAdminRoutes() []adminRoute {
	return []adminRoute{
		{http.MethodPost, "/v1/admin/keys", `{"name":"x"}`},
		{http.MethodGet, "/v1/admin/keys", ""},
		{http.MethodGet, "/v1/admin/keys/abc", ""},
		{http.MethodDelete, "/v1/admin/keys/abc", ""},
		{http.MethodPost, "/v1/admin/keys/abc/rotate", ""},
		{http.MethodPut, "/v1/admin/keys/abc/permissions", `{"roles":["user"]}`},
		{http.MethodPut, "/v1/admin/keys/abc/quota", `{"rpm":1}`},
		{http.MethodGet, "/v1/admin/keys/abc/quota", ""},
		{http.MethodGet, "/v1/admin/roles", ""},
		{http.MethodGet, "/v1/admin/workers", ""},
		{http.MethodGet, "/v1/admin/workers/abc", ""},
		{http.MethodGet, "/v1/admin/gpus", ""},
		{http.MethodPost, "/v1/admin/workers/abc/drain", ""},
		{http.MethodPost, "/v1/admin/workers/abc/models", `{"model":"llama3"}`},
		{http.MethodDelete, "/v1/admin/workers/abc/models/llama3", ""},
		{http.MethodGet, "/v1/admin/stats", ""},
		{http.MethodGet, "/v1/admin/audit", ""},
	}
}

// TestAdminAuthzNonAdminForbidden proves AC1: a valid but non-admin key receives
// 403 on EVERY admin endpoint (and the error envelope carries code "forbidden").
func TestAdminAuthzNonAdminForbidden(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	// A key with the "user" role only — authenticated, but not admin.
	userToken := mustKey(t, authSvc, auth.Permissions{Roles: []string{authz.RoleUser}})

	for _, rt := range allAdminRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, userToken, rt.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if code := errorCode(t, rec); code != "forbidden" {
				t.Errorf("error code = %q, want forbidden", code)
			}
		})
	}
}

// TestAdminAuthzUnauthenticated proves AC1: a request with no/invalid bearer
// token receives 401 on EVERY admin endpoint.
func TestAdminAuthzUnauthenticated(t *testing.T) {
	s, _ := adminTestServer(t, &fakeFleet{})

	for _, rt := range allAdminRoutes() {
		t.Run("missing "+rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, "", rt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("no-token status = %d, want 401", rec.Code)
			}
		})
		t.Run("invalid "+rt.method+" "+rt.path, func(t *testing.T) {
			rec := req(t, s, rt.method, rt.path, "agpu_bogus_token", rt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("bad-token status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestAdminKeyLifecycle proves AC2: an admin key can create, list, inspect,
// rotate, set permissions/quota on, and revoke a key, with consistent shapes and
// the plaintext token returned ONLY on create and rotate.
func TestAdminKeyLifecycle(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	// Create.
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"svc","roles":["user"],"allow_models":["llama3"],"deny_models":["secret"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Token       string   `json:"token"`
		Roles       []string `json:"roles"`
		AllowModels []string `json:"allow_models"`
		DenyModels  []string `json:"deny_models"`
		Created     int64    `json:"created"`
	}
	decode(t, rec, &created)
	if created.ID == "" || created.Name != "svc" {
		t.Fatalf("create response wrong: %+v", created)
	}
	if !strings.HasPrefix(created.Token, "agpu_") {
		t.Fatalf("create did not return a plaintext token: %q", created.Token)
	}
	if len(created.Roles) != 1 || created.Roles[0] != "user" || created.AllowModels[0] != "llama3" {
		t.Errorf("create permissions wrong: %+v", created)
	}
	id := created.ID

	// List: the new key appears, never with a secret. The list response is the
	// shared cursor-paginated envelope ({"data":[...],"pagination":{...}}).
	rec = req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	assertNoSecret(t, rec.Body.String())
	var list struct {
		Data       []map[string]any `json:"data"`
		Pagination struct {
			NextCursor *string `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		} `json:"pagination"`
	}
	decode(t, rec, &list)
	if !containsKeyID(list.Data, id) {
		t.Fatalf("list does not contain created key %s: %+v", id, list.Data)
	}
	if list.Pagination.HasMore {
		t.Errorf("single-key list should not report has_more")
	}

	// Inspect one.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+id, adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	assertNoSecret(t, rec.Body.String())
	var got map[string]any
	decode(t, rec, &got)
	if got["id"] != id || got["revoked"] != false {
		t.Errorf("get response wrong: %+v", got)
	}

	// Set permissions (full replace): clear allow-list, set deny.
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/permissions", adminToken,
		`{"roles":["read-only"],"deny_models":["llama3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set permissions status = %d, want 200", rec.Code)
	}
	updated, err := authSvc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get updated key: %v", err)
	}
	if len(updated.Roles) != 1 || updated.Roles[0] != "read-only" ||
		len(updated.AllowModels) != 0 || len(updated.DenyModels) != 1 || updated.DenyModels[0] != "llama3" {
		t.Errorf("permissions not replaced: %+v", updated)
	}

	// Set quota: RPM=5, others default to 0 (unlimited).
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/quota", adminToken, `{"rpm":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set quota status = %d, want 200", rec.Code)
	}
	updated, _ = authSvc.Get(context.Background(), id)
	if updated.Limits == nil || updated.Limits.RPM != 5 {
		t.Fatalf("limits not set: %+v", updated.Limits)
	}

	// Clearing the override (empty body) drops Limits back to nil (global default).
	rec = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/quota", adminToken, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear quota status = %d, want 200", rec.Code)
	}
	updated, _ = authSvc.Get(context.Background(), id)
	if updated.Limits != nil {
		t.Errorf("limits not cleared: %+v", updated.Limits)
	}

	// Usage snapshot.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+id+"/quota", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota status = %d, want 200", rec.Code)
	}
	var usage struct {
		KeyID  string `json:"key_id"`
		Limits struct {
			RPM uint64 `json:"rpm"`
		} `json:"limits"`
	}
	decode(t, rec, &usage)
	if usage.KeyID != id {
		t.Errorf("usage key_id = %q, want %q", usage.KeyID, id)
	}

	// Rotate: returns a fresh one-time token, distinct from create's.
	rec = req(t, s, http.MethodPost, "/v1/admin/keys/"+id+"/rotate", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200", rec.Code)
	}
	var rotated struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decode(t, rec, &rotated)
	if rotated.ID != id || !strings.HasPrefix(rotated.Token, "agpu_") || rotated.Token == created.Token {
		t.Fatalf("rotate response wrong: %+v", rotated)
	}

	// Revoke: 204, and the key now reads back revoked.
	rec = req(t, s, http.MethodDelete, "/v1/admin/keys/"+id, adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", rec.Code)
	}
	revoked, _ := authSvc.Get(context.Background(), id)
	if !revoked.Revoked() {
		t.Errorf("key not revoked after DELETE")
	}
}

// TestAdminCreateKeyEnrichment proves #96 AC2/AC3: creating a key with
// owner/team/expires_at round-trips those fields into both the create response
// and the GET key view, and created_by is set to the creating admin key's id
// (the actor on the authenticated request context). SecretHash/Salt never leak.
func TestAdminCreateKeyEnrichment(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())
	// The actor id is the admin key's id (the token is agpu_<id>_<secret>).
	adminID := strings.Split(adminToken, "_")[1]

	expiry := time.Now().Add(24 * time.Hour).Unix()
	body := `{"name":"svc","owner":"alice@example.com","team":"platform","expires_at":` +
		strconv.FormatInt(expiry, 10) + `}`
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	assertNoSecret(t, rec.Body.String())

	var created struct {
		ID        string `json:"id"`
		Owner     string `json:"owner"`
		Team      string `json:"team"`
		CreatedBy string `json:"created_by"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	decode(t, rec, &created)
	if created.Owner != "alice@example.com" || created.Team != "platform" {
		t.Errorf("create response labels = owner=%q team=%q", created.Owner, created.Team)
	}
	if created.CreatedBy != adminID {
		t.Errorf("create response created_by = %q, want %q (the admin key id)", created.CreatedBy, adminID)
	}
	if created.ExpiresAt == nil || *created.ExpiresAt != expiry {
		t.Errorf("create response expires_at = %v, want %d", created.ExpiresAt, expiry)
	}

	// The GET view surfaces the same enrichment fields.
	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+created.ID, adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	assertNoSecret(t, rec.Body.String())
	var view struct {
		Owner     string `json:"owner"`
		Team      string `json:"team"`
		CreatedBy string `json:"created_by"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	decode(t, rec, &view)
	if view.Owner != "alice@example.com" || view.Team != "platform" || view.CreatedBy != adminID {
		t.Errorf("key view enrichment wrong: %+v", view)
	}
	if view.ExpiresAt == nil || *view.ExpiresAt != expiry {
		t.Errorf("key view expires_at = %v, want %d", view.ExpiresAt, expiry)
	}
}

// TestAdminCreateKeyBackwardCompat proves #96 AC3: a key created WITHOUT the new
// fields renders exactly as before — the enrichment keys are omitted entirely
// from both the create response and the GET view (so existing clients see no
// change).
func TestAdminCreateKeyBackwardCompat(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"plain"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created map[string]any
	decode(t, rec, &created)
	id, _ := created["id"].(string)
	for _, k := range []string{"owner", "team", "expires_at"} {
		if _, present := created[k]; present {
			t.Errorf("create response should omit %q when unset: %v", k, created)
		}
	}

	rec = req(t, s, http.MethodGet, "/v1/admin/keys/"+id, adminToken, "")
	var view map[string]any
	decode(t, rec, &view)
	// owner/team/expires_at are caller-supplied: omitted when not provided.
	for _, k := range []string{"owner", "team", "expires_at"} {
		if _, present := view[k]; present {
			t.Errorf("key view should omit %q when unset: %v", k, view)
		}
	}
	// created_by reflects the authenticated creating actor, so it IS present here
	// (the admin key id). A key minted outside the admin API has no actor and would
	// omit it — covered by the store/auth backward-compat tests.
	if _, present := view["created_by"]; !present {
		t.Errorf("key view should record created_by (the creating actor): %v", view)
	}
}

// TestAdminCreateKeyRejectsBadExpiry proves #96: a non-positive or already-past
// expires_at is rejected with 400 (invalid_request_error) and no key is created.
func TestAdminCreateKeyRejectsBadExpiry(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	cases := map[string]string{
		"past":     strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10),
		"zero":     "0",
		"negative": "-5",
	}
	for name, ts := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"name":"x","expires_at":` + ts + `}`
			rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s expiry", rec.Code, name)
			}
			if code := errorCode(t, rec); code != "invalid_request_error" {
				t.Errorf("error code = %q, want invalid_request_error", code)
			}
		})
	}

	// No key was created by any of the rejected requests.
	keys, err := authSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, k := range keys {
		if k.Name == "x" {
			t.Fatalf("a key was created despite a rejected expiry: %+v", k)
		}
	}
}

// TestAdminExpiredKeyFailsAuth proves #96 AC1 end-to-end through the router: a key
// minted with an ExpiresAt in the past (via the auth service's controllable
// clock) cannot authenticate against an admin endpoint — it gets 401, the same as
// any other auth failure (no enumeration).
func TestAdminExpiredKeyFailsAuth(t *testing.T) {
	st := store.NewMemory()
	// A clock pinned in the past at key creation; "now" (real wall clock at
	// request time) is far later, so the key is already expired when used.
	createTime := time.Unix(1_000, 0).UTC()
	authSvc := auth.NewService(st, auth.WithClock(func() time.Time { return createTime }))
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		fleet: &fakeFleet{},
		auth:  authSvc,
		authz: authz.NewAuthorizer(authz.WithLogger(discard)),
		quota: quota.NewEngine(quota.NewMemoryCounterStore()),
		log:   discard,
	}

	// Mint an admin key that expires one second after createTime — long before the
	// real clock the Authenticate path will compare against (createTime is fixed,
	// but the key's expiry is in the absolute past relative to time.Now()).
	expiry := time.Unix(1_001, 0).UTC()
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "expired-admin", auth.Permissions{
		Roles:     []string{authz.RoleAdmin},
		ExpiresAt: &expiry,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Advance the service clock past the expiry, then use the key: 401.
	createTime = time.Unix(2_000, 0).UTC()
	rec := req(t, s, http.MethodGet, "/v1/admin/keys", token, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key status = %d, want 401", rec.Code)
	}
}

// TestAuditCapturesEnrichment proves #96: the create audit entry includes the
// owner/team/created_by/expires_at metadata (none of it secret).
func TestAuditCapturesEnrichment(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())
	adminID := strings.Split(adminToken, "_")[1]

	expiry := time.Now().Add(48 * time.Hour).Unix()
	body := `{"name":"svc","owner":"carol","team":"research","expires_at":` +
		strconv.FormatInt(expiry, 10) + `}`
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	entries := auditLog.List(audit.Filter{Op: auditOpKeyCreate}, 0)
	if len(entries) != 1 {
		t.Fatalf("want 1 create entry, got %d", len(entries))
	}
	after := entries[0].After
	if after["owner"] != "carol" || after["team"] != "research" || after["created_by"] != adminID {
		t.Errorf("audit after missing enrichment: %+v", after)
	}
	// expires_at is recorded as the unix seconds int64.
	if got, ok := after["expires_at"].(int64); !ok || got != expiry {
		t.Errorf("audit after expires_at = %v (type %T), want %d", after["expires_at"], after["expires_at"], expiry)
	}
}

// TestAdminUnknownKey404 proves AC2 error consistency: every {id} operation on a
// key that does not exist returns 404 with code "not_found".
func TestAdminUnknownKey404(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	cases := []adminRoute{
		{http.MethodGet, "/v1/admin/keys/nope", ""},
		{http.MethodDelete, "/v1/admin/keys/nope", ""},
		{http.MethodPost, "/v1/admin/keys/nope/rotate", ""},
		{http.MethodPut, "/v1/admin/keys/nope/permissions", `{"roles":["user"]}`},
		{http.MethodPut, "/v1/admin/keys/nope/quota", `{"rpm":1}`},
		{http.MethodGet, "/v1/admin/keys/nope/quota", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := req(t, s, c.method, c.path, adminToken, c.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if code := errorCode(t, rec); code != "not_found" {
				t.Errorf("error code = %q, want not_found", code)
			}
		})
	}
}

// TestAdminMalformedBody400 proves AC2 error consistency: a malformed JSON body
// on a write endpoint yields 400 with code "invalid_request_error".
func TestAdminMalformedBody400(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	for _, path := range []string{
		"/v1/admin/keys",
		"/v1/admin/keys/abc/permissions",
		"/v1/admin/keys/abc/quota",
	} {
		method := http.MethodPost
		if strings.Contains(path, "/abc/") {
			method = http.MethodPut
		}
		t.Run(path, func(t *testing.T) {
			rec := req(t, s, method, path, adminToken, `{not json`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if code := errorCode(t, rec); code != "invalid_request_error" {
				t.Errorf("error code = %q, want invalid_request_error", code)
			}
		})
	}
}

// TestAdminWorkers proves AC2 for the worker resource: list returns the fleet
// snapshot projection, drain returns 204 and forwards the id, and a drain on an
// unknown worker maps ErrWorkerNotFound → 404.
func TestAdminWorkers(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		{ID: "w1", Models: []types.Model{{Name: "llama3"}}, Status: types.WorkerOnline, ActiveJobs: 2, Load: 50, GPUType: "a100"},
	}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	// List.
	rec := req(t, s, http.MethodGet, "/v1/admin/workers", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list workers status = %d, want 200", rec.Code)
	}
	var out struct {
		Data []struct {
			ID         string   `json:"id"`
			Models     []string `json:"models"`
			Status     string   `json:"status"`
			ActiveJobs uint32   `json:"active_jobs"`
			GPUType    string   `json:"gpu_type"`
		} `json:"data"`
	}
	decode(t, rec, &out)
	if len(out.Data) != 1 || out.Data[0].ID != "w1" || out.Data[0].Status != "online" ||
		out.Data[0].ActiveJobs != 2 || out.Data[0].GPUType != "a100" ||
		len(out.Data[0].Models) != 1 || out.Data[0].Models[0] != "llama3" {
		t.Fatalf("worker projection wrong: %+v", out.Data)
	}

	// Drain a known worker → 204, id forwarded to the control plane.
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("drain status = %d, want 204", rec.Code)
	}
	if fleet.drained != "w1" {
		t.Errorf("DrainWorker called with %q, want w1", fleet.drained)
	}

	// Drain an unknown worker → 404.
	fleet.drainErr = server.ErrWorkerNotFound
	rec = req(t, s, http.MethodPost, "/v1/admin/workers/ghost/drain", adminToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("drain unknown status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_found" {
		t.Errorf("error code = %q, want not_found", code)
	}
}

// TestAdminStats proves AC1/AC4 for the consolidated monitoring endpoint (#10):
// GET /v1/admin/stats returns the documented shape — queue depth (total +
// per-priority), per-worker load, and the time-in-queue distribution
// (count/sum/max/mean + cumulative buckets) — built live from QueueStats(),
// Fleet(), and WaitTimeStats(). Admin gating (non-admin 403, unauth 401) is
// proven for this route by the table-driven authz tests above.
func TestAdminStats(t *testing.T) {
	fleet := &fakeFleet{
		snapshot: []types.Worker{
			{ID: "w1", Status: types.WorkerOnline, ActiveJobs: 2, Load: 50},
			{ID: "w2", Status: types.WorkerDraining, ActiveJobs: 0, Load: 10},
		},
		queueStats: queue.Stats{
			Total:      3,
			ByPriority: map[queue.Priority]int{queue.PriorityHigh: 1, queue.PriorityNormal: 2},
		},
		waitStats: server.WaitTimeStats{
			Count: 2,
			SumMs: 150,
			MaxMs: 120,
			Buckets: []server.WaitBucket{
				{LeMs: 10, Count: 0},
				{LeMs: 100, Count: 1},
				{LeMs: 1000, Count: 2},
				{LeMs: 10000, Count: 2},
				{LeMs: 0, Count: 2}, // +Inf
			},
		},
	}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/stats", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}

	var out struct {
		Queue struct {
			Total      int            `json:"total"`
			ByPriority map[string]int `json:"by_priority"`
		} `json:"queue"`
		Workers []struct {
			ID         string `json:"id"`
			ActiveJobs uint32 `json:"active_jobs"`
			Load       uint32 `json:"load"`
			Status     string `json:"status"`
		} `json:"workers"`
		WaitTime struct {
			Count   uint64 `json:"count"`
			SumMs   uint64 `json:"sum_ms"`
			MaxMs   uint64 `json:"max_ms"`
			MeanMs  uint64 `json:"mean_ms"`
			Buckets []struct {
				LeMs  uint64 `json:"le_ms"`
				Count uint64 `json:"count"`
			} `json:"buckets"`
		} `json:"wait_time"`
	}
	decode(t, rec, &out)

	// Queue depth: total plus the per-priority breakdown keyed by name.
	if out.Queue.Total != 3 {
		t.Errorf("queue total = %d, want 3", out.Queue.Total)
	}
	if out.Queue.ByPriority["high"] != 1 || out.Queue.ByPriority["normal"] != 2 {
		t.Errorf("by_priority = %v, want high:1 normal:2", out.Queue.ByPriority)
	}

	// Per-worker load projection (id/active_jobs/load/status), order-preserving.
	if len(out.Workers) != 2 {
		t.Fatalf("workers len = %d, want 2", len(out.Workers))
	}
	if out.Workers[0].ID != "w1" || out.Workers[0].ActiveJobs != 2 ||
		out.Workers[0].Load != 50 || out.Workers[0].Status != "online" {
		t.Errorf("worker[0] = %+v, want w1 active=2 load=50 online", out.Workers[0])
	}
	if out.Workers[1].ID != "w2" || out.Workers[1].Status != "draining" {
		t.Errorf("worker[1] = %+v, want w2 draining", out.Workers[1])
	}

	// Wait-time: summary + computed mean + cumulative buckets (incl. +Inf).
	if out.WaitTime.Count != 2 || out.WaitTime.SumMs != 150 || out.WaitTime.MaxMs != 120 {
		t.Errorf("wait_time summary = %+v, want count=2 sum=150 max=120", out.WaitTime)
	}
	if out.WaitTime.MeanMs != 75 { // 150 / 2
		t.Errorf("wait_time mean = %d, want 75", out.WaitTime.MeanMs)
	}
	if len(out.WaitTime.Buckets) != 5 {
		t.Fatalf("buckets len = %d, want 5 (4 bounds + Inf)", len(out.WaitTime.Buckets))
	}
	last := out.WaitTime.Buckets[len(out.WaitTime.Buckets)-1]
	if last.LeMs != 0 || last.Count != 2 {
		t.Errorf("+Inf bucket = %+v, want le_ms=0 count=2", last)
	}
}

// TestAdminStatsEmpty proves the endpoint is well-formed against a fresh server:
// an empty queue, no workers, and no recorded waits yield zeroed sections with a
// zero mean (no divide-by-zero) and a non-null buckets array.
func TestAdminStatsEmpty(t *testing.T) {
	// A real server.New() exercises the genuine WaitTimeStats()/QueueStats()
	// zero state (the +Inf bucket present, mean guarded at Count==0).
	grpcSrv := server.New()
	defer func() { _ = grpcSrv.Close() }()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		fleet: grpcSrv,
		auth:  authSvc,
		authz: authz.NewAuthorizer(authz.WithLogger(discard)),
		quota: quota.NewEngine(quota.NewMemoryCounterStore()),
		log:   discard,
	}
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/stats", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}
	var out struct {
		Queue struct {
			Total int `json:"total"`
		} `json:"queue"`
		Workers  []any `json:"workers"`
		WaitTime struct {
			Count   uint64 `json:"count"`
			MeanMs  uint64 `json:"mean_ms"`
			Buckets []struct {
				LeMs uint64 `json:"le_ms"`
			} `json:"buckets"`
		} `json:"wait_time"`
	}
	decode(t, rec, &out)
	if out.Queue.Total != 0 || out.WaitTime.Count != 0 || out.WaitTime.MeanMs != 0 {
		t.Errorf("empty stats not zeroed: %+v", out)
	}
	if len(out.WaitTime.Buckets) != 5 {
		t.Errorf("buckets len = %d, want 5 even when empty", len(out.WaitTime.Buckets))
	}
}

// TestAdminCreateKeyNeverLeaksSecret proves no response (create, list, get)
// includes the stored secret hash or salt fields.
func TestAdminCreateKeyNeverLeaksSecret(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"svc"}`)
	assertNoSecret(t, rec.Body.String())

	rec = req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	assertNoSecret(t, rec.Body.String())
}

// ---- helpers ----

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, rec, &body)
	return body.Error.Code
}

// assertNoSecret fails if the response body carries any field that would expose
// the stored secret material. SecretHash/Salt are []byte and would marshal as
// base64 under those JSON keys if the record were serialized directly.
func assertNoSecret(t *testing.T, body string) {
	t.Helper()
	for _, banned := range []string{"SecretHash", "secret_hash", "secrethash", "Salt", "\"salt\""} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(banned)) {
			t.Fatalf("response leaks secret material (%q): %s", banned, body)
		}
	}
}

func containsKeyID(keys []map[string]any, id string) bool {
	for _, k := range keys {
		if k["id"] == id {
			return true
		}
	}
	return false
}
