package contractcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Addresses are built at run time (ip) so that no source line holds an address outside the documentation ranges.

func normJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	out := norm(t, body)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("the normalised body does not parse: %v\n%s", err, out)
	}
	return m
}

func TestNormaliseRewritesThreeDistinctAddressesRepeatedToDocumentationAddressesByFirstAppearance(t *testing.T) {
	a, b, c := ip(100, 128, 0, 2), ip(100, 128, 0, 3), ip(8, 8, 8, 8)
	body := fmt.Sprintf(`{"signals":{"engine_mx_hosts":[%q,%q,%q]},"meta":{"verifier_ip":%q},"again":%q,"more":[%q,%q]}`, a, b, a, c, a, b, c)
	out := norm(t, body)
	for _, original := range []string{a, b, c} {
		if strings.Contains(out, original) {
			t.Fatalf("an original address survived:\n%s", out)
		}
	}
	var got struct {
		Signals struct {
			Hosts []string `json:"engine_mx_hosts"`
		} `json:"signals"`
		Meta  map[string]string `json:"meta"`
		Again string            `json:"again"`
		More  []string          `json:"more"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	// a is the first to appear, then b, then c; a repeated address is the same one every time.
	if strings.Join(got.Signals.Hosts, ",") != "192.0.2.1,192.0.2.2,192.0.2.1" || got.Meta["verifier_ip"] != "192.0.2.3" || got.Again != "192.0.2.1" || strings.Join(got.More, ",") != "192.0.2.2,192.0.2.3" {
		t.Fatalf("mapping = %+v", got)
	}
	// The paired control: the raw body is secret-shaped and the output is not.
	if raw, _ := FindSecrets([]byte(body)); len(raw) == 0 {
		t.Fatal("the raw body must hold a public address (the control)")
	}
	if fs, _ := FindSecrets([]byte(out)); len(fs) != 0 {
		t.Fatalf("the normalised body must be clean, got %s", classes(fs))
	}
}

func TestNormaliseRewritesAnAddressInsideALongerStringInPlace(t *testing.T) {
	a, b := ip(100, 128, 0, 2), ip(100, 128, 0, 3)
	m := normJSON(t, fmt.Sprintf(`{"x":"mx.example [%s]:25","y":"%s -> %s","z":"(%s),<%s>;%s.","w":"%s:%s"}`, a, a, b, a, b, a, a, b))
	want := map[string]string{
		"x": "mx.example [192.0.2.1]:25",
		"y": "192.0.2.1 -> 192.0.2.2",
		"z": "(192.0.2.1),<192.0.2.2>;192.0.2.1.",
		"w": "192.0.2.1:192.0.2.2",
	}
	for k, w := range want {
		if m[k] != w {
			t.Fatalf("%s = %q, want %q", k, m[k], w)
		}
	}
}

func TestNormaliseRewritesAnAddressInAStringThatAlsoHoldsAUUIDAndATimestamp(t *testing.T) {
	a := ip(100, 128, 0, 2)
	m := normJSON(t, fmt.Sprintf(`{"msg":"job 3f2b8a10-5c1d-4e7f-9a0b-1c2d3e4f5a6b from %s at 2026-11-17T21:32:01Z"}`, a))
	if m["msg"] != "job 00000000-0000-4000-8000-000000000001 from 192.0.2.1 at 2026-01-01T00:00:00Z" {
		t.Fatalf("msg = %q", m["msg"])
	}
}

func TestNormaliseLeavesTextThatIsNotAnIPv4AddressAlone(t *testing.T) {
	notAddresses := []string{
		"1.2.3", ip(999, 1, 1, 1), "v" + ip(1, 2, 3, 4), ip(1, 2, 3, 4) + ".5", "x" + ip(1, 2, 3, 4), ip(1, 2, 3, 4) + "x", "." + ip(1, 2, 3, 4), "01." + "2.3.4", ip(1, 2, 3, 4) + "5.6",
	}
	for _, s := range notAddresses {
		m := normJSON(t, fmt.Sprintf(`{"v":%q}`, s))
		if m["v"] != s {
			t.Fatalf("%q is not an address and must be left alone, got %q", s, m["v"])
		}
	}
	// The paired control: the same digits as a real address are rewritten.
	if m := normJSON(t, fmt.Sprintf(`{"v":%q}`, ip(1, 2, 3, 4))); m["v"] != "192.0.2.1" {
		t.Fatalf("a real address must be rewritten, got %q", m["v"])
	}
}

func TestNormaliseMapsTheSameAddressInTwoFieldsAndInAKeyIdentically(t *testing.T) {
	a, b := ip(100, 128, 0, 2), ip(100, 128, 0, 9)
	out := norm(t, fmt.Sprintf(`{"first":%q,"nested":{%q:"v","other":%q},"last":%q}`, a, a, b, a))
	for _, want := range []string{`"first": "192.0.2.1"`, `"192.0.2.1": "v"`, `"other": "192.0.2.2"`, `"last": "192.0.2.1"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	// The paired control: two different addresses do not collapse into one.
	if strings.Count(out, "192.0.2.2") != 1 || strings.Count(out, "192.0.2.1") != 3 {
		t.Fatalf("distinct addresses must stay distinct:\n%s", out)
	}
}

func TestNormaliseMapsDocumentationAddressesToo(t *testing.T) {
	// A body that already holds documentation addresses is mapped like any other: a distinct input never shares an output with another (a literal 192.0.2.1 appearing after a
	// public address must not be confused with the public address's stand-in), and the same input is the same output.
	a := ip(100, 128, 0, 2)
	out := norm(t, fmt.Sprintf(`["%s","192.0.2.1","192.0.2.1","%s","203.0.113.7"]`, a, a))
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "192.0.2.1,192.0.2.2,192.0.2.2,192.0.2.1,192.0.2.3" {
		t.Fatalf("mapping = %v", got)
	}
}

func TestNormaliseWithAddressesIsDeterministicAndIdempotent(t *testing.T) {
	a, b, c := ip(100, 128, 0, 2), ip(10, 1, 2, 3), ip(127, 0, 0, 1)
	body := fmt.Sprintf(`{"hosts":["%s","%s"],"%s":"x","note":"via %s then %s","ip":"%s"}`, a, b, c, c, a, b)
	first := norm(t, body)
	if again := norm(t, body); again != first {
		t.Fatal("the same input gave two outputs")
	}
	if twice := norm(t, first); twice != first {
		t.Fatalf("normalising twice changed the output:\n%s\nvs\n%s", twice, first)
	}
	for _, original := range []string{a, b, c} {
		if strings.Contains(first, original) {
			t.Fatalf("an original address survived (private and loopback addresses are rewritten too):\n%s", first)
		}
	}
}

func TestNormaliseAddressesUnderSecretAndIdNamesDoNotUseUpANumber(t *testing.T) {
	// A string that is replaced as a whole (a secret, an id) takes no documentation address with it, so the first address that stays is 192.0.2.1 and a second pass agrees.
	a, b, c := ip(100, 128, 0, 2), ip(100, 128, 0, 3), ip(100, 128, 0, 4)
	body := fmt.Sprintf(`{"token":%q,"request_id":%q,"host":%q}`, a, b, c)
	m := normJSON(t, body)
	if m["token"] != "REDACTED" || m["host"] != "192.0.2.1" {
		t.Fatalf("output = %v", m)
	}
	first := norm(t, body)
	if norm(t, first) != first {
		t.Fatal("a second pass changed the output")
	}
}

func TestNormaliseRefusesMoreDistinctAddressesThanTheDocumentationRangesHave(t *testing.T) {
	build := func(distinct int) string {
		items := make([]string, distinct)
		for i := range items {
			items[i] = fmt.Sprintf("%q", ip(10, i/250, i%250+1, 9))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	if MaxDistinctAddresses != 762 {
		t.Fatalf("MaxDistinctAddresses = %d, want 762", MaxDistinctAddresses)
	}
	out := norm(t, build(MaxDistinctAddresses))
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	// The ranges are used in order, 254 hosts each.
	for i, want := range map[int]string{0: "192.0.2.1", 253: "192.0.2.254", 254: "198.51.100.1", 507: "198.51.100.254", 508: "203.0.113.1", 761: "203.0.113.254"} {
		if got[i] != want {
			t.Fatalf("address %d = %s, want %s", i, got[i], want)
		}
	}
	// One more distinct address is refused, with a typed error and no output.
	over, err := Normalise([]byte(build(MaxDistinctAddresses + 1)))
	if !errors.Is(err, ErrTooManyAddresses) || over != nil {
		t.Fatalf("want ErrTooManyAddresses and no output, got %v %q", err, over)
	}
	// The paired control: the same number of occurrences with one address repeated is only 762 distinct addresses and is accepted.
	repeated := build(MaxDistinctAddresses)
	repeated = repeated[:len(repeated)-1] + "," + fmt.Sprintf("%q", ip(10, 0, 1, 9)) + "]"
	if _, err := Normalise([]byte(repeated)); err != nil {
		t.Fatalf("a repeated address is not a new one: %v", err)
	}
	// Record passes the refusal on and prints nothing.
	var buf bytes.Buffer
	if err := Record(&buf, jobContract(), 200, []byte(build(MaxDistinctAddresses+1))); !errors.Is(err, ErrTooManyAddresses) || buf.Len() != 0 {
		t.Fatalf("Record: %v %q", err, buf.String())
	}
}

var reAnyIPv4 = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+`)

func inDocumentationRange(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	for _, p := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
		if netip.MustParsePrefix(p).Contains(a) {
			return true
		}
	}
	return false
}

func TestRecordOfABodyThatHeldAPublicAddressNowSucceedsAndPrintsOnlyDocumentationAddresses(t *testing.T) {
	a, b, c := ip(100, 128, 0, 2), ip(100, 128, 0, 3), ip(8, 8, 4, 4)
	body := fmt.Sprintf(`{"signals":{"engine_mx_hosts":[%q,%q]},"meta":{"verifier_ip":%q,"banner":"220 mx [%s]:25 ready"}}`, a, b, c, a)
	rc := modclient.RouteContract{Module: modclient.M1, Route: "POST /v1/verify", Fixture: "verify-ok.json", Status: 200}
	var buf bytes.Buffer
	if err := Record(&buf, rc, 200, []byte(body)); err != nil {
		t.Fatalf("a body with addresses must be recorded, not refused: %v", err)
	}
	block := buf.String()
	for _, original := range []string{a, b, c} {
		if strings.Contains(block, original) {
			t.Fatalf("an original address was printed:\n%s", block)
		}
	}
	found := reAnyIPv4.FindAllString(block, -1)
	if len(found) != 4 {
		t.Fatalf("the block must hold the four addresses of the body, found %v", found)
	}
	for _, f := range found {
		if !inDocumentationRange(f) {
			t.Fatalf("%s is not in a documentation range:\n%s", f, block)
		}
	}
	// And what was printed reads back as a recording that the secret check accepts.
	recs, err := ParseRecorded(buf.Bytes())
	if err != nil || len(recs) != 1 {
		t.Fatalf("the block must parse back: %v %v", recs, err)
	}

	// The paired controls: the secret check still refuses the RAW body, with the paths the harness reported, and CheckFixtures still refuses a fixture that holds a public address.
	fs, _ := FindSecrets([]byte(body))
	if !strings.Contains(classes(fs), ClassIPv4+"@signals.engine_mx_hosts[]") || !strings.Contains(classes(fs), ClassIPv4+"@meta.verifier_ip") {
		t.Fatalf("the raw body must be flagged where the harness saw it, got %s", classes(fs))
	}
	rc.Shape = modclient.Object(modclient.Req("signals", modclient.Any), modclient.Req("meta", modclient.Any))
	problems := CheckFixtures([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"verify-ok.json": body}), fxDir)
	secret := 0
	for _, p := range problems {
		if p.Kind == Secret && strings.Contains(p.Detail, ClassIPv4) {
			secret++
		}
	}
	if secret != 3 { // the hosts array, verifier_ip and the banner
		t.Fatalf("CheckFixtures must still refuse a fixture holding a public address, problems: %v", kinds(problems))
	}
	// A normalised body, committed as a fixture, passes.
	if clean := CheckFixtures([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"verify-ok.json": string(recs[0].Body)}), fxDir); len(clean) != 0 {
		t.Fatalf("a recorded fixture must pass the check: %v", kinds(clean))
	}
}

func TestNormaliseAndTheSecretCheckSaySomethingAboutIPv6AndItIsNothing(t *testing.T) {
	// Today neither the normaliser nor the secret check looks at IPv6: an address in that form is left as it is and is not flagged. The check is IPv4-only (the
	// repository's personal-details gate is too); the test pins that so that a change is a decision and not an accident.
	a := ip(100, 128, 0, 2)
	body := fmt.Sprintf(`{"v6":"2001:db8::1","v4":%q}`, a)
	m := normJSON(t, body)
	if m["v6"] != "2001:db8::1" || m["v4"] != "192.0.2.1" {
		t.Fatalf("output = %v", m)
	}
	if fs, _ := FindSecrets([]byte(`{"v6":"2001:db8::1"}`)); len(fs) != 0 {
		t.Fatalf("IPv6 is not looked at, got %s", classes(fs))
	}
}

func TestIPv4SpansReadAddressesAsThePersonalDetailsGateDoes(t *testing.T) {
	a := ip(1, 2, 3, 4)
	cases := []struct {
		text string
		want int
	}{
		{"a " + a + " b", 1}, {"(" + a + ")", 1}, {a + ".", 1}, {a + ".x", 1}, {a + ":25", 1}, {a + " " + a, 2},
		{a + ".5", 0}, {"v" + a, 0}, {"x" + a, 0}, {a + "x", 0}, {"." + a, 0}, {a + "_", 0}, {"1.2.3", 0},
	}
	for _, c := range cases {
		if got := len(ipv4Spans(c.text)); got != c.want {
			t.Fatalf("%q: %d spans, want %d", c.text, got, c.want)
		}
	}
	// The secret check uses the same reading: a version number is not flagged, an address is.
	if fs, _ := FindSecrets([]byte(fmt.Sprintf(`{"v":%q}`, a+".5"))); len(fs) != 0 {
		t.Fatalf("a five-part version number is not an address, got %s", classes(fs))
	}
	if fs, _ := FindSecrets([]byte(fmt.Sprintf(`{"v":%q}`, "("+a+")"))); len(fs) != 1 || fs[0].Class != ClassIPv4 {
		t.Fatalf("an address in parentheses is one, got %s", classes(fs))
	}
}
