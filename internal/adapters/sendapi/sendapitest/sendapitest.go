// Package sendapitest is the test kit the sender adapters (internal/adapters/sendapi, m2 and, after it, m3) share: an httptest stub that records what the client sent, a fake campaign
// report with the module's keyset paging (and its look-ahead bug on demand), a fixture mutator, and the checks every adapter's tests must run (no rotting fixtures, the registry of
// module-forced special cases, the tenant on every request, exact set equality).
//
// Every check that can fail returns its problems as a list of sentences instead of calling t.Fatal, so that a test can run the SAME check on a deliberately broken input and require that it
// reports the reason: a check that cannot be shown to fail is not a check. It is test support, not adapter code: nothing outside a _test.go file imports it.
package sendapitest

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Token makes a bearer secret at run time: no secret is ever committed.
func Token(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "tok-" + hex.EncodeToString(b[:])
}

// Seen is one request a Stub received.
type Seen struct {
	Method string
	Path   string
	Query  url.Values
	Auth   string
	Body   []byte
}

// Stub is an httptest server that records its requests and hands each to a handler with its 1-based number.
type Stub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []Seen
}

// NewStub starts a Stub; it is closed at the end of the test.
func NewStub(t testing.TB, h func(w http.ResponseWriter, r *http.Request, n int)) *Stub {
	t.Helper()
	s := &Stub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.seen = append(s.seen, Seen{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Auth: r.Header.Get("Authorization"), Body: body})
		n := len(s.seen)
		s.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// Close shuts the stub down now (a connection to it is then refused); the cleanup at the end of the test closes it again, harmlessly.
func (s *Stub) Close() { s.srv.Close() }

// URL is the base URL of the stub.
func (s *Stub) URL() string { return s.srv.URL }

// Requests are the requests received so far, in order.
func (s *Stub) Requests() []Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Seen(nil), s.seen...)
}

// Count is the number of requests received.
func (s *Stub) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// JSON writes a JSON response.
func JSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Serve answers every request with status and body (a fixture): the stub of the contract tests.
func Serve(status int, body []byte) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) { JSON(w, status, string(body)) }
}

