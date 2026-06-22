package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adminStub is an httptest server standing in for the agent-gpu admin + catalog
// HTTP API. It records the last request (method, path, decoded body, auth header)
// so a CLI test can assert the command sent the right call, and serves canned
// JSON per route. routes maps "METHOD PATH" to a response.
type adminStub struct {
	srv      *httptest.Server
	lastReq  recordedReq
	response map[string]stubResponse
}

type recordedReq struct {
	method string
	path   string
	query  string // raw URL query string (RawQuery), for asserting filter encoding
	auth   string
	body   map[string]any
}

type stubResponse struct {
	status int
	body   string
}

// newAdminStub builds an adminStub with the given canned responses. A request
// with no matching route gets 404 with an error envelope, mirroring the server.
func newAdminStub(t *testing.T, routes map[string]stubResponse) *adminStub {
	t.Helper()
	a := &adminStub{response: routes}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.lastReq = recordedReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			a.lastReq.body = map[string]any{}
			_ = json.Unmarshal(b, &a.lastReq.body)
		}
		resp, ok := a.response[r.Method+" "+r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"key not found","code":"not_found"}}`)
			return
		}
		if resp.body == "" {
			w.WriteHeader(resp.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// runCmd drives a top-level CLI handler with an injected writer, appending the
// stub's URL and a test token so the command runs in HTTP mode against the stub.
func runHTTP(t *testing.T, a *adminStub, fn func(context.Context, io.Writer, []string) error, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	full := append(args, "--server", a.srv.URL, "--token", "agpu_admin_secret")
	err := fn(context.Background(), &out, full)
	return out.String(), err
}

func TestKeyCreateHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"POST /v1/admin/keys": {http.StatusCreated,
			`{"id":"k1","name":"app","token":"agpu_k1_thesecret","roles":["user"],"allow_models":[],"deny_models":[],"created":1}`},
	})

	out, err := runHTTP(t, a, runKeyCmd, "create", "--name", "app", "--role", "user")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out, "Token: agpu_k1_thesecret") {
		t.Fatalf("create did not print the one-time token: %q", out)
	}
	if !strings.Contains(out, "will not be shown again") {
		t.Fatalf("create missing one-time warning: %q", out)
	}
	if a.lastReq.method != http.MethodPost || a.lastReq.path != "/v1/admin/keys" {
		t.Fatalf("sent %s %s", a.lastReq.method, a.lastReq.path)
	}
	if a.lastReq.auth != "Bearer agpu_admin_secret" {
		t.Fatalf("auth = %q", a.lastReq.auth)
	}
	if a.lastReq.body["name"] != "app" {
		t.Fatalf("body name = %v", a.lastReq.body["name"])
	}
	roles, _ := a.lastReq.body["roles"].([]any)
	if len(roles) != 1 || roles[0] != "user" {
		t.Fatalf("body roles = %v", a.lastReq.body["roles"])
	}
}

func TestKeyListHTTPNoSecretLeak(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/keys": {http.StatusOK,
			`{"data":[{"id":"k1","name":"app","roles":["admin"],"admin_scopes":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":7,"created":1700000000,"last_used":1700000100}],"pagination":{"next_cursor":null,"has_more":false}}`},
	})

	out, err := runHTTP(t, a, runKeyCmd, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"k1", "app", "admin", "7"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("list output should not contain any secret material: %q", out)
	}
}

func TestKeyRevokeHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"DELETE /v1/admin/keys/k1": {http.StatusNoContent, ""},
	})

	out, err := runHTTP(t, a, runKeyCmd, "revoke", "k1")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !strings.Contains(out, "Revoked key k1") {
		t.Fatalf("revoke output: %q", out)
	}
	if a.lastReq.method != http.MethodDelete || a.lastReq.path != "/v1/admin/keys/k1" {
		t.Fatalf("sent %s %s, want DELETE /v1/admin/keys/k1", a.lastReq.method, a.lastReq.path)
	}
}

func TestKeyRotateHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"POST /v1/admin/keys/k1/rotate": {http.StatusOK, `{"id":"k1","token":"agpu_k1_rotated"}`},
	})

	out, err := runHTTP(t, a, runKeyCmd, "rotate", "k1")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !strings.Contains(out, "Token: agpu_k1_rotated") {
		t.Fatalf("rotate did not print the new token: %q", out)
	}
	if a.lastReq.path != "/v1/admin/keys/k1/rotate" {
		t.Fatalf("path = %q", a.lastReq.path)
	}
}

func TestKeyPermsHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/keys/k1/permissions": {http.StatusOK,
			`{"id":"k1","name":"app","roles":["user"],"allow_models":["llama3"],"deny_models":[],"revoked":false,"usage_count":0,"created":1}`},
	})

	out, err := runHTTP(t, a, runKeyCmd, "perms", "k1", "--role", "user", "--allow-model", "llama3")
	if err != nil {
		t.Fatalf("perms: %v", err)
	}
	if !strings.Contains(out, "Updated permissions for key k1") {
		t.Fatalf("perms output: %q", out)
	}
	if a.lastReq.method != http.MethodPut || a.lastReq.path != "/v1/admin/keys/k1/permissions" {
		t.Fatalf("sent %s %s", a.lastReq.method, a.lastReq.path)
	}
	allow, _ := a.lastReq.body["allow_models"].([]any)
	if len(allow) != 1 || allow[0] != "llama3" {
		t.Fatalf("body allow_models = %v", a.lastReq.body["allow_models"])
	}
}

func TestQuotaSetHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/keys/k1/quota": {http.StatusOK,
			`{"id":"k1","name":"app","roles":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":0,"created":1,"limits":{"rpm":60,"tpm":1000,"daily_tokens":0,"monthly_tokens":0}}`},
	})

	out, err := runHTTP(t, a, runQuotaCmd, "set", "k1", "--rpm", "60", "--tpm", "1000")
	if err != nil {
		t.Fatalf("quota set: %v", err)
	}
	if !strings.Contains(out, "Updated quota for key k1") {
		t.Fatalf("set output: %q", out)
	}
	if !strings.Contains(out, "RPM: 60") || !strings.Contains(out, "TPM: 1000") {
		t.Fatalf("set did not echo limits: %q", out)
	}
	if a.lastReq.method != http.MethodPut || a.lastReq.path != "/v1/admin/keys/k1/quota" {
		t.Fatalf("sent %s %s", a.lastReq.method, a.lastReq.path)
	}
	// Only rpm and tpm were provided; the body must carry them and omit daily/monthly.
	if _, ok := a.lastReq.body["rpm"]; !ok {
		t.Fatalf("body missing rpm: %v", a.lastReq.body)
	}
	if _, ok := a.lastReq.body["daily_tokens"]; ok {
		t.Fatalf("body should omit daily_tokens: %v", a.lastReq.body)
	}
}

func TestQuotaSetClearHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/keys/k1/quota": {http.StatusOK,
			`{"id":"k1","name":"app","roles":[],"allow_models":[],"deny_models":[],"revoked":false,"usage_count":0,"created":1}`},
	})

	out, err := runHTTP(t, a, runQuotaCmd, "set", "k1", "--clear")
	if err != nil {
		t.Fatalf("quota set --clear: %v", err)
	}
	if !strings.Contains(out, "Cleared quota override for key k1") {
		t.Fatalf("clear output: %q", out)
	}
	if len(a.lastReq.body) != 0 {
		t.Fatalf("clear body should be empty, got %v", a.lastReq.body)
	}
}

func TestQuotaSetRequiresAFlag(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	// Neither a numeric flag nor --clear: a usage error, no request sent.
	_, err := runHTTP(t, a, runQuotaCmd, "set", "k1")
	if err == nil {
		t.Fatal("expected a usage error for an empty quota set")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("exit code = %d, want %d", exitCode(err), exitUsage)
	}
}

func TestQuotaShowHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/keys/k1/quota": {http.StatusOK,
			`{"key_id":"k1","limits":{"rpm":60,"tpm":1000,"daily_tokens":0,"monthly_tokens":0},"requests_this_minute":5,"tokens_this_minute":100,"tokens_today":500,"tokens_this_month":900,"minute_resets_at":1700000000,"day_resets_at":1700000000,"month_resets_at":1700000000}`},
	})

	out, err := runHTTP(t, a, runQuotaCmd, "show", "k1")
	if err != nil {
		t.Fatalf("quota show: %v", err)
	}
	for _, want := range []string{"Quota for key k1", "requests/min", "60", "1000", "unlimited"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show missing %q: %q", want, out)
		}
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/v1/admin/keys/k1/quota" {
		t.Fatalf("sent %s %s", a.lastReq.method, a.lastReq.path)
	}
}

func TestModelsListHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /models": {http.StatusOK,
			`{"models":[{"name":"llama3","digest":"sha256:abcdef0123456789","worker_count":2,"workers":["w2","w1"]},{"name":"mistral","digest":"","worker_count":0,"workers":[]}]}`},
	})

	out, err := runHTTP(t, a, runModelsCmd, "list")
	if err != nil {
		t.Fatalf("models list: %v", err)
	}
	// Table header + both models, with the digest trimmed and workers sorted.
	for _, want := range []string{"NAME", "DIGEST", "WORKERS", "llama3", "abcdef012345", "w1,w2", "mistral"} {
		if !strings.Contains(out, want) {
			t.Fatalf("models list missing %q: %q", want, out)
		}
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/models" {
		t.Fatalf("sent %s %s, want GET /models", a.lastReq.method, a.lastReq.path)
	}
}

