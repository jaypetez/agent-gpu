package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// ---- request shape ----

// chatCompletionRequest is the subset of the OpenAI chat/completions request
// agent-gpu acts on. Unknown fields (top_p, presence_penalty, …) are accepted
// and ignored: the inference parameters not yet plumbed to Ollama are a
// documented seam, not a rejection. model, messages, stream, and tools are the
// fields that change behavior today.
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []chatTool    `json:"tools"`
	// SessionID selects STATEFUL mode (#36): the server prepends the session's
	// stored history to the request's messages before dispatch and persists the
	// new turn(s) + the assistant reply afterward. It is the body counterpart of
	// the X-Session-Id header (AFFINITY mode); see resolveSession for how the two
	// are reconciled when both are present.
	SessionID string `json:"session_id,omitempty"`
}

// chatMessage is one OpenAI conversation message. ToolCalls replays a prior
// assistant turn's calls back to the model; ToolCallID/Name carry a tool result.
type chatMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	Name       string             `json:"name,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCallWire `json:"tool_calls,omitempty"`
}

// chatTool is an OpenAI tool (function) definition. Parameters is the raw
// JSON-schema object, passed through unchanged end-to-end.
type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatToolCallWire is the OpenAI wire form of a tool call: the function
// arguments are a JSON string (not an object), matching the response shape.
type chatToolCallWire struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatToolCallFuncWire `json:"function"`
}

type chatToolCallFuncWire struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ---- response shapes ----

type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usage        `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatChunkResponse is one SSE frame for a streaming chat completion
// (object "chat.completion.chunk"). Each frame carries a delta, not the full
// message so far.
type chatChunkResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

// chatDelta is the incremental delta in a streaming chunk: the first frame
// carries the role, subsequent frames carry content (or tool_calls).
type chatDelta struct {
	Role      string             `json:"role,omitempty"`
	Content   string             `json:"content,omitempty"`
	ToolCalls []chatToolCallWire `json:"tool_calls,omitempty"`
}

// sessionHeader is the HTTP header carrying a session id in AFFINITY (stateless)
// mode: the client keeps the full conversation history client-side and sends it
// in messages[] every turn; the header only pins the job to the session's
// warm-cache worker via job.SessionID. Contrast STATEFUL mode (session_id body
// field), where the server owns the history.
const sessionHeader = "X-Session-Id"

// chatMode is the resolved session behavior for a chat request.
type chatMode int

const (
	// modeStateless is the default: no session id anywhere; existing byte-identical
	// behavior, no history reconstruction or persistence.
	modeStateless chatMode = iota
	// modeAffinity tags the job with the X-Session-Id header for warm-worker
	// routing; the client supplies full history and the server stores nothing.
	modeAffinity
	// modeStateful reconstructs context from server-stored history and persists the
	// new turn(s) + assistant reply after a successful response.
	modeStateful
)

// resolveSession decides the session mode and id for a chat request. The two
// session inputs express different intents:
//
//   - X-Session-Id header  → AFFINITY: client owns history, server only routes.
//   - session_id body field → STATEFUL: server owns history and reconstructs it.
//
// They are mutually exclusive intents. If BOTH are set the body wins (stateful):
// a client that bothered to store history server-side clearly wants it used, so
// the header is treated as redundant — but its id still tags the job for
// affinity, so a stateful conversation also routes to its warm worker. Either
// way job.SessionID is set so the dispatcher can pin/rebind the worker (#34).
func resolveSession(r *http.Request, req chatCompletionRequest) (mode chatMode, id string) {
	header := r.Header.Get(sessionHeader)
	switch {
	case req.SessionID != "":
		return modeStateful, req.SessionID
	case header != "":
		return modeAffinity, header
	default:
		return modeStateless, ""
	}
}

// ---- handler ----

