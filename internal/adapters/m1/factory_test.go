package m1

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// TestFromConfig_RefusesAMissingURLAndCarriesTheDials: an unset PLATFORM_M1_URL is an Invalid error that names the variable (never a Factory that fails later, never a silent
// no-op); a URL that is not plain http(s) is Invalid too; a configured one builds a Factory whose adapters use the config's dials (here two retries: an idempotent read after two
// 503s succeeds on the third attempt). Control: the same call with the dials at zero stops at the first 503.
func TestFromConfig_RefusesAMissingURLAndCarriesTheDials(t *testing.T) {
	f, err := FromConfig(config.Config{})
	if f != nil || modclient.KindOf(err) != modclient.Invalid || !strings.Contains(err.Error(), config.VarM1URL) {
		t.Fatalf("an unset URL: got %v %v, want an Invalid error that names %s and no factory", f, err, config.VarM1URL)
	}
	for _, u := range []string{"ftp://m1:8080", "http://user:pw-secret@m1:8080", "m1:8080"} {
		f, err := FromConfig(config.Config{M1URL: u})
		if f != nil || modclient.KindOf(err) != modclient.Invalid || leaks(err.Error(), "pw-secret") {
			t.Errorf("URL %q: got %v %v, want an Invalid error that quotes nothing", u, f, err)
		}
	}

	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(fixtureErr("unavailable")))
			return
		}
		_, _ = w.Write([]byte(completeBody(3)))
	}))
	defer srv.Close()
	scope := tenant.Scope{M1Key: config.Secret(newToken(t))}

	retrying, err := FromConfig(config.Config{M1URL: srv.URL, ModuleRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	a, err := retrying.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	if js, err := a.Status(testCtx(t), testJob); err != nil || !js.Complete || n.Load() != 3 {
		t.Fatalf("with ModuleRetries 2: %+v %v after %d request(s), want success on the third", js, err, n.Load())
	}
	n.Store(0)
	once, err := FromConfig(config.Config{M1URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	b, err := once.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Status(testCtx(t), testJob); modclient.KindOf(err) != modclient.Unavailable || n.Load() != 1 {
		t.Fatalf("control, retries 0: %v after %d request(s), want the first 503 as the error", err, n.Load())
	}
}

// TestForScope_EachTenantSendsItsOwnKeyAndAScopeWithoutOneRefuses: two adapters made for two scopes (the keys tenant.DeriveM1Key derives for two users from one secret) send two
// DIFFERENT bearer tokens on the same stand-in; the stand-in records only the SHA-256 of the Authorization header, and nothing prints a key. The same scope twice sends the same
// token (control: the digests could have been equal, so "different" says something). A scope with no key, and a nil factory, are Invalid refusals made before any network call
// (the stand-in's request count stays zero); the same factory then serves a scope with a key.
func TestForScope_EachTenantSendsItsOwnKeyAndAScopeWithoutOneRefuses(t *testing.T) {
	var secretBytes [32]byte
	if _, err := rand.Read(secretBytes[:]); err != nil {
		t.Fatal(err)
	}
	secret := config.Secret(hex.EncodeToString(secretBytes[:]))
	keyFor := func(userID string) config.Secret {
		k, err := tenant.DeriveM1Key(secret, userID)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	scopeA := tenant.Scope{M2UserID: "00000000-0000-4000-8000-000000000001", M1Key: keyFor("00000000-0000-4000-8000-000000000001")}
	scopeB := tenant.Scope{M2UserID: "00000000-0000-4000-8000-000000000002", M1Key: keyFor("00000000-0000-4000-8000-000000000002")}

	var mu sync.Mutex
	var digests []string
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		sum := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		mu.Lock()
		digests = append(digests, hex.EncodeToString(sum[:]))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(completeBody(3)))
	}))
	defer srv.Close()
	fac, err := NewFactory(srv.URL, modclient.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx(t)
	for _, s := range []tenant.Scope{scopeA, scopeB, scopeA} {
		a, err := fac.ForScope(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Status(ctx, testJob); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	got := append([]string(nil), digests...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("%d request(s), want 3", len(got))
	}
	if got[0] == got[1] {
		t.Fatal("two tenants sent the same bearer token: module 1 would see one tenant")
	}
	if got[0] != got[2] {
		t.Fatal("control: the same scope twice sent two different tokens, so a difference between tenants proves nothing")
	}
	want := sha256.Sum256([]byte("Bearer " + scopeA.M1Key.Reveal()))
	if got[0] != hex.EncodeToString(want[:]) {
		t.Fatal("the token sent for scope A is not its derived key")
	}

	before := requests.Load()
	a, refusal := fac.ForScope(tenant.Scope{M2UserID: scopeA.M2UserID})
	if e := asModclient(refusal); a != nil || e == nil || e.Kind != modclient.Invalid || e.Route != "m1.scope" || !strings.Contains(e.Message, "no module 1 key") {
		t.Fatalf("a scope with no key: got %v %v, want an Invalid error from the scope check (m1.scope) and no adapter", a, refusal)
	}
	var nilFactory *Factory
	if a, err := nilFactory.ForScope(scopeA); a != nil || modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a nil factory: got %v %v, want an Invalid error and no adapter", a, err)
	}
	if requests.Load() != before {
		t.Fatalf("a refused scope made %d request(s)", requests.Load()-before)
	}
	if leaks(refusal.Error(), scopeA.M1Key.Reveal(), scopeB.M1Key.Reveal()) {
		t.Fatalf("the refusal carries a key: %v", refusal)
	}
}
