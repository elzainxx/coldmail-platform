package contractcheck

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// MaxObservations is the most distinct observations a Session keeps. Identical ones (same module, route, method, path, status and body) are counted, not kept again; past the
// cap further distinct observations are counted as dropped and the report says so (a gate that silently covered only the first few thousand responses would be a false green).
const MaxObservations = 4096

// Session is the live drift gate of one module in the harness: it collects what the module's Client observed while the harness's tests ran (Observer), and at the end
// (Finish) matches it against the module's contracts and fixtures, fails the test on every difference, every undeclared route and every contract that was never exercised, and
// prints the coverage. In record mode it prints the fixtures instead.
//
// How a stage binary uses it (one Session per module, created before the first test and finished once, in the last test or in TestMain after the run):
//
//	sess := contractcheck.NewSession(m1adapter.Contracts(), m1adapter.Fixtures, "testdata/contract", map[string]string{
//	    "m1|POST /v1/verify/bulk|paused": "needs the module paused by an operator, which no harness test does",
//	})
//	opts := modclient.DefaultOptions()
//	opts.Observe = sess.Observer()          // build the adapter's Client with these options
//	... run the end-to-end tests through the adapter ...
//	sess.Finish(t)                          // once, at the end
//
// A Session is safe for concurrent use.
type Session struct {
	contracts []modclient.RouteContract
	fsys      fs.FS
	dir       string
	allow     map[string]string

	mu      sync.Mutex
	entries []sessionEntry
	seen    map[[32]byte]int // the digest of an observation -> its place in entries
	dropped int
	record  bool
}

type sessionEntry struct {
	obs   Observation
	count int
}

// NewSession makes a Session for the contracts of one module (or of several). fsys and dir say where the recorded fixtures are (the adapter package's embedded testdata and
// "testdata/contract"). allowUnexercised lists, by "module|route|case", the contracts the harness cannot elicit, each with the reason (mandatory); it is checked at Finish. The
// Session is in record mode when CONTRACT_RECORD=1 is set in the environment.
func NewSession(contracts []modclient.RouteContract, fsys fs.FS, dir string, allowUnexercised map[string]string) *Session {
	allow := make(map[string]string, len(allowUnexercised))
	for k, v := range allowUnexercised {
		allow[k] = v
	}
	return &Session{
		contracts: append([]modclient.RouteContract(nil), contracts...),
		fsys:      fsys, dir: dir, allow: allow,
		seen:   map[[32]byte]int{},
		record: os.Getenv(EnvRecord) == "1",
	}
}

// SetRecordMode overrides the mode the environment gave.
func (s *Session) SetRecordMode(on bool) {
	s.mu.Lock()
	s.record = on
	s.mu.Unlock()
}

// Recording reports whether the Session is in record mode.
func (s *Session) Recording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record
}

