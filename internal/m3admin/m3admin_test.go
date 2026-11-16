package m3admin

// Unit tests of the client against the in-process module 3 of fake_test.go. Every refusal is paired with a
// control that proves the same setup would have succeeded had the refused property held, so a test that
// "passes" because the client never got that far fails instead.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// ---- suspend ------------------------------------------------------------------------------------------------------

func TestSuspend_200_ChangesState(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "suspend a known, active user", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
	// Non-vacuity: the call really reached the route and module 3 really changed state.
	if !f.isSuspended(userA) {
		t.Fatal("the fake's state did not change: the request did not reach the suspend route")
	}
	if n := f.count(); n != 1 {
		t.Fatalf("%d requests, want exactly 1", n)
	}
}

func TestSuspend_422Conflict_IsAlreadySuspended(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	// userB is already suspended: module 3 answers 422 with the conflict detail, and that is a
	// confirmation of the state, not a failure.
	res, err := c.SetSuspended(testCtx(t), userB, true, "")
	wantResult(t, "suspend an already suspended user", res, err, Result{State: StateSuspended, Changed: false, HTTPStatus: 422})
	if !f.isSuspended(userB) || f.count() != 1 {
		t.Fatalf("suspended=%v requests=%d, want the user still suspended after exactly 1 request", f.isSuspended(userB), f.count())
	}

	// Paired control: for a user who is not suspended the same call changes state, so Changed is what
	// tells the two answers apart, and a client that ignored the status could not pass both.
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: first suspend", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: second suspend of the same user", res, err, Result{State: StateSuspended, Changed: false, HTTPStatus: 422})

	// The exact documented 422 body, written out rather than built by the fake.
	const documented = `{"error":{"code":"unprocessable","message":"This user is already suspended.","details":[{"field":"suspended","issue":"conflict"}],"request_id":"req_0123456789abcdef0123456789abcdef"}}`
	f.setForce(always(&reply{status: 422, body: documented}))
	res, err = c.SetSuspended(testCtx(t), userC, true, "")
	wantResult(t, "the documented 422 body", res, err, Result{State: StateSuspended, Changed: false, HTTPStatus: 422})
}

func TestSuspend_404_UnknownUser(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	res, err := c.SetSuspended(testCtx(t), userC, true, "")
	wantFailure(t, "suspend an unknown user", res, err, "unknown-user")
	if f.count() != 1 {
		t.Fatalf("%d requests, want 1: a 404 is not retried inside the client", f.count())
	}
	if err != nil && !strings.Contains(err.Error(), reqID) {
		t.Errorf("the error does not carry module 3's request id %s: %v", reqID, err)
	}

	// Control: the same client and route for a user module 3 knows.
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: a known user", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})

	// A 404 that is NOT module 3's own not_found envelope says nothing about the user (the wrong base URL,
	// a gateway), and concluding "no such user, nothing to suspend" from it would be the unsafe direction.
	notEnvelope := map[string]*reply{
		"an empty 404":                  {status: 404},
		"an html 404":                   {status: 404, body: "<html><body>404</body></html>", header: map[string]string{"Content-Type": "text/html"}},
		"json that is not an error":     {status: 404, body: `{"message":"not found"}`},
		"an envelope with another code": {status: 404, body: envBody("conflict", "x")},
		"an empty object":               {status: 404, body: `{}`},
	}
	for name, rep := range notEnvelope {
		f.setForce(always(rep))
		res, err = c.SetSuspended(testCtx(t), userC, true, "")
		wantFailure(t, name, res, err, "retryable")
		if statusOf(err) != 404 {
			t.Errorf("%s: carried status %d, want 404", name, statusOf(err))
		}
	}

	// The same envelope answering an unsuspend POST is the same verdict.
	f.setForce(always(notFound()))
	res, err = c.SetSuspended(testCtx(t), userC, false, "")
	wantFailure(t, "a not_found answer to the unsuspend POST", res, err, "unknown-user")
}

func TestSuspend_401_Unauthorized(t *testing.T) {
	f := newFake(t)
	wrong, err := New(f.srv.URL, config.Secret("zzzz-wrong-key-zzzz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	both(func(suspended bool, op string) {
		res, err := wrong.SetSuspended(testCtx(t), userA, suspended, "")
		wantFailure(t, op+" with a wrong key", res, err, "unauthorized")
	})
	if f.count() != 2 {
		t.Fatalf("%d requests for 2 calls: a 401 must never be retried inside the client", f.count())
	}
	if f.isSuspended(userA) {
		t.Fatal("a refused call changed state")
	}

	// Paired control: the right key against the same fake is accepted, so the 401 was about the key.
	right := f.newClient(t, nil)
	res, err := right.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: the right key", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})

	// A 401 with no usable body is still a refused key.
	for name, rep := range map[string]*reply{
		"an empty 401": {status: 401},
		"an html 401":  {status: 401, body: "<html>login</html>", header: map[string]string{"Content-Type": "text/html"}},
	} {
		f.setForce(always(rep))
		both(func(suspended bool, op string) {
			res, err := right.SetSuspended(testCtx(t), userB, suspended, "")
			wantFailure(t, name+" to "+op, res, err, "unauthorized")
		})
	}
}

func TestSuspend_400_Invalid(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	f.setForce(always(envReply(400, "validation_failed", "One or more fields are invalid.", detail{"body", "malformed"})))
	both(func(suspended bool, op string) {
		res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
		wantFailure(t, op+" answered 400", res, err, "invalid")
		if err != nil && !strings.Contains(err.Error(), "validation_failed") {
			t.Errorf("%s: the module's error code is not in the error: %v", op, err)
		}
	})
	if f.count() != 2 {
		t.Fatalf("%d requests for 2 calls, want 2 (a 400 is not retried)", f.count())
	}
	// Control: without the forced 400 the same calls succeed.
	f.setForce(nil)
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: no forced 400", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
}

func TestSuspend_5xxAnd429_Retryable(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	cases := []struct {
		status int
		rep    *reply
	}{
		{500, envReply(500, "internal_error", "An unexpected error occurred.")},
		{500, &reply{status: 500}},
		{502, &reply{status: 502, body: "<html>bad gateway</html>", header: map[string]string{"Content-Type": "text/html"}}},
		{503, &reply{status: 503, body: "upstream down"}},
		{504, &reply{status: 504}},
		{599, &reply{status: 599}},
		{429, envReply(429, "rate_limited", "Too many requests.")},
		{429, &reply{status: 429}},
	}
	for _, tc := range cases {
		f.setForce(always(tc.rep))
		both(func(suspended bool, op string) {
			label := fmt.Sprintf("%s answered %d", op, tc.status)
			res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
			wantFailure(t, label, res, err, "retryable")
			if statusOf(err) != tc.status {
				t.Errorf("%s: the error carries status %d", label, statusOf(err))
			}
		})
	}
	if f.isSuspended(userA) {
		t.Fatal("a failed call changed state in the fake")
	}
	// Control: the same calls succeed once the fault is gone.
	f.setForce(nil)
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: no fault", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
}

