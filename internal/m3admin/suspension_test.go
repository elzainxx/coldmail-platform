package m3admin

// Unit tests of Client.Suspension, the read of module 3's suspension route (module 3 238bd0a, Docs/api-contract.md "Reading a user's suspension state"), against the in-process module 3 of
// fake_test.go. Every refusal is paired with a control that proves the same setup would have succeeded had the refused property held, so a test that "passes" because the client never got
// that far fails instead. The bodies are written out as module 3 writes them (five keys, always present), never built by the code under test.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// susp is what one read is expected to say: the two instants are RFC 3339 strings in UTC as Go prints them, "" for null.
type susp struct {
	suspended                 bool
	suspendedAt, reinstatedAt string
	version                   int64
}

// renderState is one line that says everything a SuspensionState says, so that two of them are equal exactly when every field is.
func renderState(st SuspensionState) string {
	at := func(t *time.Time) string {
		if t == nil {
			return "null"
		}
		return t.Format(time.RFC3339Nano) + "/" + t.Location().String()
	}
	return fmt.Sprintf("user=%s suspended=%t suspended_at=%s reinstated_at=%s version=%d", st.User, st.Suspended, at(st.SuspendedAt), at(st.ReinstatedAt), st.Version)
}

// renderWant is renderState's line for the expectation: every instant is UTC.
func renderWant(user string, w susp) string {
	at := func(s string) string {
		if s == "" {
			return "null"
		}
		return s + "/UTC"
	}
	return fmt.Sprintf("user=%s suspended=%t suspended_at=%s reinstated_at=%s version=%d", user, w.suspended, at(w.suspendedAt), at(w.reinstatedAt), w.version)
}

func wantSuspension(t *testing.T, label string, st SuspensionState, err error, user string, want susp) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: unexpected error: %v", label, err)
		return
	}
	if got, w := renderState(st), renderWant(user, want); got != w {
		t.Errorf("%s:\n  got  %s\n  want %s", label, got, w)
	}
}

// wantSuspensionFailure asserts the read returned no state and an error in exactly the named class.
func wantSuspensionFailure(t *testing.T, label string, st SuspensionState, err error, class string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want class %s (the read returned %s)", label, class, renderState(st))
		return
	}
	if st.User != "" || st.Suspended || st.SuspendedAt != nil || st.ReinstatedAt != nil || st.Version != 0 {
		t.Errorf("%s: a state (%s) was reported alongside the error %v", label, renderState(st), err)
	}
	if got := classes(err); got != class {
		t.Errorf("%s: error class = %s, want %s (error: %v)", label, got, class, err)
	}
}

// wantReadRequest asserts the request the fake received is exactly module 3's read route for the user, with the admin key and nothing else.
func wantReadRequest(t *testing.T, label string, r seen, id string) {
	t.Helper()
	if r.Method != http.MethodGet || r.Path != "/v1/admin/users/"+id+"/suspension" || r.Query != "" || r.Auth != "Bearer "+testToken || len(r.Body) != 0 || r.ContentType != "" {
		t.Errorf("%s: the request was %+v, want a bare GET /v1/admin/users/%s/suspension with the admin key, no query, no body and no content type", label, r, id)
	}
}

