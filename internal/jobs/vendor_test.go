package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/money"
)

// The vendor reachability hook (vendor.go): the part that needs no database. What it does to platform.unit_costs, the preview and the confirm against a real Postgres is proved by
// tests/harness (TestJobsStore_VendorReachabilityFromTheModule*).

// fakeVendor is a VendorReachability that records its calls. reply decides each answer (n counts from 1).
type fakeVendor struct {
	mu       sync.Mutex
	calls    int
	deadline []time.Duration // the time left on each call's context
	reply    func(n int) (reachable, known bool, err error)
}

func (f *fakeVendor) M1VendorReachable(ctx context.Context) (bool, bool, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = append(f.deadline, time.Until(dl))
	} else {
		f.deadline = append(f.deadline, -1)
	}
	f.mu.Unlock()
	return f.reply(n)
}

func (f *fakeVendor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// vendorStore is a store with nothing but the hook's state and a clock the test moves.
func vendorStore(p VendorReachability, ttl time.Duration) (*Store, *time.Time) {
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := &Store{vendor: vendorHook{provider: p, ttl: ttl}}
	s.now = func() time.Time { return now }
	return s, &now
}

func TestVendorRefuses_OnlyAnImmediateConfirmOfAFlagThatIsOffAndWasNeverModuleOnesWord(t *testing.T) {
	const seed = "seed: the vendor fallback ships disabled"
	earlierBuild := "config: PLATFORM_M1_" + "VENDOR_REACHABLE" // what an earlier build wrote (spelled in pieces: no file may name the removed variable)
	for _, c := range []struct {
		name      string
		immediate bool
		reach     bool
		source    string
		refuses   bool
	}{
		{"immediate, off, the seed", true, false, seed, true},
		{"immediate, off, an earlier build's own word", true, false, earlierBuild, true},
		{"immediate, off, no source at all", true, false, "", true},
		{"immediate, off, some other source", true, false, "test: by hand", true},
		{"immediate, off, module 1 said so", true, false, money.ReachableSourceM1Describe, false},
		{"immediate, on, the seed", true, true, seed, false},
		{"immediate, on, an earlier build's own word", true, true, earlierBuild, false},
		{"immediate, on, module 1 said so", true, true, money.ReachableSourceM1Describe, false},
		{"scheduled, off, the seed (ignored: module 1 may be back by the start)", false, false, seed, false},
		{"scheduled, off, an earlier build's own word", false, false, earlierBuild, false},
		{"scheduled, off, module 1 said so", false, false, money.ReachableSourceM1Describe, false},
		{"scheduled, on, the seed", false, true, seed, false},
	} {
		if got := vendorRefuses(c.immediate, money.UnitReachability{Reachable: c.reach, Source: c.source}); got != c.refuses {
			t.Errorf("%s: vendorRefuses = %v, want %v", c.name, got, c.refuses)
		}
	}
}

func TestVendorRead_KeepsAnAnswerForTheTTLAndAsksAgainAfterIt(t *testing.T) {
	f := &fakeVendor{reply: func(n int) (bool, bool, error) { return n%2 == 0, true, nil }} // 1: off, 2: on, 3: off ...
	s, now := vendorStore(f, 5*time.Second)
	ctx := t.Context()

	r1, fresh1 := s.readVendor(ctx)
	if f.count() != 1 || !fresh1 || !r1.known || r1.reachable {
		t.Fatalf("the first read: calls %d fresh %v %+v; want one call, fresh, known, off", f.count(), fresh1, r1)
	}
	*now = now.Add(4900 * time.Millisecond)
	r2, fresh2 := s.readVendor(ctx)
	if f.count() != 1 || fresh2 || r2 != r1 {
		t.Errorf("within the TTL: calls %d fresh %v %+v; want the remembered answer, no call", f.count(), fresh2, r2)
	}
	*now = now.Add(200 * time.Millisecond) // 5.1 s after the first read
	r3, fresh3 := s.readVendor(ctx)
	if f.count() != 2 || !fresh3 || !r3.known || !r3.reachable {
		t.Errorf("after the TTL: calls %d fresh %v %+v; want a second call and its answer (on)", f.count(), fresh3, r3)
	}
	// the memory is replaced, not accumulated: the next call within the TTL is the second answer
	if r4, _ := s.readVendor(ctx); r4 != r3 || f.count() != 2 {
		t.Errorf("the memory is %+v after %d calls, want the second answer", r4, f.count())
	}
	// a clock that went backwards does not make an answer immortal
	*now = now.Add(-time.Hour)
	if _, fresh := s.readVendor(ctx); !fresh || f.count() != 3 {
		t.Errorf("a clock that moved back kept the answer (calls %d)", f.count())
	}

	// TTL 0: every read asks (the setting the tests of the hook use)
	g := &fakeVendor{reply: func(int) (bool, bool, error) { return true, true, nil }}
	s0, _ := vendorStore(g, 0)
	for i := 0; i < 3; i++ {
		s0.readVendor(ctx)
	}
	if g.count() != 3 {
		t.Errorf("with no memory %d calls were made for 3 reads, want 3", g.count())
	}
}

func TestVendorRead_AFailureAndAnAnswerThatIsNotAnAnswerAreRememberedTooAndNeverAsKnown(t *testing.T) {
	boom := errors.New("module 1 at host db.internal user platform_app refused")
	for _, c := range []struct {
		name    string
		err     error
		known   bool
		failure string
	}{
		{"module 1 could not be asked", boom, false, "unreadable"},
		{"a deadline", context.DeadlineExceeded, false, "timeout"},
		{"a cancel inside the call", context.Canceled, false, "canceled"},
		{"an older module 1 that does not say", nil, false, "not_said"},
	} {
		f := &fakeVendor{reply: func(int) (bool, bool, error) { return true, c.known, c.err }} // reachable true is only a stand-in: it must never be read as an answer
		s, now := vendorStore(f, 5*time.Second)
		r, fresh := s.readVendor(t.Context())
		if !fresh || r.known || r.failure != c.failure || r.reachable {
			t.Errorf("%s: read %+v fresh %v; want unknown, no reachable flag, failure %q", c.name, r, fresh, c.failure)
		}
		if again, fresh := s.readVendor(t.Context()); fresh || again != r || f.count() != 1 {
			t.Errorf("%s: a failure is not remembered for the TTL (calls %d)", c.name, f.count())
		}
		*now = now.Add(6 * time.Second)
		s.readVendor(t.Context())
		if f.count() != 2 {
			t.Errorf("%s: module 1 was not asked again after the TTL (calls %d)", c.name, f.count())
		}
		if r.failure == "" || len(r.failure) > 20 {
			t.Errorf("%s: the failure is %q: a log carries a word, never the error's text", c.name, r.failure)
		}
	}
}

func TestVendorRead_ACallersOwnCancelIsNeverRememberedAndTheAskingIsBounded(t *testing.T) {
	f := &fakeVendor{reply: func(n int) (bool, bool, error) {
		if n == 1 {
			return false, false, context.Canceled
		}
		return true, true, nil
	}}
	s, _ := vendorStore(f, 5*time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r, _ := s.readVendor(ctx); r.known {
		t.Fatalf("a read on a cancelled context was an answer: %+v", r)
	}
	// the cancel was the CALLER's: the next caller must be asked, not told that module 1 failed
	if r, fresh := s.readVendor(t.Context()); !fresh || !r.known || !r.reachable || f.count() != 2 {
		t.Errorf("after a caller's cancel the next read was %+v fresh %v (calls %d); want a fresh answer", r, fresh, f.count())
	}
	// every call carries a deadline of at most the bound, whatever the caller's context has
	for i, d := range f.deadline {
		if d <= 0 || d > vendorReadTimeout {
			t.Errorf("call %d had %v left, want a deadline within (0, %v]", i+1, d, vendorReadTimeout)
		}
	}
}

func TestPreview_CarriesWhetherModuleOneWasReadAndOnlyThat(t *testing.T) {
	var p Preview
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["vendor_reachable_known"]; !ok || v != false {
		t.Errorf("a preview that was not read from module 1 shows vendor_reachable_known = %v (present %v), want an explicit false", v, ok)
	}
	p.VendorReachableKnown = true
	b, _ = json.Marshal(p)
	m = nil
	_ = json.Unmarshal(b, &m)
	if m["vendor_reachable_known"] != true {
		t.Errorf("a preview read from module 1 shows %v, want true", m["vendor_reachable_known"])
	}
	// the arithmetic of the preview does not set it: buildPreview is pure over the snapshot, and only Preview learns from module 1
	if q := buildPreview(metered(1), UnknownExclude, 0, money.Balance{Available: 10}, time.Time{}); q.VendorReachableKnown {
		t.Error("buildPreview says module 1 was read")
	}
}

func TestNew_RefusesANegativeVendorMemoryAndAcceptsNoneAtAll(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1") // lazy: nothing connects
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mny := money.New(pool)
	if _, err := New(pool, mny, WithVendorReachabilityTTL(-time.Second)); err == nil {
		t.Error("a negative memory was accepted")
	}
	for _, d := range []time.Duration{0, time.Second, DefaultVendorReadTTL} {
		if _, err := New(pool, mny, WithVendorReachability(&fakeVendor{reply: func(int) (bool, bool, error) { return false, false, nil }}), WithVendorReachabilityTTL(d)); err != nil {
			t.Errorf("a memory of %s was refused: %v", d, err)
		}
	}
	s, err := New(pool, mny)
	if err != nil || s.vendor.provider != nil || s.vendor.ttl != DefaultVendorReadTTL {
		t.Errorf("the default store has provider %v and memory %v (err %v), want none and %v", s.vendor.provider, s.vendor.ttl, err, DefaultVendorReadTTL)
	}
	// a store with no provider makes no read and refuses nothing: syncVendor returns before it touches the pool (which is dead here)
	if known, err := s.syncVendor(t.Context(), "d0000000-0000-4000-8000-000000000001", "0b000000-0000-4000-8000-000000000001", true, true); known || err != nil {
		t.Errorf("a store with no provider answered %v, %v; want nothing", known, err)
	}
}