func TestSuspend_NetworkError_Retryable(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	// Control: while the fake is up the call works.
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: the server is up", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})

	f.srv.Close()
	both(func(suspended bool, op string) {
		res, err := c.SetSuspended(testCtx(t), userB, suspended, "")
		wantFailure(t, op+" against a closed server", res, err, "retryable")
		var re *RetryableError
		if !errors.As(err, &re) {
			return
		}
		if re.HTTPStatus != 0 || re.Cause == nil {
			t.Errorf("%s: status %d cause %v, want status 0 and a cause", op, re.HTTPStatus, re.Cause)
		}
		if !strings.Contains(re.Reason, "connection refused") {
			t.Errorf("%s: reason %q does not name the class", op, re.Reason)
		}
		// The text of net/http's error carries the URL; the class is all the error may say.
		if strings.Contains(err.Error(), f.srv.URL) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: the error text carries the address: %v", op, err)
		}
	})
}

func TestSuspend_Timeout_Retryable(t *testing.T) {
	f := newFake(t)
	short := &http.Client{Timeout: 150 * time.Millisecond}
	c := f.newClient(t, short)

	both(func(suspended bool, op string) {
		f.setForce(always(&reply{status: 200, body: okBody(userA, suspended), delay: time.Minute}))
		res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
		wantFailure(t, op+" that outlasts the client timeout", res, err, "retryable")
		var re *RetryableError
		if !errors.As(err, &re) {
			return
		}
		if re.HTTPStatus != 0 || re.Cause == nil || !strings.Contains(re.Reason, "timeout") {
			t.Errorf("%s: status %d cause %v reason %q, want status 0, a cause and a timeout", op, re.HTTPStatus, re.Cause, re.Reason)
		}
		if strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), f.srv.URL) {
			t.Errorf("%s: the error text carries net/http's wording or the URL: %v", op, err)
		}

		// Paired control: the same client with the same short timeout and a prompt reply succeeds, so the
		// delay and not the client is what failed.
		f.setForce(always(&reply{status: 200, body: okBody(userA, suspended)}))
		res, err = c.SetSuspended(testCtx(t), userA, suspended, "")
		wantResult(t, op+" control: a prompt reply", res, err, Result{State: stateFor(suspended), Changed: true, HTTPStatus: 200})
	})
}

func TestSuspend_200Disagreement_NeverAResult(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	if strings.ToUpper(userB) == userB {
		t.Fatal("test setup: the fixture id has no letters, so upper-casing it proves nothing")
	}
	cases := []struct {
		name      string
		id        string // the user the call is made for
		suspended bool
		body      string
	}{
		{"suspend answered suspended:false", userA, true, okBody(userA, false)},
		{"unsuspend answered suspended:true", userA, false, okBody(userA, true)},
		{"suspend answered for another user", userA, true, okBody(userB, true)},
		{"unsuspend answered for another user", userA, false, okBody(userB, false)},
		{"the user id echoed in another case", userB, true, okBody(strings.ToUpper(userB), true)},
		{"the user id echoed with a suffix", userA, false, okBody(userA+"0", false)},
	}
	for _, tc := range cases {
		f.setForce(always(&reply{status: 200, body: tc.body}))
		res, err := c.SetSuspended(testCtx(t), tc.id, tc.suspended, "")
		wantFailure(t, tc.name, res, err, "retryable")
		if statusOf(err) != 200 {
			t.Errorf("%s: carried status %d, want 200", tc.name, statusOf(err))
		}
	}
	// Controls: the agreeing body for each direction is a Result.
	both(func(suspended bool, op string) {
		f.setForce(always(&reply{status: 200, body: okBody(userA, suspended)}))
		res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
		wantResult(t, op+" control: an agreeing 200", res, err, Result{State: stateFor(suspended), Changed: true, HTTPStatus: 200})
	})
}

func TestSuspend_200GarbageBody_Retryable(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	garbage := map[string]string{
		"empty":                   ``,
		"whitespace":              " \n",
		"plain text":              `ok`,
		"html":                    `<html><body>OK</body></html>`,
		"a json array":            `[]`,
		"json null":               `null`,
		"a json string":           `"suspended"`,
		"an empty object":         `{}`,
		"suspended missing":       `{"user_id":"{ID}"}`,
		"user_id missing":         `{"suspended":{S}}`,
		"suspended is a string":   `{"user_id":"{ID}","suspended":"{S}"}`,
		"suspended is null":       `{"user_id":"{ID}","suspended":null}`,
		"suspended is a number":   `{"user_id":"{ID}","suspended":1}`,
		"user_id is a number":     `{"user_id":5,"suspended":{S}}`,
		"user_id is null":         `{"user_id":null,"suspended":{S}}`,
		"truncated":               `{"user_id":"{ID}","suspended":tr`,
		"trailing text":           `{"user_id":"{ID}","suspended":{S}} ok`,
		"two objects":             `{"user_id":"{ID}","suspended":{S}}{"user_id":"{ID}","suspended":{S}}`,
		"an object inside a list": `[{"user_id":"{ID}","suspended":{S}}]`,
		"the shape of an error":   envBody("unprocessable", "x"),
	}
	fill := func(form string, suspended bool) string {
		return strings.NewReplacer("{ID}", userA, "{S}", fmt.Sprint(suspended)).Replace(form)
	}
	for name, form := range garbage {
		both(func(suspended bool, op string) {
			f.setForce(always(&reply{status: 200, body: fill(form, suspended), header: map[string]string{"Content-Type": "text/plain"}}))
			res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
			wantFailure(t, op+" answered 200 with "+name, res, err, "retryable")
			if statusOf(err) != 200 {
				t.Errorf("%s/%s: carried status %d, want 200", op, name, statusOf(err))
			}
		})
	}
	// Controls: the minimal documented shape, and the documented shape plus fields this client does not
	// know, are both accepted, so what is refused above is the shape and not strictness about extras.
	for name, form := range map[string]string{
		"the documented shape":        `{"user_id":"{ID}","suspended":{S}}`,
		"the shape with extra fields": `{"user_id":"{ID}","suspended":{S},"reason":"operator","extra":[1,2]}`,
		"leading and trailing space":  "  {\"user_id\":\"{ID}\",\"suspended\":{S}}\n",
	} {
		both(func(suspended bool, op string) {
			f.setForce(always(&reply{status: 200, body: fill(form, suspended)}))
			res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
			wantResult(t, op+" control: "+name, res, err, Result{State: stateFor(suspended), Changed: true, HTTPStatus: 200})
		})
	}
}

