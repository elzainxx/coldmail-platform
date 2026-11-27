package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/coldmail-platform/platform/internal/render"
)

// THE TEMPLATE RENDERER (Phase 5, chunk S2; the language is internal/render, the page for customers is Docs/templates.md). A send job's campaign is three templates (subject, HTML, text); this file is what
// the store does with them:
//
//   - Preparer / PreparedCampaign: the interface the store PREFERS to Renderer when it has one (WithPreparer). A Renderer is asked per row and does not know the job; a Preparer is asked once per job
//     per call, compiles the three templates once and then answers for every row, which is what a page of 5000 rows needs and what the seed (job id and row id) needs. Renderer, PlainRenderer and
//     every caller of them are unchanged: a store with only a Renderer does no template work at all (no syntax check, no missing fields, no samples, no skipped rows).
//   - TemplateRenderer: the Preparer over internal/render.
//
// WHAT A ROW RENDERS TO. The values of a row are, in this order of precedence: the two reserved names (`email` is the row's normalised address, `unsubscribe_url` is the link the store made for the row),
// then the row's merge data (a column named like a reserved field NEVER overrides it). The seed is render.SpinSeed(job id, row id): the same job and row render the same bytes, whoever renders them and
// whenever (no clock, no randomness, no map order: a test renders twice, with two renderers, and after a rolled-back EnsureJobItems). The text part is the explicit text template rendered, or, when the text
// template is blank, render.HTMLToText of the rendered HTML (so every message has a text part). THE UNSUBSCRIBE FOOTER, PER RENDERED PART: each part a recipient can read must carry the link, and
// that is judged on the RENDERED part, never on the template. The HTML gets the footer unless render.HTMLToText of the rendered HTML (which keeps a link as `text (url)` and drops comments, tags, attributes,
// scripts and styles: it is the test of what a recipient sees) already holds the link; an explicit text gets its footer line unless the rendered text holds it; a generated text derives from the final HTML,
// so it carries the link exactly when the HTML does and is never footed twice. After the footer is added the HTML is tested again: a message that STILL shows no link (an unclosed comment or tag swallowed
// the footer) is refused as WordNoVisibleLink and left out at confirm, free, never sent without one. The subject never gets a footer and decides nothing. To make the placement unconditional
// {{unsubscribe_url}} is refused at save inside a spintax option (checkLinkPlacement).

// Preparer is the optional second face of a store's renderer: it prepares a job's campaign once (compiling the templates, which is where a syntax error is found) and answers for each row.
// jobID is "" when the campaign is only being checked (a draft being created: the job has no id yet), and then Render must not be called.
type Preparer interface {
	Prepare(jobID string, c Content) (PreparedCampaign, error)
}

// PreparedCampaign is a campaign ready to be rendered for the rows of one job. It is immutable and safe for concurrent use.
type PreparedCampaign interface {
	// Missing names the merge fields the campaign needs for this row that the row does not have and that carry no fallback, sorted, without repeats; empty when the row can be rendered.
	Missing(row EngineRow) []string
	// Render is the row's message: subject, HTML and text, exactly what is sent. The row carries its own unsubscribe link (EngineRow.UnsubscribeURL).
	Render(row EngineRow) (subject, html, text string, err error)
}

// The fixed wording of the unsubscribe footer (documented in Docs/templates.md). The link is the row's own, inserted after FooterTextLead in a text and between FooterHTMLLead and FooterHTMLTrail in HTML
// (HTML-escaped there).
const (
	FooterTextLead  = "To stop receiving these messages, unsubscribe: "
	FooterHTMLLead  = `<p>To stop receiving these messages, <a href="`
	FooterHTMLTrail = `">unsubscribe</a>.</p>`
)

// The parts of a campaign a template error names.
const (
	TemplateSubject = "subject"
	TemplateHTML    = "html"
	TemplateText    = "text"
)

// TemplateRenderer is the Preparer of Phase 5: merge fields, spintax and the generated text part. The zero value is ready.
type TemplateRenderer struct{}

