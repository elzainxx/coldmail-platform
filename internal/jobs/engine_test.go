package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
)

// The pure half of the engine's side of the store (engine.go, render.go): what is decided in Go before the database is asked. The statements themselves are proved against a real Postgres by
// tests/harness/jobs_engine_test.go.

const (
	idA = "aaaaaaaa-0000-4000-8000-000000000001"
	idB = "bbbbbbbb-0000-4000-8000-000000000002"
	idC = "cccccccc-0000-4000-8000-000000000003"
)

func TestPrepareItems_NormalisesDedupesAndSortsOutWhatCannotBeRecorded(t *testing.T) {
	sent := charge.Outcome{Module: "m2", State: "sent"}
	queued := charge.Outcome{Module: "m2", State: "queued"}
	items := []ResultItem{
		{ItemRef: idA, Outcome: sent},
		{ItemRef: strings.ToUpper(idA), Outcome: sent}, // the same item in another spelling: one item
		{ItemRef: " " + idB + " ", Outcome: charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"}}, // padded: normalised
		{ItemRef: idC, Outcome: queued},        // not final: never recorded
		{ItemRef: "not-a-uuid", Outcome: sent}, // no row can have this reference
		{ItemRef: "", Outcome: sent},
		{ItemRef: "dddddddd-0000-4000-8000-000000000004", Outcome: charge.Outcome{Module: "m2"}}, // no state at all
	}
	final, rejected, notFinal, err := prepareItems("m2", items)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 2 || final[0].ref != idA || final[1].ref != idB {
		t.Fatalf("final = %+v, want the items %s and %s, in order, once each", final, idA, idB)
	}
	if final[0].decision.Kind != charge.Charge || final[0].decision.Unit != "m2_accepted" || final[0].decision.Rule != "M2-01" {
		t.Errorf("the sent item is decided %+v, want a charge of m2_accepted under M2-01", final[0].decision)
	}
	if final[1].decision.Kind != charge.Free || final[1].decision.Rule != "M2-03" {
		t.Errorf("the failed item is decided %+v, want free under M2-03", final[1].decision)
	}
	if rejected != 3 || notFinal != 1 {
		t.Errorf("rejected %d, notFinal %d; want 3 (no uuid, blank, no state) and 1 (queued)", rejected, notFinal)
	}
	if final[0].detail != "{}" {
		t.Errorf("an item with no detail carries %q, want {}", final[0].detail)
	}
	// the control of the module check: the same items for the right module are accepted, for another module the whole call is refused and names no address
	_, _, _, err = prepareItems("m1", items[:1])
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), `"m2"`) {
		t.Errorf("an outcome of module m2 in a job of module m1: got %v, want ErrBadRequest naming the module", err)
	}
	if _, _, _, err := prepareItems("m2", items[:1]); err != nil {
		t.Errorf("the control: the same item for its own module is refused: %v", err)
	}
}

func TestPrepareItems_Module3OutcomesAreFreeUnderTheM3Rules(t *testing.T) {
	final, _, nf, err := prepareItems("m3", []ResultItem{
		{ItemRef: idA, Outcome: charge.Outcome{Module: "m3", State: "sent"}},
		{ItemRef: idB, Outcome: charge.Outcome{Module: "m3", State: "cancelled"}},
	})
	if err != nil || nf != 0 || len(final) != 2 {
		t.Fatalf("got %d final, %d not final, %v", len(final), nf, err)
	}
	if final[0].decision.Kind != charge.Free || final[0].decision.Rule != "M3-99" || final[1].decision.Kind != charge.Free || final[1].decision.Rule != "M3-01" {
		t.Errorf("module 3 decisions: %+v and %+v, want free M3-99 and free M3-01", final[0].decision, final[1].decision)
	}
}

