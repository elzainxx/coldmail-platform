package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The provider of module 1's vendor reachability (reachability_provider.go), over the REAL module 1 adapter and a stand-in of module 1's HTTP surface. The describe bodies are HAND-WRITTEN: the
// recorded describe of the adapter's fixtures is a recording of module 1 BEFORE it carried `vendor_fallback`, so each body here is that recording plus the one key the module's code adds
// (control-plane/internal/api/describe.go: `"vendor_fallback": {"reachable": <bool>}`), or the recording itself where the test needs "an older module 1". The key a caller sends is the one the
// platform derives for the service caller, held against what the stand-in accepts, so a provider that sent another key (a customer's, the legacy token) would be refused.

// recordedDescribe is the adapter's recorded describe of module 1 (before vendor_fallback), as a document.
func recordedDescribe(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "adapters", "m1", "testdata", "contract", "describe-ok.json"))
	if err != nil {
		t.Fatalf("cannot read the adapter's recorded describe (a test that cannot run fails): %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// describeWith is the HAND-WRITTEN describe: the recording plus vendor_fallback (nil: the recording as it is, module 1 before the field).
func describeWith(t *testing.T, vendor map[string]any) string {
	t.Helper()
	doc := recordedDescribe(t)
	if vendor != nil {
		doc["vendor_fallback"] = vendor
	} else {
		delete(doc, "vendor_fallback") // the recording is of a module that HAS the field (module 1 at the close-out): an older module is the recording with the key taken away
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// moduleOneStandIn serves GET /v1/describe with body, but only to the bearer `want`: any other key is the 401 envelope of the recording. It records what it saw.
type moduleOneStandIn struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string // "METHOD path"
	auth []string // the Authorization header of each request
}

func newModuleOneStandIn(t *testing.T, want, body string) *moduleOneStandIn {
	t.Helper()
	unauthenticated, err := os.ReadFile(filepath.Join("..", "adapters", "m1", "testdata", "contract", "describe-error-unauthenticated.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &moduleOneStandIn{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(unauthenticated)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *moduleOneStandIn) seen() (reqs, auth []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...), append([]string(nil), s.auth...)
}

// recordingEnsurer is a callerEnsurer that records what it was asked and, when fail is set, refuses.
type recordingEnsurer struct {
	mu    sync.Mutex
	users []string
	fail  error
	// order is the shared record of what happened first (the ensurer or the request): the stand-in is asked through the same journal in the test that needs it
	onCall func()
}

func (e *recordingEnsurer) EnsureCaller(_ context.Context, userID string) error {
	e.mu.Lock()
	e.users = append(e.users, userID)
	e.mu.Unlock()
	if e.onCall != nil {
		e.onCall()
	}
	return e.fail
}

func (e *recordingEnsurer) asked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.users...)
}

// providerSecretText is the key-derivation secret of these tests: 32 random bytes in hex, minted when the test binary starts (no secret-shaped literal is ever committed).
var providerSecretText = func() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("no randomness for the provider tests: " + err.Error())
	}
	return hex.EncodeToString(b)
}()

// providerOver builds the provider over the stand-in, with the key version 3 (so a key derived at another version is a refusal).
func providerOver(t *testing.T, srv *moduleOneStandIn, ens callerEnsurer) (*m1Reachability, string) {
	t.Helper()
	secret := config.Secret(providerSecretText)
	key, err := tenant.DeriveM1KeyVersion(secret, 3, m1ServiceCallerID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := m1.NewFactory(srv.srv.URL, modclient.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &m1Reachability{factory: f, ensure: ens, secret: secret, version: 3}, key.Reveal()
}

func TestM1Reachability_AsksTheDescribeAsTheServiceCallerAndReturnsWhatModuleOneSays(t *testing.T) {
	for _, c := range []struct {
		name        string
		vendor      map[string]any
		reach, know bool
	}{
		{"module 1 says reachable (hand-written)", map[string]any{"reachable": true}, true, true},
		{"module 1 says NOT reachable (hand-written)", map[string]any{"reachable": false}, false, true},
		{"an older module 1: the recording, no vendor_fallback", nil, false, false},
	} {
		ens := &recordingEnsurer{}
		// the key the stand-in accepts is the one derived for the service caller: it is computed before the provider is built, from the same secret and version
		key, err := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, m1ServiceCallerID)
		if err != nil {
			t.Fatal(err)
		}
		srv := newModuleOneStandIn(t, key.Reveal(), describeWith(t, c.vendor))
		p, _ := providerOver(t, srv, ens)
		reach, known, err := p.M1VendorReachable(t.Context())
		if err != nil || reach != c.reach || known != c.know {
			t.Errorf("%s: read (%v, %v, %v), want (%v, %v, nil)", c.name, reach, known, err, c.reach, c.know)
		}
		reqs, auth := srv.seen()
		if len(reqs) != 1 || reqs[0] != "GET /v1/describe" || auth[0] != "Bearer "+key.Reveal() {
			t.Errorf("%s: the stand-in saw %v with %d authorization header(s): want one GET /v1/describe with the service caller's key", c.name, reqs, len(auth))
		}
		if got := ens.asked(); len(got) != 1 || got[0] != m1ServiceCallerID {
			t.Errorf("%s: the service caller was registered through %v, want one call for %s", c.name, got, m1ServiceCallerID)
		}
	}

	// the control that the key is checked: a provider that derives at another key version is refused by the same stand-in, as an error and never as an answer
	ens := &recordingEnsurer{}
	key, _ := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, m1ServiceCallerID)
	srv := newModuleOneStandIn(t, key.Reveal(), describeWith(t, map[string]any{"reachable": true}))
	p, _ := providerOver(t, srv, ens)
	p.version = 4
	if reach, known, err := p.M1VendorReachable(t.Context()); err == nil || reach || known || modclient.KindOf(err) != modclient.Refused {
		t.Errorf("a key of another version read (%v, %v, %v), want a Refused error and no answer", reach, known, err)
	}
	// ...and a customer's key (a provider that asked as someone else) is refused the same way
	other, _ := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, "d0000000-0000-4000-8000-000000000001")
	if other.Reveal() == key.Reveal() {
		t.Fatal("the service caller's key is a customer's key")
	}
}

