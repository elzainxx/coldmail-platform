//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
)

// Module 1's vendor reachability in the job store (Phase 4, owner ruling OD-7, row C3; internal/jobs/vendor.go) against a REAL PostgreSQL. The store reads module 1's describe through a
// jobs.VendorReachability BEFORE the transaction of a module 1 preview and confirm; here that provider is a FAKE that says what the test tells it (module 1 itself is the harness scope jobs' stage J6):
//
//	APreviewMakesTheFlagModuleOnesAnswerAndTheRateFollows        off: 1 per row, on: 2, off again: 1; the flag, its history rows (source m1_describe) and the stored preview; a job that is not a module 1 job asks nothing
//	AConfirmWhoseFingerprintWasComputedUnderTheOtherAnswerIsStale the preview was made under off, module 1 says on at the confirm: ErrStalePreview and nothing held; preview again: 2 per row and it reserves 2; a later off changes no job
//	AnUnreadableDescribeKeepsTheStoredFlagAndRefusesOnlyTheSeededOff   24 cases: 6 stored states x 2 ways of not being read x immediate and scheduled
//	ModuleOneIsAskedBeforeTheTransactionNeverUnderTheLockAndOnlyForTheUsersModuleOneJob   the lock is free while module 1 is asked; the asking happens while a confirm waits for the lock; a replay, a foreign job and a module 2 job ask nothing
//	WithNoProviderNothingIsReadAndNothingIsRefused               the default store: the seeded off flag does not refuse an immediate confirm
//	(and, in money's own terms) TestMoneyReachable_UnitReachabilityReadsTheFlagWithItsSource

// vendorFake is a jobs.VendorReachability that says what the test tells it and counts its calls; probe runs inside a call (the test uses it to look at the database while module 1 is being asked).
type vendorFake struct {
	mu        sync.Mutex
	reachable bool
	known     bool
	err       error
	calls     int
	probe     func()
}

var _ jobs.VendorReachability = (*vendorFake)(nil)

func (f *vendorFake) M1VendorReachable(context.Context) (bool, bool, error) {
	f.mu.Lock()
	f.calls++
	probe, reach, known, err := f.probe, f.reachable, f.known, f.err
	f.mu.Unlock()
	if probe != nil {
		probe()
	}
	return reach, known, err
}

// says: module 1 answers, with this flag.
func (f *vendorFake) says(reachable bool) {
	f.mu.Lock()
	f.reachable, f.known, f.err = reachable, true, nil
	f.mu.Unlock()
}

// cannotBeAsked: module 1 is down (an error; the flag it carries must be ignored).
func (f *vendorFake) cannotBeAsked() {
	f.mu.Lock()
	f.reachable, f.known, f.err = true, true, errors.New("module 1 cannot be asked (a fake's text, never logged)")
	f.mu.Unlock()
}

// doesNotSay: module 1 answers and its describe has no vendor_fallback (an older module 1).
func (f *vendorFake) doesNotSay() {
	f.mu.Lock()
	f.reachable, f.known, f.err = true, false, nil
	f.mu.Unlock()
}

