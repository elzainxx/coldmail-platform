package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Leader election for the two loops (Phase 2 close-out D9, finding F36).
//
// More than one platform instance may run against one database (a rolling restart, a second node for availability), and every instance serves HTTP. The expiry and reconcile loops, though, must be
// RUN by one instance at a time: two runners are safe (the expiry is idempotent per user by the ledger's unique key, the reconciler's writes are compare-and-set on the wanted version) but they
// double the calls to module 3, race each other's retries and make every log twice as hard to read. So the loops run only in the instance that holds the leader advisory lock (LeaderLockKey).
//
//   - The lock is SESSION-level and is taken with pg_try_advisory_lock, which never waits, on ONE dedicated connection (pool.Acquire once, kept for as long as the instance leads). PostgreSQL
//     frees a session lock the instant its session ends, so a leader that crashes, is killed or loses its connection frees it without any lease, heartbeat or clock.
//   - Every tick of both loops begins with Leader.Ensure: a standby tries the lock (non-blocking) and does nothing else this tick; a leader confirms its session is alive and still holds the lock.
//     Any error on the leader's session drops the leadership (the connection is closed, never returned to the pool while it may hold the lock), logs ONCE at warn, and the next tick tries again.
//   - On shutdown the leader unlocks explicitly and returns the connection to the pool BEFORE the pool closes (pgxpool's Close waits for every checked-out connection, so a held one would hang the
//     process). The session ends with the process anyway; the explicit unlock is what makes a planned stop hand over at once instead of when the server notices the closed socket.
//
// What this does NOT promise: a pass already running when the lock is lost runs to its end (bounded by its own timeout) while the next leader may have started; the database's constraints, not the
// lock, are what keep that safe. The lock needs a connection that stays attached to one backend, so the platform's connection string must be the direct or a session-mode one, not a
// transaction-mode pooler (Docs/install.md).

// MaxInstanceIDBytes is the longest instance name: PostgreSQL cuts application_name at 63 bytes.
const MaxInstanceIDBytes = 63

// defaultInstanceID is the name of an instance that was given none and whose host name cannot be read.
const defaultInstanceID = "platform"

// Bounds of the database calls of the election. They are limits so that an unreachable database costs a loop one bounded wait per tick, not a hung tick; they are not dials.
const (
	leaderOpTimeout      = 10 * time.Second // one try, one check or one unlock
	leaderDropTimeout    = 3 * time.Second  // the clean close of a connection that is being thrown away
	leaderReleaseTimeout = 10 * time.Second // the whole release on shutdown
)

// The statements of the election. Every one names its schema (pg_catalog).
const (
	// The try also reads the id of the backend that took the lock. A session lock lives exactly as long as its backend and only this code unlocks it, so "the same backend, still answering" IS "the
	// lock is still held"; and a connection that was swapped for another backend behind a transaction pooler answers with a different id and is not trusted. (The check does not read pg_locks: that
	// view is for the operator and the tests, and a role without the right to read it must not be able to make the leader give up every tick.)
	sqlLeaderTry    = `SELECT pg_catalog.pg_try_advisory_lock($1), pg_catalog.pg_backend_pid()`
	sqlLeaderPID    = `SELECT pg_catalog.pg_backend_pid()`
	sqlLeaderName   = `SELECT pg_catalog.set_config('application_name', $1, false)`
	sqlLeaderUnlock = `SELECT pg_catalog.pg_advisory_unlock($1)`
	sqlLeaderUnname = `RESET application_name`
)

// errLeaderNotHeld is what Release reports when the lock it was about to release was not held (it was lost before the unlock).
var errLeaderNotHeld = errors.New("entitlements: the leader lock was not held at the release")

// leaderReason is the text a log line carries for a failure of the election: the failure class (never the driver's text, which names the host, the login and the database), or the one
// condition that is not an infrastructure error.
func leaderReason(err error) string {
	if errors.Is(err, errLeaderNotHeld) {
		return "the lock was not held at the release"
	}
	return errClass(err)
}

