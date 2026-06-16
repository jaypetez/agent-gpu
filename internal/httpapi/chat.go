package httpapi

import (
	"encoding/json"
	"net/http"
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

	job := types.Job{
		ID:       newID("job-"),
		Model:    req.Model,
		Messages: toDomainMessages(req.Messages),
		Tools:    toDomainTools(req.Tools),
	}

	if req.Stream {
		s.streamChat(w, r, key, job, req.Model)
		return
	}

	res, err := s.engine.SubmitAuthorizedJob(r.Context(), key, job)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      newID("chatcmpl-"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index: 0,
			Message: chatMessage{
				Role:      "assistant",
				Content:   res.Output,
				ToolCalls: fromDomainToolCalls(res.ToolCalls),
			},
			FinishReason: finishReasonOrStop(res.FinishReason),
		}},
		Usage: usageFrom(res.PromptTokens, res.CompletionTokens, res.Tokens),
	})
}

// streamChat handles a stream=true chat request: it gates+dispatches via
// SubmitAuthorizedJobStream and forwards each JobChunk as an OpenAI SSE frame.
// The first frame announces the assistant role; each subsequent frame carries a
// content (and/or tool_calls) delta; the terminal chunk sets finish_reason; the
// stream ends with the OpenAI [DONE] sentinel. Headers are written before the
// first frame, so a gating error that arrives before any byte is returned as a
// JSON error + status instead.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, key store.APIKey, job types.Job, model string) {
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

	for chunk := range chunks {
		if chunk.Err != nil {
			// Mid-stream failure: emit a terminal error frame then [DONE] so the
			// client's stream parser ends cleanly rather than hanging.
			s.writeChatErrorFrame(w, flusher, id, created, model, chunk.Err)
			break
		}

		choice := chatChunkChoice{Index: 0}
		if !roleSent {
			choice.Delta.Role = "assistant"
			roleSent = true
		}
		choice.Delta.Content = chunk.Delta
		choice.Delta.ToolCalls = fromDomainToolCalls(chunk.ToolCalls)
		if chunk.Done {
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
