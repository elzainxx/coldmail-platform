package modclient

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// describeM1 is module 1's GET /v1/describe in the shape control-plane/internal/api/describe.go writes (the Phase 3 close-out, partial Tier 4): module "verify", capabilities with
// idempotent_bulk_create and job_totals true and quote, run, report false, routes, schemas, eight vocabularies each with source, values and fields, error_codes sorted by status then
// code, and the idempotency contract of the bulk create. There is NO `pricing`. The vocabularies are abridged to a few values each (the adapter's own tests hold the full sets);
// nothing here is a secret or an address.
const describeM1 = `{"module":"verify","api_version":"v1",
"capabilities":{"sync_verify":true,"bulk_verify":true,"idempotent_bulk_create":true,"job_totals":true,"quote":false,"run":false,"report":false},
"routes":[
 {"id":"verify","method":"POST","path":"/v1/verify","auth":"per_caller_key_or_legacy_token","description":"Verify one address synchronously.","request_schema_ref":"#/schemas/verify_request","response_schema_ref":"#/schemas/verdict"},
 {"id":"verify.bulk.create","method":"POST","path":"/v1/verify/bulk","auth":"per_caller_key","description":"Submit a job of addresses.","request_schema_ref":"#/schemas/bulk_request","response_schema_ref":"#/schemas/bulk_accepted_response"},
 {"id":"verify.bulk.status","method":"GET","path":"/v1/jobs/{id}","auth":"per_caller_key","description":"A job's status.","response_schema_ref":"#/schemas/job_status_response"},
 {"id":"describe","method":"GET","path":"/v1/describe","auth":"per_caller_key","description":"This document."}],
"schemas":{"verify_request":{"type":"object","properties":{"email":{"type":"string"}},"required":["email"]}},
"vocabularies":{
 "status":{"source":"module_constant","values":["valid","invalid","risky","unknown"],"fields":["verify.status","job.results[].status"]},
 "reason":{"source":"module_constant","values":["invalid_syntax","accepted","no_mx"],"fields":["verify.reason"]},
 "recommendation":{"source":"module_constant","values":["send","review","skip"],"fields":["verify.recommendation"]},
 "failure_origin":{"source":"module_constant","values":["own","remote","undetermined"],"fields":["verify.signals.failure_origin"]},
 "smtp_skipped_reason":{"source":"module_constant","values":["no_eligible_node","audit_red"],"fields":["verify.signals.smtp_skipped_reason"]},
 "job_status":{"source":"module_constant","values":["submitted","running","complete","complete_with_unknowns"],"fields":["job.status"]},
 "submit_status":{"source":"module_constant","values":["accepted"],"fields":["bulk.accepted.status"]},
 "detail_issue":{"source":"module_constant","values":["required","malformed","out_of_range","not_found","conflict","duplicate"],"fields":["error.details[].issue"]}},
"error_codes":[{"code":"validation_failed","http_status":400},{"code":"unauthenticated","http_status":401},{"code":"not_found","http_status":404},{"code":"method_not_allowed","http_status":405},
 {"code":"idempotency_key_reused","http_status":409},{"code":"payload_too_large","http_status":413},{"code":"contract_violation","http_status":500},{"code":"internal_error","http_status":500},
 {"code":"paused","http_status":503},{"code":"unavailable","http_status":503}],
"idempotency":{"header":"Idempotency-Key","key_pattern":"^[A-Za-z0-9._:-]{1,128}$","replay_response_header":"Idempotent-Replayed","reused_with_other_request":"idempotency_key_reused","route":"POST /v1/verify/bulk","scope":"authenticated_caller"}}`

