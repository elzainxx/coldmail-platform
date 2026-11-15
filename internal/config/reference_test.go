package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Docs/config-reference.md must say what Vars() says, in both directions. The instrument below reads the
// table out of the markdown and diffs it against the code; a nonzero row floor makes an instrument that
// matched nothing RED instead of vacuously green, and the controls feed it documents that are wrong in
// each way to prove it notices.

const referencePath = "../../Docs/config-reference.md"

const referenceHeader = "| Variable | Default | Required | Secret | Meaning |"

// separatorRow is the line goodDoc writes under the header, with its newline.
const separatorRow = "|---|---|---|---|---|\n"

type docRow struct {
	Name     string
	Default  string
	Required bool
	Secret   bool
	Meaning  string
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

func yesNo(cell string) (bool, error) {
	switch cell {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	return false, fmt.Errorf("%q is not yes or no", cell)
}

// parseReference extracts the rows of the variable table: the lines after the header and its separator,
// up to the first line that is not a table row. It errors on a missing header, a malformed row or an
// unparseable flag, so a hand-edit that breaks the table cannot be mistaken for "no differences".
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
		return nil, errors.New("the table header is not followed by a separator row")
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
		req, err := yesNo(cells[2])
		if err != nil {
			return nil, fmt.Errorf("row %q: Required: %w", l, err)
		}
		sec, err := yesNo(cells[3])
		if err != nil {
			return nil, fmt.Errorf("row %q: Secret: %w", l, err)
		}
		def := strings.Trim(cells[1], "`")
		if def == "(none)" {
			def = ""
		}
		rows = append(rows, docRow{
			Name:     strings.Trim(cells[0], "`"),
			Default:  def,
			Required: req,
			Secret:   sec,
			Meaning:  cells[4],
		})
	}
	return rows, nil
}

// diffReference returns one line per disagreement between the documented rows and vars, in both
// directions, and a line when there are no rows at all.
func diffReference(rows []docRow, vars []Var) []string {
	var out []string
	if len(rows) == 0 {
		out = append(out, "the documentation table has no rows")
	}
	byName := map[string]docRow{}
	for _, r := range rows {
		if _, dup := byName[r.Name]; dup {
			out = append(out, fmt.Sprintf("%s is documented twice", r.Name))
		}
		byName[r.Name] = r
	}
	declared := map[string]bool{}
	for _, v := range vars {
		declared[v.Name] = true
		r, ok := byName[v.Name]
		if !ok {
			out = append(out, fmt.Sprintf("%s is in Vars() but not documented", v.Name))
			continue
		}
		if r.Default != v.Default {
			out = append(out, fmt.Sprintf("%s: documented default %q, code default %q", v.Name, r.Default, v.Default))
		}
		if r.Required != v.Required {
			out = append(out, fmt.Sprintf("%s: documented required=%v, code required=%v", v.Name, r.Required, v.Required))
		}
		if r.Secret != v.Secret {
			out = append(out, fmt.Sprintf("%s: documented secret=%v, code secret=%v", v.Name, r.Secret, v.Secret))
		}
		if r.Meaning != v.Doc {
			out = append(out, fmt.Sprintf("%s: documented meaning differs from Var.Doc\n    doc:  %s\n    code: %s", v.Name, r.Meaning, v.Doc))
		}
	}
	for _, r := range rows {
		if !declared[r.Name] {
			out = append(out, fmt.Sprintf("%s is documented but not in Vars()", r.Name))
		}
	}
	return out
}

func readReference(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(referencePath))
	if err != nil {
		t.Fatalf("cannot read the configuration reference (a test that cannot run must fail): %v", err)
	}
	return string(b)
}

func TestConfigReference_MatchesVars(t *testing.T) {
	rows, err := parseReference(readReference(t))
	if err != nil {
		t.Fatal(err)
	}
	// The nonzero floor: a parser that found nothing is a broken instrument, not a clean document.
	if len(rows) == 0 {
		t.Fatal("found no rows in the documentation table: the instrument matched nothing")
	}
	if len(rows) != len(Vars()) {
		t.Errorf("found %d documented rows for %d variables", len(rows), len(Vars()))
	}
	if diffs := diffReference(rows, Vars()); len(diffs) != 0 {
		t.Fatalf("Docs/config-reference.md and Vars() disagree:\n  %s", strings.Join(diffs, "\n  "))
	}
}

