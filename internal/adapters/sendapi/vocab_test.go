package sendapi_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// switchStates reads the string literals of the case clauses of stateClass from the source: the adapter's own switch, as written.
func switchStates(t testing.TB) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "profile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "stateClass" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						s, _ := strconv.Unquote(bl.Value)
						out = append(out, s)
					}
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// setProblems compares the values a switch handles with the union of the vocabularies it serves: a value the switch handles that no vocabulary lists, and a value a vocabulary lists that
// the switch does not handle, each named.
func setProblems(handled []string, vocabs ...[]string) []string {
	var union []string
	for _, v := range vocabs {
		for _, s := range v {
			if !slices.Contains(union, s) {
				union = append(union, s)
			}
		}
	}
	var problems []string
	for _, h := range handled {
		if !slices.Contains(union, h) {
			problems = append(problems, "the switch handles "+h+", which no vocabulary lists")
		}
	}
	for _, u := range union {
		if !slices.Contains(handled, u) {
			problems = append(problems, "a vocabulary lists "+u+", which the switch does not handle")
		}
	}
	return problems
}

// The adapter's switch over item states handles exactly the values of the vocabularies it serves (module 2's seven and module 3's six): none that no vocabulary lists (a dead case that
// would one day settle something nobody meant), none missing. The paired controls run the same comparison on a switch with an extra case and one with a case missing, and prove the
// real switch refuses a state it was not written for.
func TestStateSwitchIsTheUnionOfTheVocabularies(t *testing.T) {
	handled := switchStates(t)
	if len(handled) == 0 {
		t.Fatal("the instrument matched nothing: no case literal found in stateClass")
	}
	if problems := setProblems(handled, m2like().JobStates, m3like().JobStates); len(problems) != 0 {
		t.Fatalf("the switch and the vocabularies disagree: %v", problems)
	}
	for _, p := range profiles() {
		for _, s := range p.JobStates {
			if _, ok := sendapi.StateClassForTest(s); !ok {
				t.Fatalf("%s lists %q and the switch does not handle it", p.Module, s)
			}
		}
	}
	// controls
	if problems := setProblems(append(slices.Clone(handled), "paused"), m2like().JobStates, m3like().JobStates); len(problems) != 1 || !strings.Contains(problems[0], "switch handles paused") {
		t.Fatalf("an extra case was not caught: %v", problems)
	}
	if problems := setProblems(handled[1:], m2like().JobStates, m3like().JobStates); len(problems) != 1 || !strings.Contains(problems[0], "switch does not handle") {
		t.Fatalf("a missing case was not caught: %v", problems)
	}
	for _, alien := range []string{"paused", "expired", "", "SENT", "sent "} {
		if _, ok := sendapi.StateClassForTest(alien); ok {
			t.Fatalf("the switch handles %q", alien)
		}
	}
	// the classes themselves: what each state means for settlement
	want := map[string]sendapi.Class{"queued": sendapi.ClassPending, "assigned": sendapi.ClassInFlight, "sending": sendapi.ClassInFlight,
		"sent": sendapi.ClassTerminal, "bounced": sendapi.ClassTerminal, "failed": sendapi.ClassTerminal, "cancelled": sendapi.ClassTerminal}
	for s, c := range want {
		if got, _ := sendapi.StateClassForTest(s); got != c {
			t.Fatalf("%s is %s, want %s", s, got, c)
		}
	}
}

// The vocabularies a contract names are exactly the sets the profile (and so the adapter's code) handles, for every contract of both profiles: job_state on every contract that carries a
// state, error_class and (module 2 only) item_reason on the reports, error_codes on every error, all four on describe.
func TestContractVocabulariesAreTheProfilesSets(t *testing.T) {
	for name, p := range profiles() {
		t.Run(name, func(t *testing.T) {
			cs := sendapi.Contracts(p)
			if len(cs) == 0 {
				t.Fatal("no contracts: the subject is empty")
			}
			want := p.Vocabularies()
			if _, has := want["item_reason"]; has != (name == "m2") {
				t.Fatalf("item_reason in the profile: %v", has)
			}
			seen := map[string]int{}
			for _, c := range cs {
				for k, v := range c.Vocab {
					exp, ok := want[k]
					exp = withCancelledItemCode(c, k, exp)
					if !ok || !slices.Equal(v, exp) {
						t.Fatalf("%s / %s: vocabulary %q is %v, the profile's is %v", c.Route, c.Case, k, v, exp)
					}
					seen[k]++
				}
				if c.Module != p.Module {
					t.Fatalf("%s / %s names module %s", c.Route, c.Case, c.Module)
				}
			}
			for k := range want {
				if seen[k] == 0 {
					t.Fatalf("no contract carries the vocabulary %q", k)
				}
			}
			// the copies are copies: editing a contract's set does not edit the profile's or the next call's
			for _, c := range cs {
				if v, ok := c.Vocab["job_state"]; ok {
					v[0] = "tampered"
				}
			}
			if p.JobStates[0] == "tampered" {
				t.Fatal("a contract's vocabulary shares its slice with the profile")
			}
			for _, c := range sendapi.Contracts(p) {
				if v, ok := c.Vocab["job_state"]; ok && v[0] == "tampered" {
					t.Fatal("two calls of Contracts share a slice")
				}
			}
			vs := p.Vocabularies()
			vs["job_state"][0] = "tampered"
			if p.JobStates[0] == "tampered" {
				t.Fatal("Vocabularies shares its slices with the profile")
			}
		})
	}
}

