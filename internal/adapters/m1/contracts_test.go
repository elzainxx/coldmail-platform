package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// ---- what each contract is called with ---------------------------------------------------------------------------------------------------------------------

// contractRun serves body with status from a stand-in and calls the adapter's own method for the route, returning a one-line summary of what it decoded and its error.
type contractRun func(t *testing.T, status int, body string) (summary string, err error)

type contractExpect struct {
	run       contractRun
	summarize func(t *testing.T, body string) string // success: what the recorded body says, read WITHOUT the adapter (a generic JSON parse), in the format run returns
	kind      modclient.Kind                         // error: the class the adapter must give it (0 for a success)
	code      string                                 // error: the module's code the typed error must carry
}

func runVerify(t *testing.T, status int, body string) (string, error) {
	v, err := serve(t, status, body).adapter(t).Verify(testCtx(t), "ada@acme.example")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s %d %s %s", v.Status, v.Reason, v.Confidence, v.Recommendation, v.Domain), nil
}

func runSubmit(t *testing.T, status int, body string) (string, error) {
	id, c, err := serve(t, status, body).adapter(t).Submit(testCtx(t), threeRowPlan())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %d/%d/%d/%d", id, c.Total, c.Enqueued, c.Duplicate, c.Malformed), nil
}

// runSubmitRaw is the call the recorded 202 is decoded with: the recording is that of a raw submission (two of its three rows malformed), which a plan never sends and
// Submit would refuse as a count that disagrees with its prediction (TestSubmit_ACountThatDisagreesWithThePlanIsAContractError).
func runSubmitRaw(t *testing.T, status int, body string) (string, error) {
	id, c, err := serve(t, status, body).adapter(t).SubmitRaw(testCtx(t), []Item{{"a", "ada@acme.example"}, {"b", "bob@acme.example"}, {"c", "cyd@acme.example"}})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %d/%d/%d/%d", id, c.Total, c.Enqueued, c.Duplicate, c.Malformed), nil
}

func runStatus(t *testing.T, status int, body string) (string, error) {
	js, err := serve(t, status, body).adapter(t).Status(testCtx(t), testJob)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s complete=%t %d/%d/%d paused=%t", js.Status, js.Complete, js.Counts.Total, js.Counts.Settled, js.Counts.Pending, js.ProcessingPaused), nil
}

func runResults(t *testing.T, status int, body string) (string, error) {
	rows, next, err := serve(t, status, body).adapter(t).Results(testCtx(t), testJob, "", 0)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d rows next=%q", len(rows), next), nil
}

// The summaries below read a body with encoding/json into generic maps, so they share no code with the adapter's decoding.
func docOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func verifySummary(t *testing.T, body string) string {
	d := docOf(t, body)
	return fmt.Sprintf("%s/%s %d %s %s", d["status"], d["reason"], num(t, d["confidence"]), d["recommendation"], d["domain"])
}

func submitSummary(t *testing.T, body string) string {
	d := docOf(t, body)
	c := obj(t, d["counts"])
	return fmt.Sprintf("%s %d/%d/%d/%d", d["job_id"], num(t, c["total"]), num(t, c["enqueued"]), num(t, c["duplicate"]), num(t, c["malformed"]))
}

func statusSummary(t *testing.T, body string) string {
	d := docOf(t, body)
	c := obj(t, d["counts"])
	return fmt.Sprintf("%s complete=%t %d/%d/%d paused=%t", d["status"], d["complete"], num(t, c["total"]), num(t, c["settled"]), num(t, c["pending"]), d["processing_paused"])
}

func resultsSummary(t *testing.T, body string) string {
	d := docOf(t, body)
	return fmt.Sprintf("%d rows next=%q", len(d["results"].([]any)), d["next_cursor"])
}

func contractKey(route, c string) string { return route + " | " + c }

// contractTable is what every entry of Contracts() is exercised with. A contract with no entry here, and an entry with no contract, are both red (TestContracts_EveryFixtureDecodes...).
func contractTable() map[string]contractExpect {
	return map[string]contractExpect{
		contractKey(tplVerify, "ok"):                {run: runVerify, summarize: verifySummary},
		contractKey(tplSubmit, "accepted"):          {run: runSubmitRaw, summarize: submitSummary},
		contractKey(tplSubmit, "validation_failed"): {run: runSubmit, kind: modclient.Refused, code: "validation_failed"},
		contractKey(tplSubmit, "unauthenticated"):   {run: runSubmit, kind: modclient.Refused, code: "unauthenticated"},
		contractKey(tplSubmit, "payload_too_large"): {run: runSubmit, kind: modclient.Refused, code: "payload_too_large"},
		contractKey(tplSubmit, "paused"):            {run: runSubmit, kind: modclient.Paused, code: "paused"},
		contractKey(tplSubmit, "unavailable"):       {run: runSubmit, kind: modclient.Unavailable, code: "unavailable"},
		contractKey(tplSubmit, "internal_error"):    {run: runSubmit, kind: modclient.Internal, code: "internal_error"},
		contractKey(tplStatus, "running"):           {run: runStatus, summarize: statusSummary},
		contractKey(tplStatus, "complete"):          {run: runStatus, summarize: statusSummary},
		contractKey(tplStatus, "not_found"):         {run: runStatus, kind: modclient.NotFound, code: "not_found"},
		contractKey(tplStatus, "unauthenticated"):   {run: runStatus, kind: modclient.Refused, code: "unauthenticated"},
		contractKey(tplResults, "page"):             {run: runResults, summarize: resultsSummary},
		contractKey(tplResults, "empty_page"):       {run: runResults, summarize: resultsSummary},
		contractKey(tplResults, "bad_cursor"):       {run: runResults, kind: modclient.Refused, code: "validation_failed"},
		contractKey(tplResults, "unauthenticated"):  {run: runResults, kind: modclient.Refused, code: "unauthenticated"},
		contractKey(tplResults, "not_found"):        {run: runResults, kind: modclient.NotFound, code: "not_found"},
	}
}

