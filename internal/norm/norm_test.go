package norm

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	xnorm "golang.org/x/text/unicode/norm"
)

// ---- the vectors --------------------------------------------------------------------------------------------------------------------------------------------------------------------

// TestNorm_Vectors: every vector of the shared file through Normalize, one subtest per vector: malformed exactly where module 1's real function said so, and the normalised string
// byte for byte where it did not. It fails on the first rule anyone changes (a missing case fold, a changed trim, an invented length limit, a different IDNA profile).
func TestNorm_Vectors(t *testing.T) {
	vs := loadVectors(t)
	for _, v := range vs {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			if p := checkVector(Normalize, v); p != "" {
				t.Error(p)
			}
		})
	}
	if len(vs) < 80 {
		t.Fatalf("only %d vectors: the instrument matched too little (floor 80)", len(vs))
	}
}

// TestNorm_VectorFileShape: the file is what the two repositories agreed it is. A vector file that is thin, or whose entries do not say what they must, would make every other test
// here a pass over nothing.
func TestNorm_VectorFileShape(t *testing.T) {
	f, raw := readVectorFile(t)
	if f.Format != "coldmail-norm-vectors/1" || f.Scheme != "norm/v1" {
		t.Errorf("format %q scheme %q, want coldmail-norm-vectors/1 and norm/v1", f.Format, f.Scheme)
	}
	for _, need := range []string{"module 1's REAL function", "Hasher.Normalize", "NOT produced by the platform's implementation"} {
		if !strings.Contains(f.GeneratedBy, need) {
			t.Errorf("generated_by does not say %q: the header must say how the expectations were obtained", need)
		}
	}
	if f.Fields == "" {
		t.Error("the fields documentation is empty")
	}
	if !isASCII(raw) {
		t.Error("the file holds a byte outside ASCII: every non-ASCII input is written as a \\u escape or as input_b64, so no editor or tool can change it")
	}
	if bytes.IndexByte(raw, '\r') >= 0 || !strings.HasSuffix(string(raw), "}\n") || strings.HasSuffix(string(raw), "\n\n") {
		t.Error("the file must use LF line ends and end with one newline after the closing brace")
	}

	names := map[string]bool{}
	var malformed, accepted, b64, nul int
	groups := map[string]int{}
	for _, v := range f.Vectors {
		if names[v.Name] {
			t.Errorf("duplicate vector name %q", v.Name)
		}
		names[v.Name] = true
		if !validName(v.Name) {
			t.Errorf("vector name %q is not lower-case kebab-case", v.Name)
		}
		if strings.TrimSpace(v.Why) == "" {
			t.Errorf("%s: no why", v.Name)
		}
		in, err := v.raw()
		if err != nil {
			t.Error(err)
			continue
		}
		if v.InputB64 != nil {
			b64++
			if utf8.ValidString(in) {
				t.Errorf("%s: input_b64 carries valid UTF-8; the file uses it only for bytes JSON cannot carry (use input)", v.Name)
			}
		} else if !utf8.ValidString(in) {
			t.Errorf("%s: input is not valid UTF-8", v.Name)
		}
		if strings.ContainsRune(in, 0) {
			nul++
		}
		if v.Malformed {
			malformed++
			if v.Normalized != nil || v.Group != nil {
				t.Errorf("%s: a malformed vector has no normalized string and no group", v.Name)
			}
			continue
		}
		accepted++
		if v.Normalized == nil || *v.Normalized == "" || !utf8.ValidString(*v.Normalized) {
			t.Errorf("%s: a well-formed vector needs a non-empty, valid UTF-8 normalized string", v.Name)
		}
		if v.group() == "" {
			t.Errorf("%s: a well-formed vector needs a group", v.Name)
		}
		groups[v.group()]++
	}
	big := 0
	for _, n := range groups {
		if n >= 3 {
			big++
		}
	}
	if len(f.Vectors) < 80 || malformed < 30 || accepted < 50 || len(groups) < 40 || big < 5 || b64 < 5 || nul < 3 {
		t.Errorf("the file is too thin: %d vectors (floor 80), %d malformed (30), %d well-formed (50), %d groups (40), %d groups of three or more (5), %d with input_b64 (5), %d with a NUL (3)",
			len(f.Vectors), malformed, accepted, len(groups), big, b64, nul)
	}

	// The boundary of every rule has a vector, by name: the list below is the coverage the owner asked for, and a vector deleted from the file is a red line here.
	required := []string{
		"ascii-plain", "ascii-upper-all", "ascii-upper-local-only", "ascii-upper-domain-only", // case folding, each side by itself
		"trim-leading-space", "trim-tabs", "trim-crlf", "trim-no-break-space", "trim-ideographic-space", "trim-nel-and-line-separator", // trim
		"trim-leaves-zero-width-space-in-local", "trim-leaves-byte-order-mark-in-local", "trim-leaves-nul-in-local", "only-white-space", // what the trim leaves
		"empty-string", "no-at-sign", "at-sign-only", "two-at-signs-adjacent", "two-at-signs-apart", "trailing-at-sign", "leading-at-sign", // the split
		"fullwidth-at-sign", "at-sign-inside-quoted-local", "quoted-local-with-space", "display-name-and-angle-brackets", "angle-brackets-only", // quotes and brackets
		"header-injection-second-address", "header-injection-no-second-at", "space-in-local", "space-in-domain", "tab-in-local", // spaces inside
		"plus-tag", "dotted-local", "undotted-local", "gmail-dotted", "leading-dot-local", "double-dot-local", // plus tags and dots are kept
		"non-ascii-local-composed", "non-ascii-local-decomposed", "angstrom-sign", "kelvin-sign", "j-caron-combining", "dotted-capital-i", "greek-capital-sigma-local", // NFC and case
		"fullwidth-letters-local", "cyrillic-a-look-alike-local", "ligature-fi-local", // no NFKC in the local part
		"nul-in-local", "control-character-in-local", "domain-nul", "domain-control-character", // control characters
		"domain-trailing-dot", "domain-trailing-two-dots", "domain-trailing-ideographic-stop", "domain-dot-only", "domain-dots-only", // trailing dots and their order
		"idn-composed", "idn-decomposed", "idn-punycode", "idn-punycode-upper", "domain-sharp-s", "domain-fullwidth-letters", "domain-soft-hyphen", // IDNA
		"domain-single-label", "domain-empty-label-inside", "domain-empty-label-leading", "domain-label-leading-hyphen", "domain-underscore", // domain shapes
		"domain-hyphens-in-positions-three-and-four", "domain-invalid-punycode-short", "domain-zero-width-joiner", "domain-unassigned-code-point", "domain-unicode-16-letter",
		"local-64-bytes", "local-65-bytes", "label-63-bytes", "label-64-bytes", "domain-253-bytes", "domain-254-bytes", "address-254-bytes", "address-255-bytes", // lengths
		"invalid-utf8-local-ff", "invalid-utf8-local-fe", "invalid-utf8-in-domain", "invalid-utf8-only", "utf16le-encoded-address", // bytes that are not UTF-8
		"not-an-address-first", "not-an-address-second", // a malformed text repeated
	}
	for _, name := range required {
		if !names[name] {
			t.Errorf("the vector %q is missing from the file: it covers a rule or a boundary the coverage list requires", name)
		}
	}
	t.Logf("%d vectors: %d well-formed in %d groups, %d malformed, %d as input_b64, %d with a NUL", len(f.Vectors), accepted, len(groups), malformed, b64, nul)
}

