package m1

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/coldmail-platform/platform/internal/contractcheck"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// ---- an independent reading of VocabAt -------------------------------------------------------------------------------------------------------------------
//
// The drift gate reads a vocabulary's position from RouteContract.VocabAt (a bare set name is read as a path to a top-level key, which most bodies do not have, so the gate was
// inert). These helpers read the same grammar (a.b is a key, x[] is every element of the array x) with encoding/json into generic values, sharing no code with contractcheck, so that
// the package can say for itself, today, whether each vocabulary constrains something in its own fixture.

type pathSeg struct {
	key  string
	each bool // x[]: every element of the array under key
}

func parseVocabPath(p string) ([]pathSeg, bool) {
	if p == "" {
		return nil, false
	}
	var segs []pathSeg
	for _, part := range strings.Split(p, ".") {
		s := pathSeg{key: part}
		if strings.HasSuffix(part, "[]") {
			s = pathSeg{key: strings.TrimSuffix(part, "[]"), each: true}
		}
		if s.key == "" || strings.ContainsAny(s.key, "[]{}") {
			return nil, false
		}
		segs = append(segs, s)
	}
	return segs, true
}

// stringsAtPath collects the string values found at the path of a generic JSON value: a key that is absent, a value that is not an object where a key is wanted, an array that is
// empty, and a leaf that is not a string all contribute nothing.
func stringsAtPath(v any, segs []pathSeg, out *[]string) {
	if len(segs) == 0 {
		if s, ok := v.(string); ok {
			*out = append(*out, s)
		}
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	next, ok := m[segs[0].key]
	if !ok {
		return
	}
	if !segs[0].each {
		stringsAtPath(next, segs[1:], out)
		return
	}
	arr, ok := next.([]any)
	if !ok {
		return
	}
	for _, e := range arr {
		stringsAtPath(e, segs[1:], out)
	}
}

// vocabProblems says what is wrong with the vocabularies of cs against the fixtures of dir in fsys, each problem naming the vocabulary: a set with no path, a path for no set, a path
// that is not of the grammar, a path at which the fixture holds NO string (the set would constrain nothing), a string the set does not list, an empty set.
func vocabProblems(cs []modclient.RouteContract, fsys fs.FS, dir string) []string {
	var out []string
	for _, c := range cs {
		who := fmt.Sprintf("%s case=%s", c.Route, c.Case)
		data, err := fs.ReadFile(fsys, path.Join(dir, c.Fixture))
		if err != nil {
			out = append(out, fmt.Sprintf("%s: the fixture %s cannot be read", who, c.Fixture))
			continue
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			out = append(out, fmt.Sprintf("%s: the fixture %s is not JSON", who, c.Fixture))
			continue
		}
		for name := range c.VocabAt {
			if _, ok := c.Vocab[name]; !ok {
				out = append(out, fmt.Sprintf("%s: VocabAt has a path for %q, which is not in Vocab", who, name))
			}
		}
		for name, list := range c.Vocab {
			at, ok := c.VocabAt[name]
			if !ok {
				out = append(out, fmt.Sprintf("%s: the vocabulary %q has no VocabAt path (a bare name is read as a top-level key)", who, name))
				continue
			}
			segs, ok := parseVocabPath(at)
			if !ok {
				out = append(out, fmt.Sprintf("%s: the vocabulary %q has a path that is not of the grammar", who, name))
				continue
			}
			if len(list) == 0 {
				out = append(out, fmt.Sprintf("%s: the vocabulary %q is empty", who, name))
			}
			var vals []string
			stringsAtPath(doc, segs, &vals)
			if len(vals) == 0 {
				out = append(out, fmt.Sprintf("%s: the vocabulary %q constrains nothing: its fixture has no string at %q", who, name, at))
			}
			listed := map[string]bool{}
			for _, v := range list {
				listed[v] = true
			}
			for _, v := range vals {
				if !listed[v] {
					out = append(out, fmt.Sprintf("%s: the vocabulary %q does not list %q, which its fixture holds at %q", who, name, v, at))
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// checkerReadsVocabAt says whether contractcheck.CheckFixtures reads RouteContract.VocabAt: it gives a contract whose set name is NOT a path of the fixture but whose VocabAt is, and
// takes silence for yes. A checker that predates VocabAt reads the bare name, finds no such key and reports it.
func checkerReadsVocabAt() bool {
	fsys := fstest.MapFS{"d/f.json": {Data: []byte(`{"state":"ok"}`)}}
	rc := modclient.RouteContract{
		Module: modclient.M1, Route: "GET /probe", Fixture: "f.json", Status: 200,
		Shape: modclient.Object(modclient.Req("state", modclient.Str)),
		Vocab: map[string][]string{"probe_set": {"ok"}}, VocabAt: map[string]string{"probe_set": "state"},
	}
	return len(contractcheck.CheckFixtures([]modclient.RouteContract{rc}, fsys, "d")) == 0
}

// bareNameIsAPosition says whether name, read as a path, holds a string in c's fixture.
func bareNameIsAPosition(t *testing.T, c modclient.RouteContract, name string, fsys fs.FS, dir string) bool {
	t.Helper()
	data, err := fs.ReadFile(fsys, path.Join(dir, c.Fixture))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	segs, ok := parseVocabPath(name)
	if !ok {
		return false
	}
	var vals []string
	stringsAtPath(doc, segs, &vals)
	return len(vals) > 0
}

func naming(problems []contractcheck.Problem, name string) []contractcheck.Problem {
	var out []contractcheck.Problem
	for _, p := range problems {
		if strings.Contains(p.String(), name) {
			out = append(out, p)
		}
	}
	return out
}

func problemTexts(ps []contractcheck.Problem) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

// withVocabAt is a copy of cs in which every vocabulary of every contract has the path edit makes of it (an empty result deletes the entry).
func withVocabAt(cs []modclient.RouteContract, edit func(c modclient.RouteContract, name, at string) string) []modclient.RouteContract {
	out := make([]modclient.RouteContract, len(cs))
	for i, c := range cs {
		c.VocabAt = map[string]string{}
		for name, at := range cs[i].VocabAt {
			if p := edit(cs[i], name, at); p != "" {
				c.VocabAt[name] = p
			}
		}
		out[i] = c
	}
	return out
}

// TestEveryVocabularyConstrainsSomethingInItsOwnFixture: every vocabulary of every contract has a VocabAt path, the path resolves to at least one string in the contract's own
// recorded fixture, and every string there is in the set: a vocabulary that constrains nothing in its fixture would be inert in the live gate, and a bare name was exactly that. Two
// judges read it. The package's own reading (vocabProblems: encoding/json into generic values, sharing no code with contractcheck) and contractcheck.CheckFixtures, the gate's no-rot
// check, which must read VocabAt (checkerReadsVocabAt probes that with a synthetic contract, so a checker that predates it fails the test instead of passing it for the wrong reason)
// and report zero problems for Contracts() and ContractFixtures(). Paired controls, each on a copy of the contracts with the paths damaged: a path with a letter added, a path to an
// object where a string is wanted, a path that is deleted, a path that is not of the grammar, a trailing dot, an unclosed span, a row set on a page that has no row, and a set that
// omits the value its fixture holds each make BOTH judges name the vocabulary. The one exception is stated: with the path deleted a set is read as a path by its own name, and
// where that name is itself a string position (the verdict's status, reason and recommendation) the gate rightly reports nothing, so only the package's own reading is asked.
func TestEveryVocabularyConstrainsSomethingInItsOwnFixture(t *testing.T) {
	cs, fsys, dir := Contracts(), ContractFixtures(), "testdata/contract"

	// the subject is not empty
	pairs, withVocab := 0, 0
	for _, c := range cs {
		pairs += len(c.Vocab)
		if len(c.Vocab) > 0 {
			withVocab++
		}
	}
	if pairs < 10 || withVocab < 5 {
		t.Fatalf("%d vocabularies on %d contracts: the instrument matched too little", pairs, withVocab)
	}

	// 1. the package's own reading
	if p := vocabProblems(cs, fsys, dir); len(p) != 0 {
		t.Fatalf("a vocabulary does not constrain what it claims in its own fixture:\n%s", strings.Join(p, "\n"))
	}

	// 2. the gate's no-rot check, which must read VocabAt: a checker that predates it reads each bare set name as a top-level key and its verdict is not the live gate's
	if !checkerReadsVocabAt() {
		t.Fatal("contractcheck.CheckFixtures does not read RouteContract.VocabAt: its verdict on the vocabularies of Contracts() is not the live gate's")
	}
	if ps := contractcheck.CheckFixtures(cs, fsys, dir); len(ps) != 0 {
		t.Fatalf("CheckFixtures reports %d problem(s):\n%s", len(ps), strings.Join(problemTexts(ps), "\n"))
	}

	// 3. controls: damaged paths
	object := func(c modclient.RouteContract, _, at string) string { // a key that holds an object or an array in the fixture, never a string
		switch {
		case strings.HasPrefix(at, "error."):
			return "error"
		case c.Route == tplVerify:
			return "signals"
		case c.Route == tplResults:
			return "results"
		}
		return "counts"
	}
	damage := map[string]func(c modclient.RouteContract, name, at string) string{
		"a letter added to the path":   func(_ modclient.RouteContract, _, at string) string { return at + "x" },
		"a path to an object":          object,
		"a path that is deleted":       func(modclient.RouteContract, string, string) string { return "" },
		"a path that is not a path":    func(modclient.RouteContract, string, string) string { return "a..b" },
		"a path with a trailing dot":   func(_ modclient.RouteContract, _, at string) string { return at + "." },
		"a path with an unclosed span": func(_ modclient.RouteContract, _, at string) string { return at + "[" },
	}
	coincidences := 0 // (contract, set) pairs whose bare name is itself a string position, which the "deleted path" control cannot damage
	names := make([]string, 0, len(damage))
	for n := range damage {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, how := range names {
		bad := withVocabAt(cs, damage[how])
		own := vocabProblems(bad, fsys, dir)
		for _, c := range cs {
			for name := range c.Vocab {
				found := false
				for _, p := range own {
					if strings.Contains(p, fmt.Sprintf("%s case=%s: ", c.Route, c.Case)) && strings.Contains(p, fmt.Sprintf("%q", name)) {
						found = true
					}
				}
				if !found {
					t.Errorf("control %q: the package's own reading does not name the vocabulary %q of %s case=%s", how, name, c.Route, c.Case)
				}
			}
		}
		theirs := contractcheck.CheckFixtures(bad, fsys, dir)
		for _, c := range cs {
			for name := range c.Vocab {
				var mine []contractcheck.Problem
				for _, p := range naming(theirs, name) {
					if p.Route == c.Route && p.Case == c.Case {
						mine = append(mine, p)
					}
				}
				if len(mine) == 0 && how == "a path that is deleted" && bareNameIsAPosition(t, c, name, fsys, dir) {
					// a set with no VocabAt path is read as a path by its own name (RouteContract.VocabAt), and here the name IS a key that holds a string (the verdict's own
					// `status`, `reason` and `recommendation`), so the gate has nothing to report: this is the contract's own explicit VocabAt entry doing its job, not a gap
					coincidences++
					continue
				}
				if len(mine) == 0 {
					t.Errorf("control %q: CheckFixtures reports no problem that names %q on %s case=%s", how, name, c.Route, c.Case)
				}
			}
		}
	}

	if coincidences == 0 {
		t.Error("control: the deleted-path control found no set whose bare name is a position; the exemption it carries is dead code")
	}

	// control: a set on a page that has no row to hold it. The results page the real node answered under case `empty_page` has no row, so a status set at results[].status
	// constrains nothing there (this is why that contract lists no row set), and a gate that took the listing at its word would be inert on it.
	var emptyPage []modclient.RouteContract
	for _, c := range cs {
		if c.Route == tplResults && c.Case == "empty_page" {
			c.Vocab = map[string][]string{"status": vocabSets()["status"]}
			c.VocabAt = map[string]string{"status": "results[].status"}
			emptyPage = append(emptyPage, c)
		}
	}
	if len(emptyPage) != 1 || len(recordedRows(t, emptyPage[0].Fixture)) != 0 {
		t.Fatalf("control setup: the contract of the empty results page was not found or has rows")
	}
	if own := vocabProblems(emptyPage, fsys, dir); !containsSub(own, `the vocabulary "status" constrains nothing`) {
		t.Errorf("control: a row set on a page with no row was not reported by the package's own reading: %v", own)
	}
	if theirs := contractcheck.CheckFixtures(emptyPage, fsys, dir); len(naming(theirs, "status")) == 0 {
		t.Errorf("control: CheckFixtures reports nothing about a row set on a page with no row")
	}

	// control: a set that leaves out the value its fixture holds (the first value found at its path is dropped from the list)
	short := make([]modclient.RouteContract, len(cs))
	for i, c := range cs {
		c.Vocab = map[string][]string{}
		for name, list := range cs[i].Vocab {
			data, err := fs.ReadFile(fsys, path.Join(dir, c.Fixture))
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			segs, _ := parseVocabPath(cs[i].VocabAt[name])
			var vals []string
			stringsAtPath(doc, segs, &vals)
			drop := ""
			if len(vals) > 0 {
				drop = vals[0]
			}
			var kept []string
			for _, v := range list {
				if v != drop {
					kept = append(kept, v)
				}
			}
			c.Vocab[name] = kept
		}
		short[i] = c
	}
	own := vocabProblems(short, fsys, dir)
	for _, c := range cs {
		for name := range c.Vocab {
			if !containsSub(own, fmt.Sprintf("%s case=%s: the vocabulary %q does not list", c.Route, c.Case, name)) {
				t.Errorf("control: a set without its fixture's value was not named: %q on %s case=%s", name, c.Route, c.Case)
			}
		}
	}
	theirs := contractcheck.CheckFixtures(short, fsys, dir)
	for _, c := range cs {
		for name := range c.Vocab {
			if len(naming(theirs, name)) == 0 {
				t.Errorf("control: CheckFixtures reports nothing that names %q (its fixture's value is not in the set)", name)
			}
		}
	}

	// control: the contracts themselves are clean (so every problem above came from the damage)
	if p := vocabProblems(cs, fsys, dir); len(p) != 0 {
		t.Fatalf("the undamaged contracts report %v", p)
	}
	t.Logf("%d vocabularies on %d contracts, each with a VocabAt path that holds a string in its fixture, and %d set(s) whose bare name coincides with a position (not damageable by deleting the path)", pairs, withVocab, coincidences)
}
