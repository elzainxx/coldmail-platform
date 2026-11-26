package tenant

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/config"
)

// Phase 4 B16: M1CallerEnsurer against a recording stand-in for the database. What these tests can prove is the Go half of the door: which statement is sent and with what (the user and the digest of the
// derived key at the resolver's version), what is remembered and what is asked again, how the function's refusals map onto the package's typed errors, and that no error carries key material. What
// they cannot prove is the function itself (SECURITY DEFINER, its grants, its read-back): that is tests/harness/m1_provision_test.go, against the real module 1 table on the real image.

// ensureDB answers every call through answer(call index, args) and records it; safe for concurrent use.
type ensureDB struct {
	mu     sync.Mutex
	calls  []dbCall
	answer func(n int, args []any) (string, error)
}

func (d *ensureDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	d.mu.Lock()
	d.calls = append(d.calls, dbCall{sql, args})
	n := len(d.calls) - 1
	d.mu.Unlock()
	word, err := d.answer(n, args)
	return rowFunc(func(dest ...any) error {
		if err != nil {
			return err
		}
		if len(dest) != 1 {
			return fmt.Errorf("the ensurer scans %d columns, the function returns 1", len(dest))
		}
		*(dest[0].(*string)) = word
		return nil
	})
}

func (d *ensureDB) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func created(int, []any) (string, error) { return "created", nil }

// forbiddenEnsureDB fails the test if it is ever asked: a refusal decided before the database costs no call.
func forbiddenEnsureDB(t *testing.T) *ensureDB {
	return &ensureDB{answer: func(int, []any) (string, error) {
		t.Errorf("the database was asked by a call that must be refused before it")
		return "", errors.New("forbidden")
	}}
}

