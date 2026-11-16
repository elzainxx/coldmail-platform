package money

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
)

// SettleItem is one item of a job as a module reported it: the module's own id for it (ItemRef) and what happened (Outcome). charge.Decide turns the
// outcome into money.
type SettleItem struct {
	ItemRef string // non-blank, at most 200 bytes, unique within one call
	Outcome charge.Outcome
}

// SettleReq settles a page of a job's items.
type SettleReq struct {
	JobID string
	Items []SettleItem // at most MaxSettleItems
}

// UnmappedItem is an item whose outcome the charge table cannot classify: flagged to the caller, never charged, never guessed.
type UnmappedItem struct {
	ItemRef string
	Rule    string // the id of the charge-table row that said "unmapped" ("M1-18"), or "none"
	Note    string
}

// SettleResult counts what one Settle call did. Charged and Credits are what THIS call newly wrote; an item the ledger already charged is in
// AlreadySettled (whatever this call's outcome says: the first charge stands); Free, NotFinal and Unmapped items wrote nothing.
type SettleResult struct {
	Charged        int
	Credits        int64
	AlreadySettled int
	Free           int
	NotFinal       int
	Unmapped       []UnmappedItem
	ByUnit         map[string]int // newly charged items per unit
}

// Settle charges a page of a job's items against the job's reserve, in ONE transaction (see SettleIn).
func (s *Store) Settle(ctx context.Context, req SettleReq) (SettleResult, error) {
	if err := validateSettle(req); err != nil {
		return SettleResult{}, opErr("settle", err)
	}
	var res SettleResult
	err := s.run(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = s.settle(ctx, tx, req)
		return err
	})
	if err != nil {
		return SettleResult{}, opErr("settle", err)
	}
	return res, nil
}

// SettleIn is Settle inside a transaction the caller owns. The steps:
//  1. the job must exist, belong to a metered module and have left draft; the (user, module) lock is taken; the job must hold a reserve (ErrNoReserve);
//  2. the prices come from the job's FROZEN snapshot (platform.job_unit_prices), never from platform.unit_costs;
//  3. an item whose item_ref the ledger already charged is AlreadySettled and nothing more is done with it;
//  4. every other item goes through charge.Decide: Charge is priced from the snapshot (a unit with no frozen price is ErrUnpriced), Free and NotFinal are
//     counted and write nothing, Unmapped is collected and writes nothing;
//  5. the charges of THIS call must fit what the reserve still holds (reserved - charged - released), else ErrReserveExceeded, and nothing is written;
//  6. each charge is one ledger row (units = 1, the frozen price, item_ref, key 'charge:<job>:<item_ref>') inserted ON CONFLICT DO NOTHING: a row the
//     database refuses as a duplicate counts as AlreadySettled. The database's unique index on (job_id, item_ref) is what holds the idempotence, whatever
//     key a writer derived.
//
// Everything is decided before the first write, so a refusal leaves the caller's transaction clean; a database error aborts it and the caller rolls back.
func (s *Store) SettleIn(ctx context.Context, tx pgx.Tx, req SettleReq) (SettleResult, error) {
	if err := validateSettle(req); err != nil {
		return SettleResult{}, opErr("settle", err)
	}
	res, err := s.settle(ctx, tx, req)
	return res, opErr("settle", err)
}

// settleChunk is how many charge inserts travel in one round trip.
const settleChunk = 500

type pendingCharge struct {
	ref   string
	unit  string
	price int
}

