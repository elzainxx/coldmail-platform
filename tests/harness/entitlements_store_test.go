//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/entitlements"
)

// The entitlement store against a REAL PostgreSQL: a migrated scratch database of its own per test, a SUPERUSER connection that seeds
// and observes (never the code under test), and the store running over a pool of real logins as platform_app, so it holds exactly the privileges the service has: SELECT and INSERT
// on the ledger, SELECT on the entitlements and the delivery rows, nothing that writes an entitlement. Fixtures are committed for real
// (the store runs on other connections and could not see an open transaction). Every name here starts with `ent` so it cannot clash with another file's helpers.

const (
	entAdminA = "ea000000-0000-4000-8000-00000000000a"
	entAdminB = "ea000000-0000-4000-8000-00000000000b"
	// entNobody is a well-formed id that no table holds.
	entNobody = "0e0e0e0e-0000-4000-8000-000000000000"
)

var entKeySeq atomic.Int64

// entKey is a fresh idempotency key.
func entKey(prefix string) string { return fmt.Sprintf("%s-%d", prefix, entKeySeq.Add(1)) }

// entUser is the id of customer i (0-based) of an entEnv.
func entUser(i int) string { return fmt.Sprintf("e1000000-0000-4000-8000-%012x", i) }

// entLockKey is the advisory-lock key of a user's module-3 writers, computed HERE and not imported: the contention tests hold this exact number from another
// connection, so a change of the number in the store without a change here makes TestEntStore_TheLockKeyIsThePlatformsAndTheStoresOwn fail.
func entLockKey(user string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(user) + "|m3"))
	return 7_000_000_000 + int64(h.Sum64()%1_000_000_000)
}

type entEnv struct {
	t     *testing.T
	s     scratch
	admin *pgx.Conn     // a superuser connection to the scratch database: seeding and observing
	pool  *pgxpool.Pool // real platform_app logins: what the store and the reconciler run on
	store *entitlements.Store
}

// newEntEnv migrates a scratch database with the real binary, opens the store over a platform_app pool, and seeds `customers` users (entUser(0..)) and two admins.
func newEntEnv(t *testing.T, customers int) *entEnv {
	t.Helper()
	s, admin := migratedScratch(t)
	e := &entEnv{t: t, s: s, admin: admin}
	e.pool = entPool(t, s.DSN, "ent-test-"+s.Name)
	e.store = entitlements.NewStore(e.pool)
	e.exec(`INSERT INTO auth.users (id, email) SELECT ('e1000000-0000-4000-8000-' || lpad(to_hex(g), 12, '0'))::uuid, 'ent-' || g || '@example.test' FROM generate_series(0, $1::int - 1) g`, customers)
	e.exec(`INSERT INTO public.users (id, auth_user_id, email) SELECT id, id, email FROM auth.users`)
	for _, a := range []struct{ id, email, name string }{{entAdminA, "ent-admin-a@example.test", "Ent Admin A"}, {entAdminB, "ent-admin-b@example.test", "Ent Admin B"}} {
		e.exec("INSERT INTO auth.users (id, email) VALUES ($1, $2)", a.id, a.email)
		e.exec("INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", a.id, a.email)
		e.exec("INSERT INTO platform.admins (id, display_name) VALUES ($1, $2)", a.id, a.name)
	}
	return e
}

// entPool opens a pool of real logins as platform_app on the scratch database and proves it is that identity and not a superuser. application_name lets a test pick the pool's backends out of
// pg_stat_activity.
func entPool(t *testing.T, dsn, application string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("cannot parse the scratch connection string: %s", scrubbed(err, dsn))
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "platform_app", requireEnv(t, envAppPassword)
	delete(cfg.ConnConfig.RuntimeParams, "timezone") // like rolesAppLogin: nothing from the test process may override the role's settings
	delete(cfg.ConnConfig.RuntimeParams, "options")
	cfg.ConnConfig.RuntimeParams["application_name"] = application
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open the platform_app pool: %s", scrubbed(err, dsn))
	}
	t.Cleanup(pool.Close)
	var who string
	var super bool
	if err := pool.QueryRow(testCtx(t), "SELECT session_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&who, &super); err != nil {
		t.Fatalf("the platform_app pool cannot run a query: %s", scrubbed(err, dsn))
	}
	if who != "platform_app" || super {
		t.Fatalf("the pool logged in as %q (superuser %v), want a real login as platform_app and no superuser: the code under test would not run with the service's privileges", who, super)
	}
	return pool
}

func (e *entEnv) exec(sql string, args ...any) pgconn.CommandTag {
	e.t.Helper()
	tag, err := e.admin.Exec(testCtx(e.t), sql, args...)
	if err != nil {
		e.t.Fatalf("seed/observe statement failed: %v\n  sql: %s", err, strings.Join(strings.Fields(sql), " "))
	}
	return tag
}

func (e *entEnv) int(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.admin.QueryRow(testCtx(e.t), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("query failed: %v\n  sql: %s", err, strings.Join(strings.Fields(sql), " "))
	}
	return n
}

func (e *entEnv) str(sql string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.admin.QueryRow(testCtx(e.t), sql, args...).Scan(&s); err != nil {
		e.t.Fatalf("query failed: %v\n  sql: %s", err, strings.Join(strings.Fields(sql), " "))
	}
	return s
}

// dbNow is the DATABASE's clock (clock_timestamp), the only clock a test uses to place a date.
func (e *entEnv) dbNow() time.Time {
	e.t.Helper()
	var now time.Time
	if err := e.admin.QueryRow(testCtx(e.t), "SELECT clock_timestamp()").Scan(&now); err != nil {
		e.t.Fatal(err)
	}
	return now.UTC()
}

// ledgerRows lists a user's module-3 ledger rows in order, as "kind|admin|until|reason|key" (admin and until are "-" when NULL; until is RFC 3339 with microseconds).
func (e *entEnv) m3Ledger(user string) []string {
	e.t.Helper()
	rows, err := e.admin.Query(testCtx(e.t), `SELECT kind, COALESCE(acting_admin_id::text, '-'), COALESCE(to_char(until_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), '-'), COALESCE(reason, '-'), idempotency_key
		FROM platform.ledger WHERE user_id = $1 AND module_id = 'm3' ORDER BY id`, user)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, admin, until, reason, key string
		if err := rows.Scan(&kind, &admin, &until, &reason, &key); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, strings.Join([]string{kind, admin, until, reason, key}, "|"))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *entEnv) countKind(user, kind string) int64 {
	e.t.Helper()
	return e.int("SELECT count(*) FROM platform.ledger WHERE user_id = $1 AND kind = $2", user, kind)
}

