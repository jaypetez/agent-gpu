package httpapi

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// The Idempotency-Key middleware (#90) makes admin WRITES safe to retry: a
// client that resends a request with the same Idempotency-Key within a TTL gets
// back the exact response the first attempt produced (status + body), instead of
// the write running twice. This protects against duplicate key creation, double
// revoke/rotate, etc. on a flaky network where a client retries without knowing
// whether the first attempt landed.
//
// The cache is in-memory, mutex-guarded, and bounded by a TTL with lazy
// eviction. A request WITHOUT the header is unaffected (the middleware is a
// transparent pass-through), so the header is purely opt-in. Only successful
// 2xx responses are cached as the canonical result; a write that failed (4xx/5xx)
// is not cached, so a client may legitimately retry it (the failure was not the
// committed outcome).
//
// Scope (deliberately simple, per #90): the cache deduplicates SEQUENTIAL
// retries — the overwhelmingly common case where a client resends after a
// timeout. It does not serialize two requests with the same key that are
// genuinely in flight at the same instant (both may miss the cache and execute);
// suppressing that would require per-key in-flight locking, which is out of
// scope here. The store-level operations remain correct either way.

// idempotencyHeader is the request header carrying the client-chosen
// idempotency key. It mirrors the de-facto industry standard (Stripe et al.).
const idempotencyHeader = "Idempotency-Key"

// maxIdempotencyKeyLen bounds an accepted key so a client cannot bloat the cache
// with an arbitrarily long key. It is comfortably above any sane UUID/ULID.
const maxIdempotencyKeyLen = 255

// defaultIdempotencyTTL is how long a cached response is replayed for a repeated
// key. It is long enough to cover a client's retry window but short enough that
// the cache stays small.
const defaultIdempotencyTTL = 24 * time.Hour

// idempotencyCacheMax bounds the number of cached responses so the in-memory map
// cannot grow without limit. It is generous for an admin API's write volume; the
// oldest entries are dropped (whole-map clear) when exceeded.
const idempotencyCacheMax = 4096

// cachedResponse is the captured result of a first attempt: the status code, the
// response body, and the Content-Type to replay. Stored by idempotency key.
type cachedResponse struct {
	status      int
	body        []byte
	contentType string
	expires     time.Time
}

// idempotencyCache is a bounded, TTL-expiring map of idempotency key → captured
// response. It is safe for concurrent use. Eviction is lazy (on access) plus a
// hard cap that drops the whole map when exceeded — the simplest correct bound
// for the modest write volume an admin API sees; a smarter LRU is unwarranted.
type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]cachedResponse
	ttl     time.Duration
	max     int
	now     func() time.Time
}

// newIdempotencyCache returns a cache with the given TTL and entry cap. A
// non-positive ttl falls back to the default; a non-positive max disables the
// hard cap (TTL eviction still applies).
func newIdempotencyCache(ttl time.Duration, max int) *idempotencyCache {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	return &idempotencyCache{
		entries: make(map[string]cachedResponse),
		ttl:     ttl,
		max:     max,
		now:     time.Now,
	}
}

// get returns the cached response for key if present and unexpired. An expired
// entry is evicted on access and reported as a miss.
func (c *idempotencyCache) get(key string) (cachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[key]
	if !ok {
		return cachedResponse{}, false
	}
	if !c.now().Before(r.expires) {
		delete(c.entries, key)
		return cachedResponse{}, false
	}
	return r, true
}

