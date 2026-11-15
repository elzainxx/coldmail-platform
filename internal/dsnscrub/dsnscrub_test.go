package dsnscrub

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A DSN credential must never reach a log line. These tests exercise the REAL driver's error text
// (pgxpool.ParseConfig), because a scrub verified only against hand-written error strings proves
// nothing about what pgx actually prints.
//
// EVERY TRIGGER HERE IS MEASURED, NOT ASSUMED. `sslmode=bogusmode` and an out-of-range port make pgx
// fail at PARSE time (no socket, no Postgres, deterministic). Which shapes leak is a property of the
// pinned pgx release, so TestPgxMeasurement_* re-measures it on every run: if a pgx bump changes which
// shapes leak, those tests go red and say so, instead of the suite quietly going vacuous.
//
// THE FIXTURE THE TASK NAMED DOES NOT LEAK. `postgres://u:<pw>@h:5432/db?sslmode=bogusmode` fails at
// parse time as expected, but pgx v5.13.0 redacts the userinfo password in the message
// (`postgres://u:xxxxx@h...`), so "the raw error contains the password" is false for it and a scrub test
// built on it alone would be vacuous. It stays in the table below as a pinned "pgx redacts this" case;
// the shapes that DO leak at this pin are the keyword/value spellings pgx's redaction regexp cannot match.

const pw = "s3cretPW"

type shape struct {
	name     string
	dsn      string
	pgxLeaks bool // measured: the RAW pgx error text contains pw
}

func shapes() []shape {
	return []shape{
		// pgx v5.13.0 leaks these (its redaction only matches the unspaced `password=`).
		{"kv spaced on both sides", "host=h password = " + pw + " dbname=db sslmode=bogusmode", true},
		{"kv space before equals", "host=h password =" + pw + " dbname=db sslmode=bogusmode", true},
		{"kv space after equals", "host=h password= " + pw + " dbname=db sslmode=bogusmode", true},
		{"bare token (the whole variable is the secret)", pw, true},
		// pgx v5.13.0 redacts these itself. Pinned so a regression upstream is noticed.
		{"url userinfo (the task's named fixture)", "postgres://u:" + pw + "@h:5432/db?sslmode=bogusmode", false},
		{"url userinfo, bad port", "postgres://u:" + pw + "@h:99999999/db", false},
		{"url query password", "postgres://u@h:5432/db?password=" + pw + "&sslmode=bogusmode", false},
		{"url query sslpassword", "postgres://u@h:5432/db?sslpassword=" + pw + "&sslmode=bogusmode", false},
		{"kv unspaced password", "host=h password=" + pw + " dbname=db sslmode=bogusmode", false},
	}
}

// parseErr returns the raw driver error for a shape and fails the test if there is none: a DSN that
// parses cleanly would make every assertion about "the error text" vacuous.
func parseErr(t *testing.T, dsn string) error {
	t.Helper()
	_, err := pgxpool.ParseConfig(dsn)
	if err == nil {
		t.Fatalf("pgxpool.ParseConfig(%q) returned no error: the trigger no longer fires at this pgx release, so every assertion about its error text would be vacuous", dsn)
	}
	return err
}

// leakViolations is the property, as a function, so a test can also run it against a scrub that is
// known to be wrong. It returns one line per shape whose scrubbed error still shows the password or the
// whole DSN (or is nil for a non-nil input).
func leakViolations(t *testing.T, scrub func(error, string) error) []string {
	t.Helper()
	var bad []string
	for _, s := range shapes() {
		out := scrub(parseErr(t, s.dsn), s.dsn)
		switch {
		case out == nil:
			bad = append(bad, s.name+": scrub returned nil for a non-nil error")
		case strings.Contains(out.Error(), pw):
			bad = append(bad, s.name+": the password survived in the error text")
		case strings.Contains(out.Error(), s.dsn):
			bad = append(bad, s.name+": the whole DSN survived in the error text")
		}
	}
	return bad
}

