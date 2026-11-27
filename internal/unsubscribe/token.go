// Package unsubscribe makes and checks the per-recipient unsubscribe token (Phase 5, chunk S3). It is PURE: it reads no clock, no environment, no database and no network, and it holds no
// state but the secret it was built with.
//
// THE TOKEN (Docs/phase-5-spec.md, decision 5). One token names one item of a send job (public.job_items.id = platform.job_rows.id, the id the platform already hands every module):
//
//	token = base64url-without-padding( item id (16 raw bytes) || HMAC-SHA256( secret, "unsub|v1|" + item id ) first 24 bytes )        54 characters
//
// where the item id in the MAC's text is the canonical lower-case uuid text. The address is NOT in the token and cannot be read from it (the endpoint resolves the item through the store), so a
// link in a forwarded message or a browser history tells nothing about the recipient, and a token for one item says nothing about another.
//
// WHAT IS STRICT.
//   - The token is exactly TokenLength characters of the URL-safe alphabet. No padding, no white space, no line break (the standard library's decoder would silently skip a CR or LF), no other
//     character. The check is made before the decoder runs.
//   - The decoding is canonical (base64.Encoding.Strict): 40 bytes make 53 and a third characters, so the last character carries two bits that must be zero; a token whose last character differs
//     only in them is NOT another spelling of the same token, it is refused.
//   - The MAC is compared in constant time (crypto/hmac.Equal over the whole 24 bytes), and every refusal is the same refusal: a short token, a wrong alphabet, a bad MAC, a token minted under
//     another secret, and an altered item id all answer ("", false) with no word about why. Whether the item EXISTS is the store's question and is answered the same way by the caller.
//   - The MAC is computed for EVERY token that is long enough to be one, and for a token of the wrong shape over a fixed stand-in, so that the time Verify takes does not tell a malformed token
//     from a well-formed one with a wrong MAC.
//
// THE SECRET is at least MinSecretBytes bytes (the size of the HMAC-SHA256 output, like every other secret of the platform). A shorter one is refused where a Signer is built, and by Token and
// Verify themselves, so no caller can mint a token under a weak key by calling the functions directly.
package unsubscribe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	// MinSecretBytes is the shortest secret accepted. internal/config pins PLATFORM_UNSUBSCRIBE_SECRET's floor equal to it (a test there).
	MinSecretBytes = 32

	// MACBytes is how much of the HMAC-SHA256 output the token carries: 24 bytes, 192 bits.
	MACBytes = 24

	idBytes = 16

	// TokenLength is the length of a token in characters: 16 + 24 = 40 bytes, 54 characters unpadded.
	TokenLength = 54

	macDomain = "unsub|v1|"

	// PathPrefix is the part of the link between the public base URL and the token.
	PathPrefix = "/u/"
)

var (
	// ErrWeakSecret: the secret is shorter than MinSecretBytes.
	ErrWeakSecret = errors.New("unsubscribe: the secret is shorter than 32 bytes")
	// ErrBadItemID: the item id is not the canonical lower-case uuid text.
	ErrBadItemID = errors.New("unsubscribe: the item id is not a canonical lower-case uuid")
)

// enc is the one encoding: URL-safe, no padding, canonical.
var enc = base64.RawURLEncoding.Strict()

// standIn is the item id a malformed token is judged against, so that the MAC is computed whatever the token looks like. It is never returned and never a real item.
var standIn = [idBytes]byte{}

// Token returns the token of the item under secret. The item id must be the canonical lower-case uuid text and the secret at least MinSecretBytes bytes.
func Token(secret []byte, itemID string) (string, error) {
	if len(secret) < MinSecretBytes {
		return "", ErrWeakSecret
	}
	raw, ok := parseUUID(itemID)
	if !ok {
		return "", ErrBadItemID
	}
	buf := make([]byte, 0, idBytes+MACBytes)
	buf = append(buf, raw[:]...)
	buf = append(buf, mac(secret, itemID)[:MACBytes]...)
	return enc.EncodeToString(buf), nil
}

// Verify checks a token under secret and returns the item id it names (canonical lower-case uuid text) and true, or ("", false). Every way a token can be wrong, and a secret that is too short,
// is the same answer. It does not know whether the item exists.
func Verify(secret []byte, token string) (string, bool) {
	if len(secret) < MinSecretBytes {
		return "", false
	}
	shaped := wellFormed(token)
	var raw []byte
	if shaped {
		var err error
		if raw, err = enc.DecodeString(token); err != nil || len(raw) != idBytes+MACBytes {
			shaped = false
		}
	}
	id := standIn
	got := make([]byte, MACBytes)
	if shaped {
		copy(id[:], raw[:idBytes])
		copy(got, raw[idBytes:])
	}
	itemID := formatUUID(id)
	want := mac(secret, itemID)[:MACBytes]
	equal := hmac.Equal(got, want) // constant time over the whole 24 bytes
	if !shaped || !equal {
		return "", false
	}
	return itemID, true
}

// Link is the public link of a token: the base URL, "/u/" and the token. A trailing slash of the base is dropped, so the link never holds "//u/".
func Link(baseURL, token string) string {
	return strings.TrimRight(baseURL, "/") + PathPrefix + token
}

// Signer is a secret that has been checked once. It is safe for concurrent use: it holds a copy of the secret and nothing that changes.
type Signer struct {
	secret []byte
}

// NewSigner copies the secret and refuses one shorter than MinSecretBytes.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < MinSecretBytes {
		return nil, ErrWeakSecret
	}
	return &Signer{secret: append([]byte(nil), secret...)}, nil
}

// Token is the package function under the signer's secret.
func (s *Signer) Token(itemID string) (string, error) { return Token(s.secret, itemID) }

// Verify is the package function under the signer's secret.
func (s *Signer) Verify(token string) (string, bool) { return Verify(s.secret, token) }

// LinkMaker returns the function the job store is given (jobs.WithUnsubscribe): the item id to its public link under baseURL.
func (s *Signer) LinkMaker(baseURL string) func(itemID string) (string, error) {
	return func(itemID string) (string, error) {
		t, err := s.Token(itemID)
		if err != nil {
			return "", err
		}
		return Link(baseURL, t), nil
	}
}

// wellFormed says the token is exactly TokenLength characters of the URL-safe alphabet, by a loop of its own: the standard decoder skips CR and LF and would let a token with one of them (and
// two more characters of the alphabet) through at another length.
func wellFormed(token string) bool {
	if len(token) != TokenLength {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// mac is the full HMAC-SHA256 of the domain text and the item id under secret.
func mac(secret []byte, itemID string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(macDomain))
	h.Write([]byte(itemID))
	return h.Sum(nil)
}

const hexDigits = "0123456789abcdef"

// formatUUID is the canonical lower-case text of 16 bytes.
func formatUUID(b [idBytes]byte) string {
	var out [36]byte
	j := 0
	for i := 0; i < idBytes; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j], out[j+1] = hexDigits[b[i]>>4], hexDigits[b[i]&0x0f]
		j += 2
	}
	return string(out[:])
}

// parseUUID reads the canonical lower-case uuid text (8-4-4-4-12 lower-case hexadecimal digits) and nothing else.
func parseUUID(s string) ([idBytes]byte, bool) {
	var out [idBytes]byte
	if len(s) != 36 {
		return out, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return out, false
			}
			continue
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		default:
			return out, false
		}
		if n%2 == 0 {
			out[n/2] = v << 4
		} else {
			out[n/2] |= v
		}
		n++
	}
	return out, n == 2*idBytes
}
