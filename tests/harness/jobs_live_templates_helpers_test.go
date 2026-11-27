//go:build harness

package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/suppress"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
	"github.com/coldmail-platform/platform/internal/unsubscribe/endpoint"
)

// Phase 5 S9, the LIVE proofs of templates and unsubscribe: a templated send job is driven by the job engine of THIS process through module 2's or module 3's REAL Control and worker, and every claim is read AT THE
// SINK, from the raw bytes the controllable sink accepted (GET /api/message?seq=N), never from the database. The scenario is written ONCE here (jltRunTemplates, jltRunUnsubscribe) over a jltRig and the two senders supply
// the rig: jobs_live_templates_test.go (module 3, tag harness) and jobs_live_templates_m2_test.go (module 2, tag m2worker). Every name starts with `jlt` so that it cannot clash with another file.
//
// WHAT IS REAL. The platform's job store with the TEMPLATE RENDERER and the unsubscribe link maker (jobs.WithPreparer(jobs.TemplateRenderer{}), as internal/app wires the service: newJlStackWith builds its store
// without one, so jltUseTemplates swaps the stack's store and engine), the engine, the real module, its worker and the sink; the unsubscribe handler is the production handler (internal/unsubscribe/endpoint) over the
// stack's REAL store, the REAL internal/suppress and the harness signer, called in process through httptest. WHAT IS NOT: the engine runs in the test process, not in the platform service, and the handler is called
// directly and not through the service's mux and access log (those are proved by internal/httpapi and internal/unsubscribe/endpoint's own tests).
//
// THE SECOND PLATFORM INSTANCE is a second jobs.Store over the same database with its own TemplateRenderer (jltStore makes one per call): it previews the job (its sample messages are the third witness of the bytes),
// replays the engine's retry path (EnsureJobItems, EnsureModuleItems: write nothing twice) and an independent jobs.TemplateRenderer{}.Prepare renders every row of the job from its row id, its merge data and its link.
// Only exported APIs are used.
//
// WHAT "BYTE FOR BYTE" TOLERATES, and nothing else: the bytes the sink holds are the message AFTER module 2 or module 3 framed it for SMTP, so a part is compared after its Content-Transfer-Encoding is undone and
// after CRLF is read as LF (the module writes the text part with CRLF; the SMTP data is CRLF-separated). Every other byte of the subject (RFC 2047-decoded), the HTML part and the text part must be equal.

// ---- the rig: what differs between the two senders ----------------------------------------------------------------------------------------------------------------------------------------

// jltRig is the sender-specific side of a scenario.
type jltRig struct {
	T       *testing.T
	S       *jlStack
	Module  string
	Label   string // "module 2" or "module 3", for messages
	Metered bool   // module 2: credits are reserved and charged
	Grant   int64  // credits a new user gets (metered)
	Run     string // the run's id: the start of every recipient's local part, so that the sink view is this test's
	// NewUser makes a tenant able to send (an account at the module; credits or an entitlement) and tells the stack's engine it is this test's. It returns the user id.
	NewUser func(name string) string
	// Ctl is a call to the controllable sink's control API that must answer 200; it returns the body.
	Ctl func(method, path string) []byte
	// Attach tells the module-side cleanup about the job's campaign (module 2's cancel-on-failure cleanup knows a tenant's campaign by its id; module 3's needs nothing).
	Attach func(user, job string)
	// Diag is what a failed wait appends.
	Diag func(user string) string
}

// Prefix is the start of every recipient address of one scenario.
func (g *jltRig) Prefix(tag string) string { return g.Run + tag + "-" }

// ---- the stack: a store that renders templates ---------------------------------------------------------------------------------------------------------------------------------------------

