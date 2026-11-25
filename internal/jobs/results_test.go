package jobs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The paging of a job's results. The SQL of Results is proved against a real database (tests/harness: TestJobsAPI_*, and the store's own); what is proved here, without one, is the walk
// EachResult makes over it: the page it asks for, where it stops, what it does with an error, and that it holds one page of memory, never the list.

// pageFake is a ResultPager over n results (ordinals 1..n) that records every call.
type pageFake struct {
	n      int
	calls  []pageCall
	err    error // returned by the call numbered failOn (1-based), when failOn > 0
	failOn int
	hook   func(call int) // runs inside a call, before it answers
}

type pageCall struct{ after, limit int }

func (p *pageFake) Results(_ context.Context, _, _ string, after, limit int) ([]Result, error) {
	p.calls = append(p.calls, pageCall{after, limit})
	if p.hook != nil {
		p.hook(len(p.calls))
	}
	if p.failOn > 0 && len(p.calls) == p.failOn {
		return nil, p.err
	}
	var out []Result
	for o := after + 1; o <= p.n && len(out) < limit; o++ {
		out = append(out, Result{Ordinal: o, Email: fmt.Sprintf("r%d@example.test", o), Origin: "module", State: "valid", Decision: "charge", Unit: "m1_syntax", Rule: "P-01"})
	}
	return out, nil
}

const (
	testUser = "d0000000-0000-4000-8000-000000000001"
	testJob  = "0b000000-0000-4000-8000-000000000001"
)

func TestEachResultOf_ReadsPagesOfAThousandAndStopsOnAShortPage(t *testing.T) {
	for _, c := range []struct {
		n     int
		calls int // the number of pages asked for
	}{
		{0, 1}, {1, 1}, {999, 1},
		{1000, 2}, // a full page may be the last: the next page is empty, and that is how the walk learns it
		{1001, 2}, {2000, 3}, {2500, 3}, {3000, 4},
	} {
		p := &pageFake{n: c.n}
		var got []int
		if err := EachResultOf(context.Background(), p, testUser, testJob, func(r Result) error { got = append(got, r.Ordinal); return nil }); err != nil {
			t.Fatalf("%d results: %v", c.n, err)
		}
		if len(p.calls) != c.calls {
			t.Errorf("%d results: %d page calls, want %d (%v)", c.n, len(p.calls), c.calls, p.calls)
		}
		// every call asks for exactly one page, from where the walk had got to: nothing is held beyond a page, nothing is read twice
		for i, call := range p.calls {
			if call.limit != ResultsPageSize || call.after != i*ResultsPageSize {
				t.Errorf("%d results: call %d was (after %d, limit %d), want (after %d, limit %d)", c.n, i+1, call.after, call.limit, i*ResultsPageSize, ResultsPageSize)
			}
		}
		// each result once, in order
		if len(got) != c.n {
			t.Errorf("%d results: fn saw %d", c.n, len(got))
			continue
		}
		for i, o := range got {
			if o != i+1 {
				t.Errorf("%d results: the %dth result seen has ordinal %d", c.n, i+1, o)
				break
			}
		}
	}
	if ResultsPageSize != 1000 {
		t.Errorf("the page of EachResult is %d, the contract says 1000", ResultsPageSize)
	}
}

func TestEachResultOf_StopsAtTheFirstErrorAndReturnsItAsItIs(t *testing.T) {
	stop := errors.New("the client went away")

	// fn fails in the middle of the first page: no second page is asked for, and the error is fn's own (not wrapped, so the caller can tell its write failing from the store)
	p := &pageFake{n: 2500}
	seen := 0
	err := EachResultOf(context.Background(), p, testUser, testJob, func(Result) error {
		seen++
		if seen == 5 {
			return stop
		}
		return nil
	})
	if err != stop || seen != 5 || len(p.calls) != 1 {
		t.Fatalf("fn's error: returned %v after %d results and %d page calls, want it as it is after 5 and 1", err, seen, len(p.calls))
	}

	// the pager fails on its second page: the first page was delivered, nothing after
	boom := errors.New("the database is gone")
	p = &pageFake{n: 2500, failOn: 2, err: boom}
	seen = 0
	err = EachResultOf(context.Background(), p, testUser, testJob, func(Result) error { seen++; return nil })
	if err != boom || seen != ResultsPageSize || len(p.calls) != 2 {
		t.Fatalf("the pager's error: returned %v after %d results and %d calls, want it after one page and 2 calls", err, seen, len(p.calls))
	}

	// the control: with neither failing, everything is delivered and the walk ends cleanly
	p = &pageFake{n: 2500}
	seen = 0
	if err := EachResultOf(context.Background(), p, testUser, testJob, func(Result) error { seen++; return nil }); err != nil || seen != 2500 {
		t.Fatalf("without a failure: %v after %d results, want nil after 2500", err, seen)
	}
}

