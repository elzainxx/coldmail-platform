package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/dsnscrub"
)

// advisoryLockKey is the session-level advisory lock every up and down holds for its whole run. The platform's
// band is 7_xxx_xxx_xxx and no other (CLAUDE.md, Phase 0 rule 9): module 3 holds 8_, module 2 holds 9_ (its
// migrator takes 9_100_000_001), so no module's lock can ever be this one. The key is per database, so a migrator
// run against one database never waits on a migrator run against another.
const advisoryLockKey int64 = 7_100_000_001

const (
	// lockPollInterval paces the pg_try_advisory_lock retry loop. Pacing, not a tuning dial: how long the loop may
	// run is PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC.
	lockPollInterval = 250 * time.Millisecond
	// defaultConnectTimeout bounds a connection attempt so an unreachable Postgres fails fast instead of waiting for
	// the operating system's TCP timeout. The connection string's own connect_timeout replaces it.
	defaultConnectTimeout = 15 * time.Second
	// cleanupTimeout bounds the unlock, rollback and close that run after the work is done or has failed.
	cleanupTimeout = 5 * time.Second
	// applicationName shows in pg_stat_activity, so an operator (and a test) can tell which backend is the migrator.
	applicationName = "platform-migrate"
)

// ErrDownForbidden is returned by Down when PLATFORM_MIGRATE_ALLOW_DOWN is not true. It is a configuration refusal,
// decided before any connection is made.
var ErrDownForbidden = errors.New("down is forbidden unless " + EnvAllowDown + "=true (forward-only in prod)")

// Runner applies and rolls back migrations against Cfg.DatabaseURL. Progress goes to Out (os.Stdout when nil).
type Runner struct {
	Cfg Config
	Out io.Writer
}

func (r *Runner) out() io.Writer {
	if r.Out == nil {
		return os.Stdout
	}
	return r.Out
}

// querier is what the bookkeeping reads need; a *pgx.Conn and a pgx.Tx both satisfy it.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// suppressedError replaces a driver error whose text embedded the connection string. Unwrap keeps errors.Is and
// errors.As working; only the string, which is what reaches a terminal or a ticket, is replaced.
type suppressedError struct{ err error }

func (e *suppressedError) Error() string {
	return "connection string rejected by the driver (message suppressed: it embedded the DSN, check " + EnvDatabaseURL + ")"
}
func (e *suppressedError) Unwrap() error { return e.err }

// scrubDriverError runs a driver error through internal/dsnscrub. That package's own replacement text names the
// service's PLATFORM_DATABASE_URL, which is the wrong variable for the migrator, so when it fired this returns the
// same suppression naming the right one.
func scrubDriverError(err error, dsn string) error {
	if err == nil {
		return nil
	}
	scrubbed := dsnscrub.Scrub(err, dsn)
	if scrubbed.Error() == err.Error() {
		return err
	}
	return &suppressedError{err: scrubbed}
}

// redactedError is err with its text passed through Redact.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func redactErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if red := Redact(msg, secrets...); red != msg {
		return &redactedError{msg: red, err: err}
	}
	return err
}

// connect opens one connection. A connection string the driver cannot parse is a configuration error (exit 2); a
// connection that cannot be made is a runtime error (exit 1). Both go through the scrub, because pgx echoes a
// rejected connection string whole and its own redaction misses several spellings of the password.
func (r *Runner) connect(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(r.Cfg.DatabaseURL)
	if err != nil {
		return nil, configErr("%s is not a valid connection string: %v", EnvDatabaseURL, scrubDriverError(err, r.Cfg.DatabaseURL))
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = applicationName
	}
	cctx := ctx
	if cfg.ConnectTimeout == 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, defaultConnectTimeout)
		defer cancel()
	}
	c, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", scrubDriverError(err, r.Cfg.DatabaseURL))
	}
	return c, nil
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = c.Close(ctx)
}

// acquireLock takes the advisory lock with a bounded wait: a retry loop around the non-blocking
// pg_try_advisory_lock, so it never blocks forever and never depends on a server-side wait.
func (r *Runner) acquireLock(ctx context.Context, c *pgx.Conn) error {
	deadline := time.Now().Add(time.Duration(r.Cfg.LockTimeoutSec) * time.Second)
	for {
		var got bool
		if err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&got); err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		if got {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("could not acquire migration advisory lock within %ds (another migrator running?)", r.Cfg.LockTimeoutSec)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// releaseLock gives the lock back explicitly. Closing the connection would release it too; doing it here keeps
// "held for the whole run, released after" true even if a caller reuses the connection.
func (r *Runner) releaseLock(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, _ = c.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
}

// discover lists the versions on disk and refuses an empty directory: a runner that found nothing and reported
// "already current" would turn a wrong PLATFORM_MIGRATIONS_DIR into a green.
func (r *Runner) discover() ([]File, error) {
	files, err := Discover(r.Cfg.MigrationsDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("found 0 migration files in %q: refusing to report success on an empty chain (is %s right?)", r.Cfg.MigrationsDir, EnvMigrationsDir)
	}
	return files, nil
}

// trackingTableExists is the one special case the bootstrap needs: before it has run there is no tracking table.
func trackingTableExists(ctx context.Context, q querier) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, "SELECT to_regclass('platform.schema_migrations') IS NOT NULL").Scan(&ok); err != nil {
		return false, fmt.Errorf("check platform.schema_migrations: %w", err)
	}
	return ok, nil
}