func TestSuspend_422WithoutConflictDetail_IsAnError(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	notConflict := map[string]string{
		"no details":            envBody("unprocessable", "The request is well-formed but cannot be processed."),
		"an empty details list": `{"error":{"code":"unprocessable","message":"x","details":[],"request_id":"` + reqID + `"}}`,
		"another field":         envBody("unprocessable", "x", detail{"reason", "conflict"}),
		"another issue":         envBody("unprocessable", "x", detail{"suspended", "malformed"}),
		"details outside error": `{"details":[{"field":"suspended","issue":"conflict"}]}`,
		"details not a list":    `{"error":{"code":"unprocessable","details":"suspended conflict"}}`,
		"the message only":      `{"error":{"code":"unprocessable","message":"This user is already suspended."}}`,
		"plain text":            `This user is already suspended.`,
		"empty":                 ``,
		"json null":             `null`,
	}
	for name, body := range notConflict {
		f.setForce(always(&reply{status: 422, body: body}))
		both(func(suspended bool, op string) {
			res, err := c.SetSuspended(testCtx(t), userB, suspended, "")
			wantFailure(t, op+" answered 422 with "+name, res, err, "retryable")
			if statusOf(err) != 422 {
				t.Errorf("%s/%s: carried status %d, want 422", op, name, statusOf(err))
			}
		})
	}
	// Control: a 422 whose details include the conflict (among others) is accepted as "already in that
	// state". For unsuspend that is followed by the existence probe, which the model answers 200 for userB.
	conflictAmongOthers := envBody("unprocessable", "x", detail{"reason", "malformed"}, detail{"suspended", "conflict"})
	f.setForce(onPost(&reply{status: 422, body: conflictAmongOthers}))
	both(func(suspended bool, op string) {
		res, err := c.SetSuspended(testCtx(t), userB, suspended, "")
		wantResult(t, op+" control: the conflict among other details", res, err, Result{State: stateFor(suspended), Changed: false, HTTPStatus: 422})
	})
}

// ---- unsuspend ----------------------------------------------------------------------------------------------------

func TestUnsuspend_200_ChangesState(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	res, err := c.SetSuspended(testCtx(t), userB, false, "")
	wantResult(t, "unsuspend a suspended user", res, err, Result{State: StateActive, Changed: true, HTTPStatus: 200})
	if f.isSuspended(userB) {
		t.Fatal("the fake's state did not change: the request did not reach the unsuspend route")
	}
	if n := f.count(); n != 1 {
		t.Fatalf("%d requests, want exactly 1: a 200 needs no existence probe", n)
	}

	// Paired control: the same call for a user who is no longer suspended is NOT a change.
	res, err = c.SetSuspended(testCtx(t), userB, false, "")
	wantResult(t, "control: unsuspend again", res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})
}

func TestUnsuspend_422_ConfigOK_ActiveUnchanged(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	res, err := c.SetSuspended(testCtx(t), userA, false, "")
	wantResult(t, "unsuspend a known user that is not suspended", res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2 (the unsuspend POST, then the existence probe): %+v", len(reqs), reqs)
	}
	if reqs[0].Method != http.MethodPost || !strings.HasSuffix(reqs[0].Path, "/unsuspend") ||
		reqs[1].Method != http.MethodGet || reqs[1].Path != "/v1/admin/users/"+userA+"/config" {
		t.Fatalf("the two requests were %s %s then %s %s", reqs[0].Method, reqs[0].Path, reqs[1].Method, reqs[1].Path)
	}

	// The exact documented 422 body, then the probe answering 200.
	const documented = `{"error":{"code":"unprocessable","message":"This user is not suspended.","details":[{"field":"suspended","issue":"conflict"}],"request_id":"req_0123456789abcdef0123456789abcdef"}}`
	f.reset()
	f.setForce(func(s seen) *reply {
		if s.Method == http.MethodPost {
			return &reply{status: 422, body: documented}
		}
		return &reply{status: 200, body: `{}`}
	})
	res, err = c.SetSuspended(testCtx(t), userC, false, "")
	wantResult(t, "the documented 422 body, probe 200", res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})
	if f.count() != 2 {
		t.Fatalf("%d requests, want 2", f.count())
	}
}

func TestUnsuspend_422_Config404_UnknownUser(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	// Non-vacuity of the trap itself: module 3's unsuspend answers the SAME 422 for a user that exists
	// and is not suspended and for one that does not exist. Only the existence probe can tell them apart.
	stKnown, bodyKnown := rawDo(t, f, http.MethodPost, "/v1/admin/users/"+userA+"/unsuspend")
	stUnknown, bodyUnknown := rawDo(t, f, http.MethodPost, "/v1/admin/users/"+userC+"/unsuspend")
	if stKnown != 422 || stUnknown != 422 || bodyKnown != bodyUnknown {
		t.Fatalf("the fake does not model the trap: known %d %q, unknown %d %q", stKnown, bodyKnown, stUnknown, bodyUnknown)
	}
	f.reset()

	res, err := c.SetSuspended(testCtx(t), userC, false, "")
	wantFailure(t, "unsuspend an unknown user", res, err, "unknown-user")
	if f.count() != 2 {
		t.Fatalf("%d requests, want 2 (the POST and the probe)", f.count())
	}

	// Paired control: the identical 422 for a user that exists is confirmed as active.
	f.reset()
	res, err = c.SetSuspended(testCtx(t), userA, false, "")
	wantResult(t, "control: a known user answers the same 422", res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})

	// A probe 404 that is not module 3's own envelope does not prove the user is unknown.
	f.setForce(onGet(&reply{status: 404, body: "<html>404</html>", header: map[string]string{"Content-Type": "text/html"}}))
	res, err = c.SetSuspended(testCtx(t), userA, false, "")
	wantFailure(t, "a bare 404 from the probe", res, err, "retryable")
}

func TestUnsuspend_422_ConfigFailure_Retryable(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	cases := []struct {
		name  string
		rep   *reply
		class string
		code  int
	}{
		{"probe 500", envReply(500, "internal_error", "An unexpected error occurred."), "retryable", 500},
		{"probe 503 html", &reply{status: 503, body: "<html>unavailable</html>"}, "retryable", 503},
		{"probe 429", envReply(429, "rate_limited", "Too many requests."), "retryable", 429},
		{"probe closes the connection", &reply{hangup: true}, "retryable", 0},
		{"probe 401", envReply(401, "unauthenticated", "Missing or invalid admin API key."), "unauthorized", -1},
		{"probe 400", envReply(400, "validation_failed", "One or more fields are invalid."), "invalid", -1},
		{"probe 302", &reply{status: 302, header: map[string]string{"Location": "/elsewhere"}}, "retryable", 302},
		{"probe 200 is the control for all of these", nil, "", 0},
	}
	for _, tc := range cases {
		f.reset()
		f.setForce(nil)
		if tc.rep != nil {
			f.setForce(onGet(tc.rep))
		}
		res, err := c.SetSuspended(testCtx(t), userA, false, "")
		if tc.rep == nil {
			wantResult(t, tc.name, res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})
		} else {
			wantFailure(t, tc.name, res, err, tc.class)
			if tc.class == "retryable" && statusOf(err) != tc.code {
				t.Errorf("%s: carried status %d, want %d", tc.name, statusOf(err), tc.code)
			}
		}
		if f.count() != 2 {
			t.Errorf("%s: %d requests, want 2 (the POST and one probe, never a second probe)", tc.name, f.count())
		}
	}
}

