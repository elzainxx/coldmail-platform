//go:build harness

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/m3admin"
)

// The reconciler against a REAL PostgreSQL, with a FAKE module 3. These tests prove the DATABASE bookkeeping: what the reconciler writes to platform.m3_sync as platform_app (and nothing
// else), when it refuses to write, and that it never records a convergence module 3 did not confirm. The real module 3 is exercised by the harness's own chunks (suspend, halt, resume,
// module 3 down at expiry); a fake is the right instrument HERE because it can answer wrongly, slowly, or not at all on command. Helpers come from entitlements_store_test.go.

// entCanary stands for module 3's admin key: it must never appear in a stored error, a report or a log.
const entCanary = "CANARY-ent-m3-admin-key-5521"

type entCall struct {
	User      string
	Suspended bool
	Reason    string
}

// entFake is a Suspender whose answers the test controls. By default it confirms the wanted state as a change.
type entFake struct {
	mu     sync.Mutex
	calls  []entCall
	answer func(ctx context.Context, n int, c entCall) (m3admin.Result, error)
}

func (f *entFake) SetSuspended(ctx context.Context, user string, suspended bool, reason string) (m3admin.Result, error) {
	f.mu.Lock()
	c := entCall{user, suspended, reason}
	f.calls = append(f.calls, c)
	n, fn := len(f.calls), f.answer
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, n, c)
	}
	return entEcho(suspended, true), nil
}

// entEcho is the confirmation a healthy module 3 gives: the state asked for.
func entEcho(suspended, changed bool) m3admin.Result {
	st, code := m3admin.StateActive, 422
	if suspended {
		st = m3admin.StateSuspended
	}
	if changed {
		code = 200
	}
	return m3admin.Result{State: st, Changed: changed, HTTPStatus: code}
}

func (f *entFake) set(fn func(ctx context.Context, n int, c entCall) (m3admin.Result, error)) {
	f.mu.Lock()
	f.answer = fn
	f.mu.Unlock()
}

func (f *entFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *entFake) called() []entCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]entCall(nil), f.calls...)
}

// entFailWith answers every call with the same error.
func entFailWith(err error) func(context.Context, int, entCall) (m3admin.Result, error) {
	return func(context.Context, int, entCall) (m3admin.Result, error) { return m3admin.Result{}, err }
}

// entAnswer answers every call with the same Result and no error.
func entAnswer(res m3admin.Result) func(context.Context, int, entCall) (m3admin.Result, error) {
	return func(context.Context, int, entCall) (m3admin.Result, error) { return res, nil }
}

type entLogs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *entLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *entLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// entClock is the reconciler's clock for BACKOFF arithmetic, which the tests move by hand.
type entClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *entClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *entClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// reconciler builds a reconciler over the platform_app pool with no retry delay and no re-verification; mutate changes that.
func (e *entEnv) reconciler(m3 entitlements.Suspender, mutate func(*entitlements.Reconciler)) (*entitlements.Reconciler, *entLogs) {
	logs := &entLogs{}
	r := &entitlements.Reconciler{
		Pool: e.pool, M3: m3, Redact: []string{entCanary},
		Log:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Backoff: func(int) time.Duration { return 0 },
	}
	if mutate != nil {
		mutate(r)
	}
	return r, logs
}

// run makes one pass and fails the test on a reconciler error or on a report whose counters do not add up.
func (e *entEnv) run(r *entitlements.Reconciler) entitlements.Report {
	e.t.Helper()
	rep, err := r.RunOnce(testCtx(e.t))
	if err != nil {
		e.t.Fatalf("RunOnce: %v", err)
	}
	if !rep.Balanced() {
		e.t.Fatalf("the report does not balance (every attempt must end as converged, failed or stale): %+v", rep)
	}
	return rep
}

// requireUnconverged fails unless the delivery row is exactly "module 3 not seen in the wanted state": converged_version below want_version.
func (e *entEnv) requireBehind(when, user string) entSync {
	e.t.Helper()
	s := e.syncOf(user)
	if s.ConvergedVers >= s.WantVersion {
		e.t.Fatalf("%s: converged_version %d is not behind want_version %d: a convergence was recorded that module 3 did not confirm", when, s.ConvergedVers, s.WantVersion)
	}
	return s
}

