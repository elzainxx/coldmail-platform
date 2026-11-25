//go:build harness

package harness

import (
	"fmt"
	"strings"
	"testing"
)

// Fixtures for every test that needs a platform.jobs row past draft. Since platform_0011 a job is BORN draft and changes status only along platform.job_transitions, with an actor and a reason: a test
// that wants a running job walks one there, the way the engine will.
//
// The paths below are HAND-WRITTEN on purpose and never read from platform.job_transitions: a lifecycle test that took its fixtures from the table it tests would agree with the table whatever it said.
// TestJobLifecycle_FixturePathsAreLegalMoves holds this file to the expected list of job_lifecycle_test.go (and to the actors that list allows) without a database, so a path that drifts from the design
// fails there, not as a confusing refusal in the middle of an unrelated test.

// jobChains is, for each target status, the chains of statuses a job passes through to reach it. A walk starts at the job's CURRENT status, takes the first chain of the target that contains it and
// goes on from there, so a job can be walked to a state from draft, or from wherever an earlier step left it. A chain needs no start at draft (a failed job can come from any active state) and each
// ends at the target. Statuses that are not reachable from the current one (succeeded from cancelling: a cancelled job never succeeds; cancelling for module 1: it has no cancel) have no chain and the
// walk fails, saying so.
var jobChains = map[string][][]string{
	"draft":     {{"previewed", "draft"}},
	"previewed": {{"draft", "previewed"}},
	"confirmed": {{"draft", "previewed", "confirmed"}},
	"scheduled": {{"draft", "previewed", "scheduled"}, {"paused", "scheduled"}},
	"running": {
		{"draft", "previewed", "confirmed", "running"},
		{"draft", "previewed", "scheduled", "running"},
		{"paused", "scheduled", "running"},
	},
	// no module can pause a RUNNING job (platform.modules.can_pause is false for all of them), so a paused job is a job whose start was deferred
	"paused": {{"draft", "previewed", "scheduled", "paused"}},
	"cancelling": {
		{"draft", "previewed", "confirmed", "running", "cancelling"},
		{"draft", "previewed", "scheduled", "running", "cancelling"},
		{"paused", "scheduled", "running", "cancelling"},
	},
	"completing": {
		{"draft", "previewed", "confirmed", "running", "completing"},
		{"draft", "previewed", "scheduled", "running", "completing"},
		{"paused", "scheduled", "running", "completing"},
		{"cancelling", "completing"},
	},
	"succeeded": {
		{"draft", "previewed", "confirmed", "running", "completing", "succeeded"},
		{"draft", "previewed", "scheduled", "running", "completing", "succeeded"},
		{"paused", "scheduled", "running", "completing", "succeeded"},
	},
	"failed": {
		{"draft", "previewed", "confirmed", "failed"},
		{"scheduled", "failed"}, {"paused", "failed"}, {"running", "failed"}, {"cancelling", "failed"}, {"completing", "failed"},
	},
	"cancelled": {
		{"draft", "cancelled"},
		{"draft", "previewed", "cancelled"},
		{"draft", "previewed", "confirmed", "cancelled"},
		{"draft", "previewed", "scheduled", "cancelled"},
		{"draft", "previewed", "scheduled", "paused", "cancelled"},
		{"draft", "previewed", "confirmed", "running", "cancelling", "completing", "cancelled"},
	},
	"expired": {
		{"draft", "previewed", "confirmed", "expired"},
		{"scheduled", "expired"},
		{"paused", "expired"},
	},
}

// jobUserHops are the moves only the job's owner (or an admin) takes; every other move of a fixture is the engine's.
var jobUserHops = map[string]bool{
	"draft>previewed": true, "draft>cancelled": true,
	"previewed>draft": true, "previewed>confirmed": true, "previewed>scheduled": true, "previewed>cancelled": true,
	"confirmed>cancelled": true,
	"scheduled>paused":    true, "scheduled>cancelled": true,
	"paused>scheduled": true, "paused>running": true, "paused>cancelled": true,
	"running>paused": true, "running>cancelling": true,
}

// jobMoveSQL is the one sanctioned statement of a move (the store's, and the fixtures'): the job, the status it goes to, the kind of actor, the actor's id (NULL for the engine) and the reason.
const jobMoveSQL = "SELECT platform.job_transition($1::uuid, $2::text, $3::text, $4::uuid, $5::text)"

