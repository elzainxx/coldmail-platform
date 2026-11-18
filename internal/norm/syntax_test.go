package norm

import (
	"errors"
	"strings"
	"testing"
)

// The syntax half of the address rule (syntax.go). The shared vector file pins module 1's REAL answers (TestNorm_Vectors reads `syntax_ok`); the tests here pin the boundaries of the layer by
// hand, both sides of every limit, and the error contract. Each boundary has its paired control: the address one byte inside the limit is accepted.

func local(n int) string { return strings.Repeat("a", n) }
func domainOf(total int) string {
	// a dotted domain of exactly total bytes: 50-byte labels, then one last label of the rest (at most 53 bytes)
	var labels []string
	left := total
	for left > 53 {
		labels = append(labels, strings.Repeat("b", 50))
		left -= 51 // the label and its dot
	}
	labels = append(labels, strings.Repeat("c", left))
	return strings.Join(labels, ".")
}

func TestSyntax_LocalPartLimitIsSixtyFourBytes(t *testing.T) {
	if _, err := Validate(local(64) + "@example.com"); err != nil {
		t.Errorf("a 64-byte local part was refused: %v", err)
	}
	_, err := Validate(local(65) + "@example.com")
	if r, ok := SyntaxReasonOf(err); !ok || r != ReasonLocalTooLong || !errors.Is(err, ErrSyntax) || errors.Is(err, ErrMalformed) || !Rejected(err) {
		t.Errorf("a 65-byte local part: %v (reason %q, %v), want a local_too_long syntax refusal that is not ErrMalformed", err, r, ok)
	}
	// BYTES, not characters: 33 two-byte letters are 66 bytes; 32 are 64
	if _, err := Validate(strings.Repeat("é", 32) + "@example.com"); err != nil {
		t.Errorf("32 two-byte letters (64 bytes) were refused: %v", err)
	}
	if r, ok := SyntaxReasonOf(func() error { _, e := Validate(strings.Repeat("é", 33) + "@example.com"); return e }()); !ok || r != ReasonLocalTooLong {
		t.Errorf("33 two-byte letters (66 bytes): reason %q %v, want local_too_long", r, ok)
	}
	// the limit is on the NORMALISED local part: upper case folds to the same length, so it does not move the boundary
	if _, err := Validate(strings.ToUpper(local(64)) + "@example.com"); err != nil {
		t.Errorf("a 64-byte upper-case local part was refused: %v", err)
	}
}

func TestSyntax_DomainLimitIsTwoHundredFiftyFiveBytes(t *testing.T) {
	d255, d256 := domainOf(255), domainOf(256)
	if len(d255) != 255 || len(d256) != 256 {
		t.Fatalf("the test's domains are %d and %d bytes, want 255 and 256", len(d255), len(d256))
	}
	if n, err := Validate("a@" + d255); err != nil || n != "a@"+d255 {
		t.Errorf("a 255-byte domain: %q, %v; want accepted unchanged (norm/v1 does not check DNS lengths, the layer allows 255)", n, err)
	}
	_, err := Validate("a@" + d256)
	if r, ok := SyntaxReasonOf(err); !ok || r != ReasonDomainTooLong {
		t.Errorf("a 256-byte domain: %v (reason %q, %v), want domain_too_long", err, r, ok)
	}
	// the length is that of the punycode form: a short Unicode label can be long once encoded
	uni := strings.Repeat("é", 63) + ".com" // 126 bytes of UTF-8, but its punycode form is longer; still under 255
	if _, err := Validate("a@" + uni); err != nil {
		t.Errorf("a 63-letter Unicode label: %v", err)
	}
}

func TestSyntax_ADomainWithoutADotFailsAndALeadingOrDoubleDotDoesNot(t *testing.T) {
	for _, in := range []string{"a@b", "a@localhost", "a@B", "a@xn--bcher-kva"} {
		_, err := Validate(in)
		if r, ok := SyntaxReasonOf(err); !ok || r != ReasonDomainNoDot {
			t.Errorf("%q: %v (reason %q, %v), want domain_no_dot", in, err, r, ok)
		}
	}
	// norm/v1 keeps a leading dot and an empty inner label, and the layer only asks whether a dot is present, so these PASS (module 1's behaviour, pinned by the shared vector file)
	for _, in := range []string{"a@.b", "a@b..c", "a@b.c", "a+tag@b.co"} {
		if _, err := Validate(in); err != nil {
			t.Errorf("%q: %v, want accepted", in, err)
		}
	}
	// a trailing dot is removed by norm/v1 first, so "a@b." is a one-label domain, not a dotted one
	if r, ok := SyntaxReasonOf(func() error { _, e := Validate("a@b."); return e }()); !ok || r != ReasonDomainNoDot {
		t.Errorf("a@b.: reason %q %v, want domain_no_dot (the trailing dot is trimmed before the layer looks)", r, ok)
	}
}

