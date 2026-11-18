//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/m3admin"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Helpers of the module 3 ADAPTER tests (m3_adapter_e2e_test.go, m3_adapter_e2e_outage_test.go): Phase 3 chunks B9 (end to end), B10 (pagination), B11 (cross-tenant) and B12 (the outage) for module 3.
// Every name starts with `m3a` so it cannot clash with another file's helpers; the file builds on the m3l* and m3h* helpers (m3_live_helpers_test.go, m3_hostile_helpers_test.go) for what already
// works against the real module 3 and its sinks: the HTTP client of the operator surface (m3lEnv), the waiting loop (m3lWait), the sink's control API types (m3hMode, m3hSinkMsg, m3hRcpt, m3hInflight).
//
// WHAT IS DRIVEN, AND WHAT IS NOT. Every call to module 3's DOMAIN A (quote, run, report, the accounts, the test button, bulk add) in these tests goes through internal/adapters/m3 (built by m3.FromConfig,
// the way the service builds it, with the one option only the harness needs: the suspension reader; the sink is a private address, which module 3's destination guard lets through because run.sh
// stage 5b sets its smtp_host_allow_cidrs to the private compose network) and, for the page-size tests, through
// sendapi.New over the same module client. What module 3 needs before it can send and what the platform does not do is set up beside the adapter and is the tenant's own setup, not the adapter's: the user
// rows and the job items (the platform's tables, written as the superuser, as the other module 3 tests do), the per-user pacing (module 3's operator surface, the admin key), the dials restored at the end of
// a test (module 3's own table). The entitlement and its delivery to module 3 go through the platform's real path: entitlements.Store as platform_app, delivered by the platform SERVICE's reconcile loop.
//
// THE TENANT. The login is a real auth.users row and the platform user a real public.users row on the LIVE database; the module scope the adapter is built from comes from internal/tenant (ByAuthUser,
// then Scope: the platform user id, read back from the database, never the id the test wrote down).
//
// THE MONEY. Module 3 is NOT metered (charge table row M3-01: free, whatever the state; money.ErrNotMetered): the platform charges nothing for a module 3 send and its gate is the entitlement. So
// "settled item by item" means: Classify sorts every item (settle list, not yet, unmapped) and the ledger, the balances and the reserves of the tenant gain NOTHING; m3aCompareClassified and
// m3aCompareBooks say so and are shown to fail on corrupted copies. The hand oracle m3aHand is written from the owner's rule without reading rules.psv or calling charge.Decide.

const (
	m3aSMTPPort           = 1025
	m3aEnvPlatformURL     = "HARNESS_PLATFORM_URL" // optional: the platform service's base URL; the stack's own name for it is the default
	m3aDefaultPlatformURL = "http://platform:8080"
	m3aPollEvery          = time.Second
	m3aBudget             = 15 * time.Minute

	// m3aEndToEndControls is the number of corrupted copies the end-to-end test shows its comparison functions to catch (six of the report, four of the sink, five of Classify, three of the books).
	m3aEndToEndControls = 18
)

// ---- the environment ----

type m3aEnv struct {
	ctx      context.Context
	raw      *m3lEnv // HTTP to module 3 (the operator surface, the admin key) and to the sinks
	ctlURL   string  // the controllable sink's control API
	cfg      config.Config
	mc       *modclient.Client // the module client the page-size tests build their own sendapi.Client over (m3.Adapter.For fixes the page size)
	adm      *m3admin.Client   // the platform's real admin client: the adapter's suspension reader
	ad       *m3.Adapter       // m3.FromConfig + the suspension reader
	db       *pgx.Conn         // a SUPERUSER: seeds and observes, never the code under test
	pool     *pgxpool.Pool     // real logins as platform_app on the live database
	res      *tenant.Resolver
	store    *entitlements.Store
	money    *money.Store
	admin    string // an active platform admin
	runID    string
	prefix   string // "m3a<run>-": the local-part prefix of every recipient of this test
	sinkIP   string // the controllable sink's private address: what the accounts name
	sinkPass string
	workers  []string
}

