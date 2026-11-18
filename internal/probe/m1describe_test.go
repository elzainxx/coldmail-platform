package probe_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

// Module 1 has a describe since the Phase 3 close-out: GET /v1/describe, authenticated with the same
// per-caller key as the job lookup. The probe records m1.describe.ok, one check per asserted field and the
// two refusals. Every test below starts from the healthy stack, which the paired control of each one runs
// first, and changes exactly one thing. wantRed asserts that EXACTLY the named checks fail and that each
// failure line carries the named reason.

const m1RightKey = "Bearer " + probetest.SentinelM1

// m1DescribeFieldIDs are the checks a describe call that produced no usable body fails: ok and every field.
var m1DescribeFieldIDs = []string{
	"m1.describe.ok", "m1.describe.module", "m1.describe.api_version", "m1.describe.capabilities",
	"m1.describe.vocabularies", "m1.describe.error_codes",
}

// m1DescribeRefusalIDs are the four refusal checks, in execution order.
var m1DescribeRefusalIDs = []string{
	"m1.describe.no_key.rejected", "m1.describe.no_key.request_id",
	"m1.describe.wrong_key.rejected", "m1.describe.wrong_key.request_id",
}

// editM1Describe changes the 200 answer of module 1's describe (the one given to the right key).
func editM1Describe(fn func(body map[string]any)) probetest.Wrap {
	return probetest.MutateJSON("/v1/describe", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
		if status == 200 {
			fn(body)
		}
		return status, body
	})
}

// onlyTheRightKeyGets serves h to a request that carries the configured key and the healthy 401 envelope to
// any other request, so a fault in what the right key is given leaves the two refusals untouched.
func onlyTheRightKeyGets(h http.HandlerFunc) probetest.Wrap {
	return probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == m1RightKey {
			h(w, r)
			return
		}
		probetest.WriteJSON(w, 401, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
	})
}

// reasonFor builds a wantRed map giving every id the same reason text.
func reasonFor(ids []string, text string) map[string]string {
	out := map[string]string{}
	for _, id := range ids {
		out[id] = text
	}
	return out
}

// ---- the healthy run -------------------------------------------------------------------------------------------