func TestModelsListJSON(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /models": {http.StatusOK, `{"models":[{"name":"llama3","digest":"d","worker_count":1,"workers":["w1"]}]}`},
	})

	out, err := runHTTP(t, a, runModelsCmd, "list", "--json")
	if err != nil {
		t.Fatalf("models list --json: %v", err)
	}
	// The raw JSON is re-emitted (indented); it must parse back to the same shape.
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(parsed.Models) != 1 || parsed.Models[0].Name != "llama3" {
		t.Fatalf("unexpected JSON: %s", out)
	}
}

func TestModelsListOpenAI(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/models": {http.StatusOK, `{"object":"list","data":[{"id":"llama3","object":"model","created":0,"owned_by":"agent-gpu"}]}`},
	})

	out, err := runHTTP(t, a, runModelsCmd, "list", "--openai")
	if err != nil {
		t.Fatalf("models list --openai: %v", err)
	}
	if a.lastReq.path != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", a.lastReq.path)
	}
	if !strings.Contains(out, "llama3") || !strings.Contains(out, "\"object\"") {
		t.Fatalf("openai output: %q", out)
	}
}

// TestHTTPErrorExitCodes proves the typed client errors map to the documented
// exit codes through a real CLI invocation.
func TestHTTPErrorExitCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`, exitAuth},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"admin role required","code":"forbidden"}}`, exitAuth},
		{"not found", http.StatusNotFound, `{"error":{"message":"key not found","code":"not_found"}}`, exitNotFound},
		{"server", http.StatusInternalServerError, `{"error":{"message":"boom","code":"internal_error"}}`, exitError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAdminStub(t, map[string]stubResponse{
				"GET /v1/admin/keys": {tc.status, tc.body},
			})
			_, err := runHTTP(t, a, runKeyCmd, "list")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := exitCode(err); got != tc.want {
				t.Fatalf("exit code = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

// TestNoTokenIsUsageError proves that without a token (and without --local) the
// command refuses with a usage error rather than silently doing the wrong thing.
func TestNoTokenIsUsageError(t *testing.T) {
	// Not parallel: t.Setenv pins AGENTGPU_TOKEN empty so the result does not
	// depend on the ambient environment, and Setenv forbids t.Parallel.
	var out bytes.Buffer
	t.Setenv("AGENTGPU_TOKEN", "")
	err := runKeyCmd(context.Background(), &out, []string{"create", "--name", "app", "--server", "http://127.0.0.1:9"})
	if err == nil {
		t.Fatal("expected a usage error without a token")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("exit code = %d, want %d", exitCode(err), exitUsage)
	}
	if !strings.Contains(err.Error(), "--local") {
		t.Fatalf("error should mention the --local bootstrap path: %v", err)
	}
}

// TestModelsNoTokenUsageError proves the http-only models command refuses without
// a token AND does not suggest --local (which it does not support).
func TestModelsNoTokenUsageError(t *testing.T) {
	// Not parallel: t.Setenv pins AGENTGPU_TOKEN empty so the result is independent
	// of the ambient environment.
	var out bytes.Buffer
	t.Setenv("AGENTGPU_TOKEN", "")
	err := runModelsCmd(context.Background(), &out, []string{"list", "--server", "http://127.0.0.1:9"})
	if err == nil {
		t.Fatal("expected a usage error without a token")
	}
	if exitCode(err) != exitUsage {
		t.Fatalf("exit code = %d, want %d", exitCode(err), exitUsage)
	}
	if strings.Contains(err.Error(), "--local") {
		t.Fatalf("models is http-only; error should not suggest --local: %v", err)
	}
}

// TestNetworkErrorExit proves an unreachable server maps to the network exit code.
func TestNetworkErrorExit(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	// Port 1 on loopback refuses immediately; with a token set the command reaches
	// the transport and fails there, not at the no-token guard.
	err := runKeyCmd(context.Background(), &out,
		[]string{"list", "--server", "http://127.0.0.1:1", "--token", "agpu_x_y"})
	if err == nil {
		t.Fatal("expected a network error")
	}
	if got := exitCode(err); got != exitNetwork {
		t.Fatalf("exit code = %d, want %d (err: %v)", got, exitNetwork, err)
	}
}
