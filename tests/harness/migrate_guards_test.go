//go:build harness

package harness

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// chainPlus returns the real chain on disk and the first version number beyond it, for tests that extend a copy of it.
func chainPlus(t *testing.T, dir string) (chain []string, next int) {
	t.Helper()
	chain = chainOnDisk(t, dir)
	return chain, len(chain)
}

func withAll(chain []string, extra ...string) []string {
	return append(append([]string(nil), chain...), extra...)
}

// TestRunnerContiguityGap: a chain with a hole is an error that names the hole, and nothing is applied, not even the
// versions before it. The control adds the missing file and the same command applies the lot.
func TestRunnerContiguityGap(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	dir := copyMigrations(t)
	chain, next := chainPlus(t, dir)
	missing, beyond := ver(next), ver(next+1)
	writePair(t, dir, beyond, "x", recordSQL(beyond), removeSQL(beyond))
	env := migEnv(s.DSN, dir, nil)

	res := runMigrator(t, env, "up")
	mustExit(t, res, 1, "up over a gap")
	for _, want := range []string{"chain gap", beyond, missing} {
		mustContain(t, res.Stderr, want, "up over a gap")
	}
	if got := appliedVersions(t, admin); len(got) != 0 {
		t.Fatalf("up over a gap applied %v: the chain is checked before the first file runs, so nothing may be applied (in particular not %s)", got, beyond)
	}

	// Control: the same command with the missing file present applies the whole chain.
	writePair(t, dir, missing, "x", recordSQL(missing), removeSQL(missing))
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "up with the gap filled")
	if got, want := appliedVersions(t, admin), withAll(chain, missing, beyond); !sameStrings(got, want) {
		t.Fatalf("after the gap was filled: applied = %v, want %v", got, want)
	}
}

// TestRunnerLockIsReallyTaken proves contention instead of hoping for it: another session holds the migrator's advisory
// lock, so `up` must wait the configured 2 seconds and fail with the lock-timeout error, apply nothing, and succeed once
// the lock is released. `status` takes no lock and answers while it is held.
func TestRunnerLockIsReallyTaken(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	holder := s.admin(t)
	dir := repoMigrationsDir(t)
	chain := chainOnDisk(t, dir)
	env := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC": "2"})

	if !tryAdvisoryLock(t, holder) {
		t.Fatalf("precondition: advisory lock %d was already held in the scratch database", migratorLockKey)
	}
	held := true
	t.Cleanup(func() {
		if held {
			_ = holder.Close(testCtx(t))
		}
	})

	// status does not lock: it answers while the lock is held.
	st := runMigrator(t, env, "status")
	mustExit(t, st, 0, "status while the lock is held")
	mustContain(t, st.Stdout, "summary: 0 applied", "status while the lock is held")

	// up waits for the lock, then fails with the lock-timeout error, within a bounded time.
	res := runMigrator(t, env, "up")
	mustExit(t, res, 1, "up while the lock is held")
	mustContain(t, res.Stderr, "could not acquire migration advisory lock within 2s", "up while the lock is held")
	if res.Elapsed < 1800*time.Millisecond {
		t.Fatalf("up gave up after %s: it must retry for the whole 2s lock timeout, not fail at the first refusal", res.Elapsed)
	}
	if res.Elapsed > 30*time.Second {
		t.Fatalf("up took %s to give up on a 2s lock timeout", res.Elapsed)
	}
	if got := appliedVersions(t, admin); len(got) != 0 {
		t.Fatalf("up applied %v although it never held the lock", got)
	}
	// down is bounded by the same lock.
	down := runMigrator(t, migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC": "1", "PLATFORM_MIGRATE_ALLOW_DOWN": "true"}), "down", chain[len(chain)-1])
	mustExit(t, down, 1, "down while the lock is held")
	mustContain(t, down.Stderr, "could not acquire migration advisory lock within 1s", "down while the lock is held")

	// The paired control: release the lock and the very same command succeeds.
	advisoryUnlock(t, holder)
	held = false
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "up after the lock was released")
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("after the release: applied = %v, want %v", got, chain)
	}
	// ...and the migrator released it again on its way out: the session that held it can take it.
	if !tryAdvisoryLock(t, holder) {
		t.Fatal("the lock is still held after the migrator exited: it was not released")
	}
	advisoryUnlock(t, holder)
}

