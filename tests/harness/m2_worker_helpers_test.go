//go:build harness && m2worker

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/config"
)

// Helpers of the TestM2Worker_* tests (m2_worker_test.go). Every name starts with `m2w` so it cannot clash with another file's helpers; the file depends on helpers_test.go only (requireEnv, dial, envAdminURL,
// randomHex) and on nothing of the other live tests.
//
// WHAT THE TESTS DRIVE. Module 2 as the stack runs it: its Control, its REAL worker (run.sh stage 5c; docker-compose.m2worker.yml) and the controllable SMTP sink (tests/harness/sink) as the worker's
// SMTP target. The tests call module 2's own routes over HTTP: the platform surface (quote, run, report, cancel; the service token) and the operator surface (pools, accounts, config, health; the operator
// key). The sink is the independent witness: what it received, what it answered at RCPT TO and what it is holding. The superuser database connection SEEDS the platform's two tables (public.users and
// public.job_items, which module 2 reads) and, for evidence lines only, reads module 2's job rows; no assertion rests on a module-2 table.
//
// WHAT EACH TEST OWNS. One tenant (a user, a campaign, a dedicated pool, one account naming the sink's PRIVATE address) per scenario, so module 2's one-active-campaign-per-user rule never gets in the way.
// The module is shared by every test and ONE worker serves them all, one job at a time: the tests run one after another (no t.Parallel), the test that holds sends open runs last, and every test cancels
// what it started and waits for the sink to drain before it ends, so a failed test cannot leave a held send under the next one.
//
// THE DIALS. The global config of module 2 is changed for the run (m2wDials) and put back when the test ends: with the module's defaults a job that is deferred twice would take about half an hour.

const (
	m2wEnvURL      = "HARNESS_M2_URL"        // module 2's control: the platform surface and the operator surface on one port
	m2wEnvToken    = "HARNESS_M2_TOKEN"      // the platform's service token
	m2wEnvAdminKey = "HARNESS_M2_ADMIN_KEY"  // the operator key (a different credential)
	m2wEnvWorkerID = "HARNESS_M2_WORKER_ID"  // the worker's send_managed.boxes.id
	m2wEnvCtlURL   = "HARNESS_CTL_URL"       // the controllable sink's control API (also used by the module-3 tests)
	m2wEnvSinkPass = "HARNESS_SINK_PASSWORD" // the account's SMTP secret: the sink accepts any credential
	m2wCtlHost     = "smtp-ctl"              // the sink's service name: resolved to its private address, which is what the SMTP account names
	m2wSMTPPort    = 1025                    // STARTTLS port of the sink (module 2 derives the security mode from the port: 465 is implicit TLS, everything else STARTTLS)
	m2wNull        = "<null>"                // a JSON null in a report
	m2wAbsent      = "<absent>"              // a key that is not in the object
	m2wAny         = "<any>"                 // a field the scenario does not assert (printed in the evidence lines all the same)
	m2wPollEvery   = 1 * time.Second         // every call to module 2 costs a bcrypt verify: poll at seconds
	m2wHealthyBeat = 60                      // seconds: the worker's last heartbeat must be this recent
	m2wHoldMs      = 7000                    // how long the sink holds the final reply of a held send (module 2's operation timeout is 30 s: the hold must stay far under it)
	m2wHeldItems   = 5                       // items of a held campaign: one is in flight, the rest still queued when the cancel comes
	m2wOpTimeout   = 30 * time.Second        // module 2's smtp_operation_timeout_sec (seeded 30): the longest a worker waits for a reply
	m2wMaxPages    = 1000                    // a walk that has not ended after this many pages is a loop
	m2wDrainLimit  = 45 * time.Second        // how long a test's cleanup waits for the sink to stop holding sessions
	m2wTestBudget  = 12 * time.Minute        // the context of one test
	m2wStateQueued = "queued"
)

// m2wDial is one dial of module 2's global config (PUT /v1/admin/config/{key} with {"value": <inner>}; the module wraps it in {"v": ...}). Name, type and range are read from module 2's code at the pin:
//
//	job_max_failed_attempts       integer 1..1000 (config_validate.go; send_managed_0028): the Nth FAILED SMTP attempt ends the job as failed/attempts_exhausted. Default 6.
//	retry_backoff_schedule        json {"ladder_seconds":[positive ints, at least one],"max_seconds":positive int} (validateBackoffSchedule): the requeue delay of a transport_4xx, timeout or unknown
//	                              failure is ladder[attempt-1] capped at max_seconds (dispatch/config.go backoffForAttempt). Default [30,120,600] / 600.
//	send_delay_min_sec / _max_sec integer, no semantic validator (type check only): the per-lane cool-down after a send is uniform in [min,max]; a range with max <= min collapses to min (pacing.go
//	                              uniformDelay), so 0/0 is no pause. Default 3 / 7.
//	concurrency_per_user_default  integer, no semantic validator: a user's lanes. With ONE worker, more than one lane only makes the dispatcher claim jobs it cannot push and requeue them every tick
//	                              (loop.go: no_worker_available, which also writes last_error_class 'unknown'); one lane means a user has at most one job in flight and nothing is claimed while it is. Default 3.
//	dispatch_tick_interval_ms     integer, no semantic validator, read fresh at every wait (runner.go): how soon the next job is claimed after a state change. Default 1000.
//
// None of the six is in an ordering pair (configordering: smtp_operation_timeout_sec / job_max_processing_time_sec, box_outcome_buffer_ttl_sec / startup_reconnect_window_sec).
type m2wDial struct {
	Key   string
	Value string // the inner JSON value
}

var m2wDials = []m2wDial{
	{"job_max_failed_attempts", `2`},
	{"retry_backoff_schedule", `{"ladder_seconds":[1,1,1],"max_seconds":1}`},
	{"send_delay_min_sec", `0`},
	{"send_delay_max_sec", `0`},
	{"concurrency_per_user_default", `1`},
	{"dispatch_tick_interval_ms", `250`},
}

// ---- the sink's control API (tests/harness/sink) ----

// m2wMode is the body of POST /api/mode. The sink REPLACES its whole mode with what it is sent, so a zero m2wMode is "no hostility at all". reject_code is omitted when 0 (the sink's default is 550).
type m2wMode struct {
	RejectRcpt      []string `json:"reject_rcpt"`
	RejectCode      int      `json:"reject_code,omitempty"`
	DeferRcpt       []string `json:"defer_rcpt"`
	DataDelayMs     int      `json:"data_delay_ms"`
	DelayRecipients []string `json:"delay_recipients"`
	FailAuth        bool     `json:"fail_auth"`
}

type m2wSinkMsg struct {
	Seq       int64    `json:"seq"`
	To        []string `json:"to"`
	From      string   `json:"from"`
	Subject   string   `json:"subject"`
	MessageID string   `json:"message_id"`
	HeldMs    int64    `json:"held_ms"`
}

type m2wHeld struct {
	To          []string `json:"to"`
	AgeMs       int64    `json:"age_ms"`
	RemainingMs int64    `json:"remaining_ms"`
}

type m2wInflight struct {
	Inflight  int       `json:"inflight"`
	Peak      int       `json:"peak"`
	HeldTotal int       `json:"held_total"`
	Held      []m2wHeld `json:"held"`
}

// m2wRcpt is what the sink answered to one recipient at RCPT TO since its last reset.
type m2wRcpt struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Deferred int `json:"deferred"`
}

