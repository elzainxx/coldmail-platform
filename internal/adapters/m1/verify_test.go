package m1

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/norm"
)

// TestVerify_SendsTheAddressAndMapsTheRecordedVerdict: POST /v1/verify with the bearer key and a body of exactly {"email": <the address as given>}; the verdict RECORDED from the
// real node decodes to the fields the platform shows and charges on (what an independent parse of the file says: status, reason, confidence, recommendation, domain, the cache flag,
// and the skip token and failure origin when the file has them), and its Outcome is module 1's own words. Control: a different verdict (an unknown with its origin, written inline)
// decodes to a different Verdict and a different Outcome.
func TestVerify_SendsTheAddressAndMapsTheRecordedVerdict(t *testing.T) {
	body := fixture(t, "verify-ok.json")
	doc := recordedDoc(t, "verify-ok.json")
	sig := obj(t, doc["signals"])
	origin, _ := sig["failure_origin"].(string)
	skip, _ := sig["smtp_skipped_reason"].(string)
	want := Verdict{
		Status: doc["status"].(string), Reason: doc["reason"].(string), Confidence: num(t, doc["confidence"]), Recommendation: doc["recommendation"].(string),
		Domain: doc["domain"].(string), FromCache: obj(t, doc["flags"])["from_cache"].(bool), SkipReason: skip, FailureOrigin: origin,
	}
	if want.Status == "" || want.Reason == "" || want.Domain == "" || want.Recommendation == "" {
		t.Fatalf("setup: the recording lacks a verdict field: %+v", want)
	}
	address := " Ada@" + want.Domain + " " // case and surrounding space are the caller's: the adapter sends it as given
	f := newFakeM1(t, func(call) (int, string) { return 200, body })
	a := f.adapter(t)
	v, err := a.Verify(testCtx(t), address)
	if err != nil {
		t.Fatal(err)
	}
	if v != want {
		t.Fatalf("Verify returned %+v, want %+v (what the recorded file says)", v, want)
	}
	wantOutcome := charge.Outcome{Module: "m1", State: want.Status, Reason: want.Reason, FromCache: want.FromCache, SkipReason: skip, FailureOrigin: origin}
	if o := v.Outcome(); o != wantOutcome {
		t.Fatalf("outcome %+v, want %+v", o, wantOutcome)
	}
	r := f.seen()[0]
	var sent map[string]string
	if r.Method != http.MethodPost || r.Path != "/v1/verify" || r.Auth != "Bearer "+f.key || json.Unmarshal(r.Body, &sent) != nil || len(sent) != 1 || sent["email"] != address {
		t.Fatalf("the request was %+v (body %s), want POST /v1/verify with exactly {\"email\": the address as given}", r, r.Body)
	}

	body = `{"status":"unknown","reason":"dns_error","confidence":10,"recommendation":"review","domain":"gone.example","flags":{"from_cache":true},"signals":{"failure_origin":"remote","smtp_skipped_reason":"audit_red"}}`
	c, err := a.Verify(testCtx(t), "ada@gone.example")
	if err != nil || c == v {
		t.Fatalf("control: %+v %v", c, err)
	}
	if o := c.Outcome(); o != (charge.Outcome{Module: "m1", State: "unknown", Reason: "dns_error", FromCache: true, SkipReason: "audit_red", FailureOrigin: "remote"}) {
		t.Fatalf("control: outcome %+v carries not every input of the charge table", o)
	}
}

