package m3_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 6 A4: DeleteAccount, the module's SOFT delete (DELETE /v1/send-byo/accounts/{id}?user_id=..., 204). No recording of it exists yet (m3.UncoveredRoutes says so), so these tests use bodies written from the
// module's api-contract.md (a 204 with no body; the 404 envelope that the recorded get answers, which is the same answer for an account that never existed, one that is deleted and one of another tenant).

// noContent is the module's answer to a delete: 204, no body, no content type.
func noContent(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) }

// A delete carries the tenant in the QUERY (never in a body: a DELETE has none), names the account in the path, is a DELETE, and a 204 is success with nothing returned. The control is the same call
// for an id the module does not know: the module's 404 is NotFound, and the adapter says nothing else about it.
func TestDeleteAccountCarriesTheTenantInTheQueryAndTakesA204(t *testing.T) {
	s := newStack(t, noContent, modclient.Options{})
	if err := s.client(t, userA).DeleteAccount(ctxT(t), acct1); err != nil {
		t.Fatalf("a 204: %v", err)
	}
	reqs := s.stub.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d request(s), want one", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodDelete || r.Path != "/v1/send-byo/accounts/"+acct1 || r.Query.Get("user_id") != userA || len(r.Body) != 0 || r.Auth != "Bearer "+s.token {
		t.Fatalf("the request was %s %s?%v with a body of %d byte(s) and auth %q", r.Method, r.Path, r.Query, len(r.Body), digest(r.Auth))
	}
	// the control: the module that does not know the account answers its 404 envelope, which is NotFound (and the tenant is still the one this client was built for)
	nf := newStack(t, replyFixture(404, "accounts.get.error.not_found.json", t), modclient.Options{})
	err := nf.client(t, userB).DeleteAccount(ctxT(t), acct1)
	wantKind(t, err, modclient.NotFound)
	if got := nf.stub.Requests()[0].Query.Get("user_id"); got != userB {
		t.Fatalf("the control asked as %q, want the client's own tenant %q", got, userB)
	}
}

// Anything but a 204 is not a success: a 200 with a body (a different route's answer) and a 202 are Contract errors, a 409 is a refusal, a module that is down was never asked, and one that stalls is
// an open outcome (MaybeApplied), never retried (the module's answer to a repeat is the 404 of an account that is gone, which would read as "there was nothing to delete").
func TestDeleteAccountJudgesEveryOtherAnswerAndIsNeverRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		h     handler
		kind  modclient.Kind
		maybe bool
	}{
		"a 200 with an account":  {replyFixture(200, "accounts.get.ok.json", t), modclient.Contract, true},
		"a 202":                  {reply(202, `{}`), modclient.Contract, true},
		"a refusal":              {reply(409, sendapitest.Envelope("conflict")), modclient.Refused, false},
		"the module's own fault": {reply(500, sendapitest.Envelope("internal_error")), modclient.Internal, true},
		"unavailable":            {reply(503, sendapitest.Envelope("unavailable")), modclient.Unavailable, true},
	} {
		s := newStack(t, tc.h, modclient.Options{Retries: 3})
		err := s.client(t, userA).DeleteAccount(ctxT(t), acct1)
		wantKind(t, err, tc.kind)
		if got := m3.MaybeApplied(err); got != tc.maybe {
			t.Fatalf("%s: MaybeApplied = %v, want %v (%v)", name, got, tc.maybe, err)
		}
		if got := s.stub.Count(); got != 1 {
			t.Fatalf("%s: %d request(s) with the retry dial at 3, want one: a delete is never repeated", name, got)
		}
	}
	// a module that is down is Unavailable and was never reached
	down := newStack(t, noContent, modclient.Options{})
	down.stub.Close()
	if err := down.client(t, userA).DeleteAccount(ctxT(t), acct1); modclient.KindOf(err) != modclient.Unavailable || m3.MaybeApplied(err) {
		t.Fatalf("a refused connection: %v", err)
	}
	// the caller's deadline after the request left is an open outcome
	slow := newStack(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		time.Sleep(600 * time.Millisecond)
		noContent(w, r, b)
	}, modclient.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := slow.client(t, userA).DeleteAccount(ctx, acct1); modclient.KindOf(err) != modclient.Timeout || !m3.MaybeApplied(err) {
		t.Fatalf("a deadline in flight: %v", err)
	}
	// the control: the 204 passes under the same options
	ok := newStack(t, noContent, modclient.Options{Retries: 3})
	if err := ok.client(t, userA).DeleteAccount(ctxT(t), acct1); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// An id that is not a lower-case uuid never reaches the network (so a path cannot be built from it); the control is a good id that does.
