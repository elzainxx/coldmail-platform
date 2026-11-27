//go:build harness

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"regexp"
	"sort"
	"strings"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/render"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
)

// THE RENDERED BYTES OF A SEND JOB (Phase 5 exit criterion: the same job and recipient render byte-identically across retries and a SIGKILL-resume of the platform process; chunk S9). verify holds the message each
// recipient RECEIVED, read from the controllable sink's raw message (subject, HTML part, text part), against the PURE RENDER of that recipient's row for that job id, which this program computes ITSELF, here,
// from what the database holds AFTER the run: the job id, the item id (platform.job_rows.id, the id the platform hands every module), the row's merge data and the job's frozen campaign
// (platform.jobs.frozen_settings, key content), through the platform's own public code and no copy of it:
//
//   - jobs.TemplateRenderer{}.Prepare(job, campaign) and PreparedCampaign.Render(row): the subject, the HTML with the unsubscribe footer the platform adds, the text (the explicit template, or the text made from
//     the final HTML), exactly as insertItems builds them. Nothing of the footer or of the text conversion is written again here;
//   - internal/render, directly, for the subject: render.Compile and Template.Render with render.SpinSeed(job, item) and the part seeds (render.PartSeed, applied inside Render), a SECOND route to the same
//     bytes, so that the seed contract itself (job id and item id, the subject's own stream) is held and not only the renderer's use of it;
//   - internal/unsubscribe: the link of an item is made with the platform's own signer under the platform's secret and public address (SENDJOBS_UNSUBSCRIBE_SECRET and SENDJOBS_PUBLIC_BASE_URL are the values the
//     instances were given), never read back from the database, so a platform that wrote another link (or signed under another secret) differs from what is expected.
//
// Nothing here is remembered across a kill: verify runs after the job ended, from the database, and a replay of a killed step (the same job id, the same rows) is what it reads.
//
// ONE CANONICALISATION, and only one: the senders put a message on the wire with CRLF line endings (SMTP's own: both workers dot-stuff and turn every line feed into CR LF), so a line ending is read as a line feed
// on both sides before the two are compared. Nothing else is normalised: a byte of a subject, an HTML part or a text part that differs is a failure that names the part and the first byte.
//
// WHAT THIS DOES NOT PROVE: that the footer or the text conversion are RIGHT (internal/jobs and internal/render have their own tests and the preview and live-template tests read them); it proves that what reached the
// recipient is what the platform's own pure function makes of the stored job and row, whoever rendered it, however many times, across a kill.

const (
	// the two dials the platform's instances sign and address their unsubscribe links by (PLATFORM_UNSUBSCRIBE_SECRET, PLATFORM_PUBLIC_BASE_URL): the scenario program is given THE SAME values
	// (docker-compose.sendjobs.yml), to make the link of an item itself with the platform's own code
	sjUnsubSecret = "SENDJOBS_UNSUBSCRIBE_SECRET"
	sjPublicBase  = "SENDJOBS_PUBLIC_BASE_URL"

	oneClickValue = "List-Unsubscribe=One-Click" // RFC 8058: the one value of List-Unsubscribe-Post
	// minSpinSample is how many compared messages it takes before "the spin varies" is asserted: three draws of the template's groups coincide with a probability far below one in a million, two with about one
	// in eight thousand, which on a few hundred runs of a scope would be a flake.
	minSpinSample = 3
)

// loadLinkMaker is the platform's own unsubscribe link maker (internal/unsubscribe, the one jobs.WithUnsubscribe is given) under the values the instances were given.
func loadLinkMaker() (func(itemID string) (string, error), error) {
	secret, err := sendVar(sjUnsubSecret)
	if err != nil {
		return nil, err
	}
	base, err := sendVar(sjPublicBase)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(base, "https://") {
		return nil, failf(exitFail, "%s is not an https URL: the platform refuses a link that is not, and the scenario program makes the platform's own links", sjPublicBase)
	}
	s, err := unsubscribe.NewSigner([]byte(secret))
	if err != nil {
		return nil, failf(exitFail, "%s is shorter than the %d bytes the platform's PLATFORM_UNSUBSCRIBE_SECRET must be", sjUnsubSecret, unsubscribe.MinSecretBytes)
	}
	return s.LinkMaker(base), nil
}

// ---- what the database holds, after the run --------------------------------------------------------------------------------------------------------------------------------------------------

