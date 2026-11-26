//go:build harness

package harness

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/contractcheck"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: the PENDING contracts of the three drift sessions.
//
// The answers of module 2 and module 3 to POST /v1/pause, POST /v1/resume and a run of a job item the platform marked cancelled (409 job_item_cancelled), and the answers of module 1 to POST
// /v1/jobs/{id}/cancel and a results page that holds a cancelled row, are contracts the adapters DECLARE (m2.PauseContracts, m3.PauseContracts, m1.CancelContracts) and LIST (Contracts) only
// once the lead has installed their recordings and flipped the switch of the adapter package (sendapi.RecordedPauseFixtures, m1.RecordedPauseFixtures). Until then the sessions
//
//   - in RECORD mode (CONTRACT_RECORD=1) run the steps and print each pending contract's answer as a delimited block under the contract's own file name, ONE block per contract: the step names
//     the contract it is the recording of (the "primary" answer), so the file name always says what the file holds (the contractcheck matcher tells contracts of one route apart by their
//     fixtures, and a contract with no fixture yet has nothing to be told apart by); the other answers of the same shape are compared in compare mode only;
//   - in COMPARE mode run a step only when the fixture file of its contract is installed, and compare each answer with it (contractcheck.AssertLive: a difference fails naming route, case and
//     key); a step whose fixture is not installed is SKIPPED, with a printed note that names it, and counted (cpSet.finish prints the count; cpSkipAllowed is the allowance the lead removes in phase 2);
//   - never show these calls to the module's main session (m1Drift, m2Drift, m3Drift): a call no listed contract declares is an "undeclared dependency" there, and a contract with no fixture is red.
//
// Once the switch is on the contracts are listed, the recordings are installed, and the steps send what they observe to the main session like every other test (cpSet.switchOn): the allowance, the
// capture and AssertLive are then dead code and phase 2 removes them.
//
// None of this runs a module: the harness's steps (contract_pause_*_test.go) make the calls, through adapters whose options carry a cpCapture, and hand each answer to judge.

// cpSkipAllowed is the allowance: a step whose fixture is not installed is skipped (with a note) instead of failing. It is true until the lead has installed the recordings; phase 2 sets it to false
// (or deletes the skip path), after which a missing fixture is red here whatever the switch says.
const cpSkipAllowed = false

// cpCapture is the observer of a step's adapter: it keeps the responses of the calls the step made (the answer of a call is the LAST response observed since the previous take: a retried call
// observes each attempt) and, once the switch is on, also hands every response to the module's main session.
type cpCapture struct {
	mu  sync.Mutex
	obs []modclient.Observation
	fwd func(modclient.Observation)
}

// Observe is modclient.Options.Observe.
func (c *cpCapture) Observe(o modclient.Observation) {
	c.mu.Lock()
	c.obs = append(c.obs, o)
	c.mu.Unlock()
	if c.fwd != nil {
		c.fwd(o)
	}
}

// answer is the response of the call just made: the last one observed since the previous call to answer, which must have the HTTP status the step expects (a step that expected a 409 and got a 200 has
// found something worth a failure that says so, not a comparison of two bodies). It fails the step when no response was observed or the body was cut at the client's cap.
func (c *cpCapture) answer(t contractcheck.TB, what string, status int) modclient.Observation {
	t.Helper()
	c.mu.Lock()
	obs := c.obs
	c.obs = nil
	c.mu.Unlock()
	if len(obs) == 0 {
		t.Fatalf("%s: no response was observed: the call never reached the module, or the client was built without the capture", what)
		return modclient.Observation{}
	}
	o := obs[len(obs)-1]
	if o.Status != status || o.Truncated {
		t.Fatalf("%s: the module answered HTTP %d (body cut at the client's cap: %v), want HTTP %d: the step cannot be compared with the contract it is the answer of", what, o.Status, o.Truncated, status)
		return modclient.Observation{}
	}
	return o
}

