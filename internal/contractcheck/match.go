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
	// Contracts and Observations are the sizes of the two inputs.
	Contracts    int
	Observations int

	undeclaredObs [][]Observation // the observations behind each Undeclared entry (record mode prints one of them; never reported)
}

type rank struct{ diffs, oneSided int }

// scored is one candidate contract for an observation, with the report that scored it.
type scored struct {
	idx int
	rep Report
	rk  rank
}

func (a rank) less(b rank) bool {
	if a.diffs != b.diffs {
		return a.diffs < b.diffs
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

// Match assigns every observation to the contract of the same module, route and status that fits it best: the one with the fewest differences from Compare against the
// contract's fixture (read from fixtures under dir), then the fewest positions that tell nothing on one side only, then the first by contract order that no observation has been
// assigned to yet, then the first. The route of a contract is its "METHOD /template": it fits an observation whose method and path (as sent) fit the template, or whose route
// label it names. Contracts of one route differ by case (a page and an empty page), so several can fit one observation; the others are listed in Matched.Tied.
//
// The observations are put in a canonical order first, so the result does not depend on the order they arrived in. An observation no contract fits by route and status is
// Undeclared (grouped, with no body); a contract no observation was assigned to is Unexercised. Match reads fixtures but never fails: a fixture that cannot be read is a
// difference in the report of every observation assigned to its contract.
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
			all = append(all, scored{i, rep, rank{len(rep.Diffs), oneSidedGaps(rep)}})
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
		}
	}
	return res
}

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