func (c m2wRcpt) tried() int { return c.Accepted + c.Rejected + c.Deferred }

// ---- the run ----

type m2wItem struct {
	Index int
	ID    string // public.job_items.id (the platform's own key; what module 2 calls job_item_id)
	Email string
	Kind  string // ok | bounce | defer | hold: what the sink is told to do with the address
	JobID string // module 2's job id, from the 202 of POST /v1/run
}

type m2wTenant struct {
	Name       string
	UserID     string
	CampaignID string
	PoolID     string
	AccountID  string
	From       string
	Items      []*m2wItem
	byID       map[string]*m2wItem
}

type m2wRun struct {
	ctx      context.Context
	hc       *http.Client
	m2URL    string
	ctlURL   string
	token    config.Secret
	adminKey config.Secret
	sinkPass config.Secret
	workerID string
	db       *pgx.Conn // a SUPERUSER: seeds public.users and public.job_items, and reads module 2's job rows for evidence lines; no assertion rests on it
	runID    string
	sinkIP   string // the controllable sink's private address: what the SMTP accounts name
	tenants  []*m2wTenant
	started  time.Time
}

func m2wBaseURL(t *testing.T, name string) string {
	t.Helper()
	raw := strings.TrimSpace(requireEnv(t, name))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		t.Fatalf("%s must be an http(s) URL with a host and no credentials", name)
	}
	return strings.TrimRight(raw, "/")
}

// m2wNewRun reads the environment, connects the superuser and finds the sink's private address. It changes nothing in module 2 or in the sink.
func m2wNewRun(t *testing.T) *m2wRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), m2wTestBudget)
	t.Cleanup(cancel)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // a credential must never be handed to a proxy
	tr.DisableKeepAlives = true
	r := &m2wRun{
		ctx:      ctx,
		m2URL:    m2wBaseURL(t, m2wEnvURL),
		ctlURL:   m2wBaseURL(t, m2wEnvCtlURL),
		token:    config.Secret(strings.TrimSpace(requireEnv(t, m2wEnvToken))),
		adminKey: config.Secret(strings.TrimSpace(requireEnv(t, m2wEnvAdminKey))),
		sinkPass: config.Secret(strings.TrimSpace(requireEnv(t, m2wEnvSinkPass))),
		workerID: strings.ToLower(strings.TrimSpace(requireEnv(t, m2wEnvWorkerID))),
		runID:    randomHex(t, 4),
		started:  time.Now(),
	}
	r.hc = &http.Client{
		Timeout:       30 * time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if r.token == r.adminKey || r.token == "" || r.adminKey == "" {
		t.Fatal("module 2's service token and operator key must both be set and must be two different credentials")
	}
	r.db = dial(t, requireEnv(t, envAdminURL), func(cfg *pgx.ConnConfig) { cfg.RuntimeParams["application_name"] = "m2-worker-test" })

	// the sink's private address: module 2's worker skips certificate validation only for `localhost`, `smtp-sink` and private addresses (services/worker/src/smtp.js isPrivateHost), and the controllable
	// sink's certificate is self-signed, so the account must name the ADDRESS (with the name `smtp-ctl` every send would fail starttls_failed, which the module classifies as `timeout`)
	addrs, err := net.DefaultResolver.LookupHost(ctx, m2wCtlHost)
	if err != nil {
		t.Fatalf("cannot resolve the controllable sink %q from the test container: %v", m2wCtlHost, err)
	}
	for _, a := range addrs {
		if m2wPrivateIPv4(a) {
			r.sinkIP = a
			break
		}
	}
	if r.sinkIP == "" {
		t.Fatalf("%s resolves to %v and not one of these is in 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16: module 2's worker validates a certificate unless the SMTP host is a private address, and the sink's is self-signed, so every send would fail", m2wCtlHost, addrs)
	}
	return r
}

// m2wPrivateIPv4 is true for the three private ranges module 2's worker treats as "a test/sink target on a private network" (and only for them skips certificate validation).
func m2wPrivateIPv4(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168)
}

// ---- HTTP ----

// scrub removes the three credentials of the run from any text that is about to be printed.
func (r *m2wRun) scrub(s string) string {
	for _, secret := range []string{r.token.Reveal(), r.adminKey.Reveal(), r.sinkPass.Reveal()} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// snip is a body shortened and scrubbed: what a failure message may quote.
func (r *m2wRun) snip(b []byte, limit int) string {
	s := string(b)
	if len(s) > limit {
		s = s[:limit] + "...(truncated)"
	}
	return r.scrub(s)
}

func m2wPathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// doCtx makes one request and returns the status and the body (at most 16 MiB) or the error. key is sent as a Bearer credential when it is not empty; body, when not nil, is sent as JSON.
func (r *m2wRun) doCtx(ctx context.Context, base, method, path string, key config.Secret, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("%s %s: cannot encode the request body: %v", method, m2wPathOnly(path), err)
		}
		payload = b
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: cannot build the request: %s", method, m2wPathOnly(path), r.scrub(err.Error()))
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key.Reveal())
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: transport error: %s", method, m2wPathOnly(path), r.scrub(err.Error()))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: reading the answer: %s", method, m2wPathOnly(path), r.scrub(err.Error()))
	}
	return resp.StatusCode, b, nil
}

// do is doCtx that FAILS the test on a transport error (a GET is tried three times first: the answer to a read cannot change anything).
func (r *m2wRun) do(t *testing.T, base, method, path string, key config.Secret, body any) (int, []byte) {
	t.Helper()
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-r.ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
		status, b, err := r.doCtx(r.ctx, base, method, path, key, body)
		if err == nil {
			return status, b
		}
		lastErr = err
	}
	t.Fatalf("%v", lastErr)
	return 0, nil
}

func (r *m2wRun) svc(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return r.do(t, r.m2URL, method, path, r.token, body)
}

func (r *m2wRun) admin(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return r.do(t, r.m2URL, method, path, r.adminKey, body)
}

// errCode is the `error.code` of module 2's error envelope {"error":{"code","message","details"?,"request_id"}}, "" when the body is not one.
func m2wErrCode(body []byte) string {
	var env struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || env.Error == nil {
		return ""
	}
	return env.Error.Code
}

// expect fails unless status is want and (when wantCode is not empty) the error envelope carries wantCode.
func (r *m2wRun) expect(t *testing.T, what string, status int, body []byte, want int, wantCode string) {
	t.Helper()
	if status != want {
		t.Fatalf("%s: HTTP %d (code %q), want %d: %s", what, status, m2wErrCode(body), want, r.snip(body, 600))
	}
	if wantCode != "" && m2wErrCode(body) != wantCode {
		t.Fatalf("%s: HTTP %d with error code %q, want %q: %s", what, status, m2wErrCode(body), wantCode, r.snip(body, 600))
	}
}

// ctlCall is one call to the sink's control API; a transport failure or an unexpected status FAILS the test with the sink's own words.
func (r *m2wRun) ctlCall(t *testing.T, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	status, out := r.do(t, r.ctlURL, method, path, "", body)
	if status != wantStatus {
		t.Fatalf("the controllable sink answered HTTP %d to %s %s, want %d: %s", status, method, path, wantStatus, r.snip(out, 400))
	}
	return out
}

// ---- the sink: mode, messages, counters, held sends ----