func TestDecide_TheLedgerIsTheAuthorityOnWhatWasCharged(t *testing.T) {
	charged := preparedItem{ref: idA, decision: charge.Decision{Kind: charge.Charge, Unit: "m1_own", Rule: "M1-03"}}
	free := preparedItem{ref: idB, decision: charge.Decision{Kind: charge.Free, Rule: "M1-21"}}
	unm := preparedItem{ref: idC, decision: charge.Decision{Kind: charge.Unmapped, Rule: "M1-23"}}
	for _, c := range []struct {
		name       string
		p          preparedItem
		ledgerUnit string
		unpriced   bool
		metered    bool
		want       resultDecision
		wantErr    bool
	}{
		{"a charge the ledger holds is recorded under the ledger's unit", charged, "m1_own", false, true, resultDecision{"charge", "m1_own", "M1-03"}, false},
		{"the first charge stands even when the table now says another unit", charged, "m1_cache", false, true, resultDecision{"charge", "m1_cache", "M1-03"}, false},
		{"a charge the ledger holds wins over a free decision (a module changing its mind)", free, "m1_own", false, true, resultDecision{"charge", "m1_own", "M1-21"}, false},
		{"a charge the snapshot cannot price is unmapped and has no unit", charged, "", true, true, resultDecision{"unmapped", "", "M1-03"}, false},
		{"a charge for a module that holds no credits is unmapped, never charged", charged, "", false, false, resultDecision{"unmapped", "", "M1-03"}, false},
		{"a free outcome", free, "", false, true, resultDecision{"free", "", "M1-21"}, false},
		{"an outcome the table cannot classify", unm, "", false, true, resultDecision{"unmapped", "", "M1-23"}, false},
		{"a charge the table asks for and the ledger does not hold is an error, never a guess", charged, "", false, true, resultDecision{}, true},
	} {
		got, err := decide(c.p, c.ledgerUnit, c.unpriced, c.metered)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v (error %v)", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestDetailJSON(t *testing.T) {
	if got := detailJSON(nil); got != "{}" {
		t.Errorf("nil detail = %q", got)
	}
	if got := detailJSON(map[string]any{"a": 1, "b": "x"}); got != `{"a":1,"b":"x"}` {
		t.Errorf("small detail = %q", got)
	}
	if got := detailJSON(map[string]any{"c": make(chan int)}); got != `{"detail":"not_encodable"}` {
		t.Errorf("a value that cannot be encoded = %q", got)
	}
	big := map[string]any{"s": strings.Repeat("x", MaxDetailBytes)}
	if got := detailJSON(big); !strings.HasPrefix(got, `{"detail":"too_large","bytes":`) {
		t.Errorf("a detail over the limit = %q", got)
	}
	// the control: a detail just under the limit is kept whole
	ok := map[string]any{"s": strings.Repeat("x", MaxDetailBytes-20)}
	if got := detailJSON(ok); !strings.Contains(got, strings.Repeat("x", 100)) || strings.Contains(got, "too_large") {
		t.Errorf("a detail under the limit was replaced: %.60s", got)
	}
	// jsonb cannot hold \u0000 (PostgreSQL refuses it): a detail that encodes it is replaced, never sent
	if got := detailJSON(map[string]any{"s": "bad\x00nul"}); got != `{"detail":"not_text"}` {
		t.Errorf("a NUL in a detail must not reach the database: %q", got)
	}
}

func TestCheckRendered(t *testing.T) {
	long := strings.Repeat("a", MaxSubjectBytes+1)
	for _, c := range []struct {
		name                         string
		subject, html, text, replyTo string
		wantErr                      string // a word of the reason, "" for accepted
		wantText                     string
	}{
		{"a full message", "Hello", "<p>hi</p>", "hi", "r@example.test", "", "hi"},
		{"a blank text becomes NULL (no plain-text part)", "Hello", "<p>hi</p>", "  \n\t ", "", "", ""},
		{"an empty text becomes NULL", "Hello", "<p>hi</p>", "", "", "", ""},
		{"a text only, no html, is a message", "Hello", "", "hi", "", "", "hi"},
		{"a blank subject", "  ", "<p>hi</p>", "hi", "", "subject is blank", ""},
		{"a subject over the header limit", long, "<p>hi</p>", "", "", "subject is", ""},
		{"a subject of exactly the limit", strings.Repeat("a", MaxSubjectBytes), "<p>hi</p>", "", "", "", ""},
		{"a subject on two lines", "a\nb", "<p>hi</p>", "", "", "not one line", ""},
		{"no body at all", "Hello", " ", "\n", "", "no body", ""},
		{"a NUL in the html", "Hello", "a\x00b", "", "", "not valid UTF-8", ""},
		{"a body over a MiB", "Hello", strings.Repeat("a", MaxBodyBytes+1), "", "", "html is", ""},
		{"a text over a MiB", "Hello", "<p>x</p>", strings.Repeat("a", MaxBodyBytes+1), "", "text is", ""},
		{"a body of exactly a MiB", "Hello", strings.Repeat("a", MaxBodyBytes), "", "", "", ""},
	} {
		m, err := checkRendered(7, c.subject, c.html, c.text, c.replyTo)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			} else if m.text != c.wantText {
				t.Errorf("%s: text %q, want %q", c.name, m.text, c.wantText)
			}
			continue
		}
		var br *BadRequestError
		if !errors.As(err, &br) || !errors.Is(err, ErrBadRequest) || !strings.Contains(br.Reason, c.wantErr) || !strings.Contains(br.Reason, "row 7") {
			t.Errorf("%s: got %v, want ErrBadRequest naming row 7 and %q", c.name, err, c.wantErr)
		}
		if err != nil && (strings.Contains(err.Error(), c.html) && len(c.html) > 8) {
			t.Errorf("%s: the refusal quotes the message", c.name)
		}
	}
}