// jltStore is a job store as the SERVICE builds one (internal/app/auth.go: the template renderer is installed with jobs.WithPreparer), over the stack's own pool and money store, with the harness link maker and, for
// a stack with module 3, its pre-check. Every call makes a NEW store with its own renderer: the stack's engine runs on one and "a second platform instance" is another.
func (s *jlStack) jltStore() *jobs.Store {
	s.t.Helper()
	log := slog.New(slog.NewTextHandler(s.ring, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts := []jobs.Option{jobs.WithMaxRows(jobs.DefaultMaxRows), jobs.WithMaxStartDelay(jobs.DefaultMaxStartDelay), jobs.WithLogger(log), harnessUnsubscribeOption(), jobs.WithPreparer(jobs.TemplateRenderer{})}
	if s.m3ad != nil {
		opts = append(opts, jobs.WithPreChecks(jlM3PreChecks{ad: s.m3ad}))
	}
	st, err := jobs.New(s.pool, s.mny, opts...)
	if err != nil {
		s.t.Fatalf("jobs.New with the template renderer: %v", err)
	}
	if !st.TemplatesEnabled() || !st.UnsubscribeConfigured() {
		s.t.Fatalf("the store built for the template proofs renders templates %v and makes unsubscribe links %v, want both", st.TemplatesEnabled(), st.UnsubscribeConfigured())
	}
	return st
}

// jltUseTemplates makes the stack's store the template store and gives the stack an engine over it (the engine of newJlStackWith was built over the store without a renderer and is never ticked again).
func (s *jlStack) jltUseTemplates(tr *jlTrace) {
	s.t.Helper()
	s.store = s.jltStore()
	s.jlpcUseEngine(tr, jlpcEngineConfig(500, 15*time.Minute))
}

// jltEndpoint is the production unsubscribe handler over the stack's real store and the real suppression writer, with the harness signer and a bucket large enough that the rate limit never decides a proof.
func (s *jlStack) jltEndpoint() *endpoint.Handler {
	s.t.Helper()
	h, err := endpoint.New(endpoint.Config{Signer: harnessUnsubSigner, Items: s.store, Suppressions: endpoint.SuppressionsOf(s.pool), RatePerMin: endpoint.MaxRatePerMin})
	if err != nil {
		s.t.Fatalf("endpoint.New: %v", err)
	}
	return h
}

// jltItem is one row of public.job_items as module 2 and module 3 read it.
type jltItem struct{ Subject, Body, Text, Link string }

// jltItems is every public.job_items row of the job's campaign, by item id.
func (s *jlStack) jltItems(job string) map[string]jltItem {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT id::text, rendered_subject, rendered_body, COALESCE(rendered_text, ''), COALESCE(unsubscribe_url, '') FROM public.job_items WHERE campaign_id = $1::uuid ORDER BY id`, job)
	if err != nil {
		s.t.Fatalf("read the job items of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	out := map[string]jltItem{}
	for q.Next() {
		var id string
		var it jltItem
		if err := q.Scan(&id, &it.Subject, &it.Body, &it.Text, &it.Link); err != nil {
			s.t.Fatal(err)
		}
		out[id] = it
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// ---- the message as the sink holds it ---------------------------------------------------------------------------------------------------------------------------------------------------

// jltWire is one message of the sink as a recipient's mail program reads it.
type jltWire struct {
	HeaderLines  []string            // the lines of the top-level header block, continuation lines included
	Names        map[string]int      // lower-case header name -> how many header fields carry it
	Values       map[string][]string // lower-case header name -> its values (unfolded, trimmed), in order
	Subject      string              // the first Subject header, RFC 2047-decoded ("" when there is none)
	SubjectLines int                 // the physical lines of the first Subject header: its own and every continuation
	Parts        []string            // the media types of the parts, in order (one entry for a single-part message)
	HTML, Text   string              // the text/html and text/plain parts, decoded, LF line ends ("" when the message has none)
}

func jltLF(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// jltParseWire reads the raw bytes of an accepted message. It FAILS on what no message made by module 2 or module 3 can be: no header block, a header line without a name.
func jltParseWire(raw []byte) (jltWire, error) {
	w := jltWire{Names: map[string]int{}, Values: map[string][]string{}}
	end, sep := bytes.Index(raw, []byte("\r\n\r\n")), 4
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 && (end < 0 || i < end) {
		end, sep = i, 2
	}
	if end < 0 {
		return w, errors.New("no blank line ends the header block")
	}
	cur := ""
	for n, line := range strings.Split(strings.ReplaceAll(string(raw[:end]), "\r\n", "\n"), "\n") {
		w.HeaderLines = append(w.HeaderLines, line)
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if cur == "" {
				return w, fmt.Errorf("header line %d is a continuation with no header before it", n+1)
			}
			vs := w.Values[cur]
			vs[len(vs)-1] += " " + strings.TrimSpace(line)
			if cur == "subject" && len(vs) == 1 {
				w.SubjectLines++
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return w, fmt.Errorf("header line %d (%.40q) is not a header field", n+1, line)
		}
		cur = name
		w.Names[name]++
		w.Values[name] = append(w.Values[name], strings.TrimSpace(value))
		if name == "subject" && w.Names[name] == 1 {
			w.SubjectLines = 1
		}
	}
	if v := w.Values["subject"]; len(v) > 0 {
		w.Subject = v[0]
		if d, err := new(mime.WordDecoder).DecodeHeader(w.Subject); err == nil {
			w.Subject = d
		}
	}
	ctype := "text/plain"
	if v := w.Values["content-type"]; len(v) > 0 {
		ctype = v[0]
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return w, fmt.Errorf("the Content-Type %.60q does not parse: %v", ctype, err)
	}
	body := raw[end+sep:]
	if !strings.HasPrefix(mt, "multipart/") {
		cte := ""
		if v := w.Values["content-transfer-encoding"]; len(v) > 0 {
			cte = v[0]
		}
		dec, err := m3cDecode(cte, body)
		if err != nil {
			return w, fmt.Errorf("the single-part body does not decode under %q: %v", cte, err)
		}
		w.Parts = []string{mt}
		switch mt {
		case "text/html":
			w.HTML = jltLF(dec)
		case "text/plain":
			w.Text = jltLF(dec)
		}
		return w, nil
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return w, fmt.Errorf("a part of the multipart body does not parse: %v", err)
		}
		pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		b, err := io.ReadAll(p)
		if err != nil {
			return w, fmt.Errorf("a part of the multipart body does not read: %v", err)
		}
		dec, err := m3cDecode(p.Header.Get("Content-Transfer-Encoding"), b)
		if err != nil {
			return w, fmt.Errorf("a %s part does not decode: %v", pt, err)
		}
		w.Parts = append(w.Parts, pt)
		switch pt {
		case "text/html":
			w.HTML = jltLF(dec)
		case "text/plain":
			w.Text = jltLF(dec)
		}
	}
	return w, nil
}

// jltMsg is a message reduced to what the proofs compare: the subject, the HTML part and the text part, LF line ends.
type jltMsg struct{ Subject, HTML, Text string }

func jltMsgOf(subject, htmlBody, text string) jltMsg {
	return jltMsg{Subject: strings.TrimSpace(subject), HTML: jltLF(htmlBody), Text: jltLF(text)}
}

func (w jltWire) msg() jltMsg { return jltMsg{Subject: w.Subject, HTML: w.HTML, Text: w.Text} }

// jltDiff says how two messages differ, part by part, naming the first differing byte; it is empty when they are equal.
func jltDiff(what string, got, want jltMsg) []string {
	var p []string
	for _, c := range []struct{ part, x, y string }{{"subject", got.Subject, want.Subject}, {"HTML part", got.HTML, want.HTML}, {"text part", got.Text, want.Text}} {
		if c.x != c.y {
			p = append(p, fmt.Sprintf("%s: the %s differs: %s", what, c.part, jltFirstDiff(c.x, c.y)))
		}
	}
	return p
}

func jltFirstDiff(x, y string) string {
	n, i := min(len(x), len(y)), 0
	for i < n && x[i] == y[i] {
		i++
	}
	cut := func(s string) string { return fmt.Sprintf("%.40q", s[min(max(0, i-15), len(s)):]) }
	return fmt.Sprintf("the first difference is at byte %d (lengths %d and %d): %s against %s", i, len(x), len(y), cut(x), cut(y))
}

// jltSafetyProblems is what must hold of EVERY message the platform sends, whatever the merge data held: one Subject on one physical line with no CR or LF, no Bcc and no Cc header, one recipient in the
// envelope, and no live script, image, frame or svg tag in the HTML (the templates of these tests have none, so a tag in the HTML came from a merge value).
func jltSafetyProblems(w jltWire, to []string) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	if w.Names["subject"] != 1 {
		say("the message has %d Subject header field(s), want exactly 1", w.Names["subject"])
	}
	if w.SubjectLines != 1 {
		say("the Subject header takes %d physical line(s), want 1 (a subject that holds a line break takes more)", w.SubjectLines)
	}
	for _, h := range []string{"bcc", "cc"} {
		if w.Names[h] != 0 {
			say("the message has %d %s header field(s): a header was injected", w.Names[h], strings.ToUpper(h[:1])+h[1:])
		}
	}
	if strings.ContainsAny(w.Subject, "\r\n") {
		say("the subject holds a CR or LF")
	}
	if len(to) != 1 {
		say("the envelope lists %d recipient(s), want exactly 1", len(to))
	}
	low := strings.ToLower(w.HTML)
	for _, tag := range []string{"<script", "<img", "<iframe", "<svg"} {
		if strings.Contains(low, tag) {
			say("the HTML part holds a live %s tag: no template of these tests writes one, so it came from a merge value", tag)
		}
	}
	return p
}

// ---- the sink: raw messages, counters ---------------------------------------------------------------------------------------------------------------------------------------------------------

type jltListed struct {
	Seq int64    `json:"seq"`
	To  []string `json:"to"`
}

// jltSeen is one accepted message to a recipient of the scenario.
type jltSeen struct {
	Seq int64
	To  []string
	W   jltWire
}

// jltRcpt is what the sink answered at RCPT TO to one address.
type jltRcpt struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Deferred int `json:"deferred"`
}

func (g *jltRig) sinkList() []jltListed {
	g.T.Helper()
	var out []jltListed
	if err := json.Unmarshal(g.Ctl(http.MethodGet, "/api/messages"), &out); err != nil {
		g.T.Fatalf("the sink's message list is not a JSON list of messages: %v", err)
	}
	return out
}

// sinkMaxSeq is the newest message number the sink holds (0 for none): a mark to read what came after.
func (g *jltRig) sinkMaxSeq() int64 {
	g.T.Helper()
	var top int64
	for _, m := range g.sinkList() {
		top = max(top, m.Seq)
	}
	return top
}

// sinkRead reads, parsed from the raw bytes, every message after seq `after` to an address that starts with prefix, by exact lower-cased address (a recipient with two messages has two entries).
func (g *jltRig) sinkRead(prefix string, after int64) map[string][]jltSeen {
	g.T.Helper()
	out := map[string][]jltSeen{}
	for _, m := range g.sinkList() {
		if m.Seq <= after {
			continue
		}
		var mine []string
		for _, a := range m.To {
			if a = strings.ToLower(strings.TrimSpace(a)); strings.HasPrefix(a, prefix) {
				mine = append(mine, a)
			}
		}
		if len(mine) == 0 {
			continue
		}
		raw := g.Ctl(http.MethodGet, fmt.Sprintf("/api/message?seq=%d", m.Seq))
		w, err := jltParseWire(raw)
		if err != nil {
			g.T.Fatalf("message %d of the sink (to %v) does not read as a mail message: %v\n  the first 400 bytes: %.400q", m.Seq, mine, err, raw)
		}
		for _, a := range mine {
			out[a] = append(out[a], jltSeen{Seq: m.Seq, To: m.To, W: w})
		}
	}
	return out
}

// sinkRcpt is what the sink answered at RCPT TO, for the addresses that start with prefix.
func (g *jltRig) sinkRcpt(prefix string) map[string]jltRcpt {
	g.T.Helper()
	var all map[string]jltRcpt
	if err := json.Unmarshal(g.Ctl(http.MethodGet, "/api/rcpt"), &all); err != nil {
		g.T.Fatalf("the sink's /api/rcpt is not a map of counters: %v", err)
	}
	out := map[string]jltRcpt{}
	for a, c := range all {
		if a = strings.ToLower(a); strings.HasPrefix(a, prefix) {
			out[a] = c
		}
	}
	return out
}

// ---- the templates of the proofs, and their rows --------------------------------------------------------------------------------------------------------------------------------------------------

// jltTemplateContent is the campaign of the template proof: the subject of the task, an HTML body with four spintax groups of four or five options and merge fields (one without a fallback: company), and a
// BLANK text template (the text is generated from the HTML). The platform adds the unsubscribe footer itself: the template does not place the link.
func jltTemplateContent() jobs.Content {
	return jobs.Content{
		Subject: "{Hello|Hi|Hey} {{first_name|friend}}",
		HTML: "<html><body>\n" +
			"<p>{Hello|Hi|Hey|Greetings|Dear} {{first_name|friend}},</p>\n" +
			"<p>{I saw|I noticed|I read|I came across|I found} the post from {{company}} and {wanted to reach out|thought I would write|had to say hello|wanted to share an idea}.</p>\n" +
			"<p>Your plan: {{plan|free}}. {Best|Regards|Cheers|Thanks|Warmly}, Sam</p>\n" +
			"</body></html>",
		Text: "",
	}
}

// jltSpinRes read, from a rendered HTML part, the option each of the four groups drew.
var jltSpinRes = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^<p>(Hello|Hi|Hey|Greetings|Dear) `),
	regexp.MustCompile(`(?m)^<p>(I saw|I noticed|I read|I came across|I found) the post from `),
	regexp.MustCompile(`(wanted to reach out|thought I would write|had to say hello|wanted to share an idea)\.</p>`),
	regexp.MustCompile(`\. (Best|Regards|Cheers|Thanks|Warmly), Sam</p>`),
}

// jltSpin is the four draws of a rendered HTML part, joined; an error when a group's text is not there (the part is not the template's).
func jltSpin(htmlPart string) (string, error) {
	var draws []string
	for i, re := range jltSpinRes {
		m := re.FindStringSubmatch(htmlPart)
		if m == nil {
			return "", fmt.Errorf("the HTML part holds no draw of group %d", i+1)
		}
		draws = append(draws, m[1])
	}
	return strings.Join(draws, "|"), nil
}

// jltInjection is a row whose merge values try to be something else, and what the sink must hold for it.
type jltInjection struct {
	Name      string
	Fields    map[string]string
	SubjectRe *regexp.Regexp
	HTMLHas   []string // exact text the HTML part must hold (the escaped value, or the literal one)
	TextHas   []string // exact text the text part must hold (the raw value)
}

var jltInjections = []jltInjection{
	{
		Name:      "a value that looks like a field and a group",
		Fields:    map[string]string{"first_name": "{{x}}", "company": "{a|b}", "plan": `\{y\}`},
		SubjectRe: regexp.MustCompile(`^(Hello|Hi|Hey) \{\{x\}\}$`),
		HTMLHas:   []string{"{{x}},", "the post from {a|b} and ", `Your plan: \{y\}.`},
		TextHas:   []string{"{{x}},", "the post from {a|b} and ", `Your plan: \{y\}.`},
	},
	{
		Name:      "a value with a script tag and one that closes an attribute",
		Fields:    map[string]string{"first_name": "<script>alert(1)</script>", "company": `"><img src=x onerror=1>`, "plan": "pro"},
		SubjectRe: regexp.MustCompile(`^(Hello|Hi|Hey) <script>alert\(1\)</script>$`),
		HTMLHas:   []string{"&lt;script&gt;alert(1)&lt;/script&gt;,", "the post from &quot;&gt;&lt;img src=x onerror=1&gt; and "},
		TextHas:   []string{"<script>alert(1)</script>,", `the post from "><img src=x onerror=1> and `},
	},
	{
		Name:      "a value with a line break and a Bcc header",
		Fields:    map[string]string{"first_name": "Eve\r\nBcc: attacker@example.test", "company": "Acme-INJ", "plan": "pro"},
		SubjectRe: regexp.MustCompile(`^(Hello|Hi|Hey) Eve  Bcc: attacker@example\.test$`),
		HTMLHas:   []string{"Bcc: attacker@example.test,"},
		TextHas:   []string{"Bcc: attacker@example.test,"},
	},
}

// jltInjectionProblems is what the sink must hold for one injection row: the values literal (escaped in the HTML, raw in the text, raw but on one line in the subject).
func jltInjectionProblems(inj jltInjection, w jltWire) []string {
	var p []string
	if !inj.SubjectRe.MatchString(w.Subject) {
		p = append(p, fmt.Sprintf("%s: the subject is %q, want it to match %s", inj.Name, w.Subject, inj.SubjectRe))
	}
	for _, s := range inj.HTMLHas {
		if !strings.Contains(w.HTML, s) {
			p = append(p, fmt.Sprintf("%s: the HTML part does not hold %q", inj.Name, s))
		}
	}
	for _, s := range inj.TextHas {
		if !strings.Contains(w.Text, s) {
			p = append(p, fmt.Sprintf("%s: the text part does not hold %q", inj.Name, s))
		}
	}
	return p
}

type jltKind int

const (
	jltBenign  jltKind = iota // every field present
	jltNoFirst                // no first_name and no plan: the fallbacks are used and the row is sent
	jltInject                 // the merge values are hostile
	jltMissing                // no company (absent, or blank): the row is never sent
)

// jltRowFx is one row of the fixture and what the sink must hold for it.
type jltRowFx struct {
	Addr       string
	Kind       jltKind
	Fields     map[string]string
	Injection  *jltInjection
	SubjectRe  *regexp.Regexp // a benign row's subject
	HTMLHas    []string       // a benign row's HTML
	TextHas    []string       // a benign row's text
	Ordinal    int
	IsSendable bool
}

// jltFixture is the list of the template proof: 17 rows, 15 of them sendable (12 ordinary, 3 hostile) and 2 that lack the one field with no fallback.
type jltFixture struct {
	Rows     []jobs.RowIn
	By       map[string]*jltRowFx
	Order    []string
	Sendable []string
	Missing  []string
}

func jltBenignWant(f map[string]string) (re *regexp.Regexp, htmlHas, textHas []string) {
	name := "friend"
	if v := f["first_name"]; v != "" {
		name = v
	}
	plan := "free"
	if v := f["plan"]; v != "" {
		plan = v
	}
	company := f["company"]
	re = regexp.MustCompile(`^(Hello|Hi|Hey) ` + regexp.QuoteMeta(name) + `$`)
	htmlCompany := strings.ReplaceAll(company, "&", "&amp;") // the one HTML-special character an ordinary value holds
	htmlHas = []string{name + ",", "the post from " + htmlCompany + " and ", "Your plan: " + plan + "."}
	textHas = []string{name + ",", "the post from " + company + " and ", "Your plan: " + plan + "."}
	return re, htmlHas, textHas
}

func jltTemplateFixture(prefix string) jltFixture {
	fx := jltFixture{By: map[string]*jltRowFx{}}
	add := func(kind jltKind, fields map[string]string, inj *jltInjection) {
		r := &jltRowFx{Addr: fmt.Sprintf("%s%02d@example.test", prefix, len(fx.Order)+1), Kind: kind, Fields: fields, Injection: inj, Ordinal: len(fx.Order) + 1}
		r.IsSendable = kind != jltMissing
		if kind == jltBenign || kind == jltNoFirst {
			r.SubjectRe, r.HTMLHas, r.TextHas = jltBenignWant(fields)
		}
		fx.Rows = append(fx.Rows, jobs.RowIn{Email: r.Addr, Fields: fields})
		fx.By[r.Addr] = r
		fx.Order = append(fx.Order, r.Addr)
		if r.IsSendable {
			fx.Sendable = append(fx.Sendable, r.Addr)
		} else {
			fx.Missing = append(fx.Missing, r.Addr)
		}
	}
	m := func(kv ...string) map[string]string {
		out := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	add(jltBenign, m("first_name", "Alice", "company", "Acme-01", "plan", "pro"), nil)
	add(jltBenign, m("first_name", "Bob", "company", "Acme-02"), nil)
	add(jltBenign, m("first_name", "Carol", "company", "Acme-03", "plan", "team"), nil)
	add(jltNoFirst, m("company", "Acme-04"), nil)
	add(jltBenign, m("first_name", "Dave", "company", "Fox & Hound", "plan", "pro"), nil)
	add(jltMissing, m("first_name", "Zed", "plan", "pro"), nil) // no company column at all
	add(jltBenign, m("first_name", "Erin", "company", "Acme-07"), nil)
	add(jltBenign, m("first_name", "Frank", "company", "Acme-08", "plan", "team"), nil)
	add(jltInject, jltInjections[0].Fields, &jltInjections[0])
	add(jltBenign, m("first_name", "Grace", "company", "Acme-10", "plan", "pro"), nil)
	add(jltInject, jltInjections[1].Fields, &jltInjections[1])
	add(jltBenign, m("first_name", "Heidi", "company", "Acme-12"), nil)
	add(jltMissing, m("first_name", "Yan", "company", "   ", "plan", "pro"), nil) // a company of spaces only is missing too
	add(jltBenign, m("first_name", "Ivan", "company", "Acme-14", "plan", "team"), nil)
	add(jltBenign, m("first_name", "Judy", "company", "Acme-15"), nil)
	add(jltBenign, m("first_name", "Mallory", "company", "Acme-16", "plan", "pro"), nil)
	add(jltInject, jltInjections[2].Fields, &jltInjections[2])
	return fx
}

// jltRowProblems is what the sink must hold for one sendable row of the fixture, apart from being equal to the platform's own render: for a hostile row its values literal, for an ordinary row its values as plain text
// (the control of the hostile ones), a row with no first name greeted by the fallback, no template tag left in any part, and, because the template does not place the link, the platform's footer in the HTML and in
// the text.
func jltRowProblems(f *jltRowFx, w jltWire) []string {
	var p []string
	if f.Kind == jltInject {
		p = append(p, jltInjectionProblems(*f.Injection, w)...)
	} else {
		if !f.SubjectRe.MatchString(w.Subject) {
			p = append(p, fmt.Sprintf("the subject is %q, want it to match %s", w.Subject, f.SubjectRe))
		}
		for _, want := range f.HTMLHas {
			if !strings.Contains(w.HTML, want) {
				p = append(p, fmt.Sprintf("the HTML part does not hold %q", want))
			}
		}
		for _, want := range f.TextHas {
			if !strings.Contains(w.Text, want) {
				p = append(p, fmt.Sprintf("the text part does not hold %q", want))
			}
		}
		if strings.ContainsAny(w.Subject+w.HTML+w.Text, "{}") {
			p = append(p, "the message still holds a brace: a template tag was left unrendered")
		}
	}
	if !strings.Contains(w.HTML, "To stop receiving these messages") || !strings.Contains(w.Text, "unsubscribe") {
		p = append(p, "the template did not place the link, so the platform's footer must be in the HTML and in the text")
	}
	return p
}

// jltRenderAll renders every pending row of the job with a fresh, independent jobs.TemplateRenderer from the row's id, its ordinal, its address, its merge data and its own link, by row id.
func jltRenderAll(t *testing.T, jobID string, c jobs.Content, rows []jlRow, fields map[string]map[string]string) map[string]jltMsg {
	t.Helper()
	prep, err := jobs.TemplateRenderer{}.Prepare(jobID, c)
	if err != nil {
		t.Fatalf("an independent renderer refused the campaign: %v", err)
	}
	out := map[string]jltMsg{}
	for _, r := range rows {
		if r.State != "pending" {
			continue
		}
		f := fields[r.Norm]
		if f == nil {
			f = map[string]string{}
		}
		er := jobs.EngineRow{ID: r.ID, Ordinal: r.Ordinal, EmailNorm: r.Norm, Fields: f, UnsubscribeURL: jtLink(r.ID)}
		if miss := prep.Missing(er); len(miss) != 0 {
			t.Fatalf("row %d is pending but an independent renderer says it lacks %v", r.Ordinal, miss)
		}
		subject, body, text, err := prep.Render(er)
		if err != nil {
			t.Fatalf("an independent renderer refused row %d: %v", r.Ordinal, err)
		}
		out[r.ID] = jltMsgOf(subject, body, text)
	}
	return out
}

// ---- the money of a metered job -------------------------------------------------------------------------------------------------------------------------------------------------------------

// jltMetered asserts the credits of a finished module 2 job whose reserve was held for `sent` rows: the reserve is exactly sent x the preview's rate (a row left out is not in it), each sent row is charged once
// and none of the rows that were left out (their ids are `left`) is charged, the remainder is released, and the balance is what it was before the confirm less the charges. It returns the credits charged.
func (g *jltRig) jltMetered(user, job string, pv jobs.Preview, sent int, left []string, availBefore int64) int64 {
	t, s := g.T, g.S
	t.Helper()
	reserved, charged, released, held := s.reserveOf(job)
	if !held || reserved != int64(sent)*int64(pv.UnitCost) {
		t.Fatalf("the reserve of the job is %d (held %v), want %d row(s) x %d credits = %d: a row that is never sent is not in the reserve", reserved, held, sent, pv.UnitCost, int64(sent)*int64(pv.UnitCost))
	}
	ch := s.ledgerCharges(job)
	var sum int64
	leftSet := map[string]bool{}
	for _, id := range left {
		leftSet[id] = true
	}
	for _, c := range ch {
		sum += int64(c.Cost) * int64(c.Units)
		if leftSet[c.ItemRef] {
			t.Fatalf("the ledger charges row %s, a row the job left out: it is free", jlShort(c.ItemRef))
		}
	}
	if len(ch) != sent || charged != sum || sum < int64(sent) {
		t.Fatalf("the ledger holds %d charge row(s) worth %d credits and the reserve says %d were charged, want %d row(s) (one per sent message) and the two numbers equal", len(ch), sum, charged, sent)
	}
	if released != reserved-charged {
		t.Fatalf("the reserve released %d of the %d it holds after %d were charged, want the remainder %d", released, reserved, charged, reserved-charged)
	}
	if av, rv := s.balance(user, g.Module); av != availBefore-charged || rv != 0 {
		t.Fatalf("the balance is available %d reserved %d, want %d and 0 (what it was before the confirm less the %d credits charged)", av, rv, availBefore-charged, charged)
	}
	s.requireBooksScoped("after the templated job", job)
	return charged
}

// ---- the scenario: templates ----------------------------------------------------------------------------------------------------------------------------------------------------------------

// jltRunTemplates is the proof of exit criteria 1 to 3 for one sender: ONE templated send job of 17 rows, read at the sink.
//
//	byte-identical   every sent message (subject, HTML part, text part) equals what the second instance's Preview sample shows (three rows), what an independent renderer makes of the row, and the bytes of public.job_items
//	                 before AND after the second instance replays the engine's retry path (nothing is written twice); paired controls: a tampered copy is not equal, the recipients do not all render alike, the same
//	                 recipient rendered twice is equal, and the same row under another job id renders differently (the seed)
//	missing field    the two rows without the field that has no fallback are never at the sink (no message, no RCPT), their results are the platform's own free missing_field (P-09), they are not in the reserve and
//	                 not charged; the rows that have the field (and the one that lacks only fields with fallbacks) are sent
//	injection        a value that looks like a field or a group, a script tag, an attribute breaker and a line break followed by a Bcc header renders literally: escaped in the HTML, raw in the text, one line in the subject,
//	                 no extra header, one recipient; a benign value renders as plain text
func jltRunTemplates(g *jltRig) {
	t, s := g.T, g.S
	user := g.NewUser("tpl")
	prefix := g.Prefix("tpl")
	fx := jltTemplateFixture(prefix)
	content := jltTemplateContent()
	diag := func() string { return g.Diag(user) }
	nSend, nMiss := len(fx.Sendable), len(fx.Missing)
	storeB := s.jltStore() // the second platform instance

	// ---- create, preview (instance A and instance B), confirm ----
	j := s.create(user, jobs.CreateReq{Module: g.Module, Rows: fx.Rows, Content: &content})
	g.Attach(user, j.ID)
	pv := s.preview(user, j.ID)
	if pv.Rows != len(fx.Rows) || pv.Pending != len(fx.Rows) || pv.Malformed != 0 || pv.Duplicates != 0 || pv.Suppressed != 0 || pv.MissingFields != nMiss || pv.Unrenderable != 0 || pv.Units != nSend {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d suppressed %d missing %d unrenderable %d units %d, want %d, %d, 0, 0, 0, %d, 0, %d",
			pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Suppressed, pv.MissingFields, pv.Unrenderable, pv.Units, len(fx.Rows), len(fx.Rows), nMiss, nSend)
	}
	if !slices.Equal(pv.MissingFieldNames, []string{"company"}) || pv.MissingFieldNamesTruncated {
		t.Fatalf("the preview names the missing fields %v (truncated %v), want exactly [company]", pv.MissingFieldNames, pv.MissingFieldNamesTruncated)
	}
	if len(pv.Samples) != jobs.SampleCount {
		t.Fatalf("the preview holds %d sample message(s), want %d", len(pv.Samples), jobs.SampleCount)
	}
	if g.Metered && (!pv.Metered || pv.UnitCost < 1 || pv.Credits != int64(nSend)*int64(pv.UnitCost) || pv.Shortfall != 0) {
		t.Fatalf("the preview says metered %v, %d credits at %d each, shortfall %d, want %d sendable row(s) x the rate and no shortfall", pv.Metered, pv.Credits, pv.UnitCost, pv.Shortfall, nSend)
	}
	pvB, err := storeB.Preview(s.ctx, user, j.ID)
	if err != nil {
		t.Fatalf("the second instance's Preview of the same job: %v", err)
	}
	if pvB.Fingerprint != pv.Fingerprint || len(pvB.Samples) != len(pv.Samples) || !slices.Equal(pvB.Samples, pv.Samples) {
		t.Fatalf("the second instance previews the same job differently: fingerprints equal %v, its %d sample(s) equal to the first instance's %v", pvB.Fingerprint == pv.Fingerprint, len(pvB.Samples), slices.Equal(pvB.Samples, pv.Samples))
	}
	availBefore, _ := s.balance(user, g.Module)
	res := s.confirmOK(user, j.ID, pv.Fingerprint)
	if res.Job.Status != jobs.StatusConfirmed || res.Pending != nSend || res.MissingFields != nMiss || res.Unrenderable != 0 || res.Suppressed != 0 {
		t.Fatalf("the confirm answered status %s pending %d missing %d unrenderable %d suppressed %d, want confirmed, %d, %d, 0 and 0", res.Job.Status, res.Pending, res.MissingFields, res.Unrenderable, res.Suppressed, nSend, nMiss)
	}
	if g.Metered {
		if res.Reserved != int64(nSend)*int64(pv.UnitCost) || res.Reserved == int64(len(fx.Rows))*int64(pv.UnitCost) {
			t.Fatalf("the confirm held %d credits, want %d (%d sendable rows x %d), not %d (all %d rows)", res.Reserved, int64(nSend)*int64(pv.UnitCost), nSend, pv.UnitCost, int64(len(fx.Rows))*int64(pv.UnitCost), len(fx.Rows))
		}
		if av, rv := s.balance(user, g.Module); rv != res.Reserved || av != availBefore-res.Reserved {
			t.Fatalf("after the confirm the balance is available %d reserved %d, want %d and %d", av, rv, availBefore-res.Reserved, res.Reserved)
		}
	} else if res.Reserved != 0 {
		t.Fatalf("the confirm held %d credits for %s, which holds none", res.Reserved, g.Label)
	}

	// ---- the rows the platform ended itself ----
	rows := s.rows(j.ID)
	byAddr, byOrd := map[string]jlRow{}, map[int]jlRow{}
	fields := map[string]map[string]string{}
	for _, r := range rows {
		byAddr[r.Norm], byOrd[r.Ordinal] = r, r
	}
	for addr, f := range fx.By {
		r, ok := byAddr[addr]
		if !ok {
			t.Fatalf("the job has no row for recipient %d (%s)", f.Ordinal, addr)
		}
		fields[addr] = f.Fields
		want := "pending"
		if f.Kind == jltMissing {
			want = "missing_field"
		}
		if r.State != want || r.Submitted || (f.Kind == jltMissing && r.Reason != "company") {
			t.Fatalf("row %d is %s (reason %q, submitted %v), want %s (reason %q for a missing row)", f.Ordinal, r.State, r.Reason, r.Submitted, want, "company")
		}
	}
	var leftIDs []string
	for _, a := range fx.Missing {
		leftIDs = append(leftIDs, byAddr[a].ID)
	}
	if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); n != 0 {
		t.Fatalf("public.job_items holds %d row(s) of the campaign before the engine ran: the confirm must not write the module's rows", n)
	}

	// ---- the engine writes the items; the second instance replays the retry path ----
	s.tickUntil("the engine wrote the job's module-facing items", 2*time.Minute, 700*time.Millisecond, diag, func() (bool, string) {
		n := s.count("read whether the items were written", `SELECT count(*) FROM platform.jobs WHERE id = $1::uuid AND items_ensured_at IS NOT NULL`, j.ID)
		return n == 1, fmt.Sprintf("items_ensured_at is set on %d job(s)", n)
	})
	before := s.jltItems(j.ID)
	var pendingIDs []string
	for _, a := range fx.Sendable {
		pendingIDs = append(pendingIDs, byAddr[a].ID)
	}
	if len(before) != nSend {
		t.Fatalf("public.job_items holds %d row(s) of the campaign, want the %d sendable rows and none for a row that was left out", len(before), nSend)
	}
	for _, id := range pendingIDs {
		if _, ok := before[id]; !ok {
			t.Fatalf("a sendable row has no public.job_items row")
		}
	}
	if n, err := storeB.EnsureJobItems(s.ctx, j.ID); err != nil || n != 0 {
		t.Fatalf("the second instance's EnsureJobItems of a job whose items were written wrote %d, %v: want 0 and no error (write nothing twice)", n, err)
	}
	if n, err := storeB.EnsureModuleItems(s.ctx, j.ID, pendingIDs); err != nil || n != 0 {
		t.Fatalf("the second instance's EnsureModuleItems of rows that have items wrote %d, %v: want 0 and no error", n, err)
	}
	if after := s.jltItems(j.ID); !equalItems(before, after) {
		t.Fatalf("the replay of the engine's retry path changed public.job_items: the bytes written first are not the bytes there now")
	}

	// ---- the engine sends ----
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 4*time.Minute, diag)
	items := s.jltItems(j.ID)
	if !equalItems(before, items) {
		t.Fatalf("public.job_items changed while the job ran: a message was re-rendered after it was written")
	}

	// ---- READ AT THE SINK ----
	seen := g.sinkRead(prefix, 0)
	rcpt := g.sinkRcpt(prefix)
	for _, a := range fx.Missing {
		if n := len(seen[a]); n != 0 || rcpt[a] != (jltRcpt{}) {
			t.Fatalf("the sink holds %d message(s) and answered RCPT %+v for recipient %d, a row that lacks a required field: it must never be sent", n, rcpt[a], fx.By[a].Ordinal)
		}
	}
	wires := map[string]jltWire{}
	for _, a := range fx.Sendable {
		ms := seen[a]
		if len(ms) != 1 || rcpt[a].Accepted != 1 {
			t.Fatalf("the sink holds %d message(s) and accepted RCPT %d time(s) for recipient %d, want exactly 1 and 1: the row has everything its templates need", len(ms), rcpt[a].Accepted, fx.By[a].Ordinal)
		}
		wires[a] = ms[0].W
		if p := jltSafetyProblems(ms[0].W, ms[0].To); len(p) != 0 {
			t.Fatalf("recipient %d: the message the sink accepted is unsafe:\n  %s", fx.By[a].Ordinal, strings.Join(p, "\n  "))
		}
		if len(ms[0].W.Parts) != 2 || ms[0].W.Parts[0] != "text/plain" || ms[0].W.Parts[1] != "text/html" || ms[0].W.Text == "" || ms[0].W.HTML == "" {
			t.Fatalf("recipient %d: the parts of the message are %v, want text/plain then text/html, both with content (the blank text template makes a text part from the HTML)", fx.By[a].Ordinal, ms[0].W.Parts)
		}
	}
	if len(seen) != nSend {
		t.Fatalf("the sink holds messages to %d address(es) of this scenario, want %d", len(seen), nSend)
	}

	// ---- the results: the missing rows are the platform's own and FREE ----
	results := map[string]jlResult{}
	for _, r := range s.results(j.ID) {
		results[r.Ref] = r
	}
	if len(results) != len(fx.Rows) {
		t.Fatalf("the job holds %d result(s), want one for each of its %d rows", len(results), len(fx.Rows))
	}
	for _, a := range fx.Missing {
		r := results[byAddr[a].ID]
		if r.Origin != "platform" || r.State != "missing_field" || r.Reason != "company" || r.Decision != "free" || r.Unit != "" || r.Rule != "P-09" {
			t.Fatalf("the result of recipient %d is %+v, want the platform's own free missing_field result by P-09 naming the field company", fx.By[a].Ordinal, r)
		}
	}
	for _, a := range fx.Sendable {
		if r := results[byAddr[a].ID]; r.Origin != "module" || r.State != "sent" {
			t.Fatalf("the result of recipient %d is %+v, want a module result in state sent", fx.By[a].Ordinal, r)
		}
	}
	charged := int64(0)
	if g.Metered {
		charged = g.jltMetered(user, j.ID, pv, nSend, leftIDs, availBefore)
	} else {
		if _, _, _, held := s.reserveOf(j.ID); held || s.ledgerRowsOf(j.ID) != 0 {
			t.Fatalf("the job holds a reserve or ledger rows: %s is not metered", g.Label)
		}
		s.requireBooksScoped("after the templated job", j.ID)
	}

	// ---- 1: byte-identical across instances and retries ----
	indep := jltRenderAll(t, j.ID, content, rows, fields)
	again := jltRenderAll(t, j.ID, content, rows, fields) // the same recipient rendered twice, by two fresh renderers
	other := jltRenderAll(t, s.str("a fresh job id", `SELECT gen_random_uuid()::text`), content, rows, fields)
	var problems []string
	differ, spins, subjects := 0, map[string]bool{}, map[string]bool{}
	for _, a := range fx.Sendable {
		r := byAddr[a]
		w := wires[a].msg()
		it := items[r.ID]
		problems = append(problems, jltDiff(fmt.Sprintf("recipient %d, the sink against public.job_items", r.Ordinal), w, jltMsgOf(it.Subject, it.Body, it.Text))...)
		problems = append(problems, jltDiff(fmt.Sprintf("recipient %d, the sink against an independent render", r.Ordinal), w, indep[r.ID])...)
		if indep[r.ID] != again[r.ID] {
			problems = append(problems, fmt.Sprintf("recipient %d: two independent renders of the same row differ", r.Ordinal))
		}
		if it.Link != jtLink(r.ID) {
			problems = append(problems, fmt.Sprintf("recipient %d: the item's link is not the signer's link for the row", r.Ordinal))
		}
		if other[r.ID] != indep[r.ID] {
			differ++
		}
		sp, err := jltSpin(w.HTML)
		if err != nil {
			problems = append(problems, fmt.Sprintf("recipient %d: %v", r.Ordinal, err))
		}
		spins[sp] = true
		subjects[strings.SplitN(w.Subject, " ", 2)[0]] = true
	}
	for _, sm := range pvB.Samples {
		r, ok := byOrd[sm.Ordinal]
		if !ok || r.State != "pending" {
			problems = append(problems, fmt.Sprintf("the second instance's sample is of ordinal %d, which is not a row that was sent", sm.Ordinal))
			continue
		}
		problems = append(problems, jltDiff(fmt.Sprintf("recipient %d, the sink against the second instance's Preview sample", r.Ordinal), wires[r.Norm].msg(), jltMsgOf(sm.Subject, sm.HTML, sm.Text))...)
	}
	if len(problems) != 0 {
		t.Fatalf("the messages the sink accepted are not the messages the platform computes (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	// the paired controls: the comparison can fail, the spin follows the recipient, and the seed follows the job
	first := byAddr[fx.Sendable[0]]
	tampered := indep[first.ID]
	tampered.HTML = strings.Replace(tampered.HTML, "Sam", "Sal", 1)
	if len(jltDiff("control", wires[fx.Sendable[0]].msg(), tampered)) == 0 {
		t.Fatalf("control: a message with one byte changed compares equal to the sink's: the byte-for-byte comparison cannot fail")
	}
	if len(spins) < 5 || len(subjects) < 2 {
		t.Fatalf("control: the %d sent messages hold %d distinct draws of the HTML groups and %d distinct subject greetings, want at least 5 and 2: the recipients do not render differently, so the equality above shows nothing", nSend, len(spins), len(subjects))
	}
	if differ < nSend/2 {
		t.Fatalf("control: the same %d rows rendered under another job id differ from the sent messages for only %d of them, want most: the job id is not in the seed", nSend, differ)
	}

	// ---- 2: the missing rows were never sent and cost nothing (read above); the rows that have the field were ----
	// ---- 3: injection renders literally; a benign value renders as plain text ----
	for _, a := range fx.Sendable {
		if p := jltRowProblems(fx.By[a], wires[a]); len(p) != 0 {
			t.Fatalf("recipient %d: the sink's message is not what the row's values make:\n  %s", fx.By[a].Ordinal, strings.Join(p, "\n  "))
		}
	}
	t.Logf("NOTE jl templates %s: 17 rows (15 sendable: 12 ordinary and 3 hostile; 2 lacking the field company); the sink holds %d message(s) each exactly once and none for the 2 missing rows (no RCPT either); each equals public.job_items, an independent render and %d of the second instance's sample(s) byte for byte after the CRLF-as-LF tolerance; %d distinct HTML draws and %d subject greetings among them; the same rows under another job id differ in %d of %d; credits charged %d of %d reserved",
		g.Label, len(wires), len(pvB.Samples), len(spins), len(subjects), differ, nSend, charged, res.Reserved)
}

func equalItems(a, b map[string]jltItem) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ---- the scenario: unsubscribe --------------------------------------------------------------------------------------------------------------------------------------------------------------

var (
	jltLinkRe   = regexp.MustCompile(`https://platform\.example\.test/u/[A-Za-z0-9_-]{54}`)
	jltHeaderRe = regexp.MustCompile(`^<(https://[^<>\s]+)>$`)
)

// jltTail is a link as a failure message may show it: the last characters of its token only.
func jltTail(u string) string {
	if len(u) > 8 {
		return "..." + u[len(u)-8:]
	}
	return u
}

// jltLinkProblems is what must hold of one message's unsubscribe link: both headers exactly once, the pair's values, the URL of the header and the link in the HTML (and in the text) one string that appears once in
// the HTML, and its token naming the recipient's own row.
func jltLinkProblems(w jltWire, itemID string) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	lu, post := w.Values["list-unsubscribe"], w.Values["list-unsubscribe-post"]
	if len(lu) != 1 {
		say("the message has %d List-Unsubscribe header field(s), want exactly 1", len(lu))
	}
	if len(post) != 1 {
		say("the message has %d List-Unsubscribe-Post header field(s), want exactly 1", len(post))
	} else if post[0] != "List-Unsubscribe=One-Click" {
		say("List-Unsubscribe-Post is %q, want List-Unsubscribe=One-Click", post[0])
	}
	var hdr string
	if len(lu) == 1 {
		if m := jltHeaderRe.FindStringSubmatch(lu[0]); m == nil {
			say("List-Unsubscribe is not one https URL in angle brackets")
		} else {
			hdr = m[1]
		}
	}
	links := jltLinkRe.FindAllString(w.HTML, -1)
	switch {
	case len(links) != 1:
		say("the HTML part holds %d unsubscribe link(s), want exactly 1", len(links))
	case hdr != "" && links[0] != hdr:
		say("the link in the HTML (%s) is not the List-Unsubscribe URL (%s)", jltTail(links[0]), jltTail(hdr))
	}
	if hdr != "" {
		if !strings.Contains(w.Text, hdr) {
			say("the text part does not hold the link")
		}
		tok := strings.TrimPrefix(hdr, harnessPublicBase+"/u/")
		if id, ok := harnessUnsubSigner.Verify(tok); !ok || id != itemID {
			say("the token of the link names item %q (verified %v), want this recipient's own row", jlShort(id), ok)
		}
	}
	return p
}

