package sendapi_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

func str(s string) *string { return &s }

func item(n int, state string, reason *string) sendapi.Item {
	return sendapi.Item{JobItemID: idOf(n), State: state, Reason: reason}
}

// Module 2's states and reasons against the platform's charge table: only a terminal item settles, and the rule that decides it is the one the table names for it (M2-01 sent is charged
// as m2_accepted, M2-02 bounced as m2_bounced, M2-03 and M2-04 every failed is free, M2-08 cancelled is free); queued, assigned and sending are "not yet" (the table says NotFinal for
// them too: M2-05 to M2-07). Every (state, reason) pair the module can emit is covered: nothing is Unmapped.
func TestClassifyModule2AgainstTheChargeTable(t *testing.T) {
	c := newEnv(t, m2like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	type want struct {
		rule string
		kind charge.Kind
		unit string
	}
	terminal := map[string]map[string]want{ // state -> reason ("" = null) -> decision
		"sent":      {"": {"M2-01", charge.Charge, "m2_accepted"}},
		"bounced":   {"bounce_permanent": {"M2-02", charge.Charge, "m2_bounced"}},
		"failed":    {"attempts_exhausted": {"M2-03", charge.Free, ""}, "pool_exhausted": {"M2-04", charge.Free, ""}, "": {"M2-04", charge.Free, ""}},
		"cancelled": {"cancelled": {"M2-08", charge.Free, ""}},
	}
	notYet := map[string]string{"queued": "M2-05", "assigned": "M2-06", "sending": "M2-07"}
	n := 0
	for state, byReason := range terminal {
		for reason, w := range byReason {
			n++
			var r *string
			if reason != "" {
				r = str(reason)
			}
			got, err := c.Classify([]sendapi.Item{item(n, state, r)})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Settle) != 1 || len(got.NotYet) != 0 || len(got.Unmapped) != 0 {
				t.Fatalf("%s/%q: %+v", state, reason, got)
			}
			d := charge.Decide(got.Settle[0].Outcome)
			if d.Rule != w.rule || d.Kind != w.kind || d.Unit != w.unit {
				t.Fatalf("%s/%q: decided by %s as %s %q, want %s %s %q", state, reason, d.Rule, d.Kind, d.Unit, w.rule, w.kind, w.unit)
			}
			if got.Settle[0].Outcome.Module != "m2" || got.Settle[0].ItemRef != idOf(n) {
				t.Fatalf("the settle item: %+v", got.Settle[0])
			}
		}
	}
	for state, rule := range notYet {
		n++
		got, err := c.Classify([]sendapi.Item{item(n, state, nil)})
		if err != nil || len(got.Settle) != 0 || len(got.NotYet) != 1 || len(got.Unmapped) != 0 || got.NotYet[0].State != state {
			t.Fatalf("%s: %v %+v", state, err, got)
		}
		if d := charge.Decide(c.Profile().Outcome(item(n, state, nil))); d.Kind != charge.NotFinal || d.Rule != rule {
			t.Fatalf("%s: the table says %s by %s, want notfinal by %s", state, d.Kind, d.Rule, rule)
		}
	}
	// every state of the module's vocabulary is one of the above: nothing is left to guess
	for _, s := range c.Profile().JobStates {
		if _, a := terminal[s]; !a && notYet[s] == "" {
			t.Fatalf("the state %q of the vocabulary has no expected decision in this test", s)
		}
	}
	// every reason the module can name, under every state it can name it: never Unmapped, and the class is what the state says
	for _, s := range c.Profile().JobStates {
		for _, r := range append([]string{""}, c.Profile().ItemReasons...) {
			n++
			var rp *string
			if r != "" {
				rp = str(r)
			}
			got, err := c.Classify([]sendapi.Item{item(n, s, rp)})
			if err != nil || len(got.Unmapped) != 0 || len(got.Settle)+len(got.NotYet) != 1 {
				t.Fatalf("%s/%q: %v %+v", s, r, err, got)
			}
		}
	}
}

