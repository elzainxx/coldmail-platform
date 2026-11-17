package entitlements

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// These tests need no database: they cover what the store decides BEFORE it asks one. The Store here has no
// pool, so a request that passes validation reaches the pool check and returns errNoPool: that distinct error is
// the paired control of every refusal below (a request refused for the named reason never got that far, and a
// valid request always does).

const (
	userA  = "11111111-1111-1111-1111-111111111111"
	adminA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func goodEnable() EnableReq {
	return EnableReq{UserID: userA, AdminID: adminA, Reason: "support ticket 4411", Key: "k-1"}
}

func goodDisable() DisableReq {
	return DisableReq{UserID: userA, AdminID: adminA, Reason: "abuse report", Key: "k-2"}
}

func TestStore_ValidationRefusesBeforeTheDatabase(t *testing.T) {
	s := &Store{} // no pool: anything that reaches it fails with errNoPool
	until := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	long := strings.Repeat("r", MaxReasonBytes+1)

	type try func() error
	enable := func(mut func(*EnableReq)) try {
		return func() error {
			r := goodEnable()
			mut(&r)
			_, err := s.Enable(context.Background(), r)
			return err
		}
	}
	disable := func(mut func(*DisableReq)) try {
		return func() error {
			r := goodDisable()
			mut(&r)
			_, err := s.Disable(context.Background(), r)
			return err
		}
	}

	refused := map[string]struct {
		do   try
		want []error // every one must match with errors.Is
	}{
		"enable: user not a uuid":           {enable(func(r *EnableReq) { r.UserID = "not-a-uuid" }), []error{ErrInvalid}},
		"enable: user id with a suffix":     {enable(func(r *EnableReq) { r.UserID = userA + "x" }), []error{ErrInvalid}},
		"enable: empty user":                {enable(func(r *EnableReq) { r.UserID = "" }), []error{ErrInvalid}},
		"enable: the system user":           {enable(func(r *EnableReq) { r.UserID = zeroUUID }), []error{ErrInvalid, ErrSystemUser}},
		"enable: admin not a uuid":          {enable(func(r *EnableReq) { r.AdminID = "root" }), []error{ErrInvalid}},
		"enable: the system user as admin":  {enable(func(r *EnableReq) { r.AdminID = zeroUUID }), []error{ErrInvalid}},
		"enable: blank key":                 {enable(func(r *EnableReq) { r.Key = "   " }), []error{ErrInvalid}},
		"enable: empty key":                 {enable(func(r *EnableReq) { r.Key = "" }), []error{ErrInvalid}},
		"enable: key too long":              {enable(func(r *EnableReq) { r.Key = strings.Repeat("k", MaxKeyBytes+1) }), []error{ErrInvalid}},
		"enable: key with a NUL":            {enable(func(r *EnableReq) { r.Key = "k\x00" }), []error{ErrInvalid}},
		"enable: reason one byte too long":  {enable(func(r *EnableReq) { r.Reason = long }), []error{ErrInvalid, ErrReasonTooLong}},
		"enable: reason with a NUL":         {enable(func(r *EnableReq) { r.Reason = "a\x00b" }), []error{ErrInvalid}},
		"enable: reason not UTF-8":          {enable(func(r *EnableReq) { r.Reason = "a\xffb" }), []error{ErrInvalid}},
		"enable: the zero time as the date": {enable(func(r *EnableReq) { z := time.Time{}; r.Until = &z }), []error{ErrInvalid}},
		"disable: user not a uuid":          {disable(func(r *DisableReq) { r.UserID = "nope" }), []error{ErrInvalid}},
		"disable: the system user":          {disable(func(r *DisableReq) { r.UserID = zeroUUID }), []error{ErrInvalid, ErrSystemUser}},
		"disable: admin not a uuid":         {disable(func(r *DisableReq) { r.AdminID = "" }), []error{ErrInvalid}},
		"disable: blank key":                {disable(func(r *DisableReq) { r.Key = "\t" }), []error{ErrInvalid}},
		"disable: reason one byte too long": {disable(func(r *DisableReq) { r.Reason = long }), []error{ErrInvalid, ErrReasonTooLong}},
		"disable: the system user as admin": {disable(func(r *DisableReq) { r.AdminID = zeroUUID }), []error{ErrInvalid}},
		"expire: a limit of zero":           {func() error { _, err := s.ExpireDue(context.Background(), 0); return err }, []error{ErrInvalid}},
		"expire: a negative limit":          {func() error { _, err := s.ExpireDueReport(context.Background(), -3); return err }, []error{ErrInvalid}},
		"get: user not a uuid":              {func() error { _, err := s.Get(context.Background(), "x"); return err }, []error{ErrInvalid}},
		"sync: user not a uuid":             {func() error { _, err := s.Sync(context.Background(), "x"); return err }, []error{ErrInvalid}},
	}
	if len(refused) < 20 {
		t.Fatalf("only %d refusal cases: the table shrank", len(refused))
	}
	for name, tc := range refused {
		err := tc.do()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if errors.Is(err, errNoPool) {
			t.Errorf("%s: reached the database layer instead of being refused first: %v", name, err)
		}
		for _, want := range tc.want {
			if !errors.Is(err, want) {
				t.Errorf("%s: error %q does not match %v", name, err, want)
			}
		}
	}
	// The refused system user is not "invalid" by accident: a request that merely has a bad id is NOT ErrSystemUser.
	if _, err := s.Enable(context.Background(), EnableReq{UserID: "x", AdminID: adminA, Key: "k"}); errors.Is(err, ErrSystemUser) {
		t.Error("a malformed id was reported as the system user")
	}

	// Paired controls: each of the same requests, made valid, passes validation (and stops at the pool check).
	accepted := map[string]try{
		"enable":             enable(func(*EnableReq) {}),
		"enable with a date": enable(func(r *EnableReq) { r.Until = &until }),
		"enable with an upper-case uuid": enable(func(r *EnableReq) {
			r.UserID, r.AdminID = strings.ToUpper("abcdefab-abcd-abcd-abcd-abcdefabcdef"), strings.ToUpper(adminA)
		}),
		"enable with a reason of exactly the limit": enable(func(r *EnableReq) { r.Reason = strings.Repeat("r", MaxReasonBytes) }),
		"enable with no reason":                     enable(func(r *EnableReq) { r.Reason = "" }),
		"enable with a key of exactly the limit":    enable(func(r *EnableReq) { r.Key = strings.Repeat("k", MaxKeyBytes) }),
		"enable with a multi-byte reason":           enable(func(r *EnableReq) { r.Reason = "résumé 请求" }),
		"disable":                                   disable(func(*DisableReq) {}),
		"expire":                                    func() error { _, err := s.ExpireDue(context.Background(), 1); return err },
		"get":                                       func() error { _, err := s.Get(context.Background(), userA); return err },
		"sync":                                      func() error { _, err := s.Sync(context.Background(), userA); return err },
	}
	for name, do := range accepted {
		if err := do(); !errors.Is(err, errNoPool) {
			t.Errorf("control %s: got %v, want it to pass validation and stop at the pool (errNoPool)", name, err)
		}
	}
}

func TestNormalise_CanonicalForm(t *testing.T) {
	d := time.Date(2031, 5, 6, 7, 8, 9, 123_456_789, time.FixedZone("x", 5*3600))
	r, err := normalise("m3_enable", strings.ToUpper(userA), strings.ToUpper(adminA), "  padded reason \n", "key-1", &d)
	if err != nil {
		t.Fatal(err)
	}
	if r.user != userA || r.admin != adminA {
		t.Errorf("ids not canonicalised to lower case: %q %q", r.user, r.admin)
	}
	if r.reason != "padded reason" {
		t.Errorf("reason = %q, want it trimmed", r.reason)
	}
	if r.until == nil || r.until.Location() != time.UTC || r.until.Nanosecond() != 123_456_000 {
		t.Errorf("until = %v, want UTC truncated to the microsecond PostgreSQL stores", r.until)
	}
	if !r.until.Equal(d.Truncate(time.Microsecond)) {
		t.Errorf("truncation changed the instant: %v vs %v", r.until, d)
	}
	// The caller's value is not modified.
	if d.Nanosecond() != 123_456_789 {
		t.Error("the caller's time was modified")
	}
	// Disable carries no date at all.
	if r2, err := normalise("m3_disable", userA, adminA, "", "k", nil); err != nil || r2.until != nil || r2.reason != "" {
		t.Errorf("disable normalised to %+v (%v)", r2, err)
	}
}

func TestMapWriteError_ByConstraintNameNeverByMessage(t *testing.T) {
	pg := func(code, constraint, msg string) error {
		return &pgconn.PgError{Code: code, ConstraintName: constraint, Message: msg}
	}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"disabled admin", pg("23514", "admin_is_active", "unrelated words"), ErrAdminDisabled},
		{"unknown admin", pg("23503", "ledger_acting_admin_id_fkey", "unrelated words"), ErrUnknownAdmin},
		{"unknown user", pg("23503", "ledger_user_id_fkey", "unrelated words"), ErrUnknownUser},
		{"unknown admin, raised by the entitlement row", pg("23503", "m3_entitlements_changed_by_fkey", "unrelated words"), ErrUnknownAdmin},
		{"unknown user, raised by the entitlement row", pg("23503", "m3_entitlements_user_id_fkey", "unrelated words"), ErrUnknownUser},
		{"system user", pg("23514", "ledger_not_the_system_user", "unrelated words"), ErrSystemUser},
		{"date not in the future", pg("23514", "ledger_until_at_shape", "unrelated words"), ErrUntilNotFuture},
	}
	for _, tc := range cases {
		if got := mapWriteError(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("%s: mapped to %v, want %v", tc.name, got, tc.want)
		}
		// The original error is kept reachable.
		var pe *pgconn.PgError
		if got := mapWriteError(tc.err); !errors.As(got, &pe) {
			t.Errorf("%s: the database error is no longer reachable from %v", tc.name, got)
		}
	}
	// Paired controls: the message text that LOOKS like each refusal does not map it when the constraint is another;
	// and the right constraint under the wrong SQLSTATE does not map either.
	look := "a disabled admin can no longer act: admin_is_active"
	for name, err := range map[string]error{
		"another constraint, the disabled-admin wording": pg("23514", "some_other_check", look),
		"no constraint, the disabled-admin wording":      pg("23514", "", look),
		"the right name under another SQLSTATE":          pg("23505", "admin_is_active", look),
		"a plain error that mentions it":                 errors.New(look),
	} {
		if got := mapWriteError(err); errors.Is(got, ErrAdminDisabled) {
			t.Errorf("%s was mapped to ErrAdminDisabled: the mapping reads message text", name)
		}
	}
	// What is not known is wrapped, never swallowed.
	other := pg("40001", "", "serialization failure")
	if got := mapWriteError(other); !errors.Is(got, other) {
		t.Errorf("an unknown refusal lost its cause: %v", got)
	}
}

