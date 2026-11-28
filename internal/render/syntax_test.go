package render

import (
	"errors"
	"strings"
	"testing"
)

type syntaxCase struct {
	name string
	src  string
	off  int
	line int
	col  int
	msg  string
}

const (
	msgStray      = `unexpected }: write \} for a literal brace`
	msgUnclosedG  = `unclosed {: close the group with } or write \{`
	msgUnclosedF  = `unclosed {{: close the field with }} or write \{\{`
	msgOneOption  = `a group needs at least two options, {a|b}; write \{ and \} for literal braces`
	msgEmptyName  = "empty field name"
	msgBadStart   = "invalid field name: it must start with a letter or an underscore"
	msgFieldFirst = "a group cannot begin with a field: put text before the field or reorder the options"
	msgSingleClos = "a field is closed with }}, not a single }"
	msgFbBrace    = `a fallback is plain text, with no field or group: write \{ for a brace`
	msgFbLone     = `a single } in a fallback must be written \}`
	msgTooDeep    = "groups are nested more than 8 deep"
	msgTooMany    = "a group has at most 64 options"
	msgNameLong   = "field name is longer than 64 characters"
)

func nest(n int) string { // n groups, one inside the last option of the other: {x|{x|...{x|y}}}
	return strings.Repeat("{x|", n-1) + "{x|y}" + strings.Repeat("}", n-1)
}

func options(n int) string { return "{" + strings.TrimSuffix(strings.Repeat("a|", n), "|") + "}" }

var syntaxCases = []syntaxCase{
	{"unclosed group", "{a|b", 0, 1, 1, msgUnclosedG},
	{"unclosed group after text", "hello {a|b", 6, 1, 7, msgUnclosedG},
	{"unclosed group, the outer one, after a closed inner one", "{a|{b|c}", 0, 1, 1, msgUnclosedG},
	{"unclosed group at the end of the source", "{", 0, 1, 1, msgUnclosedG},
	{"stray } at the end", "x }", 2, 1, 3, msgStray},
	{"stray } alone", "}", 0, 1, 1, msgStray},
	{"stray } after a closed group", "{a|b}}", 5, 1, 6, msgStray},
	{"stray } after a field", "{{a}}}", 5, 1, 6, msgStray},
	{"group of one option", "{a}", 0, 1, 1, msgOneOption},
	{"group of one option after text", "ab {a}", 3, 1, 4, msgOneOption},
	{"empty group", "{}", 0, 1, 1, msgOneOption},
	{"group of one option nested in a group", "{a|{b}}", 3, 1, 4, msgOneOption},
	{"outer group of one option holding a closed group", "{a{b|c}}", 0, 1, 1, msgOneOption},
	{"an escaped brace leaves the next one a group of one", `\{{a}}`, 2, 1, 3, msgOneOption},
	{"field name starting with a digit", "{{1a}}", 2, 1, 3, msgBadStart},
	{"field name with a leading space", "{{ a}}", 2, 1, 3, msgBadStart},
	{"field name starting with a hyphen", "{{-a}}", 2, 1, 3, msgBadStart},
	{"field name starting with a non-ASCII letter", "{{é}}", 2, 1, 3, msgBadStart},
	{"space inside a field name", "{{a b}}", 3, 1, 4, `invalid character ' ' in a field name`},
	{"hyphen inside a field name", "{{a-b}}", 3, 1, 4, `invalid character '-' in a field name`},
	{"space before the closing braces", "{{a }}", 3, 1, 4, `invalid character ' ' in a field name`},
	{"non-ASCII letter inside a field name", "{{aé}}", 3, 1, 4, `invalid character 'é' in a field name`},
	{"empty field name", "{{}}", 2, 1, 3, msgEmptyName},
	{"empty field name with a fallback", "{{|x}}", 2, 1, 3, msgEmptyName},
	{"unclosed field", "{{a", 0, 1, 1, msgUnclosedF},
	{"unclosed field with a fallback", "{{a|b", 0, 1, 1, msgUnclosedF},
	{"unclosed field, only the opening braces", "{{", 0, 1, 1, msgUnclosedF},
	{"unclosed field because the closing brace is escaped", `{{a|b\}}`, 0, 1, 1, msgUnclosedF},
	{"a field whose source ends after one closing brace", "{{a}", 0, 1, 1, msgUnclosedF},
	{"a fallback whose source ends after one closing brace", "{{a|b}", 0, 1, 1, msgUnclosedF},
	{"a field closed with a single brace and more text", "{{a}b}}", 3, 1, 4, msgSingleClos},
	{"a group cannot begin with a field", "{{{x}}|b}", 2, 1, 3, msgFieldFirst},
	{"a brace in a fallback", "{{x|a{b}}", 5, 1, 6, msgFbBrace},
	{"a lone closing brace in a fallback", "{{x|a}b}}", 5, 1, 6, msgFbLone},
	{"depth 9", nest(9), 24, 1, 25, msgTooDeep},
	{"depth 10 reports the ninth", nest(10), 24, 1, 25, msgTooDeep},
	{"65 options", options(65), 128, 1, 129, msgTooMany},
	{"a field name of 65 characters", "{{" + strings.Repeat("a", 65) + "}}", 66, 1, 67, msgNameLong},
	{"second line", "line1\nline2 {a}", 12, 2, 7, msgOneOption},
	{"third line", "a\n\n}", 3, 3, 1, msgStray},
	{"CRLF line ends", "a\r\n{x}", 3, 2, 1, msgOneOption},
	{"column counts runes: two-byte letters", "héllo wörld {a}", 14, 1, 13, msgOneOption},
	{"column counts runes: three-byte letters", "日本語 {a}", 10, 1, 5, msgOneOption},
	{"column counts runes: a four-byte emoji", "👋{a}", 4, 1, 2, msgOneOption},
	{"column counts runes after a newline", "é\nü{a}", 5, 2, 2, msgOneOption},
	{"invalid UTF-8 counts one rune per byte", "\xff\xfe{a}", 2, 1, 3, msgOneOption},
	{"unclosed field on the second line", "a\n{{b", 2, 2, 1, msgUnclosedF},
}

