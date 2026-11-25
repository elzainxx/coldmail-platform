package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// parsedToken is a token whose structure has been checked and whose signature has not.
type parsedToken struct {
	header, payload map[string]json.RawMessage
	signingInput    string // the first two segments and the dot between them, as received
	sig             []byte
}

// b64 is the encoding of every segment: base64url, no padding, and strict about the unused trailing bits (so one signature has one spelling).
var b64 = base64.RawURLEncoding.Strict()

// parseToken is check 1: the structure of a compact JWS. Every failure is ErrMalformed with a fixed reason; nothing of the token is echoed.
func parseToken(s string) (*parsedToken, error) {
	if s == "" {
		return nil, fail(ErrMalformed, "the token is empty")
	}
	if len(s) > MaxTokenBytes {
		return nil, fail(ErrMalformed, "the token is longer than 8192 bytes")
	}
	// The alphabet first: the base64 decoder skips line breaks, so a token with one inside would otherwise decode, and white space around a token is a sign of a header that was not parsed.
	dots := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			dots++
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return nil, fail(ErrMalformed, "the token holds a character outside the base64url alphabet")
		}
	}
	if dots != 2 {
		return nil, fail(ErrMalformed, "the token does not have exactly three segments")
	}
	first := strings.IndexByte(s, '.')
	second := first + 1 + strings.IndexByte(s[first+1:], '.')
	seg0, seg1, seg2 := s[:first], s[first+1:second], s[second+1:]
	if seg0 == "" || seg1 == "" {
		return nil, fail(ErrMalformed, "the header or the payload segment is empty")
	}
	rawHeader, err := b64.DecodeString(seg0)
	if err != nil {
		return nil, fail(ErrMalformed, "the header segment is not valid base64url")
	}
	rawPayload, err := b64.DecodeString(seg1)
	if err != nil {
		return nil, fail(ErrMalformed, "the payload segment is not valid base64url")
	}
	// An empty signature segment is well formed here: it is the shape of an unsecured ("alg":"none") token, which the algorithm check refuses under its own name.
	sig, err := b64.DecodeString(seg2)
	if err != nil {
		return nil, fail(ErrMalformed, "the signature segment is not valid base64url")
	}
	header, err := parseObject(rawHeader)
	if err != nil {
		return nil, fail(ErrMalformed, "the header is not a JSON object")
	}
	payload, err := parseObject(rawPayload)
	if err != nil {
		return nil, fail(ErrMalformed, "the payload is not a JSON object")
	}
	return &parsedToken{header: header, payload: payload, signingInput: s[:second], sig: sig}, nil
}

// parseObject decodes a JSON object into its members, refusing a repeated key (decoders disagree about which of two members wins, and a token whose header says two things is
// read differently by two parties) and anything after the closing brace.
func parseObject(b []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errNotObject
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			return nil, errNotObject
		}
		out[key] = raw
	}
	if t, err = dec.Token(); err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '}' {
		return nil, errNotObject
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errNotObject
	}
	return out, nil
}

var errNotObject = errors.New("not a JSON object")
