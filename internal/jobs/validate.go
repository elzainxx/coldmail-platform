package jobs

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits. Every one is checked BEFORE the database is asked; the database's own CHECKs stay as the second wall.
const (
	// DefaultMaxRows is the longest list a job holds unless the store is given another limit (module 1's own limit).
	DefaultMaxRows = 100000
	// HardMaxRows is the largest limit WithMaxRows accepts: ordinals are integers and a list is held in memory while it is classified.
	HardMaxRows = 1000000
	// DefaultMaxStartDelay is how long after its start (the scheduled time, or the confirm) a job may still be waiting for the engine to start it before it expires.
	DefaultMaxStartDelay = 30 * time.Minute
	// MaxStartDelayLimit is the largest delay WithMaxStartDelay accepts.
	MaxStartDelayLimit = 31 * 24 * time.Hour

	MaxSubjectBytes = 998     // RFC 5322 section 2.1.1: a header line is at most 998 characters
	MaxBodyBytes    = 1 << 20 // html and text, each
	MaxReplyToBytes = 254     // an address, RFC 5321 section 4.5.3.1.3

	MaxFieldKeys  = 64   // merge data keys per row
	MaxFieldBytes = 4096 // the keys and values of one row together
	MaxRawBytes   = 4096 // job_rows_email_raw_bounded: the longest address text a row keeps

	DefaultListLimit = 25
	MaxListLimit     = 100
	DefaultEvents    = 100
	MaxEvents        = 1000

	// MinScheduleAhead and MaxScheduleAhead bound a scheduled start by the database's clock.
	MinScheduleAhead = time.Minute
	MaxScheduleAhead = 366 * 24 * time.Hour
)

