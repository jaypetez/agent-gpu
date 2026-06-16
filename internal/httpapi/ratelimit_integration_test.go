package httpapi_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// retryAfterSeconds reads and parses the Retry-After header, failing the test
// when it is missing or not a positive integer (the #6 contract: integer
// seconds, minimum 1).
func retryAfterSeconds(t *testing.T, resp *http.Response) int {
	t.Helper()
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		t.Fatalf("Retry-After header missing on 429")
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer: %v", raw, err)
	}
	if n < 1 {
		t.Fatalf("Retry-After = %d, want >= 1", n)
	}
	return n
}

// mintUserKey creates a fresh user key permitted for the given model, optionally
// with an RPM cap, returning its bearer token.
func mintUserKey(t *testing.T, authSvc *auth.Service, model string, rpm uint64) string {
	t.Helper()
	ctx := context.Background()
	token, created, err := authSvc.CreateWithPermissions(ctx, "user",
		auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{model}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if rpm != 0 {
		if _, err := authSvc.SetLimits(ctx, created.ID, &store.Limits{RPM: rpm}); err != nil {
			t.Fatalf("set limits: %v", err)
		}
	}
	return token
}

// TestPerKeyRateLimitRetryAfter proves a per-key over-limit request returns 429
// with a numeric Retry-After header, the KeyThrottled metric increments, and
// advancing the injected clock past the minute window lets a request succeed
// again. (AC1, AC4 per-key.)
func TestPerKeyRateLimitRetryAfter(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithClock(clk.nowFn))

	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarnessWith(t, exec, "llama3", server.WithQuota(eng))

	token := mintUserKey(t, h.authSvc, "llama3", 1)
	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// First request fits under RPM=1.
	resp := h.postAs(t, token, "/v1/chat/completions", body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("request 1: status = %d, want 200", status)
	}

	// Second trips the per-key limit: 429 + numeric Retry-After (the minute
	// window resets within 60s).
	resp = h.postAs(t, token, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusTooManyRequests {
		_ = resp.Body.Close()
		t.Fatalf("over-limit request: status = %d, want 429", resp.StatusCode)
	}
	ra := retryAfterSeconds(t, resp)
	_ = resp.Body.Close()
	if ra > 60 {
		t.Errorf("Retry-After = %d, want <= 60 (minute window)", ra)
	}

	if got := h.httpSrv.RateLimitStats(); got.KeyThrottled != 1 {
		t.Errorf("KeyThrottled = %d, want 1", got.KeyThrottled)
	}
	// The global limiter is off in this test, so it must not have throttled.
	if got := h.httpSrv.RateLimitStats(); got.GlobalThrottled != 0 {
		t.Errorf("GlobalThrottled = %d, want 0 (no global limit configured)", got.GlobalThrottled)
	}

	// Advance past the minute window: the rolling RPM resets.
	clk.advance(time.Minute)
	resp = h.postAs(t, token, "/v1/chat/completions", body)
	status = resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("after window reset: status = %d, want 200", status)
	}
}

// TestGlobalRateLimitRetryAfter proves the global limiter throttles independent
// of per-key quota: with GlobalRPM=N and distinct fresh keys each well under
// their own quota, the (N+1)th request still returns 429 with a Retry-After, and
// GlobalThrottled (not KeyThrottled) increments — proving the denial is the
// server-wide limit, not any individual key's. Advancing the clock past the
// minute window then admits a request again. (AC2, AC4 global.)
func TestGlobalRateLimitRetryAfter(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
	const globalRPM = 2
	eng := quota.NewEngine(quota.NewMemoryCounterStore(),
		quota.WithClock(clk.nowFn),
		quota.WithGlobalLimits(globalRPM, 0),
	)

	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarnessWith(t, exec, "llama3", server.WithQuota(eng))

	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// N requests, each on a DISTINCT fresh key with no per-key cap (so any denial
	// can only be the global limit). All N fit under GlobalRPM=N.
	for i := 0; i < globalRPM; i++ {
		token := mintUserKey(t, h.authSvc, "llama3", 0)
		resp := h.postAs(t, token, "/v1/chat/completions", body)
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusOK {
			t.Fatalf("global request %d: status = %d, want 200", i+1, status)
		}
	}

	// The (N+1)th, on yet another fresh under-quota key, trips the GLOBAL limit.
	token := mintUserKey(t, h.authSvc, "llama3", 0)
	resp := h.postAs(t, token, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusTooManyRequests {
		_ = resp.Body.Close()
		t.Fatalf("over-global request: status = %d, want 429", resp.StatusCode)
	}
	ra := retryAfterSeconds(t, resp)
	_ = resp.Body.Close()
	if ra > 60 {
		t.Errorf("Retry-After = %d, want <= 60 (global minute window)", ra)
	}

	stats := h.httpSrv.RateLimitStats()
	if stats.GlobalThrottled != 1 {
		t.Errorf("GlobalThrottled = %d, want 1 (proves global, not per-key)", stats.GlobalThrottled)
	}
	if stats.KeyThrottled != 0 {
		t.Errorf("KeyThrottled = %d, want 0 (the key was under its own quota)", stats.KeyThrottled)
	}

	// Cross the minute boundary: the global window resets and a fresh key is
	// admitted again.
	clk.advance(time.Minute)
	token = mintUserKey(t, h.authSvc, "llama3", 0)
	resp = h.postAs(t, token, "/v1/chat/completions", body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("after global window reset: status = %d, want 200", status)
	}
}

// TestNoGlobalLimitUnthrottled proves the default (no global limit configured)
// leaves the request path unthrottled: many requests on a single uncapped key
// all succeed, and neither throttle counter moves. (AC3 default behavior.)
func TestNoGlobalLimitUnthrottled(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithClock(clk.nowFn))

	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarnessWith(t, exec, "llama3", server.WithQuota(eng))

	token := mintUserKey(t, h.authSvc, "llama3", 0)
	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 25; i++ {
		resp := h.postAs(t, token, "/v1/chat/completions", body)
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (no limits configured)", i+1, status)
		}
	}

	if got := h.httpSrv.RateLimitStats(); got.GlobalThrottled != 0 || got.KeyThrottled != 0 {
		t.Errorf("RateLimitStats = %+v, want all zero", got)
	}
}
