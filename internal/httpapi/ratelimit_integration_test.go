package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/testutil"
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
// with an RPM cap, returning its bearer token. It is a thin alias over
// testutil.MintToken specialized to this suite's "user role + one allowed model"
// shape (rpm == 0 leaves the key uncapped).
func mintUserKey(t *testing.T, authSvc *auth.Service, model string, rpm uint64) string {
	t.Helper()
	opts := []testutil.KeyOption{
		testutil.WithKeyName("user"),
		testutil.WithRoles(authz.RoleUser),
		testutil.WithAllowModels(model),
	}
	if rpm != 0 {
		opts = append(opts, testutil.WithRPM(rpm))
	}
	return testutil.MintToken(t, authSvc, opts...)
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

// TestGlobalTPMRateLimit proves global TPM is enforced end-to-end: with a small
// GlobalTPM and a scripted executor returning a known token count per request,
// the request whose recorded tokens push the global minute-token counter to the
// limit succeeds, and the NEXT request — on a distinct under-quota key, so the
// denial can only be the global limit — returns 429 rate_limit_exceeded with a
// numeric Retry-After. GlobalThrottled increments (proving the global scope) and
// KeyThrottled does not (the keys are well under their own quota). This guards
// the fix for the previously silent global TPM: nothing recorded onto the global
// counter made the configured budget never deny. (AC2 token dimension.)
func TestGlobalTPMRateLimit(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
	// Each request produces promptTokens+completionTokens = 2 tokens. GlobalTPM=2
	// lets the first request through (counter starts at 0 < 2); recording its 2
	// tokens then exhausts the budget so the next request is denied on TPM.
	const globalTPM = 2
	eng := quota.NewEngine(quota.NewMemoryCounterStore(),
		quota.WithClock(clk.nowFn),
		quota.WithGlobalLimits(0, globalTPM),
	)

	exec := &scriptedExecutor{deltas: []string{"ok"}, promptTokens: 1, completionTokens: 1}
	h := newInferenceHarnessWith(t, exec, "llama3", server.WithQuota(eng))

	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`

	// First request: under the token budget (global RPM is unlimited here). Its 2
	// tokens are recorded onto the global counter after the job completes — the
	// non-streaming response only returns post-dispatch, so the budget is updated
	// by the time we issue the next request.
	token := mintUserKey(t, h.authSvc, "llama3", 0)
	resp := h.postAs(t, token, "/v1/chat/completions", body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", status)
	}

	// Next request, on a distinct fresh under-quota key, trips the GLOBAL TPM
	// budget (already exhausted), not any per-key limit.
	token = mintUserKey(t, h.authSvc, "llama3", 0)
	resp = h.postAs(t, token, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusTooManyRequests {
		_ = resp.Body.Close()
		t.Fatalf("over-global-TPM request: status = %d, want 429", resp.StatusCode)
	}
	ra := retryAfterSeconds(t, resp)
	var eb quotaErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode error body: %v", err)
	}
	_ = resp.Body.Close()
	if eb.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded", eb.Error.Code)
	}
	if ra > 60 {
		t.Errorf("Retry-After = %d, want <= 60 (global minute window)", ra)
	}

	stats := h.httpSrv.RateLimitStats()
	if stats.GlobalThrottled != 1 {
		t.Errorf("GlobalThrottled = %d, want 1 (proves global TPM, not per-key)", stats.GlobalThrottled)
	}
	if stats.KeyThrottled != 0 {
		t.Errorf("KeyThrottled = %d, want 0 (the key was under its own quota)", stats.KeyThrottled)
	}

	// Cross the minute boundary: the global token window resets and a fresh key is
	// admitted again, proving the TPM budget is a rolling minute window.
	clk.advance(time.Minute)
	token = mintUserKey(t, h.authSvc, "llama3", 0)
	resp = h.postAs(t, token, "/v1/chat/completions", body)
	status = resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("after global TPM window reset: status = %d, want 200", status)
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
