package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The consumers of the three job dials in the service itself (Phase 4 B14b; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"): PLATFORM_JOB_MAX_ROWS and
// PLATFORM_JOB_MAX_START_DELAY_MIN reach the job store (newJobStore), PLATFORM_RESULTS_PAGE_MAX reaches the API (api.Deps.ResultsPageMax). internal/config's jobdials_test.go proves what each
// variable may say; what is proved HERE is that each value arrives, and, by running the whole service over a request, that the API acts on it. The same tests prove that a service with no key source
// does not mount the job routes, and that one with a key source does, behind authentication.

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app:x@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestJobStore_TheLimitDialsReachTheStore(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		name  string
		env   map[string]string
		rows  int
		delay time.Duration
	}{
		{"the defaults", nil, 100000, 30 * time.Minute},
		{"both set", map[string]string{config.VarJobMaxRows: "7", config.VarJobMaxStartDelayMin: "11"}, 7, 11 * time.Minute},
		{"the edges", map[string]string{config.VarJobMaxRows: "1", config.VarJobMaxStartDelayMin: "1440"}, 1, 24 * time.Hour},
	} {
		cfg, err := config.Load(envOf(merge(base("postgres://platform_app:x@h/db"), c.env)))
		if err != nil {
			t.Fatal(err)
		}
		st, err := newJobStore(pool, newMoneyStore(pool, cfg), cfg, log)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if st.MaxRows() != c.rows || st.MaxStartDelay() != c.delay {
			t.Errorf("%s: the store holds %d rows and waits %s, want %d and %s", c.name, st.MaxRows(), st.MaxStartDelay(), c.rows, c.delay)
		}
	}
}