// TestContracts_EveryFixtureDecodesThroughTheAdaptersRealPath: each contract's recorded body, served by a stand-in with the recorded status, goes through the adapter's own method for
// its route: a success decodes to what the fixture says (a summary of the decoded values, so a decoder that returned zeros would be caught), an error envelope becomes the
// modclient error of the class and code the adapter expects. The table of calls and Contracts() must cover each other exactly. Paired control: the same call over a body of the
// wrong shape fails for every success entry, so the "decodes" is a property the stand-in could have broken.
func TestContracts_EveryFixtureDecodesThroughTheAdaptersRealPath(t *testing.T) {
	cs := Contracts()
	table := contractTable()
	if len(cs) == 0 {
		t.Fatal("Contracts() is empty: the gate's subject is empty")
	}
	seen := map[string]bool{}
	for _, c := range cs {
		key := contractKey(c.Route, c.Case)
		seen[key] = true
		exp, ok := table[key]
		if !ok {
			t.Errorf("contract %q has no entry in the test's table: nothing exercises it", key)
			continue
		}
		body := fixture(t, c.Fixture)
		summary, err := exp.run(t, c.Status, body)
		if exp.kind == 0 {
			want := exp.summarize(t, body)
			if err != nil || summary != want || summary == "" {
				t.Errorf("%s: got %q, %v, want what the file says: %q", key, summary, err, want)
			}
			// control: the wrong shape is refused by the same call
			if _, cerr := exp.run(t, c.Status, `{"unexpected":true}`); modclient.KindOf(cerr) != modclient.Contract {
				t.Errorf("%s control: a body of the wrong shape gave %v, want a Contract error", key, cerr)
			}
			continue
		}
		e := asModclient(err)
		if e == nil || e.Kind != exp.kind || e.Code != exp.code || e.Status != c.Status || !e.Enveloped {
			t.Errorf("%s: got %v, want a %v error with code %q and status %d decoded from the envelope", key, err, exp.kind, exp.code, c.Status)
		}
	}
	for key := range table {
		if !seen[key] {
			t.Errorf("the test's table has %q and Contracts() does not: a dangling expectation", key)
		}
	}
	t.Logf("%d contract(s) exercised: %d success, %d error envelope(s); routes NOT covered: %v", len(cs), countKind(table, false), countKind(table, true), UncoveredRoutes())
}

func countKind(table map[string]contractExpect, errors bool) int {
	n := 0
	for _, e := range table {
		if (e.kind != 0) == errors {
			n++
		}
	}
	return n
}

// TestContracts_EveryShapeChecksSomethingAndEveryFixtureConformsToIt: each entry's recorded body conforms to its own Shape (the keys the adapter reads are really there), and a Shape
// is not the zero Shape (which checks nothing): an empty object breaks every one of them. Also: each entry names a module, a route, a status, a fixture, and a (route, case) pair
// that is unique; each Vocab set listed is non-empty.
func TestContracts_EveryShapeChecksSomethingAndEveryFixtureConformsToIt(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Contracts() {
		key := contractKey(c.Route, c.Case)
		if c.Module != modclient.M1 || c.Route == "" || c.Fixture == "" || c.Status == 0 {
			t.Errorf("%s: an entry is missing its module, route, fixture or status: %+v", key, c)
		}
		if seen[key] {
			t.Errorf("%s: two contracts share a route and case", key)
		}
		seen[key] = true
		b := []byte(fixture(t, c.Fixture))
		if err := modclient.Decode(&modclient.Response{Status: c.Status, Body: b}, nil, c.Shape); err != nil {
			t.Errorf("%s: the recorded body does not conform to its own shape: %v", key, err)
		}
		if err := modclient.Decode(&modclient.Response{Status: c.Status, Body: []byte(`{}`)}, nil, c.Shape); modclient.KindOf(err) != modclient.Contract {
			t.Errorf("%s: an empty object conforms to the shape (%v): it checks nothing", key, err)
		}
		for name, vals := range c.Vocab {
			if len(vals) == 0 {
				t.Errorf("%s: the vocabulary %q is empty", key, name)
			}
		}
	}
}

// ---- the fixture directory and the contracts name each other --------------------------------------------------------------------------------------------------

// checkContractFiles says what is wrong between the contracts and the .json files under dir: a contract that names no file, a name two contracts share, a file no contract names.
func checkContractFiles(dir string, cs []modclient.RouteContract) []string {
	var problems []string
	named := map[string]int{}
	for _, c := range cs {
		if c.Fixture == "" {
			problems = append(problems, fmt.Sprintf("contract %q names no fixture", contractKey(c.Route, c.Case)))
			continue
		}
		named[c.Fixture]++
		if _, err := os.Stat(filepath.Join(dir, c.Fixture)); err != nil {
			problems = append(problems, fmt.Sprintf("contract %q names %s, which does not exist", contractKey(c.Route, c.Case), c.Fixture))
		}
	}
	for name, n := range named {
		if n > 1 {
			problems = append(problems, fmt.Sprintf("%s is named by %d contracts", name, n))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return append(problems, "cannot read the fixture directory: "+err.Error())
	}
	files := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files++
		if named[e.Name()] == 0 {
			problems = append(problems, fmt.Sprintf("%s is named by no contract (a stray or rotten fixture)", e.Name()))
		}
	}
	if files == 0 {
		problems = append(problems, "found 0 fixture files: the instrument matched nothing")
	}
	sort.Strings(problems)
	return problems
}

