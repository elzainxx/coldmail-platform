package suppress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The CSV import (import.go): the header, the classification of every line into added, already there and invalid with a reason of the closed vocabulary, the bound on the file, the sample, and that no
// address is ever echoed. The database is a scripted fake (see removal_test.go); the real statement's behaviour (a duplicate is skipped, the first row stands) is proved in the harness.

// acceptAll answers every batch statement as if every address in it were new.
func acceptAll() *scriptExec {
	return &scriptExec{fn: func(_ int, _ string, args []any) (pgconn.CommandTag, error) {
		return tagOf("INSERT", len(args[1].([]string))), nil
	}}
}

func TestImportCSV_ClassifiesEveryLineAndCountsThem(t *testing.T) {
	// line 1 is the header; the user already had one address of the seven that reach the database
	csv := "Name , EMAIL ,note\n" +
		"Ada,ada@example.com,x\n" + // 2 added
		"Bob,  Bob@EXAMPLE.com ,\n" + // 3 added (normalised)
		"Cy,not-an-address,\n" + // 4 invalid: no_at
		"Di,,\n" + // 5 invalid: empty
		"Ed,ed@@example.com,\n" + // 6 invalid: multiple_at
		"Flo,flo@example,\n" + // 7 invalid: domain_no_dot
		"Gus,gus@example.com,\n" + // 8 added (or already there)
		"Hal\n" + // 9 invalid: bad_line (no cell for the email column)
		"Ivy,ada@example.com,again\n" // 10 the same address twice in the file: already there
	f := &scriptExec{fn: func(_ int, _ string, args []any) (pgconn.CommandTag, error) {
		return tagOf("INSERT", len(args[1].([]string))-1), nil // one of the valid ones was already on the list
	}}
	res, err := ImportCSV(context.Background(), f, userA, strings.NewReader(csv), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d statement(s), want one batch", len(f.calls))
	}
	if got := f.calls[0].args[1].([]string); strings.Join(got, ",") != "ada@example.com,bob@example.com,gus@example.com,ada@example.com" {
		t.Fatalf("the batch carried %v, want the valid addresses in order, normalised", got)
	}
	if f.calls[0].args[0] != userA || f.calls[0].args[2] != SourceImport {
		t.Fatalf("the user and the source written are %v and %v", f.calls[0].args[0], f.calls[0].args[2])
	}
	if res.Added != 3 || res.AlreadyThere != 1 || res.Invalid != 5 {
		t.Fatalf("result %+v, want 3 added, 1 already there, 5 invalid", res)
	}
	var got []string
	for _, s := range res.Sample {
		got = append(got, fmt.Sprintf("%d:%s", s.Line, s.Reason))
	}
	if strings.Join(got, ",") != "4:no_at,5:empty,6:multiple_at,7:domain_no_dot,9:bad_line" {
		t.Fatalf("the sample is %v", got)
	}
	// every word is one of the closed vocabulary
	vocab := map[string]bool{}
	for _, w := range Reasons() {
		vocab[w] = true
	}
	for _, s := range res.Sample {
		if !vocab[s.Reason] {
			t.Errorf("the reason %q is not in the vocabulary", s.Reason)
		}
	}
}