func TestM1Describe_HealthyRunRecordsTheExactCheckList(t *testing.T) {
	st := start(t, probetest.Options{})
	o := run(t, st.Env(), nil)
	wantGreen(t, o)

	want := []string{
		"m1.describe.ok", "m1.describe.module", "m1.describe.api_version", "m1.describe.capabilities",
		"m1.describe.vocabularies", "m1.describe.error_codes",
		"m1.describe.no_key.rejected", "m1.describe.no_key.request_id",
		"m1.describe.wrong_key.rejected", "m1.describe.wrong_key.request_id",
	}
	var got []string
	var kinds []probe.Kind
	for _, r := range o.report.Results {
		if strings.HasPrefix(r.ID, "m1.describe.") {
			got = append(got, r.ID)
			kinds = append(kinds, r.Kind)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("module 1 describe checks = %v, want %v", got, want)
	}
	// The first six (the call and its fields) are positive, the four refusals are negative.
	for i, k := range kinds {
		wantKind := probe.Negative
		if i < 6 {
			wantKind = probe.Positive
		}
		if k != wantKind {
			t.Errorf("%s is %s, want %s", got[i], k, wantKind)
		}
	}
	// They come after the job lookups and before module 2's checks.
	order := map[string]int{}
	for i, r := range o.report.Results {
		order[r.ID] = i
	}
	if !(order["m1.jobs.wrong_key.request_id"] < order["m1.describe.ok"] && order["m1.describe.wrong_key.request_id"] < order["m2.healthz.ok"]) {
		t.Fatalf("module 1's describe checks are not between its job lookups and module 2: %v", order)
	}

	// The three calls carried the right credentials, in the order positive, none, wrong.
	var describe []string
	for _, r := range st.Requests("m1") {
		if r.Path == "/v1/describe" {
			describe = append(describe, r.Auth)
		}
	}
	if strings.Join(describe, ",") != "right,none,wrong" {
		t.Fatalf("module 1 received describe with %v, want right,none,wrong", describe)
	}

	// The stand-in's body is module 1's, not a sender's: it carries no pricing and no job_state, so nothing
	// the checks assert is borrowed from the senders.
	body := probetest.M1Describe()
	if _, ok := body["pricing"]; ok {
		t.Error("module 1's describe has no pricing key, and the stand-in must not have one")
	}
	if _, ok := body["vocabularies"].(map[string]any)["job_state"]; ok {
		t.Error("module 1's describe has no job_state vocabulary (that is the senders'), and the stand-in must not have one")
	}
}

// ---- one asserted field at a time ------------------------------------------------------------------------------

func TestRed_M1Describe_EachAssertedField(t *testing.T) {
	// The paired control: the same wrapper with an edit that changes nothing is green, so every red below
	// comes from its own edit and not from the wrapper.
	st := start(t, probetest.Options{M1: editM1Describe(func(map[string]any) {})})
	wantGreen(t, run(t, st.Env(), nil))

	caps := func(b map[string]any) map[string]any { return b["capabilities"].(map[string]any) }
	cases := map[string]struct {
		edit func(b map[string]any)
		id   string
		want string
	}{
		"module key missing": {func(b map[string]any) { delete(b, "module") },
			"m1.describe.module", `expected module="verify", got <missing>`},
		"module is send_byo": {func(b map[string]any) { b["module"] = "send_byo" },
			"m1.describe.module", `expected module="verify", got "send_byo"`},
		"module is send_managed": {func(b map[string]any) { b["module"] = "send_managed" },
			"m1.describe.module", `expected module="verify", got "send_managed"`},
		"module is not a string": {func(b map[string]any) { b["module"] = 1 },
			"m1.describe.module", `expected module="verify", got 1`},
		"api_version v2": {func(b map[string]any) { b["api_version"] = "v2" },
			"m1.describe.api_version", `expected api_version="v1", got "v2"`},
		"api_version missing": {func(b map[string]any) { delete(b, "api_version") },
			"m1.describe.api_version", `expected api_version="v1", got <missing>`},
		"capabilities an array": {func(b map[string]any) { b["capabilities"] = []any{1} },
			"m1.describe.capabilities", "expected capabilities to be an object, got an array of 1"},
		"capabilities missing": {func(b map[string]any) { delete(b, "capabilities") },
			"m1.describe.capabilities", "expected capabilities to be an object, got <missing>"},
		"capabilities without idempotent_bulk_create": {func(b map[string]any) { delete(caps(b), "idempotent_bulk_create") },
			"m1.describe.capabilities", "expected capabilities.idempotent_bulk_create=true, got <missing>"},
		"idempotent_bulk_create false": {func(b map[string]any) { caps(b)["idempotent_bulk_create"] = false },
			"m1.describe.capabilities", "expected capabilities.idempotent_bulk_create=true, got false"},
		"idempotent_bulk_create the string true": {func(b map[string]any) { caps(b)["idempotent_bulk_create"] = "true" },
			"m1.describe.capabilities", `expected capabilities.idempotent_bulk_create=true, got "true"`},
		"capabilities without job_totals": {func(b map[string]any) { delete(caps(b), "job_totals") },
			"m1.describe.capabilities", "expected capabilities.job_totals=true, got <missing>"},
		"job_totals false": {func(b map[string]any) { caps(b)["job_totals"] = false },
			"m1.describe.capabilities", "expected capabilities.job_totals=true, got false"},
		"vocabularies missing": {func(b map[string]any) { delete(b, "vocabularies") },
			"m1.describe.vocabularies", "expected vocabularies.status.values to be a non-empty array, got <missing>"},
		"status vocabulary missing": {func(b map[string]any) { delete(b["vocabularies"].(map[string]any), "status") },
			"m1.describe.vocabularies", "expected vocabularies.status.values to be a non-empty array, got <missing>"},
		"status values empty": {func(b map[string]any) {
			b["vocabularies"].(map[string]any)["status"] = map[string]any{"values": []any{}}
		},
			"m1.describe.vocabularies", "expected vocabularies.status.values to be a non-empty array, got an array of 0"},
		"status values null": {func(b map[string]any) { b["vocabularies"].(map[string]any)["status"] = map[string]any{"values": nil} },
			"m1.describe.vocabularies", "got null"},
		"vocabularies are the senders' (job_state, no status)": {func(b map[string]any) {
			b["vocabularies"] = probetest.Describe("send_managed", false)["vocabularies"]
		}, "m1.describe.vocabularies", "got <missing>"},
		"error_codes empty": {func(b map[string]any) { b["error_codes"] = []any{} },
			"m1.describe.error_codes", "expected error_codes to be a non-empty array, got an array of 0"},
		"error_codes missing": {func(b map[string]any) { delete(b, "error_codes") },
			"m1.describe.error_codes", "expected error_codes to be a non-empty array, got <missing>"},
		"error_codes entry without a status": {func(b map[string]any) {
			b["error_codes"] = []any{map[string]any{"code": "x", "http_status": 401}, map[string]any{"code": "y"}}
		}, "m1.describe.error_codes", `expected error_codes[1] to be {code: non-empty string, http_status: 100-599}, got code="y" http_status=<missing>`},
		"error_codes status 999": {func(b map[string]any) {
			b["error_codes"] = []any{map[string]any{"code": "x", "http_status": 999}}
		}, "m1.describe.error_codes", "error_codes[0]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st := start(t, probetest.Options{M1: editM1Describe(c.edit)})
			wantRed(t, run(t, st.Env(), nil), map[string]string{c.id: c.want})
		})
	}
}

