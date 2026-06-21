package httpapi

import (
	"net/http"
	"time"
)

// Prometheus request-path instrumentation (#24). The middleware here times every
// request and records request count + latency; the inference handlers separately
// meter tokens and the rate limiter meters throttles via s.metrics. All of it is
// nil-safe: with metrics disabled (s.metrics == nil) the middleware still serves
// the request through an identical wrapper but records nothing.

// routeLabels bounds the cardinality of the endpoint label to a fixed allowlist
// of route patterns. net/http exposes no route enumeration, and labeling by the
// raw request path would let a caller mint unbounded series (e.g. distinct
// /v1/sessions/{id} ids), so every known route maps to a stable label and any
// unmatched path collapses to "other". The {id} routes are matched by prefix
// because their trailing segment varies; longest-prefix order is enforced in
// endpointLabel.
var routeLabels = []struct {
	prefix string
	label  string
	// exact requires the path to equal prefix (no trailing segment), used for the
	// collection routes that must not swallow their {id} siblings.
	exact bool
}{
	{prefix: "/v1/chat/completions", label: "/v1/chat/completions", exact: true},
	{prefix: "/v1/completions", label: "/v1/completions", exact: true},
	{prefix: "/v1/models", label: "/v1/models", exact: true},
	{prefix: "/models", label: "/models", exact: true},
	// Admin sub-resources before their collection so the more specific label wins.
	{prefix: "/v1/admin/keys/", label: "/v1/admin/keys/{id}"},
	{prefix: "/v1/admin/keys", label: "/v1/admin/keys", exact: true},
	{prefix: "/v1/admin/workers/", label: "/v1/admin/workers/{id}"},
	{prefix: "/v1/admin/workers", label: "/v1/admin/workers", exact: true},
	{prefix: "/v1/admin/stats", label: "/v1/admin/stats", exact: true},
	{prefix: "/v1/sessions/", label: "/v1/sessions/{id}"},
	{prefix: "/v1/sessions", label: "/v1/sessions", exact: true},
}

// endpointLabel maps a request path to its bounded route label, returning "other"
// for anything not on the allowlist so an arbitrary URL can never inflate
// cardinality. The allowlist is ordered most-specific-first so a {id} route is
// preferred over its collection prefix.
func endpointLabel(path string) string {
	for _, r := range routeLabels {
		if r.exact {
			if path == r.prefix {
				return r.label
			}
			continue
		}
		if len(path) > len(r.prefix) && path[:len(r.prefix)] == r.prefix {
			return r.label
		}
	}
	return "other"
}

// statusRecorder wraps an http.ResponseWriter to capture the response status
// code for the requests_total{code} label. It defaults to 200 because a handler
// that writes a body without an explicit WriteHeader implies 200 (net/http does
// the same).
//
// CRITICAL: it forwards http.Flusher so the streaming inference endpoints (SSE)
// keep flushing each frame to the client. beginSSE type-asserts the writer to
// http.Flusher; if this wrapper hid Flush, streaming would silently break (the
// handler would treat the writer as non-streamable and 500, or buffer the whole
// response). It also forwards Unwrap so http.ResponseController and any other
// optional-interface probing reach the underlying writer. wroteHeader guards
// against a double WriteHeader (the standard library logs a warning on that).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader records the status the first time it is set and forwards it.
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write records an implicit 200 (a body written with no prior WriteHeader) and
// forwards the bytes, mirroring net/http's own implicit-200 behavior.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's Flusher so SSE frames are pushed to
// the client immediately. If the underlying writer is not a Flusher this is a
// no-op rather than a panic; beginSSE separately checks for the interface, but
// statusRecorder always advertises Flush so the inference path's
// w.(http.Flusher) assertion succeeds through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the wrapped writer so http.ResponseController (Go 1.20+) and any
// other interface-probing helper can reach the original writer's capabilities.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// responseStarted reports whether a status line / body has already been written.
// recoverMiddleware uses it (through the responseStarter interface) to decide
// whether a panic recovered mid-request can still be turned into a 500 JSON
// envelope: once the response has started (e.g. a streaming SSE handler that
// panicked after the first frame) only logging is possible, since the status line
// is already on the wire.
func (r *statusRecorder) responseStarted() bool { return r.wroteHeader }

// recordTokens meters one completed inference's tokens against
// tokens_generated_total{model,kind} (#24). It mirrors usageFrom's accounting so
// the metric agrees with the usage object the client sees: when the backend
// reports no prompt/completion split (sum == 0) the reported total is surfaced
// as completion tokens (prompt 0), matching the documented fallback. No-op when
// metrics are disabled (s.metrics == nil, handled by AddTokens).
func (s *Server) recordTokens(model string, prompt, completion, total uint64) {
	if prompt+completion == 0 {
		// Same fallback as usageFrom: a total-only backend's work is metered as
		// completion tokens so usage and the metric do not disagree.
		s.metrics.AddTokens(model, 0, total)
		return
	}
	s.metrics.AddTokens(model, prompt, completion)
}

// metricsMiddleware times the request and records requests_total +
// request_duration_seconds via s.metrics, labeled by the bounded route label and
// method. It is the OUTERMOST middleware (see Handler) so it captures the final
// status of even a response short-circuited by an inner layer (a 401/429/…). It
// always wraps the writer in a statusRecorder — which forwards Flush so SSE keeps
// working — even when metrics are disabled, so the request path is uniform; the
// recording itself is a no-op on a nil *metrics.Metrics.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.metrics.ObserveRequest(endpointLabel(r.URL.Path), r.Method, rec.status, time.Since(start).Seconds())
	})
}