// renderRow is one pending row of the job: what the platform's renderer is given of it.
type renderRow struct {
	ID      string // platform.job_rows.id, which is the item id, the seed's second half and public.job_items.id
	Ordinal int
	Email   string // the normalised address
	Fields  map[string]string
}

// renderInputs reads the job's frozen campaign and its pending rows with their merge data, as the database holds them now.
func (d *db) renderInputs(ctx context.Context, job string) (jobs.Content, []renderRow, error) {
	var c jobs.Content
	var raw []byte
	if err := d.c.QueryRow(ctx, `SELECT frozen_settings -> 'content' FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&raw); err != nil {
		return c, nil, d.scrub(err)
	}
	if len(raw) == 0 {
		return c, nil, failf(exitFail, "job %s holds no frozen campaign (frozen_settings has no content): it is not a send job, or its content was cleared by the retention", job)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, nil, failf(exitFail, "the frozen campaign of job %s is not a campaign (subject, html, text, reply_to)", job)
	}
	rs, err := d.c.Query(ctx, `SELECT id::text, ordinal, COALESCE(email_norm, ''), fields FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending' ORDER BY ordinal`, job)
	if err != nil {
		return c, nil, d.scrub(err)
	}
	defer rs.Close()
	var rows []renderRow
	for rs.Next() {
		var r renderRow
		var fraw []byte
		if err := rs.Scan(&r.ID, &r.Ordinal, &r.Email, &fraw); err != nil {
			return c, nil, d.scrub(err)
		}
		var m map[string]any
		if err := json.Unmarshal(fraw, &m); err != nil {
			return c, nil, failf(exitFail, "the merge data of row %d of job %s is not a JSON object", r.Ordinal, job)
		}
		r.Fields = make(map[string]string, len(m))
		for name, v := range m {
			s, ok := v.(string)
			if !ok {
				return c, nil, failf(exitFail, "the merge value %q of row %d of job %s is not text: this scenario writes text only", name, r.Ordinal, job)
			}
			r.Fields[name] = s
		}
		rows = append(rows, r)
	}
	return c, rows, d.scrub(rs.Err())
}

// storedItem is one public.job_items row of the campaign: what the platform wrote for the modules to send (the text is "" when the column is NULL).
type storedItem struct{ Subject, HTML, Text, Link string }

func (d *db) storedItems(ctx context.Context, job string) (map[string]storedItem, error) {
	rs, err := d.c.Query(ctx, `SELECT id::text, rendered_subject, rendered_body, COALESCE(rendered_text, ''), COALESCE(unsubscribe_url, '') FROM public.job_items WHERE campaign_id = $1::uuid`, job)
	if err != nil {
		return nil, d.scrub(err)
	}
	defer rs.Close()
	out := map[string]storedItem{}
	for rs.Next() {
		var id string
		var s storedItem
		if err := rs.Scan(&id, &s.Subject, &s.HTML, &s.Text, &s.Link); err != nil {
			return nil, d.scrub(err)
		}
		out[strings.ToLower(id)] = s
	}
	return out, d.scrub(rs.Err())
}

// ---- the pure render --------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// wantMsg is the pure render of one pending row for the job: the three parts, the link of the item, and the SPIN SIGNATURE (the three parts rendered for the same job and item with the same merge data for every
// recipient, so that only the seed can make two of them differ).
type wantMsg struct {
	ID, Addr, Link      string
	Ordinal             int
	Subject, HTML, Text string
	Spin                string
}

// expectedRenders computes the pure render of every pending row (keyed by the lower-case address) and the problems it met.
func expectedRenders(jobID string, c jobs.Content, rows []renderRow, link func(string) (string, error)) (map[string]wantMsg, []string) {
	want := map[string]wantMsg{}
	prep, err := jobs.TemplateRenderer{}.Prepare(jobID, c)
	if err != nil {
		return want, []string{"the job's frozen templates do not compile under the platform's own renderer: " + clip(err.Error(), 300)}
	}
	subj, err := render.Compile(c.Subject)
	if err != nil {
		return want, []string{"the job's frozen subject compiles for the platform's renderer and not for internal/render: " + clip(err.Error(), 300)}
	}
	spinData := map[string]string{} // the same value for every merge field of the three templates: what is left to tell two recipients apart is the seed
	for _, src := range []string{c.Subject, c.HTML, c.Text} {
		t, err := render.Compile(src)
		if err != nil {
			return want, []string{"a frozen template compiles for the platform's renderer and not for internal/render: " + clip(err.Error(), 300)}
		}
		for _, f := range t.Fields() {
			if !render.IsReserved(f.Name) {
				spinData[f.Name] = "x"
			}
		}
	}
	var probs []string
	for _, r := range rows {
		l, err := link(r.ID)
		if err != nil {
			probs = append(probs, fmt.Sprintf("row %d: the unsubscribe link of item %s cannot be made", r.Ordinal, short(r.ID)))
			continue
		}
		er := jobs.EngineRow{ID: r.ID, Ordinal: r.Ordinal, EmailNorm: r.Email, Fields: r.Fields, UnsubscribeURL: l}
		if miss := prep.Missing(er); len(miss) > 0 {
			probs = append(probs, fmt.Sprintf("row %d is a pending row that lacks the merge field(s) %s: a confirm leaves such a row out, so none of them can be pending", r.Ordinal, strings.Join(miss, ",")))
			continue
		}
		s, h, t, err := prep.Render(er)
		if err != nil {
			probs = append(probs, fmt.Sprintf("row %d: the platform's own renderer refuses the row: %s", r.Ordinal, clip(err.Error(), 200)))
			continue
		}
		vals := func(name string) (string, bool) { // the lookup of template.go's values, for the subject only
			switch name {
			case render.FieldEmail:
				return r.Email, true
			case render.FieldUnsubscribeURL:
				return l, true
			}
			v, ok := r.Fields[name]
			return v, ok
		}
		if direct := subj.Render(render.SpinSeed(jobID, r.ID), render.PartSubject, vals); direct != s {
			probs = append(probs, fmt.Sprintf("row %d: the subject has two pure renders (the platform's TemplateRenderer, and internal/render's Render with SpinSeed(job, item) and the subject's part seed) and they differ", r.Ordinal))
			continue
		}
		if strings.TrimSpace(t) == "" { // a blank text is no text part (public.job_items.rendered_text is NULL)
			t = ""
		}
		sr := er
		sr.EmailNorm, sr.Fields, sr.UnsubscribeURL = "spin@example.test", spinData, "https://platform.example.test/u/spin"
		ss, sh, st, err := prep.Render(sr)
		if err != nil {
			probs = append(probs, fmt.Sprintf("row %d: the platform's own renderer refuses the row with constant merge data: %s", r.Ordinal, clip(err.Error(), 200)))
			continue
		}
		addr := strings.ToLower(r.Email)
		if _, dup := want[addr]; dup {
			probs = append(probs, fmt.Sprintf("row %d: another pending row of the job has the same address: the platform sends one message to an address", r.Ordinal))
			continue
		}
		want[addr] = wantMsg{ID: r.ID, Addr: addr, Link: l, Ordinal: r.Ordinal, Subject: s, HTML: h, Text: t, Spin: ss + "\x00" + sh + "\x00" + st}
	}
	return want, probs
}

// wantsInOrder is the expected messages by position in the list (a stable order for what is printed).
func wantsInOrder(want map[string]wantMsg) []wantMsg {
	out := make([]wantMsg, 0, len(want))
	for _, w := range want {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

// ---- what the sink received -------------------------------------------------------------------------------------------------------------------------------------------------------------------

// gotMsg is one accepted message, read from its raw bytes: the Subject header (an RFC 2047 word decoded), the two parts of the multipart/alternative (or the HTML of a single text/html part) decoded from their
// transfer encoding, and every value of the two List-Unsubscribe headers.
type gotMsg struct {
	Subject, HTML, Text string
	Unsub, UnsubPost    []string
}

// sentMsg is one message the sink accepted for a recipient of the job; Err says why its raw bytes could not be read ("" when they could).
type sentMsg struct {
	Seq  int64
	Addr string
	Msg  gotMsg
	Err  string
}

// decodeTransfer undoes a part's Content-Transfer-Encoding (7bit, 8bit and binary are the bytes themselves; quoted-printable and base64 are decoded).
func decodeTransfer(cte string, raw []byte) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		b, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		return string(b), err
	case "base64":
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		return string(b), err
	}
	return string(raw), nil
}

// parseSinkMessage reads the raw bytes the sink accepted (dot-stuffing undone: what the SMTP DATA carried).
func parseSinkMessage(raw []byte) (gotMsg, error) {
	var g gotMsg
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return g, fmt.Errorf("it does not parse as a mail message (%v)", err)
	}
	if n := len(msg.Header["Subject"]); n != 1 {
		return g, fmt.Errorf("it has %d Subject headers, want exactly 1", n)
	}
	g.Subject = msg.Header.Get("Subject")
	if dec, err := new(mime.WordDecoder).DecodeHeader(g.Subject); err == nil {
		g.Subject = dec
	}
	g.Unsub, g.UnsubPost = msg.Header["List-Unsubscribe"], msg.Header["List-Unsubscribe-Post"]
	ct, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return g, fmt.Errorf("it has no readable Content-Type (%v)", err)
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return g, fmt.Errorf("its body cannot be read (%v)", err)
	}
	switch {
	case ct == "multipart/alternative":
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		plain, html := 0, 0
		for {
			p, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return g, fmt.Errorf("a part of the multipart/alternative does not parse (%v)", err)
			}
			pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			pb, err := io.ReadAll(p)
			if err != nil {
				return g, fmt.Errorf("a %s part cannot be read (%v)", pt, err)
			}
			dec, err := decodeTransfer(p.Header.Get("Content-Transfer-Encoding"), pb)
			if err != nil {
				return g, fmt.Errorf("a %s part does not decode under its Content-Transfer-Encoding (%v)", pt, err)
			}
			switch pt {
			case "text/plain":
				plain++
				g.Text = dec
			case "text/html":
				html++
				g.HTML = dec
			default:
				return g, fmt.Errorf("a part of the multipart/alternative is %q, want text/plain and text/html", pt)
			}
		}
		if plain > 1 || html > 1 {
			return g, fmt.Errorf("the multipart/alternative holds %d text/plain and %d text/html parts, want at most one of each", plain, html)
		}
	case ct == "text/html":
		dec, err := decodeTransfer(msg.Header.Get("Content-Transfer-Encoding"), body)
		if err != nil {
			return g, fmt.Errorf("the text/html body does not decode under its Content-Transfer-Encoding (%v)", err)
		}
		g.HTML = dec
	default:
		return g, fmt.Errorf("its Content-Type is %q, want multipart/alternative or text/html", ct)
	}
	return g, nil
}