func (e *entEnv) requireConverged(when, user, observed string) entSync {
	e.t.Helper()
	s := e.syncOf(user)
	if s.ConvergedVers != s.WantVersion || s.Observed != observed || s.ObservedAt == nil || s.Attempts != 0 || s.LastError != nil {
		e.t.Fatalf("%s: delivery row %+v, want converged at want_version with observed %q, an observation time, no attempts and no error", when, s, observed)
	}
	return s
}

// ---- convergence ---------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestEntReconcile_ConvergesAUserWhoseWantIsSuspendedAndRecordsWhatModule3Answered(t *testing.T) {
	e := newEntEnv(t, 3)
	disabled, enabled, expired := entUser(0), entUser(1), entUser(2)
	e.mustDisable(disabled)
	e.mustEnable(enabled, nil)
	e.backdatedEnable(expired, 30)
	if got, err := e.store.ExpireDue(testCtx(t), 10); err != nil || len(got) != 1 || got[0].UserID != expired {
		t.Fatalf("setup: ExpireDue = %v, %v", got, err)
	}
	for _, u := range []string{disabled, enabled, expired} {
		if s := e.requireBehind("before the first pass", u); s.Observed != "unknown" || s.ObservedAt != nil {
			t.Fatalf("before the first pass %s has been observed: %+v", u, s)
		}
	}

	fake := &entFake{}
	r, _ := e.reconciler(fake, nil)
	rep := e.run(r)
	if rep.Attempted != 3 || rep.Converged != 3 || rep.Failed != 0 || rep.StaleSkipped != 0 || rep.Drifted != 0 || rep.ConfigFault || rep.Unconverged != 0 || len(rep.Errors) != 0 {
		t.Fatalf("report = %+v, want three conversions, no failure, no drift (a first delivery that changes module 3 is not drift), nothing unconverged", rep)
	}

	byUser := map[string]entCall{}
	for _, c := range fake.called() {
		byUser[c.User] = c
	}
	for u, want := range map[string]entCall{
		disabled: {disabled, true, "platform entitlement: disabled"},
		enabled:  {enabled, false, "platform entitlement: enabled"},
		expired:  {expired, true, "platform entitlement: expired"},
	} {
		if byUser[u] != want {
			t.Errorf("the call for %s was %+v, want %+v", u, byUser[u], want)
		}
	}
	e.requireConverged("the disabled user", disabled, "suspended")
	e.requireConverged("the enabled user", enabled, "active")
	s := e.requireConverged("the expired user", expired, "suspended")
	if s.WantVersion != 2 {
		t.Errorf("the expired user's want_version is %d, want 2 (enable, then expiry)", s.WantVersion)
	}
	if st, err := e.store.Sync(testCtx(t), disabled); err != nil || !st.Converged() {
		t.Errorf("Sync(%s) = %+v, %v, want Converged()", disabled, st, err)
	}

	// Paired control: with everything converged and no re-verification a second pass calls nobody.
	if rep := e.run(r); rep.Attempted != 0 || rep.Unconverged != 0 || fake.count() != 3 {
		t.Errorf("a pass over a converged database made %d attempts and %d calls in all, want none beyond the first three", rep.Attempted, fake.count())
	}
}