// entitlementOf reads the stored entitlement with the superuser (an independent path from the store's own Get).
type entRow struct {
	Enabled   bool
	Until     *time.Time
	ChangedBy *string
}

// String prints the values and not the pointers, so a before/after comparison of two reads is a comparison of what the rows hold.
func (r entRow) String() string {
	until, by := "-", "-"
	if r.Until != nil {
		until = r.Until.UTC().Format(time.RFC3339Nano)
	}
	if r.ChangedBy != nil {
		by = *r.ChangedBy
	}
	return fmt.Sprintf("{enabled=%v until=%s changed_by=%s}", r.Enabled, until, by)
}

func (e *entEnv) entitlementOf(user string) (entRow, bool) {
	e.t.Helper()
	var r entRow
	err := e.admin.QueryRow(testCtx(e.t), "SELECT enabled, enabled_until, changed_by::text FROM platform.m3_entitlements WHERE user_id = $1", user).Scan(&r.Enabled, &r.Until, &r.ChangedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return entRow{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return r, true
}

// syncOf reads the delivery row with the superuser.
type entSync struct {
	WantSuspended              bool
	WantVersion, ConvergedVers int64
	Observed                   string
	ObservedAt                 *time.Time
	Attempts                   int
	LastAttemptAt              *time.Time
	LastError                  *string
}

func (e *entEnv) syncOf(user string) entSync {
	e.t.Helper()
	var s entSync
	err := e.admin.QueryRow(testCtx(e.t), `SELECT want_suspended, want_version, converged_version, observed, observed_at, attempts, last_attempt_at, last_error
		FROM platform.m3_sync WHERE user_id = $1`, user).Scan(&s.WantSuspended, &s.WantVersion, &s.ConvergedVers, &s.Observed, &s.ObservedAt, &s.Attempts, &s.LastAttemptAt, &s.LastError)
	if err != nil {
		e.t.Fatalf("no delivery row for %s: %v", user, err)
	}
	return s
}

// mustEnable and mustDisable go through the store and fail the test on any error.
func (e *entEnv) mustEnable(user string, until *time.Time) entitlements.Entry {
	e.t.Helper()
	en, err := e.store.Enable(testCtx(e.t), entitlements.EnableReq{UserID: user, Until: until, AdminID: entAdminA, Reason: "harness enable", Key: entKey("enable")})
	if err != nil {
		e.t.Fatalf("Enable(%s): %v", user, err)
	}
	return en
}

func (e *entEnv) mustDisable(user string) entitlements.Entry {
	e.t.Helper()
	en, err := e.store.Disable(testCtx(e.t), entitlements.DisableReq{UserID: user, AdminID: entAdminA, Reason: "harness disable", Key: entKey("disable")})
	if err != nil {
		e.t.Fatalf("Disable(%s): %v", user, err)
	}
	return en
}

// backdatedEnable makes an entitlement due at once WITHOUT waiting: it enables the user through the ledger with a legal end date an hour ahead (since platform_0005 the ledger stamps created_at with the
// database's now(), so a ledger row can no longer be backdated) and then moves the entitlement's enabled_until `agoMinutes` minutes into the past as the superuser: the same act as ledgerMakeDue,
// and the only way a test makes a due entitlement without waiting (no role can write platform.m3_entitlements).
func (e *entEnv) backdatedEnable(user string, agoMinutes int) {
	e.t.Helper()
	e.exec(`INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, until_at, reason, idempotency_key)
		VALUES ($1, 'm3', 'm3_enable', 0, 0, $2, now() + interval '1 hour', 'enabled by the harness, made due below', $3)`, user, entAdminA, entKey("backdated"))
	e.exec(`UPDATE platform.m3_entitlements SET enabled_until = now() - make_interval(mins => $2) WHERE user_id = $1 AND enabled`, user, agoMinutes)
}

// requireIs fails unless err matches every want with errors.Is.
func entRequireIs(t *testing.T, what string, err error, want ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want an error", what)
	}
	for _, w := range want {
		if !errors.Is(err, w) {
			t.Fatalf("%s: error %q does not match %v", what, err, w)
		}
	}
}

// entPgError returns the database error inside err, or fails.
func entPgError(t *testing.T, what string, err error) *pgconn.PgError {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: no database error inside %v", what, err)
	}
	return pe
}

