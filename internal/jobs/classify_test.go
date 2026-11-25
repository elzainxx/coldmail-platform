package jobs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The classification (what job_rows.state, reason, duplicate_of and email_norm hold) against an ORACLE written by hand from the rule's own words (internal/norm: trim, split at the one '@',
// IDNA of the domain then the trailing dots, NFC + lower-case of the local part; then module 1's syntax layer: local at most 64 bytes, domain at most 255, a dot in the domain), never from
// the code under test. Every refusal has its legal twin in the same table: the boundary that is still accepted.

// seqIDs makes ids that are readable in a failure: 00000000-0000-4000-8000-000000000001, ...
func seqIDs() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
}

type oracleRow struct {
	in     string
	state  string
	norm   string // "" for a malformed row
	reason string
	dupOf  int // the ORDINAL of the row it repeats (0: none)
}

func oracleTable() []oracleRow {
	long64 := strings.Repeat("a", 64)
	return []oracleRow{
		/* 1  */ {"alice@example.com", StatePending, "alice@example.com", "", 0},
		/* 2  */ {"  Bob@Example.COM  ", StatePending, "bob@example.com", "", 0}, // padded, mixed case
		/* 3  */ {"ALICE@example.com", StateDuplicate, "alice@example.com", "", 1}, // repeated in another case
		/* 4  */ {"bob@example.com", StateDuplicate, "bob@example.com", "", 2}, // the padded one, typed plain
		/* 5  */ {"", StateMalformed, "", "empty", 0},
		/* 6  */ {"   \t ", StateMalformed, "", "empty", 0},
		/* 7  */ {"no-at-sign.example.com", StateMalformed, "", "no_at", 0},
		/* 8  */ {"a@b@c.com", StateMalformed, "", "multiple_at", 0},
		/* 9  */ {"@example.com", StateMalformed, "", "empty_local", 0},
		/* 10 */ {"a@", StateMalformed, "", "empty_domain", 0},
		/* 11 */ {"user@localhost", StateMalformed, "", "domain_no_dot", 0}, // norm/v1 accepts a one-label domain, module 1's syntax layer does not
		/* 12 */ {strings.Repeat("a", 65) + "@example.com", StateMalformed, "", "local_too_long", 0},
		/* 13 */ {long64 + "@example.com", StatePending, long64 + "@example.com", "", 0}, // the twin: 64 bytes is still fine
		/* 14 */ {"müller@bücher.example", StatePending, "müller@xn--bcher-kva.example", "", 0}, // IDN: the domain becomes punycode, the local part stays unicode
		/* 15 */ {"MÜLLER@BÜCHER.EXAMPLE", StateDuplicate, "müller@xn--bcher-kva.example", "", 14}, // repeated after normalising
		/* 16 */ {"carol@example.com.", StatePending, "carol@example.com", "", 0}, // the trailing dot goes
		/* 17 */ {"Carol@EXAMPLE.com", StateDuplicate, "carol@example.com", "", 16}, // repeated only AFTER normalising: the first had a dot
		/* 18 */ {"dave@exa mple.com", StateMalformed, "", "bad_idna", 0},
		/* 19 */ {"erin@example.com\x00", StateMalformed, "", ReasonInvalidText, 0}, // a NUL: PostgreSQL text cannot hold it
		/* 20 */ {"frank@" + strings.Repeat("a", 256) + ".com", StateMalformed, "", "domain_too_long", 0},
		/* 21 */ {strings.Repeat("a", 5000) + "@example.com", StateMalformed, "", ReasonTooLong, 0}, // more than job_rows keeps
		/* 22 */ {"bad\xffbyte@example.com", StateMalformed, "", ReasonInvalidText, 0}, // not UTF-8
		/* 23 */ {"alice@Example.Com", StateDuplicate, "alice@example.com", "", 1}, // a THIRD occurrence names the FIRST row, not the second
		/* 24 */ {"gina@example.com", StatePending, "gina@example.com", "", 0}, // a clean row after the mess
	}
}