func validName(s string) bool {
	if s == "" || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// TestNorm_Groups: the group claims. Vectors of one group are one address (one normalised string), vectors of different groups never are. This is the property the suppressions
// table, module 1's per-job duplicate detection and a hash all rely on.
func TestNorm_Groups(t *testing.T) {
	vs := loadVectors(t)
	if problems := checkGroups(Normalize, vs); len(problems) != 0 {
		t.Errorf("%d group problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	// the claims also hold against the file's own normalised strings (a group is one string there too), so a file edited by hand into inconsistency is caught without the function
	byGroup := map[string]string{}
	byString := map[string]string{}
	for _, v := range vs {
		if v.Malformed {
			continue
		}
		g, s := v.group(), v.normalized()
		if prev, ok := byGroup[g]; ok && prev != s {
			t.Errorf("the file itself gives group %q two strings: %q and %q", g, prev, s)
		}
		byGroup[g] = s
		if other, ok := byString[s]; ok && other != g {
			t.Errorf("the file itself gives groups %q and %q the same string %q", other, g, s)
		}
		byString[s] = g
	}
	if len(byGroup) < 40 {
		t.Fatalf("%d groups: the instrument matched too little (floor 40)", len(byGroup))
	}
}

// ---- the paired controls: the machinery goes red when the property breaks -------------------------------------------------------------------------------------------------

// variant is a re-implementation of the algorithm with ONE deliberate deviation per field; the zero variant is the algorithm. It exists so that the vector checks can be run on a
// broken copy: a check that stays green on every mutant below is a check that cannot fail.
type variant struct {
	skipTrim, skipLower, skipNFCBefore, skipNFCAfter, nfkc bool
	stripDotsBeforeIDNA, skipTrailingDots                  bool
	acceptManyAt, stripPlusTag, stripLocalDots             bool
	rejectControlInLocal, requireDotInDomain               bool
	rejectInvalidUTF8                                      bool
	maxBytes, maxLocal                                     int
	noIDNA, transitional, laxSTD3, verifyDNSLength         bool
}

func (v variant) profile() *idna.Profile {
	switch {
	case v.transitional:
		return idna.New(idna.MapForLookup(), idna.Transitional(true), idna.StrictDomainName(true))
	case v.laxSTD3:
		return idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false))
	case v.verifyDNSLength:
		return idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(true), idna.VerifyDNSLength(true))
	}
	return idnaProfile
}

var errVariant = errors.New("variant: refused")

