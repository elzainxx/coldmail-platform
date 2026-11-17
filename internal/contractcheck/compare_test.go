package contractcheck

import (
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func compareJob(t *testing.T, live []byte) Report {
	t.Helper()
	return Compare(jobContract(), []byte(jobBody), live, 200)
}

func TestCompareOfABodyWithItselfHasNoDifferenceForEveryBodyInTheTable(t *testing.T) {
	table := []struct {
		name string
		sh   modclient.Shape
		body string
	}{
		{"job", jobShape, jobBody},
		{"zero shape object", modclient.Shape{}, `{"a":1,"b":[1,2],"c":{"d":null}}`},
		{"root array", modclient.ArrayOf(modclient.Object(modclient.Req("id", modclient.Str))), `[{"id":"a"},{"id":"b"}]`},
		{"root null", modclient.Any.OrNull(), `null`},
		{"root scalar", modclient.Str, `"x"`},
		{"empty containers", modclient.Shape{}, `{"a":[],"b":{},"c":[[]],"d":null}`},
		{"mixed array", modclient.Shape{}, `{"a":[1,"x",null,{"k":1},[2]]}`},
		{"map", modclient.MapOf(modclient.Num), `{"x":1,"y":2.5}`},
		{"empty map", modclient.MapOf(modclient.Num), `{}`},
		{"huge number", modclient.Num, strings.Repeat("9", 400)},
		{"deep but allowed", modclient.Shape{}, strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)},
	}
	for _, c := range table {
		t.Run(c.name, func(t *testing.T) {
			rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200, Shape: c.sh}
			rep := Compare(rc, []byte(c.body), []byte(c.body), 200)
			if !rep.Clean() {
				t.Fatalf("a body compared with itself differs:\n%s", detailOf(rep))
			}
		})
	}
}

func TestCompareStatusChanged(t *testing.T) {
	rep := Compare(jobContract(), []byte(jobBody), []byte(jobBody), 409)
	wantDiffs(t, rep, "StatusChanged:(status)")
	if d := rep.Diffs[0]; !strings.Contains(d.Detail, "200") || !strings.Contains(d.Detail, "409") {
		t.Fatalf("the detail must name both statuses: %s", d.Detail)
	}
	wantDiffs(t, compareJob(t, []byte(jobBody))) // the paired control: the same status is clean
}

func TestCompareKeyMissingNamesTheKeyAndTheShapeAgrees(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { delete(m["counts"].(map[string]any), "done") })
	// KeyMissing is the drift against the fixture; ShapeViolation is the adapter's own refusal of the live body: two facts, one path.
	wantDiffs(t, compareJob(t, live), "KeyMissing:counts.done", "ShapeViolation:counts.done")
}

func TestCompareAKeyTheShapeDoesNotListIsStillAMissingKeyWhenItDisappears(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { delete(m, "created_at") })
	wantDiffs(t, compareJob(t, live), "KeyMissing:created_at")
}

func TestCompareAnOptionalKeyThatIsAbsentIsNotADifferenceButARequiredOneIs(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		delete(m, "provider_message_id")
		delete(m["items"].([]any)[1].(map[string]any), "note")
	})
	wantDiffs(t, compareJob(t, live))
	// The paired control: remove a required key and it is a difference.
	live = mutate(t, jobBody, func(m map[string]any) { delete(m, "state") })
	wantDiffs(t, compareJob(t, live), "KeyMissing:state", "ShapeViolation:state")
}

func TestCompareKeyAddedIsItsOwnKindAndIsNotAShapeViolation(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		m["new_field"] = "x"
		m["counts"].(map[string]any)["failed"] = 0
		m["items"].([]any)[0].(map[string]any)["extra"] = true
	})
	wantDiffs(t, compareJob(t, live), "KeyAdded:new_field", "KeyAdded:counts.failed", "KeyAdded:items[].extra")
}

func TestCompareTypeChangedNamesThePathAndBothTypes(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { m["counts"].(map[string]any)["total"] = "three" })
	rep := compareJob(t, live)
	wantDiffs(t, rep, "TypeChanged:counts.total", "ShapeViolation:counts.total")
	if d := rep.Of(TypeChanged)[0]; !strings.Contains(d.Detail, "recorded number") || !strings.Contains(d.Detail, "live string") {
		t.Fatalf("the detail must name both types: %s", d.Detail)
	}
}

