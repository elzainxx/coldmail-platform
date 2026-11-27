package config

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// THE WEB APP'S DIALS (Phase 6, chunk A2; consumer: internal/webapp, built by internal/app). PLATFORM_WEB_DIR names the directory of the built web app (web/dist); with it the one service serves
// the app and the API from one origin. PLATFORM_PUBLIC_SUPABASE_URL and PLATFORM_PUBLIC_SUPABASE_ANON_KEY are what the browser needs to sign in against Supabase's Auth directly; they are handed to it by
// GET /config.json, so they are PUBLIC by design (the anon key is a publishable key: it is shown to every visitor, it is not a secret and is not redacted) and they are required exactly when the app
// is served. A service with no PLATFORM_WEB_DIR serves nothing of this and behaves as it always did.
const (
	VarWebDir                = "PLATFORM_WEB_DIR"
	VarPublicSupabaseURL     = "PLATFORM_PUBLIC_SUPABASE_URL"
	VarPublicSupabaseAnonKey = "PLATFORM_PUBLIC_SUPABASE_ANON_KEY"
)

// The bounds of the web dials: a path of at most 4096 bytes, an address of at most 500 characters (it ends up in a Content-Security-Policy header and in config.json), and a key of at most 2048
// printable characters (a Supabase anon key is a JWT of a few hundred; a publishable key is shorter).
const (
	maxWebDirBytes       = 4096
	maxPublicSupabaseURL = 500
	maxAnonKeyBytes      = 2048
)

const (
	webDirDoc = "Directory of the built web app (the `web/dist` of the repository; the platform image holds it at `/srv/web`), for example `/srv/web`. With it the service also serves the app from its own origin: every GET or HEAD of a file under it, `index.html` for every other path that is not the API and has no file extension (the app's own routes), and `GET /config.json`; the paths `/v1/`, `/u/`, `/healthz` and `/readyz` keep their handlers. Every answer carries a strict Content-Security-Policy, `Cache-Control: no-store` on `index.html`, `/config.json` and every fallback and a year of immutable caching on `/assets/`. 1 to 4096 bytes of text with no control character. The directory and its `index.html` must exist when the service starts, or it refuses to start (exit status 2). Requires PLATFORM_PUBLIC_SUPABASE_URL and PLATFORM_PUBLIC_SUPABASE_ANON_KEY. The default (unset) means the app is not served and nothing else changes. Consumed by `internal/app` (`webapp.New`)."
	webURLDoc = "The browser-facing address of the Supabase project the web app signs in against, for example `https://<project-ref>.supabase.co`: `http` or `https`, a host (a name, an IPv4 address or a bracketed IPv6 address) and an optional port, an optional path of letters, digits and `._~-/`, no credentials, query or fragment, at most 500 characters; trailing slashes are dropped. It is given to the browser by `GET /config.json` (as `supabase_url`) and its origin is the one origin besides the platform's own that the Content-Security-Policy lets the page connect to. **Required when PLATFORM_WEB_DIR is set** (the service refuses to start without it). The default (unset) means not configured. Consumed by `internal/app` (`webapp.New`)."
	webKeyDoc = "The public (anon or publishable) key of the Supabase project, which the browser sends to Auth with every sign-in: 1 to 2048 characters of printable ASCII with no spaces. It is PUBLIC by design (every visitor of the app receives it in `GET /config.json`, as `anon_key`), so it is not a secret and is not redacted; never put the project's service-role key or JWT secret here. **Required when PLATFORM_WEB_DIR is set** (the service refuses to start without it). The default (unset) means not configured. Consumed by `internal/app` (`webapp.New`)."
)

// ValidAnonKey is the rule PLATFORM_PUBLIC_SUPABASE_ANON_KEY is held to, here and in internal/webapp.New: 1 to 2048 bytes of printable ASCII with no space.
func ValidAnonKey(s string) bool { return validClaimText(s, maxAnonKeyBytes) }

