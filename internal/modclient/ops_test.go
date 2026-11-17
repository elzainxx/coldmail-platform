package modclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

func serve(t testing.TB, status int, ctype, body string) *stub {
	t.Helper()
	return newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func TestHealth_AnswersOnlyForALiveModuleThatSaysOk(t *testing.T) {
	tok := mustToken(t)
	for _, m := range []Module{M1, M2, M3} { // all three serve the same liveness document
		s := serve(t, 200, "application/json", healthOK)
		c := s.client(t, m, tok, Options{})
		h, err := c.Health(ctxT(t, 5*time.Second))
		if err != nil || h == nil || h.Status != "ok" || !h.At.Equal(time.Date(2026, 11, 17, 10, 0, 0, 123456789, time.UTC)) {
			t.Fatalf("%s: %+v, %v", m, h, err)
		}
		if r := s.requests()[0]; r.path != "/healthz" || r.auth != "" || r.method != "GET" {
			t.Fatalf("%s: the liveness call %+v carried a credential or the wrong route", m, r)
		}
	}
	rows := []struct {
		name   string
		status int
		ctype  string
		body   string
		kind   Kind
		key    string
	}{
		{"a status that is not ok", 200, "application/json", `{"status":"degraded","at":"2026-11-17T10:00:00Z"}`, Contract, "status"},
		{"no status", 200, "application/json", `{"at":"2026-11-17T10:00:00Z"}`, Contract, "status"},
		{"no timestamp", 200, "application/json", `{"status":"ok"}`, Contract, "at"},
		{"a timestamp that is not a time", 200, "application/json", `{"status":"ok","at":"yesterday"}`, Contract, "at"},
		{"a timestamp of the wrong type", 200, "application/json", `{"status":"ok","at":5}`, Contract, "at"},
		{"plain text OK", 200, "text/plain", `OK`, Contract, ""},
		{"a 503", 503, "application/json", m1NotWired, Unavailable, ""},
		{"a bare 503", 503, "", ``, Unavailable, ""},
		{"a 404 (the wrong base URL)", 404, "text/html", `<html>`, NotFound, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := serve(t, row.status, row.ctype, row.body)
			h, err := s.client(t, M2, tok, Options{}).Health(ctxT(t, 5*time.Second))
			if h != nil {
				t.Fatalf("a Health came back with %v", err)
			}
			if e := wantKind(t, err, row.kind); e.Key != row.key || e.Route != "m2.healthz" {
				t.Fatalf("%+v", *e)
			}
		})
	}
	// Health is idempotent: a module that comes back is found.
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			jsonReply(w, 503, m1NotWired)
			return
		}
		jsonReply(w, 200, healthOK)
	})
	if h, err := s.client(t, M2, tok, Options{Retries: 1, Sleep: (&recorder{}).sleep}).Health(ctxT(t, 5*time.Second)); err != nil || h == nil || s.count() != 2 {
		t.Fatalf("%+v, %v, %d hit(s)", h, err, s.count())
	}
}

func TestReady_NotReadyIsAnAnswerNotAnError(t *testing.T) {
	tok := mustToken(t)
	rows := []struct {
		name     string
		module   Module
		status   int
		body     string
		ready    bool
		word     string
		failing  []string
		reasons  int
		checksOf int
	}{
		{"module 1 ready", M1, 200, m1Ready200, true, "ready", nil, 0, 3},
		{"module 1 not ready, with its reason", M1, 503, m1Ready503, false, "not_ready", []string{"queue"}, 1, 3},
		{"module 2 ready", M2, 200, senderReady200, true, "ready", nil, 0, 2},
		{"module 2 not ready", M2, 503, senderReady503, false, "not_ready", []string{"db"}, 0, 2},
		{"module 3 ready", M3, 200, senderReady200, true, "ready", nil, 0, 2},
		{"module 3 not ready", M3, 503, senderReady503, false, "not_ready", []string{"db"}, 0, 2},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := serve(t, row.status, "application/json", row.body)
			r, err := s.client(t, row.module, tok, Options{Retries: 3, Sleep: (&recorder{}).sleep}).Ready(ctxT(t, 5*time.Second))
			if err != nil || r == nil {
				t.Fatalf("a readiness answer was turned into an error: %+v, %v", r, err)
			}
			if r.Ready != row.ready || r.Status != row.word || r.HTTPStatus != row.status || len(r.Checks) != row.checksOf || len(r.Reasons) != row.reasons ||
				strings.Join(r.Failing(), ",") != strings.Join(row.failing, ",") {
				t.Fatalf("%+v", r)
			}
			// an answer is not retried, whatever it says, and carries no credential
			if q := s.requests(); len(q) != 1 || q[0].path != "/readyz" || q[0].auth != "" {
				t.Fatalf("requests %+v", q)
			}
		})
	}

	t.Run("the reasons are bounded", func(t *testing.T) {
		var reasons []string
		for i := 0; i < 30; i++ {
			reasons = append(reasons, strings.Repeat("é", 400)) // 800 bytes each
		}
		body := `{"status":"not_ready","checks":{"queue":false},"reasons":["` + strings.Join(reasons, `","`) + `"]}`
		s := serve(t, 503, "application/json", body)
		r, err := s.client(t, M1, tok, Options{}).Ready(ctxT(t, 5*time.Second))
		if err != nil || len(r.Reasons) != 20 {
			t.Fatalf("%v, %d reason(s)", err, len(r.Reasons))
		}
		for _, x := range r.Reasons {
			if len(x) > 500 || !strings.HasPrefix(x, "é") {
				t.Fatalf("a reason of %d bytes", len(x))
			}
		}
	})
}

