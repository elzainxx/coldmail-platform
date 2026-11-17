package contractcheck

import (
	"errors"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func skel(t *testing.T, body string, sh modclient.Shape) *Skel {
	t.Helper()
	s, err := Skeleton([]byte(body), sh)
	if err != nil {
		t.Fatalf("Skeleton(%q): %v", body, err)
	}
	return s
}

func TestSkeletonListsKeysAndTypesInDocumentOrder(t *testing.T) {
	got := skel(t, `{"id":"x","n":1,"ok":true,"gone":null,"obj":{"k":"v"},"list":[1,2]}`, modclient.Shape{}).String()
	want := "(body): object\n" +
		"id: string\n" +
		"n: number\n" +
		"ok: boolean\n" +
		"gone: null (not exercised: null only)\n" +
		"obj: object\n" +
		"obj.k: string\n" +
		"list: array\n" +
		"list[]: number\n"
	if got != want {
		t.Fatalf("skeleton:\n%s\nwant:\n%s", got, want)
	}
}

func TestSkeletonFoldsArrayElementsIntoTheUnionOfTheirSkeletons(t *testing.T) {
	s := skel(t, `{"items":[{"a":1,"b":"x"},{"a":"s","c":null},{"a":2,"d":[1]}]}`, modclient.Shape{})
	p, ok := s.Lookup("items[].a")
	if !ok || strings.Join(p.Types, "|") != "number|string" {
		t.Fatalf("items[].a = %v %v, want number|string", p, ok)
	}
	for _, key := range []string{"items[].b", "items[].c", "items[].d", "items[].d[]"} {
		if _, ok := s.Lookup(key); !ok {
			t.Fatalf("the union must hold %s", key)
		}
	}
	if _, ok := s.Lookup("items[].zz"); ok {
		t.Fatal("a key no element has must not be in the union (paired control)")
	}
}

func TestSkeletonEmptyArrayIsNotExercisedAndANonEmptyOneIs(t *testing.T) {
	empty := skel(t, `{"items":[],"other":[[]]}`, modclient.Shape{})
	gaps := empty.Gaps()
	if len(gaps) != 2 || gaps[0].Path != "items" || gaps[0].Why != "empty array" || gaps[1].Path != "other[]" {
		t.Fatalf("gaps = %+v, want items and other[] (empty arrays)", gaps)
	}
	if g := skel(t, `{"items":[{"a":1}]}`, modclient.Shape{}).Gaps(); len(g) != 0 {
		t.Fatalf("a non-empty array has no gap, got %+v", g)
	}
}

func TestSkeletonMapKeysAreIgnoredOnlyWhereTheShapeSaysMapOf(t *testing.T) {
	body := `{"checks":{"db":{"ready":true},"queue":{"ready":false}}}`
	withMap := skel(t, body, modclient.Object(modclient.Req("checks", modclient.MapOf(modclient.Object(modclient.Req("ready", modclient.Bool))))))
	if _, ok := withMap.Lookup("checks{}.ready"); !ok {
		t.Fatal("a MapOf position must expose checks{}.ready")
	}
	if _, ok := withMap.Lookup("checks.db"); ok {
		t.Fatal("the keys of a MapOf must not be positions of the skeleton")
	}
	// The paired control: without the MapOf the same body is an object with two keys (maps are never guessed).
	plain := skel(t, body, modclient.Shape{})
	if _, ok := plain.Lookup("checks.db.ready"); !ok {
		t.Fatal("without a MapOf the keys are positions")
	}
	if _, ok := plain.Lookup("checks{}.ready"); ok {
		t.Fatal("without a MapOf there is no checks{} position")
	}
}

func TestSkeletonEmptyMapIsNotExercised(t *testing.T) {
	s := skel(t, `{"checks":{}}`, modclient.Object(modclient.Req("checks", modclient.MapOf(modclient.Str))))
	g := s.Gaps()
	if len(g) != 1 || g[0].Path != "checks" || g[0].Why != "empty map" {
		t.Fatalf("gaps = %+v", g)
	}
}

func TestSkeletonKnowsWhetherEveryNumberAtAPositionIsAnInteger(t *testing.T) {
	cases := map[string]bool{`{"n":1}`: true, `{"n":-7}`: true, `{"n":1.5}`: false, `{"n":1e3}`: false, `{"n":1.0}`: false, `{"n":[1,2.5]}`: false}
	for body, want := range cases {
		s := skel(t, body, modclient.Shape{})
		path := "n"
		if strings.Contains(body, "[") {
			path = "n[]"
		}
		p, ok := s.Lookup(path)
		if !ok || p.Integral != want {
			t.Fatalf("%s: Integral = %v, want %v", body, p.Integral, want)
		}
	}
}

func TestSkeletonRefusesWhatTheParserRefusesWithTheSameKind(t *testing.T) {
	for body, want := range map[string]ParseKind{"": ParseEmpty, `{"a":`: ParseSyntax, `{"a":1,"a":1}`: ParseDuplicateKey, `[` + strings.Repeat("[", 100): ParseTooDeep} {
		s, err := Skeleton([]byte(body), modclient.Shape{})
		if s != nil {
			t.Fatalf("an error must come with no skeleton (%q)", body)
		}
		if ParseKindOf(err) != want {
			t.Fatalf("%q: kind = %v, want %v", body, err, want)
		}
	}
}

func TestParsePathGrammar(t *testing.T) {
	good := []string{"", "state", "error.code", "items[].state", "[]", "[].id", "a[][]", "checks{}.status", "a-b_c.d9", "m{}[]"}
	for _, p := range good {
		if _, err := parsePath(p); err != nil {
			t.Fatalf("%q must parse: %v", p, err)
		}
	}
	bad := []string{".", ".a", "a.", "a..b", "a[]b", "[]a", "a b", "items[0].x", "a.[]", "a?b", "a{}b", "a/b"}
	for _, p := range bad {
		if _, err := parsePath(p); err == nil {
			t.Fatalf("%q must be refused", p)
		}
	}
	segs, _ := parsePath("items[].state")
	if len(segs) != 3 || segs[0].kind != sKey || segs[1].kind != sElem || segs[2].name != "state" {
		t.Fatalf("segments = %+v", segs)
	}
}

func TestResolveSeparatesAMissingPositionFromOneBeyondAnUnexercisedPosition(t *testing.T) {
	s := skel(t, `{"items":[],"gone":null,"obj":{"a":1}}`, modclient.Shape{})
	check := func(path string, want resolution) {
		t.Helper()
		segs, err := parsePath(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, got := resolve(s.root, segs); got != want {
			t.Fatalf("resolve(%q) = %d, want %d", path, got, want)
		}
	}
	check("obj.a", found)
	check("items[].anything", beyond)
	check("gone.anything", beyond)
	check("obj.zz", missing)
	check("obj[]", missing)
	check("nothere", missing)
}

func TestLookupReportsTheTypesOfAPositionAndRefusesWhatIsNotThere(t *testing.T) {
	s := skel(t, `{"a":{"b":[{"c":"x"}]}}`, modclient.Shape{})
	if p, ok := s.Lookup("a.b[].c"); !ok || strings.Join(p.Types, "|") != "string" {
		t.Fatalf("a.b[].c = %v %v", p, ok)
	}
	if p, ok := s.Lookup(""); !ok || strings.Join(p.Types, "|") != "object" {
		t.Fatalf("the document itself = %v %v", p, ok)
	}
	for _, bad := range []string{"a.zz", "a.b.c", "not a path", "a.b[].c.d"} {
		if _, ok := s.Lookup(bad); ok {
			t.Fatalf("%q must not be found", bad)
		}
	}
}

func TestSkeletonOfTheJobBodyHasTheMapAsOnePosition(t *testing.T) {
	s := skel(t, jobBody, jobShape)
	for _, path := range []string{"job_id", "counts.total", "items[].note", "checks{}.ready", "created_at"} {
		if _, ok := s.Lookup(path); !ok {
			t.Fatalf("%s must be in the skeleton:\n%s", path, s)
		}
	}
	if _, ok := s.Lookup("checks.db"); ok {
		t.Fatal("the keys of the map must not be in the skeleton")
	}
}

func TestSkeletonReportsAShapeItCannotReadAsTypedError(t *testing.T) {
	// ErrShapeUnreadable is reachable through the same path the real Shapes take: a Shape deeper than the limit.
	sh := modclient.Str
	for i := 0; i < MaxDepth+2; i++ {
		sh = modclient.ArrayOf(sh)
	}
	s, err := Skeleton([]byte(`[]`), sh)
	if s != nil || !errors.Is(err, ErrShapeUnreadable) {
		t.Fatalf("want no skeleton and ErrShapeUnreadable, got %v %v", s, err)
	}
}

func TestSkeletonUnionsNestedArraysAcrossTheElementsOfAnArray(t *testing.T) {
	s := skel(t, `{"items":[{"d":[]},{"d":[1]},{"d":["x"]}]}`, modclient.Shape{})
	p, ok := s.Lookup("items[].d[]")
	if !ok || strings.Join(p.Types, "|") != "number|string" {
		t.Fatalf("items[].d[] = %v %v, want number|string (the nested arrays of every element)", p, ok)
	}
	// The paired control: with only empty nested arrays the position is not exercised.
	if g := skel(t, `{"items":[{"d":[]},{"d":[]}]}`, modclient.Shape{}).Gaps(); len(g) != 1 || g[0].Path != "items[].d" {
		t.Fatalf("gaps = %+v", g)
	}
}