func TestEntReconcile_ANetworkFailureNeverRecordsSuccessAndTheNextPassAfterTheBackoffRetries(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u)
	clock := &entClock{t: e.dbNow()}
	fake := &entFake{}
	fake.set(entFailWith(fmt.Errorf("Post \"https://op:%s@m3.internal:8080/v1/admin/users/x/suspend\": dial tcp 10.0.0.9:8080: connect: connection refused (Authorization: Bearer %s)", entCanary, entCanary)))
	r, logs := e.reconciler(fake, func(r *entitlements.Reconciler) {
		r.Now = clock.now
		r.Backoff = func(int) time.Duration { return 10 * time.Second }
	})

	// pass 1: the call fails. Nothing is recorded as converged or observed; the failure is recorded, scrubbed.
	rep := e.run(r)
	if rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || rep.ConfigFault || rep.Unconverged != 1 || len(rep.Errors) != 1 {
		t.Fatalf("pass 1 report = %+v, want one failed attempt and one unconverged user", rep)
	}
	s := e.requireBehind("after a failed call", u)
	if s.Observed != "unknown" || s.ObservedAt != nil || s.Attempts != 1 || s.LastAttemptAt == nil || s.LastError == nil {
		t.Fatalf("after a failed call the row is %+v, want observed unknown, no observation time, one attempt, a last_error", s)
	}
	if strings.Contains(*s.LastError, entCanary) || strings.Contains(rep.Errors[0], entCanary) || strings.Contains(logs.String(), entCanary) {
		t.Errorf("the admin key reached the stored error, the report or the log:\n%s\n%s\n%s", *s.LastError, rep.Errors[0], logs.String())
	}
	if len(*s.LastError) > entitlements.MaxErrorBytes || !strings.Contains(*s.LastError, "connection refused") {
		t.Errorf("last_error = %q (%d bytes), want the cause kept and the text capped at %d", *s.LastError, len(*s.LastError), entitlements.MaxErrorBytes)
	}
	if !strings.Contains(rep.Errors[0], u) {
		t.Errorf("the report error %q does not name the user", rep.Errors[0])
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "module 3 call failed") {
		t.Errorf("the failure was not logged at warn level:\n%s", logs.String())
	}

	// pass 2, the clock unmoved: the retry delay holds the row back, and module 3 is not called (the control: the delay is real).
	rep = e.run(r)
	if rep.Attempted != 0 || rep.BackoffSkipped != 1 || fake.count() != 1 {
		t.Fatalf("pass 2 report = %+v with %d calls, want the row held back by its retry delay", rep, fake.count())
	}
	if after := e.syncOf(u); after.Attempts != 1 {
		t.Fatalf("a pass that skipped the row changed its attempts to %d", after.Attempts)
	}

	// pass 3, after the delay: it retries, fails again, and the attempts count it; still nothing converged.
	clock.advance(time.Hour)
	rep = e.run(r)
	if rep.Attempted != 1 || rep.Failed != 1 || fake.count() != 2 {
		t.Fatalf("pass 3 report = %+v with %d calls, want a second failed attempt", rep, fake.count())
	}
	if s := e.requireBehind("after the second failed call", u); s.Attempts != 2 || s.Observed != "unknown" {
		t.Fatalf("after the second failure the row is %+v, want 2 attempts, still observed unknown", s)
	}

	// module 3 recovers: the next pass after the delay converges, records what module 3 answered, and clears the failure.
	fake.set(nil)
	clock.advance(time.Hour)
	rep = e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.Failed != 0 || rep.Unconverged != 0 {
		t.Fatalf("pass 4 report = %+v, want the user converged", rep)
	}
	e.requireConverged("after module 3 recovered", u, "suspended")
}

func TestEntReconcile_AWrongAnswerIsAFailureNotAConvergence(t *testing.T) {
	e := newEntEnv(t, 3)
	u, bogus, back := entUser(0), entUser(1), entUser(2)
	for _, x := range []string{u, bogus, back} {
		e.mustDisable(x) // all want suspended
	}
	fake := &entFake{}
	fake.set(func(_ context.Context, _ int, c entCall) (m3admin.Result, error) {
		switch c.User {
		case u:
			return entEcho(false, true), nil // asked to suspend, says ACTIVE
		case bogus:
			return m3admin.Result{State: "weird", Changed: true, HTTPStatus: 200}, nil
		}
		return entEcho(c.Suspended, true), nil // the control user: answers correctly
	})
	r, logs := e.reconciler(fake, nil)

	rep := e.run(r)
	if rep.Attempted != 3 || rep.Converged != 1 || rep.Failed != 2 || rep.Unconverged != 2 {
		t.Fatalf("report = %+v, want exactly the correctly answered user converged", rep)
	}
	// asked to suspend, module 3 said active: recorded as what module 3 said, never as converged
	s := e.requireBehind("a wrong answer", u)
	if s.Observed != "active" || s.Attempts != 1 || s.LastError == nil || !strings.Contains(*s.LastError, "not trusted") || !strings.Contains(*s.LastError, `"active"`) || !strings.Contains(*s.LastError, `"suspended"`) {
		t.Errorf("after a wrong answer the row is %+v, want observed active (module 3's own answer), one attempt, and an error naming both states", s)
	}
	if st, _ := e.store.Sync(testCtx(t), u); st.Converged() {
		t.Error("the store reports a user module 3 contradicted as converged")
	}
	// an answer outside the vocabulary cannot even be recorded as observed: it is a plain failure and observed stays unknown
	s = e.requireBehind("an unrecognised answer", bogus)
	if s.Observed != "unknown" || s.ObservedAt != nil || s.Attempts != 1 || s.LastError == nil || strings.Contains(*s.LastError, "weird") {
		t.Errorf("after an unrecognised answer the row is %+v, want observed unknown, one attempt, and an error that does not echo the answer", s)
	}
	e.requireConverged("the control user", back, "suspended")
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "not trusted") {
		t.Errorf("a contradicting answer was not logged as an error:\n%s", logs.String())
	}

	// module 3 then answers correctly: both converge, the failure is cleared (the paired control for the rows above).
	fake.set(nil)
	rep = e.run(r)
	if rep.Converged != 2 || rep.Failed != 0 || rep.Unconverged != 0 {
		t.Fatalf("after module 3 answered correctly: %+v", rep)
	}
	e.requireConverged("after the correct answer", u, "suspended")
	e.requireConverged("after the correct answer", bogus, "suspended")
}

