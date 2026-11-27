package suppress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// THE CSV IMPORT (Phase 6, A4). One call reads the list, classifies every line and writes the valid addresses through AddMany, which validates them first: this file writes nothing itself.
//
// WHAT A FILE IS. A header row that names an `email` column (any letter case, surrounding space ignored; a byte order mark before it is dropped) and one address per row in that column. Every other column is
// ignored. At most maxRows data rows are read (the platform's row dial, the same limit a job's list has): the next one is a *TooManyRowsError and nothing is written. A header that is missing, names no email
// column or names it twice is an *ImportHeaderError and nothing is written.
//
// WHAT A LINE BECOMES. Every data line is exactly one of: added (a new entry, source `import`), already there (the user had it, or the file names it twice: the first row stands, its source included) and
// invalid, with a reason from the closed vocabulary of reasons.go. A line that is not valid CSV, or has no cell for the email column, is invalid with the reason bad_line. An address is never echoed: not in an
// error, not in the sample, which holds the first InvalidSampleMax invalid lines as (line, reason) in file order. The line is the line of the file the row starts on (the header is line 1).
//
// WHAT IS NOT ATOMIC. The file is read whole (at most maxRows rows) and then written in batches; a failure of the database part way leaves the batches already written written, and the call returns the error
// with the counts so far. A repeat of the same file is harmless: what was written is "already there".

// InvalidSampleMax is how many invalid lines an ImportResult names.
const InvalidSampleMax = 20

// InvalidLine is one invalid line of an import: its line number and why, as a word of Reasons.
type InvalidLine struct {
	Line   int
	Reason string
}

// ImportResult is what ImportCSV did. Invalid counts every invalid line; Sample holds the first InvalidSampleMax of them in file order.
type ImportResult struct {
	Added        int
	AlreadyThere int
	Invalid      int
	Sample       []InvalidLine
}

// TooManyRowsError: the file has more data rows than the limit. It never holds a row.
type TooManyRowsError struct{ Limit int }

func (e *TooManyRowsError) Error() string {
	return fmt.Sprintf("suppress.import: the file has more than %d rows", e.Limit)
}

// The reasons a file is refused before any line is read as an address: closed words.
const (
	HeaderNone        = "no_header"
	HeaderNoEmail     = "no_email_column"
	HeaderEmailTwice  = "email_column_twice"
	HeaderUnreadable  = "header_unreadable"
	maxImportRowsHard = 1 << 24 // a dial above this is a mistake of the caller, not a limit
)

// ImportHeaderError: the file has no usable header. Reason is one of the Header words; the text never holds a cell.
type ImportHeaderError struct{ Reason string }

func (e *ImportHeaderError) Error() string {
	return "suppress.import: the header is not usable: " + e.Reason
}

// ImportCSV imports the CSV in body for the user (see the notes above). q must survive a failed statement (the pool or a plain connection). maxRows is the most data rows to read (at least one). Errors:
// *ImportHeaderError, *TooManyRowsError, ErrBadUser, a failure of the database (mapped as AddMany maps it), and whatever the body's reader returns (a cap of bytes, a dropped connection), unwrapped so that
// errors.As reaches an *http.MaxBytesError.
func ImportCSV(ctx context.Context, q Execer, userID string, body io.Reader, maxRows int) (ImportResult, error) {
	if !uuidShape.MatchString(userID) {
		return ImportResult{}, opErr("import", ErrBadUser)
	}
	if q == nil {
		return ImportResult{}, opErr("import", errNoExecer)
	}
	if maxRows < 1 || maxRows > maxImportRowsHard {
		return ImportResult{}, opErr("import", fmt.Errorf("the row limit %d is not usable", maxRows))
	}
	br := bufio.NewReader(body)
	if head, err := br.Peek(3); err == nil && bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3) // a byte order mark, which a spreadsheet writes before the header
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1 // a row with another number of columns than the header is judged by its email cell, not refused as a file
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return ImportResult{}, &ImportHeaderError{Reason: HeaderNone}
	}
	if err != nil {
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			return ImportResult{}, &ImportHeaderError{Reason: HeaderUnreadable}
		}
		return ImportResult{}, err
	}
	col := -1
	for i, h := range header {
		if strings.EqualFold(strings.TrimSpace(h), "email") {
			if col >= 0 {
				return ImportResult{}, &ImportHeaderError{Reason: HeaderEmailTwice}
			}
			col = i
		}
	}
	if col < 0 {
		return ImportResult{}, &ImportHeaderError{Reason: HeaderNoEmail}
	}

	var (
		emails []string
		lines  []int // the line of each address in emails
		bad    []InvalidLine
		rows   int
	)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		var pe *csv.ParseError
		switch {
		case err != nil && errors.As(err, &pe):
			rows++
			if rows > maxRows {
				return ImportResult{}, &TooManyRowsError{Limit: maxRows}
			}
			bad = append(bad, InvalidLine{Line: pe.StartLine, Reason: ReasonBadLine})
			continue
		case err != nil:
			return ImportResult{}, err
		}
		rows++
		if rows > maxRows {
			return ImportResult{}, &TooManyRowsError{Limit: maxRows}
		}
		line, _ := cr.FieldPos(0)
		if col >= len(rec) {
			bad = append(bad, InvalidLine{Line: line, Reason: ReasonBadLine})
			continue
		}
		line, _ = cr.FieldPos(col)
		emails = append(emails, rec[col])
		lines = append(lines, line)
	}

	res := ImportResult{}
	many, err := AddMany(ctx, q, userID, emails, SourceImport)
	res.Added, res.AlreadyThere = many.Added, many.AlreadyThere
	for _, r := range many.Rejected {
		bad = append(bad, InvalidLine{Line: lines[r.Index], Reason: r.Reason})
	}
	sort.SliceStable(bad, func(i, j int) bool { return bad[i].Line < bad[j].Line })
	res.Invalid = len(bad)
	res.Sample = bad[:min(len(bad), InvalidSampleMax)]
	if err != nil {
		return res, opErr("import", err)
	}
	return res, nil
}