// TestVerify_AMalformedAddressIsNeverSent: an address that fails norm/v1 returns a Verdict that says why (Malformed), whose Outcome is platform/malformed and which the charge table
// rules Free by P-08, with no request. Control: the same call with a good address does send.
func TestVerify_AMalformedAddressIsNeverSent(t *testing.T) {
	f := serve(t, 200, fixture(t, "verify-ok.json"))
	a := f.adapter(t)
	for addr, reason := range map[string]norm.Reason{"no-at-sign.example": norm.ReasonNoAt, "two@@signs.example": norm.ReasonMultipleAt, "   ": norm.ReasonEmpty, "@x.example": norm.ReasonEmptyLocal} {
		v, err := a.Verify(testCtx(t), addr)
		if err != nil || v.Malformed != reason || v.Status != "" {
			t.Errorf("%q: got %+v %v, want a verdict that says malformed (%s) and nothing else", addr, v, err, reason)
			continue
		}
		if d := charge.Decide(v.Outcome()); d.Kind != charge.Free || d.Rule != "P-08" {
			t.Errorf("%q: decides %+v, want Free by P-08", addr, d)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for malformed addresses", f.count())
	}
	if v, err := a.Verify(testCtx(t), "ada@acme.example"); err != nil || v.Malformed != "" || f.count() != 1 {
		t.Fatalf("control: %+v %v after %d request(s)", v, err, f.count())
	}
}

// TestVerify_IsNeverRetried: a synchronous check is billed work (another SMTP session, another vendor call), so whatever the dials say it is sent ONCE: after a 503, a 500 and a
// stalled answer the error comes back and the stand-in has seen one request. Paired control: Status against the same 503 IS retried, so the dial (2 retries) is live.
func TestVerify_IsNeverRetried(t *testing.T) {
	for name, status := range map[string]int{"503": 503, "500": 500, "502": 502} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(fixtureErr("unavailable")))
		}))
		a, err := New(srv.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
		if err != nil {
			t.Fatal(err)
		}
		v, err := a.Verify(testCtx(t), "ada@acme.example")
		if err == nil || v != (Verdict{}) || n.Load() != 1 {
			t.Errorf("%s: got %+v %v after %d request(s), want an error, no verdict and exactly one request", name, v, err, n.Load())
		}
		if status != 500 { // a 500 is the module's own fault and is not retried even for a read; 502 and 503 are
			if _, err := a.Status(testCtx(t), testJob); err == nil || n.Load() != 1+3 {
				t.Errorf("%s control: Status made %d request(s) in all (%v), want 4 (the one Verify sent, then 1 + 2 retries)", name, n.Load(), err)
			}
		}
		srv.Close()
	}
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // read the request first, so the server notices the client hanging up and the handler ends with it
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer stall.Close()
	a, err := New(stall.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verify(ctxFor(t, 200*time.Millisecond), "ada@acme.example"); modclient.KindOf(err) != modclient.Timeout {
		t.Fatalf("a stalled module: got %v, want a Timeout error", err)
	}
}

// TestVerify_RefusesAVerdictOffContract: a missing key, a wrong type or a value outside the closed vocabulary is a Contract error naming the key and no verdict (never a verdict
// with a zero field that reads as an answer). The mutations are made on the verdict RECORDED from the real node by editing its parsed tree, so each is checked to apply (a
// mutation that touches nothing fails the test as proving nothing); the keys of the open signals bag that the recording does not have are added. Each case is paired with the
// recorded verdict itself, which is accepted.
func TestVerify_RefusesAVerdictOffContract(t *testing.T) {
	good := fixture(t, "verify-ok.json")
	address := "ada@" + recordedDoc(t, "verify-ok.json")["domain"].(string)
	if v, err := serve(t, 200, good).adapter(t).Verify(testCtx(t), address); err != nil || v.Status != "valid" {
		t.Fatalf("control: the recorded verdict was refused: %v", err)
	}
	cases := []mutation{
		outside("status", "fine"),
		outside("reason", "vibes"),
		outside("recommendation", "yes"),
		retype("confidence", 190),
		retype("confidence", "90"),
		remove("flags.from_cache"),
		add("signals.failure_origin", "theirs"),
		add("signals.smtp_skipped_reason", "lazy"),
		remove("domain"),
	}
	for _, m := range cases {
		v, err := serve(t, 200, m.apply(t, good)).adapter(t).Verify(testCtx(t), address)
		if kind, key := kindAndKey(err); kind != modclient.Contract || key != m.key || v != (Verdict{}) {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no verdict and a Contract error naming %q", m.name, v, kind, key, err, m.key)
		}
	}
}

// TestVerify_AnAddressThatFailsModuleOnesSyntaxLayerIsNeverSent: the synchronous check applies the same rule as Prepare (Phase 3 close-out A5, row M1-10 unreachable from the platform): an address
// that normalises but would be answered invalid/invalid_syntax (local part over 64 bytes, domain with no dot, domain over 255 bytes) returns a Malformed verdict with its syntax reason, free by
// P-08, and no request. Control: the boundary address one step inside the limit is sent.
func TestVerify_AnAddressThatFailsModuleOnesSyntaxLayerIsNeverSent(t *testing.T) {
	f := serve(t, 200, fixture(t, "verify-ok.json"))
	a := f.adapter(t)
	for addr, reason := range map[string]norm.Reason{
		strings.Repeat("a", 65) + "@acme.example": norm.ReasonLocalTooLong,
		"ada@localhost":                          norm.ReasonDomainNoDot,
		"ada@" + strings.Repeat("b.", 128) + "c": norm.ReasonDomainTooLong,
	} {
		v, err := a.Verify(testCtx(t), addr)
		if err != nil || v.Malformed != reason || v.Status != "" {
			t.Errorf("%.30q...: got %+v %v, want a verdict that says rejected (%s) and nothing else", addr, v, err, reason)
			continue
		}
		if d := charge.Decide(v.Outcome()); d.Kind != charge.Free || d.Rule != "P-08" {
			t.Errorf("%.30q...: decides %+v, want Free by P-08", addr, d)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for addresses module 1's syntax layer would fail", f.count())
	}
	if v, err := a.Verify(testCtx(t), strings.Repeat("a", 64)+"@acme.example"); err != nil || v.Malformed != "" || f.count() != 1 {
		t.Fatalf("control: %+v %v after %d request(s)", v, err, f.count())
	}
}
