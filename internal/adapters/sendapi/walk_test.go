package sendapi_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// buggyWalk is a deliberately wrong walker: it reads every page through the real page reader and then drops the last row of each page that has a successor, the way a look-ahead cursor
// bug does. The same assertion that passes the real walk must fail it.
func buggyWalk(ctx context.Context, c *sendapi.Client) ([]string, error) {
	var got []string
	cursor := ""
	for {
		page, err := c.ReportCampaignPage(ctx, campaign, cursor)
		if err != nil {
			return nil, err
		}
		items := ids(page.Items)
		if page.NextCursor != "" && len(items) > 0 {
			items = items[:len(items)-1]
		}
		got = append(got, items...)
		if page.NextCursor == "" {
			return got, nil
		}
		cursor = page.NextCursor
	}
}

// The walk reads every item of a campaign exactly once, in order, at page sizes that put the boundary at the start (1), in the middle (3, 4), at the end (the count itself and one more) and
// exactly on it (a count that is a multiple of the page size), for both modules' item spellings. The assertion is exact set equality (sendapitest.ExactSet: nothing missing, nothing extra,
// nothing twice, same order).
func TestWalkReadsEveryItemExactlyOnceAtEveryBoundary(t *testing.T) {
	for name, p := range profiles() {
		for _, n := range []int{0, 1, 6, 7} {
			for _, size := range []int{1, 2, 3, 4, 6, 7, 8, 200} {
				t.Run(fmt.Sprintf("%s/%d items/page size %d", name, n, size), func(t *testing.T) {
					e := newEnv(t, p, userA, fakeCampaign(p, userA, n, false).Handler(), modclient.Options{}, sendapi.Options{PageSize: size})
					rep, err := e.c.ReportCampaign(ctxT(t), campaign)
					if err != nil {
						t.Fatal(err)
					}
					if problems := sendapitest.ExactSet(ids(rep.Items), wantIDs(n)); len(problems) != 0 {
						t.Fatalf("the walk is not the exact set: %v", problems)
					}
					wantPages := (n + size - 1) / size
					if wantPages == 0 {
						wantPages = 1
					}
					if rep.Pages != wantPages || e.stub.Count() != wantPages {
						t.Fatalf("%d page(s), %d request(s), want %d", rep.Pages, e.stub.Count(), wantPages)
					}
					if !rep.ItemsMatchTotal() || rep.Totals.Total != int64(n) || rep.CampaignID != campaign {
						t.Fatalf("report header: %+v", rep.Totals)
					}
					if n == 0 && !rep.NothingKnown() {
						t.Fatal("an empty campaign is not NothingKnown")
					}
				})
			}
		}
	}
}

// The control of the walk: the SAME exact-set assertion catches (a) a buggy walker, (b) a module whose cursor is anchored on the look-ahead row (module 2's defect before its fix), and
// (c) a checker fed a duplicated or reordered listing. A test whose assertion cannot be seen to fail proves nothing.
func TestWalkControlsTheAssertionCatchesABrokenWalker(t *testing.T) {
	const n, size = 7, 3 // pages of 3, 3, 1: two boundaries
	p := m2like()
	good := newEnv(t, p, userA, fakeCampaign(p, userA, n, false).Handler(), modclient.Options{}, sendapi.Options{PageSize: size})
	rep, err := good.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || len(sendapitest.ExactSet(ids(rep.Items), wantIDs(n))) != 0 {
		t.Fatalf("the control walk is not exact: %v", err)
	}

	// (a) the buggy walker, against a correct module
	got, err := buggyWalk(ctxT(t), good.c)
	if err != nil {
		t.Fatal(err)
	}
	problems := sendapitest.ExactSet(got, wantIDs(n))
	if len(problems) != 2 || !strings.HasPrefix(problems[0], "missing") {
		t.Fatalf("the buggy walker was not caught for the right reason: %v", problems)
	}

	// (b) the real walker against a module with the look-ahead cursor bug
	bad := newEnv(t, p, userA, fakeCampaign(p, userA, n, true).Handler(), modclient.Options{}, sendapi.Options{PageSize: size})
	rep, err = bad.c.ReportCampaign(ctxT(t), campaign)
	if err != nil {
		t.Fatal(err)
	}
	problems = sendapitest.ExactSet(ids(rep.Items), wantIDs(n))
	if len(problems) != 1 || problems[0] != "missing "+idOf(4) { // the look-ahead row of the first boundary is skipped
		t.Fatalf("the look-ahead bug was not caught for the right reason: %v", problems)
	}
	if rep.ItemsMatchTotal() {
		t.Fatal("the module's own total agrees with the lossy listing, so the caller's completeness check could not see the loss")
	}

	// (c) the checker itself
	w := wantIDs(3)
	for name, got := range map[string][]string{
		"a duplicate": {w[0], w[1], w[1], w[2]},
		"reordered":   {w[1], w[0], w[2]},
		"extra":       {w[0], w[1], w[2], "zz"},
		"empty":       nil,
	} {
		if len(sendapitest.ExactSet(got, w)) == 0 {
			t.Fatalf("the exact-set check passed %s", name)
		}
	}
	if len(sendapitest.ExactSet(w, w)) != 0 {
		t.Fatal("the exact-set check refused an exact listing")
	}
}

