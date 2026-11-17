package contractcheck

import (
	"fmt"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

type hostileCase struct {
	name string
	body string
	want ParseKind // 0: a document
}

func hostileCorpus() []hostileCase {
	manyDigits := strings.Repeat("9", 5000)
	bigArray := "[" + strings.Repeat("1,", 100000) + "1]"
	return []hostileCase{
		{"empty", "", ParseEmpty},
		{"white space", "  \r\n\t ", ParseEmpty},
		{"null", "null", 0},
		{"true", "true", 0},
		{"a bare number", "-12.5e3", 0},
		{"a bare string", `"x"`, 0},
		{"empty object", "{}", 0},
		{"empty array", "[]", 0},
		{"top level array of objects", `[{"id":"a"},{"id":"b"}]`, 0},
		{"top level array of arrays", `[[],[[]],[[[]]]]`, 0},
		{"truncated object", `{"a":`, ParseSyntax},
		{"truncated string", `{"a":"x`, ParseSyntax},
		{"truncated array", `[1,2`, ParseSyntax},
		{"trailing comma", `{"a":1,}`, ParseSyntax},
		{"single quotes", `{'a':1}`, ParseSyntax},
		{"comment", `/* x */ {}`, ParseSyntax},
		{"NaN", `{"a":NaN}`, ParseSyntax},
		{"raw newline in a string", "{\"a\":\"x\ny\"}", ParseSyntax},
		{"NUL byte", "{\"a\":\"x\x00y\"}", ParseSyntax},
		{"byte order mark", "\xef\xbb\xbf{}", ParseSyntax},
		{"two documents", `{}{}`, ParseTrailingData},
		{"garbage after", `{} garbage`, ParseTrailingData},
		{"deep arrays closed, 10000 levels", strings.Repeat("[", 10000) + strings.Repeat("]", 10000), ParseTooDeep},
		{"deep arrays unclosed, 10000 levels", strings.Repeat("[", 10000), ParseTooDeep},
		{"deep objects unclosed, 10000 levels", strings.Repeat(`{"a":`, 10000), ParseTooDeep},
		{"deep mixed, 100000 levels", strings.Repeat(`[{"a":`, 50000), ParseTooDeep},
		{"nesting at the limit", strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth), 0},
		{"huge exponent", `{"n":1e999999999,"m":-1e-999999999}`, 0},
		{"five thousand digits", `{"n":` + manyDigits + `}`, 0},
		{"duplicate key", `{"a":1,"a":2}`, ParseDuplicateKey},
		{"duplicate key in a nested array", `[{"a":1},{"a":1,"a":1}]`, ParseDuplicateKey},
		{"invalid UTF-8 in a value", "{\"a\":\"\xc3\x28\"}", ParseInvalidUTF8},
		{"invalid UTF-8 in a key", "{\"\xff\":1}", ParseInvalidUTF8},
		{"lone surrogate escape", `{"a":"\ud800"}`, 0},
		{"escaped NUL", `{"a":"\u0000"}`, 0},
		{"a very long key", `{"` + strings.Repeat("k", 100000) + `":1}`, 0},
		{"a very long string", `{"a":"` + strings.Repeat("x", 1<<20) + `"}`, 0},
		{"a hundred thousand elements", bigArray, 0},
		{"over the size limit", `"` + strings.Repeat("a", MaxBody) + `"`, ParseTooLarge},
	}
}

func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	f()
}

func TestHostileBodiesGetADefinedAnswerFromEveryParserAndNeverPanic(t *testing.T) {
	for _, c := range hostileCorpus() {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(c.body)
			noPanic(t, "parse", func() {
				_, err := parse(body)
				if ParseKindOf(err) != c.want {
					t.Fatalf("parse: kind = %v (%v), want %v", ParseKindOf(err), err, c.want)
				}
			})
			noPanic(t, "Skeleton", func() {
				for _, sh := range []modclient.Shape{{}, jobShape, modclient.ArrayOf(modclient.Any), modclient.MapOf(modclient.Any)} {
					s, err := Skeleton(body, sh)
					if (err == nil) != (c.want == 0) || (err == nil) != (s != nil) || ParseKindOf(err) != c.want {
						t.Fatalf("Skeleton: %v %v, want kind %v and a skeleton only on success", s != nil, err, c.want)
					}
				}
			})
			noPanic(t, "Normalise", func() {
				out, err := Normalise(body)
				if (err == nil) != (c.want == 0) || (err == nil) != (out != nil) || ParseKindOf(err) != c.want {
					t.Fatalf("Normalise: %v %v, want kind %v", out != nil, err, c.want)
				}
			})
			noPanic(t, "FindSecrets", func() {
				fs, err := FindSecrets(body)
				if ParseKindOf(err) != c.want || (err != nil && fs != nil) {
					t.Fatalf("FindSecrets: %v %v, want kind %v", fs, err, c.want)
				}
			})
		})
	}
}

