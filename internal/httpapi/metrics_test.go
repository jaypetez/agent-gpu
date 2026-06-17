package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/metrics"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// fakeEngine is a minimal inferenceEngine for the metrics request-path tests: it
// returns a fixed result (with a token split) and a one-chunk stream, so the
// chat/completions handlers run end-to-end through the middleware without a
// control plane. The streaming chunk sets Done so the terminal-chunk token
// metering fires.
type fakeEngine struct {
	res types.JobResult
}

func (e *fakeEngine) SubmitAuthorizedJob(context.Context, store.APIKey, types.Job) (types.JobResult, error) {
	return e.res, nil
}

func (e *fakeEngine) SubmitAuthorizedJobStream(_ context.Context, _ store.APIKey, job types.Job) (<-chan types.JobChunk, error) {
	ch := make(chan types.JobChunk, 1)
	ch <- types.JobChunk{
		JobID:            job.ID,
		Delta:            e.res.Output,
		Done:             true,
		FinishReason:     "stop",
		PromptTokens:     e.res.PromptTokens,
		CompletionTokens: e.res.CompletionTokens,
		Tokens:           e.res.Tokens,
	}
	close(ch)
	return ch, nil
}

// metricsTestServer builds an httpapi.Server wired to a real auth service over an
// in-memory store, the given fake engine, and a fresh *metrics.Metrics, returning
// the server, the metrics instrument, and an admin token. It is the internal-
// package analog of testServer for the request-path metrics assertions.
func metricsTestServer(t *testing.T, eng inferenceEngine) (*Server, *metrics.Metrics, string) {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	m := metrics.New()
	s := &Server{
		fleet:   &fakeFleet{},
		engine:  eng,
		auth:    authSvc,
		authz:   az,
		metrics: m,
		log:     discard,
	}
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return s, m, token
}

// flushRecorder is an http.ResponseWriter that records whether Flush was called,
// so a test can prove Flush forwards through the statusRecorder wrapper.
type flushRecorder struct {
	httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

// TestStatusRecorderForwarding proves the metrics writer wrapper (a) records the
// status code (explicit and implicit-200), (b) forwards Flush so SSE keeps
// working, and (c) Unwraps to the underlying writer so http.ResponseController
// reaches its capabilities.
func TestStatusRecorderForwarding(t *testing.T) {
	t.Run("explicit WriteHeader recorded", func(t *testing.T) {
		base := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: base}
		rec.WriteHeader(http.StatusTeapot)
		rec.WriteHeader(http.StatusOK) // second call must not overwrite the first
		if rec.status != http.StatusTeapot {
			t.Errorf("status = %d, want 418 (first WriteHeader wins)", rec.status)
		}
	})
	t.Run("implicit 200 on first Write", func(t *testing.T) {
		base := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: base}
		if _, err := rec.Write([]byte("hi")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if rec.status != http.StatusOK {
			t.Errorf("status = %d, want 200 (implicit on Write)", rec.status)
		}
	})
	t.Run("Flush forwards", func(t *testing.T) {
		fr := &flushRecorder{}
		rec := &statusRecorder{ResponseWriter: fr}
		// Reachable both directly and through http.ResponseController (which uses
		// Unwrap to find the underlying Flusher).
		rec.Flush()
		if !fr.flushed {
			t.Error("direct Flush did not forward to the underlying writer")
		}
		fr.flushed = false
		if err := http.NewResponseController(rec).Flush(); err != nil {
			t.Fatalf("ResponseController.Flush: %v", err)
		}
		if !fr.flushed {
			t.Error("ResponseController.Flush did not reach the underlying Flusher via Unwrap")
		}
	})
	t.Run("Unwrap returns the underlying writer", func(t *testing.T) {
		base := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: base}
		if rec.Unwrap() != base {
			t.Error("Unwrap did not return the wrapped writer")
		}
	})
}

// TestRecordTokensTotalOnlyFallback proves the total-only fallback meters the
// reported total as completion tokens (prompt 0), matching usageFrom so the
// metric and the usage object agree.
func TestRecordTokensTotalOnlyFallback(t *testing.T) {
	s, m, _ := metricsTestServer(t, &fakeEngine{})
	s.recordTokens("echo", 0, 0, 12) // no split reported
	if got := tokenCount(t, m, "echo", "completion"); got != 12 {
		t.Errorf("tokens{echo,completion} = %v, want 12 (total-only fallback)", got)
	}
}

