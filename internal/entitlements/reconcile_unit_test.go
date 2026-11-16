package entitlements

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/m3admin"
)

// ---- Report arithmetic ----------------------------------------------------------------------------------------

func TestReport_Arithmetic(t *testing.T) {
	good := []Report{
		{},
		{Attempted: 3, Converged: 2, Failed: 1},
		{Attempted: 4, Converged: 1, Failed: 1, StaleSkipped: 2},
		{Attempted: 2, Converged: 2, Drifted: 1}, // a drift is a kind of convergence: it adds nothing of its own
		{Attempted: 1, StaleSkipped: 1},
		{Attempted: 5, Converged: 5, BackoffSkipped: 9, Unconverged: -1}, // skipped rows are not attempts
	}
	for _, r := range good {
		if !r.Balanced() {
			t.Errorf("%+v should balance: every attempt has exactly one outcome", r)
		}
	}
	// Paired controls: a report that lost an outcome, or counted one twice, does not balance.
	bad := []Report{
		{Attempted: 3, Converged: 2},
		{Attempted: 1, Converged: 1, Failed: 1},
		{Attempted: 0, Failed: 1},
		{Attempted: 2, Converged: 1, StaleSkipped: 2},
	}
	for _, r := range bad {
		if r.Balanced() {
			t.Errorf("%+v balances but it should not: the check cannot fail", r)
		}
	}
}

func TestReport_ErrorsAreCapped(t *testing.T) {
	var r Report
	for i := 0; i < maxReportErrors*3; i++ {
		r.addError(fmt.Sprintf("u%d", i), "boom")
	}
	if len(r.Errors) != maxReportErrors {
		t.Fatalf("Errors holds %d entries, want the cap %d", len(r.Errors), maxReportErrors)
	}
	if r.Errors[0] != "u0: boom" || !strings.HasPrefix(r.Errors[maxReportErrors-1], fmt.Sprintf("u%d:", maxReportErrors-1)) {
		t.Errorf("the cap keeps the FIRST failures: %v", r.Errors)
	}
}

// ---- judge: the one decision about what a call proves --------------------------------------------------------

func TestJudge_ConvergenceIsExactlyTheWantedState(t *testing.T) {
	answers := []m3admin.State{m3admin.StateSuspended, m3admin.StateActive, "", "bogus", "SUSPENDED", " suspended"}
	for _, wantSuspended := range []bool{true, false} {
		for _, answer := range answers {
			for _, changed := range []bool{true, false} {
				v := judge(wantSuspended, m3admin.Result{State: answer, Changed: changed, HTTPStatus: 200}, nil, nil)
				converged := answer == stateFor(wantSuspended)
				switch {
				case converged && (v.kind != outConverged || v.state != answer || v.changed != changed):
					t.Errorf("want_suspended=%v, answer %q, changed=%v: got %+v, want a convergence carrying the answer", wantSuspended, answer, changed, v)
				case !converged && v.kind == outConverged:
					t.Errorf("want_suspended=%v, answer %q: a state other than the wanted one was judged CONVERGED", wantSuspended, answer)
				case !converged && (answer == m3admin.StateSuspended || answer == m3admin.StateActive) && v.kind != outMismatch:
					t.Errorf("want_suspended=%v, answer %q: a recognised wrong state is a mismatch, got %+v", wantSuspended, answer, v)
				case !converged && answer != m3admin.StateSuspended && answer != m3admin.StateActive && v.kind != outFailed:
					t.Errorf("want_suspended=%v, answer %q: an unrecognised state is a plain failure, got %+v", wantSuspended, answer, v)
				}
				if !converged && v.text == "" {
					t.Errorf("want_suspended=%v, answer %q: a refusal with no explanation to store", wantSuspended, answer)
				}
				if converged && v.text != "" {
					t.Errorf("a convergence carries an error text %q (it would be stored as last_error)", v.text)
				}
			}
		}
	}
	// The mismatch text names both states and nothing else from the answer.
	v := judge(true, m3admin.Result{State: m3admin.StateActive}, nil, nil)
	if !strings.Contains(v.text, `"active"`) || !strings.Contains(v.text, `"suspended"`) {
		t.Errorf("the mismatch text does not name the two states: %q", v.text)
	}
}

