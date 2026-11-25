package api

import (
	"bufio"
	"bytes"
	"context"
	"github.com/coldmail-platform/platform/internal/jobs"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const goodFP = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

func confirmBodyJSON(extra string) string {
	return `{"fingerprint":"` + goodFP + `"` + extra + `}`
}

func TestBody_TheCapsAre64MiBForACreateAnd64KiBEverywhereElse(t *testing.T) {
	if maxCreateBody != 64<<20 || maxSmallBody != 64<<10 || maxCSVQuery != 64<<10 {
		t.Fatalf("the caps are %d, %d and %d bytes, the documented ones are 64 MiB, 64 KiB and 64 KiB", maxCreateBody, maxSmallBody, maxCSVQuery)
	}
	r := newRig(t)
	tok := r.token(loginA)

	// a small route: a confirm just under the cap is read (and refused only for what it says), one byte over is a 413 before it is decoded
	pad := func(n int) string { return confirmBodyJSON(`,"scheduled_start":"` + strings.Repeat("x", n) + `"`) }
	under := pad(int(maxSmallBody) - len(pad(0)))
	over := pad(int(maxSmallBody) - len(pad(0)) + 1)
	if len(under) != int(maxSmallBody) || len(over) != int(maxSmallBody)+1 {
		t.Fatalf("the bodies are %d and %d bytes", len(under), len(over))
	}
	h := http.Header{"Idempotency-Key": {"key-1"}}
	resp, body := r.do(req{method: "POST", path: "/v1/jobs/" + jobIDA + "/confirm", token: tok, ctype: "application/json", body: []byte(under), header: h})
	r.wantError(resp, body, 400, "validation_failed") // read in full, and refused for its start time, not for its size
	resp, body = r.do(req{method: "POST", path: "/v1/jobs/" + jobIDA + "/confirm", token: tok, ctype: "application/json", stream: bytes.NewReader([]byte(over)), header: h})
	r.wantError(resp, body, 413, "payload_too_large")

	// a route with no body takes none beyond the cap either: a body of exactly the cap is drained and the route is served, one byte over is refused and the store is not asked
	r.store.reset()
	resp, body = r.do(req{method: "POST", path: "/v1/jobs/" + jobIDA + "/cancel", token: tok, body: bytes.Repeat([]byte("z"), int(maxSmallBody))})
	r.wantStatus(resp, body, 200)
	if len(r.store.seen()) != 1 {
		t.Fatalf("the cancel with a body of the cap's size did not reach the store: %v", r.store.seen())
	}
	r.store.reset()
	resp, body = r.do(req{method: "POST", path: "/v1/jobs/" + jobIDA + "/cancel", token: tok, stream: bytes.NewReader(bytes.Repeat([]byte("z"), int(maxSmallBody)+1))})
	r.wantError(resp, body, 413, "payload_too_large")
	if len(r.store.seen()) != 0 {
		t.Fatalf("a cancel with an over-long body reached the store")
	}

	// the create's cap, as the client shows it by sending more: a streamed body that never ends is cut at 64 MiB and answered 413
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		head := []byte(`{"module":"m1","rows":[{"email":"`)
		if _, err := pw.Write(head); err != nil {
			return
		}
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for i := 0; i < 70; i++ { // 70 MiB, 6 over the cap
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()
	hr, _ := http.NewRequest("POST", r.srv.URL+"/v1/jobs", pr)
	hr.Header.Set("Authorization", "Bearer "+tok)
	hr.Header.Set("Content-Type", "application/json")
	got, err := (&http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(hr)
	if err != nil {
		// the server may close the connection once it has answered: the answer is what is judged, and a reset before it is read is the same refusal
		t.Logf("the over-long create ended with a transport error (%v): the server stopped reading", err)
	} else {
		b, _ := io.ReadAll(got.Body)
		got.Body.Close()
		r.wantError(got, b, 413, "payload_too_large")
	}
	pr.CloseWithError(io.ErrClosedPipe)
	for _, c := range r.store.seen() {
		if c.Method == "CreateDraft" {
			t.Fatalf("the store was asked to create a job from a body over the cap")
		}
	}

	// the control: a body well under the cap is a create
	resp, body = r.post(loginA, "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test"}]}`)
	r.wantStatus(resp, body, 201)
}

// rawPost sends the headers of a POST that DECLARES a body of the given length and sends none of it, and returns the answer: what a server does with a declared length is judged before a byte
// of the body exists.
func rawPost(t *testing.T, addr, path, token, ctype string, declared int64, body []byte, extra ...string) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var b strings.Builder
	b.WriteString("POST " + path + " HTTP/1.1\r\nHost: " + addr + "\r\nAuthorization: Bearer " + token + "\r\nConnection: close\r\n")
	if ctype != "" {
		b.WriteString("Content-Type: " + ctype + "\r\n")
	}
	for _, h := range extra {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("Content-Length: " + strconv.FormatInt(declared, 10) + "\r\n\r\n")
	if _, err := conn.Write(append([]byte(b.String()), body...)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a declared length of %d: %v", declared, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestBody_ADeclaredLengthOverTheCapIsRefusedBeforeAnythingIsRead(t *testing.T) {
	r := newRig(t)
	addr := strings.TrimPrefix(r.srv.URL, "http://")
	tok := r.token(loginA)
	for _, c := range []struct {
		name, path, ctype string
		limit             int64
		extra             []string
	}{
		{"create", "/v1/jobs", "application/json", maxCreateBody, nil},
		{"create as CSV", "/v1/jobs?module=m1", "text/csv", maxCreateBody, nil},
		{"confirm", "/v1/jobs/" + jobIDA + "/confirm", "application/json", maxSmallBody, []string{"Idempotency-Key: key-1"}},
		{"cancel", "/v1/jobs/" + jobIDA + "/cancel", "", maxSmallBody, nil},
		{"preview", "/v1/jobs/" + jobIDA + "/preview", "", maxSmallBody, nil},
	} {
		for _, declared := range []int64{c.limit + 1, c.limit * 4} {
			resp := rawPost(t, addr, c.path, tok, c.ctype, declared, nil, c.extra...)
			b, _ := io.ReadAll(resp.Body)
			r.wantError(resp, b, 413, "payload_too_large")
		}
	}
	if n := len(r.store.seen()); n != 0 {
		t.Fatalf("the store was called %d time(s) for requests that declared too much", n)
	}

	// the control: the same routes with a declared length AT the cap and the bytes to match are read (a small route; the create's cap is the same code, and 64 MiB is not sent twice)
	full := bytes.Repeat([]byte(" "), int(maxSmallBody))
	resp := rawPost(t, addr, "/v1/jobs/"+jobIDA+"/cancel", tok, "", maxSmallBody, full)
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 {
		t.Fatalf("a body of exactly the cap was answered %d: %s", resp.StatusCode, b)
	}
}

func TestBody_UnknownMembersBadJSONAndTrailingDataAreRefusedAndReachNothing(t *testing.T) {
	r := newRig(t)
	create := `{"module":"m1","rows":[{"email":"a@example.test"}]}`
	h := http.Header{"Idempotency-Key": {"key-1"}}
	for _, c := range []struct {
		name, path, body string
		hdr              http.Header
		reason           string
	}{
		{"an unknown member of the create", "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test"}],"colour":"red"}`, nil, "the body has a member the API does not know: colour"},
		{"an unknown member of a row", "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test","nickname":"a"}]}`, nil, "nickname"},
		{"an unknown member of the content", "/v1/jobs", `{"module":"m2","rows":[{"email":"a@example.test"}],"content":{"subject":"s","html":"h","body":"x"}}`, nil, "body"},
		{"an unknown member of the settings", "/v1/jobs", `{"module":"m2","rows":[{"email":"a@example.test"}],"settings":{"unknown":"include"}}`, nil, "unknown"},
		{"an unknown member of the confirm", "/v1/jobs/" + jobIDA + "/confirm", confirmBodyJSON(`,"force":true`), h, "force"},
		{"an unknown member with an odd name", "/v1/jobs", `{"module":"m1","rows":[],"a b\n":1}`, nil, "the body has a member the API does not know"},
		{"a body that is not JSON", "/v1/jobs", `module=m1`, nil, "not valid JSON"},
		{"a truncated body", "/v1/jobs", `{"module":"m1","rows":[{"email":"a@exa`, nil, "ends too soon"},
		{"an empty body", "/v1/jobs", ``, nil, "the body is empty"},
		{"two JSON values", "/v1/jobs", create + create, nil, "after the JSON value"},
		{"data after the value", "/v1/jobs", create + ` x`, nil, "after the JSON value"},
		{"rows that are not a list", "/v1/jobs", `{"module":"m1","rows":"a@example.test"}`, nil, "rows has the wrong type"},
		{"a merge field that is a number", "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test","fields":{"age":31}}]}`, nil, "has the wrong type"},
		{"a confirm whose fingerprint is a number", "/v1/jobs/" + jobIDA + "/confirm", `{"fingerprint":1}`, h, "fingerprint has the wrong type"},
	} {
		r.store.reset()
		resp, body := r.do(req{method: "POST", path: c.path, token: r.token(loginA), ctype: "application/json", body: []byte(c.body), header: c.hdr})
		e := r.wantError(resp, body, 400, "bad_request")
		if got, _ := e.Details["reason"].(string); !strings.Contains(got, c.reason) {
			t.Errorf("%s: the reason is %q, want it to say %q", c.name, got, c.reason)
		}
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called", c.name)
		}
	}
	// the control: the well-formed twin of each is served
	resp, body := r.post(loginA, "/v1/jobs", create)
	r.wantStatus(resp, body, 201)
	resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(``), h)
	r.wantStatus(resp, body, 200)
	resp, body = r.post(loginA, "/v1/jobs", `{"module":"m2","rows":[{"email":"a@example.test","fields":{"age":"31"}}],"content":{"subject":"s","html":"h","text":"t","reply_to":""},"settings":{"unknown_choice":"exclude"}}`)
	r.wantStatus(resp, body, 201)
}

func TestBody_TheContentTypeIsEnforcedAndACharsetIsAccepted(t *testing.T) {
	r := newRig(t)
	good := `{"module":"m1","rows":[{"email":"a@example.test"}]}`
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/jsonx", "text/json", "multipart/form-data; boundary=x"} {
		resp, body := r.do(req{method: "POST", path: "/v1/jobs", token: r.token(loginA), ctype: ct, body: []byte(good)})
		r.wantError(resp, body, 415, "unsupported_media_type")
	}
	resp, body := r.do(req{method: "POST", path: "/v1/jobs/" + jobIDA + "/confirm", token: r.token(loginA), body: []byte(confirmBodyJSON(``)), header: http.Header{"Idempotency-Key": {"k"}}})
	r.wantError(resp, body, 415, "unsupported_media_type")
	if len(r.store.seen()) != 0 {
		t.Fatal("the store was called for a body of the wrong media type")
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON", "application/json;charset=UTF-8"} {
		resp, body := r.do(req{method: "POST", path: "/v1/jobs", token: r.token(loginA), ctype: ct, body: []byte(good)})
		r.wantStatus(resp, body, 201)
	}
}

func TestConfirm_NeedsExactlyOneIdempotencyKeyAndPassesTheRestToTheStore(t *testing.T) {
	r := newRig(t)
	path := "/v1/jobs/" + jobIDA + "/confirm"
	// no key, two keys: refused before the store
	resp, body := r.post(loginA, path, confirmBodyJSON(``))
	e := r.wantError(resp, body, 400, "validation_failed")
	if e.Details["reason"] != "the Idempotency-Key header is required" {
		t.Errorf("reason %v", e.Details)
	}
	resp, body = r.post(loginA, path, confirmBodyJSON(``), http.Header{"Idempotency-Key": {"k1", "k2"}})
	r.wantError(resp, body, 400, "validation_failed")
	if len(r.store.seen()) != 0 {
		t.Fatal("the store was called without exactly one key")
	}

	// one key: the store gets the job, the fingerprint and the key as sent; no start time is nil
	r.store.confirm.Replayed, r.store.confirm.Reserved, r.store.confirm.Pending, r.store.confirm.Suppressed = true, 120, 100, 3
	resp, body = r.post(loginA, path, confirmBodyJSON(``), http.Header{"Idempotency-Key": {"my-key.1:2"}})
	m := r.wantStatus(resp, body, 200)
	if m["replayed"] != true || m["reserved"] != float64(120) || m["pending"] != float64(100) || m["suppressed"] != float64(3) {
		t.Fatalf("the confirm answer is %v", m)
	}
	if _, ok := m["job"].(map[string]any); !ok {
		t.Fatalf("the confirm answer has no job: %v", m)
	}
	if got := r.store.confirmR; got.JobID != jobIDA || got.Fingerprint != goodFP || got.Key != "my-key.1:2" || got.ScheduledStart != nil {
		t.Fatalf("the store was asked %+v", got)
	}

	// a start time: an RFC 3339 instant, handed over in UTC whatever offset it was written with
	resp, body = r.post(loginA, path, confirmBodyJSON(`,"scheduled_start":"2030-01-02T05:30:00+02:00"`), http.Header{"Idempotency-Key": {"k3"}})
	r.wantStatus(resp, body, 200)
	if got := r.store.confirmR.ScheduledStart; got == nil || !got.Equal(time.Date(2030, 1, 2, 3, 30, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("the start time reached the store as %v", got)
	}
	for _, bad := range []string{`"tomorrow"`, `"2030-01-02"`, `"2030-01-02 05:30:00"`, `""`} {
		resp, body = r.post(loginA, path, confirmBodyJSON(`,"scheduled_start":`+bad), http.Header{"Idempotency-Key": {"k4"}})
		r.wantError(resp, body, 400, "validation_failed")
	}
}

func TestQuery_UnknownRepeatedAndOutOfRangeParametersAreRefused(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		path string
		code string
	}{
		{"/v1/jobs?foo=1", "bad_request"},
		{"/v1/jobs?limit=1&limit=2", "bad_request"},
		{"/v1/jobs?cursor=a&cursor=b", "bad_request"},
		{"/v1/jobs?limit=abc", "validation_failed"},
		{"/v1/jobs?limit=0", "validation_failed"},
		{"/v1/jobs?limit=-3", "validation_failed"},
		{"/v1/jobs?limit=101", "validation_failed"},
		{"/v1/jobs?limit=1.5", "validation_failed"},
		{"/v1/jobs?limit=99999999999999999999", "validation_failed"},
		{"/v1/jobs?%zz=1", "bad_request"},
		{"/v1/jobs/" + jobIDA + "?expand=content", "bad_request"},
		{"/v1/jobs/" + jobIDA + "/events?after=-1", "validation_failed"},
		{"/v1/jobs/" + jobIDA + "/events?limit=1001", "validation_failed"},
		{"/v1/jobs/" + jobIDA + "/events?since=1", "bad_request"},
	} {
		r.store.reset()
		resp, body := r.get(loginA, c.path)
		r.wantError(resp, body, 400, c.code)
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called", c.path)
		}
	}
	// the controls: the edges are served, and an empty value is an absent one
	for _, p := range []string{"/v1/jobs?limit=1", "/v1/jobs?limit=100", "/v1/jobs?cursor=&limit=", "/v1/jobs/" + jobIDA + "/events?after=0&limit=1000", "/v1/jobs/" + jobIDA + "/events?after=&limit="} {
		resp, body := r.get(loginA, p)
		r.wantStatus(resp, body, 200)
	}
	r.store.reset()
	r.get(loginA, "/v1/jobs?cursor=&limit=")
	if c := r.store.seen(); len(c) != 1 || c[0].Method != "List" || c[0].Arg.(jobs.ListOpts) != (jobs.ListOpts{}) {
		t.Fatalf("calls %+v", c)
	}
}

// The read deadline a route sets to take its body ends with the body. The server watches the connection of a request with no body left to read (that is how it learns that a client went away) and
// ends the request's context when that read fails; a deadline that outlived the body would end a slow store call (a confirm waiting for a lock) in the middle of it. The windows are shortened here and
// the store takes three times as long as the shortest.
func TestBody_TheReadDeadlineEndsWithTheBodyAndNeverCancelsASlowStoreCall(t *testing.T) {
	old := smallBodyWindow
	smallBodyWindow = 250 * time.Millisecond
	t.Cleanup(func() { smallBodyWindow = old })

	r := newRig(t)
	cancelled := make(chan bool, 1)
	r.store.moveHook = func(ctx context.Context) {
		time.Sleep(3 * smallBodyWindow) // the window is over long before the store answers
		cancelled <- ctx.Err() != nil
	}
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/cancel", nil)
	r.wantStatus(resp, body, 200)
	if <-cancelled {
		t.Fatal("the request's context was cancelled while the store was working: the read deadline outlived the body")
	}

	// the control: a deadline that is NOT cleared (what the handler would do without the deferred end of the window) does cancel the context of a request with no body, so the check above can fail
	stale := make(chan bool, 1)
	srv := httpServer(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(smallBodyWindow))
		time.Sleep(3 * smallBodyWindow)
		stale <- q.Context().Err() != nil
	}))
	hr, _ := http.NewRequest("POST", srv.URL, nil)
	if got, err := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(hr); err == nil {
		got.Body.Close()
	}
	if !<-stale {
		t.Fatal("a read deadline that was left set did not cancel the request: the control cannot tell the difference, so the check above proves nothing")
	}
}
