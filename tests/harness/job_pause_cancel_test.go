//go:build harness

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// platform_0016 (Phase 4 close-out, item D1): the modules gained their per-job routes (modules 2 and 3 pause and resume ONE campaign, module 1 cancels ONE job), so the lifecycle needs the moves that follow:
// the capability seeds of platform.modules, the two legal pairs paused -> cancelling (user, admin) and paused -> completing (engine), and three walls of jobs_transition_guard for them. The EXPECTED LIST of
// pairs lives in job_lifecycle_test.go (jobLegal, held equal to the table by TestJobLifecycle_TheExpectedListIsTheTable) and the matrix there takes every pair for every actor; these tests are about the
// BEHAVIOUR that list cannot show: what the seeds are and that the guard reads them, what a job paused before its start and one paused while running can and cannot do next, how each of them ends, what the
// events say, that the slot is held, that a resume restarts the staleness clock, what the application role can do, and that the down of the migration refuses what the older lifecycle cannot end.
//
// Every test runs on a migrated scratch database inside transactions that are rolled back, every refusal names the constraint that raised it beside its legal twin, and every fixture is built with the hand-written
// chains of jobs_helpers_test.go (a job in the 'paused' chain paused from SCHEDULED; jobPauseWhileRunning makes one that paused from RUNNING).

// pcModules are the three modules, in the order of platform.modules.
var pcModules = []string{"m1", "m2", "m3"}

// pcActorText is the actor id a move of this kind carries in its event, as text: the owner of the fixture jobs (user A), the first admin, or "-" for the engine (a person is not involved).
func pcActorText(kind string) string {
	switch kind {
	case "user":
		return ledgerUserA
	case "admin":
		return ledgerAdminA
	}
	return "-"
}

// pcLastEvent is the newest event of the job as one line: kind|from>to|actor kind|actor id|reason.
func pcLastEvent(t *testing.T, db ledgerDB, id string) string {
	t.Helper()
	return ledgerString(t, db, `SELECT kind || '|' || COALESCE(from_status, '-') || '>' || COALESCE(to_status, '-') || '|' || actor_kind || '|' || COALESCE(acting_user_id::text, acting_admin_id::text, '-') || '|' || reason
		FROM platform.job_events WHERE job_id = $1 ORDER BY id DESC LIMIT 1`, id)
}

// pcMove takes one move of the job through the sanctioned function as the actor of this kind, requires it to be accepted, and requires exactly one new event that says who moved it, from and to which status, and why.
func pcMove(t *testing.T, sp pgx.Tx, id, to, kind, reason string) {
	t.Helper()
	from := jobStatus(t, sp, id)
	before := jobEvents(t, sp, id)
	ledgerExpectOK(t, sp, fmt.Sprintf("%s -> %s by %s", from, to, kind), jobMoveSQL, id, to, kind, jobActorFor(kind), reason)
	if got := jobStatus(t, sp, id); got != to {
		t.Fatalf("after %s -> %s by %s the job is %s", from, to, kind, got)
	}
	if after := jobEvents(t, sp, id); after != before+1 {
		t.Fatalf("%s -> %s by %s wrote %d event(s), want exactly 1", from, to, kind, after-before)
	}
	want := fmt.Sprintf("transition|%s>%s|%s|%s|%s", from, to, kind, pcActorText(kind), reason)
	if got := pcLastEvent(t, sp, id); got != want {
		t.Fatalf("the event of %s -> %s by %s is %q, want %q", from, to, kind, got, want)
	}
}

// pcCols reads the lifecycle columns a move sets, as one line: whether paused_from, cancel_requested_at, finished_at are set.
func pcCols(t *testing.T, db ledgerDB, id string) string {
	t.Helper()
	return ledgerString(t, db, `SELECT 'paused_from=' || COALESCE(paused_from, '-') || ' cancel_requested=' || (cancel_requested_at IS NOT NULL)::text || ' finished=' || (finished_at IS NOT NULL)::text FROM platform.jobs WHERE id = $1`, id)
}

// ---------------------------------------------------------------------------------------------------------------------
// the seeds and the guard that reads them
// ---------------------------------------------------------------------------------------------------------------------