// TestContracts_NoFixtureRotsAndNoContractDangles: every file of testdata/contract is named by exactly one contract and every contract names a file that exists. Paired controls on a
// scratch copy: a stray file is caught, a dangling name is caught, a name two contracts share is caught, and an empty directory is red.
func TestContracts_NoFixtureRotsAndNoContractDangles(t *testing.T) {
	dir := filepath.Join("testdata", "contract")
	cs := Contracts()
	if problems := checkContractFiles(dir, cs); len(problems) != 0 {
		t.Fatalf("%v", problems)
	}

	scratch := t.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scratch, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if p := checkContractFiles(scratch, cs); len(p) != 0 {
		t.Fatalf("control: the untouched copy is not clean: %v", p)
	}
	if err := os.WriteFile(filepath.Join(scratch, "stray.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := checkContractFiles(scratch, cs); len(p) != 1 || !strings.Contains(p[0], "stray.json is named by no contract") {
		t.Fatalf("a stray file: got %v, want exactly the stray file named", p)
	}
	_ = os.Remove(filepath.Join(scratch, "stray.json"))

	dangling := append([]modclient.RouteContract(nil), cs...)
	dangling[0].Fixture = "no-such-file.json"
	p := checkContractFiles(scratch, dangling)
	var sawDangling, sawOrphanFile bool
	for _, s := range p {
		sawDangling = sawDangling || strings.Contains(s, "no-such-file.json, which does not exist")
		sawOrphanFile = sawOrphanFile || strings.Contains(s, cs[0].Fixture+" is named by no contract")
	}
	if !sawDangling || !sawOrphanFile {
		t.Fatalf("a dangling name: got %v, want the missing file and the now-orphaned file both named", p)
	}

	shared := append([]modclient.RouteContract(nil), cs...)
	shared[1].Fixture = cs[0].Fixture
	if p := checkContractFiles(scratch, shared); !containsSub(p, "is named by 2 contracts") {
		t.Fatalf("a shared name: got %v", p)
	}
	if p := checkContractFiles(t.TempDir(), cs); !containsSub(p, "found 0 fixture files") {
		t.Fatalf("an empty directory: got %v, want the empty-subject refusal", p)
	}
}

func containsSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---- mutated fixtures are refused, naming the key ---------------------------------------------------------------------------------------------------------

type pathStep struct {
	key   string
	idx   int
	isIdx bool
}

// parsePath reads "results[1].signals.failure_origin" into steps.
func parsePath(p string) []pathStep {
	var steps []pathStep
	for _, part := range strings.Split(p, ".") {
		name := part
		var idxs []int
		for strings.HasSuffix(name, "]") {
			i := strings.LastIndex(name, "[")
			n, err := strconv.Atoi(name[i+1 : len(name)-1])
			if i < 0 || err != nil {
				break
			}
			idxs = append([]int{n}, idxs...)
			name = name[:i]
		}
		if name != "" {
			steps = append(steps, pathStep{key: name})
		}
		for _, n := range idxs {
			steps = append(steps, pathStep{idx: n, isIdx: true})
		}
	}
	return steps
}

func decodeDoc(t *testing.T, body string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func encodeDoc(t *testing.T, doc any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(doc); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// edit applies op to the container that holds the last step of path. A mutation that touches nothing proves nothing, so it is an error when the path is not what the mode says:
// mode "exist" needs the key (or element) to be in the document, mode "add" needs the parent to be an object that does NOT have the key yet.
func edit(doc any, path, mode string, op func(container any, last pathStep)) error {
	steps := parsePath(path)
	cur := doc
	for _, s := range steps[:len(steps)-1] {
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[s.key]
			if s.isIdx || !ok {
				return fmt.Errorf("%s: no key %q", path, s.key)
			}
			cur = next
		case []any:
			if !s.isIdx || s.idx >= len(c) {
				return fmt.Errorf("%s: no element %d", path, s.idx)
			}
			cur = c[s.idx]
		default:
			return fmt.Errorf("%s: cannot step into a scalar", path)
		}
	}
	last := steps[len(steps)-1]
	switch c := cur.(type) {
	case map[string]any:
		_, has := c[last.key]
		switch {
		case last.isIdx:
			return fmt.Errorf("%s: the parent is an object, not an array", path)
		case mode == "add" && has:
			return fmt.Errorf("%s: the document already has that key", path)
		case mode != "add" && !has:
			return fmt.Errorf("%s: the document has no such key", path)
		}
	case []any:
		if mode == "add" || !last.isIdx || last.idx >= len(c) {
			return fmt.Errorf("%s: the document has no such element", path)
		}
	default:
		return fmt.Errorf("%s: the parent is a scalar", path)
	}
	op(cur, last)
	return nil
}

type mutation struct {
	name string
	path string
	op   string // remove, set, add
	to   any    // the value for set and add
	key  string // the key the Contract error must name
}

func remove(path string) mutation {
	return mutation{name: "remove " + path, path: path, op: "remove", key: path}
}
func retype(path string, to any) mutation {
	return mutation{name: fmt.Sprintf("retype %s to %v", path, to), path: path, op: "set", to: to, key: path}
}
func outside(path string, to any) mutation {
	return mutation{name: fmt.Sprintf("set %s outside its vocabulary (%v)", path, to), path: path, op: "set", to: to, key: path}
}

// add puts a key the recording does not have (a vocabulary-bearing key of the open signals bag, say) into an object that exists.
func add(path string, to any) mutation {
	return mutation{name: fmt.Sprintf("add %s = %v", path, to), path: path, op: "add", to: to, key: path}
}

// naming says the error is expected to name another key than the mutated path (replacing an array by one whose element is wrong is refused at the element).
func (m mutation) naming(key string) mutation { m.key = key; return m }

func (m mutation) apply(t *testing.T, body string) string {
	t.Helper()
	doc := decodeDoc(t, body)
	mode := "exist"
	if m.op == "add" {
		mode = "add"
	}
	err := edit(doc, m.path, mode, func(container any, last pathStep) {
		switch c := container.(type) {
		case map[string]any:
			if m.op == "remove" {
				delete(c, last.key)
			} else {
				c[last.key] = m.to
			}
		case []any:
			c[last.idx] = m.to
		}
	})
	if err != nil {
		t.Fatalf("%s: the mutation does not apply to the recorded body (it would prove nothing): %v", m.name, err)
	}
	return encodeDoc(t, doc)
}

// rowMutations are the mutations of a recorded results page that HAS rows, written against its own first, middle and last rows and against its first row that repeats an address
// (found by the independent parse): the indexes are the page's, not a literal, so a longer or shorter recording keeps them meaningful. It fails the test when the page has no
// row that repeats an address, because then the duplicate_of mutations would not apply and the instrument would cover less than it claims.
func rowMutations(t *testing.T, name string) []mutation {
	t.Helper()
	rows := recordedRows(t, name)
	if len(rows) < 3 {
		t.Fatalf("%s has %d row(s): too few to mutate first, middle and last", name, len(rows))
	}
	dup := -1
	for i, r := range rows {
		if _, ok := r["duplicate_of"]; ok {
			dup = i
			break
		}
	}
	if dup < 0 {
		t.Fatalf("%s has no row with duplicate_of: its mutations would not apply", name)
	}
	first, mid, last := 0, len(rows)/2, len(rows)-1
	at := func(i int, key string) string { return fmt.Sprintf("results[%d].%s", i, key) }
	return []mutation{
		remove("job_id"), remove("results"), retype("results", map[string]any{}), retype("results", nil), remove("next_cursor"), retype("next_cursor", 4),
		remove(at(first, "ordinal")), retype(at(mid, "ordinal"), "1"), retype(at(mid, "ordinal"), -1),
		remove(at(last, "item_id")), retype(at(last, "item_id"), ""), retype(at(last, "item_id"), 3),
		remove(at(first, "status")), outside(at(first, "status"), "maybe"),
		remove(at(mid, "reason")), outside(at(mid, "reason"), "vibes"),
		remove(at(last, "flags")), remove(at(last, "flags.from_cache")), retype(at(last, "flags.from_cache"), "false"),
		remove(at(first, "signals")), retype(at(mid, "signals"), 3),
		add(at(mid, "signals.failure_origin"), "theirs"), add(at(mid, "signals.failure_origin"), 1), add(at(mid, "signals.smtp_skipped_reason"), "lazy"),
		outside(at(dup, "duplicate_of"), num(t, rows[dup]["ordinal"])), outside(at(dup, "duplicate_of"), -1), retype(at(dup, "duplicate_of"), "0"),
		retype(fmt.Sprintf("results[%d]", first), []any{1}),
	}
}

// emptyMutations are the mutations of a recorded results page with NO rows: its own keys, and what happens when a row is put where there is none.
func emptyMutations() []mutation {
	return []mutation{
		remove("job_id"), remove("next_cursor"), retype("next_cursor", 7), retype("results", nil), remove("results"), retype("results", map[string]any{}),
		retype("results", []any{map[string]any{"ordinal": 0}}).naming("results[0].item_id"),
		retype("results", []any{1}).naming("results[0]"),
	}
}

// TestContracts_AMutatedFixtureIsRefusedNamingTheKey: for every success contract, a required key removed, a key's type changed, and a vocabulary value outside Vocab each make the adapter's
// own call fail with a Contract error that names the key. The same call over the untouched fixture succeeds (TestContracts_EveryFixtureDecodesThroughTheAdaptersRealPath), so the
// mutation is the only thing that changed. Every mutation is checked to have changed the document, and one that does not apply to the body it is given fails the test as proving
// nothing. The two results contracts get DIFFERENT mutations according to what their recording holds (rows or none), decided from the file and not from the case name: the page
// that was recorded with rows gets row mutations, the page that was recorded empty gets the mutations of an empty page, and a recording set with neither kind is red.
func TestContracts_AMutatedFixtureIsRefusedNamingTheKey(t *testing.T) {
	table := contractTable()
	static := map[string][]mutation{
		contractKey(tplVerify, "ok"): {
			remove("status"), retype("status", 5), outside("status", "fine"),
			remove("reason"), outside("reason", "vibes"),
			remove("confidence"), retype("confidence", "90"), retype("confidence", 190),
			remove("recommendation"), outside("recommendation", "yes"),
			remove("domain"), retype("domain", nil),
			remove("flags"), remove("flags.from_cache"), retype("flags.from_cache", "no"),
			remove("signals"), retype("signals", []any{}),
			add("signals.failure_origin", "theirs"), add("signals.smtp_skipped_reason", "lazy"),
		},
		contractKey(tplSubmit, "accepted"): {
			remove("job_id"), retype("job_id", 7), outside("job_id", "J-42"),
			remove("status"), outside("status", "queued"),
			remove("counts"), remove("counts.total"), remove("counts.enqueued"), remove("counts.duplicate"), remove("counts.malformed"),
			retype("counts.total", "3"), retype("counts.enqueued", 2.5), retype("counts.malformed", nil),
		},
		contractKey(tplStatus, "running"): {
			remove("job_id"), remove("status"), outside("status", "queued"), retype("status", 1),
			remove("complete"), retype("complete", "false"),
			remove("counts"), remove("counts.total"), remove("counts.settled"), remove("counts.pending"), retype("counts.pending", "2"),
			remove("processing_paused"), retype("processing_paused", "no"),
		},
		contractKey(tplStatus, "complete"): {
			remove("complete"), outside("status", "running"), outside("status", "done"), remove("counts.settled"),
		},
	}
	total, populated, empty := 0, 0, 0
	for _, c := range Contracts() {
		key := contractKey(c.Route, c.Case)
		if c.Status < 200 || c.Status > 299 {
			continue
		}
		exp, ok := table[key]
		if !ok {
			t.Fatalf("%q is a contract with no entry in the table", key)
		}
		ms, hasStatic := static[key]
		if c.Route == tplResults {
			if n := len(recordedRows(t, c.Fixture)); n > 0 {
				ms, populated = rowMutations(t, c.Fixture), populated+1
			} else {
				ms, empty = emptyMutations(), empty+1
			}
		} else if !hasStatic {
			t.Errorf("%s: no mutations are written for this success contract", key)
			continue
		}
		base := fixture(t, c.Fixture)
		for _, m := range ms {
			total++
			mutated := m.apply(t, base)
			if mutated == encodeDoc(t, decodeDoc(t, base)) {
				t.Errorf("%s / %s: the mutation changed nothing", key, m.name)
				continue
			}
			summary, err := exp.run(t, c.Status, mutated)
			kind, got := kindAndKey(err)
			if kind != modclient.Contract || got != m.key {
				t.Errorf("%s / %s: got %q, kind %v key %q (%v), want a Contract error naming %q", key, m.name, summary, kind, got, err, m.key)
			}
		}
		// control: an unlisted key retyped is tolerated (module 1's changes are additive; the adapter reads only what it lists)
		if key == contractKey(tplVerify, "ok") {
			tolerated := mutation{path: "signals.mx_found", op: "set", to: "yes"}.apply(t, base)
			if summary, err := exp.run(t, c.Status, tolerated); err != nil || summary != exp.summarize(t, base) {
				t.Errorf("control: a retyped key the adapter does not read broke the decode: %q %v", summary, err)
			}
		}
	}
	if populated == 0 || empty == 0 {
		t.Fatalf("the results recordings are %d page(s) with rows and %d without: the mutations of one kind never ran", populated, empty)
	}
	if total < 60 {
		t.Fatalf("only %d mutation(s) ran: the instrument covers too little", total)
	}
	t.Logf("%d mutated fixture(s) refused, each naming its key (%d results page(s) with rows, %d empty)", total, populated, empty)
}

// TestContracts_AnErrorEnvelopeIsReadByItsStatusAndOnlyWhenWellFormed: for EVERY error contract (the three recorded in run 44211659227 that the first suite did not declare, GET
// /v1/jobs/{id} 401 and GET /v1/jobs/{id}/results 401 and 404, among them), a mutated recording is read for what it is. With the code removed the body is no longer module 1's
// envelope: a 4xx is a Contract error (never the class its status would give, never an answer) and a 5xx keeps the class of its status, with no code and Enveloped false; with the
// code replaced by a word module 1 may add later the class stays that of the status (an unknown code is data) and the word is carried, except that a 503 is Paused ONLY for the
// code `paused`. And the envelope served with the success status of its own route is a Contract error: an error body is never read as an answer. Each mutation is paired with
// the unmutated recording, which gives the class the table expects.
func TestContracts_AnErrorEnvelopeIsReadByItsStatusAndOnlyWhenWellFormed(t *testing.T) {
	table := contractTable()
	successStatus := map[string]int{} // route -> the status of its first success contract
	for _, c := range Contracts() {
		if _, ok := successStatus[c.Route]; !ok && c.Status >= 200 && c.Status < 300 {
			successStatus[c.Route] = c.Status
		}
	}
	checked := 0
	for _, c := range Contracts() {
		key := contractKey(c.Route, c.Case)
		exp := table[key]
		if exp.kind == 0 {
			continue
		}
		checked++
		base := fixture(t, c.Fixture)
		if _, err := exp.run(t, c.Status, base); modclient.KindOf(err) != exp.kind {
			t.Errorf("%s control: the unmutated recording gave %v, want %v", key, err, exp.kind)
		}

		// the code removed
		_, err := exp.run(t, c.Status, remove("error.code").apply(t, base))
		wantKind := modclient.Contract
		if c.Status >= 500 {
			wantKind = modclient.Internal
			if c.Status == 503 {
				wantKind = modclient.Unavailable
			}
		}
		if e := asModclient(err); e == nil || e.Kind != wantKind || e.Code != "" || e.Enveloped {
			t.Errorf("%s: with the code removed got %v, want a %v error with no code and no envelope", key, err, wantKind)
		}

		// the code replaced by a word module 1 may add later
		_, err = exp.run(t, c.Status, retype("error.code", "some_new_code").apply(t, base))
		wantKind = exp.kind
		if c.Status == 503 {
			wantKind = modclient.Unavailable // paused is the one code that changes a class
		}
		if e := asModclient(err); e == nil || e.Kind != wantKind || e.Code != "some_new_code" || !e.Enveloped {
			t.Errorf("%s: with an unknown code got %v, want a %v error that carries the code", key, err, wantKind)
		}

		// the envelope where the answer should be
		ok, found := successStatus[c.Route]
		if !found {
			t.Errorf("%s: the route has no success contract to compare with", key)
			continue
		}
		if _, err := exp.run(t, ok, base); modclient.KindOf(err) != modclient.Contract {
			t.Errorf("%s: the envelope served as the route's success (HTTP %d) gave %v, want a Contract error: an error body must never be read as an answer", key, ok, err)
		}
	}
	if checked < 11 {
		t.Fatalf("only %d error contract(s) checked", checked)
	}
	for _, want := range []string{contractKey(tplStatus, "unauthenticated"), contractKey(tplResults, "unauthenticated"), contractKey(tplResults, "not_found")} {
		if table[want].kind == 0 {
			t.Errorf("the recorded contract %q is not an error entry of the table", want)
		}
	}
}

// TestContracts_EveryVocabValueIsAcceptedAndASiblingIsRefused: for every set a contract publishes, the adapter accepts each value of it in the place it maps from, and refuses the same
// value with a letter added, naming the key. This is the Vocab-has-a-value-the-adapter-does-not-handle direction; the other direction (the switch handles a value Vocab lacks) is
// TestVocabListsAreTheSwitches.
func TestContracts_EveryVocabValueIsAcceptedAndASiblingIsRefused(t *testing.T) {
	union := map[string][]string{}
	for _, c := range Contracts() {
		for name, vals := range c.Vocab {
			if prev, ok := union[name]; ok && !reflect.DeepEqual(prev, vals) {
				t.Errorf("the vocabulary %q is listed with two different value sets", name)
			}
			union[name] = vals
		}
	}
	// what is published: every set of the adapter except the ones no recorded body can hold (unpublishedSets), and the error codes; each exactly as the adapter holds it
	want := []string{"error_code"}
	for name := range vocabSets() {
		if _, none := unpublishedSets[name]; !none {
			want = append(want, name)
		}
	}
	sort.Strings(want)
	var got []string
	for name, vals := range union {
		got = append(got, name)
		own := vocabSets()[name]
		if name == "error_code" {
			own = errorCodeValues
		}
		if !reflect.DeepEqual(sortedCopy(vals), sortedCopy(own)) {
			t.Errorf("the published vocabulary %q is not the adapter's own list", name)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the vocabularies published are %v, want %v", got, want)
	}

	verdict := func(status, reason, recommendation, signals string) string {
		return strings.NewReplacer("STATUS", status, "REASON", reason, "REC", recommendation, "SIGNALS", signals).Replace(
			`{"status":"STATUS","reason":"REASON","confidence":50,"recommendation":"REC","domain":"d.example","flags":{"from_cache":false},"signals":SIGNALS}`)
	}
	rowPage := func(status, reason, signals string) string {
		return page(testJob, "", strings.NewReplacer("STATUS", status, "REASON", reason, "SIGNALS", signals).Replace(
			`{"status":"STATUS","reason":"REASON","item_id":"r","ordinal":0,"flags":{"from_cache":false},"signals":SIGNALS}`))
	}
	checks := map[string]struct {
		place string
		call  func(value string) error
	}{
		"status": {"results[0].status", func(v string) error {
			_, _, err := serve(t, 200, rowPage(v, "accepted", "{}")).adapter(t).Results(testCtx(t), testJob, "", 0)
			return err
		}},
		"reason": {"results[0].reason", func(v string) error {
			_, _, err := serve(t, 200, rowPage("valid", v, "{}")).adapter(t).Results(testCtx(t), testJob, "", 0)
			return err
		}},
		"failure_origin": {"results[0].signals.failure_origin", func(v string) error {
			_, _, err := serve(t, 200, rowPage("unknown", "dns_error", `{"failure_origin":"`+v+`"}`)).adapter(t).Results(testCtx(t), testJob, "", 0)
			return err
		}},
		"smtp_skipped_reason": {"results[0].signals.smtp_skipped_reason", func(v string) error {
			_, _, err := serve(t, 200, rowPage("risky", "plausible", `{"smtp_skipped_reason":"`+v+`"}`)).adapter(t).Results(testCtx(t), testJob, "", 0)
			return err
		}},
		"recommendation": {"recommendation", func(v string) error {
			_, err := serve(t, 200, verdict("valid", "accepted", v, "{}")).adapter(t).Verify(testCtx(t), "ada@acme.example")
			return err
		}},
		"job_status": {"status", func(v string) error {
			_, err := serve(t, 200, statusBody(testJob, v, completeJobStatus(v), 3, 1)).adapter(t).Status(testCtx(t), testJob)
			return err
		}},
		"submit_status": {"status", func(v string) error {
			b := mustReplace(t, acceptedFor(t, threeRowPlan().Expected()), `"accepted"`, `"`+v+`"`)
			_, _, err := serve(t, 202, b).adapter(t).Submit(testCtx(t), threeRowPlan())
			return err
		}},
	}
	checked := 0
	for name, vals := range vocabSets() { // every set of the adapter, published or not: what the adapter accepts does not depend on whether a recording can hold it
		chk, ok := checks[name]
		if !ok {
			t.Errorf("the vocabulary %q has no check", name)
			continue
		}
		for _, v := range vals {
			checked++
			if err := chk.call(v); err != nil {
				t.Errorf("%s: the adapter refuses its own vocabulary value %q: %v", name, v, err)
			}
			err := chk.call(v + "x")
			if kind, key := kindAndKey(err); kind != modclient.Contract || key != chk.place {
				t.Errorf("%s: the value %q+x gave kind %v key %q (%v), want a Contract error naming %q", name, v, kind, key, err, chk.place)
			}
		}
	}
	if checked != len(statusValues)+len(reasonValues)+len(originValues)+len(skipValues)+len(recommendationValues)+len(jobStatusValues)+len(submitStatusValues) {
		t.Fatalf("%d value(s) checked, want every value of every set", checked)
	}
	// the error codes are data to the adapter: each published code, and a code module 1 may add later, comes back on the typed error with the class of its STATUS (the adapter has no
	// switch on it; modclient decides by status, and only `paused` changes a class)
	for _, code := range append(append([]string(nil), errorCodeValues...), "some_new_code") {
		f := serve(t, 400, `{"error":{"code":"`+code+`","message":"x","request_id":"00000000-0000-4000-8000-000000000001"}}`)
		_, err := f.adapter(t).Status(testCtx(t), testJob)
		if e := asModclient(err); e == nil || e.Kind != modclient.Refused || e.Code != code {
			t.Errorf("the code %q on a 400: got %v, want a Refused error that carries it", code, err)
		}
	}
	if len(errorCodeValues) != 9 {
		t.Fatalf("module 1 has 9 error codes (api/errors.go), the list holds %d", len(errorCodeValues))
	}
	// the module's own counts, so a silent edit of a list is seen: 4 statuses and 27 reasons (vocab.go), 3 origins, 6 skip tokens (signals.go), 3 recommendations, 4 job words
	if len(statusValues) != 4 || len(reasonValues) != 27 || len(originValues) != 3 || len(skipValues) != 6 || len(recommendationValues) != 3 || len(jobStatusValues) != 4 || len(submitStatusValues) != 1 {
		t.Fatalf("a vocabulary changed size: %d statuses, %d reasons, %d origins, %d skip tokens, %d recommendations, %d job words, %d submit words",
			len(statusValues), len(reasonValues), len(originValues), len(skipValues), len(recommendationValues), len(jobStatusValues), len(submitStatusValues))
	}
}

// ---- the lists are the switches ----------------------------------------------------------------------------------------------------------------------------

// switchCases returns the string literals of the `case` clauses of func name of src (which must hold at least one).
func switchCases(src []byte, name string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "vocab.go", src, 0)
	if err != nil {
		return nil, err
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name {
			continue
		}
		var out []string
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			found = true
			for _, e := range cc.List {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					if s, err := strconv.Unquote(bl.Value); err == nil {
						out = append(out, s)
					}
				}
			}
			return true
		})
		if !found {
			return nil, fmt.Errorf("%s has no switch", name)
		}
		return out, nil
	}
	return nil, fmt.Errorf("there is no function %s", name)
}

