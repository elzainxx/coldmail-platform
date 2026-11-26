package m3_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: the three contracts of the pause, resume and cancelled-item recordings (m3.PauseContracts) are listed by m3.Contracts only once the switch
// (sendapi.RecordedPauseFixtures) is on, and so are their cases here: TestContractsOfModule3 adds pauseCases to its table when the switch is on (a contract the suite has no case for is red, and
// so is a case with no contract). Every value a case checks is READ from the recording (the campaign it asks about, the `paused` it says), never written here.

// pauseCases are the suite's cases for the three contracts of module 3's recordings.
func pauseCases() map[string]sendapitest.Case {
	hold := func(file string, do func(ctx context.Context, c *m3.Client, id string) (sendapi.PauseResult, error)) sendapitest.Case {
		return sendapitest.Case{
			Do: call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) {
				return do(ctx, c, asStr(t, doc(t, file), "campaign_id"))
			}),
			Check: func(t testing.TB, v any, err error) {
				r := v.(sendapi.PauseResult)
				paused, ok := must(t, doc(t, file), "paused").(bool)
				if !ok || r.CampaignID != asStr(t, doc(t, file), "campaign_id") || r.Paused != paused {
					t.Fatalf("%s decoded as %+v, want the campaign and the paused=%v the file says", file, r, paused)
				}
			},
			Mutations: []sendapitest.Mutation{
				del("campaign_id", "campaign_id"), del("paused", "paused"), set("paused", "true", "paused"), set("paused", 1, "paused"),
				set("campaign_id", "00000000-0000-4000-8000-0000000000ff", "campaign_id"), // the answer is for another campaign than the one named
			},
		}
	}
	return map[string]sendapitest.Case{
		"POST /v1/pause | ok": hold("pause.ok.json", func(ctx context.Context, c *m3.Client, id string) (sendapi.PauseResult, error) {
			return c.Pause(ctx, id)
		}),
		"POST /v1/resume | ok": hold("resume.ok.json", func(ctx context.Context, c *m3.Client, id string) (sendapi.PauseResult, error) {
			return c.Resume(ctx, id)
		}),
		"POST /v1/run | error job_item_cancelled": {
			Do:   call(func(ctx context.Context, c *m3.Client, t testing.TB) (any, error) { return c.Run(ctx, jobItem) }),
			Kind: modclient.Refused,
			Code: sendapi.CodeItemCancelled,
			Check: func(t testing.TB, v any, err error) {
				// module 3 names the causes of its `conflict` 409; this code is a code of its own and is never one of them
				if !errors.Is(err, sendapi.ErrItemCancelled) || errors.Is(err, sendapi.ErrConflictUnexplained) || errors.Is(err, sendapi.ErrRunCampaignActive) || errors.Is(err, sendapi.ErrRunQueueCap) {
					t.Fatalf("module 3's recorded 409 of a cancelled item is %v, want ErrItemCancelled and no cause of a conflict", err)
				}
			},
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
	}
}

// With the switch off the three contracts are declared and pending and nothing lists them; on, they are listed and nothing is pending. Either way each is named once by m3.PauseContracts, and the
// cases of this file are exactly those three (so that flipping the switch is the only edit the suite needs).
func TestThePauseContractsOfModule3AreDeclaredAndTheirCasesAreExactlyThose(t *testing.T) {
	declared := m3.PauseContracts()
	cases := pauseCases()
	if len(declared) != 3 || len(cases) != 3 {
		t.Fatalf("%d declared contracts and %d cases, want three of each", len(declared), len(cases))
	}
	for _, c := range declared {
		if _, ok := cases[sendapitest.Key(c)]; !ok {
			t.Errorf("the declared contract %s has no case in pauseCases", sendapitest.Key(c))
		}
	}
	listed, pending := map[string]bool{}, map[string]bool{}
	for _, c := range m3.Contracts() {
		listed[sendapitest.Key(c)] = true
	}
	for _, c := range m3.PendingContracts() {
		pending[sendapitest.Key(c)] = true
	}
	for _, c := range declared {
		k := sendapitest.Key(c)
		if listed[k] == pending[k] || listed[k] != m3.RecordedPauseFixtures() {
			t.Errorf("%s: listed %v, pending %v with the switch %v: want exactly one of the two, and listed only when the switch is on", k, listed[k], pending[k], m3.RecordedPauseFixtures())
		}
	}
	// the run 409 lists module 3's own error codes (which include the cancelled item's code since the pin that lists it)
	for _, c := range declared {
		if c.Route == sendapi.RouteRun && len(c.Vocab["error_codes"]) != len(m3.Profile().ErrorCodes) {
			t.Errorf("the run 409 contract lists %v as the error codes, module 3's profile has %v", c.Vocab["error_codes"], m3.Profile().ErrorCodes)
		}
	}
}
