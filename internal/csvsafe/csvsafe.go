// Package csvsafe writes CSV that opens safely in a spreadsheet and still reads back exactly (Phase 4, "Locked here" 30).
//
// THE THREAT. A cell that starts with =, +, - or @ (or a tab or a carriage return) is read by Excel, LibreOffice and Google Sheets as a formula: `=HYPERLINK(...)`, `=cmd|' /C calc'!A0`. The values of a results file
// are the customer's own list (addresses, merge fields), so the platform cannot trust a cell.
//
// THE RULE. Every cell is written through encoding/csv (commas, quotes and line breaks are quoted by the standard rules), and a cell that starts with one of = + - @ TAB CR or ' gets ONE leading apostrophe.
// The inverse is "strip one leading apostrophe when there is one", and the mapping is INJECTIVE: a cell that starts with an apostrophe is prefixed too (so the three-character cell apostrophe, equals, x is written with TWO apostrophes in front of the equals sign and a reader that strips one gets the original back, never one that starts with the equals sign). Unescape(Cell(s)) == s for every string, which the property test proves over awkward alphabets, and no written cell begins with a formula trigger.
//
// NOT CHECKED (printed by the package's tests and in Docs/jobs-api.md): the full-width and other look-alike characters of a formula trigger (U+FF1D and friends), a trigger after leading white space or a BOM,
// a spreadsheet's own handling of a quoted cell (the apostrophe is shown by Excel and hidden by others: the file is meant for exchange, and Unescape is the exact inverse for a program), the encoding of the bytes (the
// caller writes UTF-8), and a reader that normalises a carriage return inside a quoted cell (encoding/csv's Reader turns CRLF into LF there; this package's writer keeps what it was given).
package csvsafe

import (
	"encoding/csv"
	"io"
	"strings"
)

// trigger is every first byte that makes a spreadsheet read a cell as a formula, plus the apostrophe this package uses as its mark.
const trigger = "=+-@\t\r'"

// Cell returns the cell as it is written: s itself, or s with one leading apostrophe when it starts with a formula trigger or an apostrophe.
func Cell(s string) string {
	if s != "" && strings.IndexByte(trigger, s[0]) >= 0 {
		return "'" + s
	}
	return s
}

// Unescape is the exact inverse of Cell: it strips one leading apostrophe when there is one.
func Unescape(s string) string {
	if strings.HasPrefix(s, "'") {
		return s[1:]
	}
	return s
}

// Triggers reports whether a cell, as it would be OPENED, starts with a formula trigger: the check a test applies to every written cell.
func Triggers(s string) bool {
	return s != "" && strings.IndexByte("=+-@\t\r", s[0]) >= 0
}

// Writer writes records with every cell escaped by Cell.
type Writer struct {
	w *csv.Writer
}

// NewWriter returns a Writer over w. Rows end with LF (the standard library's default).
func NewWriter(w io.Writer) *Writer { return &Writer{w: csv.NewWriter(w)} }

// Write writes one record, each field through Cell.
func (w *Writer) Write(record []string) error {
	out := make([]string, len(record))
	for i, f := range record {
		out[i] = Cell(f)
	}
	return w.w.Write(out)
}

// Flush writes buffered data to the underlying writer; Error reports the first error of a Write or Flush (the usual encoding/csv contract).
func (w *Writer) Flush() { w.w.Flush() }

// Error returns the first error the writer met.
func (w *Writer) Error() error { return w.w.Error() }
