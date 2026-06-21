package httpapi

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/store"
	usagepkg "github.com/jaypetez/agent-gpu/internal/usage"
)

// Usage/quota reporting (#97): GET /v1/admin/usage returns, for every key, its
// current consumption versus its effective limits (the same live quota.Snapshot
// the per-key GET .../quota endpoint reports) enriched with a bounded rolling
// DAILY token series (for a sparkline) and a best-effort exhaustion forecast (for
// a "running out" warning). A top-level summary carries the AGGREGATE throttle
// counts (the rate limiter tracks throttling fleet-wide, not per key, so per-key
// throttling is deliberately NOT fabricated). The rows are cursor-paginated in the
// shared list envelope; the same data is available as a flat CSV via ?format=csv.
//
// This is a pure read of usage telemetry — gated to telemetry:read and not audited
// (matching the other admin read endpoints). The quota engine is the source of the
// per-key snapshot; the usage series store is the source of the history. When the
// quota engine is not wired the endpoint returns 501 (mirroring the per-key quota
// endpoint); when only the series store is missing, rows are served with empty
// series and no forecast (the series is a pure enrichment).

// adminUsageResponse is the JSON envelope of GET /v1/admin/usage: a top-level
// throttle summary plus the cursor-paginated per-key usage rows. The rows live
// under the shared list envelope shape ({"data":[...],"pagination":{...}}); the
// summary sits alongside it so a dashboard reads the fleet-wide throttle counts
// once regardless of which page it fetched.
type adminUsageResponse struct {
	Summary    adminUsageSummary `json:"summary"`
	Data       []adminUsageRow   `json:"data"`
	Pagination paginationMeta    `json:"pagination"`
}

// adminUsageSummary is the top-level, fleet-wide section of the usage response.
// The throttle counts are AGGREGATE (server-wide), sourced from RateLimitStats —
// the rate limiter does not attribute throttling to individual keys, so these are
// reported once at the top level and never per row. KeyCount is the number of keys
// matched by the active filters (the full result set, not just the current page).
type adminUsageSummary struct {
	KeyCount        int    `json:"key_count"`
	GlobalThrottled uint64 `json:"global_throttled"`
	KeyThrottled    uint64 `json:"key_throttled"`
}

// adminUsageRow is one key's usage row: its identity/labels, the live quota
// snapshot view (current usage vs effective limits, criterion 2 — reusing the same
// fields as the per-key quota endpoint), the rolling daily token series (for a
// sparkline; empty when no history or the series store is disabled), and a
// best-effort exhaustion forecast (null fields when unlimited or insufficient
// data). Owner/Team are echoed (omitted when unset) so a dashboard can group/label
// rows without a second lookup.
type adminUsageRow struct {
	KeyID  string     `json:"key_id"`
	Name   string     `json:"name"`
	Owner  string     `json:"owner,omitempty"`
	Team   string     `json:"team,omitempty"`
	Limits limitsView `json:"limits"`

	RequestsThisMinute uint64 `json:"requests_this_minute"`
	TokensThisMinute   uint64 `json:"tokens_this_minute"`
	TokensToday        uint64 `json:"tokens_today"`
	TokensThisMonth    uint64 `json:"tokens_this_month"`

	MinuteResetsAt int64 `json:"minute_resets_at"`
	DayResetsAt    int64 `json:"day_resets_at"`
	MonthResetsAt  int64 `json:"month_resets_at"`

	// Series is the rolling daily token history, oldest first (at most
	// usage.MaxDays points). It is always present (an empty array, never null) so a
	// client can iterate without a nil guard.
	Series []adminUsageSeriesPoint `json:"series"`

	// Forecast is the best-effort exhaustion projection. Its fields are null when no
	// estimate is available (unlimited dimension or insufficient history); it is
	// documented as an estimate for rendering, not an enforcement input.
	Forecast adminUsageForecast `json:"forecast"`
}

// adminUsageSeriesPoint is one daily point of a key's usage series: the UTC day
// (unix epoch seconds at midnight) and that day's cumulative tokens plus the
// requests observed in the minute window at sample time.
type adminUsageSeriesPoint struct {
	Day      int64  `json:"day"`
	Tokens   uint64 `json:"tokens"`
	Requests uint64 `json:"requests"`
}

