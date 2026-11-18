package sendapi_test

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/contractcheck"
	"github.com/coldmail-platform/platform/internal/modclient"
)

func hasProblem(problems []string, subs ...string) bool {
	for _, p := range problems {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(p, s)
		}
		if ok {
			return true
		}
	}
	return false
}

// withContract returns a copy of the contracts in which the one named route and case has been changed by edit (its Vocab and VocabAt maps are copies too).
func withContract(t testing.TB, cs []modclient.RouteContract, route, name string, edit func(c *modclient.RouteContract)) []modclient.RouteContract {
	t.Helper()
	out := append([]modclient.RouteContract(nil), cs...)
	for i := range out {
		if out[i].Route == route && out[i].Case == name {
			out[i].Vocab, out[i].VocabAt = maps.Clone(out[i].Vocab), maps.Clone(out[i].VocabAt)
			edit(&out[i])
			return out
		}
	}
	t.Fatalf("no contract %s / %s", route, name)
	return nil
}

func problemText(ps []contractcheck.Problem) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

// Every vocabulary a contract lists constrains something in the contract's own recorded body: it has a path (VocabAt), the path is of the grammar, the body holds a string there and
// every string it holds is in the set. The adapter side's own check (sendapitest.CheckVocabAt) and the drift gate's no-rot check (contractcheck.CheckFixtures) are both run on the
// contracts of the module-3-shaped profile and its fixtures. The paired controls: a path with a typo, a path at a position that holds no string, a vocabulary with no path, a path with no
// vocabulary, a value of the body that the set does not list and a path that is not of the grammar are each reported, naming the vocabulary and the reason.
//
// WHAT BITES ONLY WHEN CheckFixtures READS VocabAt: until then it reads each Vocab key as a path itself, so the zero-problems assertion on CheckFixtures fails on the NAMES (job_state is
// not a position of any body) and its control cannot tell a right path from a wrong one; sendapitest.CheckVocabAt is the independent check that holds in both cases.
func TestEveryVocabularyConstrainsSomethingInItsOwnFixture(t *testing.T) {
	cs := sendapi.Contracts(m3like())
	fsys := os.DirFS(".")
	if problems := sendapitest.CheckVocabAt(cs, fsys, fixtures); len(problems) != 0 {
		t.Fatalf("a vocabulary constrains nothing, or is misplaced, in its own fixture: %v", problems)
	}
	if problems := contractcheck.CheckFixtures(cs, fsys, fixtures); len(problems) != 0 {
		t.Fatalf("the drift gate's fixture check reports problems: %v", problemText(problems))
	}
	// the subject is not empty: some contract carries a vocabulary and a path
	carried := 0
	for _, c := range cs {
		carried += len(c.Vocab)
	}
	if carried < 6 {
		t.Fatalf("only %d vocabularies are placed: the subject of the check is thin", carried)
	}

	type control struct {
		name  string
		route string
		cas   string
		edit  func(c *modclient.RouteContract)
		want  []string // substrings of one problem of CheckVocabAt
		gate  bool     // whether contractcheck.CheckFixtures must also name the vocabulary (the cases it has a check for)
	}
	controls := []control{
		{"a path with a typo", sendapi.RouteReport, "campaign page", func(c *modclient.RouteContract) { c.VocabAt["error_class"] = "items.results[].error_clas" },
			[]string{`"error_class"`, "constrains nothing"}, true},
		{"a path at a position that holds no string", sendapi.RouteReport, "campaign page", func(c *modclient.RouteContract) { c.VocabAt["job_state"] = "totals.total" },
			[]string{`"job_state"`, "constrains nothing"}, true},
		{"a path at the wrong depth", sendapi.RouteRun, "created", func(c *modclient.RouteContract) { c.VocabAt["job_state"] = "items.results[].state" },
			[]string{`"job_state"`, "constrains nothing"}, true},
		{"a vocabulary with no path", sendapi.RouteRun, "created", func(c *modclient.RouteContract) { delete(c.VocabAt, "job_state") },
			[]string{`"job_state"`, "gives it no path"}, false},
		{"a path with no vocabulary", sendapi.RouteRun, "created", func(c *modclient.RouteContract) { c.VocabAt["error_codes"] = "error.code" },
			[]string{`"error_codes"`, "no such entry"}, false},
		{"a value of the body that the set does not list", sendapi.RouteRun, "error conflict", func(c *modclient.RouteContract) {
			c.Vocab["error_codes"] = slices.DeleteFunc(slices.Clone(c.Vocab["error_codes"]), func(s string) bool { return s == "conflict" })
		}, []string{`"error_codes"`, `"conflict"`, "does not list"}, true},
		{"a path that is not of the grammar", sendapi.RouteReport, "campaign page", func(c *modclient.RouteContract) { c.VocabAt["job_state"] = "items..state" },
			[]string{`"job_state"`, "not of the grammar"}, true},
		{"a set that lists a value twice", sendapi.RouteRun, "created", func(c *modclient.RouteContract) { c.Vocab["job_state"] = append(c.Vocab["job_state"], "queued") },
			[]string{`"job_state"`, "twice"}, true},
		{"an empty set", sendapi.RouteRun, "created", func(c *modclient.RouteContract) { c.Vocab["job_state"] = nil }, []string{`"job_state"`, "empty"}, true},
	}
	for _, tc := range controls {
		t.Run(tc.name, func(t *testing.T) {
			bad := withContract(t, cs, tc.route, tc.cas, tc.edit)
			problems := sendapitest.CheckVocabAt(bad, fsys, fixtures)
			if !hasProblem(problems, tc.want...) {
				t.Fatalf("the check did not report %v: %v", tc.want, problems)
			}
			// the drift gate's own check must refuse the same contract too, naming the vocabulary (it can only tell this contract from the good one once it reads VocabAt: see the doc of
			// this test)
			if cf := problemText(contractcheck.CheckFixtures(bad, fsys, fixtures)); tc.gate && (len(cf) == 0 || !hasProblem(cf, strings.Trim(tc.want[0], `"`))) {
				t.Fatalf("CheckFixtures did not report the vocabulary %s: %v", tc.want[0], cf)
			}
		})
	}
	t.Run("empty subjects", func(t *testing.T) {
		if p := sendapitest.CheckVocabAt(nil, fsys, fixtures); !hasProblem(p, "subject of the check is empty") {
			t.Fatalf("%v", p)
		}
		noVocab := withContract(t, cs, sendapi.RouteQuote, "ok", func(c *modclient.RouteContract) {})
		for i := range noVocab {
			noVocab[i].Vocab, noVocab[i].VocabAt = nil, nil
		}
		if p := sendapitest.CheckVocabAt(noVocab, fsys, fixtures); !hasProblem(p, "subject of the check is empty") {
			t.Fatalf("%v", p)
		}
		missing := withContract(t, cs, sendapi.RouteRun, "created", func(c *modclient.RouteContract) { c.Fixture = "absent.json" })
		if p := sendapitest.CheckVocabAt(missing, fsys, fixtures); !hasProblem(p, "absent.json", "cannot be read") {
			t.Fatalf("%v", p)
		}
	})
}

