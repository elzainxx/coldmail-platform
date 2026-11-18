package contractcheck

import (
	"net/netip"
	"regexp"
	"strings"
)

// Finding is one thing in a body that looks like a credential or a personal detail. It names the path and the class and never the value: the point of the check is that the
// value is not printed anywhere.
type Finding struct {
	Path  string
	Class string
}

func (f Finding) String() string { return f.Class + " at " + display(f.Path) }

// The classes of Finding.
const (
	ClassSecretNamedKey = "secret-named-key" // a string value under a key named key, secret, password, token, authorization, credential that is not a placeholder
	ClassBearer         = "bearer-token"
	ClassJWT            = "jwt"
	ClassPrivateKey     = "private-key-header"
	ClassKnownPrefix    = "known-credential-prefix"
	ClassEmail          = "email-address"
	ClassIPv4           = "ipv4-address"
	ClassLongRun        = "long-opaque-run"
)

const maxFindings = 20

var (
	reSecretName  = regexp.MustCompile(`(?:^|_)(?:secret|password|passwd|passphrase|token|apikey|key|authorization|credential|bearer)s?$`)
	reBearer      = regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{16,})`)
	reJWT         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	rePrivateKey  = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	reKnownPrefix = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{20,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{8,}`)
	reEmail       = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)`)
	reIPv4        = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	reRun         = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
	reUUID        = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reTimestamp   = regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt ][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:[Zz]|[+-][0-9]{2}:[0-9]{2})`)
	rePlaceholder = regexp.MustCompile(`(?i)^(?:redacted|synthetic[a-z0-9_-]*|<[a-z -]+>)$`)
)

// canonName lowers a key name to snake case: `requestID` and `request-id` both become `request_id`.
func canonName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '-':
			b.WriteByte('_')
		case c >= 'A' && c <= 'Z':
			if i > 0 && (name[i-1] >= 'a' && name[i-1] <= 'z' || name[i-1] >= '0' && name[i-1] <= '9') {
				b.WriteByte('_')
			}
			b.WriteByte(c + ('a' - 'A'))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func secretName(name string) bool { return reSecretName.MatchString(canonName(name)) }

// uniform reports whether s is one character repeated (a trailing run of '=' padding aside): the form of every placeholder this package writes.
func uniform(s string) bool {
	core := strings.TrimRight(s, "=")
	if core == "" {
		return false
	}
	for i := 1; i < len(core); i++ {
		if core[i] != core[0] {
			return false
		}
	}
	return true
}

// isPlaceholder reports whether a value is a stand-in and not a real value: one repeated character, REDACTED, `synthetic...` or `<word>`.
func isPlaceholder(s string) bool { return uniform(s) || rePlaceholder.MatchString(s) }

// emailAllowed: the documentation domains (RFC 2606 and 6761) are fine in a public repository, nothing else is.
func emailAllowed(domain string) bool {
	d := strings.ToLower(domain)
	for _, base := range []string{"example.com", "example.org", "example.net"} {
		if d == base || strings.HasSuffix(d, "."+base) {
			return true
		}
	}
	for _, tld := range []string{".test", ".example", ".invalid", ".localhost"} {
		if strings.HasSuffix(d, tld) {
			return true
		}
	}
	return false
}

var allowedV4 = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", // documentation
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // private
		"127.0.0.0/8", "169.254.0.0/16", "0.0.0.0/8", "255.255.255.255/32", // loopback, link-local, unspecified, broadcast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// ipv4Spans finds the IPv4 addresses written in s, as the repository's personal-details gate reads them: four dotted numbers that are not glued to a letter, a digit, an
