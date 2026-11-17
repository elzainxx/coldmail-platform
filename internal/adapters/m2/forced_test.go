package m2_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
)

func anyHas(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func writeDir(t *testing.T, goSrc, forced string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(goSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "forced.txt"), []byte(forced), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Every MODULE-FORCED tag in this package's non-test sources has a line in forced.txt and every line has a tag; zero of both is red (a gate whose subject is empty is red). The
// controls run the same check on a directory with an unregistered tag, a line with no tag, and no tags at all.
func TestForcedSpecialCasesAreRegistered(t *testing.T) {
	problems, ids := sendapitest.CheckForced(".")
	if len(problems) != 0 {
		t.Fatalf("the registry and the tags disagree: %v", problems)
	}
	if len(ids) == 0 {
		t.Fatal("no MODULE-FORCED tag found in the package: the subject of the check is empty")
	}
	// the tags the research says module 2 forces on its own route: the cancel position and the empty foreign cancel
	want := map[string]bool{"M2-F1": false, "M2-F2": false}
	for _, id := range ids {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Fatalf("the tag %s is not in the package", id)
		}
	}
	tag := "// MODULE-" + "FORCED(ZZ-F1): a quirk the module forces."
	if p, got := sendapitest.CheckForced(writeDir(t, "package x\n"+tag+"\n", "ZZ-F1|a.go|a quirk\n")); len(p) != 0 || len(got) != 1 {
		t.Fatalf("the control (a registered tag) is refused: %v %v", p, got)
	}
	if p, _ := sendapitest.CheckForced(writeDir(t, "package x\n"+tag+"\n", "# none\n")); len(p) != 1 || !anyHas(p, "ZZ-F1 in a.go has no line") {
		t.Fatalf("an unregistered tag: %v", p)
	}
	if p, _ := sendapitest.CheckForced(writeDir(t, "package x\n", "ZZ-F1|a.go|a quirk\n")); len(p) != 1 || !anyHas(p, "no tag has that id") {
		t.Fatalf("a line with no tag: %v", p)
	}
	if p, _ := sendapitest.CheckForced(writeDir(t, "package x\n", "")); !anyHas(p, "subject of the check is empty") {
		t.Fatalf("an empty subject: %v", p)
	}
}
