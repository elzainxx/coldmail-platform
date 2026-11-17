package contractcheck

import (
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func norm(t *testing.T, body string) string {
	t.Helper()
	out, err := Normalise([]byte(body))
	if err != nil {
		t.Fatalf("Normalise(%q): %v", body, err)
	}
	return string(out)
}

func TestNormaliseMapsUUIDsByOrderOfFirstAppearanceAndKeepsTheirIdentity(t *testing.T) {
	a := "3f2b8a10-5c1d-4e7f-9a0b-1c2d3e4f5a6b"
	b := "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
	out := norm(t, `{"first":"`+a+`","second":"`+b+`","again":"`+strings.ToUpper(a)+`","embedded":"job:`+b+`/x","`+a+`":1}`)
	for _, want := range []string{
		`"first": "00000000-0000-4000-8000-000000000001"`,
		`"second": "00000000-0000-4000-8000-000000000002"`,
		`"again": "00000000-0000-4000-8000-000000000001"`, // the same uuid in another case is the same synthetic uuid
		`"embedded": "job:00000000-0000-4000-8000-000000000002/x"`,
		`"00000000-0000-4000-8000-000000000001": 1`, // a uuid used as a key
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), strings.ToLower(a)) || strings.Contains(strings.ToLower(out), strings.ToLower(b)) {
		t.Fatalf("a real uuid survived:\n%s", out)
	}
}

