//go:build harness

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The API half of the scenario: tokens, requests, the list a job is made of, and the commands that create and confirm a job through the platform's HTTP API (Docs/jobs-api.md). The platform is the
// process on the other end of the request (a jobs-a or jobs-b instance): the scenario holds none of its code.

// ---- the access token ---------------------------------------------------------------------------------------------------------------------------------------------------------------------

// signer mints the access token of a user the way Supabase's GoTrue does for the platform's HS256 configuration: header {"alg":"HS256","typ":"JWT"}, the claims of a signed-in user (sub, iss, aud, exp,
// iat, role), signed with the project's secret, which is the one the instances were given (PLATFORM_JWT_SECRET). It exists because internal/auth/authtest mints under a secret of its own making and
// takes a *testing.T; this program is the harness's caller, not a test. The secret stays inside this value: it is never printed, and a token never leaves a request header.
type signer struct {
	signing  []byte
	issuer   string
	audience string
}

func newSigner() (*signer, error) {
	secret, err := env("JOBS_JWT_SECRET")
	if err != nil {
		return nil, err
	}
	if len(secret) < 32 {
		return nil, failf(exitFail, "the JWT secret of the scenario is shorter than the 32 bytes the platform requires")
	}
	issuer, err := env("JOBS_JWT_ISSUER")
	if err != nil {
		return nil, err
	}
	return &signer{signing: []byte(secret), issuer: issuer, audience: envOr("JOBS_JWT_AUDIENCE", "authenticated")}, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mint returns the access token of the platform user (the login's id is the user's id: newUser makes both the same).
func (s *signer) mint(user string, now time.Time) string {
	hdr := b64url(mustJSON(map[string]string{"alg": "HS256", "typ": "JWT"}))
	claims := b64url(mustJSON(map[string]any{
		"sub": user, "iss": s.issuer, "aud": s.audience, "role": "authenticated", "email": "jobs-scenario@example.test",
		"iat": now.Unix() - 10, "exp": now.Unix() + 3600,
	}))
	mac := hmac.New(sha256.New, s.signing)
	mac.Write([]byte(hdr + "." + claims))
	return hdr + "." + claims + "." + b64url(mac.Sum(nil))
}

// ---- requests -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// httpClient makes every request on a connection of its own: a keep-alive connection to an instance that has since been killed and replaced would turn the first request after a restart into a
// confusing reset.
func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}}
}

// answer is the platform's HTTP answer: the status, the body and, for an error, the code of the envelope and the request id.
type answer struct {
	Status    int
	Body      []byte
	ErrorCode string
	RequestID string
}

// transportError is a request that got no HTTP answer at all: a refused or reset connection, an end of file, a timeout. It is what a killed process looks like from outside.
type transportError struct{ err error }

func (e *transportError) Error() string { return "no HTTP answer: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// call sends one request. A body that is not nil is JSON. The access token goes in the Authorization header and nowhere else.
func call(ctx context.Context, hc *http.Client, base, method, path, access string, headers map[string]string, body any) (answer, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return answer{}, failf(exitFail, "cannot build the request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return answer{}, &transportError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return answer{}, &transportError{err: err} // the answer began and did not end: the connection was cut
	}
	a := answer{Status: resp.StatusCode, Body: raw, RequestID: resp.Header.Get("X-Request-Id")}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		a.ErrorCode = e.Error.Code
	}
	return a, nil
}

// jobBody is {"job": {...}} as the API answers a create, a preview and a confirm: the id, the status, the fingerprint and the preview's counts.
type jobBody struct {
	Job struct {
		ID                 string `json:"id"`
		Status             string `json:"status"`
		PreviewFingerprint string `json:"preview_fingerprint"`
		Preview            *struct {
			Rows    int   `json:"rows"`
			Pending int   `json:"pending"`
			Credits int64 `json:"credits"`
		} `json:"preview"`
	} `json:"job"`
}

