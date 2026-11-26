//go:build harness

package harness

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The database half of owner ruling OD-3 (platform_0014): the flag that tells a verify job's results that the address was on the user's suppression list. The store half is in
// jobs_suppression_scope_test.go; the CHECKs that existed before it (job_rows_results_test.go) were extended with the flag where a row of the state `suppressed` is written.

// TestJobRows_TheSuppressedFlag: the state `suppressed` and the flag are tied by a CHECK (a second writer cannot make a suppressed row look like any other: the violating row is refused for ITS name beside the legal
// twin); the flag is only on a row that is pending or suppressed (a malformed, duplicate or excluded row was never on the list); a PENDING row that carries it exists only in a module 1 job (in a send job a flagged
// pending row would reach the module although the user suppressed it: the row guard refuses it, on INSERT and on UPDATE, and the verify job's twin is accepted); NULL is refused; and the flag is part of the list,
// which changes only while the job is draft or previewed (the one column the guard let through, submitted_at, is not the flag: for each of the eleven statuses a module 1 job can have, the flag moves in a draft
// and a previewed job and is refused in the others, and the same value written back is no change).
// It fails if the CHECK or the guard is missing or weaker, if a NULL gets through, or if the flag can move after the job has been confirmed.
func TestJobRows_TheSuppressedFlag(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c8")
	ord := 0
	a := ledgerUserA
	verifyJob, sendJob := ledgerJobA1, ledgerJobA2 // user A's draft jobs on module 1 and module 2
	check := func(c string) ledgerWhy { return ledgerWhy{Constraint: c} }
	mk := func(job string, kv ...any) ledgerCols { return jrRow(next, &ord, job, a, kv...) }

	t.Run("a row in the state suppressed carries the flag", func(t *testing.T) {
		ledgerRefusedInsert(t, tx, "a suppressed row with no flag (a writer cannot make it look like any other row)", "23514", check("job_rows_suppressed_state_has_the_flag"), jrRows, mk(sendJob, "state", "suppressed"))
		ledgerOKInsert(t, tx, "control: the same row with the flag", jrRows, mk(sendJob, "state", "suppressed", "suppressed", true))
		ledgerRefusedInsert(t, tx, "a suppressed row of a module 1 job with no flag (the rule is not a send job's)", "23514", check("job_rows_suppressed_state_has_the_flag"), jrRows, mk(verifyJob, "state", "suppressed"))
		ledgerTrial(t, tx, func(sp pgx.Tx) { // the same on UPDATE: a pending row cannot be made suppressed without the flag
			r := mk(sendJob)
			ledgerOKInsert(t, sp, "a pending row", jrRows, r)
			ledgerExpectRefused(t, sp, "a pending row made suppressed with no flag", "23514", check("job_rows_suppressed_state_has_the_flag"), "UPDATE platform.job_rows SET state = 'suppressed' WHERE id = $1", r["id"])
			ledgerExpectOK(t, sp, "control: made suppressed in one statement with the flag (what the confirm does)", "UPDATE platform.job_rows SET state = 'suppressed', suppressed = true WHERE id = $1", r["id"])
		})
	})

	t.Run("the flag is NOT NULL", func(t *testing.T) {
		ledgerRefusedInsert(t, tx, "a NULL flag", "23502", ledgerWhy{Column: "suppressed"}, jrRows, mk(sendJob, "suppressed", nil))
		plain := mk(sendJob)
		ledgerOKInsert(t, tx, "control: a row that does not name the flag", jrRows, plain)
		if got := ledgerRows(t, tx, "SELECT suppressed::text FROM platform.job_rows WHERE id = $1", plain["id"]); len(got) != 1 || got[0] != "false" {
			t.Fatalf("a row that names no flag holds %v, want false", got)
		}
	})

	t.Run("the flag is only on a row that is pending or suppressed", func(t *testing.T) {
		const only = "job_rows_flag_only_on_listable_rows"
		first := mk(verifyJob)
		ledgerOKInsert(t, tx, "a pending row (the one the others repeat)", jrRows, first)
		for _, c := range []struct {
			what string
			kv   []any
		}{
			{"a malformed row", []any{"state", "malformed", "email_norm", nil, "reason", "no_at"}},
			{"a duplicate row", []any{"state", "duplicate", "duplicate_of", first["id"], "email_norm", first["email_norm"]}},
			{"an excluded row", []any{"state", "excluded", "reason", "invalid"}},
		} {
			flagged := append(append([]any{}, c.kv...), "suppressed", true)
			ledgerRefusedInsert(t, tx, c.what+" that carries the flag (it was never going to be sent: it has no address on the list)", "23514", check(only), jrRows, mk(verifyJob, flagged...))
			ledgerOKInsert(t, tx, "control: "+c.what+" with the flag false", jrRows, mk(verifyJob, c.kv...))
		}
	})

	t.Run("a pending row with the flag is a verify job's", func(t *testing.T) {
		const guard = "job_rows_pending_flag_is_verify_only"
		ledgerOKInsert(t, tx, "control: a pending row with the flag in a module 1 job (verified, charged, flagged)", jrRows, mk(verifyJob, "suppressed", true))
		ledgerRefusedInsert(t, tx, "a pending row with the flag in a module 2 job (it would reach the module although the user suppressed it)", "23514", check(guard), jrRows, mk(sendJob, "suppressed", true))
		m3job := next()
		ledgerMust(t, tx, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm3')", m3job, a)
		ledgerRefusedInsert(t, tx, "a pending row with the flag in a module 3 job", "23514", check(guard), jrRows, mk(m3job, "suppressed", true))
		ledgerOKInsert(t, tx, "control: the module 3 job's suppressed row (state suppressed, flagged)", jrRows, mk(m3job, "state", "suppressed", "suppressed", true))
		ledgerTrial(t, tx, func(sp pgx.Tx) { // the same on UPDATE
			v, s := mk(verifyJob), mk(sendJob)
			ledgerOKInsert(t, sp, "a pending row of the verify job", jrRows, v)
			ledgerOKInsert(t, sp, "a pending row of the send job", jrRows, s)
			ledgerExpectRefused(t, sp, "the send job's pending row flagged", "23514", check(guard), "UPDATE platform.job_rows SET suppressed = true WHERE id = $1", s["id"])
			ledgerExpectOK(t, sp, "control: the verify job's pending row flagged", "UPDATE platform.job_rows SET suppressed = true WHERE id = $1", v["id"])
		})
	})

	t.Run("the flag changes only while the job is draft or previewed", func(t *testing.T) {
		editable := map[string]bool{"draft": true, "previewed": true}
		frozen := check("job_rows_frozen")
		for _, s := range jobStates {
			s := s
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				job := next()
				ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm1')", job, a)
				n := 0
				r := jrRow(next, &n, job, a)
				ledgerOKInsert(t, sp, "a pending row of a draft verify job", jrRows, r)
				jobWalkTo(t, sp, job, s)
				ledgerExpectOK(t, sp, fmt.Sprintf("control: the flag written back unchanged in a %s job", s), "UPDATE platform.job_rows SET suppressed = suppressed WHERE id = $1", r["id"])
				if editable[s] {
					ledgerExpectOK(t, sp, fmt.Sprintf("the flag set in a %s job", s), "UPDATE platform.job_rows SET suppressed = true WHERE id = $1", r["id"])
				} else {
					ledgerExpectRefused(t, sp, fmt.Sprintf("the flag set in a %s job (the guard let a change of this one column through until it was named in its comparison)", s), "23001", frozen, "UPDATE platform.job_rows SET suppressed = true WHERE id = $1", r["id"])
				}
			})
		}
	})
}

