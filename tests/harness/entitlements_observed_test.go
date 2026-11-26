//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/m3admin"
)

// Close-out D5 (open decision 12): CONVERGENCE FRESHNESS BY THE DATABASE, and the reconciler that reads module 3 back. platform_0008 gave platform.m3_sync `observed_version` (the want_version an observation was
// made at) and `m3_version` (module 3's own counter from its suspension read route), and replaced the convergence CHECK with m3_sync_converged_only_when_observed_at_version: converged_version = want_version
// needs observed_version = want_version AND observed = the wanted state. This file proves (1) the database refuses the stale observation whoever writes it, (2) what the reconciler records and refuses with a
// FAKE module 3 that can answer wrongly, slowly or not at all on command (the helpers are entitlements_reconcile_test.go's), (3) the one-time backfill, and (4) against the REAL module 3 that the version the
// platform records is the version module 3 itself reports. Every name here starts with entObs so it cannot clash with another file's helpers.

const entObsOnly = "m3_sync_converged_only_when_observed_at_version"

// entObsLines counts the log lines that carry every one of parts.
func entObsLines(logs *entLogs, parts ...string) int {
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		ok := line != ""
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			n++
		}
	}
	return n
}

// entObsRefused fails unless err is the database refusing with SQLSTATE 23514 and the replacement convergence CHECK's name.
func entObsRefused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: ACCEPTED by the database, want 23514 / %s", what, entObsOnly)
		return
	}
	pe := entPgError(t, what, err)
	if pe.Code != "23514" || pe.ConstraintName != entObsOnly {
		t.Errorf("%s: the database said %s / constraint %q (%s), want 23514 / %q", what, pe.Code, pe.ConstraintName, pe.Message, entObsOnly)
	}
}

