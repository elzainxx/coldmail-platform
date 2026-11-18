//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
)

// TestM3Live_SeveralLanesAndAHostileSink is the Phase 2 close-out proof E2 for module 3 (its real Control and its three real workers, in the harness stack): several lanes of one user, a sink that
// REJECTS, DEFERS and DELAYS on command, a halt and a resume under that sink, and what the charge table says about every item state module 3 shows.
//
// THE SINK. tests/harness/sink is a small SMTP server of our own (module 3's Mailpit can neither reject nor delay) with a control API. Module 3's worker validates the sink's self-signed certificate
// unless the SMTP host is `localhost`, `smtp-sink` or a PRIVATE ADDRESS (smtp.js:136-145), so every account here names the sink's private IP (resolved from the service name `smtp-ctl`, asserted to be in
// 10/8, 172.16/12 or 192.168/16), port 1025. The From-gate is off (run.sh stage 5b), as in the other live test.
//
// FOUR TENANTS, ONE CAMPAIGN EACH (module 3 allows one active campaign per user), each with the per-user override concurrency_per_user=3 and a send delay of 0-1 s, so three lanes run on the three workers:
//
//	A  36 items  several lanes: every send is held 3 s at the sink so three are in flight at once; all three workers take part; every recipient exactly once.
//	B  24 items  a rejecting sink: 6 recipients are answered 550 at RCPT (module 3 must report them bounced, never retry them), 3 are answered 451 (module 3 must retry them, and they complete when the sink
//	             relents); the sink's multiset of recipients must equal exactly the items reported sent.
//	C  30 items  a delaying sink and a HALT: every send is held 4 s; once two are held the user is suspended through module 3's admin route; the held sends finish, nothing new is claimed, nothing is lost or
//	             failed, and after the reinstatement the SAME jobs complete, each recipient exactly once.
//	D  12 items  the control of C: never suspended, sends while C is halted (so C's silence is the suspension and not a stalled stack).
//
// WHY THE ADMIN ROUTE AND NOT THE ENTITLEMENT PATH for C's halt: the expiry and reconcile loops of the platform service run on their own clocks, so the instant at which the suspension lands relative to the
// sends in flight would be anyone's guess, and this test needs two sends held at that instant (a halt with nothing in flight proves nothing about the sends that must finish). The entitlement path itself
// (expiry, delivery, convergence, resume) is proved against the same module 3 by TestM3Live_ExpiredEntitlementHaltsSendingAndReenableResumes. No entitlement is enabled for these users on purpose: the
// platform's reconcile loop re-sends the wanted state of every user it knows and would undo a suspension made behind its back.
//
// Phases run in order and the first failure stops the test (a later phase assumes the earlier ones). Every wait is bounded and polls; every instant is the DATABASE's clock.
func TestM3Live_SeveralLanesAndAHostileSink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3hTestBudget)
	t.Cleanup(cancel)
	r := m3hNewRun(t, ctx)
	for _, p := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"1_preconditions", r.preconditions},
		{"2_seed", r.seed},
		{"3_lanes_and_accounts", r.lanesAndAccounts},
		{"4_several_lanes_on_three_workers", r.severalLanes},
		{"5_a_rejecting_sink", r.rejectingSink},
		{"6_a_delaying_sink_and_the_halt", r.delayingSinkAndHalt},
		{"7_module_3_is_not_metered", r.chargeDecisions},
	} {
		if !t.Run(p.name, p.fn) {
			t.FailNow() // fail fast: the next phase assumes this one
		}
	}
}

const (
	m3hTestBudget = 26 * time.Minute

	m3hLanes    = 3 // per-user override: concurrency_per_user
	m3hDelayMin = 0 // per-user override: send_delay_min_sec
	m3hDelayMax = 1 // per-user override: send_delay_max_sec

	m3hNA      = 36   // A: items
	m3hAHoldMs = 3000 // A: how long the sink holds every final reply
	m3hNB      = 24   // B: items
	m3hNC      = 30   // C: items
	m3hCHoldMs = 4000 // C and D: how long the sink holds every final reply
	m3hND      = 12   // D: items

	m3hHoldFor = 8 * time.Second // how long C's silence is observed at least
)

// B's items (0-based, in the order they are sent) that the sink answers 550 at RCPT, and 451.
var (
	m3hRejectIdx = []int{2, 6, 10, 14, 18, 22}
	m3hDeferIdx  = []int{4, 12, 20}
)

// ---- 1: preconditions ----

