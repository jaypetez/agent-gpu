package main

import (
	"net/http"
	"strings"
	"testing"
)

// gpusBody is a canned GET /v1/admin/gpus inventory (fleet roll-up, by-type, and a
// per-worker cell) the gpus-command tests serve.
const gpusBody = `{
	"fleet":{"worker_count":2,"total_vram":85899345920,"free_vram":42949672960,"mean_load":30,"max_load":55},
	"by_type":[{"gpu_type":"a100","worker_count":1,"total_vram":42949672960,"free_vram":21474836480},
		{"gpu_type":"cpu","worker_count":1,"total_vram":0,"free_vram":0}],
	"workers":[{"id":"w1","gpu_type":"a100","total_vram":42949672960,"free_vram":21474836480,"load":55,"status":"online","active_jobs":2}]
}`

// TestGPUsHTTP proves `gpus` prints the fleet summary, the by-type grouping, and the
// per-worker section.
func TestGPUsHTTP(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/gpus": {http.StatusOK, gpusBody},
	})

	out, err := runHTTP(t, a, runGPUsCmd)
	if err != nil {
		t.Fatalf("gpus: %v", err)
	}
	for _, want := range []string{"Fleet: 2 worker(s)", "By GPU type:", "a100", "cpu", "Workers:", "w1", "GiB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("gpus missing %q: %q", want, out)
		}
	}
	if a.lastReq.method != http.MethodGet || a.lastReq.path != "/v1/admin/gpus" {
		t.Fatalf("sent %s %s, want GET /v1/admin/gpus", a.lastReq.method, a.lastReq.path)
	}
}

// TestGPUsEmptyFleet proves an empty fleet prints just the roll-up (no by-type or
// per-worker tables) without error.
func TestGPUsEmptyFleet(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{
		"GET /v1/admin/gpus": {http.StatusOK,
			`{"fleet":{"worker_count":0,"total_vram":0,"free_vram":0,"mean_load":0,"max_load":0},"by_type":[],"workers":[]}`},
	})
	out, err := runHTTP(t, a, runGPUsCmd)
	if err != nil {
		t.Fatalf("gpus: %v", err)
	}
	if !strings.Contains(out, "Fleet: 0 worker(s)") {
		t.Fatalf("empty fleet roll-up missing: %q", out)
	}
	if strings.Contains(out, "By GPU type:") || strings.Contains(out, "Workers:") {
		t.Fatalf("empty fleet should not print the grouping tables: %q", out)
	}
}

// TestGPUsErrors proves the gpus command maps auth/forbidden errors to the auth
// exit code and accepts a clean --help.
func TestGPUsErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key","code":"unauthorized"}}`, exitAuth},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"insufficient scope: gpus:read","code":"forbidden"}}`, exitAuth},
		{"server", http.StatusInternalServerError, `{"error":{"message":"boom","code":"internal_error"}}`, exitError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAdminStub(t, map[string]stubResponse{"GET /v1/admin/gpus": {tc.status, tc.body}})
			_, err := runHTTP(t, a, runGPUsCmd)
			if got := exitCode(err); got != tc.want {
				t.Fatalf("exit = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

// TestGPUsHelp proves `gpus --help` is a clean exit with the synopsis.
func TestGPUsHelp(t *testing.T) {
	t.Parallel()
	a := newAdminStub(t, map[string]stubResponse{})
	out, err := runHTTP(t, a, runGPUsCmd, "--help")
	if exitCode(err) != exitOK {
		t.Fatalf("--help exit = %d, want 0 (err: %v)", exitCode(err), err)
	}
	if !strings.Contains(out, "GPU/fleet capacity") {
		t.Fatalf("help missing synopsis: %q", out)
	}
}
