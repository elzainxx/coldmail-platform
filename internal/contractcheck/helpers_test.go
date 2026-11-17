package contractcheck

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// jobShape is the Shape of a made-up module response that has every construct the comparison cares about: required and optional keys, a nullable key, integers, an array of
// objects, a map of objects.
var jobShape = modclient.Object(
	modclient.Req("job_id", modclient.Str),
	modclient.Req("state", modclient.Str),
	modclient.Req("counts", modclient.Object(modclient.Req("total", modclient.Int), modclient.Req("done", modclient.Int))),
	modclient.Opt("provider_message_id", modclient.Str),
	modclient.Req("reason", modclient.Str.OrNull()),
	modclient.Req("items", modclient.ArrayOf(modclient.Object(
		modclient.Req("id", modclient.Str),
		modclient.Req("state", modclient.Str),
		modclient.Opt("note", modclient.Str),
	))),
	modclient.Req("checks", modclient.MapOf(modclient.Object(modclient.Req("ready", modclient.Bool)))),
)

const jobBody = `{
  "job_id": "00000000-0000-4000-8000-000000000001",
  "state": "completed",
  "counts": {"total": 3, "done": 3},
  "provider_message_id": "0000000000000000",
  "reason": null,
  "items": [
    {"id": "00000000-0000-4000-8000-000000000002", "state": "ok"},
    {"id": "00000000-0000-4000-8000-000000000003", "state": "failed", "note": "mailbox full"}
  ],
  "checks": {"db": {"ready": true}, "queue": {"ready": true}},
  "created_at": "2026-01-01T00:00:00Z"
}`

func jobContract() modclient.RouteContract {
	return modclient.RouteContract{
		Module:  modclient.M2,
		Route:   "GET /v1/jobs/{id}",
		Case:    "done",
		Fixture: "job-done.json",
		Status:  200,
		Shape:   jobShape,
		Vocab: map[string][]string{
			"state":         {"queued", "running", "completed", "failed"},
			"items[].state": {"ok", "failed"},
		},
	}
}

// mutate decodes a body generically, lets f change it and encodes it again (key order is not kept; the comparison does not depend on it).
func mutate(t *testing.T, body string, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("test body does not parse: %v", err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("cannot encode the mutated body: %v", err)
	}
	return out
}

// keyed lists a report as sorted "Kind:path" strings.
func keyed(rep Report) []string {
	var out []string
	for _, d := range rep.Diffs {
		out = append(out, string(d.Kind)+":"+d.Path)
	}
	sort.Strings(out)
	return out
}

// wantDiffs asserts that the report holds exactly these "Kind:path" differences (and so asserts the Kind and the path of each, and that there is nothing else).
func wantDiffs(t *testing.T, rep Report, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := keyed(rep)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("differences:\n got  %v\n want %v\n detail:\n%s", got, want, detailOf(rep))
	}
}

func detailOf(rep Report) string {
	var b strings.Builder
	for _, d := range rep.Diffs {
		b.WriteString("  " + d.String() + "\n")
	}
	return b.String()
}

// token returns a run-time random hex string: a test never stores a secret-shaped literal and never prints one.
func token(t *testing.T, nBytes int) string {
	t.Helper()
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("no randomness: %v", err)
	}
	return hex.EncodeToString(b)
}

// ip builds an address at run time, so that no source line holds an address the personal-details gate would have to allow.
func ip(a, b, c, d int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d) }

type fakeTB struct {
	errs   []string
	logs   []string
	fatals []string
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
}

func (f *fakeTB) failed() bool { return len(f.errs)+len(f.fatals) > 0 }

func (f *fakeTB) allText() string {
	return strings.Join(f.errs, "\n") + "\n" + strings.Join(f.logs, "\n") + "\n" + strings.Join(f.fatals, "\n")
}

// gapsAt returns the gaps of a report at one path.
func gapsAt(rep Report, path string) []Gap {
	var out []Gap
	for _, g := range rep.Gaps {
		if g.Path == path {
			out = append(out, g)
		}
	}
	return out
}
