package contractcheck

import (
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/coldmail-platform/platform/internal/modclient"
)

const fxDir = "testdata/contract"

func fixtureFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[fxDir+"/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

func twoContracts() []modclient.RouteContract {
	a := jobContract()
	b := modclient.RouteContract{Module: modclient.M2, Route: "GET /healthz", Fixture: "health.json", Status: 200, Shape: modclient.Object(modclient.Req("status", modclient.Str))}
	return []modclient.RouteContract{a, b}
}

func cleanFiles() map[string]string {
	return map[string]string{"job-done.json": jobBody, "health.json": `{"status":"ok"}`}
}

// kinds lists the problems as sorted "Kind:fixture" strings.
func kinds(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, string(p.Kind)+":"+p.Fixture)
	}
	sort.Strings(out)
	return out
}

func wantProblems(t *testing.T, got []Problem, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(kinds(got), "\n") != strings.Join(want, "\n") {
		var b strings.Builder
		for _, p := range got {
			b.WriteString("  " + p.String() + "\n")
		}
		t.Fatalf("problems:\n got  %v\n want %v\n detail:\n%s", kinds(got), want, b.String())
	}
}

func TestCheckFixturesAcceptsAConsistentSet(t *testing.T) {
	wantProblems(t, CheckFixtures(twoContracts(), fixtureFS(cleanFiles()), fxDir))
}

func TestCheckFixturesFlagsADanglingNameWhileTheCleanSetPasses(t *testing.T) {
	files := cleanFiles()
	delete(files, "health.json")
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	wantProblems(t, got, "Dangling:health.json")
	if !strings.Contains(got[0].Route, "/healthz") {
		t.Fatalf("the problem must name the contract: %s", got[0])
	}
}

func TestCheckFixturesFlagsAStrayFileThatNoContractNames(t *testing.T) {
	files := cleanFiles()
	files["old-route.json"] = `{"a":1}`
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	wantProblems(t, got, "Stray:old-route.json")
	if !strings.Contains(got[0].Detail, "no longer uses") {
		t.Fatalf("the detail must say why: %s", got[0].Detail)
	}
}

func TestCheckFixturesFlagsASubdirectoryAsStray(t *testing.T) {
	fsys := fixtureFS(cleanFiles())
	fsys[fxDir+"/nested/x.json"] = &fstest.MapFile{Data: []byte(`{}`)}
	wantProblems(t, CheckFixtures(twoContracts(), fsys, fxDir), "Stray:nested")
}

func TestCheckFixturesFlagsOneFileNamedByTwoContracts(t *testing.T) {
	cs := twoContracts()
	cs[1].Fixture = cs[0].Fixture
	cs[1].Shape = jobShape
	wantProblems(t, CheckFixtures(cs, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir), "DuplicateFixture:job-done.json")
}

func TestCheckFixturesFlagsTwoContractsForOneRouteAndCase(t *testing.T) {
	cs := twoContracts()
	dup := cs[0]
	dup.Fixture = "job-done-2.json"
	files := cleanFiles()
	files["job-done-2.json"] = jobBody
	wantProblems(t, CheckFixtures(append(cs, dup), fixtureFS(files), fxDir), "DuplicateContract:job-done-2.json")
}

func TestCheckFixturesFlagsAFixtureThatIsNotOneJSONDocument(t *testing.T) {
	for _, bad := range []string{"", `{"a":`, `{} {}`, `{"a":1,"a":2}`, "<html>"} {
		files := cleanFiles()
		files["health.json"] = bad
		got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
		wantProblems(t, got, "NotJSON:health.json")
	}
}

func TestCheckFixturesFlagsAFixtureItsOwnShapeRefusesAndNamesTheKey(t *testing.T) {
	files := cleanFiles()
	files["job-done.json"] = string(mutate(t, jobBody, func(m map[string]any) { delete(m["counts"].(map[string]any), "total") }))
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	wantProblems(t, got, "ShapeRefused:job-done.json")
	if !strings.Contains(got[0].Detail, "counts.total") {
		t.Fatalf("the problem must name the key: %s", got[0].Detail)
	}
}

func TestCheckFixturesFlagsASecretShapedFixtureWithoutPrintingIt(t *testing.T) {
	tok := token(t, 24)
	files := cleanFiles()
	files["job-done.json"] = string(mutate(t, jobBody, func(m map[string]any) { m["items"].([]any)[1].(map[string]any)["note"] = "Bearer " + tok }))
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	wantProblems(t, got, "Secret:job-done.json", "Secret:job-done.json") // the same text is a bearer token and a long opaque run
	for _, p := range got {
		if strings.Contains(p.String(), tok) {
			t.Fatalf("the problem prints the secret: %s", p)
		}
	}
	if !strings.Contains(got[0].Detail, "items[].note") {
		t.Fatalf("the problem must name the path: %s", got[0].Detail)
	}
}

func TestCheckFixturesFlagsAnAddressAndAnEmailOutsideTheDocumentationRanges(t *testing.T) {
	files := cleanFiles()
	files["health.json"] = `{"status":"ok","from":"` + "me@" + `corp.com","host":"` + ip(8, 8, 4, 4) + `"}`
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	wantProblems(t, got, "Secret:health.json", "Secret:health.json")
	files["health.json"] = `{"status":"ok","from":"me@example.com","host":"192.0.2.1"}`
	wantProblems(t, CheckFixtures(twoContracts(), fixtureFS(files), fxDir))
}

