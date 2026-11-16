// Package config loads the platform's runtime configuration from the environment.
//
// CONFIG, NOT CODE. Every dial the process reads is declared once, in Vars(): name, default, whether it
// is required, whether it is a secret, and what it means. Load reads exactly those variables and nothing
// else; Docs/config-reference.md is checked against Vars() by a test in both directions, so the table an
// operator reads cannot drift from the code that reads it. A dial without a consumer is a bug: the tests
// in this package fail when a Var is not read by Load, when changing it changes nothing in Config, or
// when the documentation disagrees.
//
// Phase 0 reads process-wiring values only (address, credentials, timeouts of the HTTP server). Phase 2
// adds the process wiring of module 3's entitlement flow (where module 3's control is, the credential for
// its admin route, and how often the two loops run); the entitlement rules themselves live in the
// database. Tunables that govern product behaviour belong in the database's config table when it exists,
// not here.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/logging"
)

// The environment variables the platform reads. Exported so callers name them instead of repeating a
// string literal.
const (
	VarHTTPAddr             = "PLATFORM_HTTP_ADDR"
	VarDatabaseURL          = "PLATFORM_DATABASE_URL"
	VarLogLevel             = "PLATFORM_LOG_LEVEL"
	VarEnv                  = "PLATFORM_ENV"
	VarShutdownTimeoutSec   = "PLATFORM_SHUTDOWN_TIMEOUT_SEC"
	VarReadyzTimeoutMS      = "PLATFORM_READYZ_TIMEOUT_MS"
	VarReadHeaderTimeoutSec = "PLATFORM_READ_HEADER_TIMEOUT_SEC"

	// Module 3's entitlement flow (internal/entitlements, internal/m3admin).
	VarM3URL                        = "PLATFORM_M3_URL"
	VarM3AdminToken                 = "PLATFORM_M3_ADMIN_TOKEN"
	VarEntitlementExpireIntervalSec = "PLATFORM_ENTITLEMENT_EXPIRE_INTERVAL_SEC"
	VarM3ReconcileIntervalSec       = "PLATFORM_M3_RECONCILE_INTERVAL_SEC"
	VarM3ReverifyIntervalSec        = "PLATFORM_M3_REVERIFY_INTERVAL_SEC"
	VarM3RequestTimeoutSec          = "PLATFORM_M3_REQUEST_TIMEOUT_SEC"
)

// Upper bounds on the integer dials. They are sanity guards, not tuning: they keep a typo (an extra digit,
// a value in the wrong unit) from becoming a one-day shutdown, and keep the multiplication into a
// time.Duration far from overflow.
const (
	maxTimeoutSec = 3600
	maxReadyzMS   = 60000

	maxLoopIntervalSec = 3600  // the expiry and reconcile loops
	minReverifySec     = 10    // a re-check more often than this would be the reconcile loop's own job
	maxReverifySec     = 86400 // one day
	maxM3RequestSec    = 120
)

// Var describes one environment variable. Vars() is the single source of truth for every dial.
type Var struct {
	Name     string
	Default  string // "" for a variable with no default (a required one)
	Doc      string // the text of the Meaning column in Docs/config-reference.md, verbatim
	Required bool
	Secret   bool
}

