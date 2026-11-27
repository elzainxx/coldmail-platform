//go:build harness

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
)

// The SENDERS of the scope sendjobs (tests/harness/jobs/send-stage.sh, Phase 4 close-out D5 and D6): a user made ready to send at module 2 or module 3 (module 2: credits through the ledger, a dedicated pool
// and one account naming the controllable sink's private address; module 3: a verified, aligned account at the sink's alias, one lane, the entitlement through the ledger delivered to module 3 by the
// platform's own reconcile loop), the controllable sink told which recipients to reject and how long to hold each message, and a draft with the campaign's content, through the platform's API. Then what
// a send job leaves behind is read from the module's own tables, its campaign report and the sink, which counts every message per recipient. Nothing here mocks a module: module 2 and module 3 are the
// pinned, unmodified services of the harness, their workers send real SMTP to the sink.
//
// The environment (docker-compose.sendjobs.yml gives it to the scenario program's container; a missing variable fails the command naming it): the module's URL, its service token and its operator key, the
// sink's control API and the SMTP password every account uses (the sink accepts any). None of them is ever printed: a body quoted in a failure is scrubbed first.
const (
	sjM2URL      = "SENDJOBS_M2_URL"
	sjM2Svc      = "SENDJOBS_M2_TOKEN"
	sjM2Admin    = "SENDJOBS_M2_ADMIN_KEY"
	sjM3URL      = "SENDJOBS_M3_URL"
	sjM3Svc      = "SENDJOBS_M3_TOKEN"
	sjM3Admin    = "SENDJOBS_M3_ADMIN_KEY"
	sjCtlURL     = "SENDJOBS_CTL_URL"
	sjSinkPass   = "SENDJOBS_SINK_PASSWORD"
	sjCtlHost    = "smtp-ctl"                 // the controllable sink's service name: module 2's account names its PRIVATE address (module 2's worker skips certificate validation only for private addresses)
	sjM3SinkHost = "10.smtp-ctl.example.test" // the sink's alias module 3's accounts name (docker-compose.yml): a private-looking name below the From domain, so the account verifies and is aligned (m3adapter/m3dns.json)
	sjSMTPPort   = 1025
	sjM3Resolver = "m3dns:53" // module 3's From-gate asks the fixture DNS of stage 5b
)

// m2Dials are module 2's global dials for the scope (the values of the m2worker stage, tests/harness/m2_worker_helpers_test.go m2wDials, and why): two failed attempts end a job, a one-second retry ladder, no
// pause between sends, one lane per user (with one worker: at most one send of a user in flight), a 250 ms dispatch tick. They are never restored: the scope ends with the run.
var m2Dials = []struct{ key, value string }{
	{"job_max_failed_attempts", `2`},
	{"retry_backoff_schedule", `{"ladder_seconds":[1,1,1],"max_seconds":1}`},
	{"send_delay_min_sec", `0`},
	{"send_delay_max_sec", `0`},
	{"concurrency_per_user_default", `1`},
	{"dispatch_tick_interval_ms", `250`},
}

func sendVar(name string) (string, error) {
	v := strings.TrimSpace(envOr(name, ""))
	if v == "" {
		return "", failf(exitFail, "the environment variable %s is not set (the compose service jobscenario gets it from tests/harness/docker-compose.sendjobs.yml in the scope sendjobs)", name)
	}
	return v, nil
}

// sendEnv is what the scenario needs to talk to one sender and to the sink.
type sendEnv struct {
	module         string
	url            string
	token, admin   config.Secret
	ctl            string
	sinkPass       config.Secret
	hc             *http.Client
	secretsToScrub []string
}

