//go:build harness && m2worker

package harness

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// jlpcM2Tenant is the module 2 side of a sender of the pause scenarios: its tenant (a user, a pool of its own and an account naming the sink) and the kind of each recipient (the sink is told to hold them all).
type jlpcM2Tenant struct {
	tn     *m2wTenant
	kindOf map[string]string
}

// jlpcM2Rig is the pause scenarios' rig for module 2 (metered): its REAL Control, its REAL worker (one, so one send at a time; one lane per user) and the controllable sink, the engine of this process with a submit
// batch of `batch`, and credits for every sender. The sink holds the final reply of every send to a recipient of the scenario for jlpcHold, so an item takes about that and the worker serves one job at a time:
// while the held job is paused the worker serves the twin alone.
func jlpcM2Rig(t *testing.T, batch int) *jlpcRig {
	t.Helper()
	const grant = 1000
	m := newJlM2Env(t)
	r := m.r
	s, tr := newJlStackWith(t, r.ctx, jlOpts{
		Cfg: func(config.Secret) config.Config {
			return config.Config{M2URL: r.m2URL, M2ServiceToken: r.token, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
		},
		MaxStartDelay: jlpcStartDelay,
	})
	cfg := jlpcEngineConfig(batch, jlpcStale)
	s.jlpcUseEngine(tr, cfg)
	tick := 0 * time.Millisecond // module 2's dispatch tick is the dial this run set (m2wDials): read from the list that sets it, never assumed
	for _, d := range m2wDials {
		if d.Key == "dispatch_tick_interval_ms" {
			n, err := strconv.Atoi(d.Value)
			if err != nil || n < 1 {
				t.Fatalf("the dial dispatch_tick_interval_ms of m2wDials is %q, want a positive whole number of milliseconds", d.Value)
			}
			tick = time.Duration(n) * time.Millisecond
		}
	}
	if tick == 0 {
		t.Fatalf("m2wDials sets no dispatch_tick_interval_ms: the observation window cannot be sized from the module's dispatch tick")
	}
	tenant := func(sn *jlpcSender) *jlpcM2Tenant { return sn.Handle.(*jlpcM2Tenant) }
	return &jlpcRig{
		T: t, S: s, Tr: tr, Module: jobs.ModuleSendManaged, Metered: true, Grant: grant, Cfg: cfg,
		Window: max(3*tick+jlpcWorkerPoll, jlpcMinWindow), // more than three of module 2's dispatch ticks and the worker's poll
		New: func(name string, n int) *jlpcSender {
			tn := m.tenant(s, name)
			s.grant(tn.UserID, jobs.ModuleSendManaged, grant)
			addrs, kindOf := m.recipients(tn, slices.Repeat([]string{"hold"}, n))
			return &jlpcSender{Name: name, User: tn.UserID, Addrs: addrs, Handle: &jlpcM2Tenant{tn: tn, kindOf: kindOf}}
		},
		Mode: func(all []string) {
			r.setMode(t, m2wMode{DataDelayMs: int(jlpcHold / time.Millisecond), DelayRecipients: all})
		},
		Attach: func(sn *jlpcSender, job string) { m.attach(tenant(sn).tn, job, s.rows(job), tenant(sn).kindOf) },
		Report: func(sn *jlpcSender, job string) sendapi.CampaignReport { return m.report(sn.User, job) },
		Sink: func() jlpcSink {
			v := r.sinkView(t)
			out := jlpcSink{Msgs: map[string]int{}, Rcpt: map[string]jlpcRcpt{}}
			for a, ms := range v.Msgs {
				out.Msgs[a] = len(ms)
			}
			for a, c := range v.Rcpt {
				out.Rcpt[a] = jlpcRcpt{Accepted: c.Accepted, Rejected: c.Rejected, Deferred: c.Deferred}
			}
			return out
		},
		Held: func() []string {
			var out []string
			for _, h := range r.inflight(t).Held {
				for _, a := range h.To {
					out = append(out, strings.ToLower(strings.TrimSpace(a)))
				}
			}
			return out
		},
		ModuleJobs: func(_ *jlpcSender, job string) int {
			var n int
			if err := r.db.QueryRow(r.ctx, `SELECT count(*) FROM send_managed.jobs WHERE campaign_id = $1::uuid`, job).Scan(&n); err != nil {
				t.Fatalf("count module 2's own jobs of the campaign: %v", err)
			}
			return n
		},
		Diag: func(sn *jlpcSender) string { return r.diagnostics(t, tenant(sn).tn) },
		Final: func(sn *jlpcSender, job string, pv jobs.Preview, cancelled bool) {
			v := m.view(s, tenant(sn).tn, job)
			if cancelled {
				v.Rows = jlpcNeverHandedAsCancelled(v.Rows)
			}
			if problems := jlM2ThreeWay(v); len(problems) != 0 {
				t.Fatalf("module 2's report, the results, the ledger and the sink disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
			}
			m.requireBooksOfJob(s, sn.User, job, pv, pv.Units, grant, v)
		},
	}
}

// TestJobsLive_M2_PauseAndResumeARunningJob (Phase 4 close-out D1, live, METERED: the engine of this process, module 2's REAL Control and worker, the controllable sink): the scenario of
// TestJobsLive_M3_PauseAndResumeARunningJob (jobs_live_pause_m3_test.go has its full statement) against module 2, with the money of module 2 added.
//
// User K sends twelve recipients (and a malformed row and a repeat) with the sink holding every send for three seconds and a submit batch of four: one tick hands the module four rows, the sink shows one in flight,
// the user pauses, and eight rows were never handed over. The twin T (ten recipients, never paused, another user: module 2 allows one active campaign per user) starts after the pause and shares the one worker.
//
// THE MONEY, in addition: while K is held its reserve is whole (the preview's credits at the dearest reachable unit), what finished before the pause is charged ONCE at the frozen price (m2_accepted), nothing is
// released and nothing more is charged for the whole window (the books are read at its start and its end), the balance's reserved part is the reserve less what was charged, and a second confirm for the same
// user and module is ErrSlotTaken with nothing held for it; at the end the reserve is charged what module 2's own report implies and the remainder released ONCE (at most one release row), the ledger holds ONE charge row per
// sent item, results == rows, and every recipient of both jobs has exactly ONE message at the sink. The restart of module 2 itself while paused is proved by module 2's own CI (its SUITE 14 restarts the Control and the
// worker and sees the campaign still held).
func TestJobsLive_M2_PauseAndResumeARunningJob(t *testing.T) {
	jlpcRunPauseResume(jlpcM2Rig(t, jlpcBatch))
}

// TestJobsLive_M2_CancelAPausedRunningJob (Phase 4 close-out D1, live, METERED, module 2's REAL Control and worker): the scenario of TestJobsLive_M3_CancelAPausedRunningJob against module 2. The user's cancel of
// the paused job moves it `paused -> cancelling` (the database refuses the direct move to cancelled for the user and for an administrator), the engine calls module 2's cancel (which also releases the campaign's pause),
// the queued items are cancelled free by M2-08, the item that was in flight is charged once, the rows never handed over are ended by the platform (P-01, free), the unspent reserve is released ONCE, nothing
// reaches the sink after the module reported complete, and module 2's report, the results, the ledger and the sink agree item by item. THE CONTROL is another user's job paused and then resumed: it ends succeeded with
// every recipient sent once and no move to cancelling.
func TestJobsLive_M2_CancelAPausedRunningJob(t *testing.T) {
	jlpcRunCancelPaused(jlpcM2Rig(t, jlpcCancelBatch))
}