func TestEachResultOf_AContextThatEndsStopsTheWalkBetweenPages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pageFake{n: 5000}
	p.hook = func(call int) {
		if call == 2 {
			cancel() // the client goes away while the second page is being read
		}
	}
	seen := 0
	err := EachResultOf(ctx, p, testUser, testJob, func(Result) error { seen++; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("returned %v, want context.Canceled", err)
	}
	if len(p.calls) != 2 || seen != 2*ResultsPageSize {
		t.Fatalf("after the cancel the walk made %d calls and delivered %d results, want it to stop after the page in hand (2 calls, %d results)", len(p.calls), seen, 2*ResultsPageSize)
	}

	// the control: an unfinished context lets the same walk run to its end
	p = &pageFake{n: 5000}
	seen = 0
	if err := EachResultOf(context.Background(), p, testUser, testJob, func(Result) error { seen++; return nil }); err != nil || seen != 5000 || len(p.calls) != 6 {
		t.Fatalf("without a cancel: %v, %d results, %d calls, want nil, 5000 and 6", err, seen, len(p.calls))
	}
}

func TestEachResultOf_APagerThatDoesNotAdvanceCannotHoldTheCaller(t *testing.T) {
	stuck := &stuckPager{}
	err := EachResultOf(context.Background(), stuck, testUser, testJob, func(Result) error { return nil })
	if !errors.Is(err, ErrPagerStuck) {
		t.Fatalf("returned %v, want ErrPagerStuck", err)
	}
	if stuck.calls > 2 {
		t.Fatalf("the walk asked a stuck pager %d times", stuck.calls)
	}
}

// stuckPager answers the same full page whatever it is asked for.
type stuckPager struct{ calls int }

func (s *stuckPager) Results(_ context.Context, _, _ string, _, limit int) ([]Result, error) {
	s.calls++
	out := make([]Result, limit)
	for i := range out {
		out[i] = Result{Ordinal: i + 1}
	}
	return out, nil
}

// Results refuses a request that is wrong before it asks the database, and lets a right one through to it. The control is a store over a pool nothing listens behind: a request that comes back
// with a connection error was not refused by the validation.
func TestResults_ValidatesBeforeTheDatabaseAndLetsALegalRequestThrough(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1") // lazy: nothing connects until a query
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool, dsn: pool.Config().ConnString()}

	ctx := context.Background()
	for _, c := range []struct {
		name         string
		user, job    string
		after, limit int
		want         error
	}{
		{"a user that is not a customer's uuid", "not-a-uuid", testJob, 0, 10, ErrBadRequest},
		{"the system user", systemUser, testJob, 0, 10, ErrBadRequest},
		{"a job id that is not a uuid", testUser, "not-a-uuid", 0, 10, ErrNotFound},
		{"a negative cursor", testUser, testJob, -1, 10, ErrBadRequest},
		{"limit 0", testUser, testJob, 0, 0, ErrBadRequest},
		{"limit one over the largest page plus one", testUser, testJob, 0, MaxResultsLimit + 1, ErrBadRequest},
	} {
		if _, err := s.Results(ctx, c.user, c.job, c.after, c.limit); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	for _, c := range []struct {
		name         string
		after, limit int
	}{{"limit 1", 0, 1}, {"the largest limit", 0, MaxResultsLimit}, {"a cursor of the int32 ceiling and over", 1 << 40, 10}} {
		_, err := s.Results(ctx, testUser, testJob, c.after, c.limit)
		if err == nil || errors.Is(err, ErrBadRequest) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want a database error: a legal request must reach the database", c.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "nowhere") && strings.Contains(err.Error(), "platform_app@") {
			t.Errorf("%s: the error carries the connection string: %v", c.name, err)
		}
	}
}

// A result carries what the customer may see and nothing of the module's own detail: adding a field to Result is a decision, so the list is pinned (the API's JSON and CSV views are pinned to it
// in internal/api).
func TestResult_HoldsExactlyWhatACustomerMaySee(t *testing.T) {
	var names []string
	rt := reflect.TypeOf(Result{})
	for i := 0; i < rt.NumField(); i++ {
		names = append(names, rt.Field(i).Name)
	}
	want := "Ordinal,Email,Origin,State,Reason,Decision,Unit,Rule,RecordedAt"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("Result has fields %s, want %s: a new field of a result is shown to the customer only on purpose (update internal/api's views with it)", got, want)
	}
}