func TestUnsuspend_422WithoutConflictDetail_NoConfigProbe(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	f.setForce(onPost(envReply(422, "unprocessable", "The request is well-formed but cannot be processed.")))
	res, err := c.SetSuspended(testCtx(t), userA, false, "")
	wantFailure(t, "unsuspend answered 422 without the conflict detail", res, err, "retryable")
	if n := f.count(); n != 1 {
		t.Fatalf("%d requests, want 1: a 422 that is not the conflict is not confirmed by a probe", n)
	}

	// Paired control: with the conflict detail the probe IS made.
	f.reset()
	f.setForce(onPost(conflict("This user is not suspended.")))
	res, err = c.SetSuspended(testCtx(t), userA, false, "")
	wantResult(t, "control: the conflict detail", res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})
	if n := f.count(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

// ---- request shape ------------------------------------------------------------------------------------------------

func TestRequestShape_Suspend(t *testing.T) {
	// No reason: the request has no body at all.
	f := newFake(t)
	c := f.newClient(t, nil)
	if _, err := c.SetSuspended(testCtx(t), userA, true, ""); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/v1/admin/users/"+userA+"/suspend" || r.Query != "" {
		t.Errorf("request line = %s %s ?%s", r.Method, r.Path, r.Query)
	}
	if r.Auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want exactly the bearer form of the admin key", r.Auth)
	}
	if r.ContentType != "application/json" {
		t.Errorf("Content-Type = %q", r.ContentType)
	}
	if len(r.Body) != 0 {
		t.Errorf("body = %q, want none when no reason is given", r.Body)
	}

	// With a reason: the body is {"reason": <reason>} and nothing else, whatever the characters.
	reasons := []string{
		"operator hold",
		`quotes " and \ and <tag> & unicode é ✓`,
		strings.Repeat("a", MaxReasonBytes),
		strings.Repeat("é", MaxReasonBytes/2),
	}
	for _, reason := range reasons {
		f := newFake(t)
		c := f.newClient(t, nil)
		res, err := c.SetSuspended(testCtx(t), userA, true, reason)
		wantResult(t, "suspend with a reason", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
		reqs := f.requests()
		if len(reqs) != 1 {
			t.Fatalf("%d requests, want 1", len(reqs))
		}
		checkReasonBody(t, reqs[0], reason)
	}
}

// checkReasonBody asserts the request body is exactly one JSON object with one key, "reason", holding
// reason, and that module 3's strict decoder (unknown fields refused) would take it.
func checkReasonBody(t *testing.T, r seen, reason string) {
	t.Helper()
	if r.ContentType != "application/json" {
		t.Errorf("Content-Type = %q", r.ContentType)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &keys); err != nil {
		t.Errorf("the body is not a JSON object: %v", err)
		return
	}
	if len(keys) != 1 || keys["reason"] == nil {
		t.Errorf("the body's keys are not exactly [reason]: %s", r.Body)
	}
	var strict struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&strict); err != nil {
		t.Errorf("module 3's strict decode refuses the body: %v", err)
	}
	if strict.Reason != reason {
		t.Errorf("the reason arrived as %q, want %q", strict.Reason, reason)
	}
}

func TestRequestShape_Unsuspend(t *testing.T) {
	// The 200 path: one request, the same shape, with a reason.
	f := newFake(t)
	c := f.newClient(t, nil)
	res, err := c.SetSuspended(testCtx(t), userB, false, "paid in full")
	wantResult(t, "unsuspend with a reason", res, err, Result{State: StateActive, Changed: true, HTTPStatus: 200})
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests on the 200 path, want exactly 1", len(reqs))
	}
	if reqs[0].Method != http.MethodPost || reqs[0].Path != "/v1/admin/users/"+userB+"/unsuspend" ||
		reqs[0].Auth != "Bearer "+testToken || reqs[0].Query != "" {
		t.Errorf("request = %+v", reqs[0])
	}
	checkReasonBody(t, reqs[0], "paid in full")

	// The 422 path: two requests, the second the existence probe, authenticated the same way and with no body.
	f = newFake(t)
	c = f.newClient(t, nil)
	res, err = c.SetSuspended(testCtx(t), userA, false, "")
	wantResult(t, "unsuspend a user that is not suspended", res, err, Result{State: StateActive, Changed: false, HTTPStatus: 422})
	reqs = f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests on the 422 path, want exactly 2", len(reqs))
	}
	if len(reqs[0].Body) != 0 {
		t.Errorf("the POST without a reason has a body: %q", reqs[0].Body)
	}
	probe := reqs[1]
	if probe.Method != http.MethodGet || probe.Path != "/v1/admin/users/"+userA+"/config" || probe.Query != "" {
		t.Errorf("probe = %s %s ?%s", probe.Method, probe.Path, probe.Query)
	}
	if probe.Auth != "Bearer "+testToken || probe.Auth != reqs[0].Auth {
		t.Errorf("probe Authorization = %q, POST Authorization = %q", probe.Auth, reqs[0].Auth)
	}
	if len(probe.Body) != 0 || probe.ContentType != "" {
		t.Errorf("the probe has a body %q or a Content-Type %q", probe.Body, probe.ContentType)
	}
}

func TestBadUserIDs_RefusedBeforeAnyRequest(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	upper := strings.ToUpper(userB)
	if upper == userB {
		t.Fatal("test setup: the fixture id has no letters, so upper-casing it proves nothing")
	}
	ids := map[string]string{
		"upper case":               upper,
		"mixed case":               "66666666-7777-4888-9999-aaaaAAAAaaaa",
		"empty":                    "",
		"the all-zero system user": "00000000-0000-0000-0000-000000000000",
		"no hyphens":               strings.ReplaceAll(userA, "-", ""),
		"one character short":      userA[:35],
		"one character long":       userA + "0",
		"a non-hex digit":          "g" + userA[1:],
		"a misplaced hyphen":       "1111111-12222-4333-8444-555555555555",
		"in braces":                "{" + userA + "}",
		"a urn prefix":             "urn:uuid:" + userA,
		"a leading space":          " " + userA[1:],
		"a trailing space":         userA[:35] + " ",
		"a trailing newline":       userA[:35] + "\n",
		"a path traversal":         "../config/../../x",
		"a slash inside":           userA[:8] + "/" + userA[9:],
		"a query character":        userA[:35] + "?",
		"a nul byte":               userA[:35] + "\x00",
		"full-width digits":        strings.Repeat("１", 8) + userA[8:],
		"a word":                   "not-a-uuid",
	}
	for name, id := range ids {
		both(func(suspended bool, op string) {
			res, err := c.SetSuspended(testCtx(t), id, suspended, "")
			wantFailure(t, op+" with "+name, res, err, "invalid")
		})
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d requests were made for ids that must be refused before any request: %+v", n, f.requests())
	}

	// Paired control: the canonical id is sent, so the zero above is the validation and not a client that
	// cannot reach the server.
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: the canonical id", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
	if n := f.count(); n != 1 {
		t.Fatalf("%d requests after the control, want 1", n)
	}
}

