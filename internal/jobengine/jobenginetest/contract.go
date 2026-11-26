package jobenginetest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// THE MODULE CONTRACT. One set of properties every implementation of jobengine.Module must have, run against the fake of this package AND against the real wrappers over the adapters
// (internal/jobengine/modules_contract_test.go): the fake is only worth what it is held to, and the wrappers are only worth what they are held to, so both are held to the same text. A property that
// one of them cannot show is a difference between the fake and the real module, which is exactly what this exists to find.
//
// The properties, each a check that returns its problems as sentences (so that a test can run the SAME check on a deliberately broken module and require that it reports the reason: a check that
// cannot be shown to fail is not a check; contract_test.go does that with six mutants):
//
//	submit    every offered row is accepted exactly once and nothing else is; module 1 returns its own reference and the senders none; a replay is the same answer
//	observe   nothing is final at first (nothing is handed over, not complete); what the module settled is handed over with the module's own outcome (module, state, reason) and nothing else:
//	          no item that is not final, no outcome the module did not report; the totals are the module's (Total, Terminal, Complete); a second observation says the same; once the engine has
//	          recorded what it was handed, a further observation adds nothing and still says the same totals; every row settled means complete
//	reference the job handed to Observe is the job as the store reads it: after a Submit the engine's Start records the module's handle (module 1's job id) and the next read of the job carries it
//	          (jobs.EngineJob.ModuleRef). The contract hands Observe exactly that, nothing more, and module 1 must read the handle from it: with none, or another, it is ErrWait and hands over nothing
//	          (a module that remembers the handle itself would pass every other check and fail the first restart); a sender has no handle and does not need one
//	callback  an error of the engine's callback ends the observation and comes back as it was (errors.Is), never taken for an answer of the module
//	cancel    module 1 is ErrNotSupported; a sender takes its queued items back as `cancelled` (one result each, once), leaves what already settled alone and is idempotent
//	outage    a module that cannot answer is ErrWait on every call, changes nothing, and what it reports after it is back is what it reported before

// Subject is one Module under the contract with the levers the contract needs on the module's own side.
type Subject struct {
	// Module is what is tested.
	Module jobengine.Module
	// Job is the job the contract submits: its id, its owner, its module, Pending equal to the number of Rows and nothing submitted yet.
	Job jobs.EngineJob
	// Rows are the rows of the job, in order.
	Rows []jobs.EngineRow
	// Finals are the outcomes the module can settle an item with (the module's own words: module 1 a verdict, a sender sent, bounced, failed); the contract settles row i with Finals[i mod len].
	Finals []charge.Outcome
	// Settle is the module's worker finishing ONE item (by the row's id) with this outcome. Nothing else settles by itself.
	Settle func(ref string, o charge.Outcome)
	// Down makes the module unreachable (true) or reachable again (false).
	Down func(down bool)
}

// NewSubject builds a Subject for module ("m1", "m2" or "m3") with n rows, fresh for one subtest.
type NewSubject func(t *testing.T, module string, n int) *Subject

// Rows returns n synthetic rows (uuids and example.test addresses), the shape of what the store hands the engine.
func Rows(n int) []jobs.EngineRow {
	out := make([]jobs.EngineRow, n)
	for i := range out {
		out[i] = jobs.EngineRow{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Ordinal: i + 1, EmailNorm: fmt.Sprintf("row%d@example.test", i+1)}
	}
	return out
}

// FinalsOf is the outcomes a module can settle an item with, by module id: the vocabulary of the charge table's rows for it.
func FinalsOf(module string) []charge.Outcome {
	switch module {
	case jobs.ModuleVerify:
		return []charge.Outcome{
			{Module: "m1", State: "valid", Reason: "accepted"},
			{Module: "m1", State: "invalid", Reason: "mailbox_not_found"},
			{Module: "m1", State: "risky", Reason: "catch_all"},
		}
	case jobs.ModuleSendManaged:
		return []charge.Outcome{
			{Module: "m2", State: "sent"},
			{Module: "m2", State: "bounced", Reason: "bounce_permanent"},
			{Module: "m2", State: "failed", Reason: "attempts_exhausted"},
		}
	}
	return []charge.Outcome{{Module: "m3", State: "sent"}, {Module: "m3", State: "bounced"}, {Module: "m3", State: "failed"}}
}