// goodBody is the five keys of the read route as module 3 writes them, with one raw value replaced (over) and one key dropped (drop). The order is the contract's.
func goodBody(over map[string]string, drop string) string {
	raw := map[string]string{
		"user_id": `"` + userA + `"`, "suspended": `true`, "suspended_at": `"2026-11-17T09:30:00.123456Z"`, "reinstated_at": `null`, "version": `1`,
	}
	for k, v := range over {
		if _, ok := raw[k]; !ok {
			panic("goodBody: unknown key " + k)
		}
		raw[k] = v
	}
	var parts []string
	for _, k := range []string{"user_id", "suspended", "suspended_at", "reinstated_at", "version"} {
		if k != drop {
			parts = append(parts, `"`+k+`":`+raw[k])
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestSuspension_ReadsTheDocumentedBodiesVerbatim(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	const (
		first  = "2026-11-17T09:30:00.123456Z"
		second = "2026-11-17T09:45:10.5Z" // module 3 drops trailing zeros of the fraction
	)
	cases := []struct {
		name, body string
		want       susp
	}{
		{"a suspended user (the contract's own example, spaces and all)",
			`{"user_id": "` + userA + `", "suspended": true, "suspended_at": "2026-11-17T09:30:00.123456Z", "reinstated_at": null, "version": 1}`,
			susp{suspended: true, suspendedAt: first, version: 1}},
		{"a reinstated user (suspended_at is kept after a reinstatement)",
			`{"user_id":"` + userA + `","suspended":false,"suspended_at":"2026-11-17T09:30:00.123456Z","reinstated_at":"2026-11-17T09:45:10.5Z","version":2}`,
			susp{suspended: false, suspendedAt: first, reinstatedAt: second, version: 2}},
		{"a user suspended again (reinstated_at is kept by a later suspension)",
			`{"user_id":"` + userA + `","suspended":true,"suspended_at":"2026-11-17T10:00:00.000001Z","reinstated_at":"2026-11-17T09:45:10.5Z","version":3}`,
			susp{suspended: true, suspendedAt: "2026-11-17T10:00:00.000001Z", reinstatedAt: second, version: 3}},
		{"a never-suspended user (no row: active, no instants, version 0)",
			`{"user_id":"` + userA + `","suspended":false,"suspended_at":null,"reinstated_at":null,"version":0}`,
			susp{suspended: false, version: 0}},
		{"a body with a key the contract does not list (module 3's API is additive)",
			`{"user_id":"` + userA + `","suspended":true,"suspended_at":"2026-11-17T09:30:00.123456Z","reinstated_at":null,"version":1,"added_later":{"x":[1,2]}}`,
			susp{suspended: true, suspendedAt: first, version: 1}},
		{"an instant written with an offset is read as the same instant in UTC",
			`{"user_id":"` + userA + `","suspended":true,"suspended_at":"2026-11-17T11:30:00.123456+02:00","reinstated_at":null,"version":1}`,
			susp{suspended: true, suspendedAt: first, version: 1}},
		{"a version beyond 32 bits",
			`{"user_id":"` + userA + `","suspended":false,"suspended_at":null,"reinstated_at":null,"version":9007199254740993}`,
			susp{suspended: false, version: 9007199254740993}},
	}
	seenStates := map[string]string{}
	for _, tc := range cases {
		f.reset()
		f.setForce(always(&reply{status: 200, body: tc.body}))
		st, err := c.Suspension(testCtx(t), userA)
		wantSuspension(t, tc.name, st, err, userA, tc.want)
		reqs := f.requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests, want exactly 1", tc.name, len(reqs))
		}
		wantReadRequest(t, tc.name, reqs[0], userA)
		if want := tc.want.suspended; st.Suspended != want || (st.State() == StateSuspended) != want {
			t.Errorf("%s: Suspended=%v State()=%s, want suspended=%v", tc.name, st.Suspended, st.State(), want)
		}
		if st.SuspendedAt != nil && st.SuspendedAt.Location() != time.UTC {
			t.Errorf("%s: suspended_at is in %s, want UTC", tc.name, st.SuspendedAt.Location())
		}
		seenStates[renderState(st)] = tc.name
	}
	// Control: the cases are distinguishable (a parser that returned one fixed state, or ignored a field, would collapse some of them into one line). Two of
	// them are the first case's state on purpose (the body with an extra key and the instant with an offset), so there are two lines fewer than cases.
	if got, want := len(seenStates), len(cases)-2; got != want {
		t.Errorf("the %d cases produced %d distinct states, want %d: %v", len(cases), got, want, sortedKeys(seenStates))
	}

	// Control: with nothing forced the same client reads the stateful fake: userA was never suspended (version 0), userB is suspended at version 1.
	f.reset()
	f.setForce(nil)
	st, err := c.Suspension(testCtx(t), userA)
	wantSuspension(t, "control: a known user with no row", st, err, userA, susp{})
	st, err = c.Suspension(testCtx(t), userB)
	wantSuspension(t, "control: a suspended user", st, err, userB, susp{suspended: true, suspendedAt: first, version: 1})
	if n := f.count(); n != 2 {
		t.Errorf("%d requests for two reads, want 2", n)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestSuspension_ReadsBackWhatTheSuspendRoutesDo(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	read := func(label, id string) SuspensionState {
		t.Helper()
		st, err := c.Suspension(testCtx(t), id)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return st
	}
	var versions []int64
	record := func(st SuspensionState) { versions = append(versions, st.Version) }

	// never suspended: active, no instants, version 0
	st := read("A before anything", userA)
	record(st)
	if st.Suspended || st.SuspendedAt != nil || st.ReinstatedAt != nil || st.Version != 0 || st.State() != StateActive {
		t.Fatalf("a user with no row reads %s, want active, no instants, version 0", renderState(st))
	}

	// suspend: the read says suspended, with an instant and version 1
	if res, err := c.SetSuspended(testCtx(t), userA, true, ""); err != nil || !res.Changed {
		t.Fatalf("suspend A: %+v, %v", res, err)
	}
	st = read("A after suspend", userA)
	record(st)
	if !st.Suspended || st.SuspendedAt == nil || st.ReinstatedAt != nil || st.Version != 1 || st.State() != StateSuspended {
		t.Fatalf("after a suspend A reads %s, want suspended, suspended_at set, version 1", renderState(st))
	}
	suspendedAt := *st.SuspendedAt

	// a refused POST (422: already suspended) changes no flag and does NOT move the version
	if res, err := c.SetSuspended(testCtx(t), userA, true, ""); err != nil || res.Changed {
		t.Fatalf("suspend A again: %+v, %v", res, err)
	}
	st = read("A after a refused suspend", userA)
	record(st)
	if st.Version != 1 || !st.Suspended {
		t.Fatalf("a refused suspend moved the state: A reads %s, want suspended at version 1", renderState(st))
	}

	// unsuspend: active, both instants (suspended_at is kept), version 2
	if res, err := c.SetSuspended(testCtx(t), userA, false, ""); err != nil || !res.Changed {
		t.Fatalf("unsuspend A: %+v, %v", res, err)
	}
	st = read("A after unsuspend", userA)
	record(st)
	if st.Suspended || st.SuspendedAt == nil || !st.SuspendedAt.Equal(suspendedAt) || st.ReinstatedAt == nil || !st.ReinstatedAt.After(suspendedAt) || st.Version != 2 {
		t.Fatalf("after an unsuspend A reads %s, want active, suspended_at kept (%s), reinstated_at after it, version 2", renderState(st), suspendedAt.Format(time.RFC3339Nano))
	}

	// suspended again: version 3, reinstated_at kept
	if res, err := c.SetSuspended(testCtx(t), userA, true, ""); err != nil || !res.Changed {
		t.Fatalf("suspend A a second time: %+v, %v", res, err)
	}
	st = read("A after a second suspend", userA)
	record(st)
	if !st.Suspended || st.Version != 3 || st.ReinstatedAt == nil || st.SuspendedAt == nil || !st.SuspendedAt.After(*st.ReinstatedAt) {
		t.Fatalf("after a second suspend A reads %s, want suspended at version 3 with reinstated_at kept and suspended_at after it", renderState(st))
	}

	// a read that follows a write never shows an older version, and only a real transition moves it by exactly one
	for i := 1; i < len(versions); i++ {
		if versions[i] < versions[i-1] || versions[i] > versions[i-1]+1 {
			t.Errorf("the versions read in order are %v: step %d moved by %d, want 0 (a refused call) or 1 (a transition)", versions, i, versions[i]-versions[i-1])
		}
	}
	if want := []int64{0, 1, 1, 2, 3}; fmt.Sprint(versions) != fmt.Sprint(want) {
		t.Errorf("the versions read in order are %v, want %v", versions, want)
	}

	// control: the other user, who was never touched, still reads as it was
	if st := read("B untouched", userB); !st.Suspended || st.Version != 1 {
		t.Errorf("userB reads %s, want suspended at version 1 (the controls above must not leak across users)", renderState(st))
	}
}

func TestSuspension_404_AnUnknownUserIsNeverAnAnswer(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	st, err := c.Suspension(testCtx(t), userC)
	wantSuspensionFailure(t, "an unknown user", st, err, "unknown-user")
	if f.count() != 1 {
		t.Fatalf("%d requests, want 1: a 404 is not retried inside the client", f.count())
	}
	if err != nil && !strings.Contains(err.Error(), reqID) {
		t.Errorf("the error does not carry module 3's request id %s: %v", reqID, err)
	}

	// control: a user module 3 knows, on the same client and route, is a 200
	st, err = c.Suspension(testCtx(t), userA)
	wantSuspension(t, "control: a known user", st, err, userA, susp{})

	// a 404 that is NOT module 3's own not_found envelope says nothing about the user (the wrong base URL, a gateway): retryable, not "unknown"
	notEnvelope := map[string]*reply{
		"an empty 404":                  {status: 404},
		"an html 404":                   {status: 404, body: "<html><body>404</body></html>", header: map[string]string{"Content-Type": "text/html"}},
		"json that is not an error":     {status: 404, body: `{"message":"not found"}`},
		"an envelope with another code": {status: 404, body: envBody("conflict", "x")},
	}
	for name, rep := range notEnvelope {
		f.setForce(always(rep))
		st, err = c.Suspension(testCtx(t), userC)
		wantSuspensionFailure(t, name, st, err, "retryable")
		if statusOf(err) != 404 {
			t.Errorf("%s: carried status %d, want 404", name, statusOf(err))
		}
	}
	// control: module 3's own envelope for the same request IS the typed unknown-user error
	f.setForce(always(notFound()))
	st, err = c.Suspension(testCtx(t), userC)
	wantSuspensionFailure(t, "control: module 3's not_found envelope", st, err, "unknown-user")
}

func TestSuspension_401_TheWrongKeyIsAConfigurationFault(t *testing.T) {
	f := newFake(t)
	wrong, err := New(f.srv.URL, config.Secret("zzzz-wrong-key-zzzz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := wrong.Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "a wrong key", st, err, "unauthorized")
	if f.count() != 1 {
		t.Fatalf("%d requests for 1 call: a 401 must never be retried inside the client", f.count())
	}

	// control: the right key against the same fake is accepted, so the 401 was about the key
	st, err = f.newClient(t, nil).Suspension(testCtx(t), userA)
	wantSuspension(t, "control: the right key", st, err, userA, susp{})

	// a 401 with no usable body is still a refused key
	right := f.newClient(t, nil)
	for name, rep := range map[string]*reply{
		"an empty 401": {status: 401},
		"an html 401":  {status: 401, body: "<html>login</html>", header: map[string]string{"Content-Type": "text/html"}},
	} {
		f.setForce(always(rep))
		st, err = right.Suspension(testCtx(t), userB)
		wantSuspensionFailure(t, name, st, err, "unauthorized")
	}
}

func TestSuspension_ServerErrorsAndTransportFailuresAreRetryableNeverAState(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	for _, status := range []int{500, 502, 503, 504, 429, 405, 302, 307, 418, 201, 204} {
		f.reset()
		f.setForce(always(&reply{status: status, body: goodBody(nil, ""), header: map[string]string{"Location": "http://127.0.0.1:1/"}}))
		st, err := c.Suspension(testCtx(t), userA)
		label := fmt.Sprintf("HTTP %d carrying a perfectly good body", status)
		wantSuspensionFailure(t, label, st, err, "retryable")
		if got := statusOf(err); got != status {
			t.Errorf("%s: the error carries status %d", label, got)
		}
		if f.count() != 1 {
			t.Errorf("%s: %d requests, want 1 (no retry inside the client)", label, f.count())
		}
	}
	// a 400 is module 3 refusing the request itself (it documents none for this route, but a gateway or a changed contract may): the typed invalid error, never a state
	f.setForce(always(envReply(400, "validation_failed", "x")))
	st, err := c.Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "a 400", st, err, "invalid")
	f.setForce(always(envReply(500, "internal_error", "boom")))
	st, err = c.Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "a 500 in module 3's own envelope", st, err, "retryable")
	if err != nil && !strings.Contains(err.Error(), "internal_error") {
		t.Errorf("a 500 in module 3's envelope does not name its code: %v", err)
	}

	// the connection dropped without an answer
	f.setForce(always(&reply{hangup: true}))
	st, err = c.Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "a connection closed without an answer", st, err, "retryable")

	// a slow module 3 and a context that ends: both end the call, neither reports a state
	f.setForce(always(&reply{status: 200, body: goodBody(nil, ""), delay: time.Minute}))
	st, err = f.newClient(t, &http.Client{Timeout: 100 * time.Millisecond}).Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "a client timeout", st, err, "retryable")
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	st, err = c.Suspension(short, userA)
	wantSuspensionFailure(t, "a context deadline", st, err, "retryable")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a context deadline: the error does not wrap context.DeadlineExceeded: %v", err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	st, err = c.Suspension(canceled, userA)
	wantSuspensionFailure(t, "a canceled context", st, err, "retryable")

	// control: with nothing forced the same client and the same route answer 200
	f.setForce(nil)
	st, err = c.Suspension(testCtx(t), userB)
	wantSuspension(t, "control: no fault", st, err, userB, susp{suspended: true, suspendedAt: "2026-11-17T09:30:00.123456Z", version: 1})

	// and a closed server is a transport failure, not a state
	f.srv.Close()
	st, err = c.Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "a closed server", st, err, "retryable")
}

func TestSuspension_AMalformedBodyIsNeverAState(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)

	// control FIRST: the unmutated body is read, so every refusal below is about the one thing it changes
	f.setForce(always(&reply{status: 200, body: goodBody(nil, "")}))
	st, err := c.Suspension(testCtx(t), userA)
	wantSuspension(t, "control: the unmutated body", st, err, userA, susp{suspended: true, suspendedAt: "2026-11-17T09:30:00.123456Z", version: 1})
	f.setForce(always(&reply{status: 200, body: "\n " + goodBody(nil, "") + " \n"}))
	st, err = c.Suspension(testCtx(t), userA)
	wantSuspension(t, "control: the same body with whitespace around it", st, err, userA, susp{suspended: true, suspendedAt: "2026-11-17T09:30:00.123456Z", version: 1})

	good := goodBody(nil, "")
	bad := []struct{ name, body string }{
		{"an empty body", ``},
		{"text", `not json`},
		{"an array", `[]`},
		{"an array of the good object", `[` + good + `]`},
		{"null", `null`},
		{"a string", `"suspended"`},
		{"an empty object", `{}`},
		{"the POST answer's two keys (not the read shape)", `{"user_id":"` + userA + `","suspended":true}`},
		{"the good object followed by another", good + good},
		{"the good object followed by text", good + ` trailing`},
		{"the good object cut short", good[:len(good)-1]},
		{"the key missing: user_id", goodBody(nil, "user_id")},
		{"the key missing: suspended", goodBody(nil, "suspended")},
		{"the key missing: suspended_at (absent is not null)", goodBody(nil, "suspended_at")},
		{"the key missing: reinstated_at (absent is not null)", goodBody(nil, "reinstated_at")},
		{"the key missing: version", goodBody(nil, "version")},
		{"user_id null", goodBody(map[string]string{"user_id": `null`}, "")},
		{"user_id a number", goodBody(map[string]string{"user_id": `5`}, "")},
		{"another user's id", goodBody(map[string]string{"user_id": `"` + userB + `"`}, "")},
		{"suspended null", goodBody(map[string]string{"suspended": `null`}, "")},
		{"suspended a string", goodBody(map[string]string{"suspended": `"true"`}, "")},
		{"suspended a number", goodBody(map[string]string{"suspended": `1`}, "")},
		{"suspended_at a number", goodBody(map[string]string{"suspended_at": `1763371800`}, "")},
		{"suspended_at not an instant", goodBody(map[string]string{"suspended_at": `"yesterday"`}, "")},
		{"suspended_at a date only", goodBody(map[string]string{"suspended_at": `"2026-11-17"`}, "")},
		{"suspended_at an empty string", goodBody(map[string]string{"suspended_at": `""`}, "")},
		{"suspended_at an object", goodBody(map[string]string{"suspended_at": `{}`}, "")},
		{"reinstated_at not an instant", goodBody(map[string]string{"reinstated_at": `"2026-13-45T25:61:61Z"`}, "")},
		{"reinstated_at a boolean", goodBody(map[string]string{"reinstated_at": `false`}, "")},
		{"version a quoted number", goodBody(map[string]string{"version": `"1"`}, "")},
		{"version a fraction", goodBody(map[string]string{"version": `1.5`}, "")},
		{"version in exponent form", goodBody(map[string]string{"version": `1e3`}, "")},
		{"version a whole number written as a decimal", goodBody(map[string]string{"version": `1.0`}, "")},
		{"version negative", goodBody(map[string]string{"version": `-1`}, "")},
		{"version null", goodBody(map[string]string{"version": `null`}, "")},
		{"version a boolean", goodBody(map[string]string{"version": `true`}, "")},
		{"version beyond int64", goodBody(map[string]string{"version": `9223372036854775808`}, "")},
	}
	for _, tc := range bad {
		f.setForce(always(&reply{status: 200, body: tc.body}))
		st, err := c.Suspension(testCtx(t), userA)
		wantSuspensionFailure(t, tc.name, st, err, "retryable")
		if got := statusOf(err); got != 200 {
			t.Errorf("%s: the error carries status %d, want 200 (the status that was received)", tc.name, got)
		}
		if err != nil && !strings.Contains(reasonOf(err), "documented") && !strings.Contains(reasonOf(err), "different user") {
			t.Errorf("%s: the reason does not say what was wrong: %q", tc.name, reasonOf(err))
		}
	}
	if len(bad) < 30 {
		t.Fatalf("only %d malformed bodies: the instrument matched too little", len(bad))
	}

	// module 3 echoes the id as it was sent, so a lower-case request answered with the same id in upper case is not the answer for this id (userA is all digits, userB has letters)
	f.setForce(always(&reply{status: 200, body: goodBody(map[string]string{"user_id": `"` + strings.ToUpper(userB) + `"`}, "")}))
	st, err = c.Suspension(testCtx(t), userB)
	wantSuspensionFailure(t, "the user's id echoed in upper case", st, err, "retryable")
	f.setForce(always(&reply{status: 200, body: goodBody(map[string]string{"user_id": `"` + userB + `"`}, "")}))
	st, err = c.Suspension(testCtx(t), userB)
	wantSuspension(t, "control: the same id echoed as it was sent", st, err, userB, susp{suspended: true, suspendedAt: "2026-11-17T09:30:00.123456Z", version: 1})
}

