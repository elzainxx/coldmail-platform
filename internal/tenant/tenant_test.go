package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/money"
)

// ---- fakes: the database half is the queries the resolver asks, so the unit tests drive it with answers ----

const (
	userA = "a1a1a1a1-0000-4000-8000-00000000000a"
	userB = "b2b2b2b2-0000-4000-8000-00000000000b"
)

// word builds a stand-in secret of a given size from a repeated word (never a long literal next to the word secret).
func word(w string, size int) string { return strings.Repeat(w, size/len(w)+1)[:size] }

type rowFunc func(dest ...any) error

func (f rowFunc) Scan(dest ...any) error { return f(dest...) }

// userRow answers the resolver's query with one platform user.
func userRow(id, auth string, active bool) pgx.Row {
	return rowFunc(func(dest ...any) error {
		if len(dest) != 3 {
			return fmt.Errorf("the resolver scans %d columns, the query returns 3", len(dest))
		}
		*(dest[0].(*string)), *(dest[1].(*string)), *(dest[2].(*bool)) = id, auth, active
		return nil
	})
}

func errRow(err error) pgx.Row { return rowFunc(func(...any) error { return err }) }

type dbCall struct {
	sql  string
	args []any
}

// fakeDB records every query and answers with answer(call index).
type fakeDB struct {
	calls  []dbCall
	answer func(n int, sql string, args []any) pgx.Row
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls = append(f.calls, dbCall{sql, args})
	return f.answer(len(f.calls)-1, sql, args)
}

// forbiddenDB fails the test if it is ever asked anything: a refusal decided before the database costs no query.
func forbiddenDB(t *testing.T) *fakeDB {
	return &fakeDB{answer: func(n int, sql string, _ []any) pgx.Row {
		t.Errorf("the database was asked (%q) by a call that must be refused before it", sql)
		return errRow(errors.New("forbidden"))
	}}
}