// appliedVersions reads platform.schema_migrations; a database the bootstrap has not touched has none.
func appliedVersions(ctx context.Context, q querier) (map[string]bool, error) {
	applied := map[string]bool{}
	exists, err := trackingTableExists(ctx, q)
	if err != nil || !exists {
		return applied, err
	}
	rows, err := q.Query(ctx, "SELECT version FROM platform.schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("select versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// rowRecorded reports whether the version's tracking row exists. A tracking table that is gone holds no row, which is
// the correct reading after the bootstrap's down has dropped the schema.
func rowRecorded(ctx context.Context, q querier, version string) (bool, error) {
	exists, err := trackingTableExists(ctx, q)
	if err != nil || !exists {
		return false, err
	}
	var ok bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM platform.schema_migrations WHERE version = $1)", version).Scan(&ok); err != nil {
		return false, fmt.Errorf("read platform.schema_migrations: %w", err)
	}
	return ok, nil
}

// timeoutStatements are the two statements that open every file's transaction. SET LOCAL ends with the
// transaction, so neither timeout can leak onto the next file or the next command on the same session.
func timeoutStatements(lockSec, statementSec int) []string {
	return []string{
		"SET LOCAL lock_timeout = '" + strconv.Itoa(lockSec) + "s'",
		"SET LOCAL statement_timeout = '" + strconv.Itoa(statementSec) + "s'",
	}
}

func (r *Runner) beginFileTx(ctx context.Context, c *pgx.Conn) (pgx.Tx, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return nil, err
	}
	for _, stmt := range timeoutStatements(r.Cfg.LockTimeoutSec, r.Cfg.StatementTimeoutSec) {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			rollbackQuietly(tx)
			return nil, err
		}
	}
	return tx, nil
}

func rollbackQuietly(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = tx.Rollback(ctx) // after a successful Commit this is pgx.ErrTxClosed, which is the point of calling it unconditionally
}

// Up applies every pending version in ascending order, one transaction per file.
func (r *Runner) Up(ctx context.Context) error {
	files, err := r.discover()
	if err != nil {
		return err
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConn(c)

	if err := r.acquireLock(ctx, c); err != nil {
		return err
	}
	defer r.releaseLock(c)

	applied, err := appliedVersions(ctx, c)
	if err != nil {
		return err
	}
	// The gap check runs over the WHOLE chain before the first file is applied, so a dropped or renamed migration
	// produces an error and no half-applied schema.
	if err := CheckChain(files, applied); err != nil {
		return err
	}

	pending := 0
	for _, f := range files {
		if applied[f.Version] {
			continue
		}
		if err := r.applyOne(ctx, c, f); err != nil {
			return err
		}
		fmt.Fprintf(r.out(), "applied  %s_%s\n", f.Version, f.Name)
		pending++
	}
	if pending == 0 {
		fmt.Fprintln(r.out(), "up: no pending migrations (already current)")
	} else {
		fmt.Fprintf(r.out(), "up: applied %d migration(s)\n", pending)
	}
	return nil
}

// applyOne runs one up file in ONE transaction and VERIFIES, before committing, that the file recorded its own
// version. A file that did not is rolled back whole: committing it would leave changes the tracking table does not
// know about, and the next run would apply them again.
func (r *Runner) applyOne(ctx context.Context, c *pgx.Conn, f File) error {
	label := f.Version + "_" + f.Name
	body, err := os.ReadFile(f.UpPath)
	if err != nil {
		return fmt.Errorf("migration %s: %w", label, err)
	}
	tx, err := r.beginFileTx(ctx, c)
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", label, err)
	}
	defer rollbackQuietly(tx)

	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("migration %s: %w", label, err)
	}
	recorded, err := rowRecorded(ctx, tx, f.Version)
	if err != nil {
		return fmt.Errorf("migration %s did not record itself: %w", label, err)
	}
	if !recorded {
		return fmt.Errorf("migration %s did not record itself in platform.schema_migrations (the up file must INSERT its own version row); its transaction was rolled back", label)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migration %s: commit: %w", label, err)
	}
	return nil
}

