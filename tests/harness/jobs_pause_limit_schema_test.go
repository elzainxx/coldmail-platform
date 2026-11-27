//go:build harness

package harness

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Phase 5 S7 and S8, the SCHEMA half: platform_0018's paused_at and content_cleared_at, their walls, and the two moves the engine gained. The engine against a database (the sweep, the purge, the races) is in
// jobs_pause_limit_test.go and jobs_content_retention_test.go; the names of this file start with `pls`.
//
//	PausedAt...                  paused_at is the database's: set by the move into paused, gone with the move out, overwritten when a writer states it, changed by no write that leaves the status alone (for the owner of
//	                             the data as for the superuser), and a job has it exactly when it is paused (the NULL case too)
//	ContentClearedAt...          set once, only for a terminal job, never before it ended; platform_app may write it and the administrator role may not
//	TheEngineMayEndAPause...     paused -> cancelled and paused -> cancelling are the engine's too, and each keeps its wall (a job its module holds is never cancelled directly; one that never started never goes through
//	                             the engine; a module that cannot cancel is refused), with the lifecycle columns the guard sets

// pauseLimitObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0018's down (TestRunnerLifecycle): before it, the two columns, the two CHECKs, the two partial indexes, the set-once trigger and
// its function are there, the guard names paused_at and the engine holds the two moves; after it, none of them is, and the guard is platform_0016's text again.
func pauseLimitObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	expect("the columns paused_at and content_cleared_at of platform.jobs", 2,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.jobs'::regclass AND NOT attisdropped AND attname::text IN ('paused_at', 'content_cleared_at')")
	expect("the CHECKs jobs_paused_at_shape and jobs_content_cleared_after_the_end", 2,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.jobs'::regclass AND contype = 'c' AND conname::text IN ('jobs_paused_at_shape', 'jobs_content_cleared_after_the_end')")
	expect("the partial indexes jobs_paused_at_idx and jobs_content_retention_idx", 2,
		"SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND tablename = 'jobs' AND indexname::text IN ('jobs_paused_at_idx', 'jobs_content_retention_idx')")
	expect("the trigger jobs_content_cleared_once and its function", 2,
		`SELECT (SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = 'platform.jobs'::regclass AND tgname::text = 'jobs_content_cleared_once')
		      + (SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_content_cleared_once')`)
	expect("the guard that sets NEW.paused_at", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_transition_guard' AND prosrc LIKE '%NEW.paused_at%'")
	expect("the actor engine on paused -> cancelled and paused -> cancelling", 2,
		"SELECT count(*) FROM platform.job_transitions WHERE from_status = 'paused' AND to_status IN ('cancelled', 'cancelling') AND 'engine' = ANY (actors)")
	// what the migration stands beside and never removes: the three walls of platform_0016 are in the guard either way
	if got := ledgerInt64(t, c, `SELECT count(*) FROM unnest(ARRAY['job_unsubmitted_pause_never_cancelling', 'job_unsubmitted_pause_never_completing', 'job_submitted_pause_cancels_through_the_engine']) AS w(name)
		 WHERE EXISTS (SELECT 1 FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_transition_guard' AND prosrc LIKE '%' || w.name || '%')`); got != 3 {
		t.Fatalf("%s: %d of the three walls of platform_0016 are in the guard, want 3 (platform_0018 carries them and its down restores them)", when, got)
	}
}

func plsIDs() func() string { return jobIDs("b8") }

func TestPauseLimitSchema_PausedAtIsTheDatabasesAndMovesOnlyWithAMove(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := plsIDs()
	a := ledgerUserA
	moves := ledgerWhy{Constraint: "job_lifecycle_columns_move_with_a_transition"}
	shape := ledgerWhy{Constraint: "jobs_paused_at_shape"}
	guardOff := func(sp pgx.Tx) { ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard") }
	pausedAtIsNow := func(sp pgx.Tx, id string) bool {
		return ledgerBool(t, sp, "SELECT paused_at = now() FROM platform.jobs WHERE id = $1", id)
	}

	t.Run("it is the database's now() of the move into paused, NULL in every other status, and a writer's value is overwritten", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "scheduled")
			if !ledgerBool(t, sp, "SELECT paused_at IS NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a scheduled job has a paused_at")
			}
			jobHop(t, sp, id, "paused")
			if !pausedAtIsNow(sp, id) {
				t.Fatalf("a paused job's paused_at is %s, want the transaction's now() (the database's clock at the move)", ledgerString(t, sp, "SELECT paused_at::text FROM platform.jobs WHERE id = $1", id))
			}
			jobHop(t, sp, id, "scheduled") // the resume
			if !ledgerBool(t, sp, "SELECT paused_at IS NULL AND paused_from IS NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a resumed job keeps its paused_at: a pause that ended is not a pause")
			}
			jobHop(t, sp, id, "paused") // a second pause starts a new count
			if !pausedAtIsNow(sp, id) {
				t.Fatal("a job paused a second time did not get a new paused_at")
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) { // a writer that states the time inside the move: whatever it said is overwritten
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "scheduled")
			ledgerExpectOK(t, sp, "a pause whose statement also says paused_at = 2001", jobAsOnce("user", a, "why", fmt.Sprintf("UPDATE platform.jobs SET status = 'paused', paused_at = '2001-01-01T00:00:00Z' WHERE id = '%s'", id)))
			if !pausedAtIsNow(sp, id) {
				t.Fatalf("a writer chose the time of the pause: paused_at is %s", ledgerString(t, sp, "SELECT paused_at::text FROM platform.jobs WHERE id = $1", id))
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) { // a job that left paused by any move has none: cancelled, and the engine's own way out
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			jobHop(t, sp, id, "cancelled")
			if !ledgerBool(t, sp, "SELECT paused_at IS NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a job cancelled out of a pause keeps its paused_at")
			}
		})
	})

	t.Run("no write that leaves the status alone moves it, for platform_app as for the superuser; the same value written back is no change", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			for _, set := range []string{"paused_at = paused_at - interval '1 day'", "paused_at = now() + interval '1 day'", "paused_at = NULL"} {
				ledgerExpectRefused(t, sp, "a paused job: "+set, "23001", moves, "UPDATE platform.jobs SET "+set+" WHERE id = $1", id)
			}
			ledgerExpectOK(t, sp, "control: the same value written back is no change", "UPDATE platform.jobs SET paused_at = paused_at WHERE id = $1", id)
			ledgerTrial(t, sp, func(sp2 pgx.Tx) {
				ledgerSwitchRole(t, sp2, "platform_app")
				ledgerExpectRefused(t, sp2, "platform_app moves paused_at back a day", "23001", moves, "UPDATE platform.jobs SET paused_at = paused_at - interval '1 day' WHERE id = $1", id)
				ledgerExpectOK(t, sp2, "control: platform_app writes the same value back", "UPDATE platform.jobs SET paused_at = paused_at WHERE id = $1", id)
			})
			// the route this closes: a paused job whose paused_at is pushed into the future would never reach its pause limit. With the guard off it exists (the control), with it on it is refused
			before := ledgerString(t, sp, "SELECT paused_at::text FROM platform.jobs WHERE id = $1", id)
			guardOff(sp)
			ledgerExpectOK(t, sp, "control: with the guard off the time can be pushed back (so it is the guard that refuses)", "UPDATE platform.jobs SET paused_at = paused_at + interval '30 days' WHERE id = $1", id)
			if after := ledgerString(t, sp, "SELECT paused_at::text FROM platform.jobs WHERE id = $1", id); after == before {
				t.Fatal("the control wrote nothing: the case proves nothing")
			}
		})
	})

	t.Run("a job has paused_at exactly when it is paused (jobs_paused_at_shape), the NULL case included", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "running")
			guardOff(sp)
			ledgerExpectRefused(t, sp, "a running job that says it was paused", "23514", shape, "UPDATE platform.jobs SET paused_at = now() WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: a running job that says nothing", "UPDATE platform.jobs SET paused_at = NULL WHERE id = $1", id)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			guardOff(sp)
			ledgerExpectRefused(t, sp, "a paused job with no time (the NULL case: a CHECK that evaluates to NULL passes, this one says IS NOT NULL)", "23514", shape, "UPDATE platform.jobs SET paused_at = NULL WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: a paused job's time written back", "UPDATE platform.jobs SET paused_at = paused_at WHERE id = $1", id)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			ledgerExpectRefused(t, sp, "a draft job born with a paused_at", "23514", shape,
				"INSERT INTO platform.jobs (id, user_id, module_id, paused_at) VALUES ($1, $2, 'm2', now())", id, a)
			ledgerExpectOK(t, sp, "control: a draft job born without one", "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", id, a)
		})
		// the fixture of the tests that insert a paused job with the guard off carries the time (jobShapeKV), so the vocabulary test still reaches the CHECK it is about
		kv := jobShapeKV("paused")
		hasTime := false
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] == "paused_at" {
				hasTime = true
			}
		}
		if !hasTime {
			t.Fatal("jobShapeKV(paused) carries no paused_at: the tests that insert a paused job with the guard off would be refused by jobs_paused_at_shape for a reason that has nothing to do with them")
		}
	})

	t.Run("the two partial indexes of the sweep and of the purge exist", func(t *testing.T) {
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND tablename = 'jobs' AND indexname IN ('jobs_paused_at_idx', 'jobs_content_retention_idx')"); n != 2 {
			t.Fatalf("%d of the two partial indexes of platform_0018 exist, want 2", n)
		}
	})
}

