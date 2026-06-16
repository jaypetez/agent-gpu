// Package quota implements per-key consumption limits for agent-gpu:
// requests-per-minute (RPM), tokens-per-minute (TPM), and daily/monthly token
// budgets, with fixed reset windows aligned to UTC boundaries.
//
// # Windows (fixed/calendar, NOT sliding)
//
// Each limit dimension is enforced over a fixed window whose start is truncated
// to a UTC boundary; a window resets the instant the clock crosses its
// boundary. The boundaries are:
//
//	minute  -> truncate to the start of the UTC minute   (RPM, TPM)
//	day     -> UTC midnight (00:00:00 UTC)               (daily token budget)
//	month   -> the 1st of the month at 00:00:00 UTC      (monthly token budget)
//
// A counter carries the start timestamp of the window it accumulated, so the
// engine can detect a boundary crossing and roll (zero) the counter on the
// first access in a new window. This is a fixed-window counter, not a
// continuously-sliding window: at the boundary the allowance fully resets.
//
// # Limits
//
// Limits attach to a key (store.APIKey.Limits). A nil pointer means "use the
// global defaults from QuotaConfig"; a non-nil value overrides per dimension. A
// zero value for any single dimension means UNLIMITED for that dimension.
//
// # Reserve-then-record
//
// CheckAndReserve runs BEFORE dispatch: it rolls expired windows, denies if RPM
// would be exceeded or a token budget is already exhausted, and otherwise
// reserves (increments) the request counter. RecordTokens runs AFTER the job
// returns, adding the produced tokens to the TPM/daily/monthly counters. A
// request therefore always consumes one RPM unit (the attempt), but only
// consumes token budget if the job actually produced tokens.
//
// # Scope
//
// This package owns the quota *engine*, the enforcement primitive
// (ErrQuotaExceeded), the server-wide global limiter (CheckAndReserveGlobal,
// #6), the in-memory counter store, and its checkpoint persistence. A Redis
// counter backend and real Ollama token counts (#11) are out of scope;
// ErrQuotaExceeded is the typed seam the HTTP layer maps to HTTP 429 (both for
// per-key quota and the global rate limit).
package quota

