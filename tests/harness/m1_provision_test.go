//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 4 B16: module 1's caller key is registered AUTOMATICALLY, at a user's first module 1 job, through platform.provision_m1_caller (db/install/postmodule1.sql): a SECURITY DEFINER function owned by module 1's
// grant-admin, executable by platform_app and nobody else, that writes exactly one kind of row into module 1's REAL verify.api_callers. The Go half is internal/tenant's M1CallerEnsurer (unit-tested in that
// package against a recording stand-in); THESE tests are the half only a database can prove, on the pinned supabase image as the real roles, against module 1's real table and its real front door.
//
// THE NAME. Every test is TestM1LiveProvision_*: they need module 1 migrated, so the db-tests workflow (which has no module 1) skips them by the prefix TestM1Live (tests/harness/run-db-tests.sh) and the full
// harness runs and counts them; newM1pEnv FAILS, never skips, where module 1 or the function is not there (like TestM1LiveTenant_*).
//
// WHAT IS REAL. The function and the table (module 1's migrations, then postmodule1.sql, run by the harness as the non-superuser `postgres`), the login (a pool of real platform_app connections, proven not to be a
// superuser), module 1's front door over HTTP (a key that is registered authenticates, one that is not is a 401, another user's key is a 404 on the first user's job). WHAT IS NOT: the platform's job engine (it calls the
// ensurer; here the tests do). The rows are COMMITTED (the function runs in the pool's own sessions and module 1 reads them from its own), so every test removes the rows it caused, through the superuser
// connection, when it ends; nothing else in module 1's table is touched.
//
// Every key is derived from a secret minted for the test and never printed; no error text here carries one.

const m1pFn = "platform.provision_m1_caller(uuid, bytea)"

// m1pFnReg is the function as a catalog lookup, written into the statements (a constant, never a parameter: the driver need not know the type regprocedure).
const m1pFnReg = "'" + m1pFn + "'::regprocedure"

type m1pEnv struct {
	t       *testing.T
	ctx     context.Context
	adminCS string        // the superuser's connection string (a second writer, for the in-flight insert of the forced interleave)
	admin   *pgx.Conn     // a WRITABLE superuser connection to the live database: plants rows, reads module 1's table, removes what the test caused
	pool    *pgxpool.Pool // real platform_app logins: what the service runs as
	secret  config.Secret
	res     *tenant.Resolver
	users   []string // every user id the test made, so that its rows can be removed
	digests [][]byte // every digest planted by hand
}

// newM1pEnv FAILS (never skips) when the live database has no module 1 or no function: these tests need the full harness, where run.sh migrated module 1 and ran db/install/postmodule1.sql.
func newM1pEnv(t *testing.T) *m1pEnv {
	t.Helper()
	requireLiveMigrated(t)
	e := &m1pEnv{t: t, ctx: testCtx(t), adminCS: requireEnv(t, envAdminURL), secret: config.Secret(randomHex(t, 32))}
	e.admin = dial(t, e.adminCS, nil)
	var present, installed bool
	if err := e.admin.QueryRow(e.ctx, "SELECT to_regclass('verify.api_callers') IS NOT NULL, to_regprocedure($1) IS NOT NULL", m1pFn).Scan(&present, &installed); err != nil {
		t.Fatalf("cannot ask the live database whether module 1 and the function are there: %v", err)
	}
	if !present {
		t.Fatalf("module 1's verify.api_callers does not exist in the live database: this test needs the full harness, where module 1 is migrated (the db-tests run skips TestM1Live*)")
	}
	if !installed {
		t.Fatalf("%s does not exist in the live database: db/install/postmodule1.sql was not run after module 1's migrations (tests/harness/run.sh stage 4 runs it; Docs/install.md step 3b)", m1pFn)
	}
	e.pool = mnyPool(t, e.adminCS) // proves a real login as platform_app, not a superuser
	var err error
	if e.res, err = tenant.NewResolver(e.pool, e.secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.cleanup)
	return e
}

// cleanup removes every row this test caused in module 1's table, by owner and by planted digest, as the superuser. It runs before the connections close (t.Cleanup is last in, first out).
func (e *m1pEnv) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if len(e.users) > 0 {
		if _, err := e.admin.Exec(ctx, "DELETE FROM verify.api_callers WHERE user_id = ANY($1::uuid[]) OR caller_id = ANY($2::text[])", e.users, m1pCallerIDs(e.users)); err != nil {
			e.t.Errorf("cleanup: could not remove the rows of the test's users: %v", err)
		}
	}
	for _, d := range e.digests {
		if _, err := e.admin.Exec(ctx, "DELETE FROM verify.api_callers WHERE key_sha256 = $1", d); err != nil {
			e.t.Errorf("cleanup: could not remove a planted row: %v", err)
		}
	}
}

func m1pCallerIDs(users []string) []string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = tenant.M1CallerID(u)
	}
	return out
}

