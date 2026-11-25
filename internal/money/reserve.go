package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReserveReq holds credits for a job: Units of the job's module at the dearest of the module's unit prices.
type ReserveReq struct {
	JobID string
	Units int // 1 .. 2^31-1
}

// ReserveResult is the reserve row and the price snapshot frozen with it.
type ReserveResult struct {
	ReserveID int64
	Unit      string         // the unit whose price is the reserve rate (the dearest; a tie goes to the lexicographically first unit)
	UnitCost  int            // the reserve rate, credits per unit
	Units     int            // units reserved
	Reserved  int64          // Units * UnitCost, credits held
	Prices    map[string]int // the frozen snapshot: every unit of the job's module and its price at that instant
	Replayed  bool           // true when the job already held this reserve and this is that reserve returned again
}

// Reserve holds credits for a job in ONE transaction (see ReserveIn for the steps).
func (s *Store) Reserve(ctx context.Context, req ReserveReq) (ReserveResult, error) {
	if err := validateReserve(req); err != nil {
		return ReserveResult{}, opErr("reserve", err)
	}
	var res ReserveResult
	err := s.run(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = s.reserve(ctx, tx, req)
		return err
	})
	if err != nil {
		return ReserveResult{}, opErr("reserve", err)
	}
	return res, nil
}

// ReserveIn is Reserve inside a transaction the caller owns (the job engine confirms a job in its own transaction). It does NOT change the job's status.
//
// The steps, in this order, all in the caller's transaction:
//  0. SET LOCAL lock_timeout to the store's wait (bound): every wait below ends with ErrLockTimeout once it has lasted that long, and the setting stays on the caller's
//     transaction until it ends;
//  1. read the job (it must exist and belong to a metered module);
//  2. take the (user, module) advisory lock;
//  3. SELECT ... FROM platform.jobs WHERE id = $1 FOR UPDATE: the job row is ours until the transaction ends;
//  4. IDEMPOTENCE: when the job already holds a reserve, return it (Replayed) if the units agree, else ErrIdempotencyMismatch. This comes before the status
//     check, so a replay still answers after the engine moved the job out of draft;
//  5. the job must be a draft or (since Phase 4) previewed (ErrJobState);
//  6. SELECT ... FROM platform.unit_costs WHERE module_id = $1 FOR SHARE (EVERY unit of the module, reachable or not): a price change or a flip of the reachable flag waits until this transaction ends,
//     so the snapshot below EQUALS the table at that instant instead of racing it; the prices used are the REACHABLE units (platform_0013);
//  7. the reserve rate is the DEAREST price (ties: the lexicographically first unit); needed = units * rate; the available balance is read under the
//     lock and available < needed is ErrInsufficientCredits{Available, Needed, Shortfall}, decided BEFORE anything is written;
//  8. ONE INSERT ... SELECT copies every REACHABLE unit of the module into platform.job_unit_prices, and the ledger reserve row is inserted at the rate
//     ('reserve:<job>' is its key). The database refuses a reserve that is not at the dearest price or whose snapshot is incomplete.
//
// On any error the caller rolls its transaction back (a refusal at steps 1-7 has written nothing, so the transaction is still usable).
func (s *Store) ReserveIn(ctx context.Context, tx pgx.Tx, req ReserveReq) (ReserveResult, error) {
	if err := validateReserve(req); err != nil {
		return ReserveResult{}, opErr("reserve", err)
	}
	res, err := s.reserve(ctx, tx, req)
	return res, opErr("reserve", err)
}

