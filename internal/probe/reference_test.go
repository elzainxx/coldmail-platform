package probe_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

// Docs/probe-reference.md must say what the code does, in both directions. The instrument reads the two
// tables out of the markdown and diffs them against Vars() and against the checks of a healthy run. A
// nonzero row floor makes an instrument that matched nothing RED instead of vacuously green, and the
// controls feed it documents that are wrong in each way to prove it notices.

const referencePath = "../../Docs/probe-reference.md"

const (
	varsHeader   = "| Variable | Default | Required | Secret | Meaning |"
	checksHeader = "| Check | Kind | Asserts |"
)

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

// parseTable returns the rows under header: the lines after the header and its separator, up to the
// first line that is not a table row. It errors on a missing header, a missing separator or a row with
// the wrong number of cells, so a hand-edit that breaks a table cannot be mistaken for "no differences".
func parseTable(md, header string, cols int) ([][]string, error) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("the table header %q was not found", header)
	}
	if start+1 >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[start+1]), "|---") {
		return nil, errors.New("the table header is not followed by a separator row")
	}
	var rows [][]string
	for _, l := range lines[start+2:] {
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			break
		}
		cells := splitRow(l)
		if len(cells) != cols {
			return nil, fmt.Errorf("row %q has %d cells, want %d", l, len(cells), cols)
		}
		for i := range cells {
			if cells[i] == "" {
				return nil, fmt.Errorf("row %q has an empty cell", l)
			}
		}
		rows = append(rows, cells)
	}
	return rows, nil
}

func tick(s string) string { return strings.Trim(s, "`") }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// diffVars returns one line per disagreement between the documented rows and vars, in both directions,
// and a line when there are no rows at all.
func diffVars(rows [][]string, vars []probe.Var) []string {
	var out []string
	if len(rows) == 0 {
		out = append(out, "the variables table has no rows")
	}
	byName := map[string][]string{}
	for _, r := range rows {
		if _, dup := byName[tick(r[0])]; dup {
			out = append(out, fmt.Sprintf("%s is documented twice", tick(r[0])))
		}
		byName[tick(r[0])] = r
	}
	declared := map[string]bool{}
	for _, v := range vars {
		declared[v.Name] = true
		r, ok := byName[v.Name]
		if !ok {
			out = append(out, fmt.Sprintf("%s is in Vars() but not documented", v.Name))
			continue
		}
		def := tick(r[1])
		if def == "(none)" {
			def = ""
		}
		if def != v.Default {
			out = append(out, fmt.Sprintf("%s: documented default %q, code default %q", v.Name, def, v.Default))
		}
		if r[2] != yesNo(v.Required) {
			out = append(out, fmt.Sprintf("%s: documented required=%s, code required=%v", v.Name, r[2], v.Required))
		}
		if r[3] != yesNo(v.Secret) {
			out = append(out, fmt.Sprintf("%s: documented secret=%s, code secret=%v", v.Name, r[3], v.Secret))
		}
		if r[4] != v.Doc {
			out = append(out, fmt.Sprintf("%s: documented meaning differs from Var.Doc\n    doc:  %s\n    code: %s", v.Name, r[4], v.Doc))
		}
	}
	for _, r := range rows {
		if !declared[tick(r[0])] {
			out = append(out, fmt.Sprintf("%s is documented but not in Vars()", tick(r[0])))
		}
	}
	return out
}

