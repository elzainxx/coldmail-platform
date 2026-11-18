//go:build harness

package harness

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_AccountEventsOfAnEnforcedHealthAction (Phase 3 close-out D3, owner decision OD-10): module 3's account events observed NON-EMPTY with a real health action in them, through the adapter.
//
// THE ACTION. A send whose AUTH the provider refuses is an `auth_failure`, and module 3 quarantines the account at once for it (a single one, regardless of any streak or rate: dispatch/account_effect.go,
// quarantineDecision row 1), if autoquarantine_mode is `enforce`. The decision is written, in the SAME transaction as the requeue of the job, as an account_events row with actor `system`, action
// `quarantine`, flags before (not quarantined) and after (quarantined) and the reason `auth_failure`, and the account is stored quarantined with quarantine_reason `auth_failure`. At the seeded mode `shadow` the
// same decision is recorded as an OBSERVATION only: actor `system`, action `updated`, the flags before and after EQUAL (nothing changed) and the reason `shadow_would_quarantine: trigger=auth_failure`;
// the account is NOT quarantined and keeps sending. The harness runs at shadow (module 3's launch-safety default); this test sets enforce for one account and puts the seeded value back (withDial restores it
// the moment the flow ends, whatever happens in it).
//
// WHAT IT DRIVES. Two tenants of one account each, one item each. For each: the account is created and verified while the controllable sink behaves; the sink is then switched to refuse AUTH (535, Mode.FailAuth: the
// worker's AUTH at SEND time is refused too); one job is run through the adapter; the test waits until the job shows a failed attempt (the outcome and the account effect are one transaction, so the effect is
// committed when the failure is visible); the sink is switched back at once and the tenant is suspended through module 3's operator route when the flow ends (the item is retried every 30 s; nothing of it may
// be claimed again). The ENFORCE tenant is the subject, the SHADOW tenant is the paired control: the same flow with the one dial changed.
//
// WHAT IT ASSERTS, through the adapter (and, as ground truth beside it, module 3's own tables):
//
//	enforce   the account's events page is NON-EMPTY, oldest first, every row the tenant's own; it holds exactly one `quarantine` row, actor system, flags before {quarantined false} and after {quarantined true},
//	          reason in the closed set of quarantine reasons and equal to auth_failure; GetAccount and the account list say quarantined with reason auth_failure and a quarantined_at; the page is the module's table, row
//	          for row (the count equals SELECT count(*) FROM send_byo.account_events for the account); another tenant reading the same account id gets the documented EMPTY page
//	shadow    the account's page holds NO `quarantine` row and exactly one observation row (`updated`, system, flags before = flags after, quarantined false, reason `shadow_would_quarantine: trigger=auth_failure`
//	          naming the SAME word the enforced quarantine named); the account is NOT quarantined (adapter and module 3's table); its job failed the same way (a failed attempt with an error class), so the only difference is the dial
func TestM3Adapter_AccountEventsOfAnEnforcedHealthAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	t.Cleanup(func() { e.restoreSink() }) // the refusing mode is gone when the test ends, whatever happened (prepare registers the same; this one is the one this test relies on and says so)

	enf := e.driveAuthFailure(t, "qenf", "enforce")
	shd := e.driveAuthFailure(t, "qshd", "shadow")

	// ---- enforce: the quarantine ----
	if len(enf.Events) == 0 {
		t.Fatalf("the events page of the quarantined account is EMPTY: module 3 writes an account_events row for a quarantine it enforces")
	}
	if enf.JobClass == "" || shd.JobClass == "" {
		t.Errorf("the two jobs show error classes %q (enforce) and %q (shadow), want both non-empty: each flow must have reported a failed attempt before the account was read", enf.JobClass, shd.JobClass)
	}
	var quarantines []m3.AccountEvent
	for _, ev := range enf.Events {
		if ev.Action == "quarantine" {
			quarantines = append(quarantines, ev)
		}
	}
	if len(quarantines) != 1 {
		t.Fatalf("the enforced account's page holds %d `quarantine` event(s), want exactly 1: %s", len(quarantines), m3dTrail(enf.Events))
	}
	q := quarantines[0]
	if q.AccountID != enf.Account.ID || q.Actor != "system" || q.From == nil || q.To == nil || q.From.Quarantined || !q.To.Quarantined || q.To.Paused || q.To.Disabled || q.From.Disabled || q.From.Paused {
		t.Errorf("the quarantine event is %+v (from %+v to %+v), want account %s, actor system, flags before {quarantined false} and after {quarantined true}, nothing else changed", q, q.From, q.To, enf.Account.ID)
	}
	if q.Reason == nil || *q.Reason != "auth_failure" || !slices.Contains(m3cQuarantineWords, *q.Reason) {
		t.Errorf("the quarantine event's reason is %s, want auth_failure, one of the module's closed words %v", m3aText(q.Reason), m3cQuarantineWords)
	}
	if q.CreatedAt.IsZero() || q.CreatedAt.Before(enf.Start.Add(-time.Minute)) || q.CreatedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("the quarantine event is dated %s, want an instant within this test's run (started %s)", q.CreatedAt.Format(time.RFC3339), enf.Start.Format(time.RFC3339))
	}
	for i, ev := range enf.Events {
		if ev.AccountID != enf.Account.ID || ev.ID == "" || ev.Actor == "" || ev.Action == "" || ev.Actor == "unrecognised" || ev.Action == "unrecognised" || (i > 0 && ev.CreatedAt.Before(enf.Events[i-1].CreatedAt)) {
			t.Errorf("event %d of the page is %+v, want the account's own event with plain words, oldest first", i+1, ev)
		}
		if ev.Reason != nil {
			if l := m3cLeaks(*ev.Reason); len(l) != 0 {
				t.Errorf("event %d's reason carries internal markers %v", i+1, l)
			}
		}
	}
	a := enf.Account
	if !a.Quarantined || a.QuarantineReason != "auth_failure" || a.QuarantinedAt == nil || a.Disabled || a.Paused {
		t.Errorf("the quarantined account reads quarantined %v reason %q at %v (disabled %v, paused %v), want quarantined, auth_failure, an instant, in service otherwise", a.Quarantined, a.QuarantineReason, a.QuarantinedAt, a.Disabled, a.Paused)
	}
	if enf.Listed == nil || !enf.Listed.Quarantined || enf.Listed.QuarantineReason != "auth_failure" {
		t.Errorf("the account list says %+v for the quarantined account, want the same as GetAccount (quarantined, auth_failure)", enf.Listed)
	}
	if !enf.SQL.Quarantined || enf.SQL.Reason != "auth_failure" || enf.SQL.EventsAll != len(enf.Events) || enf.SQL.EventsQuarantine != 1 {
		t.Errorf("module 3's own tables say quarantined %v reason %q, %d event row(s) of which %d quarantine; the adapter listed %d event(s) (%d quarantine): the page must be the table, row for row", enf.SQL.Quarantined, enf.SQL.Reason, enf.SQL.EventsAll, enf.SQL.EventsQuarantine, len(enf.Events), len(quarantines))
	}

	// ---- shadow: the control ----
	if len(shd.Events) == 0 {
		t.Fatalf("the events page of the shadow account is EMPTY: module 3 records the decision it did not act on")
	}
	var observations []m3.AccountEvent
	for _, ev := range shd.Events {
		if ev.Action == "quarantine" {
			t.Errorf("the SHADOW account's page holds a `quarantine` event %+v: at autoquarantine_mode=shadow module 3 decides and does not act", ev)
		}
		if ev.Reason != nil && strings.HasPrefix(*ev.Reason, "shadow_would_quarantine:") {
			observations = append(observations, ev)
		}
	}
	if len(observations) != 1 {
		t.Fatalf("the shadow account's page holds %d would-quarantine observation(s), want exactly 1 (the failure was reported once and the tenant is frozen before the 30 s retry): %s", len(observations), m3dTrail(shd.Events))
	}
	o := observations[0]
	if o.AccountID != shd.Account.ID || o.Actor != "system" || o.Action != "updated" || o.From == nil || o.To == nil || *o.From != *o.To || o.From.Quarantined {
		t.Errorf("the shadow observation is %+v (from %+v to %+v), want account %s, actor system, action updated, flags before EQUAL to flags after (nothing changed) and not quarantined", o, o.From, o.To, shd.Account.ID)
	}
	if want := "shadow_would_quarantine: trigger=" + m3aText(q.Reason); *o.Reason != want {
		t.Errorf("the shadow observation's reason is %q, want %q: a shadow row names the same word the enforced quarantine named", *o.Reason, want)
	}
	s := shd.Account
	if s.Quarantined || s.QuarantineReason != "" || s.QuarantinedAt != nil {
		t.Errorf("the SHADOW account reads quarantined %v reason %q at %v, want NOT quarantined: shadow decides, it does not act", s.Quarantined, s.QuarantineReason, s.QuarantinedAt)
	}
	if shd.SQL.Quarantined || shd.SQL.Reason != "" || shd.SQL.EventsQuarantine != 0 || shd.SQL.EventsAll != len(shd.Events) {
		t.Errorf("module 3's own tables say quarantined %v reason %q for the shadow account with %d `quarantine` row(s) and %d event row(s) (the adapter listed %d)", shd.SQL.Quarantined, shd.SQL.Reason, shd.SQL.EventsQuarantine, shd.SQL.EventsAll, len(shd.Events))
	}

	// ---- the page is the tenant's: another tenant reading the enforced account's events gets the documented empty page ----
	foreign, err := shd.Tenant.C.AccountEvents(ctx, enf.Account.ID, "")
	if err != nil || len(foreign.Items) != 0 {
		t.Errorf("the other tenant reading the enforced account's events got %d event(s) (error %v), want the documented empty page: events are scoped to the tenant", len(foreign.Items), err)
	}
	t.Logf("NOTE m3a events enforce: %d event(s): %s; the account reads quarantined=%v reason=%s; the shadow control: %d event(s): %s; quarantined=%v; the jobs showed error class %s (enforce) and %s (shadow)", len(enf.Events), m3dTrail(enf.Events), a.Quarantined, a.QuarantineReason, len(shd.Events), m3dTrail(shd.Events), s.Quarantined, enf.JobClass, shd.JobClass)
}