func TestClassifyRows_AgreesWithTheHandComputedOracle(t *testing.T) {
	table := oracleTable()
	in := make([]RowIn, len(table))
	for i, r := range table {
		in[i] = RowIn{Email: r.in}
	}
	got, err := classifyRows(in, seqIDs())
	if err != nil {
		t.Fatalf("classifyRows: %v", err)
	}
	if len(got) != len(table) {
		t.Fatalf("%d rows in, %d classified: a row was lost or invented", len(table), len(got))
	}
	for i, want := range table {
		g := got[i]
		ord := i + 1
		if g.Ordinal != ord {
			t.Errorf("row %d: ordinal %d, want %d (1-based, input order)", ord, g.Ordinal, ord)
		}
		if g.State != want.state || g.Norm != want.norm || g.Reason != want.reason {
			t.Errorf("row %d: got state %q norm %q reason %q, want state %q norm %q reason %q", ord, g.State, g.Norm, g.Reason, want.state, want.norm, want.reason)
		}
		wantDup := ""
		if want.dupOf != 0 {
			wantDup = got[want.dupOf-1].ID
		}
		if g.DuplicateOf != wantDup {
			t.Errorf("row %d: duplicate_of %q, want the id of row %d (%q)", ord, g.DuplicateOf, want.dupOf, wantDup)
		}
	}
}

// A malformed row keeps no address anywhere it is not the user's own raw text: its reason is a word, its normalised address is empty (job_rows.email_norm is NULL by the table's CHECK), and it
// is never the target of a duplicate.
func TestClassifyRows_MalformedRowsHoldNoAddressInTheirReason(t *testing.T) {
	table := oracleTable()
	in := make([]RowIn, len(table))
	for i, r := range table {
		in[i] = RowIn{Email: r.in}
	}
	got, _ := classifyRows(in, seqIDs())
	malformed := 0
	targets := map[string]bool{}
	for _, g := range got {
		if g.DuplicateOf != "" {
			targets[g.DuplicateOf] = true
		}
	}
	for i, g := range got {
		if g.State != StateMalformed {
			if g.Reason != "" {
				t.Errorf("row %d (%s) carries a reason %q: only malformed rows have one", i+1, g.State, g.Reason)
			}
			continue
		}
		malformed++
		if g.Norm != "" {
			t.Errorf("row %d is malformed and holds the normalised address %q: email_norm must be NULL", i+1, g.Norm)
		}
		if strings.ContainsAny(g.Reason, "@. ") || g.Reason == "" || len(g.Reason) > 32 {
			t.Errorf("row %d: the reason %q is not a word", i+1, g.Reason)
		}
		if targets[g.ID] {
			t.Errorf("row %d is malformed and a duplicate names it as the row it repeats", i+1)
		}
	}
	if malformed < 10 {
		t.Fatalf("only %d malformed rows in the oracle: the test would prove little", malformed)
	}
}

// What is stored as email_raw must be storable: valid UTF-8, no NUL, at most MaxRawBytes. The reason a row is malformed does not change with the cleaning.
func TestClassifyRows_StoredRawTextIsAlwaysStorable(t *testing.T) {
	in := []RowIn{{Email: "x\x00y@example.com"}, {Email: "bad\xffbyte@example.com"}, {Email: strings.Repeat("é", 3000) + "@example.com"}, {Email: strings.Repeat("a", MaxRawBytes) + "x"}, {Email: "ok@example.com"}}
	got, err := classifyRows(in, seqIDs())
	if err != nil {
		t.Fatal(err)
	}
	for i, g := range got {
		if !textOK(g.Raw) || len(g.Raw) > MaxRawBytes {
			t.Errorf("row %d: email_raw is not storable (valid=%v, %d bytes)", i+1, textOK(g.Raw), len(g.Raw))
		}
	}
	wantReasons := []string{ReasonInvalidText, ReasonInvalidText, ReasonTooLong, ReasonTooLong, ""}
	for i, g := range got {
		if g.Reason != wantReasons[i] {
			t.Errorf("row %d: reason %q, want %q", i+1, g.Reason, wantReasons[i])
		}
	}
	// the twin: text exactly at the limit with a valid address is not refused for its length
	exact := strings.Repeat(" ", MaxRawBytes-len("ok@example.com")) + "ok@example.com"
	if len(exact) != MaxRawBytes {
		t.Fatalf("the twin is %d bytes, want %d", len(exact), MaxRawBytes)
	}
	g, _ := classifyRows([]RowIn{{Email: exact}}, seqIDs())
	if g[0].State != StatePending || g[0].Norm != "ok@example.com" {
		t.Errorf("a padded address of exactly %d bytes is %s / %q, want pending / ok@example.com", MaxRawBytes, g[0].State, g[0].Norm)
	}
}

