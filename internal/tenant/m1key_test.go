package tenant

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/config"
)

// THE PINNED VECTORS. The derivation is a contract with the rows already written in module 1's database: changing one byte of it changes every user's key. Each expected value was computed
// outside this code, twice and independently (Python's hmac/hashlib/base64 and `openssl dgst -sha256 -hmac ... | base64`, which agreed), so a mistake in the Go derivation cannot hide in a test
// that runs the same Go.
//
//	material = "vector-" x 6 (42 bytes)         message = "m1-caller|" + user          out = base64url-no-padding(HMAC-SHA256(material, message))
//	sum      = hex(SHA-256(the text of out))    wrong    = hex(SHA-256(the 32 raw HMAC bytes))   (the digest rule module 1 does NOT use)
var vectorMaterial = strings.Repeat("vector-", 6)

var vectors = []struct{ user, material, out, sum, wrong string }{
	{"11111111-1111-4111-8111-111111111111", vectorMaterial, "RDXStwhy1zpOmq8-zRf6OEaTB5Xi-IdX0B8VL5G2DyA",
		"2019400fe7790eb4fcd58346c5875dff2d32c864e8397c5e80865e8032388fcb", "7fd377dfb9660635473d50dfbfc2ff015b451f9593ec6359608d5b5f2e6c7f95"},
	{"22222222-2222-4222-8222-222222222222", vectorMaterial, "UTpcxZtA6nJM2Tsnalo8UqzDMDe-0Me9uZRIonyaqe4",
		"239905399be668038d54a563f7bd61f2059dba0bdd047d57a5ed85da8b051e7a", "3219b0ede7e9b54ef0b5b8540e28b4b758940bd2c86b0fbb050849cda6b7b5ea"},
	// the first user's id under another material: the output depends on the secret
	{"11111111-1111-4111-8111-111111111111", strings.Repeat("other-", 6), "RG9vmy-c9MCUYVHuHtte1PsNUZpHGHUGRwm1JBmYBRQ", "", ""},
}

func TestDeriveM1Key_PinnedVectors(t *testing.T) {
	if len(vectors) < 3 {
		t.Fatal("the vector list shrank")
	}
	for _, v := range vectors {
		got, err := DeriveM1Key(config.Secret(v.material), v.user)
		if err != nil {
			t.Fatalf("user %s: %v", v.user, err)
		}
		if got.Reveal() != v.out {
			t.Errorf("user %s, material %q...: the derived text is %q, the pinned vector is %q: the derivation changed", v.user, v.material[:6], got.Reveal(), v.out)
		}
		if len(got.Reveal()) != 43 || strings.ContainsAny(got.Reveal(), "=+/ \t\r\n") {
			t.Errorf("the derived text must be 43 characters of base64url without padding, got %d: %q", len(got.Reveal()), got.Reveal())
		}
		if v.sum == "" {
			continue
		}
		if sum := hex.EncodeToString(M1KeyDigest(got)); sum != v.sum {
			t.Errorf("user %s: the digest is %s, the pinned vector is %s", v.user, sum, v.sum)
		}
		// the control: the digest module 1 does NOT compute (over the raw HMAC bytes) is a different value, so the vector can tell the two rules apart
		raw, err := base64.RawURLEncoding.DecodeString(got.Reveal())
		if err != nil {
			t.Fatal(err)
		}
		if wrong := sha256.Sum256(raw); hex.EncodeToString(wrong[:]) != v.wrong || v.wrong == v.sum {
			t.Errorf("user %s: the digest over the raw bytes is %x, the pinned wrong-rule value is %s", v.user, wrong, v.wrong)
		}
	}
	// the independent recomputation inside the test, so a future vector edit is checked against the standard library too
	mac := hmac.New(sha256.New, []byte(vectorMaterial))
	mac.Write([]byte("m1-caller|" + vectors[0].user))
	if got := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); got != vectors[0].out {
		t.Fatalf("the pinned vector %q is not what the standard library computes (%q)", vectors[0].out, got)
	}
}

func TestDeriveM1Key_DependsOnTheUserAndTheSecretAndOnNothingElse(t *testing.T) {
	secret := config.Secret(word("derive-", 45))
	a1, err := DeriveM1Key(secret, userA)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := DeriveM1Key(secret, userA)
	b, _ := DeriveM1Key(secret, userB)
	if a1 != a2 {
		t.Errorf("the same secret and user gave two keys")
	}
	if a1 == b || bytes.Equal(M1KeyDigest(a1), M1KeyDigest(b)) {
		t.Errorf("two users share a key or a digest")
	}
	// one character of the user id, one character of the secret, each changes the key
	oneOff := []byte(userA)
	oneOff[len(oneOff)-1] = 'b'
	if k, _ := DeriveM1Key(secret, string(oneOff)); k == a1 {
		t.Errorf("a user id one character away gave the same key")
	}
	if k, _ := DeriveM1Key(config.Secret(secret.Reveal()+"x"), userA); k == a1 {
		t.Errorf("a secret one character longer gave the same key")
	}
	if k, _ := DeriveM1Key(config.Secret("y"+secret.Reveal()[1:]), userA); k == a1 {
		t.Errorf("a secret one character different gave the same key")
	}
	// the digest is a function of the key text alone
	if !bytes.Equal(M1KeyDigest(a1), M1KeyDigest(a2)) || len(M1KeyDigest(a1)) != 32 {
		t.Errorf("the digest is not stable or not 32 bytes")
	}
}

