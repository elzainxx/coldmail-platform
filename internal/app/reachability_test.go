package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The reachability sync (reachability.go; Phase 4, chunk B3, its source changed by C3: module 1's describe). No database and no module are needed: the reader (module 1's answer) and the setter are
// fakes, and the clock the loop waits on is injected, so the rhythm is asserted exactly and takes no time. What SetUnitReachable does against a real Postgres is proved by tests/harness
// (TestMoneyReachable_*); what the provider does against a stand-in of module 1's HTTP surface is in reachability_provider_test.go.

type reachCall struct {
	unit        string
	reachable   bool
	source      string
	hadDeadline bool
}

// fakeReach is a reachabilitySetter that records its calls. fn decides each answer (n counts from 1); nil means "changed nothing, no error".
type fakeReach struct {
	mu    sync.Mutex
	calls []reachCall
	fn    func(ctx context.Context, n int) (bool, error)
}

func (f *fakeReach) SetUnitReachable(ctx context.Context, unit string, reachable bool, source string) (bool, error) {
	_, has := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, reachCall{unit, reachable, source, has})
	n := len(f.calls)
	f.mu.Unlock()
	if f.fn == nil {
		return false, nil
	}
	return f.fn(ctx, n)
}

func (f *fakeReach) got() []reachCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reachCall(nil), f.calls...)
}

// fakeModule1 is the reader of the loop: module 1's answer, as a script. fn decides each read (n counts from 1); nil means "module 1 says reachable = reachable".
type fakeModule1 struct {
	mu        sync.Mutex
	reads     int
	reachable bool
	fn        func(ctx context.Context, n int) (reachable, known bool, err error)
}

var _ jobs.VendorReachability = (*fakeModule1)(nil)

func (f *fakeModule1) M1VendorReachable(ctx context.Context) (bool, bool, error) {
	f.mu.Lock()
	f.reads++
	n := f.reads
	f.mu.Unlock()
	if f.fn == nil {
		return f.reachable, true, nil
	}
	return f.fn(ctx, n)
}

func (f *fakeModule1) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// says is a reader that always answers with the given flag.
func says(reachable bool) *fakeModule1 { return &fakeModule1{reachable: reachable} }

// stepper is the injected clock: it records every wait the loop asks for and answers at once; at the limit-th wait it cancels the loop's context, which ends it.
type stepper struct {
	mu     sync.Mutex
	waits  []time.Duration
	limit  int
	cancel context.CancelFunc
}

func (s *stepper) wait(ctx context.Context, d time.Duration) bool {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	n := len(s.waits)
	s.mu.Unlock()
	if n >= s.limit {
		s.cancel()
		return false
	}
	return ctx.Err() == nil
}

// runSync runs syncReachability for exactly `turns` passes on the injected clock and returns the waits it asked for and its log.
func runSync(t *testing.T, src reachabilityReader, set reachabilitySetter, turns int) ([]time.Duration, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &stepper{limit: turns, cancel: cancel}
	logs := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncReachability(ctx, src, set, slog.New(slog.NewJSONHandler(logs, nil)), st.wait)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("syncReachability did not stop at the end of the injected clock")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]time.Duration(nil), st.waits...), logs
}

