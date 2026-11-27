package render

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// vals is the lookup of a row: a name the row has returns (value, true), any other (value "", false).
func vals(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func mustCompile(t testing.TB, src string) *Template {
	t.Helper()
	tpl, err := Compile(src)
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return tpl
}

// renderFn is what the table tests call, so that a control can swap in a broken renderer and prove the table notices.
type renderFn func(tpl *Template, seed uint64, part Part, values func(string) (string, bool)) string

func realRender(tpl *Template, seed uint64, part Part, values func(string) (string, bool)) string {
	return tpl.Render(seed, part, values)
}

type renderCase struct {
	name string
	src  string
	seed uint64
	part Part
	vals map[string]string
	want string
}

// checkRenderCases runs every case through fn and returns the names of the cases that did not render to their expected output.
// report receives one message per failure (t.Errorf for the real test, a no-op for a control).
func checkRenderCases(cases []renderCase, fn renderFn, report func(string, ...any)) []string {
	var failed []string
	for _, c := range cases {
		tpl, err := Compile(c.src)
		if err != nil {
			report("%s: Compile(%q): %v", c.name, c.src, err)
			failed = append(failed, c.name)
			continue
		}
		if got := fn(tpl, c.seed, c.part, vals(c.vals)); got != c.want {
			report("%s: Render(%q, seed %d, part %d) = %q, want %q", c.name, c.src, c.seed, c.part, got, c.want)
			failed = append(failed, c.name)
		}
	}
	return failed
}

// The draws behind the expectations were computed with an independent SplitMix64 written in Python (index = high 64 bits of output*n):
//
//	seed 0   n=2: 1 0 0 1 0 0   n=3: 2 1 0 2 0 0   n=4: 3 1 0 3 0 1
//	seed 1   n=2: 1 1 1 0 0 1   n=3: 1 2 2 1 1 2   n=4: 2 2 3 1 1 3
//	seed 3   n=2: 0 1 1 0 0 1   n=3: 0 2 1 0 0 1   n=4: 0 2 2 0 0 2
//	seed 42  n=2: 1 0 0 0 0 1   n=3: 2 0 0 1 0 2   n=4: 2 0 1 1 0 3
//
// and the subject stream (PartSeed of PartSubject), for n=3, first draw: seed 0 -> 0, seed 3 -> 2.
var renderCases = []renderCase{
	// literal text and fields
	{name: "empty template", src: "", want: ""},
	{name: "plain text", src: "Hello world", want: "Hello world"},
	{name: "one field", src: "{{first_name}}", vals: map[string]string{"first_name": "Ann"}, want: "Ann"},
	{name: "field in text", src: "Hi {{first_name}}, welcome", vals: map[string]string{"first_name": "Ann"}, want: "Hi Ann, welcome"},
	{name: "two fields adjacent", src: "{{a}}{{b}}", vals: map[string]string{"a": "1", "b": "2"}, want: "12"},
	{name: "same field twice", src: "{{x}} and {{x}}", vals: map[string]string{"x": "Q"}, want: "Q and Q"},
	{name: "value kept untrimmed", src: "[{{x}}]", vals: map[string]string{"x": " a "}, want: "[ a ]"},
	{name: "value zero is a value", src: "{{x}}", vals: map[string]string{"x": "0"}, want: "0"},
	{name: "underscore and digits in a name", src: "{{_a1}}-{{A_B_2}}", vals: map[string]string{"_a1": "u", "A_B_2": "v"}, want: "u-v"},
	{name: "64-character name", src: "{{" + strings.Repeat("n", 64) + "}}", vals: map[string]string{strings.Repeat("n", 64): "ok"}, want: "ok"},
	{name: "unicode text and value", src: "Привет {{first_name}} 👋", vals: map[string]string{"first_name": "Ünal"}, want: "Привет Ünal 👋"},
	{name: "CRLF and newlines in the template are kept", src: "a\r\nb\nc", want: "a\r\nb\nc"},
	{name: "tags in the template are emitted as written", src: "<b>{{x}}</b>", vals: map[string]string{"x": "y"}, want: "<b>y</b>"},

	// {{a|b}} is a field with a fallback, never a group
	{name: "a|b is a field: fallback when missing", src: "{{a|b}}", want: "b"},
	{name: "a|b is a field: value when present", src: "{{a|b}}", vals: map[string]string{"a": "X"}, want: "X"},
	{name: "fallback when absent", src: "Hi {{first_name|there}}", want: "Hi there"},
	{name: "fallback when empty", src: "Hi {{first_name|there}}", vals: map[string]string{"first_name": ""}, want: "Hi there"},
	{name: "fallback when whitespace only", src: "Hi {{first_name|there}}", vals: map[string]string{"first_name": " \t\r\n "}, want: "Hi there"},
	{name: "missing without a fallback renders nothing", src: "Hi {{first_name}}!", want: "Hi !"},
	{name: "empty fallback", src: "a{{x|}}b", want: "ab"},
	{name: "fallback may hold a bar", src: "{{x|a|b}}", want: "a|b"},
	{name: "fallback escapes", src: `{{x|a\|b\}\}c\\d}}`, want: `a|b}}c\d`},
	{name: "fallback keeps a non-escape backslash", src: `{{x|a\nb}}`, want: `a\nb`},
	{name: "fallback is template text, not escaped", src: "{{x|<b>hey</b> & co}}", want: "<b>hey</b> & co"},
	{name: "fallback is not escaped in the subject either", src: "{{x|a & b}}", part: PartSubject, want: "a & b"},

	// spintax
	{name: "group of three, seed 0", src: "{a|b|c}", seed: 0, want: "c"},
	{name: "group of three, seed 1", src: "{a|b|c}", seed: 1, want: "b"},
	{name: "group of three, seed 3", src: "{a|b|c}", seed: 3, want: "a"},
	{name: "group of two, seed 0", src: "{a|b}", seed: 0, want: "b"},
	{name: "group of two, seed 3", src: "{a|b}", seed: 3, want: "a"},
	{name: "group of four, seed 1", src: "{a|b|c|d}", seed: 1, want: "c"},
	{name: "group of four, seed 0", src: "{a|b|c|d}", seed: 0, want: "d"},
	{name: "empty last option, seed 0", src: "x{a|}y", seed: 0, want: "xy"},
	{name: "empty last option, seed 3", src: "x{a|}y", seed: 3, want: "xay"},
	{name: "two empty options", src: "x{|}y", want: "xy"},
	{name: "adjacent groups, seed 0", src: "{a|b}{c|d}", seed: 0, want: "bc"},
	{name: "adjacent groups, seed 1", src: "{a|b}{c|d}", seed: 1, want: "bd"},
	{name: "adjacent groups, seed 3", src: "{a|b}{c|d}", seed: 3, want: "ad"},
	{name: "nested, inner chosen then drawn (seed 0)", src: "{a|{b|c}}", seed: 0, want: "b"},
	{name: "nested, inner chosen then drawn (seed 1)", src: "{a|{b|c}}", seed: 1, want: "c"},
	{name: "nested, outer takes the plain option", src: "{a|{b|c}}", seed: 3, want: "a"},
	{name: "a nested group of an unchosen option draws nothing (seed 3)", src: "{a|{b|c}}{d|e|f|g}", seed: 3, want: "af"},
	{name: "nested groups then a group (seed 0)", src: "{a|{b|c}}{d|e|f|g}", seed: 0, want: "bd"},
	{name: "nested groups then a group (seed 1)", src: "{a|{b|c}}{d|e|f|g}", seed: 1, want: "cg"},
	{name: "a field in a group option, chosen", src: "{Hi {{n}}|Hello {{n}}}", seed: 0, vals: map[string]string{"n": "Ann"}, want: "Hello Ann"},
	{name: "a field in a group option, other option", src: "{Hi {{n}}|Hello {{n}}}", seed: 3, vals: map[string]string{"n": "Ann"}, want: "Hi Ann"},
	{name: "x|{{y|z}}: the field option with its fallback", src: "{x|{{y|z}}}", seed: 0, want: "z"},
	{name: "x|{{y|z}}: the field option with a value", src: "{x|{{y|z}}}", seed: 0, vals: map[string]string{"y": "Q"}, want: "Q"},
	{name: "x|{{y|z}}: the text option", src: "{x|{{y|z}}}", seed: 3, want: "x"},
	{name: "group option holding text around a field", src: "{a|b {{n}} c}", seed: 0, vals: map[string]string{"n": "N"}, want: "b N c"},
	{name: "unicode options", src: "{héllo|wörld|日本}", seed: 0, want: "日本"},
	{name: "html tags in options", src: "<b>{a|b}</b>", seed: 0, want: "<b>b</b>"},
	{name: "a field at the start of an option after a space", src: "{ {{x}}|b}", seed: 3, vals: map[string]string{"x": "Z"}, want: " Z"},

	// escapes
	{name: "escaped braces and bar are literal", src: `\{a|b\}`, want: "{a|b}"},
	{name: "escaped double brace is not a field", src: `\{\{x\}\}`, vals: map[string]string{"x": "no"}, want: "{{x}}"},
	{name: "escaped backslash", src: `a\\b`, want: `a\b`},
	{name: "escaped backslash then a group", src: `\\{a|b}`, seed: 3, want: `\a`},
	{name: "any other backslash is literal", src: `a\nb\tc\`, want: `a\nb\tc\`},
	{name: "a bar outside a group is literal", src: "a|b", want: "a|b"},
	{name: "an escaped bar outside a group", src: `a\|b`, want: "a|b"},
	{name: "an escaped bar inside an option", src: `{a\|b|c}`, seed: 3, want: "a|b"},
	{name: "an escaped bar inside an option, other option", src: `{a\|b|c}`, seed: 0, want: "c"},
	{name: "css braces escaped", src: `p \{ color: red \}`, want: "p { color: red }"},

	// values are literal
	{name: "a value that looks like a field", src: "{{x}}", vals: map[string]string{"x": "{{y}}", "y": "no"}, want: "{{y}}"},
	{name: "a value that looks like a group", src: "{{x}}", vals: map[string]string{"x": "{a|b}"}, want: "{a|b}"},
	{name: "a value with escapes", src: "{{x}}", vals: map[string]string{"x": `\{ \} \| \\`}, want: `\{ \} \| \\`},
	{name: "a value that opens a group is not joined to the template", src: "{{x}}a|b", vals: map[string]string{"x": "{"}, want: "{a|b"},
	{name: "html escapes the five characters", src: "{{x}}", vals: map[string]string{"x": `<b>&"'`}, want: "&lt;b&gt;&amp;&quot;&#39;"},
	{name: "text keeps the five characters", src: "{{x}}", part: PartText, vals: map[string]string{"x": `<b>&"'`}, want: `<b>&"'`},
	{name: "subject keeps the five characters", src: "{{x}}", part: PartSubject, vals: map[string]string{"x": `<b>&"'`}, want: `<b>&"'`},
	{name: "html escaping of an attribute value", src: `<a href="{{u}}">`, vals: map[string]string{"u": `" onmouseover="x`}, want: `<a href="&quot; onmouseover=&quot;x">`},
	{name: "reserved email field", src: "{{email}}", vals: map[string]string{"email": "a+b@example.com"}, want: "a+b@example.com"},
	{name: "reserved unsubscribe_url is escaped in html", src: `<a href="{{unsubscribe_url}}">`, vals: map[string]string{"unsubscribe_url": "https://example.com/u?a=1&b=2"}, want: `<a href="https://example.com/u?a=1&amp;b=2">`},
	{name: "reserved unsubscribe_url is raw in text", src: "{{unsubscribe_url}}", part: PartText, vals: map[string]string{"unsubscribe_url": "https://example.com/u?a=1&b=2"}, want: "https://example.com/u?a=1&b=2"},
	{name: "an unknown part escapes like html", src: "{{x}}", part: Part(99), vals: map[string]string{"x": "<"}, want: "&lt;"},

	// parts and streams
	{name: "text part spins like the html part", src: "{a|b|c}", part: PartText, seed: 0, want: "c"},
	{name: "subject spins on its own stream, seed 0", src: "{a|b|c}", part: PartSubject, seed: 0, want: "a"},
	{name: "subject spins on its own stream, seed 3", src: "{a|b|c}", part: PartSubject, seed: 3, want: "c"},
	{name: "subject: CR LF become two spaces", src: "Hi {{x}}!", part: PartSubject, vals: map[string]string{"x": "A\r\nBcc: e@example.com"}, want: "Hi A  Bcc: e@example.com!"},
	{name: "subject: C0 controls and tab become spaces", src: "{{x}}", part: PartSubject, vals: map[string]string{"x": "a\tb\x00c\x1fd"}, want: "a b c d"},
	{name: "subject: result is trimmed", src: "{{x}}", part: PartSubject, vals: map[string]string{"x": "\n\r hi \r\n"}, want: "hi"},
	{name: "subject: trimmed when the fallback is empty", src: "{{x|}} hello", part: PartSubject, want: "hello"},
	{name: "subject: DEL and non-ASCII are not C0", src: "{{x}}", part: PartSubject, vals: map[string]string{"x": "a\x7fé"}, want: "a\x7fé"},
	{name: "subject: a control character of the template is cleaned too", src: "a\nb", part: PartSubject, want: "a b"},
	{name: "html: a NUL is kept", src: "{{x}}", vals: map[string]string{"x": "a\x00b"}, want: "a\x00b"},
	{name: "text: CR LF are kept", src: "{{x}}", part: PartText, vals: map[string]string{"x": "a\r\nb"}, want: "a\r\nb"},
}

func TestRenderTable(t *testing.T) {
	if len(renderCases) < 40 {
		t.Fatalf("the render table has %d cases; the floor is 40", len(renderCases))
	}
	names := map[string]bool{}
	for _, c := range renderCases {
		if names[c.name] {
			t.Errorf("duplicate case name %q", c.name)
		}
		names[c.name] = true
	}
	if failed := checkRenderCases(renderCases, realRender, t.Errorf); len(failed) != 0 {
		t.Errorf("%d of %d cases failed", len(failed), len(renderCases))
	}
}

// TestRenderTableControl proves the table can fail: each broken renderer must be caught, and by the cases that exist to catch it.
func TestRenderTableControl(t *testing.T) {
	quiet := func(string, ...any) {}
	mutants := []struct {
		name   string
		fn     renderFn
		caught []string // cases that must be among the failures
	}{
		{"a renderer that ignores the seed", func(tpl *Template, _ uint64, p Part, v func(string) (string, bool)) string {
			return tpl.Render(12345, p, v)
		}, []string{"group of three, seed 0", "group of three, seed 1", "adjacent groups, seed 0"}},
		{"a renderer started from the wrong seed", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			return tpl.Render(seed+1, p, v)
		}, []string{"nested, inner chosen then drawn (seed 0)"}},
		{"a renderer that does not escape html", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			return tpl.Render(seed, PartText, v)
		}, []string{"html escapes the five characters", "html escaping of an attribute value", "reserved unsubscribe_url is escaped in html"}},
		{"a renderer that escapes the text part", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p == PartText {
				p = PartHTML
			}
			return tpl.Render(seed, p, v)
		}, []string{"text keeps the five characters"}},
		{"a renderer that re-parses the output", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			out := tpl.Render(seed, p, v)
			again, err := Compile(out)
			if err != nil {
				return out
			}
			return again.Render(seed, p, v)
		}, []string{"a value that looks like a field", "a value that looks like a group", "a value with escapes"}},
		{"a renderer that joins the subject and html streams", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p == PartSubject {
				p = PartHTML
			}
			return tpl.Render(seed, p, v)
		}, []string{"subject spins on its own stream, seed 0", "subject: CR LF become two spaces"}},
		{"a renderer that leaves the subject as it is", func(tpl *Template, seed uint64, p Part, v func(string) (string, bool)) string {
			if p == PartSubject {
				p = PartText
			}
			return tpl.Render(seed, p, v)
		}, []string{"subject: CR LF become two spaces", "subject: result is trimmed"}},
	}
	for _, m := range mutants {
		failed := checkRenderCases(renderCases, m.fn, quiet)
		in := map[string]bool{}
		for _, f := range failed {
			in[f] = true
		}
		if len(failed) == 0 {
			t.Errorf("%s: the table did not notice", m.name)
			continue
		}
		for _, want := range m.caught {
			if !in[want] {
				t.Errorf("%s: case %q should have failed and did not (failed: %v)", m.name, want, failed)
			}
		}
	}
}

