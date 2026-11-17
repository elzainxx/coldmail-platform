package contractcheck

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

const (
	liveJob = `{"job_id":"3f2b8a10-5c1d-4e7f-9a0b-1c2d3e4f5a6b","state":"completed","counts":{"total":3,"done":3},"provider_message_id":"abcdef0123456789abcdef","reason":null,` +
		`"items":[{"id":"11111111-2222-4333-8444-555555555555","state":"ok"},{"id":"66666666-7777-4888-8999-aaaaaaaaaaaa","state":"failed","note":"mailbox full"}],` +
		`"checks":{"db":{"ready":true},"queue":{"ready":true}},"created_at":"2026-11-17T21:32:01.0030894Z"}`
	beginJob = "==== CONTRACT-FIXTURE BEGIN module=m2 route=GET /v1/jobs/{id} case=done file=job-done.json status=200 ===="
)

func recordBlock(t *testing.T, rc modclient.RouteContract, status int, body string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Record(&buf, rc, status, []byte(body)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return buf.String()
}

// withPrefix puts a log prefix in front of every line of a block.
func withPrefix(block, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestRecordWritesExactlyTheDelimitedBlock(t *testing.T) {
	got := recordBlock(t, jobContract(), 200, `{"id":"3f2b8a10-5c1d-4e7f-9a0b-1c2d3e4f5a6b","n":1}`)
	want := beginJob + "\n{\n  \"id\": \"00000000-0000-4000-8000-000000000001\",\n  \"n\": 1\n}\n==== CONTRACT-FIXTURE END ====\n"
	if got != want {
		t.Fatalf("block:\n%q\nwant:\n%q", got, want)
	}
}

func TestRecordRefusesASecretShapedBodyAndPrintsNothing(t *testing.T) {
	tok := token(t, 24)
	var buf bytes.Buffer
	err := Record(&buf, jobContract(), 200, []byte(`{"note":"Bearer `+tok+`"}`))
	if !errors.Is(err, ErrSecretShaped) {
		t.Fatalf("want ErrSecretShaped, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a refused record must print nothing, printed %q", buf.String())
	}
	if strings.Contains(err.Error(), tok) || !strings.Contains(err.Error(), "note") {
		t.Fatalf("the error must name the path and not the value: %v", err)
	}
	// The paired control: the same body without the token is recorded.
	recordBlock(t, jobContract(), 200, `{"note":"fine"}`)
}

func TestRecordCredentialShapedIdsAreRecordedBecauseNormalisationReplacesThem(t *testing.T) {
	long := token(t, 24)
	block := recordBlock(t, jobContract(), 200, `{"request_id":"`+long+`"}`)
	if strings.Contains(block, long) {
		t.Fatal("the id survived into the block")
	}
}

func TestRecordRefusesAnUnusableHeaderAndPrintsNothing(t *testing.T) {
	mk := func(f func(rc *modclient.RouteContract)) modclient.RouteContract {
		rc := jobContract()
		f(&rc)
		return rc
	}
	cases := map[string]modclient.RouteContract{
		"route holds the case marker": mk(func(rc *modclient.RouteContract) { rc.Route = "GET /x case=y" }),
		"route has a newline":         mk(func(rc *modclient.RouteContract) { rc.Route = "GET /x\nGET /y" }),
		"empty route":                 mk(func(rc *modclient.RouteContract) { rc.Route = "" }),
		"case holds the file marker":  mk(func(rc *modclient.RouteContract) { rc.Case = "a file=b" }),
		"case has a control char":     mk(func(rc *modclient.RouteContract) { rc.Case = "a\tb" }),
		"fixture is a path":           mk(func(rc *modclient.RouteContract) { rc.Fixture = "sub/x.json" }),
		"fixture climbs":              mk(func(rc *modclient.RouteContract) { rc.Fixture = "a..b.json" }),
		"fixture has a space":         mk(func(rc *modclient.RouteContract) { rc.Fixture = "a b.json" }),
		"empty module":                mk(func(rc *modclient.RouteContract) { rc.Module = "" }),
	}
	for name, rc := range cases {
		var buf bytes.Buffer
		if err := Record(&buf, rc, 200, []byte(`{}`)); !errors.Is(err, ErrBadHeader) || buf.Len() != 0 {
			t.Fatalf("%s: want ErrBadHeader and no output, got %v %q", name, err, buf.String())
		}
	}
	for _, status := range []int{0, 99, 600} {
		var buf bytes.Buffer
		if err := Record(&buf, jobContract(), status, []byte(`{}`)); !errors.Is(err, ErrBadHeader) || buf.Len() != 0 {
			t.Fatalf("status %d: want ErrBadHeader and no output, got %v", status, err)
		}
	}
	recordBlock(t, jobContract(), 200, `{}`) // the paired control
}

func TestRecordRefusesAnUnreadableBodyWithTheParserKindAndPrintsNothing(t *testing.T) {
	for body, want := range map[string]ParseKind{"": ParseEmpty, "<html>": ParseSyntax, `{"a":1,"a":2}`: ParseDuplicateKey} {
		var buf bytes.Buffer
		err := Record(&buf, jobContract(), 200, []byte(body))
		if ParseKindOf(err) != want || buf.Len() != 0 {
			t.Fatalf("%q: want %v and no output, got %v %q", body, want, err, buf.String())
		}
	}
}

func TestParseRecordedReadsTheBlocksAcrossEveryLogPrefix(t *testing.T) {
	block := recordBlock(t, jobContract(), 200, liveJob)
	prefixes := map[string]string{
		"bare":                            "",
		"timestamp":                       "2026-11-17T21:32:01Z ",
		"timestamp with ticks":            "2026-11-17T21:32:01.0030894Z ",
		"job and step then timestamp":     "harness\tThe harness\t2026-11-17T21:32:01.0030894Z ",
		"go test decoration":              "    assert.go:104: ",
		"timestamp then go test":          "2026-11-17T21:32:01.0030894Z     assert.go:104: ",
		"job, step, timestamp, go test":   "harness\tThe harness\t2026-11-17T21:32:01.0030894Z     assert.go:104: ",
		"go test decoration in a subpath": "    internal/x/assert.go:7: ",
	}
	want, _ := Normalise([]byte(liveJob))
	for name, prefix := range prefixes {
		t.Run(name, func(t *testing.T) {
			recs, err := ParseRecorded([]byte(withPrefix(block, prefix)))
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 {
				t.Fatalf("blocks = %d, want 1", len(recs))
			}
			r := recs[0]
			if r.Module != modclient.M2 || r.Route != "GET /v1/jobs/{id}" || r.Case != "done" || r.File != "job-done.json" || r.Status != 200 {
				t.Fatalf("header read wrongly: %+v", r)
			}
			if !bytes.Equal(r.Body, want) {
				t.Fatalf("body differs from what Normalise gave:\n%s\nwant:\n%s", r.Body, want)
			}
		})
	}
}

func TestParseRecordedDoesNotMistakeATimestampInsideABodyLineForAPrefix(t *testing.T) {
	// The normalised body has lines that start with white space or a quote, and strings that hold a timestamp followed by a space.
	body := `{"note":"2026-11-17T21:32:01Z later","at":"2026-11-17T21:32:01Z"}`
	block := recordBlock(t, jobContract(), 200, body)
	recs, err := ParseRecorded([]byte(withPrefix(block, "harness\tThe harness\t2026-11-17T21:32:01.0030894Z ")))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Normalise([]byte(body))
	if !bytes.Equal(recs[0].Body, want) || !strings.Contains(string(recs[0].Body), "2026-01-01T00:00:00Z later") {
		t.Fatalf("the body was damaged:\n%s", recs[0].Body)
	}
}

func TestRecordThenParseThenCompareIsTheIdentity(t *testing.T) {
	bodies := []string{liveJob, `{"a":[]}`, `{"a":null,"b":{"c":[1,2.5]}}`, `[{"id":"x"}]`, `{"checks":{"db":{"ready":true}}}`}
	for _, body := range bodies {
		rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Case: "empty page", Fixture: "x.json", Status: 200}
		recs, err := ParseRecorded([]byte(withPrefix(recordBlock(t, rc, 200, body), "2026-11-17T21:32:01Z ")))
		if err != nil {
			t.Fatal(err)
		}
		if recs[0].Case != "empty page" {
			t.Fatalf("a case with a space did not round-trip: %q", recs[0].Case)
		}
		rep := Compare(rc, recs[0].Body, []byte(body), 200)
		if !rep.Clean() {
			t.Fatalf("record, parse, compare is not the identity for %s:\n%s", body, detailOf(rep))
		}
	}
	// The paired control: a recording of a different body is not the identity.
	rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200}
	recs, _ := ParseRecorded([]byte(recordBlock(t, rc, 200, `{"a":1}`)))
	if rep := Compare(rc, recs[0].Body, []byte(`{"a":1,"b":2}`), 200); rep.Clean() {
		t.Fatal("the control must see the added key")
	}
}

