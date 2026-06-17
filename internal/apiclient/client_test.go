package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient returns a Client pointed at srv with a fixed token. The httptest
// server's own *http.Client is reused so requests stay in-process.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(srv.URL, "agpu_test_secret", WithHTTPClient(srv.Client()))
}

// capture records the method, path, Authorization header, and decoded body of the
// last request a handler saw, so a test can assert the client sent what it should.
type capture struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

// recordingHandler returns an http.HandlerFunc that records the request into cap
// and replies with status and respBody (respBody may be empty for 204).
func recordingHandler(cap *capture, status int, respBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.auth = r.Header.Get("Authorization")
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			cap.body = map[string]any{}
			_ = json.Unmarshal(b, &cap.body)
		}
		if respBody == "" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}
}

func TestCreateKey(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusCreated,
		`{"id":"abc","name":"app","token":"agpu_abc_secret","roles":["user"],"allow_models":["llama3"],"deny_models":[],"created":100}`))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.CreateKey(context.Background(), CreateKeyRequest{
		Name:        "app",
		Roles:       []string{"user"},
		AllowModels: []string{"llama3"},
	})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if resp.Token != "agpu_abc_secret" || resp.ID != "abc" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/admin/keys" {
		t.Fatalf("sent %s %s, want POST /v1/admin/keys", cap.method, cap.path)
	}
	if cap.auth != "Bearer agpu_test_secret" {
		t.Fatalf("auth header = %q", cap.auth)
	}
	if cap.body["name"] != "app" {
		t.Fatalf("request body name = %v", cap.body["name"])
	}
}

func TestListKeys(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[{"id":"a","name":"one","roles":["admin"],"revoked":false,"usage_count":3,"created":10},{"id":"b","name":"two","roles":[],"revoked":true,"usage_count":0,"created":20}]}`)
	}))
	defer srv.Close()

	keys, err := newTestClient(t, srv).ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != "a" || !keys[1].Revoked {
		t.Fatalf("unexpected keys: %+v", keys)
	}
}

func TestRevokeKey(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusNoContent, ""))
	defer srv.Close()

	if err := newTestClient(t, srv).RevokeKey(context.Background(), "xyz"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != "/v1/admin/keys/xyz" {
		t.Fatalf("sent %s %s, want DELETE /v1/admin/keys/xyz", cap.method, cap.path)
	}
}

func TestRotateKey(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, `{"id":"xyz","token":"agpu_xyz_new"}`))
	defer srv.Close()

	resp, err := newTestClient(t, srv).RotateKey(context.Background(), "xyz")
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if resp.Token != "agpu_xyz_new" {
		t.Fatalf("token = %q", resp.Token)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/admin/keys/xyz/rotate" {
		t.Fatalf("sent %s %s, want POST .../rotate", cap.method, cap.path)
	}
}

func TestSetPermissions(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK,
		`{"id":"xyz","name":"app","roles":["user"],"allow_models":["llama3"],"deny_models":[],"revoked":false,"usage_count":0,"created":1}`))
	defer srv.Close()

	key, err := newTestClient(t, srv).SetPermissions(context.Background(), "xyz", PermissionsRequest{
		Roles:       []string{"user"},
		AllowModels: []string{"llama3"},
	})
	if err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	if key.Roles[0] != "user" {
		t.Fatalf("roles = %v", key.Roles)
	}
	if cap.method != http.MethodPut || cap.path != "/v1/admin/keys/xyz/permissions" {
		t.Fatalf("sent %s %s, want PUT .../permissions", cap.method, cap.path)
	}
}

func TestSetQuotaSendsPointers(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK,
		`{"id":"xyz","name":"app","roles":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":0,"created":1,"limits":{"rpm":60,"tpm":0,"daily_tokens":0,"monthly_tokens":0}}`))
	defer srv.Close()

	rpm := uint64(60)
	key, err := newTestClient(t, srv).SetQuota(context.Background(), "xyz", QuotaRequest{RPM: &rpm})
	if err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	if key.Limits == nil || key.Limits.RPM != 60 {
		t.Fatalf("limits = %+v", key.Limits)
	}
	if cap.method != http.MethodPut || cap.path != "/v1/admin/keys/xyz/quota" {
		t.Fatalf("sent %s %s, want PUT .../quota", cap.method, cap.path)
	}
	// Only rpm was set; the body must carry rpm and omit the others (pointer + omitempty).
	if _, ok := cap.body["rpm"]; !ok {
		t.Fatalf("body missing rpm: %v", cap.body)
	}
	if _, ok := cap.body["tpm"]; ok {
		t.Fatalf("body should omit tpm: %v", cap.body)
	}
}