// TestDescribe_Module1MakesTheCallAndDecodesItsOwnShape: a client built for module 1 now asks the module (GET /v1/describe, with the per-caller key as the bearer credential, once) and
// decodes the family's keys of its describe: module, api_version, capabilities, eight vocabularies with their values, ten error codes, and NO pricing (the zero Pricing). Raw is
// the whole body, so the adapter can read `idempotency` and `routes`. Vocabularies is the same sets. Paired controls: the very same body is a Contract error naming `pricing` for a
// client built for module 2 (their shape is not relaxed), and module 2's own describe, which has pricing, still decodes for module 1 (pricing is optional there, not forbidden).
func TestDescribe_Module1MakesTheCallAndDecodesItsOwnShape(t *testing.T) {
	tok := mustToken(t)
	s := serve(t, 200, "application/json", describeM1)
	c := s.client(t, M1, tok, Options{})
	d, err := c.Describe(ctxT(t, 5*time.Second))
	if err != nil || d == nil {
		t.Fatalf("%v", err)
	}
	if s.count() != 1 {
		t.Fatalf("describe of module 1 made %d request(s), want 1: it is no longer answered without a call", s.count())
	}
	if r := s.requests()[0]; r.method != "GET" || r.path != "/v1/describe" || r.auth != "Bearer "+tok {
		t.Fatalf("describe needs the per-caller key as the credential: %+v", r)
	}
	if d.Module != "verify" || d.APIVersion != "v1" || !d.Capabilities["idempotent_bulk_create"] || !d.Capabilities["job_totals"] || d.Capabilities["quote"] || d.Capabilities["run"] || d.Capabilities["report"] {
		t.Fatalf("%+v", d)
	}
	if p := d.Pricing; p.Currency != "" || p.Model != "" || p.UnitPriceCredits != 0 || p.Models != nil || p.CostFieldsInert {
		t.Fatalf("module 1 has no pricing, yet %+v", p)
	}
	if len(d.Vocabularies) != 8 || len(d.ErrorCodes) != 10 {
		t.Fatalf("%d vocabularies and %d error codes, want 8 and 10", len(d.Vocabularies), len(d.ErrorCodes))
	}
	if vals, ok := d.Vocabularies.Values("status"); !ok || strings.Join(vals, ",") != "valid,invalid,risky,unknown" {
		t.Fatalf("status = %v", vals)
	}
	found := false
	for _, ec := range d.ErrorCodes {
		found = found || (ec.Code == "idempotency_key_reused" && ec.HTTPStatus == 409)
	}
	if !found {
		t.Fatalf("error_codes %v lack idempotency_key_reused/409", d.ErrorCodes)
	}
	if !bytes.Equal(d.Raw, []byte(describeM1)) {
		t.Fatal("Raw is not the response body")
	}
	var raw struct {
		Idempotency struct {
			Header string `json:"header"`
		} `json:"idempotency"`
		Routes []json.RawMessage `json:"routes"`
	}
	if err := json.Unmarshal(d.Raw, &raw); err != nil || raw.Idempotency.Header != "Idempotency-Key" || len(raw.Routes) != 4 {
		t.Fatalf("Raw does not carry the module-specific keys: %+v, %v", raw, err)
	}
	v, err := c.Vocabularies(ctxT(t, 5*time.Second))
	if err != nil || len(v) != 8 || s.count() != 2 {
		t.Fatalf("Vocabularies: %d sets, %v, %d request(s)", len(v), err, s.count())
	}
	if !v.Diff("job_status", []string{"submitted", "running", "complete", "complete_with_unknowns"}).Exact() {
		t.Fatalf("job_status: %+v", v.Diff("job_status", []string{"submitted", "running", "complete", "complete_with_unknowns"}))
	}

	// control: module 2's and module 3's shape is not relaxed: a describe with no pricing is refused for them, naming the key
	for _, m := range []Module{M2, M3} {
		o := serve(t, 200, "application/json", describeM1)
		od, err := o.client(t, m, tok, Options{}).Describe(ctxT(t, 5*time.Second))
		if od != nil {
			t.Fatalf("%s: a describe with no pricing was accepted", m)
		}
		if e := wantKind(t, err, Contract); e.Key != "pricing" || e.Route != string(m)+".describe" {
			t.Fatalf("%s: %+v", m, *e)
		}
	}
	// control: a describe that DOES carry module 2's pricing is read for module 1 too (optional is not forbidden), with the pricing decoded
	p := serve(t, 200, "application/json", describeM2)
	pd, err := p.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second))
	if err != nil || pd == nil || pd.Pricing.Currency != "credits" || pd.Pricing.UnitPriceCredits != 1 {
		t.Fatalf("a describe with pricing for module 1: %+v, %v", pd, err)
	}
}

