//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
)

// The money store (internal/money) against a REAL migrated database, as a REAL platform_app login through a pgxpool: the privileges are the live service's, so the store cannot
// write a balance, cannot row-lock one, and its only serialiser is the advisory lock. Every test has its own scratch database and a paired control: a refusal is shown beside the
// legal request that is not refused, and every refusal asserts the NAMED reason (a sentinel, a field, a constraint), never only "an error".
// The concurrency proofs are in money_concurrency_test.go; the pure functions (lock key, validation, error mapping) are unit tests in internal/money.

// mnyRequireSettle asserts every counter of a SettleResult.
func mnyRequireSettle(t *testing.T, what string, got money.SettleResult, charged int, credits int64, already, free, notFinal, unmapped int) {
	t.Helper()
	if got.Charged != charged || got.Credits != credits || got.AlreadySettled != already || got.Free != free || got.NotFinal != notFinal || len(got.Unmapped) != unmapped {
		t.Fatalf("%s: SettleResult is charged=%d credits=%d already=%d free=%d notfinal=%d unmapped=%d %v by-unit=%v, want charged=%d credits=%d already=%d free=%d notfinal=%d unmapped=%d",
			what, got.Charged, got.Credits, got.AlreadySettled, got.Free, got.NotFinal, len(got.Unmapped), got.Unmapped, got.ByUnit, charged, credits, already, free, notFinal, unmapped)
	}
}

// rawCharge inserts a charge row AS platform_app, bypassing the store: what any other writer of the ledger could do. The database alone must hold it.
func (e *mnyEnv) rawCharge(user, module, job string, reserveID int64, unit string, cost int, item, key string) error {
	e.t.Helper()
	_, err := e.pool.Exec(testCtx(e.t), `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, unit, units, unit_cost, item_ref, idempotency_key)
		VALUES ($1, $2, 'charge', 0, $3, $4, $5, $6, 1, $7, $8, $9)`, user, module, -int64(cost), job, reserveID, unit, cost, item, key)
	return err
}

// ---------------------------------------------------------------------------------------------------------------------
// grant
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneyStore_Grant: a grant writes one ledger row and the balance follows; the same request again returns the first row and writes nothing; a DIFFERENT request under the same
// key (another amount, admin, reason, or another kind) is refused as an idempotency mismatch and writes nothing. Controls: the same request under a new key IS a second grant, and
// the key is scoped to (user, module), so another module or another user may use it.
// It fails if a replay writes a second row, if a mismatch is taken for a replay, or if a key leaks across users or modules.
func TestMoneyStore_Grant(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	req := money.GrantReq{UserID: mnyUserA, Module: "m1", Amount: 100, AdminID: mnyAdminA, Reason: "  welcome credits \n", Key: "grant-1"}

	first, err := e.store.Grant(ctx, req)
	mnyNoErr(t, "the first grant", err)
	if first.ID < 1 || first.Replayed || first.Kind != "grant" || first.Amount != 100 || first.UserID != mnyUserA || first.Module != "m1" ||
		first.AdminID != mnyAdminA || first.Reason != "welcome credits" || first.Key != "grant-1" || first.CreatedAt.IsZero() {
		t.Fatalf("the first grant returned %+v (want a new row of kind grant, 100 credits, the reason trimmed)", first)
	}
	if got := e.rows("SELECT kind, available_delta, reserved_delta, acting_admin_id::text, reason, idempotency_key FROM platform.ledger WHERE id = $1", first.ID); len(got) != 1 || got[0] != "grant|100|0|"+mnyAdminA+"|welcome credits|grant-1" {
		t.Fatalf("the ledger row of the grant is %v", got)
	}
	e.requireBal("after the grant", mnyUserA, "m1", 100, 0)
	if _, _, ok := e.bal(mnyUserA, "m2"); ok {
		t.Fatal("a grant on m1 created a balance row on m2: credits never move between modules")
	}
	if b, err := e.store.Balance(ctx, mnyUserA, "m1"); err != nil || b != (money.Balance{Available: 100, Reserved: 0, Exists: true}) {
		t.Fatalf("Store.Balance after the grant = %+v, %v, want 100/0 and Exists", b, err)
	}
	e.requireViewsEmpty("after the grant")

	// a replay: the same request returns the first row and writes nothing (the reason is compared trimmed, so the same words with other white space are the same request)
	before := e.state()
	for _, replay := range []money.GrantReq{req, {UserID: req.UserID, Module: req.Module, Amount: req.Amount, AdminID: req.AdminID, Reason: "welcome credits", Key: req.Key}} {
		again, err := e.store.Grant(ctx, replay)
		mnyNoErr(t, "a replayed grant", err)
		if !again.Replayed || again.ID != first.ID || again.Amount != 100 || !again.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("a replayed grant returned %+v, want the first row (id %d) with Replayed", again, first.ID)
		}
		e.requireSame("a replayed grant", before)
	}

	// a different request under the same key: the first stands, nothing is written
	for _, c := range []struct {
		name string
		mut  func(*money.GrantReq)
	}{
		{"another amount", func(r *money.GrantReq) { r.Amount = 101 }},
		{"another admin", func(r *money.GrantReq) { r.AdminID = mnyAdminB }},
		{"another reason", func(r *money.GrantReq) { r.Reason = "a different reason" }},
	} {
		bad := req
		c.mut(&bad)
		_, err := e.store.Grant(ctx, bad)
		mnyIs(t, "the same key with "+c.name, err, money.ErrIdempotencyMismatch)
		e.requireSame("the same key with "+c.name, before)
	}
	// the same key used for ANOTHER KIND: an adjustment cannot be mistaken for the grant that holds the key
	_, err = e.store.Adjust(ctx, money.AdjustReq(req))
	mnyIs(t, "an adjustment under the key of a grant", err, money.ErrIdempotencyMismatch)
	e.requireSame("an adjustment under the key of a grant", before)

	// the controls: a new key is a second grant; the key is per (user, module)
	second := req
	second.Key = "grant-2"
	got, err := e.store.Grant(ctx, second)
	mnyNoErr(t, "the same request under a new key", err)
	if got.Replayed || got.ID == first.ID {
		t.Fatalf("the same request under a new key returned %+v: it must be a second grant", got)
	}
	e.requireBal("after the second grant", mnyUserA, "m1", 200, 0)
	otherModule := req
	otherModule.Module = "m2"
	if got, err := e.store.Grant(ctx, otherModule); err != nil || got.Replayed {
		t.Fatalf("the key of a grant on m1 refused or replayed on m2: %+v, %v", got, err)
	}
	otherUser := req
	otherUser.UserID = mnyUserB
	if got, err := e.store.Grant(ctx, otherUser); err != nil || got.Replayed {
		t.Fatalf("the key of a grant to user A refused or replayed for user B: %+v, %v", got, err)
	}
	e.requireBal("module m2 of user A", mnyUserA, "m2", 100, 0)
	e.requireBal("user B", mnyUserB, "m1", 100, 0)
	e.requireViewsEmpty("after the grants")
}

// TestMoneyStore_GrantAndAdjustRefuseBadInputsBeforeTheDatabase: a malformed request is refused with an InvalidError naming the FIELD, by a store whose pool points at a database that
// does not exist (so it cannot have been asked), and by the real store with ZERO rows written. The paired control is the same dead store given a LEGAL request: it fails on the
// connection, which proves the dead pool really is dead and the refusals above were decided before it. Module 3 is ErrNotMetered. The edge of every limit is accepted.
// It fails if a bad input reaches the database, is refused under another name, or if a legal edge is refused.
func TestMoneyStore_GrantAndAdjustRefuseBadInputsBeforeTheDatabase(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	dead := money.New(mnyDeadPool(t))
	good := money.GrantReq{UserID: mnyUserA, Module: "m1", Amount: 10, AdminID: mnyAdminA, Reason: "a reason", Key: "bad-inputs"}

	if _, err := dead.Grant(ctx, good); err == nil || errors.Is(err, money.ErrInvalid) {
		t.Fatalf("control: a LEGAL grant on the dead store returned %v, want a connection error (not nil, not a validation error): the dead pool is not dead, so the refusals below prove nothing", err)
	}
	if _, err := dead.Adjust(ctx, money.AdjustReq(good)); err == nil || errors.Is(err, money.ErrInvalid) {
		t.Fatalf("control: a LEGAL adjustment on the dead store returned %v, want a connection error", err)
	}

	cases := []struct {
		name  string
		mut   func(*money.GrantReq)
		field string
	}{
		{"a blank user", func(r *money.GrantReq) { r.UserID = "" }, "user_id"},
		{"an upper-case user id", func(r *money.GrantReq) { r.UserID = strings.ToUpper(mnyAdminA) }, "user_id"},
		{"a user id that is not a uuid", func(r *money.GrantReq) { r.UserID = "not-a-uuid" }, "user_id"},
		{"the system user", func(r *money.GrantReq) { r.UserID = money.SystemUser }, "user_id"},
		{"an unknown module", func(r *money.GrantReq) { r.Module = "m9" }, "module"},
		{"a blank module", func(r *money.GrantReq) { r.Module = "" }, "module"},
		{"zero credits", func(r *money.GrantReq) { r.Amount = 0 }, "amount"},
		{"negative credits", func(r *money.GrantReq) { r.Amount = -5 }, "amount"},
		{"more than 2^53 credits", func(r *money.GrantReq) { r.Amount = money.MaxAmount + 1 }, "amount"},
		{"a blank admin", func(r *money.GrantReq) { r.AdminID = "" }, "admin_id"},
		{"a blank reason", func(r *money.GrantReq) { r.Reason = "  \t " }, "reason"},
		{"a reason of 1001 bytes", func(r *money.GrantReq) { r.Reason = strings.Repeat("r", 1001) }, "reason"},
		{"a reason that is not UTF-8", func(r *money.GrantReq) { r.Reason = "bad \xff byte" }, "reason"},
		{"a blank key", func(r *money.GrantReq) { r.Key = "   " }, "key"},
		{"a key of 201 bytes", func(r *money.GrantReq) { r.Key = strings.Repeat("k", 201) }, "key"},
		{"a key with a NUL", func(r *money.GrantReq) { r.Key = "a\x00b" }, "key"},
		{"a key in the shape of a reserve key", func(r *money.GrantReq) { r.Key = "reserve:" + mnyNobody }, "key"},
		{"a key in the shape of a charge key", func(r *money.GrantReq) { r.Key = "charge:" + mnyNobody + ":x" }, "key"},
	}
	for _, c := range cases {
		bad := good
		c.mut(&bad)
		for _, s := range []struct {
			name  string
			store *money.Store
		}{{"dead pool", dead}, {"real database", e.store}} {
			_, err := s.store.Grant(ctx, bad)
			mnyInvalid(t, fmt.Sprintf("Grant with %s (%s)", c.name, s.name), err, c.field)
			_, err = s.store.Adjust(ctx, money.AdjustReq(bad))
			mnyInvalid(t, fmt.Sprintf("Adjust with %s (%s)", c.name, s.name), err, c.field)
		}
	}
	m3 := good
	m3.Module = "m3"
	for _, s := range []*money.Store{dead, e.store} {
		_, err := s.Grant(ctx, m3)
		mnyIs(t, "a grant on module 3", err, money.ErrNotMetered)
		_, err = s.Adjust(ctx, money.AdjustReq(m3))
		mnyIs(t, "an adjustment on module 3", err, money.ErrNotMetered)
	}
	for _, table := range []string{"platform.ledger", "platform.balances", "platform.admin_audit"} {
		if n := e.int("SELECT count(*) FROM " + table); n != 0 {
			t.Fatalf("%d row(s) in %s after %d refused requests: a refusal must write nothing", n, table, 2*len(cases)+2)
		}
	}

	// the edge of every limit is legal (the control of the refusals above): 1 credit, 2^53 credits, a reason of 1000 bytes, a key of 200 bytes
	for _, c := range []struct {
		name string
		mut  func(*money.GrantReq)
	}{
		{"one credit", func(r *money.GrantReq) { r.Amount = 1 }},
		{"2^53 credits", func(r *money.GrantReq) { r.Amount = money.MaxAmount }},
		{"a reason of exactly 1000 bytes", func(r *money.GrantReq) { r.Reason = strings.Repeat("r", 1000) }},
		{"a key of exactly 200 bytes", func(r *money.GrantReq) { r.Key = strings.Repeat("k", 200) }},
	} {
		edge := good
		edge.Key = "edge-" + c.name
		c.mut(&edge)
		if _, err := e.store.Grant(ctx, edge); err != nil {
			t.Fatalf("the legal edge %q was refused: %v", c.name, err)
		}
	}
	if n := e.int("SELECT count(*) FROM platform.ledger"); n != 4 {
		t.Fatalf("%d ledger row(s) after the four legal edges, want 4", n)
	}
	e.requireViewsEmpty("after the edges")
}

