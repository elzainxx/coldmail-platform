package jobenginetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The fake is what the engine's database tests stand on, so it is held to its own claims here: a test double that does not do what the tests say it does makes every green above it a guess.

func rowsOf(n int) []jobs.EngineRow {
	out := make([]jobs.EngineRow, n)
	for i := range out {
		out[i] = jobs.EngineRow{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Ordinal: i + 1, EmailNorm: fmt.Sprintf("r%d@example.test", i+1)}
	}
	return out
}

func job(id string) jobs.EngineJob { return jobs.EngineJob{Job: jobs.Job{ID: id}} }

func ids(rows []jobs.EngineRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestFake_SubmitIsIdempotentAndTakesOnlySomeRowsPerCall(t *testing.T) {
	f := &FakeModule{AcceptPerCall: 2}
	ctx := context.Background()
	rows := rowsOf(5)
	res, err := f.Submit(ctx, job("j1"), rows)
	if err != nil || !eq(res.Accepted, ids(rows[:2])) {
		t.Fatalf("first call accepted %v (%v), want the first two rows", res.Accepted, err)
	}
	// the same offer again: the two it holds come back (a replay) and two more are taken
	res, err = f.Submit(ctx, job("j1"), rows)
	if err != nil || !eq(res.Accepted, ids(rows[:4])) {
		t.Fatalf("second call accepted %v (%v), want the two held rows again and two new ones", res.Accepted, err)
	}
	// a replay of only held rows takes nothing new and holds still four
	res, _ = f.Submit(ctx, job("j1"), rows[:4])
	if len(res.Accepted) != 4 || f.HeldCount("j1") != 4 {
		t.Errorf("a replay changed what the module holds: accepted %d, holds %d", len(res.Accepted), f.HeldCount("j1"))
	}
	if f.Offers(rows[0].ID) != 3 || f.Held(rows[0].ID) != 3 || f.Offers(rows[4].ID) != 2 || f.Held(rows[4].ID) != 0 {
		t.Errorf("offer counts: row1 %d/%d, row5 %d/%d", f.Offers(rows[0].ID), f.Held(rows[0].ID), f.Offers(rows[4].ID), f.Held(rows[4].ID))
	}
	// the control: a module with no limit takes everything, and another job's rows are its own
	g := &FakeModule{}
	if res, _ := g.Submit(ctx, job("j1"), rows); len(res.Accepted) != 5 {
		t.Errorf("an unlimited module accepted %d rows, want 5", len(res.Accepted))
	}
	if res, _ := f.Submit(ctx, job("j2"), rows[:1]); len(res.Accepted) != 1 || f.HeldCount("j2") != 1 || f.HeldCount("j1") != 4 {
		t.Errorf("jobs are not separate in the fake")
	}
	if c := f.Calls(); c.Submit != 4 || c.Total != 4 {
		t.Errorf("calls = %+v", c)
	}
}

func TestFake_ModuleOneReturnsItsReferenceAndTheSendersDoNot(t *testing.T) {
	m1 := &FakeModule{Module: jobs.ModuleVerify}
	res, err := m1.Submit(context.Background(), job("j1"), rowsOf(2))
	if err != nil || res.ModuleRef == "" || !strings.Contains(res.ModuleRef, "j1") {
		t.Errorf("module 1 answered %+v, %v: it must return its own handle for the job", res, err)
	}
	if res2, _ := m1.Submit(context.Background(), job("j1"), rowsOf(2)); res2.ModuleRef != res.ModuleRef {
		t.Error("the handle changed on a replay")
	}
	m2 := &FakeModule{}
	if res, _ := m2.Submit(context.Background(), job("j1"), rowsOf(2)); res.ModuleRef != "" {
		t.Errorf("a sender returned the handle %q", res.ModuleRef)
	}
}

func TestFake_WaitSubmitsAnswerErrWaitAndChangeNothing(t *testing.T) {
	f := &FakeModule{WaitSubmits: 2}
	rows := rowsOf(3)
	for i := 1; i <= 2; i++ {
		_, err := f.Submit(context.Background(), job("j1"), rows)
		if !errors.Is(err, jobengine.ErrWait) {
			t.Fatalf("call %d: %v, want ErrWait", i, err)
		}
		if f.HeldCount("j1") != 0 {
			t.Fatalf("call %d: a refused submit held rows", i)
		}
	}
	// the control: the third call is answered
	if res, err := f.Submit(context.Background(), job("j1"), rows); err != nil || len(res.Accepted) != 3 {
		t.Errorf("the call after the waits: %+v, %v", res, err)
	}
}

func TestFake_DownAnswersErrWaitToEveryMethodAndChangesNothing(t *testing.T) {
	f := &FakeModule{}
	rows := rowsOf(2)
	if _, err := f.Submit(context.Background(), job("j1"), rows); err != nil {
		t.Fatal(err)
	}
	f.SetDown(true)
	before := f.Items("j1")
	if _, err := f.Submit(context.Background(), job("j1"), rowsOf(4)); !errors.Is(err, jobengine.ErrWait) {
		t.Errorf("submit while down: %v", err)
	}
	called := false
	if _, err := f.Observe(context.Background(), job("j1"), func([]jobs.ResultItem) error { called = true; return nil }); !errors.Is(err, jobengine.ErrWait) || called {
		t.Errorf("observe while down: %v (callback called: %v)", err, called)
	}
	if err := f.Cancel(context.Background(), job("j1")); !errors.Is(err, jobengine.ErrWait) {
		t.Errorf("cancel while down: %v", err)
	}
	after := f.Items("j1")
	if len(after) != len(before) || after[0].State != StateQueued || after[1].State != StateQueued {
		t.Errorf("a module that is down changed its items: %+v -> %+v", before, after)
	}
	f.SetDown(false)
	if _, err := f.Observe(context.Background(), job("j1"), func([]jobs.ResultItem) error { return nil }); err != nil {
		t.Errorf("the control: observe after the module is back: %v", err)
	}
}

func TestFake_ObserveWorksTheItemsAndHandsOverOnlyFinalOnes(t *testing.T) {
	f := &FakeModule{FinalizePerObserve: 2, PageSize: 2}
	rows := rowsOf(5)
	f.Submit(context.Background(), job("j1"), rows)
	var pages [][]jobs.ResultItem
	collect := func(p []jobs.ResultItem) error { pages = append(pages, p); return nil }
	obs, err := f.Observe(context.Background(), job("j1"), collect)
	if err != nil || obs.Complete || obs.Total != 5 || obs.Terminal != 2 || len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("first observe: %+v, %d pages, %v; want 2 of 5 final, one page, not complete", obs, len(pages), err)
	}
	for _, it := range pages[0] {
		if it.Outcome.State != StateSent || it.Outcome.Module != jobs.ModuleSendManaged || it.Detail["fake"] != true {
			t.Errorf("an item of the report: %+v", it)
		}
	}
	// every final item is handed over every time: the engine must tolerate repeats
	pages = nil
	obs, _ = f.Observe(context.Background(), job("j1"), collect)
	total := 0
	for _, p := range pages {
		total += len(p)
		if len(p) > 2 {
			t.Errorf("a page of %d items, the limit is 2", len(p))
		}
	}
	if obs.Terminal != 4 || total != 4 || len(pages) != 2 {
		t.Errorf("second observe: terminal %d, %d items in %d pages; want 4 items in 2 pages (the first two again)", obs.Terminal, total, len(pages))
	}
	obs, _ = f.Observe(context.Background(), job("j1"), collect)
	if !obs.Complete || obs.Terminal != 5 {
		t.Errorf("third observe: %+v, want complete", obs)
	}
	// an error from the callback stops the walk and is returned
	boom := errors.New("boom")
	n := 0
	if _, err := f.Observe(context.Background(), job("j1"), func([]jobs.ResultItem) error { n++; return boom }); !errors.Is(err, boom) || n != 1 {
		t.Errorf("a failing callback: %v after %d calls", err, n)
	}
}

