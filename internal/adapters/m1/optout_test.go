package m1

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The per-user vendor opt-out as module 1 serves it (optout.go; module 1's ADR-0024): the key in the describe, the `policy` of a submission and the policy stamp of a result row.
//
// WHAT IS RECORDED AND WHAT IS WRITTEN HERE. The recording of the describe (testdata/contract/describe-ok.json) was made BEFORE module 1 published vendor_fallback.opt_out_policy and the lead re-records it
// after the pin to 8aa147c; every test below is therefore written to hold both before and after that re-recording (it edits the recording to say the key, or to leave it out), and the one test that
// reads the recording as it is (TestOptOutPolicy_TheRecordedDescribeIsReadAsItIs) accepts either state and says which it saw. Every body here is HAND-WRITTEN from module 1's Docs/api-contract.md and
// named so; no recorded fixture is edited on disk.

const noVendorKey = "cold-email-no-vendor"

// withOptOutPolicy is the HAND-WRITTEN edit that gives the describe's vendor_fallback the key module 1 now adds to it (reachable stays whatever the recording says, false).
func withOptOutPolicy(v any) func(doc map[string]any) {
	return func(doc map[string]any) {
		vf, _ := doc["vendor_fallback"].(map[string]any)
		if vf == nil {
			vf = map[string]any{"reachable": false}
		}
		vf["opt_out_policy"] = v
		doc["vendor_fallback"] = vf
	}
}

func optOutOf(t *testing.T, body string) (OptOutPolicy, error, *fakeM1) {
	t.Helper()
	a, f := describeAdapter(t, body)
	got, err := a.OptOutPolicy(testCtx(t))
	return got, err, f
}

func TestOptOutPolicy_ReadsTheKeyFromTheDescribeAndSaysWhenThereIsNone(t *testing.T) {
	// HAND-WRITTEN: the key present, with the vendor NOT reachable (the key is there whatever reachable says: ADR-0024 item 6), and with it reachable
	off, err, f := optOutOf(t, editDescribe(t, withOptOutPolicy(noVendorKey)))
	if err != nil || off != (OptOutPolicy{Known: true, Key: noVendorKey}) {
		t.Fatalf("a describe that publishes the key (vendor off) read as %+v, %v; want {Known: true, Key: %q}", off, err, noVendorKey)
	}
	on, err, _ := optOutOf(t, editDescribe(t, func(doc map[string]any) {
		withOptOutPolicy(noVendorKey)(doc)
		doc["vendor_fallback"].(map[string]any)["reachable"] = true
	}))
	if err != nil || on != off {
		t.Fatalf("with the vendor reachable the key read as %+v, %v; want the same answer as with it off, %+v", on, err, off)
	}
	// the control: the describe with NO key (an older module 1; the recording before it was re-recorded) is "not said", never an empty key, and with no vendor_fallback at all as well
	none, err, _ := optOutOf(t, editDescribe(t, func(doc map[string]any) { doc["vendor_fallback"] = map[string]any{"reachable": true} }))
	if err != nil || none != (OptOutPolicy{}) || none.Known {
		t.Fatalf("a describe without the key read as %+v, %v; want {Known: false} and no error", none, err)
	}
	old, err, _ := optOutOf(t, editDescribe(t, withoutVendorFallback))
	if err != nil || old != (OptOutPolicy{}) {
		t.Fatalf("a describe without vendor_fallback read as %+v, %v; want {Known: false} and no error", old, err)
	}
	// the key is read from the describe route with the caller's key and nowhere else
	calls := f.seen()
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/v1/describe" || calls[0].Auth != "Bearer "+f.key {
		t.Errorf("the read made %+v, want one authenticated GET /v1/describe", calls)
	}
	// reading the key does not disturb reading the vendor's reachability, and the other way round (two answers of ONE describe)
	a, _ := describeAdapter(t, editDescribe(t, func(doc map[string]any) {
		withOptOutPolicy(noVendorKey)(doc)
		doc["vendor_fallback"].(map[string]any)["reachable"] = true
	}))
	if v, err := a.VendorReachable(testCtx(t)); err != nil || v != (VendorReachable{Known: true, Reachable: true}) {
		t.Errorf("the vendor read of a describe that holds the key is %+v, %v", v, err)
	}
}