import (
	"errors"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// ErrQuotaExceeded is returned by CheckAndReserve when a request would exceed a
// key's quota (RPM) or when a token budget (TPM/daily/monthly) is already
// exhausted. It is the typed seam the request path (#6) maps to HTTP 429,
// mirroring auth.ErrUnauthenticated (401) and authz.ErrForbidden (403). Match
// it with errors.Is.
var ErrQuotaExceeded = errors.New("quota: exceeded")

// Limits are the per-key quota limits. It aliases store.Limits, which lives in
// the store package so it can be persisted on APIKey without an import cycle.
// A zero value for any field means "unlimited" for that dimension.
type Limits = store.Limits

// window identifies a quota reset window.
type window int

const (
	windowMinute window = iota
	windowDay
	windowMonth
)

// String renders the window for audit logs.
func (w window) String() string {
	switch w {
	case windowMinute:
		return "minute"
	case windowDay:
		return "day"
	case windowMonth:
		return "month"
	default:
		return "unknown"
	}
}

// windowStart returns the UTC-aligned start of w's window containing t.
func windowStart(w window, t time.Time) time.Time {
	t = t.UTC()
	switch w {
	case windowMinute:
		return t.Truncate(time.Minute)
	case windowDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case windowMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return t
	}
}

// WindowReset returns the time at which the window containing t next resets
// (i.e. the start of the following window). It is exported so the request path
// (#6) can surface a Retry-After hint.
func WindowReset(w window, t time.Time) time.Time {
	start := windowStart(w, t)
	switch w {
	case windowMinute:
		return start.Add(time.Minute)
	case windowDay:
		return start.AddDate(0, 0, 1)
	case windowMonth:
		return start.AddDate(0, 1, 0)
	default:
		return start
	}
}

// Counters holds one key's fixed-window counters. Each counter records the
// start of the window it accumulated, so the engine can roll it when the clock
// crosses a boundary. The minute window tracks both requests and tokens; the
// day and month windows track tokens only.
//
// Counters is a plain value type (no methods, no locking): the CounterStore is
// responsible for all serialization. It is JSON-serializable for the checkpoint
// file.
type Counters struct {
	// MinuteStart is the start of the current minute window.
	MinuteStart time.Time `json:"minute_start"`
	// MinuteRequests is requests reserved in the current minute window.
	MinuteRequests uint64 `json:"minute_requests"`
	// MinuteTokens is tokens recorded in the current minute window.
	MinuteTokens uint64 `json:"minute_tokens"`

	// DayStart is the start of the current day window.
	DayStart time.Time `json:"day_start"`
	// DayTokens is tokens recorded in the current day window.
	DayTokens uint64 `json:"day_tokens"`

	// MonthStart is the start of the current month window.
	MonthStart time.Time `json:"month_start"`
	// MonthTokens is tokens recorded in the current month window.
	MonthTokens uint64 `json:"month_tokens"`
}

// roll zeroes any counter whose window boundary now has been crossed, resetting
// its start to the current window. It is the single place windows reset.
func (c *Counters) roll(now time.Time) {
	if ms := windowStart(windowMinute, now); !c.MinuteStart.Equal(ms) {
		c.MinuteStart = ms
		c.MinuteRequests = 0
		c.MinuteTokens = 0
	}
	if ds := windowStart(windowDay, now); !c.DayStart.Equal(ds) {
		c.DayStart = ds
		c.DayTokens = 0
	}
	if mos := windowStart(windowMonth, now); !c.MonthStart.Equal(mos) {
		c.MonthStart = mos
		c.MonthTokens = 0
	}
}

// Snapshot is a point-in-time view of a key's usage versus its effective
// limits, for inspection (the `key quota` CLI and the future admin API). A
// limit of 0 means unlimited for that dimension.
type Snapshot struct {
	// KeyID is the key the snapshot describes.
	KeyID string
	// Limits are the effective limits (per-key override or global default).
	Limits Limits

	// RequestsThisMinute is requests reserved in the current minute window.
	RequestsThisMinute uint64
	// TokensThisMinute is tokens recorded in the current minute window.
	TokensThisMinute uint64
	// TokensToday is tokens recorded in the current day window.
	TokensToday uint64
	// TokensThisMonth is tokens recorded in the current month window.
	TokensThisMonth uint64

	// MinuteResetsAt / DayResetsAt / MonthResetsAt are when each window next
	// resets (UTC).
	MinuteResetsAt time.Time
	DayResetsAt    time.Time
	MonthResetsAt  time.Time
}

// RetryAfter returns when the soonest exhausted limit dimension in the snapshot
// next resets, given the current time now. It is the seam the request path uses
// to set a Retry-After hint on a per-key quota 429:
//
//   - Among the dimensions at or over their (non-zero) limit, it returns the
//     soonest reset — that window must clear before another request is admitted.
//   - If no dimension is reported at/over its limit (e.g. a concurrent reset, or
//     a denial whose cause already rolled), it falls back to the soonest reset
//     strictly after now, so a caller always receives a forward-looking hint.
//
// The boolean is false only when no window resets after now (no limits in play),
// in which case the caller should omit Retry-After. now is supplied (rather than
// read from a clock) so the computation is deterministic under an injected
// engine clock.
func (s Snapshot) RetryAfter(now time.Time) (time.Time, bool) {
	now = now.UTC()
	var (
		best  time.Time
		found bool
	)
	consider := func(reset time.Time) {
		if !reset.After(now) {
			return
		}
		if !found || reset.Before(best) {
			best, found = reset, true
		}
	}

	// Prefer the soonest reset among dimensions that are actually at/over limit.
	if s.Limits.RPM != 0 && s.RequestsThisMinute >= s.Limits.RPM {
		consider(s.MinuteResetsAt)
	}
	if s.Limits.TPM != 0 && s.TokensThisMinute >= s.Limits.TPM {
		consider(s.MinuteResetsAt)
	}
	if s.Limits.DailyTokens != 0 && s.TokensToday >= s.Limits.DailyTokens {
		consider(s.DayResetsAt)
	}
	if s.Limits.MonthlyTokens != 0 && s.TokensThisMonth >= s.Limits.MonthlyTokens {
		consider(s.MonthResetsAt)
	}
	if found {
		return best, true
	}

	// Fallback: no dimension reported at/over its limit. Return the soonest
	// window reset strictly after now so the hint is still forward-looking.
	consider(s.MinuteResetsAt)
	consider(s.DayResetsAt)
	consider(s.MonthResetsAt)
	return best, found
}