// ---- the writers ------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestEntStore_EnableAndDisableWriteTheRightLedgerRowsAndTheEntitlementFollows(t *testing.T) {
	e := newEntEnv(t, 2)
	u, other := entUser(0), entUser(1)
	ctx := testCtx(t)

	// Nothing yet: no entitlement, no delivery row (the control for every "now there is one" below).
	if got, err := e.store.Get(ctx, u); err != nil || got.Exists {
		t.Fatalf("Get before any entry = %+v, %v, want Exists == false", got, err)
	}
	if got, err := e.store.Sync(ctx, u); err != nil || got.Exists {
		t.Fatalf("Sync before any entry = %+v, %v, want Exists == false", got, err)
	}

	// 1. enable with no end date
	en := e.mustEnable(u, nil)
	if en.Kind != "m3_enable" || en.Replayed || en.AdminID == nil || *en.AdminID != entAdminA || en.Until != nil || en.UserID != u || en.LedgerID == 0 {
		t.Fatalf("the entry of the first Enable = %+v", en)
	}
	row, ok := e.entitlementOf(u)
	if !ok || !row.Enabled || row.Until != nil || row.ChangedBy == nil || *row.ChangedBy != entAdminA {
		t.Fatalf("after Enable the entitlement is %+v (exists %v), want enabled, no date, changed by the admin", row, ok)
	}
	if s := e.syncOf(u); s.WantSuspended || s.WantVersion != 1 || s.ConvergedVers != 0 || s.Observed != "unknown" || s.Attempts != 0 {
		t.Fatalf("the delivery row after the first Enable = %+v, want want_suspended=false version 1, not converged, observed unknown", s)
	}

	// 2. re-date: an enabled entitlement given a date changes the date and NOT the wanted state (no new version)
	until := e.dbNow().Add(time.Hour).Truncate(time.Microsecond)
	en2 := e.mustEnable(u, &until)
	if en2.Until == nil || !en2.Until.Equal(until) {
		t.Fatalf("the entry's date = %v, want %v", en2.Until, until)
	}
	row, _ = e.entitlementOf(u)
	if !row.Enabled || row.Until == nil || !row.Until.Equal(until) {
		t.Fatalf("after the dated Enable the entitlement is %+v, want enabled until %v", row, until)
	}
	if s := e.syncOf(u); s.WantVersion != 1 || s.WantSuspended {
		t.Fatalf("re-dating raised the wanted version: %+v (an unchanged state must not be re-delivered)", s)
	}

	// 3. disable: the date is cleared and module 3 is wanted suspended (version 2)
	dis := e.mustDisable(u)
	if dis.Kind != "m3_disable" || dis.Until != nil || dis.AdminID == nil {
		t.Fatalf("the entry of Disable = %+v", dis)
	}
	row, _ = e.entitlementOf(u)
	if row.Enabled || row.Until != nil || row.ChangedBy == nil || *row.ChangedBy != entAdminA {
		t.Fatalf("after Disable the entitlement is %+v, want disabled, no date", row)
	}
	if s := e.syncOf(u); !s.WantSuspended || s.WantVersion != 2 || s.ConvergedVers != 0 {
		t.Fatalf("the delivery row after Disable = %+v, want want_suspended=true version 2, still not converged", s)
	}

	// 4. enable again: wanted active, version 3
	e.mustEnable(u, nil)
	if s := e.syncOf(u); s.WantSuspended || s.WantVersion != 3 {
		t.Fatalf("the delivery row after the second Enable = %+v, want want_suspended=false version 3", s)
	}

	// The ledger: exactly these four rows, in order, all of module m3 and all moving no credits.
	rows := e.m3Ledger(u)
	wantKinds := []string{"m3_enable", "m3_enable", "m3_disable", "m3_enable"}
	if len(rows) != len(wantKinds) {
		t.Fatalf("the ledger holds %d module-3 rows for the user, want %d: %v", len(rows), len(wantKinds), rows)
	}
	for i, k := range wantKinds {
		if !strings.HasPrefix(rows[i], k+"|"+entAdminA+"|") {
			t.Errorf("ledger row %d = %q, want kind %s by admin A", i+1, rows[i], k)
		}
	}
	if !strings.Contains(rows[1], until.Format("2006-01-02T15:04:05.000000Z")) {
		t.Errorf("the dated row %q does not carry the date %s", rows[1], until.Format("2006-01-02T15:04:05.000000Z"))
	}
	if n := e.int(`SELECT count(*) FROM platform.ledger WHERE user_id = $1 AND (module_id <> 'm3' OR available_delta <> 0 OR reserved_delta <> 0 OR job_id IS NOT NULL)`, u); n != 0 {
		t.Errorf("%d of the user's rows are not pure module-3 events with zero deltas", n)
	}
	if n := e.int("SELECT count(*) FROM platform.balance_drift"); n != 0 {
		t.Errorf("platform.balance_drift lists %d rows after entitlement events (they move no credits)", n)
	}
	if n := e.int("SELECT count(*) FROM platform.balances WHERE user_id = $1", u); n != 0 {
		t.Errorf("an entitlement event created %d balance rows: module 3 is not metered", n)
	}
	// The other user is untouched (paired control for "the right user").
	if _, ok := e.entitlementOf(other); ok || len(e.m3Ledger(other)) != 0 {
		t.Error("the other user has an entitlement or a ledger row: the writes leaked across users")
	}

	// The readers see what the superuser saw.
	got, err := e.store.Get(ctx, strings.ToUpper(u))
	if err != nil || !got.Exists || !got.Enabled || got.Until != nil || got.ChangedBy == nil || *got.ChangedBy != entAdminA || got.UserID != u {
		t.Errorf("Get = %+v, %v", got, err)
	}
	st, err := e.store.Sync(ctx, u)
	if err != nil || !st.Exists || st.WantVersion != 3 || st.WantSuspended || st.Converged() || st.Observed != "unknown" || st.LastError != nil || st.ObservedAt != nil {
		t.Errorf("Sync = %+v, %v", st, err)
	}
}

