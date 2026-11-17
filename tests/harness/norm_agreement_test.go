//go:build harness

package harness

// Phase 3, part A2: the platform's normaliser (internal/norm) agrees with module 1's frozen norm/v1, proven three ways on ONE shared vector file, and every proof has a control that goes
// red when the property breaks.
//
//	TestNormAgreement_Database      the platform.suppressions CHECK (platform_0003): for every vector the database accepts the normaliser's OUTPUT exactly where the file says the address
//	                                is well formed, refuses the raw input exactly where it says malformed, stores the very string the normaliser returned, and treats the vectors of one
//	                                group as one primary key. Where the CHECK is deliberately narrower or wider than norm/v1 the vector is named in nrmDBDivergence with the reason.
//	TestM1Live_NormAgreement        the same file as ONE bulk job on the real module 1: the rows module 1 reports as malformed (create response malformed_rows) are exactly the malformed
//	                                vectors, and the rows it answers with duplicate_of are exactly the repeats inside a group, each pointing at the first row of its own group.
//	TestModulesLive_NormSharedFile  the platform's copy of the file and module 1's copy (which module 1's own test runs through its own Normalize) are the same bytes, and both modules pin
//	                                the same versions of the two Unicode libraries the answer depends on.
//
// The last two names begin with TestM1Live and TestModulesLive on purpose: tests/harness/run-db-tests.sh (the `db-tests` workflow, a Postgres and no modules) skips exactly the names that
// contain TestLive_StackCoexists, TestModulesLive, TestM3Live or TestM1Live, and these two need the module stack. The first needs only the database and runs in both workflows.
//
// What the runner must prove (nothing here has run: no Docker, Postgres or module 1 on the machine that wrote it): the three tests green; the divergence table of the database leg is exactly
// what the CHECK does (it was written from the SQL, not from a run); module 1 completes a job of about 190 addresses (about 95 distinct) in time with no route out; module 1's copy of the vector
// file exists at the pinned commit (until module 1 is re-pinned to a commit that carries it, the shared-file test is red with a message that says so).
//
// NOT CHECKED, said on every run: vectors whose input is not valid UTF-8 cannot travel as JSON, so they are not sent to module 1 (module 1's decoder would replace the bytes with U+FFFD
// before its Normalize ever saw them); how module 1's syntax layer answers an address that normalises (a local part over 64 bytes, a domain over 255 bytes, a domain with no dot) is
// printed, not asserted.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	m1results "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/norm"
)

const (
	nrmVectorRel   = "internal/norm/testdata/norm-vectors.json"
	nrmM1VectorRel = "coldmail-verify/control-plane/internal/hash/testdata/norm-vectors.json"
	nrmM1GoModRel  = "coldmail-verify/control-plane/go.mod"
	nrmConstraint  = "suppressions_email_norm_v1"
)

// ---- the vector file ---------------------------------------------------------------------------------------------------------------------------------------------------------------

type nrmVec struct {
	Name       string  `json:"name"`
	Input      *string `json:"input"`
	InputB64   *string `json:"input_b64"`
	Malformed  bool    `json:"malformed"`
	Normalized *string `json:"normalized"`
	Group      *string `json:"group"`
	Why        string  `json:"why"`
}

type nrmFile struct {
	Format      string   `json:"format"`
	Scheme      string   `json:"scheme"`
	GeneratedBy string   `json:"generated_by"`
	Fields      string   `json:"fields"`
	Vectors     []nrmVec `json:"vectors"`
}

func (v nrmVec) raw() (string, error) {
	switch {
	case v.Input != nil && v.InputB64 == nil:
		return *v.Input, nil
	case v.InputB64 != nil && v.Input == nil:
		b, err := base64.StdEncoding.DecodeString(*v.InputB64)
		if err != nil {
			return "", fmt.Errorf("vector %q: input_b64 is not base64", v.Name)
		}
		return string(b), nil
	}
	return "", fmt.Errorf("vector %q must have exactly one of input and input_b64", v.Name)
}

func (v nrmVec) group() string {
	if v.Group == nil {
		return ""
	}
	return *v.Group
}

func (v nrmVec) normalized() string {
	if v.Normalized == nil {
		return ""
	}
	return *v.Normalized
}

func nrmDecode(t *testing.T, where string, b []byte) nrmFile {
	t.Helper()
	var f nrmFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("%s does not decode as a norm vector file: %v", where, err)
	}
	if len(f.Vectors) < 80 {
		t.Fatalf("%s holds %d vectors: the instrument matched too little (floor 80)", where, len(f.Vectors))
	}
	return f
}