func TestM1Describe_NoPricingIsAsserted(t *testing.T) {
	// Paired with the senders' checks, which do assert pricing: module 1 has none, and giving it one changes
	// nothing for module 1 (no check reads it), while removing it from a sender does turn m2 red.
	st := start(t, probetest.Options{M1: editM1Describe(func(b map[string]any) {
		b["pricing"] = map[string]any{"currency": "usd"}
	})})
	wantGreen(t, run(t, st.Env(), nil))

	st2 := start(t, probetest.Options{M2: probetest.MutateJSON("/v1/describe", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
		if status == 200 {
			delete(body, "pricing")
		}
		return status, body
	})})
	wantRed(t, run(t, st2.Env(), nil), map[string]string{
		"m2.describe.pricing": "expected pricing to be an object, got <missing>",
	})
}

func TestRed_M1Describe_AnotherModulesWholeDescribe(t *testing.T) {
	serve := func(module string, extension bool) probetest.Wrap {
		return probetest.MutateJSON("/v1/describe", func(_ *http.Request, status int, body map[string]any) (int, map[string]any) {
			if status == 200 {
				return status, probetest.Describe(module, extension)
			}
			return status, body
		})
	}
	// Both senders' describe documents share module 1's api_version and error_codes shape, and differ in
	// the module name, the capabilities and the vocabularies: exactly those three checks are red.
	for name, opts := range map[string]probetest.Options{
		"module 1 answers with module 2's describe": {M1: serve("send_managed", false)},
		"module 1 answers with module 3's describe": {M1: serve("send_byo", true)},
	} {
		t.Run(name, func(t *testing.T) {
			st := start(t, opts)
			wantRed(t, run(t, st.Env(), nil), map[string]string{
				"m1.describe.module":       `expected module="verify", got "send_`,
				"m1.describe.capabilities": "expected capabilities.idempotent_bulk_create=true, got <missing>",
				"m1.describe.vocabularies": "expected vocabularies.status.values to be a non-empty array, got <missing>",
			})
		})
	}
}

// ---- nothing passes by omission --------------------------------------------------------------------------------

func TestRed_M1Describe_NoUsableBodyFailsEveryField(t *testing.T) {
	const fieldReasonBody = "describe body is not a JSON object, so this field cannot be checked"

	// The control first: the same wrapper, serving the healthy describe, is green.
	healthy := onlyTheRightKeyGets(func(w http.ResponseWriter, r *http.Request) {
		probetest.WriteJSON(w, 200, probetest.M1Describe())
	})
	wantGreen(t, run(t, start(t, probetest.Options{M1: healthy}).Env(), nil))

	cases := map[string]struct {
		serve    http.HandlerFunc
		okReason string
		field    string
	}{
		"an internal error envelope": {
			func(w http.ResponseWriter, r *http.Request) {
				probetest.WriteJSON(w, 500, probetest.Envelope("internal_error", probetest.M1RequestID()))
			},
			"expected status 200, got 500", "describe answered 500, not 200, so this field cannot be checked"},
		"a module without the route (404 not_found)": {
			func(w http.ResponseWriter, r *http.Request) {
				probetest.WriteJSON(w, 404, probetest.Envelope("not_found", probetest.M1RequestID()))
			},
			"expected status 200, got 404", "describe answered 404, not 200, so this field cannot be checked"},
		"a body that is not JSON": {
			func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>proxy error</html>")) },
			"expected a JSON object body, got a body that is not JSON", fieldReasonBody},
		"an empty body": {
			func(w http.ResponseWriter, r *http.Request) {},
			"expected a JSON object body, got an empty body", fieldReasonBody},
		"a JSON array": {
			func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[1,2]")) },
			"got a JSON value that is not an object", fieldReasonBody},
		"a redirect that is not followed": {
			func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/v1/describe?moved=1", http.StatusFound)
			},
			"expected status 200, got 302 (a redirect; redirects are not followed)", "describe answered 302, not 200, so this field cannot be checked"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st := start(t, probetest.Options{M1: onlyTheRightKeyGets(c.serve)})
			want := reasonFor(m1DescribeFieldIDs[1:], c.field)
			want["m1.describe.ok"] = c.okReason
			wantRed(t, run(t, st.Env(), nil), want)
		})
	}
}