func TestOptOutPolicy_TheRecordedDescribeIsReadAsItIs(t *testing.T) {
	// the recording says what it says: before the lead's re-recording it has no key (not said), after it the key (and then it is the one module 1 serves). Both are legal; neither is an error.
	got, err, _ := optOutOf(t, fixture(t, "describe-ok.json"))
	if err != nil {
		t.Fatalf("the recorded describe: %v", err)
	}
	recorded, _ := obj(t, decodeDoc(t, fixture(t, "describe-ok.json")))["vendor_fallback"].(map[string]any)
	switch key, said := recorded["opt_out_policy"].(string); {
	case said && (!got.Known || got.Key != key):
		t.Errorf("the recording says %q and the adapter read %+v", key, got)
	case !said && got.Known:
		t.Errorf("the recording says nothing and the adapter read %+v", got)
	}
	t.Logf("the recorded describe publishes the opt-out policy: %v (re-record after the pin to 8aa147c to make it true)", got.Known)
}

func TestOptOutPolicy_AMalformedKeyIsAContractErrorNamingItAndNeverAKey(t *testing.T) {
	// HAND-WRITTEN: every way the new key can be wrong. A wrong TYPE is refused by the describe's shape; a string that is not a policy name by the reader: both name vendor_fallback.opt_out_policy
	for _, c := range []struct {
		name string
		v    any
	}{
		{"a number", 7},
		{"a boolean", true},
		{"an array", []any{noVendorKey}},
		{"an object", map[string]any{"key": noVendorKey}},
		{"null", nil},
		{"the empty string", ""},
		{"a space in it", "cold email"},
		{"a slash in it", "cold-email/no-vendor"},
		{"a quote in it", `cold-email"-no-vendor`},
		{"a line break in it", "cold-email\nno-vendor"},
		{"65 characters", strings.Repeat("a", 65)},
	} {
		got, err, _ := optOutOf(t, editDescribe(t, withOptOutPolicy(c.v)))
		e := asModclient(err)
		if got != (OptOutPolicy{}) || e == nil || e.Kind != modclient.Contract || e.Key != "vendor_fallback.opt_out_policy" || e.Route != "m1.describe" {
			t.Errorf("%s: read as %+v, %v; want the zero value and a Contract error naming vendor_fallback.opt_out_policy on m1.describe", c.name, got, err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "cold email") || strings.Contains(err.Error(), "no-vendor") {
			t.Errorf("%s: the error carries the module's text: %v", c.name, err)
		}
	}
	// the controls: 64 characters is a name, and the words the key may be made of are accepted
	for _, ok := range []string{strings.Repeat("a", 64), "cold-email-no-vendor", "Policy_v2.1:x"} {
		if got, err, _ := optOutOf(t, editDescribe(t, withOptOutPolicy(ok))); err != nil || !got.Known || got.Key != ok {
			t.Errorf("control: %q read as %+v, %v", ok, got, err)
		}
	}
}

func TestOptOutPolicy_AnErrorIsNeverAnAnswer(t *testing.T) {
	good := editDescribe(t, withOptOutPolicy(noVendorKey))
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"module 1 refuses the key", 401, fixture(t, "describe-error-unauthenticated.json")},
		{"module 1 is down", 503, `{"error":{"code":"unavailable","message":"x","request_id":"r"}}`},
		{"a body that is not JSON", 200, "not json"},
	} {
		f := serve(t, c.status, c.body)
		got, err := f.adapter(t).OptOutPolicy(testCtx(t))
		if err == nil || got != (OptOutPolicy{}) {
			t.Errorf("%s: read as %+v, %v; want an error and the zero value (never a key beside an error)", c.name, got, err)
		}
	}
	// the control: the same adapter over a module that answers is Known
	if got, err, _ := optOutOf(t, good); err != nil || !got.Known {
		t.Errorf("control: %+v, %v", got, err)
	}
	if _, err := OptOutPolicyOf(nil); err == nil {
		t.Error("OptOutPolicyOf(nil) is not an error")
	}
}