// jltTokenOf is the token of a message's List-Unsubscribe header ("" when it has none).
func jltTokenOf(w jltWire) string {
	if lu := w.Values["list-unsubscribe"]; len(lu) == 1 {
		if m := jltHeaderRe.FindStringSubmatch(lu[0]); m != nil {
			return strings.TrimPrefix(m[1], harnessPublicBase+"/u/")
		}
	}
	return ""
}

// jltFilter is which of the addresses the user has suppressed, asked through internal/suppress as the platform asks it.
func (s *jlStack) jltFilter(user string, addrs ...string) map[string]bool {
	s.t.Helper()
	m, err := suppress.Filter(s.ctx, s.pool, user, addrs)
	if err != nil {
		s.t.Fatalf("suppress.Filter: %v", err)
	}
	return m
}

// jltOtherSigner is a signer under a secret that is not the harness's: the tokens it makes are well formed and signed, for the wrong key.
func jltOtherSigner() (*unsubscribe.Signer, error) {
	return unsubscribe.NewSigner([]byte(strings.Repeat("another-secret-than-the-harness", 2)))
}

// jltFlip changes the character of the token at index i to another of the token alphabet.
func jltFlip(tok string, i int) string {
	c := byte('A')
	if tok[i] == 'A' {
		c = 'B'
	}
	return tok[:i] + string(c) + tok[i+1:]
}

