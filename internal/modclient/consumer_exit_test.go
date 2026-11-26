package modclient_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/config"
)

// This test is in the EXTERNAL test package of modclient: it runs the whole service (internal/app), and the service now reaches the modules through the adapters, which import modclient, so an
// internal test of modclient that imported the service would be an import cycle. It is the same test as before, moved, and it proves the same thing.

func dialEnv(extra map[string]string) func(string) string {
	m := map[string]string{
		config.VarDatabaseURL:  "postgres://platform_app:pw@db.example:5432/postgres",
		config.VarM3URL:        "http://m3.example:8080",
		config.VarM3AdminToken: "operator-operator-ok",
	}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

// An invalid value stops the process at start-up with exit status 2, naming the variable and the reason, never the value (the entry point's own check, not only Load's); the
// controls are the accepted edges, which load.
func TestModuleDials_InvalidValuesStopTheProcessAtStartupWithExitStatus2(t *testing.T) {
	bad := map[string][]string{
		config.VarModuleTimeoutMS:      {"499", "0", "-1", "300001", "soon-XYZ123", "1.5"},
		config.VarModuleRetries:        {"-1", "11", "often-XYZ123", "1.5"},
		config.VarModuleRetryBackoffMS: {"-1", "30001", "slowly-XYZ123", "0.5"},
	}
	n := 0
	for name, values := range bad {
		for _, v := range values {
			n++
			err := app.Run(context.Background(), dialEnv(map[string]string{name: v}), io.Discard)
			var ce *config.Error
			if !errors.As(err, &ce) {
				t.Errorf("%s=%q: Run = %v, want a *config.Error", name, v, err)
				continue
			}
			if got := app.ExitCode(err); got != 2 {
				t.Errorf("%s=%q: exit status %d, want 2", name, v, got)
			}
			if len(ce.Problems) != 1 || ce.Problems[0].Name != name {
				t.Errorf("%s=%q: problems %+v, want exactly one, naming the variable", name, v, ce.Problems)
			}
			if len(v) >= 5 && strings.Contains(err.Error(), v) {
				t.Errorf("%s=%q: the error echoes the value: %v", name, v, err)
			}
		}
	}
	if n < 14 {
		t.Fatalf("only %d invalid values were tried", n)
	}
	for name, ok := range map[string][]string{
		config.VarModuleTimeoutMS:      {"500", "300000", "15000", "  "},
		config.VarModuleRetries:        {"0", "10", "2", "  "},
		config.VarModuleRetryBackoffMS: {"0", "30000", "250", "  "},
	} {
		for _, v := range ok {
			if _, err := config.Load(dialEnv(map[string]string{name: v})); err != nil {
				t.Errorf("%s=%q was refused: %v", name, v, err)
			}
		}
	}
}
