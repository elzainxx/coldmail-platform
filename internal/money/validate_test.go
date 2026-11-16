package money

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
)

const (
	vUser  = "11111111-1111-4111-8111-111111111111"
	vAdmin = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	vJob   = "0b000000-0000-4000-8000-000000000001"
)

func TestIsCanonicalUUID(t *testing.T) {
	good := []string{vUser, vAdmin, vJob, "00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	for _, s := range good {
		if !isCanonicalUUID(s) {
			t.Errorf("%q must be canonical", s)
		}
	}
	bad := []string{
		"", "x", strings.ToUpper(vAdmin), // upper case is a second spelling of the same uuid and would hash to a second lock
		"11111111111141118111111111111111", // no hyphens
		"{11111111-1111-4111-8111-111111111111}",
		"11111111-1111-4111-8111-11111111111",   // one short
		"11111111-1111-4111-8111-1111111111111", // one long
		"1111111g-1111-4111-8111-111111111111",  // not hex
		"11111111_1111-4111-8111-111111111111",  // wrong separator
		" 11111111-1111-4111-8111-111111111111",
	}
	for _, s := range bad {
		if isCanonicalUUID(s) {
			t.Errorf("%q must not be canonical", s)
		}
	}
}

// field extracts the field of an InvalidError, or "" when err is not one.
func field(err error) string {
	var ie *InvalidError
	if errors.As(err, &ie) {
		return ie.Field
	}
	return ""
}

func TestValidateEntry_EveryBadFieldIsNamed(t *testing.T) {
	ok := func() (string, string, int64, string, string, string) {
		return vUser, "m1", 10, vAdmin, "a reason", "key-1"
	}
	check := func(user, module string, amount int64, admin, reason, key string) error {
		_, err := validateEntry("grant", user, module, amount, admin, reason, key)
		return err
	}
	{ // the control: the legal request passes, and its reason is stored trimmed
		u, m, a, ad, r, k := ok()
		got, err := validateEntry("grant", u, m, a, ad, "  "+r+" \n", k)
		if err != nil || got.reason != r {
			t.Fatalf("the legal request was refused or not trimmed: %+v, %v", got, err)
		}
	}
	cases := []struct {
		name  string
		err   error
		field string
	}{
		{"blank user", check("", "m1", 10, vAdmin, "r", "k"), "user_id"},
		{"upper-case user", check(strings.ToUpper(vAdmin), "m1", 10, vAdmin, "r", "k"), "user_id"},
		{"system user", check(SystemUser, "m1", 10, vAdmin, "r", "k"), "user_id"},
		{"unknown module", check(vUser, "m9", 10, vAdmin, "r", "k"), "module"},
		{"blank module", check(vUser, "", 10, vAdmin, "r", "k"), "module"},
		{"upper-case module", check(vUser, "M1", 10, vAdmin, "r", "k"), "module"},
		{"zero amount", check(vUser, "m1", 0, vAdmin, "r", "k"), "amount"},
		{"negative amount", check(vUser, "m1", -1, vAdmin, "r", "k"), "amount"},
		{"amount above 2^53", check(vUser, "m1", MaxAmount+1, vAdmin, "r", "k"), "amount"},
		{"max int64 amount", check(vUser, "m1", math.MaxInt64, vAdmin, "r", "k"), "amount"},
		{"bad admin", check(vUser, "m1", 10, "not-a-uuid", "r", "k"), "admin_id"},
		{"blank reason", check(vUser, "m1", 10, vAdmin, " \t\n ", "k"), "reason"},
		{"non-breaking-space reason", check(vUser, "m1", 10, vAdmin, "\u00a0\u00a0", "k"), "reason"},
		{"reason of 1001 bytes", check(vUser, "m1", 10, vAdmin, strings.Repeat("r", 1001), "k"), "reason"},
		{"reason with invalid UTF-8", check(vUser, "m1", 10, vAdmin, "bad \xff byte", "k"), "reason"},
		{"reason with NUL", check(vUser, "m1", 10, vAdmin, "a\x00b", "k"), "reason"},
		{"blank key", check(vUser, "m1", 10, vAdmin, "r", "  "), "key"},
		{"key of 201 bytes", check(vUser, "m1", 10, vAdmin, "r", strings.Repeat("k", 201)), "key"},
		{"key with NUL", check(vUser, "m1", 10, vAdmin, "r", "a\x00"), "key"},
		{"key in the reserve shape", check(vUser, "m1", 10, vAdmin, "r", "reserve:"+vJob), "key"},
		{"key in the charge shape", check(vUser, "m1", 10, vAdmin, "r", "charge:"+vJob+":x"), "key"},
		{"key in the release shape", check(vUser, "m1", 10, vAdmin, "r", "release:7"), "key"},
	}
	for _, c := range cases {
		if c.err == nil {
			t.Errorf("%s: was accepted", c.name)
			continue
		}
		if !errors.Is(c.err, ErrInvalid) || field(c.err) != c.field {
			t.Errorf("%s: refused with %v, want an InvalidError on field %q", c.name, c.err, c.field)
		}
	}
	// module 3 is a real module that holds no credits: its own error
	if err := check(vUser, "m3", 10, vAdmin, "r", "k"); !errors.Is(err, ErrNotMetered) || errors.Is(err, ErrInvalid) {
		t.Errorf("m3: got %v, want ErrNotMetered and not ErrInvalid", err)
	}
}

