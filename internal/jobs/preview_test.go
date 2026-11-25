package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/money"
)

// buildPreview is the arithmetic of a preview: units = pending - suppressed, credits = units x the reserve rate, shortfall = max(0, credits - available). The numbers are small enough to
// check by hand, and every refusal-shaped case (a shortfall) has its twin (a balance that just covers).

func metered(rate int) *snapshot {
	return &snapshot{
		metered: true, rows: 10, pending: 6, malformed: 2, duplicates: 1, excluded: 1,
		reasons:     map[string]int{"no_at": 1, "empty": 1},
		quote:       money.RateQuote{Unit: "m1_own", UnitCost: rate, Prices: map[string]int{"m1_own": rate}},
		fingerprint: "f",
	}
}

func TestBuildPreview_UnitsCreditsAndShortfall(t *testing.T) {
	at := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	for _, c := range []struct {
		name                          string
		rate, onList                  int
		available                     int64
		units, suppressed             int
		credits, wantAvail, shortfall int64
	}{
		{"2 of 6 pending rows are suppressed, rate 1, short by 1", 1, 2, 3, 4, 2, 4, 3, 1},
		{"the same, the balance just covers (the twin)", 1, 2, 4, 4, 2, 4, 4, 0},
		{"the same, plenty", 1, 2, 1000, 4, 2, 4, 1000, 0},
		{"a dearer rate (2): the vendor unit reachable", 2, 2, 7, 4, 2, 8, 7, 1},
		{"no suppression", 1, 0, 0, 6, 0, 6, 0, 6},
		{"every pending row suppressed: zero units is a valid preview", 1, 6, 0, 0, 6, 0, 0, 0},
	} {
		p := buildPreview(metered(c.rate), UnknownExclude, c.onList, money.Balance{Available: c.available}, at)
		if p.Units != c.units || p.Suppressed != c.suppressed || p.Credits != c.credits || p.Available != c.wantAvail || p.Shortfall != c.shortfall {
			t.Errorf("%s: units %d suppressed %d credits %d available %d shortfall %d; want %d %d %d %d %d",
				c.name, p.Units, p.Suppressed, p.Credits, p.Available, p.Shortfall, c.units, c.suppressed, c.credits, c.wantAvail, c.shortfall)
		}
		if p.Rows != 10 || p.Pending != 6 || p.Malformed != 2 || p.Duplicates != 1 || p.Excluded != 1 || p.MissingFields != 0 || !p.Metered || p.RateUnit != "m1_own" || p.UnitCost != c.rate {
			t.Errorf("%s: the counts changed: %+v", c.name, p)
		}
		if !p.ComputedAt.Equal(at) || p.Fingerprint != "f" {
			t.Errorf("%s: computed_at %v fingerprint %q", c.name, p.ComputedAt, p.Fingerprint)
		}
	}
}

func TestBuildPreview_ModuleThreeHoldsNoCredits(t *testing.T) {
	sn := &snapshot{metered: false, rows: 3, pending: 3, fingerprint: "f"}
	// even a balance and a rate that would make a metered job short change nothing here
	p := buildPreview(sn, UnknownExclude, 1, money.Balance{Available: 0}, time.Time{})
	if p.Metered || p.Credits != 0 || p.Available != 0 || p.Shortfall != 0 || p.UnitCost != 0 || p.RateUnit != "" || p.Units != 2 {
		t.Errorf("module 3 preview = %+v, want units 2 and no credits at all", p)
	}
}

func TestBuildPreview_AlreadySuppressedRowsAreReadAsPendingAndCountedSuppressed(t *testing.T) {
	sn := metered(1)
	sn.pending, sn.suppressed = 5, 1 // never in a draft, but the fingerprint reads them as pending and so does the arithmetic
	p := buildPreview(sn, UnknownExclude, 1, money.Balance{Available: 100}, time.Time{})
	if p.Pending != 6 || p.Suppressed != 2 || p.Units != 4 {
		t.Errorf("pending %d suppressed %d units %d; want 6, 2, 4", p.Pending, p.Suppressed, p.Units)
	}
}

func TestBuildPreview_ChainedCarriesTheVerdictsAndTheChoice(t *testing.T) {
	sn := metered(1)
	sn.verdicts = map[string]int{"valid": 5, "invalid": 2, "risky": 1, "unknown": 2}
	sn.selected = 6
	p := buildPreview(sn, UnknownExclude, 0, money.Balance{}, time.Time{})
	if p.Selected != 6 || p.UnknownChoice != UnknownExclude || len(p.Verdicts) != 4 || p.Verdicts["unknown"] != 2 {
		t.Errorf("chained preview = %+v", p)
	}
	// a job that is not chained has no verdicts, and the JSON says so
	plain := buildPreview(metered(1), UnknownExclude, 0, money.Balance{}, time.Time{})
	b, _ := json.Marshal(plain)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, has := m["verdicts"]; has {
		t.Errorf("a job that is not chained has a verdicts member: %s", b)
	}
	for _, k := range []string{"rows", "pending", "malformed", "malformed_reasons", "duplicates", "excluded", "suppressed", "missing_fields", "units", "unknown_choice", "metered", "unit_cost", "credits", "available", "shortfall", "fingerprint", "computed_at"} {
		if _, has := m[k]; !has {
			t.Errorf("the preview JSON has no %q member (the API and the stored preview read it)", k)
		}
	}
}

func TestPreview_JSONRoundTrips(t *testing.T) {
	p := buildPreview(metered(2), UnknownInclude, 1, money.Balance{Available: 5}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Preview
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(back)
	if string(b) != string(b2) {
		t.Errorf("the stored preview does not read back as itself:\n%s\n%s", b, b2)
	}
}