func TestSetQuotaClearSendsEmptyBody(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK,
		`{"id":"xyz","name":"app","roles":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":0,"created":1}`))
	defer srv.Close()

	// An all-nil request is the "clear the override" signal; with omitempty the
	// body is the empty JSON object so the server clears the per-key limits.
	if _, err := newTestClient(t, srv).SetQuota(context.Background(), "xyz", QuotaRequest{}); err != nil {
		t.Fatalf("SetQuota clear: %v", err)
	}
	if len(cap.body) != 0 {
		t.Fatalf("clear body should be empty, got %v", cap.body)
	}
}

func TestGetQuota(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"key_id":"xyz","limits":{"rpm":60,"tpm":1000,"daily_tokens":0,"monthly_tokens":0},"requests_this_minute":5,"tokens_this_minute":100,"tokens_today":500,"tokens_this_month":900,"minute_resets_at":111,"day_resets_at":222,"month_resets_at":333}`)
	}))
	defer srv.Close()

	u, err := newTestClient(t, srv).GetQuota(context.Background(), "xyz")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if u.Limits.RPM != 60 || u.RequestsThisMinute != 5 || u.MinuteResetsAt != 111 {
		t.Fatalf("unexpected usage: %+v", u)
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK,
		`{"models":[{"name":"llama3","digest":"sha256:abcdef0123456789","worker_count":2,"workers":["w1","w2"]}]}`))
	defer srv.Close()

	models, err := newTestClient(t, srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Name != "llama3" || models[0].WorkerCount != 2 {
		t.Fatalf("unexpected models: %+v", models)
	}
	if cap.path != "/models" {
		t.Fatalf("path = %q, want /models", cap.path)
	}
}

func TestListWorkers(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"workers":[{"id":"w1","models":["llama3"],"status":"online","active_jobs":1,"load":2}]}`)
	}))
	defer srv.Close()

	workers, err := newTestClient(t, srv).ListWorkers(context.Background())
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != "w1" {
		t.Fatalf("unexpected workers: %+v", workers)
	}
}

// TestErrorMapping proves each HTTP status class maps to its sentinel error and
// the decoded envelope message surfaces.
func TestErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		msg    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`, ErrUnauthorized, "invalid api key"},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"admin role required","code":"forbidden"}}`, ErrForbidden, "admin role required"},
		{"not found", http.StatusNotFound, `{"error":{"message":"key not found","code":"not_found"}}`, ErrNotFound, "key not found"},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down","code":"rate_limited"}}`, ErrRateLimited, "slow down"},
		{"server error", http.StatusInternalServerError, `{"error":{"message":"boom","code":"internal_error"}}`, ErrServer, "boom"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv).GetKey(context.Background(), "x")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v is not %v", err, tc.want)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not *APIError: %T", err)
			}
			if apiErr.Status != tc.status {
				t.Fatalf("status = %d, want %d", apiErr.Status, tc.status)
			}
			if apiErr.Message != tc.msg {
				t.Fatalf("message = %q, want %q", apiErr.Message, tc.msg)
			}
		})
	}
}

// TestErrorNonJSONBody proves a non-JSON error body still yields a useful
// *APIError carrying the status and the raw body as the message.
func TestErrorNonJSONBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream is down")
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).GetKey(context.Background(), "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %v", err)
	}
	if apiErr.Status != http.StatusBadGateway || apiErr.Message != "upstream is down" {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if !errors.Is(err, ErrServer) {
		t.Fatalf("502 should map to ErrServer: %v", err)
	}
}

// TestTransportErrorNotAPIError proves a connection failure (server closed) is a
// transport error, not an *APIError, so the CLI can map it to the network exit
// code.
func TestTransportErrorNotAPIError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // close immediately so the next request is refused.

	c := New(url, "agpu_test_secret")
	_, err := c.ListKeys(context.Background())
	if err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("transport failure should not be an *APIError: %v", err)
	}
}

// TestNoTokenOmitsAuthHeader proves an empty token sends no Authorization header
// (the offline/no-auth case), so the header is present iff a token is configured.
func TestNoTokenOmitsAuthHeader(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, `{"keys":[]}`))
	defer srv.Close()

	c := New(srv.URL, "", WithHTTPClient(srv.Client()))
	if _, err := c.ListKeys(context.Background()); err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if cap.auth != "" {
		t.Fatalf("expected no Authorization header, got %q", cap.auth)
	}
}