func TestClassifyRows_FieldsAreKeptAndBounded(t *testing.T) {
	ok := map[string]string{"first_name": "Ada", "city": "Zürich"}
	got, err := classifyRows([]RowIn{{Email: "a@example.com", Fields: ok}, {Email: "b@example.com"}}, seqIDs())
	if err != nil {
		t.Fatalf("legal fields were refused: %v", err)
	}
	if got[0].Fields != `{"city":"Zürich","first_name":"Ada"}` {
		t.Errorf("fields = %s, want the object with sorted keys", got[0].Fields)
	}
	if got[1].Fields != "{}" {
		t.Errorf("a row with no fields holds %s, want {}", got[1].Fields)
	}

	many := map[string]string{}
	for i := 0; i < MaxFieldKeys+1; i++ {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	atLimit := map[string]string{}
	for i := 0; i < MaxFieldKeys; i++ {
		atLimit[fmt.Sprintf("k%d", i)] = "v"
	}
	fat := map[string]string{"a": strings.Repeat("x", MaxFieldBytes)} // the key counts too: 1 + 4096 bytes
	exact := map[string]string{"a": strings.Repeat("x", MaxFieldBytes-1)}
	for _, c := range []struct {
		name   string
		f      map[string]string
		reason string // "" = accepted (the legal twin)
	}{
		{"64 keys is the limit", atLimit, ""},
		{"65 keys", many, "65 merge fields, the limit is 64"},
		{"4096 bytes in total is the limit", exact, ""},
		{"4097 bytes", fat, "merge fields hold 4097 bytes, the limit is 4096"},
		{"a blank key", map[string]string{" ": "v"}, "a merge field has a blank name"},
		{"a NUL in a value", map[string]string{"a": "x\x00"}, "valid UTF-8 text without NUL"},
		{"a value that is not UTF-8", map[string]string{"a": "x\xff"}, "valid UTF-8 text without NUL"},
	} {
		_, err := classifyRows([]RowIn{{Email: "a@example.com"}, {Email: "b@example.com", Fields: c.f}}, seqIDs())
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.reason != "" && err == nil:
			t.Errorf("%s: accepted, want a refusal naming %q", c.name, c.reason)
		case c.reason != "":
			if !isBadRequest(err) || !strings.Contains(err.Error(), c.reason) || !strings.Contains(err.Error(), "row 2") {
				t.Errorf("%s: got %v, want a bad request naming row 2 and %q", c.name, err, c.reason)
			}
			if strings.Contains(err.Error(), "example.com") {
				t.Errorf("%s: the refusal quotes an address: %v", c.name, err)
			}
		}
	}
}

func TestClassifyChain_SelectsByVerdictAndNeverSendsAnUnreadableOne(t *testing.T) {
	res := []sourceResult{
		{"r1", "v1@example.com", VerdictValid},
		{"r2", "i1@example.com", VerdictInvalid},
		{"r3", "k1@example.com", VerdictRisky},
		{"r4", "u1@example.com", VerdictUnknown},
		{"r5", "w1@example.com", "teleported"}, // a verdict this store does not know
		{"r6", "v1@example.com", VerdictValid}, // an impossible repeat: still caught
		{"r7", "v2@example.com", VerdictValid},
	}
	for _, c := range []struct {
		choice string
		want   []string // the state of each result
	}{
		{UnknownExclude, []string{StatePending, StateExcluded, StatePending, StateExcluded, StateExcluded, StateDuplicate, StatePending}},
		{UnknownInclude, []string{StatePending, StateExcluded, StatePending, StatePending, StateExcluded, StateDuplicate, StatePending}}, // the twin: the choice flips exactly the unknown row
	} {
		got := classifyChain(res, c.choice, seqIDs())
		for i, g := range got {
			if g.State != c.want[i] {
				t.Errorf("choice %s, result %d (%s): state %s, want %s", c.choice, i+1, res[i].Verdict, g.State, c.want[i])
			}
			if g.SourceItem != res[i].ItemRef || g.Ordinal != i+1 || g.Norm != res[i].Email || g.Raw != res[i].Email {
				t.Errorf("choice %s, result %d: source item %q ordinal %d norm %q raw %q, want the result's own", c.choice, i+1, g.SourceItem, g.Ordinal, g.Norm, g.Raw)
			}
			if g.State == StateExcluded && g.Reason != excludedReason(res[i].Verdict) {
				t.Errorf("choice %s, result %d: excluded with reason %q, want the verdict", c.choice, i+1, g.Reason)
			}
		}
		if got[5].DuplicateOf != got[0].ID {
			t.Errorf("choice %s: the repeat names %q, want the first row %q", c.choice, got[5].DuplicateOf, got[0].ID)
		}
	}
}

