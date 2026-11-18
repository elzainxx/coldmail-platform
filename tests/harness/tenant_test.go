//go:build harness

package harness

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 3 B2: internal/tenant against the real database. Two families:
//
//   - TestTenant_*  runs on a migrated scratch database with a REAL pool of platform_app logins (the identity the service runs as): committed rows, a user per case, the resolver's answers
//     compared with what the tables hold. They need no module, so the db-tests workflow runs them too.
//   - TestTenantLive_* and TestM1LiveTenant_*  run in a rolled-back transaction on the LIVE database, where the full harness has module 1 migrated: the auth.users table is the image's own
//     (run.sh checks it has GoTrue's encrypted_password column), verify.api_callers is module 1's own table with module 1's own grants, and the roles are the real ones. TestM1Live* is
//     skipped by the db-tests workflow (no module 1 there) and counted by the full harness's floor.
//
// What a runner proves and a reader cannot: that the statements below execute on the supabase image (the role switches bite, the real auth.users accepts the rows, verify_app and platform_app are
// refused by privilege) and that module 1's lookup finds the derived key. That module 1 ACCEPTS the key over HTTP is chunk B9, not this one.

const (
	tenUserA    = "a7a7a7a7-0000-4000-8000-0000000000a1" // a login with an active platform user
	tenUserB    = "b8b8b8b8-0000-4000-8000-0000000000b2" // a second one
	tenInactive = "c9c9c9c9-0000-4000-8000-0000000000c3" // a login whose platform user is switched off
	tenAuthOnly = "d1d1d1d1-0000-4000-8000-0000000000d4" // a login in auth.users that no admin has made a platform user of
	tenNobody   = "e2e2e2e2-0000-4000-8000-0000000000e5" // an id that no table holds
)

// tenKey and tenDigest are the derivation written out a second time, in the test, from the specification (internal/tenant/m1key.go header), so that a wrong derivation in the package cannot
// hide behind a test that calls the same code.
func tenKey(secret config.Secret, user string) string { return tenKeyV(secret, 1, user) }

