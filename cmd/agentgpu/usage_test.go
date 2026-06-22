package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// usageReportBody is a canned GET /v1/admin/usage report (summary + one row) the
// usage-command tests serve. Both GetUsage and ListUsage hit GET /v1/admin/usage,
// so the single stub route serves both calls the command makes.
const usageReportBody = `{
	"summary":{"key_count":1,"global_throttled":4,"key_throttled":1},
	"data":[{"key_id":"k1","name":"batch","owner":"alice","team":"platform",
		"limits":{"rpm":60,"tpm":0,"daily_tokens":1000000,"monthly_tokens":0},
		"requests_this_minute":3,"tokens_this_minute":1200,"tokens_today":640000,"tokens_this_month":5120000,
		"minute_resets_at":1718960460,"day_resets_at":1719014400,"month_resets_at":1719792000,
		"series":[],"forecast":{"daily_exhaustion_at":null,"monthly_exhaustion_at":null}}],
	"pagination":{"next_cursor":null,"has_more":false}
}`

// TestUsageHTTP proves `usage` prints the fleet summary and the per-key row table.
func TestUsageHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/usage": {http.StatusOK, usageReportBody},
	})

	out, err := runHTTP(t, a, runUsageCmd)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	for _, want := range []string{"Keys: 1", "Throttled (global): 4", "KEY", "k1", "batch", "alice", "platform", "640000", "60"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage missing %q: %q", want, out)
		}
	}
	if a.lastReq.path != "/v1/admin/usage" {
		t.Fatalf("path = %q, want /v1/admin/usage", a.lastReq.path)
	}
}

// TestUsageFilters proves the filter flags are sent as query parameters.
func TestUsageFilters(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/usage": {http.StatusOK, usageReportBody},
	})

	if _, err := runHTTP(t, a, runUsageCmd, "--key", "k1", "--owner", "alice", "--team", "platform"); err != nil {
		t.Fatalf("usage: %v", err)
	}
	q, _ := url.ParseQuery(a.lastReq.query)
	if q.Get("key_id") != "k1" || q.Get("owner") != "alice" || q.Get("team") != "platform" {
		t.Fatalf("query missing filters: %q", a.lastReq.query)
	}
}

// TestUsageEmpty proves an empty (no-matching-keys) report still prints the summary
// and a clear notice rather than an empty table.
func TestUsageEmpty(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/usage": {http.StatusOK,
			`{"summary":{"key_count":0,"global_throttled":0,"key_throttled":0},"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})
	out, err := runHTTP(t, a, runUsageCmd, "--owner", "nobody")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if !strings.Contains(out, "Keys: 0") || !strings.Contains(out, "No matching keys.") {
		t.Fatalf("empty usage output: %q", out)
	}
}

// TestUsageForbidden proves a 403 (a token lacking usage:read / telemetry:read)
// maps to the auth exit code.
func TestUsageForbidden(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/usage": {http.StatusForbidden, `{"error":{"message":"insufficient scope","code":"forbidden"}}`},
	})
	_, err := runHTTP(t, a, runUsageCmd)
	if exitCode(err) != exitAuth {
		t.Fatalf("forbidden exit = %d, want %d (err: %v)", exitCode(err), exitAuth, err)
	}
}

// TestUsageUnauthorized proves a 401 maps to the auth exit code (the other auth
// class), rounding out the auth-fail coverage.
func TestUsageUnauthorized(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/usage": {http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`},
	})
	_, err := runHTTP(t, a, runUsageCmd)
	if exitCode(err) != exitAuth {
		t.Fatalf("unauthorized exit = %d, want %d (err: %v)", exitCode(err), exitAuth, err)
	}
}