// ValidInstanceID reports whether s can name an instance: 1 to MaxInstanceIDBytes bytes, each printable ASCII (0x20 to 0x7e), which is what PostgreSQL keeps of an application_name as written.
// internal/config applies the same rule to PLATFORM_INSTANCE_ID; a test in each package pins the same cases.
func ValidInstanceID(s string) bool {
	if len(s) < 1 || len(s) > MaxInstanceIDBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ResolveInstanceID returns the name an instance goes by: the configured one when there is one (refused if it is not a ValidInstanceID), otherwise the host name, cut to MaxInstanceIDBytes with
// every byte outside printable ASCII replaced by '-', otherwise "platform". The configuration leaves an unset PLATFORM_INSTANCE_ID empty and this is where it becomes the host name.
func ResolveInstanceID(configured string) (string, error) {
	if configured != "" {
		if !ValidInstanceID(configured) {
			return "", fmt.Errorf("entitlements: the instance id must be 1 to %d printable ASCII bytes", MaxInstanceIDBytes)
		}
		return configured, nil
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return defaultInstanceID, nil
	}
	b := []byte(host)
	if len(b) > MaxInstanceIDBytes {
		b = b[:MaxInstanceIDBytes]
	}
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '-'
		}
	}
	return string(b), nil
}

// lockTaker is the part of the election that touches the database: ONE dedicated session that can hold the leader lock. The real one is poolLock; a unit test supplies a fake, so the state machine
// is proved without a database (the database tests then prove the real one). A taker is not safe for concurrent use: Leader serialises every call.
type lockTaker interface {
	// TryAcquire takes the lock without waiting. (true, nil): this taker now holds it, on a session it keeps until Release or a failure. (false, nil): another session holds it and this taker holds
	// nothing. An error leaves nothing held: whatever session was opened has been closed.
	TryAcquire(ctx context.Context) (bool, error)
	// StillHeld confirms that the session is alive and is still the one that took the lock. (true, nil): yes. (false, nil): the session answered as another backend. An error: the session is broken. In both of
	// the last two cases the taker has already closed the session and holds nothing.
	StillHeld(ctx context.Context) (bool, error)
	// Release unlocks explicitly and returns the connection. With nothing held it does nothing. Whatever fails, the session is closed, so the lock cannot outlive the call.
	Release(ctx context.Context) error
}

// poolLock is the real lockTaker: the leader lock on a connection checked out of the pool and kept.
type poolLock struct {
	pool     *pgxpool.Pool
	key      int64
	instance string
	conn     *pgxpool.Conn // the dedicated session; nil unless this taker holds the lock
	pid      int32         // the backend that took the lock: the session is trusted only while it answers as that backend
}

// discard closes a connection instead of returning it to the pool: the connection is hijacked (so the pool's slot is free at once and pool.Close never waits for it) and closed, which ends the
// session and with it any lock it holds. It is how every uncertain connection leaves: the pool only ever receives a connection that is known to hold nothing.
func discard(c *pgxpool.Conn) {
	raw := c.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), leaderDropTimeout)
	defer cancel()
	_ = raw.Close(ctx)
}

func (p *poolLock) drop() {
	c := p.conn
	p.conn = nil
	if c != nil {
		discard(c)
	}
}

func (p *poolLock) TryAcquire(ctx context.Context) (bool, error) {
	if p.conn != nil { // Leader asks only when it believes nothing is held; if a session is still here, look at it instead of taking the same lock twice (a session lock stacks)
		held, err := p.StillHeld(ctx)
		if err != nil || held {
			return held, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, leaderOpTimeout)
	defer cancel()
	c, err := p.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	var (
		got bool
		pid int32
	)
	if err := c.QueryRow(ctx, sqlLeaderTry, p.key).Scan(&got, &pid); err != nil {
		discard(c) // the answer is unknown: the lock may be held on this session
		return false, err
	}
	if !got {
		c.Release() // nothing is held on it, so it goes back to the pool as it is
		return false, nil
	}
	// The lock is ours. Name the session so that pg_stat_activity shows which instance leads; only this connection is renamed, and Release resets the name.
	if _, err := c.Exec(ctx, sqlLeaderName, p.instance); err != nil {
		discard(c) // closing the session releases the lock we just took
		return false, err
	}
	p.conn, p.pid = c, pid
	return true, nil
}

func (p *poolLock) StillHeld(ctx context.Context) (bool, error) {
	if p.conn == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, leaderOpTimeout)
	defer cancel()
	var pid int32
	if err := p.conn.QueryRow(ctx, sqlLeaderPID).Scan(&pid); err != nil {
		p.drop()
		return false, err
	}
	if pid != p.pid { // another backend answered: this connection is not the session that took the lock
		p.drop()
		return false, nil
	}
	return true, nil
}