// TestJobPauseCancel_TheSeedsAreWhatTheModulesCanDoAndTheGuardReadsThem: a migrated database says module 1 can cancel one job but cannot pause one, and modules 2 and 3 can do both (platform_0016; platform_0011 had
// seeded m1|false|false, m2|false|true, m3|false|true). For each module and each of the three moves that depend on a capability (running -> paused on can_pause; running -> cancelling on can_cancel; paused ->
// cancelling of a job that paused from running, which platform_0016 put under can_cancel too) the move is tried with the capability true and with it false, in a scratch savepoint where the row is written: true is
// accepted and false is refused 23001 with job_module_cannot_pause or job_module_cannot_cancel, for every module (18 cells), and at the SEED the answer is the seed's (9 cells), so the data is what decides and the
// seed is what the database says.
// It fails if a seed is not what the module can do, if the guard ignores a flag, or if a flag decides a move it is not about.
func TestJobPauseCancel_TheSeedsAreWhatTheModulesCanDoAndTheGuardReadsThem(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("a1")
	a := ledgerUserA

	if got := strings.Join(ledgerRows(t, tx, "SELECT id, can_pause::text, can_cancel::text FROM platform.modules ORDER BY id"), ","); got != "m1|false|true,m2|true|true,m3|true|true" {
		t.Fatalf("the capabilities of the modules are %s, want m1|false|true,m2|true|true,m3|true|true (module 1 cancels one job and has no per-job pause; modules 2 and 3 do both)", got)
	}
	seedPause := map[string]bool{"m1": false, "m2": true, "m3": true}
	seedCancel := map[string]bool{"m1": true, "m2": true, "m3": true}

	cells := 0
	// the three moves, each as a function of the capability it is about: how to make the job, which flag, which refusal
	type move struct {
		name, flag, refusal string
		build               func(sp pgx.Tx, id, module string) // the job, in the status the move starts from
		to                  string
		seed                map[string]bool
	}
	moves := []move{
		{"running -> paused", "can_pause", "job_module_cannot_pause", func(sp pgx.Tx, id, m string) { jobInsertAt(t, sp, id, a, m, "running") }, "paused", seedPause},
		{"running -> cancelling", "can_cancel", "job_module_cannot_cancel", func(sp pgx.Tx, id, m string) { jobInsertAt(t, sp, id, a, m, "running") }, "cancelling", seedCancel},
		{"paused (from running) -> cancelling", "can_cancel", "job_module_cannot_cancel", func(sp pgx.Tx, id, m string) {
			// the job is made with the pause capability on whatever the seed says (module 1 has none), so the capability under test is the only difference between the trials
			ledgerMust(t, sp, "UPDATE platform.modules SET can_pause = true WHERE id = $1", m)
			jobInsertAt(t, sp, id, a, m, "running")
			jobHop(t, sp, id, "paused")
		}, "cancelling", seedCancel},
	}
	for _, mv := range moves {
		for _, m := range pcModules {
			// at the seed: the answer is what the seeded flag says
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				mv.build(sp, id, m)
				o := jobTry(t, sp, jobMoveSQL, id, mv.to, "user", a, "why")
				cells++
				if mv.seed[m] != o.ok() || (!o.ok() && (o.State != "23001" || o.Constraint != mv.refusal)) {
					t.Fatalf("%s for module %s at its seed (%s = %v): %s, want %s", mv.name, m, mv.flag, mv.seed[m], o, map[bool]string{true: "accepted", false: "refused 23001 " + mv.refusal}[mv.seed[m]])
				}
			})
			// the flag written both ways in a scratch savepoint: the move follows it
			for _, v := range []bool{true, false} {
				v := v
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					id := next()
					mv.build(sp, id, m)
					ledgerMust(t, sp, fmt.Sprintf("UPDATE platform.modules SET %s = $2 WHERE id = $1", mv.flag), m, v)
					o := jobTry(t, sp, jobMoveSQL, id, mv.to, "user", a, "why")
					cells++
					if v != o.ok() || (!o.ok() && (o.State != "23001" || o.Constraint != mv.refusal)) {
						t.Fatalf("%s for module %s with %s = %v: %s, want %s", mv.name, m, mv.flag, v, o, map[bool]string{true: "accepted", false: "refused 23001 " + mv.refusal}[v])
					}
				})
			}
		}
	}
	if want := len(moves) * len(pcModules) * 3; cells != want || cells != 27 {
		t.Fatalf("the test tried %d cells, want %d (3 moves x 3 modules x (the seed, true, false)): the instrument matched too little", cells, 27)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// what a paused job can do next
// ---------------------------------------------------------------------------------------------------------------------

// pcAnswer is what a cell of the table below expects: pcOK, pcActor (the pair is a row but this actor may not take it) or the constraint that refuses it.
const (
	pcOK    = "ok"
	pcActor = "job_transition_actor_not_allowed"
	pcResum = "job_resume_to_where_it_paused_from"
	pcNoCan = "job_unsubmitted_pause_never_cancelling"
	pcNoCom = "job_unsubmitted_pause_never_completing"
	pcSubCn = "job_submitted_pause_cancels_through_the_engine"
	pcIlleg = "job_transition_illegal"
)

// pcPausedTable is the HAND-WRITTEN answer of the guard for a paused job, by where it paused from, the status it moves to and the actor (user, admin, engine). It is not derived from jobLegal: it is the design of
// platform_0016 written out for the one status whose moves depend on how the job got there.
var pcPausedTable = map[string][]struct{ to, user, admin, engine string }{
	// a job paused BEFORE its start submitted nothing: it resumes to scheduled, is cancelled directly, expires if nobody resumes it, and can never go through the engine
	"scheduled": {
		{"scheduled", pcOK, pcOK, pcActor},
		{"running", pcResum, pcResum, pcActor},
		{"cancelling", pcNoCan, pcNoCan, pcNoCan}, // platform_0018: the engine may now take the pair (the pause limit), and the wall of platform_0016 refuses it for a job that never started
		{"completing", pcActor, pcActor, pcNoCom},
		{"cancelled", pcOK, pcOK, pcOK}, // platform_0018: the engine cancels a job paused before its start once the pause outlasted the limit
		{"failed", pcActor, pcOK, pcOK},
		{"expired", pcActor, pcActor, pcOK},
	},
	// a job paused WHILE RUNNING is held by its module: it resumes to running, is cancelled only through cancelling, completes when the module reports everything final, and never expires
	"running": {
		{"scheduled", pcResum, pcResum, pcActor},
		{"running", pcOK, pcOK, pcActor},
		{"cancelling", pcOK, pcOK, pcOK}, // platform_0018: the engine takes a job it holds through cancelling once the pause outlasted the limit
		{"completing", pcActor, pcActor, pcOK},
		{"cancelled", pcSubCn, pcSubCn, pcSubCn}, // platform_0018: the engine may take the pair, and the wall of platform_0016 still refuses it for a job its module holds
		{"failed", pcActor, pcOK, pcOK},
		{"expired", pcActor, pcActor, pcIlleg},
	},
}

// TestJobPauseCancel_APausedJobTakesOnlyTheMovesItsPauseAllows: for each of modules 2 and 3 (the two that can pause), each place a job can have paused from (before its start; while running), each of the seven
// statuses a paused job has a row for, and each kind of actor (84 cells), the move is tried in a savepoint that is rolled back and the answer is the hand-written table's: accepted (16 cells for each module...
// 32 in all), or refused 23001 for ONE named reason, the order of the guard being the pair, then the actor, then the pause walls. The walls of platform_0016 each have their legal twin in the same table: a job paused
// from scheduled is cancelled directly (job_unsubmitted_pause_never_cancelling and job_unsubmitted_pause_never_completing refuse the two moves through the engine) and a job paused from running is taken to
// cancelling and to completing, never straight to cancelled (job_submitted_pause_cancels_through_the_engine); both resume only where they paused from; only the one that was running never expires.
// It fails if a wall is missing or fires for the wrong job, if a pair is refused that should pass, or if a refusal comes from another rule.
func TestJobPauseCancel_APausedJobTakesOnlyTheMovesItsPauseAllows(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("a2")
	a := ledgerUserA
	past := jobOpts{Deadline: jobDeadlinePast} // the deadline of the confirm has passed: the database only forbids EXPIRING before it, so the expiry cells are decided by the pause and not by the clock

	cells, accepted := 0, 0
	var problems []string
	for _, m := range []string{"m2", "m3"} {
		for _, pausedFrom := range []string{"scheduled", "running"} {
			for _, row := range pcPausedTable[pausedFrom] {
				for i, kind := range []string{"user", "admin", "engine"} {
					want := []string{row.user, row.admin, row.engine}[i]
					var o jobOutcome
					ledgerTrial(t, tx, func(sp pgx.Tx) {
						id := next()
						if pausedFrom == "scheduled" {
							jobInsertAtWith(t, sp, id, a, m, "paused", past)
						} else {
							jobInsertAtWith(t, sp, id, a, m, "running", past)
							jobPauseWhileRunningWith(t, sp, id, past)
						}
						if got := ledgerString(t, sp, "SELECT paused_from FROM platform.jobs WHERE id = $1", id); got != pausedFrom {
							t.Fatalf("the fixture for %s paused from %s is paused from %s", m, pausedFrom, got)
						}
						o = jobTry(t, sp, jobMoveSQL, id, row.to, kind, jobActorFor(kind), "why")
					})
					cells++
					where := fmt.Sprintf("%s paused from %s -> %s by %s", m, pausedFrom, row.to, kind)
					switch {
					case want == pcOK && o.ok():
						accepted++
					case want == pcOK:
						problems = append(problems, fmt.Sprintf("%s: a legal move was %s (%s)", where, o, o.Message))
					case o.State != "23001" || o.Constraint != want:
						problems = append(problems, fmt.Sprintf("%s: %s, want refused 23001 %q", where, o, want))
					}
				}
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("%d cell(s) of the table are not what the guard says:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	if cells != 84 || accepted != 32 {
		t.Fatalf("the table tried %d cells and %d were accepted, want 84 and 32 (2 modules x 2 places x 7 statuses x 3 actors; 8 accepted for each place and module since platform_0018 gave the engine the two moves of the pause limit)", cells, accepted)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// how a job paused while running ends
// ---------------------------------------------------------------------------------------------------------------------

// TestJobPauseCancel_AJobPausedWhileRunningEndsThroughTheEngineAndNeverStraightToCancelled: the lives of a job whose module holds its items, each move written as one event with who, from and to and why:
//   - the user (and, in a second life, an admin) pauses it, then cancels it: paused -> cancelling clears paused_from and sets cancel_requested_at; the engine takes it to completing and then to cancelled;
//     completing -> succeeded is refused (job_cancelled_cannot_succeed) beside the legal cancelled, and finished_at is set only by the last move;
//   - the module reports everything final while it is held: paused -> completing by the engine (no cancel was requested), then completing -> cancelled is refused (job_cancelled_needs_a_cancel) beside the legal
//     succeeded;
//   - the user resumes it: paused -> running clears paused_from, and it can be paused again;
//   - the direct move paused -> cancelled is refused for the user AND for the admin (job_submitted_pause_cancels_through_the_engine), beside the same move of a job that paused from SCHEDULED, which is accepted.
//
// It fails if the engine path is not open, if a cancel can end a job without it, or if a move writes no event or the wrong one.
func TestJobPauseCancel_AJobPausedWhileRunningEndsThroughTheEngineAndNeverStraightToCancelled(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("a3")
	a := ledgerUserA
	why := func(constraint string) ledgerWhy { return ledgerWhy{Constraint: constraint} }

	for _, c := range []struct{ name, module, canceller string }{
		{"the user cancels a module 2 job", "m2", "user"}, {"an admin cancels a module 3 job", "m3", "admin"},
	} {
		c := c
		t.Run("the cancel goes through the engine: "+c.name, func(t *testing.T) {
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, c.module, "running")
				pcMove(t, sp, id, "paused", "user", "the user paused it")
				if got := pcCols(t, sp, id); got != "paused_from=running cancel_requested=false finished=false" {
					t.Fatalf("a job paused while running: %s", got)
				}
				// the direct end is refused for everyone who may take it, beside the legal move of the same job
				for _, kind := range []string{"user", "admin"} {
					ledgerExpectRefused(t, sp, "paused -> cancelled of a job paused while running by "+kind, "23001", why("job_submitted_pause_cancels_through_the_engine"), jobMoveSQL, id, "cancelled", kind, jobActorFor(kind), "why")
				}
				pcMove(t, sp, id, "cancelling", c.canceller, "the cancel is told to the module")
				if got := pcCols(t, sp, id); got != "paused_from=- cancel_requested=true finished=false" {
					t.Fatalf("a job cancelled while paused: %s, want paused_from cleared and the cancel recorded", got)
				}
				pcMove(t, sp, id, "completing", "engine", "the module reports every item final")
				ledgerExpectRefused(t, sp, "completing -> succeeded after the cancel", "23001", why("job_cancelled_cannot_succeed"), jobMoveSQL, id, "succeeded", "engine", nil, "why")
				pcMove(t, sp, id, "cancelled", "engine", "every result is recorded after the cancel")
				if got := pcCols(t, sp, id); got != "paused_from=- cancel_requested=true finished=true" {
					t.Fatalf("a cancelled job: %s", got)
				}
			})
		})
	}

	t.Run("a held job whose module finished everything completes the normal way", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "running")
			pcMove(t, sp, id, "paused", "user", "the user paused it")
			pcMove(t, sp, id, "completing", "engine", "the module reports every item it holds final")
			if got := pcCols(t, sp, id); got != "paused_from=- cancel_requested=false finished=false" {
				t.Fatalf("a completing job that was paused: %s", got)
			}
			ledgerExpectRefused(t, sp, "completing -> cancelled with no cancel requested", "23001", why("job_cancelled_needs_a_cancel"), jobMoveSQL, id, "cancelled", "engine", nil, "why")
			pcMove(t, sp, id, "succeeded", "engine", "every result is recorded")
			if got := pcCols(t, sp, id); got != "paused_from=- cancel_requested=false finished=true" {
				t.Fatalf("a succeeded job that was paused: %s", got)
			}
		})
	})

	t.Run("a resume goes back to running and clears where it paused from; the job can be paused again", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m3", "running")
			pcMove(t, sp, id, "paused", "user", "the user paused it")
			pcMove(t, sp, id, "running", "user", "the user resumed it")
			if got := pcCols(t, sp, id); got != "paused_from=- cancel_requested=false finished=false" {
				t.Fatalf("a resumed job: %s", got)
			}
			pcMove(t, sp, id, "paused", "admin", "an admin paused it again")
			if got := pcCols(t, sp, id); got != "paused_from=running cancel_requested=false finished=false" {
				t.Fatalf("a job paused again: %s", got)
			}
			pcMove(t, sp, id, "failed", "engine", "ended by the engine")
		})
	})

	t.Run("control: a job that paused from SCHEDULED is cancelled directly, which is the very move refused above", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			if got := pcCols(t, sp, id); got != "paused_from=scheduled cancel_requested=false finished=false" {
				t.Fatalf("a job paused before its start: %s", got)
			}
			pcMove(t, sp, id, "cancelled", "user", "cancelled before it started")
		})
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the slot, the staleness clock and the application role
// ---------------------------------------------------------------------------------------------------------------------