// TestMoneyStore_GrantAndAdjustRefuseWhatTheDatabaseRefuses: the database's own refusals reach the caller by NAME. A disabled admin is ErrAdminDisabled (constraint admin_is_active) and
// writes nothing; an admin who is not in platform.admins is ErrAdminUnknown; a user who is not in public.users is ErrUserNotFound. Controls: the active admin passes, and the SAME request
// passes once the admin is enabled again (the refusal left nothing behind).
// It fails if a disabled admin can grant or adjust, or if the refusals come back as anonymous errors.
func TestMoneyStore_GrantAndAdjustRefuseWhatTheDatabaseRefuses(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	ok := money.GrantReq{UserID: mnyUserA, Module: "m1", Amount: 50, AdminID: mnyAdminA, Reason: "r", Key: "dbref-ok"}
	if _, err := e.store.Grant(ctx, ok); err != nil {
		t.Fatalf("control: the active admin's grant was refused: %v", err)
	}

	e.exec("UPDATE platform.admins SET disabled_at = now() WHERE id = $1", mnyAdminB)
	disabled := ok
	disabled.AdminID, disabled.Key = mnyAdminB, "dbref-disabled"
	before := e.state()
	_, err := e.store.Grant(ctx, disabled)
	mnyIs(t, "a grant by a disabled admin", err, money.ErrAdminDisabled)
	e.requireSame("a grant by a disabled admin", before)
	disabled.Amount = 5
	_, err = e.store.Adjust(ctx, money.AdjustReq(disabled))
	mnyIs(t, "an adjustment by a disabled admin", err, money.ErrAdminDisabled)
	e.requireSame("an adjustment by a disabled admin", before)
	e.requireBal("the user's credits after the refusals", mnyUserA, "m1", 50, 0)

	e.exec("UPDATE platform.admins SET disabled_at = NULL WHERE id = $1", mnyAdminB)
	if _, err := e.store.Grant(ctx, disabled); err != nil {
		t.Fatalf("control: the same grant by the admin once enabled again was refused: %v", err)
	}

	unknownAdmin := ok
	unknownAdmin.AdminID, unknownAdmin.Key, unknownAdmin.Amount = mnyNobody, "dbref-unknown-admin", 5
	before = e.state()
	_, err = e.store.Grant(ctx, unknownAdmin)
	mnyIs(t, "a grant by an admin who does not exist", err, money.ErrAdminUnknown)
	_, err = e.store.Adjust(ctx, money.AdjustReq(unknownAdmin))
	mnyIs(t, "an adjustment by an admin who does not exist", err, money.ErrAdminUnknown)
	e.requireSame("an unknown admin", before)

	unknownUser := ok
	unknownUser.UserID, unknownUser.Key = mnyNobody, "dbref-unknown-user"
	_, err = e.store.Grant(ctx, unknownUser)
	mnyIs(t, "a grant to a user who does not exist", err, money.ErrUserNotFound)
	e.requireSame("an unknown user", before)
	e.requireViewsEmpty("after the refusals")
}

// ---------------------------------------------------------------------------------------------------------------------
// adjustment
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneyStore_Adjust: an adjustment (a clawback) removes only AVAILABLE credits. With 10 available and 5 reserved, 11 is refused as ErrInsufficientCredits{10, 11, shortfall 1} and
// NOTHING changes; 10 goes through, the reserved 5 stay, and the database writes the admin_audit row. The replay returns the first row even though its credits are gone; a different
// amount under the key is a mismatch. The next removal is refused with available 0 although 5 are reserved (reserved credits are never touched).
// It fails if an adjustment reaches reserved credits, if the shortfall is wrong, if a refusal writes anything, or if the audit row is missing.
func TestMoneyStore_Adjust(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 15)
	job := e.newJob(mnyUserA, "m2")
	e.reserve(job, 5) // the m2 units are priced 1 and 1: 5 credits are held
	e.requireBal("the setup", mnyUserA, "m2", 10, 5)

	req := money.AdjustReq{UserID: mnyUserA, Module: "m2", Amount: 11, AdminID: mnyAdminA, Reason: "chargeback of order 7", Key: "adj-1"}
	before := e.state()
	_, err := e.store.Adjust(ctx, req)
	mnyInsufficient(t, "an adjustment of 11 against 10 available", err, 10, 11, 1)
	e.requireSame("the refused adjustment", before)
	e.requireBal("after the refused adjustment", mnyUserA, "m2", 10, 5)

	req.Amount = 10 // the control: exactly the available credits
	got, err := e.store.Adjust(ctx, req)
	mnyNoErr(t, "an adjustment of exactly the available credits", err)
	if got.ID < 1 || got.Replayed || got.Kind != "adjustment" || got.Amount != 10 || got.AdminID != mnyAdminA || got.Reason != req.Reason || got.Key != "adj-1" {
		t.Fatalf("the adjustment returned %+v", got)
	}
	if rows := e.rows("SELECT kind, available_delta, reserved_delta, acting_admin_id::text, reason, job_id IS NULL, reserve_id IS NULL FROM platform.ledger WHERE id = $1", got.ID); len(rows) != 1 || rows[0] != "adjustment|-10|0|"+mnyAdminA+"|chargeback of order 7|true|true" {
		t.Fatalf("the ledger row of the adjustment is %v", rows)
	}
	e.requireBal("after the adjustment: the 5 reserved credits stay", mnyUserA, "m2", 0, 5)
	wantAudit := fmt.Sprintf("%s|action|ledger.adjustment|ledger|%d|%s|chargeback of order 7|10|m2", mnyAdminA, got.ID, mnyUserA)
	if rows := e.rows(`SELECT admin_id::text, entry_type, action, target_type, target_id, target_user_id::text, reason, details->>'credits_removed', details->>'module_id' FROM platform.admin_audit`); len(rows) != 1 || rows[0] != wantAudit {
		t.Fatalf("the admin_audit rows are %v, want exactly [%s]", rows, wantAudit)
	}
	e.requireViewsEmpty("after the adjustment")

	// the replay answers although the credits it removed are gone (a funds check first would say "insufficient" here)
	before = e.state()
	again, err := e.store.Adjust(ctx, req)
	mnyNoErr(t, "a replayed adjustment", err)
	if !again.Replayed || again.ID != got.ID || again.Amount != 10 {
		t.Fatalf("a replayed adjustment returned %+v, want the first row with Replayed", again)
	}
	e.requireSame("a replayed adjustment", before)
	nine := req
	nine.Amount = 9
	_, err = e.store.Adjust(ctx, nine)
	mnyIs(t, "the same key with another amount", err, money.ErrIdempotencyMismatch)
	e.requireSame("the same key with another amount", before)

	// nothing is left to remove: available is 0 and the 5 reserved credits are not available to a clawback
	more := req
	more.Amount, more.Key = 1, "adj-2"
	_, err = e.store.Adjust(ctx, more)
	mnyInsufficient(t, "a second adjustment with 0 available and 5 reserved", err, 0, 1, 1)
	e.requireSame("the second adjustment", before)
	e.requireBal("reserved credits are never touched", mnyUserA, "m2", 0, 5)

	// the other refusals, each with nothing written
	e.fund(mnyUserA, "m2", 3)
	e.exec("UPDATE platform.admins SET disabled_at = now() WHERE id = $1", mnyAdminB)
	before = e.state()
	byDisabled := more
	byDisabled.AdminID, byDisabled.Key = mnyAdminB, "adj-3"
	_, err = e.store.Adjust(ctx, byDisabled)
	mnyIs(t, "an adjustment by a disabled admin", err, money.ErrAdminDisabled)
	m3 := more
	m3.Module = "m3"
	_, err = e.store.Adjust(ctx, m3)
	mnyIs(t, "an adjustment on module 3", err, money.ErrNotMetered)
	zero := more
	zero.Amount = 0
	_, err = e.store.Adjust(ctx, zero)
	mnyInvalid(t, "an adjustment of zero", err, "amount")
	blank := more
	blank.Reason = " "
	_, err = e.store.Adjust(ctx, blank)
	mnyInvalid(t, "an adjustment with a blank reason", err, "reason")
	e.requireSame("the refused adjustments", before)
	e.requireViewsEmpty("at the end")
}