func TestRed_M1Describe_RouteMissingForEveryCaller(t *testing.T) {
	// A module pinned from before the close-out answers 404 not_found to describe whatever the credential:
	// the positive call, its five fields and the two status checks of the refusals are red; the refusals'
	// request ids are still well formed, so they pass.
	notFound := probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
		probetest.WriteJSON(w, 404, probetest.Envelope("not_found", probetest.M1RequestID()))
	})
	st := start(t, probetest.Options{M1: notFound})
	want := reasonFor(m1DescribeFieldIDs[1:], "describe answered 404, not 200")
	want["m1.describe.ok"] = "expected status 200, got 404"
	want["m1.describe.no_key.rejected"] = `expected status 401, got 404; expected error.code="unauthenticated", got "not_found"`
	want["m1.describe.wrong_key.rejected"] = `expected status 401, got 404; expected error.code="unauthenticated", got "not_found"`
	wantRed(t, run(t, st.Env(), nil), want)
}

func TestRed_M1Describe_TheConfiguredKeyIsWrong(t *testing.T) {
	// The harness-probe fault for module 1: PROBE_M1_KEY is not the key. The job lookup and the describe
	// call are both answered 401 and every describe field behind the call fails; the refusals, which expect
	// a 401 anyway, still pass.
	st := start(t, probetest.Options{})
	wantGreen(t, run(t, st.Env(), nil))

	env := st.Env()
	env["PROBE_M1_KEY"] = "zzzz-sentinel-not-the-key-zzzz"
	want := reasonFor(m1DescribeFieldIDs[1:], "describe answered 401, not 200")
	want["m1.describe.ok"] = "expected status 200, got 401"
	want["m1.jobs.right_key.accepted"] = "expected status 404, got 401 (a 401 means the key was rejected)"
	wantRed(t, run(t, env, nil), want)
}

func TestRed_M1Describe_ModuleUnreachable(t *testing.T) {
	t.Parallel()
	st := start(t, probetest.Options{})
	healthy := run(t, st.Env(), nil)
	wantGreen(t, healthy)

	env := st.Env()
	env["PROBE_M1_URL"] = probetest.ClosedURL(t)
	want := map[string]string{}
	for _, r := range healthy.report.Results {
		if strings.HasPrefix(r.ID, "m1.") {
			want[r.ID] = "request failed: connection refused"
		}
	}
	if len(want) != healthyM1Checks {
		t.Fatalf("module 1 has %d checks in a healthy run, want %d", len(want), healthyM1Checks)
	}
	for _, id := range append(append([]string(nil), m1DescribeFieldIDs...), m1DescribeRefusalIDs...) {
		if _, ok := want[id]; !ok {
			t.Fatalf("%s is not among module 1's checks", id)
		}
	}
	wantRed(t, run(t, env, nil), want)
}

// ---- the two refusals ------------------------------------------------------------------------------------------

func TestRed_M1Describe_OpenWithoutTheKey(t *testing.T) {
	open := func(w http.ResponseWriter, r *http.Request) {
		probetest.WriteJSON(w, 200, probetest.M1Describe())
	}
	// A refusal that was not given: no error code and no request id in a describe document.
	notRefused := func(name string) map[string]string {
		return map[string]string{
			"m1.describe." + name + ".rejected":   "expected status 401, got 200",
			"m1.describe." + name + ".request_id": "expected error.request_id to be non-empty, got <missing>",
		}
	}
	merge := func(ms ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	// Serve the 401 envelope when the credential is refused, the open document otherwise.
	refuse := func(refusing func(auth string) bool) probetest.Wrap {
		return probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == m1RightKey || !refusing(auth) {
				open(w, r)
				return
			}
			probetest.WriteJSON(w, 401, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
		})
	}

	// The control: a module that refuses both is green (the healthy run), and one that refuses
	// everything but the right key is the same module.
	wantGreen(t, run(t, start(t, probetest.Options{M1: refuse(func(string) bool { return true })}).Env(), nil))

	t.Run("fully open: no key and a wrong key are both accepted", func(t *testing.T) {
		st := start(t, probetest.Options{M1: probetest.Replace("/v1/describe", open)})
		wantRed(t, run(t, st.Env(), nil), merge(notRefused("no_key"), notRefused("wrong_key")))
	})
	t.Run("open only when the header is absent", func(t *testing.T) {
		// The realistic bug: middleware that skips authentication when there is no Authorization header.
		st := start(t, probetest.Options{M1: refuse(func(auth string) bool { return auth != "" })})
		wantRed(t, run(t, st.Env(), nil), notRefused("no_key"))
	})
	t.Run("a wrong key is answered 200", func(t *testing.T) {
		// Any bearer value is accepted; only the absence of one is refused.
		st := start(t, probetest.Options{M1: refuse(func(auth string) bool { return auth == "" })})
		wantRed(t, run(t, st.Env(), nil), notRefused("wrong_key"))
	})
	t.Run("refused with the wrong status", func(t *testing.T) {
		st := start(t, probetest.Options{M1: probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == m1RightKey {
				open(w, r)
				return
			}
			probetest.WriteJSON(w, 403, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
		})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m1.describe.no_key.rejected":    "expected status 401, got 403",
			"m1.describe.wrong_key.rejected": "expected status 401, got 403",
		})
	})
	t.Run("refused with the wrong error code", func(t *testing.T) {
		st := start(t, probetest.Options{M1: probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == m1RightKey {
				open(w, r)
				return
			}
			probetest.WriteJSON(w, 401, probetest.Envelope("forbidden", probetest.M1RequestID()))
		})})
		wantRed(t, run(t, st.Env(), nil), map[string]string{
			"m1.describe.no_key.rejected":    `expected error.code="unauthenticated", got "forbidden"`,
			"m1.describe.wrong_key.rejected": `expected error.code="unauthenticated", got "forbidden"`,
		})
	})
}

