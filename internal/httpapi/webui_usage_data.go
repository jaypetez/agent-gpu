package httpapi

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
)

// webui_usage_data.go maps the server's in-process usage/quota state onto the
// console's Usage view-models (#103). It reuses the SAME per-key projection the
// JSON GET /v1/admin/usage builds — s.auth.List for the keys, s.quota.UsageForKey
// for each live snapshot, and s.newUsageRow (the shared row builder that folds in
// the daily series + the best-effort forecast) — so the console's meters, sparkline,
// and forecast never disagree with the API. It performs no new probing and starts
// no pollers; every value is read on demand for one consistent instant. When the
// quota engine is not wired the board reports Enabled=false (mirroring the JSON
// endpoint's 501), and a nil series store yields empty sparklines / no forecast.

// collectUsage builds the Usage board from the live quota snapshots. Enabled is
// false when no quota engine is wired (the screen renders the disabled notice), so
// the partial never panics on a nil engine. Rows are sorted by key id for a stable
// render across refreshes, matching the JSON endpoint's deterministic order.
func (s *Server) collectUsage(ctx context.Context) webui.UsageBoard {
	if s.quota == nil {
		return webui.UsageBoard{Enabled: false}
	}
	keys, err := s.auth.List(ctx)
	if err != nil {
		// The auth store read failed; report an empty-but-enabled board so the screen
		// shows its empty state rather than stale data. The error is surfaced by the
		// handler's logging, not fabricated here.
		return webui.UsageBoard{Enabled: true}
	}

	now := s.quota.Now()
	rows := make([]webui.UsageRow, 0, len(keys))
	for _, k := range keys {
		snap, err := s.quota.UsageForKey(ctx, k)
		if err != nil {
			continue
		}
		rows = append(rows, newUsageViewRow(s.newUsageRow(k, snap, now), now))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].KeyID < rows[j].KeyID })

	stats := s.RateLimitStats()
	return webui.UsageBoard{
		Enabled:         true,
		KeyCount:        len(rows),
		GlobalThrottled: formatCount(stats.GlobalThrottled),
		KeyThrottled:    formatCount(stats.KeyThrottled),
		Rows:            rows,
	}
}

// newUsageViewRow projects one JSON usage row (the shared adminUsageRow the API
// returns) into the console row model: the four consumption-vs-limit meters, the
// 7-day token sparkline, and the relative exhaustion forecast. now is the engine
// clock the caller captured once so every row's forecast phrase is relative to the
// same instant.
func newUsageViewRow(r adminUsageRow, now time.Time) webui.UsageRow {
	// Sparkline over the daily token series (oldest→newest, already in that order).
	tokens := make([]uint64, len(r.Series))
	var peak uint64
	for i, p := range r.Series {
		tokens[i] = p.Tokens
		if p.Tokens > peak {
			peak = p.Tokens
		}
	}
	spark := webui.Sparkline{Points: webui.Sparkpoints(tokens), Has: len(tokens) >= 2}
	if len(tokens) > 0 {
		spark.Last = formatCount(tokens[len(tokens)-1])
		spark.Peak = formatCount(peak)
	}

	return webui.UsageRow{
		KeyID:         r.KeyID,
		Name:          r.Name,
		Owner:         r.Owner,
		Team:          r.Team,
		DailyTokens:   tokenMeter("Daily tokens", r.TokensToday, r.Limits.DailyTokens),
		MonthlyTokens: tokenMeter("Monthly tokens", r.TokensThisMonth, r.Limits.MonthlyTokens),
		Requests:      tokenMeter("Requests / min", r.RequestsThisMinute, r.Limits.RPM),
		TokensPerMin:  tokenMeter("Tokens / min", r.TokensThisMinute, r.Limits.TPM),
		Spark:         spark,
		Forecast:      usageForecastView(r.Forecast, now),
	}
}

// tokenMeter builds one consumption-vs-limit bar. A zero limit means the dimension
// is unlimited: the meter is marked Limited=false and renders as an informational
// "no limit" track (never a full or empty meter, which would misread as at/under
// capacity). Otherwise the fill percentage is used/limit clamped to [0,100] and the
// tone crosses the usage thresholds.
func tokenMeter(label string, used, limit uint64) webui.UsageMeter {
	if limit == 0 {
		return webui.UsageMeter{
			Label:   label,
			Used:    formatCount(used),
			Limit:   "no limit",
			Pct:     0,
			Tone:    webui.ToneIdle,
			Limited: false,
		}
	}
	pct := int((used * 100) / limit)
	if pct > 100 {
		pct = 100
	}
	return webui.UsageMeter{
		Label:   label,
		Used:    formatCount(used),
		Limit:   formatCount(limit),
		Pct:     pct,
		Tone:    webui.UsageTone(pct),
		Limited: true,
	}
}

// usageForecastView turns the unix-seconds exhaustion forecast into a relative
// phrase + urgency tone for the row. It prefers the SOONER of the daily/monthly
// projections (the budget that runs out first is what an operator must act on),
// names which budget it is, and tones it by how soon: alert within a day, watch
// within a few. When neither budget is projected to exhaust, Has is false and the
// row shows a calm "no forecast" note.
func usageForecastView(f adminUsageForecast, now time.Time) webui.UsageForecast {
	type cand struct {
		at  int64
		dim string
	}
	var best *cand
	consider := func(at *int64, dim string) {
		if at == nil {
			return
		}
		if best == nil || *at < best.at {
			best = &cand{at: *at, dim: dim}
		}
	}
	consider(f.DailyExhaustionAt, "daily")
	consider(f.MonthlyExhaustionAt, "monthly")
	if best == nil {
		return webui.UsageForecast{Has: false}
	}
	d := time.Unix(best.at, 0).Sub(now)
	tone := webui.ToneInfo
	switch {
	case d <= 24*time.Hour:
		tone = webui.ToneDanger
	case d <= 72*time.Hour:
		tone = webui.ToneWarn
	}
	return webui.UsageForecast{
		Has:       true,
		Phrase:    forecastPhrase(d),
		Dimension: best.dim,
		Tone:      tone,
	}
}

// forecastPhrase renders a duration-to-exhaustion as a coarse human phrase: "< 1h"
// when imminent (or already past, a defensive clamp), "~Nh" within a day, "~Nd"
// beyond. It is intentionally coarse — a forecast is an estimate, not a clock.
func forecastPhrase(d time.Duration) string {
	if d < time.Hour {
		return "< 1h"
	}
	if d < 24*time.Hour {
		return "~" + strconv.Itoa(int(d.Hours())) + "h"
	}
	return "~" + strconv.Itoa(int(d.Hours()/24)) + "d"
}