func (p *poolLock) Release(ctx context.Context) error {
	c := p.conn
	if c == nil {
		return nil
	}
	p.conn = nil
	ctx, cancel := context.WithTimeout(ctx, leaderOpTimeout)
	defer cancel()
	var unlocked bool
	if err := c.QueryRow(ctx, sqlLeaderUnlock, p.key).Scan(&unlocked); err != nil {
		discard(c)
		return err
	}
	if !unlocked {
		discard(c)
		return errLeaderNotHeld
	}
	if _, err := c.Exec(ctx, sqlLeaderUnname); err != nil {
		discard(c) // the lock is released; only the name could not be reset, so this connection does not go back to the pool
		return nil
	}
	c.Release()
	return nil
}

// Leader is the election of one instance. Ensure is what every tick of the loops asks; Status is what Health reports; Release is what shutdown calls.
type Leader struct {
	taker    lockTaker
	log      *slog.Logger
	now      func() time.Time
	instance string

	op ctxLock // serialises Ensure and Release: the taker holds one session and is not safe for concurrent use. An Ensure whose context ends while it waits for the lock gives up (see ctxLock)
	// owned by op:
	failClass     string // the failure class last warned about, "" while the lock can be tried; the same class again is quiet
	standbyLogged bool   // "another instance leads" has been written for this stretch of standby

	mu      sync.Mutex // guards what Status reads; never held across a database call, so Status never waits for one
	leading bool
	since   time.Time
	decided bool // Ensure has answered at least once
}

// ctxLock is a mutex whose waiter can give up when its context ends. The two loops of the entitlement service and the job engine (Phase 4) share ONE Leader, and the engine is stopped before the loops:
// with a plain mutex the engine's last Ensure would wait for a loop's Ensure that is still in a database call (a hung connect costs its bounded wait, ten seconds) and that call's own context is
// not cancelled until the engine has stopped. The zero value is ready to use.
type ctxLock struct {
	once sync.Once
	ch   chan struct{}
}

func (c *ctxLock) init() { c.once.Do(func() { c.ch = make(chan struct{}, 1) }) }

// Lock waits for the lock without limit.
func (c *ctxLock) Lock() {
	c.init()
	c.ch <- struct{}{}
}

// LockCtx waits for the lock until ctx ends; it reports whether it holds the lock.
func (c *ctxLock) LockCtx(ctx context.Context) bool {
	c.init()
	select {
	case c.ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Unlock releases the lock.
func (c *ctxLock) Unlock() { <-c.ch }

// LeaderStatus is a snapshot of an election.
type LeaderStatus struct {
	// Leading is true while this instance holds the lock and so runs the loops; Since is when it took it (zero otherwise).
	Leading bool
	Since   time.Time
	// Decided is true once the first Ensure has answered: before it, nothing is known about this instance's role.
	Decided bool
}

// NewLeader builds the election of one instance over pool. instance names the session that holds the lock (see ResolveInstanceID: empty means the host name); log and now may be nil.
func NewLeader(pool *pgxpool.Pool, instance string, log *slog.Logger, now func() time.Time) (*Leader, error) {
	if pool == nil {
		return nil, errors.New("entitlements: NewLeader needs a pool")
	}
	id, err := ResolveInstanceID(instance)
	if err != nil {
		return nil, err
	}
	return newLeader(&poolLock{pool: pool, key: LeaderLockKey, instance: id}, id, log, now), nil
}

// newLeader is NewLeader with the lock-taker supplied (the seam of the unit tests).
func newLeader(taker lockTaker, instance string, log *slog.Logger, now func() time.Time) *Leader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &Leader{taker: taker, log: log, now: now, instance: instance}
}

// Instance is the name this election goes by.
func (l *Leader) Instance() string { return l.instance }

// Status returns a snapshot.
func (l *Leader) Status() LeaderStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LeaderStatus{Leading: l.leading, Since: l.since, Decided: l.decided}
}

