//go:build harness

package harness

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The job lifecycle (Phase 4, chunk B1, platform_0011; Docs/phase-4-spec.md, Locked 21, 22 and 26): a job is BORN draft and changes status only along platform.job_transitions, by an actor that row allows, with a
// reason, within what its module can do and the database's clock, and every move writes its event. These tests hold the database to that, whoever the writer is.
//
// The method: the EXPECTED list of legal moves below is written by hand, here, and is never read from the table it judges. TestJobLifecycle_TheExpectedListIsTheTable holds the list and the rows of
// platform.job_transitions equal in both directions, so a drift of either fails; every other test takes its expectations from the list or from a hand-written table of its own, and every refusal names the
// constraint that raised it (ledgerExpectRefused), beside the legal twin of the same move. The fixtures (jobs_helpers_test.go) are hand-written chains of the same moves, held to this list without a database
// by TestJobLifecycle_FixturePathsAreLegalMoves. Each test runs on a migrated scratch database inside a transaction that is rolled back.

// jobMove is one legal move of the expected list: from a status to a status, taken by an actor of one of these kinds.
type jobMove struct {
	From, To string
	Actors   []string
}

// jobStates are the twelve statuses of platform.jobs, the four terminal ones last.
var jobStates = []string{"draft", "previewed", "confirmed", "scheduled", "running", "paused", "cancelling", "completing", "succeeded", "failed", "cancelled", "expired"}

// jobLegal is the expected list: every legal (from, to) pair and the actors that may take it (user: the job's owner; admin: a named admin; engine: the job engine, with no id). It is the design of
// Docs/phase-4-spec.md and of the comments of platform_0011, written out by hand.
var jobLegal = []jobMove{
	{"draft", "previewed", []string{"user", "admin"}},
	{"draft", "cancelled", []string{"user", "admin"}},
	{"previewed", "draft", []string{"user", "admin"}},
	{"previewed", "confirmed", []string{"user", "admin"}},
	{"previewed", "scheduled", []string{"user", "admin"}},
	{"previewed", "cancelled", []string{"user", "admin"}},
	{"confirmed", "running", []string{"engine"}},
	{"confirmed", "cancelled", []string{"user", "admin"}},
	{"confirmed", "expired", []string{"engine"}},
	{"confirmed", "failed", []string{"admin", "engine"}},
	{"scheduled", "running", []string{"engine"}},
	{"scheduled", "paused", []string{"user", "admin"}},
	{"scheduled", "cancelled", []string{"user", "admin"}},
	{"scheduled", "expired", []string{"engine"}},
	{"scheduled", "failed", []string{"admin", "engine"}},
	{"paused", "scheduled", []string{"user", "admin"}},
	{"paused", "running", []string{"user", "admin"}},
	{"paused", "cancelled", []string{"user", "admin", "engine"}}, // platform_0018: the engine too, for a job that paused from SCHEDULED, once its pause outlasted PLATFORM_JOB_PAUSE_MAX_DAYS (the wall of platform_0016 still refuses it for a job paused while running)
	{"paused", "expired", []string{"engine"}},
	{"paused", "failed", []string{"admin", "engine"}},
	// platform_0016: a job paused WHILE RUNNING is held by its module, so its cancel goes through the engine (paused -> cancelling) and, when the module reports every item final while it is held, it ends the
	// normal way (paused -> completing). The database allows both only for a job that paused from running; a job that paused from scheduled submitted nothing and is cancelled directly (paused -> cancelled).
	{"paused", "cancelling", []string{"user", "admin", "engine"}}, // platform_0018: the engine too, for a job that paused from RUNNING (the wall of platform_0016 still refuses it for one paused before its start)
	{"paused", "completing", []string{"engine"}},
	{"running", "paused", []string{"user", "admin"}},
	{"running", "cancelling", []string{"user", "admin"}},
	{"running", "completing", []string{"engine"}},
	{"running", "failed", []string{"admin", "engine"}},
	{"cancelling", "completing", []string{"engine"}},
	{"cancelling", "failed", []string{"admin", "engine"}},
	{"completing", "succeeded", []string{"engine"}},
	{"completing", "cancelled", []string{"engine"}},
	{"completing", "failed", []string{"admin", "engine"}},
}

func jobMoveOf(from, to string) (jobMove, bool) {
	for _, m := range jobLegal {
		if m.From == from && m.To == to {
			return m, true
		}
	}
	return jobMove{}, false
}

// jobIsLegal says whether (from, to) is a row of the expected list.
func jobIsLegal(from, to string) bool {
	_, ok := jobMoveOf(from, to)
	return ok
}

func jobActorsOf(from, to string) []string {
	m, _ := jobMoveOf(from, to)
	return m.Actors
}

// jobLegalActors is the expected list keyed "from>to", the form jobChainProblems takes.
func jobLegalActors() map[string][]string {
	out := map[string][]string{}
	for _, m := range jobLegal {
		out[m.From+">"+m.To] = m.Actors
	}
	return out
}

// jobActorFor is the actor id a move of this kind states for a job of ledgerUserA: the owner, an active admin, nobody for the engine.
func jobActorFor(kind string) any {
	switch kind {
	case "user":
		return ledgerUserA
	case "admin":
		return ledgerAdminA
	}
	return nil
}

// jobOutcome is what one statement did: nothing refused it, or this SQLSTATE and constraint did.
type jobOutcome struct{ State, Constraint, Message string }

func (o jobOutcome) ok() bool { return o.State == "" }

func (o jobOutcome) String() string {
	if o.ok() {
		return "accepted"
	}
	return fmt.Sprintf("refused %s %q", o.State, o.Constraint)
}

// jobTry runs one statement in a savepoint that is ALWAYS rolled back and says what happened to it.
func jobTry(t *testing.T, tx pgx.Tx, sql string, args ...any) jobOutcome {
	t.Helper()
	ctx := testCtx(t)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := sp.Exec(ctx, sql, args...)
	if err := sp.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if execErr == nil {
		return jobOutcome{}
	}
	var pe *pgconn.PgError
	if !errors.As(execErr, &pe) {
		t.Fatalf("a statement failed with a non-database error: %v\n  sql: %s", execErr, ledgerOneLine(sql))
	}
	return jobOutcome{State: pe.Code, Constraint: pe.ConstraintName, Message: pe.Message}
}

// jobEvents is the number of events a job has.
func jobEvents(t *testing.T, db ledgerDB, id string) int64 {
	t.Helper()
	return ledgerInt64(t, db, "SELECT count(*) FROM platform.job_events WHERE job_id = $1", id)
}

// jobIDs hands out ids for the jobs a test makes: ledgerUUID(prefix, 1), (prefix, 2) and so on.
func jobIDs(prefix string) func() string {
	n := 0
	return func() string { n++; return ledgerUUID(prefix, n) }
}

// ---------------------------------------------------------------------------------------------------------------------
// the fixtures and the expected list
// ---------------------------------------------------------------------------------------------------------------------

// TestJobLifecycle_FixturePathsAreLegalMoves holds the hand-written chains of the fixtures (jobChains, jobUserHops: how every other test makes a job of a status) to the expected list, with no
// database: every chain ends at its target, repeats nothing, takes only legal moves and takes each as an actor that move allows; every status has a chain to it; no hop names a move that is not legal.
// The control feeds jobChainProblems five broken copies and requires each to be reported for its reason, so the check cannot agree with everything.
// It fails if a fixture path drifts from the design, so that the next test to need a running job does not die with a refusal that has nothing to do with it.
func TestJobLifecycle_FixturePathsAreLegalMoves(t *testing.T) {
	if p := jobChainProblems(jobChains, jobUserHops, jobLegalActors()); len(p) != 0 {
		t.Fatalf("%d problem(s) in the fixture paths:\n  %s", len(p), strings.Join(p, "\n  "))
	}
	for _, s := range jobStates {
		if len(jobChains[s]) == 0 {
			t.Fatalf("no chain leads to %s: a test could not make a job of that status", s)
		}
	}
	if len(jobChains) != len(jobStates) || len(jobLegal) != 31 {
		t.Fatalf("%d targets for %d statuses and %d legal moves: the instrument looks at the wrong lists", len(jobChains), len(jobStates), len(jobLegal))
	}

	clone := func() map[string][][]string {
		out := map[string][][]string{}
		for k, v := range jobChains {
			for _, c := range v {
				out[k] = append(out[k], append([]string(nil), c...))
			}
		}
		return out
	}
	cases := []struct {
		name   string
		chains map[string][][]string
		hops   map[string]bool
		want   string
	}{
		{"a chain that stops short of its target", func() map[string][][]string {
			c := clone()
			c["succeeded"][0] = c["succeeded"][0][:len(c["succeeded"][0])-1]
			return c
		}(), jobUserHops, "does not end at it"},
		{"a chain that takes a move that is not legal (draft straight to running)", func() map[string][][]string {
			c := clone()
			c["running"][0] = []string{"draft", "running"}
			return c
		}(), jobUserHops, "which is not a legal move"},
		{"a chain that repeats a status", func() map[string][][]string {
			c := clone()
			c["running"][0] = []string{"draft", "previewed", "draft", "previewed", "confirmed", "running"}
			return c
		}(), jobUserHops, "repeats"},
		{"the engine taking a move only a user or an admin may take", clone(), func() map[string]bool {
			h := map[string]bool{}
			for k, v := range jobUserHops {
				h[k] = v
			}
			delete(h, "draft>previewed")
			return h
		}(), "takes draft>previewed as engine"},
		{"a user taking a move only the engine may take", clone(), func() map[string]bool {
			h := map[string]bool{}
			for k, v := range jobUserHops {
				h[k] = v
			}
			h["confirmed>running"] = true
			return h
		}(), "takes confirmed>running as user"},
	}
	for _, c := range cases {
		c := c
		t.Run("control: "+c.name, func(t *testing.T) {
			p := jobChainProblems(c.chains, c.hops, jobLegalActors())
			if len(p) == 0 {
				t.Fatal("the check reported nothing for a broken copy: it cannot fail")
			}
			if !strings.Contains(strings.Join(p, "\n"), c.want) {
				t.Fatalf("no problem mentions %q; the check said:\n  %s", c.want, strings.Join(p, "\n  "))
			}
		})
	}
}