func TestDeriveM1Key_Refusals(t *testing.T) {
	ok := config.Secret(word("refuse-", 40))
	weak := config.Secret(word("w", 31))
	for _, c := range []struct {
		name   string
		secret config.Secret
		user   string
		want   error
	}{
		{"no secret", "", userA, ErrNoM1KeySecret},
		{"a 31-byte secret", weak, userA, ErrWeakSecret},
		{"an empty user id", ok, "", ErrBadID},
		{"a user id in upper case (two spellings would be two keys)", ok, strings.ToUpper(userA), ErrBadID},
		{"a user id that is not an id", ok, "not-an-id", ErrBadID},
		{"the system user", ok, SystemUserID, ErrSystemUser},
	} {
		got, err := DeriveM1Key(c.secret, c.user)
		if !errors.Is(err, c.want) || got != "" {
			t.Errorf("%s: %q, %v; want no key and errors.Is(%v)", c.name, got.Reveal(), err, c.want)
		}
		if err != nil && c.secret != "" && strings.Contains(err.Error(), c.secret.Reveal()) {
			t.Errorf("%s: the error carries the secret: %v", c.name, err)
		}
	}
	// the control: a 32-byte secret is the first that derives
	if _, err := DeriveM1Key(config.Secret(word("w", 32)), userA); err != nil {
		t.Errorf("a 32-byte secret was refused: %v", err)
	}
}

func TestM1CallerID_IsPrefixedAndHoldsNoPII(t *testing.T) {
	if got := M1CallerID(userA); got != "platform-user-"+userA {
		t.Errorf("M1CallerID = %q", got)
	}
}

// ---- ProvisionM1Caller against a recording connection ----

type fakeAdmin struct {
	execs, queries []dbCall
	execErr        error
	read           pgx.Row
}

func (f *fakeAdmin) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, dbCall{sql, args})
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeAdmin) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queries = append(f.queries, dbCall{sql, args})
	return f.read
}

func ownerRow(owner string, valid bool) pgx.Row {
	return rowFunc(func(dest ...any) error {
		if len(dest) != 2 {
			return fmt.Errorf("the read-back scans %d columns, the query returns 2", len(dest))
		}
		*(dest[0].(*string)), *(dest[1].(*bool)) = owner, valid
		return nil
	})
}

func TestProvisionM1Caller_RefusesWhatWouldLeaveANullOwnerBeforeAnyStatement(t *testing.T) {
	secret := word("provision-", 44)
	r := resolverOver(t, nil, secret)
	for _, c := range []struct {
		name, id string
		want     error
	}{
		{"an empty id", "", ErrBadID},
		{"a blank id", "   ", ErrBadID},
		{"a word", "not-an-id", ErrBadID},
		{"the NULL literal", "NULL", ErrBadID},
		{"upper case", strings.ToUpper(userA), ErrBadID},
		{"the system user", SystemUserID, ErrSystemUser},
	} {
		adm := &fakeAdmin{read: ownerRow(c.id, true)}
		if err := r.ProvisionM1Caller(context.Background(), adm, c.id); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want errors.Is(%v)", c.name, err, c.want)
		}
		if len(adm.execs)+len(adm.queries) != 0 {
			t.Errorf("%s: %d statement(s) reached the database", c.name, len(adm.execs)+len(adm.queries))
		}
	}
	// no secret, no connection: refused before any statement as well
	adm := &fakeAdmin{read: ownerRow(userA, true)}
	if err := resolverOver(t, nil, "").ProvisionM1Caller(context.Background(), adm, userA); !errors.Is(err, ErrNoM1KeySecret) || len(adm.execs)+len(adm.queries) != 0 {
		t.Errorf("without the secret: %v, %d statement(s)", err, len(adm.execs)+len(adm.queries))
	}
	if err := r.ProvisionM1Caller(context.Background(), nil, userA); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("without a connection: %v, want ErrNoDatabase", err)
	}
	if err := (*Resolver)(nil).ProvisionM1Caller(context.Background(), adm, userA); err == nil || len(adm.execs)+len(adm.queries) != 0 {
		t.Errorf("a nil resolver: %v, %d statement(s)", err, len(adm.execs)+len(adm.queries))
	}

	// THE CONTROL: a good id writes exactly one row, with the owner bound as the third parameter, the digest of the derived key as the second, and the schema named in both statements
	adm = &fakeAdmin{read: ownerRow(userA, true)}
	if err := r.ProvisionM1Caller(context.Background(), adm, userA); err != nil {
		t.Fatalf("control: %v", err)
	}
	if len(adm.execs) != 1 || len(adm.queries) != 1 {
		t.Fatalf("control: %d insert(s) and %d read(s), want 1 and 1", len(adm.execs), len(adm.queries))
	}
	ins := adm.execs[0]
	key, _ := DeriveM1Key(config.Secret(secret), userA)
	if len(ins.args) != 3 || ins.args[0] != "platform-user-"+userA || ins.args[2] != userA || ins.args[2] == "" {
		t.Fatalf("control: the insert's parameters are %v, want (caller id, digest, the user id)", ins.args)
	}
	if d, ok := ins.args[1].([]byte); !ok || hex.EncodeToString(d) != hex.EncodeToString(M1KeyDigest(key)) || len(d) != 32 {
		t.Errorf("control: the second parameter is not the 32-byte digest of the derived key: %v", ins.args[1])
	}
	for _, want := range []string{"INSERT INTO verify.api_callers", "ON CONFLICT (key_sha256) DO NOTHING", "$3::uuid", "not_before"} {
		if !strings.Contains(ins.sql, want) {
			t.Errorf("control: the insert does not contain %q:\n%s", want, ins.sql)
		}
	}
	if !strings.Contains(adm.queries[0].sql, "FROM verify.api_callers WHERE key_sha256 = $1") {
		t.Errorf("control: the read-back does not name its schema and key:\n%s", adm.queries[0].sql)
	}
	for _, sql := range []string{ins.sql, adm.queries[0].sql} {
		if strings.Contains(strings.ToLower(sql), "public.") || strings.Contains(sql, " api_callers") {
			t.Errorf("a statement names a table without its schema or in public:\n%s", sql)
		}
	}
}