func TestHostileBodiesAsLiveAndAsFixtureAreOneDefinedDifferenceEach(t *testing.T) {
	zero := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200}
	for _, c := range hostileCorpus() {
		t.Run(c.name, func(t *testing.T) {
			noPanic(t, "Compare", func() {
				asLive := Compare(zero, []byte(`{}`), []byte(c.body), 200)
				asFixture := Compare(zero, []byte(c.body), []byte(`{}`), 200)
				if c.want != 0 {
					wantDiffs(t, asLive, "ShapeViolation:(body)")
					wantDiffs(t, asFixture, "ContractInvalid:(body)")
					return
				}
				// A readable body against `{}`: never a refusal, and no ShapeViolation under the zero Shape.
				for _, rep := range []Report{asLive, asFixture} {
					if len(rep.Of(ShapeViolation))+len(rep.Of(ContractInvalid)) != 0 {
						t.Fatalf("a readable body must be compared:\n%s", detailOf(rep))
					}
				}
			})
		})
	}
}

func TestEveryReadableHostileBodyComparedWithItselfIsClean(t *testing.T) {
	zero := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200}
	n := 0
	for _, c := range hostileCorpus() {
		if c.want != 0 {
			continue
		}
		n++
		if rep := Compare(zero, []byte(c.body), []byte(c.body), 200); !rep.Clean() {
			t.Fatalf("%s: a body compared with itself differs:\n%s", c.name, detailOf(rep))
		}
	}
	if n < 10 {
		t.Fatalf("only %d readable bodies in the corpus: the instrument matched nothing", n)
	}
}

func TestNormalisedReadableHostileBodiesKeepTheirSkeleton(t *testing.T) {
	for _, c := range hostileCorpus() {
		if c.want != 0 || len(c.body) > 1<<18 {
			continue
		}
		out, err := Normalise([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		before, _ := Skeleton([]byte(c.body), modclient.Shape{})
		after, err := Skeleton(out, modclient.Shape{})
		if err != nil || before.String() != after.String() {
			t.Fatalf("%s: normalising changed the structure (%v)", c.name, err)
		}
	}
}

func TestHostileLogsGetADefinedAnswerFromParseRecorded(t *testing.T) {
	logs := map[string]string{
		"empty":               "",
		"only markers":        strings.Repeat(beginPrefix+"\n"+endMarker+"\n", 100),
		"binary":              "\x00\x01\x02\xff\xfe" + strings.Repeat("\x00", 1000),
		"a million equals":    strings.Repeat("=", 1<<20),
		"a begin never ended": beginJob + "\n" + strings.Repeat(`{"a":1}`+"\n", 1000),
		"deep body":           beginJob + "\n" + strings.Repeat("[", 10000) + "\n" + endMarker + "\n",
		"duplicate key body":  beginJob + "\n" + `{"a":1,"a":2}` + "\n" + endMarker + "\n",
		"many short prefixes": strings.Repeat("2026-11-17T21:32:01Z \n", 5000),
		"tabs":                strings.Repeat("a\tb\tc\t", 10000),
	}
	for name, log := range logs {
		noPanic(t, name, func() {
			recs, err := ParseRecorded([]byte(log))
			if (err == nil) == (recs == nil) {
				t.Fatalf("%s: want exactly one of blocks and error, got %d blocks and %v", name, len(recs), err)
			}
		})
	}
}

func TestHostileFixtureContentGetsADefinedProblemAndNeverPanics(t *testing.T) {
	for _, c := range hostileCorpus() {
		noPanic(t, c.name, func() {
			files := map[string]string{"job-done.json": c.body}
			got := CheckFixtures([]modclient.RouteContract{jobContract()}, fixtureFS(files), fxDir)
			if c.want != 0 {
				found := false
				for _, p := range got {
					found = found || p.Kind == NotJSON
				}
				if !found {
					t.Fatalf("%s: want NotJSON, got %v", c.name, kinds(got))
				}
			}
		})
	}
}

func TestHostileLiveBodiesThroughAssertLiveFailTheTestWithoutPanicking(t *testing.T) {
	for _, c := range hostileCorpus() {
		for _, record := range []bool{false, true} {
			noPanic(t, fmt.Sprintf("%s record=%v", c.name, record), func() {
				tb := &fakeTB{}
				AssertLive(tb, jobContract(), fixtureFS(jobFS()), fxDir, 200, []byte(c.body), newRec(record))
				if c.want != 0 && !tb.failed() {
					t.Fatalf("%s record=%v: an unreadable body must fail the test", c.name, record)
				}
			})
		}
	}
}
