package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorBody is the JSON envelope for an error response. The shape mirrors the
// OpenAI error object ({"error":{...}}) so OpenAI-compatible clients parse it,
// while the typed code lets agent-gpu clients branch programmatically.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// writeJSON serializes v as JSON with the given status. On a marshal failure it
// falls back to a 500 with a plain error envelope so a handler bug never leaves
// the client hanging on an empty body. It is shared by every JSON-returning
// handler (model discovery today, #13 next).
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		// Marshalling our own response types should never fail; if it does, the
		// client still gets a well-formed error rather than a truncated body.
		slog.Default().Error("httpapi: marshal response", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal error","code":"internal_error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeError writes a JSON error envelope with the given HTTP status, machine
// code, and human message. Messages are deliberately generic so no secret,
// token, or internal detail ever reaches the client.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Code: code}})
}