func (v variant) fn(raw string) (string, error) {
	s := raw
	if !v.skipTrim {
		s = strings.TrimSpace(s)
	}
	if v.rejectInvalidUTF8 && !utf8.ValidString(s) {
		return "", errVariant
	}
	if v.maxBytes > 0 && len(s) > v.maxBytes {
		return "", errVariant
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 || (!v.acceptManyAt && strings.Count(s, "@") != 1) {
		return "", errVariant
	}
	local, dom := s[:at], s[at+1:]
	if v.maxLocal > 0 && len(local) > v.maxLocal {
		return "", errVariant
	}
	if v.stripDotsBeforeIDNA {
		dom = strings.TrimRight(dom, ".")
	}
	if v.noIDNA {
		dom = strings.ToLower(dom)
	} else {
		var err error
		if dom, err = v.profile().ToASCII(dom); err != nil {
			return "", errVariant
		}
	}
	if !v.skipTrailingDots {
		dom = strings.TrimRight(dom, ".")
	}
	if dom == "" || (v.requireDotInDomain && !strings.Contains(dom, ".")) {
		return "", errVariant
	}
	if v.rejectControlInLocal && strings.IndexFunc(local, unicode.IsControl) >= 0 {
		return "", errVariant
	}
	form := xnorm.NFC
	if v.nfkc {
		form = xnorm.NFKC
	}
	if !v.skipNFCBefore {
		local = form.String(local)
	}
	if !v.skipLower {
		local = strings.ToLower(local)
	}
	if !v.skipNFCAfter {
		local = form.String(local)
	}
	if v.stripPlusTag {
		if i := strings.IndexByte(local, '+'); i >= 0 {
			local = local[:i]
		}
	}
	if v.stripLocalDots {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + dom, nil
}

// TestNorm_VectorCheckersDetectBrokenCopies is the paired control of TestNorm_Vectors and TestNorm_Groups. The unbroken variant passes both checks (so the variant machinery is a faithful
// copy and the checks are not red for no reason); every mutant is a copy of the algorithm with one rule changed or one rule invented, and each must be reported by the vector check
// with the vector that pins that rule, by name. The last cases change the EXPECTATION instead of the function: a one-character difference in a normalised string, a flipped
// malformed flag and a swapped group each make the checker report that vector.
func TestNorm_VectorCheckersDetectBrokenCopies(t *testing.T) {
	vs := loadVectors(t)
	if p := checkVectors(variant{}.fn, vs); len(p) != 0 {
		t.Fatalf("the unbroken variant disagrees with the file (%d problems, first: %s): the variant is not a faithful copy, so the mutants below prove nothing", len(p), p[0])
	}
	if p := checkGroups(variant{}.fn, vs); len(p) != 0 {
		t.Fatalf("the unbroken variant breaks the group claims: %v", p)
	}
	mutants := []struct {
		name string
		v    variant
		want string // a vector the check must report
	}{
		{"no trim", variant{skipTrim: true}, "trim-leading-space"},
		{"no case folding of the local part", variant{skipLower: true}, "ascii-upper-all"},
		{"no NFC before the lower-casing", variant{skipNFCBefore: true}, "capital-i-and-combining-dot"},
		{"no NFC after the lower-casing", variant{skipNFCAfter: true}, "j-caron-combining"},
		{"NFKC instead of NFC", variant{nfkc: true}, "fullwidth-letters-local"},
		{"trailing dots stripped before IDNA", variant{stripDotsBeforeIDNA: true, skipTrailingDots: true}, "domain-trailing-ideographic-stop"},
		{"trailing dots kept", variant{skipTrailingDots: true}, "domain-trailing-dot"},
		{"several @ accepted", variant{acceptManyAt: true}, "two-at-signs-apart"},
		{"+tag folded", variant{stripPlusTag: true}, "plus-tag"},
		{"dots of the local part removed", variant{stripLocalDots: true}, "dotted-local"},
		{"an invented control-character rule", variant{rejectControlInLocal: true}, "control-character-in-local"},
		{"an invented 254-byte limit", variant{maxBytes: 254}, "address-255-bytes"},
		{"an invented 64-byte local part limit", variant{maxLocal: 64}, "local-65-bytes"},
		{"an invented dot-in-the-domain rule", variant{requireDotInDomain: true}, "domain-single-label"},
		{"an invented UTF-8 rule", variant{rejectInvalidUTF8: true}, "invalid-utf8-local-ff"},
		{"no IDNA", variant{noIDNA: true}, "idn-composed"},
		{"transitional IDNA", variant{transitional: true}, "domain-sharp-s"},
		{"lax STD3 rules", variant{laxSTD3: true}, "domain-underscore"},
		{"DNS length verified", variant{verifyDNSLength: true}, "label-64-bytes"},
	}
	for _, m := range mutants {
		m := m
		t.Run(m.name, func(t *testing.T) {
			problems := checkVectors(m.v.fn, vs)
			if len(problems) == 0 {
				t.Fatal("the vector check found nothing wrong with this broken copy: it cannot fail for this rule")
			}
			if !anyHasPrefix(problems, m.want+":") {
				t.Errorf("the check is red but does not name the vector %q that pins this rule; it named: %v", m.want, firstN(problems, 5))
			}
		})
	}

	// the group check on the mutants that merge or split addresses
	for _, m := range []struct {
		name string
		v    variant
		frag string
	}{
		{"dots removed merge two groups", variant{stripLocalDots: true}, `"j-a-n-e" and "jane-plain"`},
		{"+tag folded merges two groups", variant{stripPlusTag: true}, "jane-plus"},
		{"no NFC after the lower-casing splits a group", variant{skipNFCAfter: true}, `group "j-caron"`},
	} {
		problems := checkGroups(m.v.fn, vs)
		if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), m.frag) {
			t.Errorf("%s: the group check reports %v, want a problem mentioning %s", m.name, firstN(problems, 3), m.frag)
		}
	}

	// the expectation changed instead of the function: one character, one flag, one group
	good := variant{}.fn
	i := indexOfVector(vs, "ascii-plain")
	flipped := cloneVectors(vs)
	flipped[i].Normalized = strPtr(flipLastChar(vs[i].normalized()))
	if p := checkVectors(good, flipped); len(p) != 1 || !strings.HasPrefix(p[0], "ascii-plain:") {
		t.Errorf("a one-character difference in a normalised string is reported as %v, want exactly the vector ascii-plain", p)
	}
	j := indexOfVector(vs, "no-at-sign")
	unflagged := cloneVectors(vs)
	unflagged[j].Malformed = false
	unflagged[j].Normalized = strPtr("x@example.com")
	unflagged[j].Group = strPtr("x")
	if p := checkVectors(good, unflagged); len(p) != 1 || !strings.HasPrefix(p[0], "no-at-sign:") {
		t.Errorf("a malformed vector turned well-formed in the file is reported as %v, want exactly no-at-sign", p)
	}
	k := indexOfVector(vs, "ascii-upper-all")
	flagged := cloneVectors(vs)
	flagged[k].Malformed, flagged[k].Normalized, flagged[k].Group = true, nil, nil
	if p := checkVectors(good, flagged); len(p) != 1 || !strings.HasPrefix(p[0], "ascii-upper-all:") {
		t.Errorf("a well-formed vector turned malformed in the file is reported as %v, want exactly ascii-upper-all", p)
	}
	regrouped := cloneVectors(vs)
	regrouped[indexOfVector(vs, "ascii-upper-all")].Group = strPtr("not-jane")
	if p := checkGroups(good, regrouped); len(p) == 0 {
		t.Error("a vector moved to another group is not reported by the group check")
	}
}

func anyHasPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func firstN(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

func strPtr(s string) *string { return &s }

func cloneVectors(vs []vector) []vector { return append([]vector(nil), vs...) }

func indexOfVector(vs []vector, name string) int {
	for i, v := range vs {
		if v.Name == name {
			return i
		}
	}
	panic("no vector named " + name)
}

func flipLastChar(s string) string {
	if s == "" {
		return "x"
	}
	r, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size] + string(r+1)
}

// ---- the reasons --------------------------------------------------------------------------------------------------------------------------------------------------------------------

// reasonOfMalformed is the platform's own classification of every malformed vector, written by hand from the algorithm (module 1 has no reason words; the file does not carry them).
// The reason is told apart by the FIRST step that refuses: nothing left after the trim, then the count of '@', then the first byte, then the last byte, then IDNA, then the dots.
var reasonOfMalformed = map[string]Reason{
	"trim-leaves-nul-after-domain": ReasonBadIDNA, "only-white-space": ReasonEmpty, "empty-string": ReasonEmpty,
	"no-at-sign": ReasonNoAt, "at-sign-only": ReasonEmptyLocal, "two-at-signs-adjacent": ReasonMultipleAt, "two-at-signs-apart": ReasonMultipleAt,
	"three-at-signs": ReasonMultipleAt, "trailing-at-sign": ReasonEmptyDomain, "leading-at-sign": ReasonEmptyLocal,
	"fullwidth-at-sign": ReasonNoAt, "small-commercial-at": ReasonNoAt, "at-sign-inside-quoted-local": ReasonMultipleAt,
	"display-name-and-angle-brackets": ReasonBadIDNA, "angle-brackets-only": ReasonBadIDNA, "trailing-angle-bracket-only": ReasonBadIDNA,
	"two-addresses-with-comma": ReasonMultipleAt, "two-addresses-with-semicolon": ReasonMultipleAt, "header-injection-second-address": ReasonMultipleAt,
	"header-injection-no-second-at": ReasonBadIDNA, "space-after-at": ReasonBadIDNA, "space-in-domain": ReasonBadIDNA, "tab-in-domain": ReasonBadIDNA,
	"not-an-address-first": ReasonNoAt, "not-an-address-second": ReasonNoAt,
	"domain-zero-width-joiner": ReasonBadIDNA, "domain-zero-width-non-joiner": ReasonBadIDNA, "domain-unassigned-code-point": ReasonBadIDNA,
	"domain-unicode-16-letter": ReasonBadIDNA, "domain-private-use": ReasonBadIDNA, "domain-noncharacter": ReasonBadIDNA,
	"domain-leading-combining-mark": ReasonBadIDNA, "domain-control-character": ReasonBadIDNA, "domain-nul": ReasonBadIDNA, "domain-delete-character": ReasonBadIDNA,
	"domain-fullwidth-at-sign": ReasonBadIDNA, "domain-underscore": ReasonBadIDNA, "domain-wildcard": ReasonBadIDNA, "domain-slash": ReasonBadIDNA, "domain-percent": ReasonBadIDNA,
	"domain-leading-hyphen": ReasonBadIDNA, "domain-label-leading-hyphen": ReasonBadIDNA, "domain-label-trailing-hyphen": ReasonBadIDNA,
	"domain-hyphens-in-positions-three-and-four": ReasonBadIDNA, "domain-ipv4-literal": ReasonBadIDNA, "domain-ipv6-literal": ReasonBadIDNA,
	"domain-dot-only": ReasonEmptyDomain, "domain-dots-only": ReasonEmptyDomain, "domain-ideographic-stop-only": ReasonEmptyDomain,
	"domain-invalid-punycode-short": ReasonBadIDNA, "domain-punycode-of-ascii-garbage": ReasonBadIDNA,
	"replacement-character-in-domain": ReasonBadIDNA, "replacement-character-after-domain": ReasonBadIDNA,
	"invalid-utf8-only": ReasonNoAt, "utf16le-encoded-address": ReasonBadIDNA,
}

