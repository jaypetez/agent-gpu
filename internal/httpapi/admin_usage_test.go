package httpapi

import (
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/store"
	usagepkg "github.com/jaypetez/agent-gpu/internal/usage"
)

// usageResponse mirrors the GET /v1/admin/usage JSON envelope for decoding in
// tests. Local to the test so a drift in the handler's field tags is caught here.
type usageResponse struct {
	Summary struct {
		KeyCount        int    `json:"key_count"`
		GlobalThrottled uint64 `json:"global_throttled"`
		KeyThrottled    uint64 `json:"key_throttled"`
	} `json:"summary"`
	Data []struct {
		KeyID  string `json:"key_id"`
		Name   string `json:"name"`
		Owner  string `json:"owner"`
		Team   string `json:"team"`
		Limits struct {
			RPM           uint64 `json:"rpm"`
			TPM           uint64 `json:"tpm"`
			DailyTokens   uint64 `json:"daily_tokens"`
			MonthlyTokens uint64 `json:"monthly_tokens"`
		} `json:"limits"`
		RequestsThisMinute uint64 `json:"requests_this_minute"`
		TokensThisMinute   uint64 `json:"tokens_this_minute"`
		TokensToday        uint64 `json:"tokens_today"`
		TokensThisMonth    uint64 `json:"tokens_this_month"`
		Series             []struct {
			Day      int64  `json:"day"`
			Tokens   uint64 `json:"tokens"`
			Requests uint64 `json:"requests"`
		} `json:"series"`
		Forecast struct {
			DailyExhaustionAt   *int64 `json:"daily_exhaustion_at"`
			MonthlyExhaustionAt *int64 `json:"monthly_exhaustion_at"`
		} `json:"forecast"`
	} `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
}

// usageTestServer builds a Server wired to a fresh fleet, an auth service over an
// in-memory store, a quota engine (with the given defaults), and the supplied
// usage series store (which may be nil to exercise the disabled-series path). It
// mirrors adminTestServer but lets a test control the quota engine and series.
func usageTestServer(t *testing.T, defaults quota.Limits, series *usagepkg.Store) (*Server, *auth.Service, *quota.Engine) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithDefaults(defaults), quota.WithLogger(discard))
	s := &Server{
		fleet:       &fakeFleet{},
		auth:        authSvc,
		authz:       az,
		quota:       eng,
		usageSeries: series,
		log:         discard,
	}
	return s, authSvc, eng
}

// telemetryKey mints a key holding only telemetry:read (the usage endpoint's
// scope), so a test exercises the real gate rather than the admin superuser.
func telemetryKey(t *testing.T, authSvc *auth.Service) string {
	t.Helper()
	return mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeTelemetryRead}})
}

// TestAdminUsageRowsVsLimits proves criterion 1+2: each per-key row reports the
// key's effective limits and current usage from the quota snapshot, rows are
// sorted by key id, and the fleet-wide throttle summary is present.
func TestAdminUsageRowsVsLimits(t *testing.T) {
	// A global default so a key without its own override still reports limits.
	s, authSvc, eng := usageTestServer(t, quota.Limits{RPM: 100, DailyTokens: 1_000_000}, usagepkg.New())
	telem := telemetryKey(t, authSvc)

	// Two keys: one with an explicit per-key override, one on the defaults. Mint via
	// the auth service so they are real store keys the handler's List sees.
	withLimits := mustKeyNamed(t, authSvc, "alpha", auth.Permissions{},
		&store.Limits{RPM: 5, TPM: 0, DailyTokens: 50_000, MonthlyTokens: 2_000_000})
	onDefaults := mustKeyNamed(t, authSvc, "beta", auth.Permissions{}, nil)

	// Record some usage against the override key so its row is non-zero.
	ctx := context.Background()
	if err := eng.CheckAndReserve(ctx, mustGet(t, authSvc, withLimits)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	eng.RecordTokens(ctx, withLimits, 1234)

	rec := req(t, s, http.MethodGet, "/v1/admin/usage", telem, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got usageResponse
	decode(t, rec, &got)

	if got.Summary.KeyCount != 3 { // telem key + the two minted here
		t.Errorf("summary.key_count = %d, want 3", got.Summary.KeyCount)
	}
	if len(got.Data) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(got.Data), got.Data)
	}
	// Rows sorted by key id.
	for i := 1; i < len(got.Data); i++ {
		if got.Data[i-1].KeyID > got.Data[i].KeyID {
			t.Errorf("rows not sorted by key_id: %q then %q", got.Data[i-1].KeyID, got.Data[i].KeyID)
		}
	}

	// Find the override key's row and assert it mirrors the snapshot.
	var over *struct {
		KeyID  string `json:"key_id"`
		Name   string `json:"name"`
		Owner  string `json:"owner"`
		Team   string `json:"team"`
		Limits struct {
			RPM           uint64 `json:"rpm"`
			TPM           uint64 `json:"tpm"`
			DailyTokens   uint64 `json:"daily_tokens"`
			MonthlyTokens uint64 `json:"monthly_tokens"`
		} `json:"limits"`
		RequestsThisMinute uint64 `json:"requests_this_minute"`
		TokensThisMinute   uint64 `json:"tokens_this_minute"`
		TokensToday        uint64 `json:"tokens_today"`
		TokensThisMonth    uint64 `json:"tokens_this_month"`
		Series             []struct {
			Day      int64  `json:"day"`
			Tokens   uint64 `json:"tokens"`
			Requests uint64 `json:"requests"`
		} `json:"series"`
		Forecast struct {
			DailyExhaustionAt   *int64 `json:"daily_exhaustion_at"`
			MonthlyExhaustionAt *int64 `json:"monthly_exhaustion_at"`
		} `json:"forecast"`
	}
	for i := range got.Data {
		if got.Data[i].KeyID == withLimits {
			over = &got.Data[i]
		}
	}
	if over == nil {
		t.Fatalf("override key row not found in %+v", got.Data)
	}
	if over.Limits.RPM != 5 || over.Limits.DailyTokens != 50_000 || over.Limits.MonthlyTokens != 2_000_000 {
		t.Errorf("override row limits = %+v, want rpm=5 daily=50000 monthly=2000000", over.Limits)
	}
	if over.RequestsThisMinute != 1 || over.TokensToday != 1234 || over.TokensThisMonth != 1234 {
		t.Errorf("override row usage = req=%d today=%d month=%d, want 1/1234/1234",
			over.RequestsThisMinute, over.TokensToday, over.TokensThisMonth)
	}
	// The defaults-key row inherits the global defaults.
	for i := range got.Data {
		if got.Data[i].KeyID == onDefaults && got.Data[i].Limits.RPM != 100 {
			t.Errorf("defaults row rpm = %d, want 100 (global default)", got.Data[i].Limits.RPM)
		}
	}
	// series is always a present (possibly empty) array, never null.
	if !strings.Contains(rec.Body.String(), `"series":`) {
		t.Errorf("response missing series field: %s", rec.Body.String())
	}
}

// TestAdminUsagePagination proves the rows honor ?limit= and ?cursor=, following
// the shared cursor envelope across pages.
func TestAdminUsagePagination(t *testing.T) {
	s, authSvc, _ := usageTestServer(t, quota.Limits{}, usagepkg.New())
	telem := telemetryKey(t, authSvc)
	// Mint several keys so pagination has something to slice (plus the telem key).
	for i := 0; i < 5; i++ {
		mustKeyNamed(t, authSvc, "k", auth.Permissions{}, nil)
	}

	// First page of 2.
	rec := req(t, s, http.MethodGet, "/v1/admin/usage?limit=2", telem, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", rec.Code)
	}
	var page1 usageResponse
	decode(t, rec, &page1)
	if len(page1.Data) != 2 {
		t.Fatalf("page1 rows = %d, want 2", len(page1.Data))
	}
	if !page1.Pagination.HasMore || page1.Pagination.NextCursor == nil {
		t.Fatalf("page1 should report has_more with a cursor: %+v", page1.Pagination)
	}
	// summary.key_count is the FULL set (6), independent of the page size.
	if page1.Summary.KeyCount != 6 {
		t.Errorf("page1 summary.key_count = %d, want 6 (full set)", page1.Summary.KeyCount)
	}

	// Follow the cursor and ensure no overlap and eventual exhaustion.
	seen := map[string]bool{}
	for _, r := range page1.Data {
		seen[r.KeyID] = true
	}
	cursor := *page1.Pagination.NextCursor
	for {
		rec := req(t, s, http.MethodGet, "/v1/admin/usage?limit=2&cursor="+cursor, telem, "")
		var pg usageResponse
		decode(t, rec, &pg)
		for _, r := range pg.Data {
			if seen[r.KeyID] {
				t.Errorf("cursor page repeated key %q", r.KeyID)
			}
			seen[r.KeyID] = true
		}
		if pg.Pagination.NextCursor == nil {
			break
		}
		cursor = *pg.Pagination.NextCursor
	}
	if len(seen) != 6 {
		t.Errorf("paged through %d distinct keys, want 6", len(seen))
	}
}

// TestAdminUsageFilters proves the key_id/owner/team filters narrow the rows by
// exact match (ANDed), and that the summary.key_count reflects the filtered set.
func TestAdminUsageFilters(t *testing.T) {
	s, authSvc, _ := usageTestServer(t, quota.Limits{}, usagepkg.New())
	telem := telemetryKey(t, authSvc)

	alice := mustKeyNamed(t, authSvc, "a", auth.Permissions{Owner: "alice", Team: "platform"}, nil)
	mustKeyNamed(t, authSvc, "b", auth.Permissions{Owner: "bob", Team: "platform"}, nil)
	mustKeyNamed(t, authSvc, "c", auth.Permissions{Owner: "alice", Team: "research"}, nil)

	cases := []struct {
		name  string
		query string
		want  int // expected matched rows
	}{
		{"by key_id", "?key_id=" + alice, 1},
		{"by owner", "?owner=alice", 2},
		{"by team", "?team=platform", 2},
		{"owner AND team", "?owner=alice&team=platform", 1},
		{"no match", "?owner=nobody", 0},
		{"unfiltered", "", 4}, // 3 minted + telem
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := req(t, s, http.MethodGet, "/v1/admin/usage"+tc.query, telem, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got usageResponse
			decode(t, rec, &got)
			if len(got.Data) != tc.want || got.Summary.KeyCount != tc.want {
				t.Errorf("rows=%d key_count=%d, want %d", len(got.Data), got.Summary.KeyCount, tc.want)
			}
		})
	}
}

// TestAdminUsageScopeGate proves the telemetry:read gate: a key with only
// telemetry:read gets 200, a key with a different scope gets 403, and an
// unauthenticated request gets 401.
func TestAdminUsageScopeGate(t *testing.T) {
	s, authSvc, _ := usageTestServer(t, quota.Limits{}, usagepkg.New())

	telem := telemetryKey(t, authSvc)
	otherScope := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})

	if rec := req(t, s, http.MethodGet, "/v1/admin/usage", telem, ""); rec.Code != http.StatusOK {
		t.Errorf("telemetry:read status = %d, want 200", rec.Code)
	}
	rec := req(t, s, http.MethodGet, "/v1/admin/usage", otherScope, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("other-scope status = %d, want 403", rec.Code)
	} else if code := errorCode(t, rec); code != "forbidden" {
		t.Errorf("403 error code = %q, want forbidden", code)
	}
	if rec := req(t, s, http.MethodGet, "/v1/admin/usage", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
}

// TestAdminUsageCSV proves criterion 4: ?format=csv returns text/csv with the
// expected header and one row per key (the full filtered set, not paginated), and
// that the row carries the key's limits and usage in the documented columns.
func TestAdminUsageCSV(t *testing.T) {
	s, authSvc, eng := usageTestServer(t, quota.Limits{}, usagepkg.New())
	telem := telemetryKey(t, authSvc)
	k := mustKeyNamed(t, authSvc, "csv-key", auth.Permissions{Owner: "owner1", Team: "team1"},
		&store.Limits{RPM: 7, DailyTokens: 9000})
	eng.RecordTokens(context.Background(), k, 321)

	rec := req(t, s, http.MethodGet, "/v1/admin/usage?format=csv", telem, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type = %q, want text/csv...", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "usage.csv") {
		t.Errorf("content-disposition = %q, want a usage.csv attachment", cd)
	}

	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	// Header + one row per key (telem key + csv-key = 2).
	if len(rows) != 3 {
		t.Fatalf("csv rows (incl header) = %d, want 3:\n%s", len(rows), rec.Body.String())
	}
	wantHeader := []string{
		"key_id", "name", "owner", "team",
		"rpm_limit", "tpm_limit", "daily_tokens_limit", "monthly_tokens_limit",
		"requests_this_minute", "tokens_this_minute", "tokens_today", "tokens_this_month",
		"daily_exhaustion_at", "monthly_exhaustion_at",
	}
	if strings.Join(rows[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("csv header = %v, want %v", rows[0], wantHeader)
	}
	// Locate the csv-key data row by its id (column 0).
	var dataRow []string
	for _, r := range rows[1:] {
		if r[0] == k {
			dataRow = r
		}
	}
	if dataRow == nil {
		t.Fatalf("csv missing data row for key %q", k)
	}
	if dataRow[1] != "csv-key" || dataRow[2] != "owner1" || dataRow[3] != "team1" {
		t.Errorf("csv row identity = name=%q owner=%q team=%q, want csv-key/owner1/team1", dataRow[1], dataRow[2], dataRow[3])
	}
	if dataRow[4] != "7" || dataRow[6] != "9000" {
		t.Errorf("csv row limits = rpm=%q daily=%q, want 7/9000", dataRow[4], dataRow[6])
	}
	if dataRow[10] != "321" || dataRow[11] != "321" { // tokens_today, tokens_this_month
		t.Errorf("csv row tokens today/month = %q/%q, want 321/321", dataRow[10], dataRow[11])
	}
}

// TestAdminUsageNilSeries proves that without a series store the endpoint still
// serves rows (200) with an empty series and a null forecast — no 500.
func TestAdminUsageNilSeries(t *testing.T) {
	s, authSvc, eng := usageTestServer(t, quota.Limits{DailyTokens: 1000}, nil) // nil series
	telem := telemetryKey(t, authSvc)
	k := mustKeyNamed(t, authSvc, "k", auth.Permissions{}, nil)
	eng.RecordTokens(context.Background(), k, 10)

	rec := req(t, s, http.MethodGet, "/v1/admin/usage", telem, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got usageResponse
	decode(t, rec, &got)
	for _, r := range got.Data {
		if len(r.Series) != 0 {
			t.Errorf("nil-series row %q has series %+v, want empty", r.KeyID, r.Series)
		}
		if r.Forecast.DailyExhaustionAt != nil || r.Forecast.MonthlyExhaustionAt != nil {
			t.Errorf("nil-series row %q has a forecast, want none: %+v", r.KeyID, r.Forecast)
		}
	}
	// The series field must still serialize as [] (never null) so clients can iterate.
	if !strings.Contains(rec.Body.String(), `"series":[]`) {
		t.Errorf("nil-series body should carry empty series arrays: %s", rec.Body.String())
	}
}

// TestAdminUsageNilQuota proves the endpoint mirrors the per-key quota endpoint's
// 501 when the quota engine is not enabled (no usage to report).
func TestAdminUsageNilQuota(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	s := &Server{
		fleet: &fakeFleet{},
		auth:  authSvc,
		authz: authz.NewAuthorizer(authz.WithLogger(discard)),
		quota: nil, // disabled
		log:   discard,
	}
	telem := telemetryKey(t, authSvc)

	rec := req(t, s, http.MethodGet, "/v1/admin/usage", telem, "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if code := errorCode(t, rec); code != "not_implemented" {
		t.Errorf("error code = %q, want not_implemented", code)
	}
}

// TestAdminUsageSeriesAndForecast proves the captured daily series and a
// best-effort forecast surface in a row: with a rising series approaching the
// daily limit, the row carries series points and a non-null daily forecast; an
// unlimited key carries series points but no forecast.
func TestAdminUsageSeriesAndForecast(t *testing.T) {
	series := usagepkg.New()
	// A fixed engine clock so the forecast (and the series day) are deterministic.
	nowDay := time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowDay }

	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithClock(clock), quota.WithLogger(discard))
	s := &Server{
		fleet:       &fakeFleet{},
		auth:        authSvc,
		authz:       authz.NewAuthorizer(authz.WithLogger(discard)),
		quota:       eng,
		usageSeries: series,
		log:         discard,
	}
	telem := telemetryKey(t, authSvc)

	limited := mustKeyNamed(t, authSvc, "limited", auth.Permissions{}, &store.Limits{DailyTokens: 200_000})
	unlimited := mustKeyNamed(t, authSvc, "unlimited", auth.Permissions{}, nil)

	// Build a rising daily series for the limited key over the prior days, plus
	// today's partial consumption, so a daily-exhaustion forecast is produced.
	for d := -3; d < 0; d++ {
		day := nowDay.AddDate(0, 0, d)
		series.Record([]quota.Snapshot{{KeyID: limited, TokensToday: 400_000}}, day)
	}
	// Today: 100k of the 200k budget consumed (recorded into both the engine and the
	// series at today's date).
	eng.RecordTokens(context.Background(), limited, 100_000)
	series.Record([]quota.Snapshot{{KeyID: limited, TokensToday: 100_000}}, nowDay)
	// An unlimited key with its own rising series but no budget.
	for d := -3; d <= 0; d++ {
		series.Record([]quota.Snapshot{{KeyID: unlimited, TokensToday: 400_000}}, nowDay.AddDate(0, 0, d))
	}

	rec := req(t, s, http.MethodGet, "/v1/admin/usage", telem, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got usageResponse
	decode(t, rec, &got)

	var limitedRow, unlimitedRow int = -1, -1
	for i := range got.Data {
		switch got.Data[i].KeyID {
		case limited:
			limitedRow = i
		case unlimited:
			unlimitedRow = i
		}
	}
	if limitedRow < 0 || unlimitedRow < 0 {
		t.Fatalf("rows not found: limited=%d unlimited=%d", limitedRow, unlimitedRow)
	}
	// The limited key has a multi-day series and a daily forecast.
	if len(got.Data[limitedRow].Series) < 2 {
		t.Errorf("limited row series len = %d, want >= 2 for a slope", len(got.Data[limitedRow].Series))
	}
	if got.Data[limitedRow].Forecast.DailyExhaustionAt == nil {
		t.Errorf("limited row should carry a daily forecast, got none")
	}
	// The unlimited key has a series but no forecast (no budget to exhaust).
	if got.Data[unlimitedRow].Forecast.DailyExhaustionAt != nil || got.Data[unlimitedRow].Forecast.MonthlyExhaustionAt != nil {
		t.Errorf("unlimited row should have no forecast, got %+v", got.Data[unlimitedRow].Forecast)
	}
}

// TestWithUsageSeriesOption proves the WithUsageSeries option wires the series
// store into a Server built via the option (the cmd path), so the usage endpoint's
// rows carry the captured series rather than the disabled-series empty default.
func TestWithUsageSeriesOption(t *testing.T) {
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithLogger(discard))

	series := usagepkg.New()
	// Apply the option to a bare Server, exactly as NewServer does, then assert it
	// took effect by serving a row whose series carries the recorded point.
	s := &Server{fleet: &fakeFleet{}, auth: authSvc, authz: az, quota: eng, log: discard}
	WithUsageSeries(series)(s)
	if s.usageSeries != series {
		t.Fatalf("WithUsageSeries did not set the series store")
	}

	k := mustKeyNamed(t, authSvc, "k", auth.Permissions{}, nil)
	series.Record([]quota.Snapshot{{KeyID: k, TokensToday: 777}}, time.Now())
	telem := telemetryKey(t, authSvc)

	rec := req(t, s, http.MethodGet, "/v1/admin/usage?key_id="+k, telem, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got usageResponse
	decode(t, rec, &got)
	if len(got.Data) != 1 || len(got.Data[0].Series) != 1 || got.Data[0].Series[0].Tokens != 777 {
		t.Errorf("wired series not reflected in row: %+v", got.Data)
	}
}

// mustKeyNamed mints a key with a name, permissions, and an optional per-key quota
// override (nil leaves the key on the global defaults), returning its id (not the
// token) for tests that locate the key's row by id. Limits are applied via the
// auth service's SetLimits after creation, mirroring how the admin quota endpoint
// sets them.
func mustKeyNamed(t *testing.T, authSvc *auth.Service, name string, perms auth.Permissions, limits *store.Limits) string {
	t.Helper()
	_, key, err := authSvc.CreateWithPermissions(context.Background(), name, perms)
	if err != nil {
		t.Fatalf("create key %q: %v", name, err)
	}
	if limits != nil {
		if _, err := authSvc.SetLimits(context.Background(), key.ID, limits); err != nil {
			t.Fatalf("set limits on %q: %v", name, err)
		}
	}
	return key.ID
}

// mustGet fetches a key by id for use in a quota reservation.
func mustGet(t *testing.T, authSvc *auth.Service, id string) store.APIKey {
	t.Helper()
	k, err := authSvc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get key %q: %v", id, err)
	}
	return k
}
