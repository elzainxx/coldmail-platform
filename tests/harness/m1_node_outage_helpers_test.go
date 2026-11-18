//go:build harness && m1node

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/norm"
)

// The helpers of the module-1 OUTAGE tests (m1_node_outage_test.go), behind the build tag m1node. Every name starts with m1o so that it cannot clash with another file of this package; the file
// leans on the end-to-end helpers (m1_node_e2e_helpers_test.go: the stack, the tenants, the items, the oracle, the comparisons) and on the money helpers.
//
// THE SHAPE. Three tests, three separate `go test` processes (each in a fresh test container), and between them the stage script stops and starts module 1's control plane (m1n). Nothing
// survives in memory, so the state lives in the database the tests already reach as the superuser: one scratch table, harness_outage.m1, which the first phase creates and nothing ever
// migrates. It holds the tenant, the job ids, the items, the platform's row counts at the moment the module was still up, and the secret from which the tenant's key is derived (a value
// generated for this run, in this stack's throwaway database; never printed).

const (
	m1oEnvStage = "HARNESS_M1_OUTAGE" // set to 1 by the outage sub-stage only (m1n_outage_phase)
	m1oSchema   = "harness_outage"
	m1oFast     = 8  // rows that settle at once and are charged: they prove the first phase has PROGRESS
	m1oSlow     = 20 // rows held by the fixture's DNS silence (2 s each, one name per row, the drainer is sequential): they keep the job in flight for 40 s or more
	m1oGrant    = 500
	m1oZone     = "fixture.test"

	// m1oCallTimeout is the dial of the outage tests' adapters: one attempt may take 5 s, and there are no retries (Options.Retries is 0). The same factory options are used by the three
	// phases, so phase 2 talks to the dead module with an adapter built exactly as phase 1's.
	m1oCallTimeout = 5 * time.Second
	// m1oResumeGrace is how long after the restart a typed Unavailable or Timeout from Status is a transient and not yet a failure.
	m1oResumeGrace = 60 * time.Second
)

func m1oOptions() modclient.Options { return modclient.Options{Timeout: m1oCallTimeout} }

// m1oRequireStage makes a phase refuse to run anywhere but in the outage sub-stage: the ordinary run of the stage excludes these tests by name (-test.skip), and a run that includes them by
// mistake fails here, loudly, before it creates state or touches a module.
func m1oRequireStage(t *testing.T) {
	t.Helper()
	if os.Getenv(m1oEnvStage) != "1" {
		t.Fatalf("this test is one phase of the outage sub-stage (M1N-4b of tests/harness/m1node/m1node.sh), which sets %s=1 and stops and starts module 1 between the phases: it is not part of the ordinary run", m1oEnvStage)
	}
}

// ---- the rows of the job ---------------------------------------------------------------------------------------------------------------------------------------

// m1oBuildItems is the job of the outage: m1oFast addresses at the NXDOMAIN recipe (DNS only, settled in tens of milliseconds, charged m1_own), one repeat of the first of them (free, P-07),
// one malformed address (never sent, free, P-08) and m1oSlow addresses that each sit at a name of their own which the fixture's DNS never answers (unknown/dns_error, failure origin own:
// free). No row needs the node, so the verdicts do not depend on the node's state, which module 1's restart resets to provisioning. It returns the names to be put into silence.
func m1oBuildItems(e *m1nEnv) ([]m1eItem, []string) {
	noMX, silent := m1eRecipe(e, "no_mx"), m1eRecipe(e, "dns_silent_own")
	var items []m1eItem
	first := -1
	for i := 0; i < m1oFast; i++ {
		idx := m1eAddItem(&items, m1eNewRecipeItem(noMX, fresh("e2eo")))
		if first < 0 {
			first = idx
		}
	}
	m1eAddItem(&items, m1eNewDuplicate(items[first], "duplicate_of_first", strings.ToUpper(items[first].address)))
	m1eAddItem(&items, m1eNewMalformed(norm.ReasonNoAt, "not-an-address-outage"))
	var names []string
	for i := 0; i < m1oSlow; i++ {
		name := fmt.Sprintf("outslow%02d.%s", i, m1oZone)
		names = append(names, name)
		local := fresh("e2eo")
		it := m1eNewRecipeItem(silent, local)
		it.address = local + "@" + name
		m1eAddItem(&items, it)
	}
	return items, names
}