func TestScrub_NoCredentialSurvivesRealPgxErrors(t *testing.T) {
	if got := len(shapes()); got < 5 {
		t.Fatalf("only %d shapes under test, want a real table", got)
	}
	if bad := leakViolations(t, Scrub); len(bad) != 0 {
		t.Fatalf("Scrub let a credential through:\n%s", strings.Join(bad, "\n"))
	}
}

// TestPgxMeasurement_RawErrorLeaksForTheLeakyShapes is the PAIRED CONTROL of the test above: it asserts
// the raw driver error still contains the password for the shapes marked leaky (so the scrub test
// defends something), and still does NOT for the shapes pgx redacts (so the table is not mislabelled).
func TestPgxMeasurement_RawErrorLeaksForTheLeakyShapes(t *testing.T) {
	leaky := 0
	for _, s := range shapes() {
		t.Run(s.name, func(t *testing.T) {
			raw := parseErr(t, s.dsn).Error()
			got := strings.Contains(raw, pw)
			if got != s.pgxLeaks {
				t.Fatalf("PREMISE CHANGED for %q: raw pgx error contains the password = %v, table says %v.\nraw error: %s\n"+
					"Re-measure before touching anything: a shape that stopped leaking makes the scrub test vacuous for it.",
					s.name, got, s.pgxLeaks, raw)
			}
		})
		if s.pgxLeaks {
			leaky++
		}
	}
	if leaky == 0 {
		t.Fatal("no shape leaks at this pgx release: the scrub tests have nothing to defend, which is a premise failure to investigate, not a pass")
	}
}

// TestScrub_ViolationCheckDetectsABrokenScrub proves leakViolations can fail: an identity scrub (the
// bug Scrub exists to prevent) must be reported for exactly the shapes that leak at this pin, and not
// for the ones pgx already redacts.
func TestScrub_ViolationCheckDetectsABrokenScrub(t *testing.T) {
	identity := func(err error, _ string) error { return err }
	bad := leakViolations(t, identity)

	var want []string
	for _, s := range shapes() {
		if s.pgxLeaks {
			want = append(want, s.name)
		}
	}
	var got []string
	for _, line := range bad {
		got = append(got, strings.SplitN(line, ": ", 2)[0])
	}
	sort.Strings(want)
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the identity scrub should be flagged for exactly the leaky shapes\n got: %q\nwant: %q", got, want)
	}

	// And a nil-returning scrub is flagged for every shape.
	if n := len(leakViolations(t, func(error, string) error { return nil })); n != len(shapes()) {
		t.Fatalf("a scrub that returns nil was flagged for %d shapes, want all %d", n, len(shapes()))
	}
}

func TestScrub_WrappedDriverErrorIsStillScrubbed(t *testing.T) {
	for _, s := range shapes() {
		if !s.pgxLeaks {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			wrapped := fmt.Errorf("create database pool: %w", parseErr(t, s.dsn))
			if !strings.Contains(wrapped.Error(), pw) {
				t.Fatal("control failed: the wrapped error does not carry the password, so scrubbing it proves nothing")
			}
			if out := Scrub(wrapped, s.dsn); strings.Contains(out.Error(), pw) {
				t.Fatalf("password survived scrubbing of a wrapped error: %s", out)
			}
		})
	}
}

