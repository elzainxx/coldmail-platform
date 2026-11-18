//go:build harness && m2worker

package harness

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// The plain-text part (Phase 3 close-out B3 / D5): the platform writes ONE MORE nullable column of the shared table, public.job_items.rendered_text, module 2 freezes it with the rest of the row
// (statement J2) and its REAL worker builds multipart/alternative when, and only when, the text is a real one. This test proves the whole path on the wire: rows are seeded by the platform's own
// INSERT shape (rendered_text set afterwards by an UPDATE, as the platform's writer will), run through the adapter, sent by the real worker to the controllable sink, and the RAW bytes the sink
// accepted are read back (GET /api/message) and parsed. The paired controls are inside it: the same campaign carries a row with no text, a row with a whitespace-only text (both must be the single
// text/html part they have always been) and rows with a text; a difference between the four is the property, not an accident.

// m2pParsed is one accepted message, parsed.
type m2pParsed struct {
	ctype  string
	params map[string]string
	parts  []m2pPart // multipart/alternative: its parts in order; otherwise empty
	body   string    // a single-part message: its decoded body
}

type m2pPart struct{ ctype, charset, cte, body string }

func m2pDecode(cte string, raw []byte) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		b, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		return string(b), err
	case "base64":
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		return string(b), err
	}
	return string(raw), nil // 7bit, 8bit, binary or none: the bytes are the text
}

func m2pParse(t *testing.T, raw []byte) m2pParsed {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the raw message does not parse as a mail message (%v):\n%.600s", err, raw)
	}
	ct := msg.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("the message has no readable Content-Type %q (%v)", ct, err)
	}
	out := m2pParsed{ctype: mt, params: params}
	if !strings.HasPrefix(mt, "multipart/") {
		body, _ := io.ReadAll(msg.Body)
		out.body, err = m2pDecode(msg.Header.Get("Content-Transfer-Encoding"), body)
		if err != nil {
			t.Fatalf("the single-part body does not decode under its Content-Transfer-Encoding: %v", err)
		}
		return out
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a part of the multipart message does not parse: %v", err)
		}
		pt, pp, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		b, _ := io.ReadAll(p)
		dec, err := m2pDecode(p.Header.Get("Content-Transfer-Encoding"), b)
		if err != nil {
			t.Fatalf("a part does not decode under its Content-Transfer-Encoding: %v", err)
		}
		out.parts = append(out.parts, m2pPart{ctype: pt, charset: strings.ToLower(pp["charset"]), cte: p.Header.Get("Content-Transfer-Encoding"), body: dec})
	}
	return out
}