// sinkMessages is every message the sink accepted for an address that begins with the prefix (a user's), with its raw bytes read and parsed, by recipient and then by arrival.
func (e *sendEnv) sinkMessages(ctx context.Context, prefix string) ([]sentMsg, error) {
	b, err := e.expect(ctx, "the controllable sink's messages", http.MethodGet, e.ctl+"/api/messages", "", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var list []sinkMsg
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, failf(exitFail, "the controllable sink's /api/messages is not a list of messages")
	}
	var out []sentMsg
	for _, m := range list {
		addr := ""
		for _, raw := range m.To {
			if a := strings.ToLower(strings.TrimSpace(raw)); strings.HasPrefix(a, prefix) {
				addr = a
				break
			}
		}
		if addr == "" {
			continue
		}
		rb, err := e.expect(ctx, fmt.Sprintf("the controllable sink's raw message %d", m.Seq), http.MethodGet, fmt.Sprintf("%s/api/message?seq=%d", e.ctl, m.Seq), "", nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		s := sentMsg{Seq: m.Seq, Addr: addr}
		if g, perr := parseSinkMessage(rb); perr != nil {
			s.Err = perr.Error()
		} else {
			s.Msg = g
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Addr != out[j].Addr {
			return out[i].Addr < out[j].Addr
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// ---- the checks (pure: each returns the problems it found, none is a pass) -------------------------------------------------------------------------------------------------------------------

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// concat is the problems of several checks as one list.
func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// the capability part of an unsubscribe link is not printed (a log of the run is public; the link names an item of the harness's fake recipients, and there is no reason to publish it)
var tokenRe = regexp.MustCompile(`/u/[A-Za-z0-9_-]{20,}`)

func redact(s string) string { return tokenRe.ReplaceAllString(s, "/u/[token]") }

func around(s string, i int) string { return redact(s[max(0, i-24):min(len(s), i+24)]) }

// partDiff is "" when the two texts are the same and the first difference in a few words otherwise. With wire set a line ending is read as a line feed on both sides first (see the header of this file).
func partDiff(part, want, got string, wire bool) string {
	if wire {
		want, got = lf(want), lf(got)
	}
	if want == got {
		return ""
	}
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	return fmt.Sprintf("the %s differs at byte %d (the pure render has %d bytes, the other %d): pure %q, other %q", part, i, len(want), len(got), around(want, i), around(got, i))
}

func (m gotMsg) signature() string { return m.Subject + "\x00" + m.HTML + "\x00" + m.Text }

// renderStats is how many readable messages of pending rows were compared and how many different messages they were.
func renderStats(want map[string]wantMsg, got []sentMsg) (compared, distinct int) {
	seen := map[string]bool{}
	for _, g := range got {
		if _, ok := want[g.Addr]; ok && g.Err == "" {
			compared++
			seen[g.Msg.signature()] = true
		}
	}
	return compared, len(seen)
}

// renderedBytesEqual: every message the sink accepted for the job's recipients is, subject, HTML part and text part, the pure render of its recipient's row (line endings read as line feeds); at least one message
// was compared; and, from minSpinSample messages on, the messages are not all the same and the pure renders of those recipients do not all make the same spin choices (a renderer that ignores the seed would
// give every recipient the same).
func renderedBytesEqual(want map[string]wantMsg, got []sentMsg) []string {
	if len(got) == 0 {
		return []string{"the sink holds no message for the job's recipients: no rendered message was compared (a proof of an empty set)"}
	}
	var unread, stray, diffs []string
	spins := map[string]bool{}
	for _, g := range got {
		w, ok := want[g.Addr]
		switch {
		case g.Err != "":
			unread = append(unread, fmt.Sprintf("%s (message %d): %s", g.Addr, g.Seq, g.Err))
			continue
		case !ok:
			stray = append(stray, g.Addr)
			continue
		}
		spins[w.Spin] = true
		for _, c := range []struct{ part, want, got string }{{"subject", w.Subject, g.Msg.Subject}, {"html", w.HTML, g.Msg.HTML}, {"text", w.Text, g.Msg.Text}} {
			if d := partDiff(c.part, c.want, c.got, true); d != "" {
				diffs = append(diffs, fmt.Sprintf("%s (ordinal %d, item %s): %s", g.Addr, w.Ordinal, short(w.ID), d))
			}
		}
	}
	var out []string
	if len(unread) > 0 {
		out = append(out, "messages the scenario program cannot read: "+sample(unread))
	}
	if len(stray) > 0 {
		out = append(out, "messages to recipients that are no pending row of the job (there is no pure render to compare them with): "+sample(stray))
	}
	if len(diffs) > 0 {
		out = append(out, "messages that differ from the pure render of their row: "+sample(diffs))
	}
	compared, distinct := renderStats(want, got)
	if compared == 0 && len(unread) == 0 && len(stray) == 0 {
		out = append(out, "no message of a pending row was compared")
	}
	if compared >= minSpinSample {
		if distinct < 2 {
			out = append(out, fmt.Sprintf("all %d messages the sink accepted are byte-identical: the spin did not vary from one recipient to the next", compared))
		}
		if len(spins) < 2 {
			out = append(out, fmt.Sprintf("the pure render gives the %d compared recipients the same spin choices, though their merge data is made the same: the seed (job id, item id) does not drive the choice", compared))
		}
	}
	return out
}

// storedEqualsPure: the module-facing row the platform wrote for each pending row (public.job_items) holds, byte for byte, the pure render of the row, and the link the harness's own secret makes for the item.
func storedEqualsPure(want map[string]wantMsg, stored map[string]storedItem) []string {
	var missing, diffs []string
	for _, w := range wantsInOrder(want) {
		s, ok := stored[w.ID]
		if !ok {
			missing = append(missing, fmt.Sprintf("row %d (item %s)", w.Ordinal, short(w.ID)))
			continue
		}
		for _, c := range []struct{ part, want, got string }{{"stored subject", w.Subject, s.Subject}, {"stored html", w.HTML, s.HTML}, {"stored text", w.Text, s.Text}} {
			if d := partDiff(c.part, c.want, c.got, false); d != "" {
				diffs = append(diffs, fmt.Sprintf("row %d (item %s): %s", w.Ordinal, short(w.ID), d))
			}
		}
		if s.Link != w.Link {
			diffs = append(diffs, fmt.Sprintf("row %d (item %s): the stored unsubscribe_url is not the link the harness's own secret and public address make for the item (the instances and the scenario program do not share them, or the platform wrote another link)", w.Ordinal, short(w.ID)))
		}
	}
	var out []string
	if len(missing) > 0 {
		out = append(out, "pending rows with no public.job_items row to compare: "+sample(missing))
	}
	if len(diffs) > 0 {
		out = append(out, "module-facing rows that differ from the pure render of their row: "+sample(diffs))
	}
	return out
}

// unsubscribeHeadersOK: every message the sink accepted for a pending row carries exactly one List-Unsubscribe header, the link of ITS OWN item in angle brackets, and exactly one List-Unsubscribe-Post header
// that says List-Unsubscribe=One-Click; at least one message was read.
func unsubscribeHeadersOK(want map[string]wantMsg, got []sentMsg) []string {
	if len(got) == 0 {
		return []string{"the sink holds no message for the job's recipients: no List-Unsubscribe header was read (a proof of an empty set)"}
	}
	var bad []string
	checked := 0
	for _, g := range got {
		w, ok := want[g.Addr]
		if g.Err != "" || !ok {
			continue // renderedBytesEqual names it
		}
		checked++
		switch {
		case len(g.Msg.Unsub) != 1:
			bad = append(bad, fmt.Sprintf("%s carries %d List-Unsubscribe header(s), want exactly 1", g.Addr, len(g.Msg.Unsub)))
		case g.Msg.Unsub[0] != "<"+w.Link+">":
			bad = append(bad, fmt.Sprintf("%s: its List-Unsubscribe is %q, want the link of its own item %q", g.Addr, redact(g.Msg.Unsub[0]), redact("<"+w.Link+">")))
		}
		if len(g.Msg.UnsubPost) != 1 || g.Msg.UnsubPost[0] != oneClickValue {
			bad = append(bad, fmt.Sprintf("%s: its List-Unsubscribe-Post is %q (%d header(s)), want exactly one that says %s", g.Addr, strings.Join(g.Msg.UnsubPost, " | "), len(g.Msg.UnsubPost), oneClickValue))
		}
	}
	var out []string
	if checked == 0 {
		out = append(out, "no message of a pending row could be read, so no List-Unsubscribe header was checked")
	}
	if len(bad) > 0 {
		out = append(out, "messages whose List-Unsubscribe pair is not exactly the one of their own item: "+sample(bad))
	}
	return out
}

// ---- the controls ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// flipByte is s with one bit of its middle byte flipped (a string that is empty becomes "x"): a value that can never equal the original, byte for byte.
func flipByte(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	b[len(b)/2] ^= 0x01
	return string(b)
}

func mentions(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func copyWants(w map[string]wantMsg) map[string]wantMsg {
	out := make(map[string]wantMsg, len(w))
	for k, v := range w {
		out[k] = v
	}
	return out
}

// liveRenderControl is the paired control INSIDE a verify, on the job's own data: one byte flipped in what the pure render says the first readable recipient must have received, subject, HTML part and text part
// in turn, must turn the comparison red for the part it names; one byte flipped in the expected link must turn the header check red. A job whose sink holds no message for it cannot show that the comparison
// is able to fail, which is a failure of its own.
func liveRenderControl(want map[string]wantMsg, got []sentMsg) []string {
	var target *sentMsg
	for i := range got {
		if _, ok := want[got[i].Addr]; ok && got[i].Err == "" {
			target = &got[i]
			break
		}
	}
	if target == nil {
		return []string{"the live render control had no readable message of a pending row to corrupt: the comparison was never shown able to fail on this job's own messages"}
	}
	var failed []string
	for _, part := range []string{"subject", "html", "text"} {
		c := copyWants(want)
		w := c[target.Addr]
		switch part {
		case "subject":
			w.Subject = flipByte(w.Subject)
		case "html":
			w.HTML = flipByte(w.HTML)
		default:
			w.Text = flipByte(w.Text)
		}
		c[target.Addr] = w
		if !mentions(renderedBytesEqual(c, got), "the "+part+" differs") {
			failed = append(failed, fmt.Sprintf("the live control: one byte flipped in the expected %s of %s was NOT found by the comparison of the rendered bytes", part, target.Addr))
		}
	}
	c := copyWants(want)
	w := c[target.Addr]
	w.Link = flipByte(w.Link)
	c[target.Addr] = w
	if !mentions(unsubscribeHeadersOK(c, got), "its List-Unsubscribe is") {
		failed = append(failed, fmt.Sprintf("the live control: one byte flipped in the expected link of %s was NOT found by the List-Unsubscribe check", target.Addr))
	}
	return failed
}

// controlRenderDetectsCorruption is the control of the three checks above on a small consistent fixture (three recipients, whose messages carry CRLF line endings, as on the wire): every check passes on it, and each
// corruption is found by the check that guards it, FOR THE REASON it names.
func controlRenderDetectsCorruption() []string {
	var failed []string
	linkOf := func(n int) string { return fmt.Sprintf("https://platform.example.test/u/ctl%d", n) }
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	var wants []wantMsg
	for n := 1; n <= 3; n++ {
		l := linkOf(n)
		wants = append(wants, wantMsg{ID: fmt.Sprintf("%08x-0000-4000-8000-000000000000", n), Ordinal: n, Addr: fmt.Sprintf("sj-ctl-%02d@example.test", n), Link: l,
			Subject: fmt.Sprintf("Quick note %d for Ann", n), HTML: fmt.Sprintf("<p>Hi %d,</p>\n<p>To stop, <a href=\"%s\">unsubscribe</a>.</p>", n, l), Text: fmt.Sprintf("Hi %d,\n\nTo stop: %s", n, l), Spin: fmt.Sprintf("spin-%d", n)})
	}
	wantOf := func() map[string]wantMsg {
		m := map[string]wantMsg{}
		for _, w := range wants {
			m[w.Addr] = w
		}
		return m
	}
	gotOf := func() []sentMsg {
		var out []sentMsg
		for i, w := range wants {
			out = append(out, sentMsg{Seq: int64(i + 1), Addr: w.Addr, Msg: gotMsg{Subject: w.Subject, HTML: crlf(w.HTML), Text: crlf(w.Text), Unsub: []string{"<" + w.Link + ">"}, UnsubPost: []string{oneClickValue}}})
		}
		return out
	}
	storedOf := func() map[string]storedItem {
		m := map[string]storedItem{}
		for _, w := range wants {
			m[w.ID] = storedItem{Subject: w.Subject, HTML: w.HTML, Text: w.Text, Link: w.Link}
		}
		return m
	}
	if len(renderedBytesEqual(wantOf(), gotOf())) != 0 || len(unsubscribeHeadersOK(wantOf(), gotOf())) != 0 || len(storedEqualsPure(wantOf(), storedOf())) != 0 {
		return []string{fmt.Sprintf("the render checks found a problem in the GOOD fixture: %s %s %s", renderedBytesEqual(wantOf(), gotOf()), unsubscribeHeadersOK(wantOf(), gotOf()), storedEqualsPure(wantOf(), storedOf()))}
	}
	expect := func(what, why string, ps []string) {
		if !mentions(ps, why) {
			failed = append(failed, fmt.Sprintf("the corruption %s was NOT found for its reason %q (the check said: %s)", what, why, strings.Join(ps, "; ")))
		}
	}
	// a byte of the expected value flipped: the part that holds it is named
	for _, part := range []string{"subject", "html", "text"} {
		w := wantOf()
		a := w[wants[0].Addr]
		switch part {
		case "subject":
			a.Subject = flipByte(a.Subject)
		case "html":
			a.HTML = flipByte(a.HTML)
		default:
			a.Text = flipByte(a.Text)
		}
		w[a.Addr] = a
		expect("one byte flipped in the expected "+part, "the "+part+" differs", renderedBytesEqual(w, gotOf()))
	}
	swapped := gotOf()
	swapped[0].Msg, swapped[1].Msg = swapped[1].Msg, swapped[0].Msg
	expect("two recipients' messages exchanged (each one the render of the other's row)", "messages that differ from the pure render of their row", renderedBytesEqual(wantOf(), swapped))
	expect("no message at all (the comparison of an empty set)", "no rendered message was compared", renderedBytesEqual(wantOf(), nil))
	unread := gotOf()
	unread[1].Err, unread[1].Msg = "it does not parse as a mail message", gotMsg{}
	expect("a message that cannot be read", "cannot read", renderedBytesEqual(wantOf(), unread))
	stray := append(gotOf(), sentMsg{Seq: 9, Addr: "sj-ctl-99@example.test", Msg: gotMsg{Subject: "x", HTML: "x", Text: "x"}})
	expect("a message to a recipient that is no pending row", "no pending row", renderedBytesEqual(wantOf(), stray))
	// the spin: a renderer that ignores the seed gives every recipient the same message (its pure render is then the same for all, so only "not all equal" can say so)
	same, sameGot := wantOf(), gotOf()
	for i, w := range wants {
		a := same[w.Addr]
		a.Subject, a.HTML, a.Text = wants[0].Subject, wants[0].HTML, wants[0].Text
		same[w.Addr] = a
		sameGot[i].Msg.Subject, sameGot[i].Msg.HTML, sameGot[i].Msg.Text = wants[0].Subject, crlf(wants[0].HTML), crlf(wants[0].Text)
	}
	expect("every recipient given the byte-identical message (a renderer that ignores the seed)", "byte-identical", renderedBytesEqual(same, sameGot))
	sameSpin := wantOf()
	for addr, w := range sameSpin {
		w.Spin = "one-spin"
		sameSpin[addr] = w
	}
	expect("the pure renders of three recipients making the same spin choices", "the same spin choices", renderedBytesEqual(sameSpin, gotOf()))
	two, twoGot := same, sameGot[:2]
	delete(two, wants[2].Addr)
	if ps := renderedBytesEqual(two, twoGot); len(ps) != 0 {
		failed = append(failed, "the spin is asserted from "+fmt.Sprint(minSpinSample)+" messages on, and two identical messages were refused: "+strings.Join(ps, "; "))
	}
	// the link and its headers
	noHdr := gotOf()
	noHdr[0].Msg.Unsub = nil
	expect("a message with no List-Unsubscribe header", "carries 0 List-Unsubscribe header(s)", unsubscribeHeadersOK(wantOf(), noHdr))
	twiceHdr := gotOf()
	twiceHdr[0].Msg.Unsub = append(twiceHdr[0].Msg.Unsub, twiceHdr[0].Msg.Unsub[0])
	expect("a message with two List-Unsubscribe headers", "carries 2 List-Unsubscribe header(s)", unsubscribeHeadersOK(wantOf(), twiceHdr))
	other := gotOf()
	other[0].Msg.Unsub = []string{"<" + wants[1].Link + ">"}
	expect("a message that carries another recipient's link", "its List-Unsubscribe is", unsubscribeHeadersOK(wantOf(), other))
	noPost := gotOf()
	noPost[2].Msg.UnsubPost = nil
	expect("a message with no List-Unsubscribe-Post header", "its List-Unsubscribe-Post is", unsubscribeHeadersOK(wantOf(), noPost))
	badPost := gotOf()
	badPost[2].Msg.UnsubPost = []string{"List-Unsubscribe=No"}
	expect("a List-Unsubscribe-Post that is not one-click", "its List-Unsubscribe-Post is", unsubscribeHeadersOK(wantOf(), badPost))
	expect("no message at all (the headers of an empty set)", "no List-Unsubscribe header was read", unsubscribeHeadersOK(wantOf(), nil))
	// the module-facing rows
	for _, part := range []string{"subject", "html", "text"} {
		st := storedOf()
		s := st[wants[1].ID]
		switch part {
		case "subject":
			s.Subject = flipByte(s.Subject)
		case "html":
			s.HTML = flipByte(s.HTML)
		default:
			s.Text = flipByte(s.Text)
		}
		st[wants[1].ID] = s
		expect("one byte flipped in the stored "+part, "the stored "+part+" differs", storedEqualsPure(wantOf(), st))
	}
	st := storedOf()
	s := st[wants[0].ID]
	s.Link = linkOf(9)
	st[wants[0].ID] = s
	expect("a stored link that is not the item's own", "the stored unsubscribe_url", storedEqualsPure(wantOf(), st))
	st = storedOf()
	delete(st, wants[2].ID)
	expect("a pending row with no module-facing row", "no public.job_items row", storedEqualsPure(wantOf(), st))
	// the seed contract itself: the HTML and the text share one stream, the subject has its own
	seed := render.SpinSeed("00000000-0000-4000-8000-0000000000aa", wants[0].ID)
	if render.PartSeed(seed, render.PartHTML) != render.PartSeed(seed, render.PartText) || render.PartSeed(seed, render.PartSubject) == render.PartSeed(seed, render.PartHTML) {
		failed = append(failed, "render.PartSeed: the HTML and the text must share one stream and the subject must have its own")
	}
	if render.SpinSeed("00000000-0000-4000-8000-0000000000aa", wants[0].ID) == render.SpinSeed("00000000-0000-4000-8000-0000000000ab", wants[0].ID) || render.SpinSeed("00000000-0000-4000-8000-0000000000aa", wants[0].ID) == render.SpinSeed("00000000-0000-4000-8000-0000000000aa", wants[1].ID) {
		failed = append(failed, "render.SpinSeed gives two jobs, or two items of one job, the same seed")
	}
	return failed
}