func resolverOver(t *testing.T, db DB, secret string) *Resolver {
	t.Helper()
	r, err := NewResolver(db, config.Secret(secret))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

// ---- ids ----

func TestParseID_AcceptsOnlyTheHyphenatedFormInEitherCaseAndReturnsTheLowerCaseOne(t *testing.T) {
	good := map[string]string{
		userA:                                  userA,
		strings.ToUpper(userA):                 userA, // the same login in the other spelling
		"0f0f0f0f-0000-4000-8000-000000000000": "0f0f0f0f-0000-4000-8000-000000000000",
		"ABCDEF01-2345-6789-ABCD-EF0123456789": "abcdef01-2345-6789-abcd-ef0123456789",
		SystemUserID:                           SystemUserID,
	}
	for in, want := range good {
		if got, ok := parseID(in); !ok || got != want {
			t.Errorf("parseID(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	bad := []string{
		"", " ", userA + " ", " " + userA, userA + "\n", userA[:35], userA + "0",
		strings.ReplaceAll(userA, "-", ""),                          // no hyphens: Postgres would accept it
		"{" + userA + "}",                                           // braces: Postgres would accept it
		"urn:uuid:" + userA,                                         // a urn: nobody accepts it
		"a1a1a1a10-000-4000-8000-00000000000a",                      // hyphens in the wrong places
		"a1a1a1a1-0000-4000-8000-00000000000g",                      // not hexadecimal
		"a1a1a1a1-0000-4000-8000-00000000000\x00",                   // a NUL
		"a1a1a1a1_0000_4000_8000_00000000000a",                      // other separators
		"é1a1a1a1-0000-4000-8000-00000000000a",                      // wide characters count as bytes, not as digits
		"'; DROP TABLE users; --" + strings.Repeat("0", 10) + "xxx", // hostile, 36 bytes long
	}
	for _, in := range bad {
		if got, ok := parseID(in); ok {
			t.Errorf("parseID(%q) accepted it as %q", in, got)
		}
	}
	if len(bad) < 10 {
		t.Fatal("the list of bad ids shrank")
	}
	// isCanonical is the lower-case spelling only
	if !isCanonical(userA) || isCanonical(strings.ToUpper(userA)) || isCanonical("") {
		t.Errorf("isCanonical: lower-case %v, upper-case %v, empty %v; want true, false, false", isCanonical(userA), isCanonical(strings.ToUpper(userA)), isCanonical(""))
	}
}

func TestSystemUserID_IsTheMoneyStoresSystemUser(t *testing.T) {
	if SystemUserID != money.SystemUser {
		t.Fatalf("SystemUserID = %q, money.SystemUser = %q: the two packages must name the same reserved user", SystemUserID, money.SystemUser)
	}
	if got := strings.Repeat("0", 8) + "-0000-0000-0000-" + strings.Repeat("0", 12); SystemUserID != got {
		t.Fatalf("SystemUserID = %q, want the all-zero uuid %q", SystemUserID, got)
	}
}

// ---- ByAuthUser ----

// A malformed id and the all-zero id are decided BEFORE the database: the paired control is a good id on the same resolver, which reaches the database exactly once, in the lower-case spelling.
func TestByAuthUser_RefusesBadAndSystemIDsBeforeTheDatabase(t *testing.T) {
	canary := "CANARY-token-" + word("x", 30) // 43 bytes: not an id, and it must not be echoed
	r := resolverOver(t, forbiddenDB(t), "")
	for _, c := range []struct {
		name, id string
		want     error
		not      error
	}{
		{"empty", "", ErrBadID, ErrSystemUser},
		{"a word", "not-a-uuid", ErrBadID, ErrSystemUser},
		{"a token-like value", canary, ErrBadID, ErrSystemUser},
		{"no hyphens", strings.ReplaceAll(userA, "-", ""), ErrBadID, ErrSystemUser},
		{"braces", "{" + userA + "}", ErrBadID, ErrSystemUser},
		{"trailing newline", userA + "\n", ErrBadID, ErrSystemUser},
		{"the system user", SystemUserID, ErrSystemUser, ErrBadID},
	} {
		got, err := r.ByAuthUser(context.Background(), c.id)
		if !errors.Is(err, c.want) || errors.Is(err, c.not) {
			t.Errorf("%s: err = %v, want errors.Is(%v) and not %v", c.name, err, c.want, c.not)
		}
		if got != (Tenant{}) {
			t.Errorf("%s: returned the value %+v along with an error", c.name, got)
		}
		if err != nil && strings.Contains(err.Error(), canary) {
			t.Errorf("%s: the error echoes the refused value: %v", c.name, err)
		}
	}

	db := &fakeDB{answer: func(int, string, []any) pgx.Row { return userRow(userA, userA, true) }}
	good := resolverOver(t, db, "")
	got, err := good.ByAuthUser(context.Background(), strings.ToUpper(userA))
	if err != nil || got != (Tenant{UserID: userA, AuthUserID: userA, Active: true}) {
		t.Fatalf("control: ByAuthUser(upper-case login) = %+v, %v; want the user in canonical form", got, err)
	}
	if len(db.calls) != 1 || len(db.calls[0].args) != 1 || db.calls[0].args[0] != userA {
		t.Fatalf("control: the database was asked %+v, want one query with the lower-case login as its only parameter", db.calls)
	}
}

// Every refusal has its own error, and every answer that is not a refusal is a plain database failure that is none of them.
func TestByAuthUser_MapsTheDatabaseAnswerToTheTypedErrors(t *testing.T) {
	typed := []error{ErrUnknownLogin, ErrInactive, ErrSystemUser, ErrBadID, ErrTenantMismatch, ErrNoM1KeySecret, ErrNoDatabase}
	boom := errors.New("connection reset by peer")
	for _, c := range []struct {
		name string
		row  pgx.Row
		want error // nil: a plain failure
	}{
		{"no platform user for the login", errRow(pgx.ErrNoRows), ErrUnknownLogin},
		{"an inactive user", userRow(userA, userA, false), ErrInactive},
		{"a row that is the system user", userRow(SystemUserID, userA, true), ErrSystemUser},
		{"a row whose login is the system login", userRow(userA, SystemUserID, true), ErrSystemUser},
		{"a row of another login", userRow(userB, userB, true), nil},
		{"a row with an id that is not canonical", userRow(strings.ToUpper(userA), userA, true), nil},
		{"a database failure", errRow(boom), nil},
	} {
		db := &fakeDB{answer: func(int, string, []any) pgx.Row { return c.row }}
		got, err := resolverOver(t, db, "").ByAuthUser(context.Background(), userA)
		if err == nil {
			t.Errorf("%s: no error, got %+v", c.name, got)
			continue
		}
		if got != (Tenant{}) {
			t.Errorf("%s: returned the value %+v along with an error", c.name, got)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want errors.Is(%v)", c.name, err, c.want)
		}
		if c.want == nil {
			for _, e := range typed {
				if errors.Is(err, e) {
					t.Errorf("%s: a plain failure is reported as %v", c.name, e)
				}
			}
		}
		if c.name == "a database failure" && !errors.Is(err, boom) {
			t.Errorf("%s: the driver's error is not reachable with errors.Is: %v", c.name, err)
		}
		// each typed error is distinct from the others: a caller that tests one never matches another
		for _, e := range typed {
			if c.want != nil && e != c.want && errors.Is(err, e) {
				t.Errorf("%s: err matches %v as well as %v", c.name, e, c.want)
			}
		}
	}

	// the control: an active user resolves, and the query is the sanctioned one
	db := &fakeDB{answer: func(int, string, []any) pgx.Row { return userRow(userA, userA, true) }}
	got, err := resolverOver(t, db, "").ByAuthUser(context.Background(), userA)
	if err != nil || got != (Tenant{UserID: userA, AuthUserID: userA, Active: true}) {
		t.Fatalf("control: %+v, %v", got, err)
	}
	q := strings.ToLower(db.calls[0].sql)
	if !strings.Contains(q, "platform.listable_users") || strings.Contains(q, "public.users") || strings.Contains(q, "insert") || strings.Contains(q, "update") {
		t.Fatalf("the resolver's query is %q: it must read platform.listable_users, the one sanctioned list of users, and write nothing", db.calls[0].sql)
	}
	if _, err := (*Resolver)(nil).ByAuthUser(context.Background(), userA); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("a nil resolver: %v, want ErrNoDatabase", err)
	}
	if _, err := resolverOver(t, nil, "").ByAuthUser(context.Background(), userA); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("a resolver without a database: %v, want ErrNoDatabase", err)
	}
}

// ---- Scope ----

func TestScope_NeverTrustsTheTenantItIsGiven(t *testing.T) {
	secret := word("scope-", 40)
	dbOf := func(row pgx.Row) *fakeDB { return &fakeDB{answer: func(int, string, []any) pgx.Row { return row }} }
	genuine := Tenant{UserID: userA, AuthUserID: userA, Active: true}

	// the control first: the genuine tenant gets the platform user id for modules 2 and 3 and the derived key for module 1
	want, err := DeriveM1Key(config.Secret(secret), userA)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolverOver(t, dbOf(userRow(userA, userA, true)), secret).Scope(context.Background(), genuine)
	if err != nil || got.M2UserID != userA || got.M3UserID != userA || got.M1Key != want {
		t.Fatalf("control: Scope = {%s %s key-ok=%v}, %v; want both user ids = %s and the derived key", got.M2UserID, got.M3UserID, got.M1Key == want, err, userA)
	}

	for _, c := range []struct {
		name   string
		tenant Tenant
		row    pgx.Row
		want   error
		dbHits int // how many queries the refusal may have cost
	}{
		{"user B's id on user A's login (a forged tenant)", Tenant{UserID: userB, AuthUserID: userA, Active: true}, userRow(userA, userA, true), ErrTenantMismatch, 1},
		{"the user id in the other letter case", Tenant{UserID: strings.ToUpper(userA), AuthUserID: userA, Active: true}, userRow(userA, userA, true), ErrBadID, 0},
		{"an empty user id", Tenant{AuthUserID: userA, Active: true}, userRow(userA, userA, true), ErrBadID, 0},
		{"the system user as the user id", Tenant{UserID: SystemUserID, AuthUserID: userA, Active: true}, userRow(userA, userA, true), ErrSystemUser, 0},
		{"the system user as the login", Tenant{UserID: userA, AuthUserID: SystemUserID, Active: true}, userRow(userA, userA, true), ErrSystemUser, 0},
		{"a login that is not an id", Tenant{UserID: userA, AuthUserID: "x", Active: true}, userRow(userA, userA, true), ErrBadID, 0},
		{"a user switched off since the tenant was resolved", genuine, userRow(userA, userA, false), ErrInactive, 1},
		{"a user that no longer exists", genuine, errRow(pgx.ErrNoRows), ErrUnknownLogin, 1},
		{"a tenant that claims to be active when it is not (Active is not trusted)", Tenant{UserID: userA, AuthUserID: userA, Active: true}, userRow(userA, userA, false), ErrInactive, 1},
	} {
		db := dbOf(c.row)
		got, err := resolverOver(t, db, secret).Scope(context.Background(), c.tenant)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want errors.Is(%v)", c.name, err, c.want)
		}
		if got != (Scope{}) {
			t.Errorf("%s: returned the scope %+v along with an error", c.name, got)
		}
		if len(db.calls) != c.dbHits {
			t.Errorf("%s: %d database queries, want %d", c.name, len(db.calls), c.dbHits)
		}
	}

	// no secret: a tenant that is fine still gets no scope (there is no partial scope with an empty module 1 key)
	if got, err := resolverOver(t, dbOf(userRow(userA, userA, true)), "").Scope(context.Background(), genuine); !errors.Is(err, ErrNoM1KeySecret) || got != (Scope{}) {
		t.Errorf("without the secret: %+v, %v; want the zero scope and ErrNoM1KeySecret", got, err)
	}
	// ... but an inactive user is refused as inactive even then: the tenant is judged first
	if _, err := resolverOver(t, dbOf(userRow(userA, userA, false)), "").Scope(context.Background(), genuine); !errors.Is(err, ErrInactive) {
		t.Errorf("an inactive user without the secret: %v, want ErrInactive", err)
	}
}