func TestEntStore_AReplayByKeyReturnsTheOriginalAndADifferentRequestIsRefused(t *testing.T) {
	e := newEntEnv(t, 2)
	u := entUser(0)
	ctx := testCtx(t)
	until := e.dbNow().Add(time.Hour)
	req := entitlements.EnableReq{UserID: u, Until: &until, AdminID: entAdminA, Reason: "ticket 1", Key: "replay-key"}

	first, err := e.store.Enable(ctx, req)
	if err != nil || first.Replayed {
		t.Fatalf("first Enable = %+v, %v", first, err)
	}
	again, err := e.store.Enable(ctx, req)
	if err != nil || !again.Replayed || again.LedgerID != first.LedgerID || again.Kind != "m3_enable" || again.Until == nil || !again.Until.Equal(until.Truncate(time.Microsecond)) {
		t.Fatalf("the replay = %+v, %v, want the original entry flagged Replayed", again, err)
	}
	if n := e.countKind(u, "m3_enable"); n != 1 {
		t.Fatalf("a replay wrote a row: %d m3_enable rows", n)
	}
	// An upper-case spelling of the user is the same user, so the same key is the same request.
	req2 := req
	req2.UserID = strings.ToUpper(u)
	if r, err := e.store.Enable(ctx, req2); err != nil || !r.Replayed || r.LedgerID != first.LedgerID {
		t.Fatalf("the upper-case replay = %+v, %v", r, err)
	}

	// The same key with ANY different parameter is refused, and writes nothing.
	later := until.Add(time.Minute)
	other := map[string]func(entitlements.EnableReq) entitlements.EnableReq{
		"another date":   func(r entitlements.EnableReq) entitlements.EnableReq { r.Until = &later; return r },
		"no date":        func(r entitlements.EnableReq) entitlements.EnableReq { r.Until = nil; return r },
		"another reason": func(r entitlements.EnableReq) entitlements.EnableReq { r.Reason = "ticket 2"; return r },
		"no reason":      func(r entitlements.EnableReq) entitlements.EnableReq { r.Reason = ""; return r },
		"another admin":  func(r entitlements.EnableReq) entitlements.EnableReq { r.AdminID = entAdminB; return r },
	}
	before := e.m3Ledger(u)
	for name, mutate := range other {
		_, err := e.store.Enable(ctx, mutate(req))
		entRequireIs(t, name, err, entitlements.ErrIdempotencyMismatch)
	}
	_, err = e.store.Disable(ctx, entitlements.DisableReq{UserID: u, AdminID: entAdminA, Reason: "ticket 1", Key: "replay-key"})
	entRequireIs(t, "a Disable under an Enable's key", err, entitlements.ErrIdempotencyMismatch)
	if after := e.m3Ledger(u); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("a refused request changed the ledger:\nbefore %v\nafter  %v", before, after)
	}
	if row, _ := e.entitlementOf(u); !row.Enabled || row.Until == nil || !row.Until.Equal(until.Truncate(time.Microsecond)) {
		t.Fatalf("a refused request changed the entitlement: %+v", row)
	}

	// A Disable replays the same way.
	dReq := entitlements.DisableReq{UserID: u, AdminID: entAdminA, Reason: "abuse", Key: "disable-replay-key"}
	d1, err := e.store.Disable(ctx, dReq)
	if err != nil || d1.Replayed {
		t.Fatalf("first Disable = %+v, %v", d1, err)
	}
	if d2, err := e.store.Disable(ctx, dReq); err != nil || !d2.Replayed || d2.LedgerID != d1.LedgerID || d2.Kind != "m3_disable" {
		t.Fatalf("the Disable replay = %+v, %v", d2, err)
	}

	// A replay writes nothing, so it keeps working after the admin has been disabled; a NEW request by that admin does not (paired control).
	e.exec("UPDATE platform.admins SET disabled_at = now() WHERE id = $1", entAdminA)
	if r, err := e.store.Enable(ctx, req); err != nil || !r.Replayed {
		t.Fatalf("the replay after the admin was disabled = %+v, %v, want the original entry", r, err)
	}
	_, err = e.store.Enable(ctx, entitlements.EnableReq{UserID: u, AdminID: entAdminA, Key: "a-new-key"})
	entRequireIs(t, "a new request by the disabled admin", err, entitlements.ErrAdminDisabled)

	// Eight simultaneous identical requests make exactly one row: one writer, seven replays (the per-user advisory lock serialises them).
	u2 := entUser(1)
	var wg sync.WaitGroup
	results := make([]entitlements.Entry, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.store.Disable(testCtx(t), entitlements.DisableReq{UserID: u2, AdminID: entAdminB, Reason: "race", Key: "same-key"})
		}()
	}
	wg.Wait()
	replays := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("simultaneous request %d failed: %v", i, errs[i])
		}
		if results[i].Replayed {
			replays++
		}
		if results[i].LedgerID != results[0].LedgerID {
			t.Errorf("request %d got ledger id %d, request 0 got %d: two rows were written", i, results[i].LedgerID, results[0].LedgerID)
		}
	}
	if replays != 7 || e.countKind(u2, "m3_disable") != 1 {
		t.Errorf("%d replays and %d rows from eight identical simultaneous requests, want 7 and 1", replays, e.countKind(u2, "m3_disable"))
	}
}

