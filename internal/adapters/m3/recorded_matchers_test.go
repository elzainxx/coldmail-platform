package m3_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// readFixture is the recorded (or hand-written) body of a fixture file.
func readFixture(t testing.TB, name string) []byte { return sendapitest.Read(t, fixtures, name) }

// optInt is an optional integer of a recording: nil when the key is absent or null.
func optInt(d any, path string) *int64 {
	v, ok := lookup(d, path)
	if !ok || v == nil {
		return nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	n := int64(f)
	return &n
}

func samePtrInt(t testing.TB, what string, got, want *int64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Fatalf("%s: got %v, the recording says %v", what, got, want)
	case *got != *want:
		t.Fatalf("%s: got %d, the recording says %d", what, *got, *want)
	}
}

// accountPageMatches holds an AccountPage to the listing it was decoded from: every account, in order, and the cursor (empty when the recording's is null).
func accountPageMatches(t testing.TB, p m3.AccountPage, d any) {
	t.Helper()
	same(t, "items", len(p.Items), length(t, d, "items"))
	if p.Items == nil {
		t.Fatal("a page with no accounts is an empty page, not a missing one")
	}
	next := ""
	if n := optStr(d, "next_cursor"); n != nil {
		next = *n
	}
	same(t, "next_cursor", p.NextCursor, next)
	for i, a := range p.Items {
		accountMatches(t, a, must(t, d, fmt.Sprintf("items[%d]", i)))
	}
}

// feasibilityMatches holds a Feasibility to the quote it was decoded from: the inert price, the verdict, the reasons, the caps, the capacity and the warnings.
func feasibilityMatches(t testing.TB, f m3.Feasibility, d any) {
	t.Helper()
	same(t, "recipient_count", f.RecipientCount, asInt(t, d, "recipient_count"))
	same(t, "cost", f.Cost, asInt(t, d, "cost"))
	same(t, "unit_price_credits", f.UnitPriceCredits, asInt(t, d, "unit_price_credits"))
	same(t, "currency", f.Currency, asStr(t, d, "currency"))
	same(t, "tier", f.Tier, asStr(t, d, "tier"))
	same(t, "feasible", f.Feasible, must(t, d, "feasible").(bool))
	same(t, "reasons", len(f.Reasons), length(t, d, "reasons"))
	for i, r := range f.Reasons {
		base := fmt.Sprintf("reasons[%d].", i)
		same(t, base+"code", string(r.Code), mapped(asStr(t, d, base+"code"), vocabSet(t, "quote_reason_code")))
		samePtrInt(t, base+"limit", r.Limit, optInt(d, base+"limit"))
		samePtrInt(t, base+"requested", r.Requested, optInt(d, base+"requested"))
		samePtrInt(t, base+"current", r.Current, optInt(d, base+"current"))
		cause := ""
		if c := optStr(d, base+"cause"); c != nil {
			cause = *c
		}
		same(t, base+"cause", string(r.Cause), cause)
		cap := ""
		if c := optStr(d, base+"cap_name"); c != nil {
			cap = *c
		}
		same(t, base+"cap_name", r.CapName, cap)
	}
	same(t, "caps.queue_depth_cap", f.Caps.QueueDepthCap, asInt(t, d, "caps.queue_depth_cap"))
	same(t, "caps.daily_send_cap", f.Caps.DailySendCap, asInt(t, d, "caps.daily_send_cap"))
	same(t, "caps.account_count_cap", f.Caps.AccountCountCap, asInt(t, d, "caps.account_count_cap"))
	same(t, "caps.campaign_size_cap", f.Caps.CampaignSizeCap, asInt(t, d, "caps.campaign_size_cap"))
	same(t, "capacity.eligible_accounts", f.Capacity.EligibleAccounts, asInt(t, d, "capacity.eligible_accounts"))
	same(t, "capacity.daily_capacity_remaining", f.Capacity.DailyCapacityRemaining, asInt(t, d, "capacity.daily_capacity_remaining"))
	same(t, "capacity.effective_concurrency", f.Capacity.EffectiveConcurrency, asInt(t, d, "capacity.effective_concurrency"))
	samePtrInt(t, "capacity.estimated_duration_sec", f.Capacity.EstimatedDurationSec, optInt(d, "capacity.estimated_duration_sec"))
	basis := ""
	if b := optStr(d, "capacity.estimate_basis"); b != nil {
		basis = *b
	}
	same(t, "capacity.estimate_basis", f.Capacity.EstimateBasis, basis)
	same(t, "warnings", len(f.Warnings), length(t, d, "warnings"))
	for i, w := range f.Warnings {
		base := fmt.Sprintf("warnings[%d].", i)
		same(t, base+"code", w.Code, asStr(t, d, base+"code"))
		same(t, base+"estimated_duration_sec", w.EstimatedDurationSec, asInt(t, d, base+"estimated_duration_sec"))
		same(t, base+"threshold_sec", w.ThresholdSec, asInt(t, d, base+"threshold_sec"))
	}
}

