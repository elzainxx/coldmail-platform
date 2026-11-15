package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// RequestIDHeader is set on every response.
const RequestIDHeader = "X-Request-Id"

// External error codes of the error envelope. They are the same words the three modules use, so a client
// needs no per-module vocabulary. Additive-only: never rename or repurpose one.
const (
	codeNotFound         = "not_found"          // 404
	codeMethodNotAllowed = "method_not_allowed" // 405
	codeInternalError    = "internal_error"     // 500
)

// fixedMessages are the only texts a client ever sees for a code. Nothing a caller sent is reflected and
// nothing internal (a path, a driver error, a panic value) is serialized: that detail goes to the log,
// keyed by request_id.
var fixedMessages = map[string]string{
	codeNotFound:         "The requested resource was not found.",
	codeMethodNotAllowed: "This method is not allowed on this path.",
	codeInternalError:    "An unexpected error occurred.",
}

var codeStatus = map[string]int{
	codeNotFound:         http.StatusNotFound,
	codeMethodNotAllowed: http.StatusMethodNotAllowed,
	codeInternalError:    http.StatusInternalServerError,
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// newRequestID mints an opaque correlation id of the shape req_<32 hex>, the same as the modules', so one
// log-grep pattern serves every surface. crypto/rand.Read never returns an error (since Go 1.24 it
// terminates the process if the system source fails), so there is no fallback id to get wrong.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, requestID, code string) {
	writeJSON(w, codeStatus[code], errorEnvelope{Error: errorBody{
		Code:      code,
		Message:   fixedMessages[code],
		RequestID: requestID,
	}})
}