// m3cQuarantineWords is the closed set of quarantine reasons, from the module's documentation (api-contract.md, config-reference.md: auth_failure, consecutive_failures, failure_rate; manual by an operator).
var m3cQuarantineWords = []string{"auth_failure", "consecutive_failures", "failure_rate", "manual"}

// m3dHealth is what one flow of the test saw.
type m3dHealth struct {
	Tenant   *m3aTenant
	Account  m3.Account
	Listed   *m3.Account
	Events   []m3.AccountEvent
	JobClass string
	Start    time.Time
	SQL      struct {
		Quarantined      bool
		Reason           string
		EventsAll        int
		EventsQuarantine int
	}
}

// driveAuthFailure runs the flow in the doc of the test for one tenant at the given autoquarantine_mode and returns what was seen. The dial is restored when the flow ends and the sink's mode is switched
// back the moment the failed attempt is visible; the tenant is suspended when this function returns (after every read).
func (e *m3aEnv) driveAuthFailure(t *testing.T, name, mode string) m3dHealth {
	t.Helper()
	h := m3dHealth{Start: time.Now()}
	tn := e.seedTenant(t, name, []string{"authfail"})
	h.Tenant = tn
	defer e.freeze(t, tn, "m3 adapter harness: the queued item of the health action flow must never be claimed again")
	e.setPacing(t, tn, 1, 0, 0)
	acct := e.createVerified(t, tn, "1", e.sinkIP)
	e.withDial(t, "autoquarantine_mode", `"`+mode+`"`, func() {
		e.setMode(t, m3hMode{FailAuth: true}) // every AUTH, the worker's at SEND time too, is answered 535
		defer e.setMode(t, m3hMode{})
		res, err := tn.C.Run(e.ctx, tn.Items[0].ID)
		if err != nil || !res.Created {
			t.Fatalf("Run of the item (%s) answered %+v (error %v), want a new job", name, res, err)
		}
		var job sendapi.Item
		m3lWait(t, e.ctx, fmt.Sprintf("the job of %s shows a failed attempt (the sink refuses AUTH)", name), 90*time.Second, 500*time.Millisecond, func() (bool, string) {
			one, err := tn.C.ReportJob(e.ctx, tn.Items[0].ID)
			if err != nil {
				t.Fatalf("ReportJob of %s failed (%s): %s", name, modclient.KindOf(err), e.raw.scrub(err.Error()))
			}
			job = one
			return one.AttemptCount >= 1 && one.ErrorClass != nil, fmt.Sprintf("state %s attempt_count %d error class %s", one.State, one.AttemptCount, m3aText(one.ErrorClass))
		})
		h.JobClass = m3aText(job.ErrorClass)
	})
	// the AUTH refusal is over (the sink is back) and the tenant is frozen when this function returns: every read below is a few milliseconds, the retry is 30 s away
	got, err := tn.C.GetAccount(e.ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount of %s failed (%s): %s", name, modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	h.Account = got
	all, err := tn.C.AllAccounts(e.ctx)
	if err != nil {
		t.Fatalf("AllAccounts of %s failed: %s", name, e.raw.scrub(err.Error()))
	}
	for i := range all {
		if all[i].ID == acct.ID {
			h.Listed = &all[i]
		}
	}
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := tn.C.AccountEvents(e.ctx, acct.ID, cursor)
		if err != nil {
			t.Fatalf("AccountEvents of %s failed (%s): %s", name, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		h.Events = append(h.Events, page.Items...)
		if page.NextCursor == "" || pages >= 5 {
			break
		}
		cursor = page.NextCursor
	}
	err = e.db.QueryRow(e.ctx, `SELECT a.quarantined, COALESCE(a.quarantine_reason::text, ''),
			(SELECT count(*) FROM send_byo.account_events WHERE account_id = a.id),
			(SELECT count(*) FROM send_byo.account_events WHERE account_id = a.id AND actor::text = 'system' AND action::text = 'quarantine')
		FROM send_byo.accounts a WHERE a.id = $1::uuid`, acct.ID).Scan(&h.SQL.Quarantined, &h.SQL.Reason, &h.SQL.EventsAll, &h.SQL.EventsQuarantine)
	if err != nil {
		t.Fatalf("read module 3's own record of account %s: %v", name, err)
	}
	return h
}

// freeze suspends the tenant through module 3's operator route: nothing of its queue is claimed again.
func (e *m3aEnv) freeze(t *testing.T, tn *m3aTenant, why string) {
	t.Helper()
	status, body := e.raw.do(t, e.raw.m3URL, http.MethodPost, "/v1/admin/users/"+tn.UserID+"/suspend", e.raw.adminKey, map[string]string{"reason": why})
	e.raw.expect(t, "POST /v1/admin/users/{"+tn.Name+"}/suspend", status, body, http.StatusOK, "")
}

// m3dTrail is the page as one line: actor:action (reason) per event, oldest first.
func m3dTrail(evs []m3.AccountEvent) string {
	parts := make([]string, len(evs))
	for i, ev := range evs {
		parts[i] = ev.Actor + ":" + ev.Action
		if ev.Reason != nil {
			parts[i] += " (" + *ev.Reason + ")"
		}
	}
	return strings.Join(parts, ", ")
}
