package probe_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

var sentinels = []string{probetest.SentinelM1, probetest.SentinelM2, probetest.SentinelM3}

func leaked(o outcome, extra ...string) []string {
	var out []string
	for _, s := range append(append([]string(nil), sentinels...), extra...) {
		if strings.Contains(o.stdout, s) {
			out = append(out, s+" in stdout")
		}
		if strings.Contains(o.stderr, s) {
			out = append(out, s+" in stderr")
		}
	}
	return out
}

// echoCredential makes a module answer 200 and put the Authorization header it received into the fields
// the probe quotes in a failure reason, which is the worst case for a secret leaking through an error
// string.
func echoCredential() probetest.Wrap {
	return probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
		d := probetest.Describe("send_managed", false)
		h := r.Header.Get("Authorization")
		d["module"], d["api_version"] = h, h
		probetest.WriteJSON(w, 200, d)
	})
}

// longToken is longer than any display width a clipped value could have had.
var longToken = "zzzz-long-" + strings.Repeat("0123456789", 8)

func TestSecrets_NeverAppearInAnyOutput(t *testing.T) {
	always401 := func(w http.ResponseWriter, r *http.Request) {
		probetest.WriteJSON(w, 401, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
	}
	scenarios := []struct {
		name     string
		opts     probetest.Options
		env      func(map[string]string)
		wantExit int
		wantRed  string   // a failing check id that must be present, "" for a green run
		redacted bool     // the failure reason must have carried a secret that the scrubber replaced
		extra    []string // further values that must not appear
	}{
		{name: "a passing run", wantExit: 0},
		{name: "wrong tokens for two modules", wantExit: 1, wantRed: "m3.describe.ok",
			env: func(e map[string]string) {
				e["PROBE_M2_TOKEN"], e["PROBE_M3_TOKEN"] = "zzzz-wrong-a-zzzz", "zzzz-wrong-b-zzzz"
			}},
		{name: "module 1 rejects its own key", wantExit: 1, wantRed: "m1.jobs.right_key.accepted",
			opts: probetest.Options{M1: probetest.Replace("/v1/jobs/", always401)}},
		{name: "a module that echoes the credential into describe", wantExit: 1, wantRed: "m2.describe.module", redacted: true,
			opts: probetest.Options{M2: echoCredential()}},
		{name: "a module that echoes a very long credential", wantExit: 1, wantRed: "m2.describe.module", redacted: true,
			opts: probetest.Options{M2: echoCredential()}, extra: []string{longToken},
			env: func(e map[string]string) { e["PROBE_M2_TOKEN"] = longToken }},
		{name: "module 1 echoing the key into its error code", wantExit: 1, wantRed: "m1.jobs.right_key.accepted", redacted: true,
			opts: probetest.Options{M1: probetest.Replace("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
				probetest.WriteJSON(w, 404, probetest.Envelope(r.Header.Get("Authorization"), probetest.M1RequestID()))
			})}},
		{name: "an unreachable module", wantExit: 1, wantRed: "m2.healthz.ok",
			env: func(e map[string]string) { e["PROBE_M2_URL"] = probetest.ClosedURL(t) }},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			st := start(t, sc.opts)
			env := st.Env()
			if sc.env != nil {
				sc.env(env)
			}
			o := runMain(env)
			if o.code != sc.wantExit {
				t.Fatalf("exit %d, want %d\n%s", o.code, sc.wantExit, o)
			}
			if sc.wantRed != "" && !strings.Contains(o.stdout, "CHECK "+sc.wantRed+" FAIL ") {
				t.Fatalf("expected %s to fail\n%s", sc.wantRed, o)
			}
			if l := leaked(o, sc.extra...); len(l) != 0 {
				t.Fatalf("secrets in the output: %v\n%s", l, o)
			}
			// Non-vacuity: the credentials really were on the wire, so "not in the output" is a
			// property of the probe and not of a probe that never used them.
			if sc.name == "a passing run" {
				if reqs := st.Requests("m2"); reqs[2].Auth != "right" {
					t.Fatalf("m2 did not receive the configured token: %v", reqs)
				}
				if reqs := st.Requests("m1"); reqs[2].Auth != "right" {
					t.Fatalf("m1 did not receive the configured key: %v", reqs)
				}
			}
			if sc.redacted && !strings.Contains(o.stdout, "[redacted]") {
				t.Fatalf("the failure reason carried the credential but no [redacted] marker appears: the scrubber did not run\n%s", o)
			}
			if !strings.Contains(o.stdout, "PROBE SUMMARY") {
				t.Fatalf("no summary line\n%s", o)
			}
		})
	}
}