// TestEndpointLabelBounded asserts the endpoint label collapses to a fixed
// allowlist (so cardinality stays bounded) and maps unknown paths to "other".
func TestEndpointLabelBounded(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/completions", "/v1/completions"},
		{"/v1/models", "/v1/models"},
		{"/models", "/models"},
		{"/v1/admin/keys", "/v1/admin/keys"},
		{"/v1/admin/keys/abc123", "/v1/admin/keys/{id}"},
		{"/v1/admin/keys/abc123/quota", "/v1/admin/keys/{id}"},
		{"/v1/admin/workers", "/v1/admin/workers"},
		{"/v1/admin/workers/w1/drain", "/v1/admin/workers/{id}"},
		{"/v1/admin/stats", "/v1/admin/stats"},
		{"/v1/sessions", "/v1/sessions"},
		{"/v1/sessions/sess-1", "/v1/sessions/{id}"},
		{"/v1/unknown", "other"},
		{"/", "other"},
		{"/metrics", "other"},
	}
	for _, c := range cases {
		if got := endpointLabel(c.path); got != c.want {
			t.Errorf("endpointLabel(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestMetricsMiddlewareRecordsRequest drives an authenticated request through the
// full handler chain and asserts requests_total and the duration histogram were
// recorded with the bounded endpoint label and the response code.
func TestMetricsMiddlewareRecordsRequest(t *testing.T) {
	s, m, token := metricsTestServer(t, &fakeEngine{})

	// A models discovery GET (200) and an unauthenticated GET (401) so two
	// distinct code labels are exercised.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	unauth := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	urec := httptest.NewRecorder()
	s.Handler().ServeHTTP(urec, unauth)
	if urec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", urec.Code)
	}

	if got := requestCount(t, m, "/v1/models", "GET", "200"); got != 1 {
		t.Errorf("requests_total{/v1/models,GET,200} = %v, want 1", got)
	}
	if got := requestCount(t, m, "/v1/models", "GET", "401"); got != 1 {
		t.Errorf("requests_total{/v1/models,GET,401} = %v, want 1", got)
	}
	if got := durationCount(t, m, "/v1/models", "GET"); got != 2 {
		t.Errorf("request_duration_seconds count{/v1/models,GET} = %v, want 2", got)
	}
}

// TestMetricsTokensNonStreaming asserts a non-streaming chat completion meters
// the token split against tokens_generated_total{model,kind}.
func TestMetricsTokensNonStreaming(t *testing.T) {
	s, m, token := metricsTestServer(t, &fakeEngine{res: types.JobResult{
		Output: "hi", PromptTokens: 7, CompletionTokens: 3, Tokens: 10, FinishReason: "stop",
	}})

	post(t, s, token, "/v1/chat/completions", `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`, http.StatusOK)

	if got := tokenCount(t, m, "llama3", "prompt"); got != 7 {
		t.Errorf("tokens{llama3,prompt} = %v, want 7", got)
	}
	if got := tokenCount(t, m, "llama3", "completion"); got != 3 {
		t.Errorf("tokens{llama3,completion} = %v, want 3", got)
	}
}

// TestMetricsTokensStreamingFlushes asserts a streaming chat completion (a) still
// flushes SSE frames through the statusRecorder wrapper — the critical Flusher
// forwarding — and (b) meters the terminal-chunk tokens. The httptest recorder
// implements http.Flusher and records Flushed, so a working stream sets it.
func TestMetricsTokensStreamingFlushes(t *testing.T) {
	s, m, token := metricsTestServer(t, &fakeEngine{res: types.JobResult{
		Output: "hi", PromptTokens: 4, CompletionTokens: 6, Tokens: 10, FinishReason: "stop",
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		readerOf(`{"model":"llama3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The SSE handler flushes each frame; a wrapper that hid Flush would break
	// streaming. The recorder records the last Flush.
	if !rec.Flushed {
		t.Errorf("streaming response was not flushed through the metrics wrapper (Flusher lost)")
	}
	if body := rec.Body.String(); !containsAll(body, "data: ", "[DONE]") {
		t.Errorf("stream body missing SSE framing: %q", body)
	}
	// Terminal-chunk tokens are metered for a cleanly-terminated stream.
	if got := tokenCount(t, m, "llama3", "completion"); got != 6 {
		t.Errorf("stream tokens{llama3,completion} = %v, want 6", got)
	}
	if got := tokenCount(t, m, "llama3", "prompt"); got != 4 {
		t.Errorf("stream tokens{llama3,prompt} = %v, want 4", got)
	}
}

// ---- small assertion helpers reading the gathered registry ----

// metricValue gathers m's registry and returns the value of the sample named
// `name` whose labels exactly match `labels`, or fails if absent. It reads
// through the public Registry() so the tests assert on the real exposition the
// scraper sees rather than poking unexported collectors.
func metricValue(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if !labelsEqual(met.GetLabel(), labels) {
				continue
			}
			switch {
			case met.GetCounter() != nil:
				return met.GetCounter().GetValue()
			case met.GetGauge() != nil:
				return met.GetGauge().GetValue()
			case met.GetHistogram() != nil:
				return float64(met.GetHistogram().GetSampleCount())
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found in exposition", name, labels)
	return 0
}

func labelsEqual(have []*dto.LabelPair, want map[string]string) bool {
	if len(have) != len(want) {
		return false
	}
	for _, l := range have {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

func requestCount(t *testing.T, m *metrics.Metrics, endpoint, method, code string) float64 {
	t.Helper()
	return metricValue(t, m, "agentgpu_requests_total", map[string]string{"endpoint": endpoint, "method": method, "code": code})
}

func durationCount(t *testing.T, m *metrics.Metrics, endpoint, method string) float64 {
	t.Helper()
	return metricValue(t, m, "agentgpu_request_duration_seconds", map[string]string{"endpoint": endpoint, "method": method})
}

func tokenCount(t *testing.T, m *metrics.Metrics, model, kind string) float64 {
	t.Helper()
	return metricValue(t, m, "agentgpu_tokens_generated_total", map[string]string{"model": model, "kind": kind})
}

func post(t *testing.T, s *Server, token, path, body string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, readerOf(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("POST %s status = %d, want %d; body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

func readerOf(s string) *stringReader { return &stringReader{s: s} }

// stringReader is a tiny io.Reader over a string (avoids importing strings just
// for NewReader in this file).
type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