// TestVocabListsAreTheSwitches: the lists Contracts() publishes are exactly the cases of the known* switches the adapter maps with, in both directions, read from the source of
// vocab.go. Paired controls on mutated source text: a case added to a switch (a value the adapter would handle that Vocab lacks) and a case removed (a Vocab value the adapter
// would not handle) are each reported.
func TestVocabListsAreTheSwitches(t *testing.T) {
	src, err := os.ReadFile("vocab.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]string{
		"status": "knownStatus", "reason": "knownReason", "failure_origin": "knownOrigin", "smtp_skipped_reason": "knownSkipToken",
		"recommendation": "knownRecommendation", "job_status": "knownJobStatus", "submit_status": "knownSubmitStatus",
	}
	sets := vocabSets()
	published := map[string][]string{}
	for _, c := range Contracts() {
		for name, vals := range c.Vocab {
			published[name] = vals
		}
	}
	diff := func(src []byte, name string) []string {
		cases, err := switchCases(src, funcs[name])
		if err != nil {
			return []string{err.Error()}
		}
		// the list the switch is compared with is the adapter's own (vocabSets), which Contracts() copies for the sets it publishes (checked below)
		a, b := append([]string(nil), cases...), append([]string(nil), sets[name]...)
		sort.Strings(a)
		sort.Strings(b)
		var out []string
		have := map[string]bool{}
		for _, v := range a {
			have[v] = true
		}
		listed := map[string]bool{}
		for _, v := range b {
			listed[v] = true
			if !have[v] {
				out = append(out, fmt.Sprintf("%s: Vocab lists %q and the switch does not handle it", name, v))
			}
		}
		for _, v := range a {
			if !listed[v] {
				out = append(out, fmt.Sprintf("%s: the switch handles %q and Vocab does not list it", name, v))
			}
		}
		return out
	}
	if len(funcs) != len(sets) {
		t.Fatalf("%d switch(es) for %d set(s)", len(funcs), len(sets))
	}
	for name := range funcs {
		if d := diff(src, name); len(d) != 0 {
			t.Errorf("%v", d)
		}
		switch _, declared := unpublishedSets[name]; {
		case len(published[name]) == 0 && !declared:
			t.Errorf("%s: no contract publishes this set and the test does not declare it unpublished", name)
		case len(published[name]) != 0 && declared:
			t.Errorf("%s: a contract now publishes this set: remove it from unpublishedSets", name)
		case len(published[name]) != 0 && !reflect.DeepEqual(sortedCopy(sets[name]), sortedCopy(published[name])):
			t.Errorf("%s: vocabSets() and the published Vocab differ", name)
		}
	}
	if _, ok := funcs["error_code"]; ok {
		t.Error("error_code has a switch: it is data to the adapter, published for the drift gate only")
	}

	// controls
	added := bytes.Replace(src, []byte(`case "valid", "invalid", "risky", "unknown":`), []byte(`case "valid", "invalid", "risky", "unknown", "maybe":`), 1)
	if bytes.Equal(added, src) {
		t.Fatal("control setup: the switch of knownStatus is not where the control expects it")
	}
	if d := diff(added, "status"); len(d) != 1 || !strings.Contains(d[0], `the switch handles "maybe"`) {
		t.Fatalf("control: a case added to the switch gave %v, want exactly that case reported", d)
	}
	removed := bytes.Replace(src, []byte(`case "own", "remote", "undetermined":`), []byte(`case "own", "remote":`), 1)
	if bytes.Equal(removed, src) {
		t.Fatal("control setup: the switch of knownOrigin is not where the control expects it")
	}
	if d := diff(removed, "failure_origin"); len(d) != 1 || !strings.Contains(d[0], `Vocab lists "undetermined"`) {
		t.Fatalf("control: a case removed from the switch gave %v, want that value reported", d)
	}
}

