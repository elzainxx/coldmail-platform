package render

import (
	"errors"
	goparser "go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkRenderProperties is what must hold of any source, any seed, any value and any part. It returns the first violated property, or "".
// FuzzCompileRender and the random sweep below both use it, and TestPropertiesCanFail proves it fails on broken renders.
func checkRenderProperties(src string, seed uint64, value string, part int) string {
	tpl, err := Compile(src)
	if err != nil {
		var se *SyntaxError
		if !errors.As(err, &se) {
			return "Compile returned an error that is not a *SyntaxError"
		}
		if se.Offset < 0 || se.Offset > len(src) || se.Line < 1 || se.Column < 1 {
			return "a syntax error with an impossible position"
		}
		if len(se.Message) > 160 || longestEcho(src, se.Message) > 40 {
			return "a syntax error message that repeats the source"
		}
		if strings.Count(src[:se.Offset], "\n")+1 != se.Line {
			return "a syntax error whose line does not match its offset"
		}
		return ""
	}
	p := Part(((part % 4) + 4) % 4) // 0..2 are the parts, 3 is an unknown one
	look := func(name string) (string, bool) {
		switch {
		case name == "email":
			return value, true
		case len(name)%3 == 0:
			return value, true
		case len(name)%3 == 1:
			return " ", true
		}
		return "", false
	}
	out := tpl.Render(seed, p, look)
	if again := tpl.Render(seed, p, look); again != out {
		return "Render is not deterministic"
	}
	if p == PartSubject {
		for i := 0; i < len(out); i++ {
			if out[i] < 0x20 {
				return "a C0 control character in a rendered subject"
			}
		}
		if out != strings.TrimSpace(out) {
			return "an untrimmed subject"
		}
	}
	for _, name := range tpl.Missing(look) {
		if IsReserved(name) {
			return "a reserved name is missing"
		}
	}
	known := map[string]bool{}
	for _, f := range tpl.Fields() {
		known[f.Name] = true
	}
	for _, name := range tpl.Missing(nil) {
		if !known[name] {
			return "Missing names a field the template does not use"
		}
	}
	// The property of the brief: a merge value never puts a `<` into the HTML. Take the template with every `<` of its text removed (so that
	// every `<` of the output could only come from a value) and a value that is full of them.
	stripped, err := Compile(strings.ReplaceAll(src, "<", ""))
	if err == nil {
		hostile := "<" + value + "<script>"
		all := func(string) (string, bool) { return hostile, true }
		for _, hp := range []Part{PartHTML, Part(99)} {
			if strings.Contains(stripped.Render(seed, hp, all), "<") {
				return "a < from a merge value reached the HTML output"
			}
		}
	}
	return ""
}

var fuzzSeeds = []struct {
	src   string
	seed  uint64
	value string
	part  int
}{
	{"", 0, "", 0},
	{"Hello {{first_name|there}}, {we|I} wrote {a|b|c}.", 42, "<b>Ann</b>", 0},
	{"{a|b}{{x}}", 1, "{{x}}", 1},
	{"{x|{{y|z}}}", 7, "&\"'", 2},
	{`\{\{x\}\} \| \\ \n`, 3, "{a|b}", 0},
	{"{{a|b\\}\\}}}", 3, "v", 0},
	{"{a|{b|{c|{d|{e|{f|{g|{h|i}}}}}}}}}", 9, "v", 0},
	{"{a|{b|{c|{d|{e|{f|{g|{h|{i|j}}}}}}}}}}", 9, "v", 0},
	{"{{", 0, "", 0},
	{"{", 0, "", 1},
	{"}", 0, "", 2},
	{"{a}", 0, "x", 0},
	{"{{1a}}", 0, "x", 0},
	{"{{a b}}", 0, "x", 0},
	{"{{{x}}|b}", 0, "x", 0},
	{"<p style=\"{color:red}\">{{x}}</p>", 5, "\" onmouseover=\"x", 0},
	{"Subject {{x}}", 5, "a\r\nBcc: e@example.com\x00", 2},
	{"é{日|本}{{n}}", 11, "\xff\xfe", 0},
	{"{{x|a|b}}{{y|}}{{z}}", 0, "v", 3},
	{"a|b}c", 0, "", 0},
}

func FuzzCompileRender(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s.src, s.seed, s.value, s.part)
	}
	f.Fuzz(func(t *testing.T, src string, seed uint64, value string, part int) {
		if msg := checkRenderProperties(src, seed, value, part); msg != "" {
			t.Fatalf("%s (src %q, seed %d, value %q, part %d)", msg, src, seed, value, part)
		}
	})
}