// ModuleContract runs the contract for each of modules ("m1", "m2", "m3") over subjects made by newSubject.
func ModuleContract(t *testing.T, modules []string, newSubject NewSubject) {
	t.Helper()
	if why := contractProblem(modules, newSubject); why != "" {
		t.Fatal(why)
	}
	for _, m := range modules {
		t.Run(m, func(t *testing.T) {
			for _, c := range Checks() {
				t.Run(c.Name, func(t *testing.T) {
					s := newSubject(t, m, c.Rows)
					if s == nil || s.Module == nil || len(s.Rows) != c.Rows {
						t.Fatalf("the subject for %s is missing, or holds %d rows and the check needs %d", m, func() int {
							if s == nil {
								return 0
							}
							return len(s.Rows)
						}(), c.Rows)
					}
					for _, p := range c.Run(context.Background(), m, s) {
						t.Errorf("%s: %s", c.Name, p)
					}
				})
			}
		})
	}
}

// contractProblem says why a contract cannot be run (a gate whose subject is empty is red: an instrument that matched nothing must not say OK), or "".
func contractProblem(modules []string, newSubject NewSubject) string {
	switch {
	case len(modules) == 0:
		return "the contract was given no module to run for: the instrument matched nothing"
	case newSubject == nil:
		return "the contract was given no way to build a subject: the instrument matched nothing"
	}
	for _, m := range modules {
		switch m {
		case jobs.ModuleVerify, jobs.ModuleSendManaged, jobs.ModuleSendBYO:
		default:
			return fmt.Sprintf("the contract was asked for module %q, which is none of m1, m2, m3", m)
		}
	}
	return ""
}

// Check is one property of the contract.
type Check struct {
	Name string
	Rows int // how many rows its subject has
	Run  func(ctx context.Context, module string, s *Subject) []string
}

// Checks are the properties, in order. A test of the contract itself runs each against a broken module.
func Checks() []Check {
	return []Check{
		{"submit accepts exactly the offered rows and a replay is the same", 5, CheckSubmit},
		{"observe: nothing is final at first", 4, CheckObserveNothingFinal},
		{"observe: what settled is handed over with the module's own outcome and nothing else", 6, CheckObserveSettled},
		{"observe: a second observation says the same, and what was recorded is not needed again", 6, CheckObserveIdempotent},
		{"observe: every row settled means complete", 4, CheckObserveComplete},
		{"observe: module 1 reads its handle from the job it is handed", 3, CheckObserveReference},
		{"observe: the error of the callback comes back as it was", 3, CheckCallbackError},
		{"cancel", 5, CheckCancel},
		{"outage: ErrWait on every call and nothing changes", 4, CheckOutage},
	}
}

// ---- helpers ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

type handed struct {
	items map[string]charge.Outcome // by item reference, the outcome as handed over
	dups  []string                  // item references handed over more than once in one observation
	obs   jobengine.Observation
}

// observe runs one Observe and collects what the callback was handed. recorded is the number of results the engine has recorded (EngineJob.PendingResults).
func observe(ctx context.Context, s *Subject, recorded int) (handed, error) {
	h := handed{items: map[string]charge.Outcome{}}
	j := s.Job
	j.PendingResults = recorded
	obs, err := s.Module.Observe(ctx, j, func(items []jobs.ResultItem) error {
		for _, it := range items {
			if _, dup := h.items[it.ItemRef]; dup {
				h.dups = append(h.dups, it.ItemRef)
			}
			h.items[it.ItemRef] = it.Outcome
		}
		return nil
	})
	h.obs = obs
	return h, err
}

// submit offers rows as the engine does and then does what the engine's Start does with the answer: the module's handle, when it gave one, is what the next read of the job carries (s.Job.ModuleRef),
// and every later Observe of the contract is handed the job with it, never more.
func submit(ctx context.Context, s *Subject, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
	j := s.Job
	j.Pending = len(s.Rows)
	res, err := s.Module.Submit(ctx, j, rows)
	if err == nil && res.ModuleRef != "" {
		s.Job.ModuleRef = res.ModuleRef
	}
	return res, err
}