func TestApiDeps_BuildMakesTheStoresOnceAndTheAPIOnlyWithAKeySource(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	cfg, err := config.Load(envOf(base("postgres://platform_app:x@h/db")))
	if err != nil {
		t.Fatal(err)
	}
	// no key source: the stores exist (the engine will need them), the API does not, and nothing is mounted
	none, err := newAPIDeps(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := none.build(pool, cfg, log); err != nil {
		t.Fatal(err)
	}
	if none.Money == nil || none.Jobs == nil || none.Tenants == nil || none.API != nil || none.mount() != nil {
		t.Fatalf("with no key source: money %v jobs %v tenants %v api %v mount %v", none.Money != nil, none.Jobs != nil, none.Tenants != nil, none.API != nil, none.mount() != nil)
	}
	// with one: the API is built and mounted
	withKey, err := config.Load(envOf(merge(base("postgres://platform_app:x@h/db"), map[string]string{config.VarJWTSecret: randomSecret(t)})))
	if err != nil {
		t.Fatal(err)
	}
	deps, err := newAPIDeps(withKey, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.build(pool, withKey, log); err != nil {
		t.Fatal(err)
	}
	if deps.API == nil || deps.mount() == nil || deps.Jobs == nil || deps.Money == nil {
		t.Fatalf("with a key source: api %v mount %v", deps.API != nil, deps.mount() != nil)
	}
	// ONE money store: what the reachability loop is given is the very store the job store was built over
	if got := newReachabilitySetter(pool, deps.Money, withKey); got != reachabilitySetter(deps.Money) {
		t.Fatal("the reachability loop would write through another money store than apiDeps.Money")
	}
	if deps.Money.LockTimeout() != withKey.MoneyLockTimeout {
		t.Fatalf("the one money store waits %s, want PLATFORM_MONEY_LOCK_TIMEOUT_MS's %s", deps.Money.LockTimeout(), withKey.MoneyLockTimeout)
	}
}

// fakeTenants answers the login it knows with a platform user, and no other.
type fakeTenants struct{ login, user string }

func (f fakeTenants) ByAuthUser(_ context.Context, login string) (tenant.Tenant, error) {
	if login == f.login {
		return tenant.Tenant{UserID: f.user, AuthUserID: login, Active: true}, nil
	}
	return tenant.Tenant{}, tenant.ErrUnknownLogin
}

var _ api.Resolver = fakeTenants{}

func withTenants(t *testing.T, f fakeTenants) {
	t.Helper()
	old := apiResolver
	apiResolver = func(*tenant.Resolver) api.Resolver { return f }
	t.Cleanup(func() { apiResolver = old })
}

const (
	svcLogin = "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e90"
	svcUser  = "a1a1a1a1-0000-4000-8000-000000000001"
	svcJob   = "0b000000-0000-4000-8000-000000000001"
)

// call sends a request to the running service and returns the status and the decoded error envelope (code and details), when the body has one.
func call(t *testing.T, r *running, method, path, token, ctype string, body string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewBufferString(body)
	}
	q, err := http.NewRequest(method, "http://"+r.addr+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		q.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		q.Header.Set("Content-Type", ctype)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(q)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	e, _ := m["error"].(map[string]any)
	return resp.StatusCode, e, resp.Header
}

func TestRun_WithoutAKeySourceTheJobRoutesAreNotMountedAndWithOneTheyAreBehindAuthentication(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	paths := []struct{ method, path string }{
		{"POST", "/v1/jobs"}, {"GET", "/v1/jobs"}, {"GET", "/v1/jobs/" + svcJob}, {"POST", "/v1/jobs/" + svcJob + "/confirm"}, {"GET", "/v1/jobs/" + svcJob + "/results.csv"},
	}

	// no key source: the 404 envelope of any unknown path, the start-up line says so, and nothing was mounted
	r := startApp(t, base(closedPortDSN(t)))
	for _, p := range paths {
		status, e, _ := call(t, r, p.method, p.path, "any-token-at-all", "", "")
		if status != 404 || e["code"] != "not_found" {
			t.Errorf("no key source, %s %s: %d %v, want the 404 envelope", p.method, p.path, status, e)
		}
	}
	if status, _, _ := call(t, r, "GET", "/healthz", "", "", ""); status != 200 {
		t.Errorf("/healthz = %d", status)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	lines := r.logs.lines(t)
	if find(lines, "job API authentication is not configured") == nil || find(lines, "job API mounted") != nil {
		t.Errorf("the start-up log does not say the job API is not served:\n%s", r.logs.String())
	}

	// a key source: the same requests are refused as unauthenticated, with the challenge, and the start-up line says what was mounted
	secret := randomSecret(t)
	r2 := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarJWTSecret: secret}))
	for _, p := range paths {
		status, e, h := call(t, r2, p.method, p.path, "", "", "")
		if status != 401 || e["code"] != "unauthenticated" || h.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("a key source, %s %s with no token: %d %v %q, want the 401 envelope", p.method, p.path, status, e, h.Get("WWW-Authenticate"))
		}
	}
	// the control: an unknown path is still the 404 beside the mounted routes
	if status, e, _ := call(t, r2, "GET", "/v1/nothing", "", "", ""); status != 404 || e["code"] != "not_found" {
		t.Errorf("an unknown path: %d %v", status, e)
	}
	if err := r2.stop(t); err != nil {
		t.Fatal(err)
	}
	l := find(r2.logs.lines(t), "job API mounted")
	if l == nil || l["routes"] != float64(13) || l["max_rows"] != float64(100000) || l["results_page_max"] != float64(1000) || l["max_start_delay"] != "30m0s" {
		t.Errorf("the job API's start-up line is %v:\n%s", l, r2.logs.String())
	}
	if strings.Contains(r2.logs.String(), secret) {
		t.Error("the secret is in the log")
	}
}

