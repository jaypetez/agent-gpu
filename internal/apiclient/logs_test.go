package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLogs proves the client decodes the GET /v1/admin/logs page into typed
// LogEntry rows, sends the filters as query parameters, and exposes the structured
// attrs as a discrete map (including the redacted marker passed through verbatim).
func TestLogs(t *testing.T) {
	t.Parallel()
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data":[
				{"time":"2026-03-04T05:06:47Z","level":"ERROR","message":"dispatch failed","attrs":{"request_id":"r2","worker":"w2","token":"[REDACTED]"}},
				{"time":"2026-03-04T05:06:37Z","level":"WARN","message":"slow worker","attrs":{"worker":"w1"}}
			],
			"pagination":{"next_cursor":null,"has_more":false}
		}`)
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv).Logs(context.Background(), LogFilter{
		Level:     "warn",
		RequestID: "r2",
		Worker:    "w2",
		Since:     time.Unix(1000, 0),
		Until:     time.Unix(2000, 0),
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].Level != "ERROR" || got[0].Message != "dispatch failed" {
		t.Errorf("row[0] decoded wrong: %+v", got[0])
	}
	if got[0].Attrs["request_id"] != "r2" || got[0].Attrs["worker"] != "w2" {
		t.Errorf("row[0] attrs decoded wrong: %+v", got[0].Attrs)
	}
	// The redacted marker is carried through verbatim (the client never un-redacts).
	if got[0].Attrs["token"] != "[REDACTED]" {
		t.Errorf("row[0] redacted attr = %v, want [REDACTED]", got[0].Attrs["token"])
	}

	// The filter is sent as query parameters: level, request_id, worker, and the
	// unix-seconds time bounds, alongside the page limit.
	for _, want := range []string{"limit=200", "level=warn", "request_id=r2", "worker=w2", "since=1000", "until=2000"} {
		if !strings.Contains(seenPath, want) {
			t.Errorf("request path %q missing %q", seenPath, want)
		}
	}
}

// TestLogsFollowsCursor proves Logs follows next_cursor across pages and assembles
// the full set, sending the cursor on the second request.
func TestLogsFollowsCursor(t *testing.T) {
	t.Parallel()
	var seenPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"data":[{"time":"2026-03-04T05:06:47Z","level":"ERROR","message":"one","attrs":{}}],
				"pagination":{"next_cursor":"MQ","has_more":true}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"time":"2026-03-04T05:06:37Z","level":"WARN","message":"two","attrs":{}}],
			"pagination":{"next_cursor":null,"has_more":false}}`)
	}))
	defer srv.Close()

	rows, err := newTestClient(t, srv).Logs(context.Background(), LogFilter{Level: "warn"})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(rows) != 2 || rows[0].Message != "one" || rows[1].Message != "two" {
		t.Fatalf("assembled rows wrong: %+v", rows)
	}
	if len(seenPaths) != 2 {
		t.Fatalf("requests = %d, want 2 (cursor-followed)", len(seenPaths))
	}
	if !strings.Contains(seenPaths[1], "cursor=MQ") {
		t.Errorf("page2 query missing cursor: %q", seenPaths[1])
	}
}

// TestLogsForbidden proves the client maps a 403 (a token lacking logs:read) to
// the typed ErrForbidden sentinel.
func TestLogsForbidden(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient scope","code":"forbidden"}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).Logs(context.Background(), LogFilter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Logs err = %v, want ErrForbidden", err)
	}
}