func TestNormaliseKeepsTheShapeOfTimestamps(t *testing.T) {
	out := norm(t, `{"a":"2026-11-17T21:32:01Z","b":"2026-11-17T21:32:01.0030894Z","c":"2026-11-17 21:32:01+02:00","d":"since 2026-11-17T21:32:01.5-07:00 ago","e":"2026-11-17"}`)
	for _, want := range []string{
		`"a": "2026-01-01T00:00:00Z"`,
		`"b": "2026-01-01T00:00:00.0000000Z"`,
		`"c": "2026-01-01 00:00:00+00:00"`,
		`"d": "since 2026-01-01T00:00:00.0+00:00 ago"`,
		`"e": "2026-11-17"`, // a date alone is not a timestamp: untouched
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
}

func TestNormaliseReplacesIdsAndCursorsWithOneCharacterOfTheSameAlphabetAndLength(t *testing.T) {
	hex40 := strings.Repeat("a1b2c3d4e5", 4)
	b64 := "Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5MA"
	out := norm(t, `{"request_id":"req_9f8e7d6c5b","cursor":"eyJwIjoyfQ","next_cursor":"AbC123","trace":"`+hex40+`","opaque":"`+hex40+`","token_b64":"`+b64+`","ids":["xyz789","abc123"],"jobId":"Q1w2e3"}`)
	for _, want := range []string{
		`"request_id": "aaaaaaaaaaaaaa"`,              // lower-case and not hex, so a; length kept
		`"cursor": "AAAAAAAAAA"`,                      // a capital letter in the value: A
		`"next_cursor": "000000"`,                     // every character is a hex digit: 0
		`"trace": "` + strings.Repeat("0", 40) + `"`,  // hex stays hex
		`"opaque": "` + strings.Repeat("0", 40) + `"`, // no key said so, but 40 hex characters is an identifier
		`"token_b64": "` + strings.Repeat("A", len(b64)) + `"`,
		`"jobId": "AAAAAA"`, // a capital letter in the value: 'A'
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"aaaaaa"`) || !strings.Contains(out, `"000000"`) {
		t.Fatalf("every string of an id array is replaced:\n%s", out)
	}
}

func TestNormaliseLeavesVocabularyWordsModelNamesAndShortStringsAlone(t *testing.T) {
	body := `{"state":"completed","reason":"no_results_available_for_this_request","model":"claude-sonnet-4-5-20250929","note":"mailbox full","code":"E42","short":"abc123","id":""}`
	out := norm(t, body)
	for _, want := range []string{`"completed"`, `"no_results_available_for_this_request"`, `"claude-sonnet-4-5-20250929"`, `"mailbox full"`, `"E42"`, `"abc123"`, `"id": ""`} {
		if !strings.Contains(out, want) {
			t.Fatalf("%s must be untouched:\n%s", want, out)
		}
	}
}

func TestNormaliseRedactsAStringUnderASecretName(t *testing.T) {
	tok := token(t, 16)
	out := norm(t, `{"api_key":"`+tok+`","nested":{"password":"hunter2","Authorization":"Basic `+tok+`"},"token_type":"bearer","key":null,"secret":5}`)
	if strings.Contains(out, tok) || strings.Contains(out, "hunter2") {
		t.Fatalf("a secret survived:\n%s", out)
	}
	for _, want := range []string{`"api_key": "REDACTED"`, `"password": "REDACTED"`, `"Authorization": "REDACTED"`, `"token_type": "bearer"`, `"key": null`, `"secret": 5`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
}

func TestNormaliseKeepsKeyOrderStructureNumbersAndLiterals(t *testing.T) {
	body := `{"z":1.50,"a":[true,false,null,{"y":[],"x":{}}],"m":1e999999999,"n":-0}`
	out := norm(t, body)
	want := "{\n  \"z\": 1.50,\n  \"a\": [\n    true,\n    false,\n    null,\n    {\n      \"y\": [],\n      \"x\": {}\n    }\n  ],\n  \"m\": 1e999999999,\n  \"n\": -0\n}\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	// The skeleton before and after is the same (structure unchanged).
	before, _ := Skeleton([]byte(body), modclient.Shape{})
	after, _ := Skeleton([]byte(out), modclient.Shape{})
	if before.String() != after.String() {
		t.Fatalf("the structure changed:\n%s\nvs\n%s", before, after)
	}
}

func TestNormaliseIsDeterministicAndIdempotent(t *testing.T) {
	body := `{"id":"3f2b8a10-5c1d-4e7f-9a0b-1c2d3e4f5a6b","at":"2026-11-17T21:32:01.25Z","cursor":"Ab12Cd34Ef56","x":["3f2b8a10-5c1d-4e7f-9a0b-1c2d3e4f5a6b","11111111-2222-4333-8444-555555555555"],"request_id":"deadbeefdeadbeef","api_key":"zzz"}`
	first := norm(t, body)
	if again := norm(t, body); again != first {
		t.Fatal("the same input gave two outputs")
	}
	if twice := norm(t, first); twice != first {
		t.Fatalf("normalising twice changed the output:\n%s\nvs\n%s", twice, first)
	}
}

func TestNormaliseOutputPassesTheSecretCheckEvenWhenTheInputHeldCredentialShapedIds(t *testing.T) {
	long := token(t, 24)
	body := `{"request_id":"` + long + `","cursor":"` + long + `","trace":"` + long + `","provider_message_id":"` + long + `"}`
	if fs, _ := FindSecrets([]byte(body)); len(fs) == 0 {
		t.Fatal("the raw body must look secret-shaped (the control)")
	}
	out := norm(t, body)
	if fs, _ := FindSecrets([]byte(out)); len(fs) != 0 {
		t.Fatalf("a normalised body must be clean, got %s", classes(fs))
	}
	if strings.Contains(out, long[:8]) {
		t.Fatal("the id survived")
	}
}

func TestNormaliseOutputIsCleanWhereTheIdsWereRecognisedAndRefusedWhereTheyWereNot(t *testing.T) {
	// A credential in free text is NOT normalised away: Record's check must still see it.
	tok := token(t, 24)
	out := norm(t, `{"note":"Bearer `+tok+`"}`)
	if fs, _ := FindSecrets([]byte(out)); len(fs) == 0 {
		t.Fatal("a bearer token in free text must survive normalisation so that the secret check sees it")
	}
}

func TestNormaliseRefusesWhatTheParserRefusesWithTheSameKind(t *testing.T) {
	for body, want := range map[string]ParseKind{"": ParseEmpty, `{"a":`: ParseSyntax, `{"a":1,"a":2}`: ParseDuplicateKey, "{\"a\":\"\xff\"}": ParseInvalidUTF8, `{} []`: ParseTrailingData} {
		out, err := Normalise([]byte(body))
		if out != nil || ParseKindOf(err) != want {
			t.Fatalf("%q: want no output and %v, got %q %v", body, want, out, err)
		}
	}
}

func TestNormaliseDoesNotEscapeHTMLCharactersInStrings(t *testing.T) {
	out := norm(t, `{"note":"a<b & c>d","emoji":"café"}`)
	if !strings.Contains(out, `"a<b & c>d"`) || !strings.Contains(out, `"café"`) {
		t.Fatalf("strings must be written as they are:\n%s", out)
	}
}

func TestNormalisedSyntheticUUIDsHaveTheDocumentedForm(t *testing.T) {
	re := regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)
	out := norm(t, `{"a":"11111111-2222-4333-8444-555555555555"}`)
	m := regexp.MustCompile(`"a": "([^"]+)"`).FindStringSubmatch(out)
	if m == nil || !re.MatchString(m[1]) || m[1] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("synthetic uuid = %v", m)
	}
}