func TestCompareAnObjectThatBecameAnArrayIsATypeChangeAtThatPath(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { m["counts"] = []any{1, 2} })
	rep := compareJob(t, live)
	if got := rep.Of(TypeChanged); len(got) != 1 || got[0].Path != "counts" {
		t.Fatalf("want one TypeChanged at counts, got:\n%s", detailOf(rep))
	}
	if len(rep.Of(KeyMissing)) != 0 {
		t.Fatalf("the keys below a position whose type changed are not reported as missing:\n%s", detailOf(rep))
	}
}

func TestCompareIntegerBecameFractionalIsATypeChangeOnlyWhereTheShapeSaysInteger(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { m["counts"].(map[string]any)["total"] = 3.5 })
	wantDiffs(t, compareJob(t, live), "TypeChanged:counts.total", "ShapeViolation:counts.total")
	// The paired control: the same change under a Shape that says Num is no difference.
	rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /n", Fixture: "n.json", Status: 200, Shape: modclient.Object(modclient.Req("n", modclient.Num))}
	wantDiffs(t, Compare(rc, []byte(`{"n":3}`), []byte(`{"n":3.5}`), 200))
	// And with the Shape saying Int the very same pair is a difference.
	rc.Shape = modclient.Object(modclient.Req("n", modclient.Int))
	wantDiffs(t, Compare(rc, []byte(`{"n":3}`), []byte(`{"n":3.5}`), 200), "TypeChanged:n", "ShapeViolation:n")
}

func TestCompareNullOnOneSideSaysNothingAboutTheTypeAndIsAGap(t *testing.T) {
	// The fixture has reason null; the live body has a string: the Shape allows both, so no difference, and the fixture side is a gap.
	live := mutate(t, jobBody, func(m map[string]any) { m["reason"] = "bounced" })
	rep := compareJob(t, live)
	wantDiffs(t, rep)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Side != "fixture" || rep.Gaps[0].Path != "reason" {
		t.Fatalf("gaps = %+v, want the fixture side of reason", rep.Gaps)
	}
	// The other direction: a fixture with a string and a live null.
	fx := strings.Replace(jobBody, `"reason": null`, `"reason": "bounced"`, 1)
	rep = Compare(jobContract(), []byte(fx), []byte(jobBody), 200)
	wantDiffs(t, rep)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Side != "live" {
		t.Fatalf("gaps = %+v, want the live side", rep.Gaps)
	}
}

func TestCompareNullWhereTheShapeForbidsItIsAShapeViolationNotATypeChange(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { m["counts"].(map[string]any)["total"] = nil })
	wantDiffs(t, compareJob(t, live), "ShapeViolation:counts.total")
}

func TestCompareVocabNewNamesThePathAndTheValue(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		m["state"] = "paused"
		m["items"].([]any)[1].(map[string]any)["state"] = "deferred"
	})
	rep := compareJob(t, live)
	wantDiffs(t, rep, "VocabNew:state", "VocabNew:items[].state")
	joined := detailOf(rep)
	if !strings.Contains(joined, `"paused"`) || !strings.Contains(joined, `"deferred"`) {
		t.Fatalf("the vocabulary values must be named:\n%s", joined)
	}
	// The paired control: a value the vocabulary lists is no difference.
	live = mutate(t, jobBody, func(m map[string]any) { m["state"] = "running" })
	wantDiffs(t, compareJob(t, live))
}

func TestCompareVocabValueSeenSeveralTimesIsReportedOnce(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		for _, it := range m["items"].([]any) {
			it.(map[string]any)["state"] = "weird"
		}
	})
	rep := compareJob(t, live)
	if got := rep.Of(VocabNew); len(got) != 1 {
		t.Fatalf("want one VocabNew for one new value, got:\n%s", detailOf(rep))
	}
}

func TestCompareDoesNotPrintAVocabularyValueThatIsNotAWord(t *testing.T) {
	secret := "user@" + "corp.com"
	long := token(t, 20)
	for _, v := range []string{secret, long, "has\nnewline"} {
		live := mutate(t, jobBody, func(m map[string]any) { m["state"] = v })
		rep := compareJob(t, live)
		wantDiffs(t, rep, "VocabNew:state")
		if strings.Contains(detailOf(rep), v) || strings.Contains(rep.Diffs[0].Detail, "corp") {
			t.Fatalf("a vocabulary value that is not a plain word must not be printed: %s", rep.Diffs[0].Detail)
		}
		if !strings.Contains(rep.Diffs[0].Detail, "bytes, not printed") {
			t.Fatalf("the detail must say the value was withheld: %s", rep.Diffs[0].Detail)
		}
	}
}

