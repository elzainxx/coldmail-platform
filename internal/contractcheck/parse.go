package contractcheck

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

const (
	// MaxDepth is the deepest container nesting the parser accepts. The modules' responses are three or four levels deep; a body that nests further is refused with
	// ParseTooDeep instead of being walked (the walkers recurse once per level).
	MaxDepth = 64
	// MaxBody is the largest body the parser reads.
	MaxBody = 8 << 20
)

type vkind uint8

const (
	vNull vkind = iota
	vBool
	vNumber
	vString
	vObject
	vArray
)

// value is a parsed JSON document: like the decoder's generic form, but with object members in the order the input gave them (the order is evidence) and numbers kept as their
// literal text (nothing is rounded, so a huge number is only a number).
type value struct {
	kind    vkind
	str     string // the string value, the literal of a number, or "true" / "false"
	members []member
	items   []value
}

type member struct {
	key string
	val value
}

type frame struct {
	obj     bool
	path    string
	members []member
	items   []value
	key     string
	haveKey bool
	seen    map[string]struct{}
}

// parse reads exactly one JSON document. It is iterative (a stack of frames, not recursion), so a hostile body that nests 10,000 levels costs MaxDepth frames before it is
// refused; it refuses invalid UTF-8, a key named twice, trailing data and a body that is not a complete document, each with its own ParseKind.
func parse(body []byte) (value, error) {
	var zero value
	if len(body) > MaxBody {
		return zero, &ParseError{Kind: ParseTooLarge}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return zero, &ParseError{Kind: ParseEmpty}
	}
	if !utf8.Valid(body) {
		return zero, &ParseError{Kind: ParseInvalidUTF8}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var stack []*frame
	var root value
	done := false

	attach := func(v value) {
		if len(stack) == 0 {
			root, done = v, true
			return
		}
		top := stack[len(stack)-1]
		if top.obj {
			top.members = append(top.members, member{key: top.key, val: v})
			top.haveKey = false
			return
		}
		top.items = append(top.items, v)
	}
	here := func() string {
		if len(stack) == 0 {
			return ""
		}
		top := stack[len(stack)-1]
		if top.obj {
			return join(top.path, safeKey(top.key))
		}
		return top.path + "[]"
	}

	for !done {
		tok, err := dec.Token()
		if err != nil {
			return zero, &ParseError{Kind: ParseSyntax, Path: here()}
		}
		if d, isDelim := tok.(json.Delim); isDelim {
			switch d {
			case '{', '[':
				if len(stack) >= MaxDepth {
					return zero, &ParseError{Kind: ParseTooDeep, Path: here()}
				}
				stack = append(stack, &frame{obj: d == '{', path: here(), seen: map[string]struct{}{}})
			case '}', ']':
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if top.obj {
					attach(value{kind: vObject, members: top.members})
				} else {
					attach(value{kind: vArray, items: top.items})
				}
			}
			continue
		}
		if len(stack) > 0 {
			if top := stack[len(stack)-1]; top.obj && !top.haveKey {
				k, ok := tok.(string)
				if !ok {
					return zero, &ParseError{Kind: ParseSyntax, Path: top.path}
				}
				if _, dup := top.seen[k]; dup {
					return zero, &ParseError{Kind: ParseDuplicateKey, Path: join(top.path, safeKey(k))}
				}
				top.seen[k] = struct{}{}
				top.key, top.haveKey = k, true
				continue
			}
		}
		switch t := tok.(type) {
		case string:
			attach(value{kind: vString, str: t})
		case json.Number:
			attach(value{kind: vNumber, str: t.String()})
		case bool:
			attach(value{kind: vBool, str: strconv.FormatBool(t)})
		case nil:
			attach(value{kind: vNull})
		default:
			return zero, &ParseError{Kind: ParseSyntax, Path: here()}
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return zero, &ParseError{Kind: ParseTrailingData}
	}
	return root, nil
}

// safeKey renders a key a body chose for a path or a message: the alphabet [A-Za-z0-9_-] only, at most 40 bytes, anything else as '?'. A key of a body can be anything (an
// address, an id), and the grammar of a path needs a name without dots and brackets.
func safeKey(k string) string {
	if k == "" {
		return "?"
	}
	b := make([]byte, 0, 40)
	for i := 0; i < len(k) && len(b) < 40; i++ {
		c := k[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b = append(b, c)
		} else {
			b = append(b, '?')
		}
	}
	return string(b)
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// display names the document itself in a message.
func display(path string) string {
	if path == "" {
		return "(body)"
	}
	return path
}