// cpSet is the pending contracts of ONE module's drift session and what the session does with them.
type cpSet struct {
	label    string
	fsys     fs.FS
	dir      string
	switchOn bool                   // the adapter package's switch: the contracts are listed and the main session judges them
	main     *contractcheck.Session // the module's main drift session: where the captures forward once the switch is on
	declared map[string]modclient.RouteContract
	order    []string
	rec      *contractcheck.Recorder

	mu      sync.Mutex
	armed   map[string]bool
	skipped []string
	ran     map[string]bool
	// recorded holds, in record mode, the primary answer of each contract: finish prints it (a step runs in a subtest, whose log lines carry a deeper indent than the one cmd/contract-fixtures reads)
	recorded map[string]modclient.Observation
}

// newCPSet makes the set of one module. declared are its contracts (PauseContracts / CancelContracts: declared whether or not listed), fsys and dir say where the recorded fixtures are, switchOn is
// the adapter package's switch and main its session. A contract is ARMED (its step runs and its answer is judged) in record mode, once the switch is on, and in compare mode when its fixture file
// is installed; every other contract is SKIPPED.
func newCPSet(label string, declared []modclient.RouteContract, fsys fs.FS, dir string, switchOn bool, main *contractcheck.Session) *cpSet {
	return newCPSetWith(label, declared, fsys, dir, switchOn, main, contractcheck.NewRecorder())
}

// newCPSetWith is newCPSet with the recorder given (NewRecorder reads the record mode from the environment): the tests of the helpers must not depend on how the run was started.
func newCPSetWith(label string, declared []modclient.RouteContract, fsys fs.FS, dir string, switchOn bool, main *contractcheck.Session, rec *contractcheck.Recorder) *cpSet {
	s := &cpSet{label: label, fsys: fsys, dir: dir, switchOn: switchOn, main: main, declared: map[string]modclient.RouteContract{}, rec: rec, armed: map[string]bool{}, ran: map[string]bool{}, recorded: map[string]modclient.Observation{}}
	for _, rc := range declared {
		k := sendapitest.Key(rc)
		s.declared[k] = rc
		s.order = append(s.order, k)
		if s.rec.Recording() || switchOn || cpFixtureInstalled(fsys, dir, rc.Fixture) {
			s.armed[k] = true
		} else {
			s.skipped = append(s.skipped, k)
		}
	}
	return s
}

// cpFixtureInstalled says whether the fixture file of a contract exists under dir in fsys.
func cpFixtureInstalled(fsys fs.FS, dir, name string) bool {
	if fsys == nil || name == "" || !fs.ValidPath(name) || strings.Contains(name, "/") {
		return false
	}
	st, err := fs.Stat(fsys, path.Join(dir, name))
	return err == nil && !st.IsDir()
}

// isArmed says whether the contract (by "route | case") is armed.
func (s *cpSet) isArmed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.armed[key]
}

// anyArmed says whether any step has anything to do: a session with every contract skipped runs none of its steps (and says so).
func (s *cpSet) anyArmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.armed) > 0
}

// capture makes the observer of a step's adapter: it forwards to the main session only once the switch is on (the contracts are then listed, and a call is judged there like any other).
func (s *cpSet) capture() *cpCapture {
	c := &cpCapture{}
	if s.switchOn && s.main != nil {
		c.fwd = s.main.Observer()
	}
	return c
}

// noteSkipped prints, for a step that is skipped as a whole, which contracts it is the answer of and why it did nothing.
func (s *cpSet) noteSkipped(t contractcheck.TB, step string) {
	t.Helper()
	t.Logf("NOTE contract-pending %s: the step %q is SKIPPED (compare mode, the recordings of %s are not installed, the switch is off): %s", s.label, step, strings.Join(s.skipped, "; "), "it runs when CONTRACT_RECORD=1, and once the lead has installed the files and flipped the switch")
}

