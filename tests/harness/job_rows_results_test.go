//go:build harness

package harness

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The database half of chunk B2 (platform_0012, Docs/phase-4-spec.md Locked 25): a job's LIST (platform.job_rows), its RESULTS (platform.job_results) and the view that proves the ledger, the results and the rows
// agree (platform.job_settlement_drift). Every refusal names the constraint that raised it (ledgerExpectRefused) beside a legal twin, and every CHECK has the row that would evaluate to NULL.

const (
	jrRows    = "platform.job_rows"
	jrResults = "platform.job_results"
)

// jrRow is a legal pending row of a job: a fresh id, the next ordinal of the job and an address of its own (row<ordinal>@example.com, raw with its case and spaces): a job never sends one address twice
// (job_rows_one_pending_per_address), so two pending rows of one job must not share one, and the ordinal counter never repeats within a test. next gives the ids and the ordinals.
func jrRow(next func() string, ordinal *int, job, user string, kv ...any) ledgerCols {
	*ordinal++
	n := *ordinal
	return ledgerCols{"id": next(), "job_id": job, "user_id": user, "ordinal": n, "email_raw": fmt.Sprintf(" Row%d@Example.com ", n), "email_norm": fmt.Sprintf("row%d@example.com", n)}.with(kv...)
}

// jrResultOf is a legal FREE result for the row, written by the module, state sent: it names the ROW's job, user, item reference and ordinal (the composite foreign keys) and the ROW's address, the normalised one
// or, for a row that has none (a malformed one), the raw text (job_results_match_their_row). kv overrides a column, which is how a test says "another row's address" or "another ordinal".
func jrResultOf(row ledgerCols, kv ...any) ledgerCols {
	email := row["email_raw"]
	if row["email_norm"] != nil {
		email = row["email_norm"]
	}
	return ledgerCols{"job_id": row["job_id"], "user_id": row["user_id"], "item_ref": row["id"], "ordinal": row["ordinal"], "email": email, "origin": "module", "module_state": "sent", "decision": "free", "rule": "M2-04"}.with(kv...)
}

