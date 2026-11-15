package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Service is one thing the probe calls. Name is the prefix of its check ids.
type Service struct {
	Name       string
	BaseURL    string
	Credential Secret // module 1: the caller key; modules 2 and 3: the service token; the platform: unused
}

type credential int

const (
	credNone       credential = iota // no Authorization header at all
	credConfigured                   // the configured key or token
	credWrong                        // a bearer value that no service can have issued
)

// wrongCredential is what the negative checks send: a bearer value no service can have issued. It is not a
// secret and is the same on every run (and deliberately low in entropy, so the secret scanner is not
// asked to judge it).
const wrongCredential = "zzzz-not-issued-zzzz"

func (c credential) label() string {
	switch c {
	case credConfigured:
		return "configured"
	case credWrong:
		return "wrong"
	}
	return "none"
}

// exchange is the outcome of one GET. When failure is not empty the request produced no usable response
// (a network error, a followed redirect, an oversized body) and every check on it fails with that text.
type exchange struct {
	status   int
	body     map[string]any // the body as a JSON object, nil when it is not one
	bodyNote string         // why body is nil
	failure  string
}

// get makes one GET to svc at path with the given credential, prints the request line and returns what
// came back. The URL's userinfo is dropped (it would otherwise be sent as Basic auth, which would turn
// a "no credential" request into an authenticated one).
func (r *Recorder) get(ctx context.Context, d Doer, svc Service, path string, cred credential) exchange {
	u, err := url.Parse(svc.BaseURL)
	if err != nil {
		return exchange{failure: "the base URL is not valid"}
	}
	u.User = nil
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	u.Path = strings.TrimRight(u.Path, "/") + path
	target := u.Scheme + "://" + u.Host + u.Path

	r.request(http.MethodGet, target, cred.label())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return exchange{failure: "the request could not be built"}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "platform-probe")
	switch cred {
	case credConfigured:
		req.Header.Set("Authorization", "Bearer "+svc.Credential.Reveal())
	case credWrong:
		req.Header.Set("Authorization", "Bearer "+wrongCredential)
	}

	resp, err := d.Do(req)
	if err != nil {
		return exchange{failure: "request failed: " + classify(err)}
	}
	defer resp.Body.Close()
	// A doer that follows redirects hides one from the status code; the response then comes from
	// somewhere other than where it was asked.
	if resp.Request != nil && resp.Request.URL != nil &&
		(resp.Request.URL.Host != u.Host || resp.Request.URL.Path != u.Path || resp.Request.URL.RawQuery != "") {
		return exchange{failure: "a redirect was followed: the response did not come from the requested URL"}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return exchange{failure: "reading the response body failed: " + classify(err)}
	}
	if len(b) > MaxBodyBytes {
		return exchange{failure: "the response body is larger than 1 MiB"}
	}
	ex := exchange{status: resp.StatusCode}
	ex.body, ex.bodyNote = decodeObject(b)
	return ex
}

// decodeObject parses b as one JSON object. Numbers stay json.Number so integers survive.
func decodeObject(b []byte) (map[string]any, string) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, "an empty body"
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "a body that is not JSON"
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "a body with data after the JSON value"
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, "a JSON value that is not an object"
	}
	return m, ""
}

// ---- assertions ------------------------------------------------------------------------------------------------

// problems collects what is wrong with one response, so one check reports all of it at once.
type problems []string

func (p *problems) add(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

// verdict records the check: a pass when nothing is wrong, otherwise a fail with every problem.
func (p problems) verdict(r *Recorder, id string, kind Kind) {
	if len(p) == 0 {
		r.Pass(id, kind)
		return
	}
	r.Fail(id, kind, strings.Join(p, "; "))
}

// usable reports a response that cannot be asserted on. A check that cannot run FAILS, with the reason.
func (ex exchange) usable(r *Recorder, id string, kind Kind) bool {
	if ex.failure == "" {
		return true
	}
	r.Fail(id, kind, ex.failure)
	return false
}

func (ex exchange) wantStatus(p *problems, want int, note string) {
	if ex.status == want {
		return
	}
	msg := fmt.Sprintf("expected status %d, got %d", want, ex.status)
	switch {
	case ex.status >= 300 && ex.status < 400:
		msg += " (a redirect; redirects are not followed)"
	case note != "":
		msg += " (" + note + ")"
	}
	p.add("%s", msg)
}

func (ex exchange) wantObject(p *problems) bool {
	if ex.body != nil {
		return true
	}
	p.add("expected a JSON object body, got %s", ex.bodyNote)
	return false
}

// lookup follows a dotted path through nested objects.
func lookup(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, key := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// show renders a value for a failure reason: its kind, and for a scalar the value. Never a whole body.
//
// Nothing is clipped here, on purpose. A clipped value could cut a configured secret in the middle and
// leave a prefix of it that the Recorder's scrub no longer recognises. The Recorder scrubs the whole line
// first and caps its length after, so the line length is what bounds the output.
func show(v any, found bool) string {
	if !found {
		return "<missing>"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", x)
	case bool:
		return fmt.Sprint(x)
	case json.Number:
		return x.String()
	case []any:
		return fmt.Sprintf("an array of %d", len(x))
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("a %T", v)
}

func (p *problems) wantString(m map[string]any, path, want string) {
	v, found := lookup(m, path)
	if s, ok := v.(string); !ok || s != want {
		p.add("expected %s=%q, got %s", path, want, show(v, found))
	}
}

func (p *problems) wantNonEmptyString(m map[string]any, path string) {
	v, found := lookup(m, path)
	if s, ok := v.(string); !ok || s == "" {
		p.add("expected %s to be a non-empty string, got %s", path, show(v, found))
	}
}

func (p *problems) wantTrue(m map[string]any, path string) {
	v, found := lookup(m, path)
	if b, ok := v.(bool); !ok || !b {
		p.add("expected %s=true, got %s", path, show(v, found))
	}
}

func (p *problems) wantObjectAt(m map[string]any, path string) {
	v, found := lookup(m, path)
	if _, ok := v.(map[string]any); !ok {
		p.add("expected %s to be an object, got %s", path, show(v, found))
	}
}

func (p *problems) wantNonEmptyArray(m map[string]any, path string) {
	v, found := lookup(m, path)
	if a, ok := v.([]any); !ok || len(a) == 0 {
		p.add("expected %s to be a non-empty array, got %s", path, show(v, found))
	}
}

// notTrueChecks names the entries of the response's "checks" object that are not true, for the reason
// text of a readiness failure: which dependency, never a value. The names come from the response, so
// they are filtered to a safe alphabet and at most five are listed.
func notTrueChecks(m map[string]any) string {
	obj, ok := m["checks"].(map[string]any)
	if !ok {
		return ""
	}
	var names []string
	for k, v := range obj {
		if b, ok := v.(bool); ok && b {
			continue
		}
		clean := strings.Map(func(c rune) rune {
			if c == '_' || c == '-' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
				return c
			}
			return '?'
		}, k)
		names = append(names, clean)
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = names[:5]
	}
	return strings.Join(names, ",")
}