func m1oInputs(items []m1eItem) []m1ad.Input {
	in := make([]m1ad.Input, len(items))
	for i, it := range items {
		in[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	return in
}

// m1oItemJSON is a row as the state table keeps it (m1eItem has unexported fields).
type m1oItemJSON struct {
	Ref, Address, Kind, Label, Malform string
	Status, Reason, Origin, Skip       string
	Cache, Reaches                     bool
	FirstRef, Unit, Rule               string
}

func m1oItemsToJSON(items []m1eItem) []m1oItemJSON {
	out := make([]m1oItemJSON, len(items))
	for i, it := range items {
		out[i] = m1oItemJSON{Ref: it.ref, Address: it.address, Kind: it.kind, Label: it.label, Malform: string(it.malform),
			Status: it.want.status, Reason: it.want.reason, Origin: it.want.origin, Skip: it.want.skip, Cache: it.want.cache,
			Reaches: it.reaches, FirstRef: it.firstRef, Unit: it.unit, Rule: it.rule}
	}
	return out
}

func m1oItemsFromJSON(js []m1oItemJSON) []m1eItem {
	out := make([]m1eItem, len(js))
	for i, j := range js {
		out[i] = m1eItem{ref: j.Ref, address: j.Address, kind: j.Kind, label: j.Label, malform: norm.Reason(j.Malform),
			want:    m1eVerdict{status: j.Status, reason: j.Reason, origin: j.Origin, skip: j.Skip, cache: j.Cache},
			reaches: j.Reaches, firstRef: j.FirstRef, unit: j.Unit, rule: j.Rule}
	}
	return out
}

// ---- what the platform and the module hold at one moment -------------------------------------------------------------------------------------------------------

// m1oSnapshot is everything the outage proof compares between the phases: the platform's own rows for the tenant (ledger, reserves, charges, public.job_items, the balance) and what the
// module's own tables say of the tenant's jobs and of this job's rows and work items (Terminal = settled or dead).
type m1oSnapshot struct {
	Ledger, Reserves, Charges, JobItems int64
	Available, Reserved                 int64
	ModuleJobs, ModuleRows              int
	Items, Terminal                     int
}

func (s *m1eStack) m1oSnapshot(userID, moduleJob string) m1oSnapshot {
	t := s.t
	t.Helper()
	var sn m1oSnapshot
	read := func(what string, dst any, sql string, args ...any) {
		if err := s.admin.QueryRow(s.ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("reading %s: %v", what, err)
		}
	}
	read("the tenant's ledger rows", &sn.Ledger, `SELECT count(*) FROM platform.ledger WHERE user_id = $1`, userID)
	read("the tenant's reserve rows", &sn.Reserves, `SELECT count(*) FROM platform.reserves WHERE user_id = $1`, userID)
	read("the tenant's charge rows", &sn.Charges, `SELECT count(*) FROM platform.ledger WHERE user_id = $1 AND kind = 'charge'`, userID)
	read("the tenant's public.job_items rows", &sn.JobItems, `SELECT count(*) FROM public.job_items WHERE user_id = $1::uuid`, userID)
	avail, resv, _ := s.mny.bal(userID, "m1")
	sn.Available, sn.Reserved = avail, resv
	err := s.admin.QueryRow(s.ctx, `
		SELECT (SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid),
		       (SELECT count(*) FROM verify.job_rows WHERE job_id = $2::uuid),
		       (SELECT count(*) FROM verify.work_items WHERE job_id = $2::uuid),
		       (SELECT count(*) FROM verify.work_items WHERE job_id = $2::uuid AND status IN ('settled', 'dead'))`, userID, moduleJob).Scan(&sn.ModuleJobs, &sn.ModuleRows, &sn.Items, &sn.Terminal)
	if err != nil {
		t.Fatalf("reading module 1's own tables for the job: %v", err)
	}
	return sn
}

// m1oBooksProblems says how the platform's rows (and the module's job and row counts) now differ from what phase 1 recorded. Nothing may: while the module is down nothing is settled,
// reserved, granted or invented, and nothing is created in the module.
func m1oBooksProblems(before, now m1oSnapshot) []string {
	var p []string
	cmp := func(what string, a, b int64) {
		if a != b {
			p = append(p, fmt.Sprintf("%s is %d, phase 1 recorded %d", what, b, a))
		}
	}
	cmp("the tenant's ledger row count", before.Ledger, now.Ledger)
	cmp("the tenant's reserve row count", before.Reserves, now.Reserves)
	cmp("the tenant's charge row count", before.Charges, now.Charges)
	cmp("the tenant's public.job_items row count", before.JobItems, now.JobItems)
	cmp("the tenant's available balance", before.Available, now.Available)
	cmp("the tenant's reserved balance", before.Reserved, now.Reserved)
	cmp("module 1's job count for the tenant", int64(before.ModuleJobs), int64(now.ModuleJobs))
	cmp("module 1's row count for the job", int64(before.ModuleRows), int64(now.ModuleRows))
	cmp("module 1's work-item count for the job", int64(before.Items), int64(now.Items))
	return p
}

// m1oWhileDownProblems says what the module's own work-item table must show while the control plane is stopped: the job is NOT finished (it was in flight when the module stopped: otherwise
// the outage proves nothing), it has not moved backwards since phase 1, and it does not move: two reads some seconds apart are the same.
func m1oWhileDownProblems(before, a, b m1oSnapshot) []string {
	var p []string
	if a.Items == 0 {
		p = append(p, "module 1's work-item table holds no row for the job")
	}
	if a.Terminal >= a.Items {
		p = append(p, fmt.Sprintf("all %d work item(s) of the job were already settled when the module was stopped: the job finished before the stage could stop the module, so the outage proves nothing (lengthen the slow rows)", a.Items))
	}
	if a.Terminal < before.Terminal {
		p = append(p, fmt.Sprintf("%d work item(s) are settled, phase 1 saw %d: the job went backwards", a.Terminal, before.Terminal))
	}
	if a.Terminal != b.Terminal {
		p = append(p, fmt.Sprintf("the job's settled work items moved from %d to %d while the control plane was stopped: something is still draining it", a.Terminal, b.Terminal))
	}
	return p
}

// ---- the calls against the stopped module -------------------------------------------------------------------------------------------------------------------------

// m1oObs is what one call returned: the error, whether every value beside it was the zero value, whether the request had left (the modclient error's Sent), and, for Submit, whether the
// adapter called the outcome a possible orphan job.
type m1oObs struct {
	name    string
	err     error
	noValue bool
	sent    bool
	orphan  bool
}

func m1oObserve(name string, err error, noValue bool) m1oObs {
	o := m1oObs{name: name, err: err, noValue: noValue}
	var me *modclient.Error
	if errors.As(err, &me) {
		o.sent = me.Sent
	}
	o.orphan = errors.Is(err, m1ad.ErrOrphanPossible)
	return o
}

// m1oDownCalls makes every call the platform polls or writes with, once, against the module, and returns what each gave. plan is a well-formed batch for Submit (it creates nothing when the
// module is down, which phase 3 proves from the module's own tables).
func m1oDownCalls(ctx context.Context, ad *m1ad.Adapter, jobID string, plan m1ad.Plan) []m1oObs {
	var out []m1oObs
	js, err := ad.Status(ctx, jobID)
	out = append(out, m1oObserve("Status", err, js == (m1ad.JobStatus{})))
	rows, next, err := ad.Results(ctx, jobID, "", 0)
	out = append(out, m1oObserve("Results (the default page)", err, rows == nil && next == ""))
	rows, next, err = ad.Results(ctx, jobID, "", 5)
	out = append(out, m1oObserve("Results (limit 5)", err, rows == nil && next == ""))
	rows, next, err = ad.Results(ctx, jobID, "a-cursor-the-module-never-issued", 5)
	out = append(out, m1oObserve("Results (a cursor)", err, rows == nil && next == ""))
	delivered := 0
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	st, err := ad.Drain(dctx, jobID, func(page []m1ad.Row) error { delivered += len(page); return nil })
	cancel()
	out = append(out, m1oObserve("Drain", err, delivered == 0 && st == (m1ad.DrainStats{})))
	id, counts, err := ad.Submit(ctx, plan)
	out = append(out, m1oObserve("Submit", err, id == "" && counts == (m1ad.SubmitCounts{})))
	h, err := ad.Health(ctx)
	out = append(out, m1oObserve("Health", err, h == nil))
	rd, err := ad.Ready(ctx)
	out = append(out, m1oObserve("Ready", err, rd == nil))
	return out
}

// m1oCallProblems is how one call differs from what a stopped module permits: it must return an error, of kind Unavailable or Timeout (a connection refused, a name that no longer resolves
// and a deadline are all those), and NO value beside it (no status, no row, no cursor, no count, no id).
func m1oCallProblems(o m1oObs) []string {
	var p []string
	switch k := modclient.KindOf(o.err); {
	case o.err == nil:
		p = append(p, o.name+": no error: the module is down, so any answer is an invention")
	case k != modclient.Unavailable && k != modclient.Timeout:
		p = append(p, fmt.Sprintf("%s: the error is of kind %v, want unavailable or timeout", o.name, k))
	}
	if !o.noValue {
		p = append(p, o.name+": a value (a status, a row, a cursor, a count or an id) came back beside the error")
	}
	if o.name == "Submit" && o.err != nil && o.orphan != o.sent {
		p = append(p, fmt.Sprintf("Submit: the adapter calls the outcome a possible orphan job (%t) but the request had left (%t): a request that never left is not an orphan, and one that left may be", o.orphan, o.sent))
	}
	return p
}

// m1oHalt is what a polling loop did against the stopped module.
type m1oHalt struct {
	polls    int
	took     time.Duration
	kinds    map[modclient.Kind]int
	last     error
	answered bool
}

// m1oPollUntilHalted is the platform's polling loop in its simplest form: Status every 250 ms with ONE context, and the loop stops when that context does. It returns at the first answer
// (which would mean the module is up).
func m1oPollUntilHalted(ad *m1ad.Adapter, jobID string, budget time.Duration) m1oHalt {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	h := m1oHalt{kinds: map[modclient.Kind]int{}}
	start := time.Now()
	for {
		_, err := ad.Status(ctx, jobID)
		h.polls++
		if err == nil {
			h.answered = true
			break
		}
		h.last = err
		h.kinds[modclient.KindOf(err)]++
		select {
		case <-ctx.Done():
			h.took = time.Since(start)
			return h
		case <-time.After(250 * time.Millisecond):
		}
	}
	h.took = time.Since(start)
	return h
}

func m1oHaltProblems(h m1oHalt, budget time.Duration) []string {
	var p []string
	switch {
	case h.answered:
		p = append(p, "the polling loop got an answer: the module is not down")
	case h.polls < 2:
		p = append(p, fmt.Sprintf("the polling loop made %d poll(s): a loop that gives up at the first error is not polling", h.polls))
	}
	for k, n := range h.kinds {
		if k != modclient.Unavailable && k != modclient.Timeout {
			p = append(p, fmt.Sprintf("%d poll(s) failed with kind %v, want unavailable or timeout", n, k))
		}
	}
	if h.took > budget+3*time.Second {
		p = append(p, fmt.Sprintf("the loop ran %v on a context of %v: it did not stop by its context", h.took.Round(10*time.Millisecond), budget))
	}
	if !h.answered && h.took < budget-500*time.Millisecond {
		p = append(p, fmt.Sprintf("the loop stopped after %v, before its context of %v ended, and by no answer: it stopped for a reason of its own", h.took.Round(10*time.Millisecond), budget))
	}
	return p
}

// m1oDialClass says what a direct TCP connection to the module's address does: "" when it is accepted (the module is up), else a word for why not. The address itself is never printed.
func m1oDialClass(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "the base URL has no host"
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err == nil {
		_ = conn.Close()
		return ""
	}
	switch msg := err.Error(); {
	case strings.Contains(msg, "no such host"):
		return "the name no longer resolves"
	case strings.Contains(msg, "refused"):
		return "the connection is refused"
	case strings.Contains(msg, "timeout"):
		return "the connection times out"
	}
	return "the connection fails"
}

// ---- the resume --------------------------------------------------------------------------------------------------------------------------------------------------------

// m1oResumeVerdict tells a transient from a wrong answer, for one failed poll after the restart: a typed Unavailable or Timeout within the grace is a transient to retry; the same after the
// grace is a module that never came back; any other kind (Refused, NotFound, Contract, Internal, ...) is a wrong answer at once.
func m1oResumeVerdict(err error, sinceRestart time.Duration) string {
	switch k := modclient.KindOf(err); {
	case k == modclient.Unavailable || k == modclient.Timeout:
		if sinceRestart < m1oResumeGrace {
			return "transient"
		}
		return "never came back"
	default:
		return "wrong answer"
	}
}

// m1oWaitResumed polls Status as the platform does until the job is complete. A transient failure is counted and retried within m1oResumeGrace; anything else fails the test.
func (s *m1eStack) m1oWaitResumed(tn *m1eTenant, jobID string, rows int) (m1ad.JobStatus, int, int, time.Duration) {
	s.t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(s.ctx, m1eJobWait)
	defer cancel()
	polls, transient := 0, 0
	for {
		js, err := tn.ad.Status(ctx, jobID)
		polls++
		switch {
		case err == nil && js.Complete:
			if js.Counts.Total != rows || js.Counts.Settled != rows || js.Counts.Pending != 0 {
				s.t.Fatalf("the job is complete but its counts are %+v for %d row(s)", js.Counts, rows)
			}
			return js, polls, transient, time.Since(start)
		case err != nil:
			switch v := m1oResumeVerdict(err, time.Since(start)); v {
			case "transient":
				transient++
			default:
				s.t.Fatalf("Status after the restart gave %s (kind %v) after %d poll(s) and %v: %v", v, modclient.KindOf(err), polls, time.Since(start).Round(100*time.Millisecond), err)
			}
		}
		select {
		case <-ctx.Done():
			s.t.Fatalf("the job was not complete %v after the restart (%d poll(s), %d transient): module 1 last said %+v", m1eJobWait, polls, transient, func() m1ad.JobStatus { js, _ := tn.ad.Status(s.ctx, jobID); return js }())
		case <-time.After(m1nPollEvery):
		}
	}
}

// ---- the state table ---------------------------------------------------------------------------------------------------------------------------------------------------------

// m1oState is what the first phase hands to the next two.
type m1oState struct {
	KeySecret   string
	UserID      string
	PlatformJob string
	ModuleJob   string
	Grant       int64
	Reserve     money.ReserveResult
	Items       []m1oItemJSON
	Counts      m1ad.SubmitCounts
	FxBase      int
	Before      m1oSnapshot    // phase 1: the platform's rows and the module's counts while the module was up
	Progress    m1ad.JobStatus // phase 1: what the adapter's Status said when it stopped waiting
	During      *m1oSnapshot   // phase 2: the same, with the module down
}

// m1oPhaseSentence is why a phase cannot start from the state it found, or "" when it can. have is the phase the state says finished (0 when there is no state).
func m1oPhaseSentence(found bool, have, want int) string {
	switch {
	case !found:
		return fmt.Sprintf("this phase needs the row that phase %d wrote in %s.m1 and found none: the phases run in order (TestM1Node_OutagePhase1Start first), in sub-stage M1N-4b, each in its own process; a phase never skips", want, m1oSchema)
	case have != want:
		return fmt.Sprintf("the state says phase %d finished, but this phase needs phase %d to be the last one: the phases ran twice or out of order", have, want)
	}
	return ""
}

// m1oCreateState creates the scratch schema and table (dropping an earlier run's): phase 1 only. Nothing migrates it; it lives in this stack's throwaway database.
func (s *m1eStack) m1oCreateState() {
	s.t.Helper()
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS ` + m1oSchema,
		`DROP TABLE IF EXISTS ` + m1oSchema + `.m1`,
		`CREATE TABLE ` + m1oSchema + `.m1 (
			id          int PRIMARY KEY CHECK (id = 1),
			phase       int NOT NULL,
			user_id     text NOT NULL,
			platform_job text NOT NULL,
			module_job  text NOT NULL,
			state       jsonb NOT NULL,
			phase1_at   timestamptz,
			phase2_at   timestamptz,
			phase3_at   timestamptz)`,
	} {
		if _, err := s.admin.Exec(s.ctx, q); err != nil {
			s.t.Fatalf("creating the outage state table: %v", err)
		}
	}
}

// m1oSave writes the state and marks the phase finished.
func (s *m1eStack) m1oSave(phase int, st *m1oState) {
	s.t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		s.t.Fatalf("encoding the outage state: %v", err)
	}
	col := fmt.Sprintf("phase%d_at", phase)
	_, err = s.admin.Exec(s.ctx, `INSERT INTO `+m1oSchema+`.m1 (id, phase, user_id, platform_job, module_job, state, `+col+`) VALUES (1, $1, $2, $3, $4, $5::text::jsonb, now())
		ON CONFLICT (id) DO UPDATE SET phase = EXCLUDED.phase, state = EXCLUDED.state, `+col+` = now()`, phase, st.UserID, st.PlatformJob, st.ModuleJob, string(raw))
	if err != nil {
		s.t.Fatalf("saving the outage state after phase %d: %v", phase, err)
	}
}

// m1oLoad reads the state that phase `want` must find (the row the phase before it wrote, which says that phase finished), or fails with a sentence: a phase never skips. It takes a connection and
// not the stack, because the next two phases need the key secret IN the state before they can build the stack.
func m1oLoad(t *testing.T, db *pgx.Conn, want int) *m1oState {
	t.Helper()
	ctx := testCtx(t)
	var present bool
	if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, m1oSchema+".m1").Scan(&present); err != nil {
		t.Fatalf("looking for the outage state table: %v", err)
	}
	var phase int
	var raw string
	found := present
	if present {
		err := db.QueryRow(ctx, `SELECT phase, state::text FROM `+m1oSchema+`.m1 WHERE id = 1`).Scan(&phase, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
		} else if err != nil {
			t.Fatalf("reading the outage state: %v", err)
		}
	}
	if why := m1oPhaseSentence(found, phase, want); why != "" {
		t.Fatal(why)
	}
	var st m1oState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("decoding the outage state: %v", err)
	}
	return &st
}

func m1oKindCounts(m map[modclient.Kind]int) string {
	var parts []string
	for k, n := range m {
		parts = append(parts, fmt.Sprintf("%s:%d", k, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
