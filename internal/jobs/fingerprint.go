package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
)

// THE FINGERPRINT (Docs/phase-4-spec.md, "Preview"). It is the sha256, in hex, of a canonical JSON document (keys sorted, no white space) of everything a preview's numbers depended on:
//
//	job             the job's id
//	module          the job's module
//	rows            the digest of the list: one line `ordinal|state|email_norm|reason` per row in ordinal order, with the state `suppressed` read as `pending` (the user's suppression list
//	                is applied at confirm and is never part of the fingerprint: a new suppression changes the preview's counts, not what the user is confirming)
//	content         the sha256 of the campaign's canonical JSON ("" for a verify job)
//	unknown_choice  the chained send's choice for `unknown` verdicts
//	source          the source job's id ("" when none)
//	source_results  the digest of the source job's module results, one line `item_ref|module_state` per result ordered by item_ref ("" when none)
//	prices          unit -> cost of the REACHABLE units of the module (a price change and a flip of a unit's reachability each change it; {} for module 3)
//	fields          (send jobs whose rows carry merge data only: the member is absent otherwise, so no fingerprint of a job without merge data changed) the digest of the rows' merge data, one line
//	                `ordinal|md5 of the row's fields as jsonb text` per row that has any, in ordinal order. The templates are in `content`; the missing-field count and the samples of a preview are functions of the
//	                templates and of these values, so a change of a value must be a stale preview. The values themselves are never in the fingerprint, only their digest.
//
// The credits the user holds are NOT in it: a balance that moved is not a stale preview (confirm checks the balance itself). Preview and Confirm compute it with the same code.

// fingerprintInput is what the fingerprint is the digest of.
type fingerprintInput struct {
	JobID         string
	Module        string
	RowsDigest    string
	ContentDigest string
	UnknownChoice string
	SourceJobID   string
	SourceDigest  string
	FieldsDigest  string // "" when no row of a send job carries merge data (and for every verify job)
	Prices        map[string]int
}

// fingerprint is the hex sha256 of the canonical JSON of the input.
func (in fingerprintInput) fingerprint() string {
	prices := make(map[string]any, len(in.Prices))
	for unit, cost := range in.Prices {
		prices[unit] = int64(cost)
	}
	doc := map[string]any{
		"job":            in.JobID,
		"module":         in.Module,
		"rows":           in.RowsDigest,
		"content":        in.ContentDigest,
		"unknown_choice": in.UnknownChoice,
		"source":         in.SourceJobID,
		"source_results": in.SourceDigest,
		"prices":         prices,
	}
	if in.FieldsDigest != "" { // present only when there is merge data to name: a job without any keeps the fingerprint it had before the templates
		doc["fields"] = in.FieldsDigest
	}
	b, err := canonicalJSON(doc)
	if err != nil { // the document is built from strings and integers above: this cannot happen
		panic("jobs: the fingerprint document cannot be encoded: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON encodes v as JSON with every object's keys in sorted (byte) order, no white space and no HTML escaping. It accepts the types the fingerprint is built from: nil, bool, string,
// int, int64, map[string]any and []any; anything else is an error (a float would be a second spelling of one number).
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		return writeJSONString(b, x)
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJSONString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("jobs: cannot canonicalise a %T", v)
	}
	return nil
}

// writeJSONString writes s as a JSON string with encoding/json's escaping but without its HTML escaping of <, > and &.
func writeJSONString(b *bytes.Buffer, s string) error {
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(s); err != nil {
		return err
	}
	b.Write(bytes.TrimSuffix(enc.Bytes(), []byte("\n")))
	return nil
}

// lineDigest builds a digest of lines of `|`-separated fields. A field is escaped (backslash, bar and newline) so that no two different lists make one text.
type lineDigest struct {
	h hash.Hash
	n int
}

func newLineDigest() *lineDigest { return &lineDigest{h: sha256.New()} }

var fieldEscaper = strings.NewReplacer(`\`, `\\`, `|`, `\|`, "\n", `\n`, "\r", `\r`)

// add writes one line: the fields joined by '|', then a newline.
func (d *lineDigest) add(fields ...string) {
	for i, f := range fields {
		if i > 0 {
			d.h.Write([]byte{'|'})
		}
		d.h.Write([]byte(fieldEscaper.Replace(f)))
	}
	d.h.Write([]byte{'\n'})
	d.n++
}

// sum is the hex sha256 of everything added; "" when nothing was (an empty list has no digest, and a missing one is not an empty one).
func (d *lineDigest) sum() string {
	if d.n == 0 {
		return ""
	}
	return hex.EncodeToString(d.h.Sum(nil))
}

// rowLine adds a row to the rows digest: ordinal|state|email_norm|reason, with `suppressed` (and the two states a send job's confirm leaves rows in) read as `pending`.
func (d *lineDigest) rowLine(ordinal int, state, emailNorm, reason string) {
	if state == StateSuppressed || state == StateMissingField || state == StateUnrenderable {
		state = StatePending
	}
	d.add(strconv.Itoa(ordinal), state, emailNorm, reason)
}

// contentDigest is the sha256 of the campaign's canonical JSON; "" for a job with no campaign.
func contentDigest(c *Content) string {
	if c == nil {
		return ""
	}
	b, err := canonicalJSON(map[string]any{"subject": c.Subject, "html": c.HTML, "text": c.Text, "reply_to": c.ReplyTo})
	if err != nil {
		panic("jobs: the content cannot be encoded: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
