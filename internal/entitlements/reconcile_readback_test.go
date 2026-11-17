package entitlements

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/m3admin"
)

// D5 (open decision 12): the read-back is the confirmation, and every observation carries the version it was made at. These are the unit tests of the pure parts (judgeRead, the two
// predicates of a row, the shape of the statements); what the reconciler does against a real database is proven by tests/harness/entitlements_observed_test.go.

func TestJudgeRead_ConvergenceIsExactlyTheWantedStateOfThisUser(t *testing.T) {
	const user = "11111111-2222-4333-8444-555555555555"
	read := func(suspended bool) m3admin.SuspensionState {
		return m3admin.SuspensionState{User: user, Suspended: suspended, Version: 4}
	}
	for _, wantSuspended := range []bool{true, false} {
		for _, readSuspended := range []bool{true, false} {
			v := judgeRead(user, wantSuspended, read(readSuspended), nil, nil)
			converged := wantSuspended == readSuspended
			switch {
			case converged && (v.kind != outConverged || v.state != stateFor(wantSuspended) || v.text != ""):
				t.Errorf("want_suspended=%v, read suspended=%v: got %+v, want a convergence carrying the state and no error text", wantSuspended, readSuspended, v)
			case !converged && v.kind != outMismatch:
				t.Errorf("want_suspended=%v, read suspended=%v: got %+v, want a MISMATCH (module 3 itself said it is in the other state)", wantSuspended, readSuspended, v)
			case !converged && (v.state != stateFor(readSuspended) || !strings.Contains(v.text, "not converged")):
				t.Errorf("want_suspended=%v, read suspended=%v: the mismatch %+v must carry what module 3 said and say it is not converged", wantSuspended, readSuspended, v)
			}
		}
	}
	// the mismatch text names both states and nothing else from the answer
	if v := judgeRead(user, true, read(false), nil, nil); !strings.Contains(v.text, `"active"`) || !strings.Contains(v.text, `"suspended"`) {
		t.Errorf("the mismatch text does not name the two states: %q", v.text)
	}

	// A state read for ANOTHER user, or for no user, converges nothing, even when it is the wanted state (a zero SuspensionState reads as active, so it must be refused by its user).
	other := m3admin.SuspensionState{User: "66666666-7777-4888-9999-aaaaaaaaaaaa", Suspended: true}
	for name, st := range map[string]m3admin.SuspensionState{"another user": other, "no user at all": {Suspended: true}, "the zero state": {}} {
		wantSuspended := st.Suspended
		if v := judgeRead(user, wantSuspended, st, nil, nil); v.kind != outFailed || v.text == "" {
			t.Errorf("%s: judged %+v, want a failure with a text: a state that was not read for this user is not a confirmation", name, v)
		}
	}
	// Control: the same states, read for the right user, DO converge, so the refusals above were about the user.
	if v := judgeRead(user, true, m3admin.SuspensionState{User: user, Suspended: true}, nil, nil); v.kind != outConverged {
		t.Errorf("control failed: the right user's suspended state, wanted suspended, was judged %+v", v)
	}
	if v := judgeRead(user, false, m3admin.SuspensionState{User: user}, nil, nil); v.kind != outConverged {
		t.Errorf("control failed: the right user's active state, wanted active, was judged %+v", v)
	}
}