// TestJobPauseCancel_APausedJobHoldsTheSlotUntilItEnds: a job paused while running holds its user's slot on its module through paused, cancelling and completing (a second job of the same user and module,
// previewed, is refused 23505 jobs_one_active_per_user_module at the door of confirmed) and releases it only when it ends (the same probe is accepted after cancelled); the same user's slot on ANOTHER module is not held
// at any of those steps (the control that the refusal is the slot's, not the user's); and nothing about the pause expires the job (a job paused while running that is long past its start deadline is still paused).
// It fails if a pause frees the slot, if the engine path frees it too early, or if the refusal is for another reason.
func TestJobPauseCancel_APausedJobHoldsTheSlotUntilItEnds(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("a4")
	a := ledgerUserA

	// probe is a second job of user A on the module, previewed and then moved to confirmed in a savepoint that is rolled back: true when the slot refused it
	probe := func(sp pgx.Tx, module string) bool {
		held := false
		ledgerTrial(t, sp, func(sp2 pgx.Tx) {
			id := next()
			jobInsertAt(t, sp2, id, a, module, "previewed")
			sql, args := jobMoveStmt("previewed", "confirmed", id, "user", a, "probe", jobOpts{})
			switch o := jobTry(t, sp2, sql, args...); {
			case o.ok():
			case o.State == "23505" && o.Constraint == "jobs_one_active_per_user_module":
				held = true
			default:
				t.Fatalf("the probe on %s was refused for another reason: %s (%s)", module, o, o.Message)
			}
		})
		return held
	}
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		first := next()
		jobInsertAtWith(t, sp, first, a, "m2", "running", jobOpts{Deadline: jobDeadlinePast}) // its start deadline is long past: the pause is not what keeps it alive, the rule that a running job never expires is
		if !probe(sp, "m2") {
			t.Fatal("a running job does not hold its slot")
		}
		pcMove(t, sp, first, "paused", "user", "the user paused it")
		steps := []struct{ to, kind string }{{"", ""}, {"cancelling", "user"}, {"completing", "engine"}, {"cancelled", "engine"}}
		for _, s := range steps {
			if s.to != "" {
				pcMove(t, sp, first, s.to, s.kind, "next step")
			}
			status := jobStatus(t, sp, first)
			wantHeld := status != "cancelled"
			if got := probe(sp, "m2"); got != wantHeld {
				t.Fatalf("while the first job is %s the slot of module 2 is held = %v, want %v", status, got, wantHeld)
			}
			if probe(sp, "m3") { // the same user's other module is another slot: never held by this job
				t.Fatalf("while the first job is %s the slot of module 3 is held: the refusal is the user's, not the slot's", status)
			}
		}
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		id := next()
		jobInsertAtWith(t, sp, id, a, "m2", "running", jobOpts{Deadline: jobDeadlinePast})
		jobPauseWhileRunningWith(t, sp, id, jobOpts{Deadline: jobDeadlinePast})
		ledgerExpectRefused(t, sp, "a job paused while running, past its start deadline, expired", "23001", ledgerWhy{Constraint: "job_transition_illegal"}, jobMoveSQL, id, "expired", "engine", nil, "why")
		if got := jobStatus(t, sp, id); got != "paused" {
			t.Fatalf("the job is %s, still paused was expected", got)
		}
	})
}