func TestSuspension_TheBodyCapIsOneMiBInclusive(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	// a valid body of exactly total bytes: the documented five keys, then a padding key the contract does not list (ignored, which is how a body of any size is built)
	padded := func(total int) string {
		head := strings.TrimSuffix(goodBody(nil, ""), "}") + `,"pad":"`
		const tail = `"}`
		return head + strings.Repeat("p", total-len(head)-len(tail)) + tail
	}
	if len(padded(MaxBodyBytes)) != MaxBodyBytes || len(padded(MaxBodyBytes+1)) != MaxBodyBytes+1 {
		t.Fatal("test setup: the bodies are not the size they claim")
	}

	// control: exactly 1 MiB is read
	f.setForce(always(&reply{status: 200, body: padded(MaxBodyBytes)}))
	st, err := c.Suspension(testCtx(t), userA)
	wantSuspension(t, "a 200 of exactly 1 MiB", st, err, userA, susp{suspended: true, suspendedAt: "2026-11-17T09:30:00.123456Z", version: 1})

	// one byte more is refused, naming the cap, and so is much more
	for _, total := range []int{MaxBodyBytes + 1, 3 * MaxBodyBytes} {
		f.setForce(always(&reply{status: 200, body: padded(total)}))
		st, err = c.Suspension(testCtx(t), userA)
		wantSuspensionFailure(t, fmt.Sprintf("a 200 of %d bytes", total), st, err, "retryable")
		if !strings.Contains(reasonOf(err), "1 MiB") {
			t.Errorf("a 200 of %d bytes: the reason does not name the cap: %q", total, reasonOf(err))
		}
	}

	// an oversized not_found envelope is not trusted as "unknown user"; an oversized 401 is still a 401 because its status alone decides
	big := strings.Repeat("x", 2*MaxBodyBytes)
	f.setForce(always(&reply{status: 404, body: strings.Replace(envBody("not_found", "x"), `"message":"x"`, `"message":"`+big+`"`, 1)}))
	st, err = c.Suspension(testCtx(t), userC)
	wantSuspensionFailure(t, "an oversized 404", st, err, "retryable")
	f.setForce(always(&reply{status: 401, body: big}))
	st, err = c.Suspension(testCtx(t), userA)
	wantSuspensionFailure(t, "an oversized 401", st, err, "unauthorized")
}