// TestNorm_MalformedReasons: every malformed vector is refused with the typed error and the reason written above; no well-formed vector carries an error; every reason the algorithm
// can give (but the guard's) is exercised by a vector; the error text never holds the address.
func TestNorm_MalformedReasons(t *testing.T) {
	vs := loadVectors(t)
	seen := map[Reason]int{}
	named := map[string]bool{}
	for _, v := range vs {
		in, err := v.raw()
		if err != nil {
			t.Fatal(err)
		}
		got, gotErr := Normalize(in)
		if !v.Malformed {
			if gotErr != nil {
				t.Errorf("%s: a well-formed vector returned the error %v", v.Name, gotErr)
			}
			if _, ok := ReasonOf(gotErr); ok {
				t.Errorf("%s: ReasonOf names a reason for a nil error", v.Name)
			}
			continue
		}
		named[v.Name] = true
		want, ok := reasonOfMalformed[v.Name]
		if !ok {
			t.Errorf("%s: malformed in the file but no reason is written for it in reasonOfMalformed", v.Name)
			continue
		}
		if got != "" {
			t.Errorf("%s: a refusal returned %q", v.Name, got)
		}
		if !errors.Is(gotErr, ErrMalformed) {
			t.Errorf("%s: errors.Is(err, ErrMalformed) is false for %v", v.Name, gotErr)
		}
		var me *MalformedError
		if !errors.As(gotErr, &me) {
			t.Errorf("%s: the error is %T, want *MalformedError", v.Name, gotErr)
			continue
		}
		if r, ok := ReasonOf(gotErr); !ok || r != want || me.Reason != want {
			t.Errorf("%s: the reason is %q, want %q", v.Name, me.Reason, want)
		}
		seen[me.Reason]++
		if len(in) >= 3 && strings.Contains(gotErr.Error(), in) {
			t.Errorf("%s: the error text holds the address: %q", v.Name, gotErr.Error())
		}
		if gotErr.Error() != "norm: the address is malformed (norm/v1): "+string(want) {
			t.Errorf("%s: the error text is %q, want the fixed text with the reason only", v.Name, gotErr.Error())
		}
	}
	for name := range reasonOfMalformed {
		if !named[name] {
			t.Errorf("reasonOfMalformed names %q, which is not a malformed vector of the file", name)
		}
	}
	for _, r := range Reasons() {
		if r == ReasonInternal {
			continue // only the panic guard gives it: TestNorm_PanicGuard
		}
		if seen[r] == 0 {
			t.Errorf("no malformed vector has the reason %q: the reason is not exercised", r)
		}
	}
	if len(named) < 30 {
		t.Fatalf("%d malformed vectors checked: the instrument matched too little (floor 30)", len(named))
	}
}

// TestNorm_ErrorShape pins the vocabulary and the helpers: the words are frozen (callers store them), ReasonOf works through wrapping and refuses every other error, Reasons returns a
// copy.
func TestNorm_ErrorShape(t *testing.T) {
	want := []string{"empty", "no_at", "multiple_at", "empty_local", "empty_domain", "bad_idna", "internal"}
	var got []string
	for _, r := range Reasons() {
		got = append(got, string(r))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the reasons are %v, want %v: a word is never renamed, repurposed or removed", got, want)
	}
	rs := Reasons()
	rs[0] = "hacked"
	if Reasons()[0] != ReasonEmpty {
		t.Error("mutating the slice Reasons returned changed the package's reasons")
	}
	_, err := Normalize("no at sign")
	wrapped := errors.Join(errors.New("while reading row 7"), err)
	if r, ok := ReasonOf(wrapped); !ok || r != ReasonNoAt {
		t.Errorf("ReasonOf through a wrapped error = (%q, %v), want (no_at, true)", r, ok)
	}
	if !errors.Is(wrapped, ErrMalformed) {
		t.Error("a wrapped refusal is no longer errors.Is ErrMalformed")
	}
	for _, e := range []error{nil, errors.New("other"), ErrMalformed} {
		if r, ok := ReasonOf(e); ok || r != "" {
			t.Errorf("ReasonOf(%v) = (%q, %v), want (\"\", false): only a *MalformedError has a reason", e, r, ok)
		}
	}
	if errors.Is(errors.New("norm: the address is malformed (norm/v1)"), ErrMalformed) {
		t.Error("an error with the same text is the sentinel: ErrMalformed must be matched by identity")
	}
}

// ---- the documented behaviour, written out by hand ---------------------------------------------------------------------------------------------------------------------------------