// underscore or a dot on the left, nor to a letter, a digit or an underscore on the right, nor followed by a further dot and digit ("1.2.3.4.5" and "v1.2.3.4" are version
// numbers). A span is a candidate: whether it is an address (octets up to 255, no leading zero) is for netip.ParseAddr.
func ipv4Spans(s string) [][2]int {
	var out [][2]int
	word := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
	}
	for _, loc := range reIPv4.FindAllStringIndex(s, -1) {
		a, b := loc[0], loc[1]
		if a > 0 && (word(s[a-1]) || s[a-1] == '.') {
			continue
		}
		if b < len(s) && (word(s[b]) || s[b] == '.' && b+1 < len(s) && s[b+1] >= '0' && s[b+1] <= '9') {
			continue
		}
		out = append(out, [2]int{a, b})
	}
	return out
}

// ipv4Allowed: documentation, private and special-use addresses may be written in a public repository, any other address may not. Text that does not parse as an address
// (a version number, 999.1.1.1) is not an address.
func ipv4Allowed(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return true
	}
	for _, p := range allowedV4 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// FindSecrets scans a body (every string value and every key) for what must not be committed to a public repository: a bearer token, a JWT, a private-key header, a known
// credential prefix, a string under a key named key / secret / password / token / authorization / credential that is not a placeholder, an e-mail address outside the
// documentation domains, an IPv4 address outside the documentation, private and special-use ranges, and a long run (32 or more characters) of base64-like text that mixes
// digits and letters (a hex or base64 credential). The findings carry a path and a class, never the text. It is a heuristic: it names what it looks for and not finding
// something is not proof that there is nothing (the repository's gitleaks run is the other wall).
func FindSecrets(body []byte) ([]Finding, error) {
	v, err := parse(body)
	if err != nil {
		return nil, err
	}
	var out []Finding
	seen := map[Finding]bool{}
	add := func(path, class string) {
		f := Finding{Path: path, Class: class}
		if !seen[f] && len(out) < maxFindings {
			seen[f] = true
			out = append(out, f)
		}
	}
	scan("", "", v, add)
	return out, nil
}

func scan(path, name string, v value, add func(path, class string)) {
	switch v.kind {
	case vString:
		scanText(path, name, v.str, add)
	case vObject:
		for _, m := range v.members {
			kp := join(path, safeKey(m.key))
			scanText(kp+"(key)", "", m.key, add)
			scan(kp, m.key, m.val, add)
		}
	case vArray:
		for _, it := range v.items {
			scan(path+"[]", name, it, add)
		}
	}
}

func scanText(path, name, s string, add func(path, class string)) {
	if s == "" {
		return
	}
	if name != "" && secretName(name) && !isPlaceholder(s) {
		add(path, ClassSecretNamedKey)
	}
	for _, m := range reBearer.FindAllStringSubmatch(s, -1) {
		// A word after "bearer" (all lower-case letters) is prose ("bearer authentication"), not a token.
		if strings.Trim(m[1], "abcdefghijklmnopqrstuvwxyz") != "" {
			add(path, ClassBearer)
		}
	}
	if reJWT.MatchString(s) {
		add(path, ClassJWT)
	}
	if rePrivateKey.MatchString(s) {
		add(path, ClassPrivateKey)
	}
	if reKnownPrefix.MatchString(s) {
		add(path, ClassKnownPrefix)
	}
	for _, m := range reEmail.FindAllStringSubmatch(s, -1) {
		if !emailAllowed(m[1]) {
			add(path, ClassEmail)
		}
	}
	for _, sp := range ipv4Spans(s) {
		if !ipv4Allowed(s[sp[0]:sp[1]]) {
			add(path, ClassIPv4)
		}
	}
	rest := reTimestamp.ReplaceAllString(reUUID.ReplaceAllString(s, " "), " ")
	for _, run := range reRun.FindAllString(rest, -1) {
		if looksLikeCredential(run) {
			add(path, ClassLongRun)
		}
	}
}

// looksLikeCredential: a long run with a digit in it and at least six different characters (a hex or base64 secret; not a repeated-character placeholder, not a long word).
func looksLikeCredential(run string) bool {
	if isPlaceholder(run) {
		return false
	}
	distinct := map[rune]bool{}
	digit := false
	for _, r := range run {
		distinct[r] = true
		if r >= '0' && r <= '9' {
			digit = true
		}
	}
	return digit && len(distinct) >= 6
}
