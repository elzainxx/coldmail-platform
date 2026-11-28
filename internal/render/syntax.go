// Package render is the template language of the platform: merge fields, spintax and the plain-text view of an HTML message.
//
// The grammar (Docs/templates.md is the customer-facing page; Docs/phase-5-spec.md, decision 1, is the ruling):
//
//	{{name}}            a merge field; name is [A-Za-z_][A-Za-z0-9_]*, at most 64 characters
//	{{name|fallback}}   the same, with a fallback used when the row has no value: plain template text up to the closing }}
//	{a|b|c}             a spintax group: at least two options, at most 64, nestable to depth 8; an option holds text, fields and groups
//	\{ \} \| \\         escapes: the literal character; any other backslash is literal
//
// `{{` always opens a merge field, so {{first_name|there}} is never a group; a literal `{{` is written \{\{.
// A `|` outside every group and field is literal. A stray `}`, an unclosed `{` or `{{`, an invalid or empty field name, a group with fewer
// than two options, more than 64 options or nesting deeper than 8 are syntax errors that carry their position.
//
// This package imports the standard library only (a test proves it), and it never uses math/rand: the generator that chooses the spintax
// options is implemented here (SplitMix64, seed.go) so that a given seed gives a given choice for as long as the platform exists.
package render

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxNameLen is the longest field name, in characters (a name is ASCII).
	MaxNameLen = 64
	// MaxDepth is the deepest nesting of spintax groups.
	MaxDepth = 8
	// MaxOptions is the largest number of options in one group.
	MaxOptions = 64

	// FieldEmail and FieldUnsubscribeURL are the two reserved field names: the renderer's caller always supplies them.
	FieldEmail          = "email"
	FieldUnsubscribeURL = "unsubscribe_url"
)

// IsReserved reports whether name is one of the two reserved field names.
func IsReserved(name string) bool { return name == FieldEmail || name == FieldUnsubscribeURL }

// ValidFieldName reports whether name is a merge-field name the grammar accepts: a letter or an underscore, then letters, digits and underscores, at most MaxNameLen characters (all ASCII). It is the
// rule `{{name}}` is parsed by, for a caller that has to say which keys of a row's merge data a template could ever name.
func ValidFieldName(name string) bool {
	if len(name) == 0 || len(name) > MaxNameLen || !isNameStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isNameChar(name[i]) {
			return false
		}
	}
	return true
}

// SyntaxError is what Compile returns for a template that does not follow the grammar. Offset is a byte offset into the source,
// Line and Column are 1-based (Column counts runes; lines end at \n). Message never repeats more than one character of the source.
type SyntaxError struct {
	Offset  int
	Line    int
	Column  int
	Message string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("template syntax error at line %d, column %d: %s", e.Line, e.Column, e.Message)
}

type nodeKind uint8

const (
	kindText nodeKind = iota
	kindField
	kindGroup
)

// node is one element of a compiled template. kindText holds its literal in text; kindField its name and, when hasFallback, the fallback
// template text in text; kindGroup its options.
type node struct {
	kind        nodeKind
	text        string
	name        string
	hasFallback bool
	options     [][]node
}

type parser struct {
	src string
	pos int
}

func isNameStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isNameChar(c byte) bool  { return isNameStart(c) || (c >= '0' && c <= '9') }
func isEscapable(c byte) bool { return c == '{' || c == '}' || c == '|' || c == '\\' }

// fail builds a SyntaxError at a byte offset of the source.
func (p *parser) fail(off int, msg string) *SyntaxError {
	if off > len(p.src) {
		off = len(p.src)
	}
	head := p.src[:off]
	lineStart := strings.LastIndexByte(head, '\n') + 1
	return &SyntaxError{
		Offset:  off,
		Line:    1 + strings.Count(head, "\n"),
		Column:  1 + utf8.RuneCountInString(head[lineStart:]),
		Message: msg,
	}
}

// Compile parses src. A syntax error is a *SyntaxError.
func Compile(src string) (*Template, error) {
	p := &parser{src: src}
	nodes, _, err := p.nodes(0)
	if err != nil {
		return nil, err
	}
	t := &Template{nodes: nodes}
	t.index()
	return t, nil
}

