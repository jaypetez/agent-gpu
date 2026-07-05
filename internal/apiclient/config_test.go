package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the GetConfig/PutConfig client methods added in #104 (the only admin
// endpoints the typed client did not already wrap). They use the shared
// recordingHandler so each asserts the exact request the client sends and the
// response/error mapping, against an in-process httptest server.

// configBody is a representative GET/PUT /v1/admin/config response: the tunable
// settings, the boot-only read-only values, and the read-only field-key list.
const configBody = `{
	"settings":{
		"log_level":"info",
		"quota_default_rpm":60,"quota_default_tpm":1000,"quota_default_daily_tokens":0,"quota_default_monthly_tokens":0,
		"quota_global_rpm":0,"quota_global_tpm":0,
		"session_ttl":"30m0s","session_max_turns":20,"session_max_bytes":1048576,
		"session_max_context_tokens":8192,"session_max_sessions_per_key":100,
		"session_overflow_policy":"trim","model_warm_max":"5m0s",
		"heartbeat_timeout":"45s"
	},
	"read_only":{
		"server_listen":"0.0.0.0:9090","server_http_listen":"0.0.0.0:8080","server_metrics_listen":"",
		"quota_path":"/var/lib/agentgpu/quota.json","session_path":"/var/lib/agentgpu/sessions.json",
		"log_format":"json","log_output":"stderr"
	},
	"read_only_fields":["log_format","log_output","quota_path","server_http_listen","server_listen","server_metrics_listen","session_path"]
}`

// TestGetConfig is the happy path: the client decodes the effective config into
// the typed ConfigResponse and sends GET to the right path with the bearer token.
func TestGetConfig(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, configBody))
	defer srv.Close()

	got, err := newTestClient(t, srv).GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/admin/config" {
		t.Fatalf("sent %s %s, want GET /v1/admin/config", cap.method, cap.path)
	}
	if cap.auth != "Bearer agpu_test_secret" {
		t.Fatalf("auth header = %q", cap.auth)
	}
	if got.Settings.LogLevel != "info" || got.Settings.QuotaDefaultRPM != 60 ||
		got.Settings.SessionTTL != "30m0s" || got.Settings.SessionMaxTurns != 20 ||
		got.Settings.HeartbeatTimeout != "45s" {
		t.Fatalf("settings decoded wrong: %+v", got.Settings)
	}
	if got.ReadOnly.ServerListen != "0.0.0.0:9090" || got.ReadOnly.LogFormat != "json" {
		t.Fatalf("read-only decoded wrong: %+v", got.ReadOnly)
	}
	if len(got.ReadOnlyFields) != 7 || got.ReadOnlyFields[0] != "log_format" {
		t.Fatalf("read-only fields wrong: %+v", got.ReadOnlyFields)
	}
}

// TestGetConfigErrors is the table of GET error mappings: auth (401), forbidden
// scope (403), and a 503 when runtime config is not wired (a plain APIError, not a
// sentinel class — so it must NOT match the typed sentinels).
func TestGetConfigErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		body    string
		wantIs  error // sentinel the error must match (nil = none)
		wantMsg string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`, ErrUnauthorized, "invalid api key"},
		{"forbidden_scope", http.StatusForbidden, `{"error":{"message":"insufficient scope: config:read","code":"forbidden"}}`, ErrForbidden, "insufficient scope"},
		{"unavailable", http.StatusServiceUnavailable, `{"error":{"message":"runtime config is not enabled","code":"unavailable"}}`, nil, "runtime config is not enabled"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv).GetConfig(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantIs)
			}
			// A 503 must not be miscategorized as one of the branchable sentinels.
			if tc.wantIs == nil {
				for _, s := range []error{ErrUnauthorized, ErrForbidden, ErrNotFound, ErrRateLimited} {
					if errors.Is(err, s) {
						t.Fatalf("503 err = %v wrongly matched sentinel %v", err, s)
					}
				}
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestPutConfig is the happy path: the client sends the partial patch as the
// request body (only the present keys), PUTs to the right path, and decodes the
// resulting config from the response.
func TestPutConfig(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, configBody))
	defer srv.Close()

	got, err := newTestClient(t, srv).PutConfig(context.Background(), map[string]any{
		"log_level":         "debug",
		"quota_default_rpm": 120,
		"session_ttl":       "1h",
	})
	if err != nil {
		t.Fatalf("PutConfig: %v", err)
	}
	if cap.method != http.MethodPut || cap.path != "/v1/admin/config" {
		t.Fatalf("sent %s %s, want PUT /v1/admin/config", cap.method, cap.path)
	}
	// Only the three patched fields are on the wire; nothing else is sent.
	if len(cap.body) != 3 {
		t.Fatalf("body should carry exactly the patched fields, got %v", cap.body)
	}
	if cap.body["log_level"] != "debug" || cap.body["session_ttl"] != "1h" {
		t.Fatalf("body = %v, want the patched values", cap.body)
	}
	if v, _ := cap.body["quota_default_rpm"].(float64); int(v) != 120 {
		t.Fatalf("body quota_default_rpm = %v, want 120", cap.body["quota_default_rpm"])
	}
	// The response is decoded so the CLI can print the resulting config.
	if got.Settings.LogLevel != "info" {
		t.Fatalf("response not decoded: %+v", got.Settings)
	}
}

// TestPutConfigEmptyPatch proves an empty patch is still a well-formed request
// (the server treats it as a no-op success that echoes the current config) and the
// client decodes the echoed config.
func TestPutConfigEmptyPatch(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, configBody))
	defer srv.Close()

	got, err := newTestClient(t, srv).PutConfig(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("PutConfig(empty): %v", err)
	}
	if cap.method != http.MethodPut || cap.path != "/v1/admin/config" {
		t.Fatalf("sent %s %s, want PUT /v1/admin/config", cap.method, cap.path)
	}
	if got.Settings.LogLevel != "info" {
		t.Fatalf("response not decoded: %+v", got.Settings)
	}
}

// TestPutConfigErrors is the table of PUT error mappings: a validation 400 (bad
// value), a read-only-field 400, and an unknown-field 400 — all surfaced as a
// plain APIError carrying the server's message (NOT a branchable sentinel, since a
// 400 is a server-reported misuse the CLI prints verbatim) — plus auth/forbidden.
func TestPutConfigErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		body    string
		wantIs  error
		wantMsg string
	}{
		{"validation", http.StatusBadRequest, `{"error":{"message":"invalid log_level \"loud\": must be one of debug, info, warn, error","code":"invalid_request_error"}}`, nil, "invalid log_level"},
		{"read_only_field", http.StatusBadRequest, `{"error":{"message":"field \"server_listen\" is read-only; restart to change it","code":"invalid_request_error"}}`, nil, "is read-only"},
		{"unknown_field", http.StatusBadRequest, `{"error":{"message":"unknown config field \"nope\"","code":"invalid_request_error"}}`, nil, "unknown config field"},
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`, ErrUnauthorized, ""},
		{"forbidden_scope", http.StatusForbidden, `{"error":{"message":"insufficient scope: config:write","code":"forbidden"}}`, ErrForbidden, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv).PutConfig(context.Background(), map[string]any{"log_level": "loud"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantIs)
			}
			if tc.wantIs == nil && errors.Is(err, ErrNotFound) {
				t.Fatalf("400 err = %v wrongly matched ErrNotFound", err)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
