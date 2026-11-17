package contractcheck

import (
	"errors"
	"strings"
	"testing"
)

func TestParseKeepsKeyOrderAndLiteralsOfNumbersAndBools(t *testing.T) {
	v, err := parse([]byte(`{"b":1.50,"a":[true,null,"x"],"c":false}`))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, m := range v.members {
		keys = append(keys, m.key)
	}
	if strings.Join(keys, ",") != "b,a,c" {
		t.Fatalf("key order = %v, want b,a,c", keys)
	}
	if v.members[0].val.str != "1.50" {
		t.Fatalf("number literal = %q, want 1.50 (the text, not a rounded float)", v.members[0].val.str)
	}
	arr := v.members[1].val
	if len(arr.items) != 3 || arr.items[0].kind != vBool || arr.items[0].str != "true" || arr.items[1].kind != vNull || arr.items[2].str != "x" {
		t.Fatalf("array parsed wrongly: %+v", arr)
	}
	if v.members[2].val.str != "false" {
		t.Fatalf("a false lost its value")
	}
}

func TestParseRefusalsHaveTheirOwnKindAndTheCorrectedBodyParses(t *testing.T) {
	huge := `"` + strings.Repeat("a", MaxBody) + `"`
	cases := []struct {
		name string
		bad  string
		want ParseKind
		fix  string
	}{
		{"empty", ``, ParseEmpty, `{}`},
		{"white space only", " \n\t ", ParseEmpty, `{}`},
		{"truncated object", `{"a":`, ParseSyntax, `{"a":1}`},
		{"trailing comma", `[1,]`, ParseSyntax, `[1]`},
		{"bare word", `nul`, ParseSyntax, `null`},
		{"missing comma", `{"a":1 "b":2}`, ParseSyntax, `{"a":1,"b":2}`},
		{"unterminated string", `{"a":"x`, ParseSyntax, `{"a":"x"}`},
		{"second document", `{} {}`, ParseTrailingData, `{}`},
		{"stray closer", `{"a":1}}`, ParseTrailingData, `{"a":1}`},
		{"text after the document", `[1] x`, ParseTrailingData, `[1]`},
		{"duplicate key", `{"a":1,"a":2}`, ParseDuplicateKey, `{"a":1,"b":2}`},
		{"invalid utf-8", "{\"a\":\"\xff\"}", ParseInvalidUTF8, `{"a":"x"}`},
		{"over the size limit", huge, ParseTooLarge, `"a"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse([]byte(c.bad))
			if got := ParseKindOf(err); got != c.want {
				t.Fatalf("kind = %v (%v), want %v", got, err, c.want)
			}
			if _, err := parse([]byte(c.fix)); err != nil {
				t.Fatalf("the corrected body %q must parse (the paired control): %v", c.fix, err)
			}
		})
	}
}

func TestParseDuplicateKeyIsPerObjectNotGlobal(t *testing.T) {
	if _, err := parse([]byte(`{"a":{"x":1},"b":{"x":2},"c":[{"x":1},{"x":2}]}`)); err != nil {
		t.Fatalf("the same key in different objects is not a duplicate: %v", err)
	}
	_, err := parse([]byte(`{"a":{"x":1,"x":2}}`))
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Kind != ParseDuplicateKey || pe.Path != "a.x" {
		t.Fatalf("a duplicate inside a nested object must name its path: %v", err)
	}
}

func TestParseDepthLimitIsExactlyMaxDepth(t *testing.T) {
	nest := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	if _, err := parse([]byte(nest(MaxDepth))); err != nil {
		t.Fatalf("nesting of exactly %d must be accepted: %v", MaxDepth, err)
	}
	_, err := parse([]byte(nest(MaxDepth + 1)))
	if ParseKindOf(err) != ParseTooDeep {
		t.Fatalf("nesting of %d must be refused as too deep, got %v", MaxDepth+1, err)
	}
}

func TestParseDoesNotBlowTheStackOnHostileNesting(t *testing.T) {
	for _, n := range []int{10000, 1000000} {
		closed := strings.Repeat("[", n) + strings.Repeat("]", n)
		open := strings.Repeat("{\"a\":", n)
		for name, body := range map[string]string{"closed arrays": closed, "unclosed objects": open} {
			_, err := parse([]byte(body))
			if ParseKindOf(err) != ParseTooDeep {
				t.Fatalf("%d levels of %s: want a too-deep refusal, got %v", n, name, err)
			}
		}
	}
}

func TestParseKeepsAHugeNumberAsItsLiteral(t *testing.T) {
	lit := strings.Repeat("9", 5000)
	v, err := parse([]byte(`{"n":1e999999999,"m":` + lit + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.members[0].val.str != "1e999999999" || v.members[1].val.str != lit {
		t.Fatal("a huge number must stay its literal")
	}
}

func TestParseTopLevelScalarsAndArraysAreDocuments(t *testing.T) {
	for _, body := range []string{`null`, `true`, `42`, `"x"`, `[]`, `[1,"a",{}]`, `{}`} {
		if _, err := parse([]byte(body)); err != nil {
			t.Fatalf("%q is a JSON document: %v", body, err)
		}
	}
}

func TestSafeKeyNeverEmitsPathSyntaxOrMoreThanFortyBytes(t *testing.T) {
	cases := map[string]string{
		"state":                  "state",
		"a.b":                    "a?b",
		"items[]":                "items??",
		"x{}":                    "x??",
		"user@host.example":      "user?host?example",
		"":                       "?",
		strings.Repeat("k", 100): strings.Repeat("k", 40),
	}
	for in, want := range cases {
		if got := safeKey(in); got != want {
			t.Fatalf("safeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseErrorsNeverCarryBodyText(t *testing.T) {
	marker := "marker" + strings.Repeat("Q", 12)
	for _, body := range []string{`{"a":"` + marker + `" x}`, `{"a":"` + marker + "\xff\"}", `{"a":"` + marker + `"}}`, `["` + marker + `",`} {
		_, err := parse([]byte(body))
		if err == nil {
			t.Fatalf("%q must be refused", body)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("the error carries body text: %v", err)
		}
	}
}
