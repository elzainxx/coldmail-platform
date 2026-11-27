package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 5 S8 as extended by the owner's ruling (platform_0021): the template and the merge data are cleared with the rendered messages. What a database must show (the guards refusing every other change, the
// purge, the digests) is in tests/harness/jobs_content_retention_test.go and jobs_retention_template_test.go; these hold the text of the migration against the older texts it replaces and against the Go code.

func migrationFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("%s is not where the unit tests look for it: %v", name, err)
	}
	if len(b) < 500 {
		t.Fatalf("%s holds %d bytes: the instrument matched nothing", name, len(b))
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

const (
	mig0021Up   = "platform_0021_retention_clears_template_and_merge_data.up.sql"
	mig0021Down = "platform_0021_retention_clears_template_and_merge_data.down.sql"
)

// lineCounts is how many times each trimmed, non-empty line occurs: a multiset, so that a line that is repeated (END IF;) cannot hide that one more of it was added or one was lost.
func lineCounts(s string) map[string]int {
	out := map[string]int{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l]++
		}
	}
	return out
}

// lostAndGained are the lines the new text has fewer of and more of than the old one.
func lostAndGained(old, now string) (lost, gained []string) {
	o, n := lineCounts(old), lineCounts(now)
	for l, c := range o {
		for i := n[l]; i < c; i++ {
			lost = append(lost, l)
		}
	}
	for l, c := range n {
		for i := o[l]; i < c; i++ {
			gained = append(gained, l)
		}
	}
	return lost, gained
}