// A cursor that repeats, one that does not advance, an empty page that names a next cursor, a walk that never ends and the same item on two pages are each a Contract error naming the key:
// never an infinite loop and never a listing. The control is the well-behaved module.
func TestWalkRefusesWhatWouldLoopOrLie(t *testing.T) {
	p := m2like()
	page := func(cursor string, which ...int) string {
		items := []any{}
		for _, i := range which {
			items = append(items, itemJSON(p, i, "sent"))
		}
		next := any(nil)
		if cursor != "" {
			next = cursor
		}
		b := header(p)(len(which))
		b["campaign_id"] = campaign
		b["items"] = map[string]any{"results": items, "next_cursor": next, "page_size": 2}
		return jsonOf(b)
	}
	for name, tc := range map[string]struct {
		h       handler
		opts    sendapi.Options
		key     string
		wantErr bool
	}{
		"the next cursor is the one just used": {func(w http.ResponseWriter, r *http.Request, n int) {
			sendapitest.JSON(w, 200, page("c1", n))
		}, sendapi.Options{}, "items.next_cursor", true},
		"a cursor comes back after another": {func(w http.ResponseWriter, r *http.Request, n int) {
			sendapitest.JSON(w, 200, page([]string{"c1", "c2", "c1"}[(n-1)%3], n))
		}, sendapi.Options{}, "items.next_cursor", true},
		"an empty page with a next cursor": {func(w http.ResponseWriter, r *http.Request, n int) {
			sendapitest.JSON(w, 200, page("c1"))
		}, sendapi.Options{}, "items.next_cursor", true},
		"a walk that never ends": {func(w http.ResponseWriter, r *http.Request, n int) {
			sendapitest.JSON(w, 200, page("c"+strconv.Itoa(n), n))
		}, sendapi.Options{MaxPages: 4}, "items.next_cursor", true},
		"the same item on two pages": {func(w http.ResponseWriter, r *http.Request, n int) {
			if n == 1 {
				sendapitest.JSON(w, 200, page("c1", 1, 2))
				return
			}
			sendapitest.JSON(w, 200, page("", 2, 3))
		}, sendapi.Options{}, "items.results", true},
		"a well-behaved module (control)": {func(w http.ResponseWriter, r *http.Request, n int) {
			if n == 1 {
				sendapitest.JSON(w, 200, page("c1", 1, 2))
				return
			}
			sendapitest.JSON(w, 200, page("", 3))
		}, sendapi.Options{}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, p, userA, tc.h, modclient.Options{}, tc.opts)
			rep, err := e.c.ReportCampaign(ctxT(t), campaign)
			if !tc.wantErr {
				if err != nil || len(rep.Items) != 3 {
					t.Fatalf("the control is refused: %v %+v", err, rep)
				}
				return
			}
			ce := wantKind(t, err, modclient.Contract)
			if ce.Key != tc.key {
				t.Fatalf("key %q, want %q: %v", ce.Key, tc.key, err)
			}
			if !reflect.DeepEqual(rep, sendapi.CampaignReport{}) {
				t.Fatalf("a report came back with the error: %+v", rep)
			}
			if e.stub.Count() > 6 {
				t.Fatalf("%d requests: the walk did not stop", e.stub.Count())
			}
		})
	}
}

