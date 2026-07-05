package usage

import (
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/quota"
)

// risingSeries builds a 3-day series climbing by perDay tokens each day, so the
// burn rate is a clean perDay tokens/day.
func risingSeries(perDay uint64) []DailySample {
	return []DailySample{
		{Day: dayStart(day(0)), Tokens: perDay},
		{Day: dayStart(day(1)), Tokens: perDay},
		{Day: dayStart(day(2)), Tokens: perDay},
	}
}

// TestProjectForecast is the table-driven proof of the best-effort exhaustion
// forecast: a rising daily-token slope approaching a daily/monthly limit yields a
// projected instant; an unlimited budget, a flat/empty series, or an
// already-exhausted budget yields no estimate (nil). It uses a fixed now so the
// projection is deterministic (no wall clock).
func TestProjectForecast(t *testing.T) {
	now := day(2).Add(6 * time.Hour) // mid-day on the latest sample's day
	const perDay = 100_000

	cases := []struct {
		name      string
		series    []DailySample
		lim       quota.Limits
		snap      quota.Snapshot
		wantDaily bool // expect a non-nil daily_exhaustion_at
		wantMonth bool // expect a non-nil monthly_exhaustion_at
	}{
		{
			name: "rising usage toward a daily limit forecasts daily exhaustion",
			// A fast burn (~400k/day) with only 100k of the 200k daily budget left:
			// the remaining 100k is consumed in ~6h, well before the ~18h-away reset,
			// so a daily estimate is produced.
			series: risingSeries(400_000),
			lim:    quota.Limits{DailyTokens: 200_000},
			snap: quota.Snapshot{
				TokensToday: 100_000,
				DayResetsAt: day(3), // next UTC midnight, ~18h away
			},
			wantDaily: true,
			wantMonth: false,
		},
		{
			name:      "rising usage toward a monthly limit forecasts monthly exhaustion",
			series:    risingSeries(perDay),
			lim:       quota.Limits{MonthlyTokens: 1_000_000},
			snap:      quota.Snapshot{TokensThisMonth: 500_000},
			wantDaily: false,
			wantMonth: true,
		},
		{
			name:      "unlimited budgets yield no forecast",
			series:    risingSeries(perDay),
			lim:       quota.Limits{}, // all zero = unlimited
			snap:      quota.Snapshot{TokensToday: 100_000, TokensThisMonth: 500_000},
			wantDaily: false,
			wantMonth: false,
		},
		{
			name:   "insufficient history yields no forecast",
			series: []DailySample{{Day: dayStart(day(2)), Tokens: perDay}}, // one day only
			lim:    quota.Limits{DailyTokens: 200_000, MonthlyTokens: 1_000_000},
			snap:   quota.Snapshot{TokensToday: 100_000, TokensThisMonth: 500_000},
			// A single sample cannot establish a slope.
			wantDaily: false,
			wantMonth: false,
		},
		{
			name:      "flat (zero-token) series yields no forecast",
			series:    risingSeries(0),
			lim:       quota.Limits{DailyTokens: 200_000, MonthlyTokens: 1_000_000},
			snap:      quota.Snapshot{TokensToday: 0, TokensThisMonth: 0},
			wantDaily: false,
			wantMonth: false,
		},
		{
			name: "burn far below an enormous budget exceeds the horizon (no estimate)",
			// A trickle (~1 token/day) against a budget so large that exhaustion is
			// centuries away: beyond the forecast horizon, so no monthly estimate.
			series:    risingSeries(1),
			lim:       quota.Limits{MonthlyTokens: 1_000_000_000},
			snap:      quota.Snapshot{TokensThisMonth: 0},
			wantDaily: false,
			wantMonth: false,
		},
		{
			name:   "already-exhausted daily budget yields no daily estimate",
			series: risingSeries(perDay),
			lim:    quota.Limits{DailyTokens: 200_000},
			// Today already at the limit: there is nothing left to project to.
			snap:      quota.Snapshot{TokensToday: 200_000, DayResetsAt: day(3)},
			wantDaily: false,
			wantMonth: false,
		},
		{
			name: "daily reset before projected exhaustion suppresses the daily estimate",
			// At ~400k/day with 800k of budget left, exhaustion is ~2 days out — within
			// the forecast horizon but well past the ~18h-away daily reset, so the
			// reset-clamp (not the horizon cap) suppresses the daily estimate.
			series:    risingSeries(400_000),
			lim:       quota.Limits{DailyTokens: 800_000},
			snap:      quota.Snapshot{TokensToday: 0, DayResetsAt: day(3)},
			wantDaily: false,
			wantMonth: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Project(tc.series, tc.lim, tc.snap, now)
			if (f.DailyExhaustionAt != nil) != tc.wantDaily {
				t.Errorf("daily forecast present = %v, want %v (at=%v)", f.DailyExhaustionAt != nil, tc.wantDaily, f.DailyExhaustionAt)
			}
			if (f.MonthlyExhaustionAt != nil) != tc.wantMonth {
				t.Errorf("monthly forecast present = %v, want %v (at=%v)", f.MonthlyExhaustionAt != nil, tc.wantMonth, f.MonthlyExhaustionAt)
			}
			// When a daily estimate is produced, it must lie in the future and no later
			// than the daily reset (a daily budget cannot be exhausted past its reset).
			if f.DailyExhaustionAt != nil {
				if !f.DailyExhaustionAt.After(now) {
					t.Errorf("daily exhaustion %v not after now %v", f.DailyExhaustionAt, now)
				}
				if !tc.snap.DayResetsAt.IsZero() && f.DailyExhaustionAt.After(tc.snap.DayResetsAt) {
					t.Errorf("daily exhaustion %v after reset %v", f.DailyExhaustionAt, tc.snap.DayResetsAt)
				}
			}
		})
	}
}

// TestProjectMonthlyOrdering proves the monthly projection is monotonic in burn
// rate: a faster burn rate exhausts the same remaining budget sooner.
func TestProjectMonthlyOrdering(t *testing.T) {
	now := day(2)
	lim := quota.Limits{MonthlyTokens: 1_000_000}
	snap := quota.Snapshot{TokensThisMonth: 0}

	slow := Project(risingSeries(50_000), lim, snap, now)
	fast := Project(risingSeries(200_000), lim, snap, now)
	if slow.MonthlyExhaustionAt == nil || fast.MonthlyExhaustionAt == nil {
		t.Fatalf("both rates should forecast: slow=%v fast=%v", slow.MonthlyExhaustionAt, fast.MonthlyExhaustionAt)
	}
	if !fast.MonthlyExhaustionAt.Before(*slow.MonthlyExhaustionAt) {
		t.Errorf("faster burn (%v) should exhaust sooner than slower (%v)", fast.MonthlyExhaustionAt, slow.MonthlyExhaustionAt)
	}
}
