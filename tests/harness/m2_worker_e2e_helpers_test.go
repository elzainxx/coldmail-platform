//go:build harness && m2worker

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Helpers of the module 2 ADAPTER tests (m2_worker_e2e_test.go): Phase 3 chunks B9 (end to end), B10 (pagination), B11 (cross-tenant) and B13 (cancel) for module 2. Every name starts with `m2e`
// so it cannot clash with another file's helpers; the file builds on the m2w* helpers (m2_worker_helpers_test.go) for what already works against the real worker: the tenant, its pool and
// account (module 2's admin API), the sink, the dials, the cleanups.
//
// WHAT IS DRIVEN, AND WHAT IS NOT. Every call to module 2's PLATFORM API in these tests goes through internal/adapters/m2 and internal/adapters/sendapi (Quote, Run, ReportJob,
// ReportCampaign / Walk / ReportCampaignPage, Cancel, Classify) over a modclient built the way the service builds it (m2.FromConfig). No request to /v1/quote, /v1/run, /v1/report or /v1/cancel
// is written by hand here. What module 2 needs before it can send (a pool, an account, the user and the job items) is set up through its operator API and the platform's two tables, exactly as
// in m2_worker_test.go; that is the tenant's own setup, not the adapter's.
//
// THE TENANT. The login is a real auth.users row and the platform user a real public.users row on the LIVE database (m2wRun.newTenant); the module scope the adapter is built from comes from
// internal/tenant (ByAuthUser, then Scope: the platform user id, read back from the database, never the id the test wrote down).
//
// THE MONEY. The platform's money core runs on the LIVE database, as the platform_app login the service has (not on a scratch database like the module 1 test of the same kind): module 2 reads
// public.users and public.job_items of the live database, so the tenants live there, and the scoped run (H_SCOPE=m2worker) does not build the migration binary a scratch database needs. Each test
// has users of its own, so nothing of another test's money is touched, and every check of the books is made per user and per job; the two global invariants (platform.balance_drift and
// platform.reserve_overdrawn) are empty on the live database at every moment a test asserts them.
//
// THE ORACLE. m2eHand is the charge table written out by hand from the owner's rules (sent and bounced are charged, every failed and cancelled item is free, an item that is not final waits)
// WITHOUT reading rules.psv or calling charge.Decide: the comparison asks both and reports a disagreement, so a mapping bug and an oracle bug cannot cancel.

const (
	m2eGrant = 1000 // credits given to a tenant before it reserves
)

// m2eEnv is the adapter side of a test: module 2's adapter, the tenant resolver, the money store and the superuser connection the checks read the ledger with.
type m2eEnv struct {
	r     *m2wRun
	cfg   config.Config
	mc    *modclient.Client // the module client the page-size tests build their own sendapi.Client over (m2.Adapter.For fixes the page size)
	ad    *m2.Adapter       // built by m2.FromConfig, the way the service builds it
	pool  *pgxpool.Pool     // real logins as platform_app on the live database
	res   *tenant.Resolver
	store *money.Store
	admin string // an active platform admin (the acting admin of a grant)
}

