package m1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestSubmit_SendsTheItemsShapeWithTheBearerKey: POST /v1/verify/bulk with `Authorization: Bearer <key>`, a JSON body that is exactly {"items":[{item_id, address}...]} (no
// `addresses` key: module 1 refuses both), and the 202 (the recording, its counts set to what the plan predicts) decodes to the recorded job id and those counts. Control: the stand-in answers 401 to any
// other credential, so a client that sent the wrong header would be seen; and one with the wrong key is (a Refused error that is not an orphan: module 1 said no).
func TestSubmit_SendsTheItemsShapeWithTheBearerKey(t *testing.T) {
	var key string
	f := newFakeM1(t, func(c call) (int, string) {
		if c.Auth != "Bearer "+key {
			return 401, fixture(t, "submit-error-unauthenticated.json")
		}
		return 202, acceptedFor(t, threeRowPlan().Expected())
	})
	key = f.key
	a := f.adapter(t)
	plan := threeRowPlan()
	id, counts, err := a.Submit(testCtx(t), plan)
	if err != nil {
		t.Fatal(err)
	}
	if recID, _ := recordedCounts(t, "submit-accepted.json"); id != recID || id != testJob || counts != plan.Expected() || counts == (SubmitCounts{}) {
		t.Fatalf("Submit returned %q %+v, want the recorded job id %q and the plan's counts %+v", id, counts, testJob, plan.Expected())
	}
	calls := f.seen()
	if len(calls) != 1 {
		t.Fatalf("%d request(s), want 1", len(calls))
	}
	got := calls[0]
	if got.Method != http.MethodPost || got.Path != "/v1/verify/bulk" || got.Auth != "Bearer "+key || got.ContentType != "application/json" {
		t.Fatalf("the request was %+v", got)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["items"] == nil {
		t.Fatalf("the body has keys %v, want exactly [items]", body)
	}
	var sent []Item
	if err := json.Unmarshal(body["items"], &sent); err != nil || !reflect.DeepEqual(sent, plan.Items) {
		t.Fatalf("the items sent were %+v (%v), want %+v", sent, err, plan.Items)
	}

	// control: another key is refused by the stand-in, and the adapter reports the 401 by status, code and route, and it is NOT an orphan
	bad, err := New(f.srv.URL, config.Secret(newToken(t)), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = bad.Submit(testCtx(t), plan)
	e := asModclient(err)
	if e == nil || e.Kind != modclient.Refused || e.Status != 401 || e.Code != "unauthenticated" || e.Route != "m1.submit" || errors.Is(err, ErrOrphanPossible) {
		t.Fatalf("control: the wrong key gave %v, want a Refused 401 unauthenticated on m1.submit that is not an orphan", err)
	}
}

// TestSubmit_AnUnconfirmedOutcomeIsAnOrphanPossibilityAndIsNeverRetried: the bulk POST is not idempotent, so whatever the dials say it is sent ONCE (Retries is 2 here), and an
// outcome that does not confirm the 202 is an *OrphanError: errors.Is(err, ErrOrphanPossible), wrapping the modclient error (KindOf says which class). What did not leave, and what
// module 1 REFUSED in its own words, is not an orphan. Paired control: the same stand-in answering GET /v1/jobs/{id} the same way IS retried (three requests), so the dial is
// live and it is the adapter's choice that spares the POST.
func TestSubmit_AnUnconfirmedOutcomeIsAnOrphanPossibilityAndIsNeverRetried(t *testing.T) {
	type answer func(w http.ResponseWriter, r *http.Request)
	reply := func(status int, body string) answer {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	reset := func(w http.ResponseWriter, r *http.Request) { // the request was read and the connection dropped without an answer
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	cases := []struct {
		name   string
		answer answer
		kind   modclient.Kind
		orphan bool
	}{
		{"500 internal_error", reply(500, fixture(t, "submit-error-internal.json")), modclient.Internal, true},
		{"503 unavailable", reply(503, fixture(t, "submit-error-unavailable.json")), modclient.Unavailable, true},
		{"502 from a gateway", reply(502, `<html>bad gateway</html>`), modclient.Unavailable, true},
		{"504 from a gateway", reply(504, ``), modclient.Unavailable, true},
		{"a reset connection", reset, modclient.Unavailable, true},
		{"a 200 where the route answers 202", reply(200, fixture(t, "submit-accepted.json")), modclient.Contract, true},
		{"a 202 that is not JSON", reply(202, `accepted`), modclient.Contract, true},
		{"a 202 with a job id that is not a uuid", reply(202, mustReplace(t, fixture(t, "submit-accepted.json"), testJob, "J-42")), modclient.Contract, true},
		{"a 202 that does not say accepted", reply(202, mustReplace(t, fixture(t, "submit-accepted.json"), `"accepted"`, `"queued"`)), modclient.Contract, true},
		{"a 202 with no job id", reply(202, `{"status":"accepted","counts":{"total":3,"enqueued":2,"duplicate":1,"malformed":0}}`), modclient.Contract, true},

		{"400 validation_failed (module 1 refused)", reply(400, fixture(t, "submit-error-validation.json")), modclient.Refused, false},
		{"401 unauthenticated", reply(401, fixture(t, "submit-error-unauthenticated.json")), modclient.Refused, false},
		{"413 payload_too_large", reply(413, fixture(t, "submit-error-too-large.json")), modclient.Refused, false},
		{"503 paused (module 1 says retry unchanged later)", reply(503, fixture(t, "submit-error-paused.json")), modclient.Paused, false},
		{"404 (a wrong base URL)", reply(404, `<html>`), modclient.NotFound, false},
		{"429", reply(429, `{"error":{"code":"rate_limited","message":"x","request_id":"r"}}`), modclient.RateLimited, false},
	}
	for _, c := range cases {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			c.answer(w, r)
		}))
		a, err := New(srv.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
		if err != nil {
			t.Fatal(err)
		}
		id, counts, err := a.Submit(testCtx(t), threeRowPlan())
		srv.Close()
		if err == nil || id != "" || counts != (SubmitCounts{}) {
			t.Errorf("%s: got %q %+v %v, want an error and no value", c.name, id, counts, err)
			continue
		}
		if got := modclient.KindOf(err); got != c.kind {
			t.Errorf("%s: kind %v, want %v (%v)", c.name, got, c.kind, err)
		}
		if got := errors.Is(err, ErrOrphanPossible); got != c.orphan {
			t.Errorf("%s: errors.Is(ErrOrphanPossible) is %t, want %t (%v)", c.name, got, c.orphan, err)
		}
		var oe *OrphanError
		if errors.As(err, &oe) != c.orphan {
			t.Errorf("%s: errors.As(*OrphanError) is %t, want %t", c.name, !c.orphan, c.orphan)
		}
		if n.Load() != 1 {
			t.Errorf("%s: the POST was sent %d time(s), want exactly 1 (it is not idempotent: no retry may repeat it)", c.name, n.Load())
		}
	}

	// the stalled module: the caller's deadline passes after the request was sent
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // read the request first, so the server notices the client hanging up and the handler ends with it
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer stall.Close()
	sa, err := New(stall.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = sa.Submit(ctxFor(t, 200*time.Millisecond), threeRowPlan())
	if modclient.KindOf(err) != modclient.Timeout || !errors.Is(err, ErrOrphanPossible) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a stalled module: got %v, want a Timeout that is an orphan possibility and unwraps to DeadlineExceeded", err)
	}

	// the request never left: nothing can have been created
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	da, err := New(deadURL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := da.Submit(testCtx(t), threeRowPlan()); modclient.KindOf(err) != modclient.Unavailable || errors.Is(err, ErrOrphanPossible) {
		t.Errorf("a refused connection: got %v, want Unavailable that is NOT an orphan possibility (the request never left)", err)
	}

	// control: a GET with the same 503 is retried to the dial's limit
	var gets atomic.Int32
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(fixture(t, "submit-error-unavailable.json")))
	}))
	defer rs.Close()
	ra, err := New(rs.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ra.Status(testCtx(t), testJob); modclient.KindOf(err) != modclient.Unavailable || gets.Load() != 3 {
		t.Fatalf("control: Status against a 503 made %d request(s) (%v), want 3 (1 + 2 retries)", gets.Load(), err)
	}
}

// TestSubmit_ACountThatDisagreesWithThePlanIsAContractError: module 1 confirmed the job but its counts are not what the plan predicted (for example it judged a row malformed that
// norm/v1 accepted: the two implementations have drifted), or they do not add up, or one is negative. Submit returns no value and a *MismatchError that carries the job id (the job
// EXISTS), is a Contract error, names the counts key, and is NOT an orphan. The recording itself is one of the cases: it is the 202 of a raw submission that held two malformed rows,
// which a plan never sends, so a plan predicting none of them disagrees with it. Control: the recording with its counts set to the prediction is accepted.
func TestSubmit_ACountThatDisagreesWithThePlanIsAContractError(t *testing.T) {
	body := func(total, enq, dup, mal int) string {
		b, _ := json.Marshal(map[string]any{"job_id": testJob, "status": "accepted", "counts": map[string]int{"total": total, "enqueued": enq, "duplicate": dup, "malformed": mal}})
		return string(b)
	}
	cases := []struct {
		name, body, key string
	}{
		{"one more distinct address than predicted", body(3, 3, 0, 0), "counts.enqueued"},
		{"no duplicate where one was predicted", body(3, 2, 0, 1), "counts.duplicate"},
		{"a row judged malformed that norm accepted", body(4, 2, 1, 1), "counts.total"},
		{"counts that do not add up", body(3, 2, 1, 1), "counts.total"},
		{"a negative count", body(3, 2, 1, -1), "counts.malformed"},
	}
	for _, c := range cases {
		f := serve(t, 202, c.body)
		id, counts, err := f.adapter(t).Submit(testCtx(t), threeRowPlan())
		var me *MismatchError
		kind, key := kindAndKey(err)
		if !errors.As(err, &me) || me.JobID != testJob || kind != modclient.Contract || key != c.key || id != "" || counts != (SubmitCounts{}) || errors.Is(err, ErrOrphanPossible) {
			t.Errorf("%s: got %q %+v kind %v key %q (%v), want no value and a MismatchError for job %s, a Contract error naming %q", c.name, id, counts, kind, key, err, testJob, c.key)
		}
	}
	// the recording as it is: its counts are whatever the real run produced, and the plan's prediction must differ from them or this case proves nothing
	_, rec := recordedCounts(t, "submit-accepted.json")
	want := threeRowPlan().Expected()
	wantKey := ""
	for _, k := range []struct {
		name      string
		got, want int
	}{{"counts.total", rec.Total, want.Total}, {"counts.enqueued", rec.Enqueued, want.Enqueued}, {"counts.duplicate", rec.Duplicate, want.Duplicate}, {"counts.malformed", rec.Malformed, want.Malformed}} {
		if k.got != k.want {
			wantKey = k.name
			break
		}
	}
	if wantKey == "" {
		t.Fatalf("the recorded counts %+v equal the plan's prediction %+v: the case would prove nothing", rec, want)
	}
	_, _, rerr := serve(t, 202, fixture(t, "submit-accepted.json")).adapter(t).Submit(testCtx(t), threeRowPlan())
	var me *MismatchError
	if kind, key := kindAndKey(rerr); !errors.As(rerr, &me) || me.JobID != testJob || kind != modclient.Contract || key != wantKey {
		t.Errorf("the recorded 202: got kind %v key %q (%v), want a MismatchError naming %q", kind, key, rerr, wantKey)
	}
	f := serve(t, 202, acceptedFor(t, want))
	if _, _, err := f.adapter(t).Submit(testCtx(t), threeRowPlan()); err != nil {
		t.Fatalf("control: the recording with the plan's counts was refused: %v", err)
	}
}

// TestSubmitRaw_AppliesModule1sLimitsAndChecksNothingAgainstAPrediction: the unfiltered path refuses the same unusable labels, and an empty list, before any request, and returns
// the 202's counts as the module reports them. Control: a good list goes.
func TestSubmitRaw_AppliesModule1sLimitsAndChecksNothingAgainstAPrediction(t *testing.T) {
	f := serve(t, 202, fixture(t, "submit-accepted.json"))
	a := f.adapter(t)
	for name, items := range map[string][]Item{
		"none":      nil,
		"blank":     {{"", "ada@acme.example"}},
		"too long":  {{strings.Repeat("r", 201), "ada@acme.example"}},
		"repeated":  {{"a", "ada@acme.example"}, {"a", "bob@acme.example"}},
		"a newline": {{"a\nb", "ada@acme.example"}},
	} {
		if _, _, err := a.SubmitRaw(testCtx(t), items); modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("%s: got %v, want an Invalid error", name, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for refused lists", f.count())
	}
	id, counts, err := a.SubmitRaw(testCtx(t), []Item{{"a", "ada@acme.example"}})
	recID, recCounts := recordedCounts(t, "submit-accepted.json")
	if err != nil || id != recID || counts != recCounts || counts.Total == 1 {
		t.Fatalf("control: %q %+v %v, want the recording's %q %+v (the counts are the module's, not compared with the one row sent: it says %d rows)", id, counts, err, recID, recCounts, recCounts.Total)
	}
}

// TestOrphanError_IsATypedWrapper: the OrphanError text names the possibility and the modclient error; errors.Is matches the sentinel and the kind sentinel of the wrapped error
// (modclient.ErrTimeout); a plain modclient error is neither.
func TestOrphanError_IsATypedWrapper(t *testing.T) {
	inner := &modclient.Error{Kind: modclient.Timeout, Module: modclient.M1, Route: routeSubmit, Sent: true, Message: "x"}
	var err error = &OrphanError{Err: inner}
	if !errors.Is(err, ErrOrphanPossible) || !errors.Is(err, modclient.ErrTimeout) || errors.Is(err, modclient.ErrUnavailable) {
		t.Fatal("the OrphanError must be ErrOrphanPossible and the kind of what it wraps, and no other kind")
	}
	if !strings.Contains(err.Error(), "orphan job is possible") || !strings.Contains(err.Error(), routeSubmit) {
		t.Fatalf("the text does not say what happened: %q", err)
	}
	var plain error = inner
	if errors.Is(plain, ErrOrphanPossible) {
		t.Fatal("control: a plain modclient error claims an orphan")
	}
}

// mustReplace replaces the one occurrence of old in s and fails the test when there is not exactly one: a mutation that touched nothing proves nothing.
func mustReplace(t testing.TB, s, old, new string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("setup: %q occurs %d time(s) in the fixture, want exactly 1", old, n)
	}
	return strings.Replace(s, old, new, 1)
}