// TestJobRows_Checks: each CHECK of platform.job_rows refuses its row for ITS name beside the legal row, and the row that would make it evaluate to NULL is refused too (a CHECK that evaluates to NULL passes): the
// ordinal starts at 1 and is unique in the job; the state is one of five; the raw address is bounded in BYTES (4096 accepted, 4097 not, and a letter of two bytes counts twice); the normalised address is NULL
// exactly when the row is malformed (a pending row with none, a malformed one with one); a row is a duplicate exactly when it names the row it repeats, and that row is a row of the same job; a malformed or
// excluded row says why (NULL, blank); only a pending row is submitted; the fields are an object; NOT NULL columns refuse NULL; a row is never its own duplicate; and an address holds ONE seat in a
// job (a unique index over the pending and the suppressed rows: a second one is 23505 and the index is named, the same address in another job, as a duplicate, as an excluded row, or once the first is
// malformed is accepted, and two malformed rows are accepted: both have no address, the NULL case).
// It fails if a constraint is missing, weaker than its design, or lets a NULL through.
func TestJobRows_Checks(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c1")
	ord := 0
	a, job, other := ledgerUserA, ledgerJobA2, ledgerJobA1b // two draft jobs of user A
	row := func(kv ...any) ledgerCols { return jrRow(next, &ord, job, a, kv...) }
	check := func(c string) ledgerWhy { return ledgerWhy{Constraint: c} }
	first := row()
	ledgerOKInsert(t, tx, "control: a pending row", jrRows, first)
	ledgerOKInsert(t, tx, "a row whose source_item names nothing in particular (the column is free)", jrRows, row("source_item", ledgerUUID("c2", 1)))

	t.Run("the ordinal", func(t *testing.T) {
		for _, bad := range []int{0, -1} {
			ledgerRefusedInsert(t, tx, fmt.Sprintf("ordinal %d", bad), "23514", check("job_rows_ordinal_positive"), jrRows, row("ordinal", bad))
		}
		ledgerRefusedInsert(t, tx, "a NULL ordinal", "23502", ledgerWhy{Column: "ordinal"}, jrRows, row("ordinal", nil))
		ledgerRefusedInsert(t, tx, "the ordinal of another row of the same job", "23505", check("job_rows_ordinal_key"), jrRows, row("ordinal", first["ordinal"]))
		ledgerOKInsert(t, tx, "control: the same ordinal in ANOTHER job", jrRows, ledgerCols{"id": next(), "job_id": other, "user_id": a, "ordinal": first["ordinal"], "email_raw": "x@example.com", "email_norm": "x@example.com"})
		ledgerOKInsert(t, tx, "control: ordinal 1 is the smallest", jrRows, ledgerCols{"id": next(), "job_id": ledgerJobA0, "user_id": a, "ordinal": 1, "email_raw": "y@example.com", "email_norm": "y@example.com"})
	})

	t.Run("the state", func(t *testing.T) {
		ledgerRefusedInsert(t, tx, "a state that is none of the five", "23514", check("job_rows_state_vocabulary"), jrRows, row("state", "bogus"))
		ledgerRefusedInsert(t, tx, "a NULL state", "23502", ledgerWhy{Column: "state"}, jrRows, row("state", nil))
		for _, s := range []string{"suppressed"} {
			ledgerOKInsert(t, tx, "control: state "+s+" (it needs no reason)", jrRows, row("state", s))
		}
	})

	t.Run("the raw address is bounded in bytes", func(t *testing.T) {
		ledgerOKInsert(t, tx, "control: 4096 bytes", jrRows, row("email_raw", strings.Repeat("a", 4096)))
		ledgerOKInsert(t, tx, "control: 2048 two-byte letters are 4096 bytes", jrRows, row("email_raw", strings.Repeat("é", 2048)))
		ledgerRefusedInsert(t, tx, "4097 bytes", "23514", check("job_rows_email_raw_bounded"), jrRows, row("email_raw", strings.Repeat("a", 4097)))
		ledgerRefusedInsert(t, tx, "2049 two-byte letters (4098 bytes: characters are not bytes)", "23514", check("job_rows_email_raw_bounded"), jrRows, row("email_raw", strings.Repeat("é", 2049)))
		ledgerRefusedInsert(t, tx, "a NULL raw address", "23502", ledgerWhy{Column: "email_raw"}, jrRows, row("email_raw", nil))
	})

	t.Run("the normalised address is NULL exactly when the row is malformed", func(t *testing.T) {
		ledgerRefusedInsert(t, tx, "a pending row with no normalised address (the NULL case)", "23514", check("job_rows_email_norm_shape"), jrRows, row("email_norm", nil))
		ledgerRefusedInsert(t, tx, "a malformed row that has one", "23514", check("job_rows_email_norm_shape"), jrRows, row("state", "malformed", "reason", "no domain"))
		ledgerOKInsert(t, tx, "control: a malformed row with none and a reason", jrRows, row("state", "malformed", "reason", "no domain", "email_norm", nil))
		ledgerRefusedInsert(t, tx, "a suppressed row with no normalised address", "23514", check("job_rows_email_norm_shape"), jrRows, row("state", "suppressed", "email_norm", nil))
	})

	t.Run("a row is a duplicate exactly when it names the row it repeats, in the same job", func(t *testing.T) {
		ledgerRefusedInsert(t, tx, "a duplicate that names no row (the NULL case)", "23514", check("job_rows_duplicate_shape"), jrRows, row("state", "duplicate"))
		ledgerRefusedInsert(t, tx, "a pending row that names a row it repeats", "23514", check("job_rows_duplicate_shape"), jrRows, row("duplicate_of", first["id"]))
		ledgerOKInsert(t, tx, "control: a duplicate of the first row (it repeats its address: a duplicate takes no seat)", jrRows, row("state", "duplicate", "duplicate_of", first["id"], "email_norm", first["email_norm"]))
		self := next()
		ledgerRefusedInsert(t, tx, "a duplicate of itself", "23514", check("job_rows_duplicate_not_itself"), jrRows, row("id", self, "state", "duplicate", "duplicate_of", self))
		elsewhere := ledgerCols{"id": next(), "job_id": other, "user_id": a, "ordinal": 7, "email_raw": "z@example.com", "email_norm": "z@example.com"}
		ledgerOKInsert(t, tx, "control: a row of another job", jrRows, elsewhere)
		ledgerRefusedInsert(t, tx, "a duplicate of a row of ANOTHER job", "23503", check("job_rows_duplicate_fk"), jrRows, row("state", "duplicate", "duplicate_of", elsewhere["id"]))
		ledgerRefusedInsert(t, tx, "a duplicate of a row that does not exist", "23503", check("job_rows_duplicate_fk"), jrRows, row("state", "duplicate", "duplicate_of", ledgerNoSuchID))
	})

	t.Run("a malformed or excluded row says why", func(t *testing.T) {
		for _, s := range []string{"malformed", "excluded"} {
			kv := []any{"state", s} // an excluded row keeps the address of its own (it takes no seat); a malformed one has none
			if s == "malformed" {
				kv = append(kv, "email_norm", nil)
			}
			ledgerRefusedInsert(t, tx, "a "+s+" row with no reason (the NULL case)", "23514", check("job_rows_reason_shape"), jrRows, row(kv...))
			ledgerRefusedInsert(t, tx, "a "+s+" row with a blank reason", "23514", check("job_rows_reason_shape"), jrRows, row(append(append([]any{}, kv...), "reason", "   ")...))
			ledgerOKInsert(t, tx, "control: a "+s+" row with a reason", jrRows, row(append(append([]any{}, kv...), "reason", "because")...))
		}
	})

	t.Run("an address holds one seat in a job", func(t *testing.T) {
		const seat = "job_rows_one_pending_per_address"
		norm := first["email_norm"]
		ledgerRefusedInsert(t, tx, "a second pending row with the address of the first", "23505", check(seat), jrRows, row("email_norm", norm))
		ledgerRefusedInsert(t, tx, "a suppressed row with the address of a pending row (a suppressed row is a pending one that the user's own list took out: the index covers both)", "23505", check(seat), jrRows, row("state", "suppressed", "email_norm", norm))
		ledgerOKInsert(t, tx, "control: the same address as a pending row of ANOTHER job", jrRows, ledgerCols{"id": next(), "job_id": other, "user_id": a, "ordinal": 20, "email_raw": "x", "email_norm": norm})
		ledgerOKInsert(t, tx, "control: the same address as a duplicate row (it names the row it repeats)", jrRows, row("state", "duplicate", "duplicate_of", first["id"], "email_norm", norm))
		ledgerOKInsert(t, tx, "control: and a second duplicate of it (a duplicate takes no seat)", jrRows, row("state", "duplicate", "duplicate_of", first["id"], "email_norm", norm))
		ledgerOKInsert(t, tx, "control: the same address as an excluded row (it takes no seat)", jrRows, row("state", "excluded", "reason", "not selected", "email_norm", norm))
		ledgerOKInsert(t, tx, "control: a malformed row (it has no address)", jrRows, row("state", "malformed", "reason", "no domain", "email_norm", nil))
		ledgerOKInsert(t, tx, "a second malformed row: two NULL addresses are two rows (the NULL case)", jrRows, row("state", "malformed", "reason", "no at sign", "email_norm", nil))
		ledgerTrial(t, tx, func(sp pgx.Tx) { // the seat is the row's while it is pending, and free again when the row is not
			ledgerRefusedInsert(t, sp, "before: a second pending row with the address", "23505", check(seat), jrRows, row("email_norm", norm))
			ledgerExpectOK(t, sp, "the first row becomes malformed (it keeps no address)", "UPDATE platform.job_rows SET state = 'malformed', email_norm = NULL, reason = 'no longer valid' WHERE id = $1", first["id"])
			ledgerOKInsert(t, sp, "after: the address is free again", jrRows, row("email_norm", norm))
		})
	})

	t.Run("only a pending row is submitted", func(t *testing.T) {
		ledgerOKInsert(t, tx, "control: a pending row that has been submitted", jrRows, row("submitted_at", "2030-01-01T00:00:00Z"))
		ledgerRefusedInsert(t, tx, "a malformed row that has been submitted", "23514", check("job_rows_only_pending_is_submitted"), jrRows, row("state", "malformed", "email_norm", nil, "reason", "r", "submitted_at", "2030-01-01T00:00:00Z"))
		ledgerRefusedInsert(t, tx, "a suppressed row that has been submitted", "23514", check("job_rows_only_pending_is_submitted"), jrRows, row("state", "suppressed", "submitted_at", "2030-01-01T00:00:00Z"))
	})

	t.Run("the fields are an object", func(t *testing.T) {
		ledgerOKInsert(t, tx, "control: the fields of a row", jrRows, row("fields", `{"first_name":"Ada","n":[1,2]}`))
		for _, f := range []string{`[]`, `"x"`, `null`, `1`, `true`} {
			ledgerRefusedInsert(t, tx, "the fields "+f, "23514", check("job_rows_fields_is_an_object"), jrRows, row("fields", f))
		}
		ledgerRefusedInsert(t, tx, "SQL NULL fields", "23502", ledgerWhy{Column: "fields"}, jrRows, row("fields", nil))
	})

	t.Run("the row belongs to a job and a user", func(t *testing.T) {
		ledgerRefusedInsert(t, tx, "a NULL job", "23502", ledgerWhy{Column: "job_id"}, jrRows, row("job_id", nil))
		ledgerRefusedInsert(t, tx, "a NULL user", "23502", ledgerWhy{Column: "user_id"}, jrRows, row("user_id", nil))
	})
}

