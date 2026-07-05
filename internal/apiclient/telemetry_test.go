package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTelemetry proves the client decodes the GET /v1/admin/telemetry summary into
// the typed Telemetry across every section and sends the request to the right path.
// It uses the shared recordingHandler so the exact request the client makes is
// asserted alongside the decoded response.
func TestTelemetry(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, `{
		"requests":{
			"count":1840,
			"latency":{
				"sum_ms":220800,"max_ms":4200,"mean_ms":120,
				"buckets":[
					{"le_ms":10,"count":320},
					{"le_ms":100,"count":1180},
					{"le_ms":1000,"count":1790},
					{"le_ms":10000,"count":1840},
					{"le_ms":0,"count":1840}
				]
			}
		},
		"throttles":{"global":4,"key":1},
		"fleet":{
			"worker_count":2,
			"by_status":{"online":1,"draining":1},
			"queue":{"total":3,"by_priority":{"normal":2,"high":1}},
			"wait_time":{"count":128,"sum_ms":76800,"max_ms":4200,"mean_ms":600,"buckets":[
				{"le_ms":100,"count":40},
				{"le_ms":0,"count":128}
			]}
		},
		"sessions":{"active":7},
		"affinity":{"hits":512,"misses":18,"rebinds":18},
		"uptime_seconds":3600
	}`))
	defer srv.Close()

	got, err := newTestClient(t, srv).Telemetry(context.Background())
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}

	if cap.method != http.MethodGet || cap.path != "/v1/admin/telemetry" {
		t.Fatalf("sent %s %s, want GET /v1/admin/telemetry", cap.method, cap.path)
	}

	// requests + latency
	if got.Requests.Count != 1840 {
		t.Errorf("requests.count = %d, want 1840", got.Requests.Count)
	}
	if got.Requests.Latency.SumMs != 220800 || got.Requests.Latency.MaxMs != 4200 || got.Requests.Latency.MeanMs != 120 {
		t.Errorf("latency core = %+v", got.Requests.Latency)
	}
	if n := len(got.Requests.Latency.Buckets); n != 5 {
		t.Fatalf("latency buckets len = %d, want 5", n)
	}
	if last := got.Requests.Latency.Buckets[4]; last.LeMs != 0 || last.Count != 1840 {
		t.Errorf("latency +Inf bucket = %+v, want {le_ms:0 count:1840}", last)
	}

	// throttles
	if got.Throttles.Global != 4 || got.Throttles.Key != 1 {
		t.Errorf("throttles = %+v, want {global:4 key:1}", got.Throttles)
	}

	// fleet
	if got.Fleet.WorkerCount != 2 {
		t.Errorf("fleet.worker_count = %d, want 2", got.Fleet.WorkerCount)
	}
	if got.Fleet.ByStatus["online"] != 1 || got.Fleet.ByStatus["draining"] != 1 {
		t.Errorf("fleet.by_status = %+v", got.Fleet.ByStatus)
	}
	if got.Fleet.Queue.Total != 3 || got.Fleet.Queue.ByPriority["normal"] != 2 || got.Fleet.Queue.ByPriority["high"] != 1 {
		t.Errorf("fleet.queue = %+v", got.Fleet.Queue)
	}
	if got.Fleet.WaitTime.Count != 128 || got.Fleet.WaitTime.MeanMs != 600 {
		t.Errorf("fleet.wait_time = %+v", got.Fleet.WaitTime)
	}

	// sessions, affinity, uptime
	if got.Sessions.Active != 7 {
		t.Errorf("sessions.active = %d, want 7", got.Sessions.Active)
	}
	if got.Affinity.Hits != 512 || got.Affinity.Misses != 18 || got.Affinity.Rebinds != 18 {
		t.Errorf("affinity = %+v", got.Affinity)
	}
	if got.UptimeSeconds != 3600 {
		t.Errorf("uptime_seconds = %d, want 3600", got.UptimeSeconds)
	}
}

// TestTelemetryForbidden proves the client maps a 403 (a token lacking
// telemetry:read) to the typed ErrForbidden sentinel.
func TestTelemetryForbidden(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient scope","code":"forbidden"}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).Telemetry(context.Background()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Telemetry err = %v, want ErrForbidden", err)
	}
}
