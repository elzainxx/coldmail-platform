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

// The contract is held to itself here: the fake satisfies it for each module, and each of twenty-one deliberately broken modules (a mutant of the fake) is reported, by the check that exists for that
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
		Down:  f.SetDown,
		Claim: func(ref string) bool { return f.Claim(contractJob, ref) },
	}
}

func TestContract_TheFakeSatisfiesItForEveryModule(t *testing.T) {
	ModuleContract(t, []string{jobs.ModuleVerify, jobs.ModuleSendManaged, jobs.ModuleSendBYO}, fakeSubject)
}

func TestContract_TheContractRefusesAnEmptySubjectAndHasItsElevenChecks(t *testing.T) {
	// an instrument with no subject is red, never green: a contract with no module, no way to build a subject or a module that does not exist is not run
	if len(Checks()) < 11 {
		t.Fatalf("the contract has %d checks, it was written with 11 (nine, then the pause and the cancel of a held campaign)", len(Checks()))
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
	pause   func(next jobengine.Module, ctx context.Context, j jobs.EngineJob) error
	resume  func(next jobengine.Module, ctx context.Context, j jobs.EngineJob) error
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

func (w *wrap) Pause(ctx context.Context, j jobs.EngineJob) error {
	if w.pause != nil {
		return w.pause(w.Module, ctx, j)
	}
	return w.Module.Pause(ctx, j)
}

func (w *wrap) Resume(ctx context.Context, j jobs.EngineJob) error {
	if w.resume != nil {
		return w.resume(w.Module, ctx, j)
	}
	return w.Module.Resume(ctx, j)
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
		{"module 1 that cannot cancel (its route is older than the platform's seed)", jobs.ModuleVerify, CheckCancel, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, cancel: func(jobengine.Module, context.Context, jobs.EngineJob) error { return jobengine.ErrNotSupported }}
			}, "cancel failed"},
		{"module 1 that words a cancelled row with no reason (the charge table's row M1-24 names cancelled_by_caller)", jobs.ModuleVerify, CheckCancel, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					return n.Observe(ctx, j, func(items []jobs.ResultItem) error {
						for i := range items {
							if items[i].Outcome.State == "cancelled" {
								items[i].Outcome.Reason = ""
							}
						}
						return fn(items)
					})
				}}
			}, "the module reported"},
		{"a module whose worker claims while the campaign is held", jobs.ModuleSendManaged, CheckPauseResume, 5,
			func(s *Subject, f *FakeModule) {
				s.Claim = func(ref string) bool { // the worker ignores the hold: it lets go, claims and holds again
					_ = f.Resume(context.Background(), s.Job)
					ok := f.Claim(contractJob, ref)
					_ = f.Pause(context.Background(), s.Job)
					return ok
				}
			}, "claimed the queued item"},
		{"a held module that invents an outcome for a row it never claimed", jobs.ModuleSendBYO, CheckPauseResume, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					o, err := n.Observe(ctx, j, fn)
					if err == nil && o.Paused {
						_ = fn([]jobs.ResultItem{{ItemRef: s.Rows[3].ID, Outcome: charge.Outcome{Module: jobs.ModuleSendBYO, State: StateSent}}})
					}
					return o, err
				}}
			}, "an invented outcome"},
		{"a module whose report never says paused", jobs.ModuleSendManaged, CheckPauseResume, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
					o, err := n.Observe(ctx, j, fn)
					o.Paused = false
					return o, err
				}}
			}, "does not say paused"},
		{"a pause that is an error the second time", jobs.ModuleSendBYO, CheckPauseResume, 5,
			func(s *Subject, f *FakeModule) {
				calls := 0
				s.Module = &wrap{Module: f, pause: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob) error {
					calls++
					if calls > 1 {
						return errors.New("already paused")
					}
					return n.Pause(ctx, j)
				}}
			}, "a second pause of a held campaign failed"},
		{"a resume that cancels the campaign instead", jobs.ModuleSendManaged, CheckPauseResume, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, resume: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob) error { return n.Cancel(ctx, j) }}
			}, "after the resume"},
		{"a module 1 that supports pause", jobs.ModuleVerify, CheckPauseResume, 5,
			func(s *Subject, f *FakeModule) {
				s.Module = &wrap{Module: f, pause: func(jobengine.Module, context.Context, jobs.EngineJob) error { return nil }}
			}, "module 1 has no per-job pause"},
		{"a cancel that leaves the campaign held", jobs.ModuleSendManaged, CheckCancelWhileHeld, 5,
			func(s *Subject, f *FakeModule) {
				cancelled := false
				s.Module = &wrap{Module: f,
					cancel: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob) error {
						cancelled = true
						return n.Cancel(ctx, j)
					},
					observe: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
						o, err := n.Observe(ctx, j, fn)
						if cancelled {
							o.Paused = true // the hold survives the cancel
						}
						return o, err
					}}
			}, "a cancel ends the hold"},
		{"a pause under an outage that is a plain error", jobs.ModuleSendBYO, CheckOutage, 4,
			func(s *Subject, f *FakeModule) {
				down.Store(false)
				plain := errors.New("connection refused")
				s.Module = &wrap{Module: f, pause: func(n jobengine.Module, ctx context.Context, j jobs.EngineJob) error {
					if down.Load() {
						return plain
					}
					return n.Pause(ctx, j)
				}}
				s.Down = func(d bool) { down.Store(d); f.SetDown(d) }
			}, "pause of a module that is down answered"},
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
				Settle: func(ref string, o charge.Outcome) { f.Settle(contractJob, ref, o) },
				Claim:  func(ref string) bool { return f.Claim(contractJob, ref) }}
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
	if len(mutants) < 21 {
		t.Fatalf("%d mutants: the contract was proved against 21", len(mutants))
	}
}