func TestReady_WhatIsNotAnAnswerIsAnError(t *testing.T) {
	tok := mustToken(t)
	rows := []struct {
		name   string
		status int
		ctype  string
		body   string
		kind   Kind
		key    string
	}{
		{"a 503 with no body", 503, "", ``, Unavailable, ""},
		{"a 503 gateway page", 503, "text/html", `<html>Service Unavailable</html>`, Unavailable, ""},
		{"a 503 whose JSON is not a readiness document", 503, "application/json", `{"status":"not_ready"}`, Unavailable, ""},
		{"a 503 error envelope", 503, "application/json", m1NotWired, Unavailable, ""},
		{"a 503 paused envelope", 503, "application/json", m1Paused, Paused, ""},
		{"a 500", 500, "application/json", m2Internal, Internal, ""},
		{"a 404", 404, "application/json", m2NotFound, NotFound, ""},
		{"a 200 that says not_ready", 200, "application/json", `{"status":"not_ready","checks":{"db":false}}`, Contract, "status"},
		{"a 503 that says ready", 503, "application/json", `{"status":"ready","checks":{"db":true}}`, Contract, "status"},
		{"ready with a check that is false", 200, "application/json", `{"status":"ready","checks":{"db":true,"singleton_lock":false}}`, Contract, "checks.singleton_lock"},
		{"a status word that is neither", 200, "application/json", `{"status":"degraded","checks":{"db":true}}`, Contract, "status"},
		{"no checks", 200, "application/json", `{"status":"ready"}`, Contract, "checks"},
		{"a check that is not a boolean", 200, "application/json", `{"status":"ready","checks":{"db":"yes"}}`, Contract, "checks.db"},
		{"reasons that are not strings", 503, "application/json", `{"status":"not_ready","checks":{"db":false},"reasons":[1]}`, Unavailable, ""},
		{"a 200 that is not JSON", 200, "application/json", `ready`, Contract, ""},
		{"a 200 served as text", 200, "text/plain", `{"status":"ready","checks":{"db":true}}`, Contract, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := serve(t, row.status, row.ctype, row.body)
			r, err := s.client(t, M2, tok, Options{}).Ready(ctxT(t, 5*time.Second))
			if r != nil {
				t.Fatalf("a Readiness came back with %v", err)
			}
			if e := wantKind(t, err, row.kind); e.Key != row.key || e.Route != "m2.readyz" {
				t.Fatalf("%+v", *e)
			}
		})
	}
	// the paired control: the well-formed documents of the same two statuses are answers
	for _, ok := range []struct {
		status int
		body   string
	}{{200, `{"status":"ready","checks":{"db":true,"singleton_lock":true}}`}, {503, `{"status":"not_ready","checks":{"db":false}}`}} {
		s := serve(t, ok.status, "application/json", ok.body)
		if r, err := s.client(t, M2, tok, Options{}).Ready(ctxT(t, 5*time.Second)); err != nil || r == nil {
			t.Fatalf("control failed: %v", err)
		}
	}
}

