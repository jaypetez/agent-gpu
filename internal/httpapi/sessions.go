package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jaypetez/agent-gpu/internal/session"
)

// The session CRUD surface (#36): create a server-side conversation, read its
// metadata + history, and end+purge it. Every endpoint is behind authMiddleware
// and owner-scoped by the authenticated key id (keyFromContext): a session is
// only ever visible to, or mutable by, the key that created it. A request for a
// session that is missing, owned by another key, or already expired yields 404
// uniformly, so the API never leaks the existence of another owner's session
// (mirroring session.ErrSessionNotFound's deliberate ambiguity).
//
// These endpoints power the stateful chat mode in chat.go: a client creates a
// session here, then sends only new turns referencing it by its body session_id,
// and the server reconstructs the full context from the stored history.

// createSessionRequest is the POST /v1/sessions body. Only the target model is
// required; everything else (TTL, caps) is server policy.
type createSessionRequest struct {
	Model string `json:"model"`
}

// sessionResponse is the JSON shape returned by POST /v1/sessions: the minimal
// handle a client needs to start sending turns. created is a unix timestamp for
// parity with the OpenAI object envelopes used elsewhere.
type sessionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
}

// sessionDetailResponse is the JSON shape returned by GET /v1/sessions/{id}: the
// session metadata plus its full conversation history rendered in the OpenAI
// message wire shape, so a retrieved session can be inspected or replayed.
type sessionDetailResponse struct {
	ID         string        `json:"id"`
	Object     string        `json:"object"`
	Model      string        `json:"model"`
	Created    int64         `json:"created"`
	LastActive int64         `json:"last_active"`
	Messages   []chatMessage `json:"messages"`
}

// handleCreateSession serves POST /v1/sessions. It mints a new owner-scoped
// session targeting the requested model and returns its id. Auth has already
// happened; the owner is the authenticated key id.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	key, ok := keyFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}
	if !s.sessionsEnabled(w) {
		return
	}

	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
		return
	}

	sess, err := s.sessionMgr.Create(r.Context(), key.ID, req.Model)
	if err != nil {
		s.reqLog(r.Context()).Error("session create failed", "key_id", key.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create session")
		return
	}

	writeJSON(w, http.StatusCreated, sessionResponse{
		ID:      sess.ID,
		Object:  "session",
		Model:   sess.Model,
		Created: sess.CreatedAt.Unix(),
	})
}

// handleGetSession serves GET /v1/sessions/{id}. It returns the owner's session
// metadata and full history. A missing/not-owned/expired session yields 404, no
// existence leak.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	key, ok := keyFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}
	if !s.sessionsEnabled(w) {
		return
	}

	id := r.PathValue("id")
	sess, err := s.sessionMgr.Get(r.Context(), id, key.ID)
	if err != nil {
		s.writeSessionLookupError(r, w, err)
		return
	}
	hist, err := s.sessionMgr.History(r.Context(), id, key.ID)
	if err != nil {
		// The session existed a moment ago; a not-found here means it was deleted
		// or expired between the two reads — treat it the same as a missing session.
		s.writeSessionLookupError(r, w, err)
		return
	}

	writeJSON(w, http.StatusOK, sessionDetailResponse{
		ID:         sess.ID,
		Object:     "session",
		Model:      sess.Model,
		Created:    sess.CreatedAt.Unix(),
		LastActive: sess.LastActiveAt.Unix(),
		Messages:   fromDomainMessages(hist),
	})
}

// handleDeleteSession serves DELETE /v1/sessions/{id}. It ends the owner's
// session and purges its history, returning 204. A missing/not-owned session
// yields 404, no existence leak.
//
// On a successful delete it best-effort asks the session's bound worker to unload
// the conversation's model, freeing its VRAM promptly rather than after the
// keep_alive window (#35). The bound worker + model are read BEFORE the delete
// (which erases them); the unload is fire-and-forget and never affects the 204
// the client receives — Ollama's idle keep_alive timer is the backstop release
// path if the worker is gone or the message is dropped.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	key, ok := keyFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}
	if !s.sessionsEnabled(w) {
		return
	}

	id := r.PathValue("id")
	// Capture the bound worker + model before deleting, so we know where to send
	// the unload. A lookup failure is non-fatal: Delete below produces the
	// authoritative 404 for a missing/not-owned session.
	var boundWorker, model string
	if sess, err := s.sessionMgr.Get(r.Context(), id, key.ID); err == nil {
		boundWorker, model = sess.BoundWorkerID, sess.Model
	}
	if err := s.sessionMgr.Delete(r.Context(), id, key.ID); err != nil {
		s.writeSessionLookupError(r, w, err)
		return
	}
	s.unloadSessionModel(r.Context(), boundWorker, model)
	w.WriteHeader(http.StatusNoContent)
}

// modelUnloader is the optional engine capability used to release a session's
// model on its bound worker (#35). *server.Server satisfies it
// (UnloadSessionModel); the chat/completions handlers' inferenceEngine does NOT
// require it, so test fakes that only exercise inference are unaffected — the
// delete handler simply skips the unload when the engine does not implement it.
type modelUnloader interface {
	UnloadSessionModel(ctx context.Context, workerID, model string)
}

// unloadSessionModel best-effort releases an ended session's model on its bound
// worker, when both are known and the engine supports unloading. It is a no-op
// otherwise (an unbound session, a model-less session, or an engine without the
// capability), so the keep_alive idle timer remains the release path in those
// cases. It never blocks or fails the delete response.
func (s *Server) unloadSessionModel(ctx context.Context, workerID, model string) {
	if workerID == "" || model == "" {
		return
	}
	if u, ok := s.engine.(modelUnloader); ok {
		u.UnloadSessionModel(ctx, workerID, model)
	}
}

// sessionsEnabled reports whether the session subsystem is wired in. When it is
// not (sessionMgr nil — only possible in unit tests, never in cmd), it writes a
// 501 and returns false so the caller returns. This keeps the session endpoints
// from panicking on a nil manager and gives a clear, documented signal.
func (s *Server) sessionsEnabled(w http.ResponseWriter) bool {
	if s.sessionMgr == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "sessions are not enabled")
		return false
	}
	return true
}

// writeSessionLookupError maps a session lookup/mutation error to its HTTP
// status. ErrSessionNotFound (missing, not-owned, or expired) is a uniform 404
// so existence never leaks across owners; anything else is a server fault (500)
// and is logged with the underlying cause via the request-scoped logger (so the
// line carries request_id).
func (s *Server) writeSessionLookupError(r *http.Request, w http.ResponseWriter, err error) {
	if errors.Is(err, session.ErrSessionNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	s.reqLog(r.Context()).Error("session lookup failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
}