func entObsSameVersion(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func entObsStr(v *int64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(*v)
}

// ---- 1. the database -------------------------------------------------------------------------------------------------------------------------------------------------------

// TestEntObserved_TheDatabaseRefusesAStaleObservation: with converged_version = want_version, an observation that was not made at THIS want_version is refused with the constraint's name and SQLSTATE 23514,
// whoever writes (platform_app, and the superuser, so it is the constraint and not a grant) and whatever the observation says: the wrong version (older, ahead, NULL) or the wrong state at the right version.
// The scenario is the one the platform_0006 rule let through: version 1 wanted active and seen active, version 2 suspended, version 3 active AGAIN: the old observation 'active' matches the new wanted state, and
// only its version says it is not about this one. The paired control of every refusal is the same statement with the observation made at the right version, accepted.
func TestEntObserved_TheDatabaseRefusesAStaleObservation(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	ctx := testCtx(t)
	byApp := func(sets string) error { // platform_app: the identity the service writes with
		_, err := e.pool.Exec(ctx, "UPDATE platform.m3_sync SET "+sets+" WHERE user_id = $1", u)
		return err
	}
	bySuperuser := func(sets string) error { // the superuser: the constraint holds for every writer, not only for the role's grants
		_, err := e.admin.Exec(ctx, "UPDATE platform.m3_sync SET "+sets+" WHERE user_id = $1", u)
		return err
	}
	// refused runs the statement as platform_app and as the superuser, requires the refusal from both, and requires that the row did not move.
	refused := func(what, sets string) {
		t.Helper()
		before := e.syncOf(u)
		for _, who := range []struct {
			name string
			run  func(string) error
		}{{"platform_app", byApp}, {"the superuser", bySuperuser}} {
			entObsRefused(t, what+" (as "+who.name+")", who.run(sets))
		}
		after := e.syncOf(u)
		if after.ConvergedVers != before.ConvergedVers || after.Observed != before.Observed || !entObsSameVersion(after.ObservedVersion, before.ObservedVersion) || !entObsSameVersion(after.M3Version, before.M3Version) {
			t.Errorf("%s: a refused statement changed the row: before %+v, after %+v", what, before, after)
		}
	}
	accepted := func(what, sets string) {
		t.Helper()
		if err := byApp(sets); err != nil {
			t.Fatalf("control failed, %s: %v", what, err)
		}
	}

	// version 1: wanted active; the legal convergence is an observation of version 1
	e.mustEnable(u, nil)
	if s := e.syncOf(u); s.WantVersion != 1 || s.WantSuspended || s.ConvergedVers != 0 || s.ObservedVersion != nil || s.M3Version != nil {
		t.Fatalf("setup: the delivery row is %+v, want version 1, wanted active, nothing converged and no observation version", s)
	}
	accepted("converged at version 1 with 'active' observed at version 1", "observed = 'active', observed_at = now(), observed_version = want_version, m3_version = 0, converged_version = want_version")
	if s := e.syncOf(u); s.ConvergedVers != 1 || s.Observed != "active" || s.ObservedVersion == nil || *s.ObservedVersion != 1 || s.M3Version == nil || *s.M3Version != 0 {
		t.Fatalf("control: the converged row is %+v, want converged at 1, 'active' observed at version 1, module 3's counter 0", s)
	}

	// the admin disables (version 2) and enables again (version 3): the wanted state is 'active' once more and the row still carries the observation of version 1, which also says 'active'
	e.mustDisable(u)
	e.mustEnable(u, nil)
	if s := e.syncOf(u); s.WantVersion != 3 || s.WantSuspended || s.ConvergedVers != 1 || s.Observed != "active" || s.ObservedVersion == nil || *s.ObservedVersion != 1 {
		t.Fatalf("setup: the delivery row is %+v, want version 3 wanted active, converged at 1, and still the observation 'active' of version 1", s)
	}
	refused("the old observation re-used (the state matches, its version is 1, the wanted version is 3)", "converged_version = want_version")
	refused("an observation of the version before the wanted one", "observed_version = want_version - 1, converged_version = want_version")
	refused("an observation with no version (NULL)", "observed_version = NULL, converged_version = want_version")
	refused("an observation of a version ahead of the wanted one", "observed_version = want_version + 1, converged_version = want_version")
	refused("the right version, the other state", "observed = 'suspended', observed_version = want_version, converged_version = want_version")
	refused("the right version, never observed", "observed = 'unknown', observed_version = want_version, converged_version = want_version")
	accepted("converged at version 3 with 'active' observed at version 3", "observed = 'active', observed_at = now(), observed_version = want_version, m3_version = 2, converged_version = want_version")
	if s := e.syncOf(u); s.ConvergedVers != 3 || s.ObservedVersion == nil || *s.ObservedVersion != 3 || s.M3Version == nil || *s.M3Version != 2 {
		t.Fatalf("control: the converged row is %+v, want converged at 3 with the observation of version 3", s)
	}

	// a converged row cannot have its observation walked back to another version, or its state moved, while it still claims the convergence
	refused("moving observed_version back on a converged row", "observed_version = observed_version - 1")
	refused("moving observed away from the wanted state on a converged row", "observed = 'suspended'")

	// the other direction: version 4 wants SUSPENDED; the row carries 'active' observed at version 3 (the other state AND an older version)
	e.mustDisable(u)
	if s := e.syncOf(u); s.WantVersion != 4 || !s.WantSuspended || s.ConvergedVers != 3 {
		t.Fatalf("setup: the delivery row is %+v, want version 4 wanted suspended, converged at 3", s)
	}
	refused("the observation of version 3 (the other state, an older version)", "converged_version = want_version")
	refused("the wanted state asserted without moving the version it was observed at (still 3)", "observed = 'suspended', converged_version = want_version")
	accepted("converged at version 4 with 'suspended' observed at version 4", "observed = 'suspended', observed_at = now(), observed_version = want_version, m3_version = 3, converged_version = want_version")
	if s := e.syncOf(u); s.ConvergedVers != 4 || s.Observed != "suspended" || s.ObservedVersion == nil || *s.ObservedVersion != 4 {
		t.Fatalf("control: the converged row is %+v, want converged at 4, 'suspended' observed at version 4", s)
	}
}

// ---- 2. the reconciler against a fake module 3 -----------------------------------------------------------------------------------------------------------------------------

// TestEntObserved_AConvergenceIsTheReadBackRecordedWithTheVersionItWasMadeAt: a first delivery makes the call and then READS the state back; the row converges only on the read, and records the read's state, the
// want_version it was compared against, and module 3's own counter exactly as the read answered it (1 after a suspend, 0 for a user module 3 never suspended). Paired control: converged rows are not read again.
func TestEntObserved_AConvergenceIsTheReadBackRecordedWithTheVersionItWasMadeAt(t *testing.T) {
	e := newEntEnv(t, 2)
	disabled, enabled := entUser(0), entUser(1)
	e.mustDisable(disabled)
	e.mustEnable(enabled, nil)

	var (
		mu     sync.Mutex
		events []string
	)
	note := func(s string) {
		mu.Lock()
		events = append(events, s)
		mu.Unlock()
	}
	fake := &entFake{}
	// module 3: a suspend changes the state (200); an unsuspend of a user who was never suspended is the 422 "already" (nothing changes)
	fake.set(func(_ context.Context, _ int, c entCall) (m3admin.Result, error) {
		note("call:" + c.User)
		return entEcho(c.Suspended, c.Suspended), nil
	})
	fake.setRead(func(_ context.Context, _ int, user string) (m3admin.SuspensionState, error) {
		note("read:" + user)
		st := fake.m3State(user)
		return m3admin.SuspensionState{User: user, Suspended: st.suspended, Version: st.version}, nil
	})
	r, _ := e.reconciler(fake, nil)

	rep := e.run(r)
	if rep.Attempted != 2 || rep.Converged != 2 || rep.Failed != 0 || rep.StaleSkipped != 0 || rep.Unconverged != 0 {
		t.Fatalf("report = %+v, want both users converged", rep)
	}
	position := func(ev string) int {
		mu.Lock()
		defer mu.Unlock()
		for i, x := range events {
			if x == ev {
				return i
			}
		}
		return -1
	}
	for _, u := range []string{disabled, enabled} {
		if c, rd := position("call:"+u), position("read:"+u); c < 0 || rd < 0 || rd < c {
			t.Errorf("%s: the call is event %d and the read-back event %d, want both, the read AFTER the call (events %v)", u, c, rd, events)
		}
	}
	sd := e.requireConverged("the disabled user", disabled, "suspended")
	se := e.requireConverged("the enabled user", enabled, "active")
	if *sd.M3Version != 1 || *se.M3Version != 0 {
		t.Errorf("module 3's own counter is recorded as the read answered it: %d for the suspended user (want 1), %d for the user it never suspended (want 0)", *sd.M3Version, *se.M3Version)
	}
	if st, err := e.store.Sync(testCtx(t), disabled); err != nil || !st.Converged() || st.ObservedVersion == nil || *st.ObservedVersion != st.WantVersion || st.M3Version == nil {
		t.Errorf("Store.Sync(%s) = %+v, %v, want a converged state carrying the observation's version and module 3's counter", disabled, st, err)
	}
	if fake.count() != 2 || fake.readCount() != 2 {
		t.Errorf("%d calls and %d reads, want one of each per user", fake.count(), fake.readCount())
	}
	// paired control: with everything converged and no re-verification a second pass neither calls nor reads
	if rep := e.run(r); rep.Attempted != 0 || fake.count() != 2 || fake.readCount() != 2 {
		t.Errorf("a pass over converged rows made %d attempts, %d calls and %d reads in all, want none beyond the first two of each", rep.Attempted, fake.count(), fake.readCount())
	}
}

// TestEntObserved_AReadBackThatDisagreesNeverConvergesAndWarnsOncePerStretch: module 3 answers the call 200 and its READ says it is in the other state. The row is not converged, records what the read said (at the
// version it was compared against, with the read's counter), and the call is made again on every pass because the observation contradicts the wanted state; the warning is logged once for the stretch and the
// later passes only at debug level. When the read agrees the row converges (the control), and a NEW stretch (a new wanted version that disagrees again) warns again.
func TestEntObserved_AReadBackThatDisagreesNeverConvergesAndWarnsOncePerStretch(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u) // wanted suspended, version 1
	fake := &entFake{}
	fake.set(func(_ context.Context, n int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, n == 1), nil
	}) // 200 once, then the 422 "already"
	fake.setRead(func(_ context.Context, _ int, user string) (m3admin.SuspensionState, error) {
		return m3admin.SuspensionState{User: user, Suspended: false, Version: 0}, nil // the call said 200; the READ says module 3 is active
	})
	r, logs := e.reconciler(fake, nil)

	for pass := 1; pass <= 3; pass++ {
		rep := e.run(r)
		if rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || rep.StaleSkipped != 0 || rep.Unconverged != 1 {
			t.Fatalf("pass %d: report = %+v, want one failed attempt and the user still unconverged", pass, rep)
		}
		s := e.requireBehind(fmt.Sprintf("pass %d, the read disagrees", pass), u)
		if s.Observed != "active" || s.ObservedVersion == nil || *s.ObservedVersion != s.WantVersion || s.M3Version == nil || *s.M3Version != 0 || s.Attempts != pass ||
			s.LastError == nil || !strings.Contains(*s.LastError, `suspension read says "active"`) || !strings.Contains(*s.LastError, "not converged") {
			t.Fatalf("pass %d: the row is %+v (last_error %v), want what the READ said (active, version 0) recorded at this version, %d attempts and an error naming both states", pass, s, s.LastError, pass)
		}
		if st, _ := e.store.Sync(testCtx(t), u); st.Converged() {
			t.Fatalf("pass %d: the store reports a user whose read-back disagreed as converged", pass)
		}
	}
	// the observation contradicts the wanted state, so the call is due on every pass, each followed by a read
	if fake.count() != 3 || fake.readCount() != 3 {
		t.Errorf("%d calls and %d reads over three passes, want 3 of each", fake.count(), fake.readCount())
	}
	if warns, quiet := entObsLines(logs, "level=WARN", "suspension read says another state"), entObsLines(logs, "level=DEBUG", "still disagrees"); warns != 1 || quiet != 2 {
		t.Errorf("%d warnings and %d quiet lines for a stretch of three disagreeing passes, want 1 and 2 (once per stretch):\n%s", warns, quiet, logs.String())
	}

	// the control: module 3 gets it right (the read agrees), the same row converges
	fake.setRead(nil)
	if rep := e.run(r); rep.Converged != 1 || rep.Unconverged != 0 {
		t.Fatalf("after the read agreed: %+v", rep)
	}
	if s := e.requireConverged("once the read agrees", u, "suspended"); *s.M3Version != 1 {
		t.Errorf("m3_version = %d, want module 3's counter 1 (the one change)", *s.M3Version)
	}

	// a NEW stretch: the admin re-enables (version 2 wants active) and the read disagrees again (it still says suspended): warned again
	e.mustEnable(u, nil)
	fake.setRead(func(_ context.Context, _ int, user string) (m3admin.SuspensionState, error) {
		return m3admin.SuspensionState{User: user, Suspended: true, Version: 1}, nil
	})
	if rep := e.run(r); rep.Failed != 1 || rep.Converged != 0 {
		t.Fatalf("the new stretch: %+v, want one failure", rep)
	}
	if warns := entObsLines(logs, "level=WARN", "suspension read says another state"); warns != 2 {
		t.Errorf("%d warnings after a second stretch of disagreement, want 2 (one per stretch)", warns)
	}
	fake.setRead(nil)
	e.run(r)
	e.requireConverged("the second stretch resolved", u, "active")
}