// A page that cannot be read fails the whole walk and no listing comes back, whatever fails: the module going away, a fault, a pause, a body that is not the shape. The pages before it were
// handed to the callback (a caller that settles page by page has seen a prefix) and the error says so; Walk returns no stats and ReportCampaign no report. The control is the walk that is
// not interrupted.
func TestAFailedPageFailsTheWholeWalk(t *testing.T) {
	p := m2like()
	for name, tc := range map[string]struct {
		fail handler
		kind modclient.Kind
	}{
		"module fault":    {reply(500, sendapitest.Envelope("internal_error")), modclient.Internal},
		"paused":          {reply(503, sendapitest.Envelope("paused")), modclient.Paused},
		"unavailable":     {reply(503, sendapitest.Envelope("unavailable")), modclient.Unavailable},
		"truncated":       {reply(200, `{"campaign_id":"`+campaign+`","items":{"res`), modclient.Contract},
		"another shape":   {reply(200, `{"campaign_id":"`+campaign+`"}`), modclient.Contract},
		"a refusal":       {reply(401, sendapitest.Envelope("unauthenticated")), modclient.Refused},
		"rate limited":    {reply(429, sendapitest.Envelope("rate_limited")), modclient.RateLimited},
		"gone mid-answer": {func(w http.ResponseWriter, r *http.Request, n int) { hijackClose(w) }, modclient.Unavailable},
	} {
		t.Run(name, func(t *testing.T) {
			good := fakeCampaign(p, userA, 9, false).Handler()
			e := newEnv(t, p, userA, func(w http.ResponseWriter, r *http.Request, n int) {
				if n >= 3 { // not only the third: Go's transport silently repeats a GET whose reused connection died, and that repeat must fail too
					tc.fail(w, r, n)
					return
				}
				good(w, r, n)
			}, modclient.Options{}, sendapi.Options{PageSize: 2})
			delivered := 0
			st, err := e.c.Walk(ctxT(t), campaign, func(sendapi.CampaignPage) error { delivered++; return nil })
			ce := wantKind(t, err, tc.kind)
			if st != (sendapi.WalkStats{}) {
				t.Fatalf("stats came back with the error: %+v", st)
			}
			if delivered != 2 || !strings.Contains(err.Error(), "page 3") || !strings.Contains(err.Error(), "NOT complete") || ce.Route != "m2.report.campaign" {
				t.Fatalf("delivered %d page(s); error %v", delivered, err)
			}
			rep, err := newEnv(t, p, userA, func(w http.ResponseWriter, r *http.Request, n int) {
				if n >= 3 {
					tc.fail(w, r, n)
					return
				}
				good(w, r, n)
			}, modclient.Options{}, sendapi.Options{PageSize: 2}).c.ReportCampaign(ctxT(t), campaign)
			wantKind(t, err, tc.kind)
			if !reflect.DeepEqual(rep, sendapi.CampaignReport{}) {
				t.Fatalf("a partial report came back: %d item(s)", len(rep.Items))
			}
		})
	}
	// control: the same 9 items at the same page size with no failure
	e := newEnv(t, p, userA, fakeCampaign(p, userA, 9, false).Handler(), modclient.Options{}, sendapi.Options{PageSize: 2})
	st, err := e.c.Walk(ctxT(t), campaign, func(sendapi.CampaignPage) error { return nil })
	if err != nil || st.Pages != 5 || st.Items != 9 {
		t.Fatalf("control: %v %+v", err, st)
	}
}

// An error of the callback ends the walk and comes back as it is (the caller's own error is not wrapped into a module error); a nil callback is refused before any request.
func TestWalkCallbackErrorsAndNilCallback(t *testing.T) {
	p := m2like()
	e := newEnv(t, p, userA, fakeCampaign(p, userA, 5, false).Handler(), modclient.Options{}, sendapi.Options{PageSize: 2})
	boom := errors.New("the caller's own failure")
	st, err := e.c.Walk(ctxT(t), campaign, func(sendapi.CampaignPage) error { return boom })
	if !errors.Is(err, boom) || modclient.KindOf(err) != 0 || st != (sendapi.WalkStats{}) || e.stub.Count() != 1 {
		t.Fatalf("%v %+v after %d request(s)", err, st, e.stub.Count())
	}
	if _, err := e.c.Walk(ctxT(t), campaign, nil); modclient.KindOf(err) != modclient.Invalid || e.stub.Count() != 1 {
		t.Fatalf("a nil callback: %v", err)
	}
}