// ModClient builds a modclient.Client for module against url with a fresh transport: no retries and a short timeout unless opts says otherwise.
func ModClient(t testing.TB, module modclient.Module, baseURL, token string, opts modclient.Options) *modclient.Client {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	c, err := modclient.New(module, baseURL, config.Secret(token), &http.Client{Transport: tr}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Envelope is a module error body: {"error":{code, message, details, request_id}} with a fixed request id (details are field, issue pairs).
func Envelope(code string, details ...[2]string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var b strings.Builder
	b.WriteString(`{"error":{"code":` + q(code) + `,"message":"A fixed sentence."`)
	if len(details) > 0 {
		b.WriteString(`,"details":[`)
		for i, d := range details {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"field":` + q(d[0]) + `,"issue":` + q(d[1]) + `}`)
		}
		b.WriteByte(']')
	}
	b.WriteString(`,"request_id":"req_0123456789abcdef0123456789abcdef"}}`)
	return b.String()
}

// ---- fixtures and mutations ------------------------------------------------------------------------------------------------

// Read reads a fixture; a fixture that cannot be read is a failed test (never a skip).
func Read(t testing.TB, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("the fixture %s cannot be read: %v", name, err)
	}
	return b
}

// Op is what Mutate does at a path.
type Op int

const (
	// Delete removes the key (or array element).
	Delete Op = iota + 1
	// Set replaces the value (nil is JSON null).
	Set
)

var segRe = regexp.MustCompile(`^([A-Za-z0-9_]+)((?:\[[0-9]+\])*)$`)

// Mutate returns body with the value at path (keys joined by '.', array indexes as [n]: "items.results[0].state") deleted or replaced. A path that does not exist is an error: a mutation
// that changes nothing would let the test it feeds pass for no reason.
func Mutate(body []byte, path string, op Op, value any) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the fixture is not JSON: %v", err)
	}
	type step struct {
		key string
		idx int // -1 for a key step
	}
	var steps []step
	for _, seg := range strings.Split(path, ".") {
		m := segRe.FindStringSubmatch(seg)
		if m == nil {
			return nil, fmt.Errorf("bad path segment %q", seg)
		}
		steps = append(steps, step{key: m[1], idx: -1})
		for _, ix := range regexp.MustCompile(`\[([0-9]+)\]`).FindAllStringSubmatch(m[2], -1) {
			n, _ := strconv.Atoi(ix[1])
			steps = append(steps, step{idx: n})
		}
	}
	// walk to the parent of the last step
	var set func(parent any, i int) (any, error)
	set = func(parent any, i int) (any, error) {
		s := steps[i]
		last := i == len(steps)-1
		if s.idx < 0 {
			m, ok := parent.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("path %q: expected an object before %q", path, s.key)
			}
			cur, present := m[s.key]
			if !present {
				return nil, fmt.Errorf("path %q: key %q does not exist", path, s.key)
			}
			switch {
			case last && op == Delete:
				delete(m, s.key)
			case last:
				m[s.key] = value
			default:
				nv, err := set(cur, i+1)
				if err != nil {
					return nil, err
				}
				m[s.key] = nv
			}
			return m, nil
		}
		a, ok := parent.([]any)
		if !ok || s.idx >= len(a) {
			return nil, fmt.Errorf("path %q: index %d is out of range", path, s.idx)
		}
		switch {
		case last && op == Delete:
			a = append(a[:s.idx:s.idx], a[s.idx+1:]...)
		case last:
			a[s.idx] = value
		default:
			nv, err := set(a[s.idx], i+1)
			if err != nil {
				return nil, err
			}
			a[s.idx] = nv
		}
		return a, nil
	}
	out, err := set(doc, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// ---- no rot -----------------------------------------------------------------------------------------------------------------

// CheckNoRot compares the files of a testdata/contract directory with the contracts: every file is named by exactly one contract and every contract names an existing file. It returns the
// problems (none when it is clean), and reports an empty subject (no contract, no file) as a problem too.
func CheckNoRot(dir string, contracts []modclient.RouteContract) []string {
	var problems []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"the fixture directory cannot be read: " + err.Error()}
	}
	files := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			problems = append(problems, "the fixture directory holds a subdirectory "+e.Name())
			continue
		}
		files[e.Name()] = true
	}
	if len(contracts) == 0 {
		problems = append(problems, "there are no contracts: the subject of the check is empty")
	}
	if len(files) == 0 {
		problems = append(problems, "there are no fixture files: the subject of the check is empty")
	}
	named := map[string]int{}
	seenCase := map[string]bool{}
	for _, c := range contracts {
		key := string(c.Module) + "|" + c.Route + "|" + c.Case
		if seenCase[key] {
			problems = append(problems, "two contracts have the same route and case: "+c.Route+" / "+c.Case)
		}
		seenCase[key] = true
		if c.Fixture == "" || strings.ContainsAny(c.Fixture, `/\`) {
			problems = append(problems, "the contract "+c.Route+" / "+c.Case+" names no plain file name")
			continue
		}
		named[c.Fixture]++
		if !files[c.Fixture] {
			problems = append(problems, "the contract "+c.Route+" / "+c.Case+" names the missing file "+c.Fixture)
		}
	}
	var names []string
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		switch n := named[f]; {
		case n == 0:
			problems = append(problems, "the file "+f+" is named by no contract (a fixture nothing uses has rotted)")
		case n > 1:
			problems = append(problems, fmt.Sprintf("the file %s is named by %d contracts", f, n))
		}
	}
	return problems
}

// ---- the registry of module-forced special cases --------------------------------------------------------------------------

var (
	tagLoose  = regexp.MustCompile(`MODULE-FORCED\(`)
	tagStrict = regexp.MustCompile(`//\s*MODULE-FORCED\(([A-Za-z0-9][A-Za-z0-9-]*)\):\s*\S`)
)

// CheckForced compares the MODULE-FORCED tags of the non-test .go files of dir with its forced.txt (one `<ID>|<file>|<one-line why>` per line; blank lines and # comments ignored): a tag with
// no line, a line with no tag, a line whose file is not where the tag is, a repeated id, a malformed tag or line, and zero tags with zero lines are each a problem. It returns the problems and the
// ids found, sorted.
func CheckForced(dir string) (problems, ids []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"the package directory cannot be read: " + err.Error()}, nil
	}
	tagFile := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			problems = append(problems, "cannot read "+n+": "+err.Error())
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !tagLoose.MatchString(line) {
				continue
			}
			m := tagStrict.FindStringSubmatch(line)
			if m == nil {
				problems = append(problems, fmt.Sprintf("%s:%d: a MODULE-FORCED tag that is not the registered form (a comment line, the word, the id in parentheses, a colon and a sentence)", n, i+1))
				continue
			}
			if prev, dup := tagFile[m[1]]; dup {
				problems = append(problems, fmt.Sprintf("the id %s is tagged twice (%s and %s)", m[1], prev, n))
			}
			tagFile[m[1]] = n
		}
	}
	lines := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "forced.txt"))
	if err != nil {
		problems = append(problems, "forced.txt cannot be read: "+err.Error())
	}
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 || strings.TrimSpace(f[0]) == "" || strings.TrimSpace(f[1]) == "" || strings.TrimSpace(f[2]) == "" {
			problems = append(problems, fmt.Sprintf("forced.txt:%d is not `ID|file|why`", i+1))
			continue
		}
		if _, dup := lines[f[0]]; dup {
			problems = append(problems, "forced.txt lists the id "+f[0]+" twice")
		}
		lines[f[0]] = f[1]
	}
	if len(tagFile) == 0 && len(lines) == 0 {
		problems = append(problems, "there are no MODULE-FORCED tags and no forced.txt lines: the subject of the check is empty")
	}
	for id, file := range tagFile {
		lf, ok := lines[id]
		switch {
		case !ok:
			problems = append(problems, "the tag "+id+" in "+file+" has no line in forced.txt")
		case lf != file:
			problems = append(problems, "forced.txt says the tag "+id+" is in "+lf+" and it is in "+file)
		}
		ids = append(ids, id)
	}
	for id := range lines {
		if _, ok := tagFile[id]; !ok {
			problems = append(problems, "forced.txt lists "+id+" and no tag has that id")
		}
	}
	sort.Strings(problems)
	sort.Strings(ids)
	return problems, ids
}

