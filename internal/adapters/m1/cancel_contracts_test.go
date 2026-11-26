package m1

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: the contracts of module 1's cancel recordings (CancelContracts) are LISTED by Contracts, and the cancel route leaves UncoveredRoutes, only when the switch
// recordedPauseFixtures is true, which the lead flips after installing the three recordings (CancelFixtureNames). These tests hold the two halves of that change together (the switch is never
// ahead of the files, the files are never ahead of the switch) and the declared contracts to what the adapter calls; contracts_test.go adds the three contracts to its table of calls when the
// switch is on.

// cancelFixtureState says which of the named files exist in dir: "name" for each that is missing and for each that is present, sorted.
func cancelFixtureState(dir string, names []string) (missing, present []string) {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			present = append(present, n)
		} else {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(present)
	return missing, present
}

func scratchWithFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// While the switch is on, every file it is about must exist in testdata/contract: the lead flips the switch only after installing the recordings, and a flip that is ahead of them is red here (and
// in the no-rot check) naming the missing files. The controls run the same check on scratch directories: one with a file missing is reported, naming exactly that file; one with none and one with
// all of them are told apart.
func TestCancelFixturesAreInstalledWhenTheSwitchIsOn(t *testing.T) {
	names := CancelFixtureNames()
	if len(names) != 3 {
		t.Fatalf("the switch is about %d file(s) %v, want three: the cancel's 200, its 404 and the results page that holds a cancelled row", len(names), names)
	}
	if missing, present := cancelFixtureState(scratchWithFiles(t, names[0], names[2]), names); len(missing) != 1 || missing[0] != names[1] || len(present) != 2 {
		t.Fatalf("control: a directory without %s: missing %v, present %v, want exactly that file missing", names[1], missing, present)
	}
	if missing, _ := cancelFixtureState(scratchWithFiles(t, names...), names); len(missing) != 0 {
		t.Fatalf("control: a directory with every file: missing %v", missing)
	}
	if missing, _ := cancelFixtureState(scratchWithFiles(t), names); len(missing) != len(names) {
		t.Fatalf("control: an empty directory: missing %v, want all %d", missing, len(names))
	}
	if !recordedPauseFixtures {
		t.Logf("the switch is off: the %d file(s) of module 1's cancel recordings are not required yet", len(names))
		return
	}
	if missing, _ := cancelFixtureState(filepath.Join("testdata", "contract"), names); len(missing) != 0 {
		t.Fatalf("recordedPauseFixtures is true and these recordings are not installed in testdata/contract: %v (install them, as the files the record run printed and cmd/contract-fixtures wrote, BEFORE the switch is flipped)", missing)
	}
}

// While the switch is off, none of the files may exist in testdata/contract: a recording that is installed must be listed (flip the switch in m1/contracts.go, and sendapi's in the same change if module
// 2's and module 3's are installed), or the no-rot check would be red for a file named by no contract and the harness would compare nothing against it. The controls: a scratch directory with
// one of the files is reported, naming it, and one without any is not.
func TestCancelFixturesDoNotExistWhileTheSwitchIsOff(t *testing.T) {
	names := CancelFixtureNames()
	if _, present := cancelFixtureState(scratchWithFiles(t, names[1]), names); len(present) != 1 || present[0] != names[1] {
		t.Fatalf("control: a directory with %s: present %v, want exactly that file", names[1], present)
	}
	if _, present := cancelFixtureState(scratchWithFiles(t), names); len(present) != 0 {
		t.Fatalf("control: an empty directory: present %v", present)
	}
	if recordedPauseFixtures {
		t.Log("the switch is on: the files are required (TestCancelFixturesAreInstalledWhenTheSwitchIsOn), not forbidden")
		return
	}
	if _, present := cancelFixtureState(filepath.Join("testdata", "contract"), names); len(present) != 0 {
		t.Fatalf("recordedPauseFixtures is false and these recordings are installed in testdata/contract: %v (flip the switch, the constant in m1/contracts.go, in the same change)", present)
	}
}

