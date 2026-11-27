package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// POST /v1/templates/check (templatecheck.go; Phase 6 A3). The judgement itself is jobs.CheckCampaign, held to the save by internal/jobs/template_check_test.go (the first error is exactly the one the
// save's Prepare returns). What the API owns here: the shape of the two answers, that a template with a mistake is a 200 and not an error, that the request is judged as strictly as every other body, that it
// is behind the authentication, and that it touches no store.

func (r *rig) check(login string, body string) (*http.Response, []byte) {
	r.t.Helper()
	return r.post(login, "/v1/templates/check", body)
}

func TestTemplateCheck_ACampaignThatCompilesIsOKWithItsFields(t *testing.T) {
	r := newRig(t)
	resp, body := r.check(loginA, `{"subject":"Hi {{first_name|there}}","html":"<p>{{first_name}} at {{company}} {{email}} {{unsubscribe_url}}</p>","text":"{{company|your company}}"}`)
	m := r.wantStatus(resp, body, 200)
	if len(m) != 2 || m["ok"] != true {
		t.Fatalf("the answer is %s, want exactly {ok:true, fields:[...]}", body)
	}
	fields, _ := m["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields = %v, want first_name and company (the reserved names are not listed)", fields)
	}
	for i, want := range []struct {
		name string
		fb   bool
	}{{"first_name", false}, {"company", false}} {
		f, _ := fields[i].(map[string]any)
		if len(f) != 2 || f["name"] != want.name || f["has_fallback"] != want.fb {
			t.Errorf("field %d = %v, want {name:%s, has_fallback:%v}", i, f, want.name, want.fb)
		}
	}
	// a field with a fallback at every use says so
	resp, body = r.check(loginA, `{"subject":"{{a|x}}","html":"{{a|y}}"}`)
	if f, _ := r.wantStatus(resp, body, 200)["fields"].([]any); len(f) != 1 || f[0].(map[string]any)["has_fallback"] != true {
		t.Errorf("a field with a fallback at every use: %s", body)
	}
	// it is the principal's request only so far as it is authenticated: nothing of the store or of the account was read
	if n := len(r.store.seen()) + len(r.account.seen()); n != 0 {
		t.Fatalf("the check made %d call(s) to the stores: it must touch nothing", n)
	}
}

func TestTemplateCheck_AMistakeIsA200WithTheErrorsAndNotAFailure(t *testing.T) {
	r := newRig(t)
	resp, body := r.check(loginA, `{"subject":"Hi {{name","html":"<p>ok</p>\n<p>{only}</p>","text":"fine {{x}}"}`)
	m := r.wantStatus(resp, body, 200)
	if len(m) != 2 || m["ok"] != false {
		t.Fatalf("the answer is %s, want exactly {ok:false, errors:[...]}", body)
	}
	errs, _ := m["errors"].([]any)
	if len(errs) != 2 {
		t.Fatalf("errors = %v, want one for the subject and one for the html (the text is fine)", errs)
	}
	for i, part := range []string{"subject", "html"} {
		e, _ := errs[i].(map[string]any)
		if len(e) != 4 || e["part"] != part {
			t.Errorf("error %d = %v, want exactly part, line, column, message, for the %s", i, e, part)
			continue
		}
		if line, _ := e["line"].(float64); line < 1 {
			t.Errorf("error %d: line = %v", i, e["line"])
		}
		if col, _ := e["column"].(float64); col < 1 {
			t.Errorf("error %d: column = %v", i, e["column"])
		}
		if msg, _ := e["message"].(string); msg == "" {
			t.Errorf("error %d: no message", i)
		}
	}
	if e, _ := errs[1].(map[string]any); e["line"] != float64(2) {
		t.Errorf("the html's mistake is on line 2, the answer says %v", e["line"])
	}
	// the rule that is the store's and not the grammar's
	resp, body = r.check(loginA, `{"subject":"Hi","html":"{a|{{unsubscribe_url}}}"}`)
	m = r.wantStatus(resp, body, 200)
	if e, _ := m["errors"].([]any); m["ok"] != false || len(e) != 1 || !strings.Contains(e[0].(map[string]any)["message"].(string), "{{unsubscribe_url}}") {
		t.Errorf("an unsubscribe link inside a group: %s", body)
	}
	// the answer never carries the template back: the message quotes at most one character of what was written
	resp, body = r.check(loginA, `{"subject":"Hi","html":"<p>SECRETCANARY5531 {{bad name}} more SECRETCANARY5531</p>"}`)
	r.wantStatus(resp, body, 200)
	if strings.Contains(string(body), "SECRETCANARY5531") || strings.Contains(r.logs.String(), "SECRETCANARY5531") {
		t.Fatalf("the template reached the answer or the log: %s", body)
	}
}

func TestTemplateCheck_AnEmptyPartIsAllowed(t *testing.T) {
	r := newRig(t)
	for name, body := range map[string]string{
		"nothing":      `{}`,
		"all empty":    `{"subject":"","html":"","text":""}`,
		"only subject": `{"subject":"Hello"}`,
		"only text":    `{"text":"hi {{name}}"}`,
		"blanks":       `{"subject":"   ","html":" \n ","text":"\t"}`,
	} {
		resp, b := r.check(loginA, body)
		m := r.wantStatus(resp, b, 200)
		if m["ok"] != true {
			t.Errorf("%s: %s", name, b)
		}
		if f, isList := m["fields"].([]any); !isList {
			t.Errorf("%s: fields is %v, want a list (never null)", name, m["fields"])
		} else if name == "only text" && len(f) != 1 {
			t.Errorf("%s: fields = %v", name, f)
		}
	}
}

