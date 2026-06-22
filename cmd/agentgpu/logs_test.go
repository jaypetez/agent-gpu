package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestLogsHTTP proves `logs` reads the log buffer and renders the table including a
// compact attrs rendering.
func TestLogsHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/logs": {http.StatusOK,
			`{"data":[{"time":"2024-06-21T10:00:00Z","level":"ERROR","message":"dispatch failed","attrs":{"request_id":"req-9","worker":"w1"}}],"pagination":{"next_cursor":null,"has_more":false}}`},
	})

	out, err := runHTTP(t, a, runLogsCmd, "--level", "error")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	for _, want := range []string{"TIME", "LEVEL", "MESSAGE", "ATTRS", "ERROR", "dispatch failed", "request_id=req-9", "worker=w1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs missing %q: %q", want, out)
		}
	}
	if a.lastReq.path != "/v1/admin/logs" {
		t.Fatalf("path = %q, want /v1/admin/logs", a.lastReq.path)
	}
}

// TestLogsFilters proves the filter flags (and a relative --since) are sent as query
// parameters.
func TestLogsFilters(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/logs": {http.StatusOK, `{"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})

	if _, err := runHTTP(t, a, runLogsCmd, "--level", "warn", "--request-id", "req-1", "--session-id", "s-1", "--worker", "w1", "--since", "1h"); err != nil {
		t.Fatalf("logs: %v", err)
	}
	q, _ := url.ParseQuery(a.lastReq.query)
	if q.Get("level") != "warn" || q.Get("request_id") != "req-1" || q.Get("session_id") != "s-1" || q.Get("worker") != "w1" {
		t.Fatalf("query missing filters: %q", a.lastReq.query)
	}
	if q.Get("since") == "" {
		t.Fatalf("relative --since should encode a since bound: %q", a.lastReq.query)
	}
}

// TestLogsEmpty proves an empty result prints the no-entries notice.
func TestLogsEmpty(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/logs": {http.StatusOK, `{"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})
	out, err := runHTTP(t, a, runLogsCmd)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(out, "No log entries.") {
		t.Fatalf("empty notice missing: %q", out)
	}
}

// TestLogsBadUntil proves a malformed --until is a usage error before any request.
func TestLogsBadUntil(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	_, err := runHTTP(t, a, runLogsCmd, "--until", "soon")
	if got := exitCode(err); got != exitUsage {
		t.Fatalf("exit = %d, want %d (err: %v)", got, exitUsage, err)
	}
}

// TestLogsForbidden proves a 403 (a token lacking logs:read) maps to the auth exit
// code.
func TestLogsForbidden(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/logs": {http.StatusForbidden, `{"error":{"message":"insufficient scope: logs:read","code":"forbidden"}}`},
	})
	_, err := runHTTP(t, a, runLogsCmd)
	if exitCode(err) != exitAuth {
		t.Fatalf("forbidden exit = %d, want %d (err: %v)", exitCode(err), exitAuth, err)
	}
}
