package quota

import (
	"context"
	"log/slog"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// Engine enforces per-key quotas over a CounterStore. It is safe for concurrent
// use; the CounterStore serializes the check-and-increment so counts stay exact
// under concurrent requests.
//
// Limit resolution: a key's per-key Limits override the engine's global
// defaults wholesale (a non-nil APIKey.Limits is used as-is; nil falls back to
// defaults). Within the effective limits, a zero field means unlimited.
type Engine struct {
	cs       CounterStore
	defaults Limits
	global   Limits
	now      func() time.Time
	log      *slog.Logger
}

// globalKeyID is the reserved CounterStore key the server-wide (global) limiter
// reserves against. Real key ids are minted as "agpu_" + hex (see auth), so the
// double-underscore sentinel can never collide with a real key's per-key
// counters: the global window is accounted entirely separately from any key.
const globalKeyID = "__global__"

// Option configures an Engine.
type Option func(*Engine)

// WithClock overrides the time source (for tests). All window math uses UTC.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithLogger sets the structured audit logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// WithDefaults sets the global default limits applied to keys whose own Limits
// are nil. The zero Limits value (all unlimited) is the default default.
func WithDefaults(d Limits) Option {
	return func(e *Engine) { e.defaults = d }
}

// WithGlobalLimits sets the server-wide (global) rate limits enforced by
// CheckAndReserveGlobal, independent of any per-key quota. rpm caps total
// requests-per-minute across the whole fleet; tpm caps total tokens-per-minute.
// A zero value for either dimension means UNLIMITED for that dimension (the
// default — global limiting is off unless configured). Only the minute-window
// RPM/TPM dimensions are used; daily/monthly global budgets are not modeled.
func WithGlobalLimits(rpm, tpm uint64) Option {
	return func(e *Engine) { e.global = Limits{RPM: rpm, TPM: tpm} }
}

// NewEngine constructs an Engine over cs. Without WithClock it uses time.Now;
// without WithLogger it audits to slog.Default(); without WithDefaults all
// dimensions are unlimited.
func NewEngine(cs CounterStore, opts ...Option) *Engine {
	e := &Engine{
		cs:  cs,
		now: time.Now,
		log: slog.Default(),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// effectiveLimits returns the limits applied to key: its own Limits if set,
// else the engine defaults.
func (e *Engine) effectiveLimits(key store.APIKey) Limits {
	if key.Limits != nil {
		return *key.Limits
	}
	return e.defaults
}

// CheckAndReserve enforces a key's quota for one inbound request, BEFORE the job
// is dispatched. It rolls any windows the clock has crossed, then denies with
// ErrQuotaExceeded if either:
//
//   - reserving this request would exceed RPM, or
//   - a token budget (TPM, daily, or monthly) is already exhausted,
//
// otherwise it reserves the request (increments the minute request counter) and
// returns nil. Token budgets are checked as "already exhausted" here because the
// token cost of this request is not known until the job returns
// (RecordTokens); the request is admitted and its tokens recorded afterward.
//
// Every decision is audited via slog (key_id, quota_type, usage, limit,
// decision); denials log at Warn. Secrets are never logged.
func (e *Engine) CheckAndReserve(ctx context.Context, key store.APIKey) error {
	lim := e.effectiveLimits(key)
	now := e.now().UTC()

	var (
		denied    bool
		quotaType string
		usage     uint64
		limit     uint64
	)
	err := e.cs.Reserve(ctx, key.ID, now, func(c Counters) error {
		// RPM: the reservation itself must fit.
		if lim.RPM != 0 && c.MinuteRequests+1 > lim.RPM {
			denied, quotaType, usage, limit = true, "rpm", c.MinuteRequests, lim.RPM
			return ErrQuotaExceeded
		}
		// Token budgets: deny only if already exhausted (>= limit), since this
		// request's token cost is recorded post-dispatch.
		if lim.TPM != 0 && c.MinuteTokens >= lim.TPM {
			denied, quotaType, usage, limit = true, "tpm", c.MinuteTokens, lim.TPM
			return ErrQuotaExceeded
		}
		if lim.DailyTokens != 0 && c.DayTokens >= lim.DailyTokens {
			denied, quotaType, usage, limit = true, "daily_tokens", c.DayTokens, lim.DailyTokens
			return ErrQuotaExceeded
		}
		if lim.MonthlyTokens != 0 && c.MonthTokens >= lim.MonthlyTokens {
			denied, quotaType, usage, limit = true, "monthly_tokens", c.MonthTokens, lim.MonthlyTokens
			return ErrQuotaExceeded
		}
		return nil
	})

	if err != nil {
		if denied {
			e.log.Log(ctx, slog.LevelWarn, "quota decision",
				"decision", "denied",
				"key_id", key.ID,
				"quota_type", quotaType,
				"usage", usage,
				"limit", limit,
			)
		}
		return err
	}
	e.log.Log(ctx, slog.LevelDebug, "quota decision",
		"decision", "reserved",
		"key_id", key.ID,
		"quota_type", "rpm",
	)
	return nil
}

// CheckAndReserveGlobal enforces the server-wide (global) rate limit for one
// inbound request, BEFORE the job is dispatched and independent of any per-key
// quota. It reserves against a reserved global counter (globalKeyID), denying
// with ErrQuotaExceeded if reserving this request would exceed the global RPM,
// or if the global token budget (TPM) is already exhausted; otherwise it
// reserves (increments the global minute request counter) and returns nil.
//
// When both global RPM and TPM are zero (unlimited — the default), it
// short-circuits without touching the counter store, so a server without global
// limits behaves exactly as before. The reservation is atomic with the same
// per-key mutex discipline as CheckAndReserve; global accounting never touches a
// real key's counters, so an allowed request is still independently subject to
// its per-key CheckAndReserve.
func (e *Engine) CheckAndReserveGlobal(ctx context.Context) error {
	lim := e.global
	if lim.RPM == 0 && lim.TPM == 0 {
		return nil // global limiting disabled: byte-identical to the pre-#6 path.
	}
	now := e.now().UTC()

	var (
		denied    bool
		quotaType string
		usage     uint64
		limit     uint64
	)
	err := e.cs.Reserve(ctx, globalKeyID, now, func(c Counters) error {
		if lim.RPM != 0 && c.MinuteRequests+1 > lim.RPM {
			denied, quotaType, usage, limit = true, "global_rpm", c.MinuteRequests, lim.RPM
			return ErrQuotaExceeded
		}
		if lim.TPM != 0 && c.MinuteTokens >= lim.TPM {
			denied, quotaType, usage, limit = true, "global_tpm", c.MinuteTokens, lim.TPM
			return ErrQuotaExceeded
		}
		return nil
	})

	if err != nil {
		if denied {
			e.log.Log(ctx, slog.LevelWarn, "quota decision",
				"decision", "denied",
				"key_id", globalKeyID,
				"quota_type", quotaType,
				"usage", usage,
				"limit", limit,
			)
		}
		return err
	}
	e.log.Log(ctx, slog.LevelDebug, "quota decision",
		"decision", "reserved",
		"key_id", globalKeyID,
		"quota_type", "global_rpm",
	)
	return nil
}

// Now returns the engine's current time (UTC) from its injected clock. The
// request path uses it to compute Retry-After against the same time source the
// quota windows reset on, so the hint is deterministic under an injected clock.
func (e *Engine) Now() time.Time { return e.now().UTC() }

// GlobalMinuteReset returns the time at which the global minute window currently
// in effect next resets, computed from the engine clock. It is the seam the
// request path uses to set a Retry-After hint on a global-limit 429.
func (e *Engine) GlobalMinuteReset() time.Time {
	return WindowReset(windowMinute, e.now().UTC())
}

// RecordTokens adds n tokens to the key's TPM/daily/monthly counters, AFTER the
// job returns. n==0 (e.g. a failed job that produced nothing) is a no-op so a
// failed request consumes an RPM unit but no token budget. Rolling expired
// windows happens inside the CounterStore.
func (e *Engine) RecordTokens(ctx context.Context, keyID string, n uint64) {
	if n == 0 {
		return
	}
	now := e.now().UTC()
	if err := e.cs.AddTokens(ctx, keyID, now, n); err != nil {
		e.log.Log(ctx, slog.LevelError, "quota record tokens failed", "key_id", keyID, "tokens", n, "err", err)
		return
	}
	e.log.Log(ctx, slog.LevelDebug, "quota tokens recorded", "key_id", keyID, "tokens", n)
}

// RecordGlobalTokens adds n tokens to the server-wide (global) minute-token
// counter, AFTER a job returns, so the global TPM budget enforced by
// CheckAndReserveGlobal reflects fleet-wide usage. It mirrors RecordTokens but
// targets the reserved global counter (globalKeyID) instead of a real key, and
// is the token dimension's counterpart to the global RPM reservation that
// CheckAndReserveGlobal already performs per request.
//
// When no global limits are configured (RPM==0 && TPM==0 — the default), it
// short-circuits without touching the counter store, so a server without global
// limits behaves exactly as before and the global counter never grows. n==0 is
// a no-op for the same reason as RecordTokens.
func (e *Engine) RecordGlobalTokens(ctx context.Context, n uint64) {
	if n == 0 {
		return
	}
	if e.global.RPM == 0 && e.global.TPM == 0 {
		return // global limiting disabled: never touch the store (zero overhead).
	}
	now := e.now().UTC()
	if err := e.cs.AddTokens(ctx, globalKeyID, now, n); err != nil {
		e.log.Log(ctx, slog.LevelError, "quota record global tokens failed", "key_id", globalKeyID, "tokens", n, "err", err)
		return
	}
	e.log.Log(ctx, slog.LevelDebug, "quota global tokens recorded", "key_id", globalKeyID, "tokens", n)
}

// Usage returns a Snapshot of keyID's current usage versus the supplied
// effective limits. Callers pass the limits (resolved from the key) so Usage
// does not need the full APIKey; UsageForKey is the convenience wrapper.
func (e *Engine) Usage(ctx context.Context, keyID string, lim Limits) (Snapshot, error) {
	now := e.now().UTC()
	c, err := e.cs.Get(ctx, keyID, now)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		KeyID:              keyID,
		Limits:             lim,
		RequestsThisMinute: c.MinuteRequests,
		TokensThisMinute:   c.MinuteTokens,
		TokensToday:        c.DayTokens,
		TokensThisMonth:    c.MonthTokens,
		MinuteResetsAt:     WindowReset(windowMinute, now),
		DayResetsAt:        WindowReset(windowDay, now),
		MonthResetsAt:      WindowReset(windowMonth, now),
	}, nil
}

// UsageForKey returns a Snapshot for key, resolving its effective limits
// (per-key override or engine defaults).
func (e *Engine) UsageForKey(ctx context.Context, key store.APIKey) (Snapshot, error) {
	return e.Usage(ctx, key.ID, e.effectiveLimits(key))
}
