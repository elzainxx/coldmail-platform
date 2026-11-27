package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Phase 5 S7 and S8 (platform_0018), the parts of the store that need no database. What a database must show (the sweep itself, the guard, the purge, the controls) is in
// tests/harness/jobs_pause_limit_test.go and jobs_content_retention_test.go; these hold what is decided in Go before the database is asked and the text of the migration the Go code relies on.

func migration0018(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "platform_0018_pause_limit_and_content_retention."+name+".sql"))
	if err != nil {
		t.Fatalf("platform_0018 (%s) is not where the unit tests look for it: %v", name, err)
	}
	if len(b) < 1000 {
		t.Fatalf("platform_0018 (%s) holds %d bytes: the instrument matched nothing", name, len(b))
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// functionBody is the body of the CREATE OR REPLACE FUNCTION platform.<name>() a migration carries, between its AS $fn$ and its closing $fn$.
func functionBody(t *testing.T, sql, name string) string {
	t.Helper()
	i := strings.Index(sql, "CREATE OR REPLACE FUNCTION platform."+name+"()")
	if i < 0 {
		t.Fatalf("the migration does not replace %s", name)
	}
	rest := sql[i:]
	open := strings.Index(rest, "AS $fn$")
	if open < 0 {
		t.Fatalf("%s has no $fn$ body", name)
	}
	rest = rest[open+len("AS $fn$"):]
	end := strings.Index(rest, "$fn$;")
	if end < 0 {
		t.Fatalf("the body of %s does not end", name)
	}
	return rest[:end]
}

// guardBody is the body of the CREATE OR REPLACE FUNCTION platform.jobs_transition_guard() a migration carries.
func guardBody(t *testing.T, sql string) string {
	t.Helper()
	return functionBody(t, sql, "jobs_transition_guard")
}

func trimmedLines(s string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}

func TestCheckPauseLimit_TheEdgesAreTheStoresAndEveryOtherValueIsABadRequest(t *testing.T) {
	for _, ok := range []time.Duration{MinPauseLimit, 2 * time.Second, time.Hour, 7 * 24 * time.Hour, MaxPauseLimit} {
		if err := checkPauseLimit(ok); err != nil {
			t.Errorf("a limit of %s is refused: %v", ok, err)
		}
	}
	for _, bad := range []time.Duration{0, -time.Second, MinPauseLimit - 1, MaxPauseLimit + 1, 91 * 24 * time.Hour} {
		if err := checkPauseLimit(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("a limit of %s: got %v, want ErrBadRequest", bad, err)
		}
	}
	if MaxPauseLimit != 90*24*time.Hour || MinPauseLimit != time.Second {
		t.Errorf("the bounds are %s to %s, the dial says 1 day to 90 days and the engine's tests use seconds", MinPauseLimit, MaxPauseLimit)
	}
}

func TestCancelOverLimitPaused_ABadLimitOrBatchIsRefusedBeforeTheDatabaseIsAsked(t *testing.T) {
	s := &Store{} // no pool: anything that reached the database would panic
	ctx := context.Background()
	for name, c := range map[string]struct {
		limit time.Duration
		max   int
	}{
		"no limit": {0, 10}, "a negative limit": {-time.Hour, 10}, "more than 90 days": {MaxPauseLimit + time.Second, 10},
		"a negative batch": {time.Hour, -1}, "a batch above the maximum": {time.Hour, MaxPauseLimitBatch + 1},
	} {
		res, err := s.CancelOverLimitPaused(ctx, c.limit, c.max)
		if !errors.Is(err, ErrBadRequest) || res.Found != 0 {
			t.Errorf("%s: got %+v, %v; want ErrBadRequest and nothing done", name, res, err)
		}
	}
}

func TestPauseLimitReason_BeginsWithTheWordAndNamesTheLimitInOneLine(t *testing.T) {
	for limit, want := range map[time.Duration]string{
		24 * time.Hour: "1 day", 7 * 24 * time.Hour: "7 days", 90 * 24 * time.Hour: "90 days", 36 * time.Hour: "36h0m0s", 90 * time.Second: "1m30s",
	} {
		r := pauseLimitReason(limit)
		if !strings.HasPrefix(r, ReasonPauseLimit+": ") || !strings.Contains(r, "limit of "+want+" ") || strings.ContainsAny(r, "\r\n") || len(r) > maxReasonBytes {
			t.Errorf("the reason for a limit of %s is %q: want it to begin %q, name %q, be one line and at most %d bytes", limit, r, ReasonPauseLimit, want, maxReasonBytes)
		}
	}
	// the control: the check can fail
	if strings.Contains(pauseLimitReason(time.Hour), "limit of 7 days") {
		t.Error("the reason names a limit that was not asked for")
	}
}

// The migration the Go code relies on: every name the store, the engine and the harness tests look for is raised or created by it, and a rename on one side only fails here.
func TestPlatform0018_NamesTheColumnsConstraintsAndIndexesTheGoCodeRelaysOn(t *testing.T) {
	up := migration0018(t, "up")
	for _, name := range []string{
		"ADD COLUMN paused_at timestamptz", "ADD COLUMN content_cleared_at timestamptz",
		"jobs_paused_at_shape", "jobs_paused_at_idx", "jobs_content_cleared_after_the_end", "jobs_content_retention_idx",
		"CONSTRAINT = 'jobs_content_cleared_is_set_once'", "CREATE TRIGGER jobs_content_cleared_once", "(status = 'paused') = (paused_at IS NOT NULL)",
		"NEW.paused_at IS DISTINCT FROM OLD.paused_at", "INSERT INTO platform.schema_migrations (version) VALUES ('0018')",
	} {
		if !strings.Contains(up, name) {
			t.Errorf("platform_0018 does not contain %q", name)
		}
	}
	if flat := strings.Join(strings.Fields(up), " "); !strings.Contains(flat, "NEW.paused_at := CASE WHEN NEW.status = 'paused' THEN now() ELSE NULL END;") {
		t.Error("the guard of platform_0018 does not set paused_at to the database's now() at the move into paused and NULL out of it")
	}
	// the index of platform_0018 covers the jobs that carry a campaign; platform_0021 replaced it (see TestPlatform0021_*), because the purge now covers every ended job
	for _, w := range []string{"content_cleared_at IS NULL", "status IN ('succeeded', 'failed', 'cancelled', 'expired')", "jsonb_exists(frozen_settings, 'content')"} {
		if !strings.Contains(up, w) {
			t.Errorf("the index jobs_content_retention_idx does not say %q", w)
		}
	}
	// the control: a name nobody creates is reported
	if strings.Contains(up, "jobs_invented_for_the_control") {
		t.Error("the control matched a name that is not there")
	}
	down := migration0018(t, "down")
	for _, name := range []string{"DROP TRIGGER jobs_content_cleared_once", "DROP INDEX platform.jobs_content_retention_idx", "DROP COLUMN content_cleared_at", "DROP COLUMN paused_at",
		"DROP CONSTRAINT jobs_paused_at_shape", "DELETE FROM platform.schema_migrations WHERE version = '0018'"} {
		if !strings.Contains(down, name) {
			t.Errorf("the down of platform_0018 does not contain %q", name)
		}
	}
}

// THE GUARD CARRIES EVERY RULE OF platform_0016 (Docs: "diff your function body against the latest one"). The body of the replacement is the old body with paused_at added: the lines it lost are exactly the two
// that listed the lifecycle columns (the annotation rule's condition and its message) and every line it gained mentions paused_at. A rule dropped by a careless copy would show as a lost line.
func TestPlatform0018_TheGuardIsPlatform0016sBodyPlusPausedAtAndNothingElse(t *testing.T) {
	old := guardBody(t, migrationText(t)) // platform_0016's up file, which pause_cancel_test.go reads
	now := guardBody(t, migration0018(t, "up"))
	oldLines, newLines := trimmedLines(old), trimmedLines(now)
	var lost, gained []string
	for l := range oldLines {
		if !newLines[l] {
			lost = append(lost, l)
		}
	}
	for l := range newLines {
		if !oldLines[l] {
			gained = append(gained, l)
		}
	}
	sort.Strings(lost)
	sort.Strings(gained)
	if len(lost) != 2 {
		t.Errorf("the new guard lost %d lines of platform_0016's, want exactly 2 (the annotation rule's condition and its message): %q", len(lost), lost)
	}
	for _, l := range lost {
		if !strings.Contains(l, "NEW.paused_from IS DISTINCT FROM OLD.paused_from OR NEW.cancel_requested_at") && !strings.Contains(l, "paused_from, cancel_requested_at, confirmed_at") {
			t.Errorf("the new guard lost a line that is not one of the two it was meant to change: %q", l)
		}
	}
	if len(gained) < 3 || len(gained) > 5 {
		t.Errorf("the new guard gained %d lines, want 3 to 5 (the condition, the message, the comment and the assignment): %q", len(gained), gained)
	}
	for _, l := range gained {
		if !strings.Contains(l, "paused_at") {
			t.Errorf("the new guard gained a line that is not about paused_at: %q", l)
		}
	}
	// every constraint name the old guard raises is raised by the new one
	for _, l := range strings.Split(old, "\n") {
		if i := strings.Index(l, "CONSTRAINT = '"); i >= 0 && !strings.Contains(now, strings.TrimSpace(l[i:])) {
			t.Errorf("the new guard no longer raises %s", strings.TrimSpace(l[i:]))
		}
	}
	if len(trimmedLines(old)) < 80 {
		t.Fatalf("the old guard has %d distinct lines: the instrument read the wrong text", len(oldLines))
	}
	// the control: a guard that dropped a rule is seen (take one wall out of the new text and the same comparison loses it)
	broken := strings.Replace(now, "job_started_early", "job_removed_for_the_control", 1)
	if strings.Contains(broken, "CONSTRAINT = 'job_started_early'") || !strings.Contains(old, "CONSTRAINT = 'job_started_early'") {
		t.Error("the control cannot tell a dropped wall from a kept one")
	}
}

// THE DOWN RESTORES platform_0016's guard to the letter.
func TestPlatform0018_TheDownRestoresTheOlderGuardToTheLetter(t *testing.T) {
	if guardBody(t, migrationText(t)) != guardBody(t, migration0018(t, "down")) {
		t.Fatal("the guard the down of platform_0018 installs is not platform_0016's body, byte for byte")
	}
	if guardBody(t, migrationText(t)) == guardBody(t, migration0018(t, "up")) {
		t.Fatal("the control: the up guard is the old one, so the comparison above proves nothing")
	}
}