func TestValidateEntry_TheLimitsAreInclusive(t *testing.T) {
	for _, c := range []struct {
		name   string
		amount int64
		reason string
		key    string
	}{
		{"amount 1", 1, "r", "k"},
		{"amount 2^53", MaxAmount, "r", "k"},
		{"reason of exactly 1000 bytes", 5, strings.Repeat("r", MaxReasonBytes), "k"},
		{"key of exactly 200 bytes", 5, "r", strings.Repeat("k", MaxKeyBytes)},
		{"a key that merely contains a reserved word", 5, "r", "my-reserve:1"},
		{"multi-byte text", 5, "résumé — ok", "clé-1"},
	} {
		if _, err := validateEntry("adjustment", vUser, "m2", c.amount, vAdmin, c.reason, c.key); err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
	}
}

func TestValidateReserve(t *testing.T) {
	if err := validateReserve(ReserveReq{JobID: vJob, Units: 1}); err != nil {
		t.Fatalf("the legal request was refused: %v", err)
	}
	if err := validateReserve(ReserveReq{JobID: vJob, Units: math.MaxInt32}); err != nil {
		t.Fatalf("the largest legal request was refused: %v", err)
	}
	for _, c := range []struct {
		name  string
		req   ReserveReq
		field string
	}{
		{"zero units", ReserveReq{JobID: vJob, Units: 0}, "units"},
		{"negative units", ReserveReq{JobID: vJob, Units: -4}, "units"},
		{"units above int32", ReserveReq{JobID: vJob, Units: math.MaxInt32 + 1}, "units"},
		{"blank job", ReserveReq{JobID: "", Units: 1}, "job_id"},
		{"upper-case job", ReserveReq{JobID: strings.ToUpper(vJob), Units: 1}, "job_id"},
	} {
		if err := validateReserve(c.req); !errors.Is(err, ErrInvalid) || field(err) != c.field {
			t.Errorf("%s: got %v, want an InvalidError on %q", c.name, err, c.field)
		}
	}
	if err := validateRelease(vJob); err != nil {
		t.Fatalf("release of a legal job id refused: %v", err)
	}
	if err := validateRelease("x"); field(err) != "job_id" {
		t.Fatalf("release of a bad job id: %v", err)
	}
}

