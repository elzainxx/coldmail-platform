//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 close-out D1 (live): the pause and resume of a RUNNING module 2 / module 3 job and the cancel of a paused one, through the job engine of THIS process, against the REAL modules. The scenario is written ONCE here
// (jlpcRunPauseResume, jlpcRunCancelPaused) over a jlpcRig, and the two modules supply the rig: jobs_live_pause_m3_test.go (module 3, tag harness) and m2_worker_jobs_pause_test.go (module 2, tag m2worker). Every
// name of this file and of the three files that use it starts with `jlpc` so that it cannot clash with another agent's file.
//
// WHAT THE SCENARIO NEEDS OF A SENDER MODULE: a way to make a tenant (a user, an account, credits or an entitlement) and its recipients, to put the sink in "hold every send" mode, to read the module's OWN campaign report
// and the sink (messages and RCPT answers per recipient, and the sends it is holding right now), and the module's own three-way comparison of an ended job. Nothing else differs between the two senders: the platform
// side (the store, the engine, the lifecycle) is the same code, which is the point.
//
// THE ENGINE IS THE PRODUCTION ONE with two dials changed for what is being proved (jlpcEngineConfig): a SMALL submit batch, so that the job is paused while rows are NOT yet handed to the module, and a TINY staleness
// limit (jlpcStale), so that "a held job is never stale" can fail. The batch works like this and no other way: the starting step of the engine hands the module ONE batch and moves the job to running; the REST of the
// rows is the running step's, at the NEXT tick. The test therefore ticks the engine exactly once, then waits for the sink to hold a send of the job WITHOUT ticking, and pauses: some rows are in flight at the module,
// some are queued there, and the rest (the rows beyond the first batch) were never handed over and cannot be while the job is paused.

const (
	// jlpcBatch is the engine's submit batch of the pause scenario and jlpcCancelBatch the cancel scenario's: a list longer than this is handed over in parts, and the job is paused after the first.
	jlpcBatch       = 4
	jlpcCancelBatch = 3
	// jlpcStale is the staleness limit of the engines of these tests (the service's is 15 minutes). A RUNNING job whose items take longer than this to finish raises `stale` events (the twin does: the control), a held
	// job must not.
	jlpcStale = 2 * time.Second
	// jlpcStartDelay is the store's max start delay: a job that is not started this long after its confirm expires. A job paused while running has a start deadline in its past for most of its pause and must
	// NOT expire (the premise is read from the database's clock before it is asserted).
	jlpcStartDelay = 15 * time.Second
	// jlpcHold is how long the sink holds the final reply of every send (DATA accepted, 250 withheld): the time a send is "in flight". It must be longer than the gap between the sink showing the held send and the
	// store's pause answering (one database transaction), and short enough that a job of a dozen rows ends in minutes.
	jlpcHold = 3 * time.Second
	// jlpcWorkerPoll is what the observation window adds to three of the module's dispatch ticks: the worker's own scheduling after a claim, the sink's reply delay and the slowness of a runner.
	jlpcWorkerPoll = 3 * time.Second
	// jlpcMinWindow is the least the window lasts: it must be long enough for the twin (which is not paused) to deliver a message in it, one item taking about jlpcHold plus a lane's pause and a dispatch tick.
	jlpcMinWindow = 10 * time.Second
	// the sizes of the lists: the held job, its twin and the cancelled job's control; the held job's list also holds one malformed row and one repeat (rows the platform ends itself)
	jlpcHeldRows   = 12
	jlpcTwinRows   = 10
	jlpcCancelRows = 9
	jlpcCtlRows    = 5
)

// ---- the stack ------------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlpcEngineConfig is the engine of these tests: the production dials of the live stacks (a 700 ms tick, an observation every second, an idle job looked at again within two seconds) with a small submit batch and a
// tiny staleness limit for the two senders.
func jlpcEngineConfig(batch int, stale time.Duration) jobengine.Config {
	return jobengine.Config{
		Tick:        700 * time.Millisecond,
		SettleEvery: time.Second,
		StaleAfter: map[string]time.Duration{
			jobs.ModuleVerify:      30 * time.Minute,
			jobs.ModuleSendManaged: stale,
			jobs.ModuleSendBYO:     stale,
		},
		SubmitBatch:    batch,
		SubmitBatchFor: map[string]int{jobs.ModuleVerify: jobs.MaxEngineIDs},
		StepTimeout:    jobengine.DefaultStepTimeout,
		BackoffCap:     2 * time.Second,
	}
}