// jobShapeKV is the name/value pairs of an INSERT that makes a job of the given status satisfy every CHECK of platform_0011 (a job past previewed has a preview, its fingerprint, a confirm key and a
// confirmed_at; confirmed and scheduled a start deadline; scheduled a start time; a confirmed job none; a paused job says where it paused from). It is for the tests that insert a job in a status with the
// lifecycle trigger switched off, to prove the CHECKs and the vocabulary behind it; every other test walks a draft job (jobWalkTo).
func jobShapeKV(status string) []any {
	kv := []any{"status", status}
	switch status {
	case "confirmed", "scheduled", "running", "paused", "cancelling", "completing":
		kv = append(kv, "preview", `{}`, "preview_fingerprint", strings.Repeat("a", 64), "confirm_key", "k-"+status, "confirmed_at", "2030-01-01T00:00:00Z")
	}
	switch status {
	case "confirmed", "scheduled":
		kv = append(kv, "start_deadline", "2031-01-01T00:00:00Z")
	}
	if status == "scheduled" {
		kv = append(kv, "scheduled_start", "2030-06-01T00:00:00Z")
	}
	if status == "paused" {
		kv = append(kv, "paused_from", "scheduled")
	}
	return kv
}

// jobStatus is the job's status now.
func jobStatus(t *testing.T, db ledgerDB, id string) string {
	t.Helper()
	return ledgerString(t, db, "SELECT status FROM platform.jobs WHERE id = $1", id)
}

// jobPath is the statuses a walk takes from `from` to `target`, not counting `from`: the rest of the first hand-written chain of the target that contains `from`.
func jobPath(from, target string) ([]string, bool) {
	for _, chain := range jobChains[target] {
		for i, s := range chain {
			if s == from {
				return chain[i+1:], true
			}
		}
	}
	return nil, false
}

// jobOpts are the choices of a walk that a test makes on purpose. Deadline is the SQL expression of the start_deadline the move into confirmed or scheduled carries (platform_0011 refuses a change of
// start_deadline by any write that leaves the status alone, so the deadline travels WITH that move): empty is a day ahead; a walk to expired is given a deadline that has passed, because the database
// only forbids EXPIRING before the deadline, not a deadline in the past.
type jobOpts struct{ Deadline string }

const (
	jobDeadlineAhead = "now() + interval '1 day'"
	jobDeadlinePast  = "now() - interval '1 second'"
)

func (o jobOpts) deadline() string {
	if o.Deadline == "" {
		return jobDeadlineAhead
	}
	return o.Deadline
}

// jobMoveStmt is the statement of a legal move of job id from `from` to `to` by an actor of this kind and id, with the reason. It is the sanctioned function (jobMoveSQL and its arguments) except for the one move
// that carries a column: previewed -> confirmed or scheduled is ONE UPDATE of the status and the start deadline, run under the three settings stated by hand (and cleared again after it, as the function
// clears them), because no write that leaves the status alone may set the deadline. That form has no arguments (they are in the text: ids and statuses of the tests, never user input); `kind` "" and a blank
// reason are "not stated", as they are to the function.
func jobMoveStmt(from, to, id, kind string, actor any, reason string, o jobOpts) (string, []any) {
	if from == "previewed" && (to == "confirmed" || to == "scheduled") {
		a := ""
		if actor != nil {
			a = fmt.Sprint(actor)
		}
		return jobAsOnce(kind, a, reason, fmt.Sprintf("UPDATE platform.jobs SET status = '%s', start_deadline = %s WHERE id = '%s'", to, o.deadline(), id)), nil
	}
	return jobMoveSQL, []any{id, to, kind, actor, reason}
}

// jobWalkTo moves the job from its CURRENT status to target along the hand-written chains, filling what the CHECKs of platform_0011 need on the way (a preview, its fingerprint and a confirm key before
// the job leaves draft; a start deadline carried by the move into confirmed or scheduled and a start time for scheduled) and making the clock pass where the database insists (a scheduled job is due before it
// starts; a walk to expired carries a deadline that has passed). Every hop is `platform.job_transition`, taken by the engine, or by the job's own user where only a user or an admin may. It fails the test,
// naming the hop and what the database said, when a hop is refused: a fixture that cannot be built is a failed test, never a skipped one.
func jobWalkTo(t *testing.T, db ledgerDB, id, target string) {
	t.Helper()
	jobWalkToWith(t, db, id, target, jobOpts{})
}

