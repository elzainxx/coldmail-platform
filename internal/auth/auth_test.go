package auth

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func verify(v *Verifier, tok string) (Claims, error) { return v.Verify(context.Background(), tok) }

func TestVerify_ValidTokenIsAcceptedForEachAlgorithm(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	full := with(claimsAt(now), "nbf", now.Unix()-5, "exp", float64(now.Unix())+3600.5) // a fractional exp is a valid NumericDate
	for _, f := range w.fixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			got, err := verify(f.v, f.mint(t, full))
			if err != nil {
				t.Fatalf("a valid %s token was refused: %v", f.name, err)
			}
			if got.Subject != testSub || got.Issuer != testIssuer || len(got.Audience) != 1 || got.Audience[0] != "authenticated" ||
				got.Role != "authenticated" || got.Email != "someone@example.test" || got.SessionID != testSub2 {
				t.Errorf("the claims were decoded wrongly: %+v", got)
			}
			if !got.ExpiresAt.Equal(now.Add(3600*time.Second+500*time.Millisecond)) || !got.NotBefore.Equal(now.Add(-5*time.Second)) || !got.IssuedAt.Equal(now.Add(-10*time.Second)) {
				t.Errorf("the times were decoded wrongly: exp %v nbf %v iat %v", got.ExpiresAt, got.NotBefore, got.IssuedAt)
			}
			// The array form of aud is accepted and carried whole.
			got, err = verify(f.v, f.mint(t, with(claimsAt(now), "aud", []string{"other", "authenticated"})))
			if err != nil || len(got.Audience) != 2 || got.Audience[1] != "authenticated" {
				t.Errorf("an aud array holding the audience: %+v, %v", got, err)
			}
			// A token without nbf, iat, role, email and session id is valid, and those claims are simply empty.
			got, err = verify(f.v, f.mint(t, with(claimsAt(now), "nbf", nil, "iat", nil, "role", nil, "email", nil, "session_id", nil)))
			if err != nil || !got.NotBefore.IsZero() || !got.IssuedAt.IsZero() || got.Role != "" || got.Email != "" || got.SessionID != "" {
				t.Errorf("a token without the optional claims: %+v, %v", got, err)
			}
			// A claim of the wrong type that is only carried is not carried, and does not fail the token.
			got, err = verify(f.v, f.mint(t, with(claimsAt(now), "role", 7, "email", []string{"x"})))
			if err != nil || got.Role != "" || got.Email != "" {
				t.Errorf("a role of the wrong type: %+v, %v", got, err)
			}
		})
	}
	// A verifier with both sources takes each algorithm.
	for _, f := range w.fixtures(t) {
		if _, err := verify(w.bothV, f.mint(t, claimsAt(now))); err != nil {
			t.Errorf("the verifier with both sources refused a valid %s token: %v", f.name, err)
		}
	}
	// A nil context is tolerated.
	if _, err := w.hmacV.Verify(nil, w.fixtures(t)[0].mint(t, claimsAt(now))); err != nil {
		t.Errorf("a nil context: %v", err)
	}
}

func TestVerify_TimeWindowAtItsExactBoundaries(t *testing.T) {
	sec := testNow.Unix()
	secF := float64(sec)
	cases := []struct {
		name   string
		leeway time.Duration
		claims map[string]any
		want   error
	}{
		// exp, no leeway: the instant itself is already expired.
		{"exp == now", NoLeeway, with(claimsAt(testNow), "exp", sec), ErrExpired},
		{"exp one second ahead", NoLeeway, with(claimsAt(testNow), "exp", sec+1), nil},
		{"exp one second behind", NoLeeway, with(claimsAt(testNow), "exp", sec-1), ErrExpired},
		{"exp half a second ahead", NoLeeway, with(claimsAt(testNow), "exp", secF+0.5), nil},
		{"exp half a second behind", NoLeeway, with(claimsAt(testNow), "exp", secF-0.5), ErrExpired},
		// exp with the default leeway of 30 s: expired exactly when now >= exp + 30 s.
		{"default leeway: exp 30 s behind", 0, with(claimsAt(testNow), "exp", sec-30), ErrExpired},
		{"default leeway: exp 29 s behind", 0, with(claimsAt(testNow), "exp", sec-29), nil},
		{"default leeway: exp 31 s behind", 0, with(claimsAt(testNow), "exp", sec-31), ErrExpired},
		{"leeway 5 s: exp 5 s behind", 5 * time.Second, with(claimsAt(testNow), "exp", sec-5), ErrExpired},
		{"leeway 5 s: exp 4 s behind", 5 * time.Second, with(claimsAt(testNow), "exp", sec-4), nil},
		{"leeway 5 min: exp 300 s behind", MaxLeeway, with(claimsAt(testNow), "exp", sec-300), ErrExpired},
		{"leeway 5 min: exp 299 s behind", MaxLeeway, with(claimsAt(testNow), "exp", sec-299), nil},
		// nbf: not yet valid when now + leeway < nbf.
		{"nbf == now", NoLeeway, with(claimsAt(testNow), "nbf", sec), nil},
		{"nbf one second ahead", NoLeeway, with(claimsAt(testNow), "nbf", sec+1), ErrNotYetValid},
		{"default leeway: nbf 30 s ahead", 0, with(claimsAt(testNow), "nbf", sec+30), nil},
		{"default leeway: nbf 31 s ahead", 0, with(claimsAt(testNow), "nbf", sec+31), ErrNotYetValid},
		// iat in the future beyond the leeway is not yet valid too.
		{"iat == now", NoLeeway, with(claimsAt(testNow), "iat", sec), nil},
		{"iat one second ahead", NoLeeway, with(claimsAt(testNow), "iat", sec+1), ErrNotYetValid},
		{"default leeway: iat 30 s ahead", 0, with(claimsAt(testNow), "iat", sec+30), nil},
		{"default leeway: iat 31 s ahead", 0, with(claimsAt(testNow), "iat", sec+31), ErrNotYetValid},
		{"iat long ago", NoLeeway, with(claimsAt(testNow), "iat", sec-86400*365), nil},
		// exp is required, and every date is a finite number of seconds in 0 .. 9999-12-31.
		{"exp missing", 0, with(claimsAt(testNow), "exp", nil), ErrMalformed},
		{"exp a string", 0, with(claimsAt(testNow), "exp", fmt.Sprint(sec+3600)), ErrMalformed},
		{"exp null", 0, with(claimsAt(testNow), "exp", rawJSON("null")), ErrMalformed},
		{"exp true", 0, with(claimsAt(testNow), "exp", true), ErrMalformed},
		{"exp an array", 0, with(claimsAt(testNow), "exp", []int{1}), ErrMalformed},
		{"exp negative", 0, with(claimsAt(testNow), "exp", -1), ErrMalformed},
		{"exp overflowing a float", 0, with(claimsAt(testNow), "exp", rawJSON("1e999")), ErrMalformed},
		{"exp 1e20", 0, with(claimsAt(testNow), "exp", rawJSON("1e20")), ErrMalformed},
		{"exp one second past year 9999", 0, with(claimsAt(testNow), "exp", 253402300800), ErrMalformed},
		{"exp the last second of year 9999", 0, with(claimsAt(testNow), "exp", 253402300799), nil},
		{"nbf a string", 0, with(claimsAt(testNow), "nbf", "soon"), ErrMalformed},
		{"iat a string", 0, with(claimsAt(testNow), "iat", "soon"), ErrMalformed},
		{"nbf null", 0, with(claimsAt(testNow), "nbf", rawJSON("null")), ErrMalformed},
	}
	secret := randBytes(t, 40)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := mustNew(t, Options{HMACSecret: secret, Leeway: c.leeway, Now: newClock().Now})
			_, err := verify(v, mint(t, hmacSigner{secret}, "", c.claims))
			wantErr(t, c.name, err, c.want)
		})
	}
	// The exact boundary is the same for the asymmetric algorithms (the check does not depend on the algorithm).
	w := newWorld(t)
	for _, f := range w.fixtures(t) {
		_, err := verify(f.v, f.mint(t, with(claimsAt(testNow), "exp", sec-30)))
		wantErr(t, f.name+": exp 30 s behind under the default leeway", err, ErrExpired)
		_, err = verify(f.v, f.mint(t, with(claimsAt(testNow), "exp", sec-29)))
		wantErr(t, f.name+": exp 29 s behind under the default leeway", err, nil)
	}
}