func (f *vendorFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newVendorEnv is a job-store environment whose store reads the fake and remembers nothing (a memory of 0: every preview and confirm asks), and whose flag starts false.
func newVendorEnv(t *testing.T, f *vendorFake, opts ...jobs.Option) *jbsEnv {
	t.Helper()
	return newJbsEnv(t, append([]jobs.Option{jobs.WithVendorReachability(f), jobs.WithVendorReachabilityTTL(0)}, opts...)...)
}

// vendorFlag is the stored flag of m1_vendor and who said so, as one string.
func (e *jbsEnv) vendorFlag() string {
	e.t.Helper()
	return e.str(`SELECT reachable::text || '|' || COALESCE(reachable_source, '') FROM platform.unit_costs WHERE unit = 'm1_vendor'`)
}

// vendorHistory is the rows of the flag's history, oldest first: "reachable|source".
func (e *jbsEnv) vendorHistory() []string {
	e.t.Helper()
	return e.rows(`SELECT reachable::text || '|' || source FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' ORDER BY id`)
}

// setVendorFlag puts the stored flag in the state the test needs, with the source the test needs: a flag that already has the value is first made the other way, because the writer writes nothing (and
// so never changes a source) for a flag that already says so. It does it through the one function that may, as the superuser; the history keeps the throwaway row.
func (e *jbsEnv) setVendorFlag(reachable bool, source string) {
	e.t.Helper()
	if e.boolQ("SELECT reachable FROM platform.unit_costs WHERE unit = 'm1_vendor'") == reachable {
		e.exec("SELECT platform.set_unit_reachable('m1_vendor', $1, 'test: made to differ first')", !reachable)
	}
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', $1, $2)", reachable, source)
	if got, want := e.vendorFlag(), fmt.Sprintf("%t|%s", reachable, source); got != want {
		e.t.Fatalf("the flag is %q, want %q", got, want)
	}
}

// the seed of platform_0013 and the word an earlier build wrote from its own configuration (spelled in pieces: no file may name the removed variable)
const (
	vendorSeedSource    = "seed: the vendor fallback ships disabled"
	vendorEarlierSource = "config: PLATFORM_M1_" + "VENDOR_REACHABLE"
)

func TestJobsStore_VendorReachabilityFromTheModule_APreviewMakesTheFlagModuleOnesAnswerAndTheRateFollows(t *testing.T) {
	f := &vendorFake{}
	e := newVendorEnv(t, f)
	u := e.newUser()
	e.fund(u, "m1", 1000)
	j := e.create(u, "m1", 10)
	h0 := e.vendorHistory()

	// module 1 says off: 1 per row, the flag already says so, so nothing is written (not even a history row)
	f.says(false)
	p := e.preview(u, j.ID)
	if p.UnitCost != 1 || p.RateUnit == money.UnitM1Vendor || p.Credits != 10 || p.Units != 10 || !p.VendorReachableKnown {
		t.Fatalf("module 1 says off: preview %+v, want rate 1 (not m1_vendor), 10 credits and vendor_reachable_known", p)
	}
	if h := e.vendorHistory(); len(h) != len(h0) {
		t.Fatalf("a flag that already said off was written again: history %v -> %v", h0, h)
	}
	if got := e.str("SELECT preview->>'vendor_reachable_known' FROM platform.jobs WHERE id = $1", j.ID); got != "true" {
		t.Errorf("the stored preview says vendor_reachable_known = %q, want true: the stored document and the answer must agree", got)
	}

	// module 1 says on: the very next preview shows 2 per row and the flag is module 1's word, audited with its source
	f.says(true)
	p2 := e.preview(u, j.ID)
	if p2.UnitCost != 2 || p2.RateUnit != money.UnitM1Vendor || p2.Credits != 20 || !p2.VendorReachableKnown || p2.Fingerprint == p.Fingerprint {
		t.Fatalf("module 1 says on: preview %+v, want rate 2 at m1_vendor, 20 credits, known and a new fingerprint (%s)", p2, p.Fingerprint)
	}
	if got := e.vendorFlag(); got != "true|m1_describe" {
		t.Errorf("the flag is %q, want true set by m1_describe", got)
	}
	if h := e.vendorHistory(); len(h) != len(h0)+1 || h[len(h)-1] != "true|m1_describe" {
		t.Errorf("the history is %v after one flip, want one more row, true|m1_describe", h)
	}
	// the same answer again writes nothing more (idempotent: no history row when nothing changes)
	e.preview(u, j.ID)
	if h := e.vendorHistory(); len(h) != len(h0)+1 {
		t.Errorf("a second preview under the same answer wrote history: %v", h)
	}

	// and off again: 1 per row, and the fingerprint of the same list under the same prices is the first one again
	f.says(false)
	p3 := e.preview(u, j.ID)
	if p3.UnitCost != 1 || p3.RateUnit == money.UnitM1Vendor || p3.Credits != 10 || p3.Fingerprint != p.Fingerprint {
		t.Errorf("module 1 says off again: preview %+v, want rate 1 and the first fingerprint (%s)", p3, p.Fingerprint)
	}
	if h := e.vendorHistory(); len(h) != len(h0)+2 || h[len(h)-1] != "false|m1_describe" {
		t.Errorf("the history is %v after two flips, want two more rows ending false|m1_describe", h)
	}

	// control: a job that is not a module 1 job asks module 1 nothing and says nothing about it
	n := f.count()
	u2 := e.newUser()
	e.fund(u2, "m2", 100)
	_, pm := e.previewed(u2, "m2", 3)
	if f.count() != n || pm.VendorReachableKnown {
		t.Errorf("a module 2 preview asked module 1 (%d -> %d calls) or says module 1 was read (%v)", n, f.count(), pm.VendorReachableKnown)
	}
	e.requireBooks("after the previews")
}

func TestJobsStore_VendorReachabilityFromTheModule_AConfirmWhoseFingerprintWasComputedUnderTheOtherAnswerIsStale(t *testing.T) {
	f := &vendorFake{}
	e := newVendorEnv(t, f)
	f.says(false)
	A := e.newUser()
	e.fund(A, "m1", 1000)
	j, p := e.previewed(A, "m1", 10) // under off: 1 per row

	// control first: nothing changes between the preview and the confirm, and the confirm is accepted at 1 per row
	C := e.newUser()
	e.fund(C, "m1", 1000)
	jc, pc := e.previewed(C, "m1", 10)
	if r := e.confirm(C, jc.ID, pc.Fingerprint); r.Reserved != 10 {
		t.Fatalf("control: an unchanged answer reserved %d credits, want 10", r.Reserved)
	}

	// module 1's vendor is switched on AFTER A's preview: the confirm reads it, the flag follows, the fingerprint of the list under the new prices is not the one A saw
	f.says(true)
	w := e.writes()
	_, err := e.tryConfirm(A, j.ID, p.Fingerprint, e.key(), nil)
	jbsIs(t, "a confirm of a preview made under the other answer", err, jobs.ErrStalePreview)
	if got := e.vendorFlag(); got != "true|m1_describe" {
		t.Errorf("the refused confirm did not make the flag module 1's answer: %q", got)
	}
	if e.reserveRows(j.ID) != 0 {
		t.Error("a stale confirm left a reserve")
	}
	if a, r, _ := e.bal(A, "m1"); a != 1000 || r != 0 {
		t.Errorf("a stale confirm moved A's balance to %d available and %d reserved, want 1000 and 0", a, r)
	}
	if st := e.jobStatus(j.ID); st != "previewed" {
		t.Errorf("the refused job is %s, want previewed", st)
	}
	// the only thing the refused confirm may leave is the flag and its history row: the books and the job tables are as they were
	if after := e.writes(); after != w {
		t.Errorf("a stale confirm wrote more than the flag:\n  before %s\n  after  %s", w, after)
	}

	// preview again: 2 per row, a new fingerprint; the confirm reserves 2 per row, frozen into the price snapshot
	p2 := e.preview(A, j.ID)
	if p2.UnitCost != 2 || p2.RateUnit != money.UnitM1Vendor || p2.Credits != 20 || p2.Fingerprint == p.Fingerprint {
		t.Fatalf("the new preview %+v, want rate 2 at m1_vendor, 20 credits and a new fingerprint", p2)
	}
	r := e.confirm(A, j.ID, p2.Fingerprint)
	if r.Reserved != 20 || e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j.ID) != 2 {
		t.Fatalf("the confirm reserved %d credits at %d each, want 20 at 2", r.Reserved, e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j.ID))
	}
	if got := e.frozen(j.ID); got[money.UnitM1Vendor] != 2 || len(got) != 3 {
		t.Errorf("the price snapshot is %v, want the three units of module 1 with m1_vendor at 2", got)
	}
	// C was confirmed under off, before the flip: its reserve is what it was
	if got := e.frozen(jc.ID); len(got) != 2 || got[money.UnitM1Vendor] != 0 {
		t.Errorf("C's price snapshot is %v, want the two units reachable when it was confirmed", got)
	}

	// a later off changes no job: A keeps its reserve and its snapshot, a new preview is at 1 per row again
	f.says(false)
	B := e.newUser()
	e.fund(B, "m1", 1000)
	_, pb := e.previewed(B, "m1", 10)
	if pb.UnitCost != 1 || pb.Credits != 10 || !pb.VendorReachableKnown {
		t.Errorf("a preview after module 1 went off again is %+v, want 1 per row", pb)
	}
	if got := e.frozen(j.ID); got[money.UnitM1Vendor] != 2 || e.int("SELECT reserved_delta FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", j.ID) != 20 {
		t.Errorf("a flip of module 1's answer changed a confirmed job: snapshot %v", got)
	}
	e.requireBooks("after the flips")
}