func TestEntStore_EveryRefusalNamesItsReasonAndWritesNothing(t *testing.T) {
	e := newEntEnv(t, 2)
	u, b := entUser(0), entUser(1)
	ctx := testCtx(t)
	good := entitlements.EnableReq{UserID: u, AdminID: entAdminA, Reason: "ok", Key: "control-ok"}

	// Control first: the request that every refusal below is a one-field variation of is accepted.
	if _, err := e.store.Enable(ctx, good); err != nil {
		t.Fatalf("control failed: the good request was refused: %v", err)
	}
	baseline := e.int("SELECT count(*) FROM platform.ledger")

	// A disabled admin: refused by the database (SQLSTATE 23514, constraint admin_is_active), mapped to ErrAdminDisabled.
	e.exec("UPDATE platform.admins SET disabled_at = now() WHERE id = $1", entAdminB)
	for what, call := range map[string]func() error{
		"enable by a disabled admin": func() error {
			_, err := e.store.Enable(ctx, entitlements.EnableReq{UserID: b, AdminID: entAdminB, Key: "k-dis-1"})
			return err
		},
		"disable by a disabled admin": func() error {
			_, err := e.store.Disable(ctx, entitlements.DisableReq{UserID: b, AdminID: entAdminB, Key: "k-dis-2"})
			return err
		},
	} {
		err := call()
		entRequireIs(t, what, err, entitlements.ErrAdminDisabled)
		if pe := entPgError(t, what, err); pe.Code != "23514" || pe.ConstraintName != "admin_is_active" {
			t.Errorf("%s: the database said %s / %q, want 23514 / admin_is_active", what, pe.Code, pe.ConstraintName)
		}
		if errors.Is(err, entitlements.ErrInvalid) {
			t.Errorf("%s: a database refusal is not an ErrInvalid (that is for requests refused before the database)", what)
		}
	}

	// An admin that does not exist, a user that does not exist: foreign keys, 23503.
	// (The ledger's own foreign key and the foreign key of the entitlement row its trigger writes are both refusals of the same fact; the store maps either, and the ledger's fires first.)
	_, err := e.store.Enable(ctx, entitlements.EnableReq{UserID: u, AdminID: entNobody, Key: "k-unk-admin"})
	entRequireIs(t, "an unknown admin", err, entitlements.ErrUnknownAdmin)
	if pe := entPgError(t, "an unknown admin", err); pe.Code != "23503" || (pe.ConstraintName != "ledger_acting_admin_id_fkey" && pe.ConstraintName != "m3_entitlements_changed_by_fkey") {
		t.Errorf("unknown admin: %s / %q", pe.Code, pe.ConstraintName)
	}
	_, err = e.store.Disable(ctx, entitlements.DisableReq{UserID: entNobody, AdminID: entAdminA, Key: "k-unk-user"})
	entRequireIs(t, "an unknown user", err, entitlements.ErrUnknownUser)
	if pe := entPgError(t, "an unknown user", err); pe.Code != "23503" || (pe.ConstraintName != "ledger_user_id_fkey" && pe.ConstraintName != "m3_entitlements_user_id_fkey") {
		t.Errorf("unknown user: %s / %q", pe.Code, pe.ConstraintName)
	}

	// The date must lie at least one second ahead by the DATABASE's clock: a past date and a date half a second ahead are refused; an hour ahead is accepted (control).
	for what, until := range map[string]time.Time{"a date in the past": e.dbNow().Add(-time.Hour), "a date half a second ahead": e.dbNow().Add(500 * time.Millisecond), "a date right now": e.dbNow()} {
		_, err := e.store.Enable(ctx, entitlements.EnableReq{UserID: u, AdminID: entAdminA, Until: &until, Key: entKey("k-until")})
		entRequireIs(t, what, err, entitlements.ErrUntilNotFuture, entitlements.ErrInvalid)
	}
	hour := e.dbNow().Add(time.Hour)
	if _, err := e.store.Enable(ctx, entitlements.EnableReq{UserID: u, AdminID: entAdminA, Until: &hour, Key: entKey("k-until-ok")}); err != nil {
		t.Fatalf("control failed: a date an hour ahead was refused: %v", err)
	}

	// Refused before the database (the validation the unit tests cover without one), seen here through the real store: the system user, a long reason.
	_, err = e.store.Enable(ctx, entitlements.EnableReq{UserID: "00000000-0000-0000-0000-000000000000", AdminID: entAdminA, Key: "k-sys"})
	entRequireIs(t, "the system user", err, entitlements.ErrSystemUser, entitlements.ErrInvalid)
	_, err = e.store.Enable(ctx, entitlements.EnableReq{UserID: u, AdminID: entAdminA, Reason: strings.Repeat("x", entitlements.MaxReasonBytes+1), Key: "k-long"})
	entRequireIs(t, "a reason of 1001 bytes", err, entitlements.ErrReasonTooLong, entitlements.ErrInvalid)
	exact := strings.Repeat("é", entitlements.MaxReasonBytes/2) // 1000 bytes of two-byte characters
	if en, err := e.store.Enable(ctx, entitlements.EnableReq{UserID: u, AdminID: entAdminA, Reason: exact, Key: "k-exact"}); err != nil || en.Reason != exact {
		t.Fatalf("control failed: a reason of exactly 1000 bytes: %+v, %v", en, err)
	}
	if got := e.str("SELECT reason FROM platform.ledger WHERE idempotency_key = 'k-exact'"); got != exact {
		t.Errorf("the stored reason differs from the one given (%d bytes stored)", len(got))
	}

	// Nothing but the two accepted controls (k-until-ok and k-exact) was written by all of the above.
	if got := e.int("SELECT count(*) FROM platform.ledger"); got != baseline+2 {
		t.Errorf("the ledger holds %d rows, want %d: a refused request left a row (or an accepted one did not write)", got, baseline+2)
	}
	if n := e.int("SELECT count(*) FROM platform.m3_entitlements WHERE user_id = $1", b); n != 0 {
		t.Errorf("the user of the refused requests has an entitlement")
	}
}

// TestEntStore_TheConstraintNamesTheStoreMapsExist: the store turns a refusal into a named error by SQLSTATE and CONSTRAINT NAME. Here each of those names is produced by the database,
// by a raw INSERT as platform_app of the kind the store makes, so a rename in a migration fails THIS test (naming the constraint) rather than silently turning every refusal into a
// generic error. Each refused row has a legal twin that succeeds.
func TestEntStore_TheConstraintNamesTheStoreMapsExist(t *testing.T) {
	e := newEntEnv(t, 4)
	ctx := testCtx(t)
	insert := func(user, kind, admin, key, untilSQL string) error {
		sql := fmt.Sprintf(`INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, until_at, idempotency_key)
			VALUES ($1, 'm3', $2, 0, 0, NULLIF($3, '')::uuid, %s, $4)`, untilSQL)
		_, err := e.pool.Exec(ctx, sql, user, kind, admin, key)
		return err
	}
	expect := func(what string, err error, code string, constraints ...string) {
		t.Helper()
		pe := entPgError(t, what, err)
		named := false
		for _, c := range constraints {
			named = named || pe.ConstraintName == c
		}
		if pe.Code != code || !named {
			t.Errorf("%s: the database said %s / %q, want %s / one of %v (the store maps this refusal by exactly that name)", what, pe.Code, pe.ConstraintName, code, constraints)
		}
	}
	future := "now() + interval '1 hour'"

	// the legal twin of every row below
	if err := insert(entUser(0), "m3_enable", entAdminA, "twin-1", future); err != nil {
		t.Fatalf("control failed: the legal enable was refused: %v", err)
	}
	expect("a date that is not after created_at", insert(entUser(1), "m3_enable", entAdminA, "r-until", "now()"), "23514", "ledger_until_at_shape")
	expect("an unknown admin", insert(entUser(1), "m3_enable", entNobody, "r-admin", future), "23503", "ledger_acting_admin_id_fkey", "m3_entitlements_changed_by_fkey")
	expect("an unknown user", insert(entNobody, "m3_enable", entAdminA, "r-user", future), "23503", "ledger_user_id_fkey", "m3_entitlements_user_id_fkey")
	expect("the system user", insert("00000000-0000-0000-0000-000000000000", "m3_enable", entAdminA, "r-system", future), "23514", "ledger_not_the_system_user")
	expect("a repeated key", insert(entUser(0), "m3_enable", entAdminA, "twin-1", future), "23505", "ledger_idempotency_key")
	e.exec("UPDATE platform.admins SET disabled_at = now() WHERE id = $1", entAdminB)
	expect("a disabled admin", insert(entUser(1), "m3_disable", entAdminB, "r-disabled", "NULL"), "23514", "admin_is_active")
	// an expiry of an entitlement that is not due, and of a user who has none: the database's own is-due check
	expect("an expiry that is not due", insert(entUser(0), "m3_expire", "", "r-early", "NULL"), "23514", "m3_expire_is_due")
	expect("an expiry of a user with no entitlement", insert(entUser(2), "m3_expire", "", "r-none", "NULL"), "23514", "m3_expire_is_due")
	// the legal twins of the two expiry refusals: once the entitlement is due, the same statement is accepted
	e.backdatedEnable(entUser(3), 30)
	if err := insert(entUser(3), "m3_expire", "", "twin-expire", "NULL"); err != nil {
		t.Fatalf("control failed: an expiry of a due entitlement was refused: %v", err)
	}
	expect("an expiry of an entitlement already expired", insert(entUser(3), "m3_expire", "", "r-twice", "NULL"), "23514", "m3_expire_is_due")
	if row, _ := e.entitlementOf(entUser(3)); row.Enabled || row.Until != nil || row.ChangedBy != nil {
		t.Errorf("after the expiry the entitlement is %+v, want disabled with no date and changed_by NULL (the system)", row)
	}
}

