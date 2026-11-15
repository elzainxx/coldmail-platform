package migrate

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Docs/migrator-reference.md must say what Vars() says, in both directions. The instrument below reads the table out
// of the markdown and diffs it against the code. A floor on the number of rows makes an instrument that matched
// nothing RED instead of vacuously green, and the controls feed it documents that are wrong in each way to prove it
// notices.

const referencePath = "../../Docs/migrator-reference.md"

const referenceHeader = "| Variable | Default | Required | Secret | Meaning |"

type docRow struct {
	Name, Default, Required, Meaning string
	Secret                           bool
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func unquote(cell string) string { return strings.Trim(cell, "`") }

// parseReference extracts the rows of the variable table: the lines after the header and its separator, up to the first
// line that is not a table row. A missing header, a malformed row, an unparseable flag or an empty table is an
// error, so a hand-edit that breaks the table cannot be mistaken for "no differences".
func parseReference(md string) ([]docRow, error) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == referenceHeader {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("the table header %q was not found", referenceHeader)
	}
	if start+1 >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[start+1]), "|---") {
		return nil, fmt.Errorf("the table header is not followed by a separator row")
	}
	var rows []docRow
	for _, l := range lines[start+2:] {
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			break
		}
		cells := splitRow(l)
		if len(cells) != 5 {
			return nil, fmt.Errorf("row %q has %d cells, want 5", l, len(cells))
		}
		var secret bool
		switch cells[3] {
		case "yes":
			secret = true
		case "no":
		default:
			return nil, fmt.Errorf("row %q: Secret is %q, want yes or no", l, cells[3])
		}
		def := unquote(cells[1])
		if def == "(none)" {
			def = ""
		}
		rows = append(rows, docRow{Name: unquote(cells[0]), Default: def, Required: cells[2], Secret: secret, Meaning: cells[4]})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("found 0 rows under the table header: the instrument matched nothing")
	}
	return rows, nil
}

// diffReference compares the document's rows with the code's variables, both ways, and says how each differs.
func diffReference(rows []docRow, vars []Var) []string {
	var diffs []string
	byName := map[string]docRow{}
	for _, r := range rows {
		if _, dup := byName[r.Name]; dup {
			diffs = append(diffs, fmt.Sprintf("%s is listed twice in the document", r.Name))
		}
		byName[r.Name] = r
	}
	inCode := map[string]bool{}
	for _, v := range vars {
		inCode[v.Name] = true
		r, ok := byName[v.Name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s is read by the code but missing from the document", v.Name))
			continue
		}
		if r.Default != v.Default {
			diffs = append(diffs, fmt.Sprintf("%s: default is %q in the document and %q in the code", v.Name, r.Default, v.Default))
		}
		if r.Required != v.Required {
			diffs = append(diffs, fmt.Sprintf("%s: required is %q in the document and %q in the code", v.Name, r.Required, v.Required))
		}
		if r.Secret != v.Secret {
			diffs = append(diffs, fmt.Sprintf("%s: secret is %t in the document and %t in the code", v.Name, r.Secret, v.Secret))
		}
		if r.Meaning != v.Meaning {
			diffs = append(diffs, fmt.Sprintf("%s: the meaning differs between the document and the code", v.Name))
		}
	}
	for _, r := range rows {
		if !inCode[r.Name] {
			diffs = append(diffs, fmt.Sprintf("%s is documented but the code does not read it", r.Name))
		}
	}
	return diffs
}

// renderReference writes a document from the code's variables, the way the real one is written. The controls mutate it.
func renderReference(vars []Var) string {
	var b strings.Builder
	b.WriteString("# reference\n\n" + referenceHeader + "\n|---|---|---|---|---|\n")
	for _, v := range vars {
		def := "`" + v.Default + "`"
		if v.Default == "" {
			def = "(none)"
		}
		secret := "no"
		if v.Secret {
			secret = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", v.Name, def, v.Required, secret, v.Meaning)
	}
	b.WriteString("\nafter the table\n")
	return b.String()
}

func TestMigratorReference_MatchesVars(t *testing.T) {
	raw, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseReference(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", referencePath, err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: parsed 0 rows", referencePath)
	}
	if want := len(Vars()); len(rows) != want {
		t.Errorf("the document lists %d variables, the code reads %d", len(rows), want)
	}
	for _, d := range diffReference(rows, Vars()) {
		t.Error(d)
	}
}

