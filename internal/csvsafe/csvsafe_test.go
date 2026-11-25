package csvsafe

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// parseRFC4180 is the EXACT reader the tests use as the oracle: encoding/csv's Reader turns CRLF inside a quoted cell into LF, which would make a round trip of a cell holding CRLF look lossy when it is not.
// Rows end with LF (what this package's writer emits); a quoted cell may hold anything; a quote inside is doubled.
func parseRFC4180(data string) ([][]string, error) {
	var rows [][]string
	var row []string
	var cell strings.Builder
	inQuotes, quoted := false, false
	flushCell := func() { row = append(row, cell.String()); cell.Reset(); quoted = false }
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inQuotes:
			if c == '"' {
				if i+1 < len(data) && data[i+1] == '"' {
					cell.WriteByte('"')
					i++
				} else {
					inQuotes = false
				}
			} else {
				cell.WriteByte(c)
			}
		case c == '"' && cell.Len() == 0 && !quoted:
			inQuotes, quoted = true, true
		case c == ',':
			flushCell()
		case c == '\n':
			flushCell()
			rows = append(rows, row)
			row = nil
		default:
			cell.WriteByte(c)
		}
	}
	if inQuotes {
		return nil, fmt.Errorf("unterminated quoted cell")
	}
	if cell.Len() > 0 || len(row) > 0 || quoted {
		flushCell()
		rows = append(rows, row)
	}
	return rows, nil
}

func write(t *testing.T, rows [][]string) string {
	t.Helper()
	var b bytes.Buffer
	w := NewWriter(&b)
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return b.String()
}

// awkward is the table the exit criterion names (commas, quotes, newlines, =cmd cells) and the neighbours that break a naive escape.
var awkward = []string{
	"", " ", "plain", "a,b", `say "hi"`, `"`, `""`, "line1\nline2", "line1\r\nline2", "\n", "\r", "\r\n", "tab\there", "\ttab first", " =leading space",
	"=cmd|' /C calc'!A0", "=HYPERLINK(\"http://example.invalid\",\"x\")", "+1+1", "-2+3", "@SUM(A1)", "'", "'=x", "''", "'''=x", "=", "+", "-", "@",
	"=1,2", "=\"quoted\"", "=1\n=2", "é", "世界", "=世界", "a'b", "x=1", "\x00nul", "emoji 😀", "=😀",
}

func TestRoundTrip_AwkwardValuesAreExactAndNoCellOpensAsAFormula(t *testing.T) {
	var rows [][]string
	for i := 0; i < len(awkward); i += 3 {
		var r []string
		for j := i; j < i+3 && j < len(awkward); j++ {
			r = append(r, awkward[j])
		}
		rows = append(rows, r)
	}
	out := write(t, rows)
	back, err := parseRFC4180(out)
	if err != nil {
		t.Fatalf("the file does not parse: %v\n%s", err, out)
	}
	if len(back) != len(rows) {
		t.Fatalf("%d row(s) came back, wrote %d", len(back), len(rows))
	}
	for i := range rows {
		if len(back[i]) != len(rows[i]) {
			t.Fatalf("row %d: %d cell(s) came back, wrote %d: %q", i, len(back[i]), len(rows[i]), back[i])
		}
		for j := range rows[i] {
			if Triggers(back[i][j]) {
				t.Errorf("row %d cell %d opens as a formula in a spreadsheet: %q (written from %q)", i, j, back[i][j], rows[i][j])
			}
			if got := Unescape(back[i][j]); got != rows[i][j] {
				t.Errorf("row %d cell %d: %q came back as %q", i, j, rows[i][j], got)
			}
		}
	}
}

