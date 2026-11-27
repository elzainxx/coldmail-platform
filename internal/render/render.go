package render

import "strings"

// Field is a merge field a template uses. HasFallback is true only when EVERY use of the name carries a fallback; a single use without one
// makes the field required (see Missing).
type Field struct {
	Name        string
	HasFallback bool
}

// Template is a compiled template. It is immutable: one Template may be rendered by any number of goroutines at once.
type Template struct {
	nodes  []node
	fields []Field // unique by name, in order of first appearance
	bare   []bool  // parallel to fields: some use of the name has no fallback
}

// index collects the fields of a freshly compiled template, once.
func (t *Template) index() {
	pos := map[string]int{}
	var walk func(ns []node)
	walk = func(ns []node) {
		for i := range ns {
			n := &ns[i]
			switch n.kind {
			case kindField:
				k, seen := pos[n.name]
				if !seen {
					k = len(t.fields)
					pos[n.name] = k
					t.fields = append(t.fields, Field{Name: n.name})
					t.bare = append(t.bare, false)
				}
				if !n.hasFallback {
					t.bare[k] = true
				}
			case kindGroup:
				for _, o := range n.options {
					walk(o)
				}
			}
		}
	}
	walk(t.nodes)
	for k := range t.fields {
		t.fields[k].HasFallback = !t.bare[k]
	}
}

// Fields returns the fields the template uses: unique by name, in order of first appearance, the reserved names only when used. The slice is
// a copy and is never nil.
func (t *Template) Fields() []Field {
	out := []Field{}
	if t != nil {
		out = append(out, t.fields...)
	}
	return out
}

// lookup asks the row for one value, once. A nil lookup has no values. The value is missing, for every purpose, when the second result is
// false: the lookup said false, or the value is empty after strings.TrimSpace. This is the one definition of "missing".
func lookup(values func(string) (string, bool), name string) (value string, present bool) {
	if values == nil {
		return "", false
	}
	v, ok := values(name)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// Missing returns the names, in order of first appearance, that the row is missing and that have no fallback: a row for which this is not
// empty cannot be rendered and the caller excludes it beforehand. The reserved names are never missing (the caller always supplies them).
// A field used only inside one option of a group counts as used: which option is chosen depends on the seed, and a row is excluded or
// kept the same way for every seed.
func (t *Template) Missing(values func(name string) (string, bool)) []string {
	out := []string{}
	if t == nil {
		return out
	}
	for k, f := range t.fields {
		if IsReserved(f.Name) || !t.bare[k] {
			continue
		}
		if _, present := lookup(values, f.Name); !present {
			out = append(out, f.Name)
		}
	}
	return out
}

// Render renders the template for one part of one message.
//
// Spintax is resolved first, by a SplitMix64 generator seeded with PartSeed(seed, part): each group, in document order, draws exactly once
// when it is evaluated, and a group inside an option that was not chosen draws nothing. Then the merge fields are substituted. A value is
// inserted LITERALLY: it is never read as a field or as spintax. It is HTML-escaped in PartHTML, raw in PartText, and in PartSubject raw.
// Literal template text and a fallback are emitted as written (after the escapes are processed). A missing field with a fallback renders
// the fallback; one without renders nothing (the caller excluded the row with Missing). An unknown Part is treated as PartHTML.
//
// The subject is then cleaned as a whole: every C0 control character (CR and LF included) becomes one space and the result is trimmed, so
// that no value, and no template, can end the header line.
func (t *Template) Render(seed uint64, part Part, values func(name string) (string, bool)) string {
	if t == nil {
		return ""
	}
	r := renderer{gen: splitMix64{state: PartSeed(seed, part)}, part: part, values: values}
	r.walk(t.nodes)
	out := r.b.String()
	if part == PartSubject {
		out = cleanSubject(out)
	}
	return out
}

type renderer struct {
	b      strings.Builder
	gen    splitMix64
	part   Part
	values func(string) (string, bool)
}

func (r *renderer) walk(ns []node) {
	for i := range ns {
		n := &ns[i]
		switch n.kind {
		case kindText:
			r.b.WriteString(n.text)
		case kindGroup:
			r.walk(n.options[r.gen.intn(len(n.options))])
		case kindField:
			v, present := lookup(r.values, n.name)
			if !present {
				if n.hasFallback {
					r.b.WriteString(n.text)
				}
				continue
			}
			if r.part == PartText || r.part == PartSubject {
				r.b.WriteString(v)
			} else {
				writeHTMLEscaped(&r.b, v)
			}
		}
	}
}

// writeHTMLEscaped writes s with & < > " ' replaced by character references.
func writeHTMLEscaped(b *strings.Builder, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		var rep string
		switch s[i] {
		case '&':
			rep = "&amp;"
		case '<':
			rep = "&lt;"
		case '>':
			rep = "&gt;"
		case '"':
			rep = "&quot;"
		case '\'':
			rep = "&#39;"
		default:
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString(rep)
		last = i + 1
	}
	b.WriteString(s[last:])
}

// cleanSubject turns every C0 control character into one space and trims the result.
func cleanSubject(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			bs := []byte(s)
			for j := i; j < len(bs); j++ {
				if bs[j] < 0x20 {
					bs[j] = ' '
				}
			}
			s = string(bs)
			break
		}
	}
	return strings.TrimSpace(s)
}