// newUser is a fresh random user id (module 1 holds no user table: the id is the only thing a row says about the user).
func (e *m1pEnv) newUser() string {
	e.t.Helper()
	var id string
	if err := e.admin.QueryRow(e.ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	e.users = append(e.users, id)
	return id
}

// digest is the digest module 1 stores for the user's key, derived HERE from the specification (tenKey is the test's own copy of the derivation) and not by the package under test.
func (e *m1pEnv) digest(user string) []byte { return tenDigest(tenKey(e.secret, user)) }

// ensurer is an ensurer over the platform_app pool and this test's resolver, with no memory unless n says otherwise: every call reaches the function.
func (e *m1pEnv) ensurer(res *tenant.Resolver, entries int) *tenant.M1CallerEnsurer {
	return tenant.NewM1CallerEnsurer(res, e.pool, tenant.WithEnsureCacheEntries(entries))
}

// call is the function itself, called as platform_app (the pool), with the arguments given as they are (a nil user is SQL NULL).
func (e *m1pEnv) call(user any, digest any) (string, error) {
	var word string
	err := e.pool.QueryRow(e.ctx, "SELECT platform.provision_m1_caller($1::uuid, $2::bytea)", user, digest).Scan(&word)
	return word, err
}

func (e *m1pEnv) total() int64 {
	return tenCount(e.t, e.admin, "SELECT count(*) FROM verify.api_callers")
}

// rows is every column of every row of one user as text, so two reads that are equal mean the rows were not touched.
func (e *m1pEnv) rows(user string) []string {
	return mnyRows(e.t, e.admin, `SELECT caller_id, encode(key_sha256, 'hex'), status, COALESCE(user_id::text, 'NULL'), not_before::text, COALESCE(not_after::text, 'none'), COALESCE(label, 'none'), created_at::text
		FROM verify.api_callers WHERE user_id = $1 ORDER BY key_sha256`, user)
}

func (e *m1pEnv) rowsOfDigest(d []byte) []string {
	return mnyRows(e.t, e.admin, `SELECT caller_id, encode(key_sha256, 'hex'), status, COALESCE(user_id::text, 'NULL'), not_before::text, COALESCE(not_after::text, 'none'), COALESCE(label, 'none'), created_at::text
		FROM verify.api_callers WHERE key_sha256 = $1`, d)
}

// plant writes a row by hand as the superuser (what a hand-written seeding does): the given owner (nil = NULL), window and caller_id, for the digest.
func (e *m1pEnv) plant(d []byte, callerID string, owner any, notBefore, notAfter string) {
	e.t.Helper()
	e.digests = append(e.digests, d)
	if _, err := e.admin.Exec(e.ctx, `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, not_after, user_id)
		VALUES ($1, $2, `+notBefore+`, `+notAfter+`, $3::uuid)`, callerID, d, owner); err != nil {
		e.t.Fatalf("plant a row by hand: %v", err)
	}
}

func (e *m1pEnv) pgErr(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// ---- module 1 over HTTP ----

var m1pKeySeq atomic.Int64

func m1pIdemKey(label string) string {
	return fmt.Sprintf("m1p.%s.%d.%d", label, time.Now().UnixNano(), m1pKeySeq.Add(1))
}

// adapter is module 1's front door as the holder of this key (the key travels in every request, nothing else names the caller).
func (e *m1pEnv) adapter(key config.Secret) *m1ad.Adapter {
	e.t.Helper()
	a, err := m1ad.New(m3lBaseURL(e.t, m1bEnvURL), key, nil, modclient.Options{Timeout: 30 * time.Second})
	if err != nil {
		e.t.Fatalf("cannot build the module 1 client: %v", err) // New's errors never carry the URL or the key
	}
	return a
}

// submit sends a job of two rows that module 1 answers from its own syntax layer (a one-label domain and a missing @): no DNS, no node, nothing leaves the stack.
func (e *m1pEnv) submit(a *m1ad.Adapter) (string, error) {
	res, err := a.SubmitRaw(e.ctx, []m1ad.Item{{ItemID: "row-0", Address: "dup@nodot"}, {ItemID: "row-1", Address: "no-at-sign.example"}}, m1pIdemKey("job"))
	return res.JobID, err
}

func (e *m1pEnv) kindOf(err error) (modclient.Kind, int) {
	var me *modclient.Error
	if errors.As(err, &me) {
		return me.Kind, me.Status
	}
	return 0, 0
}

// ---- the tests ----

// TestM1LiveProvision_TheFirstCallRegistersExactlyOneRowOwnedByTheUser: as platform_app the function answers 'created' and ONE row appears in module 1's real table: the constant caller_id of the user, the digest
// of the key derived from the specification, the owner set, status active, a start in the past and no expiry. A second user's row is separate and the first row is untouched. The paired control is the
// state before (no row for either user, a total that moves by exactly one per user).
func TestM1LiveProvision_TheFirstCallRegistersExactlyOneRowOwnedByTheUser(t *testing.T) {
	e := newM1pEnv(t)
	a, b := e.newUser(), e.newUser()
	n0 := e.total()
	if len(e.rows(a)) != 0 || len(e.rows(b)) != 0 {
		t.Fatal("precondition: the fresh users already own rows")
	}
	word, err := e.call(a, e.digest(a))
	if err != nil || word != "created" {
		t.Fatalf("the first call for user A = %q, %v; want 'created'", word, err)
	}
	if got := len(e.rows(a)); got != 1 || len(e.rows(b)) != 0 || e.total() != n0+1 {
		t.Fatalf("after A's first call: A owns %d rows (want 1), B owns %d (want 0), total %d (want %d)", got, len(e.rows(b)), e.total(), n0+1)
	}
	var callerID, digest, status, owner string
	var noExpiry, started bool
	if err := e.admin.QueryRow(e.ctx, `SELECT caller_id, encode(key_sha256, 'hex'), status, user_id::text, not_after IS NULL, not_before <= now() AND not_before >= now() - interval '2 hours'
		FROM verify.api_callers WHERE user_id = $1`, a).Scan(&callerID, &digest, &status, &owner, &noExpiry, &started); err != nil {
		t.Fatal(err)
	}
	if callerID != "platform-user-"+a || owner != a || status != "active" || !noExpiry || !started {
		t.Fatalf("A's row is (caller_id %q, owner %q, status %q, no expiry %v, started within the last two hours %v)", callerID, owner, status, noExpiry, started)
	}
	if want := fmt.Sprintf("%x", e.digest(a)); digest != want {
		t.Fatalf("A's key_sha256 is %s, want the SHA-256 of the key derived from the specification %s", digest, want)
	}
	rowsA := e.rows(a)
	if word, err := e.call(b, e.digest(b)); err != nil || word != "created" {
		t.Fatalf("the first call for user B = %q, %v; want 'created'", word, err)
	}
	if len(e.rows(b)) != 1 || len(e.rows(a)) != 1 || e.total() != n0+2 {
		t.Fatalf("after B's first call: A %d, B %d, total %d (want 1, 1, %d)", len(e.rows(a)), len(e.rows(b)), e.total(), n0+2)
	}
	if again := e.rows(a); fmt.Sprint(again) != fmt.Sprint(rowsA) {
		t.Fatalf("registering B touched A's row: %v -> %v", rowsA, again)
	}
	if strings.Split(e.rows(b)[0], "|")[1] == strings.Split(rowsA[0], "|")[1] {
		t.Fatal("A and B share a key digest")
	}
}

// TestM1LiveProvision_TheKeyAuthenticatesAtModuleOnesFrontDoorAsThatUserAndNobodyElse: THE end-to-end proof, over HTTP, against the real module 1. Before the first call the user's derived key is a 401 (Refused) and
// creates no job; EnsureCaller (the Go door, as platform_app, through the function) registers it; then the same key submits a job that module 1 stamps with the user's id, and reads it back. A second user,
// registered the same way, gets 404 on the first user's job (the same answer as for a job that does not exist) and reads its own. The paired controls are the 401 before, the owner's own read, and the second
// user's own job.
func TestM1LiveProvision_TheKeyAuthenticatesAtModuleOnesFrontDoorAsThatUserAndNobodyElse(t *testing.T) {
	e := newM1pEnv(t)
	ens := e.ensurer(e.res, 0)
	a, b := e.newUser(), e.newUser()
	keyA, err := tenant.DeriveM1Key(e.secret, a)
	if err != nil {
		t.Fatal(err)
	}
	if keyA.Reveal() != tenKey(e.secret, a) {
		t.Fatal("the package's key of user A is not the key of the specification")
	}
	adA := e.adapter(keyA)

	// before: module 1 has never heard of the key
	jobsBefore := tenCount(t, e.admin, "SELECT count(*) FROM verify.jobs")
	if _, err := e.submit(adA); err == nil {
		t.Fatal("module 1 accepted a job from a key nobody registered")
	} else if kind, status := e.kindOf(err); kind != modclient.Refused || status != http.StatusUnauthorized {
		t.Fatalf("an unregistered key: %v; want a Refused 401", err)
	}
	if n := tenCount(t, e.admin, "SELECT count(*) FROM verify.jobs"); n != jobsBefore {
		t.Fatalf("the refused submit created %d job(s)", n-jobsBefore)
	}

	// the first module 1 job of user A: the platform ensures the caller first
	if err := ens.EnsureCaller(e.ctx, a); err != nil {
		t.Fatalf("EnsureCaller(A): %v", err)
	}
	jobA, err := e.submit(adA)
	if err != nil {
		t.Fatalf("A's key was registered but module 1 refused its submit: %v", err)
	}
	var jobOwner string
	if err := e.admin.QueryRow(e.ctx, "SELECT COALESCE(owner_user_id::text, 'NULL') FROM verify.jobs WHERE id = $1::uuid", jobA).Scan(&jobOwner); err != nil || jobOwner != a {
		t.Fatalf("module 1 stamped job %s with owner %q (err %v), want user A %s: the key's user_id is what makes a job readable", jobA, jobOwner, err, a)
	}
	if _, err := adA.Status(e.ctx, jobA); err != nil {
		t.Fatalf("A cannot read its own job (the control): %v", err)
	}

	// a second user, registered the same way, is a different tenant
	if err := ens.EnsureCaller(e.ctx, b); err != nil {
		t.Fatalf("EnsureCaller(B): %v", err)
	}
	keyB, _ := tenant.DeriveM1Key(e.secret, b)
	adB := e.adapter(keyB)
	if _, err := adB.Status(e.ctx, jobA); err == nil {
		t.Fatal("user B read user A's job")
	} else if kind, status := e.kindOf(err); kind != modclient.NotFound || status != http.StatusNotFound {
		t.Fatalf("B reading A's job: %v; want NotFound 404", err)
	}
	jobB, err := e.submit(adB)
	if err != nil {
		t.Fatalf("B's own submit (the control): %v", err)
	}
	if _, err := adB.Status(e.ctx, jobB); err != nil {
		t.Fatalf("B cannot read its own job (the control): %v", err)
	}
	if _, err := adA.Status(e.ctx, jobB); err == nil {
		t.Fatal("user A read user B's job")
	}
}

// TestM1LiveProvision_ASecondCallIsANoOpAndReportsExists: the second and third call change nothing (the rows read back byte for byte equal, created_at included, so nothing was rewritten), the function says
// 'exists' and EnsureCaller (with no memory, so it reaches the function) returns nil. The paired control is a second user, which does change the table: the comparison can see a change.
func TestM1LiveProvision_ASecondCallIsANoOpAndReportsExists(t *testing.T) {
	e := newM1pEnv(t)
	ens := e.ensurer(e.res, 0)
	a, b := e.newUser(), e.newUser()
	if word, err := e.call(a, e.digest(a)); err != nil || word != "created" {
		t.Fatalf("the first call = %q, %v", word, err)
	}
	rows, total := e.rows(a), e.total()
	for i := 2; i <= 3; i++ {
		word, err := e.call(a, e.digest(a))
		if err != nil || word != "exists" {
			t.Fatalf("call %d = %q, %v; want 'exists'", i, word, err)
		}
		if got := e.rows(a); fmt.Sprint(got) != fmt.Sprint(rows) || e.total() != total {
			t.Fatalf("call %d changed the table: rows %v -> %v, total %d -> %d", i, rows, got, total, e.total())
		}
	}
	if err := ens.EnsureCaller(e.ctx, a); err != nil {
		t.Fatalf("EnsureCaller for a user already registered: %v", err)
	}
	if got := e.rows(a); fmt.Sprint(got) != fmt.Sprint(rows) || e.total() != total {
		t.Fatalf("EnsureCaller changed the table: rows %v -> %v", rows, got)
	}
	if err := ens.EnsureCaller(e.ctx, b); err != nil {
		t.Fatal(err)
	}
	if e.total() != total+1 {
		t.Fatalf("control: a second user did not add a row (total %d, want %d): the comparison above could not see a change", e.total(), total+1)
	}
}

// TestM1LiveProvision_ARotatedKeyVersionIsRegisteredBesideTheOldOneAsTheSameTenant: the function is also how the NEW version's key gets in after a rotation (the resolver's version moves, the ensurer derives the
// new key, a new digest, a new row): two rows of one user under ONE caller_id, and module 1 accepts both keys as the same tenant (the version-2 key reads the job the version-1 key submitted). Nothing is
// retired (the function cannot end a row: that is the operator's step). The paired control is the version-2 key before it is registered: a 401.
func TestM1LiveProvision_ARotatedKeyVersionIsRegisteredBesideTheOldOneAsTheSameTenant(t *testing.T) {
	e := newM1pEnv(t)
	a := e.newUser()
	if err := e.ensurer(e.res, 0).EnsureCaller(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	key1, _ := tenant.DeriveM1Key(e.secret, a)
	job, err := e.submit(e.adapter(key1))
	if err != nil {
		t.Fatalf("the version-1 key's submit: %v", err)
	}

	secret2 := config.Secret(randomHex(t, 32))
	r2, err := tenant.NewResolver(e.pool, secret2, tenant.WithM1KeyVersion(2))
	if err != nil {
		t.Fatal(err)
	}
	key2, err := tenant.DeriveM1KeyVersion(secret2, 2, a)
	if err != nil {
		t.Fatal(err)
	}
	ad2 := e.adapter(key2)
	if _, err := ad2.Status(e.ctx, job); err == nil {
		t.Fatal("control: the version-2 key authenticates before it was registered")
	} else if kind, status := e.kindOf(err); kind != modclient.Refused || status != http.StatusUnauthorized {
		t.Fatalf("control: the unregistered version-2 key: %v; want a Refused 401", err)
	}
	if err := e.ensurer(r2, 0).EnsureCaller(e.ctx, a); err != nil {
		t.Fatalf("EnsureCaller at version 2: %v", err)
	}
	e.digests = append(e.digests, tenDigest(tenKeyV(secret2, 2, a)))
	if got := len(e.rows(a)); got != 2 {
		t.Fatalf("user A has %d rows after the second version, want 2 (one per version, module 1's rotation design)", got)
	}
	if n := tenCount(t, e.admin, "SELECT count(DISTINCT caller_id) FROM verify.api_callers WHERE user_id = $1", a); n != 1 {
		t.Fatalf("the two versions have %d caller_ids, want one (both are the same principal)", n)
	}
	if _, err := ad2.Status(e.ctx, job); err != nil {
		t.Fatalf("the version-2 key cannot read the job the version-1 key submitted: %v (both must be the same tenant)", err)
	}
	var retired int64
	if err := e.admin.QueryRow(e.ctx, "SELECT count(*) FROM verify.api_callers WHERE user_id = $1 AND (not_after IS NOT NULL OR status <> 'active')", a).Scan(&retired); err != nil || retired != 0 {
		t.Fatalf("%d row(s) of user A were ended or marked by provisioning (err %v): the function cannot and must not", retired, err)
	}
}

// TestM1LiveProvision_PlatformAppCannotWriteModuleOnesTableItselfAndTheFunctionIsWhatGivesThePower: as platform_app every direct statement on verify.api_callers is refused by privilege (42501): SELECT, INSERT,
// UPDATE, DELETE, TRUNCATE, and so is becoming module 1's grant-admin or owner (SET ROLE). The paired controls: the same INSERT as the superuser works (the statement is valid; in a rolled-back
// transaction), and the SAME platform_app session that was just refused gets its row through the function: it is the function, not the role, that holds the power.
func TestM1LiveProvision_PlatformAppCannotWriteModuleOnesTableItselfAndTheFunctionIsWhatGivesThePower(t *testing.T) {
	e := newM1pEnv(t)
	a := e.newUser()
	d := e.digest(a)
	const insert = `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id) VALUES ('zz-direct', $1, now() - interval '1 hour', $2::uuid)`
	// the control: the statement is valid, a superuser's run of it works (rolled back)
	tx := shapeRWTx(t, liveAdmin(t))
	if tag, err := tx.Exec(e.ctx, insert, d, a); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("control: the INSERT as the superuser = %v, %v; the statement is not valid, so the refusals below prove nothing", tag, err)
	}
	_ = tx.Rollback(e.ctx)

	n0 := e.total()
	for _, c := range []struct {
		what, sql string
		args      []any
	}{
		{"SELECT", "SELECT count(*) FROM verify.api_callers", nil},
		{"INSERT", insert, []any{d, a}},
		{"UPDATE", "UPDATE verify.api_callers SET not_after = now() WHERE user_id = $1::uuid", []any{a}},
		{"DELETE", "DELETE FROM verify.api_callers WHERE user_id = $1::uuid", []any{a}},
		{"TRUNCATE", "TRUNCATE verify.api_callers", nil},
		{"SET ROLE verify_admin", "SET ROLE verify_admin", nil},
		{"SET ROLE verify_owner", "SET ROLE verify_owner", nil},
	} {
		_, err := e.pool.Exec(e.ctx, c.sql, c.args...)
		if pe := e.pgErr(err); pe == nil || pe.Code != "42501" {
			t.Errorf("platform_app: %s = %v; want a refusal by privilege (42501)", c.what, err)
		}
	}
	if e.total() != n0 || len(e.rows(a)) != 0 {
		t.Fatalf("a refused statement left a trace: total %d -> %d, A's rows %d", n0, e.total(), len(e.rows(a)))
	}
	// the control: the pool itself is healthy, and the same identity registers the user through the function
	if _, err := e.pool.Exec(e.ctx, "SELECT 1"); err != nil {
		t.Fatalf("control: the pool cannot run a statement: %v", err)
	}
	if word, err := e.call(a, d); err != nil || word != "created" || len(e.rows(a)) != 1 {
		t.Fatalf("control: the function as the same platform_app = %q, %v, %d row(s); want 'created' and 1", word, err, len(e.rows(a)))
	}
}

// TestM1LiveProvision_TheFunctionRefusesWhatItMustNotAndWritesNothing: a NULL user, the all-zero system user and a digest that is not 32 bytes (31, 33, 0, and NULL) are refused by the function itself, each with the
// SQLSTATE and the constraint name the Go side maps, and no row appears. The paired control is a good user and a 32-byte digest, which succeed.
func TestM1LiveProvision_TheFunctionRefusesWhatItMustNotAndWritesNothing(t *testing.T) {
	e := newM1pEnv(t)
	a := e.newUser()
	good := e.digest(a)
	n0 := e.total()
	for _, c := range []struct {
		what       string
		user, dig  any
		code, name string
	}{
		{"a NULL user", nil, good, "22004", "m1_caller_user_required"},
		{"the all-zero system user", tenant.SystemUserID, good, "22023", "m1_caller_not_the_system_user"},
		{"a 31-byte digest", a, good[:31], "22023", "m1_caller_digest_is_32_bytes"},
		{"a 33-byte digest", a, append(append([]byte{}, good...), 0), "22023", "m1_caller_digest_is_32_bytes"},
		{"an empty digest", a, []byte{}, "22023", "m1_caller_digest_is_32_bytes"},
		{"a NULL digest", a, nil, "22023", "m1_caller_digest_is_32_bytes"},
	} {
		word, err := e.call(c.user, c.dig)
		pe := e.pgErr(err)
		if pe == nil || pe.Code != c.code || pe.ConstraintName != c.name || word != "" {
			t.Errorf("%s: (%q, %v); want SQLSTATE %s on constraint %s and no answer", c.what, word, err, c.code, c.name)
		}
		if e.total() != n0 || len(e.rows(a)) != 0 {
			t.Fatalf("%s: a row appeared (total %d -> %d)", c.what, n0, e.total())
		}
		if err != nil && (strings.Contains(err.Error(), fmt.Sprintf("%x", good)) || strings.Contains(err.Error(), tenKey(e.secret, a))) {
			t.Errorf("%s: the error text carries key material: %v", c.what, err)
		}
	}
	if word, err := e.call(a, good); err != nil || word != "created" || len(e.rows(a)) != 1 {
		t.Fatalf("control: a good user and a 32-byte digest = %q, %v, %d row(s); want 'created' and 1", word, err, len(e.rows(a)))
	}
}

// TestM1LiveProvision_ARowThatDoesNotActForTheUserIsRefusedAndLeftUnchanged: a row planted by hand that holds the user's digest but would not authenticate the user is NEVER hidden by the silent DO NOTHING: the
// function RAISES (23505, constraint m1_caller_row_acts_for_the_user), EnsureCaller returns ErrM1CallerConflict, and the planted row reads back byte for byte as it was. Four traps: no owner (module 1's table
// allows it: such a key accepts jobs nobody can read), another user's owner, an end in the past, a start in the future. The paired control is a user with no planted row, whose call succeeds with the owner
// set, and the table's total, which moves only by that one row.
func TestM1LiveProvision_ARowThatDoesNotActForTheUserIsRefusedAndLeftUnchanged(t *testing.T) {
	e := newM1pEnv(t)
	ens := e.ensurer(e.res, 0)
	other := e.newUser()
	traps := []struct {
		what, notBefore, notAfter string
		owner                     func(user string) any
	}{
		{"no owner", "now() - interval '1 hour'", "NULL", func(string) any { return nil }},
		{"another user's owner", "now() - interval '1 hour'", "NULL", func(string) any { return other }},
		{"an end in the past", "now() - interval '2 hours'", "now() - interval '1 minute'", func(u string) any { return u }},
		{"a start in the future", "now() + interval '1 hour'", "NULL", func(u string) any { return u }},
	}
	for _, c := range traps {
		user := e.newUser()
		d := e.digest(user)
		e.plant(d, "hand-written", c.owner(user), c.notBefore, c.notAfter)
		planted := e.rowsOfDigest(d)
		total := e.total()
		word, err := e.call(user, d)
		if pe := e.pgErr(err); pe == nil || pe.Code != "23505" || pe.ConstraintName != "m1_caller_row_acts_for_the_user" || word != "" {
			t.Errorf("%s: the function = (%q, %v); want SQLSTATE 23505 on constraint m1_caller_row_acts_for_the_user", c.what, word, err)
		}
		if err := ens.EnsureCaller(e.ctx, user); !errors.Is(err, tenant.ErrM1CallerConflict) {
			t.Errorf("%s: EnsureCaller = %v; want errors.Is(ErrM1CallerConflict)", c.what, err)
		}
		if got := e.rowsOfDigest(d); fmt.Sprint(got) != fmt.Sprint(planted) || e.total() != total {
			t.Errorf("%s: the refused call changed the table: %v -> %v, total %d -> %d", c.what, planted, got, total, e.total())
		}
	}
	// the control: no planted row, the same function succeeds and the row has the owner
	user := e.newUser()
	if err := ens.EnsureCaller(e.ctx, user); err != nil {
		t.Fatalf("control: %v", err)
	}
	if n := tenCount(t, e.admin, "SELECT count(*) FROM verify.api_callers WHERE key_sha256 = $1 AND user_id = $2", e.digest(user), user); n != 1 {
		t.Fatalf("control: the row of a user with no planted row has no owner or is missing (%d)", n)
	}
}

// TestM1LiveProvision_TheOperatorsWriterAndTheFunctionRegisterTheSameRow: tenant.ProvisionM1Caller (the privileged writer of the operator tools) and the function agree on what a user's row is: whichever
// goes first, the other finds it, judges it fine and adds nothing. Run on the real table, as the superuser for the operator's writer and as platform_app for the function.
func TestM1LiveProvision_TheOperatorsWriterAndTheFunctionRegisterTheSameRow(t *testing.T) {
	e := newM1pEnv(t)
	ens := e.ensurer(e.res, 0)
	a, b := e.newUser(), e.newUser()
	// the function first, then the operator's writer
	if err := ens.EnsureCaller(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	rowsA := e.rows(a)
	if err := e.res.ProvisionM1Caller(e.ctx, e.admin, a); err != nil {
		t.Fatalf("the operator's writer does not accept the row the function wrote: %v", err)
	}
	if got := e.rows(a); fmt.Sprint(got) != fmt.Sprint(rowsA) || len(got) != 1 {
		t.Fatalf("the operator's writer added or changed a row after the function: %v -> %v", rowsA, got)
	}
	// the operator's writer first, then the function
	if err := e.res.ProvisionM1Caller(e.ctx, e.admin, b); err != nil {
		t.Fatal(err)
	}
	rowsB := e.rows(b)
	if word, err := e.call(b, e.digest(b)); err != nil || word != "exists" {
		t.Fatalf("the function after the operator's writer = %q, %v; want 'exists'", word, err)
	}
	if got := e.rows(b); fmt.Sprint(got) != fmt.Sprint(rowsB) || len(got) != 1 {
		t.Fatalf("the function added or changed a row after the operator's writer: %v -> %v", rowsB, got)
	}
}

// ---- who may execute it, and what it is ----

type m1pQ interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func m1pStrings(ctx context.Context, q m1pQ, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// m1pAclProblems says who can execute the function, in sentences; none means exactly the owner and platform_app hold EXECUTE, nobody holds it with grant option, and no other role of the image, directly or through
// a membership, can call it. The instrument is the catalog (aclexplode) and has_function_privilege for the roles that matter, so a grant to PUBLIC and a grant to a role both show.
func m1pAclProblems(ctx context.Context, q m1pQ) ([]string, error) {
	var v []string
	acl, err := m1pStrings(ctx, q, `SELECT (CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END) || ':' || a.privilege_type || ':' || a.is_grantable::text
		FROM pg_proc p CROSS JOIN LATERAL aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a WHERE p.oid = `+m1pFnReg+` ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	if got, want := strings.Join(acl, ", "), "platform_app:EXECUTE:false, verify_admin:EXECUTE:false"; got != want {
		v = append(v, fmt.Sprintf("the privileges on %s are [%s], want exactly [%s] (the owner and platform_app, nobody WITH GRANT OPTION)", m1pFn, got, want))
	}
	for _, role := range []string{"platform_app", "verify_app", "anon", "authenticated", "service_role", "platform_admin", "platform_owner"} {
		var exists, can bool
		if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1::name), COALESCE((SELECT has_function_privilege(r.oid, p.oid, 'EXECUTE') FROM pg_roles r, pg_proc p WHERE r.rolname = $1::name AND p.oid = "+m1pFnReg+"), false)", role).Scan(&exists, &can); err != nil {
			return nil, err
		}
		switch {
		case !exists:
			v = append(v, fmt.Sprintf("role %s does not exist: this is not the Supabase image the install is written for, and the check could not look at it", role))
		case role == "platform_app" && !can:
			v = append(v, "platform_app cannot execute "+m1pFn+": the service could not register a user")
		case role != "platform_app" && can:
			v = append(v, fmt.Sprintf("role %s can execute %s, and only platform_app may", role, m1pFn))
		}
	}
	return v, nil
}

// m1pShapeProblems says whether the function is what db/install/postmodule1.sql builds, in sentences: schema platform, owner verify_admin (not a superuser, not BYPASSRLS), SECURITY DEFINER, volatile, search_path
// pinned to pg_catalog, pg_temp, text result, and a body that INSERTs into verify.api_callers and does nothing else to any table; the schema's privileges (platform_app USAGE and no CREATE, verify_admin neither).
func m1pShapeProblems(ctx context.Context, q m1pQ) ([]string, error) {
	var v []string
	var n int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname = 'provision_m1_caller'").Scan(&n); err != nil {
		return nil, err
	}
	if n != 1 {
		v = append(v, fmt.Sprintf("schema platform holds %d functions named provision_m1_caller, want exactly 1 (an overload would be a second, unreviewed door)", n))
	}
	var owner, args, ret, volatile, src string
	var secdef bool
	var cfg []string
	var super, bypass bool
	if err := q.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner), pg_get_function_arguments(p.oid), p.prorettype::regtype::text, p.provolatile::text, p.prosecdef, COALESCE(p.proconfig, ARRAY[]::text[]), p.prosrc,
		(SELECT rolsuper FROM pg_roles WHERE oid = p.proowner), (SELECT rolbypassrls FROM pg_roles WHERE oid = p.proowner)
		FROM pg_proc p WHERE p.oid = `+m1pFnReg).Scan(&owner, &args, &ret, &volatile, &secdef, &cfg, &src, &super, &bypass); err != nil {
		return nil, err
	}
	if owner != "verify_admin" {
		v = append(v, fmt.Sprintf("the function is owned by %s, want verify_admin (the role that may INSERT into module 1's table and nothing more)", owner))
	}
	if super || bypass {
		v = append(v, fmt.Sprintf("the function's owner is a superuser (%v) or BYPASSRLS (%v): a SECURITY DEFINER function would run with far more than one INSERT", super, bypass))
	}
	if !secdef {
		v = append(v, "the function is not SECURITY DEFINER: platform_app has no right on module 1's table, so it would fail for the service (and, if it ever worked, run with the caller's rights)")
	}
	if !m1pContains(cfg, "search_path=pg_catalog, pg_temp") {
		v = append(v, fmt.Sprintf("the function does not pin search_path = pg_catalog, pg_temp (proconfig %v): a SECURITY DEFINER function without it can be hijacked by an object the caller shadows", cfg))
	}
	if volatile != "v" || ret != "text" || args != "p_user_id uuid, p_key_sha256 bytea" {
		v = append(v, fmt.Sprintf("the function is (%s) returns %s volatility %s, want (p_user_id uuid, p_key_sha256 bytea) returns text volatile", args, ret, volatile))
	}
	if n := len(regexp.MustCompile(`(?i)\binsert\s+into\s+verify\.api_callers\b`).FindAllString(src, -1)); n != 1 {
		v = append(v, fmt.Sprintf("the function body holds %d INSERT INTO verify.api_callers, want exactly 1", n))
	}
	if m := regexp.MustCompile(`(?i)\b(update|delete|truncate|merge|copy|grant|revoke|alter|drop|create|execute)\b`).FindAllString(src, -1); len(m) > 0 {
		v = append(v, fmt.Sprintf("the function body mentions %v: it may write one kind of row and nothing else", m))
	}
	for _, r := range []struct {
		role, priv string
		want       bool
	}{
		{"platform_app", "USAGE", true}, {"platform_app", "CREATE", false}, {"verify_admin", "USAGE", false}, {"verify_admin", "CREATE", false},
	} {
		var got bool
		if err := q.QueryRow(ctx, "SELECT has_schema_privilege($1::name, 'platform'::text, $2::text)", r.role, r.priv).Scan(&got); err != nil {
			return nil, err
		}
		if got != r.want {
			v = append(v, fmt.Sprintf("%s holds %s on schema platform = %v, want %v (the install grants verify_admin both for the length of its transaction and takes them back)", r.role, r.priv, got, r.want))
		}
	}
	return v, nil
}

func m1pContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestM1LiveProvision_ExecuteIsHeldByPlatformAppAndNobodyElse: the catalog says the function's privileges are exactly the owner's and platform_app's, with no grant option, and has_function_privilege says that
// PUBLIC (through anon, authenticated, service_role and verify_app, every role the image's default privileges could have reached) and the platform's own other roles cannot call it. The paired controls: the
// instrument says TRUE for platform_app, and each breakage below (made in a rolled-back transaction as the superuser: a grant to PUBLIC, to anon, to verify_app, to platform_admin, WITH GRANT OPTION) is
// reported by name, so the checker can say no.
func TestM1LiveProvision_ExecuteIsHeldByPlatformAppAndNobodyElse(t *testing.T) {
	e := newM1pEnv(t)
	v, err := m1pAclProblems(e.ctx, e.admin)
	if err != nil {
		t.Fatalf("the checker could not run: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("the function's privileges are wrong:\n  %s", strings.Join(v, "\n  "))
	}
	var can bool
	if err := e.admin.QueryRow(e.ctx, "SELECT has_function_privilege('platform_app'::name, "+m1pFnReg+", 'EXECUTE')").Scan(&can); err != nil || !can {
		t.Fatalf("control: has_function_privilege says platform_app cannot execute the function (%v, err %v): the instrument cannot say yes", can, err)
	}
	for _, c := range []struct{ what, grant, want string }{
		{"a grant to PUBLIC", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO PUBLIC", "PUBLIC:EXECUTE"},
		{"a grant to anon", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO anon", "role anon can execute"},
		{"a grant to authenticated", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO authenticated", "role authenticated can execute"},
		{"a grant to service_role", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO service_role", "role service_role can execute"},
		{"a grant to verify_app", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO verify_app", "role verify_app can execute"},
		{"a grant to platform_admin", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO platform_admin", "role platform_admin can execute"},
		{"platform_app's grant WITH GRANT OPTION", "GRANT EXECUTE ON FUNCTION " + m1pFn + " TO platform_app WITH GRANT OPTION", "platform_app:EXECUTE:true"},
		{"platform_app's grant taken away", "REVOKE EXECUTE ON FUNCTION " + m1pFn + " FROM platform_app", "platform_app cannot execute"},
	} {
		tx := shapeRWTx(t, liveAdmin(t))
		if _, err := tx.Exec(e.ctx, c.grant); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		got, err := m1pAclProblems(e.ctx, tx)
		_ = tx.Rollback(e.ctx)
		if err != nil {
			t.Fatalf("%s: the checker could not run: %v", c.what, err)
		}
		if !strings.Contains(strings.Join(got, "\n"), c.want) {
			t.Errorf("%s: the checker reported %v, want a problem containing %q: it cannot fail", c.what, got, c.want)
		}
	}
	if v, err := m1pAclProblems(e.ctx, e.admin); err != nil || len(v) != 0 {
		t.Fatalf("after the rolled-back breakages the function's privileges read %v (err %v): a breakage was not rolled back", v, err)
	}
}

// TestM1LiveProvision_TheFunctionIsASecurityDefinerOfTheRightOwnerWithAPinnedPathAndOneInsert: the catalog says it is owned by verify_admin (not a superuser), SECURITY DEFINER, with search_path pinned to
// pg_catalog, pg_temp, in schema platform, with the two-argument signature and a text result; that its body holds one INSERT into verify.api_callers and no other writing word; that platform_app can use
// schema platform and not create in it, and that verify_admin holds neither right on it after the install (it needs both only while the install creates the function). Each breakage below, made in a rolled-back
// transaction, is reported by name.
func TestM1LiveProvision_TheFunctionIsASecurityDefinerOfTheRightOwnerWithAPinnedPathAndOneInsert(t *testing.T) {
	e := newM1pEnv(t)
	v, err := m1pShapeProblems(e.ctx, e.admin)
	if err != nil {
		t.Fatalf("the checker could not run: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("the function is not what the install builds:\n  %s", strings.Join(v, "\n  "))
	}
	for _, c := range []struct{ what, sql, want string }{
		{"SECURITY INVOKER", "ALTER FUNCTION " + m1pFn + " SECURITY INVOKER", "not SECURITY DEFINER"},
		{"the pinned path reset", "ALTER FUNCTION " + m1pFn + " RESET search_path", "does not pin search_path"},
		{"another search_path", "ALTER FUNCTION " + m1pFn + " SET search_path = public", "does not pin search_path"},
		{"owned by platform_owner", "ALTER FUNCTION " + m1pFn + " OWNER TO platform_owner", "is owned by platform_owner"},
		{"owned by a superuser", "ALTER FUNCTION " + m1pFn + " OWNER TO supabase_admin", "owner is a superuser"},
		{"a second overload", "CREATE FUNCTION platform.provision_m1_caller(uuid) RETURNS text LANGUAGE sql AS $$ SELECT 'x' $$", "2 functions named provision_m1_caller"},
		{"verify_admin keeps CREATE on schema platform", "GRANT CREATE ON SCHEMA platform TO verify_admin", "verify_admin holds CREATE on schema platform"},
		{"verify_admin keeps USAGE on schema platform", "GRANT USAGE ON SCHEMA platform TO verify_admin", "verify_admin holds USAGE on schema platform"},
		{"platform_app may create in schema platform", "GRANT CREATE ON SCHEMA platform TO platform_app", "platform_app holds CREATE on schema platform"},
		{"a body that also deletes", "CREATE OR REPLACE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$ BEGIN INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('x', p_key_sha256, p_user_id); DELETE FROM verify.api_callers WHERE true; RETURN 'created'; END $f$", "mentions"},
	} {
		tx := shapeRWTx(t, liveAdmin(t))
		if _, err := tx.Exec(e.ctx, c.sql); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		got, err := m1pShapeProblems(e.ctx, tx)
		_ = tx.Rollback(e.ctx)
		if err != nil {
			t.Fatalf("%s: the checker could not run: %v", c.what, err)
		}
		if !strings.Contains(strings.Join(got, "\n"), c.want) {
			t.Errorf("%s: the checker reported %v, want a problem containing %q: it cannot fail", c.what, got, c.want)
		}
	}
	if v, err := m1pShapeProblems(e.ctx, e.admin); err != nil || len(v) != 0 {
		t.Fatalf("after the rolled-back breakages the function reads %v (err %v): a breakage was not rolled back", v, err)
	}
}

// ---- two callers at once ----

// TestM1LiveProvision_TwoEnsurersAtOnceBothSucceedAndLeaveOneRow: the same user, ensured by two ensurers with empty memories (two instances, or a restart) at once. THE INTERLEAVE IS FORCED: a superuser
// transaction holds the user's row UNCOMMITTED, the call from platform_app is proven BLOCKED on it (pg_stat_activity: a backend of platform_app, waiting on a Lock, running the function) and has not returned; when
// that transaction commits the call returns 'exists' (it neither failed on the duplicate nor wrote a second row); when it rolls back instead (the paired control) the same blocked call goes on to write the row
// itself and returns 'created': so it really was waiting on the other writer. Then eight callers at once on a fresh user (NOT forced: a burst, with a count assertion): all succeed, one row.
func TestM1LiveProvision_TwoEnsurersAtOnceBothSucceedAndLeaveOneRow(t *testing.T) {
	e := newM1pEnv(t)
	for _, commit := range []bool{true, false} {
		user := e.newUser()
		d := e.digest(user)
		holder := dial(t, e.adminCS, nil) // its own session: the in-flight inserter
		tx, err := holder.BeginTx(e.ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.ctx, `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id) VALUES ($1, $2, now() - interval '1 hour', $3::uuid)`, tenant.M1CallerID(user), d, user); err != nil {
			t.Fatalf("the holder's INSERT: %v", err)
		}
		type result struct {
			word string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			w, err := e.call(user, d)
			done <- result{w, err}
		}()
		blocked := false
		for i := 0; i < 200 && !blocked; i++ {
			select {
			case r := <-done:
				_ = tx.Rollback(e.ctx)
				t.Fatalf("commit=%v: the call returned (%q, %v) while another transaction held the same row uncommitted: nothing forced the interleave", commit, r.word, r.err)
			default:
			}
			var n int
			if err := e.admin.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename = 'platform_app' AND datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%provision_m1_caller%'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			blocked = n >= 1
			if !blocked {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if !blocked {
			_ = tx.Rollback(e.ctx)
			t.Fatalf("commit=%v: no platform_app backend running the function was seen waiting on a lock after 20 s: the interleave was not forced", commit)
		}
		var want string
		if commit {
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			want = "exists"
		} else {
			if err := tx.Rollback(e.ctx); err != nil {
				t.Fatal(err)
			}
			want = "created"
		}
		select {
		case r := <-done:
			if r.err != nil || r.word != want {
				t.Errorf("commit=%v: the blocked call returned (%q, %v) once the other transaction ended; want (%q, nil)", commit, r.word, r.err, want)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("commit=%v: the blocked call did not return 30 s after the other transaction ended", commit)
		}
		if rows := e.rows(user); len(rows) != 1 {
			t.Errorf("commit=%v: %d row(s) for the user, want exactly 1: %v", commit, len(rows), rows)
		}
	}

	// the burst (not forced): eight callers, two ensurers, one fresh user
	user := e.newUser()
	ensA, ensB := e.ensurer(e.res, 0), e.ensurer(e.res, 0)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		ens := ensA
		if i%2 == 1 {
			ens = ensB
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ens.EnsureCaller(e.ctx, user)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent EnsureCaller failed: %v", err)
		}
	}
	if rows := e.rows(user); len(rows) != 1 {
		t.Errorf("the burst left %d row(s) for the user, want exactly 1: %v", len(rows), rows)
	}
}