// nrmPlatformFile reads the platform's own copy (the repository the harness mounts at HARNESS_REPO_ROOT) and its bytes.
func nrmPlatformFile(t *testing.T) (nrmFile, []byte) {
	t.Helper()
	p := filepath.Join(requireEnv(t, envRepoRoot), filepath.FromSlash(nrmVectorRel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading the platform's vector file %s: %v", nrmVectorRel, err)
	}
	return nrmDecode(t, nrmVectorRel, b), b
}

// nrmRawOf returns the vector's input or fails the test.
func nrmRawOf(t *testing.T, v nrmVec) string {
	t.Helper()
	s, err := v.raw()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- 1. the database -----------------------------------------------------------------------------------------------------------------------------------------------------------------

// nrmDivergence is a vector on which platform.suppressions' CHECK deliberately differs from norm/v1, by name. dbAccepts is what the table does with what is inserted (the normaliser's
// output for a well-formed vector, the raw input for a malformed one). The table was written by reading platform_0003, not by a run: the first runner pass is its proof, and the test
// reports EVERY mismatch at once so that one pass is enough to correct it.
type nrmDivergence struct {
	dbAccepts bool
	why       string
}

var nrmDBDivergence = map[string]nrmDivergence{
	// NARROWER: module 1 accepts the address, the CHECK refuses it.
	"trim-leaves-nul-in-local":       {false, "a NUL cannot be stored in text (SQLSTATE 22021, before any CHECK)"},
	"nul-in-local":                   {false, "a NUL cannot be stored in text (SQLSTATE 22021, before any CHECK)"},
	"quoted-local-with-space":        {false, "the local part holds a space: [^@[:space:]]+"},
	"quoted-local-lower":             {false, "the local part holds a space: [^@[:space:]]+"},
	"line-feed-in-local":             {false, "the local part holds a line feed: [^@[:space:]]+"},
	"space-in-local":                 {false, "the local part holds a space: [^@[:space:]]+"},
	"space-in-local-upper":           {false, "the local part holds a space: [^@[:space:]]+"},
	"space-before-at":                {false, "the local part ends with a space: [^@[:space:]]+"},
	"tab-in-local":                   {false, "the local part holds a tab: [^@[:space:]]+"},
	"domain-empty-label-inside":      {false, "an empty label: split_part(email, '@', 2) !~ '\\.\\.'"},
	"domain-empty-label-leading":     {false, "a leading dot: the domain must start with [a-z0-9]"},
	"domain-empty-punycode-label":    {false, "the domain becomes .com, a leading dot"},
	"domain-leading-dot-only-label":  {false, "a leading dot: the domain must start with [a-z0-9]"},
	"local-4096-bytes":               {false, "over 254 bytes: octet_length(email) <= 254"},
	"domain-253-bytes":               {false, "the address is over 254 bytes: octet_length(email) <= 254"},
	"domain-254-bytes":               {false, "the address is over 254 bytes: octet_length(email) <= 254"},
	"domain-300-bytes":               {false, "the address is over 254 bytes: octet_length(email) <= 254"},
	"address-255-bytes":              {false, "one byte over 254: octet_length(email) <= 254"},
	"multibyte-local-over-254-bytes": {false, "260 bytes: octet_length(email) <= 254 counts bytes, not characters"},
	// WIDER: module 1 refuses the address, the CHECK cannot see why.
	"domain-hyphens-in-positions-three-and-four": {true, "IDNA's CheckHyphens (no -- in positions three and four) has no SQL equivalent in the CHECK"},
	"domain-invalid-punycode-short":              {true, "xn--a is not valid punycode: the CHECK only sees [a-z0-9.-]"},
	"domain-punycode-of-ascii-garbage":           {true, "xn--zzzzzzzz is not valid punycode: the CHECK only sees [a-z0-9.-]"},
}

// nrmDBOutcome is what the database did with one insert.
type nrmDBOutcome struct {
	inserted   string // the string handed to the database
	accepted   bool
	stored     string // RETURNING email, when accepted
	state      string // SQLSTATE, when refused
	constraint string
}

// nrmNeedsEncoding reports whether text cannot reach the CHECK at all: PostgreSQL refuses NUL and invalid UTF-8 in a text value (SQLSTATE 22021) before any constraint runs.
func nrmNeedsEncoding(s string) bool { return strings.ContainsRune(s, 0) || !utf8.ValidString(s) }

// nrmDBProblems is the whole verdict for one vector, as sentences (a pure function, so a control can show it goes red): the platform's normaliser against the file, the database against
// the file and the divergence table, the stored value against what was inserted, and the reason of a refusal.
func nrmDBProblems(v nrmVec, got string, gotErr error, o nrmDBOutcome, div map[string]nrmDivergence) []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, v.Name+": "+fmt.Sprintf(f, a...)) }
	if (gotErr != nil) != v.Malformed {
		add("the platform's normaliser says malformed=%t (%v), the file says %t", gotErr != nil, gotErr, v.Malformed)
	} else if gotErr == nil && got != v.normalized() {
		add("the normaliser returned %q, the file says %q", got, v.normalized())
	}
	want := !v.Malformed
	d, diverges := div[v.Name]
	if diverges {
		want = d.dbAccepts
	}
	switch {
	case o.accepted != want && diverges:
		add("the divergence table says the database %s it (%s), the database %s it", verb(want), d.why, verb(o.accepted))
	case o.accepted != want && o.accepted:
		add("the database ACCEPTED %q, which norm/v1 refuses and the divergence table does not list", o.inserted)
	case o.accepted != want:
		add("the database REFUSED %q (SQLSTATE %s, constraint %q), which norm/v1 accepts and the divergence table does not list", o.inserted, o.state, o.constraint)
	}
	if o.accepted && o.stored != o.inserted {
		add("the database stored %q, not the string inserted %q", o.stored, o.inserted)
	}
	if !o.accepted {
		wantState, wantConstraint := "23514", nrmConstraint
		if nrmNeedsEncoding(o.inserted) {
			wantState, wantConstraint = "22021", ""
		}
		if o.state != wantState || o.constraint != wantConstraint {
			add("refused with SQLSTATE %s constraint %q, want %s constraint %q", o.state, o.constraint, wantState, wantConstraint)
		}
	}
	return p
}

func verb(accepted bool) string {
	if accepted {
		return "accepts"
	}
	return "refuses"
}

// nrmTryInsert inserts one address for a user inside a savepoint that is ALWAYS rolled back, and reports what the database did.
func nrmTryInsert(t *testing.T, tx pgx.Tx, user, email string) nrmDBOutcome {
	t.Helper()
	ctx := testCtx(t)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o := nrmDBOutcome{inserted: email}
	err = sp.QueryRow(ctx, "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual') RETURNING email", user, email).Scan(&o.stored)
	if rerr := sp.Rollback(ctx); rerr != nil {
		t.Fatal(rerr)
	}
	switch {
	case err == nil:
		o.accepted = true
	default:
		var pe *pgconn.PgError
		if !errors.As(err, &pe) {
			t.Fatalf("inserting a vector failed with a non-database error: %v", err)
		}
		o.state, o.constraint = pe.Code, pe.ConstraintName
	}
	return o
}

