//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestM3Live_ExpiredEntitlementHaltsSendingAndReenableResumes is the proof (Phase 2, Part B8) that an EXPIRED module-3 entitlement halts REAL sending and that re-enabling resumes it, against the REAL module 3 (its
// control and its worker) and a real SMTP sink, inside the harness stack.
//
// WHO DOES WHAT. The platform SERVICE running in the same stack owns the expiry loop and the reconcile loop (every two seconds, pointed at module 3's admin route), exactly as in production. This test therefore
// calls neither Store.ExpireDue nor Reconciler.RunOnce and builds no module 3 admin client: it writes what an ADMIN writes (entitlements.Store.Enable, as platform_app, through the ledger) and then OBSERVES what the
// service did, in the database (platform.ledger, platform.m3_entitlements, platform.m3_sync) and in module 3's own tables (send_byo.user_suspensions, job_events, send_logs, jobs), reading the database clock only.
//
// THE CONTROL (CLAUDE.md: every test has a paired control that fails when the property breaks). Two tenants send through the same module 3, the same worker and the same sink. U1 is the entitlement under test; U2
// is never suspended. U1's sending is first PROVEN to work (a paired control: the sink holds at least 5 of U1's messages and 2 of U2's while U1's entitlement, dated an hour ahead, is NOT expired by the service);
// then U1's date is moved a few seconds ahead; when it passes, the service expires it and delivers the suspension; U1's sink count then stops, while U2's keeps growing (so the halt is the suspension, not a stalled
// stack); and after the re-enable U1's SAME items complete, each message exactly once.
//
// ONE DEVIATION FROM THE RECIPE, ON PURPOSE: the entitlement's date is moved close to "now" only AFTER sending is proven (a re-date of an already enabled entitlement, a documented store operation that does not
// raise the wanted state), instead of being set to now+25s before the sending starts. With the service's own expiry loop running, a date set up front expires on ITS clock whatever the preparation takes (account
// verification, a bcrypt-checked request per item), which would race the setup; this way the window between "sending works" and "the date passes" is exactly m3lExpiryIn. And U2 has 20 items, not 8: at one send per
// 2-3 s, 8 would be finished before U1's halt even begins, and the control "U2 keeps sending while U1 is halted" would pass vacuously.
//
// Phases run in order and the first failure stops the test (a later phase assumes the earlier ones). Every wait is bounded and polls; nothing sleeps blindly.
func TestM3Live_ExpiredEntitlementHaltsSendingAndReenableResumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3lTestBudget)
	t.Cleanup(cancel)
	r := m3lNewRun(t, ctx)
	for _, p := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"1_preconditions", r.preconditions},
		{"2_seed", r.seed},
		{"3_pacing_and_accounts", r.pacingAndAccounts},
		{"4_entitlements_delivered_to_module_3", r.entitlementsDelivered},
		{"5_sending_works_while_entitled", r.sending},
		{"6_expiry_is_delivered_to_module_3", r.expiry},
		{"7_the_halt", r.halt},
		{"8_re-enable_resumes_the_same_items", r.resume},
	} {
		if !t.Run(p.name, p.fn) {
			t.FailNow() // fail fast: the next phase assumes this one
		}
	}
}

const (
	m3lN1          = 24 // U1's items: the entitlement under test
	m3lN2          = 20 // U2's items: the control, still sending while U1 is halted
	m3lDelaySec    = 2  // per-user pacing: min = max = 2 s between two sends of one user
	m3lConcurrency = 1  // per-user pacing: one lane
	m3lMinU1       = 5  // k: U1's messages in the sink before the expiry (the paired control)
	m3lMinU2       = 2  // U2's messages in the sink before the expiry
	m3lFarFuture   = time.Hour
	m3lExpiryIn    = 10 * time.Second // from the re-date to the date
	m3lHoldFor     = 8 * time.Second  // how long U1's silence is observed
	m3lTestBudget  = 14 * time.Minute
)

// m3lT and m3lTP print instants (UTC, microseconds) for failure messages and evidence lines.
func m3lT(x time.Time) string { return x.UTC().Format("15:04:05.000000") }

func m3lTP(x *time.Time) string {
	if x == nil {
		return "-"
	}
	return m3lT(*x)
}

// ---- 1: preconditions ----