func TestParseRecordedKeepsAnEmptyCase(t *testing.T) {
	rc := jobContract()
	rc.Case = ""
	recs, err := ParseRecorded([]byte(recordBlock(t, rc, 200, `{"a":1}`)))
	if err != nil || recs[0].Case != "" || recs[0].Route != "GET /v1/jobs/{id}" {
		t.Fatalf("empty case: %+v %v", recs, err)
	}
}

func TestParseRecordedRefusesEachMalformedLogWithItsOwnError(t *testing.T) {
	good := recordBlock(t, jobContract(), 200, `{"a":1}`)
	tok := token(t, 24)
	badBody := strings.Replace(good, "{\n  \"a\": 1\n}", `{ "note": "Bearer `+tok+`" }`, 1)
	cases := []struct {
		name string
		log  string
		is   error
		kind ParseKind
	}{
		{"empty log", "", ErrNoBlocks, 0},
		{"no block in a long log", "noise\nmore noise\n", ErrNoBlocks, 0},
		{"never closed", strings.Replace(good, endMarker+"\n", "", 1), ErrMalformedLog, 0},
		{"nested begin", beginJob + "\n" + good, ErrMalformedLog, 0},
		{"end without begin", endMarker + "\n", ErrMalformedLog, 0},
		{"unreadable header", strings.Replace(good, "status=200", "status=abc", 1), ErrMalformedLog, 0},
		{"file climbs", strings.Replace(good, "file=job-done.json", "file=../x.json", 1), ErrMalformedLog, 0},
		{"status out of range", strings.Replace(good, "status=200", "status=999", 1), ErrMalformedLog, 0},
		{"body is not json", strings.Replace(good, "{\n  \"a\": 1\n}", "not json", 1), nil, ParseSyntax},
		{"body is empty", beginJob + "\n" + endMarker + "\n", nil, ParseEmpty},
		{"body is secret shaped", badBody, ErrSecretShaped, 0},
		{"same file twice with different bodies", good + strings.Replace(good, `"a": 1`, `"a": 2`, 1), ErrConflict, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs, err := ParseRecorded([]byte(c.log))
			if recs != nil || err == nil {
				t.Fatalf("must be refused with no blocks, got %v %v", recs, err)
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("want %v, got %v", c.is, err)
			}
			if c.kind != 0 && ParseKindOf(err) != c.kind {
				t.Fatalf("want parse kind %v, got %v", c.kind, err)
			}
			if strings.Contains(err.Error(), tok) {
				t.Fatal("the error carries the secret")
			}
		})
	}
	// The paired control: the unmodified block, and an identical duplicate of it, are read (the duplicate is one block).
	recs, err := ParseRecorded([]byte(good + good))
	if err != nil || len(recs) != 1 {
		t.Fatalf("an identical duplicate is one block: %v %v", recs, err)
	}
}

