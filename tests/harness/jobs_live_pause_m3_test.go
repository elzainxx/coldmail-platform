//go:build harness

package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// jlpcM3Rig is the pause scenarios' rig for module 3: its REAL Control, its three workers and the controllable sink, the engine of this process with a submit batch of `batch`, and tenants with ONE lane (a second's
// pause between two sends of the lane) and an entitlement delivered by the platform service, so that the items of a job go one at a time, each held at the sink for jlpcHold.
func jlpcM3Rig(t *testing.T, ctx context.Context, e *m3aEnv, batch int) *jlpcRig {
	t.Helper()
	s, tr := newJlStackWith(t, ctx, jlOpts{Cfg: func(sec config.Secret) config.Config { return jlConfig(t, sec) }, MaxStartDelay: jlpcStartDelay})
	cfg := jlpcEngineConfig(batch, jlpcStale)
	s.jlpcUseEngine(tr, cfg)
	tick := time.Duration(e.dialInt(t, "dispatch_tick_interval_ms")) * time.Millisecond
	tenant := func(sn *jlpcSender) *m3aTenant { return sn.Handle.(*m3aTenant) }
	return &jlpcRig{
		T: t, S: s, Tr: tr, Module: jobs.ModuleSendBYO, Metered: false, Cfg: cfg,
		Window: max(3*tick+jlpcWorkerPoll, jlpcMinWindow), // more than three of module 3's dispatch ticks and the worker's poll
		New: func(name string, n int) *jlpcSender {
			tn := s.m3User(e, name, true)
			e.setPacing(t, tn, 1, 1, 1)
			return &jlpcSender{Name: name, User: tn.UserID, Addrs: jlM3Addrs(tn, n), Handle: tn}
		},
		Mode:   func([]string) { e.setMode(t, m3hMode{DataDelayMs: int(jlpcHold / time.Millisecond)}) },
		Attach: func(*jlpcSender, string) {}, // module 3's tenant needs no list of items: its views are read by the tenant's prefix
		Report: func(sn *jlpcSender, job string) sendapi.CampaignReport {
			rep, err := tenant(sn).C.ReportCampaign(ctx, job)
			if err != nil {
				t.Fatalf("ReportCampaign of the job through the adapter failed: %s", e.raw.scrub(err.Error()))
			}
			return rep
		},
		Sink: func() jlpcSink {
			v := e.sinkView(t)
			e.requireNoDups(t, v)
			out := jlpcSink{Msgs: map[string]int{}, Rcpt: map[string]jlpcRcpt{}}
			for a, n := range v.Counts {
				out.Msgs[a] = n
			}
			for a, c := range v.Rcpt {
				out.Rcpt[a] = jlpcRcpt{Accepted: c.Accepted, Rejected: c.Rejected, Deferred: c.Deferred}
			}
			return out
		},
		Held: func() []string {
			var out []string
			for _, h := range e.inflight(t).Held {
				for _, a := range h.To {
					out = append(out, strings.ToLower(strings.TrimSpace(a)))
				}
			}
			return out
		},
		ModuleJobs: func(sn *jlpcSender, job string) int { return len(s.m3JobsOfCampaign(e, tenant(sn), job)) },
		Diag:       func(sn *jlpcSender) string { return e.diagnostics(t, tenant(sn)) },
		Final: func(sn *jlpcSender, job string, _ jobs.Preview, cancelled bool) {
			tn := tenant(sn)
			view, _, _ := s.m3View(e, tn, job)
			if cancelled {
				view.Rows = jlpcNeverHandedAsCancelled(view.Rows)
			}
			if problems := jlM3ThreeWay(view); len(problems) != 0 {
				t.Fatalf("module 3's report, the sink, module 3's own table and the job's results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
			}
			if _, _, _, held := s.reserveOf(job); held {
				t.Fatalf("the module 3 job holds a reserve: module 3 is not metered")
			}
			m3aProblems(t, "the books of the user", m3aCompareBooks(e.books(t, tn.UserID)))
			s.requireBooksScoped("after the module 3 job", job)
		},
	}
}

// TestJobsLive_M3_PauseAndResumeARunningJob (Phase 4 close-out D1, live: the engine of this process, the REAL module 3, its three workers and the controllable sink).
//
// THE SCENARIO. User K sends a job of twelve recipients (and one malformed row and one repeat, which the platform ends itself) with ONE lane and the sink holding every send for three seconds, so an item takes about
// five. The engine's submit batch is FOUR (jlpcBatch), and why: the engine hands the module one batch when it starts a job and the REST of the rows at its next tick, so the test ticks once, waits (without ticking)
// until the sink holds one of K's sends and pauses then: one item is in flight, three are queued at module 3 and EIGHT rows were never handed to it. The twin T (ten recipients, never paused) is started after the
// pause. The engine's staleness limit is TWO SECONDS (the service's is fifteen minutes) and its store's max start delay FIFTEEN, so that "a held job is not stale and does not expire" can fail.
//
// WHAT MUST HOLD, each asserted from the platform's tables, module 3's own report and the sink:
//
//	the API's answer     the job is `paused` with paused_from `running` the moment the store answers; the engine has not yet called the module (no pause call, and the module's own report says paused false: the lag is real)
//	the module's answer  the engine's next tick calls the module's pause (a successful call), module 3's own report says `paused`, and ONE `module_paused` note is written, after the user's transition
//	in flight            the item in flight when the user paused finishes and is recorded and settled once (a free result, M3-99), the three-way comparison of the job's rows, results, module report and sink is clean
//	                     at three checkpoints (jlpcProblems), nothing else is claimed: the module's report keeps its four items (no further row is handed to it) and its own job table its four jobs
//	the window           for longer than three of module 3's dispatch ticks plus the worker's poll (at least ten seconds), with the engine ticking, the sink gains NOTHING for K's recipients (messages and RCPT answers,
//	                     compared recipient by recipient with a reading taken when the hold was complete) and the module's report stays paused with four items
//	the control          in the SAME window the SAME comparison over the twin's recipients finds messages arriving (the observation can fail), and the twin, a running job whose items take longer than the staleness
//	                     limit, raises `stale` events while the held job raises none between its pause and its resume
//	not expired          the held job's start deadline is in the past by the database's clock and the job is still paused: no expired, failed or cancelled move (the control of the expiry is the scheduled job paused
//	                     before its start, TestJobsLive_Expiry_* and TestJobsPauseCancel_*: a job paused while running is the only one that never expires)
//	credits and slot     module 3 holds no credits, so there is no reserve to keep (asserted: none exists); the slot is held: a second job of the same user is not confirmed (ErrSlotTaken, or module 3's own cause
//	                     campaign_active: the store asks the module first), nothing is held for it, and its draft is discarded
//	the restart          a SECOND engine, with no memory of the first, ticks eight times: the job is still paused, no second module_paused note, no pause call, the sink unchanged, the report still paused. (The
//	                     restart of the MODULE while paused is proved by module 3's own CI, tests/harness/phase4_pause.sh: the Control and the worker are restarted and the campaign is still held.)
//	the resume           the store answers running at once, the module's report still says paused (the lag), the engine then hands over the rest of the rows and tells the module (one `module_resumed` note); every
//	                     recipient of both jobs ends with exactly ONE message, results == rows, the moves are running -> paused once, paused -> running once, running -> completing -> succeeded once
func TestJobsLive_M3_PauseAndResumeARunningJob(t *testing.T) {
	ctx := jlpcBudget(t)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	jlpcRunPauseResume(jlpcM3Rig(t, ctx, e, jlpcBatch))
}

// TestJobsLive_M3_CancelAPausedRunningJob (Phase 4 close-out D1, live, REAL module 3): a job paused while running is cancelled by its user.
//
// THE SCENARIO. User K sends nine recipients (and a malformed row and a repeat) with a submit batch of THREE: one tick hands the module three, the user pauses while one is in flight, the engine tells the module,
// the item in flight finishes (sent, once) and two items stay queued at the module while six rows were never handed to it. Then the user cancels.
//
// WHAT MUST HOLD. The database refuses a raw move of the held job to `cancelled` for the user and for an administrator (23001, job_submitted_pause_cancels_through_the_engine: the API's word for that constraint is
// submitted_job_ends_through_the_engine, proved in internal/api); the store's cancel moves it `paused -> cancelling` (never `cancelled`), once, by the user; a second cancel is refused and changes nothing; the engine
// calls the module's cancel, records what the module reports (two items cancelled while queued, free by M3-01, the item in flight sent) and ends the rows no module was given as the platform's own free cancelled
// results (P-01, six of them, their items marked cancelled); the job ends `cancelled` through completing, by the engine, once; the sink does not change AT ALL for the whole window after the module reported complete;
// the module's own report, the platform's results and the sink agree item by item (jlM3ThreeWay with the rows never handed over counted as the platform's); the history holds one module_paused and one module_resumed
// note (a cancel ends the module's hold) and no flag or stale event; module 3 holds no credits, so there is no reserve to release (asserted: none).
//
// THE CONTROL is a second user's job of the same shape, paused while running and then RESUMED: it ends `succeeded` with every one of its five recipients sent exactly once and no move to cancelling, so the cancelled
// items of the first job are the cancel's doing.
func TestJobsLive_M3_CancelAPausedRunningJob(t *testing.T) {
	ctx := jlpcBudget(t)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	jlpcRunCancelPaused(jlpcM3Rig(t, ctx, e, jlpcCancelBatch))
}
