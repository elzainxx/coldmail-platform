//go:build harness

package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/dsnscrub"
)

const (
	envAdminURL    = "HARNESS_ADMIN_DATABASE_URL"
	envHostURL     = "HARNESS_ADMIN_DATABASE_HOST_URL"
	envAppPassword = "HARNESS_PLATFORM_APP_PASSWORD"
	envMigrateBin  = "HARNESS_MIGRATE_BIN"
	envRepoRoot    = "HARNESS_REPO_ROOT"
)

// migratorLockKey is the migrator's advisory lock. It is written out here on purpose and NOT imported from
// internal/migrate: the tests prove contention on this number, so changing the number in the code without changing it
// here fails TestRunnerLockIsReallyTaken. The platform's band is 7_xxx_xxx_xxx.
const migratorLockKey int64 = 7100000001

// requireEnv returns the variable, or FAILS the test naming it. There is no skip: a test that cannot run is a red test.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if strings.TrimSpace(v) == "" {
		t.Fatalf("required environment variable %s is not set: the harness stack provides it, and this test never skips", name)
	}
	return v
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// scrubbed is an error's text with any connection string or password taken out (internal/dsnscrub), so a failed dial
// in a CI log never carries the superuser's credentials.
func scrubbed(err error, dsn string) string { return dsnscrub.Scrub(err, dsn).Error() }

func migrateBin(t *testing.T) string {
	t.Helper()
	bin := requireEnv(t, envMigrateBin)
	if st, err := os.Stat(bin); err != nil || st.IsDir() {
		t.Fatalf("%s=%q is not a file: build cmd/platform-migrate first", envMigrateBin, bin)
	}
	return bin
}

func repoMigrationsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(requireEnv(t, envRepoRoot), "db", "migrations")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("%s/db/migrations is not a directory (%s=%q)", dir, envRepoRoot, os.Getenv(envRepoRoot))
	}
	return dir
}

// ---- connections ----

// tryDial connects and returns the error to the caller; mutate may change the parsed config (user, password, params).
func tryDial(dsn string, mutate func(*pgx.ConnConfig)) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("cannot parse the connection string: " + scrubbed(err, dsn))
	}
	if mutate != nil {
		mutate(cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return pgx.ConnectConfig(ctx, cfg)
}

// dial connects or fails the test, and closes the connection when the test ends.
func dial(t *testing.T, dsn string, mutate func(*pgx.ConnConfig)) *pgx.Conn {
	t.Helper()
	c, err := tryDial(dsn, mutate)
	if err != nil {
		t.Fatalf("connect: %s", scrubbed(err, dsn))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c
}

// liveAdmin is a superuser connection to the LIVE database. It is read-only by construction (the session defaults to
// read-only transactions), so a test that only has this connection cannot alter the live database.
func liveAdmin(t *testing.T) *pgx.Conn {
	t.Helper()
	return dial(t, requireEnv(t, envAdminURL), func(cfg *pgx.ConnConfig) {
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
		cfg.RuntimeParams["application_name"] = "harness-live-readonly"
	})
}

// requireLiveMigrated fails unless the live database has platform migration 0000. Tests that run `down 0000` on a
// scratch database call it first: the down tries to drop the cluster-global roles and keeps any that are still
// referenced, and what references them is the live database. Without it a down could drop platform_app out from under
// the running service.
func requireLiveMigrated(t *testing.T) {
	t.Helper()
	c := liveAdmin(t)
	var n int
	if err := c.QueryRow(testCtx(t), "SELECT count(*) FROM platform.schema_migrations WHERE version = '0000'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the live database is not migrated (platform.schema_migrations has no 0000: n=%d err=%v): the harness stack must run migrate-platform first, and a down here could drop the cluster-global roles", n, err)
	}
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ---- scratch databases ----

type scratch struct {
	Name string
	DSN  string // superuser connection to the scratch database
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// newScratch creates platform_h_<random> from the host URL and drops it when the test ends, pass or fail.
func newScratch(t *testing.T) scratch {
	t.Helper()
	hostURL := requireEnv(t, envHostURL)
	u, err := url.Parse(hostURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// URL", envHostURL)
	}
	name := "platform_h_" + randomHex(t, 6)
	host := dial(t, hostURL, nil)
	if _, err := host.Exec(testCtx(t), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch database %s: %s", name, scrubbed(err, hostURL))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, hostURL)
		if err != nil {
			t.Errorf("cleanup: cannot connect to drop scratch database %s: %s", name, scrubbed(err, hostURL))
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup: drop scratch database %s: %s", name, scrubbed(err, hostURL))
		}
	})
	u.Path = "/" + name
	u.RawPath = ""
	return scratch{Name: name, DSN: u.String()}
}

// admin opens a superuser connection to the scratch database, closed before the database is dropped.
func (s scratch) admin(t *testing.T) *pgx.Conn {
	t.Helper()
	return dial(t, s.DSN, nil)
}

// ---- the migrator as a real process ----

type result struct {
	Code    int
	Stdout  string
	Stderr  string
	Elapsed time.Duration
}

func (r result) all() string { return r.Stdout + "\n" + r.Stderr }

type running struct {
	cmd    *exec.Cmd
	so, se *bytes.Buffer
	cancel context.CancelFunc
	start  time.Time
	done   chan error
}

// migEnv is the environment of one migrator run: the connection string and directory, plus any overrides.
func migEnv(dsn, dir string, extra map[string]string) map[string]string {
	env := map[string]string{"PLATFORM_MIGRATE_DATABASE_URL": dsn, "PLATFORM_MIGRATIONS_DIR": dir}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// cleanEnv is the process environment without anything that could configure the migrator or libpq behind the test's
// back (PLATFORM_*, PG*, HARNESS_*), plus extra.
func cleanEnv(extra map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			k := strings.ToUpper(kv[:i])
			if strings.HasPrefix(k, "PLATFORM_") || strings.HasPrefix(k, "PG") || strings.HasPrefix(k, "HARNESS_") {
				continue
			}
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}

func startMigrator(t *testing.T, env map[string]string, args ...string) *running {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, migrateBin(t), args...)
	cmd.Env = cleanEnv(env)
	cmd.Dir = t.TempDir() // so a relative default path can never resolve to the repository
	r := &running{cmd: cmd, so: &bytes.Buffer{}, se: &bytes.Buffer{}, cancel: cancel, start: time.Now(), done: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = r.so, r.se
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("cannot start %s: %v", filepath.Base(cmd.Path), err)
	}
	go func() { r.done <- cmd.Wait() }()
	t.Cleanup(cancel) // a migrator that is still running when the test ends is killed
	return r
}

func (r *running) wait(t *testing.T) result {
	t.Helper()
	err := <-r.done
	r.cancel()
	res := result{Stdout: r.so.String(), Stderr: r.se.String(), Elapsed: time.Since(r.start)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		res.Code = ee.ExitCode()
	default:
		t.Fatalf("the migrator did not exit normally (killed after the 90s limit?): %v\nstdout: %s\nstderr: %s", err, res.Stdout, res.Stderr)
	}
	return res
}

func runMigrator(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	return startMigrator(t, env, args...).wait(t)
}

// mustExit fails the test unless the run exited with want, printing both streams.
func mustExit(t *testing.T, r result, want int, what string) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("%s: exit %d, want %d\nstdout: %s\nstderr: %s", what, r.Code, want, r.Stdout, r.Stderr)
	}
}

