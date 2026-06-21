package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// errorBody is the JSON envelope for an error response. The shape mirrors the
// OpenAI error object ({"error":{...}}) so OpenAI-compatible clients parse it,
// while the typed code lets agent-gpu clients branch programmatically.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	// Message and Code are the agent-gpu contract: a generic human message and a
	// stable machine code clients branch on. Type is the OpenAI error class
	// ("invalid_request_error", "authentication_error", "rate_limit_error",
	// "server_error"), added so the envelope is a strict superset of OpenAI's
	// error object — an OpenAI client that keys off error.type keeps working while
	// agent-gpu clients keep using the finer-grained code. It is populated from the
	// HTTP status in writeError, so no call site sets it explicitly. omitempty
	// keeps a hand-built envelope without a type (none today) clean.
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type,omitempty"`
}

// openAIErrorType maps an HTTP status to the OpenAI error `type` so the envelope
// is a strict superset of OpenAI's error object. The mapping mirrors OpenAI's
// own classes: client-fault statuses (400/403/404/405/409/422) are
// "invalid_request_error", 401 is "authentication_error", 429 is
// "rate_limit_error", and any 5xx is "server_error". A status outside these
// ranges (none are produced today) falls back to "server_error" so the field is
// always populated with a sane class.
func openAIErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusConflict,
		http.StatusUnprocessableEntity:
		return "invalid_request_error"
	}
	// Everything else (notably 5xx and any unmapped status) is a server fault.
	return "server_error"
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
		_, _ = w.Write([]byte(`{"error":{"message":"internal error","code":"internal_error","type":"server_error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeError writes a JSON error envelope with the given HTTP status, machine
// code, and human message. Messages are deliberately generic so no secret,
// token, or internal detail ever reaches the client. The OpenAI `type` is
// derived from the status (openAIErrorType) and added to the envelope here, so
// every error response is a strict superset of OpenAI's error object without any
// call site having to supply it.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Message: msg,
		Code:    code,
		Type:    openAIErrorType(status),
	}})
}

// streamErrorBody builds the OpenAI error envelope emitted as a terminal SSE
// frame when an inference fails mid-stream (after the headers/first frame, so a
// JSON error + status is no longer possible). The message is deliberately
// generic — never the worker's internal jerr.Message — so no internal detail
// leaks to the client, matching writeError's policy; the worker's actual code is
// logged server-side by the caller. The machine code is the worker-reported
// jerr.Code when present, else "internal_error", and the OpenAI type is
// "server_error" (a mid-stream failure is a server fault). A nil jerr is treated
// as a generic internal error so the helper never dereferences nil.
func streamErrorBody(jerr *types.JobError) errorBody {
	code := "internal_error"
	if jerr != nil && jerr.Code != "" {
		code = jerr.Code
	}
	return errorBody{Error: errorDetail{
		Message: "inference failed",
		Code:    code,
		Type:    "server_error",
	}}
}