func TestIsConstraint_RequiresBothTheStateAndTheName(t *testing.T) {
	err := &pgconn.PgError{Code: "23505", ConstraintName: "ledger_idempotency_key"}
	if !isConstraint(err, "23505", "ledger_idempotency_key") {
		t.Error("the exact state and name were not recognised")
	}
	if isConstraint(err, "23505", "ledger_other_key") || isConstraint(err, "23514", "ledger_idempotency_key") || isConstraint(errors.New("x"), "23505", "ledger_idempotency_key") {
		t.Error("a different name, a different state, or a non-database error was recognised")
	}
	if !isConstraint(errors.Join(errors.New("a"), err), "23505", "ledger_idempotency_key") {
		t.Error("a joined error hides the database error")
	}
}

func TestEntry_SameRequestComparesEveryParameter(t *testing.T) {
	at := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	admin := adminA
	other := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	base := Entry{Kind: "m3_enable", AdminID: &admin, Until: &at, Reason: "why"}
	req := request{kind: "m3_enable", admin: adminA, until: &at, reason: "why"}
	if !base.sameRequest(req) {
		t.Fatal("control failed: identical parameters are not the same request")
	}
	later := at.Add(time.Microsecond)
	same := at // a different pointer, the same instant
	for name, mutate := range map[string]func(*Entry, *request){
		"another kind":    func(e *Entry, _ *request) { e.Kind = "m3_disable" },
		"another admin":   func(e *Entry, _ *request) { e.AdminID = &other },
		"no admin":        func(e *Entry, _ *request) { e.AdminID = nil },
		"another date":    func(_ *Entry, r *request) { r.until = &later },
		"date vs no date": func(_ *Entry, r *request) { r.until = nil },
		"no date vs date": func(e *Entry, _ *request) { e.Until = nil },
		"another reason":  func(e *Entry, _ *request) { e.Reason = "other" },
		"reason vs none":  func(_ *Entry, r *request) { r.reason = "" },
	} {
		e, r := base, req
		mutate(&e, &r)
		if e.sameRequest(r) {
			t.Errorf("%s: judged the same request: a different request would be replayed silently", name)
		}
	}
	e, r := base, req
	r.until = &same
	if !e.sameRequest(r) {
		t.Error("the same instant behind another pointer is a different request")
	}
}

