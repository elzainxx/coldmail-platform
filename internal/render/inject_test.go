package render

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// htmlOracle is the reference escaping, a different implementation from the package's: the stdlib's replacer.
var htmlOracle = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

// subjectOracle: every C0 control character is one space, then trimmed.
func subjectOracle(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s))
}

type injection struct {
	name    string
	value   string
	html    string
	text    string
	subject string
}

func bigValue() string  { return strings.Repeat(`<&>"'`, 20000) } // 100 000 bytes
func bigFields() string { return strings.Repeat("{{x}}", 20000) } // 100 000 bytes that look like fields
func bigGroups() string { return strings.Repeat("{a|b}", 20000) } // and like groups

// injectionRows are rendered through the template "[{{x}}]" in every part.
func injectionRows() []injection {
	return []injection{
		{"a field", "{{y}}", "[{{y}}]", "[{{y}}]", "[{{y}}]"},
		{"the field itself", "{{x}}", "[{{x}}]", "[{{x}}]", "[{{x}}]"},
		{"a group", "{a|b}", "[{a|b}]", "[{a|b}]", "[{a|b}]"},
		{"a field with a fallback", "{{y|z}}", "[{{y|z}}]", "[{{y|z}}]", "[{{y|z}}]"},
		{"escapes", `\{ \} \| \\`, `[\{ \} \| \\]`, `[\{ \} \| \\]`, `[\{ \} \| \\]`},
		{"a script", "<script>alert(1)</script>", "[&lt;script&gt;alert(1)&lt;/script&gt;]", "[<script>alert(1)</script>]", "[<script>alert(1)</script>]"},
		{"a closing tag and an image", "</a><img src=x onerror=1>", "[&lt;/a&gt;&lt;img src=x onerror=1&gt;]", "[</a><img src=x onerror=1>]", "[</a><img src=x onerror=1>]"},
		{"double and single quotes", `"d" 's'`, "[&quot;d&quot; &#39;s&#39;]", `["d" 's']`, `["d" 's']`},
		{"an attribute breakout", `" onmouseover="x`, "[&quot; onmouseover=&quot;x]", `[" onmouseover="x]`, `[" onmouseover="x]`},
		{"ampersands and entities", "a & b &amp; c &#39;", "[a &amp; b &amp;amp; c &amp;#39;]", "[a & b &amp; c &#39;]", "[a & b &amp; c &#39;]"},
		{"CR LF and a header", "x\r\nBcc: e@example.com", "[x\r\nBcc: e@example.com]", "[x\r\nBcc: e@example.com]", "[x  Bcc: e@example.com]"},
		{"LF", "x\nBcc: e@example.com", "[x\nBcc: e@example.com]", "[x\nBcc: e@example.com]", "[x Bcc: e@example.com]"},
		{"CR", "x\rBcc: e@example.com", "[x\rBcc: e@example.com]", "[x\rBcc: e@example.com]", "[x Bcc: e@example.com]"},
		{"a NUL", "a\x00b", "[a\x00b]", "[a\x00b]", "[a b]"},
		{"other C0 controls", "\x01\x02\x1f\t", "[\x01\x02\x1f\t]", "[\x01\x02\x1f\t]", "[    ]"},
		{"DEL, C1 and line separators are not C0", "a\x7f\u0085 b", "[a\x7f\u0085 b]", "[a\x7f\u0085 b]", "[a\x7f\u0085 b]"},
		{"non-ASCII", "Ünal 日本 👋", "[Ünal 日本 👋]", "[Ünal 日本 👋]", "[Ünal 日本 👋]"},
		{"100 KB of markup characters", bigValue(), "[" + htmlOracle.Replace(bigValue()) + "]", "[" + bigValue() + "]", "[" + bigValue() + "]"},
		{"100 KB of fields", bigFields(), "[" + bigFields() + "]", "[" + bigFields() + "]", "[" + bigFields() + "]"},
		{"100 KB of groups", bigGroups(), "[" + bigGroups() + "]", "[" + bigGroups() + "]", "[" + bigGroups() + "]"},
	}
}

func injectionCases() []renderCase {
	var cases []renderCase
	for _, r := range injectionRows() {
		v := map[string]string{"x": r.value}
		cases = append(cases,
			renderCase{name: "injection/" + r.name + "/html", src: "[{{x}}]", part: PartHTML, vals: v, want: r.html},
			renderCase{name: "injection/" + r.name + "/text", src: "[{{x}}]", part: PartText, vals: v, want: r.text},
			renderCase{name: "injection/" + r.name + "/subject", src: "[{{x}}]", part: PartSubject, vals: v, want: r.subject},
		)
	}
	return cases
}

func TestInjectionTable(t *testing.T) {
	cases := injectionCases()
	if len(cases) < 3*15 {
		t.Fatalf("the injection table has %d cases", len(cases))
	}
	if failed := checkRenderCases(cases, realRender, func(f string, a ...any) {
		// a 100 KB case would print 100 KB: shorten what is printed
		t.Error(strings.ToValidUTF8(shorten(f, a...), "?"))
	}); len(failed) != 0 {
		t.Errorf("%d of %d cases failed", len(failed), len(cases))
	}
}