type failingRenderer struct{}

func (failingRenderer) Render(Content, EngineRow) (string, string, string, error) {
	return "", "", "", errors.New("merge field {first_name} of alice@example.test is missing")
}

func TestPlainRenderer_IsTheIdentityAndARefusingRendererNamesOnlyTheRow(t *testing.T) {
	c := Content{Subject: "S", HTML: "<b>h</b>", Text: "t", ReplyTo: "r@example.test"}
	s, h, x, err := PlainRenderer{}.Render(c, EngineRow{ID: idA, Ordinal: 3, EmailNorm: "a@example.test", Fields: map[string]string{"k": "v"}})
	if err != nil || s != "S" || h != "<b>h</b>" || x != "t" {
		t.Errorf("PlainRenderer = %q %q %q %v, want the content as given", s, h, x, err)
	}
	m, err := renderRow(PlainRenderer{}, c, EngineRow{Ordinal: 3})
	if err != nil || m.subject != "S" || m.html != "<b>h</b>" || m.text != "t" || m.replyTo != "r@example.test" {
		t.Errorf("renderRow(PlainRenderer) = %+v %v", m, err)
	}
	_, err = renderRow(failingRenderer{}, c, EngineRow{Ordinal: 9})
	var br *BadRequestError
	if !errors.As(err, &br) || !strings.Contains(br.Reason, "row 9") {
		t.Fatalf("a refusing renderer: got %v, want ErrBadRequest naming row 9", err)
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "first_name") {
		t.Errorf("the renderer's own text, which quotes the row, reached the error: %v", err)
	}
}

func TestCheckIDs(t *testing.T) {
	got, err := checkIDs([]string{idA, idB, idA})
	if err != nil || len(got) != 2 || got[0] != idA || got[1] != idB {
		t.Errorf("checkIDs = %v, %v; want the two ids once each, in order", got, err)
	}
	for _, bad := range []string{"", "x", strings.ToUpper(idA), idA + "0", "aaaaaaaa-0000-4000-8000-00000000000g"} {
		if _, err := checkIDs([]string{idA, bad}); !errors.Is(err, ErrBadRequest) {
			t.Errorf("checkIDs accepted %q: %v", bad, err)
		}
	}
	if got, err := checkIDs(nil); err != nil || len(got) != 0 {
		t.Errorf("checkIDs(nil) = %v, %v", got, err)
	}
}

// listOfIDs is n distinct canonical row ids.
func listOfIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
	}
	return out
}