// jobWalkToWith is jobWalkTo with the choices of jobOpts.
func jobWalkToWith(t *testing.T, db ledgerDB, id, target string, o jobOpts) {
	t.Helper()
	from := jobStatus(t, db, id)
	if from == target {
		return
	}
	path, ok := jobPath(from, target)
	if !ok {
		t.Fatalf("fixture: there is no legal path from %s to %s for job %s", from, target, id)
	}
	if target == "expired" && o.Deadline == "" {
		o.Deadline = jobDeadlinePast
	}
	for _, to := range path {
		jobHopWith(t, db, id, to, o)
	}
}

// jobHop moves the job one status, as jobWalkTo does for each step of a path.
func jobHop(t *testing.T, db ledgerDB, id, to string) {
	t.Helper()
	jobHopWith(t, db, id, to, jobOpts{})
}

func jobHopWith(t *testing.T, db ledgerDB, id, to string, o jobOpts) {
	t.Helper()
	var from, owner string
	if err := db.QueryRow(testCtx(t), "SELECT status, user_id::text FROM platform.jobs WHERE id = $1", id).Scan(&from, &owner); err != nil {
		t.Fatalf("fixture: read job %s: %v", id, err)
	}
	jobPrepareHop(t, db, id, from, to)
	kind, actor := "engine", any(nil)
	if jobUserHops[from+">"+to] {
		kind, actor = "user", owner
	}
	sql, args := jobMoveStmt(from, to, id, kind, actor, "test fixture", o)
	if _, err := db.Exec(testCtx(t), sql, args...); err != nil {
		t.Fatalf("fixture: job %s could not go from %s to %s as %s: %v", id, from, to, kind, err)
	}
}

// jobPrepareHop writes what a LEGAL hop from `from` to `to` needs in the columns that may be written on their own: a preview, its fingerprint and a confirm key before the job leaves draft (a discarded draft needs
// none; all three are editable while the job is a draft), the start time of a job that goes to scheduled, and a start time that has passed for the job that starts (scheduled_start is not one of the columns
// that change only with a move). The columns that DO change only with a move (the start deadline above all) travel with it: jobMoveStmt. The matrix of TestJobLifecycle_Matrix calls it only for the pairs it
// expects to succeed.
func jobPrepareHop(t *testing.T, db ledgerDB, id, from, to string) {
	t.Helper()
	if from == "draft" && to != "cancelled" {
		ledgerMust(t, db, "UPDATE platform.jobs SET preview = '{}'::jsonb, preview_fingerprint = repeat('a', 64), confirm_key = 'fx-' || id::text WHERE id = $1", id)
	}
	if from == "previewed" && to == "scheduled" {
		ledgerMust(t, db, "UPDATE platform.jobs SET scheduled_start = now() + interval '1 hour' WHERE id = $1", id)
	}
	if from == "scheduled" && to == "running" {
		jobMakeDue(t, db, id)
	}
}

// jobMakeDue lets time pass for a scheduled (or a paused-from-scheduled) job: its start time is one second in the past, so the database no longer refuses to start it (job_started_early). The UPDATE
// leaves the status alone, which the guard allows for scheduled_start.
func jobMakeDue(t *testing.T, db ledgerDB, id string) {
	t.Helper()
	tag := ledgerMust(t, db, "UPDATE platform.jobs SET scheduled_start = now() - interval '1 second' WHERE id = $1 AND status IN ('scheduled', 'paused')", id)
	if tag.RowsAffected() != 1 {
		t.Fatalf("fixture: jobMakeDue touched %d rows for job %s, want exactly 1 scheduled or paused job", tag.RowsAffected(), id)
	}
}

// jobInsertAt inserts a draft job (the only way a job is born) and walks it to status.
func jobInsertAt(t *testing.T, db ledgerDB, id, user, module, status string) {
	t.Helper()
	jobInsertAtWith(t, db, id, user, module, status, jobOpts{})
}

