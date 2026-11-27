package jobengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The per-user vendor opt-out in the real module 1 wrapper (module_m1_optout.go; Phase 5 S6, module 1's ADR-0024): an opted-out job is submitted under module 1's own opt-out policy and its rows are
// checked against it; with no key it WAITS and is never submitted under the default policy; an opted-in job is exactly what it was. The module is the stateful stand-in of modules_rig_test.go, which
// records the `policy` of a submission and stamps every row with it (HAND-WRITTEN from module 1's Docs/api-contract.md, "Policies"); the live module is the scope `jobs` of the harness.

const optOutKey = "cold-email-no-vendor"

// policySource is the OptOutPolicySource of these tests: it says what it is told and counts its asks.
type policySource struct {
	mu    sync.Mutex
	key   string
	err   error
	calls int
}

func (p *policySource) M1OptOutPolicy(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.key, p.err
}

func (p *policySource) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// optOutModule is module 1's wrapper for the rig over a registry that has the source.
func optOutModule(t *testing.T, rig *m1Rig, src jobs.OptOutPolicySource) (*jobengine.Registry, jobengine.Module) {
	t.Helper()
	f, err := m1.NewFactory(rig.stub.URL(), modclient.Options{Timeout: 3 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := jobengine.NewRegistry(jobengine.RegistryDeps{M1: f, Scoper: &rigScoper{key: rig.key}, OptOutPolicy: src})
	mod, err := reg.For(ctxT(t), rigUser, jobs.ModuleVerify)
	if err != nil {
		t.Fatal(err)
	}
	return reg, mod
}

func optedOut(n int) jobs.EngineJob {
	j := jobOf(jobs.ModuleVerify, n)
	j.Settings.VendorOptOut = true
	return j
}

func submittedPolicy(t *testing.T, rig *m1Rig) (policy string, members []string) {
	t.Helper()
	reqs := rig.stub.Requests()
	if len(reqs) == 0 {
		t.Fatal("module 1 received no request")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatalf("the request body %s: %v", reqs[0].Body, err)
	}
	for k := range body {
		members = append(members, k)
	}
	if raw, ok := body["policy"]; ok {
		if err := json.Unmarshal(raw, &policy); err != nil {
			t.Fatalf("the policy member %s: %v", raw, err)
		}
	}
	return policy, members
}

func TestM1OptOut_AnOptedOutJobIsSubmittedUnderModuleOnesOwnKeyAndAnOptedInJobNamesNone(t *testing.T) {
	src := &policySource{key: optOutKey}
	rows := jobenginetestRows(5)

	// opted out: the body carries the key module 1's describe published (the source), beside the items
	rigOut := newM1Rig(t)
	_, modOut := optOutModule(t, rigOut, src)
	res, err := modOut.Submit(ctxT(t), optedOut(5), rows)
	if err != nil || len(res.Accepted) != 5 || res.ModuleRef == "" {
		t.Fatalf("an opted-out submit: %+v, %v", res, err)
	}
	policy, members := submittedPolicy(t, rigOut)
	if policy != optOutKey || len(members) != 2 {
		t.Fatalf("the opted-out job's body has members %v and policy %q, want the items and policy %q", members, policy, optOutKey)
	}
	if src.count() != 1 {
		t.Errorf("the key was read %d time(s) for one submit, want once", src.count())
	}

	// the control: the SAME wrapper, the SAME source, an opted-in job: no policy member at all, and the source is not even asked
	rigIn := newM1Rig(t)
	_, modIn := optOutModule(t, rigIn, src)
	n := src.count()
	if res, err := modIn.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 5), rows); err != nil || len(res.Accepted) != 5 {
		t.Fatalf("an opted-in submit: %+v, %v", res, err)
	}
	if policy, members := submittedPolicy(t, rigIn); policy != "" || len(members) != 1 || members[0] != "items" {
		t.Errorf("the opted-in job's body has members %v and policy %q, want exactly [items]", members, policy)
	}
	if src.count() != n {
		t.Errorf("an opted-in job asked for the opt-out key (%d -> %d)", n, src.count())
	}
}

func TestM1OptOut_WithNoKeyAnOptedOutJobWaitsAndNothingIsEverSentUnderTheDefaultPolicy(t *testing.T) {
	rows := jobenginetestRows(4)
	down := &modclient.Error{Kind: modclient.Unavailable, Module: modclient.M1, Route: "m1.describe", Message: "module 1 is down"}
	// answers that would be a REFUSAL OF THE JOB at the submit phase (the engine fails a job for those when the module holds none of its rows): a describe never is one
	refusal := &modclient.Error{Kind: modclient.Refused, Module: modclient.M1, Route: "m1.describe", Status: 400, Code: "validation_failed", Message: "no"}
	notFound := &modclient.Error{Kind: modclient.NotFound, Module: modclient.M1, Route: "m1.describe", Status: 404, Code: "not_found", Message: "no"}
	contract := &modclient.Error{Kind: modclient.Contract, Module: modclient.M1, Route: "m1.describe", Message: "the describe is not module 1's", Key: "vendor_fallback.opt_out_policy"}
	for _, c := range []struct {
		name string
		src  jobs.OptOutPolicySource
		want string
	}{
		{"no source at all in this process", nil, "no source"},
		{"module 1 cannot be asked", &policySource{err: down}, ""},
		{"module 1 publishes no key (an older module 1)", &policySource{err: jobs.ErrNoOptOutPolicy}, "publishes none"},
		{"an answer with no key and no error", &policySource{}, "publishes none"},
		{"an answer that is not a policy name", &policySource{key: "not a name"}, "publishes none"},
		{"module 1 refuses the describe", &policySource{err: refusal}, ""},
		{"module 1 does not know the describe", &policySource{err: notFound}, ""},
		{"module 1's describe is off contract", &policySource{err: contract}, ""},
		{"an error of no known kind", &policySource{err: errors.New("boom")}, ""},
	} {
		rig := newM1Rig(t)
		reg, mod := optOutModule(t, rig, c.src)
		_, err := mod.Submit(ctxT(t), optedOut(4), rows)
		if !errors.Is(err, jobengine.ErrWait) || errors.Is(err, jobengine.ErrRefused) {
			t.Errorf("%s: Submit = %v, want ErrWait and never ErrRefused (the engine would fail the job)", c.name, err)
			continue
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the reason %q does not say %q", c.name, err.Error(), c.want)
		}
		if rig.stub.Count() != 0 || rig.jobCount() != 0 {
			t.Errorf("%s: module 1 received %d request(s) and holds %d job(s): an opted-out job must never be submitted without its policy", c.name, rig.stub.Count(), rig.jobCount())
		}
		// the same job being OBSERVED waits too, and reads no result of module 1 (it has none to read: nothing was submitted)
		j := optedOut(4)
		j.ModuleRef = "00000000-0000-4000-8000-000000000001"
		if _, err := mod.Observe(ctxT(t), j, func([]jobs.ResultItem) error { return nil }); !errors.Is(err, jobengine.ErrWait) {
			t.Errorf("%s: Observe = %v, want ErrWait", c.name, err)
		}
		_ = reg
	}
	// the control: with the key the very same call is a submission under it
	rig := newM1Rig(t)
	_, mod := optOutModule(t, rig, &policySource{key: optOutKey})
	if res, err := mod.Submit(ctxT(t), optedOut(4), rows); err != nil || len(res.Accepted) != 4 {
		t.Fatalf("the control: %+v, %v", res, err)
	}
	if policy, _ := submittedPolicy(t, rig); policy != optOutKey {
		t.Errorf("the control was submitted under %q", policy)
	}
}

func TestM1OptOut_EveryResultRowIsCheckedAgainstThePolicyAndAViolationIsNeverRecorded(t *testing.T) {
	rows := jobenginetestRows(6)
	valid := []charge.Outcome{{Module: "m1", State: "valid", Reason: "accepted"}}

	// a module 1 that honours the policy: every row says it, and the engine reads them all
	rig := newM1Rig(t)
	reg, mod := optOutModule(t, rig, &policySource{key: optOutKey})
	res, err := mod.Submit(ctxT(t), optedOut(6), rows)
	if err != nil {
		t.Fatal(err)
	}
	rig.settleAll(valid)
	j := optedOut(6)
	j.ModuleRef = res.ModuleRef
	got := map[string]jobs.ResultItem{}
	obs, err := mod.Observe(ctxT(t), j, collect(got))
	if err != nil || !obs.Complete || len(got) != 6 {
		t.Fatalf("an honoured policy: observe = %+v, %v, %d result(s); want a complete job and six results", obs, err, len(got))
	}

	// a module 1 that ignores the policy it was sent (the customer's choice is not being honoured): every row says the default policy, the page is refused whole, NOTHING is delivered to be recorded or
	// charged, the step waits, and the registry counts an answer it does not understand
	bad := newM1Rig(t)
	bad.ignorePolicy.Store(true)
	breg, bmod := optOutModule(t, bad, &policySource{key: optOutKey})
	bres, err := bmod.Submit(ctxT(t), optedOut(6), rows)
	if err != nil {
		t.Fatal(err)
	}
	bad.settleAll(valid)
	bj := optedOut(6)
	bj.ModuleRef = bres.ModuleRef
	delivered := map[string]jobs.ResultItem{}
	before := breg.UnexpectedAnswers()
	_, err = bmod.Observe(ctxT(t), bj, collect(delivered))
	if !errors.Is(err, jobengine.ErrWait) || errors.Is(err, jobengine.ErrRefused) {
		t.Fatalf("a module that ignored the policy: observe = %v, want ErrWait", err)
	}
	if len(delivered) != 0 {
		t.Errorf("%d result(s) of an opted-out job made under another policy were handed over to be recorded", len(delivered))
	}
	if breg.UnexpectedAnswers() != before+1 {
		t.Errorf("the registry counted %d unexpected answer(s), want one more than %d", breg.UnexpectedAnswers(), before)
	}
	if strings.Contains(err.Error(), "cold-email") && !strings.Contains(err.Error(), "meta.policy") {
		t.Errorf("the reason carries a policy value: %v", err)
	}
	_ = reg

	// the control: the very same module, ignoring the policy, serves an OPTED-IN job without complaint (its rows are not judged for a policy it never named)
	in := newM1Rig(t)
	in.ignorePolicy.Store(true)
	_, imod := optOutModule(t, in, &policySource{key: optOutKey})
	ires, err := imod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 6), rows)
	if err != nil {
		t.Fatal(err)
	}
	in.settleAll(valid)
	ij := jobOf(jobs.ModuleVerify, 6)
	ij.ModuleRef = ires.ModuleRef
	idelivered := map[string]jobs.ResultItem{}
	if _, err := imod.Observe(ctxT(t), ij, collect(idelivered)); err != nil || len(idelivered) != 6 {
		t.Errorf("an opted-in job on the same module: %d result(s), %v; want all six", len(idelivered), err)
	}
}

