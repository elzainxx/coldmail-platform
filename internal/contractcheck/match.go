package contractcheck

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Observation is one response a modclient.Client received (Options.Observe delivers it): the module, the route label, the method and path as sent, the status and a copy of
// the body.
type Observation = modclient.Observation

// routeSpec is a contract's Route read as "METHOD /path/{template}": the method and the segments of the path, a `{...}` segment standing for any one non-empty segment.
type routeSpec struct {
	method string
	segs   []string
	ok     bool
}

func parseRouteSpec(route string) routeSpec {
	i := strings.IndexByte(route, ' ')
	if i <= 0 || i == len(route)-1 {
		return routeSpec{}
	}
	method, tpl := route[:i], route[i+1:]
	for j := 0; j < len(method); j++ {
		if method[j] < 'A' || method[j] > 'Z' {
			return routeSpec{}
		}
	}
	if tpl[0] != '/' || strings.ContainsAny(tpl, " ?#") {
		return routeSpec{}
	}
	return routeSpec{method: method, segs: strings.Split(tpl[1:], "/"), ok: true}
}

func isPlaceholderSeg(s string) bool { return len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' }

func (r routeSpec) matches(method, p string) bool {
	if !r.ok || method != r.method || p == "" || p[0] != '/' {
		return false
	}
	segs := strings.Split(p[1:], "/")
	if len(segs) != len(r.segs) {
		return false
	}
	for i, want := range r.segs {
		switch {
		case isPlaceholderSeg(want):
			if segs[i] == "" {
				return false
			}
		case segs[i] != want:
			return false
		}
	}
	return true
}

// routeMatches says whether a contract's route is the route an observation carries: the contract's "METHOD /template" fits the method and path as sent, or the contract names the
// request's route label itself.
func routeMatches(rc modclient.RouteContract, spec routeSpec, o Observation) bool {
	if rc.Module != o.Module {
		return false
	}
	return rc.Route == o.Route || spec.matches(o.Method, o.Path)
}

// Matched is an observation assigned to a contract.
type Matched struct {
	Observation Observation
	// Index is the position of the contract in the list given to Match.
	Index    int
	Contract modclient.RouteContract
	// Report is Compare of the observation against the contract's fixture.
	Report Report
	// Tied are the cases of the other contracts that fitted this observation exactly as well (the one chosen is the first by contract order that no observation had been
	// assigned to yet); empty when the best was unique.
	Tied []string
}

// UndeclaredCall is a group of observations of a route and status that no contract declares: a dependency nobody wrote down. It holds no body.
type UndeclaredCall struct {
	Module modclient.Module
	// Route is the request's route label.
	Route  string
	Method string
	// Path is the path with every segment that no contract of the module names as a literal replaced by {} (a real id never reaches a report).
	Path   string
	Status int
	// RouteDeclared is true when a contract declares the route for other statuses (the module answered a status nobody expected); false when the route itself is unknown.
	RouteDeclared bool
	// Count is the number of observations in the group.
	Count int
}

// Template is the route as a contract would name it: "METHOD /path/{}".
func (u UndeclaredCall) Template() string { return u.Method + " " + u.Path }

func (u UndeclaredCall) String() string {
	d := "no contract declares this route"
	if u.RouteDeclared {
		d = "a contract declares the route, but not this status"
	}
	return fmt.Sprintf("%s %s answered %d (route label %s, %d observation(s)): %s", u.Module, u.Template(), u.Status, u.Route, u.Count, d)
}

// MatchResult is what Match made of a set of observations.
type MatchResult struct {
	// Matched are the observations that fitted a contract, in canonical order.
	Matched []Matched
	// Undeclared are the groups of observations no contract declares, in canonical order.
	Undeclared []UndeclaredCall
	// Unexercised are the contracts no observation was assigned to, in contract order.
	Unexercised []modclient.RouteContract
	// NearestMisses has one entry, in contract order, for each unexercised contract of which at least one observation of the same module, route and status exists: how far its
	// fixture is from the closest of them. A contract nothing of its module, route and status was observed for has none (it was never called).
	NearestMisses []NearestMiss
	// Contracts and Observations are the sizes of the two inputs.
	Contracts    int
	Observations int

	undeclaredObs [][]Observation // the observations behind each Undeclared entry (record mode prints one of them; never reported)
}

// rank orders the contracts that could take an observation by DISTANCE: the differences from Compare, plus the positions that tell nothing on one side only (one-sided gaps: an
// empty array, an empty map or a null in the fixture where the live body has content, or the reverse), plus the vocabulary positions at which the fixture and the live body carry
// different values (a test button's ok against failed against unavailable: contracts whose fixtures differ in nothing else), each counting as one position like one difference. Of equal distance the
// one with fewer gaps comes first. Both halves matter. Differences alone let a fixture that says nothing (an empty page) fit every body with zero differences and take the
// observations of a populated contract; gaps first let a fixture of a completely different structure (a dozen differences, no gap) lose to one of the right structure that has a
// single gap. Together, a fixture of the right structure with one stale key or one gap beats one of another structure with a dozen differences, and of two at the same distance the
// one that agrees about what is exercised wins.
type rank struct{ total, oneSided int }

func newRank(rep Report) rank {
	g := oneSidedGaps(rep)
	return rank{total: len(rep.Diffs) + g + len(rep.VocabMismatch), oneSided: g}
}

// scored is one candidate contract for an observation, with the report that scored it.
type scored struct {
	idx int
	rep Report
	rk  rank
}

func (a rank) less(b rank) bool {
	if a.total != b.total {
		return a.total < b.total
	}
	return a.oneSided < b.oneSided
}

// oneSidedGaps counts the positions that tell nothing on one side only: a live empty page against a fixture that has elements fits worse than against a fixture that is an empty
// page too, though neither is a difference.
func oneSidedGaps(rep Report) int {
	sides := map[string]int{}
	for _, g := range rep.Gaps {
		sides[g.Path]++
	}
	n := 0
	for _, c := range sides {
		if c == 1 {
			n++
		}
	}
	return n
}

func obsLess(a, b Observation) bool {
	switch {
	case a.Module != b.Module:
		return a.Module < b.Module
	case a.Method != b.Method:
		return a.Method < b.Method
	case a.Path != b.Path:
		return a.Path < b.Path
	case a.Route != b.Route:
		return a.Route < b.Route
	case a.Status != b.Status:
		return a.Status < b.Status
	case a.Truncated != b.Truncated:
		return !a.Truncated
	}
	return bytes.Compare(a.Body, b.Body) < 0
}

type fixtureCache struct {
	fsys fs.FS
	dir  string
	data map[int][]byte
	errs map[int]error
}

func (f *fixtureCache) load(i int, rc modclient.RouteContract) ([]byte, error) {
	if err, bad := f.errs[i]; bad {
		return nil, err
	}
	if b, ok := f.data[i]; ok {
		return b, nil
	}
	var b []byte
	var err error
	switch {
	case f.fsys == nil:
		err = fs.ErrInvalid
	case rc.Fixture == "" || !fs.ValidPath(rc.Fixture) || strings.Contains(rc.Fixture, "/"):
		err = fs.ErrInvalid
	default:
		b, err = fs.ReadFile(f.fsys, path.Join(f.dir, rc.Fixture))
	}
	if err != nil {
		f.errs[i] = err
		return nil, err
	}
	f.data[i] = b
	return b, nil
}

// compareObservation is Compare of one observation against a contract whose fixture may be missing (then the report says so in words, not as an empty document).
func compareObservation(rc modclient.RouteContract, fixture []byte, ferr error, o Observation) Report {
	rep := Compare(rc, fixture, o.Body, o.Status)
	if ferr != nil {
		for i, d := range rep.Diffs {
			if d.Kind == ContractInvalid && strings.HasPrefix(d.Detail, "the recorded fixture cannot be read") {
				rep.Diffs[i].Detail = "the recorded fixture cannot be read (" + errWord(ferr) + "): record it with " + EnvRecord + "=1"
			}
		}
	}
	return rep
}

// Match assigns every observation to the contract of the same module, route and status that fits it best: the one with the smallest distance (differences from Compare against the
// contract's fixture, read from fixtures under dir, plus one-sided gaps; see rank), then the fewest gaps, then the first by contract order that no observation has been assigned to
// yet, then the first. The route of a contract is its "METHOD /template": it fits an observation whose method and path (as sent) fit the template, or whose route label it names.
// Contracts of one route differ by case (a page and an empty page), so several can fit one observation; the others are listed in Matched.Tied.
//
// The observations are put in a canonical order first, so the result does not depend on the order they arrived in. An observation no contract fits by route and status is
// Undeclared (grouped, with no body); a contract no observation was assigned to is Unexercised. Match reads fixtures but never fails: a fixture that cannot be read is a
// difference in the report of every observation assigned to its contract. For every unexercised contract that has observed responses of its module, route and status, the result
// holds a NearestMiss: how far its fixture is from the closest of them.
func Match(contracts []modclient.RouteContract, fixtures fs.FS, dir string, obs []Observation) MatchResult {
	res := MatchResult{Contracts: len(contracts), Observations: len(obs)}
	sorted := append([]Observation(nil), obs...)
	sort.SliceStable(sorted, func(i, j int) bool { return obsLess(sorted[i], sorted[j]) })

	specs := make([]routeSpec, len(contracts))
	known := map[modclient.Module]map[string]bool{}
	for i, rc := range contracts {
		specs[i] = parseRouteSpec(rc.Route)
		for _, s := range specs[i].segs {
			if !isPlaceholderSeg(s) {
				if known[rc.Module] == nil {
					known[rc.Module] = map[string]bool{}
				}
				known[rc.Module][s] = true
			}
		}
	}
	cache := &fixtureCache{fsys: fixtures, dir: dir, data: map[int][]byte{}, errs: map[int]error{}}
	assigned := make([]int, len(contracts))
	tracks := make([]missTrack, len(contracts))
	type groupKey struct {
		module        modclient.Module
		method, path  string
		route         string
		status        int
		routeDeclared bool
	}
	groups := map[groupKey]int{}

	for _, o := range sorted {
		var cands []int
		routeDeclared := false
		for i, rc := range contracts {
			if !routeMatches(rc, specs[i], o) {
				continue
			}
			routeDeclared = true
			if rc.Status == o.Status {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			g := groupKey{o.Module, o.Method, redactPath(known[o.Module], o.Path), o.Route, o.Status, routeDeclared}
			idx, ok := groups[g]
			if !ok {
				idx = len(res.Undeclared)
				groups[g] = idx
				res.Undeclared = append(res.Undeclared, UndeclaredCall{Module: o.Module, Route: o.Route, Method: o.Method, Path: g.path, Status: o.Status, RouteDeclared: routeDeclared})
				res.undeclaredObs = append(res.undeclaredObs, nil)
			}
			res.Undeclared[idx].Count++
			res.undeclaredObs[idx] = append(res.undeclaredObs[idx], o)
			continue
		}
		all := make([]scored, 0, len(cands))
		for _, i := range cands {
			fx, ferr := cache.load(i, contracts[i])
			rep := compareObservation(contracts[i], fx, ferr, o)
			sc := scored{i, rep, newRank(rep)}
			all = append(all, sc)
			tracks[i].offer(sc)
		}
		best := all[0].rk
		for _, s := range all[1:] {
			if s.rk.less(best) {
				best = s.rk
			}
		}
		var top []scored
		for _, s := range all {
			if s.rk == best {
				top = append(top, s)
			}
		}
		chosen := top[0]
		for _, s := range top {
			if assigned[s.idx] == 0 {
				chosen = s
				break
			}
		}
		assigned[chosen.idx]++
		m := Matched{Observation: o, Index: chosen.idx, Contract: contracts[chosen.idx], Report: chosen.rep}
		for _, s := range top {
			if s.idx != chosen.idx {
				m.Tied = append(m.Tied, contracts[s.idx].Case)
			}
		}
		res.Matched = append(res.Matched, m)
	}
	for i, rc := range contracts {
		if assigned[i] == 0 {
			res.Unexercised = append(res.Unexercised, rc)
			if tracks[i].seen > 0 {
				res.NearestMisses = append(res.NearestMisses, NearestMiss{Contract: rc, Observed: tracks[i].seen, Misses: missesOf(tracks[i].rep)})
			}
		}
	}
	return res
}

// missTrack follows, for one contract, the closest of the observations that could have been assigned to it (same module, route and status), whether or not it was.
type missTrack struct {
	seen  int
	total int
	rep   Report
}

// offer notes one candidate: the closest is the one with the fewest differences plus one-sided gaps; of equal ones the first in canonical order stays.
func (m *missTrack) offer(s scored) {
	m.seen++
	if t := s.rk.total; m.seen == 1 || t < m.total {
		m.total, m.rep = t, s.rep
	}
}

// Miss is one way a fixture misses an observation: a path and the name of a difference Kind, GapFixture / GapLive for a position that tells nothing on that side only, or
// VocabValue for a vocabulary position that holds different values in the two. It names no value.
type Miss struct {
	Path string
	Kind string
}

// NearestMiss says how far the fixture of an unexercised contract is from the closest observed response of its module, route and status: the responses were there, but each was
// assigned to another contract that fitted it better, or none fitted. It is how a real difference that hides behind an "unexercised" line becomes visible.
type NearestMiss struct {
	Contract modclient.RouteContract
	// Observed is the number of observed responses of the contract's module, route and status.
	Observed int
	// Misses are the differences, then the positions that tell nothing on one side only, of the closest one.
	Misses []Miss
}

// MaxMissesListed is how many misses a NearestMiss line names before it says how many more there are.
const MaxMissesListed = 8

// String is the line the reports print: paths and kinds only, never a value.
func (n NearestMiss) String() string {
	c := ""
	if n.Contract.Case != "" {
		c = " case=" + n.Contract.Case
	}
	head := fmt.Sprintf("nearest miss: %s %s%s: %d differences against the closest of %d observed responses", n.Contract.Module, n.Contract.Route, c, len(n.Misses), n.Observed)
	if len(n.Misses) == 0 {
		return head + ": none (every observed response was assigned to another contract that fitted at least as well)"
	}
	parts := make([]string, 0, MaxMissesListed+1)
	for i, m := range n.Misses {
		if i == MaxMissesListed {
			parts = append(parts, fmt.Sprintf("and %d more", len(n.Misses)-MaxMissesListed))
			break
		}
		parts = append(parts, m.Path+" "+m.Kind)
	}
	return head + ": " + strings.Join(parts, ", ")
}

// missesOf lists a report's differences and then its one-sided gaps (in the order the report has them), each as a path and a kind.
func missesOf(rep Report) []Miss {
	var out []Miss
	for _, d := range rep.Diffs {
		out = append(out, Miss{Path: d.Path, Kind: string(d.Kind)})
	}
	sides := map[string][]string{}
	var order []string
	for _, g := range rep.Gaps {
		if _, ok := sides[g.Path]; !ok {
			order = append(order, g.Path)
		}
		sides[g.Path] = append(sides[g.Path], g.Side)
	}
	for _, p := range order {
		if len(sides[p]) == 1 {
			out = append(out, Miss{Path: p, Kind: "Gap" + strings.ToUpper(sides[p][0][:1]) + sides[p][0][1:]})
		}
	}
	for _, p := range rep.VocabMismatch {
		out = append(out, Miss{Path: p, Kind: KindVocabValue})
	}
	return out
}

// KindVocabValue is the Miss kind of a vocabulary position at which the fixture and the closest observed response carry different values. A Miss names the path only, never the values.
const KindVocabValue = "VocabValue"

// redactPath keeps the segments of a path that some contract of the module names as a literal and replaces every other segment with {}: the path of an undeclared call is
// printed in reports and must not carry a real id.
func redactPath(known map[string]bool, p string) string {
	if p == "" || p[0] != '/' {
		return "/{}"
	}
	segs := strings.Split(p[1:], "/")
	for i, s := range segs {
		if !known[s] {
			segs[i] = "{}"
		}
	}
	return "/" + strings.Join(segs, "/")
}