// setMode replaces the sink's mode and checks that the sink says it now is what was sent.
func (r *m2wRun) setMode(t *testing.T, m m2wMode) {
	t.Helper()
	if m.RejectRcpt == nil {
		m.RejectRcpt = []string{}
	}
	if m.DeferRcpt == nil {
		m.DeferRcpt = []string{}
	}
	if m.DelayRecipients == nil {
		m.DelayRecipients = []string{}
	}
	out := r.ctlCall(t, http.MethodPost, "/api/mode", m, http.StatusOK)
	var got m2wMode
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the sink's answer to POST /api/mode is not a mode (%v): %s", err, r.snip(out, 400))
	}
	wantCode := m.RejectCode
	if wantCode == 0 {
		wantCode = 550
	}
	norm := func(l []string) []string {
		c := append([]string{}, l...)
		sort.Strings(c)
		return c
	}
	if !reflect.DeepEqual(norm(got.RejectRcpt), norm(m.RejectRcpt)) || !reflect.DeepEqual(norm(got.DeferRcpt), norm(m.DeferRcpt)) || !reflect.DeepEqual(norm(got.DelayRecipients), norm(m.DelayRecipients)) ||
		got.DataDelayMs != m.DataDelayMs || got.FailAuth != m.FailAuth || got.RejectCode != wantCode {
		t.Fatalf("the sink did not take the mode it was sent: sent %d reject (code %d) / %d defer / delay %d ms for %d recipient(s) / fail_auth %v, the sink says %s",
			len(m.RejectRcpt), wantCode, len(m.DeferRcpt), m.DataDelayMs, len(m.DelayRecipients), m.FailAuth, r.snip(out, 400))
	}
}

func (r *m2wRun) inflight(t *testing.T) m2wInflight {
	t.Helper()
	out := r.ctlCall(t, http.MethodGet, "/api/inflight", nil, http.StatusOK)
	var v m2wInflight
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("the sink's /api/inflight is not the documented shape (%v): %s", err, r.snip(out, 400))
	}
	return v
}