// TestEntObserved_AReadRouteFailureLeavesTheUserUnconvergedAndTheNextPassesReadWithoutCalling: the call is confirmed and the read route answers 5xx. The user is NOT converged (the call's confirmation is not one),
// the row keeps the confirmation (observed at this version, no module 3 counter: nothing was read) and the failure, the pass retries the READ alone (the call is not repeated) and converges the moment the read
// answers. The control is the end of the same test: the same row converges once the route is back.
func TestEntObserved_AReadRouteFailureLeavesTheUserUnconvergedAndTheNextPassesReadWithoutCalling(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u)
	fake := &entFake{}
	fake.set(func(_ context.Context, n int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, n == 1), nil
	})
	fake.setRead(func(context.Context, int, string) (m3admin.SuspensionState, error) {
		return m3admin.SuspensionState{}, &m3admin.RetryableError{Op: "suspension read", HTTPStatus: 503, Reason: "a server error"}
	})
	r, logs := e.reconciler(fake, nil)

	// pass 1: the call is made, its answer is the wanted state, the read fails
	rep := e.run(r)
	if rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || rep.Unconverged != 1 || len(rep.Errors) != 1 {
		t.Fatalf("pass 1: report = %+v, want one failed attempt", rep)
	}
	s := e.requireBehind("after a read failure", u)
	if s.Observed != "suspended" || s.ObservedVersion == nil || *s.ObservedVersion != s.WantVersion || s.M3Version != nil || s.Attempts != 1 ||
		s.LastError == nil || !strings.Contains(*s.LastError, "read-back failed") || !strings.Contains(*s.LastError, "HTTP 503") {
		t.Fatalf("pass 1: the row is %+v (last_error %v), want the call's confirmation kept (suspended, at this version), no module 3 counter, one attempt and an error naming the read-back and the 503", s, s.LastError)
	}
	if st, _ := e.store.Sync(testCtx(t), u); st.Converged() {
		t.Fatal("the store reports a user whose read-back failed as converged")
	}
	if fake.count() != 1 || fake.readCount() != 1 {
		t.Fatalf("pass 1: %d calls and %d reads, want 1 of each", fake.count(), fake.readCount())
	}
	if entObsLines(logs, "level=WARN", "read-back failed") != 1 {
		t.Errorf("the read failure was not logged once at warn level:\n%s", logs.String())
	}

	// pass 2: the read still fails; the confirmation of this version is kept, so the CALL IS NOT MADE AGAIN
	rep = e.run(r)
	if rep.Attempted != 1 || rep.Failed != 1 || fake.count() != 1 || fake.readCount() != 2 {
		t.Fatalf("pass 2: report = %+v with %d calls and %d reads, want a failed attempt that only read", rep, fake.count(), fake.readCount())
	}
	if s := e.requireBehind("after the second read failure", u); s.Attempts != 2 || s.Observed != "suspended" || s.M3Version != nil {
		t.Fatalf("pass 2: the row is %+v, want 2 attempts and the same confirmation", s)
	}

	// pass 3: the route is back: converged by the read alone, still one call in all
	fake.setRead(nil)
	rep = e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.Unconverged != 0 || fake.count() != 1 || fake.readCount() != 3 {
		t.Fatalf("pass 3: report = %+v with %d calls and %d reads, want the user converged by the third read and no second call", rep, fake.count(), fake.readCount())
	}
	if s := e.requireConverged("after the read route came back", u, "suspended"); *s.M3Version != 1 {
		t.Errorf("m3_version = %d, want module 3's counter 1", *s.M3Version)
	}
}