func TestPauseLimitSchema_ContentClearedAtIsSetOnceOnlyForAnEndedJobAndNeverBeforeItEnded(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := plsIDs()
	a := ledgerUserA
	once := ledgerWhy{Constraint: "jobs_content_cleared_is_set_once"}
	ended := ledgerWhy{Constraint: "jobs_content_cleared_after_the_end"}
	const mark = "UPDATE platform.jobs SET content_cleared_at = now() WHERE id = $1"

	t.Run("an ended job takes the mark once (the control of every refusal below), and the mark is the NULL it started as", func(t *testing.T) {
		for _, to := range []string{"succeeded", "failed", "cancelled", "expired"} {
			to := to
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, "m2", to)
				if !ledgerBool(t, sp, "SELECT content_cleared_at IS NULL FROM platform.jobs WHERE id = $1", id) {
					t.Fatalf("a %s job is born marked", to)
				}
				ledgerExpectOK(t, sp, "control: the mark of a "+to+" job", mark, id)
				ledgerExpectOK(t, sp, "control: the same mark written back is no change", "UPDATE platform.jobs SET content_cleared_at = content_cleared_at WHERE id = $1", id)
				ledgerExpectRefused(t, sp, "a "+to+" job's mark moved later", "23514", once, "UPDATE platform.jobs SET content_cleared_at = content_cleared_at + interval '1 second' WHERE id = $1", id)
				ledgerExpectRefused(t, sp, "a "+to+" job's mark cleared", "23514", once, "UPDATE platform.jobs SET content_cleared_at = NULL WHERE id = $1", id)
			})
		}
	})

	t.Run("a job that has not ended cannot be marked (jobs_content_cleared_after_the_end), whatever its status", func(t *testing.T) {
		for _, s := range []string{"draft", "previewed", "confirmed", "scheduled", "running", "paused", "cancelling", "completing"} {
			s := s
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, "m2", s)
				ledgerExpectRefused(t, sp, "a "+s+" job marked", "23514", ended, mark, id)
				ledgerExpectOK(t, sp, "control: a "+s+" job left unmarked (NULL passes: the CHECK says IS NULL)", "UPDATE platform.jobs SET content_cleared_at = NULL WHERE id = $1", id)
			})
		}
	})

	t.Run("never before the job ended", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "cancelled")
			ledgerExpectRefused(t, sp, "a mark one second before finished_at", "23514", ended, "UPDATE platform.jobs SET content_cleared_at = finished_at - interval '1 second' WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: a mark at finished_at", "UPDATE platform.jobs SET content_cleared_at = finished_at WHERE id = $1", id)
		})
	})

	t.Run("platform_app writes it and the administrator role does not", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "failed")
			ledgerTrial(t, sp, func(sp2 pgx.Tx) {
				ledgerSwitchRole(t, sp2, "platform_app")
				ledgerExpectOK(t, sp2, "control: platform_app marks the ended job (the store runs as this role)", mark, id)
			})
			ledgerTrial(t, sp, func(sp2 pgx.Tx) {
				ledgerSwitchRole(t, sp2, "platform_admin")
				ledgerExpectRefused(t, sp2, "platform_admin marks the ended job", "42501", ledgerWhy{Message: "permission denied"}, mark, id)
			})
		})
	})
}