// A cursor the module does not accept is a typed error of its own that still unwraps to the module's Kind: a 400 validation_failed naming the cursor, for a cursor that is not base64 and for
// one that decodes but is not an index and an id alike (both modules answer so since Phase 4 A1; the forged one used to be a 500). The control is the same status on a request that carried
// NO cursor, and a 400 that is not about the cursor: neither is ErrBadCursor (and TestAServerFaultOnACursorRequestIsNotABadCursor holds the 500 with a cursor).
func TestForgedCursorIsATypedError(t *testing.T) {
	p := m2like()
	c := fakeCampaign(p, userA, 5, false)
	e := newEnv(t, p, userA, c.Handler(), modclient.Options{}, sendapi.Options{PageSize: 2})
	forged := base64.RawURLEncoding.EncodeToString([]byte("x|y")) // decodes, has a '|', is not an index and an id
	_, err := e.c.ReportCampaignPage(ctxT(t), campaign, forged)
	me := wantKind(t, err, modclient.Refused)
	if !errors.Is(err, sendapi.ErrBadCursor) || !errors.Is(err, modclient.ErrRefused) || me.Code != "validation_failed" || !hasCursorDetail(me) {
		t.Fatalf("a forged cursor: %v (code %q, details %v)", err, me.Code, me.Details)
	}
	_, err = e.c.ReportCampaignPage(ctxT(t), campaign, "not base64 !!")
	me = wantKind(t, err, modclient.Refused)
	if !errors.Is(err, sendapi.ErrBadCursor) || !errors.Is(err, modclient.ErrRefused) || me.Code != "validation_failed" || !hasCursorDetail(me) {
		t.Fatalf("a malformed cursor: %v (code %q, details %v)", err, me.Code, me.Details)
	}
	// controls
	h500 := newEnv(t, p, userA, reply(500, sendapitest.Envelope("internal_error")), modclient.Options{}, sendapi.Options{})
	if _, err := h500.c.ReportCampaignPage(ctxT(t), campaign, ""); modclient.KindOf(err) != modclient.Internal || errors.Is(err, sendapi.ErrBadCursor) {
		t.Fatalf("a 500 with no cursor is read as a bad cursor: %v", err)
	}
	h400 := newEnv(t, p, userA, reply(400, sendapitest.Envelope("validation_failed", [2]string{"page_size", "malformed"})), modclient.Options{}, sendapi.Options{})
	if _, err := h400.c.ReportCampaignPage(ctxT(t), campaign, "abc"); modclient.KindOf(err) != modclient.Refused || errors.Is(err, sendapi.ErrBadCursor) {
		t.Fatalf("a 400 about another field is read as a bad cursor: %v", err)
	}
	h503 := newEnv(t, p, userA, reply(503, sendapitest.Envelope("unavailable")), modclient.Options{}, sendapi.Options{})
	if _, err := h503.c.ReportCampaignPage(ctxT(t), campaign, "abc"); errors.Is(err, sendapi.ErrBadCursor) {
		t.Fatalf("an outage with a cursor is read as a bad cursor: %v", err)
	}
	// and the real cursor of the module works
	page, err := e.c.ReportCampaignPage(ctxT(t), campaign, "")
	if err != nil || page.NextCursor == "" {
		t.Fatalf("the first page: %v %+v", err, page)
	}
	if _, err := e.c.ReportCampaignPage(ctxT(t), campaign, page.NextCursor); err != nil {
		t.Fatalf("the module's own cursor is refused: %v", err)
	}
}

// hasCursorDetail: the module named exactly one thing wrong, the cursor, as malformed (the one answer both modules give to a cursor they do not accept).
func hasCursorDetail(e *modclient.Error) bool {
	return len(e.Details) == 1 && e.Details[0] == modclient.Detail{Field: "cursor", Issue: "malformed"}
}

