package tenant

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/config"
)

// The key version (Phase 3 close-out A4, owner ruling OD-2): "m1-caller|v<N>|" + user id. These tests pin the version into the derivation and prove the rotation path in the unit layer; the
// harness (tests/harness/tenant_test.go, TestM1LiveTenant_RotationBothVersionsAuthenticate...) proves it against module 1's real front door.

// The version-2 vectors, computed outside this code by Node's crypto and by `openssl dgst -sha256 -hmac ... | base64`, which agreed (the same two tools as the version-1 vectors).
var vectorsV2 = []struct{ user, out, sum string }{
	{"11111111-1111-4111-8111-111111111111", "2-YhvQ2DZDJsGJvdP6HUqn3dpYhuW9_q7J_5sKWRgEc", "adaa05cae6c6028741c905a11ab7b2ab54ae2fe7f20bc46ab926a5b6a6c7c943"},
	{"22222222-2222-4222-8222-222222222222", "oNbpg9mtqcZhcntDqbFh855_fF0EaiM2hW3vK2Ganug", "f88e5d70e0e6f17477ad196c642d882af21981dcaa7703be6cb88a9b4dd3baba"},
}

// version 12 of the first user: the digits are decimal text, never a padded or a hexadecimal spelling
const vectorV12 = "Fcpox23ie5dALkiaymdsEXm5tlwu1BrMCJbqpOdCycs"

func TestDeriveM1KeyVersion_PinnedVectors(t *testing.T) {
	for _, v := range vectorsV2 {
		got, err := DeriveM1KeyVersion(config.Secret(vectorMaterial), 2, v.user)
		if err != nil {
			t.Fatalf("user %s: %v", v.user, err)
		}
		if got.Reveal() != v.out {
			t.Errorf("version 2, user %s: the derived text is %q, the pinned vector is %q: the derivation changed", v.user, got.Reveal(), v.out)
		}
		if sum := hex.EncodeToString(M1KeyDigest(got)); sum != v.sum {
			t.Errorf("version 2, user %s: the digest is %s, the pinned vector is %s", v.user, sum, v.sum)
		}
	}
	if got, err := DeriveM1KeyVersion(config.Secret(vectorMaterial), 12, vectors[0].user); err != nil || got.Reveal() != vectorV12 {
		t.Errorf("version 12: %q, %v; want the pinned vector", got.Reveal(), err)
	}
	// the independent recomputation inside the test, so a vector edit is checked against the standard library too
	mac := hmac.New(sha256.New, []byte(vectorMaterial))
	mac.Write([]byte("m1-caller|v2|" + vectorsV2[0].user))
	if got := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); got != vectorsV2[0].out {
		t.Fatalf("the pinned version-2 vector %q is not what the standard library computes (%q)", vectorsV2[0].out, got)
	}
}

// DeriveM1Key is version 1, and the version is in the key: the same secret and user give a different key at every version, and a key of one version is never the key of another.
func TestDeriveM1KeyVersion_VersionsOfOneUserAreDistinctAndTheDefaultIsVersionOne(t *testing.T) {
	secret := config.Secret(word("versions-", 50))
	v1, err := DeriveM1KeyVersion(secret, 1, userA)
	if err != nil {
		t.Fatal(err)
	}
	dflt, _ := DeriveM1Key(secret, userA)
	if v1 != dflt {
		t.Errorf("DeriveM1Key is not version 1")
	}
	seen := map[string]int{v1.Reveal(): 1}
	digests := map[string]int{string(M1KeyDigest(v1)): 1}
	for _, ver := range []int{2, 3, 10, 11, 100, 999} {
		k, err := DeriveM1KeyVersion(secret, ver, userA)
		if err != nil {
			t.Fatalf("version %d: %v", ver, err)
		}
		if other, dup := seen[k.Reveal()]; dup {
			t.Errorf("versions %d and %d of one user gave the same key", other, ver)
		}
		if other, dup := digests[string(M1KeyDigest(k))]; dup {
			t.Errorf("versions %d and %d of one user gave the same digest", other, ver)
		}
		seen[k.Reveal()], digests[string(M1KeyDigest(k))] = ver, ver
		again, _ := DeriveM1KeyVersion(secret, ver, userA)
		if again != k {
			t.Errorf("version %d: the same inputs gave two keys", ver)
		}
	}
}

func TestDeriveM1KeyVersion_RefusesAVersionOutsideItsRangeBeforeAnyWork(t *testing.T) {
	secret := config.Secret(word("range-", 40))
	for _, ver := range []int{-1, 0, config.MaxM1KeyVersion + 1, 1000, 1 << 30} {
		k, err := DeriveM1KeyVersion(secret, ver, userA)
		if !errors.Is(err, ErrBadKeyVersion) || k != "" {
			t.Errorf("version %d: %q, %v; want no key and ErrBadKeyVersion", ver, k.Reveal(), err)
		}
	}
	for _, ver := range []int{config.MinM1KeyVersion, config.MaxM1KeyVersion} {
		if _, err := DeriveM1KeyVersion(secret, ver, userA); err != nil {
			t.Errorf("the boundary version %d was refused: %v", ver, err)
		}
	}
}

