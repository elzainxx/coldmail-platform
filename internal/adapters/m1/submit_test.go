package m1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// unconfirmedPhrase is what the message of a submission whose outcome is not known must say.
const unconfirmedPhrase = "same Idempotency-Key may be sent again"

// TestSubmit_SendsTheItemsShapeWithTheBearerKey: POST /v1/verify/bulk with `Authorization: Bearer <key>`, the caller's `Idempotency-Key`, a JSON body that is exactly {"items":[{item_id,
// address}...]} (no `addresses` key: module 1 refuses both), and the 202 (the recording, its counts set to what the plan predicts) decodes to the recorded job id and those counts, not
// replayed. Control: the stand-in answers 401 to any other credential, so a client that sent the wrong header would be seen; and one with the wrong key is a Refused error that is not
// an unconfirmed outcome (module 1 said no, and said so in its own words).
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
	idem := newIdemKey(t)
	res, err := a.Submit(testCtx(t), plan, idem)
	if err != nil {
		t.Fatal(err)
	}
	if recID, _ := recordedCounts(t, "submit-accepted.json"); res.JobID != recID || res.JobID != testJob || res.Counts != plan.Expected() || res.Counts == (SubmitCounts{}) || res.Replayed {
		t.Fatalf("Submit returned %+v, want the recorded job id %q, the plan's counts %+v and no replay", res, testJob, plan.Expected())
	}
	calls := f.seen()
	if len(calls) != 1 {
		t.Fatalf("%d request(s), want 1", len(calls))
	}
	got := calls[0]
	if got.Method != http.MethodPost || got.Path != "/v1/verify/bulk" || got.Auth != "Bearer "+key || got.ContentType != "application/json" || got.IdemKey != idem {
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

	// control: another key is refused by the stand-in, and the adapter reports the 401 by status, code and route; it is not an unconfirmed outcome
	bad, err := New(f.srv.URL, config.Secret(newToken(t)), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := bad.Submit(testCtx(t), plan, idem)
	e := asModclient(err)
	if e == nil || e.Kind != modclient.Refused || e.Status != 401 || e.Code != "unauthenticated" || e.Route != "m1.submit" || strings.Contains(err.Error(), unconfirmedPhrase) || r != (SubmitResult{}) {
		t.Fatalf("control: the wrong key gave %+v %v, want a Refused 401 unauthenticated on m1.submit that is not an unconfirmed outcome, and no value", r, err)
	}
}

// TestSubmit_TheIdempotencyKeyIsTheHeaderAndNowhereElse: the key the caller gives is sent as the Idempotency-Key request header and in no other place: not the body, the path, the query,
// the credential or any other header, and never in an error message. Control: the same scan finds it in the one header it is in (so the scan can fail), and a failing submission's
// error text is scanned too.
func TestSubmit_TheIdempotencyKeyIsTheHeaderAndNowhereElse(t *testing.T) {
	idem := newIdemKey(t)
	f := serve(t, 202, acceptedFor(t, threeRowPlan().Expected()))
	if _, err := f.adapter(t).Submit(testCtx(t), threeRowPlan(), idem); err != nil {
		t.Fatal(err)
	}
	c := f.seen()[0]
	if c.IdemKey != idem || len(c.Header.Values("Idempotency-Key")) != 1 {
		t.Fatalf("the header is %v, want the key exactly once", c.Header.Values("Idempotency-Key"))
	}
	elsewhere := []string{c.Path, c.Query, string(c.Body), c.Auth, c.ContentType}
	for name, vs := range c.Header {
		if name != "Idempotency-Key" {
			elsewhere = append(elsewhere, vs...)
		}
	}
	if leaks(strings.Join(elsewhere, "\n"), idem) {
		t.Fatalf("the idempotency key is somewhere other than its header: %q", elsewhere)
	}
	if !leaks(strings.Join(c.Header.Values("Idempotency-Key"), ""), idem) {
		t.Fatal("control: the scan does not see the key in the header it is in")
	}
	// and never in an error
	g := serve(t, 500, fixture(t, "submit-error-internal.json"))
	_, err := g.adapter(t).Submit(testCtx(t), threeRowPlan(), idem)
	if err == nil || leaks(err.Error(), idem) {
		t.Fatalf("the error of a failed submission carries the key: %v", err)
	}
}

// TestSubmit_AnUnconfirmedOutcomeIsRetriedWithTheSameKeyAndIsATypedErrorThatSaysSo: the bulk POST carries its key and so is idempotent for modclient: after a failure that leaves the
// outcome unknown and that the client retries (503 unavailable, 502, 504, a reset connection: Retries is 2 here, so three attempts) EVERY attempt carries the same key; a failure
// that is not retried (a 500, a 2xx that is not the documented 202) is sent once. When the retries are exhausted the error is the typed one of the last attempt (Unavailable,
// Timeout, Internal, Contract) and its message says the same key may be sent again; there is no error type of its own for it any more. What did not leave, and what module 1 REFUSED in its own words or
// said is paused, is no unconfirmed outcome and does not say so. Paired controls: the same stand-in answering GET /v1/jobs/{id} the same way is retried too (so the dial is live), and
// a POST that is NOT idempotent (the synchronous verification, which is billed) is still sent once under the same dials.
func TestSubmit_AnUnconfirmedOutcomeIsRetriedWithTheSameKeyAndIsATypedErrorThatSaysSo(t *testing.T) {
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
		name        string
		answer      answer
		kind        modclient.Kind
		attempts    int  // how many times the one POST is sent under Retries: 2
		unconfirmed bool // the message says the same key may be sent again
	}{
		{"500 internal_error", reply(500, fixture(t, "submit-error-internal.json")), modclient.Internal, 1, true},
		{"503 unavailable", reply(503, fixture(t, "submit-error-unavailable.json")), modclient.Unavailable, 3, true},
		{"502 from a gateway", reply(502, `<html>bad gateway</html>`), modclient.Unavailable, 3, true},
		{"504 from a gateway", reply(504, ``), modclient.Unavailable, 3, true},
		{"a reset connection", reset, modclient.Unavailable, 3, true},
		{"a 200 where the route answers 202", reply(200, fixture(t, "submit-accepted.json")), modclient.Contract, 1, true},
		{"a 202 that is not JSON", reply(202, `accepted`), modclient.Contract, 1, true},
		{"a 202 with a job id that is not a uuid", reply(202, mustReplace(t, fixture(t, "submit-accepted.json"), testJob, "J-42")), modclient.Contract, 1, true},
		{"a 202 that does not say accepted", reply(202, mustReplace(t, fixture(t, "submit-accepted.json"), `"accepted"`, `"queued"`)), modclient.Contract, 1, true},
		{"a 202 with no job id", reply(202, `{"status":"accepted","counts":{"total":3,"enqueued":2,"duplicate":1,"malformed":0}}`), modclient.Contract, 1, true},

		{"400 validation_failed (module 1 refused)", reply(400, fixture(t, "submit-error-validation.json")), modclient.Refused, 1, false},
		{"401 unauthenticated", reply(401, fixture(t, "submit-error-unauthenticated.json")), modclient.Refused, 1, false},
		{"413 payload_too_large", reply(413, fixture(t, "submit-error-too-large.json")), modclient.Refused, 1, false},
		{"503 paused (module 1 says retry unchanged later)", reply(503, fixture(t, "submit-error-paused.json")), modclient.Paused, 1, false},
		{"404 (a wrong base URL)", reply(404, `<html>`), modclient.NotFound, 1, false},
		{"429", reply(429, `{"error":{"code":"rate_limited","message":"x","request_id":"r"}}`), modclient.RateLimited, 3, false},
	}
	for _, c := range cases {
		idem := newIdemKey(t)
		var n atomic.Int32
		var mu sync.Mutex
		var keys []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			c.answer(w, r)
		}))
		a, err := New(srv.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
		if err != nil {
			t.Fatal(err)
		}
		res, err := a.Submit(testCtx(t), threeRowPlan(), idem)
		srv.Close()
		if err == nil || res != (SubmitResult{}) {
			t.Errorf("%s: got %+v %v, want an error and no value", c.name, res, err)
			continue
		}
		if got := modclient.KindOf(err); got != c.kind {
			t.Errorf("%s: kind %v, want %v (%v)", c.name, got, c.kind, err)
		}
		if got := strings.Contains(err.Error(), unconfirmedPhrase); got != c.unconfirmed {
			t.Errorf("%s: the message says the key may be sent again: %t, want %t (%v)", c.name, got, c.unconfirmed, err)
		}
		if n.Load() != int32(c.attempts) {
			t.Errorf("%s: the POST was sent %d time(s), want %d", c.name, n.Load(), c.attempts)
		}
		mu.Lock()
		for i, k := range keys {
			if k != idem {
				t.Errorf("%s: attempt %d carried the key %q, want the same key as the first", c.name, i+1, k)
			}
		}
		mu.Unlock()
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
	_, err = sa.Submit(ctxFor(t, 200*time.Millisecond), threeRowPlan(), newIdemKey(t))
	if modclient.KindOf(err) != modclient.Timeout || !strings.Contains(err.Error(), unconfirmedPhrase) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a stalled module: got %v, want a Timeout that says the key may be sent again and unwraps to DeadlineExceeded", err)
	}

	// the request never left: nothing can have been created, and the message does not pretend it is unconfirmed
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	da, err := New(deadURL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	_, err = da.Submit(testCtx(t), threeRowPlan(), newIdemKey(t))
	if e := asModclient(err); e == nil || e.Kind != modclient.Unavailable || e.Sent || strings.Contains(err.Error(), unconfirmedPhrase) {
		t.Errorf("a refused connection: got %v, want Unavailable that never sent and does not call itself unconfirmed", err)
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
	// control: the synchronous verification is a POST that cannot be repeated (it is billed), and under the very same dials it is sent once
	gets.Store(0)
	if _, err := ra.Verify(testCtx(t), "ada@acme.example"); modclient.KindOf(err) != modclient.Unavailable || gets.Load() != 1 {
		t.Fatalf("control: Verify against a 503 made %d request(s) (%v), want exactly 1: only a request that carries a key is retried", gets.Load(), err)
	}
}

// TestSubmit_ARetryAfterAResetSendsTheSameKeyAndEndsWithTheReplayedJob: module 1 creates the job and the answer is lost (the connection is dropped): the client sends the request
// again, with the SAME key, and module 1 answers with the job it already made, marked as a replay. Submit returns that job, Replayed true, one job in the module, two attempts with one
// key. Paired controls: a module that fails BEFORE it creates anything (a 503) is retried to a first answer (Replayed false, one job); the very same loss with no retries is an
// unconfirmed error, the job EXISTS, and sending the same key again by hand returns it (a replay, still one job); and a different key for the same plan makes a second job (the key is
// what ties the attempts together).
func TestSubmit_ARetryAfterAResetSendsTheSameKeyAndEndsWithTheReplayedJob(t *testing.T) {
	plan := threeRowPlan()
	idem := newIdemKey(t)
	m := newIdemModule(t)
	m.dropAfterCreate = 1
	res, err := m.adapter(t, modclient.Options{Retries: 2}).Submit(testCtx(t), plan, idem)
	if err != nil {
		t.Fatalf("a lost answer was not recovered by the retry: %v", err)
	}
	if res.JobID != "00000000-0000-4000-8000-000000000001" || res.Counts != plan.Expected() || !res.Replayed {
		t.Fatalf("got %+v, want the job the first attempt made, the plan's counts, and Replayed", res)
	}
	if keys := m.attemptKeys(); !reflect.DeepEqual(keys, []string{idem, idem}) || m.jobCount() != 1 {
		t.Fatalf("keys %v, %d job(s): want the same key twice and ONE job", keys, m.jobCount())
	}

	// control: the first attempt fails before anything is created
	b := newIdemModule(t)
	b.beforeCreate = []int{503}
	res, err = b.adapter(t, modclient.Options{Retries: 2}).Submit(testCtx(t), plan, idem)
	if err != nil || res.Replayed || res.JobID != "00000000-0000-4000-8000-000000000001" || b.attemptCount() != 2 || b.jobCount() != 1 {
		t.Fatalf("control: %+v %v after %d attempt(s), %d job(s): want a first answer (not a replay) from the second attempt", res, err, b.attemptCount(), b.jobCount())
	}

	// control: no retries: the answer is lost, the error says the key may be sent again, and sending it again is the replay
	c := newIdemModule(t)
	c.dropAfterCreate = 1
	ca := c.adapter(t, modclient.Options{})
	if r, err := ca.Submit(testCtx(t), plan, idem); err == nil || r != (SubmitResult{}) || modclient.KindOf(err) != modclient.Unavailable || !strings.Contains(err.Error(), unconfirmedPhrase) {
		t.Fatalf("control: a lost answer with no retries gave %+v %v, want an Unavailable error that says the key may be sent again and no value", r, err)
	}
	if c.jobCount() != 1 {
		t.Fatalf("control: the module holds %d job(s) after the lost answer, want the one it made", c.jobCount())
	}
	again, err := ca.Submit(testCtx(t), plan, idem)
	if err != nil || !again.Replayed || again.JobID != "00000000-0000-4000-8000-000000000001" || c.jobCount() != 1 {
		t.Fatalf("control: the same key again gave %+v %v, %d job(s): want the original job as a replay", again, err, c.jobCount())
	}
	other, err := ca.Submit(testCtx(t), plan, newIdemKey(t))
	if err != nil || other.Replayed || other.JobID == again.JobID || c.jobCount() != 2 {
		t.Fatalf("control: another key gave %+v %v, %d job(s): want a second job", other, err, c.jobCount())
	}
}

// TestSubmit_TheSameKeyTwiceIsTheSameJobAndTheSecondIsReplayed: two Submit calls with the same key and the same plan return the same job id and the same counts; the first is not marked
// replayed and the second is, and the module holds ONE job. Control: a second key for the same plan is a second job and is not a replay.
func TestSubmit_TheSameKeyTwiceIsTheSameJobAndTheSecondIsReplayed(t *testing.T) {
	m := newIdemModule(t)
	a := m.adapter(t, modclient.Options{})
	plan := threeRowPlan()
	idem := newIdemKey(t)
	first, err := a.Submit(testCtx(t), plan, idem)
	if err != nil || first.Replayed {
		t.Fatalf("the first answer: %+v %v, want a job that is not a replay", first, err)
	}
	second, err := a.Submit(testCtx(t), plan, idem)
	if err != nil || !second.Replayed || second.JobID != first.JobID || second.Counts != first.Counts || m.jobCount() != 1 || m.attemptCount() != 2 {
		t.Fatalf("the second answer: %+v %v (%d job(s), %d attempt(s)), want the same job and counts as a replay, one job", second, err, m.jobCount(), m.attemptCount())
	}
	third, err := a.Submit(testCtx(t), plan, newIdemKey(t))
	if err != nil || third.Replayed || third.JobID == first.JobID || m.jobCount() != 2 {
		t.Fatalf("control: another key gave %+v %v (%d job(s)), want a second job that is not a replay", third, err, m.jobCount())
	}
	// a replay is a first answer to the caller: it is still checked against the plan
	if second.Counts != plan.Expected() {
		t.Fatalf("the replay's counts %+v are not the plan's %+v", second.Counts, plan.Expected())
	}
}

// TestSubmit_TheSameKeyWithAnotherRequestIsRefusedAndNothingIsCreated: module 1 answers 409 `idempotency_key_reused` and creates nothing; Submit returns no value and an error that is
// Refused, carries the module's code, status and its Idempotency-Key detail, and for which errors.Is(err, ErrIdempotencyKeyReused) is true; it is sent once (a refusal is not retried).
// Controls: the same key with the same request is a replay and no error; a 409 with ANOTHER code is Refused but is not ErrIdempotencyKeyReused; a 400 for a malformed key header is
// Refused and is not it either; and an ordinary Refused error is not it.
func TestSubmit_TheSameKeyWithAnotherRequestIsRefusedAndNothingIsCreated(t *testing.T) {
	m := newIdemModule(t)
	a := m.adapter(t, modclient.Options{Retries: 2})
	idem := newIdemKey(t)
	if _, err := a.Submit(testCtx(t), threeRowPlan(), idem); err != nil {
		t.Fatal(err)
	}
	before := m.attemptCount()
	res, err := a.Submit(testCtx(t), otherPlan(), idem)
	e := asModclient(err)
	if res != (SubmitResult{}) || e == nil || e.Kind != modclient.Refused || e.Status != 409 || e.Code != "idempotency_key_reused" || e.Route != "m1.submit" ||
		!errors.Is(err, ErrIdempotencyKeyReused) || !errors.Is(err, modclient.ErrRefused) || !e.Enveloped {
		t.Fatalf("got %+v %v, want a Refused 409 idempotency_key_reused on m1.submit that is ErrIdempotencyKeyReused and no value", res, err)
	}
	if len(e.Details) != 1 || e.Details[0] != (modclient.Detail{Field: "Idempotency-Key", Issue: "conflict"}) {
		t.Fatalf("the details are %+v, want the Idempotency-Key conflict", e.Details)
	}
	if m.jobCount() != 1 || m.attemptCount() != before+1 {
		t.Fatalf("%d job(s) and %d attempt(s) after the refused request (it was %d), want ONE job and exactly one more attempt", m.jobCount(), m.attemptCount(), before)
	}
	if !strings.Contains(err.Error(), "nothing was created") || leaks(err.Error(), idem) {
		t.Fatalf("the error %q must say nothing was created and never quote the key", err)
	}

	// control: the same key with the same request is no error
	if r, err := a.Submit(testCtx(t), threeRowPlan(), idem); err != nil || !r.Replayed {
		t.Fatalf("control: the same request again gave %+v %v, want a replay", r, err)
	}
	// controls: other 4xx answers are Refused and are not the reused key
	for name, body := range map[string]struct {
		status int
		body   string
	}{
		"a 409 with another code":          {409, `{"error":{"code":"conflict","message":"x","request_id":"r"}}`},
		"a 400 for a malformed key header": {400, `{"error":{"code":"validation_failed","message":"x","details":[{"field":"Idempotency-Key","issue":"malformed"}],"request_id":"r"}}`},
		"a 401":                            {401, fixture(t, "submit-error-unauthenticated.json")},
	} {
		_, err := serve(t, body.status, body.body).adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t))
		if modclient.KindOf(err) != modclient.Refused || errors.Is(err, ErrIdempotencyKeyReused) {
			t.Errorf("%s: got %v, want a Refused error that is not ErrIdempotencyKeyReused", name, err)
		}
	}
	// the code on another status is data, not the reused key
	if _, err := serve(t, 400, `{"error":{"code":"idempotency_key_reused","message":"x","request_id":"r"}}`).adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t)); errors.Is(err, ErrIdempotencyKeyReused) {
		t.Errorf("a 400 that carries the code was read as a reused key: %v", err)
	}
}

