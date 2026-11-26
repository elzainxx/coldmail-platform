package m1

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// editDescribe is the recorded describe (describe-ok.json) with fn applied to its decoded document, as a body to serve. Every test below starts from that one file and changes one
// thing, so what it finds is the change.
func editDescribe(t *testing.T, fn func(doc map[string]any)) string {
	t.Helper()
	doc, ok := decodeDoc(t, fixture(t, "describe-ok.json")).(map[string]any)
	if !ok {
		t.Fatal("the describe fixture is not an object")
	}
	fn(doc)
	return encodeDoc(t, doc)
}

func vocabEntry(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	e, ok := obj(t, doc["vocabularies"])[name].(map[string]any)
	if !ok {
		t.Fatalf("the describe fixture has no vocabulary %q", name)
	}
	return e
}

func vocabValues(t *testing.T, doc map[string]any, name string) []any {
	t.Helper()
	vs, ok := vocabEntry(t, doc, name)["values"].([]any)
	if !ok || len(vs) == 0 {
		t.Fatalf("the vocabulary %q of the fixture has no values", name)
	}
	return vs
}

func errorCodeList(t *testing.T, doc map[string]any) []any {
	t.Helper()
	cs, ok := doc["error_codes"].([]any)
	if !ok {
		t.Fatal("the fixture has no error_codes")
	}
	return cs
}

// describeAdapter is an adapter over a stand-in that answers every request with body (status 200).
func describeAdapter(t *testing.T, body string) (*Adapter, *fakeM1) {
	f := serve(t, 200, body)
	return f.adapter(t), f
}