type compileFn func(string) error

func realCompile(src string) error { _, err := Compile(src); return err }

// checkSyntaxCases returns the names of the cases whose error is not exactly the expected one.
func checkSyntaxCases(cases []syntaxCase, fn compileFn, report func(string, ...any)) []string {
	var failed []string
	for _, c := range cases {
		err := fn(c.src)
		var se *SyntaxError
		if !errors.As(err, &se) {
			report("%s: Compile(%.60q) returned %v, want a *SyntaxError", c.name, c.src, err)
			failed = append(failed, c.name)
			continue
		}
		if se.Offset != c.off || se.Line != c.line || se.Column != c.col || se.Message != c.msg {
			report("%s: Compile(%.60q) = offset %d line %d column %d %q; want offset %d line %d column %d %q",
				c.name, c.src, se.Offset, se.Line, se.Column, se.Message, c.off, c.line, c.col, c.msg)
			failed = append(failed, c.name)
		}
	}
	return failed
}

func TestSyntaxErrorTable(t *testing.T) {
	if len(syntaxCases) < 30 {
		t.Fatalf("the syntax table has %d cases; the floor is 30", len(syntaxCases))
	}
	if failed := checkSyntaxCases(syntaxCases, realCompile, t.Errorf); len(failed) != 0 {
		t.Errorf("%d of %d cases failed", len(failed), len(syntaxCases))
	}
}

// TestSyntaxErrorTableControl proves the table can fail on a wrong position, a wrong line, a wrong message and a missing error.
func TestSyntaxErrorTableControl(t *testing.T) {
	quiet := func(string, ...any) {}
	shift := func(f func(*SyntaxError)) compileFn {
		return func(src string) error {
			err := realCompile(src)
			var se *SyntaxError
			if errors.As(err, &se) {
				cp := *se
				f(&cp)
				return &cp
			}
			return err
		}
	}
	mutants := []struct {
		name string
		fn   compileFn
	}{
		{"offset off by one", shift(func(e *SyntaxError) { e.Offset++ })},
		{"column off by one", shift(func(e *SyntaxError) { e.Column++ })},
		{"line off by one", shift(func(e *SyntaxError) { e.Line++ })},
		{"message changed", shift(func(e *SyntaxError) { e.Message += "." })},
		{"accepts everything", func(string) error { return nil }},
		{"a plain error", func(string) error { return errors.New("x") }},
	}
	for _, m := range mutants {
		failed := checkSyntaxCases(syntaxCases, m.fn, quiet)
		if len(failed) != len(syntaxCases) {
			t.Errorf("%s: only %d of %d cases noticed", m.name, len(failed), len(syntaxCases))
		}
	}
	// a column counted in bytes instead of runes must be caught by the multi-byte cases and by nothing else among the one-line cases
	bytesColumn := func(src string) error {
		err := realCompile(src)
		var se *SyntaxError
		if errors.As(err, &se) {
			cp := *se
			head := src[:cp.Offset]
			cp.Column = 1 + len(head) - (strings.LastIndexByte(head, '\n') + 1)
			return &cp
		}
		return err
	}
	failed := checkSyntaxCases(syntaxCases, bytesColumn, quiet)
	got := map[string]bool{}
	for _, f := range failed {
		got[f] = true
	}
	for _, name := range []string{
		"column counts runes: two-byte letters",
		"column counts runes: three-byte letters",
		"column counts runes: a four-byte emoji",
		"column counts runes after a newline",
	} {
		if !got[name] {
			t.Errorf("a column counted in bytes was not caught by %q", name)
		}
	}
	if got["unclosed group"] || got["stray } at the end"] {
		t.Error("the control fails ASCII-only cases too: it is not isolating the rune count")
	}
}