func TestTemplateCheck_TheSavesSizeAndTextRulesAreThe400OfEveryRoute(t *testing.T) {
	r := newRig(t)
	for name, body := range map[string]string{
		"a subject over 998 bytes": `{"subject":"` + strings.Repeat("a", 999) + `","html":"<p>x</p>"}`,
		"a subject on two lines":   `{"subject":"one\ntwo","html":"<p>x</p>"}`,
		"a NUL in the html":        `{"subject":"Hi","html":"<p>\u0000</p>"}`,
		"an html over 1 MiB":       `{"subject":"Hi","html":"` + strings.Repeat("a", (1<<20)+1) + `"}`,
	} {
		resp, b := r.check(loginA, body)
		e := r.wantError(resp, b, http.StatusBadRequest, "validation_failed")
		if reason, _ := e.Details["reason"].(string); reason == "" || strings.Contains(reason, "aaaa") {
			t.Errorf("%s: the reason is %q, want a sentence that quotes nothing", name, reason)
		}
	}
	// the twins: the edges themselves are accepted
	for name, body := range map[string]string{
		"a subject of 998 bytes": `{"subject":"` + strings.Repeat("a", 998) + `","html":"<p>x</p>"}`,
		"an html of 1 MiB":       `{"subject":"Hi","html":"` + strings.Repeat("a", 1<<20) + `"}`,
	} {
		if resp, b := r.check(loginA, body); resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b[:min(len(b), 200)])
		}
	}
}

func TestTemplateCheck_TheRequestIsJudgedAsStrictlyAsEveryOtherBody(t *testing.T) {
	r := newRig(t)
	tok := r.token(loginA)
	for name, c := range map[string]struct {
		q      req
		status int
		code   string
	}{
		"an unknown member":      {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "application/json", body: []byte(`{"subject":"a","reply_to":"x@example.test"}`)}, 400, "bad_request"},
		"a user":                 {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "application/json", body: []byte(`{"user_id":"` + userB + `"}`)}, 400, "bad_request"},
		"a member of a bad type": {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "application/json", body: []byte(`{"subject":5}`)}, 400, "bad_request"},
		"not JSON":               {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "application/json", body: []byte(`subject=a`)}, 400, "bad_request"},
		"an empty body":          {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "application/json", body: []byte(``)}, 400, "bad_request"},
		"data after the object":  {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "application/json", body: []byte(`{} {}`)}, 400, "bad_request"},
		"a query":                {req{method: http.MethodPost, path: "/v1/templates/check?user_id=" + userB, token: tok, ctype: "application/json", body: []byte(`{}`)}, 400, "bad_request"},
		"another content type":   {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, ctype: "text/plain", body: []byte(`{}`)}, 415, "unsupported_media_type"},
		"no content type":        {req{method: http.MethodPost, path: "/v1/templates/check", token: tok, body: []byte(`{}`)}, 415, "unsupported_media_type"},
	} {
		resp, body := r.do(c.q)
		r.wantError(resp, body, c.status, c.code)
		if n := len(r.store.seen()) + len(r.account.seen()); n != 0 {
			t.Fatalf("%s: a store was called", name)
		}
	}
	// a body over the cap is a 413, judged by the DECLARED length before a byte is read: the request below declares one byte over the cap and sends none of it, so the answer can only come from the header
	conn, err := net.Dial("tcp", r.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "POST /v1/templates/check HTTP/1.1\r\nHost: platform\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", tok, maxCheckBody+1)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a request that declares a body over the cap: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	r.wantError(resp, b, http.StatusRequestEntityTooLarge, "payload_too_large")
	// the twin of every refusal above: the same route with a good body
	if resp, _ := r.check(loginA, `{"subject":"a"}`); resp.StatusCode != 200 {
		t.Fatalf("control failed: %d", resp.StatusCode)
	}
	// only POST
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		resp, body := r.do(req{method: m, path: "/v1/templates/check", token: tok})
		r.wantError(resp, body, http.StatusMethodNotAllowed, "method_not_allowed")
		if !strings.Contains(resp.Header.Get("Allow"), "POST") {
			t.Errorf("%s: Allow = %q", m, resp.Header.Get("Allow"))
		}
	}
}

func TestTemplateCheck_IsBehindTheAuthenticationLikeEveryRoute(t *testing.T) {
	r := newRig(t)
	for name, q := range map[string]req{
		"no token":          {method: http.MethodPost, path: "/v1/templates/check", ctype: "application/json", body: []byte(`{"subject":"a"}`)},
		"a forged token":    {method: http.MethodPost, path: "/v1/templates/check", token: r.mint.Forged(r.mint.Good(loginA)), ctype: "application/json", body: []byte(`{"subject":"a"}`)},
		"an unknown login":  {method: http.MethodPost, path: "/v1/templates/check", token: r.token("c3c3c3c3-0000-4000-8000-0000000000cc"), ctype: "application/json", body: []byte(`{"subject":"a"}`)},
		"a bad body, still": {method: http.MethodPost, path: "/v1/templates/check", ctype: "application/json", body: []byte(`not json`)},
	} {
		resp, body := r.do(q)
		r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
		if strings.Contains(string(body), "fields") || strings.Contains(string(body), "errors") {
			t.Errorf("%s: the answer of a refused login says something about the template: %s", name, body)
		}
	}
	if resp, _ := r.check(loginA, `{"subject":"a"}`); resp.StatusCode != 200 { // the twin
		t.Fatalf("control failed: %d", resp.StatusCode)
	}
}