// Prepare compiles the subject, the HTML and (when it is not blank) the text template, in that order; the first syntax error is returned as a *TemplateError.
func (TemplateRenderer) Prepare(jobID string, c Content) (PreparedCampaign, error) {
	p := &templates{jobID: jobID}
	var err error
	if p.subject, err = compilePart(TemplateSubject, c.Subject); err != nil {
		return nil, err
	}
	if p.html, err = compilePart(TemplateHTML, c.HTML); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.Text) != "" { // a blank text template means "make the text from the HTML"
		if p.text, err = compilePart(TemplateText, c.Text); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func compilePart(part, src string) (*render.Template, error) {
	t, err := render.Compile(src)
	if err != nil {
		var se *render.SyntaxError
		if errors.As(err, &se) {
			return nil, &TemplateError{Part: part, Line: se.Line, Column: se.Column, Message: se.Message}
		}
		return nil, fmt.Errorf("the %s template could not be compiled", part) // Compile returns nothing else: not reachable, and never quotes the source
	}
	if off := linkInGroup(src); off >= 0 { // the source is valid: its braces balance and its escapes are known
		line, col := position(src, off)
		return nil, &TemplateError{Part: part, Line: line, Column: col, Message: "{{unsubscribe_url}} cannot be inside a group {a|b}: the link must always be there, so put it outside the braces"}
	}
	return t, nil
}

// linkInGroup is the byte offset of the first {{unsubscribe_url}} (with or without a fallback) that stands inside a spintax option of a source that compiled, -1 when there is none. A field in an option is
// there for the recipients whose draw picks that option and not for the others, which would leave some recipients with no link in the message; the placement must be unconditional. It reads the source with
// the grammar's own rules (a backslash makes the next { } | or  plain; {{ opens a field that ends at }}; a single { opens a group and a } closes the innermost), and it lives here because internal/render does not
// say where a field is.
func linkInGroup(src string) int {
	depth := 0
	for i := 0; i < len(src); {
		switch src[i] {
		case '\\':
			if i+1 < len(src) && (src[i+1] == '{' || src[i+1] == '}' || src[i+1] == '|' || src[i+1] == '\\') {
				i += 2
			} else {
				i++
			}
		case '{':
			if i+1 >= len(src) || src[i+1] != '{' { // a group
				depth++
				i++
				continue
			}
			start, k := i, i+2
			for k < len(src) && (src[k] == '_' || src[k] >= 'a' && src[k] <= 'z' || src[k] >= 'A' && src[k] <= 'Z' || src[k] >= '0' && src[k] <= '9') {
				k++
			}
			if depth > 0 && src[i+2:k] == render.FieldUnsubscribeURL {
				return start
			}
			for i = k; i < len(src); { // to the }} that closes the field; a fallback is plain text whose escapes are skipped
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
				} else if src[i] == '}' {
					i += 2
					break
				} else {
					i++
				}
			}
		case '}':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return -1
}

// position is the 1-based line and column (in characters) of a byte offset of src, counted as internal/render counts them in a SyntaxError.
func position(src string, off int) (line, col int) {
	head := src[:off]
	start := strings.LastIndexByte(head, '\n') + 1
	return 1 + strings.Count(head, "\n"), 1 + utf8.RuneCountInString(head[start:])
}

// templates is the compiled campaign of one job.
type templates struct {
	jobID               string
	subject, html, text *render.Template // text is nil when the text template is blank
}

// values is the lookup of one row: the reserved names first, then the row's merge data.
func (p *templates) values(row EngineRow) func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch name {
		case render.FieldEmail:
			return row.EmailNorm, true
		case render.FieldUnsubscribeURL:
			return row.UnsubscribeURL, true
		}
		v, ok := row.Fields[name]
		return v, ok
	}
}

