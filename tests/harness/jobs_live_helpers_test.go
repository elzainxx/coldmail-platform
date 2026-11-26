//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Helpers of the LIVE job tests (TestJobsLive_*): Phase 4 chunks B8 to B12 against the REAL modules, with the job engine driven IN THIS PROCESS. Every name starts with `jl` so that it cannot clash with another file.
//
// WHAT IS REAL. The platform's job store over the LIVE database as platform_app (a real login, proven not to be a superuser), the money store, the tenant resolver and the privileged-function ensurer of module 1's
// caller keys (tenant.M1CallerEnsurer over platform.provision_m1_caller), the production jobengine.Registry over the three adapters, jobengine.Engine, and the REAL module 1 (its control plane, its bulk job) and the
// REAL module 3 (its Control, its three workers, the SMTP sinks). WHAT IS NOT: the engine runs in the test process instead of the platform service (PLATFORM_JOB_ENGINE_ENABLED is off in the harness services, so no
// other engine works on this database); its gate always leads (a test is not an election); its tick is driven by the test (Engine.Tick in a loop, every 700 ms) and its observation interval is one second (the service's
// dials are 1.5 s and 10 s); module 2 is not configured (its adapter is nil, as in a service whose dials are unset: its jobs would wait). Module 1 here has no verifier node (the main stack's m1; the node is the
// m1node stage's own control plane): what its verdicts can be is said in jobs_live_chain_test.go.
//
// THE WIRING IS internal/app/jobengine.go's, imitated: the adapters from one config.Config (m1.FromConfig, m3.FromConfig; module 2 absent), jobs.New with the module 3 pre-check (jlM3PreChecks: app.m3PreChecks is
// unexported, so its 20 lines are RE-IMPLEMENTED here, line for line), jobengine.NewRegistry with tenant.NewM1CallerEnsurer(resolver, pool) as its provisioner, jobengine.Config as engineConfig builds it (module 1's
// batch is jobs.MaxEngineIDs). ONE decoration: the registry is wrapped so that the engine serves only the users these tests made (jlMine): the live database may hold a job some other test left in a state the engine
// would start, and a test must never send for it.

// jlKeySeq makes every idempotency key of these tests its own.
var jlKeySeq atomic.Int64

// jlKey is a confirm / grant key: letters, digits and . _ : - only (the confirm key's alphabet, module 1's idempotency alphabet).
func jlKey(label string) string {
	return fmt.Sprintf("jl.%s.%d.%d", label, time.Now().UnixNano(), jlKeySeq.Add(1))
}

func jlShort(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// jlCtx is the test's budget: every wait of the test, every database read and every engine tick ends with it.
func jlCtx(t *testing.T, budget time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	return ctx
}

// ---- the log of the engine, kept for the failure message ----------------------------------------------------------------------------------------------------------------

// jlRing keeps the last lines the engine, the registry and the store logged, so that a wait that times out can say what the engine was doing. It never reaches the log of the run unless the test fails; the
// engine and the store log words, ids and counts, never an address or a key.
type jlRing struct {
	mu    sync.Mutex
	lines []string
	part  string
	max   int
}

func (r *jlRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.part += string(p)
	for {
		i := strings.IndexByte(r.part, '\n')
		if i < 0 {
			break
		}
		line := r.part[:i]
		r.part = r.part[i+1:]
		if len(line) > 420 {
			line = line[:420] + "..."
		}
		r.lines = append(r.lines, line)
		if len(r.lines) > r.max {
			r.lines = r.lines[len(r.lines)-r.max:]
		}
	}
	return len(p), nil
}

func (r *jlRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-n:]...)
}

// ---- the module 3 pre-check (a re-implementation of app.m3PreChecks) -------------------------------------------------------------------------------------------------------

// jlM3PreChecks is jobs.PreChecks over the module 3 adapter, exactly as internal/app/jobengine.go's m3PreChecks maps it (that type is unexported): the module's own quote, asked for the job's id and the rows that
// would reach it; a module that cannot be asked now (unreachable, timed out, rate limited, paused by its operator) is Unavailable; any other failure is an error; the causes are the words of a run refusal the
// module predicts, in its order.
type jlM3PreChecks struct{ ad *m3.Adapter }

var _ jobs.PreChecks = jlM3PreChecks{}