func TestParseRecordedReturnsBlocksInOrderAndIgnoresNoiseBetweenThem(t *testing.T) {
	a := jobContract()
	b := modclient.RouteContract{Module: modclient.M3, Route: "GET /healthz", Fixture: "health.json", Status: 200}
	log := "setup line\n" + recordBlock(t, a, 200, `{"a":1}`) + "=== RUN   TestSomething\n--- PASS\n" + recordBlock(t, b, 200, `{"status":"ok"}`) + "tail\n"
	recs, err := ParseRecorded([]byte(log))
	if err != nil || len(recs) != 2 || recs[0].File != "job-done.json" || recs[1].File != "health.json" || recs[1].Module != modclient.M3 {
		t.Fatalf("blocks = %+v %v", recs, err)
	}
}

func TestParseRecordedHandlesCRLFBlankLinesAndAByteOrderMark(t *testing.T) {
	block := recordBlock(t, jobContract(), 200, `{"a":1,"b":[1]}`)
	want, _ := ParseRecorded([]byte(block))
	crlf := strings.ReplaceAll(block, "\n", "\r\n")
	blank := strings.ReplaceAll(block, "\n", "\n\n")
	for name, log := range map[string]string{"crlf": crlf, "blank lines": blank, "bom": "\xef\xbb\xbf" + block} {
		got, err := ParseRecorded([]byte(log))
		if err != nil || len(got) != 1 || !bytes.Equal(got[0].Body, want[0].Body) {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
}

func TestParseRecordedRefusesAHugeLog(t *testing.T) {
	_, err := ParseRecorded(make([]byte, MaxLog+1))
	if !errors.Is(err, ErrMalformedLog) {
		t.Fatalf("want ErrMalformedLog, got %v", err)
	}
}

func TestWriteRecordedWritesReportsAndLeavesIdenticalFilesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out", "contract")
	recs, _ := ParseRecorded([]byte(recordBlock(t, jobContract(), 200, `{"a":1}`)))
	var out bytes.Buffer
	if err := WriteRecorded(dir, recs, false, &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "job-done.json"))
	if err != nil || !bytes.Equal(got, recs[0].Body) {
		t.Fatalf("file content: %q %v", got, err)
	}
	if !strings.Contains(out.String(), "written m2 GET /v1/jobs/{id} case=done -> job-done.json") {
		t.Fatalf("the report must name module and route: %q", out.String())
	}
	out.Reset()
	if err := WriteRecorded(dir, recs, false, &out); err != nil || !strings.HasPrefix(out.String(), "unchanged ") {
		t.Fatalf("a second identical write is unchanged: %v %q", err, out.String())
	}
}