func TestBadReasons_RefusedBeforeAnyRequest(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	reasons := map[string]string{
		"2001 ASCII bytes":                 strings.Repeat("a", MaxReasonBytes+1),
		"1001 two-byte runes (2002 bytes)": strings.Repeat("é", MaxReasonBytes/2+1),
		"a newline":                        "line one\nline two",
		"a carriage return":                "a\rb",
		"a tab":                            "a\tb",
		"a nul byte":                       "a\x00b",
		"an escape character":              "a\x1bb",
		"a delete character":               "a\x7fb",
		"a C1 control character (U+0085)":  "a\u0085b",
		"invalid UTF-8":                    "a\xff\xfeb",
		"a truncated multi-byte sequence":  "caf\xc3",
	}
	for name, reason := range reasons {
		both(func(suspended bool, op string) {
			res, err := c.SetSuspended(testCtx(t), userA, suspended, reason)
			wantFailure(t, op+" with "+name, res, err, "invalid")
		})
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d requests were made for reasons that must be refused before any request", n)
	}

	// Paired controls: the limit is in BYTES, so exactly 2000 bytes is accepted whether it is 2000 ASCII
	// characters or 1000 two-byte ones, and it arrives intact.
	for name, reason := range map[string]string{
		"2000 ASCII bytes":                     strings.Repeat("a", MaxReasonBytes),
		"1000 two-byte runes (2000 bytes)":     strings.Repeat("é", MaxReasonBytes/2),
		"spaces and punctuation, not controls": "a b, c; d: e - f (g)",
	} {
		f := newFake(t)
		c := f.newClient(t, nil)
		res, err := c.SetSuspended(testCtx(t), userA, true, reason)
		wantResult(t, "control: "+name, res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
		reqs := f.requests()
		if len(reqs) != 1 {
			t.Fatalf("control %s: %d requests, want 1", name, len(reqs))
		}
		checkReasonBody(t, reqs[0], reason)
	}
}

// ---- construction ------------------------------------------------------------------------------------------------

func TestNew_ValidatesItsInputs(t *testing.T) {
	badURLs := map[string]string{
		"empty":           "",
		"no scheme":       "127.0.0.1:8080",
		"scheme-relative": "//host:8080",
		"ftp":             "ftp://host",
		"no host":         "http://",
		"port only":       "http://:8080",
		"opaque":          "http:host",
		"userinfo":        "http://operator:hunter2hunter2@host:8080",
		"a username":      "http://operator@host:8080",
		"a query":         "http://host:8080?x=1",
		"an empty query":  "http://host:8080/?",
		"a fragment":      "http://host:8080/#x",
		"not a url":       "ht!tp://host",
	}
	for name, u := range badURLs {
		c, err := New(u, testSecret, nil)
		if err == nil || c != nil {
			t.Errorf("%s: New(%q) = %v, %v, want an error and no client", name, u, c, err)
			continue
		}
		for _, leak := range []string{"hunter2hunter2", testToken, "operator"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error contains %q: %v", name, leak, err)
			}
		}
		if len(u) > 6 && strings.Contains(err.Error(), u) {
			t.Errorf("%s: the error echoes the URL: %v", name, err)
		}
	}

	badTokens := map[string]string{
		"empty":              "",
		"a space":            "zzzz tok",
		"only whitespace":    "   ",
		"a trailing newline": "zzzz-tok-zzzz\n",
		"a trailing CRLF":    "zzzz-tok-zzzz\r\n",
		"a leading tab":      "\tzzzz-tok-zzzz",
		"a nul byte":         "zzzz\x00tok",
		"a delete character": "zzzz\x7ftok",
		"a non-ASCII letter": "zzzz-tök-zzzz",
	}
	for name, tok := range badTokens {
		c, err := New("http://127.0.0.1:8080", config.Secret(tok), nil)
		if err == nil || c != nil {
			t.Errorf("%s: New with that token = %v, %v, want an error and no client", name, c, err)
			continue
		}
		if len(strings.TrimSpace(tok)) > 3 && strings.Contains(err.Error(), strings.TrimSpace(tok)) {
			t.Errorf("%s: the error contains the token: %v", name, err)
		}
	}

	// Controls: well-formed URLs are accepted and normalised, and the normal token is accepted.
	for in, want := range map[string]string{
		"http://127.0.0.1:8080":    "http://127.0.0.1:8080",
		"http://127.0.0.1:8080/":   "http://127.0.0.1:8080",
		"http://127.0.0.1:8080///": "http://127.0.0.1:8080",
		"https://m3.internal":      "https://m3.internal",
		"HTTP://Host:1/m3/":        "http://Host:1/m3",
		"http://[::1]:8080/":       "http://[::1]:8080",
	} {
		c, err := New(in, testSecret, nil)
		if err != nil {
			t.Errorf("New(%q): %v", in, err)
			continue
		}
		if c.base != want {
			t.Errorf("New(%q).base = %q, want %q", in, c.base, want)
		}
	}

	// The trimmed base is what requests go to: no doubled slash, and a path prefix is kept.
	f := newFake(t)
	c, err := New(f.srv.URL+"/", testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetSuspended(testCtx(t), userA, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.requests()[0].Path; got != "/v1/admin/users/"+userA+"/suspend" {
		t.Errorf("a trailing slash on the base URL gave the path %q", got)
	}
	f.reset()
	f.setForce(always(&reply{status: 200, body: okBody(userA, true)}))
	if c, err = New(f.srv.URL+"/m3/", testSecret, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetSuspended(testCtx(t), userA, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.requests()[0].Path; got != "/m3/v1/admin/users/"+userA+"/suspend" {
		t.Errorf("a path prefix on the base URL gave the path %q", got)
	}
}

func TestNew_HTTPClientDefaults(t *testing.T) {
	c, err := New("http://127.0.0.1:1", testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if DefaultTimeout != 10*time.Second || c.hc.Timeout != 10*time.Second {
		t.Errorf("default timeout = %v (constant %v), want 10s", c.hc.Timeout, DefaultTimeout)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || !tr.DisableKeepAlives {
		t.Errorf("the default transport must be an *http.Transport with no proxy and no connection reuse: %#v", c.hc.Transport)
	}
	if c.hc.CheckRedirect == nil || !errors.Is(c.hc.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("the default client follows redirects")
	}

	// A supplied client is copied, not modified; its own settings survive; redirects are switched off
	// whatever policy it came with.
	own := &http.Client{Timeout: 3 * time.Second}
	c2, err := New("http://127.0.0.1:1", testSecret, own)
	if err != nil {
		t.Fatal(err)
	}
	if c2.hc == own || own.CheckRedirect != nil {
		t.Error("the supplied client was used directly or modified")
	}
	if c2.hc.Timeout != 3*time.Second {
		t.Errorf("the supplied client's timeout was lost: %v", c2.hc.Timeout)
	}
	follows := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	c3, err := New("http://127.0.0.1:1", testSecret, follows)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c3.hc.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("a supplied redirect-following client still follows redirects through the Client")
	}
	if follows.CheckRedirect(nil, nil) != nil {
		t.Error("the caller's own client was modified")
	}
}

// ---- secrecy -----------------------------------------------------------------------------------------------------

// leaks reports every way s carries the token: verbatim, or as the hex that %x of an error produces.
func leaks(s string) []string {
	var out []string
	if strings.Contains(s, testToken) {
		out = append(out, "the token")
	}
	if strings.Contains(strings.ToLower(s), hex.EncodeToString([]byte(testToken))) {
		out = append(out, "the token in hex")
	}
	return out
}

// renderings is every way the standard library renders v.
func renderings(v any) []string {
	return []string{
		fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v),
		fmt.Sprintf("%s", v), fmt.Sprintf("%q", v), fmt.Sprintf("%d", v), fmt.Sprintf("%x", v), fmt.Sprint(v),
	}
}

func TestSecrecy_TokenNeverPrinted(t *testing.T) {
	auth := "Bearer " + testToken
	// A hostile module 3: it echoes the Authorization header into every field of every response, which is
	// the worst case for a credential reaching an error string.
	hostile := func(status int, code string, details ...detail) *reply {
		b, err := json.Marshal(map[string]any{
			"error":     map[string]any{"code": code, "message": auth, "details": details, "request_id": testToken},
			"user_id":   testToken,
			"suspended": false,
			"reason":    auth,
		})
		if err != nil {
			t.Fatal(err)
		}
		return &reply{status: status, body: string(b)}
	}
	suspendedConflict := detail{"suspended", "conflict"}

	f := newFake(t)
	c := f.newClient(t, nil)
	type scenario struct {
		name      string
		suspended bool
		force     func(seen) *reply
		class     string
	}
	scenarios := []scenario{
		{"401", true, always(hostile(401, "unauthenticated")), "unauthorized"},
		{"401 with the token as its code", true, always(hostile(401, testToken)), "unauthorized"},
		{"400", false, always(hostile(400, "validation_failed")), "invalid"},
		{"404 not_found with the token as request id", true, always(hostile(404, "not_found")), "unknown-user"},
		{"404 with the token as its code", true, always(hostile(404, testToken)), "retryable"},
		{"500", true, always(hostile(500, "internal_error")), "retryable"},
		{"500 with the token as its code", false, always(hostile(500, testToken)), "retryable"},
		{"429", true, always(hostile(429, "rate_limited")), "retryable"},
		{"418", true, always(hostile(418, "teapot")), "retryable"},
		{"422 without the conflict", true, always(hostile(422, "unprocessable")), "retryable"},
		{"200 that disagrees", true, always(hostile(200, "unprocessable")), "retryable"},
		{"200 garbage", true, always(&reply{status: 200, body: auth + " OK"}), "retryable"},
		{"200 for another user", true, always(&reply{status: 200, body: `{"user_id":"` + testToken + `","suspended":true}`}), "retryable"},
		{"a redirect", true, always(&reply{status: 302, body: auth, header: map[string]string{"Location": "http://127.0.0.1:1/" + testToken}}), "retryable"},
		{"an oversized body", true, always(&reply{status: 200, body: strings.Repeat(auth+" ", MaxBodyBytes/len(auth))}), "retryable"},
		{"unsuspend 422, probe 404", false, func(s seen) *reply {
			if s.Method == http.MethodPost {
				return hostile(422, "unprocessable", suspendedConflict)
			}
			return hostile(404, "not_found")
		}, "unknown-user"},
		{"unsuspend 422, probe 500", false, func(s seen) *reply {
			if s.Method == http.MethodPost {
				return hostile(422, "unprocessable", suspendedConflict)
			}
			return hostile(500, "internal_error")
		}, "retryable"},
		{"unsuspend 422, probe 401", false, func(s seen) *reply {
			if s.Method == http.MethodPost {
				return hostile(422, "unprocessable", suspendedConflict)
			}
			return hostile(401, "unauthenticated")
		}, "unauthorized"},
	}

	type outcome struct {
		name string
		err  error
	}
	var outs []outcome
	for _, sc := range scenarios {
		// Non-vacuity: this scenario's response really does carry the token (or its header form).
		if rep := sc.force(seen{Method: http.MethodPost}); rep != nil && !strings.Contains(rep.body, testToken) && rep.header["Location"] == "" {
			t.Fatalf("%s: the hostile response does not contain the token, so the scenario proves nothing", sc.name)
		}
		f.setForce(sc.force)
		res, err := c.SetSuspended(testCtx(t), userA, sc.suspended, "")
		wantFailure(t, sc.name, res, err, sc.class)
		outs = append(outs, outcome{sc.name, err})
	}
	f.setForce(nil)

	// Refusals made before any request.
	res, err := c.SetSuspended(testCtx(t), "NOT-A-UUID", true, "")
	wantFailure(t, "a bad id", res, err, "invalid")
	outs = append(outs, outcome{"a bad id", err})
	res, err = c.SetSuspended(testCtx(t), userA, true, strings.Repeat("a", MaxReasonBytes+1))
	wantFailure(t, "a bad reason", res, err, "invalid")
	outs = append(outs, outcome{"a bad reason", err})

	// A canceled context, a timeout, and a closed server.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = c.SetSuspended(canceled, userA, true, "")
	wantFailure(t, "a canceled context", res, err, "retryable")
	outs = append(outs, outcome{"a canceled context", err})

	slow := newFake(t)
	slow.setForce(always(&reply{status: 200, body: okBody(userA, true), delay: time.Minute}))
	res, err = slow.newClient(t, &http.Client{Timeout: 100 * time.Millisecond}).SetSuspended(testCtx(t), userA, true, "")
	wantFailure(t, "a timeout", res, err, "retryable")
	outs = append(outs, outcome{"a timeout", err})

	// Non-vacuity: the key really was on the wire, in the exact bearer form, on the requests above.
	sawKey := false
	for _, r := range f.requests() {
		if r.Auth == auth {
			sawKey = true
		}
	}
	if !sawKey {
		t.Fatal("no request carried the admin key: nothing here proves the key is kept out of errors")
	}

	f.srv.Close()
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantFailure(t, "a closed server", res, err, "retryable")
	outs = append(outs, outcome{"a closed server", err})

	// Every error, every error in its chain, three ways and then some, and the Client itself.
	for _, o := range outs {
		if o.err == nil {
			t.Errorf("%s: no error to render", o.name)
			continue
		}
		for e := o.err; e != nil; e = errors.Unwrap(e) {
			for _, s := range renderings(e) {
				if l := leaks(s); len(l) != 0 {
					t.Errorf("%s: a rendering of %T carries %v: %q", o.name, e, l, s)
				}
			}
		}
		if l := leaks(o.err.Error()); len(l) != 0 {
			t.Errorf("%s: the error text carries %v: %q", o.name, l, o.err.Error())
		}
	}
	for i, v := range []any{c, *c, &c} {
		for _, s := range renderings(v) {
			if l := leaks(s); len(l) != 0 {
				t.Errorf("rendering of the client (variant %d) carries %v: %q", i, l, s)
			}
		}
	}
	// The Client renders as redacted rather than as nothing, so the protection is what is under test.
	if s := fmt.Sprintf("%+v", c); !strings.Contains(s, "[redacted]") || !strings.Contains(s, c.base) {
		t.Errorf("the Client renders as %q, want its base URL and [redacted]", s)
	}
	// And through slog, both handlers.
	var logged bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&logged, nil), slog.NewJSONHandler(&logged, nil)} {
		slog.New(h).Info("m3 call", "client", c, "clientValue", *c, "err", outs[0].err, "retryable", outs[len(outs)-1].err)
	}
	if logged.Len() == 0 {
		t.Fatal("slog wrote nothing")
	}
	if l := leaks(logged.String()); len(l) != 0 {
		t.Errorf("slog output carries %v: %s", l, logged.String())
	}

	// Controls: the instrument can see a leak. A struct that holds the same Secret in an UNEXPORTED field
	// prints it raw (fmt does not call methods on unexported fields), which is why Client implements Format
	// itself, and an error that quotes a response body leaks it.
	naive := struct{ token config.Secret }{testSecret}
	if l := leaks(fmt.Sprintf("%+v", naive)); len(l) == 0 {
		t.Error("control failed: a struct with an unexported config.Secret field did not print the token, so the Client's own Format is not what is being tested")
	}
	careless := fmt.Errorf("bad body: %s", auth)
	if l := leaks(careless.Error()); len(l) == 0 {
		t.Error("control failed: leaks() cannot see a token in an error string")
	}
	if l := leaks(strings.ToUpper(fmt.Sprintf("%x", careless))); len(l) == 0 {
		t.Error("control failed: leaks() cannot see the hex form of a token")
	}
}

// ---- transport ---------------------------------------------------------------------------------------------------

func TestRedirects_NotFollowed(t *testing.T) {
	origin := newFake(t)
	target := newFake(t)

	// Non-vacuity: the target would have accepted the request. A client that followed the redirect would
	// have got a success from it.
	control := newFake(t)
	res, err := control.newClient(t, nil).SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: the target accepts a direct call", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})

	clients := map[string]*http.Client{
		"the default client":               nil,
		"a supplied client with a timeout": {Timeout: 5 * time.Second},
		"a supplied client that follows":   {},
	}
	for _, status := range []int{301, 302, 303, 307, 308} {
		for name, hc := range clients {
			origin.reset()
			target.reset()
			origin.setForce(always(&reply{status: status,
				header: map[string]string{"Location": target.srv.URL + "/v1/admin/users/" + userA + "/suspend"}}))
			label := fmt.Sprintf("%d with %s", status, name)
			res, err := origin.newClient(t, hc).SetSuspended(testCtx(t), userA, true, "because")
			wantFailure(t, label, res, err, "retryable")
			if statusOf(err) != status {
				t.Errorf("%s: carried status %d", label, statusOf(err))
			}
			if origin.count() != 1 {
				t.Errorf("%s: %d requests to the origin, want 1", label, origin.count())
			}
			if target.count() != 0 {
				t.Errorf("%s: the redirect was followed: the target received %+v", label, target.requests())
			}
		}
	}
	if target.isSuspended(userA) {
		t.Fatal("the target's state changed")
	}

	// Paired control: with the redirect removed the origin answers and the call succeeds.
	origin.reset()
	origin.setForce(nil)
	res, err = origin.newClient(t, nil).SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: no redirect", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
}

func TestResponseSizeCap(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	// ok200 is a valid 200 body of exactly total bytes.
	ok200 := func(total int) string {
		head, tail := `{"user_id":"`+userA+`","suspended":true,"pad":"`, `"}`
		return head + strings.Repeat("p", total-len(head)-len(tail)) + tail
	}
	// conflict422 is the documented conflict envelope of exactly total bytes.
	conflict422 := func(total int) string {
		head := `{"error":{"code":"unprocessable","details":[{"field":"suspended","issue":"conflict"}],"request_id":"` + reqID + `","pad":"`
		tail := `"}}`
		return head + strings.Repeat("p", total-len(head)-len(tail)) + tail
	}
	if len(ok200(MaxBodyBytes)) != MaxBodyBytes || len(conflict422(MaxBodyBytes)) != MaxBodyBytes {
		t.Fatal("test setup: the bodies are not the size they claim")
	}

	// The cap is 1 MiB INCLUSIVE: exactly 1 MiB is read, one byte more is refused.
	f.setForce(always(&reply{status: 200, body: ok200(MaxBodyBytes)}))
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "a 200 of exactly 1 MiB", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
	for _, total := range []int{MaxBodyBytes + 1, 3 * MaxBodyBytes} {
		f.setForce(always(&reply{status: 200, body: ok200(total)}))
		res, err = c.SetSuspended(testCtx(t), userA, true, "")
		wantFailure(t, fmt.Sprintf("a 200 of %d bytes", total), res, err, "retryable")
		if !strings.Contains(reasonOf(err), "1 MiB") {
			t.Errorf("a 200 of %d bytes: the reason does not name the cap: %q", total, reasonOf(err))
		}
	}

	// The same on the conflict path, which would otherwise confirm "already suspended" from a body that
	// was never fully read.
	f.setForce(always(&reply{status: 422, body: conflict422(MaxBodyBytes)}))
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "a 422 conflict of exactly 1 MiB", res, err, Result{State: StateSuspended, Changed: false, HTTPStatus: 422})
	f.setForce(always(&reply{status: 422, body: conflict422(MaxBodyBytes + 1)}))
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantFailure(t, "a 422 conflict of 1 MiB + 1", res, err, "retryable")

	// For unsuspend the oversized 422 is refused before any probe.
	f.reset()
	f.setForce(onPost(&reply{status: 422, body: conflict422(MaxBodyBytes + 1)}))
	res, err = c.SetSuspended(testCtx(t), userA, false, "")
	wantFailure(t, "an oversized unsuspend 422", res, err, "retryable")
	if f.count() != 1 {
		t.Errorf("%d requests, want 1: no probe after an unreadable 422", f.count())
	}

	// An oversized not_found envelope is not trusted as "unknown user"; an oversized 401 is still a 401
	// because its status alone decides.
	big := strings.Repeat("x", 2*MaxBodyBytes)
	f.setForce(always(&reply{status: 404, body: strings.Replace(envBody("not_found", "x"), `"message":"x"`, `"message":"`+big+`"`, 1)}))
	res, err = c.SetSuspended(testCtx(t), userC, true, "")
	wantFailure(t, "an oversized 404", res, err, "retryable")
	f.setForce(always(&reply{status: 401, body: big}))
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantFailure(t, "an oversized 401", res, err, "unauthorized")
}

func TestContext_DeadlineAndCancelHonoured(t *testing.T) {
	f := newFake(t)
	// No client timeout: only the context bounds the call.
	c := f.newClient(t, &http.Client{})
	f.setForce(always(&reply{status: 200, body: okBody(userA, true), delay: time.Minute}))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := c.SetSuspended(ctx, userA, true, "")
	elapsed := time.Since(start)
	wantFailure(t, "a context deadline", res, err, "retryable")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the deadline is not reachable with errors.Is: %v", err)
	}
	if !strings.Contains(reasonOf(err), "timeout") {
		t.Errorf("reason = %q, want it to name a timeout", reasonOf(err))
	}
	if elapsed > 10*time.Second {
		t.Errorf("the call took %v against a 200ms deadline", elapsed)
	}

	// A context canceled before the call: no request is made.
	f.reset()
	pre, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	res, err = c.SetSuspended(pre, userA, true, "")
	wantFailure(t, "a context canceled before the call", res, err, "retryable")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation is not reachable with errors.Is: %v", err)
	}
	if f.count() != 0 {
		t.Errorf("%d requests were made with a canceled context", f.count())
	}

	// A context canceled while the request is held by the server.
	f.reset()
	mid, cancelMid := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for stop := time.Now().Add(20 * time.Second); f.count() == 0 && time.Now().Before(stop); {
			time.Sleep(5 * time.Millisecond)
		}
		cancelMid()
	}()
	res, err = c.SetSuspended(mid, userA, true, "")
	<-done
	wantFailure(t, "a context canceled mid-flight", res, err, "retryable")
	if !errors.Is(err, context.Canceled) || f.count() != 1 {
		t.Errorf("err = %v, requests = %d, want a cancellation after exactly 1 request", err, f.count())
	}

	// Paired control: a generous deadline and a prompt reply succeed.
	f.setForce(always(&reply{status: 200, body: okBody(userA, true)}))
	ok, cancelOK := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelOK()
	res, err = c.SetSuspended(ok, userA, true, "")
	wantResult(t, "control: a generous deadline", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
}