// TestEntObserved_APureReadPassConvergesFromTheReadAloneAndOnlyForTheObservationOfThisVersion: a row whose call module 3 already confirmed at THIS version (the state an earlier pass left when it could not
// read) converges from the read alone, with NO call; the paired controls are the two rows that look alike and must be called: one whose confirmation is of an OLDER version (a stale observation confirms
// nothing) and one whose observation is the OTHER state at this version (module 3 contradicted the wanted state).
func TestEntObserved_APureReadPassConvergesFromTheReadAloneAndOnlyForTheObservationOfThisVersion(t *testing.T) {
	e := newEntEnv(t, 3)
	fresh, stale, other := entUser(0), entUser(1), entUser(2)
	// fresh: version 1, wanted suspended, and the call's confirmation of THIS version is already recorded (an earlier pass could not read)
	e.mustDisable(fresh)
	e.exec("UPDATE platform.m3_sync SET observed = 'suspended', observed_at = now(), observed_version = 1 WHERE user_id = $1", fresh)
	// stale: version 2 wanted suspended; the recorded 'suspended' is an observation made for version 1, so it confirms nothing for version 2
	e.mustEnable(stale, nil)
	e.mustDisable(stale)
	e.exec("UPDATE platform.m3_sync SET observed = 'suspended', observed_at = now(), observed_version = 1 WHERE user_id = $1", stale)
	// other: version 1, wanted suspended, and module 3 said the OTHER state at this version
	e.mustDisable(other)
	e.exec("UPDATE platform.m3_sync SET observed = 'active', observed_at = now(), observed_version = 1 WHERE user_id = $1", other)

	// module 3 already holds all three suspended at its counter 3, so a call that is made is answered by the 422 "already" and changes nothing
	fake := &entFake{state: map[string]entM3State{fresh: {true, 3}, stale: {true, 3}, other: {true, 3}}}
	fake.set(func(_ context.Context, _ int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, false), nil
	})
	r, _ := e.reconciler(fake, nil)
	rep := e.run(r)
	if rep.Attempted != 3 || rep.Converged != 3 || rep.Failed != 0 || rep.Unconverged != 0 {
		t.Fatalf("report = %+v, want the three users converged", rep)
	}
	called := map[string]bool{}
	for _, c := range fake.called() {
		called[c.User] = true
	}
	if called[fresh] || !called[stale] || !called[other] || fake.count() != 2 {
		t.Errorf("the calls were %+v, want exactly two: for the row with a stale observation and for the row module 3 contradicted, and NONE for the row whose call was already confirmed at this version", fake.called())
	}
	if fake.readCount() != 3 {
		t.Errorf("%d reads, want 3 (every row is converged by a read)", fake.readCount())
	}
	for _, u := range []string{fresh, stale, other} {
		s := e.requireConverged("after the pass", u, "suspended")
		if *s.M3Version != 3 {
			t.Errorf("%s: m3_version = %d, want module 3's counter 3 as the read answered it", u, *s.M3Version)
		}
	}
	if s := e.syncOf(stale); s.WantVersion != 2 || *s.ObservedVersion != 2 {
		t.Errorf("the stale row converged at version %d with an observation of version %d, want 2 and 2: the old observation was replaced, not reused", s.WantVersion, *s.ObservedVersion)
	}
}

