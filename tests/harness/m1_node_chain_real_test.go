//go:build harness && m1node && m2worker

package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// TestM1Node_ChainRealVerdictsIntoModule2 (Phase 4 close-out, D4): a chain driven by REAL verdicts into a REAL send. A verify job runs through the platform's job engine (this process: the production registry
// over the production adapters, a real store on the live database) against module 1's NODE-WIRED control plane (`m1n`: its real verifier node, the fixtures, the recipe table of m1_node_test.go), SUCCEEDS,
// and send jobs chained on it (CreateReq.SourceJobID) run through the same engine against module 2's REAL Control, its REAL worker and the controllable sink. The old chain test
// (TestJobsLive_Chain_ExactlySelectedVerdictsMove) ran against the main stack's module 1, which has no node and can only say `unknown`; this test is the one that shows the other three classes moving, and not
// moving. It exists only in the full run (module 2 is not part of scope m1node): the stage that runs it is M1N-4c of tests/harness/m1node/m1node.sh, which prints that it was NOT run when module 2 is absent.
//
// THE LISTS (rchSourceSpecs, written by hand in m1_node_chain_checks_test.go): the source holds two addresses of each verdict class, valid, risky, invalid and unknown, taken from the shared recipe table
// (which address gets which answer from the real node); the second source holds a valid, a risky and an invalid address and NO unknown one. Every address carries this run's prefix (so that the sink's counters
// are read by it) and was never verified or sent to before.
//
// THE STEPS, each with its control (the control of a step is the same check made to fail on a copy or on the other choice):
//
//	1  the source    the verify job succeeds; its results carry the verdicts the recipe table gives, three ways equal: the ledger, the platform's results and module 1's OWN report read with the user's key
//	                 (jlM1ThreeWay), and the hand-written class of every address (rchVerdictProblems); the fixture saw the node's two RCPTs for each address that reaches it and none for the others
//	                 (m1eCheckProbes), so the verdicts are the node's and not a stand-in's. Controls: jlM1Controls, the probe check against an address nobody verified, rchVerdictProblems on tampered copies
//	2  exclude       a chain of that source: the preview counts every verdict and requires a choice; the confirm with NO choice is refused (ErrUnknownChoiceRequired with the unknown count) and writes nothing
//	                 (rchNothingProblems: still previewed, no reserve, no result, no module row, the slot free, the balance as it was); previewed and confirmed under `exclude` it sends EXACTLY the valid and risky
//	                 addresses: the sink gained one message and one accepted RCPT TO for each of them and NOTHING for an invalid or an unknown one (not even a RCPT TO), module 2's own report, its job table and
//	                 public.job_items hold only the selected rows, the excluded rows have the platform's free P-EXC results, and the books close (jlM2ThreeWay over the report, the results, the ledger and the sink)
//	3  include       a second chain of the SAME source under `include`: the selected addresses and the unknown ones, never an invalid one. Control: each chain's delivery judged by the OTHER choice must be refused
//	4  no unknown    a second source with no unknown row: its chain confirms with NO choice and sends its valid and risky addresses (the control of the refusal in step 2)
//	5  the source    the first source's results are byte for byte what they were before the chains (a digest of every column, the database's own md5, the status, the ledger and the balance)
//	6  the end       over all three sends the sink holds as many messages per address as the chains that selected it, and module 2's own tables as many rows and jobs; an invalid address is present in
//	                 its source and absent at the sink and at module 2, and the same readers DO see the valid ones (so a reader that saw nothing would not pass)
func TestM1Node_ChainRealVerdictsIntoModule2(t *testing.T) {
	m := newJlM2Env(t) // module 2's run: FAILS naming a missing variable, a worker that is not alive or a sink that is not clean; the dials of module 2 are set and restored
	e := newM1nEnv(t)
	s, _ := newJlStackWith(t, m.r.ctx, jlOpts{Cfg: func(config.Secret) config.Config {
		return config.Config{
			M1URL:              m3lBaseURL(t, m1bEnvURL), // module 1's NODE-WIRED control plane (m1n)
			M2URL:              m.r.m2URL,
			M2ServiceToken:     m.r.token,
			ModuleTimeout:      20 * time.Second,
			ModuleRetries:      2,
			ModuleRetryBackoff: 250 * time.Millisecond,
		}
	}})
	user := s.newUser("d4chain")
	r := &rchRun{t: t, m: m, e: e, s: s, user: user, pfx: "d4" + randomHex(t, 3)}
	r.tn = &m2wTenant{Name: "d4chain", UserID: user, byID: map[string]*m2wItem{}}
	r.tn.From = fmt.Sprintf("sender-%s-%s@example.test", r.tn.Name, m.r.runID)
	m.r.tenants = append(m.r.tenants, r.tn)
	m.r.createPoolAndAccount(t, r.tn) // a dedicated pool owned by the user and one account naming the sink's private address
	s.grant(user, jobs.ModuleVerify, rchVerifyGrant)
	s.grant(user, jobs.ModuleSendManaged, rchSendGrant)
	r.requireNode()
	if v := r.sink(); len(v.Msgs) != 0 || len(v.Rcpt) != 0 {
		t.Fatalf("the sink already holds %d message(s) and %d RCPT TO answer(s) for addresses of this run's prefix before anything was sent: the prefix is not unique", len(v.Msgs), len(v.Rcpt))
	}
	t.Logf("NOTE d4 run: user %s; the sink's private address %s; module 1 is the node-wired control plane, module 2 has its real worker; prefix of every address %s-", jlShort(user), m.r.sinkIP, r.pfx)

	// ---- step 1: the first source, a real verify job ----
	plan1 := rchPlan(t, e, r.pfx+"-a", rchSourceSpecs)
	src1 := r.verify("source 1", plan1)
	nUnknown := rchCount(plan1, "unknown")
	if nUnknown != 2 || len(src1.mod) != len(plan1) {
		t.Fatalf("the first source holds %d unknown and %d module result(s), want 2 and %d", nUnknown, len(src1.mod), len(plan1))
	}
	m1Av, m1Rv := s.balance(user, jobs.ModuleVerify)

	// ---- step 2 and 3: the two chains of the first source ----
	chA := r.chain("chain A (exclude)", src1, jobs.UnknownExclude, "d4 chain, unknown excluded")
	chB := r.chain("chain B (include)", src1, jobs.UnknownInclude, "d4 chain, unknown included")

	// the controls of the sink check, over the real readings: each chain's delivery judged by the OTHER choice is refused, and the one function the old chain test used says the same
	jlWantProblem(t, "the include chain's delivery judged by the exclude selection", "selected=false) gained 1 message(s)", rchSinkProblems(plan1, false, chB.before, chB.after))
	jlWantProblem(t, "the exclude chain's delivery judged by the include selection", "selected=true) gained 0 message(s)", rchSinkProblems(plan1, true, chA.before, chA.after))
	jlWantProblem(t, "the include chain's counts judged by the exclude selection (jlSinkDeltaProblems)", "selected=false gained 1", jlSinkDeltaProblems(chB.before.Msgs, chB.after.Msgs, rchSelectedSet(plan1, false), rchAddrs(plan1)))
	if p := jlSinkDeltaProblems(chB.before.Msgs, chB.after.Msgs, rchSelectedSet(plan1, true), rchAddrs(plan1)); len(p) != 0 {
		t.Fatalf("the include chain's counts judged by its own selection (jlSinkDeltaProblems): %v", p)
	}
	if p := jlSinkDeltaProblems(chA.before.Msgs, chA.after.Msgs, rchSelectedSet(plan1, false), rchAddrs(plan1)); len(p) != 0 {
		t.Fatalf("the exclude chain's counts judged by its own selection (jlSinkDeltaProblems): %v", p)
	}

	// ---- step 5, for the first source: nothing the chains did changed it ----
	r.requireSourceUntouched("after the two chains", src1, m1Av, m1Rv)

	// ---- step 4: a source with no unknown row needs no choice ----
	plan2 := rchPlan(t, e, r.pfx+"-b", rchSmallSpecs)
	src2 := r.verify("source 2", plan2)
	if rchCount(plan2, "unknown") != 0 {
		t.Fatalf("the second source was meant to hold no unknown row")
	}
	m1Av2, m1Rv2 := s.balance(user, jobs.ModuleVerify)
	chC := r.chain("chain C (no choice needed)", src2, "", "d4 chain, nothing to choose")
	r.requireSourceUntouched("after the chain of the second source", src2, m1Av2, m1Rv2)
	if got := rchDigest(s.results(src1.job.ID)); got != src1.digest {
		t.Fatalf("the first source's results changed while the second source and its chain ran: %s -> %s", src1.digest, got)
	}

	// ---- step 6: the end ----
	rounds := []rchRound{{plan: plan1, include: false}, {plan: plan1, include: true}, {plan: plan2, include: false}}
	final := r.sink()
	if p := rchCumulativeProblems(rounds, final); len(p) != 0 {
		t.Fatalf("what the sink holds at the end is not what the three chained sends select (%d problem(s)):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	items, mjobs := r.moduleCounts()
	if p := rchModuleCountProblems(rounds, items, mjobs); len(p) != 0 {
		t.Fatalf("module 2's own tables are not what the three chained sends select (%d problem(s)):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	// an invalid address is present in its source (as invalid) and absent from the sink and from module 2; the same readers do see the valid ones
	var invalidN, validN int
	for _, pair := range []struct {
		plan []rchAddr
		src  *rchSource
	}{{plan1, src1}, {plan2, src2}} {
		verdictOf := map[string]string{}
		for _, x := range pair.src.mod {
			verdictOf[x.Email] = x.State
		}
		for _, a := range pair.plan {
			switch a.class {
			case "invalid":
				invalidN++
				if verdictOf[a.addr] != "invalid" {
					t.Fatalf("the invalid address of the recipe %s is not in its source job as invalid (the source says %q)", a.id, verdictOf[a.addr])
				}
				if final.Msgs[a.addr] != 0 || final.Rcpt[a.addr] != (m2wRcpt{}) || items[a.addr] != 0 || mjobs[a.addr] != 0 {
					t.Fatalf("the invalid address of the recipe %s reached the sink (%d message(s), RCPT TO %+v) or module 2 (%d row(s), %d job(s))", a.id, final.Msgs[a.addr], final.Rcpt[a.addr], items[a.addr], mjobs[a.addr])
				}
			case "valid":
				validN++
				if final.Msgs[a.addr] < 1 || items[a.addr] < 1 || mjobs[a.addr] < 1 {
					t.Fatalf("the sink and module 2 hold %d message(s), %d row(s) and %d job(s) for the valid address of the recipe %s: the readers that show an invalid address absent see nothing at all", final.Msgs[a.addr], items[a.addr], mjobs[a.addr], a.id)
				}
			}
		}
	}
	if invalidN != 3 || validN != 3 {
		t.Fatalf("the two sources hold %d invalid and %d valid address(es), want 3 and 3", invalidN, validN)
	}
	// the control of the end checks: the same readings with a message added for an invalid address must be refused
	{
		w := final.clone()
		for _, a := range plan1 {
			if a.class == "invalid" {
				w.Msgs[a.addr] = 1
				break
			}
		}
		jlWantProblem(t, "the end: a message at the sink for an invalid address", "(a invalid verdict) holds 1 message(s)", rchCumulativeProblems(rounds, w))
	}
	r.requireSourceUntouched("at the end", src1, -1, -1)
	r.requireSourceUntouched("at the end", src2, -1, -1)
	s.requireBooksScoped("at the end of the chain test", chC.job.ID)

	t.Logf("NOTE d4 verdict: the first source's real verdicts (%s) moved exactly as the owner's rule says: chain A sent to %d address(es) (valid and risky), chain B to %d (and the unknown ones), chain C (a source with no unknown row, no choice) to %d; the three invalid addresses reached neither the sink nor module 2; the first source's results are unchanged (digest %s)",
		jlStates(src1.mod), len(rchSelectedSet(plan1, false)), len(rchSelectedSet(plan1, true)), len(rchSelectedSet(plan2, false)), src1.digest[:12])
}

// rchRun is what the steps of the live test share.
type rchRun struct {
	t    *testing.T
	m    *jlM2Env
	e    *m1nEnv
	s    *jlStack
	user string
	tn   *m2wTenant
	pfx  string // the local-part prefix of every address of this run: "d4" and three random bytes in hex
}

const (
	rchVerifyGrant = 500  // credits of module 1: far above the 8 + 3 rows at the reserve rate
	rchSendGrant   = 1000 // credits of module 2: far above 3 chained sends at the dearest reachable unit
)

// rchSource is a succeeded verify job and everything the chains are judged against.
type rchSource struct {
	job     jobs.Job
	plan    []rchAddr
	mod     []jlSrc // its results of origin module, in the source's order
	digest  string  // rchDigest of all its results, read when it had just succeeded
	hash    string  // the database's own digest of them (resultsHash)
	charges int     // its ledger charge rows
}

// requireNode FAILS unless module 1's node is active, green and port 25 open: the verdicts of this test are the node's.
func (r *rchRun) requireNode() {
	t := r.t
	t.Helper()
	var status, audit string
	var port25 bool
	if err := r.s.admin.QueryRow(r.s.ctx, `select status, coalesce(audit_status, ''), coalesce(port25_open, false) from verify.nodes where id = $1`, r.e.nodeID).Scan(&status, &audit, &port25); err != nil {
		t.Fatalf("reading the node's row verify.nodes[%s]: %v", r.e.nodeID, err)
	}
	if status != "active" || audit != "green" || !port25 {
		t.Fatalf("the node's row is status=%s audit_status=%s port25_open=%t, want active, green, true: module 1 promotes the node itself from a green audit recorded after its boot (stage M1N-3 and M1N-4b wait for it), and without the node every verdict of this test would be risky/plausible with no_eligible_node", status, audit, port25)
	}
}

// sink reads the controllable sink for the addresses of this run (the ones that start with the run's prefix): the messages it accepted and the answers it gave at RCPT TO.
func (r *rchRun) sink() rchSinkView {
	t := r.t
	t.Helper()
	prefix := r.pfx + "-"
	v := rchSinkView{Msgs: map[string]int{}, Rcpt: map[string]m2wRcpt{}}
	out := r.m.r.ctlCall(t, http.MethodGet, "/api/messages", nil, http.StatusOK)
	var msgs []m2wSinkMsg
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("the sink's message list is not a JSON list of messages (%v): %s", err, r.m.r.snip(out, 300))
	}
	for _, mg := range msgs {
		for _, raw := range mg.To {
			if a := strings.ToLower(strings.TrimSpace(raw)); strings.HasPrefix(a, prefix) {
				v.Msgs[a]++
			}
		}
	}
	out = r.m.r.ctlCall(t, http.MethodGet, "/api/rcpt", nil, http.StatusOK)
	var all map[string]m2wRcpt
	if err := json.Unmarshal(out, &all); err != nil {
		t.Fatalf("the sink's /api/rcpt is not a map of counters (%v): %s", err, r.m.r.snip(out, 300))
	}
	for a, c := range all {
		if a = strings.ToLower(a); strings.HasPrefix(a, prefix) {
			v.Rcpt[a] = c
		}
	}
	return v
}

// moduleCounts reads module 2's OWN tables as the superuser, for every address of this run: the rows the platform wrote for it (public.job_items) and the jobs module 2 made of them (send_managed.jobs).
func (r *rchRun) moduleCounts() (items, mjobs map[string]int64) {
	t := r.t
	t.Helper()
	items, mjobs = map[string]int64{}, map[string]int64{}
	q, err := r.m.r.db.Query(r.m.r.ctx, `SELECT lower(ji.recipient_email), count(DISTINCT ji.id)::bigint, count(j.id)::bigint
		FROM public.job_items ji LEFT JOIN send_managed.jobs j ON j.job_item_id = ji.id
		WHERE lower(ji.recipient_email) LIKE $1::text || '%' GROUP BY 1`, r.pfx+"-")
	if err != nil {
		t.Fatalf("reading module 2's own rows of this run: %v", err)
	}
	defer q.Close()
	for q.Next() {
		var addr string
		var ni, nj int64
		if err := q.Scan(&addr, &ni, &nj); err != nil {
			t.Fatal(err)
		}
		items[addr], mjobs[addr] = ni, nj
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	return items, mjobs
}

// moduleAddrs is the addresses module 2 holds a job for under one campaign (the chained job's id), sorted: read from module 2's own table.
func (r *rchRun) moduleAddrs(campaign string) []string {
	t := r.t
	t.Helper()
	q, err := r.m.r.db.Query(r.m.r.ctx, `SELECT lower(ji.recipient_email) FROM send_managed.jobs j JOIN public.job_items ji ON ji.id = j.job_item_id WHERE j.campaign_id = $1::uuid ORDER BY 1`, campaign)
	if err != nil {
		t.Fatalf("reading module 2's own jobs of the campaign: %v", err)
	}
	defer q.Close()
	var out []string
	for q.Next() {
		var a string
		if err := q.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// state reads what a confirm writes about one chained job and its user.
func (r *rchRun) state(job string) rchState {
	t, s := r.t, r.s
	t.Helper()
	var st rchState
	st.Status = s.status(job)
	_, _, _, st.ReserveHeld = s.reserveOf(job)
	st.LedgerRows = s.ledgerRowsOf(job)
	st.Results = len(s.results(job))
	st.JobItems = s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, job)
	if err := r.m.r.db.QueryRow(r.m.r.ctx, `SELECT count(*) FROM send_managed.jobs WHERE campaign_id = $1::uuid`, job).Scan(&st.ModuleJobs); err != nil {
		t.Fatalf("count module 2's own jobs of the campaign: %v", err)
	}
	st.ActiveM2 = s.count("count the active module 2 jobs of the user", `SELECT count(*) FROM platform.jobs WHERE user_id = $1::uuid AND module_id = 'm2' AND status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')`, r.user)
	st.ConfirmKey = s.str("read the confirm key of the job", `SELECT COALESCE(confirm_key, '') FROM platform.jobs WHERE id = $1::uuid`, job)
	st.Confirmed = s.moves(job, jobs.StatusConfirmed)
	st.Available, st.Reserved = s.balance(r.user, jobs.ModuleSendManaged)
	return st
}

// verify runs a verify job of the plan's addresses through the engine against module 1's node-wired control plane and judges it (step 1).
func (r *rchRun) verify(label string, plan []rchAddr) *rchSource {
	t, s, e, user := r.t, r.s, r.e, r.user
	t.Helper()
	n := len(plan)
	avail0, rsv0 := s.balance(user, jobs.ModuleVerify)
	if rsv0 != 0 {
		t.Fatalf("%s: the user holds %d reserved credit(s) of module 1 before the job: an earlier job is still holding some", label, rsv0)
	}
	fxBase := e.fx.seq()
	j := s.create(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: jlRowsOf(rchAddrs(plan))})
	pv := s.preview(user, j.ID)
	if pv.Rows != n || pv.Pending != n || pv.Units != n || pv.Malformed != 0 || pv.Duplicates != 0 || pv.Suppressed != 0 || pv.Excluded != 0 || !pv.Metered || pv.UnitCost < 1 || pv.Credits != int64(n)*int64(pv.UnitCost) || pv.Available != avail0 || pv.Shortfall != 0 {
		t.Fatalf("%s: the preview counts rows %d pending %d units %d malformed %d repeats %d suppressed %d excluded %d metered %v credits %d (rate %d) available %d shortfall %d, want %d row(s), all pending, metered, units x rate, the %d the user holds and no shortfall",
			label, pv.Rows, pv.Pending, pv.Units, pv.Malformed, pv.Duplicates, pv.Suppressed, pv.Excluded, pv.Metered, pv.Credits, pv.UnitCost, pv.Available, pv.Shortfall, n, avail0)
	}
	res := s.confirmOK(user, j.ID, pv.Fingerprint)
	if res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != n {
		t.Fatalf("%s: the confirm answered status %s reserved %d pending %d, want confirmed, %d and %d", label, res.Job.Status, res.Reserved, res.Pending, pv.Credits, n)
	}
	diag := s.m1Diag(user, j.ID)
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 8*time.Minute, diag)

	view := s.m1View(user, j.ID, n)
	if p := jlM1ThreeWay(view); len(p) != 0 {
		t.Fatalf("%s: the ledger, the results and module 1's own report disagree (%d):\n  %s\nmodule 1's report and the oracle's decisions:\n%s", label, len(p), strings.Join(p, "\n  "), m1bTable(view.Report))
	}
	if p := rchVerdictProblems(plan, view.Rows, view.Results, view.Report); len(p) != 0 {
		t.Fatalf("%s: the verdicts are not the recipe table's (%d):\n  %s\nmodule 1's report:\n%s", label, len(p), strings.Join(p, "\n  "), m1bTable(view.Report))
	}
	// the verdicts are the node's: the fixture saw the target and the probe of each address that reaches the node, from the node, and nothing else
	sessions := e.fx.sessions(fxBase, "")
	if p := m1eCheckProbes(sessions, e.nodeIP, rchReaching(plan)); len(p) != 0 {
		t.Fatalf("%s: the fixture's view of this job is not one verification of each address that reaches the node (%d RCPT(s) seen):\n  %s", label, len(sessions), strings.Join(p, "\n  "))
	}
	jlWantProblem(t, label+": the probe check against an address nobody verified", "was RCPTed 0 time(s)", m1eCheckProbes(sessions, e.nodeIP, append(rchReaching(plan), r.pfx+"-nobody@valid-never-verified.test")))
	// the money of the verify job: what module 1's own report implies, at the frozen prices
	var wantCharged int64
	for _, mrow := range view.Report {
		if d := m1bOracle(mrow); d.Kind == "charge" {
			wantCharged += int64(view.Prices[d.Unit])
		}
	}
	reserved, charged, released, ok := s.reserveOf(j.ID)
	if left := pv.Credits - wantCharged; !ok || reserved != pv.Credits || charged != wantCharged || released != left || reserved-charged-released != 0 {
		t.Fatalf("%s: the reserve is (reserved %d, charged %d, released %d, exists %v), the report implies charged %d of the %d held: want the whole reserve accounted for", label, reserved, charged, released, ok, wantCharged, pv.Credits)
	}
	if av, rv := s.balance(user, jobs.ModuleVerify); av != avail0-wantCharged || rv != 0 {
		t.Fatalf("%s: at the end the balance is available %d reserved %d, want %d and 0", label, av, rv, avail0-wantCharged)
	}
	s.requireBooksScoped(label, j.ID)
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	if k := s.eventKindCount(j.ID, "flag") + s.eventKindCount(j.ID, "stale"); k != 0 || s.invented(j.ID) != 0 {
		t.Fatalf("%s: the job raised %d flag or stale event(s) and holds %d invented result(s): nothing may be doubted or invented\n%s", label, k, s.invented(j.ID), strings.Join(s.events(j.ID), "\n"))
	}
	s.jlM1Controls(t, view) // the comparison above, shown to fail on tampered copies of what it accepted

	src := &rchSource{plan: plan}
	for _, x := range view.Results {
		if x.Origin == "module" {
			src.mod = append(src.mod, jlSrc{Ref: x.Ref, Email: x.Email, State: x.State})
		}
	}
	if len(src.mod) != n {
		t.Fatalf("%s: the job holds %d result(s) of module 1, want %d", label, len(src.mod), n)
	}
	final, err := s.store.Get(s.ctx, user, j.ID)
	if err != nil || final.Status != jobs.StatusSucceeded {
		t.Fatalf("%s: the job is %+v (error %v), want succeeded", label, final, err)
	}
	src.job = final
	src.digest, src.hash, src.charges = rchDigest(view.Results), s.resultsHash(j.ID), len(view.Ledger)
	if rchDigest(view.Results[1:]) == src.digest {
		t.Fatalf("%s: the digest of the results did not change when a result was dropped", label)
	}
	t.Logf("NOTE d4 %s: a verify job of %d address(es) ran through the engine against the node-wired module 1 and succeeded; the fixture saw %d RCPT(s), all from the node; verdicts (hand-written class, recipe, module 1's own words):", label, n, len(sessions))
	for _, line := range rchVerdictTable(plan, view.Rows, view.Results, view.Report) {
		t.Logf("NOTE d4 %s | %s", label, line)
	}
	t.Logf("NOTE d4 %s books: %d credit(s) reserved, %d charged by the report (ledger %d row(s)), %d released; digest of the results %s", label, pv.Credits, wantCharged, len(view.Ledger), released, src.digest[:12])
	return src
}

// rchVerdictTable is one line per address of the plan: the recipe, the hand-written class and what module 1's own report says (no address).
func rchVerdictTable(plan []rchAddr, rows []jlRow, results []jlResult, report []m1ad.Row) []string {
	rowOf := map[string]jlRow{}
	for _, row := range rows {
		rowOf[row.Norm] = row
	}
	repOf := map[string]m1ad.Row{}
	for _, m := range report {
		repOf[m.ItemID] = m
	}
	state := map[string]string{}
	for _, x := range results {
		state[x.Ref] = x.State
	}
	out := make([]string, 0, len(plan))
	for _, a := range plan {
		row := rowOf[a.addr]
		out = append(out, fmt.Sprintf("%-20s class=%-8s result=%-8s module 1: %s (reaches the node: %v)", a.id, a.class, state[row.ID], m1eVerdictOfRow(repOf[row.ID]), a.reaches))
	}
	return out
}

// requireSourceUntouched: the source job's results are what they were when it succeeded (a digest of every column read in Go, the database's own md5, its status and its ledger charges). m1Av and m1Rv, when
// not negative, are the user's module 1 balance as it was just before the chains: a chained module 2 send holds no module 1 credits.
func (r *rchRun) requireSourceUntouched(when string, src *rchSource, m1Av, m1Rv int64) {
	t, s := r.t, r.s
	t.Helper()
	res := s.results(src.job.ID)
	if got := rchDigest(res); got != src.digest {
		t.Fatalf("%s: the source job's results changed (digest %s -> %s)", when, src.digest, got)
	}
	if got := s.resultsHash(src.job.ID); got != src.hash {
		t.Fatalf("%s: the source job's results changed by the database's own digest (%s -> %s)", when, src.hash, got)
	}
	if st := s.status(src.job.ID); st != jobs.StatusSucceeded {
		t.Fatalf("%s: the source job is %s, want succeeded", when, st)
	}
	if got := len(s.ledgerCharges(src.job.ID)); got != src.charges {
		t.Fatalf("%s: the source job's ledger charges went from %d to %d", when, src.charges, got)
	}
	if m1Av >= 0 {
		if av, rv := s.balance(r.user, jobs.ModuleVerify); av != m1Av || rv != m1Rv {
			t.Fatalf("%s: the user's module 1 balance moved to available %d reserved %d from %d and %d: a chained module 2 send holds no module 1 credits", when, av, rv, m1Av, m1Rv)
		}
	}
	// the control: a copy of the results with one result changed has another digest
	if len(res) > 0 {
		w := append([]jlResult(nil), res...)
		w[0].State += "-changed"
		if rchDigest(w) == src.digest {
			t.Fatalf("%s: the digest did not change when a result was changed", when)
		}
	}
}

// rchChainRun is what one chained send left behind.
type rchChainRun struct {
	job    jobs.Job
	pv     jobs.Preview
	before rchSinkView
	after  rchSinkView
	view   jlM2View
}

// chain creates, previews, confirms and runs a module 2 send chained to the source, under the given choice for the unknown verdicts ("" for a source that has none), and judges everything (steps 2, 3 and 4).
func (r *rchRun) chain(label string, src *rchSource, choice, subject string) *rchChainRun {
	t, s, m, user := r.t, r.s, r.m, r.user
	t.Helper()
	include := choice == jobs.UnknownInclude
	plan := src.plan
	nUnknown := rchCount(plan, "unknown")
	sel := rchSelectedSet(plan, include)
	nSel := len(sel)
	if nSel == 0 {
		t.Fatalf("%s: the owner's rule selects nothing from this source: the chain would prove nothing", label)
	}
	avail0, rsv0 := s.balance(user, jobs.ModuleSendManaged)
	if rsv0 != 0 {
		t.Fatalf("%s: the user holds %d reserved credit(s) of module 2 before the chain: an earlier job is still holding some", label, rsv0)
	}

	j := s.create(user, jobs.CreateReq{Module: jobs.ModuleSendManaged, SourceJobID: src.job.ID, Content: jlContent(subject)})
	// the campaign is the job's id: a tenant of the run's cleanup that cancels it at module 2 whatever happens to this test
	ctn := &m2wTenant{Name: r.tn.Name, UserID: user, CampaignID: j.ID, PoolID: r.tn.PoolID, AccountID: r.tn.AccountID, From: r.tn.From, byID: map[string]*m2wItem{}}
	m.r.tenants = append(m.r.tenants, ctn)
	diag := func() string { return m.r.diagnostics(t, ctn) }

	// the first preview, with no choice made: the unknown rows are shown excluded and a choice is required exactly when the source holds one
	pv0 := s.preview(user, j.ID)
	if p := rchPreviewProblems(src.mod, pv0, ""); len(p) != 0 {
		t.Fatalf("%s: the first preview (no choice made) is not what the source's verdicts make of the list (%d):\n  %s", label, len(p), strings.Join(p, "\n  "))
	}
	pv := pv0
	if nUnknown > 0 {
		// THE REFUSAL: a confirm that says nothing about the unknown verdicts is refused with their count, and nothing is written
		_, err := s.confirmChoice(user, j.ID, pv0.Fingerprint, "")
		var need *jobs.UnknownChoiceRequiredError
		if !errors.Is(err, jobs.ErrUnknownChoiceRequired) || !errors.As(err, &need) || need.Unknown != nUnknown || errors.Is(err, jobs.ErrStalePreview) {
			t.Fatalf("%s: the confirm of a chain whose source holds %d unknown result(s), with no choice, answered %v, want ErrUnknownChoiceRequired naming %d unknown result(s) (and not a stale preview)", label, nUnknown, err, nUnknown)
		}
		if p := rchNothingProblems(r.state(j.ID), avail0, rsv0); len(p) != 0 {
			t.Fatalf("%s: the refused confirm wrote something (%d):\n  %s", label, len(p), strings.Join(p, "\n  "))
		}
		s.requireBooksScoped(label+": after the refused confirm", j.ID)
		var err2 error
		if pv, err2 = s.store.Preview(s.ctx, user, j.ID, jobs.PreviewUnknownChoice(choice)); err2 != nil {
			t.Fatalf("%s: the preview under the choice %q: %v", label, choice, err2)
		}
	} else if choice != "" {
		t.Fatalf("%s: a choice was given for a source with no unknown result: this step is the one that needs none", label)
	}
	if p := rchPreviewProblems(src.mod, pv, choice); len(p) != 0 {
		t.Fatalf("%s: the preview under the choice %q is not what the source's verdicts make of the list (%d):\n  %s", label, choice, len(p), strings.Join(p, "\n  "))
	}

	// the rows of the chain are the source's rows, one for one, and each is pending exactly when the owner's rule selects its verdict
	rows := s.rows(j.ID)
	if p := jlChainRowsProblems(src.mod, rows, include); len(p) != 0 {
		t.Fatalf("%s: the chain's rows are not the source's results by the owner's rule: %v", label, p)
	}
	if p := rchRowsAreSource(src.mod, rows); len(p) != 0 {
		t.Fatalf("%s: the chain's rows are not the source's rows: %v", label, p)
	}
	jlWantProblem(t, label+": a row dropped from the chain", "row(s) for the", rchRowsAreSource(src.mod, rows[1:]))
	if nUnknown > 0 {
		jlWantProblem(t, label+": the chain's rows judged by the other choice", "became a chain row in state", jlChainRowsProblems(src.mod, rows, !include))
	}
	pending := 0
	for _, row := range rows {
		if row.State == "pending" {
			pending++
		}
	}
	if pending != nSel {
		t.Fatalf("%s: the chain has %d pending row(s), the owner's rule selects %d", label, pending, nSel)
	}

	// THE CONFIRM, under the choice the job was previewed under
	before := r.sink()
	res := s.confirmChoiceOK(user, j.ID, pv.Fingerprint, choice)
	if res.Replayed || res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != nSel {
		t.Fatalf("%s: the confirm answered status %s reserved %d pending %d replayed %v, want confirmed, %d and %d", label, res.Job.Status, res.Reserved, res.Pending, res.Replayed, pv.Credits, nSel)
	}
	if av, rv := s.balance(user, jobs.ModuleSendManaged); av != avail0-pv.Credits || rv != pv.Credits {
		t.Fatalf("%s: after the confirm the balance is available %d reserved %d, want %d and %d", label, av, rv, avail0-pv.Credits, pv.Credits)
	}
	// the control of the refusal check: the same check on this job, which WAS confirmed, must report what the confirm wrote
	jlWantProblem(t, label+": the nothing-written check on the confirmed job", "holds a reserve", rchNothingProblems(r.state(j.ID), avail0, rsv0))
	// the platform's own results of the excluded rows exist from the confirm, before any module is asked; and nothing was handed to module 2 yet
	if n := s.count("count the excluded results", `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'platform' AND module_state = 'excluded' AND rule = 'P-EXC' AND decision = 'free'`, j.ID); n != int64(len(plan)-nSel) {
		t.Fatalf("%s: the confirm wrote %d free P-EXC result(s) for the excluded rows, want %d", label, n, len(plan)-nSel)
	}
	if st := r.state(j.ID); st.JobItems != 0 || st.ModuleJobs != 0 {
		t.Fatalf("%s: before the engine ran public.job_items holds %d row(s) and module 2 %d job(s) of the campaign: the confirm must not write the module's rows", label, st.JobItems, st.ModuleJobs)
	}

	s.tickToStatus(j.ID, jobs.StatusSucceeded, 5*time.Minute, diag)

	// what the three places say
	after := r.sink()
	rep := m.report(user, j.ID)
	msgs, rcpt := rchDelta(before, after)
	view := jlM2View{Rows: s.rows(j.ID), Results: s.results(j.ID), Ledger: s.ledgerCharges(j.ID), Prices: s.prices(j.ID), Report: rep.Items, Msgs: msgs, Rcpt: rcpt, Cap: m.capN}
	if p := jlM2ThreeWay(view); len(p) != 0 {
		t.Fatalf("%s: module 2's report, the results, the ledger and the sink disagree (%d):\n  %s", label, len(p), strings.Join(p, "\n  "))
	}
	if p := rchSinkProblems(plan, include, before, after); len(p) != 0 {
		t.Fatalf("%s: what the sink gained is not exactly the selected addresses (%d):\n  %s", label, len(p), strings.Join(p, "\n  "))
	}
	// module 2 holds exactly the selected rows: its own report, its own job table, the rows the platform wrote for it
	selRows := map[string]bool{}
	for _, row := range view.Rows {
		if row.State == "pending" {
			selRows[row.ID] = true
		}
	}
	if len(rep.Items) != nSel {
		t.Fatalf("%s: module 2's report holds %d item(s), want %d (the selected rows only)", label, len(rep.Items), nSel)
	}
	for _, it := range rep.Items {
		if !selRows[it.JobItemID] {
			t.Fatalf("%s: module 2's report holds an item that is no selected row of the chain", label)
		}
	}
	want := make([]string, 0, nSel)
	for a := range sel {
		want = append(want, a)
	}
	sort.Strings(want)
	if got := r.moduleAddrs(j.ID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: module 2's own job table holds %d job(s) of the campaign for %v, want exactly the %d selected address(es) %v", label, len(got), got, nSel, want)
	}
	m.requireModuleJobs(j.ID, nSel)
	if st := r.state(j.ID); st.JobItems != int64(nSel) {
		t.Fatalf("%s: public.job_items holds %d row(s) of the chain, want one per selected row (%d): an excluded row is never written for the module", label, st.JobItems, nSel)
	}
	for _, x := range view.Results {
		if x.Origin == "platform" && (x.State != "excluded" || x.Rule != "P-EXC" || x.Decision != "free" || x.Unit != "") {
			t.Fatalf("%s: a platform result of the chain is %+v, want only free P-EXC results of excluded rows", label, x)
		}
	}
	charged := m.requireBooksOfJob(s, user, j.ID, pv, nSel, avail0, view)
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	if k := s.eventKindCount(j.ID, "flag") + s.eventKindCount(j.ID, "stale"); k != 0 || s.invented(j.ID) != 0 {
		t.Fatalf("%s: the chain raised %d flag or stale event(s) and holds %d invented result(s)\n%s", label, k, s.invented(j.ID), strings.Join(s.events(j.ID), "\n"))
	}
	jlM2Controls(t, view) // the comparison above, shown to fail on tampered copies of what it accepted
	final, err := s.store.Get(s.ctx, user, j.ID)
	if err != nil || final.Status != jobs.StatusSucceeded || final.FinishedAt == nil {
		t.Fatalf("%s: the chain is %+v (error %v), want succeeded with a finish time", label, final, err)
	}
	var sent []string
	for _, a := range plan {
		if sel[a.addr] {
			sent = append(sent, a.id)
		}
	}
	t.Logf("NOTE d4 %s: choice %q; %d of %d source rows selected and sent (%s), %d excluded with free P-EXC results and never at module 2; the sink gained one message and one accepted RCPT TO for each selected address and nothing, not even a RCPT TO, for any other; module 2 holds %d job(s), the report %d item(s); %d credit(s) reserved, %d charged, the rest released once",
		label, choice, nSel, len(plan), strings.Join(sent, " "), len(plan)-nSel, nSel, len(rep.Items), pv.Credits, charged)
	return &rchChainRun{job: final, pv: pv, before: before, after: after, view: view}
}
