package contractcheck

import (
	"encoding/json"
	"strings"
	"testing"
)

func quoted(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func classes(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Class+"@"+f.Path)
	}
	return strings.Join(out, ",")
}

func TestFindSecretsAcceptsTheNormalisedJobBody(t *testing.T) {
	fs, err := FindSecrets([]byte(jobBody))
	if err != nil || len(fs) != 0 {
		t.Fatalf("a synthetic body must be clean, got %v %v", classes(fs), err)
	}
}

func TestFindSecretsFlagsEachClassAndAcceptsItsBenignTwin(t *testing.T) {
	tok := token(t, 20)
	jwt := "eyJ" + token(t, 8) + "." + token(t, 8) + "." + token(t, 8)
	pem := "-----BEGIN " + "RSA PRIVATE " + "KEY-----"
	akia := "AKIA" + strings.ToUpper(token(t, 8))
	cases := []struct {
		name    string
		bad     string
		class   string
		benign  string
		benName string
	}{
		{"bearer token", `{"note":` + quoted(t, "Authorization failed for Bearer "+tok) + `}`, ClassBearer,
			`{"note":"the bearer authentication scheme is required"}`, "bearer used as a word"},
		{"secret named key", `{"api_key":` + quoted(t, tok) + `}`, ClassSecretNamedKey,
			`{"api_key":"REDACTED","password":"xxxxxxxx","secret":"synthetic-secret"}`, "placeholders under secret names"},
		{"password key", `{"user":{"password":` + quoted(t, "hunter"+tok[:4]) + `}}`, ClassSecretNamedKey,
			`{"user":{"password":""}}`, "an empty password"},
		{"jwt", `{"note":` + quoted(t, jwt) + `}`, ClassJWT,
			`{"note":"eyJ is a prefix, not a token"}`, "a short prefix"},
		{"private key header", `{"note":` + quoted(t, pem) + `}`, ClassPrivateKey,
			`{"note":"a private key is never recorded"}`, "prose about keys"},
		{"known prefix", `{"note":` + quoted(t, akia) + `}`, ClassKnownPrefix,
			`{"note":"AKIA is the prefix of one family of ids"}`, "the bare prefix"},
		{"email address", `{"to":` + quoted(t, "someone@"+"corp.com") + `}`, ClassEmail,
			`{"to":"someone@example.com","cc":"x@mail.example.org","d":"y@host.test"}`, "documentation domains"},
		{"ipv4 address", `{"host":` + quoted(t, ip(8, 8, 8, 8)) + `}`, ClassIPv4,
			`{"a":"192.0.2.7","b":"198.51.100.9","c":"203.0.113.4","d":"10.1.2.3","e":"172.16.5.5","f":"192.168.1.1","g":"127.0.0.1"}`, "documentation and private addresses"},
		{"long hex run", `{"note":` + quoted(t, "id "+tok+tok) + `}`, ClassLongRun,
			`{"note":"00000000-0000-4000-8000-000000000001 at 2026-01-01T00:00:00Z","x":"0000000000000000000000000000000000000000"}`, "uuid, timestamp and a placeholder run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, err := FindSecrets([]byte(c.bad))
			if err != nil || len(fs) == 0 {
				t.Fatalf("must flag %s, got %v %v", c.class, fs, err)
			}
			found := false
			for _, f := range fs {
				found = found || f.Class == c.class
			}
			if !found {
				t.Fatalf("must flag class %s, got %s", c.class, classes(fs))
			}
			fs, err = FindSecrets([]byte(c.benign))
			if err != nil || len(fs) != 0 {
				t.Fatalf("the benign twin (%s) must be clean, got %s %v", c.benName, classes(fs), err)
			}
		})
	}
}

func TestFindSecretsNeverReturnsTheValue(t *testing.T) {
	tok := token(t, 24)
	body := `{"api_key":` + quoted(t, tok) + `,"note":` + quoted(t, "Bearer "+tok) + `,"to":` + quoted(t, tok+"@corp.com") + `}`
	fs, err := FindSecrets([]byte(body))
	if err != nil || len(fs) < 3 {
		t.Fatalf("want at least three findings, got %s %v", classes(fs), err)
	}
	for _, f := range fs {
		if strings.Contains(f.String(), tok) || strings.Contains(f.Path, tok[:8]) {
			t.Fatalf("a finding carries the value: %s", f)
		}
	}
}

func TestFindSecretsLooksAtKeysAsWellAsValues(t *testing.T) {
	tok := token(t, 20)
	fs, err := FindSecrets([]byte(`{"` + tok + tok + `":1}`))
	if err != nil || len(fs) != 1 || fs[0].Class != ClassLongRun || !strings.HasSuffix(fs[0].Path, "(key)") {
		t.Fatalf("a credential used as a key must be flagged at the key, got %s %v", classes(fs), err)
	}
}

func TestFindSecretsIpv4OnlyFlagsPublicAddressesAndIgnoresNonAddresses(t *testing.T) {
	flagged := []string{ip(8, 8, 8, 8), ip(1, 1, 1, 1), ip(100, 20, 3, 4), ip(172, 32, 0, 1), ip(193, 0, 2, 1)}
	for _, a := range flagged {
		if fs, _ := FindSecrets([]byte(`{"h":"` + a + `"}`)); len(fs) != 1 || fs[0].Class != ClassIPv4 {
			t.Fatalf("%s must be flagged, got %s", a, classes(fs))
		}
	}
	allowed := []string{ip(192, 0, 2, 255), ip(10, 255, 255, 255), ip(172, 31, 255, 255), ip(192, 168, 0, 1), ip(169, 254, 1, 1), ip(0, 0, 0, 0), ip(999, 1, 1, 1), "01." + "2.3.4", "1.2.3"}
	for _, a := range allowed {
		if fs, _ := FindSecrets([]byte(`{"h":"` + a + `"}`)); len(fs) != 0 {
			t.Fatalf("%s must not be flagged, got %s", a, classes(fs))
		}
	}
}

func TestFindSecretsSecretNamesAreMatchedInEveryCaseConvention(t *testing.T) {
	tok := token(t, 10)
	for _, name := range []string{"api_key", "apiKey", "API-KEY", "Authorization", "client_secret", "accessToken", "passwd", "key", "idempotency_key", "bearer"} {
		fs, _ := FindSecrets([]byte(`{"` + name + `":"` + tok + `"}`))
		if len(fs) != 1 || fs[0].Class != ClassSecretNamedKey {
			t.Fatalf("a string under %q must be flagged, got %s", name, classes(fs))
		}
	}
	for _, name := range []string{"monkey", "token_type", "tokens_used", "keyboard", "passport", "state"} {
		if fs, _ := FindSecrets([]byte(`{"` + name + `":"` + tok + `"}`)); len(fs) != 0 {
			t.Fatalf("a string under %q must not be flagged, got %s", name, classes(fs))
		}
	}
}

func TestFindSecretsRefusesWhatTheParserRefuses(t *testing.T) {
	for _, body := range []string{"", `{"a":`, `{"a":1,"a":2}`} {
		fs, err := FindSecrets([]byte(body))
		if fs != nil || err == nil {
			t.Fatalf("%q: want a refusal and no findings, got %v %v", body, fs, err)
		}
	}
}

func TestFindSecretsStopsAtTheFindingCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 50; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"k` + strings.Repeat("a", i) + `":"` + ip(8, 8, 8, 8) + `"`)
	}
	b.WriteString("}")
	fs, _ := FindSecrets([]byte(b.String()))
	if len(fs) != maxFindings {
		t.Fatalf("findings = %d, want the cap %d", len(fs), maxFindings)
	}
}