// settleFirst settles the first k rows (row i with Finals[i mod len]) and returns what the module must report for them.
func settleFirst(s *Subject, k int) map[string]charge.Outcome {
	want := map[string]charge.Outcome{}
	for i := 0; i < k; i++ {
		o := s.Finals[i%len(s.Finals)]
		s.Settle(s.Rows[i].ID, o)
		want[s.Rows[i].ID] = o
	}
	return want
}

func setOf(ids []string) map[string]int {
	m := map[string]int{}
	for _, id := range ids {
		m[id]++
	}
	return m
}

func rowIDs(rows []jobs.EngineRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func sorted(m map[string]charge.Outcome) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diffOutcomes names how got differs from want, item by item.
func diffOutcomes(got, want map[string]charge.Outcome) []string {
	var p []string
	for _, ref := range sorted(want) {
		g, ok := got[ref]
		switch {
		case !ok:
			p = append(p, "the settled item "+ref+" was not handed over")
		case g != want[ref]:
			p = append(p, fmt.Sprintf("the item %s was handed over as %+v, the module reported %+v", ref, g, want[ref]))
		}
	}
	for _, ref := range sorted(got) {
		if _, ok := want[ref]; !ok {
			p = append(p, fmt.Sprintf("the item %s was handed over as %+v and the module reported it as nothing final: an invented outcome", ref, got[ref]))
		}
	}
	return p
}

func submitAll(ctx context.Context, s *Subject) []string {
	res, err := submit(ctx, s, s.Rows)
	var p []string
	if err != nil {
		return []string{"submit of every row failed: " + err.Error()}
	}
	if got := setOf(res.Accepted); len(got) != len(s.Rows) || len(res.Accepted) != len(s.Rows) {
		p = append(p, fmt.Sprintf("submit answered %d accepted row(s) (%d distinct) for %d offered", len(res.Accepted), len(got), len(s.Rows)))
	}
	return p
}

// ---- the checks -----------------------------------------------------------------------------------------------------------------------------------------------------------------------

// CheckSubmit: every offered row is accepted exactly once and nothing else; module 1 returns a reference and the senders none; a replay answers the same.
func CheckSubmit(ctx context.Context, module string, s *Subject) []string {
	var p []string
	res, err := submit(ctx, s, s.Rows)
	if err != nil {
		return []string{"submit failed: " + err.Error()}
	}
	want := setOf(rowIDs(s.Rows))
	got := setOf(res.Accepted)
	if len(res.Accepted) != len(s.Rows) {
		p = append(p, fmt.Sprintf("submit accepted %d row(s) of %d offered", len(res.Accepted), len(s.Rows)))
	}
	for id := range want {
		if got[id] != 1 {
			p = append(p, fmt.Sprintf("the row %s is accepted %d time(s), want exactly once", id, got[id]))
		}
	}
	for id := range got {
		if want[id] == 0 {
			p = append(p, "submit accepted a row nobody offered: "+id)
		}
	}
	switch {
	case module == jobs.ModuleVerify && strings.TrimSpace(res.ModuleRef) == "":
		p = append(p, "module 1's submit returned no module reference")
	case module != jobs.ModuleVerify && res.ModuleRef != "":
		p = append(p, "a sender's submit returned a module reference: it has no handle for the job")
	}
	// a replay of the same rows is the same answer (module 1: the same job under the same key; the senders: the same items)
	res2, err := submit(ctx, s, s.Rows)
	switch {
	case err != nil:
		p = append(p, "the replay of a submit failed: "+err.Error())
	case res2.ModuleRef != res.ModuleRef:
		p = append(p, fmt.Sprintf("the replay of a submit answered another module reference (%q, then %q): it made a second module job", res.ModuleRef, res2.ModuleRef))
	default:
		g2 := setOf(res2.Accepted)
		if len(g2) != len(got) || len(res2.Accepted) != len(res.Accepted) {
			p = append(p, fmt.Sprintf("the replay accepted %d row(s), the first call %d", len(res2.Accepted), len(res.Accepted)))
		}
		for id := range got {
			if g2[id] != 1 {
				p = append(p, "the replay did not hold the row "+id)
			}
		}
	}
	// an empty offer holds nothing and fails nothing
	if res3, err := submit(ctx, s, nil); err != nil || len(res3.Accepted) != 0 {
		p = append(p, fmt.Sprintf("an empty offer answered %d row(s) and %v", len(res3.Accepted), err))
	}
	return p
}

// CheckObserveNothingFinal: before any item is final, nothing is handed over and the module is not complete.
func CheckObserveNothingFinal(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	h, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"observe failed: " + err.Error()}
	}
	var p []string
	if len(h.items) != 0 {
		p = append(p, fmt.Sprintf("observe handed over %d item(s) while none is final: %v", len(h.items), sorted(h.items)))
	}
	if h.obs.Complete {
		p = append(p, "the module is called complete while no item is final")
	}
	if h.obs.Terminal != 0 {
		p = append(p, fmt.Sprintf("the observation counts %d terminal item(s) while none is final", h.obs.Terminal))
	}
	if h.obs.Total != len(s.Rows) {
		p = append(p, fmt.Sprintf("the observation says the module holds %d item(s), it holds %d", h.obs.Total, len(s.Rows)))
	}
	return p
}