// TestEntObserved_AnUnknownUserOrARejectedKeyOnTheReadIsNeverAnAnswer: module 3's 404 on the read (typed unknown user) is not "active": a user WANTED ACTIVE, whose call was confirmed, is not converged by it;
// and a 401 on the read is a configuration fault that stops the pass like a 401 on the call and never puts the key in the stored error, the report or the log. The control is the same users converging once
// the read answers.
func TestEntObserved_AnUnknownUserOrARejectedKeyOnTheReadIsNeverAnAnswer(t *testing.T) {
	e := newEntEnv(t, 3)
	u0, u1, u2 := entUser(0), entUser(1), entUser(2)
	e.mustEnable(u0, nil) // wanted ACTIVE: the unknown user's zero state would read as active
	fake := &entFake{}
	fake.set(func(_ context.Context, n int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, false), nil
	})
	fake.setRead(func(_ context.Context, _ int, user string) (m3admin.SuspensionState, error) {
		return m3admin.SuspensionState{}, fmt.Errorf("suspension read: %w", m3admin.ErrUnknownUser)
	})
	r, _ := e.reconciler(fake, nil)

	rep := e.run(r)
	if rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || rep.ConfigFault || rep.Unconverged != 1 {
		t.Fatalf("unknown user: report = %+v, want one failed attempt and no convergence", rep)
	}
	s := e.requireBehind("a 404 on the read", u0)
	if s.Attempts != 1 || s.LastError == nil || !strings.Contains(*s.LastError, "no state to confirm") || s.M3Version != nil {
		t.Errorf("unknown user: the row is %+v (last_error %v), want one attempt, no module 3 counter and an error that says there is no state to confirm", s, s.LastError)
	}

	// a rejected key on the read: two more users wait; the pass stops after ONE read, whoever is first
	e.mustEnable(u1, nil)
	e.mustEnable(u2, nil)
	before := map[string]int{}
	for _, u := range []string{u0, u1, u2} {
		before[u] = e.syncOf(u).Attempts
	}
	readsBefore := fake.readCount()
	fake.setRead(func(context.Context, int, string) (m3admin.SuspensionState, error) {
		return m3admin.SuspensionState{}, fmt.Errorf("suspension read: %w (the key was %s)", m3admin.ErrUnauthorized, entCanary)
	})
	rep = e.run(r)
	if !rep.ConfigFault || rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || fake.readCount() != readsBefore+1 || rep.Unconverged != 3 {
		t.Fatalf("rejected key: report = %+v after %d reads, want a config fault after ONE read (the key is wrong for every user)", rep, fake.readCount()-readsBefore)
	}
	moved := 0
	for _, u := range []string{u0, u1, u2} {
		s := e.requireBehind("a 401 on the read", u)
		moved += s.Attempts - before[u]
		if s.LastError != nil && (strings.Contains(*s.LastError, entCanary) || (s.Attempts > before[u] && !strings.Contains(*s.LastError, "PLATFORM_M3_ADMIN_TOKEN"))) {
			t.Errorf("%s: last_error %q must name the variable and never carry the key", u, *s.LastError)
		}
	}
	if moved != 1 {
		t.Errorf("%d attempts were recorded by the stopped pass, want exactly 1", moved)
	}
	if strings.Contains(strings.Join(rep.Errors, "|"), entCanary) {
		t.Errorf("the report carries the admin key: %v", rep.Errors)
	}

	// control: the read answers (module 3 knows the users, they are active): everyone converges, at their own version
	fake.setRead(nil)
	if rep := e.run(r); rep.Converged != 3 || rep.Unconverged != 0 {
		t.Fatalf("after the read answered: %+v", rep)
	}
	for _, u := range []string{u0, u1, u2} {
		e.requireConverged("after the read answered", u, "active")
	}
}

// TestEntObserved_AStaleWantVersionDuringTheReadIsNotRecordedAsConverging: module 3's read is held open while the entitlement moves on to version 3, whose wanted state (suspended) is the same as the one the held
// read is about to report for version 1: the observation MATCHES the new wanted state and only its version is stale. The compare-and-set refuses it (counted as stale, nothing recorded: no observation, no
// version, no attempt), and the next pass delivers version 3 with its own call and read and converges at 3. The paired control is that very convergence.
func TestEntObserved_AStaleWantVersionDuringTheReadIsNotRecordedAsConverging(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u) // version 1, wanted suspended

	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	fake := &entFake{}
	fake.setRead(func(_ context.Context, n int, user string) (m3admin.SuspensionState, error) {
		if n == 1 {
			entered <- struct{}{}
			<-gate // module 3 is slow to answer the read
		}
		return m3admin.SuspensionState{User: user, Suspended: true, Version: 1}, nil // the right answer for version 1 AND for version 3
	})
	r, _ := e.reconciler(fake, nil)

	type result struct {
		rep entitlements.Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := r.RunOnce(testCtx(t))
		done <- result{rep, err}
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconciler never read module 3")
	}
	// while the read for version 1 is in flight: enabled (version 2), disabled again (version 3: wanted suspended once more)
	e.mustEnable(u, nil)
	e.mustDisable(u)
	if s := e.syncOf(u); s.WantVersion != 3 || !s.WantSuspended {
		t.Fatalf("setup: the delivery row is %+v, want version 3 wanted suspended", s)
	}
	close(gate)

	res := <-done
	if res.err != nil {
		t.Fatalf("RunOnce: %v", res.err)
	}
	if !res.rep.Balanced() || res.rep.Attempted != 1 || res.rep.StaleSkipped != 1 || res.rep.Converged != 0 || res.rep.Failed != 0 {
		t.Fatalf("report = %+v, want the observation of version 1 counted as stale and nothing converged", res.rep)
	}
	s := e.requireBehind("after a stale observation", u)
	if s.WantVersion != 3 || s.ConvergedVers != 0 || s.Observed != "unknown" || s.ObservedAt != nil || s.ObservedVersion != nil || s.M3Version != nil || s.Attempts != 0 || s.LastError != nil || s.LastAttemptAt != nil {
		t.Fatalf("the stale observation was recorded: %+v", s)
	}

	// the control: the next pass delivers version 3 on its own call and read, and converges AT 3
	rep := e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.StaleSkipped != 0 {
		t.Fatalf("the next pass: %+v", rep)
	}
	if s := e.requireConverged("version 3", u, "suspended"); s.WantVersion != 3 || *s.ObservedVersion != 3 {
		t.Errorf("converged at %d with an observation of version %d, want 3 and 3", s.WantVersion, *s.ObservedVersion)
	}
	if calls := fake.called(); len(calls) != 2 || !calls[0].Suspended || !calls[1].Suspended {
		t.Errorf("the calls were %+v, want a suspend for version 1 and a second suspend for version 3", calls)
	}
}