// jobTableRows reads platform.job_transitions as the list of moves it holds.
func jobTableRows(t *testing.T, db ledgerDB) []jobMove {
	t.Helper()
	rows, err := db.Query(testCtx(t), "SELECT from_status, to_status, actors FROM platform.job_transitions ORDER BY 1, 2")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []jobMove
	for rows.Next() {
		var m jobMove
		if err := rows.Scan(&m.From, &m.To, &m.Actors); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// jobDrift is every difference between an expected list and the rows of the table, in both directions, as sentences.
func jobDrift(want, got []jobMove) []string {
	key := func(m jobMove) string { return m.From + ">" + m.To }
	actors := func(m jobMove) string {
		a := append([]string(nil), m.Actors...)
		sort.Strings(a)
		return strings.Join(a, ",")
	}
	have := map[string]jobMove{}
	for _, m := range got {
		have[key(m)] = m
	}
	expected := map[string]jobMove{}
	for _, m := range want {
		expected[key(m)] = m
	}
	var d []string
	for _, m := range want {
		g, ok := have[key(m)]
		switch {
		case !ok:
			d = append(d, fmt.Sprintf("%s is in the expected list but not in platform.job_transitions", key(m)))
		case actors(g) != actors(m):
			d = append(d, fmt.Sprintf("%s: the table allows the actors [%s], the expected list [%s]", key(m), actors(g), actors(m)))
		}
	}
	for _, m := range got {
		if _, ok := expected[key(m)]; !ok {
			d = append(d, fmt.Sprintf("%s is in platform.job_transitions but not in the expected list", key(m)))
		}
	}
	sort.Strings(d)
	return d
}

// TestJobLifecycle_TheExpectedListIsTheTable: the 31 legal moves written out by hand in this file and the rows of platform.job_transitions are the same set with the same actors, in both directions. The
// control is the comparison itself: against a list with a move missing, a list with a move too many and a list with other actors it reports each, and against a TABLE that gains a row or loses one (a
// savepoint that is rolled back; the superuser may insert, and deletes only with the append-only trigger switched off) it reports that too, so it reads the table and not a copy of the list.
// It fails if a migration changes a legal move without this list changing on purpose.
func TestJobLifecycle_TheExpectedListIsTheTable(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)

	got := jobTableRows(t, tx)
	if len(got) < 20 {
		t.Fatalf("platform.job_transitions holds %d rows: the instrument matched too little", len(got))
	}
	if d := jobDrift(jobLegal, got); len(d) != 0 {
		t.Fatalf("the expected list and platform.job_transitions differ:\n  %s", strings.Join(d, "\n  "))
	}

	t.Run("control: the comparison reports a list that is not the table", func(t *testing.T) {
		short := append([]jobMove(nil), jobLegal[1:]...)
		if d := jobDrift(short, got); len(d) != 1 || !strings.Contains(d[0], "draft>previewed is in platform.job_transitions but not in the expected list") {
			t.Fatalf("a list without draft>previewed: the comparison said %v", d)
		}
		long := append(append([]jobMove(nil), jobLegal...), jobMove{"previewed", "running", []string{"engine"}})
		if d := jobDrift(long, got); len(d) != 1 || !strings.Contains(d[0], "previewed>running is in the expected list but not in platform.job_transitions") {
			t.Fatalf("a list with previewed>running: the comparison said %v", d)
		}
		other := append([]jobMove(nil), jobLegal...)
		other[0] = jobMove{"draft", "previewed", []string{"engine"}}
		if d := jobDrift(other, got); len(d) != 1 || !strings.Contains(d[0], "draft>previewed: the table allows the actors [admin,user], the expected list [engine]") {
			t.Fatalf("a list with other actors for draft>previewed: the comparison said %v", d)
		}
	})
	t.Run("control: the comparison reports a table that is not the list", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerExpectOK(t, sp, "the owner adds a move (the table is insert-only, never closed to a migration)", "INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES ('previewed', 'running', ARRAY['engine'], 'a control')")
			if d := jobDrift(jobLegal, jobTableRows(t, sp)); len(d) != 1 || !strings.Contains(d[0], "previewed>running is in platform.job_transitions but not in the expected list") {
				t.Fatalf("a table with an extra move: the comparison said %v", d)
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.job_transitions DISABLE TRIGGER job_transitions_append_only")
			ledgerExpectOK(t, sp, "a move deleted with the append-only trigger off", "DELETE FROM platform.job_transitions WHERE from_status = 'running' AND to_status = 'completing'")
			if d := jobDrift(jobLegal, jobTableRows(t, sp)); len(d) != 1 || !strings.Contains(d[0], "running>completing is in the expected list but not in platform.job_transitions") {
				t.Fatalf("a table with a move missing: the comparison said %v", d)
			}
		})
		if d := jobDrift(jobLegal, jobTableRows(t, tx)); len(d) != 0 {
			t.Fatalf("the rolled-back savepoints left the table changed: %v", d)
		}
	})

	t.Run("the table's own CHECKs, each beside the legal row and with its NULL case", func(t *testing.T) {
		const ins = "INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES ($1, $2, $3, $4)"
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerExpectOK(t, sp, "control: a legal new row (previewed -> running by the engine)", ins, "previewed", "running", []string{"engine"}, "a control")
		})
		ok := []any{"previewed", "running", []string{"engine"}, "a control"}
		with := func(i int, v any) []any { a := append([]any(nil), ok...); a[i] = v; return a }
		refused := func(what, state, constraint string, args []any) {
			t.Helper()
			ledgerExpectRefused(t, tx, what, state, ledgerWhy{Constraint: constraint}, ins, args...)
		}
		refused("a terminal status to move FROM (a terminal job never moves)", "23514", "job_transitions_from_vocabulary", with(0, "succeeded"))
		refused("a status that is not one of the twelve to move TO", "23514", "job_transitions_to_vocabulary", with(1, "bogus"))
		refused("a move to itself", "23514", "job_transitions_moves", []any{"running", "running", []string{"engine"}, "a control"})
		refused("no actor at all (an empty array)", "23514", "job_transitions_actors", with(2, []string{}))
		refused("an actor that is not a kind", "23514", "job_transitions_actors", with(2, []string{"robot"}))
		refused("an actor list with a NULL in it (the CHECK must not evaluate to NULL and pass)", "23514", "job_transitions_actors", with(2, []*string{nil}))
		refused("a blank note", "23514", "job_transitions_note_not_blank", with(3, "   "))
		ledgerExpectRefused(t, tx, "a NULL from_status", "23502", ledgerWhy{Column: "from_status"}, ins, with(0, nil)...)
		ledgerExpectRefused(t, tx, "a NULL to_status", "23502", ledgerWhy{Column: "to_status"}, ins, with(1, nil)...)
		ledgerExpectRefused(t, tx, "a NULL actors", "23502", ledgerWhy{Column: "actors"}, ins, with(2, nil)...)
		ledgerExpectRefused(t, tx, "a NULL note", "23502", ledgerWhy{Column: "note"}, ins, with(3, nil)...)
		refused("a move that is already a row (the key is the pair)", "23505", "job_transitions_pk", []any{"draft", "previewed", []string{"engine"}, "a control"})
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the matrix
// ---------------------------------------------------------------------------------------------------------------------

// jobFlavor is one way of being in a status: a paused job paused from scheduled or from running, a completing job after a cancel the module accepted or without one. The rules that look at HOW a job got
// there (the resume rule, the expiry of a paused job, a cancelled job never succeeds and the reverse) can only be told apart on the flavors.
type jobFlavor struct {
	Name  string
	Build func(t *testing.T, sp pgx.Tx, id string, o jobOpts)
}

func jobFlavorsOf(status string) []jobFlavor {
	walk := jobFlavor{status, func(t *testing.T, sp pgx.Tx, id string, o jobOpts) { jobWalkToWith(t, sp, id, status, o) }}
	switch status {
	case "paused":
		return []jobFlavor{
			{"paused from scheduled", func(t *testing.T, sp pgx.Tx, id string, o jobOpts) { jobWalkToWith(t, sp, id, "paused", o) }},
			{"paused from running", func(t *testing.T, sp pgx.Tx, id string, o jobOpts) {
				// module 2 can pause a running job since platform_0016 (platform.modules.can_pause is its seed): no flip is needed
				jobPauseWhileRunningWith(t, sp, id, o)
			}},
		}
	case "completing":
		return []jobFlavor{
			{"completing, no cancel", func(t *testing.T, sp pgx.Tx, id string, o jobOpts) { jobWalkToWith(t, sp, id, "completing", o) }},
			{"completing after an accepted cancel", func(t *testing.T, sp pgx.Tx, id string, o jobOpts) {
				jobWalkToWith(t, sp, id, "running", o)
				jobWalkTo(t, sp, id, "cancelling")
				jobWalkTo(t, sp, id, "completing")
			}},
		}
	}
	return []jobFlavor{walk}
}

// jobFlavorRefusals are the moves that are rows of the lifecycle and that a job in a status still cannot take in SOME flavor, and the constraint that says why.
var jobFlavorRefusals = map[string][]string{
	"paused>scheduled": {"job_resume_to_where_it_paused_from"},
	"paused>running":   {"job_resume_to_where_it_paused_from"},
	"paused>expired":   {"job_transition_illegal"},
	// platform_0016: what a job that paused from scheduled (it submitted nothing) cannot do, and what one that paused from running (its module holds items) cannot do
	"paused>cancelling":    {"job_unsubmitted_pause_never_cancelling"},
	"paused>completing":    {"job_unsubmitted_pause_never_completing"},
	"paused>cancelled":     {"job_submitted_pause_cancels_through_the_engine"},
	"completing>succeeded": {"job_cancelled_cannot_succeed"},
	"completing>cancelled": {"job_cancelled_needs_a_cancel"},
}

// TestJobLifecycle_Matrix: for EVERY ordered pair of the twelve statuses (132 pairs) and each of the three kinds of actor (396 cells), a move is accepted exactly when the pair is in the hand-written expected list AND
// the actor is one the list allows for it (54 cells). The job of each cell is a fresh draft job walked to the starting status (jobWalkTo) inside a savepoint that is rolled back; for a pair of the list what the move
// needs is made true first (a start time that has passed, a deadline that has, a module that can pause), so that the legal twin of every refusal is in the same matrix. A refused cell is refused for ITS reason:
// from a terminal status jobs_terminal_is_final; a pair that is not in the list job_transition_illegal; a pair in the list taken by the wrong actor job_transition_actor_not_allowed; the moves that depend on how
// the job got where it is (a pause remembers where it paused from, a cancel decides how a job ends) are tried in each flavor and are accepted in at least one and refused in the others for the one named reason.
// A status written back to itself is no move: accepted, and no event. Every difference is collected and printed at once.
// It fails if a pair the design forbids is accepted, a pair it allows is refused, or a refusal comes from the wrong rule.
func TestJobLifecycle_Matrix(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("9a")
	kinds := []string{"user", "admin", "engine"}

	var problems []string
	cells, accepted, wantAccepted := 0, 0, 0
	for _, m := range jobLegal {
		wantAccepted += len(m.Actors)
	}
	for _, from := range jobStates {
		for _, to := range jobStates {
			if from == to {
				continue
			}
			legal := jobIsLegal(from, to)
			for _, kind := range kinds {
				cells++
				allowed := legal && containsString(jobActorsOf(from, to), kind)
				var outcomes []jobOutcome
				for _, fl := range jobFlavorsOf(from) {
					fl := fl
					ledgerTrial(t, tx, func(sp pgx.Tx) {
						id := next()
						ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", id, ledgerUserA)
						o := jobOpts{}
						if legal && to == "expired" {
							o.Deadline = jobDeadlinePast // the database only forbids EXPIRING before the deadline: the fixture carries one that has passed
						}
						fl.Build(t, sp, id, o)
						if got := jobStatus(t, sp, id); got != from {
							t.Fatalf("the cell %s -> %s (%s): the fixture is %s, not %s", from, to, fl.Name, got, from)
						}
						if legal {
							jobPrepareHop(t, sp, id, from, to)
						}
						sql, args := jobMoveStmt(from, to, id, kind, jobActorFor(kind), "the matrix", o)
						outcomes = append(outcomes, jobTry(t, sp, sql, args...))
					})
				}
				where := fmt.Sprintf("%s -> %s by %s", from, to, kind)
				if allowed {
					okCount := 0
					for i, o := range outcomes {
						switch {
						case o.ok():
							okCount++
						case o.State != "23001" || !containsString(jobFlavorRefusals[from+">"+to], o.Constraint):
							problems = append(problems, fmt.Sprintf("%s (%s): a legal move was %s, not for one of %v", where, jobFlavorsOf(from)[i].Name, o, jobFlavorRefusals[from+">"+to]))
						}
					}
					if okCount == 0 {
						problems = append(problems, where+": a legal move was never accepted, in any flavor")
					} else {
						accepted++
					}
					continue
				}
				want := "job_transition_illegal"
				switch {
				case jobIsTerminal(from):
					want = "jobs_terminal_is_final"
				case legal:
					want = "job_transition_actor_not_allowed"
				}
				for i, o := range outcomes {
					if o.State != "23001" || o.Constraint != want {
						problems = append(problems, fmt.Sprintf("%s (%s): %s, want refused 23001 %q", where, jobFlavorsOf(from)[i].Name, o, want))
					}
				}
			}
		}
	}
	if len(problems) > 0 {
		shown := problems
		if len(shown) > 40 {
			shown = append(shown[:40:40], fmt.Sprintf("... and %d more", len(problems)-40))
		}
		t.Fatalf("%d cell(s) of the matrix are not what the expected list says:\n  %s", len(problems), strings.Join(shown, "\n  "))
	}
	if want := len(jobStates) * (len(jobStates) - 1) * len(kinds); cells != want {
		t.Fatalf("the matrix tried %d cells, want %d (12 x 11 x 3)", cells, want)
	}
	if accepted != wantAccepted || accepted != 54 {
		t.Fatalf("the matrix accepted %d cells, want %d (one for each actor of each of the 31 legal moves; the expected list says %d)", accepted, 54, wantAccepted)
	}

	t.Run("a status written back to itself is no move: accepted, and it writes no event", func(t *testing.T) {
		for _, s := range jobStates {
			s := s
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, ledgerUserA, "m2", s)
				before := jobEvents(t, sp, id)
				ledgerExpectOK(t, sp, "a "+s+" job written "+s+" again", "UPDATE platform.jobs SET status = $2 WHERE id = $1", id, s)
				if after := jobEvents(t, sp, id); after != before {
					t.Fatalf("a %s job written %s again wrote %d event(s): it is no move", s, s, after-before)
				}
			})
		}
	})
	t.Run("control: a real move writes exactly one event", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, ledgerUserA, "m2", "previewed")
			before := jobEvents(t, sp, id)
			jobWalkTo(t, sp, id, "cancelled")
			if after := jobEvents(t, sp, id); after != before+1 {
				t.Fatalf("a move from previewed to cancelled wrote %d event(s), want 1", after-before)
			}
		})
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the guard's refusals
// ---------------------------------------------------------------------------------------------------------------------

