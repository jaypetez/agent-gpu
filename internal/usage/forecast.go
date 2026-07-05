package usage

import (
	"time"

	"github.com/jaypetez/agent-gpu/internal/quota"
)

// Forecast is a best-effort projection of when a key will exhaust its daily and
// monthly token budgets, derived from the recent daily-token slope in its series.
// It is an ESTIMATE for dashboard rendering (a sparkline's trend line), NOT an
// enforcement input — the quota engine alone decides admission. Each field is a
// pointer so "no forecast" (unlimited dimension, or too little data to project a
// slope) is distinct from "exhausted now": nil means no projection is available.
type Forecast struct {
	// DailyExhaustionAt is the projected instant the key's DAILY token budget is
	// reached, or nil when the daily budget is unlimited, the slope is flat/negative
	// (usage not rising), or there is insufficient history. Because the daily budget
	// resets every UTC midnight, this is only meaningful within the current day; it
	// is clamped to no later than the next daily reset.
	DailyExhaustionAt *time.Time
	// MonthlyExhaustionAt is the projected instant the key's MONTHLY token budget is
	// reached at the recent daily burn rate, or nil when the monthly budget is
	// unlimited, the burn rate is non-positive, or there is insufficient history.
	MonthlyExhaustionAt *time.Time
}

// dailyBurnRate estimates the key's tokens-per-day burn rate from its series. It
// uses the slope between the earliest and latest distinct-day samples (total
// tokens consumed across the covered days divided by the number of days spanned),
// which is robust to the sampling cadence and to a single spiky day. It returns
// (rate, true) only when there are at least two distinct days of history and the
// rate is strictly positive; otherwise (0, false), signaling "cannot project".
func dailyBurnRate(series []DailySample) (float64, bool) {
	if len(series) < 2 {
		return 0, false
	}
	first := series[0]
	last := series[len(series)-1]
	days := last.Day.Sub(first.Day).Hours() / 24
	if days < 1 {
		return 0, false
	}
	// Sum the per-day token figures across all but the first sample as the tokens
	// consumed over the spanned window. Each sample's Tokens is that day's
	// cumulative total (daily budgets reset at midnight), so summing the later days
	// approximates total throughput over the window without double-counting a
	// running cumulative.
	var total float64
	for _, s := range series[1:] {
		total += float64(s.Tokens)
	}
	rate := total / days
	if rate <= 0 {
		return 0, false
	}
	return rate, true
}

// Project computes a best-effort exhaustion Forecast for a key given its daily
// series, its effective limits, the latest live snapshot (for the up-to-the-minute
// today/this-month consumption), and the current time now. A zero Forecast (both
// fields nil) is returned when no projection can be made — an unlimited budget,
// non-rising usage, or too little history — so the caller renders "no estimate"
// rather than a misleading number. now is supplied (not read from a clock) so the
// projection is deterministic under test.
func Project(series []DailySample, lim quota.Limits, snap quota.Snapshot, now time.Time) Forecast {
	var f Forecast
	rate, ok := dailyBurnRate(series)
	if !ok {
		return f
	}
	now = now.UTC()
	perSecond := rate / (24 * 60 * 60)
	if perSecond <= 0 {
		return f
	}

	// Daily budget: project from today's consumed tokens at the daily burn rate,
	// clamped to the next UTC-midnight reset (a daily budget cannot be exhausted
	// past the moment it resets). Skipped when unlimited or already exhausted.
	if lim.DailyTokens != 0 && snap.TokensToday < lim.DailyTokens {
		remaining := float64(lim.DailyTokens - snap.TokensToday)
		if at, ok := projectAt(now, remaining, perSecond); ok {
			// Suppress the estimate when the budget resets before the projected
			// exhaustion: it will not be exhausted this day.
			if reset := snap.DayResetsAt; reset.IsZero() || !at.After(reset) {
				f.DailyExhaustionAt = &at
			}
		}
	}

	// Monthly budget: project from this month's consumed tokens at the same daily
	// burn rate. Skipped when unlimited or already exhausted.
	if lim.MonthlyTokens != 0 && snap.TokensThisMonth < lim.MonthlyTokens {
		remaining := float64(lim.MonthlyTokens - snap.TokensThisMonth)
		if at, ok := projectAt(now, remaining, perSecond); ok {
			f.MonthlyExhaustionAt = &at
		}
	}

	return f
}

// maxForecastHorizon caps how far a forecast may project. Beyond this the estimate
// is meaningless (and the nanosecond arithmetic risks overflowing a time.Duration),
// so projectAt reports "no estimate" rather than a garbage or wrapped timestamp. A
// year comfortably exceeds the monthly window any real budget exhausts within.
const maxForecastHorizon = 365 * 24 * time.Hour

// projectAt returns the instant `remaining` tokens are consumed at perSecond
// tokens/second after now, and whether that projection is within the sane horizon.
// It guards against a near-zero rate producing an absurd (or duration-overflowing)
// far-future time: when the horizon is exceeded it returns ok=false so the caller
// renders "no estimate".
func projectAt(now time.Time, remaining, perSecond float64) (time.Time, bool) {
	secs := remaining / perSecond
	if secs <= 0 || secs > maxForecastHorizon.Seconds() {
		return time.Time{}, false
	}
	return now.Add(time.Duration(secs * float64(time.Second))), true
}
