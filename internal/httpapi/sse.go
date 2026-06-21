package httpapi

import (
	"encoding/json"
	"net/http"
)

// Server-Sent Events plumbing shared by the streaming inference endpoints. The
// framing matches OpenAI's streaming responses exactly: each event is a single
// `data: <json>\n\n` line, the terminus is the literal `data: [DONE]\n\n`
// sentinel, and every frame is flushed immediately so tokens reach the client
// as they are produced rather than being buffered into one response.

// beginSSE writes the SSE response headers and returns the writer's Flusher so
// each frame can be pushed to the client immediately. It reports false when the
// ResponseWriter does not support flushing (no http.Flusher), in which case the
// caller must not attempt to stream. Headers are written here, before any data
// frame, so a later WriteHeader cannot conflict.
func beginSSE(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx) so frames are not coalesced upstream.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

// writeSSEData marshals v and writes it as one SSE data frame, then flushes. A
// marshal failure (which should never happen for our own response types) is
// dropped rather than corrupting the stream framing; the [DONE] sentinel still
// terminates the stream cleanly.
func writeSSEData(w http.ResponseWriter, flusher http.Flusher, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n\n"))
	flusher.Flush()
}

// writeSSEError marshals an error envelope and writes it as one SSE data frame,
// then flushes. It is the mid-stream-failure counterpart of writeSSEData: once a
// stream has begun (headers and the first frame are sent) a JSON error + status
// is no longer possible, so an upstream failure is surfaced as a single
// `data: {"error":{...}}\n\n` frame carrying the OpenAI error envelope. The
// caller follows it with writeSSEDone so the client's stream parser ends cleanly,
// and — crucially — emits NO chunk with a fake finish_reason, so a truncated
// answer is never mistaken for a clean completion. Framing is identical to
// writeSSEData; the distinct name documents the intent at the call site.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, body errorBody) {
	writeSSEData(w, flusher, body)
}

// writeSSEDone writes the OpenAI stream terminator and flushes it, signalling
// end-of-stream to an OpenAI-compatible client.
func writeSSEDone(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}
