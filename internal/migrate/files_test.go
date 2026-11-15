package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseName(t *testing.T) {
	good := []struct {
		name string
		want Parsed
	}{
		{"platform_0000_bootstrap.up.sql", Parsed{"0000", "bootstrap", "up"}},
		{"platform_0000_bootstrap.down.sql", Parsed{"0000", "bootstrap", "down"}},
		{"platform_0012_add_users_table.up.sql", Parsed{"0012", "add_users_table", "up"}},
		{"platform_9999_x.down.sql", Parsed{"9999", "x", "down"}},
		{"platform_0001_a.b.up.sql", Parsed{"0001", "a.b", "up"}}, // the description is everything up to the last .up.sql
	}
	for _, c := range good {
		got, ok := ParseName(c.name)
		if !ok || got != c.want {
			t.Errorf("ParseName(%q) = %+v, %v; want %+v, true", c.name, got, ok, c.want)
		}
	}
	bad := []string{
		"",
		"platform_0001.up.sql",                // no description
		"platform_0001_.up.sql",               // empty description
		"platform_001_x.up.sql",               // three digits
		"platform_00001_x.up.sql",             // five digits
		"platform_000a_x.up.sql",              // not digits
		"platform_0001_x.UP.sql",              // case matters
		"platform_0001_x.up.SQL",              // case matters
		"platform_0001_x.sql",                 // no direction
		"platform_0001_x.up.sql.bak",          // trailing junk
		"Platform_0001_x.up.sql",              // prefix case
		"send_managed_0001_x.up.sql",          // another module's file
		"x/platform_0001_x.up.sql",            // a path, not a name
		"platform_0001_x.sideways.sql",        // unknown direction
		" platform_0001_x.up.sql",             // leading space
		"platform_0001_x.up.sql\n",            // trailing newline
		"platform-0001-x.up.sql",              // wrong separators
		"platform_0001_x.up.sql.up.sql.down",  // trailing junk after a valid-looking tail
		"platform_0x01_x.up.sql",              // hex-looking
		"platform_0001_x.down.sql.sql",        // doubled suffix
		"platform_٠٠٠١_x.up.sql",              // non-ASCII digits are not digits
		"platform_0001_x.up.sql/../etc/hosts", // path trickery
	}
	for _, name := range bad {
		if got, ok := ParseName(name); ok {
			t.Errorf("ParseName(%q) = %+v, true; want it rejected", name, got)
		}
	}
}

// writeFiles creates empty files with the given names in a new temp directory, in the order given.
func writeFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("-- test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func versionsOf(files []File) string {
	var v []string
	for _, f := range files {
		v = append(v, f.Version)
	}
	return strings.Join(v, ",")
}

func TestDiscover_OrdersAscendingWhateverTheCreationOrder(t *testing.T) {
	dir := writeFiles(t,
		"platform_0003_c.down.sql", "platform_0003_c.up.sql",
		"platform_0000_bootstrap.up.sql", "platform_0000_bootstrap.down.sql",
		"platform_0010_late.up.sql", "platform_0010_late.down.sql",
		"platform_0002_b.up.sql", "platform_0002_b.down.sql",
		"platform_0001_a.down.sql", "platform_0001_a.up.sql",
	)
	files, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := versionsOf(files), "0000,0001,0002,0003,0010"; got != want {
		t.Fatalf("versions = %s, want %s", got, want)
	}
	for _, f := range files {
		if filepath.Base(f.UpPath) != "platform_"+f.Version+"_"+f.Name+".up.sql" {
			t.Errorf("version %s: UpPath %q does not match its name %q", f.Version, f.UpPath, f.Name)
		}
		if f.DownPath == "" || !strings.HasSuffix(f.DownPath, ".down.sql") {
			t.Errorf("version %s: DownPath = %q, want its down file", f.Version, f.DownPath)
		}
	}
}

