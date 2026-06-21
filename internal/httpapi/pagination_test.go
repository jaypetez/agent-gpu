package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCursorRoundTrip proves an encoded cursor decodes back to its offset and a
// garbage cursor degrades to offset 0 (start over) rather than erroring.
func TestCursorRoundTrip(t *testing.T) {
	for _, off := range []int{0, 1, 50, 12345} {
		if got := decodeCursor(encodeCursor(off)); got != off {
			t.Errorf("round-trip %d → %d", off, got)
		}
	}
	for _, bad := range []string{"", "!!!notbase64!!!", "////"} {
		if got := decodeCursor(bad); got != 0 {
			t.Errorf("bad cursor %q decoded to %d, want 0", bad, got)
		}
	}
}

// TestParsePageParams proves limit clamping and cursor decoding from the query.
func TestParsePageParams(t *testing.T) {
	cases := []struct {
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"", defaultPageSize, 0},
		{"limit=10", 10, 0},
		{"limit=0", defaultPageSize, 0},   // non-positive → default
		{"limit=-5", defaultPageSize, 0},  // negative → default
		{"limit=99999", maxPageSize, 0},   // over cap → clamped
		{"limit=abc", defaultPageSize, 0}, // malformed → default
		{"cursor=" + encodeCursor(20), defaultPageSize, 20},
		{"limit=5&cursor=" + encodeCursor(7), 5, 7},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x?"+c.query, nil)
			limit, offset := parsePageParams(r)
			if limit != c.wantLimit || offset != c.wantOffset {
				t.Errorf("parsePageParams(%q) = (%d,%d), want (%d,%d)", c.query, limit, offset, c.wantLimit, c.wantOffset)
			}
		})
	}
}

// TestPaginateSlicing proves the page math: a middle page, the final partial
// page (no next cursor), and an offset past the end (empty page, no cursor).
func TestPaginateSlicing(t *testing.T) {
	items := []int{0, 1, 2, 3, 4} // 5 items

	page, next := paginate(items, 2, 0)
	if len(page) != 2 || page[0] != 0 || next == nil || decodeCursor(*next) != 2 {
		t.Fatalf("first page wrong: page=%v next=%v", page, next)
	}

	page, next = paginate(items, 2, 2)
	if len(page) != 2 || page[0] != 2 || next == nil || decodeCursor(*next) != 4 {
		t.Fatalf("middle page wrong: page=%v next=%v", page, next)
	}

	page, next = paginate(items, 2, 4)
	if len(page) != 1 || page[0] != 4 || next != nil {
		t.Fatalf("final page wrong: page=%v next=%v", page, next)
	}

	page, next = paginate(items, 2, 99)
	if len(page) != 0 || next != nil {
		t.Fatalf("past-end page should be empty with no cursor: page=%v next=%v", page, next)
	}
}

// TestWriteListEnvelope proves writeList emits the documented envelope shape with
// a non-null data array and the pagination footer.
func TestWriteListEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeList(rec, []string{"a", "b", "c"}, 2, 0)

	var env struct {
		Data       []string `json:"data"`
		Pagination struct {
			NextCursor *string `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	if len(env.Data) != 2 || env.Data[0] != "a" {
		t.Errorf("data = %v, want [a b]", env.Data)
	}
	if !env.Pagination.HasMore || env.Pagination.NextCursor == nil {
		t.Errorf("expected has_more with a next_cursor, got %+v", env.Pagination)
	}
}

// TestWriteListEmptyIsArray proves an empty result serializes data as [] (not
// null) and reports no further page.
func TestWriteListEmptyIsArray(t *testing.T) {
	rec := httptest.NewRecorder()
	writeList(rec, []string{}, defaultPageSize, 0)
	body := rec.Body.String()
	// data must be an empty array, and next_cursor null.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["data"]) != "[]" {
		t.Errorf("data = %s, want []", raw["data"])
	}
	var pg struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	}
	_ = json.Unmarshal(raw["pagination"], &pg)
	if pg.HasMore || pg.NextCursor != nil {
		t.Errorf("empty list should not report more: %+v", pg)
	}
}