// TestValidNeighbours pairs the error table: the limits themselves compile, and so do the near misses of each error.
func TestValidNeighbours(t *testing.T) {
	valid := []string{
		nest(8),
		options(64),
		options(2),
		"{{" + strings.Repeat("a", 64) + "}}",
		"{a|}", "{|a}", "{|}", "{a||b}",
		"{{a}}", "{{a|}}", "{{a|b}}", "{ {{x}}|b}", "{b|{{x}}}",
		`\{`, `\}`, `\|`, `\\`, `\{\{x\}\}`, `{{x|\{\}}}`,
		"a|b", "a | b", "100% | done", "",
		"{a|{b|{c|d}}}", "{a|b}{c|d}", "x{{y}}z{p|q}",
		"\\", "a\\", `\x`,
	}
	for _, src := range valid {
		if _, err := Compile(src); err != nil {
			t.Errorf("Compile(%.60q) = %v, want no error", src, err)
		}
	}
}

func TestSyntaxErrorType(t *testing.T) {
	_, err := Compile("a\n{x}")
	var se *SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("Compile returned %T, want *SyntaxError", err)
	}
	if got, want := se.Error(), "template syntax error at line 2, column 1: "+msgOneOption; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// the nil-pointer-in-an-interface trap: a valid compile must return a nil interface, and a failed one no template
	tpl, err := Compile("{a|b}")
	if err != nil || tpl == nil {
		t.Errorf("Compile of a valid template = %v, %v", tpl, err)
	}
	tpl, err = Compile("{a}")
	if err == nil || tpl != nil {
		t.Errorf("Compile of an invalid template = %v, %v", tpl, err)
	}
}

// longestEcho returns the length of the longest run of bytes of src that also appears in msg.
func longestEcho(src, msg string) int {
	best := 0
	for i := 0; i < len(src); i++ {
		n := best + 1
		for i+n <= len(src) && strings.Contains(msg, src[i:i+n]) {
			best = n
			n++
		}
	}
	return best
}

func TestSyntaxErrorMessageNeverEchoesTheSource(t *testing.T) {
	long := strings.Repeat("Zq9", 5000)
	sources := []string{
		"{{" + long + "}}",
		"{{" + long + " }}",
		"{{a|" + long + "{",
		"{{a|" + long,
		long + "}",
		"{" + long,
		"{" + long + "}",
		"{{" + long + "|x",
		"{{x|" + long + "}b}}",
		"{{é" + long + "}}",
	}
	for _, src := range sources {
		_, err := Compile(src)
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Fatalf("Compile(%.40q...) = %v, want a syntax error", src, err)
		}
		if n := longestEcho(src, se.Message); n > 40 {
			t.Errorf("the message of Compile(%.40q...) repeats %d bytes of the source", src, n)
		}
		if len(se.Message) > 160 {
			t.Errorf("the message is %d bytes long", len(se.Message))
		}
	}
	// the control: the measure sees an echo when there is one
	if n := longestEcho(long, "error near "+long[:100]); n < 100 {
		t.Errorf("the echo measure found %d bytes of a 100-byte echo", n)
	}
	if n := longestEcho("abcdef", "xyz"); n != 0 {
		t.Errorf("the echo measure found %d bytes where there is none", n)
	}
}

// ValidFieldName is the rule `{{name}}` is parsed by, for a caller that has to say which keys of a row's merge data a template could ever name. Every name it accepts must compile as a field, and every name
// it refuses must not: the two halves are one grammar.
func TestValidFieldName_IsExactlyTheNameTheGrammarParses(t *testing.T) {
	good := []string{"a", "A", "_", "_x", "first_name", "City2", "x9", strings.Repeat("a", MaxNameLen)}
	bad := []string{"", "1a", "9", "first name", "first-name", "first.name", "e-mail", "café", "ünder", "a|b", "a}", "{a", "a\n", " a", "a ", strings.Repeat("a", MaxNameLen+1)}
	for _, n := range good {
		if !ValidFieldName(n) {
			t.Errorf("ValidFieldName(%q) = false", n)
		}
		if _, err := Compile("{{" + n + "}}"); err != nil {
			t.Errorf("control: %q is accepted by ValidFieldName and does not compile as a field: %v", n, err)
		}
	}
	for _, n := range bad {
		if ValidFieldName(n) {
			t.Errorf("ValidFieldName(%q) = true", n)
		}
		if tpl, err := Compile("{{" + n + "}}"); err == nil && n != "" && len(tpl.Fields()) == 1 && tpl.Fields()[0].Name == n {
			t.Errorf("control: %q is refused by ValidFieldName and the grammar parses it as a field", n)
		}
	}
}