func TestFake_LeakNotFinalHandsOverQueuedItemsToo_TheControlOfTheEnginesNeverInvents(t *testing.T) {
	for _, leak := range []bool{false, true} {
		f := &FakeModule{FinalizePerObserve: 1, LeakNotFinal: leak}
		f.Submit(context.Background(), job("j1"), rowsOf(3))
		var got []jobs.ResultItem
		f.Observe(context.Background(), job("j1"), func(p []jobs.ResultItem) error { got = append(got, p...); return nil })
		queued := 0
		for _, it := range got {
			if it.Outcome.State == StateQueued {
				queued++
			}
		}
		if (leak && (queued != 2 || len(got) != 3)) || (!leak && (queued != 0 || len(got) != 1)) {
			t.Errorf("LeakNotFinal=%v: %d items, %d of them queued", leak, len(got), queued)
		}
	}
}

func TestFake_CancelTakesBackQueuedItemsAndLetsTheOnesInFlightFinish(t *testing.T) {
	f := &FakeModule{FinalizePerObserve: 1}
	rows := rowsOf(4)
	f.Submit(context.Background(), job("j1"), rows)
	if n := f.ClaimInFlight("j1", 1); n != 1 {
		t.Fatalf("claimed %d", n)
	}
	if err := f.Cancel(context.Background(), job("j1")); err != nil {
		t.Fatal(err)
	}
	items := f.Items("j1")
	if items[0].State != StateSending || items[1].State != StateCancelled || items[2].State != StateCancelled || items[3].State != StateCancelled {
		t.Fatalf("states after the cancel: %v %v %v %v; want the claimed item in flight and the rest cancelled", items[0].State, items[1].State, items[2].State, items[3].State)
	}
	var got []jobs.ResultItem
	obs, _ := f.Observe(context.Background(), job("j1"), func(p []jobs.ResultItem) error { got = append(got, p...); return nil })
	if !obs.Complete || obs.Terminal != 4 {
		t.Errorf("after the cancel and one observe: %+v, want complete (the in-flight item finished)", obs)
	}
	states := map[string]int{}
	for _, it := range got {
		states[it.Outcome.State]++
		if it.Outcome.Module != jobs.ModuleSendManaged {
			t.Errorf("a cancelled item names module %q", it.Outcome.Module)
		}
	}
	if states[StateSent] != 1 || states[StateCancelled] != 3 {
		t.Errorf("the report: %v, want one sent and three cancelled", states)
	}
	fo := f.FinalOutcomes("j1")
	cancelled := 0
	for _, o := range fo {
		if o.State == StateCancelled {
			cancelled++
		}
	}
	if len(fo) != 4 || cancelled != 3 {
		t.Errorf("FinalOutcomes after the cancel: %d outcomes, %d cancelled; want what the module reported: 4 and 3", len(fo), cancelled)
	}
	// nothing new is claimed or accepted after the cancel
	if _, err := f.Submit(context.Background(), job("j1"), rowsOf(6)); !errors.Is(err, jobengine.ErrRefused) {
		t.Errorf("a submit after the cancel: %v, want ErrRefused", err)
	}
	// the control: module 1 cannot cancel
	m1 := &FakeModule{Module: jobs.ModuleVerify}
	if err := m1.Cancel(context.Background(), job("j1")); !errors.Is(err, jobengine.ErrNotSupported) {
		t.Errorf("module 1 cancel: %v, want ErrNotSupported", err)
	}
}