func (s *Store) settle(ctx context.Context, tx pgx.Tx, req SettleReq) (SettleResult, error) {
	res := SettleResult{ByUnit: map[string]int{}}
	job, err := s.meteredJob(ctx, tx, req.JobID)
	if err != nil {
		return SettleResult{}, err
	}
	if err := s.lock(ctx, tx, job.user, job.module); err != nil {
		return SettleResult{}, err
	}
	job, err = s.job(ctx, tx, req.JobID) // the status, read again under the lock
	if err != nil {
		return SettleResult{}, err
	}
	if job.status == "draft" {
		return SettleResult{}, fmt.Errorf("%w: job %s is still a draft, a settle needs a job that has left draft", ErrJobState, req.JobID)
	}
	rsv, err := s.reserveOf(ctx, tx, req.JobID)
	if err != nil {
		return SettleResult{}, err
	}
	prices, err := s.jobPrices(ctx, tx, req.JobID)
	if err != nil {
		return SettleResult{}, err
	}

	// which of these items did the ledger already charge?
	settled := map[string]bool{}
	if len(req.Items) > 0 {
		refs := make([]string, len(req.Items))
		for i, it := range req.Items {
			refs[i] = it.ItemRef
		}
		rows, err := tx.Query(ctx, `SELECT item_ref FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge' AND item_ref = ANY($2::text[])`, req.JobID, refs)
		if err != nil {
			return SettleResult{}, s.fail(err, pgCtx{})
		}
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				rows.Close()
				return SettleResult{}, s.fail(err, pgCtx{})
			}
			settled[ref] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return SettleResult{}, s.fail(err, pgCtx{})
		}
	}

	var pending []pendingCharge
	var sum int64
	for i, it := range req.Items {
		if settled[it.ItemRef] {
			res.AlreadySettled++
			continue
		}
		d := charge.Decide(it.Outcome)
		switch d.Kind {
		case charge.Charge:
			p, ok := prices[d.Unit]
			if !ok {
				return SettleResult{}, fmt.Errorf("%w: item %d (rule %s) is charged as unit %q, which job %s has no frozen price for", ErrUnpriced, i, d.Rule, d.Unit, req.JobID)
			}
			pending = append(pending, pendingCharge{ref: it.ItemRef, unit: d.Unit, price: p})
			sum += int64(p)
		case charge.Free:
			res.Free++
		case charge.NotFinal:
			res.NotFinal++
		case charge.Unmapped:
			res.Unmapped = append(res.Unmapped, UnmappedItem{ItemRef: it.ItemRef, Rule: d.Rule, Note: d.Note})
		default:
			return SettleResult{}, invalid("items", "item %d: the charge table returned an unknown decision kind %d", i, int(d.Kind))
		}
	}

	if left := rsv.remainder(); sum > left {
		return SettleResult{}, fmt.Errorf("%w: this call charges %d credit(s) for %d new item(s) but the reserve of job %s has %d left (reserved %d, charged %d, released %d)",
			ErrReserveExceeded, sum, len(pending), req.JobID, left, rsv.reserved, rsv.charged, rsv.released)
	}

	const insert = `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, unit, units, unit_cost, item_ref, idempotency_key)
		VALUES ($1::uuid, $2, 'charge', 0, $3, $4::uuid, $5, $6, 1, $7, $8, $9)
		ON CONFLICT DO NOTHING`
	for start := 0; start < len(pending); start += settleChunk {
		chunk := pending[start:min(start+settleChunk, len(pending))]
		var b pgx.Batch
		for _, c := range chunk {
			b.Queue(insert, job.user, job.module, -int64(c.price), req.JobID, rsv.id, c.unit, c.price, c.ref, "charge:"+req.JobID+":"+c.ref)
		}
		br := tx.SendBatch(ctx, &b)
		for _, c := range chunk {
			tag, err := br.Exec()
			if err != nil {
				_ = br.Close()
				return SettleResult{}, s.fail(err, pgCtx{})
			}
			if tag.RowsAffected() == 0 {
				res.AlreadySettled++ // the database's key refused a duplicate: the first charge stands
				continue
			}
			res.Charged++
			res.Credits += int64(c.price)
			res.ByUnit[c.unit]++
		}
		if err := br.Close(); err != nil {
			return SettleResult{}, s.fail(err, pgCtx{})
		}
	}
	return res, nil
}