func TestDescribe_DecodesTheKeysTheTwoSendersShare(t *testing.T) {
	tok := mustToken(t)
	for _, tc := range []struct {
		module Module
		body   string
		name   string
		model  string
		unit   int64
		inert  bool
		codes  int
		states int
	}{
		{M2, describeM2, "send_managed", "flat_per_recipient", 1, false, 7, 7},
		{M3, describeM3, "send_byo", "flat_subscription", 0, true, 9, 6},
	} {
		t.Run(string(tc.module), func(t *testing.T) {
			s := serve(t, 200, "application/json; charset=utf-8", tc.body)
			d, err := s.client(t, tc.module, tok, Options{}).Describe(ctxT(t, 5*time.Second))
			if err != nil || d == nil {
				t.Fatalf("%v", err)
			}
			if d.Module != tc.name || d.APIVersion != "v1" || !d.Capabilities["single_send"] || d.Capabilities["sequences"] ||
				d.Pricing.Currency != "credits" || d.Pricing.Model != tc.model || d.Pricing.UnitPriceCredits != tc.unit || d.Pricing.CostFieldsInert != tc.inert ||
				len(d.Pricing.Models) != 3 || len(d.ErrorCodes) != tc.codes {
				t.Fatalf("%+v", d)
			}
			states, ok := d.Vocabularies.Values("job_state")
			if !ok || len(states) != tc.states {
				t.Fatalf("job_state = %v", states)
			}
			found := false
			for _, ec := range d.ErrorCodes {
				found = found || (ec.Code == "paused" && ec.HTTPStatus == 503)
			}
			if !found {
				t.Fatalf("error_codes %v lack paused/503", d.ErrorCodes)
			}
			if r := s.requests()[0]; r.path != "/v1/describe" || r.auth != "Bearer "+tok {
				t.Fatalf("describe needs the credential: %+v", r)
			}
		})
	}
}

func TestDescribe_AResponseThatBreaksTheShapeIsAContractError(t *testing.T) {
	tok := mustToken(t)
	rows := []struct {
		name string
		from string
		to   string
		key  string
	}{
		{"module missing", `"module":"send_managed",`, ``, "module"},
		{"api_version a number", `"api_version":"v1"`, `"api_version":1`, "api_version"},
		{"a capability that is not a boolean", `"single_send":true`, `"single_send":"yes"`, "capabilities.single_send"},
		{"pricing missing its currency", `"currency":"credits",`, ``, "pricing.currency"},
		{"a unit price that is a string", `"unit_price_credits":1,`, `"unit_price_credits":"1",`, "pricing.unit_price_credits"},
		{"a unit price that is a fraction", `"unit_price_credits":1,`, `"unit_price_credits":1.5,`, "pricing.unit_price_credits"},
		{"cost_fields_inert not a boolean", `"cost_fields_inert":false`, `"cost_fields_inert":0`, "pricing.cost_fields_inert"},
		{"a vocabulary whose values are not an array", `"values":["shared","dedicated"]`, `"values":"shared"`, "vocabularies.pool_kind.values"},
		{"a vocabulary value that is not a string", `"values":["shared","dedicated"]`, `"values":["shared",2]`, "vocabularies.pool_kind.values[1]"},
		{"error_codes with a status that is a string", `{"code":"validation_failed","http_status":400}`, `{"code":"validation_failed","http_status":"400"}`, "error_codes[0].http_status"},
		{"error_codes entry without a code", `{"code":"unauthenticated","http_status":401}`, `{"http_status":401}`, "error_codes[1].code"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if !strings.Contains(describeM2, row.from) {
				t.Fatalf("the fixture no longer contains %q", row.from)
			}
			s := serve(t, 200, "application/json", strings.Replace(describeM2, row.from, row.to, 1))
			d, err := s.client(t, M2, tok, Options{}).Describe(ctxT(t, 5*time.Second))
			if d != nil {
				t.Fatalf("a Describe came back with %v", err)
			}
			if e := wantKind(t, err, Contract); e.Key != row.key || e.Route != "m2.describe" {
				t.Fatalf("%+v", *e)
			}
		})
	}
	for _, body := range []string{`[]`, `"describe"`, `{}`, ``} {
		s := serve(t, 200, "application/json", body)
		d, err := s.client(t, M2, tok, Options{}).Describe(ctxT(t, 5*time.Second))
		if d != nil {
			t.Fatalf("a Describe came back for %q", body)
		}
		wantKind(t, err, Contract)
	}
	// the control: the unmodified fixture is decoded
	s := serve(t, 200, "application/json", describeM2)
	if d, err := s.client(t, M2, tok, Options{}).Describe(ctxT(t, 5*time.Second)); err != nil || d == nil {
		t.Fatalf("control failed: %v", err)
	}
	// a refused token and a missing route are the module's answers, not a shape problem
	for status, kind := range map[int]Kind{401: Refused, 404: NotFound, 500: Internal, 503: Unavailable} {
		s := serve(t, status, "application/json", m2Unauth)
		if d, err := s.client(t, M2, tok, Options{}).Describe(ctxT(t, 5*time.Second)); d != nil {
			t.Fatalf("status %d: a Describe came back", status)
		} else {
			wantKind(t, err, kind)
		}
	}
}