func TestFake_PlanDecidesWhatEachRowBecomes(t *testing.T) {
	f := &FakeModule{Plan: func(r jobs.EngineRow) charge.Outcome {
		if r.Ordinal%2 == 0 {
			return charge.Outcome{Module: "m2", State: StateBounced}
		}
		return charge.Outcome{Module: "m2", State: StateSent}
	}}
	f.Submit(context.Background(), job("j1"), rowsOf(4))
	f.Observe(context.Background(), job("j1"), func([]jobs.ResultItem) error { return nil })
	fo := f.FinalOutcomes("j1")
	if len(fo) != 4 {
		t.Fatalf("%d final outcomes", len(fo))
	}
	refs := SortedRefs(fo)
	if fo[refs[0]].State != StateSent || fo[refs[1]].State != StateBounced {
		t.Errorf("the plan was not followed: %+v", fo)
	}
}

func TestFake_TheCrashSwitchDiesBeforeACallOrRightAfterIt(t *testing.T) {
	// BEFORE call n+1: the call has no effect and answers like a cancelled context
	killed := 0
	f := &FakeModule{}
	f.Arm(1, false, func() { killed++ })
	if _, err := f.Submit(context.Background(), job("j1"), rowsOf(2)); err != nil {
		t.Fatalf("call 1 must run: %v", err)
	}
	if _, err := f.Observe(context.Background(), job("j1"), func([]jobs.ResultItem) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("call 2 must die first: %v", err)
	}
	if killed != 1 || f.Armed() || f.Calls().Total != 1 || f.Items("j1")[0].State != StateQueued {
		t.Errorf("a call that died first left traces: killed %d, armed %v, calls %+v, state %s", killed, f.Armed(), f.Calls(), f.Items("j1")[0].State)
	}
	// and it fires once: the next call runs
	if _, err := f.Observe(context.Background(), job("j1"), func([]jobs.ResultItem) error { return nil }); err != nil || killed != 1 {
		t.Errorf("after the kill the module answers again: %v (killed %d)", err, killed)
	}

	// AFTER call n: the effect stands, the answer is returned, the kill happened
	killed = 0
	g := &FakeModule{}
	g.Arm(1, true, func() { killed++ })
	res, err := g.Submit(context.Background(), job("j1"), rowsOf(2))
	if err != nil || len(res.Accepted) != 2 || killed != 1 || g.HeldCount("j1") != 2 {
		t.Errorf("a call killed after it ran: %+v, %v, killed %d, holds %d; want its effect and its answer", res, err, killed, g.HeldCount("j1"))
	}
	// the controls: an unarmed module never fires; Arm(0, true) never fires; Disarm removes it
	h := &FakeModule{}
	h.Submit(context.Background(), job("j1"), rowsOf(1))
	h.Arm(0, true, func() { killed += 100 })
	h.Submit(context.Background(), job("j1"), rowsOf(1))
	h.Arm(5, false, func() { killed += 100 })
	h.Disarm()
	for i := 0; i < 7; i++ {
		h.Submit(context.Background(), job("j1"), rowsOf(1))
	}
	if killed != 1 {
		t.Errorf("a switch fired that must not have: killed = %d", killed)
	}
}