// waitInflight waits until the sink holds at least n sessions (a send is in flight: the worker is at the end of DATA and the sink has not answered yet) and returns what it holds.
func (r *m2wRun) waitInflight(t *testing.T, n int, limit time.Duration) m2wInflight {
	t.Helper()
	start := time.Now()
	for {
		v := r.inflight(t)
		if v.Inflight >= n {
			return v
		}
		if time.Since(start) > limit {
			t.Fatalf("the sink held %d session(s) after %s, want at least %d: no send reached DATA (peak %d, held in all %d)\n%s", v.Inflight, time.Since(start).Round(time.Millisecond), n, v.Peak, v.HeldTotal, r.diagnostics(t, nil))
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("the test's context ended while waiting for the sink to hold a send: %v", r.ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// m2wView is the sink seen through this run's recipients (every address of this run starts with its prefix).
type m2wView struct {
	Msgs map[string][]m2wSinkMsg // exact lower-cased address -> messages the sink accepted
	Rcpt map[string]m2wRcpt      // exact lower-cased address -> what the sink answered at RCPT TO
}

func (v m2wView) messages(addr string) int { return len(v.Msgs[addr]) }

func (r *m2wRun) prefix() string { return "m2w-" + r.runID + "-" }

// known is the set of every recipient address this run seeded.
func (r *m2wRun) known() map[string]*m2wItem {
	k := map[string]*m2wItem{}
	for _, tn := range r.tenants {
		for _, it := range tn.Items {
			k[it.Email] = it
		}
	}
	return k
}

// sinkView reads the sink once and FAILS on what can never be true at any instant: an address carrying this run's prefix that was never one of its recipients, a message without a Message-ID, a
// Message-ID seen twice (one job sent twice). A recipient with more than one message is NOT failed here: the callers assert exact counts, and the failure must name the scenario.
func (r *m2wRun) sinkView(t *testing.T) m2wView {
	t.Helper()
	v := m2wView{Msgs: map[string][]m2wSinkMsg{}, Rcpt: map[string]m2wRcpt{}}
	known := r.known()
	out := r.ctlCall(t, http.MethodGet, "/api/messages", nil, http.StatusOK)
	var msgs []m2wSinkMsg
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("the sink's message list is not a JSON list of messages (%v): %s", err, r.snip(out, 400))
	}
	ids := map[string]string{}
	for _, m := range msgs {
		mine := false
		for _, raw := range m.To {
			a := strings.ToLower(strings.TrimSpace(raw))
			if !strings.HasPrefix(a, r.prefix()) {
				continue
			}
			mine = true
			if known[a] == nil {
				t.Fatalf("the sink holds a message to %s: it carries the prefix of this run but was never one of its recipients", a)
			}
			v.Msgs[a] = append(v.Msgs[a], m)
		}
		if !mine {
			continue
		}
		if m.MessageID == "" {
			t.Fatalf("the sink holds a message of this run without a Message-ID (to %v, subject %q): module 2 builds one for every send", m.To, m.Subject)
		}
		if prev, dup := ids[m.MessageID]; dup {
			t.Fatalf("the sink holds two messages with the Message-ID %s (to %s and to %v): one job was sent twice", m.MessageID, prev, m.To)
		}
		ids[m.MessageID] = strings.Join(m.To, ",")
	}
	out = r.ctlCall(t, http.MethodGet, "/api/rcpt", nil, http.StatusOK)
	var all map[string]m2wRcpt
	if err := json.Unmarshal(out, &all); err != nil {
		t.Fatalf("the sink's /api/rcpt is not a map of counters (%v): %s", err, r.snip(out, 400))
	}
	for a, c := range all {
		a = strings.ToLower(a)
		if !strings.HasPrefix(a, r.prefix()) {
			continue
		}
		if known[a] == nil {
			t.Fatalf("the sink answered RCPT TO for %s: it carries the prefix of this run but was never one of its recipients", a)
		}
		v.Rcpt[a] = c
	}
	return v
}

// noteSink prints the sink's view of every recipient of the tenant: the per-recipient counts the first-run review reads.
func (r *m2wRun) noteSink(t *testing.T, label string, tn *m2wTenant, v m2wView) {
	t.Helper()
	for _, it := range tn.Items {
		c := v.Rcpt[it.Email]
		t.Logf("NOTE sink %s %s %02d %-6s %s: RCPT accepted=%d rejected=%d deferred=%d, messages accepted=%d", label, tn.Name, it.Index, it.Kind, it.Email, c.Accepted, c.Rejected, c.Deferred, v.messages(it.Email))
	}
}

// ---- seeding: the platform's tables ----

// newTenant seeds, for real (committed, as the superuser), a user (auth.users and public.users) with a campaign of one job item per entry of kinds, and, when withAccount is set, creates through module 2's
// operator surface a dedicated pool owned by that user and one account whose smtp_host is the sink's private address. A kind is what the sink will be told to do with that recipient.
func (r *m2wRun) newTenant(t *testing.T, name string, kinds []string, withAccount bool) *m2wTenant {
	t.Helper()
	tn := &m2wTenant{Name: name, byID: map[string]*m2wItem{}}
	var ids [2]string
	for i := range ids {
		if err := r.db.QueryRow(r.ctx, "SELECT gen_random_uuid()::text").Scan(&ids[i]); err != nil {
			t.Fatalf("gen_random_uuid: %v", err)
		}
	}
	tn.UserID, tn.CampaignID = ids[0], ids[1]
	tn.From = fmt.Sprintf("sender-%s-%s@example.test", name, r.runID)
	email := fmt.Sprintf("m2w-user-%s-%s@example.test", name, r.runID)

	tx, err := r.db.Begin(r.ctx)
	if err != nil {
		t.Fatalf("begin the seeding transaction of %s: %v", name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.ctx)) }()
	if _, err := tx.Exec(r.ctx, "INSERT INTO auth.users (id, email) VALUES ($1::uuid, $2)", tn.UserID, email); err != nil {
		t.Fatalf("seed auth.users for %s: %v", name, err)
	}
	if _, err := tx.Exec(r.ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)", tn.UserID, email); err != nil {
		t.Fatalf("seed public.users for %s: %v", name, err)
	}
	counts := map[string]int{}
	for i, kind := range kinds {
		counts[kind]++
		it := &m2wItem{Index: i + 1, Kind: kind, Email: strings.ToLower(fmt.Sprintf("%s%s-%s%02d@example.test", r.prefix(), name, kind, counts[kind]))}
		err := tx.QueryRow(r.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`,
			tn.CampaignID, tn.UserID, it.Email, fmt.Sprintf("[m2w %s] %s %02d", r.runID, name, it.Index), fmt.Sprintf("<p>m2w %s %s %02d</p>", r.runID, name, it.Index)).Scan(&it.ID)
		if err != nil {
			t.Fatalf("seed public.job_items %d of %s: %v", i+1, name, err)
		}
		tn.Items = append(tn.Items, it)
		tn.byID[it.ID] = it
	}
	if err := tx.Commit(r.ctx); err != nil {
		t.Fatalf("commit the seeding transaction of %s: %v", name, err)
	}
	r.tenants = append(r.tenants, tn)

	if withAccount {
		r.createPoolAndAccount(t, tn)
	}
	return tn
}

// createPoolAndAccount: POST /v1/admin/pools (dedicated, owned by the tenant) and POST /v1/admin/accounts (smtp_host = the sink's private address, port 1025, the credential written through the API:
// module 2 encrypts it with its KMS key and the worker receives it decrypted, per send). Nothing is read back from the database.
func (r *m2wRun) createPoolAndAccount(t *testing.T, tn *m2wTenant) {
	t.Helper()
	status, body := r.admin(t, http.MethodPost, "/v1/admin/pools", map[string]any{"kind": "dedicated", "owner_user_id": tn.UserID, "name": fmt.Sprintf("m2w %s %s", tn.Name, r.runID)})
	r.expect(t, "POST /v1/admin/pools (a dedicated pool owned by the tenant)", status, body, http.StatusCreated, "")
	var pool struct {
		ID          string  `json:"id"`
		Kind        string  `json:"kind"`
		OwnerUserID *string `json:"owner_user_id"`
		Enabled     bool    `json:"enabled"`
	}
	if err := json.Unmarshal(body, &pool); err != nil || pool.ID == "" || pool.Kind != "dedicated" || pool.OwnerUserID == nil || *pool.OwnerUserID != tn.UserID || !pool.Enabled {
		t.Fatalf("POST /v1/admin/pools answered 201 with %s, want an enabled dedicated pool owned by %s", r.snip(body, 500), tn.UserID)
	}
	tn.PoolID = pool.ID

	status, body = r.admin(t, http.MethodPost, "/v1/admin/accounts", map[string]any{
		"pool_id":       tn.PoolID,
		"smtp_host":     r.sinkIP,
		"smtp_port":     m2wSMTPPort,
		"smtp_username": fmt.Sprintf("m2w-%s-%s", tn.Name, r.runID),
		"smtp_secret":   r.sinkPass.Reveal(),
		"from_address":  tn.From,
		"from_name":     "Harness",
		"daily_limit":   1000,
	})
	r.expect(t, "POST /v1/admin/accounts (an account naming the sink's private address)", status, body, http.StatusCreated, "")
	if strings.Contains(string(body), r.sinkPass.Reveal()) {
		t.Fatalf("POST /v1/admin/accounts echoed the SMTP secret: the credential is write-only (the answer is not shown)")
	}
	var acc struct {
		ID            string `json:"id"`
		UserID        string `json:"user_id"`
		PoolID        string `json:"pool_id"`
		SMTPHost      string `json:"smtp_host"`
		SMTPPort      int    `json:"smtp_port"`
		HasCredential bool   `json:"has_credential"`
		Status        struct {
			Paused      bool `json:"paused"`
			Quarantined bool `json:"quarantined"`
			Disabled    bool `json:"disabled"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &acc); err != nil || acc.ID == "" || acc.UserID != tn.UserID || acc.PoolID != tn.PoolID || acc.SMTPHost != r.sinkIP || acc.SMTPPort != m2wSMTPPort || !acc.HasCredential ||
		acc.Status.Paused || acc.Status.Quarantined || acc.Status.Disabled {
		t.Fatalf("POST /v1/admin/accounts answered 201 with %s, want a healthy account of user %s in pool %s at %s:%d holding a credential", r.snip(body, 600), tn.UserID, tn.PoolID, r.sinkIP, m2wSMTPPort)
	}
	tn.AccountID = acc.ID
}

// accountState reads the account back through the operator surface.
type m2wAccount struct {
	DailySentCount int `json:"daily_sent_count"`
	Score          int `json:"account_status_score"`
	Status         struct {
		Paused      bool `json:"paused"`
		Quarantined bool `json:"quarantined"`
		Disabled    bool `json:"disabled"`
	} `json:"status"`
}

func (r *m2wRun) account(t *testing.T, tn *m2wTenant) m2wAccount {
	t.Helper()
	status, body := r.admin(t, http.MethodGet, "/v1/admin/accounts/"+tn.AccountID, nil)
	r.expect(t, "GET /v1/admin/accounts/{id}", status, body, http.StatusOK, "")
	var a m2wAccount
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("GET /v1/admin/accounts/{id} is not an account (%v): %s", err, r.snip(body, 400))
	}
	return a
}

// ---- the dials and the worker ----

// setDials puts module 2's global config into the state this run needs and registers the restore of every key it read. Each dial is written, then read back and compared (a PUT that answers 200 and
// stores something else is a defect this must see).
func (r *m2wRun) setDials(t *testing.T) {
	t.Helper()
	prior := r.readDials(t)
	t.Cleanup(func() { r.restoreDials(t, prior) }) // restore: module 2's config is global, and nothing of a later test or stage may inherit a dial of this one
	r.putDials(t)
}

// readDials reads the current value of every dial this run sets (an unknown key is a 404: the dial must exist at the pinned module 2).
func (r *m2wRun) readDials(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	prior := map[string]json.RawMessage{}
	for _, d := range m2wDials {
		status, body := r.admin(t, http.MethodGet, "/v1/admin/config/"+d.Key, nil)
		r.expect(t, "GET /v1/admin/config/"+d.Key+" (the dial must exist at the pinned module 2: an unknown key is 404)", status, body, http.StatusOK, "")
		var row struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(body, &row); err != nil || len(row.Value) == 0 {
			t.Fatalf("GET /v1/admin/config/%s is not a config row (%v): %s", d.Key, err, r.snip(body, 400))
		}
		prior[d.Key] = row.Value
	}
	return prior
}

// restoreDials puts every dial back to the value readDials read. It never aborts the test (a failed restore is reported with t.Errorf): it runs from a cleanup.
func (r *m2wRun) restoreDials(t *testing.T, prior map[string]json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 60*time.Second)
	defer cancel()
	for _, d := range m2wDials {
		status, body, err := r.doCtx(ctx, r.m2URL, http.MethodPut, "/v1/admin/config/"+d.Key, r.adminKey, map[string]json.RawMessage{"value": prior[d.Key]})
		if err != nil || status != http.StatusOK {
			t.Errorf("restoring the dial %s to %s failed (HTTP %d, %v): %s", d.Key, prior[d.Key], status, err, r.snip(body, 300))
		}
	}
}