func sameWaits(got []time.Duration, want ...time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

var (
	errRefused = errors.New("money.set_unit_reachable: failed to connect to `host=127.0.0.1 user=platform_app database=platform`: dial error (dial tcp 127.0.0.1:1: connect: connection refused)")
	errDown    = &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command (secretword)"}
)

const (
	s5  = 5 * time.Second
	s60 = 60 * time.Second
)

func TestSyncReachability_FiveSecondsUntilTheFirstSuccessThenSixty(t *testing.T) {
	// three failures to WRITE, then it works for good: 5 s, 5 s, 5 s, then the minute
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) {
		if n <= 3 {
			return false, errRefused
		}
		return false, nil
	}}
	waits, _ := runSync(t, says(true), f, 7)
	if !sameWaits(waits, s5, s5, s5, s60, s60, s60, s60) {
		t.Errorf("waits = %v, want three of 5s (failing), then 60s from the first success on", waits)
	}
	if n := len(f.got()); n != 7 {
		t.Errorf("%d writes in 7 turns", n)
	}

	// the same rhythm when it is the READ that fails (module 1 is not up yet): no write at all until module 1 answers
	late := &fakeModule1{fn: func(_ context.Context, n int) (bool, bool, error) {
		if n <= 3 {
			return false, false, &modclient.Error{Kind: modclient.Unavailable}
		}
		return true, true, nil
	}}
	setter := &fakeReach{}
	if waits, _ := runSync(t, late, setter, 6); !sameWaits(waits, s5, s5, s5, s60, s60, s60) {
		t.Errorf("a module 1 that comes up late: waits %v, want 5s while it is down and the minute once it answered", waits)
	}
	if n := len(setter.got()); n != 3 {
		t.Errorf("%d writes in 6 turns of which 3 could not read module 1, want 3 (a read that failed writes nothing)", n)
	}

	// control: a database and a module 1 that are there from the start never make the loop wait 5 s
	ok := &fakeReach{}
	if waits, _ := runSync(t, says(true), ok, 4); !sameWaits(waits, s60, s60, s60, s60) {
		t.Errorf("a healthy start waited %v, want 60s every turn", waits)
	}
	// control: a database that never comes up is retried at 5 s for ever (the loop does not slow down to the minute without a success)
	down := &fakeReach{fn: func(context.Context, int) (bool, error) { return false, errRefused }}
	if waits, _ := runSync(t, says(true), down, 4); !sameWaits(waits, s5, s5, s5, s5) {
		t.Errorf("a database that stays down waited %v, want 5s every turn", waits)
	}
	// a failure AFTER the first success waits for the next minute, not five seconds (the flag was set; this is only the re-assertion)
	later := &fakeReach{fn: func(_ context.Context, n int) (bool, error) {
		if n == 2 || n == 3 {
			return false, errRefused
		}
		return false, nil
	}}
	if waits, _ := runSync(t, says(true), later, 4); !sameWaits(waits, s60, s60, s60, s60) {
		t.Errorf("failures after the first success waited %v, want 60s every turn", waits)
	}
}

func TestSyncReachability_WritesWhatModuleOneSaysForTheVendorUnitWithItsSource(t *testing.T) {
	for _, want := range []bool{true, false} {
		f := &fakeReach{}
		runSync(t, says(want), f, 3)
		calls := f.got()
		if len(calls) != 3 {
			t.Fatalf("module 1 says %v: %d writes in 3 turns", want, len(calls))
		}
		for i, c := range calls {
			if c.unit != "m1_vendor" || c.reachable != want || c.source != "m1_describe" {
				t.Errorf("module 1 says %v, write %d: %+v, want m1_vendor, %v and the source m1_describe", want, i+1, c, want)
			}
			if !c.hadDeadline {
				t.Errorf("module 1 says %v, write %d had no deadline: a database that never answers would hold the loop for ever", want, i+1)
			}
		}
	}
	// the two runs differ only in what module 1 said, so the check above is not satisfied by a constant
	a, b := &fakeReach{}, &fakeReach{}
	runSync(t, says(true), a, 1)
	runSync(t, says(false), b, 1)
	if a.got()[0].reachable == b.got()[0].reachable {
		t.Error("control failed: module 1's two answers produced the same write")
	}
	if reachabilitySource != "m1_describe" || reachabilityUnit != "m1_vendor" {
		t.Errorf("the unit and the source are %q and %q", reachabilityUnit, reachabilitySource)
	}
	if money.ReachableSourceM1Describe != "m1_describe" || money.UnitM1Vendor != "m1_vendor" {
		t.Errorf("the money store's names are %q and %q: the history the store writes and the sources the job store tests are the words of the platform_0013 data", money.UnitM1Vendor, money.ReachableSourceM1Describe)
	}
	// the answer follows module 1 turn by turn: it flips and the write flips
	flip := &fakeModule1{fn: func(_ context.Context, n int) (bool, bool, error) { return n >= 3, true, nil }}
	f := &fakeReach{}
	runSync(t, flip, f, 5)
	var got []bool
	for _, c := range f.got() {
		got = append(got, c.reachable)
	}
	if fmt.Sprint(got) != "[false false true true true]" {
		t.Errorf("module 1 flipped on its third answer and the loop wrote %v, want [false false true true true]", got)
	}
}

