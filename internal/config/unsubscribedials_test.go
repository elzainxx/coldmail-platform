package config_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
)

// The consumer of the three unsubscribe dials (Phase 5 S3; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"): PLATFORM_UNSUBSCRIBE_SECRET and PLATFORM_PUBLIC_BASE_URL
// are read by Load into Config and handed, by internal/app, to internal/unsubscribe (the signer and the link) and so to the job store; PLATFORM_UNSUBSCRIBE_RATE_PER_MIN to the public endpoint's
// limiter. internal/app's unsubscribe_test.go proves the wiring itself; the tests below prove the dials' own rules and that what they load is what the token code works from. The secret is built
// at run time from a repeated word (a long quoted literal next to the word "secret" is what a secret scanner flags).

func unsubSecret(word string, size int) string { return strings.Repeat(word, size/len(word)+1)[:size] }

func TestUnsubscribeDials_SecretNeedsAtLeast32PrintableBytesWhenSet(t *testing.T) {
	for _, c := range []struct{ name, value, reason string }{
		{"12 bytes", "short-XYZ123", "at least 32 bytes"},
		{"31 bytes", unsubSecret("a", 31), "at least 32 bytes"},
		{"31 bytes with white space around (trimmed first)", "  " + unsubSecret("b", 31) + "\n", "at least 32 bytes"},
		{"a space inside", unsubSecret("c", 20) + " " + unsubSecret("d", 20), "printable ASCII"},
		{"a newline inside", unsubSecret("c", 20) + "\n" + unsubSecret("d", 20), "printable ASCII"},
		{"a tab inside", unsubSecret("c", 20) + "\t" + unsubSecret("d", 20), "printable ASCII"},
		{"non-ASCII", strings.Repeat("é", 20), "printable ASCII"},
		{"a control character", unsubSecret("c", 40) + "\x01", "printable ASCII"},
	} {
		_, err := config.Load(lockEnv(map[string]string{config.VarUnsubscribeSecret: c.value}))
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarUnsubscribeSecret {
			t.Errorf("%s: Load = %v, want exactly one problem naming %s", c.name, err, config.VarUnsubscribeSecret)
			continue
		}
		if !strings.Contains(ce.Problems[0].Reason, c.reason) {
			t.Errorf("%s: the reason is %q, want it to say %q", c.name, ce.Problems[0].Reason, c.reason)
		}
		if v := strings.TrimSpace(c.value); len(v) >= 5 && strings.Contains(err.Error(), v) {
			t.Errorf("%s: the error echoes the secret", c.name)
		}
	}
	// the twins: the floor, far above it, trimmed, blank (unset) and the marker (unset)
	for _, c := range []struct{ name, value, want string }{
		{"exactly 32 bytes", unsubSecret("e", 32), unsubSecret("e", 32)},
		{"64 bytes", unsubSecret("f", 64), unsubSecret("f", 64)},
		{"trimmed", "  " + unsubSecret("g", 32) + "\n", unsubSecret("g", 32)},
		{"blank means unset", "   ", ""},
		{"the marker means unset", "(unset)", ""},
	} {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarUnsubscribeSecret: c.value}))
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		if cfg.UnsubscribeSecret.Reveal() != c.want {
			t.Errorf("%s: loaded a secret of %d bytes, want %d", c.name, len(cfg.UnsubscribeSecret.Reveal()), len(c.want))
		}
	}
	if cfg, err := config.Load(lockEnv(nil)); err != nil || cfg.UnsubscribeSecret != "" {
		t.Fatalf("an environment without the dial did not load to an empty secret: %v (%v)", cfg, err)
	}
	if config.MinUnsubscribeSecretBytes != unsubscribe.MinSecretBytes {
		t.Fatalf("the dial's floor is %d bytes and the token code's is %d: they must be one number", config.MinUnsubscribeSecretBytes, unsubscribe.MinSecretBytes)
	}
}

// baseOfLen is a base URL of exactly n characters whose host labels are all valid (so a refusal of it is about its length alone).
func baseOfLen(n int) string {
	m := 1
	if (n-8)%2 == 0 {
		m = 2
	}
	return "https://" + strings.Repeat("a.", (n-8-m)/2) + strings.Repeat("c", m)
}