// TestJobPauseCancel_AResumeStartsTheStalenessClockAgain: the guard sets last_progress_at to the database's now() for every move into running, so a job that sat paused for hours is not stale the moment it resumes.
// The job's last_progress_at is made a day old (an annotation: no move), it is paused (the value is unchanged: the pause is not progress) and cancelled through the engine (still unchanged), and, in another trial,
// resumed (now() exactly: the transaction's clock). The control is the pair of unchanged values; the test fails if the resume leaves the old value or if a move that is not into running changes it.
func TestJobPauseCancel_AResumeStartsTheStalenessClockAgain(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("a5")
	a := ledgerUserA
	old := func(sp pgx.Tx, id string) bool {
		return ledgerBool(t, sp, "SELECT last_progress_at IS NOT NULL AND last_progress_at < now() - interval '1 hour' FROM platform.jobs WHERE id = $1", id)
	}
	fresh := func(sp pgx.Tx, id string) bool {
		return ledgerBool(t, sp, "SELECT last_progress_at = now() FROM platform.jobs WHERE id = $1", id)
	}
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		id := next()
		jobInsertAt(t, sp, id, a, "m2", "running")
		if !fresh(sp, id) {
			t.Fatal("the move into running did not set last_progress_at to now(): the fixture of this test is wrong")
		}
		ledgerMust(t, sp, "UPDATE platform.jobs SET last_progress_at = now() - interval '1 day' WHERE id = $1", id)
		if !old(sp, id) {
			t.Fatal("the fixture could not age last_progress_at")
		}
		pcMove(t, sp, id, "paused", "user", "the user paused it")
		if !old(sp, id) {
			t.Fatal("pausing a job moved last_progress_at: a pause is not progress")
		}
		ledgerTrial(t, sp, func(sp2 pgx.Tx) { // the control: the way through the engine does not touch it either
			pcMove(t, sp2, id, "cancelling", "user", "cancelled while paused")
			if !old(sp2, id) {
				t.Fatal("a move into cancelling moved last_progress_at")
			}
		})
		pcMove(t, sp, id, "running", "user", "the user resumed it")
		if !fresh(sp, id) {
			t.Fatal("a resumed job kept its old last_progress_at: it would be stale the moment it resumed")
		}
	})
}

