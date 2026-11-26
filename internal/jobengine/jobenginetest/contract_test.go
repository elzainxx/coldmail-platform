package jobenginetest

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The contract is held to itself here: the fake satisfies it for each module, and each of twelve deliberately broken modules (a mutant of the fake) is reported, by the check that exists for that
// property and for the reason it names. A contract that no broken module can fail would be one more green nobody can read.

const contractJob = "0b000000-0000-4000-8000-000000000042"

func contractJobOf(module string, n int) jobs.EngineJob {
	return jobs.EngineJob{Job: jobs.Job{ID: contractJob, UserID: "a1a1a1a1-0000-4000-8000-000000000001", Module: module, Status: jobs.StatusConfirmed}, Rows: n, Pending: n}
}

// fakeSubject is the fake of this package as a Subject: Manual (nothing settles by itself), settled and taken down by the contract.
func fakeSubject(t *testing.T, module string, n int) *Subject {
	t.Helper()
	f := &FakeModule{Module: module, Manual: true}
	return &Subject{
		Module: f, Job: contractJobOf(module, n), Rows: Rows(n), Finals: FinalsOf(module),
		Settle: func(ref string, o charge.Outcome) {
			if !f.Settle(contractJob, ref, o) {
				t.Fatalf("the fake does not hold the item %s, or it is already final", ref)
			}
		},
		Down: f.SetDown,
	}
}

func TestContract_TheFakeSatisfiesItForEveryModule(t *testing.T) {
	ModuleContract(t, []string{jobs.ModuleVerify, jobs.ModuleSendManaged, jobs.ModuleSendBYO}, fakeSubject)
}

func TestContract_TheContractRefusesAnEmptySubjectAndHasItsNineChecks(t *testing.T) {
	// an instrument with no subject is red, never green: a contract with no module, no way to build a subject or a module that does not exist is not run
	if len(Checks()) < 9 {
		t.Fatalf("the contract has %d checks, it was written with 9", len(Checks()))
	}
	seen := map[string]bool{}
	for _, c := range Checks() {
		if c.Name == "" || c.Rows < 3 || c.Run == nil || seen[c.Name] {
			t.Errorf("the check %q is malformed or listed twice", c.Name)
		}
		seen[c.Name] = true
	}
	good := []string{jobs.ModuleVerify, jobs.ModuleSendManaged, jobs.ModuleSendBYO}
	for name, c := range map[string]struct {
		modules []string
		subject NewSubject
	}{"no module": {nil, fakeSubject}, "no subject": {good, nil}, "a module that does not exist": {[]string{"m4"}, fakeSubject}} {
		if contractProblem(c.modules, c.subject) == "" {
			t.Errorf("%s: the contract would have run and said OK over nothing", name)
		}
	}
	if why := contractProblem(good, fakeSubject); why != "" { // the control: a well-formed request is not refused
		t.Errorf("a well-formed contract was refused: %s", why)
	}
}

// ---- the mutants ------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// wrap is a Module whose methods can each be replaced; the zero replacement passes the call through.
type wrap struct {
	jobengine.Module
	submit  func(next jobengine.Module, ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error)
	observe func(next jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error)
	cancel  func(next jobengine.Module, ctx context.Context, j jobs.EngineJob) error
}

func (w *wrap) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
	if w.submit != nil {
		return w.submit(w.Module, ctx, j, rows)
	}
	return w.Module.Submit(ctx, j, rows)
}

func (w *wrap) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
	if w.observe != nil {
		return w.observe(w.Module, ctx, j, fn)
	}
	return w.Module.Observe(ctx, j, fn)
}

func (w *wrap) Cancel(ctx context.Context, j jobs.EngineJob) error {
	if w.cancel != nil {
		return w.cancel(w.Module, ctx, j)
	}
	return w.Module.Cancel(ctx, j)
}

