package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
)

// adminReq issues an admin request to the harness through the routed HTTP server
// with the given bearer token and optional JSON body, returning the response.
func (h inferenceHarness) adminReq(t *testing.T, method, token, path, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body != "" {
		r, err = http.NewRequest(method, h.url+path, strings.NewReader(body))
	} else {
		r, err = http.NewRequest(method, h.url+path, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestAdminPermissionChangeImmediate proves AC3 (immediate effect): an admin sets
// a deny-model on a key via PUT .../permissions and, with no restart, the very
// next inference for that model through the public chat endpoint is denied (403).
// The change is observed by the dispatch-time authorizer, which reads the key
// fresh from the store on every request.
func TestAdminPermissionChangeImmediate(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarness(t, exec, "llama3")
	admin := h.token

	// Create a user key allowed for llama3 via the admin API.
	resp := h.adminReq(t, http.MethodPost, admin, "/v1/admin/keys",
		`{"name":"svc","roles":["user"],"allow_models":["llama3"]}`)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeBody(t, resp, &created)
	if resp.StatusCode != http.StatusCreated || created.Token == "" {
		t.Fatalf("create key: status %d, body %+v", resp.StatusCode, created)
	}

	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// Baseline: the key can run inference now.
	resp = h.postAs(t, created.Token, "/v1/chat/completions", body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("baseline inference: status = %d, want 200", status)
	}

	// Admin denies llama3 for the key. Deny wins over allow in authz.
	resp = h.adminReq(t, http.MethodPut, admin, "/v1/admin/keys/"+created.ID+"/permissions",
		`{"roles":["user"],"allow_models":["llama3"],"deny_models":["llama3"]}`)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("set permissions: status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Immediately (no restart): the same inference is now denied 403.
	resp = h.postAs(t, created.Token, "/v1/chat/completions", body)
	status = resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusForbidden {
		t.Fatalf("post-deny inference: status = %d, want 403", status)
	}
}

// TestAdminRevokeImmediate proves AC3 (immediate effect): an admin revokes a key
// via DELETE and the very next authenticated call with that key fails 401, with
// no restart.
func TestAdminRevokeImmediate(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarness(t, exec, "llama3")
	admin := h.token

	resp := h.adminReq(t, http.MethodPost, admin, "/v1/admin/keys",
		`{"name":"svc","roles":["user"],"allow_models":["llama3"]}`)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeBody(t, resp, &created)

	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// Works before revoke.
	resp = h.postAs(t, created.Token, "/v1/chat/completions", body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("pre-revoke inference: status = %d, want 200", status)
	}

	// Revoke via the admin API.
	resp = h.adminReq(t, http.MethodDelete, admin, "/v1/admin/keys/"+created.ID, "")
	if resp.StatusCode != http.StatusNoContent {
		_ = resp.Body.Close()
		t.Fatalf("revoke: status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Immediately: the revoked key no longer authenticates (401).
	resp = h.postAs(t, created.Token, "/v1/chat/completions", body)
	status = resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusUnauthorized {
		t.Fatalf("post-revoke inference: status = %d, want 401", status)
	}
}

// TestAdminQuotaChangeImmediate proves AC3 (immediate effect) via the quota
// dimension: an admin sets RPM=1 on a key through PUT .../quota and, with no
// restart, the key's second chat request in the window is rejected 429. The
// quota engine reads the key's limits fresh from the store on each check.
func TestAdminQuotaChangeImmediate(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithClock(clk.nowFn))

	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarnessWith(t, exec, "llama3", server.WithQuota(eng))
	admin := h.token

	resp := h.adminReq(t, http.MethodPost, admin, "/v1/admin/keys",
		`{"name":"svc","roles":["user"],"allow_models":["llama3"]}`)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeBody(t, resp, &created)

	// Admin sets RPM=1 via the admin API.
	resp = h.adminReq(t, http.MethodPut, admin, "/v1/admin/keys/"+created.ID+"/quota", `{"rpm":1}`)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("set quota: status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// First request fits under RPM=1; the second trips it immediately (429).
	resp = h.postAs(t, created.Token, "/v1/chat/completions", body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", status)
	}
	resp = h.postAs(t, created.Token, "/v1/chat/completions", body)
	status = resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", status)
	}

	// The admin usage snapshot reflects the enforced limit and the request.
	resp = h.adminReq(t, http.MethodGet, admin, "/v1/admin/keys/"+created.ID+"/quota", "")
	var usage struct {
		KeyID  string `json:"key_id"`
		Limits struct {
			RPM uint64 `json:"rpm"`
		} `json:"limits"`
		RequestsThisMinute uint64 `json:"requests_this_minute"`
	}
	decodeBody(t, resp, &usage)
	if usage.KeyID != created.ID || usage.Limits.RPM != 1 || usage.RequestsThisMinute < 1 {
		t.Fatalf("usage snapshot wrong: %+v", usage)
	}
}

// TestAdminDrainWorkerEndToEnd proves AC2/AC3 for the worker resource through the
// real control plane: an admin drains the registered worker via the admin API,
// the fleet snapshot then reports it draining, and draining an unknown worker
// maps to 404.
func TestAdminDrainWorkerEndToEnd(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarness(t, exec, "llama3")
	admin := h.token

	// The worker is registered (the harness waited for its model to appear).
	resp := h.adminReq(t, http.MethodGet, admin, "/v1/admin/workers", "")
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	decodeBody(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].ID != "worker-1" {
		t.Fatalf("worker list wrong: %+v", list.Data)
	}

	// Drain it through the admin API.
	resp = h.adminReq(t, http.MethodPost, admin, "/v1/admin/workers/worker-1/drain", "")
	if resp.StatusCode != http.StatusNoContent {
		_ = resp.Body.Close()
		t.Fatalf("drain: status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The fleet snapshot now reports the worker draining.
	waitFor(t, 2*time.Second, "worker reported draining", func() bool {
		r := h.adminReq(t, http.MethodGet, admin, "/v1/admin/workers", "")
		defer func() { _ = r.Body.Close() }()
		var l struct {
			Data []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			return false
		}
		return len(l.Data) == 1 && l.Data[0].Status == "draining"
	})

	// Draining an unknown worker → 404.
	resp = h.adminReq(t, http.MethodPost, admin, "/v1/admin/workers/ghost/drain", "")
	if resp.StatusCode != http.StatusNotFound {
		_ = resp.Body.Close()
		t.Fatalf("drain unknown: status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestAdminNonAdminForbiddenEndToEnd proves AC1 through the live HTTP server: a
// non-admin key is rejected 403 on an admin endpoint, and an unauthenticated
// request is rejected 401, end-to-end (not just through the in-process handler).
func TestAdminNonAdminForbiddenEndToEnd(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarness(t, exec, "llama3")

	// Mint a non-admin key directly (the admin token created it would itself pass).
	userToken, _, err := h.authSvc.CreateWithPermissions(context.Background(), "user",
		auth.Permissions{Roles: []string{authz.RoleUser}})
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}

	resp := h.adminReq(t, http.MethodGet, userToken, "/v1/admin/keys", "")
	if resp.StatusCode != http.StatusForbidden {
		_ = resp.Body.Close()
		t.Fatalf("non-admin list keys: status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unauthenticated.
	r, err := http.NewRequest(http.MethodGet, h.url+"/v1/admin/keys", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		_ = resp.Body.Close()
		t.Fatalf("unauthenticated list keys: status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// decodeBody decodes a response body into v and closes it.
func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", buf.String(), err)
	}
}
