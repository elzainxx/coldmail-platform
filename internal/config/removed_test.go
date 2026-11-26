package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The removed variables (removed.go): what happens to one that is still set, and the gate that keeps a removed variable removed. Phase 4 C3 removed the first one (the platform's own switch for
// module 1's vendor fallback: module 1 now says it itself); the tests below name no variable by its text, they take the names from the list.

func TestRemovedVariables_LoadNeverReadsThemAndTheyAreReportedByNameNeverByValue(t *testing.T) {
	removed := RemovedVars()
	if len(removed) == 0 {
		t.Fatal("the list of removed variables is empty: the instrument below would prove nothing")
	}
	const sentinel = "zzzz-removed-value-zzzz"
	for _, r := range removed {
		// a removed variable is never in the list of variables Load reads, and Load never asks for it
		for _, v := range Vars() {
			if v.Name == r.Name {
				t.Errorf("%s is removed and still declared in Vars()", r.Name)
			}
		}
		asked := map[string]bool{}
		if _, err := Load(func(k string) string { asked[k] = true; return base()[k] }); err != nil {
			t.Fatal(err)
		}
		if asked[r.Name] {
			t.Errorf("Load reads the removed variable %s", r.Name)
		}

		// set to anything (a value an earlier version refused included), it changes nothing and fails nothing
		clean, err := Load(envOf(base()))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"true", "false", "maybe-XYZ123", sentinel, "   "} {
			got, err := Load(envOf(with(base(), r.Name, v)))
			if err != nil {
				t.Errorf("%s=%q made Load fail: %v (a removed variable is ignored, whatever it holds)", r.Name, v, err)
			}
			if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", clean) {
				t.Errorf("%s=%q changed the Config", r.Name, v)
			}
		}
		// the control: a variable that IS read still fails on a bad value, so the loop above could have failed
		if _, err := Load(envOf(with(base(), VarJobTickMS, "maybe-XYZ123"))); err == nil {
			t.Error("control failed: a bad value of a live variable did not fail Load")
		}

		// SetRemoved names it exactly when it is set to something non-blank, and never returns the value
		got := SetRemoved(envOf(with(base(), r.Name, sentinel)))
		if len(got) != 1 || got[0].Name != r.Name || got[0].Instead == "" {
			t.Fatalf("SetRemoved with %s set returned %+v, want that variable and what replaced it", r.Name, got)
		}
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", []RemovedVar{r}) || strings.Contains(fmt.Sprintf("%+v", got), sentinel) {
			t.Errorf("SetRemoved returned %+v: it must be the registry entry, with no value in it", got)
		}
		for _, unset := range []string{"", "  ", "\t\n"} {
			if got := SetRemoved(envOf(with(base(), r.Name, unset))); len(got) != 0 {
				t.Errorf("%s=%q (blank) was reported as set: %+v", r.Name, unset, got)
			}
		}
	}
	if got := SetRemoved(envOf(base())); len(got) != 0 {
		t.Errorf("an environment with no removed variable reported %+v", got)
	}
	if got := SetRemoved(nil); got != nil {
		t.Errorf("a nil getenv reported %+v, want nothing", got)
	}
	// RemovedVars hands out a copy: a caller cannot empty the registry
	cp := RemovedVars()
	cp[0].Name = "changed"
	if RemovedVars()[0].Name == "changed" {
		t.Error("RemovedVars returns the registry itself")
	}
}

func TestRemovedVariables_TheNamesAreWellFormedAndDistinctFromEveryLiveVariable(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range RemovedVars() {
		if !strings.HasPrefix(r.Name, "PLATFORM_") || strings.ToUpper(r.Name) != r.Name || strings.ContainsAny(r.Name, " =\"'") {
			t.Errorf("%q is not a PLATFORM_ variable name in upper case", r.Name)
		}
		if seen[r.Name] {
			t.Errorf("%s is listed twice", r.Name)
		}
		seen[r.Name] = true
		if len(r.Instead) < 20 {
			t.Errorf("%s: what replaced it is %q: the warning would tell an operator nothing", r.Name, r.Instead)
		}
	}
}

// ---- the configuration reference lists them -------------------------------------------------------------------------------------------------------------------------------------------

// removedHeader is the heading of the section of Docs/config-reference.md that lists the removed variables, one bullet per variable: "- `NAME` ...".
const removedHeader = "## Removed variables"

// removedListed returns the names the document's "Removed variables" section lists (bullets that start with a backticked name), and an error when the section is missing.
func removedListed(md string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == removedHeader {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("the heading %q was not found", removedHeader)
	}
	var out []string
	for _, l := range lines[start+1:] {
		if strings.HasPrefix(l, "#") {
			break
		}
		if strings.HasPrefix(l, "- `") {
			rest := strings.TrimPrefix(l, "- `")
			if end := strings.Index(rest, "`"); end > 0 {
				out = append(out, rest[:end])
			}
		}
	}
	return out, nil
}