// judge hands the answer of a call to the contract it is an answer of (key is "route | case"). primary says that this is the answer to RECORD (one per contract): in record mode only the primary
// answer is printed (two blocks for one file with different bodies cannot be installed), every other answer of the same shape is only compared, in compare mode. Once the switch is on the main
// session judges everything and this only notes that the step ran.
func (s *cpSet) judge(t contractcheck.TB, key string, o modclient.Observation, primary bool) {
	t.Helper()
	rc, ok := s.declared[key]
	if !ok {
		t.Fatalf("contract-pending %s: the step names the contract %q, which the adapter does not declare (declared: %v)", s.label, key, s.order)
		return
	}
	s.mu.Lock()
	s.ran[key] = true
	armed := s.armed[key]
	s.mu.Unlock()
	if s.switchOn {
		return
	}
	if !armed {
		t.Fatalf("contract-pending %s: the step judged %q, which is not armed (its fixture is not installed and this is not a record run): the step should have been skipped", s.label, key)
		return
	}
	if s.rec.Recording() {
		if !primary {
			t.Logf("NOTE contract-pending %s: an answer of the shape family of %s (HTTP %d, %d bytes) is not recorded again: one block per contract; it is compared when the run is not a record run", s.label, key, o.Status, len(o.Body))
			return
		}
		// the block is printed by finish, on the session's own test: the step runs in a subtest, whose log lines carry a deeper indent than cmd/contract-fixtures strips
		s.mu.Lock()
		s.recorded[key] = o
		s.mu.Unlock()
		return
	}
	contractcheck.AssertLive(t, rc, s.fsys, s.dir, o.Status, o.Body, s.rec)
}

// finish is what the session's test calls last: it prints how many cases were skipped and which (never silently), fails when a skip is not allowed, and fails for an armed contract no step judged
// (a step that did not run is a contract not exercised, as in the main session).
func (s *cpSet) finish(t contractcheck.TB) {
	t.Helper()
	s.finishWith(t, cpSkipAllowed)
}

func (s *cpSet) finishWith(t contractcheck.TB, skipAllowed bool) {
	t.Helper()
	s.mu.Lock()
	skipped := append([]string(nil), s.skipped...)
	var armed []modclient.RouteContract
	for _, k := range s.order {
		if s.armed[k] {
			armed = append(armed, s.declared[k])
		}
	}
	s.mu.Unlock()
	sort.Strings(skipped)
	switch {
	case s.switchOn:
		t.Logf("NOTE contract-pending %s: the switch is on: the %d contract(s) are listed and judged by the module's own drift session; nothing is pending or skipped", s.label, len(s.order))
		return
	case len(skipped) == 0:
		t.Logf("NOTE contract-pending %s: %d of %d case(s) armed, 0 skipped", s.label, len(armed), len(s.order))
	default:
		t.Logf("NOTE contract-pending %s: %d of %d case(s) armed, %d SKIPPED because their fixture is not installed (compare mode, the switch off): %s", s.label, len(armed), len(s.order), len(skipped), strings.Join(skipped, "; "))
		if !skipAllowed {
			t.Errorf("contract-pending %s: %d case(s) skipped (%s) and the allowance is gone: install the recordings and flip the switch", s.label, len(skipped), strings.Join(skipped, "; "))
		}
	}
	// record mode: the primary answers are printed here, one block per contract, in the order the contracts are declared
	for _, k := range s.order {
		s.mu.Lock()
		o, ok := s.recorded[k]
		s.mu.Unlock()
		if ok {
			contractcheck.AssertLive(t, s.declared[k], s.fsys, s.dir, o.Status, o.Body, s.rec)
		}
	}
	if len(armed) > 0 {
		s.rec.Finish(t, armed) // an armed contract no step judged is red, and the coverage is printed
	}
}

// cpKey is how a step names the contract it judges: "route | case" (sendapitest.Key).
func cpKey(route, kase string) string { return route + " | " + kase }

// cpFields decodes the body of an answer into its keys, for the words a step checks besides the contract (the module's own state words and counts).
func cpFields(t contractcheck.TB, o modclient.Observation) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(o.Body, &m); err != nil {
		t.Fatalf("the answer of %s %s is not a JSON object: %v", o.Method, o.Path, err)
		return map[string]any{}
	}
	return m
}

// cpInt is a JSON number of a decoded answer as an integer (-1 when it is not a number).
func cpInt(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return -1
}

// cpFakeTB is the part of testing.TB the helpers use, recording what a test of the helpers must see.
type cpFakeTB struct {
	errs, logs []string
	fatal      bool
}

func (f *cpFakeTB) Helper() {}
func (f *cpFakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}
func (f *cpFakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *cpFakeTB) Fatalf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
	f.fatal = true
}

func (f *cpFakeTB) text() string {
	return strings.Join(f.errs, "\n") + "\n" + strings.Join(f.logs, "\n")
}