func TestDescribe_Module1HasNoneAndMakesNoNetworkCall(t *testing.T) {
	tok := mustToken(t)
	var calls atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(healthOK)), Request: r}, nil
	})
	c, err := New(M1, "http://module-one.test:8080", config.Secret(tok), &http.Client{Transport: rt}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The paired control: the same client DOES use its transport for an operation module 1 has.
	if h, err := c.Health(ctxT(t, 5*time.Second)); err != nil || h == nil || calls.Load() != 1 {
		t.Fatalf("control failed: %+v, %v, %d call(s)", h, err, calls.Load())
	}
	d, err := c.Describe(ctxT(t, 5*time.Second))
	if d != nil {
		t.Fatal("a Describe came back for a module that has none")
	}
	e := wantKind(t, err, NotSupported)
	if !errors.Is(err, ErrNotSupported) || e.Module != M1 || e.Route != "m1.describe" || e.Sent || e.Status != 0 {
		t.Fatalf("%+v", *e)
	}
	v, err := c.Vocabularies(ctxT(t, 5*time.Second))
	if v != nil {
		t.Fatalf("a Vocabularies came back for a module that has none: %v", v)
	}
	wantKind(t, err, NotSupported)
	if calls.Load() != 1 {
		t.Fatalf("describe and vocabularies of module 1 made %d network call(s)", calls.Load()-1)
	}
	// even with a cancelled context, and from a client that cannot reach anything, the answer is the same one
	dead, err := New(M1, "http://127.0.0.1:1", config.Secret(tok), transport(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.Describe(ctxT(t, time.Second)); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("an unreachable module 1 client answered %v", err)
	}
	// and modules 2 and 3 DO have one
	s := serve(t, 200, "application/json", describeM3)
	if _, err := s.client(t, M3, tok, Options{}).Describe(ctxT(t, 5*time.Second)); err != nil || s.count() != 1 {
		t.Fatalf("%v, %d hit(s)", err, s.count())
	}
}

func TestVocabularies_TheAdapterCanSeeDriftBeforeItMeetsAnUnknownState(t *testing.T) {
	tok := mustToken(t)
	m2 := serve(t, 200, "application/json", describeM2)
	v2, err := m2.client(t, M2, tok, Options{}).Vocabularies(ctxT(t, 5*time.Second))
	if err != nil || len(v2) != 6 {
		t.Fatalf("%v, %d vocabularies", err, len(v2))
	}
	expectedM2 := []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"}
	if d := v2.Diff("job_state", expectedM2); !d.Exact() || d.Breaking() || d.Name != "job_state" {
		t.Fatalf("module 2 against its own states: %+v", d)
	}
	reasons, _ := v2.Values("item_reason")
	if len(reasons) != 12 {
		t.Fatalf("item_reason = %v", reasons)
	}

	m3 := serve(t, 200, "application/json", describeM3)
	v3, err := m3.client(t, M3, tok, Options{}).Vocabularies(ctxT(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// module 3 has no `cancelled`: an adapter written for module 2's seven states sees what is missing, and that it breaks the contract
	d := v3.Diff("job_state", expectedM2)
	if d.Exact() || !d.Breaking() || len(d.Missing) != 1 || d.Missing[0] != "cancelled" || len(d.Extra) != 0 {
		t.Fatalf("module 3 against module 2's states: %+v", d)
	}
	// the other direction: a module that ADDS a state is not breaking, and the new state is named
	d = v2.Diff("job_state", expectedM2[:6])
	if d.Exact() || d.Breaking() || len(d.Extra) != 1 || d.Extra[0] != "cancelled" {
		t.Fatalf("an added state: %+v", d)
	}
	// a vocabulary the module does not publish is not advertised, and is breaking for an adapter that needs it
	if d := v3.Diff("item_reason", []string{"pool_exhausted"}); d.Advertised || !d.Breaking() || d.Exact() {
		t.Fatalf("an unpublished vocabulary: %+v", d)
	}
	// duplicates and order do not matter, and the lists are sorted
	if d := v2.Diff("pool_kind", []string{"dedicated", "shared", "shared", "z-new", "a-new"}); d.Breaking() != true || len(d.Missing) != 2 || d.Missing[0] != "a-new" || d.Missing[1] != "z-new" {
		t.Fatalf("%+v", d)
	}
	// a vocabulary entry without `values` is not a vocabulary
	s := serve(t, 200, "application/json", strings.Replace(describeM2, `"pool_kind":{"source":"db_enum","values":["shared","dedicated"],"fields":["pool.kind"]}`, `"pool_kind":{"source":"db_enum"}`, 1))
	v, err := s.client(t, M2, tok, Options{}).Vocabularies(ctxT(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Values("pool_kind"); ok || v.Diff("pool_kind", nil).Advertised {
		t.Fatal("an entry without values was taken for a vocabulary")
	}
	// an error gives no vocabulary at all, never a stale or built-in one
	bad := serve(t, 503, "application/json", m2Paused)
	none, err := bad.client(t, M2, tok, Options{}).Vocabularies(ctxT(t, 5*time.Second))
	if none != nil {
		t.Fatalf("a vocabulary came back with an error: %v", none)
	}
	wantKind(t, err, Paused)
}