// MaxEngineIDs is the longest list a job may hold: module 1 takes the WHOLE list in one request, so the engine must be able to name every row of the largest job in one call (it was 20000, and a module 1
// job above that could never start). The equality is pinned both ways, so that a change to either number is read: the store's own limit (DefaultMaxRows, module 1's limit) and the engine's.
func TestMaxEngineIDs_IsTheLongestListAJobMayHoldAndNotOneMore(t *testing.T) {
	engine, store := MaxEngineIDs, DefaultMaxRows
	if engine != store || engine != 100000 {
		t.Fatalf("MaxEngineIDs = %d, DefaultMaxRows = %d: the engine must be able to name every row of the largest job in one call (module 1's own limit is 100000)", engine, store)
	}
	// checkIDs: exactly the limit is accepted (the control), one more is refused for its count and not for what the ids look like
	got, err := checkIDs(listOfIDs(MaxEngineIDs))
	if err != nil || len(got) != MaxEngineIDs {
		t.Fatalf("checkIDs of %d valid ids = %d ids, %v; want all of them accepted", MaxEngineIDs, len(got), err)
	}
	_, err = checkIDs(listOfIDs(MaxEngineIDs + 1))
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), fmt.Sprint(MaxEngineIDs+1)) || !strings.Contains(err.Error(), fmt.Sprint("the limit is ", MaxEngineIDs)) {
		t.Errorf("checkIDs of %d valid ids: %v; want ErrBadRequest naming the count and the limit", MaxEngineIDs+1, err)
	}
	// the limit of PendingRows is the same number (its check is a pure function: the call itself needs a database, tests/harness/jobs_engine_test.go runs it at the limit)
	for _, c := range []struct {
		limit int
		ok    bool
	}{{1, true}, {MaxEngineIDs, true}, {0, false}, {-1, false}, {MaxEngineIDs + 1, false}} {
		err := checkPendingLimit(c.limit)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrBadRequest)) {
			t.Errorf("checkPendingLimit(%d) = %v, want accepted=%v (and ErrBadRequest when refused)", c.limit, err, c.ok)
		}
	}
	// and the calls refuse what the limit refuses before any database is asked (a store with no pool would panic if they got further)
	s := &Store{}
	if _, err := s.PendingRows(context.Background(), idA, MaxEngineIDs+1); !errors.Is(err, ErrBadRequest) {
		t.Errorf("PendingRows(limit %d) = %v, want ErrBadRequest", MaxEngineIDs+1, err)
	}
	if err := s.MarkSubmitted(context.Background(), idA, listOfIDs(MaxEngineIDs+1)); !errors.Is(err, ErrBadRequest) {
		t.Errorf("MarkSubmitted of %d ids = %v, want ErrBadRequest", MaxEngineIDs+1, err)
	}
	if _, err := s.EnsureModuleItems(context.Background(), idA, listOfIDs(MaxEngineIDs+1)); !errors.Is(err, ErrBadRequest) {
		t.Errorf("EnsureModuleItems of %d ids = %v, want ErrBadRequest", MaxEngineIDs+1, err)
	}
}

func TestCleanReason(t *testing.T) {
	if got := cleanReason("  the module\trefused\nthe   job \x00 "); got != "the module refused the job" {
		t.Errorf("cleanReason = %q", got)
	}
	if got := cleanReason("   \n "); got != "" {
		t.Errorf("a blank reason must stay blank, got %q", got)
	}
	if got := cleanReason(strings.Repeat("é", maxReasonBytes)); len(got) > maxReasonBytes || !textOK(got) {
		t.Errorf("a long reason is %d bytes (valid text: %v), want at most %d and valid", len(got), textOK(got), maxReasonBytes)
	}
}

