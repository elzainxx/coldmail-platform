package probe_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

// ---- the healthy run -------------------------------------------------------------------------------------------

func TestHealthyRun_AllFourServicesPass(t *testing.T) {
	st := start(t, probetest.Options{})
	o := run(t, st.Env(), nil)

	// The exact outcome: 52 checks, all passing, nothing NOT-APPLICABLE, floor met.
	wantGreen(t, o)
	if o.report.Checks < o.report.Floor {
		t.Fatalf("a green run executed %d checks, below the floor %d", o.report.Checks, o.report.Floor)
	}
	if got := o.summary(); got != "PROBE SUMMARY checks=52 pass=52 fail=0 not_applicable=0" {
		t.Fatalf("summary = %q", got)
	}

	// Every stdout line is one of the three line kinds, and nothing else.
	pass := 0
	for _, l := range o.stdoutLines() {
		if !checkLineRE.MatchString(l) {
			t.Errorf("stdout line %q is not CHECK / NOT-APPLICABLE / PROBE SUMMARY", l)
		}
		if strings.HasSuffix(l, " PASS") {
			pass++
		}
	}
	if pass != healthyChecks {
		t.Fatalf("%d PASS lines, want %d", pass, healthyChecks)
	}
	// Module 1 has a describe now: it is checked for real, and nothing is recorded as not applicable.
	if strings.Contains(o.stdout, "NOT-APPLICABLE") {
		t.Fatalf("a healthy run printed a NOT-APPLICABLE line\n%s", o)
	}
	if !strings.Contains(o.stdout, "CHECK m1.describe.ok PASS\n") {
		t.Fatalf("module 1's describe was not checked\n%s", o)
	}

	// Positive and negative counts, unique ids, and no not-applicable line.
	var pos, neg, na int
	seen := map[string]bool{}
	for _, r := range o.report.Results {
		if seen[r.ID] {
			t.Errorf("check id %s recorded twice", r.ID)
		}
		seen[r.ID] = true
		switch {
		case r.Status == probe.StatusNotApplicable:
			na++
		case r.Kind == probe.Negative:
			neg++
		default:
			pos++
		}
	}
	if pos != healthyPositive || neg != healthyNegative || na != healthyNotApplicable {
		t.Fatalf("positive=%d negative=%d not-applicable=%d, want %d/%d/%d", pos, neg, na, healthyPositive, healthyNegative, healthyNotApplicable)
	}

	// The request lines on stderr: one per request, labelled, and never carrying a credential.
	reqs := 0
	for _, l := range strings.Split(strings.TrimRight(o.stderr, "\n"), "\n") {
		if !strings.HasPrefix(l, "REQUEST ") {
			continue
		}
		reqs++
		if !requestRE.MatchString(l) {
			t.Errorf("request line %q does not match the expected form", l)
		}
	}
	if reqs != 20 {
		t.Fatalf("%d request lines, want 20\n%s", reqs, o.stderr)
	}
	if !strings.Contains(o.stderr, "PROBE NOT CHECKED:") {
		t.Fatalf("the run did not say what it did not check\n%s", o.stderr)
	}
	// The not-checked line no longer claims that module 1 has no describe.
	if strings.Contains(o.stderr, "module 1 describe") || strings.Contains(o.stderr, "has none") {
		t.Fatalf("the not-checked line still says module 1 has no describe\n%s", o.stderr)
	}

	// What the services actually received: the negative checks really sent no credential and a wrong
	// one, the positive checks the right one. Without this, "the negative check passed" proves nothing.
	jobRE := regexp.MustCompile(`^/v1/jobs/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	got := func(name string) []string {
		var out []string
		for _, r := range st.Requests(name) {
			path := r.Path
			if jobRE.MatchString(path) {
				path = "/v1/jobs/<uuid>"
			} else if strings.HasPrefix(path, "/v1/jobs/") {
				t.Errorf("%s was asked for %q, which is not a well-formed v4 UUID", name, path)
			}
			out = append(out, r.Method+" "+path+" "+r.Auth)
		}
		return out
	}
	want := map[string][]string{
		"platform": {"GET /healthz none", "GET /readyz none"},
		"m1": {"GET /healthz none", "GET /readyz none",
			"GET /v1/jobs/<uuid> right", "GET /v1/jobs/<uuid> none", "GET /v1/jobs/<uuid> wrong",
			"GET /v1/describe right", "GET /v1/describe none", "GET /v1/describe wrong"},
		"m2": {"GET /healthz none", "GET /readyz none",
			"GET /v1/describe right", "GET /v1/describe none", "GET /v1/describe wrong"},
		"m3": {"GET /healthz none", "GET /readyz none",
			"GET /v1/describe right", "GET /v1/describe none", "GET /v1/describe wrong"},
	}
	for name, w := range want {
		if g := got(name); strings.Join(g, "|") != strings.Join(w, "|") {
			t.Errorf("%s received %v, want %v", name, g, w)
		}
	}

	// The whole program (Main) prints the same thing and exits 0.
	om := runMain(st.Env())
	if om.code != 0 || om.stdout != o.stdout {
		t.Fatalf("Main differs from Execute:\n%s", om)
	}
}

// ---- one test per way to go red; each names exactly which checks fail ------------------------------------------

func describeIDs(name string, extension bool) []string {
	ids := []string{"ok", "module", "api_version", "capabilities", "pricing", "vocabularies", "error_codes"}
	for _, k := range []string{"extension_routes", "schemas", "bulk_add"} {
		if extension {
			ids = append(ids, "present."+k)
		} else {
			ids = append(ids, "absent."+k)
		}
	}
	for i := range ids {
		ids[i] = name + ".describe." + ids[i]
	}
	return ids
}

func TestRed_DescribeModuleNameSwapped(t *testing.T) {
	swap := func(to string) probetest.Wrap {
		return probetest.MutateJSON("/v1/describe", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
			if status == 200 {
				body["module"] = to
			}
			return status, body
		})
	}
	t.Run("module 2 says it is module 3", func(t *testing.T) {
		st := start(t, probetest.Options{M2: swap("send_byo")})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m2.describe.module": `expected module="send_managed", got "send_byo"`,
		})
	})
	t.Run("module 3 says it is module 2", func(t *testing.T) {
		st := start(t, probetest.Options{M3: swap("send_managed")})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m3.describe.module": `expected module="send_byo", got "send_managed"`,
		})
	})
	t.Run("module 2 answers with module 3's whole describe", func(t *testing.T) {
		// A real swap of the two services: the name AND the three extension keys.
		st := start(t, probetest.Options{M2: probetest.MutateJSON("/v1/describe",
			func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
				if status == 200 {
					return status, probetest.Describe("send_byo", true)
				}
				return status, body
			})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m2.describe.module":                  `got "send_byo"`,
			"m2.describe.absent.extension_routes": "expected extension_routes to be absent in module 2",
			"m2.describe.absent.schemas":          "expected schemas to be absent in module 2",
			"m2.describe.absent.bulk_add":         "expected bulk_add to be absent in module 2",
		})
	})
	t.Run("module 3 lacks its extension keys", func(t *testing.T) {
		st := start(t, probetest.Options{M3: probetest.MutateJSON("/v1/describe",
			func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
				if status == 200 {
					delete(body, "extension_routes")
					delete(body, "bulk_add")
					body["schemas"] = nil
				}
				return status, body
			})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m3.describe.present.extension_routes": "expected extension_routes to be a non-empty array, got <missing>",
			"m3.describe.present.schemas":          "expected schemas to be present, got null",
			"m3.describe.present.bulk_add":         "expected bulk_add to be present, got <missing>",
		})
	})
}

func TestRed_DescribeOpenWithoutAToken(t *testing.T) {
	open := func(w http.ResponseWriter, r *http.Request) {
		probetest.WriteJSON(w, 200, probetest.Describe("send_managed", false))
	}
	t.Run("fully open: no token and a wrong token are both accepted", func(t *testing.T) {
		st := start(t, probetest.Options{M2: probetest.Replace("/v1/describe", open)})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m2.describe.no_token.rejected":      "expected status 401, got 200",
			"m2.describe.no_token.request_id":    "expected error.request_id matching ^req_[0-9a-f]{32}$, got <missing>",
			"m2.describe.wrong_token.rejected":   "expected status 401, got 200",
			"m2.describe.wrong_token.request_id": "expected error.request_id matching ^req_[0-9a-f]{32}$, got <missing>",
		})
	})
	t.Run("open only when the header is absent", func(t *testing.T) {
		// The realistic bug: middleware that skips authentication when there is no Authorization header.
		skip := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/describe" && r.Header.Get("Authorization") == "" {
					open(w, r)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
		st := start(t, probetest.Options{M2: skip})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m2.describe.no_token.rejected":   "expected status 401, got 200",
			"m2.describe.no_token.request_id": "expected error.request_id",
		})
	})
}

func TestRed_ReadyzNotReady(t *testing.T) {
	notReady := func(db, lock bool, status int) probetest.Wrap {
		return probetest.MutateJSON("/readyz", func(_ *http.Request, _ int, body map[string]any) (int, map[string]any) {
			body["status"] = "not_ready"
			body["checks"] = map[string]any{"db": db, "singleton_lock": lock}
			body["db"], body["singleton_lock"] = db, lock
			return status, body
		})
	}
	t.Run("503", func(t *testing.T) {
		st := start(t, probetest.Options{M3: notReady(false, true, 503)})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m3.readyz.ok": "expected status 200, got 503",
		})
		// The reason names the failing dependency.
		o := run(t, st.Env(), nil)
		if !strings.Contains(o.checkLine("m3.readyz.ok"), "checks not true: db") {
			t.Fatalf("the reason does not name the failing check\n%s", o)
		}
	})
	t.Run("200 that lies: status ready but a dependency false", func(t *testing.T) {
		st := start(t, probetest.Options{M2: probetest.MutateJSON("/readyz", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
			body["checks"] = map[string]any{"db": true, "singleton_lock": false}
			body["singleton_lock"] = false
			return status, body
		})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m2.readyz.ok": "expected checks.singleton_lock=true, got false",
		})
	})
	t.Run("module 1 without its pepper", func(t *testing.T) {
		st := start(t, probetest.Options{M1: probetest.MutateJSON("/readyz", func(_ *http.Request, _ int, body map[string]any) (int, map[string]any) {
			body["status"] = "not_ready"
			body["checks"] = map[string]any{"pepper": false, "db": true}
			body["ready"] = false
			return 503, body
		})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m1.readyz.ok": "checks not true: pepper",
		})
	})
	t.Run("the platform database down", func(t *testing.T) {
		st := start(t, probetest.Options{Platform: probetest.MutateJSON("/readyz", func(_ *http.Request, _ int, body map[string]any) (int, map[string]any) {
			body["status"] = "not_ready"
			body["checks"] = map[string]any{"db": false}
			body["db"] = false
			return 503, body
		})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"platform.readyz.ok": "expected status 200, got 503",
		})
	})
}

func TestRed_M1RejectsTheRightKey(t *testing.T) {
	always401 := func(w http.ResponseWriter, r *http.Request) {
		probetest.WriteJSON(w, 401, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
	}
	st := start(t, probetest.Options{M1: probetest.Replace("/v1/jobs/", always401)})
	wantRed(t, run(t, st.Env(), nil), map[string]string{
		"m1.jobs.right_key.accepted": "expected status 404, got 401 (a 401 means the key was rejected)",
	})
}

func TestRed_M1AnswersWithoutAKey(t *testing.T) {
	skip := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/v1/jobs/") && r.Header.Get("Authorization") == "" {
				probetest.WriteJSON(w, 200, map[string]any{"job": "somebody else's"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	st := start(t, probetest.Options{M1: skip})
	wantRed(t, run(t, st.Env(), nil), map[string]string{
		"m1.jobs.no_key.rejected":   "expected status 401, got 200",
		"m1.jobs.no_key.request_id": "expected error.request_id to be non-empty",
	})
}

func TestRed_M1WrongKeyAccepted(t *testing.T) {
	// Paired with the two above: a module that accepts ANY bearer answers 404 to the wrong key.
	anyKey := probetest.Replace("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			probetest.WriteJSON(w, 401, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
			return
		}
		probetest.WriteJSON(w, 404, probetest.Envelope("not_found", probetest.M1RequestID()))
	})
	st := start(t, probetest.Options{M1: anyKey})
	wantRed(t, run(t, st.Env(), nil), map[string]string{
		"m1.jobs.wrong_key.rejected": "expected status 401, got 404",
	})
}

func TestRed_EmptyOrMissingJobStateVocabulary(t *testing.T) {
	edit := func(set func(vocab map[string]any)) probetest.Wrap {
		return probetest.MutateJSON("/v1/describe", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
			if status == 200 {
				set(body["vocabularies"].(map[string]any))
			}
			return status, body
		})
	}
	for name, c := range map[string]struct {
		set  func(map[string]any)
		want string
	}{
		"values is empty": {
			func(v map[string]any) { v["job_state"] = map[string]any{"values": []any{}} },
			"expected vocabularies.job_state.values to be a non-empty array, got an array of 0",
		},
		"values is null": {
			func(v map[string]any) { v["job_state"] = map[string]any{"values": nil} },
			"got null",
		},
		"job_state is missing": {
			func(v map[string]any) { delete(v, "job_state") },
			"got <missing>",
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := start(t, probetest.Options{M2: edit(c.set)})
			wantRed(t, run(t, st.Env(), nil), map[string]string{"m2.describe.vocabularies": c.want})
		})
	}
}

func TestRed_MalformedDescribeFields(t *testing.T) {
	mutate := func(fn func(body map[string]any)) probetest.Wrap {
		return probetest.MutateJSON("/v1/describe", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
			if status == 200 {
				fn(body)
			}
			return status, body
		})
	}
	cases := map[string]struct {
		fn   func(map[string]any)
		id   string
		want string
	}{
		"api_version v2":        {func(b map[string]any) { b["api_version"] = "v2" }, "m3.describe.api_version", `expected api_version="v1", got "v2"`},
		"capabilities an array": {func(b map[string]any) { b["capabilities"] = []any{1} }, "m3.describe.capabilities", "expected capabilities to be an object, got an array of 1"},
		"pricing in dollars":    {func(b map[string]any) { b["pricing"] = map[string]any{"currency": "usd"} }, "m3.describe.pricing", `expected pricing.currency="credits", got "usd"`},
		"error_codes empty":     {func(b map[string]any) { b["error_codes"] = []any{} }, "m3.describe.error_codes", "expected error_codes to be a non-empty array, got an array of 0"},
		"error_codes entry without a status": {func(b map[string]any) {
			b["error_codes"] = []any{map[string]any{"code": "x", "http_status": 401}, map[string]any{"code": "y"}}
		}, "m3.describe.error_codes", "expected error_codes[1] to be {code: non-empty string, http_status: 100-599}, got code=\"y\" http_status=<missing>"},
		"error_codes status 999": {func(b map[string]any) {
			b["error_codes"] = []any{map[string]any{"code": "x", "http_status": 999}}
		}, "m3.describe.error_codes", "error_codes[0]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st := start(t, probetest.Options{M3: mutate(c.fn)})
			wantRed(t, run(t, st.Env(), nil), map[string]string{c.id: c.want})
		})
	}
}

func TestRed_WrongServiceTokenForOneModule(t *testing.T) {
	// The "harness probe" fault of the phase spec: one module's token is wrong. The positive describe
	// and every field check behind it fail; the negative checks, which expect a 401 anyway, still pass.
	for _, tc := range []struct {
		name, variable string
		extension      bool
	}{{"m2", "PROBE_M2_TOKEN", false}, {"m3", "PROBE_M3_TOKEN", true}} {
		t.Run(tc.name, func(t *testing.T) {
			st := start(t, probetest.Options{})
			env := st.Env()
			env[tc.variable] = "zzzz-sentinel-not-the-token-zzzz"
			want := map[string]string{}
			for _, id := range describeIDs(tc.name, tc.extension) {
				want[id] = "describe answered 401, not 200"
			}
			want[tc.name+".describe.ok"] = "expected status 200, got 401"
			wantRed(t, run(t, env, nil), want)
		})
	}
}

func TestRed_ServiceUnreachable(t *testing.T) {
	t.Parallel()
	// The platform is the service with the fewest requests, so a slow refusal costs the least.
	st := start(t, probetest.Options{})
	env := st.Env()
	env["PROBE_PLATFORM_URL"] = probetest.ClosedURL(t)
	o := run(t, env, nil)
	wantRed(t, o, map[string]string{
		"platform.healthz.ok": "request failed: connection refused",
		"platform.readyz.ok":  "request failed: connection refused",
	})
	// The services that are up are unaffected.
	if !strings.Contains(o.stdout, "CHECK m2.describe.ok PASS") {
		t.Fatalf("an unreachable platform took the other services' checks down with it\n%s", o)
	}
}

func TestRed_SlowServicePastTheTimeout(t *testing.T) {
	t.Parallel()
	slow := probetest.Slow("/healthz", 600*time.Millisecond)

	// Red: the timeout is shorter than the service's answer.
	st := start(t, probetest.Options{M2: slow})
	wantRed(t, run(t, st.Env(), shortTimeout(150*time.Millisecond)), map[string]string{
		"m2.healthz.ok": "request failed: timeout",
	})

	// The paired control: the same service, the same delay, a timeout that is long enough. Green. So the
	// red above is the timeout, not something else about the slow handler.
	st2 := start(t, probetest.Options{M2: slow})
	wantGreen(t, run(t, st2.Env(), shortTimeout(5*time.Second)))
}

func TestRed_Redirect(t *testing.T) {
	// The redirect target serves the correct readiness body, so FOLLOWING the redirect would pass.
	moved := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" && r.URL.RawQuery == "" {
				http.Redirect(w, r, "/readyz?moved=1", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	t.Run("the probe's client does not follow it", func(t *testing.T) {
		st := start(t, probetest.Options{M2: moved})
		o := run(t, st.Env(), nil)
		wantRed(t, o, map[string]string{
			"m2.readyz.ok": "expected status 200, got 302 (a redirect; redirects are not followed)",
		})
		// Not followed means exactly one request to /readyz, and the target is never printed.
		readyz := 0
		for _, r := range st.Requests("m2") {
			if r.Path == "/readyz" {
				readyz++
			}
		}
		if readyz != 1 {
			t.Fatalf("m2 /readyz was requested %d times, want 1 (the redirect must not be followed)", readyz)
		}
		if strings.Contains(o.stdout+o.stderr, "moved=1") {
			t.Fatalf("the redirect target was printed\n%s", o)
		}
	})

	t.Run("a doer that does follow redirects is caught by the check itself", func(t *testing.T) {
		st := start(t, probetest.Options{M2: moved})
		cfg, err := probe.LoadConfig(getenvOf(st.Env()))
		if err != nil {
			t.Fatal(err)
		}
		follower := &http.Client{Timeout: 5 * time.Second} // net/http's default policy: follows
		wantRed(t, runPlan(t, cfg, follower, probe.DefaultPlan(cfg)), map[string]string{
			"m2.readyz.ok": "a redirect was followed",
		})
	})

	t.Run("control: the same follower is green on a service that does not redirect", func(t *testing.T) {
		st := start(t, probetest.Options{})
		cfg, err := probe.LoadConfig(getenvOf(st.Env()))
		if err != nil {
			t.Fatal(err)
		}
		wantGreen(t, runPlan(t, cfg, &http.Client{Timeout: 5 * time.Second}, probe.DefaultPlan(cfg)))
	})
}

func TestRed_ResponseBodyLimit(t *testing.T) {
	// The body is a valid readiness-shaped healthz answer padded to an exact length.
	padded := func(total int) probetest.Wrap {
		const head, tail = `{"status":"ok","at":"x","pad":"`, `"}`
		return probetest.Replace("/healthz", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(head + strings.Repeat("a", total-len(head)-len(tail)) + tail))
		})
	}
	// Exactly 1 MiB passes; one byte more fails. The boundary is the instrument.
	st := start(t, probetest.Options{M2: padded(probe.MaxBodyBytes)})
	wantGreen(t, run(t, st.Env(), nil))

	st2 := start(t, probetest.Options{M2: padded(probe.MaxBodyBytes + 1)})
	wantRed(t, run(t, st2.Env(), nil), map[string]string{
		"m2.healthz.ok": "the response body is larger than 1 MiB",
	})
}

func TestRed_NotJSONAndEmptyBodies(t *testing.T) {
	for name, c := range map[string]struct {
		body, want string
	}{
		"html":    {"<html>proxy error</html>", "expected a JSON object body, got a body that is not JSON"},
		"empty":   {"", "expected a JSON object body, got an empty body"},
		"array":   {"[1,2]", "got a JSON value that is not an object"},
		"trailer": {`{"status":"ok","at":"x"} {}`, "got a body with data after the JSON value"},
	} {
		t.Run(name, func(t *testing.T) {
			st := start(t, probetest.Options{Platform: probetest.Replace("/healthz", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			})})
			wantRed(t, run(t, st.Env(), nil), map[string]string{"platform.healthz.ok": c.want})
		})
	}
}

// ---- a check that cannot run is a failure ----------------------------------------------------------------------

func TestRed_AStepThatPanicsFailsInsteadOfCrashing(t *testing.T) {
	st := start(t, probetest.Options{})
	cfg, _ := probe.LoadConfig(getenvOf(st.Env()))
	cfg.MinChecks = 1
	plan := []probe.Step{
		{Name: "boom", Run: func(context.Context, probe.Doer, *probe.Recorder) {
			panic("secret-looking text that must not be printed")
		}},
		probe.DefaultPlan(cfg)[0],
	}
	o := runPlan(t, cfg, probe.NewClient(cfg.Timeout), plan)
	if o.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", o.code, o)
	}
	if line := o.checkLine("boom.completed"); !strings.Contains(line, "FAIL the step panicked (string)") {
		t.Fatalf("no failure recorded for the panicking step: %q\n%s", line, o)
	}
	if strings.Contains(o.stdout+o.stderr, "secret-looking") {
		t.Fatalf("the panic value was printed\n%s", o)
	}
	// The step after the panic still ran.
	if !strings.Contains(o.stdout, "CHECK platform.healthz.ok PASS") {
		t.Fatalf("a panicking step stopped the later steps\n%s", o)
	}
}

func TestFloor_FewerChecksThanMinChecksIsRed(t *testing.T) {
	st := start(t, probetest.Options{})
	cfg, err := probe.LoadConfig(getenvOf(st.Env()))
	if err != nil {
		t.Fatal(err)
	}
	// A stubbed-out module list: only the platform's two checks ran, and every one of them passed.
	plan := probe.DefaultPlan(cfg)[:1]
	o := runPlan(t, cfg, probe.NewClient(cfg.Timeout), plan)
	if o.code != 1 {
		t.Fatalf("exit %d, want 1: two passing checks are below the floor of %d\n%s", o.code, cfg.MinChecks, o)
	}
	if want := "PROBE SUMMARY checks=2 pass=2 fail=0 not_applicable=0"; o.summary() != want {
		t.Fatalf("summary = %q, want %q (red must come from the floor alone: no check failed)", o.summary(), want)
	}
	if !strings.Contains(o.stderr, "fewer than the floor PROBE_MIN_CHECKS=25") {
		t.Fatalf("the message does not name the floor\n%s", o.stderr)
	}

	// An empty plan is red whatever the floor says: zero checks is never green.
	cfg.MinChecks = 0
	o = runPlan(t, cfg, probe.NewClient(cfg.Timeout), nil)
	if o.code != 1 || o.summary() != "PROBE SUMMARY checks=0 pass=0 fail=0 not_applicable=0" {
		t.Fatalf("an empty plan with a floor of 0 was not red\n%s", o)
	}
}

func TestFloor_BoundaryIsInclusive(t *testing.T) {
	// The same healthy stack, floor at exactly the number of checks, then one above. Both runs have
	// fail=0, so the second is red because of the floor and nothing else.
	st := start(t, probetest.Options{})
	env := st.Env()

	env["PROBE_MIN_CHECKS"] = "52"
	wantGreen(t, runMain(env))

	env["PROBE_MIN_CHECKS"] = "53"
	o := runMain(env)
	if o.code != 1 || o.summary() != "PROBE SUMMARY checks=52 pass=52 fail=0 not_applicable=0" {
		t.Fatalf("a floor of 53 against 52 checks was not red for the floor alone\n%s", o)
	}
	if !strings.Contains(o.stderr, "only 52 checks were executed, fewer than the floor PROBE_MIN_CHECKS=53") {
		t.Fatalf("the message does not name the floor\n%s", o.stderr)
	}
}

// ---- the request id of an error envelope -----------------------------------------------------------------------

func TestRed_RequestIDShape(t *testing.T) {
	// A refused describe whose envelope carries this request id, in place of the healthy one.
	refusedWith := func(requestID any) probetest.Wrap {
		return probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
			if tok := r.Header.Get("Authorization"); tok == "Bearer "+probetest.SentinelM2 {
				probetest.WriteJSON(w, 200, probetest.Describe("send_managed", false))
				return
			}
			env := probetest.Envelope("unauthenticated", "")
			env["error"].(map[string]any)["request_id"] = requestID
			probetest.WriteJSON(w, 401, env)
		})
	}
	const rule = "expected error.request_id matching ^req_[0-9a-f]{32}$, got "
	for name, c := range map[string]struct {
		id   any
		want string
	}{
		"too short":          {"req_abc123", `"req_abc123"`},
		"upper case hex":     {"req_" + strings.Repeat("A", 32), `"req_AAAAAAAA`},
		"no prefix":          {strings.Repeat("a", 32), `"aaaaaaaa`},
		"module 1's UUID":    {probetest.M1RequestID(), `"`},
		"one hex digit over": {"req_" + strings.Repeat("a", 33), `"req_aaaa`},
		"empty":              {"", `""`},
		"not a string":       {12345, "12345"},
		"null":               {nil, "null"},
	} {
		t.Run("module 2 "+name, func(t *testing.T) {
			st := start(t, probetest.Options{M2: refusedWith(c.id)})
			wantRed(t, run(t, st.Env(), nil), map[string]string{
				"m2.describe.no_token.request_id":    rule + c.want,
				"m2.describe.wrong_token.request_id": rule + c.want,
			})
		})
	}

	// Module 1's ids are UUIDs, so its rule is "not empty": a UUID passes (the healthy run proves it), an
	// empty or absent id fails.
	refusedM1 := func(requestID any) probetest.Wrap {
		return probetest.Replace("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer "+probetest.SentinelM1 {
				probetest.WriteJSON(w, 404, probetest.Envelope("not_found", probetest.M1RequestID()))
				return
			}
			env := probetest.Envelope("unauthenticated", "")
			if requestID == nil {
				delete(env["error"].(map[string]any), "request_id")
			} else {
				env["error"].(map[string]any)["request_id"] = requestID
			}
			probetest.WriteJSON(w, 401, env)
		})
	}
	for name, id := range map[string]any{"empty": "", "absent": nil, "not a string": 7} {
		t.Run("module 1 "+name, func(t *testing.T) {
			st := start(t, probetest.Options{M1: refusedM1(id)})
			wantRed(t, run(t, st.Env(), nil), map[string]string{
				"m1.jobs.no_key.request_id":    "expected error.request_id to be non-empty, got ",
				"m1.jobs.wrong_key.request_id": "expected error.request_id to be non-empty, got ",
			})
		})
	}
}

