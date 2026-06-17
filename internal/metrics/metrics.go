// Package metrics is the Prometheus instrumentation layer for agent-gpu (#24): a
// self-contained registry, a small set of bounded-cardinality collectors, and an
// exposition handler the server mounts on a dedicated /metrics listener.
//
// # What it exposes
//
// Two kinds of metrics live here:
//
//   - Request-path collectors, owned by the *Metrics value and updated inline by
//     the HTTP layer as requests flow: agentgpu_requests_total (a Counter),
//     agentgpu_request_duration_seconds (a Histogram), agentgpu_tokens_generated_total
//     (a Counter), and agentgpu_throttled_total (a Counter). The httpapi package
//     calls ObserveRequest / AddTokens / IncThrottle on the hot path.
//   - A live custom Collector (see collector.go) that reads the control-plane
//     server's in-memory snapshots at scrape time — queue depth, the time-in-queue
//     histogram, per-worker GPU utilization / VRAM / active jobs / start time, and
//     affinity hit/miss — so those gauges always reflect real state without a
//     background poller.
//
// The Go runtime and process collectors are registered too, so standard
// process_* and go_* metrics come for free.
//
// # Cardinality
//
// Every label set here is deliberately bounded: endpoint (a fixed route
// allowlist), method, code, model, worker, gpu_type, priority, scope, and kind.
// Metrics are NEVER labeled by API key id — per-key data is unbounded and is
// surfaced through the admin/quota API instead (a conscious deviation from the
// issue's "label by key" wording). See docs/metrics.md.
//
// # Nil-safety
//
// A nil *Metrics is a valid, disabled instrument: every method is a no-op, so a
// caller (or a unit test) that does not wire metrics in behaves exactly as before
// this package existed. This mirrors the option-pattern nil tolerance used
// elsewhere in the repo (e.g. a nil quota engine = unlimited).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace prefixes every agent-gpu metric name so they group cleanly under
// agentgpu_* in a shared Prometheus instance and never collide with the runtime
// process_* / go_* families.
const namespace = "agentgpu"

// Metrics owns the Prometheus registry and the request-path collectors the HTTP
// layer updates inline. It is constructed once at startup (New) and threaded into
// httpapi.NewServer; the live server collector is registered separately via
// RegisterServerCollector. A nil *Metrics is a disabled no-op (see package doc).
type Metrics struct {
	reg *prometheus.Registry

	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	tokensTotal     *prometheus.CounterVec
	throttledTotal  *prometheus.CounterVec
}

// durationBuckets are the latency histogram boundaries (seconds). They span the
// sub-millisecond admin/discovery responses through multi-second streamed
// inference completions, so the same histogram gives a usable p50/p95/p99 for
// both fast and slow endpoints. The implicit +Inf bucket catches anything slower.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// New constructs a Metrics with a fresh, isolated registry (not the global
// default, so tests and multiple instances never cross-contaminate), registers
// the request-path collectors plus the Go runtime and process collectors, and
// returns it ready to thread into the HTTP layer. The live server collector is
// registered separately (RegisterServerCollector) once the control-plane server
// exists, avoiding a construction-order cycle between the two.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "requests_total",
			Help:      "Total HTTP API requests handled, labeled by endpoint, method, and response status code.",
		}, []string{"endpoint", "method", "code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "request_duration_seconds",
			Help:      "HTTP API request latency in seconds, labeled by endpoint and method.",
			Buckets:   durationBuckets,
		}, []string{"endpoint", "method"}),
		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tokens_generated_total",
			Help:      "Total tokens accounted on completed inference, labeled by model and kind (prompt|completion).",
		}, []string{"model", "kind"}),
		throttledTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "throttled_total",
			Help:      "Total requests rejected by rate limiting, labeled by scope (global|key).",
		}, []string{"scope"}),
	}
	reg.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.tokensTotal,
		m.throttledTotal,
		// Standard process_* and go_* metrics for free operational visibility.
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	return m
}

// Registry returns the underlying Prometheus registry so the caller can register
// the live server collector (RegisterServerCollector wraps this) or, in tests,
// gather/compare directly. It returns nil for a nil *Metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// Handler returns the /metrics exposition handler for this instrument's registry.
// It is mounted on the dedicated metrics listener (never the API mux) so scraping
// needs no API authentication and cannot drift the OpenAPI route set. For a nil
// *Metrics it returns a 404 handler, so a disabled build still serves a benign
// response rather than panicking.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		// Continue exposing whatever gathered cleanly if one collector errors,
		// so a single faulty metric never blanks the whole scrape.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// ObserveRequest records one completed HTTP request: it bumps requests_total for
// the (endpoint, method, code) tuple and observes the request duration (seconds)
// in request_duration_seconds for (endpoint, method). endpoint must already be a
// bounded route label (see httpapi's route allowlist) so cardinality stays fixed.
// No-op on a nil *Metrics.
func (m *Metrics) ObserveRequest(endpoint, method string, code int, seconds float64) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(endpoint, method, statusLabel(code)).Inc()
	m.requestDuration.WithLabelValues(endpoint, method).Observe(seconds)
}

// AddTokens records the prompt and completion tokens of one completed inference
// against tokens_generated_total{model,kind}. A zero count for a kind is skipped
// so an idle series is not created (e.g. a backend that reports only a total
// surfaces it as completion tokens upstream, so prompt stays zero). No-op on a
// nil *Metrics.
func (m *Metrics) AddTokens(model string, prompt, completion uint64) {
	if m == nil {
		return
	}
	if prompt > 0 {
		m.tokensTotal.WithLabelValues(model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.tokensTotal.WithLabelValues(model, "completion").Add(float64(completion))
	}
}

// IncThrottle increments throttled_total for the given scope ("global" or
// "key"). It is called at the rate-limit rejection site so the metric is a true
// monotonic counter. No-op on a nil *Metrics.
func (m *Metrics) IncThrottle(scope string) {
	if m == nil {
		return
	}
	m.throttledTotal.WithLabelValues(scope).Inc()
}

// statusLabel renders an HTTP status code as its decimal string for the code
// label. The set of codes the API returns is small and bounded (2xx/4xx/5xx the
// handlers emit), so the raw code is a safe, low-cardinality label.
func statusLabel(code int) string {
	// A handler that writes the body without an explicit WriteHeader implies 200.
	if code == 0 {
		code = http.StatusOK
	}
	return itoa(code)
}

// itoa renders a small non-negative int without pulling strconv onto the hot
// path's import surface for a single use; HTTP status codes are always 3 digits.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