func TestJobsStore_VendorReachabilityFromTheModule_AnUnreadableDescribeKeepsTheStoredFlagAndRefusesOnlyTheSeededOff(t *testing.T) {
	f := &vendorFake{}
	e := newVendorEnv(t, f)

	// the seed's own words are what the table says (so the string the cases below call "the seed" is the real one)
	if first := e.vendorHistory(); len(first) == 0 || !strings.HasSuffix(first[0], "|"+vendorSeedSource) || !strings.HasPrefix(first[0], "false|") {
		t.Fatalf("the first row of the flag's history is %v, want false set by %q", first, vendorSeedSource)
	}

	type state struct {
		name     string
		reach    bool
		source   string
		rate     int  // what a job reserves under the stored flag
		refusing bool // an IMMEDIATE confirm is refused when module 1 cannot be read
	}
	states := []state{
		{"stored true, set by module 1", true, money.ReachableSourceM1Describe, 2, false},
		{"stored true, by the seed's words", true, vendorSeedSource, 2, false},
		{"stored false, set by module 1", false, money.ReachableSourceM1Describe, 1, false},
		{"stored false, the seed", false, vendorSeedSource, 1, true},
		{"stored false, an earlier build's own word", false, vendorEarlierSource, 1, true},
		{"stored false, a test's source", false, "test: by hand", 1, true},
	}
	readings := []struct {
		name string
		set  func()
	}{{"module 1 cannot be asked", f.cannotBeAsked}, {"module 1 does not say", f.doesNotSay}}

	const rows = 4
	cases, refused := 0, 0
	for _, st := range states {
		for _, rd := range readings {
			label := st.name + " / " + rd.name
			e.setVendorFlag(st.reach, st.source)
			rd.set()
			flag0, hist0 := e.vendorFlag(), len(e.vendorHistory())

			// a preview never refuses: it shows the numbers under the stored flag and says they are not fresh
			u := e.newUser()
			e.fund(u, "m1", 1000)
			j := e.create(u, "m1", rows)
			p := e.preview(u, j.ID)
			if p.VendorReachableKnown || p.UnitCost != st.rate || p.Credits != int64(rows*st.rate) {
				t.Fatalf("%s: preview %+v, want vendor_reachable_known false and the stored flag's rate %d", label, p, st.rate)
			}
			if e.str("SELECT preview->>'vendor_reachable_known' FROM platform.jobs WHERE id = $1", j.ID) != "false" {
				t.Errorf("%s: the stored preview does not say it was not read", label)
			}

			// an IMMEDIATE confirm
			w := e.writes()
			res, err := e.tryConfirm(u, j.ID, p.Fingerprint, e.key(), nil)
			switch {
			case st.refusing:
				refused++
				jbsIs(t, label+": an immediate confirm", err, jobs.ErrModuleUnavailable)
				e.requireWrites(label+": the refused confirm", w)
				if e.reserveRows(j.ID) != 0 || e.jobStatus(j.ID) != "previewed" {
					t.Errorf("%s: the refused confirm left a reserve or moved the job (%s)", label, e.jobStatus(j.ID))
				}
				// the same job, SCHEDULED, is accepted: it is not judged against module 1's availability, and reserves at the stored flag's rate
				start := time.Now().Add(2 * time.Hour)
				res, err = e.tryConfirm(u, j.ID, p.Fingerprint, e.key(), &start)
				if err != nil || res.Reserved != int64(rows*st.rate) || res.Job.Status != "scheduled" {
					t.Errorf("%s: the scheduled confirm of the same job answered %+v, %v; want scheduled and %d credits", label, res, err, rows*st.rate)
				}
			default:
				if err != nil || res.Reserved != int64(rows*st.rate) || res.Job.Status != "confirmed" {
					t.Errorf("%s: the immediate confirm answered %+v, %v; want confirmed and %d credits (the stored flag's rate)", label, res, err, rows*st.rate)
				}
				// the scheduled twin, for another user
				u2 := e.newUser()
				e.fund(u2, "m1", 1000)
				j2, p2 := e.previewed(u2, "m1", rows)
				start := time.Now().Add(2 * time.Hour)
				res2, err := e.tryConfirm(u2, j2.ID, p2.Fingerprint, e.key(), &start)
				if err != nil || res2.Reserved != int64(rows*st.rate) || res2.Job.Status != "scheduled" {
					t.Errorf("%s: the scheduled confirm answered %+v, %v; want scheduled and %d credits", label, res2, err, rows*st.rate)
				}
			}
			// an unreadable module 1 writes NOTHING about the flag: the same flag, the same source, no history row
			if got := e.vendorFlag(); got != flag0 {
				t.Errorf("%s: the flag changed from %q to %q although module 1 could not be read", label, flag0, got)
			}
			if h := len(e.vendorHistory()); h != hist0 {
				t.Errorf("%s: %d history row(s) were written although module 1 could not be read", label, h-hist0)
			}
			// the frozen snapshot of what was reserved holds exactly the reachable units of the stored flag
			wantUnits := 2
			if st.reach {
				wantUnits = 3
			}
			if got := e.frozen(j.ID); len(got) != wantUnits && e.reserveRows(j.ID) == 1 {
				t.Errorf("%s: the reserve froze %v, want %d units", label, got, wantUnits)
			}
			cases++
		}
	}
	if cases != 12 || refused != 6 {
		t.Fatalf("the table ran %d case(s) with %d refusing, want 12 and 6: the instrument did not cover what it says", cases, refused)
	}

	// the control that the refusal is about module 1 not being read and nothing else: with module 1 answering, the very state that refuses is accepted, and module 1's word is written
	e.setVendorFlag(false, vendorSeedSource)
	f.says(false)
	u := e.newUser()
	e.fund(u, "m1", 1000)
	j, p := e.previewed(u, "m1", rows)
	if r, err := e.tryConfirm(u, j.ID, p.Fingerprint, e.key(), nil); err != nil || r.Reserved != rows {
		t.Errorf("control: the seeded off with module 1 answering off was refused or mispriced: %+v, %v", r, err)
	}
	e.requireBooks("after the unreadable cases")
}