// The declared contracts, the listed ones, the pending ones and the uncovered route are four views of one fact. The three contracts are declared and name the three files once each; while the switch
// is off Contracts lists none of them, PendingContracts all, and UncoveredRoutes names the cancel route with a reason; once it is on Contracts lists all of them, nothing is pending and the cancel
// route is no longer uncovered. The two health routes stay uncovered either way (the control: the switch moves nothing else).
func TestTheCancelContractsAreDeclaredAndListedOrPendingNeverBoth(t *testing.T) {
	declared := CancelContracts()
	names := CancelFixtureNames()
	if len(declared) != 3 {
		t.Fatalf("%d declared contracts, want the cancel's 200 and 404 and the cancelled results page", len(declared))
	}
	var files []string
	for _, c := range declared {
		files = append(files, c.Fixture)
		if c.Module != modclient.M1 || c.Route == "" || c.Case == "" || c.Fixture == "" || c.Status == 0 || len(c.Shape.Fields()) == 0 {
			t.Errorf("the declared contract %s / %s is incomplete: %+v", c.Route, c.Case, c)
		}
	}
	sort.Strings(files)
	want := append([]string(nil), names...)
	sort.Strings(want)
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Errorf("the declared contracts name %v, the switch is about %v", files, want)
	}
	listed := map[string]bool{}
	for _, c := range Contracts() {
		listed[contractKey(c.Route, c.Case)] = true
	}
	pending := map[string]bool{}
	for _, c := range PendingContracts() {
		pending[contractKey(c.Route, c.Case)] = true
	}
	for _, c := range declared {
		k := contractKey(c.Route, c.Case)
		if listed[k] == pending[k] || listed[k] != recordedPauseFixtures {
			t.Errorf("%s: listed %v, pending %v with the switch %v: want exactly one of the two, and listed only when the switch is on", k, listed[k], pending[k], recordedPauseFixtures)
		}
	}
	unc := UncoveredRoutes()
	why, cancelUncovered := unc[tplCancel]
	if cancelUncovered == recordedPauseFixtures || (cancelUncovered && strings.TrimSpace(why) == "") {
		t.Errorf("the cancel route is declared uncovered %v (reason %q) with the switch %v: want uncovered, with a reason, only while it is off", cancelUncovered, why, recordedPauseFixtures)
	}
	if _, a := unc["GET /healthz"]; !a {
		t.Error("GET /healthz is not declared uncovered")
	}
	if _, a := unc["GET /readyz"]; !a {
		t.Error("GET /readyz is not declared uncovered")
	}
	if recordedPauseFixtures && len(PendingContracts()) != 0 {
		t.Errorf("the switch is on and %d contract(s) are still pending", len(PendingContracts()))
	}
}

// runCancel is the call the cancel contracts are decoded with: the summary is what the adapter decoded (the outcome word, the job's status word and flags, the four counts, cancel_requested).
func runCancel(t *testing.T, status int, body string) (string, error) {
	res, err := serve(t, status, body).adapter(t).Cancel(testCtx(t), testJob)
	if err != nil {
		return "", err
	}
	s := res.Status
	return fmt.Sprintf("%s %s complete=%t counts=%d/%d/%d/%d requested=%t", res.Outcome, s.Status, s.Complete, s.Counts.Total, s.Counts.Settled, s.Counts.Pending, s.Counts.Cancelled, s.CancelRequested), nil
}

// cancelSummary reads the same things out of the body with encoding/json into generic maps, sharing no code with the adapter's decoding.
func cancelSummary(t *testing.T, body string) string {
	d := docOf(t, body)
	c := obj(t, d["counts"])
	requested, _ := d["cancel_requested"].(bool)
	cancelled := 0
	if _, ok := c["cancelled"]; ok {
		cancelled = num(t, c["cancelled"])
	}
	return fmt.Sprintf("%s %s complete=%t counts=%d/%d/%d/%d requested=%t", d["cancel_outcome"], d["status"], d["complete"], num(t, c["total"]), num(t, c["settled"]), num(t, c["pending"]), cancelled, requested)
}

// cancelContractTable are the entries of contracts_test.go's table of calls for the three contracts, added to it only when the switch is on (a contract with no entry is red, and so is an entry
// with no contract).
func cancelContractTable() map[string]contractExpect {
	return map[string]contractExpect{
		contractKey(tplCancel, "ok"):              {run: runCancel, summarize: cancelSummary},
		contractKey(tplCancel, "not_found"):       {run: runCancel, kind: modclient.NotFound, code: "not_found"},
		contractKey(tplResults, "page_cancelled"): {run: runResults, summarize: resultsSummary},
	}
}

// The entries of cancelContractTable are exactly the declared contracts (so that flipping the switch is the only edit the table of calls needs), whichever way the switch is.
func TestTheCancelContractTableIsExactlyTheDeclaredContracts(t *testing.T) {
	table := cancelContractTable()
	declared := CancelContracts()
	if len(table) != len(declared) {
		t.Fatalf("%d entries for %d declared contracts", len(table), len(declared))
	}
	for _, c := range declared {
		if _, ok := table[contractKey(c.Route, c.Case)]; !ok {
			t.Errorf("the declared contract %s has no entry in cancelContractTable", contractKey(c.Route, c.Case))
		}
	}
}

