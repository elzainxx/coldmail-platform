package app

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/jobengine"
)

// Phase 5 S7 and S8: the two housekeeping dials of the engine reach it (engineConfig), each alone and each as days, and the engine accepts the edges of both. The dials themselves (bounds, refusals,
// the meaning the owner reads) are in internal/config; what the engine does with them against a database is in tests/harness/jobs_pause_limit_test.go and jobs_content_retention_test.go.

func TestEngineConfig_TheTwoHousekeepingDialsReachTheEngineEachAloneAndInDays(t *testing.T) {
	cfg := loadEngineCfg(t, nil)
	got := engineConfig(cfg)
	if got.PauseMax != 7*24*time.Hour || got.RetentionAfter != 7*24*time.Hour {
		t.Fatalf("the defaults reached the engine as %s and %s, want 7 days each", got.PauseMax, got.RetentionAfter)
	}
	for _, c := range []struct {
		name, value string
		change      func(*jobengine.Config)
	}{
		{config.VarJobPauseMaxDays, "3", func(c *jobengine.Config) { c.PauseMax = 3 * 24 * time.Hour }},
		{config.VarContentRetentionDays, "30", func(c *jobengine.Config) { c.RetentionAfter = 30 * 24 * time.Hour }},
	} {
		one := engineConfig(loadEngineCfg(t, map[string]string{c.name: c.value}))
		exp := engineConfig(cfg)
		c.change(&exp)
		if !reflect.DeepEqual(one, exp) {
			t.Errorf("%s alone reached the engine as %+v, want only its own field changed (%+v)", c.name, one, exp)
		}
		// the control: the comparison sees a change (the two engine configs differ by exactly the field that was changed)
		if reflect.DeepEqual(one, engineConfig(cfg)) {
			t.Errorf("%s=%s changed nothing that reaches the engine", c.name, c.value)
		}
	}
}

func TestNewJobEngine_TheEdgesOfTheHousekeepingDialsAreAcceptedByTheEngineItself(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	for name, env := range map[string]map[string]string{
		"the lowest":  {config.VarJobPauseMaxDays: "1", config.VarContentRetentionDays: "1"},
		"the highest": {config.VarJobPauseMaxDays: "90", config.VarContentRetentionDays: "365"},
	} {
		cfg := loadEngineCfg(t, env)
		deps := apiDeps{}
		if err := deps.build(pool, cfg, log); err != nil {
			t.Fatal(err)
		}
		leader, err := entitlements.NewLeader(pool, "platform-t", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		eng, err := newJobEngine(cfg, deps, leader, log)
		if err != nil || eng == nil {
			t.Errorf("%s: newJobEngine = %v, %v: the engine refused a value its own dial allows", name, eng, err)
		}
	}
}
