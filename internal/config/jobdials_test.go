package config_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The three limits of a job and of the job API (Phase 4 B14b; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"): PLATFORM_JOB_MAX_ROWS,
// PLATFORM_JOB_MAX_START_DELAY_MIN and PLATFORM_RESULTS_PAGE_MAX. Load reads them into Config.JobMaxRows, Config.JobMaxStartDelay and Config.ResultsPageMax; internal/app hands them to the job
// store (jobs.WithMaxRows, jobs.WithMaxStartDelay) and to the API (api.Deps.ResultsPageMax), and internal/app's jobdials_test.go proves each value arrives (and that the API acts on it). What is
// proved HERE is what each variable may say, and that its bounds agree with what the consumers can take.

type jobDial struct {
	name   string
	def    string
	lo, hi int
	read   func(config.Config) time.Duration // the dial as a Duration, for one comparison across the three
	unit   time.Duration
}

func jobDials() []jobDial {
	return []jobDial{
		{config.VarJobMaxRows, "100000", 1, 100000, func(c config.Config) time.Duration { return time.Duration(c.JobMaxRows) }, 1},
		{config.VarJobMaxStartDelayMin, "30", 1, 1440, func(c config.Config) time.Duration { return c.JobMaxStartDelay }, time.Minute},
		{config.VarResultsPageMax, "1000", 1, 1000, func(c config.Config) time.Duration { return time.Duration(c.ResultsPageMax) }, 1},
	}
}

func TestJobDials_DefaultsEdgesAndRefusalsAreAsDocumented(t *testing.T) {
	for _, d := range jobDials() {
		load := func(v *string) (config.Config, error) {
			extra := map[string]string{}
			if v != nil {
				extra[d.name] = *v
			}
			return config.Load(lockEnv(extra))
		}
		want := func(n int) time.Duration { return time.Duration(n) * d.unit }

		// the default, unset and blank, and the edges of the range, each beside its refused neighbour
		def, _ := strconv.Atoi(d.def)
		for _, c := range []struct {
			name string
			in   *string
			want time.Duration
		}{
			{"unset", nil, want(def)}, {"blank", ptr("  "), want(def)}, {"the lowest", ptr(strconv.Itoa(d.lo)), want(d.lo)}, {"the highest", ptr(strconv.Itoa(d.hi)), want(d.hi)},
			{"padded with space", ptr(" " + strconv.Itoa(d.hi) + "\t"), want(d.hi)},
		} {
			cfg, err := load(c.in)
			if err != nil || d.read(cfg) != c.want {
				t.Errorf("%s, %s: loaded %v, %v; want %v", d.name, c.name, d.read(cfg), err, c.want)
			}
		}
		for _, bad := range []string{strconv.Itoa(d.lo - 1), strconv.Itoa(d.hi + 1), "-1", "1.5", "1e3", "many-XYZ123", "0x10", "+5x"} {
			_, err := load(&bad)
			var ce *config.Error
			if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != d.name {
				t.Errorf("%s=%q: Load = %v, want exactly one problem naming the variable", d.name, bad, err)
				continue
			}
			if len(bad) >= 5 && strings.Contains(err.Error(), bad) {
				t.Errorf("%s=%q: the error echoes the value: %v", d.name, bad, err)
			}
		}
	}
}

// The bounds are the consumers' own: a dial that could be set to what its consumer refuses would be a start-up that fails at New, or a service that serves less than it says.
func TestJobDials_TheBoundsAreWhatTheirConsumersCanTake(t *testing.T) {
	ds := jobDials()
	rows, delay, page := ds[0], ds[1], ds[2]
	if rows.def != strconv.Itoa(jobs.DefaultMaxRows) || rows.hi != jobs.DefaultMaxRows {
		t.Errorf("the row dial's default %s and highest %d are not the store's default %d: module 1's limit is both", rows.def, rows.hi, jobs.DefaultMaxRows)
	}
	if rows.hi > jobs.HardMaxRows {
		t.Errorf("the row dial may be %d, the store takes at most %d", rows.hi, jobs.HardMaxRows)
	}
	if time.Duration(delay.hi)*time.Minute > jobs.MaxStartDelayLimit {
		t.Errorf("the start delay dial may be %d minutes, the store takes at most %s", delay.hi, jobs.MaxStartDelayLimit)
	}
	if delay.def != strconv.Itoa(int(jobs.DefaultMaxStartDelay/time.Minute)) {
		t.Errorf("the start delay dial defaults to %s minutes, the store's own default is %s", delay.def, jobs.DefaultMaxStartDelay)
	}
	if page.hi != jobs.ResultsPageSize || page.def != strconv.Itoa(jobs.ResultsPageSize) {
		t.Errorf("the results page dial is %s to %d, the page the store reads (and the CSV streams) is %d", page.def, page.hi, jobs.ResultsPageSize)
	}
	// the edges are accepted by the store, the neighbours of them by the dial are not needed: build the store's option at each edge
	for _, n := range []int{rows.lo, rows.hi} {
		if n < 1 || n > jobs.HardMaxRows {
			t.Errorf("the row dial's edge %d is outside what the store takes", n)
		}
	}
}

func ptr(s string) *string { return &s }

// A dial the owner reads says what it does, what its range is and where it is consumed: the Meaning is the one place the owner looks, and a test holds it to the facts.
func jobDialDocMissing(name, doc string) []string {
	facts := map[string][]string{
		config.VarJobMaxRows:          {"too_many_rows", "1 to 100000", "module 1's own limit", "jobs.WithMaxRows"},
		config.VarJobMaxStartDelayMin: {"expires", "released", "added by the database's clock", "keeps the deadline", "1 to 1440", "jobs.WithMaxStartDelay"},
		config.VarResultsPageMax:      {"the largest `limit`", "400 `validation_failed`", "1000 at a time", "1 to 1000", "api.New"},
	}[name]
	var missing []string
	for _, f := range facts {
		if !strings.Contains(doc, f) {
			missing = append(missing, f)
		}
	}
	return missing
}

func TestJobDials_TheMeaningSaysWhatTheOwnerMustKnow(t *testing.T) {
	seen := 0
	for _, v := range config.Vars() {
		switch v.Name {
		case config.VarJobMaxRows, config.VarJobMaxStartDelayMin, config.VarResultsPageMax:
		default:
			continue
		}
		seen++
		if v.Secret || v.Required {
			t.Errorf("%s is declared secret or required: %+v", v.Name, v)
		}
		if m := jobDialDocMissing(v.Name, v.Doc); len(m) != 0 {
			t.Errorf("the Meaning of %s no longer says %q", v.Name, m)
		}
		// the control: a Meaning with one fact taken out is reported as missing exactly that fact
		for _, fact := range []string{"Integer", "Consumed by"} {
			if !strings.Contains(v.Doc, fact) {
				t.Errorf("the Meaning of %s has no %q", v.Name, fact)
			}
		}
		first := jobDialDocMissing(v.Name, "")[0]
		if m := jobDialDocMissing(v.Name, strings.Replace(v.Doc, first, "...", 1)); len(m) != 1 || m[0] != first {
			t.Errorf("a Meaning without %q was reported as missing %q", first, m)
		}
	}
	if seen != 3 {
		t.Fatalf("found %d of the three job dials in Vars(): the check looked at the wrong thing", seen)
	}
}
