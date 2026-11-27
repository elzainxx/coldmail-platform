//go:build harness

package harness

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/unsubscribe/endpoint"
)

// Phase 5 S9, the live proofs of templates and unsubscribe against module 3 (the scenarios and their comparisons are in jobs_live_templates_helpers_test.go; module 2's rig is jobs_live_templates_m2_test.go). The
// two live tests need the full harness stack (they FAIL, never skip, when it is not there: run-db-tests.sh names them in its skip list because the database-only run has no module); the two comparison controls need
// nothing and run everywhere, and they pin every hand-written expectation of the live scenarios against the platform's own renderer, so that a wrong expectation is red in minutes and not after a half-hour run.

// jltM3Rig is the rig of module 3: its REAL Control, its three workers and the controllable sink, tenants with three lanes and no pause between two sends, and an entitlement delivered by the platform service.
func jltM3Rig(t *testing.T) *jltRig {
	t.Helper()
	ctx := jlCtx(t, 10*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	s, tr := newJlStackWith(t, ctx, jlOpts{Cfg: func(sec config.Secret) config.Config { return jlConfig(t, sec) }})
	s.jltUseTemplates(tr)
	tenants := map[string]*m3aTenant{}
	return &jltRig{
		T: t, S: s, Module: jobs.ModuleSendBYO, Label: "module 3", Run: "jlt" + randomHex(t, 4) + "-",
		NewUser: func(name string) string {
			tn := s.m3User(e, name, true)
			tenants[tn.UserID] = tn
			return tn.UserID
		},
		Ctl:    func(method, path string) []byte { return e.ctl(t, method, path, nil, http.StatusOK) },
		Attach: func(string, string) {},
		Diag:   func(user string) string { return e.diagnostics(t, tenants[user]) },
	}
}

// TestJobsLive_Templates_M3_RenderedMessagesAtTheSink (Phase 5 S9, exit criteria 1 to 3, the REAL module 3, its workers and the controllable sink): ONE templated send job of seventeen rows, read at the sink.
// The subject is `{Hello|Hi|Hey} {{first_name|friend}}`, the HTML has four spintax groups of four or five options and three merge fields (one, company, with no fallback), the text template is blank, and the
// rows carry their merge data as CSV columns. Fifteen rows can be sent (twelve ordinary, one of them with no first name and no plan, and three hostile) and two lack the company.
//
//	byte for byte   every message at the sink equals public.job_items, an independent render, and the second platform instance's Preview samples; the second instance's replay of the engine's retry path writes nothing
//	                and leaves public.job_items as it was. Controls: a tampered message is not equal; the recipients do not all render alike; the same recipient rendered twice is equal; the seed follows the job id
//	missing field   the two rows with no company are never at the sink (no message, no RCPT), are the platform's own free missing_field results (P-09) and cost nothing; the rows that have it are sent
//	injection       a value that looks like a field or a group, a script tag, an attribute breaker and a line break followed by a Bcc header is literal at the sink: escaped in the HTML, raw in the text, one subject
//	                line, no Bcc, one recipient. Control: an ordinary value is plain text
func TestJobsLive_Templates_M3_RenderedMessagesAtTheSink(t *testing.T) {
	jltRunTemplates(jltM3Rig(t))
}

// TestJobsLive_Unsubscribe_M3_OneClickLandsOnSuppressionAndTheNextJobExcludesIt (Phase 5 S9, exit criterion 4, the REAL module 3): the template of this job places {{unsubscribe_url}} itself, so there is no footer. See
// jltRunUnsubscribe for the whole statement: the List-Unsubscribe pair read at the sink, the public handler (GET changes nothing, one-click and form POSTs suppress, forged tokens are one 404 and write nothing, a
// repeat writes no second row, another user is untouched) and a SECOND job of the same user over the same list that leaves A and B out at confirm and sends the rest.
func TestJobsLive_Unsubscribe_M3_OneClickLandsOnSuppressionAndTheNextJobExcludesIt(t *testing.T) {
	jltRunUnsubscribe(jltM3Rig(t), true)
}

// ---- the controls: no stack ---------------------------------------------------------------------------------------------------------------------------------------------------------------

// jltFrame builds a message the way module 3's and module 2's workers do (assemble.js serializeAlternative): the header lines, a blank line, the text part first with CRLF line ends, the HTML part last as it is
// (8bit), CRLF between the lines.
func jltFrame(subject, htmlBody, text string, extra ...string) []byte {
	const boundary = "----=_Alt_synthetic"
	head := []string{`From: "Harness" <sender@example.test>`, "To: rcpt@example.test", "Subject: " + subject, "Message-ID: <synthetic@example.test>", "Date: Mon, 02 Jan 2006 15:04:05 +0000"}
	head = append(head, extra...)
	head = append(head, "MIME-Version: 1.0", `Content-Type: multipart/alternative; boundary="`+boundary+`"`)
	var sb strings.Builder
	sb.WriteString(strings.Join(head, "\r\n") + "\r\n\r\n")
	for _, p := range []struct{ typ, body string }{{"text/plain", strings.ReplaceAll(jltLF(text), "\n", "\r\n")}, {"text/html", htmlBody}} {
		sb.WriteString("--" + boundary + "\r\nContent-Type: " + p.typ + "; charset=\"utf-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + p.body + "\r\n")
	}
	sb.WriteString("--" + boundary + "--")
	return []byte(sb.String())
}

func jltMustParse(t *testing.T, raw []byte) jltWire {
	t.Helper()
	w, err := jltParseWire(raw)
	if err != nil {
		t.Fatalf("a synthetic message does not parse: %v\n%.300q", err, raw)
	}
	return w
}

// TestJobsLive_ComparisonCanFail_Templates needs no stack. It renders the live scenario's fixture with the platform's own renderer, frames every message as a worker does and parses it back, and requires: the
// scenario's hand-written expectations hold of the platform's real output (the two rows that lack the company are the only ones missing, the greeting and the fallbacks, the escapes, the one-line subject, the
// footer); the comparison of the sink with the render is equal for every row; the recipients render differently under every one of fifty job ids (the thresholds of the live controls are never near the edge); and
// each of the live checks, run on a copy tampered one way at a time, reports that way.
func TestJobsLive_ComparisonCanFail_Templates(t *testing.T) {
	fx := jltTemplateFixture("jltctl-tpl-")
	content := jltTemplateContent()
	var rows []jlRow
	fields := map[string]map[string]string{}
	for _, a := range fx.Order {
		f := fx.By[a]
		st := "pending"
		if f.Kind == jltMissing {
			st = "missing_field"
		}
		rows = append(rows, jlRow{ID: jlID(f.Ordinal), Ordinal: f.Ordinal, Norm: a, State: st})
		fields[a] = f.Fields
	}
	if len(fx.Rows) != 17 || len(fx.Sendable) != 15 || len(fx.Missing) != 2 {
		t.Fatalf("the fixture has %d rows, %d sendable and %d missing, want 17, 15 and 2", len(fx.Rows), len(fx.Sendable), len(fx.Missing))
	}

	// the two rows that lack the company are the only rows that lack anything, and only the company
	prep, err := jobs.TemplateRenderer{}.Prepare(jlID(9000), content)
	if err != nil {
		t.Fatalf("the platform's renderer refuses the live campaign: %v", err)
	}
	for _, r := range rows {
		er := jobs.EngineRow{ID: r.ID, Ordinal: r.Ordinal, EmailNorm: r.Norm, Fields: fields[r.Norm], UnsubscribeURL: jtLink(r.ID)}
		miss := prep.Missing(er)
		if want := fx.By[r.Norm].Kind == jltMissing; (len(miss) != 0) != want || (want && !slices.Equal(miss, []string{"company"})) {
			t.Errorf("row %d: the platform says it lacks %v, the fixture says missing %v", r.Ordinal, miss, want)
		}
	}

	rendered := jltRenderAll(t, jlID(9000), content, rows, fields)
	if len(rendered) != 15 {
		t.Fatalf("the renderer made %d messages, want 15", len(rendered))
	}
	wires := map[string]jltWire{}
	for _, a := range fx.Sendable {
		f := fx.By[a]
		id := jlID(f.Ordinal)
		m := rendered[id]
		raw := jltFrame(m.Subject, m.HTML, m.Text, "List-Unsubscribe: <"+jtLink(id)+">", "List-Unsubscribe-Post: List-Unsubscribe=One-Click")
		w := jltMustParse(t, raw)
		wires[a] = w
		for _, p := range [][]string{jltDiff("the framed message", w.msg(), m), jltSafetyProblems(w, []string{a}), jltRowProblems(f, w), jltLinkProblems(w, id)} {
			if len(p) != 0 {
				t.Errorf("row %d: the consistent message is reported wrong: %v", f.Ordinal, p)
			}
		}
		if _, err := jltSpin(w.HTML); err != nil {
			t.Errorf("row %d: %v", f.Ordinal, err)
		}
		if len(w.Parts) != 2 || w.Parts[0] != "text/plain" || w.Parts[1] != "text/html" {
			t.Errorf("row %d: the parts are %v", f.Ordinal, w.Parts)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// the thresholds of the live controls are far from the edge: under fifty other job ids the same fifteen rows draw at least 5 distinct HTML tuples and 2 subject greetings, and differ from job to job
	for k := 0; k < 50; k++ {
		job := jlID(10000 + k)
		r := jltRenderAll(t, job, content, rows, fields)
		spins, greet, differ := map[string]bool{}, map[string]bool{}, 0
		for _, a := range fx.Sendable {
			id := jlID(fx.By[a].Ordinal)
			sp, err := jltSpin(r[id].HTML)
			if err != nil {
				t.Fatalf("job %d: %v", k, err)
			}
			spins[sp], greet[strings.SplitN(r[id].Subject, " ", 2)[0]] = true, true
			if r[id] != rendered[id] {
				differ++
			}
		}
		if len(spins) < 5 || len(greet) < 2 || differ < 12 {
			t.Errorf("job id %d: %d distinct HTML draws, %d greetings, %d of 15 differ from the first job's messages: the live controls' thresholds (5, 2, 7) are too near", k, len(spins), len(greet), differ)
		}
		if again := jltRenderAll(t, job, content, rows, fields); !slices.Equal(jltKeys(again, fx), jltKeys(r, fx)) {
			t.Errorf("job id %d: two renders of the same job differ", k)
		}
	}

	// ---- every live check, on a copy tampered one way at a time ----
	a0 := fx.Sendable[0] // Alice
	m0, w0 := rendered[jlID(fx.By[a0].Ordinal)], wires[a0]
	tam := func(f func(*jltMsg)) jltMsg { c := w0.msg(); f(&c); return c }
	jlWantProblem(t, "one byte of the HTML changed", "HTML part", jltDiff("x", tam(func(m *jltMsg) { m.HTML = strings.Replace(m.HTML, "Sam", "Sal", 1) }), m0))
	jlWantProblem(t, "one byte of the text changed", "text part", jltDiff("x", tam(func(m *jltMsg) { m.Text += "." }), m0))
	jlWantProblem(t, "the subject changed", "subject", jltDiff("x", tam(func(m *jltMsg) { m.Subject += "!" }), m0))
	if p := jltDiff("x", w0.msg(), m0); len(p) != 0 {
		t.Errorf("control: the untampered message differs from itself: %v", p)
	}
	framed := func(subject string, extra ...string) jltWire {
		return jltMustParse(t, jltFrame(subject, m0.HTML, m0.Text, extra...))
	}
	jlWantProblem(t, "a second Subject header", "Subject header field", jltSafetyProblems(framed(m0.Subject, "Subject: injected"), []string{a0}))
	jlWantProblem(t, "a Bcc header", "Bcc header field", jltSafetyProblems(framed(m0.Subject, "Bcc: attacker@example.test"), []string{a0}))
	jlWantProblem(t, "a Cc header", "Cc header field", jltSafetyProblems(framed(m0.Subject, "Cc: attacker@example.test"), []string{a0}))
	jlWantProblem(t, "a subject with a line break and a Bcc line (the injection itself)", "Bcc header field", jltSafetyProblems(framed("Hello\r\nBcc: attacker@example.test"), []string{a0}))
	jlWantProblem(t, "a subject folded onto a second line", "physical line", jltSafetyProblems(framed("Hello\r\n there"), []string{a0}))
	jlWantProblem(t, "two recipients in the envelope", "recipient(s)", jltSafetyProblems(w0, []string{a0, "other@example.test"}))
	for tag, name := range map[string]string{"<script>alert(1)</script>": "<script", `<img src=x onerror=1>`: "<img", "<iframe src=x></iframe>": "<iframe"} {
		w := jltMustParse(t, jltFrame(m0.Subject, m0.HTML+tag, m0.Text))
		jlWantProblem(t, "a live "+name+" tag in the HTML", name, jltSafetyProblems(w, []string{a0}))
	}
	// the expectations of one ordinary row and of the three hostile rows
	jlWantProblem(t, "the company missing from the HTML", "HTML part does not hold", jltRowProblems(fx.By[a0], jltMustParse(t, jltFrame(m0.Subject, strings.Replace(m0.HTML, "Acme-01", "Acme-99", 1), m0.Text))))
	jlWantProblem(t, "the greeting is not the name", "subject is", jltRowProblems(fx.By[a0], jltMustParse(t, jltFrame("Hello Mallory", m0.HTML, m0.Text))))
	jlWantProblem(t, "a template tag left in the message", "brace", jltRowProblems(fx.By[a0], jltMustParse(t, jltFrame(m0.Subject, m0.HTML+"{{company}}", m0.Text))))
	jlWantProblem(t, "no footer in the HTML", "footer", jltRowProblems(fx.By[a0], jltMustParse(t, jltFrame(m0.Subject, strings.ReplaceAll(m0.HTML, "To stop receiving these messages", "Bye"), m0.Text))))
	// the hostile rows (the 9th, the 11th and the 17th of the list): the consistent message holds the values literally, and each way of NOT keeping a value literal is reported for its own reason
	hostile := func(ordinal int) (string, *jltRowFx, jltMsg) {
		a := fx.Order[ordinal-1]
		f := fx.By[a]
		if f.Kind != jltInject {
			t.Fatalf("row %d of the fixture is not a hostile row", ordinal)
		}
		if p := append(jltInjectionProblems(*f.Injection, wires[a]), jltSafetyProblems(wires[a], []string{a})...); len(p) != 0 {
			t.Errorf("the consistent hostile message of row %d is reported wrong: %v", ordinal, p)
		}
		return a, f, rendered[jlID(ordinal)]
	}
	check := func(ordinal int, name, mention string, subject, htmlBody, text string) {
		t.Helper()
		a, f, _ := hostile(ordinal)
		w, err := jltParseWire(jltFrame(subject, htmlBody, text))
		if err != nil {
			t.Errorf("%s: the tampered message does not even parse (%v): the check was never run on it", name, err)
			return
		}
		jlWantProblem(t, name, mention, append(jltInjectionProblems(*f.Injection, w), jltSafetyProblems(w, []string{a})...))
	}
	_, _, m9 := hostile(9)
	check(9, "a value read as a group", "HTML part does not hold", m9.Subject, strings.Replace(m9.HTML, "{a|b}", "a", 1), m9.Text)
	check(9, "a value read as a field", "subject is", strings.Replace(m9.Subject, "{{x}}", "", 1), m9.HTML, m9.Text)
	check(9, "a literal value dropped from the text", "text part does not hold", m9.Subject, m9.HTML, strings.Replace(m9.Text, "{a|b}", "", 1))
	_, _, m11 := hostile(11)
	check(11, "a value not escaped in the HTML", "<script", m11.Subject, strings.NewReplacer("&lt;", "<", "&gt;", ">").Replace(m11.HTML), m11.Text)
	check(11, "a value escaped in the text", "text part does not hold", m11.Subject, m11.HTML, strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(m11.Text))
	_, _, m17 := hostile(17)
	check(17, "a line break and a Bcc line left in the subject", "Bcc header field", "Hello Eve\r\nBcc: attacker@example.test", m17.HTML, m17.Text)
	check(17, "the subject's control characters not turned into spaces", "subject is", "Hello Eve Bcc: attacker@example.test", m17.HTML, m17.Text)
	if _, err := jltSpin("<p>nothing of the template</p>"); err == nil {
		t.Error("jltSpin accepted an HTML part that holds none of the template's groups")
	}
	if _, err := jltParseWire([]byte("not a message")); err == nil {
		t.Error("jltParseWire accepted bytes with no header block")
	}
	if _, err := jltParseWire([]byte("From: a@example.test\r\nthis line is no header\r\n\r\nbody")); err == nil {
		t.Error("jltParseWire accepted a header block with a line that is not a header field")
	}
	// the single-part and quoted-printable paths of the parser
	single := jltMustParse(t, []byte("Subject: =?UTF-8?B?SGkgdGhlcmU=?=\r\nContent-Type: text/html; charset=\"utf-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>caf=C3=A9 =3D x</p>"))
	if single.Subject != "Hi there" || single.HTML != "<p>café = x</p>" || len(single.Parts) != 1 || single.Text != "" {
		t.Errorf("a single-part quoted-printable message with an RFC 2047 subject reads as subject %q, HTML %q, parts %v", single.Subject, single.HTML, single.Parts)
	}
}

// jltKeys is every sendable row's message of a render, in the fixture's order, for comparing two renders.
func jltKeys(r map[string]jltMsg, fx jltFixture) []jltMsg {
	var out []jltMsg
	for _, a := range fx.Sendable {
		out = append(out, r[jlID(fx.By[a].Ordinal)])
	}
	return out
}

// jltFakeItems and jltFakeSupp stand in for the store and the suppression writer of the handler, so that the helpers of the live unsubscribe scenario (jltCall, the page constants, the forged tokens) are shown to
// work without a database.
type jltFakeItems struct{ id, owner, email string }

func (f jltFakeItems) ItemOwner(_ context.Context, itemID string) (string, string, error) {
	if itemID == f.id {
		return f.owner, f.email, nil
	}
	return "", "", jobs.ErrNotFound
}

type jltFakeSupp struct {
	adds int
	seen map[string]bool
}

func (f *jltFakeSupp) Add(_ context.Context, user, email string) (bool, error) {
	f.adds++
	k := user + "|" + email
	created := !f.seen[k]
	f.seen[k] = true
	return created, nil
}

// TestJobsLive_ComparisonCanFail_Unsubscribe needs no stack: the live unsubscribe scenario's checks are shown to report a missing, a doubled, a wrong and a foreign link or header, the forged tokens it sends are
// shown to be refused by the signer, and its handler calls (a GET, the one-click POST, the form POST, a forged token, a token of another secret, a repeat) are shown to behave as the scenario says against a handler
// whose store and suppression writer are fakes.
func TestJobsLive_ComparisonCanFail_Unsubscribe(t *testing.T) {
	id, other := jlID(1), jlID(2)
	link, otherLink := jtLink(id), jtLink(other)
	htmlOf := func(l string) string {
		return "<html><body>\n<p>Hello</p>\n<p>To stop receiving these messages, <a href=\"" + l + "\">unsubscribe</a>.</p>\n</body></html>"
	}
	textOf := func(l string) string { return "Hello\n\nTo stop receiving these messages, unsubscribe (" + l + ")." }
	pair := []string{"List-Unsubscribe: <" + link + ">", "List-Unsubscribe-Post: List-Unsubscribe=One-Click"}
	frame := func(html, text string, extra ...string) jltWire {
		return jltMustParse(t, jltFrame("Hello", html, text, extra...))
	}
	good := frame(htmlOf(link), textOf(link), pair...)
	if p := jltLinkProblems(good, id); len(p) != 0 {
		t.Fatalf("control: the consistent message is reported wrong: %v", p)
	}
	if tok := jltTokenOf(good); len(tok) != 54 || strings.Contains(tok, "/") {
		t.Fatalf("jltTokenOf reads %q from a message whose header holds %s", tok, link)
	}
	jlWantProblem(t, "no List-Unsubscribe", "0 List-Unsubscribe header", jltLinkProblems(frame(htmlOf(link), textOf(link), pair[1]), id))
	jlWantProblem(t, "two List-Unsubscribe", "2 List-Unsubscribe header", jltLinkProblems(frame(htmlOf(link), textOf(link), pair[0], pair[0], pair[1]), id))
	jlWantProblem(t, "no List-Unsubscribe-Post", "0 List-Unsubscribe-Post", jltLinkProblems(frame(htmlOf(link), textOf(link), pair[0]), id))
	jlWantProblem(t, "two List-Unsubscribe-Post", "2 List-Unsubscribe-Post", jltLinkProblems(frame(htmlOf(link), textOf(link), pair[0], pair[1], pair[1]), id))
	jlWantProblem(t, "a Post value that is not one-click", "List-Unsubscribe-Post is", jltLinkProblems(frame(htmlOf(link), textOf(link), pair[0], "List-Unsubscribe-Post: nope"), id))
	jlWantProblem(t, "a URL with no angle brackets", "angle brackets", jltLinkProblems(frame(htmlOf(link), textOf(link), "List-Unsubscribe: "+link, pair[1]), id))
	jlWantProblem(t, "a header URL that is not the HTML link", "is not the List-Unsubscribe URL", jltLinkProblems(frame(htmlOf(otherLink), textOf(link), pair...), id))
	jlWantProblem(t, "two links in the HTML", "2 unsubscribe link", jltLinkProblems(frame(htmlOf(link)+htmlOf(link), textOf(link), pair...), id))
	jlWantProblem(t, "no link in the HTML", "0 unsubscribe link", jltLinkProblems(frame("<p>Hello</p>", textOf(link), pair...), id))
	jlWantProblem(t, "no link in the text", "text part does not hold", jltLinkProblems(frame(htmlOf(link), "Hello", pair...), id))
	jlWantProblem(t, "a link that names another recipient's row", "names item", jltLinkProblems(good, other))
	otherSigner, err := jltOtherSigner()
	if err != nil {
		t.Fatal(err)
	}
	foreignLink, err := otherSigner.LinkMaker(harnessPublicBase)(id)
	if err != nil || foreignLink == link {
		t.Fatalf("a link made under another secret: %v (equal to the harness's: %v)", err, foreignLink == link)
	}
	pairOf := func(l string) []string {
		return []string{"List-Unsubscribe: <" + l + ">", "List-Unsubscribe-Post: List-Unsubscribe=One-Click"}
	}
	jlWantProblem(t, "a link signed with another secret", "names item", jltLinkProblems(frame(htmlOf(foreignLink), textOf(foreignLink), pairOf(foreignLink)...), id))

	// the forged tokens are refused by the signer, and the real one is not
	tok := jltTokenOf(good)
	if got, ok := harnessUnsubSigner.Verify(tok); !ok || got != id {
		t.Fatalf("control: the signer refuses the real token (%v) or reads item %q", ok, got)
	}
	for _, i := range []int{0, 20, 53} {
		forged := jltFlip(tok, i)
		diff := 0
		for k := range tok {
			if tok[k] != forged[k] {
				diff++
			}
		}
		if _, ok := harnessUnsubSigner.Verify(forged); diff != 1 || ok {
			t.Errorf("the token with character %d changed differs in %d place(s) and is accepted: %v", i, diff, ok)
		}
	}
	foreignTok := strings.TrimPrefix(foreignLink, harnessPublicBase+"/u/")
	if _, ok := harnessUnsubSigner.Verify(foreignTok); ok {
		t.Errorf("the signer accepts a token made under another secret")
	}
	if got, ok := otherSigner.Verify(foreignTok); !ok || got != id {
		t.Errorf("control: the other signer refuses its own token")
	}

	// the handler, over fakes: a GET adds nothing, the POSTs add, every wrong token is the one 404 and adds nothing, a repeat is the same page
	supp := &jltFakeSupp{seen: map[string]bool{}}
	h, err := endpoint.New(endpoint.Config{Signer: harnessUnsubSigner, Items: jltFakeItems{id: id, owner: jlID(77), email: "a@example.test"}, Suppressions: supp, RatePerMin: endpoint.MaxRatePerMin})
	if err != nil {
		t.Fatal(err)
	}
	if rec := jltCall(h, http.MethodGet, tok, "", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), jltPageConfirm) || supp.adds != 0 {
		t.Fatalf("a GET answered %d (confirm page %v) and the writer was called %d time(s), want 200, the page and 0", rec.Code, strings.Contains(rec.Body.String(), jltPageConfirm), supp.adds)
	}
	rec := jltCall(h, http.MethodPost, tok, jltFormType, jltOneClickBody)
	thanks := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(thanks, jltPageThanks) || supp.adds != 1 {
		t.Fatalf("the one-click POST answered %d (thank-you page %v) and the writer was called %d time(s), want 200, the page and 1", rec.Code, strings.Contains(thanks, jltPageThanks), supp.adds)
	}
	garbage := jltCall(h, http.MethodPost, "not-a-token", jltFormType, jltOneClickBody)
	if garbage.Code != http.StatusNotFound || !strings.Contains(garbage.Body.String(), jltPageNotValid) {
		t.Fatalf("a garbage token answered %d, want the 404 page", garbage.Code)
	}
	for name, forged := range map[string]string{"one character changed": jltFlip(tok, 20), "the last character changed": jltFlip(tok, 53), "another secret": foreignTok} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			if rec := jltCall(h, method, forged, jltFormType, jltOneClickBody); rec.Code != http.StatusNotFound || rec.Body.String() != garbage.Body.String() || supp.adds != 1 {
				t.Errorf("%s (%s): answered %d with the 404 page %v, the writer was called %d time(s), want the 404 page of every wrong token and no call", name, method, rec.Code, rec.Body.String() == garbage.Body.String(), supp.adds)
			}
		}
	}
	if rec := jltCall(h, http.MethodPost, tok, jltFormType, ""); rec.Code != http.StatusOK || rec.Body.String() != thanks || supp.adds != 2 || len(supp.seen) != 1 {
		t.Errorf("a repeated POST (a form click) answered %d, the same page %v, the writer was called %d time(s) for %d address(es)", rec.Code, rec.Body.String() == thanks, supp.adds, len(supp.seen))
	}
}