func TestUnsubscribeDials_PublicBaseURLShapes(t *testing.T) {
	accepted := map[string]string{ // value -> what it loads as
		"https://app.example.com":                         "https://app.example.com",
		"https://app.example.com/":                        "https://app.example.com",
		"https://app.example.com///":                      "https://app.example.com",
		"  https://app.example.com  ":                     "https://app.example.com",
		"https://app.example.com:8443":                    "https://app.example.com:8443",
		"https://app.example.com:443/":                    "https://app.example.com:443",
		"https://app.example.com/mail":                    "https://app.example.com/mail",
		"https://app.example.com/mail/v1.2/a_b~c-d/":      "https://app.example.com/mail/v1.2/a_b~c-d",
		"https://xn--bcher-kva.example":                   "https://xn--bcher-kva.example",
		"https://localhost":                               "https://localhost",
		"https://App.Example.COM":                         "https://App.Example.COM",
		"https://10.0.0.5":                                "https://10.0.0.5",
		"https://" + strings.Repeat("a", 63) + ".example": "https://" + strings.Repeat("a", 63) + ".example",
	}
	rejected := map[string]string{ // value -> the shape it breaks
		"http://app.example.com":                          "scheme http",
		"HTTPS://app.example.com":                         "scheme not in lower case",
		"app.example.com":                                 "no scheme",
		"//app.example.com":                               "no scheme",
		"ftp://app.example.com":                           "scheme ftp",
		"https://":                                        "no host",
		"https:///path":                                   "no host",
		"https://user@app.example.com":                    "a user name",
		"https://user:pw@app.example.com":                 "credentials",
		"https://app.example.com?a=b":                     "a query",
		"https://app.example.com/?":                       "an empty query",
		"https://app.example.com#f":                       "a fragment",
		"https://app.example.com/a?b":                     "a query after a path",
		"https://app.example.com/a b":                     "a space in the path",
		"https://app.example .com":                        "a space in the host",
		"https://app.example.com/<x>":                     "angle brackets",
		"https://app.example.com/\"x\"":                   "a double quote",
		"https://app.example.com/'x'":                     "a single quote",
		"https://app.example.com/a\\b":                    "a backslash",
		"https://app.example.com/%41":                     "a percent escape",
		"https://app.example.com//a":                      "an empty path segment",
		"https://app.example.com/a//b":                    "an empty path segment",
		"https://app.example.com/../x":                    "a dot-dot segment",
		"https://app.example.com/./x":                     "a dot segment",
		"https://app.example.com:0":                       "port 0",
		"https://app.example.com:65536":                   "port out of range",
		"https://app.example.com:80a":                     "a port that is not a number",
		"https://app.example.com:":                        "an empty port",
		"https://app.example.com:0443":                    "a port with a leading zero",
		"https://-app.example.com":                        "a label starting with a hyphen",
		"https://app-.example.com":                        "a label ending with a hyphen",
		"https://app..example.com":                        "an empty label",
		"https://.example.com":                            "a leading dot",
		"https://" + strings.Repeat("a", 64) + ".example": "a label of 64 bytes",
		"https://bücher.example":                          "a non-ASCII host (use the punycode form)",
		"https://[::1]":                                   "an IPv6 literal",
		"https://app_example.com":                         "an underscore in the host",
		"https://app.example.com/\x7f":                    "a control character",
		"https://app.example.com/é":                       "a non-ASCII path",
		"mailto:ops@example.com":                          "an opaque URL",
		baseOfLen(501):                                    "over 500 characters",
	}
	for v, want := range accepted {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarPublicBaseURL: v}))
		if err != nil || cfg.PublicBaseURL != want {
			t.Errorf("%q: loaded as %q (%v), want %q", v, cfg.PublicBaseURL, err, want)
		}
	}
	for v, why := range rejected {
		_, err := config.Load(lockEnv(map[string]string{config.VarPublicBaseURL: v}))
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarPublicBaseURL {
			t.Errorf("%q (%s) loaded, or was refused for another variable: %v", v, why, err)
			continue
		}
		if !strings.Contains(ce.Problems[0].Reason, "https URL") {
			t.Errorf("%q (%s): the reason is %q, want it to say what a public base URL is", v, why, ce.Problems[0].Reason)
		}
	}
	// a rejected value is never echoed: values that carry a canary the reason cannot contain by itself
	for _, v := range []string{"http://user-XYZ123:pw-XYZ123@host-XYZ123.example", "https://host-XYZ123.example/?token=XYZ123", "https://host-XYZ123.example/a b"} {
		_, err := config.Load(lockEnv(map[string]string{config.VarPublicBaseURL: v}))
		if err == nil || strings.Contains(err.Error(), "XYZ123") {
			t.Errorf("%q: refused=%v, and the error must not echo the value: %v", v, err != nil, err)
		}
	}
	// the boundary: exactly 500 characters is accepted, 501 is not (the twin of the "over 500" row above; every label is valid, so only the length is in question)
	for _, c := range []struct {
		n    int
		want bool
	}{{499, true}, {500, true}, {501, false}, {800, false}} {
		v := baseOfLen(c.n)
		if len(v) != c.n {
			t.Fatalf("built a value of %d characters, wanted %d", len(v), c.n)
		}
		_, err := config.Load(lockEnv(map[string]string{config.VarPublicBaseURL: v}))
		if (err == nil) != c.want {
			t.Errorf("a base URL of %d characters: accepted=%v, want %v (%v)", c.n, err == nil, c.want, err)
		}
	}
	if len(accepted) < 12 || len(rejected) < 40 {
		t.Fatal("the shape tables shrank: the check would pass over fewer cases than it was written for")
	}
	if cfg, err := config.Load(lockEnv(nil)); err != nil || cfg.PublicBaseURL != "" {
		t.Fatalf("an environment without the dial did not load to an empty URL: %v (%v)", cfg, err)
	}
	for _, unset := range []string{"", "   ", "(unset)"} {
		if cfg, err := config.Load(lockEnv(map[string]string{config.VarPublicBaseURL: unset})); err != nil || cfg.PublicBaseURL != "" {
			t.Errorf("%q should mean unset: %q (%v)", unset, cfg.PublicBaseURL, err)
		}
	}
}

