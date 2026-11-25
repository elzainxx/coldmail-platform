package jobs

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every refusal below has its legal twin: the nearest input that is still accepted, so a refusal is the input's doing and not the validator's mood.

func TestValidKey(t *testing.T) {
	for _, c := range []struct {
		key string
		ok  bool
	}{
		{"a", true}, {"K-1_2.3:4", true}, {strings.Repeat("k", 128), true}, // the limits and every allowed character
		{"", false}, {strings.Repeat("k", 129), false}, {"has space", false}, {"semi;colon", false}, {"slash/slash", false}, {"ünï", false}, {"new\nline", false}, {"trailing\n", false},
	} {
		if got := validKey(c.key); got != c.ok {
			t.Errorf("validKey(%q) = %v, want %v", c.key, got, c.ok)
		}
	}
}

func TestValidFingerprint(t *testing.T) {
	good := strings.Repeat("ab12", 16)
	for _, c := range []struct {
		fp string
		ok bool
	}{{good, true}, {good[:63], false}, {good + "0", false}, {strings.ToUpper(good), false}, {strings.Repeat("g", 64), false}, {"", false}} {
		if got := validFingerprint(c.fp); got != c.ok {
			t.Errorf("validFingerprint(%q) = %v, want %v", c.fp, got, c.ok)
		}
	}
}

func TestIsUUID_OnlyTheCanonicalForm(t *testing.T) {
	const good = "0b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	if !isUUID(good) {
		t.Fatal("the canonical form was refused")
	}
	for _, bad := range []string{"", strings.ToUpper(good), good[:35], good + "0", strings.ReplaceAll(good, "-", ""), "{" + good + "}", "0b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5g", " " + good} {
		if isUUID(bad) {
			t.Errorf("isUUID(%q) accepted a spelling that would hash to another advisory lock", bad)
		}
	}
}

func TestCheckUser_RefusesTheSystemUserAndNonCanonicalIDs(t *testing.T) {
	if err := checkUser("0b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"); err != nil {
		t.Errorf("a customer was refused: %v", err)
	}
	for _, bad := range []string{"", systemUser, "0B6F6D5E-1A2B-4C3D-8E9F-0A1B2C3D4E5F", "not-a-uuid"} {
		if err := checkUser(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("checkUser(%q) = %v, want ErrBadRequest", bad, err)
		}
	}
}

func TestNewUUID_IsAVersion4UUIDAndDoesNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		u := newUUID()
		if !isUUID(u) || u[14] != '4' || !strings.ContainsRune("89ab", rune(u[19])) {
			t.Fatalf("newUUID() = %q is not a canonical version 4 uuid", u)
		}
		if seen[u] {
			t.Fatalf("newUUID() repeated %q within 2000 draws", u)
		}
		seen[u] = true
	}
}

func TestNormaliseSettings(t *testing.T) {
	for _, c := range []struct {
		in      string
		chained bool
		want    string // "" = refused
	}{
		{"", false, UnknownExclude}, {"", true, UnknownExclude}, // the default is the safe one
		{UnknownExclude, false, UnknownExclude}, {UnknownExclude, true, UnknownExclude},
		{UnknownInclude, true, UnknownInclude}, // the legal twin of the next row
		{UnknownInclude, false, ""},            // meaningless without a source: refused, not silently ignored
		{"Include", true, ""}, {"yes", true, ""},
	} {
		got, err := normaliseSettings(Settings{UnknownChoice: c.in}, c.chained)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("normaliseSettings(%q, chained=%v) accepted %q", c.in, c.chained, got.UnknownChoice)
		case c.want != "" && (err != nil || got.UnknownChoice != c.want):
			t.Errorf("normaliseSettings(%q, chained=%v) = %q, %v; want %q", c.in, c.chained, got.UnknownChoice, err, c.want)
		case c.want == "" && !errors.Is(err, ErrBadRequest):
			t.Errorf("normaliseSettings(%q, chained=%v): %v is not an ErrBadRequest", c.in, c.chained, err)
		}
	}
}

