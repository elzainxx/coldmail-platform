package m3_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/contractcheck"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// A vocabulary names a closed set and says, by VocabAt, which position of the response it constrains; one that points at nothing in its own fixture constrains nothing in the live gate and
// cannot be told from a mistyped path. contractcheck's no-rot check holds every contract of the package to its recorded file: each Vocab has its VocabAt, the path is of the grammar, the
// fixture holds a string there (or an array on the way is empty, a note), every value the fixture holds there is in the set, no set is empty or repeats a word, and no VocabAt names a
// set that is not in Vocab. The controls break one contract each way and require the same check to say so, naming the vocabulary: a path that does not exist, a path that exists and holds no
// string, a set the fixture's own value is not in, and a VocabAt without its vocabulary.
func TestEveryVocabularyConstrainsSomethingInItsOwnFixture(t *testing.T) {
	cs := m3.Contracts()
	rep := contractcheck.InspectFixtures(cs, m3.ContractFixtures(), "testdata/contract")
	for _, p := range rep.Problems {
		t.Errorf("%s", p)
	}
	for _, n := range rep.Notes {
		t.Logf("note: %s", n)
	}
	// the subject is not empty: M3-only contracts carry vocabularies and every one of them has its path
	var withVocab, vocabularies int
	for _, c := range cs {
		for name := range c.Vocab {
			vocabularies++
			if _, ok := c.VocabAt[name]; !ok {
				t.Errorf("%s | %s: the vocabulary %s has no VocabAt, so it would be read as a path itself", c.Route, c.Case, name)
			}
		}
		for name := range c.VocabAt {
			if _, ok := c.Vocab[name]; !ok {
				t.Errorf("%s | %s: VocabAt names %s, which is no vocabulary of the contract", c.Route, c.Case, name)
			}
		}
		if len(c.Vocab) > 0 {
			withVocab++
		}
	}
	if withVocab < 20 || vocabularies < 30 {
		t.Fatalf("%d contracts hold %d vocabularies: the subject of the check shrank", withVocab, vocabularies)
	}

	// controls: each breakage is reported, naming the vocabulary
	byCase := func(route, name string) int {
		for i, c := range cs {
			if c.Route == route && c.Case == name {
				return i
			}
		}
		t.Fatalf("no contract %s | %s", route, name)
		return -1
	}
	broken := func(route, name string, f func(c *modclient.RouteContract)) []string {
		cc := make([]modclient.RouteContract, len(cs))
		for i, c := range cs {
			cc[i] = c
			cc[i].Vocab = cloneVocab(c.Vocab)
			cc[i].VocabAt = cloneAt(c.VocabAt)
		}
		f(&cc[byCase(route, name)])
		var out []string
		for _, p := range contractcheck.CheckFixtures(cc, m3.ContractFixtures(), "testdata/contract") {
			out = append(out, p.String())
		}
		return out
	}
	has := func(problems []string, kind, vocab string) bool {
		for _, p := range problems {
			if strings.Contains(p, "kind="+kind) && strings.Contains(p, vocab) {
				return true
			}
		}
		return false
	}
	// a path that does not exist (a typo)
	if p := broken(m3.RouteAccountsGet, "ok", func(c *modclient.RouteContract) { c.VocabAt["credential_verification_state"] = "verification.stat" }); !has(p, "VocabInert", "credential_verification_state") {
		t.Fatalf("a mistyped path was not reported: %v", p)
	}
	// a path that exists and holds no string (the whole object)
	if p := broken(m3.RouteAccountsGet, "ok", func(c *modclient.RouteContract) { c.VocabAt["from_verification_state"] = "status" }); !has(p, "VocabInert", "from_verification_state") {
		t.Fatalf("a path with no string was not reported: %v", p)
	}
	// a set the fixture's own value is not in
	if p := broken(m3.RouteAccountsGet, "ok", func(c *modclient.RouteContract) {
		c.Vocab["credential_verification_state"] = slices.DeleteFunc(slices.Clone(c.Vocab["credential_verification_state"]), func(s string) bool { return s == "verified" })
	}); !has(p, "VocabOutside", "credential_verification_state") {
		t.Fatalf("a value outside the set was not reported: %v", p)
	}
	// a VocabAt for a vocabulary the contract does not have
	if p := broken(m3.RouteAccountsGet, "ok", func(c *modclient.RouteContract) { c.VocabAt["nothing_at_all"] = "verification.state" }); !has(p, "VocabKey", "nothing_at_all") {
		t.Fatalf("a VocabAt without its vocabulary was not reported: %v", p)
	}
	// a set attached to a position the fixture leaves empty (the quote that is feasible names no reason): an array on the way that is empty is a note, a key that is absent is not
	if p := broken(m3.RouteQuote, "feasible", func(c *modclient.RouteContract) {
		c.Vocab = map[string][]string{"quote_reason_code": {"exceeds_queue_cap"}}
		c.VocabAt = map[string]string{"quote_reason_code": "reason[].code"}
	}); !has(p, "VocabInert", "quote_reason_code") {
		t.Fatalf("a position the fixture lacks was not reported: %v", p)
	}
	// and the unbroken contracts are clean (the control of every control above)
	if p := broken(m3.RouteAccountsGet, "ok", func(c *modclient.RouteContract) {}); len(p) != 0 {
		t.Fatalf("the unbroken contracts are not clean: %v", p)
	}
}

func cloneVocab(v map[string][]string) map[string][]string {
	if v == nil {
		return nil
	}
	out := make(map[string][]string, len(v))
	for k, s := range v {
		out[k] = slices.Clone(s)
	}
	return out
}

func cloneAt(v map[string]string) map[string]string {
	out := make(map[string]string, len(v)+1)
	for k, s := range v {
		out[k] = s
	}
	return out
}
