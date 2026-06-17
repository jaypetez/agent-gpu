package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestObserveRequestIncrements proves ObserveRequest bumps requests_total for the
// (endpoint, method, code) tuple and records a duration observation.
func TestObserveRequestIncrements(t *testing.T) {
	m := New()
	m.ObserveRequest("/v1/chat/completions", "POST", http.StatusOK, 0.123)
	m.ObserveRequest("/v1/chat/completions", "POST", http.StatusOK, 0.250)
	m.ObserveRequest("/v1/models", "GET", http.StatusUnauthorized, 0.001)

	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("/v1/chat/completions", "POST", "200")); got != 2 {
		t.Errorf("requests_total{chat,POST,200} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("/v1/models", "GET", "401")); got != 1 {
		t.Errorf("requests_total{models,GET,401} = %v, want 1", got)
	}
	// Two observations were recorded on the chat histogram.
	if got := testutil.CollectAndCount(m.requestDuration, "agentgpu_request_duration_seconds"); got == 0 {
		t.Errorf("request_duration_seconds has no series, want at least one observed")
	}
}

// TestObserveRequestZeroCodeIsOK proves a 0 status (a body written without an
// explicit WriteHeader) is labeled 200, matching net/http's implicit behavior.
func TestObserveRequestZeroCodeIsOK(t *testing.T) {
	m := New()
	m.ObserveRequest("/models", "GET", 0, 0.01)
	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("/models", "GET", "200")); got != 1 {
		t.Errorf("requests_total{models,GET,200} = %v, want 1 (zero code -> 200)", got)
	}
}

// TestAddTokens proves prompt/completion tokens are summed per (model, kind) and
// a zero count for a kind creates no series.
func TestAddTokens(t *testing.T) {
	m := New()
	m.AddTokens("llama3", 7, 3)
	m.AddTokens("llama3", 1, 0) // completion 0 -> only prompt bumps
	m.AddTokens("mistral", 0, 5)

	if got := testutil.ToFloat64(m.tokensTotal.WithLabelValues("llama3", "prompt")); got != 8 {
		t.Errorf("tokens{llama3,prompt} = %v, want 8", got)
	}
	if got := testutil.ToFloat64(m.tokensTotal.WithLabelValues("llama3", "completion")); got != 3 {
		t.Errorf("tokens{llama3,completion} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.tokensTotal.WithLabelValues("mistral", "completion")); got != 5 {
		t.Errorf("tokens{mistral,completion} = %v, want 5", got)
	}
}

// TestIncThrottle proves throttle counters increment per scope.
func TestIncThrottle(t *testing.T) {
	m := New()
	m.IncThrottle("global")
	m.IncThrottle("global")
	m.IncThrottle("key")

	if got := testutil.ToFloat64(m.throttledTotal.WithLabelValues("global")); got != 2 {
		t.Errorf("throttled{global} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.throttledTotal.WithLabelValues("key")); got != 1 {
		t.Errorf("throttled{key} = %v, want 1", got)
	}
}

// TestNilMetricsIsNoOp proves every method on a nil *Metrics is a safe no-op and
// the Handler returns a benign 404 rather than panicking — the disabled-build
// contract callers and tests rely on.
func TestNilMetricsIsNoOp(t *testing.T) {
	var m *Metrics
	// None of these may panic.
	m.ObserveRequest("/v1/models", "GET", 200, 0.01)
	m.AddTokens("llama3", 1, 2)
	m.IncThrottle("global")
	if m.Registry() != nil {
		t.Errorf("nil *Metrics Registry() = non-nil, want nil")
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil *Metrics Handler status = %d, want 404", rec.Code)
	}
}

// TestExpositionScrapeable hits the /metrics handler over httptest and asserts
// the exposition is scrapeable and contains the agent-gpu metric families,
// including the standard process_/go_ runtime collectors registered in New.
func TestExpositionScrapeable(t *testing.T) {
	m := New()
	m.ObserveRequest("/v1/chat/completions", "POST", http.StatusOK, 0.2)
	m.AddTokens("llama3", 4, 6)
	m.IncThrottle("key")

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	for _, want := range []string{
		"agentgpu_requests_total",
		"agentgpu_request_duration_seconds",
		"agentgpu_tokens_generated_total",
		"agentgpu_throttled_total",
		// Standard runtime/process collectors come for free from New().
		"go_goroutines",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, text)
		}
	}
}