// uid is the n-th distinct canonical user id of a test.
func uid(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", n+1, n+1) }

func digestOf(t *testing.T, secret string, version int, user string) []byte {
	t.Helper()
	key, err := DeriveM1KeyVersion(config.Secret(secret), version, user)
	if err != nil {
		t.Fatal(err)
	}
	return M1KeyDigest(key)
}

func TestEnsureCaller_CallsTheFunctionWithTheUserAndTheDigestOfTheKeyDerivedAtTheResolversVersion(t *testing.T) {
	secret := word("ensure-args-", 44)
	ctx := context.Background()

	db := &ensureDB{answer: created}
	e := NewM1CallerEnsurer(resolverOver(t, nil, secret), db)
	if err := e.EnsureCaller(ctx, userA); err != nil {
		t.Fatal(err)
	}
	if db.count() != 1 {
		t.Fatalf("%d call(s), want 1", db.count())
	}
	c := db.calls[0]
	if !strings.Contains(c.sql, "platform.provision_m1_caller(") || !strings.Contains(c.sql, "$1::uuid") || !strings.Contains(c.sql, "$2::bytea") {
		t.Errorf("the statement does not call platform.provision_m1_caller($1::uuid, $2::bytea):\n%s", c.sql)
	}
	if strings.Contains(c.sql, "verify.") || strings.Contains(strings.ToUpper(c.sql), "INSERT") {
		t.Errorf("the ensurer's statement touches module 1's table itself; only the function may:\n%s", c.sql)
	}
	if len(c.args) != 2 || c.args[0] != userA {
		t.Fatalf("the arguments are %v, want (the user id, the digest)", c.args)
	}
	d1, ok := c.args[1].([]byte)
	if !ok || len(d1) != 32 || hex.EncodeToString(d1) != hex.EncodeToString(digestOf(t, secret, 1, userA)) {
		t.Errorf("the second argument is not the 32-byte digest of the version-1 key of user A")
	}

	// the version follows the resolver: version 2 is another key, so another digest, and it is the digest of the version-2 derivation
	r2, err := NewResolver(nil, config.Secret(secret), WithM1KeyVersion(2))
	if err != nil {
		t.Fatal(err)
	}
	db2 := &ensureDB{answer: created}
	if err := NewM1CallerEnsurer(r2, db2).EnsureCaller(ctx, userA); err != nil {
		t.Fatal(err)
	}
	d2, _ := db2.calls[0].args[1].([]byte)
	if hex.EncodeToString(d2) != hex.EncodeToString(digestOf(t, secret, 2, userA)) {
		t.Errorf("the version-2 resolver did not send the digest of the version-2 key")
	}
	if hex.EncodeToString(d1) == hex.EncodeToString(d2) {
		t.Fatal("control: the digests of version 1 and version 2 are equal, so the check above cannot tell a resolver's version from another")
	}
	// the digest follows the user too
	db3 := &ensureDB{answer: created}
	if err := NewM1CallerEnsurer(resolverOver(t, nil, secret), db3).EnsureCaller(ctx, userB); err != nil {
		t.Fatal(err)
	}
	if d3, _ := db3.calls[0].args[1].([]byte); hex.EncodeToString(d3) == hex.EncodeToString(d1) || db3.calls[0].args[0] != userB {
		t.Errorf("another user did not get another digest, or the user id was not passed")
	}
}

func TestEnsureCaller_RemembersWhatItEnsuredAndAsksAgainForAnythingElse(t *testing.T) {
	secret := word("ensure-memory-", 44)
	ctx := context.Background()
	r := resolverOver(t, nil, secret)
	answers := []string{"created", "exists", "exists", "exists", "exists", "exists"}
	db := &ensureDB{answer: func(n int, _ []any) (string, error) { return answers[n], nil }}
	e := NewM1CallerEnsurer(r, db)

	for i := 0; i < 3; i++ {
		if err := e.EnsureCaller(ctx, userA); err != nil {
			t.Fatal(err)
		}
	}
	if db.count() != 1 {
		t.Fatalf("three ensures of one user made %d database call(s), want 1", db.count())
	}
	// the controls: another user is another call, and the first user is still remembered
	if err := e.EnsureCaller(ctx, userB); err != nil || db.count() != 2 {
		t.Fatalf("a second user: %v, %d call(s), want success and 2", err, db.count())
	}
	if err := e.EnsureCaller(ctx, userA); err != nil || db.count() != 2 {
		t.Fatalf("the first user again: %v, %d call(s), want success and still 2", err, db.count())
	}
	// a different key version is a different key: a new call, with a different digest
	r.version = 2
	if err := e.EnsureCaller(ctx, userA); err != nil || db.count() != 3 {
		t.Fatalf("user A at another key version: %v, %d call(s), want success and 3", err, db.count())
	}
	if d, _ := db.calls[2].args[1].([]byte); hex.EncodeToString(d) != hex.EncodeToString(digestOf(t, secret, 2, userA)) {
		t.Errorf("the call after the version moved did not carry the version-2 digest")
	}
	// a restart: a second ensurer with an empty memory asks once more, the database says 'exists', and nothing else happens (the memory is an optimisation, not the truth)
	r.version = 0
	e2 := NewM1CallerEnsurer(r, db)
	if err := e2.EnsureCaller(ctx, userA); err != nil || db.count() != 4 {
		t.Fatalf("a fresh ensurer for a user already registered: %v, %d call(s), want success and 4", err, db.count())
	}
	if err := e2.EnsureCaller(ctx, userA); err != nil || db.count() != 4 {
		t.Fatalf("the fresh ensurer's second call: %v, %d call(s), want success and still 4", err, db.count())
	}

	// only a success is remembered: a failure, a conflict and an unknown answer are asked again
	for _, c := range []struct {
		name string
		err  error
		word string
	}{
		{"a connection failure", errors.New("connection reset by peer"), ""},
		{"a conflict", &pgconn.PgError{Code: "23505", ConstraintName: m1pgRowNotTheUsers}, ""},
		{"an unknown answer", nil, "maybe"},
	} {
		fdb := &ensureDB{answer: func(int, []any) (string, error) { return c.word, c.err }}
		fe := NewM1CallerEnsurer(r, fdb)
		for i := 0; i < 2; i++ {
			if err := fe.EnsureCaller(ctx, userA); err == nil {
				t.Errorf("%s: call %d succeeded", c.name, i+1)
			}
		}
		if fdb.count() != 2 {
			t.Errorf("%s: %d database call(s) for two ensures, want 2: a failure must not be remembered as a success", c.name, fdb.count())
		}
	}
}

func TestEnsureCaller_TheMemoryIsBoundedAndForgetsTheOldestFirst(t *testing.T) {
	secret := word("ensure-bound-", 44)
	ctx := context.Background()
	r := resolverOver(t, nil, secret)
	db := &ensureDB{answer: created}
	e := NewM1CallerEnsurer(r, db, WithEnsureCacheEntries(3))
	for i := 0; i < 50; i++ {
		if err := e.EnsureCaller(ctx, uid(i)); err != nil {
			t.Fatal(err)
		}
		if n := e.cache.len(); n > 3 {
			t.Fatalf("after %d distinct users the memory holds %d entries, want at most 3", i+1, n)
		}
	}
	if db.count() != 50 || e.cache.len() != 3 {
		t.Fatalf("50 distinct users: %d call(s) and %d remembered, want 50 and 3", db.count(), e.cache.len())
	}
	// the three newest are remembered, the oldest is not
	if err := e.EnsureCaller(ctx, uid(49)); err != nil || db.count() != 50 {
		t.Fatalf("the newest user again: %v, %d call(s), want success and still 50", err, db.count())
	}
	if err := e.EnsureCaller(ctx, uid(0)); err != nil || db.count() != 51 {
		t.Fatalf("the oldest user again: %v, %d call(s), want success and 51 (it was evicted)", err, db.count())
	}
	// the controls: a memory of size 0 stores nothing (every call asks), and the default is a real, positive bound
	off := NewM1CallerEnsurer(r, db, WithEnsureCacheEntries(0))
	before := db.count()
	for i := 0; i < 3; i++ {
		if err := off.EnsureCaller(ctx, userA); err != nil {
			t.Fatal(err)
		}
	}
	if db.count() != before+3 || off.cache.len() != 0 {
		t.Errorf("a memory of size 0: %d call(s) for 3 ensures, %d remembered, want 3 and 0", db.count()-before, off.cache.len())
	}
	if def := NewM1CallerEnsurer(r, db); def.cache.max != DefaultM1EnsureCacheEntries || DefaultM1EnsureCacheEntries < 1 {
		t.Errorf("the default memory holds %d entries (the constant says %d), want a positive bound", def.cache.max, DefaultM1EnsureCacheEntries)
	}
}

func TestEnsureCaller_MapsTheFunctionsRefusalsToTheTypedErrors(t *testing.T) {
	secret := word("ensure-map-", 44)
	typed := []error{ErrBadID, ErrSystemUser, ErrM1CallerConflict, ErrNoM1KeySecret, ErrNoDatabase, ErrWeakSecret}
	boom := errors.New("connection reset by peer")
	for _, c := range []struct {
		name    string
		answer  func(int, []any) (string, error)
		want    error  // the one typed error that must match; nil: none of them
		wantErr bool   // false: success
		text    string // a fragment the error text must contain
	}{
		{"created (the control: success)", func(int, []any) (string, error) { return "created", nil }, nil, false, ""},
		{"exists (the control: the idempotent second call)", func(int, []any) (string, error) { return "exists", nil }, nil, false, ""},
		{"a row that is not this user's", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "23505", ConstraintName: "m1_caller_row_acts_for_the_user", Message: "provision_m1_caller: a module 1 caller row ... does not act for that user"}
		}, ErrM1CallerConflict, true, ""},
		{"a NULL user", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "22004", ConstraintName: "m1_caller_user_required"}
		}, ErrBadID, true, ""},
		{"the system user", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "22023", ConstraintName: "m1_caller_not_the_system_user"}
		}, ErrSystemUser, true, ""},
		{"a digest that is not 32 bytes: a defect of the platform, no typed error of the user's", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "22023", ConstraintName: "m1_caller_digest_is_32_bytes"}
		}, nil, true, "not 32 bytes"},
		{"the row is gone after the insert", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "55000", ConstraintName: "m1_caller_row_after_insert"}
		}, nil, true, "not there after the insert"},
		{"the same SQLSTATE (23505) of ANOTHER constraint is not a conflict: the mapping reads the function's own name", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "23505", ConstraintName: "api_callers_pkey"}
		}, nil, true, "23505"},
		{"the same SQLSTATE (22023) of ANOTHER constraint is not the system user", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "22023", ConstraintName: "something_else"}
		}, nil, true, "22023"},
		{"the function is not installed", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "42883", Message: "function platform.provision_m1_caller(uuid, bytea) does not exist"}
		}, nil, true, "db/install/postmodule1.sql"},
		{"platform_app may not execute it", func(int, []any) (string, error) {
			return "", &pgconn.PgError{Code: "42501", Message: "permission denied for function provision_m1_caller"}
		}, nil, true, "db/install/postmodule1.sql"},
		{"a failed connection", func(int, []any) (string, error) { return "", boom }, nil, true, "connection reset by peer"},
		{"a cancelled context", func(int, []any) (string, error) { return "", context.Canceled }, nil, true, "context canceled"},
		{"an answer that is neither word", func(int, []any) (string, error) { return "updated", nil }, nil, true, "neither"},
	} {
		db := &ensureDB{answer: c.answer}
		e := NewM1CallerEnsurer(resolverOver(t, nil, secret), db)
		err := e.EnsureCaller(context.Background(), userA)
		if !c.wantErr {
			if err != nil {
				t.Errorf("%s: %v, want success", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		for _, want := range typed {
			if got := errors.Is(err, want); got != (want == c.want) {
				t.Errorf("%s: errors.Is(err, %v) = %v, want %v (err: %v)", c.name, want, got, want == c.want, err)
			}
		}
		if c.text != "" && !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: the error text %q lacks %q", c.name, err.Error(), c.text)
		}
		if c.name == "a failed connection" && !errors.Is(err, boom) {
			t.Errorf("%s: the driver's error is not reachable with errors.Is", c.name)
		}
		if c.name == "a cancelled context" && !errors.Is(err, context.Canceled) {
			t.Errorf("%s: context.Canceled is not reachable with errors.Is", c.name)
		}
		var pe *pgconn.PgError
		if strings.HasPrefix(c.name, "the same SQLSTATE") && !errors.As(err, &pe) {
			t.Errorf("%s: the driver's PgError is not reachable with errors.As", c.name)
		}
	}
}

func TestEnsureCaller_RefusesBeforeTheDatabaseWhatTheFunctionWouldRefuse(t *testing.T) {
	secret := word("ensure-refuse-", 44)
	ctx := context.Background()
	r := resolverOver(t, nil, secret)
	e := NewM1CallerEnsurer(r, forbiddenEnsureDB(t))
	for _, c := range []struct {
		name, id string
		want     error
	}{
		{"an empty id", "", ErrBadID},
		{"a blank id", "   ", ErrBadID},
		{"a word", "not-an-id", ErrBadID},
		{"the NULL literal", "NULL", ErrBadID},
		{"upper case", strings.ToUpper(userA), ErrBadID},
		{"braces", "{" + userA + "}", ErrBadID},
		{"the system user", SystemUserID, ErrSystemUser},
	} {
		if err := e.EnsureCaller(ctx, c.id); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want errors.Is(%v)", c.name, err, c.want)
		}
	}
	// no secret, a secret too short to derive from, no resolver, no ensurer: refused before any call, and the id is judged before the secret (as ProvisionM1Caller does)
	if err := NewM1CallerEnsurer(resolverOver(t, nil, ""), forbiddenEnsureDB(t)).EnsureCaller(ctx, userA); !errors.Is(err, ErrNoM1KeySecret) {
		t.Errorf("no secret: %v, want ErrNoM1KeySecret", err)
	}
	weak := &Resolver{secret: config.Secret(strings.Repeat("s", 5))}
	if err := NewM1CallerEnsurer(weak, forbiddenEnsureDB(t)).EnsureCaller(ctx, userA); !errors.Is(err, ErrWeakSecret) {
		t.Errorf("a weak secret: %v, want ErrWeakSecret", err)
	}
	if err := NewM1CallerEnsurer(nil, forbiddenEnsureDB(t)).EnsureCaller(ctx, userA); !errors.Is(err, ErrNoM1KeySecret) {
		t.Errorf("no resolver: %v, want ErrNoM1KeySecret", err)
	}
	if err := (*M1CallerEnsurer)(nil).EnsureCaller(ctx, userA); !errors.Is(err, ErrNoM1KeySecret) {
		t.Errorf("no ensurer: %v, want ErrNoM1KeySecret", err)
	}
	if err := NewM1CallerEnsurer(nil, forbiddenEnsureDB(t)).EnsureCaller(ctx, ""); !errors.Is(err, ErrBadID) {
		t.Errorf("an empty id with no resolver: %v, want ErrBadID (the id is judged first)", err)
	}
	if err := NewM1CallerEnsurer(r, nil).EnsureCaller(ctx, userA); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("no database handle: %v, want ErrNoDatabase", err)
	}
	// the control: the same ensurer with a working handle asks exactly once for a good id
	db := &ensureDB{answer: created}
	if err := NewM1CallerEnsurer(r, db).EnsureCaller(ctx, userA); err != nil || db.count() != 1 {
		t.Fatalf("control: %v, %d call(s), want success and 1", err, db.count())
	}
}