func TestValidateContent(t *testing.T) {
	good := Content{Subject: "Hello", HTML: "<p>hi</p>", Text: "hi", ReplyTo: "reply@example.com"}
	with := func(f func(*Content)) *Content { c := good; f(&c); return &c }
	for _, c := range []struct {
		name   string
		in     *Content
		reason string // "" = accepted
	}{
		{"a full campaign", &good, ""},
		{"html only", with(func(c *Content) { c.Text = "" }), ""},
		{"text only", with(func(c *Content) { c.HTML = "" }), ""},
		{"no reply-to", with(func(c *Content) { c.ReplyTo = "" }), ""},
		{"a subject of 998 bytes", with(func(c *Content) { c.Subject = strings.Repeat("s", MaxSubjectBytes) }), ""},
		{"a body of exactly 1 MiB", with(func(c *Content) { c.HTML = strings.Repeat("h", MaxBodyBytes) }), ""},

		{"no content at all", nil, "content is required for a send job"},
		{"a blank subject", with(func(c *Content) { c.Subject = "  " }), "content.subject must not be blank"},
		{"a subject of 999 bytes", with(func(c *Content) { c.Subject = strings.Repeat("s", MaxSubjectBytes+1) }), "content.subject is 999 bytes"},
		{"a subject with a line break", with(func(c *Content) { c.Subject = "a\r\nBcc: x@example.com" }), "content.subject must be one line"},
		{"an html body over 1 MiB", with(func(c *Content) { c.HTML = strings.Repeat("h", MaxBodyBytes+1) }), "content.html is"},
		{"a text body over 1 MiB", with(func(c *Content) { c.Text = strings.Repeat("t", MaxBodyBytes+1) }), "content.text is"},
		{"no body", with(func(c *Content) { c.HTML, c.Text = " ", "\n" }), "content needs a body"},
		{"a NUL in the subject", with(func(c *Content) { c.Subject = "a\x00b" }), "content.subject must be valid UTF-8 text without NUL"},
		{"a body that is not UTF-8", with(func(c *Content) { c.HTML = "x\xff" }), "content.html must be valid UTF-8 text without NUL"},
		{"two reply-to addresses", with(func(c *Content) { c.ReplyTo = "a@example.com, b@example.com" }), "content.reply_to must be a single bare address"},
		{"a reply-to with a display name", with(func(c *Content) { c.ReplyTo = "Ann <a@example.com>" }), "content.reply_to must be a single bare address"},
		{"a reply-to that is not an address", with(func(c *Content) { c.ReplyTo = "not an address" }), "content.reply_to must be a single bare address"},
		{"a reply-to with a header injection", with(func(c *Content) { c.ReplyTo = "a@example.com\r\nBcc: x@example.com" }), "content.reply_to is not a single address"},
	} {
		out, err := validateContent(c.in)
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.reason == "" && out == nil:
			t.Errorf("%s: accepted but returned no content", c.name)
		case c.reason != "" && err == nil:
			t.Errorf("%s: accepted, want a refusal naming %q", c.name, c.reason)
		case c.reason != "" && (!errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), c.reason)):
			t.Errorf("%s: got %v, want a bad request naming %q", c.name, err, c.reason)
		}
		if err != nil && c.in != nil && (strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "Hello")) {
			t.Errorf("%s: the refusal quotes the content: %v", c.name, err)
		}
	}
	// the stored form is trimmed, and does not alias the caller's value
	in := with(func(c *Content) { c.Subject, c.ReplyTo = "  Hi  ", " r@example.com " })
	out, err := validateContent(in)
	if err != nil || out.Subject != "Hi" || out.ReplyTo != "r@example.com" {
		t.Errorf("validateContent trimmed to %+v, %v; want subject Hi and reply-to r@example.com", out, err)
	}
	if in.Subject != "  Hi  " {
		t.Error("validateContent changed the caller's content")
	}
}

func TestCursor_RoundTripAndRefusals(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 20, 30, 123456000, time.UTC)
	const id = "0b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	c := encodeCursor(at, id)
	gotT, gotID, err := decodeCursor(c)
	if err != nil || !gotT.Equal(at) || gotID != id {
		t.Fatalf("round trip: %v, %s, %v; want %v, %s", gotT, gotID, err, at, id)
	}
	// another zone is the same instant
	if c2 := encodeCursor(at.In(time.FixedZone("x", 3600*5)), id); c2 != c {
		t.Errorf("the same instant in another zone encodes differently: %s vs %s", c2, c)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, bad := range map[string]string{
		"not base64":            "!!!",
		"empty":                 "",
		"not json":              enc("hello"),
		"a bad time":            enc(`{"t":"yesterday","id":"` + id + `"}`),
		"a bad id":              enc(`{"t":"2026-11-25T10:20:30Z","id":"x"}`),
		"an upper-case id":      enc(`{"t":"2026-11-25T10:20:30Z","id":"` + strings.ToUpper(id) + `"}`),
		"an unknown member":     enc(`{"t":"2026-11-25T10:20:30Z","id":"` + id + `","x":1}`),
		"trailing data":         enc(`{"t":"2026-11-25T10:20:30Z","id":"` + id + `"}{}`),
		"standard base64 (pad)": base64.StdEncoding.EncodeToString([]byte(`{"t":"2026-11-25T10:20:30Z","id":"`+id+`"}`)) + "=",
	} {
		if _, _, err := decodeCursor(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("decodeCursor(%s): %v, want ErrBadRequest", name, err)
		} else if strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("decodeCursor(%s): the error quotes the cursor", name)
		}
	}
}

func TestPageLimit(t *testing.T) {
	for _, c := range []struct {
		n, def, upper, want int
		ok                  bool
	}{
		{0, 25, 100, 25, true}, {1, 25, 100, 1, true}, {100, 25, 100, 100, true}, // the twins
		{101, 25, 100, 0, false}, {-1, 25, 100, 0, false},
	} {
		got, err := pageLimit(c.n, c.def, c.upper)
		if (err == nil) != c.ok || (c.ok && got != c.want) || (!c.ok && !errors.Is(err, ErrBadRequest)) {
			t.Errorf("pageLimit(%d, %d, %d) = %d, %v; want %d ok=%v", c.n, c.def, c.upper, got, err, c.want, c.ok)
		}
	}
}

func TestCutText_NeverSplitsACodePoint(t *testing.T) {
	s := strings.Repeat("é", 10) // 20 bytes
	for n := 0; n <= 21; n++ {
		got := cutText(s, n)
		if len(got) > n && n < len(s) {
			t.Errorf("cutText(%d) is %d bytes", n, len(got))
		}
		if !textOK(got) {
			t.Errorf("cutText(%d) split a code point: %q", n, got)
		}
	}
	if cutText("abc", 10) != "abc" {
		t.Error("a short text was changed")
	}
}
