package render

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

type textCase struct {
	name string
	html string
	want string
}

var htmlToTextCases = []textCase{
	// text, whitespace
	{"empty", "", ""},
	{"plain text", "plain text", "plain text"},
	{"white space runs collapse", "  a \n\t b   \r\n c  ", "a b c"},
	{"a no-break space collapses with the others", "a    b", "a b"},
	{"unicode text", "<p>日本語 &amp; café</p>", "日本語 & café"},
	{"inline tags add nothing", "a<b>b</b><i>c</i> <span>d</span>", "abc d"},
	{"uppercase tags", "<P>A</P><BR>B", "A\n\nB"},

	// block elements
	{"one paragraph", "<p>Hello</p>", "Hello"},
	{"two paragraphs are a blank line apart", "<p>One</p><p>Two</p>", "One\n\nTwo"},
	{"divs are a line break apart", "<div>One</div><div>Two</div>", "One\nTwo"},
	{"br is one newline", "One<br>Two", "One\nTwo"},
	{"br variants", "One<br/>Two<br />Three", "One\nTwo\nThree"},
	{"two br are a blank line", "One<br><br>Two", "One\n\nTwo"},
	{"many br are still one blank line", "One<br><br><br><br>Two", "One\n\nTwo"},
	{"br at the start and the end are dropped", "<br><br>text<br>", "text"},
	{"blank lines between blocks collapse", "<p>a</p>\n\n\n<p>b</p>", "a\n\nb"},
	{"heading then paragraph", "<h1>Title</h1><p>Body</p>", "Title\n\nBody"},
	{"all six heading levels break", "<h1>1</h1><h2>2</h2><h3>3</h3><h4>4</h4><h5>5</h5><h6>6</h6>", "1\n\n2\n\n3\n\n4\n\n5\n\n6"},
	{"hr and blockquote", "A<hr>B<blockquote>C</blockquote>D", "A\n\nB\n\nC\n\nD"},
	{"a paragraph inside a div", "<div><p>A</p></div><div>B</div>", "A\n\nB"},

	// lists
	{"unordered list", "<ul><li>A</li><li>B</li></ul>", "- A\n- B"},
	{"ordered list uses the same bullet", "<ol><li>A</li><li>B</li></ol>", "- A\n- B"},
	{"a list between paragraphs", "Before<ul><li>A</li></ul>After", "Before\n\n- A\n\nAfter"},
	{"nested list", "<ul><li>A<ul><li>B</li></ul></li><li>C</li></ul>", "- A\n- B\n- C"},
	{"a paragraph in a list item", "<ul><li><p>A</p></li><li><p>B</p></li></ul>", "- A\n\n- B"},
	{"an empty list item has no bullet", "<ul><li></li><li>B</li></ul>", "- B"},
	{"a stray li", "<li>A</li>", "- A"},

	// tables
	{"table cells and rows", "<table><tr><td>A</td><td>B</td></tr><tr><td>C</td><td>D</td></tr></table>", "A B\nC D"},
	{"table with white space", "<table>\n <tr>\n  <td>A</td>\n  <td></td>\n  <td>B</td>\n </tr>\n</table>", "A B"},
	{"th cells", "<table><tr><th>H1</th><th>H2</th></tr></table>", "H1 H2"},

	// links
	{"a link", `<a href="https://example.com/x">Click here</a>`, "Click here (https://example.com/x)"},
	{"link text equal to the url", `<a href="https://example.com">https://example.com</a>`, "https://example.com"},
	{"a relative url keeps the text only", `<a href="/path">Go</a>`, "Go"},
	{"an anchor keeps the text only", `<a href="#top">Top</a>`, "Top"},
	{"javascript: keeps the text only", `<a href="javascript:alert(1)">Go</a>`, "Go"},
	{"JavaScript: in mixed case keeps the text only", `<a href="JavaScript:alert(1)">Go</a>`, "Go"},
	{"data: keeps the text only", `<a href="data:text/html,x">Go</a>`, "Go"},
	{"mailto with the address as its text", `<a href="mailto:a@example.com">a@example.com</a>`, "a@example.com"},
	{"mailto with other text", `<a href="mailto:a@example.com">Mail me</a>`, "Mail me (mailto:a@example.com)"},
	{"tel", `<a href="tel:+15550100">Call</a>`, "Call (tel:+15550100)"},
	{"an image-only link is the url", `<a href="https://example.com/x"><img src="i.png"></a>`, "https://example.com/x"},
	{"an anchor without href", `<a name="x">Anchor</a>`, "Anchor"},
	{"upper case tag and attribute, single quotes, an entity in the url", `<A HREF='https://example.com/q?a=1&amp;b=2'>Q</A>`, "Q (https://example.com/q?a=1&b=2)"},
	{"an unquoted href", `<a href=https://example.com/z>Z</a>`, "Z (https://example.com/z)"},
	{"a > inside a quoted attribute", `<a title="a>b" href="https://example.com">L</a>`, "L (https://example.com)"},
	{"a link in a sentence", `Visit <a href="https://example.com">our site</a>, thanks.`, "Visit our site (https://example.com), thanks."},
	{"padding around the url", `<a href="  https://example.com  ">T</a>`, "T (https://example.com)"},
	{"white space inside the url", `<a href="https://exa mple.com">T</a>`, "T"},
	{"a scheme with nothing after it", `<a href="https:">x</a>`, "x"},
	{"http without slashes", `<a href="https:example.com">x</a>`, "x"},
	{"a link with a url as the text of a list item", `<ul><li><a href="https://example.com">https://example.com</a></li></ul>`, "- https://example.com"},
	{"a link that is never closed", `<a href="https://example.com">Open`, "Open (https://example.com)"},
	{"a second link closes the first", `<a href="https://a.example">A<a href="https://b.example">B</a>`, "A (https://a.example)B (https://b.example)"},
	{"a stray closing a", "x</a>y", "xy"},
	{"an unsubscribe footer", `<p>Unsubscribe: <a href="https://example.com/u/abc">here</a></p>`, "Unsubscribe: here (https://example.com/u/abc)"},

	// entities
	{"the basic entities", "Fish &amp; Chips &lt;3 &gt; &quot;q&quot; &#39;s&#39; &#65;&#x42;", `Fish & Chips <3 > "q" 's' AB`},
	{"nbsp", "a&nbsp;&nbsp;b", "a b"},
	{"an nbsp-only paragraph vanishes", "<p>&nbsp;</p>x", "x"},
	{"an encoded tag is text, not a tag", "&lt;b&gt;bold&lt;/b&gt;", "<b>bold</b>"},
	{"a numeric reference out of range", "&#0;&#xD800;&#1114112;", "���"},
	{"an unknown entity and a bare ampersand", "&unknown; & done", "&unknown; & done"},
	{"decoded once, not twice", "&amp;lt;", "&lt;"},

	// content that is dropped
	{"script and style", "A<script>var x = '<b>';</script>B<style>p{color:red}</style>C", "ABC"},
	{"an end tag in another case with a space", "A<SCRIPT>x</ScRiPt >B", "AB"},
	{"a script that is never closed", "A<script>alert(1)", "A"},
	{"a script holding something that looks like another end tag", "A<script>if (a</b>) x()</script>B", "AB"},
	{"a comment", "A<!-- hidden <b> -->B", "AB"},
	{"abrupt comments", "A<!-->B<!--->C", "ABC"},
	{"a comment that is never closed", "A<!-- x", "A"},
	{"a document with doctype, head and title", `<!DOCTYPE html><html><head><title>T</title></head><body><p>Hi</p></body></html>`, "Hi"},
	{"a processing instruction", "A<?xml version=1.0?>B", "AB"},
	{"a cdata-like declaration", "A<![CDATA[x]]>B", "AB"},

	// malformed
	{"a paragraph never closed", "<p>unclosed paragraph<p>next", "unclosed paragraph\n\nnext"},
	{"a tag cut off by the end", "text <b", "text"},
	{"an attribute cut off by the end", `text <a href="https://example.com`, "text"},
	{"a less-than followed by a space", "a < b and c > d", "a < b and c > d"},
	{"a less-than followed by a digit", "1<2", "1<2"},
	{"a lone less-than at the end", "a<", "a<"},
	{"stray closing tags", "</p></div>text", "text"},
	{"a closing tag with no name", "a</ >b", "ab"},
	{"attributes without values and with odd names", "<p class>Hi</p><p =x>there</p>", "Hi\n\nthere"},
	{"invalid UTF-8 becomes the replacement character", "a\xffb", "a�b"},
	{"a NUL is kept", "a\x00b", "a\x00b"},
}