// TestContractPending_ArmingFollowsTheModeTheSwitchAndTheInstalledFixtures: a pending contract is armed in record mode, when the switch is on and in compare mode when its file is installed, and
// skipped otherwise, with the skipped ones named; a set whose contracts are all skipped has nothing to run. The control for each: the same set built with the other input arms differently, and a
// skip with the allowance gone is an error.
func TestContractPending_ArmingFollowsTheModeTheSwitchAndTheInstalledFixtures(t *testing.T) {
	shape := modclient.Object(modclient.Req("campaign_id", modclient.Str), modclient.Req("paused", modclient.Bool))
	declared := []modclient.RouteContract{
		{Module: modclient.M2, Route: "POST /v1/pause", Case: "ok", Fixture: "pause.ok.json", Status: 200, Shape: shape},
		{Module: modclient.M2, Route: "POST /v1/resume", Case: "ok", Fixture: "resume.ok.json", Status: 200, Shape: shape},
	}
	only := fstestFS(map[string]string{"testdata/contract/pause.ok.json": `{"campaign_id":"x","paused":true}`})
	s := newCPSetMode("m2", declared, only, "testdata/contract", false, false)
	if !s.isArmed("POST /v1/pause | ok") || s.isArmed("POST /v1/resume | ok") || !s.anyArmed() || len(s.skipped) != 1 || s.skipped[0] != "POST /v1/resume | ok" {
		t.Fatalf("compare mode with only the pause fixture installed: armed %v, skipped %v", s.armed, s.skipped)
	}
	none := newCPSetMode("m2", declared, fstestFS(nil), "testdata/contract", false, false)
	if none.anyArmed() || len(none.skipped) != 2 {
		t.Fatalf("compare mode with nothing installed: armed %v, skipped %v, want every contract skipped", none.armed, none.skipped)
	}
	if rec := newCPSetMode("m2", declared, fstestFS(nil), "testdata/contract", false, true); len(rec.skipped) != 0 || !rec.isArmed("POST /v1/resume | ok") {
		t.Fatalf("record mode with nothing installed: armed %v, skipped %v, want every contract armed", rec.armed, rec.skipped)
	}
	if on := newCPSetMode("m2", declared, fstestFS(nil), "testdata/contract", true, false); len(on.skipped) != 0 || !on.anyArmed() {
		t.Fatalf("the switch on with nothing installed: armed %v, skipped %v, want every contract armed (the main session reports a missing fixture)", on.armed, on.skipped)
	}
	// the skip is printed, and with the allowance gone it is an error that names what was skipped
	ft := &cpFakeTB{}
	none.finishWith(ft, true)
	if len(ft.errs) != 0 || !strings.Contains(ft.text(), "2 SKIPPED") || !strings.Contains(ft.text(), "POST /v1/pause | ok") {
		t.Fatalf("finish with the allowance: errors %v, notes %v, want a note naming the two skipped cases and no error", ft.errs, ft.logs)
	}
	ft = &cpFakeTB{}
	none.finishWith(ft, false)
	if len(ft.errs) != 1 || !strings.Contains(ft.errs[0], "the allowance is gone") {
		t.Fatalf("finish without the allowance: errors %v, want one saying the allowance is gone", ft.errs)
	}
	// a path that is not a plain file name is never an installed fixture
	for _, bad := range []string{"", "../x.json", "a/b.json"} {
		if cpFixtureInstalled(only, "testdata/contract", bad) {
			t.Fatalf("%q was taken for an installed fixture", bad)
		}
	}
}

