package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// The wiring of the web app (Phase 6 A2): PLATFORM_WEB_DIR switches the serving on in the real service, over a real listener, and with it off the service is what it was. internal/webapp's own tests
// prove what is served and with which headers; these prove that the SERVICE serves it, that the platform's own paths keep their answers byte for byte, and that a directory that is not the app stops
// the process with a configuration error that names the dial.

const (
	testSupabase = "https://abcdefgh.supabase.example"
	testAnonKey  = "anon-anon-anon-anon"
	testIndex    = "<!doctype html><title>ColdmailApp</title><div id=root></div>"
)

// builtApp is a directory shaped like a Vite build.
func builtApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": testIndex, filepath.Join("assets", "app-abc123.js"): "console.log(1)"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func webEnv(t *testing.T, dir string) map[string]string {
	return merge(base(closedPortDSN(t)), map[string]string{
		config.VarWebDir:                dir,
		config.VarPublicSupabaseURL:     testSupabase,
		config.VarPublicSupabaseAnonKey: testAnonKey,
	})
}

type reply struct {
	code   int
	header http.Header
	body   string
}

func get(t *testing.T, url string) reply {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, string(b)}
}

var requestIDInBody = regexp.MustCompile(`req_[0-9a-f]{32}`)

// atInBody is the clock of the probes' answers, which is the one other thing that differs between two answers of the same route.
var atInBody = regexp.MustCompile(`"at":"[^"]*"`)

func sameAnswer(s string) string {
	return atInBody.ReplaceAllString(requestIDInBody.ReplaceAllString(s, "X"), `"at":"T"`)
}

func TestRun_TheServiceServesTheBuiltAppAndEveryPlatformPathKeepsItsAnswer(t *testing.T) {
	dir := builtApp(t)
	on := startApp(t, webEnv(t, dir))
	off := startApp(t, base(closedPortDSN(t)))

	// the app: the root and a route of the app are index.html, the build's own file is itself, /config.json is the two public dials, all under the policy
	for _, p := range []string{"/", "/jobs", "/jobs/7/results", "/new-send"} {
		r := get(t, "http://"+on.addr+p)
		if r.code != http.StatusOK || r.body != testIndex || !strings.Contains(r.header.Get("Content-Security-Policy"), "connect-src 'self' "+testSupabase+";") || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s = %d %q (CSP %q, Cache-Control %q), want the index under the policy", p, r.code, r.body, r.header.Get("Content-Security-Policy"), r.header.Get("Cache-Control"))
		}
	}
	if r := get(t, "http://"+on.addr+"/assets/app-abc123.js"); r.code != http.StatusOK || r.body != "console.log(1)" || r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("an asset = %d %q (Cache-Control %q)", r.code, r.body, r.header.Get("Cache-Control"))
	}
	if r := get(t, "http://"+on.addr+"/config.json"); r.code != http.StatusOK || r.body != `{"supabase_url":"`+testSupabase+`","anon_key":"`+testAnonKey+`"}` {
		t.Errorf("/config.json = %d %q", r.code, r.body)
	}
	// a file that is not there is the platform's own 404, with the policy on it and never the index
	if r := get(t, "http://"+on.addr+"/assets/missing.js"); r.code != http.StatusNotFound || !strings.Contains(r.body, `"code":"not_found"`) || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("a missing asset = %d %q", r.code, r.body)
	}

	// the platform's paths: the answer with the app on is the answer with it off, byte for byte but for the request id, and none of them carries the app's policy
	for _, p := range []string{"/healthz", "/readyz", "/v1/jobs", "/v1/jobs/0b000000-0000-4000-8000-000000000001", "/v1/nothing-here", "/v1/", "/v2/jobs", "/u/abc", "/u/", "/healthz/x"} {
		a, b := get(t, "http://"+off.addr+p), get(t, "http://"+on.addr+p)
		if a.code != b.code || sameAnswer(a.body) != sameAnswer(b.body) || a.header.Get("Content-Type") != b.header.Get("Content-Type") {
			t.Errorf("GET %s differs with the app on: off %d %q, on %d %q", p, a.code, a.body, b.code, b.body)
		}
		if b.body == testIndex || b.header.Get("Content-Security-Policy") != "" {
			t.Errorf("GET %s was answered by the app", p)
		}
	}
	if r := get(t, "http://"+on.addr+"/v1/nothing-here"); r.code != http.StatusNotFound || !strings.Contains(r.body, `"code":"not_found"`) {
		t.Errorf("an unknown /v1/ path with the app on = %d %q, want the JSON 404", r.code, r.body)
	}
	if r := get(t, "http://"+on.addr+"/healthz"); r.code != http.StatusOK {
		t.Errorf("/healthz with the app on = %d", r.code)
	}

	// the twin: with the dial unset the same pages are the 404 envelope they always were, and there is no /config.json
	for _, p := range []string{"/", "/jobs", "/config.json", "/assets/app-abc123.js"} {
		if r := get(t, "http://"+off.addr+p); r.code != http.StatusNotFound || !strings.Contains(r.body, `"code":"not_found"`) || r.header.Get("Content-Security-Policy") != "" {
			t.Errorf("GET %s with no PLATFORM_WEB_DIR = %d %q, want the 404 envelope", p, r.code, r.body)
		}
	}
	for _, r := range []*running{on, off} {
		if err := r.stop(t); err != nil {
			t.Errorf("Run returned %v after the cancel", err)
		}
	}
	if find(on.logs.lines(t), "web app is served from this process") == nil {
		t.Errorf("the start-up log does not say the app is served: %s", on.logs.String())
	}
	if find(off.logs.lines(t), "web app is served from this process") != nil {
		t.Errorf("the service without the dial says it serves the app")
	}
	for _, secret := range []string{testAnonKey, dir} {
		if strings.Contains(on.logs.String(), secret) {
			t.Errorf("a value of a web dial reached the log: %q", secret)
		}
	}
}