func TestSyncReachability_ADescribeThatCannotBeReadOrDoesNotSayNeverWrites(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(context.Context, int) (bool, bool, error)
		class string
	}{
		{"module 1 is unavailable", func(context.Context, int) (bool, bool, error) {
			return true, true, &modclient.Error{Kind: modclient.Unavailable, Message: "host db.internal refused"}
		}, "module 1 unavailable"},
		{"module 1 refuses the key", func(context.Context, int) (bool, bool, error) {
			return true, true, &modclient.Error{Kind: modclient.Refused}
		}, "module 1 refused"},
		{"module 1's describe breaks the contract", func(context.Context, int) (bool, bool, error) {
			return true, true, &modclient.Error{Kind: modclient.Contract, Key: "vendor_fallback.reachable"}
		}, "module 1 contract"},
		{"module 1 does not say (an older module 1)", func(context.Context, int) (bool, bool, error) { return true, false, nil }, "module 1 does not say"},
		{"the service caller could not be registered", func(context.Context, int) (bool, bool, error) {
			return true, true, fmt.Errorf("register the platform's module 1 service caller: %w", errDown)
		}, "database error 57P01"},
	} {
		f := &fakeReach{}
		src := &fakeModule1{fn: c.reply} // reachable true in every reply: a loop that read it anyway would write it
		waits, logs := runSync(t, src, f, 4)
		if n := len(f.got()); n != 0 {
			t.Errorf("%s: %d write(s) were made on a describe that gave no answer", c.name, n)
		}
		if !sameWaits(waits, s5, s5, s5, s5) {
			t.Errorf("%s: waits %v, want 5s every turn (no write has ever succeeded)", c.name, waits)
		}
		lines := logs.lines(t)
		if n := countLines(lines, msgReachabilityFailed); n != 1 {
			t.Errorf("%s: the failure was logged %d times in 4 turns, want once per class", c.name, n)
		} else if l := find(lines, msgReachabilityFailed); l["reason"] != c.class || l["level"] != "WARN" {
			t.Errorf("%s: logged as %v, want WARN with the class %q", c.name, l, c.class)
		}
		for _, leak := range []string{"db.internal", "platform_app", "secretword"} {
			if strings.Contains(logs.String(), leak) {
				t.Errorf("%s: the log carries %q:\n%s", c.name, leak, logs.String())
			}
		}
		if countLines(lines, msgReachabilitySynced) != 0 {
			t.Errorf("%s: a first success was announced", c.name)
		}
	}
	// the control: the same loop, module 1 answering, writes on every turn
	f := &fakeReach{}
	runSync(t, says(true), f, 4)
	if n := len(f.got()); n != 4 {
		t.Errorf("control: %d writes in 4 turns with module 1 answering, want 4", n)
	}
	// a flag that was SAID false is a write of false, not a skipped turn: a false that was said and a false that was not are different
	g := &fakeReach{}
	runSync(t, says(false), g, 2)
	if n := len(g.got()); n != 2 || g.got()[0].reachable {
		t.Errorf("module 1 says false: %+v, want two writes of false", g.got())
	}
}