func TestImportCSV_TheSampleIsBoundedAndTheCountIsNot(t *testing.T) {
	var b strings.Builder
	b.WriteString("email\n")
	for i := 0; i < 57; i++ {
		fmt.Fprintf(&b, "nobody%d\n", i) // 57 lines with no @: all invalid
	}
	b.WriteString("fine@example.com\n")
	res, err := ImportCSV(context.Background(), acceptAll(), userA, strings.NewReader(b.String()), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Invalid != 57 || len(res.Sample) != InvalidSampleMax || InvalidSampleMax != 20 || res.Added != 1 {
		t.Fatalf("invalid %d, sample %d, added %d: want 57 invalid, a sample of 20 and one added", res.Invalid, len(res.Sample), res.Added)
	}
	if res.Sample[0].Line != 2 || res.Sample[19].Line != 21 {
		t.Fatalf("the sample runs from line %d to line %d, want 2 to 21 (the first twenty, in file order)", res.Sample[0].Line, res.Sample[19].Line)
	}
	// the control: a file with fewer than twenty invalid lines names them all
	res, err = ImportCSV(context.Background(), acceptAll(), userA, strings.NewReader("email\nx\ny\nz@example.com\n"), 10)
	if err != nil || res.Invalid != 2 || len(res.Sample) != 2 {
		t.Fatalf("control: %+v %v", res, err)
	}
}

func TestImportCSV_TheHeaderMustNameTheEmailColumnOnceAndNothingIsWrittenOtherwise(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"an empty file":                   {"", HeaderNone},
		"only blank lines":                {"\n\n", HeaderNone},
		"no email column":                 {"name,address\nAda,ada@example.com\n", HeaderNoEmail},
		"the email column twice":          {"email,Email\nada@example.com,x\n", HeaderEmailTwice},
		"a header with a broken quote":    {"em\"ail,x\nada@example.com,y\n", HeaderUnreadable},
		"the email in the first row only": {"ada@example.com\nbob@example.com\n", HeaderNoEmail},
	} {
		f := acceptAll()
		_, err := ImportCSV(context.Background(), f, userA, strings.NewReader(c.body), 10)
		var h *ImportHeaderError
		if !errors.As(err, &h) || h.Reason != c.want {
			t.Errorf("%s: %v, want an *ImportHeaderError with %q", name, err, c.want)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: %d statement(s) for a file with no usable header", name, len(f.calls))
		}
		if err != nil && strings.Contains(err.Error(), "ada@example.com") {
			t.Errorf("%s: the error holds an address: %v", name, err)
		}
	}
	// the controls: the header in any letter case, among other columns, after a byte order mark, with CRLF line ends
	for name, body := range map[string]string{
		"lower case":        "email\nada@example.com\n",
		"upper case":        "EMAIL\r\nada@example.com\r\n",
		"among columns":     "id, e-mail ,Email\n1,x,ada@example.com\n",
		"a byte order mark": "\xEF\xBB\xBFemail\nada@example.com\n",
	} {
		res, err := ImportCSV(context.Background(), acceptAll(), userA, strings.NewReader(body), 10)
		if err != nil || res.Added != 1 || res.Invalid != 0 {
			t.Errorf("control %s: %+v %v", name, res, err)
		}
	}
}