func newM3aEnv(t *testing.T, ctx context.Context) *m3aEnv {
	t.Helper()
	adminURL := requireEnv(t, envAdminURL)
	raw := m3lNewEnv(t, ctx)
	e := &m3aEnv{ctx: ctx, raw: raw, ctlURL: m3lBaseURL(t, m3hEnvCtlURL), runID: randomHex(t, 4), sinkPass: m3lSinkPassword(t)}
	e.prefix = "m3a" + e.runID + "-"
	e.db = dial(t, adminURL, func(cfg *pgx.ConnConfig) { cfg.RuntimeParams["application_name"] = "m3-adapter-test" })
	e.pool = m3lAppPool(t, adminURL, requireEnv(t, envAppPassword))
	e.store = entitlements.NewStore(e.pool)
	e.money = money.New(e.pool)
	res, err := tenant.NewResolver(e.pool, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	e.res = res
	e.cfg = config.Config{M3URL: raw.m3URL, M3ServiceToken: raw.token, M3AdminToken: raw.adminKey, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
	// every client of module 3 this file builds carries the contract drift session's observer (zz_m3_adapter_contract_drift_test.go): each real response is compared with its recorded fixture
	mopts := modclient.FromConfig(e.cfg)
	mopts.Observe = m3Drift.Observer()
	e.mc, err = modclient.New(modclient.M3, raw.m3URL, raw.token, nil, mopts)
	if err != nil {
		t.Fatalf("modclient.New: %s", raw.scrub(err.Error()))
	}
	e.adm, err = m3admin.New(raw.m3URL, raw.adminKey, nil)
	if err != nil {
		t.Fatalf("m3admin.New: %s", raw.scrub(err.Error()))
	}
	e.ad = e.adapter(t, e.cfg, e.adm)
	if err := e.db.QueryRow(ctx, "SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at LIMIT 1").Scan(&e.admin); err != nil {
		t.Fatalf("platform.admins has no active admin (the first-admin stage must have run: every entitlement is written against a named admin): %v", err)
	}
	for _, id := range strings.Split(requireEnv(t, m3hEnvWorkerIDs), ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			e.workers = append(e.workers, id)
		}
	}
	if len(e.workers) != 3 {
		t.Fatalf("%s names %d worker(s), want module 3's three workers", m3hEnvWorkerIDs, len(e.workers))
	}
	return e
}

// adapter builds the adapter the way the service does (m3.FromConfig). reader is the suspension reader.
func (e *m3aEnv) adapter(t *testing.T, cfg config.Config, reader m3.SuspensionReader) *m3.Adapter {
	t.Helper()
	opts := []m3.Option{m3.WithSuspensionReader(reader)}
	if _, err := m3.FromConfig(cfg, opts...); err != nil { // the way the service builds its adapter: the configuration must be accepted as it stands
		t.Fatalf("m3.FromConfig refused the harness configuration (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	// the adapter the tests drive is built over the same options with the drift session's observer added
	mopts := modclient.FromConfig(cfg)
	mopts.Observe = m3Drift.Observer()
	mc, err := modclient.New(modclient.M3, cfg.M3URL, cfg.M3ServiceToken, nil, mopts)
	if err != nil {
		t.Fatalf("modclient.New: %s", e.raw.scrub(err.Error()))
	}
	ad, err := m3.NewAdapter(mc, opts...)
	if err != nil {
		t.Fatalf("m3.NewAdapter: %s", e.raw.scrub(err.Error()))
	}
	return ad
}

// prepare checks what every test of this file needs and returns: module 3's three workers are registered and heartbeating, the private address the accounts will name is resolved, and the controllable sink
// answers. resetNow forgets everything the sink holds and puts it in its default mode (phase 3 of the outage proof passes false: the messages and counters the sink holds ARE its evidence); resetAtEnd puts the
// sink back to its default mode when the test ends (phase 1 of the outage proof passes false: the hold must still be in force when the stage stops the module).
func (e *m3aEnv) prepare(t *testing.T, resetNow, resetAtEnd bool) {
	t.Helper()
	rows, err := e.db.Query(e.ctx, "SELECT id::text FROM send_byo.boxes WHERE status = 'active' AND last_heartbeat_at > now() - interval '60 seconds'")
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
	for _, id := range e.workers {
		if !active[id] {
			t.Fatalf("module 3's worker %s has no heartbeat in the last 60 seconds (send_byo.boxes): these tests need the three workers (run.sh stage 5b starts them, the outage sub-stage restarts them)", id)
		}
	}
	addrs, err := net.DefaultResolver.LookupHost(e.ctx, m3hCtlHost)
	if err != nil {
		t.Fatalf("cannot resolve the controllable sink %q from the test container: %v", m3hCtlHost, err)
	}
	for _, a := range addrs {
		if m3hPrivateIPv4(a) {
			e.sinkIP = a
			break
		}
	}
	if e.sinkIP == "" {
		t.Fatalf("%s resolves to %v and not one of these is a private address: module 3's worker validates a certificate unless the SMTP host is private, and the sink's is self-signed", m3hCtlHost, addrs)
	}
	if resetNow {
		e.ctl(t, http.MethodPost, "/api/reset", nil, http.StatusOK)
		e.setMode(t, m3hMode{})
	} else if got := e.ctl(t, http.MethodGet, "/api/mode", nil, http.StatusOK); len(got) == 0 {
		t.Fatalf("the controllable sink answered nothing to GET /api/mode")
	}
	if resetAtEnd {
		t.Cleanup(e.restoreSink)
	}
}

func (e *m3aEnv) restoreSink() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.ctlURL+"/api/reset", nil)
	if err != nil {
		return
	}
	if resp, err := e.raw.hc.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// ---- the controllable sink ----

func (e *m3aEnv) ctl(t *testing.T, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	status, out := e.raw.do(t, e.ctlURL, method, path, "", body)
	if status != wantStatus {
		t.Fatalf("the controllable sink answered HTTP %d to %s %s, want %d: %s", status, method, path, wantStatus, e.raw.snip(out))
	}
	return out
}

// setMode REPLACES the sink's whole mode (a zero m3hMode is "no hostility at all") and checks the sink took it.
func (e *m3aEnv) setMode(t *testing.T, m m3hMode) {
	t.Helper()
	if m.RejectRcpt == nil {
		m.RejectRcpt = []string{}
	}
	if m.DeferRcpt == nil {
		m.DeferRcpt = []string{}
	}
	if m.DelayRecipients == nil {
		m.DelayRecipients = []string{}
	}
	out := e.ctl(t, http.MethodPost, "/api/mode", m, http.StatusOK)
	var got m3hMode
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the sink's answer to POST /api/mode is not a mode (%v): %s", err, e.raw.snip(out))
	}
	wantCode := m.RejectCode
	if wantCode == 0 {
		wantCode = 550
	}
	if len(got.RejectRcpt) != len(m.RejectRcpt) || len(got.DeferRcpt) != len(m.DeferRcpt) || got.DataDelayMs != m.DataDelayMs || len(got.DelayRecipients) != len(m.DelayRecipients) || got.FailAuth != m.FailAuth || got.RejectCode != wantCode {
		t.Fatalf("the sink did not take the mode it was sent: sent %d reject / %d defer / delay %d ms / %d delay recipient(s) / fail_auth %v / code %d, the sink says %s", len(m.RejectRcpt), len(m.DeferRcpt), m.DataDelayMs, len(m.DelayRecipients), m.FailAuth, wantCode, e.raw.snip(out))
	}
}

func (e *m3aEnv) inflight(t *testing.T) m3hInflight {
	t.Helper()
	var v m3hInflight
	out := e.ctl(t, http.MethodGet, "/api/inflight", nil, http.StatusOK)
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("the sink's /api/inflight is not the documented shape (%v): %s", err, e.raw.snip(out))
	}
	return v
}

// m3aView is the sink as a view of this test's recipients: how many messages each exact address received, each message, and what the sink answered at RCPT TO. Dups lists what can never be true at any
// instant: an address with more than one message, a Message-ID seen twice.
type m3aView struct {
	Counts map[string]int
	Msgs   []m3hSinkMsg
	Rcpt   map[string]m3hRcpt
	Dups   []string
}

func (v m3aView) of(tn *m3aTenant) int {
	n := 0
	for _, it := range tn.Items {
		n += v.Counts[it.Email]
	}
	return n
}

func (e *m3aEnv) sinkView(t *testing.T) m3aView {
	t.Helper()
	v := m3aView{Counts: map[string]int{}, Rcpt: map[string]m3hRcpt{}}
	var msgs []m3hSinkMsg
	if err := json.Unmarshal(e.ctl(t, http.MethodGet, "/api/messages", nil, http.StatusOK), &msgs); err != nil {
		t.Fatalf("the sink's message list is not a JSON list of messages: %v", err)
	}
	ids := map[string]string{}
	for _, m := range msgs {
		mine := false
		for _, raw := range m.To {
			a := strings.ToLower(strings.TrimSpace(raw))
			if strings.HasPrefix(a, e.prefix) {
				mine = true
				v.Counts[a]++
				if v.Counts[a] == 2 {
					v.Dups = append(v.Dups, fmt.Sprintf("%s holds 2 messages", a))
				}
			}
		}
		if !mine {
			continue
		}
		if m.MessageID == "" {
			v.Dups = append(v.Dups, fmt.Sprintf("a message to %v has no Message-ID", m.To))
		} else if prev, dup := ids[m.MessageID]; dup {
			v.Dups = append(v.Dups, fmt.Sprintf("the Message-ID %s was sent to %s and to %v", m.MessageID, prev, m.To))
		}
		ids[m.MessageID] = strings.Join(m.To, ",")
		v.Msgs = append(v.Msgs, m)
	}
	var rc map[string]m3hRcpt
	if err := json.Unmarshal(e.ctl(t, http.MethodGet, "/api/rcpt", nil, http.StatusOK), &rc); err != nil {
		t.Fatalf("the sink's /api/rcpt is not a map of counters: %v", err)
	}
	for a, c := range rc {
		if strings.HasPrefix(strings.ToLower(a), e.prefix) {
			v.Rcpt[strings.ToLower(a)] = c
		}
	}
	return v
}