// TestJobPauseCancel_TheApplicationRoleTakesTheNewMovesAndCannotAddOne: the role the service runs as (platform_app) takes paused -> cancelling as the user and paused -> completing as the engine through
// platform.job_transition (each writes its event, which its INSERT privilege on job_events allows), the three walls bind it as they bind everyone (a job paused from scheduled is refused the same two moves, a job
// paused from running refused the direct cancel), and it cannot add a pair to the table of legal moves (42501): a new move is a migration, never the service's.
// It fails if the service role cannot walk the new pairs, if a wall does not bind it, or if it can write the table.
func TestJobPauseCancel_TheApplicationRoleTakesTheNewMovesAndCannotAddOne(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA
	running := ledgerJobA2 // user A, module 2, a draft
	jobPauseWhileRunning(t, tx, running)
	why := func(constraint string) ledgerWhy { return ledgerWhy{Constraint: constraint} }

	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerSwitchRole(t, sp, "platform_app")
		before := jobEvents(t, sp, running)
		ledgerExpectRefused(t, sp, "platform_app takes paused -> cancelled of a job paused while running", "23001", why("job_submitted_pause_cancels_through_the_engine"), jobMoveSQL, running, "cancelled", "user", a, "why")
		ledgerExpectOK(t, sp, "platform_app takes paused -> cancelling as the user", jobMoveSQL, running, "cancelling", "user", a, "cancel requested")
		ledgerExpectOK(t, sp, "platform_app takes cancelling -> completing as the engine", jobMoveSQL, running, "completing", "engine", nil, "every item final")
		ledgerExpectOK(t, sp, "platform_app takes completing -> cancelled as the engine", jobMoveSQL, running, "cancelled", "engine", nil, "recorded")
		if n := jobEvents(t, sp, running); n != before+3 {
			t.Fatalf("three moves of platform_app wrote %d event(s), want 3", n-before)
		}
		ledgerResetRole(t, sp)
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerSwitchRole(t, sp, "platform_app")
		ledgerExpectOK(t, sp, "platform_app takes paused -> completing as the engine", jobMoveSQL, running, "completing", "engine", nil, "every item final")
		ledgerExpectOK(t, sp, "platform_app takes completing -> succeeded as the engine", jobMoveSQL, running, "succeeded", "engine", nil, "recorded")
		ledgerResetRole(t, sp)
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerResetRole(t, sp)
		// the job of the fixture is the module 2 job of user A: a job paused from scheduled needs another module's slot, so module 3 is used
		id := ledgerUUID("a6", 1)
		jobInsertAt(t, sp, id, a, "m3", "paused")
		ledgerSwitchRole(t, sp, "platform_app")
		ledgerExpectRefused(t, sp, "platform_app takes paused -> cancelling of a job paused from scheduled", "23001", why("job_unsubmitted_pause_never_cancelling"), jobMoveSQL, id, "cancelling", "user", a, "why")
		ledgerExpectRefused(t, sp, "platform_app takes paused -> completing of a job paused from scheduled", "23001", why("job_unsubmitted_pause_never_completing"), jobMoveSQL, id, "completing", "engine", nil, "why")
		ledgerExpectOK(t, sp, "control: platform_app cancels the same job directly", jobMoveSQL, id, "cancelled", "user", a, "cancelled before it started")
		ledgerResetRole(t, sp)
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerSwitchRole(t, sp, "platform_app")
		ledgerExpectRefused(t, sp, "platform_app adds a legal move of its own", "42501", ledgerWhy{Message: "permission denied for table job_transitions"},
			"INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES ('paused', 'succeeded', ARRAY['engine'], 'a control')")
		ledgerResetRole(t, sp)
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the migration's own control and its down
// ---------------------------------------------------------------------------------------------------------------------

// pauseCancelObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0016's down, in the lifecycle test of the runner (TestRunnerLifecycle): before it, modules 2 and 3 can pause and module 1 can
// cancel, the two moves paused -> cancelling and paused -> completing are rows of platform.job_transitions (31 rows in all), the guard's text names the three walls, and the rules BITE (a job paused while running
// is cancelled through cancelling, completing and cancelled, completes the normal way, and is refused the direct cancel; a job paused from scheduled is refused both moves through the engine); after it the
// capabilities are platform_0011's seeds again (m1|false|false, m2|false|true, m3|false|true), the two rows are gone (29 in all), the guard's text names none of the walls, a running job cannot be paused, and a
// paused job is refused both moves as job_transition_illegal. The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func pauseCancelObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	wantCaps, wantRows := "m1|false|false,m2|false|true,m3|false|true", int64(29)
	if present {
		wantCaps, wantRows = "m1|false|true,m2|true|true,m3|true|true", 31
	}
	if got := strings.Join(ledgerRows(t, c, "SELECT id, can_pause::text, can_cancel::text FROM platform.modules ORDER BY id"), ","); got != wantCaps {
		t.Fatalf("%s: the capabilities of the modules are %s, want %s", when, got, wantCaps)
	}
	if got := ledgerInt64(t, c, "SELECT count(*) FROM platform.job_transitions"); got != wantRows {
		t.Fatalf("%s: platform.job_transitions holds %d rows, want %d", when, got, wantRows)
	}
	expect("the two moves paused -> cancelling and paused -> completing", 2,
		"SELECT count(*) FROM platform.job_transitions WHERE from_status = 'paused' AND to_status IN ('cancelling', 'completing')")
	expect("the three walls of platform_0016 named in the text of platform.jobs_transition_guard", 3,
		`SELECT count(*) FROM unnest(ARRAY['job_unsubmitted_pause_never_cancelling', 'job_unsubmitted_pause_never_completing', 'job_submitted_pause_cancels_through_the_engine']) AS w(name)
		 WHERE EXISTS (SELECT 1 FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_transition_guard' AND prosrc LIKE '%' || w.name || '%')`)

	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	job, a := ledgerJobA2, ledgerUserA // module 2, user A, a draft
	why := func(constraint string) ledgerWhy { return ledgerWhy{Constraint: constraint} }
	if present {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			jobPauseWhileRunning(t, sp, job)
			ledgerExpectRefused(t, sp, when+": a job paused while running, cancelled directly", "23001", why("job_submitted_pause_cancels_through_the_engine"), jobMoveSQL, job, "cancelled", "user", a, "why")
			ledgerTrial(t, sp, func(sp2 pgx.Tx) {
				for _, m := range []struct{ to, kind string }{{"cancelling", "user"}, {"completing", "engine"}, {"cancelled", "engine"}} {
					ledgerExpectOK(t, sp2, when+": a job paused while running, "+m.to, jobMoveSQL, job, m.to, m.kind, jobActorFor(m.kind), "why")
				}
			})
			for _, m := range []struct{ to, kind string }{{"completing", "engine"}, {"succeeded", "engine"}} {
				ledgerExpectOK(t, sp, when+": a job paused while running completes the normal way, "+m.to, jobMoveSQL, job, m.to, m.kind, jobActorFor(m.kind), "why")
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			jobWalkTo(t, sp, job, "paused") // paused from scheduled
			ledgerExpectRefused(t, sp, when+": a job paused from scheduled, cancelling", "23001", why("job_unsubmitted_pause_never_cancelling"), jobMoveSQL, job, "cancelling", "user", a, "why")
			ledgerExpectRefused(t, sp, when+": a job paused from scheduled, completing", "23001", why("job_unsubmitted_pause_never_completing"), jobMoveSQL, job, "completing", "engine", nil, "why")
			ledgerExpectOK(t, sp, when+": control: a job paused from scheduled, cancelled directly", jobMoveSQL, job, "cancelled", "user", a, "why")
		})
		return
	}
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		jobWalkTo(t, sp, job, "running")
		ledgerExpectRefused(t, sp, when+": a running job paused (no module can pause one under platform_0011's seeds)", "23001", why("job_module_cannot_pause"), jobMoveSQL, job, "paused", "user", a, "why")
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		jobWalkTo(t, sp, job, "paused")
		ledgerExpectRefused(t, sp, when+": a paused job, cancelling", "23001", why("job_transition_illegal"), jobMoveSQL, job, "cancelling", "user", a, "why")
		ledgerExpectRefused(t, sp, when+": a paused job, completing", "23001", why("job_transition_illegal"), jobMoveSQL, job, "completing", "engine", nil, "why")
		ledgerExpectOK(t, sp, when+": control: a paused job, cancelled directly", jobMoveSQL, job, "cancelled", "user", a, "why")
	})
}

