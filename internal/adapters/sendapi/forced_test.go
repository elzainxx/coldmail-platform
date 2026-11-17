package sendapi_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
)

// tag builds a registered-form tag at run time, so that this file (which the registry does not scan) never carries the literal and a scan of the repository does not count it.
func tag(id, why string) string { return "// MODULE-" + "FORCED(" + id + "): " + why }

func writeDir(t *testing.T, goSrc map[string]string, forced string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range goSrc {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if forced != "" {
		if err := os.WriteFile(filepath.Join(dir, "forced.txt"), []byte(forced), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func anyHas(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// Every MODULE-FORCED tag in this package's non-test sources has a line in forced.txt and every line has a tag (and names the file the tag is in); with no tags and no lines the check
// is red, because a gate whose subject is empty is red. The controls run the same check on directories with an unregistered tag, a line with no tag, a line naming another file, a repeated
// id, a tag that is not the registered form, and no tags at all.
func TestForcedSpecialCasesAreRegistered(t *testing.T) {
	problems, ids := sendapitest.CheckForced(".")
	if len(problems) != 0 {
		t.Fatalf("the registry and the tags disagree: %v", problems)
	}
	if len(ids) == 0 {
		t.Fatal("no MODULE-FORCED tag found in the package: the subject of the check is empty")
	}
	src := func(extra string) map[string]string {
		return map[string]string{"a.go": "package x\n\n" + tag("ZZ-F1", "a quirk the module forces.") + "\nfunc f() {}\n" + extra}
	}
	t.Run("registered is clean", func(t *testing.T) {
		p, got := sendapitest.CheckForced(writeDir(t, src(""), "ZZ-F1|a.go|a quirk\n"))
		if len(p) != 0 || len(got) != 1 || got[0] != "ZZ-F1" {
			t.Fatalf("%v %v", p, got)
		}
	})
	t.Run("a tag with no line", func(t *testing.T) {
		p, _ := sendapitest.CheckForced(writeDir(t, src(tag("ZZ-F2", "another.")+"\n"), "ZZ-F1|a.go|a quirk\n"))
		if len(p) != 1 || !anyHas(p, "ZZ-F2 in a.go has no line") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a line with no tag", func(t *testing.T) {
		p, _ := sendapitest.CheckForced(writeDir(t, src(""), "ZZ-F1|a.go|a quirk\nZZ-F9|a.go|a ghost\n"))
		if len(p) != 1 || !anyHas(p, "no tag has that id") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a line that names another file", func(t *testing.T) {
		p, _ := sendapitest.CheckForced(writeDir(t, src(""), "ZZ-F1|b.go|a quirk\n"))
		if len(p) != 1 || !anyHas(p, "it is in a.go") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a repeated id", func(t *testing.T) {
		p, _ := sendapitest.CheckForced(writeDir(t, src(tag("ZZ-F1", "again.")+"\n"), "ZZ-F1|a.go|a quirk\n"))
		if !anyHas(p, "tagged twice") {
			t.Fatalf("%v", p)
		}
		p, _ = sendapitest.CheckForced(writeDir(t, src(""), "ZZ-F1|a.go|a quirk\nZZ-F1|a.go|again\n"))
		if !anyHas(p, "lists the id ZZ-F1 twice") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a tag that is not the registered form", func(t *testing.T) {
		bad := map[string]string{"a.go": "package x\n// MODULE-" + "FORCED(ZZ-F1) no colon\n"}
		if p, _ := sendapitest.CheckForced(writeDir(t, bad, "ZZ-F1|a.go|a quirk\n")); !anyHas(p, "not the registered form") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a malformed line", func(t *testing.T) {
		if p, _ := sendapitest.CheckForced(writeDir(t, src(""), "ZZ-F1|a.go\n")); !anyHas(p, "is not `ID|file|why`") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("no tags and no lines", func(t *testing.T) {
		p, _ := sendapitest.CheckForced(writeDir(t, map[string]string{"a.go": "package x\n"}, "# nothing\n"))
		if !anyHas(p, "subject of the check is empty") {
			t.Fatalf("%v", p)
		}
	})
	t.Run("a tag in a test file is not scanned", func(t *testing.T) {
		p, _ := sendapitest.CheckForced(writeDir(t, map[string]string{"a.go": "package x\n" + tag("ZZ-F1", "a quirk.") + "\n", "a_test.go": "package x\n" + tag("ZZ-F7", "ignored.") + "\n"}, "ZZ-F1|a.go|a quirk\n"))
		if len(p) != 0 {
			t.Fatalf("%v", p)
		}
	})
	t.Run("an unreadable directory", func(t *testing.T) {
		if p, _ := sendapitest.CheckForced(filepath.Join(t.TempDir(), "absent")); !anyHas(p, "cannot be read") {
			t.Fatalf("%v", p)
		}
	})
}