func appendValue(t testing.TB, body []byte, vocab, v string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	e := doc["vocabularies"].(map[string]any)[vocab].(map[string]any)
	e["values"] = append(e["values"].([]any), v)
	out, _ := json.Marshal(doc)
	return out
}

func appendCode(t testing.TB, body []byte, code string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	doc["error_codes"] = append(doc["error_codes"].([]any), map[string]any{"code": code, "http_status": 418})
	out, _ := json.Marshal(doc)
	return out
}

func mutated(t testing.TB, body []byte, path string, op sendapitest.Op, v any) []byte {
	t.Helper()
	out, err := sendapitest.Mutate(body, path, op, v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// CheckVocabularies reads the module's describe and says what no longer matches: a vocabulary that is gone, a missing value, a state the adapter does not handle, a missing error code are
// Contract errors naming the vocabulary and, in the message, only values the ADAPTER expects; an extra error class or error code is additive and is returned, not raised. The control is the
// describe that matches exactly.
func TestCheckVocabulariesNamesWhatChanged(t *testing.T) {
	p := m3like()
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	check := func(t *testing.T, body []byte) (sendapi.VocabReport, error) {
		e := newEnv(t, p, userA, serve(200, body), modclient.Options{}, sendapi.Options{})
		return sendapi.CheckVocabularies(ctxT(t), e.c.Module(), p)
	}
	rep, err := check(t, good)
	if err != nil || rep.Breaking() || rep.Additive() || len(rep.Diffs) != 3 { // job_state, error_class and run_conflict_cause (module 3 names the causes of a run 409)
		t.Fatalf("the control (an exact describe): %v %+v", err, rep)
	}
	for _, d := range rep.Diffs {
		if !d.Exact() {
			t.Fatalf("%s is not exact", d.Name)
		}
	}
	breaking := map[string]struct {
		body    []byte
		key     string
		message string
	}{
		"a state is gone":                  {mutated(t, good, "vocabularies.job_state.values[5]", sendapitest.Delete, nil), "vocabularies.job_state", "bounced"},
		"a state is added (a strict set)":  {appendValue(t, good, "job_state", "paused"), "vocabularies.job_state", "does not handle"},
		"a vocabulary is gone":             {mutated(t, good, "vocabularies.error_class", sendapitest.Delete, nil), "vocabularies.error_class", "no longer advertises"},
		"a cause of a run refusal is gone": {mutated(t, good, "vocabularies.run_conflict_cause.values[3]", sendapitest.Delete, nil), "vocabularies.run_conflict_cause", "queue_cap"},
		"a cause is added (a strict set)":  {appendValue(t, good, "run_conflict_cause", "full_moon"), "vocabularies.run_conflict_cause", "does not handle"},
		"the cause vocabulary is gone":     {mutated(t, good, "vocabularies.run_conflict_cause", sendapitest.Delete, nil), "vocabularies.run_conflict_cause", "no longer advertises"},
		"an error class is gone":           {mutated(t, good, "vocabularies.error_class.values[0]", sendapitest.Delete, nil), "vocabularies.error_class", "bounce_permanent"},
		"an error code is gone":            {mutated(t, good, "error_codes[8]", sendapitest.Delete, nil), "error_codes", "paused"},
		"the first error code is gone":     {mutated(t, good, "error_codes[0]", sendapitest.Delete, nil), "error_codes", "validation_failed"},
		"the module is another module":     {mutated(t, good, "module", sendapitest.Set, "send_managed"), "module", "wired"},
		"the api version is not v1":        {mutated(t, good, "api_version", sendapitest.Set, "v2"), "api_version", "API version"},
		"job_state has no values at all":   {mutated(t, good, "vocabularies.job_state.values", sendapitest.Delete, nil), "vocabularies.job_state", "no longer advertises"},
		"describe lacks a required key":    {mutated(t, good, "pricing", sendapitest.Delete, nil), "pricing", ""},
		"a vocabulary values is not array": {mutated(t, good, "vocabularies.job_state.values", sendapitest.Set, "queued"), "vocabularies.job_state.values", ""},
	}
	for name, tc := range breaking {
		t.Run(name, func(t *testing.T) {
			rep, err := check(t, tc.body)
			ce := wantKind(t, err, modclient.Contract)
			if ce.Key != tc.key || !strings.Contains(ce.Message, tc.message) {
				t.Fatalf("key %q message %q, want %q containing %q", ce.Key, ce.Message, tc.key, tc.message)
			}
			if len(rep.Diffs) != 0 || rep.Breaking() {
				t.Fatalf("a report came back with the error: %+v", rep)
			}
		})
	}
	// additive differences are reported, not raised
	for name, tc := range map[string][]byte{
		"an extra error class": appendValue(t, good, "error_class", "quota"),
		"an extra error code":  appendCode(t, good, "teapot"),
	} {
		t.Run(name, func(t *testing.T) {
			rep, err := check(t, tc)
			if err != nil || rep.Breaking() || !rep.Additive() {
				t.Fatalf("%v breaking=%v additive=%v", err, rep.Breaking(), rep.Additive())
			}
		})
	}
	// a module that is down is its own error, with no report
	e := newEnv(t, p, userA, nil, modclient.Options{}, sendapi.Options{})
	e.stub.Close()
	if r, err := sendapi.CheckVocabularies(ctxT(t), e.c.Module(), p); modclient.KindOf(err) != modclient.Unavailable || len(r.Diffs) != 0 {
		t.Fatalf("a module that is down: %v %+v", err, r)
	}
	// a client that is not the profile's module
	if _, err := sendapi.CheckVocabularies(ctxT(t), e.c.Module(), m2like()); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a module 3 client with module 2's profile: %v", err)
	}
	if _, err := sendapi.CheckVocabularies(ctxT(t), nil, p); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("no client: %v", err)
	}
}

