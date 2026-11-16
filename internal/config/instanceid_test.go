package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
)

// PLATFORM_INSTANCE_ID (Phase 2 close-out D9) names a platform instance: it is the application_name of the database session that holds the leader lock of the entitlement loops, so that
// pg_stat_activity shows which instance leads. Its consumer is internal/entitlements (Config.InstanceID, ResolveInstanceID); these tests are the proof that the variable is validated by the
// process's own loader, that the default means "the host name" and that the loader and the consumer apply the SAME rule to a name.

func idEnv(extra map[string]string) func(string) string {
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

func TestInstanceIDDial_TheDefaultStandsForTheHostNameAndLoadsEmpty(t *testing.T) {
	var v config.Var
	for _, c := range config.Vars() {
		if c.Name == config.VarInstanceID {
			v = c
		}
	}
	if v.Name == "" || v.Required || v.Secret || v.Default != "(hostname)" {
		t.Fatalf("the declared dial is %+v, want an optional, non-secret variable whose documented default is (hostname)", v)
	}
	// Unset, blank, and the default spelled out all load to the same thing: an empty name, which the service resolves to the host name.
	for name, env := range map[string]map[string]string{
		"unset":            nil,
		"blank":            {config.VarInstanceID: "   "},
		"the default text": {config.VarInstanceID: "(hostname)"},
	} {
		cfg, err := config.Load(idEnv(env))
		if err != nil || cfg.InstanceID != "" {
			t.Errorf("%s: InstanceID = %q, %v, want \"\" (the host name, resolved by the service)", name, cfg.InstanceID, err)
		}
	}
	// The service resolves it to a valid name (never an error for an unset variable).
	got, err := entitlements.ResolveInstanceID("")
	if err != nil || !entitlements.ValidInstanceID(got) {
		t.Errorf("the default resolves to %q, %v, want a valid instance name", got, err)
	}
}

func TestInstanceIDDial_ANameReachesTheConfigAndIsValidated(t *testing.T) {
	long63, long64 := strings.Repeat("i", 63), strings.Repeat("i", 64)
	for _, c := range []struct {
		name  string
		value string
		ok    bool
	}{
		{"a typical name", "platform-1", true},
		{"one byte", "a", true},
		{"63 bytes", long63, true},
		{"a space inside", "platform one", true},
		{"64 bytes", long64, false},
		{"a tab inside", "plat\tform-XYZ123", false},
		{"a newline inside", "plat\nform-XYZ123", false},
		{"a non-ASCII letter", "platéform-XYZ123", false},
		{"a delete character", "plat\x7fform-XYZ123", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := config.Load(idEnv(map[string]string{config.VarInstanceID: c.value}))
			if c.ok {
				if err != nil || cfg.InstanceID != c.value {
					t.Fatalf("Load = %q, %v, want the name kept", cfg.InstanceID, err)
				}
			} else {
				var ce *config.Error
				if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarInstanceID || !strings.Contains(ce.Problems[0].Reason, "1 to 63 printable ASCII") {
					t.Fatalf("Load = %v, want exactly one problem naming %s and saying what a name is", err, config.VarInstanceID)
				}
				if strings.Contains(err.Error(), c.value) || strings.Contains(err.Error(), "XYZ123") {
					t.Errorf("the error echoes the rejected value: %v", err)
				}
			}
			// The consumer applies the same rule: a name the loader accepts is a name the service accepts, and the other way round (so a variable that loads can never be refused at start-up).
			if got := entitlements.ValidInstanceID(c.value); got != c.ok {
				t.Errorf("entitlements.ValidInstanceID(%q) = %v but the loader says %v: the two rules drifted apart", c.value, got, c.ok)
			}
		})
	}
}

// The value of the dial reaches the printed configuration (every field is rendered) and nothing secret comes with it.
func TestInstanceIDDial_IsRenderedInTheConfigAndChangesIt(t *testing.T) {
	base, err := config.Load(idEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	named, err := config.Load(idEnv(map[string]string{config.VarInstanceID: "platform-2"}))
	if err != nil {
		t.Fatal(err)
	}
	if base == named || named.InstanceID != "platform-2" {
		t.Fatalf("the dial changed nothing in the Config: %v vs %v", base, named)
	}
	if !strings.Contains(named.String(), "instance_id=platform-2") || strings.Contains(base.String(), "instance_id=platform") {
		t.Errorf("String() does not render the instance: %q / %q", named.String(), base.String())
	}
}
