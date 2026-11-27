//go:build harness

package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The migration lifecycle of platform_0019 (the two states a send job's confirm leaves rows in when its templates cannot be rendered for them, Phase 5 S2). The chain test (migrate_lifecycle_test.go)
// calls templateRowsObjects before and after the down of 0019; the refusal of the down while a row is in either state is its own test below.

// templateRowsObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0019's down: before it, the two CHECKs of the rows (the state vocabulary and the reason rule) name missing_field and
// unrenderable and the comment on the state column says so, and the rules bite (lifecycleTemplateRows); after it the two CHECKs are platform_0012's text again (five states, only a malformed or excluded row
// must say why), the comment is gone, and a row in either state is refused for the vocabulary. The two constraints are there either way (the down puts the older ones back, it does not drop them).
func templateRowsObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.job_rows'::regclass AND contype = 'c' AND conname::text IN ('job_rows_state_vocabulary', 'job_rows_reason_shape')"); got != 2 {
		t.Fatalf("%s: found %d of the two CHECKs job_rows_state_vocabulary and job_rows_reason_shape, want both (the down puts the older ones back, it does not drop them)", when, got)
	}
	expect("the state vocabulary naming missing_field and unrenderable", 1,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.job_rows'::regclass AND contype = 'c' AND conname::text = 'job_rows_state_vocabulary' AND pg_get_constraintdef(oid) LIKE '%''missing_field''%' AND pg_get_constraintdef(oid) LIKE '%''unrenderable''%'")
	expect("the reason rule naming missing_field and unrenderable", 1,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.job_rows'::regclass AND contype = 'c' AND conname::text = 'job_rows_reason_shape' AND pg_get_constraintdef(oid) LIKE '%''missing_field''%' AND pg_get_constraintdef(oid) LIKE '%''unrenderable''%'")
	expect("the comment on the state column", 1,
		"SELECT count(*) FROM pg_description WHERE objoid = 'platform.job_rows'::regclass AND objsubid = (SELECT attnum FROM pg_attribute WHERE attrelid = 'platform.job_rows'::regclass AND attname = 'state') AND description LIKE '%missing_field%'")
	lifecycleTemplateRows(t, c, present, when)
}

// lifecycleTemplateRows is the behavioural half of templateRowsObjects, in a transaction that is ALWAYS rolled back. With platform_0019 a draft send job takes a row in either new state when it says why, and
// refuses one that does not (23514, job_rows_reason_shape: the NULL case and the blank case); without it the same row is refused for the vocabulary (23514, job_rows_state_vocabulary) and the older rule
// (a malformed or excluded row says why) still bites.
func lifecycleTemplateRows(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	const row = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state, reason) VALUES ($1, $2, $3, $4, $5, $5, $6, $7)"
	if !enforced {
		ledgerExpectRefused(t, tx, when+": a missing_field row (the older chain has five states)", "23514", ledgerWhy{Constraint: "job_rows_state_vocabulary"}, row, ledgerUUID("fd", 1), ledgerJobA2, ledgerUserA, 1, "a@example.com", "missing_field", "first_name")
		ledgerExpectRefused(t, tx, when+": an unrenderable row", "23514", ledgerWhy{Constraint: "job_rows_state_vocabulary"}, row, ledgerUUID("fd", 2), ledgerJobA2, ledgerUserA, 2, "b@example.com", "unrenderable", "subject_blank")
		ledgerExpectRefused(t, tx, when+": an excluded row with no reason (the older rule is the one that is left)", "23514", ledgerWhy{Constraint: "job_rows_reason_shape"}, row, ledgerUUID("fd", 3), ledgerJobA2, ledgerUserA, 3, "c@example.com", "excluded", nil)
		return
	}
	ledgerExpectOK(t, tx, when+": a missing_field row with its field names", row, ledgerUUID("fd", 1), ledgerJobA2, ledgerUserA, 1, "a@example.com", "missing_field", "first_name,plan")
	ledgerExpectOK(t, tx, when+": an unrenderable row with its word", row, ledgerUUID("fd", 2), ledgerJobA2, ledgerUserA, 2, "b@example.com", "unrenderable", "subject_blank")
	ledgerExpectRefused(t, tx, when+": a missing_field row with no reason", "23514", ledgerWhy{Constraint: "job_rows_reason_shape"}, row, ledgerUUID("fd", 3), ledgerJobA2, ledgerUserA, 3, "c@example.com", "missing_field", nil)
	ledgerExpectRefused(t, tx, when+": an unrenderable row with a blank reason", "23514", ledgerWhy{Constraint: "job_rows_reason_shape"}, row, ledgerUUID("fd", 4), ledgerJobA2, ledgerUserA, 4, "d@example.com", "unrenderable", "  ")
}

