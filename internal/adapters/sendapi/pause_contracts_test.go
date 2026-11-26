package sendapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 4, chunk D1, phase 1: the contracts of the pause, resume and cancelled-item recordings are DECLARED (sendapi.PauseContracts) and LISTED (sendapi.Contracts) only when the switch
// sendapi.RecordedPauseFixtures is true, which the lead flips after installing the three recordings. These tests hold the two halves of that change together (the switch is never ahead of the
// files, the files are never ahead of the switch), the contracts to what the client really calls, and, once the switch is true, exercise the three contracts through the same suite as every other.

// pauseFixtureDirs are the three testdata/contract directories the switch is about (this package's, module 2's and module 3's), relative to this package: the contracts of the shared body are
// the same for the two modules and each adapter package holds the recordings of its own module.
var pauseFixtureDirs = []string{"testdata/contract", "../m2/testdata/contract", "../m3/testdata/contract"}

// pauseFixtureState says which of the named files exist under each directory: it returns "dir/name" for every file that is missing and for every file that is present, sorted.
func pauseFixtureState(dirs, names []string) (missing, present []string) {
	for _, d := range dirs {
		for _, n := range names {
			p := filepath.ToSlash(filepath.Join(d, n))
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				present = append(present, p)
			} else {
				missing = append(missing, p)
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(present)
	return missing, present
}

// scratchWith makes a directory holding exactly the named files (empty JSON documents: only their presence is looked at).
func scratchWith(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// While the switch is on, every file it is about must exist in all three directories: the lead flips the switch only after installing the recordings, and a flip that is ahead of them is red here
// (and in every adapter's no-rot check) naming the missing files. The controls run the same check on scratch directories: one with a file missing is reported, naming exactly that file, one with
// none and one with all of them are told apart, so the check is not one that passes for want of looking.
func TestPauseFixturesAreInstalledWhenTheSwitchIsOn(t *testing.T) {
	names := sendapi.PauseFixtureNames()
	if len(names) != 3 {
		t.Fatalf("the switch is about %d file(s) %v, want three: pause, resume and the 409 of a cancelled item", len(names), names)
	}
	// the controls (they run whatever the switch says)
	partial := scratchWith(t, names[0], names[2])
	if missing, present := pauseFixtureState([]string{partial}, names); len(missing) != 1 || !strings.HasSuffix(missing[0], "/"+names[1]) || len(present) != 2 {
		t.Fatalf("control: a directory without %s: missing %v, present %v, want exactly that file missing", names[1], missing, present)
	}
	if missing, _ := pauseFixtureState([]string{scratchWith(t, names...)}, names); len(missing) != 0 {
		t.Fatalf("control: a directory with every file: missing %v", missing)
	}
	if missing, _ := pauseFixtureState([]string{scratchWith(t)}, names); len(missing) != len(names) {
		t.Fatalf("control: an empty directory: missing %v, want all %d", missing, len(names))
	}
	if !sendapi.RecordedPauseFixtures() {
		t.Logf("the switch is off: the %d file(s) of the Phase 4 D1 recordings are not required in the %d directories yet", len(names), len(pauseFixtureDirs))
		return
	}
	if missing, _ := pauseFixtureState(pauseFixtureDirs, names); len(missing) != 0 {
		t.Fatalf("recordedPauseFixtures is true and these recordings are not installed: %v (install them, as the files the record run printed and cmd/contract-fixtures wrote, BEFORE the switch is flipped)", missing)
	}
}

// While the switch is off, none of the files may exist in any of the three directories: a recording that is installed must be listed (flip the switch in sendapi/contracts.go and in
// m1/contracts.go for module 1's), or the no-rot check of the package that holds it would be red for a file named by no contract, and the harness would compare nothing against it. The controls:
// a scratch directory with one of the files is reported, naming it, and one without any is not.
func TestPauseFixturesDoNotExistWhileTheSwitchIsOff(t *testing.T) {
	names := sendapi.PauseFixtureNames()
	if _, present := pauseFixtureState([]string{scratchWith(t, names[1])}, names); len(present) != 1 || !strings.HasSuffix(present[0], "/"+names[1]) {
		t.Fatalf("control: a directory with %s: present %v, want exactly that file", names[1], present)
	}
	if _, present := pauseFixtureState([]string{scratchWith(t)}, names); len(present) != 0 {
		t.Fatalf("control: an empty directory: present %v", present)
	}
	if sendapi.RecordedPauseFixtures() {
		t.Log("the switch is on: the files are required (TestPauseFixturesAreInstalledWhenTheSwitchIsOn), not forbidden")
		return
	}
	if _, present := pauseFixtureState(pauseFixtureDirs, names); len(present) != 0 {
		t.Fatalf("recordedPauseFixtures is false and these recordings are installed: %v (flip the switch, the constant in sendapi/contracts.go, in the same change)", present)
	}
}

// The declared contracts, the listed ones, the pending ones and the uncovered routes are four views of one fact. For each profile: the three contracts are declared (two for a profile with no
// pause capability's routes: one), each names a file of PauseFixtureNames once, a vocabulary that lists job_item_cancelled for the 409, and the Shape the client decodes with; while the switch
// is off Contracts lists none of them, PendingContracts all, and UncoveredRoutes names exactly the two routes with a reason; once it is on Contracts lists all of them, nothing is pending and
// nothing is uncovered. The control is the profile with no pause capability, which has no pause route to cover or to leave uncovered.
func TestThePauseContractsAreDeclaredAndListedOrPendingNeverBoth(t *testing.T) {
	names := sendapi.PauseFixtureNames()
	for name, p := range profiles() {
		declared := sendapi.PauseContracts(p)
		if len(declared) != 3 {
			t.Fatalf("%s: %d declared contracts, want pause, resume and the cancelled item's 409", name, len(declared))
		}
		var files []string
		for _, c := range declared {
			files = append(files, c.Fixture)
			if c.Module != p.Module || c.Fixture == "" || c.Status == 0 || len(c.Shape.Fields()) == 0 {
				t.Errorf("%s: the declared contract %s / %s is incomplete: %+v", name, c.Route, c.Case, c)
			}
		}
		if !slices.Equal(sorted(files), sorted(names)) {
			t.Errorf("%s: the declared contracts name %v, the switch is about %v", name, files, names)
		}
		for _, c := range declared {
			if c.Route != sendapi.RouteRun {
				continue
			}
			if c.Status != 409 || c.VocabAt["error_codes"] != sendapi.PathErrorCode || !slices.Contains(c.Vocab["error_codes"], sendapi.CodeItemCancelled) {
				t.Errorf("%s: the 409 contract is %d at %q with the codes %v, want 409 at error.code with job_item_cancelled among the codes", name, c.Status, c.VocabAt["error_codes"], c.Vocab["error_codes"])
			}
		}
		listed := map[string]bool{}
		for _, c := range sendapi.Contracts(p) {
			listed[sendapitest.Key(c)] = true
		}
		pending := sendapi.PendingContracts(p)
		uncovered := sendapi.UncoveredRoutes(p)
		for _, c := range declared {
			k := sendapitest.Key(c)
			inPending := slices.ContainsFunc(pending, func(x modclient.RouteContract) bool { return sendapitest.Key(x) == k })
			switch {
			case sendapi.RecordedPauseFixtures() && (!listed[k] || inPending):
				t.Errorf("%s: the switch is on and %s is listed %v, pending %v: want listed and not pending", name, k, listed[k], inPending)
			case !sendapi.RecordedPauseFixtures() && (listed[k] || !inPending):
				t.Errorf("%s: the switch is off and %s is listed %v, pending %v: want pending and not listed", name, k, listed[k], inPending)
			}
		}
		for _, route := range []string{sendapi.RoutePause, sendapi.RouteResume} {
			why, isUncovered := uncovered[route]
			if want := !sendapi.RecordedPauseFixtures(); isUncovered != want || (isUncovered && strings.TrimSpace(why) == "") {
				t.Errorf("%s: %s is declared uncovered %v (reason %q), want %v with a reason", name, route, isUncovered, why, want)
			}
		}
		if sendapi.RecordedPauseFixtures() && len(pending) != 0 {
			t.Errorf("%s: the switch is on and %d contract(s) are still pending", name, len(pending))
		}
	}
	// the control: a profile with no pause capability declares only the 409 and has no pause route to leave uncovered
	bare := m3like()
	bare.PauseCapability = ""
	if cs := sendapi.PauseContracts(bare); len(cs) != 1 || cs[0].Route != sendapi.RouteRun {
		t.Errorf("control: a profile with no pause capability declares %d contract(s) %+v, want only the run 409", len(cs), cs)
	}
	if u := sendapi.UncoveredRoutes(bare); len(u) != 0 {
		t.Errorf("control: a profile with no pause capability leaves %v uncovered, want nothing", u)
	}
	if sendapi.PauseContracts(sendapi.Profile{}) != nil || sendapi.PendingContracts(sendapi.Profile{}) != nil {
		t.Error("control: an invalid profile has declared contracts")
	}
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

// The routes the client calls are exactly the routes of its contracts plus the routes it declares uncovered, with none in both: collected from the requests the client's methods really make (not from a
// list), as module 1's adapter does. The controls: dropping a declaration is caught, and a route nothing calls that a contract names is caught.
func TestTheRoutesTheClientCallsAreCoveredOrDeclaredUncovered(t *testing.T) {
	routeOf := func(method, path string) string {
		switch path {
		case "/v1/describe", "/v1/quote", "/v1/run", "/v1/report", "/v1/cancel", "/v1/pause", "/v1/resume":
			return method + " " + path
		}
		return method + " " + path + " (unknown)"
	}
	for name, p := range profiles() {
		e := newEnv(t, p, userA, reply(200, "{}"), modclient.Options{}, sendapi.Options{})
		ctx := ctxT(t)
		_, _ = e.c.Quote(ctx, 5)
		_, _ = e.c.Run(ctx, jobItem)
		_, _ = e.c.ReportJob(ctx, jobItem)
		_, _ = e.c.ReportCampaignPage(ctx, campaign, "")
		_, _ = e.c.Cancel(ctx, campaign)
		_, _ = e.c.Pause(ctx, campaign)
		_, _ = e.c.Resume(ctx, campaign)
		_, _ = sendapi.Describe(ctx, e.c.Module(), p)
		called := map[string]bool{}
		for _, r := range e.stub.Requests() {
			called[routeOf(r.Method, r.Path)] = true
		}
		covered := map[string]bool{}
		for _, c := range sendapi.Contracts(p) {
			covered[c.Route] = true
		}
		uncovered := map[string]bool{}
		for r := range sendapi.UncoveredRoutes(p) {
			uncovered[r] = true
		}
		gap := func(called, covered, uncovered map[string]bool) []string {
			var out []string
			for r := range called {
				switch {
				case !covered[r] && !uncovered[r]:
					out = append(out, r+": called, covered by no contract and not declared uncovered")
				case covered[r] && uncovered[r]:
					out = append(out, r+": both covered and declared uncovered")
				}
			}
			for r := range covered {
				if !called[r] {
					out = append(out, r+": covered by a contract and never called by the client")
				}
			}
			for r := range uncovered {
				if !called[r] {
					out = append(out, r+": declared uncovered and never called by the client")
				}
			}
			sort.Strings(out)
			return out
		}
		if len(called) != 7 {
			t.Fatalf("%s: the client called %d distinct route(s) (%v), want 7 (quote, run, report, describe, cancel, pause, resume)", name, len(called), called)
		}
		if g := gap(called, covered, uncovered); len(g) != 0 {
			t.Fatalf("%s: %v", name, g)
		}
		if !sendapi.RecordedPauseFixtures() {
			// the control: without the declaration the two routes are called and covered by nothing
			if g := gap(called, covered, map[string]bool{}); len(g) != 2 || !strings.Contains(g[0]+g[1], "POST /v1/pause: called, covered by no contract") {
				t.Fatalf("%s: control: dropping the declarations gave %v, want the two routes named", name, g)
			}
		}
		withGhost := maps2(covered)
		withGhost["GET /v1/nothing"] = true
		if g := gap(called, withGhost, uncovered); len(g) != 1 || !strings.Contains(g[0], "GET /v1/nothing: covered by a contract and never called") {
			t.Fatalf("%s: control: a contract for a route nothing calls gave %v", name, g)
		}
	}
}

func maps2(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// recordedCampaign is the campaign id (and, for the recorded answer of a pause or a resume, the only id the client will accept) that a fixture of this package holds.
func recordedCampaign(t testing.TB, file string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sendapitest.Read(t, fixtures, file), &m); err != nil {
		t.Fatal(err)
	}
	id, _ := m["campaign_id"].(string)
	if id == "" {
		t.Fatalf("%s holds no campaign_id", file)
	}
	return id
}

// recordedPaused is the `paused` the fixture says.
func recordedPaused(t testing.TB, file string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sendapitest.Read(t, fixtures, file), &m); err != nil {
		t.Fatal(err)
	}
	b, ok := m["paused"].(bool)
	if !ok {
		t.Fatalf("%s holds no boolean paused", file)
	}
	return b
}

// pauseCases are the cases of the suite for the three Phase 4 D1 contracts; TestContractsOfTheSharedBody adds them to its table only when the switch is on (a contract the suite does not
// list has no case, and a case with no contract is red).
func pauseCases() map[string]sendapitest.Case {
	hold := func(file string, call func(c *sendapi.Client, ctx context.Context, id string) (sendapi.PauseResult, error)) sendapitest.Case {
		return sendapitest.Case{
			Do: func(t testing.TB, url, token string) (any, error) {
				_, c := m3clients(t, url, token)
				return call(c, sendapitest.Ctx(t), recordedCampaign(t, file))
			},
			Check: func(t testing.TB, v any, err error) {
				r := v.(sendapi.PauseResult)
				if want := recordedCampaign(t, file); r.CampaignID != want || r.Paused != recordedPaused(t, file) {
					t.Fatalf("%s decoded as %+v, want campaign %s and paused=%v as the file says", file, r, want, recordedPaused(t, file))
				}
			},
			Mutations: []sendapitest.Mutation{
				del("campaign_id", "campaign_id"), del("paused", "paused"), set("paused", "true", "paused"), set("paused", 1, "paused"),
				set("campaign_id", "00000000-0000-4000-8000-0000000000ff", "campaign_id"), // the answer is for another campaign than the one named
			},
		}
	}
	return map[string]sendapitest.Case{
		"POST /v1/pause | ok": hold("pause.ok.json", func(c *sendapi.Client, ctx context.Context, id string) (sendapi.PauseResult, error) {
			return c.Pause(ctx, id)
		}),
		"POST /v1/resume | ok": hold("resume.ok.json", func(c *sendapi.Client, ctx context.Context, id string) (sendapi.PauseResult, error) {
			return c.Resume(ctx, id)
		}),
		"POST /v1/run | error job_item_cancelled": {
			Do:   call(func(ctx context.Context, c *sendapi.Client) (any, error) { return c.Run(ctx, jobItem) }),
			Kind: modclient.Refused,
			Code: sendapi.CodeItemCancelled,
			Check: func(t testing.TB, v any, err error) {
				if !errors.Is(err, sendapi.ErrItemCancelled) || errors.Is(err, sendapi.ErrBackpressure) || errors.Is(err, sendapi.ErrConflictUnexplained) {
					t.Fatalf("the recorded 409 of a cancelled item is %v, want ErrItemCancelled and neither back-pressure nor an unexplained conflict", err)
				}
			},
			Mutations: []sendapitest.Mutation{brokenEnvelope()},
		},
	}
}
