package config_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
)

// The nine dials of the job engine (Phase 4 B9; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"): PLATFORM_JOB_ENGINE_ENABLED, _TICK_MS, _SETTLE_INTERVAL_SEC,
// _STALE_AFTER_MIN_M1, _M2 and _M3, _SUBMIT_BATCH, _SUBMIT_CONCURRENCY and _STEP_TIMEOUT_SEC. Load reads them into Config.JobEngineEnabled, JobTick, JobSettleInterval, JobStaleAfterM1/M2/M3,
// JobSubmitBatch, JobSubmitConcurrency and JobStepTimeout; internal/app builds the engine and its registry from them (engineConfig, registryDeps) and internal/app's jobengine_test.go proves each
// value arrives. What is proved HERE is what each variable may say, and that its default and bounds agree with what the consumer can take.

type engineDial struct {
	name   string
	def    string
	lo, hi int
	read   func(config.Config) time.Duration // the dial as a Duration, for one comparison across the eight
	unit   time.Duration
}

func engineDials() []engineDial {
	return []engineDial{
		{config.VarJobTickMS, "1500", 100, 60000, func(c config.Config) time.Duration { return c.JobTick }, time.Millisecond},
		{config.VarJobSettleIntervalSec, "10", 1, 3600, func(c config.Config) time.Duration { return c.JobSettleInterval }, time.Second},
		{config.VarJobStaleAfterMinM1, "30", 1, 10080, func(c config.Config) time.Duration { return c.JobStaleAfterM1 }, time.Minute},
		{config.VarJobStaleAfterMinM2, "15", 1, 10080, func(c config.Config) time.Duration { return c.JobStaleAfterM2 }, time.Minute},
		{config.VarJobStaleAfterMinM3, "15", 1, 10080, func(c config.Config) time.Duration { return c.JobStaleAfterM3 }, time.Minute},
		{config.VarJobSubmitBatch, "500", 1, 5000, func(c config.Config) time.Duration { return time.Duration(c.JobSubmitBatch) }, 1},
		{config.VarJobSubmitConcurrency, "8", 1, 64, func(c config.Config) time.Duration { return time.Duration(c.JobSubmitConcurrency) }, 1},
		{config.VarJobStepTimeoutSec, "120", 5, 3600, func(c config.Config) time.Duration { return c.JobStepTimeout }, time.Second},
	}
}