// WithM1KeyVersion is the consumer of PLATFORM_M1_KEY_VERSION: the resolver presents (Scope) exactly the key the derivation gives at that version, a resolver without the option presents version 1, and
// a version outside the range is refused when the resolver is built.
func TestResolver_ScopePresentsTheConfiguredKeyVersion(t *testing.T) {
	secret := word("scope-version-", 50)
	for _, ver := range []int{1, 2, 7} {
		want, err := DeriveM1KeyVersion(config.Secret(secret), ver, userA)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewResolver(nil, config.Secret(secret), WithM1KeyVersion(ver))
		if err != nil {
			t.Fatalf("version %d: %v", ver, err)
		}
		got, err := r.deriveKey(userA)
		if err != nil || got != want {
			t.Errorf("version %d: the resolver derived %q (%v), want %q", ver, got.Reveal(), err, want.Reveal())
		}
		if !strings.Contains(r.String(), "m1_key_version: ") {
			t.Errorf("the resolver's description does not say its version: %s", r.String())
		}
	}
	dflt, err := NewResolver(nil, config.Secret(secret))
	if err != nil {
		t.Fatal(err)
	}
	k1, _ := DeriveM1KeyVersion(config.Secret(secret), 1, userA)
	if got, _ := dflt.deriveKey(userA); got != k1 {
		t.Errorf("a resolver without the option does not present version 1")
	}
	for _, bad := range []int{0, -3, config.MaxM1KeyVersion + 1} {
		if r, err := NewResolver(nil, config.Secret(secret), WithM1KeyVersion(bad)); !errors.Is(err, ErrBadKeyVersion) || r != nil {
			t.Errorf("a resolver with version %d: %v, %v; want ErrBadKeyVersion and no resolver", bad, r, err)
		}
	}
}

// ProvisionM1CallerVersion writes one row per version, with the SAME caller id (module 1's rotation design) and the digest of that version's key; the two digests differ.
func TestProvisionM1CallerVersion_WritesOneRowPerVersionUnderOneCallerID(t *testing.T) {
	secret := word("provision-version-", 50)
	r := resolverOver(t, nil, secret)
	digests := map[int][]byte{}
	for _, ver := range []int{1, 2} {
		adm := &fakeAdmin{read: ownerRow(userA, true)}
		if err := r.ProvisionM1CallerVersion(context.Background(), adm, userA, ver); err != nil {
			t.Fatalf("version %d: %v", ver, err)
		}
		if len(adm.execs) != 1 {
			t.Fatalf("version %d: %d insert(s), want 1", ver, len(adm.execs))
		}
		args := adm.execs[0].args
		if args[0] != M1CallerID(userA) || args[2] != userA {
			t.Errorf("version %d: caller id or owner is %v, want %q and the user", ver, args, M1CallerID(userA))
		}
		key, _ := DeriveM1KeyVersion(config.Secret(secret), ver, userA)
		d, ok := args[1].([]byte)
		if !ok || !bytes.Equal(d, M1KeyDigest(key)) {
			t.Errorf("version %d: the digest written is not the digest of the key of that version", ver)
		}
		digests[ver] = d
	}
	if bytes.Equal(digests[1], digests[2]) {
		t.Errorf("version 1 and version 2 of one user were written with the same digest: the rotation would overwrite nothing and protect nothing")
	}
	// ProvisionM1Caller writes the resolver's own version
	r2, _ := NewResolver(nil, config.Secret(secret), WithM1KeyVersion(2))
	adm := &fakeAdmin{read: ownerRow(userA, true)}
	if err := r2.ProvisionM1Caller(context.Background(), adm, userA); err != nil {
		t.Fatal(err)
	}
	if d, _ := adm.execs[0].args[1].([]byte); !bytes.Equal(d, digests[2]) {
		t.Errorf("ProvisionM1Caller of a version-2 resolver did not write the version-2 digest")
	}
	// a bad version is refused before any statement
	adm = &fakeAdmin{read: ownerRow(userA, true)}
	if err := r.ProvisionM1CallerVersion(context.Background(), adm, userA, 0); !errors.Is(err, ErrBadKeyVersion) || len(adm.execs)+len(adm.queries) != 0 {
		t.Errorf("version 0: %v, %d statement(s); want ErrBadKeyVersion and none", err, len(adm.execs)+len(adm.queries))
	}
}