// TestJobRows_FrozenWithTheJob: a row of a job's list is added, changed or removed only while the job is draft or previewed (23001, job_rows_frozen) - for each of the twelve statuses the three writes are tried, accepted
// in draft and previewed and refused in the ten others, the same write on the same row being the control; submitted_at is the one column that moves later, once and only once: stamped on any status, the same stamp
// written back is no change, another or a cleared one is refused; id, job_id, user_id, ordinal and created_at never change even in a draft.
// It fails if a confirmed job's list can change, if a draft's cannot, or if a submit can be stamped twice.
func TestJobRows_FrozenWithTheJob(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c3")
	a := ledgerUserA
	editable := map[string]bool{"draft": true, "previewed": true}
	frozen := ledgerWhy{Constraint: "job_rows_frozen"}

	for _, s := range jobStates {
		s := s
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			job := next()
			ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", job, a)
			ord := 0
			keep, drop := jrRow(next, &ord, job, a), jrRow(next, &ord, job, a)
			ledgerOKInsert(t, sp, "two rows of a draft job", jrRows, keep)
			ledgerOKInsert(t, sp, "two rows of a draft job", jrRows, drop)
			jobWalkTo(t, sp, job, s)
			add := jrRow(next, &ord, job, a)
			insertSQL, insertArgs := ledgerInsertInto(jrRows, add)
			writes := []struct {
				what, sql string
				args      []any
			}{
				{"a row added", insertSQL, insertArgs},
				{"a row changed", "UPDATE platform.job_rows SET email_raw = 'changed@example.com' WHERE id = $1", []any{keep["id"]}},
				{"a row removed", "DELETE FROM platform.job_rows WHERE id = $1", []any{drop["id"]}},
			}
			for _, w := range writes {
				if editable[s] {
					ledgerExpectOK(t, sp, fmt.Sprintf("%s of a %s job", w.what, s), w.sql, w.args...)
				} else {
					ledgerExpectRefused(t, sp, fmt.Sprintf("%s of a %s job", w.what, s), "23001", frozen, w.sql, w.args...)
				}
			}
			// the one column that moves later: stamped once, on any status
			ledgerExpectOK(t, sp, fmt.Sprintf("the submit stamp of a row of a %s job", s), "UPDATE platform.job_rows SET submitted_at = '2030-01-01T00:00:00Z' WHERE id = $1", keep["id"])
			ledgerExpectOK(t, sp, "control: the same stamp written back", "UPDATE platform.job_rows SET submitted_at = '2030-01-01T00:00:00Z' WHERE id = $1", keep["id"])
			ledgerExpectRefused(t, sp, "another stamp", "23001", frozen, "UPDATE platform.job_rows SET submitted_at = '2031-01-01T00:00:00Z' WHERE id = $1", keep["id"])
			ledgerExpectRefused(t, sp, "a cleared stamp", "23001", frozen, "UPDATE platform.job_rows SET submitted_at = NULL WHERE id = $1", keep["id"])
		})
	}

	t.Run("the identity of a row never changes, even in a draft", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			job := next()
			ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", job, a)
			ord := 0
			r := jrRow(next, &ord, job, a)
			ledgerOKInsert(t, sp, "a row", jrRows, r)
			ledgerExpectOK(t, sp, "control: its address changed in the draft", "UPDATE platform.job_rows SET email_raw = 'new@example.com', email_norm = 'new@example.com' WHERE id = $1", r["id"])
			for _, c := range []struct{ col, set string }{
				{"id", "id = gen_random_uuid()"}, {"job_id", "job_id = '" + ledgerJobA1b + "'"}, {"user_id", "user_id = '" + ledgerUserB + "'"}, {"ordinal", "ordinal = 99"}, {"created_at", "created_at = created_at + interval '1 day'"},
			} {
				ledgerExpectRefused(t, sp, "the "+c.col+" of a row of a draft job", "23001", frozen, "UPDATE platform.job_rows SET "+c.set+" WHERE id = $1", r["id"])
				ledgerExpectOK(t, sp, "control: the same "+c.col+" written back", "UPDATE platform.job_rows SET "+c.col+" = "+c.col+" WHERE id = $1", r["id"])
			}
			ledgerExpectOK(t, sp, "control: a submit stamp on a draft job's row is allowed once", "UPDATE platform.job_rows SET submitted_at = now() WHERE id = $1", r["id"])
		})
	})
}

