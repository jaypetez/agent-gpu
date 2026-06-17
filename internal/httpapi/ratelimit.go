package httpapi

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jaypetez/agent-gpu/internal/quota"
)

// Rate limiting (#6): a server-wide (global) limiter fronting the inference
// surface, plus a Retry-After hint on every 429 and throttle metrics.
//
// Two scopes are enforced, in order, before a job is ever dispatched:
//
//   - GLOBAL: rateLimitMiddleware reserves one unit against the engine's
//     server-wide counter (quota.CheckAndReserveGlobal). It protects the whole
//     fleet from aggregate overload regardless of which key is calling, and runs
//     before the handler so a global 429 never touches per-key counters or the
//     dispatcher.
//   - PER-KEY: the existing submit path (server.SubmitAuthorizedJob[Stream] →
//     quota.CheckAndReserve) enforces each key's own quota and surfaces 429s
//     through writeSubmitError. #6 only adds the Retry-After header there; the
//     counting/reset internals are owned by the quota engine (#5).
//
// Both scopes return HTTP 429 with code "rate_limit_exceeded" and a Retry-After
// header (integer seconds, minimum 1) derived from the relevant window's reset,
// computed against the engine clock so it is deterministic under an injected
// clock. Throttle counts are exposed via RateLimitStats for the metrics epic
// (#24). No secrets or tokens are ever logged or surfaced.

// RateLimitStats is an observable snapshot of throttling (#6): GlobalThrottled
// counts requests rejected by the global limiter; KeyThrottled counts per-key
// quota 429s. It mirrors server.AffinityStats / QueueStats as the throttle
// metrics seam for #24 (Prometheus) until that lands.
type RateLimitStats struct {
	GlobalThrottled uint64
	KeyThrottled    uint64
}

// RateLimitStats returns a point-in-time snapshot of the throttle counters.
func (s *Server) RateLimitStats() RateLimitStats {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	return RateLimitStats{GlobalThrottled: s.globalThrottled, KeyThrottled: s.keyThrottled}
}

// rateLimitMiddleware enforces the server-wide (global) rate limit on the
// inference routes. It runs after authMiddleware (the authenticated key is on
// the context, used only for the throttle log — never to re-check per-key
// quota) and before the handler. On a global denial it writes a 429 with a
// Retry-After header and returns without calling next, so neither the per-key
// quota counter nor the dispatcher is touched. When the engine has no global
// limit configured (the default), CheckAndReserveGlobal short-circuits to nil
// and every request falls straight through to next — byte-identical to the
// pre-#6 path. A nil quota engine (some unit tests) is treated as unlimited.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.quota == nil {
			next.ServeHTTP(w, r)
			return
		}
		if err := s.quota.CheckAndReserveGlobal(r.Context()); err != nil {
			if errors.Is(err, quota.ErrQuotaExceeded) {
				retryAfter := secondsUntil(s.quota.GlobalMinuteReset(), s.quotaNow())
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				s.incGlobalThrottled()
				keyID := ""
				if key, ok := keyFromContext(r.Context()); ok {
					keyID = key.ID
				}
				s.reqLog(r.Context()).Warn("request throttled",
					"scope", "global",
					"key_id", keyID,
					"retry_after", retryAfter,
				)
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded")
				return
			}
			// Any other error from the global limiter is a server fault (e.g. a
			// counter-store failure); fail closed with a 500 rather than admitting.
			s.reqLog(r.Context()).Error("global rate limit check failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// incGlobalThrottled / incKeyThrottled bump the throttle counters under rlMu.
func (s *Server) incGlobalThrottled() {
	s.rlMu.Lock()
	s.globalThrottled++
	s.rlMu.Unlock()
}

func (s *Server) incKeyThrottled() {
	s.rlMu.Lock()
	s.keyThrottled++
	s.rlMu.Unlock()
}

// quotaNow reads the engine clock so Retry-After is computed against the same
// time source the windows reset on (deterministic under an injected clock). It
// guards against a nil engine (returning the wall clock) so callers need not.
func (s *Server) quotaNow() time.Time {
	if s.quota == nil {
		return time.Now()
	}
	return s.quota.Now()
}

// secondsUntil returns the whole seconds from now until reset, rounded up and
// clamped to a minimum of 1 so a Retry-After is always a positive integer (a
// client must wait at least a moment, and 0 would invite an immediate retry
// storm). A reset at or before now still yields 1.
func secondsUntil(reset, now time.Time) int {
	d := reset.Sub(now)
	if d <= 0 {
		return 1
	}
	secs := math.Ceil(d.Seconds())
	if secs < 1 {
		return 1
	}
	return int(secs)
}
