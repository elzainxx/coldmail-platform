// Package norm is the platform's copy of module 1's FROZEN address normalisation, `norm/v1`: the one rule that says whether an address is well formed and which
// string is "the same address" for a suppression, a duplicate check and a hash.
//
// WHY IT EXISTS. An address that module 1 cannot normalise is "malformed": module 1 answers it with an invalid/invalid_syntax row of its own and the old charge table
// charged that row (M1-10). The owner's ruling (Phase 3, part A2) is that a malformed address is rejected BEFORE any module is called, counted, skipped, never sent and
// never charged (charge-table row P-08). To do that the platform needs the same judgement module 1 makes, so this package IMPLEMENTS module 1's function, byte for byte,
// instead of inventing a nicer one. Module 1's rule is frozen (a change re-partitions every hash it ever stored), so a copy that agrees today agrees for ever, as long
// as the two stay built from the same library versions (see "What can still make the two differ").
//
// THE ALGORITHM (module 1: control-plane/internal/hash/hash.go, Hasher.Normalize; scheme tag norm/v1+hmac-sha256/v1; every step below is that function's, in order):
//
//  1. TRIM. strings.TrimSpace(raw): removes every leading and trailing Unicode White_Space code point (the ASCII blanks, U+0085, U+00A0, U+1680, U+2000..U+200A,
//     U+2028, U+2029, U+202F, U+205F, U+3000). NOT trimmed: NUL and every control character that is not white space, U+200B (zero-width space), U+FEFF (byte-order mark).
//     Bytes that are not UTF-8 are not white space and are left in place.
//  2. SPLIT. The trimmed text must hold exactly one '@' (byte 0x40: the fullwidth U+FF20 is not one), with at least one byte before it and at least one after it. The
//     split is at that '@': local = everything before, domain = everything after. Nothing else about the local part is judged here: no length limit, no character
//     class, no quote or angle-bracket syntax (a quoted local part is just text, a display name makes the domain illegal, see below).
//     Otherwise the address is MALFORMED, with the reason empty (nothing is left after the trim), no_at, multiple_at, empty_local or empty_domain.
//  3. DOMAIN. IDNA/UTS-46 ToASCII with the profile idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(true)): mapping and case folding
//     (UTS-46 maps the fullwidth and ideographic full stops U+FF0E, U+3002, U+FF61 to '.', and folds compatibility forms such as fullwidth letters), the label
//     validity rules (hyphens, joiners, NFC), the STD3 ASCII rules (only letters, digits and hyphens: an underscore, a space, '<', '>', '[' or a control character is
//     refused), every non-ASCII label as punycode (xn--...), non-transitional (so U+00DF stays, it is not "ss"). DNS length limits are NOT verified (idna.VerifyDNSLength
//     is off), empty labels are not an error, a label of 64 bytes or a domain of 300 passes. If ToASCII fails the address is MALFORMED (reason bad_idna).
//     THEN strings.TrimRight(domain, ".") removes every trailing '.', and the order matters (the Unicode full stops only become '.' inside ToASCII). A domain that is
//     empty after that ("a@.", "a@...") is MALFORMED (reason empty_domain). A leading dot or a dot inside ("a@.b", "a@b..c") is kept as it is. A domain of one label
//     ("a@b", "a@localhost") is fine.
//  4. LOCAL. NFC, then strings.ToLower, then NFC again (the second NFC is load-bearing: lower-casing can leave a sequence that is not in NFC). Nothing else: dots and
//     "+tag" are KEPT, there is no NFKC (a fullwidth or Cyrillic look-alike stays a different address), Go's per-code-point lower-casing is used (the dotted capital I
//     U+0130 becomes a plain "i"; capital sigma always becomes the medial sigma), and bytes that are not UTF-8 become U+FFFD under ToLower (so two different invalid
//     local parts can normalise to one address).
//  5. RECOMPOSE. local + "@" + domain.
//
// So an address is malformed for exactly these reasons and no others. There is NO "too long" reason and NO "control character" reason: module 1 has none (its syntax
// layer, which refuses a local part over 64 bytes, a domain over 255 bytes or a domain with no dot, runs AFTER normalisation and answers invalid/invalid_syntax for a
// well-formed address; that answer is a module answer and is charged, row M1-10). Inventing either rule here would make this package refuse what module 1 accepts, and
// the shared vector file (testdata/norm-vectors.json, also run through module 1's own function in CI) would go red.
//
// HOW IT IS KEPT IN STEP. internal/norm/testdata/norm-vectors.json holds addresses with module 1's verdict (malformed or not, and the normalised string), obtained by
// running module 1's real function. TestNorm_Vectors makes this package reproduce every verdict byte for byte, and the harness (tests/harness/norm_agreement_test.go)
// re-runs the same file through module 1 as a live bulk job, through platform.suppressions, and against module 1's own copy of the file. Any difference is red.
//
// WHERE THE SUPPRESSIONS TABLE DIFFERS. platform.suppressions stores this package's output and its CHECK (platform_0003) refuses what SQL can see; it is a backstop, not a second
// copy of the rule. It is NARROWER than norm/v1: it refuses a well-formed address whose local part holds white space or a NUL, whose domain has an empty label or a leading dot
// ("a@b..c", "a@.b"), or that is longer than 254 bytes, all of which module 1 accepts. It is WIDER on a domain label that IDNA refuses but SQL cannot see (invalid punycode
// such as xn--a, "--" in the third and fourth position). The harness pins every such vector by name (tests/harness/norm_agreement_test.go, normDBDivergence) and fails when the
// table's behaviour moves; this package does not copy the narrower rules, because module 1 does not have them, and a caller that stores an address must be ready for the
// table to refuse it (SQLSTATE 23514, constraint suppressions_email_norm_v1).
//
// WHAT CAN STILL MAKE THE TWO DIFFER. The IDNA and NFC tables of golang.org/x/net and golang.org/x/text are chosen by the Go release that compiles them (x/net/idna
// tables15.0.0.go for Go before 1.27, tables17.0.0.go from Go 1.27), and the libraries' own versions move the Unicode data. go.mod pins both libraries to module 1's
// versions and the harness compares the two go.mod files; a toolchain that moves one side only is not detected until the vector that pins a code point of a newer Unicode
// version (the vector named "domain-unicode-16-letter") changes its answer.
//
// COST. Linear in the input for everything except a domain with many non-ASCII code points: IDNA's punycode step is quadratic in them (about 0.2 s at 5,000 code points, 3 s at
// 20,000, 9 s at 60,000; module 1's function costs the same). A caller that reads user files bounds the length of a row before it calls Normalize; the length is not a norm/v1 rule
// and this package does not invent one.
//
// NOT IDEMPOTENT FOR ONE CLASS OF INPUT. For every input that is valid UTF-8, Normalize(Normalize(x)) == Normalize(x). An invalid byte in the DOMAIN is accepted (IDNA reads it as
// U+FFFD and encodes the label) but the punycode result is refused when fed back; an invalid byte in the local part becomes U+FFFD there and is a fixed point (so two different
// invalid local parts are one address). TestNorm_Hostile pins the first, the vectors of the group "replacement" the second.
//
// The package is pure: no I/O, no clock, no global state that changes after initialisation. Normalize never panics (a recover guard turns a panic inside a library into a
// refusal with the reason internal) and never puts the address in an error.
package norm

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/idna"
	xnorm "golang.org/x/text/unicode/norm"
)

