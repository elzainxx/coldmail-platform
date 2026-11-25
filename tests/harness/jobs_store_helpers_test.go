//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// Helpers of the job-store tests (jobs_store_test.go). Every name starts with `jbs` so that it cannot clash with the helpers of the other files of this package.
//
// The shape every test shares: a migrated scratch database of its own (newMnyEnv), a SUPERUSER connection to it (e.admin: seeding and observing, never the code under test) and the job store
// under test (e.jobs) over a pool of REAL logins as platform_app, with the money store it calls. Fixtures are committed for real: the store runs on other connections and could not see an
// open transaction. The raw INSERT and UPDATE statements below are TESTS (scripts/check-job-writers.sh reads no test file): they are how a test shows what the store does against what the
// database holds, and how it moves the clock or a price, which no application role can.

type jbsEnv struct {
	*mnyEnv
	jobs  *jobs.Store
	users atomic.Int64 // the next fixture user
}

// newJbsEnv migrates a scratch database and builds the job store over it with the options.
func newJbsEnv(t *testing.T, opts ...jobs.Option) *jbsEnv {
	t.Helper()
	e := &jbsEnv{mnyEnv: newMnyEnv(t)}
	// the tests that hold the (user, module) lock and wait for two confirms to queue behind it need the store's wait to outlast the polling, whatever the runner's speed
	e.store = money.New(e.pool, money.WithLockTimeout(45*time.Second))
	st, err := jobs.New(e.pool, e.store, opts...)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	e.jobs = st
	e.vendorOff()
	return e
}

// vendorOff makes every jobs-store test start in the world the platform ships in: module 1's vendor unit m1_vendor NOT reachable, so a verify job reserves at 1 per row. The shared fixtures (newMnyEnv,
// written for the Phase 2 tests) switch the vendor on, which would price every m1 job here at 2. The flag found is read first and restored when the test ends; a test that wants the vendor on sets it
// explicitly (and turns it off again).
func (e *jbsEnv) vendorOff() {
	t := e.t
	t.Helper()
	was := e.boolQ("SELECT reachable FROM platform.unit_costs WHERE unit = 'm1_vendor'")
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', false, 'test: the jobs store tests start with the vendor off')")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := e.admin.Exec(ctx, "SELECT platform.set_unit_reachable('m1_vendor', $1, 'test: the flag the jobs store test found')", was); err != nil {
			t.Errorf("cannot restore the vendor flag: %v", err)
		}
	})
}

// jbsView is what two answers of one confirm must share, as one comparable string: the job, its status and key, the fingerprint it was confirmed on, and the numbers. It leaves Replayed out (the
// one thing that differs) and the times (a replay is read later).
func jbsView(r jobs.ConfirmResult) string {
	return fmt.Sprintf("job=%s status=%s key=%s fp=%s reserved=%d pending=%d suppressed=%d", r.Job.ID, r.Job.Status, r.Job.ConfirmKey, r.Job.PreviewFingerprint, r.Reserved, r.Pending, r.Suppressed)
}

// at returns the same environment reporting to t (a subtest must fail ITS OWN T, never its parent's).
func (e *jbsEnv) at(t *testing.T) *jbsEnv {
	c := &jbsEnv{mnyEnv: e.mnyEnv.at(t), jobs: e.jobs}
	c.users.Store(e.users.Load())
	return c
}

var jbsKeySeq atomic.Int64

// key is a fresh, valid confirm key.
func (e *jbsEnv) key() string { return fmt.Sprintf("jbs-key-%d", jbsKeySeq.Add(1)) }

