//go:build harness && m1node

package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/norm"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The helpers of the module-1 end-to-end tests (m1_node_e2e_test.go), behind the build tag m1node like the rest of the module-1 node tests. Every name starts with m1e so that it cannot
// clash with another file of this package; the file leans on m1_node_helpers_test.go (the fixture's control API, the node's addresses, the recipe wants), on m1_bulk_settle_test.go (the
// ledger reader m1bLedger) and on the money and tenant helpers.
//
// WHAT IS DIFFERENT FROM THE OTHER MODULE-1 NODE TESTS. They talk to module 1 as the one caller key run.sh seeded, through hand-written HTTP, to prove what the NODE does. These talk to it the
// way the platform will: through internal/adapters/m1, built per tenant by Factory.ForScope from a tenant.Scope, with the key the platform derives for a real user and provisions with
// tenant.ProvisionM1Caller. The money side is the platform's own money core (internal/money) on the LIVE database, as the platform_app login the service uses: this scope has no
// platform-migrate binary, so there is no scratch database to migrate (migratedScratch), and every identity below is a fresh random uuid so that nothing collides with another run's rows.

const (
	m1eGrantA    = 500 // credits granted to tenant A before its job: far above the dearest reserve of any job below
	m1eGrantB    = 100
	m1ePageLimit = 7 // the page size of the settling drain: a job of two dozen rows takes four pages
	m1eJobWait   = 4 * time.Minute

	m1eKindRecipe    = "recipe"
	m1eKindCacheHit  = "cache_hit"
	m1eKindDuplicate = "duplicate"
	m1eKindMalformed = "malformed"
)

var m1eSeq atomic.Int64

// m1eKey is the Idempotency-Key of one submission of these tests: the label, the clock and the run's counter (letters, digits, '.' and '-', well inside module 1's 128), so no two
// submissions of any test share one: module 1 scopes a key to the caller, and a repeat of a key with another request is a 409.
func m1eKey(label string) string {
	return fmt.Sprintf("m1e.%s.%d.%d", label, time.Now().UnixNano(), m1eSeq.Add(1))
}