// TestDescribe_Module1IsDecodedStrictly: a describe that breaks module 1's shape is a Contract error naming the key and returns no value: a missing module, api_version, capabilities
// or error_codes; a capability that is not a boolean; a vocabulary entry with no `values`, or whose values are not an array of strings; an error code whose status is a string; a
// pricing that is present and wrong. Keys it does not list (source, fields, routes, schemas, idempotency, an extra capability) are tolerated. Paired control for the one place module 1
// is STRICTER than the others: the same entry without `values` is accepted for module 2, whose shape says an entry without a list is not a vocabulary (and the unmodified body is
// accepted for module 1).
func TestDescribe_Module1IsDecodedStrictly(t *testing.T) {
	tok := mustToken(t)
	rows := []struct {
		name string
		from string
		to   string
		key  string
	}{
		{"module missing", `"module":"verify",`, ``, "module"},
		{"module a number", `"module":"verify"`, `"module":1`, "module"},
		{"api_version missing", `"api_version":"v1",`, ``, "api_version"},
		{"capabilities missing", `"capabilities":{"sync_verify":true,"bulk_verify":true,"idempotent_bulk_create":true,"job_totals":true,"quote":false,"run":false,"report":false},`, ``, "capabilities"},
		{"a capability that is not a boolean", `"idempotent_bulk_create":true`, `"idempotent_bulk_create":"yes"`, "capabilities.idempotent_bulk_create"},
		{"vocabularies missing", `"vocabularies":{`, `"unrelated":{`, "vocabularies"},
		{"a vocabulary entry with no values", `"values":["valid","invalid","risky","unknown"],`, ``, "vocabularies.status.values"},
		{"a vocabulary whose values are not an array", `"values":["send","review","skip"]`, `"values":"send"`, "vocabularies.recommendation.values"},
		{"a vocabulary value that is not a string", `"values":["own","remote","undetermined"]`, `"values":["own",3,"undetermined"]`, "vocabularies.failure_origin.values[1]"},
		{"a vocabulary entry that is not an object", `"submit_status":{"source":"module_constant","values":["accepted"],"fields":["bulk.accepted.status"]}`, `"submit_status":["accepted"]`, "vocabularies.submit_status"},
		{"error_codes missing", `"error_codes":[`, `"error_codez":[`, "error_codes"},
		{"an error code with a status that is a string", `{"code":"paused","http_status":503}`, `{"code":"paused","http_status":"503"}`, "error_codes[8].http_status"},
		{"an error code without a code", `{"code":"validation_failed","http_status":400}`, `{"http_status":400}`, "error_codes[0].code"},
		{"a pricing that is present and wrong", `"capabilities":{`, `"pricing":{"currency":5},"capabilities":{`, "pricing.currency"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(describeM1, row.from) != 1 {
				t.Fatalf("the fixture holds %q %d time(s), want once", row.from, strings.Count(describeM1, row.from))
			}
			s := serve(t, 200, "application/json", strings.Replace(describeM1, row.from, row.to, 1))
			d, err := s.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second))
			if d != nil {
				t.Fatalf("a Describe came back with %v", err)
			}
			if e := wantKind(t, err, Contract); e.Key != row.key || e.Route != "m1.describe" || e.Module != M1 {
				t.Fatalf("%+v", *e)
			}
		})
	}
	for _, body := range []string{`[]`, `"describe"`, `{}`, ``, `null`} {
		s := serve(t, 200, "application/json", body)
		d, err := s.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second))
		if d != nil {
			t.Fatalf("a Describe came back for %q", body)
		}
		wantKind(t, err, Contract)
	}

	// tolerated: keys the shape does not list, a vocabulary without its source or fields, an extra capability, an extra vocabulary
	tolerated := strings.NewReplacer(
		`"fields":["verify.status","job.results[].status"]`, `"extra_key":1`,
		`"source":"module_constant","values":["send","review","skip"]`, `"values":["send","review","skip"]`,
		`"report":false}`, `"report":false,"some_future_capability":true}`,
		`"routes":[`, `"future_key":{"a":1},"routes":[`,
	).Replace(describeM1)
	if tolerated == describeM1 {
		t.Fatal("setup: the tolerated body is the fixture")
	}
	s := serve(t, 200, "application/json", tolerated)
	if d, err := s.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second)); err != nil || d == nil || !d.Capabilities["some_future_capability"] {
		t.Fatalf("a describe with keys the shape does not list was refused: %v", err)
	}

	// the paired control: the one place module 1 is stricter. An entry without `values` is accepted (and is no vocabulary) for module 2, refused for module 1
	noValuesM2 := strings.Replace(describeM2, `"pool_kind":{"source":"db_enum","values":["shared","dedicated"],"fields":["pool.kind"]}`, `"pool_kind":{"source":"db_enum"}`, 1)
	if noValuesM2 == describeM2 {
		t.Fatal("setup: module 2's fixture has no pool_kind entry to empty")
	}
	m2 := serve(t, 200, "application/json", noValuesM2)
	if d, err := m2.client(t, M2, tok, Options{}).Describe(ctxT(t, 5*time.Second)); err != nil || d == nil {
		t.Fatalf("control: module 2's entry without values was refused: %v", err)
	} else if _, ok := d.Vocabularies.Values("pool_kind"); ok {
		t.Fatal("control: an entry without values was taken for a vocabulary")
	}
	m1 := serve(t, 200, "application/json", noValuesM2)
	if d, err := m1.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second)); d != nil {
		t.Fatalf("module 1's client accepted an entry without values: %v", err)
	} else if e := wantKind(t, err, Contract); e.Key != "vocabularies.pool_kind.values" {
		t.Fatalf("%+v", *e)
	}
	ok := serve(t, 200, "application/json", describeM1)
	if d, err := ok.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second)); err != nil || d == nil {
		t.Fatalf("control: the unmodified body was refused: %v", err)
	}
}