// unpublishedSets are the sets the adapter maps that NO contract publishes, and why. A set is published only on a contract whose recorded body has a string at the path it
// constrains (TestEveryVocabularyConstrainsSomethingInItsOwnFixture); neither of these keys is in any recording of the real node, so listing them would make them inert. They are
// tested on inline bodies (TestContracts_EveryVocabValueIsAcceptedAndASiblingIsRefused) and NOT checked against a live response. When a recording holds one, a contract lists it
// and TestVocabListsAreTheSwitches says to remove it from here.
var unpublishedSets = map[string]string{
	"failure_origin":      "signals.failure_origin is on unknown answers only, and no recorded verdict or results row is one",
	"smtp_skipped_reason": "signals.smtp_skipped_reason is on answers that skipped the SMTP stage only, and no recorded verdict or results row is one",
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ---- the routes the adapter calls are the routes the contracts cover ----------------------------------------------------------------------------------------

// routesGap says which routes are called but neither covered nor declared uncovered, which are both, and which are declared or covered without being called.
func routesGap(called map[string]bool, covered, uncovered map[string]bool) []string {
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
			out = append(out, r+": covered by a contract and never called by the adapter")
		}
	}
	for r := range uncovered {
		if !called[r] {
			out = append(out, r+": declared uncovered and never called by the adapter")
		}
	}
	sort.Strings(out)
	return out
}

