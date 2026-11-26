package jobs

import (
	"errors"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/money"
)

// The two owner rulings of the Phase 4 close-out, as far as they are arithmetic or a pure rule (the transaction that applies them is proved against a database by TestJobsStore_* in tests/harness).
//
//	OD-3  suppression applies to SEND jobs only: a verify job's suppressed rows are counted, still verified and still charged
//	OD-4  the `unknown` choice of a chained send is explicit: nothing is defaulted, and checkUnknownChoice is the one rule of what a confirm may carry

func TestBuildPreview_ASuppressedAddressIsStillVerifiedAndChargedInAVerifyJobAndLeftOutOfASendJob(t *testing.T) {
	at := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	for _, c := range []struct {
		name            string
		module          string
		onList          int
		stillVerified   bool
		units           int
		credits         int64
		suppressedShown int
	}{
		{"verify: 2 of the 6 pending rows are on the list and all 6 are verified and charged", ModuleVerify, 2, true, 6, 6, 2},
		{"send (managed): the same two are left out, 4 units", ModuleSendManaged, 2, false, 4, 4, 2},
		{"send (BYO): the same two are left out", ModuleSendBYO, 2, false, 4, 4, 2},
		{"verify with no suppression: nothing differs (the twin)", ModuleVerify, 0, true, 6, 6, 0},
		{"verify: every pending row on the list is still a full job, not an empty one", ModuleVerify, 6, true, 6, 6, 6},
		{"send: every pending row on the list leaves nothing to send", ModuleSendManaged, 6, false, 0, 0, 6},
	} {
		sn := metered(1)
		sn.module = c.module
		p := buildPreview(sn, "", c.onList, money.Balance{Available: 100}, at)
		if p.Units != c.units || p.Credits != c.credits || p.Suppressed != c.suppressedShown || p.SuppressedStillVerified != c.stillVerified || p.Pending != 6 {
			t.Errorf("%s: units %d credits %d suppressed %d still_verified %v pending %d; want %d %d %d %v 6", c.name, p.Units, p.Credits, p.Suppressed, p.SuppressedStillVerified, p.Pending, c.units, c.credits, c.suppressedShown, c.stillVerified)
		}
	}
}

func TestBuildPreview_TheUnknownCountAndWhetherAChoiceIsRequired(t *testing.T) {
	chained := func(unknown int) *snapshot {
		sn := metered(1)
		sn.module = ModuleSendManaged
		sn.verdicts = map[string]int{"valid": 5, "invalid": 2, "risky": 1, "unknown": unknown}
		return sn
	}
	for _, c := range []struct {
		name     string
		sn       *snapshot
		choice   string
		count    int
		required bool
	}{
		{"unknown results and no choice yet", chained(2), "", 2, true},
		{"unknown results and a choice made: still required (the confirm must repeat it)", chained(2), UnknownInclude, 2, true},
		{"no unknown result: nothing to choose (the control)", chained(0), "", 0, false},
		{"a job that is not chained has no verdicts and needs no choice", metered(1), "", 0, false},
	} {
		p := buildPreview(c.sn, c.choice, 0, money.Balance{}, time.Time{})
		if p.UnknownCount != c.count || p.UnknownChoiceRequired != c.required || p.UnknownChoice != c.choice {
			t.Errorf("%s: unknown_count %d required %v choice %q; want %d %v %q", c.name, p.UnknownCount, p.UnknownChoiceRequired, p.UnknownChoice, c.count, c.required, c.choice)
		}
	}
}