// CheckObserveSettled: what the module settled is handed over with its own outcome, nothing else, and the totals are the module's.
func CheckObserveSettled(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	k := len(s.Rows) - 2 // some settled, two not
	want := settleFirst(s, k)
	h, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"observe failed: " + err.Error()}
	}
	p := diffOutcomes(h.items, want)
	if len(h.dups) > 0 {
		p = append(p, fmt.Sprintf("an item was handed over twice in one observation: %v", h.dups))
	}
	switch {
	case h.obs.Terminal != k:
		p = append(p, fmt.Sprintf("the observation counts %d terminal item(s), the module settled %d", h.obs.Terminal, k))
	case h.obs.Total != len(s.Rows):
		p = append(p, fmt.Sprintf("the observation says the module holds %d item(s), it holds %d", h.obs.Total, len(s.Rows)))
	case h.obs.Complete:
		p = append(p, "the module is called complete with two items not final")
	}
	return p
}

// CheckObserveIdempotent: observing again says the same; and once the engine has recorded what it was handed, observing again hands over nothing it needs and says the same totals.
func CheckObserveIdempotent(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	k := len(s.Rows) - 3
	want := settleFirst(s, k)
	var p []string
	first, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"observe failed: " + err.Error()}
	}
	second, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"the second observe failed: " + err.Error()}
	}
	for _, d := range diffOutcomes(second.items, want) {
		p = append(p, "the second observation: "+d)
	}
	if second.obs.Terminal != first.obs.Terminal || second.obs.Total != first.obs.Total || second.obs.Complete != first.obs.Complete {
		p = append(p, fmt.Sprintf("the second observation says %+v, the first said %+v", second.obs, first.obs))
	}
	// the engine recorded k results: what it is handed now is at most what it already has, and the totals are unchanged
	third, err := observe(ctx, s, len(first.items))
	if err != nil {
		return append(p, "the observe after the engine recorded failed: "+err.Error())
	}
	for ref, o := range third.items {
		if w, ok := want[ref]; !ok || w != o {
			p = append(p, fmt.Sprintf("after the engine recorded its results the item %s was handed over as %+v, which the module did not report", ref, o))
		}
	}
	if third.obs.Terminal != first.obs.Terminal || third.obs.Total != first.obs.Total || third.obs.Complete != first.obs.Complete {
		p = append(p, fmt.Sprintf("the observation after recording says %+v, the first said %+v", third.obs, first.obs))
	}
	// one more item settles: the engine is handed it (and may be handed the others again)
	more := s.Rows[k]
	o := s.Finals[k%len(s.Finals)]
	s.Settle(more.ID, o)
	want[more.ID] = o
	fourth, err := observe(ctx, s, len(first.items))
	if err != nil {
		return append(p, "the observe after one more item settled failed: "+err.Error())
	}
	if got, ok := fourth.items[more.ID]; !ok || got != o {
		p = append(p, fmt.Sprintf("the item that settled after the engine's last record was handed over as %+v (present %v), want %+v", got, ok, o))
	}
	for ref, g := range fourth.items {
		if w, ok := want[ref]; !ok || w != g {
			p = append(p, fmt.Sprintf("the item %s was handed over as %+v, which the module did not report", ref, g))
		}
	}
	if fourth.obs.Terminal != k+1 {
		p = append(p, fmt.Sprintf("the observation counts %d terminal item(s), the module settled %d", fourth.obs.Terminal, k+1))
	}
	return p
}