// TestFuzzSeedsHold runs every seed of the fuzz test as an ordinary test and requires both outcomes (a template that compiles and one that
// does not) among them, so that the fuzz test is not a loop over nothing.
func TestFuzzSeedsHold(t *testing.T) {
	ok, bad := 0, 0
	for _, s := range fuzzSeeds {
		if msg := checkRenderProperties(s.src, s.seed, s.value, s.part); msg != "" {
			t.Errorf("%s (src %q)", msg, s.src)
		}
		if _, err := Compile(s.src); err != nil {
			bad++
		} else {
			ok++
		}
	}
	if ok < 8 || bad < 8 {
		t.Errorf("the seeds hold %d compiling and %d failing templates", ok, bad)
	}
}

// TestFuzzCorpusIsCheckedIn fails when the checked-in corpus is missing or has been thinned: `go test` replays it as part of
// FuzzCompileRender, and a directory that is not there would replay nothing and still pass.
func TestFuzzCorpusIsCheckedIn(t *testing.T) {
	dir := filepath.Join("testdata", "fuzz", "FuzzCompileRender")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the fuzz corpus is not there: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "go test fuzz v1\n") {
			t.Errorf("%s is not a fuzz corpus file", e.Name())
		}
		n++
	}
	if n < 8 {
		t.Errorf("the corpus has %d files; the floor is 8", n)
	}
	// the control: the reader of the directory does see a missing one
	if _, err := os.ReadDir(filepath.Join("testdata", "fuzz", "NoSuchFuzzTest")); err == nil {
		t.Error("ReadDir accepted a directory that does not exist")
	}
}

