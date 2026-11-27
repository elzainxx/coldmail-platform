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

// THE PURGE NEVER TOUCHES AN OUTCOME. retention.go is read as text: the only tables it writes are public.job_items and, for the mark, platform.jobs; no statement of it names the rows, the results, the events or the
// ledger as the target of a write. (The harness test compares a digest of those tables before and after; this holds the source to the same promise.)
func TestRetentionSource_WritesNothingButJobItemsAndTheMark(t *testing.T) {
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
		if tbl != "public.job_items" && tbl != "platform.jobs" {
			t.Errorf("retention.go writes %s: only public.job_items and the mark on platform.jobs are its to write", tbl)
		}
	}
	if targets["public.job_items"] != 1 || targets["platform.jobs"] != 1 {
		t.Errorf("retention.go has %v writes, want exactly one statement on public.job_items and one on platform.jobs", targets)
	}
	// the mark sets one column and the page sets exactly the four of the spec
	if !strings.Contains(src, "SET content_cleared_at = now()") || !strings.Contains(src, "SET rendered_subject = $3, rendered_body = $3, rendered_text = NULL, unsubscribe_url = NULL") {
		t.Error("retention.go no longer sets what the spec says: subject and body to the placeholder, text and link to NULL, and the mark")
	}
	if strings.Contains(src, "reply_to") && strings.Contains(strings.ReplaceAll(src, "`reply_to`", ""), "reply_to =") {
		t.Error("retention.go writes reply_to: it is not content of the body and stays")
	}
	// the control: the matcher sees a forbidden write
	if m := write.FindStringSubmatch("UPDATE platform.job_results SET x = 1"); m == nil || m[3] != "platform.job_results" {
		t.Error("the matcher does not see a write to platform.job_results: it cannot fail")
	}
}

// Every statement of the purge holds the job's own conditions (terminal, old enough, not marked, with content): what makes the items of a job that has not ended safe from a caller that names the wrong job.
func TestRetentionDue_SaysTerminalOldEnoughAndNotMarked(t *testing.T) {
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
