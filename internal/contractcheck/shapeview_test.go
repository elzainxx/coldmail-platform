package contractcheck

import (
	"errors"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func TestViewOfReadsEveryKindOfShape(t *testing.T) {
	cases := []struct {
		name string
		sh   modclient.Shape
		want skind
		null bool
	}{
		{"zero", modclient.Shape{}, skUnchecked, false},
		{"any", modclient.Any, skAny, false},
		{"string", modclient.Str, skString, false},
		{"number", modclient.Num, skNumber, false},
		{"integer", modclient.Int, skInteger, false},
		{"bool", modclient.Bool, skBool, false},
		{"object", modclient.Object(), skObject, false},
		{"map", modclient.MapOf(modclient.Str), skMap, false},
		{"array", modclient.ArrayOf(modclient.Str), skArray, false},
		{"nullable string", modclient.Str.OrNull(), skString, true},
		{"nullable object", modclient.Object().OrNull(), skObject, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := viewOf(c.sh)
			if err != nil {
				t.Fatal(err)
			}
			if v.kind != c.want || v.nullable != c.null {
				t.Fatalf("view = kind %d nullable %v, want kind %d nullable %v", v.kind, v.nullable, c.want, c.null)
			}
		})
	}
}

func TestViewOfKeepsRequiredAndOptionalApart(t *testing.T) {
	v, err := viewOf(modclient.Object(modclient.Req("a", modclient.Str), modclient.Opt("b", modclient.Int)))
	if err != nil {
		t.Fatal(err)
	}
	if f := v.field("a"); f == nil || !f.required || f.view.kind != skString {
		t.Fatalf("a must be a required string: %+v", f)
	}
	if f := v.field("b"); f == nil || f.required || f.view.kind != skInteger {
		t.Fatalf("b must be an optional integer: %+v", f)
	}
	if v.field("c") != nil {
		t.Fatal("an unlisted key has no field")
	}
	// The paired control: with the two swapped the view must swap too.
	w, _ := viewOf(modclient.Object(modclient.Opt("a", modclient.Str), modclient.Req("b", modclient.Int)))
	if w.field("a").required || !w.field("b").required {
		t.Fatal("the view does not follow Req and Opt")
	}
}

func TestViewOfWalksNestedObjectsArraysAndMaps(t *testing.T) {
	v, err := viewOf(jobShape)
	if err != nil {
		t.Fatal(err)
	}
	if v.field("counts").view.field("total").view.kind != skInteger {
		t.Fatal("counts.total must be an integer")
	}
	items := v.field("items").view
	if items.kind != skArray || items.elem.kind != skObject || items.elem.field("note") == nil || items.elem.field("note").required {
		t.Fatal("items must be an array of objects with an optional note")
	}
	checks := v.field("checks").view
	if !checks.isMap() || checks.elem.field("ready").view.kind != skBool {
		t.Fatal("checks must be a map of objects with a boolean ready")
	}
	if !v.field("reason").view.nullable {
		t.Fatal("reason must be nullable")
	}
}

func TestViewOfFailsLoudlyOnAKindOfShapeItWasNotTaughtAndKnowsEveryKindModclientNames(t *testing.T) {
	if _, err := skindOf(modclient.ShapeKind(200)); !errors.Is(err, ErrShapeUnreadable) {
		t.Fatalf("an unknown kind must be ErrShapeUnreadable, got %v", err)
	}
	// The paired control, and the guard against a new kind being added to modclient and forgotten here: every kind that has a name must be handled.
	n := 0
	for k := modclient.ShapeUnchecked; k < modclient.ShapeKind(64); k++ {
		if k.String() == "unknown" {
			continue
		}
		n++
		if _, err := skindOf(k); err != nil {
			t.Fatalf("modclient names the kind %s and this package does not handle it: %v", k, err)
		}
	}
	if n < 9 {
		t.Fatalf("only %d named kinds: the instrument matched nothing", n)
	}
}

func TestViewOfRefusesAShapeNestedBeyondTheDepthLimit(t *testing.T) {
	sh := modclient.Str
	for i := 0; i < MaxDepth+5; i++ {
		sh = modclient.ArrayOf(sh)
	}
	if _, err := viewOf(sh); !errors.Is(err, ErrShapeUnreadable) {
		t.Fatalf("a Shape nested %d deep must be refused, got %v", MaxDepth+5, err)
	}
	ok := modclient.Str
	for i := 0; i < 10; i++ {
		ok = modclient.ArrayOf(ok)
	}
	if _, err := viewOf(ok); err != nil {
		t.Fatalf("a Shape nested 10 deep is fine: %v", err)
	}
}