func (p jlM3PreChecks) M3Feasibility(ctx context.Context, userID, campaignID string, recipients int) (jobs.Feasibility, error) {
	c, err := p.ad.For(userID)
	if err != nil {
		return jobs.Feasibility{}, err
	}
	f, err := c.FeasibilityForCampaign(ctx, campaignID, int64(recipients))
	if err != nil {
		if modclient.Retryable(err) || modclient.KindOf(err) == modclient.Paused {
			return jobs.Feasibility{Unavailable: true}, nil
		}
		return jobs.Feasibility{}, err
	}
	causes := make([]string, 0, len(f.Causes()))
	for _, c := range f.Causes() {
		causes = append(causes, string(c))
	}
	return jobs.Feasibility{Feasible: f.Feasible, Causes: causes}, nil
}

// ---- the registry, limited to this test's users ---------------------------------------------------------------------------------------------------------------------------

// jlMine is a jobengine.Modules that serves the users this test made and makes every other user's job WAIT (jobengine.ErrWait: the same thing a module that cannot answer does), so that a job some other test left in
// the live database is never started, observed or cancelled by this engine's registry.
type jlMine struct {
	inner jobengine.Modules
	mu    sync.Mutex
	users map[string]bool
}

func (m *jlMine) add(user string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[user] = true
}

func (m *jlMine) For(ctx context.Context, userID, moduleID string) (jobengine.Module, error) {
	m.mu.Lock()
	ok := m.users[userID]
	m.mu.Unlock()
	if !ok {
		return nil, jobengine.Wait("this job belongs to another test: the live job tests' engine serves only its own users")
	}
	return m.inner.For(ctx, userID, moduleID)
}

// ---- the stack ---------------------------------------------------------------------------------------------------------------------------------------------------------------

// jlStack is the platform side of one live test: its own connections, store, registry and engine, over the live database and the real modules.
type jlStack struct {
	t       *testing.T
	ctx     context.Context
	admin   *pgx.Conn     // a SUPERUSER connection: seeds and observes, never the code under test
	pool    *pgxpool.Pool // real logins as platform_app: what the store, the money store, the resolver and the ensurer run on
	cfg     config.Config
	mny     *money.Store
	res     *tenant.Resolver
	fac     *m1ad.Factory
	m3ad    *m3.Adapter
	store   *jobs.Store
	mine    *jlMine
	gate    *jobenginetest.Gate
	eng     *jobengine.Engine
	ring    *jlRing
	adminID string
	users   []string // every user this test made, for the cleanup
	made    []string // every job this test made, for the history a failure prints
}

// jlConfig is the configuration the service would read: the module URLs and tokens of the harness stack (the names the other live tests read) and a key secret MINTED FOR THIS TEST (like every other test that derives
// module 1 keys: the key of a user is a function of the secret, and the same secret registers it and presents it, so no value of it is ever stored or printed).
func jlConfig(t *testing.T, secret config.Secret) config.Config {
	t.Helper()
	return config.Config{
		M1URL:              m3lBaseURL(t, m1bEnvURL),
		M3URL:              m3lBaseURL(t, m3lEnvURL),
		M3ServiceToken:     config.Secret(strings.TrimSpace(requireEnv(t, m3lEnvToken))),
		M3AdminToken:       config.Secret(strings.TrimSpace(requireEnv(t, m3lEnvAdminKey))),
		M1KeySecret:        secret,
		M1KeyVersion:       config.MinM1KeyVersion,
		ModuleTimeout:      20 * time.Second,
		ModuleRetries:      2,
		ModuleRetryBackoff: 250 * time.Millisecond,
	}
}