// Missing is the union, sorted, of what each of the three templates lacks for the row.
func (p *templates) Missing(row EngineRow) []string {
	vals := p.values(row)
	var set map[string]struct{}
	for _, t := range []*render.Template{p.subject, p.html, p.text} {
		for _, n := range t.Missing(vals) {
			if set == nil {
				set = map[string]struct{}{}
			}
			set[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Render renders the row. A field the row lacks renders its fallback or nothing (the caller leaves such a row out beforehand, with Missing) and the shared table's own limits are judged by the caller
// (checkRendered). The one refusal Render makes itself is WordNoVisibleLink.
//
// THE FOOTER IS DECIDED ON WHAT WAS RENDERED, never on the template (a link placed in a comment, in an <img src> or in a script is in the template and not on the screen). With a link to place:
//   - HTML (when the rendered HTML is not blank): if render.HTMLToText of it does not hold the link, the footer is added, before the last </body> or, when that does not make the link visible (the </body> is
//     itself inside a comment), at the end. If the HTML still shows no link, the row has no way to unsubscribe in its HTML (an unclosed comment, tag, script or style swallowed the footer): refused, not sent.
//   - an explicit text (when the rendered text is not blank): if it does not hold the link, the footer line is added (after which it does).
//   - a generated text is HTMLToText of the FINAL html, so it holds the link exactly when the html does, once.
//
// With no link (a store with no maker: no send job can be confirmed there) nothing is added and nothing is refused. The same row always gives the same bytes: every decision is a pure function of the
// rendered text.
func (p *templates) Render(row EngineRow) (string, string, string, error) {
	seed := render.SpinSeed(p.jobID, row.ID)
	vals := p.values(row)
	subject := p.subject.Render(seed, render.PartSubject, vals)
	body := p.html.Render(seed, render.PartHTML, vals)
	link := row.UnsubscribeURL
	htmlSent := strings.TrimSpace(body) != "" // a text-only campaign has no HTML part to add a footer to or to show a link in
	seen := ""                                // render.HTMLToText of the final html, once it has been made
	if htmlSent && (p.text == nil || link != "") {
		seen = render.HTMLToText(body)
		if link != "" && !strings.Contains(seen, link) {
			shown := false
			for _, cand := range footerCandidates(body, link) {
				if t := render.HTMLToText(cand); strings.Contains(t, link) {
					body, seen, shown = cand, t, true
					break
				}
			}
			if !shown {
				return "", "", "", refuse(WordNoVisibleLink, "row %d: the message has no unsubscribe link a recipient can see", row.Ordinal)
			}
		}
	}
	var text string
	if p.text != nil {
		text = p.text.Render(seed, render.PartText, vals)
		if link != "" && strings.TrimSpace(text) != "" && !strings.Contains(text, link) {
			text = strings.TrimRight(text, " \t\r\n") + "\n\n" + FooterTextLead + link
		}
	} else if htmlSent {
		text = seen // from the FINAL html: the text has the link exactly when the html does (its own, or the footer's), once
	}
	return subject, body, text, nil
}

// footerCandidates are the ways to add the footer paragraph to the HTML, in the order they are tried: before the last </body> (compared without regard to ASCII case) when there is one, and at the end.
func footerCandidates(body, link string) []string {
	footer := FooterHTMLLead + html.EscapeString(link) + FooterHTMLTrail
	var out []string
	if i := lastBodyClose(body); i >= 0 {
		out = append(out, body[:i]+footer+"\n"+body[i:])
	}
	return append(out, body+"\n"+footer)
}

// withHTMLFooter adds the footer paragraph before the last </body> or, when there is none, at the end.
func withHTMLFooter(body, link string) string { return footerCandidates(body, link)[0] }

// lastBodyClose is the byte index of the last "</body>" of s, -1 when there is none. It compares bytes (ASCII fold), not a lower-cased copy: lower-casing can change the length of non-ASCII text.
func lastBodyClose(s string) int {
	const want = "</body>"
	for i := len(s) - len(want); i >= 0; i-- {
		if s[i] != '<' {
			continue
		}
		ok := true
		for j := 1; j < len(want); j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != want[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// plainCampaign is the prepared form of a store that has only a Renderer: every row goes to the Renderer as it always did, and no row lacks anything.
type plainCampaign struct {
	r Renderer
	c Content
}

func (p plainCampaign) Missing(EngineRow) []string { return nil }

func (p plainCampaign) Render(row EngineRow) (string, string, string, error) {
	return p.r.Render(p.c, row)
}

// prepare is the store's campaign for a job: the Preparer's when the store has one, the Renderer's otherwise.
func (s *Store) prepare(jobID string, c Content) (PreparedCampaign, error) {
	if s.prep != nil {
		return s.prep.Prepare(jobID, c)
	}
	return plainCampaign{r: s.render, c: c}, nil
}

// checkTemplates is the syntax check of a campaign at save: a store with a Preparer compiles the subject, the HTML and the text and refuses the first mistake (a *TemplateError, a 400 that carries the
// part, the line, the column and the message); a store with only a Renderer checks nothing, because it reads no syntax. Every path that gives a send job its content passes through here.
func (s *Store) checkTemplates(c *Content) error {
	if s.prep == nil || c == nil {
		return nil
	}
	_, err := s.prep.Prepare("", *c)
	return err
}

// ---- the merge data of a row --------------------------------------------------------------------------------------------------------------------------------------------------------------

// decodeFields reads a row's merge data (platform.job_rows.fields, a JSON object) into the strings the renderer takes. The store writes strings only; a number or a boolean that any other writer put there
// is coerced to the text it is written as (a JSON number as its digits, true and false as those words), and anything else (null, an array, an object) is MISSING: it is left out, so the field is
// absent for the row. A value is never interpreted: whatever the text is, the renderer inserts it as data.
func decodeFields(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case json.Number:
			out[k] = x.String()
		case bool:
			out[k] = strconv.FormatBool(x)
		}
	}
	return out, nil
}

// maxReasonNameBytes bounds the reason of a missing_field row: the names of the fields, joined by commas, and a count of the rest.
const maxReasonNameBytes = 400

// missingReason is the reason of a missing_field row: the sorted field names joined by a comma, or, when they do not fit in maxReasonNameBytes, the first that do and ",+N more". It names fields only.
func missingReason(names []string) string {
	var b strings.Builder
	for i, n := range names {
		if b.Len()+len(n)+1 > maxReasonNameBytes {
			fmt.Fprintf(&b, ",+%d more", len(names)-i)
			return b.String()
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
	}
	return b.String()
}

// TemplatesEnabled says whether the store renders through a Preparer (WithPreparer): the service's production wiring (internal/app) must, and a test proves it does.
func (s *Store) TemplatesEnabled() bool { return s.prep != nil }