// TestFallbackRule pairs each side of the rule: a missing field with a fallback renders the fallback, a present one never does.
func TestFallbackRule(t *testing.T) {
	tpl := mustCompile(t, "{{n|FB}}")
	cases := []struct {
		name string
		look func(string) (string, bool)
		want string
	}{
		{"absent", vals(nil), "FB"},
		{"lookup says false but returns a value", func(string) (string, bool) { return "V", false }, "FB"},
		{"empty", vals(map[string]string{"n": ""}), "FB"},
		{"spaces", vals(map[string]string{"n": "   "}), "FB"},
		{"unicode space (no-break, ideographic)", vals(map[string]string{"n": " 　"}), "FB"},
		{"nil lookup", nil, "FB"},
		{"one visible character", vals(map[string]string{"n": "."}), "."},
		{"a value with padding is not trimmed", vals(map[string]string{"n": " V "}), " V "},
	}
	for _, c := range cases {
		for _, part := range []Part{PartHTML, PartText, PartSubject} {
			got := tpl.Render(1, part, c.look)
			want := c.want
			if part == PartSubject {
				want = strings.TrimSpace(want)
			}
			if got != want {
				t.Errorf("%s, part %d: got %q, want %q", c.name, part, got, want)
			}
		}
	}
	// the control: without a fallback the same rows render nothing, so the fallback above is what produced the text
	bare := mustCompile(t, "{{n}}")
	if got := bare.Render(1, PartHTML, vals(nil)); got != "" {
		t.Errorf("a missing field without a fallback rendered %q, want nothing", got)
	}
}