type textFn func(string) string

func checkTextCases(cases []textCase, fn textFn, report func(string, ...any)) []string {
	var failed []string
	for _, c := range cases {
		if got := fn(c.html); got != c.want {
			report("%s: HTMLToText(%q) = %q, want %q", c.name, c.html, got, c.want)
			failed = append(failed, c.name)
		}
	}
	return failed
}

func TestHTMLToTextTable(t *testing.T) {
	if len(htmlToTextCases) < 30 {
		t.Fatalf("the table has %d cases; the floor is 30", len(htmlToTextCases))
	}
	if failed := checkTextCases(htmlToTextCases, HTMLToText, t.Errorf); len(failed) != 0 {
		t.Errorf("%d of %d cases failed", len(failed), len(htmlToTextCases))
	}
}

// TestHTMLToTextTableControl: converters that each leave one rule out are caught, by the cases written for that rule.
func TestHTMLToTextTableControl(t *testing.T) {
	quiet := func(string, ...any) {}
	mutants := []struct {
		name   string
		fn     textFn
		caught []string
	}{
		{"strip every tag and nothing else", func(s string) string {
			var b strings.Builder
			in := false
			for i := 0; i < len(s); i++ {
				switch {
				case s[i] == '<':
					in = true
				case s[i] == '>' && in:
					in = false
				case !in:
					b.WriteByte(s[i])
				}
			}
			return strings.Join(strings.Fields(b.String()), " ")
		}, []string{"two paragraphs are a blank line apart", "a link", "unordered list", "script and style", "the basic entities"}},
		{"links without their urls", func(s string) string {
			return HTMLToText(strings.NewReplacer("href=", "data-x=", "HREF=", "data-x=").Replace(s))
		}, []string{"a link", "a link in a sentence", "an unsubscribe footer"}},
		{"entities left as they are", func(s string) string {
			return HTMLToText(strings.ReplaceAll(s, "&", "&amp;"))
		}, []string{"the basic entities", "nbsp", "an encoded tag is text, not a tag"}},
		{"output not trimmed", func(s string) string { return "\n" + HTMLToText(s) + "\n" }, []string{"one paragraph", "plain text"}},
		{"the bullet left out", func(s string) string { return strings.ReplaceAll(HTMLToText(s), "- ", "") }, []string{"unordered list", "nested list"}},
		{"paragraph breaks without a blank line", func(s string) string { return strings.ReplaceAll(HTMLToText(s), "\n\n", "\n") }, []string{"two paragraphs are a blank line apart", "heading then paragraph"}},
	}
	for _, m := range mutants {
		failed := checkTextCases(htmlToTextCases, m.fn, quiet)
		in := map[string]bool{}
		for _, f := range failed {
			in[f] = true
		}
		if len(failed) == 0 {
			t.Errorf("%s: the table did not notice", m.name)
		}
		for _, want := range m.caught {
			if !in[want] {
				t.Errorf("%s: case %q should have failed and did not", m.name, want)
			}
		}
	}
}