func TestJudge_AnErrorNeverConverges(t *testing.T) {
	// Even when the Result carries the wanted state, an error means module 3 confirmed nothing.
	right := m3admin.Result{State: m3admin.StateSuspended, Changed: true}
	cases := map[string]struct {
		err       error
		kind      outcome
		retryable bool
	}{
		"a transport error":    {errors.New("dial tcp: connection refused"), outFailed, false},
		"a retryable error":    {&m3admin.RetryableError{Op: "suspend", HTTPStatus: 503, Reason: "module 3 failed"}, outFailed, true},
		"wrapped retryable":    {fmt.Errorf("pass: %w", &m3admin.RetryableError{Op: "suspend", Reason: "timeout", Cause: context.DeadlineExceeded}), outFailed, true},
		"unknown user":         {m3admin.ErrUnknownUser, outFailed, false},
		"invalid":              {m3admin.ErrInvalid, outFailed, false},
		"unauthorized":         {m3admin.ErrUnauthorized, outConfigFault, false},
		"wrapped unauthorized": {fmt.Errorf("suspend: %w", m3admin.ErrUnauthorized), outConfigFault, false},
	}
	for name, tc := range cases {
		v := judge(true, right, tc.err, nil)
		if v.kind != tc.kind || v.retryable != tc.retryable {
			t.Errorf("%s: got kind %d retryable %v, want kind %d retryable %v", name, v.kind, v.retryable, tc.kind, tc.retryable)
		}
		if v.kind == outConverged {
			t.Errorf("%s: an error was judged a convergence", name)
		}
		if v.text == "" {
			t.Errorf("%s: no text to store", name)
		}
	}
	// Control: the same Result with no error IS a convergence, so the loop above is about the error.
	if v := judge(true, right, nil, nil); v.kind != outConverged {
		t.Errorf("control failed: the right answer with no error was judged %+v", v)
	}
}

func TestJudge_TheStoredTextNeverCarriesTheKey(t *testing.T) {
	leaky := fmt.Errorf("post https://op:%s@m3:8080/v1: Authorization: Bearer %s failed", m3Canary, m3Canary)
	v := judge(false, m3admin.Result{}, leaky, []string{m3Canary})
	if v.kind != outFailed || strings.Contains(v.text, m3Canary) || len(v.text) > MaxErrorBytes {
		t.Errorf("a failure carrying the key was stored as %q (%d bytes)", v.text, len(v.text))
	}
	if !strings.Contains(leaky.Error(), m3Canary) {
		t.Fatal("control failed: the fixture error does not carry the key")
	}
	// The configuration-fault text is fixed: it never echoes the error at all.
	if v := judge(false, m3admin.Result{}, fmt.Errorf("%w %s", m3admin.ErrUnauthorized, m3Canary), []string{m3Canary}); strings.Contains(v.text, m3Canary) || !strings.Contains(v.text, "PLATFORM_M3_ADMIN_TOKEN") {
		t.Errorf("the config-fault text is %q: it must name the variable and nothing from the error", v.text)
	}
}

func TestReasonFor_NamesTheCauseAndTheReverification(t *testing.T) {
	for _, tc := range []struct {
		row      syncRow
		reverify bool
		want     string
	}{
		{syncRow{WantSuspended: true, Auto: false}, false, "platform entitlement: disabled"},
		{syncRow{WantSuspended: true, Auto: true}, false, "platform entitlement: expired"},
		{syncRow{WantSuspended: false, Auto: true}, false, "platform entitlement: enabled"},
		{syncRow{WantSuspended: false, Auto: false}, false, "platform entitlement: enabled"},
		{syncRow{WantSuspended: true, Auto: true}, true, "platform entitlement: expired (re-verify)"},
		{syncRow{WantSuspended: false}, true, "platform entitlement: enabled (re-verify)"},
	} {
		if got := reasonFor(tc.row, tc.reverify); got != tc.want {
			t.Errorf("reasonFor(%+v, %v) = %q, want %q", tc.row, tc.reverify, got, tc.want)
		}
		if len(reasonFor(tc.row, tc.reverify)) > m3admin.MaxReasonBytes {
			t.Error("the reason is longer than module 3 accepts")
		}
	}
}

// ---- the retry delay ------------------------------------------------------------------------------------------