// putDials writes this run's dials, reads each back and compares (a PUT that answers 200 and stores something else is a defect this must see).
func (r *m2wRun) putDials(t *testing.T) {
	t.Helper()
	for _, d := range m2wDials {
		status, body := r.admin(t, http.MethodPut, "/v1/admin/config/"+d.Key, map[string]json.RawMessage{"value": json.RawMessage(d.Value)})
		r.expect(t, "PUT /v1/admin/config/"+d.Key, status, body, http.StatusOK, "")
		var row struct {
			Value json.RawMessage `json:"value"`
		}
		var want, got any
		if json.Unmarshal(body, &row) != nil || json.Unmarshal(row.Value, &got) != nil || json.Unmarshal([]byte(d.Value), &want) != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("PUT /v1/admin/config/%s answered 200 but its value is %s, want %s: %s", d.Key, r.snip(row.Value, 200), d.Value, r.snip(body, 400))
		}
		status, body = r.admin(t, http.MethodGet, "/v1/admin/config/"+d.Key, nil)
		r.expect(t, "GET /v1/admin/config/"+d.Key+" (read back)", status, body, http.StatusOK, "")
		if json.Unmarshal(body, &row) != nil || json.Unmarshal(row.Value, &got) != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("the dial %s reads back as %s after the PUT, want %s", d.Key, r.snip(row.Value, 200), d.Value)
		}
	}
}

// requireWorker FAILS (never skips) unless module 2 has an ACTIVE worker and the worker run.sh seeded is the one with a fresh heartbeat. Two witnesses: the operator surface's own health document and
// the box row.
func (r *m2wRun) requireWorker(t *testing.T) {
	t.Helper()
	var status string
	var age float64
	err := r.db.QueryRow(r.ctx, "SELECT status::text, COALESCE(EXTRACT(EPOCH FROM (now() - last_heartbeat_at)), 1e9)::float8 FROM send_managed.boxes WHERE id = $1::uuid", r.workerID).Scan(&status, &age)
	if err != nil {
		t.Fatalf("module 2's worker %s has no send_managed.boxes row (%v): run.sh stage 5c inserts it before the worker starts", r.workerID, err)
	}
	if status != "active" || age > m2wHealthyBeat {
		t.Fatalf("module 2's worker %s is %q and its last heartbeat was %.0f s ago (want active and under %d s): the worker is not connected; see the m2-worker log", r.workerID, status, age, m2wHealthyBeat)
	}
	h := r.health(t)
	if h.Boxes.Active < 1 {
		t.Fatalf("module 2's own health document counts %d active worker(s) (boxes: %+v), want at least 1 although the box row of %s is active", h.Boxes.Active, h.Boxes, r.workerID)
	}
}

type m2wHealth struct {
	Dispatch struct {
		Paused bool   `json:"dispatch_paused"`
		Rung   string `json:"dispatch_rung"`
	} `json:"dispatch"`
	Queue map[string]int `json:"queue"`
	Boxes struct {
		Active   int `json:"active"`
		Stale    int `json:"stale"`
		Disabled int `json:"disabled"`
	} `json:"boxes"`
	Accounts map[string]int `json:"accounts"`
}

func (r *m2wRun) health(t *testing.T) m2wHealth {
	t.Helper()
	status, body := r.admin(t, http.MethodGet, "/v1/admin/health", nil)
	r.expect(t, "GET /v1/admin/health (the operator surface must be mounted: ADMIN_API_KEY_HASHES on m2)", status, body, http.StatusOK, "")
	var h m2wHealth
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("GET /v1/admin/health is not the health document (%v): %s", err, r.snip(body, 400))
	}
	return h
}

// prepare is the part every TestM2Worker test starts with: the worker is alive, the sink is clean and idle (a held send of an earlier test must not run under this one), the dials are set (and
// restored when the test ends), and the cleanup that cancels what the test started and waits for the sink to drain is registered. Cleanups run last-in first-out: the campaigns are cancelled first, the
// sink is drained and put back to its default mode second, the dials are restored last.
func (r *m2wRun) prepare(t *testing.T) {
	t.Helper()
	r.requireWorker(t)
	r.ctlCall(t, http.MethodGet, "/readyz", nil, http.StatusOK)
	r.setDials(t)
	t.Cleanup(r.restoreSink)
	t.Cleanup(r.cancelAll)
	r.setMode(t, m2wMode{})
	// the sink must hold nothing: sessions left by an earlier test are not this test's to wait out silently
	start := time.Now()
	for {
		if v := r.inflight(t); v.Inflight == 0 {
			break
		}
		if time.Since(start) > m2wDrainLimit {
			t.Fatalf("the sink still holds %d session(s) %s after the test began: a send of an earlier test is still in flight", r.inflight(t).Inflight, time.Since(start).Round(time.Second))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("NOTE preconditions: run %s; the worker %s is active; module 2's sink target is %s:%d (a private address); the dials are %s; the sink is clean, in its default mode and holds nothing", r.runID, r.workerID, r.sinkIP, m2wSMTPPort, m2wDialText())
}

func m2wDialText() string {
	parts := make([]string, 0, len(m2wDials))
	for _, d := range m2wDials {
		parts = append(parts, d.Key+"="+d.Value)
	}
	return strings.Join(parts, " ")
}

// cancelAll cancels every campaign the test created (idempotent: a finished campaign answers its final position and changes nothing), so that a test that failed halfway does not leave the worker
// sending its remaining items under the next test. It never fails the test.
func (r *m2wRun) cancelAll() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 60*time.Second)
	defer cancel()
	for _, tn := range r.tenants {
		_, _, _ = r.doCtx(ctx, r.m2URL, http.MethodPost, "/v1/cancel", r.token, map[string]string{"campaign_id": tn.CampaignID, "user_id": tn.UserID})
	}
}

// restoreSink waits (bounded) for the sink to stop holding sessions, then puts it back to its default mode: a delay left on would hold every later send to it, and the module-3 tests that follow begin
// by asserting that the sink holds nothing. It never fails the test, except to say that the sink did not drain.
func (r *m2wRun) restoreSink() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), m2wDrainLimit+30*time.Second)
	defer cancel()
	deadline := time.Now().Add(m2wDrainLimit)
	for {
		status, body, err := r.doCtx(ctx, r.ctlURL, http.MethodGet, "/api/inflight", "", nil)
		var v m2wInflight
		if err == nil && status == http.StatusOK && json.Unmarshal(body, &v) == nil && v.Inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("m2_worker: the sink still holds sessions %s after the test ended (HTTP %d, %v): the next test starts with a held send under it\n", m2wDrainLimit, status, err)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	_, _, _ = r.doCtx(ctx, r.ctlURL, http.MethodPost, "/api/mode", "", m2wMode{RejectRcpt: []string{}, DeferRcpt: []string{}, DelayRecipients: []string{}})
}

// ---- module 2's platform surface ----

type m2wQuote struct {
	RecipientCount int64  `json:"recipient_count"`
	Cost           int64  `json:"cost"`
	Currency       string `json:"currency"`
	UnitPrice      int64  `json:"unit_price_credits"`
	Tier           string `json:"tier"`
}