// TestSubmit_ABadKeyIsRefusedBeforeAnyRequest: a key that is empty, has a space, a character outside [A-Za-z0-9._:-], a non-ASCII letter, a line break, or is 129 characters long is an
// Invalid error made before any request, by Submit and by SubmitRaw, with no value, and the message never quotes the key. Control: keys of 1 and of 128 characters and one that uses
// every allowed character class are sent, verbatim.
func TestSubmit_ABadKeyIsRefusedBeforeAnyRequest(t *testing.T) {
	f := serve(t, 202, acceptedFor(t, threeRowPlan().Expected()))
	a := f.adapter(t)
	probe := "Zq9probe"
	bad := map[string]string{
		"empty": "", "a space": "job " + probe, "a slash": "job/" + probe, "a plus": "job+" + probe, "an equals sign": "job=" + probe, "a comma": "job," + probe, "a semicolon": probe + ";",
		"not ASCII": probe + "é", "a newline": probe + "\nX-Injected: 1", "a tab": probe + "\t", "129 characters": strings.Repeat("k", 129), "a quote": probe + `"`, "a NUL": probe + "\x00",
	}
	for name, key := range bad {
		if ValidIdempotencyKey(key) {
			t.Fatalf("setup: %s is a valid key, so the case proves nothing", name)
		}
		res, err := a.Submit(testCtx(t), threeRowPlan(), key)
		if e := asModclient(err); e == nil || e.Kind != modclient.Invalid || e.Route != "m1.submit" || res != (SubmitResult{}) || leaks(err.Error(), probe) {
			t.Errorf("Submit with %s: got %+v %v, want an Invalid error that quotes nothing, and no value", name, res, err)
		}
		raw, err := a.SubmitRaw(testCtx(t), []Item{{"a", "ada@acme.example"}}, key)
		if e := asModclient(err); e == nil || e.Kind != modclient.Invalid || raw != (SubmitResult{}) {
			t.Errorf("SubmitRaw with %s: got %+v %v, want an Invalid error and no value", name, raw, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for keys module 1 would refuse", f.count())
	}
	for _, key := range []string{"k", strings.Repeat("k", 128), "AZaz09._:-", "run.42:job-7_a"} {
		if !ValidIdempotencyKey(key) {
			t.Fatalf("setup: %q is refused", clip(key))
		}
		if _, err := a.Submit(testCtx(t), threeRowPlan(), key); err != nil {
			t.Errorf("control: the key %q was refused: %v", clip(key), err)
		}
		if got := f.seen()[f.count()-1].IdemKey; got != key {
			t.Errorf("control: the key %q went out as %q", clip(key), clip(got))
		}
	}
}

// TestValidIdempotencyKey_IsModule1sPattern: ValidIdempotencyKey agrees with the regular expression module 1 publishes (IdempotencyKeyPattern, `idempotency.key_pattern` of its describe) on
// a corpus that holds both sides of every boundary, and the corpus has members of both answers. Control: the regexp itself is checked on known members.
func TestValidIdempotencyKey_IsModule1sPattern(t *testing.T) {
	re := regexp.MustCompile(IdempotencyKeyPattern)
	if !re.MatchString("a") || re.MatchString("") || re.MatchString(strings.Repeat("a", 129)) || !re.MatchString(strings.Repeat("a", 128)) {
		t.Fatal("control: the pattern constant is not the 1..128 rule")
	}
	corpus := []string{"", "a", "A", "z", "0", "9", ".", "_", ":", "-", "..", "a-b", "a_b", "a:b", "a.b", " ", "a b", "a/b", "a\\b", "a@b", "a#b", "a~b", "é", "ab\n", "ab\r\n", "\x00", "\x7f",
		strings.Repeat("a", 127), strings.Repeat("a", 128), strings.Repeat("a", 129), strings.Repeat("-", 128), strings.Repeat("é", 64), "k" + strings.Repeat(".", 127), "k" + strings.Repeat(".", 128)}
	for c := 0; c < 256; c++ { // every single byte
		corpus = append(corpus, string([]byte{byte(c)}), "k"+string([]byte{byte(c)}))
	}
	yes, no := 0, 0
	for _, k := range corpus {
		want := re.MatchString(k)
		if got := ValidIdempotencyKey(k); got != want {
			t.Errorf("%q: ValidIdempotencyKey is %t, the pattern says %t", clip(k), got, want)
		}
		if want {
			yes++
		} else {
			no++
		}
	}
	if yes < 20 || no < 20 {
		t.Fatalf("the corpus holds %d valid and %d invalid keys", yes, no)
	}
}

// TestSubmit_AReplayMarkerThatIsNotTrueIsAContractError: module 1 marks a replay with `Idempotent-Replayed: true` and a first answer carries no such header. A header with another value
// is the module contradicting its own contract: a Contract error naming the header (that says the key may be sent again) and no value. Control: no header is a first answer, "true" a replay.
func TestSubmit_AReplayMarkerThatIsNotTrueIsAContractError(t *testing.T) {
	body := acceptedFor(t, threeRowPlan().Expected())
	run := func(value string) (SubmitResult, error) {
		f := newFakeM1H(t, func(call) (int, string, http.Header) {
			if value == "" {
				return 202, body, nil
			}
			return 202, body, http.Header{"Idempotent-Replayed": {value}}
		})
		return f.adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t))
	}
	if r, err := run(""); err != nil || r.Replayed || r.JobID != testJob {
		t.Fatalf("control: no marker gave %+v %v, want a first answer", r, err)
	}
	if r, err := run("true"); err != nil || !r.Replayed || r.JobID != testJob {
		t.Fatalf("control: the marker true gave %+v %v, want a replay", r, err)
	}
	for _, v := range []string{"false", "1", "True", "yes", "TRUE"} {
		r, err := run(v)
		kind, key := kindAndKey(err)
		if r != (SubmitResult{}) || kind != modclient.Contract || key != "Idempotent-Replayed" || !strings.Contains(err.Error(), unconfirmedPhrase) {
			t.Errorf("the marker %q: got %+v kind %v key %q (%v), want no value and a Contract error naming the header", v, r, kind, key, err)
		}
	}
}