// TestEntObserved_AModule3VersionThatGoesBackwardsIsWarnedAboutAndStillJudged: m3_version is informational, so a counter that moved BACKWARDS (a restored module 3 database) does not stop a convergence that the
// read proves, but it is logged at warn level naming both numbers, and the lower number is what is recorded. The control: a counter that did not go backwards logs nothing.
func TestEntObserved_AModule3VersionThatGoesBackwardsIsWarnedAboutAndStillJudged(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u)
	fake := &entFake{}
	r, logs := e.reconciler(fake, func(r *entitlements.Reconciler) { r.ReverifyEvery = time.Hour })

	e.run(r) // converges: the first call changes module 3, whose counter becomes 1
	if s := e.requireConverged("first delivery", u, "suspended"); *s.M3Version != 1 {
		t.Fatalf("m3_version = %d after the first delivery, want 1", *s.M3Version)
	}
	fake.set(func(_ context.Context, _ int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, false), nil
	}) // re-verification: module 3 still holds the state

	// the recorded counter is ahead of what module 3 now says (it was restored from a backup), and the observation is old enough to be re-verified
	e.exec("UPDATE platform.m3_sync SET m3_version = 9, observed_at = now() - interval '2 hours' WHERE user_id = $1", u)
	rep := e.run(r)
	if rep.Converged != 1 || rep.Drifted != 0 {
		t.Fatalf("the re-verification: %+v, want a convergence and no drift", rep)
	}
	if s := e.requireConverged("after the re-verification", u, "suspended"); *s.M3Version != 1 {
		t.Errorf("m3_version = %d, want the lower number module 3 now reports (1) recorded", *s.M3Version)
	}
	if n := entObsLines(logs, "level=WARN", "went BACKWARDS", "was=9", "now=1"); n != 1 {
		t.Errorf("%d warnings that the counter went backwards, want exactly 1 naming 9 and 1:\n%s", n, logs.String())
	}

	// the control: a counter that did not go backwards logs nothing more
	e.exec("UPDATE platform.m3_sync SET m3_version = 0, observed_at = now() - interval '2 hours' WHERE user_id = $1", u)
	if rep := e.run(r); rep.Converged != 1 {
		t.Fatalf("the second re-verification: %+v", rep)
	}
	if n := entObsLines(logs, "went BACKWARDS"); n != 1 {
		t.Errorf("%d warnings after a counter that rose from 0 to 1, want still 1", n)
	}
}

// ---- 3. the backfill -------------------------------------------------------------------------------------------------------------------------------------------------------