func TestNewClient_Policy(t *testing.T) {
	c := probe.NewClient(7 * time.Second)
	if c.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	// The private network is reached directly, whatever HTTP_PROXY says (net/http skips the proxy for
	// loopback hosts, so a loopback test cannot show this; the policy is asserted where it is set).
	if tr.Proxy != nil {
		t.Error("the transport consults the proxy environment")
	}
	if !tr.DisableKeepAlives {
		t.Error("connections are reused between checks")
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("redirects are followed")
	}
	// Control: the stock client is the opposite on every count, so each assertion above can fail.
	stock := http.DefaultTransport.(*http.Transport)
	if stock.Proxy == nil || stock.DisableKeepAlives || (&http.Client{}).CheckRedirect != nil {
		t.Fatal("control failed: the net/http defaults are not what the assertions above negate")
	}
}

// ---- the health and readiness bodies, on every service --------------------------------------------------------

func TestRed_HealthAndReadinessBodies(t *testing.T) {
	services := map[string]func(*probetest.Options, probetest.Wrap){
		"platform": func(o *probetest.Options, w probetest.Wrap) { o.Platform = w },
		"m1":       func(o *probetest.Options, w probetest.Wrap) { o.M1 = w },
		"m2":       func(o *probetest.Options, w probetest.Wrap) { o.M2 = w },
		"m3":       func(o *probetest.Options, w probetest.Wrap) { o.M3 = w },
	}
	edit := func(path string, fn func(body map[string]any)) probetest.Wrap {
		return probetest.MutateJSON(path, func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
			fn(body)
			return status, body
		})
	}
	for name, set := range services {
		for what, c := range map[string]struct {
			path, id, want string
			fn             func(map[string]any)
		}{
			"healthz status degraded": {"/healthz", ".healthz.ok", `expected status="ok", got "degraded"`, func(b map[string]any) { b["status"] = "degraded" }},
			"healthz status missing":  {"/healthz", ".healthz.ok", `expected status="ok", got <missing>`, func(b map[string]any) { delete(b, "status") }},
			"healthz at missing":      {"/healthz", ".healthz.ok", "expected at to be a non-empty string, got <missing>", func(b map[string]any) { delete(b, "at") }},
			"healthz at empty":        {"/healthz", ".healthz.ok", `expected at to be a non-empty string, got ""`, func(b map[string]any) { b["at"] = "" }},
			"healthz at a number":     {"/healthz", ".healthz.ok", "expected at to be a non-empty string, got 1700000000", func(b map[string]any) { b["at"] = 1700000000 }},
			"readyz status not_ready": {"/readyz", ".readyz.ok", `expected status="ready", got "not_ready"`, func(b map[string]any) { b["status"] = "not_ready" }},
			"readyz at missing":       {"/readyz", ".readyz.ok", "expected at to be a non-empty string, got <missing>", func(b map[string]any) { delete(b, "at") }},
			"readyz checks missing":   {"/readyz", ".readyz.ok", "expected checks.db=true, got <missing>", func(b map[string]any) { delete(b, "checks") }},
		} {
			t.Run(name+" "+what, func(t *testing.T) {
				var o probetest.Options
				set(&o, edit(c.path, c.fn))
				st := start(t, o)
				wantRed(t, run(t, st.Env(), nil), map[string]string{name + c.id: c.want})
			})
		}
	}
}