// Vars returns every dial the platform reads, in documentation order. The slice is a fresh copy.
func Vars() []Var {
	return []Var{
		{Name: VarHTTPAddr, Default: ":8080",
			Doc: "TCP listen address of the HTTP server, as host:port. Port 0 picks a free port."},
		{Name: VarDatabaseURL, Required: true, Secret: true,
			Doc: "PostgreSQL connection string of the least-privileged `platform_app` login. Never printed or logged."},
		{Name: VarLogLevel, Default: "info",
			Doc: "Minimum log level: one of " + strings.Join(logging.LevelNames(), ", ") + "."},
		{Name: VarEnv, Default: "development",
			Doc: "Free-form label for the deployment (for example development or production), reported in the startup log line."},
		{Name: VarShutdownTimeoutSec, Default: "30",
			Doc: fmt.Sprintf("Seconds in-flight requests get to finish after SIGINT or SIGTERM before the remaining connections are cut. Integer, 1 to %d.", maxTimeoutSec)},
		{Name: VarReadyzTimeoutMS, Default: "2000",
			Doc: fmt.Sprintf("Milliseconds the readiness probe waits for the database ping before answering not ready. Integer, 1 to %d.", maxReadyzMS)},
		{Name: VarReadHeaderTimeoutSec, Default: "10",
			Doc: fmt.Sprintf("Seconds a client has to send its request headers before the connection is closed. Integer, 1 to %d.", maxTimeoutSec)},
		{Name: VarM3URL, Required: true,
			Doc: "Base URL of module 3's control service (http or https, with a host and no credentials), for example http://m3:8080. The platform suspends and resumes users through its admin route."},
		{Name: VarM3AdminToken, Required: true, Secret: true,
			Doc: "Module 3's operator (admin) key, sent as a bearer token on its admin route. A different credential from the service token. Never printed or logged."},
		{Name: VarEntitlementExpireIntervalSec, Default: "30",
			Doc: fmt.Sprintf("Seconds between passes of the expiry loop, which ends every M3 entitlement whose enabled-until date has passed by the database's clock. Integer, 1 to %d.", maxLoopIntervalSec)},
		{Name: VarM3ReconcileIntervalSec, Default: "5",
			Doc: fmt.Sprintf("Seconds between passes of the reconcile loop, which delivers each wanted suspend or resume to module 3 and records what module 3 answered. Integer, 1 to %d.", maxLoopIntervalSec)},
		{Name: VarM3ReverifyIntervalSec, Default: "300",
			Doc: fmt.Sprintf("Seconds after which a user already in the wanted state is checked against module 3 again (the same suspend or resume is sent once more; a changed answer means module 3 had drifted). Integer, %d to %d.", minReverifySec, maxReverifySec)},
		{Name: VarM3RequestTimeoutSec, Default: "10",
			Doc: fmt.Sprintf("Seconds one request to module 3's admin route may take before it counts as failed and is retried later. Integer, 1 to %d.", maxM3RequestSec)},
	}
}

// Config is the resolved configuration. It prints without its secret: see String, GoString, LogValue and
// the Secret type.
type Config struct {
	HTTPAddr          string
	DatabaseURL       Secret
	LogLevel          string // normalised to lower case
	Env               string
	ShutdownTimeout   time.Duration
	ReadyzTimeout     time.Duration
	ReadHeaderTimeout time.Duration

	M3URL                     string
	M3AdminToken              Secret
	EntitlementExpireInterval time.Duration
	M3ReconcileInterval       time.Duration
	M3ReverifyInterval        time.Duration
	M3RequestTimeout          time.Duration
}

// Problem is one invalid or missing variable. Reason is a fixed sentence: it never contains a value, so
// an error built from Problems cannot leak one.
type Problem struct {
	Name   string
	Reason string
}

// Error is the error Load returns: every problem found, not just the first.
type Error struct{ Problems []Problem }

