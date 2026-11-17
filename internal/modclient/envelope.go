package modclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The error envelope the three modules share (module 1 internal/api/errors.go:49-103 and :141-155; module 2 and module 3 internal/platformapi/errors.go):
//
//	{"error":{"code":"<code>","message":"<fixed sentence>","details":[{"field":"<f>","issue":"<i>"}],"request_id":"<id>"}}
//
// `details` is omitted when empty. `message` is a fixed sentence per code, never interpolated, and is NOT copied here: the code says what the sentence says. The request id is a
// UUIDv7 from module 1 and `req_` plus 32 hex digits from modules 2 and 3.
//
// The code vocabulary this client knows (every one is additive-only in the modules; a code it does not know is kept as data and the status decides the kind):
//
//	unauthenticated 401, validation_failed 400, not_found 404, method_not_allowed 405, conflict 409 (modules 2, 3), unprocessable 422 (module 3 and the admin domains),
//	rate_limited 429 (module 3 and the admin domains), forbidden 403 (admin domains), payload_too_large 413 (module 1), internal_error 500, contract_violation 500 (module 1),
//	unavailable 503 (module 1), paused 503 (all three), config_ordering_violation 400 (admin domains).
//
// Only `paused` changes a kind: a 503 is Paused when its code is `paused` and Unavailable otherwise. Every other kind is decided by the status, so a code the platform has
// never seen (an additive change) degrades by status, exactly as the modules' own documentation says a client should.

// envState says what a response body is as an error envelope.
type envState int

const (
	envNone      envState = iota // not a JSON object with an "error" key: no envelope is claimed
	envOK                        // a well-formed envelope
	envMalformed                 // an object with an "error" key whose value is not an envelope: it claims to be one and is not
)

type envelope struct {
	code      string
	requestID string
	details   []Detail
}

const (
	maxCodeLen      = 64
	maxRequestIDLen = 64
	maxDetailLen    = 100
	maxDetails      = 100 // module 1 lists at most 100 (item_id problems); more is truncated, not an error
)

// safeWord accepts the alphabet of a module's fixed identifiers: ASCII letters, digits and _ - . [ ] , up to max bytes. A response that stuffs anything else into a field gets
// nothing of it into an error.
func safeWord(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == '[', c == ']':
		default:
			return false
		}
	}
	return true
}

// parseEnvelope reads body as an error envelope. secret is the bearer value: a field that contains it is treated as unsafe (a module, or something in front of it, that echoes the
// credential back must not get it into an error).
func parseEnvelope(body []byte, secret string) (envelope, envState) {
	if len(bytes.TrimSpace(body)) == 0 {
		return envelope{}, envNone
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return envelope{}, envNone
	}
	if _, err := dec.Token(); err != io.EOF {
		return envelope{}, envNone
	}
	raw, ok := top["error"]
	if !ok {
		return envelope{}, envNone
	}
	var inner struct {
		Code      *string           `json:"code"`
		Message   json.RawMessage   `json:"message"`
		Details   []json.RawMessage `json:"details"`
		RequestID json.RawMessage   `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &inner); err != nil || inner.Code == nil {
		return envelope{}, envMalformed
	}
	clean := func(s string, max int) bool {
		return safeWord(s, max) && (secret == "" || !strings.Contains(s, secret))
	}
	if !clean(*inner.Code, maxCodeLen) {
		return envelope{}, envMalformed
	}
	env := envelope{code: *inner.Code}
	if len(inner.RequestID) > 0 {
		var id string
		if json.Unmarshal(inner.RequestID, &id) == nil && clean(id, maxRequestIDLen) {
			env.requestID = id
		}
	}
	for i, d := range inner.Details {
		if i >= maxDetails {
			break
		}
		var one struct {
			Field *string `json:"field"`
			Issue *string `json:"issue"`
		}
		if err := json.Unmarshal(d, &one); err != nil || one.Field == nil || one.Issue == nil {
			return envelope{}, envMalformed
		}
		if clean(*one.Field, maxDetailLen) && clean(*one.Issue, maxDetailLen) {
			env.details = append(env.details, Detail{Field: *one.Field, Issue: *one.Issue})
		}
	}
	return env, envOK
}

// headerRequestID reads the request id a module put in a response header, if it did. None of the three does today (a search of their sources finds no X-Request-Id and no
// Retry-After on any response), so this is the "response header if any" the contract leaves room for.
func headerRequestID(h http.Header, secret string) string {
	for _, name := range []string{"X-Request-Id", "X-Request-ID"} {
		if id := strings.TrimSpace(h.Get(name)); safeWord(id, maxRequestIDLen) && (secret == "" || !strings.Contains(id, secret)) {
			return id
		}
	}
	return ""
}

// bodyRequestID reads a top-level request_id of a SUCCESS body (module 3's bulk-add answers one). Anything that is not a short safe string is "".
func bodyRequestID(body []byte, secret string) string {
	var w struct {
		RequestID json.RawMessage `json:"request_id"`
	}
	if json.Unmarshal(body, &w) != nil || len(w.RequestID) == 0 {
		return ""
	}
	var id string
	if json.Unmarshal(w.RequestID, &id) != nil || !safeWord(id, maxRequestIDLen) || (secret != "" && strings.Contains(id, secret)) {
		return ""
	}
	return id
}

// maxRetryAfter bounds what a Retry-After may say: anything longer is a day, and the arithmetic stays far from overflow.
const maxRetryAfter = 24 * time.Hour

// parseRetryAfter reads a Retry-After header: delta-seconds or an HTTP date (RFC 9110 section 10.2.3). Absent, unreadable, negative or past is 0.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		switch {
		case d <= 0:
			return 0
		case d > maxRetryAfter:
			return maxRetryAfter
		}
		return d
	}
	return 0
}