// Module 3's table row says every state is free (M3-99: not metered; M3-01 is the cancelled row), and the adapter still settles only the terminal ones: the paired control shows the table alone would call a queued
// item free, so the terminal check is the adapter's.
func TestClassifyModule3NeverSettlesWhatIsNotFinal(t *testing.T) {
	c := newEnv(t, m3like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	for _, s := range []string{"queued", "assigned", "sending"} {
		if d := charge.Decide(charge.Outcome{Module: "m3", State: s}); d.Kind != charge.Free || d.Rule != "M3-99" {
			t.Fatalf("control: the table says %s by %s for module 3 %s", d.Kind, d.Rule, s)
		}
		got, err := c.Classify([]sendapi.Item{item(1, s, nil)})
		if err != nil || len(got.Settle) != 0 || len(got.NotYet) != 1 {
			t.Fatalf("%s: %v %+v", s, err, got)
		}
	}
	for _, s := range []string{"sent", "bounced", "failed"} {
		got, err := c.Classify([]sendapi.Item{item(1, s, nil)})
		if err != nil || len(got.Settle) != 1 || len(got.NotYet) != 0 || len(got.Unmapped) != 0 {
			t.Fatalf("%s: %v %+v", s, err, got)
		}
		if d := charge.Decide(got.Settle[0].Outcome); d.Kind != charge.Free || d.Rule != "M3-99" || got.Settle[0].Outcome.Module != "m3" {
			t.Fatalf("%s: %+v", s, d)
		}
	}
}

// An item whose state the profile does not list is Unmapped with rule "none" and is never settled or waited for: a hand-made item, because the adapter's own decoding never produces one.
// The second case drives a profile whose module the charge table does not name: the table says unmapped and the item stays out of Settle. The control is a known state.
func TestClassifyFlagsWhatTheTableCannotClassify(t *testing.T) {
	c := newEnv(t, m2like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	got, err := c.Classify([]sendapi.Item{item(1, "teleported", str("because")), item(2, "sent", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Settle) != 1 || got.Settle[0].ItemRef != idOf(2) || len(got.Unmapped) != 1 {
		t.Fatalf("%+v", got)
	}
	u := got.Unmapped[0]
	if u.JobItemID != idOf(1) || u.State != "teleported" || u.Reason != "because" || u.Rule != "none" || u.Note == "" {
		t.Fatalf("unmapped = %+v", u)
	}

	odd := m2like()
	odd.Module = "m9" // not in the charge table: every outcome of it is unmapped
	uc := sendapi.UnvalidatedClientForTest(odd)
	got, err = uc.Classify([]sendapi.Item{item(1, "sent", nil), item(2, "queued", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Settle) != 0 || len(got.Unmapped) != 1 || got.Unmapped[0].Rule != "none" || len(got.NotYet) != 1 {
		t.Fatalf("a module the table does not name: %+v", got)
	}
}

// A batch that cannot be keyed completely is refused whole and nothing is returned; the item_ref is the job item id wherever the item sits in the batch.
func TestClassifyKeysByTheJobItemID(t *testing.T) {
	c := newEnv(t, m2like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	for name, items := range map[string][]sendapi.Item{
		"no id":         {item(1, "sent", nil), {State: "sent"}},
		"a blank id":    {{JobItemID: "  ", State: "sent"}},
		"a repeated id": {item(1, "sent", nil), item(1, "bounced", str("bounce_permanent"))},
		"a long id":     {{JobItemID: strings.Repeat("a", 201), State: "sent"}},
	} {
		got, err := c.Classify(items)
		wantKind(t, err, modclient.Invalid)
		if !reflect.DeepEqual(got, sendapi.Classified{}) {
			t.Fatalf("%s: a classification came back with the error: %+v", name, got)
		}
	}
	got, err := c.Classify([]sendapi.Item{item(9, "sent", nil), item(3, "bounced", str("bounce_permanent")), item(5, "queued", nil)})
	if err != nil || len(got.Settle) != 2 || got.Settle[0].ItemRef != idOf(9) || got.Settle[1].ItemRef != idOf(3) || got.NotYet[0].JobItemID != idOf(5) {
		t.Fatalf("control: %v %+v", err, got)
	}
	var _ []money.SettleItem = got.Settle // the shape money.Settle takes
	if len(got.Settle[0].ItemRef) > 200 || strings.TrimSpace(got.Settle[0].ItemRef) == "" {
		t.Fatal("the item_ref does not satisfy money.SettleItem")
	}
	// the class is recomputed from the state: an edited Item.Class changes nothing
	edited := item(1, "queued", nil)
	edited.Class = sendapi.ClassTerminal
	got, err = c.Classify([]sendapi.Item{edited})
	if err != nil || len(got.Settle) != 0 || len(got.NotYet) != 1 {
		t.Fatalf("an edited class settled a queued item: %v %+v", err, got)
	}
}

// What reaches the charge table is the module, the state and the reason, and nothing else: not the attempt count (which is not an attempt count), not the error class, not the provider
// id, not the account. The control changes the reason and the state, which do change it.
func TestTheOutcomeReadsOnlyStateAndReason(t *testing.T) {
	p := m2like()
	base := item(1, "failed", str("attempts_exhausted"))
	want := charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"}
	if got := p.Outcome(base); got != want {
		t.Fatalf("outcome = %+v", got)
	}
	noisy := base
	noisy.JobItemID, noisy.JobID, noisy.AttemptCount = "something else", "a job", 9999
	noisy.ErrorClass, noisy.ProviderMessageID, noisy.SMTPResponse, noisy.AccountID = str("timeout"), str("<x>"), str("550"), str("acct")
	if got := p.Outcome(noisy); got != want {
		t.Fatalf("the outcome depends on more than state and reason: %+v", got)
	}
	if p.Outcome(item(1, "failed", str("pool_exhausted"))) == want || p.Outcome(item(1, "sent", str("attempts_exhausted"))) == want {
		t.Fatal("the control: a different reason or state gives the same outcome")
	}
	if got := p.Outcome(item(1, "sent", nil)); got.Reason != "" {
		t.Fatalf("a null reason is %q", got.Reason)
	}
	if got := m3like().Outcome(item(1, "sent", nil)); got.Module != "m3" {
		t.Fatalf("module 3's outcome names module %q", got.Module)
	}
}

func TestClassStrings(t *testing.T) {
	for c, want := range map[sendapi.Class]string{sendapi.ClassPending: "pending", sendapi.ClassInFlight: "in flight", sendapi.ClassTerminal: "terminal", 0: "class(0)"} {
		if c.String() != want {
			t.Fatalf("%d is %q, want %q", c, c.String(), want)
		}
	}
}

// Module 3 has the `cancelled` state since the Phase 3 close-out (C1): a cancelled item is terminal, settles FREE as the charge table's row M3-01 (module 3 is not metered; the row names
// the cancel, the wildcard row M3-99 stays the rule of every other state), and an item that is not final is still not an outcome. The controls: module 2's cancelled is M2-08, and a profile
// that does not list the state flags it Unmapped (the adapter never maps a state it was not given).
func TestClassifyModule3CancelledIsTerminalAndFreeByRowM3_01(t *testing.T) {
	c := newEnv(t, m3like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	got, err := c.Classify([]sendapi.Item{item(1, "cancelled", nil), item(2, "queued", nil), item(3, "sent", nil)})
	if err != nil || len(got.Settle) != 2 || len(got.NotYet) != 1 || len(got.Unmapped) != 0 {
		t.Fatalf("%v %+v", err, got)
	}
	var cancelled money.SettleItem
	for _, s := range got.Settle {
		if s.ItemRef == idOf(1) {
			cancelled = s
		}
	}
	if d := charge.Decide(cancelled.Outcome); cancelled.Outcome.Module != "m3" || cancelled.Outcome.State != "cancelled" || d.Kind != charge.Free || d.Rule != "M3-01" || d.Unit != "" {
		t.Fatalf("a cancelled item of module 3: %+v by %+v", cancelled.Outcome, d)
	}
	if d := charge.Decide(charge.Outcome{Module: "m3", State: "sent"}); d.Rule != "M3-99" || d.Kind != charge.Free {
		t.Fatalf("control: a sent item of module 3 is %+v, the wildcard row says free by M3-99", d)
	}
	m2c := newEnv(t, m2like(), userA, nil, modclient.Options{}, sendapi.Options{}).c
	g2, err := m2c.Classify([]sendapi.Item{item(1, "cancelled", nil)})
	if d := charge.Decide(g2.Settle[0].Outcome); err != nil || d.Rule != "M2-08" || d.Kind != charge.Free {
		t.Fatalf("control: module 2's cancelled item: %+v %v", g2, err)
	}
	noCancel := m3like()
	noCancel.JobStates, noCancel.Cancel = slices.DeleteFunc(slices.Clone(noCancel.JobStates), func(s string) bool { return s == "cancelled" }), false
	nc := newEnv(t, noCancel, userA, nil, modclient.Options{}, sendapi.Options{}).c
	g3, err := nc.Classify([]sendapi.Item{item(1, "cancelled", nil)})
	if err != nil || len(g3.Settle) != 0 || len(g3.Unmapped) != 1 || g3.Unmapped[0].Rule != "none" {
		t.Fatalf("control: a profile without cancelled: %+v %v", g3, err)
	}
}

// A campaign report of module 3 carries `totals.cancelled` and items in the state `cancelled`, read as they are (terminal, counted): the report of a campaign cancelled before anything was
// claimed is complete. The control is the same report with the key missing (the module always sends it now: a missing key is a Contract error naming it).
func TestModule3CampaignReportCarriesTheCancelledCount(t *testing.T) {
	c := fakeCampaign(m3like(), userA, 3, false)
	c.Items[0]["state"], c.Items[1]["state"] = "cancelled", "cancelled"
	base := c.Header
	c.Header = func(total int) map[string]any {
		h := base(total)
		h["totals"].(map[string]any)["cancelled"] = 2
		h["totals"].(map[string]any)["sent"] = 1
		h["progress"].(map[string]any)["terminal"] = 3
		return h
	}
	e := newEnv(t, m3like(), userA, c.Handler(), modclient.Options{}, sendapi.Options{})
	rep, err := e.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || rep.Totals.Cancelled != 2 || rep.Totals.Sent != 1 || len(rep.Items) != 3 || !rep.Progress.IsComplete || !rep.ItemsMatchTotal() {
		t.Fatalf("%+v %v", rep, err)
	}
	for i, it := range rep.Items[:2] {
		if it.State != "cancelled" || it.Class != sendapi.ClassTerminal {
			t.Fatalf("item %d: %+v", i, it)
		}
	}
	// the control: the key a module that has the state always sends is required
	c2 := fakeCampaign(m3like(), userA, 3, false)
	base2 := c2.Header
	c2.Header = func(total int) map[string]any {
		h := base2(total)
		delete(h["totals"].(map[string]any), "cancelled")
		return h
	}
	e2 := newEnv(t, m3like(), userA, c2.Handler(), modclient.Options{}, sendapi.Options{})
	if _, err := e2.c.ReportCampaign(ctxT(t), campaign); modclient.KindOf(err) != modclient.Contract || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("a report with no totals.cancelled: %v", err)
	}
}