func TestSyncReachability_AFailureIsReportedOncePerClassAndNeverEndsTheLoop(t *testing.T) {
	// the sequence: the database is down (3 turns), the port is refused (2), it works (2), it goes down again (1)
	seq := []error{errDown, errDown, errDown, errRefused, errRefused, nil, nil, errDown}
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) { return false, seq[n-1] }}
	waits, logs := runSync(t, says(true), f, len(seq))
	if len(waits) != len(seq) || len(f.got()) != len(seq) {
		t.Fatalf("the loop made %d writes and %d waits for %d turns: a failure ended it", len(f.got()), len(waits), len(seq))
	}
	lines := logs.lines(t)
	var reasons []string
	for _, l := range lines {
		if l["msg"] == msgReachabilityFailed {
			reasons = append(reasons, fmt.Sprint(l["reason"]))
			if l["level"] != "WARN" {
				t.Errorf("a failure was logged at %v, want WARN", l["level"])
			}
		}
	}
	if want := []string{"database error 57P01", "connection refused", "database error 57P01"}; strings.Join(reasons, "|") != strings.Join(want, "|") {
		t.Errorf("the warnings carry the classes %v, want %v (once per class and per outage: 8 turns, 6 failures, 3 lines)", reasons, want)
	}
	if n := countLines(lines, msgReachabilityRecovered); n != 1 {
		t.Errorf("recovery was announced %d times, want once", n)
	}
	if n := countLines(lines, msgReachabilitySynced); n != 1 {
		t.Errorf("the first success was announced %d times, want once", n)
	}
	// the class is logged, the driver's text is not: not the host, the login or the message
	for _, leak := range []string{"secretword", "platform_app", "127.0.0.1", "terminating connection"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("the log carries %q:\n%s", leak, logs.String())
		}
	}
}

func TestSyncReachability_StopsWithTheContextAndAShutdownIsNoFailure(t *testing.T) {
	entered := make(chan struct{}, 1)
	f := &fakeReach{fn: func(ctx context.Context, _ int) (bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done() // a database that never answers: only the shutdown frees it
		return false, fmt.Errorf("money.set_unit_reachable: %w", ctx.Err())
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncReachability(ctx, says(true), f, slog.New(slog.NewJSONHandler(logs, nil)), sleepContext)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncReachability did not return after the cancel: a pass in flight held it")
	}
	if n := countLines(logs.lines(t), msgReachabilityFailed); n != 0 {
		t.Errorf("a shutdown in the middle of a pass was logged as a failure %d time(s):\n%s", n, logs.String())
	}
	if n := len(f.got()); n != 1 {
		t.Errorf("%d writes: the loop went on after the cancel", n)
	}

	// the same for a READ that module 1 never answers: only the shutdown frees it, and it is no failure
	rd := &fakeModule1{fn: func(ctx context.Context, _ int) (bool, bool, error) {
		<-ctx.Done()
		return false, false, &modclient.Error{Kind: modclient.Canceled}
	}}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	logs2 := &syncBuffer{}
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		syncReachability(ctx2, rd, &fakeReach{}, slog.New(slog.NewJSONHandler(logs2, nil)), sleepContext)
	}()
	waitUntil(t, "the first read of the loop", func() bool { return rd.count() >= 1 })
	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("syncReachability did not return after the cancel: a read in flight held it")
	}
	if n := countLines(logs2.lines(t), msgReachabilityFailed); n != 0 {
		t.Errorf("a shutdown in the middle of a read was logged as a failure %d time(s):\n%s", n, logs2.String())
	}

	// the control: the same error on a LIVE context is a failure and is logged
	live := &fakeReach{fn: func(context.Context, int) (bool, error) {
		return false, fmt.Errorf("money.set_unit_reachable: %w", context.DeadlineExceeded)
	}}
	_, logs3 := runSync(t, says(true), live, 1)
	if l := find(logs3.lines(t), msgReachabilityFailed); l == nil || l["reason"] != "timeout" {
		t.Errorf("a pass that timed out on a live context was logged as %v, want the warning with the class timeout", l)
	}

	// the real clock: it waits what it is asked, is cut short by the context, and is false for a context that is already done
	t0 := time.Now()
	if !sleepContext(context.Background(), 40*time.Millisecond) || time.Since(t0) < 40*time.Millisecond {
		t.Error("sleepContext returned early or false on a live context")
	}
	cctx, ccancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); ccancel() }()
	t0 = time.Now()
	if sleepContext(cctx, time.Hour) || time.Since(t0) > 5*time.Second {
		t.Error("sleepContext did not return false promptly when its context was cancelled")
	}
	if sleepContext(cctx, 0) {
		t.Error("sleepContext returned true for a context that is done")
	}
}