func TestUnsubscribeDials_RateRangeAndDefault(t *testing.T) {
	cfg, err := config.Load(lockEnv(nil))
	if err != nil || cfg.UnsubscribeRatePerMin != 30 {
		t.Fatalf("the default is %d (%v), want 30", cfg.UnsubscribeRatePerMin, err)
	}
	for _, c := range []struct{ value, reason string }{
		{"0", "between 1 and 600"}, {"-1", "between 1 and 600"}, {"601", "between 1 and 600"},
		{"often-XYZ123", "integer"}, {"1.5", "integer"}, {"99999999999999999999", "integer"},
	} {
		_, err := config.Load(lockEnv(map[string]string{config.VarUnsubscribeRatePerMin: c.value}))
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarUnsubscribeRatePerMin || !strings.Contains(ce.Problems[0].Reason, c.reason) {
			t.Errorf("%q: Load = %v, want one problem naming the dial and saying %q", c.value, err, c.reason)
		}
	}
	// the twins: both edges
	for value, want := range map[string]int{"1": 1, "30": 30, "600": 600, " 45 ": 45} {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarUnsubscribeRatePerMin: value}))
		if err != nil || cfg.UnsubscribeRatePerMin != want {
			t.Errorf("%q loaded as %d (%v), want %d", value, cfg.UnsubscribeRatePerMin, err, want)
		}
	}
}

// TestUnsubscribeDials_ReachTheTokenCode is the consumer proof: what Load reads is what the token code signs with and what the link starts with, so another value of either dial gives another
// link, and an independent computation of the token from the same values agrees.
func TestUnsubscribeDials_ReachTheTokenCode(t *testing.T) {
	const item = "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa"
	independent := func(secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("unsub|v1|" + item))
		raw := []byte{0x0f, 0x6c, 0x1a, 0x52, 0x9b, 0x7e, 0x4d, 0x1c, 0x8a, 0x35, 0x2e, 0x7b, 0x94, 0xc0, 0xd1, 0xaa}
		return base64.RawURLEncoding.EncodeToString(append(raw, mac.Sum(nil)[:24]...))
	}
	linkOf := func(env map[string]string) string {
		cfg, err := config.Load(lockEnv(env))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		s, err := unsubscribe.NewSigner([]byte(cfg.UnsubscribeSecret.Reveal()))
		if err != nil {
			t.Fatalf("NewSigner: %v", err)
		}
		link, err := s.LinkMaker(cfg.PublicBaseURL)(item)
		if err != nil {
			t.Fatal(err)
		}
		return link
	}
	one, two := unsubSecret("one-", 40), unsubSecret("two-", 40)
	l1 := linkOf(map[string]string{config.VarUnsubscribeSecret: one, config.VarPublicBaseURL: "https://app.example.test/"})
	if want := "https://app.example.test/u/" + independent(one); l1 != want {
		t.Fatalf("the link built from the dials is not the independent computation of the same values: %q vs %q", l1, want)
	}
	if l2 := linkOf(map[string]string{config.VarUnsubscribeSecret: two, config.VarPublicBaseURL: "https://app.example.test/"}); l2 == l1 || l2 != "https://app.example.test/u/"+independent(two) {
		t.Fatalf("another secret must give another link, the independent computation of that secret: %q", l2)
	}
	if l3 := linkOf(map[string]string{config.VarUnsubscribeSecret: one, config.VarPublicBaseURL: "https://other.example.test"}); l3 == l1 || !strings.HasPrefix(l3, "https://other.example.test/u/") {
		t.Fatalf("another base URL must give another link: %q", l3)
	}
}