// nodes parses a run of template text: the whole source at depth 0, or one option of a group at depth >= 1. At depth >= 1 it stops, without
// consuming it, at the `|` (term '|') or `}` (term '}') that ends the option; term 0 means the end of the source.
func (p *parser) nodes(depth int) (out []node, term byte, err *SyntaxError) {
	var text []byte
	flush := func() {
		if len(text) > 0 {
			out = append(out, node{kind: kindText, text: string(text)})
			text = text[:0]
		}
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '\\':
			if p.pos+1 < len(p.src) && isEscapable(p.src[p.pos+1]) {
				text = append(text, p.src[p.pos+1])
				p.pos += 2
			} else {
				text = append(text, '\\')
				p.pos++
			}
		case '{':
			flush()
			var n node
			var e *SyntaxError
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == '{' {
				n, e = p.field()
			} else {
				n, e = p.group(depth + 1)
			}
			if e != nil {
				return nil, 0, e
			}
			out = append(out, n)
		case '}':
			if depth == 0 {
				return nil, 0, p.fail(p.pos, `unexpected }: write \} for a literal brace`)
			}
			flush()
			return out, '}', nil
		case '|':
			if depth == 0 {
				text = append(text, '|')
				p.pos++
				continue
			}
			flush()
			return out, '|', nil
		default:
			text = append(text, c)
			p.pos++
		}
	}
	flush()
	return out, 0, nil
}

// group parses `{a|b|...}`; p.pos is at the opening brace and depth is this group's own depth (1 for a top-level group).
func (p *parser) group(depth int) (node, *SyntaxError) {
	start := p.pos
	if depth > MaxDepth {
		return node{}, p.fail(start, "groups are nested more than 8 deep")
	}
	p.pos++
	var opts [][]node
	for {
		ns, term, err := p.nodes(depth)
		if err != nil {
			return node{}, err
		}
		opts = append(opts, ns)
		switch term {
		case 0:
			return node{}, p.fail(start, `unclosed {: close the group with } or write \{`)
		case '|':
			if len(opts) == MaxOptions {
				return node{}, p.fail(p.pos, "a group has at most 64 options")
			}
			p.pos++
		default: // '}'
			p.pos++
			if len(opts) < 2 {
				return node{}, p.fail(start, `a group needs at least two options, {a|b}; write \{ and \} for literal braces`)
			}
			return node{kind: kindGroup, options: opts}, nil
		}
	}
}

// field parses `{{name}}` or `{{name|fallback}}`; p.pos is at the first brace.
func (p *parser) field() (node, *SyntaxError) {
	start := p.pos
	unclosed := func() (node, *SyntaxError) {
		return node{}, p.fail(start, `unclosed {{: close the field with }} or write \{\{`)
	}
	p.pos += 2
	nameStart := p.pos
	if p.pos >= len(p.src) {
		return unclosed()
	}
	switch c := p.src[p.pos]; {
	case c == '}' || c == '|':
		return node{}, p.fail(p.pos, "empty field name")
	case c == '{':
		return node{}, p.fail(p.pos, "a group cannot begin with a field: put text before the field or reorder the options")
	case !isNameStart(c):
		return node{}, p.fail(p.pos, "invalid field name: it must start with a letter or an underscore")
	}
	for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
		if p.pos-nameStart == MaxNameLen {
			return node{}, p.fail(p.pos, "field name is longer than 64 characters")
		}
		p.pos++
	}
	name := p.src[nameStart:p.pos]
	if p.pos >= len(p.src) {
		return unclosed()
	}
	switch p.src[p.pos] {
	case '}':
		if p.pos+1 < len(p.src) && p.src[p.pos+1] == '}' {
			p.pos += 2
			return node{kind: kindField, name: name}, nil
		}
		if p.pos+1 >= len(p.src) {
			return unclosed()
		}
		return node{}, p.fail(p.pos, "a field is closed with }}, not a single }")
	case '|':
		p.pos++
	default:
		r, _ := utf8.DecodeRuneInString(p.src[p.pos:])
		return node{}, p.fail(p.pos, fmt.Sprintf("invalid character %q in a field name", r))
	}
	var fb []byte
	for {
		if p.pos >= len(p.src) {
			return unclosed()
		}
		switch c := p.src[p.pos]; c {
		case '\\':
			if p.pos+1 < len(p.src) && isEscapable(p.src[p.pos+1]) {
				fb = append(fb, p.src[p.pos+1])
				p.pos += 2
			} else {
				fb = append(fb, '\\')
				p.pos++
			}
		case '{':
			return node{}, p.fail(p.pos, `a fallback is plain text, with no field or group: write \{ for a brace`)
		case '}':
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == '}' {
				p.pos += 2
				return node{kind: kindField, name: name, hasFallback: true, text: string(fb)}, nil
			}
			if p.pos+1 >= len(p.src) {
				return unclosed()
			}
			return node{}, p.fail(p.pos, `a single } in a fallback must be written \}`)
		default:
			fb = append(fb, c)
			p.pos++
		}
	}
}