func TestDeleteAccountChecksTheIDBeforeTheNetwork(t *testing.T) {
	s := newStack(t, noContent, modclient.Options{})
	c := s.client(t, userA)
	for _, bad := range []string{"", "x", strings.ToUpper(userB), acct1 + "/test", "../" + acct1, acct1 + "?user_id=" + userB} {
		if err := c.DeleteAccount(ctxT(t), bad); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("delete %q: %v", bad, err)
		}
	}
	if s.stub.Count() != 0 {
		t.Fatalf("%d request(s) were made for ids that cannot be accounts", s.stub.Count())
	}
	if err := c.DeleteAccount(ctxT(t), acct1); err != nil || s.stub.Count() != 1 {
		t.Fatalf("control: %v after %d request(s)", err, s.stub.Count())
	}
}

// The routes the account methods really call are each a contract's route or in UncoveredRoutes with a reason, never both and never neither: the routes are collected from the requests the methods make,
// not from a list. The controls: an undeclared route and a contract for a route nothing calls are each reported.
func TestTheRoutesTheAccountMethodsCallAreCoveredOrDeclaredUncovered(t *testing.T) {
	s := newStack(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Method == http.MethodDelete {
			noContent(w, r, b)
			return
		}
		moduleStub(t)(w, r, b)
	}, modclient.Options{})
	c := s.client(t, userA)
	ctx := ctxT(t)
	_, _ = c.ListAccounts(ctx, "")
	_, _ = c.GetAccount(ctx, acct1)
	_, _ = c.CreateAccount(ctx, newAcct(t, 1))
	_, _ = c.TestAccount(ctx, acct1, false)
	_, _ = c.BulkAddAccounts(ctx, batch(t, 2))
	_, _ = c.AccountEvents(ctx, acct1, "")
	_ = c.DeleteAccount(ctx, acct1)
	called := map[string]bool{}
	for _, r := range s.stub.Requests() {
		p := strings.ReplaceAll(r.Path, acct1, "{id}")
		called[r.Method+" "+p] = true
	}
	if len(called) != 7 {
		t.Fatalf("the account methods called %d distinct route(s) (%v), want 7", len(called), called)
	}
	covered := map[string]bool{}
	for _, c := range m3.Contracts() {
		covered[c.Route] = true
	}
	uncovered := m3.UncoveredRoutes()
	t.Logf("%d route(s) of the account methods covered by a contract; routes NOT covered: %v", len(called)-len(uncovered), uncovered)
	gap := func(called, covered map[string]bool, uncovered map[string]string) []string {
		var out []string
		for r := range called {
			_, u := uncovered[r]
			switch {
			case !covered[r] && !u:
				out = append(out, r+": called, covered by no contract and not declared uncovered")
			case covered[r] && u:
				out = append(out, r+": both covered by a contract and declared uncovered")
			}
		}
		for r := range uncovered {
			if !called[r] {
				out = append(out, r+": declared uncovered and never called by the adapter")
			}
		}
		sort.Strings(out)
		return out
	}
	if g := gap(called, covered, uncovered); len(g) != 0 {
		t.Fatalf("%v", g)
	}
	for r, why := range uncovered {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is declared uncovered with no reason", r)
		}
	}
	// the controls
	if g := gap(called, covered, map[string]string{}); !anyHas(g, "DELETE /v1/send-byo/accounts/{id}: called, covered by no contract and not declared uncovered") {
		t.Fatalf("control: dropping the declaration gave %v", g)
	}
	called["GET /v1/send-byo/nothing"] = true
	if g := gap(called, covered, uncovered); !anyHas(g, "GET /v1/send-byo/nothing: called, covered by no contract") {
		t.Fatalf("control: a route that nothing covers gave %v", g)
	}
	delete(called, "GET /v1/send-byo/nothing")
	covered[m3.RouteAccountsDelete] = true
	if g := gap(called, covered, uncovered); !anyHas(g, "both covered by a contract and declared uncovered") {
		t.Fatalf("control: a route that is in both gave %v", g)
	}
	// and the declared route is the constant the method's request path is named by
	if _, ok := uncovered[m3.RouteAccountsDelete]; !ok || m3.RouteAccountsDelete != "DELETE /v1/send-byo/accounts/{id}" {
		t.Fatalf("UncoveredRoutes = %v", uncovered)
	}
}