func (r *m3lRun) preconditions(t *testing.T) {
	// The platform: an admin to write entitlements against (the first-admin stage created one).
	err := r.db.QueryRow(r.ctx, "SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at LIMIT 1").Scan(&r.admin)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("platform.admins has no active admin: the first-admin stage must have run before this test (every entitlement is written against a named admin)")
	}
	if err != nil {
		t.Fatalf("read platform.admins: %v", err)
	}

	// Module 3's schema: every table and column this test reads is inferred from module 3's migrations. A wrong guess is found here, in the first second.
	r.requireColumns(t, "send_byo", "boxes", "status", "last_heartbeat_at")
	r.requireColumns(t, "send_byo", "jobs", "user_id", "campaign_id", "job_item_id", "state")
	r.requireColumns(t, "send_byo", "job_events", "job_id", "user_id", "from_state", "to_state", "created_at")
	r.requireColumns(t, "send_byo", "send_logs", "job_id", "user_id", "recipient_email")
	r.requireColumns(t, "send_byo", "user_suspensions", "user_id", "suspended", "suspended_at", "reinstated_at", "reason")
	r.requireColumns(t, "send_byo", "user_config_overrides", "user_id", "concurrency_per_user", "send_delay_min_sec", "send_delay_max_sec")

	// Module 3's worker is registered and heartbeating (verify-on-add and every send need an idle connected worker).
	if n := r.count(t, "count the active workers", "SELECT count(*) FROM send_byo.boxes WHERE status = 'active' AND last_heartbeat_at > now() - interval '60 seconds'"); n < 1 {
		t.Fatal("module 3 has no active worker with a heartbeat in the last 60 seconds (send_byo.boxes): nothing can be verified or sent; the harness stack must start the worker, and the compose --wait does not notice a dead one")
	}

	// The sink answers and its listing decodes (a decode surprise fails here with the raw JSON, not after minutes of sending).
	r.sinkBaseline = len(r.env.sinkMessages(t))

	// Paired controls on the credentials, so that the successful calls below mean something: the two credentials are distinct and both are checked.
	nobodyU, nobodyC := r.uuid(t), r.uuid(t)
	status, body := r.env.do(t, r.env.m3URL, http.MethodGet, "/v1/admin/users/"+nobodyU+"/config", r.env.token, nil)
	r.env.expect(t, "control: the SERVICE token presented to the ADMIN route (module 3's admin surface must be mounted and must reject a credential that is not the admin key)", status, body, http.StatusUnauthorized, "unauthenticated")
	reportPath := fmt.Sprintf("/v1/report?campaign_id=%s&user_id=%s&page_size=1", nobodyC, nobodyU)
	status, body = r.env.do(t, r.env.m3URL, http.MethodGet, reportPath, r.env.adminKey, nil)
	r.env.expect(t, "control: the ADMIN key presented to the platform route", status, body, http.StatusUnauthorized, "unauthenticated")
	status, body = r.env.do(t, r.env.m3URL, http.MethodGet, reportPath, r.env.token, nil)
	r.env.expect(t, "the service token on the platform route (an unknown campaign reads as an empty report)", status, body, http.StatusOK, "")
	if rep := r.parseReport(t, "an unknown campaign", body); rep.Totals.Total != 0 {
		t.Fatalf("a campaign that does not exist reports %s: the report is not scoped by campaign", rep.Totals)
	}
	t.Logf("preconditions: admin %s, an active worker, the sink holds %d message(s) already (cumulative), both credentials work and are distinct", r.admin, r.sinkBaseline)
}

// ---- 2: seed ----

