package contractcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	reIdName    = regexp.MustCompile(`(?:^|_)(?:id|ids|uuid|cursor|etag|nonce|digest|hash|signature|sig|trace|span)$`)
	reTimeParts = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})([Tt ])([0-9]{2}:[0-9]{2}:[0-9]{2})(\.[0-9]+)?([Zz]|[+-][0-9]{2}:[0-9]{2})$`)
)

const (
	syntheticTime    = "2026-01-01"
	syntheticClock   = "00:00:00"
	redactedWord     = "REDACTED"
	syntheticUUIDFmt = "00000000-0000-4000-8000-%012d"
)

// Normalise rewrites a recorded body so that it can be committed to a public repository and compared again later: every UUID becomes a synthetic one by order of first
// appearance (`00000000-0000-4000-8000-000000000001`, `...002`, in a key or in a string, case-insensitively); every RFC 3339 timestamp becomes `2026-01-01T00:00:00Z` with the
// same separator, fraction length and offset style; a string under a key that names an id, cursor, token, hash or the like, and any long opaque string of the safe
// alphabet (16 or more hex characters; 24 or more base64-like characters with a letter and a digit), becomes one repeated character of the same alphabet and the same length;
// a string under a key that names a secret (key, secret, password, token, authorization, credential) becomes REDACTED. Keys, key order and structure are unchanged; numbers,
// booleans, null and every other string are untouched. The output has two-space indentation and ends with one newline; the same input always gives the same output, and
// normalising an already normalised body changes nothing.
func Normalise(body []byte) ([]byte, error) {
	v, err := parse(body)
	if err != nil {
		return nil, err
	}
	n := &normaliser{uuids: map[string]string{}}
	var buf bytes.Buffer
	n.emit(&buf, v, "", 0)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

type normaliser struct {
	uuids map[string]string
}

func (n *normaliser) uuid(s string) string {
	return reUUID.ReplaceAllStringFunc(s, func(u string) string {
		k := strings.ToLower(u)
		if syn, ok := n.uuids[k]; ok {
			return syn
		}
		syn := fmt.Sprintf(syntheticUUIDFmt, len(n.uuids)+1)
		n.uuids[k] = syn
		return syn
	})
}

func timestamp(s string) string {
	return reTimestamp.ReplaceAllStringFunc(s, func(t string) string {
		p := reTimeParts.FindStringSubmatch(t)
		if p == nil {
			return t
		}
		zone := "Z"
		if p[5] == "z" {
			zone = "z"
		} else if p[5] != "Z" {
			zone = "+00:00"
		}
		frac := ""
		if p[4] != "" {
			frac = "." + strings.Repeat("0", len(p[4])-1)
		}
		return syntheticTime + p[2] + syntheticClock + frac + zone
	})
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// opaque reports whether a string, with no key to say so, looks like an identifier or a token: 16 or more hex characters, or 24 or more characters of the base64 alphabet that
// hold both a letter and a digit and at most two separators (a model name or a slug has more).
func opaque(s string) bool {
	if isHex(s) {
		return len(s) >= 16
	}
	if len(s) < 24 {
		return false
	}
	letter, digit, seps := false, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9':
			digit = true
		case c == '-' || c == '_':
			seps++
		case c == '+' || c == '/' || c == '=':
		default:
			return false
		}
	}
	return letter && digit && seps <= 2
}

// placeholder is one repeated character of the alphabet of s and the length of s ('=' padding kept): '0' for hex and digits, 'a' when there is no capital letter, else 'A'. A
// string that is already one repeated character is returned as it is, so normalising twice changes nothing.
func placeholder(s string) string {
	if uniform(s) {
		return s
	}
	core := strings.TrimRight(s, "=")
	pad := s[len(core):]
	fill := "A"
	switch {
	case isHex(core):
		fill = "0"
	case core == strings.ToLower(core):
		fill = "a"
	}
	return strings.Repeat(fill, utf8.RuneCountInString(core)) + pad
}

func (n *normaliser) str(s, name string) string {
	if s == "" {
		return s
	}
	if name != "" && secretName(name) {
		return redactedWord
	}
	s = timestamp(n.uuid(s))
	if reUUID.MatchString(s) || reTimestamp.MatchString(s) {
		return s
	}
	if name != "" && reIdName.MatchString(canonName(name)) || opaque(s) {
		return placeholder(s)
	}
	return s
}

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// emit writes v with two-space indentation; name is the key of the member (or of the array's owner) that v belongs to.
func (n *normaliser) emit(buf *bytes.Buffer, v value, name string, depth int) {
	switch v.kind {
	case vNull:
		buf.WriteString("null")
	case vBool, vNumber:
		buf.WriteString(v.str)
	case vString:
		buf.WriteString(quote(n.str(v.str, name)))
	case vObject:
		if len(v.members) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i, m := range v.members {
			buf.WriteString(strings.Repeat("  ", depth+1))
			buf.WriteString(quote(n.uuid(m.key)))
			buf.WriteString(": ")
			n.emit(buf, m.val, m.key, depth+1)
			if i < len(v.members)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(strings.Repeat("  ", depth) + "}")
	case vArray:
		if len(v.items) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, it := range v.items {
			buf.WriteString(strings.Repeat("  ", depth+1))
			n.emit(buf, it, name, depth+1)
			if i < len(v.items)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(strings.Repeat("  ", depth) + "]")
	}
}