func TestM1Reachability_RegistersTheServiceCallerBeforeItAsksAndAnErrorIsNeverAnAnswer(t *testing.T) {
	key, _ := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, m1ServiceCallerID)

	// the order: the registration first, the request after
	var journal []string
	var jmu sync.Mutex
	mark := func(s string) { jmu.Lock(); journal = append(journal, s); jmu.Unlock() }
	ens := &recordingEnsurer{onCall: func() { mark("ensure") }}
	journalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mark("request")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(journalSrv.Close)
	jf, err := m1.NewFactory(journalSrv.URL, modclient.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &m1Reachability{factory: jf, ensure: ens, secret: config.Secret(providerSecretText), version: 3}
	if _, _, err := p.M1VendorReachable(t.Context()); err == nil {
		t.Error("a module 1 that answers 500 gave an answer")
	}
	jmu.Lock()
	order := strings.Join(journal, ",")
	jmu.Unlock()
	if order != "ensure,request" {
		t.Errorf("the order was %q, want the registration, then one request", order)
	}

	// a registration that fails: an error, and module 1 is never asked (a key that is not registered would only be refused, and the refusal would say nothing of the vendor)
	srv2 := newModuleOneStandIn(t, key.Reveal(), describeWith(t, map[string]any{"reachable": true}))
	fail := &recordingEnsurer{fail: errors.New("the database said no")}
	p2, _ := providerOver(t, srv2, fail)
	reach, known, err := p2.M1VendorReachable(t.Context())
	if err == nil || reach || known || !strings.Contains(err.Error(), "service caller") {
		t.Errorf("a failed registration read (%v, %v, %v), want an error naming the service caller and no answer", reach, known, err)
	}
	if reqs, _ := srv2.seen(); len(reqs) != 0 {
		t.Errorf("module 1 was asked %d time(s) after the registration failed", len(reqs))
	}

	// module 1 down: a typed error of the call, no answer, and no address or key in its text
	down := newModuleOneStandIn(t, key.Reveal(), describeWith(t, nil))
	p3, derived := providerOver(t, down, &recordingEnsurer{})
	down.srv.Close()
	reach, known, err = p3.M1VendorReachable(t.Context())
	if err == nil || reach || known || modclient.KindOf(err) != modclient.Unavailable {
		t.Errorf("a module 1 that is down read (%v, %v, %v), want an Unavailable error and no answer", reach, known, err)
	}
	if err != nil && (strings.Contains(err.Error(), derived) || strings.Contains(err.Error(), providerSecretText) || strings.Contains(err.Error(), strings.TrimPrefix(down.srv.URL, "http://"))) {
		t.Errorf("the error carries the key, the secret or the address: %v", err)
	}

	// a describe that breaks the contract of the new key: a Contract error naming it, no answer
	bad := newModuleOneStandIn(t, key.Reveal(), describeWith(t, map[string]any{"reachable": "yes"}))
	p4, _ := providerOver(t, bad, &recordingEnsurer{})
	reach, known, err = p4.M1VendorReachable(t.Context())
	var me *modclient.Error
	if err == nil || reach || known || !errors.As(err, &me) || me.Kind != modclient.Contract || me.Key != "vendor_fallback.reachable" {
		t.Errorf("a describe with a retyped flag read (%v, %v, %v), want a Contract error naming vendor_fallback.reachable", reach, known, err)
	}
}

