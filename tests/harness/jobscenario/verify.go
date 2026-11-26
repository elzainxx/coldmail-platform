//go:build harness

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	m1 "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// verifyOKs are the names of the invariants verify prints as `OK <name>`, in order. The stage (tests/harness/jobs/stage.sh, JOBS_VERIFY_OKS) requires every one of them, exactly once, in the output of
// every verify it runs: a program that printed nothing, or fewer lines than this, fails the stage. tests/harness/jobs/stage-selftest.sh holds the two lists equal.
var verifyOKs = []string{
	"rows_equal_results",
	"every_pending_row_has_one_result",
	"results_name_their_rows",
	"job_succeeded",
	"one_charge_per_item",
	"charges_equal_results_with_a_unit",
	"settlement_drift_empty",
	"balance_drift_empty",
	"reserve_closed",
	"one_release_per_reserve",
	"module_report_covers_the_pending_rows",
	"module_report_equals_results",
	"module_report_equals_ledger",
	"module_holds_the_job_once",
	"module_item_count_equals_pending",
	"events_legal_and_once_each",
	"proof_not_vacuous",
	"control_checks_detect_corruption",
}

// verdict collects what verify found: an OK line per property that holds, a FAIL line (on stderr) per property that does not, and the exit status.
type verdict struct {
	ok     map[string]bool
	failed []string
}

func (v *verdict) check(name string, problems []string) {
	if v.ok == nil {
		v.ok = map[string]bool{}
	}
	if len(problems) == 0 {
		v.ok[name] = true
		fmt.Fprintf(stdout, "OK %s\n", name)
		return
	}
	for _, p := range problems {
		v.failed = append(v.failed, name+": "+p)
		fmt.Fprintf(os.Stderr, "FAIL %s: %s\n", name, p)
	}
}