// newUser inserts a fixture customer and returns the id. Each test that needs a job in the slot of its own takes a user of its own.
func (e *jbsEnv) newUser() string {
	e.t.Helper()
	n := e.users.Add(1)
	id := fmt.Sprintf("d0000000-0000-4000-8000-%012d", n)
	e.exec("INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, fmt.Sprintf("jbs-%d@example.test", n))
	e.exec("INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", id, fmt.Sprintf("jbs-%d@example.test", n))
	return id
}

// jbsAddrs makes n distinct, valid addresses: <tag>00001@example.test, ...
func jbsAddrs(n int, tag string) []jobs.RowIn {
	out := make([]jobs.RowIn, n)
	for i := range out {
		out[i] = jobs.RowIn{Email: fmt.Sprintf("%s%05d@example.test", tag, i+1)}
	}
	return out
}

// jbsContent is a valid campaign.
func jbsContent() *jobs.Content {
	return &jobs.Content{Subject: "Hello from the harness", HTML: "<p>hello</p>", Text: "hello", ReplyTo: "reply@example.test"}
}

// create makes a draft of n distinct addresses on the module (with content for a send module) and fails the test on an error.
func (e *jbsEnv) create(user, module string, n int) jobs.Job {
	e.t.Helper()
	return e.createRows(user, module, jbsAddrs(n, "r"))
}

func (e *jbsEnv) createRows(user, module string, rows []jobs.RowIn) jobs.Job {
	e.t.Helper()
	req := jobs.CreateReq{Module: module, Rows: rows}
	if module != "m1" {
		req.Content = jbsContent()
	}
	j, err := e.jobs.CreateDraft(testCtx(e.t), user, req)
	if err != nil {
		e.t.Fatalf("CreateDraft(%s, %d rows): %v", module, len(rows), err)
	}
	return j
}

// preview previews and fails the test on an error.
func (e *jbsEnv) preview(user, job string) jobs.Preview {
	e.t.Helper()
	p, err := e.jobs.Preview(testCtx(e.t), user, job)
	if err != nil {
		e.t.Fatalf("Preview(%s): %v", job, err)
	}
	return p
}

// previewed creates a job and previews it.
func (e *jbsEnv) previewed(user, module string, n int) (jobs.Job, jobs.Preview) {
	e.t.Helper()
	j := e.create(user, module, n)
	return j, e.preview(user, j.ID)
}

// confirm confirms with a fresh key and fails the test on an error.
func (e *jbsEnv) confirm(user, job, fingerprint string) jobs.ConfirmResult {
	e.t.Helper()
	r, err := e.tryConfirm(user, job, fingerprint, e.key(), nil)
	if err != nil {
		e.t.Fatalf("Confirm(%s): %v", job, err)
	}
	return r
}

// tryConfirm is Confirm with everything stated.
func (e *jbsEnv) tryConfirm(user, job, fingerprint, key string, start *time.Time) (jobs.ConfirmResult, error) {
	e.t.Helper()
	return e.jobs.Confirm(testCtx(e.t), user, jobs.ConfirmReq{JobID: job, Fingerprint: fingerprint, Key: key, ScheduledStart: start})
}

// confirmed creates, previews and confirms a job of n rows for a user who is funded with enough credits (plenty) and returns the preview and the confirm.
func (e *jbsEnv) confirmed(user, module string, n int) (jobs.Job, jobs.ConfirmResult) {
	e.t.Helper()
	if module != "m3" {
		e.fund(user, module, 1000)
	} else {
		e.enableM3(user, false)
	}
	j, p := e.previewed(user, module, n)
	return j, e.confirm(user, j.ID, p.Fingerprint)
}

// suppressFor adds the addresses to the user's suppression list through the one sanctioned writer.
func (e *jbsEnv) suppressFor(user string, emails ...string) {
	e.t.Helper()
	for _, a := range emails {
		if _, err := suppress.Add(testCtx(e.t), e.pool, user, a, suppress.SourceManual); err != nil {
			e.t.Fatalf("suppress.Add: %v", err)
		}
	}
}

// enableM3 enables module 3 for the user through the ledger (the only writer of the entitlement); expiring makes the entitlement due one hour ago, which is how time passes for it.
func (e *jbsEnv) enableM3(user string, expired bool) {
	e.t.Helper()
	e.exec(`INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key, until_at)
		VALUES ($1, 'm3', 'm3_enable', 0, 0, $2, 'test entitlement', $3, now() + interval '1 hour')`, user, mnyAdminA, e.key())
	if expired {
		e.exec("UPDATE platform.m3_entitlements SET enabled_until = now() - interval '1 hour' WHERE user_id = $1", user)
	}
}

// ---- observing the job tables ----

// jbsRow is a row of platform.job_rows as the database holds it.
type jbsRow struct {
	ID, Raw, Norm, State, Reason, DupOf string
	Ordinal                             int
}

func (e *jbsEnv) rowsOf(job string) []jbsRow {
	e.t.Helper()
	rs, err := e.admin.Query(testCtx(e.t), `SELECT id::text, ordinal, email_raw, COALESCE(email_norm, ''), state, COALESCE(reason, ''), COALESCE(duplicate_of::text, '')
		FROM platform.job_rows WHERE job_id = $1 ORDER BY ordinal`, job)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []jbsRow
	for rs.Next() {
		var r jbsRow
		var ord int32
		if err := rs.Scan(&r.ID, &ord, &r.Raw, &r.Norm, &r.State, &r.Reason, &r.DupOf); err != nil {
			e.t.Fatal(err)
		}
		r.Ordinal = int(ord)
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// stateCounts is the number of the job's rows in each state.
func (e *jbsEnv) stateCounts(job string) map[string]int {
	e.t.Helper()
	out := map[string]int{}
	for _, r := range e.rows("SELECT state, count(*) FROM platform.job_rows WHERE job_id = $1 GROUP BY state", job) {
		var st string
		var n int
		if _, err := fmt.Sscanf(strings.Replace(r, "|", " ", 1), "%s %d", &st, &n); err != nil {
			e.t.Fatalf("cannot read %q: %v", r, err)
		}
		out[st] = n
	}
	return out
}

// ruleCounts is the number of the job's results under each rule label.
func (e *jbsEnv) ruleCounts(job string) map[string]int {
	e.t.Helper()
	out := map[string]int{}
	for _, r := range e.rows("SELECT rule, count(*) FROM platform.job_results WHERE job_id = $1 GROUP BY rule", job) {
		var rule string
		var n int
		if _, err := fmt.Sscanf(strings.Replace(r, "|", " ", 1), "%s %d", &rule, &n); err != nil {
			e.t.Fatalf("cannot read %q: %v", r, err)
		}
		out[rule] = n
	}
	return out
}

// eventLines are the job's events, oldest first, as "kind|from>to|actor kind|acting user|reason".
func (e *jbsEnv) eventLines(job string) []string {
	e.t.Helper()
	return e.rows(`SELECT kind || '|' || COALESCE(from_status, '') || '>' || COALESCE(to_status, '') || '|' || actor_kind || '|' || COALESCE(acting_user_id::text, '') || '|' || reason
		FROM platform.job_events WHERE job_id = $1 ORDER BY id`, job)
}

// reserveRows is the number of reserve rows the ledger holds for the job.
func (e *jbsEnv) reserveRows(job string) int64 {
	e.t.Helper()
	return e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", job)
}

// writes is a fingerprint of everything a job operation could write: the money state (ledger, price snapshots, audit, balances, reserves) and the job tables (the statuses and keys of every
// job, the state of every row, the number of results and events). Two equal values mean that nothing was written between them.
func (e *jbsEnv) writes() string {
	e.t.Helper()
	return e.state() + "/" + e.str(`SELECT (SELECT count(*) FROM platform.job_events)::text || ':' || (SELECT count(*) FROM platform.job_results)::text
		|| ':' || COALESCE((SELECT md5(string_agg(id::text || status || COALESCE(confirm_key, '') || COALESCE(preview_fingerprint, ''), ',' ORDER BY id)) FROM platform.jobs), '-')
		|| ':' || COALESCE((SELECT md5(string_agg(id::text || state, ',' ORDER BY id)) FROM platform.job_rows), '-')`)
}

func (e *jbsEnv) requireWrites(what, before string) {
	e.t.Helper()
	if after := e.writes(); after != before {
		e.t.Fatalf("%s: the database changed although nothing should have been written (%s -> %s)", what, before, after)
	}
}

// requireBooks fails when the balance invariant, the per-reserve invariant or the settlement proof (ledger, results and rows agree) is broken.
func (e *jbsEnv) requireBooks(when string) {
	e.t.Helper()
	e.requireViewsEmpty(when)
	if d := e.rows(`SELECT job_id::text, COALESCE(item_ref, '-'), problem FROM platform.job_settlement_drift ORDER BY 1, 2, 3`); len(d) != 0 {
		e.t.Fatalf("%s: platform.job_settlement_drift is not empty (job|item|problem): %v", when, d)
	}
}

// ---- asserting the store's answers ----

func jbsIs(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got error %v, want errors.Is(%v)", what, err, want)
	}
}

// jbsNotAllowed asserts the error is ErrNotAllowed with exactly this reason (the database's constraint name, or the store's word).
func jbsNotAllowed(t *testing.T, what string, err error, reason string) {
	t.Helper()
	var na *jobs.NotAllowedError
	if !errors.Is(err, jobs.ErrNotAllowed) || !errors.As(err, &na) || na.Reason != reason {
		t.Fatalf("%s: got %v, want ErrNotAllowed with reason %q", what, err, reason)
	}
}

// jbsBadRequest asserts the error is ErrBadRequest whose reason contains the text, and that it quotes no address.
func jbsBadRequest(t *testing.T, what string, err error, contains string) {
	t.Helper()
	var br *jobs.BadRequestError
	if !errors.Is(err, jobs.ErrBadRequest) || !errors.As(err, &br) || !strings.Contains(br.Reason, contains) {
		t.Fatalf("%s: got %v, want ErrBadRequest naming %q", what, err, contains)
	}
	if strings.Contains(err.Error(), "@") {
		t.Fatalf("%s: the refusal quotes an address: %v", what, err)
	}
}

// ---- the module 3 pre-check, faked ----

type jbsCheckCall struct {
	User, Campaign string
	Recipients     int
}

// jbsFake is a jobs.PreChecks that records its calls and answers what the test says; probe runs inside the call (the test uses it to prove that no lock is held while the module is asked).
type jbsFake struct {
	mu     sync.Mutex
	calls  []jbsCheckCall
	answer jobs.Feasibility
	err    error
	probe  func()
}

func (f *jbsFake) M3Feasibility(ctx context.Context, userID, campaignID string, recipients int) (jobs.Feasibility, error) {
	f.mu.Lock()
	f.calls = append(f.calls, jbsCheckCall{userID, campaignID, recipients})
	probe, ans, err := f.probe, f.answer, f.err
	f.mu.Unlock()
	if probe != nil {
		probe()
	}
	return ans, err
}

func (f *jbsFake) set(a jobs.Feasibility, err error) {
	f.mu.Lock()
	f.answer, f.err = a, err
	f.mu.Unlock()
}

func (f *jbsFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---- the forced interleave of two confirms ----

// jbsOutcome is what one Confirm returned.
type jbsOutcome struct {
	Res jobs.ConfirmResult
	Err error
}

// jbsCall is one confirm of an interleave.
type jbsCall struct{ Job, Fingerprint, Key string }

// interleaveConfirms starts two confirms for the SAME (user, module) while the test holds the advisory lock the store takes, PROVES from the server that both backends are blocked on it (two
// distinct platform_app backends, wait_event "advisory", blocked by the holder, whose current statement is the store's pg_advisory_xact_lock) and that neither call has returned, then lets go and
// returns what both answered. It fails the test when the proof does not hold: a test that hopes for contention proves nothing.
func (e *jbsEnv) interleaveConfirms(user, module string, calls [2]jbsCall) [2]jbsOutcome {
	t := e.t
	t.Helper()
	h := e.comHold(user, module)
	var chans [2]chan jbsOutcome
	for i, c := range calls {
		ch := make(chan jbsOutcome, 1)
		chans[i] = ch
		c := c
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			r, err := e.jobs.Confirm(ctx, user, jobs.ConfirmReq{JobID: c.Job, Fingerprint: c.Fingerprint, Key: c.Key})
			ch <- jbsOutcome{r, err}
		}()
	}
	ws, ok := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		var adv []mnyWait
		for _, w := range ws {
			if w.Event == "advisory" {
				adv = append(adv, w)
			}
		}
		if len(adv) != 2 || adv[0].PID == adv[1].PID {
			return false
		}
		for _, w := range adv {
			if !mnyHasPID(w.Blockers, h.pid) || !strings.Contains(w.Query, "pg_advisory_xact_lock") {
				return false
			}
		}
		return true
	})
	if !ok {
		h.release()
		t.Fatalf("the two confirms were never both seen blocked on the (user, module) advisory lock behind the test's holder: %s", mnyDescribe(ws))
	}
	if len(chans[0])+len(chans[1]) != 0 {
		h.release()
		t.Fatalf("a confirm came back while the test still held the lock the store takes first: the store did not wait for it")
	}
	h.requireStillHeld("while both confirms were blocked")
	h.release()
	var out [2]jbsOutcome
	for i, ch := range chans {
		select {
		case out[i] = <-ch:
		case <-time.After(120 * time.Second):
			t.Fatalf("confirm %d did not return within 120s after the test let go", i+1)
		}
	}
	return out
}

