package render

import (
	"reflect"
	"strings"
	"testing"
)

func TestFieldsTable(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []Field
	}{
		{"no fields", "plain {a|b} text", []Field{}},
		{"empty template", "", []Field{}},
		{"one field", "{{a}}", []Field{{"a", false}}},
		{"one field with a fallback", "{{a|x}}", []Field{{"a", true}}},
		{"an empty fallback is a fallback", "{{a|}}", []Field{{"a", true}}},
		{"unique, in order of first appearance", "{{b}} {{a}} {{b}} {{c|z}} {{a}}", []Field{{"b", false}, {"a", false}, {"c", true}}},
		{"every use has a fallback", "{{a|x}} {{a|y}}", []Field{{"a", true}}},
		{"one use without a fallback makes the field required", "{{a|x}} {{a}}", []Field{{"a", false}}},
		{"the bare use first", "{{a}} {{a|x}}", []Field{{"a", false}}},
		{"fields inside groups, in document order", "{p|{{b}}} {{a}} {q|{r|{{c|d}}}}", []Field{{"b", false}, {"a", false}, {"c", true}}},
		{"reserved names are included when used", "{{email}} {{unsubscribe_url}} {{x}}", []Field{{"email", false}, {"unsubscribe_url", false}, {"x", false}}},
		{"reserved names are absent when unused", "{{x}}", []Field{{"x", false}}},
		{"a fallback is text: a name inside it is not a field", `{{a|\{\{b\}\}}}`, []Field{{"a", true}}},
		{"names are case sensitive", "{{A}} {{a}}", []Field{{"A", false}, {"a", false}}},
	}
	for _, c := range cases {
		got := mustCompile(t, c.src).Fields()
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Fields(%q) = %v, want %v", c.name, c.src, got, c.want)
		}
	}
	// Fields returns a copy
	tpl := mustCompile(t, "{{a}} {{b}}")
	f := tpl.Fields()
	f[0].Name, f[1].HasFallback = "zzz", true
	if got := tpl.Fields(); got[0].Name != "a" || got[1].HasFallback {
		t.Errorf("Fields exposed the template's own slice: %v", got)
	}
	// the control: the comparison is able to fail
	if reflect.DeepEqual(mustCompile(t, "{{a}}").Fields(), []Field{{"a", true}}) {
		t.Error("DeepEqual cannot tell a required field from one with a fallback")
	}
}

type missingCase struct {
	name string
	src  string
	look func(string) (string, bool)
	want []string
}

func lookupFalseWithValue(string) (string, bool) {
	return "has a value but the lookup says false", false
}

var missingCases = []missingCase{
	{"all present", "{{a}} {{b}}", vals(map[string]string{"a": "1", "b": "2"}), []string{}},
	{"absent", "{{a}} {{b}} {{c}}", vals(map[string]string{"a": "ok"}), []string{"b", "c"}},
	{"empty", "{{a}}", vals(map[string]string{"a": ""}), []string{"a"}},
	{"whitespace only", "{{a}}", vals(map[string]string{"a": " \t\r\n "}), []string{"a"}},
	{"unicode whitespace only", "{{a}}", vals(map[string]string{"a": " 　 "}), []string{"a"}},
	{"padding around a value is not missing", "{{a}}", vals(map[string]string{"a": " x "}), []string{}},
	{"a fallback means never missing", "{{a|x}}", vals(nil), []string{}},
	{"a fallback and an empty value", "{{a|x}}", vals(map[string]string{"a": "  "}), []string{}},
	{"one use without a fallback is enough", "{{a|x}} {{a}}", vals(nil), []string{"a"}},
	{"one use without a fallback, present", "{{a|x}} {{a}}", vals(map[string]string{"a": "v"}), []string{}},
	{"reserved names are never missing", "{{email}} {{unsubscribe_url}}", vals(nil), []string{}},
	{"reserved names, with other fields", "{{email}} {{x}} {{unsubscribe_url}}", vals(nil), []string{"x"}},
	{"order of first appearance, without repeats", "{{z}}{{y}}{{z}}{{x}}", vals(nil), []string{"z", "y", "x"}},
	{"a field of a group option counts", "{p|{{b}}}", vals(nil), []string{"b"}},
	{"a field of a nested option counts", "{p|{q|{{b|f}}{{c}}}}", vals(nil), []string{"c"}},
	{"the lookup says false, whatever it returns", "{{a}}", lookupFalseWithValue, []string{"a"}},
	{"nil lookup", "{{a}} {{email}}", nil, []string{"a"}},
	{"no fields", "plain", vals(nil), []string{}},
}