// ---------------------------------------------------------------------------------------------------------------------
// reserve
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneyStore_Reserve: the happy path of module 1 (the snapshot rows are the module's units at the table's prices, the rate is the DEAREST, available and reserved move together, the
// job's status is untouched) and of module 2 (a tie goes to the lexicographically first unit); a replay returns the same reserve and writes nothing, even after the job left draft; the
// same job with other units is a mismatch.
// It fails if the snapshot is partial, the rate is not the dearest, a replay writes, or the store changes the job's status.
func TestMoneyStore_Reserve(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 100)
	job := e.newJob(mnyUserA, "m1")

	res, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 10})
	mnyNoErr(t, "the reserve", err)
	if res.ReserveID < 1 || res.Replayed || res.Unit != "m1_vendor" || res.UnitCost != 2 || res.Units != 10 || res.Reserved != 20 {
		t.Fatalf("the reserve returned %+v, want unit m1_vendor (the dearest) at 2 credits x 10 units = 20", res)
	}
	mnyPricesEqual(t, "the returned snapshot", res.Prices, map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2})
	table := e.rows("SELECT unit, cost FROM platform.unit_costs WHERE module_id = 'm1' ORDER BY unit")
	snapshot := e.rows("SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", job)
	if len(table) != 3 || strings.Join(table, ",") != strings.Join(snapshot, ",") {
		t.Fatalf("the job's snapshot %v is not the module's unit_costs %v (module 1 has three units)", snapshot, table)
	}
	if got := e.rows("SELECT kind, available_delta, reserved_delta, unit, units, unit_cost, idempotency_key, reserve_id IS NULL FROM platform.ledger WHERE id = $1", res.ReserveID); len(got) != 1 || got[0] != "reserve|-20|20|m1_vendor|10|2|reserve:"+job+"|true" {
		t.Fatalf("the ledger row of the reserve is %v", got)
	}
	if got := e.rows("SELECT reserved, charged, released, job_id::text FROM platform.reserves WHERE reserve_id = $1", res.ReserveID); len(got) != 1 || got[0] != "20|0|0|"+job {
		t.Fatalf("the reserves row is %v, want 20 reserved, nothing charged or released", got)
	}
	e.requireBal("after the reserve", mnyUserA, "m1", 80, 20)
	if s := e.jobStatus(job); s != "draft" {
		t.Fatalf("the job's status is %q after the reserve: the store must not change it (the job engine does, in its own transaction)", s)
	}
	e.requireViewsEmpty("after the reserve")
	e.requireReservesMatchLedger("after the reserve")

	// a replay: the same reserve, nothing written, also after the engine moved the job out of draft
	before := e.state()
	again, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 10})
	mnyNoErr(t, "a replayed reserve", err)
	if !again.Replayed || again.ReserveID != res.ReserveID || again.Unit != res.Unit || again.UnitCost != 2 || again.Reserved != 20 || again.Units != 10 {
		t.Fatalf("a replayed reserve returned %+v, want the first reserve %+v with Replayed", again, res)
	}
	mnyPricesEqual(t, "the replayed snapshot", again.Prices, res.Prices)
	e.requireSame("a replayed reserve", before)
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 11})
	mnyIs(t, "the same job with other units", err, money.ErrIdempotencyMismatch)
	e.requireSame("the same job with other units", before)
	e.setStatus(job, "running")
	again, err = e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 10})
	if err != nil || !again.Replayed || again.ReserveID != res.ReserveID {
		t.Fatalf("a replay after the job left draft returned %+v, %v, want the first reserve with Replayed", again, err)
	}
	e.requireSame("a replay after the job left draft", before)

	// module 2: both units cost 1, a tie, and the lexicographically first unit is the rate's unit; its balance is its own
	e.fund(mnyUserA, "m2", 50)
	job2 := e.newJob(mnyUserA, "m2")
	res2 := e.reserve(job2, 7)
	if res2.Unit != "m2_accepted" || res2.UnitCost != 1 || res2.Reserved != 7 || res2.Replayed {
		t.Fatalf("the module 2 reserve returned %+v, want the tie to go to m2_accepted at 1 credit x 7", res2)
	}
	mnyPricesEqual(t, "the module 2 snapshot", res2.Prices, map[string]int{"m2_accepted": 1, "m2_bounced": 1})
	e.requireBal("module 2 after its reserve", mnyUserA, "m2", 43, 7)
	e.requireBal("module 1 is not touched by a module 2 reserve", mnyUserA, "m1", 80, 20)
	e.requireViewsEmpty("at the end")
	if n := e.requireReservesMatchLedger("at the end"); n != 2 {
		t.Fatalf("%d reserves compared, want 2", n)
	}
}

// TestMoneyStore_ReserveInsufficientCredits: 19 available against a need of 20 is ErrInsufficientCredits{19, 20, shortfall 1} and NOTHING is left behind: no snapshot row, no ledger row, no
// balance change; a user with no balance row at all is refused with available 0. The control: after the one missing credit arrives the SAME job reserves (so the refusal left nothing
// that blocks it), at exactly the balance.
// It fails if a refused reserve leaves its price snapshot behind, or if the shortfall is wrong.
func TestMoneyStore_ReserveInsufficientCredits(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 19)
	job := e.newJob(mnyUserA, "m1")
	before := e.state()

	_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 10}) // 10 units x the dearest price 2 = 20
	mnyInsufficient(t, "10 units at 2 credits against 19 available", err, 19, 20, 1)
	e.requireSame("the refused reserve", before)
	if n := e.int("SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", job); n != 0 {
		t.Fatalf("%d snapshot row(s) were left behind by a refused reserve", n)
	}
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1", job); n != 0 {
		t.Fatalf("%d ledger row(s) for the job after a refused reserve", n)
	}
	e.requireBal("after the refused reserve", mnyUserA, "m1", 19, 0)

	// a user who never had a balance row: available is 0
	jobB := e.newJob(mnyUserB, "m1")
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: jobB, Units: 3})
	mnyInsufficient(t, "a user with no balance row", err, 0, 6, 6)
	if _, _, ok := e.bal(mnyUserB, "m1"); ok {
		t.Fatal("a refused reserve created a balance row")
	}

	// the control: one more credit and the very same job reserves, at exactly the balance
	e.fund(mnyUserA, "m1", 1)
	res, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 10})
	mnyNoErr(t, "the reserve after the missing credit arrived", err)
	if res.Reserved != 20 {
		t.Fatalf("the reserve held %d credits, want 20", res.Reserved)
	}
	e.requireBal("after the successful reserve", mnyUserA, "m1", 0, 20)
	e.requireViewsEmpty("at the end")
}

// TestMoneyStore_ReserveRefusals: an unknown job is ErrJobNotFound, a job that is not a draft is ErrJobState (every other status, each with nothing written), a module 3 job is
// ErrNotMetered, bad units and a bad job id are InvalidErrors. The control is the same job as a draft, which reserves.
// It fails if a job that has left draft can reserve, or if a refusal writes anything.
func TestMoneyStore_ReserveRefusals(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 100)
	job := e.newJob(mnyUserA, "m1")
	before := e.state()

	_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: mnyNobody, Units: 1})
	mnyIs(t, "a job that does not exist", err, money.ErrJobNotFound)
	// A terminal job is final in the database since platform_0007: the four statuses a job passes through are tried on `job` itself (it moves on, and the control below moves it back to draft), and each
	// of the three terminal statuses on a job of its own that is made terminal once and never moved again.
	for _, status := range []string{"scheduled", "running", "paused", "cancelling", "succeeded", "failed", "cancelled"} {
		subject := job
		if status == "succeeded" || status == "failed" || status == "cancelled" {
			subject = e.newJob(mnyUserA, "m1")
		}
		e.setStatus(subject, status)
		before = e.state()
		_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: subject, Units: 1})
		mnyIs(t, "a reserve for a "+status+" job", err, money.ErrJobState)
		if !strings.Contains(err.Error(), status) {
			t.Fatalf("the error for a %s job does not name the status: %v", status, err)
		}
		e.requireSame("a reserve for a "+status+" job", before)
	}

	m3 := e.newJob(mnyUserA, "m3")
	before = e.state()
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: m3, Units: 1})
	mnyIs(t, "a module 3 job", err, money.ErrNotMetered)
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 0})
	mnyInvalid(t, "zero units", err, "units")
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: -3})
	mnyInvalid(t, "negative units", err, "units")
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 1 << 31})
	mnyInvalid(t, "more units than the ledger column holds", err, "units")
	_, err = e.store.Reserve(ctx, money.ReserveReq{JobID: strings.ToUpper(job), Units: 1})
	mnyInvalid(t, "an upper-case job id", err, "job_id")
	e.requireSame("the refused reserves", before)

	e.setStatus(job, "draft")
	if res, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 1}); err != nil || res.Reserved != 2 {
		t.Fatalf("control: the same job as a draft returned %+v, %v, want a reserve of 2", res, err)
	}
	e.requireViewsEmpty("at the end")
}

