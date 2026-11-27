package unsubscribe

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every test below has a paired control: the property it proves is shown to be absent from, or refused by, the thing next to it, so a test cannot pass because the property is true of
// everything. The secrets are built at run time from a repeated word (a quoted long literal next to the word "secret" is what a secret scanner flags).

func key(word string, n int) []byte { return bytes.Repeat([]byte(word), n/len(word)+1)[:n] }

var (
	secretA = key("alpha-", 40)
	secretB = key("bravo-", 40)
)

const (
	itemOne = "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa"
	itemTwo = "f1e2d3c4-b5a6-4789-9abc-def012345678"
)

func mustToken(t *testing.T, secret []byte, id string) string {
	t.Helper()
	tok, err := Token(secret, id)
	if err != nil {
		t.Fatalf("Token(%q): %v", id, err)
	}
	return tok
}

func TestToken_FormatIsTheSpecifiedOneAndRoundTrips(t *testing.T) {
	tok := mustToken(t, secretA, itemOne)
	if len(tok) != TokenLength || TokenLength != 54 {
		t.Fatalf("token is %d characters, the format says 54", len(tok))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{54}$`).MatchString(tok) {
		t.Fatalf("token is not 54 characters of the URL-safe alphabet without padding: %q", tok)
	}

	// the format, computed here by code that does not call the package: item id (16 raw bytes) || HMAC-SHA256(secret, "unsub|v1|" + id)[:24], base64url without padding
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 40 {
		t.Fatalf("token does not decode to 40 bytes: %d bytes, %v", len(raw), err)
	}
	wantID := []byte{0x0f, 0x6c, 0x1a, 0x52, 0x9b, 0x7e, 0x4d, 0x1c, 0x8a, 0x35, 0x2e, 0x7b, 0x94, 0xc0, 0xd1, 0xaa}
	if !bytes.Equal(raw[:16], wantID) {
		t.Fatalf("the first 16 bytes are not the item id's raw bytes: %x", raw[:16])
	}
	m := hmac.New(sha256.New, secretA)
	m.Write([]byte("unsub|v1|" + itemOne))
	if !bytes.Equal(raw[16:], m.Sum(nil)[:24]) {
		t.Fatalf("the last 24 bytes are not the first 24 bytes of HMAC-SHA256(secret, \"unsub|v1|\" + item id)")
	}

	// the round trip, over many ids
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		var b [16]byte
		rng.Read(b[:])
		id := formatUUID(b)
		got, ok := Verify(secretA, mustToken(t, secretA, id))
		if !ok || got != id {
			t.Fatalf("round trip of %s gave (%q, %v)", id, got, ok)
		}
	}

	// control: the same token under the independent computation of ANOTHER item is different, so the equality above is not vacuous
	if mustToken(t, secretA, itemTwo) == tok {
		t.Fatal("control failed: two items have the same token")
	}
}

func TestToken_IsDeterministicAndDependsOnItemAndSecret(t *testing.T) {
	a1, a2 := mustToken(t, secretA, itemOne), mustToken(t, secretA, itemOne)
	if a1 != a2 {
		t.Fatal("the same item under the same secret gave two tokens: a replay of the engine's insert would change what an earlier step wrote")
	}
	if a1 == mustToken(t, secretA, itemTwo) {
		t.Fatal("two items share a token")
	}
	if a1 == mustToken(t, secretB, itemOne) {
		t.Fatal("two secrets give the same token")
	}
}

func TestVerify_EverySingleBitFlipOfTheDecodedTokenIsRefused(t *testing.T) {
	tok := mustToken(t, secretA, itemOne)
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}
	// control: the untouched token verifies, and re-encoding the untouched bytes gives the token back
	if id, ok := Verify(secretA, base64.RawURLEncoding.EncodeToString(raw)); !ok || id != itemOne {
		t.Fatalf("control failed: the unflipped token does not verify: (%q, %v)", id, ok)
	}
	flips := 0
	for i := range raw {
		for bit := 0; bit < 8; bit++ {
			mut := append([]byte(nil), raw...)
			mut[i] ^= 1 << bit
			flips++
			if id, ok := Verify(secretA, base64.RawURLEncoding.EncodeToString(mut)); ok || id != "" {
				t.Fatalf("flipping bit %d of byte %d still verifies as %q", bit, i, id)
			}
		}
	}
	if flips != 320 {
		t.Fatalf("flipped %d bits, the token has 320", flips)
	}
}

func TestVerify_EverySingleBitFlipOfTheTextIsRefused(t *testing.T) {
	tok := mustToken(t, secretA, itemOne)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	flips := 0
	for i := 0; i < len(tok); i++ {
		idx := strings.IndexByte(alphabet, tok[i])
		for bit := 0; bit < 6; bit++ {
			mut := []byte(tok)
			mut[i] = alphabet[idx^(1<<bit)]
			flips++
			if id, ok := Verify(secretA, string(mut)); ok || id != "" {
				t.Fatalf("flipping bit %d of character %d still verifies as %q", bit, i, id)
			}
		}
	}
	if flips != 54*6 {
		t.Fatalf("flipped %d bits of the text, want %d", flips, 54*6)
	}
	// control: the same loop without a flip verifies, so the refusals above are the flips' doing
	if _, ok := Verify(secretA, tok); !ok {
		t.Fatal("control failed: the unaltered token is refused")
	}
}

func TestVerify_NonCanonicalTailDoesNotAlias(t *testing.T) {
	tok := mustToken(t, secretA, itemOne)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, tok[len(tok)-1])
	if last%4 != 0 {
		t.Fatalf("the last character of a canonical 40-byte token carries two zero bits; got index %d", last)
	}
	lenient := base64.RawURLEncoding // the standard decoder, which ignores the spare bits
	want, _ := lenient.DecodeString(tok)
	aliases := 0
	for low := 1; low < 4; low++ {
		alias := tok[:len(tok)-1] + string(alphabet[last|low])
		got, err := lenient.DecodeString(alias)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("control failed: a lenient decoder does not read %q as the same bytes (%v)", alias, err)
		}
		aliases++
		if id, ok := Verify(secretA, alias); ok || id != "" {
			t.Fatalf("a token that differs only in the spare bits of its last character verifies as %q: the decoding is not canonical", id)
		}
	}
	if aliases != 3 {
		t.Fatalf("tried %d aliases, want 3", aliases)
	}
}

func TestVerify_TruncationAndExtensionAreRefused(t *testing.T) {
	tok := mustToken(t, secretA, itemOne)
	for n := 0; n < len(tok); n++ {
		if _, ok := Verify(secretA, tok[:n]); ok {
			t.Fatalf("the first %d characters of a token verify", n)
		}
		if _, ok := Verify(secretA, tok[n:]); ok && n > 0 {
			t.Fatalf("the token without its first %d characters verifies", n)
		}
	}
	for _, ext := range []string{"A", "=", "AA", "AAAA", "\n", " ", "/", "?x=1", strings.Repeat("A", 4096), tok} {
		if _, ok := Verify(secretA, tok+ext); ok {
			t.Fatalf("the token extended by %q verifies", ext)
		}
		if _, ok := Verify(secretA, ext+tok); ok {
			t.Fatalf("the token with %q in front verifies", ext)
		}
	}
	// control: the whole token verifies
	if _, ok := Verify(secretA, tok); !ok {
		t.Fatal("control failed: the whole token is refused")
	}
}

func TestVerify_ATokenMintedUnderAnotherSecretIsRefused(t *testing.T) {
	other := mustToken(t, secretB, itemOne)
	if id, ok := Verify(secretA, other); ok || id != "" {
		t.Fatalf("a token minted under another secret verifies as %q", id)
	}
	// controls: each token verifies under its own secret, and a secret that differs in one byte is another secret
	if _, ok := Verify(secretB, other); !ok {
		t.Fatal("control failed: the token is refused under the secret that minted it")
	}
	near := append([]byte(nil), secretA...)
	near[len(near)-1] ^= 1
	if _, ok := Verify(near, mustToken(t, secretA, itemOne)); ok {
		t.Fatal("a secret that differs in one bit verifies a token of the other")
	}
}

func TestVerify_HostileInputsAreAllTheSameRefusal(t *testing.T) {
	good := mustToken(t, secretA, itemOne)
	std := base64.StdEncoding.EncodeToString(mustRaw(t, good)) // the standard alphabet, with its padding: 56 characters
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"one space", " "},
		{"a NUL", "\x00"},
		{"54 NULs", strings.Repeat("\x00", 54)},
		{"54 spaces", strings.Repeat(" ", 54)},
		{"54 As (a well-formed shape with a wrong MAC)", strings.Repeat("A", 54)},
		{"54 underscores", strings.Repeat("_", 54)},
		{"the token with padding in place of the last character", good[:53] + "="},
		{"the token with padding appended and one character fewer", good[:53] + "=="},
		{"leading space", " " + good[:53]},
		{"trailing space", good[:53] + " "},
		{"leading space and the whole token", " " + good},
		{"trailing newline", good + "\n"},
		{"trailing newline in place of the last character", good[:53] + "\n"},
		{"a newline inside (the standard decoder skips it)", good[:20] + "\n" + good[20:53]},
		{"a carriage return inside", good[:20] + "\r" + good[20:53]},
		{"a CRLF inside", good[:20] + "\r\n" + good[20:52]},
		{"a tab inside", good[:20] + "\t" + good[21:]},
		{"a plus (standard alphabet)", good[:10] + "+" + good[11:]},
		{"a slash (standard alphabet)", good[:10] + "/" + good[11:]},
		{"a percent-encoded character", good[:10] + "%41" + good[13:]},
		{"a dot", good[:10] + "." + good[11:]},
		{"a multi-byte character in place of one", good[:10] + "é" + good[12:]},
		{"upper-cased", strings.ToUpper(good)},
		{"lower-cased", strings.ToLower(good)},
		{"standard alphabet with padding", std},
		{"the item id's text", itemOne},
		{"the item id's text without hyphens", strings.ReplaceAll(itemOne, "-", "")},
		{"the link", "https://example.test/u/" + good},
		{"a path", "/u/" + good},
		{"two tokens", good + good},
		{"a token and a query", good + "?a=b"},
		{"a long string", strings.Repeat("A", 1<<20)},
		{"SQL", "' OR '1'='1"},
		{"a real token of the all-zero item", mustToken(t, secretA, "00000000-0000-0000-0000-000000000000")},
	}
	// the last row is a REAL token of the all-zero item under secretA: it must verify (it is the control that the stand-in of a malformed token is not special)
	ctl := cases[len(cases)-1]
	cases = cases[:len(cases)-1]
	for _, c := range cases {
		id, ok := Verify(secretA, c.in)
		if ok || id != "" {
			t.Errorf("%s: Verify = (%q, %v), want (\"\", false)", c.name, id, ok)
		}
	}
	if len(cases) < 30 {
		t.Fatalf("the hostile table shrank to %d rows", len(cases))
	}
	if id, ok := Verify(secretA, ctl.in); !ok || id != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("control failed: a real token of the all-zero item is refused: (%q, %v), so the stand-in a malformed token is judged against could be mistaken for one", id, ok)
	}
	if _, ok := Verify(secretA, good); !ok {
		t.Fatal("control failed: the good token is refused")
	}
}

func mustRaw(t *testing.T, tok string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerify_RandomInputsNeverVerifyAndNeverPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	verified := 0
	for i := 0; i < 20000; i++ {
		n := 54
		if i%3 == 0 {
			n = rng.Intn(120)
		}
		b := make([]byte, n)
		for j := range b {
			if i%5 == 0 {
				b[j] = byte(rng.Intn(256))
			} else {
				b[j] = alphabet[rng.Intn(len(alphabet))]
			}
		}
		if _, ok := Verify(secretA, string(b)); ok {
			verified++
		}
	}
	if verified != 0 {
		t.Fatalf("%d of 20000 random strings verified: a 192-bit MAC was guessed", verified)
	}
	// control: a token for a random item always verifies under the same loop's conditions
	for i := 0; i < 200; i++ {
		var b [16]byte
		rng.Read(b[:])
		if _, ok := Verify(secretA, mustToken(t, secretA, formatUUID(b))); !ok {
			t.Fatal("control failed: a minted token is refused")
		}
	}
}

func TestSecret_ShorterThan32BytesIsRefusedEverywhere(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31} {
		short := key("s", n)
		if _, err := Token(short, itemOne); !errors.Is(err, ErrWeakSecret) {
			t.Errorf("Token with a %d-byte secret: %v, want ErrWeakSecret", n, err)
		}
		if id, ok := Verify(short, mustToken(t, secretA, itemOne)); ok || id != "" {
			t.Errorf("Verify with a %d-byte secret answered (%q, %v)", n, id, ok)
		}
		if s, err := NewSigner(short); !errors.Is(err, ErrWeakSecret) || s != nil {
			t.Errorf("NewSigner with a %d-byte secret: (%v, %v), want ErrWeakSecret and no signer", n, s, err)
		}
	}
	if _, err := Token(nil, itemOne); !errors.Is(err, ErrWeakSecret) {
		t.Errorf("Token with a nil secret: %v", err)
	}
	// the twins: exactly 32 bytes is accepted
	s32 := key("t", 32)
	if tok, err := Token(s32, itemOne); err != nil || len(tok) != 54 {
		t.Fatalf("a 32-byte secret was refused: %v", err)
	}
	if _, err := NewSigner(s32); err != nil {
		t.Fatalf("a 32-byte secret was refused by NewSigner: %v", err)
	}
	if MinSecretBytes != 32 {
		t.Fatalf("MinSecretBytes = %d, the spec says 32", MinSecretBytes)
	}
}

func TestToken_RefusesAnItemIDThatIsNotTheCanonicalLowerCaseUUID(t *testing.T) {
	for _, bad := range []string{
		"", "x", strings.ToUpper(itemOne), strings.ReplaceAll(itemOne, "-", ""), "{" + itemOne + "}", "urn:uuid:" + itemOne, " " + itemOne, itemOne + " ", itemOne + "\n",
		itemOne[:35], itemOne + "0", "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aG", "0f6c1a52_9b7e_4d1c_8a35_2e7b94c0d1aa", "0F6C1A52-9b7e-4d1c-8a35-2e7b94c0d1aa",
	} {
		if tok, err := Token(secretA, bad); !errors.Is(err, ErrBadItemID) || tok != "" {
			t.Errorf("Token(%q) = (%q, %v), want ErrBadItemID", bad, tok, err)
		}
	}
	// the twin: the canonical form works, and what Verify returns is always the canonical form (the MAC's text)
	id, ok := Verify(secretA, mustToken(t, secretA, itemOne))
	if !ok || id != itemOne || id != strings.ToLower(id) {
		t.Fatalf("control failed: (%q, %v)", id, ok)
	}
}

// TestVerify_UsesAConstantTimeComparison reads the source: the MAC is compared with hmac.Equal and with no == or bytes.Equal on the MAC. The instrument is run over a copy that was made naive
// (the control), which it must refuse.
func TestVerify_UsesAConstantTimeComparison(t *testing.T) {
	src, err := os.ReadFile("token.go")
	if err != nil {
		t.Fatalf("cannot read token.go (a test that cannot run must fail): %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if problems := nonConstantTimeUses(text); len(problems) != 0 {
		t.Fatalf("token.go compares the MAC in a way that is not constant time: %v", problems)
	}
	naive := strings.Replace(text, "hmac.Equal(got, want)", "bytes.Equal(got, want)", 1)
	if naive == text {
		t.Fatal("control failed: the line the control edits is not in the source")
	}
	if problems := nonConstantTimeUses(naive); len(problems) == 0 {
		t.Fatal("control failed: a source that compares with bytes.Equal was not flagged")
	}
	naive2 := strings.Replace(text, "hmac.Equal(got, want)", "string(got) == string(want)", 1)
	if problems := nonConstantTimeUses(naive2); len(problems) == 0 {
		t.Fatal("control failed: a source that compares with == was not flagged")
	}
}

// nonConstantTimeUses returns what in the source compares a MAC without a constant-time function: any bytes.Equal, bytes.Compare, subtle-less == on got/want, and the absence of hmac.Equal.
func nonConstantTimeUses(src string) []string {
	var out []string
	if !strings.Contains(src, "hmac.Equal(got, want)") {
		out = append(out, "no hmac.Equal(got, want)")
	}
	for _, bad := range []string{"bytes.Equal(", "bytes.Compare(", "string(got) ==", "string(want) ==", "== string(want)", "== string(got)", "got == want", "want == got"} {
		if strings.Contains(src, bad) {
			out = append(out, "uses "+bad)
		}
	}
	return out
}

func TestLink_IsTheBaseURLThePrefixAndTheToken(t *testing.T) {
	tok := mustToken(t, secretA, itemOne)
	if got, want := Link("https://app.example.test", tok), "https://app.example.test/u/"+tok; got != want {
		t.Fatalf("Link = %q, want %q", got, want)
	}
	if got, want := Link("https://app.example.test/mail", tok), "https://app.example.test/mail/u/"+tok; got != want {
		t.Fatalf("Link with a path = %q, want %q", got, want)
	}
	// a trailing slash is dropped (the link never holds //u/); the control is the same base without it
	if a, b := Link("https://app.example.test/", tok), Link("https://app.example.test", tok); a != b || strings.Contains(a, "//u/") {
		t.Fatalf("a trailing slash changed the link: %q vs %q", a, b)
	}
	if PathPrefix != "/u/" {
		t.Fatalf("PathPrefix = %q", PathPrefix)
	}
}

func TestSigner_LinkMakerMakesLinksThatVerifyAndSatisfyTheColumnCheck(t *testing.T) {
	s, err := NewSigner(secretA)
	if err != nil {
		t.Fatal(err)
	}
	mk := s.LinkMaker("https://app.example.test")
	// the shape platform_0017's CHECK demands: https, no white space, no < or >, at most 2000 characters
	check := regexp.MustCompile(`^https://[^\s<>]+$`)
	for _, id := range []string{itemOne, itemTwo, "00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		link, err := mk(id)
		if err != nil {
			t.Fatal(err)
		}
		if !check.MatchString(link) || len(link) > 2000 || strings.ContainsAny(link, "\"'\\`") {
			t.Fatalf("link %q does not satisfy the column's CHECK", link)
		}
		tok := strings.TrimPrefix(link, "https://app.example.test/u/")
		if got, ok := s.Verify(tok); !ok || got != id {
			t.Fatalf("the token of %q does not verify: (%q, %v)", link, got, ok)
		}
	}
	if _, err := mk("not-a-uuid"); !errors.Is(err, ErrBadItemID) {
		t.Fatalf("the link maker accepted a bad item id: %v", err)
	}
}

func TestSigner_CopiesItsSecret(t *testing.T) {
	secret := key("copy-", 40)
	s, err := NewSigner(secret)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Token(itemOne)
	for i := range secret {
		secret[i] = 'x'
	}
	after, _ := s.Token(itemOne)
	if before != after {
		t.Fatal("changing the slice the signer was built from changed its tokens")
	}
	// control: the package function reads the slice it is given, so the same mutation changes ITS tokens
	if direct, _ := Token(secret, itemOne); direct == before {
		t.Fatal("control failed: mutating the secret did not change the token the package function makes")
	}
}
