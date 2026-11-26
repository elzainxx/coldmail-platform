package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Phase 4 close-out D1 (platform_0016), the parts of the store that need no database. What a database must show (the moves themselves, the engine's work, the notes) is in tests/harness (jobs_store_test.go,
// jobs_pause_cancel_engine_test.go); these hold the words and names the rest relies on, against the migration's own text, so that a rename on one side fails here and not at the first CI run of the heavy workflows.

// migrationText reads platform_0016's up file (the unit tests of this package run in internal/jobs).
func migrationText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "platform_0016_pause_cancel_capabilities.up.sql"))
	if err != nil {
		t.Fatalf("platform_0016 is not where the unit tests look for it: %v", err)
	}
	if len(b) < 1000 {
		t.Fatalf("platform_0016 holds %d bytes: the instrument matched nothing", len(b))
	}
	return string(b)
}

// The three walls of platform_0016 arrive as ErrNotAllowed carrying the constraint's name (the API words the one a user can act on), and the widened capability check keeps its old name; the controls
// are the same names under another SQLSTATE, which are not refusals of the lifecycle.
func TestMapDBError_ThePauseWallsOfPlatform0016ArriveAsNamedNotAllowedReasons(t *testing.T) {
	s := &Store{}
	for _, name := range []string{"job_unsubmitted_pause_never_cancelling", "job_unsubmitted_pause_never_completing", ReasonSubmittedJobCancelsThroughTheEngine, "job_module_cannot_cancel"} {
		got := s.fail(pgErr("23001", name))
		var na *NotAllowedError
		if !errors.Is(got, ErrNotAllowed) || !errors.As(got, &na) || na.Reason != name {
			t.Errorf("a 23001 refusal named %s is %v, want ErrNotAllowed with that reason", name, got)
		}
		var pe *pgconn.PgError
		if errors.As(got, &pe) {
			t.Errorf("%s: the mapped error still carries the driver's error", name)
		}
		// the control: another SQLSTATE under the same name is not a refusal of the lifecycle
		if other := s.fail(pgErr("23514", name)); errors.Is(other, ErrNotAllowed) {
			t.Errorf("a 23514 violation named %s was mapped as a refusal of the lifecycle", name)
		}
	}
}

// The names the Go side uses are the names the migration raises: a constraint renamed in one place only is a wall the API no longer words and a test no longer finds.
func TestPlatform0016_TheConstraintNamesOfTheStoreAreTheOnesTheGuardRaises(t *testing.T) {
	sql := migrationText(t)
	for _, name := range []string{ReasonSubmittedJobCancelsThroughTheEngine, "job_unsubmitted_pause_never_cancelling", "job_unsubmitted_pause_never_completing", "job_module_cannot_cancel"} {
		if !strings.Contains(sql, "CONSTRAINT = '"+name+"'") {
			t.Errorf("platform_0016 does not raise a constraint named %s", name)
		}
	}
	// the control: the check can fail (a name nobody raises is reported)
	if strings.Contains(sql, "CONSTRAINT = 'job_invented_for_the_control'") {
		t.Fatal("the control name is in the migration")
	}
	// the two pairs the store's moves depend on are rows of the table the migration inserts into
	for _, pair := range []string{"('paused', 'cancelling', ARRAY['user', 'admin']", "('paused', 'completing', ARRAY['engine']"} {
		if !strings.Contains(sql, pair) {
			t.Errorf("platform_0016 does not insert the pair %s", pair)
		}
	}
	// the capabilities it seeds
	for _, seed := range []string{"UPDATE platform.modules SET can_pause = true WHERE id IN ('m2', 'm3')", "UPDATE platform.modules SET can_cancel = true WHERE id = 'm1'"} {
		if !strings.Contains(sql, seed) {
			t.Errorf("platform_0016 does not seed: %s", seed)
		}
	}
}

// The module-hold note: its two words are the ones the statement writes and the one it reads back to decide whether the module's state CHANGED (a typo in either half would write a note at every look or
// never), and an id that cannot be a job is refused before the database is asked.
func TestNoteModuleHold_TheWordsAreTheStatementsAndABadIDIsRefusedBeforeTheDatabase(t *testing.T) {
	if NoteModulePaused != "module_paused" || NoteModuleResumed != "module_resumed" {
		t.Fatalf("the two words are %q and %q, want module_paused and module_resumed (the job's history and Docs/jobs-api.md use them)", NoteModulePaused, NoteModuleResumed)
	}
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "IN ('module_paused', 'module_resumed')") {
		t.Error("the statement of NoteModuleHold does not read back the two words it writes")
	}
	s := &Store{}
	for _, id := range []string{"", "not-a-uuid", "00000000-0000-4000-8000-00000000000", "00000000-0000-4000-8000-0000000000000"} {
		if wrote, err := s.NoteModuleHold(context.Background(), id, true); !errors.Is(err, ErrNotFound) || wrote {
			t.Errorf("NoteModuleHold(%q) = %v, %v; want ErrNotFound and nothing written", id, wrote, err)
		}
	}
	// the control: a well-formed id passes the check (the call then needs its pool: it panics on the empty store, which is the proof that the check was passed)
	defer func() {
		if recover() == nil {
			t.Error("a well-formed job id was refused before the database was asked")
		}
	}()
	_, _ = s.NoteModuleHold(context.Background(), "1b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5f", false)
}

// The store's moves of a running job pick the move from the job's state and leave the rule to the database: the Go text of Cancel, Pause and Resume holds no capability or lifecycle table of its own
// (no module id, no can_pause, no status pair), and the three older names are thin callers.
func TestMoves_TheCapabilityAndTheLegalPairsAreTheDatabasesNotAGoIf(t *testing.T) {
	b, err := os.ReadFile("moves.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, forbidden := range []string{"can_pause", "can_cancel", `"m1"`, `"m2"`, `"m3"`, "ModuleVerify", "ModuleSendManaged", "ModuleSendBYO"} {
		for _, fn := range []string{"func (s *Store) Pause(", "func (s *Store) Resume(", "func (s *Store) Cancel("} {
			at := strings.Index(src, fn)
			if at < 0 {
				t.Fatalf("moves.go has no %s", fn)
			}
			end := strings.Index(src[at+1:], "\nfunc ")
			if end < 0 {
				end = len(src) - at - 1
			}
			if strings.Contains(src[at:at+1+end], forbidden) {
				t.Errorf("%s decides on %s in Go: the capability of a module is a row of platform.modules and the guard refuses what it cannot do", fn, forbidden)
			}
		}
	}
	// the control: the check finds what it looks for (the old names call the new ones)
	for _, call := range []string{"return s.Cancel(ctx, userID, jobID)", "return s.Pause(ctx, userID, jobID)", "return s.Resume(ctx, userID, jobID)"} {
		if !strings.Contains(src, call) {
			t.Errorf("moves.go does not hold %q: an older name is no thin caller", call)
		}
	}
}
