package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestAuthMiddlewareStashesKey documents the reuse contract for #13: behind
// authMiddleware, a handler always finds the authenticated store.APIKey on the
// request context (so chat/completions can authorize + meter per key without
// re-authenticating). This is a unit test of the middleware in isolation.
func TestAuthMiddlewareStashesKey(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{onlineWorker("w1", types.Model{Name: "llama3"})}}
	s, authSvc := testServer(t, fleet)
	token := mustKey(t, authSvc, adminPerms())

	var seen store.APIKey
	var ok bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = keyFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.authMiddleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !ok {
		t.Fatal("handler found no key on context")
	}
	if seen.ID == "" {
		t.Fatal("stashed key has empty ID")
	}
}

// TestSanitizeRequestID exercises the inbound-correlation-id allow-list directly:
// a clean id is accepted verbatim, while empty, over-long, and
// injection/metacharacter-bearing values are rejected (returning "" so the
// middleware mints a fresh id). This is the defense that keeps an
// attacker-controlled X-Request-Id out of the logs and the echoed header.
func TestSanitizeRequestID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"req-0123456789abcdef", "req-0123456789abcdef"}, // a minted-shaped id
		{"trace.abc_123-XYZ", "trace.abc_123-XYZ"},       // unreserved set
		{"", ""},                // empty → mint
		{"has space", ""},       // space rejected
		{"semi;colon", ""},      // metachar rejected
		{"slash/path", ""},      // slash rejected
		{"newline\ninject", ""}, // CR/LF rejected
		{"tab\tinject", ""},     // control char rejected
		{strings.Repeat("a", maxRequestIDLen), strings.Repeat("a", maxRequestIDLen)}, // at the bound
		{strings.Repeat("a", maxRequestIDLen+1), ""},                                 // over the bound → reject
	}
	for _, tc := range cases {
		if got := sanitizeRequestID(tc.in); got != tc.want {
			t.Errorf("sanitizeRequestID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRequestIDMiddlewareMintsAndStashes proves the middleware mints a request
// id when none is supplied, echoes it on the response header, and stashes both
// the id and a request-scoped logger (bound with request_id) on the context.
func TestRequestIDMiddlewareMintsAndStashes(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{onlineWorker("w1", types.Model{Name: "llama3"})}}
	s, _ := testServer(t, fleet)

	var gotID string
	var hadLogger bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, _ = requestIDFromContext(r.Context())
		// reqLog must return a non-nil logger distinct from the bare server logger
		// path (it is s.log.With("request_id", id)); we can at least assert it is
		// non-nil and that the id is present.
		hadLogger = s.reqLog(r.Context()) != nil
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	s.requestIDMiddleware(next).ServeHTTP(rec, req)

	if gotID == "" {
		t.Fatal("middleware did not stash a request id on the context")
	}
	if !strings.HasPrefix(gotID, "req-") {
		t.Errorf("minted id = %q, want req- prefix", gotID)
	}
	if echoed := rec.Header().Get("X-Request-Id"); echoed != gotID {
		t.Errorf("echoed header = %q, want stashed id %q", echoed, gotID)
	}
	if !hadLogger {
		t.Error("reqLog returned nil inside the middleware chain")
	}
}

// TestRequestIDMiddlewareHonorsCleanInbound proves a clean inbound X-Request-Id
// is reused (not replaced) so a caller can supply its own trace id.
func TestRequestIDMiddlewareHonorsCleanInbound(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{onlineWorker("w1", types.Model{Name: "llama3"})}}
	s, _ := testServer(t, fleet)

	const inbound = "trace-clean-123"
	var gotID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, _ = requestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-Request-Id", inbound)
	rec := httptest.NewRecorder()
	s.requestIDMiddleware(next).ServeHTTP(rec, req)

	if gotID != inbound {
		t.Errorf("stashed id = %q, want inbound %q", gotID, inbound)
	}
	if rec.Header().Get("X-Request-Id") != inbound {
		t.Errorf("echoed header = %q, want inbound %q", rec.Header().Get("X-Request-Id"), inbound)
	}
}

// TestReqLogFallsBackToServerLogger proves reqLog returns the server's base
// logger when no request-scoped logger is on the context (a handler exercised
// outside the middleware chain), so logging never panics.
func TestReqLogFallsBackToServerLogger(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{onlineWorker("w1", types.Model{Name: "llama3"})}}
	s, _ := testServer(t, fleet)
	if got := s.reqLog(context.Background()); got != s.log {
		t.Errorf("reqLog with no scoped logger = %p, want server base logger %p", got, s.log)
	}
}

// TestJobIDForFallsBackToMintedID proves jobIDFor mints a job- id when no
// correlation id is on the request context (the handler-outside-middleware case),
// so a job is always uniquely identified. When a correlation id IS present it is
// reused verbatim (request_id == job_id).
func TestJobIDForFallsBackToMintedID(t *testing.T) {
	// No request id on the context → minted job- id.
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
	if got := jobIDFor(req); !strings.HasPrefix(got, "job-") {
		t.Errorf("jobIDFor without correlation id = %q, want job- prefix", got)
	}

	// Correlation id present → reused as the job id.
	ctx := context.WithValue(req.Context(), requestIDContextKey, "req-deadbeef")
	if got := jobIDFor(req.WithContext(ctx)); got != "req-deadbeef" {
		t.Errorf("jobIDFor with correlation id = %q, want it reused verbatim", got)
	}
}

// TestBearerToken exercises header parsing directly: scheme case-insensitivity,
// missing/empty/malformed headers.
func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
		wantOK bool
	}{
		{"Bearer abc123", "abc123", true},
		{"bearer abc123", "abc123", true},
		{"BEARER abc123", "abc123", true},
		{"Bearer   spaced  ", "spaced", true},
		{"", "", false},
		{"Token abc", "", false},
		{"Bearer ", "", false},
		{"Bearer", "", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		got, ok := bearerToken(req)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("bearerToken(%q) = (%q,%v), want (%q,%v)", tc.header, got, ok, tc.want, tc.wantOK)
		}
	}
}
