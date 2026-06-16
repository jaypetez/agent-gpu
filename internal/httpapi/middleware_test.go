package httpapi

import (
	"net/http"
	"net/http/httptest"
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