// diffChecks compares the documented check rows with the checks a healthy run executed: the same ids in
// the same order with the same kinds. NOT-APPLICABLE lines are not checks and are not in the table.
func diffChecks(rows [][]string, results []probe.Result) []string {
	var executed []probe.Result
	for _, r := range results {
		if r.Status != probe.StatusNotApplicable {
			executed = append(executed, r)
		}
	}
	var out []string
	if len(rows) == 0 {
		out = append(out, "the checks table has no rows")
	}
	docIDs := map[string]bool{}
	for _, r := range rows {
		docIDs[tick(r[0])] = true
	}
	codeIDs := map[string]bool{}
	for _, r := range executed {
		codeIDs[r.ID] = true
		if !docIDs[r.ID] {
			out = append(out, fmt.Sprintf("%s runs but is not documented", r.ID))
		}
	}
	for _, r := range rows {
		if !codeIDs[tick(r[0])] {
			out = append(out, fmt.Sprintf("%s is documented but does not run", tick(r[0])))
		}
	}
	if len(out) == 0 {
		for i, r := range rows {
			if i >= len(executed) {
				break
			}
			if tick(r[0]) != executed[i].ID {
				out = append(out, fmt.Sprintf("row %d documents %s, but the run's check %d is %s (order differs)", i+1, tick(r[0]), i+1, executed[i].ID))
				break
			}
			if r[1] != executed[i].Kind.String() {
				out = append(out, fmt.Sprintf("%s: documented kind %s, code kind %s", executed[i].ID, r[1], executed[i].Kind))
			}
		}
	}
	return out
}

func readReference(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(referencePath))
	if err != nil {
		t.Fatalf("cannot read the probe reference (a test that cannot run must fail): %v", err)
	}
	return string(b)
}

func TestReference_MatchesVars(t *testing.T) {
	rows, err := parseTable(readReference(t), varsHeader, 5)
	if err != nil {
		t.Fatal(err)
	}
	// The nonzero floor: a parser that found nothing is a broken instrument, not a clean document.
	if len(rows) == 0 {
		t.Fatal("found no rows in the variables table: the instrument matched nothing")
	}
	if len(rows) != len(probe.Vars()) {
		t.Errorf("found %d documented rows for %d variables", len(rows), len(probe.Vars()))
	}
	if diffs := diffVars(rows, probe.Vars()); len(diffs) != 0 {
		t.Fatalf("Docs/probe-reference.md and Vars() disagree:\n  %s", strings.Join(diffs, "\n  "))
	}
}

func TestReference_ChecksTableMatchesAHealthyRun(t *testing.T) {
	rows, err := parseTable(readReference(t), checksHeader, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("found no rows in the checks table: the instrument matched nothing")
	}
	st := start(t, probetest.Options{})
	o := run(t, st.Env(), nil)
	wantGreen(t, o)
	if len(rows) != healthyChecks {
		t.Errorf("found %d documented checks, a healthy run executes %d", len(rows), healthyChecks)
	}
	// The prose count is held to the same numbers as the table.
	if want := fmt.Sprintf("executes %d checks (%d positive, %d negative)", healthyChecks, healthyPositive, healthyNegative); !strings.Contains(readReference(t), want) {
		t.Errorf("the document no longer says %q", want)
	}
	if diffs := diffChecks(rows, o.report.Results); len(diffs) != 0 {
		t.Fatalf("Docs/probe-reference.md and a healthy run disagree:\n  %s", strings.Join(diffs, "\n  "))
	}
}

// ---- controls: the instrument above must notice each kind of drift ---------------------------------------------

func goodVarsDoc(vars []probe.Var) string {
	var b strings.Builder
	b.WriteString("# Title\n\nprose\n\n" + varsHeader + "\n|---|---|---|---|---|\n")
	for _, v := range vars {
		def := "`" + v.Default + "`"
		if v.Default == "" {
			def = "(none)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", v.Name, def, yesNo(v.Required), yesNo(v.Secret), v.Doc)
	}
	b.WriteString("\nmore prose\n")
	return b.String()
}

func goodChecksDoc(results []probe.Result) string {
	var b strings.Builder
	b.WriteString("# Title\n\n" + checksHeader + "\n|---|---|---|\n")
	for _, r := range results {
		if r.Status != probe.StatusNotApplicable {
			fmt.Fprintf(&b, "| `%s` | %s | text |\n", r.ID, r.Kind)
		}
	}
	b.WriteString("\nmore prose\n")
	return b.String()
}

