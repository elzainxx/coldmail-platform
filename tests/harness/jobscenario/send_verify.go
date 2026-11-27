//go:build harness

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/charge"
)

// verify of a SEND job (module 2 or module 3; scope sendjobs, tests/harness/jobs/send-stage.sh SJ_VERIFY_OKS, which the stage's selftest holds equal to this list). What is compared: the platform's rows,
// results, ledger and events; the module's own campaign report (through the platform's adapter, every page) and its own jobs table; and the controllable sink, which counts every message and every RCPT
// answer per recipient. "Nothing sent twice" is the sink's word, never the module's: at most one message per recipient, exactly one for every item the module reports sent, none for an item it reports
// bounced, failed or cancelled and none for any line the platform ended itself (malformed, a repeat), and the sink's total for the job's recipients equals the number of items the module reports sent.
// Since Phase 5 the job is TEMPLATED (send_content.go) and two more properties are read from the sink's RAW messages (send_render.go): the message each recipient received (subject, HTML part, text part) is
// byte-identical to the pure render of that recipient's row for that job id, which this program computes itself from the database after the run through the platform's own public code
// (rendered_bytes_equal_the_pure_render: the exit criterion that the same job and recipient render the same across retries and a SIGKILL-resume), and every message carries the List-Unsubscribe pair of its own
// item exactly once (unsubscribe_headers_on_every_message).
var sendVerifyOKs = []string{
	"rows_equal_results",
	"every_pending_row_has_one_result",
	"results_name_their_rows",
	"job_ended_as_its_path_says",
	"one_charge_per_item",
	"charges_equal_results_with_a_unit",
	"settlement_drift_empty",
	"balance_drift_empty",
	"reserve_closed",
	"one_release_per_reserve",
	"module_report_covers_the_submitted_rows",
	"module_report_equals_results",
	"module_report_equals_ledger",
	"module_holds_each_item_once",
	"sink_at_most_one_message_per_recipient",
	"sink_one_message_per_sent_item_none_otherwise",
	"sink_total_equals_sent",
	"rendered_bytes_equal_the_pure_render",
	"unsubscribe_headers_on_every_message",
	"events_follow_the_path_once_each",
	"nothing_invented",
	"proof_not_vacuous",
	"control_checks_detect_corruption",
}

// The paths a send job takes in the stage: normal (created, previewed, confirmed, started, completed, succeeded), pause (paused by its user while it ran, then resumed) and cancel (cancelled by its user
// while it ran: the items in flight finish, the rest is cancelled by the module, and the job ends cancelled).
const (
	pathNormal = "normal"
	pathPause  = "pause"
	pathCancel = "cancel"
)

type sendPath struct {
	hops []string
	end  string
}

var sendPaths = map[string]sendPath{
	pathNormal: {hops: normalPath, end: "succeeded"},
	pathPause:  {hops: []string{"draft>previewed", "previewed>confirmed", "confirmed>running", "running>paused", "paused>running", "running>completing", "completing>succeeded"}, end: "succeeded"},
	pathCancel: {hops: []string{"draft>previewed", "previewed>confirmed", "confirmed>running", "running>cancelling", "cancelling>completing", "completing>cancelled"}, end: "cancelled"},
}

// sendItem is one item of the module's campaign report as the checks read it: the platform's job item id, the module's state and reason, whether the state is final, and what the charge table decides.
type sendItem struct {
	ID, State, Reason string
	Terminal          bool
	Decision          charge.Decision
}

func toSendItems(p sendapi.Profile, items []sendapi.Item) []sendItem {
	out := make([]sendItem, 0, len(items))
	for _, it := range items {
		s := sendItem{ID: strings.ToLower(it.JobItemID), State: it.State, Terminal: it.Class == sendapi.ClassTerminal, Decision: charge.Decide(p.Outcome(it))}
		if it.Reason != nil {
			s.Reason = *it.Reason
		}
		out = append(out, s)
	}
	return out
}