// ---- the expiry ---------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestEntStore_ExpireDueExpiresExactlyTheDueUsersOnTheDatabasesClock(t *testing.T) {
	e := newEntEnv(t, 5)
	ctx := testCtx(t)
	dueA, dueB, later, noDate, off := entUser(0), entUser(1), entUser(2), entUser(3), entUser(4)

	// Two entitlements that become due in a few seconds (committed enables with a near-future date), one with a date an hour away, one with no date, one disabled.
	soon := e.dbNow().Add(5 * time.Second)
	e.mustEnable(dueA, &soon)
	soon2 := soon.Add(time.Second)
	e.mustEnable(dueB, &soon2)
	hour := e.dbNow().Add(time.Hour)
	e.mustEnable(later, &hour)
	e.mustEnable(noDate, nil)
	e.mustDisable(off)

	// CONTROL: before anything is due, an expiry pass expires nobody. (If this machine stalled for five seconds since the enable the control is void and the test says so.)
	if n := e.int("SELECT count(*) FROM platform.m3_entitlements WHERE enabled AND enabled_until <= now()"); n != 0 {
		t.Fatalf("%d entitlements were already due when the not-yet-due control was to run: the machine stalled for 5 s between the enable and the control, so the control is void; re-run", n)
	}
	if got, err := e.store.ExpireDue(ctx, 100); err != nil || len(got) != 0 {
		t.Fatalf("ExpireDue before anything is due = %v, %v, want nobody", got, err)
	}
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE kind = 'm3_expire'"); n != 0 {
		t.Fatalf("a pass with nothing due wrote %d expiry rows", n)
	}

	// Wait on the DATABASE's clock until both are due (bounded), never on the test's.
	deadline := time.Now().Add(30 * time.Second)
	for e.int("SELECT count(*) FROM platform.m3_entitlements WHERE enabled AND enabled_until <= now()") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the database's clock did not reach the entitlements' dates within 30 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	untilA, _ := e.entitlementOf(dueA)
	untilB, _ := e.entitlementOf(dueB)
	untouched := func() string {
		var parts []string
		for _, u := range []string{later, noDate, off} {
			row, _ := e.entitlementOf(u)
			parts = append(parts, fmt.Sprintf("%s=%+v|%v", u, row, e.m3Ledger(u)))
		}
		return strings.Join(parts, "\n")
	}
	before := untouched()

	rep, err := e.store.ExpireDueReport(ctx, 100)
	if err != nil {
		t.Fatalf("ExpireDueReport: %v", err)
	}
	if rep.Due != 2 || len(rep.Expired) != 2 || rep.Skipped != 0 {
		t.Fatalf("report = %+v, want 2 due, 2 expired, 0 skipped", rep)
	}
	if rep.Expired[0].UserID != dueA || rep.Expired[1].UserID != dueB {
		t.Errorf("expired %v then %v, want the earliest date first (%s then %s)", rep.Expired[0].UserID, rep.Expired[1].UserID, dueA, dueB)
	}
	if !rep.Expired[0].EnabledUntil.Equal(*untilA.Until) || !rep.Expired[1].EnabledUntil.Equal(*untilB.Until) {
		t.Errorf("the reported dates %v, %v differ from the entitlements' %v, %v", rep.Expired[0].EnabledUntil, rep.Expired[1].EnabledUntil, untilA.Until, untilB.Until)
	}

	for i, u := range []string{dueA, dueB} {
		row, _ := e.entitlementOf(u)
		if row.Enabled || row.Until != nil || row.ChangedBy != nil {
			t.Errorf("%s after the expiry: %+v, want disabled, no date, changed_by NULL (the system acted)", u, row)
		}
		rows := e.m3Ledger(u)
		last := rows[len(rows)-1]
		parts := strings.Split(last, "|")
		wantKey := fmt.Sprintf("m3-expire:%s:%d", u, rep.Expired[i].EnabledUntil.UnixMicro())
		if parts[0] != "m3_expire" || parts[1] != "-" || parts[4] != wantKey || !strings.HasPrefix(parts[3], "entitlement expired") {
			t.Errorf("%s: the last ledger row is %q, want an m3_expire row with no admin and the key %s", u, last, wantKey)
		}
		if id := e.int("SELECT id FROM platform.ledger WHERE user_id = $1 AND kind = 'm3_expire'", u); id != rep.Expired[i].LedgerID {
			t.Errorf("%s: the reported ledger id %d is not the m3_expire row %d", u, rep.Expired[i].LedgerID, id)
		}
		// the expiry raised the wanted state: module 3 must now be suspended (version 2: the enable was 1)
		if s := e.syncOf(u); !s.WantSuspended || s.WantVersion != 2 || s.ConvergedVers != 0 {
			t.Errorf("%s: the delivery row after the expiry = %+v, want want_suspended=true version 2, not converged", u, s)
		}
	}
	// PAIRED CONTROL: the users that are not due, and the user who was never enabled with a date, are exactly as they were.
	if after := untouched(); after != before {
		t.Errorf("an expiry pass touched a user that was not due:\nbefore %s\nafter  %s", before, after)
	}
	if row, _ := e.entitlementOf(later); !row.Enabled || row.Until == nil {
		t.Errorf("the user enabled with a later date is %+v, want still enabled with the date", row)
	}

	// A second pass is a no-op (an entitlement is not expired twice), and writes nothing.
	ledgerRows := e.int("SELECT count(*) FROM platform.ledger")
	if got, err := e.store.ExpireDue(ctx, 100); err != nil || len(got) != 0 {
		t.Fatalf("the second ExpireDue = %v, %v, want nobody", got, err)
	}
	if n := e.int("SELECT count(*) FROM platform.ledger"); n != ledgerRows {
		t.Errorf("the second pass wrote %d rows", n-ledgerRows)
	}
	// Re-enabling an expired user and letting it lapse again expires it AGAIN (a new date is a new key): the idempotency is per date, not per user.
	if row, _ := e.entitlementOf(dueA); row.Enabled {
		t.Fatal("precondition: the expired user should be disabled")
	}
	again := e.dbNow().Add(3 * time.Second)
	e.mustEnable(dueA, &again)
	for e.int("SELECT count(*) FROM platform.m3_entitlements WHERE user_id = $1 AND enabled_until <= now()", dueA) == 0 {
		if time.Now().After(deadline.Add(30 * time.Second)) {
			t.Fatal("the re-dated entitlement never fell due")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got, err := e.store.ExpireDue(ctx, 100); err != nil || len(got) != 1 || got[0].UserID != dueA {
		t.Fatalf("the re-dated entitlement was not expired again: %v, %v", got, err)
	}
	if n := e.countKind(dueA, "m3_expire"); n != 2 {
		t.Errorf("%d expiry rows for the user who lapsed twice, want 2", n)
	}
}

func TestEntStore_ExpireDueHonoursTheLimitAndTheOrder(t *testing.T) {
	e := newEntEnv(t, 7)
	ctx := testCtx(t)
	// Five users whose dates passed 10..50 minutes ago, written in a shuffled order, and two that are not due.
	for _, c := range []struct{ user, ago int }{{3, 20}, {0, 50}, {4, 10}, {1, 40}, {2, 30}} {
		e.backdatedEnable(entUser(c.user), c.ago)
	}
	hour := e.dbNow().Add(time.Hour)
	e.mustEnable(entUser(5), &hour)
	e.mustEnable(entUser(6), nil)

	if got, err := e.store.ExpireDue(ctx, 2); err != nil || len(got) != 2 || got[0].UserID != entUser(0) || got[1].UserID != entUser(1) {
		t.Fatalf("ExpireDue(2) = %v, %v, want the two oldest dates (users 0 and 1) in that order", got, err)
	}
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE kind = 'm3_expire'"); n != 2 {
		t.Fatalf("a limit of 2 wrote %d expiry rows", n)
	}
	got, err := e.store.ExpireDue(ctx, 100)
	if err != nil || len(got) != 3 || got[0].UserID != entUser(2) || got[1].UserID != entUser(3) || got[2].UserID != entUser(4) {
		t.Fatalf("ExpireDue(100) = %v, %v, want users 2, 3, 4 in the order of their dates", got, err)
	}
	if got, err := e.store.ExpireDue(ctx, 100); err != nil || len(got) != 0 {
		t.Fatalf("the pass after that = %v, %v, want nobody", got, err)
	}
	for _, u := range []string{entUser(5), entUser(6)} {
		if row, _ := e.entitlementOf(u); !row.Enabled {
			t.Errorf("%s was expired though it was not due", u)
		}
	}
	// ExpireDue refuses a limit below one rather than treating it as "none" or "all".
	for _, n := range []int{0, -1} {
		if _, err := e.store.ExpireDue(ctx, n); !errors.Is(err, entitlements.ErrInvalid) {
			t.Errorf("ExpireDue(%d) = %v, want ErrInvalid", n, err)
		}
	}
}