// junk builds pseudo-random HTML-ish input from fragments that exercise every branch of the tokenizer.
func junk(rng *rand.Rand, n int) string {
	frags := []string{
		"<", ">", "</", "/>", "<!--", "-->", "<!", "<?", "&", "&amp;", "&#", "&#x", ";", "\"", "'", "=", " ", "\n", "\x00", "\xff",
		"<a", "<a href=", `<a href="`, `<a href="https://example.com">`, "</a>", "<p>", "</p>", "<br>", "<li>", "<ul>", "</ul>",
		"<script>", "</script>", "<style>", "</style", "<title>", "</title>", "<td>", "<tr>", "<table>", "<div", "x", "é", "日本",
		"javascript:", "https://", "mailto:", "href", "<b>", "</b>", "<h1>", "<hr/>", "<!DOCTYPE html>", "<![CDATA[", "]]>",
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(frags[rng.Intn(len(frags))])
	}
	return b.String()
}

func TestHTMLToTextNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	nonEmpty := 0
	for i := 0; i < 30000; i++ {
		in := junk(rng, rng.Intn(40))
		out := HTMLToText(in)
		if out != strings.TrimSpace(out) {
			t.Fatalf("HTMLToText(%q) = %q is not trimmed", in, out)
		}
		if strings.Contains(out, "\n\n\n") || strings.Contains(out, "  ") {
			t.Fatalf("HTMLToText(%q) = %q has an uncollapsed run", in, out)
		}
		if out != "" {
			nonEmpty++
		}
	}
	if nonEmpty < 15000 {
		t.Errorf("only %d of 30000 inputs produced output: the sample does not exercise the text paths", nonEmpty)
	}
	// raw bytes too
	for i := 0; i < 5000; i++ {
		b := make([]byte, rng.Intn(200))
		rng.Read(b)
		_ = HTMLToText(string(b))
	}
	// the control of the invariant check above: it does see an uncollapsed run
	if !strings.Contains("a\n\n\nb", "\n\n\n") || !strings.Contains("a  b", "  ") {
		t.Fatal("the run checks cannot fail")
	}
}

