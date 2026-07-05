package main

import (
	"net/http"
	"strings"
	"testing"
)

// cliConfigBody is the canned GET/PUT /v1/admin/config response the config-command
// tests serve. It mirrors the server's configResponse shape.
const cliConfigBody = `{
	"settings":{
		"log_level":"info",
		"quota_default_rpm":60,"quota_default_tpm":0,"quota_default_daily_tokens":0,"quota_default_monthly_tokens":0,
		"quota_global_rpm":0,"quota_global_tpm":0,
		"session_ttl":"30m0s","session_max_turns":20,"session_max_bytes":1048576,
		"session_max_context_tokens":8192,"session_max_sessions_per_key":100,
		"session_overflow_policy":"trim","model_warm_max":"5m0s","heartbeat_timeout":"45s"
	},
	"read_only":{
		"server_listen":"0.0.0.0:9090","server_http_listen":"0.0.0.0:8080","server_metrics_listen":"",
		"quota_path":"/var/lib/agentgpu/quota.json","session_path":"","log_format":"json","log_output":"stderr"
	},
	"read_only_fields":["log_format","log_output","quota_path","server_http_listen","server_listen","server_metrics_listen","session_path"]
}`

// TestConfigGetHTTP proves `config get` reads GET /v1/admin/config and renders both
// the tunable settings and the read-only section.
func TestConfigGetHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/config": {http.StatusOK, cliConfigBody},
	})

	out, err := runHTTP(t, a, runConfigCmd, "get")
	if err != nil {
		t.Fatalf("config get: %v", err)
	}
	for _, want := range []string{"log_level", "info", "quota_default_rpm", "60", "session_ttl", "30m0s",
		"Read-only", "server_listen", "0.0.0.0:9090"} {
		if !strings.Contains(out, want) {
			t.Fatalf("config get missing %q: %q", want, out)
		}
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/v1/admin/config" {
		t.Fatalf("sent %s %s, want GET /v1/admin/config", a.lastReq.method, a.lastReq.path)
	}
}

// TestConfigSetHTTP proves `config set field=value ...` coerces value types
// correctly (integer fields as numbers, the rest as strings), PUTs them, and echoes
// the resulting config.
func TestConfigSetHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/config": {http.StatusOK, cliConfigBody},
	})

	out, err := runHTTP(t, a, runConfigCmd, "set", "log_level=debug", "quota_default_rpm=120", "session_ttl=45m")
	if err != nil {
		t.Fatalf("config set: %v", err)
	}
	if !strings.Contains(out, "Updated runtime config.") {
		t.Fatalf("set did not confirm the update: %q", out)
	}
	if a.lastReq.method != http.MethodPut || a.lastReq.path != "/v1/admin/config" {
		t.Fatalf("sent %s %s, want PUT /v1/admin/config", a.lastReq.method, a.lastReq.path)
	}
	// log_level is a string, quota_default_rpm is a number, session_ttl is a string.
	if a.lastReq.body["log_level"] != "debug" || a.lastReq.body["session_ttl"] != "45m" {
		t.Fatalf("string fields wrong on wire: %v", a.lastReq.body)
	}
	if rpm, ok := a.lastReq.body["quota_default_rpm"].(float64); !ok || rpm != 120 {
		t.Fatalf("quota_default_rpm should be a JSON number 120, got %v (%T)",
			a.lastReq.body["quota_default_rpm"], a.lastReq.body["quota_default_rpm"])
	}
	if len(a.lastReq.body) != 3 {
		t.Fatalf("body should carry exactly the 3 patched fields, got %v", a.lastReq.body)
	}
}