func TestProvisionM1Caller_JudgesTheRowItLeavesAndNeverPutsTheKeyInAnError(t *testing.T) {
	secret := word("judge-", 44)
	r := resolverOver(t, nil, secret)
	key, _ := DeriveM1Key(config.Secret(secret), userA)
	secrets := []string{secret, key.Reveal(), hex.EncodeToString(M1KeyDigest(key))}

	for _, c := range []struct {
		name  string
		row   pgx.Row
		want  error // nil: success
		plain bool  // an error that is none of the typed ones
	}{
		{"the row names this user and is valid (the idempotent second call)", ownerRow(userA, true), nil, false},
		{"a row with no owner (the silent trap: jobs nobody can read)", ownerRow("", true), ErrM1CallerConflict, false},
		{"a row that names another user", ownerRow(userB, true), ErrM1CallerConflict, false},
		{"a row outside its validity window", ownerRow(userA, false), ErrM1CallerConflict, false},
		{"no row after the insert", errRow(pgx.ErrNoRows), nil, true},
		{"the read failed", errRow(errors.New("connection reset")), nil, true},
	} {
		adm := &fakeAdmin{read: c.row}
		err := r.ProvisionM1Caller(context.Background(), adm, userA)
		switch {
		case c.want == nil && !c.plain && err != nil:
			t.Errorf("%s: %v, want success", c.name, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: %v, want errors.Is(%v)", c.name, err, c.want)
		case c.plain && (err == nil || errors.Is(err, ErrM1CallerConflict)):
			t.Errorf("%s: %v, want a plain failure that is not a conflict", c.name, err)
		}
		if len(adm.execs) != 1 {
			t.Errorf("%s: %d insert(s), want exactly 1 (a conflict is judged after the insert, and nothing is rewritten)", c.name, len(adm.execs))
		}
		if err != nil {
			for _, s := range secrets {
				if strings.Contains(err.Error(), s) {
					t.Errorf("%s: the error carries the secret, the key or its digest: %v", c.name, err)
				}
			}
		}
	}

	// the insert itself fails (the runtime role: SQLSTATE 42501): the driver's error is reachable, nothing is read back, the text carries no key material
	denied := &pgconn.PgError{Code: "42501", Message: "permission denied for table api_callers"}
	adm := &fakeAdmin{execErr: denied, read: ownerRow(userA, true)}
	err := r.ProvisionM1Caller(context.Background(), adm, userA)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" || errors.Is(err, ErrM1CallerConflict) {
		t.Fatalf("a refused insert: %v, want the driver's 42501 and not a conflict", err)
	}
	if len(adm.queries) != 0 {
		t.Errorf("a refused insert was followed by %d read(s): nothing was written, nothing may be judged", len(adm.queries))
	}
	for _, s := range secrets {
		if strings.Contains(err.Error(), s) {
			t.Errorf("a refused insert's error carries the secret, the key or its digest: %v", err)
		}
	}
}