func cmdVerify(args []string) error {
	fs := flags("verify")
	var job, path string
	fs.StringVar(&job, "job", "", "the finished job")
	fs.StringVar(&path, "path", pathNormal, "a send job's path: normal, pause (paused while running, then resumed) or cancel (cancelled while running)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if job == "" {
		return failf(exitUsage, "--job is required")
	}
	if _, ok := sendPaths[path]; !ok {
		return failf(exitUsage, "--path is normal, pause or cancel (got %q)", path)
	}
	ctx, cancel := ctxFor(10 * time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	h, err := d.head(ctx, job) // a job that does not exist ends here, naming it: the program's own control
	if err != nil {
		return err
	}
	if h.Module != "m1" {
		return verifySend(ctx, d, h, path) // modules 2 and 3: the senders' invariants, with the sink (send_verify.go)
	}
	if path != pathNormal {
		return failf(exitUsage, "--path %s is a path of a send job; job %s is a module 1 job", path, job)
	}
	var v verdict

	rows, results, charges, err := d.loadJob(ctx, job)
	if err != nil {
		return err
	}
	byRef := map[string]resultInfo{}
	for _, r := range results {
		byRef[r.Ref] = r
	}
	pending := map[string]rowInfo{}
	for _, w := range rows {
		if w.State == "pending" {
			pending[w.ID] = w
		}
	}

	// (a) the rows and their results
	v.check("rows_equal_results", rowsEqualResults(rows, results))
	v.check("every_pending_row_has_one_result", everyPendingRowHasOneResult(rows, results))
	v.check("results_name_their_rows", resultsNameTheirRows(rows, results))

	// (b) the job
	var jp []string
	if h.Status != "succeeded" {
		jp = append(jp, fmt.Sprintf("the job is %s, want succeeded", h.Status))
	}
	if !h.Confirmed || !h.Started || !h.Finished {
		jp = append(jp, fmt.Sprintf("confirmed_at, started_at and finished_at set: %t, %t, %t, want all three", h.Confirmed, h.Started, h.Finished))
	}
	v.check("job_succeeded", jp)

	// (c) the ledger
	v.check("one_charge_per_item", atMostOneChargePerItem(charges))
	v.check("charges_equal_results_with_a_unit", chargesEqualResultsWithAUnit(charges, results))
	sd, err := d.driftRows(ctx, `SELECT job_id::text || ' ' || COALESCE(item_ref, '-') || ' ' || problem FROM platform.job_settlement_drift ORDER BY 1 LIMIT 6`)
	if err != nil {
		return err
	}
	v.check("settlement_drift_empty", prefixed("platform.job_settlement_drift is not empty: ", sd))
	bd, err := d.driftRows(ctx, `SELECT user_id::text || ' ' || module_id || ' stored ' || stored_available || '/' || stored_reserved || ' ledger ' || ledger_available || '/' || ledger_reserved FROM platform.balance_drift ORDER BY 1 LIMIT 6`)
	if err != nil {
		return err
	}
	v.check("balance_drift_empty", prefixed("platform.balance_drift is not empty: ", bd))
	rf, err := d.reserveFactsOf(ctx, job, h.User, "m1")
	if err != nil {
		return err
	}
	v.check("reserve_closed", reserveClosed(rf))
	v.check("one_release_per_reserve", oneReleasePerReserve(rf))

	// (d) module 1's own report, three ways. A report that could not be read fails all three checks that need it, naming why: nothing is compared with an empty list and called equal.
	report, rp := moduleReport(ctx, h, len(pending))
	if len(report) == 0 && len(pending) > 0 {
		for _, name := range []string{"module_report_covers_the_pending_rows", "module_report_equals_results", "module_report_equals_ledger"} {
			v.check(name, append([]string{"module 1's report could not be read"}, rp...))
		}
	} else {
		v.check("module_report_covers_the_pending_rows", append(append([]string{}, rp...), reportCovers(report, pending)...))
		v.check("module_report_equals_results", reportEqualsResults(report, byRef))
		v.check("module_report_equals_ledger", reportEqualsLedger(report, charges))
	}

	// (e) module 1 holds the job once
	once, count, err := d.moduleHolding(ctx, h, len(pending))
	if err != nil {
		return err
	}
	v.check("module_holds_the_job_once", once)
	v.check("module_item_count_equals_pending", count)

	// (f) the events
	evs, err := d.events(ctx, job)
	if err != nil {
		return err
	}
	allowed, err := d.allowedHops(ctx)
	if err != nil {
		return err
	}
	v.check("events_legal_and_once_each", eventsLegalAndOnceEach(evs, allowed, h.Status))

	// the proof is not empty: it holds charges (a double charge needs one to be seen), and the module answered every pending row
	var charged, freeModule, unmapped int
	dist := map[string]int{}
	for _, r := range results {
		if r.Origin != "module" {
			continue
		}
		dist[r.State+"/"+r.Reason+" "+r.Decision]++
		switch r.Decision {
		case "charge":
			charged++
		case "free":
			freeModule++
		default:
			unmapped++
		}
	}
	var np []string
	if charged < 1 {
		np = append(np, "no item of the job was charged: the ledger half of every check above holds of an empty set (module 1 was not given the scope's DNS fixture, or answered nothing a charge rule prices)")
	}
	if len(pending) < 1 || charged+freeModule+unmapped != len(pending) {
		np = append(np, fmt.Sprintf("the module answered %d of the %d pending rows", charged+freeModule+unmapped, len(pending)))
	}
	v.check("proof_not_vacuous", np)

	// the control: every check above can go red
	v.check("control_checks_detect_corruption", controlDetectsCorruption())

	keys := make([]string, 0, len(dist))
	for k := range dist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, dist[k]))
	}
	fact("verify", "job", job, "ok", len(v.ok), "failed", len(v.failed), "rows", len(rows), "pending", len(pending), "results", len(results), "charged", charged, "free_module", freeModule,
		"unmapped", unmapped, "charge_rows", len(charges), "events", len(evs), "answers", strings.Join(parts, "; "))
	if len(v.failed) > 0 {
		return failf(exitFail, "job %s: %d of %d properties failed: %s", job, len(v.failed), len(verifyOKs), strings.Join(failedNames(v.failed), ", "))
	}
	return nil
}

func prefixed(prefix string, lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	return []string{prefix + strings.Join(lines, " | ")}
}