// quote is POST /v1/quote: pure arithmetic (recipient_count * the live unit price); it validates no capacity, pool, account or worker. The test checks the shape and the product.
func (r *m2wRun) quote(t *testing.T, tn *m2wTenant, count int) m2wQuote {
	t.Helper()
	status, body := r.svc(t, http.MethodPost, "/v1/quote", map[string]any{"recipient_count": count, "user_id": tn.UserID})
	r.expect(t, "POST /v1/quote", status, body, http.StatusOK, "")
	var q m2wQuote
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatalf("POST /v1/quote is not a quote (%v): %s", err, r.snip(body, 400))
	}
	if q.RecipientCount != int64(count) || q.Cost != int64(count)*q.UnitPrice || q.UnitPrice < 0 || q.Currency != "credits" || q.Tier == "" {
		t.Errorf("the quote for %d recipient(s) is %s, want recipient_count %d, cost = count x unit_price_credits, currency credits, a tier", count, r.snip(body, 400), count)
	}
	return q
}

// runItem is POST /v1/run for one item: 202 and state queued for a new job, 200 and the CURRENT state for a job that already exists (idempotent on the job item). It returns the status, the job id and
// the state.
func (r *m2wRun) runItem(t *testing.T, tn *m2wTenant, it *m2wItem) (int, string, string) {
	t.Helper()
	status, body := r.svc(t, http.MethodPost, "/v1/run", map[string]string{"job_item_id": it.ID, "user_id": tn.UserID})
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("POST /v1/run for %s item %02d (%s): HTTP %d (code %q), want 202 or 200: %s", tn.Name, it.Index, it.Kind, status, m2wErrCode(body), r.snip(body, 500))
	}
	var ok struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &ok); err != nil || ok.JobID == "" || ok.State == "" {
		t.Fatalf("POST /v1/run for %s item %02d answered %d with %s, want {job_id, state}", tn.Name, it.Index, status, r.snip(body, 400))
	}
	return status, ok.JobID, ok.State
}

// runAll accepts every item of the tenant (each must be a NEW job: 202 and queued) and returns how long that took.
func (r *m2wRun) runAll(t *testing.T, tn *m2wTenant) time.Duration {
	t.Helper()
	start := time.Now()
	seen := map[string]string{}
	for _, it := range tn.Items {
		status, jobID, state := r.runItem(t, tn, it)
		if status != http.StatusAccepted || state != m2wStateQueued {
			t.Fatalf("POST /v1/run for %s item %02d answered HTTP %d with state %q, want 202 and %q (a new job)", tn.Name, it.Index, status, state, m2wStateQueued)
		}
		if prev, dup := seen[jobID]; dup {
			t.Fatalf("POST /v1/run answered the job id %s for two items (%s and %s)", jobID, prev, it.ID)
		}
		seen[jobID] = it.ID
		it.JobID = jobID
	}
	return time.Since(start)
}

// ---- reports ----

// m2wRow is one item of a report in the words the tests compare: every field is the JSON value as text, m2wNull for a JSON null and m2wAbsent for a key that is not there. The single-job report omits
// provider_message_id and completed_at until they are set; the campaign report carries them as null.
type m2wRow struct {
	ItemID      string
	JobID       string
	State       string
	Reason      string
	ErrorClass  string
	PMID        string
	CompletedAt string
	Attempts    string
}

func m2wText(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return m2wAbsent
	}
	if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return m2wNull
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(v))
}

func m2wRowOf(raw map[string]json.RawMessage) m2wRow {
	return m2wRow{
		ItemID: m2wText(raw, "job_item_id"), JobID: m2wText(raw, "job_id"), State: m2wText(raw, "state"), Reason: m2wText(raw, "reason"), ErrorClass: m2wText(raw, "error_class"),
		PMID: m2wText(raw, "provider_message_id"), CompletedAt: m2wText(raw, "completed_at"), Attempts: m2wText(raw, "attempt_count"),
	}
}

func (w m2wRow) set(v string) bool { return v != m2wNull && v != m2wAbsent && v != "" }

func (w m2wRow) String() string {
	return fmt.Sprintf("state=%s reason=%s error_class=%s provider_message_id=%s completed_at=%s attempt_count=%s", w.State, w.Reason, w.ErrorClass, w.shortID(w.PMID), w.CompletedAt, w.Attempts)
}

func (w m2wRow) shortID(s string) string {
	if !w.set(s) {
		return s
	}
	return "set"
}

func m2wTerminal(state string) bool {
	switch state {
	case "sent", "failed", "bounced", "cancelled":
		return true
	}
	return false
}

// singleReport is GET /v1/report?job_item_id=&user_id= (the platform's own key; the answer is module 2's job).
func (r *m2wRun) singleReport(t *testing.T, tn *m2wTenant, it *m2wItem) m2wRow {
	t.Helper()
	status, body := r.svc(t, http.MethodGet, fmt.Sprintf("/v1/report?job_item_id=%s&user_id=%s", it.ID, tn.UserID), nil)
	r.expect(t, fmt.Sprintf("GET /v1/report for %s item %02d (%s)", tn.Name, it.Index, it.Kind), status, body, http.StatusOK, "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the single-job report of %s item %02d is not a JSON object (%v): %s", tn.Name, it.Index, err, r.snip(body, 400))
	}
	row := m2wRowOf(raw)
	row.ItemID = it.ID
	if row.State == m2wAbsent || row.JobID == m2wAbsent {
		t.Fatalf("the single-job report of %s item %02d has no state or job_id: %s", tn.Name, it.Index, r.snip(body, 400))
	}
	if row.Reason == m2wAbsent || row.ErrorClass == m2wAbsent {
		t.Errorf("the single-job report of %s item %02d lacks the key reason or error_class (module 2 always writes both, null when there is nothing to say): %s", tn.Name, it.Index, r.snip(body, 400))
	}
	return row
}

// singleTerminal polls the single-job report of the item until it is terminal or fails at the deadline with the report: module 2 reads the campaign report and the single-job report with separate queries,
// so one can lead the other by an instant, and a report that never becomes terminal while the other says it is must be a failure that shows both.
func (r *m2wRun) singleTerminal(t *testing.T, tn *m2wTenant, it *m2wItem, limit time.Duration) m2wRow {
	t.Helper()
	start := time.Now()
	for {
		row := r.singleReport(t, tn, it)
		if m2wTerminal(row.State) {
			return row
		}
		if time.Since(start) > limit {
			t.Fatalf("the single-job report of %s item %02d (%s) is still not terminal after %s although the campaign report says it is: %s\n%s", tn.Name, it.Index, it.Email, limit, row, r.diagnostics(t, tn))
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("the test's context ended while waiting for the single-job report of %s item %02d: %v", tn.Name, it.Index, r.ctx.Err())
		case <-time.After(m2wPollEvery):
		}
	}
}

type m2wTotals struct {
	Total, Sent, Bounced, Failed, InFlight, Pending, Waiting, Cancelled int
}

func (tt m2wTotals) String() string {
	return fmt.Sprintf("total=%d sent=%d bounced=%d failed=%d cancelled=%d in_flight=%d pending=%d waiting_for_capacity=%d", tt.Total, tt.Sent, tt.Bounced, tt.Failed, tt.Cancelled, tt.InFlight, tt.Pending, tt.Waiting)
}

func (tt m2wTotals) terminal() int { return tt.Sent + tt.Bounced + tt.Failed + tt.Cancelled }