func TestWaitingOutBackoff(t *testing.T) {
	base := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := base.Add(d); return &x }
	clock := base.Add(10 * time.Second) // "now" of the arithmetic

	rec := &Reconciler{Now: func() time.Time { return clock }}
	for name, tc := range map[string]struct {
		row  syncRow
		want bool
	}{
		"never attempted":                         {syncRow{LastAttemptAt: nil, UpdatedAt: base}, false},
		"failed 1s ago, 2 failures (4s delay)":    {syncRow{LastAttemptAt: at(9 * time.Second), UpdatedAt: *at(9 * time.Second), Attempts: 2}, true},
		"failed 5s ago, 2 failures (4s delay)":    {syncRow{LastAttemptAt: at(5 * time.Second), UpdatedAt: *at(5 * time.Second), Attempts: 2}, false},
		"failed exactly 4s ago (the edge)":        {syncRow{LastAttemptAt: at(6 * time.Second), UpdatedAt: *at(6 * time.Second), Attempts: 2}, false},
		"failed 59s ago, many failures (60s cap)": {syncRow{LastAttemptAt: at(-49 * time.Second), UpdatedAt: *at(-49 * time.Second), Attempts: 40}, true},
		"failed 61s ago, many failures (60s cap)": {syncRow{LastAttemptAt: at(-51 * time.Second), UpdatedAt: *at(-51 * time.Second), Attempts: 40}, false},
		"the want changed AFTER the last attempt": {syncRow{LastAttemptAt: at(9 * time.Second), UpdatedAt: *at(9*time.Second + time.Millisecond), Attempts: 6}, false},
		"updated_at BEFORE the last attempt":      {syncRow{LastAttemptAt: at(9 * time.Second), UpdatedAt: *at(8 * time.Second), Attempts: 2}, true},
	} {
		if got := rec.waitingOutBackoff(tc.row); got != tc.want {
			t.Errorf("%s: waitingOutBackoff = %v, want %v", name, got, tc.want)
		}
	}

	// A custom Backoff is the one used (the seam the database tests drive).
	zero := &Reconciler{Now: func() time.Time { return clock }, Backoff: func(int) time.Duration { return 0 }}
	if zero.waitingOutBackoff(syncRow{LastAttemptAt: at(10 * time.Second), UpdatedAt: *at(10 * time.Second), Attempts: 9}) {
		t.Error("a zero backoff still holds a row back")
	}
	long := &Reconciler{Now: func() time.Time { return clock }, Backoff: func(int) time.Duration { return time.Hour }}
	if !long.waitingOutBackoff(syncRow{LastAttemptAt: at(0), UpdatedAt: *at(0), Attempts: 1}) {
		t.Error("an hour's backoff does not hold a row back")
	}
}

func TestRunOnce_NeedsItsDependencies(t *testing.T) {
	for name, r := range map[string]*Reconciler{"nil reconciler": nil, "no pool, no client": {}, "no client": {Pool: nil, M3: nil}} {
		rep, err := r.RunOnce(context.Background())
		if err == nil {
			t.Errorf("%s: RunOnce succeeded with nothing to work on", name)
		}
		if rep.Attempted != 0 || rep.Unconverged != -1 {
			t.Errorf("%s: a refused pass reported work: %+v", name, rep)
		}
	}
}

// ---- the statements -------------------------------------------------------------------------------------------

// setClause is the text between SET and WHERE of an UPDATE.
func setClause(sql string) string {
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(sql, " ")
	i, j := strings.Index(flat, " SET "), strings.Index(flat, " WHERE ")
	if i < 0 || j < i {
		return ""
	}
	return flat[i+len(" SET ") : j]
}

// setColumns lists the columns an UPDATE assigns, splitting on the commas that are not inside parentheses.
func setColumns(sql string) []string {
	var cols []string
	depth, start := 0, 0
	set := setClause(sql)
	for i := 0; i <= len(set); i++ {
		switch {
		case i < len(set) && set[i] == '(':
			depth++
		case i < len(set) && set[i] == ')':
			depth--
		case i == len(set) || (set[i] == ',' && depth == 0):
			if col, _, ok := strings.Cut(strings.TrimSpace(set[start:i]), " = "); ok {
				cols = append(cols, col)
			}
			start = i + 1
		}
	}
	return cols
}

func whereClause(sql string) string {
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(sql, " ")
	_, w, _ := strings.Cut(flat, " WHERE ")
	return w
}

// hasVersionGuard is the property of a compare-and-set: the WHERE clause pins the version acted on.
func hasVersionGuard(sql string) bool {
	return strings.Contains(whereClause(sql), "want_version = $3")
}

func TestSQL_TheUpdatesAreCompareAndSetOnTheVersionActedOn(t *testing.T) {
	for name, sql := range map[string]string{"converge": sqlConverge, "fail": sqlFail, "mismatch": sqlMismatch} {
		if !hasVersionGuard(sql) {
			t.Errorf("%s: the WHERE clause does not pin want_version = $3: a stale answer could mark a newer version\n%s", name, whereClause(sql))
		}
		if !strings.Contains(whereClause(sql), "user_id = $1::uuid") {
			t.Errorf("%s: the statement is not scoped to one user", name)
		}
	}
	// The success statement also pins the wanted STATE, and writes the very version it pinned.
	w := whereClause(sqlConverge)
	if !strings.Contains(w, "want_suspended = $4") {
		t.Errorf("converge: the WHERE clause does not pin want_suspended = $4: %s", w)
	}
	if !strings.Contains(setClause(sqlConverge), "converged_version = $3") {
		t.Errorf("converge: converged_version is not set to the pinned version $3: %s", setClause(sqlConverge))
	}
	// Paired control: an UPDATE without the guard is recognised as unguarded.
	unguarded := `UPDATE platform.m3_sync SET converged_version = $3, observed = $2 WHERE user_id = $1::uuid`
	if hasVersionGuard(unguarded) {
		t.Fatal("control failed: an unguarded UPDATE passed the guard check")
	}
}