// TestJobRows_CompositeForeignKeys: a row and a result carry their user and name their job by the pair (id, user_id), so neither can name another user's job (23503 job_rows_job_fk, job_results_job_fk), and a result
// names a row OF ITS OWN JOB at ITS OWN POSITION (23503 job_results_row_fk: an ordinal that is not its row's); the guard of the results answers first for a row of another job and a row that does not exist
// (23514 job_results_match_their_row: no such row in the job has no address to match), so those two are proved again with the guard disabled (a savepoint that is rolled back), where the foreign key
// refuses them. The legal twin of each is accepted.
// It fails if a tenant filter on user_id could be bypassed by a row that names another user's job, or if a result could carry a position other than its row's.
func TestJobRows_CompositeForeignKeys(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c4")
	ord := 0
	a, b := ledgerUserA, ledgerUserB
	m1job, m2job := ledgerJobA1, ledgerJobA2 // two jobs of user A, on two modules (two slots)
	r1, r2 := jrRow(next, &ord, m1job, a), jrRow(next, &ord, m2job, a)
	ledgerOKInsert(t, tx, "control: a row of user A's job", jrRows, r1)
	ledgerOKInsert(t, tx, "control: a row of user A's other job", jrRows, r2)
	ledgerRefusedInsert(t, tx, "a row that names user A's job and user B", "23503", ledgerWhy{Constraint: "job_rows_job_fk"}, jrRows, jrRow(next, &ord, m1job, b))
	ledgerRefusedInsert(t, tx, "a row of a job that does not exist (the guard of the list answers before the key)", "23001", ledgerWhy{Constraint: "job_rows_frozen"}, jrRows, jrRow(next, &ord, ledgerNoSuchID, a))
	jobWalkTo(t, tx, m1job, "confirmed")
	jobWalkTo(t, tx, m2job, "confirmed")

	rowFK, matches := ledgerWhy{Constraint: "job_results_row_fk"}, ledgerWhy{Constraint: "job_results_match_their_row"}
	ledgerOKInsert(t, tx, "control: the result of a row of its own job", jrResults, jrResultOf(r1))
	ledgerRefusedInsert(t, tx, "a result that names user A's job and user B", "23503", ledgerWhy{Constraint: "job_results_job_fk"}, jrResults, jrResultOf(r2, "user_id", b))
	ledgerRefusedInsert(t, tx, "a result whose ordinal is not its row's", "23503", rowFK, jrResults, jrResultOf(r2, "ordinal", 99))
	ledgerRefusedInsert(t, tx, "a result at the ordinal of another row of its job", "23503", rowFK, jrResults, jrResultOf(r2, "ordinal", r1["ordinal"]))
	ledgerRefusedInsert(t, tx, "a result of a row of ANOTHER job (the guard of the results: that job has no such row)", "23514", matches, jrResults, jrResultOf(r1, "job_id", m2job))
	ledgerRefusedInsert(t, tx, "a result of a row that does not exist (the guard of the results)", "23514", matches, jrResults, jrResultOf(r2, "item_ref", ledgerNoSuchID))
	ledgerTrial(t, tx, func(sp pgx.Tx) { // the second wall, with the first one down
		ledgerMust(t, sp, "ALTER TABLE platform.job_results DISABLE TRIGGER job_results_after_confirm")
		ledgerTrial(t, sp, func(sp2 pgx.Tx) {
			ledgerOKInsert(t, sp2, "control (the guard disabled): the result of a row of its own job", jrResults, jrResultOf(r2))
		})
		ledgerRefusedInsert(t, sp, "the guard disabled: a result of a row of ANOTHER job", "23503", rowFK, jrResults, jrResultOf(r1, "job_id", m2job))
		ledgerRefusedInsert(t, sp, "the guard disabled: a result of a row that does not exist", "23503", rowFK, jrResults, jrResultOf(r2, "item_ref", ledgerNoSuchID))
		ledgerRefusedInsert(t, sp, "the guard disabled: a result whose ordinal is not its row's", "23503", rowFK, jrResults, jrResultOf(r2, "ordinal", 99))
	})
	ledgerOKInsert(t, tx, "control: the result of a row of the other job", jrResults, jrResultOf(r2))
	ledgerRefusedInsert(t, tx, "a second result of the same row (the key is the row)", "23505", ledgerWhy{Constraint: "job_results_pk"}, jrResults, jrResultOf(r2, "module_state", "bounced"))
}