func TestIsTerminalAndUnresolved(t *testing.T) {
	for _, s := range []string{StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired} {
		if !IsTerminal(s) {
			t.Errorf("%s is terminal", s)
		}
	}
	for _, s := range []string{StatusDraft, StatusPreviewed, StatusConfirmed, StatusScheduled, StatusRunning, StatusPaused, StatusCancelling, StatusCompleting, "", "weird"} {
		if IsTerminal(s) {
			t.Errorf("%q is not terminal", s)
		}
	}
	if got := (EngineJob{Pending: 10, PendingResults: 7}).Unresolved(); got != 3 {
		t.Errorf("Unresolved = %d, want 3", got)
	}
	if got := (EngineJob{Pending: 4, PendingResults: 4, Results: 9}).Unresolved(); got != 0 {
		t.Errorf("Unresolved = %d, want 0 (platform results do not count)", got)
	}
}

func TestMapDBError_TheEngineSRefusals(t *testing.T) {
	s := &Store{}
	if got := s.fail(pgErr("23505", "jobs_module_job_ref_key")); !errors.Is(got, ErrModuleRefTaken) {
		t.Errorf("the module reference index: got %v, want ErrModuleRefTaken", got)
	}
	// the control: the same name under another SQLSTATE, and another index, are not it
	if got := s.fail(pgErr("23514", "jobs_module_job_ref_key")); errors.Is(got, ErrModuleRefTaken) {
		t.Error("a check violation was mapped to ErrModuleRefTaken")
	}
	if got := s.fail(pgErr("23505", "jobs_pkey")); errors.Is(got, ErrModuleRefTaken) {
		t.Error("another unique index was mapped to ErrModuleRefTaken")
	}
	for _, c := range []struct{ constraint, reason string }{
		{"job_started_early", "job_started_early"}, {"job_expired_early", "job_expired_early"}, {"job_cancelled_cannot_succeed", "job_cancelled_cannot_succeed"},
		{"job_cancelled_needs_a_cancel", "job_cancelled_needs_a_cancel"}, {"jobs_terminal_is_final", "jobs_terminal_is_final"}, {"job_rows_frozen", "job_rows_frozen"},
	} {
		var na *NotAllowedError
		got := s.fail(pgErr("23001", c.constraint))
		if !errors.As(got, &na) || na.Reason != c.reason {
			t.Errorf("%s: got %v, want ErrNotAllowed with that reason", c.constraint, got)
		}
	}
	inc := opErr("complete", &IncompleteError{Missing: 3})
	var ie *IncompleteError
	if !errors.Is(inc, ErrIncomplete) || !errors.As(inc, &ie) || ie.Missing != 3 || errors.Is(inc, ErrNotAllowed) || !strings.Contains(inc.Error(), "3 pending row(s)") {
		t.Errorf("IncompleteError = %v", inc)
	}
	if refusalWord(inc) != "incomplete" || refusalWord(ErrModuleRefTaken) != "module_ref_taken" {
		t.Errorf("refusal words: %q, %q", refusalWord(inc), refusalWord(ErrModuleRefTaken))
	}
}

// topLevelColumns counts the items of a select list, splitting only at the commas that are not inside parentheses or quotes.
func topLevelColumns(list string) int {
	depth, n, quoted := 0, 1, false
	for _, c := range list {
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			n++
		}
	}
	return n
}

