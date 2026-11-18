package contractcheck

import (
	"fmt"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The comparison needs two facts about a contract's Shape that modclient.Decode does not give back: where a MapOf is (so that the keys of a map are not compared) and which object
// keys are optional (so that an absent optional key is not a difference). It reads them through the read-only walk of modclient.Shape (Kind, Nullable, Fields, Elem) and keeps
// them in a view of its own. A kind of Shape this file has not been taught about is refused with ErrShapeUnreadable and reported, never guessed.

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
	open     bool // an object that is a bag of module-defined keys (modclient.Shape.Open)
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

func (s *sview) isOpen() bool { return s != nil && s.kind == skObject && s.open }

// skindOf maps a kind of the modclient walk to this package's; a kind it does not know is an error.
func skindOf(k modclient.ShapeKind) (skind, error) {
	switch k {
	case modclient.ShapeUnchecked:
		return skUnchecked, nil
	case modclient.ShapeAny:
		return skAny, nil
	case modclient.ShapeString:
		return skString, nil
	case modclient.ShapeNumber:
		return skNumber, nil
	case modclient.ShapeInteger:
		return skInteger, nil
	case modclient.ShapeBool:
		return skBool, nil
	case modclient.ShapeObject:
		return skObject, nil
	case modclient.ShapeMap:
		return skMap, nil
	case modclient.ShapeArray:
		return skArray, nil
	}
	return 0, fmt.Errorf("%w: a kind of Shape this package has never seen (%s)", ErrShapeUnreadable, k)
}

// viewOf turns a Shape into a view. It fails with ErrShapeUnreadable for a Shape nested deeper than MaxDepth and for a kind it does not know.
func viewOf(sh modclient.Shape) (*sview, error) { return viewShape(sh, 0) }

func viewShape(sh modclient.Shape, depth int) (*sview, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("%w: the Shape nests deeper than %d", ErrShapeUnreadable, MaxDepth)
	}
	k, err := skindOf(sh.Kind())
	if err != nil {
		return nil, err
	}
	out := &sview{kind: k, nullable: sh.Nullable(), open: sh.IsOpen()}
	for _, f := range sh.Fields() {
		child, err := viewShape(f.Shape, depth+1)
		if err != nil {
			return nil, err
		}
		out.fields = append(out.fields, sfield{name: f.Name, view: child, required: f.Required})
	}
	if el, ok := sh.Elem(); ok {
		child, err := viewShape(el, depth+1)
		if err != nil {
			return nil, err
		}
		out.elem = child
	}
	return out, nil
}