func TestContract_EveryBrokenModuleIsReportedForTheReasonItBreaks(t *testing.T) {
	type mutant struct {
		name   string
		module string
		check  func(ctx context.Context, module string, s *Subject) []string
		rows   int
		make   func(s *Subject, f *FakeModule) // changes the subject into the broken one
		want   string                          // what the check must say
	}
	var down atomic.Bool
	mutants := []mutant{
		{"hands over an item that is not final (an invented outcome)", jobs.ModuleSendManaged, CheckObserveSettled, 6,
			func(s *Subject, f *FakeModule) { f.LeakNotFinal = true }, "an invented outcome"},
		{"accepts a row nobody offered", jobs.ModuleSendBYO, CheckSubmit, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, submit: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
					r, err := n.Submit(ctx, j, rows)
					r.Accepted = append(r.Accepted, "ffffffff-ffff-4fff-8fff-ffffffffffff")
					return r, err
				}}
			}, "a row nobody offered"},
		{"module 1 without its reference", jobs.ModuleVerify, CheckSubmit, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, submit: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
					r, err := n.Submit(ctx, j, rows)
					r.ModuleRef = ""
					return r, err
				}}
			}, "returned no module reference"},
		{"a sender with a reference", jobs.ModuleSendManaged, CheckSubmit, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, submit: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
					r, err := n.Submit(ctx, j, rows)
					r.ModuleRef = "a-handle"
					return r, err
				}}
			}, "returned a module reference"},
		{"forgets an item on the second observation", jobs.ModuleSendManaged, CheckObserveIdempotent, 6,
			func(s *Subject, f *FakeModule) {
				calls := 0
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					calls++
					return n.Observe(ctx, j, func(items []jobs.ResultItem) error {
						if calls == 2 && len(items) > 0 {
							items = items[1:]
						}
						return fn(items)
					})
				}}
			}, "the second observation: the settled item"},
		{"swallows the error of the callback", jobs.ModuleSendManaged, CheckCallbackError, 3,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					return n.Observe(ctx, j, func(items []jobs.ResultItem) error { _ = fn(items); return nil })
				}}
			}, "answered no error"},
		{"module 1 that cancels", jobs.ModuleVerify, CheckCancel, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, cancel: func(jobengine.Module, context.Context, jobs.EngineJob) error { return nil }}
			}, "module 1 has no cancel"},
		{"an outage that is a plain error", jobs.ModuleSendBYO, CheckOutage, 4,
			func(s *Subject, f *FakeModule) {
				down.Store(false)
				plain := errors.New("connection refused")
				s.Module = &wrap{Module: f,
					submit: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
						if down.Load() {
							return jobengine.SubmitResult{}, plain
						}
						return n.Submit(ctx, j, rows)
					},
					observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
						if down.Load() {
							return jobengine.Observation{}, plain
						}
						return n.Observe(ctx, j, fn)
					},
				}
				s.Down = down.Store
			}, "want ErrWait"},
		{"module 1 that remembers its handle instead of reading it from the job", jobs.ModuleVerify, CheckObserveReference, 3,
			func(s *Subject, f *FakeModule) {
				var handle string
				s.Module = &wrap{Module: f,
					submit: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
						r, err := n.Submit(ctx, j, rows)
						handle = r.ModuleRef
						return r, err
					},
					observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
						j.ModuleRef = handle // what the process-local cache did: right until the process restarts or another instance leads
						return n.Observe(ctx, j, fn)
					}}
			}, "the handle comes from the job"},
		{"complete too early", jobs.ModuleSendManaged, CheckObserveNothingFinal, 4,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					o, err := n.Observe(ctx, j, fn)
					o.Complete = true
					return o, err
				}}
			}, "is called complete while no item is final"},
		{"changes the outcome of an item", jobs.ModuleSendManaged, CheckObserveComplete, 4,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					return n.Observe(ctx, j, func(items []jobs.ResultItem) error {
						for i := range items {
							items[i].Outcome.Reason = "invented"
						}
						return fn(items)
					})
				}}
			}, "the module reported"},
		{"hands an item over twice in one observation", jobs.ModuleSendBYO, CheckObserveComplete, 4,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					return n.Observe(ctx, j, func(items []jobs.ResultItem) error { return fn(append(items, items[0])) })
				}}
			}, "handed over twice"},
	}
	for _, m := range mutants {
		t.Run(m.name, func(t *testing.T) {
			// the control: the same check on the unbroken fake is clean
			if p := m.check(context.Background(), m.module, fakeSubject(t, m.module, m.rows)); len(p) != 0 {
				t.Fatalf("the control: the check reports problems on the fake itself: %v", p)
			}
			f := &FakeModule{Module: m.module, Manual: true}
			s := &Subject{Module: f, Job: contractJobOf(m.module, m.rows), Rows: Rows(m.rows), Finals: FinalsOf(m.module), Down: f.SetDown,
				Settle: func(ref string, o charge.Outcome) { f.Settle(contractJob, ref, o) }}
			m.make(s, f)
			problems := m.check(context.Background(), m.module, s)
			if len(problems) == 0 {
				t.Fatalf("the broken module (%s) passed the check", m.name)
			}
			if !strings.Contains(strings.Join(problems, "\n"), m.want) {
				t.Fatalf("the check reported %v, which does not say %q", problems, m.want)
			}
		})
	}
	if len(mutants) < 12 {
		t.Fatalf("%d mutants: the contract was proved against 12", len(mutants))
	}
}
