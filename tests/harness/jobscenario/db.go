//go:build harness

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/dsnscrub"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The database half of the scenario: ONE superuser connection (the harness's own, HARNESS_ADMIN_DATABASE_URL) used to create a user, to grant credits through the ledger and to READ what the
// platform, module 1 and the ledger hold. Nothing here writes a job table: the job is created, previewed and confirmed through the platform's HTTP API, as a user would.

// terminal says whether a job status is one of the four a job never leaves.
func terminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "expired":
		return true
	}
	return false
}

// db is the scenario's connection.
type db struct {
	c   *pgx.Conn
	dsn string
}

func connect(ctx context.Context) (*db, error) {
	dsn, err := env("HARNESS_ADMIN_DATABASE_URL")
	if err != nil {
		return nil, err
	}
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, failf(exitFail, "cannot connect to the database: %v", dsnscrub.Scrub(err, dsn))
	}
	return &db{c: c, dsn: dsn}, nil
}

func (d *db) close() { _ = d.c.Close(context.Background()) }

// scrub is an error of the driver with the connection string suppressed.
func (d *db) scrub(err error) error { return dsnscrub.Scrub(err, d.dsn) }

// uuidV4 is a random lower-case uuid (version 4).
func uuidV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("no randomness: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// newUser makes a user the way the harness's other stages do (stage 9c): a login in auth.users and the platform user in public.users, the same id for both (the token's `sub` is the login).
// No GoTrue is involved. It returns the user's id.
func (d *db) newUser(ctx context.Context, tag string) (string, error) {
	id, err := uuidV4()
	if err != nil {
		return "", err
	}
	mail := fmt.Sprintf("jobs-%s-%s@example.test", tag, id[:8])
	tx, err := d.c.Begin(ctx)
	if err != nil {
		return "", d.scrub(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `INSERT INTO auth.users (id, email) VALUES ($1::uuid, $2)`, id, mail); err != nil {
		return "", failf(exitFail, "could not create the login of the scenario's user: %v", d.scrub(err))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)`, id, mail); err != nil {
		return "", failf(exitFail, "could not create the scenario's platform user: %v", d.scrub(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return "", d.scrub(err)
	}
	return id, nil
}

// grant gives the user credits for module 1 through the LEDGER, as the harness's money tests do (a `grant` row naming an active admin: the first admin run.sh made in stage 4b). The balance is the
// ledger trigger's: nothing writes platform.balances here.
func (d *db) grant(ctx context.Context, user string, credits int) error {
	return d.grantModule(ctx, user, "m1", credits)
}

// grantModule is grant for a metered module of the caller's choice (m1 or m2: module 3 holds no credits).
func (d *db) grantModule(ctx context.Context, user, module string, credits int) error {
	var admin string
	err := d.c.QueryRow(ctx, `SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at, id LIMIT 1`).Scan(&admin)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return failf(exitFail, "platform.admins holds no active admin: stage 4b (create-first-admin) did not run, and a grant names an admin")
	case err != nil:
		return d.scrub(err)
	}
	ref := "jobs-grant-" + user
	if module != "m1" {
		ref = "jobs-grant-" + module + "-" + user
	}
	if _, err := d.c.Exec(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key)
		VALUES ($1::uuid, $5, 'grant', $2, $3::uuid, 'the jobs scenario funds its user', $4)`, user, credits, admin, ref, module); err != nil {
		return failf(exitFail, "could not grant credits through the ledger: %v", d.scrub(err))
	}
	return nil
}

// jobHead is what the scenario needs of a platform.jobs row to talk about the job.
type jobHead struct {
	ID, User, Module, Status, ModuleRef, ConfirmRef, Fingerprint string
	Confirmed, Started, Finished                                 bool
}

// head reads a job, or fails saying there is none.
func (d *db) head(ctx context.Context, job string) (jobHead, error) {
	if !isUUID(job) {
		return jobHead{}, failf(exitFail, "%q is not a job id (a lower-case uuid)", job)
	}
	h := jobHead{ID: job}
	err := d.c.QueryRow(ctx, `SELECT user_id::text, module_id, status, COALESCE(module_job_ref, ''), COALESCE(confirm_key, ''), COALESCE(preview_fingerprint, ''),
			confirmed_at IS NOT NULL, started_at IS NOT NULL, finished_at IS NOT NULL
		FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&h.User, &h.Module, &h.Status, &h.ModuleRef, &h.ConfirmRef, &h.Fingerprint, &h.Confirmed, &h.Started, &h.Finished)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return jobHead{}, failf(exitFail, "job %s does not exist in platform.jobs", job)
	case err != nil:
		return jobHead{}, d.scrub(err)
	}
	return h, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// event is one row of platform.job_events.
type event struct {
	ID                                  int64
	Kind, From, To, Actor, Reason, When string
}

func (d *db) events(ctx context.Context, job string) ([]event, error) {
	rows, err := d.c.Query(ctx, `SELECT id, kind, COALESCE(from_status, ''), COALESCE(to_status, ''), actor_kind, reason, created_at FROM platform.job_events WHERE job_id = $1::uuid ORDER BY id`, job)
	if err != nil {
		return nil, d.scrub(err)
	}
	defer rows.Close()
	var out []event
	for rows.Next() {
		var e event
		var at time.Time
		if err := rows.Scan(&e.ID, &e.Kind, &e.From, &e.To, &e.Actor, &e.Reason, &at); err != nil {
			return nil, d.scrub(err)
		}
		e.When = at.UTC().Format(time.RFC3339Nano)
		out = append(out, e)
	}
	return out, d.scrub(rows.Err())
}

// printEvents prints the status history of a job, one fact per event.
func printEvents(evs []event) {
	for _, e := range evs {
		fact("event", "id", e.ID, "kind", e.Kind, "from", e.From, "to", e.To, "actor", e.Actor, "at", e.When, "reason", clip(e.Reason, 120))
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stateOf is what the database holds of one job, as ordered name/value pairs (the order is the order of the line printed). Every value is an int64 or a string, so a condition can compare them.
type kv struct {
	Name  string
	Value any
}

type snapshot struct {
	Head jobHead
	Vals []kv
}

func (s snapshot) get(name string) (string, bool) {
	for _, p := range s.Vals {
		if p.Name == name {
			return fmt.Sprint(p.Value), true
		}
	}
	return "", false
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// snapshotOf reads the job's numbers in two statements (one over the job, one over what hangs off it): what the platform recorded, what the ledger holds, and what module 1 holds for the user. They are
// not one transaction, so a job that is moving can differ between the two; the stages read them when nothing moves (a killed platform, a stopped module) or compare orders of magnitude.
func (d *db) snapshotOf(ctx context.Context, job string) (snapshot, error) {
	h, err := d.head(ctx, job)
	if err != nil {
		return snapshot{}, err
	}
	var rows, pending, stamped, results, moduleResults, reserves, charges, releases int64
	var rsvReserved, rsvCharged, rsvReleased, running, stale, flags, availableNow, reservedNow, m1Jobs, invented int64
	err = d.c.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid),
		(SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending'),
		(SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND submitted_at IS NOT NULL),
		(SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid),
		(SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'module'),
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'),
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge'),
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'release'),
		COALESCE((SELECT reserved FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT charged FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT released FROM platform.reserves WHERE job_id = $1::uuid), 0),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND to_status = 'running'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'stale'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'flag'),
		COALESCE((SELECT available FROM platform.balances WHERE user_id = $2::uuid AND module_id = $4), 0),
		COALESCE((SELECT reserved FROM platform.balances WHERE user_id = $2::uuid AND module_id = $4), 0),
		(SELECT count(*) FROM verify.jobs WHERE idem_caller_id = $3),
		(SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'platform' AND module_state NOT IN ('malformed', 'duplicate', 'suppressed', 'excluded', 'missing_field', 'unrenderable'))`, job, h.User, tenant.M1CallerID(h.User), h.Module).
		Scan(&rows, &pending, &stamped, &results, &moduleResults, &reserves, &charges, &releases, &rsvReserved, &rsvCharged, &rsvReleased, &running, &stale, &flags, &availableNow, &reservedNow, &m1Jobs, &invented)
	if err != nil {
		return snapshot{}, d.scrub(err)
	}
	return snapshot{Head: h, Vals: []kv{
		{"status", h.Status}, {"module_ref", h.ModuleRef}, {"confirm_key_set", b2i(h.ConfirmRef != "")},
		{"confirmed", b2i(h.Confirmed)}, {"started", b2i(h.Started)}, {"finished", b2i(h.Finished)},
		{"rows", rows}, {"pending", pending}, {"stamped", stamped},
		{"results", results}, {"results_module", moduleResults}, {"results_platform", results - moduleResults}, {"results_invented", invented},
		{"reserves", reserves}, {"charges", charges}, {"releases", releases},
		{"reserve_reserved", rsvReserved}, {"reserve_charged", rsvCharged}, {"reserve_released", rsvReleased}, {"remainder", rsvReserved - rsvCharged - rsvReleased},
		{"balance_available", availableNow}, {"balance_reserved", reservedNow},
		{"events_running", running}, {"events_stale", stale}, {"events_flag", flags},
		{"module_jobs", m1Jobs},
	}}, nil
}

// holders reads the engine's leader lock from the server, as the harness's stage 9c does: who holds the session advisory lock 7200000002 (the application_name of the session that holds it is the
// instance's name) and how many such locks are granted.
func (d *db) holders(ctx context.Context) (names []string, locks int, err error) {
	rows, err := d.c.Query(ctx, `SELECT a.application_name FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		  AND l.database = (SELECT d.oid FROM pg_database d WHERE d.datname = current_database())
		  AND ((l.classid::bigint << 32) | l.objid::bigint) = 7200000002
		ORDER BY a.application_name`)
	if err != nil {
		return nil, 0, d.scrub(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, 0, d.scrub(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, d.scrub(err)
	}
	return names, len(names), nil
}

func joinNames(n []string) string { return strings.Join(n, ",") }