func TestEntReconcile_AConvergedUserWhomModule3ContradictsIsTakenBackToNotConverged(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u)
	fake := &entFake{}
	r, _ := e.reconciler(fake, func(r *entitlements.Reconciler) { r.ReverifyEvery = time.Hour })
	e.run(r)
	e.requireConverged("first", u, "suspended")

	// the observation grows old; module 3 now says the user is ACTIVE although the platform wants it suspended
	e.exec("UPDATE platform.m3_sync SET observed_at = now() - interval '2 hours' WHERE user_id = $1", u)
	fake.set(entAnswer(entEcho(false, true)))
	rep := e.run(r)
	if rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || rep.Unconverged != 1 {
		t.Fatalf("report = %+v, want the re-verification counted as a failure and the user unconverged", rep)
	}
	s := e.requireBehind("after module 3 contradicted a converged user", u)
	if s.ConvergedVers != s.WantVersion-1 || s.Observed != "active" || s.Attempts != 1 {
		t.Errorf("row = %+v, want converged_version lowered to want_version-1, observed active (what module 3 said), one attempt", s)
	}
	// ... and the normal loop then puts it right: module 3 answers correctly, the user converges again (not a drift: it is the ordinary path).
	fake.set(nil)
	rep = e.run(r)
	if rep.Converged != 1 || rep.Drifted != 0 || rep.Unconverged != 0 {
		t.Fatalf("after module 3 recovered: %+v", rep)
	}
	e.requireConverged("after recovery", u, "suspended")
}

// ---- the compare-and-set --------------------------------------------------------------------------------------------------------------------------------------------------

func TestEntReconcile_AStaleAnswerNeverMarksTheNewVersionConverged(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u) // want suspended, version 1

	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	fake := &entFake{}
	fake.set(func(_ context.Context, n int, c entCall) (m3admin.Result, error) {
		if n == 1 {
			entered <- struct{}{}
			<-gate // module 3 is "slow": the entitlement changes while this call is in flight
		}
		return entEcho(c.Suspended, true), nil
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
		t.Fatal("the reconciler never called module 3")
	}
	// while the call for version 1 is in flight an admin re-enables the user: want_suspended false, version 2
	e.mustEnable(u, nil)
	if s := e.syncOf(u); s.WantVersion != 2 || s.WantSuspended {
		t.Fatalf("setup: the delivery row is %+v, want version 2 and not suspended", s)
	}
	close(gate)

	res := <-done
	if res.err != nil {
		t.Fatalf("RunOnce: %v", res.err)
	}
	if !res.rep.Balanced() || res.rep.Attempted != 1 || res.rep.StaleSkipped != 1 || res.rep.Converged != 0 || res.rep.Failed != 0 {
		t.Fatalf("report = %+v, want the answer for version 1 counted as stale and nothing converged", res.rep)
	}
	// Nothing of the stale answer reached the row: not converged, not observed, not an attempt.
	s := e.requireBehind("after a stale answer", u)
	if s.WantVersion != 2 || s.WantSuspended || s.ConvergedVers != 0 || s.Observed != "unknown" || s.ObservedAt != nil || s.Attempts != 0 || s.LastError != nil || s.LastAttemptAt != nil {
		t.Fatalf("the stale answer was recorded: %+v", s)
	}

	// The next pass delivers the NEW version and converges it with module 3's answer for it (the control: the guard refused only the stale one).
	rep := e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.StaleSkipped != 0 {
		t.Fatalf("the next pass: %+v", rep)
	}
	if calls := fake.called(); len(calls) != 2 || !calls[0].Suspended || calls[1].Suspended {
		t.Errorf("the calls were %+v, want a suspend for version 1 and then an unsuspend for version 2", calls)
	}
	if s := e.requireConverged("version 2", u, "active"); s.WantVersion != 2 {
		t.Errorf("converged at %d, want 2", s.WantVersion)
	}
}