// TestNorm_Documented: the claims of the package comment as small cases whose expectations are written out here, not read from the file. (The file proves agreement with module 1;
// these say what the agreement means, in words a reviewer can check.) Source stays ASCII: every non-ASCII character is an escape.
func TestNorm_Documented(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"module 1's golden example", "Jane.Doe@Example.COM", "jane.doe@example.com"},
		{"trim, a trailing dot, +tag and dots kept", "  user+tag@Example.com. ", "user+tag@example.com"},
		{"the shortest address", "a@b.co", "a@b.co"},
		{"an IDN domain becomes punycode", "user@ex\u00e4mple.com", "user@xn--exmple-cua.com"},
		{"the punycode form is the same address", "user@xn--exmple-cua.com", "user@xn--exmple-cua.com"},
		{"composed and decomposed local parts meet", "jo\u0308hn@example.com", "j\u00f6hn@example.com"},
		{"the ideographic full stop after the domain is a dot, then removed", "user@example.com\u3002", "user@example.com"},
		{"the dotted capital I lower-cases to i", "\u0130x@ex.com", "ix@ex.com"},
		{"sharp s stays in the local part and in the domain label", "Stra\u00dfe@Stra\u00dfe.de", "stra\u00dfe@xn--strae-oqa.de"},
		{"no provider folding: dots and +tags of a gmail address stay", "J.A.N.E+Promo@Gmail.com", "j.a.n.e+promo@gmail.com"},
		{"a one-label domain is fine, with or without its trailing dot", "A@B.", "a@b"},
		{"an empty label in the domain is kept", "a@b..c", "a@b..c"},
		{"a leading dot in the domain is kept", "a@.b.c", "a@.b.c"},
		{"white space inside the local part is kept", "a b@c.com", "a b@c.com"},
		{"a NUL inside the local part is kept", "a\x00b@c.com", "a\x00b@c.com"},
		{"a quoted local part is just text", "\"A B\"@c.com", "\"a b\"@c.com"},
		{"the fullwidth letters of the local part are NOT folded (no NFKC)", "\uff21@c.com", "\uff41@c.com"},
		{"the fullwidth letters of the domain ARE folded (UTS-46)", "a@\uff43.com", "a@c.com"},
		{"a byte that is not UTF-8 becomes U+FFFD in the local part", "a\xffb@c.com", "a\ufffdb@c.com"},
		{"a long local part and a long label are not refused", strings.Repeat("A", 100) + "@" + strings.Repeat("b", 100) + ".com", strings.Repeat("a", 100) + "@" + strings.Repeat("b", 100) + ".com"},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: Normalize(%q) = (%q, %v), want %q", c.name, c.in, got, err, c.want)
		}
	}
	for _, c := range []struct{ name, in string }{
		{"no @", "jane.example.com"}, {"two @", "a@b@c.com"}, {"nothing before the @", "@c.com"}, {"nothing after the @", "a@"}, {"nothing at all", ""},
		{"an underscore in the domain", "a@b_c.com"}, {"a space in the domain", "a@b c.com"}, {"a domain of dots", "a@..."}, {"a leading hyphen on a label", "a@-b.com"},
	} {
		if got, err := Normalize(c.in); err == nil || got != "" || !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: Normalize(%q) = (%q, %v), want a refusal", c.name, c.in, got, err)
		}
	}
}

// ---- hostile input ------------------------------------------------------------------------------------------------------------------------------------------------------------------

func hostileInputs() []string {
	long := func(s string, n int) string { return strings.Repeat(s, n) }
	return []string{
		"", " ", "@", "@@", "@@@@@@@@", "a@", "@a", "a@@b", ".", "..", "a@.", "a@..", "@.", ".@.", "a@b@", "@a@",
		"\x00", "\x00@\x00", "a@\x00", "\x00@a.com", "a\x00@a.com", "a@a.com\x00", "\x00\x00\x00@\x00\x00\x00",
		"\xff", "\xff@\xff", "a@\xff", "\xff@a.com", "\xc0\xaf", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xf8\x88\x80\x80\x80", "\xe2\x82", "\x80\x80\x80",
		"a@xn--", "a@xn--.com", "a@xn--a", "a@xn--zzzzzzzzzzzzzzzzzzzz.com", "a@XN--EXMPLE-CUA.COM", "a@xn--xn--a.com", "a@xn--a-.com", "a@\u00e4--b.com",
		"a@-", "a@--", "a@-.com", "a@.-", "a@a.-", "a@a--.com",
		"a@\u202egmail.com", "\u202ea@b.com", "a\u202e@b.com", "a@b.com\u202e", "\u2066a@b.com\u2069",
		"a@\u200d", "a@\u200c.com", "a@\u200d.com", "a@a\u200d.com", "a@\u0301", "a@\u0301.com", "a@a\u0301.com", "a@\u0378", "a@\ufdd0", "a@\U0010ffff", "a@\U0001f600", "a@\ud7ff",
		"\U0010ffff@\U0010ffff", "\u0301@b.com", "\u0301\u0301\u0301@b.com", "a\u0301\u0301\u0301\u0301\u0301@b.com", "\u1100\u1161\u11a8@b.com", "\u0f73\u0f71\u0f72@b.com", "\ufb2c@b.com", "\u0958@b.com",
		"a@[::1]", "a@[192.0.2.4]", "a@192.0.2.4", "a@192.0.2.4.", "a@999.999.999.999", "a@0x7f.1", "a@%00.com", "a@a%2eb", "a@a/b", "a@a\\b", "a@a:80", "a@a:b@c", "a@a?b", "a@a#b", "a@a b",
		"<a@b.com>", "\"a@b.com\"", "'a@b.com'", "(a@b.com)", "a@b.com,c@d.com", "a@b.com;c@d.com", "a@b.com c@d.com", "mailto:a@b.com", "a@b.com\r\nBcc: c@d.com", "a@b.com\nX: y",
		"a@b.com\u0085", "\u0085a@b.com", "a@b.com\u2028", "a@b.com\u2029\u2029", "\u180ea@b.com", "a@b.com\u180e",
		"\t", "\r\n\r\n", "\v\f", "\u00a0\u00a0", "\u3000@\u3000", "\u2000@\u2000",
		"A@B", "a@b.", "a@b..", "a@b...", "a@.b", "a@..b", "a@b..c", "a@\u3002", "a@\u3002\u3002", "a@b\u3002", "a@b\u3002\u3002c",
		"\u0130@a.com", "I\u0307@a.com", "\u03a3@a.com", "\u1e9e@a.com", "\u01c5@a.com", "\u1f88@a.com", "\ufb03@a.com", "\u0149@a.com", "\u1e96@a.com",
		"a@\u00df", "a@\u1e9e", "a@\u03c2", "a@\u03a3", "a@\u0130", "a@I\u0307", "a@\ufb03", "a@\u00ad", "a@\u200b", "a@\ufeff", "a@\u2060", "a@\u034f", "a@\ufe0f",
		long("a", 70) + "@" + long("b", 70) + ".c", long("a", 5000) + "@b.com", "a@" + long("b.", 3000) + "c", "a@" + long("b", 5000) + ".com", long("@", 2000), long(".", 5000), long("a@", 2000),
		"a@" + long("\u00e4", 500) + ".com", long("\u00e4", 3000) + "@b.com", "a@" + long("\u4e00", 800) + ".com", long("\u0301", 2000) + "@b.com", "a@" + long("-", 100) + ".com", long("xn--", 300),
		"a@" + long("a\u0301", 400) + ".com", long("\x00", 2000), long("\xff", 2000), long(" ", 5000) + "a@b.com" + long(" ", 5000), long("\u200b", 3000) + "@b.com",
	}
}

