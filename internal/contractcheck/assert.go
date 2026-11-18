package contractcheck

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// EnvRecord is the environment variable that switches AssertLive from comparing to recording: "1" records.
const EnvRecord = "CONTRACT_RECORD"

// TB is the part of testing.TB the helpers use (a *testing.T satisfies it), so that this package imports `testing` only in its own tests.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Recorder remembers which contracts the harness exercised. It is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	seen   map[Exercised]bool
	order  []Exercised
	record bool
}

// NewRecorder returns a Recorder; it is in record mode when CONTRACT_RECORD=1 is set in the environment.
func NewRecorder() *Recorder {
	return &Recorder{seen: map[Exercised]bool{}, record: os.Getenv(EnvRecord) == "1"}
}

// SetRecordMode overrides the mode the environment gave.
func (r *Recorder) SetRecordMode(on bool) {
	r.mu.Lock()
	r.record = on
	r.mu.Unlock()
}

// Recording reports whether the Recorder is in record mode.
func (r *Recorder) Recording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.record
}

// Observe notes one live call. AssertLive does it for the contract it checks; a harness test that calls a module route without a contract notes it here so that Finish
// can fail on the undeclared dependency.
func (r *Recorder) Observe(e Exercised) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.seen[e] {
		r.seen[e] = true
		r.order = append(r.order, e)
	}
}

// Exercised returns the distinct calls noted so far, in the order first noted.
func (r *Recorder) Exercised() []Exercised {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Exercised(nil), r.order...)
}

// logWriter sends what is written to the test log one line per Logf call, so that every line of a recorded block is a line of the log (a multi-line Logf would indent the
// continuation lines and change the body).
type logWriter struct {
	t   TB
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.t.Logf("%s", string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// AssertLive is what a harness test calls after it called a module route for real: rc is the route's contract, fixtureFS and dir say where the recorded bodies are (for
// example the adapter package's embedded testdata and "testdata/contract"), status and body are what the module answered, and ex is the Recorder of the whole run (nil is
// allowed).
//
// It marks the contract exercised, loads the fixture, runs Compare and fails the test once per Diff, naming module, route, case, path and kind. With CONTRACT_RECORD=1 (or a
// Recorder in record mode) it does not compare: it writes the delimited block of Record to the test log (the lead commits what it prints) and fails only when Record
// refuses (a secret-shaped body). Positions that could not be judged are logged, not failed.
//
//	// in a harness test (tag harness), once per contract of an adapter:
//	rec := contractcheck.NewRecorder()
//	for _, rc := range verify.Contracts() {
//	    resp := callTheRealModule(t, rc)                       // status and body as the module answered
//	    contractcheck.AssertLive(t, rc, verify.Fixtures, "testdata/contract", resp.Status, resp.Body, rec)
//	}
//	rec.Finish(t, verify.Contracts()) // fails on a contract no call exercised and a call no contract declares; prints the coverage
func AssertLive(t TB, rc modclient.RouteContract, fixtureFS fs.FS, dir string, status int, body []byte, ex *Recorder) {
	t.Helper()
	if ex != nil {
		ex.Observe(Exercised{rc.Module, rc.Route, rc.Case})
	}
	who := Exercised{rc.Module, rc.Route, rc.Case}.String()
	recording := os.Getenv(EnvRecord) == "1"
	if ex != nil {
		recording = ex.Recording()
	}
	if recording {
		if err := Record(&logWriter{t: t}, rc, status, body); err != nil {
			t.Errorf("contract %s: the response cannot be recorded: %v", who, err)
		}
		return
	}
	if fixtureFS == nil {
		t.Fatalf("contract %s: AssertLive was given no file system for the fixtures", who)
		return
	}
	if rc.Fixture == "" || !fs.ValidPath(rc.Fixture) || strings.Contains(rc.Fixture, "/") {
		t.Errorf("contract %s: the contract names no usable fixture file", who)
		return
	}
	fixture, err := fs.ReadFile(fixtureFS, path.Join(dir, rc.Fixture))
	if err != nil {
		t.Errorf("contract %s: the recorded fixture %s cannot be read (%s): run the harness with %s=1 to record it", who, rc.Fixture, errWord(err), EnvRecord)
		return
	}
	rep := Compare(rc, fixture, body, status)
	for _, d := range rep.Diffs {
		t.Errorf("contract drift: %s", d)
	}
	for _, g := range rep.Gaps {
		t.Logf("contract %s: not compared at %s on the %s side (%s)", who, g.Path, g.Side, g.Why)
	}
	for _, p := range rep.Optional {
		t.Logf("contract %s: optional per the Shape, presence not compared: %s", who, p)
	}
}

// Finish fails the test when a contract was never exercised, when a call has no contract, and when there are no contracts at all; it always logs the coverage.
func (r *Recorder) Finish(t TB, contracts []modclient.RouteContract) {
	t.Helper()
	rep := Coverage(contracts, r.Exercised())
	rep.Print(&logWriter{t: t})
	if rep.Contracts == 0 {
		t.Errorf("contract drift gate: found 0 contracts: the instrument matched nothing")
	}
	for _, e := range rep.Never {
		t.Errorf("contract %s was never exercised against the real module", e)
	}
	for _, e := range rep.Undeclared {
		t.Errorf("live call %s has no contract", e)
	}
}