// TestRunnerLifecycle_Down0019RefusesWhileARowIsLeftOutByTheTemplates: platform_0019's down says no (SQLSTATE 55000, naming how many) while any row is in state missing_field or unrenderable: the older
// vocabulary cannot hold the state, and the row has a platform result (P-09, P-10) that is kept for ever, so it is not rewritten into another state either. The database is left exactly as it was (still at
// 0019). The control is the same down once the row is gone (a draft's list may change): it passes and the two CHECKs are the older ones.
// It fails if the guard of the down is missing, if it counts wrongly, or if a refused down changes anything.
func TestRunnerLifecycle_Down0019RefusesWhileARowIsLeftOutByTheTemplates(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1
	for i, v := range chain {
		if v == "0019" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0019 after other migrations", chain)
	}
	seed, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSeed(t, seed)
	if err := seed.Commit(ctx); err != nil { // the migrator runs on its own connection: the rows must be committed
		t.Fatal(err)
	}
	migRollBackAbove(t, admin, envDown, chain, at)

	skipped := func() int64 {
		return ledgerInt64(t, admin, "SELECT count(*) FROM platform.job_rows WHERE state IN ('missing_field', 'unrenderable')")
	}
	refused := func(what string, n int) {
		t.Helper()
		res := runMigrator(t, envDown, "down", "0019")
		mustExit(t, res, 1, "down 0019 with "+what)
		mustContain(t, res.Stderr, fmt.Sprintf("platform_0019 down: %d row(s) are in state missing_field or unrenderable", n), "down 0019 with "+what)
		mustContain(t, res.Stderr, "55000", "down 0019 with "+what)
		if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
			t.Fatalf("a refused down 0019 (%s) changed the database: applied = %v, want %v", what, got, chain[:at+1])
		}
		if got := skipped(); got != int64(n) {
			t.Fatalf("a refused down 0019 (%s) left %d row(s) in the new states, want %d", what, got, n)
		}
	}
	const row = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state, reason) VALUES ($1, $2, $3, $4, $5, $5, $6, $7)"
	first, second := ledgerUUID("fe", 1), ledgerUUID("fe", 2)
	ledgerMust(t, admin, row, first, ledgerJobA2, ledgerUserA, 1, "a@example.com", "missing_field", "first_name")
	refused("a missing_field row", 1)
	ledgerMust(t, admin, row, second, ledgerJobA2, ledgerUserA, 2, "b@example.com", "unrenderable", "subject_blank")
	refused("a missing_field row and an unrenderable one", 2)

	// the control: with the two rows gone (a draft's list may change) the same down passes, and the older CHECKs are back
	ledgerMust(t, admin, "DELETE FROM platform.job_rows WHERE id = ANY($1::text[]::uuid[])", []string{first, second})
	mustExit(t, runMigrator(t, envDown, "down", "0019"), 0, "down 0019 once nothing is left out")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0019 that passed: applied = %v, want %v", got, chain[:at])
	}
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.job_rows'::regclass AND conname::text = 'job_rows_state_vocabulary' AND pg_get_constraintdef(oid) LIKE '%missing_field%'"); n != 0 {
		t.Fatalf("after the down 0019 that passed the state vocabulary still names missing_field")
	}
}