func TestValidateSettle_ItemsAreCheckedAndUnique(t *testing.T) {
	item := func(ref string) SettleItem {
		return SettleItem{ItemRef: ref, Outcome: charge.Outcome{Module: "m2", State: "sent"}}
	}
	if err := validateSettle(SettleReq{JobID: vJob, Items: []SettleItem{item("a"), item("b"), item(strings.Repeat("x", MaxItemRefBytes))}}); err != nil {
		t.Fatalf("legal items were refused: %v", err)
	}
	if err := validateSettle(SettleReq{JobID: vJob}); err != nil {
		t.Fatalf("an empty page is legal: %v", err)
	}
	for _, c := range []struct {
		name  string
		items []SettleItem
		want  string
	}{
		{"blank ref", []SettleItem{item("a"), item("  ")}, "blank"},
		{"empty ref", []SettleItem{item("")}, "blank"},
		{"ref of 201 bytes", []SettleItem{item(strings.Repeat("x", MaxItemRefBytes+1))}, "201 bytes"},
		{"repeated ref", []SettleItem{item("a"), item("b"), item("a")}, "repeats"},
		{"NUL in ref", []SettleItem{item("a\x00")}, "NUL"},
		{"invalid UTF-8 ref", []SettleItem{item("\xff")}, "UTF-8"},
	} {
		err := validateSettle(SettleReq{JobID: vJob, Items: c.items})
		if !errors.Is(err, ErrBadItem) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want ErrBadItem mentioning %q", c.name, err, c.want)
		}
	}
	if err := validateSettle(SettleReq{JobID: "nope", Items: []SettleItem{item("a")}}); field(err) != "job_id" {
		t.Errorf("a bad job id: %v", err)
	}
	many := make([]SettleItem, MaxSettleItems+1)
	for i := range many {
		many[i] = item(fmt.Sprintf("item-%d", i))
	}
	if err := validateSettle(SettleReq{JobID: vJob, Items: many}); field(err) != "items" {
		t.Errorf("%d items in one call: got %v, want an InvalidError on items", len(many), err)
	}
	if err := validateSettle(SettleReq{JobID: vJob, Items: many[:MaxSettleItems]}); err != nil {
		t.Errorf("exactly %d items (the control): refused: %v", MaxSettleItems, err)
	}
}

func TestValidateBalance(t *testing.T) {
	if err := validateBalance(vUser, "m2"); err != nil {
		t.Fatal(err)
	}
	if err := validateBalance(vUser, "m3"); !errors.Is(err, ErrNotMetered) {
		t.Errorf("m3: %v", err)
	}
	if err := validateBalance("x", "m1"); field(err) != "user_id" {
		t.Errorf("bad user: %v", err)
	}
}

func TestDearest_HighestPriceThenLexicographicallyFirstUnit(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []price
		want string
	}{
		{"one dearest", []price{{"m1_cache", 1}, {"m1_own", 1}, {"m1_vendor", 2}}, "m1_vendor"},
		{"a tie goes to the first unit", []price{{"m2_bounced", 1}, {"m2_accepted", 1}}, "m2_accepted"},
		{"a tie, the other order", []price{{"m2_accepted", 1}, {"m2_bounced", 1}}, "m2_accepted"},
		{"a dearer later row wins a tie earlier", []price{{"a", 3}, {"b", 3}, {"c", 4}}, "c"},
		{"one row", []price{{"only", 9}}, "only"},
	} {
		if got := dearest(c.in); got.unit != c.want {
			t.Errorf("%s: dearest = %s, want %s", c.name, got.unit, c.want)
		}
	}
	if !samePrices([]price{{"a", 1}, {"b", 2}}, []price{{"b", 2}, {"a", 1}}) {
		t.Error("samePrices must ignore order")
	}
	if samePrices([]price{{"a", 1}, {"b", 2}}, []price{{"a", 1}, {"b", 3}}) || samePrices([]price{{"a", 1}}, []price{{"a", 1}, {"b", 2}}) || samePrices([]price{{"a", 1}}, []price{{"c", 1}}) {
		t.Error("samePrices must see a changed price, a missing unit and a different unit")
	}
}