// A 500 on a request that carried a cursor is a module fault and nothing else: the adapter returns the Internal error as it is and does NOT claim it as a bad cursor (it used to, because
// module 3 answered a forged cursor with a 500; since Phase 4 A1 no module does, so a 500 cannot be told from a fault and is not guessed to be one). The paired control is the same request
// answered with the 400 validation_failed whose detail is the cursor, which IS ErrBadCursor (and a Refused error, not an Internal one): if the 500 were mapped to the sentinel again, or the
// 400 stopped being mapped, one of the two halves goes red. Both profiles, and the request is proved to have carried the cursor (the mapping is only reached with one).
func TestAServerFaultOnACursorRequestIsNotABadCursor(t *testing.T) {
	cursor := base64.RawURLEncoding.EncodeToString([]byte("0|" + idOf(1)))
	carried := func(t *testing.T, e *env) {
		t.Helper()
		reqs := e.stub.Requests()
		if len(reqs) == 0 {
			t.Fatal("the stub saw no request, so the answer proves nothing")
		}
		for _, r := range reqs {
			if r.Query.Get("cursor") != cursor {
				t.Fatalf("a request did not carry the cursor (query %v): the mapping under test was not reached", r.Query)
			}
		}
	}
	for name, p := range profiles() {
		t.Run(name, func(t *testing.T) {
			fault := newEnv(t, p, userA, reply(500, sendapitest.Envelope("internal_error")), modclient.Options{}, sendapi.Options{})
			_, err := fault.c.ReportCampaignPage(ctxT(t), campaign, cursor)
			me := wantKind(t, err, modclient.Internal)
			if me.Status != 500 || errors.Is(err, sendapi.ErrBadCursor) || !errors.Is(err, modclient.ErrInternal) {
				t.Fatalf("a 500 on a cursor request: status %d, %v: it must stay a plain Internal error and not be ErrBadCursor", me.Status, err)
			}
			carried(t, fault)
			// control: the 400 validation_failed naming the cursor is a bad cursor, through the same call
			bad := newEnv(t, p, userA, reply(400, sendapitest.Envelope("validation_failed", [2]string{"cursor", "malformed"})), modclient.Options{}, sendapi.Options{})
			_, err = bad.c.ReportCampaignPage(ctxT(t), campaign, cursor)
			me = wantKind(t, err, modclient.Refused)
			if me.Status != 400 || me.Code != "validation_failed" || !hasCursorDetail(me) || !errors.Is(err, sendapi.ErrBadCursor) || errors.Is(err, modclient.ErrInternal) {
				t.Fatalf("a 400 naming the cursor: status %d, code %q, details %v, %v: it must be ErrBadCursor", me.Status, me.Code, me.Details, err)
			}
			carried(t, bad)
		})
	}
}

// The cursor the module issues is passed back verbatim (never edited, never built), page_size is the Options dial, and the query always names the campaign and the tenant.
func TestCursorIsPassedBackVerbatim(t *testing.T) {
	p := m2like()
	e := newEnv(t, p, userA, fakeCampaign(p, userA, 5, false).Handler(), modclient.Options{}, sendapi.Options{PageSize: 2})
	if _, err := e.c.ReportCampaign(ctxT(t), campaign); err != nil {
		t.Fatal(err)
	}
	reqs := e.stub.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	var issued []string
	for i, r := range reqs {
		if r.Query.Get("page_size") != "2" || r.Query.Get("campaign_id") != campaign || r.Query.Get("user_id") != userA || r.Query.Get("job_id") != "" {
			t.Fatalf("request %d: %v", i, r.Query)
		}
		if i == 0 && r.Query.Has("cursor") {
			t.Fatal("the first request carries a cursor")
		}
		if i > 0 && r.Query.Get("cursor") != issued[i-1] {
			t.Fatalf("request %d cursor %q, the module issued %q", i, r.Query.Get("cursor"), issued[i-1])
		}
		// what the module issued on this page is what the next request must carry: read it back through a fresh walk of single pages
		page, err := e.c.ReportCampaignPage(ctxT(t), campaign, r.Query.Get("cursor"))
		if err != nil {
			t.Fatal(err)
		}
		issued = append(issued, page.NextCursor)
	}
	// default page size when none is set
	d := newEnv(t, p, userA, fakeCampaign(p, userA, 1, false).Handler(), modclient.Options{}, sendapi.Options{})
	if _, err := d.c.ReportCampaign(ctxT(t), campaign); err != nil {
		t.Fatal(err)
	}
	if got := d.stub.Requests()[0].Query.Get("page_size"); got != strconv.Itoa(sendapi.DefaultPageSize) {
		t.Fatalf("default page_size %q", got)
	}
}