func TestSyncReachability_APanickingReaderOrSetterNeverEndsTheLoop(t *testing.T) {
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) {
		if n <= 2 {
			panic("boom-secretvalue")
		}
		return false, nil
	}}
	waits, logs := runSync(t, says(true), f, 4)
	if !sameWaits(waits, s5, s5, s60, s60) || len(f.got()) != 4 {
		t.Fatalf("waits %v and %d writes: a panic ended or slowed the loop", waits, len(f.got()))
	}
	lines := logs.lines(t)
	if l := find(lines, msgReachabilityFailed); l == nil || !strings.Contains(fmt.Sprint(l["reason"]), "panicked (string)") || countLines(lines, msgReachabilityFailed) != 1 {
		t.Errorf("the panic was logged as %v (%d lines), want one warning naming the type of the panic value", l, countLines(lines, msgReachabilityFailed))
	}
	if strings.Contains(logs.String(), "boom-secretvalue") {
		t.Errorf("the VALUE of a panic reached the log:\n%s", logs.String())
	}
	if find(lines, msgReachabilitySynced) == nil {
		t.Errorf("the loop never reported its first success after the panics:\n%s", logs.String())
	}

	// a reader that panics is the same: it is an error of that turn, the loop goes on and writes once the reader works
	r := &fakeModule1{fn: func(_ context.Context, n int) (bool, bool, error) {
		if n <= 2 {
			panic(errors.New("boom-secretvalue-2"))
		}
		return true, true, nil
	}}
	g := &fakeReach{}
	waits2, logs2 := runSync(t, r, g, 4)
	if !sameWaits(waits2, s5, s5, s60, s60) || len(g.got()) != 2 {
		t.Fatalf("reader panics: waits %v and %d writes, want 5s, 5s, 60s, 60s and the two writes after the panics", waits2, len(g.got()))
	}
	if strings.Contains(logs2.String(), "boom-secretvalue-2") || !strings.Contains(logs2.String(), "panicked (*errors.errorString)") {
		t.Errorf("a reader's panic was logged as:\n%s", logs2.String())
	}
}

func TestSyncReachability_AFlagThatFollowsModuleOneIsSaidSoAtInfoAndNeverAsAWarning(t *testing.T) {
	// the first write changes the flag (the usual start); a later write that changes it again means module 1's answer changed since the last turn
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) { return n == 1 || n == 3, nil }}
	_, logs := runSync(t, says(true), f, 4)
	lines := logs.lines(t)
	if l := find(lines, msgReachabilitySynced); l == nil || l["changed"] != true || l["level"] != "INFO" || l["reachable"] != true {
		t.Errorf("the first success was logged as %v, want INFO with changed=true and the flag", l)
	}
	if n := countLines(lines, msgReachabilityFollowed); n != 1 {
		t.Errorf("the flag moved once after the first success and was reported %d times, want once", n)
	}
	if l := find(lines, msgReachabilityFollowed); l == nil || l["level"] != "INFO" {
		t.Errorf("the report is %v, want an info line (module 1 changed, nobody 'put it back')", l)
	}
	if countLines(lines, msgReachabilityFailed) != 0 {
		t.Errorf("a flag that followed module 1 produced a warning:\n%s", logs.String())
	}
	// the control: a flag nobody touches is never reported, and the first success says changed=false when it found the flag as module 1 says
	quiet := &fakeReach{}
	_, logs2 := runSync(t, says(false), quiet, 4)
	lines2 := logs2.lines(t)
	if countLines(lines2, msgReachabilityFollowed) != 0 || countLines(lines2, msgReachabilityFailed) != 0 {
		t.Errorf("a quiet flag produced reports:\n%s", logs2.String())
	}
	if l := find(lines2, msgReachabilitySynced); l == nil || l["changed"] != false || l["reachable"] != false {
		t.Errorf("the first success of a flag already as module 1 says was logged as %v, want changed=false", l)
	}
	if l := find(lines2, msgReachabilityStarted); l == nil || l["unit"] != "m1_vendor" || l["source"] != "m1_describe" {
		t.Errorf("the start line is %v, want the unit and the source", l)
	}
}

type weirdError struct{}

func (*weirdError) Error() string { return "something with host=db.internal user=platform_app in it" }