// TestDescribe_ChecksItIsModuleOneOfferingWhatTheAdapterRelies: Describe sends one GET /v1/describe with the per-caller key and returns the document when it is module 1's: `module` is
// "verify", `api_version` "v1", `capabilities.idempotent_bulk_create` and `job_totals` true, `idempotency.header` is Idempotency-Key, `idempotency.replay_response_header` is
// Idempotent-Replayed, and `routes` lists the four routes the adapter calls. Each of those that is not so is a Contract error that names its key and the route m1.describe, with no value
// (and a capability that is absent is as false as one that says false). Paired control: the recording, unchanged, is accepted by the same call, and what it returns is what the file
// says; and answers of the module's own keep their class (the per-caller key refused, a pause, a missing route).
func TestDescribe_ChecksItIsModuleOneOfferingWhatTheAdapterRelies(t *testing.T) {
	a, f := describeAdapter(t, fixture(t, "describe-ok.json"))
	d, err := a.Describe(testCtx(t))
	if err != nil || d == nil {
		t.Fatalf("the recorded describe was refused: %v", err)
	}
	if c := f.seen(); len(c) != 1 || c[0].Method != http.MethodGet || c[0].Path != "/v1/describe" || c[0].Auth != "Bearer "+f.key {
		t.Fatalf("the describe call was %+v, want one GET /v1/describe with the key", c)
	}
	rec := recordedDoc(t, "describe-ok.json")
	if d.Module != rec["module"] || d.APIVersion != rec["api_version"] || len(d.Vocabularies) != len(obj(t, rec["vocabularies"])) || len(d.ErrorCodes) != len(rec["error_codes"].([]any)) ||
		!d.Capabilities["idempotent_bulk_create"] || !d.Capabilities["job_totals"] || d.Capabilities["quote"] || d.Capabilities["run"] || d.Capabilities["report"] {
		t.Fatalf("Describe returned %+v, want what the file says", d)
	}
	if !bytes.Equal(d.Raw, []byte(fixture(t, "describe-ok.json"))) {
		t.Fatal("Raw is not the response body")
	}

	dropRoute := func(method, path string) func(doc map[string]any) {
		return func(doc map[string]any) {
			var kept []any
			for _, r := range doc["routes"].([]any) {
				m := obj(t, r)
				if m["method"] == method && m["path"] == path {
					continue
				}
				kept = append(kept, r)
			}
			if len(kept) != len(doc["routes"].([]any))-1 {
				t.Fatalf("setup: the fixture does not list %s %s exactly once", method, path)
			}
			doc["routes"] = kept
		}
	}
	setCap := func(name string, v any) func(doc map[string]any) {
		return func(doc map[string]any) {
			if v == nil {
				delete(obj(t, doc["capabilities"]), name)
				return
			}
			obj(t, doc["capabilities"])[name] = v
		}
	}
	cases := []struct {
		name string
		edit func(doc map[string]any)
		key  string
	}{
		{"another module (a sender's describe at module 1's address)", func(doc map[string]any) { doc["module"] = "send_byo" }, "module"},
		{"module in another case", func(doc map[string]any) { doc["module"] = "Verify" }, "module"},
		{"another API version", func(doc map[string]any) { doc["api_version"] = "v2" }, "api_version"},
		{"an empty API version", func(doc map[string]any) { doc["api_version"] = "" }, "api_version"},
		{"idempotent_bulk_create false", setCap("idempotent_bulk_create", false), "capabilities.idempotent_bulk_create"},
		{"idempotent_bulk_create absent", setCap("idempotent_bulk_create", nil), "capabilities.idempotent_bulk_create"},
		{"job_totals false", setCap("job_totals", false), "capabilities.job_totals"},
		{"job_totals absent", setCap("job_totals", nil), "capabilities.job_totals"},
		{"another idempotency header", func(doc map[string]any) { obj(t, doc["idempotency"])["header"] = "X-Request-Key" }, "idempotency.header"},
		{"the idempotency header in another case", func(doc map[string]any) { obj(t, doc["idempotency"])["header"] = "idempotency-key" }, "idempotency.header"},
		{"another replay header", func(doc map[string]any) { obj(t, doc["idempotency"])["replay_response_header"] = "X-Replayed" }, "idempotency.replay_response_header"},
		{"no idempotency contract", func(doc map[string]any) { delete(doc, "idempotency") }, "idempotency"},
		{"no route table", func(doc map[string]any) { delete(doc, "routes") }, "routes"},
		{"the verify route not listed", dropRoute("POST", "/v1/verify"), "routes"},
		{"the bulk route not listed", dropRoute("POST", "/v1/verify/bulk"), "routes"},
		{"the status route not listed", dropRoute("GET", "/v1/jobs/{id}"), "routes"},
		{"the results route not listed", dropRoute("GET", "/v1/jobs/{id}/results"), "routes"},
		{"the bulk route listed under another method", func(doc map[string]any) {
			for _, r := range doc["routes"].([]any) {
				if m := obj(t, r); m["path"] == "/v1/verify/bulk" {
					m["method"] = "PUT"
				}
			}
		}, "routes"},
	}
	for _, c := range cases {
		g, _ := describeAdapter(t, editDescribe(t, c.edit))
		d, err := g.Describe(testCtx(t))
		e := asModclient(err)
		if d != nil || e == nil || e.Kind != modclient.Contract || e.Key != c.key || e.Route != "m1.describe" || e.Module != modclient.M1 || e.Status != 200 || !e.Sent {
			t.Errorf("%s: got %+v %v, want no value and a Contract error naming %q on m1.describe", c.name, d, err, c.key)
		}
		if err != nil && leaks(err.Error(), "send_byo", "X-Request-Key", "X-Replayed", "v2") {
			t.Errorf("%s: the error quotes what the module sent: %v", c.name, err)
		}
	}

	// the module's own answers keep their class and give no value
	for _, row := range []struct {
		status int
		body   string
		kind   modclient.Kind
	}{
		{401, fixture(t, "status-error-unauthenticated.json"), modclient.Refused},
		{404, fixture(t, "status-error-not-found.json"), modclient.NotFound},
		{503, fixture(t, "submit-error-paused.json"), modclient.Paused},
		{503, fixture(t, "submit-error-unavailable.json"), modclient.Unavailable},
		{500, fixture(t, "submit-error-internal.json"), modclient.Internal},
	} {
		d, err := serve(t, row.status, row.body).adapter(t).Describe(testCtx(t))
		if d != nil || modclient.KindOf(err) != row.kind {
			t.Errorf("status %d: got %+v %v, want kind %v and no value", row.status, d, err, row.kind)
		}
	}
}