// A campaign that is unknown, a typo or another tenant's answers the same empty 200 (never a 404): nothing is known, and the adapter says only that. The paired control: the owner reads
// the same campaign id and sees its items, so the empty answer is about the tenant and not about the id.
func TestAnotherTenantsCampaignIsNothingKnown(t *testing.T) {
	p := m2like()
	h := fakeCampaign(p, userA, 4, false).Handler()
	owner := newEnv(t, p, userA, h, modclient.Options{}, sendapi.Options{})
	rep, err := owner.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || len(rep.Items) != 4 || rep.NothingKnown() {
		t.Fatalf("the owner: %v %+v", err, rep)
	}
	stranger := newEnv(t, p, userB, h, modclient.Options{}, sendapi.Options{})
	rep, err = stranger.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || !rep.NothingKnown() || !rep.ItemsMatchTotal() || rep.Totals.Total != 0 || rep.Progress.IsComplete {
		t.Fatalf("a stranger: %v %+v", err, rep)
	}
	typo := newEnv(t, p, userA, h, modclient.Options{}, sendapi.Options{})
	rep, err = typo.c.ReportCampaign(ctxT(t), "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a03")
	if err != nil || !rep.NothingKnown() {
		t.Fatalf("a typo: %v %+v", err, rep)
	}
}

// A campaign page that contradicts itself is a Contract error naming the key: a negative count, a report for another campaign, a page size outside the module's range, more items than the
// page size the module says it used, an empty or oversized cursor. The control is the unmodified page.
func TestACampaignPageThatContradictsItselfIsRefused(t *testing.T) {
	p := m2like()
	build := func(mut func(m map[string]any)) handler {
		return func(w http.ResponseWriter, r *http.Request, n int) {
			cp := fakeCampaign(p, userA, 3, false)
			cp.Header = func(total int) map[string]any {
				h := header(p)(total)
				mut(h)
				return h
			}
			cp.Handler()(w, r, n)
		}
	}
	set := func(path string, v any) func(m map[string]any) {
		return func(m map[string]any) {
			parts := strings.Split(path, ".")
			cur := m
			for _, k := range parts[:len(parts)-1] {
				cur = cur[k].(map[string]any)
			}
			cur[parts[len(parts)-1]] = v
		}
	}
	for name, tc := range map[string]struct {
		mut func(m map[string]any)
		key string
	}{
		"negative total":        {set("totals.total", -1), "totals.total"},
		"negative terminal":     {set("progress.terminal", -2), "progress.terminal"},
		"total of a wrong type": {set("totals.total", "3"), "totals.total"},
		"missing is_complete":   {func(m map[string]any) { delete(m["progress"].(map[string]any), "is_complete") }, "progress.is_complete"},
		"missing totals":        {func(m map[string]any) { delete(m, "totals") }, "totals"},
		"missing cancelled":     {func(m map[string]any) { delete(m["totals"].(map[string]any), "cancelled") }, "totals.cancelled"},
		"the control":           {func(m map[string]any) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, p, userA, build(tc.mut), modclient.Options{}, sendapi.Options{})
			page, err := e.c.ReportCampaignPage(ctxT(t), campaign, "")
			if tc.key == "" {
				if err != nil || len(page.Items) != 3 {
					t.Fatalf("the control is refused: %v", err)
				}
				return
			}
			if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key {
				t.Fatalf("key %q, want %q: %v", ce.Key, tc.key, err)
			}
			if !isZero(page) {
				t.Fatalf("a page came back with the error: %+v", page)
			}
		})
	}
	// the page-level contradictions need a body the fake module would not produce
	other := fakeCampaign(p, userA, 3, false)
	otherID := "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a99"
	for name, tc := range map[string]struct {
		mut func(out map[string]any)
		key string
	}{
		"another campaign": {func(o map[string]any) { o["campaign_id"] = otherID }, "campaign_id"},
		"page size zero":   {func(o map[string]any) { o["items"].(map[string]any)["page_size"] = 0 }, "items.page_size"},
		"page size 201":    {func(o map[string]any) { o["items"].(map[string]any)["page_size"] = 201 }, "items.page_size"},
		"more items than the page size says": {func(o map[string]any) {
			o["items"].(map[string]any)["page_size"] = 2
		}, "items.results"},
		"an empty cursor": {func(o map[string]any) { o["items"].(map[string]any)["next_cursor"] = "" }, "items.next_cursor"},
		"a cursor longer than any issued": {func(o map[string]any) {
			o["items"].(map[string]any)["next_cursor"] = strings.Repeat("a", 2000)
		}, "items.next_cursor"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, p, userA, func(w http.ResponseWriter, r *http.Request, n int) {
				rec := httptest.NewRecorder()
				other.Handler()(rec, r, n)
				out := decodeMap(t, rec.Body.Bytes())
				tc.mut(out)
				sendapitest.JSON(w, 200, jsonOf(out))
			}, modclient.Options{}, sendapi.Options{})
			_, err := e.c.ReportCampaignPage(ctxT(t), campaign, "")
			if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key {
				t.Fatalf("key %q, want %q: %v", ce.Key, tc.key, err)
			}
		})
	}
}

