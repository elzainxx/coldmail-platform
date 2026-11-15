package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// fileRe is the one definition of a migration file name: platform_NNNN_<desc>.up.sql or .down.sql. It is the same
// pattern scripts/check-migrations.sh enforces, so the lint and the runner cannot disagree about what a migration is.
var fileRe = regexp.MustCompile(`^platform_([0-9]{4})_(.+)\.(up|down)\.sql$`)

// Parsed is a file name taken apart.
type Parsed struct {
	Version   string // "0001"
	Desc      string // "bootstrap"
	Direction string // "up" or "down"
}

// ParseName takes a bare file name apart. ok is false for anything that is not a migration file name.
func ParseName(name string) (Parsed, bool) {
	m := fileRe.FindStringSubmatch(name)
	if m == nil {
		return Parsed{}, false
	}
	return Parsed{Version: m[1], Desc: m[2], Direction: m[3]}, true
}

// nearMiss reports a name that looks like a migration (platform_ prefix, .sql suffix) but does not parse. Silently
// ignoring it would leave a migration that never applies, which is the same class of failure as a gap.
func nearMiss(name string) bool {
	return strings.HasPrefix(name, "platform_") && strings.HasSuffix(strings.ToLower(name), ".sql")
}

// File is one version: its up file and, if present, its down file.
type File struct {
	Version  string // "0001"
	Name     string // description from the up file name
	UpPath   string
	DownPath string // "" when the version has no down file
}

// Discover reads dir and returns the versions found, ascending. A near-miss name, or two up (or two down) files for
// one version, is an error. Files that are not named like migrations at all are ignored.
func Discover(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return groupNames(dir, names)
}

// groupNames is the pure half of Discover: it turns a list of file names into ordered versions.
func groupNames(dir string, names []string) ([]File, error) {
	byVersion := map[string]*File{}
	var problems []string
	for _, n := range names {
		p, ok := ParseName(n)
		if !ok {
			if nearMiss(n) {
				problems = append(problems, fmt.Sprintf("%s looks like a migration but is not named platform_NNNN_<desc>.up.sql or .down.sql, so it would never be applied", n))
			}
			continue
		}
		f := byVersion[p.Version]
		if f == nil {
			f = &File{Version: p.Version}
			byVersion[p.Version] = f
		}
		path := filepath.Join(dir, n)
		switch p.Direction {
		case "up":
			if f.UpPath != "" {
				problems = append(problems, fmt.Sprintf("two up files for version %s: %s and %s", p.Version, filepath.Base(f.UpPath), n))
				continue
			}
			f.UpPath, f.Name = path, p.Desc
		case "down":
			if f.DownPath != "" {
				problems = append(problems, fmt.Sprintf("two down files for version %s: %s and %s", p.Version, filepath.Base(f.DownPath), n))
				continue
			}
			f.DownPath = path
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("migrations dir %q: %s", dir, strings.Join(problems, "; "))
	}
	var files []File
	for _, f := range byVersion {
		if f.UpPath != "" { // a down file with no up file is not a migration; it is never applied and never selected
			files = append(files, *f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Version < files[j].Version })
	return files, nil
}

// CheckChain is the contiguity guard, a pure function of the files on disk and the versions already applied.
// Every version above 0000 needs its predecessor either applied or on disk; a file whose predecessor is neither
// is a gap (a dropped or renamed migration, a bad rebase) and is an error, never a silent skip. files must be
// ascending and unique, as Discover returns them. It returns nil for an empty list: refusing an empty directory
// is the caller's job, because only the caller knows what the directory was meant to be.
func CheckChain(files []File, applied map[string]bool) error {
	onDisk := make(map[int]bool, len(files))
	prev := -1
	for _, f := range files {
		n, err := strconv.Atoi(f.Version)
		if err != nil {
			return fmt.Errorf("version %q is not a number", f.Version)
		}
		if n <= prev {
			return fmt.Errorf("migration files are not strictly ascending: %s follows %04d", f.Version, prev)
		}
		prev = n
		onDisk[n] = true
	}
	for _, f := range files {
		n, _ := strconv.Atoi(f.Version)
		if n == 0 {
			continue
		}
		pred := fmt.Sprintf("%04d", n-1)
		if !applied[pred] && !onDisk[n-1] {
			return fmt.Errorf("migration chain gap: %s is present but its predecessor %s is neither applied nor on disk", f.Version, pred)
		}
	}
	return nil
}

// NormalizeVersion turns the argument of down into a four-digit version: one to four digits, zero-padded.
func NormalizeVersion(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 4 {
		return "", configErr("down needs a version of one to four digits, for example 0001")
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return "", configErr("down needs a version of one to four digits, for example 0001")
		}
	}
	return strings.Repeat("0", 4-len(v)) + v, nil
}