func digestOf(o Observation) [32]byte {
	h := sha256.New()
	part := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, f := range []string{string(o.Module), o.Method, o.Path, o.Route, fmt.Sprint(o.Status), fmt.Sprint(o.Truncated)} {
		part([]byte(f))
	}
	part(o.Body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Observer returns the function to set as modclient.Options.Observe. It is safe to call from any goroutine.
func (s *Session) Observer() func(modclient.Observation) {
	return func(o modclient.Observation) {
		d := digestOf(o)
		s.mu.Lock()
		defer s.mu.Unlock()
		if i, ok := s.seen[d]; ok {
			s.entries[i].count++
			return
		}
		if len(s.entries) >= MaxObservations {
			s.dropped++
			return
		}
		s.seen[d] = len(s.entries)
		s.entries = append(s.entries, sessionEntry{obs: o, count: 1})
	}
}

// snapshot returns the distinct observations kept, how many responses were observed in all (kept or counted again) and how many distinct ones were dropped at the cap.
func (s *Session) snapshot() (obs []Observation, total, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		obs = append(obs, e.obs)
		total += e.count
	}
	return obs, total + s.dropped, s.dropped
}

func keyOf(rc modclient.RouteContract) Exercised { return Exercised{rc.Module, rc.Route, rc.Case} }

// Finish matches what was observed against the contracts and reports it. Outside record mode it fails the test with: every difference between a live response and its
// fixture (once per distinct difference, with how many observations showed it: module, route, case, path, kind), every undeclared route, every contract nothing exercised
// that is not in the allowlist, every allowlist entry without a reason, naming no contract or naming a contract that WAS exercised (stale), the cap if it was hit, and an
// empty list of contracts; and it prints the coverage with the fixed sentence about what the gate cannot see and the allowed-unexercised contracts with their reasons. In record
// mode it prints one CONTRACT-FIXTURE block per contract that was matched and one per group of undeclared calls (file UNDECLARED-<module>-<n>.json), does not fail on
// differences, undeclared routes or unexercised contracts, and fails only when a block is refused (a secret-shaped body, a contract that cannot be written into a header)
// or when nothing was observed at all.
func (s *Session) Finish(t TB) {
	t.Helper()
	obs, total, dropped := s.snapshot()
	res := Match(s.contracts, s.fsys, s.dir, obs)
	if s.Recording() {
		s.finishRecord(t, res, total, dropped)
		return
	}
	s.finishCompare(t, res, total, dropped)
}

func (s *Session) finishCompare(t TB, res MatchResult, total, dropped int) {
	t.Helper()
	if len(s.contracts) == 0 {
		t.Errorf("contract drift gate: found 0 contracts: the instrument matched nothing")
	}
	exercised := map[Exercised]bool{}
	for _, m := range res.Matched {
		exercised[keyOf(m.Contract)] = true
	}
	declared := map[Exercised]bool{}
	for _, rc := range s.contracts {
		declared[keyOf(rc)] = true
	}

	// The allowlist: every entry must be well formed, name a contract, say why, and not be stale.
	allowed := map[Exercised]string{}
	names := make([]string, 0, len(s.allow))
	for k := range s.allow {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			t.Errorf("allowlist entry %q is not of the form module|route|case", k)
			continue
		}
		ex := Exercised{modclient.Module(parts[0]), parts[1], parts[2]}
		switch reason := strings.TrimSpace(s.allow[k]); {
		case !declared[ex]:
			t.Errorf("allowlist entry %q names no contract (a typo, or a contract that was removed)", k)
		case reason == "":
			t.Errorf("allowlist entry %q has no reason: say why the harness cannot elicit this response", k)
		case exercised[ex]:
			t.Errorf("allowlist entry %q is stale: the contract was exercised, remove the entry", k)
		default:
			allowed[ex] = reason
		}
	}

	if dropped > 0 {
		t.Errorf("contract drift gate: %d distinct observations were dropped after the cap of %d: the comparison below is incomplete", dropped, MaxObservations)
	}

	// Every distinct difference once, with the number of observations that showed it.
	type seenDiff struct {
		text string
		n    int
	}
	var diffs []*seenDiff
	byText := map[string]*seenDiff{}
	type gapKey struct {
		idx        int
		side, path string
	}
	gapSeen := map[gapKey]bool{}
	var gapLines []string
	tieSeen := map[string]bool{}
	var tieLines []string
	for _, m := range res.Matched {
		for _, d := range m.Report.Diffs {
			txt := d.String()
			if sd, ok := byText[txt]; ok {
				sd.n++
				continue
			}
			sd := &seenDiff{text: txt, n: 1}
			byText[txt] = sd
			diffs = append(diffs, sd)
		}
		for _, g := range m.Report.Gaps {
			k := gapKey{m.Index, g.Side, g.Path}
			if !gapSeen[k] {
				gapSeen[k] = true
				gapLines = append(gapLines, fmt.Sprintf("not compared at %s on the %s side (%s): %s", g.Path, g.Side, g.Why, keyOf(m.Contract)))
			}
		}
		if len(m.Tied) > 0 {
			line := fmt.Sprintf("%s fitted %d contracts equally well (cases %q); it was assigned to case %q", m.Observation.Module, len(m.Tied)+1, m.Tied, m.Contract.Case)
			if !tieSeen[line] {
				tieSeen[line] = true
				tieLines = append(tieLines, line)
			}
		}
	}
	for _, sd := range diffs {
		if sd.n > 1 {
			t.Errorf("contract drift: %s (shown by %d observations)", sd.text, sd.n)
		} else {
			t.Errorf("contract drift: %s", sd.text)
		}
	}
	for _, u := range res.Undeclared {
		t.Errorf("undeclared dependency: %s", u)
	}

	// The coverage text (and the failure for each contract nothing exercised).
	var calls []Exercised
	seenCall := map[Exercised]bool{}
	for _, m := range res.Matched {
		if k := keyOf(m.Contract); !seenCall[k] {
			seenCall[k] = true
			calls = append(calls, k)
		}
	}
	for _, u := range res.Undeclared {
		calls = append(calls, Exercised{u.Module, u.Template(), fmt.Sprintf("status %d", u.Status)})
	}
	cov := Coverage(s.contracts, calls)
	var never []Exercised
	for _, e := range cov.Never {
		if reason, ok := allowed[e]; ok {
			cov.Allowed = append(cov.Allowed, AllowedGap{Contract: e, Reason: reason})
		} else {
			never = append(never, e)
		}
	}
	cov.Never = never
	for _, e := range cov.Never {
		t.Errorf("contract %s was never exercised against the real module (if the harness cannot elicit it, say why in the allowlist)", e)
	}
	w := &logWriter{t: t}
	cov.Print(w)
	logNearestMisses(t, res)
	t.Logf("contract drift gate: %d responses observed (%d distinct kept)", total, len(res.Matched)+countObs(res.Undeclared))
	for _, l := range tieLines {
		t.Logf("note: %s", l)
	}
	for _, l := range gapLines {
		t.Logf("note: %s", l)
	}
	for _, l := range optionalNotes(res) {
		t.Logf("note: %s", l)
	}
}

