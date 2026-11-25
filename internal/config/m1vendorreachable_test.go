package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
)

// PLATFORM_M1_VENDOR_REACHABLE (Phase 4 B3; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"). The dial is read by Load into Config.M1VendorReachable; its consumer is
// internal/app's syncReachability, which writes it into platform.unit_costs through money.Store.SetUnitReachable (internal/app's reachability_test.go proves the value reaches that call, and that an
// invalid value is exit status 2 from app.Run). What is proved HERE is what the variable may say.

func TestM1VendorReachableDial_DefaultsToOffAndAcceptsExactlyTrueAndFalse(t *testing.T) {
	load := func(v *string) (config.Config, error) {
		extra := map[string]string{}
		if v != nil {
			extra[config.VarM1VendorReachable] = *v
		}
		return config.Load(lockEnv(extra))
	}
	// accepted: the default (unset, blank), and the two spellings, each beside its opposite; surrounding white space is trimmed like for every dial
	for _, c := range []struct {
		name string
		in   *string
		want bool
	}{
		{"unset", nil, false}, {"blank", ptrTo("   "), false}, {"false", ptrTo("false"), false}, {"true", ptrTo("true"), true}, {"true with white space", ptrTo(" true\t"), true},
	} {
		cfg, err := load(c.in)
		if err != nil || cfg.M1VendorReachable != c.want {
			t.Errorf("%s: loaded %v, %v; want %v", c.name, cfg.M1VendorReachable, err, c.want)
		}
	}
	// refused: exactly one problem naming the variable, saying what is accepted, never echoing the value. Every one of these is something an operator would plausibly type for "on"
	// (or "off"), so each is refused rather than read as off: a switch that silently read "yes" as false would reserve every customer at the wrong rate.
	for _, bad := range []string{"TRUE", "True", "FALSE", "False", "yes", "no", "1", "0", "on", "off", "enabled", "t", "tru", "true,", "true false", "maybe-XYZ123", "truee-XYZ123"} {
		cfg, err := load(&bad)
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarM1VendorReachable || !strings.Contains(ce.Problems[0].Reason, "must be true or false") {
			t.Errorf("%q: Load = %v, %v; want exactly one problem naming %s that says true or false", bad, cfg.M1VendorReachable, err, config.VarM1VendorReachable)
			continue
		}
		if len(bad) >= 5 && strings.Contains(err.Error(), bad) {
			t.Errorf("%q: the error echoes the value: %v", bad, err)
		}
		if cfg.M1VendorReachable {
			t.Errorf("%q: a refused value left the switch ON in the returned Config", bad)
		}
	}
	// the dial is a Var like the others: the default stands in Vars() as the text the table documents
	for _, v := range config.Vars() {
		if v.Name == config.VarM1VendorReachable && (v.Default != "false" || v.Required || v.Secret) {
			t.Errorf("Vars() declares %+v", v)
		}
	}
}

func ptrTo(s string) *string { return &s }

// A dial the owner must switch TOGETHER with a setting in another repository says so in the one place the owner reads it (the Meaning of its row in Docs/config-reference.md, which a test holds equal
// to Var.Doc): that it mirrors module 1's TM_EXTERNAL_* switches, what each value does to the reserve, that a job already confirmed is not moved, and that every instance must be given the same value.
// docMissing returns the facts a Meaning does not state; the control hands it Meanings with one fact taken out each.
func docMissing(doc string) []string {
	var missing []string
	for _, fact := range []string{
		"TM_EXTERNAL_", "BOTH", "OWNER-ACTIONS.md", "1 credit per row", "2 credits per row", "never charged", "never changes the reserve of a job already confirmed",
		"all instances must be given the same value", "exit status 2", "true` or `false` and nothing else",
	} {
		if !strings.Contains(doc, fact) {
			missing = append(missing, fact)
		}
	}
	return missing
}

func TestM1VendorReachableDial_TheMeaningSaysWhatTheOwnerMustKnow(t *testing.T) {
	var doc string
	for _, v := range config.Vars() {
		if v.Name == config.VarM1VendorReachable {
			doc = v.Doc
		}
	}
	if doc == "" {
		t.Fatalf("%s is not declared in Vars(): the check below would pass over nothing", config.VarM1VendorReachable)
	}
	if m := docMissing(doc); len(m) != 0 {
		t.Errorf("the Meaning of %s no longer says %q", config.VarM1VendorReachable, m)
	}
	// the control: a Meaning with one fact taken out is reported as missing exactly that fact
	for _, fact := range []string{"BOTH", "2 credits per row", "never changes the reserve of a job already confirmed", "all instances must be given the same value"} {
		if m := docMissing(strings.Replace(doc, fact, "...", 1)); len(m) != 1 || m[0] != fact {
			t.Errorf("a Meaning without %q was reported as missing %q", fact, m)
		}
	}
}