// TestNorm_Hostile: Normalize never panics and never misbehaves, whatever it is handed: a table of hostile inputs and a seeded random corpus. For every input the verdict is
// deterministic; a refusal is the typed error with a known reason and the empty string; an acceptance has exactly one @, is valid UTF-8, and (for valid UTF-8 input) is a fixed point:
// normalising the output returns it unchanged, which is what lets a stored address be compared with a fresh one. The documented exception is pinned too: an invalid byte in the
// DOMAIN is accepted and encoded, and the punycode result is then refused when fed back.
func TestNorm_Hostile(t *testing.T) {
	inputs := hostileInputs()
	rng := rand.New(rand.NewSource(20261117))
	frags := []string{"@", "@", ".", "..", "-", "--", "_", " ", "\t", "\n", "\x00", "\xff", "\xc0\xaf", "\xed\xa0\x80", "xn--", "\u3002", "\uff0e", "\uff20", "\u00df", "\u1e9e", "\u0130", "I", "\u0307",
		"\u03a3", "\u03c2", "\u212b", "A\u030a", "\u00e4", "a\u0308", "\ufb01", "\uff21", "\u0430", "\u200b", "\u200d", "\ufeff", "\u00ad", "\u00a0", "\u3000", "\u0085", "\U0001f600", "\ufffd",
		"\ue000", "\u0378", "\u0301", "\u05d3", "example", "com", "EXAMPLE", "jane", "a", "0", "+", "<", ">", "(", ")", "[", "]", "\"", "\\", "/", "%", "*", "ab--cd", "-a", "a-"}
	for i := 0; i < 150000; i++ {
		var sb strings.Builder
		switch rng.Intn(4) {
		case 0:
			for j, l := 0, rng.Intn(24); j < l; j++ {
				sb.WriteByte(byte(rng.Intn(256)))
			}
		case 1:
			for j, l := 0, rng.Intn(16); j < l; j++ {
				sb.WriteRune(rune(rng.Intn(0x30000)))
			}
		default:
			l := 1 + rng.Intn(10)
			at := rng.Intn(l + 1)
			for j := 0; j < l; j++ {
				if j == at && rng.Intn(10) != 0 {
					sb.WriteByte('@')
				}
				sb.WriteString(frags[rng.Intn(len(frags))])
			}
		}
		inputs = append(inputs, sb.String())
	}

	known := map[Reason]bool{}
	for _, r := range Reasons() {
		known[r] = true
	}
	accepted, refused, pinned := 0, 0, 0
	for _, in := range inputs {
		got, err := safely(t, in)
		again, err2 := safely(t, in)
		if got != again || (err == nil) != (err2 == nil) {
			t.Fatalf("Normalize is not deterministic on %q: (%q, %v) then (%q, %v)", in, got, err, again, err2)
		}
		if err != nil {
			refused++
			r, ok := ReasonOf(err)
			if got != "" || !errors.Is(err, ErrMalformed) || !ok || !known[r] || r == ReasonInternal {
				t.Fatalf("a refusal of %q is (%q, %v): want the empty string, ErrMalformed and a known reason that is not internal", in, got, err)
			}
			continue
		}
		accepted++
		if strings.Count(got, "@") != 1 || !utf8.ValidString(got) || got == "" || strings.HasPrefix(got, "@") || strings.HasSuffix(got, "@") {
			t.Fatalf("Normalize(%q) = %q: an accepted address has one @ with something on each side and is valid UTF-8", in, got)
		}
		again2, err3 := Normalize(got)
		switch {
		case utf8.ValidString(in):
			if err3 != nil || again2 != got {
				t.Fatalf("not a fixed point: Normalize(%q) = %q but Normalize(%q) = (%q, %v)", in, got, got, again2, err3)
			}
		case err3 != nil:
			// the documented exception: an invalid byte in the DOMAIN (one in the local part becomes U+FFFD there, and that is a fixed point)
			if utf8.ValidString(in[strings.LastIndexByte(in, '@')+1:]) {
				t.Fatalf("Normalize(%q) = %q is not a fixed point, and the domain of the input is valid UTF-8: only an invalid byte in the domain may do that", in, got)
			}
			pinned++
		}
	}
	if accepted < 5000 || refused < 5000 || len(inputs) < 100000 {
		t.Fatalf("the corpus gave %d accepted and %d refused of %d inputs: the instrument matched too little (floors 5000 / 5000 / 100000)", accepted, refused, len(inputs))
	}
	t.Logf("%d inputs: %d accepted, %d refused, %d accepted outputs that are not fixed points (all with an invalid byte in the domain)", len(inputs), accepted, refused, pinned)

	// the documented exception, pinned, with its paired control
	out, err := Normalize("jane@exa\xffmple.com")
	if err != nil || out != "jane@xn--example-2e14b.com" {
		t.Fatalf("an invalid byte in the domain: (%q, %v), want the accepted punycode form", out, err)
	}
	if _, err := Normalize(out); err == nil {
		t.Error("the punycode form of an invalid byte is accepted when fed back: the documented non-fixed-point is gone (module 1 changed, or the libraries did): update the package comment and the file")
	}
	if again, err := Normalize("jane@exa\u00e4mple.com"); err != nil {
		t.Fatalf("control: %v", err)
	} else if back, err := Normalize(again); err != nil || back != again {
		t.Errorf("control: a valid IDN is not a fixed point: (%q, %v)", back, err)
	}
	if n := len(hostileInputs()); n < 150 {
		t.Errorf("the hostile table has %d entries (floor 150)", n)
	}
}