// goodDoc renders a document that matches vars exactly, so each control below corrupts ONE thing in a
// document known to pass.
func goodDoc(vars []Var) string {
	var b strings.Builder
	b.WriteString("# Title\n\nprose\n\n" + referenceHeader + "\n" + separatorRow)
	for _, v := range vars {
		def := "`" + v.Default + "`"
		if v.Default == "" {
			def = "(none)"
		}
		yn := func(x bool) string {
			if x {
				return "yes"
			}
			return "no"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", v.Name, def, yn(v.Required), yn(v.Secret), v.Doc)
	}
	b.WriteString("\nmore prose\n")
	return b.String()
}

func TestConfigReference_CheckDetectsEveryKindOfDrift(t *testing.T) {
	vars := Vars()

	// Control zero: the generated document passes, so a failure below is caused by the corruption.
	rows, err := parseReference(goodDoc(vars))
	if err != nil || len(rows) != len(vars) || len(diffReference(rows, vars)) != 0 {
		t.Fatalf("control failed: the generated document does not pass: rows=%d err=%v diffs=%v", len(rows), err, diffReference(rows, vars))
	}

	corrupt := map[string]struct {
		mutate func(string) string
		want   string
	}{
		"a variable removed from the docs": {
			func(md string) string { return dropLine(md, "`"+VarEnv+"`") },
			VarEnv + " is in Vars() but not documented",
		},
		"a variable documented but not in code": {
			func(md string) string {
				return strings.Replace(md, separatorRow, separatorRow+"| `PLATFORM_GHOST` | `1` | no | no | x |\n", 1)
			},
			"PLATFORM_GHOST is documented but not in Vars()",
		},
		"a default changed": {
			func(md string) string { return strings.Replace(md, "`:8080`", "`:9999`", 1) },
			VarHTTPAddr + ": documented default",
		},
		"a required flag flipped": {
			func(md string) string { return flipFlag(md, VarDatabaseURL, 2) },
			VarDatabaseURL + ": documented required",
		},
		"a secret flag flipped": {
			func(md string) string { return flipFlag(md, VarDatabaseURL, 3) },
			VarDatabaseURL + ": documented secret",
		},
		"the meaning edited": {
			func(md string) string { return strings.Replace(md, "Free-form label", "Strict label", 1) },
			VarEnv + ": documented meaning differs",
		},
		"a variable documented twice": {
			func(md string) string {
				return strings.Replace(md, separatorRow, separatorRow+rowOf(md, VarEnv)+"\n", 1)
			},
			VarEnv + " is documented twice",
		},
	}
	for name, c := range corrupt {
		t.Run(name, func(t *testing.T) {
			md := c.mutate(goodDoc(vars))
			rows, err := parseReference(md)
			if err != nil {
				t.Fatalf("the corrupted document no longer parses (the control should reach the diff): %v", err)
			}
			diffs := strings.Join(diffReference(rows, vars), "\n")
			if !strings.Contains(diffs, c.want) {
				t.Fatalf("drift %q was not reported.\nwant a line containing %q\ngot: %q", name, c.want, diffs)
			}
		})
	}
}

func TestConfigReference_AnInstrumentThatMatchesNothingIsRed(t *testing.T) {
	// No table at all.
	if _, err := parseReference("# Title\n\nno table here\n"); err == nil {
		t.Fatal("a document without the table parsed without error")
	}
	// A header with no rows: the parser succeeds with zero rows, and the diff must be red.
	rows, err := parseReference(referenceHeader + "\n|---|---|---|---|---|\n\nprose\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected zero rows, got %d", len(rows))
	}
	if diffs := diffReference(rows, Vars()); len(diffs) == 0 || !strings.Contains(diffs[0], "no rows") {
		t.Fatalf("an empty table was not reported as red: %v", diffs)
	}
	// Variables declared, but a table that documents none of them.
	if diffs := diffReference(nil, Vars()); len(diffs) != len(Vars())+1 {
		t.Fatalf("diff of an empty table has %d lines, want one per variable plus the no-rows line", len(diffs))
	}
	// Malformed rows are errors, not silent skips.
	for name, md := range map[string]string{
		"missing separator": referenceHeader + "\n| `A` | `1` | no | no | x |\n",
		"short row":         referenceHeader + "\n|---|---|---|---|---|\n| `A` | `1` | no |\n",
		"bad flag":          referenceHeader + "\n|---|---|---|---|---|\n| `A` | `1` | maybe | no | x |\n",
	} {
		if _, err := parseReference(md); err == nil {
			t.Errorf("%s: a malformed table parsed without error", name)
		}
	}
}

func TestConfigReference_DocumentsTheOperationalRules(t *testing.T) {
	md := readReference(t)
	// The two rules the operator must not get wrong, asserted as prose the file must keep saying. Each
	// phrase list is a set of facts; the paired control proves the matcher can fail.
	rules := map[string][]string{
		"the migrator needs the direct session-mode connection, not the transaction pooler": {
			"migrator", "DIRECT", "session-mode", "transaction", "pooler", "advisory lock",
		},
		"the runtime URL is the least-privileged platform_app login": {
			"PLATFORM_DATABASE_URL", "least-privileged", "`platform_app`",
		},
	}
	for rule, phrases := range rules {
		for _, p := range phrases {
			if !strings.Contains(md, p) {
				t.Errorf("the reference no longer says %q (rule: %s)", p, rule)
			}
		}
	}
	if strings.Contains("a document that says nothing", "advisory lock") {
		t.Fatal("control failed: the matcher finds phrases that are not there")
	}
}

// ---- helpers for the drift controls ----------------------------------------------------------------------

func dropLine(md, contains string) string {
	var keep []string
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "|") && strings.Contains(l, contains) {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

func rowOf(md, name string) string {
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "| `"+name+"`") {
			return l
		}
	}
	return ""
}

// flipFlag flips the yes/no in the given cell index of the named variable's row.
func flipFlag(md, name string, cell int) string {
	row := rowOf(md, name)
	cells := strings.Split(row, "|")
	// cells[0] is the empty text before the first '|'; cell indexes in splitRow's numbering start at 0
	// for the Variable column, which is cells[1] here.
	c := strings.TrimSpace(cells[cell+1])
	flipped := " yes "
	if c == "yes" {
		flipped = " no "
	}
	cells[cell+1] = flipped
	return strings.Replace(md, row, strings.Join(cells, "|"), 1)
}