// TestDescribeShapeAgreesWithModclient: the describe contract's Shape (what Contracts() lists and the drift gate compares) and what the adapter really demands are the same: for the
// keys modclient decodes (module, api_version, capabilities, vocabularies with their values, error_codes) the Shape and modclient's own decode of a client built for module 1 accept
// and refuse the same bodies; for the two keys only the adapter reads (routes, idempotency) the Shape refuses a body that modclient alone would accept, and so does Describe, naming
// the same key. Every body of each kind is exercised (a count says so), with the unchanged recording as the control that both accept.
func TestDescribeShapeAgreesWithModclient(t *testing.T) {
	var shape modclient.Shape
	for _, c := range Contracts() {
		if c.Route == tplDescribe && c.Case == "ok" {
			shape = c.Shape
		}
	}
	rec := fixture(t, "describe-ok.json")
	del := func(path string) string { return remove(path).apply(t, rec) }
	set := func(path string, to any) string { return retype(path, to).apply(t, rec) }
	both := map[string]string{ // refused by both
		"no module":                   del("module"),
		"module is a number":          set("module", 3),
		"no api_version":              del("api_version"),
		"api_version is a number":     set("api_version", 1),
		"no capabilities":             del("capabilities"),
		"capabilities not booleans":   set("capabilities.job_totals", "yes"),
		"no vocabularies":             del("vocabularies"),
		"vocabularies is a list":      set("vocabularies", []any{}),
		"a vocabulary without values": del("vocabularies.status.values"),
		"values not an array":         set("vocabularies.reason.values", "invalid_syntax"),
		"a value that is not text":    set("vocabularies.reason.values[0]", 1),
		"an entry that is not object": set("vocabularies.status", "valid"),
		"no error_codes":              del("error_codes"),
		"error code without a status": del("error_codes[0].http_status"),
		"error code status is text":   set("error_codes[0].http_status", "400"),
		"error code without a code":   del("error_codes[0].code"),
	}
	adapterOnly := map[string]string{ // accepted by modclient, refused by the contract's Shape and by Describe
		"no routes":                        del("routes"),
		"routes is an object":              set("routes", map[string]any{}),
		"a route without a method":         del("routes[0].method"),
		"a route without a path":           del("routes[1].path"),
		"a route whose method is a number": set("routes[0].method", 1),
		"no idempotency":                   del("idempotency"),
		"idempotency without a header":     del("idempotency.header"),
		"idempotency header not text":      set("idempotency.header", 5),
		"idempotency without the replay":   del("idempotency.replay_response_header"),
	}
	accepted := map[string]string{ // accepted by both
		"the recording": rec,
		"no schemas":    del("schemas"),
		"an extra key":  editDescribe(t, func(doc map[string]any) { doc["future_key"] = map[string]any{"a": 1} }),
		"a pricing object": editDescribe(t, func(doc map[string]any) {
			doc["pricing"] = map[string]any{"currency": "credits", "model": "free", "unit_price_credits": 0}
		}),
		"an extra capability": editDescribe(t, func(doc map[string]any) { obj(t, doc["capabilities"])["future"] = true }),
		"an extra vocabulary": editDescribe(t, func(doc map[string]any) {
			obj(t, doc["vocabularies"])["future_set"] = map[string]any{"values": []any{"a"}}
		}),
		"a vocabulary without its source": editDescribe(t, func(doc map[string]any) { delete(vocabEntry(t, doc, "status"), "source") }),
		"a vocabulary without its fields": editDescribe(t, func(doc map[string]any) { delete(vocabEntry(t, doc, "status"), "fields") }),
		"an extra error code": editDescribe(t, func(doc map[string]any) {
			doc["error_codes"] = append(errorCodeList(t, doc), map[string]any{"code": "x", "http_status": 418})
		}),
		"an extra idempotency key":      editDescribe(t, func(doc map[string]any) { obj(t, doc["idempotency"])["future"] = "x" }),
		"a route without a description": editDescribe(t, func(doc map[string]any) { delete(obj(t, doc["routes"].([]any)[0]), "description") }),
	}
	check := func(group string, bodies map[string]string, wantClient, wantShape, wantAdapter bool) {
		t.Helper()
		if len(bodies) == 0 {
			t.Fatalf("%s: no bodies", group)
		}
		for name, body := range bodies {
			a, _ := describeAdapter(t, body)
			_, cerr := a.c.Describe(testCtx(t))
			serr := modclient.Decode(&modclient.Response{Status: 200, Body: []byte(body)}, nil, shape)
			_, aerr := a.Describe(testCtx(t))
			if (cerr == nil) != wantClient || (serr == nil) != wantShape || (aerr == nil) != wantAdapter {
				t.Errorf("%s / %s: modclient accepts %t (%v), the contract Shape %t (%v), Describe %t (%v); want %t, %t and %t", group, name, cerr == nil, cerr, serr == nil, serr, aerr == nil, aerr, wantClient, wantShape, wantAdapter)
				continue
			}
			if wantShape || wantAdapter {
				continue
			}
			// a body the Shape and Describe both refuse is refused at the same key
			if sk, ak := keyOf(asModclient(serr)), keyOf(asModclient(aerr)); sk == "" || sk != ak {
				t.Errorf("%s / %s: the Shape names %q, Describe names %q", group, name, sk, ak)
			}
		}
	}
	check("refused by both", both, false, false, false)
	check("refused by the adapter's reading only", adapterOnly, true, false, false)
	check("accepted by both", accepted, true, true, true)
	t.Logf("%d bodies refused by both, %d by the adapter's reading only, %d accepted by both", len(both), len(adapterOnly), len(accepted))
}