func TestClassifyChain_ResultsTheTableCannotKeepAreMalformedNotACrash(t *testing.T) {
	res := []sourceResult{
		{"r1", "nul\x00@example.com", VerdictValid},
		{"r2", strings.Repeat("a", MaxRawBytes+1), VerdictValid},
		{"r3", "", VerdictValid},
		{"r4", "ok@example.com", VerdictValid},
	}
	got := classifyChain(res, UnknownExclude, seqIDs())
	want := []struct{ state, reason string }{{StateMalformed, ReasonInvalidText}, {StateMalformed, ReasonTooLong}, {StateMalformed, ReasonInvalidText}, {StatePending, ""}}
	for i, g := range got {
		if g.State != want[i].state || g.Reason != want[i].reason {
			t.Errorf("result %d: %s / %q, want %s / %q", i+1, g.State, g.Reason, want[i].state, want[i].reason)
		}
		if !textOK(g.Raw) || len(g.Raw) > MaxRawBytes || g.Raw == "" {
			t.Errorf("result %d: email_raw is not a storable non-empty text", i+1)
		}
		if g.State == StateMalformed && g.Norm != "" {
			t.Errorf("result %d: malformed but email_norm is %q", i+1, g.Norm)
		}
	}
}

// The rule labels of the platform's own results: malformed, duplicate and cancelled are rows of the charge table, read from it and required to be free there; suppressed and excluded are the
// store's two labels.
func TestResultRule_ComesFromTheChargeTable(t *testing.T) {
	for state, want := range map[string]string{StateMalformed: "P-08", StateDuplicate: "P-07", "cancelled": "P-01", StateSuppressed: RuleSuppressed, StateExcluded: RuleExcluded} {
		got, err := resultRule(state)
		if err != nil || got != want {
			t.Errorf("resultRule(%q) = %q, %v; want %q", state, got, err, want)
		}
	}
	// the control: a state the charge table does not make free (it is unmapped there) is an error, never a label
	if r, err := resultRule("teleported"); err == nil {
		t.Errorf("resultRule(teleported) = %q: a platform outcome the table does not make free must be refused", r)
	}
	if RuleSuppressed != "P-SUP" || RuleExcluded != "P-EXC" {
		t.Errorf("the two labels are %q and %q, want P-SUP and P-EXC", RuleSuppressed, RuleExcluded)
	}
}

func TestSelects_TheVerdictRule(t *testing.T) {
	for _, c := range []struct {
		verdict, choice string
		want            bool
	}{
		{VerdictValid, UnknownExclude, true}, {VerdictValid, UnknownInclude, true},
		{VerdictRisky, UnknownExclude, true}, {VerdictRisky, UnknownInclude, true},
		{VerdictUnknown, UnknownExclude, false}, {VerdictUnknown, UnknownInclude, true},
		{VerdictInvalid, UnknownExclude, false}, {VerdictInvalid, UnknownInclude, false},
		{"", UnknownInclude, false}, {"VALID", UnknownInclude, false}, {"unmapped", UnknownInclude, false},
	} {
		if got := selects(c.verdict, c.choice); got != c.want {
			t.Errorf("selects(%q, %q) = %v, want %v", c.verdict, c.choice, got, c.want)
		}
	}
}

func isBadRequest(err error) bool { return errors.Is(err, ErrBadRequest) }
