package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/logging"
)

// The wiring of the unsubscribe link (Phase 5 S3): the three dials become the job store's link maker and the public endpoint, and with either of the first two unset there is neither. What the maker
// writes, what the endpoint does and what the store refuses are proved where they live (internal/unsubscribe, its endpoint package, internal/jobs and the harness); these tests prove the WIRING, with
// a pool that connects to nothing (no request below reaches the database).

type okPing struct{}

func (okPing) Ping(context.Context) error { return nil }

func unsubEnv(extra map[string]string) map[string]string {
	return merge(base("postgres://platform_app:x@h/db"), extra)
}

func TestUnsubscribeSigner_SaysOnceWhetherTheLinkIsConfiguredAndNeverTheSecret(t *testing.T) {
	secret := randomSecret(t)
	for _, c := range []struct {
		name       string
		env        map[string]string
		configured bool
		level      string
		msg        string
		missing    string // the dial the line must name as unset
	}{
		{"neither dial", nil, false, "INFO", "unsubscribe link is not configured", ""},
		{"only the secret", map[string]string{config.VarUnsubscribeSecret: secret}, false, "WARN", "unsubscribe link is only half configured", config.VarPublicBaseURL},
		{"only the public address", map[string]string{config.VarPublicBaseURL: "https://app.example.test"}, false, "WARN", "unsubscribe link is only half configured", config.VarUnsubscribeSecret},
		{"both", map[string]string{config.VarUnsubscribeSecret: secret, config.VarPublicBaseURL: "https://app.example.test"}, true, "INFO", "unsubscribe link configured", ""},
	} {
		cfg, err := config.Load(envOf(unsubEnv(c.env)))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var buf bytes.Buffer
		log, _ := logging.New("debug", &buf)
		signer, err := newUnsubscribeSigner(cfg, log)
		if err != nil || (signer != nil) != c.configured {
			t.Fatalf("%s: signer %v, err %v, want configured=%v", c.name, signer != nil, err, c.configured)
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) != 1 {
			t.Fatalf("%s: %d log lines, want ONE:\n%s", c.name, len(lines), buf.String())
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
			t.Fatal(err)
		}
		if line["msg"] != c.msg || line["level"] != c.level || line["enabled"] != c.configured {
			t.Errorf("%s: the line is %v, want %q at %s with configured=%v", c.name, line, c.msg, c.level, c.configured)
		}
		if strings.Contains(buf.String(), secret) || strings.Contains(buf.String(), secret[:16]) {
			t.Errorf("%s: the log line holds the secret", c.name)
		}
		if c.missing != "" && !strings.Contains(buf.String(), c.missing) {
			t.Errorf("%s: the line does not name %s", c.name, c.missing)
		}
		if c.configured && (line["public_base_url"] != "https://app.example.test" || line["rate_per_min"] != float64(30)) {
			t.Errorf("%s: the configured line is %v", c.name, line)
		}
	}
}

func TestApiDeps_TheLinkMakerAndTheEndpointExistOnlyWhenBothDialsAreSet(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	both := map[string]string{config.VarUnsubscribeSecret: randomSecret(t), config.VarPublicBaseURL: "https://app.example.test"}
	for _, c := range []struct {
		name string
		env  map[string]string
		on   bool
	}{
		{"neither", nil, false},
		{"only the secret", map[string]string{config.VarUnsubscribeSecret: both[config.VarUnsubscribeSecret]}, false},
		{"only the address", map[string]string{config.VarPublicBaseURL: both[config.VarPublicBaseURL]}, false},
		{"both", both, true},
		{"both and a key source for the API", merge(both, map[string]string{config.VarJWTSecret: randomSecret(t)}), true},
	} {
		cfg, err := config.Load(envOf(unsubEnv(c.env)))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		deps, err := newAPIDeps(cfg, log)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := deps.build(pool, cfg, log); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := deps.Jobs.UnsubscribeConfigured(); got != c.on {
			t.Errorf("%s: the job store has a link maker = %v, want %v", c.name, got, c.on)
		}
		if got := deps.UnsubEndpoint != nil; got != c.on || (deps.Unsub != nil) != c.on {
			t.Errorf("%s: endpoint %v signer %v, want both %v", c.name, got, deps.Unsub != nil, c.on)
		}
		// the public routes are mounted with no key source for the API at all, and without them nothing is mounted when there is no API either
		hasAPI := c.env[config.VarJWTSecret] != ""
		if mounted := deps.mount() != nil; mounted != (c.on || hasAPI) {
			t.Errorf("%s: mount() != nil is %v, want %v", c.name, mounted, c.on || hasAPI)
		}
	}
}