func TestSecrets_TheScrubberIsWhatRemovesThem(t *testing.T) {
	const secret = "zzzz-scrub-control-zzzz"
	// Without the secret registered, the recorder writes it: the instrument can see a leak.
	var plain bytes.Buffer
	probe.NewRecorder(&plain, &plain).Fail("x.y", probe.Positive, "expected a, got "+secret)
	if !strings.Contains(plain.String(), secret) {
		t.Fatalf("control failed: an unregistered secret was not visible in the output %q", plain.String())
	}

	// Registered: replaced, wherever it sits in the reason.
	var scrubbed bytes.Buffer
	rec := probe.NewRecorder(&scrubbed, &scrubbed, probe.Secret(secret))
	rec.Fail("x.y", probe.Positive, "got Bearer "+secret+" and again "+secret)
	rec.NotApplicable("x.z", "because "+secret)
	if strings.Contains(scrubbed.String(), secret) || strings.Count(scrubbed.String(), "[redacted]") != 3 {
		t.Fatalf("scrubbed output = %q", scrubbed.String())
	}

	// A line cut by the length cap cannot leave a secret fragment behind: the scrub runs first.
	var capped bytes.Buffer
	rec = probe.NewRecorder(&capped, &capped, probe.Secret(secret))
	rec.Fail("x.y", probe.Positive, strings.Repeat("p", 380)+secret)
	if strings.Contains(capped.String(), "zzzz-scrub") {
		t.Fatalf("a fragment of the secret survived the cap: %q", capped.String())
	}

	// An empty secret is ignored (replacing "" would garble every line).
	var empty bytes.Buffer
	probe.NewRecorder(&empty, &empty, "").Pass("a.b", probe.Positive)
	if empty.String() != "CHECK a.b PASS\n" {
		t.Fatalf("empty secret garbled the output: %q", empty.String())
	}
}

func fmtAll(v any) string {
	return fmt.Sprintf("%v|%+v|%#v|%s|%q|%d|%x|%T", v, v, v, v, v, v, v, v)
}

func TestSecrets_TheSecretTypeCannotBePrintedByAccident(t *testing.T) {
	const secret = "zzzz-type-control-zzzz"
	s := probe.Secret(secret)
	cfg := probe.Config{M1Key: s, M2Token: s, M3Token: s}

	rendered := []string{fmtAll(s), fmtAll(cfg), fmtAll(&cfg), fmt.Sprint(s), fmt.Sprint([]probe.Secret{s}), fmt.Sprint(map[string]probe.Secret{"k": s})}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rendered = append(rendered, string(b))
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "k", s, "cfg", cfg)
	rendered = append(rendered, logged.String())
	for i, r := range rendered {
		if strings.Contains(r, secret) {
			t.Errorf("rendering %d printed the secret: %q", i, r)
		}
	}
	// Control: Reveal is the one way to the value.
	if s.Reveal() != secret {
		t.Fatal("Reveal does not return the value")
	}
}

func TestSecrets_URLCredentialsAreNeitherSentNorPrinted(t *testing.T) {
	t.Parallel()
	const userinfo = "probeuser:zzzz-urlpass-zzzz@"
	withCreds := func(url string) string { return strings.Replace(url, "http://", "http://"+userinfo, 1) }

	// A live service: the credentials in the URL must not be sent as Basic auth (it would turn a "no
	// credential" request into an authenticated one) and must not be printed.
	st := start(t, probetest.Options{})
	cfg, err := probe.LoadConfig(getenvOf(st.Env()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.M2URL, cfg.M3URL = withCreds(cfg.M2URL), withCreds(cfg.M3URL)
	o := runPlan(t, cfg, probe.NewClient(cfg.Timeout), probe.DefaultPlan(cfg))
	wantGreen(t, o)
	for _, name := range []string{"m2", "m3"} {
		if reqs := st.Requests(name); reqs[3].Auth != "none" || reqs[4].Auth != "wrong" {
			t.Fatalf("%s: the no-credential request was %q (userinfo sent as Basic auth?), the wrong-credential one %q: %v", name, reqs[3].Auth, reqs[4].Auth, reqs)
		}
	}
	if strings.Contains(o.stdout+o.stderr, "zzzz-urlpass-zzzz") || strings.Contains(o.stdout+o.stderr, "probeuser") {
		t.Fatalf("URL credentials were printed\n%s", o)
	}

	// An unreachable service whose URL carries credentials: the transport error text of net/http quotes
	// the whole URL, so the probe must name the class only.
	cfg.M2URL = withCreds(probetest.ClosedURL(t))
	o = runPlan(t, cfg, probe.NewClient(cfg.Timeout), probe.DefaultPlan(cfg))
	if o.code != 1 || !strings.Contains(o.checkLine("m2.healthz.ok"), "request failed: connection refused") {
		t.Fatalf("an unreachable m2 was not reported as refused\n%s", o)
	}
	if strings.Contains(o.stdout+o.stderr, "zzzz-urlpass-zzzz") || strings.Contains(o.stdout+o.stderr, "probeuser") {
		t.Fatalf("URL credentials were printed in a network error\n%s", o)
	}
}

func TestSecrets_ATimeoutNamesTheClassNotTheRequest(t *testing.T) {
	t.Parallel()
	st := start(t, probetest.Options{M3: probetest.Slow("/healthz", time.Second)})
	cfg, err := probe.LoadConfig(getenvOf(st.Env()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 100 * time.Millisecond
	cfg.M3URL = strings.Replace(cfg.M3URL, "http://", "http://probeuser:zzzz-urlpass-zzzz@", 1)
	o := runPlan(t, cfg, probe.NewClient(cfg.Timeout), probe.DefaultPlan(cfg))
	if line := o.checkLine("m3.healthz.ok"); !strings.HasSuffix(line, "FAIL request failed: timeout") {
		t.Fatalf("m3 healthz line = %q, want it to end with the class only\n%s", line, o)
	}
	if strings.Contains(o.stdout+o.stderr, "zzzz-urlpass-zzzz") || strings.Contains(o.stdout+o.stderr, "Client.Timeout") {
		t.Fatalf("a timeout printed the URL or net/http's error text\n%s", o)
	}
}