// adminUsageForecast is the wire shape of a key's exhaustion forecast: the
// projected unix-seconds instant each token budget is reached at the recent daily
// burn rate, or null when there is no estimate (unlimited budget, non-rising
// usage, or insufficient history). It is a best-effort ESTIMATE for dashboard
// rendering, not an enforcement signal.
type adminUsageForecast struct {
	DailyExhaustionAt   *int64 `json:"daily_exhaustion_at"`
	MonthlyExhaustionAt *int64 `json:"monthly_exhaustion_at"`
}

// handleAdminUsage serves GET /v1/admin/usage. It lists every key, builds a usage
// row from the key's live quota snapshot + its daily series + a best-effort
// forecast, applies the optional key_id/owner/team filters, sorts deterministically
// by key id, and returns either the JSON envelope (default) or a flat CSV export
// (?format=csv). The fleet-wide throttle summary is read once from RateLimitStats.
// Gated to telemetry:read (s.requireScope). A nil quota engine yields 501; a nil
// series store yields rows with empty series and no forecast (never a 500).
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if s.quota == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "quota is not enabled")
		return
	}

	keys, err := s.auth.List(r.Context())
	if err != nil {
		s.reqLog(r.Context()).Error("admin usage list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list keys")
		return
	}

	// Optional exact/label filters (#96 labels). key_id is an exact match; owner and
	// team match the corresponding key labels exactly. An empty filter matches all.
	q := r.URL.Query()
	filterKeyID := q.Get("key_id")
	filterOwner := q.Get("owner")
	filterTeam := q.Get("team")

	now := s.quota.Now()
	rows := make([]adminUsageRow, 0, len(keys))
	for _, k := range keys {
		if !usageKeyMatches(k, filterKeyID, filterOwner, filterTeam) {
			continue
		}
		snap, err := s.quota.UsageForKey(r.Context(), k)
		if err != nil {
			s.reqLog(r.Context()).Error("admin usage snapshot failed", "key_id", k.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "could not read usage")
			return
		}
		rows = append(rows, s.newUsageRow(k, snap, now))
	}
	// Deterministic order so a cursor names a stable position and the CSV is stable.
	sort.Slice(rows, func(i, j int) bool { return rows[i].KeyID < rows[j].KeyID })

	stats := s.RateLimitStats()
	summary := adminUsageSummary{
		KeyCount:        len(rows),
		GlobalThrottled: stats.GlobalThrottled,
		KeyThrottled:    stats.KeyThrottled,
	}

	// CSV export is the full (filtered) result set — it is a flat report, not a
	// paginated view — so a dashboard's "export" gets every matching row in one file.
	if q.Get("format") == "csv" {
		s.writeUsageCSV(w, r, rows)
		return
	}

	limit, offset := parsePageParams(r)
	page, next := paginate(rows, limit, offset)
	writeJSON(w, http.StatusOK, adminUsageResponse{
		Summary: summary,
		Data:    page,
		Pagination: paginationMeta{
			NextCursor: next,
			HasMore:    next != nil,
		},
	})
}

// usageKeyMatches reports whether k passes the active usage filters. An empty
// filter field is a wildcard for that dimension; non-empty fields must match
// exactly and are ANDed, mirroring the audit-log filter semantics.
func usageKeyMatches(k store.APIKey, keyID, owner, team string) bool {
	if keyID != "" && k.ID != keyID {
		return false
	}
	if owner != "" && k.Owner != owner {
		return false
	}
	if team != "" && k.Team != team {
		return false
	}
	return true
}