// TestRunnerLockHeldWhileFileRuns: while a migration file is executing (observed in pg_stat_activity, scoped to the
// migrator's own backend) the lock is held, a second migrator is turned away, and when the first finishes the lock is free.
func TestRunnerLockHeldWhileFileRuns(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	observer := s.admin(t)
	dir := copyMigrations(t)
	chain, next := chainPlus(t, dir)
	slow := ver(next)
	writePair(t, dir, slow, "slow", "SELECT pg_sleep(6);\n"+recordSQL(slow), removeSQL(slow))

	first := startMigrator(t, migEnv(s.DSN, dir, nil), "up")

	// Wait until the first migrator is demonstrably inside the slow file. The filter names the migrator's own
	// application_name and excludes this observer's backend, whose query text also mentions pg_sleep.
	inside := false
	seen := map[string]bool{} // every distinct session state observed, for the failure message
	for deadline := time.Now().Add(40 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var n int
		if err := observer.QueryRow(testCtx(t), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND application_name = 'platform-migrate' AND state = 'active' AND query ILIKE '%pg_sleep(6)%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			inside = true
			break
		}
		rows, err := observer.Query(testCtx(t), `SELECT application_name, state, left(replace(query, E'\n', ' '), 60) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var app, state, query string
			if err := rows.Scan(&app, &state, &query); err != nil {
				t.Fatal(err)
			}
			seen[fmt.Sprintf("app=%q state=%q query=%q", app, state, query)] = true
		}
		rows.Close()
	}
	if !inside {
		res := first.wait(t)
		var obs []string
		for k := range seen {
			obs = append(obs, k)
		}
		sort.Strings(obs)
		t.Fatalf("never saw the migrator inside the slow file (exit %d, ran %s)\nstdout: %s\nstderr: %s\nsessions observed:\n  %s",
			res.Code, res.Elapsed, res.Stdout, res.Stderr, strings.Join(obs, "\n  "))
	}

	// At this instant the migrator is mid-file: the lock must be held.
	if tryAdvisoryLock(t, observer) {
		advisoryUnlock(t, observer)
		t.Fatal("the advisory lock was FREE while the migrator was running a file: it is not held for the whole of up")
	}
	// A second migrator is turned away by it.
	second := runMigrator(t, migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC": "1"}), "up")
	mustExit(t, second, 1, "a second up during the first")
	mustContain(t, second.Stderr, "could not acquire migration advisory lock within 1s", "a second up during the first")

	res := first.wait(t)
	mustExit(t, res, 0, "the first up")
	if got, want := appliedVersions(t, admin), withAll(chain, slow); !sameStrings(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
	if res.Elapsed < 5*time.Second {
		t.Fatalf("the first up took %s: the six-second file cannot have run", res.Elapsed)
	}
	if !tryAdvisoryLock(t, observer) {
		t.Fatal("the lock is still held after the migrator exited")
	}
	advisoryUnlock(t, observer)
}

// TestRunnerFileFailureRollsBack: a file that fails after creating a table leaves no table and no tracking row; the
// files before it stay applied (one transaction per file). The control applies the same file without the failing statement.
func TestRunnerFileFailureRollsBack(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	dir := copyMigrations(t)
	chain, next := chainPlus(t, dir)
	v := ver(next)
	create := "CREATE TABLE platform.zz_rollback_probe (id int PRIMARY KEY);\nINSERT INTO platform.zz_rollback_probe VALUES (1);\n"
	down := "DROP TABLE platform.zz_rollback_probe;\n" + removeSQL(v)
	writePair(t, dir, v, "fails_midway", create+"INSERT INTO platform.zz_no_such_table VALUES (1);\n"+recordSQL(v), down)
	env := migEnv(s.DSN, dir, nil)

	res := runMigrator(t, env, "up")
	mustExit(t, res, 1, "up with a failing file")
	mustContain(t, res.Stderr, v+"_fails_midway", "up with a failing file")
	mustContain(t, res.Stderr, "zz_no_such_table", "up with a failing file")
	if relationExists(t, admin, "platform.zz_rollback_probe") {
		t.Fatal("platform.zz_rollback_probe exists after the file failed: the file did not run in one transaction")
	}
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("applied = %v, want the earlier files %v and not %s", got, chain, v)
	}

	// Control: the same file without the failing statement applies.
	writePair(t, dir, v, "fails_midway", create+recordSQL(v), down)
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "up with the file repaired")
	if !relationExists(t, admin, "platform.zz_rollback_probe") {
		t.Fatal("the repaired file ran but its table is not there")
	}
	var rows int
	if err := admin.QueryRow(testCtx(t), "SELECT count(*) FROM platform.zz_rollback_probe").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("zz_rollback_probe has %d rows (err %v), want 1", rows, err)
	}
	if got, want := appliedVersions(t, admin), withAll(chain, v); !sameStrings(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

// TestRunnerFileMustRecordItself: an up file that does not INSERT its own version is rolled back and reported.
func TestRunnerFileMustRecordItself(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	dir := copyMigrations(t)
	chain, next := chainPlus(t, dir)
	v := ver(next)
	body := "CREATE TABLE platform.zz_unrecorded (id int);\n"
	writePair(t, dir, v, "forgets", body, "DROP TABLE platform.zz_unrecorded;\n"+removeSQL(v))
	env := migEnv(s.DSN, dir, nil)

	res := runMigrator(t, env, "up")
	mustExit(t, res, 1, "up with a file that does not record itself")
	mustContain(t, res.Stderr, "migration "+v+"_forgets did not record itself", "up with a file that does not record itself")
	if relationExists(t, admin, "platform.zz_unrecorded") {
		t.Fatal("platform.zz_unrecorded exists: a file that did not record itself was committed")
	}
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("applied = %v, want %v", got, chain)
	}

	// Control: the same file with the INSERT added is applied.
	writePair(t, dir, v, "forgets", body+recordSQL(v), "DROP TABLE platform.zz_unrecorded;\n"+removeSQL(v))
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "up with the file recording itself")
	if !relationExists(t, admin, "platform.zz_unrecorded") {
		t.Fatal("the repaired file ran but its table is not there")
	}
}

// TestRunnerDownGuards: down refuses while a higher version is applied, refuses a version that is not applied, rolls back
// a transaction whose down file did not remove the row, and accepts the correct down.
func TestRunnerDownGuards(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	good := copyMigrations(t)
	chain, next := chainPlus(t, good)
	v := ver(next)
	up := "CREATE TABLE platform.zz_down_probe (id int);\n" + recordSQL(v)
	writePair(t, good, v, "probe", up, "DROP TABLE platform.zz_down_probe;\n"+removeSQL(v))
	// The same chain with a down file that forgets to remove the tracking row.
	bad := copyMigrations(t)
	writePair(t, bad, v, "probe", up, "DROP TABLE platform.zz_down_probe;\n")
	allow := map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"}
	envGood, envBad := migEnv(s.DSN, good, allow), migEnv(s.DSN, bad, allow)
	full := withAll(chain, v)

	mustExit(t, runMigrator(t, envGood, "up"), 0, "up")
	if got := appliedVersions(t, admin); !sameStrings(got, full) {
		t.Fatalf("after up: applied = %v, want %v", got, full)
	}

	// Refused while a higher version is applied: the bootstrap's CASCADE would destroy the probe table unseen. With one migration below the probe the runner could only name the probe;
	// with the chain 0000..0003 it names ONE of the higher versions that are applied, whichever it meets first (it walks a map), so the assertion is that the version it names is one of
	// them - not that it is the probe - and that the refusal says what to do.
	res := runMigrator(t, envGood, "down", "0000")
	mustExit(t, res, 1, "down 0000 while "+v+" is applied")
	named := regexp.MustCompile(`refusing to roll back 0000 while higher version ([0-9]{4}) is still applied; roll back highest-first`).FindStringSubmatch(res.Stderr)
	if named == nil {
		t.Fatalf("down 0000 while %s is applied: the refusal does not read 'refusing to roll back 0000 while higher version NNNN is still applied; roll back highest-first':\n%s", v, res.Stderr)
	}
	if !containsString(full[1:], named[1]) {
		t.Fatalf("down 0000 names higher version %s, but the versions above 0000 that are applied are %v", named[1], full[1:])
	}
	if got := appliedVersions(t, admin); !sameStrings(got, full) || !relationExists(t, admin, "platform.zz_down_probe") {
		t.Fatalf("a refused down changed the database: applied = %v", got)
	}
	// The same rule in the middle of the chain: 0001 cannot be rolled back while 0002, 0003 and the probe are applied.
	if len(chain) > 2 {
		mid := chain[1]
		res = runMigrator(t, envGood, "down", mid)
		mustExit(t, res, 1, "down "+mid+" while higher versions are applied")
		named = regexp.MustCompile(`refusing to roll back ` + mid + ` while higher version ([0-9]{4}) is still applied; roll back highest-first`).FindStringSubmatch(res.Stderr)
		if named == nil || !containsString(full[2:], named[1]) {
			t.Fatalf("down %s while %v are applied: the refusal does not name one of the higher versions %v:\n%s", mid, full, full[2:], res.Stderr)
		}
		if got := appliedVersions(t, admin); !sameStrings(got, full) {
			t.Fatalf("a refused down %s changed the database: applied = %v", mid, got)
		}
	}

	// A down file that leaves the row behind fails, and the whole down is rolled back (the table is still there).
	res = runMigrator(t, envBad, "down", v)
	mustExit(t, res, 1, "down with a file that does not remove its row")
	mustContain(t, res.Stderr, "down did not remove its tracking row", "down with a file that does not remove its row")
	if !relationExists(t, admin, "platform.zz_down_probe") {
		t.Fatal("zz_down_probe is gone: a down that failed verification was committed")
	}
	if got := appliedVersions(t, admin); !sameStrings(got, full) {
		t.Fatalf("applied = %v after the failed down, want %v", got, full)
	}

	// Control: the correct down file removes the row and the table.
	res = runMigrator(t, envGood, "down", v)
	mustExit(t, res, 0, "down "+v)
	mustContain(t, res.Stdout, "rolled back "+v+"_probe", "down "+v)
	if relationExists(t, admin, "platform.zz_down_probe") {
		t.Fatal("zz_down_probe still exists after a successful down")
	}
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("after down %s: applied = %v, want %v", v, got, chain)
	}

	// Rolling back a version that is not applied, or that has no file, is an error and touches nothing.
	res = runMigrator(t, envGood, "down", v)
	mustExit(t, res, 1, "down of a version that is not applied")
	mustContain(t, res.Stderr, "is not applied: nothing to roll back", "down of a version that is not applied")
	res = runMigrator(t, envGood, "down", "9999")
	mustExit(t, res, 1, "down of a version with no file")
	mustContain(t, res.Stderr, "no migration with version 9999", "down of a version with no file")
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("applied = %v, want %v", got, chain)
	}
}

// TestRunnerTimeoutsBite proves the two timeout variables have consumers: each file sees the configured values, a
// statement longer than PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC is cancelled, and a statement blocked longer than
// PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC is cancelled. Each failing case has a control with only the dial changed.
func TestRunnerTimeoutsBite(t *testing.T) {
	s := newScratch(t)
	admin := s.admin(t)
	dir := copyMigrations(t)
	chain, next := chainPlus(t, dir)
	mustExit(t, runMigrator(t, migEnv(s.DSN, dir, nil), "up"), 0, "up of the real chain")

	// 1. Inside a file, the session settings are the configured ones.
	vSettings := ver(next)
	writePair(t, dir, vSettings, "settings", `CREATE TABLE platform.zz_settings (k text PRIMARY KEY, v text NOT NULL);
INSERT INTO platform.zz_settings VALUES ('lock_timeout', current_setting('lock_timeout')), ('statement_timeout', current_setting('statement_timeout'));
`+recordSQL(vSettings), "DROP TABLE platform.zz_settings;\n"+removeSQL(vSettings))
	res := runMigrator(t, migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC": "7", "PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC": "42"}), "up")
	mustExit(t, res, 0, "up with 7s and 42s")
	got := map[string]string{}
	rows, err := admin.Query(testCtx(t), "SELECT k, v FROM platform.zz_settings")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	rows.Close()
	if got["lock_timeout"] != "7s" || got["statement_timeout"] != "42s" {
		t.Fatalf("inside the file lock_timeout/statement_timeout = %q/%q, want 7s/42s (SET LOCAL from the two variables)", got["lock_timeout"], got["statement_timeout"])
	}

	// 2. statement_timeout: a 4-second statement dies at 1 second and is rolled back; the same file passes with 30.
	vSlow := ver(next + 1)
	writePair(t, dir, vSlow, "slow", "SELECT pg_sleep(4);\n"+recordSQL(vSlow), removeSQL(vSlow))
	res = runMigrator(t, migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC": "1"}), "up")
	mustExit(t, res, 1, "up with a 1s statement timeout")
	mustContain(t, res.Stderr, "statement timeout", "up with a 1s statement timeout")
	if res.Elapsed >= 3500*time.Millisecond {
		t.Fatalf("up took %s: the 1s statement timeout did not cut the 4s statement short", res.Elapsed)
	}
	if got := appliedVersions(t, admin); !sameStrings(got, withAll(chain, vSettings)) {
		t.Fatalf("applied = %v: %s must not be recorded", got, vSlow)
	}
	res = runMigrator(t, migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC": "30"}), "up")
	mustExit(t, res, 0, "up with a 30s statement timeout")
	if res.Elapsed < 3500*time.Millisecond {
		t.Fatalf("the control finished in %s: the 4s file cannot have run", res.Elapsed)
	}

	// 3. lock_timeout: an ALTER TABLE blocked by another session's ACCESS EXCLUSIVE lock is cancelled at 2 seconds.
	if _, err := admin.Exec(testCtx(t), "CREATE TABLE platform.zz_locked (i int)"); err != nil {
		t.Fatal(err)
	}
	locker := s.admin(t)
	tx, err := locker.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(testCtx(t))
	if _, err := tx.Exec(testCtx(t), "LOCK TABLE platform.zz_locked IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	vBlocked := ver(next + 2)
	writePair(t, dir, vBlocked, "blocked", "ALTER TABLE platform.zz_locked ADD COLUMN c int;\n"+recordSQL(vBlocked), removeSQL(vBlocked))
	envLock := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC": "2", "PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC": "60"})
	res = runMigrator(t, envLock, "up")
	mustExit(t, res, 1, "up blocked behind a table lock")
	mustContain(t, res.Stderr, "lock timeout", "up blocked behind a table lock")
	if res.Elapsed < 1800*time.Millisecond || res.Elapsed > 30*time.Second {
		t.Fatalf("up gave up after %s: a statement blocked on a lock must be cancelled at the 2s lock timeout", res.Elapsed)
	}
	var hasCol bool
	if err := admin.QueryRow(testCtx(t), "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='platform' AND table_name='zz_locked' AND column_name='c')").Scan(&hasCol); err != nil || hasCol {
		t.Fatalf("zz_locked has the column (%v, err %v): the blocked file was not rolled back", hasCol, err)
	}
	// Control: release the table lock and the same command applies the file.
	if err := tx.Rollback(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	res = runMigrator(t, envLock, "up")
	mustExit(t, res, 0, "up once the table lock is released")
	if got, want := appliedVersions(t, admin), withAll(chain, vSettings, vSlow, vBlocked); !sameStrings(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

// TestRunnerSetAppPassword: up-and-set-password checks the password FIRST (nothing is applied without one), sets it, and
// the role can then log in with exactly that password. The only password ever set is HARNESS_PLATFORM_APP_PASSWORD, the
// one the live service already uses, so this changes nothing for it.
func TestRunnerSetAppPassword(t *testing.T) {
	pw := requireEnv(t, envAppPassword)
	s := newScratch(t)
	admin := s.admin(t)
	dir := repoMigrationsDir(t)
	chain := chainOnDisk(t, dir)
	env := migEnv(s.DSN, dir, nil)

	// No password: exit 2, the variable named, and NOTHING applied (the check precedes the migrations).
	for name, val := range map[string]string{"unset": "", "blank": "   "} {
		e := migEnv(s.DSN, dir, map[string]string{"PLATFORM_APP_PASSWORD": val})
		res := runMigrator(t, e, "up-and-set-password")
		mustExit(t, res, 2, "up-and-set-password with the password "+name)
		mustContain(t, res.Stderr, "PLATFORM_APP_PASSWORD", "up-and-set-password with the password "+name)
		if schemaExists(t, admin, "platform") {
			t.Fatalf("up-and-set-password with the password %s applied migrations: the password must be checked before any file runs", name)
		}
	}
	res := runMigrator(t, env, "set-app-password")
	mustExit(t, res, 2, "set-app-password with no password")

	// With the password: applied, set, and never printed.
	envPW := migEnv(s.DSN, dir, map[string]string{"PLATFORM_APP_PASSWORD": pw})
	res = runMigrator(t, envPW, "up-and-set-password")
	mustExit(t, res, 0, "up-and-set-password")
	mustContain(t, res.Stdout, "set-app-password: platform_app password set", "up-and-set-password")
	if strings.Contains(res.all(), pw) {
		t.Fatal("up-and-set-password printed the password")
	}
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("applied = %v, want %v", got, chain)
	}
	res = runMigrator(t, envPW, "set-app-password")
	mustExit(t, res, 0, "set-app-password")
	if strings.Contains(res.all(), pw) {
		t.Fatal("set-app-password printed the password")
	}

	// The role logs in with that password (over the network, not via SET ROLE)...
	app := dial(t, s.DSN, func(cfg *pgx.ConnConfig) { cfg.User, cfg.Password = "platform_app", pw })
	var who string
	if err := app.QueryRow(testCtx(t), "SELECT session_user").Scan(&who); err != nil || who != "platform_app" {
		t.Fatalf("session_user = %q (err %v), want platform_app", who, err)
	}
	// ...and not with another: the control that the login above was really authenticated.
	_, err := tryDial(s.DSN, func(cfg *pgx.ConnConfig) { cfg.User, cfg.Password = "platform_app", pw+"-wrong" })
	if err == nil {
		t.Fatal("platform_app logged in with a wrong password: the server is not enforcing passwords on this connection, so the login above proves nothing")
	}
	if sqlState(err) != "28P01" {
		t.Fatalf("wrong password: SQLSTATE %q (%s), want 28P01", sqlState(err), scrubbed(err, s.DSN))
	}
}

// TestRunnerErrorsAreScrubbed runs the real binary against a host nothing listens on, with a recognisable password in the
// connection string and in PLATFORM_APP_PASSWORD, and checks neither string appears in anything it prints.
func TestRunnerErrorsAreScrubbed(t *testing.T) {
	const (
		probe = "RECOGNISABLE-pw-Zq83-1177"
		appPW = "App-pw-Lm49-2204"
	)
	dir := repoMigrationsDir(t)
	unreachable := "postgres://migrator:" + probe + "@127.0.0.1:1/platform?connect_timeout=3"
	env := migEnv(unreachable, dir, map[string]string{"PLATFORM_APP_PASSWORD": appPW, "PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	for _, args := range [][]string{{"up"}, {"status"}, {"down", "0000"}, {"set-app-password"}, {"up-and-set-password"}} {
		name := strings.Join(args, " ")
		res := runMigrator(t, env, args...)
		mustExit(t, res, 1, name+" against an unreachable host")
		mustContain(t, res.Stderr, "connect:", name+" against an unreachable host") // an error WAS reported, so the absences below mean something
		for _, secret := range []string{probe, appPW, unreachable} {
			if strings.Contains(res.all(), secret) {
				t.Fatalf("%s: the output contains %q:\n%s", name, secret, res.all())
			}
		}
	}
	// A connection string the driver rejects is a configuration error (exit 2) and is scrubbed too.
	bad := "postgres://migrator:" + probe + "@db.example:notaport/platform"
	res := runMigrator(t, migEnv(bad, dir, nil), "status")
	mustExit(t, res, 2, "status with an unparseable connection string")
	mustContain(t, res.Stderr, "PLATFORM_MIGRATE_DATABASE_URL", "status with an unparseable connection string")
	for _, secret := range []string{probe, bad} {
		if strings.Contains(res.all(), secret) {
			t.Fatalf("the output contains %q:\n%s", secret, res.all())
		}
	}
}