// engineJobColumns is jobColumns with the campaign taken out and sixteen columns after it; scanEngineJob scans them by position. A change to jobColumns that breaks either is caught here, not as a
// scan error on a database nobody has at hand.
func TestEngineJobColumns_DropTheCampaignAndKeepTheOrder(t *testing.T) {
	base := topLevelColumns(jobColumns)
	if base != 19 {
		t.Fatalf("jobColumns has %d columns, scanJobWith scans 19: update both together", base)
	}
	if got := topLevelColumns(engineJobColumns); got != base+16 {
		t.Errorf("engineJobColumns has %d columns, want %d (jobColumns plus the sixteen the engine adds)", got, base+16)
	}
	// the module's own handle of the job is read with the job (COALESCEd: a job with none has "" and not a NULL the scan cannot take), the database's own clock follows it (the engine measures staleness against it) and
	// the three annotations of the schedule (next_poll_at, idle_polls, items_ensured_at) are the LAST columns scanEngineJob scans
	if !strings.Contains(engineJobColumns, "COALESCE(j.module_job_ref, ''), now(),") || !strings.HasSuffix(strings.TrimSpace(engineJobColumns), "j.next_poll_at, j.idle_polls, j.items_ensured_at") {
		t.Errorf("engineJobColumns does not hold the module reference, the database's clock and then the three annotations of the schedule: %s", engineJobColumns)
	}
	if !strings.Contains(engineJobColumns, "(j.frozen_settings - 'content')") || strings.Contains(engineJobColumns, "j.frozen_settings,") {
		t.Errorf("engineJobColumns must select the frozen settings without the campaign: %s", engineJobColumns)
	}
	// the control: the full list does select the campaign (it is the one EnsureModuleItems reads through getJobByID)
	if !strings.Contains(jobColumns, "j.frozen_settings,") {
		t.Error("jobColumns no longer selects the frozen settings: the replacement in engineJobColumns matched nothing")
	}
	for _, name := range []string{"n_rows", "n_pending", "n_submitted", "n_results", "n_pending_results", "n_unsubmitted"} {
		if !strings.Contains(engineJobColumns, "c."+name) || !strings.Contains(engineJobFrom, " AS "+name) {
			t.Errorf("the count %s is selected and computed in different places", name)
		}
	}
}

// NotePoll refuses what cannot be a backoff before a database is asked (a store with no pool would panic if the call got further): the database's own wall is the 600 seconds MaxPollBackoff says, and a
// ceiling under the base would make the wait shrink while the job stays idle. The control is a backoff that is allowed: it passes the checks and only then meets the missing pool.
func TestNotePoll_RefusesWhatCannotBeABackoffBeforeAnyDatabaseIsAsked(t *testing.T) {
	s := &Store{}
	for name, c := range map[string]struct{ base, ceiling time.Duration }{
		"a negative base":                {-time.Second, time.Second},
		"a ceiling under the base":       {2 * time.Second, time.Second},
		"a ceiling past the DB's wall":   {time.Second, MaxPollBackoff + time.Nanosecond},
		"a base past the wall as well":   {MaxPollBackoff + time.Second, MaxPollBackoff + time.Second},
		"a negative ceiling with a base": {0, -time.Second},
	} {
		if _, err := s.NotePoll(context.Background(), idA, StatusRunning, false, c.base, c.ceiling); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: NotePoll = %v, want ErrBadRequest", name, err)
		}
	}
	for _, id := range []string{"", "not-a-uuid", "00000000-0000-4000-8000-00000000000"} {
		if _, err := s.NotePoll(context.Background(), id, StatusRunning, true, time.Second, time.Minute); !errors.Is(err, ErrNotFound) {
			t.Errorf("NotePoll(%q) = %v, want ErrNotFound", id, err)
		}
	}
	// the control: the largest backoff the store takes passes the checks (the call then needs its pool: it panics on the empty store, which is the proof that the checks were passed)
	defer func() {
		if recover() == nil {
			t.Error("a ceiling of exactly MaxPollBackoff was refused before the database was asked")
		}
	}()
	_, _ = s.NotePoll(context.Background(), idA, StatusRunning, false, 0, MaxPollBackoff)
}

// The backoff the engine may ask for and the wall the database keeps are one number, and the statements that write the three schedule columns say what the migration says.
func TestSchedule_TheWallOfTheDatabaseIsTheCeilingOfTheStoreAndTheStatementsNameTheColumns(t *testing.T) {
	if MaxPollBackoff != 600*time.Second {
		t.Errorf("MaxPollBackoff = %s: platform_0015's trigger refuses a wait of more than 600 seconds, PLATFORM_JOB_BACKOFF_CAP_SEC takes at most 600: change the three together", MaxPollBackoff)
	}
	for _, col := range []string{"j.next_poll_at", "j.idle_polls", "j.items_ensured_at"} {
		if !strings.Contains(engineJobColumns, col) {
			t.Errorf("engineJobColumns does not select %s", col)
		}
	}
}