func mustContain(t *testing.T, text, want, what string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%s: output does not contain %q:\n%s", what, want, text)
	}
}

// ---- migration directories ----

var upFileRe = regexp.MustCompile(`^platform_([0-9]{4})_.+\.up\.sql$`)

// chainOnDisk returns the versions of the up files in dir, ascending, and fails unless they are 0000, 0001, ... with no gap.
func chainOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if m := upFileRe.FindStringSubmatch(e.Name()); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("found 0 platform_NNNN_*.up.sql files in %s: the instrument matched nothing", dir)
	}
	for i, v := range out {
		if want := ver(i); v != want {
			t.Fatalf("the migrations in %s are not 0000, 0001, ... with no gap: position %d is %s, want %s", dir, i, v, want)
		}
	}
	return out
}

func ver(n int) string { return fmt.Sprintf("%04d", n) }

// copyMigrations copies every platform_*.sql file of the repository into a new temp directory, which a test may extend.
func copyMigrations(t *testing.T) string {
	t.Helper()
	src := repoMigrationsDir(t)
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "platform_") || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("copied 0 migration files from %s", src)
	}
	return dst
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePair writes platform_<version>_<desc>.up.sql and .down.sql.
func writePair(t *testing.T, dir, version, desc, up, down string) {
	t.Helper()
	writeFile(t, dir, "platform_"+version+"_"+desc+".up.sql", up)
	writeFile(t, dir, "platform_"+version+"_"+desc+".down.sql", down)
}

// recordSQL and removeSQL are the self-tracking convention: what every up file and every down file does with its version.
func recordSQL(version string) string {
	return "INSERT INTO platform.schema_migrations (version) VALUES ('" + version + "');\n"
}

func removeSQL(version string) string {
	return "DELETE FROM platform.schema_migrations WHERE version = '" + version + "';\n"
}

// ---- observing a database ----

// appliedVersions reads platform.schema_migrations; nil when the table does not exist.
func appliedVersions(t *testing.T, c *pgx.Conn) []string {
	t.Helper()
	ctx := testCtx(t)
	var exists bool
	if err := c.QueryRow(ctx, "SELECT to_regclass('platform.schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return nil
	}
	rows, err := c.Query(ctx, "SELECT version FROM platform.schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameStrings(a, b []string) bool {
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

func relationExists(t *testing.T, c *pgx.Conn, qualified string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(testCtx(t), "SELECT to_regclass($1) IS NOT NULL", qualified).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func schemaExists(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(testCtx(t), "SELECT to_regnamespace($1) IS NOT NULL", name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// tryAdvisoryLock takes the migrator's lock on c if it is free and says whether it did. A caller that gets true must
// release it with advisoryUnlock.
func tryAdvisoryLock(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	var got bool
	if err := c.QueryRow(testCtx(t), "SELECT pg_try_advisory_lock($1)", migratorLockKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func advisoryUnlock(t *testing.T, c *pgx.Conn) {
	t.Helper()
	var released bool
	if err := c.QueryRow(testCtx(t), "SELECT pg_advisory_unlock($1)", migratorLockKey).Scan(&released); err != nil || !released {
		t.Fatalf("pg_advisory_unlock(%d) = %v, %v", migratorLockKey, released, err)
	}
}