// cancelledPageProblems says what is wrong with a results page as a page that holds a row cancelled before its check began: the adapter must decode it (no Contract error), and every row whose
// status is `cancelled` must be the non-verdict row the module's contract shows (the reason cancelled_by_caller, no cache signal, no skip token, no origin, no repeat), and there must be at least one.
func cancelledPageProblems(t *testing.T, body string) []string {
	t.Helper()
	rows, _, err := serve(t, 200, body).adapter(t).Results(testCtx(t), testJob, "", 0)
	if err != nil {
		return []string{"the adapter refuses the page: " + err.Error()}
	}
	var p []string
	cancelled := 0
	for _, r := range rows {
		if r.Status != StatusCancelled {
			continue
		}
		cancelled++
		if r.Reason != ReasonCancelledByCaller || r.FromCache || r.SkipReason != "" || r.FailureOrigin != "" || r.DuplicateOf != nil {
			p = append(p, fmt.Sprintf("the cancelled row %d decoded as %+v, want reason %s and no verdict provenance", r.Ordinal, r, ReasonCancelledByCaller))
		}
	}
	if cancelled == 0 {
		p = append(p, "the page holds no cancelled row")
	}
	return p
}

// cancelledPageMutations are the mutations of the recorded page that holds a cancelled row, written for the row the recording has (found by its status, never by its position): each makes the
// adapter's own call fail with a Contract error naming the key. The page's own keys and a verdict row's are covered by the mutations of the other recorded pages.
func cancelledPageMutations(t *testing.T, name string) []mutation {
	t.Helper()
	idx := -1
	for i, r := range recordedRows(t, name) {
		if r["status"] == StatusCancelled {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("%s holds no cancelled row: the mutations of a cancelled row would not apply", name)
	}
	at := func(key string) string { return fmt.Sprintf("results[%d].%s", idx, key) }
	return []mutation{
		remove("job_id"), remove("results"), remove("next_cursor"), retype("next_cursor", 4),
		remove(at("ordinal")), remove(at("item_id")), retype(at("item_id"), 3),
		remove(at("status")), outside(at("status"), "withdrawn"),
		remove(at("reason")), outside(at("reason"), "vibes"),
		remove(at("flags")), remove(at("signals")),
	}
}

// The check that a page holds a cancelled row decodable as one is proved on a hand-written page that holds one (the control that it can pass), on a page of verdicts only and on a page whose
// cancelled row carries a reason of a verdict (the controls that it can fail, each for its own reason); and, once the switch is on, it is made of the recording.
func TestTheCancelledPageCheckPassesOnACancelledRowAndFailsWithoutOne(t *testing.T) {
	good := page(testJob, "", handCancelledRow(0), handVerdictRow(1))
	if p := cancelledPageProblems(t, good); len(p) != 0 {
		t.Fatalf("control: a page that holds a cancelled row (hand-written from the module's contract): %v", p)
	}
	if p := cancelledPageProblems(t, fixture(t, "results-page.json")); len(p) != 1 || !strings.Contains(p[0], "no cancelled row") {
		t.Fatalf("control: the recorded page of verdicts only: %v, want exactly that it holds no cancelled row", p)
	}
	odd := page(testJob, "", handRow("cancelled", "accepted", "", "row-0", 0))
	if p := cancelledPageProblems(t, odd); len(p) != 1 || !strings.Contains(p[0], "refuses the page") {
		t.Fatalf("control: a cancelled row with a verdict's reason: %v, want the adapter's refusal", p)
	}
	if !recordedPauseFixtures {
		t.Log("the switch is off: the recording of the cancelled page is not checked yet")
		return
	}
	if p := cancelledPageProblems(t, fixture(t, "results-page-cancelled.json")); len(p) != 0 {
		t.Fatalf("the recorded page that holds a cancelled row: %v", p)
	}
}

// cancelOkMutations are the mutations of the recorded answer of a cancel (the job-status body plus the outcome): the keys the adapter reads, each removed, retyped or (the outcome) set outside its three
// words must make Cancel fail with a Contract error naming it. The identity submitted = settled + cancelled + pending is checked on the status itself by the status tests.
func cancelOkMutations() []mutation {
	return []mutation{
		remove("cancel_outcome"), retype("cancel_outcome", 5), outside("cancel_outcome", "teleported"),
		remove("job_id"), remove("status"), outside("status", "done"), remove("complete"), retype("complete", "no"),
		remove("counts"), remove("counts.total"), remove("counts.settled"), remove("counts.pending"), retype("counts.cancelled", "1"),
		remove("totals"), remove("totals.submitted"), remove("totals.by_status"), retype("totals.cancelled", "1"),
		remove("processing_paused"), retype("cancel_requested", "yes"),
	}
}