// newJlStack builds the stack. It FAILS (never skips) when the environment of the harness stack is not there: the variable it misses is named.
func newJlStack(t *testing.T, ctx context.Context) *jlStack {
	t.Helper()
	adminURL := requireEnv(t, envAdminURL)
	s := &jlStack{t: t, ctx: ctx, ring: &jlRing{max: 120}}
	s.admin = dial(t, adminURL, func(c *pgx.ConnConfig) { c.RuntimeParams["application_name"] = "jobs-live-test" })
	s.pool = mnyPool(t, adminURL) // proves a real login as platform_app, not a superuser
	secret := config.Secret(randomHex(t, 32))
	s.cfg = jlConfig(t, secret)
	log := slog.New(slog.NewTextHandler(s.ring, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := s.admin.QueryRow(ctx, `SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at, id LIMIT 1`).Scan(&s.adminID); err != nil {
		t.Fatalf("platform.admins has no active admin (stage 4b creates the first one: every grant and entitlement is written against a named admin): %v", err)
	}

	var err error
	if s.res, err = tenant.NewResolver(s.pool, s.cfg.M1KeySecret, tenant.WithM1KeyVersion(s.cfg.M1KeyVersion)); err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	if s.fac, err = m1ad.FromConfig(s.cfg); err != nil {
		t.Fatalf("m1.FromConfig: %v", err) // never carries the URL
	}
	if s.m3ad, err = m3.FromConfig(s.cfg); err != nil {
		t.Fatalf("m3.FromConfig: %v", err)
	}
	s.mny = money.New(s.pool, money.WithLockTimeout(20*time.Second))
	if s.store, err = jobs.New(s.pool, s.mny, jobs.WithMaxRows(jobs.DefaultMaxRows), jobs.WithMaxStartDelay(jobs.DefaultMaxStartDelay), jobs.WithLogger(log), jobs.WithPreChecks(jlM3PreChecks{ad: s.m3ad})); err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	reg := jobengine.NewRegistry(jobengine.RegistryDeps{
		Resolver:          s.res,
		M1:                s.fac,
		M2:                nil, // module 2 is not part of these tests: a service whose dials are unset serves its jobs by waiting
		M3:                s.m3ad,
		Provisioner:       tenant.NewM1CallerEnsurer(s.res, s.pool),
		SubmitConcurrency: jobengine.DefaultSubmitConcurrency,
		Log:               log,
	})
	s.mine = &jlMine{inner: reg, users: map[string]bool{}}
	s.gate = jobenginetest.NewGate(true)
	s.eng, err = jobengine.New(jobengine.Deps{Store: s.store, Modules: s.mine, Leader: s.gate, Log: log}, jobengine.Config{
		Tick:        700 * time.Millisecond,
		SettleEvery: time.Second,
		StaleAfter: map[string]time.Duration{
			jobs.ModuleVerify:      30 * time.Minute,
			jobs.ModuleSendManaged: 15 * time.Minute,
			jobs.ModuleSendBYO:     15 * time.Minute,
		},
		SubmitBatch:    500,
		SubmitBatchFor: map[string]int{jobs.ModuleVerify: jobs.MaxEngineIDs},
		StepTimeout:    jobengine.DefaultStepTimeout,
	})
	if err != nil {
		t.Fatalf("jobengine.New: %v", err)
	}
	t.Cleanup(s.cleanup)
	return s
}

// cleanup removes the module 1 caller rows of this test's users (module 1's table is committed state of the live database; the keys derived from this test's own secret are worthless afterwards) and, for a
// test that failed, ends what it left running as far as the store allows. It runs before the connections close (t.Cleanup is last in, first out).
func (s *jlStack) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if s.t.Failed() {
		for _, id := range s.made {
			var user, status string
			if err := s.admin.QueryRow(ctx, `SELECT user_id::text, status FROM platform.jobs WHERE id = $1::uuid`, id).Scan(&user, &status); err != nil {
				continue
			}
			var err error
			switch status {
			case jobs.StatusDraft, jobs.StatusPreviewed:
				_, err = s.store.DiscardDraft(ctx, user, id)
			case jobs.StatusConfirmed, jobs.StatusScheduled, jobs.StatusPaused, jobs.StatusRunning:
				_, err = s.store.CancelBeforeStart(ctx, user, id)
			}
			if err != nil {
				s.t.Logf("cleanup: job %s (%s) could not be ended through the store: %v", jlShort(id), status, err)
			}
		}
	}
	if len(s.users) > 0 {
		callers := make([]string, len(s.users))
		for i, u := range s.users {
			callers[i] = tenant.M1CallerID(u)
		}
		if _, err := s.admin.Exec(ctx, "DELETE FROM verify.api_callers WHERE user_id = ANY($1::uuid[]) OR caller_id = ANY($2::text[])", s.users, callers); err != nil {
			s.t.Errorf("cleanup: could not remove the module 1 caller rows of the test's users: %v", err)
		}
	}
}

// ---- users, credits -------------------------------------------------------------------------------------------------------------------------------------------------------

// adopt tells the engine's registry that a user is this test's.
func (s *jlStack) adopt(user string) {
	s.users = append(s.users, user)
	s.mine.add(user)
}

// newUser makes a real tenant: a login in auth.users, its platform user, resolved through the resolver exactly as the service resolves a login (the module scope is then built by the registry from the database).
func (s *jlStack) newUser(name string) string {
	s.t.Helper()
	var id string
	if err := s.admin.QueryRow(s.ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	email := "jl-" + name + "-" + id + "@example.test"
	tenSeedLogin(s.t, s.admin, id, email)
	tenSeedUser(s.t, s.admin, id, email, true)
	tn, err := s.res.ByAuthUser(s.ctx, id)
	if err != nil || tn.UserID != id || !tn.Active {
		s.t.Fatalf("tenant.ByAuthUser(%s) = %+v, %v: the user the test made must resolve as itself and active", name, tn, err)
	}
	s.adopt(id)
	return id
}

// grant puts credits on the user's balance through the money store (the ledger's one writer of balances).
func (s *jlStack) grant(user, module string, amount int64) {
	s.t.Helper()
	if _, err := s.mny.Grant(s.ctx, money.GrantReq{UserID: user, Module: module, Amount: amount, AdminID: s.adminID, Reason: "jobs live test funding", Key: jlKey("grant")}); err != nil {
		s.t.Fatalf("grant %d credits on %s: %v", amount, module, err)
	}
}

// balance is the user's balance on a module, straight from the table (0, 0 when the user has none).
func (s *jlStack) balance(user, module string) (available, reserved int64) {
	s.t.Helper()
	err := s.admin.QueryRow(s.ctx, `SELECT available, reserved FROM platform.balances WHERE user_id = $1::uuid AND module_id = $2`, user, module).Scan(&available, &reserved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.t.Fatalf("read the balance of %s on %s: %v", jlShort(user), module, err)
	}
	return available, reserved
}

// callerRows is how many rows module 1's verify.api_callers holds for the user: the proof that the engine's first submit registered the user's key, and that nothing did before.
func (s *jlStack) callerRows(user string) int64 {
	s.t.Helper()
	return s.count("count the module 1 caller rows", `SELECT count(*) FROM verify.api_callers WHERE user_id = $1::uuid`, user)
}

// m3User makes a module 3 tenant the way the module 3 adapter tests do (a login, a platform user, an adapter bound to the resolved user), adds a verified, aligned account at the controllable sink, gives the user three
// lanes with a short pause, and, when entitled, enables module 3 through the entitlements store and waits for the platform SERVICE's reconcile loop to deliver it.
func (s *jlStack) m3User(e *m3aEnv, name string, entitled bool) *m3aTenant {
	s.t.Helper()
	tn := e.seedTenant(s.t, name, nil)
	s.adopt(tn.UserID)
	e.createVerified(s.t, tn, "1", e.sinkIP)
	e.setPacing(s.t, tn, 3, 0, 1)
	if entitled {
		s.entitle(e, tn)
	}
	return tn
}

func (s *jlStack) entitle(e *m3aEnv, tn *m3aTenant) {
	s.t.Helper()
	en := e.enable(s.t, tn, "jobs-live")
	st := e.waitSync(s.t, tn, 1, false, "active", 60*time.Second)
	s.t.Logf("NOTE jl entitlement %s: enabled (ledger row %d), delivered by the platform service: %s", tn.Name, en.LedgerID, m3aSyncString(st))
}

// ---- jobs --------------------------------------------------------------------------------------------------------------------------------------------------------------------

func (s *jlStack) create(user string, req jobs.CreateReq) jobs.Job {
	s.t.Helper()
	j, err := s.store.CreateDraft(s.ctx, user, req)
	if err != nil {
		s.t.Fatalf("CreateDraft(%s): %v", req.Module, err)
	}
	s.made = append(s.made, j.ID)
	return j
}

func (s *jlStack) preview(user, job string) jobs.Preview {
	s.t.Helper()
	p, err := s.store.Preview(s.ctx, user, job)
	if err != nil {
		s.t.Fatalf("Preview(%s): %v", jlShort(job), err)
	}
	return p
}

// confirm confirms a previewed job and returns the store's answer and error as they are (the caller says which it expects).
func (s *jlStack) confirm(user, job, fingerprint string, start *time.Time) (jobs.ConfirmResult, error) {
	s.t.Helper()
	return s.store.Confirm(s.ctx, user, jobs.ConfirmReq{JobID: job, Fingerprint: fingerprint, Key: jlKey("confirm"), ScheduledStart: start})
}

func (s *jlStack) confirmOK(user, job, fingerprint string) jobs.ConfirmResult {
	s.t.Helper()
	res, err := s.confirm(user, job, fingerprint, nil)
	if err != nil {
		s.t.Fatalf("Confirm(%s): %v", jlShort(job), err)
	}
	return res
}

// start is create, preview and confirm of a job in one go.
func (s *jlStack) start(user string, req jobs.CreateReq) (jobs.Job, jobs.Preview, jobs.ConfirmResult) {
	s.t.Helper()
	j := s.create(user, req)
	pv := s.preview(user, j.ID)
	res := s.confirmOK(user, j.ID, pv.Fingerprint)
	return res.Job, pv, res
}

// ---- reading what the platform holds -------------------------------------------------------------------------------------------------------------------------------------

func (s *jlStack) count(what, sql string, args ...any) int64 {
	s.t.Helper()
	var n int64
	if err := s.admin.QueryRow(s.ctx, sql, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v\n  sql: %s", what, err, strings.Join(strings.Fields(sql), " "))
	}
	return n
}

func (s *jlStack) str(what, sql string, args ...any) string {
	s.t.Helper()
	var v string
	if err := s.admin.QueryRow(s.ctx, sql, args...).Scan(&v); err != nil {
		s.t.Fatalf("%s: %v\n  sql: %s", what, err, strings.Join(strings.Fields(sql), " "))
	}
	return v
}

func (s *jlStack) status(job string) string {
	s.t.Helper()
	return s.str("read the status of a job", `SELECT status FROM platform.jobs WHERE id = $1::uuid`, job)
}

// dbNow is the DATABASE's clock, the only clock a deadline is judged by.
func (s *jlStack) dbNow() time.Time {
	s.t.Helper()
	var now time.Time
	if err := s.admin.QueryRow(s.ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		s.t.Fatalf("read the database clock: %v", err)
	}
	return now.UTC()
}

func (s *jlStack) rows(job string) []jlRow {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT id::text, ordinal, COALESCE(email_norm, ''), state, COALESCE(reason, ''), COALESCE(source_item::text, ''), submitted_at IS NOT NULL
		FROM platform.job_rows WHERE job_id = $1::uuid ORDER BY ordinal`, job)
	if err != nil {
		s.t.Fatalf("read the rows of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	var out []jlRow
	for q.Next() {
		var r jlRow
		var ord int32
		if err := q.Scan(&r.ID, &ord, &r.Norm, &r.State, &r.Reason, &r.Source, &r.Submitted); err != nil {
			s.t.Fatal(err)
		}
		r.Ordinal = int(ord)
		out = append(out, r)
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *jlStack) results(job string) []jlResult {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT item_ref::text, ordinal, email, origin, module_state, COALESCE(reason, ''), decision, COALESCE(unit, ''), rule
		FROM platform.job_results WHERE job_id = $1::uuid ORDER BY ordinal, item_ref`, job)
	if err != nil {
		s.t.Fatalf("read the results of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	var out []jlResult
	for q.Next() {
		var r jlResult
		var ord int32
		if err := q.Scan(&r.Ref, &ord, &r.Email, &r.Origin, &r.State, &r.Reason, &r.Decision, &r.Unit, &r.Rule); err != nil {
			s.t.Fatal(err)
		}
		r.Ordinal = int(ord)
		out = append(out, r)
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// ledgerCharges is the job's charge rows of the ledger (the reader of the module 1 settlement test, over this test's connection).
func (s *jlStack) ledgerCharges(job string) []m1bCharge {
	s.t.Helper()
	return (&mnyEnv{t: s.t, admin: s.admin}).m1bLedger(job)
}

// prices is the job's frozen price snapshot: unit -> credits.
func (s *jlStack) prices(job string) map[string]int {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1::uuid`, job)
	if err != nil {
		s.t.Fatalf("read the price snapshot of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	out := map[string]int{}
	for q.Next() {
		var unit string
		var cost int
		if err := q.Scan(&unit, &cost); err != nil {
			s.t.Fatal(err)
		}
		out[unit] = cost
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// reserveOf is what the job's reserve held, charged and released (ok false when the job holds none).
func (s *jlStack) reserveOf(job string) (reserved, charged, released int64, ok bool) {
	s.t.Helper()
	err := s.admin.QueryRow(s.ctx, `SELECT reserved, charged, released FROM platform.reserves WHERE job_id = $1::uuid`, job).Scan(&reserved, &charged, &released)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, false
	}
	if err != nil {
		s.t.Fatalf("read the reserve of job %s: %v", jlShort(job), err)
	}
	return reserved, charged, released, true
}

func (s *jlStack) releaseRows(job string) (rows, credits int64) {
	s.t.Helper()
	if err := s.admin.QueryRow(s.ctx, `SELECT count(*), COALESCE(sum(available_delta), 0)::bigint FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'release'`, job).Scan(&rows, &credits); err != nil {
		s.t.Fatalf("count the release rows of job %s: %v", jlShort(job), err)
	}
	return rows, credits
}

func (s *jlStack) ledgerRowsOf(job string) int64 {
	s.t.Helper()
	return s.count("count the ledger rows of a job", `SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid`, job)
}

// events is the job's history, oldest first, one line each: what a failure prints and what the tests count.
func (s *jlStack) events(job string) []string {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT id, kind, COALESCE(from_status, ''), COALESCE(to_status, ''), actor_kind, reason FROM platform.job_events WHERE job_id = $1::uuid ORDER BY id`, job)
	if err != nil {
		s.t.Fatalf("read the events of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	var out []string
	for q.Next() {
		var id int64
		var kind, from, to, actor, reason string
		if err := q.Scan(&id, &kind, &from, &to, &actor, &reason); err != nil {
			s.t.Fatal(err)
		}
		if len(reason) > 160 {
			reason = reason[:160] + "..."
		}
		out = append(out, fmt.Sprintf("#%d %-10s %s -> %s by %s: %s", id, kind, orDash(from), orDash(to), actor, reason))
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// moves is how many times the job moved to the status (a `transition` event: the database writes one per change of status, with the actor and the reason).
func (s *jlStack) moves(job, to string) int64 {
	s.t.Helper()
	return s.count("count the moves of a job", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND to_status = $2`, job, to)
}

func (s *jlStack) movesBy(job, to, actor string) int64 {
	s.t.Helper()
	return s.count("count the moves of a job by an actor", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND to_status = $2 AND actor_kind = $3`, job, to, actor)
}

// requireBooks fails when the balance invariant, the per-reserve invariant or the settlement proof (ledger, results and rows agree) is broken anywhere in the live database.
func (s *jlStack) requireBooks(when string) {
	s.t.Helper()
	if d := mnyRows(s.t, s.admin, `SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`); len(d) != 0 {
		s.t.Fatalf("%s: platform.balance_drift is not empty (user|module|stored available|ledger available|stored reserved|ledger reserved): %v", when, d)
	}
	if o := mnyRows(s.t, s.admin, `SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`); len(o) != 0 {
		s.t.Fatalf("%s: platform.reserve_overdrawn is not empty (reserve|user|module|job|reserved|settled): %v", when, o)
	}
	if d := mnyRows(s.t, s.admin, `SELECT job_id::text, COALESCE(item_ref, '-'), problem FROM platform.job_settlement_drift ORDER BY 1, 2, 3`); len(d) != 0 {
		s.t.Fatalf("%s: platform.job_settlement_drift is not empty (job|item|problem): %v", when, d)
	}
}

// ---- driving the engine ---------------------------------------------------------------------------------------------------------------------------------------------------

// dump is what a failed wait prints: the engine's counters, the history of every job of the test, the last lines the engine and the store logged, and what the caller can say about the module.
func (s *jlStack) dump(diag func() string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  engine counters: %+v\n", s.eng.Stats())
	for _, j := range s.made {
		var status string
		if err := s.admin.QueryRow(context.Background(), `SELECT status FROM platform.jobs WHERE id = $1::uuid`, j).Scan(&status); err != nil {
			status = "(unreadable: " + err.Error() + ")"
		}
		fmt.Fprintf(&b, "  job %s is %s; its history:\n", jlShort(j), status)
		q, err := s.admin.Query(context.Background(), `SELECT id, kind, COALESCE(from_status, ''), COALESCE(to_status, ''), actor_kind, reason FROM platform.job_events WHERE job_id = $1::uuid ORDER BY id`, j)
		if err != nil {
			continue
		}
		for q.Next() {
			var id int64
			var kind, from, to, actor, reason string
			if q.Scan(&id, &kind, &from, &to, &actor, &reason) == nil {
				if len(reason) > 140 {
					reason = reason[:140] + "..."
				}
				fmt.Fprintf(&b, "    #%d %s %s -> %s by %s: %s\n", id, kind, orDash(from), orDash(to), actor, reason)
			}
		}
		q.Close()
	}
	b.WriteString("  the last lines of the engine's and the store's log:\n")
	for _, l := range s.ring.tail(30) {
		b.WriteString("    " + l + "\n")
	}
	if diag != nil {
		b.WriteString("  " + diag() + "\n")
	}
	return b.String()
}

// tickUntil ticks the engine, then asks cond, then waits `every`, until cond is true; at the deadline (or the test's own) it FAILS with what the wait was for, what cond last saw, the job histories and the log.
func (s *jlStack) tickUntil(what string, within, every time.Duration, diag func() string, cond func() (bool, string)) {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for {
		s.eng.Tick(s.ctx)
		ok, seen := cond()
		if ok {
			return
		}
		if !time.Now().Before(deadline) {
			s.t.Fatalf("%s: not reached within %s; last observation: %s\n%s", what, within, seen, s.dump(diag))
		}
		select {
		case <-s.ctx.Done():
			s.t.Fatalf("%s: the test's own time budget ended first; last observation: %s\n%s", what, seen, s.dump(diag))
		case <-time.After(every):
		}
	}
}

// tickToStatus ticks until the job has the status. A job that ends in another terminal status fails the test at once, with its history.
func (s *jlStack) tickToStatus(job, want string, within time.Duration, diag func() string) {
	s.t.Helper()
	s.tickUntil(fmt.Sprintf("job %s reaches %s", jlShort(job), want), within, 700*time.Millisecond, diag, func() (bool, string) {
		st := s.status(job)
		if st == want {
			return true, st
		}
		if jobs.IsTerminal(st) {
			s.t.Fatalf("job %s ended %s while the test waited for %s\n%s", jlShort(job), st, want, s.dump(diag))
		}
		return false, "status " + st
	})
}

// ---- module 1's own side -----------------------------------------------------------------------------------------------------------------------------------------------

// m1For is module 1's front door as the user: the adapter built from the module scope the resolver derives from the database (the user's own derived key), page size 7 so that a report of thirty rows takes five pages.
func (s *jlStack) m1For(user string) *m1ad.Adapter {
	s.t.Helper()
	tn, err := s.res.ByAuthUser(s.ctx, user)
	if err != nil {
		s.t.Fatalf("tenant.ByAuthUser: %v", err)
	}
	sc, err := s.res.Scope(s.ctx, tn)
	if err != nil {
		s.t.Fatalf("tenant.Scope: %v", err)
	}
	ad, err := s.fac.ForScope(sc)
	if err != nil {
		s.t.Fatalf("Factory.ForScope: %v", err) // never carries the key
	}
	ad.PageLimit, ad.Poll = 7, 500*time.Millisecond
	return ad
}

// m1Report reads module 1's OWN listing of its job, every page, with the user's key, after proving from the module's own status that the job is complete and holds exactly the rows the platform sent.
func (s *jlStack) m1Report(user, moduleJob string, sent int) []m1ad.Row {
	s.t.Helper()
	ad := s.m1For(user)
	st, err := ad.Status(s.ctx, moduleJob)
	if err != nil {
		s.t.Fatalf("module 1's status of its job: %v", err)
	}
	if !st.Complete || st.Counts.Total != sent || st.Counts.Settled != sent || st.Counts.Pending != 0 || st.Totals.Submitted != sent || st.Totals.Duplicates != 0 || st.Totals.Malformed != 0 {
		s.t.Fatalf("module 1's own status of the job is %+v: want complete, %d row(s) submitted and settled, none pending, none a repeat, none malformed (the platform sent only rows its own rules accept, once each)", st, sent)
	}
	var out []m1ad.Row
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 1000 {
			s.t.Fatalf("module 1's results listing did not end after %d pages", pages)
		}
		rows, next, err := ad.Results(s.ctx, moduleJob, cursor, 7)
		if err != nil {
			s.t.Fatalf("module 1's results listing: %v", err)
		}
		out = append(out, rows...)
		if next == "" {
			return out
		}
		if next == cursor {
			s.t.Fatalf("module 1's results listing does not advance")
		}
		cursor = next
	}
}

// moduleJobRef is module 1's own id of the platform job (platform.jobs.module_job_ref, written by the move to running).
func (s *jlStack) moduleJobRef(job string) string {
	s.t.Helper()
	return s.str("read the module reference of a job", `SELECT COALESCE(module_job_ref, '') FROM platform.jobs WHERE id = $1::uuid`, job)
}

// m1ModuleRows is what module 1's OWN tables hold for its job (the superuser reads them): the rows it recorded, how many it judged malformed or a repeat, and who owns the job.
func (s *jlStack) m1ModuleRows(moduleJob string) (rows, malformed, repeats int64, owner string) {
	s.t.Helper()
	err := s.admin.QueryRow(s.ctx, `SELECT count(*), count(*) FILTER (WHERE malformed), count(*) FILTER (WHERE duplicate_of_ordinal IS NOT NULL),
		COALESCE((SELECT owner_user_id::text FROM verify.jobs WHERE id = $1::uuid), '') FROM verify.job_rows WHERE job_id = $1::uuid`, moduleJob).Scan(&rows, &malformed, &repeats, &owner)
	if err != nil {
		s.t.Fatalf("read module 1's own rows of its job: %v", err)
	}
	return rows, malformed, repeats, owner
}

// ---- the lists the tests send ------------------------------------------------------------------------------------------------------------------------------------------

// jlList is a list a test sends with what the platform's own rules must make of it, known BY CONSTRUCTION (the tests assert the counts the store reports against these).
type jlList struct {
	Rows       []jobs.RowIn
	Pending    []string // the normalised addresses of the rows that are pending, in order
	Malformed  int
	Duplicates int
}

// jlMalformed are four addresses the platform's one address rule (norm/v1, then module 1's syntax layer) refuses, each for a different reason: no @, two @, no local part, a domain with no dot.
var jlMalformed = []string{"no-at-sign.example", "two@@signs.example", "@no-local-part.example", "dotless@nodot"}

// jlBuildList is a list of the pending addresses (given, already normalised) with malformed rows and repeats of the FIRST pending addresses in other spellings (upper case, the same text, spaces round it, a domain in
// capitals) spread between them: after every `gap` pending rows comes the next extra, alternately a malformed row and a repeat, so that a repeat always comes AFTER the row it repeats (the first spelling
// to appear is the pending one). malformed is at most len(jlMalformed), repeats at most four.
func jlBuildList(pending []string, malformed, repeats int) jlList {
	spell := []func(string) string{
		strings.ToUpper,
		func(a string) string { return a },
		func(a string) string { return "  " + a + "  " },
		func(a string) string {
			i := strings.IndexByte(a, '@')
			return a[:i+1] + strings.ToUpper(a[i+1:])
		},
	}
	var extras []jobs.RowIn
	for k := 0; k < max(malformed, repeats); k++ {
		if k < malformed {
			extras = append(extras, jobs.RowIn{Email: jlMalformed[k]})
		}
		if k < repeats {
			extras = append(extras, jobs.RowIn{Email: spell[k](pending[k])})
		}
	}
	l := jlList{Pending: append([]string(nil), pending...), Malformed: malformed, Duplicates: repeats}
	gap := max(1, len(pending)/(len(extras)+1))
	next := 0
	for i, a := range pending {
		l.Rows = append(l.Rows, jobs.RowIn{Email: a})
		if (i+1)%gap == 0 && next < len(extras) {
			l.Rows = append(l.Rows, extras[next])
			next++
		}
	}
	l.Rows = append(l.Rows, extras[next:]...)
	return l
}

// jlAddrs are n distinct, valid, lower-case addresses: <local>-NN@jl<k>.example.test, spread over `domains` domains of the harness's own (the platform's rule accepts them; module 1 meets a name no resolver of the
// private network can answer, whatever it then says of it).
func jlAddrs(local string, n, domains int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%02d@jl%d.example.test", local, i+1, i%domains)
	}
	return out
}

// jlContent is the campaign of a send job: a subject, an html body and a plain-text part.
func jlContent(what string) *jobs.Content {
	return &jobs.Content{Subject: "[jobs live] " + what, HTML: "<p>jobs live: " + what + "</p>", Text: "jobs live: " + what}
}