// optionalNotes lists, once per contract and path, the keys the Shape declares Opt that were present on one side only in some observation: their presence is not compared.
func optionalNotes(res MatchResult) []string {
	type key struct {
		idx  int
		path string
	}
	seen := map[key]bool{}
	var out []string
	for _, m := range res.Matched {
		for _, p := range m.Report.Optional {
			k := key{m.Index, p}
			if !seen[k] {
				seen[k] = true
				out = append(out, fmt.Sprintf("optional per the Shape, presence not compared: %s (%s)", p, keyOf(m.Contract)))
			}
		}
	}
	return out
}

// logNearestMisses prints, for each unexercised contract that has observed responses of its module, route and status, how far its fixture is from the closest of them, and how many
// of the unexercised contracts have one. It prints nothing when every contract was exercised.
func logNearestMisses(t TB, res MatchResult) {
	t.Helper()
	if len(res.Unexercised) == 0 {
		return
	}
	for _, nm := range res.NearestMisses {
		t.Logf("%s", nm)
	}
	t.Logf("contract drift gate: %d of %d unexercised contracts have a nearest miss among the observed responses of their module, route and status", len(res.NearestMisses), len(res.Unexercised))
}

func countObs(us []UndeclaredCall) int {
	n := 0
	for _, u := range us {
		n += u.Count
	}
	return n
}