// TestGroupDrawsOncePerEvaluation checks the draw order against a reference interpreter written in the test from the rule alone:
// one draw per group, in document order, when the group is evaluated; a group inside an option that was not chosen draws nothing.
func TestGroupDrawsOncePerEvaluation(t *testing.T) {
	const src = "{a|{x|y}|c}-{d|e}-{f|{g|{h|i}}}"
	tpl := mustCompile(t, src)
	pick := func(g *splitMix64, opts ...string) string { return opts[g.intn(len(opts))] }
	lazy := func(seed uint64) string {
		g := splitMix64{state: seed}
		var b strings.Builder
		switch g.intn(3) { // {a|{x|y}|c}
		case 0:
			b.WriteString("a")
		case 1:
			b.WriteString(pick(&g, "x", "y"))
		default:
			b.WriteString("c")
		}
		b.WriteString("-" + pick(&g, "d", "e") + "-")
		if g.intn(2) == 0 { // {f|{g|{h|i}}}
			b.WriteString("f")
		} else if g.intn(2) == 0 {
			b.WriteString("g")
		} else {
			b.WriteString(pick(&g, "h", "i"))
		}
		return b.String()
	}
	// eager draws for every group of the template up front, chosen or not: the mistake this rule exists to rule out
	eager := func(seed uint64) string {
		g := splitMix64{state: seed}
		outer1, inner1, de, outer3, mid3, inner3 := g.intn(3), g.intn(2), g.intn(2), g.intn(2), g.intn(2), g.intn(2)
		first := []string{"a", []string{"x", "y"}[inner1], "c"}[outer1]
		last := "f"
		if outer3 == 1 {
			last = "g"
			if mid3 == 1 {
				last = []string{"h", "i"}[inner3]
			}
		}
		return first + "-" + []string{"d", "e"}[de] + "-" + last
	}
	seen, eagerDiffers := map[string]bool{}, 0
	for seed := uint64(0); seed < 3000; seed++ {
		got := tpl.Render(seed, PartHTML, nil)
		if want := lazy(seed); got != want {
			t.Fatalf("seed %d: Render = %q, reference = %q", seed, got, want)
		}
		seen[got] = true
		if eager(seed) != got {
			eagerDiffers++
		}
	}
	// the controls: the reference reaches every branch, and an eager renderer is visibly different
	for _, must := range []string{"a-d-f", "x-e-g", "y-d-h", "c-e-i"} {
		if !seen[must] {
			t.Errorf("no seed in 0..2999 produced %q: the reference does not exercise every branch", must)
		}
	}
	if eagerDiffers < 300 {
		t.Errorf("an eager renderer differs from Render on only %d of 3000 seeds: the test cannot see a wasted draw", eagerDiffers)
	}
}