func loadSendEnv(module string) (*sendEnv, error) {
	var urlVar, tokVar, admVar string
	switch module {
	case "m2":
		urlVar, tokVar, admVar = sjM2URL, sjM2Svc, sjM2Admin
	case "m3":
		urlVar, tokVar, admVar = sjM3URL, sjM3Svc, sjM3Admin
	default:
		return nil, failf(exitUsage, "module %q is not a sender (m2 or m3)", module)
	}
	e := &sendEnv{module: module, hc: &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var err error
	var tok, adm, pass string
	if e.url, err = sendVar(urlVar); err != nil {
		return nil, err
	}
	if tok, err = sendVar(tokVar); err != nil {
		return nil, err
	}
	if adm, err = sendVar(admVar); err != nil {
		return nil, err
	}
	if e.ctl, err = sendVar(sjCtlURL); err != nil {
		return nil, err
	}
	if pass, err = sendVar(sjSinkPass); err != nil {
		return nil, err
	}
	if tok == adm {
		return nil, failf(exitFail, "%s and %s are the same value: the service token and the operator key are two credentials", tokVar, admVar)
	}
	e.url, e.ctl = strings.TrimRight(e.url, "/"), strings.TrimRight(e.ctl, "/")
	e.token, e.admin, e.sinkPass = config.Secret(tok), config.Secret(adm), config.Secret(pass)
	e.secretsToScrub = []string{tok, adm, pass}
	return e, nil
}

// scrub takes every credential of the environment out of a text that is about to be printed.
func (e *sendEnv) scrub(s string) string {
	for _, v := range e.secretsToScrub {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return clip(s, 400)
}

// do sends one request (a JSON body when body is not nil, the key as a Bearer credential when it is not empty) and returns the status and the body. A request that got no answer is a *transportError.
func (e *sendEnv) do(ctx context.Context, method, url string, key config.Secret, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, failf(exitFail, "cannot build the request %s %s", method, pathOf(url))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k := key.Reveal(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return 0, nil, &transportError{err: errors.New(e.scrub(err.Error()))}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, &transportError{err: errors.New(e.scrub(err.Error()))}
	}
	return resp.StatusCode, b, nil
}

// expect is do that fails unless the answer has the status wanted; the failure names the request, the status and the module's own error code, never a credential.
func (e *sendEnv) expect(ctx context.Context, what, method, url string, key config.Secret, body any, want int) ([]byte, error) {
	status, b, err := e.do(ctx, method, url, key, body)
	if err != nil {
		return nil, failf(exitFail, "%s: %s %s got no answer: %v", what, method, pathOf(url), err)
	}
	if status != want {
		return b, failf(exitFail, "%s: %s %s answered HTTP %d, want %d: %s", what, method, pathOf(url), status, want, e.scrub(string(b)))
	}
	return b, nil
}

func pathOf(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
		if j := strings.IndexByte(url, '/'); j >= 0 {
			url = url[j:]
		}
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url = url[:i]
	}
	return url
}

// client is the platform's own adapter of the module, bound to the user (the reader of the campaign report: module 2 and module 3 answer the same shape through sendapi).
func (e *sendEnv) client(user string) (*sendapi.Client, error) {
	cfg := config.Config{ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
	switch e.module {
	case "m2":
		cfg.M2URL, cfg.M2ServiceToken = e.url, e.token
		ad, err := m2.FromConfig(cfg)
		if err != nil {
			return nil, failf(exitFail, "module 2's adapter cannot be built: %s", e.scrub(err.Error()))
		}
		c, err := ad.For(user)
		if err != nil {
			return nil, failf(exitFail, "module 2's adapter for the user cannot be built: %s", e.scrub(err.Error()))
		}
		return c.Client, nil
	}
	c, err := e.m3Client(user)
	if err != nil {
		return nil, err
	}
	return c.Client, nil
}

func (e *sendEnv) m3Client(user string) (*m3.Client, error) {
	cfg := config.Config{M3URL: e.url, M3ServiceToken: e.token, M3AdminToken: e.admin, ModuleTimeout: 20 * time.Second, ModuleRetries: 2, ModuleRetryBackoff: 250 * time.Millisecond}
	ad, err := m3.FromConfig(cfg)
	if err != nil {
		return nil, failf(exitFail, "module 3's adapter cannot be built: %s", e.scrub(err.Error()))
	}
	c, err := ad.For(user)
	if err != nil {
		return nil, failf(exitFail, "module 3's adapter for the user cannot be built: %s", e.scrub(err.Error()))
	}
	return c, nil
}

// profile is the module's profile (the charge table's module column and the outcome of an item).
func (e *sendEnv) profile() sendapi.Profile {
	if e.module == "m2" {
		return m2.Profile()
	}
	return m3.Profile()
}

// schema is the module's own schema (its jobs table is read as the superuser: ground truth beside the report).
func schemaOf(module string) string {
	if module == "m2" {
		return "send_managed"
	}
	return "send_byo"
}

// ---- the controllable sink (tests/harness/sink) ----------------------------------------------------------------------------------------------------------------------------------------------

// sinkMode is the body of POST /api/mode: the sink REPLACES its whole mode with it.
type sinkMode struct {
	RejectRcpt      []string `json:"reject_rcpt"`
	RejectCode      int      `json:"reject_code,omitempty"`
	DeferRcpt       []string `json:"defer_rcpt"`
	DataDelayMs     int      `json:"data_delay_ms"`
	DelayRecipients []string `json:"delay_recipients"`
	FailAuth        bool     `json:"fail_auth"`
}

type sinkMsg struct {
	Seq       int64    `json:"seq"`
	To        []string `json:"to"`
	MessageID string   `json:"message_id"`
}

// sinkRcpt is what the sink answered one recipient at RCPT TO since its last reset.
type sinkRcpt struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Deferred int `json:"deferred"`
}

func (c sinkRcpt) tried() int { return c.Accepted + c.Rejected + c.Deferred }

// sinkView is the sink seen through the addresses that begin with one prefix (a user's: every recipient and every malformed line of a send job carries it): the messages per exact address, the RCPT
// counters, a Message-ID seen twice and a message without one.
type sinkView struct {
	Msgs   map[string]int
	Rcpt   map[string]sinkRcpt
	DupIDs []string
	NoID   int
}

func (e *sendEnv) sinkSet(ctx context.Context, m sinkMode) error {
	for _, l := range []*[]string{&m.RejectRcpt, &m.DeferRcpt, &m.DelayRecipients} {
		if *l == nil {
			*l = []string{}
		}
	}
	b, err := e.expect(ctx, "the controllable sink's mode", http.MethodPost, e.ctl+"/api/mode", "", m, http.StatusOK)
	if err != nil {
		return err
	}
	var got sinkMode
	want := m.RejectCode
	if want == 0 {
		want = 550
	}
	if json.Unmarshal(b, &got) != nil || len(got.RejectRcpt) != len(m.RejectRcpt) || len(got.DelayRecipients) != len(m.DelayRecipients) || got.DataDelayMs != m.DataDelayMs || got.RejectCode != want {
		return failf(exitFail, "the controllable sink did not take the mode it was sent (reject %d, hold %d ms for %d recipient(s)): it says %s", len(m.RejectRcpt), m.DataDelayMs, len(m.DelayRecipients), e.scrub(string(b)))
	}
	return nil
}

func (e *sendEnv) sinkRead(ctx context.Context, prefix string) (sinkView, error) {
	v := sinkView{Msgs: map[string]int{}, Rcpt: map[string]sinkRcpt{}}
	b, err := e.expect(ctx, "the controllable sink's messages", http.MethodGet, e.ctl+"/api/messages", "", nil, http.StatusOK)
	if err != nil {
		return v, err
	}
	var msgs []sinkMsg
	if err := json.Unmarshal(b, &msgs); err != nil {
		return v, failf(exitFail, "the controllable sink's /api/messages is not a list of messages")
	}
	ids := map[string]bool{}
	for _, m := range msgs {
		mine := false
		for _, raw := range m.To {
			a := strings.ToLower(strings.TrimSpace(raw))
			if strings.HasPrefix(a, prefix) {
				mine = true
				v.Msgs[a]++
			}
		}
		if !mine {
			continue
		}
		switch {
		case m.MessageID == "":
			v.NoID++
		case ids[m.MessageID]:
			v.DupIDs = append(v.DupIDs, m.MessageID)
		}
		ids[m.MessageID] = true
	}
	b, err = e.expect(ctx, "the controllable sink's RCPT counters", http.MethodGet, e.ctl+"/api/rcpt", "", nil, http.StatusOK)
	if err != nil {
		return v, err
	}
	var rc map[string]sinkRcpt
	if err := json.Unmarshal(b, &rc); err != nil {
		return v, failf(exitFail, "the controllable sink's /api/rcpt is not a map of counters")
	}
	for a, c := range rc {
		if a = strings.ToLower(a); strings.HasPrefix(a, prefix) {
			v.Rcpt[a] = c
		}
	}
	return v, nil
}

// userPrefix is the recipient prefix of a user's send jobs: every recipient and every malformed line of the user's lists begins with it, and the sink is read through it.
func userPrefix(user string) string { return "sj-" + user[:8] + "-" }

// privateAddr resolves a service name of the private network to its IPv4 address in one of the three private ranges.
func privateAddr(ctx context.Context, host string) (string, error) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "", failf(exitFail, "cannot resolve %s from the scenario program's container: %v", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if v4 := ip.To4(); v4 != nil && (v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168)) {
			return a, nil
		}
	}
	return "", failf(exitFail, "%s resolves to %v, none of them a private IPv4 address: module 2's worker validates a certificate unless the SMTP host is a private address", host, addrs)
}