func shorten(f string, a ...any) string {
	for i, x := range a {
		if s, ok := x.(string); ok && len(s) > 120 {
			a[i] = s[:60] + "..." + s[len(s)-30:]
		}
	}
	return strings.TrimSpace(fmt.Sprintf(f, a...))
}

// TestInjectionTableControl: renderers that fail the one property each, and the rows written to catch them.
func TestInjectionTableControl(t *testing.T) {
	cases := injectionCases()
	quiet := func(string, ...any) {}
	mutants := []struct {
		name   string
		fn     renderFn
		caught []string
	}{
		{"html is not escaped", func(tpl *Template, seed uint64, _ Part, v func(string) (string, bool)) string {
			return tpl.Render(seed, PartText, v)
		}, []string{"injection/a script/html", "injection/an attribute breakout/html", "injection/100 KB of markup characters/html"}},
		{"a value is parsed again", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			out := tpl.Render(seed, p, v)
			if len(out) > 1000 { // the 100 KB rows would be re-rendered into gigabytes; the small rows catch this mutant
				return out
			}
			again, err := Compile(out)
			if err != nil {
				return out
			}
			return again.Render(seed, p, v)
		}, []string{"injection/a field/html", "injection/a field with a fallback/text", "injection/a field/subject"}},
		{"a value is spun again", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			out := tpl.Render(seed, p, v)
			if len(out) > 1000 {
				return out
			}
			again, err := Compile(out)
			if err != nil {
				return out
			}
			return again.Render(seed, p, nil)
		}, []string{"injection/a group/html", "injection/a group/text"}},
		{"the subject keeps its line breaks", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p == PartSubject {
				p = PartText
			}
			return tpl.Render(seed, p, v)
		}, []string{"injection/CR LF and a header/subject", "injection/a NUL/subject"}},
		{"the subject is escaped like html", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p == PartSubject {
				return subjectOracle(tpl.Render(seed, PartHTML, v))
			}
			return tpl.Render(seed, p, v)
		}, []string{"injection/a script/subject", "injection/double and single quotes/subject"}},
		{"the text part is escaped", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p == PartText {
				p = PartHTML
			}
			return tpl.Render(seed, p, v)
		}, []string{"injection/a script/text"}},
		{"only CR and LF are replaced in the subject", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p != PartSubject {
				return tpl.Render(seed, p, v)
			}
			return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(tpl.Render(seed, PartText, v)))
		}, []string{"injection/a NUL/subject", "injection/other C0 controls/subject"}},
	}
	for _, m := range mutants {
		failed := checkRenderCases(cases, m.fn, quiet)
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

// TestInjectionRandomValues renders random values, built from the characters that matter, through a fixed template and checks the exact
// output against the oracles: a value is never read as template, whatever it holds.
func TestInjectionRandomValues(t *testing.T) {
	alphabet := []string{"{", "}", "|", `\`, "{{", "}}", "{{x}}", "{a|b}", "<", ">", "&", `"`, "'", "\r", "\n", "\x00", "\x1f", " ", "\t", "a", "Z", "é", "日", "&amp;", "<script>"}
	rng := rand.New(rand.NewSource(20260101))
	tpl := mustCompile(t, "<i>{{x}}</i>{p|q}")
	// the spun tail does not depend on the value: it is what the template renders for the part with no values
	tail := func(seed uint64, part Part) string {
		out := tpl.Render(seed, part, nil)
		if !strings.HasPrefix(out, "<i></i>") {
			t.Fatalf("unexpected render %q", out)
		}
		return strings.TrimPrefix(out, "<i></i>")
	}
	nonBlank, blank, qTails := 0, 0, 0
	for i := 0; i < 5000; i++ {
		var b strings.Builder
		for k, n := 0, rng.Intn(14); k < n; k++ {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		v := b.String()
		if strings.TrimSpace(v) == "" { // missing: nothing is rendered for the field
			blank++
			v = ""
		} else {
			nonBlank++
		}
		look := vals(map[string]string{"x": b.String()})
		seed := rng.Uint64()
		if tail(seed, PartHTML) == "q" {
			qTails++
		}
		want := map[Part]string{
			PartHTML:    "<i>" + htmlOracle.Replace(v) + "</i>" + tail(seed, PartHTML),
			PartText:    "<i>" + v + "</i>" + tail(seed, PartText),
			PartSubject: subjectOracle("<i>" + v + "</i>" + tail(seed, PartSubject)),
		}
		for part, w := range want {
			if got := tpl.Render(seed, part, look); got != w {
				t.Fatalf("part %d, value %q: got %q, want %q", part, b.String(), got, w)
			}
		}
	}
	if nonBlank < 3000 || blank == 0 || qTails < 1500 || qTails > 3500 {
		t.Errorf("the sample is not what the test says it is: %d non-blank, %d blank values, %d of 5000 spun tails are q", nonBlank, blank, qTails)
	}
}
