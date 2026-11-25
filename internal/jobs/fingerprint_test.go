package jobs

import "testing"

// The fingerprint is what a confirm is held to, so it must be (a) STABLE: the same inputs always give the same text, whatever the map order; (b) SENSITIVE: each thing the preview depends
// on, changed alone, changes it; (c) NOT sensitive to what must not matter (a suppression, the balance, a time). The golden values below were computed with sha256sum on the canonical
// text written out by hand, not by the code under test.

const goldJob = "11111111-1111-4111-8111-111111111111"

func baseInput() fingerprintInput {
	return fingerprintInput{
		JobID: goldJob, Module: "m2", RowsDigest: "r", ContentDigest: "c", UnknownChoice: "exclude",
		Prices: map[string]int{"m2_accepted": 1, "m2_bounced": 1},
	}
}

func TestFingerprint_GoldenValue(t *testing.T) {
	// sha256 of {"content":"c","job":"1111...","module":"m2","prices":{"m2_accepted":1,"m2_bounced":1},"rows":"r","source":"","source_results":"","unknown_choice":"exclude"}
	const want = "148a9234b6cf571a93802a668a464e1fa6228d290d18fcc64c3541f68f001704"
	if got := baseInput().fingerprint(); got != want {
		t.Fatalf("fingerprint = %s, want the independently computed %s: the canonical document changed", got, want)
	}
}

func TestFingerprint_IsStableWhateverTheMapOrder(t *testing.T) {
	first := baseInput().fingerprint()
	for i := 0; i < 200; i++ { // Go randomises map iteration: 200 runs would show an order dependence
		in := baseInput()
		in.Prices = map[string]int{"m2_bounced": 1, "m2_accepted": 1}
		if got := in.fingerprint(); got != first {
			t.Fatalf("run %d: %s, want %s: the fingerprint depends on map order", i, got, first)
		}
	}
}

func TestFingerprint_EachInputChangesIt(t *testing.T) {
	base := baseInput().fingerprint()
	for name, mutate := range map[string]func(*fingerprintInput){
		"the job":            func(in *fingerprintInput) { in.JobID = "22222222-2222-4222-8222-222222222222" },
		"the module":         func(in *fingerprintInput) { in.Module = "m1" },
		"the list":           func(in *fingerprintInput) { in.RowsDigest = "r2" },
		"the content":        func(in *fingerprintInput) { in.ContentDigest = "c2" },
		"the unknown choice": func(in *fingerprintInput) { in.UnknownChoice = "include" },
		"the source job":     func(in *fingerprintInput) { in.SourceJobID = goldJob },
		"the source results": func(in *fingerprintInput) { in.SourceDigest = "s" },
		"a price":            func(in *fingerprintInput) { in.Prices = map[string]int{"m2_accepted": 2, "m2_bounced": 1} },
		"a unit that became reachable": func(in *fingerprintInput) {
			in.Prices = map[string]int{"m2_accepted": 1, "m2_bounced": 1, "m2_extra": 2}
		},
		"a unit that stopped being reachable": func(in *fingerprintInput) { in.Prices = map[string]int{"m2_accepted": 1} },
	} {
		in := baseInput()
		mutate(&in)
		if got := in.fingerprint(); got == base {
			t.Errorf("changing %s left the fingerprint unchanged: a confirm would not notice", name)
		}
	}
	// the control: an equal copy is equal
	if got := baseInput().fingerprint(); got != base {
		t.Errorf("an identical input gave %s, want %s", got, base)
	}
	// a field value cannot move into its neighbour (no concatenation ambiguity: each is a keyed JSON member)
	a, b := baseInput(), baseInput()
	a.RowsDigest, a.ContentDigest = "xy", ""
	b.RowsDigest, b.ContentDigest = "x", "y"
	if a.fingerprint() == b.fingerprint() {
		t.Errorf("rows %q + content %q and rows %q + content %q give one fingerprint", a.RowsDigest, a.ContentDigest, b.RowsDigest, b.ContentDigest)
	}
}

