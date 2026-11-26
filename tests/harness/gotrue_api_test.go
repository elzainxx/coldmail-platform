//go:build harness && gotrue

package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 4, chunk B15 (Docs/phase-4-spec.md "Locked here" 29, exit criterion "API auth: cross-user reads refused (paired control); bad tokens refused"): the platform's job HTTP API, in this process, over REAL tokens
// that the real GoTrue issued. httptest serves the real httpapi handler with the real internal/api mounted exactly as internal/app mounts it (api.New over the job store, the verifier and the tenant resolver, then
// httpapi.Deps.Mount), over the harness database as platform_app; the verifier is the one gotrue_test.go builds from the stack's JWT secret, issuer and the audience `authenticated`. Nothing is mocked.
//
// What gotrue_test.go proves of internal/auth on its own (each way a token can be wrong, refused with ITS OWN sentinel) is proved here one layer up: through the HTTP API, every refusal is the SAME 401, byte for
// byte but for the request id, and a user can read their own jobs and nobody else's.

// gtrReply is one answer of the API.
type gtrReply struct {
	Status int
	Header http.Header
	Body   []byte
}

// gtrAPI is the API under test.
type gtrAPI struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client
}

// get sends one GET with the Authorization header exactly as given (nil sends none).
func (a *gtrAPI) get(path string, authorization *string) gtrReply {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.srv.URL+path, nil)
	if err != nil {
		a.t.Fatalf("build GET %s: %v", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if authorization != nil {
		req.Header.Set("Authorization", *authorization)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		a.t.Fatalf("GET %s: the request failed: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		a.t.Fatalf("GET %s: reading the answer: %v", path, err)
	}
	return gtrReply{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// gtrNormal is an answer with its request id blanked: two refusals that are one answer are equal in this form, byte for byte.
func gtrNormal(r gtrReply) string {
	return fmt.Sprintf("%d %s %s", r.Status, r.Header.Get("WWW-Authenticate"), regexpRID.ReplaceAllString(string(r.Body), "req_X"))
}

func gtrCut(s string) string {
	if len(s) > 220 {
		return s[:220] + "..."
	}
	return s
}

// gtrIdenticalRefusals is the check, as a pure function so that a control can feed it doctored answers: every answer is a 401 carrying `WWW-Authenticate: Bearer` and the first one's body byte for byte (the
// request id aside). It reports every way they differ.
func gtrIdenticalRefusals(names []string, rs []gtrReply) []string {
	if len(rs) == 0 || len(rs) != len(names) {
		return []string{"no refusal to compare: the instrument matched nothing"}
	}
	var p []string
	ref := gtrNormal(rs[0])
	for i, r := range rs {
		if r.Status != http.StatusUnauthorized {
			p = append(p, fmt.Sprintf("%s: HTTP %d, want 401", names[i], r.Status))
		}
		if got := r.Header.Get("WWW-Authenticate"); got != "Bearer" {
			p = append(p, fmt.Sprintf("%s: WWW-Authenticate is %q, want Bearer", names[i], got))
		}
		if got := gtrNormal(r); got != ref {
			p = append(p, fmt.Sprintf("%s: the answer differs from the first refusal's (%s | %s)", names[i], gtrCut(got), gtrCut(ref)))
		}
	}
	return p
}

// gtrResign re-signs the token's own header with a payload the test changes, under the REAL secret: a token that is genuinely signed and wrong in one claim.
func gtrResign(e *gtrEnv, tok config.Secret, mutate func(map[string]any)) string {
	hdr, pl, _, ok := gtrParts(tok)
	if !ok {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(pl, &m) != nil {
		return ""
	}
	mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return gtrSignHS256(e.secret.Reveal(), hdr, out)
}

// gtrJobIDs reads the ids of GET /v1/jobs' answer, sorted.
func gtrJobIDs(t *testing.T, what string, r gtrReply) []string {
	t.Helper()
	if r.Status != http.StatusOK {
		t.Fatalf("%s: HTTP %d, want 200: %s", what, r.Status, gtrCut(string(r.Body)))
	}
	var body struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("%s: the answer is not the job list: %v", what, err)
	}
	ids := make([]string, 0, len(body.Jobs))
	for _, j := range body.Jobs {
		ids = append(ids, j.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestGoTrue_API_ARealTokenIsAcceptedATamperedOneRefusedAndUsersAreIsolated: two users sign in at the real GoTrue (the platform user of each is made as an admin makes it); user A has two job drafts and user B one.
//
//	accepted   GET /v1/jobs with A's real access token is 200 and lists A's two jobs and nothing else (B's id is nowhere in the answer); with B's it lists B's one
//	refused    the SAME request with the token's signature tampered, with the payload tampered (B's id as the sub, under A's signature), with a token validly signed but expired an hour ago, with one validly signed for
//	           another audience, with `Authorization: Bearer ` and no token, with no header at all, and with a REAL token of a GoTrue login that has no platform user: each is ONE identical 401 (the status, the
//	           `WWW-Authenticate: Bearer` header and the body, byte for byte but for the request id), and none of them lists, names or leaks a job
//	isolated   B's token on A's job id is 404, and that answer is byte for byte the answer for a job id that does not exist; A's token on the same id is 200 and names the job
//
// THE CONTROLS: before and after the refusals A's genuine token is 200 again (the refusals are about the alterations, not the setup); each altered token is shown to be refused by the verifier ITSELF for the
// reason the alteration names (a signature, an expiry, an audience) and the real login with no platform user is shown to carry a token the verifier accepts (so its 401 is the resolver's); and the identical-401
// comparison is shown to FAIL on a doctored set (a 403 among the 401s, a body that differs, a 200 among them, an empty set).
func TestGoTrue_API_ARealTokenIsAcceptedATamperedOneRefusedAndUsersAreIsolated(t *testing.T) {
	e := newGtrEnv(t)

	// the platform side, exactly as internal/app builds it: the store over the pool as platform_app, the resolver, the API over them with the real verifier, mounted on the handler
	pool := mnyPool(t, requireEnv(t, envAdminURL))
	res, err := tenant.NewResolver(pool, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	store, err := jobs.New(pool, money.New(pool))
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	apiH, err := api.New(api.Deps{Service: store, Verifier: e.v, Resolver: res})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{Pinger: pool, ReadyTimeout: 5 * time.Second, Mount: apiH.Mount}))
	t.Cleanup(srv.Close)
	a := &gtrAPI{t: t, srv: srv, hc: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}

	// two users who signed in at GoTrue, a platform user for each, and a third login that has none
	la, lb, lc := e.signup("api-a"), e.signup("api-b"), e.signup("api-nobody")
	e.seedPlatformUser(la)
	e.seedPlatformUser(lb)
	idA, idB := la.Session.User.ID, lb.Session.User.ID
	draft := func(user string, n int) []string {
		var ids []string
		for i := 0; i < n; i++ {
			j, err := store.CreateDraft(e.ctx, user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: jlRowsOf(jlAddrs("gta"+randomHex(t, 3), 3, 1))})
			if err != nil {
				t.Fatalf("CreateDraft for %s: %v", gtrShort(user), err)
			}
			ids = append(ids, j.ID)
			jid := j.ID
			t.Cleanup(func() { _, _ = store.DiscardDraft(e.ctx, user, jid) })
		}
		sort.Strings(ids)
		return ids
	}
	jobsA, jobsB := draft(idA, 2), draft(idB, 1)
	bearer := func(tok string) *string { s := "Bearer " + tok; return &s }
	tokA, tokB := la.Session.AccessToken, lb.Session.AccessToken
	good, goodB := tokA.Reveal(), tokB.Reveal()

	// ---- accepted: a real token, the user's own jobs and nobody else's ----
	rA := a.get("/v1/jobs", bearer(good))
	if got := gtrJobIDs(t, "A's real token on GET /v1/jobs", rA); strings.Join(got, ",") != strings.Join(jobsA, ",") {
		t.Fatalf("A's real token lists %v, want exactly A's own jobs %v", got, jobsA)
	}
	if bytes.Contains(rA.Body, []byte(jobsB[0])) {
		t.Fatalf("A's list holds B's job id: users are not isolated")
	}
	rB := a.get("/v1/jobs", bearer(goodB))
	if got := gtrJobIDs(t, "B's real token on GET /v1/jobs", rB); strings.Join(got, ",") != strings.Join(jobsB, ",") {
		t.Fatalf("B's real token lists %v, want exactly B's own jobs %v", got, jobsB)
	}
	for _, id := range jobsA {
		if bytes.Contains(rB.Body, []byte(id)) {
			t.Fatalf("B's list holds A's job id: users are not isolated")
		}
	}

	// ---- refused: every way a token or a header can be wrong is ONE identical 401 ----
	otherSub := gtrTamperPayload(tokA, func(m map[string]any) { m["sub"] = idB })
	expired := gtrResign(e, tokA, func(m map[string]any) {
		m["iat"], m["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	})
	otherAud := gtrResign(e, tokA, func(m map[string]any) { m["aud"] = "service" })
	empty := "Bearer "
	cases := []struct {
		name   string
		token  string // "" when the case has no token to check against the verifier
		header *string
		why    error // what the verifier ITSELF must say of the token; nil when the verifier accepts it or there is no token
	}{
		{"one flipped character in the signature", gtrFlipSignatureChar(good), bearer(gtrFlipSignatureChar(good)), auth.ErrSignature},
		{"another sub under A's signature", otherSub, bearer(otherSub), auth.ErrSignature},
		{"a validly signed token that expired an hour ago", expired, bearer(expired), auth.ErrExpired},
		{"a validly signed token for another audience", otherAud, bearer(otherAud), auth.ErrAudience},
		{"Authorization: Bearer with no token", "", &empty, nil},
		{"no Authorization header", "", nil, nil},
		{"a real token of a login with no platform user", lc.Session.AccessToken.Reveal(), bearer(lc.Session.AccessToken.Reveal()), nil},
	}
	names := make([]string, len(cases))
	replies := make([]gtrReply, len(cases))
	for i, c := range cases {
		names[i] = c.name
		if c.token == "" && c.header != nil && c.header != &empty {
			t.Fatalf("%s: the test could not build the token", c.name)
		}
		if c.token != "" {
			claims, err := e.v.Verify(e.ctx, c.token)
			switch {
			case c.why != nil && !errors.Is(err, c.why):
				t.Errorf("control: the verifier answered %v for %q, want %v: the 401 below would not be about the alteration the case names", err, c.name, c.why)
			case c.why == nil && err != nil:
				t.Errorf("control: the verifier refused %q (%v): this login's token is genuine, so the 401 must be the resolver's and the case proves nothing about it", c.name, err)
			case c.why != nil && claims.Subject != "":
				t.Errorf("control: a refused token still returned claims (%q)", c.name)
			}
		}
		replies[i] = a.get("/v1/jobs", c.header)
		if ids := gtrJobIDs2(replies[i]); len(ids) != 0 || bytes.Contains(replies[i].Body, []byte(jobsA[0])) || bytes.Contains(replies[i].Body, []byte(jobsB[0])) {
			t.Errorf("%s: the refusal lists or names a job", c.name)
		}
	}
	if p := gtrIdenticalRefusals(names, replies); len(p) != 0 {
		t.Errorf("the refusals are not one identical 401 (%d problem(s)):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	// the refusals did not lock anybody out, and the same request with the genuine token is still the user's own list
	if got := gtrJobIDs(t, "A's real token after the refusals", a.get("/v1/jobs", bearer(good))); strings.Join(got, ",") != strings.Join(jobsA, ",") {
		t.Errorf("control: after the refusals A's real token lists %v, want %v", got, jobsA)
	}
	// the same refusals on a single job's route are the same 401 (the middleware is one, in front of every route)
	one := a.get("/v1/jobs/"+jobsA[0], bearer(gtrFlipSignatureChar(good)))
	if p := gtrIdenticalRefusals([]string{"a tampered token on GET /v1/jobs/{id}", "the first refusal of GET /v1/jobs"}, []gtrReply{one, replies[0]}); len(p) != 0 {
		t.Errorf("a tampered token on a job's own route is not the same 401 as on the list: %v", p)
	}

	// ---- the comparison itself can fail ----
	fake := func(status int, www, body string) gtrReply {
		return gtrReply{Status: status, Header: http.Header{"Www-Authenticate": []string{www}}, Body: []byte(body)}
	}
	env := `{"error":{"code":"unauthenticated","message":"sign in again","request_id":"req_00000000000000000000000000000001"}}`
	envOther := `{"error":{"code":"unauthenticated","message":"your token expired","request_id":"req_00000000000000000000000000000002"}}`
	pair := []string{"first", "second"}
	if p := gtrIdenticalRefusals(pair, []gtrReply{fake(401, "Bearer", env), fake(401, "Bearer", strings.ReplaceAll(env, "000001", "000002"))}); len(p) != 0 {
		t.Errorf("control: two refusals that differ only by their request id were called different: %v", p)
	}
	for _, c := range []struct {
		what, want string
		rs         []gtrReply
		names      []string
	}{
		{"a 403 among the 401s", "want 401", []gtrReply{fake(401, "Bearer", env), fake(403, "Bearer", env)}, pair},
		{"a body that differs", "differs from the first refusal's", []gtrReply{fake(401, "Bearer", env), fake(401, "Bearer", envOther)}, pair},
		{"a 200 among them", "want 401", []gtrReply{fake(401, "Bearer", env), fake(200, "Bearer", env)}, pair},
		{"no challenge header", "WWW-Authenticate is", []gtrReply{fake(401, "Bearer", env), fake(401, "", env)}, pair},
		{"an empty set", "matched nothing", nil, nil},
	} {
		if got := strings.Join(gtrIdenticalRefusals(c.names, c.rs), "\n"); !strings.Contains(got, c.want) {
			t.Errorf("control: for %s the comparison said %q, want it to say %q", c.what, got, c.want)
		}
	}

	// ---- isolated: B on A's job is the answer for a job that does not exist, A on it is 200 ----
	var ghost string
	if err := e.admin.QueryRow(e.ctx, "SELECT gen_random_uuid()::text").Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	cross := a.get("/v1/jobs/"+jobsA[0], bearer(goodB))
	absent := a.get("/v1/jobs/"+ghost, bearer(goodB))
	if cross.Status != http.StatusNotFound {
		t.Fatalf("B's token on A's job answered HTTP %d, want 404", cross.Status)
	}
	if bytes.Contains(cross.Body, []byte(jobsA[0])) || bytes.Contains(cross.Body, []byte(idA)) {
		t.Errorf("B's 404 names A's job or A: %s", gtrCut(string(cross.Body)))
	}
	if gtrNormal(cross) != gtrNormal(absent) {
		t.Errorf("B's answer for A's job (%s) differs from its answer for a job that does not exist (%s): the first would tell B the job is there", gtrCut(gtrNormal(cross)), gtrCut(gtrNormal(absent)))
	}
	own := a.get("/v1/jobs/"+jobsA[0], bearer(good)) // the control: the owner gets it
	var got struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if own.Status != http.StatusOK || json.Unmarshal(own.Body, &got) != nil || got.Job.ID != jobsA[0] {
		t.Errorf("control: A's own token on A's job answered HTTP %d (job %q), want 200 and the job %s", own.Status, got.Job.ID, jobsA[0])
	}
	if mine := a.get("/v1/jobs/"+jobsB[0], bearer(goodB)); mine.Status != http.StatusOK {
		t.Errorf("control: B's own token on B's job answered HTTP %d, want 200", mine.Status)
	}
	t.Logf("NOTE api: the real GoTrue tokens of two users listed their own jobs through the platform's HTTP API (A %d jobs, B %d, neither id in the other's answer); %d wrong requests (a flipped signature character, another sub under A's signature, an expired token, another audience, an empty bearer, no header, a real login with no platform user) were each ONE identical 401 with WWW-Authenticate Bearer, none listing a job; B's token on A's job was 404, byte for byte the answer for a job that does not exist, while A's own was 200",
		len(jobsA), len(jobsB), len(cases))
}

// gtrJobIDs2 is the ids a list answer holds, or none when the answer is not one (a refusal): for the check that a refusal names no job.
func gtrJobIDs2(r gtrReply) []string {
	if r.Status != http.StatusOK {
		return nil
	}
	var body struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	if json.Unmarshal(r.Body, &body) != nil {
		return nil
	}
	var ids []string
	for _, j := range body.Jobs {
		ids = append(ids, j.ID)
	}
	return ids
}
