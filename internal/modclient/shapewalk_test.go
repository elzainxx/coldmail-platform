package modclient

import (
	"reflect"
	"testing"
)

func TestShapeWalk_EveryConstructorReportsItsKind(t *testing.T) {
	cases := []struct {
		name string
		sh   Shape
		want ShapeKind
	}{
		{"zero", Shape{}, ShapeUnchecked},
		{"any", Any, ShapeAny},
		{"string", Str, ShapeString},
		{"number", Num, ShapeNumber},
		{"integer", Int, ShapeInteger},
		{"bool", Bool, ShapeBool},
		{"object", Object(), ShapeObject},
		{"map", MapOf(Str), ShapeMap},
		{"array", ArrayOf(Str), ShapeArray},
	}
	seen := map[ShapeKind]string{}
	for _, c := range cases {
		if got := c.sh.Kind(); got != c.want {
			t.Fatalf("%s: Kind = %s, want %s", c.name, got, c.want)
		}
		if prev, dup := seen[c.sh.Kind()]; dup {
			t.Fatalf("%s and %s report the same kind: the walk would not tell them apart", c.name, prev)
		}
		seen[c.sh.Kind()] = c.name
	}
}

func TestShapeWalk_KindNamesAreDistinctAndOnlyUnknownKindsAreUnknown(t *testing.T) {
	names := map[string]ShapeKind{}
	for k := ShapeUnchecked; k <= ShapeArray; k++ {
		n := k.String()
		if n == "unknown" || n == "" {
			t.Fatalf("kind %d has no name", k)
		}
		if prev, dup := names[n]; dup {
			t.Fatalf("kinds %d and %d share the name %q", prev, k, n)
		}
		names[n] = k
	}
	if ShapeKind(200).String() != "unknown" {
		t.Fatal("a kind nobody defined must say so")
	}
}

func TestShapeWalk_NullableFollowsOrNull(t *testing.T) {
	if Str.Nullable() || Object().Nullable() || ArrayOf(Str).Nullable() {
		t.Fatal("a plain Shape is not nullable")
	}
	if !Str.OrNull().Nullable() || !Object().OrNull().Nullable() || !MapOf(Str).OrNull().Nullable() {
		t.Fatal("OrNull must show")
	}
	if Str.OrNull().Kind() != ShapeString {
		t.Fatal("OrNull must not change the kind")
	}
}

func TestShapeWalk_FieldsKeepOrderRequiredAndNestedShapes(t *testing.T) {
	sh := Object(Req("a", Str), Opt("b", Int), Req("c", ArrayOf(Object(Req("d", Bool)))))
	fs := sh.Fields()
	if len(fs) != 3 || fs[0].Name != "a" || !fs[0].Required || fs[1].Name != "b" || fs[1].Required || fs[2].Name != "c" {
		t.Fatalf("fields = %+v", fs)
	}
	if fs[1].Shape.Kind() != ShapeInteger {
		t.Fatal("a field carries its own shape")
	}
	el, ok := fs[2].Shape.Elem()
	if !ok || el.Kind() != ShapeObject || len(el.Fields()) != 1 || el.Fields()[0].Name != "d" {
		t.Fatalf("nested shape not reachable: %+v %v", el, ok)
	}
	// The paired control: swapping Req and Opt is visible.
	sw := Object(Opt("a", Str), Req("b", Int))
	if sw.Fields()[0].Required || !sw.Fields()[1].Required {
		t.Fatal("the walk does not follow Req and Opt")
	}
}

func TestShapeWalk_FieldsAreACopyAndOnlyObjectsHaveThem(t *testing.T) {
	sh := Object(Req("a", Str))
	fs := sh.Fields()
	fs[0].Name = "changed"
	fs[0].Required = false
	if got := sh.Fields()[0]; got.Name != "a" || !got.Required {
		t.Fatalf("changing the returned slice changed the Shape: %+v", got)
	}
	for _, other := range []Shape{Str, MapOf(Object(Req("a", Str))), ArrayOf(Object(Req("a", Str))), Shape{}, Object()} {
		if other.Fields() != nil {
			t.Fatalf("%s has no fields to list", other.Kind())
		}
	}
}

func TestShapeWalk_ElemExistsOnlyForArraysAndMaps(t *testing.T) {
	for _, sh := range []Shape{ArrayOf(Int), MapOf(Int)} {
		el, ok := sh.Elem()
		if !ok || el.Kind() != ShapeInteger {
			t.Fatalf("%s: elem = %v %v", sh.Kind(), el.Kind(), ok)
		}
	}
	for _, sh := range []Shape{Str, Num, Any, Bool, Shape{}, Object(Req("a", Str))} {
		if _, ok := sh.Elem(); ok {
			t.Fatalf("%s has no element shape", sh.Kind())
		}
	}
}

func TestShapeWalk_AWalkOfTheDocumentedExampleMatchesWhatDecodeAccepts(t *testing.T) {
	sh := Object(
		Req("job_id", Str),
		Req("counts", Object(Req("total", Int))),
		Opt("provider_message_id", Str),
		Req("reason", Str.OrNull()),
		Req("results", ArrayOf(Object(Req("state", Str)))),
	)
	var paths []string
	var walk func(prefix string, s Shape)
	walk = func(prefix string, s Shape) {
		paths = append(paths, prefix+":"+s.Kind().String())
		for _, f := range s.Fields() {
			walk(prefix+"."+f.Name, f.Shape)
		}
		if el, ok := s.Elem(); ok {
			walk(prefix+"[]", el)
		}
	}
	walk("$", sh)
	want := []string{"$:object", "$.job_id:string", "$.counts:object", "$.counts.total:integer", "$.provider_message_id:string", "$.reason:string", "$.results:array", "$.results[]:object", "$.results[].state:string"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("walk = %v, want %v", paths, want)
	}
}
