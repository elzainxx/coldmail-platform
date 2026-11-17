package contractcheck

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Kind is what differs between a recorded fixture and a live response.
type Kind string

const (
	// StatusChanged: the live HTTP status is not the recorded one.
	StatusChanged Kind = "StatusChanged"
	// KeyMissing: a key of the fixture is not in the live body (unless the contract's Shape lists it as optional: Opt).
	KeyMissing Kind = "KeyMissing"
	// KeyAdded: a key of the live body is not in the fixture. The modules' API changes are additive and the platform tolerates an added key, but the fixture is stale and
	// must be refreshed, so it is its own difference.
	KeyAdded Kind = "KeyAdded"
	// TypeChanged: the JSON type at a path differs (null is not a type here: a null side says nothing about the type and is listed as a gap). Where the Shape says Int, a
	// number that stopped being an integer (or became one) is a TypeChanged too.
	TypeChanged Kind = "TypeChanged"
	// VocabNew: a value of a closed vocabulary seen live that the contract's Vocab does not list.
	VocabNew Kind = "VocabNew"
	// ShapeViolation: the live body does not decode with the contract's Shape (the key is the one modclient.Decode names), or cannot be read at all.
	ShapeViolation Kind = "ShapeViolation"
	// ContractInvalid: the comparison cannot be made because the contract or its recorded fixture is unusable (the fixture does not parse, a Vocab key is not a path, the
	// Shape cannot be read). Not in the original list of differences: it keeps ShapeViolation meaning "the LIVE body breaks the Shape".
	ContractInvalid Kind = "ContractInvalid"
)

// Diff is one difference. It names the module, the route, the case and the path, and carries no value of the body except a vocabulary value.
type Diff struct {
	Module modclient.Module
	Route  string
	Case   string
	// Path is in the grammar documented in skeleton.go ("(body)" for the document, "(status)" for the HTTP status).
	Path   string
	Kind   Kind
	Detail string
}

func (d Diff) String() string {
	c := ""
	if d.Case != "" {
		c = " case=" + d.Case
	}
	return fmt.Sprintf("%s %s%s path=%s kind=%s: %s", d.Module, d.Route, c, d.Path, d.Kind, d.Detail)
}

// Report is the result of one comparison.
type Report struct {
	Diffs []Diff
	// Gaps are the positions that could not be judged (an empty array, an empty map, a null on either side). They are not differences.
	Gaps []Gap
}

// Clean is true when there is no difference.
func (r Report) Clean() bool { return len(r.Diffs) == 0 }