// TestNormAgreement_Database: see the file comment. It fails if the database accepts what norm/v1 refuses or refuses what it accepts without the divergence table naming the vector, if a
// stored value is not the normaliser's output, if the vectors of a group are not one primary key, or if the divergence table is stale or decorative. Its subtests are its controls.
func TestNormAgreement_Database(t *testing.T) {
	f, _ := nrmPlatformFile(t)
	vs := f.Vectors
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	user := ledgerUserA

	var problems []string
	outcomes := make([]nrmDBOutcome, len(vs))
	accepted, refused, divergent := 0, 0, map[string]bool{}
	for i, v := range vs {
		in := nrmRawOf(t, v)
		got, gotErr := norm.Normalize(in)
		email := in // a malformed input is inserted as it came: the database must refuse it
		if gotErr == nil {
			email = got // a well-formed one is inserted as the normaliser returned it
		}
		o := nrmTryInsert(t, tx, user, email)
		outcomes[i] = o
		problems = append(problems, nrmDBProblems(v, got, gotErr, o, nrmDBDivergence)...)
		if o.accepted {
			accepted++
		} else {
			refused++
		}
		if _, ok := nrmDBDivergence[v.Name]; ok && o.accepted != !v.Malformed { // the database did the opposite of the default: the divergence is real
			divergent[v.Name] = true
		}
	}
	if len(problems) != 0 {
		t.Errorf("%d disagreement(s) between the normaliser, the file and platform.suppressions (every one is listed, so one runner pass corrects the divergence table):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	// the divergence table: no stale name, and every entry is a real difference (an entry the database does not bear out would be a vector excused for nothing)
	byName := map[string]nrmVec{}
	for _, v := range vs {
		byName[v.Name] = v
	}
	for _, name := range sortedStringKeys(nrmDBDivergence) {
		v, ok := byName[name]
		if !ok {
			t.Errorf("nrmDBDivergence names %q, which is not a vector of the file", name)
			continue
		}
		if d := nrmDBDivergence[name]; d.dbAccepts == !v.Malformed {
			t.Errorf("nrmDBDivergence lists %q with the database's default behaviour (accepts=%t): it is no divergence", name, d.dbAccepts)
		}
		if !divergent[name] {
			t.Errorf("the divergence %q is not borne out by the database", name)
		}
	}
	if accepted < 100 || refused < 40 || len(nrmDBDivergence) < 10 {
		t.Fatalf("the database leg saw %d accepted and %d refused inserts and %d divergences: the instrument matched too little (floors 100, 40, 10)", accepted, refused, len(nrmDBDivergence))
	}
	t.Logf("NOTE database leg: %d vectors inserted, %d accepted, %d refused, %d of them divergences from norm/v1 that the table names (narrower: %d, wider: %d)", len(vs), accepted, refused, len(nrmDBDivergence), countDiv(false), countDiv(true))

	// identity: the vectors of a group are ONE row of the table (the primary key is (user, email)); the first member of every group is inserted for real, every other member of the group
	// is refused by suppressions_pk, and different groups never collide.
	t.Run("a group is one primary key", func(t *testing.T) {
		members := map[string][]nrmVec{}
		for i, v := range vs {
			if !v.Malformed && outcomes[i].accepted {
				members[v.group()] = append(members[v.group()], v)
			}
		}
		const ins = "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')"
		groups, repeats := 0, 0
		for _, g := range sortedKeysOfVecs(members) {
			ms := members[g]
			ledgerExpectOK(t, tx, "the first member of group "+g, ins, user, ms[0].normalized())
			groups++
			for _, m := range ms[1:] {
				ledgerExpectRefused(t, tx, "group "+g+": "+m.Name+" repeats "+ms[0].Name, "23505", ledgerWhy{Constraint: "suppressions_pk"}, ins, user, m.normalized())
				repeats++
			}
		}
		if groups < 40 || repeats < 30 {
			t.Fatalf("%d groups inserted and %d repeats refused: the instrument matched too little (floors 40 and 30)", groups, repeats)
		}
		t.Logf("NOTE identity: %d groups inserted, %d repeats refused by suppressions_pk", groups, repeats)
	})

	// the database does not normalise for us: the RAW form of a vector is refused by the CHECK while its normalised form was accepted, so the Go side is what makes an address storable
	t.Run("the raw form is refused, the normalised form is accepted", func(t *testing.T) {
		for _, name := range []string{"ascii-upper-all", "trim-both-spaces", "domain-trailing-dot", "idn-composed", "non-ascii-local-decomposed", "domain-fullwidth-letters"} {
			v := byName[name]
			raw := nrmRawOf(t, v)
			if raw == v.normalized() {
				t.Fatalf("control setup: the raw form of %s is already normalised", name)
			}
			if o := nrmTryInsert(t, tx, user, raw); o.accepted || o.state != "23514" || o.constraint != nrmConstraint {
				t.Errorf("%s: the raw input %q was %s (SQLSTATE %s, %q), want a refusal by %s", name, raw, map[bool]string{true: "ACCEPTED", false: "refused"}[o.accepted], o.state, o.constraint, nrmConstraint)
			}
			if o := nrmTryInsert(t, tx, ledgerUserB, v.normalized()); !o.accepted {
				t.Errorf("%s: the normalised form %q was refused (SQLSTATE %s, %q)", name, v.normalized(), o.state, o.constraint)
			}
		}
	})

	t.Run("the comparison is red on a corrupted outcome", func(t *testing.T) { nrmCheckDBComparisonIsRed(t, vs, outcomes) })
}

func countDiv(accepts bool) int {
	n := 0
	for _, d := range nrmDBDivergence {
		if d.dbAccepts == accepts {
			n++
		}
	}
	return n
}

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOfVecs(m map[string][]nrmVec) []string { return sortedStringKeys(m) }

// nrmCheckDBComparisonIsRed is the control of nrmDBProblems: the real outcomes are clean, and each corruption of ONE outcome or ONE normaliser output (one character, one flag) is reported
// with the vector and the reason it must name. A comparison that cannot fail proves nothing.
func nrmCheckDBComparisonIsRed(t *testing.T, vs []nrmVec, outcomes []nrmDBOutcome) {
	t.Helper()
	pick := func(accepts bool, wellFormed bool) int {
		for i, v := range vs {
			if _, ok := nrmDBDivergence[v.Name]; !ok && outcomes[i].accepted == accepts && v.Malformed == !wellFormed {
				return i
			}
		}
		t.Fatalf("setup: no vector that is %v-well-formed and %v-accepted to corrupt", wellFormed, accepts)
		return -1
	}
	run := func(i int, mutate func(got *string, gotErr *error, o *nrmDBOutcome)) []string {
		v := vs[i]
		in := nrmRawOf(t, v)
		got, gotErr := norm.Normalize(in)
		o := outcomes[i]
		mutate(&got, &gotErr, &o)
		return nrmDBProblems(v, got, gotErr, o, nrmDBDivergence)
	}
	good, bad := pick(true, true), pick(false, false)
	if p := run(good, func(*string, *error, *nrmDBOutcome) {}); len(p) != 0 {
		t.Fatalf("control: the uncorrupted outcome of %s is already red: %v", vs[good].Name, p)
	}
	cases := []struct {
		name, mention string
		i             int
		mutate        func(got *string, gotErr *error, o *nrmDBOutcome)
	}{
		{"a normaliser output that differs by one character", "the normaliser returned", good, func(got *string, _ *error, _ *nrmDBOutcome) { *got = flipLast(*got) }},
		{"a normaliser that refuses a well-formed address", "the platform's normaliser says malformed=true", good, func(got *string, e *error, _ *nrmDBOutcome) {
			*got, *e = "", &norm.MalformedError{Reason: norm.ReasonNoAt}
		}},
		{"a normaliser that accepts a malformed address", "the platform's normaliser says malformed=false", bad, func(got *string, e *error, _ *nrmDBOutcome) { *got, *e = "x@example.com", nil }},
		{"a database that refuses a well-formed address", "REFUSED", good, func(_ *string, _ *error, o *nrmDBOutcome) {
			o.accepted, o.state, o.constraint = false, "23514", nrmConstraint
		}},
		{"a database that accepts a malformed address", "ACCEPTED", bad, func(_ *string, _ *error, o *nrmDBOutcome) { o.accepted, o.stored = true, o.inserted }},
		{"a database that stores another string than the one inserted", "the database stored", good, func(_ *string, _ *error, o *nrmDBOutcome) { o.stored = flipLast(o.stored) }},
		{"a refusal by another constraint", "refused with SQLSTATE", bad, func(_ *string, _ *error, o *nrmDBOutcome) {
			o.state, o.constraint = "23514", "suppressions_source_vocabulary"
		}},
		{"a refusal of another kind", "refused with SQLSTATE", bad, func(_ *string, _ *error, o *nrmDBOutcome) { o.state, o.constraint = "23505", "suppressions_pk" }},
	}
	for _, c := range cases {
		p := run(c.i, c.mutate)
		if len(p) == 0 {
			t.Errorf("%s: the comparison found nothing wrong", c.name)
			continue
		}
		if !strings.Contains(strings.Join(p, "\n"), c.mention) || !strings.HasPrefix(p[0], vs[c.i].Name+":") {
			t.Errorf("%s: red, but not naming %s and %q: %v", c.name, vs[c.i].Name, c.mention, p)
		}
	}
	// a vector listed as a divergence, whose database behaviour is the DEFAULT, is reported by the divergence check (the table is not decorative): the named vector with its table entry flipped
	for name, d := range nrmDBDivergence {
		flipped := map[string]nrmDivergence{name: {!d.dbAccepts, "flipped by the control"}}
		for i, v := range vs {
			if v.Name != name {
				continue
			}
			got, gotErr := norm.Normalize(nrmRawOf(t, v))
			if p := nrmDBProblems(v, got, gotErr, outcomes[i], flipped); len(p) == 0 {
				t.Errorf("the divergence entry of %s flipped goes unreported", name)
			}
		}
		break // one entry is enough: the table is a map and the property does not depend on which
	}
}

func flipLast(s string) string {
	if s == "" {
		return "x"
	}
	r, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size] + string(r+1)
}

// ---- 2. module 1, live ---------------------------------------------------------------------------------------------------------------------------------------------------------------

// nrmM1Create is the 202 of POST /v1/verify/bulk, the fields this test reads (module 1: internal/api/api.go handleBulkSubmit).
type nrmM1Create struct {
	JobID  string `json:"job_id"`
	Counts struct {
		Total     int `json:"total"`
		Enqueued  int `json:"enqueued"`
		Duplicate int `json:"duplicate"`
		Malformed int `json:"malformed"`
	} `json:"counts"`
	MalformedRows []struct {
		Ordinal int    `json:"ordinal"`
		ItemID  string `json:"item_id"`
	} `json:"malformed_rows"`
	MalformedRowsTruncated bool `json:"malformed_rows_truncated"`
}

// nrmSubmitRaw posts the job itself, because m1results.Client.Submit returns only the counts and this test needs malformed_rows. The key is sent and never printed; no error carries the URL.
func nrmSubmitRaw(ctx context.Context, base string, key config.Secret, items []m1results.Item) (nrmM1Create, error) {
	var out nrmM1Create
	body, err := json.Marshal(struct {
		Items []m1results.Item `json:"items"`
	}{items})
	if err != nil {
		return out, errors.New("the items could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/verify/bulk", bytes.NewReader(body))
	if err != nil {
		return out, errors.New("the request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // a per-caller key must never be handed to a proxy
	hc := &http.Client{Timeout: 2 * time.Minute, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return out, errors.New("the request failed (no response)")
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return out, errors.New("the response body could not be read")
	}
	if resp.StatusCode != http.StatusAccepted {
		return out, fmt.Errorf("POST /v1/verify/bulk answered HTTP %d, want 202", resp.StatusCode)
	}
	if err := json.Unmarshal(b, &out); err != nil || out.JobID == "" {
		return out, errors.New("the 202 is not the documented shape")
	}
	return out, nil
}

// nrmM1Answer is what module 1 said about a job whose item_id is the vector's index in the file and whose ordinals are positions in the submitted list.
type nrmM1Answer struct {
	counts    struct{ total, enqueued, duplicate, malformed int }
	truncated bool
	malformed map[int]bool // item_id -> listed in the create response's malformed_rows
	status    map[int]string
	reason    map[int]string
	dupOf     map[int]int // item_id -> the item_id of the row it duplicates; absent when the row is not a duplicate
}

// nrmM1Problems compares what module 1 said with what the file says, as sentences: sub is the submitted vectors in submission order with their file indices.
func nrmM1Problems(vs []nrmVec, sub []int, a nrmM1Answer) []string {
	var p []string
	add := func(f string, args ...any) { p = append(p, fmt.Sprintf(f, args...)) }
	firstOfGroup := map[string]int{} // group -> file index of its first submitted well-formed member
	wantDup := map[int]int{}
	nMal, nGroups, nWell := 0, 0, 0
	for _, idx := range sub {
		v := vs[idx]
		if v.Malformed {
			nMal++
			continue
		}
		nWell++
		g := v.group()
		if first, ok := firstOfGroup[g]; ok {
			wantDup[idx] = first
		} else {
			firstOfGroup[g] = idx
			nGroups++
		}
	}
	if a.counts.total != len(sub) || a.counts.malformed != nMal || a.counts.duplicate != nWell-nGroups || a.counts.enqueued != nGroups {
		add("module 1's counts are total %d, enqueued %d, duplicate %d, malformed %d; the file implies total %d, enqueued %d, duplicate %d, malformed %d",
			a.counts.total, a.counts.enqueued, a.counts.duplicate, a.counts.malformed, len(sub), nGroups, nWell-nGroups, nMal)
	}
	if a.truncated {
		add("module 1 truncated the malformed_rows list: the comparison below is of a partial list")
	}
	for _, idx := range sub {
		v := vs[idx]
		switch {
		case v.Malformed && !a.malformed[idx]:
			add("%s (item %d): the file says MALFORMED, module 1 did not list it in malformed_rows", v.Name, idx)
		case !v.Malformed && a.malformed[idx]:
			add("%s (item %d): the file says well formed (%q), module 1 listed it as malformed", v.Name, idx, v.normalized())
		}
		if v.Malformed {
			if a.status[idx] != "invalid" || a.reason[idx] != "invalid_syntax" {
				add("%s (item %d): a malformed row came back %s/%s, want invalid/invalid_syntax", v.Name, idx, a.status[idx], a.reason[idx])
			}
			if d, dup := a.dupOf[idx]; dup {
				add("%s (item %d): a malformed row is marked a duplicate of item %d: a repeated malformed address is never a duplicate", v.Name, idx, d)
			}
			continue
		}
		got, isDup := a.dupOf[idx]
		want, shouldDup := wantDup[idx]
		switch {
		case shouldDup && !isDup:
			add("%s (item %d, group %q): the file says it repeats item %d (%s), module 1 did not mark it a duplicate", v.Name, idx, v.group(), want, vs[want].Name)
		case !shouldDup && isDup:
			add("%s (item %d, group %q): the file says it is the first of its group, module 1 marks it a duplicate of item %d (%s)", v.Name, idx, v.group(), got, vs[got].Name)
		case shouldDup && got != want:
			add("%s (item %d, group %q): module 1 says it duplicates item %d (%s), the file says item %d (%s)", v.Name, idx, v.group(), got, vs[got].Name, want, vs[want].Name)
		}
	}
	return p
}

// nrmSimulatedM1 builds the answer module 1 WOULD give if its normalisation of the submitted vectors were the strings given (norm[i] for file index i, "" for refused): the first row of each
// string is the original and later rows are duplicates of it. It is what the control feeds the comparison.
func nrmSimulatedM1(vs []nrmVec, sub []int, normalized map[int]string, refused map[int]bool) nrmM1Answer {
	a := nrmM1Answer{malformed: map[int]bool{}, status: map[int]string{}, reason: map[int]string{}, dupOf: map[int]int{}}
	first := map[string]int{}
	for _, idx := range sub {
		a.counts.total++
		if refused[idx] {
			a.counts.malformed++
			a.malformed[idx] = true
			a.status[idx], a.reason[idx] = "invalid", "invalid_syntax"
			continue
		}
		s := normalized[idx]
		if f, ok := first[s]; ok {
			a.counts.duplicate++
			a.dupOf[idx] = f
		} else {
			first[s] = idx
			a.counts.enqueued++
		}
		a.status[idx], a.reason[idx] = "unknown", "dns_error"
	}
	return a
}

// TestM1Live_NormAgreement: see the file comment. It fails if module 1 refuses a vector the file calls well formed, accepts one it calls malformed, lets a malformed row be a duplicate,
// misses a repeat inside a group, merges two groups, or does not finish the job. Its subtests are its controls: the comparison is clean on module 1's expected answer and red on every
// corruption of it, including a normaliser that differs by one character on one vector.
func TestM1Live_NormAgreement(t *testing.T) {
	f, _ := nrmPlatformFile(t)
	vs := f.Vectors
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// which vectors can travel: JSON carries only valid UTF-8, so a vector with bytes that are not UTF-8 is counted as NOT CHECKED here (the unit tests and the database leg run it)
	var sub []int
	skipped := 0
	items := []m1results.Item{}
	for i, v := range vs {
		in := nrmRawOf(t, v)
		if !utf8.ValidString(in) {
			skipped++
			continue
		}
		sub = append(sub, i)
		items = append(items, m1results.Item{ItemID: strconv.Itoa(i), Address: in})
	}
	// module 1's limits (module 1: internal/api/api.go maxBulkAddresses = 100000 rows, bulk_rows.go maxBulkBodyBytes = 64 MiB) are far above this job: one request is enough, and it must be ONE job
	// because duplicate_of exists only inside a job.
	if len(items) < 100 || len(items) > 100000 {
		t.Fatalf("%d addresses to submit: outside 100..100000 (module 1's own cap per request)", len(items))
	}

	base := m3lBaseURL(t, m1bEnvURL)
	key := config.Secret(strings.TrimSpace(requireEnv(t, m1bEnvKey)))
	create, err := nrmSubmitRaw(ctx, base, key, items)
	if err != nil {
		t.Fatalf("submitting the vectors to module 1 failed: %v\n(a 400 here means module 1 does not take the `items` shape: is the pinned module 1 at the commit that has A1, the per-item identity?)", err)
	}
	t.Logf("NOTE module 1 accepted the job: total %d, enqueued %d, duplicate %d, malformed %d (%d vector(s) with bytes that are not UTF-8 were not sent: NOT CHECKED here)", create.Counts.Total, create.Counts.Enqueued, create.Counts.Duplicate, create.Counts.Malformed, skipped)

	c, err := m1results.New(base, key, nil, modclient.Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("cannot build the module 1 client: %v", err)
	}
	c.PageLimit = 200
	c.Poll = time.Second
	ans := nrmM1Answer{malformed: map[int]bool{}, status: map[int]string{}, reason: map[int]string{}, dupOf: map[int]int{}, truncated: create.MalformedRowsTruncated}
	ans.counts.total, ans.counts.enqueued, ans.counts.duplicate, ans.counts.malformed = create.Counts.Total, create.Counts.Enqueued, create.Counts.Duplicate, create.Counts.Malformed
	for _, m := range create.MalformedRows {
		id, err := strconv.Atoi(m.ItemID)
		if err != nil || m.Ordinal < 0 || m.Ordinal >= len(sub) || sub[m.Ordinal] != id {
			t.Fatalf("module 1 listed the malformed row (ordinal %d, item_id %q), which is not a row of the submitted list in that position", m.Ordinal, m.ItemID)
		}
		ans.malformed[id] = true
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, 12*time.Minute)
	defer cancelDrain()
	var rows []m1results.Row
	if _, err := c.Drain(drainCtx, create.JobID, func(page []m1results.Row) error { rows = append(rows, page...); return nil }); err != nil {
		t.Fatalf("draining module 1's job failed: %v", err)
	}
	if len(rows) != len(sub) {
		t.Fatalf("module 1 returned %d results for %d submitted rows", len(rows), len(sub))
	}
	syntaxLayer := 0
	for _, r := range rows {
		if r.Ordinal < 0 || r.Ordinal >= len(sub) {
			t.Fatalf("a result has the ordinal %d, outside 0..%d", r.Ordinal, len(sub)-1)
		}
		id := sub[r.Ordinal]
		if r.ItemID != strconv.Itoa(id) {
			t.Fatalf("the result of ordinal %d carries item_id %q, want %d: a result must attribute to exactly one input row", r.Ordinal, r.ItemID, id)
		}
		ans.status[id], ans.reason[id] = r.Status, r.Reason
		if r.DuplicateOf != nil {
			if *r.DuplicateOf < 0 || *r.DuplicateOf >= len(sub) {
				t.Fatalf("the result of ordinal %d says it duplicates ordinal %d, outside the job", r.Ordinal, *r.DuplicateOf)
			}
			ans.dupOf[id] = sub[*r.DuplicateOf]
		}
		if !vs[id].Malformed && r.DuplicateOf == nil && r.Status == "invalid" && r.Reason == "invalid_syntax" {
			syntaxLayer++
		}
	}

	if problems := nrmM1Problems(vs, sub, ans); len(problems) != 0 {
		t.Errorf("%d disagreement(s) between module 1 and the vector file:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("NOTE %d well-formed first rows were answered invalid/invalid_syntax by module 1's syntax layer after normalising (printed, not asserted: a local part over 64 bytes, a domain over 255 bytes or with no dot); the platform charges them, row M1-10", syntaxLayer)

	t.Run("the comparison is clean on the answer the file implies and red on every corruption of it", func(t *testing.T) { nrmCheckM1ComparisonIsRed(t, vs, sub) })
}

// nrmCheckM1ComparisonIsRed is the control of nrmM1Problems: the answer module 1 would give if it agreed with the file is clean, and each corruption of it (a normaliser one character off on
// one vector, a well-formed address refused, a malformed one accepted, a malformed row marked a duplicate, a duplicate_of naming the wrong row, two groups merged, a wrong count) is red with
// the reason it must name. sub is the submitted list: file indices in submission order.
func nrmCheckM1ComparisonIsRed(t *testing.T, vs []nrmVec, sub []int) {
	t.Helper()
	norms := map[int]string{}
	refused := map[int]bool{}
	for _, idx := range sub {
		if vs[idx].Malformed {
			refused[idx] = true
		} else {
			norms[idx] = vs[idx].normalized()
		}
	}
	clean := nrmSimulatedM1(vs, sub, norms, refused)
	if p := nrmM1Problems(vs, sub, clean); len(p) != 0 {
		t.Fatalf("control: the answer the file implies is already red (%d problems, first: %s): the simulation or the comparison is wrong", len(p), p[0])
	}
	// a normaliser that differs by ONE CHARACTER on one vector: it splits that vector off its group (or merges it into another)
	var victim, other int = -1, -1
	for _, idx := range sub {
		v := vs[idx]
		if !v.Malformed && v.group() == "jane" {
			if victim < 0 {
				victim = idx
			} else if other < 0 {
				other = idx
			}
		}
	}
	if victim < 0 || other < 0 {
		t.Fatal("setup: the group jane has fewer than two submitted members")
	}
	mutated := map[int]string{}
	for k, s := range norms {
		mutated[k] = s
	}
	mutated[other] = flipLast(norms[other]) // the second member of the group now differs by one character
	cases := []struct {
		name, mention string
		ans           nrmM1Answer
	}{
		{"a normaliser one character off on one vector", "module 1 did not mark it a duplicate", nrmSimulatedM1(vs, sub, mutated, refused)},
	}
	// a well-formed vector refused, a malformed one accepted
	wf := firstIndex(vs, sub, func(v nrmVec) bool { return !v.Malformed })
	mf := firstIndex(vs, sub, func(v nrmVec) bool { return v.Malformed })
	refusedMore := map[int]bool{wf: true}
	for k := range refused {
		refusedMore[k] = true
	}
	cases = append(cases, struct {
		name, mention string
		ans           nrmM1Answer
	}{"module 1 refusing a well-formed address", "module 1 listed it as malformed", nrmSimulatedM1(vs, sub, norms, refusedMore)})
	refusedLess := map[int]bool{}
	for k := range refused {
		if k != mf {
			refusedLess[k] = true
		}
	}
	normsMore := map[int]string{}
	for k, s := range norms {
		normsMore[k] = s
	}
	normsMore[mf] = "accepted-by-mistake@example.com"
	cases = append(cases, struct {
		name, mention string
		ans           nrmM1Answer
	}{"module 1 accepting a malformed address", "module 1 did not list it in malformed_rows", nrmSimulatedM1(vs, sub, normsMore, refusedLess)})
	// a malformed row marked as a duplicate of another malformed row
	dup := nrmSimulatedM1(vs, sub, norms, refused)
	dup.dupOf[mf] = victim
	cases = append(cases, struct {
		name, mention string
		ans           nrmM1Answer
	}{"a malformed row reported as a duplicate", "a repeated malformed address is never a duplicate", dup})
	// a repeat that points at the wrong row
	wrong := nrmSimulatedM1(vs, sub, norms, refused)
	wrong.dupOf[other] = other
	cases = append(cases, struct {
		name, mention string
		ans           nrmM1Answer
	}{"a duplicate_of that names the wrong row", "the file says item", wrong})
	// two groups merged by one extra duplicate_of
	merged := nrmSimulatedM1(vs, sub, norms, refused)
	firstOther := firstIndex(vs, sub, func(v nrmVec) bool { return !v.Malformed && v.group() != vs[victim].group() })
	merged.dupOf[firstOther] = victim
	cases = append(cases, struct {
		name, mention string
		ans           nrmM1Answer
	}{"two groups merged", "the file says it is the first of its group", merged})
	// the counts
	counts := nrmSimulatedM1(vs, sub, norms, refused)
	counts.counts.duplicate++
	cases = append(cases, struct {
		name, mention string
		ans           nrmM1Answer
	}{"a duplicate count off by one", "module 1's counts are", counts})
	for _, c := range cases {
		p := nrmM1Problems(vs, sub, c.ans)
		if len(p) == 0 {
			t.Errorf("%s: the comparison found nothing wrong", c.name)
			continue
		}
		if !strings.Contains(strings.Join(p, "\n"), c.mention) {
			t.Errorf("%s: red, but not saying %q: %v", c.name, c.mention, firstStrings(p, 3))
		}
	}
}

func firstIndex(vs []nrmVec, sub []int, pred func(nrmVec) bool) int {
	for _, idx := range sub {
		if pred(vs[idx]) {
			return idx
		}
	}
	return -1
}

func firstStrings(p []string, n int) []string {
	if len(p) > n {
		return p[:n]
	}
	return p
}

// ---- 3. the shared file ---------------------------------------------------------------------------------------------------------------------------------------------------------------

// nrmFirstDifference describes where two byte strings first differ, as a sentence ("" when they are equal).
func nrmFirstDifference(a, b []byte) string {
	if bytes.Equal(a, b) {
		return ""
	}
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	line := 1 + bytes.Count(a[:i], []byte{'\n'})
	snip := func(x []byte) string {
		lo, hi := max(i-20, 0), min(i+20, len(x))
		if lo > len(x) {
			return "<the file ends before this point>"
		}
		return strconv.QuoteToASCII(string(x[lo:hi]))
	}
	return fmt.Sprintf("the two copies are %d and %d bytes and first differ at byte %d (line %d): platform ...%s..., module 1 ...%s...", len(a), len(b), i, line, snip(a), snip(b))
}

var nrmRequireRe = regexp.MustCompile(`(?m)^\s*(?:require\s+)?(golang\.org/x/(?:net|text))\s+(v[0-9][^\s]*)`)

// nrmLibraryVersions returns the version of golang.org/x/net and golang.org/x/text a go.mod requires (the first line that names each).
func nrmLibraryVersions(goMod string) map[string]string {
	out := map[string]string{}
	for _, m := range nrmRequireRe.FindAllStringSubmatch(goMod, -1) {
		if _, ok := out[m[1]]; !ok {
			out[m[1]] = m[2]
		}
	}
	return out
}

// TestModulesLive_NormSharedFile: the platform's vector file and module 1's are the same bytes, and the two go.mod files pin the same x/net and x/text. Module 1's copy is the file module 1's
// own test runs through ITS Normalize (control-plane/internal/hash), so equality is what makes "the platform agrees with module 1" a statement about module 1's function and not about this
// repository's copy of a file. Until module 1 is re-pinned to a commit that carries the file this test CANNOT pass: it says so.
func TestModulesLive_NormSharedFile(t *testing.T) {
	_, mine := nrmPlatformFile(t)
	modules := shapeModulesRoot(t)
	theirsPath := filepath.Join(modules, filepath.FromSlash(nrmM1VectorRel))
	theirs, err := os.ReadFile(theirsPath)
	if err != nil {
		t.Fatalf("module 1's copy of the vector file does not exist at the pinned commit: coldmail-verify/control-plane/internal/hash/testdata/norm-vectors.json (%v).\n"+
			"Module 1 must carry the file and an own test that runs it through its Normalize; re-pin tools/modules.lock to the module 1 commit that adds them, and the parent repository's submodule pointer with it.", err)
	}
	if d := nrmFirstDifference(mine, theirs); d != "" {
		sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:8]) }
		t.Errorf("the platform's vector file (%s) and module 1's copy differ (sha256 prefixes %s and %s): %s.\nThey must be one file: copy the newer one over the older in BOTH repositories in the same change, and re-pin.", nrmVectorRel, sum(mine), sum(theirs), d)
	}

	goMod, err := os.ReadFile(filepath.Join(requireEnv(t, envRepoRoot), "go.mod"))
	if err != nil {
		t.Fatalf("reading the platform's go.mod: %v", err)
	}
	m1Mod, err := os.ReadFile(filepath.Join(modules, filepath.FromSlash(nrmM1GoModRel)))
	if err != nil {
		t.Fatalf("reading module 1's go.mod: %v", err)
	}
	mineV, theirsV := nrmLibraryVersions(string(goMod)), nrmLibraryVersions(string(m1Mod))
	for _, lib := range []string{"golang.org/x/net", "golang.org/x/text"} {
		if mineV[lib] == "" || theirsV[lib] == "" {
			t.Errorf("%s: the platform requires %q and module 1 requires %q: both must require it (the IDNA and NFC tables are those of this library)", lib, mineV[lib], theirsV[lib])
		} else if mineV[lib] != theirsV[lib] {
			t.Errorf("%s: the platform requires %s and module 1 requires %s: norm/v1's answer depends on the library's Unicode tables, so both must build from one version", lib, mineV[lib], theirsV[lib])
		}
	}
	t.Logf("NOTE shared file: %d bytes each; x/net %s and x/text %s in both go.mod files. NOT CHECKED: that module 1's own test runs the file (that is module 1's CI), and the Go release that compiles each side (it selects the Unicode tables of x/net/idna and x/text).", len(mine), mineV["golang.org/x/net"], mineV["golang.org/x/text"])

	t.Run("a one-character difference is found, and where", func(t *testing.T) {
		if d := nrmFirstDifference(mine, mine); d != "" {
			t.Fatalf("control: a file differs from itself: %s", d)
		}
		for _, at := range []int{0, len(mine) / 2, len(mine) - 1} {
			other := append([]byte(nil), mine...)
			other[at] ^= 0x01
			d := nrmFirstDifference(mine, other)
			if d == "" || !strings.Contains(d, fmt.Sprintf("first differ at byte %d ", at)) {
				t.Errorf("a one-bit change at byte %d is reported as %q", at, d)
			}
		}
		if d := nrmFirstDifference(mine, mine[:len(mine)-1]); !strings.Contains(d, fmt.Sprintf("first differ at byte %d ", len(mine)-1)) {
			t.Errorf("a truncated copy is reported as %q", d)
		}
	})
	t.Run("the library versions are read from a go.mod, and a different version is a difference", func(t *testing.T) {
		const a = "module x\n\ngo 1.26.0\n\nrequire (\n\tgolang.org/x/net v0.62.0\n\tgolang.org/x/text v0.43.0 // indirect\n)\n"
		got := nrmLibraryVersions(a)
		if got["golang.org/x/net"] != "v0.62.0" || got["golang.org/x/text"] != "v0.43.0" {
			t.Fatalf("control: the versions of a known go.mod are read as %v", got)
		}
		if b := nrmLibraryVersions(strings.Replace(a, "v0.43.0", "v0.44.0", 1)); b["golang.org/x/text"] == got["golang.org/x/text"] {
			t.Error("a go.mod with another x/text version reads as the same version")
		}
		if c := nrmLibraryVersions("module x\n\nrequire github.com/jackc/pgx/v5 v5.13.0\n"); len(c) != 0 {
			t.Errorf("a go.mod with neither library reads as %v", c)
		}
		single := nrmLibraryVersions("module x\n\nrequire golang.org/x/net v0.1.0\nrequire golang.org/x/text v0.2.0\n")
		if single["golang.org/x/net"] != "v0.1.0" || single["golang.org/x/text"] != "v0.2.0" {
			t.Errorf("the single-line require form is read as %v", single)
		}
	})
}
