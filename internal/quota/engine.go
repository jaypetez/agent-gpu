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
	now      func() time.Time
	log      *slog.Logger
}

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