func TestJudgeRead_AFailedReadNeverConverges(t *testing.T) {
	const user = "11111111-2222-4333-8444-555555555555"
	// Even when the state carried beside the error is the wanted one, an error means module 3 confirmed nothing. The unknown user is the trap: its zero state reads as ACTIVE.
	right := m3admin.SuspensionState{User: user, Suspended: false, Version: 2}
	cases := map[string]struct {
		err       error
		kind      outcome
		retryable bool
		contains  string
	}{
		"a transport error":    {errors.New("dial tcp: connection refused"), outFailed, false, "read-back failed"},
		"a retryable error":    {&m3admin.RetryableError{Op: "suspension read", HTTPStatus: 503, Reason: "a server error"}, outFailed, true, "read-back failed"},
		"wrapped retryable":    {fmt.Errorf("pass: %w", &m3admin.RetryableError{Op: "suspension read", Reason: "timeout", Cause: context.DeadlineExceeded}), outFailed, true, "read-back failed"},
		"unknown user (a 404)": {m3admin.ErrUnknownUser, outFailed, false, "no state to confirm"},
		"wrapped unknown user": {fmt.Errorf("suspension read: %w", m3admin.ErrUnknownUser), outFailed, false, "no state to confirm"},
		"invalid":              {m3admin.ErrInvalid, outFailed, false, "read-back failed"},
		"unauthorized":         {m3admin.ErrUnauthorized, outConfigFault, false, "PLATFORM_M3_ADMIN_TOKEN"},
		"wrapped unauthorized": {fmt.Errorf("suspension read: %w", m3admin.ErrUnauthorized), outConfigFault, false, "PLATFORM_M3_ADMIN_TOKEN"},
	}
	for name, tc := range cases {
		for _, wantSuspended := range []bool{false, true} {
			v := judgeRead(user, wantSuspended, right, tc.err, nil)
			if v.kind != tc.kind || v.retryable != tc.retryable || !strings.Contains(v.text, tc.contains) {
				t.Errorf("%s (want_suspended=%v): got kind %d retryable %v text %q, want kind %d retryable %v and a text containing %q", name, wantSuspended, v.kind, v.retryable, v.text, tc.kind, tc.retryable, tc.contains)
			}
			if v.kind == outConverged {
				t.Errorf("%s (want_suspended=%v): an error was judged a convergence", name, wantSuspended)
			}
		}
	}
	// the stored text never carries the key, and is capped
	leaky := fmt.Errorf("get https://op:%s@m3:8080/v1/admin/users/x/suspension: Authorization: Bearer %s failed: %s", m3Canary, m3Canary, strings.Repeat("x", 2*MaxErrorBytes))
	if !strings.Contains(leaky.Error(), m3Canary) {
		t.Fatal("control failed: the fixture error does not carry the key")
	}
	if v := judgeRead(user, false, m3admin.SuspensionState{}, leaky, []string{m3Canary}); v.kind != outFailed || strings.Contains(v.text, m3Canary) || len(v.text) > MaxErrorBytes {
		t.Errorf("a read failure carrying the key was stored as %q (%d bytes)", v.text, len(v.text))
	}
	// Control: the same state with no error IS a convergence, so the loop above is about the error.
	if v := judgeRead(user, false, right, nil, nil); v.kind != outConverged {
		t.Errorf("control failed: the right state with no error was judged %+v", v)
	}
}