func TestEntReconcile_TheDatabaseRefusesAFalseConvergenceAndTheColumnsOnlyTheTriggerWrites(t *testing.T) {
	e := newEntEnv(t, 2)
	u := entUser(0)
	ctx := testCtx(t)
	e.mustDisable(u) // want suspended, version 1, observed unknown
	app := func(sql string, args ...any) error {
		_, err := e.pool.Exec(ctx, sql, args...)
		return err
	}
	refused := func(what string, err error, code, constraint string) {
		t.Helper()
		pe := entPgError(t, what, err)
		if pe.Code != code || (constraint != "" && pe.ConstraintName != constraint) {
			t.Errorf("%s: the database said %s / %q, want %s / %q", what, pe.Code, pe.ConstraintName, code, constraint)
		}
	}

	// the second wall: converged_version = want_version needs observed = the wanted state
	refused("converged with observed unknown", app("UPDATE platform.m3_sync SET converged_version = want_version WHERE user_id = $1", u), "23514", "m3_sync_converged_only_when_observed")
	refused("converged with the OTHER state observed", app("UPDATE platform.m3_sync SET observed = 'active', converged_version = want_version WHERE user_id = $1", u), "23514", "m3_sync_converged_only_when_observed")
	refused("an observation outside the vocabulary", app("UPDATE platform.m3_sync SET observed = 'weird' WHERE user_id = $1", u), "23514", "m3_sync_observed_vocabulary")
	refused("converged beyond the wanted version", app("UPDATE platform.m3_sync SET observed = 'suspended', converged_version = want_version + 1 WHERE user_id = $1", u), "23514", "m3_sync_converged_not_ahead")
	// the control: the right observation is accepted
	if err := app("UPDATE platform.m3_sync SET observed = 'suspended', observed_at = now(), converged_version = want_version WHERE user_id = $1", u); err != nil {
		t.Fatalf("control failed: a convergence with the wanted state observed was refused: %v", err)
	}
	if s := e.syncOf(u); s.ConvergedVers != 1 || s.Observed != "suspended" {
		t.Fatalf("the control did not write: %+v", s)
	}

	// the wanted state belongs to the trigger: the application role cannot write it, nor insert or delete a delivery row, nor write an entitlement
	for what, sql := range map[string]string{
		"want_suspended":  "UPDATE platform.m3_sync SET want_suspended = NOT want_suspended WHERE user_id = $1",
		"want_version":    "UPDATE platform.m3_sync SET want_version = want_version + 1 WHERE user_id = $1",
		"user_id":         "UPDATE platform.m3_sync SET user_id = user_id WHERE user_id = $1",
		"a delivery row":  "INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES ($1, true)",
		"delete":          "DELETE FROM platform.m3_sync WHERE user_id = $1",
		"entitlement":     "UPDATE platform.m3_entitlements SET enabled = true WHERE user_id = $1",
		"entitlement row": "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)",
	} {
		refused("platform_app: "+what, app(sql, u), "42501", "")
	}
	if s := e.syncOf(u); s.WantVersion != 1 || !s.WantSuspended {
		t.Errorf("a refused write changed the wanted state: %+v", s)
	}
}

// ---- re-verification ------------------------------------------------------------------------------------------------------------------------------------------------------