func TestRetryable_HelperAndErrorShape(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.9:8080: boom")
	re := &RetryableError{Op: "suspend", HTTPStatus: 503, Reason: "a server error", Cause: cause}

	if !Retryable(re) || !Retryable(fmt.Errorf("while suspending: %w", re)) {
		t.Error("a *RetryableError, bare or wrapped, is retryable")
	}
	var target *RetryableError
	if !errors.As(fmt.Errorf("a: %w", re), &target) || target != re {
		t.Error("errors.As does not reach the *RetryableError through a wrap")
	}
	if errors.Unwrap(re) != cause || !errors.Is(re, cause) {
		t.Error("Unwrap does not give the cause")
	}
	if (&RetryableError{Op: "x"}).Unwrap() != nil {
		t.Error("Unwrap of an error with no cause must be nil")
	}
	txt := re.Error()
	for _, want := range []string{"suspend", "a server error", "HTTP 503", "retry later"} {
		if !strings.Contains(txt, want) {
			t.Errorf("Error() = %q, missing %q", txt, want)
		}
	}
	if strings.Contains(txt, "10.0.0.9") || strings.Contains(txt, "boom") {
		t.Errorf("Error() prints the cause: %q", txt)
	}
	if strings.Contains((&RetryableError{Op: "suspend", Reason: "a timeout"}).Error(), "HTTP") {
		t.Error("Error() names an HTTP status when there was no response")
	}

	// Nothing else is retryable, and the four classes do not overlap.
	for name, err := range map[string]error{
		"nil":                  nil,
		"a plain error":        errors.New("x"),
		"ErrUnauthorized":      ErrUnauthorized,
		"ErrUnknownUser":       ErrUnknownUser,
		"ErrInvalid":           ErrInvalid,
		"a wrapped sentinel":   fmt.Errorf("%w: suspend: HTTP 401", ErrUnauthorized),
		"context deadline":     context.DeadlineExceeded,
		"an unrelated wrapped": fmt.Errorf("a: %w", io.EOF),
	} {
		if Retryable(err) {
			t.Errorf("%s is reported retryable", name)
		}
	}
	for name, want := range map[string]struct {
		err   error
		class string
	}{
		"unauthorized": {ErrUnauthorized, "unauthorized"},
		"unknown user": {ErrUnknownUser, "unknown-user"},
		"invalid":      {ErrInvalid, "invalid"},
		"retryable":    {re, "retryable"},
		"nil":          {nil, "none"},
	} {
		if got := classes(want.err); got != want.class {
			t.Errorf("%s: classes = %s, want %s", name, got, want.class)
		}
	}

	// The error SetSuspended returns for a 503 has no cause: there was a response, not a failure to get one.
	f := newFake(t)
	f.setForce(always(&reply{status: 503}))
	_, err := f.newClient(t, nil).SetSuspended(testCtx(t), userA, true, "")
	if !Retryable(err) || errors.Unwrap(err) != nil {
		t.Errorf("a 503 gave %v (unwraps to %v), want a retryable error with no cause", err, errors.Unwrap(err))
	}
}