// seed creates, as the superuser and for real (committed), two tenants: auth.users and public.users rows, one campaign each, and N1 / N2 job items with DISTINCT recipients that carry this run's id in the local part
// (the sink is cumulative and shared: recipients are matched by exact address).
func (r *m3lRun) seed(t *testing.T) {
	var ids [4]string
	for i := range ids {
		ids[i] = r.uuid(t)
	}
	r.u1 = &m3lTenant{Name: "u1", UserID: ids[0], CampaignID: ids[1]}
	r.u2 = &m3lTenant{Name: "u2", UserID: ids[2], CampaignID: ids[3]}

	tx, err := r.db.Begin(r.ctx)
	if err != nil {
		t.Fatalf("begin the seeding transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.ctx)) }()
	for _, spec := range []struct {
		tn *m3lTenant
		n  int
	}{{r.u1, m3lN1}, {r.u2, m3lN2}} {
		tn := spec.tn
		tn.Prefix = fmt.Sprintf("%s-%s-", tn.Name, r.runID)
		email := fmt.Sprintf("m3live-%s-%s@example.test", tn.Name, r.runID)
		if _, err := tx.Exec(r.ctx, "INSERT INTO auth.users (id, email) VALUES ($1::uuid, $2)", tn.UserID, email); err != nil {
			t.Fatalf("seed auth.users for %s: %v", tn.Name, err)
		}
		if _, err := tx.Exec(r.ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)", tn.UserID, email); err != nil {
			t.Fatalf("seed public.users for %s: %v", tn.Name, err)
		}
		for i := 1; i <= spec.n; i++ {
			rcpt := fmt.Sprintf("%s%02d@example.test", tn.Prefix, i)
			var id string
			err := tx.QueryRow(r.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
				VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`,
				tn.CampaignID, tn.UserID, rcpt, fmt.Sprintf("[m3live %s] %s %02d", r.runID, tn.Name, i), fmt.Sprintf("<p>m3live %s %s %02d</p>", r.runID, tn.Name, i)).Scan(&id)
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
	for _, tn := range []*m3lTenant{r.u1, r.u2} {
		if n := r.count(t, "count the seeded items", "SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND user_id = $2::uuid", tn.CampaignID, tn.UserID); n != len(tn.ItemIDs) {
			t.Fatalf("seeded %d job items for %s but the table holds %d", len(tn.ItemIDs), tn.Name, n)
		}
	}
	t.Logf("seed: run %s; U1 %s (campaign %s, %d items), U2 %s (campaign %s, %d items)", r.runID, r.u1.UserID, r.u1.CampaignID, len(r.u1.ItemIDs), r.u2.UserID, r.u2.CampaignID, len(r.u2.ItemIDs))
}

// ---- 3: per-user pacing and one SMTP account each ----

func (r *m3lRun) pacingAndAccounts(t *testing.T) {
	for _, tn := range []*m3lTenant{r.u1, r.u2} {
		// One lane and exactly two seconds between sends: about one send per 2-3 s, so 24 items take about a minute and an expiry lands in the middle of the run.
		status, body := r.env.do(t, r.env.m3URL, http.MethodPut, "/v1/admin/users/"+tn.UserID+"/config", r.env.adminKey,
			map[string]int{"concurrency_per_user": m3lConcurrency, "send_delay_min_sec": m3lDelaySec, "send_delay_max_sec": m3lDelaySec})
		r.env.expect(t, "PUT the per-user pacing of "+tn.Name, status, body, http.StatusOK, "")
		var conc, lo, hi int
		err := r.db.QueryRow(r.ctx, `SELECT COALESCE(concurrency_per_user, -1), COALESCE(send_delay_min_sec, -1), COALESCE(send_delay_max_sec, -1)
			FROM send_byo.user_config_overrides WHERE user_id = $1::uuid`, tn.UserID).Scan(&conc, &lo, &hi)
		if err != nil {
			t.Fatalf("read back module 3's per-user pacing of %s: %v", tn.Name, err)
		}
		if conc != m3lConcurrency || lo != m3lDelaySec || hi != m3lDelaySec {
			t.Fatalf("module 3 stored concurrency_per_user=%d send_delay_min_sec=%d send_delay_max_sec=%d for %s, want %d, %d, %d", conc, lo, hi, tn.Name, m3lConcurrency, m3lDelaySec, m3lDelaySec)
		}
	}
	for _, tn := range []*m3lTenant{r.u1, r.u2} {
		from := fmt.Sprintf("sender-%s-%s@example.test", tn.Name, r.runID)
		status, body := r.env.do(t, r.env.m3URL, http.MethodPost, "/v1/send-byo/accounts", r.env.token, map[string]any{
			"user_id": tn.UserID, "smtp_host": "smtp-sink", "smtp_port": 1025, "smtp_username": from, "smtp_secret": m3lSinkPassword(t),
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
		// An unverified or unaligned account never sends and never errors: it would turn every wait below into a timeout with no explanation, so it is refused here, naming what module 3 said.
		if acc.Verification.State != "verified" || acc.Alignment.State != "aligned" {
			t.Fatalf("the account of %s: module 3 says verification.state=%q (want verified) and from_alignment.state=%q (want aligned): the worker could not verify the sink, or the From gate is not off\n  body: %s",
				tn.Name, acc.Verification.State, acc.Alignment.State, r.env.snip(body))
		}
		tn.AccountID = acc.ID
	}
	t.Logf("pacing: concurrency_per_user=%d, send delay %d-%d s for both users; accounts %s (U1) and %s (U2) are verified and aligned", m3lConcurrency, m3lDelaySec, m3lDelaySec, r.u1.AccountID, r.u2.AccountID)
}

// ---- 4: the entitlements, delivered to module 3 by the SERVICE ----

// entitlementsDelivered enables U1 (dated an hour ahead: a dated entitlement that must NOT be expired by the service's loop while it is in the future) and U2 (no end date) through the store, then waits for the
// service's reconcile loop to deliver both to the real module 3: module 3 answers unsuspend with a conflict (the user was never suspended), the service reads it back through the existence probe, and records
// converged at observed "active".
func (r *m3lRun) entitlementsDelivered(t *testing.T) {
	until := r.dbNow(t).Add(m3lFarFuture)
	e1 := r.mustEnable(t, r.u1, &until, "enable")
	e2 := r.mustEnable(t, r.u2, nil, "enable")
	t.Logf("enabled at database time %s: U1 until %s (ledger %d), U2 with no end date (ledger %d), by admin %s", m3lT(e1.CreatedAt), m3lT(until), e1.LedgerID, e2.LedgerID, r.admin)

	st1 := r.waitSync(t, r.u1, 1, false, "active", 60*time.Second)
	st2 := r.waitSync(t, r.u2, 1, false, "active", 60*time.Second)
	for _, tn := range []*m3lTenant{r.u1, r.u2} {
		if s := r.suspension(t, tn.UserID); s.Suspended {
			t.Fatalf("module 3 holds %s as suspended right after an enable: %+v", tn.Name, s)
		}
	}
	t.Logf("delivered: U1 converged at version %d, observed active at %s; U2 at version %d, observed active at %s", st1.WantVersion, m3lTP(st1.ObservedAt), st2.WantVersion, m3lTP(st2.ObservedAt))
}

// ---- 5: sending works while entitled (the paired control) ----

func (r *m3lRun) run(t *testing.T, tn *m3lTenant, itemID string) {
	t.Helper()
	status, body := r.env.do(t, r.env.m3URL, http.MethodPost, "/v1/run", r.env.token, map[string]string{"job_item_id": itemID, "user_id": tn.UserID})
	r.env.expect(t, fmt.Sprintf("POST /v1/run for a job item of %s", tn.Name), status, body, http.StatusAccepted, "")
	var ok struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &ok); err != nil || ok.JobID == "" || ok.State != "queued" {
		t.Fatalf("POST /v1/run for a job item of %s answered 202 with %s, want {job_id, state: queued}", tn.Name, r.env.snip(body))
	}
}

func (r *m3lRun) sending(t *testing.T) {
	// Every item of both users is accepted BEFORE any suspension (a suspended user's /v1/run is refused with 409). Interleaved, so both start at about the same time.
	start := r.dbNow(t)
	for i := 0; i < len(r.u1.ItemIDs) || i < len(r.u2.ItemIDs); i++ {
		for _, tn := range []*m3lTenant{r.u1, r.u2} {
			if i < len(tn.ItemIDs) {
				r.run(t, tn, tn.ItemIDs[i])
			}
		}
	}
	t.Logf("accepted %d + %d items (202) in %s", len(r.u1.ItemIDs), len(r.u2.ItemIDs), r.dbNow(t).Sub(start).Round(time.Millisecond))

	// The paired control: sending works at all, for both users, while U1 is entitled. Without it a stack that never sent would pass the halt below.
	var view m3lSinkView
	m3lWait(t, r.ctx, fmt.Sprintf("the sink holds at least %d messages to U1's recipients and %d to U2's", m3lMinU1, m3lMinU2), 90*time.Second, 500*time.Millisecond, func() (bool, string) {
		view = r.sinkView(t)
		if row := r.entRow(t, r.u1.UserID); !row.Enabled {
			t.Fatalf("U1's entitlement ended (database time %s, enabled_until %s, changed_by set: %v) while its date was an hour ahead: the service's expiry loop must not expire an entitlement whose date has not passed", m3lT(row.DBNow), m3lTP(row.Until), row.ChangedBy != nil)
		}
		c1, c2 := view.of(r.u1), view.of(r.u2)
		return c1 >= m3lMinU1 && c2 >= m3lMinU2, fmt.Sprintf("U1 %d (want >= %d), U2 %d (want >= %d), sink total %d (it held %d before this test)", c1, m3lMinU1, c2, m3lMinU2, view.Total, r.sinkBaseline)
	})
	t.Logf("control: sending works while entitled: sink holds %d of U1's and %d of U2's messages; U1's entitlement, dated %s, is still enabled", view.of(r.u1), view.of(r.u2), m3lTP(r.entRow(t, r.u1.UserID).Until))
}

// ---- 6: the expiry, the REAL path ----

// expiry makes U1's date pass and observes the whole real path: the service's expiry loop writes an m3_expire ledger row (acting_admin_id NULL), the entitlement becomes disabled, the want is "suspended" at
// version 2, the service's reconcile loop calls module 3's real suspend route, module 3 records the suspension AFTER the expiry, and the service records convergence only once module 3 answered.
func (r *m3lRun) expiry(t *testing.T) {
	u1, u2 := r.u1, r.u2

	// The admin moves the date a few seconds ahead: an Enable of an already enabled entitlement re-dates it (no new want_version: the wanted state is still "active").
	until := r.dbNow(t).Add(m3lExpiryIn).Truncate(time.Microsecond)
	en := r.mustEnable(t, u1, &until, "redate-to-expire")
	if en.Until == nil || !en.Until.Equal(until) {
		t.Fatalf("the re-date was recorded with enabled-until %s, want %s", m3lTP(en.Until), m3lT(until))
	}
	r.redatedUntil = until
	if row := r.entRow(t, u1.UserID); !row.Enabled || row.Until == nil || !row.Until.Equal(until) {
		t.Fatalf("after the re-date U1's entitlement is enabled=%v until=%s, want enabled until %s", row.Enabled, m3lTP(row.Until), m3lT(until))
	}
	if st, err := r.store.Sync(r.ctx, u1.UserID); err != nil || !st.Exists || st.WantSuspended || st.WantVersion != 1 {
		t.Fatalf("after the re-date U1's delivery row is %s (err %v), want the wanted state still active at version 1: a re-date must not change what module 3 is told", r.syncString(st), err)
	}

	// Observe the SERVICE expire it. The date passes ~10 s after the re-date; the service's loop runs every two seconds.
	var seen m3lEntRow
	m3lWait(t, r.ctx, "the platform service's expiry loop ends U1's entitlement once its date has passed (is cmd/platform running its entitlement loops against this database?)", m3lExpiryIn+45*time.Second, 250*time.Millisecond, func() (bool, string) {
		seen = r.entRow(t, u1.UserID)
		return seen.Exists && !seen.Enabled, fmt.Sprintf("database time %s: enabled=%v, enabled_until=%s (the date to pass: %s)", m3lT(seen.DBNow), seen.Enabled, m3lTP(seen.Until), m3lT(until))
	})
	r.observedAt = seen.DBNow
	r.k0 = r.sinkView(t).of(u1) // k0: U1's messages in the sink at the moment the expiry was seen
	if seen.DBNow.Before(until) {
		t.Fatalf("U1's entitlement was disabled at database time %s, before its date %s: the database must refuse an early expiry", m3lT(seen.DBNow), m3lT(until))
	}
	if seen.ChangedBy != nil || seen.Until != nil {
		t.Fatalf("the entitlement is disabled but changed_by=%v enabled_until=%s: an automatic expiry leaves changed_by NULL and clears the date", seen.ChangedBy, m3lTP(seen.Until))
	}

	// The ledger explains it: the two admin enables and exactly one m3_expire, written by the service (no acting admin) after the date.
	rows, err := r.db.Query(r.ctx, "SELECT kind, acting_admin_id::text, created_at FROM platform.ledger WHERE user_id = $1::uuid AND module_id = 'm3' ORDER BY id", u1.UserID)
	if err != nil {
		t.Fatalf("read U1's module-3 ledger rows: %v", err)
	}
	var kinds []string
	var expireAdmin *string
	for rows.Next() {
		var kind string
		var admin *string
		var at time.Time
		if err := rows.Scan(&kind, &admin, &at); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
		if kind == "m3_expire" {
			expireAdmin = admin
			r.expireAt = at.UTC()
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(kinds, ","), "m3_enable,m3_enable,m3_expire"; got != want {
		t.Fatalf("U1's module-3 ledger rows are %s, want %s (the two admin enables, then exactly one automatic expiry and no disable)", got, want)
	}
	if expireAdmin != nil {
		t.Fatalf("the m3_expire row names admin %s: an automatic expiry has no acting admin", *expireAdmin)
	}
	if r.expireAt.Before(until) {
		t.Fatalf("the m3_expire row is stamped %s, before the entitlement's date %s", m3lT(r.expireAt), m3lT(until))
	}

	// The wanted state is now "suspended" at version 2 (the re-date did not bump it, the expiry did). That the delivery row is NOT YET converged at this instant is deliberately not asserted: the service's reconcile
	// loop runs every two seconds on its own clock and may already have delivered it. What is asserted instead is stronger and race-free: module 3's own record of the suspension is stamped AFTER the expiry (below),
	// so the convergence cannot have been recorded before module 3 changed state.
	st, err := r.store.Sync(r.ctx, u1.UserID)
	if err != nil || !st.Exists || !st.WantSuspended || st.WantVersion != 2 {
		t.Fatalf("right after the expiry U1's delivery row is %s (err %v), want want_suspended=true at want_version 2", r.syncString(st), err)
	}
	r.expiryVersion = st.WantVersion
	if ent, err := r.store.Get(r.ctx, u1.UserID); err != nil || !ent.Exists || ent.Enabled || ent.Until != nil || ent.ChangedBy != nil {
		t.Fatalf("Store.Get(U1) = %+v (err %v), want a disabled entitlement with no date and no acting admin", ent, err)
	}

	// Delivery: the service calls module 3's REAL suspend route and records convergence only after module 3 answered "suspended".
	st = r.waitSync(t, u1, 2, true, "suspended", 60*time.Second)

	// Module 3's own record agrees, and was written AFTER the expiry (the cause), and before the platform recorded the answer.
	s := r.suspension(t, u1.UserID)
	if !s.Exists || !s.Suspended || s.SuspendedAt == nil {
		t.Fatalf("the platform recorded U1 converged at 'suspended' but module 3's own table says %+v: the two records disagree", s)
	}
	if s.Reason == nil || *s.Reason != "operator" {
		t.Fatalf("module 3 recorded the suspension with reason %v, want 'operator' (the only reason module 3's admin route writes)", s.Reason)
	}
	r.suspendedAt = *s.SuspendedAt
	if !r.suspendedAt.After(r.expireAt) {
		t.Fatalf("module 3 recorded the suspension at %s, not after the platform's expiry at %s: something other than the expiry suspended U1", m3lT(r.suspendedAt), m3lT(r.expireAt))
	}
	if st.ObservedAt.Before(r.suspendedAt) {
		t.Fatalf("the platform recorded module 3's answer at %s, before module 3 recorded the suspension at %s", m3lTP(st.ObservedAt), m3lT(r.suspendedAt))
	}

	// The control tenant is untouched.
	if s2 := r.suspension(t, u2.UserID); s2.Suspended {
		t.Fatalf("U2, the control, is suspended in module 3: %+v", s2)
	}
	if st2, err := r.store.Sync(r.ctx, u2.UserID); err != nil || !st2.Converged() || st2.Observed != "active" {
		t.Fatalf("U2's delivery row is %s (err %v), want converged and active", r.syncString(st2), err)
	}
	t.Logf("expiry (database clock): re-dated to %s; entitlement seen disabled at %s; m3_expire ledger row at %s; module 3 recorded the suspension at %s (%s after the expiry); platform recorded the answer at %s; k0 = %d of U1's messages in the sink when the expiry was seen",
		m3lT(until), m3lT(r.observedAt), m3lT(r.expireAt), m3lT(r.suspendedAt), r.suspendedAt.Sub(r.expireAt).Round(time.Millisecond), m3lTP(st.ObservedAt), r.k0)
}

// ---- 7: the halt ----

func (r *m3lRun) halt(t *testing.T) {
	u1, u2 := r.u1, r.u2
	ts := r.suspendedAt

	// An already claimed job finishes (an SMTP session underway cannot be recalled): wait, bounded, until nothing of U1 is in flight, THEN count.
	m3lWait(t, r.ctx, "U1's campaign report shows totals.in_flight == 0 after the suspension", 60*time.Second, 500*time.Millisecond, func() (bool, string) {
		rep := r.report(t, u1, 1)
		return rep.Totals.InFlight == 0, rep.Totals.String()
	})
	view := r.sinkView(t)
	r.s1 = view.of(u1)
	rep := r.report(t, u1, 200)
	logs := r.count(t, "count U1's send_logs rows", "SELECT count(*) FROM send_byo.send_logs WHERE user_id = $1::uuid", u1.UserID)

	// S1 = U1's sink count with nothing in flight. With ONE lane (concurrency_per_user=1, one send per >= 2 s) the only sends that can still complete after the expiry was SEEN are the ones claimed before module 3
	// recorded the suspension: at most one per two seconds in the window between the observation and the suspension, plus the one that was in flight when k0 was counted (and one of slack for the clock reads).
	window := r.suspendedAt.Sub(r.observedAt)
	if window < 0 {
		window = 0
	}
	startable := int((window + time.Second) / (m3lDelaySec * time.Second)) // sends that can start in the window: one per 2 s, a second of slack for the two clock reads
	allowed := startable + 2                                               // plus the send at the window's edge, plus the one in flight when k0 was counted
	if r.s1 < r.k0 || r.s1 > r.k0+allowed {
		t.Fatalf("U1's sink count after the halt is S1 = %d, but it was k0 = %d when the expiry was seen (database time %s) and module 3 recorded the suspension at %s (%s later): allowed k0 <= S1 <= k0 + %d "+
			"(with one lane and a %d s delay, floor((%s + 1s) / %ds) = %d send(s) can start in that window, plus 1 for the send at the window's edge, plus 1 in flight when k0 was counted); more than that means the suspension did not stop the dispatcher",
			r.s1, r.k0, m3lT(r.observedAt), m3lT(ts), window.Round(time.Millisecond), allowed, m3lDelaySec, window.Round(time.Millisecond), m3lDelaySec, startable)
	}
	if r.s1 != rep.Totals.Sent || r.s1 != logs {
		t.Fatalf("U1: the sink holds S1 = %d messages, module 3's report says totals.sent = %d and send_byo.send_logs holds %d rows for U1: the three must agree once nothing is in flight", r.s1, rep.Totals.Sent, logs)
	}
	if left := m3lN1 - r.s1; left < 3 {
		t.Fatalf("only %d of U1's %d items are left after the halt (S1 = %d): too few remain for the halt to prove anything; the run was too fast or the expiry too late", left, m3lN1, r.s1)
	}

	// The sharp assertions, on module 3's own event log and the database clock: no job of U1 was CLAIMED (queued -> assigned) after module 3 recorded the suspension, and at most one (the lane) completed after it.
	// A zero is only meaningful if the same query finds the claims before it (the positive control), so both are asserted.
	claimsAfter := r.count(t, "U1's claims after the suspension", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND from_state = 'queued' AND to_state = 'assigned' AND created_at > $2::timestamptz", u1.UserID, ts)
	claimsBefore := r.count(t, "U1's claims up to the suspension", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND from_state = 'queued' AND to_state = 'assigned' AND created_at <= $2::timestamptz", u1.UserID, ts)
	sentAfter := r.count(t, "U1's completions after the suspension", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND to_state = 'sent' AND created_at > $2::timestamptz", u1.UserID, ts)
	if claimsBefore < r.s1 || claimsBefore == 0 {
		t.Fatalf("the control of the event query failed: %d queued->assigned events of U1 up to the suspension, but %d of its jobs were sent: the query does not see claims, so a zero after the suspension would prove nothing", claimsBefore, r.s1)
	}
	if claimsAfter != 0 {
		t.Fatalf("module 3 claimed (queued -> assigned) %d job(s) of U1 AFTER it recorded the suspension at %s (database clock): the dispatcher did not honour the suspension", claimsAfter, m3lT(ts))
	}
	if sentAfter > m3lConcurrency {
		t.Fatalf("%d of U1's jobs completed after the suspension at %s, but with concurrency_per_user=%d at most %d (the one already claimed) can", sentAfter, m3lT(ts), m3lConcurrency, m3lConcurrency)
	}

	// U1's queued items are still queued: not failed, not lost (module 3's report, and independently its jobs table).
	queued0 := rep.Totals.Queued()
	if rep.Totals.Total != m3lN1 || rep.Totals.Sent+queued0 != m3lN1 || rep.Totals.Failed != 0 || rep.Totals.Bounced != 0 || rep.Totals.InFlight != 0 {
		t.Fatalf("U1's campaign after the halt: %s, want total=%d, sent + pending + waiting_for_capacity = %d, failed=0, bounced=0, in_flight=0", rep.Totals, m3lN1, m3lN1)
	}
	if rep.DispatchPaused {
		t.Fatal("U1's report says dispatch_paused: the global rung is paused, so the halt is not the per-user suspension this test is about")
	}
	if len(rep.Items) != m3lN1 || rep.NextCursor != "" {
		t.Fatalf("U1's report lists %d item(s) (next_cursor %q), want all %d on one page of 200", len(rep.Items), rep.NextCursor, m3lN1)
	}
	byState := map[string]int{}
	for _, it := range rep.Items {
		byState[it.State]++
	}
	if byState["sent"] != r.s1 || byState["queued"] != queued0 || byState["sent"]+byState["queued"] != m3lN1 {
		t.Fatalf("U1's items by state are %v, want only sent=%d and queued=%d", byState, r.s1, queued0)
	}
	jobStates := map[string]int{}
	jrows, err := r.db.Query(r.ctx, "SELECT state::text, count(*) FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid GROUP BY 1", u1.UserID, u1.CampaignID)
	if err != nil {
		t.Fatalf("read U1's jobs: %v", err)
	}
	for jrows.Next() {
		var state string
		var n int
		if err := jrows.Scan(&state, &n); err != nil {
			jrows.Close()
			t.Fatal(err)
		}
		jobStates[state] = n
	}
	jrows.Close()
	if err := jrows.Err(); err != nil {
		t.Fatal(err)
	}
	if jobStates["sent"] != r.s1 || jobStates["queued"] != queued0 || len(jobStates) != 2 {
		t.Fatalf("send_byo.jobs of U1 by state is %v, want exactly sent=%d and queued=%d (nothing failed, nothing lost, nothing in flight)", jobStates, r.s1, queued0)
	}

	// THE HOLD: for at least 8 seconds (database clock) U1's sink count does not change and its queued items do not drop, while U2 keeps being sent to.
	holdStart := r.dbNow(t)
	u2Start, u2Now := -1, -1
	holdBegan := time.Now()
	samples := 0
	for {
		v := r.sinkView(t)
		if got := v.of(u1); got != r.s1 {
			t.Fatalf("U1's sink count moved from %d to %d during the hold, %s after it began (database time %s): a suspended user's items were sent", r.s1, got, time.Since(holdBegan).Round(time.Millisecond), m3lT(r.dbNow(t)))
		}
		if samples == 0 {
			u2Start = v.of(u2) // the control's count when the hold begins
		}
		cur := r.report(t, u1, 1)
		if cur.Totals.Sent != r.s1 || cur.Totals.Queued() != queued0 || cur.Totals.InFlight != 0 || cur.Totals.Failed != 0 || cur.Totals.Bounced != 0 || cur.Totals.Total != m3lN1 || cur.DispatchPaused {
			t.Fatalf("during the hold U1's campaign changed: %s (dispatch_paused=%v), want sent=%d, pending + waiting_for_capacity=%d, nothing else", cur.Totals, cur.DispatchPaused, r.s1, queued0)
		}
		if s := r.suspension(t, u1.UserID); !s.Suspended {
			t.Fatalf("module 3 no longer records U1 as suspended during the hold: %+v", s)
		}
		u2Now = v.of(u2)
		samples++
		if samples >= 8 && time.Since(holdBegan) >= m3lHoldFor {
			break
		}
		select {
		case <-r.ctx.Done():
			t.Fatal("the test's overall time budget ended during the hold")
		case <-time.After(time.Second):
		}
	}
	holdEnd := r.dbNow(t)
	if holdEnd.Sub(holdStart) < m3lHoldFor {
		t.Fatalf("the hold lasted %s on the database clock, want at least %s", holdEnd.Sub(holdStart), m3lHoldFor)
	}

	// The control: the stack was alive the whole time. U2, never suspended, was still being sent to (or had finished all its items).
	u2Rep := r.report(t, u2, 1)
	u2Claims := r.count(t, "U2's claims after U1's suspension", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND from_state = 'queued' AND to_state = 'assigned' AND created_at > $2::timestamptz", u2.UserID, ts)
	if !(u2Now > u2Start || u2Rep.IsComplete) {
		t.Fatalf("U2's sink count did not move during U1's %d-second hold (%d -> %d, report %s): U1 was silent but so was the whole stack, so the halt cannot be attributed to the suspension", int(m3lHoldFor.Seconds()), u2Start, u2Now, u2Rep.Totals)
	}
	if u2Claims == 0 && !u2Rep.IsComplete {
		t.Fatalf("module 3 claimed nothing for U2 after U1's suspension at %s and U2 is not complete (%s): the dispatcher is not running", m3lT(ts), u2Rep.Totals)
	}
	t.Logf("halt: S1 = %d (k0 = %d, allowed up to %d); report sent = %d, send_logs = %d; claims of U1 before the suspension %d, after it 0; completions after it %d; %d item(s) still queued, none failed or lost; held %s (database clock) with U1 silent while U2 went from %d to %d messages (%d claims after the suspension)",
		r.s1, r.k0, r.k0+allowed, rep.Totals.Sent, logs, claimsBefore, sentAfter, queued0, holdEnd.Sub(holdStart).Round(time.Millisecond), u2Start, u2Now, u2Claims)
}

// ---- 8: re-enable resumes the same items ----

func (r *m3lRun) resume(t *testing.T) {
	u1, u2 := r.u1, r.u2

	// The admin re-enables with no end date; the service's reconcile loop tells module 3, which answers unsuspend with a 200 (it changed state): its own record shows U1 reinstated AFTER the re-enable.
	en := r.mustEnable(t, u1, nil, "re-enable")
	st := r.waitSync(t, u1, r.expiryVersion+1, false, "active", 60*time.Second)
	s := r.suspension(t, u1.UserID)
	if !s.Exists || s.Suspended || s.ReinstatedAt == nil {
		t.Fatalf("the platform recorded U1 converged at 'active' but module 3's own table says %+v: module 3 did not reinstate U1", s)
	}
	if !s.ReinstatedAt.After(en.CreatedAt.UTC()) {
		t.Fatalf("module 3 reinstated U1 at %s, not after the admin's re-enable at %s: the 'active' the platform recorded was not module 3 answering this re-enable", m3lT(*s.ReinstatedAt), m3lT(en.CreatedAt))
	}
	if ent := r.entRow(t, u1.UserID); !ent.Enabled || ent.Until != nil {
		t.Fatalf("after the re-enable U1's entitlement is enabled=%v until=%s, want enabled with no date", ent.Enabled, m3lTP(ent.Until))
	}
	t.Logf("re-enabled at database time %s; module 3 reinstated U1 at %s; the platform recorded convergence (version %d, observed active) at %s", m3lT(en.CreatedAt), m3lT(*s.ReinstatedAt), st.WantVersion, m3lTP(st.ObservedAt))

	// U1's remaining items complete: the SAME job items go queued -> sent.
	var last m3lReport
	m3lWait(t, r.ctx, "U1's campaign completes after the re-enable (progress.is_complete)", 180*time.Second, 2*time.Second, func() (bool, string) {
		v := r.sinkView(t) // fails at once on a duplicate
		last = r.report(t, u1, 1)
		return last.IsComplete, fmt.Sprintf("%s; U1 sink count %d", last.Totals, v.of(u1))
	})
	m3lWait(t, r.ctx, "U2's campaign completes", 120*time.Second, 2*time.Second, func() (bool, string) {
		last = r.report(t, u2, 1)
		return last.IsComplete, last.Totals.String()
	})
	for i := 0; i < 3; i++ { // a late duplicate would show up here
		select {
		case <-r.ctx.Done():
			t.Fatal("the test's overall time budget ended while settling")
		case <-time.After(time.Second):
		}
		r.sinkView(t)
	}

	// Exactly once: the sink holds every recipient of both users exactly once (sinkView already refused any duplicate or stranger), none lost.
	view := r.sinkView(t)
	for _, tn := range []*m3lTenant{u1, u2} {
		var missing []string
		for _, rc := range tn.Recipients {
			if view.Counts[rc] != 1 {
				missing = append(missing, fmt.Sprintf("%s x%d", rc, view.Counts[rc]))
			}
		}
		if len(missing) > 0 {
			t.Fatalf("%s: the sink does not hold every recipient exactly once: %s", tn.Name, strings.Join(missing, ", "))
		}
		if got := view.of(tn); got != len(tn.Recipients) {
			t.Fatalf("%s: the sink holds %d messages to its recipients, want %d", tn.Name, got, len(tn.Recipients))
		}
	}

	// The SAME items: module 3's report lists exactly the inserted job item ids as sent, and so does its jobs table.
	for _, tn := range []*m3lTenant{u1, u2} {
		rep := r.report(t, tn, 200)
		if rep.Totals.Total != len(tn.ItemIDs) || rep.Totals.Sent != len(tn.ItemIDs) || rep.Totals.Failed != 0 || rep.Totals.Bounced != 0 || rep.Totals.InFlight != 0 || rep.Totals.Queued() != 0 || !rep.IsComplete {
			t.Fatalf("%s's campaign at the end: %s (complete=%v), want total=sent=%d and nothing else", tn.Name, rep.Totals, rep.IsComplete, len(tn.ItemIDs))
		}
		if len(rep.Items) != len(tn.ItemIDs) || rep.NextCursor != "" {
			t.Fatalf("%s's report lists %d item(s) (next_cursor %q), want %d on one page", tn.Name, len(rep.Items), rep.NextCursor, len(tn.ItemIDs))
		}
		var sentIDs []string
		for _, it := range rep.Items {
			if it.State == "sent" {
				sentIDs = append(sentIDs, it.JobItemID)
			}
		}
		if !m3lSameSet(sentIDs, tn.ItemIDs) {
			t.Fatalf("%s: the job item ids module 3's report lists as sent are not the ids inserted:\n  reported %v\n  inserted %v", tn.Name, m3lSorted(sentIDs), m3lSorted(tn.ItemIDs))
		}
		var dbIDs []string
		drows, err := r.db.Query(r.ctx, "SELECT job_item_id::text FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid AND state = 'sent'", tn.UserID, tn.CampaignID)
		if err != nil {
			t.Fatalf("read %s's sent jobs: %v", tn.Name, err)
		}
		for drows.Next() {
			var id string
			if err := drows.Scan(&id); err != nil {
				drows.Close()
				t.Fatal(err)
			}
			dbIDs = append(dbIDs, id)
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			t.Fatal(err)
		}
		if !m3lSameSet(dbIDs, tn.ItemIDs) {
			t.Fatalf("%s: the job items module 3's jobs table holds as sent are not the ids inserted:\n  stored   %v\n  inserted %v", tn.Name, m3lSorted(dbIDs), m3lSorted(tn.ItemIDs))
		}
		if n := r.count(t, "count send_logs", "SELECT count(*) FROM send_byo.send_logs WHERE user_id = $1::uuid", tn.UserID); n != len(tn.ItemIDs) {
			t.Fatalf("%s: send_byo.send_logs holds %d rows, want %d", tn.Name, n, len(tn.ItemIDs))
		}
	}
	after := r.count(t, "U1's claims after the reinstatement", "SELECT count(*) FROM send_byo.job_events WHERE user_id = $1::uuid AND from_state = 'queued' AND to_state = 'assigned' AND created_at > $2::timestamptz", u1.UserID, s.ReinstatedAt.UTC())
	if after < m3lN1-r.s1 {
		t.Fatalf("only %d claims of U1 after the reinstatement, but %d items were still queued at the halt: they cannot all have been sent", after, m3lN1-r.s1)
	}
	t.Logf("resume: U1's %d items (%d sent before the halt, %d after the re-enable) and U2's %d are each in the sink exactly once; the sets of job item ids are the inserted ones; no duplicates, nothing lost", m3lN1, r.s1, m3lN1-r.s1, m3lN2)
}

// ---- small set helpers ----

func m3lSorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// m3lSameSet is true when a and b hold the same ids, each once.
func m3lSameSet(a, b []string) bool {
	x, y := m3lSorted(a), m3lSorted(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] || (i > 0 && x[i] == x[i-1]) {
			return false
		}
	}
	return true
}