func newM2eEnv(t *testing.T, r *m2wRun) *m2eEnv {
	t.Helper()
	cfg := config.Config{M2URL: r.m2URL, M2ServiceToken: r.token, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
	if _, err := m2.FromConfig(cfg); err != nil { // the way the service builds its adapter: the configuration must be accepted as it stands
		t.Fatalf("m2.FromConfig refused the harness configuration (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	// the adapter the tests drive is built over the same options with the contract drift session's observer added (zz_m2_worker_contract_drift_test.go): every response module 2 gives it is compared with its recorded fixture
	opts := modclient.FromConfig(cfg)
	opts.Observe = m2Drift.Observer()
	mc, err := modclient.New(modclient.M2, r.m2URL, r.token, nil, opts)
	if err != nil {
		t.Fatalf("modclient.New: %s", r.scrub(err.Error()))
	}
	ad, err := m2.NewAdapter(mc)
	if err != nil {
		t.Fatalf("m2.NewAdapter: %s", r.scrub(err.Error()))
	}
	pool := mnyPool(t, requireEnv(t, envAdminURL))
	res, err := tenant.NewResolver(pool, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	e := &m2eEnv{r: r, cfg: cfg, mc: mc, ad: ad, pool: pool, res: res, store: money.New(pool)}
	if err := r.db.QueryRow(r.ctx, "SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at LIMIT 1").Scan(&e.admin); err != nil {
		t.Fatalf("platform.admins has no active admin (the first-admin stage must have run: every grant is written against a named admin): %v", err)
	}
	return e
}

// client is the adapter bound to a tenant, built the way the platform will build it: the login resolves to its platform user (internal/tenant ByAuthUser), the scope names module 2's user id
// (Scope re-reads the database and refuses a Tenant written by hand), and the adapter is built for THAT id.
func (e *m2eEnv) client(t *testing.T, tn *m2wTenant) *m2.Client {
	t.Helper()
	ten, err := e.res.ByAuthUser(e.r.ctx, tn.UserID) // a platform user IS its login (users_auth_link: id = auth_user_id), so the login id is the user id
	if err != nil {
		t.Fatalf("tenant.ByAuthUser(%s): %v", tn.Name, err)
	}
	if ten.UserID != tn.UserID || !ten.Active {
		t.Fatalf("tenant.ByAuthUser(%s) resolved to user %s (active %v), want %s active", tn.Name, ten.UserID, ten.Active, tn.UserID)
	}
	sc, err := e.res.Scope(e.r.ctx, ten)
	if err != nil {
		t.Fatalf("tenant.Scope(%s): %v", tn.Name, err)
	}
	if sc.M2UserID != tn.UserID {
		t.Fatalf("the module 2 scope of %s names user %s, want %s", tn.Name, sc.M2UserID, tn.UserID)
	}
	c, err := e.ad.For(sc.M2UserID)
	if err != nil {
		t.Fatalf("m2.Adapter.For(%s): %s", tn.Name, e.r.scrub(err.Error()))
	}
	if c.UserID() != tn.UserID {
		t.Fatalf("the adapter of %s acts for user %s", tn.Name, c.UserID())
	}
	return c
}

// pager is the shared sender client of a tenant with a page size of the test's choosing (m2.Adapter.For fixes it at the module's maximum): the same body, module 2's profile.
func (e *m2eEnv) pager(t *testing.T, tn *m2wTenant, pageSize int) *sendapi.Client {
	t.Helper()
	c, err := sendapi.New(e.mc, m2.Profile(), tn.UserID, sendapi.Options{PageSize: pageSize})
	if err != nil {
		t.Fatalf("sendapi.New(page size %d): %s", pageSize, e.r.scrub(err.Error()))
	}
	return c
}

// cfgInt reads one integer dial of module 2 through its operator API: the module's own number, never a constant of the test.
func (e *m2eEnv) cfgInt(t *testing.T, key string) int64 {
	t.Helper()
	status, body := e.r.admin(t, "GET", "/v1/admin/config/"+key, nil)
	e.r.expect(t, "GET /v1/admin/config/"+key, status, body, 200, "")
	var row struct {
		Value int64 `json:"value"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("config %s is not an integer row (%v): %s", key, err, e.r.snip(body, 300))
	}
	return row.Value
}

// ---- the money side ----

// fundAndReserve gives the tenant credits, creates the platform job that will own the campaign and reserves its units at the dearest frozen price, as the job engine will, and moves the job to
// `running`. It returns the job id and the reserve.
func (e *m2eEnv) fundAndReserve(t *testing.T, tn *m2wTenant, units int) (string, money.ReserveResult) {
	t.Helper()
	ctx := e.r.ctx
	if _, err := e.store.Grant(ctx, money.GrantReq{UserID: tn.UserID, Module: "m2", Amount: m2eGrant, AdminID: e.admin, Reason: "module 2 adapter end to end", Key: "m2e-grant-" + randomHex(t, 6)}); err != nil {
		t.Fatalf("grant for %s: %v", tn.Name, err)
	}
	var job string
	if err := e.r.db.QueryRow(ctx, "INSERT INTO platform.jobs (user_id, module_id) VALUES ($1::uuid, 'm2') RETURNING id::text", tn.UserID).Scan(&job); err != nil {
		t.Fatalf("create the platform job of %s: %v", tn.Name, err)
	}
	rsv, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: units})
	if err != nil {
		t.Fatalf("reserve %d unit(s) for %s: %v", units, tn.Name, err)
	}
	e.setStatus(t, job, "running")
	return job, rsv
}

// setStatus walks the platform job to a status along the lifecycle of platform_0011, as the job engine will (jobWalkTo: each hop is a platform.job_transition with its actor and reason; module 2
// can cancel a running job, so running -> cancelling -> completing -> cancelled is a legal way to the end of a cancelled job).
func (e *m2eEnv) setStatus(t *testing.T, job, status string) {
	t.Helper()
	jobWalkTo(t, e.r.db, job, status)
}

// m2eCharge is one charge row of the ledger.
type m2eCharge struct {
	ItemRef string
	Unit    string
	Cost    int64
	Units   int64
}

func (e *m2eEnv) ledger(t *testing.T, job string) []m2eCharge {
	t.Helper()
	rows, err := e.r.db.Query(e.r.ctx, `SELECT item_ref, unit, unit_cost, units FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge' ORDER BY id`, job)
	if err != nil {
		t.Fatalf("read the ledger of job %s: %v", job, err)
	}
	defer rows.Close()
	var out []m2eCharge
	for rows.Next() {
		var c m2eCharge
		if err := rows.Scan(&c.ItemRef, &c.Unit, &c.Cost, &c.Units); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// prices reads a unit -> cost table: the job's frozen snapshot (job != "") or the current price table of module 2 (job == "").
func (e *m2eEnv) prices(t *testing.T, job string) map[string]int64 {
	t.Helper()
	sql, args := `SELECT unit, cost FROM platform.unit_costs WHERE module_id = 'm2'`, []any{}
	if job != "" {
		sql, args = `SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1::uuid`, []any{job}
	}
	rows, err := e.r.db.Query(e.r.ctx, sql, args...)
	if err != nil {
		t.Fatalf("read the unit prices: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var u string
		var c int64
		if err := rows.Scan(&u, &c); err != nil {
			t.Fatal(err)
		}
		out[u] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no unit prices read (job %q): the instrument matched nothing", job)
	}
	return out
}

func (e *m2eEnv) balance(t *testing.T, tn *m2wTenant) money.Balance {
	t.Helper()
	b, err := e.store.Balance(e.r.ctx, tn.UserID, "m2")
	if err != nil {
		t.Fatalf("balance of %s: %v", tn.Name, err)
	}
	return b
}

// reserveRow is the platform.reserves row of the job (reserved, charged, released).
func (e *m2eEnv) reserveRow(t *testing.T, job string) (reserved, charged, released int64) {
	t.Helper()
	if err := e.r.db.QueryRow(e.r.ctx, "SELECT reserved, charged, released FROM platform.reserves WHERE job_id = $1::uuid", job).Scan(&reserved, &charged, &released); err != nil {
		t.Fatalf("read platform.reserves of job %s: %v", job, err)
	}
	return
}

// fingerprint is everything a settle could write for this user: its ledger (count and newest id), its balance and its reserves.
func (e *m2eEnv) fingerprint(t *testing.T, tn *m2wTenant) string {
	t.Helper()
	var fp string
	err := e.r.db.QueryRow(e.r.ctx, `SELECT (SELECT count(*) FROM platform.ledger WHERE user_id = $1::uuid AND module_id = 'm2')::text
		|| '/' || (SELECT COALESCE(max(id), 0) FROM platform.ledger WHERE user_id = $1::uuid AND module_id = 'm2')::text
		|| '/' || COALESCE((SELECT available::text || ':' || reserved::text FROM platform.balances WHERE user_id = $1::uuid AND module_id = 'm2'), '-')
		|| '/' || COALESCE((SELECT string_agg(reserved::text || ':' || charged::text || ':' || released::text, ',' ORDER BY reserve_id) FROM platform.reserves WHERE user_id = $1::uuid AND module_id = 'm2'), '-')`, tn.UserID).Scan(&fp)
	if err != nil {
		t.Fatalf("fingerprint of the books of %s: %v", tn.Name, err)
	}
	return fp
}

// requireBooks fails when the two invariants of the money core are not empty on the live database: the balance equals what the ledger explains (platform.balance_drift) and no reserve is
// spent beyond itself (platform.reserve_overdrawn).
func (e *m2eEnv) requireBooks(t *testing.T, when string) {
	t.Helper()
	if d := mnyRows(t, e.r.db, `SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`); len(d) != 0 {
		t.Fatalf("%s: platform.balance_drift is not empty (user|module|stored available|ledger available|stored reserved|ledger reserved): %v", when, d)
	}
	if o := mnyRows(t, e.r.db, `SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`); len(o) != 0 {
		t.Fatalf("%s: platform.reserve_overdrawn is not empty (reserve|user|module|job|reserved|settled): %v", when, o)
	}
}

// ---- the charge table, twice ----

// m2eDecision is a charge decision reduced to what the comparison needs: the kind (charge, free, notfinal, unmapped) and the unit of a charge.
type m2eDecision struct {
	Kind string
	Unit string
}

// m2eDecide is what the platform's charge table says (charge.Decide over internal/charge/rules.psv).
func m2eDecide(o charge.Outcome) m2eDecision {
	d := charge.Decide(o)
	return m2eDecision{Kind: d.Kind.String(), Unit: d.Unit}
}

// m2eHand is the same decision written out by hand, from the owner's rules for module 2 (Docs/phase-2-spec.md, "the charge table"): a send the server accepted is charged at the accepted
// unit; a bounce is charged at the bounced unit (the work was done, the bad address is the user's list); a failed item, whatever its reason, and a cancelled item are free (work not done, or
// our own failure); an item that has not finished waits. Anything else is flagged, never guessed. It reads neither rules.psv nor charge.Decide, and the reason does not matter to module 2's rows.
func m2eHand(state string) m2eDecision {
	switch state {
	case "sent":
		return m2eDecision{"charge", "m2_accepted"}
	case "bounced":
		return m2eDecision{"charge", "m2_bounced"}
	case "failed", "cancelled":
		return m2eDecision{"free", ""}
	case "queued", "assigned", "sending":
		return m2eDecision{"notfinal", ""}
	}
	return m2eDecision{"unmapped", ""}
}

func m2eText(p *string) string {
	if p == nil {
		return m2wNull
	}
	return *p
}

func m2eReason(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// m2eCompare is THE comparison of chunk B9: what module 2 reported, what the adapter handed to money.Settle and what the ledger holds, item by item. It returns its problems as sentences (empty
// when everything agrees) instead of failing a test, so that the same function can be shown to catch a corrupted settle list or a corrupted ledger. It checks:
//
//   - every reported item has an id and appears once;
//   - charge.Decide (through decide) and the hand oracle agree on every item;
//   - the settle list holds exactly the terminal items the hand oracle charges or frees, each once, carrying the outcome the module reported, and no item that is not final;
//   - the ledger holds, for each item the oracle charges, exactly one row of the oracle's unit at the job's frozen price for one unit, and none for any other item or any unknown item.
func m2eCompare(items []sendapi.Item, settle []money.SettleItem, ledger []m2eCharge, prices map[string]int64, decide func(charge.Outcome) m2eDecision) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	byID := map[string]sendapi.Item{}
	for _, it := range items {
		if it.JobItemID == "" {
			say("a reported item has no job item id")
			continue
		}
		if _, dup := byID[it.JobItemID]; dup {
			say("item %s is reported twice", it.JobItemID)
			continue
		}
		byID[it.JobItemID] = it
	}
	hand := map[string]m2eDecision{}
	for id, it := range byID {
		o := charge.Outcome{Module: "m2", State: it.State, Reason: m2eReason(it.Reason)}
		h := m2eHand(it.State)
		hand[id] = h
		if d := decide(o); d != h {
			say("item %s (%s/%q): charge.Decide says %s %q, the hand oracle says %s %q", id, it.State, o.Reason, d.Kind, d.Unit, h.Kind, h.Unit)
		}
	}
	inSettle := map[string]int{}
	for _, s := range settle {
		inSettle[s.ItemRef]++
		it, ok := byID[s.ItemRef]
		switch {
		case !ok:
			say("the settle list carries %s, which the module never reported", s.ItemRef)
		case s.Outcome.Module != "m2" || s.Outcome.State != it.State || s.Outcome.Reason != m2eReason(it.Reason):
			say("the settle list carries outcome %s/%s/%q for %s, the module reported %s/%q", s.Outcome.Module, s.Outcome.State, s.Outcome.Reason, s.ItemRef, it.State, m2eReason(it.Reason))
		}
	}
	for ref, n := range inSettle {
		if n > 1 {
			say("the settle list repeats %s (%d times)", ref, n)
		}
	}
	for id, h := range hand {
		switch h.Kind {
		case "charge", "free":
			if inSettle[id] == 0 {
				say("the settle list lacks the terminal item %s (%s)", id, h.Kind)
			}
		default:
			if inSettle[id] > 0 {
				say("the settle list carries %s, which is %s", id, h.Kind)
			}
		}
	}
	rows := map[string][]m2eCharge{}
	for _, c := range ledger {
		rows[c.ItemRef] = append(rows[c.ItemRef], c)
	}
	for ref := range rows {
		if _, ok := byID[ref]; !ok {
			say("the ledger holds a charge for %s, which the module never reported", ref)
		}
	}
	for id, h := range hand {
		got := rows[id]
		if h.Kind != "charge" {
			if len(got) != 0 {
				say("item %s is %s but the ledger holds %d charge(s) for it", id, h.Kind, len(got))
			}
			continue
		}
		if len(got) != 1 {
			say("item %s must be charged exactly once, the ledger holds %d charge(s) for it", id, len(got))
			continue
		}
		c := got[0]
		if c.Unit != h.Unit {
			say("item %s is charged as unit %s, the table says %s", id, c.Unit, h.Unit)
		}
		if want, ok := prices[h.Unit]; !ok || c.Cost != want || c.Units != 1 {
			say("item %s is charged %d x %d credit(s), the job's frozen price of %s is %d for one unit", id, c.Units, c.Cost, h.Unit, prices[h.Unit])
		}
	}
	sort.Strings(p)
	return p
}

// m2eExpect is the (state, reason, error class) an item of a kind must be reported with ("<null>" for a JSON null).
type m2eExpect struct{ State, Reason, ErrorClass string }

var m2eWantByKind = map[string]m2eExpect{
	"ok":     {"sent", m2wNull, m2wNull},
	"bounce": {"bounced", "bounce_permanent", "bounce_permanent"},
	"defer":  {"failed", "attempts_exhausted", "transport_4xx"},
}

// m2eKinds compares a campaign report with what the scenario submitted: every submitted item exactly once, in the order it was run, each with the state, reason and error class of its kind.
func m2eKinds(tn *m2wTenant, items []sendapi.Item) []string {
	var got, want []string
	for _, it := range items {
		got = append(got, it.JobItemID)
	}
	for _, it := range tn.Items {
		want = append(want, it.ID)
	}
	p := sendapitest.ExactSet(got, want)
	byID := map[string]sendapi.Item{}
	for _, it := range items {
		byID[it.JobItemID] = it
	}
	for _, it := range tn.Items {
		rep, ok := byID[it.ID]
		if !ok {
			continue
		}
		w := m2eWantByKind[it.Kind]
		if rep.State != w.State || m2eText(rep.Reason) != w.Reason || m2eText(rep.ErrorClass) != w.ErrorClass {
			p = append(p, fmt.Sprintf("item %02d (%s, %s) is reported %s / %s / %s, want %s / %s / %s", it.Index, it.Kind, it.Email, rep.State, m2eText(rep.Reason), m2eText(rep.ErrorClass), w.State, w.Reason, w.ErrorClass))
		}
	}
	sort.Strings(p)
	return p
}

// m2eRowKey is the identity of an item in the pagination comparisons: its id, state and reason.
func m2eRowKey(it sendapi.Item) string {
	return it.JobItemID + "|" + it.State + "|" + m2eText(it.Reason)
}

func m2eKeys(items []sendapi.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = m2eRowKey(it)
	}
	return out
}

// ---- polling through the adapter ----

func m2eAllTerminal(rep sendapi.CampaignReport, n int) bool {
	if rep.Totals.Total != int64(n) || !rep.ItemsMatchTotal() || !rep.Progress.IsComplete || len(rep.Items) != n {
		return false
	}
	for _, it := range rep.Items {
		if it.Class != sendapi.ClassTerminal {
			return false
		}
	}
	return true
}

func m2eTotalsLine(tt sendapi.Totals) string {
	return fmt.Sprintf("total=%d sent=%d bounced=%d failed=%d cancelled=%d in_flight=%d pending=%d waiting_for_capacity=%d", tt.Total, tt.Sent, tt.Bounced, tt.Failed, tt.Cancelled, tt.InFlight, tt.Pending, tt.WaitingForCapacity)
}

// waitReport polls the whole campaign report THROUGH THE ADAPTER (every page, in order) every second until module 2 says the campaign is complete AND every item the test submitted is terminal
// (the platform's own submitted count is the completeness, never is_complete alone), or fails at the deadline with the last report and the diagnostics. onPoll, when not nil, runs after every poll.
func (e *m2eEnv) waitReport(t *testing.T, ad *m2.Client, tn *m2wTenant, limit time.Duration, onPoll func(sendapi.CampaignReport)) sendapi.CampaignReport {
	t.Helper()
	start := time.Now()
	last := ""
	for {
		rep, err := ad.ReportCampaign(e.r.ctx, tn.CampaignID)
		if err != nil {
			t.Fatalf("ReportCampaign of %s through the adapter failed (%s): %s", tn.Name, modclient.KindOf(err), e.r.scrub(err.Error()))
		}
		if line := m2eTotalsLine(rep.Totals); line != last {
			last = line
			t.Logf("NOTE e2e t+%5.1fs %s: the adapter's campaign report (%d page(s), %d item(s)): %s", time.Since(start).Seconds(), tn.Name, rep.Pages, len(rep.Items), line)
		}
		if onPoll != nil {
			onPoll(rep)
		}
		if m2eAllTerminal(rep, len(tn.Items)) {
			return rep
		}
		if time.Since(start) > limit {
			t.Fatalf("the campaign of %s did not finish within %s through the adapter (%d item(s) submitted): %s, is_complete %v, items match total %v\n%s", tn.Name, limit, len(tn.Items), m2eTotalsLine(rep.Totals), rep.Progress.IsComplete, rep.ItemsMatchTotal(), e.r.diagnostics(t, tn))
		}
		select {
		case <-e.r.ctx.Done():
			t.Fatalf("the test's context ended while waiting for %s: %v", tn.Name, e.r.ctx.Err())
		case <-time.After(m2wPollEvery):
		}
	}
}

// runAll hands every item of the tenant to module 2 THROUGH THE ADAPTER: each must be a NEW job (202, state queued, class pending) with a job id of its own.
func (e *m2eEnv) runAll(t *testing.T, ad *m2.Client, tn *m2wTenant) time.Duration {
	t.Helper()
	start := time.Now()
	seen := map[string]string{}
	for _, it := range tn.Items {
		res, err := ad.Run(e.r.ctx, it.ID)
		if err != nil {
			t.Fatalf("Run of %s item %02d through the adapter failed (%s): %s", tn.Name, it.Index, modclient.KindOf(err), e.r.scrub(err.Error()))
		}
		if !res.Created || res.State != m2wStateQueued || res.Class != sendapi.ClassPending || res.JobID == "" {
			t.Fatalf("Run of %s item %02d answered %+v, want a new job (Created) in state queued of class pending with a job id", tn.Name, it.Index, res)
		}
		if prev, dup := seen[res.JobID]; dup {
			t.Fatalf("Run answered the job id %s for two items (%s and %s)", res.JobID, prev, it.ID)
		}
		seen[res.JobID] = it.ID
		it.JobID = res.JobID
	}
	return time.Since(start)
}

// problems fails the test once per problem sentence (t.Errorf: one run shows every divergence).
func m2eProblems(t *testing.T, what string, p []string) {
	t.Helper()
	for _, s := range p {
		t.Errorf("%s: %s", what, s)
	}
}

// m2eSinkWantFor is what the sink must have seen of an item module 2 reports in a state, whatever the scenario: a sent item has one accepted message, a bounced item one rejected RCPT and
// no message, an item that failed by exhaustion as many deferrals as the module's own cap, and an item that was cancelled before it started no SMTP session at all.
func m2eSinkWantFor(state string, capN int) (m2wSinkWant, bool) {
	switch state {
	case "sent":
		return m2wSinkWant{Messages: 1, Accepted: 1}, true
	case "bounced":
		return m2wSinkWant{Rejected: 1}, true
	case "failed":
		return m2wSinkWant{Deferred: capN}, true
	case "cancelled":
		return m2wSinkWant{}, true
	}
	return m2wSinkWant{}, false
}

// sinkAgainstReport asserts, item by item, that what the sink saw is what module 2's REPORT (through the adapter) says happened.
func (e *m2eEnv) sinkAgainstReport(t *testing.T, label string, tn *m2wTenant, items []sendapi.Item, view m2wView, capN int) {
	t.Helper()
	byID := map[string]sendapi.Item{}
	for _, it := range items {
		byID[it.JobItemID] = it
	}
	e.r.checkSink(t, label, tn, view, func(it *m2wItem) m2wSinkWant {
		rep, ok := byID[it.ID]
		if !ok {
			t.Errorf("%s: %s item %02d is not in the report", label, tn.Name, it.Index)
			return m2wSinkWant{}
		}
		w, ok := m2eSinkWantFor(rep.State, capN)
		if !ok {
			t.Errorf("%s: %s item %02d is reported in state %q, which has no sink expectation", label, tn.Name, it.Index, rep.State)
		}
		return w
	})
}

// ---- cross-tenant answers ----

// m2eNotFound is "" when err is the module's not-found answer about the named resource field, else a sentence saying what it is.
func m2eNotFound(err error, field string) string {
	if err == nil {
		return "the call succeeded, want the module's not-found answer"
	}
	if k := modclient.KindOf(err); k != modclient.NotFound {
		return fmt.Sprintf("the call failed as %s, want not found", k)
	}
	if f, ok := sendapi.NotFoundField(err); !ok || f != field {
		return fmt.Sprintf("the not-found answer names the field %q (named: %v), want %q", f, ok, field)
	}
	return ""
}

// m2eLeaks lists the names of the secrets (an id, an address) that appear in text: the test of "nothing of tenant A is in tenant B's answer".
func m2eLeaks(text string, secrets map[string]string) []string {
	var found []string
	for name, v := range secrets {
		if v != "" && strings.Contains(text, v) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// m2eIDs are the ids and addresses of a tenant's campaign that must never appear in another tenant's answer: its job item ids, its job ids and its recipients' addresses.
func m2eIDs(tn *m2wTenant) map[string]string {
	out := map[string]string{}
	for _, it := range tn.Items {
		k := strconv.Itoa(it.Index)
		out["item id "+k] = it.ID
		out["job id "+k] = it.JobID
		out["address "+k] = it.Email
	}
	return out
}

// ---- pagination controls ----

// m2eLookAheadWalk is the look-ahead bug of a keyset pager, over the adapter's page reader: the walker believes the module read one row more than the page size (the look-ahead row that tells
// whether another page exists) and that this row is the LAST item of the page, so it drops it, expecting the next page to start from it. Module 2 already trimmed the page (its cursor is the
// last RETURNED row: the fix of its M2-G5), so every dropped item is lost. The equality check must catch it.
func m2eLookAheadWalk(ctx context.Context, c *sendapi.Client, campaign string) ([]sendapi.Item, error) {
	var out []sendapi.Item
	cursor := ""
	for {
		page, err := c.ReportCampaignPage(ctx, campaign, cursor)
		if err != nil {
			return nil, err
		}
		items := page.Items
		if page.NextCursor != "" && len(items) > 0 {
			items = items[:len(items)-1] // the bug
		}
		out = append(out, items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// m2eSkipAheadWalk is the other look-ahead bug: to learn whether there is a page after the next one the walker reads the next page, keeps ITS cursor and throws its items away.
func m2eSkipAheadWalk(ctx context.Context, c *sendapi.Client, campaign string) ([]sendapi.Item, error) {
	var out []sendapi.Item
	cursor := ""
	for {
		page, err := c.ReportCampaignPage(ctx, campaign, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		ahead, err := c.ReportCampaignPage(ctx, campaign, page.NextCursor) // the look-ahead read
		if err != nil {
			return nil, err
		}
		if ahead.NextCursor == "" {
			return out, nil // the page read ahead is the last one, and it is dropped
		}
		cursor = ahead.NextCursor // the bug: the page read ahead is never delivered
	}
}
