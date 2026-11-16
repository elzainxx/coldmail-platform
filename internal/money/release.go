package money

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ReleaseResult is what a release returned. Released is the credits that went back from reserved to available (0 when there was nothing left); Replayed is
// true when the reserve had already been released and this call found nothing left.
type ReleaseResult struct {
	ReserveID int64
	Released  int64
	Replayed  bool
}

// Release returns the unused remainder of the job's reserve to the user's available credits, once, in ONE transaction (see ReleaseIn).
func (s *Store) Release(ctx context.Context, jobID string) (ReleaseResult, error) {
	if err := validateRelease(jobID); err != nil {
		return ReleaseResult{}, opErr("release", err)
	}
	var res ReleaseResult
	err := s.run(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = s.release(ctx, tx, jobID)
		return err
	})
	if err != nil {
		return ReleaseResult{}, opErr("release", err)
	}
	return res, nil
}

// ReleaseIn is Release inside a transaction the caller owns. The job must have ended (succeeded, failed or cancelled: ErrJobNotEnded otherwise) and hold a
// reserve (ErrNoReserve). The remainder is reserved - charged - released, read under the (user, module) lock; a remainder of 0 writes nothing (and says
// Replayed when a release row already exists); otherwise ONE release row of exactly the remainder is inserted ('release:<reserve id>', ON CONFLICT DO NOTHING),
// which the database checks against platform.reserves.
func (s *Store) ReleaseIn(ctx context.Context, tx pgx.Tx, jobID string) (ReleaseResult, error) {
	if err := validateRelease(jobID); err != nil {
		return ReleaseResult{}, opErr("release", err)
	}
	res, err := s.release(ctx, tx, jobID)
	return res, opErr("release", err)
}

func (s *Store) release(ctx context.Context, tx pgx.Tx, jobID string) (ReleaseResult, error) {
	if err := s.bound(ctx, tx); err != nil {
		return ReleaseResult{}, err
	}
	job, err := s.meteredJob(ctx, tx, jobID)
	if err != nil {
		return ReleaseResult{}, err
	}
	switch job.status {
	case "succeeded", "failed", "cancelled": // terminal: a job never leaves these
	default:
		return ReleaseResult{}, fmt.Errorf("%w: job %s is %s", ErrJobNotEnded, jobID, job.status)
	}
	if err := s.lockUser(ctx, tx, job.user, job.module); err != nil {
		return ReleaseResult{}, err
	}
	rsv, err := s.reserveOf(ctx, tx, jobID)
	if err != nil {
		return ReleaseResult{}, err
	}
	left := rsv.remainder()
	if left < 0 {
		return ReleaseResult{}, fmt.Errorf("the reserve %d of job %s has a negative remainder %d: the reserves table breaks its own CHECK", rsv.id, jobID, left)
	}
	if left == 0 {
		var released bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform.ledger WHERE reserve_id = $1 AND kind = 'release')`, rsv.id).Scan(&released)
		if err != nil {
			return ReleaseResult{}, s.fail(err, pgCtx{})
		}
		return ReleaseResult{ReserveID: rsv.id, Released: 0, Replayed: released}, nil
	}

	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, idempotency_key)
		VALUES ($1::uuid, $2, 'release', $3, $4, $5::uuid, $6, $7)
		ON CONFLICT DO NOTHING
		RETURNING id`, job.user, job.module, left, -left, jobID, rsv.id, "release:"+strconv.FormatInt(rsv.id, 10)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseResult{}, fmt.Errorf("%w: a release of reserve %d already exists but %d credit(s) remain", ErrConflict, rsv.id, left)
	}
	if err != nil {
		return ReleaseResult{}, s.fail(err, pgCtx{})
	}
	return ReleaseResult{ReserveID: rsv.id, Released: left}, nil
}