// CheckObserveComplete: every row settled means complete, with every row handed over once and the totals equal.
func CheckObserveComplete(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	want := settleFirst(s, len(s.Rows))
	h, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"observe failed: " + err.Error()}
	}
	p := diffOutcomes(h.items, want)
	if !h.obs.Complete {
		p = append(p, "every item is settled and the module is not called complete")
	}
	if h.obs.Terminal != len(s.Rows) || h.obs.Total != len(s.Rows) {
		p = append(p, fmt.Sprintf("the observation says total %d, terminal %d, for %d settled item(s)", h.obs.Total, h.obs.Terminal, len(s.Rows)))
	}
	if len(h.dups) > 0 {
		p = append(p, fmt.Sprintf("an item was handed over twice in one observation: %v", h.dups))
	}
	return p
}

// CheckObserveReference: module 1 finds its own id of the job on the job it is handed (the engine's Start recorded it, the store read it back); handed none, or another, it waits and hands over
// nothing. A sender has no handle: nothing to check for it.
func CheckObserveReference(ctx context.Context, module string, s *Subject) []string {
	if module != jobs.ModuleVerify {
		return nil
	}
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	if s.Job.ModuleRef == "" {
		return []string{"module 1's submit answered no module reference, so there is nothing to hand to Observe"}
	}
	want := settleFirst(s, len(s.Rows))
	h, err := observe(ctx, s, 0) // the control: with the handle the engine's store would give it, the module reports what it settled
	if err != nil {
		return []string{"observe of a job that carries its module reference failed: " + err.Error()}
	}
	p := diffOutcomes(h.items, want)
	saved := s.Job.ModuleRef
	defer func() { s.Job.ModuleRef = saved }()
	for _, c := range []struct{ what, ref string }{{"no module reference", ""}, {"a module reference that is not the module's", "00000000-0000-4000-8000-0000000000ff"}} {
		s.Job.ModuleRef = c.ref
		got, err := observe(ctx, s, 0)
		if !errors.Is(err, jobengine.ErrWait) || len(got.items) != 0 {
			p = append(p, fmt.Sprintf("observe of a job with %s answered %v and handed over %d item(s): want ErrWait and nothing (the handle comes from the job, not from the module's memory)", c.what, err, len(got.items)))
		}
	}
	return p
}

// errCallback is the error the callback returns in CheckCallbackError.
var errCallback = errors.New("the engine's callback failed")

// CheckCallbackError: an error of the callback ends the observation and is returned as it was.
func CheckCallbackError(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	settleFirst(s, len(s.Rows))
	calls := 0
	_, err := s.Module.Observe(ctx, s.Job, func([]jobs.ResultItem) error {
		calls++
		return errCallback
	})
	var p []string
	switch {
	case calls == 0:
		p = append(p, "the callback was never called although items are settled")
	case err == nil:
		p = append(p, "the callback failed and observe answered no error: a failed record would be taken for a recorded one")
	case !errors.Is(err, errCallback):
		p = append(p, "the callback's error did not come back as it was: "+err.Error())
	case errors.Is(err, jobengine.ErrWait) || errors.Is(err, jobengine.ErrRefused):
		p = append(p, "the callback's error was taken for an answer of the module: "+err.Error())
	}
	return p
}

