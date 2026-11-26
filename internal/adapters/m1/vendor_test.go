package m1

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The vendor fallback in module 1's describe (vendor.go; module 1 migration 0020, owner ruling C3).
//
// THE BODIES BELOW ARE HAND-WRITTEN. The recorded fixture of the describe (testdata/contract/describe-ok.json) is a recording of module 1 BEFORE it carried `vendor_fallback`, and is re-recorded
// after the pin moves to the module's new head; until then the only describe that has the field is one built here, from that recording plus the one key the module's code (control-plane/internal/api/
// describe.go, describeBody) adds: `"vendor_fallback": {"reachable": <bool>}`. So every test here names its body "hand-written", and the control of each is the unchanged recording, which must read as
// "not said" (the parser accepts BOTH: the old describe and the new one).

// withVendorFallback is the HAND-WRITTEN edit that gives the recorded describe the key the new module adds; v is what goes under it.
func withVendorFallback(v any) func(doc map[string]any) {
	return func(doc map[string]any) { doc["vendor_fallback"] = v }
}

func vendorOf(t *testing.T, body string) (VendorReachable, error, *fakeM1) {
	t.Helper()
	a, f := describeAdapter(t, body)
	got, err := a.VendorReachable(testCtx(t))
	return got, err, f
}

func TestVendorReachable_ReadsTheFieldAndSaysWhenTheDescribeHasNone(t *testing.T) {
	// HAND-WRITTEN: the recorded describe plus vendor_fallback true / false
	on, err, f := vendorOf(t, editDescribe(t, withVendorFallback(map[string]any{"reachable": true})))
	if err != nil || on != (VendorReachable{Known: true, Reachable: true}) {
		t.Fatalf("a describe that says reachable=true read as %+v, %v; want {Known: true, Reachable: true}", on, err)
	}
	off, err, _ := vendorOf(t, editDescribe(t, withVendorFallback(map[string]any{"reachable": false})))
	if err != nil || off != (VendorReachable{Known: true, Reachable: false}) {
		t.Fatalf("a describe that says reachable=false read as %+v, %v; want {Known: true, Reachable: false}: a false that was SAID is not a false that was not said", off, err)
	}
	// the control of both: the RECORDING (module 1 before the field) is accepted and is "not said", never a silent false
	old, err, _ := vendorOf(t, fixture(t, "describe-ok.json"))
	if err != nil || old != (VendorReachable{}) || old.Known {
		t.Fatalf("the recorded describe (no vendor_fallback) read as %+v, %v; want {Known: false} and no error", old, err)
	}
	if on == off || off == old || on == old {
		t.Error("the three answers (reachable, not reachable, not said) are not three different values")
	}

	// one request, to the describe route, with the caller's key
	calls := f.seen()
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/v1/describe" || calls[0].Auth != "Bearer "+f.key {
		t.Errorf("the read made %+v, want one authenticated GET /v1/describe", calls)
	}

	// a key module 1 adds INSIDE vendor_fallback later is tolerated (the object is open), and so is any other additive key of the describe
	extra, err, _ := vendorOf(t, editDescribe(t, withVendorFallback(map[string]any{"reachable": true, "future": map[string]any{"a": 1}})))
	if err != nil || extra != (VendorReachable{Known: true, Reachable: true}) {
		t.Errorf("an extra key inside vendor_fallback read as %+v, %v; want the answer all the same", extra, err)
	}
}

func TestVendorReachable_AMalformedFieldIsAContractErrorNamingItAndNeverAFalse(t *testing.T) {
	// HAND-WRITTEN: every way the new key can be wrong
	for _, c := range []struct {
		name string
		v    any
		key  string
	}{
		{"a string", "yes", "vendor_fallback"},
		{"a boolean", true, "vendor_fallback"},
		{"an array", []any{true}, "vendor_fallback"},
		{"null", nil, "vendor_fallback"},
		{"an empty object", map[string]any{}, "vendor_fallback.reachable"},
		{"reachable as text", map[string]any{"reachable": "true"}, "vendor_fallback.reachable"},
		{"reachable as a number", map[string]any{"reachable": 1}, "vendor_fallback.reachable"},
		{"reachable null", map[string]any{"reachable": nil}, "vendor_fallback.reachable"},
		{"another key instead", map[string]any{"enabled": true}, "vendor_fallback.reachable"},
	} {
		got, err, _ := vendorOf(t, editDescribe(t, withVendorFallback(c.v)))
		e := asModclient(err)
		if got != (VendorReachable{}) || e == nil || e.Kind != modclient.Contract || e.Key != c.key || e.Route != "m1.describe" || e.Module != modclient.M1 {
			t.Errorf("%s: read as %+v, %v; want the zero value and a Contract error naming %q on m1.describe", c.name, got, err, c.key)
		}
	}
	// and the same describe is refused by Describe itself, naming the same key (one place decides)
	a, _ := describeAdapter(t, editDescribe(t, withVendorFallback(map[string]any{"reachable": "true"})))
	if d, err := a.Describe(testCtx(t)); d != nil || asModclient(err) == nil || asModclient(err).Key != "vendor_fallback.reachable" {
		t.Errorf("Describe on a retyped flag returned %+v, %v; want no describe and a Contract error naming vendor_fallback.reachable", d, err)
	}
}