// TestJobResults_Checks: each CHECK of platform.job_results refuses its row for ITS name beside a legal result and with its NULL case: the origin is module or platform; the decision is charge, free or unmapped; a
// charged result names its unit and no other result does (jobs_charged_names_its_unit is built from IS NOT NULL: a charge with a NULL unit and a free result with one are both refused); the module state and the
// rule are not blank; the detail is an object; the NOT NULL columns refuse NULL; the reason may be NULL; the result names the address of ITS row (23514 job_results_match_their_row: another row's address, the
// raw text of a row that has a normalised one, and no address at all are refused; the normalised address of a row, and the raw text of a malformed row that has none, are accepted). The guard of the results
// answers before the NOT NULL check for a NULL job, a NULL item reference and a NULL address, so those three NOT NULLs are proved with the guard disabled (a savepoint that is rolled back).
// It fails if a result can say less than the design demands, name another row's address, or let a NULL through.
func TestJobResults_Checks(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c5")
	ord := 0
	a, job := ledgerUserA, ledgerJobA2
	var rows []ledgerCols
	for i := 0; i < 40; i++ {
		r := jrRow(next, &ord, job, a)
		ledgerOKInsert(t, tx, "a row of the draft job", jrRows, r)
		rows = append(rows, r)
	}
	malformed := jrRow(next, &ord, job, a, "state", "malformed", "email_norm", nil, "email_raw", "no-at-sign", "reason", "no domain")
	ledgerOKInsert(t, tx, "a malformed row of the draft job", jrRows, malformed)
	jobWalkTo(t, tx, job, "confirmed")
	used := 0
	res := func(kv ...any) ledgerCols { // a result of the next unused row (rows[used] before the call is the row it will name)
		used++
		return jrResultOf(rows[used-1], kv...)
	}
	check := func(c string) ledgerWhy { return ledgerWhy{Constraint: c} }
	ledgerOKInsert(t, tx, "control: a free result written by the module", jrResults, res())
	ledgerOKInsert(t, tx, "control: a charged result names its unit", jrResults, res("decision", "charge", "unit", "m2_accepted", "rule", "M2-01"))
	ledgerOKInsert(t, tx, "control: an unmapped result (nothing is charged for it) with the platform's reason", jrResults, res("decision", "unmapped", "origin", "platform", "reason", "unknown outcome", "rule", "M2-99"))
	ledgerOKInsert(t, tx, "control: a result with a detail", jrResults, res("detail", `{"page": 3}`))

	ledgerRefusedInsert(t, tx, "an origin that is neither", "23514", check("job_results_origin_vocabulary"), jrResults, res("origin", "bogus"))
	ledgerRefusedInsert(t, tx, "a decision that is none of the three", "23514", check("job_results_decision_vocabulary"), jrResults, res("decision", "bogus"))
	ledgerRefusedInsert(t, tx, "a charge that names no unit (the NULL case)", "23514", check("job_results_charged_names_its_unit"), jrResults, res("decision", "charge"))
	ledgerRefusedInsert(t, tx, "a free result that names a unit", "23514", check("job_results_charged_names_its_unit"), jrResults, res("unit", "m2_accepted"))
	ledgerRefusedInsert(t, tx, "an unmapped result that names a unit", "23514", check("job_results_charged_names_its_unit"), jrResults, res("decision", "unmapped", "unit", "m2_accepted"))
	ledgerRefusedInsert(t, tx, "a blank module state", "23514", check("job_results_state_not_blank"), jrResults, res("module_state", "   "))
	ledgerRefusedInsert(t, tx, "a blank rule", "23514", check("job_results_state_not_blank"), jrResults, res("rule", "   "))
	for _, d := range []string{`[]`, `"x"`, `null`, `1`} {
		ledgerRefusedInsert(t, tx, "the detail "+d, "23514", check("job_results_detail_is_an_object"), jrResults, res("detail", d))
	}
	for _, col := range []string{"origin", "module_state", "decision", "rule", "detail", "ordinal", "user_id"} {
		ledgerRefusedInsert(t, tx, "a NULL "+col, "23502", ledgerWhy{Column: col}, jrResults, res(col, nil))
	}
	// the guard of the results (BEFORE INSERT, ahead of the NOT NULL check) answers first: a NULL job is no job (no status), a NULL item reference is no row and a NULL address is not the row's
	matches := check("job_results_match_their_row")
	ledgerRefusedInsert(t, tx, "a NULL job_id", "23514", check("jobs_results_after_confirm"), jrResults, res("job_id", nil))
	ledgerRefusedInsert(t, tx, "a NULL item_ref", "23514", matches, jrResults, res("item_ref", nil))
	ledgerRefusedInsert(t, tx, "a NULL email", "23514", matches, jrResults, res("email", nil))
	ledgerTrial(t, tx, func(sp pgx.Tx) { // the NOT NULLs behind the guard, with the guard down
		ledgerMust(t, sp, "ALTER TABLE platform.job_results DISABLE TRIGGER job_results_after_confirm")
		ledgerTrial(t, sp, func(sp2 pgx.Tx) {
			ledgerOKInsert(t, sp2, "control (the guard disabled): a legal result", jrResults, res())
		})
		for _, col := range []string{"job_id", "item_ref", "email"} {
			ledgerRefusedInsert(t, sp, "the guard disabled: a NULL "+col, "23502", ledgerWhy{Column: col}, jrResults, res(col, nil))
		}
	})
	// the result names the address of its own row
	ledgerRefusedInsert(t, tx, "a result with ANOTHER row's address", "23514", matches, jrResults, res("email", rows[len(rows)-1]["email_norm"]))
	ledgerRefusedInsert(t, tx, "a result with the RAW text of a row that has a normalised address", "23514", matches, jrResults, res("email", rows[used]["email_raw"]))
	ledgerRefusedInsert(t, tx, "a result with an address that is no row's", "23514", matches, jrResults, res("email", "nobody@example.com"))
	ledgerRefusedInsert(t, tx, "a result of the malformed row with an address that is not its raw text", "23514", matches, jrResults, jrResultOf(malformed, "email", "no-at-sign@example.com"))
	ledgerOKInsert(t, tx, "control: a result of the malformed row names its raw text (it has no normalised address)", jrResults, jrResultOf(malformed, "origin", "platform", "module_state", "malformed", "rule", "P-01", "reason", "no domain"))
	ledgerOKInsert(t, tx, "control: the reason and the unit may be NULL (a free result of the module with no reason)", jrResults, res("reason", nil, "unit", nil))
}