func TestValidPolicyKey_ANameIsAShortWordOfTheSafeAlphabet(t *testing.T) {
	for _, ok := range []string{"cold-email", noVendorKey, "a", "A.b_c:d-9", strings.Repeat("z", MaxPolicyKeyBytes)} {
		if !ValidPolicyKey(ok) {
			t.Errorf("%q is a policy name and was refused", ok)
		}
	}
	for name, bad := range map[string]string{"empty": "", "a space": "a b", "a tab": "a\tb", "a slash": "a/b", "a quote": `a"b`, "a backslash": `a\b`, "unicode": "politikä", "too long": strings.Repeat("z", MaxPolicyKeyBytes+1), "a NUL": "a\x00b"} {
		if ValidPolicyKey(bad) {
			t.Errorf("%s: %q was accepted as a policy name", name, bad)
		}
	}
}

// ---- the submission --------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestSubmitUnder_TheOptOutKeyIsInTheBodyAndAnOptedInSubmissionSendsNoPolicyAtAll(t *testing.T) {
	plan := threeRowPlan()
	f := newFakeM1(t, func(c call) (int, string) { return 202, acceptedFor(t, plan.Expected()) })
	a := f.adapter(t)
	idemIn, idemOut := newIdemKey(t), newIdemKey(t)

	// opted in: Submit, and SubmitUnder with no policy, send exactly {"items": [...]} (the body of every job before the opt-out existed)
	if _, err := a.Submit(testCtx(t), plan, idemIn); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitUnder(testCtx(t), plan, idemIn, ""); err != nil {
		t.Fatal(err)
	}
	// opted out: the key module 1 published
	res, err := a.SubmitUnder(testCtx(t), plan, idemOut, noVendorKey)
	if err != nil || res.JobID != testJob || res.Counts != plan.Expected() {
		t.Fatalf("SubmitUnder returned %+v, %v", res, err)
	}
	calls := f.seen()
	if len(calls) != 3 {
		t.Fatalf("%d request(s), want 3", len(calls))
	}
	want, err := json.Marshal(struct {
		Items []Item `json:"items"`
	}{plan.Items})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if string(calls[i].Body) != string(want) {
			t.Errorf("an opted-in submission %d sent %s, want exactly %s (byte for byte what it always sent)", i, calls[i].Body, want)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(calls[i].Body, &m)
		if _, has := m["policy"]; has {
			t.Errorf("an opted-in submission %d carries a policy member: %s", i, calls[i].Body)
		}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(calls[2].Body, &m); err != nil {
		t.Fatal(err)
	}
	var policy string
	if len(m) != 2 || m["items"] == nil || json.Unmarshal(m["policy"], &policy) != nil || policy != noVendorKey {
		t.Fatalf("the opted-out submission sent %s, want exactly the items and policy %q", calls[2].Body, noVendorKey)
	}
	if string(m["items"]) == "" || !strings.Contains(string(calls[2].Body), `"items":`+string(m["items"])) || !strings.Contains(string(want), string(m["items"])) {
		t.Errorf("the opted-out submission changed the items: %s", calls[2].Body)
	}
	if calls[2].IdemKey != idemOut || calls[2].Path != "/v1/verify/bulk" || calls[2].Method != http.MethodPost {
		t.Errorf("the opted-out submission was %+v", calls[2])
	}
}