// ErrMalformed is the root of every refusal: errors.Is(err, ErrMalformed) is true for the error Normalize returns when an address is malformed, and for nothing else.
var ErrMalformed = errors.New("norm: the address is malformed (norm/v1)")

// Reason is a stable machine word for why an address is malformed. The set is closed and additive: a word is never renamed, repurposed or removed.
type Reason string

const (
	// ReasonEmpty: nothing is left after the trim (the empty string, or white space only).
	ReasonEmpty Reason = "empty"
	// ReasonNoAt: the trimmed text holds no '@'.
	ReasonNoAt Reason = "no_at"
	// ReasonMultipleAt: the trimmed text holds more than one '@' (a quoted local part with an '@' inside is two).
	ReasonMultipleAt Reason = "multiple_at"
	// ReasonEmptyLocal: the '@' is the first byte of the trimmed text.
	ReasonEmptyLocal Reason = "empty_local"
	// ReasonEmptyDomain: nothing follows the '@', or the domain is nothing but dots (once the trailing ones are removed).
	ReasonEmptyDomain Reason = "empty_domain"
	// ReasonBadIDNA: IDNA ToASCII refused the domain (a character STD3 or UTS-46 forbids, a bad hyphen, a bad joiner, an invalid punycode label, bytes that are not UTF-8...).
	ReasonBadIDNA Reason = "bad_idna"
	// ReasonInternal: a library panicked on this input. Never observed; the guard exists because Normalize promises not to panic.
	ReasonInternal Reason = "internal"
)