func TestSQL_OnlyTheColumnsPlatformAppMayWriteAreAssigned(t *testing.T) {
	// platform_0006 grants platform_app UPDATE on exactly these columns of platform.m3_sync; want_* belong to the trigger.
	allowed := map[string]bool{"observed": true, "observed_at": true, "attempts": true, "last_attempt_at": true, "last_error": true, "converged_version": true, "updated_at": true}
	for name, sql := range map[string]string{"converge": sqlConverge, "fail": sqlFail, "mismatch": sqlMismatch} {
		cols := setColumns(sql)
		if len(cols) < 4 {
			t.Fatalf("%s: parsed %d assigned columns from %q: the instrument matched too little", name, len(cols), setClause(sql))
		}
		for _, c := range cols {
			if !allowed[c] {
				t.Errorf("%s assigns %q, which platform_app may not write (want_* is the trigger's)", name, c)
			}
		}
	}
	// The failure statement never touches the convergence columns; the mismatch statement only ever LOWERS the version.
	for _, c := range setColumns(sqlFail) {
		if c == "converged_version" || c == "observed" || c == "observed_at" {
			t.Errorf("a FAILED attempt assigns %s: a failure must not record an observation or a convergence", c)
		}
	}
	if !strings.Contains(setClause(sqlMismatch), "converged_version = LEAST(converged_version, want_version - 1)") {
		t.Errorf("the mismatch statement does not only lower converged_version: %s", setClause(sqlMismatch))
	}
	// Control: the parser sees a disallowed column when there is one.
	if cols := setColumns(`UPDATE platform.m3_sync SET want_version = want_version + 1, observed = $2 WHERE user_id = $1`); len(cols) != 2 || allowed[cols[0]] {
		t.Fatalf("control failed: the column parser returned %v", cols)
	}
}

func TestSQL_EveryStatementNamesItsSchema(t *testing.T) {
	stmts := map[string]string{
		"entry by key": sqlEntryByKey, "insert entry": sqlInsertEntry, "due users": sqlDueUsers, "still due": sqlStillDue, "insert expiry": sqlInsertExpiry,
		"get entitlement": sqlGetEntitlement, "get sync": sqlGetSync, "count unconverged": sqlCountUnconverged,
		"select unconverged": sqlSelectUnconverged, "select reverify": sqlSelectReverify, "converge": sqlConverge, "fail": sqlFail, "mismatch": sqlMismatch,
	}
	rel := regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE)\s+([a-z_][a-z0-9_.]*)`)
	seen := 0
	for name, sql := range stmts {
		for _, m := range rel.FindAllStringSubmatch(sql, -1) {
			seen++
			if !strings.HasPrefix(m[1], "platform.") {
				t.Errorf("%s: the relation %q is not schema-qualified (CLAUDE.md rule 12)", name, m[1])
			}
		}
	}
	if seen < len(stmts) {
		t.Fatalf("found %d relations in %d statements: the instrument matched too little", seen, len(stmts))
	}
	if m := rel.FindStringSubmatch("SELECT 1 FROM m3_sync"); m == nil || strings.HasPrefix(m[1], "platform.") {
		t.Fatal("control failed: an unqualified relation was not recognised as such")
	}
}

func TestSQL_TheExpiryIsWrittenBySystemNotAnAdmin(t *testing.T) {
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(sqlInsertExpiry, " ")
	for _, want := range []string{"'m3_expire'", "acting_admin_id", "NULL", "'m3'", "0, 0"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the expiry insert lacks %q: %s", want, flat)
		}
	}
	// The due check is the database's: now(), never a bound time.
	for name, sql := range map[string]string{"due users": sqlDueUsers, "still due": sqlStillDue} {
		if !strings.Contains(sql, "enabled_until <= now()") {
			t.Errorf("%s: the due test is not made on the database's clock", name)
		}
	}
	if !strings.Contains(sqlUntilIsFuture, "now() + interval '1 second'") {
		t.Error("the future-date test is not made on the database's clock with the one-second lead")
	}
}