// TestJobResults_AfterConfirmAndAppendOnly: no result exists for a job that has not been confirmed (23514 jobs_results_after_confirm for draft and previewed; the same row's result is accepted for each of the ten
// other statuses, so the refusal is the status's); a result is written once and never changed or removed, for anybody: platform_app holds no UPDATE, DELETE or TRUNCATE (42501), and the table's owner and the
// superuser are refused by the append-only triggers (23001); platform_app may insert one (the control).
// It fails if a result can be recorded early or changed afterwards.
func TestJobResults_AfterConfirmAndAppendOnly(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c6")
	a := ledgerUserA

	for _, s := range jobStates {
		s := s
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			job := next()
			ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", job, a)
			ord := 0
			r := jrRow(next, &ord, job, a)
			ledgerOKInsert(t, sp, "a row of the draft job", jrRows, r)
			jobWalkTo(t, sp, job, s)
			result := jrResultOf(r)
			if s == "draft" || s == "previewed" {
				ledgerRefusedInsert(t, sp, "a result for a "+s+" job", "23514", ledgerWhy{Constraint: "jobs_results_after_confirm"}, jrResults, result)
				return
			}
			ledgerOKInsert(t, sp, "control: the result of the same row for a "+s+" job", jrResults, result)
		})
	}
	ledgerTrial(t, tx, func(sp pgx.Tx) { // the same job: refused while previewed, accepted once confirmed
		job := next()
		ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", job, a)
		ord := 0
		r := jrRow(next, &ord, job, a)
		ledgerOKInsert(t, sp, "a row of the draft job", jrRows, r)
		jobWalkTo(t, sp, job, "previewed")
		ledgerRefusedInsert(t, sp, "a result for the previewed job", "23514", ledgerWhy{Constraint: "jobs_results_after_confirm"}, jrResults, jrResultOf(r))
		jobWalkTo(t, sp, job, "confirmed")
		ledgerOKInsert(t, sp, "control: the result of the same row once the job is confirmed", jrResults, jrResultOf(r))
	})

	job := next()
	ledgerMust(t, tx, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", job, a)
	ord := 0
	r1, r2 := jrRow(next, &ord, job, a), jrRow(next, &ord, job, a)
	ledgerOKInsert(t, tx, "a row", jrRows, r1)
	ledgerOKInsert(t, tx, "a row", jrRows, r2)
	jobWalkTo(t, tx, job, "confirmed")
	ledgerOKInsert(t, tx, "a result", jrResults, jrResultOf(r1))
	mutations := []string{"UPDATE platform.job_results SET rule = 'edited'", "DELETE FROM platform.job_results", "TRUNCATE platform.job_results"}
	for _, role := range []string{"", "platform_owner"} { // the superuser, then the table's owner: the privileges are theirs and the triggers still refuse
		if role != "" {
			ledgerSwitchRole(t, tx, role)
		}
		for _, m := range mutations {
			ledgerExpectRefused(t, tx, fmt.Sprintf("%q: %s", role, m), "23001", ledgerWhy{Message: "append-only"}, m)
		}
		if role != "" {
			ledgerResetRole(t, tx)
		}
	}
	for _, role := range []string{"platform_app", "platform_admin"} {
		ledgerSwitchRole(t, tx, role)
		for _, m := range mutations {
			ledgerExpectRefused(t, tx, fmt.Sprintf("%s: %s", role, m), "42501", ledgerWhy{Message: "permission denied for table job_results"}, m)
		}
		ledgerResetRole(t, tx)
	}
	ledgerSwitchRole(t, tx, "platform_app")
	sql, args := ledgerInsertInto(jrResults, jrResultOf(r2))
	ledgerExpectOK(t, tx, "control: platform_app records a result", sql, args...)
	ledgerResetRole(t, tx)
	ledgerSwitchRole(t, tx, "platform_admin")
	sql, args = ledgerInsertInto(jrResults, jrResultOf(r2, "module_state", "bounced"))
	ledgerExpectRefused(t, tx, "platform_admin records a result", "42501", ledgerWhy{Message: "permission denied for table job_results"}, sql, args...)
	ledgerResetRole(t, tx)
}