func TestUnexpectedStatuses_AreRetryable(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	statuses := []int{201, 202, 204, 206, 300, 303, 304, 307, 308, 402, 403, 405, 406, 408, 409, 410, 415, 418, 423, 426, 451}
	for _, status := range statuses {
		f.setForce(always(&reply{status: status}))
		both(func(suspended bool, op string) {
			label := fmt.Sprintf("%s answered %d", op, status)
			res, err := c.SetSuspended(testCtx(t), userA, suspended, "")
			wantFailure(t, label, res, err, "retryable")
			if statusOf(err) != status {
				t.Errorf("%s: carried status %d", label, statusOf(err))
			}
		})
	}
	// A 405 from module 3's own envelope is no different: this client never sends the wrong method, so it
	// is something in front of module 3 or a changed contract, and no state was confirmed.
	f.setForce(always(envReply(405, "method_not_allowed", "This method is not allowed on this path.")))
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantFailure(t, "a 405 envelope", res, err, "retryable")

	// Control: 200 is accepted.
	f.setForce(nil)
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "control: 200", res, err, Result{State: StateSuspended, Changed: true, HTTPStatus: 200})
}

func TestRoundTrip_AgainstTheStatefulFake(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	step := func(name, id string, suspended bool, want Result, wantClass string, wantReqs int) {
		t.Helper()
		f.reset()
		res, err := c.SetSuspended(testCtx(t), id, suspended, "")
		if wantClass == "" {
			wantResult(t, name, res, err, want)
		} else {
			wantFailure(t, name, res, err, wantClass)
		}
		if n := f.count(); n != wantReqs {
			t.Errorf("%s: %d requests, want %d", name, n, wantReqs)
		}
	}
	step("suspend A", userA, true, Result{StateSuspended, true, 200}, "", 1)
	step("suspend A again", userA, true, Result{StateSuspended, false, 422}, "", 1)
	step("unsuspend A", userA, false, Result{StateActive, true, 200}, "", 1)
	step("unsuspend A again", userA, false, Result{StateActive, false, 422}, "", 2)
	step("suspend the unknown user", userC, true, Result{}, "unknown-user", 1)
	step("unsuspend the unknown user", userC, false, Result{}, "unknown-user", 2)
	step("suspend B, already suspended", userB, true, Result{StateSuspended, false, 422}, "", 1)
	step("unsuspend B", userB, false, Result{StateActive, true, 200}, "", 1)
	if f.isSuspended(userA) || f.isSuspended(userB) || f.isSuspended(userC) {
		t.Fatalf("the end state is wrong: A=%v B=%v C=%v", f.isSuspended(userA), f.isSuspended(userB), f.isSuspended(userC))
	}

	// WHY a lost response is safe to retry: module 3 applies the change, the response never arrives, the
	// call is a *RetryableError and nothing is claimed; asking again is answered by the 422 conflict,
	// which confirms the state the first call reached.
	f.reset()
	f.setForce(func(s seen) *reply {
		f.model(s)                  // module 3 does the work ...
		return &reply{hangup: true} // ... and the answer is lost
	})
	res, err := c.SetSuspended(testCtx(t), userA, true, "")
	wantFailure(t, "suspend whose response was lost", res, err, "retryable")
	if !f.isSuspended(userA) {
		t.Fatal("test setup: module 3 did not apply the change before the response was lost")
	}
	f.setForce(nil)
	res, err = c.SetSuspended(testCtx(t), userA, true, "")
	wantResult(t, "the retry converges", res, err, Result{State: StateSuspended, Changed: false, HTTPStatus: 422})
}
