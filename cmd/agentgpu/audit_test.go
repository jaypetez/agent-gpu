package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestAuditHTTP proves `audit` reads the audit log and renders the table, and that
// the filter flags are sent as query parameters.
func TestAuditHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/audit": {http.StatusOK,
			`{"data":[{"time":"2024-06-21T10:00:00Z","actor":"admin1","op":"key.create","target":"k9","outcome":"success","request_id":"req-1"}],"pagination":{"next_cursor":null,"has_more":false}}`},
	})

	out, err := runHTTP(t, a, runAuditCmd, "--op", "key.create", "--actor", "admin1")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, want := range []string{"TIME", "ACTOR", "OP", "admin1", "key.create", "k9", "success", "req-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit missing %q: %q", want, out)
		}
	}
	if a.lastReq.path != "/v1/admin/audit" {
		t.Fatalf("path = %q, want /v1/admin/audit", a.lastReq.path)
	}
}

// TestAuditQueryParams proves the filter (including a relative --since duration) is
// encoded into the request's query string.
func TestAuditQueryParams(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/audit": {http.StatusOK, `{"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})

	out, err := runHTTP(t, a, runAuditCmd, "--actor", "admin1", "--op", "key.revoke", "--target", "k1", "--since", "24h")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(out, "No audit entries.") {
		t.Fatalf("empty audit should print a notice: %q", out)
	}
	q, _ := url.ParseQuery(a.lastReq.query)
	if q.Get("actor") != "admin1" || q.Get("op") != "key.revoke" || q.Get("target") != "k1" {
		t.Fatalf("query missing string filters: %q", a.lastReq.query)
	}
	if q.Get("since") == "" {
		t.Fatalf("relative --since should encode a since bound: %q", a.lastReq.query)
	}
}

// TestAuditEmpty proves an empty result prints the no-entries notice.
func TestAuditEmpty(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/audit": {http.StatusOK, `{"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})
	out, err := runHTTP(t, a, runAuditCmd)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(out, "No audit entries.") {
		t.Fatalf("empty notice missing: %q", out)
	}
}

// TestAuditBadSince proves a malformed --since is a usage error BEFORE any request.
func TestAuditBadSince(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	_, err := runHTTP(t, a, runAuditCmd, "--since", "yesterday")
	if err == nil {
		t.Fatal("expected a usage error for a malformed --since")
	}
	if got := exitCode(err); got != exitUsage {
		t.Fatalf("exit = %d, want %d (err: %v)", got, exitUsage, err)
	}
}

// TestAuditForbidden proves a 403 (a token lacking audit:read) maps to the auth
// exit code.
func TestAuditForbidden(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/audit": {http.StatusForbidden, `{"error":{"message":"insufficient scope: audit:read","code":"forbidden"}}`},
	})
	_, err := runHTTP(t, a, runAuditCmd)
	if exitCode(err) != exitAuth {
		t.Fatalf("forbidden exit = %d, want %d (err: %v)", exitCode(err), exitAuth, err)
	}
}