type m2wCampaign struct {
	Totals     m2wTotals
	IsComplete bool
	Terminal   int
	Paused     bool
	PageSize   int // the page size module 2 echoed (it clamps silently: 0 or junk is 100, above 200 is 200)
	Pages      int
	Rows       []m2wRow // every item of the walk, in the order module 2 returned them
	Body       string   // the first page, scrubbed and shortened, for a failure message
}

func (c m2wCampaign) byItem() map[string]m2wRow {
	m := map[string]m2wRow{}
	for _, row := range c.Rows {
		m[row.ItemID] = row
	}
	return m
}

// campaign reads GET /v1/report?campaign_id=&user_id=&page_size= and WALKS every page (next_cursor verbatim) to the one whose next_cursor is null. An item on two pages FAILS the walk; a loop is cut at
// m2wMaxPages. totals, progress and dispatch_paused are the first page's (module 2 reads them with separate queries: they are not one snapshot with the items).
func (r *m2wRun) campaign(t *testing.T, userID, campaignID string, pageSize int) m2wCampaign {
	t.Helper()
	var out m2wCampaign
	cursor := ""
	seen := map[string]bool{}
	for page := 1; ; page++ {
		if page > m2wMaxPages {
			t.Fatalf("the campaign report did not end after %d pages: next_cursor never became null", m2wMaxPages)
		}
		path := fmt.Sprintf("/v1/report?campaign_id=%s&user_id=%s&page_size=%d", campaignID, userID, pageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		status, body := r.svc(t, http.MethodGet, path, nil)
		r.expect(t, fmt.Sprintf("GET /v1/report?campaign_id= (page %d, page_size %d)", page, pageSize), status, body, http.StatusOK, "")
		var rep struct {
			DispatchPaused *bool           `json:"dispatch_paused"`
			Totals         map[string]*int `json:"totals"`
			Progress       map[string]any  `json:"progress"`
			Items          *struct {
				Results    []map[string]json.RawMessage `json:"results"`
				NextCursor *string                      `json:"next_cursor"`
				PageSize   *int                         `json:"page_size"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &rep); err != nil || rep.Items == nil || rep.Totals == nil || rep.Progress == nil || rep.DispatchPaused == nil {
			t.Fatalf("the campaign report is not the documented shape (%v): %s", err, r.snip(body, 600))
		}
		if page == 1 {
			field := func(name string) int {
				p := rep.Totals[name]
				if p == nil {
					t.Fatalf("the campaign report's totals lack %q: %s", name, r.snip(body, 600))
				}
				return *p
			}
			out.Totals = m2wTotals{Total: field("total"), Sent: field("sent"), Bounced: field("bounced"), Failed: field("failed"), InFlight: field("in_flight"), Pending: field("pending"), Waiting: field("waiting_for_capacity"), Cancelled: field("cancelled")}
			if v, ok := rep.Progress["is_complete"].(bool); ok {
				out.IsComplete = v
			} else {
				t.Fatalf("the campaign report's progress has no boolean is_complete: %s", r.snip(body, 600))
			}
			if v, ok := rep.Progress["terminal"].(float64); ok {
				out.Terminal = int(v)
			}
			out.Paused = *rep.DispatchPaused
			if rep.Items.PageSize != nil {
				out.PageSize = *rep.Items.PageSize
			}
			out.Body = r.snip(body, 1500)
		}
		for _, raw := range rep.Items.Results {
			row := m2wRowOf(raw)
			if row.ItemID == m2wAbsent || row.ItemID == m2wNull || row.State == m2wAbsent || row.State == m2wNull {
				t.Fatalf("an item of the campaign report has no job_item_id or state: %v", raw)
			}
			if seen[row.ItemID] {
				t.Fatalf("the campaign walk returned the item %s twice (page %d, page_size %d)", row.ItemID, page, pageSize)
			}
			seen[row.ItemID] = true
			out.Rows = append(out.Rows, row)
		}
		out.Pages = page
		if rep.Items.NextCursor == nil || *rep.Items.NextCursor == "" {
			return out
		}
		cursor = *rep.Items.NextCursor
	}
}

// waitTerminal polls the campaign report (page_size 200) until EVERY item of the tenant is in a terminal state and module 2 says the campaign is complete with totals that cover every submitted item,
// or fails at the deadline with the last report, the sink's view and module 2's own health document. Every change of an item's state is logged with the elapsed time. onPoll, when not nil, runs after
// every poll (the cancel test samples the sink there).
func (r *m2wRun) waitTerminal(t *testing.T, tn *m2wTenant, limit time.Duration, onPoll func(m2wCampaign)) m2wCampaign {
	t.Helper()
	start := time.Now()
	last := map[string]string{}
	lastSingle := map[string]string{}
	var probes []*m2wItem // the single-job report is polled too, for the first item of each kind: its own words for the retry of a deferred item
	seenKind := map[string]bool{}
	for _, it := range tn.Items {
		if !seenKind[it.Kind] {
			seenKind[it.Kind] = true
			probes = append(probes, it)
		}
	}
	for {
		rep := r.campaign(t, tn.UserID, tn.CampaignID, 200)
		for _, it := range probes {
			row := r.singleReport(t, tn, it)
			if cur := row.String(); lastSingle[it.ID] != cur {
				lastSingle[it.ID] = cur
				t.Logf("NOTE t+%5.1fs %s item %02d %-6s single-job report: %s", time.Since(start).Seconds(), tn.Name, it.Index, it.Kind, cur)
			}
		}
		byItem := rep.byItem()
		done := rep.Totals.Total == len(tn.Items) && rep.IsComplete && rep.Totals.terminal() == len(tn.Items)
		for _, it := range tn.Items {
			row, ok := byItem[it.ID]
			if !ok {
				done = false
				continue
			}
			if !m2wTerminal(row.State) {
				done = false
			}
			if cur := row.String(); last[it.ID] != cur {
				last[it.ID] = cur
				t.Logf("NOTE t+%5.1fs %s item %02d %-6s %s: %s", time.Since(start).Seconds(), tn.Name, it.Index, it.Kind, it.Email, cur)
			}
		}
		if onPoll != nil {
			onPoll(rep)
		}
		if done {
			t.Logf("NOTE %s: every item terminal and the campaign complete after %s (%s)", tn.Name, time.Since(start).Round(100*time.Millisecond), rep.Totals)
			return rep
		}
		if time.Since(start) > limit {
			t.Fatalf("the campaign of %s did not finish within %s (items: %d, totals: %s, is_complete %v)\nlast campaign report (first page): %s\n%s", tn.Name, limit, len(tn.Items), rep.Totals, rep.IsComplete, rep.Body, r.diagnostics(t, tn))
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("the test's context ended while waiting for %s: %v\n%s", tn.Name, r.ctx.Err(), r.diagnostics(t, tn))
		case <-time.After(m2wPollEvery):
		}
	}
}

// diagnostics is what a failure message appends: the sink's held sessions and counters, module 2's own health document and (for a tenant) its job rows. It never fails the test.
func (r *m2wRun) diagnostics(t *testing.T, tn *m2wTenant) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 30*time.Second)
	defer cancel()
	var sb strings.Builder
	if status, body, err := r.doCtx(ctx, r.ctlURL, http.MethodGet, "/api/inflight", "", nil); err == nil {
		fmt.Fprintf(&sb, "sink /api/inflight (HTTP %d): %s\n", status, r.snip(bytes.TrimSpace(body), 600))
	} else {
		fmt.Fprintf(&sb, "sink /api/inflight: %v\n", err)
	}
	if status, body, err := r.doCtx(ctx, r.ctlURL, http.MethodGet, "/api/rcpt", "", nil); err == nil {
		var all map[string]m2wRcpt
		if json.Unmarshal(body, &all) == nil {
			var lines []string
			for a, c := range all {
				if strings.HasPrefix(strings.ToLower(a), r.prefix()) {
					lines = append(lines, fmt.Sprintf("  %s accepted=%d rejected=%d deferred=%d", a, c.Accepted, c.Rejected, c.Deferred))
				}
			}
			sort.Strings(lines)
			fmt.Fprintf(&sb, "sink /api/rcpt for this run (HTTP %d, %d address(es)):\n%s\n", status, len(lines), strings.Join(lines, "\n"))
		}
	} else {
		fmt.Fprintf(&sb, "sink /api/rcpt: %v\n", err)
	}
	if status, body, err := r.doCtx(ctx, r.m2URL, http.MethodGet, "/v1/admin/health", r.adminKey, nil); err == nil {
		var h m2wHealth
		if json.Unmarshal(body, &h) == nil {
			fmt.Fprintf(&sb, "module 2 health (HTTP %d): dispatch rung %q paused %v; queue %v; boxes %+v; accounts %v\n", status, h.Dispatch.Rung, h.Dispatch.Paused, h.Queue, h.Boxes, h.Accounts)
		} else {
			fmt.Fprintf(&sb, "module 2 health (HTTP %d): %s\n", status, r.snip(body, 500))
		}
	} else {
		fmt.Fprintf(&sb, "module 2 health: %v\n", err)
	}
	if tn != nil {
		if rows, err := r.jobRows(ctx, tn); err == nil {
			fmt.Fprintf(&sb, "module 2 job rows of %s (send_managed.jobs, read as the superuser):\n%s", tn.Name, rows)
		} else {
			fmt.Fprintf(&sb, "module 2 job rows of %s: %v\n", tn.Name, err)
		}
	}
	return sb.String()
}

// jobRows reads module 2's own job rows of the tenant's campaign, for EVIDENCE lines and failure messages only (the columns are those of send_managed_0007 and send_managed_0028).
func (r *m2wRun) jobRows(ctx context.Context, tn *m2wTenant) (string, error) {
	rows, err := r.db.Query(ctx, `SELECT ji.recipient_email, j.state::text, COALESCE(j.terminal_reason::text, ''), COALESCE(j.last_error_class::text, ''), j.attempt_count, j.failed_attempt_count,
			COALESCE(j.assigned_box_id::text, ''), j.cancel_requested_at IS NOT NULL
		FROM send_managed.jobs j JOIN public.job_items ji ON ji.id = j.job_item_id
		WHERE j.campaign_id = $1::uuid ORDER BY j.created_at, j.id`, tn.CampaignID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var email, state, reason, class, box string
		var attempts, failed int
		var cancelReq bool
		if err := rows.Scan(&email, &state, &reason, &class, &attempts, &failed, &box, &cancelReq); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "  %s state=%s terminal_reason=%q last_error_class=%q attempt_count=%d failed_attempt_count=%d worker=%s cancel_requested=%v\n", email, state, reason, class, attempts, failed, m2wWho(box, r.workerID), cancelReq)
	}
	return sb.String(), rows.Err()
}

func m2wWho(box, worker string) string {
	switch box {
	case "":
		return "none"
	case worker:
		return "the harness worker"
	}
	return "another worker"
}

// ---- cancel ----

type m2wCancel struct {
	CampaignID      string `json:"campaign_id"`
	Cancelled       int    `json:"cancelled"`
	InFlight        int    `json:"in_flight"`
	AlreadyTerminal int    `json:"already_terminal"`
}

func (c m2wCancel) total() int { return c.Cancelled + c.InFlight + c.AlreadyTerminal }

func (c m2wCancel) String() string {
	return fmt.Sprintf("cancelled=%d in_flight=%d already_terminal=%d", c.Cancelled, c.InFlight, c.AlreadyTerminal)
}

// cancel is POST /v1/cancel for the campaign as the given user. The answer is the campaign's position AFTER the call (a partition of its items), never a delta.
func (r *m2wRun) cancel(t *testing.T, userID, campaignID string) m2wCancel {
	t.Helper()
	status, body := r.svc(t, http.MethodPost, "/v1/cancel", map[string]string{"campaign_id": campaignID, "user_id": userID})
	r.expect(t, "POST /v1/cancel", status, body, http.StatusOK, "")
	var c m2wCancel
	if err := json.Unmarshal(body, &c); err != nil || c.CampaignID != campaignID {
		t.Fatalf("POST /v1/cancel answered %s, want {campaign_id: %s, cancelled, in_flight, already_terminal}", r.snip(body, 400), campaignID)
	}
	return c
}

// ---- assertions that name what they check ----

// m2wWant is what one item must look like in a report. PMID says whether provider_message_id must be set.
type m2wWant struct {
	State      string
	Reason     string // m2wNull for a JSON null
	ErrorClass string
	PMID       bool
}

// checkRow reports (t.Errorf: the test goes on, so one run shows every divergence) each field of the row that is not what the scenario expects. what names the item and where it was read.
func (r *m2wRun) checkRow(t *testing.T, what string, got m2wRow, want m2wWant) bool {
	t.Helper()
	ok := true
	if got.State != want.State {
		t.Errorf("%s: state %q, want %q (%s)", what, got.State, want.State, got)
		ok = false
	}
	if got.Reason != want.Reason && !(want.Reason == m2wNull && got.Reason == m2wAbsent) {
		t.Errorf("%s: reason %s, want %s (%s)", what, got.Reason, want.Reason, got)
		ok = false
	}
	if want.ErrorClass != m2wAny && got.ErrorClass != want.ErrorClass && !(want.ErrorClass == m2wNull && got.ErrorClass == m2wAbsent) {
		t.Errorf("%s: error_class %s, want %s (%s)", what, got.ErrorClass, want.ErrorClass, got)
		ok = false
	}
	if want.PMID != got.set(got.PMID) {
		t.Errorf("%s: provider_message_id is %s, want it %s (%s)", what, got.PMID, map[bool]string{true: "set", false: "null or absent"}[want.PMID], got)
		ok = false
	}
	switch want.State {
	case "sent", "failed", "bounced":
		if !got.set(got.CompletedAt) {
			t.Errorf("%s: a %s item has no completed_at (%s)", what, want.State, got)
			ok = false
		}
	}
	return ok
}

// sameSet reports whether a and b hold the same strings (order and repetition ignored) and says what differs.
func m2wDiff(a, b []string) (onlyA, onlyB []string) {
	ina, inb := map[string]bool{}, map[string]bool{}
	for _, x := range a {
		ina[x] = true
	}
	for _, x := range b {
		inb[x] = true
	}
	for x := range ina {
		if !inb[x] {
			onlyA = append(onlyA, x)
		}
	}
	for x := range inb {
		if !ina[x] {
			onlyB = append(onlyB, x)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return
}

func (tn *m2wTenant) emails(kind string) []string {
	var out []string
	for _, it := range tn.Items {
		if kind == "" || it.Kind == kind {
			out = append(out, it.Email)
		}
	}
	sort.Strings(out)
	return out
}

func (tn *m2wTenant) ids() []string {
	out := make([]string, 0, len(tn.Items))
	for _, it := range tn.Items {
		out = append(out, it.ID)
	}
	sort.Strings(out)
	return out
}