// jobInsertAtWith is jobInsertAt with the choices of jobOpts (a job that is confirmed with a deadline that has passed, or exactly now).
func jobInsertAtWith(t *testing.T, db ledgerDB, id, user, module, status string, o jobOpts) {
	t.Helper()
	ledgerMust(t, db, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, $3)", id, user, module)
	jobWalkToWith(t, db, id, status, o)
}

// jobIsTerminal says whether a status is one of the four that end a job.
func jobIsTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "expired":
		return true
	}
	return false
}

// jobEnd takes a job out of the slot by the shortest legal way: a draft or previewed job is cancelled by its user, any other active job fails (the engine's or an admin's act), a terminal job stays.
func jobEnd(t *testing.T, db ledgerDB, id string) {
	t.Helper()
	switch s := jobStatus(t, db, id); {
	case jobIsTerminal(s):
	case s == "draft" || s == "previewed":
		jobWalkTo(t, db, id, "cancelled")
	default:
		jobWalkTo(t, db, id, "failed")
	}
}

// jobAs wraps one UPDATE of platform.jobs in a block that first states the three transaction-local settings by hand (kind, actor id and reason; an empty string is "not stated"). It is for the tests
// that must run a move the way a writer who does not use platform.job_transition would, and for the refusals that need a setting to be missing or wrong. update must be a complete statement with its
// literals in it (ids and statuses of the test, never user input).
func jobAs(kind, actorID, reason, update string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return fmt.Sprintf(`DO $do$ BEGIN
		PERFORM set_config('platform.actor_kind', %s, true);
		PERFORM set_config('platform.actor_id', %s, true);
		PERFORM set_config('platform.reason', %s, true);
		%s;
	END $do$`, q(kind), q(actorID), q(reason), update)
}

// jobAsOnce is jobAs followed by the clearing of the three settings, as platform.job_transition clears them after its one statement: a fixture that moves a job by hand must not leave the settings behind
// for the next bare UPDATE of the transaction to be authorised by.
func jobAsOnce(kind, actorID, reason, update string) string {
	return jobAs(kind, actorID, reason, update+";\n\t\tPERFORM set_config('platform.actor_kind', '', true);\n\t\tPERFORM set_config('platform.actor_id', '', true);\n\t\tPERFORM set_config('platform.reason', '', true)")
}

// jobMoveBy is the statement of jobAs that moves one job to a status.
func jobMoveBy(kind, actorID, reason, id, to string) string {
	return jobAs(kind, actorID, reason, fmt.Sprintf("UPDATE platform.jobs SET status = '%s' WHERE id = '%s'", to, id))
}

// jobChainProblems checks the hand-written chains against an expected list of legal moves and the hops against the actors that list allows: every chain ends at its target, no chain repeats a status, every
// hop is a legal pair, and the actor a fixture takes for a hop is one the pair allows. It is a function of its inputs so that a test can hand it a broken copy.
func jobChainProblems(chains map[string][][]string, userHops map[string]bool, legal map[string][]string) []string {
	var p []string
	for target, list := range chains {
		for _, chain := range list {
			if len(chain) < 2 || chain[len(chain)-1] != target {
				p = append(p, fmt.Sprintf("the chain %v of target %s does not end at it", chain, target))
				continue
			}
			seen := map[string]bool{}
			for i, s := range chain {
				if seen[s] {
					p = append(p, fmt.Sprintf("the chain %v of target %s repeats %s", chain, target, s))
				}
				seen[s] = true
				if i == 0 {
					continue
				}
				hop := chain[i-1] + ">" + s
				actors, ok := legal[hop]
				if !ok {
					p = append(p, fmt.Sprintf("the chain %v of target %s takes the move %s, which is not a legal move", chain, target, hop))
					continue
				}
				kind := "engine"
				if userHops[hop] {
					kind = "user"
				}
				if !containsString(actors, kind) {
					p = append(p, fmt.Sprintf("the chain %v of target %s takes %s as %s, but only %v may", chain, target, hop, kind, actors))
				}
			}
		}
	}
	for hop := range userHops {
		if _, ok := legal[hop]; !ok {
			p = append(p, fmt.Sprintf("jobUserHops names the move %s, which is not a legal move", hop))
		}
	}
	return p
}
