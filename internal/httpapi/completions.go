package httpapi

import (
	"net/http"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// ---- request shape ----

// completionRequest is the subset of the legacy OpenAI completions request
// agent-gpu acts on: a model, a text prompt, and the stream flag. Other fields
// (max_tokens, temperature, …) are accepted and ignored — a documented seam for
// when those params are plumbed to Ollama. The prompt maps onto Job.Prompt, the
// foundational prompt path that EchoExecutor and #11 already exercise.
type completionRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// ---- response shapes ----

type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usage              `json:"usage"`
}

type completionChoice struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
}

// completionChunkResponse is one SSE frame of a streaming text completion
// (object "text_completion"); each frame carries the incremental text in
// choices[].text.
type completionChunkResponse struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []completionChunkChoice `json:"choices"`
}

type completionChunkChoice struct {
	Text         string  `json:"text"`
	Index        int     `json:"index"`
	FinishReason *string `json:"finish_reason"`
}

// ---- handler ----

// handleCompletions serves POST /v1/completions, the legacy text-completion
// surface. It maps the prompt onto a plain Job.Prompt (no messages), so the
// foundational dispatch path runs unchanged, and returns a text_completion (or
// streams text_completion SSE frames when stream is true). Authorization and
// quota are enforced by the server's SubmitAuthorizedJob* paths.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	var req completionRequest
	if !decodePost(w, r, &req) {
		return
	}
	key, ok := keyFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}

	job := types.Job{
		ID:     jobIDFor(r),
		Model:  req.Model,
		Prompt: req.Prompt,
	}

	if req.Stream {
		s.streamCompletion(w, r, key, job, req.Model)
		return
	}

	res, err := s.engine.SubmitAuthorizedJob(r.Context(), key, job)
	if err != nil {
		s.writeSubmitError(w, r, err)
		return
	}
	// Meter tokens by model on the success path (#24).
	s.recordTokens(req.Model, res.PromptTokens, res.CompletionTokens, res.Tokens)
	writeJSON(w, http.StatusOK, completionResponse{
		ID:      newID("cmpl-"),
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []completionChoice{{
			Text:         res.Output,
			Index:        0,
			FinishReason: finishReasonOrStop(res.FinishReason),
		}},
		Usage: usageFrom(res.PromptTokens, res.CompletionTokens, res.Tokens),
	})
}

// streamCompletion handles a stream=true completion request, forwarding each
// JobChunk delta as a text_completion SSE frame and terminating with [DONE]. As
// with chat, a gating error before the first byte is returned as a JSON error;
// a mid-stream failure emits a terminal error frame then [DONE].
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, key store.APIKey, job types.Job, model string) {
	chunks, err := s.engine.SubmitAuthorizedJobStream(r.Context(), key, job)
	if err != nil {
		s.writeSubmitError(w, r, err)
		return
	}

	flusher, sse := beginSSE(w)
	if !sse {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	id := newID("cmpl-")
	created := time.Now().Unix()

	// Token counts from the terminal chunk, metered once the stream ends cleanly
	// (#24), mirroring the chat streaming path.
	var promptTokens, completionTokens, totalTokens uint64
	failed := false
	sawDone := false

	for chunk := range chunks {
		if chunk.Err != nil {
			s.reqLog(r.Context()).Warn("completion stream failed mid-stream", "code", chunk.Err.Code, "err", chunk.Err.Message)
			fr := "error"
			writeSSEData(w, flusher, completionChunkResponse{
				ID:      id,
				Object:  "text_completion",
				Created: created,
				Model:   model,
				Choices: []completionChunkChoice{{Index: 0, FinishReason: &fr}},
			})
			failed = true
			break
		}

		choice := completionChunkChoice{Text: chunk.Delta, Index: 0}
		if chunk.Done {
			sawDone = true
			promptTokens, completionTokens, totalTokens = chunk.PromptTokens, chunk.CompletionTokens, chunk.Tokens
			fr := finishReasonOrStop(chunk.FinishReason)
			choice.FinishReason = &fr
		}
		writeSSEData(w, flusher, completionChunkResponse{
			ID:      id,
			Object:  "text_completion",
			Created: created,
			Model:   model,
			Choices: []completionChunkChoice{choice},
		})
	}

	writeSSEDone(w, flusher)

	// Meter tokens only for a cleanly-terminated stream (terminal chunk seen, no
	// mid-stream error), so a failed/aborted stream is not counted.
	if sawDone && !failed {
		s.recordTokens(model, promptTokens, completionTokens, totalTokens)
	}
}