func TestVerify_ChecksRunInTheDocumentedOrder(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	sec := now.Unix()
	hs := hmacSigner{w.secret}
	bad := with(claimsAt(now), "iss", "https://elsewhere.example.test", "aud", "other", "sub", "not-a-uuid")

	cases := []struct {
		name string
		tok  string
		want error
	}{
		// 4 before 5 before 6.
		{"expired beats wrong issuer, audience and subject", mint(t, hs, "", with(bad, "exp", sec-3600)), ErrExpired},
		{"not yet valid beats wrong issuer", mint(t, hs, "", with(bad, "nbf", sec+3600)), ErrNotYetValid},
		{"wrong issuer beats wrong audience and subject", mint(t, hs, "", bad), ErrIssuer},
		{"wrong audience beats wrong subject", mint(t, hs, "", with(bad, "iss", testIssuer)), ErrAudience},
		{"wrong subject alone", mint(t, hs, "", with(bad, "iss", testIssuer, "aud", "authenticated")), ErrSubject},
		{"the twin with nothing wrong", mint(t, hs, "", claimsAt(now)), nil},
		// 3 before 4: a forged token is a signature failure whatever it claims.
		{"bad signature beats expired", mint(t, hmacSigner{randBytes(t, 40)}, "", with(claimsAt(now), "exp", sec-3600)), ErrSignature},
		// 2 before 3, and 1 before 2.
		{"bad algorithm beats bad signature", mintWith(t, hmacSigner{randBytes(t, 40)}, map[string]any{"alg": "HS384"}, claimsAt(now)), ErrAlgorithm},
		{"malformed payload beats bad algorithm", enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`[1]`)) + ".", ErrMalformed},
		// the key lookup (3) is before the time checks (4).
		{"ES256: unknown kid beats expired", mint(t, ecSigner{t, w.ec}, "nobody", with(claimsAt(now), "exp", sec-3600)), ErrKey},
		{"ES256: the twin with a known kid is expired", mint(t, ecSigner{t, w.ec}, ecKID, with(claimsAt(now), "exp", sec-3600)), ErrExpired},
	}
	for _, c := range cases {
		v := w.bothV
		_, err := verify(v, c.tok)
		wantErr(t, c.name, err, c.want)
	}
}

func TestVerify_IssuerAndAudience(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	hs := hmacSigner{w.secret}
	noIssuer := mustNew(t, Options{HMACSecret: w.secret, Now: w.clk.Now})
	platformAud := mustNew(t, Options{HMACSecret: w.secret, Audience: "platform", Now: w.clk.Now})

	issuer := []struct {
		name   string
		claims map[string]any
		pinned error // what the verifier that pins the issuer says
		open   error // what the verifier that does not says
	}{
		{"the configured issuer", claimsAt(now), nil, nil},
		{"another issuer", with(claimsAt(now), "iss", "https://elsewhere.example.test/auth/v1"), ErrIssuer, nil},
		{"the issuer with a trailing slash", with(claimsAt(now), "iss", testIssuer+"/"), ErrIssuer, nil},
		{"the issuer in upper case", with(claimsAt(now), "iss", strings.ToUpper(testIssuer)), ErrIssuer, nil},
		{"the issuer with a space after it", with(claimsAt(now), "iss", testIssuer+" "), ErrIssuer, nil},
		{"no issuer claim", with(claimsAt(now), "iss", nil), ErrIssuer, nil},
		{"an empty issuer", with(claimsAt(now), "iss", ""), ErrIssuer, nil},
		{"an issuer that is not a string", with(claimsAt(now), "iss", 12), ErrIssuer, ErrIssuer},
		{"an issuer that is an array holding the right one", with(claimsAt(now), "iss", []string{testIssuer}), ErrIssuer, ErrIssuer},
	}
	for _, c := range issuer {
		tok := mint(t, hs, "", c.claims)
		_, err := verify(w.hmacV, tok)
		wantErr(t, "issuer pinned: "+c.name, err, c.pinned)
		_, err = verify(noIssuer, tok)
		wantErr(t, "issuer not pinned: "+c.name, err, c.open)
	}

	audience := []struct {
		name string
		aud  any // nil removes the claim
		def  error
		plat error
	}{
		{"the default audience as a string", "authenticated", nil, ErrAudience},
		{"the default audience in an array", []string{"authenticated"}, nil, ErrAudience},
		{"the default audience among others", []string{"a", "authenticated", "b"}, nil, ErrAudience},
		{"the platform audience as a string", "platform", ErrAudience, nil},
		{"the platform audience in an array", []string{"x", "platform"}, ErrAudience, nil},
		{"both audiences", []string{"platform", "authenticated"}, nil, nil},
		{"another audience as a string", "other", ErrAudience, ErrAudience},
		{"another audience in an array", []string{"a", "b"}, ErrAudience, ErrAudience},
		{"an empty array", []string{}, ErrAudience, ErrAudience},
		{"an empty string", "", ErrAudience, ErrAudience},
		{"the audience in a different case", "Authenticated", ErrAudience, ErrAudience},
		{"the audience with a trailing space", "authenticated ", ErrAudience, ErrAudience},
		{"no audience claim", nil, ErrAudience, ErrAudience},
		{"an audience that is a number", 5, ErrAudience, ErrAudience},
		{"an audience that is null", rawJSON("null"), ErrAudience, ErrAudience},
		{"an array holding the audience and a number", rawJSON(`["authenticated", 5]`), ErrAudience, ErrAudience},
		{"an object", rawJSON(`{"authenticated":true}`), ErrAudience, ErrAudience},
	}
	for _, c := range audience {
		tok := mint(t, hs, "", with(claimsAt(now), "aud", c.aud))
		_, err := verify(w.hmacV, tok)
		wantErr(t, "default audience: "+c.name, err, c.def)
		_, err = verify(platformAud, tok)
		wantErr(t, "audience platform: "+c.name, err, c.plat)
	}
}