// TestContractPending_JudgeComparesARealAnswerAndRecordsOnePrimaryAnswer: in compare mode an answer equal in shape to the installed fixture passes and an answer that lost a key fails naming the
// route, the case and the key; in record mode the primary answer is printed as one block under the contract's own file name, a second answer of the same shape is not, and an answer judged
// for a contract the adapter does not declare fails the step. The step that never judged an armed contract is red at finish.
func TestContractPending_JudgeComparesARealAnswerAndRecordsOnePrimaryAnswer(t *testing.T) {
	shape := modclient.Object(modclient.Req("campaign_id", modclient.Str), modclient.Req("paused", modclient.Bool))
	declared := []modclient.RouteContract{{Module: modclient.M3, Route: "POST /v1/pause", Case: "ok", Fixture: "pause.ok.json", Status: 200, Shape: shape}}
	fx := fstestFS(map[string]string{"testdata/contract/pause.ok.json": "{\n  \"campaign_id\": \"00000000-0000-4000-8000-000000000001\",\n  \"paused\": true\n}\n"})
	good := modclient.Observation{Module: modclient.M3, Status: 200, Body: []byte(`{"campaign_id":"5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02","paused":false}`)}
	bad := modclient.Observation{Module: modclient.M3, Status: 200, Body: []byte(`{"campaign_id":"5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"}`)}
	key := "POST /v1/pause | ok"

	s := newCPSetMode("m3", declared, fx, "testdata/contract", false, false)
	ft := &cpFakeTB{}
	s.judge(ft, key, good, true)
	if len(ft.errs) != 0 {
		t.Fatalf("compare mode, an answer of the recorded shape: %v", ft.errs)
	}
	s.judge(ft, key, bad, false)
	if len(ft.errs) == 0 || !strings.Contains(ft.errs[0], "POST /v1/pause") || !strings.Contains(ft.errs[0], "paused") || !strings.Contains(ft.errs[0], "ok") {
		t.Fatalf("compare mode, an answer that lost `paused`: %v, want a difference naming the route, the case and the key", ft.errs)
	}
	ft = &cpFakeTB{}
	s.finish(ft)
	if len(ft.errs) != 0 {
		t.Fatalf("finish after the step ran: %v", ft.errs)
	}
	idle := newCPSetMode("m3", declared, fx, "testdata/contract", false, false)
	ft = &cpFakeTB{}
	idle.finish(ft)
	if len(ft.errs) == 0 || !strings.Contains(ft.errs[0], "never exercised") {
		t.Fatalf("finish of an armed contract no step judged: %v, want it red as never exercised", ft.errs)
	}
	ft = &cpFakeTB{}
	s.judge(ft, "POST /v1/nothing | ok", good, true)
	if !ft.fatal || !strings.Contains(ft.text(), "does not declare") {
		t.Fatalf("an answer judged for an undeclared contract: %v, want the step failed", ft.errs)
	}

	// record mode: one block for the primary answer, none for the second answer of the shape family; the block names the file of the contract
	r := newCPSetMode("m3", declared, fstestFS(nil), "testdata/contract", false, true)
	ft = &cpFakeTB{}
	r.judge(ft, key, good, true)
	r.judge(ft, key, bad, false)
	if n := strings.Count(ft.text(), "==== CONTRACT-FIXTURE BEGIN"); len(ft.errs) != 0 || n != 0 {
		t.Fatalf("record mode: judge printed %d block(s) and %v, want none: the step runs in a subtest, so the blocks are printed by finish on the session's own test (errors %v)", n, ft.logs, ft.errs)
	}
	r.finish(ft)
	blocks := strings.Count(ft.text(), "==== CONTRACT-FIXTURE BEGIN module=m3 route=POST /v1/pause case=ok file=pause.ok.json status=200 ====")
	if len(ft.errs) != 0 || blocks != 1 || strings.Count(ft.text(), "==== CONTRACT-FIXTURE BEGIN") != 1 {
		t.Fatalf("record mode: errors %v, %d block(s) for the contract after finish, want exactly one block and no error (logs %v)", ft.errs, blocks, ft.logs)
	}
	if !strings.Contains(ft.text(), "is not recorded again") {
		t.Fatalf("record mode: the second answer is skipped without a note: %v", ft.logs)
	}
}

// newCPSetMode is newCPSetWith with the record mode given.
func newCPSetMode(label string, declared []modclient.RouteContract, fsys fs.FS, dir string, switchOn, recording bool) *cpSet {
	rec := contractcheck.NewRecorder()
	rec.SetRecordMode(recording)
	return newCPSetWith(label, declared, fsys, dir, switchOn, nil, rec)
}

// fstestFS is a read-only file system of the files a test names (path -> content).
func fstestFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// keep the testing import honest: the helpers take contractcheck.TB so that these tests can pass a fake, and a *testing.T satisfies it.
var _ contractcheck.TB = (*testing.T)(nil)