// TestMoneyStore_SnapshotEqualsTheTableAtThatInstant: the reserve's FOR SHARE on the module's unit_costs holds a concurrent price change until the reserve transaction ends. The test holds
// the reserve transaction OPEN (ReserveIn in a transaction it owns), starts a price change on another connection and PROVES it is blocked behind the reserve's backend (pg_stat_activity
// wait_event_type = Lock and pg_blocking_pids names the reserve's pid), reads the snapshot inside the open transaction and the committed table outside it and sees them EQUAL, commits, lets
// the change finish, and sees the snapshot at the OLD prices and the table at the new one.
// The paired control runs the same statements WITHOUT the share lock in raw SQL: the price change is not blocked, completes while the transaction is open, and the snapshot is stale by commit.
// It fails if the FOR SHARE is removed from Reserve (the price change would not be blocked), or if the snapshot is not a copy of the rows that were locked.
func TestMoneyStore_SnapshotEqualsTheTableAtThatInstant(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 1000)
	job := e.newJob(mnyUserA, "m1")

	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mnyNoErr(t, "begin the reserve transaction", err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var reserverPID int32
	mnyNoErr(t, "the reserve transaction's pid", tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&reserverPID))
	res, err := e.store.ReserveIn(ctx, tx, money.ReserveReq{JobID: job, Units: 4})
	mnyNoErr(t, "ReserveIn in the caller's transaction", err)

	change := e.startPriceChange("m1_vendor", 5, "test: the vendor became dearer while a reserve was open")
	ws, blocked := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		for _, w := range ws {
			if strings.HasPrefix(w.Query, "UPDATE platform.unit_costs") && mnyHasPID(w.Blockers, reserverPID) {
				return true
			}
		}
		return false
	})
	if !blocked {
		t.Fatalf("the price change was NOT blocked behind the open reserve transaction (pid %d): %s. The reserve holds no share lock on unit_costs, so the snapshot can differ from the table", reserverPID, mnyDescribe(ws))
	}
	select {
	case err := <-change:
		t.Fatalf("the price change finished (%v) while the reserve transaction was still open: it was not really blocked", err)
	default:
	}

	// at that instant the committed table still has the OLD prices, and the snapshot (read inside the open transaction) EQUALS it
	inTx := mnyRows(t, tx, "SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", job)
	committed := e.rows("SELECT unit, cost FROM platform.unit_costs WHERE module_id = 'm1' ORDER BY unit")
	if len(inTx) != 3 || strings.Join(inTx, ",") != strings.Join(committed, ",") {
		t.Fatalf("while the price change waits, the snapshot %v is not equal to the table %v", inTx, committed)
	}
	mnyNoErr(t, "commit the reserve transaction", tx.Commit(ctx))
	select {
	case err := <-change:
		mnyNoErr(t, "the price change once the reserve committed", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the price change did not finish after the reserve transaction committed")
	}

	if res.UnitCost != 2 || res.Reserved != 8 {
		t.Fatalf("the reserve was priced %+v, want the OLD vendor price 2 x 4 units = 8", res)
	}
	mnyPricesEqual(t, "the snapshot after the price change", e.frozen(job), map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2})
	if got := e.int("SELECT cost FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != 5 {
		t.Fatalf("the table's vendor price is %d after the change, want 5", got)
	}
	if got := e.rows("SELECT old_cost, new_cost FROM platform.unit_cost_history WHERE unit = 'm1_vendor' ORDER BY id"); strings.Join(got, ",") != "<nil>|2,2|5" {
		t.Fatalf("the price history of m1_vendor is %v, want the seed and the change 2 -> 5", got)
	}

	// THE PAIRED CONTROL: the same statements without the share lock. The price change is not blocked, and the snapshot is stale before the transaction ends.
	job2 := e.newJob(mnyUserA, "m1")
	raw, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mnyNoErr(t, "begin the control transaction", err)
	t.Cleanup(func() { _ = raw.Rollback(context.Background()) })
	_, err = raw.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", money.LockKey(mnyUserA, "m1"))
	mnyNoErr(t, "the control's advisory lock", err)
	var status string
	mnyNoErr(t, "the control's job lock", raw.QueryRow(ctx, "SELECT status FROM platform.jobs WHERE id = $1 FOR UPDATE", job2).Scan(&status))
	// (no `SELECT ... FROM platform.unit_costs ... FOR SHARE` here: that is the one statement the store has and this copy has not)
	_, err = raw.Exec(ctx, `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.module_id = 'm1'`, job2)
	mnyNoErr(t, "the control's snapshot copy", err)
	free := e.startPriceChange("m1_vendor", 7, "test: the control's price change")
	select {
	case err := <-free:
		mnyNoErr(t, "the control's price change", err)
	case <-time.After(20 * time.Second):
		t.Fatalf("CONTROL FAILED: the price change was blocked although the control transaction took no share lock (%s): the interleave above proves nothing about FOR SHARE", mnyDescribe(e.waiting()))
	}
	stale := mnyRows(t, raw, "SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", job2)
	now := e.rows("SELECT unit, cost FROM platform.unit_costs WHERE module_id = 'm1' ORDER BY unit")
	if strings.Join(stale, ",") == strings.Join(now, ",") {
		t.Fatalf("CONTROL FAILED: without the share lock the snapshot %v still equals the table: the price change did not move the table under it", stale)
	}
	if !strings.Contains(strings.Join(stale, ","), "m1_vendor|5") || !strings.Contains(strings.Join(now, ","), "m1_vendor|7") {
		t.Fatalf("CONTROL: snapshot %v and table %v, want the stale vendor price 5 against the table's 7", stale, now)
	}
}

// TestMoneyStore_SettlesAtItsFrozenCost: a job reserved while the vendor price was 2 settles a vendor outcome at 2 although the table says 5 by then (the charge row carries 2). The control: a
// job reserved AFTER the change has 5 in its snapshot and is charged 5, so the change was real and the first job simply never reads the table.
// It fails if settlement reads platform.unit_costs instead of the job's frozen snapshot.
func TestMoneyStore_SettlesAtItsFrozenCost(t *testing.T) {
	e := newMnyEnv(t)
	e.fund(mnyUserA, "m1", 100)
	job := e.newJob(mnyUserA, "m1")
	res := e.reserve(job, 5)
	if res.UnitCost != 2 || res.Reserved != 10 {
		t.Fatalf("the first reserve is %+v, want the vendor price 2 x 5 units = 10", res)
	}
	e.setStatus(job, "running")

	if err := <-e.startPriceChange("m1_vendor", 5, "test: the vendor became dearer after the reserve"); err != nil {
		t.Fatalf("the price change: %v", err)
	}
	if got := e.int("SELECT cost FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != 5 {
		t.Fatalf("the table's vendor price is %d, want 5", got)
	}

	sr := e.settle(job, mnyItem("v-1", mnyM1Vendor), mnyItem("o-1", mnyM1Accepted))
	mnyRequireSettle(t, "settle a vendor outcome and an own outcome", sr, 2, 3, 0, 0, 0, 0)
	if sr.ByUnit["m1_vendor"] != 1 || sr.ByUnit["m1_own"] != 1 || len(sr.ByUnit) != 2 {
		t.Fatalf("ByUnit = %v, want one m1_vendor and one m1_own", sr.ByUnit)
	}
	if got := e.rows("SELECT unit, unit_cost, reserved_delta FROM platform.ledger WHERE job_id = $1 AND item_ref = 'v-1'", job); len(got) != 1 || got[0] != "m1_vendor|2|-2" {
		t.Fatalf("the vendor charge row is %v, want the FROZEN price 2 (the table says 5)", got)
	}
	e.requireBal("after the first job's settlement", mnyUserA, "m1", 90, 7)

	// the control: a job reserved after the change carries the new price
	e.setStatus(job, "succeeded")
	job2 := e.newJob(mnyUserA, "m1")
	res2 := e.reserve(job2, 3)
	if res2.UnitCost != 5 || res2.Reserved != 15 || res2.Prices["m1_vendor"] != 5 {
		t.Fatalf("the second reserve is %+v, want the new vendor price 5 x 3 units = 15 in its snapshot", res2)
	}
	e.setStatus(job2, "running")
	sr2 := e.settle(job2, mnyItem("v-1", mnyM1Vendor))
	mnyRequireSettle(t, "settle a vendor outcome of the job reserved after the change", sr2, 1, 5, 0, 0, 0, 0)
	e.requireViewsEmpty("at the end")
}

// ---------------------------------------------------------------------------------------------------------------------
// settle
// ---------------------------------------------------------------------------------------------------------------------

var mnySeededPrices = map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2, "m2_accepted": 1, "m2_bounced": 1}

// mnyConcreteOutcome builds an Outcome that the table row r decides: a wildcard cell takes a value no earlier row names.
func mnyConcreteOutcome(r charge.Rule) charge.Outcome {
	o := charge.Outcome{Module: r.Module, State: r.State, Reason: r.Reason, FromCache: r.Cache == "yes"}
	if o.State == "*" {
		o.State = "x-state"
	}
	if o.Reason == "*" {
		o.Reason = ""
		if r.Module == "m1" {
			o.Reason = "x-reason"
		}
	}
	switch r.Skip {
	case "capacity":
		o.SkipReason = "no_eligible_node"
	case "*", "none":
		o.SkipReason = ""
	default:
		o.SkipReason = r.Skip
	}
	return o
}