// the paired controls: a writer that does not escape fails the safety half, a reader that does not unescape fails the exactness half
func TestRoundTrip_ControlsFailWhenTheBehaviourIsBroken(t *testing.T) {
	var b bytes.Buffer
	raw := csv.NewWriter(&b) // the standard writer: commas and quotes are right, formulas are not
	if err := raw.Write([]string{"=cmd|' /C calc'!A0", "ok"}); err != nil {
		t.Fatal(err)
	}
	raw.Flush()
	back, err := parseRFC4180(b.String())
	if err != nil {
		t.Fatal(err)
	}
	if !Triggers(back[0][0]) {
		t.Fatal("the control writer (no escaping) did not produce a cell that opens as a formula: the safety check cannot fail")
	}
	safe, err := parseRFC4180(write(t, [][]string{{"=cmd|' /C calc'!A0", "ok"}}))
	if err != nil {
		t.Fatal(err)
	}
	if Triggers(safe[0][0]) {
		t.Fatal("the real writer left a formula cell")
	}
	if safe[0][0] == "=cmd|' /C calc'!A0" {
		t.Fatal("the real writer did not change the cell")
	}
	// a reader that forgets to unescape does not get the value back
	if safe[0][0] == Unescape(safe[0][0]) {
		t.Fatal("Unescape changed nothing on an escaped cell: the exactness check cannot fail")
	}
	// the mapping is injective: a naive "prefix only when the cell starts with a trigger and strip a prefix only when the rest starts with a trigger" fails on a cell that starts with an apostrophe
	naiveCell := func(s string) string {
		if Triggers(s) {
			return "'" + s
		}
		return s
	}
	naiveUnescape := func(s string) string {
		if strings.HasPrefix(s, "'") && len(s) > 1 && Triggers(s[1:]) {
			return s[1:]
		}
		return s
	}
	if naiveUnescape(naiveCell("'=x")) == "'=x" {
		t.Fatal("the naive scheme survived the cell \"'=x\": the injectivity control cannot fail")
	}
	if Unescape(Cell("'=x")) != "'=x" {
		t.Fatal("the real scheme does not round-trip \"'=x\"")
	}
}

// the property: for arbitrary strings over an alphabet of the awkward bytes, the round trip is exact and no cell opens as a formula
func TestRoundTrip_PropertyOverArbitraryStrings(t *testing.T) {
	alphabet := []string{",", "\"", "\r", "\n", "=", "+", "-", "@", "'", "\t", " ", "a", "Z", "0", "é", "世", "😀", "\x00", "|", "\\"}
	rng := rand.New(rand.NewSource(20261125)) // deterministic: a failure is reproducible
	const rounds = 5000
	for n := 0; n < rounds; n++ {
		cells := make([]string, 1+rng.Intn(4))
		for i := range cells {
			var sb strings.Builder
			for k, l := 0, rng.Intn(9); k < l; k++ {
				sb.WriteString(alphabet[rng.Intn(len(alphabet))])
			}
			cells[i] = sb.String()
		}
		out := write(t, [][]string{cells})
		back, err := parseRFC4180(out)
		if err != nil || len(back) != 1 || len(back[0]) != len(cells) {
			t.Fatalf("round %d: %q wrote %q which does not read back as one row of %d cell(s): %v %q", n, cells, out, len(cells), err, back)
		}
		for i := range cells {
			if Triggers(back[0][i]) {
				t.Fatalf("round %d: cell %q opens as a formula after writing (%q)", n, cells[i], back[0][i])
			}
			if got := Unescape(back[0][i]); got != cells[i] {
				t.Fatalf("round %d: cell %q came back as %q (file %q)", n, cells[i], got, out)
			}
		}
	}
	t.Logf("NOTE csvsafe: %d random rows over a %d-symbol alphabet round-tripped exactly; NOT CHECKED: look-alike characters of a trigger (U+FF1D), a trigger after leading white space or a BOM, a reader that normalises CRLF inside a quoted cell", rounds, len(alphabet))
}

func TestCell_ExactRules(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, {"a", "a"}, {"=a", "'=a"}, {"+a", "'+a"}, {"-a", "'-a"}, {"@a", "'@a"}, {"\ta", "'\ta"}, {"\ra", "'\ra"}, {"'a", "''a"},
		{"a=", "a="}, {" =a", " =a"}, {"a'", "a'"},
	} {
		if got := Cell(c.in); got != c.want {
			t.Errorf("Cell(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Unescape strips ONE apostrophe and nothing else
	for _, c := range []struct{ in, want string }{{"", ""}, {"a", "a"}, {"'a", "a"}, {"''a", "'a"}, {"'", ""}} {
		if got := Unescape(c.in); got != c.want {
			t.Errorf("Unescape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
