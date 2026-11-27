package render

import (
	"html"
	"strings"
	"unicode"
)

// HTMLToText turns an HTML message body into the plain-text part of the message. It is a small tokenizer, one pass over the input with no
// backtracking, so the work is linear in the size of the input; it never panics.
//
//   - p, h1-h6, blockquote, table and hr put a blank line around their content; div, tr, li, ul and ol (inside a list) and br put a line
//     break; `<br><br>` is a blank line. Cells of a row (td, th) are separated by a space.
//   - li starts with "- ".
//   - a link `<a href="U">T</a>` becomes "T (U)", just "U" when T equals U (or is empty); a relative, javascript:, data: or any other
//     URL that is not http, https, mailto or tel keeps only the text.
//   - every other tag is dropped; the content of script, style and title is dropped; comments, doctype and processing instructions are
//     dropped; a tag that is never closed ends the document (as in a browser).
//   - entities are decoded (the five basics, &nbsp;, &#NNN;, &#xHH; and the other named ones the standard library knows).
//   - every run of white space (a no-break space too) is one space, runs of line breaks are at most one blank line, and the result is trimmed.
//
// What it does not do: <pre> is not preserved, nested lists are not indented, images contribute nothing.
func HTMLToText(src string) string {
	c := &converter{}
	c.run(src)
	return string(c.out)
}

type converter struct {
	out          []byte
	pendingBreak int  // line breaks owed before the next character: 0, 1 or 2
	pendingSpace bool // a space owed before the next character
	bullet       bool // the next character starts a list item: write "- " first
	listDepth    int
	link         linkState
}

type linkState struct {
	open  bool
	href  string
	began bool // a character has been written inside the link
	start int  // len(out) just before the first of them (after any line break, space or bullet it owed)
}

func (c *converter) run(s string) {
	i := 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			c.text(s[i:])
			break
		}
		c.text(s[i : i+lt])
		i += lt
		var next byte
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case strings.HasPrefix(s[i:], "<!--"):
			// Searching from i+2 lets the abrupt forms <!--> and <!---> end the comment, as in a browser.
			end := strings.Index(s[i+2:], "-->")
			if end < 0 {
				i = len(s)
			} else {
				i += 2 + end + 3
			}
		case next == '!' || next == '?':
			i = skipPast(s, i, '>')
		case next == '/' && i+2 < len(s) && isAlpha(s[i+2]):
			name, _, end, ok := scanTag(s, i+2)
			if !ok {
				i = len(s)
				break
			}
			c.tag(name, true, "")
			i = end
		case next == '/':
			i = skipPast(s, i, '>') // `</` and no tag name: a bogus comment
		case isAlpha(next):
			name, href, end, ok := scanTag(s, i+1)
			if !ok {
				i = len(s)
				break
			}
			c.tag(name, false, href)
			i = end
			if name == "script" || name == "style" || name == "title" {
				i = rawTextEnd(s, i, name)
			}
		default:
			c.text("<")
			i++
		}
	}
	c.closeLink()
}

// skipPast returns the index after the first b at or after i, or len(s).
func skipPast(s string, i int, b byte) int {
	k := strings.IndexByte(s[i:], b)
	if k < 0 {
		return len(s)
	}
	return i + k + 1
}

func isAlpha(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func isTagSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// scanTag reads a tag whose name starts at s[nameAt]: its lowercase name, the value of its first href attribute (entities decoded), and the
// index after its closing `>`. A `>` inside a quoted attribute value does not close it. ok is false when the input ends inside the tag.
func scanTag(s string, nameAt int) (name, href string, end int, ok bool) {
	i := nameAt
	for i < len(s) && !isTagSpace(s[i]) && s[i] != '/' && s[i] != '>' {
		i++
	}
	name = strings.ToLower(s[nameAt:i])
	seenHref := false
	for {
		for i < len(s) && (isTagSpace(s[i]) || s[i] == '/') {
			i++
		}
		if i >= len(s) {
			return "", "", len(s), false
		}
		if s[i] == '>' {
			return name, href, i + 1, true
		}
		an := i
		i++ // the first character belongs to the name, even a `=`: that is what guarantees progress
		for i < len(s) && !isTagSpace(s[i]) && s[i] != '/' && s[i] != '>' && s[i] != '=' {
			i++
		}
		attr := s[an:i]
		for i < len(s) && isTagSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return "", "", len(s), false
		}
		if s[i] != '=' {
			continue
		}
		i++
		for i < len(s) && isTagSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return "", "", len(s), false
		}
		var val string
		if q := s[i]; q == '"' || q == '\'' {
			k := strings.IndexByte(s[i+1:], q)
			if k < 0 {
				return "", "", len(s), false
			}
			val = s[i+1 : i+1+k]
			i += k + 2
		} else {
			vs := i
			for i < len(s) && !isTagSpace(s[i]) && s[i] != '>' {
				i++
			}
			val = s[vs:i]
		}
		if !seenHref && strings.EqualFold(attr, "href") {
			seenHref = true
			href = html.UnescapeString(val)
		}
	}
}