var (
	keyShape         = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	fingerprintShape = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// isUUID is true for exactly the form Postgres prints: 36 characters, lower-case hex, hyphens at 8, 13, 18 and 23. A different spelling of the same uuid would hash to a different advisory
// lock (internal/money), so only this one is accepted.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// systemUser is module 2's shared-pool owner: it holds nothing and is never a customer.
const systemUser = "00000000-0000-0000-0000-000000000000"

// checkUser refuses a user id that is not the canonical lower-case uuid of a customer. It is the authenticated principal, so a bad one is a bug of the caller, not a request to answer.
func checkUser(userID string) error {
	if !isUUID(userID) || userID == systemUser {
		return badRequest("the user id is not a customer's canonical uuid")
	}
	return nil
}

// newUUID is a random (version 4) uuid in the canonical lower-case form, from crypto/rand: the store generates row ids itself because a duplicate row names the row it repeats.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil { // crypto/rand.Read does not fail on the supported platforms (Go 1.24); a failure here is not recoverable
		panic("jobs: no randomness: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// textOK says whether a string can be stored by PostgreSQL text and jsonb: valid UTF-8 with no NUL.
func textOK(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// validKey is the shape of a confirm key (the API's Idempotency-Key): the CHECK jobs_confirm_key_shape.
func validKey(k string) bool { return keyShape.MatchString(k) }

// validFingerprint is the shape of a preview fingerprint: 64 lower-case hex digits.
func validFingerprint(f string) bool { return fingerprintShape.MatchString(f) }

// normaliseSettings refuses a value outside the vocabulary and a choice on a job that is not chained. There is NO default (owner ruling OD-4): "" stays "" (not chosen yet), and Confirm
// refuses a chained send whose source holds unknown verdicts until the user has said include or exclude. chained says whether the job has a source job: the choice is meaningful only then.
func normaliseSettings(s Settings, chained bool) (Settings, error) {
	switch s.UnknownChoice {
	case "":
	case UnknownExclude, UnknownInclude:
		if !chained {
			return Settings{}, badRequest("unknown_choice %q only applies to a send chained to a verify job", s.UnknownChoice)
		}
	default:
		return Settings{}, badRequest("unknown_choice must be %q or %q", UnknownExclude, UnknownInclude)
	}
	return s, nil
}

// validateContent checks a send job's campaign: a subject (one line, at most 998 bytes), a body (html or text, each at most 1 MiB, at least one of them not blank) and an optional reply-to
// that is one address. It returns the content as it will be stored (the subject and the reply-to trimmed). The reasons never quote the content.
func validateContent(c *Content) (*Content, error) { return validateContentAs(c, true) }

// validateContentAs is validateContent with one switch: complete false is a campaign that is still being written (the template check of POST /v1/templates/check, Phase 6 A3), where a blank subject and a
// campaign with no body yet are allowed and every other rule (the text, the sizes, the one-line subject, the reply-to) is the save's own, from this one function. A save is always complete.
func validateContentAs(c *Content, complete bool) (*Content, error) {
	if c == nil {
		return nil, badRequest("content is required for a send job")
	}
	out := *c
	out.Subject = strings.TrimSpace(c.Subject)
	out.ReplyTo = strings.TrimSpace(c.ReplyTo)
	for _, f := range []struct{ name, v string }{{"subject", out.Subject}, {"html", out.HTML}, {"text", out.Text}, {"reply_to", out.ReplyTo}} {
		if !textOK(f.v) {
			return nil, badRequest("content.%s must be valid UTF-8 text without NUL", f.name)
		}
	}
	if complete && out.Subject == "" {
		return nil, badRequest("content.subject must not be blank")
	}
	if len(out.Subject) > MaxSubjectBytes {
		return nil, badRequest("content.subject is %d bytes, the limit is %d", len(out.Subject), MaxSubjectBytes)
	}
	if strings.ContainsAny(out.Subject, "\r\n") {
		return nil, badRequest("content.subject must be one line")
	}
	if len(out.HTML) > MaxBodyBytes {
		return nil, badRequest("content.html is %d bytes, the limit is %d", len(out.HTML), MaxBodyBytes)
	}
	if len(out.Text) > MaxBodyBytes {
		return nil, badRequest("content.text is %d bytes, the limit is %d", len(out.Text), MaxBodyBytes)
	}
	if complete && strings.TrimSpace(out.HTML) == "" && strings.TrimSpace(out.Text) == "" {
		return nil, badRequest("content needs a body: html or text must not be blank")
	}
	if out.ReplyTo != "" {
		if len(out.ReplyTo) > MaxReplyToBytes || strings.ContainsAny(out.ReplyTo, "\r\n") {
			return nil, badRequest("content.reply_to is not a single address")
		}
		a, err := mail.ParseAddress(out.ReplyTo)
		if err != nil || a.Name != "" || a.Address != out.ReplyTo {
			return nil, badRequest("content.reply_to must be a single bare address")
		}
	}
	return &out, nil
}

// checkFields validates one row's merge data and returns it as the JSON object job_rows.fields holds ("{}" for none). The limits: MaxFieldKeys keys and MaxFieldBytes bytes of keys and values
// together, every key non-blank, keys and values valid text. ordinal names the row in the reason; nothing of the data is quoted.
func checkFields(ordinal int, f map[string]string) (string, error) {
	if len(f) == 0 {
		return "{}", nil
	}
	if len(f) > MaxFieldKeys {
		return "", badRequest("row %d: %d merge fields, the limit is %d", ordinal, len(f), MaxFieldKeys)
	}
	total := 0
	for k, v := range f {
		if strings.TrimSpace(k) == "" {
			return "", badRequest("row %d: a merge field has a blank name", ordinal)
		}
		if !textOK(k) || !textOK(v) {
			return "", badRequest("row %d: merge fields must be valid UTF-8 text without NUL", ordinal)
		}
		total += len(k) + len(v)
	}
	if total > MaxFieldBytes {
		return "", badRequest("row %d: merge fields hold %d bytes, the limit is %d", ordinal, total, MaxFieldBytes)
	}
	b, err := json.Marshal(f) // a map[string]string: encoding/json writes the keys sorted
	if err != nil {
		return "", badRequest("row %d: merge fields cannot be encoded", ordinal)
	}
	return string(b), nil
}

// cursor is the position after the last job of a page: the keyset of the list (created_at descending, then id).
type cursor struct {
	T  string `json:"t"` // RFC 3339, nanosecond precision, UTC
	ID string `json:"id"`
}

// encodeCursor is the opaque string a page hands back. It is URL-safe base64 of a small JSON object.
func encodeCursor(t time.Time, id string) string {
	b, _ := json.Marshal(cursor{T: t.UTC().Format(time.RFC3339Nano), ID: id}) // two strings: cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor is the inverse of encodeCursor; anything else is an ErrBadRequest (the text of the cursor is never quoted).
func decodeCursor(s string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", badRequest("malformed cursor")
	}
	var c cursor
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return time.Time{}, "", badRequest("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, c.T)
	if err != nil || !isUUID(c.ID) {
		return time.Time{}, "", badRequest("malformed cursor")
	}
	return t, c.ID, nil
}

// pageLimit applies the default and the bounds of a list's limit: 0 means the default, anything else must lie in 1..upper.
func pageLimit(n, def, upper int) (int, error) {
	switch {
	case n == 0:
		return def, nil
	case n < 0 || n > upper:
		return 0, badRequest("limit must be between 1 and %d", upper)
	}
	return n, nil
}