func m1eShort(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---- the stack: module 1 as the platform sees it, and the platform's money -------------------------------------------------------------------------------------

type m1eStack struct {
	*m1nEnv                  // the fixture's control API, the node's addresses and the context (its own seeded caller is not used by these tests)
	admin   *pgx.Conn        // a superuser connection to the live database: seeding and observing, never the code under test
	pool    *pgxpool.Pool    // real platform_app logins: what the resolver and the money store run on
	res     *tenant.Resolver // over that pool, with a secret generated for this run
	fac     *m1ad.Factory    // one adapter per tenant over one transport
	mny     *mnyEnv          // the money helpers (settle, reserve, balances, the ledger reader), built over admin and pool; there is no scratch database here
	adminID string           // the acting admin of every grant
	secret  config.Secret    // PLATFORM_M1_KEY_SECRET of this run: never printed
}

func newM1eStack(t *testing.T) *m1eStack {
	t.Helper()
	return newM1eStackWith(t, config.Secret(randomHex(t, 32)), modclient.Options{Timeout: 30 * time.Second, Observe: m1Drift.Observer()})
}

// newM1eStackWith is the stack with a given key secret and the dials of the adapters it makes. The outage tests (m1_node_outage_test.go) run in three separate processes and rebuild the
// SAME stack in each: the secret of the first phase is kept in the database and passed back here, so that the second and the third derive the first one's tenant key.
func newM1eStackWith(t *testing.T, secret config.Secret, opts modclient.Options) *m1eStack {
	t.Helper()
	env := newM1nEnv(t)
	adminURL := requireEnv(t, envAdminURL)
	s := &m1eStack{m1nEnv: env}
	s.admin = dial(t, adminURL, nil)
	s.pool = mnyPool(t, adminURL) // proves it is a real login as platform_app and not a superuser
	s.secret = secret
	res, err := tenant.NewResolver(s.pool, s.secret)
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	s.res = res
	fac, err := m1ad.NewFactory(env.base, opts, nil)
	if err != nil {
		t.Fatalf("m1.NewFactory: %v", err) // never carries the URL
	}
	s.fac = fac
	s.mny = &mnyEnv{t: t, admin: s.admin, pool: s.pool, store: money.New(s.pool)}
	s.adminID = s.platformAdmin()
	return s
}

// platformAdmin is the acting admin of every grant: the first admin run.sh created (stage 4b), or, when there is none, one seeded the way the money tests seed theirs.
func (s *m1eStack) platformAdmin() string {
	t := s.t
	t.Helper()
	var id string
	err := s.admin.QueryRow(s.ctx, `SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at, id LIMIT 1`).Scan(&id)
	if err == nil {
		return id
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading platform.admins: %v", err)
	}
	if err := s.admin.QueryRow(s.ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	email := "m1e-admin-" + id + "@example.test"
	tenSeedLogin(t, s.admin, id, email)
	tenSeedUser(t, s.admin, id, email, true)
	if _, err := s.admin.Exec(s.ctx, `INSERT INTO platform.admins (id, display_name) VALUES ($1, 'Module 1 end-to-end admin')`, id); err != nil {
		t.Fatalf("seeding an admin: %v", err)
	}
	return id
}

// m1eTenant is one real tenant: a login in auth.users, its platform user, the Scope the resolver builds for it and the adapter built from that Scope.
type m1eTenant struct {
	name   string
	userID string
	scope  tenant.Scope
	ad     *m1ad.Adapter
}

// newTenant makes a real tenant. provision says whether its derived key is written to module 1's verify.api_callers with tenant.ProvisionM1Caller; a tenant that is not provisioned is the
// control of "a key module 1 has never heard of".
func (s *m1eStack) newTenant(name string, provision bool) *m1eTenant {
	t := s.t
	t.Helper()
	var id string
	if err := s.admin.QueryRow(s.ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	email := "m1e-" + name + "-" + id + "@example.test"
	tenSeedLogin(t, s.admin, id, email)
	tenSeedUser(t, s.admin, id, email, true)
	tn, err := s.res.ByAuthUser(s.ctx, id)
	if err != nil {
		t.Fatalf("ByAuthUser for tenant %s: %v", name, err)
	}
	sc, err := s.res.Scope(s.ctx, tn)
	if err != nil {
		t.Fatalf("Scope for tenant %s: %v", name, err)
	}
	if provision {
		if err := s.res.ProvisionM1Caller(s.ctx, s.admin, id); err != nil {
			t.Fatalf("ProvisionM1Caller for tenant %s: %v", name, err)
		}
	}
	out := &m1eTenant{name: name, userID: id, scope: sc}
	out.ad = s.adapterFor(out, m1ePageLimit)
	return out
}

// existingTenant is the tenant of a user that an earlier process made and provisioned: the resolver builds its Scope again from the database and the secret, nothing is seeded.
func (s *m1eStack) existingTenant(name, userID string) *m1eTenant {
	t := s.t
	t.Helper()
	tn, err := s.res.ByAuthUser(s.ctx, userID)
	if err != nil {
		t.Fatalf("ByAuthUser for tenant %s: %v", name, err)
	}
	sc, err := s.res.Scope(s.ctx, tn)
	if err != nil {
		t.Fatalf("Scope for tenant %s: %v", name, err)
	}
	out := &m1eTenant{name: name, userID: userID, scope: sc}
	out.ad = s.adapterFor(out, m1ePageLimit)
	return out
}

// adapterFor is a fresh adapter for the tenant with the given page size (PageLimit and Poll are set once, before the first call, as the adapter documents).
func (s *m1eStack) adapterFor(tn *m1eTenant, pageLimit int) *m1ad.Adapter {
	s.t.Helper()
	ad, err := s.fac.ForScope(tn.scope)
	if err != nil {
		s.t.Fatalf("Factory.ForScope for tenant %s: %v", tn.name, err) // never carries the key
	}
	ad.PageLimit, ad.Poll = pageLimit, m1nPollEvery
	return ad
}

// key is the tenant's bearer key, for the leak checks and nothing else.
func (tn *m1eTenant) key() string { return tn.scope.M1Key.Reveal() }

// ---- what the module and the platform's database hold -------------------------------------------------------------------------------------------------------

// moduleJob reads, as the superuser, module 1's own tables for one job: how many input rows it recorded, how many of them it judged malformed, how many it marked as a repeat of an
// earlier row, and who owns it.
func (s *m1eStack) moduleJob(jobID string) (rows, malformed, repeats int, owner string) {
	s.t.Helper()
	err := s.admin.QueryRow(s.ctx, `
		SELECT count(*), count(*) FILTER (WHERE malformed), count(*) FILTER (WHERE duplicate_of_ordinal IS NOT NULL),
		       COALESCE((SELECT owner_user_id::text FROM verify.jobs WHERE id = $1::uuid), '')
		FROM verify.job_rows WHERE job_id = $1::uuid`, jobID).Scan(&rows, &malformed, &repeats, &owner)
	if err != nil {
		s.t.Fatalf("reading verify.job_rows of job %s: %v", jobID, err)
	}
	return rows, malformed, repeats, owner
}

func (s *m1eStack) moduleJobCount() int {
	s.t.Helper()
	var n int
	if err := s.admin.QueryRow(s.ctx, `SELECT count(*) FROM verify.jobs`).Scan(&n); err != nil {
		s.t.Fatalf("counting verify.jobs: %v", err)
	}
	return n
}

func (s *m1eStack) cacheRows(domain string) int {
	s.t.Helper()
	var n int
	if err := s.admin.QueryRow(s.ctx, `SELECT count(*) FROM verify.cache WHERE domain = $1`, domain).Scan(&n); err != nil {
		s.t.Fatalf("counting verify.cache rows of %s: %v", domain, err)
	}
	return n
}

// waitCacheRow waits until module 1 has written one more cache row for the domain than it held before: the drainer writes the cache AFTER it settles the work item, so a job can read
// complete a moment before its verdict is cached, and a cache-hit row submitted in that moment would be verified for real.
func (s *m1eStack) waitCacheRow(domain string, before int) {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if n := s.cacheRows(domain); n > before {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("module 1 wrote no cache row for %s within 20 s of the warm-up job completing (it held %d before): the verdict of the warm-up address is not cacheable, so the cache-hit row of the next job cannot hit", domain, before)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// newJob inserts a draft platform job of the user on module 1 (no price snapshot: the store freezes it at reserve) and returns its id.
func (s *m1eStack) newJob(user string) string {
	s.t.Helper()
	var id string
	if err := s.admin.QueryRow(s.ctx, `INSERT INTO platform.jobs (id, user_id, module_id) VALUES (gen_random_uuid(), $1, 'm1') RETURNING id::text`, user).Scan(&id); err != nil {
		s.t.Fatalf("inserting a platform job: %v", err)
	}
	return id
}

// ledgerSums are the sums of the user's ledger on module 1, which is what the balances must equal.
func (s *m1eStack) ledgerSums(user string) (available, reserved int64) {
	s.t.Helper()
	if err := s.admin.QueryRow(s.ctx, `SELECT COALESCE(sum(available_delta), 0)::bigint, COALESCE(sum(reserved_delta), 0)::bigint FROM platform.ledger WHERE user_id = $1 AND module_id = 'm1'`, user).Scan(&available, &reserved); err != nil {
		s.t.Fatalf("summing the ledger of %s: %v", user, err)
	}
	return available, reserved
}

// viewProblems are the rows of the two invariant views, as problems: platform.balance_drift (a stored balance that differs from its ledger) and platform.reserve_overdrawn.
func (s *m1eStack) viewProblems(when string) []string {
	var p []string
	if d := s.mny.rows(`SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`); len(d) != 0 {
		p = append(p, fmt.Sprintf("%s: platform.balance_drift is not empty (user|module|stored available|ledger available|stored reserved|ledger reserved): %v", when, d))
	}
	if o := s.mny.rows(`SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`); len(o) != 0 {
		p = append(p, fmt.Sprintf("%s: platform.reserve_overdrawn is not empty (reserve|user|module|job|reserved|settled): %v", when, o))
	}
	return p
}

// ---- driving a job through the adapter ------------------------------------------------------------------------------------------------------------------------

// waitDone polls the job's Status (the adapter's, as the tenant) until module 1 says it is complete and returns that status, the number of polls and how long it took.
func (s *m1eStack) waitDone(tn *m1eTenant, jobID string, within time.Duration) (m1ad.JobStatus, int, time.Duration) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, within)
	defer cancel()
	start := time.Now()
	polls := 0
	for {
		js, err := tn.ad.Status(ctx, jobID)
		polls++
		if err != nil {
			s.t.Fatalf("Status of job %s as tenant %s failed after %d poll(s): %v", jobID, tn.name, polls, err)
		}
		if js.Complete {
			return js, polls, time.Since(start)
		}
		select {
		case <-ctx.Done():
			s.t.Fatalf("job %s was not complete after %v: module 1 says status %q, total %d, settled %d, pending %d, processing_paused %t", jobID, within, js.Status, js.Counts.Total, js.Counts.Settled, js.Counts.Pending, js.ProcessingPaused)
		case <-time.After(m1nPollEvery):
		}
	}
}

// waitComplete is waitDone that also requires the counts of a finished job of rows rows: total = settled = rows, pending 0.
func (s *m1eStack) waitComplete(tn *m1eTenant, jobID string, rows int, within time.Duration) (m1ad.JobStatus, int, time.Duration) {
	s.t.Helper()
	js, polls, took := s.waitDone(tn, jobID, within)
	if js.Counts.Total != rows || js.Counts.Settled != rows || js.Counts.Pending != 0 {
		s.t.Fatalf("job %s is complete but its counts are %+v for %d row(s)", jobID, js.Counts, rows)
	}
	return js, polls, took
}

// drainAll reads every page of a job as the tenant, with the tenant's adapter, and returns the rows in delivery order.
func (s *m1eStack) drainAll(ad *m1ad.Adapter, jobID string) ([]m1ad.Row, m1ad.DrainStats) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	var rows []m1ad.Row
	st, err := ad.Drain(ctx, jobID, func(page []m1ad.Row) error { rows = append(rows, page...); return nil })
	if err != nil {
		s.t.Fatalf("Drain of job %s failed: %v", jobID, err)
	}
	return rows, st
}

// m1eRun is everything one job leaves behind when it is driven end to end: planned, funded, reserved, submitted, waited for, drained page by page, settled page by page, with the malformed
// rows settled free.
type m1eRun struct {
	tn       *m1eTenant
	items    []m1eItem
	plan     m1ad.Plan
	grant    int64
	job      string // the platform's job id
	rsv      money.ReserveResult
	jobID    string // module 1's job id
	counts   m1ad.SubmitCounts
	status   m1ad.JobStatus
	polls    int
	waited   time.Duration
	rows     []m1ad.Row // the delivered rows, in order
	list     []money.SettleItem
	results  []money.SettleResult // one per settle call of a page
	pages    [][]money.SettleItem // what each of those calls was given, for a replay
	malRes   money.SettleResult   // the settle call of the malformed rows
	stats    m1ad.DrainStats
	ledger   []m1bCharge // the job's charge rows, read by assess
	fxBase   int
	sessions []m1nSession
}

// settleThrough runs one tenant's job from grant to the last settle call and stops BEFORE the release, so that the caller can look at the books while the reserve is still held.
func (s *m1eStack) settleThrough(tn *m1eTenant, items []m1eItem, grant int64) *m1eRun {
	t := s.t
	t.Helper()
	r := &m1eRun{tn: tn, items: items, grant: grant}
	inputs := make([]m1ad.Input, len(items))
	for i, it := range items {
		inputs[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	r.plan = m1ad.Prepare(inputs)
	if err := r.plan.Err(); err != nil {
		t.Fatalf("Prepare refused the batch: %v", err)
	}
	ma := s.mny
	if _, err := ma.store.Grant(s.ctx, money.GrantReq{UserID: tn.userID, Module: "m1", Amount: grant, AdminID: s.adminID, Reason: "module 1 end-to-end test", Key: fmt.Sprintf("m1e-grant-%s-%d", tn.userID, m1eSeq.Add(1))}); err != nil {
		t.Fatalf("granting credits to tenant %s: %v", tn.name, err)
	}
	r.job = s.newJob(tn.userID)
	r.rsv = ma.reserve(r.job, len(items))
	ma.setStatus(r.job, "running")

	r.fxBase = s.fx.seq()
	var err error
	submitted, err := tn.ad.Submit(s.ctx, r.plan, m1eKey("run-"+tn.name))
	if err != nil {
		t.Fatalf("Submit as tenant %s failed (kind %v): %v", tn.name, modclient.KindOf(err), err)
	}
	r.jobID, r.counts = submitted.JobID, submitted.Counts
	r.status, r.polls, r.waited = s.waitComplete(tn, r.jobID, len(r.plan.Items), m1eJobWait)
	s.drainAndSettle(r)
	return r
}

// drainAndSettle reads every page of the run's COMPLETE job with the tenant's adapter and settles each page through the money core as it arrives, then settles the malformed rows free (they
// were never sent), and reads the fixture's SMTP sessions of the window. The outage test's third phase calls it on a run it rebuilt from the database.
func (s *m1eStack) drainAndSettle(r *m1eRun) {
	t := s.t
	t.Helper()
	tn, ma := r.tn, s.mny
	dctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	var err error
	r.stats, err = tn.ad.Drain(dctx, r.jobID, func(rows []m1ad.Row) error {
		list, err := m1ad.ToSettleItems(rows)
		if err != nil {
			return err
		}
		res, err := ma.store.Settle(dctx, money.SettleReq{JobID: r.job, Items: list})
		if err != nil {
			return err
		}
		r.rows = append(r.rows, rows...)
		r.list = append(r.list, list...)
		r.results = append(r.results, res)
		r.pages = append(r.pages, list)
		return nil
	})
	if err != nil {
		t.Fatalf("draining and settling job %s as tenant %s failed (kind %v): %v\n%d page(s) settled before the failure", r.jobID, tn.name, modclient.KindOf(err), err, len(r.results))
	}
	if mal := r.plan.MalformedSettleItems(); len(mal) > 0 { // never sent to module 1, free under P-08
		res, err := ma.store.Settle(s.ctx, money.SettleReq{JobID: r.job, Items: mal})
		if err != nil {
			t.Fatalf("settling the malformed rows of tenant %s: %v", tn.name, err)
		}
		r.malRes = res
		r.list = append(r.list, mal...)
	}
	r.sessions = s.fx.sessions(r.fxBase, "")
}

// release ends the platform job and returns the unused remainder of its reserve.
func (s *m1eStack) release(r *m1eRun) money.ReleaseResult {
	s.t.Helper()
	s.mny.setStatus(r.job, "succeeded")
	res, err := s.mny.store.Release(s.ctx, r.job)
	if err != nil {
		s.t.Fatalf("releasing job %s of tenant %s: %v", r.job, r.tn.name, err)
	}
	return res
}

// ---- the rows of a job and what each must be -------------------------------------------------------------------------------------------------------------------

// m1eVerdict is what the adapter's Row says of a verdict: the fields the charge table reads.
type m1eVerdict struct {
	status, reason, origin, skip string
	cache                        bool
}

func (v m1eVerdict) String() string {
	return fmt.Sprintf("%s/%s origin=%s skip=%s cache=%t", v.status, v.reason, orDash(v.origin), orDash(v.skip), v.cache)
}

// m1eVerdictOfWant reduces a row's want (the table of m1_node_helpers_test.go) to what the adapter's Row can show: the signals the adapter drops are asserted by TestM1Node_BulkRecipeTable.
func m1eVerdictOfWant(w m1nWant) m1eVerdict {
	skip, _ := w.sig["smtp_skipped_reason"].(string)
	return m1eVerdict{status: w.status, reason: w.reason, origin: w.origin, skip: skip}
}

func m1eVerdictOfRow(r m1ad.Row) m1eVerdict {
	return m1eVerdict{status: r.Status, reason: r.Reason, origin: r.FailureOrigin, skip: r.SkipReason, cache: r.FromCache}
}

// m1eItem is one input row of a job and EVERYTHING the test knows about it independently of what module 1 reports: its kind, the verdict its address must produce, whether the node is
// reached for it, and what it must cost by the hand-written oracle (unit "" = free).
type m1eItem struct {
	ref      string
	address  string
	kind     string      // m1eKindRecipe, m1eKindCacheHit, m1eKindDuplicate or m1eKindMalformed
	label    string      // the recipe id, or what the row is for
	malform  norm.Reason // a malformed row: the reason norm/v1 must give
	want     m1eVerdict  // a sent row: the verdict module 1 must report (a duplicate: the first row's, M1-F5)
	reaches  bool        // a recipe whose address reaches the node in THIS job: two RCPTs at the fixture
	firstRef string      // a duplicate: the ref of the first row of the address
	unit     string      // the hand-written oracle: "" = free, else the unit charged
	rule     string      // a free row of the platform's own: the charge-table rule that frees it (P-07, P-08)
}

func m1eSent(items []m1eItem) []m1eItem {
	var out []m1eItem
	for _, it := range items {
		if it.kind != m1eKindMalformed {
			out = append(out, it)
		}
	}
	return out
}

func m1eDuplicates(items []m1eItem) int {
	n := 0
	for _, it := range items {
		if it.kind == m1eKindDuplicate {
			n++
		}
	}
	return n
}

// m1eOracle is the charge table of internal/charge/rules.psv written out BY HAND for the rows of these tests, by what each row IS (its recipe: what the fixture is made to do to it), not by
// what module 1 reported and not through charge.Decide. A recipe not named here is "?" and makes every comparison red, so a recipe added to the table must be priced here by a person.
//
//	a repeat of an earlier row (P-07), a malformed row (P-08)                      free
//	answered from the cache (M1-01)                                                charge m1_cache
//	a verdict module 1 reached by proof or by a probe (M1-03 .. M1-14)             charge m1_own
//	an unknown caused by the recipient's side (M1-18, M1-20)                       charge m1_own
//	an unknown caused by us or that cannot be proven either way (M1-21, M1-22)     free
func m1eOracle(kind, label string) string {
	switch kind {
	case m1eKindDuplicate, m1eKindMalformed:
		return ""
	case m1eKindCacheHit:
		return "m1_cache"
	}
	switch label {
	case "valid", "mailbox_full", "no_mx", "no_mx_nodata", "null_mx", "mailbox_not_found", "spam_trap", "catch_all", "disposable", "provider_no_smtp", "non_public_mx":
		return "m1_own"
	case "greylisted", "dns_servfail_remote", "smtp_timeout_remote", "smtp_error_remote":
		return "m1_own" // the recipient's side caused the unknown
	case "dns_silent_own", "dns_refused_undetermined", "smtp_error_undetermined", "connection_failed_undetermined", "smtp_blocked_own":
		return "" // ours, or not provable
	}
	return "?"
}

// m1eRecipe is the recipe of the shared table with this id.
func m1eRecipe(e *m1nEnv, id string) m1nRecipe {
	for _, r := range e.recipes() {
		if r.id == id {
			return r
		}
	}
	panic("m1eRecipe: no recipe " + id)
}

func m1eNewRecipeItem(r m1nRecipe, local string) m1eItem {
	return m1eItem{label: r.id, kind: m1eKindRecipe, address: local + "@" + r.domain, want: m1eVerdictOfWant(r.want), reaches: r.reaches, unit: m1eOracle(m1eKindRecipe, r.id)}
}

func m1eNewMalformed(reason norm.Reason, address string) m1eItem {
	return m1eItem{label: "malformed_" + string(reason), kind: m1eKindMalformed, address: address, malform: reason, rule: "P-08"}
}

func m1eNewDuplicate(first m1eItem, label, address string) m1eItem {
	return m1eItem{label: label, kind: m1eKindDuplicate, address: address, want: first.want, firstRef: first.ref, rule: "P-07"}
}

// m1eAddItem appends the row and gives it its ref (unique within a job by its position) and returns its index.
func m1eAddItem(items *[]m1eItem, it m1eItem) int {
	it.ref = fmt.Sprintf("i%03d-%s", len(*items), it.label)
	*items = append(*items, it)
	return len(*items) - 1
}

// m1eBuildBulkItems is the input of tenant A's job in the B9 test: one address per recipe of the shared table (every recipe family the fixture can produce), a cache hit (the address
// of a warm-up job, already verified and cached), the first recipe's address again, in another spelling, and the greylisted address again, and three malformed addresses, each with a
// different norm/v1 reason, between them. Every local part is fresh, so no recipe address is in the cache from an earlier test.
func m1eBuildBulkItems(e *m1nEnv, warm string) []m1eItem {
	var items []m1eItem
	m1eAddItem(&items, m1eNewMalformed(norm.ReasonNoAt, "not-an-address"))
	idx := map[string]int{}
	for i, r := range e.recipes() {
		idx[r.id] = m1eAddItem(&items, m1eNewRecipeItem(r, fresh("e2e")))
		if i == 9 {
			m1eAddItem(&items, m1eNewDuplicate(items[idx["valid"]], "duplicate_same_valid", items[idx["valid"]].address)) // the same address twice
			m1eAddItem(&items, m1eNewMalformed(norm.ReasonMultipleAt, "first@second@valid.fixture.test"))
		}
	}
	m1eAddItem(&items, m1eNewDuplicate(items[idx["valid"]], "duplicate_spelling_valid", strings.ToUpper(items[idx["valid"]].address))) // another spelling that normalises to it
	m1eAddItem(&items, m1eItem{label: "cache_hit", kind: m1eKindCacheHit, address: warm, want: m1eVerdict{status: "valid", reason: "accepted", cache: true}, unit: m1eOracle(m1eKindCacheHit, "cache_hit")})
	m1eAddItem(&items, m1eNewDuplicate(items[idx["greylisted"]], "duplicate_same_greylisted", items[idx["greylisted"]].address))
	m1eAddItem(&items, m1eNewMalformed(norm.ReasonEmptyLocal, "@valid.fixture.test"))
	return items
}

// m1eBuildSmallItems is tenant B's job: two cheap addresses that never reach the node and one malformed one.
func m1eBuildSmallItems(e *m1nEnv) []m1eItem {
	var items []m1eItem
	m1eAddItem(&items, m1eNewRecipeItem(m1eRecipe(e, "no_mx"), fresh("e2eb")))
	m1eAddItem(&items, m1eNewMalformed(norm.ReasonNoAt, "not-an-address-b"))
	m1eAddItem(&items, m1eNewRecipeItem(m1eRecipe(e, "spam_trap"), fresh("e2eb")))
	return items
}

// ---- the comparison of a settlement with what the rows must cost -----------------------------------------------------------------------------------------------------

// m1eDecisionProblem is how a decision differs from the item's hand-written expectation, or "".
func m1eDecisionProblem(it m1eItem, d charge.Decision) string {
	switch {
	case it.unit == "?":
		return "the oracle has no price for this recipe"
	case it.unit == "" && d.Kind != charge.Free:
		return fmt.Sprintf("the settle list makes it %s %s (rule %s), want free", d.Kind, d.Unit, d.Rule)
	case it.unit == "" && it.rule != "" && d.Rule != it.rule:
		return fmt.Sprintf("the settle list frees it by rule %s, want %s", d.Rule, it.rule)
	case it.unit != "" && (d.Kind != charge.Charge || d.Unit != it.unit):
		return fmt.Sprintf("the settle list makes it %s %s (rule %s), want charge %s", d.Kind, d.Unit, d.Rule, it.unit)
	}
	return ""
}

// m1eCompareSettleList compares what was handed to the money core with the rows: every row exactly once, and each decided (by charge.Decide) as the hand-written oracle says.
func m1eCompareSettleList(items []m1eItem, list []money.SettleItem) []string {
	var p []string
	byRef := map[string][]money.SettleItem{}
	for _, s := range list {
		byRef[s.ItemRef] = append(byRef[s.ItemRef], s)
	}
	known := map[string]bool{}
	for _, it := range items {
		known[it.ref] = true
		got := byRef[it.ref]
		switch {
		case len(got) == 0:
			p = append(p, fmt.Sprintf("item %q (%s) is missing from the settle list", it.ref, it.label))
		case len(got) > 1:
			p = append(p, fmt.Sprintf("item %q (%s) appears %d times in the settle list: a charge is one unit of one item", it.ref, it.label, len(got)))
		default:
			if why := m1eDecisionProblem(it, charge.Decide(got[0].Outcome)); why != "" {
				p = append(p, fmt.Sprintf("item %q (%s): %s", it.ref, it.label, why))
			}
		}
	}
	for ref := range byRef {
		if !known[ref] {
			p = append(p, fmt.Sprintf("the settle list names %q, which is no input row", ref))
		}
	}
	sort.Strings(p)
	return p
}

// m1eCompareLedger compares the ledger's charge rows of the job with the rows: a charged item has exactly one row under its own ref, of the oracle's unit, at the frozen price, for one
// unit; a free item has none; and no row is keyed by anything that is not a row's ref.
func m1eCompareLedger(items []m1eItem, ledger []m1bCharge, prices map[string]int) []string {
	var p []string
	byRef := map[string][]m1bCharge{}
	for _, c := range ledger {
		byRef[c.ItemRef] = append(byRef[c.ItemRef], c)
	}
	known := map[string]bool{}
	for _, it := range items {
		known[it.ref] = true
		got := byRef[it.ref]
		if it.unit == "" {
			if len(got) != 0 {
				p = append(p, fmt.Sprintf("item %q (%s) must be free but the ledger charged it %d time(s): %v", it.ref, it.label, len(got), got))
			}
			continue
		}
		if len(got) != 1 {
			p = append(p, fmt.Sprintf("item %q (%s) must be charged %s once but the ledger holds %d charge row(s) for it", it.ref, it.label, it.unit, len(got)))
			continue
		}
		c := got[0]
		if c.Unit != it.unit {
			p = append(p, fmt.Sprintf("item %q (%s): charged as %s, the oracle says %s", it.ref, it.label, c.Unit, it.unit))
		}
		if want, ok := prices[it.unit]; !ok || c.Cost != want {
			p = append(p, fmt.Sprintf("item %q (%s): charged %d credit(s) per unit, the frozen price of %s is %d", it.ref, it.label, c.Cost, it.unit, want))
		}
		if c.Units != 1 {
			p = append(p, fmt.Sprintf("item %q (%s): the charge row is for %d unit(s), a charge is one item", it.ref, it.label, c.Units))
		}
	}
	for ref, cs := range byRef {
		if !known[ref] {
			p = append(p, fmt.Sprintf("the ledger holds %d charge row(s) under %q, which is no input row", len(cs), ref))
		}
	}
	sort.Strings(p)
	return p
}

// m1eGroupProblems: every address group (the first row and its repeats) is charged exactly once if its first row is a charge, and not at all if it is free.
func m1eGroupProblems(items []m1eItem, ledger []m1bCharge) []string {
	var p []string
	groupOf := map[string]string{}
	first := map[string]m1eItem{}
	for _, it := range items {
		if it.kind == m1eKindMalformed {
			continue
		}
		g := it.ref
		if it.kind == m1eKindDuplicate {
			g = it.firstRef
		} else {
			first[it.ref] = it
		}
		groupOf[it.ref] = g
	}
	charges := map[string]int{}
	for _, c := range ledger {
		if g, ok := groupOf[c.ItemRef]; ok {
			charges[g]++
		}
	}
	for g, it := range first {
		want := 0
		if it.unit != "" {
			want = 1
		}
		if charges[g] != want {
			p = append(p, fmt.Sprintf("the address group of %q (%s) was charged %d time(s) across all its rows, want %d", it.ref, it.label, charges[g], want))
		}
	}
	sort.Strings(p)
	return p
}

type m1eTotals struct {
	charged, free, other int
	credits              int64
	byUnit               string
}

// m1eDecideTotals is what charge.Decide makes of the outcomes the adapter reported, at the frozen prices.
func m1eDecideTotals(outcomes []charge.Outcome, prices map[string]int) m1eTotals {
	var t m1eTotals
	units := map[string]int{}
	for _, o := range outcomes {
		switch d := charge.Decide(o); d.Kind {
		case charge.Charge:
			t.charged++
			t.credits += int64(prices[d.Unit])
			units[d.Unit]++
		case charge.Free:
			t.free++
		default:
			t.other++
		}
	}
	t.byUnit = fmt.Sprint(units)
	return t
}

// m1eOracleTotals is what the hand-written oracle makes of the rows, at the same prices.
func m1eOracleTotals(items []m1eItem, prices map[string]int) m1eTotals {
	var t m1eTotals
	units := map[string]int{}
	for _, it := range items {
		if it.unit == "" {
			t.free++
			continue
		}
		t.charged++
		t.credits += int64(prices[it.unit])
		units[it.unit]++
	}
	t.byUnit = fmt.Sprint(units)
	return t
}

// m1eCorruption is a settle list that has been broken in one named way.
type m1eCorruption struct {
	name string
	ref  string // the item the comparison must name
	want string // a fragment the problem about it must contain
	list []money.SettleItem
}

func m1eIndexOf(list []money.SettleItem, ref string) int {
	for i, s := range list {
		if s.ItemRef == ref {
			return i
		}
	}
	return -1
}

// m1eCorruptLists breaks the settle list in four ways: a charged item's outcome changed to a free one, a charged item dropped, a free item duplicated, and a free item's outcome changed to
// a charged one. m1eCompareSettleList must report each, naming the item.
func m1eCorruptLists(items []m1eItem, list []money.SettleItem) []m1eCorruption {
	var charged, free *m1eItem
	var lastCharged *m1eItem
	for i := range items {
		it := &items[i]
		switch {
		case it.unit != "" && charged == nil:
			charged = it
		case it.kind == m1eKindDuplicate && free == nil:
			free = it
		}
		if it.unit != "" {
			lastCharged = it
		}
	}
	cp := func() []money.SettleItem { return append([]money.SettleItem(nil), list...) }
	var out []m1eCorruption

	changed := cp()
	changed[m1eIndexOf(changed, charged.ref)].Outcome = charge.Outcome{Module: "platform", State: "duplicate"}
	out = append(out, m1eCorruption{"one outcome changed (a charged item made free)", charged.ref, "the settle list makes it free", changed})

	dropped := cp()
	i := m1eIndexOf(dropped, lastCharged.ref)
	dropped = append(dropped[:i], dropped[i+1:]...)
	out = append(out, m1eCorruption{"one item dropped", lastCharged.ref, "is missing from the settle list", dropped})

	duplicated := append(cp(), list[m1eIndexOf(list, free.ref)])
	out = append(out, m1eCorruption{"one item duplicated", free.ref, "appears 2 times", duplicated})

	overcharged := cp()
	overcharged[m1eIndexOf(overcharged, free.ref)].Outcome = charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}
	out = append(out, m1eCorruption{"one outcome changed (a free repeat made a charge)", free.ref, "want free", overcharged})

	for i := range items { // a mapping that forgot flags.from_cache would bill the cache hit as a fresh answer
		if items[i].kind == m1eKindCacheHit {
			uncached := cp()
			uncached[m1eIndexOf(uncached, items[i].ref)].Outcome.FromCache = false
			out = append(out, m1eCorruption{"the cache flag lost in the mapping", items[i].ref, "want charge m1_cache", uncached})
		}
	}
	return out
}

// m1eLedgerCorruption is a ledger that has been broken in one named way.
type m1eLedgerCorruption struct {
	name, want string // want: a fragment the comparison must report
	ledger     []m1bCharge
}

// m1eReports says whether some line of problems names the ref and holds the fragment: the comparison reported THAT item for THAT reason, not merely something.
func m1eReports(problems []string, ref, fragment string) bool {
	for _, p := range problems {
		if strings.Contains(p, ref) && strings.Contains(p, fragment) {
			return true
		}
	}
	return false
}

// m1eCorruptLedgers breaks the ledger's rows in three ways (a charge dropped, a unit changed, a charge keyed by a position), each of which m1eCompareLedger must report.
func m1eCorruptLedgers(ledger []m1bCharge) []m1eLedgerCorruption {
	cp := func() []m1bCharge { return append([]m1bCharge(nil), ledger...) }
	dropped := cp()[1:]
	unit := cp()
	if unit[0].Unit == "m1_cache" {
		unit[0].Unit = "m1_own"
	} else {
		unit[0].Unit = "m1_cache"
	}
	keyed := cp()
	keyed[0].ItemRef = "0"
	return []m1eLedgerCorruption{
		{"a charge dropped", "ledger holds 0 charge row(s) for it", dropped},
		{"a unit changed", "the oracle says", unit},
		{"a charge keyed by a position", "which is no input row", keyed},
	}
}

// ---- the fixture's counters ----------------------------------------------------------------------------------------------------------------------------------------

// m1eCheckProbes says whether the fixture's SMTP sessions of a window are exactly one verification of each address in wantAddrs and nothing else: every one of those addresses was RCPTed
// exactly once (the target), every domain got exactly one nx- probe per address verified there, nothing was RCPTed that no row should have caused (a repeat of an address in the same
// job, a cache hit, an address of another job), and every session came from the node. wantAddrs are the DISTINCT addresses that reach the node in the window.
func m1eCheckProbes(sessions []m1nSession, nodeIP string, wantAddrs []string) []string {
	var p []string
	want := map[string]bool{}
	wantDomains := map[string]int{}
	for _, a := range wantAddrs {
		a = strings.ToLower(a)
		want[a] = true
		wantDomains[a[strings.LastIndex(a, "@")+1:]]++
	}
	targets := map[string]int{}
	probes := map[string]int{}
	for _, se := range sessions {
		rcpt := strings.ToLower(se.Rcpt)
		if se.Remote != nodeIP {
			p = append(p, fmt.Sprintf("a session from %s, only the node (%s) may open one", se.Remote, nodeIP))
		}
		if strings.HasPrefix(rcpt, "nx-") {
			probes[strings.ToLower(se.Domain)]++
		} else {
			targets[rcpt]++
		}
	}
	for _, a := range m1nSortedKeys(want) {
		if targets[a] != 1 {
			p = append(p, fmt.Sprintf("%s was RCPTed %d time(s), want exactly once", a, targets[a]))
		}
	}
	for _, a := range m1nSortedKeys(targets) {
		if !want[a] {
			p = append(p, fmt.Sprintf("%s was RCPTed %d time(s) although no row of this job should have caused it (a repeat inside the job, a cache hit, or another job's address)", a, targets[a]))
		}
	}
	for _, d := range m1nSortedKeys(wantDomains) {
		if probes[d] != wantDomains[d] {
			p = append(p, fmt.Sprintf("%s saw %d nx- probe(s), want %d (one per distinct address verified there)", d, probes[d], wantDomains[d]))
		}
	}
	for _, d := range m1nSortedKeys(probes) {
		if wantDomains[d] == 0 {
			p = append(p, fmt.Sprintf("%s saw %d nx- probe(s) although no address of this job should have reached it", d, probes[d]))
		}
	}
	if len(sessions) != 2*len(want) {
		p = append(p, fmt.Sprintf("%d RCPT(s) in total, want %d (the target and the probe of each of the %d distinct address(es))", len(sessions), 2*len(want), len(want)))
	}
	sort.Strings(p)
	return p
}

// ---- the module's own rows ----------------------------------------------------------------------------------------------------------------------------------------

// m1eCheckModuleRows says whether module 1's own tables hold exactly the rows that were sent: wantRows input rows and no malformed one (a malformed row never reaches the module).
func m1eCheckModuleRows(rows, malformed, wantRows int) []string {
	var p []string
	if rows != wantRows {
		p = append(p, fmt.Sprintf("module 1's verify.job_rows holds %d row(s) for the job, want %d (the rows that were sent)", rows, wantRows))
	}
	if malformed != 0 {
		p = append(p, fmt.Sprintf("module 1's verify.job_rows holds %d malformed row(s): a malformed address reached the module, it must never be sent", malformed))
	}
	return p
}

// ---- reading a listing: the sets, the walkers ----------------------------------------------------------------------------------------------------------------------

// m1eTuple is what two drains of one job must agree on, row by row: the identity of the input row and its verdict.
type m1eTuple struct{ item, status, reason, origin string }

func (u m1eTuple) String() string {
	return fmt.Sprintf("%s=%s/%s/%s", u.item, u.status, u.reason, orDash(u.origin))
}

func m1eTuplesOfRows(rows []m1ad.Row) []m1eTuple {
	out := make([]m1eTuple, len(rows))
	for i, r := range rows {
		out[i] = m1eTuple{r.ItemID, r.Status, r.Reason, r.FailureOrigin}
	}
	return out
}

// m1eSetDiff compares two multisets of tuples exactly: what is missing from got, what is extra, what appears a different number of times, and what changed (the same item with another
// verdict). It returns nothing only when the two are the same set, each tuple once.
func m1eSetDiff(want, got []m1eTuple) []string {
	var p []string
	wantBy, gotBy := map[string]m1eTuple{}, map[string]m1eTuple{}
	wantN, gotN := map[string]int{}, map[string]int{}
	for _, u := range want {
		wantBy[u.item] = u
		wantN[u.item]++
	}
	for _, u := range got {
		gotBy[u.item] = u
		gotN[u.item]++
	}
	for item, n := range wantN {
		switch {
		case gotN[item] == 0:
			p = append(p, fmt.Sprintf("missing %s", wantBy[item]))
		case gotN[item] != n:
			p = append(p, fmt.Sprintf("%s appears %d time(s), want %d", item, gotN[item], n))
		case gotBy[item] != wantBy[item]:
			p = append(p, fmt.Sprintf("changed %s, want %s", gotBy[item], wantBy[item]))
		}
	}
	for item := range gotN {
		if wantN[item] == 0 {
			p = append(p, fmt.Sprintf("extra %s", gotBy[item]))
		}
	}
	sort.Strings(p)
	return p
}

// m1eWalk reads the listing with Results, page by page. dropLast is the LOOK-AHEAD CURSOR BUG: a walker that believes the last row of every page is the look-ahead row (the one the module
// read only to know whether another page exists) and so does not keep it. It returns the rows it kept, the number of pages and the size of each.
func m1eWalk(ctx context.Context, ad *m1ad.Adapter, jobID string, limit int, dropLast bool) ([]m1ad.Row, []int, error) {
	var rows []m1ad.Row
	var sizes []int
	cursor := ""
	for {
		page, next, err := ad.Results(ctx, jobID, cursor, limit)
		if err != nil {
			return nil, nil, err
		}
		sizes = append(sizes, len(page))
		if dropLast && len(page) > 0 {
			page = page[:len(page)-1]
		}
		rows = append(rows, page...)
		if next == "" {
			return rows, sizes, nil
		}
		if next == cursor || len(sizes) > 5000 {
			return nil, nil, fmt.Errorf("the listing did not advance after %d page(s)", len(sizes))
		}
		cursor = next
	}
}

func m1eOrdinalProblems(rows []m1ad.Row, n int) []string {
	var p []string
	seen := make([]int, n)
	for _, r := range rows {
		if r.Ordinal < 0 || r.Ordinal >= n {
			p = append(p, fmt.Sprintf("ordinal %d is outside 0..%d", r.Ordinal, n-1))
			continue
		}
		seen[r.Ordinal]++
	}
	for o, c := range seen {
		if c != 1 {
			p = append(p, fmt.Sprintf("ordinal %d was delivered %d time(s), want once", o, c))
		}
	}
	return p
}

func m1ePages(rows, limit int) int {
	if limit <= 0 {
		limit = m1ad.DefaultPageLimit
	}
	return (rows + limit - 1) / limit
}

// ---- errors and leaks ------------------------------------------------------------------------------------------------------------------------------------------------

// m1eWantErr says how err differs from the typed error the module's answer must give: a *modclient.Error of this kind (also by modclient.KindOf), HTTP status and module error code.
func m1eWantErr(what string, err error, kind modclient.Kind, status int, code string) (*modclient.Error, []string) {
	if err == nil {
		return nil, []string{what + ": no error, want a " + kind.String() + " error"}
	}
	var me *modclient.Error
	if !errors.As(err, &me) {
		return nil, []string{fmt.Sprintf("%s: the error is a %T, want a *modclient.Error", what, err)}
	}
	var p []string
	if me.Kind != kind || modclient.KindOf(err) != kind {
		p = append(p, fmt.Sprintf("%s: kind %s, want %s", what, me.Kind, kind))
	}
	if me.Status != status {
		p = append(p, fmt.Sprintf("%s: HTTP status %d, want %d", what, me.Status, status))
	}
	if me.Code != code {
		p = append(p, fmt.Sprintf("%s: module error code %q, want %q", what, me.Code, code))
	}
	return me, p
}

// m1eErrText renders an error every way a log line could, so that a leak in any of them is seen.
func m1eErrText(err error) string {
	return fmt.Sprintf("%v | %+v | %#v | %s | %q", err, err, err, err.Error(), err.Error())
}

// m1eLeaks names the secrets (name -> value) that appear in the text, compared case-insensitively.
func m1eLeaks(text string, secrets map[string]string) []string {
	low := strings.ToLower(text)
	var out []string
	for _, name := range m1nSortedKeys(secrets) {
		if v := strings.ToLower(strings.TrimSpace(secrets[name])); v != "" && strings.Contains(low, v) {
			out = append(out, name)
		}
	}
	return out
}
