package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestWorkersListHTTP proves `workers list` reads the cursor-paginated fleet and
// renders the table, and that an empty fleet prints the no-workers notice.
func TestWorkersListHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/workers": {http.StatusOK,
			`{"data":[{"id":"w1","models":["llama3","mistral"],"status":"online","active_jobs":2,"total_vram":42949672960,"free_vram":21474836480,"load":42,"gpu_type":"a100","last_seen":1700000000}],"pagination":{"next_cursor":null,"has_more":false}}`},
	})

	out, err := runHTTP(t, a, runWorkersCmd, "list")
	if err != nil {
		t.Fatalf("workers list: %v", err)
	}
	for _, want := range []string{"ID", "STATUS", "w1", "online", "a100", "llama3,mistral", "GiB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("workers list missing %q: %q", want, out)
		}
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/v1/admin/workers" {
		t.Fatalf("sent %s %s, want GET /v1/admin/workers", a.lastReq.method, a.lastReq.path)
	}
}

// TestWorkersListEmpty proves the empty-fleet path prints a clear notice and no
// table.
func TestWorkersListEmpty(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/workers": {http.StatusOK, `{"data":[],"pagination":{"next_cursor":null,"has_more":false}}`},
	})
	out, err := runHTTP(t, a, runWorkersCmd, "list")
	if err != nil {
		t.Fatalf("workers list: %v", err)
	}
	if !strings.Contains(out, "No workers connected.") {
		t.Fatalf("empty fleet notice missing: %q", out)
	}
}

// TestWorkersDetailHTTP proves `workers detail <id>` reads the per-worker detail and
// renders its fields.
func TestWorkersDetailHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/workers/w1": {http.StatusOK,
			`{"id":"w1","models":["llama3"],"status":"online","draining":true,"active_jobs":1,"total_vram":42949672960,"free_vram":10737418240,"load":10,"gpu_type":"a100","last_seen":1700000000,"registered_at":1699990000,"uptime_seconds":10000}`},
	})

	out, err := runHTTP(t, a, runWorkersCmd, "detail", "w1")
	if err != nil {
		t.Fatalf("workers detail: %v", err)
	}
	for _, want := range []string{"w1", "online", "Draining", "true", "a100", "Uptime"} {
		if !strings.Contains(out, want) {
			t.Fatalf("workers detail missing %q: %q", want, out)
		}
	}
	if a.lastReq.path != "/v1/admin/workers/w1" {
		t.Fatalf("path = %q, want /v1/admin/workers/w1", a.lastReq.path)
	}
}

// TestWorkersDrainHTTP proves the soft and timed drain forms hit the drain endpoint
// and confirm the action; the timed form sends the deadline in the body.
func TestWorkersDrainHTTP(t *testing.T) {
	t.Parallel()
	t.Run("soft", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{"POST /v1/admin/workers/w1/drain": {http.StatusNoContent, ""}})
		out, err := runHTTP(t, a, runWorkersCmd, "drain", "w1")
		if err != nil {
			t.Fatalf("drain soft: %v", err)
		}
		if !strings.Contains(out, "Draining worker w1") {
			t.Fatalf("drain output: %q", out)
		}
		if len(a.lastReq.body) != 0 {
			t.Fatalf("soft drain should send no body, got %v", a.lastReq.body)
		}
	})
	t.Run("timed", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{"POST /v1/admin/workers/w1/drain": {http.StatusNoContent, ""}})
		out, err := runHTTP(t, a, runWorkersCmd, "drain", "w1", "--deadline", "30s")
		if err != nil {
			t.Fatalf("drain timed: %v", err)
		}
		if !strings.Contains(out, "forced after 30s") {
			t.Fatalf("timed drain output: %q", out)
		}
		if d, _ := a.lastReq.body["deadline_seconds"].(float64); d != 30 {
			t.Fatalf("deadline_seconds = %v, want 30", a.lastReq.body["deadline_seconds"])
		}
	})
}

// TestWorkersPullUnloadHTTP proves pull and unload hit the right method+path and
// carry the model.
func TestWorkersPullUnloadHTTP(t *testing.T) {
	t.Parallel()
	t.Run("pull", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{"POST /v1/admin/workers/w1/models": {http.StatusAccepted, ""}})
		out, err := runHTTP(t, a, runWorkersCmd, "pull", "w1", "llama3")
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if !strings.Contains(out, "Requested pull of \"llama3\"") {
			t.Fatalf("pull output: %q", out)
		}
		if a.lastReq.method != http.MethodPost || a.lastReq.body["model"] != "llama3" {
			t.Fatalf("pull sent %s body=%v", a.lastReq.method, a.lastReq.body)
		}
	})
	t.Run("unload", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{"DELETE /v1/admin/workers/w1/models/llama3": {http.StatusNoContent, ""}})
		out, err := runHTTP(t, a, runWorkersCmd, "unload", "w1", "llama3")
		if err != nil {
			t.Fatalf("unload: %v", err)
		}
		if !strings.Contains(out, "Requested unload of \"llama3\"") {
			t.Fatalf("unload output: %q", out)
		}
		if a.lastReq.method != http.MethodDelete || a.lastReq.path != "/v1/admin/workers/w1/models/llama3" {
			t.Fatalf("unload sent %s %s", a.lastReq.method, a.lastReq.path)
		}
	})
}

// TestWorkersUsageErrors proves the workers command rejects missing positional args
// (a usage error) and maps a not-found detail to the not-found exit code.
func TestWorkersUsageErrors(t *testing.T) {
	t.Parallel()
	t.Run("detail without id", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{})
		_, err := runHTTP(t, a, runWorkersCmd, "detail")
		if exitCode(err) != exitUsage {
			t.Fatalf("missing id exit = %d, want %d", exitCode(err), exitUsage)
		}
	})
	t.Run("pull without model", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{})
		_, err := runHTTP(t, a, runWorkersCmd, "pull", "w1")
		if exitCode(err) != exitUsage {
			t.Fatalf("missing model exit = %d, want %d", exitCode(err), exitUsage)
		}
	})
	t.Run("detail not found", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{
			"GET /v1/admin/workers/ghost": {http.StatusNotFound, `{"error":{"message":"worker not found","code":"not_found"}}`},
		})
		_, err := runHTTP(t, a, runWorkersCmd, "detail", "ghost")
		if exitCode(err) != exitNotFound {
			t.Fatalf("not-found exit = %d, want %d (err: %v)", exitCode(err), exitNotFound, err)
		}
	})
	t.Run("forbidden scope", func(t *testing.T) {
		t.Parallel()
		a := newAdminStub(t, map[string]stubResponse{
			"GET /v1/admin/workers": {http.StatusForbidden, `{"error":{"message":"insufficient scope: workers:read","code":"forbidden"}}`},
		})
		_, err := runHTTP(t, a, runWorkersCmd, "list")
		if exitCode(err) != exitAuth {
			t.Fatalf("forbidden exit = %d, want %d (err: %v)", exitCode(err), exitAuth, err)
		}
	})
}
