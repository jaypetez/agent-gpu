package main

import (
	"net/http"
	"strings"
	"testing"
)

// telemetryBody is a canned GET /v1/admin/telemetry summary the telemetry-command
// tests serve.
const telemetryBody = `{
	"requests":{"count":1234,"latency":{"sum_ms":0,"max_ms":900,"mean_ms":42,"buckets":[]}},
	"throttles":{"global":3,"key":1},
	"fleet":{"worker_count":2,"by_status":{"online":2},"queue":{"total":5,"by_priority":{"normal":5}},
		"wait_time":{"count":10,"sum_ms":0,"max_ms":250,"mean_ms":30,"buckets":[]}},
	"sessions":{"active":7},
	"affinity":{"hits":80,"misses":20,"rebinds":2},
	"uptime_seconds":3600
}`

// TestTelemetryHTTP proves `telemetry` reads the summary and renders the scalar
// fields plus the by-status breakdown.
func TestTelemetryHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/telemetry": {http.StatusOK, telemetryBody},
	})

	out, err := runHTTP(t, a, runTelemetryCmd)
	if err != nil {
		t.Fatalf("telemetry: %v", err)
	}
	for _, want := range []string{"Uptime", "Requests", "1234", "Latency", "Throttled", "Queue depth", "5",
		"Sessions active", "7", "Workers by status:", "online"} {
		if !strings.Contains(out, want) {
			t.Fatalf("telemetry missing %q: %q", want, out)
		}
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/v1/admin/telemetry" {
		t.Fatalf("sent %s %s, want GET /v1/admin/telemetry", a.lastReq.method, a.lastReq.path)
	}
}

// TestTelemetryNoStatuses proves an empty by-status map omits the breakdown section
// without error.
func TestTelemetryNoStatuses(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/telemetry": {http.StatusOK,
			`{"requests":{"count":0,"latency":{"sum_ms":0,"max_ms":0,"mean_ms":0,"buckets":[]}},
			"throttles":{"global":0,"key":0},
			"fleet":{"worker_count":0,"by_status":{},"queue":{"total":0,"by_priority":{}},"wait_time":{"count":0,"sum_ms":0,"max_ms":0,"mean_ms":0,"buckets":[]}},
			"sessions":{"active":0},"affinity":{"hits":0,"misses":0,"rebinds":0},"uptime_seconds":0}`},
	})
	out, err := runHTTP(t, a, runTelemetryCmd)
	if err != nil {
		t.Fatalf("telemetry: %v", err)
	}
	if strings.Contains(out, "Workers by status:") {
		t.Fatalf("empty by-status should omit the breakdown: %q", out)
	}
}

// TestTelemetryErrors proves auth/forbidden/server errors map to the right exit
// codes.
func TestTelemetryErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`, exitAuth},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"insufficient scope: telemetry:read","code":"forbidden"}}`, exitAuth},
		{"server", http.StatusInternalServerError, `{"error":{"message":"boom","code":"internal_error"}}`, exitError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAdminStub(t, map[string]stubResponse{"GET /v1/admin/telemetry": {tc.status, tc.body}})
			_, err := runHTTP(t, a, runTelemetryCmd)
			if got := exitCode(err); got != tc.want {
				t.Fatalf("exit = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

// TestTelemetryHelp proves `telemetry --help` is a clean exit with the synopsis.
func TestTelemetryHelp(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	out, err := runHTTP(t, a, runTelemetryCmd, "--help")
	if exitCode(err) != exitOK {
		t.Fatalf("--help exit = %d, want 0 (err: %v)", exitCode(err), err)
	}
	if !strings.Contains(out, "dashboard telemetry summary") {
		t.Fatalf("help missing synopsis: %q", out)
	}
}
