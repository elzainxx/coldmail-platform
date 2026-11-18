package modclient

import (
	"reflect"
	"testing"
)

func TestShapeOpen_ReturnsACopyMarkedOpenAndLeavesTheOriginalAlone(t *testing.T) {
	plain := Object(Req("a", Str), Opt("b", Int))
	open := plain.Open()
	if plain.IsOpen() {
		t.Fatal("Open must not change the Shape it was called on")
	}
	if !open.IsOpen() || open.Kind() != ShapeObject {
		t.Fatalf("the copy must be an open object: open=%v kind=%s", open.IsOpen(), open.Kind())
	}
	if !reflect.DeepEqual(open.Fields(), plain.Fields()) {
		t.Fatalf("the listed keys must be kept: %+v vs %+v", open.Fields(), plain.Fields())
	}
	// The mark survives the other modifier, and does not depend on it.
	if n := open.OrNull(); !n.IsOpen() || !n.Nullable() {
		t.Fatalf("OrNull on an open object: open=%v nullable=%v", n.IsOpen(), n.Nullable())
	}
	if n := plain.OrNull().Open(); !n.IsOpen() || !n.Nullable() {
		t.Fatalf("Open on a nullable object: open=%v nullable=%v", n.IsOpen(), n.Nullable())
	}
}

func TestShapeOpen_MeansSomethingOnlyOnAnObject(t *testing.T) {
	for name, sh := range map[string]Shape{"string": Str, "number": Num, "integer": Int, "bool": Bool, "any": Any, "map": MapOf(Str), "array": ArrayOf(Str), "zero": Shape{}} {
		if sh.Open().IsOpen() {
			t.Fatalf("%s: Open is meaningful on an object only", name)
		}
	}
	// The paired control: the same call on an object is.
	if !Object().Open().IsOpen() {
		t.Fatal("an object must be open after Open")
	}
}

func TestShapeOpen_ANestedOpenObjectIsReachableThroughTheWalk(t *testing.T) {
	sh := Object(Req("results", ArrayOf(Object(
		Req("email", Str),
		Req("signals", Object(Req("syntax_valid", Bool)).Open()),
	))))
	row, ok := sh.Fields()[0].Shape.Elem()
	if !ok || row.IsOpen() {
		t.Fatalf("the row is a plain object: ok=%v open=%v", ok, row.IsOpen())
	}
	fs := row.Fields()
	if len(fs) != 2 || fs[0].Shape.IsOpen() || !fs[1].Shape.IsOpen() || fs[1].Name != "signals" {
		t.Fatalf("only signals is open: %+v", fs)
	}
	if got := fs[1].Shape.Fields(); len(got) != 1 || got[0].Name != "syntax_valid" || !got[0].Required {
		t.Fatalf("the listed key of an open object is still listed and required: %+v", got)
	}
}

func TestShapeOpen_DecodingIsExactlyWhatItWas(t *testing.T) {
	build := func(open bool) Shape {
		s := Object(Req("syntax_valid", Bool), Opt("mx_provider", Str))
		if open {
			s = s.Open()
		}
		return Object(Req("signals", s))
	}
	bodies := map[string]string{
		"extra keys":                 `{"signals":{"syntax_valid":true,"mx_found":false,"has_typo":null,"anything":{"nested":[1]}}}`,
		"only the listed key":        `{"signals":{"syntax_valid":false}}`,
		"the optional key present":   `{"signals":{"syntax_valid":true,"mx_provider":"example"}}`,
		"a required key missing":     `{"signals":{"mx_found":true}}`,
		"a listed key wrongly typed": `{"signals":{"syntax_valid":"yes"}}`,
		"the optional key mistyped":  `{"signals":{"syntax_valid":true,"mx_provider":4}}`,
		"signals not an object":      `{"signals":[]}`,
	}
	for name, body := range bodies {
		plain := Decode(&Response{Body: []byte(body)}, nil, build(false))
		open := Decode(&Response{Body: []byte(body)}, nil, build(true))
		if (plain == nil) != (open == nil) || (plain != nil && plain.Error() != open.Error()) {
			t.Fatalf("%s: plain says %v, open says %v: Open must not change decoding", name, plain, open)
		}
	}
	// And the two ends are what they should be (the paired controls of the loop above): extra keys are accepted, a missing required key is refused with its key named.
	if err := Decode(&Response{Body: []byte(bodies["extra keys"])}, nil, build(true)); err != nil {
		t.Fatalf("extra keys under an open object: %v", err)
	}
	err := Decode(&Response{Body: []byte(bodies["a required key missing"])}, nil, build(true))
	if e := asErr(t, err); e.Kind != Contract || e.Key != "signals.syntax_valid" {
		t.Fatalf("a missing required key must still be refused under an open object: %v", err)
	}
	// Into a value, the decoded result is the same too.
	var a, b struct {
		Signals struct {
			SyntaxValid bool `json:"syntax_valid"`
		} `json:"signals"`
	}
	if err := Decode(&Response{Body: []byte(bodies["extra keys"])}, &a, build(false)); err != nil {
		t.Fatal(err)
	}
	if err := Decode(&Response{Body: []byte(bodies["extra keys"])}, &b, build(true)); err != nil || a != b || !a.Signals.SyntaxValid {
		t.Fatalf("decoded %+v vs %+v (%v)", a, b, err)
	}
}