func verifySend(ctx context.Context, d *db, h jobHead, path string) error {
	sp := sendPaths[path]
	e, err := loadSendEnv(h.Module)
	if err != nil {
		return err
	}
	linkOf, err := loadLinkMaker() // the platform's own link maker under the instances' secret and public address (send_render.go)
	if err != nil {
		return err
	}
	var v verdict
	rows, results, charges, err := d.loadJob(ctx, h.ID)
	if err != nil {
		return err
	}
	byRef := map[string]resultInfo{}
	for _, r := range results {
		byRef[r.Ref] = r
	}
	pending, byID := map[string]rowInfo{}, map[string]rowInfo{}
	pendAddr := map[string]bool{}
	for _, w := range rows {
		byID[w.ID] = w
		if w.State == "pending" {
			pending[w.ID] = w
			pendAddr[strings.ToLower(w.Email)] = true
		}
	}
	stamped, cancelReq, err := d.sendRowFacts(ctx, h.ID)
	if err != nil {
		return err
	}

	// (a) the rows and their results
	v.check("rows_equal_results", rowsEqualResults(rows, results))
	v.check("every_pending_row_has_one_result", everyPendingRowHasOneResult(rows, results))
	v.check("results_name_their_rows", resultsNameTheirRows(rows, results))

	// (b) the job ended where its path ends
	var jp []string
	if h.Status != sp.end {
		jp = append(jp, fmt.Sprintf("the job is %s, its path (%s) ends %s", h.Status, path, sp.end))
	}
	if !h.Confirmed || !h.Started || !h.Finished {
		jp = append(jp, fmt.Sprintf("confirmed_at, started_at and finished_at set: %t, %t, %t, want all three", h.Confirmed, h.Started, h.Finished))
	}
	if cancelReq != (path == pathCancel) {
		jp = append(jp, fmt.Sprintf("cancel_requested_at set %t on a job of the path %s", cancelReq, path))
	}
	v.check("job_ended_as_its_path_says", jp)

	// (c) the ledger: module 2 is metered (m2_accepted, m2_bounced), module 3 holds no credits at all
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
	var closed, released []string
	if h.Module == "m2" {
		rf, err := d.reserveFactsOf(ctx, h.ID, h.User, "m2")
		if err != nil {
			return err
		}
		closed, released = reserveClosed(rf), oneReleasePerReserve(rf)
	} else {
		if closed, err = d.m3NoMoney(ctx, h.ID, h.User); err != nil {
			return err
		}
		var rel int64
		if err := d.c.QueryRow(ctx, `SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'release'`, h.ID).Scan(&rel); err != nil {
			return d.scrub(err)
		}
		if rel != 0 {
			released = append(released, fmt.Sprintf("module 3 holds no credits, and the ledger holds %s for the job", plural(int(rel), "release row")))
		}
	}
	v.check("reserve_closed", closed)
	v.check("one_release_per_reserve", released)

	// (d) the module's own report, three ways; a report that cannot be read fails every check that needs it, naming why
	var items []sendItem
	var reportErr string
	if c, err := e.client(h.User); err != nil {
		reportErr = err.Error()
	} else if rep, err := c.ReportCampaign(ctx, h.ID); err != nil {
		reportErr = "the module's campaign report cannot be read: " + e.scrub(err.Error())
	} else {
		items = toSendItems(e.profile(), rep.Items)
	}
	needReport := func(name string, ps []string) {
		if reportErr != "" {
			ps = []string{reportErr}
		}
		v.check(name, ps)
	}
	needReport("module_report_covers_the_submitted_rows", sendReportCovers(items, pending, stamped))
	needReport("module_report_equals_results", sendReportEqualsResults(items, byRef))
	needReport("module_report_equals_ledger", sendReportEqualsLedger(items, charges))

	// (e) the module's own jobs table and the shared table of job items
	mj, ji, err := d.moduleJobsOf(ctx, h.Module, h.ID)
	if err != nil {
		return err
	}
	v.check("module_holds_each_item_once", moduleHoldsEachOnce(mj, ji, pending, stamped))

	// (f) the sink, per recipient
	view, err := e.sinkRead(ctx, userPrefix(h.User))
	if err != nil {
		return err
	}
	v.check("sink_at_most_one_message_per_recipient", sinkAtMostOne(view, pendAddr))
	needReport("sink_one_message_per_sent_item_none_otherwise", sinkPerItem(items, byID, view))
	needReport("sink_total_equals_sent", sinkTotalEqualsSent(items, view, pendAddr))

	// (f2) the rendered bytes: the message each recipient received, read from the sink's RAW message, against the pure render of that recipient's row for THIS job id. The expectation is computed here, now, from what the
	// database holds AFTER the run (the job's frozen campaign, the pending rows with their merge data, the item ids): after a kill and a replay the job id and the rows are the same, and nothing is remembered across the kill
	content, rrows, err := d.renderInputs(ctx, h.ID)
	if err != nil {
		return err
	}
	exp, expProblems := expectedRenders(h.ID, content, rrows, linkOf)
	sent, err := e.sinkMessages(ctx, userPrefix(h.User))
	if err != nil {
		return err
	}
	stored, err := d.storedItems(ctx, h.ID)
	if err != nil {
		return err
	}
	v.check("rendered_bytes_equal_the_pure_render", concat(expProblems, renderedBytesEqual(exp, sent), storedEqualsPure(exp, stored)))
	v.check("unsubscribe_headers_on_every_message", unsubscribeHeadersOK(exp, sent))

	// (g) the events
	evs, err := d.events(ctx, h.ID)
	if err != nil {
		return err
	}
	allowed, err := d.allowedHops(ctx)
	if err != nil {
		return err
	}
	v.check("events_follow_the_path_once_each", eventsFollowPath(evs, allowed, h.Status, sp.hops))
	v.check("nothing_invented", nothingInvented(results, evs, rows, path))

	// the proof is not empty
	n := map[string]int{}
	for _, it := range items {
		n[it.State]++
	}
	sinkTotal := 0
	for a, k := range view.Msgs {
		if pendAddr[a] {
			sinkTotal += k
		}
	}
	var np []string
	if len(pending) < 1 {
		np = append(np, "the job has no pending row: nothing was sent, so nothing can have been sent twice")
	}
	if n["sent"] < 1 || sinkTotal < 1 {
		np = append(np, fmt.Sprintf("the module reports %d sent item(s) and the sink holds %d message(s) for the job: the sink half of the proof holds of an empty set", n["sent"], sinkTotal))
	}
	if h.Module == "m2" && len(charges) < 1 {
		np = append(np, "module 2 is metered and the job holds no charge row: the money half of the proof holds of an empty set")
	}
	switch path {
	case pathCancel:
		if n["cancelled"] < 1 {
			np = append(np, "the job was cancelled while it ran and the module cancelled none of its items: the cancel came too late to prove anything")
		}
	default:
		if n["bounced"] < 1 {
			np = append(np, "the module reports no bounced item: the sink was told to reject recipients, and the charge of a bounce (module 2) or its freedom (module 3) was not seen")
		}
	}
	v.check("proof_not_vacuous", np)
	v.check("control_checks_detect_corruption", concat(controlSendDetectsCorruption(), liveRenderControl(exp, sent))) // the fixture's control, and the live one: a byte flipped in THIS job's own expected render

	// the sink's evidence, per recipient (the stage prints it)
	stateOf := map[string]string{}
	for _, it := range items {
		stateOf[it.ID] = it.State
	}
	ord := make([]rowInfo, 0, len(pending))
	for _, w := range pending {
		ord = append(ord, w)
	}
	sort.Slice(ord, func(i, j int) bool { return ord[i].Ordinal < ord[j].Ordinal })
	for _, w := range ord {
		a := strings.ToLower(w.Email)
		c := view.Rcpt[a]
		fact("sink", "job", h.ID, "row", w.Ordinal, "rcpt", a, "module_state", orDash(stateOf[w.ID]), "messages", view.Msgs[a], "rcpt_accepted", c.Accepted, "rcpt_rejected", c.Rejected, "rcpt_deferred", c.Deferred)
	}
	compared, distinct := renderStats(exp, sent)
	fact("rendered", "job", h.ID, "module", h.Module, "compared", compared, "distinct", distinct, "stored", len(stored), "expected", len(exp), "messages", len(sent))
	fact("verify", "job", h.ID, "module", h.Module, "path", path, "status", h.Status, "ok", len(v.ok), "failed", len(v.failed), "rows", len(rows), "pending", len(pending), "results", len(results),
		"sent", n["sent"], "bounced", n["bounced"], "failed_items", n["failed"], "cancelled", n["cancelled"], "charge_rows", len(charges), "sink_messages", sinkTotal, "events", len(evs))
	if len(v.failed) > 0 {
		return failf(exitFail, "send job %s (%s, path %s): %d of %d properties failed: %s", h.ID, h.Module, path, len(v.failed), len(sendVerifyOKs), strings.Join(failedNames(v.failed), ", "))
	}
	return nil
}