// Module 2's profile against module 3's describe: the pure comparison names every difference in both directions (module 3 has no item_reason vocabulary, and two more error codes; both have
// the cancelled state since the close-out, so job_state is exact), and Vocabularies hands back the advertised sets with no network beyond the one describe.
func TestCompareVocabulariesAcrossTheTwoModules(t *testing.T) {
	e := newEnv(t, m3like(), userA, serve(200, sendapitest.Read(t, fixtures, "describe.ok.json")), modclient.Options{}, sendapi.Options{})
	d, err := sendapi.Describe(ctxT(t), e.c.Module(), m3like())
	if err != nil {
		t.Fatal(err)
	}
	r := sendapi.CompareVocabularies(d, m2like())
	if !r.Breaking() || !r.Additive() {
		t.Fatalf("module 2's profile against module 3's describe: breaking=%v additive=%v", r.Breaking(), r.Additive())
	}
	var js, ir *modclient.VocabDiff
	for i := range r.Diffs {
		switch r.Diffs[i].Name {
		case "job_state":
			js = &r.Diffs[i]
		case "item_reason":
			ir = &r.Diffs[i]
		}
	}
	if js == nil || !js.Exact() {
		t.Fatalf("job_state: %+v (both modules list cancelled since the close-out)", js)
	}
	// the control: module 3's describe with its cancelled state taken out differs from module 2's profile exactly by that state
	noCancel := slices.DeleteFunc(slices.Clone(m3like().JobStates), func(s string) bool { return s == "cancelled" })
	other := m3like()
	other.JobStates, other.Cancel = noCancel, false
	if rj := sendapi.CompareVocabularies(d, other); rj.Diffs[0].Name != "job_state" || !slices.Equal(rj.Diffs[0].Extra, []string{"cancelled"}) {
		t.Fatalf("job_state against a profile without cancelled: %+v", rj.Diffs[0])
	}
	if ir == nil || ir.Advertised {
		t.Fatalf("item_reason: %+v", ir)
	}
	if len(r.CodesMissing) != 0 || !slices.Equal(r.CodesExtra, []string{"rate_limited", "unprocessable"}) {
		t.Fatalf("error codes: missing %v extra %v", r.CodesMissing, r.CodesExtra)
	}
	// the other direction
	r = sendapi.CompareVocabularies(d, m3like())
	if r.Breaking() || r.Additive() {
		t.Fatalf("module 3's profile against its own describe: %+v", r)
	}
	vs, err := sendapi.Vocabularies(ctxT(t), e.c.Module(), m3like())
	if err != nil || !slices.Equal(vs["job_state"], m3like().JobStates) {
		t.Fatalf("%v %v", err, vs)
	}
	if vs, err := sendapi.Vocabularies(ctxT(t), e.c.Module(), m2like()); modclient.KindOf(err) != modclient.Invalid || vs != nil {
		t.Fatalf("a profile for another module: %v %v", err, vs)
	}
}

