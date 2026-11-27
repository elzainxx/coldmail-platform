package money

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The rate quote of a job that is opted out of the vendor (Phase 5 S6: ReserveRateForIn). These tests need no database: they hold what the statement is made of and what it is given. That the quote and the
// reserve it announces are the same set of units, that the snapshot leaves the vendor unit out and that the ledger refuses a reserve that does not, are proved against a real PostgreSQL by tests/harness
// (jobs_vendor_optout_test.go and money_optout_test.go).

// queryTx records every statement it is asked to Query and refuses to answer (the error is the test's way of stopping at the first statement).
type queryTx struct {
	pgx.Tx
	stmts []stmtRecord
}

var errRecorded = errors.New("recorded and refused")

func (q *queryTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.stmts = append(q.stmts, stmtRecord{sql, args})
	return nil, errRecorded
}

func TestReserveRate_TheVendorUnitIsLeftOutOnlyWhenTheJobIsOptedOutAndTheFlagIsAParameter(t *testing.T) {
	s := New(nil)
	// ReserveRateIn is the opted-in quote; ReserveRateForIn says which: the SAME statement, the flag the second parameter
	for _, c := range []struct {
		name string
		run  func(tx pgx.Tx) error
		want bool
	}{
		{"ReserveRateIn", func(tx pgx.Tx) error { _, err := s.ReserveRateIn(context.Background(), tx, "m1", false); return err }, false},
		{"ReserveRateForIn, opted in", func(tx pgx.Tx) error {
			_, err := s.ReserveRateForIn(context.Background(), tx, "m1", false, false)
			return err
		}, false},
		{"ReserveRateForIn, opted out", func(tx pgx.Tx) error {
			_, err := s.ReserveRateForIn(context.Background(), tx, "m1", false, true)
			return err
		}, true},
	} {
		tx := &queryTx{}
		if err := c.run(tx); err == nil {
			t.Fatalf("%s: answered although the fake refuses every statement", c.name)
		}
		if len(tx.stmts) != 1 {
			t.Fatalf("%s: ran %d statements, want the one that reads the reachable prices", c.name, len(tx.stmts))
		}
		got := tx.stmts[0]
		sql := strings.Join(strings.Fields(got.sql), " ")
		if !strings.Contains(sql, "AND reachable AND NOT ($2::boolean AND needs_vendor)") || strings.Contains(sql, "m1_vendor") {
			t.Errorf("%s: the statement is %q: it must filter by the unit's own needs_vendor flag, never by a unit's name", c.name, sql)
		}
		if len(got.args) != 2 || got.args[0] != "m1" || got.args[1] != c.want {
			t.Errorf("%s: the arguments are %v, want [m1 %v]", c.name, got.args, c.want)
		}
	}
}

func TestReserveRate_WhateverTheJobTheRowsAreLockedBeforeTheyAreReadAndTheLockIgnoresTheChoice(t *testing.T) {
	// with lockPrices the FIRST statement locks EVERY price row of the module (the vendor's too, even for an opted-out job): a change of a price or of the vendor's reachability waits for the caller's
	// transaction exactly as it does for an opted-in job, so the rate that was compared is the rate that is reserved
	for _, optOut := range []bool{false, true} {
		s := New(nil)
		tx := &queryTx{}
		if _, err := s.ReserveRateForIn(context.Background(), tx, "m1", true, optOut); err == nil {
			t.Fatalf("optOut %v: answered although the fake refuses every statement", optOut)
		}
		if len(tx.stmts) != 1 {
			t.Fatalf("optOut %v: ran %d statements before the refusal, want the lock", optOut, len(tx.stmts))
		}
		sql := strings.Join(strings.Fields(tx.stmts[0].sql), " ")
		if !strings.Contains(sql, "FOR SHARE") || strings.Contains(sql, "needs_vendor") || strings.Contains(sql, "reachable") || len(tx.stmts[0].args) != 1 {
			t.Errorf("optOut %v: the lock statement is %q with %v: it must lock every unit of the module whatever the job's choice", optOut, sql, tx.stmts[0].args)
		}
	}
	// a module id that is not one is refused before any statement, for both entry points
	tx := &queryTx{}
	if _, err := New(nil).ReserveRateForIn(context.Background(), tx, "M1; DROP", false, true); !errors.Is(err, ErrInvalid) || len(tx.stmts) != 0 {
		t.Errorf("a bad module: %v after %d statement(s), want ErrInvalid and none", err, len(tx.stmts))
	}
}
