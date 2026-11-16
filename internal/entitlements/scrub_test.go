package entitlements

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

const m3Canary = "CANARY-m3-admin-key-7f3a91c2"

// Every spelling in which a client, a transport or a proxy could put the admin key into an error text.
func tokenSpellings() map[string]string {
	return map[string]string{
		"bare":                   "request failed: " + m3Canary,
		"bearer header":          "Authorization: Bearer " + m3Canary + " was rejected",
		"bearer, lower case":     "bearer " + m3Canary,
		"url userinfo":           "Get \"https://operator:" + m3Canary + "@m3.internal:8443/v1/admin/users/x/suspend\": dial tcp: refused",
		"url user only":          "Post https://" + m3Canary + "@m3.internal/v1/admin: eof",
		"query token":            "Post \"http://m3:8080/v1/admin?token=" + m3Canary + "&x=1\": timeout",
		"query access_token":     "http://m3:8080/?access_token=" + m3Canary,
		"query key":              "http://m3:8080/?key=" + m3Canary,
		"query escaped":          "http://m3:8080/?q=" + url.QueryEscape("a b/"+m3Canary+"+"),
		"form password":          "body: user=op&password=" + m3Canary + "&x=1",
		"embedded in the middle": "prefix " + m3Canary + " suffix",
	}
}

func TestScrubError_TheTokenNeverSurvives(t *testing.T) {
	for name, text := range tokenSpellings() {
		t.Run(name, func(t *testing.T) {
			// Paired control: the unscrubbed text DOES carry the token, so the assertion below is about the scrub.
			if !strings.Contains(text, m3Canary) && !strings.Contains(text, url.QueryEscape(m3Canary)) {
				t.Fatalf("control failed: the fixture %q does not contain the token", text)
			}
			got := scrubError(errors.New(text), m3Canary)
			if strings.Contains(got, m3Canary) {
				t.Errorf("the token survived the exact-secret pass: %q", got)
			}
			if got == "" {
				t.Error("the scrub produced nothing: the assertion would pass over an empty string")
			}
		})
	}
}

func TestScrubError_PatternsRemoveCredentialShapesWithoutKnowingTheSecret(t *testing.T) {
	// No secret is passed: only the shapes of a credential are removed. (This is the wall for a credential the
	// reconciler was never told about.)
	for name, text := range tokenSpellings() {
		if name == "bare" || name == "embedded in the middle" || name == "query escaped" {
			continue // a bare value has no shape to recognise: only the exact-secret pass can remove it
		}
		t.Run(name, func(t *testing.T) {
			got := scrubError(errors.New(text))
			if strings.Contains(got, m3Canary) {
				t.Errorf("the credential shape %q was not removed by the patterns: %q", name, got)
			}
		})
	}
	// And the paired control: the same call keeps a bare value (so the skipped cases above are skipped for a reason).
	if got := scrubError(errors.New("prefix " + m3Canary + " suffix")); !strings.Contains(got, m3Canary) {
		t.Errorf("control failed: with no secret known a bare value was removed anyway: %q", got)
	}
	// Prose that merely mentions the words is left alone.
	for _, prose := range []string{"the bearer is unknown", "no token supplied", "connection refused", "key not found"} {
		if got := scrubError(errors.New(prose)); got != prose {
			t.Errorf("scrubError(%q) = %q: prose was altered", prose, got)
		}
	}
}

func TestScrubError_ExactSecretsInTheirUrlSpellings(t *testing.T) {
	const awkward = "p@ss/w:rd+1 2" // every character a URL rewrites
	for _, form := range []string{awkward, url.QueryEscape(awkward), url.PathEscape(awkward)} {
		got := scrubError(fmt.Errorf("request to https://m3/v1?x=%s failed", form), awkward)
		if strings.Contains(got, form) {
			t.Errorf("the spelling %q of the secret survived: %q", form, got)
		}
	}
}

func TestScrubError_IsCappedAndValid(t *testing.T) {
	long := strings.Repeat("x", 2000)
	if got := scrubError(errors.New(long)); len(got) > MaxErrorBytes || len(got) < MaxErrorBytes-5 || !strings.HasSuffix(got, "...") {
		t.Errorf("a 2000 byte error became %d bytes ending %q, want at most %d and a visible cut", len(got), got[max(0, len(got)-5):], MaxErrorBytes)
	}
	// A multi-byte character straddling the cut must not be split.
	for pad := 0; pad < 4; pad++ {
		text := strings.Repeat("a", MaxErrorBytes-5+pad) + strings.Repeat("é", 20)
		got := scrubError(errors.New(text))
		if !utf8.ValidString(got) || len(got) > MaxErrorBytes {
			t.Errorf("pad %d: result is not valid UTF-8 or too long (%d bytes): %q", pad, len(got), got)
		}
	}
	// The token sitting across the cap boundary is removed BEFORE the cut, so no prefix of it is left behind.
	text := strings.Repeat("a", MaxErrorBytes-10) + m3Canary + " tail"
	got := scrubError(errors.New(text), m3Canary)
	if strings.Contains(got, m3Canary[:8]) || len(got) > MaxErrorBytes {
		t.Errorf("a token across the boundary left a prefix or overran the cap: %d bytes, %q", len(got), got[max(0, len(got)-40):])
	}
	// Control: a short text passes through whole.
	if got := scrubError(errors.New("short and clean")); got != "short and clean" {
		t.Errorf("control failed: a short clean error became %q", got)
	}
}

func TestScrubError_ControlCharactersAndBadUTF8(t *testing.T) {
	got := scrubError(errors.New("line one\nline two\r\n\x00nul\x07bell\x7fdel"))
	for _, bad := range []string{"\n", "\r", "\x00", "\x07", "\x7f"} {
		if strings.Contains(got, bad) {
			t.Errorf("a control character %q survived: %q (a NUL cannot be stored, a newline forges a log line)", bad, got)
		}
	}
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("the readable text was lost: %q", got)
	}
	if got := scrubError(errors.New("bad \xff\xfe bytes")); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived: %q", got)
	}
	if scrubError(nil) != "" {
		t.Error("nil must scrub to the empty string")
	}
	// Secrets that are empty are ignored, not turned into "replace every empty string".
	if got := scrubError(errors.New("plain"), "", ""); got != "plain" {
		t.Errorf("an empty secret altered the text: %q", got)
	}
}

func TestErrClass_NamesTheClassAndNeverTheText(t *testing.T) {
	const secretish = "host=db.internal user=platform_app password=hunter2"
	pg := &pgconn.PgError{Code: "23514", ConstraintName: "m3_expire_is_due", Message: secretish}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"pg error with a constraint", pg, "database error 23514 (constraint m3_expire_is_due)"},
		{"pg error without one", &pgconn.PgError{Code: "57P01", Message: secretish}, "database error 57P01"},
		{"dns", &net.DNSError{Err: secretish, Name: "db.internal"}, "dns lookup failed"},
		{"refused by text", errors.New("dial tcp 10.0.0.5:5432: connect: connection refused " + secretish), "connection refused"},
		{"eof", io.ErrUnexpectedEOF, "connection closed"},
		{"unknown", errors.New(secretish), "error (*errors.errorString)"},
	}
	for _, tc := range cases {
		got := errClass(tc.err)
		if got != tc.want {
			t.Errorf("%s: errClass = %q, want %q", tc.name, got, tc.want)
		}
		for _, leak := range []string{"hunter2", "platform_app", "db.internal", "10.0.0.5"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: the class %q carries %q", tc.name, got, leak)
			}
		}
	}
}