func (l *Leader) isLeading() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading
}

// publish records the outcome of an Ensure.
func (l *Leader) publish(leading bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.decided = true
	if leading && !l.leading {
		l.since = l.now()
	}
	if !leading {
		l.since = time.Time{}
	}
	l.leading = leading
}

// Ensure is asked at the START of every tick of both loops and answers whether this instance leads, so whether the tick may run its pass. A leader confirms that its session is alive and holds the
// lock; a standby tries the lock without waiting. Either way it does nothing else: a false answer means the caller does nothing at all this tick (no pass, no call to module 3).
//
// A failure never panics and never ends a loop: a leader whose session fails drops the leadership (once, at warn) and answers false; the next tick tries again as a standby would. A
// ctx that is cancelled answers false and logs nothing, since a shutdown is not a failure.
func (l *Leader) Ensure(ctx context.Context) bool {
	if !l.op.LockCtx(ctx) { // another Ensure is in a database call and this caller's shutdown has begun: a hung connect of the other must not hold this one up
		return false
	}
	defer l.op.Unlock()
	if ctx.Err() != nil {
		return false
	}

	if l.isLeading() {
		held, err := l.taker.StillHeld(ctx)
		if err == nil && held {
			return true
		}
		l.publish(false)
		l.standbyLogged = false
		reason := "the lock is no longer held by this session"
		if err != nil {
			reason = leaderReason(err)
		}
		if ctx.Err() == nil {
			l.failClass = reason
			l.log.WarnContext(ctx, "loops: this instance lost the leader lock; its loops idle and it tries again at the next tick", "instance", l.instance, "reason", reason)
		}
		return false
	}

	acquired, err := l.taker.TryAcquire(ctx)
	if err != nil {
		l.publish(false)
		if ctx.Err() == nil {
			if class := leaderReason(err); class != l.failClass {
				l.failClass = class
				l.log.WarnContext(ctx, "loops: the leader lock cannot be tried; this instance idles its loops and tries again at the next tick", "instance", l.instance, "reason", class)
			}
		}
		return false
	}
	l.failClass = ""
	if !acquired {
		l.publish(false)
		if !l.standbyLogged {
			l.standbyLogged = true
			l.log.InfoContext(ctx, "loops: another instance holds the leader lock; this instance idles its loops and takes over if that one stops", "instance", l.instance)
		}
		return false
	}
	l.publish(true)
	l.standbyLogged = false
	l.log.InfoContext(ctx, "loops: this instance is now the leader", "instance", l.instance)
	return true
}

// Release gives the leadership up: it unlocks explicitly and returns the dedicated connection to the pool. The service calls it after both loops have stopped and BEFORE the pool is closed (a
// checked-out connection would hold pool.Close for ever). With nothing held it does nothing. ctx should not be the cancelled context of the shutdown.
func (l *Leader) Release(ctx context.Context) error {
	l.op.Lock()
	defer l.op.Unlock()
	was := l.isLeading()
	err := l.taker.Release(ctx)
	l.mu.Lock()
	l.leading, l.since = false, time.Time{}
	l.mu.Unlock()
	if err != nil {
		l.log.WarnContext(ctx, "loops: releasing the leader lock failed; its session was closed, which releases it", "instance", l.instance, "reason", leaderReason(err))
		return err
	}
	if was {
		l.log.InfoContext(ctx, "loops: this instance released the leader lock", "instance", l.instance)
	}
	return nil
}
