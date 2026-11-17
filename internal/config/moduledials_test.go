package config_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/config"
)

// The four optional module dials (Phase 3): PLATFORM_M1_URL, PLATFORM_M2_URL, PLATFORM_M2_SERVICE_TOKEN and PLATFORM_M3_SERVICE_TOKEN.
//
// Their consumers are the FromConfig functions of internal/adapters/m1, m2 and m3, which are written beside this change; the service builds no adapter yet. Until each adapter exists, the proof
// that a dial is read, validated and changes something is here: the loader reads it (TestLoad_EveryVarIsReadAndNothingElseIs), changing it changes the Config
// (TestLoad_EveryVarChangesTheConfig), and each field carries ITS variable and no other one (below). The proof that an adapter reads the field it is documented to read belongs in that adapter's
// own package, as internal/modclient/consumer_test.go does for the module client's dials.
//
// Tokens are made at run time and never printed: a failure message names the variable and the length, not the value.

// dialValue returns n random bytes as hexadecimal text.
func dialValue(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

var moduleDialNames = []string{config.VarM1URL, config.VarM2URL, config.VarM2ServiceToken, config.VarM3ServiceToken}

// fieldOf names the Config field of each dial and returns its value as text, for the cross-checks below.
func fieldOf(cfg config.Config, name string) string {
	switch name {
	case config.VarM1URL:
		return cfg.M1URL
	case config.VarM2URL:
		return cfg.M2URL
	case config.VarM2ServiceToken:
		return cfg.M2ServiceToken.Reveal()
	case config.VarM3ServiceToken:
		return cfg.M3ServiceToken.Reveal()
	}
	return "<not a module dial: " + name + ">"
}

// differingFields lists the Config fields whose values differ, by name: the instrument that says which field a variable reached.
func differingFields(a, b config.Config) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := 0; i < va.NumField(); i++ {
		if va.Field(i).Interface() != vb.Field(i).Interface() {
			out = append(out, va.Type().Field(i).Name)
		}
	}
	return out
}

// None of the four is required: a deployment that calls no module starts without them, and each of the unset spellings loads to an empty field, never to the marker text and never to an
// error. The twin of each is the dial set to a value, which loads to that value, so the empty field is the dial's absence and not a field that cannot be set.
func TestModuleDials_AreOptionalAndUnsetLoadsEmpty(t *testing.T) {
	good := map[string]string{
		config.VarM1URL:          "http://m1.example:8081",
		config.VarM2URL:          "https://m2.example:9444/api",
		config.VarM2ServiceToken: dialValue(t, 16),
		config.VarM3ServiceToken: dialValue(t, 16),
	}
	for _, name := range moduleDialNames {
		for label, value := range map[string]string{"unset": "", "blank": "   ", "the marker": "(unset)", "the marker with white space": "  (unset)\t"} {
			env := map[string]string{}
			if label != "unset" {
				env[name] = value
			}
			cfg, err := config.Load(lockEnv(env))
			if err != nil {
				t.Errorf("%s %s: refused: %v", name, label, err)
				continue
			}
			if got := fieldOf(cfg, name); got != "" {
				t.Errorf("%s %s: loaded as a value of %d bytes, want an empty field (not configured)", name, label, len(got))
			}
		}
		cfg, err := config.Load(lockEnv(map[string]string{name: good[name]}))
		if err != nil || fieldOf(cfg, name) != good[name] {
			t.Errorf("%s: a value was refused or altered (the control of the empty field): %v", name, err)
		}
	}
	// And none of the four is declared required, whatever the loader happens to do with an empty environment.
	for _, v := range config.Vars() {
		for _, name := range moduleDialNames {
			if v.Name == name && v.Required {
				t.Errorf("%s is declared required: the service must start without it", name)
			}
		}
	}
}