// CheckCancel: module 1 cannot; a sender takes its queued items back, leaves what settled alone, and a repeat changes nothing.
func CheckCancel(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	if module == jobs.ModuleVerify {
		if err := s.Module.Cancel(ctx, s.Job); !errors.Is(err, jobengine.ErrNotSupported) {
			return []string{fmt.Sprintf("module 1 has no cancel and answered %v: want ErrNotSupported", err)}
		}
		return nil
	}
	want := settleFirst(s, 1)
	var p []string
	if err := s.Module.Cancel(ctx, s.Job); err != nil {
		return []string{"cancel failed: " + err.Error()}
	}
	for _, r := range s.Rows[1:] {
		want[r.ID] = charge.Outcome{Module: module, State: "cancelled"}
	}
	h, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"observe after the cancel failed: " + err.Error()}
	}
	h.items = withoutCancelReason(h.items)
	p = append(p, diffOutcomes(h.items, want)...)
	if !h.obs.Complete || h.obs.Terminal != len(s.Rows) {
		p = append(p, fmt.Sprintf("after the cancel every item is final and the observation says complete %v, terminal %d of %d", h.obs.Complete, h.obs.Terminal, len(s.Rows)))
	}
	if err := s.Module.Cancel(ctx, s.Job); err != nil {
		p = append(p, "a second cancel failed: "+err.Error())
	}
	again, err := observe(ctx, s, 0)
	if err != nil {
		return append(p, "observe after the second cancel failed: "+err.Error())
	}
	for _, d := range diffOutcomes(withoutCancelReason(again.items), want) {
		p = append(p, "after the second cancel: "+d)
	}
	return p
}

// withoutCancelReason sets aside the one word a module adds to a cancelled item: module 2 reports `cancelled` items with the reason `cancelled`, module 3 and the fake with none. The charge
// table rules a cancelled item free whatever its reason (M2-08, M3-01), so for the contract the item is cancelled either way.
func withoutCancelReason(in map[string]charge.Outcome) map[string]charge.Outcome {
	out := make(map[string]charge.Outcome, len(in))
	for ref, o := range in {
		if o.State == "cancelled" && o.Reason == "cancelled" {
			o.Reason = ""
		}
		out[ref] = o
	}
	return out
}

// CheckOutage: a module that cannot answer is ErrWait on submit, observe and (for a sender) cancel; it changes nothing, and what it says when it is back is what it said before.
func CheckOutage(ctx context.Context, module string, s *Subject) []string {
	if p := submitAll(ctx, s); len(p) > 0 {
		return p
	}
	want := settleFirst(s, 2)
	before, err := observe(ctx, s, 0)
	if err != nil {
		return []string{"observe failed: " + err.Error()}
	}
	var p []string
	s.Down(true)
	if _, err := submit(ctx, s, s.Rows); !errors.Is(err, jobengine.ErrWait) {
		p = append(p, fmt.Sprintf("submit to a module that is down answered %v: want ErrWait", err))
	}
	if h, err := observe(ctx, s, 0); !errors.Is(err, jobengine.ErrWait) || len(h.items) != 0 {
		p = append(p, fmt.Sprintf("observe of a module that is down answered %v and handed over %d item(s): want ErrWait and nothing", err, len(h.items)))
	}
	if module != jobs.ModuleVerify {
		if err := s.Module.Cancel(ctx, s.Job); !errors.Is(err, jobengine.ErrWait) {
			p = append(p, fmt.Sprintf("cancel of a module that is down answered %v: want ErrWait", err))
		}
	}
	s.Down(false)
	after, err := observe(ctx, s, 0)
	if err != nil {
		return append(p, "observe after the outage failed: "+err.Error())
	}
	for _, d := range diffOutcomes(after.items, want) {
		p = append(p, "after the outage: "+d)
	}
	if after.obs.Terminal != before.obs.Terminal || after.obs.Total != before.obs.Total || after.obs.Complete != before.obs.Complete {
		p = append(p, fmt.Sprintf("the outage changed what the module says: %+v before, %+v after", before.obs, after.obs))
	}
	return p
}