func TestDiscover_IgnoresUnrelatedFilesAndAnOrphanDown(t *testing.T) {
	dir := writeFiles(t,
		"platform_0000_bootstrap.up.sql", "platform_0000_bootstrap.down.sql",
		"README.md", "notes.txt", "send_managed_0001_x.up.sql", // not ours: wrong prefix, wrong extension
		"platform_0005_orphan.down.sql", // a down with no up is not a migration
	)
	if err := os.Mkdir(filepath.Join(dir, "platform_0002_dir.up.sql"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := versionsOf(files); got != "0000" {
		t.Fatalf("versions = %s, want only 0000 (the unrelated files, the orphan down and the directory must be ignored)", got)
	}
}

func TestDiscover_ANearMissIsAnErrorNotASkip(t *testing.T) {
	for _, bad := range []string{
		"platform_001_short.up.sql",
		"platform_0001_x.sql",
		"platform_0001_x.UP.sql",
		"platform_notes.sql",
		"platform_0001_x.up.SQL",
	} {
		dir := writeFiles(t, "platform_0000_bootstrap.up.sql", "platform_0000_bootstrap.down.sql", bad)
		_, err := Discover(dir)
		if err == nil {
			t.Errorf("%s next to a valid file: Discover returned no error, so the file would silently never apply", bad)
			continue
		}
		if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "never be applied") {
			t.Errorf("%s: error %q should name the file and say it would never be applied", bad, err)
		}
	}
	// Control: the same directory without the near-miss is accepted.
	if _, err := Discover(writeFiles(t, "platform_0000_bootstrap.up.sql", "platform_0000_bootstrap.down.sql")); err != nil {
		t.Fatalf("control: a clean directory was rejected: %v", err)
	}
}

func TestDiscover_TwoFilesForOneVersionIsAnError(t *testing.T) {
	_, err := Discover(writeFiles(t, "platform_0001_a.up.sql", "platform_0001_b.up.sql"))
	if err == nil || !strings.Contains(err.Error(), "two up files for version 0001") {
		t.Fatalf("two up files: err = %v", err)
	}
	_, err = Discover(writeFiles(t, "platform_0001_a.up.sql", "platform_0001_a.down.sql", "platform_0001_b.down.sql"))
	if err == nil || !strings.Contains(err.Error(), "two down files for version 0001") {
		t.Fatalf("two down files: err = %v", err)
	}
}

func TestDiscover_MissingDirectory(t *testing.T) {
	_, err := Discover(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil || !strings.Contains(err.Error(), "read migrations dir") {
		t.Fatalf("err = %v, want a read-migrations-dir error", err)
	}
}

func filesFor(versions ...string) []File {
	var out []File
	for _, v := range versions {
		out = append(out, File{Version: v, Name: "x"})
	}
	return out
}

// The gap guard as a pure function, with its control: the SAME list without the gap passes.
func TestCheckChain_AGapIsAnErrorAndTheSameListWithoutItPasses(t *testing.T) {
	gap := filesFor("0000", "0002")
	err := CheckChain(gap, map[string]bool{})
	if err == nil {
		t.Fatal("0000 and 0002 with no 0001: CheckChain returned nil, so the guard is a no-op")
	}
	for _, want := range []string{"chain gap", "0002", "0001"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q: it must name the file and the missing predecessor", err, want)
		}
	}
	if err := CheckChain(filesFor("0000", "0001", "0002"), map[string]bool{}); err != nil {
		t.Fatalf("control: the same chain with 0001 present was rejected: %v", err)
	}
}

func TestCheckChain_PredecessorMayBeAppliedInsteadOfOnDisk(t *testing.T) {
	cases := []struct {
		name    string
		files   []File
		applied map[string]bool
		wantErr string // "" means must pass
	}{
		{"empty list", nil, nil, ""},
		{"bootstrap alone", filesFor("0000"), nil, ""},
		{"full chain", filesFor("0000", "0001", "0002", "0003"), nil, ""},
		{"predecessor applied, not on disk", filesFor("0002"), map[string]bool{"0000": true, "0001": true}, ""},
		{"chain must start at 0000", filesFor("0001"), nil, "0000"},
		{"first file is 0005", filesFor("0005"), nil, "0004"},
		{"gap in the middle", filesFor("0000", "0001", "0003"), nil, "0002"},
		{"gap of several", filesFor("0000", "0004"), nil, "0003"},
		{"gap even when a lower version is applied", filesFor("0003"), map[string]bool{"0001": true}, "0002"},
		{"predecessor applied fills the gap", filesFor("0000", "0003"), map[string]bool{"0002": true}, ""},
	}
	for _, c := range cases {
		err := CheckChain(c.files, c.applied)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", c.name, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: no error, want one naming %s", c.name, c.wantErr)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: error %q does not name %s", c.name, err, c.wantErr)
		}
	}
}

func TestCheckChain_RefusesAListThatIsNotStrictlyAscending(t *testing.T) {
	for name, files := range map[string][]File{
		"duplicate": filesFor("0000", "0001", "0001"),
		"unsorted":  filesFor("0000", "0002", "0001"),
	} {
		err := CheckChain(files, nil)
		if err == nil || !strings.Contains(err.Error(), "not strictly ascending") {
			t.Errorf("%s: err = %v, want a not-strictly-ascending error", name, err)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{"0": "0000", "1": "0001", "12": "0012", "0001": "0001", " 7 ": "0007", "9999": "9999"} {
		got, err := NormalizeVersion(in)
		if err != nil || got != want {
			t.Errorf("NormalizeVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "00001", "abc", "1a", "-1", "1.0", "0x1", "٠٠٠١"} {
		if got, err := NormalizeVersion(in); err == nil {
			t.Errorf("NormalizeVersion(%q) = %q, nil; want an error", in, got)
		} else if _, ok := err.(*ConfigError); !ok {
			t.Errorf("NormalizeVersion(%q): error is %T, want *ConfigError (exit 2)", in, err)
		}
	}
}
