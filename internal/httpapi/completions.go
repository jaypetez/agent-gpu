package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	Model  string      `json:"model"`
	Prompt promptField `json:"prompt"`
	Stream bool        `json:"stream"`
}

// promptField is the OpenAI /v1/completions prompt, which may be either a single
// string or an array of strings. agent-gpu's dispatch path takes one prompt
// string, so an array is flattened by joining its elements with a newline — the
// natural rendering of "these are consecutive prompt fragments" onto the single
// Job.Prompt. OpenAI also permits integer-token arrays (a pre-tokenized prompt);
// agent-gpu has no tokenizer, so that shape is rejected as an invalid request
// rather than silently mishandled. The underlying type stays string so it maps
// onto Job.Prompt unchanged.
type promptField string

// UnmarshalJSON accepts the two prompt shapes OpenAI clients send — a JSON string
// or a JSON array of strings — and rejects anything else (an integer-token array,
// an object, a number) so decodePost surfaces a 400 invalid_request_error rather
// than coercing an unsupported shape. An array of strings is joined with "\n"; an
// array element that is not a string fails the whole decode.
func (p *promptField) UnmarshalJSON(data []byte) error {
	// Single string: the common case.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*p = promptField(s)
		return nil
	}
	// Array of strings: join with newlines onto the single prompt. A non-string
	// element (e.g. an integer-token array) fails to unmarshal here and is
	// rejected, since agent-gpu has no tokenizer to interpret token ids.
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*p = promptField(strings.Join(arr, "\n"))
		return nil
	}
	return errors.New("prompt must be a string or an array of strings")
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
		Prompt: string(req.Prompt),
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
			// Mid-stream failure: emit a terminal SSE error frame (the OpenAI error
			// envelope) then [DONE] so the client observes the failure rather than a
			// truncated completion that looks clean. No fake finish_reason is emitted.
			// The worker's actual code is logged server-side; the client gets only a
			// generic message.
			s.reqLog(r.Context()).Warn("completion stream failed mid-stream", "code", chunk.Err.Code, "err", chunk.Err.Message)
			writeSSEError(w, flusher, streamErrorBody(chunk.Err))
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