func TestExpiryKey_IsDerivedFromTheUserAndTheDateOnly(t *testing.T) {
	d := time.Date(2031, 1, 2, 3, 4, 5, 678_901_000, time.UTC)
	if got, want := expiryKey(userA, d), "m3-expire:"+userA+":1925089445678901"; got != want {
		t.Errorf("expiryKey = %q, want %q", got, want)
	}
	// The same instant in another zone is the same key; one microsecond later is another.
	if expiryKey(userA, d.In(time.FixedZone("x", -7*3600))) != expiryKey(userA, d) {
		t.Error("the key depends on the zone the time is held in")
	}
	if expiryKey(userA, d.Add(time.Microsecond)) == expiryKey(userA, d) {
		t.Error("two different dates share a key: a re-dated entitlement could never expire again")
	}
	if expiryKey(userA, d) == expiryKey("22222222-2222-2222-2222-222222222222", d) {
		t.Error("two users share a key")
	}
	if len(expiryKey(userA, d)) > MaxKeyBytes {
		t.Error("the expiry key is longer than the key limit")
	}
}

func TestSyncState_Converged(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   SyncState
		want bool
	}{
		{"suspended and seen suspended", SyncState{Exists: true, WantSuspended: true, WantVersion: 3, ConvergedVersion: 3, Observed: "suspended", ObservedVersion: i64(3)}, true},
		{"active and seen active", SyncState{Exists: true, WantSuspended: false, WantVersion: 1, ConvergedVersion: 1, Observed: "active", ObservedVersion: i64(1)}, true},
		{"never converged", SyncState{Exists: true, WantSuspended: true, WantVersion: 1, ConvergedVersion: 0, Observed: "unknown"}, false},
		{"converged an older version", SyncState{Exists: true, WantSuspended: true, WantVersion: 4, ConvergedVersion: 3, Observed: "suspended", ObservedVersion: i64(3)}, false},
		{"versions equal but observed the other state", SyncState{Exists: true, WantSuspended: true, WantVersion: 2, ConvergedVersion: 2, Observed: "active", ObservedVersion: i64(2)}, false},
		{"versions equal but never observed", SyncState{Exists: true, WantSuspended: false, WantVersion: 2, ConvergedVersion: 2, Observed: "unknown"}, false},
		// D5: the observation must be of THIS version. Each of these would have been Converged() before the observed version existed.
		{"the right state observed at an OLDER version (a stale observation)", SyncState{Exists: true, WantSuspended: false, WantVersion: 3, ConvergedVersion: 3, Observed: "active", ObservedVersion: i64(1)}, false},
		{"the right state with no observed version at all", SyncState{Exists: true, WantSuspended: true, WantVersion: 2, ConvergedVersion: 2, Observed: "suspended"}, false},
		{"the right state observed at a version AHEAD of the wanted one", SyncState{Exists: true, WantSuspended: true, WantVersion: 2, ConvergedVersion: 2, Observed: "suspended", ObservedVersion: i64(3)}, false},
		{"no row", SyncState{}, false},
	} {
		if got := tc.st.Converged(); got != tc.want {
			t.Errorf("%s: Converged() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// i64 is the address of a version, for the SyncState fixtures.
func i64(n int64) *int64 { return &n }