func TestReachabilityClass_NamesTheKindNeverTheText(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"nothing", nil, ""},
		{"a database error", fmt.Errorf("money: %w", &pgconn.PgError{Code: "40001", Message: "x"}), "database error 40001"},
		{"a refused connection by its text (Windows spells the errno differently)", errRefused, "connection refused"},
		{"a deadline", fmt.Errorf("money: %w", context.DeadlineExceeded), "timeout"},
		{"a cancel", fmt.Errorf("money: %w", context.Canceled), "canceled"},
		{"a lock that ran out", fmt.Errorf("money: %w", &money.LockTimeoutError{Wait: time.Second}), "waited too long for a lock on the unit"},
		{"an unknown unit", fmt.Errorf("money: %w: %q", money.ErrUnknownUnit, "m1_vendor"), "the unit is not priced"},
		{"a malformed request", fmt.Errorf("money: %w", &money.InvalidError{Field: "source", Reason: "blank"}), "refused as malformed"},
		{"a dns failure", fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "db.internal"}), "dns lookup failed"},
		{"a closed connection", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), "connection closed"},
		{"a panic", &setterPanic{typ: "string"}, "the reachability read or write panicked (string)"},
		{"an error of a type nobody listed", fmt.Errorf("wrapped: %w", &weirdError{}), "error (*app.weirdError)"},
		// module 1's side: the kind of the failed call, whatever its message and cause say
		{"module 1 unavailable", &modclient.Error{Kind: modclient.Unavailable, Message: "host db.internal user platform_app"}, "module 1 unavailable"},
		{"module 1 refused", fmt.Errorf("describe: %w", &modclient.Error{Kind: modclient.Refused}), "module 1 refused"},
		{"module 1 timed out", &modclient.Error{Kind: modclient.Timeout}, "module 1 timeout"},
		{"module 1 broke its contract", &modclient.Error{Kind: modclient.Contract}, "module 1 contract"},
		{"module 1 does not say", fmt.Errorf("read: %w", errNotSaid), "module 1 does not say"},
	} {
		got := reachabilityClass(c.err)
		if got != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got, c.want)
		}
		for _, leak := range []string{"platform_app", "db.internal", "127.0.0.1"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: the class %q carries %q", c.name, got, leak)
			}
		}
	}
}

// ---- the service: module 1's answer reaches the call, and the loop stops before the pool closes ---------------------------------------------------

// withReachabilitySetter replaces the builder of the loop's setter for one test.
func withReachabilitySetter(t *testing.T, build func(pool *pgxpool.Pool, mny *money.Store, cfg config.Config) reachabilitySetter) {
	t.Helper()
	old := newReachabilitySetter
	newReachabilitySetter = build
	t.Cleanup(func() { newReachabilitySetter = old })
}

// withVendorProvider replaces the builder of module 1's reachability provider for one test (nil: module 1 cannot be asked in this process).
func withVendorProvider(t *testing.T, p jobs.VendorReachability) {
	t.Helper()
	old := newVendorProvider
	newVendorProvider = func(moduleAdapters, *tenant.Resolver, tenant.FuncDB, config.Config) jobs.VendorReachability { return p }
	t.Cleanup(func() { newVendorProvider = old })
}

// poolIsClosed asks a pool a question and reports whether the answer was "closed pool": on a pool that is open and whose database is not there the answer is a refused connection.
func poolIsClosed(pool *pgxpool.Pool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := pool.Ping(ctx)
	return err != nil && strings.Contains(err.Error(), "closed pool")
}

