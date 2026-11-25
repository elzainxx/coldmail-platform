//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/norm"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// The suppression store (internal/suppress) against a REAL PostgreSQL: Phase 4, Part A item A2. The table's CHECK cannot decode punycode, so for two address shapes the Go validator is the ONLY wall,
// and the owner's ruling is that no code writes platform.suppressions except suppress.Add, which validates first (scripts/check-suppression-writers.sh keeps it so). The unit tests of the package
// use a fake connection and prove the decisions made before the database; THESE tests prove the facts only a real database shows: what is stored, what the CHECK itself still refuses and how that
// is typed, that nothing refused reaches the table, that a duplicate is "not created", and that Filter is scoped to one user and works in batches.
//
// Shape: a migrated scratch database of its own per test, a SUPERUSER connection that seeds and observes (never the code under test), and the store running over a pool of REAL logins as
// platform_app (mnyPool), so it holds exactly the privileges the service has. Every refusal test has its paired control: the repaired input is stored, so the refusal is the input's doing and
// not the environment's. The raw INSERT statements below are TESTS (the gate reads no test file) and are how a test shows what the database does on its own.
// Every name here starts with `supst` so it cannot clash with another file's helpers.

const (
	supstUserA  = "5a000000-0000-4000-8000-00000000000a"
	supstUserB  = "5b000000-0000-4000-8000-00000000000b"
	supstNobody = "5f000000-0000-4000-8000-0000000000ff" // a well-formed id that no table holds
	supstSystem = "00000000-0000-0000-0000-000000000000"
)

type supstEnv struct {
	t     *testing.T
	admin *pgx.Conn     // a superuser connection to the scratch database: seeding and observing
	pool  *pgxpool.Pool // real platform_app logins: what the store runs on
}

func newSupstEnv(t *testing.T) *supstEnv {
	t.Helper()
	s, admin := migratedScratch(t)
	e := &supstEnv{t: t, admin: admin, pool: mnyPool(t, s.DSN)}
	for _, u := range []struct{ id, email string }{{supstUserA, "supst-a@example.test"}, {supstUserB, "supst-b@example.test"}} {
		e.exec("INSERT INTO auth.users (id, email) VALUES ($1, $2)", u.id, u.email)
		e.exec("INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", u.id, u.email)
	}
	return e
}