type missingFn func(tpl *Template, look func(string) (string, bool)) []string

func realMissing(tpl *Template, look func(string) (string, bool)) []string { return tpl.Missing(look) }

func checkMissingCases(t testing.TB, cases []missingCase, fn missingFn, report func(string, ...any)) []string {
	var failed []string
	for _, c := range cases {
		tpl, err := Compile(c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := fn(tpl, c.look); !reflect.DeepEqual(got, c.want) {
			report("%s: Missing for %q = %#v, want %#v", c.name, c.src, got, c.want)
			failed = append(failed, c.name)
		}
	}
	return failed
}

func TestMissingTable(t *testing.T) {
	if failed := checkMissingCases(t, missingCases, realMissing, t.Errorf); len(failed) != 0 {
		t.Errorf("%d of %d cases failed", len(failed), len(missingCases))
	}
}

// TestMissingTableControl: a Missing that gets one part of the rule wrong is caught by the case written for that part.
func TestMissingTableControl(t *testing.T) {
	quiet := func(string, ...any) {}
	// each mutant is written from Fields(), not from the code under test, so it is an independent statement of a wrong rule
	build := func(skipReserved, skipFallback, trim, honourFlag bool) missingFn {
		return func(tpl *Template, look func(string) (string, bool)) []string {
			out := []string{}
			for _, f := range tpl.Fields() {
				if (skipReserved && IsReserved(f.Name)) || (skipFallback && f.HasFallback) {
					continue
				}
				v, ok := "", false
				if look != nil {
					v, ok = look(f.Name)
				}
				if trim {
					v = strings.TrimSpace(v)
				}
				if (honourFlag && !ok) || v == "" {
					out = append(out, f.Name)
				}
			}
			return out
		}
	}
	if failed := checkMissingCases(t, missingCases, build(true, true, true, true), quiet); len(failed) != 0 {
		t.Fatalf("the reference mutant base disagrees with the table on %v: the control is broken", failed)
	}
	for _, m := range []struct {
		name   string
		fn     missingFn
		caught string
	}{
		{"missing means absent only (no trim)", build(true, true, false, true), "whitespace only"},
		{"reserved names can be missing", build(false, true, true, true), "reserved names are never missing"},
		{"a fallback does not excuse a field", build(true, false, true, true), "a fallback means never missing"},
		{"the lookup's flag is ignored", build(true, true, true, false), "the lookup says false, whatever it returns"},
	} {
		failed := checkMissingCases(t, missingCases, m.fn, quiet)
		found := false
		for _, f := range failed {
			if f == m.caught {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: case %q should have failed (failed: %v)", m.name, m.caught, failed)
		}
	}
}

// TestMissingAgreesWithRender: for a row where Missing is empty, a Render never meets a missing field without a fallback; for a row where it
// is not, the names it lists are exactly the fields that rendered as nothing.
func TestMissingAgreesWithRender(t *testing.T) {
	tpl := mustCompile(t, "[{{a}}|{{b|fb}}|{{c}}]")
	row := vals(map[string]string{"a": "A", "c": "  "})
	if got := tpl.Missing(row); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("Missing = %v, want [c]", got)
	}
	if got := tpl.Render(0, PartText, row); got != "[A|fb|]" {
		t.Errorf("Render = %q, want [A|fb|]", got)
	}
	full := vals(map[string]string{"a": "A", "c": "C"})
	if got := tpl.Missing(full); len(got) != 0 {
		t.Errorf("Missing = %v, want none", got)
	}
	if got := tpl.Render(0, PartText, full); got != "[A|fb|C]" {
		t.Errorf("Render = %q, want [A|fb|C]", got)
	}
}