func TestRed_M1Describe_RefusalRequestID(t *testing.T) {
	// A refused describe whose envelope is changed by edit (the right key gets the healthy describe).
	refusedWith := func(edit func(errObj map[string]any)) probetest.Wrap {
		return probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == m1RightKey {
				probetest.WriteJSON(w, 200, probetest.M1Describe())
				return
			}
			env := probetest.Envelope("unauthenticated", "")
			edit(env["error"].(map[string]any))
			probetest.WriteJSON(w, 401, env)
		})
	}
	setID := func(id any) func(map[string]any) {
		return func(e map[string]any) { e["request_id"] = id }
	}

	// Controls: module 1's rule is "not empty", so a UUID (as in the healthy run) and a sender-style id both
	// pass. A refusal that differs from the healthy one only in the id is green.
	for name, id := range map[string]string{"a UUID": probetest.M1RequestID(), "a sender-style id": probetest.RequestID()} {
		t.Run("control: "+name+" is accepted", func(t *testing.T) {
			wantGreen(t, run(t, start(t, probetest.Options{M1: refusedWith(setID(id))}).Env(), nil))
		})
	}
	for name, c := range map[string]struct {
		edit func(map[string]any)
		got  string
	}{
		"empty":        {setID(""), `""`},
		"absent":       {func(e map[string]any) { delete(e, "request_id") }, "<missing>"},
		"not a string": {setID(7), "7"},
		"null":         {setID(nil), "null"},
	} {
		t.Run(name, func(t *testing.T) {
			st := start(t, probetest.Options{M1: refusedWith(c.edit)})
			wantRed(t, run(t, st.Env(), nil), map[string]string{
				"m1.describe.no_key.request_id":    "expected error.request_id to be non-empty, got " + c.got,
				"m1.describe.wrong_key.request_id": "expected error.request_id to be non-empty, got " + c.got,
			})
		})
	}
}

// ---- the credential never reaches the output -------------------------------------------------------------------

func TestSecrets_M1DescribeEchoIsScrubbed(t *testing.T) {
	// Module 1 puts the Authorization header it received into the field the probe quotes in a failure.
	echo := probetest.Replace("/v1/describe", func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h == m1RightKey {
			d := probetest.M1Describe()
			d["module"] = h
			probetest.WriteJSON(w, 200, d)
			return
		}
		probetest.WriteJSON(w, 401, probetest.Envelope("unauthenticated", probetest.M1RequestID()))
	})
	st := start(t, probetest.Options{M1: echo})
	o := run(t, st.Env(), nil)
	wantRed(t, o, map[string]string{"m1.describe.module": `expected module="verify", got "Bearer [redacted]"`})
	if l := leaked(o); len(l) != 0 {
		t.Fatalf("secrets in the output: %v\n%s", l, o)
	}
	// Non-vacuity: the key really was on the wire for describe.
	var right int
	for _, r := range st.Requests("m1") {
		if r.Path == "/v1/describe" && r.Auth == "right" {
			right++
		}
	}
	if right != 1 {
		t.Fatalf("module 1 received describe with the configured key %d times, want 1", right)
	}
}