// TestJobResults_TheSuppressedFlagIsDerivedFromTheRowAndNeverStatedByTheWriter: the flag of a result is its row's, set by the BEFORE INSERT guard whatever the writer said, so that RecordResults, the confirm, a
// cancel and every writer that comes later (the admin surface) get it without naming it and none can forge it. A result that states false for a flagged row holds true, one that states true for a row that is
// not flagged holds false, one that says nothing holds its row's; the platform's own outcome `suppressed` is refused for a row that is not suppressed (the CHECK, beside its accepted twin and the module's own
// word of the same name, which the CHECK does not read); platform_admin reads the flag, and the flag of a written result cannot change (append-only, even for the owner).
// It fails if a writer can state the flag, if the derivation is missing, or if a result can claim the platform's suppressed outcome for an address that is not on the list.
func TestJobResults_TheSuppressedFlagIsDerivedFromTheRowAndNeverStatedByTheWriter(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c9")
	ord := 0
	a := ledgerUserA
	verifyJob, sendJob := ledgerJobA1, ledgerJobA2
	flagged := jrRow(next, &ord, verifyJob, a, "suppressed", true) // a verify job's suppressed address: pending, flagged
	plain := jrRow(next, &ord, verifyJob, a)                       // its twin
	third := jrRow(next, &ord, verifyJob, a, "suppressed", true)
	suppressedRow := jrRow(next, &ord, sendJob, a, "state", "suppressed", "suppressed", true)
	pendingRow := jrRow(next, &ord, sendJob, a)
	for _, r := range []ledgerCols{flagged, plain, third, suppressedRow, pendingRow} {
		ledgerOKInsert(t, tx, "a row of a draft job", jrRows, r)
	}
	jobWalkTo(t, tx, verifyJob, "confirmed")
	jobWalkTo(t, tx, sendJob, "confirmed")
	flagOf := func(row ledgerCols) string {
		t.Helper()
		got := ledgerRows(t, tx, "SELECT suppressed::text FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", row["job_id"], row["id"])
		if len(got) != 1 {
			t.Fatalf("%d results for row %v, want 1", len(got), row["id"])
		}
		return got[0]
	}
	check := func(c string) ledgerWhy { return ledgerWhy{Constraint: c} }

	ledgerOKInsert(t, tx, "a result that states FALSE for the flagged row", jrResults, jrResultOf(flagged, "suppressed", false))
	if got := flagOf(flagged); got != "true" {
		t.Errorf("the result of the flagged row, stated false, holds %s, want true (the writer's word is overwritten)", got)
	}
	ledgerOKInsert(t, tx, "a result that states TRUE for a row that is not flagged", jrResults, jrResultOf(plain, "suppressed", true))
	if got := flagOf(plain); got != "false" {
		t.Errorf("the result of the unflagged row, stated true, holds %s, want false (a writer cannot flag a result)", got)
	}
	ledgerOKInsert(t, tx, "a result that says nothing about the flag (the way every writer before platform_0014 writes)", jrResults, jrResultOf(third))
	if got := flagOf(third); got != "true" {
		t.Errorf("the result of the flagged row that says nothing holds %s, want true", got)
	}

	// the platform's own outcome `suppressed`: derived true for the suppressed row (accepted), refused for a row that is not suppressed
	own := func(row ledgerCols) ledgerCols {
		return jrResultOf(row, "origin", "platform", "module_state", "suppressed", "rule", "P-SUP")
	}
	ledgerOKInsert(t, tx, "control: the platform's `suppressed` result of the suppressed row", jrResults, own(suppressedRow))
	if got := flagOf(suppressedRow); got != "true" {
		t.Errorf("the platform's suppressed result holds the flag %s, want true", got)
	}
	ledgerRefusedInsert(t, tx, "the platform's `suppressed` result of a row that is not suppressed (a result cannot claim the outcome for an address that is not on the list)", "23514", check("job_results_suppressed_platform_outcome"), jrResults, own(pendingRow))
	ledgerOKInsert(t, tx, "control: the module's own state of that name for the same row is not the platform's outcome (the CHECK reads the origin)", jrResults, jrResultOf(pendingRow, "module_state", "suppressed"))
	if got := flagOf(pendingRow); got != "false" {
		t.Errorf("the module's result for the unflagged row holds the flag %s, want false", got)
	}

	// who may read it, and that it never changes
	ledgerSwitchRole(t, tx, "platform_admin")
	if got := ledgerRows(t, tx, "SELECT count(*)::text FROM platform.job_results WHERE suppressed"); len(got) != 1 || got[0] != "3" {
		t.Errorf("platform_admin reads %v flagged results, want 3 (two verify results and the platform's own)", got)
	}
	ledgerResetRole(t, tx)
	ledgerSwitchRole(t, tx, "platform_owner")
	ledgerExpectRefused(t, tx, "the owner flipping a result's flag", "23001", ledgerWhy{Message: "append-only"}, "UPDATE platform.job_results SET suppressed = NOT suppressed")
	ledgerResetRole(t, tx)
}