func TestVerify_Subject(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	upperLast := testSub[:len(testSub)-1] + strings.ToUpper(testSub[len(testSub)-1:])
	if upperLast == testSub {
		upperLast = testSub[:len(testSub)-2] + "C" + testSub[len(testSub)-1:] // the last digit is not a letter: upper-case another position
	}
	cases := []struct {
		name string
		sub  any // nil removes the claim
		want error
	}{
		{"a canonical lower-case uuid", testSub, nil},
		{"another one", testSub2, nil},
		{"upper case", strings.ToUpper(testSub), ErrSubject},
		{"one upper-case digit", upperLast, ErrSubject},
		{"no hyphens", strings.ReplaceAll(testSub, "-", ""), ErrSubject},
		{"in braces", "{" + testSub + "}", ErrSubject},
		{"as a urn", "urn:uuid:" + testSub, ErrSubject},
		{"a leading space", " " + testSub, ErrSubject},
		{"a trailing space", testSub + " ", ErrSubject},
		{"a trailing newline", testSub + "\n", ErrSubject},
		{"one character short", testSub[:35], ErrSubject},
		{"one character long", testSub + "0", ErrSubject},
		{"a non-hex digit", "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e9g", ErrSubject},
		{"a hyphen in the wrong place", "0b9e1c1e5-d3a-4c53-9f6e-2a1d7c4b8e90", ErrSubject},
		{"an empty string", "", ErrSubject},
		{"an email address", "someone@example.test", ErrSubject},
		{"a number", 123, ErrSubject},
		{"null", rawJSON("null"), ErrSubject},
		{"an array holding a uuid", []string{testSub}, ErrSubject},
		{"an object", map[string]string{"id": testSub}, ErrSubject},
		{"missing", nil, ErrSubject},
	}
	for _, f := range w.fixtures(t) {
		for _, c := range cases {
			_, err := verify(f.v, f.mint(t, with(claimsAt(now), "sub", c.sub)))
			wantErr(t, f.name+": sub "+c.name, err, c.want)
		}
	}
}

func TestVerify_RefusesAMalformedStructure(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	hs := hmacSigner{w.secret}
	good := mint(t, hs, "", claimsAt(now))
	h, p, s := segments(good)
	if _, err := verify(w.hmacV, good); err != nil {
		t.Fatalf("control failed: the good token is refused: %v", err)
	}

	// flipLow flips the lowest bit of the last character of a segment: the bits a canonical encoder leaves at zero, so the decoded bytes do not change.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	flipLow := strings.IndexByte(alphabet, s[len(s)-1])
	nonCanonical := s[:len(s)-1] + string(alphabet[flipLow^1])

	// A raw body with a valid HMAC over exactly the segments sent: only the structure is wrong.
	rawTok := func(header, payload string) string { return mintRaw(hs, header, payload) }
	goodHeader, goodPayload := `{"alg":"HS256","typ":"JWT"}`, fmt.Sprintf(`{"sub":%q,"aud":"authenticated","iss":%q,"exp":%d}`, testSub, testIssuer, now.Unix()+3600)

	cases := []struct {
		name string
		tok  string
		want error
	}{
		{"the twin, written as raw JSON", rawTok(goodHeader, goodPayload), nil},
		{"empty", "", ErrMalformed},
		{"one space", " ", ErrMalformed},
		{"a leading space", " " + good, ErrMalformed},
		{"a trailing space", good + " ", ErrMalformed},
		{"a trailing newline", good + "\n", ErrMalformed},
		{"a newline inside the signature", h + "." + p + "." + s[:10] + "\n" + s[10:], ErrMalformed},
		{"a tab inside the payload", h + "." + p[:5] + "\t" + p[5:] + "." + s, ErrMalformed},
		{"the Bearer prefix", "Bearer " + good, ErrMalformed},
		{"one segment", h, ErrMalformed},
		{"two segments", h + "." + p, ErrMalformed},
		{"four segments", good + "." + s, ErrMalformed},
		{"five segments", good + ".a.b", ErrMalformed},
		{"only dots", "..", ErrMalformed},
		{"an empty header", "." + p + "." + s, ErrMalformed},
		{"an empty payload", h + ".." + s, ErrMalformed},
		{"the header cut in half", h[:len(h)/2] + "." + p + "." + s, ErrMalformed},
		{"the payload cut in half", h + "." + p[:len(p)/2] + "." + s, ErrMalformed},
		{"the token cut inside the payload", good[:len(h)+1+len(p)/2], ErrMalformed},
		{"the token cut after the header", good[:len(h)+1], ErrMalformed},
		{"the token cut after the payload, no signature segment", h + "." + p, ErrMalformed},
		{"the signature cut to 40 characters (well formed, wrong)", h + "." + p + "." + s[:40], ErrSignature},
		{"an empty signature segment on a signed algorithm", h + "." + p + ".", ErrSignature},
		{"characters outside the alphabet", "!!!." + p + "." + s, ErrMalformed},
		{"padding on the header", h + "=." + p + "." + s, ErrMalformed},
		{"padding on the signature", h + "." + p + "." + s + "=", ErrMalformed},
		{"the standard alphabet (a plus sign)", h + "." + p + "." + s[:5] + "+" + s[6:], ErrMalformed},
		{"the standard alphabet (a slash)", h + "." + p + "." + s[:5] + "/" + s[6:], ErrMalformed},
		{"a non-ASCII character", h + "." + p + ".é" + s, ErrMalformed},
		{"a NUL", h + "." + p + ".\x00" + s, ErrMalformed},
		{"trailing bits that are not zero (a second spelling of the same signature)", h + "." + p + "." + nonCanonical, ErrMalformed},
		{"a header that is an array", rawTok(`[]`, goodPayload), ErrMalformed},
		{"a header that is a string", rawTok(`"HS256"`, goodPayload), ErrMalformed},
		{"a header that is null", rawTok(`null`, goodPayload), ErrMalformed},
		{"a header that is a number", rawTok(`1`, goodPayload), ErrMalformed},
		{"a header that is not JSON", rawTok(`alg=HS256`, goodPayload), ErrMalformed},
		{"a header with a trailing comma", rawTok(`{"alg":"HS256",}`, goodPayload), ErrMalformed},
		{"a header with text after the object", rawTok(`{"alg":"HS256"} x`, goodPayload), ErrMalformed},
		{"a header with a second object after the first", rawTok(`{"alg":"HS256"}{}`, goodPayload), ErrMalformed},
		{"a payload that is an array", rawTok(goodHeader, `[]`), ErrMalformed},
		{"a payload that is a string", rawTok(goodHeader, `"x"`), ErrMalformed},
		{"a payload that is null", rawTok(goodHeader, `null`), ErrMalformed},
		{"an empty payload text", rawTok(goodHeader, ``), ErrMalformed},
		{"a repeated alg in the header (even with the same value)", rawTok(`{"alg":"HS256","alg":"HS256"}`, goodPayload), ErrMalformed},
		{"a repeated alg in the header (a different value)", rawTok(`{"alg":"HS256","alg":"none"}`, goodPayload), ErrMalformed},
		{"a repeated sub in the payload", rawTok(goodHeader, fmt.Sprintf(`{"sub":%q,"sub":%q,"aud":"authenticated","iss":%q,"exp":%d}`, testSub, testSub2, testIssuer, now.Unix()+3600)), ErrMalformed},
		{"a header with no alg is an algorithm problem, not a structure one", rawTok(`{}`, goodPayload), ErrAlgorithm},
		{"a member name in another case is a different member", rawTok(`{"Alg":"HS256"}`, goodPayload), ErrAlgorithm},
	}
	for _, c := range cases {
		_, err := verify(w.hmacV, c.tok)
		wantErr(t, c.name, err, c.want)
	}

	// The size cap: 8192 bytes pass, one more does not. The token grows with a padding claim; both sides are signed, so only the size differs.
	padded := func(n int) string {
		return mint(t, hs, "", with(claimsAt(now), "pad", strings.Repeat("x", n)))
	}
	var under, over string
	for n := 5000; n < 9000; n++ {
		tok := padded(n)
		if len(tok) <= MaxTokenBytes {
			under = tok
			continue
		}
		over = tok
		break
	}
	if len(under) < MaxTokenBytes-4 || len(over) <= MaxTokenBytes {
		t.Fatalf("control failed: the sizes around the cap were not found (%d, %d)", len(under), len(over))
	}
	if _, err := verify(w.hmacV, under); err != nil {
		t.Errorf("a token of %d bytes (the largest under the cap) was refused: %v", len(under), err)
	}
	_, err := verify(w.hmacV, over)
	wantErr(t, fmt.Sprintf("a token of %d bytes", len(over)), err, ErrMalformed)
	_, err = verify(w.hmacV, strings.Repeat("A", 1<<20))
	wantErr(t, "a token of one MiB", err, ErrMalformed)
}