// TestJobLifecycle_TheGuardRefusals: each rule of jobs_transition_guard, refused with 23001 and ITS constraint name, beside the legal twin of the same move. A job is born draft (job_born_draft: an INSERT with any
// other status, a NULL one left to NOT NULL, and the role the service runs as meets it too); a move names its kind and its reason (job_transition_needs_an_actor: none, an empty one, a blank one; the
// settings of one move do not authorise the next); an actor of a kind the pair does not allow is refused (job_transition_actor_not_allowed); a user acts on their own job only (job_transition_by_another_user:
// another user or an admin's id; a user or an admin that names no id, and an engine that carries one, are job_transition_needs_an_actor); what the module can do to one running
// job is data (running -> paused is refused for module 1 with job_module_cannot_pause and accepted for modules 2 and 3 since platform_0016; running -> cancelling is accepted for all three; a flipped row
// changes the answer in both directions, and the cancel capability also judges paused -> cancelling of a job that paused from running with job_module_cannot_cancel); a pause resumes only where it paused from (job_resume_to_where_it_paused_from) and the writer cannot choose where that is; a scheduled job
// does not start before its time (job_started_early) nor anything expire before its deadline (job_expired_early: a microsecond early is early, exactly on time is not, a missing deadline is early, and a paused job
// expires only if it paused from scheduled); a job whose cancel was accepted never succeeds and one whose cancel was not never ends cancelled (job_cancelled_cannot_succeed, job_cancelled_needs_a_cancel); a
// terminal status never moves (jobs_terminal_is_final) for each of the four.
// It fails if any of the refusals is missing, comes from another rule, or if the legal twin is refused.
func TestJobLifecycle_TheGuardRefusals(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("9b")
	a, b := ledgerUserA, ledgerUserB
	why := func(constraint string) ledgerWhy { return ledgerWhy{Constraint: constraint} }
	at := func(sp pgx.Tx, module, status string) string {
		id := next()
		jobInsertAt(t, sp, id, a, module, status)
		return id
	}

	t.Run("a job is born draft", func(t *testing.T) {
		const insert = "INSERT INTO platform.jobs (id, user_id, module_id, status) VALUES ($1, $2, 'm2', $3)"
		ledgerExpectOK(t, tx, "control: a job inserted draft", insert, next(), a, "draft")
		for _, s := range jobStates {
			if s != "draft" {
				ledgerExpectRefused(t, tx, "a job inserted "+s, "23001", why("job_born_draft"), insert, next(), a, s)
			}
		}
		ledgerExpectRefused(t, tx, "a job inserted with a status that is not one", "23001", why("job_born_draft"), insert, next(), a, "bogus")
		ledgerExpectRefused(t, tx, "a job inserted with a NULL status (the trigger's comparison is NULL and lets it through; the NOT NULL column refuses it)", "23502", ledgerWhy{Column: "status"}, insert, next(), a, nil)
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectOK(t, tx, "control: platform_app inserts a draft job", insert, next(), a, "draft")
		ledgerExpectRefused(t, tx, "platform_app inserts a running job", "23001", why("job_born_draft"), insert, next(), a, "running")
		ledgerResetRole(t, tx)
	})

	t.Run("a move names its actor and its reason", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "previewed")
			needs := why("job_transition_needs_an_actor")
			ledgerExpectRefused(t, sp, "a bare UPDATE of the status (no setting at all)", "23001", needs, "UPDATE platform.jobs SET status = 'confirmed' WHERE id = $1", id)
			for _, c := range []struct{ what, kind, reason string }{
				{"no kind", "", "why"}, {"a blank kind", "   ", "why"}, {"no reason", "user", ""}, {"a blank reason", "user", "   "},
			} {
				// the move into confirmed carries the start deadline in the same statement (jobMoveStmt), so the settings are stated by hand; "" and blank are what the function's NULL and '' become
				sql, args := jobMoveStmt("previewed", "confirmed", id, c.kind, a, c.reason, jobOpts{})
				ledgerExpectRefused(t, sp, "a move with "+c.what, "23001", needs, sql, args...)
			}
			sql, args := jobMoveStmt("previewed", "confirmed", id, "user", a, "why", jobOpts{})
			ledgerExpectOK(t, sp, "control: the same move with a kind, an id and a reason", sql, args...)
			ledgerExpectOK(t, sp, "control: the next move through the function", jobMoveSQL, id, "running", "engine", nil, "why")
			ledgerExpectRefused(t, sp, "the next move as a bare UPDATE right after: the settings of the function's move were for that one statement", "23001", needs,
				"UPDATE platform.jobs SET status = 'completing' WHERE id = $1", id)
			for _, kind := range []any{nil, ""} { // the function takes NULL and the empty string for "not stated" alike
				ledgerExpectRefused(t, sp, fmt.Sprintf("the function called with the kind %v", kind), "23001", needs, jobMoveSQL, id, "completing", kind, nil, "why")
			}
			ledgerExpectRefused(t, sp, "the function called with no reason", "23001", needs, jobMoveSQL, id, "completing", "engine", nil, nil)
		})
	})

	t.Run("an actor of a kind the move does not allow is refused", func(t *testing.T) {
		tried := 0
		for _, c := range []struct{ from, to, kind string }{
			{"draft", "previewed", "engine"}, {"previewed", "confirmed", "engine"}, {"previewed", "cancelled", "engine"}, {"confirmed", "running", "user"}, {"confirmed", "running", "admin"},
			{"running", "completing", "user"}, {"running", "cancelling", "engine"}, {"completing", "succeeded", "admin"}, {"completing", "failed", "user"}, {"confirmed", "expired", "admin"},
			{"draft", "previewed", "robot"},
		} {
			c := c
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				o := jobOpts{}
				if c.to == "expired" {
					o.Deadline = jobDeadlinePast
				}
				id := next()
				jobInsertAtWith(t, sp, id, a, "m2", c.from, o)
				jobPrepareHop(t, sp, id, c.from, c.to)
				ctl := jobActorsOf(c.from, c.to)[0]
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					sql, args := jobMoveStmt(c.from, c.to, id, ctl, jobActorFor(ctl), "why", o)
					ledgerExpectOK(t, sp2, fmt.Sprintf("control: %s -> %s as %s", c.from, c.to, ctl), sql, args...)
				})
				sql, args := jobMoveStmt(c.from, c.to, id, c.kind, jobActorFor(c.kind), "why", o)
				ledgerExpectRefused(t, sp, fmt.Sprintf("%s -> %s as %s", c.from, c.to, c.kind), "23001", why("job_transition_actor_not_allowed"), sql, args...)
			})
			tried++
		}
		if tried != 11 {
			t.Fatalf("%d cases tried, want 11", tried)
		}
	})

	t.Run("a user acts on their own job only, the engine carries no id, an admin names who", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "draft")
			jobPrepareHop(t, sp, id, "draft", "previewed")
			another := why("job_transition_by_another_user")
			ledgerExpectRefused(t, sp, "another user's move", "23001", another, jobMoveSQL, id, "previewed", "user", b, "why")
			ledgerExpectRefused(t, sp, "an admin's id stated as a user's", "23001", another, jobMoveSQL, id, "previewed", "user", ledgerAdminA, "why")
			ledgerExpectRefused(t, sp, "a user's move that names nobody (the id is required before the owner is compared)", "23001", why("job_transition_needs_an_actor"), jobMoveSQL, id, "previewed", "user", nil, "why")
			ledgerExpectOK(t, sp, "control: the owner", jobMoveSQL, id, "previewed", "user", a, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "draft")
			jobPrepareHop(t, sp, id, "draft", "previewed")
			ledgerExpectRefused(t, sp, "an admin's move that names nobody", "23001", why("job_transition_needs_an_actor"), jobMoveSQL, id, "previewed", "admin", nil, "why")
			ledgerExpectOK(t, sp, "control: an admin may act on another user's job (the id of an admin, not of the owner)", jobMoveSQL, id, "previewed", "admin", ledgerAdminA, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "confirmed")
			ledgerExpectRefused(t, sp, "the engine with an actor id (it is not a person)", "23001", why("job_transition_needs_an_actor"), jobMoveSQL, id, "running", "engine", ledgerAdminA, "why")
			ledgerExpectOK(t, sp, "control: the engine with no id", jobMoveSQL, id, "running", "engine", nil, "why")
		})
	})

	t.Run("what the module can do to one running job is data", func(t *testing.T) {
		// platform_0016 seeds what the modules gained at the close-out: modules 2 and 3 pause one campaign (POST /v1/pause), every module cancels one job (module 1: POST /v1/jobs/{id}/cancel); module 1 has
		// no per-job pause. (platform_0011 had seeded m1|false|false, m2|false|true, m3|false|true.)
		if got := ledgerRows(t, tx, "SELECT id, can_pause::text, can_cancel::text FROM platform.modules ORDER BY id"); strings.Join(got, ",") != "m1|false|true,m2|true|true,m3|true|true" {
			t.Fatalf("the capabilities of the modules are %v, want m1 can cancel but not pause, m2 and m3 can do both (platform_0016)", got)
		}
		ledgerExpectRefused(t, tx, "a module with a NULL can_pause", "23502", ledgerWhy{Column: "can_pause"},
			"INSERT INTO platform.modules (id, name, schema_name, metered, can_pause) VALUES ('m7', 'seventh', 'seventh_schema', true, NULL)")
		ledgerExpectRefused(t, tx, "a module with a NULL can_cancel", "23502", ledgerWhy{Column: "can_cancel"},
			"INSERT INTO platform.modules (id, name, schema_name, metered, can_cancel) VALUES ('m7', 'seventh', 'seventh_schema', true, NULL)")
		wantPause := map[string]bool{"m1": false, "m2": true, "m3": true}
		for _, m := range []string{"m1", "m2", "m3"} {
			m := m
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := at(sp, m, "running")
				// the pause: refused for module 1 (no per-job pause), accepted for modules 2 and 3 by the user and by an admin, and then the job says it paused from running
				for _, kind := range []string{"user", "admin"} {
					if wantPause[m] {
						ledgerTrial(t, sp, func(sp2 pgx.Tx) {
							ledgerExpectOK(t, sp2, fmt.Sprintf("control: running -> paused for module %s by %s", m, kind), jobMoveSQL, id, "paused", kind, jobActorFor(kind), "why")
							if got := ledgerString(t, sp2, "SELECT paused_from FROM platform.jobs WHERE id = $1", id); got != "running" {
								t.Fatalf("a job paused from running says it paused from %q", got)
							}
						})
					} else {
						ledgerExpectRefused(t, sp, fmt.Sprintf("running -> paused for module %s by %s", m, kind), "23001", why("job_module_cannot_pause"), jobMoveSQL, id, "paused", kind, jobActorFor(kind), "why")
					}
				}
				// the cancel: every module can since platform_0016
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					ledgerExpectOK(t, sp2, "control: running -> cancelling for module "+m, jobMoveSQL, id, "cancelling", "user", a, "why")
				})
				// it is the data that decides, in both directions and for both capabilities: flip the row the seed holds and the same move changes its answer
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					ledgerMust(t, sp2, "UPDATE platform.modules SET can_pause = $2 WHERE id = $1", m, !wantPause[m])
					if wantPause[m] {
						ledgerExpectRefused(t, sp2, "running -> paused once module "+m+" can no longer pause", "23001", why("job_module_cannot_pause"), jobMoveSQL, id, "paused", "user", a, "why")
					} else {
						ledgerExpectOK(t, sp2, "running -> paused once module "+m+" can pause", jobMoveSQL, id, "paused", "user", a, "why")
						if got := ledgerString(t, sp2, "SELECT paused_from FROM platform.jobs WHERE id = $1", id); got != "running" {
							t.Fatalf("a job paused from running says it paused from %q", got)
						}
					}
				})
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					ledgerMust(t, sp2, "UPDATE platform.modules SET can_cancel = false WHERE id = $1", m)
					ledgerExpectRefused(t, sp2, "running -> cancelling once module "+m+" can no longer cancel", "23001", why("job_module_cannot_cancel"), jobMoveSQL, id, "cancelling", "user", a, "why")
				})
				// the same capability widened by platform_0016: a job that was paused WHILE RUNNING is a running job for what its module can do to it, so its cancel (paused -> cancelling) needs can_cancel too.
				// Module 1 has no per-job pause, so it is given one in the scratch savepoint to make the job.
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					ledgerMust(t, sp2, "UPDATE platform.modules SET can_pause = true WHERE id = $1", m)
					jobHop(t, sp2, id, "paused")
					if got := ledgerString(t, sp2, "SELECT paused_from FROM platform.jobs WHERE id = $1", id); got != "running" {
						t.Fatalf("the fixture paused from %q, want running", got)
					}
					for _, kind := range []string{"user", "admin"} {
						kind := kind
						ledgerTrial(t, sp2, func(sp3 pgx.Tx) {
							ledgerExpectOK(t, sp3, fmt.Sprintf("control: paused -> cancelling of a job of module %s that can cancel, by %s", m, kind), jobMoveSQL, id, "cancelling", kind, jobActorFor(kind), "why")
						})
					}
					ledgerTrial(t, sp2, func(sp3 pgx.Tx) {
						ledgerMust(t, sp3, "UPDATE platform.modules SET can_cancel = false WHERE id = $1", m)
						for _, kind := range []string{"user", "admin"} {
							ledgerExpectRefused(t, sp3, fmt.Sprintf("paused -> cancelling of a job of module %s once it can no longer cancel, by %s", m, kind), "23001", why("job_module_cannot_cancel"), jobMoveSQL, id, "cancelling", kind, jobActorFor(kind), "why")
						}
						// what is not a cancel is not judged by can_cancel: the engine still ends a held job whose module reported everything final, and the resume is still legal
						ledgerExpectOK(t, sp3, "control: paused -> completing does not need can_cancel", jobMoveSQL, id, "completing", "engine", nil, "why")
					})
				})
			})
		}
	})

	t.Run("a pause resumes only where it paused from, and the writer does not choose where that is", func(t *testing.T) {
		resume := why("job_resume_to_where_it_paused_from")
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "paused")
			if got := ledgerString(t, sp, "SELECT paused_from FROM platform.jobs WHERE id = $1", id); got != "scheduled" {
				t.Fatalf("a job paused before its start says it paused from %q, want scheduled", got)
			}
			ledgerExpectRefused(t, sp, "a job paused from scheduled resumed to running", "23001", resume, jobMoveSQL, id, "running", "user", a, "why")
			ledgerExpectOK(t, sp, "control: resumed to scheduled", jobMoveSQL, id, "scheduled", "user", a, "why")
			if got := ledgerString(t, sp, "SELECT COALESCE(paused_from, '-') FROM platform.jobs WHERE id = $1", id); got != "-" {
				t.Fatalf("a resumed job still says it paused from %q", got)
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "running") // module 2 can pause a running job: its seed since platform_0016
			jobPauseWhileRunning(t, sp, id)
			ledgerExpectRefused(t, sp, "a job paused from running resumed to scheduled", "23001", resume, jobMoveSQL, id, "scheduled", "user", a, "why")
			ledgerExpectOK(t, sp, "control: resumed to running", jobMoveSQL, id, "running", "user", a, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "scheduled")
			ledgerExpectOK(t, sp, "a pause whose statement also says paused_from = running", jobAs("user", a, "why", fmt.Sprintf("UPDATE platform.jobs SET status = 'paused', paused_from = 'running' WHERE id = '%s'", id)))
			if got := ledgerString(t, sp, "SELECT paused_from FROM platform.jobs WHERE id = $1", id); got != "scheduled" {
				t.Fatalf("the writer chose where the job paused from: %q, want the status it paused from, scheduled", got)
			}
		})
		// a paused job expires only if it paused from scheduled: a job that was running never expires
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAtWith(t, sp, id, a, "m2", "paused", jobOpts{Deadline: jobDeadlinePast}) // its deadline had passed when it was confirmed: the database only forbids EXPIRING before the deadline
			ledgerExpectOK(t, sp, "control: a job paused from scheduled expires after its deadline", jobMoveSQL, id, "expired", "engine", nil, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAtWith(t, sp, id, a, "m2", "running", jobOpts{Deadline: jobDeadlinePast}) // its deadline has passed too: it is the running that decides
			jobPauseWhileRunning(t, sp, id)
			ledgerExpectRefused(t, sp, "a job paused from running expires", "23001", why("job_transition_illegal"), jobMoveSQL, id, "expired", "engine", nil, "why")
			ledgerExpectOK(t, sp, "control: it can still be failed", jobMoveSQL, id, "failed", "engine", nil, "why")
		})
	})

	t.Run("a scheduled job does not start before its time and nothing expires before its deadline", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "scheduled") // scheduled an hour ahead
			early := why("job_started_early")
			ledgerExpectRefused(t, sp, "scheduled -> running an hour early", "23001", early, jobMoveSQL, id, "running", "engine", nil, "why")
			ledgerMust(t, sp, "UPDATE platform.jobs SET scheduled_start = now() + interval '1 microsecond' WHERE id = $1", id)
			ledgerExpectRefused(t, sp, "scheduled -> running a microsecond early", "23001", early, jobMoveSQL, id, "running", "engine", nil, "why")
			ledgerMust(t, sp, "UPDATE platform.jobs SET scheduled_start = now() WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: scheduled -> running exactly on time", jobMoveSQL, id, "running", "engine", nil, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "scheduled")
			jobMakeDue(t, sp, id)
			ledgerExpectOK(t, sp, "control: scheduled -> running once the time has passed", jobMoveSQL, id, "running", "engine", nil, "why")
			if !ledgerBool(t, sp, "SELECT started_at IS NOT NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a job that started has no started_at")
			}
		})
		// the deadline travels with the move into confirmed or scheduled (no write that leaves the status alone may change it), so each deadline is a job of its own
		for _, from := range []string{"confirmed", "scheduled", "paused"} {
			for _, c := range []struct {
				deadline string
				early    bool
				what     string
			}{
				{jobDeadlineAhead, true, "a day before its deadline"}, {"now() + interval '1 microsecond'", true, "a microsecond before its deadline"}, {"now()", false, "exactly at its deadline"},
			} {
				from, c := from, c
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					id := next()
					jobInsertAtWith(t, sp, id, a, "m2", from, jobOpts{Deadline: c.deadline})
					if c.early {
						ledgerExpectRefused(t, sp, "a "+from+" job expired "+c.what, "23001", why("job_expired_early"), jobMoveSQL, id, "expired", "engine", nil, "why")
						return
					}
					ledgerExpectOK(t, sp, "control: a "+from+" job expired "+c.what, jobMoveSQL, id, "expired", "engine", nil, "why")
				})
			}
		}
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "scheduled")
			// a paused job needs no deadline of its own, and the pause may carry the clearing of it (a move may write the column; a write that leaves the status alone may not): the NULL case of the rule
			ledgerExpectOK(t, sp, "a pause that also clears the deadline", jobAsOnce("user", a, "why", fmt.Sprintf("UPDATE platform.jobs SET status = 'paused', start_deadline = NULL WHERE id = '%s'", id)))
			ledgerExpectRefused(t, sp, "a paused job with no deadline expired (a deadline that is NULL is not a deadline that has passed)", "23001", why("job_expired_early"), jobMoveSQL, id, "expired", "engine", nil, "why")
		})
	})

	t.Run("a job whose cancel was accepted never succeeds, and one whose cancel was not never ends cancelled", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "cancelling")
			if !ledgerBool(t, sp, "SELECT cancel_requested_at IS NOT NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a cancelling job has no cancel_requested_at: the guard did not record the accepted cancel")
			}
			jobWalkTo(t, sp, id, "completing")
			ledgerExpectRefused(t, sp, "completing -> succeeded after an accepted cancel", "23001", why("job_cancelled_cannot_succeed"), jobMoveSQL, id, "succeeded", "engine", nil, "why")
			ledgerExpectOK(t, sp, "control: completing -> cancelled after an accepted cancel", jobMoveSQL, id, "cancelled", "engine", nil, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "m2", "completing")
			if !ledgerBool(t, sp, "SELECT cancel_requested_at IS NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a job that was never cancelled has a cancel_requested_at")
			}
			ledgerExpectRefused(t, sp, "completing -> cancelled with no cancel", "23001", why("job_cancelled_needs_a_cancel"), jobMoveSQL, id, "cancelled", "engine", nil, "why")
			ledgerExpectOK(t, sp, "control: completing -> succeeded with no cancel", jobMoveSQL, id, "succeeded", "engine", nil, "why")
		})
		for _, flavor := range []string{"cancelling", "completing"} {
			flavor := flavor
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := at(sp, "m2", flavor)
				jobWalkTo(t, sp, id, "completing")
				ledgerExpectOK(t, sp, "completing -> failed, whether or not a cancel was accepted (it was reached through "+flavor+")", jobMoveSQL, id, "failed", "engine", nil, "why")
			})
		}
	})

	t.Run("a terminal status never moves, for each of the four", func(t *testing.T) {
		for _, term := range []string{"succeeded", "failed", "cancelled", "expired"} {
			term := term
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := at(sp, "m2", term)
				tried := 0
				for _, to := range jobStates {
					if to == term {
						continue
					}
					ledgerExpectRefused(t, sp, fmt.Sprintf("a %s job moved to %s", term, to), "23001", why("jobs_terminal_is_final"), jobMoveSQL, id, to, "engine", nil, "why")
					tried++
				}
				if tried != 11 {
					t.Fatalf("%d moves tried out of a %s job, want 11", tried, term)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the events
// ---------------------------------------------------------------------------------------------------------------------

// TestJobLifecycle_EveryMoveWritesItsEvent: a job is walked through draft, previewed, confirmed, running, cancelling, completing and cancelled with a different actor and reason each time, and after each move the
// job has exactly one more event and the newest says the right from, to, kind, actor (the user's id, the admin's id, neither for the engine), reason (trimmed) and the database's time (created_at = now()); the
// events chain (each one starts where the one before it ended); the function returns the updated job. A move whose event cannot be written does not happen: an admin who was disabled is refused with 23514
// admin_is_active and an admin nobody knows with 23503 (the foreign key), the job stays where it was, and the same move by an active admin passes; with the trigger that writes the event switched off the
// move happens and writes nothing, which is what proves the event is the trigger's doing. Writing a status back to itself writes none (TestJobLifecycle_Matrix).
// It fails if a move leaves no event, two, or a wrong one, if the actor or the reason is not recorded, or if a move can commit without its event.
func TestJobLifecycle_EveryMoveWritesItsEvent(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, adm := ledgerUserA, ledgerAdminA
	const newest = `SELECT kind || '|' || from_status || '|' || to_status || '|' || actor_kind || '|' || COALESCE(acting_user_id::text, '-') || '|' || COALESCE(acting_admin_id::text, '-') || '|' || reason
		|| '|' || (created_at = now())::text || '|' || detail::text FROM platform.job_events WHERE job_id = $1 ORDER BY id DESC LIMIT 1`

	t.Run("one event per move, with who, why and when", func(t *testing.T) {
		id := ledgerJobA2 // a draft job of user A on module 2
		steps := []struct {
			to, kind string
			actor    any
			reason   string
			tail     string
		}{
			{"previewed", "user", a, "asked for a preview", "user|" + a + "|-|asked for a preview"},
			{"confirmed", "admin", adm, "  confirmed for the customer  ", "admin|-|" + adm + "|confirmed for the customer"},
			{"running", "engine", nil, "the engine submitted the job", "engine|-|-|the engine submitted the job"},
			{"cancelling", "user", a, "changed my mind", "user|" + a + "|-|changed my mind"},
			{"completing", "engine", nil, "the module reports every item final", "engine|-|-|the module reports every item final"},
			{"cancelled", "engine", nil, "every result recorded", "engine|-|-|every result recorded"},
		}
		if n := jobEvents(t, tx, id); n != 0 {
			t.Fatalf("the job has %d event(s) before any move", n)
		}
		prev := "draft"
		for i, s := range steps {
			from := jobStatus(t, tx, id)
			if from != prev {
				t.Fatalf("step %d: the job is %s, want %s", i+1, from, prev)
			}
			jobPrepareHop(t, tx, id, from, s.to)
			var status string
			sql, args := jobMoveStmt(from, s.to, id, s.kind, s.actor, s.reason, jobOpts{})
			if args == nil { // the move into confirmed carries the start deadline in the same statement: one UPDATE under the three settings, which is what the event is written from
				if _, err := tx.Exec(testCtx(t), sql); err != nil {
					t.Fatalf("step %d, %s -> %s: %v", i+1, from, s.to, err)
				}
				status = jobStatus(t, tx, id)
			} else if err := tx.QueryRow(testCtx(t), "SELECT status FROM platform.job_transition($1::uuid, $2::text, $3::text, $4::uuid, $5::text)", args...).Scan(&status); err != nil {
				t.Fatalf("step %d, %s -> %s: %v", i+1, from, s.to, err)
			}
			if status != s.to {
				t.Fatalf("step %d: platform.job_transition returned a job that is %s, want %s", i+1, status, s.to)
			}
			if n := jobEvents(t, tx, id); n != int64(i+1) {
				t.Fatalf("step %d: the job has %d event(s), want %d: a move writes exactly one", i+1, n, i+1)
			}
			want := "transition|" + from + "|" + s.to + "|" + s.tail + "|true|{}"
			if got := ledgerString(t, tx, newest, id); got != want {
				t.Fatalf("step %d: the newest event is\n  %s\nwant\n  %s", i+1, got, want)
			}
			prev = s.to
		}
		if got := ledgerRows(t, tx, "SELECT e.from_status || '>' || e.to_status FROM platform.job_events e WHERE e.job_id = $1 AND e.id <> (SELECT min(id) FROM platform.job_events WHERE job_id = $1) AND e.from_status <> (SELECT p.to_status FROM platform.job_events p WHERE p.job_id = e.job_id AND p.id < e.id ORDER BY p.id DESC LIMIT 1)", id); len(got) != 0 {
			t.Fatalf("the events of the job do not chain (each must start where the one before it ended): %v", got)
		}
	})

	t.Run("a move whose event cannot be written does not happen", func(t *testing.T) {
		const dis = ledgerAdminB
		ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = now() WHERE id = $1", dis)
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := ledgerJobA1
			before := jobEvents(t, sp, id)
			ledgerExpectRefused(t, sp, "a move by an admin who was disabled", "23514", ledgerWhy{Constraint: "admin_is_active", Message: "platform.job_events.acting_admin_id names admin " + dis},
				jobMoveSQL, id, "previewed", "admin", dis, "why")
			ledgerExpectRefused(t, sp, "a move by an admin nobody knows", "23503", ledgerWhy{Constraint: "job_events_acting_admin_id_fkey"}, jobMoveSQL, id, "previewed", "admin", ledgerNoSuchID, "why")
			if got := jobStatus(t, sp, id); got != "draft" || jobEvents(t, sp, id) != before {
				t.Fatalf("a refused move left the job %s with %d event(s), want draft with %d", got, jobEvents(t, sp, id), before)
			}
			ledgerExpectOK(t, sp, "control: the same move by an active admin", jobMoveSQL, id, "previewed", "admin", adm, "why")
			if jobEvents(t, sp, id) != before+1 {
				t.Fatalf("the move of the active admin left %d event(s), want %d", jobEvents(t, sp, id), before+1)
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := ledgerJobA1
			ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_record_transition")
			ledgerExpectOK(t, sp, "with the event writer switched off a move happens", jobMoveSQL, id, "previewed", "user", a, "why")
			if got, n := jobStatus(t, sp, id), jobEvents(t, sp, id); got != "previewed" || n != 0 {
				t.Fatalf("with the event writer off the job is %s with %d event(s), want previewed with none: the event is written by that trigger and by nothing else", got, n)
			}
		})
	})
}

// TestJobLifecycle_TheEventTable: platform.job_events refuses what is not an event, each beside a legal row and with its NULL case (a CHECK that evaluates to NULL passes): a kind outside the vocabulary, a
// transition that does not say from and to (and a note that does), an actor whose ids do not fit its kind (engine with a user, admin with none, user with an admin as well), a user event for another user's job,
// a blank reason, a detail that is not an object (an array, a JSON null), a job that does not exist, a disabled admin and an unknown one; no column of the row may be NULL that the shape needs. The table is
// append-only for everybody (23001 for the superuser and the owner), and platform_app holds nothing but SELECT and INSERT on it and SELECT on platform.job_transitions (42501 for the rest, including an INSERT of a
// new legal move), platform_admin SELECT only.
// It fails if the table accepts a row the design forbids, or a role can change history.
func TestJobLifecycle_TheEventTable(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	job := ledgerJobA1 // a draft job of user A
	const ins = `INSERT INTO platform.job_events (job_id, kind, from_status, to_status, actor_kind, acting_user_id, acting_admin_id, reason, detail) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	legal := []any{job, "note", nil, nil, "engine", nil, nil, "a control", `{}`}
	with := func(kv ...any) []any {
		row := append([]any(nil), legal...)
		for i := 0; i+1 < len(kv); i += 2 {
			row[kv[i].(int)] = kv[i+1]
		}
		return row
	}
	const (
		cJob, cKind, cFrom, cTo, cActor, cUser, cAdmin, cReason, cDetail = 0, 1, 2, 3, 4, 5, 6, 7, 8
	)
	refused := func(what, state string, w ledgerWhy, row []any) {
		t.Helper()
		ledgerExpectRefused(t, tx, what, state, w, ins, row...)
	}
	check := func(constraint string) ledgerWhy { return ledgerWhy{Constraint: constraint} }

	t.Run("the rows of the design are accepted", func(t *testing.T) {
		ledgerExpectOK(t, tx, "a note by the engine", ins, legal...)
		ledgerExpectOK(t, tx, "a transition by the engine", ins, with(cKind, "transition", cFrom, "draft", cTo, "previewed")...)
		ledgerExpectOK(t, tx, "a flag by a user, on their own job", ins, with(cKind, "flag", cActor, "user", cUser, ledgerUserA)...)
		ledgerExpectOK(t, tx, "a stale alert by an active admin", ins, with(cKind, "stale", cActor, "admin", cAdmin, ledgerAdminA)...)
		ledgerExpectOK(t, tx, "a creation event with a detail", ins, with(cKind, "created", cDetail, `{"rows": 3}`)...)
	})
	t.Run("what is not an event is refused for its reason", func(t *testing.T) {
		refused("a kind outside the vocabulary", "23514", check("job_events_kind_vocabulary"), with(cKind, "bogus"))
		refused("a NULL kind", "23502", ledgerWhy{Column: "kind"}, with(cKind, nil))
		refused("a transition with no from_status", "23514", check("job_events_transition_shape"), with(cKind, "transition", cTo, "previewed"))
		refused("a transition with no to_status", "23514", check("job_events_transition_shape"), with(cKind, "transition", cFrom, "draft"))
		refused("a note that carries a from and a to", "23514", check("job_events_transition_shape"), with(cFrom, "draft", cTo, "previewed"))
		refused("a NULL actor_kind", "23502", ledgerWhy{Column: "actor_kind"}, with(cActor, nil))
		refused("an engine event that names a user", "23514", check("job_events_actor_shape"), with(cUser, ledgerUserA))
		refused("an admin event that names no admin", "23514", check("job_events_actor_shape"), with(cActor, "admin"))
		refused("a user event that names an admin as well", "23514", check("job_events_actor_shape"), with(cActor, "user", cUser, ledgerUserA, cAdmin, ledgerAdminA))
		refused("a user event for another user's job", "23514", check("job_event_actor_is_the_owner"), with(cActor, "user", cUser, ledgerUserB))
		refused("a user event that names no user (for a job that exists, the owner check answers first)", "23514", check("job_event_actor_is_the_owner"), with(cActor, "user"))
		refused("a user event that names no user, for a job that does not exist (the CHECK is reached)", "23514", check("job_events_actor_shape"), with(cJob, ledgerNoSuchID, cActor, "user"))
		refused("a NULL reason", "23502", ledgerWhy{Column: "reason"}, with(cReason, nil))
		refused("a blank reason", "23514", check("job_events_reason_not_blank"), with(cReason, "   "))
		refused("a detail that is an array", "23514", check("job_events_detail_is_an_object"), with(cDetail, `[]`))
		refused("a detail that is a JSON null", "23514", check("job_events_detail_is_an_object"), with(cDetail, `null`))
		refused("a SQL NULL detail", "23502", ledgerWhy{Column: "detail"}, with(cDetail, nil))
		refused("a job that does not exist", "23503", check("job_events_job_id_fkey"), with(cJob, ledgerNoSuchID))
		refused("a NULL job", "23502", ledgerWhy{Column: "job_id"}, with(cJob, nil))
		refused("an admin nobody knows", "23503", check("job_events_acting_admin_id_fkey"), with(cActor, "admin", cAdmin, ledgerNoSuchID))
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.job_events DROP CONSTRAINT job_events_actor_shape") // the shape CHECK answers first (it sorts first): without it the vocabulary is the wall
			ledgerExpectRefused(t, sp, "an actor kind that is none (the shape CHECK switched off)", "23514", check("job_events_actor_vocabulary"), ins, with(cActor, "robot")...)
		})
		ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = now() WHERE id = $1", ledgerAdminB)
		refused("an admin who was disabled", "23514", ledgerWhy{Constraint: "admin_is_active", Message: "platform.job_events.acting_admin_id names admin " + ledgerAdminB}, with(cActor, "admin", cAdmin, ledgerAdminB))
	})

	t.Run("append-only for everybody; the roles hold what the design gives them", func(t *testing.T) {
		jobWalkTo(t, tx, ledgerJobA1, "previewed") // an event to guard, whatever the rows above did
		type stmt struct{ rel, sql string }
		mutations := []stmt{
			{"job_events", "UPDATE platform.job_events SET reason = 'edited'"}, {"job_events", "DELETE FROM platform.job_events"}, {"job_events", "TRUNCATE platform.job_events"},
			{"job_transitions", "UPDATE platform.job_transitions SET note = 'edited'"}, {"job_transitions", "DELETE FROM platform.job_transitions"}, {"job_transitions", "TRUNCATE platform.job_transitions"},
		}
		for _, role := range []string{"", "platform_owner"} { // the superuser, then the table's owner: the privileges are theirs and the triggers still refuse
			if role != "" {
				ledgerSwitchRole(t, tx, role)
			}
			for _, m := range mutations {
				ledgerExpectRefused(t, tx, fmt.Sprintf("%q: %s", role, m.sql), "23001", ledgerWhy{Message: "append-only"}, m.sql)
			}
			if role != "" {
				ledgerResetRole(t, tx)
			}
		}
		const newMove = "INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES ('previewed', 'running', ARRAY['engine'], 'a control')"
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerSwitchRole(t, sp, "platform_owner")
			ledgerExpectOK(t, sp, "control: the owner inserts a new legal move (the table is insert-only, and a migration is its writer)", newMove)
		})
		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			for _, m := range mutations {
				ledgerExpectRefused(t, tx, fmt.Sprintf("%s: %s", role, m.sql), "42501", ledgerWhy{Message: "permission denied for table " + m.rel}, m.sql)
			}
			for _, rel := range []string{"job_events", "job_transitions"} {
				if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform."+rel); n < 1 {
					t.Fatalf("%s reads %d rows of %s", role, n, rel)
				}
			}
			ledgerResetRole(t, tx)
		}
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectRefused(t, tx, "platform_app inserts a legal move of its own", "42501", ledgerWhy{Message: "permission denied for table job_transitions"}, newMove)
		ledgerExpectOK(t, tx, "control: platform_app inserts an event", ins, legal...)
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		ledgerExpectRefused(t, tx, "platform_admin inserts an event", "42501", ledgerWhy{Message: "permission denied for table job_events"}, ins, legal...)
		ledgerResetRole(t, tx)
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the shape of a confirmed job
// ---------------------------------------------------------------------------------------------------------------------

// TestJobLifecycle_TheShapeOfAConfirmedJob: the CHECKs of platform_0011, each refused for ITS name beside a legal twin and with its NULL case (a CHECK that evaluates to NULL passes, so each is built from IS NOT
// NULL): a job that leaves previewed for confirmed or scheduled has a preview, its fingerprint and a confirm key (jobs_confirmed_shape: each one missing in turn; confirmed_at is the guard's, so its absence is
// shown with the guard off) and a job in any of the six states that hold the slot cannot lose one; a confirmed or scheduled job has a start deadline (jobs_start_needs_a_deadline) and a running one does not need it;
// a confirmed job has no start time (jobs_confirmed_starts_now); a job says it paused from scheduled or running exactly when it is paused (jobs_paused_from_shape, jobs_paused_from_vocabulary); the confirm
// key, the fingerprint and the preview have their shapes (jobs_confirm_key_shape: 1 to 128 characters of A-Za-z0-9._:-, 128 accepted and 129 not, a newline not; jobs_fingerprint_shape: 64 lower-case hex digits;
// jobs_preview_is_an_object: an object, not an array, a string, a number or a JSON null) and the confirm key is unique per user (jobs_confirm_key_key) and not across users, with no two NULLs in conflict.
// Since the lifecycle guard refuses any write that changes paused_from, confirmed_at, started_at, finished_at, cancel_requested_at or start_deadline without a change of status, a CHECK that such a write
// would meet is reached only with jobs_transition_guard switched off by the owner (a savepoint that is rolled back: the two-walls pattern of TestCloseout_Terminal*IsFinal), and each of those tests is paired with
// the same write with the guard on, refused 23001 job_lifecycle_columns_move_with_a_transition.
// It fails if a constraint is missing, weaker than its design, or lets a NULL through.
func TestJobLifecycle_TheShapeOfAConfirmedJob(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("9c")
	a, b := ledgerUserA, ledgerUserB
	const tbl = "platform.jobs"
	check := func(c string) ledgerWhy { return ledgerWhy{Constraint: c} }
	moves := check("job_lifecycle_columns_move_with_a_transition")
	guardOff := func(sp pgx.Tx) {
		t.Helper()
		ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
	}
	// a job that is previewed and has nothing else: the lifecycle lets a job be previewed bare, and it is the move to confirmed that wants the rest
	bare := func(sp pgx.Tx) string {
		id := next()
		ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", id, a)
		ledgerMust(t, sp, jobMoveSQL, id, "previewed", "user", a, "bare")
		return id
	}
	// a job that is previewed with its preview, fingerprint and confirm key, and no start deadline (the deadline travels with the move into confirmed or scheduled)
	filled := func(sp pgx.Tx) string {
		id := next()
		jobInsertAt(t, sp, id, a, "m2", "previewed")
		return id
	}

	t.Run("a job that leaves previewed carries a preview, a fingerprint and a confirm key (jobs_confirmed_shape)", func(t *testing.T) {
		columns := []struct{ name, set string }{
			{"preview", "preview = '{}'::jsonb"}, {"preview_fingerprint", "preview_fingerprint = repeat('a', 64)"}, {"confirm_key", "confirm_key = 'k-' || id::text"},
		}
		refusals := 0
		for _, door := range []string{"confirmed", "scheduled"} {
			for skip := -1; skip < len(columns); skip++ {
				door, skip := door, skip
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					id := bare(sp)
					for i, c := range columns {
						if i != skip {
							ledgerMust(t, sp, "UPDATE platform.jobs SET "+c.set+" WHERE id = $1", id)
						}
					}
					if door == "scheduled" {
						ledgerMust(t, sp, "UPDATE platform.jobs SET scheduled_start = now() + interval '1 hour' WHERE id = $1", id)
					}
					sql, args := jobMoveStmt("previewed", door, id, "user", a, "why", jobOpts{})
					if skip < 0 {
						ledgerExpectOK(t, sp, "control: previewed -> "+door+" with all three", sql, args...)
						return
					}
					refusals++
					ledgerExpectRefused(t, sp, fmt.Sprintf("previewed -> %s with no %s", door, columns[skip].name), "23514", check("jobs_confirmed_shape"), sql, args...)
				})
			}
		}
		if refusals != 6 {
			t.Fatalf("%d refusals tried, want 6 (three columns, two doors)", refusals)
		}
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := filled(sp)
			guardOff(sp) // confirmed_at is the guard's: without it the CHECK is what notices
			const move = "UPDATE platform.jobs SET status = 'confirmed', start_deadline = now() + interval '1 day'%s WHERE id = '%s'"
			ledgerExpectRefused(t, sp, "confirmed with no confirmed_at", "23514", check("jobs_confirmed_shape"), jobAsOnce("user", a, "why", fmt.Sprintf(move, "", id)))
			ledgerExpectOK(t, sp, "control: the same move that states a confirmed_at", jobAsOnce("user", a, "why", fmt.Sprintf(move, ", confirmed_at = now()", id)))
		})
		// a job in a state that holds the slot cannot lose one of the four. With the guards on the write is refused before the CHECK is reached (the frozen-columns guard for the preview, the fingerprint and the
		// key; the lifecycle guard for confirmed_at); with both switched off the CHECK is the wall
		cleared := 0
		for _, s := range []string{"confirmed", "scheduled", "running", "paused", "cancelling", "completing"} {
			for _, col := range []string{"preview", "preview_fingerprint", "confirm_key", "confirmed_at"} {
				s, col := s, col
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					id := next()
					jobInsertAt(t, sp, id, a, "m2", s)
					wipe := "UPDATE platform.jobs SET " + col + " = NULL WHERE id = $1"
					first := ledgerWhy{Message: "frozen"}
					if col == "confirmed_at" {
						first = moves
					}
					ledgerExpectRefused(t, sp, fmt.Sprintf("a %s job with its %s cleared, the guards on", s, col), "23001", first, wipe, id)
					ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_guard")
					guardOff(sp)
					ledgerExpectRefused(t, sp, fmt.Sprintf("a %s job with its %s cleared, the guards off", s, col), "23514", check("jobs_confirmed_shape"), wipe, id)
				})
				cleared++
			}
		}
		if cleared != 24 {
			t.Fatalf("%d clearings tried, want 24 (six states, four columns)", cleared)
		}
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := filled(sp)
			ledgerExpectOK(t, sp, "control: a previewed job may have no preview (the same statement, a status that does not need one)", "UPDATE platform.jobs SET preview = NULL WHERE id = $1", id)
		})
	})

	t.Run("a confirmed or scheduled job has a start deadline (jobs_start_needs_a_deadline)", func(t *testing.T) {
		for _, door := range []string{"confirmed", "scheduled"} {
			door := door
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := filled(sp)
				if door == "scheduled" {
					ledgerMust(t, sp, "UPDATE platform.jobs SET scheduled_start = now() + interval '1 hour' WHERE id = $1", id)
				}
				ledgerExpectRefused(t, sp, "previewed -> "+door+" with no start deadline", "23514", check("jobs_start_needs_a_deadline"), jobMoveSQL, id, door, "user", a, "why")
				sql, args := jobMoveStmt("previewed", door, id, "user", a, "why", jobOpts{})
				ledgerExpectOK(t, sp, "control: previewed -> "+door+" with one", sql, args...)
			})
		}
		// the rule is about the start: a running job may have no deadline - but only a MOVE may clear it, so the CHECK's scope is shown with the guard off and the write with it on is refused
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "running")
			ledgerExpectRefused(t, sp, "a running job's start deadline cleared by a write that leaves the status alone", "23001", moves, "UPDATE platform.jobs SET start_deadline = NULL WHERE id = $1", id)
			guardOff(sp)
			ledgerExpectOK(t, sp, "control: with the guard off, a running job needs no deadline (the CHECK is about the start)", "UPDATE platform.jobs SET start_deadline = NULL WHERE id = $1", id)
		})
	})

	t.Run("a confirmed job has no start time of its own (jobs_confirmed_starts_now)", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := filled(sp)
			ledgerMust(t, sp, "UPDATE platform.jobs SET scheduled_start = now() + interval '1 hour' WHERE id = $1", id)
			sql, args := jobMoveStmt("previewed", "confirmed", id, "user", a, "why", jobOpts{})
			ledgerExpectRefused(t, sp, "previewed -> confirmed with a start time", "23514", check("jobs_confirmed_starts_now"), sql, args...)
			sql, args = jobMoveStmt("previewed", "scheduled", id, "user", a, "why", jobOpts{})
			ledgerExpectOK(t, sp, "control: previewed -> scheduled with the same start time", sql, args...)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "confirmed")
			ledgerExpectRefused(t, sp, "a confirmed job given a start time", "23514", check("jobs_confirmed_starts_now"), "UPDATE platform.jobs SET scheduled_start = now() WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: a confirmed job's start time left NULL (written back)", "UPDATE platform.jobs SET scheduled_start = NULL WHERE id = $1", id)
		})
	})

	t.Run("a job says where it paused from exactly when it is paused (jobs_paused_from_shape, jobs_paused_from_vocabulary)", func(t *testing.T) {
		// paused_from is the guard's at a move and changes only with one: a write that leaves the status alone meets the lifecycle guard first, so the CHECKs are reached with it off
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "running")
			ledgerExpectRefused(t, sp, "a running job that says it paused from scheduled, the guard on", "23001", moves, "UPDATE platform.jobs SET paused_from = 'scheduled' WHERE id = $1", id)
			guardOff(sp)
			ledgerExpectRefused(t, sp, "a running job that says it paused from scheduled", "23514", check("jobs_paused_from_shape"), "UPDATE platform.jobs SET paused_from = 'scheduled' WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: a running job that says nothing", "UPDATE platform.jobs SET paused_from = NULL WHERE id = $1", id)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "paused")
			ledgerExpectOK(t, sp, "control: a paused job's paused_from written back is no change, the guard on", "UPDATE platform.jobs SET paused_from = paused_from WHERE id = $1", id)
			ledgerExpectRefused(t, sp, "a paused job that says nothing, the guard on", "23001", moves, "UPDATE platform.jobs SET paused_from = NULL WHERE id = $1", id)
			ledgerExpectRefused(t, sp, "a paused job that paused from draft, the guard on", "23001", moves, "UPDATE platform.jobs SET paused_from = 'draft' WHERE id = $1", id)
			guardOff(sp)
			ledgerExpectRefused(t, sp, "a paused job that says nothing (the NULL case)", "23514", check("jobs_paused_from_shape"), "UPDATE platform.jobs SET paused_from = NULL WHERE id = $1", id)
			ledgerExpectRefused(t, sp, "a paused job that paused from draft", "23514", check("jobs_paused_from_vocabulary"), "UPDATE platform.jobs SET paused_from = 'draft' WHERE id = $1", id)
			ledgerExpectOK(t, sp, "control: a paused job's paused_from written back", "UPDATE platform.jobs SET paused_from = paused_from WHERE id = $1", id)
		})
	})

	t.Run("the confirm key, the fingerprint and the preview have their shapes", func(t *testing.T) {
		row := func(user, module string, kv ...any) ledgerCols {
			return ledgerCols{"id": next(), "user_id": user, "module_id": module}.with(kv...)
		}
		ledgerOKInsert(t, tx, "control: a draft job with none of the three (NULL passes: a draft has no key, no preview and no fingerprint yet)", tbl, row(a, "m2"))
		for _, k := range []string{"a", strings.Repeat("a", 128), "AZaz09._:-"} {
			ledgerOKInsert(t, tx, fmt.Sprintf("control: a confirm key of %d characters", len(k)), tbl, row(a, "m2", "confirm_key", k))
		}
		for _, c := range []struct{ what, key string }{
			{"empty", ""}, {"of 129 characters", strings.Repeat("a", 129)}, {"with a space", "has space"}, {"with a slash", "slash/slash"}, {"with a newline", "new\nline"},
			{"with a tab", "tab\tkey"}, {"with a semicolon", "semi;colon"}, {"with a non-ASCII letter", "unicodé"},
		} {
			ledgerRefusedInsert(t, tx, "a confirm key "+c.what, "23514", check("jobs_confirm_key_shape"), tbl, row(a, "m2", "confirm_key", c.key))
		}
		ledgerOKInsert(t, tx, "control: a fingerprint of 64 lower-case hex digits", tbl, row(a, "m2", "preview_fingerprint", strings.Repeat("0123456789abcdef", 4)))
		for _, c := range []struct{ what, fp string }{
			{"of 63 digits", strings.Repeat("a", 63)}, {"of 65 digits", strings.Repeat("a", 65)}, {"in upper case", strings.Repeat("A", 64)}, {"that is not hex", strings.Repeat("g", 64)},
			{"with a trailing newline", strings.Repeat("a", 64) + "\n"}, {"that is empty", ""},
		} {
			ledgerRefusedInsert(t, tx, "a fingerprint "+c.what, "23514", check("jobs_fingerprint_shape"), tbl, row(a, "m2", "preview_fingerprint", c.fp))
		}
		for _, p := range []string{`{}`, `{"a":[1,2,{"b":null}]}`} {
			ledgerOKInsert(t, tx, "control: the preview "+p, tbl, row(a, "m2", "preview", p))
		}
		for _, p := range []string{`[]`, `[{}]`, `"x"`, `1`, `true`, `null`} {
			ledgerRefusedInsert(t, tx, "the preview "+p+" (not an object)", "23514", check("jobs_preview_is_an_object"), tbl, row(a, "m2", "preview", p))
		}
	})

	t.Run("a confirm key is unique per user, not across users, and NULLs do not collide (jobs_confirm_key_key)", func(t *testing.T) {
		row := func(user, module string, kv ...any) ledgerCols {
			return ledgerCols{"id": next(), "user_id": user, "module_id": module}.with(kv...)
		}
		ledgerOKInsert(t, tx, "control: a job with a key", tbl, row(a, "m2", "confirm_key", "the-key"))
		ledgerRefusedInsert(t, tx, "the same key on another job of the same user", "23505", check("jobs_confirm_key_key"), tbl, row(a, "m2", "confirm_key", "the-key"))
		ledgerRefusedInsert(t, tx, "the same key on a job of the same user on another module (the key is the user's)", "23505", check("jobs_confirm_key_key"), tbl, row(a, "m1", "confirm_key", "the-key"))
		ledgerOKInsert(t, tx, "control: the same key for ANOTHER user", tbl, row(b, "m2", "confirm_key", "the-key"))
		ledgerOKInsert(t, tx, "control: a job with no key", tbl, row(a, "m2"))
		ledgerOKInsert(t, tx, "control: a second job with no key (two NULLs are not equal)", tbl, row(a, "m2"))
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// the clock, the frozen columns, the privileges, the slot, the settings
// ---------------------------------------------------------------------------------------------------------------------

// TestJobLifecycle_TheClockIsTheDatabases: the times of a job are the database's, read back as now() of the transaction. A writer who states a time of his own in the same statement as the move gets the database's
// instead for confirmed_at (when a job leaves previewed, for both doors), started_at (a job that starts is started now), finished_at (each of the four terminal statuses) and cancel_requested_at (a cancel is
// accepted now); last_progress_at is set when a job starts; a time nobody stated is the database's (confirmed_at of a move with no time in it); a job that has not finished has no finished_at. That the times
// cannot be written WITHOUT a move is TestJobLifecycle_LifecycleColumnsMoveWithATransition, and that a pause and a resume keep confirmed_at is TestJobLifecycle_SettingsAreTransactionLocal (it needs
// transactions of its own: inside one, now() is the same for the confirm and the resume).
// It fails if a writer can choose when a job was confirmed, started, finished or cancelled.
func TestJobLifecycle_TheClockIsTheDatabases(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("9d")
	a := ledgerUserA
	const absurd = "2999-01-01T00:00:00Z"
	at := func(sp pgx.Tx, status string, o jobOpts) string {
		id := next()
		jobInsertAtWith(t, sp, id, a, "m2", status, o)
		return id
	}
	isNow := func(sp pgx.Tx, id, col string) bool {
		return ledgerBool(t, sp, "SELECT COALESCE("+col+" = now(), false) FROM platform.jobs WHERE id = $1", id)
	}
	// moveWith is a move the way a writer who also states a time would write it: the three settings by hand and ONE UPDATE that carries the time
	moveWith := func(sp pgx.Tx, kind, actor, id, to, extra string) {
		t.Helper()
		ledgerExpectOK(t, sp, "the move to "+to+" that also states "+extra, jobAsOnce(kind, actor, "the clock", fmt.Sprintf("UPDATE platform.jobs SET status = '%s', %s WHERE id = '%s'", to, extra, id)))
	}

	t.Run("a time nobody stated is the database's", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "previewed", jobOpts{})
			if isNow(sp, id, "confirmed_at") {
				t.Fatal("a previewed job has a confirmed_at")
			}
			sql, args := jobMoveStmt("previewed", "confirmed", id, "user", a, "why", jobOpts{})
			ledgerExpectOK(t, sp, "previewed -> confirmed", sql, args...)
			if !isNow(sp, id, "confirmed_at") {
				t.Fatal("a confirmed job's confirmed_at is not the transaction's now()")
			}
			if ledgerBool(t, sp, "SELECT finished_at IS NOT NULL OR started_at IS NOT NULL OR cancel_requested_at IS NOT NULL FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("a confirmed job has a started_at, a finished_at or a cancel_requested_at")
			}
		})
	})
	t.Run("a started_at the writer states is overwritten, and the job's last progress is now", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "confirmed", jobOpts{})
			moveWith(sp, "engine", "", id, "running", "started_at = '"+absurd+"', last_progress_at = '"+absurd+"'")
			for _, col := range []string{"started_at", "last_progress_at"} {
				if !isNow(sp, id, col) {
					t.Fatalf("a job that started has a %s that is not the transaction's now()", col)
				}
			}
		})
	})
	t.Run("a finished_at the writer states is overwritten, for each of the four terminal statuses", func(t *testing.T) {
		for _, c := range []struct {
			from, to, kind, actor string
			o                     jobOpts
		}{
			{"completing", "succeeded", "engine", "", jobOpts{}}, {"confirmed", "failed", "engine", "", jobOpts{}}, {"draft", "cancelled", "user", a, jobOpts{}},
			{"confirmed", "expired", "engine", "", jobOpts{Deadline: jobDeadlinePast}},
		} {
			c := c
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := at(sp, c.from, c.o)
				moveWith(sp, c.kind, c.actor, id, c.to, "finished_at = '"+absurd+"'")
				if !isNow(sp, id, "finished_at") {
					t.Fatalf("a job that %s has a finished_at that is not the transaction's now()", c.to)
				}
			})
		}
	})
	t.Run("a cancel_requested_at the writer states is overwritten", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := at(sp, "running", jobOpts{})
			moveWith(sp, "user", a, id, "cancelling", "cancel_requested_at = '"+absurd+"'")
			if !isNow(sp, id, "cancel_requested_at") {
				t.Fatal("a cancel was accepted at a time the writer chose")
			}
		})
	})
	// platform_0011's header: the times are set by the guard from now() at the move, never the writer's value. The move out of previewed carries the deadline in the same statement (a write that leaves the
	// status alone may not set it), and here also the confirmed_at the writer would like.
	t.Run("a confirmed_at the writer states in the move is overwritten (the times are the database's)", func(t *testing.T) {
		for _, door := range []string{"confirmed", "scheduled"} {
			door := door
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := at(sp, "previewed", jobOpts{})
				if door == "scheduled" {
					ledgerMust(t, sp, "UPDATE platform.jobs SET scheduled_start = now() + interval '1 hour' WHERE id = $1", id)
				}
				moveWith(sp, "user", a, id, door, "start_deadline = now() + interval '1 day', confirmed_at = '"+absurd+"'")
				if !isNow(sp, id, "confirmed_at") {
					t.Fatalf("a job confirmed (%s) with a confirmed_at the writer chose: %s", door, ledgerString(t, sp, "SELECT confirmed_at::text FROM platform.jobs WHERE id = $1", id))
				}
			})
		}
	})
}

// TestJobLifecycle_LifecycleColumnsMoveWithATransition: paused_from, cancel_requested_at, confirmed_at, started_at, finished_at and start_deadline carry the lifecycle's own facts, and a write that LEAVES THE STATUS
// ALONE may not change them (23001, constraint job_lifecycle_columns_move_with_a_transition): for each of the six, on a job in a status where the write is otherwise valid, the write is refused for the owner
// of the table's data role (platform_app) as for the superuser; writing the SAME value back is no change (the control of each refusal); with the lifecycle guard switched off by the owner (a savepoint that is
// rolled back) the same write is accepted, so the guard is the wall; and module_job_ref, last_progress_at, stale_alerted_at and scheduled_start, and the preview columns while a job is draft or previewed, stay
// writable. The route this closes: clearing cancel_requested_at on a cancelling job so that completing -> succeeded is accepted (job_cancelled_cannot_succeed): with the guard off the route exists (the control),
// with it on the clearing is refused and the job still cannot succeed.
// It fails if one of the six can be written on its own, or if the guard refuses a column it should leave free.
func TestJobLifecycle_LifecycleColumnsMoveWithATransition(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("8a")
	a := ledgerUserA
	moves := ledgerWhy{Constraint: "job_lifecycle_columns_move_with_a_transition"}
	guardOff := func(sp pgx.Tx) { ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard") }

	t.Run("each of the six is refused on a write that leaves the status alone", func(t *testing.T) {
		cases := []struct{ col, status, set string }{
			{"paused_from", "paused", "paused_from = 'running'"},
			{"cancel_requested_at", "cancelling", "cancel_requested_at = NULL"}, // cleared
			{"cancel_requested_at", "running", "cancel_requested_at = now()"},   // set
			{"confirmed_at", "running", "confirmed_at = '2020-01-01T00:00:00Z'"},
			{"started_at", "confirmed", "started_at = now()"}, // NULL to a value
			{"started_at", "running", "started_at = '2020-01-01T00:00:00Z'"},
			{"finished_at", "running", "finished_at = now()"},
			{"finished_at", "succeeded", "finished_at = NULL"},
			{"start_deadline", "confirmed", "start_deadline = now() + interval '2 days'"},
			{"start_deadline", "running", "start_deadline = NULL"},
		}
		seen := map[string]bool{}
		for _, c := range cases {
			c := c
			seen[c.col] = true
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, "m2", c.status)
				write := "UPDATE platform.jobs SET " + c.set + " WHERE id = $1"
				same := "UPDATE platform.jobs SET " + c.col + " = " + c.col + " WHERE id = $1"
				what := fmt.Sprintf("%s of a %s job (%s)", c.col, c.status, c.set)
				ledgerExpectOK(t, sp, "control: the same "+what+" written back is no change", same, id)
				ledgerExpectRefused(t, sp, what, "23001", moves, write, id)
				ledgerTrial(t, sp, func(sp2 pgx.Tx) {
					ledgerSwitchRole(t, sp2, "platform_app")
					ledgerExpectRefused(t, sp2, "platform_app: "+what, "23001", moves, write, id)
					ledgerExpectOK(t, sp2, "control: platform_app writes the same value back", same, id)
				})
				stored := func() string {
					return ledgerString(t, sp, "SELECT COALESCE("+c.col+"::text, '<null>') FROM platform.jobs WHERE id = $1", id)
				}
				before := stored()
				guardOff(sp)
				ledgerExpectOK(t, sp, "control: "+what+", the lifecycle guard switched off (so it is the guard that refuses)", write, id)
				if after := stored(); after == before { // a write of the value the column already holds is no change, and no change is never refused: the case would prove nothing
					t.Fatalf("%s: the write left %s as it was (%s): the case must write a value that DIFFERS from the stored one", what, c.col, before)
				}
			})
		}
		if len(seen) != 6 {
			t.Fatalf("%d of the six columns were tried, want 6", len(seen))
		}
	})

	t.Run("the columns that stay free", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "running")
			for _, set := range []string{"module_job_ref = 'ref-1'", "last_progress_at = '2020-01-01T00:00:00Z'", "stale_alerted_at = now()", "scheduled_start = now() + interval '1 day'"} {
				ledgerExpectOK(t, sp, "a running job: "+set, "UPDATE platform.jobs SET "+set+" WHERE id = $1", id)
			}
			ledgerExpectOK(t, sp, "control: several free columns in one write", "UPDATE platform.jobs SET module_job_ref = 'ref-2', stale_alerted_at = NULL WHERE id = $1", id)
			ledgerExpectRefused(t, sp, "a free column beside a protected one in one write", "23001", moves, "UPDATE platform.jobs SET module_job_ref = 'ref-3', started_at = now() + interval '1 hour' WHERE id = $1", id)
		})
		for _, status := range []string{"draft", "previewed"} {
			status := status
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, "m2", status)
				ledgerExpectOK(t, sp, "a "+status+" job: its preview columns and settings", `UPDATE platform.jobs SET preview = '{"x":1}', preview_fingerprint = repeat('b', 64), confirm_key = 'changed', frozen_settings = '{"y":2}' WHERE id = $1`, id)
			})
		}
	})

	t.Run("clearing cancel_requested_at to let a cancelled job succeed", func(t *testing.T) {
		const clearIt = "UPDATE platform.jobs SET cancel_requested_at = NULL WHERE id = $1"
		ledgerTrial(t, tx, func(sp pgx.Tx) { // the route exists without the guard: this is what the guard closes
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "cancelling")
			jobWalkTo(t, sp, id, "completing")
			ledgerExpectRefused(t, sp, "control: a cancelled job cannot succeed", "23001", ledgerWhy{Constraint: "job_cancelled_cannot_succeed"}, jobMoveSQL, id, "succeeded", "engine", nil, "why")
			guardOff(sp)
			ledgerExpectOK(t, sp, "with the guard off the cancel mark can be cleared", clearIt, id)
			ledgerMust(t, sp, "ALTER TABLE platform.jobs ENABLE TRIGGER jobs_transition_guard")
			ledgerExpectOK(t, sp, "and then the cancelled job succeeds: the route", jobMoveSQL, id, "succeeded", "engine", nil, "why")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "cancelling")
			ledgerExpectRefused(t, sp, "clearing the cancel mark of a cancelling job", "23001", moves, clearIt, id)
			jobWalkTo(t, sp, id, "completing")
			ledgerExpectRefused(t, sp, "clearing the cancel mark of a completing job", "23001", moves, clearIt, id)
			ledgerExpectRefused(t, sp, "completing -> succeeded after the refused clearing", "23001", ledgerWhy{Constraint: "job_cancelled_cannot_succeed"}, jobMoveSQL, id, "succeeded", "engine", nil, "why")
			ledgerExpectOK(t, sp, "control: completing -> cancelled", jobMoveSQL, id, "cancelled", "engine", nil, "why")
		})
	})
}

// TestJobLifecycle_FrozenColumns: frozen_settings, the preview, its fingerprint and the confirm key may change while the job is a draft or previewed and not after: for each of the twelve statuses and each of
// the four columns a changed value is accepted in draft and previewed and refused 23001 ("frozen") in the ten others; writing the same value back is never a change (the control of every refusal); the immutable
// columns of the older guard (user, module, source, creation time) still never change.
// It fails if a column of the job's contract can change after the confirm, or is frozen too early to be filled in.
func TestJobLifecycle_FrozenColumns(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("8b")
	a := ledgerUserA
	cols := []struct{ name, value string }{
		{"frozen_settings", `{"b":2}`}, {"preview", `{"x":1}`}, {"preview_fingerprint", strings.Repeat("b", 64)}, {"confirm_key", "changed"},
	}
	editable := map[string]bool{"draft": true, "previewed": true}
	cells := 0
	for _, s := range jobStates {
		for _, c := range cols {
			s, c := s, c
			cells++
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				jobInsertAt(t, sp, id, a, "m2", s)
				ledgerExpectOK(t, sp, fmt.Sprintf("control: the same %s written back to a %s job is no change", c.name, s), fmt.Sprintf("UPDATE platform.jobs SET %s = %s WHERE id = $1", c.name, c.name), id)
				set := fmt.Sprintf("UPDATE platform.jobs SET %s = $2 WHERE id = $1", c.name)
				if editable[s] {
					ledgerExpectOK(t, sp, fmt.Sprintf("%s of a %s job", c.name, s), set, id, c.value)
				} else {
					ledgerExpectRefused(t, sp, fmt.Sprintf("%s of a %s job", c.name, s), "23001", ledgerWhy{Message: "frozen"}, set, id, c.value)
				}
			})
		}
	}
	if cells != 48 {
		t.Fatalf("%d cells tried, want 48 (twelve statuses, four columns)", cells)
	}
	t.Run("the immutable columns of the older guard still never change", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			jobInsertAt(t, sp, id, a, "m2", "previewed")
			why := ledgerWhy{Message: "never change"}
			ledgerExpectRefused(t, sp, "changing user_id of a previewed job", "23001", why, "UPDATE platform.jobs SET user_id = $2 WHERE id = $1", id, ledgerUserB)
			ledgerExpectRefused(t, sp, "changing module_id of a previewed job", "23001", why, "UPDATE platform.jobs SET module_id = 'm1' WHERE id = $1", id)
			ledgerExpectRefused(t, sp, "changing created_at of a previewed job", "23001", why, "UPDATE platform.jobs SET created_at = created_at + interval '1 day' WHERE id = $1", id)
		})
	})
}

// TestJobLifecycle_ThePrivilegesOfTheLifecycle: the sanctioned move is a function platform_app may EXECUTE and nobody else (platform_admin, a role that holds nothing and PUBLIC are refused with 42501, the
// superuser is the control); as platform_app a whole life (draft, previewed, confirmed, running, completing, succeeded) is walked through it with the privileges the service really has, one event for each move;
// the move the long way (the three settings stated by hand, then one's own UPDATE) is accepted for platform_app and a bare UPDATE of the status is refused 23001, whoever the role is; platform_admin, which holds
// no UPDATE on platform.jobs, is refused the bare statement by privilege.
// It fails if a role other than the service can move a job, if the service cannot, or if the move can skip its actor and its reason.
func TestJobLifecycle_ThePrivilegesOfTheLifecycle(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("9f")
	a := ledgerUserA
	const fn = "platform.job_transition(uuid, text, text, uuid, text)"

	for role, want := range map[string]bool{"platform_app": true, "platform_admin": false} { // PUBLIC is judged by TestGrantsInvariant_Functions (no function may be executable by PUBLIC)
		if got := ledgerBool(t, tx, "SELECT has_function_privilege($1::name, $2::text, 'EXECUTE')", role, fn); got != want {
			t.Fatalf("EXECUTE on %s for %s is %v, want %v", fn, role, got, want)
		}
	}
	denied := ledgerWhy{Message: "permission denied for function job_transition"}
	id := ledgerJobA1 // a draft job of user A
	ledgerSwitchRole(t, tx, "platform_admin")
	ledgerExpectRefused(t, tx, "platform_admin moves a job", "42501", denied, jobMoveSQL, id, "previewed", "admin", ledgerAdminA, "why")
	ledgerExpectRefused(t, tx, "platform_admin moves a job the long way", "42501", ledgerWhy{Message: "permission denied for table jobs"}, "UPDATE platform.jobs SET status = 'previewed' WHERE id = $1", id)
	ledgerResetRole(t, tx)
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerMust(t, sp, "CREATE ROLE zz_lc_holds_nothing NOLOGIN")
		ledgerMust(t, sp, "GRANT USAGE ON SCHEMA platform TO zz_lc_holds_nothing")
		ledgerSwitchRole(t, sp, "zz_lc_holds_nothing")
		ledgerExpectRefused(t, sp, "a role that holds nothing moves a job", "42501", denied, jobMoveSQL, id, "previewed", "user", a, "why")
		ledgerResetRole(t, sp)
	})
	ledgerExpectOK(t, tx, "control: the superuser may call it (so the refusals above are the privilege's)", jobMoveSQL, id, "previewed", "user", a, "why")

	t.Run("platform_app walks a whole life through the function, one event for each move", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		job := next()
		ledgerExpectOK(t, tx, "platform_app inserts a draft job", "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", job, a)
		jobWalkTo(t, tx, job, "succeeded")
		if n := jobEvents(t, tx, job); n != 5 {
			t.Fatalf("a life of five moves (previewed, confirmed, running, completing, succeeded) wrote %d event(s) for platform_app, want 5", n)
		}
		ledgerResetRole(t, tx)
	})

	t.Run("the move the long way: three settings and one's own UPDATE, accepted; the bare UPDATE, refused", func(t *testing.T) {
		job := next()
		jobInsertAt(t, tx, job, a, "m2", "confirmed")
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectRefused(t, tx, "platform_app writes the status with no setting", "23001", ledgerWhy{Constraint: "job_transition_needs_an_actor"}, "UPDATE platform.jobs SET status = 'running' WHERE id = $1", job)
		ledgerExpectOK(t, tx, "control: platform_app states the three settings and writes the status itself", jobMoveBy("engine", "", "stated by hand", job, "running"))
		ledgerResetRole(t, tx)
		if got := ledgerString(t, tx, "SELECT actor_kind || '|' || reason FROM platform.job_events WHERE job_id = $1 ORDER BY id DESC LIMIT 1", job); got != "engine|stated by hand" {
			t.Fatalf("the event of the move stated by hand is %q, want engine|stated by hand", got)
		}
	})
}

// TestJobLifecycle_TheSlotThroughALife: one job is walked through eight lives (it runs and succeeds; it is cancelled while it runs; it is scheduled, paused, resumed and fails; it is paused WHILE RUNNING and then
// cancelled through the engine, or completed by its module, or resumed and failed (platform_0016); it is confirmed and expires; it is previewed, sent back to draft, previewed again and cancelled) and at every step a SECOND job of the same user and module that is previewed (and holds nothing) is tried at the door of the slot: refused
// 23505 jobs_one_active_per_user_module while the first is confirmed, scheduled, running, paused, cancelling or completing, and accepted while it is draft, previewed or ended. The probe is moved in a savepoint
// that is rolled back, so the first job's slot is judged at each step and the probe never takes it.
// It fails if the slot is held by a state it should not be or released too early or too late.
func TestJobLifecycle_TheSlotThroughALife(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("9e")
	u := ledgerAdminB
	const pausedWhileRunning = "paused while running" // the step that is jobPauseWhileRunning: the job is paused, from running
	type step struct {
		to   string
		held bool
	}
	held := func(sp pgx.Tx) bool {
		var refused bool
		ledgerTrial(t, sp, func(sp2 pgx.Tx) {
			id := next()
			jobInsertAt(t, sp2, id, u, "m2", "previewed")
			sql, args := jobMoveStmt("previewed", "confirmed", id, "user", u, "probe", jobOpts{})
			switch o := jobTry(t, sp2, sql, args...); {
			case o.ok():
			case o.State == "23505" && o.Constraint == "jobs_one_active_per_user_module":
				refused = true
			default:
				t.Fatalf("the probe was refused for another reason: %s (%s)", o, o.Message)
			}
		})
		return refused
	}
	probes := 0
	for _, life := range []struct {
		name  string
		steps []step
		opts  jobOpts // the choices of every hop of the walk: a job that nothing could start is confirmed with a start deadline that is already past (a job expires only after its start deadline)
	}{
		{"a job that runs and succeeds", []step{{"previewed", false}, {"confirmed", true}, {"running", true}, {"completing", true}, {"succeeded", false}}, jobOpts{}},
		{"a job that is cancelled while it runs (module 2 can cancel)", []step{{"previewed", false}, {"confirmed", true}, {"running", true}, {"cancelling", true}, {"completing", true}, {"cancelled", false}}, jobOpts{}},
		{"a scheduled job that is paused, resumed and fails", []step{{"previewed", false}, {"scheduled", true}, {"paused", true}, {"scheduled", true}, {"running", true}, {"failed", false}}, jobOpts{}},
		// platform_0016: a job paused WHILE RUNNING holds the slot as long as it is paused, cancelling and completing, whichever way it ends (the step named pausedWhileRunning is jobPauseWhileRunning; a move out of a
		// pause that was taken while running is one hop, because the chains of jobs_helpers_test.go name only the pause that was taken before the start)
		{"a job that is paused while it runs and cancelled through the engine", []step{{"previewed", false}, {"confirmed", true}, {"running", true}, {pausedWhileRunning, true}, {"cancelling", true}, {"completing", true}, {"cancelled", false}}, jobOpts{}},
		{"a job that is paused while it runs and whose module finishes everything", []step{{"previewed", false}, {"confirmed", true}, {"running", true}, {pausedWhileRunning, true}, {"completing", true}, {"succeeded", false}}, jobOpts{}},
		{"a job that is paused while it runs, resumed and fails", []step{{"previewed", false}, {"confirmed", true}, {"running", true}, {pausedWhileRunning, true}, {"running", true}, {"failed", false}}, jobOpts{}},
		{name: "a confirmed job that nothing could start", steps: []step{{"previewed", false}, {"confirmed", true}, {"expired", false}}, opts: jobOpts{Deadline: jobDeadlinePast}},
		{"a job discarded before it was confirmed", []step{{"previewed", false}, {"draft", false}, {"previewed", false}, {"cancelled", false}}, jobOpts{}},
	} {
		life := life
		t.Run(life.name, func(t *testing.T) {
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				first := next()
				ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", first, u)
				if held(sp) {
					t.Fatal("a draft job holds the slot")
				}
				probes++
				for _, s := range life.steps {
					switch {
					case s.to == pausedWhileRunning:
						jobPauseWhileRunningWith(t, sp, first, life.opts)
					case jobStatus(t, sp, first) == "paused" && ledgerString(t, sp, "SELECT COALESCE(paused_from, '-') FROM platform.jobs WHERE id = $1", first) == "running":
						jobHopWith(t, sp, first, s.to, life.opts) // the way out of a pause taken while running is a hop of its own
					default:
						jobWalkToWith(t, sp, first, s.to, life.opts)
					}
					probes++
					if got := held(sp); got != s.held {
						t.Fatalf("while the first job is %s the slot is held = %v, want %v", s.to, got, s.held)
					}
				}
			})
		})
	}
	if probes < 20 {
		t.Fatalf("%d probes of the slot, want at least 20: the instrument matched too little", probes)
	}
}

// TestJobLifecycle_SettingsAreTransactionLocal: the three settings of a move live and die with the transaction. A move is made the long way in one transaction and committed; in the NEXT transaction of the same
// session the settings read back empty and a bare UPDATE of the status is refused 23001 job_transition_needs_an_actor (a custom setting that was SET LOCAL reads back as the empty string, not NULL: both are
// "absent"), while the same move through the function passes (the control). The same committed scratch database shows that confirmed_at is set once: a job paused and resumed in later transactions keeps the confirmed_at of its confirm.
// It fails if a writer's settings can outlive his transaction and authorise the next one's moves.
func TestJobLifecycle_SettingsAreTransactionLocal(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	seed, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSeed(t, seed)
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	id := ledgerJobA1 // a draft job of user A on module 1
	jobWalkTo(t, admin, id, "confirmed")

	// confirmed_at is set once: a job scheduled, paused and resumed, each in a transaction of its own (autocommit), keeps the confirmed_at of the confirm, and that time is in the past of the last move
	sched := ledgerJobA2 // user A, module 2: another slot than the job above
	jobWalkTo(t, admin, sched, "scheduled")
	was := ledgerString(t, admin, "SELECT confirmed_at::text FROM platform.jobs WHERE id = $1", sched)
	jobHop(t, admin, sched, "paused")
	jobHop(t, admin, sched, "scheduled")
	if got := ledgerString(t, admin, "SELECT confirmed_at::text FROM platform.jobs WHERE id = $1", sched); got != was {
		t.Fatalf("a pause and a resume changed confirmed_at from %s to %s", was, got)
	}
	if !ledgerBool(t, admin, "SELECT confirmed_at < now() AND updated_at > confirmed_at FROM platform.jobs WHERE id = $1", sched) {
		t.Fatal("the control: the resume ran in a later transaction than the confirm, so confirmed_at must be older than updated_at")
	}

	tx1, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerExpectOK(t, tx1, "the move the long way: the three settings, then the status", jobMoveBy("engine", "", "the first transaction", id, "running"))
	if got := ledgerString(t, tx1, "SELECT COALESCE(NULLIF(current_setting('platform.reason', true), ''), '-')"); got != "the first transaction" {
		t.Fatalf("inside the transaction the reason setting reads %q, want the one that was set", got)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx2, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	for _, name := range []string{"platform.actor_kind", "platform.actor_id", "platform.reason"} {
		if got := ledgerString(t, tx2, "SELECT COALESCE(NULLIF(current_setting($1, true), ''), '-')", name); got != "-" {
			t.Fatalf("in the next transaction %s reads %q, want it absent: the setting outlived its transaction", name, got)
		}
	}
	ledgerExpectRefused(t, tx2, "a bare UPDATE of the next move, in the next transaction", "23001", ledgerWhy{Constraint: "job_transition_needs_an_actor"}, "UPDATE platform.jobs SET status = 'completing' WHERE id = $1", id)
	ledgerExpectOK(t, tx2, "control: the same move through the function", jobMoveSQL, id, "completing", "engine", nil, "the second transaction")
}