// TestDescribe_Module1AnswersAreTheModulesNotAShapeProblem: what module 1 says about the call itself keeps its class and returns no value: the legacy static token (module 1 refuses
// it on describe) is a 401 Refused, a missing route a NotFound, a pause Paused, a fault Internal and a stopped front door Unavailable; an idempotent read is retried like module 2's
// (a 503 then the document: two attempts, the document). Paired control: the same client against the document gets it.
func TestDescribe_Module1AnswersAreTheModulesNotAShapeProblem(t *testing.T) {
	tok := mustToken(t)
	for _, row := range []struct {
		status int
		body   string
		kind   Kind
	}{
		{401, m1Unauthenticated, Refused},
		{404, m1NotFound, NotFound},
		{500, m1Internal, Internal},
		{503, m1Paused, Paused},
		{503, m1NotWired, Unavailable},
	} {
		s := serve(t, row.status, "application/json", row.body)
		d, err := s.client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second))
		if d != nil {
			t.Fatalf("status %d: a Describe came back", row.status)
		}
		wantKind(t, err, row.kind)
		v, err := s.client(t, M1, tok, Options{}).Vocabularies(ctxT(t, 5*time.Second))
		if v != nil {
			t.Fatalf("status %d: a Vocabularies came back: %v", row.status, v)
		}
		wantKind(t, err, row.kind)
	}

	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			jsonReply(w, 503, m1NotWired)
			return
		}
		jsonReply(w, 200, describeM1)
	})
	d, err := s.client(t, M1, tok, Options{Retries: 2, Sleep: (&recorder{}).sleep}).Describe(ctxT(t, 5*time.Second))
	if err != nil || d == nil || s.count() != 2 {
		t.Fatalf("a 503 then the document: %v, %d request(s), want the document after 2", err, s.count())
	}
	if a, err := serve(t, 503, "application/json", m1NotWired).client(t, M1, tok, Options{}).Describe(ctxT(t, 5*time.Second)); a != nil || err == nil {
		t.Fatalf("control: with no retries the first 503 is the answer: %v %v", a, err)
	}
}