// TestScrub_CleanErrorsKeepTheirDetail is the other half of the anti-vacuity pairing: a scrub that fired
// on everything would pass every leak test while destroying every diagnosis, which is a different outage.
func TestScrub_CleanErrorsKeepTheirDetail(t *testing.T) {
	dsn := "host=h port=5432 user=u password=" + pw + " dbname=db sslmode=disable"

	// A REAL pgx error that carries no credential: a malformed pool_max_conns is rejected with the
	// value in the message but without the DSN's password (the DSN's own password is redacted).
	if _, raw := pgxpool.ParseConfig(dsn + " pool_max_conns=notanumber"); raw == nil {
		t.Fatal("expected a real pgx error for the malformed pool_max_conns")
	} else if strings.Contains(raw.Error(), pw) {
		t.Fatalf("control failed: this pgx release leaks the password for pool_max_conns too: %s", raw)
	} else if out := Scrub(raw, dsn+" pool_max_conns=notanumber"); out != raw {
		t.Fatalf("a credential-free pgx error was replaced: %s", out)
	}

	// The error an operator actually needs when the database is down: it names the peer, not the secret.
	refused := errors.New("failed to connect to `user=u database=db`: dial tcp 10.0.0.5:5432: connect: connection refused")
	out := Scrub(refused, dsn)
	if out != refused {
		t.Fatalf("a connection-refused message was suppressed (the port 5432 must not count as a credential): %s", out)
	}
	if !strings.Contains(out.Error(), "connection refused") {
		t.Fatalf("diagnostic detail lost: %s", out)
	}
}

func TestScrub_ServerSideAuthFailureTextIsNotSuppressed(t *testing.T) {
	dsn := "host=h password = " + pw + " dbname=db"
	msg := errors.New(`failed to connect: server error: password authentication failed for user "u" (SQLSTATE 28P01)`)
	if out := Scrub(msg, dsn); out != msg {
		t.Fatalf("the literal word password became a candidate and suppressed a server message: %s", out)
	}
	// Paired control: when the message DOES carry the credential, it is suppressed.
	leaky := errors.New("failed to connect: password " + pw + " rejected")
	if out := Scrub(leaky, dsn); strings.Contains(out.Error(), pw) {
		t.Fatalf("control failed: a message carrying the credential survived: %s", out)
	}
}

func TestScrub_NilEmptyDSNAndUnwrap(t *testing.T) {
	if Scrub(nil, "host=h password = "+pw) != nil {
		t.Fatal("a nil error must stay nil")
	}

	// An empty DSN has nothing to look for: it must not match every message and suppress everything.
	plain := errors.New("some error")
	if Scrub(plain, "") != plain {
		t.Fatal("an empty DSN suppressed an unrelated error")
	}

	// A scrubbed error still unwraps to the original, so errors.Is/As keep working for callers.
	sentinel := errors.New("sentinel with " + pw)
	scrubbed := Scrub(sentinel, "host=h password = "+pw)
	if strings.Contains(scrubbed.Error(), pw) {
		t.Fatalf("control failed: the sentinel's credential survived: %s", scrubbed)
	}
	if !errors.Is(scrubbed, sentinel) {
		t.Fatal("errors.Is no longer reaches the original error through the scrubbed one")
	}
	if scrubbed == sentinel {
		t.Fatal("control failed: nothing was replaced, so the Is check above proved nothing")
	}
}

func TestScrub_CandidatesAreDerivedFromTheDSN(t *testing.T) {
	// Credential-bearing values are collected in every spelling...
	cases := map[string]string{
		"url userinfo":        "postgres://u:" + pw + "@h/db",
		"url unknown param":   "postgres://u@h/db?apikey=" + pw,
		"kv unspaced":         "host=h password=" + pw,
		"kv spaced":           "host=h password = " + pw,
		"kv quoted":           "host=h password='" + pw + "'",
		"kv unknown keyword":  "host=h token = " + pw,
		"kv sslpassword":      "host=h sslpassword=" + pw,
		"url sslpassword":     "postgres://u@h/db?sslpassword=" + pw,
		"kv detached trailer": "host=h password= " + pw,
	}
	for name, dsn := range cases {
		t.Run("collects/"+name, func(t *testing.T) {
			if !contains(credentialCandidates(dsn), pw) {
				t.Fatalf("credentialCandidates(%q) = %q, want it to include the secret", dsn, credentialCandidates(dsn))
			}
		})
	}
	// ...and structural values are not, which is what stops port=5432 from suppressing every message.
	structural := credentialCandidates("host=db.internal port=5432 user=platform_app dbname=platform sslmode=require")
	for _, c := range structural {
		t.Errorf("structural value %q was collected as a credential candidate", c)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
