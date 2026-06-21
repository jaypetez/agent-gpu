package httpapi

import (
	"encoding/base64"
	"net/http"
	"strconv"
)

// Cursor pagination + the list envelope (#90) are the uniform shape every admin
// list endpoint returns:
//
//	{"data":[...], "pagination":{"next_cursor": <string|null>, "has_more": <bool>}}
//
// Offset pagination is deliberately NOT used: a cursor is stable under
// concurrent inserts/deletes (it encodes a position in a stably-sorted slice, so
// a page boundary does not skip or duplicate items the way a raw offset can when
// the underlying set shifts). The cursor here is an opaque base64 of the
// integer offset into the caller's stably-sorted slice — opaque so clients
// treat it as a token and never construct one, while staying trivial to decode
// server-side.

// listEnvelope is the wire shape of every admin list response. Data is always a
// non-nil JSON array (never null) so a client can iterate without a nil guard.
type listEnvelope struct {
	Data       any            `json:"data"`
	Pagination paginationMeta `json:"pagination"`
}

// paginationMeta is the cursor-pagination footer. NextCursor is an opaque token
// to pass as ?cursor= on the next request, or null when this is the last page.
// HasMore is the convenience boolean (true exactly when NextCursor is non-null).
type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

const (
	// defaultPageSize is the page size when the client supplies no ?limit=.
	defaultPageSize = 50
	// maxPageSize caps ?limit= so a client cannot request an unbounded page.
	maxPageSize = 200
)

// parsePageParams reads the ?limit= and ?cursor= query parameters, returning the
// resolved page size (clamped to [1, maxPageSize], defaulting to defaultPageSize)
// and the decoded start offset. A malformed limit falls back to the default; a
// malformed/garbage cursor decodes to offset 0 (the first page) rather than
// erroring, so a stale or hand-edited token degrades gracefully to "start over"
// instead of failing the request.
func parsePageParams(r *http.Request) (limit, offset int) {
	limit = defaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	offset = decodeCursor(r.URL.Query().Get("cursor"))
	return limit, offset
}

// encodeCursor renders an integer offset as an opaque base64 token. It is the
// inverse of decodeCursor; a caller never inspects the result.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// decodeCursor parses an opaque cursor token back into an offset. An empty,
// malformed, or negative token yields 0 (the first page) so a bad token never
// errors the request — it just restarts pagination.
func decodeCursor(cursor string) int {
	if cursor == "" {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// paginate slices items[offset:offset+limit] and reports the next cursor.
// offset beyond len(items) yields an empty page with no next cursor. It is the
// single place the page math lives, so every list endpoint paginates
// identically. items must already be in a stable sort order.
func paginate[T any](items []T, limit, offset int) (page []T, next *string) {
	n := len(items)
	if offset < 0 {
		offset = 0
	}
	if offset >= n {
		return []T{}, nil
	}
	end := offset + limit
	if end > n {
		end = n
	}
	page = items[offset:end]
	if end < n {
		c := encodeCursor(end)
		next = &c
	}
	return page, next
}

// writeList writes a paginated list envelope: it slices the stably-sorted items
// for the requested page and emits {"data":[...],"pagination":{...}} with the
// next cursor and has_more derived from whether a further page exists. data is
// always a JSON array (the page is a non-nil slice). It is shared by every admin
// list handler so the envelope shape is uniform.
func writeList[T any](w http.ResponseWriter, items []T, limit, offset int) {
	page, next := paginate(items, limit, offset)
	writeJSON(w, http.StatusOK, listEnvelope{
		Data: page,
		Pagination: paginationMeta{
			NextCursor: next,
			HasMore:    next != nil,
		},
	})
}