func TestRun_ModuleOnesAnswerReachesTheSetterAndTheLoopStopsBeforeThePoolCloses(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t)

	// the instrument can see a closed pool (control), and an open one is not "closed" even though its database is not there
	probe, err := pgxpool.New(context.Background(), closedPortDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if poolIsClosed(probe) {
		t.Fatal("control failed: an open pool whose database is down was reported closed")
	}
	probe.Close()
	if !poolIsClosed(probe) {
		t.Fatal("control failed: a closed pool was not reported closed, so the ordering check below proves nothing")
	}

	// module 1 says true, then false: and a pass in flight at shutdown that takes 300 ms to wind down: Run must wait for it, and the pool must still be open when it asks its question
	for _, answer := range []bool{true, false} {
		var (
			pools    atomic.Int32
			finished atomic.Bool
			sawClose atomic.Bool
		)
		f := &fakeReach{fn: func(ctx context.Context, _ int) (bool, error) {
			<-ctx.Done()
			time.Sleep(300 * time.Millisecond) // winding down: a loop that Run did not wait for would still be here when the pool closes
			sawClose.Store(poolIsClosed(lastPool.Load()))
			finished.Store(true)
			return false, ctx.Err()
		}}
		withReachabilitySetter(t, func(pool *pgxpool.Pool, _ *money.Store, cfg config.Config) reachabilitySetter {
			lastPool.Store(pool)
			pools.Add(1)
			return f
		})
		withVendorProvider(t, says(answer))
		r := startApp(t, base(closedPortDSN(t)))
		waitUntil(t, "the first write of the reachability loop", func() bool { return len(f.got()) >= 1 })
		if err := r.stop(t); err != nil {
			t.Fatalf("module 1 says %v: Run returned %v after the cancel", answer, err)
		}
		if !finished.Load() {
			t.Errorf("module 1 says %v: Run returned while a write was still in flight: the loop was not waited for", answer)
		}
		if sawClose.Load() {
			t.Errorf("module 1 says %v: the pool was already closed while the loop's last write was still running", answer)
		}
		if pools.Load() != 1 || lastPool.Load() == nil {
			t.Errorf("module 1 says %v: the setter was built %d times, want once from the service's pool", answer, pools.Load())
		}
		calls := f.got()
		if len(calls) != 1 || calls[0].unit != "m1_vendor" || calls[0].reachable != answer || calls[0].source != "m1_describe" {
			t.Errorf("module 1 says %v: the loop wrote %+v, want one write of m1_vendor=%v from module 1's describe", answer, calls, answer)
		}
		if l := find(r.logs.lines(t), msgReachabilityStarted); l == nil || l["source"] != "m1_describe" {
			t.Errorf("module 1 says %v: the start line is %v, want the source", answer, l)
		}
		// nothing runs after Run returned (paired with the wait for the first write above, which proves it was running)
		time.Sleep(100 * time.Millisecond)
		if n := len(f.got()); n != 1 {
			t.Errorf("module 1 says %v: the loop wrote again after Run returned (%d writes)", answer, n)
		}
	}
}

func TestRun_WithNoModuleOneNoReachabilityLoopRunsAndTheServiceSaysSo(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t)
	f := &fakeReach{}
	withReachabilitySetter(t, func(*pgxpool.Pool, *money.Store, config.Config) reachabilitySetter { return f })

	// no provider: module 1 cannot be asked in this process (the default environment sets neither PLATFORM_M1_URL nor PLATFORM_M1_KEY_SECRET)
	withVendorProvider(t, nil)
	r := startApp(t, base(closedPortDSN(t)))
	time.Sleep(300 * time.Millisecond)
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if n := len(f.got()); n != 0 {
		t.Errorf("%d write(s) with no module 1 to read: the loop ran on a flag nobody read", n)
	}
	lines := r.logs.lines(t)
	if find(lines, msgReachabilityStarted) != nil {
		t.Error("the loop announced itself with nothing to read")
	}
	if l := find(lines, "the reachability sync is not started: module 1 cannot be asked in this process"); l == nil || l["level"] != "INFO" {
		t.Errorf("the service did not say why there is no loop: %v", l)
	}

	// the control: with a provider the same service runs the loop and writes
	withVendorProvider(t, says(true))
	r2 := startApp(t, base(closedPortDSN(t)))
	waitUntil(t, "the first write of the loop", func() bool { return len(f.got()) >= 1 })
	if err := r2.stop(t); err != nil {
		t.Fatal(err)
	}
	if find(r2.logs.lines(t), msgReachabilityStarted) == nil {
		t.Error("control failed: the loop with a provider did not start")
	}
}