func (s *Store) reserve(ctx context.Context, tx pgx.Tx, req ReserveReq) (ReserveResult, error) {
	if err := s.bound(ctx, tx); err != nil {
		return ReserveResult{}, err
	}
	job, err := s.meteredJob(ctx, tx, req.JobID)
	if err != nil {
		return ReserveResult{}, err
	}
	if err := s.lockUser(ctx, tx, job.user, job.module); err != nil {
		return ReserveResult{}, err
	}

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM platform.jobs WHERE id = $1::uuid FOR UPDATE`, req.JobID).Scan(&status); err != nil {
		return ReserveResult{}, s.fail(err, pgCtx{})
	}

	// the idempotent answer, before the status check
	prev, found, err := s.reserveEntry(ctx, tx, req.JobID)
	if err != nil {
		return ReserveResult{}, err
	}
	if found {
		if prev.units != req.Units {
			return ReserveResult{}, fmt.Errorf("%w: job %s already holds a reserve of %d unit(s), not %d", ErrIdempotencyMismatch, req.JobID, prev.units, req.Units)
		}
		prices, err := s.jobPrices(ctx, tx, req.JobID)
		if err != nil {
			return ReserveResult{}, err
		}
		return ReserveResult{ReserveID: prev.id, Unit: prev.unit, UnitCost: prev.unitCost, Units: prev.units, Reserved: prev.reserved, Prices: prices, Replayed: true}, nil
	}
	// a job that holds nothing yet: draft (Phase 2) or previewed (Phase 4: the confirm transaction reserves from previewed, then moves the job to confirmed or scheduled itself)
	if status != "draft" && status != "previewed" {
		return ReserveResult{}, fmt.Errorf("%w: job %s is %s, a reserve needs a draft or previewed job", ErrJobState, req.JobID, status)
	}

	// FOR SHARE: the module's price rows cannot change until this transaction ends. (platform_app holds a column-level UPDATE on unit_costs, which is what
	// a row lock needs; FOR KEY SHARE would not do, an UPDATE of the cost column does not conflict with it.)
	// Phase 4 (platform_0013): lock EVERY price row of the module, reachable or not, so that a flip of the reachable flag (platform.set_unit_reachable takes FOR UPDATE on the row) waits for this transaction exactly as a
	// price change does; then read the REACHABLE ones: the snapshot and the dearest rate cover only the units a job of this module can be charged under its current configuration (module 1 with the vendor fallback
	// off reserves at 1 per row, not at the vendor's 2).
	lockRows, err := tx.Query(ctx, `SELECT unit FROM platform.unit_costs WHERE module_id = $1 ORDER BY unit FOR SHARE`, job.module)
	if err != nil {
		return ReserveResult{}, s.fail(err, pgCtx{})
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil {
		return ReserveResult{}, s.fail(err, pgCtx{})
	}
	rows, err := tx.Query(ctx, `SELECT unit, cost FROM platform.unit_costs WHERE module_id = $1 AND reachable ORDER BY unit`, job.module)
	if err != nil {
		return ReserveResult{}, s.fail(err, pgCtx{})
	}
	locked, err := s.scanPrices(rows, pgCtx{})
	if err != nil {
		return ReserveResult{}, err
	}
	if len(locked) == 0 {
		return ReserveResult{}, fmt.Errorf("%w: module %s has no reachable unit price to freeze", ErrSnapshot, job.module)
	}
	rate := dearest(locked)
	needed := int64(req.Units) * int64(rate.cost)

	bal, err := s.readBalance(ctx, tx, job.user, job.module)
	if err != nil {
		return ReserveResult{}, err
	}
	if bal.Available < needed {
		return ReserveResult{}, ErrInsufficientCredits{Available: bal.Available, Needed: needed, Shortfall: needed - bal.Available}
	}
	c := pgCtx{available: bal.Available, needed: needed}

	// the snapshot: ONE statement copies every unit of the module, in the transaction that holds the share locks
	rows, err = tx.Query(ctx, `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost)
		SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.module_id = $2 AND u.reachable
		RETURNING unit, cost`, req.JobID, job.module)
	if err != nil {
		return ReserveResult{}, s.fail(err, c)
	}
	copied, err := s.scanPrices(rows, c)
	if err != nil {
		return ReserveResult{}, err
	}
	if !samePrices(locked, copied) {
		return ReserveResult{}, fmt.Errorf("%w: the %d row(s) copied differ from the %d row(s) locked (a unit was added or changed while the reserve ran): retry", ErrSnapshot, len(copied), len(locked))
	}

	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, unit, units, unit_cost, idempotency_key)
		VALUES ($1::uuid, $2, 'reserve', $3, $4, $5::uuid, $6, $7, $8, $9)
		RETURNING id`, job.user, job.module, -needed, needed, req.JobID, rate.unit, req.Units, rate.cost, "reserve:"+req.JobID).Scan(&id)
	if err != nil {
		return ReserveResult{}, s.fail(err, c)
	}
	return ReserveResult{ReserveID: id, Unit: rate.unit, UnitCost: int(rate.cost), Units: req.Units, Reserved: needed, Prices: priceMap(copied)}, nil
}

// reserveLedgerRow is the ledger's reserve row of a job.
type reserveLedgerRow struct {
	id       int64
	unit     string
	units    int
	unitCost int
	reserved int64
}

// reserveEntry reads the job's reserve row from the ledger, if there is one.
func (s *Store) reserveEntry(ctx context.Context, tx pgx.Tx, jobID string) (reserveLedgerRow, bool, error) {
	// at most one row: ledger_one_reserve_per_job
	var r reserveLedgerRow
	var units, cost int32
	err := tx.QueryRow(ctx, `SELECT id, unit, units, unit_cost, reserved_delta FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'`, jobID).
		Scan(&r.id, &r.unit, &units, &cost, &r.reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return reserveLedgerRow{}, false, nil
	}
	if err != nil {
		return reserveLedgerRow{}, false, s.fail(err, pgCtx{})
	}
	r.units, r.unitCost = int(units), int(cost)
	return r, true, nil
}

// dearest is the reserve rate: the highest price, and on a tie the lexicographically first unit. ps is not empty.
func dearest(ps []price) price {
	best := ps[0]
	for _, p := range ps[1:] {
		if p.cost > best.cost || (p.cost == best.cost && p.unit < best.unit) {
			best = p
		}
	}
	return best
}

// samePrices is true when both lists name the same units at the same prices.
func samePrices(a, b []price) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int32, len(a))
	for _, p := range a {
		m[p.unit] = p.cost
	}
	for _, p := range b {
		if c, ok := m[p.unit]; !ok || c != p.cost {
			return false
		}
	}
	return true
}