// Each variable reaches its own field and no other: changing one dial changes exactly one Config field, the one it is documented to fill. This is the check that catches a copy-paste of a
// parse branch (M2's token stored in M3's field, or the service token stored in the admin token's field), which the generic "every dial changes the Config" check cannot see because both
// would still change something.
func TestModuleDials_EachVariableReachesItsOwnFieldAndNoOther(t *testing.T) {
	want := map[string]string{
		config.VarM1URL:          "M1URL",
		config.VarM2URL:          "M2URL",
		config.VarM2ServiceToken: "M2ServiceToken",
		config.VarM3ServiceToken: "M3ServiceToken",
	}
	values := map[string]string{
		config.VarM1URL:          "http://m1.example:8081",
		config.VarM2URL:          "http://m2.example:8082",
		config.VarM2ServiceToken: dialValue(t, 16),
		config.VarM3ServiceToken: dialValue(t, 16),
	}
	baseline, err := config.Load(lockEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range moduleDialNames {
		cfg, err := config.Load(lockEnv(map[string]string{name: values[name]}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if diff := differingFields(baseline, cfg); !reflect.DeepEqual(diff, []string{want[name]}) {
			t.Errorf("%s changed the Config fields %v, want exactly [%s]", name, diff, want[name])
		}
		if fieldOf(cfg, name) != values[name] {
			t.Errorf("%s: the field %s does not hold the value that was set", name, want[name])
		}
	}

	// All four at once, with the module 3 operator key beside them: four distinct values land in four distinct fields, and the service token is not the operator key's field.
	admin := dialValue(t, 16)
	cfg, err := config.Load(lockEnv(map[string]string{
		config.VarM1URL: values[config.VarM1URL], config.VarM2URL: values[config.VarM2URL],
		config.VarM2ServiceToken: values[config.VarM2ServiceToken], config.VarM3ServiceToken: values[config.VarM3ServiceToken],
		config.VarM3AdminToken: admin,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.M3ServiceToken.Reveal() != values[config.VarM3ServiceToken] || cfg.M3AdminToken.Reveal() != admin || cfg.M3ServiceToken == cfg.M3AdminToken {
		t.Error("the module 3 service token and the module 3 admin token did not stay two different credentials in two different fields")
	}
	if cfg.M2ServiceToken.Reveal() != values[config.VarM2ServiceToken] || cfg.M2ServiceToken == cfg.M3ServiceToken {
		t.Error("the module 2 and module 3 service tokens did not land in their own fields")
	}

	// The control: the same instrument reports a mix-up. Swapping the two tokens' values gives a Config in which the fields hold the other module's token, and differingFields sees both.
	swapped, err := config.Load(lockEnv(map[string]string{
		config.VarM2ServiceToken: values[config.VarM3ServiceToken], config.VarM3ServiceToken: values[config.VarM2ServiceToken],
	}))
	if err != nil {
		t.Fatal(err)
	}
	if diff := differingFields(baseline, swapped); !reflect.DeepEqual(diff, []string{"M2ServiceToken", "M3ServiceToken"}) {
		t.Fatalf("control failed: the instrument reported %v for two tokens swapped, want both fields", diff)
	}
	if swapped.M2ServiceToken.Reveal() == values[config.VarM2ServiceToken] {
		t.Fatal("control failed: a swapped environment loaded as if it were not swapped")
	}
}

// A value is trimmed like every other dial's (an environment file or an orchestrator often leaves a trailing newline), and the trimmed value is what the adapter will send.
func TestModuleDials_ValuesAreTrimmed(t *testing.T) {
	tok := dialValue(t, 12)
	cfg, err := config.Load(lockEnv(map[string]string{
		config.VarM1URL: "  http://m1.example:8081\n", config.VarM2URL: "\thttps://m2.example/", config.VarM2ServiceToken: tok + "\n", config.VarM3ServiceToken: "  " + tok,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.M1URL != "http://m1.example:8081" || cfg.M2URL != "https://m2.example/" || cfg.M2ServiceToken.Reveal() != tok || cfg.M3ServiceToken.Reveal() != tok {
		t.Errorf("the values were not trimmed: %v (lengths %d %d)", cfg, len(cfg.M2ServiceToken.Reveal()), len(cfg.M3ServiceToken.Reveal()))
	}
	// The control: white space INSIDE a token is not trimmed, it is refused (a trimmed-away inner space would silently send another credential).
	_, err = config.Load(lockEnv(map[string]string{config.VarM2ServiceToken: tok[:6] + " " + tok[6:]}))
	var ce *config.Error
	if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarM2ServiceToken || !strings.Contains(ce.Problems[0].Reason, "printable ASCII") {
		t.Fatalf("a token with an inner space: Load = %v, want one problem naming %s", err, config.VarM2ServiceToken)
	}
}

// The tokens are printed nowhere: no verb of fmt, no encoder, no log record shows either of them, in the Config or on the field. The URLs are not secrets and are shown (they carry no
// credentials, which the validation guarantees), and an unset dial says so without a value. The control is a plain string field of the same shape, which leaks through the same sinks.
func TestModuleDials_TokensAreNeverPrinted(t *testing.T) {
	m2, m3 := "CANARY-m2tok-"+dialValue(t, 12), "CANARY-m3tok-"+dialValue(t, 12)
	cfg, err := config.Load(lockEnv(map[string]string{
		config.VarM1URL: "http://m1.example:8081", config.VarM2URL: "http://m2.example:8082",
		config.VarM2ServiceToken: m2, config.VarM3ServiceToken: m3,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Paired control 1: the values really are in the Config, behind Reveal.
	if cfg.M2ServiceToken.Reveal() != m2 || cfg.M3ServiceToken.Reveal() != m3 {
		t.Fatal("control failed: Reveal() does not return the loaded values")
	}
	sinks := map[string]string{
		"String":   cfg.String(),
		"GoString": cfg.GoString(),
		"Sprintln": fmt.Sprintln(cfg, &cfg),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		sinks["Config "+verb] = fmt.Sprintf(verb, cfg)
		sinks["*Config "+verb] = fmt.Sprintf(verb, &cfg)
		sinks["M2 token "+verb] = fmt.Sprintf(verb, cfg.M2ServiceToken)
		sinks["M3 token "+verb] = fmt.Sprintf(verb, cfg.M3ServiceToken)
		sinks["struct{Secret} "+verb] = fmt.Sprintf(verb, struct{ A, B config.Secret }{cfg.M2ServiceToken, cfg.M3ServiceToken})
	}
	for _, v := range []any{cfg, &cfg, cfg.M2ServiceToken, cfg.M3ServiceToken} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sinks[fmt.Sprintf("json %T", v)] = string(b)
	}
	for _, h := range []string{"json", "text"} {
		var buf bytes.Buffer
		var handler slog.Handler = slog.NewJSONHandler(&buf, nil)
		if h == "text" {
			handler = slog.NewTextHandler(&buf, nil)
		}
		l := slog.New(handler)
		l.Info("loaded", "config", cfg, "m2", cfg.M2ServiceToken, "m3", cfg.M3ServiceToken)
		l.Info("pointer", "config", &cfg)
		sinks["slog "+h] = buf.String()
	}
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		for _, canary := range []string{m2, m3, m2[len("CANARY-"):], m3[len("CANARY-"):]} {
			if strings.Contains(out, canary) {
				t.Errorf("%s reveals a module service token", name)
			}
		}
	}
	// What the printed configuration does say: the tokens as the marker, the URLs as they are.
	s := cfg.String()
	for _, want := range []string{"m2_service_token=[redacted]", "m3_service_token=[redacted]", "m1_url=http://m1.example:8081", "m2_url=http://m2.example:8082"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() does not contain %q: %s", want, s)
		}
	}
	unset, err := config.Load(lockEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"m2_service_token=(unset)", "m3_service_token=(unset)", "m1_url=(unset)", "m2_url=(unset)"} {
		if !strings.Contains(unset.String(), want) {
			t.Errorf("an unset configuration does not say %q: %s", want, unset.String())
		}
	}
	// Paired control 2: a plain string field of the same shape DOES leak through the same sinks.
	type naive struct{ M2ServiceToken, M3ServiceToken string }
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "config", naive{m2, m3})
	leaky := map[string]string{"slog": buf.String(), "fmt %+v": fmt.Sprintf("%+v", naive{m2, m3})}
	for _, verb := range []string{"%d"} { // %d is the verb a Stringer does not intercept (a variable verb keeps vet from refusing the call)
		leaky["fmt "+verb] = fmt.Sprintf(verb, naive{m2, m3})
	}
	for name, out := range leaky {
		if !strings.Contains(out, m2) || !strings.Contains(out, m3) {
			t.Errorf("control failed: %s of a plain string field did not contain both tokens, so the checks above prove nothing", name)
		}
	}
}

// A rejected value is named by its variable and never echoed: not a token, not the password of a URL with credentials. Each case's error is non-empty and names the variable, so the absence of
// the canary is not an artefact of an empty error; the twin of each is the same canary in a form that loads.
func TestModuleDials_ErrorsNeverContainTheValue(t *testing.T) {
	canary := dialValue(t, 12)
	cases := []struct {
		name, bad, good, reason string
	}{
		{config.VarM1URL, "http://operator:" + canary + "@m1:8080", "http://m1:8080/" + canary, "no credentials"},
		{config.VarM2URL, "m2-" + canary + ":8080", "http://m2-" + canary + ":8080", "http or https URL"},
		{config.VarM2ServiceToken, "two words-" + canary, "two-words-" + canary, "printable ASCII"},
		{config.VarM3ServiceToken, "café-" + canary, "cafe-" + canary, "printable ASCII"},
	}
	for _, c := range cases {
		_, err := config.Load(lockEnv(map[string]string{c.name: c.bad}))
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != c.name {
			t.Errorf("%s: Load = %v, want exactly one problem naming the variable", c.name, err)
			continue
		}
		if !strings.Contains(ce.Problems[0].Reason, c.reason) || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s: the reason %q does not say %q, or the error does not name the variable: %v", c.name, ce.Problems[0].Reason, c.reason, err)
		}
		if strings.Contains(err.Error(), canary) || strings.Contains(fmt.Sprintf("%+v %#v %q", ce, ce, err), canary) {
			t.Errorf("%s: the error echoes the rejected value", c.name)
		}
		if _, err := config.Load(lockEnv(map[string]string{c.name: c.good})); err != nil {
			t.Errorf("%s: the control value (the same canary, in a form that is valid) was refused: %v", c.name, err)
		}
	}
	// All four invalid at once: four problems, in declaration order, and still no value.
	all := map[string]string{}
	for _, c := range cases {
		all[c.name] = c.bad
	}
	_, err := config.Load(lockEnv(all))
	var ce *config.Error
	if !errors.As(err, &ce) || len(ce.Problems) != 4 || strings.Contains(err.Error(), canary) {
		t.Fatalf("four invalid dials: Load = %v, want four problems and no value in the text", err)
	}
	var names []string
	for _, p := range ce.Problems {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{config.VarM1URL, config.VarM2URL, config.VarM2ServiceToken, config.VarM3ServiceToken}) {
		t.Errorf("the problems named %v, want the four dials in declaration order", names)
	}
}

// An invalid value stops the process at start-up with exit status 2, naming the variable and the reason, never the value (the entry point's own check, not only Load's). The controls are the
// valid values of the same variables, which the loader accepts.
func TestModuleDials_InvalidValuesStopTheProcessAtStartupWithExitStatus2(t *testing.T) {
	bad := map[string][]string{
		config.VarM1URL:          {"ftp://m1-XYZ123:21", "m1-XYZ123:8080", "https://", "http://u-XYZ123:p-XYZ123@m1:8080", "http://m1-XYZ123:8080?a=b"},
		config.VarM2URL:          {"ftp://m2-XYZ123:21", "m2-XYZ123:8080", "https://", "http://u-XYZ123:p-XYZ123@m2:8080", "http://m2-XYZ123:8080#f"},
		config.VarM2ServiceToken: {"two words-XYZ123", "café-XYZ123", "tab\there-XYZ123"},
		config.VarM3ServiceToken: {"two words-XYZ123", "café-XYZ123", "tab\there-XYZ123"},
	}
	n := 0
	for name, values := range bad {
		for _, v := range values {
			n++
			// Load first: if a broken loader let the value through, app.Run would go on to start the real service (a database, a listener) and the test would hang instead of failing.
			if _, err := config.Load(lockEnv(map[string]string{name: v})); err == nil {
				t.Errorf("%s: a value that must be refused was accepted by Load (%d bytes)", name, len(v))
				continue
			}
			err := app.Run(context.Background(), lockEnv(map[string]string{name: v}), io.Discard)
			var ce *config.Error
			if !errors.As(err, &ce) {
				t.Errorf("%s: Run = %v, want a *config.Error", name, err)
				continue
			}
			if got := app.ExitCode(err); got != 2 {
				t.Errorf("%s: exit status %d, want 2", name, got)
			}
			if len(ce.Problems) != 1 || ce.Problems[0].Name != name {
				t.Errorf("%s: problems %+v, want exactly one, naming the variable", name, ce.Problems)
			}
			if strings.Contains(err.Error(), "XYZ123") {
				t.Errorf("%s: the error echoes the value: %v", name, err)
			}
		}
	}
	if n < 16 {
		t.Fatalf("only %d invalid values were tried", n)
	}
	for name, ok := range map[string][]string{
		config.VarM1URL:          {"http://m1:8080", "https://m1.internal:8443/base", "  ", "(unset)"},
		config.VarM2URL:          {"http://m2:8080", "https://m2.internal:9443", "  ", "(unset)"},
		config.VarM2ServiceToken: {dialValue(t, 16), "  ", "(unset)"},
		config.VarM3ServiceToken: {dialValue(t, 16), "  ", "(unset)"},
	} {
		for _, v := range ok {
			if _, err := config.Load(lockEnv(map[string]string{name: v})); err != nil {
				t.Errorf("%s: a valid value (%d bytes) was refused: %v", name, len(v), err)
			}
		}
	}
}