func TestRun_AWebDirWithoutWhatTheBrowserNeedsRefusesToStartAndNamesTheDial(t *testing.T) {
	dir := builtApp(t)
	for _, c := range []struct {
		name string
		drop []string
		want []string
	}{
		{"no address", []string{config.VarPublicSupabaseURL}, []string{config.VarPublicSupabaseURL}},
		{"no key", []string{config.VarPublicSupabaseAnonKey}, []string{config.VarPublicSupabaseAnonKey}},
		{"neither", []string{config.VarPublicSupabaseURL, config.VarPublicSupabaseAnonKey}, []string{config.VarPublicSupabaseURL, config.VarPublicSupabaseAnonKey}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := webEnv(t, dir)
			for _, d := range c.drop {
				delete(env, d)
			}
			logs := &syncBuffer{}
			err := Run(t.Context(), envOf(env), logs)
			var ce *config.Error
			if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
				t.Fatalf("Run = %v (exit %d), want a configuration error", err, ExitCode(err))
			}
			for _, name := range c.want {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the message does not name %s: %v", name, err)
				}
			}
			for _, present := range []string{config.VarPublicSupabaseURL, config.VarPublicSupabaseAnonKey} {
				named := strings.Contains(err.Error(), present)
				wanted := false
				for _, w := range c.want {
					wanted = wanted || w == present
				}
				if named != wanted {
					t.Errorf("the message names %s = %v, want %v: %v", present, named, wanted, err)
				}
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), testAnonKey) {
				t.Errorf("the message echoes a value: %v", err)
			}
			if logs.String() != "" {
				t.Errorf("a process that could not read its configuration wrote logs: %s", logs.String())
			}
		})
	}
	// the twin: all three dials and the same directory start the service, which is listening
	r := startApp(t, webEnv(t, dir))
	if err := r.stop(t); err != nil {
		t.Fatalf("the complete configuration did not run cleanly: %v", err)
	}
}

func TestRun_AWebDirThatIsNotTheBuiltAppRefusesToStartBeforeTheDatabaseIsOpened(t *testing.T) {
	notAnApp := t.TempDir() // a directory with no index.html
	missing := filepath.Join(t.TempDir(), "no-such-directory-XYZ123")
	for name, dir := range map[string]string{"a directory with no index.html": notAnApp, "a directory that does not exist": missing} {
		t.Run(name, func(t *testing.T) {
			logs := &syncBuffer{}
			err := Run(t.Context(), envOf(webEnv(t, dir)), logs)
			var ce *config.Error
			if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
				t.Fatalf("Run = %v (exit %d), want a configuration error", err, ExitCode(err))
			}
			if len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarWebDir {
				t.Fatalf("problems = %+v, want exactly one, naming %s", ce.Problems, config.VarWebDir)
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(logs.String(), canary) {
				t.Errorf("the message echoes the path or the credential: %v / %s", err, logs.String())
			}
			if find(logs.lines(t), "web app is served from this process") != nil {
				t.Error("the service said it serves an app it could not open")
			}
		})
	}
}