// jlpcUseEngine builds an engine over the stack's store, its registry (limited to this test's users) and gate, with the trace and the configuration given, makes it the stack's engine (s.eng: what tickUntil, tickToStatus
// and the failure cleanup tick) and returns it. The engine it replaces is never ticked again. It keeps NO memory of the other: that is what the restart of the scenario needs.
func (s *jlStack) jlpcUseEngine(tr *jlTrace, cfg jobengine.Config) *jobengine.Engine {
	s.t.Helper()
	log := slog.New(slog.NewTextHandler(s.ring, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, err := jobengine.New(jobengine.Deps{Store: s.store, Modules: jlTracedModules{inner: s.mine, tr: tr}, Leader: s.gate, Log: log}, cfg)
	if err != nil {
		s.t.Fatalf("jobengine.New (the engine of the pause scenario): %v", err)
	}
	s.eng = eng
	return eng
}

// ---- reading the platform's history -------------------------------------------------------------------------------------------------------------------------------------------

// jlpcNotes is how many `note` events of the module's hold the job's history holds (module_paused or module_resumed: the engine writes one per change of what the module's report says).
func (s *jlStack) jlpcNotes(job, event string) int64 {
	s.t.Helper()
	return s.count("count the notes of the module's hold of a job", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'note' AND detail->>'event' = $2`, job, event)
}

// jlpcPair is how many times the job moved from one status to another (transition events).
func (s *jlStack) jlpcPair(job, from, to string) int64 {
	s.t.Helper()
	return s.count("count the moves of a job between two statuses", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND from_status = $2 AND to_status = $3`, job, from, to)
}

// jlpcStaleBetween is how many `stale` events the job holds with an id after afterID and before beforeID (event ids grow with time).
func (s *jlStack) jlpcStaleBetween(job string, afterID, beforeID int64) int64 {
	s.t.Helper()
	return s.count("count the stale events of a job in a stretch of its history", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'stale' AND id > $2 AND id < $3`, job, afterID, beforeID)
}

// jlpcFindEvent is the first event of the list of the kind and the two statuses (a note has none: "" and "").
func jlpcFindEvent(evs []jlEvent, kind, from, to string) (jlEvent, bool) {
	for _, e := range evs {
		if e.Kind == kind && e.From == from && e.To == to {
			return e, true
		}
	}
	return jlEvent{}, false
}

// jlpcUnsubmitted is how many pending rows of the job were never handed to a module (no submitted_at stamp).
func (s *jlStack) jlpcUnsubmitted(job string) int64 {
	s.t.Helper()
	return s.count("count the pending rows never handed to a module", `SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending' AND submitted_at IS NULL`, job)
}

// jlpcNeverHandedAsCancelled is the rows of a job that was cancelled: a pending row that was never handed to the module is ended by the engine as the platform's own free `cancelled` result (P-01), so for the
// comparisons it is a row of the platform's own class (state cancelled), not a pending row the module must answer for.
func jlpcNeverHandedAsCancelled(rows []jlRow) []jlRow {
	out := append([]jlRow(nil), rows...)
	for i := range out {
		if out[i].State == "pending" && !out[i].Submitted {
			out[i].State = "cancelled"
		}
	}
	return out
}

// ---- the sink, the same way for both senders -----------------------------------------------------------------------------------------------------------------------------------

// jlpcRcpt is what the sink answered at RCPT TO for one recipient.
type jlpcRcpt struct{ Accepted, Rejected, Deferred int }

// jlpcSink is one reading of the sink as the rig's recipients see it: messages held per exact recipient and the RCPT answers.
type jlpcSink struct {
	Msgs map[string]int
	Rcpt map[string]jlpcRcpt
}

// jlpcSinkDiff is how the sink differs between two readings for the addresses, in words that name a recipient by its position in the list (never by its address): empty when it holds the same messages and has
// given the same RCPT answers for every one of them.
func jlpcSinkDiff(base, cur jlpcSink, addrs []string) []string {
	var out []string
	for i, a := range addrs {
		if base.Msgs[a] != cur.Msgs[a] || base.Rcpt[a] != cur.Rcpt[a] {
			out = append(out, fmt.Sprintf("recipient %d: %d message(s) and RCPT %+v now, %d and %+v before", i+1, cur.Msgs[a], cur.Rcpt[a], base.Msgs[a], base.Rcpt[a]))
		}
	}
	return out
}

// jlpcMsgs is how many messages the sink holds for the addresses.
func jlpcMsgs(s jlpcSink, addrs []string) int {
	n := 0
	for _, a := range addrs {
		n += s.Msgs[a]
	}
	return n
}

// jlpcHeldOf is how many of the sink's held sends are to the addresses.
func jlpcHeldOf(held, addrs []string) int {
	n := 0
	for _, h := range held {
		if slices.Contains(addrs, h) {
			n++
		}
	}
	return n
}

// ---- the comparison of a send job in the middle of its life --------------------------------------------------------------------------------------------------------------------

// jlpcReading is everything one send job leaves at one instant, from the platform's tables, the module's own report and the sink. It is the in-flight sibling of jlM3ThreeWay / jlM2ThreeWay (which compare an ENDED
// job and say that every pending row has an answer): here a row may not have been handed to the module, may be queued or in flight there, or may be final and not yet recorded.
type jlpcReading struct {
	Module  string
	Metered bool
	Rows    []jlRow
	Results []jlResult
	Report  []sendapi.Item
	// Before is the module's report as read BEFORE an engine step that observes the job without throttling (a paused job's): every item final in it must have a result now.
	Before []sendapi.Item
	Sink   jlpcSink
	Ledger []m1bCharge    // the job's charge rows (a metered module)
	Prices map[string]int // the job's frozen price snapshot (a metered module)
}

// jlpcRule is the charge table's rule label of a module's result: module 3's from jlM3Rule, module 2's (M2-01 sent, M2-02 bounced, M2-08 cancelled).
func jlpcRule(module, state string) string {
	if module == jobs.ModuleSendBYO {
		return jlM3Rule(state)
	}
	switch state {
	case "sent":
		return "M2-01"
	case "bounced":
		return "M2-02"
	case "cancelled":
		return "M2-08"
	}
	return "?"
}

// jlpcUnit is the unit the charge table charges for a final item of a metered sender ("" for a free one): written by hand from internal/charge/rules.psv, not through charge.Decide.
func jlpcUnit(metered bool, state string) string {
	if !metered {
		return ""
	}
	switch state {
	case "sent":
		return "m2_accepted"
	case "bounced":
		return "m2_bounced"
	}
	return ""
}

// jlpcProblems says every way a reading of a send job in the middle of its life is inconsistent, and nothing when it is consistent:
//
//	a row of the platform's own class has its one free result and is in no report and no ledger row;
//	a pending row the module does not hold has no result, no ledger row, no message and no RCPT answer at the sink;
//	a pending row the module holds and has not finished (queued, assigned, sending) has no result and no ledger row, and at most one message;
//	a pending row the module holds and has finished has the sink's evidence of its state (sent: one message and one accepted RCPT; bounced: one rejected RCPT and no message; cancelled: nothing at all), and, when
//	it has a result, the result of the report's state, free or charged by the table at the frozen price with exactly one ledger charge, under the table's rule;
//	an item that was final at the earlier read has a result by now.
//
// Nothing in the results, the report or the ledger names a row the job does not have.
func jlpcProblems(v jlpcReading) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	res := jlByRef(v.Results)
	rep := map[string][]sendapi.Item{}
	for _, it := range v.Report {
		rep[it.JobItemID] = append(rep[it.JobItemID], it)
	}
	led := map[string][]m1bCharge{}
	for _, c := range v.Ledger {
		led[c.ItemRef] = append(led[c.ItemRef], c)
	}
	was := map[string]bool{}
	for _, it := range v.Before {
		if it.Class == sendapi.ClassTerminal {
			was[it.JobItemID] = true
		}
	}
	known := map[string]jlRow{}
	for _, row := range v.Rows {
		known[row.ID] = row
		rs := res[row.ID]
		if row.State != "pending" {
			if len(rs) != 1 {
				say("row %d (%s) has %d result(s), want exactly 1", row.Ordinal, row.State, len(rs))
			} else {
				p = append(p, jlPlatformResultProblems(row, rs[0])...)
			}
			if len(rep[row.ID]) != 0 {
				say("the module's report holds an item for row %d (%s): the platform ended it and never ran it", row.Ordinal, row.State)
			}
			if len(led[row.ID]) != 0 {
				say("the ledger holds a charge for row %d (%s): the platform's own outcomes are never charged", row.Ordinal, row.State)
			}
			continue
		}
		items := rep[row.ID]
		msgs, rc := v.Sink.Msgs[row.Norm], v.Sink.Rcpt[row.Norm]
		switch {
		case len(items) > 1:
			say("row %d is in the module's report %d times", row.Ordinal, len(items))
		case len(items) == 0:
			if len(rs) != 0 {
				say("row %d was not handed to the module and has %d result(s)", row.Ordinal, len(rs))
			}
			if len(led[row.ID]) != 0 {
				say("row %d was not handed to the module and the ledger holds a charge for it", row.Ordinal)
			}
			if msgs != 0 || rc != (jlpcRcpt{}) {
				say("row %d was not handed to the module and the sink holds %d message(s) and answered RCPT %+v for it", row.Ordinal, msgs, rc)
			}
		default:
			it := items[0]
			if it.Class != sendapi.ClassTerminal {
				if len(rs) != 0 {
					say("row %d is in state %s at the module (not final) and has %d result(s)", row.Ordinal, it.State, len(rs))
				}
				if len(led[row.ID]) != 0 {
					say("row %d is in state %s at the module (not final) and the ledger holds a charge for it", row.Ordinal, it.State)
				}
				if was[row.ID] {
					say("row %d was final at the earlier read and is %s now", row.Ordinal, it.State)
				}
				if msgs > 1 {
					say("row %d is in state %s and the sink holds %d messages for it", row.Ordinal, it.State, msgs)
				}
				continue
			}
			switch it.State {
			case "sent":
				if msgs != 1 || rc.Accepted != 1 || rc.Rejected != 0 {
					say("row %d is sent: the sink holds %d message(s) and answered RCPT %+v, want one message and one acceptance", row.Ordinal, msgs, rc)
				}
			case "bounced":
				if msgs != 0 || rc.Rejected != 1 || rc.Accepted != 0 {
					say("row %d is bounced: the sink holds %d message(s) and answered RCPT %+v, want no message and one rejection", row.Ordinal, msgs, rc)
				}
			case "cancelled":
				if msgs != 0 || rc != (jlpcRcpt{}) {
					say("row %d is cancelled: the sink holds %d message(s) and answered RCPT %+v, want nothing at all", row.Ordinal, msgs, rc)
				}
			default:
				say("row %d is final in state %q, which no step of these scenarios can make", row.Ordinal, it.State)
				continue
			}
			if len(rs) == 0 {
				if was[row.ID] {
					say("row %d was final (%s) at the module at the earlier read and the engine recorded no result for it since", row.Ordinal, it.State)
				}
				if len(led[row.ID]) != 0 {
					say("row %d has a charge in the ledger and no result: the two are written in one transaction", row.Ordinal)
				}
				continue
			}
			if len(rs) != 1 {
				say("row %d has %d results, want 1", row.Ordinal, len(rs))
				continue
			}
			r := rs[0]
			if r.Origin != "module" || r.State != it.State {
				say("row %d: the result says %s from %s, the module's report says %s", row.Ordinal, r.State, r.Origin, it.State)
			}
			unit := jlpcUnit(v.Metered, it.State)
			wantDecision := "free"
			if unit != "" {
				wantDecision = "charge"
			}
			if r.Decision != wantDecision || r.Unit != unit {
				say("row %d: the result says decision %q unit %q, the table says %q and %q for a %s item", row.Ordinal, r.Decision, r.Unit, wantDecision, unit, it.State)
			}
			if want := jlpcRule(v.Module, it.State); r.Rule != want {
				say("row %d: the result is under rule %q, the table says %q for a %s item", row.Ordinal, r.Rule, want, it.State)
			}
			cs := led[row.ID]
			switch {
			case unit == "" && len(cs) != 0:
				say("row %d is a free %s item and the ledger holds %d charge row(s) for it", row.Ordinal, it.State, len(cs))
			case unit != "" && len(cs) != 1:
				say("row %d is a %s item: the ledger holds %d charge row(s) for it, want exactly 1", row.Ordinal, it.State, len(cs))
			case unit != "":
				c := cs[0]
				if c.Unit != unit || c.Units != 1 {
					say("row %d is charged %d x unit %s, the table says one unit of %s", row.Ordinal, c.Units, c.Unit, unit)
				}
				if want, ok := v.Prices[unit]; !ok || c.Cost != want {
					say("row %d is charged %d credit(s) a unit, the frozen price of %s is %d", row.Ordinal, c.Cost, unit, v.Prices[unit])
				}
			}
		}
	}
	for ref, rs := range res {
		if _, ok := known[ref]; !ok {
			say("%d result(s) belong to a row the job does not have", len(rs))
		}
	}
	for id := range rep {
		if _, ok := known[id]; !ok {
			say("the module's report holds an item that is no row of the job")
		}
	}
	for ref := range led {
		if _, ok := known[ref]; !ok {
			say("the ledger holds a charge for an item that is no row of the job")
		}
	}
	sort.Strings(p)
	return p
}

// ---- module 1: the comparison of a CANCELLED job -------------------------------------------------------------------------------------------------------------------------------

// jlpcM1Oracle is m1bOracle plus the one row m1bOracle does not know: a row module 1 cancelled before its check began (charge table row M1-24): free, never a verdict, never a unit. Any other reason on a
// cancelled row is unmapped.
func jlpcM1Oracle(r m1ad.Row) m1bDecision {
	if r.Status == m1ad.StatusCancelled {
		if r.Reason != m1ad.ReasonCancelledByCaller {
			return m1bUnmapped("a cancelled row with the reason " + strconv.Quote(r.Reason))
		}
		return m1bFree("cancelled before its check began (M1-24)")
	}
	return m1bOracle(r)
}

// jlpcM1CancelThreeWay is jlM1ThreeWay for a module 1 job that was CANCELLED: the same item-by-item comparison of the platform's results, the ledger and module 1's own listing, with the oracle that knows a row
// cancelled before its check began. Each such row has ONE result of origin module in the state `cancelled`, the reason `cancelled_by_caller`, free, no unit, under rule M1-24, and NO ledger row; every other
// row of module 1's listing is judged exactly as jlM1ThreeWay judges it, and the ledger is compared with the report by m1bCompare (which expects no charge for a row the oracle calls free).
func jlpcM1CancelThreeWay(v jlM1View) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	res := jlByRef(v.Results)
	rep := map[string][]m1ad.Row{}
	for _, r := range v.Report {
		rep[r.ItemID] = append(rep[r.ItemID], r)
	}
	known := map[string]jlRow{}
	for _, row := range v.Rows {
		known[row.ID] = row
		rs := res[row.ID]
		if len(rs) != 1 {
			say("row %d (%s) has %d result(s), want exactly 1", row.Ordinal, row.State, len(rs))
			continue
		}
		r := rs[0]
		reps := rep[row.ID]
		if row.State != "pending" {
			p = append(p, jlPlatformResultProblems(row, r)...)
			if len(reps) != 0 {
				say("module 1's report holds %d row(s) for row %d (%s): the platform ended it and never sent it", len(reps), row.Ordinal, row.State)
			}
			continue
		}
		if len(reps) != 1 {
			say("row %d is pending and module 1's report holds %d row(s) for it, want exactly 1", row.Ordinal, len(reps))
			continue
		}
		m := reps[0]
		d := jlpcM1Oracle(m)
		if r.Origin != "module" {
			say("row %d has a result of origin %q, want module (module 1 answered it)", row.Ordinal, r.Origin)
		}
		if r.State != m.Status || r.Reason != m.Reason {
			say("row %d: the result says %s/%s, module 1's report says %s/%s", row.Ordinal, r.State, orDash(r.Reason), m.Status, orDash(m.Reason))
		}
		if r.Decision != d.Kind {
			say("row %d: the result says %q, the hand-written oracle says %q for module 1's %s/%s (%s)", row.Ordinal, r.Decision, d.Kind, m.Status, m.Reason, d.Why)
		}
		wantUnit := ""
		if d.Kind == "charge" {
			wantUnit = d.Unit
		}
		if r.Unit != wantUnit {
			say("row %d: the result names unit %q, the oracle says %q", row.Ordinal, r.Unit, wantUnit)
		}
		if m.Status == m1ad.StatusCancelled && r.Rule != "M1-24" {
			say("row %d is cancelled by module 1 and its result is under rule %q, want M1-24", row.Ordinal, r.Rule)
		}
		if m.Status != m1ad.StatusCancelled && r.Rule == "M1-24" {
			say("row %d has a verdict (%s) and its result is under rule M1-24, the rule of a cancelled row", row.Ordinal, m.Status)
		}
	}
	for ref, rs := range res {
		if _, ok := known[ref]; !ok {
			say("%d result(s) belong to a row the job does not have", len(rs))
		}
	}
	for id, ms := range rep {
		if _, ok := known[id]; !ok {
			say("module 1's report holds %d row(s) whose item_id is no row of the job", len(ms))
		}
	}
	// the ledger against the report: m1bCompare asks m1bOracle, which calls a cancelled row `unmapped` (not a charge), so a cancelled row with a ledger charge is reported by it as "the report makes it unmapped
	// but the ledger charged it"; every other row is judged as in jlM1ThreeWay. The explicit loop below says it in M1-24's own words.
	p = append(p, m1bCompare(v.Report, v.Ledger, v.Prices)...)
	for _, c := range v.Ledger {
		for _, m := range rep[c.ItemRef] {
			if m.Status == m1ad.StatusCancelled {
				say("the ledger holds a charge for a row module 1 cancelled before its check began (M1-24 is free)")
			}
		}
	}
	sort.Strings(p)
	return p
}

// ---- the rig ----------------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlpcSender is one tenant of a scenario: a user with an account at the module, funded or entitled, and the recipients of its job.
type jlpcSender struct {
	Name   string
	User   string
	Addrs  []string // the recipients of the job, lower case and distinct: one pending row each
	Handle any      // the module's own tenant record
}

// jlpcRig is what a scenario needs of one sender module. T, S and Tr are the test, the stack and the trace of the stack's CURRENT engine; the rest is the module's own side, supplied by its file.
type jlpcRig struct {
	T       *testing.T
	S       *jlStack
	Tr      *jlTrace
	Module  string
	Metered bool
	Grant   int64 // the credits every sender of a metered module is given
	Cfg     jobengine.Config
	Window  time.Duration // how long the sink must show nothing new for a held job
	// New makes a tenant (a login, an account, credits or an entitlement) with n distinct recipients.
	New func(name string, n int) *jlpcSender
	// Mode puts the sink in "hold the final reply of every send to these recipients for jlpcHold" mode (the whole mode is replaced).
	Mode func(all []string)
	// Attach tells the module's own harness about a job (its campaign is the job's id and its pending rows are the tenant's items); a no-op where nothing needs telling.
	Attach func(sn *jlpcSender, job string)
	// Report is the module's OWN report of the job's campaign, every page, read at the module's own address.
	Report func(sn *jlpcSender, job string) sendapi.CampaignReport
	// Sink reads the sink (it FAILS the test at once for a duplicate send); Held is the recipients of the sends the sink is holding right now.
	Sink func() jlpcSink
	Held func() []string
	// ModuleJobs is how many jobs the module itself holds for the campaign.
	ModuleJobs func(sn *jlpcSender, job string) int
	// Diag is what a failed wait says about the module's side.
	Diag func(sn *jlpcSender) string
	// Final is the module's own comparison of an ENDED job (report, results, sink, books), cancelled or not; it FAILS the test with every disagreement.
	Final func(sn *jlpcSender, job string, pv jobs.Preview, cancelled bool)
}

func (r *jlpcRig) tick() { r.S.eng.Tick(r.S.ctx) }

func (r *jlpcRig) diagOf(sn *jlpcSender) func() string { return func() string { return r.Diag(sn) } }

// reading assembles a jlpcReading of a job from the three places (the module's report is read FIRST: nothing the engine does can run between the reads, because the engine runs only when the test ticks it).
func (r *jlpcRig) reading(sn *jlpcSender, job string, before []sendapi.Item) (jlpcReading, sendapi.CampaignReport) {
	r.T.Helper()
	rep := r.Report(sn, job)
	v := jlpcReading{Module: r.Module, Metered: r.Metered, Rows: r.S.rows(job), Results: r.S.results(job), Report: rep.Items, Before: before, Sink: r.Sink()}
	if r.Metered {
		v.Ledger = r.S.ledgerCharges(job)
		v.Prices = r.S.prices(job)
	}
	return v, rep
}

// requirePartial FAILS with every disagreement of the job's three places at this moment and returns the module's report it read.
func (r *jlpcRig) requirePartial(when string, sn *jlpcSender, job string, before []sendapi.Item) sendapi.CampaignReport {
	r.T.Helper()
	v, rep := r.reading(sn, job, before)
	if p := jlpcProblems(v); len(p) != 0 {
		r.T.Fatalf("%s: the platform's rows and results, the module's own report and the sink disagree (%d):\n  %s\n%s", when, len(p), strings.Join(p, "\n  "), r.S.dump(r.diagOf(sn)))
	}
	return rep
}

// jlpcHeld is a job left PAUSED while running with its module holding the campaign, the item(s) that were in flight when the user paused finished and recorded, and rows still to come (queued at the module and,
// beyond the first batch, never handed over).
type jlpcHeld struct {
	Sender  *jlpcSender
	Job     jobs.Job
	Preview jobs.Preview
	Rows    int                    // the job's rows (pending and the platform's own)
	Handed  int64                  // the pending rows handed to the module before the pause
	Unsent  int64                  // the pending rows never handed over
	PauseEv jlEvent                // the user's `running -> paused` transition
	NoteEv  jlEvent                // the engine's module_paused note
	Report  sendapi.CampaignReport // the module's report after the in-flight item(s) finished
	Done    int64                  // the module results the platform had recorded by then
}

// holdOne makes the job of the sender and leaves it held (see jlpcHeld). extras adds one malformed row and one repeat to the list. The sequence is the one the file comment explains: ONE engine tick starts the job
// (one batch handed over, running), then the test waits for the sink to hold a send of the job WITHOUT ticking, then the user pauses.
func (r *jlpcRig) holdOne(sn *jlpcSender, label string, extras bool) *jlpcHeld {
	t, s := r.T, r.S
	t.Helper()
	pending := len(sn.Addrs)
	rows := jlRowsOf(sn.Addrs)
	wantRows := pending
	if extras {
		rows = jlBuildList(sn.Addrs, 1, 1).Rows
		wantRows = pending + 2
	}
	j, pv, res := s.start(sn.User, jobs.CreateReq{Module: r.Module, Rows: rows, Content: jlContent(label)})
	if pv.Rows != wantRows || pv.Pending != pending || res.Job.Status != jobs.StatusConfirmed || res.Pending != pending || pv.Metered != r.Metered {
		t.Fatalf("the %s job's preview counts rows %d pending %d metered %v and the confirm answered %s with %d pending; want %d, %d, metered %v, confirmed, %d", label, pv.Rows, pv.Pending, pv.Metered, res.Job.Status, res.Pending, wantRows, pending, r.Metered, pending)
	}
	if r.Metered && (res.Reserved != pv.Credits || pv.Credits < 1) {
		t.Fatalf("the %s job reserved %d credits, the preview says %d", label, res.Reserved, pv.Credits)
	}
	r.Attach(sn, j.ID)
	diag := r.diagOf(sn)

	// ONE tick starts the job: the engine hands the module the first batch and moves the job to running (a tick before that step finds the module not answering is repeated: the job is not running yet, so the
	// rest of its rows is not the engine's business yet)
	for i := 0; ; i++ {
		s.eng.Tick(s.ctx)
		if s.status(j.ID) == jobs.StatusRunning {
			break
		}
		if i >= 30 {
			t.Fatalf("the %s job is %s after %d ticks, want running\n%s", label, s.status(j.ID), i+1, s.dump(diag))
		}
		time.Sleep(700 * time.Millisecond)
	}
	unsent := s.jlpcUnsubmitted(j.ID)
	handed := int64(pending) - unsent
	if unsent < 1 || handed < 1 || handed > int64(r.Cfg.SubmitBatch) {
		t.Fatalf("after the engine's first step the %s job has handed %d of its %d rows to the module and %d are still to come: want some of each, at most the submit batch of %d handed (a step that hands over everything leaves nothing to hold back: the batch is %d and the list %d)", label, handed, pending, unsent, r.Cfg.SubmitBatch, r.Cfg.SubmitBatch, pending)
	}
	if n := r.ModuleJobs(sn, j.ID); int64(n) != handed {
		t.Fatalf("after the engine's first step the module holds %d job(s) of the %s campaign and the platform stamped %d row(s) as handed over", n, label, handed)
	}

	// the sink holds a send of the job: an item is in flight. The engine is NOT ticked meanwhile (a tick would hand the rest of the rows over).
	m3lWait(t, s.ctx, "the sink holds a send of the "+label+" job: an item is in flight at the module", 90*time.Second, 100*time.Millisecond, func() (bool, string) {
		h := r.Held()
		return jlpcHeldOf(h, sn.Addrs) >= 1, fmt.Sprintf("the sink holds %d send(s), %d of them to this job's recipients", len(h), jlpcHeldOf(h, sn.Addrs))
	})

	// ---- the user pauses: the job is paused the moment the store answers ----
	pj, err := s.store.Pause(s.ctx, sn.User, j.ID)
	if err != nil || pj.Status != jobs.StatusPaused || pj.PausedFrom != jobs.StatusRunning {
		t.Fatalf("the user's pause of the running %s job answered %+v (error %v), want a paused job with paused_from running", label, pj, err)
	}
	if st, from := s.status(j.ID), s.str("read paused_from", `SELECT COALESCE(paused_from, '') FROM platform.jobs WHERE id = $1::uuid`, j.ID); st != jobs.StatusPaused || from != jobs.StatusRunning {
		t.Fatalf("the database says the %s job is %s paused from %q after the pause, want paused from running", label, st, from)
	}
	if flying := jlpcHeldOf(r.Held(), sn.Addrs); flying < 1 {
		t.Fatalf("the %s job's send finished before the pause answered: the sink's hold (%s) is too short for this runner, so nothing was in flight when the user paused; raise jlpcHold", label, jlpcHold)
	}
	if n := r.Tr.since(0, j.ID, "pause", ""); n != 0 {
		t.Fatalf("the engine asked the module to pause %d time(s) before it was ticked: the store must not tell the module", n)
	}
	if rep := r.Report(sn, j.ID); rep.Paused {
		t.Fatalf("the module's own report says the campaign is paused before the engine told it to hold it: the lag between the API's answer and the module's acknowledgement does not exist, so the report is not the witness the engine reads")
	}
	pauseEv, ok := jlpcFindEvent(s.eventsAfter(j.ID, 0), "transition", jobs.StatusRunning, jobs.StatusPaused)
	if !ok || pauseEv.By != "user" {
		t.Fatalf("the %s job's history holds no `running -> paused` transition by the user\n%s", label, strings.Join(s.events(j.ID), "\n"))
	}

	// ---- the engine tells the module, and the module's own report says it holds the campaign ----
	s.tickUntil("the engine tells the module to hold the "+label+" campaign and the module's own report says it does", 90*time.Second, 700*time.Millisecond, diag, func() (bool, string) {
		n := s.jlpcNotes(j.ID, jobs.NoteModulePaused)
		return n >= 1, fmt.Sprintf("%d module_paused note(s)", n)
	})
	if n := s.jlpcNotes(j.ID, jobs.NoteModulePaused); n != 1 {
		t.Fatalf("the %s job's history holds %d module_paused note(s), want exactly one\n%s", label, n, strings.Join(s.events(j.ID), "\n"))
	}
	if rep := r.Report(sn, j.ID); !rep.Paused {
		t.Fatalf("the engine noted that the module holds the %s campaign and the module's own report says it does not", label)
	}
	if n := r.Tr.since(0, j.ID, "pause", "ok"); n < 1 {
		t.Fatalf("the module's report says the campaign is held and the engine never made a successful pause call (%d)", n)
	}
	noteEv, ok := jlpcFindEvent(s.eventsAfter(j.ID, 0), "note", "", "")
	if !ok || noteEv.ID <= pauseEv.ID || noteEv.By != "engine" {
		t.Fatalf("the %s job's module_paused note (%+v) is not an engine event after the user's pause (%+v): the pause is the API's answer, the note is the module's acknowledgement", label, noteEv, pauseEv)
	}

	// ---- what was in flight finishes, is recorded, and nothing else is claimed ----
	var rep sendapi.CampaignReport
	var done int64
	s.tickUntil("the item(s) in flight when the "+label+" job was paused finish and the engine records each of them", 120*time.Second, 700*time.Millisecond, diag, func() (bool, string) {
		rep = r.Report(sn, j.ID)
		_, done = s.resultCount(j.ID)
		held := jlpcHeldOf(r.Held(), sn.Addrs)
		return rep.Totals.InFlight == 0 && held == 0 && done >= 1 && done == rep.Progress.Terminal, fmt.Sprintf("module in flight %d, the sink holds %d of the job's sends, module results recorded %d, final at the module %d", rep.Totals.InFlight, held, done, rep.Progress.Terminal)
	})
	r.requirePartial("the "+label+" job after the item(s) in flight finished", sn, j.ID, rep.Items)
	return &jlpcHeld{Sender: sn, Job: pj, Preview: pv, Rows: wantRows, Handed: handed, Unsent: unsent, PauseEv: pauseEv, NoteEv: noteEv, Report: rep, Done: done}
}

// requireSlotTaken proves the job still holds its user's one active slot: a second job of the same user and module, previewed, is not confirmed (the store answers ErrSlotTaken, or, for module 3, which the store asks
// BEFORE its transaction whether the campaign can run, the module's own cause `campaign_active`: the same fact said by the module) and nothing is held for it; the draft is then discarded.
func (r *jlpcRig) requireSlotTaken(sn *jlpcSender, what string) {
	t, s := r.T, r.S
	t.Helper()
	extra := []string{strings.Replace(sn.Addrs[0], "@", "-slot@", 1), strings.Replace(sn.Addrs[1], "@", "-slot@", 1)}
	j := s.create(sn.User, jobs.CreateReq{Module: r.Module, Rows: jlRowsOf(extra), Content: jlContent("a second job while the first is held")})
	pv := s.preview(sn.User, j.ID)
	av0, rv0 := s.balance(sn.User, r.Module)
	_, err := s.confirm(sn.User, j.ID, pv.Fingerprint, nil)
	var inf jobs.ErrInfeasible
	switch {
	case errors.Is(err, jobs.ErrSlotTaken):
	case r.Module == jobs.ModuleSendBYO && errors.As(err, &inf) && slices.Equal(inf.Causes, []string{"campaign_active"}):
	default:
		t.Fatalf("%s: a second confirm for the same user and module answered %v, want ErrSlotTaken (module 3: or the module's own cause campaign_active)", what, err)
	}
	if st := s.status(j.ID); st != jobs.StatusPreviewed {
		t.Fatalf("%s: the refused second job is %s, want previewed", what, st)
	}
	if _, _, _, held := s.reserveOf(j.ID); held || s.ledgerRowsOf(j.ID) != 0 {
		t.Fatalf("%s: the refused second job holds a reserve or a ledger row", what)
	}
	if av, rv := s.balance(sn.User, r.Module); av != av0 || rv != rv0 {
		t.Fatalf("%s: the refused second job moved the balance from (%d, %d) to (%d, %d)", what, av0, rv0, av, rv)
	}
	if _, err := s.store.DiscardDraft(s.ctx, sn.User, j.ID); err != nil {
		t.Fatalf("%s: discard the refused second job: %v", what, err)
	}
}

// requireHoldsCredits proves a metered job's reserve is as it was: held whole, nothing released, the balance's reserved part what is left of it. It returns the reserve it read.
func (r *jlpcRig) requireHoldsCredits(sn *jlpcSender, job string, pv jobs.Preview, what string) (reserved, charged int64) {
	t, s := r.T, r.S
	t.Helper()
	if !r.Metered {
		if _, _, _, held := s.reserveOf(job); held || s.ledgerRowsOf(job) != 0 {
			t.Fatalf("%s: the job of an unmetered module holds a reserve or a ledger row", what)
		}
		return 0, 0
	}
	rs, ch, rl, ok := s.reserveOf(job)
	if !ok || rs != pv.Credits || rl != 0 || ch > rs {
		t.Fatalf("%s: the reserve is (reserved %d, charged %d, released %d, exists %v), want the %d credits the preview held, none released", what, rs, ch, rl, ok, pv.Credits)
	}
	if rows, _ := s.releaseRows(job); rows != 0 {
		t.Fatalf("%s: the job has %d release row(s) while it is held", what, rows)
	}
	if av, rv := s.balance(sn.User, r.Module); av != r.Grant-rs || rv != rs-ch {
		t.Fatalf("%s: the balance is available %d reserved %d, want %d and %d (the grant less the reserve; the reserve less what was charged)", what, av, rv, r.Grant-rs, rs-ch)
	}
	return rs, ch
}

// ---- the scenarios ----------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlpcRunPauseResume is TestJobsLive_M3_PauseAndResumeARunningJob / TestJobsLive_M2_PauseAndResumeARunningJob. See those tests for what it proves.
func jlpcRunPauseResume(r *jlpcRig) {
	t, s := r.T, r.S
	k := r.New("jlpk", jlpcHeldRows)
	tw := r.New("jlpt", jlpcTwinRows)
	all := append(append([]string(nil), k.Addrs...), tw.Addrs...)
	r.Mode(all)
	diagK, diagT := r.diagOf(k), r.diagOf(tw)

	h := r.holdOne(k, "pause and resume: the held job", true)
	kj := h.Job
	t.Logf("NOTE jlpc held job: %d rows, %d handed to the module before the pause (batch %d), %d never handed over; paused %s from running; the module's report says paused; %d item(s) in flight finished and were recorded (%d result(s))",
		h.Rows, h.Handed, r.Cfg.SubmitBatch, h.Unsent, jobs.StatusPaused, h.Report.Totals.Sent+h.Report.Totals.Bounced+h.Report.Totals.Failed, h.Done)

	// ---- the baseline of the window: what the sink and the module hold now, and what the books hold ----
	base := r.Sink()
	baseRep := r.Report(k, kj.ID)
	if baseRep.Totals.Total != h.Handed || !baseRep.Paused {
		t.Fatalf("the module's report holds %d item(s) (paused %v), want the %d handed over and paused", baseRep.Totals.Total, baseRep.Paused, h.Handed)
	}
	rs0, ch0 := r.requireHoldsCredits(k, kj.ID, h.Preview, "the held job at the start of the window")
	baseEv := s.lastEventID(kj.ID)

	// ---- the twin: the same kind of job, NEVER paused, started now, so that the sink is shown to deliver in the window the held job's recipients get nothing in ----
	tj, tpv, _ := s.start(tw.User, jobs.CreateReq{Module: r.Module, Rows: jlRowsOf(tw.Addrs), Content: jlContent("pause and resume: the twin that is never paused")})
	if tpv.Pending != jlpcTwinRows || tpv.Metered != r.Metered {
		t.Fatalf("the twin's preview counts %d pending (metered %v), want %d", tpv.Pending, tpv.Metered, jlpcTwinRows)
	}
	r.Attach(tw, tj.ID)
	s.tickToStatus(tj.ID, jobs.StatusRunning, 120*time.Second, diagT)
	tBase := r.Sink()
	_, tMod0 := s.resultCount(tj.ID)

	// ---- THE WINDOW: more than three dispatch ticks and the worker's poll, with the engine ticking, the job held ----
	mark := r.Tr.mark()
	start := time.Now()
	ticks := 0
	for time.Since(start) < r.Window {
		r.tick()
		ticks++
		now := r.Sink()
		if d := jlpcSinkDiff(base, now, k.Addrs); len(d) != 0 {
			t.Fatalf("tick %d of the window: the sink changed for the held job's recipients while its campaign is held (%s into the window): %s\n%s", ticks, time.Since(start).Round(100*time.Millisecond), strings.Join(d, "; "), s.dump(diagK))
		}
		rep := r.Report(k, kj.ID)
		if !rep.Paused || rep.Totals.Total != h.Handed {
			t.Fatalf("tick %d of the window: the module's report says paused %v with %d item(s), want paused with the %d handed over before the pause: no further row may be handed to the module while the job is held\n%s", ticks, rep.Paused, rep.Totals.Total, h.Handed, s.dump(diagK))
		}
		if st := s.status(kj.ID); st != jobs.StatusPaused {
			t.Fatalf("tick %d of the window: the held job is %s\n%s", ticks, st, s.dump(diagK))
		}
		if n := r.Tr.since(mark, kj.ID, "pause", ""); n != 0 {
			t.Fatalf("tick %d of the window: the engine asked the module to pause %d more time(s) although its report already said the campaign is held", ticks, n)
		}
		if n := r.Tr.since(mark, kj.ID, "submit", ""); n != 0 {
			t.Fatalf("tick %d of the window: the engine offered the module %d more submit(s) for a held job", ticks, n)
		}
		time.Sleep(700 * time.Millisecond)
	}
	if ticks < 5 {
		t.Fatalf("the window held only %d engine tick(s) in %s: it measured nothing", ticks, r.Window)
	}
	r.requirePartial("the held job at the end of the window", k, kj.ID, baseRep.Items)

	// the control: the SAME instrument over the twin's recipients, in the same window, sees messages arrive
	tNow := r.Sink()
	if d := jlpcSinkDiff(tBase, tNow, tw.Addrs); len(d) == 0 || jlpcMsgs(tNow, tw.Addrs) <= jlpcMsgs(tBase, tw.Addrs) {
		t.Fatalf("the control: the twin (never paused, running) gained no message in the %s window (the sink holds %d for it, held %d at its start): the observation that saw nothing for the held job could not have seen anything; raise jlpcHold or the window",
			r.Window, jlpcMsgs(tNow, tw.Addrs), jlpcMsgs(tBase, tw.Addrs))
	}
	if _, tMod := s.resultCount(tj.ID); s.status(tj.ID) != jobs.StatusRunning || tMod <= tMod0 || tMod > int64(jlpcTwinRows-2) {
		t.Fatalf("the control: the twin is %s with %d result(s) (%d at the window's start) of %d: want running, more results than at the start and at least two rows still to go (raise jlpcTwinRows if the module is fast)", s.status(tj.ID), tMod, tMod0, jlpcTwinRows)
	}
	t.Logf("NOTE jlpc window: %s over %d engine ticks: the held job's recipients: the sink did not change (%d message(s) before and after); the twin's: %d -> %d message(s)", r.Window, ticks, jlpcMsgs(base, k.Addrs), jlpcMsgs(tBase, tw.Addrs), jlpcMsgs(tNow, tw.Addrs))

	// ---- not stale, not expired ----
	if n := s.jlpcStaleBetween(kj.ID, baseEv, 1<<62); n != 0 {
		t.Fatalf("the held job raised %d stale event(s) in a window longer than its staleness limit of %s: a held job is not stale\n%s", n, jlpcStale, strings.Join(s.events(kj.ID), "\n"))
	}
	if n := s.eventKindCount(tj.ID, "stale"); n < 1 {
		t.Fatalf("the control: the twin, running, raised no stale event with the staleness limit at %s although each of its items takes longer: the staleness check of this stack cannot be seen, so the held job's silence proves nothing", jlpcStale)
	}
	var deadline time.Time
	if err := s.admin.QueryRow(s.ctx, `SELECT start_deadline FROM platform.jobs WHERE id = $1::uuid`, kj.ID).Scan(&deadline); err != nil {
		t.Fatalf("read the start deadline: %v", err)
	}
	for i := 0; s.dbNow().Before(deadline.Add(time.Second)); i++ { // the premise of "never expires": the deadline is in the past by the database's clock
		if i > 40 {
			t.Fatalf("the start deadline %s is still ahead of the database's clock after %d ticks", deadline.Format(time.RFC3339), i)
		}
		r.tick()
		time.Sleep(700 * time.Millisecond)
	}
	r.tick()
	if st := s.status(kj.ID); st != jobs.StatusPaused || s.moves(kj.ID, jobs.StatusExpired) != 0 || s.moves(kj.ID, jobs.StatusFailed) != 0 || s.moves(kj.ID, jobs.StatusCancelled) != 0 {
		t.Fatalf("the held job is %s with its start deadline (%s) in the past by the database's clock: a job paused while running never expires\n%s", st, deadline.Format(time.RFC3339), strings.Join(s.events(kj.ID), "\n"))
	}

	// ---- it holds its credits and its slot ----
	if rs, ch := r.requireHoldsCredits(k, kj.ID, h.Preview, "the held job after the window"); rs != rs0 || ch != ch0 {
		t.Fatalf("the reserve moved during the window: (reserved %d, charged %d) before, (%d, %d) after: nothing was finished, so nothing may be charged", rs0, ch0, rs, ch)
	}
	r.requireSlotTaken(k, "the held job")

	// ---- the engine restarts while the job is paused: a second engine on the same database converges and does nothing twice ----
	tr2 := &jlTrace{}
	s.jlpcUseEngine(tr2, r.Cfg)
	r.Tr = tr2
	for i := 0; i < 8; i++ {
		r.tick()
		time.Sleep(700 * time.Millisecond)
		if st := s.status(kj.ID); st != jobs.StatusPaused {
			t.Fatalf("restart tick %d: the held job is %s", i+1, st)
		}
		if d := jlpcSinkDiff(base, r.Sink(), k.Addrs); len(d) != 0 {
			t.Fatalf("restart tick %d: the sink changed for the held job's recipients: %s", i+1, strings.Join(d, "; "))
		}
		if np, nr := s.jlpcNotes(kj.ID, jobs.NoteModulePaused), s.jlpcNotes(kj.ID, jobs.NoteModuleResumed); np != 1 || nr != 0 {
			t.Fatalf("restart tick %d: %d module_paused and %d module_resumed note(s), want 1 and 0: a restarted engine writes no note twice", i+1, np, nr)
		}
	}
	if n := tr2.since(0, kj.ID, "pause", ""); n != 0 {
		t.Fatalf("the second engine asked the module to pause %d time(s): it must find the campaign held (its report says so) and ask for nothing", n)
	}
	if n := tr2.since(0, kj.ID, "observe", "ok"); n < 3 {
		t.Fatalf("the second engine observed the held job %d time(s) in its eight ticks: the control that it looked at the job is not met", n)
	}
	if rep := r.Report(k, kj.ID); !rep.Paused || rep.Totals.Total != h.Handed {
		t.Fatalf("after the restart the module's report says paused %v with %d item(s), want paused with %d", rep.Paused, rep.Totals.Total, h.Handed)
	}
	// (the module-side restart while paused, Control and worker, is proved by the module's own CI: module 3's tests/harness/phase4_pause.sh and module 2's SUITE 14 restart both and see the campaign still held)

	// ---- the user resumes: the module is told, the rest of the rows is handed over, every recipient gets one message ----
	rj, err := s.store.Resume(s.ctx, k.User, kj.ID)
	if err != nil || rj.Status != jobs.StatusRunning || rj.PausedFrom != "" {
		t.Fatalf("the user's resume answered %+v (error %v), want a running job: the deadline is past and a job paused while running does not care", rj, err)
	}
	if rep := r.Report(k, kj.ID); !rep.Paused {
		t.Fatalf("the module's report says the campaign is released the moment the store answered the resume: the engine has not told it yet, so the lag of the resume does not exist")
	}
	s.tickUntil("the engine hands over the rest of the rows and tells the module to let the campaign go", 120*time.Second, 700*time.Millisecond, diagK, func() (bool, string) {
		rep := r.Report(k, kj.ID)
		n := s.jlpcNotes(kj.ID, jobs.NoteModuleResumed)
		return !rep.Paused && n >= 1 && rep.Totals.Total == int64(jlpcHeldRows), fmt.Sprintf("module report paused %v, %d item(s) of %d, %d module_resumed note(s)", rep.Paused, rep.Totals.Total, jlpcHeldRows, n)
	})
	if n := r.Tr.since(0, kj.ID, "resume", "ok"); n < 1 {
		t.Fatalf("the engine never made a successful resume call (%d)", n)
	}
	s.tickToStatus(kj.ID, jobs.StatusSucceeded, 6*time.Minute, diagK)
	s.tickToStatus(tj.ID, jobs.StatusSucceeded, 6*time.Minute, diagT)

	// ---- the ended jobs: the module's own three-way comparison, every recipient exactly once, the books ----
	r.Final(k, kj.ID, h.Preview, false)
	r.Final(tw, tj.ID, tpv, false)
	end := r.Sink()
	for i, a := range append(append([]string(nil), k.Addrs...), tw.Addrs...) {
		if end.Msgs[a] != 1 {
			t.Fatalf("recipient %d of the two jobs has %d message(s) at the sink, want exactly one", i+1, end.Msgs[a])
		}
	}
	if rows, results := s.rows(kj.ID), s.results(kj.ID); len(rows) != h.Rows || len(results) != len(rows) {
		t.Fatalf("the held job has %d row(s) and %d result(s), want %d of each", len(rows), len(results), h.Rows)
	}
	if n := r.ModuleJobs(k, kj.ID); n != jlpcHeldRows {
		t.Fatalf("the module holds %d job(s) of the held campaign, want %d: the rest of the rows was handed over after the resume, once", n, jlpcHeldRows)
	}
	s.requireMoves(kj.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 2, jobs.StatusPaused: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	for _, c := range []struct {
		from, to, by string
	}{{jobs.StatusRunning, jobs.StatusPaused, "user"}, {jobs.StatusPaused, jobs.StatusRunning, "user"}, {jobs.StatusRunning, jobs.StatusCompleting, "engine"}, {jobs.StatusCompleting, jobs.StatusSucceeded, "engine"}} {
		if n := s.jlpcPair(kj.ID, c.from, c.to); n != 1 || s.movesBy(kj.ID, c.to, c.by) < 1 {
			t.Errorf("the held job moved %s -> %s %d time(s) (by %s: %d), want exactly once by the %s\n%s", c.from, c.to, n, c.by, s.movesBy(kj.ID, c.to, c.by), c.by, strings.Join(s.events(kj.ID), "\n"))
		}
	}
	if n := s.jlpcPair(kj.ID, jobs.StatusPaused, jobs.StatusCompleting) + s.jlpcPair(kj.ID, jobs.StatusPaused, jobs.StatusCancelled); n != 0 {
		t.Errorf("the held job moved out of paused to completing or cancelled %d time(s)", n)
	}
	evs := s.eventsAfter(kj.ID, 0)
	resumeEv, ok := jlpcFindEvent(evs, "transition", jobs.StatusPaused, jobs.StatusRunning)
	if !ok || resumeEv.ID <= h.PauseEv.ID {
		t.Fatalf("the held job's history holds no `paused -> running` transition after the pause")
	}
	if n := s.jlpcStaleBetween(kj.ID, h.PauseEv.ID, resumeEv.ID); n != 0 {
		t.Errorf("the held job raised %d stale event(s) between the pause and the resume", n)
	}
	if np, nr := s.jlpcNotes(kj.ID, jobs.NoteModulePaused), s.jlpcNotes(kj.ID, jobs.NoteModuleResumed); np != 1 || nr != 1 {
		t.Errorf("the held job's history holds %d module_paused and %d module_resumed note(s), want one each", np, nr)
	}
	if n := s.eventKindCount(kj.ID, "flag"); n != 0 {
		t.Errorf("the held job raised %d flag event(s): nothing may be doubted or invented\n%s", n, strings.Join(s.events(kj.ID), "\n"))
	}
	if s.invented(kj.ID) != 0 || s.invented(tj.ID) != 0 {
		t.Errorf("a job holds an infrastructure_failure or unmapped result")
	}
	if st := s.eng.Stats(); st.Refused != 0 {
		t.Errorf("the engine failed %d job(s) because a module refused them", st.Refused)
	}
	s.requireBooksScoped("after the held job", kj.ID)
	s.requireBooksScoped("after the twin", tj.ID)
	t.Logf("NOTE jlpc pause and resume: the held job ended succeeded: paused->running once, running->completing->succeeded once, one module_paused and one module_resumed note, %d message(s) for its %d recipients and %d for the twin's %d, results %d for %d rows; the second engine converged without a second note or a second pause call",
		jlpcMsgs(end, k.Addrs), jlpcHeldRows, jlpcMsgs(end, tw.Addrs), jlpcTwinRows, len(s.results(kj.ID)), h.Rows)
}

// jlpcRunCancelPaused is TestJobsLive_M3_CancelAPausedRunningJob / TestJobsLive_M2_CancelAPausedRunningJob.
func jlpcRunCancelPaused(r *jlpcRig) {
	t, s := r.T, r.S
	k := r.New("jlck", jlpcCancelRows)
	c := r.New("jlcc", jlpcCtlRows)
	all := append(append([]string(nil), k.Addrs...), c.Addrs...)
	r.Mode(all)
	diagK, diagC := r.diagOf(k), r.diagOf(c)

	// ---- K: a job held while running, with rows queued at the module and rows never handed over ----
	h := r.holdOne(k, "cancel a paused job: the cancelled job", true)
	kj := h.Job
	rs0, ch0 := r.requireHoldsCredits(k, kj.ID, h.Preview, "the held job before the cancel")

	// the database refuses a direct move of a job the module holds to cancelled, for every actor that may cancel; the API's word for the constraint is submitted_job_ends_through_the_engine (internal/api, errors_test.go)
	jpcRefuseMove(t, s.admin, kj.ID, jobs.StatusCancelled, "user", k.User, jobs.ReasonSubmittedJobCancelsThroughTheEngine)
	jpcRefuseMove(t, s.admin, kj.ID, jobs.StatusCancelled, "admin", s.adminID, jobs.ReasonSubmittedJobCancelsThroughTheEngine)
	if st := s.status(kj.ID); st != jobs.StatusPaused {
		t.Fatalf("a refused raw move left the held job %s", st)
	}

	// ---- the user cancels the PAUSED job: it goes to cancelling, never straight to cancelled ----
	cj, err := s.store.Cancel(s.ctx, k.User, kj.ID)
	if err != nil || cj.Status != jobs.StatusCancelling || cj.PausedFrom != "" || cj.CancelRequestedAt == nil {
		t.Fatalf("the user's cancel of the paused running job answered %+v (error %v), want cancelling with the request recorded", cj, err)
	}
	if n := s.moves(kj.ID, jobs.StatusCancelled); n != 0 {
		t.Fatalf("the store took a job the module holds straight to cancelled (%d move(s))", n)
	}
	if n := s.jlpcPair(kj.ID, jobs.StatusPaused, jobs.StatusCancelling); n != 1 || s.movesBy(kj.ID, jobs.StatusCancelling, "user") != 1 {
		t.Fatalf("the held job moved paused -> cancelling %d time(s), want once by the user", n)
	}
	if rs, ch := r.requireHoldsCredits(k, kj.ID, h.Preview, "the cancelling job"); rs != rs0 || ch != ch0 {
		t.Fatalf("the reserve moved with the cancel request alone: (%d, %d) -> (%d, %d): the engine releases it, not the store", rs0, ch0, rs, ch)
	}
	if av, rv := s.balance(k.User, r.Module); r.Metered && rv == 0 {
		t.Fatalf("the cancelling job's reserve is already gone (available %d reserved %d): it stays held until the module has answered", av, rv)
	}
	// a second cancel is refused and changes nothing
	before := s.events(kj.ID)
	_, err = s.store.Cancel(s.ctx, k.User, kj.ID)
	if !errors.Is(err, jobs.ErrNotAllowed) {
		t.Fatalf("a second cancel of a cancelling job answered %v, want ErrNotAllowed", err)
	}
	jeNoRawSQL(t, "the second cancel", err)
	if after := s.events(kj.ID); fmt.Sprint(after) != fmt.Sprint(before) || s.status(kj.ID) != jobs.StatusCancelling {
		t.Fatalf("a refused second cancel changed the job's history or status")
	}

	// ---- the engine tells the module, records what finished, ends the job; nothing is sent after the module reported complete ----
	mark := r.Tr.mark()
	s.tickToStatus(kj.ID, jobs.StatusCancelled, 4*time.Minute, diagK)
	endedAt := time.Now()
	if n := r.Tr.since(mark, kj.ID, "cancel", "ok"); n < 1 {
		t.Fatalf("the engine never asked the module to cancel (%d successful call(s))", n)
	}
	atEnd := r.Sink()
	for time.Since(endedAt) < r.Window {
		r.tick() // an ended job is none of the engine's work: a tick must not resubmit, recancel or settle it again
		if d := jlpcSinkDiff(atEnd, r.Sink(), k.Addrs); len(d) != 0 {
			t.Fatalf("the sink changed for the cancelled job's recipients after the module reported complete: %s", strings.Join(d, "; "))
		}
		time.Sleep(500 * time.Millisecond)
	}
	// the partition at the module: what finished before the pause is sent, what was queued is cancelled, nothing else
	rep := r.Report(k, kj.ID)
	var sent, cancelled int
	for _, it := range rep.Items {
		switch it.State {
		case "sent":
			sent++
		case "cancelled":
			cancelled++
		default:
			t.Fatalf("an item is reported %s after the cancel: want sent or cancelled", it.State)
		}
	}
	if int64(sent+cancelled) != h.Handed || sent < 1 || cancelled < 1 || rep.Paused || !rep.Progress.IsComplete {
		t.Fatalf("the module's report after the cancel holds %d sent and %d cancelled of the %d handed over (paused %v, complete %v), want at least one of each, all of them final and the hold released", sent, cancelled, h.Handed, rep.Paused, rep.Progress.IsComplete)
	}
	// the rows never handed over are ended by the platform: free cancelled results (P-01) and the terminal marker on their items
	var platformCancelled int64
	for _, row := range s.rows(kj.ID) {
		if row.State == "pending" && !row.Submitted {
			platformCancelled++
		}
	}
	if platformCancelled != h.Unsent {
		t.Fatalf("%d pending row(s) have no hand-over stamp at the end, %d were never handed over before the pause", platformCancelled, h.Unsent)
	}
	if n := s.count("count the platform's own cancelled results", `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'platform' AND module_state = 'cancelled' AND decision = 'free' AND unit IS NULL AND rule = 'P-01'`, kj.ID); n != h.Unsent {
		t.Fatalf("the job holds %d platform cancelled result(s) (P-01, free), want %d (one for each row never handed to the module)", n, h.Unsent)
	}
	if n := s.count("count the items marked cancelled", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND status = 'cancelled'`, kj.ID); n != h.Unsent {
		t.Fatalf("%d item(s) of the campaign carry the platform's cancelled marker, want %d (the rows never handed over)", n, h.Unsent)
	}
	r.Final(k, kj.ID, h.Preview, true)
	if n := r.ModuleJobs(k, kj.ID); int64(n) != h.Handed {
		t.Fatalf("the module holds %d job(s) of the cancelled campaign, want %d: a cancel hands nothing more over", n, h.Handed)
	}
	s.requireMoves(kj.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusPaused: 1, jobs.StatusCancelling: 1, jobs.StatusCompleting: 1, jobs.StatusCancelled: 1})
	for _, x := range []struct {
		from, to, by string
	}{{jobs.StatusRunning, jobs.StatusPaused, "user"}, {jobs.StatusPaused, jobs.StatusCancelling, "user"}, {jobs.StatusCancelling, jobs.StatusCompleting, "engine"}, {jobs.StatusCompleting, jobs.StatusCancelled, "engine"}} {
		if n := s.jlpcPair(kj.ID, x.from, x.to); n != 1 || s.movesBy(kj.ID, x.to, x.by) != 1 {
			t.Errorf("the cancelled job moved %s -> %s %d time(s), want exactly once by the %s\n%s", x.from, x.to, n, x.by, strings.Join(s.events(kj.ID), "\n"))
		}
	}
	if n := s.jlpcPair(kj.ID, jobs.StatusPaused, jobs.StatusCancelled); n != 0 {
		t.Errorf("%d direct move(s) from paused to cancelled: a job the module holds ends through the engine", n)
	}
	if np, nr := s.jlpcNotes(kj.ID, jobs.NoteModulePaused), s.jlpcNotes(kj.ID, jobs.NoteModuleResumed); np != 1 || nr != 1 {
		t.Errorf("the cancelled job's history holds %d module_paused and %d module_resumed note(s), want one each (the cancel ends the module's hold)", np, nr)
	}
	if n := s.eventKindCount(kj.ID, "flag") + s.eventKindCount(kj.ID, "stale"); n != 0 {
		t.Errorf("the cancelled job raised %d flag or stale event(s)\n%s", n, strings.Join(s.events(kj.ID), "\n"))
	}
	if s.invented(kj.ID) != 0 {
		t.Errorf("the cancelled job holds an infrastructure_failure or unmapped result")
	}
	t.Logf("NOTE jlpc cancel of a paused job: paused -> cancelling (user) -> completing -> cancelled (engine); module: %d sent (in flight at the pause, finished once) and %d cancelled while queued; %d row(s) never handed over ended by the platform (P-01, free); the sink did not change for %s after the module reported complete", sent, cancelled, h.Unsent, r.Window)

	// ---- THE CONTROL: the same kind of job, paused and then RESUMED, ends succeeded with every recipient sent once ----
	hc := r.holdOne(c, "cancel a paused job: the control, resumed", false)
	cj2 := hc.Job
	if _, err := s.store.Resume(s.ctx, c.User, cj2.ID); err != nil {
		t.Fatalf("the control's resume: %v", err)
	}
	s.tickToStatus(cj2.ID, jobs.StatusSucceeded, 6*time.Minute, diagC)
	r.Final(c, cj2.ID, hc.Preview, false)
	end := r.Sink()
	for i, a := range c.Addrs {
		if end.Msgs[a] != 1 {
			t.Fatalf("recipient %d of the control job has %d message(s) at the sink, want exactly one: without the cancel every row is sent", i+1, end.Msgs[a])
		}
	}
	if n := s.moves(cj2.ID, jobs.StatusCancelling) + s.moves(cj2.ID, jobs.StatusCancelled); n != 0 {
		t.Fatalf("the control job moved to cancelling or cancelled %d time(s)", n)
	}
	s.requireMoves(cj2.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 2, jobs.StatusPaused: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	s.requireBooksScoped("after the cancelled job and its control", cj2.ID)
	t.Logf("NOTE jlpc cancel control: the same job, paused and then resumed, ended succeeded with %d message(s) for its %d recipients: the cancelled job's cancelled items are the cancel's doing", jlpcMsgs(end, c.Addrs), jlpcCtlRows)
}

// ---- a context helper the scenarios share ----------------------------------------------------------------------------------------------------------------------------------------

// jlpcBudget is the context of a scenario: the setup of a module 3 tenant takes up to a minute (the entitlement is delivered by the platform service), and the held job and its twin end in minutes.
func jlpcBudget(t *testing.T) context.Context { return jlCtx(t, 16*time.Minute) }
