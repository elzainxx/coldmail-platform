package api

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Docs/jobs-api.md is the API's reference and a test holds it to the code, in both directions: its route table is the route table, its error table has every code with the status the mapping gives it,
// and it names the three dials. A document that says what the code does not, or leaves out what it does, fails here.

var (
	routeRow = regexp.MustCompile("(?m)^\\| `((?:GET|POST|PUT) /v1/[^`]+)` \\|")
	errorRow = regexp.MustCompile("(?m)^\\| (\\d{3}) \\| `([a-z_]+)` \\|")
)

func readJobsDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../Docs/jobs-api.md")
	if err != nil {
		t.Fatalf("cannot read Docs/jobs-api.md (a test that cannot run must fail): %v", err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// docDrift returns every disagreement between the document and the code.
func docDrift(doc string) []string {
	var out []string
	docRoutes := map[string]bool{}
	for _, m := range routeRow.FindAllStringSubmatch(doc, -1) {
		docRoutes[m[1]] = true
	}
	if len(docRoutes) == 0 {
		return []string{"the document's route table has no rows"}
	}
	for _, r := range Routes() {
		if !docRoutes[r] {
			out = append(out, "route "+r+" is not in the document's table")
		}
		delete(docRoutes, r)
	}
	for r := range docRoutes {
		out = append(out, "the document lists "+r+", which is not a route")
	}

	// every code the API can answer, with its status
	want := map[string]string{"method_not_allowed": "405"}
	for _, m := range mappingTable() {
		want[m.code] = strconv.Itoa(m.status)
	}
	for _, code := range []string{codeUnauthenticated, codeBadRequest, codePayloadTooLarge, codeUnsupportedMedia, codeValidationFailed} {
		if _, ok := want[code]; !ok {
			want[code] = map[string]string{codeUnauthenticated: "401", codeBadRequest: "400", codePayloadTooLarge: "413", codeUnsupportedMedia: "415", codeValidationFailed: "400"}[code]
		}
	}
	got := map[string]string{}
	for _, m := range errorRow.FindAllStringSubmatch(doc, -1) {
		got[m[2]] = m[1]
	}
	if len(got) == 0 {
		return append(out, "the document's error table has no rows")
	}
	for code, status := range want {
		switch g, ok := got[code]; {
		case !ok:
			out = append(out, "error code "+code+" is not in the document's table")
		case g != status:
			out = append(out, "error code "+code+": the document says "+g+", the code says "+status)
		}
	}
	for code := range got {
		if _, ok := want[code]; !ok {
			out = append(out, "the document lists the error code "+code+", which the API never answers")
		}
	}
	// every code with a message is a code of the table (the messages are the codes the API can write itself)
	for code := range messages {
		if _, ok := got[code]; !ok {
			out = append(out, "message code "+code+" has no row in the document")
		}
	}
	for _, need := range []string{"PLATFORM_JOB_MAX_ROWS", "PLATFORM_JOB_MAX_START_DELAY_MIN", "PLATFORM_RESULTS_PAGE_MAX", "Idempotency-Key", "WWW-Authenticate: Bearer", "strip one leading apostrophe", "internal/csvsafe", "64 MiB", "64 KiB", "8 KiB"} {
		if !strings.Contains(doc, need) {
			out = append(out, "the document does not mention "+need)
		}
	}
	sort.Strings(out)
	return out
}

func TestDocs_JobsAPIMatchesTheRoutesTheErrorsAndTheDials(t *testing.T) {
	doc := readJobsDoc(t)
	if drift := docDrift(doc); len(drift) != 0 {
		t.Fatalf("Docs/jobs-api.md and internal/api disagree:\n  %s", strings.Join(drift, "\n  "))
	}

	// the controls: a document corrupted in one way each is reported for exactly that way
	row := regexp.MustCompile("(?m)^\\| `GET /v1/jobs/\\{id\\}/events` \\|.*\n").FindString(doc)
	errRow := regexp.MustCompile("(?m)^\\| 409 \\| `job_slot_taken` \\|.*\n").FindString(doc)
	if row == "" || errRow == "" {
		t.Fatal("the rows the controls corrupt are not in the document")
	}
	for name, c := range map[string]struct {
		doc  string
		want string
	}{
		"a route missing":              {strings.Replace(doc, row, "", 1), "route GET /v1/jobs/{id}/events is not in the document's table"},
		"an error code missing":        {strings.Replace(doc, errRow, "", 1), "error code job_slot_taken is not in the document's table"},
		"an error with the wrong code": {strings.Replace(doc, errRow, strings.Replace(errRow, "409", "400", 1), 1), "error code job_slot_taken: the document says 400, the code says 409"},
		"a dial not named":             {strings.ReplaceAll(doc, "PLATFORM_RESULTS_PAGE_MAX", "PLATFORM_X"), "the document does not mention PLATFORM_RESULTS_PAGE_MAX"},
	} {
		drift := docDrift(c.doc)
		if !containsLine(drift, c.want) {
			t.Errorf("%s: the drift report is %v, want it to say %q", name, drift, c.want)
		}
	}
}

func containsLine(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
