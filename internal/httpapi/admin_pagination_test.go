package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// keyListPage is the decoded list envelope for the keys endpoint.
type keyListPage struct {
	Data       []adminKeyView `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
}

// TestListKeysPaginated proves AC6: GET /v1/admin/keys returns the cursor-
// paginated envelope, the cursor walks the full set without gaps or duplicates,
// and the last page reports has_more=false with a null cursor.
func TestListKeysPaginated(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	// Seed 5 keys (plus the admin key itself = 6 total).
	for i := 0; i < 5; i++ {
		rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"k"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed create %d status = %d", i, rec.Code)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/v1/admin/keys?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := req(t, s, http.MethodGet, path, adminToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list status = %d", rec.Code)
		}
		var page keyListPage
		decode(t, rec, &page)
		pages++
		if len(page.Data) > 2 {
			t.Fatalf("page exceeded limit: %d", len(page.Data))
		}
		for _, k := range page.Data {
			if seen[k.ID] {
				t.Fatalf("duplicate key %s across pages", k.ID)
			}
			seen[k.ID] = true
		}
		if page.Pagination.NextCursor == nil {
			if page.Pagination.HasMore {
				t.Errorf("last page reported has_more with null cursor")
			}
			break
		}
		if !page.Pagination.HasMore {
			t.Errorf("non-last page reported has_more=false")
		}
		cursor = *page.Pagination.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 6 {
		t.Fatalf("walked %d distinct keys, want 6", len(seen))
	}
	if pages != 3 {
		t.Errorf("walked %d pages of size 2 over 6 keys, want 3", pages)
	}
}

// TestListKeysDefaultPage proves a request without ?limit/?cursor returns the
// whole (small) set in one page with no further cursor.
func TestListKeysDefaultPage(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	var page keyListPage
	decode(t, rec, &page)
	if len(page.Data) != 1 || page.Pagination.NextCursor != nil || page.Pagination.HasMore {
		t.Fatalf("default page wrong: data=%d pagination=%+v", len(page.Data), page.Pagination)
	}
}

// TestListWorkersPaginated proves the worker list also uses the envelope and
// paginates, with workers stably ordered by id.
func TestListWorkersPaginated(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{
		{ID: "w3"}, {ID: "w1"}, {ID: "w2"},
	}}
	s, authSvc := adminTestServer(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/workers?limit=2", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list workers status = %d", rec.Code)
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Pagination struct {
			NextCursor *string `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		} `json:"pagination"`
	}
	decode(t, rec, &page)
	// Stable sort by id → first page is w1, w2 with a further page for w3.
	if len(page.Data) != 2 || page.Data[0].ID != "w1" || page.Data[1].ID != "w2" {
		t.Fatalf("first worker page wrong: %+v", page.Data)
	}
	if page.Pagination.NextCursor == nil || !page.Pagination.HasMore {
		t.Fatalf("expected a further page: %+v", page.Pagination)
	}

	rec = req(t, s, http.MethodGet, "/v1/admin/workers?limit=2&cursor="+*page.Pagination.NextCursor, adminToken, "")
	decode(t, rec, &page)
	if len(page.Data) != 1 || page.Data[0].ID != "w3" || page.Pagination.NextCursor != nil {
		t.Fatalf("second worker page wrong: data=%+v pagination=%+v", page.Data, page.Pagination)
	}
}

// TestIdempotentCreateThroughRouter proves AC5 end-to-end through the routed
// admin server: two POSTs with the same Idempotency-Key create ONE key and the
// second replays the first response (same id, same one-time token).
func TestIdempotentCreateThroughRouter(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	hdr := map[string]string{idempotencyHeader: "create-once-123"}
	doCreate := func() (*httptest.ResponseRecorder, string) {
		rec := reqWithHeaders(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"once"}`, hdr)
		var out struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		decode(t, rec, &out)
		return rec, out.ID + "|" + out.Token
	}

	first, firstBody := doCreate()
	second, secondBody := doCreate()

	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("status: first=%d second=%d", first.Code, second.Code)
	}
	if firstBody != secondBody {
		t.Fatalf("idempotent replay returned a different key: %q vs %q", firstBody, secondBody)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("second create should be a replay (Idempotency-Replayed header)")
	}

	// Exactly one key was created (the duplicate did not mint a second).
	rec := req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	var page keyListPage
	decode(t, rec, &page)
	count := 0
	for _, k := range page.Data {
		if k.Name == "once" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("idempotent create minted %d keys named once, want 1", count)
	}
}
