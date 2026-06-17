package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// Shared plumbing for the OpenAI-compatible inference endpoints
// (/v1/chat/completions and /v1/completions): id generation, the canonical
// error→HTTP-status mapping, and the OpenAI usage object. The two handlers in
// chat.go and completions.go build on this so the two surfaces map errors,
// IDs, and usage identically.

// usage is the OpenAI token-accounting object returned on a completion. The
// split comes from Ollama's prompt_eval_count/eval_count (#11); when the worker
// reports only a total (e.g. the echo stub), it lands in completion_tokens and
// total_tokens with prompt_tokens zero — see the package architecture notes.
type usage struct {
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
	TotalTokens      uint64 `json:"total_tokens"`
}

// usageFrom builds the OpenAI usage object from a job's reported token counts.
// PromptTokens/CompletionTokens are the Ollama split; Tokens is the quota total.
// total_tokens prefers the explicit split sum and falls back to the reported
// total so a backend that only reports a total still produces a coherent usage.
func usageFrom(prompt, completion, total uint64) usage {
	sum := prompt + completion
	if sum == 0 {
		// No split reported: surface the total as completion tokens so a caller
		// metering on usage sees the work, with the documented prompt=0 fallback.
		return usage{CompletionTokens: total, TotalTokens: total}
	}
	if total < sum {
		total = sum
	}
	return usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
}

// newID returns an OpenAI-style response id: prefix + random hex. crypto/rand
// makes it unguessable and globally unique without coordination. A rand failure
// is effectively impossible on a healthy host; if it ever occurs the id is left
// as the bare prefix rather than panicking on the request path.
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix
	}
	return prefix + hex.EncodeToString(b[:])
}

// jobIDFor returns the id to stamp on the dispatched job. It reuses the request
// correlation id (X-Request-Id, set by requestIDMiddleware) so request_id ==
// job_id: one id follows the request from the HTTP boundary, through the
// server's submit/placement logs, across the gRPC wire (Job.ID), to the worker's
// job-execution log line — making a single request traceable end-to-end (#23).
// If no correlation id is on the context (a handler exercised outside the
// middleware chain, e.g. some unit tests), it falls back to a freshly minted
// "job-" id so the job is still uniquely identified.
func jobIDFor(r *http.Request) string {
	if id, ok := requestIDFromContext(r.Context()); ok && id != "" {
		return id
	}
	return newID("job-")
}

// finishReasonOrStop normalizes a worker-reported finish_reason for the wire:
// an empty reason becomes "stop" so the OpenAI-required field is always set.
func finishReasonOrStop(r string) string {
	if r == "" {
		return "stop"
	}
	return r
}

// statusForError maps a submit-path error onto its HTTP status and a stable
// machine code, per the system error contract:
//
//   - auth.ErrUnauthenticated                       → 401
//   - authz.ErrForbidden                            → 403
//   - quota.ErrQuotaExceeded                        → 429
//   - queue.ErrQueueFull / server.ErrShuttingDown   → 503
//   - types.ErrInvalidJob                           → 400
//   - anything else                                 → 500
//
// The message is deliberately generic so no internal detail leaks; the typed
// code lets agent-gpu clients branch programmatically while OpenAI clients read
// the human message.
func statusForError(err error) (status int, code, msg string) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthorized", "invalid api key"
	case errors.Is(err, authz.ErrForbidden):
		return http.StatusForbidden, "forbidden", "not permitted to use this model"
	case errors.Is(err, quota.ErrQuotaExceeded):
		return http.StatusTooManyRequests, "rate_limit_exceeded", "quota exceeded"
	case errors.Is(err, queue.ErrQueueFull):
		return http.StatusServiceUnavailable, "unavailable", "no capacity available"
	case errors.Is(err, server.ErrShuttingDown):
		return http.StatusServiceUnavailable, "unavailable", "server shutting down"
	case errors.Is(err, types.ErrInvalidJob):
		return http.StatusBadRequest, "invalid_request_error", "invalid request"
	default:
		return http.StatusInternalServerError, "internal_error", "internal error"
	}
}

// writeSubmitError maps a submit-path error to its status/code and writes the
// JSON error envelope. It is used before the first byte of a response is
// written (both for non-streaming and for a streaming request whose gating
// failed before the stream began). Server-fault (500) errors are logged with
// the underlying cause; client-fault errors are not, to keep logs signal-rich.
//
// On a per-key quota 429 (quota.ErrQuotaExceeded) it also sets a Retry-After
// header (integer seconds, minimum 1) computed from the soonest exhausted
// window's reset for the request's authenticated key, increments the per-key
// throttle metric, and logs the throttle. The request r is threaded in so the
// key can be read from the context; a 429 without a known key (none on the
// context, or no quota engine) still returns 429 but omits Retry-After.
func (s *Server) writeSubmitError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := statusForError(err)
	if status == http.StatusInternalServerError {
		// Use the request-scoped logger so the failure carries request_id (==
		// job_id), correlating it with the rest of the request's server-side logs.
		s.reqLog(r.Context()).Error("inference submit failed", "err", err)
	}
	if status == http.StatusTooManyRequests && errors.Is(err, quota.ErrQuotaExceeded) {
		s.annotatePerKey429(w, r)
	}
	writeError(w, status, code, msg)
}

// annotatePerKey429 sets the Retry-After header for a per-key quota 429 and
// records the throttle. It reads the authenticated key from the request context
// and asks the quota engine when the key's soonest-exhausted window resets,
// computing seconds against the engine clock. If the key or engine is absent,
// or usage cannot be read, it records the throttle but omits Retry-After rather
// than emitting a misleading hint. It must be called before writeError so the
// header is set before the status line is written.
func (s *Server) annotatePerKey429(w http.ResponseWriter, r *http.Request) {
	s.incKeyThrottled()
	keyID := ""
	retryAfter := 0
	if key, ok := keyFromContext(r.Context()); ok && s.quota != nil {
		keyID = key.ID
		if snap, err := s.quota.UsageForKey(r.Context(), key); err == nil {
			if reset, ok := snap.RetryAfter(s.quotaNow()); ok {
				retryAfter = secondsUntil(reset, s.quotaNow())
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			}
		}
	}
	s.reqLog(r.Context()).Warn("request throttled",
		"scope", "key",
		"key_id", keyID,
		"retry_after", retryAfter,
	)
}

// decodePost requires POST and decodes the JSON request body into v. OpenAI
// clients send many optional fields agent-gpu ignores, so unknown fields are
// tolerated rather than 400'd. It returns false (after writing the appropriate
// error) on a wrong method or a malformed body so the caller can simply return.
func decodePost(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
		return false
	}
	return true
}