// The shape the contracts list for describe is a copy of what modclient.Describe demands (its own is unexported); this holds the two together: for the recorded describe and a set of
// mutations, modclient.Describe accepts exactly what Decode with the contract's Shape accepts.
func TestDescribeShapeAgreesWithModclient(t *testing.T) {
	p := m3like()
	var shape modclient.Shape
	for _, c := range sendapi.Contracts(p) {
		if c.Route == sendapi.RouteDescribe {
			shape = c.Shape
		}
	}
	good := sendapitest.Read(t, fixtures, "describe.ok.json")
	bodies := map[string][]byte{"the recorded describe": good}
	rejected := map[string][]byte{
		"no module":                   mutated(t, good, "module", sendapitest.Delete, nil),
		"module is a number":          mutated(t, good, "module", sendapitest.Set, 3),
		"no api_version":              mutated(t, good, "api_version", sendapitest.Delete, nil),
		"capabilities not booleans":   mutated(t, good, "capabilities.single_send", sendapitest.Set, "yes"),
		"no pricing":                  mutated(t, good, "pricing", sendapitest.Delete, nil),
		"unit price is text":          mutated(t, good, "pricing.unit_price_credits", sendapitest.Set, "free"),
		"no currency":                 mutated(t, good, "pricing.currency", sendapitest.Delete, nil),
		"vocabulary values not array": mutated(t, good, "vocabularies.job_state.values", sendapitest.Set, "queued"),
		"vocabularies is a list":      mutated(t, good, "vocabularies", sendapitest.Set, []any{}),
		"no error_codes":              mutated(t, good, "error_codes", sendapitest.Delete, nil),
		"error code without a status": mutated(t, good, "error_codes[0].http_status", sendapitest.Delete, nil),
		"error code status is text":   mutated(t, good, "error_codes[0].http_status", sendapitest.Set, "400"),
	}
	accepted := map[string][]byte{
		"an extra key":              mutated(t, good, "extension_routes", sendapitest.Delete, nil),
		"models omitted":            mutated(t, good, "pricing.models", sendapitest.Delete, nil),
		"a vocabulary with no list": mutated(t, good, "vocabularies.job_state.values", sendapitest.Delete, nil),
	}
	for k, v := range rejected {
		bodies[k] = v
	}
	for k, v := range accepted {
		bodies[k] = v
	}
	nRejected := 0
	for name, body := range bodies {
		e := newEnv(t, p, userA, serve(200, body), modclient.Options{}, sendapi.Options{})
		_, derr := e.c.Module().Describe(ctxT(t))
		serr := modclient.Decode(&modclient.Response{Status: 200, Body: body}, nil, shape)
		if (derr == nil) != (serr == nil) {
			t.Fatalf("%s: modclient.Describe says %v and the contract's shape says %v", name, derr, serr)
		}
		if _, isRejected := rejected[name]; isRejected {
			nRejected++
			if derr == nil {
				t.Fatalf("%s: both accept a describe that breaks the shape", name)
			}
		} else if derr != nil {
			t.Fatalf("%s: both refuse a describe that is fine: %v", name, derr)
		}
	}
	if nRejected != len(rejected) {
		t.Fatal("not every rejection was exercised")
	}
}

// withCancelledItemCode is the set a contract's vocabulary is expected to be: the profile's, except that the contract of the 409 `job_item_cancelled` lists that code in the set of error codes whatever
// the profile says (the contract is about it; the profiles of module 2 and module 3 list it since the pin that lists it in describe, the copies of this package's tests do not).
func withCancelledItemCode(c modclient.RouteContract, name string, set []string) []string {
	if name == "error_codes" && c.Route == sendapi.RouteRun && c.Case == "error job_item_cancelled" && !slices.Contains(set, sendapi.CodeItemCancelled) {
		return append(slices.Clone(set), sendapi.CodeItemCancelled)
	}
	return set
}
