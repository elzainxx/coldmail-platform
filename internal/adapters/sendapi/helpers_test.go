package sendapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The tenants and ids of the tests: synthetic, none of them a secret and none the all-zero system user.
const (
	userA    = "2f6d1c1e-7a4b-4c55-9f0e-5b7a3c9d8e21"
	userB    = "9c1e7a3b-5d2f-4a6c-8b0e-1f3d5a7c9e42"
	campaign = "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"
	jobItem  = "a1b2c3d4-0000-4000-8000-000000000001"
	jobID    = "0d3e5f70-8192-4a3b-8c4d-5e6f708192a3"
	fixtures = "testdata/contract" // the module-3-shaped set of this package (module 2's is under internal/adapters/m2)
)

// m2like is module 2's profile, written out here so that this package's tests do not import the m2 package (m2's own tests pin its Profile() to this copy by value, in
// TestProfileIsTheDocumentedOne).
func m2like() sendapi.Profile {
	return sendapi.Profile{
		Module: modclient.M2, DescribeModule: "send_managed",
		JobStates:    []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"},
		ErrorClasses: []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"},
		ItemReasons: []string{"pool_exhausted", "attempts_exhausted", "bounce_permanent", "cancelled", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown",
			"no_eligible_account", "waiting_for_capacity", "dispatch_paused"},
		ErrorCodes:    []string{"validation_failed", "unauthenticated", "not_found", "method_not_allowed", "conflict", "internal_error", "paused"},
		AlwaysPresent: true, LinearCost: true, Cancel: true, RunConflict: sendapi.ConflictBackpressure,
		PauseCapability: "campaign_pause_resume",
	}
}

// m3like is module 3's profile (the m3 adapter builds the real one, and its own test holds the two equal): the cancelled state and a cancel route (Phase 3 close-out C1), no reason key,
// account detail, a feasibility quote, the single report omits keys until set, and a 409 on run that names its causes (C2).
func m3like() sendapi.Profile {
	return sendapi.Profile{
		Module: modclient.M3, DescribeModule: "send_byo",
		JobStates:        []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"},
		ErrorClasses:     []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"},
		ErrorCodes:       []string{"validation_failed", "unauthenticated", "not_found", "method_not_allowed", "conflict", "unprocessable", "rate_limited", "internal_error", "paused"},
		AccountDetail:    true,
		QuoteFeasibility: true,
		Cancel:           true,
		RunConflict:      sendapi.ConflictCauses,
		PauseCapability:  "pause_resume",
	}
}

func profiles() map[string]sendapi.Profile {
	return map[string]sendapi.Profile{"m2": m2like(), "m3": m3like()}
}

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// env is a client under test: a module client built for the profile's module at a stub, the sendapi client for a tenant on top of it, and the token.
type env struct {
	t     *testing.T
	stub  *sendapitest.Stub
	c     *sendapi.Client
	token string
}

type handler = func(w http.ResponseWriter, r *http.Request, n int)

func newEnv(t *testing.T, p sendapi.Profile, user string, h handler, mopts modclient.Options, opts sendapi.Options) *env {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, _ *http.Request, _ int) {
			sendapitest.JSON(w, 500, sendapitest.Envelope("internal_error"))
		}
	}
	stub := sendapitest.NewStub(t, h)
	token := sendapitest.Token(t)
	mc := sendapitest.ModClient(t, p.Module, stub.URL(), token, mopts)
	c, err := sendapi.New(mc, p, user, opts)
	if err != nil {
		t.Fatalf("sendapi.New: %v", err)
	}
	return &env{t: t, stub: stub, c: c, token: token}
}

// serve answers every request with status and the body of a fixture.
func serve(status int, body []byte) handler { return sendapitest.Serve(status, body) }

func reply(status int, body string) handler {
	return func(w http.ResponseWriter, _ *http.Request, _ int) { sendapitest.JSON(w, status, body) }
}

// wantKind requires err to be a modclient error of exactly this kind and returns it.
func wantKind(t testing.TB, err error, want modclient.Kind) *modclient.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want kind %s", want)
	}
	if got := modclient.KindOf(err); got != want {
		t.Fatalf("kind %s, want %s: %v", got, want, err)
	}
	var e *modclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v has no *modclient.Error", err)
	}
	return e
}

func isZero(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }

// itemJSON builds a campaign item the way the module of the profile spells it.
func itemJSON(p sendapi.Profile, n int, state string) map[string]any {
	id := "a1b2c3d4-0000-4000-8000-" + strings.Repeat("0", 12-len(strconv.Itoa(n))) + strconv.Itoa(n)
	it := map[string]any{"job_item_id": id, "state": state, "provider_message_id": nil, "completed_at": nil, "attempt_count": 0, "error_class": nil}
	if len(p.ItemReasons) > 0 {
		it["reason"] = nil
	}
	if p.AccountDetail {
		it["smtp_response"], it["failed_at"], it["account_id"], it["account_username"] = nil, nil, nil, nil
	}
	return it
}

func idOf(n int) string { return itemJSON(m2like(), n, "sent")["job_item_id"].(string) }

// header builds the top-level keys of a campaign report from the item count (the totals partition the count; the numbers are not what a test reads).
func header(p sendapi.Profile) func(total int) map[string]any {
	return func(total int) map[string]any {
		totals := map[string]any{"total": total, "sent": total, "bounced": 0, "failed": 0, "in_flight": 0, "pending": 0, "waiting_for_capacity": 0}
		if slices.Contains(p.JobStates, "cancelled") {
			totals["cancelled"] = 0
		}
		h := map[string]any{
			"dispatch_paused": false, "totals": totals,
			"progress": map[string]any{"terminal": total, "fraction_complete": 1, "is_complete": total > 0},
		}
		if p.AccountDetail {
			h["totals_by_account"] = []any{}
		}
		return h
	}
}

// fakeCampaign is a module's campaign of n sent items for user, in creation order.
func fakeCampaign(p sendapi.Profile, user string, n int, bug bool) sendapitest.Campaign {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = itemJSON(p, i+1, "sent")
	}
	return sendapitest.Campaign{ID: campaign, User: user, Items: items, Header: header(p), LookaheadBug: bug}
}

func ids(items []sendapi.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.JobItemID
	}
	return out
}

func wantIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = idOf(i + 1)
	}
	return out
}

func contains(s string, needles ...string) string {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return n
		}
	}
	return ""
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func jsonOf(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func decodeMap(t testing.TB, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the fake module's body is not JSON: %v", err)
	}
	return m
}

// hijackClose drops the connection without an answer: a module that dies mid-request.
func hijackClose(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}