// TestMoneyStore_SettleEveryChargeRule: ONE settlement through the real store for EVERY row of the charge table (charge.Rules(), embedded from internal/charge/rules.psv): a CHARGE row
// writes exactly one ledger row of its unit at the job's frozen price and moves that many credits out of reserved (and says so in Credits and ByUnit); a FREE or NOT-FINAL row writes nothing
// and is counted; an UNMAPPED row writes nothing and is returned with its rule id. The outcome built for a row is first checked to be decided by THAT row, so a row the test cannot
// reach is a red test, not a skipped one.
// It fails if any row's money effect differs from its decision, or if a table row is not covered.
func TestMoneyStore_SettleEveryChargeRule(t *testing.T) {
	e := newMnyEnv(t)
	rules := charge.Rules()
	if len(rules) < 30 {
		t.Fatalf("the charge table has %d rows: the instrument matched (almost) nothing, the table has 37 at the time of writing", len(rules))
	}
	e.fund(mnyUserA, "m1", 100000)
	e.fund(mnyUserA, "m2", 100000)
	ran := map[charge.Kind]int{}
	for _, r := range rules {
		r := r
		ran[r.Kind]++
		t.Run(r.ID, func(t *testing.T) {
			e := e.at(t)
			ctx := testCtx(t)
			o := mnyConcreteOutcome(r)
			if d := charge.Decide(o); d.Rule != r.ID || d.Kind != r.Kind || d.Unit != r.Unit {
				t.Fatalf("the outcome %+v built for row %s is decided by rule %s as %s (unit %q), want this row as %s (unit %q): the test cannot reach the row", o, r.ID, d.Rule, d.Kind, d.Unit, r.Kind, r.Unit)
			}
			module := "m1"
			if r.Module == "m2" {
				module = "m2"
			}
			job := e.newJob(mnyUserA, module)
			res := e.reserve(job, 2)
			e.setStatus(job, "succeeded") // a job that has left draft; terminal, so it never holds the one-active-job slot
			availBefore, reservedBefore, _ := e.bal(mnyUserA, module)
			ref := "item-" + r.ID

			sr, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem(ref, o)}})
			mnyNoErr(t, "Settle of row "+r.ID, err)
			availAfter, reservedAfter, _ := e.bal(mnyUserA, module)
			if availAfter != availBefore {
				t.Fatalf("settling moved the AVAILABLE credits %d -> %d: only reserved credits are spent", availBefore, availAfter)
			}
			switch r.Kind {
			case charge.Charge:
				price := res.Prices[r.Unit]
				if price < 1 || price != mnySeededPrices[r.Unit] {
					t.Fatalf("the frozen price of %s is %d, the seed is %d", r.Unit, price, mnySeededPrices[r.Unit])
				}
				mnyRequireSettle(t, r.ID, sr, 1, int64(price), 0, 0, 0, 0)
				if len(sr.ByUnit) != 1 || sr.ByUnit[r.Unit] != 1 {
					t.Fatalf("ByUnit = %v, want exactly one %s", sr.ByUnit, r.Unit)
				}
				want := fmt.Sprintf("%s|1|%d|%s|%d|0|%d|charge:%s:%s", r.Unit, price, ref, -price, res.ReserveID, job, ref)
				if got := e.rows("SELECT unit, units, unit_cost, item_ref, reserved_delta, available_delta, reserve_id, idempotency_key FROM platform.ledger WHERE job_id = $1 AND kind = 'charge'", job); len(got) != 1 || got[0] != want {
					t.Fatalf("the charge rows are %v, want exactly [%s]", got, want)
				}
				if reservedAfter != reservedBefore-int64(price) {
					t.Fatalf("reserved went %d -> %d, want a drop of the frozen price %d", reservedBefore, reservedAfter, price)
				}
			case charge.Free:
				mnyRequireSettle(t, r.ID, sr, 0, 0, 0, 1, 0, 0)
			case charge.NotFinal:
				mnyRequireSettle(t, r.ID, sr, 0, 0, 0, 0, 1, 0)
			case charge.Unmapped:
				mnyRequireSettle(t, r.ID, sr, 0, 0, 0, 0, 0, 1)
				if u := sr.Unmapped[0]; u.ItemRef != ref || u.Rule != r.ID || u.Note != r.Note {
					t.Fatalf("the unmapped item is %+v, want item %s, rule %s, the row's note", u, ref, r.ID)
				}
			default:
				t.Fatalf("row %s has the unknown kind %v", r.ID, r.Kind)
			}
			if r.Kind != charge.Charge {
				if n := e.chargeRows(job); n != 0 {
					t.Fatalf("a %s outcome wrote %d charge row(s): it must write none", r.Kind, n)
				}
				if reservedAfter != reservedBefore {
					t.Fatalf("a %s outcome moved reserved %d -> %d", r.Kind, reservedBefore, reservedAfter)
				}
			}
		})
	}
	for _, k := range []charge.Kind{charge.Charge, charge.Free, charge.NotFinal, charge.Unmapped} {
		if ran[k] == 0 {
			t.Errorf("the charge table has no %s row: the test covered no row of that kind", k)
		}
	}
	e.requireViewsEmpty("after every row")
	e.requireReservesMatchLedger("after every row")
}

// TestMoneyStore_SettleNotChargedClasses: the five classes the build plan never charges for. Cancelled, expired and quarantined by the platform; an infrastructure failure; pool exhaustion
// (module 2's `failed`, and the platform's own pool_exhausted); the user's own SMTP rejecting the login; and module 1's own-capacity failures (a skip token on risky/plausible). Each is
// FREE through the real store: nothing is written, the reserved credits stay reserved until the release, and the release returns them (all but the one control item that IS charged).
// It fails if a class is charged, or if its credits do not come back.
func TestMoneyStore_SettleNotChargedClasses(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 100)
	job := e.newJob(mnyUserA, "m2")
	e.reserve(job, 20)
	e.setStatus(job, "running")

	var items []money.SettleItem
	rules := map[string]string{}
	for i, c := range []struct{ state, rule string }{
		{"cancelled", "P-01"}, {"expired", "P-02"}, {"quarantined", "P-03"}, {"infrastructure_failure", "P-04"}, {"pool_exhausted", "P-05"}, {"smtp_login_rejected", "P-06"},
	} {
		o := charge.Outcome{Module: "platform", State: c.state}
		if d := charge.Decide(o); d.Kind != charge.Free || d.Rule != c.rule {
			t.Fatalf("the platform state %s is decided %s by rule %s, want free by %s", c.state, d.Kind, d.Rule, c.rule)
		}
		ref := fmt.Sprintf("platform-%d", i)
		items = append(items, mnyItem(ref, o))
		rules[ref] = c.rule
	}
	items = append(items, mnyItem("m2-failed", mnyM2Failed)) // pool exhaustion is the only writer of `failed`
	sr := e.settle(job, items...)
	mnyRequireSettle(t, "the not-charged classes of module 2 and the platform", sr, 0, 0, 0, 7, 0, 0)
	if n := e.chargeRows(job); n != 0 {
		t.Fatalf("%d charge row(s) were written for classes that are never charged", n)
	}
	e.requireBal("after settling the free classes", mnyUserA, "m2", 80, 20)

	// the control: in the same job a real delivery IS charged, so the zero above is not "this job cannot charge"
	ctl := e.settle(job, mnyItem("m2-sent", mnyM2Sent))
	mnyRequireSettle(t, "the control: a sent item", ctl, 1, 1, 0, 0, 0, 0)
	e.requireBal("after the control", mnyUserA, "m2", 80, 19)

	// module 1's own capacity failures are free too (the four skip tokens of risky/plausible), in a module 1 job
	e.fund(mnyUserA, "m1", 100)
	job1 := e.newJob(mnyUserA, "m1")
	e.reserve(job1, 10)
	e.setStatus(job1, "succeeded")
	var capItems []money.SettleItem
	for _, tok := range []string{"no_eligible_node", "audit_red", "warmup_cap", "rate_limited"} {
		capItems = append(capItems, mnyItem("cap-"+tok, charge.Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: tok}))
	}
	capItems = append(capItems, mnyItem("cap-none", charge.Outcome{Module: "m1", State: "risky", Reason: "plausible"})) // the control: no token is a cheap-layers answer, charged
	sr1 := e.settle(job1, capItems...)
	mnyRequireSettle(t, "module 1 capacity failures", sr1, 1, 1, 0, 4, 0, 0)

	// the credits of everything free come back at the release
	e.setStatus(job, "succeeded")
	rel, err := e.store.Release(ctx, job)
	mnyNoErr(t, "the release", err)
	if rel.Released != 19 || rel.Replayed {
		t.Fatalf("the release returned %+v, want the 19 credits of the free items and the unsettled rest", rel)
	}
	e.requireBal("after the release", mnyUserA, "m2", 99, 0)
	e.requireViewsEmpty("at the end")
}

// TestMoneyStore_SettleM1Unknown: module 1's `unknown` outcomes. A genuine remote answer (greylisted) is charged at m1_own; the five reasons that cannot tell our failure from an answer (dns_error,
// smtp_timeout, smtp_error, connection_failed, smtp_blocked) are UNMAPPED: returned to the caller with their rule ids M1-18..22, never charged, never guessed, and returned again by a second call.
// It fails if an ambiguous reason is charged or dropped without being reported, or if greylisted stops being charged.
func TestMoneyStore_SettleM1Unknown(t *testing.T) {
	e := newMnyEnv(t)
	e.fund(mnyUserA, "m1", 100)
	job := e.newJob(mnyUserA, "m1")
	e.reserve(job, 10)
	e.setStatus(job, "running")

	items := []money.SettleItem{
		mnyItem("greylisted", charge.Outcome{Module: "m1", State: "unknown", Reason: "greylisted"}),
		mnyItem("control-ok", mnyM1Accepted),
	}
	wantRule := map[string]string{}
	for i, reason := range []string{"dns_error", "smtp_timeout", "smtp_error", "connection_failed", "smtp_blocked"} {
		ref := "ambiguous-" + reason
		items = append(items, mnyItem(ref, charge.Outcome{Module: "m1", State: "unknown", Reason: reason}))
		wantRule[ref] = fmt.Sprintf("M1-%d", 18+i)
	}
	sr := e.settle(job, items...)
	mnyRequireSettle(t, "module 1 unknown outcomes", sr, 2, 2, 0, 0, 0, 5)
	if sr.ByUnit["m1_own"] != 2 {
		t.Fatalf("ByUnit = %v, want greylisted and the control charged at m1_own", sr.ByUnit)
	}
	for _, u := range sr.Unmapped {
		if want, ok := wantRule[u.ItemRef]; !ok || u.Rule != want || strings.TrimSpace(u.Note) == "" {
			t.Fatalf("unmapped item %+v: want one of %v with its rule id and a note", u, wantRule)
		}
		delete(wantRule, u.ItemRef)
	}
	if len(wantRule) != 0 {
		t.Fatalf("these ambiguous items were not reported as unmapped: %v", wantRule)
	}
	if got := e.rows("SELECT item_ref FROM platform.ledger WHERE job_id = $1 AND kind = 'charge' ORDER BY item_ref", job); strings.Join(got, ",") != "control-ok,greylisted" {
		t.Fatalf("the charged items are %v, want exactly greylisted and control-ok", got)
	}
	// reported again, not guessed: the second call returns the same five and settles nothing new
	again := e.settle(job, items...)
	mnyRequireSettle(t, "the same page again", again, 0, 0, 2, 0, 0, 5)
	e.requireViewsEmpty("at the end")
}