func (s *Session) finishRecord(t TB, res MatchResult, total, dropped int) {
	t.Helper()
	if total == 0 {
		t.Errorf("contract drift gate (record mode): no response was observed: there is nothing to record")
		return
	}
	w := &logWriter{t: t}
	emit := func(rc modclient.RouteContract, o Observation) {
		t.Helper()
		err := Record(w, rc, o.Status, o.Body)
		var pe *ParseError
		switch {
		case err == nil:
		case errors.As(err, &pe):
			t.Logf("not recorded: %s of module %s answered %d with a body that is not a JSON document (%s)", rc.Route, rc.Module, o.Status, pe.Kind)
		default:
			t.Errorf("cannot record %s of module %s (status %d): %v", rc.Route, rc.Module, o.Status, err)
		}
	}
	// One block per contract that was matched: the observation that says most (an empty page is the worst example of a page).
	byContract := map[int][]Matched{}
	var order []int
	for _, m := range res.Matched {
		if _, ok := byContract[m.Index]; !ok {
			order = append(order, m.Index)
		}
		byContract[m.Index] = append(byContract[m.Index], m)
	}
	sort.Ints(order)
	for _, i := range order {
		ms := byContract[i]
		m := ms[pickIndex(len(ms), func(j int) []byte { return ms[j].Observation.Body }, s.contracts[i].Shape)]
		t.Logf("%s", fitNote(s.contracts[i], m.Report, len(ms)))
		emit(s.contracts[i], m.Observation)
	}
	perModule := map[modclient.Module]int{}
	for i, u := range res.Undeclared {
		perModule[u.Module]++
		rc := modclient.RouteContract{Module: u.Module, Route: u.Template(), Fixture: fmt.Sprintf("UNDECLARED-%s-%d.json", u.Module, perModule[u.Module]), Status: u.Status}
		obs := res.undeclaredObs[i]
		t.Logf("NOTE fixture fit: %s %s file=%s: no contract declares this call, so there is no fixture to compare it with (%d observation(s))", rc.Module, rc.Route, rc.Fixture, len(obs))
		emit(rc, obs[pickIndex(len(obs), func(j int) []byte { return obs[j].Body }, modclient.Shape{})])
	}
	t.Logf("contract drift gate (record mode): %d of %d contracts matched and printed, %d groups of undeclared calls printed under UNDECLARED-<module>-<n>.json, %d contracts still unexercised, %d responses observed",
		len(order), res.Contracts, len(res.Undeclared), len(res.Unexercised), total)
	for _, rc := range res.Unexercised {
		t.Logf("still unexercised: %s", keyOf(rc))
	}
	logNearestMisses(t, res)
	for _, l := range optionalNotes(res) {
		t.Logf("note: %s", l)
	}
	if dropped > 0 {
		t.Logf("%d distinct observations were dropped after the cap of %d", dropped, MaxObservations)
	}
}

// fitNote is the line printed right before a recorded block: how far the observation that is about to be printed is from the contract's CURRENT fixture, in differences and in
// one-sided gaps (the two halves of the distance Match ranks by). 0 and 0 is an exact fit; anything else says the recording will change the fixture, and by how much, so a
// recording that is not a close fit is visible at a glance. ParseRecorded ignores it (it is outside the block).
func fitNote(rc modclient.RouteContract, rep Report, observations int) string {
	c := ""
	if rc.Case != "" {
		c = " case=" + rc.Case
	}
	head := fmt.Sprintf("NOTE fixture fit: %s %s%s file=%s", rc.Module, rc.Route, c, rc.Fixture)
	for _, d := range rep.Diffs {
		if d.Kind == ContractInvalid && strings.HasPrefix(d.Detail, "the recorded fixture cannot be read") {
			return fmt.Sprintf("%s: there is no readable current fixture, so this is a new recording (%d observation(s) assigned to this contract)", head, observations)
		}
	}
	return fmt.Sprintf("%s: %d differences, %d gaps against the current fixture (0 and 0 is an exact fit); the block below is the one of the %d observation(s) assigned to this contract that says most",
		head, len(rep.Diffs), oneSidedGaps(rep), observations)
}

// pickIndex picks, of several observations of one contract, the one that says most: the fewest positions that tell nothing, then the longest body, then the first.
func pickIndex(n int, body func(i int) []byte, shape modclient.Shape) int {
	best, bestGaps, bestLen := 0, 1<<30, -1
	for i := 0; i < n; i++ {
		b := body(i)
		g := 1 << 29 // a body that cannot be read says nothing
		if sk, err := Skeleton(b, shape); err == nil {
			g = len(sk.Gaps())
		}
		if g < bestGaps || g == bestGaps && len(b) > bestLen {
			best, bestGaps, bestLen = i, g, len(b)
		}
	}
	return best
}