// TestContracts_TheRoutesTheAdapterCallsAreCoveredOrDeclaredUncovered: the routes are collected from the requests the adapter's methods really make (not from a list), and each is
// either the route of a contract or in UncoveredRoutes with its reason, never both. Paired controls: dropping a declaration, and a contract for a route nothing calls, are caught.
func TestContracts_TheRoutesTheAdapterCallsAreCoveredOrDeclaredUncovered(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) {
		switch {
		case c.Path == "/healthz":
			return 200, `{"status":"ok","at":"2026-01-01T00:00:00Z"}`
		case c.Path == "/readyz":
			return 200, `{"status":"ready","checks":{"db":true,"pepper":true},"at":"2026-01-01T00:00:00Z"}`
		case c.Path == "/v1/verify":
			return 200, fixture(t, "verify-ok.json")
		case c.Path == "/v1/verify/bulk":
			return 202, fixture(t, "submit-accepted.json")
		case strings.HasSuffix(c.Path, "/results"):
			return 200, fixture(t, "results-empty.json")
		}
		return 200, fixture(t, "job-complete.json")
	})
	a := f.adapter(t)
	ctx := testCtx(t)
	_, _ = a.Health(ctx)
	_, _ = a.Ready(ctx)
	_, _ = a.Verify(ctx, "ada@acme.example")
	_, _, _ = a.Submit(ctx, threeRowPlan())
	_, _ = a.Status(ctx, testJob)
	_, _, _ = a.Results(ctx, testJob, "", 0)
	_, _ = a.Describe(ctx) // sends nothing
	called := map[string]bool{}
	for _, c := range f.seen() {
		p := strings.ReplaceAll(c.Path, testJob, "{id}")
		called[c.Method+" "+p] = true
	}
	if len(called) != 6 {
		t.Fatalf("the adapter's methods called %d distinct route(s) (%v), want 6", len(called), called)
	}
	covered := map[string]bool{}
	for _, c := range Contracts() {
		covered[c.Route] = true
	}
	uncovered := map[string]bool{}
	for r := range UncoveredRoutes() {
		uncovered[r] = true
	}
	if gap := routesGap(called, covered, uncovered); len(gap) != 0 {
		t.Fatalf("%v", gap)
	}
	delete(uncovered, "GET /readyz")
	if gap := routesGap(called, covered, uncovered); !containsSub(gap, "GET /readyz: called, covered by no contract") {
		t.Fatalf("control: an undeclared route gave %v", gap)
	}
	covered["GET /v1/describe"] = true
	if gap := routesGap(called, covered, map[string]bool{"GET /readyz": true, "GET /healthz": true}); !containsSub(gap, "GET /v1/describe: covered by a contract and never called") {
		t.Fatalf("control: a contract for a route nothing calls gave %v", gap)
	}
	for r, why := range UncoveredRoutes() {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is declared uncovered with no reason", r)
		}
	}
}

