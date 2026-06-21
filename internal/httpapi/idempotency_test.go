package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// withActor returns r with the given authenticated actor key stashed on its
// context, exactly as authMiddleware does in production. The idempotency
// middleware now namespaces its cache key by the actor (plus method + path), so
// a unit test that exercises s.idempotent directly must supply one or the
// middleware fails safe to a transparent pass-through (no caching).
func withActor(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey, store.APIKey{ID: id}))
}

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
		r = withActor(r, "actor-1")
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
		r = withActor(r, "actor-1")
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
		r = withActor(r, "actor-1")
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
		r = withActor(r, "actor-1")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("over-long key should not dedup; handler ran %d times, want 2", calls)
	}
}

// TestIdempotentNoActorFailsSafe proves the defensive fail-safe: if no
// authenticated actor is on the context (which should not happen behind the auth
// middleware), the middleware does NOT serve from or write to the shared cache —
// it passes straight through, so the handler runs every time. This guarantees a
// keyless request can never be served another actor's cached (token-bearing)
// reply.
func TestIdempotentNoActorFailsSafe(t *testing.T) {
	var calls int32
	s := &Server{}
	h := s.idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)
		r.Header.Set(idempotencyHeader, "foo")
		// Deliberately NO actor on the context.
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("missing actor should fail safe (pass through); handler ran %d times, want 2", calls)
	}
}

// TestIdempotentDistinctActorsDoNotShareCache is the security regression for the
// cold-review finding: the idempotency cache is server-wide, and POST
// /v1/admin/keys returns a one-time plaintext token in its 201 body. Two
// DIFFERENT admin actors sending the SAME client-chosen Idempotency-Key must each
// get their OWN newly-created key — the second actor must NOT be served the
// first's cached id/token. The composite cache key (actor id + method + path +
// header) confines a replay to the actor that produced it.
func TestIdempotentDistinctActorsDoNotShareCache(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	// Two distinct actors, each holding keys:write (the scope POST /keys needs).
	actorA := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysWrite}})
	actorB := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysWrite}})

	hdr := map[string]string{idempotencyHeader: "foo"}

	// Actor A creates a key with Idempotency-Key: foo.
	recA := reqWithHeaders(t, s, http.MethodPost, "/v1/admin/keys", actorA, `{"name":"a"}`, hdr)
	if recA.Code != http.StatusCreated {
		t.Fatalf("actor A create status = %d, want 201", recA.Code)
	}
	var createdA struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decode(t, recA, &createdA)
	if createdA.ID == "" || createdA.Token == "" {
		t.Fatalf("actor A create response missing id/token: %+v", createdA)
	}

	// Actor B sends the SAME Idempotency-Key. It must mint a DISTINCT key, not be
	// served A's cached reply.
	recB := reqWithHeaders(t, s, http.MethodPost, "/v1/admin/keys", actorB, `{"name":"b"}`, hdr)
	if recB.Code != http.StatusCreated {
		t.Fatalf("actor B create status = %d, want 201", recB.Code)
	}
	if recB.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatal("actor B was served a REPLAYED response — cross-actor cache leak")
	}
	var createdB struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decode(t, recB, &createdB)
	if createdB.ID == createdA.ID {
		t.Fatalf("cross-actor leak: actor B got actor A's key id %q", createdA.ID)
	}
	if createdB.Token == createdA.Token {
		t.Fatal("cross-actor leak: actor B was served actor A's one-time plaintext token")
	}
}

// TestIdempotentNoCrossPathReplay proves a single actor reusing one
// Idempotency-Key across DIFFERENT paths does not get one path's response
// replayed for another. The actor creates a key on POST /v1/admin/keys, then
// sends the same key on POST /v1/admin/workers/{id}/drain: the drain must
// actually run (204 + the id reaching the control plane), not replay the create's
// 201.
func TestIdempotentNoCrossPathReplay(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc := adminTestServer(t, fleet)
	// One actor holding both write scopes so it passes both routes.
	actor := mustKey(t, authSvc, auth.Permissions{
		AdminScopes: []string{authz.ScopeKeysWrite, authz.ScopeWorkersWrite},
	})

	hdr := map[string]string{idempotencyHeader: "foo"}

	// Create a key with Idempotency-Key: foo.
	rec := reqWithHeaders(t, s, http.MethodPost, "/v1/admin/keys", actor, `{"name":"x"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}

	// Same key, DIFFERENT path: drain a worker. Must execute (204), not replay 201.
	rec = reqWithHeaders(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", actor, "", hdr)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("drain status = %d, want 204 (cross-path replay of create suspected)", rec.Code)
	}
	if rec.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatal("drain was served a REPLAYED create response — cross-path cache leak")
	}
	if fleet.drained != "w1" {
		t.Fatalf("drain did not reach the control plane (drained=%q); create response was replayed instead", fleet.drained)
	}
}

// TestIdempotentSameActorSamePathReplays confirms the positive path still holds
// after the fix: the SAME actor sending the SAME Idempotency-Key to the SAME path
// within the TTL replays the first attempt (the key is minted once; the second
// call returns the cached 201 with the replay marker, and no second key is
// created).
func TestIdempotentSameActorSamePathReplays(t *testing.T) {
	s, authSvc := adminTestServer(t, &fakeFleet{})
	actor := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysWrite, authz.ScopeKeysRead}})

	hdr := map[string]string{idempotencyHeader: "foo"}

	first := reqWithHeaders(t, s, http.MethodPost, "/v1/admin/keys", actor, `{"name":"x"}`, hdr)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", first.Code)
	}
	second := reqWithHeaders(t, s, http.MethodPost, "/v1/admin/keys", actor, `{"name":"x"}`, hdr)
	if second.Code != http.StatusCreated {
		t.Fatalf("second create status = %d, want 201 (replay)", second.Code)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("second call should be a replay (Idempotency-Replayed: true)")
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replayed body differs: %q vs %q", first.Body.String(), second.Body.String())
	}

	// Exactly ONE key was created: the replay did not mint a second key. Filter to
	// the actor's own created keys (each mustKey above also created a key).
	var firstView struct {
		ID string `json:"id"`
	}
	decode(t, first, &firstView)
	listRec := reqWithHeaders(t, s, http.MethodGet, "/v1/admin/keys", actor, "", nil)
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	decode(t, listRec, &list)
	named := 0
	for _, k := range list.Data {
		if k.Name == "x" {
			named++
		}
	}
	if named != 1 {
		t.Fatalf("replay minted %d keys named \"x\", want exactly 1", named)
	}
}