func TestVerify_TamperedTokensAreRefused(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	otherSecret := randBytes(t, 48)
	for _, f := range w.fixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			good := f.mint(t, claimsAt(now))
			h, p, s := segments(good)
			payload := mustDecode(t, p)
			sigBytes := mustDecode(t, s)

			// One bit of the payload: the email's first character. Re-encoding the payload unchanged is the twin that proves the procedure itself is sound.
			at := strings.Index(string(payload), `"email":"`) + len(`"email":"`)
			flipped := append([]byte(nil), payload...)
			flipped[at] ^= 0x01
			if got, err := verify(f.v, h+"."+enc(payload)+"."+s); err != nil {
				t.Fatalf("control failed: the payload re-encoded unchanged is refused: %v", err)
			} else if got.Email != "someone@example.test" {
				t.Fatalf("control failed: %+v", got)
			}
			_, err := verify(f.v, h+"."+enc(flipped)+"."+s)
			wantErr(t, "one bit of the payload", err, ErrSignature)

			// A longer life: exp pushed out a year, signature kept.
			longer := strings.Replace(string(payload), fmt.Sprintf(`"exp":%d`, now.Unix()+3600), fmt.Sprintf(`"exp":%d`, now.Unix()+3600+31536000), 1)
			if longer == string(payload) {
				t.Fatal("control failed: the exp was not found in the payload")
			}
			_, err = verify(f.v, h+"."+enc([]byte(longer))+"."+s)
			wantErr(t, "an exp pushed a year out", err, ErrSignature)

			// Another user: sub swapped, signature kept.
			_, err = verify(f.v, h+"."+enc([]byte(strings.Replace(string(payload), testSub, testSub2, 1)))+"."+s)
			wantErr(t, "another subject", err, ErrSignature)

			// One bit of the signature, every byte position that matters (first, middle, last).
			for _, i := range []int{0, len(sigBytes) / 2, len(sigBytes) - 1} {
				b := append([]byte(nil), sigBytes...)
				b[i] ^= 0x80
				_, err = verify(f.v, h+"."+p+"."+enc(b))
				wantErr(t, fmt.Sprintf("a bit of signature byte %d", i), err, ErrSignature)
			}
			// One bit of the header (the typ), signature kept.
			_, err = verify(f.v, enc([]byte(strings.Replace(string(mustDecode(t, h)), "JWT", "JWS", 1)))+"."+p+"."+s)
			wantErr(t, "a changed header member", err, ErrSignature)

			// The signature of another token (a valid signature, of other bytes).
			_, err = verify(f.v, h+"."+p+"."+segment3(f.mint(t, with(claimsAt(now), "role", "service_role"))))
			wantErr(t, "the signature of another token", err, ErrSignature)
			// An empty signature, a short one and a long one.
			for name, sig := range map[string][]byte{"empty": nil, "one byte": {1}, "31 bytes": sigBytes[:31], "padded by a byte": append(append([]byte(nil), sigBytes...), 0)} {
				_, err = verify(f.v, h+"."+p+"."+enc(sig))
				wantErr(t, "a signature that is "+name, err, ErrSignature)
			}
		})
	}

	// A well-formed signature from the wrong key, under the right kid.
	w2 := ecKey(t)
	for name, tok := range map[string]string{
		"HS256 under another secret":    mint(t, hmacSigner{otherSecret}, "", claimsAt(now)),
		"ES256 under another EC key":    mint(t, ecSigner{t, w2}, ecKID, claimsAt(now)),
		"RS256 under another RSA key":   mint(t, rsaSigner{t, rsaKey(t, 1)}, rsaKID, claimsAt(now)),
		"RS256 under the EC key's kid":  mint(t, rsaSigner{t, w.rsa}, ecKID, claimsAt(now)),
		"ES256 under the RSA key's kid": mint(t, ecSigner{t, w.ec}, rsaKID, claimsAt(now)),
	} {
		_, err := verify(w.bothV, tok)
		if !errors.Is(err, ErrSignature) && !errors.Is(err, ErrAlgorithm) {
			t.Errorf("%s: got %v, want a signature or algorithm refusal", name, err)
		}
	}
	// The same, one by one, with the exact refusal: the kid names a key of another type than the algorithm.
	_, err := verify(w.bothV, mint(t, rsaSigner{t, w.rsa}, ecKID, claimsAt(now)))
	wantErr(t, "RS256 under the EC key's kid", err, ErrAlgorithm)
	_, err = verify(w.bothV, mint(t, ecSigner{t, w2}, ecKID, claimsAt(now)))
	wantErr(t, "ES256 under another EC key", err, ErrSignature)

	// ES256 signatures in the wrong shape: the DER form of an ECDSA signature, 63 and 65 bytes, all zero.
	ecTok := mint(t, ecSigner{t, w.ec}, ecKID, claimsAt(now))
	h, p, s := segments(ecTok)
	raw := mustDecode(t, s)
	der := append([]byte{0x30, 0x44, 0x02, 0x20}, raw[:32]...)
	der = append(append(der, 0x02, 0x20), raw[32:]...)
	for name, sig := range map[string][]byte{"DER": der, "63 bytes": raw[:63], "65 bytes": append(append([]byte(nil), raw...), 0), "all zero": make([]byte, 64)} {
		_, err = verify(w.jwksV, h+"."+p+"."+enc(sig))
		wantErr(t, "an ES256 signature that is "+name, err, ErrSignature)
	}
	if _, err = verify(w.jwksV, ecTok); err != nil {
		t.Errorf("control failed: the untouched ES256 token is refused: %v", err)
	}
}