// ---- the registry of module-forced special cases ------------------------------------------------------------------------------------------------------------

var forcedTag = regexp.MustCompile(`MODULE-FORCED\(([A-Za-z0-9._-]+)\):`)

// checkForced compares the MODULE-FORCED tags of the non-test .go files of dir with the lines of registry (`<ID>|<file>|<why>`).
func checkForced(dir, registry string) (problems []string, tags, lines int) {
	tagFile := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"cannot read " + dir + ": " + err.Error()}, 0, 0
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, m := range forcedTag.FindAllStringSubmatch(string(b), -1) {
			tags++
			if prev, dup := tagFile[m[1]]; dup {
				problems = append(problems, fmt.Sprintf("the tag %s appears twice (%s and %s): an ID is unique inside the package", m[1], prev, e.Name()))
			}
			tagFile[m[1]] = e.Name()
		}
	}
	reg := map[string]string{}
	for _, l := range strings.Split(strings.ReplaceAll(registry, "\r", ""), "\n") {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#") {
			continue
		}
		lines++
		f := strings.SplitN(l, "|", 3)
		if len(f) != 3 || strings.TrimSpace(f[0]) == "" || strings.TrimSpace(f[1]) == "" || strings.TrimSpace(f[2]) == "" {
			problems = append(problems, fmt.Sprintf("the registry line %q is not <ID>|<file>|<why>", l))
			continue
		}
		if _, dup := reg[f[0]]; dup {
			problems = append(problems, "the registry lists "+f[0]+" twice")
		}
		reg[f[0]] = f[1]
	}
	for id, file := range tagFile {
		switch want, ok := reg[id]; {
		case !ok:
			problems = append(problems, fmt.Sprintf("the tag %s in %s has no line in forced.txt", id, file))
		case want != file:
			problems = append(problems, fmt.Sprintf("the tag %s is in %s but forced.txt says %s", id, file, want))
		}
	}
	for id := range reg {
		if _, ok := tagFile[id]; !ok {
			problems = append(problems, fmt.Sprintf("forced.txt lists %s and no non-test file carries that tag", id))
		}
	}
	if tags == 0 && lines == 0 {
		problems = append(problems, "found 0 tags and 0 registry lines: the instrument matched nothing")
	}
	sort.Strings(problems)
	return problems, tags, lines
}