// jltUnsubscribeContent is the campaign of the unsubscribe proof: every row has everything it needs. With placeLink the template puts the recipient's link where it wants it ({{unsubscribe_url}}) and the
// platform adds no footer; without, the platform's footer carries it.
func jltUnsubscribeContent(placeLink bool) jobs.Content {
	body := "<html><body>\n<p>{Hello|Hi|Hey|Greetings} {{first_name|friend}}, a short note for {{company|your team}}.</p>\n"
	if placeLink {
		body += `<p><a href="{{unsubscribe_url}}">Stop these emails</a></p>` + "\n"
	}
	return jobs.Content{Subject: "{Hello|Hi|Hey} {{first_name|friend}}", HTML: body + "</body></html>"}
}

func jltCall(h http.Handler, method, token, contentType, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/u/"+token, rd)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const (
	jltFormType        = "application/x-www-form-urlencoded"
	jltOneClickBody    = "List-Unsubscribe=One-Click"
	jltPageThanks      = "You are unsubscribed"
	jltPageNotValid    = "This link is not valid"
	jltPageConfirm     = `<button type="submit">Unsubscribe</button>`
	jltUnsubRecipients = 14
)

// jltRunUnsubscribe is the proof of exit criterion 4 for one sender: a templated send job of 14 recipients; the List-Unsubscribe pair and the link of every message read at the sink; the public handler (GET changes
// nothing, the one-click POST of A and the form POST of B land on the user's suppression list, a forged token and a token of another secret are the same 404 and write nothing, a repeat is a success with no second row,
// another user's list is untouched); then the SAME user confirms a NEW job over the same list: A and B are suppressed at confirm, the sink receives nothing new for them and the others (C among them) are sent.
func jltRunUnsubscribe(g *jltRig, placeLink bool) {
	t, s := g.T, g.S
	user := g.NewUser("unsub")
	other := s.newUser("otheruser")
	prefix := g.Prefix("unsub")
	content := jltUnsubscribeContent(placeLink)
	diag := func() string { return g.Diag(user) }
	names := []string{"Alice", "Bob", "Carol", "Dave", "Erin", "Frank", "Grace", "Heidi", "Ivan", "Judy", "Mallory", "Niaj", "Olivia", "Peggy"}
	var addrs []string
	var rows []jobs.RowIn
	for i := 0; i < jltUnsubRecipients; i++ {
		a := fmt.Sprintf("%s%02d@example.test", prefix, i+1)
		addrs = append(addrs, a)
		rows = append(rows, jobs.RowIn{Email: a, Fields: map[string]string{"first_name": names[i], "company": fmt.Sprintf("Acme-%02d", i+1)}})
	}
	A, B, C := addrs[0], addrs[1], addrs[2]

	// ---- job 1: sent, and the links read at the sink ----
	j1 := s.create(user, jobs.CreateReq{Module: g.Module, Rows: rows, Content: &content})
	g.Attach(user, j1.ID)
	pv1 := s.preview(user, j1.ID)
	if pv1.Units != jltUnsubRecipients || pv1.Suppressed != 0 || pv1.MissingFields != 0 {
		t.Fatalf("the first preview counts units %d suppressed %d missing %d, want %d, 0, 0", pv1.Units, pv1.Suppressed, pv1.MissingFields, jltUnsubRecipients)
	}
	avail1, _ := s.balance(user, g.Module)
	res1 := s.confirmOK(user, j1.ID, pv1.Fingerprint)
	if res1.Pending != jltUnsubRecipients || res1.Suppressed != 0 {
		t.Fatalf("the first confirm answered pending %d suppressed %d, want %d and 0", res1.Pending, res1.Suppressed, jltUnsubRecipients)
	}
	s.tickToStatus(j1.ID, jobs.StatusSucceeded, 4*time.Minute, diag)
	rows1 := s.rows(j1.ID)
	id1 := map[string]string{}
	for _, r := range rows1 {
		id1[r.Norm] = r.ID
	}
	seen := g.sinkRead(prefix, 0)
	rcpt1 := g.sinkRcpt(prefix)
	tokens := map[string]string{}
	for _, a := range addrs {
		ms := seen[a]
		if len(ms) != 1 || rcpt1[a].Accepted != 1 {
			t.Fatalf("the sink holds %d message(s) and accepted RCPT %d time(s) for %s, want exactly 1 and 1", len(ms), rcpt1[a].Accepted, a)
		}
		w := ms[0].W
		if p := append(jltSafetyProblems(w, ms[0].To), jltLinkProblems(w, id1[a])...); len(p) != 0 {
			t.Fatalf("the message to %s: its unsubscribe link or headers are wrong:\n  %s", a, strings.Join(p, "\n  "))
		}
		if foot := strings.Contains(w.HTML, "To stop receiving these messages"); foot == placeLink {
			t.Fatalf("the message to %s has the platform's footer %v, want %v (the template places the link itself: %v)", a, foot, !placeLink, placeLink)
		}
		tok := jltTokenOf(w)
		for _, have := range tokens {
			if have == tok {
				t.Fatalf("two recipients carry the same unsubscribe token")
			}
		}
		tokens[a] = tok
	}
	if g.Metered {
		g.jltMetered(user, j1.ID, pv1, jltUnsubRecipients, nil, avail1)
	}

	// ---- the public handler ----
	h := s.jltEndpoint()
	count := func(u string) int64 {
		return s.count("count the user's suppressions", `SELECT count(*) FROM platform.suppressions WHERE user_id = $1::uuid`, u)
	}
	listed := func(u string, a ...string) map[string]bool { return s.jltFilter(u, a...) }
	// another user already holds a suppression of its own: it must stay exactly as it is
	if _, err := suppress.Add(s.ctx, s.pool, other, "someone-else@example.test", suppress.SourceManual); err != nil {
		t.Fatalf("suppress.Add for the other user: %v", err)
	}
	otherN := count(other)
	if count(user) != 0 || otherN != 1 {
		t.Fatalf("before any request the user holds %d suppression(s) and the other user %d, want 0 and 1", count(user), otherN)
	}
	// a GET changes nothing (it is only the confirm page); the POST that follows does
	for _, a := range []string{A, B} {
		rec := jltCall(h, http.MethodGet, tokens[a], "", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), jltPageConfirm) || strings.Contains(rec.Body.String(), jltPageThanks) {
			t.Fatalf("GET of a recipient's link answered %d with %.120q, want 200 and the confirm page", rec.Code, rec.Body.String())
		}
	}
	if n := count(user); n != 0 || len(listed(user, A, B)) != 0 {
		t.Fatalf("after two GETs the user holds %d suppression(s), want 0: a GET must change nothing", n)
	}
	// the one-click POST of A (RFC 8058: the body a mail client sends) lands A
	rec := jltCall(h, http.MethodPost, tokens[A], jltFormType, jltOneClickBody)
	thanks := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(thanks, jltPageThanks) {
		t.Fatalf("the one-click POST answered %d with %.120q, want 200 and the thank-you page", rec.Code, thanks)
	}
	if n := count(user); n != 1 || !listed(user, A, B, C)[A] || len(listed(user, A, B, C)) != 1 {
		t.Fatalf("after the one-click POST of A the user holds %d suppression(s) (A listed: %v), want exactly A", n, listed(user, A)[A])
	}
	if src := s.str("the source of A's suppression", `SELECT source FROM platform.suppressions WHERE user_id = $1::uuid AND email = $2`, user, A); src != "unsubscribe" {
		t.Fatalf("A's suppression has the source %q, want unsubscribe", src)
	}
	createdA := s.str("the time A's row was written", `SELECT created_at::text FROM platform.suppressions WHERE user_id = $1::uuid AND email = $2`, user, A)
	// the form POST of B (a click on the confirm page: the form posts no fields) lands B
	rec = jltCall(h, http.MethodPost, tokens[B], jltFormType, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), jltPageThanks) {
		t.Fatalf("the form POST answered %d with %.120q, want 200 and the thank-you page", rec.Code, rec.Body.String())
	}
	if n := count(user); n != 2 || len(listed(user, A, B, C)) != 2 || !listed(user, B)[B] {
		t.Fatalf("after the form POST of B the user holds %d suppression(s), want exactly A and B", n)
	}
	// a forged token (one character changed), a token of another secret and a garbage token are ONE answer and write nothing
	otherSigner, err := jltOtherSigner()
	if err != nil {
		t.Fatalf("unsubscribe.NewSigner: %v", err)
	}
	foreign, err := otherSigner.Token(id1[C])
	if err != nil {
		t.Fatalf("a token of another secret: %v", err)
	}
	garbage := jltCall(h, http.MethodPost, "not-a-token", jltFormType, jltOneClickBody)
	if garbage.Code != http.StatusNotFound || !strings.Contains(garbage.Body.String(), jltPageNotValid) {
		t.Fatalf("a garbage token answered %d with %.120q, want the 404 page", garbage.Code, garbage.Body.String())
	}
	for name, tok := range map[string]string{"a real token with one character changed": jltFlip(tokens[C], 20), "a token of another secret": foreign, "a real token with its last character changed": jltFlip(tokens[C], 53)} {
		if tok == tokens[C] {
			t.Fatalf("%s: the forged token is the real one", name)
		}
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			body := ""
			if method == http.MethodPost {
				body = jltOneClickBody
			}
			rec := jltCall(h, method, tok, jltFormType, body)
			if rec.Code != http.StatusNotFound || rec.Body.String() != garbage.Body.String() {
				t.Fatalf("%s: %s answered %d with %.120q, want the one 404 page every wrong token gets", name, method, rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s: the 404 carries Cache-Control %q, want no-store", name, rec.Header().Get("Cache-Control"))
			}
		}
	}
	if n := count(user); n != 2 || listed(user, C)[C] {
		t.Fatalf("after the forged tokens the user holds %d suppression(s) (C listed: %v), want still A and B only", n, listed(user, C)[C])
	}
	// a repeat is a success and writes no second row
	rec = jltCall(h, http.MethodPost, tokens[A], jltFormType, jltOneClickBody)
	if rec.Code != http.StatusOK || rec.Body.String() != thanks {
		t.Fatalf("a repeated POST answered %d, want 200 and the same page", rec.Code)
	}
	if n := count(user); n != 2 || s.str("the time A's row was written", `SELECT created_at::text FROM platform.suppressions WHERE user_id = $1::uuid AND email = $2`, user, A) != createdA {
		t.Fatalf("after a repeated POST the user holds %d suppression(s), want 2 with A's row untouched", n)
	}
	// another user's list is untouched, and the same two addresses are not on it
	if n := count(other); n != otherN || len(listed(other, A, B, C)) != 0 {
		t.Fatalf("the other user holds %d suppression(s) (was %d) and lists %d of the unsubscribed addresses, want its own one only", n, otherN, len(listed(other, A, B, C)))
	}

	// ---- job 2: the SAME user, the SAME list ----
	mark := g.sinkMaxSeq()
	j2 := s.create(user, jobs.CreateReq{Module: g.Module, Rows: rows, Content: &content})
	g.Attach(user, j2.ID)
	pv2 := s.preview(user, j2.ID)
	if pv2.Pending != jltUnsubRecipients || pv2.Suppressed != 2 || pv2.Units != jltUnsubRecipients-2 {
		t.Fatalf("the second preview counts pending %d suppressed %d units %d, want %d, 2 and %d", pv2.Pending, pv2.Suppressed, pv2.Units, jltUnsubRecipients, jltUnsubRecipients-2)
	}
	avail2, _ := s.balance(user, g.Module)
	res2 := s.confirmOK(user, j2.ID, pv2.Fingerprint)
	if res2.Suppressed != 2 || res2.Pending != jltUnsubRecipients-2 {
		t.Fatalf("the second confirm answered suppressed %d pending %d, want 2 and %d", res2.Suppressed, res2.Pending, jltUnsubRecipients-2)
	}
	s.tickToStatus(j2.ID, jobs.StatusSucceeded, 4*time.Minute, diag)
	rows2 := s.rows(j2.ID)
	id2 := map[string]string{}
	for _, r := range rows2 {
		id2[r.Norm] = r.ID
		want := "pending"
		if r.Norm == A || r.Norm == B {
			want = "suppressed"
		}
		if r.State != want {
			t.Fatalf("a row of the second job is %s, want %s (row %d)", r.State, want, r.Ordinal)
		}
	}
	for _, r := range s.results(j2.ID) {
		if (r.Email == A || r.Email == B) && (r.Origin != "platform" || r.State != "suppressed" || r.Decision != "free") {
			t.Fatalf("the result of an unsubscribed recipient is %+v, want the platform's own free suppressed result", r)
		}
	}
	// AT THE SINK: nothing new for A and B, one new message for each of the others (the control: C did not unsubscribe)
	newSeen := g.sinkRead(prefix, mark)
	rcpt2 := g.sinkRcpt(prefix)
	for _, a := range addrs {
		wantNew, wantRcpt := 1, 2
		if a == A || a == B {
			wantNew, wantRcpt = 0, 1
		}
		if n := len(newSeen[a]); n != wantNew || rcpt2[a].Accepted != wantRcpt {
			t.Fatalf("after the second job the sink holds %d new message(s) for %s and has accepted RCPT %d time(s) in all, want %d and %d (A and B unsubscribed, the others did not)", n, a, rcpt2[a].Accepted, wantNew, wantRcpt)
		}
		if wantNew == 1 {
			w := newSeen[a][0].W
			if p := append(jltSafetyProblems(w, newSeen[a][0].To), jltLinkProblems(w, id2[a])...); len(p) != 0 {
				t.Fatalf("the second message to %s is wrong:\n  %s", a, strings.Join(p, "\n  "))
			}
			if jltTokenOf(w) == tokens[a] {
				t.Fatalf("the second job's message to %s carries the first job's token: a link names the item of its own job", a)
			}
		}
	}
	if len(newSeen[C]) != 1 {
		t.Fatalf("control: the recipient that did not unsubscribe got %d new message(s), want 1", len(newSeen[C]))
	}
	if g.Metered {
		g.jltMetered(user, j2.ID, pv2, jltUnsubRecipients-2, []string{id2[A], id2[B]}, avail2)
	}
	t.Logf("NOTE jl unsubscribe %s (link placed by the template: %v): %d messages each with one List-Unsubscribe and one List-Unsubscribe-Post, the URL equal to the link in the HTML and the token naming the recipient's own row; the GET changed nothing, the one-click POST of A and the form POST of B suppressed them, forged tokens were the one 404 and wrote nothing, a repeat wrote no second row; the second job suppressed A and B at confirm: %d new message(s) at the sink, none for A and B",
		g.Label, placeLink, jltUnsubRecipients, len(newSeen))
}