func (e *Error) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Name + ": " + p.Reason
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// Load resolves the configuration from getenv. It never reads the process environment itself, so tests
// and callers control exactly what it sees. An unset or blank variable takes its default; a required
// variable with no value is a problem. All problems are collected and returned together as *Error, and
// none of them contains a value.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("config: Load needs a getenv function")
	}
	var (
		c        Config
		problems []Problem
	)
	fail := func(name, reason string) { problems = append(problems, Problem{Name: name, Reason: reason}) }
	intIn := func(name, raw string, lo, hi int) (int, bool) {
		n, err := strconv.Atoi(raw)
		if err != nil {
			fail(name, "must be an integer")
			return 0, false
		}
		if n < lo || n > hi {
			fail(name, fmt.Sprintf("must be between %d and %d", lo, hi))
			return 0, false
		}
		return n, true
	}

	for _, v := range Vars() {
		raw := strings.TrimSpace(getenv(v.Name))
		if raw == "" {
			if v.Required {
				fail(v.Name, "is required but not set")
				continue
			}
			raw = v.Default
		}
		switch v.Name {
		case VarHTTPAddr:
			if !validListenAddr(raw) {
				fail(v.Name, "must be a listen address as host:port, for example :8080")
				continue
			}
			c.HTTPAddr = raw
		case VarDatabaseURL:
			c.DatabaseURL = Secret(raw)
		case VarLogLevel:
			if _, err := logging.ParseLevel(raw); err != nil {
				fail(v.Name, err.Error())
				continue
			}
			c.LogLevel = strings.ToLower(raw)
		case VarEnv:
			c.Env = raw
		case VarShutdownTimeoutSec:
			if n, ok := intIn(v.Name, raw, 1, maxTimeoutSec); ok {
				c.ShutdownTimeout = time.Duration(n) * time.Second
			}
		case VarReadyzTimeoutMS:
			if n, ok := intIn(v.Name, raw, 1, maxReadyzMS); ok {
				c.ReadyzTimeout = time.Duration(n) * time.Millisecond
			}
		case VarReadHeaderTimeoutSec:
			if n, ok := intIn(v.Name, raw, 1, maxTimeoutSec); ok {
				c.ReadHeaderTimeout = time.Duration(n) * time.Second
			}
		case VarM3URL:
			if !validM3URL(raw) {
				fail(v.Name, "must be an http or https URL with a host and no credentials, for example http://m3:8080")
				continue
			}
			c.M3URL = raw
		case VarM3AdminToken:
			if !validBearerToken(raw) {
				fail(v.Name, "must be printable ASCII with no spaces (it is sent as a bearer token)")
				continue
			}
			c.M3AdminToken = Secret(raw)
		case VarEntitlementExpireIntervalSec:
			if n, ok := intIn(v.Name, raw, 1, maxLoopIntervalSec); ok {
				c.EntitlementExpireInterval = time.Duration(n) * time.Second
			}
		case VarM3ReconcileIntervalSec:
			if n, ok := intIn(v.Name, raw, 1, maxLoopIntervalSec); ok {
				c.M3ReconcileInterval = time.Duration(n) * time.Second
			}
		case VarM3ReverifyIntervalSec:
			if n, ok := intIn(v.Name, raw, minReverifySec, maxReverifySec); ok {
				c.M3ReverifyInterval = time.Duration(n) * time.Second
			}
		case VarM3RequestTimeoutSec:
			if n, ok := intIn(v.Name, raw, 1, maxM3RequestSec); ok {
				c.M3RequestTimeout = time.Duration(n) * time.Second
			}
		default:
			// A Var that Load has no case for would be read and then dropped: a dial with no consumer.
			fail(v.Name, "is declared in Vars() but Load does not consume it (programming error)")
		}
	}
	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	return c, nil
}

// validListenAddr accepts host:port with a numeric port in 0..65535 (":8080", "127.0.0.1:0").
func validListenAddr(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}

// validM3URL accepts an absolute http or https URL with a host, no credentials, no query and no fragment
// (the base URL the module-3 client appends its routes to). A URL with credentials is refused rather than
// accepted: this variable is not a secret, so a credential in it would be printed.
func validM3URL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Hostname() != ""
}

// validBearerToken accepts printable ASCII without spaces: the characters an HTTP bearer credential can carry
// (module 3's client refuses anything else, so a stray newline or a pasted non-ASCII character fails at
// start-up with the variable named, not at the first request).
func validBearerToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return s != ""
}

// attrs is the one place Config is rendered; String, GoString and LogValue all derive from it, so they
// cannot disagree about what is shown. The database URL appears only as the redaction marker.
func (c Config) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("http_addr", c.HTTPAddr),
		slog.String("database_url", c.DatabaseURL.String()),
		slog.String("log_level", c.LogLevel),
		slog.String("env", c.Env),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Duration("readyz_timeout", c.ReadyzTimeout),
		slog.Duration("read_header_timeout", c.ReadHeaderTimeout),
		slog.String("m3_url", c.M3URL),
		slog.String("m3_admin_token", c.M3AdminToken.String()),
		slog.Duration("entitlement_expire_interval", c.EntitlementExpireInterval),
		slog.Duration("m3_reconcile_interval", c.M3ReconcileInterval),
		slog.Duration("m3_reverify_interval", c.M3ReverifyInterval),
		slog.Duration("m3_request_timeout", c.M3RequestTimeout),
	}
}

// LogValue implements slog.LogValuer, so logging a Config (or a pointer to one) can never reveal the URL.
func (c Config) LogValue() slog.Value { return slog.GroupValue(c.attrs()...) }

// String renders the Config without its secret.
func (c Config) String() string {
	attrs := c.attrs()
	parts := make([]string, len(attrs))
	for i, a := range attrs {
		parts[i] = a.Key + "=" + a.Value.String()
	}
	return "config{" + strings.Join(parts, " ") + "}"
}

// GoString is what %#v prints; it is the same redacted rendering.
func (c Config) GoString() string { return c.String() }