func failedNames(failed []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range failed {
		n, _, _ := strings.Cut(f, ":")
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// ---- reading the job --------------------------------------------------------------------------------------------------------------------------------------------------------------------

func (d *db) loadJob(ctx context.Context, job string) ([]rowInfo, []resultInfo, []chargeInfo, error) {
	var rows []rowInfo
	rs, err := d.c.Query(ctx, `SELECT id::text, ordinal, state, COALESCE(email_norm, email_raw) FROM platform.job_rows WHERE job_id = $1::uuid ORDER BY ordinal`, job)
	if err != nil {
		return nil, nil, nil, d.scrub(err)
	}
	for rs.Next() {
		var w rowInfo
		if err := rs.Scan(&w.ID, &w.Ordinal, &w.State, &w.Email); err != nil {
			rs.Close()
			return nil, nil, nil, d.scrub(err)
		}
		rows = append(rows, w)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, nil, nil, d.scrub(err)
	}
	var results []resultInfo
	rs, err = d.c.Query(ctx, `SELECT item_ref::text, ordinal, email, origin, module_state, COALESCE(reason, ''), decision, COALESCE(unit, ''), rule FROM platform.job_results WHERE job_id = $1::uuid ORDER BY ordinal`, job)
	if err != nil {
		return nil, nil, nil, d.scrub(err)
	}
	for rs.Next() {
		var r resultInfo
		if err := rs.Scan(&r.Ref, &r.Ordinal, &r.Email, &r.Origin, &r.State, &r.Reason, &r.Decision, &r.Unit, &r.Rule); err != nil {
			rs.Close()
			return nil, nil, nil, d.scrub(err)
		}
		results = append(results, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, nil, nil, d.scrub(err)
	}
	var charges []chargeInfo
	rs, err = d.c.Query(ctx, `SELECT item_ref, unit, units FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge' ORDER BY id`, job)
	if err != nil {
		return nil, nil, nil, d.scrub(err)
	}
	for rs.Next() {
		var c chargeInfo
		if err := rs.Scan(&c.Ref, &c.Unit, &c.Units); err != nil {
			rs.Close()
			return nil, nil, nil, d.scrub(err)
		}
		charges = append(charges, c)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, nil, nil, d.scrub(err)
	}
	return rows, results, charges, nil
}

// driftRows runs a query that selects one text per row of a drift view and returns them.
func (d *db) driftRows(ctx context.Context, q string) ([]string, error) {
	rs, err := d.c.Query(ctx, q)
	if err != nil {
		return nil, d.scrub(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, d.scrub(err)
		}
		out = append(out, s)
	}
	return out, d.scrub(rs.Err())
}

func (d *db) reserveFactsOf(ctx context.Context, job, user, module string) (reserveFacts, error) {
	var f reserveFacts
	err := d.c.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'),
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'release'),
		COALESCE((SELECT sum(available_delta) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'release'), 0)::bigint,
		COALESCE((SELECT reserved FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT charged FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT released FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT sum(available_delta) FROM platform.ledger WHERE user_id = $2::uuid AND module_id = $3 AND kind = 'grant'), 0)::bigint,
		COALESCE((SELECT available FROM platform.balances WHERE user_id = $2::uuid AND module_id = $3), 0),
		COALESCE((SELECT reserved FROM platform.balances WHERE user_id = $2::uuid AND module_id = $3), 0),
		COALESCE((SELECT sum(units::bigint * unit_cost::bigint) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge'), 0)::bigint`, job, user, module).
		Scan(&f.Reserves, &f.Releases, &f.ReleasedCredits, &f.Reserved, &f.Charged, &f.Released, &f.Granted, &f.Available, &f.BalanceReserved, &f.ChargedUnitCostSum)
	if err != nil {
		return reserveFacts{}, d.scrub(err)
	}
	return f, nil
}

// allowedHops is platform.job_transitions as "from>to" -> the actors that may take it.
func (d *db) allowedHops(ctx context.Context) (map[string][]string, error) {
	rs, err := d.c.Query(ctx, `SELECT from_status, to_status, actors FROM platform.job_transitions`)
	if err != nil {
		return nil, d.scrub(err)
	}
	defer rs.Close()
	out := map[string][]string{}
	for rs.Next() {
		var from, to string
		var actors []string
		if err := rs.Scan(&from, &to, &actors); err != nil {
			return nil, d.scrub(err)
		}
		out[from+">"+to] = actors
	}
	if err := rs.Err(); err != nil {
		return nil, d.scrub(err)
	}
	if len(out) == 0 {
		return nil, failf(exitFail, "platform.job_transitions is empty: the legal moves of a job cannot be read (the instrument matched nothing)")
	}
	return out, nil
}

// moduleHolding is (e): module 1 holds ONE job for the platform job. Module 1's idempotency key is the platform job id (the adapter's Submit takes it), scoped to the caller the platform derives for the
// user, so the job is found by that pair; there is no other job of the caller (a fresh user has one job); platform.jobs.module_job_ref names it; and its item count is the pending rows (the platform
// sends no repeat and no malformed line).
func (d *db) moduleHolding(ctx context.Context, h jobHead, pending int) (once, count []string, err error) {
	caller := tenant.M1CallerID(h.User)
	var keyed, all int64
	var held, ref string
	var total int64
	err = d.c.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM verify.jobs WHERE idem_caller_id = $1 AND idempotency_key = $2),
		(SELECT count(*) FROM verify.jobs WHERE idem_caller_id = $1),
		COALESCE((SELECT id::text FROM verify.jobs WHERE idem_caller_id = $1 AND idempotency_key = $2 LIMIT 1), ''),
		COALESCE((SELECT module_job_ref FROM platform.jobs WHERE id = $3::uuid), ''),
		COALESCE((SELECT total_count FROM verify.jobs WHERE idem_caller_id = $1 AND idempotency_key = $2 LIMIT 1), -1)::bigint`, caller, h.ID, h.ID).
		Scan(&keyed, &all, &held, &ref, &total)
	if err != nil {
		return nil, nil, d.scrub(err)
	}
	if keyed != 1 {
		once = append(once, fmt.Sprintf("module 1 holds %s under the idempotency key of the platform job, want exactly one", plural(int(keyed), "job")))
	}
	if all != 1 {
		once = append(once, fmt.Sprintf("module 1 holds %s for the user's caller, want exactly one (a second submit made a second job)", plural(int(all), "job")))
	}
	if held != "" && ref != held {
		once = append(once, fmt.Sprintf("platform.jobs.module_job_ref is %q, the job module 1 holds is %q", ref, held))
	}
	if held == "" {
		once = append(once, "module 1 holds no job under the idempotency key of the platform job")
		return once, []string{"module 1 holds no job, so it has no item count"}, nil
	}
	var items int64
	if err := d.c.QueryRow(ctx, `SELECT count(*) FROM verify.job_rows WHERE job_id = $1::uuid`, held).Scan(&items); err != nil {
		return nil, nil, d.scrub(err)
	}
	if total != int64(pending) {
		count = append(count, fmt.Sprintf("module 1's job counts %d item(s), the platform's job has %d pending row(s)", total, pending))
	}
	if items != int64(pending) {
		count = append(count, fmt.Sprintf("module 1's job holds %d row(s), the platform's job has %d pending row(s)", items, pending))
	}
	return once, count, nil
}

// moduleReport reads module 1's own report of the job with the user's DERIVED key (the platform's secret and key version): the job's status and totals, then every page of its results. It returns the
// rows and the problems it met (a module that cannot be asked, a job it does not call complete, totals that are not the pending count). A module that is down is a problem, not a skip.
func moduleReport(ctx context.Context, h jobHead, pending int) ([]m1.Row, []string) {
	if h.ModuleRef == "" {
		return nil, []string{"platform.jobs.module_job_ref is empty: the platform recorded no module 1 job, so there is no report to compare with"}
	}
	secret, err := env("JOBS_M1_KEY_SECRET")
	if err != nil {
		return nil, []string{err.Error()}
	}
	version := 1
	if v := envOr("JOBS_M1_KEY_VERSION", "1"); v != "1" {
		n, perr := strconv.Atoi(v)
		if perr != nil {
			return nil, []string{"JOBS_M1_KEY_VERSION is not a number"}
		}
		version = n
	}
	derived, err := tenant.DeriveM1KeyVersion(config.Secret(secret), version, h.User)
	if err != nil {
		return nil, []string{"the user's module 1 key cannot be derived: " + err.Error()}
	}
	ad, err := m1.New(envOr("JOBS_M1_URL", "http://m1:8080"), derived, nil, modclient.Options{Timeout: 30 * time.Second, Retries: 2, Backoff: 250 * time.Millisecond})
	if err != nil {
		return nil, []string{"the module 1 client cannot be built: " + err.Error()}
	}
	st, err := ad.Status(ctx, h.ModuleRef)
	if err != nil {
		return nil, []string{"module 1 cannot report the job: " + err.Error()}
	}
	var problems []string
	if !st.Complete {
		problems = append(problems, fmt.Sprintf("module 1 does not call the job complete (status %s, %d of %d settled)", st.Status, st.Totals.Settled, st.Totals.Submitted))
	}
	if st.Totals.Submitted != pending || st.Totals.Settled != pending || st.Totals.Pending != 0 {
		problems = append(problems, fmt.Sprintf("module 1's totals are submitted %d, settled %d, pending %d, want %d, %d, 0", st.Totals.Submitted, st.Totals.Settled, st.Totals.Pending, pending, pending))
	}
	if st.Totals.Duplicates != 0 || st.Totals.Malformed != 0 {
		problems = append(problems, fmt.Sprintf("module 1 counts %d duplicate and %d malformed row(s): the platform sends neither", st.Totals.Duplicates, st.Totals.Malformed))
	}
	var report []m1.Row
	cursor := ""
	for page := 0; ; page++ {
		if page >= 1000 {
			return report, append(problems, "module 1's results listing did not end within 1000 pages")
		}
		rows, next, err := ad.Results(ctx, h.ModuleRef, cursor, m1.MaxPageLimit)
		if err != nil {
			return report, append(problems, "module 1 cannot list the results: "+err.Error())
		}
		report = append(report, rows...)
		if next == "" {
			break
		}
		if next == cursor {
			return report, append(problems, "module 1's results cursor does not advance")
		}
		cursor = next
	}
	if len(report) == 0 && pending > 0 {
		problems = append(problems, "module 1's report is empty")
	}
	return report, problems
}