func TestJobsStore_VendorReachabilityFromTheModule_ModuleOneIsAskedBeforeTheTransactionNeverUnderTheLockAndOnlyForTheUsersModuleOneJob(t *testing.T) {
	f := &vendorFake{}
	e := newVendorEnv(t, f)
	f.says(false)
	u := e.newUser()
	e.fund(u, "m1", 1000)
	j, p := e.previewed(u, "m1", 3)
	if f.count() != 1 {
		t.Fatalf("a module 1 preview asked module 1 %d time(s), want once", f.count())
	}

	// the (user, module) advisory lock is FREE while module 1 is asked: it is not an HTTP call under the lock
	lockFree := false
	f.probe = func() {
		var got bool
		if err := e.admin.QueryRow(testCtx(t), "SELECT pg_try_advisory_lock($1)", money.LockKey(u, "m1")).Scan(&got); err != nil {
			t.Errorf("the probe: %v", err)
			return
		}
		lockFree = got
		if got {
			e.exec("SELECT pg_advisory_unlock($1)", money.LockKey(u, "m1"))
		}
	}
	key := e.key()
	n := f.count()
	r, err := e.tryConfirm(u, j.ID, p.Fingerprint, key, nil)
	if err != nil || r.Replayed {
		t.Fatalf("confirm with the probe: %+v, %v", r, err)
	}
	if !lockFree {
		t.Error("the (user, module) advisory lock was held while module 1 was asked: an HTTP call under the lock")
	}
	if f.count() != n+1 {
		t.Errorf("a module 1 confirm asked module 1 %d time(s), want once", f.count()-n)
	}
	f.probe = nil

	// a replay of the confirm (the same key) is answered before anything is read
	n = f.count()
	rr, err := e.tryConfirm(u, j.ID, p.Fingerprint, key, nil)
	if err != nil || !rr.Replayed || f.count() != n {
		t.Errorf("a replayed confirm: %+v, %v, %d call(s) of module 1; want a replay that asked nothing", rr, err, f.count()-n)
	}
	// another user's job, and a job that does not exist, are the ErrNotFound they always were, and module 1 is not asked for them
	other := e.newUser()
	if _, err := e.jobs.Preview(testCtx(t), other, j.ID); !errors.Is(err, jobs.ErrNotFound) || f.count() != n {
		t.Errorf("another user's job: %v, %d call(s); want ErrNotFound and no read", err, f.count()-n)
	}
	if _, err := e.jobs.Confirm(testCtx(t), other, jobs.ConfirmReq{JobID: j.ID, Fingerprint: p.Fingerprint, Key: e.key()}); !errors.Is(err, jobs.ErrNotFound) || f.count() != n {
		t.Errorf("another user's confirm: %v, %d call(s); want ErrNotFound and no read", err, f.count()-n)
	}
	if _, err := e.jobs.Preview(testCtx(t), u, "0b000000-0000-4000-8000-0000000000ff"); !errors.Is(err, jobs.ErrNotFound) || f.count() != n {
		t.Errorf("a job that does not exist: %v, %d call(s); want ErrNotFound and no read", err, f.count()-n)
	}
	// a module 2 job: neither its preview nor its confirm asks
	m2 := e.newUser()
	e.fund(m2, "m2", 100)
	jm, pm := e.previewed(m2, "m2", 2)
	e.confirm(m2, jm.ID, pm.Fingerprint)
	if f.count() != n {
		t.Errorf("a module 2 preview and confirm asked module 1 %d time(s)", f.count()-n)
	}
	// the contrast: a module 1 preview and confirm of another user ask once each
	u3 := e.newUser()
	e.fund(u3, "m1", 1000)
	j3, p3 := e.previewed(u3, "m1", 2)
	e.confirm(u3, j3.ID, p3.Fingerprint)
	if f.count() != n+2 {
		t.Errorf("a module 1 preview and a confirm asked module 1 %d time(s), want twice", f.count()-n)
	}

	// the read is made BEFORE the transaction: while the test holds the (user, module) lock, a confirm waits for it, and module 1 has already been asked
	u4 := e.newUser()
	e.fund(u4, "m1", 1000)
	j4, p4 := e.previewed(u4, "m1", 2)
	before := f.count()
	h := e.comHold(u4, "m1")
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		_, err := e.jobs.Confirm(ctx, u4, jobs.ConfirmReq{JobID: j4.ID, Fingerprint: p4.Fingerprint, Key: e.key()})
		done <- err
	}()
	if _, ok := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		for _, w := range ws {
			if w.Event == "advisory" && mnyHasPID(w.Blockers, h.pid) && strings.Contains(w.Query, "pg_advisory_xact_lock") {
				return true
			}
		}
		return false
	}); !ok {
		h.release()
		t.Fatal("the confirm was never seen blocked on the (user, module) lock behind the test's holder")
	}
	select {
	case err := <-done:
		h.release()
		t.Fatalf("the confirm returned (%v) while the test still held the lock", err)
	default:
	}
	if f.count() != before+1 {
		h.release()
		t.Fatalf("module 1 was asked %d time(s) by the time the confirm waited for the lock, want once: the read must come before the transaction", f.count()-before)
	}
	h.release()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the confirm failed after the lock was let go: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the confirm did not return after the test let go of the lock")
	}
	e.requireBooks("after the reads")
}