func TestNewResolver_RefusesAShortSecretAndNeverEchoesIt(t *testing.T) {
	for _, n := range []int{1, 16, 31} {
		secret := word("Zq9-", n) // a word the error text cannot contain by accident
		r, err := NewResolver(nil, config.Secret(secret))
		if !errors.Is(err, ErrWeakSecret) || r != nil {
			t.Errorf("a %d-byte secret: %v, %v; want ErrWeakSecret and no resolver", n, r, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("a %d-byte secret is echoed: %v", n, err)
		}
	}
	for _, n := range []int{0, 32, 33, 200} { // 0 is "not configured": a resolver that cannot derive
		if r, err := NewResolver(nil, config.Secret(word("long-", n))); err != nil || r == nil {
			t.Errorf("a %d-byte secret was refused: %v", n, err)
		}
	}
	if config.MinM1KeySecretBytes != 32 {
		t.Errorf("config.MinM1KeySecretBytes = %d, want 32 (the brief and the documentation say 32)", config.MinM1KeySecretBytes)
	}
}

// ---- printing: the key and the secret are never in a printed value ----

func TestPrinting_NeverShowsTheSecretOrTheKey(t *testing.T) {
	secret := "CANARY-m1-" + word("p", 40)
	r := resolverOver(t, nil, secret)
	key, err := DeriveM1Key(config.Secret(secret), userA)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{M2UserID: userA, M3UserID: userA, M1Key: key}
	digest := fmt.Sprintf("%x", M1KeyDigest(key))
	needles := []string{secret, secret[len("CANARY-"):], key.Reveal(), digest}

	values := map[string]any{"resolver": r, "resolver value": *r, "scope": scope, "scope pointer": &scope, "struct holding both": struct {
		R Resolver
		S Scope
	}{*r, scope}}
	sinks := map[string]string{}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			sinks[name+" "+verb] = fmt.Sprintf(verb, v)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sinks[name+" json"] = string(b)
		for _, h := range []string{"json", "text"} {
			var buf bytes.Buffer
			var handler slog.Handler = slog.NewJSONHandler(&buf, nil)
			if h == "text" {
				handler = slog.NewTextHandler(&buf, nil)
			}
			slog.New(handler).Info("tenant", "value", v)
			sinks[name+" slog "+h] = buf.String()
		}
	}
	// the errors the package returns, each built from a call that carries the secret or the key
	_, e1 := DeriveM1Key(config.Secret(word("w", 31)), userA)
	_, e2 := NewResolver(nil, config.Secret(word("w", 31)))
	_, e3 := DeriveM1Key(key, "not-an-id")
	sinks["weak secret error"], sinks["weak secret error 2"], sinks["bad id error"] = fmt.Sprint(e1), fmt.Sprint(e2), fmt.Sprint(e3)
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		for _, n := range needles {
			if strings.Contains(out, n) {
				t.Errorf("%s reveals the secret, the key or its digest: %s", name, out)
			}
		}
		if strings.Contains(out, word("w", 31)) {
			t.Errorf("%s reveals the refused secret: %s", name, out)
		}
	}
	if got := fmt.Sprintf("%+v", scope); !strings.Contains(got, userA) || !strings.Contains(got, "[redacted]") {
		t.Errorf("a printed scope should show the user id and the redaction marker: %s", got)
	}
	// the control: the same value as plain strings leaks through the same sinks
	type naive struct{ Secret, Key string }
	n := naive{secret, key.Reveal()}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "value", n)
	if !strings.Contains(buf.String(), key.Reveal()) || !strings.Contains(fmt.Sprintf("%+v", n), secret) {
		t.Fatal("control failed: plain strings did not reach the sinks, so the checks above prove nothing")
	}
}