// validWebDir accepts a directory path as text: 1 to maxWebDirBytes bytes of valid UTF-8 and no control character (a NUL would end the path in the operating system, a line break would forge a log
// line). Whether the directory exists is the service's to say at start-up (internal/webapp.New), not Load's: Load reads nothing but getenv.
func validWebDir(s string) bool {
	if len(s) < 1 || len(s) > maxWebDirBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ParsePublicURL judges the address the browser reaches Supabase at, and returns it as it is given to the browser (trailing slashes dropped) with its origin (`scheme://host[:port]`, the scheme in
// lower case: what the Content-Security-Policy names). It is the one rule PLATFORM_PUBLIC_SUPABASE_URL is held to, here and in internal/webapp.New:
//
//   - `http` or `https`, a host and no credentials, query or fragment (validModuleURL);
//   - the host is a name of letters, digits, dots and hyphens (no label empty, none starting or ending with a hyphen), or a bracketed IPv6 address; the port, when there is one, is a whole number from 1 to 65535;
//   - the path, when there is one, is letters, digits and `._~-/` with no empty segment and no `.` or `..`;
//   - at most maxPublicSupabaseURL characters.
//
// The alphabet is that narrow because the origin is written into a response header and the whole address into a JSON document: nothing outside it needs escaping anywhere, and a value that smuggles
// a `;` or a space into the policy cannot be written.
func ParsePublicURL(raw string) (full, origin string, ok bool) {
	if len(raw) < 1 || len(raw) > maxPublicSupabaseURL || !validModuleURL(raw) {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	host, port := u.Hostname(), u.Port()
	hostPort := host
	if strings.HasPrefix(u.Host, "[") {
		if !validIPv6Literal(host) {
			return "", "", false
		}
		hostPort = "[" + host + "]"
	} else if !validHostName(host) {
		return "", "", false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
			return "", "", false
		}
		hostPort += ":" + port
	}
	if hostPort != u.Host { // an empty port ("host:") or anything else the parts above do not account for
		return "", "", false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-' || c == '/') {
			return "", "", false
		}
	}
	if path != "" {
		for _, seg := range strings.Split(path[1:], "/") {
			if seg == "" || seg == "." || seg == ".." {
				return "", "", false
			}
		}
	}
	origin = strings.ToLower(u.Scheme) + "://" + hostPort
	return origin + path, origin, true
}

// validHostName accepts labels of letters, digits and hyphens, none empty, none longer than 63 bytes, none starting or ending with a hyphen.
func validHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// validIPv6Literal accepts the text between the brackets of an IPv6 host: hexadecimal digits, colons and dots (an embedded IPv4 address), at least two colons.
func validIPv6Literal(s string) bool {
	if s == "" || strings.Count(s, ":") < 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == ':' || c == '.') {
			return false
		}
	}
	return true
}

// publicKeyState is what a Config says about the public anon key: whether it is set, never the key (it is public, but a log line has no use for it).
func publicKeyState(k string) string {
	if k == "" {
		return unsetMarker
	}
	return "(set)"
}

// webCrossCheck is the rule that joins the three dials: the app cannot be served without what the browser needs to sign in, so PLATFORM_WEB_DIR set while either of the other two is not is a problem
// that names the missing dial (the service refuses to start, exit status 2). The reverse is not a problem: the two public dials alone only describe a Supabase project and serve nothing.
func webCrossCheck(c Config) []Problem {
	if c.WebDir == "" {
		return nil
	}
	var out []Problem
	if c.PublicSupabaseURL == "" {
		out = append(out, Problem{Name: VarPublicSupabaseURL, Reason: "is required when " + VarWebDir + " is set: the browser needs the Supabase address to sign in"})
	}
	if c.PublicSupabaseAnonKey == "" {
		out = append(out, Problem{Name: VarPublicSupabaseAnonKey, Reason: "is required when " + VarWebDir + " is set: the browser needs the Supabase public key to sign in"})
	}
	return out
}