func TestEnsureCaller_NoErrorCarriesTheSecretTheKeyOrTheDigest(t *testing.T) {
	secret := word("ensure-leak-", 44)
	r := resolverOver(t, nil, secret)
	key, err := DeriveM1Key(config.Secret(secret), userA)
	if err != nil {
		t.Fatal(err)
	}
	digest := M1KeyDigest(key)
	material := map[string]string{
		"the secret": secret, "the key": key.Reveal(), "the digest in hex": hex.EncodeToString(digest), "the digest in HEX": strings.ToUpper(hex.EncodeToString(digest)),
		"the digest in base64": base64.StdEncoding.EncodeToString(digest), "the digest in base64url": base64.RawURLEncoding.EncodeToString(digest), "the digest as Go prints a slice": fmt.Sprint(digest),
	}
	leaky := func(what string) func(int, []any) (string, error) {
		return func(_ int, args []any) (string, error) {
			// a driver that put the bound parameters, the key and the secret into its message (none does; this is the wall for the day one does)
			d := args[1].([]byte)
			msg := fmt.Sprintf("%s: user %v digest %x %X %v %s %s secret %s key %s", what, args[0], d, d, d, base64.StdEncoding.EncodeToString(d), base64.RawURLEncoding.EncodeToString(d), secret, key.Reveal())
			if what == "pg" {
				return "", &pgconn.PgError{Code: "XX000", Message: msg}
			}
			return "", errors.New(msg)
		}
	}
	for _, what := range []string{"plain", "pg"} {
		db := &ensureDB{answer: leaky(what)}
		// the premise, so that the check can fail: the raw error really does carry all of it
		_, rawErr := db.answer(0, []any{userA, digest})
		for name, s := range material {
			if !strings.Contains(rawErr.Error(), s) {
				t.Fatalf("premise (%s): the raw driver error does not contain %s, so the assertion below could not fail", what, name)
			}
		}
		err := NewM1CallerEnsurer(r, db).EnsureCaller(context.Background(), userA)
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		for name, s := range material {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: the error carries %s: %v", what, name, err)
			}
		}
		if !strings.Contains(err.Error(), userA) {
			t.Errorf("%s: the error does not name the user (it should: a user id is not a secret): %v", what, err)
		}
		var pe *pgconn.PgError
		if what == "pg" && !errors.As(err, &pe) {
			t.Errorf("pg: the driver's PgError is no longer reachable with errors.As")
		}
	}
	// the typed refusals and the install hints never carry it either
	for _, pe := range []*pgconn.PgError{
		{Code: "23505", ConstraintName: m1pgRowNotTheUsers, Message: "x " + material["the digest in hex"]},
		{Code: "42883", Message: "x " + material["the key"]},
		{Code: "42501", Message: "x " + material["the secret"]},
	} {
		err := NewM1CallerEnsurer(r, &ensureDB{answer: func(int, []any) (string, error) { return "", pe }}).EnsureCaller(context.Background(), userA)
		for name, s := range material {
			if err == nil || strings.Contains(err.Error(), s) {
				t.Errorf("SQLSTATE %s: the error (%v) is nil or carries %s", pe.Code, err, name)
			}
		}
	}
}