// TestEntObserved_TheBackfillGivesAConvergedRowItsVersionAndChangesNothingElse: platform_0008's one-time backfill, run on populated rows: it is rolled back (the down drops the columns), the table is left in the
// platform_0006 shape with rows in it, and the migration is applied again. Every row that has converged at least once gets observed_version = converged_version, so every converged row satisfies the new CHECK
// (the ADD CONSTRAINT validates it); a row that never converged keeps NULL (there is no version to claim), m3_version is NULL everywhere (no read-back ever existed), and no other column of any row moves.
func TestEntObserved_TheBackfillGivesAConvergedRowItsVersionAndChangesNothingElse(t *testing.T) {
	e := newEntEnv(t, 4)
	conv, susp, behind, never := entUser(0), entUser(1), entUser(2), entUser(3)
	converge := func(u, state string) {
		e.exec("UPDATE platform.m3_sync SET observed = $2, observed_at = now(), observed_version = want_version, m3_version = 0, converged_version = want_version WHERE user_id = $1", u, state)
	}
	e.mustEnable(conv, nil)
	converge(conv, "active")
	e.mustDisable(susp)
	converge(susp, "suspended")
	e.mustEnable(behind, nil)
	converge(behind, "active")
	e.mustDisable(behind) // version 2 wanted suspended, converged at 1: behind
	e.mustEnable(never, nil)

	const snapshot = `SELECT string_agg(row(user_id, want_suspended, want_version, converged_version, observed, observed_at, attempts, last_attempt_at, last_error, updated_at)::text, E'\n' ORDER BY user_id) FROM platform.m3_sync`
	want := e.str(snapshot)
	if strings.Count(want, "\n") != 3 {
		t.Fatalf("setup: the snapshot of the four delivery rows is %q", want)
	}

	dir := repoMigrationsDir(t)
	envDown := migEnv(e.s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	// the chain has grown above 0008 (platform_0009 to platform_0016 do not touch the delivery table): the runner rolls back highest-first, so they go first
	for _, v := range []string{"0016", "0015", "0014", "0013", "0012", "0011", "0010", "0009"} {
		mustExit(t, runMigrator(t, envDown, "down", v), 0, "down "+v+" on a database with delivery rows")
	}
	res := runMigrator(t, envDown, "down", "0008")
	mustExit(t, res, 0, "down 0008 on a database with delivery rows")
	// the control: the table is in the platform_0006 shape with its rows (the old CHECK, no new columns), and nothing else of any row moved
	if n := e.int("SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.m3_sync'::regclass AND attname::text IN ('observed_version', 'm3_version') AND NOT attisdropped"); n != 0 {
		t.Fatalf("after down 0008 the table still has %d of the two new columns", n)
	}
	if n := e.int("SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.m3_sync'::regclass AND conname::text = 'm3_sync_converged_only_when_observed'"); n != 1 {
		t.Fatalf("after down 0008 the platform_0006 CHECK is back %d times, want 1", n)
	}
	if got := e.str(snapshot); got != want {
		t.Fatalf("down 0008 changed the delivery rows:\n  before %q\n  after  %q", want, got)
	}

	res = runMigrator(t, migEnv(e.s.DSN, dir, nil), "up")
	mustExit(t, res, 0, "up (0008 again) on a database with delivery rows")
	if got := e.str(snapshot); got != want {
		t.Fatalf("up 0008 changed a column other than the two it adds:\n  before %q\n  after  %q", want, got)
	}
	version := func(u string) string {
		return e.str("SELECT COALESCE(observed_version::text, 'NULL') FROM platform.m3_sync WHERE user_id = $1", u)
	}
	for u, wantVersion := range map[string]string{conv: "1", susp: "1", behind: "1", never: "NULL"} {
		if got := version(u); got != wantVersion {
			t.Errorf("observed_version of %s is %s after the backfill, want %s", u, got, wantVersion)
		}
	}
	if n := e.int("SELECT count(*) FROM platform.m3_sync WHERE m3_version IS NOT NULL"); n != 0 {
		t.Errorf("%d rows have an m3_version after the backfill, want none (no read-back existed before platform_0008)", n)
	}
	if n := e.int("SELECT count(*) FROM platform.m3_sync WHERE converged_version = want_version AND observed_version IS DISTINCT FROM want_version"); n != 0 {
		t.Errorf("%d converged rows do not carry the version they converged at", n)
	}
	if n := e.int("SELECT count(*) FROM platform.m3_sync WHERE converged_version = want_version"); n != 2 {
		t.Errorf("%d converged rows, want 2 (the control: the check above looked at something)", n)
	}
	if n := e.int("SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.m3_sync'::regclass AND conname::text = 'm3_sync_converged_only_when_observed_at_version' AND convalidated"); n != 1 {
		t.Errorf("the replacement CHECK is not there and validated after up")
	}
}

// ---- 4. the real module 3 --------------------------------------------------------------------------------------------------------------------------------------------------

// entObsRoute is what module 3's suspension read route answered, decoded from the exact five keys.
type entObsRoute struct {
	UserID       string  `json:"user_id"`
	Suspended    bool    `json:"suspended"`
	SuspendedAt  *string `json:"suspended_at"`
	ReinstatedAt *string `json:"reinstated_at"`
	Version      int64   `json:"version"`
}

// entObsRead queries module 3's suspension route itself, with the admin key the platform uses, and requires exactly the five documented keys.
func (r *m3lRun) entObsRead(t *testing.T, user string) entObsRoute {
	t.Helper()
	status, body := r.env.do(t, r.env.m3URL, http.MethodGet, "/v1/admin/users/"+user+"/suspension", r.env.adminKey, nil)
	r.env.expect(t, "GET the suspension of "+user, status, body, http.StatusOK, "")
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("the suspension route answered a body that is not a JSON object (%v): %s", err, r.env.snip(body))
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "reinstated_at,suspended,suspended_at,user_id,version" {
		t.Fatalf("the suspension route answered the keys %v, want exactly user_id, suspended, suspended_at, reinstated_at and version: %s", names, r.env.snip(body))
	}
	var out entObsRoute
	if err := json.Unmarshal(body, &out); err != nil || out.UserID != user {
		t.Fatalf("the suspension route's body does not decode for %s (%v): %s", user, err, r.env.snip(body))
	}
	return out
}

// TestM3Live_TheReconcileReadsModule3BackAndRecordsTheVersionModule3Reports: against the REAL module 3 (the platform service's own expiry and reconcile loops act on it, as in production). An entitlement is
// enabled, expires, and is enabled again; at each convergence the platform's row must say what module 3 ITSELF says at that moment: observed_version = want_version, m3_version = the version of
// GET /v1/admin/users/{id}/suspension queried here with the same admin key (and of module 3's own table), and the state the route reports. The counter is 0 for a user module 3 never suspended, 1 after the
// suspension, 2 after the reinstatement: it is not a constant, so the equalities mean something. Controls: the route judges its credential (the service token is a 401) and its user (an unknown one is a 404).
func TestM3Live_TheReconcileReadsModule3BackAndRecordsTheVersionModule3Reports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	r := m3lNewRun(t, ctx)

	// preconditions: an admin, the columns this reads, the route mounted and judging
	err := r.db.QueryRow(ctx, "SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at LIMIT 1").Scan(&r.admin)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("platform.admins has no active admin: the first-admin stage must have run before this test")
	}
	if err != nil {
		t.Fatalf("read platform.admins: %v", err)
	}
	r.requireColumns(t, "send_byo", "user_suspensions", "user_id", "suspended", "suspended_at", "reinstated_at", "version")
	nobody := r.uuid(t)
	status, body := r.env.do(t, r.env.m3URL, http.MethodGet, "/v1/admin/users/"+nobody+"/suspension", r.env.adminKey, nil)
	r.env.expect(t, "control: the suspension route for a user module 3 does not know", status, body, http.StatusNotFound, "not_found")
	status, body = r.env.do(t, r.env.m3URL, http.MethodGet, "/v1/admin/users/"+nobody+"/suspension", r.env.token, nil)
	r.env.expect(t, "control: the SERVICE token presented to the suspension route (it is an admin route and must reject a credential that is not the admin key)", status, body, http.StatusUnauthorized, "unauthenticated")

	// a user module 3 knows (a public.users row): the entitlement under test
	tn := &m3lTenant{Name: "obs", UserID: r.uuid(t)}
	email := fmt.Sprintf("m3obs-%s@example.test", r.runID)
	if _, err := r.db.Exec(ctx, "INSERT INTO auth.users (id, email) VALUES ($1::uuid, $2)", tn.UserID, email); err != nil {
		t.Fatalf("seed auth.users: %v", err)
	}
	if _, err := r.db.Exec(ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)", tn.UserID, email); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	tableVersion := func() int64 {
		var v int64
		err := r.db.QueryRow(ctx, "SELECT version FROM send_byo.user_suspensions WHERE user_id = $1::uuid", tn.UserID).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0 // no row: never suspended
		}
		if err != nil {
			t.Fatalf("read send_byo.user_suspensions.version: %v", err)
		}
		return v
	}
	// agrees checks the platform's row against module 3, read just now through the route and through module 3's own table
	agrees := func(stage string, st entitlements.SyncState, wantVersion, wantCounter int64, wantSuspended bool) entObsRoute {
		t.Helper()
		route := r.entObsRead(t, tn.UserID)
		if st.ObservedVersion == nil || *st.ObservedVersion != wantVersion || st.WantVersion != wantVersion {
			t.Fatalf("%s: the platform's row is %s: want observed_version = want_version = %d", stage, r.syncString(st), wantVersion)
		}
		if st.M3Version == nil || *st.M3Version != route.Version || route.Version != wantCounter || tableVersion() != route.Version {
			t.Fatalf("%s: the platform recorded m3_version %s, module 3's route says %d, module 3's own table says %d, want all three equal to %d", stage, entObsStr(st.M3Version), route.Version, tableVersion(), wantCounter)
		}
		if route.Suspended != wantSuspended || st.Observed != map[bool]string{true: "suspended", false: "active"}[wantSuspended] {
			t.Fatalf("%s: module 3's route says suspended=%v and the platform observed %q, want suspended=%v", stage, route.Suspended, st.Observed, wantSuspended)
		}
		return route
	}

	// 1. enabled for an hour: delivered (module 3 never suspended this user: active, version 0, no instants)
	until := r.dbNow(t).Add(time.Hour)
	r.mustEnable(t, tn, &until, "enable")
	st1 := r.waitSync(t, tn, 1, false, "active", 90*time.Second)
	route := agrees("after the enable", st1, 1, 0, false)
	if route.SuspendedAt != nil || route.ReinstatedAt != nil {
		t.Errorf("a user module 3 never suspended reads suspended_at %v and reinstated_at %v, want null and null", route.SuspendedAt, route.ReinstatedAt)
	}

	// 2. the date is moved a few seconds ahead; when it passes, the service expires the entitlement and delivers the suspension
	soon := r.dbNow(t).Add(m3lExpiryIn)
	r.mustEnable(t, tn, &soon, "redate")
	st2 := r.waitSync(t, tn, 2, true, "suspended", 120*time.Second)
	route = agrees("after the expiry", st2, 2, 1, true)
	if route.SuspendedAt == nil {
		t.Errorf("a suspended user reads suspended_at null")
	}

	// 3. enabled again: module 3 reads suspended=false and the platform converges again at a higher version, with module 3's counter at 2
	r.mustEnable(t, tn, nil, "reenable")
	st3 := r.waitSync(t, tn, 3, false, "active", 120*time.Second)
	route = agrees("after the re-enable", st3, 3, 2, false)
	if route.SuspendedAt == nil || route.ReinstatedAt == nil {
		t.Errorf("a reinstated user reads suspended_at %v and reinstated_at %v, want both set", route.SuspendedAt, route.ReinstatedAt)
	}
	if got := []int64{*st1.M3Version, *st2.M3Version, *st3.M3Version}; got[0] >= got[1] || got[1] >= got[2] {
		t.Errorf("the counters the platform recorded are %v, want strictly rising (a constant would make every equality above vacuous)", got)
	}
	t.Logf("module 3's own read-back: converged at version 1 (observed active, m3_version %d), version 2 (observed suspended, m3_version %d), version 3 (observed active, m3_version %d); each equal to GET /v1/admin/users/{id}/suspension and to send_byo.user_suspensions.version at that moment", *st1.M3Version, *st2.M3Version, *st3.M3Version)
}