func TestRemovedVariables_TheConfigurationReferenceListsExactlyTheRemovedOnes(t *testing.T) {
	listed, err := removedListed(readReference(t))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, r := range RemovedVars() {
		want = append(want, r.Name)
	}
	sort.Strings(listed)
	sort.Strings(want)
	if strings.Join(listed, ",") != strings.Join(want, ",") || len(want) == 0 {
		t.Errorf("Docs/config-reference.md lists the removed variables %v, the code lists %v", listed, want)
	}
	// the controls: a document without the section, and one that lists another name, are noticed
	if _, err := removedListed("# a document\n\n## Something else\n"); err == nil {
		t.Error("control failed: a document with no Removed variables section was accepted")
	}
	if got, _ := removedListed(removedHeader + "\n\n- `PLATFORM_SOMETHING_ELSE` x\n"); strings.Join(got, ",") == strings.Join(want, ",") {
		t.Error("control failed: a document that lists another variable reads as listing the removed ones")
	}
}

// ---- the gate: a removed variable is named in no other tracked file ----------------------------------------------------------------------------------------------------------------------

// removedAllow is a tracked file that may name a removed variable, and why. required: the file MUST name one (an allowance for a file that does not is a stale entry and is red); a file that is
// not required may or may not (the phase records, which the lead amends after the build).
type removedAllow struct {
	path     string
	required bool
	why      string
}

var removedAllowed = []removedAllow{
	{"internal/config/removed.go", true, "the list of removed variables itself: the one place the code names one, so that a start-up warning can"},
	{"Docs/config-reference.md", true, "its \"Removed variables\" section says what an operator who still has one set can expect"},
	{"db/migrations/platform_0013_reachable_units.up.sql", true, "an applied migration is history: its comments describe the design of its day and are never edited"},
	{"Docs/phase-4-spec.md", false, "the Locked decision 23, open decision 7 and the row C3 as ruled: the record of the decision that removed the variable"},
	{"Docs/coldmail-platform-build-plan.md", false, "the build plan's historical text"},
}

// historyPrefix is the directory of preserved historical documents; nothing in it is judged.
const historyPrefix = "Docs/history/"

// removedNameProblems judges the files that name a removed variable (path -> the names found) against the allowances: a naming file that is not allowed is a problem, and so is a required
// allowance that names nothing.
func removedNameProblems(named map[string][]string, allow []removedAllow) []string {
	var out []string
	allowed := map[string]removedAllow{}
	for _, a := range allow {
		allowed[a.path] = a
	}
	var paths []string
	for p := range named {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if _, ok := allowed[p]; !ok {
			out = append(out, fmt.Sprintf("%s names the removed variable(s) %s and is not one of the files that may", p, strings.Join(named[p], ", ")))
		}
	}
	for _, a := range allow {
		if a.required && len(named[a.path]) == 0 {
			out = append(out, fmt.Sprintf("the allowance for %s (%s) names nothing: it is stale or the file is not what it was said to be", a.path, a.why))
		}
	}
	return out
}

// scanForNames reads each file of list (relative to root, slash-separated) and returns, per file that names any of the names, the names found. A file under historyPrefix is not judged. A file
// that cannot be read is an error, except one that is tracked and missing from the work tree (a deletion not yet committed).
func scanForNames(root string, list []string, names []string) (map[string][]string, int, error) {
	found := map[string][]string{}
	scanned := 0
	for _, rel := range list {
		if strings.HasPrefix(rel, historyPrefix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("cannot read %s: %w", rel, err)
		}
		scanned++
		for _, n := range names {
			if bytes.Contains(b, []byte(n)) {
				found[rel] = append(found[rel], n)
			}
		}
	}
	return found, scanned, nil
}

// trackedFiles lists the files git tracks under root AND the files that are not tracked yet but not ignored either (a new file the next commit will add: the gate must judge it before it is
// committed, not after). A tree with no git (or a git that fails) is an error: the gate FAILS rather than skip.
func trackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w (%s)", root, err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, f := range strings.Split(out.String(), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// minTrackedFiles is the floor of the gate's subject: a repository this size holds well over this many tracked files, so a listing of fewer is an instrument that matched nothing.
const minTrackedFiles = 300

