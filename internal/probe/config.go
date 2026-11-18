// Package probe is platform-probe: the platform's role as the first caller of the three modules. For each
// service it checks health and readiness, and for each of the three modules the credential-gated describe
// (the senders' service token, module 1's per-caller key), and it prints one line per check. Every check can fail, a check that cannot run is a failure and not a
// skip, and the run is red when fewer checks ran than the floor.
package probe

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The environment variables platform-probe reads. Docs/probe-reference.md documents each; a test fails
// when the document and Vars() disagree in either direction.
const (
	VarPlatformURL = "PROBE_PLATFORM_URL"
	VarM1URL       = "PROBE_M1_URL"
	VarM1Key       = "PROBE_M1_KEY"
	VarM2URL       = "PROBE_M2_URL"
	VarM2Token     = "PROBE_M2_TOKEN"
	VarM3URL       = "PROBE_M3_URL"
	VarM3Token     = "PROBE_M3_TOKEN"
	VarTimeoutSec  = "PROBE_TIMEOUT_SEC"
	VarMinChecks   = "PROBE_MIN_CHECKS"
)

const (
	defaultTimeoutSec = 10
	defaultMinChecks  = 25
	maxTimeoutSec     = 600
	maxMinChecks      = 1000
)

// Var describes one environment variable.
type Var struct {
	Name     string
	Default  string // "" when there is none
	Required bool
	Secret   bool
	Doc      string // the Meaning column of the reference table, verbatim
}

// Vars lists every variable LoadConfig reads, in the order the reference documents them.
func Vars() []Var {
	return []Var{
		{VarPlatformURL, "", true, false, "Base URL of the platform service, as http://host:port. No credentials, query or fragment."},
		{VarM1URL, "", true, false, "Base URL of module 1 (verify). Same rules as the platform URL."},
		{VarM1Key, "", true, true, "Raw per-caller bearer key for module 1, sent on the job lookups and on describe. Never printed."},
		{VarM2URL, "", true, false, "Base URL of module 2 (send_managed). Same rules as the platform URL."},
		{VarM2Token, "", true, true, "Service token for module 2, sent as a bearer credential to describe. Never printed."},
		{VarM3URL, "", true, false, "Base URL of module 3 (send_byo). Same rules as the platform URL."},
		{VarM3Token, "", true, true, "Service token for module 3, sent as a bearer credential to describe. Never printed."},
		{VarTimeoutSec, strconv.Itoa(defaultTimeoutSec), false, false, "Seconds one request may take, from connect to the last body byte, before its checks fail with timeout. Integer, 1 to 600."},
		{VarMinChecks, strconv.Itoa(defaultMinChecks), false, false, "The run is red when fewer checks than this were executed. Integer, 1 to 1000."},
	}
}

// Config is the validated configuration. URLs have no trailing slash.
type Config struct {
	PlatformURL string
	M1URL       string
	M1Key       Secret
	M2URL       string
	M2Token     Secret
	M3URL       string
	M3Token     Secret
	Timeout     time.Duration
	MinChecks   int
}

// secrets are the values the Recorder scrubs out of everything it writes.
func (c Config) secrets() []Secret { return []Secret{c.M1Key, c.M2Token, c.M3Token} }

// ConfigProblem is one thing wrong with one variable. Reason never contains the variable's value.
type ConfigProblem struct {
	Var    string
	Reason string
}

// ConfigError lists every problem found, so one run reports all of them.
type ConfigError struct{ Problems []ConfigProblem }

func (e *ConfigError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Var + " " + p.Reason
	}
	return strings.Join(parts, "; ")
}

// LoadConfig reads the variables through getenv. A blank value counts as unset. The error, when there is
// one, is a *ConfigError that names each bad variable and never echoes a value (a URL may carry
// credentials, and a secret is a secret).
func LoadConfig(getenv func(string) string) (Config, error) {
	var cfg Config
	var probs []ConfigProblem
	bad := func(name, reason string) { probs = append(probs, ConfigProblem{name, reason}) }
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }

	urlVar := func(name string, dst *string) {
		raw := get(name)
		if raw == "" {
			bad(name, "is required but not set")
			return
		}
		base, why := normaliseURL(raw)
		if why != "" {
			bad(name, why)
			return
		}
		*dst = base
	}
	secretVar := func(name string, dst *Secret) {
		raw := get(name)
		if raw == "" {
			bad(name, "is required but not set")
			return
		}
		if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			bad(name, "contains whitespace or a control character")
			return
		}
		*dst = Secret(raw)
	}
	intVar := func(name string, def, lo, hi int) int {
		raw := get(name)
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < lo || n > hi {
			bad(name, fmt.Sprintf("must be an integer from %d to %d", lo, hi))
			return def
		}
		return n
	}

	urlVar(VarPlatformURL, &cfg.PlatformURL)
	urlVar(VarM1URL, &cfg.M1URL)
	secretVar(VarM1Key, &cfg.M1Key)
	urlVar(VarM2URL, &cfg.M2URL)
	secretVar(VarM2Token, &cfg.M2Token)
	urlVar(VarM3URL, &cfg.M3URL)
	secretVar(VarM3Token, &cfg.M3Token)
	cfg.Timeout = time.Duration(intVar(VarTimeoutSec, defaultTimeoutSec, 1, maxTimeoutSec)) * time.Second
	cfg.MinChecks = intVar(VarMinChecks, defaultMinChecks, 1, maxMinChecks)

	if len(probs) > 0 {
		return Config{}, &ConfigError{Problems: probs}
	}
	return cfg, nil
}

// normaliseURL accepts scheme://host[:port][/prefix] and returns it without a trailing slash. The second
// result is the reason it was refused, "" when it was accepted. The reason never contains the input.
func normaliseURL(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "is not a valid URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "must be an http or https URL"
	}
	if u.Hostname() == "" {
		return "", "has no host"
	}
	if u.User != nil {
		return "", "must not contain credentials"
	}
	if strings.ContainsAny(raw, "?#") {
		return "", "must not contain a query or fragment"
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), ""
}