func TestCheckFixturesFlagsAFixtureValueOutsideItsOwnVocab(t *testing.T) {
	files := cleanFiles()
	files["job-done.json"] = string(mutate(t, jobBody, func(m map[string]any) { m["items"].([]any)[0].(map[string]any)["state"] = "mystery" }))
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	wantProblems(t, got, "VocabOutside:job-done.json")
	if !strings.Contains(got[0].Detail, `"mystery"`) || !strings.Contains(got[0].Detail, "items[].state") {
		t.Fatalf("the problem must name the value and the vocabulary: %s", got[0].Detail)
	}
}

func TestCheckFixturesFlagsAVocabKeyThatIsATypoOrNotAPath(t *testing.T) {
	cs := twoContracts()
	cs[0].Vocab = map[string][]string{"items[].stat": {"ok"}}
	wantProblems(t, CheckFixtures(cs, fixtureFS(cleanFiles()), fxDir), "VocabInert:job-done.json") // a path of the grammar that matches nothing in the fixture
	cs[0].Vocab = map[string][]string{"items[0].state": {"ok"}}
	wantProblems(t, CheckFixtures(cs, fixtureFS(cleanFiles()), fxDir), "VocabKey:job-done.json")
	cs[0].Vocab = map[string][]string{"": {"ok"}}
	wantProblems(t, CheckFixtures(cs, fixtureFS(cleanFiles()), fxDir), "VocabKey:job-done.json")
	// The paired control: the corrected key is clean.
	cs[0].Vocab = map[string][]string{"items[].state": {"ok", "failed"}}
	wantProblems(t, CheckFixtures(cs, fixtureFS(cleanFiles()), fxDir))
}

func TestCheckFixturesAcceptsAVocabPathBeneathAnEmptyArrayInTheFixture(t *testing.T) {
	files := cleanFiles()
	files["job-done.json"] = string(mutate(t, jobBody, func(m map[string]any) { m["items"] = []any{} }))
	// items[].state exists in the contract but the fixture is an empty page: not a typo, not judged.
	wantProblems(t, CheckFixtures(twoContracts(), fixtureFS(files), fxDir))
}

func TestCheckFixturesFlagsAnEmptyOrRepeatingVocabulary(t *testing.T) {
	cs := twoContracts()
	cs[0].Vocab = map[string][]string{"state": {}}
	wantProblems(t, CheckFixtures(cs, fixtureFS(cleanFiles()), fxDir), "VocabList:job-done.json", "VocabOutside:job-done.json")
	cs[0].Vocab = map[string][]string{"state": {"completed", "completed"}}
	wantProblems(t, CheckFixtures(cs, fixtureFS(cleanFiles()), fxDir), "VocabList:job-done.json")
}

func TestCheckFixturesWithNoContractsIsRedBecauseTheSubjectIsEmpty(t *testing.T) {
	got := CheckFixtures(nil, fixtureFS(cleanFiles()), fxDir)
	if len(got) != 1 || got[0].Kind != NoContracts || !strings.Contains(got[0].Detail, "found 0 contracts") {
		t.Fatalf("want one NoContracts problem, got %v", got)
	}
}

func TestCheckFixturesRefusesUnusableContracts(t *testing.T) {
	good := twoContracts()[1]
	zero := good
	zero.Shape = modclient.Shape{}
	badStatus := good
	badStatus.Status = 0
	traverse := good
	traverse.Fixture = "../secrets.json"
	subdir := good
	subdir.Fixture = "a/b.json"
	noRoute := good
	noRoute.Route = ""
	for name, rc := range map[string]modclient.RouteContract{"zero shape": zero, "status": badStatus, "traversal": traverse, "subdirectory": subdir, "no route": noRoute} {
		got := CheckFixtures([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"health.json": `{"status":"ok"}`}), fxDir)
		found := false
		for _, p := range got {
			found = found || p.Kind == BadContract
		}
		if !found {
			t.Fatalf("%s: want a BadContract problem, got %v", name, kinds(got))
		}
	}
	// The paired control: the good contract alone is clean.
	wantProblems(t, CheckFixtures([]modclient.RouteContract{good}, fixtureFS(map[string]string{"health.json": `{"status":"ok"}`}), fxDir))
}

func TestCheckFixturesWithAMissingDirectoryReportsEveryContractDangling(t *testing.T) {
	got := CheckFixtures(twoContracts(), fstest.MapFS{}, fxDir)
	wantProblems(t, got, "Dangling:job-done.json", "Dangling:health.json", "Unreadable:")
}

func TestCheckFixturesNeverCarriesAValueOfAFixtureExceptVocabularyWords(t *testing.T) {
	marker := "MARK" + token(t, 8)
	files := cleanFiles()
	files["job-done.json"] = string(mutate(t, jobBody, func(m map[string]any) {
		m["counts"] = map[string]any{"total": marker, "done": 1}
		m["state"] = marker + "@corp.com"
	}))
	got := CheckFixtures(twoContracts(), fixtureFS(files), fxDir)
	if len(got) == 0 {
		t.Fatal("the mutations must produce problems")
	}
	for _, p := range got {
		if strings.Contains(p.String(), marker) {
			t.Fatalf("a problem carries a value of the fixture: %s", p)
		}
	}
}