// requireNoDups fails at once on what can never be true: a duplicate send.
func (e *m3aEnv) requireNoDups(t *testing.T, v m3aView) {
	t.Helper()
	if len(v.Dups) != 0 {
		t.Fatalf("the sink shows a duplicate send (exactly-once is violated): %s", strings.Join(v.Dups, "; "))
	}
}

// mailpitCounts is the number of messages the (cumulative, fuzzy-searched) Mailpit sink holds for each exact address of the tenant: the sink the `smtp-sink` host name reaches.
func (e *m3aEnv) mailpitCounts(t *testing.T, tn *m3aTenant) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, m := range e.raw.sinkMessages(t) {
		for a := range m.addresses() {
			if strings.HasPrefix(a, tn.Prefix) {
				out[a]++
			}
		}
	}
	return out
}

// ---- dials ----

// setDial writes one dial of module 3 the way run.sh stage 5b writes from_gate_mode (superuser SQL on send_byo.config, the {"v": ...} envelope) and puts the ORIGINAL value back when the test ends. Module 3
// reads each of these fresh, so the new value is in force at once.
func (e *m3aEnv) setDial(t *testing.T, key, jsonValue string) {
	t.Helper()
	var orig string
	if err := e.db.QueryRow(e.ctx, "SELECT value::text FROM send_byo.config WHERE key = $1", key).Scan(&orig); err != nil {
		t.Fatalf("read module 3's dial %s: %v", key, err)
	}
	tag, err := e.db.Exec(e.ctx, "UPDATE send_byo.config SET value = $2::jsonb WHERE key = $1", key, `{"v":`+jsonValue+`}`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write module 3's dial %s: %v (rows %d)", key, err, tag.RowsAffected())
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), 20*time.Second)
		defer cancel()
		if _, err := e.db.Exec(ctx, "UPDATE send_byo.config SET value = $2::jsonb WHERE key = $1", key, orig); err != nil {
			t.Errorf("restore module 3's dial %s to %s: %v", key, orig, err)
		}
	})
	t.Logf("NOTE m3a dial %s: %s -> {\"v\":%s} (the original is restored when the test ends)", key, orig, jsonValue)
}

func (e *m3aEnv) dialInt(t *testing.T, key string) int64 {
	t.Helper()
	var n int64
	if err := e.db.QueryRow(e.ctx, "SELECT (value->>'v')::bigint FROM send_byo.config WHERE key = $1", key).Scan(&n); err != nil {
		t.Fatalf("read module 3's integer dial %s: %v", key, err)
	}
	return n
}

// ---- tenants ----

type m3aItem struct {
	Index int
	ID    string // public.job_items.id: the platform's own job item id
	Email string
	Kind  string // ok | bounce | defer | fail
	JobID string // module 3's job id, set by runAll
}

type m3aTenant struct {
	Name, UserID, CampaignID, Prefix string
	Items                            []*m3aItem
	C                                *m3.Client // the adapter bound to the tenant
	Accounts                         []m3.Account
}

func (tn *m3aTenant) ids() []string {
	out := make([]string, len(tn.Items))
	for i, it := range tn.Items {
		out[i] = it.ID
	}
	return out
}

func (tn *m3aTenant) emails(kind string) []string {
	var out []string
	for _, it := range tn.Items {
		if kind == "" || it.Kind == kind {
			out = append(out, it.Email)
		}
	}
	return out
}

func (tn *m3aTenant) accountIDs() map[string]bool {
	out := map[string]bool{}
	for _, a := range tn.Accounts {
		out[a.ID] = true
	}
	return out
}