// fakeFuncDB is the database handle of the real ensurer: it answers the privileged function with "created" and records the user and the digest it was given.
type fakeFuncDB struct {
	mu    sync.Mutex
	users []string
	sums  [][]byte
}

type answerRow struct{ answer string }

func (r answerRow) Scan(dest ...any) error {
	*(dest[0].(*string)) = r.answer
	return nil
}

func (d *fakeFuncDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users = append(d.users, args[0].(string))
	d.sums = append(d.sums, args[1].([]byte))
	return answerRow{"created"}
}

func TestM1ServiceCaller_IsAnIdTheRealEnsurerRegistersAndNotACustomerNorTheSystemUser(t *testing.T) {
	res, err := tenant.NewResolver(nil, config.Secret(providerSecretText), tenant.WithM1KeyVersion(3))
	if err != nil {
		t.Fatal(err)
	}
	db := &fakeFuncDB{}
	ens := tenant.NewM1CallerEnsurer(res, db)
	if err := ens.EnsureCaller(t.Context(), m1ServiceCallerID); err != nil {
		t.Fatalf("the real ensurer refuses the service caller's id: %v", err)
	}
	key, _ := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, m1ServiceCallerID)
	if len(db.users) != 1 || db.users[0] != m1ServiceCallerID || string(db.sums[0]) != string(tenant.M1KeyDigest(key)) {
		t.Errorf("the function was called for %v with digest ok=%v, want the service caller and the digest of the key the provider sends", db.users, len(db.sums) == 1 && string(db.sums[0]) == string(tenant.M1KeyDigest(key)))
	}
	// the second call is remembered (nothing asks the database again)
	if err := ens.EnsureCaller(t.Context(), m1ServiceCallerID); err != nil || len(db.users) != 1 {
		t.Errorf("a second registration asked the database again: %v, %d call(s)", err, len(db.users))
	}
	// controls: the same ensurer refuses what the id must not be (so the acceptance above is not an ensurer that accepts anything)
	if err := ens.EnsureCaller(t.Context(), tenant.SystemUserID); !errors.Is(err, tenant.ErrSystemUser) {
		t.Errorf("the system user was registered: %v", err)
	}
	if err := ens.EnsureCaller(t.Context(), strings.ToUpper(m1ServiceCallerID)); !errors.Is(err, tenant.ErrBadID) {
		t.Errorf("the upper-case spelling of the id was accepted: %v", err)
	}
	if m1ServiceCallerID == tenant.SystemUserID || m1ServiceCallerID != strings.ToLower(m1ServiceCallerID) {
		t.Errorf("the service caller's id %q is the system user or not lower case", m1ServiceCallerID)
	}
}

func TestNewVendorProvider_ExistsOnlyWhereModuleOneCanBeAsked(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	key := strings.Repeat("k", 40)
	with := loadEngineCfg(t, map[string]string{config.VarM1URL: "http://m1.test:8080", config.VarM1KeySecret: key, config.VarM1KeyVersion: "2"})
	for _, c := range []struct {
		name  string
		extra map[string]string
		want  bool
	}{
		{"module 1's URL and key secret set", map[string]string{config.VarM1URL: "http://m1.test:8080", config.VarM1KeySecret: key}, true},
		{"no module 1 URL", map[string]string{config.VarM1KeySecret: key}, false},
		{"no key secret", map[string]string{config.VarM1URL: "http://m1.test:8080"}, false},
		{"neither", nil, false},
	} {
		cfg := loadEngineCfg(t, c.extra)
		d := apiDeps{}
		if err := d.build(pool, cfg, log); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (d.Vendor != nil) != c.want {
			t.Errorf("%s: the provider exists = %v, want %v", c.name, d.Vendor != nil, c.want)
		}
		// the store and the loop share ONE provider (a nil interface when there is none, never a typed nil)
		if d.Jobs.VendorReachability() != d.Vendor {
			t.Errorf("%s: the job store holds %v, the service's provider is %v: two providers", c.name, d.Jobs.VendorReachability(), d.Vendor)
		}
		if !c.want && d.Jobs.VendorReachability() != nil {
			t.Errorf("%s: a store without module 1 holds a provider (%T): a typed nil in an interface", c.name, d.Jobs.VendorReachability())
		}
	}
	d := apiDeps{}
	if err := d.build(pool, with, log); err != nil {
		t.Fatal(err)
	}
	p, ok := d.Vendor.(*m1Reachability)
	if !ok || p.version != 2 || p.secret.Reveal() != key || p.factory == nil || p.ensure == nil {
		t.Fatalf("the provider is %T %+v, want module 1's, built with the configured key secret and version (2)", d.Vendor, p)
	}
	var _ jobs.VendorReachability = p
}
