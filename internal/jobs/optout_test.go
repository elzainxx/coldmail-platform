package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The per-user vendor opt-out in the job store (optout.go), the part that needs no database. What it does to the reserve, the snapshot, the preview and the confirm against a real Postgres is proved by
// tests/harness (jobs_vendor_optout_test.go).

const optOutUser = "d0000000-0000-4000-8000-000000000042"

// settingRow is a pgx.Row that answers the setting query: the value, no row, or an error.
type settingRow struct {
	val   bool
	none  bool
	fails error
}

func (r settingRow) Scan(dest ...any) error {
	switch {
	case r.fails != nil:
		return r.fails
	case r.none:
		return pgx.ErrNoRows
	}
	*(dest[0].(*bool)) = r.val
	return nil
}

// settingQ is a querier that holds one user's setting and records every statement it was asked.
type settingQ struct {
	row   settingRow
	calls []string
	args  [][]any
}

func (q *settingQ) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.calls = append(q.calls, sql)
	q.args = append(q.args, args)
	return q.row
}

func (q *settingQ) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("settingQ: Query is not expected")
}

func TestFreezeVendorChoice_AModuleOneJobReadsTheUsersSettingOnceAndNeverTakesTheCallers(t *testing.T) {
	for _, c := range []struct {
		name     string
		row      settingRow
		callers  bool // what the request put in Settings.VendorOptOut
		wantOpt  bool
		wantJSON bool // the key is in the frozen document
	}{
		{"opted out", settingRow{val: true}, false, true, true},
		{"opted in (a row that says false)", settingRow{val: false}, false, false, false},
		{"opted in (no row)", settingRow{none: true}, false, false, false},
		{"the request says opted out, the user is opted in: the request is not believed", settingRow{val: false}, true, false, false},
		{"the request says opted out, the user has no row", settingRow{none: true}, true, false, false},
		{"the request says opted in, the user is opted out: the user's own setting wins", settingRow{val: true}, false, true, true},
	} {
		q := &settingQ{row: c.row}
		b, got, err := freezeVendorChoice(context.Background(), q, optOutUser, ModuleVerify, Settings{UnknownChoice: "", VendorOptOut: c.callers}, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.VendorOptOut != c.wantOpt {
			t.Errorf("%s: the job holds VendorOptOut %v, want %v", c.name, got.VendorOptOut, c.wantOpt)
		}
		if has := strings.Contains(string(b), `"vendor_opt_out":true`); has != c.wantJSON {
			t.Errorf("%s: the frozen document is %s, want the key present = %v", c.name, b, c.wantJSON)
		}
		if strings.Contains(string(b), `"vendor_opt_out":false`) {
			t.Errorf("%s: the frozen document says an explicit false (%s): an opted-in job carries no key, like every job created before the migration", c.name, b)
		}
		if len(q.calls) != 1 || len(q.args[0]) != 1 || q.args[0][0] != optOutUser || !strings.Contains(q.calls[0], "FROM platform.user_settings WHERE user_id = $1") {
			t.Errorf("%s: the setting was read as %v %v, want ONE statement scoped by the user's id", c.name, q.calls, q.args)
		}
	}
}

func TestFreezeVendorChoice_ASendJobNeverReadsTheSettingAndNeverCarriesTheKey(t *testing.T) {
	for _, module := range []string{ModuleSendManaged, ModuleSendBYO} {
		q := &settingQ{row: settingRow{val: true}} // the user IS opted out: a send job must not even look
		content := &Content{Subject: "s", HTML: "<p>h</p>"}
		b, got, err := freezeVendorChoice(context.Background(), q, optOutUser, module, Settings{VendorOptOut: true}, content, nil)
		if err != nil {
			t.Fatalf("%s: %v", module, err)
		}
		if len(q.calls) != 0 {
			t.Errorf("%s: the setting was read (%v): a send job has no vendor", module, q.calls)
		}
		if got.VendorOptOut || strings.Contains(string(b), "vendor_opt_out") {
			t.Errorf("%s: a send job's frozen settings carry the key (%s): the database refuses it (jobs_vendor_opt_out_only_on_module_one)", module, b)
		}
		var f frozenSettings
		if err := json.Unmarshal(b, &f); err != nil || f.Content == nil || f.Content.Subject != "s" {
			t.Errorf("%s: the campaign did not survive the freezing: %+v, %v", module, f, err)
		}
	}
}

func TestFreezeVendorChoice_TheReadFailsClosedAndSaysNothingOfTheUser(t *testing.T) {
	q := &settingQ{row: settingRow{fails: errors.New("the connection broke")}}
	if b, _, err := freezeVendorChoice(context.Background(), q, optOutUser, ModuleVerify, Settings{}, nil, nil); err == nil || b != nil {
		t.Fatalf("a failed read answered %s, %v: a job must never be created with a guessed opt-out", b, err)
	}
	// a user id that is not a customer's is refused before any statement
	q = &settingQ{row: settingRow{val: true}}
	if _, _, err := freezeVendorChoice(context.Background(), q, "not-a-uuid", ModuleVerify, Settings{}, nil, nil); err == nil || len(q.calls) != 0 {
		t.Errorf("a bad user id: %v after %d statement(s), want a refusal and none", err, len(q.calls))
	}
}

func TestSettings_AFrozenDocumentWithoutTheKeyIsOptedInAndTheKeyRoundTrips(t *testing.T) {
	var old frozenSettings
	if err := json.Unmarshal([]byte(`{"settings":{"unknown_choice":""}}`), &old); err != nil || old.Settings.VendorOptOut {
		t.Fatalf("a job created before the migration reads as %+v, %v, want opted in", old, err)
	}
	var in frozenSettings
	if err := json.Unmarshal([]byte(`{"settings":{"unknown_choice":"","vendor_opt_out":true}}`), &in); err != nil || !in.Settings.VendorOptOut {
		t.Fatalf("a frozen opt-out reads as %+v, %v", in, err)
	}
	b, err := json.Marshal(frozenSettings{Settings: Settings{VendorOptOut: true}})
	if err != nil || !strings.Contains(string(b), `"vendor_opt_out":true`) {
		t.Fatalf("the settings marshal as %s, %v", b, err)
	}
	if b, _ := json.Marshal(frozenSettings{}); strings.Contains(string(b), "vendor_opt_out") {
		t.Errorf("opted-in settings marshal with the key: %s", b)
	}
}

// ---- the confirm's check of module 1's opt-out policy ------------------------------------------------------------------------------------------------------------------------------

type fakePolicy struct {
	mu    sync.Mutex
	key   string
	err   error
	calls int
	// seenDeadline is the time left on the last call's context
	seenDeadline time.Duration
}

func (f *fakePolicy) M1OptOutPolicy(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if dl, ok := ctx.Deadline(); ok {
		f.seenDeadline = time.Until(dl)
	} else {
		f.seenDeadline = -1
	}
	return f.key, f.err
}

func (f *fakePolicy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func quietStore(src OptOutPolicySource) *Store {
	return &Store{log: slog.New(slog.NewTextHandler(io.Discard, nil)), vendor: vendorHook{optOut: src}}
}

func TestRequireOptOutPolicy_ConfirmIsRefusedUnlessTheKeyIsKnown(t *testing.T) {
	const jobID = "0b000000-0000-4000-8000-000000000001"
	for _, c := range []struct {
		name string
		src  OptOutPolicySource
		want bool // refused
	}{
		{"no source in this process", nil, true},
		{"module 1 cannot be asked", &fakePolicy{err: errors.New("down")}, true},
		{"module 1 publishes no policy (an older module 1)", &fakePolicy{err: ErrNoOptOutPolicy}, true},
		{"an answer with no key and no error", &fakePolicy{}, true},
		{"the key is known", &fakePolicy{key: "cold-email-no-vendor"}, false},
	} {
		s := quietStore(c.src)
		err := s.requireOptOutPolicy(context.Background(), jobID)
		if c.want {
			var na *NotAllowedError
			if !errors.Is(err, ErrNotAllowed) || !errors.As(err, &na) || na.Reason != ReasonOptOutPolicyUnavailable {
				t.Errorf("%s: %v, want ErrNotAllowed with the reason %s", c.name, err, ReasonOptOutPolicyUnavailable)
			}
		} else if err != nil {
			t.Errorf("%s: refused (%v) although the key is known", c.name, err)
		}
	}
}

func TestRequireOptOutPolicy_TheReadIsBoundedAndACallersCancelIsNotAnAnswerOfModuleOne(t *testing.T) {
	f := &fakePolicy{key: "cold-email-no-vendor"}
	s := quietStore(f)
	if err := s.requireOptOutPolicy(context.Background(), "0b000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if f.seenDeadline <= 0 || f.seenDeadline > vendorReadTimeout {
		t.Errorf("the source was asked with %v left, want a deadline of at most %v: a module that hangs must cost seconds", f.seenDeadline, vendorReadTimeout)
	}
	// the caller went away: that says nothing about module 1 and is not the refusal "module 1's policy is unavailable"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &fakePolicy{err: context.Canceled}
	if err := quietStore(g).requireOptOutPolicy(ctx, "0b000000-0000-4000-8000-000000000001"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotAllowed) {
		t.Errorf("a cancelled caller got %v, want context.Canceled and not a refusal", err)
	}
}

func TestOptOutPolicyConfigured_SaysWhetherTheStoreWasGivenASource(t *testing.T) {
	if quietStore(nil).OptOutPolicyConfigured() {
		t.Error("a store with no source says it has one")
	}
	s := &Store{}
	WithOptOutPolicy(&fakePolicy{})(s)
	if !s.OptOutPolicyConfigured() {
		t.Error("a store given a source says it has none")
	}
	WithOptOutPolicy(nil)(s)
	if s.OptOutPolicyConfigured() {
		t.Error("a nil source did not clear the option")
	}
}

// ---- the reader's memory ----------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestOptOutPolicyReader_KeepsAnAnswerForTheTTLAndNoLonger(t *testing.T) {
	f := &fakePolicy{key: "cold-email-no-vendor"}
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	r := NewOptOutPolicyReader(f, 5*time.Second, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if k, err := r.M1OptOutPolicy(context.Background()); err != nil || k != "cold-email-no-vendor" {
			t.Fatalf("read %d: %q, %v", i, k, err)
		}
	}
	if f.count() != 1 {
		t.Fatalf("three reads inside the memory asked module 1 %d time(s), want once", f.count())
	}
	now = now.Add(4999 * time.Millisecond)
	_, _ = r.M1OptOutPolicy(context.Background())
	if f.count() != 1 {
		t.Errorf("a read 4.999 s later asked again (%d calls)", f.count())
	}
	now = now.Add(2 * time.Millisecond) // 5.001 s since the answer
	_, _ = r.M1OptOutPolicy(context.Background())
	if f.count() != 2 {
		t.Errorf("a read after the memory ran out made %d calls in all, want 2", f.count())
	}
	// the control: a reader with a memory of 0 asks every time
	g := &fakePolicy{key: "k"}
	z := NewOptOutPolicyReader(g, 0, func() time.Time { return now })
	_, _ = z.M1OptOutPolicy(context.Background())
	_, _ = z.M1OptOutPolicy(context.Background())
	if g.count() != 2 {
		t.Errorf("a reader with no memory asked %d time(s), want 2", g.count())
	}
}

func TestOptOutPolicyReader_NeverMakesAKeyOutOfNothingAndRemembersAFailureToo(t *testing.T) {
	f := &fakePolicy{} // ("", nil): the source answered and said nothing
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	r := NewOptOutPolicyReader(f, 5*time.Second, func() time.Time { return now })
	if k, err := r.M1OptOutPolicy(context.Background()); k != "" || !errors.Is(err, ErrNoOptOutPolicy) {
		t.Fatalf("an empty answer read as %q, %v, want no key and ErrNoOptOutPolicy", k, err)
	}
	// the failure is kept for the memory (a burst of confirms during an outage costs one read), and a later answer is believed once it has run out
	_, _ = r.M1OptOutPolicy(context.Background())
	if f.count() != 1 {
		t.Errorf("a failure was not kept: %d calls", f.count())
	}
	f.mu.Lock()
	f.key = "cold-email-no-vendor"
	f.mu.Unlock()
	now = now.Add(6 * time.Second)
	if k, err := r.M1OptOutPolicy(context.Background()); err != nil || k != "cold-email-no-vendor" {
		t.Errorf("after the memory ran out the reader gave %q, %v, want the new key", k, err)
	}
	// a source error comes back as it is (the key is "" with it, whatever the source returned beside it)
	e := &fakePolicy{key: "stale-key", err: errors.New("down")}
	if k, err := NewOptOutPolicyReader(e, 0, nil).M1OptOutPolicy(context.Background()); k != "" || err == nil {
		t.Errorf("an error came back as %q, %v: a key must never travel with an error", k, err)
	}
	// no source at all
	if k, err := NewOptOutPolicyReader(nil, time.Second, nil).M1OptOutPolicy(context.Background()); k != "" || !errors.Is(err, ErrNoOptOutPolicy) {
		t.Errorf("a reader with no source gave %q, %v", k, err)
	}
	var nilReader *OptOutPolicyReader
	if k, err := nilReader.M1OptOutPolicy(context.Background()); k != "" || !errors.Is(err, ErrNoOptOutPolicy) {
		t.Errorf("a nil reader gave %q, %v", k, err)
	}
}

func TestOptOutPolicyReader_AnAnswerCutOffByTheCallersContextIsNeverRemembered(t *testing.T) {
	f := &fakePolicy{err: context.Canceled}
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	r := NewOptOutPolicyReader(f, time.Minute, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.M1OptOutPolicy(ctx); err == nil {
		t.Fatal("a cancelled read returned no error")
	}
	f.mu.Lock()
	f.err, f.key = nil, "cold-email-no-vendor"
	f.mu.Unlock()
	// the next caller's own context is alive: it must ask, not be handed the cancellation of somebody else
	if k, err := r.M1OptOutPolicy(context.Background()); err != nil || k != "cold-email-no-vendor" || f.count() != 2 {
		t.Errorf("the next read gave %q, %v after %d call(s), want the key after a second call", k, err, f.count())
	}
}