// TestRenderIsSafeForConcurrentUse renders one template from many goroutines; every result must equal the sequential one.
func TestRenderIsSafeForConcurrentUse(t *testing.T) {
	tpl := mustCompile(t, "{Hi|Hello|Hey} {{first_name|there}}, {a|b|c}{{x}}")
	row := vals(map[string]string{"x": "<&>"})
	want := make([]string, 64)
	for i := range want {
		want[i] = tpl.Render(uint64(i), PartHTML, row)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64*8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range want {
				if got := tpl.Render(uint64(i), PartHTML, row); got != want[i] {
					errs <- fmt.Sprintf("seed %d: %q != %q", i, got, want[i])
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// the control: the seeds do produce different renders, so equality above is not the equality of a constant
	distinct := map[string]bool{}
	for _, w := range want {
		distinct[w] = true
	}
	if len(distinct) < 8 {
		t.Errorf("only %d distinct renders over 64 seeds", len(distinct))
	}
}

func TestNilTemplateAndNilLookup(t *testing.T) {
	var tpl *Template
	if got := tpl.Render(1, PartHTML, nil); got != "" {
		t.Errorf("nil template rendered %q", got)
	}
	if got := tpl.Fields(); got == nil || len(got) != 0 {
		t.Errorf("nil template Fields = %#v, want an empty non-nil slice", got)
	}
	if got := tpl.Missing(nil); got == nil || len(got) != 0 {
		t.Errorf("nil template Missing = %#v, want an empty non-nil slice", got)
	}
	real := mustCompile(t, "a{{x|d}}b{{y}}c")
	if got := real.Render(1, PartHTML, nil); got != "adbc" {
		t.Errorf("nil lookup rendered %q, want adbc", got)
	}
	if got := real.Missing(nil); len(got) != 1 || got[0] != "y" {
		t.Errorf("nil lookup Missing = %v, want [y]", got)
	}
}
