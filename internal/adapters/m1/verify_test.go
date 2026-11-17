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

// TestVerify_SendsTheAddressAndMapsTheRecordedVerdict: POST /v1/verify with the bearer key and a body of exactly {"email": <the address as given>}; the recorded verdict decodes to
// the fields the platform shows and charges on, and its Outcome is module 1's own words. Control: a different recorded verdict (an unknown with its origin) decodes to a different
// Verdict and a different Outcome.
func TestVerify_SendsTheAddressAndMapsTheRecordedVerdict(t *testing.T) {
	body := fixture(t, "verify-ok.json")
	f := newFakeM1(t, func(call) (int, string) { return 200, body })
	a := f.adapter(t)
	v, err := a.Verify(testCtx(t), " Ada@Acme.example ")
	if err != nil {
		t.Fatal(err)
	}
	want := Verdict{Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send", Domain: "acme.example"}
	if v != want {
		t.Fatalf("Verify returned %+v, want %+v", v, want)
	}
	if o := v.Outcome(); o != (charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}) {
		t.Fatalf("outcome %+v", o)
	}
	r := f.seen()[0]
	var sent map[string]string
	if r.Method != http.MethodPost || r.Path != "/v1/verify" || r.Auth != "Bearer "+f.key || json.Unmarshal(r.Body, &sent) != nil || len(sent) != 1 || sent["email"] != " Ada@Acme.example " {
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
// with a zero field that reads as an answer). Each case is paired with the recorded verdict, which is accepted.
func TestVerify_RefusesAVerdictOffContract(t *testing.T) {
	good := fixture(t, "verify-ok.json")
	mut := func(from, to string) string {
		if !strings.Contains(good, from) {
			t.Fatalf("setup: the recorded verdict has no %q", from)
		}
		return strings.Replace(good, from, to, 1)
	}
	if v, err := serve(t, 200, good).adapter(t).Verify(testCtx(t), "ada@acme.example"); err != nil || v.Status != "valid" {
		t.Fatalf("control: the recorded verdict was refused: %v", err)
	}
	cases := []struct{ name, body, key string }{
		{"a status outside the four", mut(`"status": "valid"`, `"status": "fine"`), "status"},
		{"a reason outside the vocabulary", mut(`"reason": "accepted"`, `"reason": "vibes"`), "reason"},
		{"a recommendation outside the three", mut(`"recommendation": "send"`, `"recommendation": "yes"`), "recommendation"},
		{"a confidence over 100", mut(`"confidence": 90`, `"confidence": 190`), "confidence"},
		{"a confidence of the wrong type", mut(`"confidence": 90`, `"confidence": "90"`), "confidence"},
		{"no flags.from_cache", mut(`"from_cache": false,
    "has_typo"`, `"has_typo"`), "flags.from_cache"},
		{"a failure origin outside the three", mut(`"mx_found": true`, `"failure_origin": "theirs", "mx_found": true`), "signals.failure_origin"},
		{"a skip token outside the six", mut(`"mx_found": true`, `"smtp_skipped_reason": "lazy", "mx_found": true`), "signals.smtp_skipped_reason"},
		{"no domain", mut(`"domain": "acme.example",`, ``), "domain"},
	}
	for _, c := range cases {
		v, err := serve(t, 200, c.body).adapter(t).Verify(testCtx(t), "ada@acme.example")
		if kind, key := kindAndKey(err); kind != modclient.Contract || key != c.key || v != (Verdict{}) {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no verdict and a Contract error naming %q", c.name, v, kind, key, err, c.key)
		}
	}
}