func TestPauseLimitSchema_TheEngineMayEndAPauseAndEveryWallOfPlatform0016StillHoldsForIt(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := plsIDs()
	a := ledgerUserA
	why := func(c string) ledgerWhy { return ledgerWhy{Constraint: c} }
	const reason = "pause_limit: a test"

	for _, to := range []string{"cancelled", "cancelling"} {
		if got := ledgerString(t, tx, "SELECT (SELECT string_agg(a, ',' ORDER BY a) FROM unnest(actors) a) FROM platform.job_transitions WHERE from_status = 'paused' AND to_status = $1", to); got != "admin,engine,user" {
			t.Errorf("paused -> %s is taken by [%s], want admin, engine and user", to, got)
		}
	}
	// the control: a move the engine does not own is still not its to take
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		id := next()
		jobInsertAt(t, sp, id, a, "m2", "paused")
		ledgerExpectRefused(t, sp, "the engine resuming a paused job", "23001", why("job_transition_actor_not_allowed"), jobMoveSQL, id, "scheduled", "engine", nil, reason)
	})

	t.Run("a job paused before its start: cancelled by the engine at once, never through the engine", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			ledgerExpectRefused(t, sp, "the engine taking a job that never started through cancelling", "23001", why("job_unsubmitted_pause_never_cancelling"), jobMoveSQL, id, "cancelling", "engine", nil, reason)
			ledgerExpectOK(t, sp, "control: the engine cancelling it directly", jobMoveSQL, id, "cancelled", "engine", nil, reason)
			if !ledgerBool(t, sp, "SELECT status = 'cancelled' AND finished_at IS NOT NULL AND paused_at IS NULL AND paused_from IS NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("the cancelled job still carries its pause, or has no end time")
			}
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'transition' AND to_status = 'cancelled' AND actor_kind = 'engine' AND reason = $2", id, reason); n != 1 {
				t.Fatalf("%d engine events for the move, want 1 (who, why)", n)
			}
		})
	})

	t.Run("a job paused while running: through cancelling, never straight to cancelled, and only for a module that can cancel", func(t *testing.T) {
		for _, module := range []string{"m2", "m3"} {
			module := module
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, module, "running")
				jobHop(t, sp, id, "paused")
				ledgerExpectRefused(t, sp, module+": the engine ending a job its module holds directly", "23001", why("job_submitted_pause_cancels_through_the_engine"), jobMoveSQL, id, "cancelled", "engine", nil, reason)
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					ledgerMust(t, sp2, "UPDATE platform.modules SET can_cancel = false WHERE id = $1", module)
					ledgerExpectRefused(t, sp2, module+": the engine cancelling a paused job of a module that cannot cancel", "23001", why("job_module_cannot_cancel"), jobMoveSQL, id, "cancelling", "engine", nil, reason)
				})
				ledgerExpectOK(t, sp, module+": control: the engine taking it through cancelling", jobMoveSQL, id, "cancelling", "engine", nil, reason)
				if !ledgerBool(t, sp, "SELECT status = 'cancelling' AND cancel_requested_at IS NOT NULL AND finished_at IS NULL AND paused_at IS NULL AND paused_from IS NULL FROM platform.jobs WHERE id = $1", id) {
					t.Fatal("a job taken out of its pause through cancelling must record the cancel, not end, and carry no pause")
				}
				// the rest of the way is the engine's own cancelling step: completing, then cancelled (never succeeded)
				ledgerExpectOK(t, sp, module+": completing", jobMoveSQL, id, "completing", "engine", nil, "the module reports every item final")
				ledgerExpectRefused(t, sp, module+": a cancelled job cannot succeed", "23001", why("job_cancelled_cannot_succeed"), jobMoveSQL, id, "succeeded", "engine", nil, "why")
				ledgerExpectOK(t, sp, module+": and ends cancelled", jobMoveSQL, id, "cancelled", "engine", nil, "why")
			})
		}
	})

	t.Run("an engine move still names who and why", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			ledgerExpectRefused(t, sp, "the engine without a reason", "23001", why("job_transition_needs_an_actor"), jobMoveSQL, id, "cancelled", "engine", nil, "")
			ledgerExpectRefused(t, sp, "the engine carrying an actor id", "23001", why("job_transition_needs_an_actor"), jobMoveSQL, id, "cancelled", "engine", a, reason)
		})
	})
}