func TestRemovedVariables_NoTrackedFileNamesOneOutsideItsRecords(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := trackedFiles(root)
	if err != nil {
		t.Fatalf("the gate cannot list the tracked files (a gate that cannot run fails): %v", err)
	}
	if len(files) < minTrackedFiles {
		t.Fatalf("git lists %d tracked files under %s, want at least %d: the instrument matched nothing", len(files), root, minTrackedFiles)
	}
	var names []string
	for _, r := range RemovedVars() {
		names = append(names, r.Name)
	}
	named, scanned, err := scanForNames(root, files, names)
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minTrackedFiles {
		t.Fatalf("the gate read %d files, want at least %d", scanned, minTrackedFiles)
	}
	if problems := removedNameProblems(named, removedAllowed); len(problems) != 0 {
		t.Fatalf("a removed variable is named where it must not be (a dial that is removed is removed everywhere: the code, the documentation, the compose files and the tests):\n  %s\nNOT CHECKED: %s* (preserved history), ignored files, and what is not in this repository (the parent's OWNER-ACTIONS.md)",
			strings.Join(problems, "\n  "), historyPrefix)
	}
	t.Logf("checked %d tracked files for %d removed variable(s); %d file(s) may name one (%s*, ignored files and the parent repository were not read)", scanned, len(names), len(removedAllowed), historyPrefix)
}

// TestRemovedVariables_TheGateSeesAnotherFileAStaleAllowanceAndSkipsOnlyHistory is the control of the gate: the same functions, over a tree the test makes, go red for the reasons they name and stay
// green on a good tree.
func TestRemovedVariables_TheGateSeesAnotherFileAStaleAllowanceAndSkipsOnlyHistory(t *testing.T) {
	const name = "PLATFORM_SOMETHING_REMOVED"
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("registry.go", "var _ = \""+name+"\"\n")
	write("Docs/reference.md", "- `"+name+"`\n")
	write(historyPrefix+"old.md", "history says "+name+"\n")
	write("cmd/other.go", "// still names "+name+"\n")
	write("compose.yml", "      "+name+": \"false\"\n")
	write("clean.txt", "nothing here\n")
	list := []string{"registry.go", "Docs/reference.md", historyPrefix + "old.md", "cmd/other.go", "compose.yml", "clean.txt", "deleted-but-tracked.txt"}
	allow := []removedAllow{{"registry.go", true, "the list"}, {"Docs/reference.md", true, "the doc"}}

	named, scanned, err := scanForNames(dir, list, []string{name})
	if err != nil || scanned != 5 {
		t.Fatalf("scanned %d files, %v; want 5 read (the history file is skipped, the missing one is not an error)", scanned, err)
	}
	if _, ok := named[historyPrefix+"old.md"]; ok {
		t.Error("a file under the history directory was judged")
	}
	problems := removedNameProblems(named, allow)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"cmd/other.go names the removed variable(s) " + name, "compose.yml names the removed variable(s) " + name} {
		if !strings.Contains(joined, want) {
			t.Errorf("the gate did not name a file that names the variable: want %q in\n%s", want, joined)
		}
	}
	if len(problems) != 2 {
		t.Errorf("%d problems, want exactly the two files that are not allowed:\n%s", len(problems), joined)
	}

	// the same tree with the two offending files taken out of the list is green (the control of the control)
	good, _, _ := scanForNames(dir, []string{"registry.go", "Docs/reference.md", historyPrefix + "old.md", "clean.txt"}, []string{name})
	if p := removedNameProblems(good, allow); len(p) != 0 {
		t.Errorf("a clean tree was refused: %v", p)
	}
	// a required allowance for a file that names nothing is stale
	stale := append([]removedAllow(nil), allow...)
	stale = append(stale, removedAllow{"clean.txt", true, "it does not"})
	if p := removedNameProblems(good, stale); len(p) != 1 || !strings.Contains(p[0], "clean.txt") || !strings.Contains(p[0], "names nothing") {
		t.Errorf("a stale required allowance was not named: %v", p)
	}
	// an allowance that is not required may name nothing
	optional := append([]removedAllow(nil), allow...)
	optional = append(optional, removedAllow{"clean.txt", false, "may or may not"})
	if p := removedNameProblems(good, optional); len(p) != 0 {
		t.Errorf("an optional allowance for a clean file was refused: %v", p)
	}
	// a name embedded in a longer word is still the name (the gate is a substring search: the variable cannot hide in a longer line)
	write("embedded.txt", "x"+name+"y\n")
	if n, _, _ := scanForNames(dir, []string{"embedded.txt"}, []string{name}); len(n["embedded.txt"]) != 1 {
		t.Error("a file that holds the name inside a longer word was not found")
	}
	// an unreadable file is an error, not a pass
	if _, _, err := scanForNames(dir, []string{"Docs"}, []string{name}); err == nil {
		t.Error("a directory in the list was read as an empty file")
	}
	// the real list has the allowances it says (every path is slash-separated and relative)
	for _, a := range removedAllowed {
		if strings.HasPrefix(a.path, "/") || strings.Contains(a.path, "\\") || a.why == "" {
			t.Errorf("allowance %+v is malformed", a)
		}
	}
	// the git listing fails loudly in a directory that is not a repository
	if _, err := trackedFiles(t.TempDir()); err == nil {
		t.Error("a directory that is not a repository was listed as if it were one")
	}
}