func TestWriteRecordedRefusesToOverwriteADifferentFileUnlessForcedAndWritesNothingElse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "job-done.json"), []byte(`{"old":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := modclient.RouteContract{Module: modclient.M3, Route: "GET /healthz", Fixture: "health.json", Status: 200}
	recs, _ := ParseRecorded([]byte(recordBlock(t, jobContract(), 200, `{"a":1}`) + recordBlock(t, b, 200, `{"status":"ok"}`)))
	var out bytes.Buffer
	err := WriteRecorded(dir, recs, false, &out)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "health.json")); !os.IsNotExist(err) {
		t.Fatal("nothing may be written when any file would be refused")
	}
	if old, _ := os.ReadFile(filepath.Join(dir, "job-done.json")); !strings.Contains(string(old), "old") {
		t.Fatal("the existing file was touched")
	}
	if err := WriteRecorded(dir, recs, true, &out); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "job-done.json")); !bytes.Equal(now, recs[0].Body) {
		t.Fatal("force must overwrite")
	}
	if !strings.Contains(out.String(), "overwritten m2") {
		t.Fatalf("the report must say overwritten: %q", out.String())
	}
}

func TestWriteRecordedRefusesAnUnusableFileNameAndAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../x.json", "a/b.json", "", "..", "a b.json"} {
		err := WriteRecorded(dir, []Recorded{{File: name, Body: []byte("{}\n")}}, true, &bytes.Buffer{})
		if !errors.Is(err, ErrBadHeader) {
			t.Fatalf("%q: want ErrBadHeader, got %v", name, err)
		}
	}
	if err := WriteRecorded(dir, nil, false, &bytes.Buffer{}); !errors.Is(err, ErrNoBlocks) {
		t.Fatalf("an empty list is red: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("nothing may be written for refused names")
	}
}
