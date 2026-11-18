package norm

// MODULE 1'S SYNTAX LAYER, the second half of the platform's one address rule (Phase 3 close-out A5, owner ruling OD-9).
//
// `norm/v1` (norm.go) says whether an address can be normalised at all. Module 1 then runs a SYNTAX LAYER over the normalised parts before anything else (control-plane/internal/layers/
// cheap_layers.go, SyntaxLayer.Run: Result Fail, signal syntax_valid=false, which its policy turns into invalid / invalid_syntax and a stop). An address that normalises and fails that layer
// was, until this ruling, sent to module 1 and CHARGED (charge-table row M1-10). The ruling is that the platform's pre-validation is norm/v1 PLUS that layer, exactly as module 1
// implements it, so such an address never reaches module 1 from the platform, is counted and skipped with the malformed ones, and is free (row P-08).
//
// THE LAYER, in module 1's own words, over the normalised parts (local = everything before the last '@' of the normalised address, domain = everything after it, exactly the split of
// the orchestrator: localPart := normalized[:indexAt(normalized)], domain from Hasher.Normalize):
//
//	ok := local != "" && domain != "" && len(local) <= 64 && len(domain) <= 255 && strings.Contains(domain, ".")
//
// The lengths are BYTES (Go's len), after normalisation: a 33-letter two-byte local part is 66 bytes and fails; a domain's length is that of its punycode form. An address that passed
// norm/v1 never has an empty local part or an empty domain (those are malformed reasons), so for a normalised address only three conditions can fail: the local part is longer than
// 64 bytes, the domain is longer than 255 bytes, the domain holds no dot ("a@b", "a@localhost": norm/v1 accepts a one-label domain, the layer does not). They are the first three syntax
// reasons below (the fourth, an empty part, is what the layer would say of text that is not normalised). Module 1 reports no reason of its own (one failing result), so the words are the platform's, additive and closed like the malformed ones.
//
// HOW IT IS KEPT IN STEP. testdata/norm-vectors.json carries, for every address that normalises, `syntax_ok`: what module 1's REAL SyntaxLayer answered for it (the file's expectations
// are produced by module 1's code, never by this package). TestNorm_Vectors makes Validate reproduce every verdict; module 1's own test runs the same file through its layer; the harness
// drives the same addresses through module 1's real bulk API and expects invalid_syntax for exactly the vectors whose syntax_ok is false.

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSyntax is the root of every syntax refusal: errors.Is(err, ErrSyntax) is true for the error Validate and CheckSyntax return for an address that normalises but fails module 1's syntax
// layer, and for nothing else. It is not ErrMalformed (the address could be normalised), but Rejected(err) is true for both: a caller that only needs "never send this" asks that.
var ErrSyntax = errors.New("norm: the address fails module 1's syntax layer")

// The limits of the layer, module 1's numbers (RFC 5321 section 4.5.3.1.1 and .2: 64 octets of local part, 255 of domain).
const (
	MaxLocalBytes  = 64
	MaxDomainBytes = 255
)

// The syntax reasons. The set is closed and additive: a word is never renamed, repurposed or removed.
const (
	// ReasonPartEmpty: the local part or the domain is empty. Unreachable for an address Normalize returned (empty_local and empty_domain are malformed reasons); it exists so that CheckSyntax, which
	// judges whatever text it is given exactly as module 1's layer does, never passes an empty part.
	ReasonPartEmpty Reason = "part_empty"
	// ReasonLocalTooLong: the normalised local part is longer than MaxLocalBytes bytes.
	ReasonLocalTooLong Reason = "local_too_long"
	// ReasonDomainTooLong: the normalised (punycode) domain is longer than MaxDomainBytes bytes.
	ReasonDomainTooLong Reason = "domain_too_long"
	// ReasonDomainNoDot: the normalised domain holds no '.', so it is a single label ("a@b", "a@localhost").
	ReasonDomainNoDot Reason = "domain_no_dot"
)

// SyntaxReasons returns the syntax reasons, in the order CheckSyntax tests them.
func SyntaxReasons() []Reason {
	return []Reason{ReasonPartEmpty, ReasonLocalTooLong, ReasonDomainTooLong, ReasonDomainNoDot}
}

// SyntaxError is the error CheckSyntax and Validate return for an address module 1's syntax layer would fail. Its text carries the reason and nothing of the address.
type SyntaxError struct {
	Reason Reason
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("norm: the address fails module 1's syntax layer: %s", e.Reason)
}

// Unwrap makes errors.Is(err, ErrSyntax) true.
func (e *SyntaxError) Unwrap() error { return ErrSyntax }

// SyntaxReasonOf returns the reason of a syntax-refusal error ("", false for any other error or nil).
func SyntaxReasonOf(err error) (Reason, bool) {
	var se *SyntaxError
	if errors.As(err, &se) && se != nil {
		return se.Reason, true
	}
	return "", false
}

// Rejected reports whether err is a refusal of the platform's address rule, either half: the address is malformed (norm/v1) or it fails module 1's syntax layer. It is false for nil and for
// any other error. A rejected address is never sent to a module and never charged.
func Rejected(err error) bool {
	return errors.Is(err, ErrMalformed) || errors.Is(err, ErrSyntax)
}

// ReasonAny returns the reason of a rejection of either half ("", false for nil or any other error).
func ReasonAny(err error) (Reason, bool) {
	if r, ok := ReasonOf(err); ok {
		return r, true
	}
	return SyntaxReasonOf(err)
}

// Split cuts a NORMALISED address at its last '@', as module 1's orchestrator does: local is everything before it, domain everything after. For an address that is not at all normalised (no
// '@') it returns the whole text as the local part and an empty domain, which the layer then fails; it does not validate.
func Split(normalized string) (local, domain string) {
	at := strings.LastIndex(normalized, "@")
	if at < 0 {
		return normalized, ""
	}
	return normalized[:at], normalized[at+1:]
}

// CheckSyntax is module 1's SyntaxLayer over a NORMALISED address (the output of Normalize): nil when the layer passes, a *SyntaxError naming the first condition that fails otherwise (the
// conditions are tested in this order: an empty part, local length, domain length, dot; module 1 answers one failing result for any of them). It does not normalise: an address that is not in norm/v1
// form is judged as written.
func CheckSyntax(normalized string) error {
	local, domain := Split(normalized)
	switch {
	case local == "" || domain == "":
		return &SyntaxError{Reason: ReasonPartEmpty}
	case len(local) > MaxLocalBytes:
		return &SyntaxError{Reason: ReasonLocalTooLong}
	case len(domain) > MaxDomainBytes:
		return &SyntaxError{Reason: ReasonDomainTooLong}
	case !strings.Contains(domain, "."):
		return &SyntaxError{Reason: ReasonDomainNoDot}
	}
	return nil
}

// Validate is the platform's whole address rule: Normalize (norm/v1), then CheckSyntax (module 1's syntax layer). It returns the normalised address and nil when module 1 would not
// reject the address before any check; otherwise "" and a *MalformedError (the address cannot be normalised) or a *SyntaxError (it can, and module 1's syntax layer fails it); Rejected(err)
// is true for both. The one function every caller that decides "may this address be sent, stored as a suppression, counted as a recipient" uses: the module 1 adapter's Prepare, the
// suppressions writer, the duplicate check. The error never contains the address.
func Validate(raw string) (normalized string, err error) {
	normalized, err = Normalize(raw)
	if err != nil {
		return "", err
	}
	if err := CheckSyntax(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}