func TestSubmitUnder_ANameThatIsNotAWordIsRefusedBeforeARequestAndAModuleThatRefusesTheNameIsTold(t *testing.T) {
	plan := threeRowPlan()
	f := newFakeM1(t, func(c call) (int, string) { return 400, fixture(t, "submit-error-validation.json") })
	a := f.adapter(t)
	for _, bad := range []string{"cold email", `x"y`, strings.Repeat("a", MaxPolicyKeyBytes+1), "a\nb"} {
		_, err := a.SubmitUnder(testCtx(t), plan, newIdemKey(t), bad)
		if e := asModclient(err); e == nil || e.Kind != modclient.Invalid || e.Route != "m1.submit" {
			t.Errorf("policy %q: %v, want an Invalid error on m1.submit", bad, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("a bad policy name made %d request(s), want none", f.count())
	}
	// a module that does not serve the name answers 400 validation_failed (details: policy / not_found): a Refused error, nothing created, and nothing is retried
	res, err := a.SubmitUnder(testCtx(t), plan, newIdemKey(t), noVendorKey)
	if e := asModclient(err); e == nil || e.Kind != modclient.Invalid && e.Kind != modclient.Refused || res != (SubmitResult{}) {
		t.Errorf("a module that refuses the name gave %+v, %v; want an error and no value", res, err)
	}
	if f.count() != 1 {
		t.Errorf("a refusal was retried: %d request(s)", f.count())
	}
}

// ---- the result rows -------------------------------------------------------------------------------------------------------------------------------------------------------------

// policyRow is one HAND-WRITTEN result element of the shape module 1's api-contract describes, stamped with meta.policy (and meta.policy_version when v > 0).
func policyRow(ordinal int, policy string, v int) string {
	meta := `"meta":{"request_id":"","checked_at":"","latency_ms":0,"policy":` + strconv.Quote(policy) + `,"policy_version":` + strconv.Itoa(v) + `,"verifier_ip":null,"node_id":null,"engine_version":null,"config_fingerprint":null,"from_cache":false}`
	return element("valid", "accepted", "row-"+strconv.Itoa(ordinal), ordinal, meta)
}

func TestResultsUnder_EveryRowMustSayItWasMadeUnderThePolicyTheJobWasSubmittedUnder(t *testing.T) {
	good := page(testJob, "", policyRow(0, noVendorKey, 1), policyRow(1, noVendorKey, 1))
	f := serve(t, 200, good)
	a := f.adapter(t)

	rows, next, err := a.ResultsUnder(testCtx(t), testJob, "", 0, noVendorKey)
	if err != nil || len(rows) != 2 || next != "" {
		t.Fatalf("a page stamped with the opt-out policy: %+v, %q, %v", rows, next, err)
	}
	if rows[0].Policy != noVendorKey || rows[0].PolicyVersion != 1 || rows[1].Policy != noVendorKey {
		t.Errorf("the rows do not carry their policy: %+v", rows)
	}

	// the violations, each judged for the KEY and never for the value found there
	const foreign = "cold-email-LEAKCHECK"
	for _, c := range []struct {
		name string
		body string
		key  string
	}{
		{"a row made under the default policy", page(testJob, "", policyRow(0, noVendorKey, 1), policyRow(1, "cold-email", 1)), "results[1].meta.policy"},
		{"the first row under a foreign policy", page(testJob, "", policyRow(0, foreign, 1), policyRow(1, noVendorKey, 1)), "results[0].meta.policy"},
		{"a row with no meta at all", page(testJob, "", row(0)), "results[0].meta.policy"},
		{"a row with an empty policy", page(testJob, "", policyRow(0, "", 1)), "results[0].meta.policy"},
		{"a row with the right policy and no version", page(testJob, "", policyRow(0, noVendorKey, 0)), "results[0].meta.policy_version"},
		{"the key in another case", page(testJob, "", policyRow(0, strings.ToUpper(noVendorKey), 1)), "results[0].meta.policy"},
		{"the key with a suffix", page(testJob, "", policyRow(0, noVendorKey+"-v2", 1)), "results[0].meta.policy"},
	} {
		g := serve(t, 200, c.body)
		rows, next, err := g.adapter(t).ResultsUnder(testCtx(t), testJob, "", 0, noVendorKey)
		e := asModclient(err)
		if rows != nil || next != "" || e == nil || e.Kind != modclient.Contract || e.Key != c.key || !errors.Is(err, ErrNotUnderPolicy) || e.Route != "m1.results" {
			t.Errorf("%s: %+v, %q, %v; want NO rows and a Contract error naming %s (cause ErrNotUnderPolicy) on m1.results", c.name, rows, next, err, c.key)
		}
		if strings.Contains(err.Error(), foreign) || strings.Contains(err.Error(), "cold-email,") {
			t.Errorf("%s: the error carries a policy value: %v", c.name, err)
		}
	}

	// the paired control: the SAME pages are fine for a job that was not submitted under a named policy (an opted-in job's rows are not judged for it), and Results is ResultsUnder with none
	for _, body := range []string{page(testJob, "", policyRow(0, "cold-email", 1)), page(testJob, "", row(0)), page(testJob, "", policyRow(0, foreign, 1))} {
		g := serve(t, 200, body)
		if rows, _, err := g.adapter(t).Results(testCtx(t), testJob, "", 0); err != nil || len(rows) != 1 {
			t.Errorf("an opted-in job's page was refused: %+v, %v", rows, err)
		}
		if rows, _, err := g.adapter(t).ResultsUnder(testCtx(t), testJob, "", 0, ""); err != nil || len(rows) != 1 {
			t.Errorf("ResultsUnder with no policy is not Results: %+v, %v", rows, err)
		}
	}
	// a policy that is not a name is refused before a request
	h := serve(t, 200, good)
	if _, _, err := h.adapter(t).ResultsUnder(testCtx(t), testJob, "", 0, "not a name"); asModclient(err) == nil || asModclient(err).Kind != modclient.Invalid || h.count() != 0 {
		t.Errorf("a bad policy to check against: %v after %d request(s), want an Invalid error and none", err, h.count())
	}
	// an empty page holds nothing that could break the rule: it is not an error
	e := serve(t, 200, page(testJob, ""))
	if rows, _, err := e.adapter(t).ResultsUnder(testCtx(t), testJob, "", 0, noVendorKey); err != nil || len(rows) != 0 {
		t.Errorf("an empty page: %+v, %v", rows, err)
	}
}

func TestResultsUnder_ACancelledRowIsJudgedLikeAnyOtherAndAMetaOfTheWrongTypeIsAContractError(t *testing.T) {
	// a row cancelled before its check began carries the work item's own policy (api-contract.md, "A cancelled row in the results"): HAND-WRITTEN from it
	cancelled := func(policy string) string {
		return `{"api_version":"v1","status":"cancelled","reason":"cancelled_by_caller","confidence":0,"recommendation":"review","email":"","domain":"acmecorp.com","flags":{},"signals":{},` +
			`"meta":{"request_id":"","checked_at":"","latency_ms":0,"policy":` + strconv.Quote(policy) + `,"policy_version":1,"verifier_ip":null,"node_id":null,"engine_version":null,"config_fingerprint":null,"from_cache":false},"raw":null,"ordinal":5,"item_id":"row-5"}`
	}
	ok := serve(t, 200, page(testJob, "", cancelled(noVendorKey)))
	if rows, _, err := ok.adapter(t).ResultsUnder(testCtx(t), testJob, "", 0, noVendorKey); err != nil || len(rows) != 1 || rows[0].Status != StatusCancelled || rows[0].Policy != noVendorKey {
		t.Fatalf("a cancelled row of the opt-out policy: %+v, %v", rows, err)
	}
	bad := serve(t, 200, page(testJob, "", cancelled("cold-email")))
	if rows, _, err := bad.adapter(t).ResultsUnder(testCtx(t), testJob, "", 0, noVendorKey); rows != nil || !errors.Is(err, ErrNotUnderPolicy) {
		t.Errorf("a cancelled row of the default policy in an opted-out job: %+v, %v; want it refused", rows, err)
	}
	// a meta (or a policy) of the wrong type is the shape's Contract error, naming the key, for every job
	for name, extra := range map[string]string{
		"meta as a string":   `"meta":"cold-email-no-vendor"`,
		"policy as a number": `"meta":{"policy":7,"policy_version":1}`,
		"version as text":    `"meta":{"policy":"x","policy_version":"1"}`,
		"meta null":          `"meta":null`,
	} {
		f := serve(t, 200, page(testJob, "", element("valid", "accepted", "row-0", 0, extra)))
		rows, _, err := f.adapter(t).Results(testCtx(t), testJob, "", 0)
		if e := asModclient(err); rows != nil || e == nil || e.Kind != modclient.Contract || !strings.HasPrefix(e.Key, "results[0].meta") {
			t.Errorf("%s: %+v, %v; want a Contract error naming results[0].meta...", name, rows, err)
		}
	}
}