func segment3(tok string) string { _, _, s := segments(tok); return s }

func mustDecode(t testing.TB, seg string) []byte {
	t.Helper()
	b, err := b64.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerify_AlgorithmAllowList(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	claims := claimsAt(now)
	hs := hmacSigner{w.secret}
	verifiers := map[string]*Verifier{"HMAC only": w.hmacV, "JWKS only": w.jwksV, "both sources": w.bothV}

	// Every case is refused as ErrAlgorithm by every verifier: the allow-list is checked before any key is looked at.
	refused := map[string]string{}
	for _, alg := range []string{"none", "None", "NONE", "nOnE", "", " ", "hs256", "Hs256", "HS256 ", " HS256", "rs256", "es256", "RS 256",
		"HS384", "HS512", "RS384", "RS512", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA", "RSA-OAEP", "A128KW", "dir", "HS256\x00"} {
		hdr := map[string]any{"alg": alg, "typ": "JWT"}
		// signed correctly with the HMAC secret under whatever name: only the name is wrong
		refused["alg "+fmt.Sprintf("%q", alg)+" with a valid HS256 MAC"] = mintWith(t, hs, hdr, claims)
		// unsecured: no signature segment, and a junk one
		in := encJSON(t, hdr) + "." + encJSON(t, claims)
		refused["alg "+fmt.Sprintf("%q", alg)+" with no signature"] = in + "."
		refused["alg "+fmt.Sprintf("%q", alg)+" with a junk signature"] = in + "." + enc(randBytes(t, 32))
	}
	for _, h := range []any{nil, 5, true, []string{"HS256"}, map[string]string{"a": "HS256"}, rawJSON("null")} {
		hdr := map[string]any{"alg": h, "typ": "JWT"}
		refused[fmt.Sprintf("alg %v", h)] = mintWith(t, hs, hdr, claims)
	}
	refused["no alg member"] = mintWith(t, hs, map[string]any{"typ": "JWT"}, claims)
	if len(refused) < 70 {
		t.Fatalf("control failed: only %d refusal cases were built", len(refused))
	}
	for name, tok := range refused {
		for vname, v := range verifiers {
			_, err := verify(v, tok)
			wantErr(t, name+" at the "+vname+" verifier", err, ErrAlgorithm)
		}
	}

	// A signed algorithm that the verifier has no source for is refused under its own name; the twin is the same token at the verifier that has the source.
	hsTok := mint(t, hs, "", claims)
	ecTok := mint(t, ecSigner{t, w.ec}, ecKID, claims)
	rsTok := mint(t, rsaSigner{t, w.rsa}, rsaKID, claims)
	for _, c := range []struct {
		name string
		tok  string
		v    *Verifier
		want error
	}{
		{"HS256 at the JWKS-only verifier", hsTok, w.jwksV, ErrAlgorithm},
		{"HS256 at the HMAC verifier", hsTok, w.hmacV, nil},
		{"ES256 at the HMAC-only verifier", ecTok, w.hmacV, ErrAlgorithm},
		{"ES256 at the JWKS verifier", ecTok, w.jwksV, nil},
		{"RS256 at the HMAC-only verifier", rsTok, w.hmacV, ErrAlgorithm},
		{"RS256 at the JWKS verifier", rsTok, w.jwksV, nil},
		{"HS256 with a kid at the HMAC-only verifier (the kid is ignored)", mint(t, hs, "whatever", claims), w.hmacV, nil},
		{"HS256 at the verifier with both", hsTok, w.bothV, nil},
		{"ES256 at the verifier with both", ecTok, w.bothV, nil},
		{"RS256 at the verifier with both", rsTok, w.bothV, nil},
	} {
		_, err := verify(c.v, c.tok)
		wantErr(t, c.name, err, c.want)
	}
}

func TestVerify_AlgorithmConfusionWithAPublicKeyIsRefused(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	rsaDER, err := x509.MarshalPKIXPublicKey(&w.rsa.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKIXPublicKey(&w.ec.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ecPoint, err := w.ec.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	pemOf := func(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}) }
	// Every way an attacker may hold "the public key" as bytes: DER, PEM, PKCS #1, the bare modulus, the SEC1 point, the JWK's own text.
	keyBytes := map[string][]byte{
		"RSA PKIX DER":    rsaDER,
		"RSA PEM":         pemOf(rsaDER),
		"RSA PKCS#1":      x509.MarshalPKCS1PublicKey(&w.rsa.PublicKey),
		"RSA modulus":     w.rsa.PublicKey.N.Bytes(),
		"EC PKIX DER":     ecDER,
		"EC PEM":          pemOf(ecDER),
		"EC SEC1 point":   ecPoint,
		"EC x||y":         ecPoint[1:],
		"RSA JWK as text": jwksDoc(t, rsaJWK(&w.rsa.PublicKey, rsaKID)),
	}
	for name, kb := range keyBytes {
		for _, kid := range []string{rsaKID, ecKID, ""} {
			tok := mint(t, hmacSigner{kb}, kid, claimsAt(now))
			// The attack: the verifier trusts the key set, and the token says HS256, MACed with the key set's own public key.
			_, err := verify(w.jwksV, tok)
			wantErr(t, name+" (kid "+kid+") at the JWKS-only verifier", err, ErrAlgorithm)
			// With an unrelated secret also configured, HS256 is judged by that secret alone: the key set is never the key.
			_, err = verify(w.bothV, tok)
			wantErr(t, name+" (kid "+kid+") at the verifier with both sources", err, ErrSignature)
			// The control: a verifier whose HMAC secret IS those bytes accepts the very same token, so the token is well formed, the MAC is right, and only the allow-list stood in the way.
			control := mustNew(t, Options{HMACSecret: kb, Issuer: testIssuer, Now: w.clk.Now})
			if _, err := verify(control, tok); err != nil {
				t.Errorf("control failed for %s: a verifier with that secret refuses the token: %v", name, err)
			}
		}
	}
}

func TestVerify_HeaderKeyMaterialIsNeverReadOrFetched(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	attacker := ecKey(t)
	hook := newJWKSServer(t, jwksDoc(t, ecJWK(&attacker.PublicKey, "attacker-kid")))
	junk := func(base map[string]any) map[string]any {
		return with(base,
			"jku", hook.url(), "x5u", hook.url(), "jwk", ecJWK(&attacker.PublicKey, "attacker-kid"),
			"x5c", []string{enc(randBytes(t, 64))}, "x5t", enc(randBytes(t, 20)), "crit", []string{"exp", "b64"}, "cty", "JWT")
	}
	claims := claimsAt(now)

	// The twins: honest tokens carrying every one of those members are accepted as if the members were not there.
	hdr := func(alg, kid string) map[string]any {
		h := map[string]any{"alg": alg, "typ": "JWT"}
		if kid != "" {
			h["kid"] = kid
		}
		return junk(h)
	}
	for name, c := range map[string]struct {
		tok string
		v   *Verifier
	}{
		"HS256": {mintWith(t, hmacSigner{w.secret}, hdr("HS256", ""), claims), w.hmacV},
		"ES256": {mintWith(t, ecSigner{t, w.ec}, hdr("ES256", ecKID), claims), w.jwksV},
		"RS256": {mintWith(t, rsaSigner{t, w.rsa}, hdr("RS256", rsaKID), claims), w.jwksV},
	} {
		if _, err := verify(c.v, c.tok); err != nil {
			t.Errorf("an honest %s token with jku, x5u, jwk, x5c and crit members was refused: %v", name, err)
		}
	}

	// The attacks: signed by the attacker's own key, which the header offers as jwk and as jku. Under the real kid the real key is used and the signature fails; under the
	// attacker's kid there is no such key and the answer is ErrKey. Neither fetches the jku.
	atk := ecSigner{t, attacker}
	_, err := verify(w.jwksV, mintWith(t, atk, hdr("ES256", ecKID), claims))
	wantErr(t, "the attacker's signature under the real kid", err, ErrSignature)
	_, err = verify(w.jwksV, mintWith(t, atk, hdr("ES256", "attacker-kid"), claims))
	wantErr(t, "the attacker's signature under the attacker's kid", err, ErrKey)
	_, err = verify(w.jwksV, mintWith(t, atk, hdr("ES256", ""), claims))
	wantErr(t, "the attacker's signature under no kid", err, ErrKey)
	_, err = verify(w.bothV, mintWith(t, atk, hdr("ES256", "attacker-kid"), claims))
	wantErr(t, "the attacker's signature, both sources", err, ErrKey)
	if n := hook.hits.Load(); n != 0 {
		t.Errorf("the verifier fetched a jku or x5u named by a token: %d requests", n)
	}
	// Control: the hook is reachable, so a zero count means it was not asked, not that it was out of reach.
	if _, err := newKeySourceForTest(hook.url(), w.clk.Now).keyFor(context.Background(), "attacker-kid"); err != nil || hook.hits.Load() != 1 {
		t.Errorf("control failed: the hook server was not reachable (%v, %d hits)", err, hook.hits.Load())
	}
}

func newKeySourceForTest(url string, now func() time.Time) *keySource {
	return newKeySource(url, hardenedClient(nil), now, DefaultMinRefreshInterval, DefaultKeyTTL)
}

func TestVerify_ReadsOnlyTheInjectedClock(t *testing.T) {
	w := newWorld(t)
	f := w.fixtures(t)[0]
	tok := f.mint(t, claimsAt(testNow)) // valid at 2001-09-09 and a quarter of a century expired by the real clock

	if _, err := verify(f.v, tok); err != nil {
		t.Fatalf("the token is refused at the injected instant: %v", err)
	}
	// The control: the same verifier without an injected clock reads time.Now and refuses the same token, so the acceptance above is the injected clock's doing.
	real := mustNew(t, Options{HMACSecret: w.secret, Issuer: testIssuer})
	_, err := verify(real, tok)
	wantErr(t, "the same token against the real clock", err, ErrExpired)

	// The clock is read at every call: moving it moves the verdict, both ways.
	w.clk.Advance(2 * time.Hour)
	_, err = verify(f.v, tok)
	wantErr(t, "two hours later", err, ErrExpired)
	w.clk.Advance(-2 * time.Hour)
	_, err = verify(f.v, tok)
	wantErr(t, "back at the first instant", err, nil)
	w.clk.Advance(-time.Hour)
	_, err = verify(f.v, f.mint(t, with(claimsAt(testNow), "nbf", testNow.Unix()-60, "iat", testNow.Unix()-60)))
	wantErr(t, "an hour before (the token's nbf and iat are then an hour ahead)", err, ErrNotYetValid)

	// And the default clock is the real one: a token minted against time.Now is valid for a verifier with none, and expired for one whose clock is two hours on.
	nowReal := time.Now()
	live := mint(t, hmacSigner{w.secret}, "", claimsAt(nowReal))
	if _, err := verify(real, live); err != nil {
		t.Errorf("a token minted against the real clock is refused by a verifier with no injected clock: %v", err)
	}
	later := mustNew(t, Options{HMACSecret: w.secret, Issuer: testIssuer, Now: func() time.Time { return nowReal.Add(2 * time.Hour) }})
	_, err = verify(later, live)
	wantErr(t, "the real-time token with a clock two hours on", err, ErrExpired)
}

func TestVerify_NoErrorContainsAnyPartOfTheToken(t *testing.T) {
	w := newWorld(t)
	now := w.clk.Now()
	sec := now.Unix()
	marker := "MARKER-" + randText(t, 16)
	kidMarker := "KID-" + randText(t, 16)
	hs := hmacSigner{w.secret}
	mk := func(extra ...any) map[string]any {
		return with(claimsAt(now), append([]any{"note", marker}, extra...)...)
	}

	tokens := map[string]string{
		"expired":                     mint(t, hs, "", mk("exp", sec-3600)),
		"not yet valid":               mint(t, hs, "", mk("nbf", sec+3600)),
		"issued in the future":        mint(t, hs, "", mk("iat", sec+3600)),
		"the exp is the marker":       mint(t, hs, "", mk("exp", marker)),
		"wrong issuer (the marker)":   mint(t, hs, "", mk("iss", marker)),
		"wrong audience (the marker)": mint(t, hs, "", mk("aud", marker)),
		"audience array of markers":   mint(t, hs, "", mk("aud", []string{marker, marker + "2"})),
		"subject is the marker":       mint(t, hs, "", mk("sub", marker)),
		"no subject":                  mint(t, hs, "", mk("sub", nil)),
		"bad signature":               mint(t, hmacSigner{randBytes(t, 40)}, "", mk()),
		"algorithm is the marker":     mintWith(t, hs, map[string]any{"alg": marker}, mk()),
		"alg none":                    encJSON(t, map[string]any{"alg": "none", "x": marker}) + "." + encJSON(t, mk()) + ".",
		"kid is the marker, ES256":    mint(t, ecSigner{t, w.ec}, kidMarker, mk()),
		"kid is the marker, RS256":    mint(t, rsaSigner{t, w.rsa}, kidMarker, mk()),
		"wrong key, ES256":            mint(t, ecSigner{t, ecKey(t)}, ecKID, mk()),
		"kid is not a string":         mintWith(t, ecSigner{t, w.ec}, map[string]any{"alg": "ES256", "kid": []string{marker}}, mk()),
		"jku is the marker":           mintWith(t, ecSigner{t, w.ec}, map[string]any{"alg": "ES256", "kid": kidMarker, "jku": marker}, mk()),
		"duplicate key (marker)":      mintRaw(hs, `{"alg":"HS256"}`, fmt.Sprintf(`{"a":%q,"a":%q}`, marker, marker)),
		"payload is an array":         mintRaw(hs, `{"alg":"HS256"}`, fmt.Sprintf(`[%q]`, marker)),
		"header is the marker":        mintRaw(hs, fmt.Sprintf(`%q`, marker), `{}`),
		"ES256 with a short signature": func() string {
			h, p, _ := segments(mint(t, ecSigner{t, w.ec}, ecKID, mk()))
			return h + "." + p + "." + enc([]byte(marker))
		}(),
	}
	// Garbage that is not even three segments: the marker is in the text itself.
	tokens["not a token"] = marker + "." + marker
	tokens["a huge token holding the marker"] = marker + strings.Repeat("A", MaxTokenBytes)

	if len(tokens) < 20 {
		t.Fatalf("control failed: only %d cases", len(tokens))
	}
	verifiers := map[string]*Verifier{"HMAC": w.hmacV, "JWKS": w.jwksV, "both": w.bothV}
	var all []string
	for _, tok := range tokens {
		all = append(all, tok)
	}
	refusals := 0
	for name, tok := range tokens {
		for vname, v := range verifiers {
			_, err := verify(v, tok)
			if err == nil {
				// a token a given verifier accepts (none here is expected to be) has no error to read
				t.Errorf("%s at the %s verifier: accepted", name, vname)
				continue
			}
			refusals++
			for _, text := range []string{err.Error(), fmt.Sprintf("%v|%+v|%#v|%q|%s", err, err, err, err, err)} {
				if l := leaks(text, []string{tok}, marker, kidMarker); len(l) > 0 {
					t.Errorf("%s at the %s verifier: the error holds %q of the token: %s", name, vname, l, text)
				}
			}
		}
	}
	if refusals < 60 {
		t.Fatalf("control failed: only %d refusals were examined", refusals)
	}

	// The key-set failures are errors too: an endpoint that answers with the marker (as a body, as a status line's text) leaks nothing into them.
	bad := newJWKSServer(t, []byte(marker))
	bad.answer(500, []byte(marker+marker))
	broken := mustNew(t, Options{JWKSURL: bad.url(), AllowInsecureHTTP: true, Now: w.clk.Now})
	_, err := verify(broken, mint(t, ecSigner{t, w.ec}, ecKID, mk()))
	if !errors.Is(err, ErrKey) || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("a key set that failed: %v", err)
	}
	bad.answer(200, []byte(`{"keys":[{"kty":"oct","k":"`+marker+`"}]}`))
	w.clk.Advance(time.Minute)
	_, err = verify(broken, mint(t, ecSigner{t, w.ec}, ecKID, mk()))
	if !errors.Is(err, ErrKey) || strings.Contains(err.Error(), marker) {
		t.Errorf("a key set with an unusable key: %v", err)
	}

	// The paired control: the instrument finds a leak when one is there. A verifier-shaped function that formats the token into its error is caught by the same check.
	leaky := func(tok string) error { return fmt.Errorf("auth: bad token %s: %w", tok, ErrMalformed) }
	for _, name := range []string{"expired", "the exp is the marker", "bad signature", "kid is the marker, ES256"} {
		tok := tokens[name]
		if l := leaks(leaky(tok).Error(), []string{tok}, marker, kidMarker); len(l) == 0 {
			t.Errorf("control failed: a leaky error built from %q was not flagged", name)
		}
	}
	if l := leaks(ErrMalformed.Error(), all, marker, kidMarker); len(l) != 0 {
		t.Errorf("control failed: the sentinel text alone is flagged as a leak: %v", l)
	}
}