// additiveVocabularies are the vocabularies module 1's describe publishes that the adapter does not COMPARE (they are not among the seven of vocabOrder): `detail_issue` (the issue words of a
// validation error) and, since module 1 migration 0020 (the cancel of a bulk job), `cancel_outcome`, `non_verdict_status` and `non_verdict_reason`. Sorted, as CheckVocabularies reports them. They are
// ADDITIVE, never breaking; the last three are held to the words the adapter handles by describedWordsProblems.
var additiveVocabularies = []string{"cancel_outcome", "detail_issue", "non_verdict_reason", "non_verdict_status"}

// describedWordsProblems compares the three additive vocabularies that carry a word the adapter handles with those words (cancelOutcomeValues, nonVerdictStatusValues, nonVerdictReasonValues,
// each held equal to its switch by the vocab tests): a word added, changed or removed in the module's describe is a problem here, though it is not a break of the report.
func describedWordsProblems(d *modclient.Describe) []string {
	var p []string
	for name, want := range map[string][]string{"cancel_outcome": cancelOutcomeValues, "non_verdict_status": nonVerdictStatusValues, "non_verdict_reason": nonVerdictReasonValues} {
		got, w := append([]string(nil), d.Vocabularies[name]...), append([]string(nil), want...)
		sort.Strings(got)
		sort.Strings(w)
		if !reflect.DeepEqual(got, w) {
			p = append(p, name+": the module publishes "+strings.Join(got, ",")+", the adapter handles "+strings.Join(w, ","))
		}
	}
	sort.Strings(p)
	return p
}