// The module echoes the page size it used (it clamps what is asked, silently): the walk follows the echo, and a module that clamps to 100 while 200 was asked is read correctly.
func TestThePageSizeIsReadFromTheModule(t *testing.T) {
	p := m2like()
	const n = 250
	good := fakeCampaign(p, userA, n, false).Handler()
	e := newEnv(t, p, userA, func(w http.ResponseWriter, r *http.Request, i int) {
		q := r.URL.Query()
		q.Set("page_size", "100") // the module clamps whatever was asked to 100
		r.URL.RawQuery = q.Encode()
		good(w, r, i)
	}, modclient.Options{}, sendapi.Options{PageSize: 200})
	rep, err := e.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || rep.Pages != 3 || len(sendapitest.ExactSet(ids(rep.Items), wantIDs(n))) != 0 {
		t.Fatalf("%v pages=%d", err, rep.Pages)
	}
	if got := e.stub.Requests()[0].Query.Get("page_size"); got != "200" {
		t.Fatalf("the adapter asked for %q", got)
	}
}

// The totals of a report are the last page's, and the caller's completeness check is ItemsMatchTotal: a module whose total says 9 while 7 items were listed does not match, and the report
// still comes back (the listing is complete as a read; whether it is the whole campaign is the caller's question).
func TestItemsMatchTotalIsTheCallersCompletenessCheck(t *testing.T) {
	p := m2like()
	c := fakeCampaign(p, userA, 7, false)
	c.Header = func(int) map[string]any { return header(p)(9) }
	e := newEnv(t, p, userA, c.Handler(), modclient.Options{}, sendapi.Options{PageSize: 3})
	rep, err := e.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || len(rep.Items) != 7 || rep.Totals.Total != 9 || rep.ItemsMatchTotal() || rep.NothingKnown() {
		t.Fatalf("%v %d items, total %d", err, len(rep.Items), rep.Totals.Total)
	}
}

// Module 3's per-account totals ride along on the same walk.
func TestAccountTotalsAreCarried(t *testing.T) {
	p := m3like()
	e := newEnv(t, p, userA, sendapitest.Serve(200, sendapitest.Read(t, fixtures, "report.campaign.last_page.json")), modclient.Options{}, sendapi.Options{})
	rep, err := e.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || len(rep.ByAccount) != 1 || rep.ByAccount[0].AccountUsername != "sender-1@example.test" || rep.ByAccount[0].Sent != 1 {
		t.Fatalf("%v %+v", err, rep.ByAccount)
	}
	for _, it := range rep.Items {
		if it.AccountID != nil {
			t.Fatalf("a queued item has an account: %+v", it)
		}
	}
	if rep.Items[0].SMTPResponse == nil || rep.Items[1].SMTPResponse != nil {
		t.Fatalf("smtp_response of the two queued items: %+v %+v", rep.Items[0].SMTPResponse, rep.Items[1].SMTPResponse)
	}
	// control: module 2's profile has no totals_by_account to carry
	m2 := newEnv(t, m2like(), userA, fakeCampaign(m2like(), userA, 2, false).Handler(), modclient.Options{}, sendapi.Options{})
	rep, err = m2.c.ReportCampaign(ctxT(t), campaign)
	if err != nil || rep.ByAccount != nil {
		t.Fatalf("%v %+v", err, rep.ByAccount)
	}
}