// TestMoneyStore_SettleIsIdempotent: settling a page TWICE charges once (the second call says AlreadySettled and writes nothing), also when the reserve is fully consumed (a replay is not "exceeds the
// reserve"). A second report with a DIFFERENT outcome for an item that is already charged is ignored: the first charge stands at the first price. Then the database alone: a raw INSERT, as
// platform_app, of a second charge for the same item under ANOTHER idempotency key is refused 23505 ledger_one_charge_per_item (control: a new item passes), and a raw charge beyond the reserve is
// refused 23514 reserves_not_overspent although the user's pooled reserved credits (another job's) could pay for it (control: the other job's reserve takes the same charge).
// It fails if a replay charges again, if the second outcome changes the first charge, or if either database wall is missing.
func TestMoneyStore_SettleIsIdempotent(t *testing.T) {
	e := newMnyEnv(t)

	// part 1: module 2, a fully consumed reserve, the same page twice, then a different outcome for the same items
	e.fund(mnyUserB, "m2", 100)
	job := e.newJob(mnyUserB, "m2")
	e.reserve(job, 3)
	e.setStatus(job, "running")
	page := []money.SettleItem{mnyItem("s1", mnyM2Sent), mnyItem("s2", mnyM2Sent), mnyItem("s3", mnyM2Bounced), mnyItem("f1", mnyM2Failed), mnyItem("q1", mnyM2Queued), mnyItem("u1", mnyM2Weird)}
	first := e.settle(job, page...)
	mnyRequireSettle(t, "the first settlement", first, 3, 3, 0, 1, 1, 1)
	if first.ByUnit["m2_accepted"] != 2 || first.ByUnit["m2_bounced"] != 1 {
		t.Fatalf("ByUnit = %v, want two m2_accepted and one m2_bounced", first.ByUnit)
	}
	e.requireBal("after the first settlement", mnyUserB, "m2", 97, 0)
	before := e.state()
	second := e.settle(job, page...)
	mnyRequireSettle(t, "the same page again (the reserve is fully consumed)", second, 0, 0, 3, 1, 1, 1)
	e.requireSame("the same page again", before)

	different := []money.SettleItem{mnyItem("s1", mnyM2Bounced), mnyItem("s2", mnyM2Failed), mnyItem("s3", mnyM2Sent)}
	third := e.settle(job, different...)
	mnyRequireSettle(t, "a different outcome for items already charged", third, 0, 0, 3, 0, 0, 0)
	if got := e.rows("SELECT item_ref, unit, unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'charge' ORDER BY item_ref", job); strings.Join(got, ",") != "s1|m2_accepted|1,s2|m2_accepted|1,s3|m2_bounced|1" {
		t.Fatalf("the charge rows are %v after a different second report: the first charge must stand", got)
	}
	e.requireSame("a different outcome for settled items", before)

	// part 2: module 1, where the price differs by outcome: own 1, vendor 2
	e.fund(mnyUserA, "m1", 100)
	job1 := e.newJob(mnyUserA, "m1")
	res1 := e.reserve(job1, 2) // the dearest price 2 x 2 units = 4 reserved
	e.setStatus(job1, "running")
	mnyRequireSettle(t, "item x as an own answer", e.settle(job1, mnyItem("x", mnyM1Accepted)), 1, 1, 0, 0, 0, 0)
	mnyRequireSettle(t, "item x reported again as a vendor answer", e.settle(job1, mnyItem("x", mnyM1Vendor)), 0, 0, 1, 0, 0, 0)
	if got := e.rows("SELECT unit, unit_cost FROM platform.ledger WHERE job_id = $1 AND item_ref = 'x'", job1); len(got) != 1 || got[0] != "m1_own|1" {
		t.Fatalf("item x is %v, want it charged once at the FIRST price (m1_own, 1)", got)
	}
	e.requireBal("after item x", mnyUserA, "m1", 96, 3)

	// the database alone: another idempotency key does not get a second charge through
	before = e.state()
	err := e.rawCharge(mnyUserA, "m1", job1, res1.ReserveID, "m1_own", 1, "x", "raw-second-key")
	mnyPgRefusal(t, "a second charge for item x under another idempotency key", err, "23505", "ledger_one_charge_per_item")
	e.requireSame("the refused raw charge", before)
	mnyNoErr(t, "control: a charge for a NEW item through the same raw statement", e.rawCharge(mnyUserA, "m1", job1, res1.ReserveID, "m1_own", 1, "y", "raw-new-item"))
	e.requireBal("after the control raw charge", mnyUserA, "m1", 96, 2)

	// the database alone: a charge beyond the reserve, while another reserve of the same user holds plenty (the pooled balance cannot hide it)
	e.fund(mnyUserC, "m2", 100)
	small := e.newJob(mnyUserC, "m2")
	smallRes := e.reserve(small, 2)
	big := e.newJob(mnyUserC, "m2")
	bigRes := e.reserve(big, 10)
	e.setStatus(small, "running")
	mnyRequireSettle(t, "the small reserve fully spent", e.settle(small, mnyItem("a", mnyM2Sent), mnyItem("b", mnyM2Sent)), 2, 2, 0, 0, 0, 0)
	e.requireBal("the pooled reserved credits", mnyUserC, "m2", 88, 10)
	before = e.state()
	err = e.rawCharge(mnyUserC, "m2", small, smallRes.ReserveID, "m2_accepted", 1, "c", "raw-over-1")
	mnyPgRefusal(t, "a third charge on a reserve of two", err, "23514", "reserves_not_overspent")
	e.requireSame("the refused over-charge", before)
	mnyNoErr(t, "control: the same raw statement against the reserve that has room", e.rawCharge(mnyUserC, "m2", big, bigRes.ReserveID, "m2_accepted", 1, "c", "raw-over-2"))
	e.requireBal("after the control", mnyUserC, "m2", 88, 9)

	e.requireViewsEmpty("at the end")
	e.requireReservesMatchLedger("at the end")
}

// TestMoneyStore_SettleRefusals: a batch whose new charges exceed what the reserve still holds is ErrReserveExceeded and NOTHING is written (3 credits held, 4 items); the same 3 items pass (the
// control) and a fourth new item is then refused while an already settled one is not. Item references that are blank, too long or repeated are ErrBadItem; a unit with no frozen price is
// ErrUnpriced; a job with no reserve is ErrNoReserve; a draft is ErrJobState; an unknown job is ErrJobNotFound; module 3 is ErrNotMetered; 10001 items are an InvalidError.
// It fails if a partial batch is written, or if any refusal writes anything.
func TestMoneyStore_SettleRefusals(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 100)
	job := e.newJob(mnyUserA, "m2")
	e.reserve(job, 3)

	// a draft job (it holds a reserve, but has not left draft)
	_, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem("d1", mnyM2Sent)}})
	mnyIs(t, "a settle for a draft job", err, money.ErrJobState)
	e.setStatus(job, "running")

	four := []money.SettleItem{mnyItem("r1", mnyM2Sent), mnyItem("r2", mnyM2Sent), mnyItem("r3", mnyM2Sent), mnyItem("r4", mnyM2Sent)}
	before := e.state()
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: job, Items: four})
	mnyIs(t, "4 charges against a reserve of 3", err, money.ErrReserveExceeded)
	if !strings.Contains(err.Error(), "has 3 left") {
		t.Fatalf("the error does not say what the reserve has left: %v", err)
	}
	e.requireSame("the refused batch", before)
	if n := e.chargeRows(job); n != 0 {
		t.Fatalf("%d charge row(s) written by a batch that was refused as a whole", n)
	}
	mnyRequireSettle(t, "control: the first three of the same items", e.settle(job, four[:3]...), 3, 3, 0, 0, 0, 0)
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: job, Items: four[3:]})
	mnyIs(t, "a fourth new item against a consumed reserve", err, money.ErrReserveExceeded)
	mnyRequireSettle(t, "control: an item that is already settled is not 'over the reserve'", e.settle(job, four[0]), 0, 0, 1, 0, 0, 0)

	// malformed items
	before = e.state()
	for _, c := range []struct {
		name  string
		items []money.SettleItem
		want  string
	}{
		{"a blank item_ref", []money.SettleItem{mnyItem("ok-1", mnyM2Sent), mnyItem("  ", mnyM2Sent)}, "blank"},
		{"an item_ref of 201 bytes", []money.SettleItem{mnyItem(strings.Repeat("x", 201), mnyM2Sent)}, "201 bytes"},
		{"a repeated item_ref", []money.SettleItem{mnyItem("dup", mnyM2Sent), mnyItem("other", mnyM2Sent), mnyItem("dup", mnyM2Bounced)}, "repeats"},
	} {
		_, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: c.items})
		mnyIs(t, c.name, err, money.ErrBadItem)
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: the error %v does not say %q", c.name, err, c.want)
		}
	}
	tooMany := make([]money.SettleItem, money.MaxSettleItems+1)
	for i := range tooMany {
		tooMany[i] = mnyItem(fmt.Sprintf("many-%d", i), mnyM2Queued)
	}
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: job, Items: tooMany})
	mnyInvalid(t, "10001 items in one call", err, "items")
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: "nope", Items: four[:1]})
	mnyInvalid(t, "a bad job id", err, "job_id")
	e.requireSame("the malformed requests", before)

	// an empty page is legal (the control of the many-items refusal) and does nothing
	mnyRequireSettle(t, "an empty page", e.settle(job), 0, 0, 0, 0, 0, 0)

	// a unit with no frozen price: an m2 outcome in a module 1 job
	e.fund(mnyUserA, "m1", 10)
	job1 := e.newJob(mnyUserA, "m1")
	e.reserve(job1, 2)
	e.setStatus(job1, "running")
	before = e.state()
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: job1, Items: []money.SettleItem{mnyItem("ok", mnyM1Accepted), mnyItem("alien", mnyM2Sent)}})
	mnyIs(t, "an m2_accepted charge in a module 1 job", err, money.ErrUnpriced)
	e.requireSame("the unpriced batch (the legal item in it is not written either)", before)
	mnyRequireSettle(t, "control: the legal item alone", e.settle(job1, mnyItem("ok", mnyM1Accepted)), 1, 1, 0, 0, 0, 0)

	// jobs the store cannot settle
	noReserve := e.newJob(mnyUserB, "m2")
	e.setStatus(noReserve, "running")
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: noReserve, Items: four[:1]})
	mnyIs(t, "a job that never reserved", err, money.ErrNoReserve)
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: mnyNobody, Items: four[:1]})
	mnyIs(t, "a job that does not exist", err, money.ErrJobNotFound)
	m3 := e.newJob(mnyUserA, "m3")
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: m3, Items: four[:1]})
	mnyIs(t, "a module 3 job", err, money.ErrNotMetered)
	e.requireViewsEmpty("at the end")
}