func TestM1OptOut_TheKeyIsReadOnlyWhereItIsNeeded(t *testing.T) {
	src := &policySource{key: optOutKey}
	rig := newM1Rig(t)
	_, mod := optOutModule(t, rig, src)
	rows := jobenginetestRows(3)
	res, err := mod.Submit(ctxT(t), optedOut(3), rows)
	if err != nil {
		t.Fatal(err)
	}
	if src.count() != 1 {
		t.Fatalf("a submit read the key %d time(s), want once", src.count())
	}
	j := optedOut(3)
	j.ModuleRef = res.ModuleRef

	// a look at a job nothing has settled yet (the status alone answers) reads no result and so needs no key
	if _, err := mod.Observe(ctxT(t), j, func([]jobs.ResultItem) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if src.count() != 1 {
		t.Errorf("an observe that read no result asked for the key (%d reads in all)", src.count())
	}
	// once rows are settled the walk needs it, and asks once
	rig.settleAll([]charge.Outcome{{Module: "m1", State: "valid", Reason: "accepted"}})
	if _, err := mod.Observe(ctxT(t), j, func([]jobs.ResultItem) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if src.count() != 2 {
		t.Errorf("a walk of the results read the key %d time(s) in all, want 2 (one submit, one walk)", src.count())
	}
	// a cancel names no policy and needs no key
	if err := mod.Cancel(ctxT(t), j); err != nil {
		t.Fatal(err)
	}
	if src.count() != 2 {
		t.Errorf("a cancel asked for the key (%d reads in all)", src.count())
	}
}