func TestEntReconcile_ReverificationResendsOnlyOldObservationsAndReportsDrift(t *testing.T) {
	e := newEntEnv(t, 2)
	u, idle := entUser(0), entUser(1)
	e.mustDisable(u)
	fake := &entFake{}
	r, logs := e.reconciler(fake, func(r *entitlements.Reconciler) { r.ReverifyEvery = time.Hour })
	stale := func() {
		e.exec("UPDATE platform.m3_sync SET observed_at = now() - interval '2 hours' WHERE user_id = $1", u)
	}

	// the first delivery changes module 3 (Changed true) and is NOT drift
	rep := e.run(r)
	if rep.Converged != 1 || rep.Drifted != 0 {
		t.Fatalf("first pass: %+v, want one convergence and no drift", rep)
	}
	// a recent observation is not re-sent (the control: the re-verification waits for the age)
	if rep := e.run(r); rep.Attempted != 0 || fake.count() != 1 {
		t.Fatalf("a pass over a freshly observed user made %d attempts and %d calls in all, want none beyond the first", rep.Attempted, fake.count())
	}

	// an old observation is re-sent; module 3 still holds the state (422 conflict: Changed false): confirmed, not drift, observation refreshed
	stale()
	fake.set(func(_ context.Context, _ int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, false), nil
	})
	rep = e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.Drifted != 0 || fake.count() != 2 {
		t.Fatalf("re-verification with an unchanged module 3: %+v, %d calls", rep, fake.count())
	}
	if c := fake.called()[1]; c.User != u || !c.Suspended || c.Reason != "platform entitlement: disabled (re-verify)" {
		t.Errorf("the re-verification call was %+v, want the same suspend with a re-verify note", c)
	}
	e.requireConverged("after an unchanged re-verification", u, "suspended")
	if n := e.int("SELECT count(*) FROM platform.m3_sync WHERE user_id = $1 AND observed_at > now() - interval '1 minute'", u); n != 1 {
		t.Error("the observation time was not refreshed by the re-verification")
	}

	// an old observation, and module 3 CHANGED the state on this call: it had drifted. Reported, logged at warn, and corrected (the row stays converged).
	stale()
	fake.set(func(_ context.Context, _ int, c entCall) (m3admin.Result, error) {
		return entEcho(c.Suspended, true), nil
	})
	rep = e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.Drifted != 1 || len(rep.DriftedUsers) != 1 || rep.DriftedUsers[0] != u {
		t.Fatalf("re-verification that found module 3 drifted: %+v", rep)
	}
	e.requireConverged("after the drift was corrected", u, "suspended")
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "had drifted") || !strings.Contains(logs.String(), u) {
		t.Errorf("the drift was not logged at warn level naming the user:\n%s", logs.String())
	}

	// a user with no entitlement is never touched, and a reconciler with re-verification off never re-sends an old observation
	if fake.count() != 3 {
		t.Errorf("%d calls in all, want 3 (no call for the user who has no entitlement)", fake.count())
	}
	_ = idle
	off, _ := e.reconciler(fake, nil) // ReverifyEvery zero
	stale()
	if rep := e.run(off); rep.Attempted != 0 || fake.count() != 3 {
		t.Errorf("with re-verification off a pass made %d attempts and %d calls in all, want none beyond the three", rep.Attempted, fake.count())
	}
}

// ---- a rejected key, the pace, the batch --------------------------------------------------------------------------------------------------------------------------------------