// put stores resp under key with a fresh TTL. If the hard cap is exceeded it
// drops every expired entry first, and if still over, clears the map — a blunt
// but safe bound that never grows without limit.
func (c *idempotencyCache) put(key string, resp cachedResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp.expires = c.now().Add(c.ttl)
	if c.max > 0 && len(c.entries) >= c.max {
		now := c.now()
		for k, v := range c.entries {
			if !now.Before(v.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			c.entries = make(map[string]cachedResponse, c.max)
		}
	}
	c.entries[key] = resp
}

// idempotencyRecorder buffers a handler's response so the middleware can both
// forward it to the client AND cache it for replay. It captures the status and
// body; the first WriteHeader (or first Write) wins, mirroring net/http.
type idempotencyRecorder struct {
	http.ResponseWriter
	status      int
	buf         bytes.Buffer
	wroteHeader bool
}

func (r *idempotencyRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *idempotencyRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// idempotent wraps a write handler so a repeated Idempotency-Key within the TTL
// replays the first attempt's response instead of re-running the handler. A
// request without the header passes straight through. Only a 2xx response is
// cached: a failed attempt is not the committed outcome, so the client may retry
// it. It is applied to admin write routes alongside the scope gate.
//
// The cache is keyed by the COMPOSITE of (authenticated actor key id, HTTP
// method, request path, raw header value), not the bare client-supplied header.
// The cache instance (s.idempotency) is server-wide and shared across every
// admin write route, and create/rotate return a one-time plaintext token in the
// 201 body; keying on the bare header alone would let one actor's reply (id +
// plaintext token) be served to a DIFFERENT actor — or to a different route —
// that happened to send the same client-chosen, non-secret key. Namespacing by
// actor + method + path confines a replay to the exact actor and request that
// produced it, which is the only correct dedup target. (This is stricter than
// the documented "concurrent same-key" simplification, which only excuses
// genuinely concurrent races — not cross-actor/cross-path reuse.)
func (s *Server) idempotent(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cache := s.idempotencyCache()
		rawKey := r.Header.Get(idempotencyHeader)
		// The over-long guard applies to the RAW client value: a client cannot bloat
		// the cache with an arbitrarily long key (the composite key adds only the
		// bounded actor id, method, and path).
		if rawKey == "" || len(rawKey) > maxIdempotencyKeyLen {
			// No key (or an over-long one we refuse to cache): behave exactly as if
			// the middleware were absent. An over-long key still executes the write;
			// it simply is not deduplicated.
			h.ServeHTTP(w, r)
			return
		}
		// Read the authenticated actor the auth middleware stashed (idempotent runs
		// inside requireScopeWrite, i.e. behind authMiddleware, so a key is present).
		// Fail safe if absent: do NOT serve from or write to the shared cache — just
		// pass through. This is defensive and should not happen in production.
		actor, ok := keyFromContext(r.Context())
		if !ok {
			h.ServeHTTP(w, r)
			return
		}
		// Compose the cache key from (actor id, method, path, raw header value),
		// joined by NUL — a byte that cannot occur in an HTTP method, URL path, or
		// header value — so the parts are unambiguous and cannot be confused across
		// actors or routes. r.URL.Path is the concrete path (it includes any {id}).
		key := actor.ID + "\x00" + r.Method + "\x00" + r.URL.Path + "\x00" + rawKey

		if cached, ok := cache.get(key); ok {
			if cached.contentType != "" {
				w.Header().Set("Content-Type", cached.contentType)
			}
			// Mark the replay so a client can tell a cached response from a fresh one.
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(cached.status)
			_, _ = w.Write(cached.body)
			return
		}

		rec := &idempotencyRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)

		// Cache only a committed (2xx) outcome; a 4xx/5xx is safe to retry and must
		// not be replayed as if it had succeeded.
		if rec.status >= 200 && rec.status < 300 {
			cache.put(key, cachedResponse{
				status:      rec.status,
				body:        append([]byte(nil), rec.buf.Bytes()...),
				contentType: rec.Header().Get("Content-Type"),
			})
		}
	})
}

// idempotencyCache returns the server's idempotency cache, lazily constructing
// one (once) when the Server was built via a struct literal that left it nil
// (some unit tests). The sync.Once makes the lazy init race-free under the
// -race detector when concurrent requests hit a struct-literal server; the
// production path sets the field in NewServer, where the Once's first Do simply
// observes the already-set field and returns it.
func (s *Server) idempotencyCache() *idempotencyCache {
	s.idempotencyOnce.Do(func() {
		if s.idempotency == nil {
			s.idempotency = newIdempotencyCache(defaultIdempotencyTTL, idempotencyCacheMax)
		}
	})
	return s.idempotency
}
