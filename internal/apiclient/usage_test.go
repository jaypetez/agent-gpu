package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGetUsage proves the client decodes the GET /v1/admin/usage report (summary +
// per-key rows with series and forecast) into the typed UsageReport and sends the
// request to the right path. It uses the shared recordingHandler so the request the
// client makes is asserted alongside the decoded response.
func TestGetUsage(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := httptest.NewServer(recordingHandler(&cap, http.StatusOK, `{
		"summary":{"key_count":2,"global_throttled":4,"key_throttled":1},
		"data":[
			{"key_id":"k1","name":"batch","owner":"alice","team":"platform",
			 "limits":{"rpm":60,"tpm":0,"daily_tokens":1000000,"monthly_tokens":0},
			 "requests_this_minute":3,"tokens_this_minute":1200,"tokens_today":640000,"tokens_this_month":5120000,
			 "minute_resets_at":1718960460,"day_resets_at":1719014400,"month_resets_at":1719792000,
			 "series":[{"day":1718841600,"tokens":420000,"requests":2},{"day":1718928000,"tokens":640000,"requests":3}],
			 "forecast":{"daily_exhaustion_at":1718999100,"monthly_exhaustion_at":null}},
			{"key_id":"k2","name":"svc","limits":{"rpm":0,"tpm":0,"daily_tokens":0,"monthly_tokens":0},
			 "requests_this_minute":0,"tokens_this_minute":0,"tokens_today":0,"tokens_this_month":0,
			 "minute_resets_at":0,"day_resets_at":0,"month_resets_at":0,
			 "series":[],"forecast":{"daily_exhaustion_at":null,"monthly_exhaustion_at":null}}
		],
		"pagination":{"next_cursor":null,"has_more":false}
	}`))
	defer srv.Close()

	got, err := newTestClient(t, srv).GetUsage(context.Background(), UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/admin/usage" {
		t.Fatalf("sent %s %s, want GET /v1/admin/usage", cap.method, cap.path)
	}

	if got.Summary.KeyCount != 2 || got.Summary.GlobalThrottled != 4 || got.Summary.KeyThrottled != 1 {
		t.Errorf("summary wrong: %+v", got.Summary)
	}
	if len(got.Data) != 2 {
		t.Fatalf("rows = %d, want 2", len(got.Data))
	}
	r0 := got.Data[0]
	if r0.KeyID != "k1" || r0.Owner != "alice" || r0.Team != "platform" ||
		r0.Limits.RPM != 60 || r0.Limits.DailyTokens != 1_000_000 ||
		r0.TokensToday != 640000 || r0.TokensThisMonth != 5_120_000 {
		t.Errorf("row[0] decoded wrong: %+v", r0)
	}
	if len(r0.Series) != 2 || r0.Series[1].Tokens != 640000 || r0.Series[1].Requests != 3 {
		t.Errorf("row[0] series wrong: %+v", r0.Series)
	}
	if r0.Forecast.DailyExhaustionAt == nil || *r0.Forecast.DailyExhaustionAt != 1718999100 {
		t.Errorf("row[0] daily forecast = %v, want 1718999100", r0.Forecast.DailyExhaustionAt)
	}
	if r0.Forecast.MonthlyExhaustionAt != nil {
		t.Errorf("row[0] monthly forecast = %v, want nil", r0.Forecast.MonthlyExhaustionAt)
	}
	// The unlimited/empty row keeps an empty (non-nil) series and no forecast.
	if got.Data[1].Series == nil || len(got.Data[1].Series) != 0 {
		t.Errorf("row[1] series = %+v, want empty non-nil", got.Data[1].Series)
	}
}

// TestListUsageFollowsCursor proves ListUsage follows next_cursor across pages and
// concatenates the rows, sending the filter as query parameters on every request.
func TestListUsageFollowsCursor(t *testing.T) {
	t.Parallel()
	var seenPaths []string
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if page == 0 {
			page++
			_, _ = io.WriteString(w, `{"summary":{"key_count":2,"global_throttled":0,"key_throttled":0},
				"data":[{"key_id":"a","name":"a","limits":{"rpm":0,"tpm":0,"daily_tokens":0,"monthly_tokens":0},
				"requests_this_minute":0,"tokens_this_minute":0,"tokens_today":0,"tokens_this_month":0,
				"minute_resets_at":0,"day_resets_at":0,"month_resets_at":0,"series":[],
				"forecast":{"daily_exhaustion_at":null,"monthly_exhaustion_at":null}}],
				"pagination":{"next_cursor":"MQ","has_more":true}}`)
			return
		}
		_, _ = io.WriteString(w, `{"summary":{"key_count":2,"global_throttled":0,"key_throttled":0},
			"data":[{"key_id":"b","name":"b","limits":{"rpm":0,"tpm":0,"daily_tokens":0,"monthly_tokens":0},
			"requests_this_minute":0,"tokens_this_minute":0,"tokens_today":0,"tokens_this_month":0,
			"minute_resets_at":0,"day_resets_at":0,"month_resets_at":0,"series":[],
			"forecast":{"daily_exhaustion_at":null,"monthly_exhaustion_at":null}}],
			"pagination":{"next_cursor":null,"has_more":false}}`)
	}))
	defer srv.Close()

	rows, err := newTestClient(t, srv).ListUsage(context.Background(), UsageFilter{Owner: "alice", Team: "platform"})
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	if len(rows) != 2 || rows[0].KeyID != "a" || rows[1].KeyID != "b" {
		t.Fatalf("rows = %+v, want [a b] across two pages", rows)
	}
	if len(seenPaths) != 2 {
		t.Fatalf("requests = %d, want 2 (cursor-followed)", len(seenPaths))
	}
	// The filter is sent as query parameters on the first request, and the cursor on
	// the second.
	if !strings.Contains(seenPaths[0], "owner=alice") || !strings.Contains(seenPaths[0], "team=platform") {
		t.Errorf("page1 query missing filters: %q", seenPaths[0])
	}
	if !strings.Contains(seenPaths[1], "cursor=MQ") {
		t.Errorf("page2 query missing cursor: %q", seenPaths[1])
	}
}

// TestListUsageForbidden proves the client maps a 403 (a token lacking
// telemetry:read) to the typed ErrForbidden sentinel.
func TestListUsageForbidden(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient scope","code":"forbidden"}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).ListUsage(context.Background(), UsageFilter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListUsage err = %v, want ErrForbidden", err)
	}
}