func (e *supstEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.admin.Exec(testCtx(e.t), sql, args...); err != nil {
		e.t.Fatalf("setup statement failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
}

// total is the number of rows of the whole table, seen by the superuser.
func (e *supstEnv) total() int64 {
	e.t.Helper()
	var n int64
	if err := e.admin.QueryRow(testCtx(e.t), "SELECT count(*) FROM platform.suppressions").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// rows is "email|source" of every row of the user, sorted.
func (e *supstEnv) rows(user string) []string {
	e.t.Helper()
	rs, err := e.admin.Query(testCtx(e.t), "SELECT email || '|' || source FROM platform.suppressions WHERE user_id = $1 ORDER BY email", user)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rs.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// holds reports how many rows, of any user, carry one of the spellings.
func (e *supstEnv) holds(spellings ...string) int64 {
	e.t.Helper()
	var n int64
	// a spelling with a NUL cannot be a parameter of a text comparison (the database refuses it), and cannot be stored either, so it is left out
	var askable []string
	for _, s := range spellings {
		if !strings.ContainsRune(s, 0) {
			askable = append(askable, s)
		}
	}
	if err := e.admin.QueryRow(testCtx(e.t), "SELECT count(*) FROM platform.suppressions WHERE email = ANY($1::text[])", askable).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// supstAddOK adds and requires success and Created.
func supstAddOK(t *testing.T, q suppress.Execer, user, email, source string) suppress.Result {
	t.Helper()
	res, err := suppress.Add(testCtx(t), q, user, email, source)
	if err != nil {
		t.Fatalf("Add(%q) failed: %v", email, err)
	}
	if !res.Created {
		t.Fatalf("Add(%q) reported Created=false for an address the user had not suppressed", email)
	}
	return res
}

// supstCounter counts the queries a Filter call makes.
type supstCounter struct {
	inner suppress.Querier
	n     int
}

func (c *supstCounter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.inner.QueryRow(ctx, sql, args...)
}

func supstSet(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func supstKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func supstEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSuppressStore_StoresTheNormalisedFormOfAMessyInput: Add stores what the validator returned, never what the caller typed. A messy input (surrounding white space, upper case, an IDN domain, a
// trailing dot) is stored in the platform's one address form, the raw spellings are in no row, and the database stamps created_at itself. CONTROLS: an address already in the form is stored
// byte for byte, and each of the three sources of the vocabulary is accepted. It fails if Add inserts its argument instead of the validator's output.
func TestSuppressStore_StoresTheNormalisedFormOfAMessyInput(t *testing.T) {
	e := newSupstEnv(t)
	cases := []struct{ name, raw, want string }{
		{"white space, upper case, +tag", "  Jane.DOE+Tag@EXAMPLE.com\t", "jane.doe+tag@example.com"},
		{"an IDN domain is stored as punycode", "User@Exämple.COM", "user@xn--exmple-cua.com"},
		{"a trailing dot of the domain is removed", "Trail@Example.org.", "trail@example.org"},
		{"CONTROL: an address already in the form is stored byte for byte", "plain@example.net", "plain@example.net"},
	}
	for i, c := range cases {
		src := []string{suppress.SourceManual, suppress.SourceImport, suppress.SourceUnsubscribe, suppress.SourceManual}[i]
		res := supstAddOK(t, e.pool, supstUserA, c.raw, src)
		if res.Email != c.want {
			t.Errorf("%s: Result.Email is %q, want %q", c.name, res.Email, c.want)
		}
		if got := e.holds(c.want); got != 1 {
			t.Errorf("%s: the table holds %d row(s) of %q, want exactly 1", c.name, got, c.want)
		}
		if c.raw != c.want {
			if got := e.holds(c.raw, strings.TrimSpace(c.raw)); got != 0 {
				t.Errorf("%s: the table holds the RAW spelling %q (%d row(s)): Add must store the validator's output, not its argument", c.name, c.raw, got)
			}
		}
	}
	want := []string{"jane.doe+tag@example.com|manual", "plain@example.net|manual", "trail@example.org|unsubscribe", "user@xn--exmple-cua.com|import"}
	if got := e.rows(supstUserA); !supstEqual(got, want) {
		t.Errorf("the user's rows are %v, want %v (email|source)", got, want)
	}
	var stamped int64
	if err := e.admin.QueryRow(testCtx(t), "SELECT count(*) FROM platform.suppressions WHERE created_at IS NOT NULL AND created_at <= now()").Scan(&stamped); err != nil || stamped != int64(len(cases)) {
		t.Errorf("%d row(s) carry the database's created_at, want %d (err %v)", stamped, len(cases), err)
	}
	if got := e.total(); got != int64(len(cases)) {
		t.Errorf("the table holds %d rows, want %d: Add wrote something besides the four addresses", got, len(cases))
	}
}

// TestSuppressStore_RefusesAnInvalidAddressBeforeAnySQLAndNothingReachesTheTable: for each way an address fails the platform's rule (empty, no @, two @, a 65-byte local part, a domain with no dot, and
// the two invalid-punycode shapes the table's CHECK cannot see) Add returns the typed refusal with its norm reason, the table is unchanged, and a connection that is DEAD (nothing listens) gives the
// SAME answer: the refusal was decided before any SQL. CONTROLS: the valid sibling of each is stored; on the dead connection a valid address fails with a connection error that is not a refusal; and
// the two punycode shapes ARE accepted by the database when written to it directly, which is exactly why the validator is the only wall for them. It fails if Add stops validating (the dead
// connection would then answer with a connection error), if a refused address is written, or if the table starts to see the punycode shapes (update Docs/install.md, "One address rule", and revisit
// scripts/check-suppression-writers.sh).
func TestSuppressStore_RefusesAnInvalidAddressBeforeAnySQLAndNothingReachesTheTable(t *testing.T) {
	e := newSupstEnv(t)
	ctx := testCtx(t)
	refused := []struct {
		name, in string
		reason   norm.Reason
	}{
		{"empty", "", norm.ReasonEmpty},
		{"no @", "jane.example.com", norm.ReasonNoAt},
		{"two @", "a@b@example.com", norm.ReasonMultipleAt},
		{"a 65-byte local part", strings.Repeat("a", 65) + "@example.com", norm.ReasonLocalTooLong},
		{"a domain with no dot", "jane@localhost", norm.ReasonDomainNoDot},
		{"an invalid punycode label (xn--a)", "jane@xn--a.com", norm.ReasonBadIDNA},
		{"punycode of ascii garbage (xn--zzzzzzzz)", "jane@xn--zzzzzzzz.com", norm.ReasonBadIDNA},
	}
	dead := mnyDeadPool(t)
	for _, c := range refused {
		before := e.total()
		res, err := suppress.Add(ctx, e.pool, supstUserA, c.in, suppress.SourceManual)
		var ia *suppress.InvalidAddressError
		switch {
		case err == nil:
			t.Errorf("%s: Add accepted %q (%+v)", c.name, c.in, res)
		case !errors.Is(err, suppress.ErrInvalidAddress) || !errors.Is(err, suppress.ErrInvalid) || !errors.As(err, &ia) || ia.Reason != c.reason || !norm.Rejected(err):
			t.Errorf("%s: %v, want an InvalidAddressError with reason %q matching ErrInvalidAddress and ErrInvalid", c.name, err, c.reason)
		}
		if res != (suppress.Result{}) {
			t.Errorf("%s: a refusal returned %+v", c.name, res)
		}
		if err != nil && (strings.Contains(err.Error(), strings.TrimSpace(c.in)) && strings.TrimSpace(c.in) != "") {
			t.Errorf("%s: the error text %q carries the address", c.name, err.Error())
		}
		if after := e.total(); after != before {
			t.Errorf("%s: the table holds %d row(s) after the refusal, had %d: a refused address was written", c.name, after, before)
		}
		if got := e.holds(c.in, strings.ToLower(c.in), strings.TrimSpace(c.in)); got != 0 {
			t.Errorf("%s: the refused address %q IS in the table", c.name, c.in)
		}
		// the same refusal on a connection that cannot work: no SQL was needed to decide it
		start := time.Now()
		if _, derr := suppress.Add(ctx, dead, supstUserA, c.in, suppress.SourceManual); !errors.Is(derr, suppress.ErrInvalidAddress) {
			t.Errorf("%s: on a dead connection Add answered %v, want ErrInvalidAddress: the validator must decide before any SQL", c.name, derr)
		} else if time.Since(start) > 2*time.Second {
			t.Errorf("%s: the refusal took %s on a dead connection: it must not have tried to connect", c.name, time.Since(start))
		}
	}
	if got := e.total(); got != 0 {
		t.Fatalf("after every refusal the table holds %d row(s), want 0", got)
	}
	// CONTROL 1: the dead connection really is dead, and a valid address then fails with a connection error, not a refusal
	if _, err := suppress.Add(ctx, dead, supstUserA, "jane@example.com", suppress.SourceManual); err == nil || errors.Is(err, suppress.ErrInvalid) {
		t.Errorf("control: a valid address on the dead connection gave %v, want a connection error that is not an invalid request", err)
	}
	// CONTROL 2: the valid sibling of each refusal is stored
	siblings := []string{"jane@example.com", "jane.example@example.com", "a@b.example.com", strings.Repeat("a", 64) + "@example.com", "jane@localhost.test", "jane@xn--exmple-cua.com", "jane@xn--bcher-kva.example"}
	for _, s := range siblings {
		supstAddOK(t, e.pool, supstUserA, s, suppress.SourceManual)
	}
	if got := e.total(); got != int64(len(siblings)) {
		t.Errorf("the table holds %d row(s), want the %d valid siblings", got, len(siblings))
	}
	// CONTROL 3: written DIRECTLY, the database accepts the two punycode shapes (its CHECK cannot decode punycode): the validator is the only wall. Rolled back.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, raw := range []string{"jane@xn--a.com", "jane@xn--zzzzzzzz.com"} {
		if _, err := tx.Exec(ctx, "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')", supstUserB, raw); err != nil {
			t.Fatalf("the database REFUSED %q written directly (%v): the validator is no longer the only wall for this shape. Update Docs/install.md (\"One address rule\"), the divergence table of norm_agreement_test.go and the comment of platform_0009, then decide whether scripts/check-suppression-writers.sh is still needed", raw, err)
		}
	}
}

// TestSuppressStore_ADuplicateIsNotCreatedAndLeavesOneRow: a second Add of an address the user already suppresses (in another spelling, with another source) is not an error: it reports
// Created=false, leaves exactly one row, and the first row stands (its source included). CONTROLS: a different address creates a second row, and the same address for ANOTHER user creates its own
// row (the key is the pair). It fails if Add errors on a duplicate, overwrites the first row, or treats the address as global.
func TestSuppressStore_ADuplicateIsNotCreatedAndLeavesOneRow(t *testing.T) {
	e := newSupstEnv(t)
	ctx := testCtx(t)
	first := supstAddOK(t, e.pool, supstUserA, "Dup@Example.com", suppress.SourceManual)
	dup, err := suppress.Add(ctx, e.pool, supstUserA, "  dup@EXAMPLE.COM ", suppress.SourceImport)
	if err != nil {
		t.Fatalf("a duplicate Add failed: %v", err)
	}
	if dup.Created || dup.Email != first.Email {
		t.Errorf("the duplicate reported %+v, want Created=false and Email %q", dup, first.Email)
	}
	if got := e.rows(supstUserA); !supstEqual(got, []string{"dup@example.com|manual"}) {
		t.Errorf("the user's rows are %v, want exactly the first row, source manual", got)
	}
	// controls
	if r := supstAddOK(t, e.pool, supstUserA, "other@example.com", suppress.SourceManual); r.Email != "other@example.com" {
		t.Errorf("control: %+v", r)
	}
	supstAddOK(t, e.pool, supstUserB, "dup@example.com", suppress.SourceUnsubscribe)
	if got := e.rows(supstUserA); !supstEqual(got, []string{"dup@example.com|manual", "other@example.com|manual"}) {
		t.Errorf("control: the user's rows are %v", got)
	}
	if got := e.rows(supstUserB); !supstEqual(got, []string{"dup@example.com|unsubscribe"}) {
		t.Errorf("control: the other user's rows are %v", got)
	}
}

// TestSuppressStore_TheTablesOwnRefusalsAreTypedAndNeverCarryTheAddress: what the validator accepts and the table still refuses is ErrCannotStore (a space in the local part, a leading dot in the
// domain, an address over 254 bytes: SQLSTATE 23514 on suppressions_email_norm_v1; a NUL: SQLSTATE 22021), the system user is ErrSystemUser, an unknown user is ErrUnknownUser, a source outside the
// vocabulary is ErrBadSource; none of them is an "invalid request", none wraps the driver's error, and none carries the address, although the driver's own error DOES (the control reads it). Nothing
// refused is in the table. CONTROLS: the repaired input is stored, and a known user is accepted. It fails if a refusal is not typed, leaks the row, or is mapped from the message text.
func TestSuppressStore_TheTablesOwnRefusalsAreTypedAndNeverCarryTheAddress(t *testing.T) {
	e := newSupstEnv(t)
	ctx := testCtx(t)
	longDomain := strings.TrimSuffix(strings.Repeat(strings.Repeat("b", 50)+".", 5), ".") // 254 bytes: the validator allows 255, the table caps the whole address at 254
	cannot := []struct{ name, in, repaired string }{
		{"white space in the local part", "ja ne@example.com", "jane@example.com"},
		{"a leading dot in the domain", "jane@.example", "jane@example.org"},
		{"an address over 254 bytes", "jane@" + longDomain, "jane@example.net"},
		{"a NUL in the local part", "ja\x00ne@example.com", "jane@example.io"},
	}
	for _, c := range cannot {
		if _, verr := norm.Validate(c.in); verr != nil {
			t.Fatalf("%s: the validator refuses %q (%v): this case is for what the validator ACCEPTS and the table refuses", c.name, c.in, verr)
		}
		before := e.total()
		res, err := suppress.Add(ctx, e.pool, supstUserA, c.in, suppress.SourceManual)
		var pe *pgconn.PgError
		switch {
		case err == nil:
			t.Errorf("%s: Add stored %q (%+v)", c.name, c.in, res)
		case !errors.Is(err, suppress.ErrCannotStore):
			t.Errorf("%s: %v, want ErrCannotStore", c.name, err)
		case errors.Is(err, suppress.ErrInvalid) || errors.Is(err, suppress.ErrInvalidAddress):
			t.Errorf("%s: %v reads as an invalid request, but the address is valid", c.name, err)
		case errors.As(err, &pe):
			t.Errorf("%s: the error wraps the driver's *PgError, whose Detail quotes the row", c.name)
		case strings.Contains(err.Error(), "example") || strings.Contains(err.Error(), "bbbbb") || strings.Contains(err.Error(), "jane"):
			t.Errorf("%s: the error text %q carries the address", c.name, err.Error())
		}
		if got := e.total(); got != before {
			t.Errorf("%s: the table changed (%d rows, had %d)", c.name, got, before)
		}
		if got := e.holds(c.in, strings.ToLower(c.in)); got != 0 {
			t.Errorf("%s: the refused address IS in the table", c.name)
		}
		supstAddOK(t, e.pool, supstUserA, c.repaired, suppress.SourceManual) // CONTROL: the repaired input is stored
	}
	// the driver's own error carries the row: this is what the typed error must not repeat
	_, rawErr := e.pool.Exec(ctx, "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')", supstUserA, "ja ne@example.com")
	var raw *pgconn.PgError
	if !errors.As(rawErr, &raw) || raw.Code != "23514" || raw.ConstraintName != "suppressions_email_norm_v1" || !strings.Contains(raw.Detail, "ja ne@example.com") {
		t.Errorf("control: the driver's error for a direct insert is %v (detail %q), want SQLSTATE 23514 on suppressions_email_norm_v1 with the row in its Detail", rawErr, func() string {
			if raw != nil {
				return raw.Detail
			}
			return ""
		}())
	}
	// the system user, an unknown user, a bad source
	before := e.total()
	if _, err := suppress.Add(ctx, e.pool, supstSystem, "sys@example.com", suppress.SourceManual); !errors.Is(err, suppress.ErrSystemUser) || errors.As(err, &raw) {
		t.Errorf("the system user: %v, want ErrSystemUser", err)
	}
	if _, err := suppress.Add(ctx, e.pool, supstNobody, "ghost@example.com", suppress.SourceManual); !errors.Is(err, suppress.ErrUnknownUser) || errors.As(err, &raw) {
		t.Errorf("an unknown user: %v, want ErrUnknownUser", err)
	}
	if _, err := suppress.Add(ctx, e.pool, supstUserA, "src@example.com", "bounce"); !errors.Is(err, suppress.ErrBadSource) || !errors.Is(err, suppress.ErrInvalid) {
		t.Errorf("a source outside the vocabulary: %v, want ErrBadSource (an ErrInvalid)", err)
	}
	if got := e.total(); got != before {
		t.Errorf("the system user, the unknown user and the bad source changed the table (%d rows, had %d)", got, before)
	}
	supstAddOK(t, e.pool, supstUserB, "sys@example.com", suppress.SourceManual) // CONTROL: the same address for a known user
	// the database's own wall for the vocabulary, written directly: SQLSTATE 23514 on suppressions_source_vocabulary
	_, srcErr := e.pool.Exec(ctx, "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, 'v@example.com', 'bounce')", supstUserA)
	if !errors.As(srcErr, &raw) || raw.Code != "23514" || raw.ConstraintName != "suppressions_source_vocabulary" {
		t.Errorf("a direct insert with source 'bounce': %v, want SQLSTATE 23514 on suppressions_source_vocabulary", srcErr)
	}
}

// TestSuppressStore_TheConstraintNamesTheStoreMapsExist: the store maps a database refusal by constraint NAME (never by message text). This pins each name against the migrated database, so a rename
// in a migration fails here and not silently in production as an "unexpected" error. CONTROL: a name that is not there is reported missing by the same query. It fails if a mapped constraint is
// renamed or dropped.
func TestSuppressStore_TheConstraintNamesTheStoreMapsExist(t *testing.T) {
	e := newSupstEnv(t)
	have := map[string]string{} // name -> type
	rs, err := e.admin.Query(testCtx(t), "SELECT conname, contype::text FROM pg_constraint WHERE conrelid = 'platform.suppressions'::regclass")
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var n, ty string
		if err := rs.Scan(&n, &ty); err != nil {
			t.Fatal(err)
		}
		have[n] = ty
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	for name, ty := range map[string]string{
		"suppressions_email_norm_v1":       "c",
		"suppressions_not_the_system_user": "c",
		"suppressions_source_vocabulary":   "c",
		"suppressions_user_id_fkey":        "f",
		"suppressions_pk":                  "p",
	} {
		if have[name] != ty {
			t.Errorf("platform.suppressions has no constraint %q of type %q (it has %v): internal/suppress maps that name", name, ty, have)
		}
	}
	if _, ok := have["suppressions_no_such_constraint"]; ok {
		t.Errorf("control: the query found a constraint that cannot exist")
	}
}

// TestSuppressStore_FilterReturnsExactlyTheSuppressedOfABatchAndNeverAnotherUsers: Filter, over a REAL table, asks about 2 * FilterBatchSize + 10 addresses in exactly three queries (never one per
// address) and returns exactly the user's own suppressed ones, including the ones on the batch edges; another user's suppressions of other addresses are never returned, and an address both users
// suppress is returned to both. An address the table can never hold (a NUL) is left out and does not fail its batch. CONTROLS: the same list asked for the other user gives the other answer; an address
// in another spelling is not found; and the unfiltered statement with a NUL really fails (SQLSTATE 22021), so the skip is needed. It fails if a batch edge loses an address, if a user's rows leak to
// another, or if Filter asks per address.
func TestSuppressStore_FilterReturnsExactlyTheSuppressedOfABatchAndNeverAnotherUsers(t *testing.T) {
	e := newSupstEnv(t)
	ctx := testCtx(t)
	n := 2*suppress.FilterBatchSize + 10
	asked := make([]string, n)
	for i := range asked {
		asked[i] = fmt.Sprintf("r%05d@example.com", i)
	}
	idxA := []int{0, suppress.FilterBatchSize - 1, suppress.FilterBatchSize, 2*suppress.FilterBatchSize - 1, 2 * suppress.FilterBatchSize, n - 1}
	idxB := []int{0, 1, suppress.FilterBatchSize + 1, 2*suppress.FilterBatchSize + 1} // 0 is suppressed by BOTH users
	var wantA, wantB []string
	for _, i := range idxA {
		supstAddOK(t, e.pool, supstUserA, asked[i], suppress.SourceManual)
		wantA = append(wantA, asked[i])
	}
	for _, i := range idxB {
		supstAddOK(t, e.pool, supstUserB, asked[i], suppress.SourceImport)
		wantB = append(wantB, asked[i])
	}
	wantA, wantB = supstSet(wantA), supstSet(wantB)

	for _, c := range []struct {
		name, user string
		want       []string
	}{{"user A", supstUserA, wantA}, {"user B", supstUserB, wantB}} {
		q := &supstCounter{inner: e.pool}
		got, err := suppress.Filter(ctx, q, c.user, asked)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if g := supstKeys(got); !supstEqual(g, c.want) {
			t.Errorf("%s: Filter returned %d address(es) %v, want exactly %d: %v", c.name, len(g), g, len(c.want), c.want)
		}
		if q.n != 3 {
			t.Errorf("%s: %d addresses were asked in %d queries, want 3 (batches of %d)", c.name, n, q.n, suppress.FilterBatchSize)
		}
	}
	// another user's address is never returned: user A is asked about an address only B suppressed
	if got, err := suppress.Filter(ctx, e.pool, supstUserA, []string{asked[1], asked[suppress.FilterBatchSize+1]}); err != nil || len(got) != 0 {
		t.Errorf("user A was asked about two addresses only B suppressed and got %v (err %v), want none", got, err)
	}
	// CONTROL: the same two addresses asked for B are found
	if got, err := suppress.Filter(ctx, e.pool, supstUserB, []string{asked[1], asked[suppress.FilterBatchSize+1]}); err != nil || len(got) != 2 {
		t.Errorf("control: user B asked about its own two got %v (err %v), want both", got, err)
	}
	// CONTROL: another spelling is not found (Filter takes normalised addresses and does not normalise), the normalised one is
	if got, _ := suppress.Filter(ctx, e.pool, supstUserA, []string{strings.ToUpper(asked[0])}); len(got) != 0 {
		t.Errorf("control: an upper-case spelling was found: %v", got)
	}
	// an empty list asks nothing; a list with a NUL does not fail its batch
	q := &supstCounter{inner: e.pool}
	if got, err := suppress.Filter(ctx, q, supstUserA, nil); err != nil || got == nil || len(got) != 0 || q.n != 0 {
		t.Errorf("an empty list: %v, %v, %d query(ies), want an empty map and none", got, err, q.n)
	}
	got, err := suppress.Filter(ctx, e.pool, supstUserA, []string{asked[0], "a\x00b@example.com", asked[1]})
	if err != nil || !supstEqual(supstKeys(got), []string{asked[0]}) {
		t.Errorf("a list holding a NUL address: %v, %v, want just %s", supstKeys(got), err, asked[0])
	}
	var found []string
	nulErr := e.pool.QueryRow(ctx, `SELECT COALESCE(array_agg(email), ARRAY[]::text[]) FROM platform.suppressions WHERE user_id = $1::uuid AND email = ANY($2::text[])`,
		supstUserA, []string{"a\x00b@example.com"}).Scan(&found)
	var pe *pgconn.PgError
	if !errors.As(nulErr, &pe) || pe.Code != "22021" {
		t.Errorf("control: the unfiltered statement with a NUL gave %v, want SQLSTATE 22021 (so the store must leave such an address out)", nulErr)
	}
}

// TestSuppressStore_AddAndFilterRunInsideTheCallersTransaction: Add takes a pgx.Tx as readily as the pool, and the caller owns the commit: a rolled-back transaction leaves no row, a committed one
// does, and Filter through the same transaction sees the transaction's own write while the pool does not until the commit. It fails if Add commits for itself or opens its own connection.
func TestSuppressStore_AddAndFilterRunInsideTheCallersTransaction(t *testing.T) {
	e := newSupstEnv(t)
	ctx := testCtx(t)
	const addr = "intx@example.com"

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	supstAddOK(t, tx, supstUserA, "INTX@Example.com", suppress.SourceManual)
	if got, err := suppress.Filter(ctx, tx, supstUserA, []string{addr}); err != nil || !got[addr] {
		t.Errorf("inside the transaction Filter found %v (err %v), want its own write", got, err)
	}
	if got, err := suppress.Filter(ctx, e.pool, supstUserA, []string{addr}); err != nil || len(got) != 0 {
		t.Errorf("from the pool Filter found %v (err %v) before the commit, want none", got, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.total(); got != 0 {
		t.Errorf("a rolled-back transaction left %d row(s)", got)
	}

	// CONTROL: the same, committed
	tx, err = e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	supstAddOK(t, tx, supstUserA, addr, suppress.SourceManual)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := suppress.Filter(ctx, e.pool, supstUserA, []string{addr}); err != nil || !got[addr] {
		t.Errorf("after the commit the pool's Filter found %v (err %v), want the row", got, err)
	}
	if got := e.total(); got != 1 {
		t.Errorf("the committed transaction left %d row(s), want 1", got)
	}
}