func TestSuspension_BadIDsAreRefusedBeforeAnyRequest(t *testing.T) {
	f := newFake(t)
	c := f.newClient(t, nil)
	for name, id := range map[string]string{
		"an empty id":              "",
		"not a uuid":               "NOT-A-UUID",
		"a uuid in upper case":     strings.ToUpper(userB),
		"a uuid without hyphens":   strings.ReplaceAll(userA, "-", ""),
		"a path inside the id":     userA + "/../" + userB,
		"an id with a query":       userA + "?x=1",
		"the system user":          "00000000-0000-0000-0000-000000000000",
		"a uuid with a trailing /": userA + "/",
	} {
		st, err := c.Suspension(testCtx(t), id)
		wantSuspensionFailure(t, name, st, err, "invalid")
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d requests were made for ids that must be refused before any request", n)
	}
	// control: a well-formed id makes exactly one request
	st, err := c.Suspension(testCtx(t), userA)
	wantSuspension(t, "control: a canonical lower-case uuid", st, err, userA, susp{})
	if n := f.count(); n != 1 {
		t.Fatalf("%d requests for one valid read, want 1", n)
	}
}

func TestSuspension_TheKeyNeverReachesAnError(t *testing.T) {
	auth := "Bearer " + testToken
	// A hostile module 3: it echoes the Authorization header into every field of every response, the worst case for a credential reaching an error string.
	hostileEnvelope := func(status int, code string) *reply {
		b, err := json.Marshal(map[string]any{
			"error":     map[string]any{"code": code, "message": auth, "request_id": testToken},
			"user_id":   testToken,
			"suspended": true, "suspended_at": auth, "reinstated_at": auth, "version": auth,
		})
		if err != nil {
			t.Fatal(err)
		}
		return &reply{status: status, body: string(b)}
	}
	f := newFake(t)
	c := f.newClient(t, nil)
	scenarios := []struct {
		name  string
		rep   *reply
		class string
	}{
		{"401", hostileEnvelope(401, "unauthenticated"), "unauthorized"},
		{"401 with the token as its code", hostileEnvelope(401, testToken), "unauthorized"},
		{"404 not_found with the token as request id", hostileEnvelope(404, "not_found"), "unknown-user"},
		{"404 with the token as its code", hostileEnvelope(404, testToken), "retryable"},
		{"500", hostileEnvelope(500, "internal_error"), "retryable"},
		{"500 with the token as its code", hostileEnvelope(500, testToken), "retryable"},
		{"200 naming the token as the user", &reply{status: 200, body: `{"user_id":"` + testToken + `","suspended":true,"suspended_at":null,"reinstated_at":null,"version":1}`}, "retryable"},
		{"200 with the token in every field", hostileEnvelope(200, "x"), "retryable"},
		{"200 garbage", &reply{status: 200, body: auth + " OK"}, "retryable"},
		{"a redirect", &reply{status: 302, body: auth, header: map[string]string{"Location": "http://127.0.0.1:1/" + testToken}}, "retryable"},
		{"an oversized body", &reply{status: 200, body: strings.Repeat(auth+" ", MaxBodyBytes/len(auth))}, "retryable"},
	}
	var errs []struct {
		name string
		err  error
	}
	for _, sc := range scenarios {
		if !strings.Contains(sc.rep.body, testToken) && sc.rep.header["Location"] == "" {
			t.Fatalf("%s: the hostile response does not contain the token, so the scenario proves nothing", sc.name)
		}
		f.setForce(always(sc.rep))
		st, err := c.Suspension(testCtx(t), userA)
		wantSuspensionFailure(t, sc.name, st, err, sc.class)
		errs = append(errs, struct {
			name string
			err  error
		}{sc.name, err})
	}
	f.setForce(nil)
	// non-vacuity: the key really was on the wire, in the exact bearer form, on the requests above
	sawKey := false
	for _, r := range f.requests() {
		if r.Auth == auth {
			sawKey = true
		}
	}
	if !sawKey {
		t.Fatal("no request carried the admin key: nothing here proves the key is kept out of errors")
	}
	for _, o := range errs {
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
	}
	var logged bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&logged, nil), slog.NewJSONHandler(&logged, nil)} {
		slog.New(h).Info("m3 read", "client", c, "err", errs[0].err, "retryable", errs[len(errs)-1].err)
	}
	if l := leaks(logged.String()); logged.Len() == 0 || len(l) != 0 {
		t.Errorf("slog output (%d bytes) carries %v: %s", logged.Len(), l, logged.String())
	}
	// control: the instrument can see a leak (an error that quotes a response body does leak it)
	if l := leaks(fmt.Errorf("bad body: %s", auth).Error()); len(l) == 0 {
		t.Error("control failed: leaks() cannot see a token in an error string")
	}
}