// eventMatches holds an AccountEvent to the event object it was decoded from (d is one element of a page's items).
func eventMatches(t testing.TB, e m3.AccountEvent, d any) {
	t.Helper()
	same(t, "id", e.ID, asStr(t, d, "id"))
	same(t, "account_id", e.AccountID, asStr(t, d, "account_id"))
	same(t, "actor", e.Actor, asStr(t, d, "actor"))
	same(t, "action", e.Action, asStr(t, d, "action"))
	flagsMatch(t, "from_flags", e.From, d, "from_flags")
	flagsMatch(t, "to_flags", e.To, d, "to_flags")
	samePtr(t, "reason", e.Reason, optStr(d, "reason"))
	rid := ""
	if r := optStr(d, "request_id"); r != nil {
		rid = *r
	}
	same(t, "request_id", e.RequestID, rid)
	sameTime(t, "created_at", &e.CreatedAt, optTime(t, d, "created_at"))
}

// flagsMatch holds the flags of an event to the object (or the null) at path of the document.
func flagsMatch(t testing.TB, what string, got *m3.Flags, d any, path string) {
	t.Helper()
	if must(t, d, path) == nil {
		if got != nil {
			t.Fatalf("%s: got %+v, the recording says null", what, *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s: got nil, the recording says an object", what)
	}
	same(t, what+".paused", got.Paused, must(t, d, path+".paused").(bool))
	pause := ""
	if p := optStr(d, path+".pause_reason"); p != nil {
		pause = mapped(*p, vocabSet(t, "pause_reason"))
	}
	same(t, what+".pause_reason", got.PauseReason, pause)
	same(t, what+".quarantined", got.Quarantined, must(t, d, path+".quarantined").(bool))
	same(t, what+".disabled", got.Disabled, must(t, d, path+".disabled").(bool))
}

// keepExisting returns the mutations whose path exists in the recording d (a mutation of a key the recording lacks cannot be made: the injection tests hold those keys), and fails the test
// when fewer than min remain, so that a recording that loses keys cannot quietly shrink what is mutated to nothing.
func keepExisting(t testing.TB, d any, min int, muts ...sendapitest.Mutation) []sendapitest.Mutation {
	t.Helper()
	var kept []sendapitest.Mutation
	var dropped []string
	for _, m := range muts {
		if _, ok := lookup(d, m.Path); ok {
			kept = append(kept, m)
		} else {
			dropped = append(dropped, m.Path)
		}
	}
	if len(kept) < min {
		t.Fatalf("only %d of the %d mutations can be made on the recording (it lacks %v): at least %d are required", len(kept), len(muts), dropped, min)
	}
	return kept
}

// listMutations are the mutations of a listing of accounts, by what the recording holds: the breakages of an account (a key removed, a type changed, a timestamp unreadable, another
// tenant's account, an id that is not a uuid) when it has accounts in it, and the cursor of an EMPTY page, which the module never names, when it has none.
func listMutations(t testing.TB, d map[string]any, other string) []sendapitest.Mutation {
	t.Helper()
	n := length(t, d, "items")
	muts := []sendapitest.Mutation{del("items", "items"), set("next_cursor", 5, "next_cursor")}
	if n == 0 {
		return keepExisting(t, d, 3, append(muts, set("next_cursor", "abc", "next_cursor"))...)
	}
	muts = append(muts,
		del("items[0].id", "items[0].id"), set("items[0].user_id", other, "items[0].user_id"), set("items[0].created_at", "yesterday", "items[0].created_at"),
		del("items[0].status", "items[0].status"), set("items[0].status.paused", "no", "items[0].status.paused"), del("items[0].verification.state", "items[0].verification.state"),
		set("items[0].smtp_port", "587", "items[0].smtp_port"), set("items[0].id", "not-a-uuid", "items[0].id"),
		set("items[0].verification.verified_at", "x", "items[0].verification.verified_at"), set("items[0].from_alignment.checked_at", "x", "items[0].from_alignment.checked_at"),
	)
	if n > 1 {
		muts = append(muts, set("items[1].user_id", other, "items[1].user_id"))
	}
	if n > 2 {
		muts = append(muts, del("items[2].status.disabled", "items[2].status.disabled"))
	}
	return keepExisting(t, d, 8, muts...)
}

// createConflictMapsAs holds the adapter's reading of a create conflict to the recorded body: a duplicate exactly when the body names smtp_username in its details, a conflict whose
// cause is not stated otherwise, and never the test button's "unavailable".
func createConflictMapsAs(t testing.TB, err error, body any) {
	t.Helper()
	if modclient.KindOf(err) != modclient.Refused {
		t.Fatalf("a create conflict is a refusal: %v", err)
	}
	named := namesUsername(t, body)
	if errors.Is(err, m3.ErrAccountDuplicate) != named || errors.Is(err, sendapi.ErrConflictUnexplained) == named || errors.Is(err, m3.ErrActionUnavailable) {
		t.Fatalf("a create conflict (the recorded body names the username: %v): %v", named, err)
	}
}
