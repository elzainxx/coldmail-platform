package app

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
)

// Phase 6 A3: the service gives the job API its account service (GET /v1/me, GET /v1/balances), so the three routes of the chunk are mounted wherever the API is, and are not where it is not. What the routes
// answer is proved by internal/api's tests (fakes) and tests/harness/jobs_api_account_test.go (the real stores, through app.NewAccountService).

func mountedPatterns(t *testing.T, mount func(*http.ServeMux), want ...string) map[string]string {
	t.Helper()
	mux := http.NewServeMux()
	if mount != nil {
		mount(mux)
	}
	out := map[string]string{}
	for _, w := range want {
		method, path, _ := splitRoute(w)
		_, pattern := mux.Handler(httptest.NewRequest(method, path, nil))
		out[w] = pattern
	}
	return out
}

func splitRoute(s string) (method, path string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i], s[i+1:], true
		}
	}
	return "", s, false
}

func TestApiDeps_TheAccountRoutesAndTheTemplateCheckAreMountedWithTheAPIAndOnlyThen(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	routes := []string{"GET /v1/me", "GET /v1/balances", "POST /v1/templates/check", "GET /v1/jobs"}

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
	for route, pattern := range mountedPatterns(t, deps.mount(), routes...) {
		if pattern != route {
			t.Errorf("%s is served by the pattern %q: it is not mounted with the API", route, pattern)
		}
	}

	// the control: with no key source the API is not built and the same routes are the catch-all's (no pattern of their own)
	none, err := newAPIDeps(mustLoad(t, base("postgres://platform_app:x@h/db")), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := none.build(pool, mustLoad(t, base("postgres://platform_app:x@h/db")), log); err != nil {
		t.Fatal(err)
	}
	for route, pattern := range mountedPatterns(t, none.mount(), routes...) {
		if pattern == route {
			t.Errorf("%s is mounted with no key source", route)
		}
	}
}

func mustLoad(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