func TestStubs_GateClockAndModules(t *testing.T) {
	g := NewGate(false)
	if g.Ensure(context.Background()) {
		t.Error("a gate that does not lead said it does")
	}
	g.Set(true)
	if !g.Ensure(context.Background()) || g.Calls() != 2 {
		t.Errorf("gate: calls %d", g.Calls())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.Ensure(ctx) {
		t.Error("a gate leads under a cancelled context")
	}

	start := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	c := NewClock(start)
	c.Advance(90 * time.Minute)
	if got := c.Now(); got != start.Add(90*time.Minute) {
		t.Errorf("clock = %v", got)
	}

	a, b := &FakeModule{}, &FakeModule{Module: jobs.ModuleVerify}
	m := &Modules{M: a, ByModule: map[string]*FakeModule{"m1": b}}
	if got, err := m.For(context.Background(), "u", "m2"); err != nil || got != jobengine.Module(a) {
		t.Errorf("For(m2) = %v, %v", got, err)
	}
	if got, _ := m.For(context.Background(), "u", "m1"); got != jobengine.Module(b) {
		t.Error("For(m1) did not answer the module registered for it")
	}
	m.SetErr(jobengine.Wait("keys"))
	if _, err := m.For(context.Background(), "u", "m2"); !errors.Is(err, jobengine.ErrWait) {
		t.Errorf("For while an error is set: %v", err)
	}
	if _, err := (&Modules{}).For(context.Background(), "u", "m2"); err == nil {
		t.Error("a Modules with no module answered")
	}
}

// The doubles are for tests: if a file that is not a test imports this package, the platform binary links a fake module. This walks the repository's cmd and internal trees and fails on the first such
// import. The control builds the same check over a directory that does import it.
func TestNothingOutsideTestsImportsThisPackage(t *testing.T) {
	const pkg = "internal/jobengine/jobenginetest"
	root := filepath.Join("..", "..", "..")
	found, scanned := importers(t, root, pkg)
	if scanned < 20 {
		t.Fatalf("the scan read only %d non-test Go files under cmd/ and internal/: it found nothing to check", scanned)
	}
	if len(found) != 0 {
		t.Errorf("non-test files import %s, so the platform binary would link a fake module: %v", pkg, found)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package x\n\nimport _ \"github.com/coldmail-platform/platform/"+pkg+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, _ := scanFiles(t, dir, pkg); len(f) != 1 {
		t.Errorf("the control: a non-test file that imports the package was not found (found %v)", f)
	}
}

func importers(t *testing.T, root, pkg string) (found []string, scanned int) {
	t.Helper()
	for _, d := range []string{"cmd", "internal"} {
		f, n := scanFiles(t, filepath.Join(root, d), pkg)
		found, scanned = append(found, f...), scanned+n
	}
	return found, scanned
}

func scanFiles(t *testing.T, dir, pkg string) (found []string, scanned int) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/jobenginetest/") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if strings.Contains(string(b), "coldmail-platform/platform/"+pkg) {
			found = append(found, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found, scanned
}