// TestCheckVocabularies_TheRecordedDescribeIsExactlyWhatTheAdapterHandles: the recording's seven vocabularies are the adapter's own sets and its ten error codes are errorCodeValues,
// so CheckVocabularies returns a clean report: every Diff exact, no error code missing or extra, not Breaking; the things it notes are the four vocabularies the adapter does not compare
// (additiveVocabularies: detail_issue, cancel_outcome, non_verdict_status, non_verdict_reason), which are Additive, and the three of them that name a word the adapter handles say exactly the
// adapter's words. CompareVocabularies, the pure function behind it, says the same of the same body. Controls: with detail_issue removed the report still notes the other three; with all four
// removed it is neither Breaking nor Additive, which shows that the four were the whole of the note; and a describe whose cancel_outcome lost a word is found by the words comparison, which a
// report that only lists names would pass.
func TestCheckVocabularies_TheRecordedDescribeIsExactlyWhatTheAdapterHandles(t *testing.T) {
	a, f := describeAdapter(t, fixture(t, "describe-ok.json"))
	r, err := a.CheckVocabularies(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("%d request(s), want one describe", f.count())
	}
	if r.Breaking() || len(r.Diffs) != 7 || len(r.CodesMissing) != 0 || len(r.CodesExtra) != 0 || !reflect.DeepEqual(r.VocabulariesExtra, additiveVocabularies) || !r.Additive() {
		t.Fatalf("the recording's report is %+v, want seven exact vocabularies, no code missing or extra and exactly %v as the additive vocabularies", r, additiveVocabularies)
	}
	var names []string
	for _, d := range r.Diffs {
		names = append(names, d.Name)
		if !d.Exact() || d.Breaking() {
			t.Errorf("%s: the recording differs from the adapter's set: %+v", d.Name, d)
		}
	}
	if !reflect.DeepEqual(names, vocabOrder) {
		t.Fatalf("the report holds %v, want the seven sets in order %v", names, vocabOrder)
	}
	d, err := a.c.Describe(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(CompareVocabularies(d), r) {
		t.Fatal("CompareVocabularies and CheckVocabularies disagree on the same body")
	}
	// the three additive vocabularies that name a word the adapter handles say exactly the adapter's words
	if p := describedWordsProblems(d); len(p) != 0 {
		t.Fatalf("the recording's additive vocabularies differ from the words the adapter handles: %v", p)
	}

	// control: with detail_issue removed, the other three are still noted (Additive, not Breaking)
	g, _ := describeAdapter(t, editDescribe(t, func(doc map[string]any) { delete(obj(t, doc["vocabularies"]), "detail_issue") }))
	if r, err := g.CheckVocabularies(testCtx(t)); err != nil || r.Breaking() || !r.Additive() || !reflect.DeepEqual(r.VocabulariesExtra, []string{"cancel_outcome", "non_verdict_reason", "non_verdict_status"}) {
		t.Fatalf("control: with detail_issue removed got %+v %v, want the three cancel vocabularies noted as additive and nothing else", r, err)
	}
	// control: with all four removed, nothing is left to note
	h, _ := describeAdapter(t, editDescribe(t, func(doc map[string]any) {
		for _, name := range additiveVocabularies {
			delete(obj(t, doc["vocabularies"]), name)
		}
	}))
	if r, err := h.CheckVocabularies(testCtx(t)); err != nil || r.Breaking() || r.Additive() || len(r.VocabulariesExtra) != 0 {
		t.Fatalf("control: with the four additive vocabularies removed got %+v %v, want a report with nothing to note", r, err)
	}
	// control: a word that changes in one of the three is not a break of the report (the vocabulary is additive) and IS found by the words comparison
	for name, edit := range map[string]func(vs map[string]any){
		"cancel_outcome lost a word": func(vs map[string]any) {
			vs["cancel_outcome"].(map[string]any)["values"] = []any{"cancelled", "already_cancelled"}
		},
		"non_verdict_status renamed": func(vs map[string]any) { vs["non_verdict_status"].(map[string]any)["values"] = []any{"withdrawn"} },
		"non_verdict_reason gained": func(vs map[string]any) {
			vs["non_verdict_reason"].(map[string]any)["values"] = []any{"cancelled_by_caller", "expired"}
		},
	} {
		k, _ := describeAdapter(t, editDescribe(t, func(doc map[string]any) { edit(obj(t, doc["vocabularies"])) }))
		kr, err := k.CheckVocabularies(testCtx(t))
		if err != nil || kr.Breaking() {
			t.Fatalf("%s: got %+v %v, want an additive report (these vocabularies are never compared by CheckVocabularies)", name, kr, err)
		}
		kd, err := k.c.Describe(testCtx(t))
		if err != nil {
			t.Fatal(err)
		}
		if p := describedWordsProblems(kd); len(p) != 1 {
			t.Errorf("%s: the words comparison found %v, want exactly the one vocabulary that changed", name, p)
		}
	}
}

// TestCheckVocabularies_EverySetIsStrict: for EACH of the seven sets the adapter handles, a value the module no longer lists, a value it lists that the adapter does not know, and the
// whole vocabulary gone are each a Break (module 1's decoding refuses an unknown value, so a describe that lists one is a contract violation found before a live row): a Contract
// error that names "vocabularies.<name>" on m1.describe, with a zero report, whose message names the values the ADAPTER expects and the module lacks and quotes nothing the module sent.
// The pure comparison says the same (Breaking, with the Diff of that name holding the Missing or Extra value). Paired control: the unmodified recording is clean (the previous test), and
// each damage names only the vocabulary it damaged.
func TestCheckVocabularies_EverySetIsStrict(t *testing.T) {
	const alien = "zz_value_the_adapter_never_heard_of"
	damage := map[string]func(t *testing.T, doc map[string]any, name string) (message string){
		"a value missing": func(t *testing.T, doc map[string]any, name string) string {
			vs := vocabValues(t, doc, name)
			vocabEntry(t, doc, name)["values"] = vs[1:]
			return vs[0].(string) // the message must name the value the adapter expects
		},
		"the last value missing": func(t *testing.T, doc map[string]any, name string) string {
			vs := vocabValues(t, doc, name)
			vocabEntry(t, doc, name)["values"] = vs[:len(vs)-1]
			return vs[len(vs)-1].(string)
		},
		"an extra value": func(t *testing.T, doc map[string]any, name string) string {
			vocabEntry(t, doc, name)["values"] = append(vocabValues(t, doc, name), alien)
			return "1 value(s)"
		},
		"the vocabulary gone": func(t *testing.T, doc map[string]any, name string) string {
			delete(obj(t, doc["vocabularies"]), name)
			return "no longer advertises"
		},
	}
	hows := make([]string, 0, len(damage))
	for how := range damage {
		hows = append(hows, how)
	}
	sort.Strings(hows)
	checked := 0
	for _, name := range vocabOrder {
		for _, how := range hows {
			var want string
			body := editDescribe(t, func(doc map[string]any) { want = damage[how](t, doc, name) })
			a, _ := describeAdapter(t, body)
			r, err := a.CheckVocabularies(testCtx(t))
			e := asModclient(err)
			checked++
			if e == nil || e.Kind != modclient.Contract || e.Key != "vocabularies."+name || e.Route != "m1.describe" || !reflect.DeepEqual(r, VocabReport{}) {
				t.Errorf("%s / %s: got %+v kind %v key %q (%v), want a Contract error naming vocabularies.%s and a zero report", name, how, r, kindOf(e), keyOf(e), err, name)
				continue
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s / %s: the message %q does not say %q", name, how, err, want)
			}
			if strings.Contains(err.Error(), alien) {
				t.Errorf("%s / %s: the message quotes a value the module sent: %q", name, how, err)
			}
			// the pure comparison
			d, derr := a.c.Describe(testCtx(t))
			if derr != nil {
				t.Fatalf("%s / %s: %v", name, how, derr)
			}
			cr := CompareVocabularies(d)
			var diff modclient.VocabDiff
			for _, x := range cr.Diffs {
				if x.Name == name {
					diff = x
				}
			}
			switch how {
			case "an extra value":
				if !cr.Breaking() || len(diff.Extra) != 1 || diff.Extra[0] != alien || len(diff.Missing) != 0 {
					t.Errorf("%s / %s: the comparison says %+v", name, how, diff)
				}
			case "the vocabulary gone":
				if !cr.Breaking() || diff.Advertised {
					t.Errorf("%s / %s: the comparison says %+v", name, how, diff)
				}
			default:
				if !cr.Breaking() || len(diff.Missing) != 1 || len(diff.Extra) != 0 || !diff.Advertised {
					t.Errorf("%s / %s: the comparison says %+v", name, how, diff)
				}
			}
			for _, x := range cr.Diffs {
				if x.Name != name && !x.Exact() {
					t.Errorf("%s / %s: the damage reached %s", name, how, x.Name)
				}
			}
		}
	}
	if checked != 7*4 {
		t.Fatalf("%d case(s) ran, want 28 (seven sets, four kinds of damage)", checked)
	}
}

func kindOf(e *modclient.Error) modclient.Kind {
	if e == nil {
		return 0
	}
	return e.Kind
}

func keyOf(e *modclient.Error) string {
	if e == nil {
		return ""
	}
	return e.Key
}

// TestCheckVocabularies_AnErrorCodeThatIsGoneIsABreakAndAnExtraOneIsNot: for each of the ten codes the adapter knows, a describe that no longer lists it is a Break naming error_codes (the
// message names the code the adapter expects, and no other); a code the adapter does not know, and a vocabulary it does not handle, are additive: no error, the report names them and
// Additive is true while Breaking is false. Paired control: the recording lists all ten and is clean.
func TestCheckVocabularies_AnErrorCodeThatIsGoneIsABreakAndAnExtraOneIsNot(t *testing.T) {
	if len(errorCodeValues) != 10 {
		t.Fatalf("the adapter knows %d error codes, want 10", len(errorCodeValues))
	}
	for _, code := range errorCodeValues {
		body := editDescribe(t, func(doc map[string]any) {
			var kept []any
			for _, c := range errorCodeList(t, doc) {
				if obj(t, c)["code"] != code {
					kept = append(kept, c)
				}
			}
			if len(kept) != len(errorCodeList(t, doc))-1 {
				t.Fatalf("setup: the fixture does not list %s exactly once", code)
			}
			doc["error_codes"] = kept
		})
		a, _ := describeAdapter(t, body)
		r, err := a.CheckVocabularies(testCtx(t))
		e := asModclient(err)
		if e == nil || e.Kind != modclient.Contract || e.Key != "error_codes" || e.Route != "m1.describe" || !reflect.DeepEqual(r, VocabReport{}) {
			t.Errorf("without %s: got %+v %v, want a Contract error naming error_codes and a zero report", code, r, err)
			continue
		}
		for _, other := range errorCodeValues {
			if other != code && !strings.Contains(code, other) && !strings.Contains(other, code) && strings.Contains(err.Error(), other) {
				t.Errorf("without %s: the message names %s too", code, other)
			}
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("without %s: the message %q does not name it", code, err)
		}
	}

	// additive only: an error code and a vocabulary the adapter does not know
	extra := editDescribe(t, func(doc map[string]any) {
		doc["error_codes"] = append(errorCodeList(t, doc), map[string]any{"code": "some_new_code", "http_status": 418})
		obj(t, doc["vocabularies"])["future_set"] = map[string]any{"source": "module_constant", "values": []any{"a", "b"}, "fields": []any{"x"}}
	})
	a, _ := describeAdapter(t, extra)
	r, err := a.CheckVocabularies(testCtx(t))
	if err != nil || r.Breaking() || !r.Additive() || !reflect.DeepEqual(r.CodesExtra, []string{"some_new_code"}) || !reflect.DeepEqual(r.VocabulariesExtra, []string{"cancel_outcome", "detail_issue", "future_set", "non_verdict_reason", "non_verdict_status"}) || len(r.CodesMissing) != 0 {
		t.Fatalf("an extra code and an extra vocabulary: got %+v %v, want an additive report that names them (the four additive vocabularies of the recording and future_set) and is not Breaking", r, err)
	}
	for _, d := range r.Diffs {
		if !d.Exact() {
			t.Errorf("an additive change moved %s: %+v", d.Name, d)
		}
	}
	onlyCode := editDescribe(t, func(doc map[string]any) {
		doc["error_codes"] = append(errorCodeList(t, doc), map[string]any{"code": "some_new_code", "http_status": 418})
		for _, name := range additiveVocabularies { // the four additive vocabularies of the recording: with them gone the code is the only thing to note
			delete(obj(t, doc["vocabularies"]), name)
		}
	})
	b, _ := describeAdapter(t, onlyCode)
	if r, err := b.CheckVocabularies(testCtx(t)); err != nil || r.Breaking() || !r.Additive() || len(r.VocabulariesExtra) != 0 || len(r.CodesExtra) != 1 {
		t.Fatalf("an extra code alone: got %+v %v", r, err)
	}
	// control: the recording is clean
	c, _ := describeAdapter(t, fixture(t, "describe-ok.json"))
	if r, err := c.CheckVocabularies(testCtx(t)); err != nil || r.Breaking() || len(r.CodesMissing) != 0 || len(r.CodesExtra) != 0 {
		t.Fatalf("control: the recording gave %+v %v", r, err)
	}
}

// TestCompareVocabularies_AnEmptyDescribeBreaksEverything: the pure comparison of a describe that holds nothing says every one of the seven vocabularies is not advertised, all ten error
// codes are missing, and the report is Breaking at the first vocabulary; it is deterministic (sorted, in the adapter's order). Control: the report of the recorded describe is not.
func TestCompareVocabularies_AnEmptyDescribeBreaksEverything(t *testing.T) {
	r := CompareVocabularies(&modclient.Describe{})
	if !r.Breaking() || r.firstBreak() != "vocabularies.status" || len(r.Diffs) != 7 || len(r.CodesMissing) != 10 || len(r.CodesExtra) != 0 || r.Additive() {
		t.Fatalf("an empty describe: %+v", r)
	}
	for i, d := range r.Diffs {
		if d.Advertised || !d.Breaking() || d.Name != vocabOrder[i] {
			t.Errorf("diff %d: %+v", i, d)
		}
	}
	if !sort.StringsAreSorted(r.CodesMissing) {
		t.Errorf("the missing codes are not sorted: %v", r.CodesMissing)
	}
	// only the codes broken: all vocabularies fine, a code missing: the break is the codes
	d := &modclient.Describe{Vocabularies: modclient.Vocabularies{}, ErrorCodes: []modclient.ErrorCode{{Code: "unauthenticated", HTTPStatus: 401}}}
	for name, vals := range vocabSets() {
		d.Vocabularies[name] = vals
	}
	if r := CompareVocabularies(d); r.firstBreak() != "error_codes" || !r.Breaking() || len(r.CodesMissing) != 9 {
		t.Fatalf("vocabularies fine and nine codes missing: %+v (first break %q)", r, r.firstBreak())
	}
	for _, code := range errorCodeValues {
		d.ErrorCodes = append(d.ErrorCodes, modclient.ErrorCode{Code: code})
	}
	if r := CompareVocabularies(d); r.Breaking() || !r.Diffs[0].Exact() {
		t.Fatalf("control: a describe that lists everything the adapter handles is Breaking: %+v", r)
	}
}

// TestDescribeAndCheckVocabularies_NeverReturnAValueBesideAnError: for every way the call can fail (the module answers 401, 404, 503 paused, 503 not ready, 500, a body that is not JSON, a
// body of the wrong shape, another module's describe, a connection that is refused, a context that is already over) Describe returns a nil document and CheckVocabularies the zero
// report, each with a typed error; and for a describe whose vocabulary broke, which Describe hands over as the module's own document, CheckVocabularies returns the zero report beside
// its error. Control: the same two calls over the recording return values and no error.
func TestDescribeAndCheckVocabularies_NeverReturnAValueBesideAnError(t *testing.T) {
	// a describe that is the module's own and whose vocabulary lost a value: Describe returns it (comparing vocabularies is CheckVocabularies'), CheckVocabularies refuses it
	lost := serve(t, 200, editDescribe(t, func(doc map[string]any) { vocabEntry(t, doc, "status")["values"] = vocabValues(t, doc, "status")[1:] })).adapter(t)
	if d, err := lost.Describe(testCtx(t)); err != nil || d == nil {
		t.Fatalf("Describe of a document with a vocabulary that lost a value: %+v %v, want the document", d, err)
	}
	if r, err := lost.CheckVocabularies(testCtx(t)); !reflect.DeepEqual(r, VocabReport{}) || modclient.KindOf(err) != modclient.Contract {
		t.Errorf("a vocabulary that lost a value: CheckVocabularies gave %+v %v, want the zero report and a Contract error", r, err)
	}
	good := fixture(t, "describe-ok.json")
	rows := []struct {
		name   string
		status int
		body   string
	}{
		{"401", 401, fixture(t, "status-error-unauthenticated.json")},
		{"404", 404, fixture(t, "status-error-not-found.json")},
		{"503 paused", 503, fixture(t, "submit-error-paused.json")},
		{"503 not ready", 503, fixture(t, "submit-error-unavailable.json")},
		{"500", 500, fixture(t, "submit-error-internal.json")},
		{"not JSON", 200, `describe`},
		{"the wrong shape", 200, `{"unexpected":true}`},
		{"a 201", 201, good},
		{"another module", 200, editDescribe(t, func(doc map[string]any) { doc["module"] = "send_managed" })},
	}
	for _, row := range rows {
		a := serve(t, row.status, row.body).adapter(t)
		d, err := a.Describe(testCtx(t))
		if d != nil || modclient.KindOf(err) == 0 {
			t.Errorf("%s: Describe gave %+v %v, want a typed error and no value", row.name, d, err)
		}
		r, err := a.CheckVocabularies(testCtx(t))
		if !reflect.DeepEqual(r, VocabReport{}) || modclient.KindOf(err) == 0 {
			t.Errorf("%s: CheckVocabularies gave %+v %v, want a typed error and the zero report", row.name, r, err)
		}
	}
	// a connection that is refused
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	da, err := New(deadURL, config.Secret(newToken(t)), nil, modclient.Options{Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := da.Describe(testCtx(t)); d != nil || modclient.KindOf(err) != modclient.Unavailable {
		t.Errorf("a refused connection: Describe gave %+v %v", d, err)
	}
	if r, err := da.CheckVocabularies(testCtx(t)); !reflect.DeepEqual(r, VocabReport{}) || modclient.KindOf(err) != modclient.Unavailable {
		t.Errorf("a refused connection: CheckVocabularies gave %+v %v", r, err)
	}
	// a context that is already over
	ga, gf := describeAdapter(t, good)
	over, cancel := context.WithCancel(testCtx(t))
	cancel()
	if d, err := ga.Describe(over); d != nil || modclient.KindOf(err) != modclient.Canceled {
		t.Errorf("a cancelled context: Describe gave %+v %v", d, err)
	}
	if r, err := ga.CheckVocabularies(over); !reflect.DeepEqual(r, VocabReport{}) || modclient.KindOf(err) != modclient.Canceled || gf.count() != 0 {
		t.Errorf("a cancelled context: CheckVocabularies gave %+v %v after %d request(s)", r, err, gf.count())
	}
	// control: the recording
	d, err := ga.Describe(testCtx(t))
	r, rerr := ga.CheckVocabularies(testCtx(t))
	if d == nil || err != nil || rerr != nil || len(r.Diffs) != 7 {
		t.Fatalf("control: the recording gave %+v %v and %+v %v", d, err, r, rerr)
	}
}