// TestForcedSpecialCasesAreRegistered: every MODULE-FORCED tag of the package's non-test source has a line in forced.txt (with the file it is in), every line has a tag, and there is at
// least one of each. Paired controls on scratch directories: an unregistered tag, a line with no tag, a tag in another file than the registry says, a repeated ID and an empty
// package are each reported for what they are.
func TestForcedSpecialCasesAreRegistered(t *testing.T) {
	reg, err := os.ReadFile("forced.txt")
	if err != nil {
		t.Fatal(err)
	}
	problems, tags, lines := checkForced(".", string(reg))
	if len(problems) != 0 || tags < 10 || tags != lines {
		t.Fatalf("%d tag(s), %d registry line(s), problems %v", tags, lines, problems)
	}
	t.Logf("%d MODULE-FORCED tag(s), all registered", tags)

	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tagLine := func(id string) string {
		return "package x\n\n// MODULE-FORCED(" + id + "): the module does a thing.\nfunc f() {}\n"
	}

	d := t.TempDir()
	write(d, "a.go", tagLine("X-F1"))
	if p, n, _ := checkForced(d, ""); n != 1 || !containsSub(p, "the tag X-F1 in a.go has no line in forced.txt") {
		t.Fatalf("control, an unregistered tag: %v", p)
	}
	if p, _, _ := checkForced(d, "X-F1|a.go|why"); len(p) != 0 {
		t.Fatalf("control: a registered tag is reported: %v", p)
	}
	if p, _, _ := checkForced(d, "X-F1|a.go|why\nX-F2|a.go|why"); !containsSub(p, "forced.txt lists X-F2 and no non-test file carries that tag") {
		t.Fatalf("control, a line with no tag: %v", p)
	}
	if p, _, _ := checkForced(d, "X-F1|b.go|why"); !containsSub(p, "the tag X-F1 is in a.go but forced.txt says b.go") {
		t.Fatalf("control, a tag in another file: %v", p)
	}
	write(d, "b.go", tagLine("X-F1"))
	if p, _, _ := checkForced(d, "X-F1|a.go|why"); !containsSub(p, "the tag X-F1 appears twice") {
		t.Fatalf("control, a repeated ID: %v", p)
	}
	write(d, "a_test.go", tagLine("X-F9")) // a tag in a test file is not a tag of the package
	if p, n, _ := checkForced(d, "X-F1|a.go|why"); n != 2 || containsSub(p, "X-F9") {
		t.Fatalf("control: a test file's tag was counted (%d tags): %v", n, p)
	}
	if p, _, _ := checkForced(t.TempDir(), ""); !containsSub(p, "found 0 tags and 0 registry lines") {
		t.Fatalf("control, an empty package: %v", p)
	}
	if p, _, _ := checkForced(d, "garbage"); !containsSub(p, `the registry line "garbage" is not <ID>|<file>|<why>`) {
		t.Fatalf("control, a malformed registry line: %v", p)
	}
}

// TestErrorsAreTypedModclientErrors: every error a method returns on a failure path is, or wraps, a *modclient.Error, so one errors.As and one KindOf serve the caller; sampled over
// the refusals made before a request and the failures after it. (The sentinels ErrNoDeadline, ErrCursorStuck, ErrIncomplete and ErrNoItemID are causes inside such an error, never
// errors of their own.)
func TestErrorsAreTypedModclientErrors(t *testing.T) {
	f := serve(t, 500, fixtureErr("internal_error"))
	a := f.adapter(t)
	ctx := testCtx(t)
	_, e1 := a.Status(ctx, "nope")
	_, _, e2 := a.Results(ctx, testJob, "", 5000)
	_, e3 := a.Verify(ctx, "ada@acme.example")
	_, _, e4 := a.Submit(ctx, Prepare(nil))
	_, e5 := a.Drain(context.Background(), testJob, nil)
	_, e6 := a.Describe(ctx)
	_, e7 := a.Status(ctx, testJob)
	_, e8 := ToSettleItems([]Row{{}})
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7, e8} {
		if err == nil || asModclient(err) == nil || modclient.KindOf(err) == 0 {
			t.Errorf("error %d is not a typed modclient error: %v", i+1, err)
		}
	}
	if !errors.Is(e6, modclient.ErrNotSupported) {
		t.Errorf("Describe: got %v, want NotSupported", e6)
	}
	if f.count() != 2 { // Verify and the one Status that carried a uuid; every refusal above was made before a request
		t.Errorf("%d request(s), want 2", f.count())
	}
}
