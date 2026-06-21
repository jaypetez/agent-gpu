package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestIdempotencyCacheGetPut proves a put is retrievable until it expires.
func TestIdempotencyCacheGetPut(t *testing.T) {
	c := newIdempotencyCache(time.Hour, 0)
	if _, ok := c.get("k"); ok {
		t.Fatal("empty cache should miss")
	}
	c.put("k", cachedResponse{status: 201, body: []byte("x")})
	got, ok := c.get("k")
	if !ok || got.status != 201 || string(got.body) != "x" {
		t.Fatalf("get after put = %+v ok=%v", got, ok)
	}
}

// TestIdempotencyCacheExpiry proves an entry past its TTL is a miss and is
// evicted on access.
func TestIdempotencyCacheExpiry(t *testing.T) {
	now := time.Now()
	c := newIdempotencyCache(time.Minute, 0)
	c.now = func() time.Time { return now }
	c.put("k", cachedResponse{status: 200})
	// Advance past the TTL.
	now = now.Add(2 * time.Minute)
	if _, ok := c.get("k"); ok {
		t.Fatal("expired entry should miss")
	}
}

// TestIdempotencyCacheCapClears proves the hard cap bounds the map: exceeding it
// (with no expired entries to reclaim) clears the map rather than growing.
func TestIdempotencyCacheCapClears(t *testing.T) {
	c := newIdempotencyCache(time.Hour, 2)
	c.put("a", cachedResponse{status: 200})
	c.put("b", cachedResponse{status: 200})
	// Third put exceeds the cap of 2 with no expired entries; the map is cleared
	// then the new entry stored.
	c.put("c", cachedResponse{status: 200})
	if len(c.entries) != 1 {
		t.Fatalf("cap clear left %d entries, want 1", len(c.entries))
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("the just-put entry should survive the cap clear")
	}
}

// TestIdempotentReplaysWrite proves the middleware replays the first attempt's
// response for a duplicate Idempotency-Key, running the handler only once (AC5).
func TestIdempotentReplaysWrite(t *testing.T) {
	var calls int32
	s := &Server{}
	h := s.idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"k1"}`))
	}))

	doReq := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)
		if key != "" {
			r.Header.Set(idempotencyHeader, key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	first := doReq("key-123")
	second := doReq("key-123")

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("handler ran %d times, want 1 (second should replay)", calls)
	}
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("status mismatch: first=%d second=%d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replayed body differs: %q vs %q", first.Body.String(), second.Body.String())
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay should set Idempotency-Replayed header")
	}
	if second.Header().Get("Content-Type") != "application/json" {
		t.Errorf("replay should restore Content-Type")
	}
}

// TestIdempotentDistinctKeysRunSeparately proves different keys each execute the
// handler (no cross-key collision).
func TestIdempotentDistinctKeysRunSeparately(t *testing.T) {
	var calls int32
	s := &Server{}
	h := s.idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	for _, key := range []string{"a", "b"} {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.Header.Set(idempotencyHeader, key)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("distinct keys ran handler %d times, want 2", calls)
	}
}

// TestIdempotentNoHeaderAlwaysRuns proves a request without the header is a
// transparent pass-through (the write runs every time).
func TestIdempotentNoHeaderAlwaysRuns(t *testing.T) {
	var calls int32
	s := &Server{}
	h := s.idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("no-header ran handler %d times, want 3", calls)
	}
}

// TestIdempotentDoesNotCacheFailures proves a failed (non-2xx) response is not
// replayed: a client may legitimately retry it because the failure was not the
// committed outcome.
func TestIdempotentDoesNotCacheFailures(t *testing.T) {
	var calls int32
	s := &Server{}
	h := s.idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.Header.Set(idempotencyHeader, "k")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("failures should not be cached; handler ran %d times, want 2", calls)
	}
}

// TestIdempotentOverLongKeyNotCached proves an over-long key still executes the
// write but is not deduplicated (it is simply not cached).
func TestIdempotentOverLongKeyNotCached(t *testing.T) {
	var calls int32
	s := &Server{}
	h := s.idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	long := make([]byte, maxIdempotencyKeyLen+1)
	for i := range long {
		long[i] = 'a'
	}
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.Header.Set(idempotencyHeader, string(long))
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("over-long key should not dedup; handler ran %d times, want 2", calls)
	}
}