// ---- send-prepare: the modules' dials and the sink, once per run ----------------------------------------------------------------------------------------------------------------------------

func cmdSendPrepare(args []string) error {
	fs := flags("send-prepare")
	var mods string
	fs.StringVar(&mods, "modules", "m2,m3", "the senders this run drives, comma separated")
	if err := parse(fs, args); err != nil {
		return err
	}
	ctx, cancel := ctxFor(5 * time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	var list []string
	for _, m := range strings.Split(mods, ",") {
		if m = strings.TrimSpace(m); m != "" {
			list = append(list, m)
		}
	}
	if len(list) == 0 {
		return failf(exitUsage, "--modules names no module")
	}
	for i, m := range list {
		e, err := loadSendEnv(m)
		if err != nil {
			return err
		}
		if i == 0 { // the sink is shared: forget what it holds and put it in its default mode, once
			if _, err := e.expect(ctx, "the controllable sink's readiness", http.MethodGet, e.ctl+"/readyz", "", nil, http.StatusOK); err != nil {
				return err
			}
			if _, err := e.expect(ctx, "the controllable sink's reset", http.MethodPost, e.ctl+"/api/reset", "", nil, http.StatusOK); err != nil {
				return err
			}
			if err := e.sinkSet(ctx, sinkMode{}); err != nil {
				return err
			}
		}
		var boxes int
		switch m {
		case "m2":
			for _, dl := range m2Dials {
				b, err := e.expect(ctx, "module 2's dial "+dl.key, http.MethodPut, e.url+"/v1/admin/config/"+dl.key, e.admin, map[string]json.RawMessage{"value": json.RawMessage(dl.value)}, http.StatusOK)
				if err != nil {
					return err
				}
				var row struct {
					Value json.RawMessage `json:"value"`
				}
				var want, got any
				if json.Unmarshal(b, &row) != nil || json.Unmarshal(row.Value, &got) != nil || json.Unmarshal([]byte(dl.value), &want) != nil || !reflect.DeepEqual(want, got) {
					return failf(exitFail, "module 2 answered 200 to the dial %s but holds %s, want %s", dl.key, e.scrub(string(row.Value)), dl.value)
				}
			}
			if err := d.c.QueryRow(ctx, `SELECT count(*) FROM send_managed.boxes WHERE status = 'active' AND last_heartbeat_at > now() - interval '60 seconds'`).Scan(&boxes); err != nil {
				return d.scrub(err)
			}
		case "m3":
			for _, dl := range []struct{ key, value string }{{"from_gate_mode", `"enforce"`}, {"from_dns_resolver", `"` + sjM3Resolver + `"`}} {
				tag, err := d.c.Exec(ctx, `UPDATE send_byo.config SET value = $2::jsonb WHERE key = $1`, dl.key, `{"v":`+dl.value+`}`)
				if err != nil {
					return d.scrub(err)
				}
				if tag.RowsAffected() != 1 {
					return failf(exitFail, "module 3 has no dial %s (send_byo.config): the pinned module is not the one the From-gate proofs were written against", dl.key)
				}
			}
			if err := d.c.QueryRow(ctx, `SELECT count(*) FROM send_byo.boxes WHERE status = 'active' AND last_heartbeat_at > now() - interval '60 seconds'`).Scan(&boxes); err != nil {
				return d.scrub(err)
			}
		}
		if boxes < 1 {
			return failf(exitFail, "module %s has no worker with a heartbeat in the last 60 s: it cannot send (the stage starts its workers before this)", m)
		}
		fact("prepared", "module", m, "workers", boxes)
	}
	return nil
}

// ---- create: a user ready to send, the sink's mode, the draft -------------------------------------------------------------------------------------------------------------------------------

// m2Account makes the user's dedicated pool at module 2 and one account in it naming the sink's private address (module 2's operator surface; the credential is write-only).
func (e *sendEnv) m2Account(ctx context.Context, user, prefix string) (string, error) {
	sinkIP, err := privateAddr(ctx, sjCtlHost)
	if err != nil {
		return "", err
	}
	b, err := e.expect(ctx, "module 2's pool of the user", http.MethodPost, e.url+"/v1/admin/pools", e.admin, map[string]any{"kind": "dedicated", "owner_user_id": user, "name": "sendjobs " + prefix}, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var pool struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &pool) != nil || !isUUID(strings.ToLower(pool.ID)) {
		return "", failf(exitFail, "module 2's answer to the pool is not a pool: %s", e.scrub(string(b)))
	}
	from := "sender-" + strings.TrimSuffix(prefix, "-") + "@example.test"
	b, err = e.expect(ctx, "module 2's account of the user", http.MethodPost, e.url+"/v1/admin/accounts", e.admin, map[string]any{
		"pool_id": pool.ID, "smtp_host": sinkIP, "smtp_port": sjSMTPPort, "smtp_username": "sendjobs-" + strings.TrimSuffix(prefix, "-"), "smtp_secret": e.sinkPass.Reveal(),
		"from_address": from, "from_name": "Harness", "daily_limit": 1000,
	}, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var acc struct {
		ID            string `json:"id"`
		UserID        string `json:"user_id"`
		HasCredential bool   `json:"has_credential"`
	}
	if json.Unmarshal(b, &acc) != nil || acc.ID == "" || acc.UserID != user || !acc.HasCredential {
		return "", failf(exitFail, "module 2's answer to the account is not an account of the user holding a credential: %s", e.scrub(string(b)))
	}
	return acc.ID, nil
}

// m3Account adds the user's account at module 3 through the platform's adapter and requires it verified (the sink answered module 3's worker) and aligned (the From-gate, in enforce against the fixture
// DNS, reads example.test as the self-hosted exception), then gives the user ONE lane with at most a second between two sends (module 3's operator surface).
func (e *sendEnv) m3Account(ctx context.Context, user, prefix string) (string, error) {
	c, err := e.m3Client(user)
	if err != nil {
		return "", err
	}
	from := "sender-" + strings.TrimSuffix(prefix, "-") + "@example.test"
	name := "Harness"
	a, err := c.CreateAccount(ctx, m3.NewAccount{SMTPHost: sjM3SinkHost, SMTPPort: sjSMTPPort, SMTPUsername: from, SMTPSecret: e.sinkPass, FromAddress: from, FromName: &name})
	if err != nil {
		return "", failf(exitFail, "module 3's account of the user (through the adapter) failed: %s", e.scrub(err.Error()))
	}
	if a.Verification.State != m3.VerificationVerified || a.FromAlignment.State != m3.AlignmentAligned {
		return "", failf(exitFail, "module 3's account of the user is verification %q (code %q) and alignment %q, want verified and aligned (the account names %s:%d; the From-gate is in enforce against %s)",
			a.Verification.State, a.Verification.Code, a.FromAlignment.State, sjM3SinkHost, sjSMTPPort, sjM3Resolver)
	}
	if _, err := e.expect(ctx, "module 3's pacing of the user", http.MethodPut, e.url+"/v1/admin/users/"+user+"/config", e.admin,
		map[string]int{"concurrency_per_user": 1, "send_delay_min_sec": 0, "send_delay_max_sec": 1}, http.StatusOK); err != nil {
		return "", err
	}
	return a.ID, nil
}

// m3Entitle enables module 3 for the user through the LEDGER (an `m3_enable` row naming the first admin, no end date: the way run.sh stage 9b does) and waits until the platform's reconcile loop (the
// leading instance) has delivered it to module 3 and read it back: converged at the wanted version, observed active.
func (d *db) m3Entitle(ctx context.Context, user string, within time.Duration) error {
	var admin string
	if err := d.c.QueryRow(ctx, `SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at, id LIMIT 1`).Scan(&admin); err != nil {
		return failf(exitFail, "platform.admins holds no active admin: an entitlement names one (%v)", d.scrub(err))
	}
	if _, err := d.c.Exec(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key)
		VALUES ($1::uuid, 'm3', 'm3_enable', $2::uuid, 'the sendjobs scenario entitles its user', $3)`, user, admin, "sendjobs-enable-"+user); err != nil {
		return failf(exitFail, "could not enable module 3 for the user through the ledger: %v", d.scrub(err))
	}
	deadline := time.Now().Add(within)
	for {
		var ok bool
		var row string
		err := d.c.QueryRow(ctx, `SELECT converged_version = want_version AND observed = 'active' AND NOT want_suspended, row_to_json(s)::text FROM platform.m3_sync s WHERE user_id = $1::uuid`, user).Scan(&ok, &row)
		switch {
		case err == nil && ok:
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return d.scrub(err)
		}
		if time.Now().After(deadline) {
			return failf(exitFail, "the platform did not deliver the user's entitlement to module 3 within %s (platform.m3_sync: %s): the leading instance's reconcile loop did not converge", within, clip(row, 300))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// createSendJob is create for module 2 or module 3.
func createSendJob(ctx context.Context, d *db, sg *signer, o createOpts) (job, user string, err error) {
	e, err := loadSendEnv(o.module)
	if err != nil {
		return "", "", err
	}
	if o.bounce < 0 || o.holdMS < 0 || o.holdMS > 20000 {
		return "", "", failf(exitUsage, "--bounce is at least 0 and --hold-ms between 0 and 20000 (both modules give up on a reply after 30 s)")
	}
	user, err = d.newUser(ctx, "sj")
	if err != nil {
		return "", "", err
	}
	prefix := userPrefix(user)
	lines, err := genRowsWith(o.rows, o.bad, o.dup, func(i int) string { return fmt.Sprintf("%s%02d@example.test", prefix, i+1) }, prefix)
	if err != nil {
		return "", user, err
	}
	var pending []string
	seen := map[string]bool{}
	merge := make([]map[string]string, len(lines)) // the merge data of every good address (send_content.go); a malformed line and a repeat carry none
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) && strings.HasSuffix(l, "@example.test") && !seen[l] {
			seen[l] = true
			merge[i] = sendMergeData(len(pending))
			pending = append(pending, l)
		}
	}
	if o.bounce >= len(pending) {
		return "", user, failf(exitUsage, "--bounce %d leaves none of the %d recipients to be sent", o.bounce, len(pending))
	}
	credits, account := 0, ""
	switch o.module {
	case "m2":
		credits = o.grant
		if credits <= 0 {
			credits = 100*o.rows + 1000 // far above any reserve
		}
		if err := d.grantModule(ctx, user, "m2", credits); err != nil {
			return "", user, err
		}
		if account, err = e.m2Account(ctx, user, prefix); err != nil {
			return "", user, err
		}
	case "m3":
		if account, err = e.m3Account(ctx, user, prefix); err != nil {
			return "", user, err
		}
		if err := d.m3Entitle(ctx, user, 2*time.Minute); err != nil {
			return "", user, err
		}
	}
	var bounced []string
	for k := 0; k < o.bounce; k++ { // spread through the list: never the first recipient, so that the first send of a campaign is a sent one
		bounced = append(bounced, pending[1+k*(len(pending)-1)/max(o.bounce, 1)])
	}
	mode := sinkMode{RejectRcpt: bounced, RejectCode: 550}
	if o.holdMS > 0 {
		mode.DataDelayMs, mode.DelayRecipients = o.holdMS, pending
	}
	if err := e.sinkSet(ctx, mode); err != nil {
		return "", user, err
	}
	tag := o.tag
	if tag == "" {
		tag = "job"
	}
	content, err := sendContent(tag, o.textMode) // a TEMPLATED campaign (spintax groups of four options, merge fields with fallbacks) and merge data on every good row
	if err != nil {
		return "", user, err
	}
	job, prev, err := apiDraft(ctx, sg, o, user, lines, merge, content)
	if err != nil {
		return job, user, err
	}
	p := prev.Job.Preview
	fact("created", "user", user, "job", job, "module", o.module, "rows", o.rows, "malformed", o.bad, "duplicates", o.dup, "pending", p.Pending, "credits", p.Credits, "granted", credits,
		"fingerprint", prev.Job.PreviewFingerprint, "prefix", prefix, "bounce", len(bounced), "hold_ms", o.holdMS, "account", account, "text_mode", orDefault(o.textMode, textExplicit))
	return job, user, nil
}

// ---- move: the user's pause, resume or cancel through the API -------------------------------------------------------------------------------------------------------------------------------

func cmdMove(args []string) error {
	fs := flags("move")
	var api, job, op, want string
	fs.StringVar(&api, "api", "http://jobs-a:8080", "the platform instance to call")
	fs.StringVar(&job, "job", "", "the job")
	fs.StringVar(&op, "op", "", "pause, resume or cancel")
	fs.StringVar(&want, "expect-status", "", "the status the answer must carry")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case job == "":
		return failf(exitUsage, "--job is required")
	case op != "pause" && op != "resume" && op != "cancel":
		return failf(exitUsage, "--op is pause, resume or cancel (got %q)", op)
	case want == "":
		return failf(exitUsage, "--expect-status is required (a move whose outcome nobody states proves nothing)")
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
	h, err := d.head(ctx, job)
	if err != nil {
		return err
	}
	a, err := call(ctx, httpClient(90*time.Second), api, http.MethodPost, "/v1/jobs/"+job+"/"+op, sg.mint(h.User, time.Now()), nil, nil)
	if err != nil {
		return err
	}
	if a.Status != http.StatusOK {
		fact("move", "job", job, "op", op, "outcome", "refused", "status", a.Status, "code", orDash(a.ErrorCode), "was", h.Status)
		return refused("the user's "+op+" of the job (it was "+h.Status+")", a)
	}
	var body jobBody
	if err := json.Unmarshal(a.Body, &body); err != nil || body.Job.Status != want {
		return failf(exitFail, "the platform answered the user's %s of job %s with the status %q, want %q (it was %s)", op, job, body.Job.Status, want, h.Status)
	}
	fact("move", "job", job, "op", op, "outcome", "ok", "from", h.Status, "status", body.Job.Status)
	return nil
}

// ---- state of a send job --------------------------------------------------------------------------------------------------------------------------------------------------------------------

// sendSnapshot adds to the state of a send job what the module's own tables, the sink and (report) the module's campaign report hold. module_jobs becomes the module's own jobs of the campaign (the job's
// id), in place of module 1's count.
func (d *db) sendSnapshot(ctx context.Context, s snapshot, report bool) (snapshot, error) {
	h := s.Head
	schema := schemaOf(h.Module)
	var ensured, cancelReq bool
	var jobItems, mJobs, mQueued, mCancelled, mSent, mTerminal, resCancelled, evPaused, evCancelling, evCompleting, notePaused, noteResumed int64
	err := d.c.QueryRow(ctx, fmt.Sprintf(`SELECT
		(SELECT items_ensured_at IS NOT NULL FROM platform.jobs WHERE id = $1::uuid),
		(SELECT cancel_requested_at IS NOT NULL FROM platform.jobs WHERE id = $1::uuid),
		(SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid),
		(SELECT count(*) FROM %[1]s.jobs WHERE campaign_id = $1::uuid),
		(SELECT count(*) FROM %[1]s.jobs WHERE campaign_id = $1::uuid AND state::text = 'queued'),
		(SELECT count(*) FROM %[1]s.jobs WHERE campaign_id = $1::uuid AND state::text = 'cancelled'),
		(SELECT count(*) FROM %[1]s.jobs WHERE campaign_id = $1::uuid AND state::text = 'sent'),
		(SELECT count(*) FROM %[1]s.jobs WHERE campaign_id = $1::uuid AND state::text IN ('sent', 'bounced', 'failed', 'cancelled')),
		(SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'module' AND module_state = 'cancelled'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND to_status = 'paused'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND to_status = 'cancelling'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'transition' AND to_status = 'completing'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'note' AND detail->>'event' = 'module_paused'),
		(SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind = 'note' AND detail->>'event' = 'module_resumed')`, schema), h.ID).
		Scan(&ensured, &cancelReq, &jobItems, &mJobs, &mQueued, &mCancelled, &mSent, &mTerminal, &resCancelled, &evPaused, &evCancelling, &evCompleting, &notePaused, &noteResumed)
	if err != nil {
		return s, d.scrub(err)
	}
	for i := range s.Vals {
		if s.Vals[i].Name == "module_jobs" {
			s.Vals[i].Value = mJobs
		}
	}
	s.Vals = append(s.Vals, kv{"module", h.Module}, kv{"items_ensured", b2i(ensured)}, kv{"cancel_requested", b2i(cancelReq)}, kv{"job_items", jobItems},
		kv{"module_queued", mQueued}, kv{"module_cancelled", mCancelled}, kv{"module_sent", mSent}, kv{"module_terminal", mTerminal}, kv{"results_module_cancelled", resCancelled},
		kv{"events_paused", evPaused}, kv{"events_cancelling", evCancelling}, kv{"events_completing", evCompleting}, kv{"events_module_paused", notePaused}, kv{"events_module_resumed", noteResumed})

	e, err := loadSendEnv(h.Module)
	if err != nil {
		return s, err
	}
	pend, err := d.pendingAddresses(ctx, h.ID)
	if err != nil {
		return s, err
	}
	v, err := e.sinkRead(ctx, userPrefix(h.User))
	if err != nil {
		return s, err
	}
	var msgs, most, foreign int64
	for a, n := range v.Msgs {
		if pend[a] {
			msgs += int64(n)
			most = max(most, int64(n))
		} else {
			foreign += int64(n)
		}
	}
	s.Vals = append(s.Vals, kv{"sink_msgs", msgs}, kv{"sink_max", most}, kv{"sink_foreign", foreign})
	if report {
		c, err := e.client(h.User)
		if err != nil {
			return s, err
		}
		rep, err := c.ReportCampaign(ctx, h.ID)
		if err != nil {
			return s, failf(exitFail, "module %s's campaign report of job %s cannot be read: %s", h.Module, h.ID, e.scrub(err.Error()))
		}
		s.Vals = append(s.Vals, kv{"module_report_paused", b2i(rep.Paused)}, kv{"module_report_complete", b2i(rep.Progress.IsComplete)}, kv{"module_report_total", rep.Totals.Total},
			kv{"module_report_sent", rep.Totals.Sent}, kv{"module_report_cancelled", rep.Totals.Cancelled}, kv{"module_report_items", int64(len(rep.Items))})
	}
	return s, nil
}

// pendingAddresses is the set of the job's pending addresses (normalised).
func (d *db) pendingAddresses(ctx context.Context, job string) (map[string]bool, error) {
	rs, err := d.c.Query(ctx, `SELECT email_norm FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending' AND email_norm IS NOT NULL`, job)
	if err != nil {
		return nil, d.scrub(err)
	}
	defer rs.Close()
	out := map[string]bool{}
	for rs.Next() {
		var a string
		if err := rs.Scan(&a); err != nil {
			return nil, d.scrub(err)
		}
		out[strings.ToLower(a)] = true
	}
	return out, d.scrub(rs.Err())
}