// rawTextEnd returns the index after the end tag of a raw-text element (script, style, title) that starts searching at from, or len(s) when
// there is none. The search is one forward pass.
func rawTextEnd(s string, from int, name string) int {
	for j := from; j < len(s); {
		k := strings.IndexByte(s[j:], '<')
		if k < 0 {
			return len(s)
		}
		j += k
		after := j + 2 + len(name)
		if after <= len(s) && s[j+1] == '/' && asciiEqualFold(s[j+2:after], name) &&
			(after == len(s) || isTagSpace(s[after]) || s[after] == '/' || s[after] == '>') {
			return skipPast(s, after, '>')
		}
		j++
	}
	return len(s)
}

func asciiEqualFold(a, lower string) bool {
	if len(a) != len(lower) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ch := a[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		if ch != lower[i] {
			return false
		}
	}
	return true
}

// tag applies one start or end tag.
func (c *converter) tag(name string, closing bool, href string) {
	switch name {
	case "br":
		if !closing && c.pendingBreak < 2 {
			c.pendingBreak++
		}
	case "p", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "table", "hr":
		c.breakAtLeast(2)
	case "div", "tr":
		c.breakAtLeast(1)
	case "td", "th":
		c.pendingSpace = true
	case "ul", "ol":
		if closing && c.listDepth > 0 {
			c.listDepth--
		}
		if c.listDepth == 0 {
			c.breakAtLeast(2)
		} else {
			c.breakAtLeast(1)
		}
		if !closing {
			c.listDepth++
		}
	case "li":
		c.breakAtLeast(1)
		c.bullet = !closing
	case "a":
		c.closeLink()
		if !closing {
			c.link = linkState{open: true, href: href}
		}
	}
}

func (c *converter) breakAtLeast(n int) {
	if c.pendingBreak < n {
		c.pendingBreak = n
	}
}

// text emits the text between two tags: entities decoded, white space collapsed.
func (c *converter) text(s string) {
	if strings.IndexByte(s, '&') >= 0 {
		s = html.UnescapeString(s)
	}
	c.emit(s)
}

// emit writes s, collapsing white space; s is not decoded again.
func (c *converter) emit(s string) {
	for _, r := range s {
		if unicode.IsSpace(r) {
			c.pendingSpace = true
			continue
		}
		c.flush()
		if c.link.open && !c.link.began {
			c.link.began, c.link.start = true, len(c.out)
		}
		c.out = append(c.out, string(r)...)
	}
}

// flush writes what is owed before a character: the line breaks or the space (never at the very start), and the bullet of a list item.
func (c *converter) flush() {
	if len(c.out) > 0 {
		switch {
		case c.pendingBreak > 0:
			for k := 0; k < c.pendingBreak; k++ {
				c.out = append(c.out, '\n')
			}
		case c.pendingSpace:
			c.out = append(c.out, ' ')
		}
	}
	c.pendingBreak, c.pendingSpace = 0, false
	if c.bullet {
		c.out = append(c.out, '-', ' ')
		c.bullet = false
	}
}

// closeLink ends the open link, if any: " (url)" after its text, or the url alone when the link has no text.
func (c *converter) closeLink() {
	l := c.link
	c.link = linkState{}
	if !l.open {
		return
	}
	u := usableURL(l.href)
	if u == "" {
		return
	}
	text := ""
	if l.began {
		text = string(c.out[l.start:])
	}
	switch {
	case text == "":
		c.emit(u)
	case text == u || (strings.HasPrefix(u, "mailto:") && text == u[len("mailto:"):]):
	default:
		c.out = append(c.out, " ("...)
		c.out = append(c.out, u...)
		c.out = append(c.out, ')')
	}
}

// usableURL returns the href, trimmed, when it is an absolute http, https, mailto or tel URL without white space or control characters;
// otherwise "".
func usableURL(href string) string {
	u := strings.TrimSpace(href)
	if u == "" {
		return ""
	}
	for i := 0; i < len(u); i++ {
		if u[i] <= ' ' || u[i] == 0x7f {
			return ""
		}
	}
	colon := strings.IndexByte(u, ':')
	if colon <= 0 || colon == len(u)-1 {
		return ""
	}
	rest := u[colon+1:]
	switch strings.ToLower(u[:colon]) {
	case "http", "https":
		if strings.HasPrefix(rest, "//") && len(rest) > 2 {
			return u
		}
	case "mailto", "tel":
		return u
	}
	return ""
}