func TestCanonicalJSON_SortsKeysAndEscapesNothingExtra(t *testing.T) {
	got, err := canonicalJSON(map[string]any{"b": int64(2), "a": []any{"x", true, nil, 3}, "c": map[string]any{"z": "<&>", "y": ""}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"a":["x",true,null,3],"b":2,"c":{"y":"","z":"<&>"}}`
	if string(got) != want {
		t.Errorf("canonicalJSON = %s, want %s", got, want)
	}
	if _, err := canonicalJSON(map[string]any{"f": 1.5}); err == nil {
		t.Error("a float was accepted: it would be a second spelling of one number")
	}
	if _, err := canonicalJSON(map[string]any{"s": []string{"a"}}); err == nil {
		t.Error("a []string was accepted: only the documented types are canonicalised")
	}
}

func TestRowsDigest_GoldenValueAndEscaping(t *testing.T) {
	// 1|pending|alice@example.com|\n2|malformed||no_at\n3|excluded|x\|y@example.com|invalid\n (the bar inside an address is escaped), sha256sum
	d := newLineDigest()
	d.rowLine(1, StatePending, "alice@example.com", "")
	d.rowLine(2, StateMalformed, "", "no_at")
	d.rowLine(3, StateExcluded, "x|y@example.com", "invalid")
	const want = "8649b35b99a92bd6acf4973ac619f51b9afc0280b4adfa2ba70ce59b9201aea4"
	if got := d.sum(); got != want {
		t.Errorf("rows digest = %s, want the independently computed %s", got, want)
	}

	// escaping makes two different lists two different digests: an address holding the separator cannot imitate two fields
	a, b := newLineDigest(), newLineDigest()
	a.rowLine(1, StateExcluded, "x|y", "")
	b.rowLine(1, StateExcluded, "x", "y")
	if a.sum() == b.sum() {
		t.Error("norm \"x|y\" with no reason and norm \"x\" with reason \"y\" give one digest: the separator is not escaped")
	}
}

func TestRowsDigest_ReadsSuppressedAsPendingAndNothingElseAsAnotherState(t *testing.T) {
	build := func(state string) string {
		d := newLineDigest()
		d.rowLine(1, state, "a@example.com", "")
		return d.sum()
	}
	if build(StatePending) != build(StateSuppressed) {
		t.Error("a suppressed row and a pending row digest differently: a suppression would make the preview stale")
	}
	for _, other := range []string{StateDuplicate, StateExcluded, StateMalformed} {
		if build(other) == build(StatePending) {
			t.Errorf("a %s row digests like a pending one: the state is not in the fingerprint", other)
		}
	}
	// ordinal and address matter
	d1, d2, d3 := newLineDigest(), newLineDigest(), newLineDigest()
	d1.rowLine(1, StatePending, "a@example.com", "")
	d2.rowLine(2, StatePending, "a@example.com", "")
	d3.rowLine(1, StatePending, "b@example.com", "")
	if d1.sum() == d2.sum() || d1.sum() == d3.sum() {
		t.Error("the ordinal or the address does not change the digest")
	}
	if newLineDigest().sum() != "" {
		t.Error("an empty digest must be \"\" (an empty list is not a list)")
	}
}

func TestContentDigest_GoldenValueAndSensitivity(t *testing.T) {
	c := &Content{Subject: "S", HTML: "<p>a&b</p>", Text: "t"}
	const want = "e203422b8f4b26c40695067b512c64b6d83915153fbb3bd5a63373f17b54e41d" // sha256sum of {"html":"<p>a&b</p>","reply_to":"","subject":"S","text":"t"}
	if got := contentDigest(c); got != want {
		t.Errorf("content digest = %s, want the independently computed %s (HTML must not be escaped)", got, want)
	}
	if contentDigest(nil) != "" {
		t.Error("a job with no content must digest to \"\"")
	}
	seen := map[string]string{contentDigest(c): "base"}
	for name, m := range map[string]Content{
		"subject": {Subject: "S2", HTML: c.HTML, Text: c.Text}, "html": {Subject: "S", HTML: "<p>a</p>", Text: c.Text},
		"text": {Subject: "S", HTML: c.HTML, Text: "t2"}, "reply_to": {Subject: "S", HTML: c.HTML, Text: c.Text, ReplyTo: "r@example.com"},
	} {
		m := m
		d := contentDigest(&m)
		if prev, dup := seen[d]; dup {
			t.Errorf("changing the %s gives the digest of %s", name, prev)
		}
		seen[d] = name
	}
}