// refused turns an answer that is not the success the caller needs into the failure that names it (the status and the envelope's code, never the body).
func refused(what string, a answer) error {
	return failf(exitRefused, "%s: the platform answered %d %s (request %s)", what, a.Status, orDash(a.ErrorCode), orDash(a.RequestID))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- the list a job is made of ------------------------------------------------------------------------------------------------------------------------------------------------------------

// The addresses are at documentation-reserved names (.test, RFC 2606). Four domains have no record at all (the fixture DNS of the scope answers NXDOMAIN, module 1 says invalid/no_mx: a charged answer) and
// one has an MX (module 1, wired to no SMTP node, says risky/plausible with the skip token smtp_unwired: a free answer), so that a job holds both kinds of outcome.
const freeDomain = "free.example.test"

func domainFor(g int) string {
	if g%5 == 4 {
		return freeDomain
	}
	return fmt.Sprintf("d%d.example.test", g%4+1)
}

// genRows makes the n lines of a list: n-bad-dup distinct good addresses, bad malformed lines and dup repeats of an earlier good address in another spelling (upper case: the platform normalises it to the
// same address, so it is a duplicate, free and never sent). The malformed and duplicate lines are spread through the list, not grouped at an end. It is deterministic.
func genRows(n, bad, dup int) ([]string, error) {
	return genRowsWith(n, bad, dup, func(i int) string { return fmt.Sprintf("user%05d@%s", i, domainFor(i)) }, "")
}

// genRowsWith is genRows with the good addresses of the caller (good(i) is the i-th distinct good address, lower case) and a mark that begins every malformed line (a send job's lines carry the
// job's recipient prefix, so that a malformed line that reached the sink would be seen as this job's).
func genRowsWith(n, bad, dup int, goodAt func(i int) string, mark string) ([]string, error) {
	good := n - bad - dup
	switch {
	case n < 1 || bad < 0 || dup < 0:
		return nil, failf(exitUsage, "rows must be at least 1 and bad and dup at least 0: got %d, %d, %d", n, bad, dup)
	case good < 1:
		return nil, failf(exitUsage, "a list of %d lines with %d malformed and %d repeated has no good address left to send", n, bad, dup)
	}
	kinds := make([]byte, n)
	for i := range kinds {
		kinds[i] = 'g'
	}
	place := func(count int, kind byte, first, minIdx int) error {
		if count == 0 {
			return nil
		}
		stride := n / (count + 1)
		if stride < 1 {
			stride = 1
		}
		for k := 0; k < count; k++ {
			idx := (first + k*stride) % n
			for tries := 0; tries < n; tries++ {
				if kinds[idx] == 'g' && idx >= minIdx {
					break
				}
				idx = (idx + 1) % n
			}
			if kinds[idx] != 'g' || idx < minIdx {
				return failf(exitUsage, "no room in a list of %d lines for %d lines of kind %c", n, count, kind)
			}
			kinds[idx] = kind
		}
		return nil
	}
	if err := place(bad, 'b', 3, 0); err != nil {
		return nil, err
	}
	if err := place(dup, 'd', 5, 2); err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	var goods []string
	badN, dupN := 0, 0
	for _, k := range kinds {
		switch k {
		case 'g':
			a := goodAt(len(goods))
			goods = append(goods, a)
			out = append(out, a)
		case 'b':
			switch badN % 4 {
			case 0:
				out = append(out, fmt.Sprintf("%sno-at-sign-%03d.example.test", mark, badN))
			case 1:
				out = append(out, fmt.Sprintf("%stwo@@signs-%03d.example.test", mark, badN))
			case 2:
				out = append(out, fmt.Sprintf("@%sno-local-part-%03d.example.test", mark, badN))
			default:
				out = append(out, fmt.Sprintf("%snodot-%03d@localhost", mark, badN))
			}
			badN++
		case 'd':
			if len(goods) == 0 {
				return nil, failf(exitUsage, "a repeated line has no earlier address to repeat")
			}
			out = append(out, strings.ToUpper(goods[(dupN*7)%len(goods)]))
			dupN++
		}
	}
	return out, nil
}

// ---- create ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

type createOpts struct {
	api    string
	module string
	rows   int
	bad    int
	dup    int
	grant  int
	// the senders only (modules 2 and 3, send.go): how many good recipients the sink rejects at RCPT TO (550), how long it holds the final reply of every message to the job's recipients, and a
	// word naming the scenario in the campaign's subject
	bounce int
	holdMS int
	tag    string
	// stage J7 (optout.go): the user opts out of the vendor fallback before the job is created, and the good addresses of a module 1 list are routed to the vendor stage
	optOut    bool
	vendorTag string
}

// addSendFlags declares the flags of a send job on a create or a setup.
func addSendFlags(fs *flag.FlagSet, o *createOpts) {
	fs.IntVar(&o.bounce, "bounce", 0, "a send job: how many of the good recipients the controllable sink rejects at RCPT TO with 550 (module 2 and 3 report them bounced)")
	fs.IntVar(&o.holdMS, "hold-ms", 0, "a send job: milliseconds the controllable sink holds the final reply of every message to the job's recipients (so that a send is in flight on demand)")
	fs.StringVar(&o.tag, "tag", "", "a send job: a word naming the scenario (in the campaign's subject)")
}

// createJob makes a fresh user with credits, then creates and previews a draft of the list through the API, and prints the facts. It returns the job and the user.
func createJob(ctx context.Context, d *db, sg *signer, o createOpts) (job, user string, err error) {
	switch o.module {
	case "m1":
	case "m2", "m3":
		return createSendJob(ctx, d, sg, o) // send.go: an account at the module, the sink's mode, the campaign's content
	default:
		return "", "", failf(exitUsage, "module %q is none of m1, m2, m3", o.module)
	}
	lines, err := genRows(o.rows, o.bad, o.dup)
	if o.vendorTag != "" {
		lines, err = vendorLines(o) // stage J7: the good addresses are routed to the vendor stage (optout.go)
	}
	if err != nil {
		return "", "", err
	}
	credits := o.grant
	if credits <= 0 {
		credits = 10*o.rows + 1000 // far above any reserve (1 credit a row, or 2 with the vendor fallback reachable)
	}
	user, err = d.newUser(ctx, "scn")
	if err != nil {
		return "", "", err
	}
	if err := d.grant(ctx, user, credits); err != nil {
		return "", user, err
	}
	if o.optOut { // the setting is stated BEFORE the job exists: the job reads it when it is created
		if err := stateOptOut(ctx, sg, o.api, user); err != nil {
			return "", user, err
		}
	}
	job, prev, err := apiDraft(ctx, sg, o, user, lines, nil)
	if err != nil {
		return job, user, err
	}
	p := prev.Job.Preview
	fact("created", "user", user, "job", job, "rows", o.rows, "malformed", o.bad, "duplicates", o.dup, "pending", p.Pending, "credits", p.Credits, "granted", credits, "fingerprint", prev.Job.PreviewFingerprint)
	return job, user, nil
}

// apiDraft creates the draft of the list through the API as the user (with the campaign's content for a send job), previews it, and requires the preview to classify the list as the scenario built
// it (rows, and pending = rows - malformed - repeats). It returns the job and the preview's answer.
func apiDraft(ctx context.Context, sg *signer, o createOpts, user string, lines []string, content map[string]string) (string, jobBody, error) {
	hc := httpClient(2 * time.Minute)
	tok := sg.mint(user, time.Now())
	items := make([]map[string]string, len(lines))
	for i, l := range lines {
		items[i] = map[string]string{"email": l}
	}
	body := map[string]any{"module": o.module, "rows": items}
	if content != nil {
		body["content"] = content
	}
	a, err := call(ctx, hc, o.api, http.MethodPost, "/v1/jobs", tok, nil, body)
	if err != nil {
		return "", jobBody{}, err
	}
	if a.Status != http.StatusCreated {
		return "", jobBody{}, refused("creating the draft", a)
	}
	var created jobBody
	if err := json.Unmarshal(a.Body, &created); err != nil || !isUUID(created.Job.ID) {
		return "", jobBody{}, failf(exitFail, "the platform's answer to the create is not a job (status %d, request %s)", a.Status, orDash(a.RequestID))
	}
	job := created.Job.ID
	a, err = call(ctx, hc, o.api, http.MethodPost, "/v1/jobs/"+job+"/preview", tok, nil, nil)
	if err != nil {
		return job, jobBody{}, err
	}
	if a.Status != http.StatusOK {
		return job, jobBody{}, refused("previewing the draft", a)
	}
	var prev jobBody
	if err := json.Unmarshal(a.Body, &prev); err != nil || prev.Job.Status != "previewed" || prev.Job.Preview == nil || len(prev.Job.PreviewFingerprint) != 64 {
		return job, jobBody{}, failf(exitFail, "the platform's answer to the preview is not a previewed job with a fingerprint (status %d, request %s)", a.Status, orDash(a.RequestID))
	}
	p := prev.Job.Preview
	wantPending := o.rows - o.bad - o.dup
	if p.Rows != o.rows || p.Pending != wantPending {
		return job, jobBody{}, failf(exitFail, "the preview counts %d row(s) of which %d pending, the list has %d lines of which %d are good addresses: the platform classified the list differently from the scenario", p.Rows, p.Pending, o.rows, wantPending)
	}
	return job, prev, nil
}

func cmdCreate(args []string) error {
	fs := flags("create")
	var o createOpts
	fs.StringVar(&o.api, "api", "http://jobs-a:8080", "the platform instance to call")
	fs.StringVar(&o.module, "module", "m1", "the module (m1, m2 or m3)")
	fs.IntVar(&o.rows, "rows", 0, "lines in the list")
	fs.IntVar(&o.bad, "bad", 0, "of which malformed")
	fs.IntVar(&o.dup, "dup", 0, "of which repeats of an earlier address")
	fs.IntVar(&o.grant, "grant", 0, "credits to grant (default: far more than the job reserves)")
	addSendFlags(fs, &o)
	addOptOutFlags(fs, &o)
	if err := parse(fs, args); err != nil {
		return err
	}
	if o.rows < 1 {
		return failf(exitUsage, "--rows is required")
	}
	ctx, cancel := ctxFor(5 * time.Minute)
	defer cancel()
	sg, err := newSigner()
	if err != nil {
		return err
	}
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	_, _, err = createJob(ctx, d, sg, o)
	return err
}

// ---- confirm ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

type confirmOpts struct {
	api         string
	job         string
	ref         string
	scheduledIn int
	expect      string
	retry       bool
}

// confirmRef is the idempotency key of the scenario's confirm of a job when the caller gives none: one per job, so a retry is the same key.
func confirmRef(job string) string { return "jobs-" + job }

// confirmJob sends the confirm of a previewed job (the user, the fingerprint and the key come from the database and the job id) and judges the answer against what the caller expects:
//
//	ok           a 200: the job is confirmed (or the confirm is replayed, which the answer says)
//	no-response  NO HTTP answer at all: the process was killed inside the request, which is what the point confirm.before_commit does
//	refused:<c>  the platform REFUSED the confirm with the envelope's code <c> (a 4xx or 5xx whose error.code is exactly <c>): the stale preview of stage J6 is refused:preview_stale
func confirmJob(ctx context.Context, d *db, sg *signer, o confirmOpts) error {
	h, err := d.head(ctx, o.job)
	if err != nil {
		return err
	}
	if h.Fingerprint == "" {
		return failf(exitFail, "job %s has no preview fingerprint (status %s): it was never previewed", o.job, h.Status)
	}
	ref := o.ref
	if ref == "" {
		ref = confirmRef(o.job)
	}
	body := map[string]any{"fingerprint": h.Fingerprint}
	if o.scheduledIn > 0 {
		body["scheduled_start"] = time.Now().UTC().Add(time.Duration(o.scheduledIn) * time.Second).Format(time.RFC3339)
	}
	a, err := call(ctx, httpClient(90*time.Second), o.api, http.MethodPost, "/v1/jobs/"+o.job+"/confirm", sg.mint(h.User, time.Now()), map[string]string{"Idempotency-Key": ref}, body)
	var te *transportError
	switch {
	case errors.As(err, &te):
		fact("confirm", "job", o.job, "outcome", "no_response", "retry", o.retry, "error", clip(te.err.Error(), 160))
		if o.expect == "no-response" {
			return nil
		}
		return failf(exitNoResponse, "the confirm of job %s got no HTTP answer (%s): the instance is down or was killed", o.job, clip(te.err.Error(), 160))
	case err != nil:
		return err
	}
	if o.expect == "no-response" {
		fact("confirm", "job", o.job, "outcome", "answered", "status", a.Status, "retry", o.retry)
		return failf(exitFail, "the confirm of job %s was ANSWERED (%d): it was meant to be killed inside the request, so the fault point did not fire", o.job, a.Status)
	}
	if code, wantRefusal := strings.CutPrefix(o.expect, "refused:"); wantRefusal {
		fact("confirm", "job", o.job, "outcome", "refused", "status", a.Status, "code", orDash(a.ErrorCode), "retry", o.retry)
		if a.Status < 400 || a.ErrorCode != code {
			return failf(exitFail, "the confirm of job %s was answered %d %s, want a refusal with the code %s", o.job, a.Status, orDash(a.ErrorCode), code)
		}
		return nil
	}
	if a.Status != http.StatusOK {
		fact("confirm", "job", o.job, "outcome", "refused", "status", a.Status, "code", orDash(a.ErrorCode), "retry", o.retry)
		return refused("confirming the job", a)
	}
	var res struct {
		jobBody
		Replayed   bool  `json:"replayed"`
		Reserved   int64 `json:"reserved"`
		Pending    int   `json:"pending"`
		Suppressed int   `json:"suppressed"`
	}
	if err := json.Unmarshal(a.Body, &res); err != nil {
		return failf(exitFail, "the platform's answer to the confirm is not a confirm result (status %d, request %s)", a.Status, orDash(a.RequestID))
	}
	fact("confirm", "job", o.job, "outcome", "ok", "status", res.Job.Status, "replayed", res.Replayed, "reserved", res.Reserved, "pending", res.Pending, "suppressed", res.Suppressed, "retry", o.retry)
	return nil
}

func cmdConfirm(args []string, retry bool) error {
	name := "confirm"
	if retry {
		name = "confirm-retry"
	}
	fs := flags(name)
	o := confirmOpts{retry: retry}
	fs.StringVar(&o.api, "api", "http://jobs-a:8080", "the platform instance to call")
	fs.StringVar(&o.job, "job", "", "the previewed job")
	fs.StringVar(&o.ref, "key", "", "the idempotency key (default: one per job)")
	fs.IntVar(&o.scheduledIn, "scheduled-in", 0, "seconds ahead to schedule the start (at least 60), 0 for now")
	fs.StringVar(&o.expect, "expect", "ok", "ok, no-response (the process is killed inside the request) or refused:<code> (the platform refuses with that code)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if o.job == "" {
		return failf(exitUsage, "--job is required")
	}
	if o.expect != "ok" && o.expect != "no-response" && !strings.HasPrefix(o.expect, "refused:") {
		return failf(exitUsage, "--expect is ok, no-response or refused:<code>")
	}
	if strings.HasPrefix(o.expect, "refused:") && len(o.expect) == len("refused:") {
		return failf(exitUsage, "--expect refused: needs a code, for example refused:preview_stale")
	}
	if retry && o.expect != "ok" {
		return failf(exitUsage, "a confirm-retry must be answered: --expect ok")
	}
	ctx, cancel := ctxFor(3 * time.Minute)
	defer cancel()
	sg, err := newSigner()
	if err != nil {
		return err
	}
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	return confirmJob(ctx, d, sg, o)
}

func cmdSetup(args []string) error {
	fs := flags("setup")
	var c createOpts
	var scheduledIn int
	fs.StringVar(&c.api, "api", "http://jobs-a:8080", "the platform instance to call")
	fs.StringVar(&c.module, "module", "m1", "the module (m1, m2 or m3)")
	fs.IntVar(&c.rows, "rows", 0, "lines in the list")
	fs.IntVar(&c.bad, "bad", 0, "of which malformed")
	fs.IntVar(&c.dup, "dup", 0, "of which repeats of an earlier address")
	fs.IntVar(&c.grant, "grant", 0, "credits to grant")
	addSendFlags(fs, &c)
	addOptOutFlags(fs, &c)
	fs.IntVar(&scheduledIn, "scheduled-in", 0, "seconds ahead to schedule the start (at least 60), 0 for now")
	if err := parse(fs, args); err != nil {
		return err
	}
	if c.rows < 1 {
		return failf(exitUsage, "--rows is required")
	}
	ctx, cancel := ctxFor(8 * time.Minute)
	defer cancel()
	sg, err := newSigner()
	if err != nil {
		return err
	}
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	job, _, err := createJob(ctx, d, sg, c)
	if err != nil {
		return err
	}
	return confirmJob(ctx, d, sg, confirmOpts{api: c.api, job: job, scheduledIn: scheduledIn, expect: "ok"})
}