func dropRow(md, name string) string {
	var keep []string
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "| `"+name+"`") {
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

// flipCell flips yes/no (or positive/negative) in the given cell of the named row.
func flipCell(md, name string, cell int, a, b string) string {
	row := rowOf(md, name)
	cells := strings.Split(row, "|")
	if strings.TrimSpace(cells[cell+1]) == a {
		cells[cell+1] = " " + b + " "
	} else {
		cells[cell+1] = " " + a + " "
	}
	return strings.Replace(md, row, strings.Join(cells, "|"), 1)
}

func TestReference_VarsCheckDetectsEveryKindOfDrift(t *testing.T) {
	vars := probe.Vars()

	// Control zero: the generated document passes, so a failure below is caused by the corruption.
	rows, err := parseTable(goodVarsDoc(vars), varsHeader, 5)
	if err != nil || len(rows) != len(vars) || len(diffVars(rows, vars)) != 0 {
		t.Fatalf("control failed: the generated document does not pass: rows=%d err=%v diffs=%v", len(rows), err, diffVars(rows, vars))
	}

	corrupt := map[string]struct {
		mutate func(string) string
		want   string
	}{
		"a variable removed from the docs": {
			func(md string) string { return dropRow(md, probe.VarM2Token) },
			probe.VarM2Token + " is in Vars() but not documented",
		},
		"a variable documented but not in code": {
			func(md string) string {
				return strings.Replace(md, "|---|---|---|---|---|\n", "|---|---|---|---|---|\n| `PROBE_GHOST` | `1` | no | no | x |\n", 1)
			},
			"PROBE_GHOST is documented but not in Vars()",
		},
		"a default changed": {
			func(md string) string { return strings.Replace(md, "`25`", "`99`", 1) },
			probe.VarMinChecks + ": documented default",
		},
		"a default added where there is none": {
			func(md string) string {
				return strings.Replace(md, "| `PROBE_M1_URL` | (none) |", "| `PROBE_M1_URL` | `x` |", 1)
			},
			probe.VarM1URL + ": documented default",
		},
		"a required flag flipped": {
			func(md string) string { return flipCell(md, probe.VarM3URL, 2, "yes", "no") },
			probe.VarM3URL + ": documented required",
		},
		"a secret flag flipped": {
			func(md string) string { return flipCell(md, probe.VarM1Key, 3, "yes", "no") },
			probe.VarM1Key + ": documented secret",
		},
		"the meaning edited": {
			func(md string) string { return strings.Replace(md, "Never printed.", "Printed.", 1) },
			": documented meaning differs",
		},
		"a variable documented twice": {
			func(md string) string {
				return strings.Replace(md, "\n\nmore prose", "\n"+rowOf(md, probe.VarTimeoutSec)+"\n\nmore prose", 1)
			},
			probe.VarTimeoutSec + " is documented twice",
		},
	}
	for name, c := range corrupt {
		t.Run(name, func(t *testing.T) {
			rows, err := parseTable(c.mutate(goodVarsDoc(vars)), varsHeader, 5)
			if err != nil {
				t.Fatalf("the corrupted document no longer parses (the control should reach the diff): %v", err)
			}
			diffs := strings.Join(diffVars(rows, vars), "\n")
			if !strings.Contains(diffs, c.want) {
				t.Fatalf("drift %q was not reported.\nwant a line containing %q\ngot: %q", name, c.want, diffs)
			}
		})
	}
}

func TestReference_ChecksCheckDetectsEveryKindOfDrift(t *testing.T) {
	st := start(t, probetest.Options{})
	results := run(t, st.Env(), nil).report.Results

	rows, err := parseTable(goodChecksDoc(results), checksHeader, 3)
	if err != nil || len(rows) != healthyChecks || len(diffChecks(rows, results)) != 0 {
		t.Fatalf("control failed: the generated document does not pass: rows=%d err=%v diffs=%v", len(rows), err, diffChecks(rows, results))
	}

	corrupt := map[string]struct {
		mutate func(string) string
		want   string
	}{
		"a check removed from the docs": {
			func(md string) string { return dropRow(md, "m3.describe.pricing") },
			"m3.describe.pricing runs but is not documented",
		},
		"a check documented that does not run": {
			func(md string) string {
				return strings.Replace(md, "|---|---|---|\n", "|---|---|---|\n| `m9.healthz.ok` | positive | x |\n", 1)
			},
			"m9.healthz.ok is documented but does not run",
		},
		"a kind flipped": {
			func(md string) string {
				return flipCell(md, "m2.describe.no_token.rejected", 1, "negative", "positive")
			},
			"m2.describe.no_token.rejected: documented kind positive, code kind negative",
		},
		"two rows swapped": {
			func(md string) string {
				a, b := rowOf(md, "m1.healthz.ok"), rowOf(md, "m1.readyz.ok")
				md = strings.Replace(md, a, "@@A@@", 1)
				md = strings.Replace(md, b, a, 1)
				return strings.Replace(md, "@@A@@", b, 1)
			},
			"order differs",
		},
	}
	for name, c := range corrupt {
		t.Run(name, func(t *testing.T) {
			rows, err := parseTable(c.mutate(goodChecksDoc(results)), checksHeader, 3)
			if err != nil {
				t.Fatalf("the corrupted document no longer parses: %v", err)
			}
			diffs := strings.Join(diffChecks(rows, results), "\n")
			if !strings.Contains(diffs, c.want) {
				t.Fatalf("drift %q was not reported.\nwant a line containing %q\ngot: %q", name, c.want, diffs)
			}
		})
	}
}

func TestReference_AnInstrumentThatMatchesNothingIsRed(t *testing.T) {
	// No table at all.
	if _, err := parseTable("# Title\n\nno table here\n", varsHeader, 5); err == nil {
		t.Fatal("a document without the table parsed without error")
	}
	// A header with no rows: the parser succeeds with zero rows, and both diffs must be red.
	rows, err := parseTable(varsHeader+"\n|---|---|---|---|---|\n\nprose\n", varsHeader, 5)
	if err != nil || len(rows) != 0 {
		t.Fatalf("expected zero rows and no error, got %d rows, err %v", len(rows), err)
	}
	if diffs := diffVars(rows, probe.Vars()); len(diffs) == 0 || !strings.Contains(diffs[0], "no rows") {
		t.Fatalf("an empty variables table was not reported as red: %v", diffs)
	}
	if diffs := diffChecks(nil, []probe.Result{{ID: "a.b", Status: probe.StatusPass}}); len(diffs) == 0 || !strings.Contains(diffs[0], "no rows") {
		t.Fatalf("an empty checks table was not reported as red: %v", diffs)
	}
	// Malformed rows are errors, not silent skips.
	for name, md := range map[string]string{
		"missing separator": varsHeader + "\n| `A` | `1` | no | no | x |\n",
		"short row":         varsHeader + "\n|---|---|---|---|---|\n| `A` | `1` | no |\n",
		"empty cell":        varsHeader + "\n|---|---|---|---|---|\n| `A` | | no | no | x |\n",
	} {
		if _, err := parseTable(md, varsHeader, 5); err == nil {
			t.Errorf("%s: a malformed table parsed without error", name)
		}
	}
	// And the real document is not one of these: it has both tables with rows.
	md := readReference(t)
	for header, cols := range map[string]int{varsHeader: 5, checksHeader: 3} {
		if rows, err := parseTable(md, header, cols); err != nil || len(rows) == 0 {
			t.Errorf("the real document yields %d rows for %q (err %v)", len(rows), header, err)
		}
	}
}