func (e *m3aEnv) uuid(t *testing.T) string {
	t.Helper()
	var id string
	if err := e.db.QueryRow(e.ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

func (e *m3aEnv) count(t *testing.T, what, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v\n  sql: %s", what, err, strings.Join(strings.Fields(sql), " "))
	}
	return n
}

// seedTenant creates, for real (committed), a login (auth.users), a platform user (public.users) and one campaign of job items with DISTINCT recipients that carry this run's id; kinds names the kind of
// each item (nil for a tenant with no items). It then resolves the login through internal/tenant and binds the adapter to the resolved user.
func (e *m3aEnv) seedTenant(t *testing.T, name string, kinds []string) *m3aTenant {
	t.Helper()
	tn := &m3aTenant{Name: name, UserID: e.uuid(t), CampaignID: e.uuid(t), Prefix: fmt.Sprintf("%s%s-", e.prefix, name)}
	login := fmt.Sprintf("m3adapter-%s-%s@example.test", name, e.runID)
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatalf("begin the seeding transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(e.ctx)) }()
	if _, err := tx.Exec(e.ctx, "INSERT INTO auth.users (id, email) VALUES ($1::uuid, $2)", tn.UserID, login); err != nil {
		t.Fatalf("seed auth.users for %s: %v", name, err)
	}
	if _, err := tx.Exec(e.ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)", tn.UserID, login); err != nil {
		t.Fatalf("seed public.users for %s: %v", name, err)
	}
	for i, kind := range kinds {
		rcpt := fmt.Sprintf("%s%02d@example.test", tn.Prefix, i+1)
		var id string
		err := tx.QueryRow(e.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`,
			tn.CampaignID, tn.UserID, rcpt, fmt.Sprintf("[m3adapter %s] %s %02d", e.runID, name, i+1), fmt.Sprintf("<p>m3adapter %s %s %02d</p>", e.runID, name, i+1)).Scan(&id)
		if err != nil {
			t.Fatalf("seed public.job_items %d of %s: %v", i+1, name, err)
		}
		tn.Items = append(tn.Items, &m3aItem{Index: i + 1, ID: id, Email: rcpt, Kind: kind})
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit the seed of %s: %v", name, err)
	}
	if n := e.count(t, "count the seeded items", "SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND user_id = $2::uuid", tn.CampaignID, tn.UserID); n != len(tn.Items) {
		t.Fatalf("seeded %d job items for %s but the table holds %d", len(tn.Items), name, n)
	}
	tn.C = e.bind(t, tn, e.ad)
	return tn
}

// bind resolves the tenant's login to its platform user (internal/tenant ByAuthUser), asks for the module scope (Scope re-reads the database and refuses a Tenant written by hand) and builds the adapter
// for the id the scope names.
func (e *m3aEnv) bind(t *testing.T, tn *m3aTenant, ad *m3.Adapter) *m3.Client {
	t.Helper()
	ten, err := e.res.ByAuthUser(e.ctx, tn.UserID) // a platform user IS its login (users_auth_link: id = auth_user_id)
	if err != nil {
		t.Fatalf("tenant.ByAuthUser(%s): %v", tn.Name, err)
	}
	if ten.UserID != tn.UserID || !ten.Active {
		t.Fatalf("tenant.ByAuthUser(%s) resolved to user %s (active %v), want %s active", tn.Name, ten.UserID, ten.Active, tn.UserID)
	}
	sc, err := e.res.Scope(e.ctx, ten)
	if err != nil {
		t.Fatalf("tenant.Scope(%s): %v", tn.Name, err)
	}
	if sc.M3UserID != tn.UserID {
		t.Fatalf("the module 3 scope of %s names user %s, want %s", tn.Name, sc.M3UserID, tn.UserID)
	}
	c, err := ad.For(sc.M3UserID)
	if err != nil {
		t.Fatalf("m3.Adapter.For(%s): %s", tn.Name, e.raw.scrub(err.Error()))
	}
	if c.UserID() != tn.UserID {
		t.Fatalf("the adapter of %s acts for user %s", tn.Name, c.UserID())
	}
	return c
}

// setPacing gives the user's lanes and the pause between two sends of one lane (module 3's operator surface, the admin key: not a Domain A route) and reads them back from module 3's own table.
func (e *m3aEnv) setPacing(t *testing.T, tn *m3aTenant, lanes, lo, hi int) {
	t.Helper()
	status, body := e.raw.do(t, e.raw.m3URL, http.MethodPut, "/v1/admin/users/"+tn.UserID+"/config", e.raw.adminKey,
		map[string]int{"concurrency_per_user": lanes, "send_delay_min_sec": lo, "send_delay_max_sec": hi})
	e.raw.expect(t, "PUT the per-user pacing of "+tn.Name, status, body, http.StatusOK, "")
	var conc, dmin, dmax int
	err := e.db.QueryRow(e.ctx, `SELECT COALESCE(concurrency_per_user, -1), COALESCE(send_delay_min_sec, -1), COALESCE(send_delay_max_sec, -1)
		FROM send_byo.user_config_overrides WHERE user_id = $1::uuid`, tn.UserID).Scan(&conc, &dmin, &dmax)
	if err != nil {
		t.Fatalf("read back module 3's per-user pacing of %s: %v", tn.Name, err)
	}
	if conc != lanes || dmin != lo || dmax != hi {
		t.Fatalf("module 3 stored concurrency_per_user=%d send_delay_min_sec=%d send_delay_max_sec=%d for %s, want %d, %d, %d", conc, dmin, dmax, tn.Name, lanes, lo, hi)
	}
}

// newAccount is the account a user adds: an SMTP login at host and the address mail is sent from. The secret is the run's sink password (the sinks accept any credential; no literal is in the tree).
func (e *m3aEnv) newAccount(label string, tn *m3aTenant, host string) m3.NewAccount {
	from := fmt.Sprintf("sender-%s-%s-%s@example.test", label, tn.Name, e.runID)
	name := "Harness"
	return m3.NewAccount{SMTPHost: host, SMTPPort: m3aSMTPPort, SMTPUsername: from, SMTPSecret: config.Secret(e.sinkPass), FromAddress: from, FromName: &name}
}

// checkAccount asserts what every account the adapter returns must be: the tenant's, at the host it named, never carrying the secret (in any fmt verb), and with a credential on file.
func (e *m3aEnv) checkAccount(t *testing.T, tn *m3aTenant, host, what string, a m3.Account) {
	t.Helper()
	if a.UserID != tn.UserID || a.SMTPHost != host || !a.HasCredential || a.ID == "" {
		t.Errorf("%s: the account is user %s host %s has_credential %v id %q, want user %s host %s, a credential on file and an id", what, a.UserID, a.SMTPHost, a.HasCredential, a.ID, tn.UserID, host)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(fmt.Sprintf(verb, a), e.sinkPass) {
			t.Errorf("%s: the SMTP secret is in the printed account (%s)", what, verb)
		}
	}
}

// createVerified adds an account through the adapter and requires it verified and aligned: the sink answered the module's worker, and the From-gate is off (run.sh stage 5b).
func (e *m3aEnv) createVerified(t *testing.T, tn *m3aTenant, label, host string) m3.Account {
	t.Helper()
	a, err := tn.C.CreateAccount(e.ctx, e.newAccount(label, tn, host))
	if err != nil {
		t.Fatalf("CreateAccount (%s of %s) through the adapter failed (%s, may have been applied: %v): %s", label, tn.Name, modclient.KindOf(err), m3.MaybeApplied(err), e.raw.scrub(err.Error()))
	}
	e.checkAccount(t, tn, host, "CreateAccount "+label, a)
	if a.Verification.State != m3.VerificationVerified || a.FromAlignment.State != m3.AlignmentAligned {
		t.Fatalf("the account %s of %s: module 3 says verification %q (code %q, response %v) and alignment %q, want verified and aligned: the worker could not verify the sink, or the From gate is not off", label, tn.Name, a.Verification.State, a.Verification.Code, a.Verification.SMTPResponse, a.FromAlignment.State)
	}
	tn.Accounts = append(tn.Accounts, a)
	return a
}

// ---- the entitlement, through the real store and the platform SERVICE's reconcile loop ----

func (e *m3aEnv) enable(t *testing.T, tn *m3aTenant, step string) entitlements.Entry {
	t.Helper()
	en, err := e.store.Enable(e.ctx, entitlements.EnableReq{UserID: tn.UserID, AdminID: e.admin, Reason: "m3 adapter harness: " + step, Key: fmt.Sprintf("m3adapter-%s-%s-%s", e.runID, tn.Name, step)})
	if err != nil {
		t.Fatalf("Store.Enable(%s, %s): %v", tn.Name, step, err)
	}
	if en.Replayed || en.Kind != "m3_enable" {
		t.Fatalf("Store.Enable(%s, %s) returned %+v: want a fresh m3_enable ledger row", tn.Name, step, en)
	}
	return en
}

func (e *m3aEnv) disable(t *testing.T, tn *m3aTenant, step string) entitlements.Entry {
	t.Helper()
	en, err := e.store.Disable(e.ctx, entitlements.DisableReq{UserID: tn.UserID, AdminID: e.admin, Reason: "m3 adapter harness: " + step, Key: fmt.Sprintf("m3adapter-%s-%s-%s", e.runID, tn.Name, step)})
	if err != nil {
		t.Fatalf("Store.Disable(%s, %s): %v", tn.Name, step, err)
	}
	if en.Replayed || en.Kind != "m3_disable" {
		t.Fatalf("Store.Disable(%s, %s) returned %+v: want a fresh m3_disable ledger row", tn.Name, step, en)
	}
	return en
}

func m3aSyncString(st entitlements.SyncState) string {
	last := "-"
	if st.LastError != nil {
		last = "set"
	}
	return fmt.Sprintf("exists=%v want_suspended=%v want_version=%d converged_version=%d observed=%s observed_version=%s m3_version=%s attempts=%d last_error=%s",
		st.Exists, st.WantSuspended, st.WantVersion, st.ConvergedVersion, st.Observed, entObsStr(st.ObservedVersion), entObsStr(st.M3Version), st.Attempts, last)
}

// waitSync waits (bounded) until the SERVICE's reconcile loop has delivered the wanted state to the real module 3 and recorded what module 3 itself answered: the row is at wantVersion, wants wantSuspended, is
// converged (converged_version = want_version and observed is the wanted state, at that version) and observed is the given state.
func (e *m3aEnv) waitSync(t *testing.T, tn *m3aTenant, wantVersion int64, wantSuspended bool, observed string, within time.Duration) entitlements.SyncState {
	t.Helper()
	var got entitlements.SyncState
	m3lWait(t, e.ctx, fmt.Sprintf("the platform service's reconcile loop converges %s at want_version %d (observed %s)", tn.Name, wantVersion, observed), within, 500*time.Millisecond, func() (bool, string) {
		st, err := e.store.Sync(e.ctx, tn.UserID)
		if err != nil {
			t.Fatalf("Store.Sync(%s): %v", tn.Name, err)
		}
		got = st
		return st.Exists && st.WantVersion == wantVersion && st.WantSuspended == wantSuspended && st.Converged() && st.Observed == observed && st.ObservedAt != nil, m3aSyncString(st)
	})
	return got
}

// ---- what module 3 itself recorded (the superuser reads its tables: ground truth beside the adapter) ----

type m3aJob struct {
	ID, ItemID, State string
	Attempts          int
}

// jobsOf is module 3's job for every job item of the user, by job item id.
func (e *m3aEnv) jobsOf(t *testing.T, userID string) map[string]m3aJob {
	t.Helper()
	rows, err := e.db.Query(e.ctx, "SELECT id::text, job_item_id::text, state::text, attempt_count FROM send_byo.jobs WHERE user_id = $1::uuid", userID)
	if err != nil {
		t.Fatalf("read module 3's jobs of %s: %v", userID, err)
	}
	defer rows.Close()
	out := map[string]m3aJob{}
	for rows.Next() {
		var j m3aJob
		if err := rows.Scan(&j.ID, &j.ItemID, &j.State, &j.Attempts); err != nil {
			t.Fatal(err)
		}
		out[j.ItemID] = j
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *m3aEnv) jobStates(jobs map[string]m3aJob) string {
	by := map[string]int{}
	for _, j := range jobs {
		by[j.State]++
	}
	var parts []string
	for s, n := range by {
		parts = append(parts, fmt.Sprintf("%s=%d", s, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

type m3aSusp struct {
	Exists       bool
	Suspended    bool
	SuspendedAt  *time.Time
	ReinstatedAt *time.Time
}

// suspension is module 3's own record (send_byo.user_suspensions); a missing row means "never suspended".
func (e *m3aEnv) suspension(t *testing.T, user string) m3aSusp {
	t.Helper()
	var s m3aSusp
	err := e.db.QueryRow(e.ctx, "SELECT suspended, suspended_at, reinstated_at FROM send_byo.user_suspensions WHERE user_id = $1::uuid", user).Scan(&s.Suspended, &s.SuspendedAt, &s.ReinstatedAt)
	switch {
	case err == pgx.ErrNoRows:
		return s
	case err != nil:
		t.Fatalf("read send_byo.user_suspensions for %s: %v", user, err)
	}
	s.Exists = true
	for _, p := range []**time.Time{&s.SuspendedAt, &s.ReinstatedAt} {
		if *p != nil {
			u := (**p).UTC()
			*p = &u
		}
	}
	return s
}

// ---- the platform's own books of a tenant ----

// m3aBooks is everything of the platform that a module 3 flow could touch for one tenant. M3 is not metered, so the money columns must stay zero for the whole life of the tenant; the entitlement rows are the
// platform's own and are written only by the test (m3_enable / m3_disable) or by the service (m3_expire).
type m3aBooks struct {
	Ledger       int    // every ledger row of the user
	OtherLedger  int    // ledger rows that are not an entitlement row (m3_enable, m3_disable, m3_expire)
	Monetary     int    // monetary ledger rows
	Balances     int    // platform.balances rows
	Reserves     int    // platform.reserves rows
	JobItems     int    // public.job_items rows of the user
	JobItemsHash string // md5 over (id, status, recipient) of those rows
	Entitlement  string // "enabled", "disabled" or "none"
	SyncFacts    string // the facts of platform.m3_sync a retry can never change: want_suspended, want_version, converged_version, observed, observed_version, m3_version
	Attempts     int    // platform.m3_sync.attempts: the platform's retry bookkeeping (informational: it rises while module 3 is down, by design)
}

func (e *m3aEnv) books(t *testing.T, userID string) m3aBooks {
	t.Helper()
	var b m3aBooks
	q := func(what, sql string, dst ...any) {
		t.Helper()
		if err := e.db.QueryRow(e.ctx, sql, userID).Scan(dst...); err != nil {
			t.Fatalf("read the books of %s (%s): %v", userID, what, err)
		}
	}
	q("ledger", `SELECT count(*), count(*) FILTER (WHERE kind NOT IN ('m3_enable', 'm3_disable', 'm3_expire')), count(*) FILTER (WHERE monetary) FROM platform.ledger WHERE user_id = $1::uuid`, &b.Ledger, &b.OtherLedger, &b.Monetary)
	q("balances", `SELECT count(*) FROM platform.balances WHERE user_id = $1::uuid`, &b.Balances)
	q("reserves", `SELECT count(*) FROM platform.reserves WHERE user_id = $1::uuid`, &b.Reserves)
	q("job items", `SELECT count(*), COALESCE(md5(string_agg(id::text || ':' || COALESCE(status::text, '') || ':' || recipient_email, ',' ORDER BY id)), '') FROM public.job_items WHERE user_id = $1::uuid`, &b.JobItems, &b.JobItemsHash)
	q("entitlement", `SELECT COALESCE((SELECT CASE WHEN enabled THEN 'enabled' ELSE 'disabled' END FROM platform.m3_entitlements WHERE user_id = $1::uuid), 'none')`, &b.Entitlement)
	q("sync", `SELECT COALESCE((SELECT want_suspended::text || '/' || want_version::text || '/' || converged_version::text || '/' || observed || '/' || COALESCE(observed_version::text, '-') || '/' || COALESCE(m3_version::text, '-')
		FROM platform.m3_sync WHERE user_id = $1::uuid), 'none'), COALESCE((SELECT attempts FROM platform.m3_sync WHERE user_id = $1::uuid), 0)`, &b.SyncFacts, &b.Attempts)
	return b
}

func (b m3aBooks) String() string {
	return fmt.Sprintf("ledger %d (other %d, monetary %d), balances %d, reserves %d, job items %d (%s), entitlement %s, sync %s (attempts %d)", b.Ledger, b.OtherLedger, b.Monetary, b.Balances, b.Reserves, b.JobItems, b.JobItemsHash, b.Entitlement, b.SyncFacts, b.Attempts)
}

// m3aBooksDiff is every fact of the platform that differs between two readings of one tenant's books. The retry counter is not a fact (the platform retries while module 3 is down: that is what it is for).
func m3aBooksDiff(before, after m3aBooks) []string {
	var p []string
	add := func(what string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			p = append(p, fmt.Sprintf("%s changed from %v to %v", what, a, b))
		}
	}
	add("the number of ledger rows", before.Ledger, after.Ledger)
	add("the number of non-entitlement ledger rows", before.OtherLedger, after.OtherLedger)
	add("the number of monetary ledger rows", before.Monetary, after.Monetary)
	add("the number of balance rows", before.Balances, after.Balances)
	add("the number of reserve rows", before.Reserves, after.Reserves)
	add("the number of job item rows", before.JobItems, after.JobItems)
	add("the job item rows' fingerprint", before.JobItemsHash, after.JobItemsHash)
	add("the entitlement", before.Entitlement, after.Entitlement)
	add("the delivery facts (platform.m3_sync)", before.SyncFacts, after.SyncFacts)
	return p
}

// m3aCompareBooks says what must hold of a module 3 tenant's books at every moment: nothing metered, nothing charged, no balance, no reserve. It returns problems, never fails a test, so that the same
// function can be shown to catch a ledger that gained a charge.
func m3aCompareBooks(b m3aBooks) []string {
	var p []string
	if b.OtherLedger != 0 {
		p = append(p, fmt.Sprintf("the ledger holds %d row(s) that are not entitlement rows for a module 3 tenant: module 3 is not metered", b.OtherLedger))
	}
	if b.Monetary != 0 {
		p = append(p, fmt.Sprintf("the ledger holds %d monetary row(s) for a module 3 tenant: nothing is charged", b.Monetary))
	}
	if b.Balances != 0 {
		p = append(p, fmt.Sprintf("the platform holds %d balance row(s) for a module 3 tenant: module 3 holds no credits", b.Balances))
	}
	if b.Reserves != 0 {
		p = append(p, fmt.Sprintf("the platform holds %d reserve row(s) for a module 3 tenant: module 3 reserves nothing", b.Reserves))
	}
	return p
}

// requireBooks fails when the two invariants of the money core are not empty on the live database.
func (e *m3aEnv) requireBooks(t *testing.T, when string) {
	t.Helper()
	if d := mnyRows(t, e.db, `SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`); len(d) != 0 {
		t.Fatalf("%s: platform.balance_drift is not empty (user|module|stored available|ledger available|stored reserved|ledger reserved): %v", when, d)
	}
	if o := mnyRows(t, e.db, `SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`); len(o) != 0 {
		t.Fatalf("%s: platform.reserve_overdrawn is not empty (reserve|user|module|job|reserved|settled): %v", when, o)
	}
}

// ---- the charge table, twice ----

type m3aDecision struct{ Kind, Unit, Rule string }

// m3aDecide is what the platform's charge table says (charge.Decide over internal/charge/rules.psv).
func m3aDecide(o charge.Outcome) m3aDecision {
	d := charge.Decide(o)
	return m3aDecision{Kind: d.Kind.String(), Unit: d.Unit, Rule: d.Rule}
}

// m3aHand is the owner's rule for module 3 written out by hand, without reading rules.psv or calling charge.Decide: module 3 is not metered (entitlements, not credits), so an item that reached an
// outcome (sent, bounced, failed) is settled free and nothing is charged; an item that has not finished waits; anything else is flagged, never guessed.
func m3aHand(state string) string {
	switch state {
	case "sent", "bounced", "failed", "cancelled": // cancelled is the Phase 3 close-out's terminal state (C1): never sent, free by row M3-01
		return "free"
	case "queued", "assigned", "sending":
		return "notfinal"
	}
	return "unmapped"
}

func m3aText(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func m3aNullIf(s string) string {
	if s == "" {
		return "<null>"
	}
	return s
}

func m3aTotalsLine(tt sendapi.Totals) string {
	return fmt.Sprintf("total=%d sent=%d bounced=%d failed=%d in_flight=%d pending=%d waiting_for_capacity=%d cancelled=%d", tt.Total, tt.Sent, tt.Bounced, tt.Failed, tt.InFlight, tt.Pending, tt.WaitingForCapacity, tt.Cancelled)
}

// ---- the comparisons: they return their problems as sentences so the SAME function can be run on a corrupted copy of what it just accepted ----

// m3aKindWant is what an item of a kind must be reported as: the state, the error class (the closed set), and a word the provider's answer must contain ("" = the report carries none: null).
type m3aKindWant struct {
	State, ErrorClass, SMTPContains string
	Dispatched                      bool // a worker took it: the report names the account that sent it
}

var m3aWantByKind = map[string]m3aKindWant{
	"ok":     {"sent", "", "", true},
	"defer":  {"sent", "", "", true}, // answered 451 until the sink relented, then sent: a sent item carries no error class and no provider text (the sticky columns are suppressed)
	"bounce": {"bounced", "bounce_permanent", "550", true},
	"fail":   {"failed", "", "", false}, // account exhaustion: the user has an account and none is eligible, so the item never reached a worker
}

// m3aCompareReport: every item the tenant submitted is reported exactly once, in the order it was run, in the (state, error class, provider text) of its kind, from the account that sent it.
func m3aCompareReport(tn *m3aTenant, items []sendapi.Item) []string {
	got := make([]string, len(items))
	for i, it := range items {
		got[i] = it.JobItemID
	}
	p := sendapitest.ExactSet(got, tn.ids())
	byID := map[string]sendapi.Item{}
	for _, it := range items {
		byID[it.JobItemID] = it
	}
	accounts := tn.accountIDs()
	for _, it := range tn.Items {
		rep, ok := byID[it.ID]
		if !ok {
			continue
		}
		w := m3aWantByKind[it.Kind]
		where := fmt.Sprintf("item %02d (%s, %s)", it.Index, it.Kind, it.Email)
		if rep.State != w.State || m3aText(rep.ErrorClass) != m3aNullIf(w.ErrorClass) || rep.Class != sendapi.ClassTerminal {
			p = append(p, fmt.Sprintf("%s is reported %s / %s (class %s), want %s / %s (terminal)", where, rep.State, m3aText(rep.ErrorClass), rep.Class, w.State, m3aNullIf(w.ErrorClass)))
		}
		if w.SMTPContains == "" && rep.SMTPResponse != nil {
			p = append(p, fmt.Sprintf("%s carries the provider text %q, want none (null)", where, *rep.SMTPResponse))
		}
		if w.SMTPContains != "" && (rep.SMTPResponse == nil || !strings.Contains(*rep.SMTPResponse, w.SMTPContains)) {
			p = append(p, fmt.Sprintf("%s carries the provider text %s, want one naming %s", where, m3aText(rep.SMTPResponse), w.SMTPContains))
		}
		if (rep.State == "sent") != (rep.ProviderMessageID != nil) {
			p = append(p, fmt.Sprintf("%s: provider message id present %v, want it exactly when the item is sent", where, rep.ProviderMessageID != nil))
		}
		if (rep.State == "failed" || rep.State == "bounced") != (rep.FailedAt != nil) {
			p = append(p, fmt.Sprintf("%s: failed_at present %v, want it exactly for a failed or bounced item", where, rep.FailedAt != nil))
		}
		switch {
		case w.Dispatched && (rep.AccountID == nil || !accounts[*rep.AccountID]):
			p = append(p, fmt.Sprintf("%s was sent by account %s, which is none of the tenant's accounts %v", where, m3aText(rep.AccountID), len(accounts)))
		case !w.Dispatched && rep.AccountID != nil:
			p = append(p, fmt.Sprintf("%s names account %s although no worker ever took it", where, *rep.AccountID))
		}
	}
	sort.Strings(p)
	return p
}

// m3aCompareSink: what the sink itself saw agrees with what module 3 reports, item by item. A sent item is one accepted RCPT and one message; a bounced item one rejected RCPT and no message, never
// retried; a failed item never reached the sink at all; an item that was answered 451 before it was sent shows at least minDeferred deferrals; and nothing is in the sink that is not an item of the tenant.
func m3aCompareSink(tn *m3aTenant, items []sendapi.Item, v m3aView, minDeferred int) []string {
	var p []string
	byID := map[string]sendapi.Item{}
	for _, it := range items {
		byID[it.JobItemID] = it
	}
	known := map[string]bool{}
	for _, it := range tn.Items {
		known[it.Email] = true
		rep, ok := byID[it.ID]
		if !ok {
			p = append(p, fmt.Sprintf("item %02d (%s) is not in the report", it.Index, it.Email))
			continue
		}
		c, msgs := v.Rcpt[it.Email], v.Counts[it.Email]
		where := fmt.Sprintf("item %02d (%s, %s)", it.Index, it.Kind, it.Email)
		switch rep.State {
		case "sent":
			if msgs != 1 || c.Accepted != 1 || c.Rejected != 0 {
				p = append(p, fmt.Sprintf("%s is reported sent but the sink holds %d message(s) and answered RCPT accepted=%d rejected=%d, want one message and one acceptance", where, msgs, c.Accepted, c.Rejected))
			}
			if it.Kind == "defer" && c.Deferred < minDeferred {
				p = append(p, fmt.Sprintf("%s was to be answered 451 at least %d time(s) before it was sent, the sink counted %d deferral(s)", where, minDeferred, c.Deferred))
			}
			if it.Kind != "defer" && c.Deferred != 0 {
				p = append(p, fmt.Sprintf("%s was never to be deferred but the sink counted %d deferral(s)", where, c.Deferred))
			}
		case "bounced":
			if c.Rejected != 1 || c.Accepted != 0 || msgs != 0 {
				p = append(p, fmt.Sprintf("%s is reported bounced but the sink answered RCPT rejected=%d accepted=%d and holds %d message(s), want exactly one rejection (a bounce is never retried) and no message", where, c.Rejected, c.Accepted, msgs))
			}
		case "failed":
			if c != (m3hRcpt{}) || msgs != 0 {
				p = append(p, fmt.Sprintf("%s is reported failed (it never reached a worker) but the sink answered RCPT %+v and holds %d message(s)", where, c, msgs))
			}
		default:
			p = append(p, fmt.Sprintf("%s is reported in state %s, which is not final", where, rep.State))
		}
	}
	for a, n := range v.Counts {
		if strings.HasPrefix(a, tn.Prefix) && !known[a] {
			p = append(p, fmt.Sprintf("the sink holds %d message(s) to %s, which is none of the tenant's recipients", n, a))
		}
	}
	p = append(p, v.Dups...)
	sort.Strings(p)
	return p
}

// m3aCompareClassified: Classify's three lists agree with the charge table AND with the hand oracle, item by item. Every terminal item is in the settle list exactly once carrying the outcome the module
// reported (module m3, its state, no reason: module 3 has none), free by row M3-01 and charged as no unit; an item that is not final is in NotYet and in no other list; nothing is unmapped.
func m3aCompareClassified(items []sendapi.Item, cl sendapi.Classified, decide func(charge.Outcome) m3aDecision) []string {
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
	inSettle, inNotYet, inUnmapped := map[string]int{}, map[string]int{}, map[string]int{}
	for _, s := range cl.Settle {
		inSettle[s.ItemRef]++
		it, ok := byID[s.ItemRef]
		switch {
		case !ok:
			say("the settle list carries %s, which the module never reported", s.ItemRef)
		case s.Outcome.Module != "m3" || s.Outcome.State != it.State || s.Outcome.Reason != "":
			say("the settle list carries outcome %s/%s/%q for %s, the module reported m3/%s with no reason", s.Outcome.Module, s.Outcome.State, s.Outcome.Reason, s.ItemRef, it.State)
		}
	}
	for _, n := range cl.NotYet {
		inNotYet[n.JobItemID]++
	}
	for _, u := range cl.Unmapped {
		inUnmapped[u.JobItemID]++
	}
	for id, n := range inSettle {
		if n > 1 {
			say("the settle list repeats %s (%d times)", id, n)
		}
	}
	for id, it := range byID {
		hand := m3aHand(it.State)
		d := decide(charge.Outcome{Module: "m3", State: it.State})
		if hand == "free" && (d.Kind != "free" || d.Unit != "") {
			say("item %s (%s): charge.Decide says %s %q (rule %s), the hand oracle says free: module 3 is never charged", id, it.State, d.Kind, d.Unit, d.Rule)
		}
		switch hand {
		case "free":
			if inSettle[id] == 0 {
				say("the settle list lacks the terminal item %s (%s)", id, it.State)
			}
			if inNotYet[id] > 0 || inUnmapped[id] > 0 {
				say("the terminal item %s is also in the not-yet or the unmapped list", id)
			}
		case "notfinal":
			if inSettle[id] > 0 {
				say("the settle list carries %s, which is notfinal (%s)", id, it.State)
			}
			if inNotYet[id] == 0 {
				say("the not-yet list lacks %s (%s)", id, it.State)
			}
		default:
			if inUnmapped[id] == 0 {
				say("the unmapped list lacks %s (%s)", id, it.State)
			}
		}
	}
	for id := range inNotYet {
		if _, ok := byID[id]; !ok {
			say("the not-yet list carries %s, which the module never reported", id)
		}
	}
	sort.Strings(p)
	return p
}

func m3aProblems(t *testing.T, what string, p []string) {
	t.Helper()
	for _, s := range p {
		t.Errorf("%s: %s", what, s)
	}
}

// ---- the keys of the pagination comparisons ----

func m3aRowKey(it sendapi.Item) string {
	return it.JobItemID + "|" + it.State + "|" + m3aText(it.ErrorClass) + "|" + m3aText(it.ProviderMessageID)
}

func m3aKeys(items []sendapi.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = m3aRowKey(it)
	}
	return out
}

func m3aItemIDs(items []sendapi.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.JobItemID
	}
	return out
}

func m3aCountPrefix(ss []string, prefix string) int {
	n := 0
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func m3aSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// m3aLookAheadWalk is the look-ahead bug of a keyset pager, over the adapter's page reader: the walker believes the module read one row more than the page size and that this row is the LAST item of the
// page, so it drops it. Module 3's cursor anchors on the last row KEPT, so every dropped item is lost. The equality check must catch it.
func m3aLookAheadWalk(ctx context.Context, c *sendapi.Client, campaign string) ([]sendapi.Item, error) {
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

// m3aSkipAheadWalk is the other look-ahead bug: to learn whether there is a page after the next one the walker reads the next page, keeps ITS cursor and throws its items away.
func m3aSkipAheadWalk(ctx context.Context, c *sendapi.Client, campaign string) ([]sendapi.Item, error) {
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

// m3aAccountLookAheadWalk is the same defect over the account list: the last account of every page that has a successor is dropped.
func m3aAccountLookAheadWalk(ctx context.Context, c *m3.Client) ([]m3.Account, error) {
	var out []m3.Account
	cursor := ""
	for {
		page, err := c.ListAccounts(ctx, cursor)
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

// ---- running and polling through the adapter ----

func m3aAllTerminal(rep sendapi.CampaignReport, n int) bool {
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

// runAll hands every item of the tenant to module 3 THROUGH THE ADAPTER: each must be a NEW job (202, state queued, class pending) with a job id of its own.
func (e *m3aEnv) runAll(t *testing.T, tn *m3aTenant) time.Duration {
	t.Helper()
	start := time.Now()
	seen := map[string]string{}
	for _, it := range tn.Items {
		res, err := tn.C.Run(e.ctx, it.ID)
		if err != nil {
			t.Fatalf("Run of %s item %02d through the adapter failed (%s): %s", tn.Name, it.Index, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if !res.Created || res.State != "queued" || res.Class != sendapi.ClassPending || res.JobID == "" {
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

// waitCampaign polls the whole campaign report THROUGH THE ADAPTER (every page, in order) every second until module 3 says the campaign is complete AND every item the test submitted is terminal (the
// platform's own submitted count is the completeness, never is_complete alone), or fails at the limit with the last report and the diagnostics. onPoll, when not nil, runs after every poll.
func (e *m3aEnv) waitCampaign(t *testing.T, tn *m3aTenant, limit time.Duration, onPoll func(sendapi.CampaignReport)) sendapi.CampaignReport {
	t.Helper()
	start := time.Now()
	last := ""
	for {
		rep, err := tn.C.ReportCampaign(e.ctx, tn.CampaignID)
		if err != nil {
			t.Fatalf("ReportCampaign of %s through the adapter failed (%s): %s", tn.Name, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if line := m3aTotalsLine(rep.Totals); line != last {
			last = line
			t.Logf("NOTE m3a t+%5.1fs %s: the adapter's campaign report (%d page(s), %d item(s)): %s", time.Since(start).Seconds(), tn.Name, rep.Pages, len(rep.Items), line)
		}
		e.requireNoDups(t, e.sinkView(t))
		if onPoll != nil {
			onPoll(rep)
		}
		if m3aAllTerminal(rep, len(tn.Items)) {
			return rep
		}
		if time.Since(start) > limit {
			t.Fatalf("the campaign of %s did not finish within %s through the adapter (%d item(s) submitted): %s, is_complete %v, items match total %v\n%s", tn.Name, limit, len(tn.Items), m3aTotalsLine(rep.Totals), rep.Progress.IsComplete, rep.ItemsMatchTotal(), e.diagnostics(t, tn))
		}
		select {
		case <-e.ctx.Done():
			t.Fatalf("the test's context ended while waiting for %s: %v", tn.Name, e.ctx.Err())
		case <-time.After(m3aPollEvery):
		}
	}
}

// diagnostics is what a failed wait prints: module 3's own job states, the sink's held sessions and the workers' heartbeats.
func (e *m3aEnv) diagnostics(t *testing.T, tn *m3aTenant) string {
	t.Helper()
	inf := e.inflight(t)
	beat := e.count(t, "count the live workers", "SELECT count(*) FROM send_byo.boxes WHERE status = 'active' AND last_heartbeat_at > now() - interval '30 seconds'")
	return fmt.Sprintf("  diagnostics: module 3's jobs of %s by state: %s; the sink holds %d session(s) (peak %d); %d worker(s) heartbeated in the last 30 s", tn.Name, e.jobStates(e.jobsOf(t, tn.UserID)), inf.Inflight, inf.Peak, beat)
}

// ---- cross-tenant answers ----

// m3aNotFound is "" when err is the module's not-found answer about the named resource field ("" names no field: the account routes answer a bare 404), else a sentence saying what it is.
func m3aNotFound(err error, field string) string {
	if err == nil {
		return "the call succeeded, want the module's not-found answer"
	}
	if k := modclient.KindOf(err); k != modclient.NotFound {
		return fmt.Sprintf("the call failed as %s, want not found", k)
	}
	if field == "" {
		return ""
	}
	if f, ok := sendapi.NotFoundField(err); !ok || f != field {
		return fmt.Sprintf("the not-found answer names the field %q (named: %v), want %q", f, ok, field)
	}
	return ""
}

// m3aLeaks lists the names of the secrets (an id, an address, a host) that appear in text: the test of "nothing of tenant A is in tenant B's answer".
func m3aLeaks(text string, secrets map[string]string) []string {
	var found []string
	for name, v := range secrets {
		if v != "" && strings.Contains(strings.ToLower(text), strings.ToLower(v)) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// m3aSecrets are the ids, addresses and hosts of a tenant that must never appear in another tenant's answer.
func m3aSecrets(tn *m3aTenant, host string) map[string]string {
	out := map[string]string{"user id": tn.UserID, "smtp host": host} // not the campaign id: B asked with it, and the module echoes what it was asked
	for i, a := range tn.Accounts {
		k := fmt.Sprint(i + 1)
		out["account id "+k], out["smtp username "+k], out["from address "+k] = a.ID, a.SMTPUsername, a.FromAddress
	}
	for _, it := range tn.Items {
		k := fmt.Sprint(it.Index)
		out["item id "+k], out["job id "+k], out["address "+k] = it.ID, it.JobID, it.Email
	}
	return out
}

// ---- the outage assertions ----

// m3aDownProblems judges ONE call made while the module is down: it must have failed, with a typed modclient error of kind Unavailable or Timeout, and it must have returned no value. It returns problems so
// that the same judge can be run on a fake that answers with a value, with no error, or with another kind of error, and be shown to report each.
func m3aDownProblems(call string, err error, zero bool) []string {
	var p []string
	if err == nil {
		p = append(p, call+": no error while the module is down (an invented answer)")
	} else if k := modclient.KindOf(err); k != modclient.Unavailable && k != modclient.Timeout {
		p = append(p, fmt.Sprintf("%s: the error is of kind %s, want unavailable or timeout", call, k))
	}
	if !zero {
		p = append(p, call+": a value came back beside the error")
	}
	return p
}

func m3aIsZero[T any](v T) bool {
	var z T
	return reflect.DeepEqual(v, z)
}

func m3aKindWord(err error) string {
	var e *modclient.Error
	if modclient.KindOf(err) == 0 {
		return "not a modclient error"
	}
	if errors.As(err, &e) {
		return fmt.Sprintf("%s (%s; request sent %v)", e.Kind, e.Message, e.Sent)
	}
	return modclient.KindOf(err).String()
}