// ---- the oracle of the classification ----

// jbsOracleRow is one input line with what the platform's address rule must make of it, written by hand from the rule's words (internal/norm: trim, split at the one '@', IDNA of the domain, then
// the trailing dots, NFC and lower case of the local part; module 1's syntax layer: local at most 64 bytes, domain at most 255, a dot in the domain), never read from the code under test.
type jbsOracleRow struct {
	in     string
	state  string
	norm   string // "" for a malformed row
	reason string
	dupOf  int // the ORDINAL of the row it repeats (0: none)
}

func jbsOracle() []jbsOracleRow {
	long64 := strings.Repeat("a", 64)
	return []jbsOracleRow{
		/* 1  */ {"alice@example.test", jobs.StatePending, "alice@example.test", "", 0},
		/* 2  */ {"  Bob@Example.TEST  ", jobs.StatePending, "bob@example.test", "", 0}, // padded, mixed case
		/* 3  */ {"ALICE@example.test", jobs.StateDuplicate, "alice@example.test", "", 1}, // repeated in another case
		/* 4  */ {"bob@example.test", jobs.StateDuplicate, "bob@example.test", "", 2}, // the padded one, typed plain
		/* 5  */ {"", jobs.StateMalformed, "", "empty", 0},
		/* 6  */ {"   \t ", jobs.StateMalformed, "", "empty", 0},
		/* 7  */ {"no-at-sign.example.test", jobs.StateMalformed, "", "no_at", 0},
		/* 8  */ {"a@b@c.test", jobs.StateMalformed, "", "multiple_at", 0},
		/* 9  */ {"@example.test", jobs.StateMalformed, "", "empty_local", 0},
		/* 10 */ {"a@", jobs.StateMalformed, "", "empty_domain", 0},
		/* 11 */ {"user@localhost", jobs.StateMalformed, "", "domain_no_dot", 0}, // norm/v1 accepts a one-label domain, module 1's syntax layer does not
		/* 12 */ {strings.Repeat("a", 65) + "@example.test", jobs.StateMalformed, "", "local_too_long", 0},
		/* 13 */ {long64 + "@example.test", jobs.StatePending, long64 + "@example.test", "", 0}, // the twin: 64 bytes is still fine
		/* 14 */ {"müller@bücher.example", jobs.StatePending, "müller@xn--bcher-kva.example", "", 0}, // IDN: the domain becomes punycode
		/* 15 */ {"MÜLLER@BÜCHER.EXAMPLE", jobs.StateDuplicate, "müller@xn--bcher-kva.example", "", 14}, // repeated after normalising
		/* 16 */ {"carol@example.test.", jobs.StatePending, "carol@example.test", "", 0}, // the trailing dot goes
		/* 17 */ {"Carol@EXAMPLE.test", jobs.StateDuplicate, "carol@example.test", "", 16}, // repeated only AFTER normalising
		/* 18 */ {"dave@exa mple.test", jobs.StateMalformed, "", "bad_idna", 0},
		/* 19 */ {"erin@example.test\x00", jobs.StateMalformed, "", jobs.ReasonInvalidText, 0}, // a NUL: PostgreSQL text cannot hold it
		/* 20 */ {"frank@" + strings.Repeat("a", 256) + ".test", jobs.StateMalformed, "", "domain_too_long", 0},
		/* 21 */ {strings.Repeat("a", 5000) + "@example.test", jobs.StateMalformed, "", jobs.ReasonTooLong, 0}, // more than job_rows keeps
		/* 22 */ {"bad\xffbyte@example.test", jobs.StateMalformed, "", jobs.ReasonInvalidText, 0}, // not UTF-8
		/* 23 */ {"alice@Example.Test", jobs.StateDuplicate, "alice@example.test", "", 1}, // a THIRD occurrence names the FIRST row
		/* 24 */ {"gina@example.test", jobs.StatePending, "gina@example.test", "", 0},
	}
}

// jbsCompareOracle compares what the database holds with the oracle, row by row, and returns every difference (none: they agree).
func jbsCompareOracle(want []jbsOracleRow, got []jbsRow) []string {
	var p []string
	if len(got) != len(want) {
		return []string{fmt.Sprintf("%d rows in the database, %d lines in the input", len(got), len(want))}
	}
	ordOf := map[string]int{}
	for _, g := range got {
		ordOf[g.ID] = g.Ordinal
	}
	for i, w := range want {
		g := got[i]
		if g.Ordinal != i+1 {
			p = append(p, fmt.Sprintf("row %d has ordinal %d", i+1, g.Ordinal))
		}
		if g.State != w.state || g.Norm != w.norm || g.Reason != w.reason {
			p = append(p, fmt.Sprintf("row %d: state %q norm %q reason %q, want %q %q %q", i+1, g.State, g.Norm, g.Reason, w.state, w.norm, w.reason))
		}
		if dup := ordOf[g.DupOf]; dup != w.dupOf {
			p = append(p, fmt.Sprintf("row %d: duplicate_of is row %d, want row %d", i+1, dup, w.dupOf))
		}
	}
	return p
}