func TestM1CallerEnsurer_ExposesExactlyWhatTheEngineTakes(t *testing.T) {
	// the interface jobengine.M1Provisioner declares (it is defined there; this package does not import the engine)
	var _ interface {
		EnsureCaller(ctx context.Context, userID string) error
	} = (*M1CallerEnsurer)(nil)
	typ := reflect.TypeOf((*M1CallerEnsurer)(nil))
	if typ.NumMethod() != 1 || typ.Method(0).Name != "EnsureCaller" {
		var names []string
		for i := 0; i < typ.NumMethod(); i++ {
			names = append(names, typ.Method(i).Name)
		}
		t.Errorf("the exported method set of *M1CallerEnsurer is %v, want exactly [EnsureCaller]", names)
	}
	// the control: the same reflection sees the several methods of a type that has them, so a count of 1 above means something
	if n := reflect.TypeOf((*Resolver)(nil)).NumMethod(); n < 4 {
		t.Errorf("control: *Resolver reports %d exported methods, the reflection cannot be trusted", n)
	}
}

func TestEnsureCaller_ConcurrentCallsAllSucceedAndTheMemoryHoldsOneEntry(t *testing.T) {
	secret := word("ensure-race-", 44)
	db := &ensureDB{answer: func(n int, _ []any) (string, error) {
		if n == 0 {
			return "created", nil
		}
		return "exists", nil
	}}
	e := NewM1CallerEnsurer(resolverOver(t, nil, secret), db)
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.EnsureCaller(context.Background(), userA)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent EnsureCaller failed: %v", err)
		}
	}
	if n := db.count(); n < 1 || n > workers {
		t.Errorf("%d database call(s) for %d concurrent ensures of one user, want between 1 and %d", n, workers, workers)
	}
	if e.cache.len() != 1 {
		t.Errorf("the memory holds %d entries, want 1", e.cache.len())
	}
	before := db.count()
	if err := e.EnsureCaller(context.Background(), userA); err != nil || db.count() != before {
		t.Errorf("after the burst: %v, %d new call(s), want success and none", err, db.count()-before)
	}
}