// safely calls Normalize and turns a panic into a test failure that names the input.
func safely(t *testing.T, in string) (out string, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Normalize panicked on %q: %v", in, r)
		}
	}()
	return Normalize(in)
}

// TestNorm_PanicGuard: a panic inside a library is a refusal with the reason internal, never a crash. The injected step panics with a string, with an error and with a nil-pointer
// dereference. The controls: an injected step that works gives the normal answer, and one that returns an error gives bad_idna (so the guard is not what produces every refusal).
func TestNorm_PanicGuard(t *testing.T) {
	for name, step := range map[string]func(string) (string, error){
		"a string": func(string) (string, error) { panic("boom") },
		"an error": func(string) (string, error) { panic(errors.New("boom")) },
		"a nil pointer": func(s string) (string, error) {
			var p *int
			return s + strconv.Itoa(*p), nil
		},
	} {
		got, err := normalize("a@b.com", step)
		if r, ok := ReasonOf(err); got != "" || !ok || r != ReasonInternal || !errors.Is(err, ErrMalformed) {
			t.Errorf("a step that panics with %s: (%q, %v), want a refusal with the reason internal", name, got, err)
		}
	}
	if got, err := normalize("A@B.com", func(s string) (string, error) { return strings.ToLower(s), nil }); err != nil || got != "a@b.com" {
		t.Errorf("control: a working step gives (%q, %v)", got, err)
	}
	if _, err := normalize("a@b.com", func(string) (string, error) { return "", errors.New("no") }); err == nil {
		t.Error("control: a failing step is accepted")
	} else if r, _ := ReasonOf(err); r != ReasonBadIDNA {
		t.Errorf("control: a failing step gives the reason %q, want bad_idna", r)
	}
}

// ---- purity -------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// allowedImports are the only packages norm.go may import: no I/O, no clock, no randomness, no network, no database and no module or platform code.
var allowedImports = map[string]bool{
	"errors": true, "fmt": true, "strings": true,
	"golang.org/x/net/idna": true, "golang.org/x/text/unicode/norm": true,
}

// importViolations parses Go source and returns every import that is not allowed.
func importViolations(src string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "norm.go", src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if !allowedImports[path] {
			bad = append(bad, path)
		}
	}
	return bad, nil
}

// TestNorm_IsPure: the package imports nothing that could do I/O or reach module code (module 1's norm/v1 is COPIED, never imported: separate Go modules). The control: the same check
// flags a source that imports the file system, the clock, a module's package or the platform's own.
func TestNorm_IsPure(t *testing.T) {
	src, err := os.ReadFile("norm.go")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := importViolations(string(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Errorf("norm.go imports %v: the package must stay pure (errors, fmt, strings and the two Unicode libraries only)", bad)
	}
	f, _ := parser.ParseFile(token.NewFileSet(), "norm.go", src, parser.ImportsOnly)
	if len(f.Imports) != len(allowedImports) {
		t.Errorf("norm.go has %d imports, want exactly the %d allowed ones: the instrument matched the wrong file", len(f.Imports), len(allowedImports))
	}
	for _, c := range []struct{ name, src, want string }{
		{"the file system", "package norm\nimport \"os\"\n", "os"},
		{"the clock", "package norm\nimport (\"strings\"; \"time\")\n", "time"},
		{"module 1's package", "package norm\nimport \"github.com/coldmail-verify/control-plane/internal/hash\"\n", "github.com/coldmail-verify/control-plane/internal/hash"},
		{"the platform's own", "package norm\nimport \"github.com/coldmail-platform/platform/internal/config\"\n", "github.com/coldmail-platform/platform/internal/config"},
		{"the network", "package norm\nimport \"net\"\n", "net"},
	} {
		got, err := importViolations(c.src)
		if err != nil || len(got) != 1 || got[0] != c.want {
			t.Errorf("control %s: the import check says (%v, %v), want [%s]", c.name, got, err, c.want)
		}
	}
	if got, err := importViolations("package norm\nimport (\"errors\"; \"strings\")\n"); err != nil || len(got) != 0 {
		t.Errorf("control: an allowed import is flagged: (%v, %v)", got, err)
	}
}