// THE TWO GUARDS CARRY EVERY TERM OF THEIR LATEST TEXT (the migration's header says so; this proves it). platform.job_rows_frozen() is platform_0014's text with ONE branch added (no line of it is lost) and
// platform.jobs_guard() is platform_0011's text with ONE term added to its frozen-columns test (the one line that ended that test is the only line lost).
func TestPlatform0021_TheTwoGuardsCarryEveryTermOfTheirLatestTextAndAddOnlyThePurge(t *testing.T) {
	up := migrationFile(t, mig0021Up)

	oldRows := functionBody(t, migrationFile(t, "platform_0014_suppressed_flag.up.sql"), "job_rows_frozen")
	newRows := functionBody(t, up, "job_rows_frozen")
	lost, gained := lostAndGained(oldRows, newRows)
	if len(lineCounts(oldRows)) < 25 {
		t.Fatalf("the old rows' guard has %d distinct lines: the instrument read the wrong text", len(lineCounts(oldRows)))
	}
	if len(lost) != 0 {
		t.Errorf("job_rows_frozen lost %d line(s) of platform_0014's text, want none: %q", len(lost), lost)
	}
	if len(gained) < 8 || len(gained) > 14 {
		t.Errorf("job_rows_frozen gained %d lines, want the one branch (8 to 14): %q", len(gained), gained)
	}
	for _, want := range []string{"-- (platform_0021) THE RETENTION PURGE", "NEW.fields = '{}'::jsonb", "IF v_status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN"} {
		found := false
		for _, g := range gained {
			if strings.Contains(g, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("job_rows_frozen did not gain a line that says %q", want)
		}
	}

	oldJob := functionBody(t, migrationFile(t, "platform_0011_job_lifecycle.up.sql"), "jobs_guard")
	newJob := functionBody(t, up, "jobs_guard")
	lost, gained = lostAndGained(oldJob, newJob)
	if len(lineCounts(oldJob)) < 8 {
		t.Fatalf("the old job guard has %d distinct lines: the instrument read the wrong text", len(lineCounts(oldJob)))
	}
	if len(lost) != 1 || !strings.HasSuffix(lost[0], "OR NEW.confirm_key IS DISTINCT FROM OLD.confirm_key) THEN") {
		t.Errorf("jobs_guard lost %q, want exactly the one line that ended the frozen-columns test", lost)
	}
	for _, g := range gained {
		if !strings.Contains(g, "platform_0021") && !strings.Contains(g, "NEW.preview") && !strings.Contains(g, "NEW.confirm_key") && !strings.Contains(g, "jsonb_set") && !strings.Contains(g, "OLD.status IN") {
			t.Errorf("jobs_guard gained a line that is not part of the purge's term: %q", g)
		}
	}
	// every message the old guards raise is raised by the new ones
	for _, pair := range [][2]string{{oldRows, newRows}, {oldJob, newJob}} {
		for _, l := range strings.Split(pair[0], "\n") {
			if i := strings.Index(l, "RAISE EXCEPTION '"); i >= 0 && !strings.Contains(pair[1], strings.TrimSpace(l[i:])) {
				t.Errorf("a refusal of the old text is not in the new one: %s", strings.TrimSpace(l[i:]))
			}
		}
	}
	// the control: a guard that dropped a refusal would show (the same comparison on a text without one)
	if l, _ := lostAndGained(newRows, strings.Replace(newRows, "job_rows_pending_flag_is_verify_only", "job_rows_x", 1)); len(l) == 0 {
		t.Error("the comparison cannot see a dropped refusal")
	}
}

// THE DOWN RESTORES THE OLDER GUARDS TO THE LETTER.
func TestPlatform0021_TheDownRestoresTheOlderGuardsToTheLetterAndRefusesWhileAJobIsCleared(t *testing.T) {
	up, down := migrationFile(t, mig0021Up), migrationFile(t, mig0021Down)
	if functionBody(t, migrationFile(t, "platform_0014_suppressed_flag.up.sql"), "job_rows_frozen") != functionBody(t, down, "job_rows_frozen") {
		t.Error("the rows' guard the down installs is not platform_0014's body, byte for byte")
	}
	if functionBody(t, migrationFile(t, "platform_0011_job_lifecycle.up.sql"), "jobs_guard") != functionBody(t, down, "jobs_guard") {
		t.Error("the job guard the down installs is not platform_0011's body, byte for byte")
	}
	if functionBody(t, up, "job_rows_frozen") == functionBody(t, down, "job_rows_frozen") || functionBody(t, up, "jobs_guard") == functionBody(t, down, "jobs_guard") {
		t.Error("the control: an up guard is the older one, so the comparisons above prove nothing")
	}
	for _, want := range []string{"ERRCODE = '55000'", "SELECT count(*) INTO n FROM platform.jobs WHERE content_cleared_at IS NOT NULL", "IF n > 0 THEN", "DELETE FROM platform.schema_migrations WHERE version = '0021'",
		"jsonb_exists(frozen_settings, 'content')"} {
		if !strings.Contains(down, want) {
			t.Errorf("the down of platform_0021 does not contain %q", want)
		}
	}
	// the refusal comes before any change
	if strings.Index(down, "IF n > 0 THEN") > strings.Index(down, "CREATE OR REPLACE FUNCTION") {
		t.Error("the down changes a guard before it has looked for a cleared job")
	}
}

// NOTHING NEW IS GRANTED: the migration holds no GRANT, no CREATE FUNCTION (a new function would need an EXECUTE decision) and replaces no CHECK; it replaces exactly the two guards and one index.
func TestPlatform0021_AddsNoGrantNoFunctionAndNoCheckAndTheIndexSaysWhatThePurgeSays(t *testing.T) {
	up := migrationFile(t, mig0021Up)
	var code []string
	for _, l := range strings.Split(up, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--") {
			code = append(code, l)
		}
	}
	text := strings.Join(code, "\n")
	for _, bad := range []string{"GRANT ", "REVOKE ", "CREATE FUNCTION", "ADD CONSTRAINT", "DROP CONSTRAINT", "CREATE TRIGGER", "ENABLE ROW LEVEL SECURITY", "CREATE POLICY"} {
		if strings.Contains(text, bad) {
			t.Errorf("platform_0021 contains %q: it must give platform_app nothing new", bad)
		}
	}
	if n := strings.Count(text, "CREATE OR REPLACE FUNCTION"); n != 2 {
		t.Errorf("platform_0021 replaces %d functions, want exactly the two guards", n)
	}
	// the index loses the term that limited it to jobs with a campaign and keeps the two others, which are the two the purge's statements say
	i := strings.Index(text, "CREATE INDEX jobs_content_retention_idx")
	if i < 0 {
		t.Fatal("platform_0021 does not recreate jobs_content_retention_idx")
	}
	idx := text[i:]
	idx = idx[:strings.Index(idx, ";")]
	if strings.Contains(idx, "jsonb_exists") {
		t.Errorf("the index still limits itself to jobs with a campaign: %s", idx)
	}
	for _, w := range []struct{ index, purge string }{
		{"content_cleared_at IS NULL", "j.content_cleared_at IS NULL"},
		{"status IN ('succeeded', 'failed', 'cancelled', 'expired')", "j.status IN ('succeeded', 'failed', 'cancelled', 'expired')"},
	} {
		if !strings.Contains(idx, w.index) || !strings.Contains(retentionDue, w.purge) {
			t.Errorf("the index and the purge's conditions do not both say %q: the partial index would not serve the select", w.index)
		}
	}
	// a mark taken back for the jobs platform_0018 marked, with the set-once trigger off for that statement only
	flat := strings.Join(strings.Fields(text), " ")
	if !strings.Contains(flat, "DISABLE TRIGGER jobs_content_cleared_once; UPDATE platform.jobs SET content_cleared_at = NULL WHERE content_cleared_at IS NOT NULL; ALTER TABLE platform.jobs ENABLE TRIGGER jobs_content_cleared_once;") {
		t.Error("platform_0021 does not take back the marks of platform_0018 with the set-once trigger off for that one statement")
	}
}

// THE STORE'S TEMPLATE EXPRESSION IS THE GUARD'S, and the placeholder in both is the one the rendered items get.
func TestPlatform0021_TheStoresTemplateExpressionIsTheGuardsAndThePlaceholderIsOne(t *testing.T) {
	guard := functionBody(t, migrationFile(t, mig0021Up), "jobs_guard")
	flatGuard := strings.Join(strings.Fields(guard), " ")
	want := strings.Join(strings.Fields(strings.ReplaceAll(templateCleared, "j.frozen_settings", "OLD.frozen_settings")), " ")
	if !strings.Contains(flatGuard, want) {
		t.Errorf("the guard does not hold the store's expression:\n  store %s\n  guard %s", want, flatGuard)
	}
	if !strings.Contains(templateCleared, `'"`+ContentPlaceholder+`"'::jsonb`) || !strings.Contains(templateCleared, `'""'::jsonb`) {
		t.Errorf("the template expression does not set the subject and the html to %q and the text to empty: %s", ContentPlaceholder, templateCleared)
	}
	for _, path := range []string{"'{content,subject}'", "'{content,html}'", "'{content,text}'"} {
		if strings.Count(templateCleared, path) != 1 {
			t.Errorf("the template expression names %s %d times, want once", path, strings.Count(templateCleared, path))
		}
	}
	if strings.Contains(templateCleared, "reply_to") {
		t.Error("the template expression touches reply_to: it is an address and stays")
	}
}