// TestJobRows_PrivilegesOfTheRoles: platform_app inserts and deletes rows and may update the eight columns the preview and the engine write (and stamps submitted_at), but never id, job_id, user_id, ordinal or
// created_at (42501); platform_admin reads and writes nothing.
// It fails if a grant is wider or narrower than the design (the catalogue twin is TestGrantsInvariant, the whole matrix TestRoles_*).
func TestJobRows_PrivilegesOfTheRoles(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("c7")
	ord := 0
	a, job := ledgerUserA, ledgerJobA2
	r := jrRow(next, &ord, job, a)
	ledgerOKInsert(t, tx, "a row (as the superuser)", jrRows, r)
	denied := ledgerWhy{Message: "permission denied for table job_rows"}

	ledgerSwitchRole(t, tx, "platform_app")
	mine := jrRow(next, &ord, job, a)
	sql, args := ledgerInsertInto(jrRows, mine)
	ledgerExpectOK(t, tx, "control: platform_app inserts a row", sql, args...)
	for _, set := range []string{"email_raw = 'x@example.com'", "email_norm = 'x@example.com'", "state = 'pending'", "reason = 'r'", "duplicate_of = NULL", `fields = '{"a":1}'`, "source_item = NULL", "submitted_at = now()"} {
		ledgerExpectOK(t, tx, "control: platform_app updates "+set, "UPDATE platform.job_rows SET "+set+" WHERE id = $1", r["id"])
	}
	for _, c := range []string{"id = id", "job_id = job_id", "user_id = user_id", "ordinal = ordinal", "created_at = created_at"} {
		ledgerExpectRefused(t, tx, "platform_app updates "+c+" (not even to itself)", "42501", denied, "UPDATE platform.job_rows SET "+c+" WHERE id = $1", r["id"])
	}
	ledgerExpectOK(t, tx, "control: platform_app deletes a row of a draft job", "DELETE FROM platform.job_rows WHERE id = $1", mine["id"])
	ledgerResetRole(t, tx)

	ledgerSwitchRole(t, tx, "platform_admin")
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_rows"); n < 1 {
		t.Fatalf("platform_admin reads %d rows", n)
	}
	sql, args = ledgerInsertInto(jrRows, jrRow(next, &ord, job, a))
	ledgerExpectRefused(t, tx, "platform_admin inserts a row", "42501", denied, sql, args...)
	ledgerExpectRefused(t, tx, "platform_admin updates a row", "42501", denied, "UPDATE platform.job_rows SET email_raw = 'x@example.com' WHERE id = $1", r["id"])
	ledgerExpectRefused(t, tx, "platform_admin deletes a row", "42501", denied, "DELETE FROM platform.job_rows WHERE id = $1", r["id"])
	ledgerResetRole(t, tx)
}