// The paired controls: each document below is wrong in exactly one way, and the instrument must say so for that
// reason. Without these a diff that always came back empty would look the same as a document that matches.
func TestMigratorReference_TheInstrumentNoticesEachKindOfDrift(t *testing.T) {
	vars := Vars()
	good := renderReference(vars)

	rows, err := parseReference(good)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffReference(rows, vars); len(d) != 0 {
		t.Fatalf("control: a document rendered from the code differs from the code: %v", d)
	}

	first := vars[0].Name
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"a variable missing from the document",
			func(md string) string { return dropLine(md, "| `"+first+"` |") }, first + " is read by the code but missing from the document"},
		{"a variable the code does not read",
			func(md string) string {
				return strings.Replace(md, "\n\nafter the table", "\n| `PLATFORM_MIGRATE_GHOST` | `1` | no | no | nothing reads this |\n\nafter the table", 1)
			}, "PLATFORM_MIGRATE_GHOST is documented but the code does not read it"},
		{"a wrong default",
			func(md string) string { return strings.Replace(md, "| `10` |", "| `11` |", 1) }, EnvLockTimeoutSec + ": default is \"11\""},
		{"a wrong required flag",
			func(md string) string { return strings.Replace(md, "| (none) | yes |", "| (none) | no |", 1) }, first + ": required is \"no\""},
		{"a wrong secret flag",
			func(md string) string { return strings.Replace(md, "| yes | yes |", "| yes | no |", 1) }, first + ": secret is false"},
		{"a changed meaning",
			func(md string) string { return strings.Replace(md, "Integer, 1 to 3600.", "Integer, 1 to 9.", 1) }, EnvLockTimeoutSec + ": the meaning differs"},
		{"a variable listed twice",
			func(md string) string {
				return strings.Replace(md, "\n\nafter the table", "\n"+findLine(md, "| `"+first+"` |")+"\n\nafter the table", 1)
			}, first + " is listed twice"},
	}
	for _, c := range cases {
		mutated := c.mutate(good)
		if mutated == good {
			t.Errorf("%s: the mutation changed nothing, so this control proves nothing", c.name)
			continue
		}
		rows, err := parseReference(mutated)
		if err != nil {
			t.Errorf("%s: the mutated document no longer parses: %v", c.name, err)
			continue
		}
		diffs := strings.Join(diffReference(rows, vars), "\n")
		if !strings.Contains(diffs, c.want) {
			t.Errorf("%s: the diff did not say %q; it said:\n%s", c.name, c.want, diffs)
		}
	}

	// Documents the instrument must refuse to read, rather than read as "no differences".
	unreadable := map[string]string{
		"no table":           "# reference\n\nno table here\n",
		"an empty table":     "# reference\n\n" + referenceHeader + "\n|---|---|---|---|---|\n\nafter\n",
		"no separator":       "# reference\n\n" + referenceHeader + "\n| `X` | `1` | no | no | m |\n",
		"a short row":        "# reference\n\n" + referenceHeader + "\n|---|---|---|---|---|\n| `X` | `1` | no |\n",
		"a bad flag":         "# reference\n\n" + referenceHeader + "\n|---|---|---|---|---|\n| `X` | `1` | no | maybe | m |\n",
		"a different header": "# reference\n\n| Name | Default | Required | Secret | Meaning |\n|---|---|---|---|---|\n| `X` | `1` | no | no | m |\n",
	}
	for name, md := range unreadable {
		if rows, err := parseReference(md); err == nil {
			t.Errorf("%s: parsed %d rows, want an error", name, len(rows))
		}
	}
}

// The default the document and Vars() promise is the default LoadConfig actually applies.
func TestVars_DefaultsAreTheDefaultsLoadConfigApplies(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN}), CmdUp)
	if err != nil {
		t.Fatal(err)
	}
	applied := map[string]string{
		EnvMigrationsDir:       cfg.MigrationsDir,
		EnvLockTimeoutSec:      fmt.Sprint(cfg.LockTimeoutSec),
		EnvStatementTimeoutSec: fmt.Sprint(cfg.StatementTimeoutSec),
		EnvAllowDown:           fmt.Sprint(cfg.AllowDown),
		EnvDatabaseURL:         "",
		EnvAppPassword:         "",
	}
	checked := 0
	for _, v := range Vars() {
		got, ok := applied[v.Name]
		if !ok {
			t.Errorf("%s: no applied default to compare with; extend this test with the new variable", v.Name)
			continue
		}
		// the DSN is set by this test, so only the variables with a default are compared
		if v.Name == EnvDatabaseURL {
			continue
		}
		if got != v.Default {
			t.Errorf("%s: Vars() promises default %q, LoadConfig applies %q", v.Name, v.Default, got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("compared 0 defaults")
	}
}

func dropLine(md, prefix string) string {
	var keep []string
	for _, l := range strings.Split(md, "\n") {
		if !strings.HasPrefix(l, prefix) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

func findLine(md, prefix string) string {
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}