func TestEntReconcile_ARejectedAdminKeyIsAConfigFaultThatStopsThePassAndKeepsNoSecret(t *testing.T) {
	e := newEntEnv(t, 3)
	users := []string{entUser(0), entUser(1), entUser(2)}
	for _, u := range users {
		e.mustDisable(u) // updated_at ascends: the pass takes them in this order
	}
	fake := &entFake{}
	fake.set(entFailWith(fmt.Errorf("suspend: %w (the key was %s)", m3admin.ErrUnauthorized, entCanary)))
	r, logs := e.reconciler(fake, nil)

	rep := e.run(r)
	if !rep.ConfigFault || rep.Attempted != 1 || rep.Failed != 1 || rep.Converged != 0 || fake.count() != 1 || rep.Unconverged != 3 {
		t.Fatalf("report = %+v with %d calls, want a config fault after ONE call (the key is wrong for every user; the pass must not hammer module 3's bcrypt check)", rep, fake.count())
	}
	first := e.requireBehind("the first user", users[0])
	if first.Attempts != 1 || first.Observed != "unknown" || first.LastError == nil || !strings.Contains(*first.LastError, "PLATFORM_M3_ADMIN_TOKEN") {
		t.Errorf("the first user's row is %+v, want one attempt, observed unknown and an error naming the variable", first)
	}
	for _, u := range users[1:] {
		if s := e.syncOf(u); s.Attempts != 0 || s.LastError != nil || s.LastAttemptAt != nil {
			t.Errorf("%s was recorded although the pass stopped before it: %+v", u, s)
		}
	}
	stored := ""
	if first.LastError != nil {
		stored = *first.LastError
	}
	for what, text := range map[string]string{"the stored error": stored, "the report": strings.Join(rep.Errors, "|"), "the log": logs.String()} {
		if strings.Contains(text, entCanary) {
			t.Errorf("%s carries the admin key: %s", what, text)
		}
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "CONFIGURATION FAULT") {
		t.Errorf("the fault was not logged as an error naming a configuration fault:\n%s", logs.String())
	}

	// Paired control: an ordinary failure is NOT a config fault and does not stop the pass.
	fake.set(entFailWith(errors.New("connection reset by peer")))
	rep = e.run(r)
	if rep.ConfigFault || rep.Attempted != 3 || rep.Failed != 3 {
		t.Fatalf("with an ordinary failure the report is %+v, want three failed attempts and no config fault", rep)
	}
	// and once the key is right everyone converges
	fake.set(nil)
	if rep := e.run(r); rep.Converged != 3 || rep.Unconverged != 0 || rep.ConfigFault {
		t.Fatalf("after the key was fixed: %+v", rep)
	}
}

func TestEntReconcile_ANewWantedStateIsNotHeldBackByTheOldOnesRetryDelay(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u)
	clock := &entClock{t: e.dbNow()}
	fake := &entFake{}
	fake.set(entFailWith(errors.New("module 3 is down")))
	r, _ := e.reconciler(fake, func(r *entitlements.Reconciler) {
		r.Now = clock.now
		r.Backoff = func(int) time.Duration { return time.Hour }
	})

	if rep := e.run(r); rep.Failed != 1 {
		t.Fatalf("pass 1: %+v", rep)
	}
	// the control: the failed version IS held back for an hour
	if rep := e.run(r); rep.Attempted != 0 || rep.BackoffSkipped != 1 {
		t.Fatalf("pass 2: %+v, want the failing version held back", rep)
	}
	// an admin re-enables the user: a NEW wanted state, which has not failed at all. It is delivered at once, with no wait for the old delay.
	e.mustEnable(u, nil)
	fake.set(nil)
	rep := e.run(r)
	if rep.Attempted != 1 || rep.Converged != 1 || rep.BackoffSkipped != 0 {
		t.Fatalf("pass 3: %+v, want the new version delivered immediately", rep)
	}
	e.requireConverged("the new version", u, "active")
}

func TestEntReconcile_APassTakesAtMostOneBatchOldestChangeFirstAndTheCountsAlwaysAddUp(t *testing.T) {
	const total = 60
	e := newEntEnv(t, total)
	for i := 0; i < total; i++ {
		e.mustDisable(entUser(i))
	}
	fake := &entFake{}
	r, _ := e.reconciler(fake, nil)

	rep := e.run(r)
	if rep.Attempted != 50 || rep.Converged != 50 || rep.Unconverged != total-50 {
		t.Fatalf("pass 1: %+v, want one batch of 50 and 10 still unconverged", rep)
	}
	seen := map[string]bool{}
	for _, c := range fake.called() {
		seen[c.User] = true
	}
	for i := 0; i < 50; i++ {
		if !seen[entUser(i)] {
			t.Errorf("user %d, one of the 50 oldest changes, was not delivered in the first pass", i)
		}
	}
	for i := 50; i < total; i++ {
		if seen[entUser(i)] {
			t.Errorf("user %d was delivered in the first pass although 50 older changes were waiting", i)
		}
	}
	rep = e.run(r)
	if rep.Attempted != 10 || rep.Converged != 10 || rep.Unconverged != 0 {
		t.Fatalf("pass 2: %+v, want the last 10", rep)
	}
	if rep := e.run(r); rep.Attempted != 0 {
		t.Fatalf("pass 3: %+v, want nothing left", rep)
	}
	if n := e.int("SELECT count(*) FROM platform.m3_sync WHERE converged_version = want_version AND observed = 'suspended'"); n != total {
		t.Errorf("%d of %d rows are converged and observed suspended", n, total)
	}
}