func TestVendorReachable_AnErrorIsNeverAnAnswer(t *testing.T) {
	good := editDescribe(t, withVendorFallback(map[string]any{"reachable": true}))
	for _, row := range []struct {
		name   string
		status int
		body   string
		kind   modclient.Kind
	}{
		{"the per-caller key refused", 401, fixture(t, "status-error-unauthenticated.json"), modclient.Refused},
		{"module 1 paused by its operator", 503, fixture(t, "submit-error-paused.json"), modclient.Paused},
		{"module 1 unavailable", 503, fixture(t, "submit-error-unavailable.json"), modclient.Unavailable},
		{"module 1 broken", 500, fixture(t, "submit-error-internal.json"), modclient.Internal},
	} {
		got, err, _ := vendorOf2(t, row.status, row.body)
		if got != (VendorReachable{}) || modclient.KindOf(err) != row.kind {
			t.Errorf("%s: read as %+v, %v; want the zero value and an error of kind %v", row.name, got, err, row.kind)
		}
	}
	// a describe of another module at this address is a wiring mistake, not an answer about module 1's vendor
	other := editDescribe(t, func(doc map[string]any) {
		doc["module"] = "send_byo"
		doc["vendor_fallback"] = map[string]any{"reachable": true}
	})
	if got, err, _ := vendorOf(t, other); got != (VendorReachable{}) || asModclient(err) == nil || asModclient(err).Key != "module" {
		t.Errorf("another module's describe read as %+v, %v; want the zero value and a Contract error naming module", got, err)
	}
	// the control: the good body reads (so the rows above fail for their reasons)
	if got, err, _ := vendorOf(t, good); err != nil || !got.Known || !got.Reachable {
		t.Fatalf("control: the good body read as %+v, %v", got, err)
	}
	// nil is refused, never read as "not said"
	if got, err := VendorReachableOf(nil); got != (VendorReachable{}) || modclient.KindOf(err) != modclient.Invalid {
		t.Errorf("VendorReachableOf(nil) = %+v, %v; want the zero value and an Invalid error", got, err)
	}
	// a describe that holds the object with no flag (not one Describe let through) is refused by name, never guessed
	d := &modclient.Describe{Raw: []byte(`{"vendor_fallback":{}}`)}
	if got, err := VendorReachableOf(d); got != (VendorReachable{}) || asModclient(err) == nil || asModclient(err).Key != "vendor_fallback.reachable" {
		t.Errorf("an object with no flag read as %+v, %v; want a Contract error naming vendor_fallback.reachable", got, err)
	}
	// an error never quotes what the module sent
	if _, err, _ := vendorOf(t, editDescribe(t, withVendorFallback(map[string]any{"reachable": "zzz-quoted-zzz"}))); err != nil && strings.Contains(err.Error(), "zzz-quoted-zzz") {
		t.Errorf("the error quotes the module's value: %v", err)
	}
}

// vendorOf2 is vendorOf for a stand-in that answers with the given status and body.
func vendorOf2(t *testing.T, status int, body string) (VendorReachable, error, *fakeM1) {
	t.Helper()
	f := serve(t, status, body)
	got, err := f.adapter(t).VendorReachable(testCtx(t))
	return got, err, f
}

func TestVendorReachable_TheDescribeShapeAcceptsBothTheOldAndTheNewModule(t *testing.T) {
	var shape modclient.Shape
	for _, c := range Contracts() {
		if c.Route == tplDescribe && c.Case == "ok" {
			shape = c.Shape
		}
	}
	decode := func(body string) error {
		return modclient.Decode(&modclient.Response{Status: 200, Body: []byte(body)}, nil, shape)
	}
	// HAND-WRITTEN bodies from the recording
	if err := decode(fixture(t, "describe-ok.json")); err != nil {
		t.Fatalf("the recording (module 1 before the field) is refused by the contract's shape: %v", err)
	}
	for _, v := range []bool{true, false} {
		if err := decode(editDescribe(t, withVendorFallback(map[string]any{"reachable": v}))); err != nil {
			t.Errorf("a describe with vendor_fallback.reachable=%v is refused by the contract's shape: %v", v, err)
		}
	}
	// the shape LISTS the key (it is not merely tolerated as an unknown one): a retyped flag is refused, and the refusal names it
	err := decode(editDescribe(t, withVendorFallback(map[string]any{"reachable": "false"})))
	if e := asModclient(err); e == nil || e.Kind != modclient.Contract || e.Key != "vendor_fallback.reachable" {
		t.Errorf("a retyped flag was judged %v, want a Contract error naming vendor_fallback.reachable", err)
	}
	// the key is OPTIONAL: it is the only key of the shape that the recording lacks and the shape accepts the lack of
	found := false
	for _, f := range shape.Fields() {
		if f.Name == "vendor_fallback" {
			found = true
			if f.Required {
				t.Error("vendor_fallback is required by the shape: a module before migration 0020 would be refused")
			}
		}
	}
	if !found {
		t.Error("the describe shape does not list vendor_fallback")
	}
}