// handleChatCompletions serves POST /v1/chat/completions. It decodes the
// OpenAI request, builds a chat types.Job carrying the full message history and
// any tool definitions, and either returns a single chat.completion (stream
// false) or streams chat.completion.chunk SSE frames (stream true). Auth has
// already happened in authMiddleware; authorization and quota are enforced by
// the server's SubmitAuthorizedJob* paths, whose errors map to HTTP status via
// statusForError.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatCompletionRequest
	if !decodePost(w, r, &req) {
		return
	}
	key, ok := keyFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}

	mode, sessionID := resolveSession(r, req)
	if mode != modeStateless && s.sessionMgr == nil {
		// A session id was supplied but the subsystem is not wired in (only possible
		// in unit tests, never in cmd). Fail closed rather than silently dropping it.
		writeError(w, http.StatusNotImplemented, "not_implemented", "sessions are not enabled")
		return
	}

	job := types.Job{
		ID:       newID("job-"),
		Model:    req.Model,
		Messages: toDomainMessages(req.Messages),
		Tools:    toDomainTools(req.Tools),
	}

	// AFFINITY (header) and STATEFUL (body) both tag the job so the dispatcher
	// pins/rebinds the conversation's warm-cache worker (#34). Only STATEFUL also
	// reconstructs server-side history into the prompt; AFFINITY passes the
	// client-supplied messages through unchanged.
	job.SessionID = sessionID
	if mode == modeStateful {
		hist, err := s.sessionMgr.History(r.Context(), sessionID, key.ID)
		if err != nil {
			s.writeSessionLookupError(w, err)
			return
		}
		// The reconstructed context is stored history followed by this turn's new
		// messages, so the worker receives the full conversation even though the
		// client sent only the new turn(s).
		job.Messages = append(hist, toDomainMessages(req.Messages)...)
	}

	if req.Stream {
		s.streamChat(w, r, key, job, req.Model, mode, sessionID, req.Messages)
		return
	}

	res, err := s.engine.SubmitAuthorizedJob(r.Context(), key, job)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}

	assistant := chatMessage{
		Role:      "assistant",
		Content:   res.Output,
		ToolCalls: fromDomainToolCalls(res.ToolCalls),
	}
	// Persist the turn AFTER a successful response so a failed inference never
	// pollutes stored history. Affinity mode stores nothing (the client owns it).
	if mode == modeStateful {
		s.persistTurn(r.Context(), sessionID, key.ID, req.Messages, res.Output, res.ToolCalls)
	}

	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      newID("chatcmpl-"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      assistant,
			FinishReason: finishReasonOrStop(res.FinishReason),
		}},
		Usage: usageFrom(res.PromptTokens, res.CompletionTokens, res.Tokens),
	})
}

// persistTurn appends a completed stateful turn to the session's history: each
// new user/tool message the client sent this turn, followed by the assistant
// reply (content + any tool_calls). It is called only after a successful
// response so a failed inference never stores a half-turn. Append errors are
// logged and swallowed — a persistence failure must never fail an inference the
// client already received (the reply is still returned; at worst the next turn
// lacks this one's context). The assistant reply is stored even when empty
// (e.g. a pure tool_calls turn) so the conversation stays well-formed.
func (s *Server) persistTurn(ctx context.Context, sessionID, ownerKeyID string, newMessages []chatMessage, output string, toolCalls []types.ToolCall) {
	for _, m := range toDomainMessages(newMessages) {
		if err := s.sessionMgr.AppendTurn(ctx, sessionID, ownerKeyID, m); err != nil {
			s.log.Warn("session append (request turn) failed", "session", sessionID, "err", err)
		}
	}
	assistant := types.Message{Role: "assistant", Content: output, ToolCalls: toolCalls}
	if err := s.sessionMgr.AppendTurn(ctx, sessionID, ownerKeyID, assistant); err != nil {
		s.log.Warn("session append (assistant turn) failed", "session", sessionID, "err", err)
	}
}

// streamChat handles a stream=true chat request: it gates+dispatches via
// SubmitAuthorizedJobStream and forwards each JobChunk as an OpenAI SSE frame.
// The first frame announces the assistant role; each subsequent frame carries a
// content (and/or tool_calls) delta; the terminal chunk sets finish_reason; the
// stream ends with the OpenAI [DONE] sentinel. Headers are written before the
// first frame, so a gating error that arrives before any byte is returned as a
// JSON error + status instead.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, key store.APIKey, job types.Job, model string, mode chatMode, sessionID string, newMessages []chatMessage) {
	chunks, err := s.engine.SubmitAuthorizedJobStream(r.Context(), key, job)
	if err != nil {
		// Failed before any byte: a normal JSON error with the mapped status.
		s.writeSubmitError(w, err)
		return
	}

	flusher, sse := beginSSE(w)
	if !sse {
		// No streaming support behind the ResponseWriter (should not happen with
		// net/http). Fail closed rather than buffering a non-streamed response.
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	id := newID("chatcmpl-")
	created := time.Now().Unix()
	roleSent := false

	// Accumulate the assistant reply across frames so a STATEFUL session can store
	// the full turn once the stream terminates cleanly. Tool-call deltas are
	// collected too so a streamed function call persists across turns. Both are
	// cheap to maintain even in stateless/affinity mode; they are only persisted
	// when mode == modeStateful.
	var content strings.Builder
	var toolCalls []types.ToolCall
	failed := false
	// sawDone tracks whether a genuine terminal chunk (Done == true) was observed.
	// A mid-stream client disconnect closes the chunk channel WITHOUT an error
	// frame (server.go's producer reacts to ctx.Done by detaching the observer and
	// returning), so the loop below would exit normally with failed == false even
	// though the upstream job was aborted and the assistant reply is truncated.
	// Persisting that partial turn would corrupt the next stateful turn's
	// reconstructed context, so persistence is gated on sawDone — only a clean
	// terminal chunk proves the inference completed.
	sawDone := false

	for chunk := range chunks {
		if chunk.Err != nil {
			// Mid-stream failure: emit a terminal error frame then [DONE] so the
			// client's stream parser ends cleanly rather than hanging. Do NOT persist
			// a partial turn for a failed stream.
			s.writeChatErrorFrame(w, flusher, id, created, model, chunk.Err)
			failed = true
			break
		}

		content.WriteString(chunk.Delta)
		toolCalls = append(toolCalls, chunk.ToolCalls...)

		choice := chatChunkChoice{Index: 0}
		if !roleSent {
			choice.Delta.Role = "assistant"
			roleSent = true
		}
		choice.Delta.Content = chunk.Delta
		choice.Delta.ToolCalls = fromDomainToolCalls(chunk.ToolCalls)
		if chunk.Done {
			sawDone = true
			fr := finishReasonOrStop(chunk.FinishReason)
			choice.FinishReason = &fr
		}

		writeSSEData(w, flusher, chatChunkResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []chatChunkChoice{choice},
		})
	}

	writeSSEDone(w, flusher)

	// Persist the turn atomically (the new user/tool message(s) AND the assistant
	// reply together) only after the stream terminated cleanly: a genuine terminal
	// chunk was observed (sawDone), no mid-stream error occurred (!failed), and the
	// client did not disconnect (request context not cancelled). On a disconnect
	// the chunk channel closes with no error frame, so !failed alone is not enough
	// — sawDone and the context check together ensure NEITHER the user turn nor a
	// partial assistant turn is persisted for an aborted stream, keeping the
	// session consistent so the client can simply retry the turn. The request
	// context may be cancelled once the client closes a SUCCESSFUL stream too, so
	// the write uses a detached context — the inference already succeeded.
	if mode == modeStateful && !failed && sawDone && r.Context().Err() == nil {
		s.persistTurn(context.WithoutCancel(r.Context()), sessionID, key.ID, newMessages, content.String(), toolCalls)
	}
}