// sendRowFacts is the set of the job's rows stamped as handed to the module, and whether a cancel was requested.
func (d *db) sendRowFacts(ctx context.Context, job string) (map[string]bool, bool, error) {
	rs, err := d.c.Query(ctx, `SELECT id::text FROM platform.job_rows WHERE job_id = $1::uuid AND submitted_at IS NOT NULL`, job)
	if err != nil {
		return nil, false, d.scrub(err)
	}
	out := map[string]bool{}
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return nil, false, d.scrub(err)
		}
		out[id] = true
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, false, d.scrub(err)
	}
	var cancelReq bool
	if err := d.c.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&cancelReq); err != nil {
		return nil, false, d.scrub(err)
	}
	return out, cancelReq, nil
}

// m3NoMoney: a module 3 job touches no money at all: no ledger row of the job, no reserve, no module 3 balance of the user.
func (d *db) m3NoMoney(ctx context.Context, job, user string) ([]string, error) {
	var led, res, bal int64
	if err := d.c.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid),
		(SELECT count(*) FROM platform.reserves WHERE job_id = $1::uuid),
		(SELECT count(*) FROM platform.balances WHERE user_id = $2::uuid AND module_id = 'm3')`, job, user).Scan(&led, &res, &bal); err != nil {
		return nil, d.scrub(err)
	}
	var out []string
	if led != 0 || res != 0 || bal != 0 {
		out = append(out, fmt.Sprintf("module 3 is not metered, and the job has %s and %s and the user %s of module 3", plural(int(led), "ledger row"), plural(int(res), "reserve"), plural(int(bal), "balance")))
	}
	return out, nil
}

// moduleJobsOf is the module's own jobs of the campaign (job_item_id -> how many jobs) and the shared table's job items of the campaign (their ids).
func (d *db) moduleJobsOf(ctx context.Context, module, job string) (map[string]int, []string, error) {
	rs, err := d.c.Query(ctx, fmt.Sprintf(`SELECT job_item_id::text, count(*) FROM %s.jobs WHERE campaign_id = $1::uuid GROUP BY 1`, schemaOf(module)), job)
	if err != nil {
		return nil, nil, d.scrub(err)
	}
	mj := map[string]int{}
	for rs.Next() {
		var id string
		var n int
		if err := rs.Scan(&id, &n); err != nil {
			rs.Close()
			return nil, nil, d.scrub(err)
		}
		mj[strings.ToLower(id)] = n
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, nil, d.scrub(err)
	}
	rs, err = d.c.Query(ctx, `SELECT id::text FROM public.job_items WHERE campaign_id = $1::uuid`, job)
	if err != nil {
		return nil, nil, d.scrub(err)
	}
	defer rs.Close()
	var ji []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, nil, d.scrub(err)
		}
		ji = append(ji, strings.ToLower(id))
	}
	return mj, ji, d.scrub(rs.Err())
}

// ---- the pure checks of a send job (each returns the problems it found; none is a pass) -------------------------------------------------------------------------------------------------------

func short(id string) string { return id[:min(8, len(id))] }

// sendReportCovers: the module's report names exactly the rows the platform handed it (stamped pending rows), each once.
func sendReportCovers(items []sendItem, pending map[string]rowInfo, stamped map[string]bool) []string {
	seen := map[string]int{}
	for _, it := range items {
		seen[it.ID]++
	}
	var missing, extra, twice, unstamped []string
	for id, w := range pending {
		switch {
		case stamped[id] && seen[id] == 0:
			missing = append(missing, fmt.Sprintf("%s(ordinal %d)", short(id), w.Ordinal))
		case !stamped[id] && seen[id] > 0:
			unstamped = append(unstamped, short(id))
		}
	}
	for id, k := range seen {
		if _, ok := pending[id]; !ok {
			extra = append(extra, short(id))
		}
		if k > 1 {
			twice = append(twice, short(id))
		}
	}
	var out []string
	if len(missing) > 0 {
		out = append(out, "rows handed to the module that its report does not name: "+sample(missing))
	}
	if len(unstamped) > 0 {
		out = append(out, "items the module reports for rows the platform never stamped as handed over: "+sample(unstamped))
	}
	if len(extra) > 0 {
		out = append(out, "items the module reports that are no pending row of the job (a malformed line or a repeat reached the module): "+sample(extra))
	}
	if len(twice) > 0 {
		out = append(out, "items the module reports more than once: "+sample(twice))
	}
	return out
}

// sendReportEqualsResults: every item of the report is final, and its result is the module's own words (state, reason) under the decision, unit and rule the charge table makes of them.
func sendReportEqualsResults(items []sendItem, results map[string]resultInfo) []string {
	var wrong, none []string
	for _, it := range items {
		res, ok := results[it.ID]
		switch {
		case !it.Terminal || it.Decision.Kind == charge.NotFinal:
			wrong = append(wrong, fmt.Sprintf("%s: the module reports %s, which is not final", short(it.ID), it.State))
		case !ok:
			none = append(none, short(it.ID))
		case res.Origin != "module":
			wrong = append(wrong, fmt.Sprintf("%s: the result is %s's, the module answered it", short(it.ID), res.Origin))
		case res.State != it.State || res.Reason != it.Reason:
			wrong = append(wrong, fmt.Sprintf("%s: the module says %s/%s, the result says %s/%s", short(it.ID), it.State, it.Reason, res.State, res.Reason))
		case res.Decision != it.Decision.Kind.String() || res.Unit != it.Decision.Unit || res.Rule != it.Decision.Rule:
			wrong = append(wrong, fmt.Sprintf("%s: the charge table makes %s/%s/%s of the module's %s, the result says %s/%s/%s", short(it.ID), it.Decision.Kind, nonEmpty(it.Decision.Unit), it.Decision.Rule, it.State,
				res.Decision, nonEmpty(res.Unit), res.Rule))
		}
	}
	var out []string
	if len(none) > 0 {
		out = append(out, "items of the module's report with no result: "+sample(none))
	}
	if len(wrong) > 0 {
		out = append(out, "results that differ from the module's report: "+sample(wrong))
	}
	return out
}

// sendReportEqualsLedger: the ledger charges an item exactly when the charge table charges the module's answer for it, once, in the unit the table names; and nothing the report does not name.
func sendReportEqualsLedger(items []sendItem, charges []chargeInfo) []string {
	by := map[string][]chargeInfo{}
	for _, c := range charges {
		by[c.Ref] = append(by[c.Ref], c)
	}
	known := map[string]bool{}
	var wrong []string
	for _, it := range items {
		known[it.ID] = true
		got := by[it.ID]
		switch {
		case it.Decision.Kind == charge.Charge && len(got) != 1:
			wrong = append(wrong, fmt.Sprintf("%s: the module's %s is a charge of %s and the ledger holds %s", short(it.ID), it.State, it.Decision.Unit, plural(len(got), "charge row")))
		case it.Decision.Kind == charge.Charge && got[0].Unit != it.Decision.Unit:
			wrong = append(wrong, fmt.Sprintf("%s: the table says unit %s, the ledger charged %s", short(it.ID), it.Decision.Unit, got[0].Unit))
		case it.Decision.Kind != charge.Charge && len(got) != 0:
			wrong = append(wrong, fmt.Sprintf("%s: the module's %s is %s and the ledger charged it %d time(s)", short(it.ID), it.State, it.Decision.Kind, len(got)))
		}
	}
	var stray []string
	for ref := range by {
		if !known[ref] {
			stray = append(stray, short(ref))
		}
	}
	var out []string
	if len(wrong) > 0 {
		out = append(out, "the ledger and the module's report disagree: "+sample(wrong))
	}
	if len(stray) > 0 {
		out = append(out, "ledger charges for items the module's report does not name: "+sample(stray))
	}
	return out
}

// moduleHoldsEachOnce: the module's own table holds exactly one job for every row handed to it and none for anything else; the shared table holds exactly the job's pending rows.
func moduleHoldsEachOnce(mj map[string]int, jobItems []string, pending map[string]rowInfo, stamped map[string]bool) []string {
	var twice, none, extra []string
	for id := range pending {
		switch k := mj[id]; {
		case stamped[id] && k == 0:
			none = append(none, short(id))
		case k > 1:
			twice = append(twice, fmt.Sprintf("%s x%d", short(id), k))
		}
	}
	for id := range mj {
		if _, ok := pending[id]; !ok {
			extra = append(extra, short(id))
		}
	}
	var out []string
	if len(twice) > 0 {
		out = append(out, "rows the module holds more than one job for (a second run made a second job): "+sample(twice))
	}
	if len(none) > 0 {
		out = append(out, "rows handed to the module that its own table holds no job for: "+sample(none))
	}
	if len(extra) > 0 {
		out = append(out, "module jobs of the campaign for no pending row of the job: "+sample(extra))
	}
	items := map[string]bool{}
	for _, id := range jobItems {
		items[id] = true
	}
	var missingItem, strayItem []string
	for id := range pending {
		if !items[id] {
			missingItem = append(missingItem, short(id))
		}
	}
	for id := range items {
		if _, ok := pending[id]; !ok {
			strayItem = append(strayItem, short(id))
		}
	}
	if len(missingItem) > 0 || len(strayItem) > 0 || len(jobItems) != len(items) {
		out = append(out, fmt.Sprintf("public.job_items holds %d row(s) of the campaign, want exactly the %d pending rows: missing %s, not pending %s", len(jobItems), len(pending), sample(missingItem), sample(strayItem)))
	}
	return out
}

// sinkAtMostOne: no recipient of the job has more than one message, no message went to an address of the user's prefix that is no pending row of the job (a malformed line, a repeat in another spelling,
// a recipient of another job), and no Message-ID was seen twice or not at all.
func sinkAtMostOne(v sinkView, pendAddr map[string]bool) []string {
	var many, foreign []string
	for a, n := range v.Msgs {
		switch {
		case !pendAddr[a]:
			foreign = append(foreign, fmt.Sprintf("%s x%d", a, n))
		case n > 1:
			many = append(many, fmt.Sprintf("%s x%d", a, n))
		}
	}
	var out []string
	if len(many) > 0 {
		out = append(out, "recipients the sink holds more than one message for (something was SENT TWICE): "+sample(many))
	}
	if len(foreign) > 0 {
		out = append(out, "messages to addresses of the user that are no pending row of the job: "+sample(foreign))
	}
	if len(v.DupIDs) > 0 {
		out = append(out, "Message-IDs the sink saw twice: "+sample(append([]string(nil), v.DupIDs...)))
	}
	if v.NoID > 0 {
		out = append(out, fmt.Sprintf("%s without a Message-ID", plural(v.NoID, "message")))
	}
	return out
}

// sinkPerItem: per item of the module's report, the sink holds what its state says: sent, one message and one accepted RCPT; bounced, no message and no accepted RCPT (the sink rejected it); failed, no
// message; cancelled, nothing at all (not even an RCPT TO).
func sinkPerItem(items []sendItem, byID map[string]rowInfo, v sinkView) []string {
	var wrong []string
	for _, it := range items {
		w, ok := byID[it.ID]
		if !ok {
			continue // sendReportCovers names it
		}
		a := strings.ToLower(w.Email)
		n, c := v.Msgs[a], v.Rcpt[a]
		switch it.State {
		case "sent":
			if n != 1 || c.Accepted != 1 {
				wrong = append(wrong, fmt.Sprintf("%s is reported sent: the sink holds %d message(s) and accepted RCPT %d time(s), want 1 and 1", a, n, c.Accepted))
			}
		case "bounced":
			if n != 0 || c.Accepted != 0 || c.Rejected < 1 {
				wrong = append(wrong, fmt.Sprintf("%s is reported bounced: the sink holds %d message(s), RCPT accepted %d rejected %d, want 0, 0 and at least 1", a, n, c.Accepted, c.Rejected))
			}
		case "failed":
			if n != 0 {
				wrong = append(wrong, fmt.Sprintf("%s is reported failed: the sink holds %d message(s), want 0", a, n))
			}
		case "cancelled":
			if n != 0 || c.tried() != 0 {
				wrong = append(wrong, fmt.Sprintf("%s is reported cancelled: the sink holds %d message(s) and was asked RCPT TO %d time(s), want nothing at all", a, n, c.tried()))
			}
		default:
			wrong = append(wrong, fmt.Sprintf("%s is reported %s, which has no sink expectation at the end of a job", a, it.State))
		}
	}
	if len(wrong) > 0 {
		return []string{"the sink and the module's report disagree: " + sample(wrong)}
	}
	return nil
}

// sinkTotalEqualsSent: the messages the sink holds for the job's recipients are exactly as many as the items the module reports sent.
func sinkTotalEqualsSent(items []sendItem, v sinkView, pendAddr map[string]bool) []string {
	sent, total := 0, 0
	for _, it := range items {
		if it.State == "sent" {
			sent++
		}
	}
	for a, n := range v.Msgs {
		if pendAddr[a] {
			total += n
		}
	}
	if sent != total {
		return []string{fmt.Sprintf("the sink holds %d message(s) for the job's recipients, the module reports %d item(s) sent", total, sent)}
	}
	return nil
}

// nothingInvented: no result is an infrastructure failure or unmapped, no pending row has the platform's own result (except, for a job cancelled while it ran, the free P-01 of a row never handed over),
// and the engine raised no `flag` (it flags what it doubts: a module that says complete with rows unrecorded, a refusal of rows it holds, a pause the module cannot honour).
func nothingInvented(results []resultInfo, evs []event, rows []rowInfo, path string) []string {
	state := map[string]string{}
	for _, w := range rows {
		state[w.ID] = w.State
	}
	var bad []string
	for _, r := range results {
		switch {
		case r.State == "infrastructure_failure" || r.Decision == "unmapped":
			bad = append(bad, fmt.Sprintf("%s: %s/%s", short(r.Ref), r.State, r.Decision))
		case state[r.Ref] == "pending" && r.Origin != "module" && !(path == pathCancel && r.State == "cancelled" && r.Rule == "P-01"):
			bad = append(bad, fmt.Sprintf("%s: a pending row ended by the platform as %s", short(r.Ref), r.State))
		}
	}
	flags := 0
	for _, e := range evs {
		if e.Kind == "flag" {
			flags++
		}
	}
	var out []string
	if len(bad) > 0 {
		out = append(out, "results the module never gave: "+sample(bad))
	}
	if flags > 0 {
		out = append(out, fmt.Sprintf("the engine raised %s on the job", plural(flags, "flag event")))
	}
	return out
}

// controlSendDetectsCorruption builds a small consistent send job of module 2 (two recipients, one sent and one bounced, a malformed line and a repeat), proves every check passes on it, then corrupts it one
// way at a time and requires the check that guards that property to say so.
func controlSendDetectsCorruption() []string {
	var failed []string
	id := func(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000000000", n) }
	a, b := "sj-control-01@example.test", "sj-control-02@example.test"
	rows := []rowInfo{
		{ID: id(1), Ordinal: 1, State: "pending", Email: a},
		{ID: id(2), Ordinal: 2, State: "malformed", Email: "sj-control-no-at-sign"},
		{ID: id(3), Ordinal: 3, State: "pending", Email: b},
		{ID: id(4), Ordinal: 4, State: "duplicate", Email: a},
	}
	sent := sendItem{ID: id(1), State: "sent", Terminal: true, Decision: charge.Decide(charge.Outcome{Module: "m2", State: "sent"})}
	bounced := sendItem{ID: id(3), State: "bounced", Terminal: true, Decision: charge.Decide(charge.Outcome{Module: "m2", State: "bounced"})}
	items := []sendItem{sent, bounced}
	results := []resultInfo{
		{Ref: id(1), Email: a, Origin: "module", State: "sent", Decision: "charge", Unit: sent.Decision.Unit, Rule: sent.Decision.Rule, Ordinal: 1},
		{Ref: id(2), Email: "sj-control-no-at-sign", Origin: "platform", State: "malformed", Decision: "free", Rule: "P-08", Ordinal: 2},
		{Ref: id(3), Email: b, Origin: "module", State: "bounced", Decision: "charge", Unit: bounced.Decision.Unit, Rule: bounced.Decision.Rule, Ordinal: 3},
		{Ref: id(4), Email: a, Origin: "platform", State: "duplicate", Decision: "free", Rule: "P-07", Ordinal: 4},
	}
	charges := []chargeInfo{{Ref: id(1), Unit: sent.Decision.Unit, Units: 1}, {Ref: id(3), Unit: bounced.Decision.Unit, Units: 1}}
	pending := map[string]rowInfo{id(1): rows[0], id(3): rows[2]}
	stamped := map[string]bool{id(1): true, id(3): true}
	byID := map[string]rowInfo{}
	for _, w := range rows {
		byID[w.ID] = w
	}
	byRef := map[string]resultInfo{}
	for _, r := range results {
		byRef[r.Ref] = r
	}
	pendAddr := map[string]bool{a: true, b: true}
	view := func() sinkView {
		return sinkView{Msgs: map[string]int{a: 1}, Rcpt: map[string]sinkRcpt{a: {Accepted: 1}, b: {Rejected: 1}}}
	}
	mj := map[string]int{id(1): 1, id(3): 1}
	ji := []string{id(1), id(3)}
	if sent.Decision.Kind != charge.Charge || bounced.Decision.Kind != charge.Charge || sent.Decision.Unit == "" || bounced.Decision.Unit == "" {
		failed = append(failed, "the charge table does not charge module 2's sent and bounced items: the control fixture is not what the table says")
	}

	good := map[string][]string{
		"sendReportCovers": sendReportCovers(items, pending, stamped), "sendReportEqualsResults": sendReportEqualsResults(items, byRef), "sendReportEqualsLedger": sendReportEqualsLedger(items, charges),
		"moduleHoldsEachOnce": moduleHoldsEachOnce(mj, ji, pending, stamped), "sinkAtMostOne": sinkAtMostOne(view(), pendAddr), "sinkPerItem": sinkPerItem(items, byID, view()),
		"sinkTotalEqualsSent": sinkTotalEqualsSent(items, view(), pendAddr), "nothingInvented": nothingInvented(results, nil, rows, pathNormal),
	}
	for name, ps := range good {
		if len(ps) != 0 {
			failed = append(failed, fmt.Sprintf("the check %s found a problem in the GOOD fixture: %s", name, strings.Join(ps, "; ")))
		}
	}
	expect := func(what string, ps []string) {
		if len(ps) == 0 {
			failed = append(failed, "the corruption "+what+" was NOT found")
		}
	}
	twice := view()
	twice.Msgs[a] = 2
	expect("a recipient with two messages (sent twice)", sinkAtMostOne(twice, pendAddr))
	expect("... and the sink's total above the sent items", sinkTotalEqualsSent(items, twice, pendAddr))
	expect("... and a sent item with two messages", sinkPerItem(items, byID, twice))
	none := view()
	delete(none.Msgs, a)
	expect("a sent item the sink never saw", sinkPerItem(items, byID, none))
	toBounced := view()
	toBounced.Msgs[b] = 1
	expect("a bounced item that reached the sink", sinkPerItem(items, byID, toBounced))
	foreign := view()
	foreign.Msgs["sj-control-no-at-sign-000.example.test"] = 1
	expect("a message to an address that is no pending row (a malformed line sent)", sinkAtMostOne(foreign, pendAddr))
	dupID := view()
	dupID.DupIDs = []string{"<id@example.test>"}
	expect("a Message-ID seen twice", sinkAtMostOne(dupID, pendAddr))
	cancelled := []sendItem{sent, {ID: id(3), State: "cancelled", Terminal: true, Decision: charge.Decide(charge.Outcome{Module: "m2", State: "cancelled"})}}
	expect("a cancelled item the sink was asked about", sinkPerItem(cancelled, byID, view()))
	expect("the report loses a row the module was handed", sendReportCovers(items[:1], pending, stamped))
	expect("the report names a repeat (a duplicate reached the module)", sendReportCovers(append(append([]sendItem{}, items...), sendItem{ID: id(4), State: "sent", Terminal: true}), pending, stamped))
	flipped := []sendItem{sent, {ID: id(3), State: "sent", Terminal: true, Decision: sent.Decision}}
	expect("the module's state differs from the result's", sendReportEqualsResults(flipped, byRef))
	notFinal := []sendItem{sent, {ID: id(3), State: "queued", Terminal: false, Decision: charge.Decide(charge.Outcome{Module: "m2", State: "queued"})}}
	expect("an item that is not final at the end", sendReportEqualsResults(notFinal, byRef))
	expect("a charge the ledger lacks", sendReportEqualsLedger(items, charges[:1]))
	expect("an item charged twice", sendReportEqualsLedger(items, append(append([]chargeInfo{}, charges...), charges[0])))
	expect("the module holds two jobs for one row", moduleHoldsEachOnce(map[string]int{id(1): 2, id(3): 1}, ji, pending, stamped))
	expect("the module holds a job for a repeat", moduleHoldsEachOnce(map[string]int{id(1): 1, id(3): 1, id(4): 1}, ji, pending, stamped))
	expect("the shared table holds an item of a repeat", moduleHoldsEachOnce(mj, append(append([]string{}, ji...), id(4)), pending, stamped))
	invented := append([]resultInfo{}, results...)
	invented[2] = resultInfo{Ref: id(3), Email: b, Origin: "platform", State: "infrastructure_failure", Decision: "free", Rule: "P-04", Ordinal: 3}
	expect("an invented outcome", nothingInvented(invented, nil, rows, pathNormal))
	expect("a flag the engine raised", nothingInvented(results, []event{{ID: 9, Kind: "flag"}}, rows, pathNormal))
	failed = append(failed, controlRenderDetectsCorruption()...) // the rendered bytes, the spin and the List-Unsubscribe pair (send_render.go): each corruption is found for its own reason
	sort.Strings(failed)
	return failed
}
