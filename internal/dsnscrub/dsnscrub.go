// Package dsnscrub suppresses driver errors whose text embeds the database connection string.
//
// WHY THIS EXISTS. pgx can return an error whose Error() quotes the connection string it was given
// (a ParseConfigError does), and its own redaction does not cover every spelling the driver honors
// (the module this one is modelled on measured a password in a URL query string, `?password=` and
// `?sslpassword=`, and the spaced keyword form `password = SECRET`, all surviving in clear). An error
// that reaches a log line, a terminal or a ticket must never carry a credential, so every error the
// platform gets from the driver for a connection string passes through Scrub before anyone can print it.
//
// WHICH SHAPES LEAK IS A MEASUREMENT, NOT A CLAIM. The shapes, the pgx version they were measured at,
// and the one shape the task description expected to leak but does not, are pinned by the tests in this
// package (TestRawPgxErrorLeaksForTheLeakyShapes and its siblings re-measure them on every run). A test
// inherited from another module is a test whose premise was verified somewhere else.
//
// WHY THE CHECK IS DERIVED, NOT A PATTERN LIST. Scrub asks "does this message contain a credential
// candidate taken from the DSN I was handed", not "does it match a known-bad spelling". A blocklist of
// `password=` spellings is precisely how the driver's own redaction came to miss two of them, and a
// blocklist cannot cover a future release that leaks a field nobody has thought of yet.
package dsnscrub

import (
	"net/url"
	"strings"
)

// suppressed is the whole replacement message. Deliberately NOT a partial redaction of the original:
// selective rewriting is what failed upstream, because it keeps whatever the pattern missed. The one
// fact an operator needs is which variable to look at.
const suppressed = "connection string rejected by the driver (message suppressed: it embedded the DSN, check PLATFORM_DATABASE_URL)"

// scrubbedError replaces a driver error whose text embeds the DSN. Unwrap is kept so errors.Is and
// errors.As still reach the original (a caller may legitimately test for a pgx sentinel); only the
// STRING, the thing that reaches a log, is replaced.
type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// Scrub returns err unchanged when its text cannot expose the DSN, and a replacement whose text carries
// neither the password nor the DSN when it can. A nil err returns nil, so it composes at a return site
// without a guard. An empty dsn gives nothing to look for and returns err unchanged.
func Scrub(err error, dsn string) error {
	if err == nil {
		return nil
	}
	if dsn == "" {
		return err
	}
	msg := err.Error()
	// The DSN appearing whole is enough on its own: user, host, port and database are not credentials,
	// but they are infrastructure detail with no business in a log line.
	leaks := strings.Contains(msg, dsn)
	if !leaks {
		for _, cand := range credentialCandidates(dsn) {
			if strings.Contains(msg, cand) {
				leaks = true
				break
			}
		}
	}
	if !leaks {
		return err
	}
	return &scrubbedError{err: err, msg: suppressed}
}

// structuralKeys are the libpq/pgx connection keywords whose VALUES cannot be a credential: they name
// where the database is and how to reach it, not who you are.
//
// THE DIRECTION OF RISK IS DELIBERATE. A key MISSING from this map is over-collection: its value becomes
// a candidate, and the cost is one error message losing its detail. A key WRONGLY PRESENT is
// under-collection, and that costs a credential, so this map holds only keywords whose libpq semantics
// make a secret value impossible. `password` and `sslpassword` are absent on purpose, as is every keyword
// nobody has enumerated: an unknown key is treated as possibly secret.
//
// `sslkey` and `passfile` are PATHS, not secrets: the file they point at holds the secret.
var structuralKeys = map[string]bool{
	"host": true, "hostaddr": true, "port": true, "dbname": true, "database": true,
	"user": true, "application_name": true, "fallback_application_name": true,
	"connect_timeout": true, "client_encoding": true, "options": true, "service": true,
	"sslmode": true, "sslrootcert": true, "sslcert": true, "sslcrl": true, "sslcrldir": true,
	"sslsni": true, "sslnegotiation": true, "sslkey": true, "passfile": true,
	"target_session_attrs": true, "load_balance_hosts": true, "channel_binding": true,
	"require_auth": true, "gssencmode": true, "gsslib": true, "krbsrvname": true,
	"replication": true, "tcp_user_timeout": true,
	"keepalives": true, "keepalives_idle": true, "keepalives_interval": true, "keepalives_count": true,
	"pool_max_conns": true, "pool_min_conns": true, "pool_min_idle_conns": true,
	"pool_max_conn_lifetime": true, "pool_max_conn_lifetime_jitter": true,
	"pool_max_conn_idle_time": true, "pool_health_check_period": true, "pool_ping_timeout": true,
	"search_path": true,
}

// knownKey is structuralKeys plus the credential-bearing keywords. Used only to recognise a BARE token
// as a key awaiting its spaced '=' rather than as a value.
func knownKey(s string) bool {
	return structuralKeys[s] || s == "password" || s == "sslpassword"
}

// minCandidateLen: under 4 characters a "secret" is likelier to appear incidentally in a driver message
// ("db", "u") than to be a real credential, and matching it would suppress every error. A backstop
// beneath structuralKeys, not the primary defence (5432 is four characters).
const minCandidateLen = 4

// credentialCandidates extracts every substring of the DSN that could be a secret, in every spelling
// pgx/libpq honors: URL userinfo password, URL query values, and keyword/value values (quoted, unquoted,
// spaced or not).
//
// THE SPACED SPELLING IS WHY THIS TRACKS STATE. libpq honors `password = SECRET`, which splits into three
// whitespace fields, so the value arrives detached from its key. Collecting EVERY bare token instead
// makes `port=5432` yield the candidate "5432", which appears in virtually every connection error
// ("dial tcp 10.0.0.5:5432: connect: connection refused"), so the scrub would fire on a message
// containing no credential and replace an operator's whole diagnosis with a suppression notice.
func credentialCandidates(dsn string) []string {
	var out []string
	add := func(s string) {
		if len(s) >= minCandidateLen {
			out = append(out, s)
		}
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
		for k, vs := range u.Query() {
			if structuralKeys[k] {
				continue
			}
			for _, v := range vs {
				add(v)
			}
		}
	}
	// Keyword/value form, and the query-ish tail of a URL that failed to parse.
	var pendingKey string
	for _, field := range strings.FieldsFunc(dsn, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '&' || r == '?'
	}) {
		if eq := strings.IndexByte(field, '='); eq >= 0 {
			key := strings.TrimSpace(field[:eq])
			val := strings.Trim(strings.TrimSpace(field[eq+1:]), `'"`)
			if key == "" {
				// A lone "=" or "=VALUE": the key was the previous field (`password = SECRET`).
				key = pendingKey
			}
			if val == "" {
				// `password =` : the value is the next field.
				pendingKey = key
				continue
			}
			pendingKey = ""
			if !structuralKeys[key] {
				add(val)
			}
			continue
		}
		token := strings.Trim(field, `'"`)
		if pendingKey != "" {
			// The detached value of a spaced pair.
			if !structuralKeys[pendingKey] {
				add(token)
			}
			pendingKey = ""
			continue
		}
		if knownKey(token) {
			// A key awaiting its spaced '='. Not a value, so not a candidate: collecting it would make the
			// literal word "password" a candidate, which suppresses every
			// `password authentication failed for user "u"` the server ever returns.
			pendingKey = token
			continue
		}
		// An unrecognised bare token: possibly the value of a spaced pair for a keyword this code has
		// never heard of. Over-collect, per the risk direction documented on structuralKeys.
		add(token)
	}
	return out
}