func TestCheckUnknownChoice(t *testing.T) {
	type want struct {
		ok           bool
		required     int // > 0: an UnknownChoiceRequiredError with this count
		stale, badRq bool
	}
	for _, c := range []struct {
		name      string
		chained   bool
		recorded  string
		unknown   int
		requested string
		want      want
	}{
		// the refusal and its twin: with unknown results the choice is required; with it, equal to the recorded one, the confirm passes
		{"unknown results and no choice: required, with the count", true, "", 3, "", want{required: 3}},
		{"unknown results, no choice, a choice was recorded: the confirm must still carry it", true, UnknownInclude, 3, "", want{required: 3}},
		{"unknown results, include recorded, include sent: passes", true, UnknownInclude, 3, UnknownInclude, want{ok: true}},
		{"unknown results, exclude recorded, exclude sent: passes", true, UnknownExclude, 3, UnknownExclude, want{ok: true}},
		// the choice the user did not see
		{"unknown results, exclude recorded, include sent: the choice changed", true, UnknownExclude, 3, UnknownInclude, want{stale: true}},
		{"unknown results, include recorded, exclude sent: the choice changed", true, UnknownInclude, 3, UnknownExclude, want{stale: true}},
		{"unknown results, nothing recorded, a choice sent: previewed under no choice, so it changed", true, "", 3, UnknownExclude, want{stale: true}},
		// no unknown result: nothing to choose
		{"no unknown result and no choice (the old default is gone, and nothing is needed)", true, "", 0, "", want{ok: true}},
		{"no unknown result and a choice given: accepted, changes nothing", true, "", 0, UnknownInclude, want{ok: true}},
		{"no unknown result, a different choice than the recorded one: still harmless", true, UnknownExclude, 0, UnknownInclude, want{ok: true}},
		// a job that is not chained has none of it
		{"not chained, no choice", false, "", 0, "", want{ok: true}},
		{"not chained, a choice: a bad request", false, "", 0, UnknownExclude, want{badRq: true}},
	} {
		err := checkUnknownChoice(c.chained, c.recorded, c.unknown, c.requested)
		var req *UnknownChoiceRequiredError
		var stale *StalePreviewError
		switch {
		case c.want.ok:
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			}
		case c.want.required > 0:
			if !errors.Is(err, ErrUnknownChoiceRequired) || !errors.As(err, &req) || req.Unknown != c.want.required {
				t.Errorf("%s: got %v, want ErrUnknownChoiceRequired with %d unknown results", c.name, err, c.want.required)
			}
		case c.want.stale:
			if !errors.Is(err, ErrStalePreview) || !errors.As(err, &stale) || stale.Reason != ReasonUnknownChoiceChanged || errors.Is(err, ErrUnknownChoiceRequired) {
				t.Errorf("%s: got %v, want a stale preview with the reason %s", c.name, err, ReasonUnknownChoiceChanged)
			}
		case c.want.badRq:
			if !errors.Is(err, ErrBadRequest) {
				t.Errorf("%s: got %v, want ErrBadRequest", c.name, err)
			}
		}
	}
}

func TestPreviewChoice_ReadsWhatTheOptionsCarry(t *testing.T) {
	if c, given := PreviewChoice(); given || c != "" {
		t.Errorf("no option reads as %q given %v", c, given)
	}
	if c, given := PreviewChoice(nil); given || c != "" {
		t.Errorf("a nil option reads as %q given %v", c, given)
	}
	if c, given := PreviewChoice(PreviewUnknownChoice(UnknownInclude)); !given || c != UnknownInclude {
		t.Errorf("include reads as %q given %v", c, given)
	}
	// an empty choice is GIVEN (and the store refuses it): it is not the same as saying nothing
	if c, given := PreviewChoice(PreviewUnknownChoice("")); !given || c != "" {
		t.Errorf("an empty choice reads as %q given %v, want given", c, given)
	}
	// the last one wins
	if c, _ := PreviewChoice(PreviewUnknownChoice(UnknownInclude), PreviewUnknownChoice(UnknownExclude)); c != UnknownExclude {
		t.Errorf("two options read as %q, want the later one", c)
	}
}

func TestValidChoice(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{{UnknownInclude, true}, {UnknownExclude, true}, {"", false}, {"Include", false}, {"maybe", false}, {" include", false}} {
		if got := validChoice(c.in); got != c.want {
			t.Errorf("validChoice(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
