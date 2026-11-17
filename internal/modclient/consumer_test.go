package modclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/config"
)

// The consumer of PLATFORM_MODULE_TIMEOUT_MS, PLATFORM_MODULE_RETRIES and PLATFORM_MODULE_RETRY_BACKOFF_MS (CLAUDE.md: "every dial is seeded with a consumer at the commit that
// seeds it"). The dials are read by config.Load into Config, handed to the client as FromConfig(cfg), and from there decide how long an attempt may take, how often an idempotent
// request is repeated and how long the client waits between attempts. The service builds no module client before the adapters (Phase 3, B3 to B5) exist, so these tests are the
// proof that the whole path works: the same chain the adapters will run.

func dialEnv(extra map[string]string) func(string) string {
	m := map[string]string{
		config.VarDatabaseURL:  "postgres://platform_app:pw@db.example:5432/postgres",
		config.VarM3URL:        "http://m3.example:8080",
		config.VarM3AdminToken: "operator-operator-ok",
	}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func fromDials(t *testing.T, extra map[string]string) Options {
	t.Helper()
	cfg, err := config.Load(dialEnv(extra))
	if err != nil {
		t.Fatal(err)
	}
	return FromConfig(cfg)
}

// The documented defaults of the dials and the defaults of the package are one set of numbers: a client built from an unset environment behaves as one built with
// DefaultOptions.
func TestFromConfig_TheDialsDefaultToWhatDefaultOptionsSays(t *testing.T) {
	o := fromDials(t, nil)
	if o.Timeout != 15*time.Second || o.Retries != 2 || o.Backoff != 250*time.Millisecond {
		t.Fatalf("the documented defaults did not reach the options: %+v", o)
	}
	d := DefaultOptions()
	if o.Timeout != d.Timeout || o.Retries != d.Retries || o.Backoff != d.Backoff || DefaultTimeout != d.Timeout || DefaultRetries != d.Retries || DefaultBackoff != d.Backoff {
		t.Fatalf("the dials' defaults %+v and the package's defaults %+v drifted apart", o, d)
	}
	// The zero Config (one that did not come from Load) still builds a usable client: New completes the timeout and the body limit, and no retries is the safe reading of 0.
	c, err := New(M2, "http://module.test:8080", "tok-x", nil, FromConfig(config.Config{}))
	if err != nil || c.opts.Timeout != DefaultTimeout || c.opts.MaxBodyBytes != DefaultMaxBodyBytes || c.opts.Retries != 0 {
		t.Fatalf("%+v, %v", c, err)
	}
}

// Changing each variable changes what the client does: the dial has a consumer. The paired control of each is the default beside it.
func TestFromConfig_EveryDialChangesWhatTheClientDoes(t *testing.T) {
	tok := mustToken(t)
	build := func(t *testing.T, s *stub, extra map[string]string, rec *recorder) *Client {
		t.Helper()
		o := fromDials(t, extra)
		if rec != nil {
			o.Sleep = rec.sleep
		}
		c, err := New(M2, s.srv.URL, config.Secret(tok), transport(t), o)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	always503 := func(w http.ResponseWriter, r *http.Request, n int) { jsonReply(w, 503, m1NotWired) }
	get := func() Request {
		r := getReq("m2.report", "/v1/report")
		r.Idempotent = true
		return r
	}

	t.Run("PLATFORM_MODULE_RETRIES is the number of extra attempts", func(t *testing.T) {
		for _, c := range []struct {
			value string // "" is the unset default
			hits  int
		}{{"", 3}, {"0", 1}, {"1", 2}, {"4", 5}, {"10", 11}} {
			s := newStub(t, always503)
			extra := map[string]string{}
			if c.value != "" {
				extra[config.VarModuleRetries] = c.value
			}
			_, err := build(t, s, extra, &recorder{}).Do(ctxT(t, 10*time.Second), get())
			wantKind(t, err, Unavailable)
			if s.count() != c.hits {
				t.Errorf("%s=%q: %d attempt(s), want %d", config.VarModuleRetries, c.value, s.count(), c.hits)
			}
		}
	})

	t.Run("PLATFORM_MODULE_RETRY_BACKOFF_MS is the first wait, and it doubles", func(t *testing.T) {
		for _, c := range []struct {
			value string
			waits []time.Duration
		}{
			{"", []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}},
			{"0", []time.Duration{0, 0}},
			{"600", []time.Duration{600 * time.Millisecond, 1200 * time.Millisecond}},
			{"30000", []time.Duration{30 * time.Second, 30 * time.Second}},
		} {
			s := newStub(t, always503)
			rec := &recorder{}
			extra := map[string]string{config.VarModuleRetries: "2"}
			if c.value != "" {
				extra[config.VarModuleRetryBackoffMS] = c.value
			}
			_, _ = build(t, s, extra, rec).Do(ctxT(t, 10*time.Second), get())
			got := rec.got()
			if len(got) != len(c.waits) || got[0] != c.waits[0] || got[1] != c.waits[1] {
				t.Errorf("%s=%q: waits %v, want %v", config.VarModuleRetryBackoffMS, c.value, got, c.waits)
			}
		}
	})

	t.Run("PLATFORM_MODULE_TIMEOUT_MS is how long one attempt may take", func(t *testing.T) {
		slowFor := func(d time.Duration) func(w http.ResponseWriter, r *http.Request, n int) {
			return func(w http.ResponseWriter, r *http.Request, n int) {
				select {
				case <-time.After(d):
					jsonReply(w, 200, okBody)
				case <-r.Context().Done():
				}
			}
		}
		noRetry := map[string]string{config.VarModuleRetries: "0", config.VarModuleTimeoutMS: "500"}
		// a module that answers within 500 ms is served; one that takes 3 s is a Timeout after about 500 ms
		quick := newStub(t, slowFor(50*time.Millisecond))
		if resp, err := build(t, quick, noRetry, nil).Do(ctxT(t, 10*time.Second), get()); err != nil || resp == nil {
			t.Fatalf("a module that answers within the timeout was refused: %v", err)
		}
		slow := newStub(t, slowFor(3*time.Second))
		start := time.Now()
		_, err := build(t, slow, noRetry, nil).Do(ctxT(t, 10*time.Second), get())
		wantKind(t, err, Timeout)
		if el := time.Since(start); el < 400*time.Millisecond || el > 2500*time.Millisecond {
			t.Errorf("the attempt ended after %s, want about the 500 ms of the dial", el)
		}
		// the same module and the same delay are served when the dial is larger (the dial, and nothing else, was the limit)
		wide := newStub(t, slowFor(700*time.Millisecond))
		if resp, err := build(t, wide, map[string]string{config.VarModuleRetries: "0", config.VarModuleTimeoutMS: "5000"}, nil).Do(ctxT(t, 10*time.Second), get()); err != nil || resp == nil {
			t.Fatalf("a larger timeout did not serve the same module: %v", err)
		}
		narrow := newStub(t, slowFor(700*time.Millisecond))
		_, err = build(t, narrow, noRetry, nil).Do(ctxT(t, 10*time.Second), get())
		wantKind(t, err, Timeout)
	})
}

// An invalid value stops the process at start-up with exit status 2, naming the variable and the reason, never the value (the entry point's own check, not only Load's); the
// controls are the accepted edges, which load.
func TestModuleDials_InvalidValuesStopTheProcessAtStartupWithExitStatus2(t *testing.T) {
	bad := map[string][]string{
		config.VarModuleTimeoutMS:      {"499", "0", "-1", "300001", "soon-XYZ123", "1.5"},
		config.VarModuleRetries:        {"-1", "11", "often-XYZ123", "1.5"},
		config.VarModuleRetryBackoffMS: {"-1", "30001", "slowly-XYZ123", "0.5"},
	}
	n := 0
	for name, values := range bad {
		for _, v := range values {
			n++
			err := app.Run(context.Background(), dialEnv(map[string]string{name: v}), io.Discard)
			var ce *config.Error
			if !errors.As(err, &ce) {
				t.Errorf("%s=%q: Run = %v, want a *config.Error", name, v, err)
				continue
			}
			if got := app.ExitCode(err); got != 2 {
				t.Errorf("%s=%q: exit status %d, want 2", name, v, got)
			}
			if len(ce.Problems) != 1 || ce.Problems[0].Name != name {
				t.Errorf("%s=%q: problems %+v, want exactly one, naming the variable", name, v, ce.Problems)
			}
			if len(v) >= 5 && strings.Contains(err.Error(), v) {
				t.Errorf("%s=%q: the error echoes the value: %v", name, v, err)
			}
		}
	}
	if n < 14 {
		t.Fatalf("only %d invalid values were tried", n)
	}
	for name, ok := range map[string][]string{
		config.VarModuleTimeoutMS:      {"500", "300000", "15000", "  "},
		config.VarModuleRetries:        {"0", "10", "2", "  "},
		config.VarModuleRetryBackoffMS: {"0", "30000", "250", "  "},
	} {
		for _, v := range ok {
			if _, err := config.Load(dialEnv(map[string]string{name: v})); err != nil {
				t.Errorf("%s=%q was refused: %v", name, v, err)
			}
		}
	}
}