// TestConfigSetValidationLocal proves the CLI rejects malformed/contradictory
// `config set` invocations BEFORE any request (a usage error, exit 2): a missing
// field=value pair, a non-integer value for an integer field, and a duplicate field.
func TestConfigSetValidationLocal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{"no pairs", []string{"set"}},
		{"not a pair", []string{"set", "log_level"}},
		{"empty field", []string{"set", "=debug"}},
		{"non-integer int field", []string{"set", "quota_default_rpm=lots"}},
		{"duplicate field", []string{"set", "log_level=debug", "log_level=info"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A stub that would 500 if the command erroneously sent a request, so a
			// pre-request usage error is unambiguous.
			a := newAdminStub(t, map[string]stubResponse{})
			_, err := runHTTP(t, a, runConfigCmd, tc.args...)
			if err == nil {
				t.Fatalf("expected a usage error for %q", tc.args)
			}
			if got := exitCode(err); got != exitUsage {
				t.Fatalf("exit code = %d, want %d (err: %v)", got, exitUsage, err)
			}
		})
	}
}

// TestConfigServerValidationError proves a server-side validation 400 (e.g. a bad
// enum value) surfaces with the server's message and the general error exit code
// (a 400 is neither auth nor not-found).
func TestConfigServerValidationError(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/config": {http.StatusBadRequest,
			`{"error":{"message":"invalid log_level \"loud\": must be one of debug, info, warn, error","code":"invalid_request_error"}}`},
	})

	_, err := runHTTP(t, a, runConfigCmd, "set", "log_level=loud")
	if err == nil {
		t.Fatal("expected the server validation error")
	}
	if got := exitCode(err); got != exitError {
		t.Fatalf("exit code = %d, want %d (err: %v)", got, exitError, err)
	}
	if !strings.Contains(err.Error(), "invalid log_level") {
		t.Fatalf("error should carry the server message: %v", err)
	}
}

// TestConfigReadOnlyFieldRejected proves a read-only field rejection from the
// server (PUT 400) surfaces clearly.
func TestConfigReadOnlyFieldRejected(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"PUT /v1/admin/config": {http.StatusBadRequest,
			`{"error":{"message":"field \"server_listen\" is read-only; restart to change it","code":"invalid_request_error"}}`},
	})

	_, err := runHTTP(t, a, runConfigCmd, "set", "server_listen=0.0.0.0:9")
	if err == nil {
		t.Fatal("expected the read-only rejection")
	}
	if !strings.Contains(err.Error(), "is read-only") {
		t.Fatalf("error should explain the field is read-only: %v", err)
	}
}

// TestConfigAuthErrors proves the config command maps the typed auth errors to the
// auth exit code on both GET and PUT.
func TestConfigAuthErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		route  string
		args   []string
		status int
		body   string
	}{
		{"get unauthorized", "GET /v1/admin/config", []string{"get"}, http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`},
		{"get forbidden", "GET /v1/admin/config", []string{"get"}, http.StatusForbidden, `{"error":{"message":"insufficient scope: config:read","code":"forbidden"}}`},
		{"set forbidden", "PUT /v1/admin/config", []string{"set", "log_level=debug"}, http.StatusForbidden, `{"error":{"message":"insufficient scope: config:write","code":"forbidden"}}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAdminStub(t, map[string]stubResponse{tc.route: {tc.status, tc.body}})
			_, err := runHTTP(t, a, runConfigCmd, tc.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := exitCode(err); got != exitAuth {
				t.Fatalf("exit code = %d, want %d (err: %v)", got, exitAuth, err)
			}
		})
	}
}

// TestConfigUnknownSubcommand proves an unknown `config` subcommand is a usage
// error and help is a clean exit.
func TestConfigUnknownSubcommand(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	if _, err := runHTTP(t, a, runConfigCmd, "frobnicate"); exitCode(err) != exitUsage {
		t.Fatalf("unknown subcommand exit = %d, want %d", exitCode(err), exitUsage)
	}
	out, err := runHTTP(t, a, runConfigCmd, "--help")
	if exitCode(err) != exitOK {
		t.Fatalf("--help exit = %d, want 0", exitCode(err))
	}
	if !strings.Contains(out, "set field=value") {
		t.Fatalf("help missing the set synopsis: %q", out)
	}
}