// Validate is Normalize then CheckSyntax: malformed addresses keep their norm/v1 reason (and are not syntax errors), well-formed ones return the normal form.
func TestValidate_ComposesTheTwoHalvesAndKeepsTheirErrorsApart(t *testing.T) {
	n, err := Validate("  Alice@Example.COM ")
	if err != nil || n != "alice@example.com" {
		t.Errorf("Validate = %q, %v; want the norm/v1 form alice@example.com", n, err)
	}
	for in, want := range map[string]Reason{"": ReasonEmpty, "no-at": ReasonNoAt, "a@@b.c": ReasonMultipleAt, "@b.c": ReasonEmptyLocal, "a@": ReasonEmptyDomain, "a@b c.d": ReasonBadIDNA} {
		got, err := Validate(in)
		r, ok := ReasonOf(err)
		if got != "" || !ok || r != want || !errors.Is(err, ErrMalformed) || errors.Is(err, ErrSyntax) || !Rejected(err) {
			t.Errorf("%q: %q, %v; want the norm/v1 reason %q and no syntax error", in, got, err, want)
		}
		if any, ok := ReasonAny(err); !ok || any != want {
			t.Errorf("%q: ReasonAny = %q %v, want %q", in, any, ok, want)
		}
	}
	if Rejected(nil) || Rejected(errors.New("other")) {
		t.Errorf("Rejected is true for nil or a foreign error")
	}
	if r, ok := ReasonAny(nil); ok || r != "" {
		t.Errorf("ReasonAny(nil) = %q %v", r, ok)
	}
	// the error carries no part of the address
	_, err = Validate("someone.private@b")
	if strings.Contains(err.Error(), "someone") || strings.Contains(err.Error(), "private") {
		t.Errorf("the error carries the address: %v", err)
	}
}

func TestCheckSyntax_JudgesTextExactlyAsModuleOnesLayerDoes(t *testing.T) {
	for in, want := range map[string]Reason{
		"":                 ReasonPartEmpty,
		"a":                ReasonPartEmpty, // no '@': the whole text is the local part and the domain is empty
		"@b.c":             ReasonPartEmpty,
		"a@":               ReasonPartEmpty,
		"a@b":              ReasonDomainNoDot,
		"a@b.c":            "",
		"a@b@c.d":          "", // split at the LAST '@' (the orchestrator's split); Normalize never lets this through, the layer would
		"a@b.c@d":          ReasonDomainNoDot,
		local(65) + "@b.c": ReasonLocalTooLong,
	} {
		err := CheckSyntax(in)
		got, ok := SyntaxReasonOf(err)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v, want accepted", in, err)
		case want != "" && (!ok || got != want):
			t.Errorf("%q: %v (reason %q), want %q", in, err, got, want)
		}
	}
	if l, d := Split("a@b@c"); l != "a@b" || d != "c" {
		t.Errorf("Split(a@b@c) = %q, %q; want the split at the last @", l, d)
	}
}

func TestSyntaxReasons_AreClosedDistinctAndDocumented(t *testing.T) {
	seen := map[Reason]bool{}
	for _, r := range SyntaxReasons() {
		if r == "" || seen[r] {
			t.Errorf("syntax reason %q is empty or repeated", r)
		}
		seen[r] = true
	}
	for _, r := range Reasons() {
		if seen[r] {
			t.Errorf("syntax reason %q is also a norm/v1 reason: the two sets must not overlap", r)
		}
	}
	if len(SyntaxReasons()) != 4 {
		t.Errorf("%d syntax reasons, want 4 (the set is additive: raise this number with the new word)", len(SyntaxReasons()))
	}
}