// A removed variable (config.RemovedVars). Setting it is ignored AND said: a warning that names the variable (and what replaced it), never its value; and it can no longer stop the process,
// whatever it holds (the variable of Phase 4 B3 was refused unless it was exactly true or false). The names come from the list: this file names none.
func TestRun_ARemovedVariableIsIgnoredWithAWarningThatNamesItAndNeverItsValue(t *testing.T) {
	removed := config.RemovedVars()
	if len(removed) == 0 {
		t.Fatal("the list of removed variables is empty: the loop below proves nothing")
	}
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t)
	withVendorProvider(t, nil)
	withReachabilitySetter(t, func(*pgxpool.Pool, *money.Store, config.Config) reachabilitySetter { return &fakeReach{} })
	const secretish = "zzzz-removed-value-zzzz"
	const warning = "a removed configuration variable is set and is ignored"

	for _, r := range removed {
		for _, value := range []string{"true", "false", "maybe-XYZ123", secretish} {
			run := startApp(t, merge(base(closedPortDSN(t)), map[string]string{r.Name: value}))
			if err := run.stop(t); err != nil {
				t.Errorf("%s=%q: Run returned %v: a removed variable must not stop the process", r.Name, value, err)
			}
			var warned []map[string]any
			for _, l := range run.logs.lines(t) {
				if l["msg"] == warning {
					warned = append(warned, l)
				}
			}
			if len(warned) != 1 || warned[0]["variable"] != r.Name || warned[0]["level"] != "WARN" || warned[0]["instead"] != r.Instead {
				t.Errorf("%s=%q: the warnings are %v, want exactly one WARN naming the variable and what replaced it", r.Name, value, warned)
			}
			if strings.Contains(run.logs.String(), secretish) || strings.Contains(run.logs.String(), "maybe-XYZ123") {
				t.Errorf("%s=%q: the value reached the log:\n%s", r.Name, value, run.logs.String())
			}
		}
		// the control: the same service with the variable unset says nothing of the kind
		run := startApp(t, base(closedPortDSN(t)))
		if err := run.stop(t); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(run.logs.String(), warning) || strings.Contains(run.logs.String(), r.Name) {
			t.Errorf("%s unset: the service warned of it anyway:\n%s", r.Name, run.logs.String())
		}
	}
	// a variable that is still live and invalid is still an error (the removed one is not special-cased into accepting everything)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := Run(ctx, envOf(merge(base(closedPortDSN(t)), map[string]string{config.VarJobTickMS: "maybe-XYZ123"})), &syncBuffer{})
	var ce *config.Error
	if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
		t.Errorf("control: an invalid live variable gave %v (exit %d), want exit status 2", err, ExitCode(err))
	}
}

// lastPool is the pool the service handed to the builder of the setter.
var lastPool atomic.Pointer[pgxpool.Pool]

// The reachability loop writes through THE money store of the process: the one newMoneyStore builds (waiting as long as PLATFORM_MONEY_LOCK_TIMEOUT_MS says) and apiDeps.Money keeps for the job
// store, not a second one of its own.
func TestDefaultReachabilitySetterIsTheOneMoneyStore(t *testing.T) {
	cfg, err := config.Load(envOf(base("postgres://platform_app:x@h/db")))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app:x@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st := newMoneyStore(pool, cfg)
	if st.LockTimeout() != cfg.MoneyLockTimeout {
		t.Errorf("the store waits %s, want PLATFORM_MONEY_LOCK_TIMEOUT_MS's %s", st.LockTimeout(), cfg.MoneyLockTimeout)
	}
	// the control: another value of the dial is another wait (the line above is not satisfied by a constant)
	cfg2, err := config.Load(envOf(merge(base("postgres://platform_app:x@h/db"), map[string]string{config.VarMoneyLockTimeoutMS: "2500"})))
	if err != nil {
		t.Fatal(err)
	}
	if got := newMoneyStore(pool, cfg2).LockTimeout(); got != 2500*time.Millisecond || got == st.LockTimeout() {
		t.Errorf("a configured lock timeout of 2.5s reached the store as %s", got)
	}
	// the default setter IS the store it is handed (one store, not a copy of it), and a store that was not handed is not made up
	if s, ok := newReachabilitySetter(pool, st, cfg).(*money.Store); !ok || s != st {
		t.Fatalf("the default builder returned %T (%p), want the very store it was given (%p)", s, s, st)
	}
	if other := newMoneyStore(pool, cfg); other == st {
		t.Fatal("two calls of newMoneyStore returned one store: the identity check above proves nothing")
	}
}