// Of returns the differences of one kind.
func (r Report) Of(k Kind) []Diff {
	var out []Diff
	for _, d := range r.Diffs {
		if d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}

type comparer struct {
	rc  modclient.RouteContract
	rep *Report
}

func (c *comparer) diff(path string, k Kind, detail string) {
	c.rep.Diffs = append(c.rep.Diffs, Diff{Module: c.rc.Module, Route: c.rc.Route, Case: c.rc.Case, Path: display(path), Kind: k, Detail: detail})
}

func (c *comparer) gap(side, path, why string) {
	c.rep.Gaps = append(c.rep.Gaps, Gap{Side: side, Path: display(path), Why: why})
}

// Compare compares one live response with the recorded fixture of its contract: the HTTP status; the keys, JSON types and (where the Shape says Int) integer-ness of the two
// skeletons; the closed vocabularies; and whether the live body still decodes with the contract's Shape (modclient.Decode, the adapter's own check).
//
// Positions that tell nothing (an empty array, an empty map, a null) are never differences: they are returned in Report.Gaps. The order of Diffs is deterministic: status, then
// the structure in fixture order, then added keys, then vocabularies by name, then the Shape check.
func Compare(rc modclient.RouteContract, fixture, live []byte, liveStatus int) Report {
	var rep Report
	c := &comparer{rc: rc, rep: &rep}
	if liveStatus != rc.Status {
		c.diff("(status)", StatusChanged, fmt.Sprintf("recorded HTTP %d, live HTTP %d", rc.Status, liveStatus))
	}
	sv, serr := viewOf(rc.Shape)
	if serr != nil {
		c.diff("(shape)", ContractInvalid, "the contract's Shape cannot be read: "+serr.Error())
	}
	fv, ferr := parse(fixture)
	if ferr != nil {
		c.diff("(body)", ContractInvalid, "the recorded fixture cannot be read: "+ferr.Error())
	}
	lv, lerr := parse(live)
	if lerr != nil {
		c.diff("(body)", ShapeViolation, "the live body cannot be read: "+lerr.Error())
	}
	if ferr == nil && lerr == nil {
		fn, ln := build(fv, sv), build(lv, sv)
		c.walk("", fn, ln, sv)
		c.vocab(lv)
	}
	if lerr == nil {
		c.shape(live, liveStatus)
	}
	return rep
}

func (c *comparer) walk(path string, fx, lv *node, sv *sview) {
	fNull, lNull := fx.types == tNull, lv.types == tNull
	if fNull {
		c.gap("fixture", path, "null only")
	}
	if lNull {
		c.gap("live", path, "null only")
	}
	if fNull || lNull {
		return
	}
	ft, lt := fx.types&^tNull, lv.types&^tNull
	switch {
	case ft != lt:
		c.diff(path, TypeChanged, fmt.Sprintf("recorded %s, live %s", strings.Join(ft.names(), "|"), strings.Join(lt.names(), "|")))
	case ft&tNumber != 0 && sv != nil && sv.kind == skInteger && fx.integral != lv.integral:
		c.diff(path, TypeChanged, fmt.Sprintf("recorded %s, live %s (the Shape says integer)", intWord(fx.integral), intWord(lv.integral)))
	}
	if fx.types&tObject != 0 && lv.types&tObject != 0 {
		if fx.isMap && lv.isMap {
			c.walkElems(path+"{}", path, fx.mapElem, lv.mapElem, "map", sv.elemView())
		} else {
			c.walkKeys(path, fx, lv, sv)
		}
	}
	if fx.types&tArray != 0 && lv.types&tArray != 0 {
		c.walkElems(path+"[]", path, fx.elem, lv.elem, "array", sv.elemView())
	}
}

func intWord(integral bool) string {
	if integral {
		return "an integer"
	}
	return "a non-integer number"
}

func (c *comparer) walkElems(childPath, path string, fx, lv *node, what string, sv *sview) {
	if fx == nil {
		c.gap("fixture", path, "empty "+what)
	}
	if lv == nil {
		c.gap("live", path, "empty "+what)
	}
	if fx == nil || lv == nil {
		return
	}
	c.walk(childPath, fx, lv, sv)
}

func (c *comparer) walkKeys(path string, fx, lv *node, sv *sview) {
	for _, k := range fx.keys {
		kp := join(path, safeKey(k))
		fld := sv.field(k)
		child, ok := lv.kids[k]
		if !ok {
			if fld != nil && !fld.required {
				continue // the Shape says the key may be absent
			}
			c.diff(kp, KeyMissing, "the key is in the recorded fixture and not in the live body")
			continue
		}
		var cv *sview
		if fld != nil {
			cv = fld.view
		}
		c.walk(kp, fx.kids[k], child, cv)
	}
	for _, k := range lv.keys {
		if _, ok := fx.kids[k]; !ok {
			c.diff(join(path, safeKey(k)), KeyAdded, "the key is in the live body and not in the recorded fixture: refresh the fixture")
		}
	}
}

func (c *comparer) vocab(live value) {
	for _, name := range sortedKeys(c.rc.Vocab) {
		segs, err := parsePath(name)
		if err != nil || len(segs) == 0 {
			c.diff(name, ContractInvalid, "the Vocab key is not a path of the documented grammar")
			continue
		}
		known := map[string]bool{}
		for _, v := range c.rc.Vocab[name] {
			known[v] = true
		}
		var seen []string
		collectStrings(live, segs, &seen)
		reported := map[string]bool{}
		for _, v := range seen {
			if known[v] || reported[v] {
				continue
			}
			reported[v] = true
			c.diff(name, VocabNew, fmt.Sprintf("the live value %s is not in the closed vocabulary (%d values)", printable(v), len(known)))
		}
	}
}

var reIndex = regexp.MustCompile(`\[[0-9]+\]`)

func (c *comparer) shape(live []byte, status int) {
	err := modclient.Decode(&modclient.Response{Status: status, Body: live}, nil, c.rc.Shape)
	if err == nil {
		return
	}
	var me *modclient.Error
	if !errors.As(err, &me) {
		c.diff("(body)", ShapeViolation, "the Shape check failed in a way that names no key")
		return
	}
	path := reIndex.ReplaceAllString(me.Key, "[]")
	if path == "(body)" {
		path = ""
	}
	c.diff(path, ShapeViolation, me.Message)
}

// printable renders a vocabulary value for a message: a short word of the safe alphabet as it is, anything else only by its size (a field that is supposed to hold a closed
// vocabulary and holds something else may hold anything, an address or a credential among it).
func printable(v string) string {
	if v != "" && len(v) <= 40 && utf8.ValidString(v) {
		ok, digit := true, false
		for i := 0; i < len(v); i++ {
			c := v[i]
			if c >= '0' && c <= '9' {
				digit = true
			}
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':' || c == '/' || c == ' ') {
				ok = false
				break
			}
		}
		// A long word with a digit in it looks like an id or a key, not like a vocabulary word.
		if ok && !(digit && len(v) >= 20) {
			return fmt.Sprintf("%q", v)
		}
	}
	return fmt.Sprintf("<%d bytes, not printed>", len(v))
}