func (r *m3hRun) preconditions(t *testing.T) {
	// the platform: an admin (the first-admin stage created one) for the rolled-back ledger control of the last phase
	if err := r.db.QueryRow(r.ctx, "SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at LIMIT 1").Scan(&r.admin); err != nil {
		t.Fatalf("read an active admin from platform.admins (the first-admin stage must have run before this test): %v", err)
	}

	// module 3's schema and the platform's ledger: every table and column this test reads is inferred from the migrations. A wrong guess is found here, in the first second.
	r.requireColumns(t, "send_byo", "boxes", "id", "status", "last_heartbeat_at")
	r.requireColumns(t, "send_byo", "jobs", "id", "user_id", "campaign_id", "job_item_id", "recipient_email", "state", "attempt_count", "assigned_box_id", "last_error_class")
	r.requireColumns(t, "send_byo", "job_events", "job_id", "user_id", "from_state", "to_state", "box_id", "created_at")
	r.requireColumns(t, "send_byo", "send_logs", "job_id", "user_id", "recipient_email")
	r.requireColumns(t, "send_byo", "user_suspensions", "user_id", "suspended", "suspended_at", "reinstated_at")
	r.requireColumns(t, "send_byo", "user_config_overrides", "user_id", "concurrency_per_user", "send_delay_min_sec", "send_delay_max_sec")
	r.requireColumns(t, "platform", "ledger", "user_id", "module_id", "kind", "monetary", "acting_admin_id", "reason", "idempotency_key", "until_at")
	r.requireColumns(t, "platform", "balances", "user_id", "module_id")

	// module 3's three workers are registered and heartbeating (the compose --wait does not notice a dead one; run.sh stage 5b waited for each)
	rows, err := r.db.Query(r.ctx, "SELECT id::text FROM send_byo.boxes WHERE status = 'active' AND last_heartbeat_at > now() - interval '60 seconds'")
	if err != nil {
		t.Fatalf("read the active workers: %v", err)
	}
	active := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		active[strings.ToLower(id)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, id := range r.workerIDs {
		if !active[id] {
			t.Fatalf("module 3's worker %s has no heartbeat in the last 60 seconds (send_byo.boxes): three lanes need three live workers; the stack starts m3-worker, m3-worker-2 and m3-worker-3 in run.sh stage 5b", id)
		}
	}

	// the controllable sink: a clean slate, the default mode, nothing held; and a control that proves the instrument is strict: a misspelt mode field is refused and changes nothing
	r.ctl(t, http.MethodPost, "/api/reset", nil, http.StatusOK)
	r.setMode(t, m3hMode{})
	if v := r.inflight(t); v.Inflight != 0 || v.Peak != 0 || v.HeldTotal != 0 {
		t.Fatalf("the sink is not clean after a reset: %+v", v)
	}
	if status, body := r.env.do(t, r.ctlURL, http.MethodPost, "/api/mode", "", map[string]any{"reject_rcpts": []string{"typo@example.test"}}); status != http.StatusBadRequest {
		t.Fatalf("control: the sink accepted a mode with a misspelt field (HTTP %d: %s): a test could set a hostile mode that does nothing and pass", status, r.env.snip(body))
	}
	if msgs := r.sinkMessages(t); len(msgs) != 0 {
		t.Fatalf("the sink holds %d message(s) after a reset", len(msgs))
	}

	// the sink's private address: module 3's worker skips TLS validation only for `localhost`, `smtp-sink` and private addresses, so the accounts name the address and not the service name
	addrs, err := net.DefaultResolver.LookupHost(r.ctx, m3hCtlHost)
	if err != nil {
		t.Fatalf("cannot resolve the controllable sink %q from the test container: %v", m3hCtlHost, err)
	}
	for _, a := range addrs {
		if m3hPrivateIPv4(a) {
			r.sinkIP = a
			break
		}
	}
	if r.sinkIP == "" {
		t.Fatalf("%s resolves to %v and not one of these is in 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16: module 3's worker validates a certificate unless the SMTP host is a private address, and the sink's is self-signed, so verify-on-add would fail", m3hCtlHost, addrs)
	}

	// the credentials, paired controls so that the successful calls below mean something: the service token is not the admin key, and the admin key reaches the admin surface
	nobody := r.uuid(t)
	status, body := r.env.do(t, r.env.m3URL, http.MethodGet, "/v1/admin/users/"+nobody+"/config", r.env.token, nil)
	r.env.expect(t, "control: the SERVICE token presented to the ADMIN route (module 3's admin surface must be mounted and must reject a credential that is not the admin key)", status, body, http.StatusUnauthorized, "unauthenticated")
	status, body = r.env.do(t, r.env.m3URL, http.MethodGet, "/v1/admin/users/"+nobody+"/config", r.env.adminKey, nil)
	r.env.expect(t, "the ADMIN key on the admin route (an unknown user reads as not found)", status, body, http.StatusNotFound, "not_found")
	t.Logf("preconditions: admin %s; workers %s all heartbeating; the sink is clean and strict, at %s (private) ports 1025 (SMTP) and its control API; both credentials work and are distinct", r.admin, strings.Join(r.workerIDs, ", "), r.sinkIP)
}

// ---- 2: seed ----

// seed creates, as the superuser and for real (committed), four tenants: auth.users and public.users rows, one campaign each, and job items with DISTINCT recipients that carry this run's id in the local part.
func (r *m3hRun) seed(t *testing.T) {
	specs := []struct {
		name string
		n    int
		dst  **m3lTenant
	}{{"a", m3hNA, &r.a}, {"b", m3hNB, &r.b}, {"c", m3hNC, &r.c}, {"d", m3hND, &r.d}}
	// the ids first (the superuser connection is then used for nothing else while the seeding transaction is open)
	for _, sp := range specs {
		*sp.dst = &m3lTenant{Name: sp.name, UserID: r.uuid(t), CampaignID: r.uuid(t), Prefix: fmt.Sprintf("%s-%s-", sp.name, r.runID)}
		r.tenants = append(r.tenants, *sp.dst)
	}

	tx, err := r.db.Begin(r.ctx)
	if err != nil {
		t.Fatalf("begin the seeding transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.ctx)) }()
	for _, sp := range specs {
		tn := *sp.dst
		email := fmt.Sprintf("m3hostile-%s-%s@example.test", sp.name, r.runID)
		if _, err := tx.Exec(r.ctx, "INSERT INTO auth.users (id, email) VALUES ($1::uuid, $2)", tn.UserID, email); err != nil {
			t.Fatalf("seed auth.users for %s: %v", tn.Name, err)
		}
		if _, err := tx.Exec(r.ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)", tn.UserID, email); err != nil {
			t.Fatalf("seed public.users for %s: %v", tn.Name, err)
		}
		for i := 1; i <= sp.n; i++ {
			rcpt := fmt.Sprintf("%s%02d@example.test", tn.Prefix, i)
			var id string
			err := tx.QueryRow(r.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
				VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`,
				tn.CampaignID, tn.UserID, rcpt, fmt.Sprintf("[m3hostile %s] %s %02d", r.runID, tn.Name, i), fmt.Sprintf("<p>m3hostile %s %s %02d</p>", r.runID, tn.Name, i)).Scan(&id)
			if err != nil {
				t.Fatalf("seed public.job_items %d of %s: %v", i, tn.Name, err)
			}
			tn.ItemIDs = append(tn.ItemIDs, id)
			tn.Recipients = append(tn.Recipients, rcpt)
		}
	}
	if err := tx.Commit(r.ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}
	for _, tn := range r.tenants {
		if n := r.count(t, "count the seeded items", "SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND user_id = $2::uuid", tn.CampaignID, tn.UserID); n != len(tn.ItemIDs) {
			t.Fatalf("seeded %d job items for %s but the table holds %d", len(tn.ItemIDs), tn.Name, n)
		}
	}
	t.Logf("seed: run %s; A %s (%d items), B %s (%d), C %s (%d), D %s (%d)", r.runID, r.a.UserID, len(r.a.ItemIDs), r.b.UserID, len(r.b.ItemIDs), r.c.UserID, len(r.c.ItemIDs), r.d.UserID, len(r.d.ItemIDs))
}

// ---- 3: three lanes and one SMTP account each ----

func (r *m3hRun) lanesAndAccounts(t *testing.T) {
	for _, tn := range r.tenants {
		// three lanes, and 0-1 s between two sends of one lane: the pacing is real but never the bottleneck
		status, body := r.env.do(t, r.env.m3URL, http.MethodPut, "/v1/admin/users/"+tn.UserID+"/config", r.env.adminKey,
			map[string]int{"concurrency_per_user": m3hLanes, "send_delay_min_sec": m3hDelayMin, "send_delay_max_sec": m3hDelayMax})
		r.env.expect(t, "PUT the per-user pacing of "+tn.Name, status, body, http.StatusOK, "")
		var conc, lo, hi int
		err := r.db.QueryRow(r.ctx, `SELECT COALESCE(concurrency_per_user, -1), COALESCE(send_delay_min_sec, -1), COALESCE(send_delay_max_sec, -1)
			FROM send_byo.user_config_overrides WHERE user_id = $1::uuid`, tn.UserID).Scan(&conc, &lo, &hi)
		if err != nil {
			t.Fatalf("read back module 3's per-user pacing of %s: %v", tn.Name, err)
		}
		if conc != m3hLanes || lo != m3hDelayMin || hi != m3hDelayMax {
			t.Fatalf("module 3 stored concurrency_per_user=%d send_delay_min_sec=%d send_delay_max_sec=%d for %s, want %d, %d, %d", conc, lo, hi, tn.Name, m3hLanes, m3hDelayMin, m3hDelayMax)
		}

		from := fmt.Sprintf("sender-%s-%s@example.test", tn.Name, r.runID)
		status, body = r.env.do(t, r.env.m3URL, http.MethodPost, "/v1/send-byo/accounts", r.env.token, map[string]any{
			"user_id": tn.UserID, "smtp_host": r.sinkIP, "smtp_port": m3hSMTPPort, "smtp_username": from, "smtp_secret": m3lSinkPassword(t),
			"from_address": from, "from_name": "Harness",
		})
		r.env.expect(t, "POST the SMTP account of "+tn.Name, status, body, http.StatusCreated, "")
		var acc struct {
			ID           string `json:"id"`
			Verification struct {
				State string `json:"state"`
			} `json:"verification"`
			Alignment struct {
				State string `json:"state"`
			} `json:"from_alignment"`
		}
		if err := json.Unmarshal(body, &acc); err != nil || acc.ID == "" {
			t.Fatalf("the account created for %s is not the documented shape (%v): %s", tn.Name, err, r.env.snip(body))
		}
		// an unverified or unaligned account never sends and never errors: it would turn every wait below into a timeout with no explanation, so it is refused here, naming what module 3 said
		if acc.Verification.State != "verified" || acc.Alignment.State != "aligned" {
			t.Fatalf("the account of %s (smtp_host %s): module 3 says verification.state=%q (want verified) and from_alignment.state=%q (want aligned): the worker could not verify the controllable sink (its TLS or AUTH), or the From gate is not off\n  body: %s",
				tn.Name, r.sinkIP, acc.Verification.State, acc.Alignment.State, r.env.snip(body))
		}
		tn.AccountID = acc.ID
	}
	t.Logf("lanes: concurrency_per_user=%d and a send delay of %d-%d s for the four users; their accounts name the sink's private address %s:%d and are verified and aligned", m3hLanes, m3hDelayMin, m3hDelayMax, r.sinkIP, m3hSMTPPort)
}

// ---- 4 (A): several lanes ----

func (r *m3hRun) severalLanes(t *testing.T) {
	a := r.a
	// every send is held 3 s at the sink: with three lanes, three sends are in flight at once, and a worker takes one send at a time, so three DIFFERENT workers must be busy at that moment
	r.setMode(t, m3hMode{DataDelayMs: m3hAHoldMs})
	took := r.runAll(t, a)
	t.Logf("A: %d items accepted (202) in %s; the sink holds every final reply for %d ms", len(a.ItemIDs), took.Round(time.Millisecond), m3hAHoldMs)

	var last m3hReport
	m3lWait(t, r.ctx, "A's campaign completes (progress.is_complete)", 240*time.Second, 500*time.Millisecond, func() (bool, string) {
		v := r.sinkView(t) // fails at once on a duplicate
		last = r.report(t, a)
		r.mustNotFail(t, a, last)
		if last.Totals.Bounced > 0 {
			t.Fatalf("A's campaign shows %d bounced item(s) although the sink rejects nobody: %s", last.Totals.Bounced, last.Totals)
		}
		return last.IsComplete, fmt.Sprintf("%s; sink %d of %d", last.Totals, v.of(a), len(a.Recipients))
	})
	r.settle(t, 3)
	v := r.sinkView(t)
	rep := r.report(t, a)

	// every recipient exactly once, all sent, the same items
	var wrong []string
	for _, rc := range a.Recipients {
		if v.Counts[rc] != 1 {
			wrong = append(wrong, fmt.Sprintf("%s x%d", rc, v.Counts[rc]))
		}
	}
	if len(wrong) > 0 || v.of(a) != m3hNA {
		t.Fatalf("A: the sink does not hold every recipient exactly once (%d messages): %s", v.of(a), strings.Join(wrong, ", "))
	}
	if rep.Totals.Total != m3hNA || rep.Totals.Sent != m3hNA || rep.Totals.Bounced != 0 || rep.Totals.Failed != 0 || rep.Totals.InFlight != 0 || rep.Totals.Queued() != 0 || !rep.IsComplete {
		t.Fatalf("A's campaign at the end: %s (complete=%v), want total=sent=%d and nothing else", rep.Totals, rep.IsComplete, m3hNA)
	}
	var sentIDs []string
	for _, it := range rep.Items {
		if it.State == "sent" {
			sentIDs = append(sentIDs, it.JobItemID)
		}
	}
	if !m3lSameSet(sentIDs, a.ItemIDs) {
		t.Fatalf("A: the job item ids module 3 reports as sent are not the ids inserted:\n  reported %v\n  inserted %v", m3lSorted(sentIDs), m3lSorted(a.ItemIDs))
	}
	if st := r.jobStates(t, a); len(st) != 1 || st["sent"] != m3hNA {
		t.Fatalf("A: send_byo.jobs by state is %v, want exactly sent=%d", st, m3hNA)
	}
	if n := r.count(t, "count A's send_logs", "SELECT count(*) FROM send_byo.send_logs WHERE user_id = $1::uuid", a.UserID); n != m3hNA {
		t.Fatalf("A: send_byo.send_logs holds %d rows, want %d", n, m3hNA)
	}
	r.checkMessageIDs(t, a, v)

	// the hostile mode bit on every send, and the lanes overlapped: three held at once, never four
	inf := r.inflight(t)
	if inf.HeldTotal != m3hNA {
		t.Fatalf("A: the sink held %d send(s) in all, want all %d: the delay did not apply to every message", inf.HeldTotal, m3hNA)
	}
	for _, m := range v.Msgs {
		if m.HeldMs < m3hAHoldMs {
			t.Fatalf("A: the message to %v was held %d ms at the sink, want at least %d", m.To, m.HeldMs, m3hAHoldMs)
		}
	}
	if inf.Peak != m3hLanes {
		t.Fatalf("A: the sink held at most %d sends at once, want exactly %d: fewer means the three lanes never overlapped, more means the per-user ceiling of %d was exceeded", inf.Peak, m3hLanes, m3hLanes)
	}
	rc := r.rcptCounts(t)
	for _, addr := range a.Recipients {
		if c := rc[addr]; c.Accepted != 1 || c.Rejected != 0 || c.Deferred != 0 {
			t.Fatalf("A: the sink's RCPT counters for %s are %+v, want exactly one acceptance (a send must reach RCPT once)", addr, c)
		}
	}

	// all three workers took part (send_byo.job_events.box_id: the worker each job was handed to): the sink saw three sends at once and a worker takes one at a time
	by := r.workersOf(t, a)
	var parts []string
	total := 0
	for _, id := range r.workerIDs {
		if by[id] < 1 {
			t.Fatalf("A: worker %s handled none of A's jobs (handled per worker: %v): the three lanes did not run on three workers", id, by)
		}
		parts = append(parts, fmt.Sprintf("%s: %d", id[:8], by[id]))
		total += by[id]
	}
	if len(by) != len(r.workerIDs) || total != m3hNA {
		t.Fatalf("A: jobs handled per worker are %v (total %d), want exactly the three workers %v and %d jobs", by, total, r.workerIDs, m3hNA)
	}
	assigned := r.count(t, "count A's jobs assigned to a worker of this stack", "SELECT count(*) FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid AND assigned_box_id = ANY($3::text[]::uuid[])", a.UserID, a.CampaignID, r.workerIDs)
	if assigned != m3hNA {
		t.Fatalf("A: only %d of %d jobs name one of the three workers as assigned_box_id", assigned, m3hNA)
	}
	t.Logf("A (several lanes): %d recipients each exactly once, all sent, the same job items; the sink held every send %d ms and at most %d at once (never more); jobs per worker {%s}; the Message-ID of each message is its job's", m3hNA, m3hAHoldMs, inf.Peak, strings.Join(parts, ", "))
}

// ---- 5 (B): a rejecting sink ----

// m3hAddrs is the recipients at the given item positions.
func m3hAddrs(tn *m3lTenant, idx []int) []string {
	var out []string
	for _, i := range idx {
		out = append(out, tn.Recipients[i])
	}
	return out
}

func m3hHas(idx []int, i int) bool {
	for _, x := range idx {
		if x == i {
			return true
		}
	}
	return false
}

func (r *m3hRun) rejectingSink(t *testing.T) {
	b := r.b
	var plain []int
	for i := range b.Recipients {
		if !m3hHas(m3hRejectIdx, i) && !m3hHas(m3hDeferIdx, i) {
			plain = append(plain, i)
		}
	}
	nPlain, nRej, nDef := len(plain), len(m3hRejectIdx), len(m3hDeferIdx)
	if nPlain+nRej+nDef != m3hNB || nRej != 6 || nDef != 3 {
		t.Fatalf("the test's own partition of B is wrong: %d plain + %d rejected + %d deferred of %d", nPlain, nRej, nDef, m3hNB)
	}
	rejected, deferred := m3hAddrs(b, m3hRejectIdx), m3hAddrs(b, m3hDeferIdx)
	r.setMode(t, m3hMode{RejectRcpt: rejected, DeferRcpt: deferred})
	took := r.runAll(t, b)
	t.Logf("B: %d items accepted (202) in %s; the sink answers 550 to %d recipients and 451 to %d", len(b.ItemIDs), took.Round(time.Millisecond), nRej, nDef)

	// the first round: every item the sink does not defer reaches its terminal state, and each deferred one has failed once (module 3 recorded the 451)
	var rep m3hReport
	m3lWait(t, r.ctx, fmt.Sprintf("B: %d items sent, %d bounced and each of the %d deferred items has failed once", nPlain, nRej, nDef), 180*time.Second, 500*time.Millisecond, func() (bool, string) {
		r.sinkView(t)
		rep = r.report(t, b)
		r.mustNotFail(t, b, rep)
		ok := rep.Totals.Sent == nPlain && rep.Totals.Bounced == nRej
		for _, i := range m3hDeferIdx {
			if rep.ByID[b.ItemIDs[i]].AttemptCount < 1 {
				ok = false
			}
		}
		return ok, rep.Totals.String()
	})
	v := r.sinkView(t)
	rc := r.rcptCounts(t)
	for _, i := range plain {
		it := rep.ByID[b.ItemIDs[i]]
		if it.State != "sent" || v.Counts[b.Recipients[i]] != 1 || it.ProviderMessageID == nil {
			t.Fatalf("B: item %d (%s) should be sent exactly once: report state %q provider_message_id %v, sink count %d", i, b.Recipients[i], it.State, it.ProviderMessageID, v.Counts[b.Recipients[i]])
		}
		if c := rc[b.Recipients[i]]; c.Accepted != 1 || c.Rejected != 0 || c.Deferred != 0 {
			t.Fatalf("B: the sink's RCPT counters for the plain recipient %s are %+v, want exactly one acceptance", b.Recipients[i], c)
		}
	}
	for _, i := range m3hRejectIdx {
		it := rep.ByID[b.ItemIDs[i]]
		if it.State != "bounced" || it.class() != "bounce_permanent" || !strings.Contains(it.response(), "550") {
			t.Fatalf("B: item %d (%s) was answered 550 at RCPT: module 3 reports state %q error_class %q smtp_response %q, want bounced / bounce_permanent / a response naming 550", i, b.Recipients[i], it.State, it.class(), it.response())
		}
		if v.Counts[b.Recipients[i]] != 0 {
			t.Fatalf("B: the rejected recipient %s is in the sink %d time(s): a rejected recipient never reaches it", b.Recipients[i], v.Counts[b.Recipients[i]])
		}
		if c := rc[b.Recipients[i]]; c.Rejected != 1 || c.Accepted != 0 || c.Deferred != 0 {
			t.Fatalf("B: the sink's RCPT counters for the rejected recipient %s are %+v, want exactly one rejection: a bounce is final and must never be retried", b.Recipients[i], c)
		}
	}
	for _, i := range m3hDeferIdx {
		it := rep.ByID[b.ItemIDs[i]]
		if it.State == "sent" || it.State == "bounced" || it.State == "failed" || v.Counts[b.Recipients[i]] != 0 {
			t.Fatalf("B: item %d (%s) was answered 451 and must stay queued: report state %q, sink count %d", i, b.Recipients[i], it.State, v.Counts[b.Recipients[i]])
		}
		if it.class() != "transport_4xx" || !strings.Contains(it.response(), "451") {
			t.Fatalf("B: item %d (%s) was answered 451 4.7.1: module 3 reports error_class %q smtp_response %q, want transport_4xx and a response naming 451", i, b.Recipients[i], it.class(), it.response())
		}
		if c := rc[b.Recipients[i]]; c.Deferred < 1 || c.Accepted != 0 {
			t.Fatalf("B: the sink's RCPT counters for the deferred recipient %s are %+v, want at least one deferral and no acceptance yet", b.Recipients[i], c)
		}
	}
	t.Logf("B round 1: %d sent (each once in the sink), %d bounced (bounce_permanent, 550, never in the sink, one RCPT each), %d deferred (queued, transport_4xx, 451)", nPlain, nRej, nDef)

	// the retry: module 3's first backoff is 30 s, the second 120 s; the deferral is lifted only after every deferred item has been tried a SECOND time (attempt_count >= 2) and failed again,
	// so the retry is proved before the sink relents, and the third attempt (120 s later) is the one that finds the sink willing
	m3lWait(t, r.ctx, fmt.Sprintf("B: each of the %d deferred items is retried (attempt_count >= 2 in module 3's report)", nDef), 240*time.Second, time.Second, func() (bool, string) {
		r.sinkView(t)
		rep = r.report(t, b)
		r.mustNotFail(t, b, rep)
		if rep.Totals.Sent != nPlain || rep.Totals.Bounced != nRej {
			t.Fatalf("B: while the deferral is on, %s, want sent=%d and bounced=%d (nothing else may finish)", rep.Totals, nPlain, nRej)
		}
		ok := true
		var seen []string
		for _, i := range m3hDeferIdx {
			it := rep.ByID[b.ItemIDs[i]]
			seen = append(seen, fmt.Sprintf("%s=%d(%s)", b.Recipients[i][len(b.Prefix):len(b.Prefix)+2], it.AttemptCount, it.State))
			if it.AttemptCount < 2 {
				ok = false
			}
		}
		return ok, rep.Totals.String() + "; attempts " + strings.Join(seen, " ")
	})
	rc = r.rcptCounts(t)
	for _, i := range m3hDeferIdx {
		it := rep.ByID[b.ItemIDs[i]]
		if c := rc[b.Recipients[i]]; c.Deferred < 2 || c.Accepted != 0 {
			t.Fatalf("B: item %d (%s) has attempt_count %d in module 3's report, but the sink's RCPT counters are %+v: want at least two deferrals and no acceptance", i, b.Recipients[i], it.AttemptCount, c)
		}
		if it.class() != "transport_4xx" || it.State == "sent" || it.State == "bounced" || it.State == "failed" {
			t.Fatalf("B: after the retry item %d is in state %q with error_class %q, want a queued item still failing as transport_4xx", i, it.State, it.class())
		}
	}
	t.Logf("B retry: every deferred item was tried again (attempts %d, %d, %d) and answered 451 again; the sink relents now", rep.ByID[b.ItemIDs[m3hDeferIdx[0]]].AttemptCount, rep.ByID[b.ItemIDs[m3hDeferIdx[1]]].AttemptCount, rep.ByID[b.ItemIDs[m3hDeferIdx[2]]].AttemptCount)

	// the sink relents (the 550s stay)
	r.setMode(t, m3hMode{RejectRcpt: rejected})
	m3lWait(t, r.ctx, "B's campaign completes (progress.is_complete)", 300*time.Second, 2*time.Second, func() (bool, string) {
		r.sinkView(t)
		rep = r.report(t, b)
		r.mustNotFail(t, b, rep)
		return rep.IsComplete, rep.Totals.String()
	})
	r.settle(t, 3)
	v = r.sinkView(t)
	rep = r.report(t, b)
	rc = r.rcptCounts(t)

	// the partition: sent, bounced, other; and the sink's recipients are exactly the sent ones
	var sentIDs, bouncedIDs, otherIDs []string
	sentRcpt := map[string]bool{}
	for i, id := range b.ItemIDs {
		switch it := rep.ByID[id]; it.State {
		case "sent":
			sentIDs = append(sentIDs, id)
			sentRcpt[b.Recipients[i]] = true
		case "bounced":
			bouncedIDs = append(bouncedIDs, id)
		default:
			otherIDs = append(otherIDs, id)
		}
	}
	wantSent := nPlain + nDef
	if len(otherIDs) != 0 || len(rep.Items) != m3hNB || len(sentIDs) != wantSent || len(bouncedIDs) != nRej {
		t.Fatalf("B at the end: %d sent, %d bounced, %d other of %d reported items (%s), want %d sent, %d bounced and none other", len(sentIDs), len(bouncedIDs), len(otherIDs), len(rep.Items), rep.Totals, wantSent, nRej)
	}
	var sinkSet, reportedSet []string
	for _, rcp := range b.Recipients {
		if v.Counts[rcp] > 0 {
			sinkSet = append(sinkSet, rcp)
		}
		if sentRcpt[rcp] {
			reportedSet = append(reportedSet, rcp)
		}
		if v.Counts[rcp] > 1 {
			t.Fatalf("B: %s is in the sink %d times", rcp, v.Counts[rcp])
		}
	}
	if !m3lSameSet(sinkSet, reportedSet) || len(sinkSet) != wantSent || v.of(b) != wantSent {
		t.Fatalf("B: the sink's recipients are not exactly the items reported sent:\n  in the sink %v\n  reported sent %v", m3lSorted(sinkSet), m3lSorted(reportedSet))
	}
	for _, i := range m3hRejectIdx {
		if v.Counts[b.Recipients[i]] != 0 {
			t.Fatalf("B: the bounced recipient %s is in the sink", b.Recipients[i])
		}
		if it := rep.ByID[b.ItemIDs[i]]; it.State != "bounced" || it.class() != "bounce_permanent" {
			t.Fatalf("B: item %d changed after the sink relented: state %q class %q, want bounced / bounce_permanent", i, it.State, it.class())
		}
		if c := rc[b.Recipients[i]]; c.Rejected != 1 || c.Accepted != 0 {
			t.Fatalf("B: the sink's final RCPT counters for the bounced recipient %s are %+v, want one rejection and nothing else", b.Recipients[i], c)
		}
	}
	for _, i := range m3hDeferIdx {
		it := rep.ByID[b.ItemIDs[i]]
		c := rc[b.Recipients[i]]
		if it.State != "sent" || it.AttemptCount < 2 || it.ProviderMessageID == nil || c.Accepted != 1 || c.Deferred < 2 || v.Counts[b.Recipients[i]] != 1 {
			t.Fatalf("B: the deferred item %d (%s) ended state %q attempt_count %d; sink RCPT counters %+v, sink count %d: want sent after at least two deferrals, accepted once, in the sink once", i, b.Recipients[i], it.State, it.AttemptCount, c, v.Counts[b.Recipients[i]])
		}
	}
	if st := r.jobStates(t, b); len(st) != 2 || st["sent"] != wantSent || st["bounced"] != nRej {
		t.Fatalf("B: send_byo.jobs by state is %v, want exactly sent=%d and bounced=%d", st, wantSent, nRej)
	}
	if n := r.count(t, "count B's send_logs for sent and bounced jobs", "SELECT count(DISTINCT job_id) FROM send_byo.send_logs WHERE user_id = $1::uuid", b.UserID); n != m3hNB {
		t.Fatalf("B: send_byo.send_logs holds attempts of %d distinct jobs, want all %d", n, m3hNB)
	}
	r.checkMessageIDs(t, b, v)
	t.Logf("B (a rejecting sink): %d items = %d sent + %d bounced + 0 other; the sink holds exactly the %d sent recipients once each; the %d rejected never reached it and were tried once; the %d deferred were retried (%d, %d, %d deferrals) and then sent",
		m3hNB, len(sentIDs), len(bouncedIDs), wantSent, nRej, nDef, rc[b.Recipients[m3hDeferIdx[0]]].Deferred, rc[b.Recipients[m3hDeferIdx[1]]].Deferred, rc[b.Recipients[m3hDeferIdx[2]]].Deferred)
}

// ---- 6 (C and D): a delaying sink and the halt ----

func (r *m3hRun) delayingSinkAndHalt(t *testing.T) {
	c, d := r.c, r.d

	// every send is held 4 s at the sink; all of C's items are accepted BEFORE the suspension (a suspended user's /v1/run is refused with 409)
	r.setMode(t, m3hMode{DataDelayMs: m3hCHoldMs})
	took := r.runAll(t, c)
	t.Logf("C: %d items accepted (202) in %s; the sink holds every final reply for %d ms", len(c.ItemIDs), took.Round(time.Millisecond), m3hCHoldMs)

	// the moment: two of C's sends are held at the sink with at least 2 s of the hold left, so they are in flight at module 3 when the suspension lands and cannot have finished before the call returns
	var held []string
	m3lWait(t, r.ctx, "two of C's sends are held at the sink with at least 2 s of the hold left (GET /api/inflight)", 90*time.Second, 100*time.Millisecond, func() (bool, string) {
		v := r.inflight(t)
		var mine []string
		long := 0
		for _, h := range v.Held {
			for _, to := range h.To {
				if to = strings.ToLower(to); strings.HasPrefix(to, c.Prefix) {
					mine = append(mine, to)
					if h.RemainingMs >= 2000 {
						long++
					}
				}
			}
		}
		if long >= 2 {
			held = mine
			return true, ""
		}
		return false, fmt.Sprintf("%d held in all, %d of C's, %d with 2 s or more left", v.Inflight, len(mine), long)
	})

	// THE HALT, through module 3's admin route (Domain B: the admin key)
	status, body := r.env.do(t, r.env.m3URL, http.MethodPost, "/v1/admin/users/"+c.UserID+"/suspend", r.env.adminKey, map[string]string{"reason": "m3 hostile harness: a halt under a delaying sink"})
	r.env.expect(t, "POST /v1/admin/users/{C}/suspend", status, body, http.StatusOK, "")
	retAt := r.dbNow(t) // the database clock right after the call returned: every commit of the suspension is before it
	var sr struct {
		UserID    string `json:"user_id"`
		Suspended bool   `json:"suspended"`
	}
	if err := json.Unmarshal(body, &sr); err != nil || !sr.Suspended || !strings.EqualFold(sr.UserID, c.UserID) {
		t.Fatalf("module 3 answered the suspend of C with %s, want {user_id: C, suspended: true}", r.env.snip(body))
	}
	susp := r.suspension(t, c.UserID)
	if !susp.Exists || !susp.Suspended || susp.SuspendedAt == nil {
		t.Fatalf("module 3 answered 200 to the suspend but its own table says %+v", susp)
	}
	ts := *susp.SuspendedAt
	if ts.After(retAt) {
		t.Fatalf("module 3 recorded the suspension at %s, after the call returned at %s on the same database clock", m3lT(ts), m3lT(retAt))
	}

	// the held sends FINISH (an SMTP session underway cannot be recalled): wait, bounded, until nothing of C is in flight, THEN count
	m3lWait(t, r.ctx, "C's campaign shows totals.in_flight == 0 after the suspension", 90*time.Second, 250*time.Millisecond, func() (bool, string) {
		rep := r.report(t, c)
		r.mustNotFail(t, c, rep)
		return rep.Totals.InFlight == 0, rep.Totals.String()
	})
	v := r.sinkView(t)
	s1 := v.of(c)
	rep := r.report(t, c)
	logs := r.count(t, "count C's send_logs rows", "SELECT count(*) FROM send_byo.send_logs WHERE user_id = $1::uuid", c.UserID)
	if s1 != rep.Totals.Sent || s1 != logs {
		t.Fatalf("C: the sink holds S1 = %d messages, module 3's report says totals.sent = %d and send_byo.send_logs holds %d rows: the three must agree once nothing is in flight", s1, rep.Totals.Sent, logs)
	}
	if left := m3hNC - s1; left < 3 {
		t.Fatalf("C: only %d of %d items are left after the halt (S1 = %d): too few remain for the halt to prove anything; the sends were too fast or the suspension too late", left, m3hNC, s1)
	}

	// the sharp assertions, on module 3's own event log and the database clock. suspended_at is the START of the suspend transaction, so a claim whose own transaction started in the few milliseconds
	// before that commit is legitimate; the call returning (retAt) is after the commit, and nothing may be claimed after it. Both are counted; the second is the assertion.
	claimsSQL := "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND from_state = 'queued' AND to_state = 'assigned' AND created_at %s $2::timestamptz"
	claimsAfterRet := r.count(t, "C's claims after the call returned", fmt.Sprintf(claimsSQL, ">"), c.UserID, retAt)
	claimsAfterTs := r.count(t, "C's claims after suspended_at", fmt.Sprintf(claimsSQL, ">"), c.UserID, ts)
	claimsUpToRet := r.count(t, "C's claims up to the call's return", fmt.Sprintf(claimsSQL, "<="), c.UserID, retAt)
	sentSQL := "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND to_state = 'sent' AND created_at > $2::timestamptz"
	sentAfterTs := r.count(t, "C's completions after suspended_at", sentSQL, c.UserID, ts)
	sentAfterRet := r.count(t, "C's completions after the call returned", sentSQL, c.UserID, retAt)
	if claimsUpToRet < s1 || claimsUpToRet == 0 {
		t.Fatalf("C: the control of the event query failed: %d queued->assigned events up to the suspension but %d of C's jobs were sent: the query does not see claims, so a zero after it would prove nothing", claimsUpToRet, s1)
	}
	if claimsAfterRet != 0 {
		t.Fatalf("C: module 3 claimed (queued -> assigned) %d job(s) of C AFTER the suspend call had returned at %s (suspended_at %s; %d after suspended_at): the dispatcher did not honour the suspension", claimsAfterRet, m3lT(retAt), m3lT(ts), claimsAfterTs)
	}
	if sentAfterTs < 1 {
		t.Fatalf("C: no send completed after the suspension at %s although %d were held at the sink with at least 2 s left when the call went out: the halt was not made while sends were in flight, so it proves nothing about them (the call took longer than the hold?)", m3lT(ts), len(held))
	}
	if sentAfterRet > m3hLanes {
		t.Fatalf("C: %d of C's jobs completed after the call returned at %s, but with concurrency_per_user=%d at most %d (the ones already claimed) can", sentAfterRet, m3lT(retAt), m3hLanes, m3hLanes)
	}
	// every send that was held at the sink when the call went out finished, exactly once
	for _, a := range held {
		idx := -1
		for i, rcp := range c.Recipients {
			if rcp == a {
				idx = i
			}
		}
		if idx < 0 {
			t.Fatalf("C: the sink held a send to %s, which is none of C's recipients", a)
		}
		if it := rep.ByID[c.ItemIDs[idx]]; it.State != "sent" || v.Counts[a] != 1 {
			t.Fatalf("C: the send to %s was held at the sink when the suspension landed and must finish: report state %q, sink count %d, want sent exactly once", a, it.State, v.Counts[a])
		}
	}

	// the rest stays queued: not failed, not lost (module 3's report and, independently, its jobs table), the same job ids
	queued := rep.Totals.Queued()
	if rep.Totals.Total != m3hNC || rep.Totals.Sent+queued != m3hNC || rep.Totals.Failed != 0 || rep.Totals.Bounced != 0 || rep.Totals.InFlight != 0 {
		t.Fatalf("C's campaign after the halt: %s, want total=%d, sent + pending + waiting_for_capacity = %d, failed=0, bounced=0, in_flight=0", rep.Totals, m3hNC, m3hNC)
	}
	if st := r.jobStates(t, c); len(st) != 2 || st["sent"] != s1 || st["queued"] != queued {
		t.Fatalf("C: send_byo.jobs by state is %v, want exactly sent=%d and queued=%d (nothing failed, nothing lost, nothing in flight)", st, s1, queued)
	}
	atHalt := r.jobIDsOf(t, c)
	var haltJobs []string
	for _, id := range atHalt {
		haltJobs = append(haltJobs, id)
	}
	if len(atHalt) != m3hNC || !m3lSameSet(haltJobs, r.jobIDs["c"]) {
		t.Fatalf("C: module 3 holds %d jobs after the halt, and their ids are not the ones POST /v1/run answered (%d ids): a job was lost or replaced", len(atHalt), len(r.jobIDs["c"]))
	}
	t.Logf("C halt: suspended_at %s, call returned %s; %d send(s) were held at the sink when it went out and all %d finished (S1 = %d: %d completed after suspended_at, %d after the call returned, at most %d allowed); claims after the call 0 (up to it %d, %d after suspended_at); %d item(s) still queued, none failed or lost",
		m3lT(ts), m3lT(retAt), len(held), len(held), s1, sentAfterTs, sentAfterRet, m3hLanes, claimsUpToRet, claimsAfterTs, queued)

	// THE HOLD: for at least 8 seconds (database clock) C's sink count does not change and its queued items do not drop, while D (the control, never suspended, accepted only now) keeps being sent to
	dTook := r.runAll(t, d)
	dStart := v.of(d)
	holdStart := r.dbNow(t)
	began := time.Now()
	samples := 0
	dNow := dStart
	for {
		view := r.sinkView(t)
		if got := view.of(c); got != s1 {
			t.Fatalf("C's sink count moved from %d to %d during the hold, %s after it began (database time %s): a suspended user's items were sent", s1, got, time.Since(began).Round(time.Millisecond), m3lT(r.dbNow(t)))
		}
		cur := r.report(t, c)
		if cur.Totals.Sent != s1 || cur.Totals.Queued() != queued || cur.Totals.InFlight != 0 || cur.Totals.Failed != 0 || cur.Totals.Bounced != 0 || cur.Totals.Total != m3hNC {
			t.Fatalf("during the hold C's campaign changed: %s, want sent=%d, pending + waiting_for_capacity=%d, nothing else", cur.Totals, s1, queued)
		}
		if s := r.suspension(t, c.UserID); !s.Suspended {
			t.Fatalf("module 3 no longer records C as suspended during the hold: %+v", s)
		}
		dNow = view.of(d)
		samples++
		if samples >= 8 && time.Since(began) >= m3hHoldFor && dNow >= 3 {
			break
		}
		if time.Since(began) > 120*time.Second {
			t.Fatalf("the control tenant D has only %d message(s) in the sink after %s of C's hold: the stack (dispatcher, workers or sink) is not sending although D is not suspended, so C's silence proves nothing", dNow, time.Since(began).Round(time.Second))
		}
		select {
		case <-r.ctx.Done():
			t.Fatal("the test's overall time budget ended during the hold")
		case <-time.After(time.Second):
		}
	}
	holdEnd := r.dbNow(t)
	if holdEnd.Sub(holdStart) < m3hHoldFor {
		t.Fatalf("the hold lasted %s on the database clock, want at least %s", holdEnd.Sub(holdStart), m3hHoldFor)
	}
	if dNow <= dStart {
		t.Fatalf("the control tenant D's sink count did not move during C's hold (%d -> %d)", dStart, dNow)
	}
	t.Logf("C hold: %s on the database clock (%d samples), C silent at %d while D (accepted %s after the halt) went from %d to %d messages", holdEnd.Sub(holdStart).Round(time.Millisecond), samples, s1, dTook.Round(time.Millisecond), dStart, dNow)

	// THE RESUME: unsuspend and clear the delay; everything completes, each recipient exactly once, the same jobs
	status, body = r.env.do(t, r.env.m3URL, http.MethodPost, "/v1/admin/users/"+c.UserID+"/unsuspend", r.env.adminKey, map[string]string{})
	r.env.expect(t, "POST /v1/admin/users/{C}/unsuspend", status, body, http.StatusOK, "")
	if err := json.Unmarshal(body, &sr); err != nil || sr.Suspended {
		t.Fatalf("module 3 answered the unsuspend of C with %s, want {suspended: false}", r.env.snip(body))
	}
	r.setMode(t, m3hMode{})
	susp2 := r.suspension(t, c.UserID)
	if susp2.Suspended || susp2.ReinstatedAt == nil || !susp2.ReinstatedAt.After(ts) {
		t.Fatalf("after the unsuspend module 3's own table says %+v (suspended_at %s), want reinstated after the suspension", susp2, m3lT(ts))
	}
	m3lWait(t, r.ctx, "C's campaign completes after the reinstatement (resume has no dispatcher kick: up to one tick)", 300*time.Second, time.Second, func() (bool, string) {
		view := r.sinkView(t)
		rep = r.report(t, c)
		r.mustNotFail(t, c, rep)
		return rep.IsComplete, fmt.Sprintf("%s; sink %d of %d", rep.Totals, view.of(c), len(c.Recipients))
	})
	m3lWait(t, r.ctx, "D's campaign completes", 240*time.Second, time.Second, func() (bool, string) {
		r.sinkView(t)
		rep = r.report(t, d)
		r.mustNotFail(t, d, rep)
		return rep.IsComplete, rep.Totals.String()
	})
	r.settle(t, 3)
	v = r.sinkView(t)
	for _, tn := range []*m3lTenant{c, d} {
		rep := r.report(t, tn)
		var wrong []string
		for _, rcp := range tn.Recipients {
			if v.Counts[rcp] != 1 {
				wrong = append(wrong, fmt.Sprintf("%s x%d", rcp, v.Counts[rcp]))
			}
		}
		if len(wrong) > 0 || v.of(tn) != len(tn.Recipients) {
			t.Fatalf("%s: the sink does not hold every recipient exactly once (%d messages): %s", strings.ToUpper(tn.Name), v.of(tn), strings.Join(wrong, ", "))
		}
		if rep.Totals.Total != len(tn.ItemIDs) || rep.Totals.Sent != len(tn.ItemIDs) || rep.Totals.Failed != 0 || rep.Totals.Bounced != 0 || rep.Totals.InFlight != 0 || rep.Totals.Queued() != 0 || !rep.IsComplete {
			t.Fatalf("%s's campaign at the end: %s (complete=%v), want total=sent=%d and nothing else", strings.ToUpper(tn.Name), rep.Totals, rep.IsComplete, len(tn.ItemIDs))
		}
		var sentIDs []string
		for _, it := range rep.Items {
			if it.State == "sent" {
				sentIDs = append(sentIDs, it.JobItemID)
			}
		}
		if !m3lSameSet(sentIDs, tn.ItemIDs) {
			t.Fatalf("%s: the job item ids module 3 reports as sent are not the ids inserted:\n  reported %v\n  inserted %v", strings.ToUpper(tn.Name), m3lSorted(sentIDs), m3lSorted(tn.ItemIDs))
		}
		if st := r.jobStates(t, tn); len(st) != 1 || st["sent"] != len(tn.ItemIDs) {
			t.Fatalf("%s: send_byo.jobs by state is %v, want exactly sent=%d", strings.ToUpper(tn.Name), st, len(tn.ItemIDs))
		}
		if n := r.count(t, "count send_logs", "SELECT count(*) FROM send_byo.send_logs WHERE user_id = $1::uuid", tn.UserID); n != len(tn.ItemIDs) {
			t.Fatalf("%s: send_byo.send_logs holds %d rows, want %d", strings.ToUpper(tn.Name), n, len(tn.ItemIDs))
		}
		r.checkMessageIDs(t, tn, v)
	}
	// the same job ids as at the halt: nothing was recreated
	afterwards := r.jobIDsOf(t, c)
	if len(afterwards) != len(atHalt) {
		t.Fatalf("C: module 3 holds %d jobs at the end, %d at the halt", len(afterwards), len(atHalt))
	}
	for item, job := range atHalt {
		if afterwards[item] != job {
			t.Fatalf("C: the job of item %s was %s at the halt and is %s at the end: a job was replaced", item, job, afterwards[item])
		}
	}
	claimsAfterReinstate := r.count(t, "C's claims after the reinstatement", fmt.Sprintf(claimsSQL, ">"), c.UserID, *susp2.ReinstatedAt)
	if claimsAfterReinstate < m3hNC-s1 {
		t.Fatalf("C: only %d claims after the reinstatement, but %d items were still queued at the halt: they cannot all have been sent", claimsAfterReinstate, m3hNC-s1)
	}
	t.Logf("C resume: reinstated %s; C's %d items (%d before the halt, %d after) and D's %d are each in the sink exactly once, the job ids are the ones from the halt, the Message-ID of each message is its job's; %d claims of C after the reinstatement",
		m3lT(*susp2.ReinstatedAt), m3hNC, s1, m3hNC-s1, m3hND, claimsAfterReinstate)
}

// ---- 7: module 3 is not metered ----

func (r *m3hRun) chargeDecisions(t *testing.T) {
	// every item state module 3's reports showed over the whole test must be FREE by the charge table, by its row M3-99 (M3-01 for a cancelled one; module 3 is not metered: entitlements, not credits)
	for _, want := range []string{"sent", "bounced", "queued"} {
		if r.seen[want] == 0 {
			t.Fatalf("module 3's reports never showed an item in state %q during this test (seen: %v): the charge check below would not cover it", want, r.seen)
		}
	}
	var states []string
	for st := range r.seen {
		states = append(states, st)
	}
	sort.Strings(states)
	// the states module 3 can show that this test did not necessarily see are decided by the same row: check them too
	for _, extra := range []string{"failed", "assigned", "sending"} {
		if r.seen[extra] == 0 {
			states = append(states, extra)
		}
	}
	for _, st := range states {
		got := charge.Decide(charge.Outcome{Module: "m3", State: st})
		wantRule := "M3-99" // the catch-all: module 3 is not metered; "cancelled" has a row of its own since the Phase 3 close-out (M3-01)
		if st == "cancelled" {
			wantRule = "M3-01"
		}
		if got.Kind != charge.Free || got.Rule != wantRule || got.Unit != "" {
			t.Fatalf("the charge table decides module 3's state %q as %+v, want Free by %s with no unit (module 3 is not metered)", st, got, wantRule)
		}
	}
	// the control: the same function does charge a metered module's delivered item, so "Free" above is a decision and not the only thing it can say
	if got := charge.Decide(charge.Outcome{Module: "m2", State: "sent"}); got.Kind != charge.Charge || got.Unit != "m2_accepted" {
		t.Fatalf("control: the charge table decides module 2's sent item as %+v, want a charge of m2_accepted: the check above could not tell a free outcome from a charged one", got)
	}

	// the database: module 3's flow gained no monetary row, no balance row and not any ledger row for these users
	ids := make([]string, 0, len(r.tenants))
	for _, tn := range r.tenants {
		ids = append(ids, tn.UserID)
	}
	ledgerMonetary := "SELECT count(*) FROM platform.ledger WHERE user_id = ANY($1::text[]::uuid[]) AND monetary"
	ledgerAll := "SELECT count(*) FROM platform.ledger WHERE user_id = ANY($1::text[]::uuid[])"
	balancesAll := "SELECT count(*) FROM platform.balances WHERE user_id = ANY($1::text[]::uuid[])"
	if n := r.count(t, "count the users' monetary ledger rows", ledgerMonetary, ids); n != 0 {
		t.Fatalf("the platform ledger holds %d monetary row(s) for users that only used module 3: module 3 is not metered", n)
	}
	if n := r.count(t, "count the users' balance rows", balancesAll, ids); n != 0 {
		t.Fatalf("the platform holds %d balance row(s) for users that only used module 3: module 3 is not metered", n)
	}
	if n := r.count(t, "count the users' ledger rows", ledgerAll, ids); n != 0 {
		t.Fatalf("the platform ledger holds %d row(s) for these users: nothing in this test writes the ledger (the entitlement path is deliberately not used), so something else did", n)
	}

	// the control of the three queries, rolled back: a ledger row of one of these users IS seen by the same queries, and a non-monetary one (module 3's own kind) is not counted as monetary
	tx, err := r.db.Begin(r.ctx)
	if err != nil {
		t.Fatalf("begin the control transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.ctx)) }()
	if _, err := tx.Exec(r.ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key, until_at)
		VALUES ($1::uuid, 'm3', 'm3_enable', $2::uuid, 'm3 hostile harness: control of the ledger query (rolled back)', $3, now() + interval '1 hour')`, r.a.UserID, r.admin, "m3hostile-control-"+r.runID); err != nil {
		t.Fatalf("control: could not insert a module 3 entitlement row for user A inside a transaction that is rolled back: %v", err)
	}
	var all, monetary int
	if err := tx.QueryRow(r.ctx, ledgerAll, ids).Scan(&all); err != nil {
		t.Fatalf("control: count the users' ledger rows: %v", err)
	}
	if err := tx.QueryRow(r.ctx, ledgerMonetary, ids).Scan(&monetary); err != nil {
		t.Fatalf("control: count the users' monetary ledger rows: %v", err)
	}
	if all != 1 || monetary != 0 {
		t.Fatalf("control: with one m3_enable row for user A the queries count %d ledger row(s) and %d monetary row(s), want 1 and 0: the instrument does not see the users' rows", all, monetary)
	}
	if err := tx.Rollback(r.ctx); err != nil {
		t.Fatalf("roll back the control transaction: %v", err)
	}
	if n := r.count(t, "count the users' ledger rows after the control", ledgerAll, ids); n != 0 {
		t.Fatalf("the rolled-back control left %d ledger row(s) for the users", n)
	}
	t.Logf("charge: module 3's item states seen in the reports %v and the unseen failed/assigned/sending are all Free by M3-01 (control: module 2's sent item is charged m2_accepted); the platform holds no ledger row, no monetary row and no balance for the four users (control: the same queries see a rolled-back m3_enable row and count it as non-monetary)", r.seen)
}