// TestUnsubscribeDials_SecretIsNeverPrinted: no way the standard library prints a Config, a Secret or a log record shows the value, and an unset dial says so without one. The control is a plain
// string field, which leaks through the same sinks.
func TestUnsubscribeDials_SecretIsNeverPrinted(t *testing.T) {
	canary := "CANARY-unsub-" + unsubSecret("z", 40)
	cfg, err := config.Load(lockEnv(map[string]string{config.VarUnsubscribeSecret: canary, config.VarPublicBaseURL: "https://app.example.test"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UnsubscribeSecret.Reveal() != canary {
		t.Fatal("control failed: Reveal() does not return the loaded value")
	}
	sinks := map[string]string{"String": cfg.String(), "GoString": cfg.GoString(), "Sprintln": fmt.Sprintln(cfg, &cfg)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		sinks["Config "+verb] = fmt.Sprintf(verb, cfg)
		sinks["Secret "+verb] = fmt.Sprintf(verb, cfg.UnsubscribeSecret)
	}
	for _, v := range []any{cfg, &cfg, cfg.UnsubscribeSecret} {
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
		slog.New(handler).Info("loaded", "config", cfg, "secret", cfg.UnsubscribeSecret)
		sinks["slog "+h] = buf.String()
	}
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		if strings.Contains(out, canary) || strings.Contains(out, canary[len("CANARY-"):]) {
			t.Errorf("%s reveals the unsubscribe secret: %s", name, out)
		}
	}
	if s := cfg.String(); !strings.Contains(s, "unsubscribe_secret=[redacted]") || !strings.Contains(s, "public_base_url=https://app.example.test") || !strings.Contains(s, "unsubscribe_rate_per_min=30") {
		t.Errorf("a set secret is not shown as the redaction marker, or the other two dials are not shown: %s", s)
	}
	unset, err := config.Load(lockEnv(nil))
	if err != nil || !strings.Contains(unset.String(), "unsubscribe_secret=(unset)") || !strings.Contains(unset.String(), "public_base_url=(unset)") {
		t.Errorf("an unset dial is not shown as (unset): %v (%v)", unset.String(), err)
	}
	type naive struct{ UnsubscribeSecret string }
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "config", naive{canary})
	if !strings.Contains(buf.String(), canary) || !strings.Contains(fmt.Sprintf("%+v", naive{canary}), canary) {
		t.Fatal("control failed: a plain string field did not reach the sinks, so the checks above prove nothing")
	}
}

// TestUnsubscribeDials_InvalidValueStopsTheProcessWithExitStatus2 is the entry point's own check, not only Load's. The controls are accepted values of the same variables.
func TestUnsubscribeDials_InvalidValueStopsTheProcessWithExitStatus2(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"a short secret":      {config.VarUnsubscribeSecret: unsubSecret("h", 31)},
		"an http public URL":  {config.VarPublicBaseURL: "http://app.example.test"},
		"a rate of zero":      {config.VarUnsubscribeRatePerMin: "0"},
		"a URL with a query":  {config.VarPublicBaseURL: "https://app.example.test?x=1"},
		"a secret with space": {config.VarUnsubscribeSecret: unsubSecret("h", 20) + " " + unsubSecret("i", 20)},
	} {
		err := app.Run(context.Background(), lockEnv(env), io.Discard)
		var ce *config.Error
		if !errors.As(err, &ce) {
			t.Errorf("%s: Run = %v, want a *config.Error", name, err)
			continue
		}
		if got := app.ExitCode(err); got != 2 {
			t.Errorf("%s: exit status %d, want 2", name, got)
		}
		for _, v := range env {
			if len(v) >= 12 && strings.Contains(err.Error(), v) {
				t.Errorf("%s: the error echoes the value", name)
			}
		}
	}
	for _, env := range []map[string]string{
		{config.VarUnsubscribeSecret: unsubSecret("j", 32)}, {config.VarPublicBaseURL: "https://app.example.test"}, {config.VarUnsubscribeRatePerMin: "600"}, {config.VarUnsubscribeSecret: "(unset)"},
	} {
		if _, err := config.Load(lockEnv(env)); err != nil {
			t.Errorf("an accepted value was refused: %v", err)
		}
	}
}
