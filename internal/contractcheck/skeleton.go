package contractcheck

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// THE PATH GRAMMAR (one rule, used by Diff.Path, by the paths of RouteContract.Vocab (through VocabAt) and by Skel.Lookup): an object key is its name, joined to its parent with a dot
// (`error.code`); `[]` after a name or after another suffix means "every element of that array" (`items[].state`, a body that is itself an array starts `[]`); `{}` means "every
// value of that map", used only where the contract's Shape says MapOf and the keys are not compared (`checks{}.status`). A name is made of letters, digits, `_` and `-`.
// The path of a vocabulary (RouteContract.VocabAt, else its name) is exactly such a path to a string-valued position, so `items[].state` constrains the `state` of every element of `items`.

type typeSet uint8

const (
	tNull typeSet = 1 << iota
	tBool
	tNumber
	tString
	tObject
	tArray
)

var typeNames = []struct {
	t    typeSet
	name string
}{
	{tArray, "array"}, {tBool, "boolean"}, {tNull, "null"}, {tNumber, "number"}, {tObject, "object"}, {tString, "string"},
}

// names lists the JSON types of the set, alphabetically.
func (s typeSet) names() []string {
	var out []string
	for _, e := range typeNames {
		if s&e.t != 0 {
			out = append(out, e.name)
		}
	}
	return out
}

// node is one position of a skeleton: the union of everything seen there (an array's elements are folded into one node, a map's values too).
type node struct {
	types    typeSet
	integral bool // every number seen here is an integer; meaningful when types has tNumber
	keys     []string
	kids     map[string]*node
	elem     *node // the union of the array elements; nil when no array here had an element
	mapElem  *node // the union of the values of a map (per the contract's Shape); nil when no map here had a value
	isMap    bool
}

func isInteger(lit string) bool {
	_, err := strconv.ParseInt(lit, 10, 64)
	return err == nil
}

// build derives the node of a parsed value; sv is the view of the contract's Shape at this position (nil when the Shape says nothing here).
func build(v value, sv *sview) *node {
	n := &node{integral: true}
	switch v.kind {
	case vNull:
		n.types = tNull
	case vBool:
		n.types = tBool
	case vString:
		n.types = tString
	case vNumber:
		n.types = tNumber
		n.integral = isInteger(v.str)
	case vObject:
		n.types = tObject
		if sv.isMap() {
			n.isMap = true
			for _, m := range v.members {
				n.mapElem = merge(n.mapElem, build(m.val, sv.elemView()))
			}
			return n
		}
		n.kids = make(map[string]*node, len(v.members))
		for _, m := range v.members {
			var child *sview
			if f := sv.field(m.key); f != nil {
				child = f.view
			}
			n.keys = append(n.keys, m.key)
			n.kids[m.key] = build(m.val, child)
		}
	case vArray:
		n.types = tArray
		for _, it := range v.items {
			n.elem = merge(n.elem, build(it, sv.elemView()))
		}
	}
	return n
}

// merge folds b into a (the union of what two positions held) and returns the result; a nil side is the other side.
func merge(a, b *node) *node {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	a.types |= b.types
	a.integral = a.integral && b.integral
	a.isMap = a.isMap || b.isMap
	for _, k := range b.keys {
		if existing, ok := a.kids[k]; ok {
			a.kids[k] = merge(existing, b.kids[k])
			continue
		}
		if a.kids == nil {
			a.kids = map[string]*node{}
		}
		a.keys = append(a.keys, k)
		a.kids[k] = b.kids[k]
	}
	a.elem = merge(a.elem, b.elem)
	a.mapElem = merge(a.mapElem, b.mapElem)
	return a
}

// unexercised says why this position tells nothing about what is below it ("" when it does).
func (n *node) unexercised() string {
	switch {
	case n.types == tNull:
		return "null only"
	case n.types&tArray != 0 && n.elem == nil:
		return "empty array"
	case n.isMap && n.types&tObject != 0 && n.mapElem == nil:
		return "empty map"
	}
	return ""
}

// Skel is the structural skeleton of a body: the keys of every object (recursively), the JSON type of every value, arrays reduced to the union of their elements (an empty
// array says nothing about its elements and is listed as not exercised), and, where the contract's Shape says MapOf, the values of the map folded into one position with the keys
// ignored. It holds no value of the body.
type Skel struct{ root *node }

// Skeleton derives the skeleton of body. shape is the contract's Shape: it says where a map is (the zero Shape says nowhere). The errors are a *ParseError, or
// ErrShapeUnreadable when the Shape cannot be walked.
func Skeleton(body []byte, shape modclient.Shape) (*Skel, error) {
	v, err := parse(body)
	if err != nil {
		return nil, err
	}
	sv, err := viewOf(shape)
	if err != nil {
		return nil, err
	}
	return &Skel{root: build(v, sv)}, nil
}

// String lists every position in document order, one line each: `path: type|type`, with `(not exercised: why)` where the position tells nothing about what is below it. The
// document itself is `(body)`.
func (s *Skel) String() string {
	if s == nil || s.root == nil {
		return ""
	}
	var b strings.Builder
	writeNode(&b, "", s.root)
	return b.String()
}

func writeNode(b *strings.Builder, path string, n *node) {
	fmt.Fprintf(b, "%s: %s", display(path), strings.Join(n.types.names(), "|"))
	if why := n.unexercised(); why != "" {
		fmt.Fprintf(b, " (not exercised: %s)", why)
	}
	b.WriteByte('\n')
	for _, k := range n.keys {
		writeNode(b, join(path, safeKey(k)), n.kids[k])
	}
	if n.mapElem != nil {
		writeNode(b, path+"{}", n.mapElem)
	}
	if n.elem != nil {
		writeNode(b, path+"[]", n.elem)
	}
}