func TestCompareVocabKeyThatIsNotAPathIsAContractProblemNotASilentPass(t *testing.T) {
	rc := jobContract()
	rc.Vocab = map[string][]string{"items[.state": {"ok"}}
	rep := Compare(rc, []byte(jobBody), []byte(jobBody), 200)
	wantDiffs(t, rep, "ContractInvalid:items[.state")
}

func TestCompareShapeViolationAloneNamesTheKeyWhenBothBodiesLackIt(t *testing.T) {
	// Fixture and live both lack job_id: nothing differs between them, but the adapter would refuse the live body, and the diff names the key.
	both := mutate(t, jobBody, func(m map[string]any) { delete(m, "job_id") })
	rep := Compare(jobContract(), both, both, 200)
	wantDiffs(t, rep, "ShapeViolation:job_id")
	if !strings.Contains(rep.Diffs[0].Detail, "job_id") {
		t.Fatalf("the detail must name the key: %s", rep.Diffs[0].Detail)
	}
}

func TestCompareShapeViolationPathHasNoArrayIndex(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { m["items"].([]any)[1].(map[string]any)["state"] = 7 })
	rep := compareJob(t, live)
	if got := rep.Of(ShapeViolation); len(got) != 1 || got[0].Path != "items[].state" {
		t.Fatalf("the Shape violation must be at items[].state (indexes folded), got:\n%s", detailOf(rep))
	}
}

func TestCompareAnEmptyLiveArrayAgainstAFilledFixtureIsAGapNotADifference(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) { m["items"] = []any{} })
	rep := compareJob(t, live)
	wantDiffs(t, rep)
	if g := gapsAt(rep, "items"); len(g) != 1 || g[0].Side != "live" {
		t.Fatalf("gaps = %+v", rep.Gaps)
	}
	// And an empty fixture array against a filled live one: the live elements' keys are not reported as added.
	fx := mutate(t, jobBody, func(m map[string]any) { m["items"] = []any{} })
	rep = Compare(jobContract(), fx, []byte(jobBody), 200)
	wantDiffs(t, rep)
	if g := gapsAt(rep, "items"); len(g) != 1 || g[0].Side != "fixture" {
		t.Fatalf("gaps = %+v", rep.Gaps)
	}
	// The paired control: a filled live array that lost a key of its elements is a difference.
	live = mutate(t, jobBody, func(m map[string]any) {
		for _, it := range m["items"].([]any) {
			delete(it.(map[string]any), "id")
		}
	})
	wantDiffs(t, compareJob(t, live), "KeyMissing:items[].id", "ShapeViolation:items[].id")
}

func TestCompareDifferentMapKeysAreNoDifferenceButADifferentValueShapeIs(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		m["checks"] = map[string]any{"cache": map[string]any{"ready": false}, "db": map[string]any{"ready": true}, "mailer": map[string]any{"ready": true}}
	})
	wantDiffs(t, compareJob(t, live))
	// The paired control: a value inside the map that changed type is a difference at checks{}.ready.
	live = mutate(t, jobBody, func(m map[string]any) {
		m["checks"] = map[string]any{"db": map[string]any{"ready": "yes"}}
	})
	wantDiffs(t, compareJob(t, live), "TypeChanged:checks{}.ready", "ShapeViolation:checks.db.ready")
	// And the control for the Shape-driven rule: with no MapOf in the Shape the key names are compared.
	rc := jobContract()
	rc.Shape = modclient.Shape{}
	live = mutate(t, jobBody, func(m map[string]any) {
		m["checks"] = map[string]any{"cache": map[string]any{"ready": false}, "db": map[string]any{"ready": true}}
	})
	wantDiffs(t, Compare(rc, []byte(jobBody), live, 200), "KeyMissing:checks.queue", "KeyAdded:checks.cache")
}

func TestCompareAnUnreadableLiveBodyIsOneShapeViolationAtTheBody(t *testing.T) {
	for _, live := range []string{"", "<html>not json</html>", `{"a":`, `{"a":1,"a":2}`, `{} {}`} {
		rep := compareJob(t, []byte(live))
		wantDiffs(t, rep, "ShapeViolation:(body)")
	}
}

func TestCompareAnUnreadableFixtureIsAContractProblemAndNotAComparison(t *testing.T) {
	rep := Compare(jobContract(), []byte(`{"a":`), []byte(jobBody), 200)
	wantDiffs(t, rep, "ContractInvalid:(body)")
	// The paired control: the same call with a readable fixture compares.
	wantDiffs(t, compareJob(t, []byte(jobBody)))
}