// The service acts on the dials: with a login the (fake) resolver knows, a request reaches the API's own checks, and those use the numbers the environment gave.
func TestRun_TheRowAndPageDialsAreActedOnByTheRunningAPI(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withTenants(t, fakeTenants{login: svcLogin, user: svcUser})
	m := authtest.NewHS256(t)
	token := m.Token(svcLogin)
	env := func(extra map[string]string) map[string]string {
		return merge(base(closedPortDSN(t)), merge(map[string]string{config.VarJWTSecret: m.Secret(), config.VarJWTIssuer: authtest.Issuer}, extra))
	}
	rows := func(n int) string {
		var rs []string
		for i := 0; i < n; i++ {
			rs = append(rs, `{"email":"u`+string(rune('a'+i))+`@example.test"}`)
		}
		return `{"module":"m1","rows":[` + strings.Join(rs, ",") + `]}`
	}
	csv := func(n int) string {
		s := "email\n"
		for i := 0; i < n; i++ {
			s += "u" + string(rune('a'+i)) + "@example.test\n"
		}
		return s
	}

	// the dials set: a list over PLATFORM_JOB_MAX_ROWS is refused with the limit, by JSON and by CSV, before the database is asked; a page over PLATFORM_RESULTS_PAGE_MAX is refused
	r := startApp(t, env(map[string]string{config.VarJobMaxRows: "2", config.VarResultsPageMax: "7"}))
	if status, e, _ := call(t, r, "POST", "/v1/jobs", token, "application/json", rows(3)); status != 413 || e["code"] != "too_many_rows" || e["details"].(map[string]any)["limit"] != float64(2) {
		t.Errorf("3 rows by JSON with the dial at 2: %d %v", status, e)
	}
	if status, e, _ := call(t, r, "POST", "/v1/jobs?module=m1", token, "text/csv", csv(3)); status != 413 || e["code"] != "too_many_rows" || e["details"].(map[string]any)["limit"] != float64(2) {
		t.Errorf("3 rows by CSV with the dial at 2: %d %v", status, e)
	}
	if status, e, _ := call(t, r, "GET", "/v1/jobs/"+svcJob+"/results?limit=8", token, "", ""); status != 400 || e["code"] != "validation_failed" {
		t.Errorf("a page of 8 with the dial at 7: %d %v", status, e)
	}
	// the controls: at the limits the request goes on to the store, whose database is not there: the answer is the server's own 500, never the refusal above
	if status, e, _ := call(t, r, "POST", "/v1/jobs", token, "application/json", rows(2)); status != 500 || e["code"] != "internal_error" {
		t.Errorf("2 rows with the dial at 2: %d %v, want the request to reach the (absent) database", status, e)
	}
	if status, e, _ := call(t, r, "GET", "/v1/jobs/"+svcJob+"/results?limit=7", token, "", ""); status != 500 || e["code"] != "internal_error" {
		t.Errorf("a page of 7 with the dial at 7: %d %v, want the request to reach the (absent) database", status, e)
	}
	// a login the resolver does not know is refused whatever the dials say
	if status, e, _ := call(t, r, "GET", "/v1/jobs", m.Token("7d4c2a90-3e51-4b8f-a6d2-91c0e5f3b7a4"), "", ""); status != 401 || e["code"] != "unauthenticated" {
		t.Errorf("an unknown login: %d %v", status, e)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}

	// the dials unset: the same requests pass the limits they met above
	r2 := startApp(t, env(nil))
	if status, e, _ := call(t, r2, "POST", "/v1/jobs", token, "application/json", rows(3)); status != 500 || e["code"] != "internal_error" {
		t.Errorf("3 rows with the dial at its default: %d %v, want the request to reach the (absent) database", status, e)
	}
	if status, e, _ := call(t, r2, "GET", "/v1/jobs/"+svcJob+"/results?limit=8", token, "", ""); status != 500 || e["code"] != "internal_error" {
		t.Errorf("a page of 8 with the dial at its default: %d %v, want the request to reach the (absent) database", status, e)
	}
	if err := r2.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestRun_AnInvalidJobDialStopsTheProcessWithExitStatus2(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"a job over 100000 rows":        {config.VarJobMaxRows: "100001"},
		"a start delay of 0":            {config.VarJobMaxStartDelayMin: "0"},
		"a results page over 1000":      {config.VarResultsPageMax: "1001"},
		"a results page that is a word": {config.VarResultsPageMax: "plenty"},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		err := Run(ctx, envOf(merge(base(closedPortDSN(t)), env)), &syncBuffer{})
		cancel()
		var ce *config.Error
		if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
			t.Errorf("%s: Run = %v (exit %d), want a configuration error, exit status 2", name, err, ExitCode(err))
		}
	}
	// the control: the edges start the service
	withSuspender(t, &fakeSuspender{}, nil)
	r := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarJobMaxRows: "100000", config.VarJobMaxStartDelayMin: "1440", config.VarResultsPageMax: "1000"}))
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}