// tenKeyV is the derivation at an explicit key version: the HMAC-SHA256 of "m1-caller|v<version>|<user>" (Phase 3 close-out A4).
func tenKeyV(secret config.Secret, version int, user string) string {
	mac := hmac.New(sha256.New, []byte(secret.Reveal()))
	mac.Write([]byte("m1-caller|v" + strconv.Itoa(version) + "|" + user))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func tenDigest(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func tenSeedLogin(t *testing.T, db mnyDB, id, email string) {
	t.Helper()
	if _, err := db.Exec(testCtx(t), "INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, email); err != nil {
		t.Fatalf("seed the login %s: %v", id, err)
	}
}

func tenSeedUser(t *testing.T, db mnyDB, id, email string, active bool) {
	t.Helper()
	if _, err := db.Exec(testCtx(t), "INSERT INTO public.users (id, auth_user_id, email, is_active) VALUES ($1, $1, $2, $3)", id, email, active); err != nil {
		t.Fatalf("seed the platform user %s: %v", id, err)
	}
}

func tenCount(t *testing.T, db mnyDB, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(testCtx(t), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	return n
}

// tenIs asserts err matches want and none of the other typed refusals, and that the Tenant is the zero value.
func tenIs(t *testing.T, what string, got tenant.Tenant, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want errors.Is(%v)", what, err, want)
	}
	for _, other := range []error{tenant.ErrUnknownLogin, tenant.ErrInactive, tenant.ErrSystemUser, tenant.ErrBadID} {
		if other != want && errors.Is(err, other) {
			t.Errorf("%s: err matches %v as well as %v", what, other, want)
		}
	}
	if got != (tenant.Tenant{}) {
		t.Errorf("%s: a value %+v came back with the error", what, got)
	}
}

// ---- the scratch environment: committed rows, a real platform_app pool ----

type tenEnv struct {
	t      *testing.T
	s      scratch
	admin  *pgx.Conn
	pool   *pgxpool.Pool
	secret config.Secret
	res    *tenant.Resolver
}

// newTenEnv migrates a scratch database with the real binary, opens the resolver over a pool of REAL platform_app logins, and seeds: logins and platform users A and B (active), an inactive user,
// and a login with no platform user (tenAuthOnly). The module 1 key secret is minted for this test and never printed.
func newTenEnv(t *testing.T) *tenEnv {
	t.Helper()
	s, admin := migratedScratch(t)
	e := &tenEnv{t: t, s: s, admin: admin, pool: mnyPool(t, s.DSN), secret: config.Secret(randomHex(t, 32))}
	var err error
	if e.res, err = tenant.NewResolver(e.pool, e.secret); err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	tenSeedLogin(t, admin, tenUserA, "ten-a@example.test")
	tenSeedLogin(t, admin, tenUserB, "ten-b@example.test")
	tenSeedLogin(t, admin, tenInactive, "ten-inactive@example.test")
	tenSeedLogin(t, admin, tenAuthOnly, "ten-authonly@example.test")
	tenSeedUser(t, admin, tenUserA, "ten-a@example.test", true)
	tenSeedUser(t, admin, tenUserB, "ten-b@example.test", true)
	tenSeedUser(t, admin, tenInactive, "ten-inactive@example.test", false)
	return e
}

// counts is everything a resolver could create: the two user tables.
func (e *tenEnv) counts() string {
	e.t.Helper()
	return fmt.Sprintf("public.users=%d auth.users=%d", tenCount(e.t, e.admin, "SELECT count(*) FROM public.users"), tenCount(e.t, e.admin, "SELECT count(*) FROM auth.users"))
}

func (e *tenEnv) setActive(id string, active bool) {
	e.t.Helper()
	if _, err := e.admin.Exec(testCtx(e.t), "UPDATE public.users SET is_active = $2 WHERE id = $1", id, active); err != nil {
		e.t.Fatalf("set is_active=%v on %s: %v", active, id, err)
	}
}

func (e *tenEnv) scope(res *tenant.Resolver, login string) tenant.Scope {
	e.t.Helper()
	tn, err := res.ByAuthUser(testCtx(e.t), login)
	if err != nil {
		e.t.Fatalf("ByAuthUser(%s): %v", login, err)
	}
	sc, err := res.Scope(testCtx(e.t), tn)
	if err != nil {
		e.t.Fatalf("Scope(%s): %v", login, err)
	}
	return sc
}

// TestTenant_AKnownLoginResolvesToItsPlatformUser: each login resolves to ITS platform user (the id of the public.users row that links to it), in either letter case, and reading writes nothing.
// The paired controls are the second login (a resolver that returned the first row would give both the same user) and the unchanged table counts.
func TestTenant_AKnownLoginResolvesToItsPlatformUser(t *testing.T) {
	e := newTenEnv(t)
	ctx := testCtx(t)
	before := e.counts()
	seen := map[string]bool{}
	for _, id := range []string{tenUserA, tenUserB} {
		got, err := e.res.ByAuthUser(ctx, id)
		if want := (tenant.Tenant{UserID: id, AuthUserID: id, Active: true}); err != nil || got != want {
			t.Fatalf("ByAuthUser(%s) = %+v, %v; want %+v", id, got, err, want)
		}
		var stored string
		if err := e.admin.QueryRow(ctx, "SELECT id::text FROM public.users WHERE auth_user_id = $1", id).Scan(&stored); err != nil || stored != got.UserID {
			t.Fatalf("the platform user of login %s is %q in public.users (err %v), the resolver said %q", id, stored, err, got.UserID)
		}
		seen[got.UserID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("two logins resolved to %d distinct platform users, want 2", len(seen))
	}
	up, err := e.res.ByAuthUser(ctx, strings.ToUpper(tenUserA))
	if err != nil || up.UserID != tenUserA {
		t.Fatalf("the upper-case spelling of login A = %+v, %v; want the same user %s", up, err, tenUserA)
	}
	if after := e.counts(); after != before {
		t.Fatalf("resolving logins changed the user tables: %s -> %s", before, after)
	}
}

// TestTenant_AnUnknownLoginIsRefusedAndNothingIsCreated: a login that has no platform user (it exists in auth.users, or it exists nowhere) is ErrUnknownLogin, and neither user table gets a
// row: users are created by an admin, never by a login. The paired control is the same call after an admin creates the platform user: it resolves, so the refusal was about the missing user
// and not about the id, the pool or the view.
func TestTenant_AnUnknownLoginIsRefusedAndNothingIsCreated(t *testing.T) {
	e := newTenEnv(t)
	ctx := testCtx(t)
	before := e.counts()
	usersBefore := tenCount(t, e.admin, "SELECT count(*) FROM public.users")
	for _, c := range []struct{ what, id string }{
		{"a login that exists in auth.users and has no platform user", tenAuthOnly},
		{"an id that exists in no table", tenNobody},
	} {
		for i := 0; i < 2; i++ { // twice: the first refusal must not have created what the second would find
			got, err := e.res.ByAuthUser(ctx, c.id)
			tenIs(t, fmt.Sprintf("%s (call %d)", c.what, i+1), got, err, tenant.ErrUnknownLogin)
		}
	}
	if after := e.counts(); after != before {
		t.Fatalf("a refused login changed the user tables: %s -> %s", before, after)
	}

	// the control: the admin creates the platform user of tenAuthOnly (the superuser here; in production the admin's create-user path)
	tenSeedUser(t, e.admin, tenAuthOnly, "ten-authonly@example.test", true)
	got, err := e.res.ByAuthUser(ctx, tenAuthOnly)
	if want := (tenant.Tenant{UserID: tenAuthOnly, AuthUserID: tenAuthOnly, Active: true}); err != nil || got != want {
		t.Fatalf("control: the same login after its platform user was created = %+v, %v; want %+v", got, err, want)
	}
	if n := tenCount(t, e.admin, "SELECT count(*) FROM public.users"); n != usersBefore+1 {
		t.Fatalf("control: public.users holds %d rows after the admin created one user, want %d", n, usersBefore+1)
	}
}

// TestTenant_AnInactiveUserIsRefusedAndTheAnswerIsNeverCached: an inactive platform user is ErrInactive (not "unknown": the row exists), and the answer follows the database on every call. The paired
// control is the same login once the admin switches the user on (it resolves) and off again (refused again).
func TestTenant_AnInactiveUserIsRefusedAndTheAnswerIsNeverCached(t *testing.T) {
	e := newTenEnv(t)
	ctx := testCtx(t)
	got, err := e.res.ByAuthUser(ctx, tenInactive)
	tenIs(t, "an inactive user", got, err, tenant.ErrInactive)

	e.setActive(tenInactive, true)
	got, err = e.res.ByAuthUser(ctx, tenInactive)
	if want := (tenant.Tenant{UserID: tenInactive, AuthUserID: tenInactive, Active: true}); err != nil || got != want {
		t.Fatalf("control: the user switched on = %+v, %v; want %+v", got, err, want)
	}
	e.setActive(tenInactive, false)
	got, err = e.res.ByAuthUser(ctx, tenInactive)
	tenIs(t, "the user switched off again (the same resolver)", got, err, tenant.ErrInactive)

	// and the active users are untouched by all of it
	if _, err := e.res.ByAuthUser(ctx, tenUserA); err != nil {
		t.Fatalf("control: an active user is refused after another user's switches: %v", err)
	}
}

// TestTenant_TheSystemUserIsNeverALogin: the all-zero user has a row (module 2 needs it), is inactive, has no login and is not in the sanctioned list; ByAuthUser refuses its id with
// ErrSystemUser BEFORE asking the database, even in the worst case where a login with that very id exists in auth.users; Scope refuses a Tenant that names it. The paired control is a real
// login resolving on the same resolver.
func TestTenant_TheSystemUserIsNeverALogin(t *testing.T) {
	e := newTenEnv(t)
	ctx := testCtx(t)
	// the premises, asserted: the row exists, is inactive, has no auth link, and the view does not list it
	var active bool
	var link *string
	if err := e.admin.QueryRow(ctx, "SELECT is_active, auth_user_id::text FROM public.users WHERE id = $1", tenant.SystemUserID).Scan(&active, &link); err != nil || active || link != nil {
		t.Fatalf("precondition: the system user row is (active=%v, auth_user_id=%v, err %v), want an inactive row with no login", active, link, err)
	}
	if n := tenCount(t, e.admin, "SELECT count(*) FROM platform.listable_users WHERE id = $1", tenant.SystemUserID); n != 0 {
		t.Fatalf("precondition: platform.listable_users lists the system user (%d row)", n)
	}
	usersBefore := tenCount(t, e.admin, "SELECT count(*) FROM public.users")

	got, err := e.res.ByAuthUser(ctx, tenant.SystemUserID)
	tenIs(t, "the system user's id as a login", got, err, tenant.ErrSystemUser)

	// the worst case: the login with the all-zero id EXISTS in auth.users. It is still refused as the system user (not "unknown"), because the id is judged before any lookup.
	tenSeedLogin(t, e.admin, tenant.SystemUserID, "ten-zero@example.test")
	got, err = e.res.ByAuthUser(ctx, tenant.SystemUserID)
	tenIs(t, "the system user's id as a login that exists in auth.users", got, err, tenant.ErrSystemUser)
	if n := tenCount(t, e.admin, "SELECT count(*) FROM public.users"); n != usersBefore {
		t.Fatalf("a refused system login changed public.users: %d -> %d rows", usersBefore, n)
	}

	sc, err := e.res.Scope(ctx, tenant.Tenant{UserID: tenant.SystemUserID, AuthUserID: tenUserA, Active: true})
	if !errors.Is(err, tenant.ErrSystemUser) || sc != (tenant.Scope{}) {
		t.Fatalf("Scope of a Tenant that names the system user = %+v, %v; want the zero scope and ErrSystemUser", sc, err)
	}

	if _, err := e.res.ByAuthUser(ctx, tenUserA); err != nil {
		t.Fatalf("control: a real login on the same resolver: %v", err)
	}
}

// TestTenant_MalformedIDsAreRefusedBeforePostgresSeesThem: every value that is not the hyphenated uuid is ErrBadID, not a database error (SQLSTATE 22P02), and the forms Postgres WOULD accept as a
// uuid (braces, no hyphens, a urn) are refused too. The paired controls: Postgres really does accept those forms (so the refusal is the platform's rule, and a lookup that reached the database
// would have found the user), and a well-formed id resolves. The forms that Postgres reads as the user's id are (braces, no hyphens, a hyphen after every group of four digits).
func TestTenant_MalformedIDsAreRefusedBeforePostgresSeesThem(t *testing.T) {
	e := newTenEnv(t)
	ctx := testCtx(t)
	bare := strings.ReplaceAll(tenUserA, "-", "")
	var groups []string
	for i := 0; i < len(bare); i += 4 {
		groups = append(groups, bare[i:i+4])
	}
	pgAccepts := []string{"{" + tenUserA + "}", bare, strings.Join(groups, "-")} // braces, no hyphens, a hyphen after every group of four digits
	for _, form := range pgAccepts {
		var back string
		if err := e.admin.QueryRow(ctx, "SELECT $1::uuid::text", form).Scan(&back); err != nil || back != tenUserA {
			t.Fatalf("control: Postgres reads %q as %q (err %v), want the user's id: the form must be one Postgres accepts", form, back, err)
		}
	}
	bad := append([]string{"", " ", "not-a-uuid", tenUserA + "\n", " " + tenUserA, tenUserA[:35], tenUserA + "0", "urn:uuid:" + tenUserA, "'; DROP TABLE auth.users; --", strings.Repeat("a", 36)}, pgAccepts...)
	for _, id := range bad {
		got, err := e.res.ByAuthUser(ctx, id)
		tenIs(t, fmt.Sprintf("the id %q", id), got, err, tenant.ErrBadID)
		if code := sqlState(err); code != "" {
			t.Errorf("the id %q reached the database and failed there (SQLSTATE %s): %v", id, code, err)
		}
	}
	if len(bad) < 12 {
		t.Fatal("the list of malformed ids shrank")
	}
	if got, err := e.res.ByAuthUser(ctx, tenUserA); err != nil || got.UserID != tenUserA {
		t.Fatalf("control: a well-formed id = %+v, %v", got, err)
	}
}

// TestTenant_ScopeRereadsTheDatabaseAndNeverTrustsAHandWrittenTenant: a Tenant is a plain struct, so Scope must not believe it. A UserID that is not the login's platform user is ErrTenantMismatch
// (in both directions), a login that has since been removed or switched off is refused with its own error, and no module 1 key is derived when the secret is not configured. The paired
// controls are the genuine Tenant (a scope), the same Tenant after the user is switched back on (a scope again), and a resolver with the secret.
func TestTenant_ScopeRereadsTheDatabaseAndNeverTrustsAHandWrittenTenant(t *testing.T) {
	e := newTenEnv(t)
	ctx := testCtx(t)
	genuineA, err := e.res.ByAuthUser(ctx, tenUserA)
	if err != nil {
		t.Fatal(err)
	}
	genuineB, err := e.res.ByAuthUser(ctx, tenUserB)
	if err != nil {
		t.Fatal(err)
	}
	if sc, err := e.res.Scope(ctx, genuineA); err != nil || sc.M2UserID != tenUserA || sc.M3UserID != tenUserA || sc.M1Key.Reveal() == "" {
		t.Fatalf("control: the genuine tenant's scope = %+v, %v", sc, err)
	}

	for _, c := range []struct {
		what   string
		tenant tenant.Tenant
		want   error
	}{
		{"user B's id on user A's login", tenant.Tenant{UserID: tenUserB, AuthUserID: tenUserA, Active: true}, tenant.ErrTenantMismatch},
		{"user A's id on user B's login", tenant.Tenant{UserID: tenUserA, AuthUserID: tenUserB, Active: true}, tenant.ErrTenantMismatch},
		{"an id that exists nowhere", tenant.Tenant{UserID: tenNobody, AuthUserID: tenNobody, Active: true}, tenant.ErrUnknownLogin},
		{"a login with no platform user", tenant.Tenant{UserID: tenAuthOnly, AuthUserID: tenAuthOnly, Active: true}, tenant.ErrUnknownLogin},
		{"an inactive user whose Tenant claims to be active", tenant.Tenant{UserID: tenInactive, AuthUserID: tenInactive, Active: true}, tenant.ErrInactive},
	} {
		sc, err := e.res.Scope(ctx, c.tenant)
		if !errors.Is(err, c.want) || sc != (tenant.Scope{}) {
			t.Errorf("%s: Scope = %+v, %v; want the zero scope and errors.Is(%v)", c.what, sc, err, c.want)
		}
	}

	// a Tenant resolved while the user was active, then the user is switched off: Scope refuses it; switched back on, the same value works again
	e.setActive(tenUserB, false)
	if sc, err := e.res.Scope(ctx, genuineB); !errors.Is(err, tenant.ErrInactive) || sc != (tenant.Scope{}) {
		t.Errorf("a tenant whose user was switched off since: Scope = %+v, %v; want the zero scope and ErrInactive", sc, err)
	}
	e.setActive(tenUserB, true)
	if sc, err := e.res.Scope(ctx, genuineB); err != nil || sc.M2UserID != tenUserB {
		t.Errorf("control: the same tenant after the user is switched back on: %+v, %v", sc, err)
	}

	// no secret: a tenant that is fine still gets no scope, and no scope has an empty module 1 key
	noSecret, err := tenant.NewResolver(e.pool, "")
	if err != nil {
		t.Fatal(err)
	}
	if sc, err := noSecret.Scope(ctx, genuineA); !errors.Is(err, tenant.ErrNoM1KeySecret) || sc != (tenant.Scope{}) {
		t.Errorf("a resolver without the module 1 key secret: Scope = %+v, %v; want the zero scope and ErrNoM1KeySecret", sc, err)
	}
	if _, err := noSecret.ByAuthUser(ctx, tenUserA); err != nil {
		t.Errorf("control: the same resolver still resolves logins (only the key needs the secret): %v", err)
	}
}

// TestTenant_ModulesTwoAndThreeGetThePlatformUsersOwnID: M2UserID and M3UserID are the id of the login's public.users row, read from the table, and they differ between users. The paired control is
// the second user (a scope that always returned one fixed id would fail it).
func TestTenant_ModulesTwoAndThreeGetThePlatformUsersOwnID(t *testing.T) {
	e := newTenEnv(t)
	got := map[string]string{}
	for _, login := range []string{tenUserA, tenUserB} {
		var stored string
		if err := e.admin.QueryRow(testCtx(t), "SELECT id::text FROM public.users WHERE auth_user_id = $1", login).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		sc := e.scope(e.res, login)
		if sc.M2UserID != stored || sc.M3UserID != stored {
			t.Errorf("login %s: module 2 user_id %q, module 3 user_id %q, public.users.id %q: want all three equal", login, sc.M2UserID, sc.M3UserID, stored)
		}
		got[login] = sc.M2UserID
	}
	if got[tenUserA] == got[tenUserB] || got[tenUserA] == "" {
		t.Fatalf("two users got the same module 2 user_id: %v", got)
	}
}

// TestTenant_EachUserGetsItsOwnModuleOneKeyAndTheSameUserTheSameKey: the key is the specified HMAC of the user's id (recomputed here from the specification, not by the package), a user's key is the
// same on every call, two users get different keys and different digests, and another secret gives another key. The key never appears in a printed scope.
func TestTenant_EachUserGetsItsOwnModuleOneKeyAndTheSameUserTheSameKey(t *testing.T) {
	e := newTenEnv(t)
	a1, a2, b := e.scope(e.res, tenUserA), e.scope(e.res, tenUserA), e.scope(e.res, tenUserB)
	if want := tenKey(e.secret, tenUserA); a1.M1Key.Reveal() != want {
		t.Fatalf("user A's module 1 key is not the specified HMAC (%d bytes vs %d)", len(a1.M1Key.Reveal()), len(want))
	}
	if want := tenKey(e.secret, tenUserB); b.M1Key.Reveal() != want {
		t.Fatalf("user B's module 1 key is not the specified HMAC")
	}
	if a1.M1Key != a2.M1Key {
		t.Fatal("the same user's key derived twice is not equal")
	}
	if a1.M1Key == b.M1Key || bytes.Equal(tenant.M1KeyDigest(a1.M1Key), tenant.M1KeyDigest(b.M1Key)) {
		t.Fatal("two users share a module 1 key or its digest")
	}
	if !bytes.Equal(tenant.M1KeyDigest(a1.M1Key), tenDigest(a1.M1Key.Reveal())) {
		t.Fatal("the digest module 1 stores is not the SHA-256 of the key's text")
	}
	if len(a1.M1Key.Reveal()) != 43 || strings.ContainsAny(a1.M1Key.Reveal(), "=+/") {
		t.Fatalf("the key is not 43 characters of unpadded base64url: %d characters", len(a1.M1Key.Reveal()))
	}
	// the control: another secret gives another key for the same user
	other, err := tenant.NewResolver(e.pool, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if ao := e.scope(other, tenUserA); ao.M1Key == a1.M1Key {
		t.Fatal("two secrets gave the same user the same key: the key does not depend on the secret")
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if out := fmt.Sprintf(verb, a1); strings.Contains(out, a1.M1Key.Reveal()) || !strings.Contains(out, "[redacted]") {
			t.Errorf("printing a scope with %s: %s", verb, out)
		}
	}
}

// TestTenantLive_RealAuthUsersRowsResolveThroughPlatformApp: the whole chain on the LIVE database, in a transaction that is rolled back: rows inserted into auth.users (the image's own table in the
// full harness) and linked public.users rows, the resolver running AS platform_app (the role switch is proven to bite), through the sanctioned view. A known login resolves, an unknown one, an
// inactive one and the system user are refused with their own errors. Paired controls: every refusal sits beside a login that resolves in the same transaction.
func TestTenantLive_RealAuthUsersRowsResolveThroughPlatformApp(t *testing.T) {
	requireLiveMigrated(t)
	ctx := testCtx(t)
	tx := shapeRWTx(t, liveAdmin(t))
	var ids [4]string
	if err := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text").Scan(&ids[0], &ids[1], &ids[2], &ids[3]); err != nil {
		t.Fatal(err)
	}
	active, inactive, authOnly, nobody := ids[0], ids[1], ids[2], ids[3]
	var gotrue bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'auth' AND table_name = 'users' AND column_name = 'encrypted_password')").Scan(&gotrue); err != nil {
		t.Fatal(err)
	}
	t.Logf("auth.users on this database is the image's own GoTrue table: %v (the full harness requires it, run.sh; the db-tests run uses the fixture)", gotrue)

	for id, email := range map[string]string{active: "live-active-" + active + "@example.test", inactive: "live-inactive-" + inactive + "@example.test", authOnly: "live-authonly-" + authOnly + "@example.test"} {
		tenSeedLogin(t, tx, id, email)
	}
	tenSeedUser(t, tx, active, "live-active-"+active+"@example.test", true)
	tenSeedUser(t, tx, inactive, "live-inactive-"+inactive+"@example.test", false)
	usersBefore := tenCount(t, tx, "SELECT count(*) FROM public.users")

	if err := shapeSwitchRole(ctx, tx, "platform_app"); err != nil {
		t.Fatal(err)
	}
	res, err := tenant.NewResolver(tx, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := res.ByAuthUser(ctx, active)
	if want := (tenant.Tenant{UserID: active, AuthUserID: active, Active: true}); err != nil || got != want {
		t.Fatalf("a real login as platform_app = %+v, %v; want %+v", got, err, want)
	}
	if sc, err := res.Scope(ctx, got); err != nil || sc.M2UserID != active || sc.M3UserID != active || sc.M1Key.Reveal() == "" {
		t.Fatalf("its scope = %+v, %v", sc, err)
	}
	for _, c := range []struct {
		what, id string
		want     error
	}{
		{"a login with no platform user", authOnly, tenant.ErrUnknownLogin},
		{"an id in no table", nobody, tenant.ErrUnknownLogin},
		{"an inactive user", inactive, tenant.ErrInactive},
		{"the system user", tenant.SystemUserID, tenant.ErrSystemUser},
		{"a malformed id", "not-a-uuid", tenant.ErrBadID},
	} {
		got, err := res.ByAuthUser(ctx, c.id)
		tenIs(t, c.what, got, err, c.want)
	}
	if after := tenCount(t, tx, "SELECT count(*) FROM public.users"); after != usersBefore {
		t.Fatalf("public.users holds %d rows after the refusals, %d before: a refused login created a user", after, usersBefore)
	}
}

// ---- module 1's own table: the live database, a rolled-back transaction, the privileged identity is the superuser ----

type tenLive struct {
	t      *testing.T
	ctx    context.Context
	tx     pgx.Tx
	secret config.Secret
	res    *tenant.Resolver
}

// newTenLive opens the rolled-back read-write transaction on the live database and FAILS (never skips) when module 1 is not migrated there: these tests need the full harness.
func newTenLive(t *testing.T) *tenLive {
	t.Helper()
	requireLiveMigrated(t)
	l := &tenLive{t: t, ctx: testCtx(t), secret: config.Secret(randomHex(t, 32))}
	l.tx = shapeRWTx(t, liveAdmin(t))
	var present bool
	if err := l.tx.QueryRow(l.ctx, "SELECT to_regclass('verify.api_callers') IS NOT NULL").Scan(&present); err != nil || !present {
		t.Fatalf("module 1's verify.api_callers does not exist in the live database (err %v): this test needs the full harness, where module 1 is migrated (the db-tests run skips TestM1Live*)", err)
	}
	var err error
	if l.res, err = tenant.NewResolver(nil, l.secret); err != nil { // provisioning needs no platform database: the privileged connection is passed per call
		t.Fatal(err)
	}
	return l
}

func (l *tenLive) newUser() string {
	l.t.Helper()
	var id string
	if err := l.tx.QueryRow(l.ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		l.t.Fatal(err)
	}
	return id
}

func (l *tenLive) total() int64 {
	return tenCount(l.t, l.tx, "SELECT count(*) FROM verify.api_callers")
}

func (l *tenLive) ownedBy(user string) int64 {
	return tenCount(l.t, l.tx, "SELECT count(*) FROM verify.api_callers WHERE user_id = $1", user)
}

// rowsOf is every column of the rows of one user as text, so two reads that are equal mean the rows were not touched.
func (l *tenLive) rowsOf(user string) []string {
	return mnyRows(l.t, l.tx, `SELECT caller_id, encode(key_sha256, 'hex'), status, COALESCE(user_id::text, 'NULL'), not_before::text, COALESCE(not_after::text, 'none'), COALESCE(label, 'none'), created_at::text
		FROM verify.api_callers WHERE user_id = $1 ORDER BY key_sha256`, user)
}

func (l *tenLive) provision(user string) error {
	return l.res.ProvisionM1Caller(l.ctx, l.tx, user)
}

// TestM1LiveTenant_ProvisionInsertsExactlyOneRowPerUserWithTheOwnerSet: provisioning a user writes one row in module 1's real verify.api_callers, owned by that user (user_id NOT NULL), with the digest of the
// derived key as specified, the user's caller_id, status active, a start in the past and no expiry. A second user's row is separate and the first row is untouched. The paired control is the count
// before: zero rows for either user and a total that moves by exactly one per user.
func TestM1LiveTenant_ProvisionInsertsExactlyOneRowPerUserWithTheOwnerSet(t *testing.T) {
	l := newTenLive(t)
	a, b := l.newUser(), l.newUser()
	n0 := l.total()
	if l.ownedBy(a) != 0 || l.ownedBy(b) != 0 {
		t.Fatalf("precondition: the fresh users already own rows")
	}
	if err := l.provision(a); err != nil {
		t.Fatalf("provision A: %v", err)
	}
	if l.ownedBy(a) != 1 || l.ownedBy(b) != 0 || l.total() != n0+1 {
		t.Fatalf("after provisioning A: A owns %d rows (want 1), B owns %d (want 0), total %d (want %d)", l.ownedBy(a), l.ownedBy(b), l.total(), n0+1)
	}
	var callerID, digest, status, owner string
	var ownerSet, noExpiry, started bool
	if err := l.tx.QueryRow(l.ctx, `SELECT caller_id, encode(key_sha256, 'hex'), status, user_id::text, user_id IS NOT NULL, not_after IS NULL, not_before <= now()
		FROM verify.api_callers WHERE user_id = $1`, a).Scan(&callerID, &digest, &status, &owner, &ownerSet, &noExpiry, &started); err != nil {
		t.Fatal(err)
	}
	if callerID != "platform-user-"+a || owner != a || !ownerSet || status != "active" || !noExpiry || !started {
		t.Fatalf("A's row is (caller_id %q, owner %q, owner set %v, status %q, no expiry %v, started %v)", callerID, owner, ownerSet, status, noExpiry, started)
	}
	if want := hex.EncodeToString(tenDigest(tenKey(l.secret, a))); digest != want {
		t.Fatalf("A's key_sha256 is %s, want the SHA-256 of the derived key's text %s", digest, want)
	}
	rowsA := l.rowsOf(a)

	if err := l.provision(b); err != nil {
		t.Fatalf("provision B: %v", err)
	}
	if l.ownedBy(b) != 1 || l.ownedBy(a) != 1 || l.total() != n0+2 {
		t.Fatalf("after provisioning B: A %d, B %d, total %d (want 1, 1, %d)", l.ownedBy(a), l.ownedBy(b), l.total(), n0+2)
	}
	if rowsB := l.rowsOf(b); len(rowsB) != 1 || rowsB[0] == rowsA[0] || strings.Split(rowsB[0], "|")[1] == strings.Split(rowsA[0], "|")[1] {
		t.Fatalf("B's row is not distinct from A's (different key_sha256 expected): %v / %v", rowsB, rowsA)
	}
	if again := l.rowsOf(a); fmt.Sprint(again) != fmt.Sprint(rowsA) {
		t.Fatalf("provisioning B touched A's row: %v -> %v", rowsA, again)
	}
}

// TestM1LiveTenant_ProvisionIsIdempotent: a second and a third call change nothing (the rows read back byte for byte equal, including created_at, so no DO UPDATE rewrote them) and report
// success. The paired control is a second user, which does change the table: the comparison can see a change.
func TestM1LiveTenant_ProvisionIsIdempotent(t *testing.T) {
	l := newTenLive(t)
	a, b := l.newUser(), l.newUser()
	if err := l.provision(a); err != nil {
		t.Fatal(err)
	}
	rows, total := l.rowsOf(a), l.total()
	for i := 2; i <= 3; i++ {
		if err := l.provision(a); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got := l.rowsOf(a); fmt.Sprint(got) != fmt.Sprint(rows) || l.total() != total {
			t.Fatalf("call %d changed the table: rows %v -> %v, total %d -> %d", i, rows, got, total, l.total())
		}
	}
	if err := l.provision(b); err != nil {
		t.Fatal(err)
	}
	if l.total() != total+1 {
		t.Fatalf("control: a second user did not add a row (total %d, want %d): the comparison above could not see a change", l.total(), total+1)
	}
}

// TestM1LiveTenant_ARowWithNoOwnerIsRefusedNotHidden: module 1 lets a caller row exist with user_id NULL (its key authenticates and accepts a job that nobody can then read), and a hand-written
// seeding does exactly that. The table allows it (shown here), so the function is the only wall: an empty or malformed id is refused with nothing written, the system user is refused, and a
// row that already holds the user's key with no owner makes the call FAIL (ErrM1CallerConflict) and is left as it is, instead of a silent DO NOTHING. The paired control is a user with no
// planted row, whose provisioning succeeds with the owner set.
func TestM1LiveTenant_ARowWithNoOwnerIsRefusedNotHidden(t *testing.T) {
	l := newTenLive(t)
	a, b := l.newUser(), l.newUser()
	total := l.total()

	for _, c := range []struct {
		what, id string
		want     error
	}{
		{"an empty id", "", tenant.ErrBadID},
		{"a blank id", "  ", tenant.ErrBadID},
		{"NULL as text", "NULL", tenant.ErrBadID},
		{"the system user", tenant.SystemUserID, tenant.ErrSystemUser},
	} {
		if err := l.provision(c.id); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want errors.Is(%v)", c.what, err, c.want)
		}
		if l.total() != total {
			t.Fatalf("%s: the table changed (%d -> %d rows) although the call was refused", c.what, total, l.total())
		}
	}

	// the trap, planted the way a hand-written script does: A's key, no user_id
	planted := tenDigest(tenKey(l.secret, a))
	if _, err := l.tx.Exec(l.ctx, "INSERT INTO verify.api_callers (caller_id, key_sha256, not_before) VALUES ('hand-written', $1, now() - interval '1 hour')", planted); err != nil {
		t.Fatalf("the table refuses a row with no owner (%v): then module 1 no longer has the trap this test is about", err)
	}
	if n := tenCount(t, l.tx, "SELECT count(*) FROM verify.api_callers WHERE key_sha256 = $1 AND user_id IS NULL", planted); n != 1 {
		t.Fatalf("the planted row is not there with a NULL owner (%d)", n)
	}
	plantedText := mnyRows(t, l.tx, "SELECT caller_id, COALESCE(user_id::text, 'NULL'), not_before::text, created_at::text FROM verify.api_callers WHERE key_sha256 = $1", planted)
	if err := l.provision(a); !errors.Is(err, tenant.ErrM1CallerConflict) {
		t.Fatalf("provisioning A over a row with no owner = %v, want ErrM1CallerConflict (a silent DO NOTHING would leave A's key ownerless)", err)
	}
	if got := mnyRows(t, l.tx, "SELECT caller_id, COALESCE(user_id::text, 'NULL'), not_before::text, created_at::text FROM verify.api_callers WHERE key_sha256 = $1", planted); fmt.Sprint(got) != fmt.Sprint(plantedText) {
		t.Fatalf("the refused call rewrote the planted row: %v -> %v", plantedText, got)
	}
	if l.total() != total+1 || l.ownedBy(a) != 0 {
		t.Fatalf("the refused call added a row or an owner: total %d (want %d), A owns %d (want 0)", l.total(), total+1, l.ownedBy(a))
	}

	// the control: B has no planted row, so the same function succeeds and the row has an owner
	if err := l.provision(b); err != nil {
		t.Fatalf("control: provisioning B: %v", err)
	}
	if n := tenCount(t, l.tx, "SELECT count(*) FROM verify.api_callers WHERE key_sha256 = $1 AND user_id = $2", tenDigest(tenKey(l.secret, b)), b); n != 1 {
		t.Fatalf("control: B's row has no owner or is missing (%d)", n)
	}
}

// TestM1LiveTenant_TheRuntimeRolesCannotProvision: the runtime roles cannot write module 1's table. Run as platform_app and as verify_app (module 1's own runtime login), the function's INSERT is
// refused by privilege (SQLSTATE 42501) and no row appears; the role switches are proven to bite. The paired control is the same call as the privileged identity (the superuser here), which
// succeeds and writes the row.
func TestM1LiveTenant_TheRuntimeRolesCannotProvision(t *testing.T) {
	l := newTenLive(t)
	a, b := l.newUser(), l.newUser()
	if err := l.provision(a); err != nil {
		t.Fatalf("control: the privileged identity cannot provision: %v", err)
	}
	if l.ownedBy(a) != 1 {
		t.Fatalf("control: the privileged identity wrote %d rows, want 1", l.ownedBy(a))
	}
	total := l.total()
	for _, role := range []string{"platform_app", "verify_app"} {
		sp, err := l.tx.Begin(l.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := shapeSwitchRole(l.ctx, sp, role); err != nil {
			t.Fatal(err)
		}
		err = l.res.ProvisionM1Caller(l.ctx, sp, b)
		_ = sp.Rollback(l.ctx)
		if code := sqlState(err); code != "42501" {
			t.Errorf("as %s: ProvisionM1Caller = %v (SQLSTATE %q), want a refusal by privilege (42501)", role, err, code)
		}
		if l.ownedBy(b) != 0 || l.total() != total {
			t.Errorf("as %s: a row appeared (B owns %d, total %d -> %d)", role, l.ownedBy(b), total, l.total())
		}
	}
}

// TestM1LiveTenant_ModuleOnesOwnLookupFindsTheDerivedKey: module 1's own lookup statement (control-plane internal/store/postgres.go:1345-1363 at 4adc316, copied verbatim) executed as verify_app,
// module 1's runtime role, with the digest of the derived key returns the user's caller_id and owner; the same statement with the digest of the OTHER candidate rule (SHA-256 over the 32 raw
// HMAC bytes instead of the key's text), with another user's key, and with a random digest finds nothing. That the module accepts the key over HTTP is chunk B9.
func TestM1LiveTenant_ModuleOnesOwnLookupFindsTheDerivedKey(t *testing.T) {
	l := newTenLive(t)
	a, b := l.newUser(), l.newUser()
	for _, u := range []string{a, b} {
		if err := l.provision(u); err != nil {
			t.Fatal(err)
		}
	}
	const lookup = `
SELECT caller_id, COALESCE(user_id::text,'') FROM verify.api_callers
WHERE key_sha256 = $1
  AND not_before <= $2
  AND (not_after IS NULL OR not_after > $2)
LIMIT 1`
	find := func(digest []byte) (callerID, owner string, found bool) {
		t.Helper()
		sp, err := l.tx.Begin(l.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Rollback(l.ctx)
		if err := shapeSwitchRole(l.ctx, sp, "verify_app"); err != nil {
			t.Fatal(err)
		}
		switch err := sp.QueryRow(l.ctx, lookup, digest, time.Now().UTC()).Scan(&callerID, &owner); {
		case err == nil:
			return callerID, owner, true
		case errors.Is(err, pgx.ErrNoRows):
			return "", "", false
		default:
			t.Fatalf("module 1's lookup failed as verify_app: %v", err)
		}
		return "", "", false
	}

	for _, u := range []string{a, b} {
		callerID, owner, found := find(tenDigest(tenKey(l.secret, u)))
		if !found || callerID != "platform-user-"+u || owner != u {
			t.Errorf("module 1's lookup of user %s's derived key = (%q, %q, found %v), want (platform-user-%s, %s, true)", u, callerID, owner, found, u, u)
		}
	}
	rawMAC, err := base64.RawURLEncoding.DecodeString(tenKey(l.secret, a))
	if err != nil {
		t.Fatal(err)
	}
	wrongRule := sha256.Sum256(rawMAC)
	if _, _, found := find(wrongRule[:]); found {
		t.Error("module 1's lookup found a row by the digest over the raw HMAC bytes: the digest rule is not the key's text, and the test above would not tell")
	}
	if _, _, found := find(tenDigest(tenKey(config.Secret(randomHex(t, 32)), a))); found {
		t.Error("module 1's lookup found a row for a key derived from another secret")
	}
	if _, _, found := find(tenDigest("not-a-provisioned-key")); found {
		t.Error("module 1's lookup found a row for a key nobody provisioned")
	}
}

// TestM1LiveTenant_RotationBothVersionsAuthenticateAsTheSameUserThenTheOldOneEnds is the proof of OD-2's rotation path (Phase 3 close-out A4) against module 1's REAL verify.api_callers and its OWN
// lookup (the statement of its front door, run as verify_app): a user has a version-1 key (secret one) and, provisioned beside it, a version-2 key (secret two). Both digests are found by module
// 1's lookup, both name the same caller_id and the same owner (so a job submitted with either is the same tenant's), and they are distinct. Retiring version 1 with a grace period leaves it valid
// until the end of the grace and marks it rotating_out; retiring it at once makes module 1's lookup stop finding it while the version-2 key still resolves; a retirement names exactly one user's
// row of exactly one version (another user's version-1 key is untouched; a version nobody provisioned is ErrM1CallerNotFound and changes nothing). The paired control is the state before each step.
func TestM1LiveTenant_RotationBothVersionsAuthenticateAsTheSameUserThenTheOldOneEnds(t *testing.T) {
	l := newTenLive(t)
	a, b := l.newUser(), l.newUser()
	secondSecret := config.Secret(randomHex(t, 32))
	r2, err := tenant.NewResolver(nil, secondSecret, tenant.WithM1KeyVersion(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{a, b} { // version 1 of both users first: the state of a deployment before the rotation
		if err := l.provision(u); err != nil {
			t.Fatal(err)
		}
	}
	const lookup = `
SELECT caller_id, COALESCE(user_id::text,'') FROM verify.api_callers
WHERE key_sha256 = $1
  AND not_before <= $2
  AND (not_after IS NULL OR not_after > $2)
LIMIT 1`
	find := func(digest []byte) (callerID, owner string, found bool) {
		t.Helper()
		sp, err := l.tx.Begin(l.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Rollback(l.ctx)
		if err := shapeSwitchRole(l.ctx, sp, "verify_app"); err != nil {
			t.Fatal(err)
		}
		switch err := sp.QueryRow(l.ctx, lookup, digest, time.Now().UTC()).Scan(&callerID, &owner); {
		case err == nil:
			return callerID, owner, true
		case errors.Is(err, pgx.ErrNoRows):
			return "", "", false
		default:
			t.Fatalf("module 1's lookup failed as verify_app: %v", err)
		}
		return "", "", false
	}
	v1a, v2a := tenDigest(tenKeyV(l.secret, 1, a)), tenDigest(tenKeyV(secondSecret, 2, a))
	v1b := tenDigest(tenKeyV(l.secret, 1, b))
	if bytes.Equal(v1a, v2a) {
		t.Fatal("the version-1 and version-2 digests of one user are equal")
	}
	// before the second version exists only the first resolves (the control of the next step)
	if _, _, found := find(v2a); found {
		t.Fatal("the version-2 key resolves before it was provisioned")
	}
	if err := r2.ProvisionM1Caller(l.ctx, l.tx, a); err != nil { // the resolver's own version: 2
		t.Fatalf("provision version 2 beside version 1: %v", err)
	}
	if got := l.ownedBy(a); got != 2 {
		t.Fatalf("user A has %d rows after the second provisioning, want 2 (one per version, module 1's rotation design)", got)
	}
	for _, c := range []struct {
		name   string
		digest []byte
	}{{"version 1", v1a}, {"version 2", v2a}} {
		caller, owner, found := find(c.digest)
		if !found || caller != "platform-user-"+a || owner != a {
			t.Errorf("module 1's lookup of user A's %s key = (%q, %q, %v), want (platform-user-%s, %s, true): both versions must be the same tenant", c.name, caller, owner, found, a, a)
		}
	}
	// a version the platform never wrote and another user's key do not resolve as user A
	if _, owner, found := find(tenDigest(tenKeyV(l.secret, 2, a))); found {
		t.Errorf("version 2 under the OLD secret resolves (owner %q): a rotation that keeps no secret apart would protect nothing", owner)
	}
	if _, owner, _ := find(v1b); owner != b {
		t.Errorf("user B's version-1 key resolves to %q, want B", owner)
	}

	// retire version 1 with a grace period: still valid inside it, marked rotating_out, version 2 untouched
	if err := l.res.RetireM1CallerVersion(l.ctx, l.tx, a, 1, time.Hour); err != nil {
		t.Fatalf("retire version 1 with a grace: %v", err)
	}
	if _, _, found := find(v1a); !found {
		t.Error("version 1 stopped resolving inside its grace period")
	}
	if got := mnyRows(t, l.tx, "SELECT status FROM verify.api_callers WHERE key_sha256 = $1", v1a); len(got) != 1 || got[0] != "rotating_out" {
		t.Errorf("version 1's status after the retirement = %v, want [rotating_out]", got)
	}
	if got := mnyRows(t, l.tx, "SELECT status FROM verify.api_callers WHERE key_sha256 = $1", v2a); len(got) != 1 || got[0] != "active" {
		t.Errorf("version 2's status after the retirement of version 1 = %v, want [active]", got)
	}
	// retire it at once: module 1's lookup stops finding it, the version-2 key still resolves, another user's version 1 is untouched
	if err := l.res.RetireM1CallerVersion(l.ctx, l.tx, a, 1, 0); err != nil {
		t.Fatalf("retire version 1 at once: %v", err)
	}
	if _, _, found := find(v1a); found {
		t.Error("module 1's lookup still finds the version-1 key after it was retired at once")
	}
	if caller, owner, found := find(v2a); !found || owner != a || caller != "platform-user-"+a {
		t.Errorf("after retiring version 1 the version-2 key resolves to (%q, %q, %v), want user A's", caller, owner, found)
	}
	if _, owner, found := find(v1b); !found || owner != b {
		t.Errorf("retiring user A's version 1 changed user B's version-1 key: (%q, %v)", owner, found)
	}
	// a retirement never reaches a version nobody provisioned or a row another user owns, and it deletes nothing
	if err := l.res.RetireM1CallerVersion(l.ctx, l.tx, a, 3, 0); !errors.Is(err, tenant.ErrM1CallerNotFound) {
		t.Errorf("retiring a version that was never provisioned: %v, want ErrM1CallerNotFound", err)
	}
	if got := l.ownedBy(a); got != 2 {
		t.Errorf("user A has %d rows after the retirements, want 2: a retirement deletes nothing", got)
	}
}