func TestMount_ThePublicRoutesNeedNoBearerAndTheJobAPIStillDoes(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	env := map[string]string{config.VarUnsubscribeSecret: randomSecret(t), config.VarPublicBaseURL: "https://app.example.test", config.VarJWTSecret: randomSecret(t)}
	cfg, err := config.Load(envOf(unsubEnv(env)))
	if err != nil {
		t.Fatal(err)
	}
	deps, err := newAPIDeps(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.build(pool, cfg, log); err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(httpapi.Deps{Pinger: okPing{}, ReadyTimeout: time.Second, Mount: deps.mount()})
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "192.0.2.50:4000"
		handler.ServeHTTP(rec, r)
		return rec
	}
	// a wrong token, with no Authorization header at all: the endpoint's own 404 page (it never reaches the database), not the API's 401
	rec := do("GET", "/u/"+strings.Repeat("A", 54))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "This link is not valid") || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("the public route answered %d %q", rec.Code, rec.Body.String())
	}
	if rec := do("PUT", "/u/x"); rec.Code != 405 || rec.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("a PUT answered %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	// the control: the job API beside it is still behind its authentication
	if rec := do("GET", "/v1/jobs"); rec.Code != 401 {
		t.Fatalf("GET /v1/jobs with no token answered %d, want 401", rec.Code)
	}
	// and with the dials unset the same path is the catch-all's JSON 404, not the page
	cfg2, err := config.Load(envOf(unsubEnv(map[string]string{config.VarJWTSecret: env[config.VarJWTSecret]})))
	if err != nil {
		t.Fatal(err)
	}
	deps2, err := newAPIDeps(cfg2, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps2.build(pool, cfg2, log); err != nil {
		t.Fatal(err)
	}
	h2 := httpapi.NewHandler(httpapi.Deps{Pinger: okPing{}, ReadyTimeout: time.Second, Mount: deps2.mount()})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("GET", "/u/"+strings.Repeat("A", 54), nil))
	if rec2.Code != 404 || !strings.Contains(rec2.Body.String(), `"not_found"`) || strings.Contains(rec2.Body.String(), "This link is not valid") {
		t.Fatalf("with the dials unset /u/ answered %d %q, want the 404 envelope", rec2.Code, rec2.Body.String())
	}
}

func TestMount_TheRateDialReachesTheEndpoint(t *testing.T) {
	pool := lazyPool(t)
	log := slog.New(slog.DiscardHandler)
	env := map[string]string{config.VarUnsubscribeSecret: randomSecret(t), config.VarPublicBaseURL: "https://app.example.test", config.VarUnsubscribeRatePerMin: "2"}
	cfg, err := config.Load(envOf(unsubEnv(env)))
	if err != nil {
		t.Fatal(err)
	}
	deps, err := newAPIDeps(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.build(pool, cfg, log); err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(httpapi.Deps{Pinger: okPing{}, ReadyTimeout: time.Second, Mount: deps.mount()})
	var got []int
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/u/"+strings.Repeat("A", 54), nil)
		r.RemoteAddr = "192.0.2.60:4000"
		handler.ServeHTTP(rec, r)
		got = append(got, rec.Code)
	}
	if want := []int{404, 404, http.StatusTooManyRequests, http.StatusTooManyRequests}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("with PLATFORM_UNSUBSCRIBE_RATE_PER_MIN=2 four requests answered %v, want %v", got, want)
	}
}
