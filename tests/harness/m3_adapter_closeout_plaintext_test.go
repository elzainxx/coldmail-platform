//go:build harness

package harness

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestM3Adapter_PlainTextPartPassesThroughTheSharedTable (Phase 3 close-out C5 / D5): the platform writes ONE MORE nullable column of the shared table, public.job_items.rendered_text; module 3 freezes it with the
// rest of the row when it accepts the run (its run statement) and its REAL worker builds multipart/alternative when, and only when, the text is a real one. This is the module 3 twin of
// TestM2Worker_PlainTextPartPassesThroughTheSharedTable and proves the whole path on the wire: five rows are seeded by the platform's own INSERT shape (rendered_text set afterwards by an UPDATE, as the
// platform's writer will), run through the ADAPTER (Run: the adapter has nothing to say about the text, which is the point: it is a column, not a field of the request), sent by module 3's worker to the
// controllable sink, and the RAW bytes the sink accepted are read back (GET /api/message, the sink every adapter test of module 3 names as the account's SMTP target) and parsed with net/mail and
// mime/multipart. The paired controls are inside the campaign: a row with no text (NULL, as every row the platform wrote before the column) and a row with a white-space-only text must arrive as the single
// text/html part module 3 has always sent, and the three rows with a text arrive as multipart/alternative, text/plain FIRST and text/html LAST, each with its charset:
//
//	1  a plain ASCII text      2  NULL      3  white space only      4  non-ASCII letters (utf-8, 8bit)      5  a line that starts with a dot (dot-stuffing on the wire, undone by the sink) and a very long line
//
// Two messages never share a boundary (it is derived from the job, not a constant).
func TestM3Adapter_PlainTextPartPassesThroughTheSharedTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	tn := e.seedTenant(t, "ptxt", m3cKinds("ok", 5))
	e.createVerified(t, tn, "1", e.sinkIP)
	e.setPacing(t, tn, 3, 0, 1)

	texts := map[int]string{
		1: "Hello Ada,\r\nthis is the plain-text alternative.\r\n",
		3: " \t\r\n  ",
		4: "Grüße aus Köln, naïve café ✓\r\n",
		5: ".starts with a dot\r\n" + strings.Repeat("a long line ", 40) + "\r\n",
	}
	for idx, txt := range texts {
		it := tn.Items[idx-1]
		if _, err := e.db.Exec(ctx, "UPDATE public.job_items SET rendered_text = $2 WHERE id = $1::uuid", it.ID, txt); err != nil {
			t.Fatalf("set rendered_text of item %d: %v (does public.job_items have the column? the platform's migration adds it)", idx, err)
		}
	}
	var withNull int
	if err := e.db.QueryRow(ctx, "SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND rendered_text IS NULL", tn.CampaignID).Scan(&withNull); err != nil || withNull != 1 {
		t.Fatalf("the campaign has %d row(s) with a NULL rendered_text (error %v), want exactly 1: the control of this test", withNull, err)
	}

	e.runAll(t, tn)
	rep := e.waitCampaign(t, tn, 180*time.Second, nil)
	if rep.Totals.Total != 5 || rep.Totals.Sent != 5 {
		t.Fatalf("the campaign's totals are (%s), want 5 sent: every message of this test must reach the sink", m3aTotalsLine(rep.Totals))
	}

	v := e.sinkView(t)
	e.requireNoDups(t, v)
	parsed := map[int]m3cMessage{}
	for i, it := range tn.Items {
		var seqs []int64
		for _, m := range v.Msgs {
			for _, to := range m.To {
				if strings.EqualFold(strings.TrimSpace(to), it.Email) {
					seqs = append(seqs, m.Seq)
				}
			}
		}
		if len(seqs) != 1 {
			t.Fatalf("the sink accepted %d message(s) to item %d (%s), want exactly 1", len(seqs), i+1, it.Email)
		}
		raw := e.ctl(t, http.MethodGet, fmt.Sprintf("/api/message?seq=%d", seqs[0]), nil, http.StatusOK)
		parsed[i+1] = m3cParse(t, raw)
	}

	// the controls: a NULL and a white-space-only text are the single text/html part, the HTML intact
	for _, idx := range []int{2, 3} {
		p := parsed[idx]
		if p.ctype != "text/html" || len(p.parts) != 0 {
			t.Errorf("item %d (no real text): Content-Type %s with %d part(s), want the single text/html part module 3 has always sent", idx, p.ctype, len(p.parts))
		}
		if !strings.Contains(p.body, e.runID) || !strings.Contains(p.body, "m3adapter") {
			t.Errorf("item %d (no real text): the body does not carry the platform's HTML (run %s): %.200q", idx, e.runID, p.body)
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
		if !strings.Contains(last.body, e.runID) || !strings.Contains(last.body, "m3adapter") {
			t.Errorf("item %d: the text/html part does not carry the platform's HTML (run %s): %.200q", idx, e.runID, last.body)
		}
	}
	// two messages never share a boundary (a boundary derived from the job, not a constant)
	if parsed[1].params["boundary"] == parsed[4].params["boundary"] || parsed[1].params["boundary"] == parsed[5].params["boundary"] || parsed[4].params["boundary"] == parsed[5].params["boundary"] {
		t.Errorf("two different messages carry the same multipart boundary %q: it must be derived per job", parsed[1].params["boundary"])
	}
	t.Logf("NOTE m3a plain-text part: items 1, 4 and 5 arrived as multipart/alternative (text/plain first), items 2 (NULL) and 3 (white space only) as the single text/html part; boundaries %q, %q, %q",
		parsed[1].params["boundary"], parsed[4].params["boundary"], parsed[5].params["boundary"])
}