// TestJobPauseCancel_Down0016RefusesAJobTheOlderLifecycleCannotEnd: platform_0016's down says no (SQLSTATE 55000, naming how many) while a job is paused while it was running, or is a module 1 job whose cancel
// was accepted (cancelling or completing), and leaves the database exactly as it was (still at 0016: 31 moves, the new capabilities). The older lifecycle has no pair that lets the engine end the first and cannot
// cancel the second, and ending such a job is a decision about money and a customer's work. Each refusal has its control in the same down, which passes once every such job has been ended, while a module 1 job
// that is running and a job paused before its start are still there (the older lifecycle ends them as it always did): the capabilities are platform_0011's again, the two moves are gone, the jobs are untouched,
// and an up again restores the migration.
// It fails if the guard of the down is missing, if it counts wrongly, if it refuses a job the older lifecycle can end, or if a refused down changes anything.
func TestJobPauseCancel_Down0016RefusesAJobTheOlderLifecycleCannotEnd(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	env := migEnv(s.DSN, dir, nil)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1
	for i, v := range chain {
		if v == "0016" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0016 after other migrations", chain)
	}
	seed, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSeed(t, seed)
	if err := seed.Commit(ctx); err != nil { // the migrator runs on its own connection: the jobs must be committed
		t.Fatal(err)
	}
	migRollBackAbove(t, admin, envDown, chain, at)

	stranded := func() int64 {
		return ledgerInt64(t, admin, `SELECT count(*) FROM platform.jobs WHERE (status = 'paused' AND paused_from IS NOT DISTINCT FROM 'running')
			OR (status IN ('cancelling', 'completing') AND module_id = 'm1' AND cancel_requested_at IS NOT NULL)`)
	}
	unchanged := func(what string) {
		t.Helper()
		if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
			t.Fatalf("a refused down 0016 (%s) changed the database: applied = %v, want %v", what, got, chain[:at+1])
		}
		if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.job_transitions"); n != 31 {
			t.Fatalf("a refused down 0016 (%s) left %d rows in platform.job_transitions, want its 31", what, n)
		}
		if got := strings.Join(ledgerRows(t, admin, "SELECT id, can_pause::text, can_cancel::text FROM platform.modules ORDER BY id"), ","); got != "m1|false|true,m2|true|true,m3|true|true" {
			t.Fatalf("a refused down 0016 (%s) left the capabilities as %s", what, got)
		}
	}
	refused := func(what string) {
		t.Helper()
		if got := stranded(); got != 1 {
			t.Fatalf("the instrument: %d stranded jobs before the down with %s, want exactly 1", got, what)
		}
		res := runMigrator(t, envDown, "down", "0016")
		mustExit(t, res, 1, "down 0016 with "+what)
		mustContain(t, res.Stderr, "platform_0016 down: 1 job(s) are paused while running, or are module 1 jobs whose cancel was accepted", "down 0016 with "+what)
		mustContain(t, res.Stderr, "55000", "down 0016 with "+what)
		unchanged(what)
	}

	// jobs the older lifecycle ends as it always did: they must not make the down refuse (a module 1 job that is running, a job paused before its start)
	jobWalkTo(t, admin, ledgerJobA0, "running") // user A, module 1
	jobWalkTo(t, admin, ledgerJobB1, "paused")  // user B, module 1: paused from scheduled
	if got := stranded(); got != 0 {
		t.Fatalf("the instrument: %d stranded jobs with a running module 1 job and a job paused before its start, want 0 (the down must not refuse them)", got)
	}

	// 1. a job paused while it was running (module 2)
	jobPauseWhileRunning(t, admin, ledgerJobA2)
	refused("a job paused while it was running")
	jobEnd(t, admin, ledgerJobA2) // the control's setup: failed, a status the older lifecycle has

	// 2. a module 1 job whose cancel was accepted: cancelling (the module 1 job of user A is already running: the cancel is taken through the sanctioned move)
	ledgerMust(t, admin, jobMoveSQL, ledgerJobA0, "cancelling", "user", ledgerUserA, "the user cancelled it")
	refused("a module 1 job whose cancel was accepted (cancelling)")
	ledgerMust(t, admin, jobMoveSQL, ledgerJobA0, "completing", "engine", nil, "every row final")
	refused("a module 1 job whose cancel was accepted (completing)")
	jobEnd(t, admin, ledgerJobA0)

	// the control: nothing is stranded, so the same down passes; the capabilities and the table are platform_0011's again and the jobs it left are untouched
	if got := stranded(); got != 0 {
		t.Fatalf("the instrument: %d stranded jobs before the down that must pass", got)
	}
	mustExit(t, runMigrator(t, envDown, "down", "0016"), 0, "down 0016 once no job is stranded")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0016 that passed: applied = %v, want %v", got, chain[:at])
	}
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.job_transitions"); n != 29 {
		t.Fatalf("after the down 0016 that passed platform.job_transitions holds %d rows, want 29", n)
	}
	if got := strings.Join(ledgerRows(t, admin, "SELECT id, can_pause::text, can_cancel::text FROM platform.modules ORDER BY id"), ","); got != "m1|false|false,m2|false|true,m3|false|true" {
		t.Fatalf("after the down 0016 that passed the capabilities are %s, want platform_0011's seeds", got)
	}
	for id, want := range map[string]string{ledgerJobB1: "paused", ledgerJobA2: "failed", ledgerJobA0: "failed"} {
		if got := jobStatus(t, admin, id); got != want {
			t.Fatalf("after the down 0016 that passed job %s is %s, want %s: a down never edits a job", id, got, want)
		}
	}

	// up again restores the migration over the jobs that were left behind
	mustExit(t, runMigrator(t, env, "up"), 0, "up after the down 0016")
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("after up again: applied = %v, want %v", got, chain)
	}
	unchanged("after up again")
}
