package contractcheck

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// A modclient.Shape keeps its structure in unexported fields and offers no accessor: the only thing the package lets a caller do with one is hand it to Decode. The drift
// comparison needs two facts Decode does not give back (where a MapOf is, so that the keys of a map are not compared; and which object keys are optional, so that an absent
// optional key is not a difference), so this file READS those fields with reflection (read-only: no Interface, no Set) and turns a Shape into a view of its own.
//
// It is guarded, not trusted: the numeric values of the kind field are learned at first use by reading the Shapes modclient builds itself (Str, Num, Int, Bool, Any, Object,
// MapOf, ArrayOf and the zero Shape), and any surprise (a field renamed, two kinds that read alike) makes every view fail with ErrShapeUnreadable, which the callers report as a
// problem instead of guessing. The cleaner fix is an exported read-only walk on modclient.Shape; until it exists this shim is the single place that knows the field names.

type skind uint8

const (
	skUnchecked skind = iota
	skAny
	skString
	skNumber
	skInteger
	skBool
	skObject
	skMap
	skArray
)

// sview is what a Shape says about one position of a body.
type sview struct {
	kind     skind
	nullable bool
	fields   []sfield
	elem     *sview
}

type sfield struct {
	name     string
	view     *sview
	required bool
}

// field returns the listed key of an object view, or nil (not an object, or not listed). It is safe on a nil view.
func (s *sview) field(name string) *sfield {
	if s == nil || s.kind != skObject {
		return nil
	}
	for i := range s.fields {
		if s.fields[i].name == name {
			return &s.fields[i]
		}
	}
	return nil
}

func (s *sview) elemView() *sview {
	if s == nil {
		return nil
	}
	return s.elem
}

func (s *sview) isMap() bool { return s != nil && s.kind == skMap }

var (
	kindOnce  sync.Once
	kindTable map[uint64]skind
	kindErr   error
)

func readKind(sh modclient.Shape) (k uint64, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrShapeUnreadable, r)
		}
	}()
	f := reflect.ValueOf(sh).FieldByName("kind")
	if !f.IsValid() || f.Kind() != reflect.Uint8 {
		return 0, fmt.Errorf("%w: no uint8 field named kind", ErrShapeUnreadable)
	}
	return f.Uint(), nil
}

func learnKinds() {
	known := []struct {
		sh modclient.Shape
		k  skind
	}{
		{modclient.Shape{}, skUnchecked},
		{modclient.Any, skAny},
		{modclient.Str, skString},
		{modclient.Num, skNumber},
		{modclient.Int, skInteger},
		{modclient.Bool, skBool},
		{modclient.Object(), skObject},
		{modclient.MapOf(modclient.Str), skMap},
		{modclient.ArrayOf(modclient.Str), skArray},
	}
	tbl := map[uint64]skind{}
	for _, e := range known {
		n, err := readKind(e.sh)
		if err != nil {
			kindErr = err
			return
		}
		if prev, dup := tbl[n]; dup {
			kindErr = fmt.Errorf("%w: kinds %d and %d read alike", ErrShapeUnreadable, prev, e.k)
			return
		}
		tbl[n] = e.k
	}
	kindTable = tbl
}

// viewOf turns a Shape into a view. It fails with ErrShapeUnreadable when modclient's Shape no longer has the internals this reads.
func viewOf(sh modclient.Shape) (v *sview, err error) {
	kindOnce.Do(learnKinds)
	if kindErr != nil {
		return nil, kindErr
	}
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("%w: %v", ErrShapeUnreadable, r)
		}
	}()
	return viewValue(reflect.ValueOf(sh), 0)
}

func viewValue(rv reflect.Value, depth int) (*sview, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("%w: the Shape nests deeper than %d", ErrShapeUnreadable, MaxDepth)
	}
	kf, nf, ff, ef := rv.FieldByName("kind"), rv.FieldByName("nullable"), rv.FieldByName("fields"), rv.FieldByName("elem")
	if !kf.IsValid() || !nf.IsValid() || !ff.IsValid() || !ef.IsValid() {
		return nil, fmt.Errorf("%w: a field of Shape is gone", ErrShapeUnreadable)
	}
	k, ok := kindTable[kf.Uint()]
	if !ok {
		return nil, fmt.Errorf("%w: a kind of Shape this package has never seen", ErrShapeUnreadable)
	}
	out := &sview{kind: k, nullable: nf.Bool()}
	for i := 0; i < ff.Len(); i++ {
		f := ff.Index(i)
		child, err := viewValue(f.FieldByName("Shape"), depth+1)
		if err != nil {
			return nil, err
		}
		out.fields = append(out.fields, sfield{name: f.FieldByName("Name").String(), view: child, required: f.FieldByName("Required").Bool()})
	}
	if !ef.IsNil() {
		child, err := viewValue(ef.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		out.elem = child
	}
	return out, nil
}