// newUsageRow builds a key's usage row from its live quota snapshot, its daily
// series (empty when the series store is disabled or the key has no history), and
// a best-effort exhaustion forecast computed from the series + snapshot + now. now
// is the engine clock captured once by the caller so every row in one response is
// forecast against the same instant.
func (s *Server) newUsageRow(k store.APIKey, snap quota.Snapshot, now time.Time) adminUsageRow {
	row := adminUsageRow{
		KeyID: k.ID,
		Name:  k.Name,
		Owner: k.Owner,
		Team:  k.Team,
		Limits: limitsView{
			RPM:           snap.Limits.RPM,
			TPM:           snap.Limits.TPM,
			DailyTokens:   snap.Limits.DailyTokens,
			MonthlyTokens: snap.Limits.MonthlyTokens,
		},
		RequestsThisMinute: snap.RequestsThisMinute,
		TokensThisMinute:   snap.TokensThisMinute,
		TokensToday:        snap.TokensToday,
		TokensThisMonth:    snap.TokensThisMonth,
		MinuteResetsAt:     snap.MinuteResetsAt.Unix(),
		DayResetsAt:        snap.DayResetsAt.Unix(),
		MonthResetsAt:      snap.MonthResetsAt.Unix(),
		Series:             []adminUsageSeriesPoint{},
	}

	if s.usageSeries != nil {
		series := s.usageSeries.SeriesForKey(k.ID)
		points := make([]adminUsageSeriesPoint, len(series))
		for i, p := range series {
			points[i] = adminUsageSeriesPoint{
				Day:      p.Day.Unix(),
				Tokens:   p.Tokens,
				Requests: p.Requests,
			}
		}
		if len(points) > 0 {
			row.Series = points
		}
		f := usagepkg.Project(series, snap.Limits, snap, now)
		if f.DailyExhaustionAt != nil {
			at := f.DailyExhaustionAt.Unix()
			row.Forecast.DailyExhaustionAt = &at
		}
		if f.MonthlyExhaustionAt != nil {
			at := f.MonthlyExhaustionAt.Unix()
			row.Forecast.MonthlyExhaustionAt = &at
		}
	}

	return row
}

// usageCSVHeader is the column order of the CSV export. It is flat (one row per
// key, no nested series) so it opens cleanly in a spreadsheet; the series and the
// fleet-wide throttle summary are JSON-only (a flat report cannot represent a
// variable-length series per row). The forecast columns are unix-seconds or empty.
var usageCSVHeader = []string{
	"key_id", "name", "owner", "team",
	"rpm_limit", "tpm_limit", "daily_tokens_limit", "monthly_tokens_limit",
	"requests_this_minute", "tokens_this_minute", "tokens_today", "tokens_this_month",
	"daily_exhaustion_at", "monthly_exhaustion_at",
}

// writeUsageCSV writes the per-key rows as a text/csv attachment. It is the full
// (filtered, sorted) set — CSV is an export, not a paginated view. A Content-
// Disposition names a sensible default filename. Header + rows use stdlib
// encoding/csv (no new dependency). A flush/write error after the header is logged
// but cannot change the already-sent status.
func (s *Server) writeUsageCSV(w http.ResponseWriter, r *http.Request, rows []adminUsageRow) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	if err := cw.Write(usageCSVHeader); err != nil {
		s.reqLog(r.Context()).Error("admin usage csv header write failed", "err", err)
		return
	}
	for _, row := range rows {
		if err := cw.Write(usageCSVRecord(row)); err != nil {
			s.reqLog(r.Context()).Error("admin usage csv row write failed", "err", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.reqLog(r.Context()).Error("admin usage csv flush failed", "err", err)
	}
}

// usageCSVRecord renders one usage row as a CSV record aligned with
// usageCSVHeader. Numeric fields are base-10 strings; the forecast columns are the
// unix-seconds projection or empty when there is no estimate.
func usageCSVRecord(row adminUsageRow) []string {
	return []string{
		row.KeyID,
		row.Name,
		row.Owner,
		row.Team,
		strconv.FormatUint(row.Limits.RPM, 10),
		strconv.FormatUint(row.Limits.TPM, 10),
		strconv.FormatUint(row.Limits.DailyTokens, 10),
		strconv.FormatUint(row.Limits.MonthlyTokens, 10),
		strconv.FormatUint(row.RequestsThisMinute, 10),
		strconv.FormatUint(row.TokensThisMinute, 10),
		strconv.FormatUint(row.TokensToday, 10),
		strconv.FormatUint(row.TokensThisMonth, 10),
		formatUnixPtr(row.Forecast.DailyExhaustionAt),
		formatUnixPtr(row.Forecast.MonthlyExhaustionAt),
	}
}

// formatUnixPtr renders an optional unix-seconds pointer as a base-10 string, or
// the empty string when nil (no estimate).
func formatUnixPtr(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}