func TestJobsStore_VendorReachabilityFromTheModule_WithNoProviderNothingIsReadAndNothingIsRefused(t *testing.T) {
	e := newJbsEnv(t) // the default store: no provider
	// the seeded off, never set by module 1: with a provider this refuses an immediate confirm of a job nobody can read module 1 for; with none there is nobody to ask and nothing is refused
	e.setVendorFlag(false, vendorSeedSource)
	u := e.newUser()
	e.fund(u, "m1", 1000)
	j, p := e.previewed(u, "m1", 5)
	if p.VendorReachableKnown {
		t.Error("a store with no provider says module 1 was read")
	}
	r, err := e.tryConfirm(u, j.ID, p.Fingerprint, e.key(), nil)
	if err != nil || r.Reserved != 5 {
		t.Errorf("an immediate confirm with no provider: %+v, %v; want accepted at 1 per row", r, err)
	}
	if got := e.vendorFlag(); got != "false|"+vendorSeedSource {
		t.Errorf("a store with no provider changed the flag to %q", got)
	}
	// the same state with a provider that cannot be asked, on a second store over the same database: refused (the contrast that makes the line above mean something)
	f := &vendorFake{}
	withProvider, err := jobs.New(e.pool, e.store, jobs.WithVendorReachability(f), jobs.WithVendorReachabilityTTL(0))
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	f.cannotBeAsked()
	u2 := e.newUser()
	e.fund(u2, "m1", 1000)
	j2 := e.create(u2, "m1", 5)
	p2, err := withProvider.Preview(testCtx(t), u2, j2.ID)
	if err != nil || p2.VendorReachableKnown {
		t.Fatalf("the preview with a provider that cannot be asked: %+v, %v; want a preview that says it was not read", p2, err)
	}
	if _, err := withProvider.Confirm(testCtx(t), u2, jobs.ConfirmReq{JobID: j2.ID, Fingerprint: p2.Fingerprint, Key: e.key()}); !errors.Is(err, jobs.ErrModuleUnavailable) {
		t.Errorf("the contrast: the same state with a provider that cannot be asked gave %v, want ErrModuleUnavailable", err)
	}
	if f.count() != 2 {
		t.Errorf("the provider was asked %d time(s) by a preview and a confirm, want 2", f.count())
	}
}