// Gap is a position a comparison could not judge: an empty array, an empty map or a null says nothing about what is below it.
type Gap struct {
	// Side is "fixture" or "live" in a comparison, "" in a skeleton on its own.
	Side string
	Path string
	Why  string
}

// Gaps lists the positions of the skeleton that tell nothing about what is below them, in document order.
func (s *Skel) Gaps() []Gap {
	if s == nil || s.root == nil {
		return nil
	}
	var out []Gap
	collectGaps("", s.root, "", &out)
	return out
}

func collectGaps(path string, n *node, side string, out *[]Gap) {
	if why := n.unexercised(); why != "" {
		*out = append(*out, Gap{Side: side, Path: display(path), Why: why})
	}
	for _, k := range n.keys {
		collectGaps(join(path, safeKey(k)), n.kids[k], side, out)
	}
	if n.mapElem != nil {
		collectGaps(path+"{}", n.mapElem, side, out)
	}
	if n.elem != nil {
		collectGaps(path+"[]", n.elem, side, out)
	}
}

// Position is what a skeleton says about one path.
type Position struct {
	// Types are the JSON types seen there, alphabetically.
	Types []string
	// Integral is true when every number seen there is an integer.
	Integral bool
	// NotExercised is why the position tells nothing about what is below it ("" when it does).
	NotExercised string
}

// Lookup finds a path (the grammar above; "" is the document itself).
func (s *Skel) Lookup(path string) (Position, bool) {
	if s == nil || s.root == nil {
		return Position{}, false
	}
	segs, err := parsePath(path)
	if err != nil {
		return Position{}, false
	}
	n, st := resolve(s.root, segs)
	if st != found {
		return Position{}, false
	}
	return Position{Types: n.types.names(), Integral: n.integral, NotExercised: n.unexercised()}, true
}

type segKind uint8

const (
	sKey segKind = iota
	sElem
	sMap
)

type seg struct {
	kind segKind
	name string
}

// parsePath reads a path of the grammar above. "" is the document itself (no segments).
func parsePath(p string) ([]seg, error) {
	var out []seg
	i := 0
	// afterName: a name has just been read (a suffix or a dot may follow); afterSuffix: `[]` or `{}` was read (a suffix or a dot may follow).
	prevNeedsName := false
	for i < len(p) {
		switch {
		case strings.HasPrefix(p[i:], "[]"):
			if prevNeedsName {
				return nil, fmt.Errorf("a name must follow the dot")
			}
			out = append(out, seg{kind: sElem})
			i += 2
		case strings.HasPrefix(p[i:], "{}"):
			if prevNeedsName {
				return nil, fmt.Errorf("a name must follow the dot")
			}
			out = append(out, seg{kind: sMap})
			i += 2
		case p[i] == '.':
			if len(out) == 0 || prevNeedsName {
				return nil, fmt.Errorf("a dot needs a name before it")
			}
			prevNeedsName = true
			i++
		default:
			j := i
			for j < len(p) && isNameByte(p[j]) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected character at offset %d", i)
			}
			if len(out) > 0 && !prevNeedsName {
				return nil, fmt.Errorf("a name must be joined to its parent with a dot")
			}
			out = append(out, seg{kind: sKey, name: p[i:j]})
			prevNeedsName = false
			i = j
		}
	}
	if prevNeedsName {
		return nil, fmt.Errorf("the path ends with a dot")
	}
	return out, nil
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

type resolution uint8

const (
	found resolution = iota
	// beyond: the path goes below a position that tells nothing about what is there (null, an empty array or map): it cannot be judged, not wrong.
	beyond
	missing
)

// resolve walks a path down a skeleton.
func resolve(n *node, segs []seg) (*node, resolution) {
	for _, s := range segs {
		if n == nil {
			return nil, beyond
		}
		if n.types == tNull {
			return nil, beyond
		}
		switch s.kind {
		case sKey:
			if n.types&tObject == 0 || n.isMap {
				return nil, missing
			}
			kid, ok := n.kids[s.name]
			if !ok {
				return nil, missing
			}
			n = kid
		case sElem:
			if n.types&tArray == 0 {
				return nil, missing
			}
			if n.elem == nil {
				return nil, beyond
			}
			n = n.elem
		case sMap:
			if !n.isMap {
				return nil, missing
			}
			if n.mapElem == nil {
				return nil, beyond
			}
			n = n.mapElem
		}
	}
	return n, found
}

// collectStrings gathers the string values at a path of a parsed body (other types at the path are not vocabulary values and are skipped).
func collectStrings(v value, segs []seg, out *[]string) {
	if len(segs) == 0 {
		if v.kind == vString {
			*out = append(*out, v.str)
		}
		return
	}
	s := segs[0]
	switch s.kind {
	case sKey:
		if v.kind == vObject {
			for _, m := range v.members {
				if m.key == s.name {
					collectStrings(m.val, segs[1:], out)
				}
			}
		}
	case sElem:
		if v.kind == vArray {
			for _, it := range v.items {
				collectStrings(it, segs[1:], out)
			}
		}
	case sMap:
		if v.kind == vObject {
			for _, m := range v.members {
				collectStrings(m.val, segs[1:], out)
			}
		}
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