// Reasons returns every reason, in the order the algorithm meets them.
func Reasons() []Reason {
	return []Reason{ReasonEmpty, ReasonNoAt, ReasonMultipleAt, ReasonEmptyLocal, ReasonEmptyDomain, ReasonBadIDNA, ReasonInternal}
}

// MalformedError is the error Normalize returns for an address that module 1 would refuse. Its text carries the reason and nothing of the address.
type MalformedError struct {
	Reason Reason
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("norm: the address is malformed (norm/v1): %s", e.Reason)
}

// Unwrap makes errors.Is(err, ErrMalformed) true.
func (e *MalformedError) Unwrap() error { return ErrMalformed }

// ReasonOf returns the reason of a malformed-address error ("", false for any other error or nil).
func ReasonOf(err error) (Reason, bool) {
	var me *MalformedError
	if errors.As(err, &me) && me != nil {
		return me.Reason, true
	}
	return "", false
}

// idnaProfile is module 1's profile, option for option: MapForLookup (UTS-46 mapping, case folding, label validation, STD3 rules) + Transitional(false) + StrictDomainName(true).
// A Profile holds no state that changes after New, so one value serves every goroutine.
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(true),
)

// Normalize returns the norm/v1 form of raw, the same string module 1 hashes: the address that "is" this one for a suppression, a duplicate check and a hash. For an
// address module 1 would refuse it returns "" and a *MalformedError (errors.Is(err, ErrMalformed)); the error never contains the address.
func Normalize(raw string) (normalized string, err error) {
	return normalize(raw, idnaProfile.ToASCII)
}

// normalize is Normalize with the IDNA step injected, so the panic guard can be exercised without a library that panics.
func normalize(raw string, toASCII func(string) (string, error)) (normalized string, err error) {
	defer func() {
		if r := recover(); r != nil {
			normalized, err = "", &MalformedError{Reason: ReasonInternal}
		}
	}()

	// 1. TRIM
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", &MalformedError{Reason: ReasonEmpty}
	}

	// 2. SPLIT: exactly one '@', something before it, something after it. Module 1 tests `at <= 0 || at == len(s)-1 || Count != 1`; the reasons below are the same refusal
	// told apart (no '@' and several '@' are what Count != 1 says, the first byte is at <= 0, the last byte is at == len(s)-1).
	switch n := strings.Count(s, "@"); {
	case n == 0:
		return "", &MalformedError{Reason: ReasonNoAt}
	case n > 1:
		return "", &MalformedError{Reason: ReasonMultipleAt}
	}
	at := strings.IndexByte(s, '@')
	if at == 0 {
		return "", &MalformedError{Reason: ReasonEmptyLocal}
	}
	if at == len(s)-1 {
		return "", &MalformedError{Reason: ReasonEmptyDomain}
	}
	local, domain := s[:at], s[at+1:]

	// 3. DOMAIN: ToASCII first, then the trailing dots (the Unicode full stops become '.' only inside ToASCII).
	domain, err = toASCII(domain)
	if err != nil {
		return "", &MalformedError{Reason: ReasonBadIDNA}
	}
	domain = strings.TrimRight(domain, ".")
	if domain == "" {
		return "", &MalformedError{Reason: ReasonEmptyDomain}
	}

	// 4. LOCAL: NFC, lower-case, NFC again.
	local = xnorm.NFC.String(strings.ToLower(xnorm.NFC.String(local)))

	// 5. RECOMPOSE
	return local + "@" + domain, nil
}
