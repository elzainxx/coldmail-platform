package config_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The two housekeeping dials of Phase 5 (S7, S8): PLATFORM_JOB_PAUSE_MAX_DAYS (default 7, 1 to 90) and PLATFORM_CONTENT_RETENTION_DAYS (default 7, 1 to 365). Load reads them into Config.JobPauseMax and
// Config.ContentRetention; internal/app hands them to jobengine.Config.PauseMax and RetentionAfter (internal/app's test proves each value arrives), which the engine hands to the job store. What is proved HERE
// is what each variable may say, that its default and bounds are the ones the engine and the store take, and that its meaning says what the owner must know.

type hkDial struct {
	name   string
	def    int
	hi     int
	read   func(config.Config) time.Duration
	engine time.Duration // the engine's own top for it
	store  time.Duration // the store's own top for it
}

func hkDials() []hkDial {
	return []hkDial{
		{config.VarJobPauseMaxDays, 7, 90, func(c config.Config) time.Duration { return c.JobPauseMax }, jobengine.MaxPauseMax, jobs.MaxPauseLimit},
		{config.VarContentRetentionDays, 7, 365, func(c config.Config) time.Duration { return c.ContentRetention }, jobengine.MaxRetentionAfter, jobs.MaxContentRetention},
	}
}

const hkDay = 24 * time.Hour

func TestHousekeepingDials_DefaultsEdgesAndRefusalsAreAsDocumented(t *testing.T) {
	if len(hkDials()) != 2 {
		t.Fatalf("%d day dials, there are two", len(hkDials()))
	}
	for _, d := range hkDials() {
		load := func(v *string) (config.Config, error) {
			extra := map[string]string{}
			if v != nil {
				extra[d.name] = *v
			}
			return config.Load(lockEnv(extra))
		}
		days := func(n int) time.Duration { return time.Duration(n) * hkDay }
		for _, c := range []struct {
			name string
			in   *string
			want time.Duration
		}{
			{"unset", nil, days(d.def)}, {"blank", ptr("  "), days(d.def)}, {"the lowest", ptr("1"), days(1)}, {"the highest", ptr(strconv.Itoa(d.hi)), days(d.hi)},
			{"padded with space", ptr(" " + strconv.Itoa(d.hi) + "\t"), days(d.hi)},
		} {
			cfg, err := load(c.in)
			if err != nil || d.read(cfg) != c.want {
				t.Errorf("%s, %s: loaded %v, %v; want %v", d.name, c.name, d.read(cfg), err, c.want)
			}
		}
		for _, bad := range []string{"0", strconv.Itoa(d.hi + 1), "-1", "1.5", "1e3", "a-week-XYZ123", "0x10", "7d", "+5x"} {
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
		// the bounds in the message are the documented ones
		_, err := load(ptr("0"))
		if err == nil || !strings.Contains(err.Error(), "between 1 and "+strconv.Itoa(d.hi)) {
			t.Errorf("%s=0: %v, want a refusal that says `between 1 and %d`", d.name, err, d.hi)
		}
	}
}

// The defaults are the engine's own and the tops are the engine's and the store's: one number in three places, so that a value the dial accepts is never one the engine or the store refuses.
func TestHousekeepingDials_TheDefaultsAndTheTopsAreTheEnginesAndTheStores(t *testing.T) {
	cfg, err := config.Load(lockEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JobPauseMax != jobengine.DefaultPauseMax || cfg.ContentRetention != jobengine.DefaultRetentionAfter {
		t.Errorf("the dials default to %s and %s, the engine's own defaults are %s and %s", cfg.JobPauseMax, cfg.ContentRetention, jobengine.DefaultPauseMax, jobengine.DefaultRetentionAfter)
	}
	for _, d := range hkDials() {
		if top := time.Duration(d.hi) * hkDay; top != d.engine || top != d.store {
			t.Errorf("%s: the dial's top is %s, the engine takes up to %s and the store up to %s", d.name, top, d.engine, d.store)
		}
		// the control: the top of the dial is accepted by the engine itself (a dial that Load takes and jobengine.New refuses would stop the service at start-up)
		eng := jobengine.Config{}
		switch d.name {
		case config.VarJobPauseMaxDays:
			eng.PauseMax = time.Duration(d.hi) * hkDay
		default:
			eng.RetentionAfter = time.Duration(d.hi) * hkDay
		}
		if _, err := jobengine.New(jobengine.Deps{Store: new(jobs.Store), Modules: &jobenginetest.Modules{}, Leader: jobenginetest.NewGate(true)}, eng); err != nil {
			t.Errorf("%s: the engine refused the dial's top: %v", d.name, err)
		}
		eng.PauseMax, eng.RetentionAfter = 0, 0
		if d.name == config.VarJobPauseMaxDays {
			eng.PauseMax = time.Duration(d.hi)*hkDay + time.Second
		} else {
			eng.RetentionAfter = time.Duration(d.hi)*hkDay + time.Second
		}
		if _, err := jobengine.New(jobengine.Deps{Store: new(jobs.Store), Modules: &jobenginetest.Modules{}, Leader: jobenginetest.NewGate(true)}, eng); err == nil {
			t.Errorf("%s: the engine accepted one second more than the dial's top: the control cannot fail", d.name)
		}
	}
}

// A dial the owner reads says what it does, what its range is and where it is consumed.
func TestHousekeepingDials_TheMeaningSaysWhatTheOwnerMustKnow(t *testing.T) {
	facts := map[string][]string{
		config.VarJobPauseMaxDays: {"database's clock", "cancelling", "paused before its start", "released", "slot", "pause_limit", "resumes at the very moment", "1 to 90", "jobengine.Config.PauseMax"},
		config.VarContentRetentionDays: {"finished_at", "`[removed]`", "`rendered_text`", "`unsubscribe_url`", "kept for ever", "has not ended", "rows that belong to no job", "1 to 365", "campaign template", "merge data", "`content_cleared_at`", "the CSV download are unchanged",
			"jobengine.Config.RetentionAfter"},
	}
	seen := 0
	for _, v := range config.Vars() {
		want, ok := facts[v.Name]
		if !ok {
			continue
		}
		seen++
		if v.Secret || v.Required {
			t.Errorf("%s is declared secret or required: %+v", v.Name, v)
		}
		if !strings.Contains(v.Doc, "Consumed by") {
			t.Errorf("the Meaning of %s has no consumer", v.Name)
		}
		for _, f := range want {
			if !strings.Contains(v.Doc, f) {
				t.Errorf("the Meaning of %s no longer says %q", v.Name, f)
			}
		}
		if strings.Contains(v.Doc, "|") {
			t.Errorf("the Meaning of %s holds a pipe, which would split its row of the markdown table", v.Name)
		}
		// the control: a Meaning with one fact taken out is missing exactly that fact
		if strings.Contains(strings.Replace(v.Doc, want[0], "...", 1), want[0]) {
			t.Errorf("the control cannot fail: removing %q left it in the text of %s", want[0], v.Name)
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of the two housekeeping dials in Vars()", seen)
	}
}