// writeChatErrorFrame emits a terminal chat chunk carrying finish_reason
// "error" so a client streaming the response observes a clean termination on an
// upstream failure (after headers are already sent, a JSON error is no longer
// an option). The actual error code is logged server-side.
func (s *Server) writeChatErrorFrame(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, jerr *types.JobError) {
	s.log.Warn("chat stream failed mid-stream", "code", jerr.Code, "err", jerr.Message)
	fr := "error"
	writeSSEData(w, flusher, chatChunkResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []chatChunkChoice{{Index: 0, FinishReason: &fr}},
	})
}

// ---- request/response conversions ----

func toDomainMessages(ms []chatMessage) []types.Message {
	if len(ms) == 0 {
		return nil
	}
	out := make([]types.Message, 0, len(ms))
	for _, m := range ms {
		out = append(out, types.Message{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toDomainToolCalls(m.ToolCalls),
		})
	}
	return out
}

// fromDomainMessages maps stored conversation turns back onto the OpenAI message
// wire shape. It is the inverse of toDomainMessages and is reused by the session
// GET handler to render a session's history exactly as a client would send it,
// so a retrieved conversation can be replayed verbatim.
func fromDomainMessages(ms []types.Message) []chatMessage {
	out := make([]chatMessage, 0, len(ms))
	for _, m := range ms {
		out = append(out, chatMessage{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			ToolCalls:  fromDomainToolCalls(m.ToolCalls),
		})
	}
	return out
}

func toDomainToolCalls(cs []chatToolCallWire) []types.ToolCall {
	if len(cs) == 0 {
		return nil
	}
	out := make([]types.ToolCall, 0, len(cs))
	for _, c := range cs {
		typ := c.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, types.ToolCall{
			ID:           c.ID,
			Type:         typ,
			FunctionName: c.Function.Name,
			Arguments:    c.Function.Arguments,
		})
	}
	return out
}

func toDomainTools(ts []chatTool) []types.Tool {
	if len(ts) == 0 {
		return nil
	}
	out := make([]types.Tool, 0, len(ts))
	for _, t := range ts {
		typ := t.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, types.Tool{
			Type: typ,
			Function: types.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  string(t.Function.Parameters),
			},
		})
	}
	return out
}

// fromDomainToolCalls maps the worker's tool calls onto the OpenAI wire form,
// assigning an id when the backend reported none (OpenAI clients require a
// non-empty id to correlate the subsequent tool result message).
func fromDomainToolCalls(cs []types.ToolCall) []chatToolCallWire {
	if len(cs) == 0 {
		return nil
	}
	out := make([]chatToolCallWire, 0, len(cs))
	for _, c := range cs {
		id := c.ID
		if id == "" {
			id = newID("call_")
		}
		typ := c.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, chatToolCallWire{
			ID:   id,
			Type: typ,
			Function: chatToolCallFuncWire{
				Name:      c.FunctionName,
				Arguments: c.Arguments,
			},
		})
	}
	return out
}