func TestCompareEveryDifferenceNamesModuleRouteCaseAndPath(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		delete(m, "state")
		m["extra"] = 1
		m["counts"].(map[string]any)["total"] = "x"
	})
	rep := Compare(jobContract(), []byte(jobBody), live, 500)
	if len(rep.Diffs) < 5 {
		t.Fatalf("expected several differences, got:\n%s", detailOf(rep))
	}
	for _, d := range rep.Diffs {
		if d.Module != modclient.M2 || d.Route != "GET /v1/jobs/{id}" || d.Case != "done" || d.Path == "" || d.Kind == "" || d.Detail == "" {
			t.Fatalf("a difference without full identity: %+v", d)
		}
		s := d.String()
		for _, part := range []string{"m2", "GET /v1/jobs/{id}", "case=done", "path=" + d.Path, "kind=" + string(d.Kind)} {
			if !strings.Contains(s, part) {
				t.Fatalf("%q lacks %q", s, part)
			}
		}
	}
}

func TestCompareReportsEveryDifferenceAndNotOnlyTheFirst(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		delete(m, "created_at")
		m["added"] = 1
		m["state"] = "paused"
		m["counts"].(map[string]any)["total"] = true
	})
	rep := compareJob(t, live)
	wantDiffs(t, rep, "KeyMissing:created_at", "KeyAdded:added", "VocabNew:state", "TypeChanged:counts.total", "ShapeViolation:counts.total")
}

func TestCompareNeverPrintsAValueOfTheBody(t *testing.T) {
	marker := "MARK" + token(t, 8)
	live := mutate(t, jobBody, func(m map[string]any) {
		m["counts"].(map[string]any)["total"] = marker + "@corp.com" // a type change: the new type is named, not the value
		m["state"] = marker + "@corp.com"                            // a vocabulary violation with a value that is not a word
		m["reason"] = 12345
		m["checks"] = map[string]any{"db": map[string]any{"ready": marker}}
		m["items"] = []any{map[string]any{"id": marker, "state": 1}}
	})
	rep := compareJob(t, live)
	if len(rep.Diffs) == 0 {
		t.Fatal("the mutations must produce differences")
	}
	if strings.Contains(detailOf(rep), marker) {
		t.Fatalf("a value of the body was printed:\n%s", detailOf(rep))
	}
}

func TestCompareOutputIsDeterministic(t *testing.T) {
	live := mutate(t, jobBody, func(m map[string]any) {
		m["a1"], m["a2"], m["a3"] = 1, 2, 3
		delete(m, "created_at")
		m["state"] = "x"
	})
	first := detailOf(compareJob(t, live))
	for i := 0; i < 20; i++ {
		if got := detailOf(compareJob(t, live)); got != first {
			t.Fatalf("run %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
}

func TestCompareRootArrayPathsStartWithTheElementMarker(t *testing.T) {
	rc := modclient.RouteContract{Module: modclient.M3, Route: "GET /v1/list", Fixture: "l.json", Status: 200, Shape: modclient.ArrayOf(modclient.Object(modclient.Req("id", modclient.Str)))}
	fx := `[{"id":"a","n":1}]`
	wantDiffs(t, Compare(rc, []byte(fx), []byte(`[{"n":1}]`), 200), "KeyMissing:[].id", "ShapeViolation:[].id")
	wantDiffs(t, Compare(rc, []byte(fx), []byte(`[{"id":"a"}]`), 200), "KeyMissing:[].n")
	wantDiffs(t, Compare(rc, []byte(fx), []byte(`{"id":"a"}`), 200), "TypeChanged:(body)", "ShapeViolation:(body)")
}

func TestCompareANullAmongOtherValuesOfAnArrayIsNotATypeChangeButAnotherTypeIs(t *testing.T) {
	rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200}
	withNull := `{"items":[{"note":null},{"note":"x"}]}`
	withoutNull := `{"items":[{"note":"a"},{"note":"b"}]}`
	wantDiffs(t, Compare(rc, []byte(withNull), []byte(withoutNull), 200))
	wantDiffs(t, Compare(rc, []byte(withoutNull), []byte(withNull), 200))
	// The paired control: a number where the fixture had strings and null is a type change.
	wantDiffs(t, Compare(rc, []byte(withNull), []byte(`{"items":[{"note":1}]}`), 200), "TypeChanged:items[].note")
}
