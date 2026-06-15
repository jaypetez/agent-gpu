package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// quotaHarness wires a server with a real quota engine (clock-injected) to a
// live echo worker, plus an auth.Service, so a test can run the full
// authenticate -> authorize -> quota reserve -> dispatch -> record-tokens flow.
type quotaHarness struct {
	h   *harness
	svc *auth.Service
	cs  *quota.MemoryCounterStore
	eng *quota.Engine
}

func newQuotaHarness(t *testing.T, now func() time.Time, defaults quota.Limits) *quotaHarness {
	t.Helper()
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })

	cs := quota.NewMemoryCounterStore()
	eng := quota.NewEngine(cs, quota.WithClock(now), quota.WithDefaults(defaults))

	az := authz.NewAuthorizer()
	h := &harness{t: t}
	h.srv = server.New(server.WithAuthorizer(az), server.WithQuota(eng))
	h.start()
	t.Cleanup(h.close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := newWorker(h, nil)
	go func() { _ = w.Run(ctx) }()
	waitFor(t, 2*time.Second, "worker to register", func() bool { return h.srv.WorkerCount() == 1 })

	return &quotaHarness{h: h, svc: auth.NewService(st), cs: cs, eng: eng}
}

// authedKeyWithLimits creates a permitted key with the supplied per-key limits
// and returns it freshly authenticated.
func (q *quotaHarness) authedKeyWithLimits(t *testing.T, lim *store.Limits) store.APIKey {
	t.Helper()
	ctx := context.Background()
	token, created, err := q.svc.CreateWithPermissions(ctx, "agent",
		auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{"llama3"}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if lim != nil {
		if _, err := q.svc.SetLimits(ctx, created.ID, lim); err != nil {
			t.Fatalf("set limits: %v", err)
		}
	}
	key, err := q.svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return key
}

// TestQuotaRPMBlocksDispatch covers AC1: once RPM is hit, further dispatch is
// blocked with ErrQuotaExceeded, before any job reaches the worker.
func TestQuotaRPMBlocksDispatch(t *testing.T) {
	clk := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	h := newQuotaHarness(t, now, quota.Limits{})
	key := h.authedKeyWithLimits(t, &store.Limits{RPM: 2})

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := h.h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "ok", Model: "llama3", Prompt: "ping"}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if _, err := h.h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "blocked", Model: "llama3", Prompt: "ping"}); !errors.Is(err, quota.ErrQuotaExceeded) {
		t.Fatalf("over-limit request: want ErrQuotaExceeded, got %v", err)
	}
}

// TestQuotaTokenAccounting covers AC2: the tokens recorded match what the echo
// executor actually produced (whitespace token count of "echo: <prompt>").
func TestQuotaTokenAccounting(t *testing.T) {
	clk := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	h := newQuotaHarness(t, now, quota.Limits{})
	key := h.authedKeyWithLimits(t, &store.Limits{RPM: 100, DailyTokens: 1000})

	ctx := context.Background()
	// "echo: alpha beta gamma" -> 4 whitespace tokens.
	res, err := h.h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "j", Model: "llama3", Prompt: "alpha beta gamma"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Tokens != 4 {
		t.Fatalf("result tokens = %d, want 4", res.Tokens)
	}
	snap, err := h.eng.UsageForKey(ctx, key)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if snap.TokensToday != 4 {
		t.Fatalf("recorded TokensToday = %d, want 4 (must match generated tokens)", snap.TokensToday)
	}
}

// TestQuotaTokenBudgetBlocksOnceExhausted covers AC1/AC2: a daily token budget
// blocks the next request once the recorded tokens reach the limit.
func TestQuotaTokenBudgetBlocksOnceExhausted(t *testing.T) {
	clk := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	h := newQuotaHarness(t, now, quota.Limits{})
	// "echo: ping" -> 2 tokens; a 2-token daily budget is exhausted after one job.
	key := h.authedKeyWithLimits(t, &store.Limits{RPM: 100, DailyTokens: 2})

	ctx := context.Background()
	if _, err := h.h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "first", Model: "llama3", Prompt: "ping"}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := h.h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "second", Model: "llama3", Prompt: "ping"}); !errors.Is(err, quota.ErrQuotaExceeded) {
		t.Fatalf("second request: want ErrQuotaExceeded (daily budget exhausted), got %v", err)
	}
}

// TestQuotaDefaultNoOpEngine covers the seam: with no WithQuota option the
// server uses an unlimited engine so dispatch is unrestricted.
func TestQuotaDefaultNoOpEngine(t *testing.T) {
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t}
	h.srv = server.New() // no quota option
	h.start()
	t.Cleanup(h.close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := newWorker(h, nil)
	go func() { _ = w.Run(ctx) }()
	waitFor(t, 2*time.Second, "worker to register", func() bool { return h.srv.WorkerCount() == 1 })

	svc := auth.NewService(st)
	token, _, err := svc.CreateWithPermissions(ctx, "agent",
		auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{"llama3"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	key, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := h.srv.SubmitAuthorizedJob(ctx, key, types.Job{ID: "j", Model: "llama3", Prompt: "ping"}); err != nil {
			t.Fatalf("request %d under no-op quota: %v", i, err)
		}
	}
}