// Where every vocabulary is placed, for BOTH profiles (the contracts are shared, and the module-3 adapter adds its own): the contracts that list a vocabulary and the paths they list it at.
// The vocabulary names of a contract are exactly its VocabAt names; a contract whose body has no string position for a vocabulary does not list it: the single-job reports list only
// job_state (error_class and reason are null or omitted in a report of a job that did not fail, and module 3 omits them for one that did), the last page only job_state, the quote, the
// empty page nothing, describe only error_codes (its other vocabularies sit under the keys of a map, which the drift gate's grammar folds), and module 3 has no item_reason anywhere.
func TestVocabAtPlacesEachVocabularyWhereItsBodyHoldsIt(t *testing.T) {
	const (
		state, itemState, class, reason, code, describeCode = sendapi.PathState, sendapi.PathItemState, sendapi.PathItemErrorClass, sendapi.PathItemReason, sendapi.PathErrorCode, sendapi.PathDescribeErrorCode
	)
	want := func(withReason bool) map[string]map[string]string { // "route | case" -> vocabulary -> path
		page := map[string]string{"job_state": itemState, "error_class": class}
		if withReason {
			page["item_reason"] = reason
		}
		errs := map[string]string{"error_codes": code}
		return map[string]map[string]string{
			"POST /v1/quote | ok":                              {},
			"POST /v1/run | created":                           {"job_state": state},
			"POST /v1/run | replay":                            {"job_state": state},
			"POST /v1/run | error validation_failed":           errs,
			"POST /v1/run | error unauthenticated":             errs,
			"POST /v1/run | error not_found":                   errs,
			"POST /v1/run | error conflict":                    errs,
			"POST /v1/run | error paused":                      errs,
			"POST /v1/run | error internal_error":              errs,
			"GET /v1/report | job sent":                        {"job_state": state},
			"GET /v1/report | job queued":                      {"job_state": state},
			"GET /v1/report | job error not_found":             errs,
			"GET /v1/report | campaign page":                   page,
			"GET /v1/report | campaign last page":              {"job_state": itemState},
			"GET /v1/report | campaign empty":                  {},
			"GET /v1/report | campaign error cursor_malformed": errs,
			"GET /v1/report | campaign error cursor_forged":    errs,
			"GET /v1/describe | ok":                            {"error_codes": describeCode},
		}
	}
	for name, p := range profiles() {
		t.Run(name, func(t *testing.T) {
			cs := sendapi.Contracts(p)
			expected := want(len(p.ItemReasons) > 0)
			if len(cs) != len(expected) {
				t.Fatalf("%d contracts, the table has %d", len(cs), len(expected))
			}
			for _, c := range cs {
				k := sendapitest.Key(c)
				exp, ok := expected[k]
				if !ok {
					t.Fatalf("the contract %q is not in the table", k)
				}
				got := map[string]string{}
				maps.Copy(got, c.VocabAt)
				if !maps.Equal(got, exp) && !(len(got) == 0 && len(exp) == 0) {
					t.Fatalf("%s: placed %v, want %v", k, got, exp)
				}
				// the names of Vocab and of VocabAt are the same names
				names := slices.Sorted(maps.Keys(c.Vocab))
				if !slices.Equal(names, slices.Sorted(maps.Keys(c.VocabAt))) {
					t.Fatalf("%s: Vocab names %v and VocabAt names %v differ", k, names, slices.Sorted(maps.Keys(c.VocabAt)))
				}
				for n, set := range c.Vocab {
					if !slices.Equal(set, p.Vocabularies()[n]) {
						t.Fatalf("%s: the set %q is not the profile's", k, n)
					}
				}
			}
			// copies: editing one contract's VocabAt edits no other and not the next call
			for _, c := range cs {
				for n := range c.VocabAt {
					c.VocabAt[n] = "tampered"
				}
			}
			for _, c := range sendapi.Contracts(p) {
				for n, at := range c.VocabAt {
					if at == "tampered" {
						t.Fatalf("two calls of Contracts share the VocabAt of %s (%s)", sendapitest.Key(c), n)
					}
				}
			}
		})
	}
	// control: the table is checked against the code, not copied from it: module 3 has no reason in it, module 2 has, and they differ
	if maps.Equal(want(true)["GET /v1/report | campaign page"], want(false)["GET /v1/report | campaign page"]) {
		t.Fatal("the two tables do not differ where the profiles do")
	}
}