func TestM2Worker_PlainTextPartPassesThroughTheSharedTable(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	e := newM2eEnv(t, r)
	ctx := r.ctx

	tn := r.newTenant(t, "ptxt", []string{"ok", "ok", "ok", "ok", "ok"}, true)
	ad := e.client(t, tn)
	r.setMode(t, m2wMode{})

	// row 1: a text (ASCII); row 2: no text (NULL, as every row the platform wrote before this column); row 3: white space only; row 4: a text with non-ASCII letters; row 5: a text with a line
	// that starts with a dot and one very long line (the wire's two classic traps)
	texts := map[int]string{
		1: "Hello Ada,\r\nthis is the plain-text alternative.\r\n",
		3: " \t\r\n  ",
		4: "Grüße aus Köln, naïve café ✓\r\n",
		5: ".starts with a dot\r\n" + strings.Repeat("a long line ", 40) + "\r\n",
	}
	for idx, txt := range texts {
		it := tn.Items[idx-1]
		if _, err := r.db.Exec(ctx, "UPDATE public.job_items SET rendered_text = $2 WHERE id = $1::uuid", it.ID, txt); err != nil {
			t.Fatalf("set rendered_text of item %d: %v", idx, err)
		}
	}
	var withNull int
	if err := r.db.QueryRow(ctx, "SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND rendered_text IS NULL", tn.CampaignID).Scan(&withNull); err != nil || withNull != 1 {
		t.Fatalf("the campaign has %d row(s) with a NULL rendered_text (err %v), want exactly 1: the control of this test", withNull, err)
	}

	e.runAll(t, ad, tn)
	rep := e.waitReport(t, ad, tn, 120*time.Second, nil)
	if rep.Totals.Total != 5 || rep.Totals.Sent != 5 {
		t.Fatalf("the campaign's totals are (%s), want 5 sent: every message of this test must reach the sink", m2eTotalsLine(rep.Totals))
	}

	v := r.sinkView(t)
	parsed := map[int]m2pParsed{}
	for i, it := range tn.Items {
		msgs := v.Msgs[it.Email]
		if len(msgs) != 1 {
			t.Fatalf("the sink accepted %d message(s) to item %d (%s), want exactly 1", len(msgs), i+1, it.Email)
		}
		raw := r.ctlCall(t, http.MethodGet, fmt.Sprintf("/api/message?seq=%d", msgs[0].Seq), nil, http.StatusOK)
		parsed[i+1] = m2pParse(t, raw)
	}

	html := func(idx int) string { return fmt.Sprintf("<p>m2w %s %s %02d</p>", r.runID, tn.Name, idx) }
	// the controls: a NULL and a white-space-only text are the single text/html part, the HTML intact
	for _, idx := range []int{2, 3} {
		p := parsed[idx]
		if p.ctype != "text/html" || len(p.parts) != 0 {
			t.Errorf("item %d (no real text): Content-Type %s with %d part(s), want the single text/html part module 2 has always sent", idx, p.ctype, len(p.parts))
		}
		if !strings.Contains(p.body, html(idx)) {
			t.Errorf("item %d (no real text): the body does not carry the HTML %q: %.200q", idx, html(idx), p.body)
		}
	}
	// the property: a real text makes multipart/alternative, text/plain FIRST, text/html LAST, each with its charset, carrying exactly the text and the HTML
	for _, idx := range []int{1, 4, 5} {
		p := parsed[idx]
		if p.ctype != "multipart/alternative" || len(p.parts) != 2 || p.params["boundary"] == "" {
			t.Errorf("item %d (a real text): Content-Type %s with %d part(s), want multipart/alternative with 2 parts and a boundary", idx, p.ctype, len(p.parts))
			continue
		}
		first, last := p.parts[0], p.parts[1]
		if first.ctype != "text/plain" || last.ctype != "text/html" {
			t.Errorf("item %d: the parts are %s then %s, want text/plain FIRST and text/html LAST (RFC 2046: least to most faithful)", idx, first.ctype, last.ctype)
		}
		if first.charset != "utf-8" || last.charset != "utf-8" {
			t.Errorf("item %d: the parts' charsets are %q and %q, want utf-8 on both", idx, first.charset, last.charset)
		}
		if norm := strings.ReplaceAll(first.body, "\r\n", "\n"); strings.TrimRight(norm, "\n") != strings.TrimRight(strings.ReplaceAll(texts[idx], "\r\n", "\n"), "\n") {
			t.Errorf("item %d: the text/plain part is %.120q, want the text %.120q", idx, first.body, texts[idx])
		}
		if !strings.Contains(last.body, html(idx)) {
			t.Errorf("item %d: the text/html part does not carry the HTML %q: %.200q", idx, html(idx), last.body)
		}
	}
	// two messages never share a boundary (a boundary derived from the job, not a constant)
	if parsed[1].params["boundary"] == parsed[4].params["boundary"] || parsed[1].params["boundary"] == parsed[5].params["boundary"] {
		t.Errorf("two different messages carry the same multipart boundary %q: it must be derived per job", parsed[1].params["boundary"])
	}
	t.Logf("NOTE plain-text part: items 1, 4 and 5 arrived as multipart/alternative (text/plain first), items 2 (NULL) and 3 (white space only) as the single text/html part; boundaries %q, %q, %q",
		parsed[1].params["boundary"], parsed[4].params["boundary"], parsed[5].params["boundary"])
}