func TestEntReconcile_ACancelledPassRecordsNothingForTheCallItInterrupted(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.mustDisable(u)
	entered := make(chan struct{}, 1)
	fake := &entFake{}
	fake.set(func(ctx context.Context, _ int, _ entCall) (m3admin.Result, error) {
		entered <- struct{}{}
		<-ctx.Done() // module 3 never answers: only the end of the pass frees the call
		return m3admin.Result{}, ctx.Err()
	})
	r, _ := e.reconciler(fake, nil)

	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	type result struct {
		rep entitlements.Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := r.RunOnce(ctx)
		done <- result{rep, err}
	}()
	<-entered
	cancel()
	res := <-done
	if res.err == nil || !errors.Is(res.err, context.Canceled) {
		t.Fatalf("RunOnce after the cancel = %v, want the context error", res.err)
	}
	if res.rep.Attempted != 0 || res.rep.Failed != 0 {
		t.Errorf("the interrupted call was counted: %+v", res.rep)
	}
	// Nothing was written for the interrupted call: its outcome is unknown, so the next pass simply repeats it.
	s := e.requireBehind("after a cancelled pass", u)
	if s.Attempts != 0 || s.LastAttemptAt != nil || s.LastError != nil || s.Observed != "unknown" {
		t.Errorf("a cancelled pass left a record: %+v", s)
	}
	fake.set(nil)
	if rep := e.run(r); rep.Converged != 1 {
		t.Fatalf("the pass after the cancel: %+v, want the user converged", rep)
	}
}

// ---- the whole flow ---------------------------------------------------------------------------------------------------------------------------------------------------------------

// TestEntFlow_AnExpiryIsSuspendedAtModule3AndOnlyConvergedAfterModule3ConfirmsIt: enable, deliver, let the date pass, expire (the store), and
// deliver the suspension while module 3 is "down" for two passes: the user stays unconverged, with the failure recorded, until module 3 answers; then converges at the expiry's version.
func TestEntFlow_AnExpiryIsSuspendedAtModule3AndOnlyConvergedAfterModule3ConfirmsIt(t *testing.T) {
	e := newEntEnv(t, 1)
	u := entUser(0)
	e.backdatedEnable(u, 30) // enabled with a date that has already passed: due at once
	fake := &entFake{}
	r, _ := e.reconciler(fake, nil)

	e.run(r) // delivers "active" (version 1)
	e.requireConverged("the enable", u, "active")

	got, err := e.store.ExpireDue(testCtx(t), 10)
	if err != nil || len(got) != 1 || got[0].UserID != u {
		t.Fatalf("ExpireDue = %v, %v", got, err)
	}
	st, _ := e.store.Sync(testCtx(t), u)
	if st.Converged() || st.WantVersion != 2 || !st.WantSuspended {
		t.Fatalf("right after the expiry the user must be wanted suspended and NOT converged: %+v", st)
	}

	// module 3 is down for two passes
	fake.set(entFailWith(errors.New("dial tcp: connection refused")))
	for pass := 1; pass <= 2; pass++ {
		rep := e.run(r)
		if rep.Failed != 1 || rep.Converged != 0 {
			t.Fatalf("down pass %d: %+v", pass, rep)
		}
		s := e.requireBehind(fmt.Sprintf("module 3 down, pass %d", pass), u)
		if s.ConvergedVers != 1 || s.Observed != "active" || s.Attempts != pass {
			t.Fatalf("down pass %d: the row is %+v, want converged_version still at the old version 1, observed still the old observation, %d attempts", pass, s, pass)
		}
	}
	// module 3 comes back: converged only now, at the expiry's version, with the state module 3 confirmed
	fake.set(nil)
	rep := e.run(r)
	if rep.Converged != 1 || rep.Unconverged != 0 {
		t.Fatalf("after module 3 came back: %+v", rep)
	}
	s := e.requireConverged("after module 3 confirmed", u, "suspended")
	if s.WantVersion != 2 {
		t.Errorf("converged at version %d, want the expiry's version 2", s.WantVersion)
	}
	calls := fake.called()
	if last := calls[len(calls)-1]; last.User != u || !last.Suspended || last.Reason != "platform entitlement: expired" {
		t.Errorf("the delivery that converged was %+v, want a suspend noted as expired", last)
	}
}