// TestSubmit_ACountThatDisagreesWithThePlanIsAContractError: module 1 confirmed the job but its counts are not what the plan predicted (for example it judged a row malformed that
// norm/v1 accepted: the two implementations have drifted), or they do not add up, or one is negative. Submit returns no value and a *MismatchError that carries the job id (the job
// EXISTS), is a Contract error, names the counts key, and does not say the outcome is unconfirmed (it is confirmed). The recording itself is one of the cases: it is the 202 of a raw
// submission that held two malformed rows, which a plan never sends, so a plan predicting none of them disagrees with it. A REPLAY with such counts is judged the same way. Control: the
// recording with its counts set to the prediction is accepted.
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
		res, err := f.adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t))
		var me *MismatchError
		kind, key := kindAndKey(err)
		if !errors.As(err, &me) || me.JobID != testJob || kind != modclient.Contract || key != c.key || res != (SubmitResult{}) || strings.Contains(err.Error(), unconfirmedPhrase) {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no value and a MismatchError for job %s, a Contract error naming %q", c.name, res, kind, key, err, testJob, c.key)
		}
	}
	// the same disagreement in a REPLAY
	g := newFakeM1H(t, func(call) (int, string, http.Header) {
		return 202, body(3, 3, 0, 0), http.Header{"Idempotent-Replayed": {"true"}}
	})
	var me *MismatchError
	if res, err := g.adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t)); !errors.As(err, &me) || me.JobID != testJob || res != (SubmitResult{}) {
		t.Errorf("a replay whose counts disagree: got %+v %v, want a MismatchError", res, err)
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
	_, rerr := serve(t, 202, fixture(t, "submit-accepted.json")).adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t))
	if kind, key := kindAndKey(rerr); !errors.As(rerr, &me) || me.JobID != testJob || kind != modclient.Contract || key != wantKey {
		t.Errorf("the recorded 202: got kind %v key %q (%v), want a MismatchError naming %q", kind, key, rerr, wantKey)
	}
	f := serve(t, 202, acceptedFor(t, want))
	if _, err := f.adapter(t).Submit(testCtx(t), threeRowPlan(), newIdemKey(t)); err != nil {
		t.Fatalf("control: the recording with the plan's counts was refused: %v", err)
	}
}

