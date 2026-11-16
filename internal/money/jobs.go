package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// jobRow is what an operation needs to know about a job before it can take the lock of the job's (user, module).
type jobRow struct {
	user    string
	module  string
	status  string
	metered bool
}

// job reads a job and its module's metering with a plain SELECT (no row lock). user_id and module_id never change (platform.jobs_guard), so they are safe to
// use for the lock key; the status is read AGAIN under the lock by the operations that decide on it.
func (s *Store) job(ctx context.Context, tx pgx.Tx, jobID string) (jobRow, error) {
	const q = `SELECT j.user_id::text, j.module_id, j.status, m.metered
		FROM platform.jobs j JOIN platform.modules m ON m.id = j.module_id
		WHERE j.id = $1::uuid`
	var j jobRow
	err := tx.QueryRow(ctx, q, jobID).Scan(&j.user, &j.module, &j.status, &j.metered)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobRow{}, fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	}
	if err != nil {
		return jobRow{}, s.fail(err, pgCtx{})
	}
	return j, nil
}

// meteredJob reads the job and refuses a job of an unmetered module (module 3 holds no credits, so nothing here applies to it).
func (s *Store) meteredJob(ctx context.Context, tx pgx.Tx, jobID string) (jobRow, error) {
	j, err := s.job(ctx, tx, jobID)
	if err != nil {
		return jobRow{}, err
	}
	if !j.metered {
		return jobRow{}, fmt.Errorf("%w: job %s belongs to module %s", ErrNotMetered, jobID, j.module)
	}
	return j, nil
}

// reserveRow is a job's reserve as platform.reserves holds it.
type reserveRow struct {
	id       int64
	reserved int64
	charged  int64
	released int64
}

func (r reserveRow) remainder() int64 { return r.reserved - r.charged - r.released }

// reserveOf reads the job's reserve, or ErrNoReserve.
func (s *Store) reserveOf(ctx context.Context, tx pgx.Tx, jobID string) (reserveRow, error) {
	const q = `SELECT reserve_id, reserved, charged, released FROM platform.reserves WHERE job_id = $1::uuid`
	var r reserveRow
	err := tx.QueryRow(ctx, q, jobID).Scan(&r.id, &r.reserved, &r.charged, &r.released)
	if errors.Is(err, pgx.ErrNoRows) {
		return reserveRow{}, fmt.Errorf("%w: job %s", ErrNoReserve, jobID)
	}
	if err != nil {
		return reserveRow{}, s.fail(err, pgCtx{})
	}
	return r, nil
}

// jobPrices reads the job's FROZEN price snapshot (platform.job_unit_prices). Settlement reads this and never platform.unit_costs.
func (s *Store) jobPrices(ctx context.Context, tx pgx.Tx, jobID string) (map[string]int, error) {
	rows, err := tx.Query(ctx, `SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1::uuid`, jobID)
	if err != nil {
		return nil, s.fail(err, pgCtx{})
	}
	ps, err := s.scanPrices(rows, pgCtx{})
	if err != nil {
		return nil, err
	}
	return priceMap(ps), nil
}