// ---- tenancy and exactness --------------------------------------------------------------------------------------------------

// CheckTenancy requires every request to carry user: in the JSON body of a POST, in the query of everything else. No requests at all is a problem (an empty subject).
func CheckTenancy(reqs []Seen, user string) []string {
	if len(reqs) == 0 {
		return []string{"no request was made: the subject of the check is empty"}
	}
	var problems []string
	for i, r := range reqs {
		var got string
		if r.Method == http.MethodPost {
			var b struct {
				UserID string `json:"user_id"`
			}
			if err := json.Unmarshal(r.Body, &b); err != nil {
				problems = append(problems, fmt.Sprintf("request %d (%s %s) has no JSON body", i+1, r.Method, r.Path))
				continue
			}
			got = b.UserID
		} else {
			got = r.Query.Get("user_id")
		}
		if got != user {
			problems = append(problems, fmt.Sprintf("request %d (%s %s) carries user_id %q, want %q", i+1, r.Method, r.Path, got, user))
		}
	}
	return problems
}

// ExactSet compares got with want as sequences of ids: every wanted id exactly once, nothing else, in order. It returns the problems.
func ExactSet(got, want []string) []string {
	var problems []string
	count := map[string]int{}
	for _, g := range got {
		count[g]++
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
		if count[w] == 0 {
			problems = append(problems, "missing "+w)
		}
	}
	for g, n := range count {
		switch {
		case !wantSet[g]:
			problems = append(problems, "unexpected "+g)
		case n > 1:
			problems = append(problems, fmt.Sprintf("%s appears %d times", g, n))
		}
	}
	if len(problems) == 0 {
		for i := range want {
			if got[i] != want[i] {
				problems = append(problems, "the order differs at position "+strconv.Itoa(i))
				break
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// ---- a campaign report with the module's keyset paging --------------------------------------------------------------------

// Campaign is a fake module's campaign: the items in creation order (each a map with at least job_item_id) and a header (dispatch_paused, totals, progress, ...) built from the item count.
type Campaign struct {
	ID    string
	User  string
	Items []map[string]any
	// Header builds the top-level keys other than `items` from the number of items of the campaign.
	Header func(total int) map[string]any
	// LookaheadBug anchors the next cursor on the look-ahead row (the one past the page) instead of the last row returned: the module-2 defect that skips that row at every page boundary.
	LookaheadBug bool
}

func cursorOf(i int, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(i) + "|" + id))
}

// Handler serves GET /v1/report?campaign_id=&user_id=[&cursor=][&page_size=] the way the modules do: page_size defaults to 100 and is clamped to 200 silently, a cursor that is not base64 or has
// no '|' is a 400 naming the cursor, a cursor that decodes but is not an index and an id is a 500, a campaign or tenant that is not this one answers an empty 200, and the last page has a null
// next_cursor.
func (c Campaign) Handler() func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, _ int) {
		q := r.URL.Query()
		items := c.Items
		if q.Get("campaign_id") != c.ID || q.Get("user_id") != c.User {
			items = nil
		}
		size := 100
		if n, err := strconv.Atoi(q.Get("page_size")); err == nil && n > 0 {
			size = n
		}
		if size > 200 {
			size = 200
		}
		start := 0
		if cur := q.Get("cursor"); cur != "" {
			raw, err := base64.RawURLEncoding.DecodeString(cur)
			if err != nil || !strings.Contains(string(raw), "|") {
				JSON(w, 400, Envelope("validation_failed", [2]string{"cursor", "malformed"}))
				return
			}
			idx, err := strconv.Atoi(strings.SplitN(string(raw), "|", 2)[0])
			if err != nil || idx < 0 {
				JSON(w, 500, Envelope("internal_error"))
				return
			}
			start = idx + 1
		}
		if start > len(items) {
			start = len(items)
		}
		end := start + size
		more := false
		if end < len(items) {
			more = true
		} else {
			end = len(items)
		}
		page := items[start:end]
		out := c.Header(len(items))
		results := make([]any, len(page))
		for i, it := range page {
			results[i] = it
		}
		var next any
		if more {
			anchor := end - 1 // the last row returned
			if c.LookaheadBug {
				anchor = end // the look-ahead row
			}
			next = cursorOf(anchor, fmt.Sprint(items[anchor]["job_item_id"]))
		}
		out["campaign_id"] = q.Get("campaign_id")
		out["items"] = map[string]any{"results": results, "next_cursor": next, "page_size": size}
		b, _ := json.Marshal(out)
		JSON(w, 200, string(b))
	}
}