func TestRetireM1CallerVersion_EndsOnlyTheNamedVersionOfTheNamedUser(t *testing.T) {
	secret := word("retire-", 50)
	r := resolverOver(t, nil, secret)
	updated := pgconn.NewCommandTag("UPDATE 1")

	// the control: one update, keyed by the digest of THAT version's key and by the owner, with the grace in seconds
	adm := &fakeAdmin{tag: updated, read: ownerRow(userA, true)}
	if err := r.RetireM1CallerVersion(context.Background(), adm, userA, 1, 90*time.Second); err != nil {
		t.Fatalf("control: %v", err)
	}
	if len(adm.execs) != 1 || len(adm.queries) != 1 {
		t.Fatalf("control: %d update(s) and %d read(s), want 1 and 1", len(adm.execs), len(adm.queries))
	}
	k1, _ := DeriveM1KeyVersion(config.Secret(secret), 1, userA)
	args := adm.execs[0].args
	if d, ok := args[0].([]byte); !ok || !bytes.Equal(d, M1KeyDigest(k1)) || args[1] != userA || args[2] != float64(90) {
		t.Errorf("control: the update's parameters are %v, want (digest of the version-1 key, the user, 90 seconds)", args)
	}
	for _, want := range []string{"UPDATE verify.api_callers", "WHERE key_sha256 = $1 AND user_id = $2::uuid", "LEAST(", "'rotating_out'"} {
		if !strings.Contains(adm.execs[0].sql, want) {
			t.Errorf("control: the update does not contain %q:\n%s", want, adm.execs[0].sql)
		}
	}
	if strings.Contains(strings.ToUpper(adm.execs[0].sql), "DELETE") {
		t.Errorf("a retirement must never delete")
	}
	// the version-2 key is not the one named: the digest differs from version 1's
	adm2 := &fakeAdmin{tag: updated, read: ownerRow(userA, false)}
	if err := r.RetireM1CallerVersion(context.Background(), adm2, userA, 2, 0); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(adm2.execs[0].args[0].([]byte), args[0].([]byte)) {
		t.Errorf("retiring version 2 and version 1 named the same row")
	}

	// no row of this user for that key: ErrM1CallerNotFound, nothing read back
	none := &fakeAdmin{read: ownerRow(userA, true)} // zero tag: no row affected
	if err := r.RetireM1CallerVersion(context.Background(), none, userA, 1, time.Minute); !errors.Is(err, ErrM1CallerNotFound) || len(none.queries) != 0 {
		t.Errorf("no row: %v, %d read(s); want ErrM1CallerNotFound and no read", err, len(none.queries))
	}
	// a zero grace must END the key: a row the read-back still finds valid is a conflict, never a success; one it finds ended is success; with a grace the key stays valid until then
	still := &fakeAdmin{tag: updated, read: ownerRow(userA, true)}
	if err := r.RetireM1CallerVersion(context.Background(), still, userA, 1, 0); !errors.Is(err, ErrM1CallerConflict) {
		t.Errorf("grace 0 but the row is still valid: %v, want ErrM1CallerConflict", err)
	}
	ended := &fakeAdmin{tag: updated, read: ownerRow(userA, false)}
	if err := r.RetireM1CallerVersion(context.Background(), ended, userA, 1, 0); err != nil {
		t.Errorf("grace 0 and the row reads as ended: %v, want success", err)
	}
	other := &fakeAdmin{tag: updated, read: ownerRow(userB, false)}
	if err := r.RetireM1CallerVersion(context.Background(), other, userA, 1, 0); !errors.Is(err, ErrM1CallerConflict) {
		t.Errorf("the read-back names another owner: %v, want ErrM1CallerConflict", err)
	}
	// refusals before any statement
	for _, c := range []struct {
		name string
		id   string
		ver  int
		g    time.Duration
		want error
	}{
		{"an empty id", "", 1, 0, ErrBadID}, {"upper case", strings.ToUpper(userA), 1, 0, ErrBadID}, {"the system user", SystemUserID, 1, 0, ErrSystemUser},
		{"version 0", userA, 0, 0, ErrBadKeyVersion}, {"a negative grace", userA, 1, -time.Second, ErrBadKeyVersion},
	} {
		a := &fakeAdmin{tag: updated, read: ownerRow(userA, true)}
		if err := r.RetireM1CallerVersion(context.Background(), a, c.id, c.ver, c.g); !errors.Is(err, c.want) || len(a.execs)+len(a.queries) != 0 {
			t.Errorf("%s: %v, %d statement(s); want %v and none", c.name, err, len(a.execs)+len(a.queries), c.want)
		}
	}
	if err := r.RetireM1CallerVersion(context.Background(), nil, userA, 1, 0); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("without a connection: %v", err)
	}
}
