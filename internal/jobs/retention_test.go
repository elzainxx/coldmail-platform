package jobs

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Phase 5 S8, the parts of the content retention that need no database (retention.go). The statements are proved against a real Postgres by tests/harness/jobs_content_retention_test.go.

func TestCheckRetention_TheEdgesAreTheStoresAndEveryOtherValueIsABadRequest(t *testing.T) {
	for _, ok := range []time.Duration{MinContentRetention, time.Minute, 7 * 24 * time.Hour, MaxContentRetention} {
		if err := checkRetention(ok); err != nil {
			t.Errorf("a retention of %s is refused: %v", ok, err)
		}
	}
	for _, bad := range []time.Duration{0, -time.Hour, MinContentRetention - 1, MaxContentRetention + 1, 366 * 24 * time.Hour} {
		if err := checkRetention(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("a retention of %s: got %v, want ErrBadRequest", bad, err)
		}
	}
	if MaxContentRetention != 365*24*time.Hour {
		t.Errorf("the highest retention is %s, the dial says 365 days", MaxContentRetention)
	}
}

func TestRetentionBounds_DefaultsAndLimits(t *testing.T) {
	if j, p, err := retentionBounds(0, 0); err != nil || j != DefaultRetentionJobs || p != DefaultRetentionPages {
		t.Errorf("the defaults are %d jobs and %d pages (%v), want %d and %d", j, p, err, DefaultRetentionJobs, DefaultRetentionPages)
	}
	if j, p, err := retentionBounds(MaxRetentionJobs, MaxRetentionPages); err != nil || j != MaxRetentionJobs || p != MaxRetentionPages {
		t.Errorf("the maxima are refused: %d, %d, %v", j, p, err)
	}
	for name, c := range map[string][2]int{"negative jobs": {-1, 1}, "negative pages": {1, -1}, "too many jobs": {MaxRetentionJobs + 1, 1}, "too many pages": {1, MaxRetentionPages + 1}} {
		if _, _, err := retentionBounds(c[0], c[1]); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want ErrBadRequest", name, err)
		}
	}
}

func TestClearRetainedContent_ABadDialOrBoundIsRefusedBeforeTheDatabaseIsAsked(t *testing.T) {
	s := &Store{} // no pool: anything that reached the database would panic
	for name, c := range map[string]struct {
		after           time.Duration
		maxJobs, maxPgs int
	}{
		"no retention": {0, 1, 1}, "a negative retention": {-time.Hour, 1, 1}, "more than a year": {MaxContentRetention + time.Second, 1, 1},
		"a negative job bound": {time.Hour, -1, 1}, "a page bound above the maximum": {time.Hour, 1, MaxRetentionPages + 1},
	} {
		res, err := s.ClearRetainedContent(context.Background(), c.after, c.maxJobs, c.maxPgs)
		if !errors.Is(err, ErrBadRequest) || res != (RetentionResult{}) {
			t.Errorf("%s: got %+v, %v; want ErrBadRequest and nothing done", name, res, err)
		}
	}
}

// The placeholder is what the modules' NOT NULL columns and their pre-flight accept: a non-blank string. A blank one would be refused by the first module that read it.
func TestContentPlaceholder_IsANonBlankOneLineString(t *testing.T) {
	if strings.TrimSpace(ContentPlaceholder) == "" || strings.ContainsAny(ContentPlaceholder, "\r\n") || ContentPlaceholder != "[removed]" {
		t.Fatalf("the placeholder is %q: the spec says [removed], and a blank or multi-line one would not pass the modules' checks", ContentPlaceholder)
	}
	if _, err := checkRendered(1, ContentPlaceholder, ContentPlaceholder, "", ""); err != nil {
		t.Errorf("the platform's own message checks refuse the placeholder as a subject and a body: %v", err)
	}
	// the control: a blank subject is refused by the same check
	if _, err := checkRendered(1, " ", ContentPlaceholder, "", ""); err == nil {
		t.Error("the control cannot fail: a blank subject was accepted")
	}
}

// THE PURGE NEVER TOUCHES AN OUTCOME. retention.go is read as text: the only tables it writes are public.job_items (the rendered messages), platform.job_rows (the merge data, `fields` and nothing else) and
// platform.jobs (the template and the mark, in one statement); no statement of it names the results, the events or the ledger as the target of a write. (The harness test compares a digest of those tables before
// and after; this holds the source to the same promise.)
func TestRetentionSource_WritesNothingButTheRenderedItemsTheMergeDataAndTheTemplateWithItsMark(t *testing.T) {
	b, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	write := regexp.MustCompile(`(?i)(update|insert\s+into|delete\s+from|truncate)\s+(only\s+)?([a-z_]+\.[a-z_]+)`)
	targets := map[string]int{}
	for _, m := range write.FindAllStringSubmatch(src, -1) {
		targets[strings.ToLower(m[3])]++
	}
	if len(targets) == 0 {
		t.Fatal("found no write in retention.go: the instrument matched nothing")
	}
	for tbl := range targets {
		if tbl != "public.job_items" && tbl != "platform.job_rows" && tbl != "platform.jobs" {
			t.Errorf("retention.go writes %s: only the rendered items, the merge data of the rows and the template with its mark are its to write", tbl)
		}
	}
	if targets["public.job_items"] != 1 || targets["platform.job_rows"] != 1 || targets["platform.jobs"] != 1 {
		t.Errorf("retention.go has %v writes, want exactly one statement on each of public.job_items, platform.job_rows and platform.jobs", targets)
	}
	// what each statement sets: the four columns of an item, the one column of a row, and the template with the mark
	for _, want := range []string{
		"SET rendered_subject = $3, rendered_body = $3, rendered_text = NULL, unsubscribe_url = NULL",
		"UPDATE platform.job_rows w SET fields = '{}'::jsonb FROM page",
		"UPDATE platform.jobs j SET frozen_settings = `+templateCleared+`, content_cleared_at = now()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("retention.go no longer says %q: the purge does not set what the ruling says", want)
		}
	}
	// the control: the matcher sees a forbidden write, and a write of the results' columns would be found
	for _, bad := range []string{"UPDATE platform.job_results SET x = 1", "DELETE FROM platform.job_events", "INSERT INTO platform.ledger"} {
		if m := write.FindStringSubmatch(bad); m == nil || m[3] == "platform.job_rows" || m[3] == "platform.jobs" || m[3] == "public.job_items" {
			t.Errorf("the matcher does not see %q as a write outside the three tables: it cannot fail", bad)
		}
	}
}

// Every statement of the purge holds the job's own conditions (terminal, old enough, not marked, with content): what makes the items of a job that has not ended safe from a caller that names the wrong job.
func TestRetentionDue_SaysTerminalOldEnoughAndNotMarked(t *testing.T) {
	if strings.Contains(retentionDue, "jsonb_exists") {
		t.Error("retentionDue limits the purge to jobs with a campaign: since platform_0021 it covers every ended job (a verify job's rows carry merge data too)")
	}
	for _, w := range []string{"j.content_cleared_at IS NULL", "'succeeded', 'failed', 'cancelled', 'expired'", "j.finished_at IS NOT NULL", "j.finished_at <= now() - make_interval(secs => $%d::double precision)"} {
		if !strings.Contains(retentionDue, w) {
			t.Errorf("retentionDue no longer says %q", w)
		}
	}
	for _, status := range []string{"draft", "previewed", "confirmed", "scheduled", "running", "paused", "cancelling", "completing"} {
		if strings.Contains(retentionDue, "'"+status+"'") {
			t.Errorf("retentionDue names %s: a job that has not ended would be cleared", status)
		}
	}
}