func TestNew_RefusesWeakOrMissingConfiguration(t *testing.T) {
	secret := randBytes(t, 64)
	field := func(err error) string {
		var oe *OptionError
		if errors.As(err, &oe) {
			return oe.Field
		}
		return ""
	}
	cases := []struct {
		name  string
		opts  Options
		field string // "" means accepted
	}{
		{"no key source", Options{}, "KeySource"},
		{"an empty secret and no URL", Options{HMACSecret: []byte{}}, "KeySource"},
		{"a secret of 1 byte", Options{HMACSecret: []byte("x")}, "HMACSecret"},
		{"a secret of 31 bytes", Options{HMACSecret: secret[:31]}, "HMACSecret"},
		{"a secret of 32 bytes", Options{HMACSecret: secret[:32]}, ""},
		{"a secret of 64 bytes", Options{HMACSecret: secret}, ""},
		{"a weak secret next to a good URL", Options{HMACSecret: secret[:10], JWKSURL: "https://project.example.test/jwks.json"}, "HMACSecret"},
		{"a good URL alone", Options{JWKSURL: "https://project.example.test/jwks.json"}, ""},
		{"both sources", Options{HMACSecret: secret, JWKSURL: "https://project.example.test/jwks.json"}, ""},
		{"leeway of one nanosecond", Options{HMACSecret: secret, Leeway: 1}, ""},
		{"leeway of five minutes", Options{HMACSecret: secret, Leeway: MaxLeeway}, ""},
		{"leeway over five minutes", Options{HMACSecret: secret, Leeway: MaxLeeway + 1}, "Leeway"},
		{"a negative leeway", Options{HMACSecret: secret, Leeway: -2 * time.Second}, "Leeway"},
		{"no leeway", Options{HMACSecret: secret, Leeway: NoLeeway}, ""},
		{"an issuer with a space", Options{HMACSecret: secret, Issuer: "https://a b"}, "Issuer"},
		{"an issuer with a newline", Options{HMACSecret: secret, Issuer: "https://a\n"}, "Issuer"},
		{"an issuer of 512 bytes", Options{HMACSecret: secret, Issuer: strings.Repeat("a", 512)}, ""},
		{"an issuer of 513 bytes", Options{HMACSecret: secret, Issuer: strings.Repeat("a", 513)}, "Issuer"},
		{"an audience with a space", Options{HMACSecret: secret, Audience: "two words"}, "Audience"},
		{"an audience that is not ASCII", Options{HMACSecret: secret, Audience: "café"}, "Audience"},
		{"a negative refresh interval", Options{HMACSecret: secret, MinRefreshInterval: -1}, "MinRefreshInterval"},
		{"a negative key lifetime", Options{HMACSecret: secret, KeyTTL: -1}, "KeyTTL"},
		// the JWKS URL: https always; http only with the flag and only to a host a test can own
		{"http without the flag", Options{JWKSURL: "http://127.0.0.1:9999/jwks.json"}, "JWKSURL"},
		{"http to a loopback address with the flag", Options{JWKSURL: "http://127.0.0.1:9999/jwks.json", AllowInsecureHTTP: true}, ""},
		{"http to an IPv6 loopback address with the flag", Options{JWKSURL: "http://[::1]:9999/jwks.json", AllowInsecureHTTP: true}, ""},
		{"http to localhost with the flag", Options{JWKSURL: "http://localhost:9999/jwks.json", AllowInsecureHTTP: true}, ""},
		{"http to a private address with the flag", Options{JWKSURL: "http://10.1.2.3/jwks.json", AllowInsecureHTTP: true}, ""},
		{"http to a single-label name with the flag", Options{JWKSURL: "http://gotrue:9999/.well-known/jwks.json", AllowInsecureHTTP: true}, ""},
		{"http to a documentation (public-looking) address with the flag", Options{JWKSURL: "http://203.0.113.9/jwks.json", AllowInsecureHTTP: true}, "JWKSURL"},
		{"http to a public name with the flag", Options{JWKSURL: "http://project.example.test/jwks.json", AllowInsecureHTTP: true}, "JWKSURL"},
		{"https with the flag is still https", Options{JWKSURL: "https://project.example.test/jwks.json", AllowInsecureHTTP: true}, ""},
		{"ftp", Options{JWKSURL: "ftp://project.example.test/jwks.json", AllowInsecureHTTP: true}, "JWKSURL"},
		{"a relative URL", Options{JWKSURL: "/jwks.json"}, "JWKSURL"},
		{"no scheme", Options{JWKSURL: "project.example.test/jwks.json"}, "JWKSURL"},
		{"no host", Options{JWKSURL: "https:///jwks.json"}, "JWKSURL"},
		{"credentials in the URL", Options{JWKSURL: "https://user:pass@project.example.test/jwks.json"}, "JWKSURL"},
		{"a query in the URL", Options{JWKSURL: "https://project.example.test/jwks.json?apikey=x"}, "JWKSURL"},
		{"a fragment in the URL", Options{JWKSURL: "https://project.example.test/jwks.json#x"}, "JWKSURL"},
		{"an opaque URL", Options{JWKSURL: "https:project.example.test"}, "JWKSURL"},
	}
	for _, c := range cases {
		_, err := New(c.opts)
		switch {
		case c.field == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.field != "" && field(err) != c.field:
			t.Errorf("%s: got %v, want an OptionError on %s", c.name, err, c.field)
		}
		if err != nil {
			if strings.Contains(err.Error(), string(secret[:10])) {
				t.Errorf("%s: the refusal repeats the secret: %v", c.name, err)
			}
			if c.opts.JWKSURL != "" && strings.Contains(err.Error(), c.opts.JWKSURL) {
				t.Errorf("%s: the refusal repeats the URL: %v", c.name, err)
			}
		}
	}

	// The defaults, read back.
	v := mustNew(t, Options{HMACSecret: secret})
	if s := v.Summary(); !s.HMAC || s.JWKS || s.IssuerPinned || s.Audience != DefaultAudience || s.Leeway != DefaultLeeway {
		t.Errorf("the defaults are %+v", s)
	}
	if s := mustNew(t, Options{HMACSecret: secret, Leeway: NoLeeway, Issuer: "x", Audience: "y", JWKSURL: "https://project.example.test/j"}).Summary(); !s.JWKS || !s.IssuerPinned || s.Audience != "y" || s.Leeway != 0 {
		t.Errorf("the configured values are %+v", s)
	}

	// The secret is copied: the caller changing its slice afterwards changes nothing.
	mine := append([]byte(nil), secret...)
	copied := mustNew(t, Options{HMACSecret: mine, Now: newClock().Now})
	tok := mint(t, hmacSigner{secret}, "", claimsAt(testNow))
	for i := range mine {
		mine[i] = 0
	}
	if _, err := verify(copied, tok); err != nil {
		t.Errorf("the verifier did not copy the secret: %v", err)
	}

	// New does no network I/O: a URL nobody serves is fine until a token needs it.
	srv := newJWKSServer(t, nil)
	if _, err := New(Options{JWKSURL: srv.url(), AllowInsecureHTTP: true}); err != nil || srv.hits.Load() != 0 {
		t.Errorf("New touched the network: %v, %d requests", err, srv.hits.Load())
	}
}
