package config

import (
	"errors"
	"strings"
	"testing"
)

// The web app's three dials (Phase 6, chunk A2; web.go): PLATFORM_WEB_DIR switches the serving of the built app on, and with it PLATFORM_PUBLIC_SUPABASE_URL and PLATFORM_PUBLIC_SUPABASE_ANON_KEY are required.
// internal/webapp's own tests and internal/app's prove what the service does with them; these prove the dials' own rules. The anon key is a PUBLIC key by design, and every value below that looks like a key
// is built at run time or is a plain word (a long quoted literal next to the word "key" is what a secret scanner flags).

const (
	webURL = "https://abcdefgh.supabase.example"
	webKey = "anon-anon-anon-anon"
)

func problemsOf(t *testing.T, err error) []Problem {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("Load = %v, want *Error", err)
	}
	return ce.Problems
}

func TestWebDials_AreOptionalNotSecretAndOffByDefault(t *testing.T) {
	byName := map[string]Var{}
	for _, v := range Vars() {
		byName[v.Name] = v
	}
	for _, name := range []string{VarWebDir, VarPublicSupabaseURL, VarPublicSupabaseAnonKey} {
		v, ok := byName[name]
		if !ok || v.Required || v.Secret || v.Default != "(unset)" {
			t.Errorf("%s must be declared optional, not secret, with the default (unset), got %+v", name, v)
		}
	}
	// the control the loop above stands on: a dial that IS required or secret fails the same predicate
	if v := byName[VarDatabaseURL]; v.Required && v.Secret && v.Default == "(unset)" {
		t.Fatal("control failed: the predicate cannot tell a required secret from an optional public dial")
	}

	cfg, err := Load(envOf(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDir != "" || cfg.PublicSupabaseURL != "" || cfg.PublicSupabaseAnonKey != "" {
		t.Fatalf("the app is off by default, but the Config holds %q, %q, %q", cfg.WebDir, cfg.PublicSupabaseURL, cfg.PublicSupabaseAnonKey)
	}
	// the marker and blank values mean unset, exactly as for every other optional dial
	for _, raw := range []string{"(unset)", "  ", "\n"} {
		env := with(with(with(base(), VarWebDir, raw), VarPublicSupabaseURL, raw), VarPublicSupabaseAnonKey, raw)
		if again, err := Load(envOf(env)); err != nil || again != cfg {
			t.Errorf("%q loaded to %v (%v), want the same Config as the unset environment", raw, again, err)
		}
	}
}

func TestWebDials_TheAppCannotBeServedWithoutWhatTheBrowserNeedsToSignIn(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string // the dials the problems name, in order
	}{
		{"neither", with(base(), VarWebDir, "/srv/web"), []string{VarPublicSupabaseURL, VarPublicSupabaseAnonKey}},
		{"only the address", with(with(base(), VarWebDir, "/srv/web"), VarPublicSupabaseURL, webURL), []string{VarPublicSupabaseAnonKey}},
		{"only the key", with(with(base(), VarWebDir, "/srv/web"), VarPublicSupabaseAnonKey, webKey), []string{VarPublicSupabaseURL}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(envOf(c.env))
			ps := problemsOf(t, err)
			var got []string
			for _, p := range ps {
				got = append(got, p.Name)
				if !strings.Contains(p.Reason, VarWebDir) || !strings.Contains(p.Reason, "required") {
					t.Errorf("the reason for %s is %q, want it to say it is required when %s is set", p.Name, p.Reason, VarWebDir)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("problems name %v, want %v", got, c.want)
			}
			if !strings.Contains(err.Error(), c.want[0]) {
				t.Errorf("the message does not name the dial: %v", err)
			}
			if strings.Contains(err.Error(), "/srv/web") || strings.Contains(err.Error(), webKey) || strings.Contains(err.Error(), webURL) {
				t.Errorf("the message echoes a value: %v", err)
			}
		})
	}

	// the twin that differs in exactly this: with both dials the same directory loads, and the values arrive
	cfg, err := Load(envOf(with(with(with(base(), VarWebDir, "/srv/web"), VarPublicSupabaseURL, webURL+"//"), VarPublicSupabaseAnonKey, webKey)))
	if err != nil {
		t.Fatalf("the complete set was refused: %v", err)
	}
	if cfg.WebDir != "/srv/web" || cfg.PublicSupabaseURL != webURL || cfg.PublicSupabaseAnonKey != webKey {
		t.Fatalf("the complete set loaded as %q, %q, %q", cfg.WebDir, cfg.PublicSupabaseURL, cfg.PublicSupabaseAnonKey)
	}
	// and the other direction is not a problem: the two public dials alone describe a project and serve nothing
	only, err := Load(envOf(with(with(base(), VarPublicSupabaseURL, webURL), VarPublicSupabaseAnonKey, webKey)))
	if err != nil || only.WebDir != "" || only.PublicSupabaseURL != webURL {
		t.Fatalf("the two public dials without PLATFORM_WEB_DIR: %v (%v)", only, err)
	}
	// a dial that is invalid is reported as invalid, and the cross-check does not pile a second problem on it
	_, err = Load(envOf(with(with(with(base(), VarWebDir, "/srv/web"), VarPublicSupabaseURL, "ftp://x.example"), VarPublicSupabaseAnonKey, webKey)))
	if ps := problemsOf(t, err); len(ps) != 1 || ps[0].Name != VarPublicSupabaseURL || strings.Contains(ps[0].Reason, "required") {
		t.Fatalf("an invalid address with the app switched on gave %+v, want one problem that says it is invalid", ps)
	}
}

func TestWebDials_ValueRules(t *testing.T) {
	long := strings.Repeat("a", maxWebDirBytes+1)
	for _, c := range []struct {
		name, dial, value, reason string
	}{
		{"a NUL in the path", VarWebDir, "/srv/we\x00b", "directory path"},
		{"a line break in the path", VarWebDir, "/srv/we\nb", "directory path"},
		{"a tab in the path", VarWebDir, "/srv/we\tb", "directory path"},
		{"a path over the limit", VarWebDir, long, "4096"},
		{"bytes that are not text", VarWebDir, "/srv/we\xffb", "directory path"},
		{"a space in the key", VarPublicSupabaseAnonKey, "two words-XYZ123", "printable ASCII"},
		{"a non-ASCII key", VarPublicSupabaseAnonKey, "café-key-XYZ123", "printable ASCII"},
		{"a tab in the key", VarPublicSupabaseAnonKey, "tab\tkey-XYZ123", "printable ASCII"},
		{"a key over the limit", VarPublicSupabaseAnonKey, strings.Repeat("k", maxAnonKeyBytes+1), "2048"},
		{"a bad address", VarPublicSupabaseURL, "ftp://supabase-XYZ123.example", "http or https URL"},
		{"an address with credentials", VarPublicSupabaseURL, "https://user-XYZ123:pw-XYZ123@supabase.example", "no credentials"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := with(base(), c.dial, c.value)
			if c.dial == VarWebDir { // the directory is judged by its own rule before the cross-check, which is skipped for an invalid dial
				env = with(with(env, VarPublicSupabaseURL, webURL), VarPublicSupabaseAnonKey, webKey)
			}
			_, err := Load(envOf(env))
			ps := problemsOf(t, err)
			if len(ps) != 1 || ps[0].Name != c.dial || !strings.Contains(ps[0].Reason, c.reason) {
				t.Fatalf("problems = %+v, want exactly one naming %s with %q in its reason", ps, c.dial, c.reason)
			}
			if len(c.value) >= 5 && strings.Contains(err.Error(), strings.TrimSpace(c.value)) {
				t.Errorf("the error text echoes the rejected value: %v", err)
			}
		})
	}
	// the twins: the edge just inside each limit, and a path or key with the characters the rules allow
	for name, env := range map[string]map[string]string{
		"a path of exactly the limit":     with(with(with(base(), VarWebDir, strings.Repeat("a", maxWebDirBytes)), VarPublicSupabaseURL, webURL), VarPublicSupabaseAnonKey, webKey),
		"a path with a space and text":    with(with(with(base(), VarWebDir, "/srv/web app/dist"), VarPublicSupabaseURL, webURL), VarPublicSupabaseAnonKey, webKey),
		"a relative path":                 with(with(with(base(), VarWebDir, "web/dist"), VarPublicSupabaseURL, webURL), VarPublicSupabaseAnonKey, webKey),
		"a key of exactly the limit":      with(base(), VarPublicSupabaseAnonKey, strings.Repeat("k", maxAnonKeyBytes)),
		"a JWT-shaped key (dots, dashes)": with(base(), VarPublicSupabaseAnonKey, "aaaa.bbbb-cccc_dddd.eeee"),
	} {
		if _, err := Load(envOf(env)); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

func TestParsePublicURL_ShapesAreExactlyTheOnesTheCSPAndTheJSONCanCarry(t *testing.T) {
	accepted := []struct{ in, full, origin string }{
		{"https://abcdefgh.supabase.co", "https://abcdefgh.supabase.co", "https://abcdefgh.supabase.co"},
		{"https://abcdefgh.supabase.co/", "https://abcdefgh.supabase.co", "https://abcdefgh.supabase.co"},
		{"https://abcdefgh.supabase.co///", "https://abcdefgh.supabase.co", "https://abcdefgh.supabase.co"},
		{"  https://abcdefgh.supabase.co  ", "", ""}, // Load trims before it calls; the function itself is strict (the next table)
		{"http://gotrue:9999", "http://gotrue:9999", "http://gotrue:9999"},
		{"http://localhost:54321/", "http://localhost:54321", "http://localhost:54321"},
		{"HTTPS://Auth.Example.COM", "https://Auth.Example.COM", "https://Auth.Example.COM"},
		{"https://auth.example.test/supabase/v1.2/a_b~c-d/", "https://auth.example.test/supabase/v1.2/a_b~c-d", "https://auth.example.test"},
		{"http://[::1]:9999", "http://[::1]:9999", "http://[::1]:9999"},
		{"https://[2001:db8::1]", "https://[2001:db8::1]", "https://[2001:db8::1]"},
		{"http://10.0.0.5:8080", "http://10.0.0.5:8080", "http://10.0.0.5:8080"},
		{"https://xn--bcher-kva.example", "https://xn--bcher-kva.example", "https://xn--bcher-kva.example"},
		{"https://" + strings.Repeat("a", 63) + ".example", "https://" + strings.Repeat("a", 63) + ".example", "https://" + strings.Repeat("a", 63) + ".example"},
		{"https://example.test:65535", "https://example.test:65535", "https://example.test:65535"},
	}
	for _, c := range accepted {
		full, origin, ok := ParsePublicURL(c.in)
		if c.full == "" { // the one entry that documents the strictness
			if ok {
				t.Errorf("%q (white space around it) was accepted", c.in)
			}
			continue
		}
		if !ok || full != c.full || origin != c.origin {
			t.Errorf("ParsePublicURL(%q) = %q, %q, %v; want %q, %q, true", c.in, full, origin, ok, c.full, c.origin)
		}
	}
	rejected := []struct{ in, why string }{
		{"", "empty"},
		{"abcdefgh.supabase.co", "no scheme"},
		{"//abcdefgh.supabase.co", "no scheme"},
		{"ftp://abcdefgh.supabase.co", "scheme ftp"},
		{"https://", "no host"},
		{"https:///path", "no host"},
		{"https://user@abcdefgh.supabase.co", "a user name"},
		{"https://user:pw@abcdefgh.supabase.co", "credentials"},
		{"https://abcdefgh.supabase.co?a=b", "a query"},
		{"https://abcdefgh.supabase.co/?", "an empty query"},
		{"https://abcdefgh.supabase.co#f", "a fragment"},
		{"https://abc defgh.supabase.co", "a space in the host"},
		{"https://abcdefgh.supabase.co;x", "a semicolon, which would end the policy's source"},
		{"https://abcdefgh.supabase.co'", "a quote, which would open a keyword of the policy"},
		{"https://abcdefgh.sup\"abase.co", "a double quote"},
		{"https://-abcdefgh.supabase.co", "a label that starts with a hyphen"},
		{"https://abcdefgh-.supabase.co", "a label that ends with a hyphen"},
		{"https://abcdefgh..supabase.co", "an empty label"},
		{"https://abcdefgh.supabase.co.", "a trailing dot"},
		{"https://*.supabase.co", "a wildcard"},
		{"https://" + strings.Repeat("a", 64) + ".example", "a label over 63 bytes"},
		{"https://abcdefgh.supabase.co:0", "port 0"},
		{"https://abcdefgh.supabase.co:65536", "port 65536"},
		{"https://abcdefgh.supabase.co:abc", "a port that is not a number"},
		{"https://abcdefgh.supabase.co:", "an empty port"},
		{"https://abcdefgh.supabase.co:0443", "a port with a leading zero"},
		{"https://abcdefgh.supabase.co/a%20b", "an escape in the path"},
		{"https://abcdefgh.supabase.co/a b", "a space in the path"},
		{"https://abcdefgh.supabase.co/a/../b", "a .. segment"},
		{"https://abcdefgh.supabase.co/a/./b", "a . segment"},
		{"https://abcdefgh.supabase.co/a//b", "an empty segment"},
		{"https://abcdefgh.supabase.co/<x>", "angle brackets"},
		{"http://[zz::1]", "an IPv6 address that is not hexadecimal"},
		{"http://[::1%25eth0]", "an IPv6 zone"},
		{"http://[1]", "an IPv6 literal with no colons"},
		{"https://" + strings.Repeat("a.", 260) + "example", "over the length limit"},
		{" https://abcdefgh.supabase.co", "white space before"},
	}
	for _, c := range rejected {
		if full, origin, ok := ParsePublicURL(c.in); ok {
			t.Errorf("ParsePublicURL(%q) accepted %s as %q, %q", c.in, c.why, full, origin)
		}
	}
	// the controls of the table: the bare form of each family of rejection is itself accepted (a table that rejected everything would pass the loop above)
	for _, in := range []string{"https://abcdefgh.supabase.co", "https://abcdefgh.supabase.co:443", "https://abcdefgh.supabase.co/auth", "http://[::1]:9999"} {
		if _, _, ok := ParsePublicURL(in); !ok {
			t.Errorf("control failed: %q was refused", in)
		}
	}
}

func TestWebDials_ARedactedConfigNeverPrintsTheKeyAndSaysWhetherTheAppIsOn(t *testing.T) {
	cfg, err := Load(envOf(with(with(with(base(), VarWebDir, "/srv/web"), VarPublicSupabaseURL, webURL), VarPublicSupabaseAnonKey, webKey)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicSupabaseAnonKey != webKey { // the control: the key really is in the Config, so its absence below is the rendering's doing
		t.Fatal("the key did not load")
	}
	for name, text := range map[string]string{"String": cfg.String(), "GoString": cfg.GoString(), "LogValue": cfg.LogValue().String()} {
		if strings.Contains(text, webKey) {
			t.Errorf("%s prints the key: %s", name, text)
		}
		for _, want := range []string{"web_dir=/srv/web", "public_supabase_url=" + webURL, "public_supabase_anon_key=(set)"} {
			if name == "String" && !strings.Contains(text, want) {
				t.Errorf("%s does not say %q: %s", name, want, text)
			}
		}
	}
	off, _ := Load(envOf(base()))
	if s := off.String(); !strings.Contains(s, "web_dir=(unset)") || !strings.Contains(s, "public_supabase_anon_key=(unset)") {
		t.Errorf("an off Config does not say so: %s", s)
	}
}

func TestWebDials_TheMeaningSaysWhatTheOperatorMustKnow(t *testing.T) {
	facts := map[string][]string{
		VarWebDir:                {"`/srv/web`", "index.html", "/config.json", "Content-Security-Policy", "no-store", "refuses to start", "exit status 2", "PLATFORM_PUBLIC_SUPABASE_URL", "Consumed by `internal/app`"},
		VarPublicSupabaseURL:     {"Required when PLATFORM_WEB_DIR is set", "supabase_url", "Content-Security-Policy", "trailing slashes are dropped"},
		VarPublicSupabaseAnonKey: {"Required when PLATFORM_WEB_DIR is set", "PUBLIC by design", "anon_key", "service-role key"},
	}
	byName := map[string]Var{}
	for _, v := range Vars() {
		byName[v.Name] = v
	}
	seen := 0
	for name, list := range facts {
		for _, f := range list {
			seen++
			if !strings.Contains(strings.ReplaceAll(byName[name].Doc, "**", ""), f) {
				t.Errorf("the meaning of %s does not say %q", name, f)
			}
		}
	}
	if seen < 10 {
		t.Fatalf("checked %d facts: the instrument matched too little", seen)
	}
	if strings.Contains(byName[VarWebDir].Doc, "never printed") {
		t.Error("control failed: the matcher finds a phrase that is not there")
	}
}