func TestImportCSV_AFileLongerThanTheLimitIsRefusedAndNothingIsWritten(t *testing.T) {
	body := "email\na@example.com\nb@example.com\nc@example.com\nd@example.com\n"
	f := acceptAll()
	_, err := ImportCSV(context.Background(), f, userA, strings.NewReader(body), 3)
	var many *TooManyRowsError
	if !errors.As(err, &many) || many.Limit != 3 {
		t.Fatalf("four rows against a limit of three: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("%d statement(s) for a file over the limit", len(f.calls))
	}
	if strings.Contains(err.Error(), "@example.com") {
		t.Fatalf("the error holds an address: %v", err)
	}
	// the control: the same file with the limit at its length is imported
	res, err := ImportCSV(context.Background(), acceptAll(), userA, strings.NewReader(body), 4)
	if err != nil || res.Added != 4 {
		t.Fatalf("control: %+v %v", res, err)
	}
	// a row that is not valid CSV counts against the limit too (a file of nothing but broken lines cannot run past it)
	broken := "email\n" + strings.Repeat("\"x\n", 30)
	if _, err := ImportCSV(context.Background(), acceptAll(), userA, strings.NewReader(broken), 5); err == nil {
		t.Fatal("a file of broken lines went past the limit")
	}
}

func TestImportCSV_ALineThatIsNotCSVIsInvalidWithItsLineAndTheRestIsStillImported(t *testing.T) {
	body := "email,name\n" +
		"ada@example.com,Ada\n" + // 2
		"bob@example.com,\"Bob \"the builder\"\n" + // 3: a quote inside a quoted field
		"cy@example.com,Cy\n" // 4
	f := acceptAll()
	res, err := ImportCSV(context.Background(), f, userA, strings.NewReader(body), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Invalid == 0 || res.Sample[0].Reason != ReasonBadLine || res.Sample[0].Line != 3 {
		t.Fatalf("the broken line was not reported as line 3 with bad_line: %+v", res)
	}
	if res.Added == 0 || len(f.calls) != 1 {
		t.Fatalf("what could be read was not imported: %+v after %d statement(s)", res, len(f.calls))
	}
}

func TestImportCSV_APassingAddressThatTheTableRefusesIsInvalidWithCannotStore(t *testing.T) {
	f := &scriptExec{fn: func(_ int, _ string, args []any) (pgconn.CommandTag, error) {
		if _, batch := args[1].([]string); batch {
			return pgconn.CommandTag{}, pgErr("23514", "suppressions_email_norm_v1", "violates", "Key (email)=(secret@example.com)")
		}
		if args[1] == "odd one@example.com" {
			return pgconn.CommandTag{}, pgErr("23514", "suppressions_email_norm_v1", "violates", "")
		}
		return tagOf("INSERT", 1), nil
	}}
	res, err := ImportCSV(context.Background(), f, userA, strings.NewReader("email\nada@example.com\nodd one@example.com\nbob@example.com\n"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Invalid != 1 || res.Sample[0].Line != 3 || res.Sample[0].Reason != ReasonCannotStore {
		t.Fatalf("result %+v, want 2 added and line 3 invalid as cannot_store", res)
	}
}

func TestImportCSV_RefusesWhatItCannotUseAndPassesOnWhatTheBodyReports(t *testing.T) {
	if _, err := ImportCSV(context.Background(), acceptAll(), "nope", strings.NewReader("email\n"), 10); !errors.Is(err, ErrBadUser) {
		t.Errorf("a bad user: %v", err)
	}
	if _, err := ImportCSV(context.Background(), nil, userA, strings.NewReader("email\n"), 10); err == nil {
		t.Error("no connection is not an error")
	}
	for _, n := range []int{0, -1, 1 << 30} {
		if _, err := ImportCSV(context.Background(), acceptAll(), userA, strings.NewReader("email\n"), n); err == nil {
			t.Errorf("a row limit of %d was accepted", n)
		}
	}
	// a body that fails part way is the body's own error (a size cap's *http.MaxBytesError reaches the caller through errors.As), and nothing was written
	boom := errors.New("the connection was dropped")
	f := acceptAll()
	_, err := ImportCSV(context.Background(), f, userA, io.MultiReader(strings.NewReader("email\na@example.com\n"), iotest.ErrReader(boom)), 10)
	if !errors.Is(err, boom) || len(f.calls) != 0 {
		t.Fatalf("a failing body: %v after %d statement(s)", err, len(f.calls))
	}
	// a failing database is reported with the counts so far and no address
	g := &scriptExec{fn: func(int, string, []any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, pgErr("23503", "suppressions_user_id_fkey", "no such user", "Key (email)=(canary@example.com)")
	}}
	res, err := ImportCSV(context.Background(), g, userA, strings.NewReader("email\ncanary@example.com\nbad\n"), 10)
	if !errors.Is(err, ErrUnknownUser) || strings.Contains(err.Error(), "canary@") || res.Invalid != 1 {
		t.Fatalf("a failing database: %+v %v", res, err)
	}
}

func TestStore_AddReadsBackTheRowThatAnswersAndRemoveAndImportAreTheFunctionsOfTheFile(t *testing.T) {
	// Store.Add writes through Add (manual) and reads the row back; a removal between the two makes it write once more, and a second miss is a conflict, never a made-up entry
	db := &storeDB{}
	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(nil); !errors.Is(err, ErrNoDB) {
		t.Fatalf("a store with no database: %v", err)
	}
	db.row = &Entry{Email: "ada@example.com", Source: SourceUnsubscribe}
	e, created, err := s.Add(context.Background(), userA, " Ada@Example.com")
	if err != nil || created || e.Email != "ada@example.com" || e.Source != SourceUnsubscribe {
		t.Fatalf("Add: %+v %v %v (an entry that was already there keeps its own source)", e, created, err)
	}
	db.tag, db.row = "INSERT 0 1", &Entry{Email: "bob@example.com", Source: SourceManual}
	if e, created, err := s.Add(context.Background(), userA, "bob@example.com"); err != nil || !created || e.Source != SourceManual {
		t.Fatalf("Add of a new address: %+v %v %v", e, created, err)
	}
	writes := db.execs
	db.row = nil
	if _, _, err := s.Add(context.Background(), userA, "gone@example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("an entry that vanished twice: %v", err)
	}
	if db.execs-writes != 2 {
		t.Fatalf("%d write(s) for an entry that could not be read back, want two (one retry)", db.execs-writes)
	}
	db.row, db.tag = &Entry{}, "DELETE 1"
	if ok, err := s.Remove(context.Background(), userA, "ada@example.com"); err != nil || !ok {
		t.Fatalf("Remove: %v %v", ok, err)
	}
}

// storeDB is a DB for the Store's tests: every Exec counts and answers its tag ("INSERT 0 0" when none is set), and a QueryRow answers the one row it holds or pgx.ErrNoRows.
type storeDB struct {
	tag   string
	row   *Entry
	execs int
}

func (d *storeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.execs++
	if d.tag == "" {
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	return pgconn.NewCommandTag(d.tag), nil
}

type entryRow struct{ e *Entry }

func (r entryRow) Scan(dest ...any) error {
	if r.e == nil {
		return pgx.ErrNoRows
	}
	*(dest[0].(*string)), *(dest[1].(*time.Time)) = r.e.Source, time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	return nil
}

func (d *storeDB) QueryRow(context.Context, string, ...any) pgx.Row { return entryRow{d.row} }
func (d *storeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("storeDB: Query is not used by these tests")
}