// TestJobResults_SettlementDrift drives a metered job through the real money store (a reserve, a settle that charges the rows it is given) and writes its results by hand, and asks platform.job_settlement_drift
// about it. A clean job (every charged row has its result with the same unit, every charged result has its charge, a terminal job has as many results as rows) leaves the view EMPTY, also once it has ended; and
// each of the four ways the three records can disagree is reported with its own word and for its own job and row: a ledger charge with no result (charge_without_result), a result that names another unit than
// the charge (that, and result_without_charge, for the same row), a charged result with no ledger charge (result_without_charge), and a terminal confirmed job with fewer results than rows
// (terminal_job_missing_results, no row). The paired control of each is the clean job and the same job once the missing result is written (a result may be added later: it is insert-only).
// It fails if the view misses a disagreement, names it wrongly, or reports a job that agrees.
func TestJobResults_SettlementDrift(t *testing.T) {
	e := newMnyEnv(t)
	e.fund(mnyUserA, "m2", 1000)
	e.fund(mnyUserB, "m2", 1000)
	e.fund(mnyUserC, "m2", 1000)
	seq := 0
	ordOf, mailOf := map[string]int{}, map[string]string{} // the position and the address of every row built, which its result must carry (job_results_row_fk, job_results_match_their_row)
	// a metered module 2 job with n rows, reserved and running
	build := func(user string, n int) (string, []string) {
		e.t.Helper()
		job := e.newJob(user, "m2")
		var rows []string
		for i := 1; i <= n; i++ {
			seq++
			id := fmt.Sprintf("c9000000-0000-4000-8000-%012d", seq)
			mail := fmt.Sprintf("row%d@example.com", seq)
			e.exec("INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm) VALUES ($1, $2, $3, $4, $5, $5)", id, job, user, i, mail)
			ordOf[id], mailOf[id] = i, mail
			rows = append(rows, id)
		}
		e.reserve(job, n)
		e.setStatus(job, "running")
		return job, rows
	}
	result := func(job, user, row, decision, unit, rule string) {
		e.t.Helper()
		var u any
		if unit != "" {
			u = unit
		}
		e.exec("INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, unit, rule) VALUES ($1, $2, $3, $4, $5, 'module', 'sent', $6, $7, $8)", job, user, row, ordOf[row], mailOf[row], decision, u, rule)
	}
	drift := func(job string) []string {
		e.t.Helper()
		return e.rows("SELECT COALESCE(item_ref, '-') || '|' || problem FROM platform.job_settlement_drift WHERE job_id = $1 ORDER BY 1", job)
	}
	requireDrift := func(what string, job string, want ...string) {
		e.t.Helper()
		got := drift(job)
		if strings.Join(got, ";") != strings.Join(want, ";") {
			e.t.Fatalf("%s: platform.job_settlement_drift says %v for the job, want %v", what, got, want)
		}
	}

	// the clean job: two charged rows and a free one, results equal, ended
	clean, cr := build(mnyUserA, 3)
	e.settle(clean, mnyItem(cr[0], mnyM2Sent), mnyItem(cr[1], mnyM2Sent), mnyItem(cr[2], mnyM2Failed))
	if n := e.chargeRows(clean); n != 2 {
		t.Fatalf("the clean job holds %d charge rows, want 2", n)
	}
	result(clean, mnyUserA, cr[0], "charge", "m2_accepted", "M2-01")
	result(clean, mnyUserA, cr[1], "charge", "m2_accepted", "M2-01")
	requireDrift("the control: a clean job with two of its three results (it is running, so the missing one is not yet a drift)", clean)
	result(clean, mnyUserA, cr[2], "free", "", "M2-03")
	requireDrift("the control: a clean running job", clean)
	e.setStatus(clean, "succeeded")
	requireDrift("the control: the clean job, ended with as many results as rows", clean)

	// a ledger charge with no result
	j1, r1 := build(mnyUserB, 2)
	e.settle(j1, mnyItem(r1[0], mnyM2Sent), mnyItem(r1[1], mnyM2Sent))
	result(j1, mnyUserB, r1[0], "charge", "m2_accepted", "M2-01")
	requireDrift("a charge whose result was never written", j1, r1[1]+"|charge_without_result")
	result(j1, mnyUserB, r1[1], "charge", "m2_accepted", "M2-01")
	requireDrift("control: the same job once the result is written", j1)
	e.setStatus(j1, "succeeded")
	requireDrift("control: that job, ended", j1)

	// a result that names another unit than the charge
	j2, r2 := build(mnyUserC, 1)
	e.settle(j2, mnyItem(r2[0], mnyM2Sent))
	result(j2, mnyUserC, r2[0], "charge", "m2_bounced", "M2-02")
	requireDrift("a result that names another unit than the charge", j2, r2[0]+"|charge_without_result", r2[0]+"|result_without_charge")

	// a charged result with no ledger charge
	j3, r3 := build(mnyUserA, 1)
	result(j3, mnyUserA, r3[0], "charge", "m2_accepted", "M2-01")
	requireDrift("a charged result of a row the ledger never charged", j3, r3[0]+"|result_without_charge")

	// a terminal confirmed job with fewer results than rows (the user B job above has ended, so its slot is free)
	j4, r4 := build(mnyUserB, 2)
	result(j4, mnyUserB, r4[0], "free", "", "M2-03")
	requireDrift("control: a running job with one result of two rows is not yet a drift", j4)
	e.setStatus(j4, "succeeded")
	requireDrift("a terminal job with one result for two rows", j4, "-|terminal_job_missing_results")
	e.requireViewsEmpty("at the end (the money invariants are not the settlement proof)")
}