// perCall returns the best time of one call of f over a few batches, each batch long enough (at least 60 ms) for a coarse clock: the Go
// clock on Windows ticks in fractions of a millisecond or more, so a single short call can read as zero.
func perCall(f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for rep := 0; rep < 2; rep++ {
		for n := 1; ; n *= 2 {
			start := time.Now()
			for i := 0; i < n; i++ {
				f()
			}
			if d := time.Since(start); d >= 60*time.Millisecond {
				if per := d / time.Duration(n); per < best {
					best = per
				}
				break
			}
		}
	}
	return best
}

// growth returns time(large)/time(small) for f applied to inputs of those sizes: about large/small for linear work, its square for quadratic.
func growth(f func(string), build func(n int) string, small, large int) float64 {
	a, b := build(small), build(large)
	return float64(perCall(func() { f(b) })) / float64(perCall(func() { f(a) }))
}

func TestHTMLToTextIsLinear(t *testing.T) {
	// 8x the input must cost far less than the 64x of quadratic work; linear work shows about 8x. The bound is generous (24x) because
	// timing is noisy, and the control below proves the instrument sees quadratic work at the same bound.
	const small, large, bound = 40_000, 320_000, 24.0
	patterns := map[string]func(n int) string{
		"unterminated comments":  func(n int) string { return strings.Repeat("<!--", n/4) },
		"unterminated attribute": func(n int) string { return strings.Repeat(`<a href="`, n/9) },
		"unterminated tags":      func(n int) string { return strings.Repeat("<a b=", n/5) },
		"unterminated scripts":   func(n int) string { return strings.Repeat("<script>", n/8) },
		"closing script look-alikes": func(n int) string {
			return "<script>" + strings.Repeat("</scrip</scr", n/12)
		},
		"less-thans":        func(n int) string { return strings.Repeat("<", n) },
		"ampersands":        func(n int) string { return strings.Repeat("&", n) },
		"entity prefixes":   func(n int) string { return strings.Repeat("&amp", n/4) },
		"nested links":      func(n int) string { return strings.Repeat(`<a href="https://example.com/x">t`, n/32) },
		"empty links":       func(n int) string { return strings.Repeat(`<a href="https://example.com/x"></a>`, n/36) },
		"long links":        func(n int) string { return `<a href="https://example.com/">` + strings.Repeat("word ", n/5) + "</a>" },
		"blocks and spaces": func(n int) string { return strings.Repeat("<p> </p>\n<br> &nbsp; ", n/22) },
		"deep lists":        func(n int) string { return strings.Repeat("<ul><li>", n/8) },
		"unclosed lists":    func(n int) string { return strings.Repeat("<li>x", n/5) },
	}
	for name, build := range patterns {
		g := growth(func(s string) { HTMLToText(s) }, build, small, large)
		t.Logf("%s: %dx the input took %.1fx the time", name, large/small, g)
		if g > bound {
			t.Errorf("%s: %dx the input took %.1fx the time (bound %.0fx): the conversion is not linear", name, large/small, g, bound)
		}
	}
	// the control: the same measurement flags a quadratic function
	quadratic := func(s string) {
		var n int
		for i := 0; i < len(s); i++ {
			for j := i; j < len(s); j++ {
				if s[i] == s[j] {
					n++
				}
			}
		}
		_ = n
	}
	build := func(n int) string { return strings.Repeat("ab", n/2) }
	if g := growth(quadratic, build, 2_000, 16_000); g <= bound {
		t.Errorf("the growth measure saw only %.1fx for 8x the input of a quadratic function (bound %.0fx): the test cannot fail", g, bound)
	}
}