// ---- the money store's read of the flag --------------------------------------------------------------------------------------------------------------------------------------------

func TestMoneyReachable_UnitReachabilityReadsTheFlagWithItsSource(t *testing.T) {
	e := newJbsEnv(t)
	ctx := testCtx(t)
	// the seed: off, by the seed's words, with a time
	e.setVendorFlag(false, vendorSeedSource)
	u, err := e.store.UnitReachability(ctx, money.UnitM1Vendor)
	if err != nil || u.Reachable || u.Source != vendorSeedSource || u.FromModule() || u.ChangedAt.IsZero() {
		t.Fatalf("the seeded flag reads %+v, %v; want off, the seed's words, not from module 1, with a time", u, err)
	}
	// a flip by module 1's source
	if changed, err := e.store.SetUnitReachable(ctx, money.UnitM1Vendor, true, money.ReachableSourceM1Describe); err != nil || !changed {
		t.Fatalf("SetUnitReachable: %v, %v", changed, err)
	}
	u2, err := e.store.UnitReachability(ctx, money.UnitM1Vendor)
	if err != nil || !u2.Reachable || u2.Source != "m1_describe" || !u2.FromModule() || !u2.ChangedAt.After(u.ChangedAt.Add(-time.Second)) {
		t.Errorf("the flipped flag reads %+v, %v; want on, m1_describe, from module 1", u2, err)
	}
	// the same value again changes nothing, and so does not change the source either (the writer's idempotence: why the job store tests the SOURCE, not the history)
	if changed, err := e.store.SetUnitReachable(ctx, money.UnitM1Vendor, true, "test: the same again"); err != nil || changed {
		t.Fatalf("the same value again: %v, %v", changed, err)
	}
	if u3, _ := e.store.UnitReachability(ctx, money.UnitM1Vendor); u3.Source != "m1_describe" {
		t.Errorf("a write that changed nothing changed the source to %q", u3.Source)
	}
	// a unit that is not priced
	if _, err := e.store.UnitReachability(ctx, "m9_nothing"); !errors.Is(err, money.ErrUnknownUnit) {
		t.Errorf("an unknown unit read as %v, want ErrUnknownUnit", err)
	}
	// a unit that is priced reads too (m1_own is reachable, by default, with no source)
	if o, err := e.store.UnitReachability(ctx, "m1_own"); err != nil || !o.Reachable || o.FromModule() {
		t.Errorf("m1_own reads %+v, %v; want reachable and not from module 1", o, err)
	}
}