func TestSyncRow_PostAnsweredIsTheCallsConfirmationOfThisVersionAndOfTheWantedState(t *testing.T) {
	base := syncRow{WantSuspended: true, WantVersion: 3, ConvergedVersion: 2, Observed: "suspended", ObservedVersion: i64(3)}
	cases := []struct {
		name string
		mut  func(*syncRow)
		want bool
	}{
		{"the control: the confirmation of the call, of the wanted state, at this version, not converged", func(*syncRow) {}, true},
		{"the same, for a wanted active state", func(r *syncRow) { r.WantSuspended, r.Observed = false, "active" }, true},
		{"never converged (converged_version 0)", func(r *syncRow) { r.ConvergedVersion = 0 }, true},
		{"observed at an OLDER version (a stale observation confirms nothing)", func(r *syncRow) { r.ObservedVersion = i64(2) }, false},
		{"observed at a version AHEAD of the wanted one", func(r *syncRow) { r.ObservedVersion = i64(4) }, false},
		{"no observed version", func(r *syncRow) { r.ObservedVersion = nil }, false},
		{"observed the OTHER state (module 3 contradicted the wanted one: the call is due again)", func(r *syncRow) { r.Observed = "active" }, false},
		{"never observed", func(r *syncRow) { r.Observed = "unknown" }, false},
		{"already converged (a re-verification, not a delivery)", func(r *syncRow) { r.ConvergedVersion = 3 }, false},
	}
	for _, tc := range cases {
		row := base
		tc.mut(&row)
		if got := row.postAnswered(); got != tc.want {
			t.Errorf("%s: postAnswered() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSyncRow_AlreadyDisagreedMarksOnlyAContradictionOfThisVersion(t *testing.T) {
	base := syncRow{WantSuspended: true, WantVersion: 3, ConvergedVersion: 2, Observed: "active", ObservedVersion: i64(3)}
	cases := []struct {
		name string
		mut  func(*syncRow)
		want bool
	}{
		{"the control: module 3 was recorded in the other state at this version", func(*syncRow) {}, true},
		{"the same, for a wanted active state", func(r *syncRow) { r.WantSuspended, r.Observed = false, "suspended" }, true},
		{"the wanted state was recorded (nothing contradicts it yet: the next disagreement is the first)", func(r *syncRow) { r.Observed = "suspended" }, false},
		{"the contradiction is of an older version (this version has not disagreed yet)", func(r *syncRow) { r.ObservedVersion = i64(2) }, false},
		{"no observed version", func(r *syncRow) { r.ObservedVersion = nil }, false},
		{"never observed", func(r *syncRow) { r.Observed = "unknown" }, false},
	}
	for _, tc := range cases {
		row := base
		tc.mut(&row)
		if got := row.alreadyDisagreed(); got != tc.want {
			t.Errorf("%s: alreadyDisagreed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSQL_ARecordedObservationAlwaysCarriesTheVersionItWasMadeAt(t *testing.T) {
	// The statements that record what module 3 said (an observation) are the ones that assign `observed`; each must stamp it with the version it was compared
	// against ($3, the pinned want_version), or an observation could be re-used for a later wanted state. The statement that records a failure assigns none.
	recorders := map[string]string{"converge": sqlConverge, "mismatch": sqlMismatch, "post answered": sqlPostAnswered}
	for name, sql := range recorders {
		cols := setColumns(sql)
		if len(cols) < 5 {
			t.Fatalf("%s: parsed %d assigned columns from %q: the instrument matched too little", name, len(cols), setClause(sql))
		}
		if !strings.Contains(setClause(sql), "observed = $2") {
			t.Errorf("%s: does not record the observation ($2): %s", name, setClause(sql))
		}
		if !strings.Contains(setClause(sql), "observed_version = $3") {
			t.Errorf("%s: records an observation without stamping it with the pinned version ($3): %s", name, setClause(sql))
		}
		if !strings.Contains(setClause(sql), "observed_at = now()") {
			t.Errorf("%s: records an observation without the database clock: %s", name, setClause(sql))
		}
	}
	for _, c := range setColumns(sqlFail) {
		if strings.HasPrefix(c, "observed") {
			t.Errorf("the failure statement assigns %s", c)
		}
	}
	// Control: the check can fail. A recorder written like the old one (an observation with no version) is not accepted by it.
	old := `UPDATE platform.m3_sync SET observed = $2, observed_at = now(), converged_version = $3 WHERE user_id = $1::uuid AND want_version = $3`
	if strings.Contains(setClause(old), "observed_version = $3") {
		t.Fatal("control failed: a statement without observed_version passed the check")
	}
	// only the converge statement carries module 3 own counter as a value; the mismatch keeps the last one when the answer came from the call
	if !strings.Contains(setClause(sqlConverge), "m3_version = $5") || !strings.Contains(setClause(sqlMismatch), "m3_version = COALESCE($5") {
		t.Errorf("the module 3 counter is not recorded as designed:\n  converge: %s\n  mismatch: %s", setClause(sqlConverge), setClause(sqlMismatch))
	}
}