// Down rolls back exactly one version (dev and CI only). It enforces reverse order: a version is refused while any
// higher one is applied, because the bootstrap's DROP SCHEMA ... CASCADE would otherwise destroy every higher
// object without its own down file ever running. Like Up it holds the lock, runs in one transaction with the same
// SET LOCAL timeouts, and verifies, before committing, that the row is gone.
func (r *Runner) Down(ctx context.Context, version string) error {
	if !r.Cfg.AllowDown {
		return ErrDownForbidden
	}
	version, err := NormalizeVersion(version)
	if err != nil {
		return err
	}
	files, err := r.discover()
	if err != nil {
		return err
	}
	var target *File
	for i := range files {
		if files[i].Version == version {
			target = &files[i]
		}
	}
	if target == nil {
		return fmt.Errorf("no migration with version %s in %q", version, r.Cfg.MigrationsDir)
	}
	if target.DownPath == "" {
		return fmt.Errorf("no down file for version %s", version)
	}
	label := target.Version + "_" + target.Name
	body, err := os.ReadFile(target.DownPath)
	if err != nil {
		return fmt.Errorf("migration %s: %w", label, err)
	}

	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConn(c)
	if err := r.acquireLock(ctx, c); err != nil {
		return err
	}
	defer r.releaseLock(c)

	applied, err := appliedVersions(ctx, c)
	if err != nil {
		return err
	}
	if !applied[version] {
		return fmt.Errorf("migration %s is not applied: nothing to roll back", label)
	}
	for v := range applied {
		if v > version { // four-digit versions: string order is numeric order
			return fmt.Errorf("refusing to roll back %s while higher version %s is still applied; roll back highest-first", version, v)
		}
	}

	tx, err := r.beginFileTx(ctx, c)
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", label, err)
	}
	defer rollbackQuietly(tx)
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("migration %s down: %w", label, err)
	}
	stillThere, err := rowRecorded(ctx, tx, version)
	if err != nil {
		return fmt.Errorf("migration %s down: %w", label, err)
	}
	if stillThere {
		return fmt.Errorf("migration %s down did not remove its tracking row from platform.schema_migrations (the down file must DELETE it); its transaction was rolled back", label)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migration %s down: commit: %w", label, err)
	}
	fmt.Fprintf(r.out(), "rolled back %s\n", label)
	return nil
}

// Status prints applied against pending. It takes no lock: it only reads, and it must work while a migration runs.
func (r *Runner) Status(ctx context.Context) error {
	files, err := r.discover()
	if err != nil {
		return err
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConn(c)

	applied, err := appliedVersions(ctx, c)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out(), "migrations dir: %s\n", r.Cfg.MigrationsDir)
	pending := 0
	onDisk := map[string]bool{}
	for _, f := range files {
		onDisk[f.Version] = true
		state := "PENDING"
		if applied[f.Version] {
			state = "applied"
		} else {
			pending++
		}
		fmt.Fprintf(r.out(), "  %s_%-24s %s\n", f.Version, f.Name, state)
	}
	var unknown []string
	for v := range applied {
		if !onDisk[v] {
			unknown = append(unknown, v)
		}
	}
	sort.Strings(unknown)
	for _, v := range unknown {
		fmt.Fprintf(r.out(), "  %s %-24s applied, but no file for it on disk\n", v, "")
	}
	fmt.Fprintf(r.out(), "summary: %d applied, %d pending\n", len(applied), pending)
	return nil
}

// SetAppPassword sets platform_app's login password from Cfg.AppPassword, out of band: the value is never in a
// migration file and never printed. The statement is built SERVER-SIDE with format('%L'), so the value is quoted by
// the same code that will parse it and no Go-side escaping can be wrong. Any error is passed through Redact.
func (r *Runner) SetAppPassword(ctx context.Context) error {
	pw := r.Cfg.AppPassword
	if strings.TrimSpace(pw) == "" {
		return configErr("%s is empty: refusing to set an empty %s password", EnvAppPassword, "platform_app")
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConn(c)

	var stmt string
	if err := c.QueryRow(ctx, "SELECT format('ALTER ROLE platform_app LOGIN PASSWORD %L', $1::text)", pw).Scan(&stmt); err != nil {
		return fmt.Errorf("format alter role: %w", redactErr(err, pw))
	}
	if _, err := c.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("alter role platform_app: %w", redactErr(err, pw))
	}
	fmt.Fprintln(r.out(), "set-app-password: platform_app password set (value not logged)")
	return nil
}