// ---------------------------------------------------------------------------------------------------------------------
// release
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneyStore_Release: a job that has not ended (draft, scheduled, running, paused, cancelling) is ErrJobNotEnded and nothing is written; once it ended, the release returns EXACTLY the remainder
// (10 reserved, 4 charged: 6), once: the second call returns 0 and says Replayed. A fully consumed reserve releases 0 and writes no row. Failed and cancelled jobs release too. A charge after the
// release is refused (ErrReserveExceeded through the store, 23514 reserves_not_overspent as a raw insert, while another job's reserve keeps the pooled balance positive); a raw release of any other
// amount is refused 23514 release_is_the_remainder, and the exact one passes. No reserve is ErrNoReserve.
// It fails if a release returns more or less than the remainder, twice, or for a job that has not ended.
func TestMoneyStore_Release(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 100)
	job := e.newJob(mnyUserA, "m2")
	res := e.reserve(job, 10)
	other := e.newJob(mnyUserA, "m2") // another job's reserve: it keeps the pooled reserved balance positive for the raw proofs below
	e.reserve(other, 5)

	for _, status := range []string{"draft", "scheduled", "running", "paused", "cancelling"} {
		e.setStatus(job, status)
		before := e.state()
		_, err := e.store.Release(ctx, job)
		mnyIs(t, "a release for a "+status+" job", err, money.ErrJobNotEnded)
		e.requireSame("a release for a "+status+" job", before)
	}
	e.setStatus(job, "running")
	mnyRequireSettle(t, "four sent items", e.settle(job, mnyItem("a", mnyM2Sent), mnyItem("b", mnyM2Sent), mnyItem("c", mnyM2Sent), mnyItem("d", mnyM2Bounced)), 4, 4, 0, 0, 0, 0)
	e.requireBal("before the release", mnyUserA, "m2", 85, 11)
	e.setStatus(job, "succeeded")

	rel, err := e.store.Release(ctx, job)
	mnyNoErr(t, "the release", err)
	if rel.Released != 6 || rel.Replayed || rel.ReserveID != res.ReserveID {
		t.Fatalf("the release returned %+v, want exactly the remainder 6 of reserve %d", rel, res.ReserveID)
	}
	if got := e.rows("SELECT available_delta, reserved_delta, reserve_id, idempotency_key, job_id::text FROM platform.ledger WHERE kind = 'release' AND reserve_id = $1", res.ReserveID); len(got) != 1 || got[0] != fmt.Sprintf("6|-6|%d|release:%d|%s", res.ReserveID, res.ReserveID, job) {
		t.Fatalf("the release rows are %v", got)
	}
	e.requireBal("after the release", mnyUserA, "m2", 91, 5)
	before := e.state()
	again, err := e.store.Release(ctx, job)
	mnyNoErr(t, "the second release", err)
	if again.Released != 0 || !again.Replayed || again.ReserveID != res.ReserveID {
		t.Fatalf("the second release returned %+v, want 0 released and Replayed", again)
	}
	e.requireSame("the second release", before)

	// a charge after the release
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem("late", mnyM2Sent)}})
	mnyIs(t, "a charge after the release", err, money.ErrReserveExceeded)
	e.requireSame("a charge after the release", before)
	err = e.rawCharge(mnyUserA, "m2", job, res.ReserveID, "m2_accepted", 1, "late-raw", "raw-late")
	mnyPgRefusal(t, "a raw charge after the release", err, "23514", "reserves_not_overspent")
	e.requireSame("a raw charge after the release", before)

	// failed and cancelled jobs release as well; a fully consumed reserve releases nothing and writes no row
	for _, status := range []string{"failed", "cancelled"} {
		j := e.newJob(mnyUserA, "m2")
		r := e.reserve(j, 2)
		e.setStatus(j, status)
		got, err := e.store.Release(ctx, j)
		if err != nil || got.Released != 2 || got.ReserveID != r.ReserveID {
			t.Fatalf("the release of a %s job returned %+v, %v, want 2 credits", status, got, err)
		}
	}
	full := e.newJob(mnyUserA, "m2")
	fullRes := e.reserve(full, 3)
	e.setStatus(full, "running")
	mnyRequireSettle(t, "the whole reserve consumed", e.settle(full, mnyItem("1", mnyM2Sent), mnyItem("2", mnyM2Sent), mnyItem("3", mnyM2Sent)), 3, 3, 0, 0, 0, 0)
	e.setStatus(full, "succeeded")
	before = e.state()
	none, err := e.store.Release(ctx, full)
	mnyNoErr(t, "the release of a fully consumed reserve", err)
	if none.Released != 0 || none.Replayed || none.ReserveID != fullRes.ReserveID {
		t.Fatalf("the release of a fully consumed reserve returned %+v, want 0 released and not Replayed", none)
	}
	e.requireSame("the release of a fully consumed reserve", before)

	// a raw release of the wrong amount is refused by the database, the exact amount passes
	rawJob := e.newJob(mnyUserA, "m2")
	rawRes := e.reserve(rawJob, 3)
	e.setStatus(rawJob, "succeeded")
	rawRelease := func(amount int64, key string) error {
		_, err := e.pool.Exec(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, idempotency_key)
			VALUES ($1, 'm2', 'release', $2, $3, $4, $5, $6)`, mnyUserA, amount, -amount, rawJob, rawRes.ReserveID, key)
		return err
	}
	before = e.state()
	mnyPgRefusal(t, "a raw release of 1 of 3", rawRelease(1, "raw-release-1"), "23514", "release_is_the_remainder")
	mnyPgRefusal(t, "a raw release of 4 of 3", rawRelease(4, "raw-release-4"), "23514", "release_is_the_remainder")
	e.requireSame("the refused raw releases", before)
	mnyNoErr(t, "control: the exact raw release", rawRelease(3, "raw-release-3"))
	back, err := e.store.Release(ctx, rawJob)
	if err != nil || back.Released != 0 || !back.Replayed {
		t.Fatalf("the store's release after a raw exact release returned %+v, %v, want 0 and Replayed", back, err)
	}

	// no reserve, no job, module 3, bad id
	noRes := e.newJob(mnyUserB, "m2")
	e.setStatus(noRes, "succeeded")
	_, err = e.store.Release(ctx, noRes)
	mnyIs(t, "a release for a job that never reserved", err, money.ErrNoReserve)
	_, err = e.store.Release(ctx, mnyNobody)
	mnyIs(t, "a release for a job that does not exist", err, money.ErrJobNotFound)
	m3 := e.newJob(mnyUserA, "m3")
	_, err = e.store.Release(ctx, m3)
	mnyIs(t, "a release for a module 3 job", err, money.ErrNotMetered)
	_, err = e.store.Release(ctx, "x")
	mnyInvalid(t, "a release with a bad job id", err, "job_id")
	e.requireViewsEmpty("at the end")
	e.requireReservesMatchLedger("at the end")
}

// ---------------------------------------------------------------------------------------------------------------------
// the whole cycle, the transaction twins, the balance read
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneyStore_FullCycle: grant -> reserve -> settle (mixed outcomes) -> release -> adjustment, on both metered modules, with every number computed by hand: module 1 is 100 granted, 40 reserved
// (20 units at the vendor price 2), 8 charged (4 own + 2 cached at 1, one vendor at 2), 32 released, 12 clawed back = 80 available and 0 reserved; module 2 is 50 - 4 = 46. At the end the two invariant views are
// empty, platform.reserves equals a recompute from the ledger, and the ledger holds exactly the rows of the story.
// It fails if any number differs, or if the books (balance_drift, reserve_overdrawn) disagree.
func TestMoneyStore_FullCycle(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	grant := func(module string, amount int64, key string) {
		t.Helper()
		if _, err := e.store.Grant(ctx, money.GrantReq{UserID: mnyUserA, Module: module, Amount: amount, AdminID: mnyAdminA, Reason: "full cycle", Key: key}); err != nil {
			t.Fatalf("grant %d on %s: %v", amount, module, err)
		}
	}

	// module 1
	grant("m1", 100, "cycle-m1")
	job := e.newJob(mnyUserA, "m1")
	res := e.reserve(job, 20)
	if res.Reserved != 40 {
		t.Fatalf("the module 1 reserve holds %d, want 20 units x 2 = 40", res.Reserved)
	}
	e.requireBal("module 1 after the reserve", mnyUserA, "m1", 60, 40)
	e.setStatus(job, "running")
	sr := e.settle(job,
		mnyItem("i1", mnyM1Accepted), mnyItem("i2", charge.Outcome{Module: "m1", State: "valid", Reason: "mailbox_full"}),
		mnyItem("i3", charge.Outcome{Module: "m1", State: "invalid", Reason: "no_mx"}), mnyItem("i4", charge.Outcome{Module: "m1", State: "unknown", Reason: "greylisted"}),
		mnyItem("i5", mnyM1Cached), mnyItem("i6", mnyM1Cached), mnyItem("i7", mnyM1Vendor),
		mnyItem("i8", mnyM1DNS), mnyItem("i9", mnyM1Capacity))
	mnyRequireSettle(t, "module 1 mixed outcomes", sr, 7, 8, 0, 1, 0, 1)
	if sr.ByUnit["m1_own"] != 4 || sr.ByUnit["m1_cache"] != 2 || sr.ByUnit["m1_vendor"] != 1 || len(sr.ByUnit) != 3 {
		t.Fatalf("ByUnit = %v, want 4 own, 2 cache, 1 vendor", sr.ByUnit)
	}
	e.requireBal("module 1 after the settlement", mnyUserA, "m1", 60, 32)
	e.setStatus(job, "succeeded")
	rel, err := e.store.Release(ctx, job)
	mnyNoErr(t, "the module 1 release", err)
	if rel.Released != 32 {
		t.Fatalf("the module 1 release returned %+v, want 32", rel)
	}
	e.requireBal("module 1 after the release", mnyUserA, "m1", 92, 0)
	adj, err := e.store.Adjust(ctx, money.AdjustReq{UserID: mnyUserA, Module: "m1", Amount: 12, AdminID: mnyAdminA, Reason: "refund the customer's chargeback", Key: "cycle-adjust"})
	mnyNoErr(t, "the adjustment", err)
	e.requireBal("module 1 at the end", mnyUserA, "m1", 80, 0)

	// module 2
	grant("m2", 50, "cycle-m2")
	job2 := e.newJob(mnyUserA, "m2")
	e.reserve(job2, 10)
	e.setStatus(job2, "running")
	sr2 := e.settle(job2, mnyItem("m1", mnyM2Sent), mnyItem("m2", mnyM2Sent), mnyItem("m3", mnyM2Sent), mnyItem("m4", mnyM2Bounced), mnyItem("m5", mnyM2Failed), mnyItem("m6", mnyM2Queued))
	mnyRequireSettle(t, "module 2 mixed outcomes", sr2, 4, 4, 0, 1, 1, 0)
	e.setStatus(job2, "succeeded")
	if rel2, err := e.store.Release(ctx, job2); err != nil || rel2.Released != 6 {
		t.Fatalf("the module 2 release returned %+v, %v, want 6", rel2, err)
	}
	e.requireBal("module 2 at the end", mnyUserA, "m2", 46, 0)

	// the books
	e.requireViewsEmpty("at the end of the cycle")
	if n := e.requireReservesMatchLedger("at the end of the cycle"); n != 2 {
		t.Fatalf("%d reserves compared, want 2", n)
	}
	if got := e.rows(`SELECT module_id, kind, count(*)::bigint FROM platform.ledger WHERE user_id = $1 GROUP BY module_id, kind ORDER BY module_id, kind`, mnyUserA); strings.Join(got, ",") !=
		"m1|adjustment|1,m1|charge|7,m1|grant|1,m1|release|1,m1|reserve|1,m2|charge|4,m2|grant|1,m2|release|1,m2|reserve|1" {
		t.Fatalf("the ledger rows by module and kind are %v", got)
	}
	if got := e.rows("SELECT count(*)::bigint FROM platform.admin_audit WHERE action = 'ledger.adjustment' AND target_id = $1", fmt.Sprint(adj.ID)); len(got) != 1 || got[0] != "1" {
		t.Fatalf("the audit rows of the adjustment are %v, want exactly one", got)
	}
	for module, want := range map[string]money.Balance{"m1": {Available: 80, Reserved: 0, Exists: true}, "m2": {Available: 46, Reserved: 0, Exists: true}} {
		if b, err := e.store.Balance(ctx, mnyUserA, module); err != nil || b != want {
			t.Fatalf("Store.Balance on %s = %+v, %v, want %+v", module, b, err, want)
		}
	}
}

// TestMoneyStore_InTwinsRunInTheCallersTransaction: the In twins write inside a transaction the CALLER owns and nothing else. ReserveIn then a rollback leaves nothing (no ledger row, no snapshot row, the
// balance as it was), and the open transaction sees its own reserve (BalanceIn) while the pool does not (Balance). GrantIn and AdjustIn roll back the same way. SettleIn and ReleaseIn in one
// transaction see each other's writes and persist together when it commits.
// It fails if an In function commits for itself, or if the caller's transaction cannot undo it.
func TestMoneyStore_InTwinsRunInTheCallersTransaction(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 20)
	job := e.newJob(mnyUserA, "m2")
	begin := func() pgx.Tx {
		tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mnyNoErr(t, "begin", err)
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	before := e.state()

	tx := begin()
	res, err := e.store.ReserveIn(ctx, tx, money.ReserveReq{JobID: job, Units: 5})
	mnyNoErr(t, "ReserveIn", err)
	if res.Reserved != 5 || res.Replayed {
		t.Fatalf("ReserveIn returned %+v", res)
	}
	in, err := e.store.BalanceIn(ctx, tx, mnyUserA, "m2")
	if err != nil || in != (money.Balance{Available: 15, Reserved: 5, Exists: true}) {
		t.Fatalf("BalanceIn inside the transaction = %+v, %v, want 15/5", in, err)
	}
	out, err := e.store.Balance(ctx, mnyUserA, "m2")
	if err != nil || out != (money.Balance{Available: 20, Reserved: 0, Exists: true}) {
		t.Fatalf("Balance outside the open transaction = %+v, %v, want the committed 20/0 (ReserveIn must not commit)", out, err)
	}
	mnyNoErr(t, "rollback", tx.Rollback(ctx))
	e.requireSame("a rolled-back ReserveIn", before)

	tx = begin()
	if _, err := e.store.GrantIn(ctx, tx, money.GrantReq{UserID: mnyUserA, Module: "m2", Amount: 7, AdminID: mnyAdminA, Reason: "in a transaction", Key: "in-grant"}); err != nil {
		t.Fatalf("GrantIn: %v", err)
	}
	if _, err := e.store.AdjustIn(ctx, tx, money.AdjustReq{UserID: mnyUserA, Module: "m2", Amount: 3, AdminID: mnyAdminA, Reason: "in a transaction", Key: "in-adjust"}); err != nil {
		t.Fatalf("AdjustIn: %v", err)
	}
	if in, err := e.store.BalanceIn(ctx, tx, mnyUserA, "m2"); err != nil || in.Available != 24 {
		t.Fatalf("BalanceIn after GrantIn and AdjustIn = %+v, %v, want 24 available", in, err)
	}
	mnyNoErr(t, "rollback", tx.Rollback(ctx))
	e.requireSame("a rolled-back GrantIn and AdjustIn", before)

	// the committed path: the reserve commits, then settle and release run in ONE transaction and see each other
	tx = begin()
	if _, err := e.store.ReserveIn(ctx, tx, money.ReserveReq{JobID: job, Units: 5}); err != nil {
		t.Fatalf("ReserveIn: %v", err)
	}
	mnyNoErr(t, "commit the reserve", tx.Commit(ctx))
	if b, err := e.store.Balance(ctx, mnyUserA, "m2"); err != nil || b.Available != 15 || b.Reserved != 5 {
		t.Fatalf("Balance after the committed ReserveIn = %+v, %v, want 15/5", b, err)
	}
	e.setStatus(job, "succeeded")
	before = e.state()
	tx = begin()
	sr, err := e.store.SettleIn(ctx, tx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem("a", mnyM2Sent), mnyItem("b", mnyM2Sent)}})
	if err != nil || sr.Charged != 2 {
		t.Fatalf("SettleIn returned %+v, %v", sr, err)
	}
	rel, err := e.store.ReleaseIn(ctx, tx, job)
	if err != nil || rel.Released != 3 {
		t.Fatalf("ReleaseIn in the same transaction returned %+v, %v, want the remainder 3 (it must see the SettleIn)", rel, err)
	}
	mnyNoErr(t, "rollback of settle and release", tx.Rollback(ctx))
	e.requireSame("a rolled-back SettleIn and ReleaseIn", before)
	tx = begin()
	if _, err := e.store.SettleIn(ctx, tx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem("a", mnyM2Sent), mnyItem("b", mnyM2Sent)}}); err != nil {
		t.Fatalf("SettleIn: %v", err)
	}
	if _, err := e.store.ReleaseIn(ctx, tx, job); err != nil {
		t.Fatalf("ReleaseIn: %v", err)
	}
	mnyNoErr(t, "commit of settle and release", tx.Commit(ctx))
	e.requireBal("after the committed settle and release", mnyUserA, "m2", 18, 0)
	e.requireViewsEmpty("at the end")
	e.requireReservesMatchLedger("at the end")
}

// TestMoneyStore_Balance: a user with no balance row reads as 0/0 with Exists false; after a grant it exists; a module the user has nothing on stays absent; module 3 and a malformed id are refused by name.
// It fails if the read invents a row or confuses modules.
func TestMoneyStore_Balance(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	if b, err := e.store.Balance(ctx, mnyUserA, "m1"); err != nil || b != (money.Balance{}) {
		t.Fatalf("Balance of a user with nothing = %+v, %v, want the zero value (Exists false)", b, err)
	}
	e.fund(mnyUserA, "m1", 9)
	if b, err := e.store.Balance(ctx, mnyUserA, "m1"); err != nil || b != (money.Balance{Available: 9, Exists: true}) {
		t.Fatalf("Balance after funding = %+v, %v, want 9 and Exists", b, err)
	}
	if b, err := e.store.Balance(ctx, mnyUserA, "m2"); err != nil || b.Exists || b.Available != 0 {
		t.Fatalf("Balance on the other module = %+v, %v, want nothing", b, err)
	}
	if b, err := e.store.Balance(ctx, mnyUserB, "m1"); err != nil || b.Exists {
		t.Fatalf("Balance of another user = %+v, %v, want nothing", b, err)
	}
	_, err := e.store.Balance(ctx, mnyUserA, "m3")
	mnyIs(t, "a balance on module 3", err, money.ErrNotMetered)
	_, err = e.store.Balance(ctx, "x", "m1")
	mnyInvalid(t, "a balance for a bad user id", err, "user_id")
}