// TestSubmitRaw_AppliesModule1sLimitsAndChecksNothingAgainstAPrediction: the unfiltered path refuses the same unusable labels, and an empty list, before any request, and returns
// the 202's counts as the module reports them, under the caller's key. Control: a good list goes.
func TestSubmitRaw_AppliesModule1sLimitsAndChecksNothingAgainstAPrediction(t *testing.T) {
	f := serve(t, 202, fixture(t, "submit-accepted.json"))
	a := f.adapter(t)
	idem := newIdemKey(t)
	for name, items := range map[string][]Item{
		"none":      nil,
		"blank":     {{"", "ada@acme.example"}},
		"too long":  {{strings.Repeat("r", 201), "ada@acme.example"}},
		"repeated":  {{"a", "ada@acme.example"}, {"a", "bob@acme.example"}},
		"a newline": {{"a\nb", "ada@acme.example"}},
	} {
		if _, err := a.SubmitRaw(testCtx(t), items, idem); modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("%s: got %v, want an Invalid error", name, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for refused lists", f.count())
	}
	res, err := a.SubmitRaw(testCtx(t), []Item{{"a", "ada@acme.example"}}, idem)
	recID, recCounts := recordedCounts(t, "submit-accepted.json")
	if err != nil || res.JobID != recID || res.Counts != recCounts || res.Counts.Total == 1 || res.Replayed || f.seen()[0].IdemKey != idem {
		t.Fatalf("control: %+v %v, want the recording's %q %+v under the key (the counts are the module's, not compared with the one row sent: it says %d rows)", res, err, recID, recCounts, recCounts.Total)
	}
}

// TestUnconfirmed_SaysTheKeyMayBeSentAgainAndChangesNothingElse: the helper that words an unconfirmed outcome adds the sentence to an error whose request was sent and got no usable
// answer (Timeout, Unavailable, Internal, Contract, Canceled), leaves every other field as it was (kind, status, code, cause, attempts), does not touch the error it was given, and
// returns an error that did not leave, or that module 1 answered in its own words (Refused, NotFound, RateLimited, Paused), unchanged. A plain error is returned as it is. And reusedKey
// makes ONLY a 409 with the code idempotency_key_reused findable.
func TestUnconfirmed_SaysTheKeyMayBeSentAgainAndChangesNothingElse(t *testing.T) {
	for _, k := range []modclient.Kind{modclient.Timeout, modclient.Unavailable, modclient.Internal, modclient.Contract, modclient.Canceled} {
		in := &modclient.Error{Kind: k, Module: modclient.M1, Route: routeSubmit, Status: 503, Code: "unavailable", Message: "x", Sent: true, Attempts: 3, Cause: context.DeadlineExceeded}
		out := asModclient(unconfirmed(in))
		if out == nil || out == in || out.Kind != k || out.Status != 503 || out.Code != "unavailable" || out.Attempts != 3 || !errors.Is(out, context.DeadlineExceeded) || !strings.Contains(out.Message, unconfirmedPhrase) {
			t.Errorf("kind %v: got %+v", k, out)
		}
		if in.Message != "x" {
			t.Errorf("kind %v: the error that was given was modified: %q", k, in.Message)
		}
	}
	for _, k := range []modclient.Kind{modclient.Refused, modclient.NotFound, modclient.RateLimited, modclient.Paused, modclient.Invalid} {
		in := &modclient.Error{Kind: k, Route: routeSubmit, Message: "x", Sent: true}
		if out := unconfirmed(in); out != error(in) {
			t.Errorf("kind %v: an answer of the module's own was changed: %v", k, out)
		}
	}
	notSent := &modclient.Error{Kind: modclient.Unavailable, Route: routeSubmit, Message: "x"}
	if out := unconfirmed(notSent); out != error(notSent) {
		t.Errorf("a request that never left was called unconfirmed: %v", out)
	}
	plain := errors.New("plain")
	if unconfirmed(plain) != plain || reusedKey(plain) != plain {
		t.Error("a plain error was changed")
	}
	reused := &modclient.Error{Kind: modclient.Refused, Status: 409, Code: "idempotency_key_reused", Message: "x"}
	if out := reusedKey(reused); !errors.Is(out, ErrIdempotencyKeyReused) || reused.Cause != nil {
		t.Errorf("a 409 idempotency_key_reused: %v (the original's cause %v)", out, reused.Cause)
	}
	for name, in := range map[string]*modclient.Error{
		"another code":   {Kind: modclient.Refused, Status: 409, Code: "conflict"},
		"another status": {Kind: modclient.Refused, Status: 400, Code: "idempotency_key_reused"},
		"another kind":   {Kind: modclient.Contract, Status: 409, Code: "idempotency_key_reused"},
	} {
		if out := reusedKey(in); errors.Is(out, ErrIdempotencyKeyReused) {
			t.Errorf("%s: read as a reused key", name)
		}
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