func TestEntStore_AKeyThatWasAlreadyUsedMeansSomebodyElseExpiredItAndIsSkippedNotAnError(t *testing.T) {
	e := newEntEnv(t, 2)
	ctx := testCtx(t)
	blocked, free := entUser(0), entUser(1)
	e.backdatedEnable(blocked, 30)
	e.backdatedEnable(free, 20)

	// Put the expiry's own idempotency key on the blocked user's ledger WITHOUT the effect an expiry has (the superuser writes a different kind with triggers off for this one
	// transaction), as if another instance had written that key: the entitlement is still due, but the key is taken.
	until, _ := e.entitlementOf(blocked)
	key := fmt.Sprintf("m3-expire:%s:%d", blocked, until.Until.UnixMicro())
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key)
		VALUES ($1, 'm3', 'm3_disable', 0, 0, $2, 'stands in for another instance', $3)`, blocked, entAdminA, key); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.entitlementOf(blocked); !row.Enabled || row.Until == nil {
		t.Fatalf("precondition: the blocked user's entitlement should still be enabled and due, got %+v", row)
	}

	rep, err := e.store.ExpireDueReport(ctx, 100)
	if err != nil {
		t.Fatalf("a taken key is not an error, got %v", err)
	}
	if rep.Due != 2 || rep.Skipped != 1 || len(rep.Expired) != 1 || rep.Expired[0].UserID != free {
		t.Fatalf("report = %+v, want 2 due, the blocked user skipped, the free one expired", rep)
	}
	if n := e.countKind(blocked, "m3_expire"); n != 0 {
		t.Errorf("the skipped user has %d expiry rows", n)
	}
	// Paired control: the same pass over the user whose key was free expired it.
	if row, _ := e.entitlementOf(free); row.Enabled {
		t.Errorf("the control user was not expired: %+v", row)
	}
}

// TestEntStore_ExpiryIsPerUserAndSerialisesOnTheUsersAdvisoryLock is the forced interleave. A test connection holds user B's advisory lock (the number computed here, not imported). An
// expiry pass over users A and B expires A at once (each user is its OWN transaction) and then blocks on B's lock: proven from the server (pg_stat_activity: wait event advisory, and
// pg_locks: not granted, on exactly B's key), not by sleeping. Then two passes are started together over user C while the lock of C is held: both are proven blocked, and when it
// is released exactly one expires C and the other skips it.
func TestEntStore_ExpiryIsPerUserAndSerialisesOnTheUsersAdvisoryLock(t *testing.T) {
	e := newEntEnv(t, 4)
	ctx := testCtx(t)
	userA, userB, userC, userD := entUser(0), entUser(1), entUser(2), entUser(3)
	e.backdatedEnable(userA, 50) // oldest date: processed first
	e.backdatedEnable(userB, 40)
	e.backdatedEnable(userC, 30)
	holder := dial(t, e.s.DSN, nil) // a second superuser connection that holds the locks

	lock := func(user string) { // session-level, held until unlocked
		var ok bool
		if err := holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", entLockKey(user)).Scan(&ok); err != nil || !ok {
			t.Fatalf("the test could not take the advisory lock of %s: %v (%v)", user, ok, err)
		}
	}
	unlock := func(user string) {
		var ok bool
		if err := holder.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", entLockKey(user)).Scan(&ok); err != nil || !ok {
			t.Fatalf("the test could not release the advisory lock of %s: %v (%v)", user, ok, err)
		}
	}
	var holderPID int32
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	// waiters lists the platform_app backends of this database that are waiting on an advisory lock, with the key they wait for.
	waiters := func() (n int, keys []int64, blockedByHolder int) {
		rows, err := e.admin.Query(ctx, `SELECT (l.classid::bigint << 32) | l.objid::bigint, $1 = ANY (pg_blocking_pids(a.pid))
			FROM pg_stat_activity a JOIN pg_locks l ON l.pid = a.pid AND l.locktype = 'advisory' AND NOT l.granted
			WHERE a.datname = current_database() AND a.usename = 'platform_app' AND a.wait_event_type = 'Lock' AND a.wait_event = 'advisory'`, holderPID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var key int64
			var byHolder bool
			if err := rows.Scan(&key, &byHolder); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, key)
			if byHolder {
				blockedByHolder++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return len(keys), keys, blockedByHolder
	}
	waitForWaiters := func(want int, key int64) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			n, keys, byHolder := waiters()
			if n == want && byHolder == want {
				for _, k := range keys {
					if k != key {
						t.Fatalf("a backend waits on advisory key %d, want %d (the platform's key of that user)", k, key)
					}
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected %d backends blocked by the test's lock on key %d within 20 s; saw %d waiting on %v (%d of them blocked by the holder)", want, key, n, keys, byHolder)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	// Control of the instrument: with nothing held, nothing waits.
	if n, _, _ := waiters(); n != 0 {
		t.Fatalf("control failed: %d backends wait on an advisory lock before the test took any", n)
	}

	// Part 1: B's lock is held; the pass expires A, then blocks on B.
	lock(userB)
	type outcome struct {
		rep entitlements.ExpireReport
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		rep, err := e.store.ExpireDueReport(ctx, 100)
		first <- outcome{rep, err}
	}()
	waitForWaiters(1, entLockKey(userB))
	if n := e.countKind(userA, "m3_expire"); n != 1 {
		t.Fatalf("while the pass waits on B's lock A has %d expiry rows, want 1: users are not expired in transactions of their own", n)
	}
	if n := e.countKind(userB, "m3_expire"); n != 0 {
		t.Fatalf("B was expired while its lock was held by the test: %d rows", n)
	}
	select {
	case o := <-first:
		t.Fatalf("the pass finished while B's lock was held: %+v, %v", o.rep, o.err)
	default:
	}
	unlock(userB)
	o := <-first
	if o.err != nil {
		t.Fatalf("the pass failed after the lock was released: %v", o.err)
	}
	if o.rep.Due != 3 || len(o.rep.Expired) != 3 || o.rep.Skipped != 0 {
		t.Fatalf("the pass report = %+v, want all three expired (C was not locked)", o.rep)
	}

	// Part 2: two passes over ONE due user (D, which falls due only now), both forced to wait for its lock.
	e.backdatedEnable(userD, 20)
	lock(userD)
	var wg sync.WaitGroup
	outs := make([]outcome, 2)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := e.store.ExpireDueReport(ctx, 100)
			outs[i] = outcome{rep, err}
		}()
	}
	waitForWaiters(2, entLockKey(userD))
	if n := e.countKind(userD, "m3_expire"); n != 0 {
		t.Fatalf("an expiry row exists while both passes are blocked: %d", n)
	}
	unlock(userD)
	wg.Wait()
	expired, skipped := 0, 0
	for i, o := range outs {
		if o.err != nil {
			t.Fatalf("pass %d failed: %v", i, o.err)
		}
		if o.rep.Due != 1 {
			t.Errorf("pass %d saw %d due users, want 1 (both started while the user was due)", i, o.rep.Due)
		}
		expired += len(o.rep.Expired)
		skipped += o.rep.Skipped
	}
	if expired != 1 || skipped != 1 {
		t.Fatalf("two simultaneous passes expired %d and skipped %d, want exactly 1 and 1", expired, skipped)
	}
	if n := e.countKind(userD, "m3_expire"); n != 1 {
		t.Fatalf("%d expiry rows for the user, want exactly 1", n)
	}
	if s := e.syncOf(userD); s.WantVersion != 2 || !s.WantSuspended {
		t.Errorf("the delivery row = %+v, want one raise to version 2 (suspended), not two", s)
	}
}

func TestEntStore_TheLockKeyIsThePlatformsAndTheStoresOwn(t *testing.T) {
	for _, u := range []string{entUser(0), entUser(1), entAdminA, "11111111-1111-1111-1111-111111111111"} {
		k := entitlements.LockKey(u)
		if k != entLockKey(u) {
			t.Errorf("the store's key for %s is %d, the harness computes %d: the contention tests would hold the wrong lock", u, k, entLockKey(u))
		}
		if k < 7_000_000_000 || k > 7_999_999_999 {
			t.Errorf("the key %d for %s is outside the platform's band 7_xxx_xxx_xxx (CLAUDE.md rule 9)", k, u)
		}
	}
	// Pinned vector: the same number internal/entitlements and internal/money agree on.
	if got := entLockKey("11111111-1111-1111-1111-111111111111"); got != 7_652_549_623 {
		t.Errorf("the harness's own key for the pinned user is %d, want 7652549623", got)
	}
}