// TestPropertiesCanFail is the control of the fuzz properties: renderers that break them are seen. The properties are the function above, so
// each broken variant is exercised through the same checks, written out here for the three it guards.
func TestPropertiesCanFail(t *testing.T) {
	// 1. an escaping that forgets "<" lets a value's "<" into the HTML
	brokenEscape := strings.NewReplacer("&", "&amp;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	hostile := "<script>"
	if out := brokenEscape.Replace(hostile); !strings.Contains(out, "<") {
		t.Error("the broken escaping does not leak a <: the control is vacuous")
	}
	if out := htmlOracle.Replace(hostile); strings.Contains(out, "<") {
		t.Error("the oracle escaping leaks a <")
	}
	// 2. the real renderer passes the property on a template that would expose a leak
	if msg := checkRenderProperties("<p>{{x}}</p>{a|b}", 1, "<<<", 0); msg != "" {
		t.Errorf("the property fails on a plain case: %s", msg)
	}
	// 3. the subject property sees a control character and an untrimmed result
	for _, bad := range []string{"a\nb", " a", "a "} {
		clean := bad == strings.TrimSpace(bad) && !strings.ContainsAny(bad, "\r\n")
		if clean {
			t.Errorf("%q should have been judged dirty", bad)
		}
	}
	// 4. a syntax error with an echo is seen by the shape check
	if longestEcho(strings.Repeat("abcdefghij", 10), "bad: "+strings.Repeat("abcdefghij", 10)) <= 40 {
		t.Error("the echo measure does not see an echo of the whole source")
	}
}

// TestRenderNeverPanics sweeps random templates built from the tokens of the grammar, with random seeds, values and parts, through the same
// properties as the fuzz test. It also counts the two outcomes, so that a generator that only ever produced garbage (or only valid
// templates) would fail the test rather than pass it.
func TestRenderNeverPanics(t *testing.T) {
	tokens := []string{
		"{", "}", "|", `\`, "{{", "}}", `\{`, `\}`, `\|`, `\\`, "a", "b", "x", "name", "_", "9", " ", "\n", "é", "日", "<", ">", "&", `"`,
		"{a|b}", "{{x}}", "{{y|z}}", "{p|{q|r}}", "{{x|", "<b>", "</b>", "\r\n",
	}
	rng := rand.New(rand.NewSource(99))
	ok, bad := 0, 0
	for i := 0; i < 40000; i++ {
		var b strings.Builder
		for k, n := 0, rng.Intn(16); k < n; k++ {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		src := b.String()
		if _, err := Compile(src); err != nil {
			bad++
		} else {
			ok++
		}
		values := []string{"", "v", "<&>", "{{x}}", "a\r\nb", " "}
		if msg := checkRenderProperties(src, rng.Uint64(), values[rng.Intn(len(values))], rng.Intn(8)); msg != "" {
			t.Fatalf("%s (src %q)", msg, src)
		}
	}
	if ok < 4000 || bad < 4000 {
		t.Errorf("the generator produced %d compiling and %d failing templates of 40000", ok, bad)
	}
	// deep nesting, wide groups and long runs, which the tokens rarely reach
	for _, src := range []string{
		strings.Repeat("{a|", 200) + strings.Repeat("}", 200),
		strings.Repeat("{", 100000),
		strings.Repeat("{{", 50000),
		strings.Repeat(`\`, 100000),
		strings.Repeat("{a|b}", 20000),
		"{" + strings.Repeat("a|", 100000) + "b}",
		strings.Repeat("{{x|y}}", 20000),
	} {
		if msg := checkRenderProperties(src, 1, "v", 0); msg != "" {
			t.Errorf("%s (src of %d bytes)", msg, len(src))
		}
	}
}

// importViolations returns what a Go source file imports that this package may not: anything that is not the standard library (an import path
// whose first element has a dot), and math/rand in any version (the choice generator is this package's own).
func importViolations(filename, src string, forbidRand bool) []string {
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, filename, src, goparser.ImportsOnly)
	if err != nil {
		return []string{"does not parse: " + err.Error()}
	}
	var out []string
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		first := strings.SplitN(path, "/", 2)[0]
		switch {
		case strings.Contains(first, "."):
			out = append(out, "imports "+path)
		case forbidRand && (path == "math/rand" || path == "math/rand/v2"):
			out = append(out, "imports "+path)
		}
	}
	return out
}

func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	prod := 0
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		isTest := strings.HasSuffix(name, "_test.go")
		if !isTest {
			prod++
		}
		// test files may use math/rand for their own sampling, but no file may reach another package of the platform or a third party
		for _, v := range importViolations(name, string(b), !isTest) {
			t.Errorf("%s: %s", name, v)
		}
	}
	if prod < 4 {
		t.Errorf("found %d non-test files: the check looked at nothing", prod)
	}
	// the control: the checker flags each kind of violation
	internal := `package x
import (
	"fmt"
	"github.com/coldmail-platform/platform/internal/norm"
)
`
	if v := importViolations("x.go", internal, true); len(v) != 1 || !strings.Contains(v[0], "internal/norm") {
		t.Errorf("an internal import was not reported: %v", v)
	}
	for _, imp := range []string{"math/rand", "math/rand/v2"} {
		if v := importViolations("x.go", "package x\nimport \""+imp+"\"\n", true); len(v) != 1 {
			t.Errorf("%s was not reported: %v", imp, v)
		}
	}
	if v := importViolations("x.go", "package x\nimport (\"strings\"; \"crypto/sha256\")\n", true); len(v) != 0 {
		t.Errorf("standard library imports were reported: %v", v)
	}
	if v := importViolations("x.go", "package x\nimport \"example.com/third/party\"\n", false); len(v) != 1 {
		t.Errorf("a third-party import was not reported: %v", v)
	}
}