func TestJobEngineDials_DefaultsEdgesAndRefusalsAreAsDocumented(t *testing.T) {
	if len(engineDials()) != 8 {
		t.Fatalf("%d numeric engine dials, there are eight", len(engineDials()))
	}
	for _, d := range engineDials() {
		load := func(v *string) (config.Config, error) {
			extra := map[string]string{}
			if v != nil {
				extra[d.name] = *v
			}
			return config.Load(lockEnv(extra))
		}
		want := func(n int) time.Duration { return time.Duration(n) * d.unit }
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

// The switch is strict: exactly `true` and `false`. A spelling that silently read as the other value would be a platform whose jobs never move (or an engine nobody meant to run).
func TestJobEngineEnabled_IsStrictAndDefaultsToOn(t *testing.T) {
	load := func(extra map[string]string) (config.Config, error) { return config.Load(lockEnv(extra)) }
	if cfg, err := load(nil); err != nil || !cfg.JobEngineEnabled {
		t.Fatalf("unset: %v, %v: the engine runs unless it is switched off", cfg.JobEngineEnabled, err)
	}
	if cfg, err := load(map[string]string{config.VarJobEngineEnabled: "  "}); err != nil || !cfg.JobEngineEnabled {
		t.Fatalf("blank: %v, %v: blank means unset", cfg.JobEngineEnabled, err)
	}
	if cfg, err := load(map[string]string{config.VarJobEngineEnabled: "true"}); err != nil || !cfg.JobEngineEnabled {
		t.Errorf("true: %v, %v", cfg.JobEngineEnabled, err)
	}
	if cfg, err := load(map[string]string{config.VarJobEngineEnabled: " false "}); err != nil || cfg.JobEngineEnabled {
		t.Errorf("false: %v, %v: the switch that turns the engine off did not", cfg.JobEngineEnabled, err)
	}
	for _, bad := range []string{"TRUE", "False", "1", "0", "yes", "no", "on", "off", "enabled", "tru"} {
		_, err := load(map[string]string{config.VarJobEngineEnabled: bad})
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarJobEngineEnabled || !strings.Contains(ce.Problems[0].Reason, "true or false") {
			t.Errorf("%q: Load = %v, want one problem naming the variable and saying `true or false`", bad, err)
		}
	}
}

// The defaults are the engine's own: a dial whose default differs from what the code falls back on would be two truths.
func TestJobEngineDials_TheDefaultsAreTheEnginesOwnAndTheBoundsWhatItCanTake(t *testing.T) {
	cfg, err := config.Load(lockEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ dial, engine time.Duration }{
		"tick":         {cfg.JobTick, jobengine.DefaultTick},
		"settle":       {cfg.JobSettleInterval, jobengine.DefaultSettleEvery},
		"stale (m2)":   {cfg.JobStaleAfterM2, jobengine.DefaultStaleAfter},
		"stale (m3)":   {cfg.JobStaleAfterM3, jobengine.DefaultStaleAfter},
		"step timeout": {cfg.JobStepTimeout, jobengine.DefaultStepTimeout},
	} {
		if c.dial != c.engine {
			t.Errorf("the %s dial defaults to %s, the engine's own default is %s", name, c.dial, c.engine)
		}
	}
	if cfg.JobSubmitBatch != jobengine.DefaultSubmitBatch {
		t.Errorf("the submit batch dial defaults to %d, the engine's own default is %d", cfg.JobSubmitBatch, jobengine.DefaultSubmitBatch)
	}
	if cfg.JobSubmitConcurrency != jobengine.DefaultSubmitConcurrency {
		t.Errorf("the submit concurrency dial defaults to %d, the registry's own default is %d", cfg.JobSubmitConcurrency, jobengine.DefaultSubmitConcurrency)
	}
	ds := engineDials()
	conc := ds[6]
	if conc.name != config.VarJobSubmitConcurrency || conc.hi != jobengine.MaxSubmitConcurrency {
		t.Errorf("the concurrency dial's highest value is %d, the registry takes at most %d", conc.hi, jobengine.MaxSubmitConcurrency)
	}
}

// A dial the owner reads says what it does, what its range is and where it is consumed.
func engineDialDocMissing(name, doc string) []string {
	facts := map[string][]string{
		config.VarJobEngineEnabled:     {"`true` or `false` and nothing else", "leader lock", "start-up log says so once", "the same value", "never failed for it", "internal/jobengine"},
		config.VarJobTickMS:            {"Milliseconds between two passes", "100 to 60000", "jobengine.Config.Tick"},
		config.VarJobSettleIntervalSec: {"running job alone", "has not recorded yet", "1 to 3600", "jobengine.Config.SettleEvery"},
		config.VarJobStaleAfterMinM1:   {"module 1 job", "`stale` event", "never failed or expired for being slow", "1 to 10080", "jobengine.Config.StaleAfter"},
		config.VarJobStaleAfterMinM2:   {"module 2 job", "`stale` event", "never failed or expired for being slow", "1 to 10080", "jobengine.Config.StaleAfter"},
		config.VarJobStaleAfterMinM3:   {"module 3 job", "`stale` event", "never failed or expired for being slow", "1 to 10080", "jobengine.Config.StaleAfter"},
		config.VarJobSubmitBatch:       {"module 2 or module 3", "Module 1 takes a job's whole list in one request", "1 to 5000", "jobengine.Config.SubmitBatch"},
		config.VarJobSubmitConcurrency: {"in flight at once", "first row of a batch always goes alone", "1 to 64", "jobengine.RegistryDeps.SubmitConcurrency"},
		config.VarJobStepTimeoutSec:    {"ONE job", "PLATFORM_MODULE_TIMEOUT_MS", "5 to 3600", "jobengine.Config.StepTimeout"},
	}[name]
	var missing []string
	for _, f := range facts {
		if !strings.Contains(doc, f) {
			missing = append(missing, f)
		}
	}
	return missing
}

func TestJobEngineDials_TheMeaningSaysWhatTheOwnerMustKnow(t *testing.T) {
	seen := 0
	for _, v := range config.Vars() {
		if !strings.HasPrefix(v.Name, "PLATFORM_JOB_") || strings.Contains(v.Name, "MAX_") {
			continue
		}
		seen++
		if v.Secret || v.Required {
			t.Errorf("%s is declared secret or required: %+v", v.Name, v)
		}
		if m := engineDialDocMissing(v.Name, v.Doc); len(m) != 0 {
			t.Errorf("the Meaning of %s no longer says %q", v.Name, m)
		}
		for _, fact := range []string{"Consumed by"} {
			if !strings.Contains(v.Doc, fact) {
				t.Errorf("the Meaning of %s has no %q", v.Name, fact)
			}
		}
		// the control: a Meaning with one fact taken out is reported as missing exactly that fact
		first := engineDialDocMissing(v.Name, "")[0]
		if m := engineDialDocMissing(v.Name, strings.Replace(v.Doc, first, "...", 1)); len(m) != 1 || m[0] != first {
			t.Errorf("a Meaning without %q was reported as missing %q", first, m)
		}
	}
	if seen != 9 {
		t.Fatalf("found %d of the nine engine dials in Vars(): the check looked at the wrong thing", seen)
	}
}
