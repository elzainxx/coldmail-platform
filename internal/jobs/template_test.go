package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/money"
)

// The pure half of Phase 5 S2 (template.go, template_scan.go, render.go): what a row renders to, what it lacks, the footer, the words of a refusal, the picks of a preview, the arithmetic of a preview
// with templates and the fingerprint of merge data. The statements (the pass over the rows, the confirm that leaves rows out, the stored preview) are proved against a real Postgres by
// tests/harness/jobs_templates_test.go.

const (
	tplJob   = "11111111-1111-4111-8111-111111111111"
	tplJob2  = "22222222-2222-4222-8222-222222222222"
	tplLink  = "https://app.example.test/u/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tplLink2 = "https://app.example.test/u/BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func rowID(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func tplRow(i int, fields map[string]string) EngineRow {
	return EngineRow{ID: rowID(i), Ordinal: i, EmailNorm: fmt.Sprintf("r%05d@example.test", i), Fields: fields, UnsubscribeURL: tplLink}
}

func prepareT(t *testing.T, jobID string, c Content) PreparedCampaign {
	t.Helper()
	p, err := TemplateRenderer{}.Prepare(jobID, c)
	if err != nil {
		t.Fatalf("Prepare(%+v): %v", c, err)
	}
	return p
}

func renderT(t *testing.T, p PreparedCampaign, row EngineRow) (subject, html, text string) {
	t.Helper()
	s, h, x, err := p.Render(row)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, x
}

// ---- determinism ----------------------------------------------------------------------------------------------------------------------------------------------------------------------

// The same job and row give the same bytes whichever instance renders them and however often; another row of the job, or the same row of another job, spins differently (a group of eight options four
// times over: two draws agree one time in 4096, so 60 draws that all agree would be a broken seed). The control inside each assertion is the other half of the pair.
func TestTemplateRenderer_SameJobAndRowSameBytesAnotherRowOrJobAnotherSpin(t *testing.T) {
	g := "{a|b|c|d|e|f|g|h}"
	c := Content{Subject: "S " + g + g + g + g, HTML: "<p>" + g + g + g + g + " {{first|x}}</p>", Text: g + g + g + g}
	first := prepareT(t, tplJob, c)
	second := prepareT(t, tplJob, c) // a second renderer instance for the same job
	rowA := tplRow(1, map[string]string{"first": "Ann"})
	s1, h1, x1 := renderT(t, first, rowA)
	for i := 0; i < 5; i++ {
		for name, p := range map[string]PreparedCampaign{"the same instance": first, "another instance": second} {
			s2, h2, x2 := renderT(t, p, rowA)
			if s1 != s2 || h1 != h2 || x1 != x2 {
				t.Fatalf("%s, render %d: %q %q %q differ from the first %q %q %q", name, i, s2, h2, x2, s1, h1, x1)
			}
		}
	}
	distinctRows, sameAcrossJobs := map[string]bool{}, 0
	other := prepareT(t, tplJob2, c)
	for i := 1; i <= 60; i++ {
		row := tplRow(i, map[string]string{"first": "Ann"})
		a, _, _ := renderT(t, first, row)
		distinctRows[a] = true
		b, _, _ := renderT(t, other, row)
		if a == b {
			sameAcrossJobs++
		}
	}
	if len(distinctRows) < 30 {
		t.Errorf("60 rows of one job gave only %d distinct subjects: the row id is not in the seed", len(distinctRows))
	}
	if sameAcrossJobs > 4 {
		t.Errorf("%d of 60 rows spun the same in two jobs: the job id is not in the seed", sameAcrossJobs)
	}
	// the control of the control: the same job twice agrees on every one of the 60
	for i := 1; i <= 60; i++ {
		row := tplRow(i, map[string]string{"first": "Ann"})
		a, ha, xa := renderT(t, first, row)
		b, hb, xb := renderT(t, second, row)
		if a != b || ha != hb || xa != xb {
			t.Fatalf("row %d: two instances of one job disagree", i)
		}
	}
}

// ---- values are data ------------------------------------------------------------------------------------------------------------------------------------------------------------------

// A value holding the language's own characters, a script, or line breaks ends up literal, escaped where it must be, and never on two lines in the subject. The control is the same text WRITTEN IN THE
// TEMPLATE, which is interpreted.
func TestTemplateRenderer_ValuesAreLiteralEscapedAndOneLineInTheSubject(t *testing.T) {
	evil := "{{x}} {a|b} \\{ <script>alert(1)</script> \"q\" 'q' &\r\nBcc: victim@example.test"
	p := prepareT(t, tplJob, Content{Subject: "Hi {{name}}", HTML: "<p>{{name}}</p>", Text: "{{name}}"})
	subject, html, text := renderT(t, p, tplRow(1, map[string]string{"name": evil}))
	if strings.ContainsAny(subject, "\r\n") {
		t.Errorf("the subject holds a line break: %q", subject)
	}
	if !strings.Contains(subject, "{{x}} {a|b} \\{ <script>") {
		t.Errorf("the subject lost the literal text of the value: %q", subject)
	}
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(html, "&#34;q&#34;") && !strings.Contains(html, "&quot;q&quot;") {
		t.Errorf("the html did not escape the value: %q", html)
	}
	if !strings.Contains(html, "{{x}} {a|b} \\{") {
		t.Errorf("the html lost the braces of the value: %q", html)
	}
	if !strings.HasPrefix(text, evil+"\n\n"+FooterTextLead) { // the text part inserts the value as it is, line breaks included (the platform's footer follows)
		t.Errorf("the text = %q, want the value as it is %q and then the footer", text, evil)
	}
	// the control: the same characters in the TEMPLATE are the language. A template `{a|b}` is a choice and `{{x|none}}` a field.
	q := prepareT(t, tplJob, Content{Subject: "{a|b} {{x|none}}", HTML: "<p>x</p>"})
	s, _, _ := renderT(t, q, tplRow(1, nil))
	if s != "a none" && s != "b none" {
		t.Errorf("the template text `{a|b} {{x|none}}` rendered %q: the language was not interpreted", s)
	}
}

// A CSV column called email or unsubscribe_url never replaces the reserved value. The control is an ordinary column, which is used.
func TestTemplateRenderer_ReservedNamesCannotBeOverriddenByAColumn(t *testing.T) {
	p := prepareT(t, tplJob, Content{Subject: "S {{name}}", HTML: "<p>{{email}} {{unsubscribe_url}} {{name}}</p>", Text: "{{email}} {{unsubscribe_url}}"})
	row := tplRow(1, map[string]string{"email": "evil@example.test", "unsubscribe_url": "https://evil.test/u", "name": "Ann"})
	_, html, text := renderT(t, p, row)
	if strings.Contains(html, "evil") || strings.Contains(text, "evil") {
		t.Fatalf("a column overrode a reserved field: %q / %q", html, text)
	}
	if !strings.Contains(html, "r00001@example.test") || !strings.Contains(html, tplLink) || !strings.Contains(text, "r00001@example.test "+tplLink) {
		t.Errorf("the reserved values are not the row's: %q / %q", html, text)
	}
	if !strings.Contains(html, " Ann") {
		t.Errorf("an ordinary column was not used: %q", html)
	}
	// and the reserved names are never missing, a column or not
	if got := p.Missing(EngineRow{ID: rowID(2), Ordinal: 2}); !reflect.DeepEqual(got, []string{"name"}) {
		t.Errorf("Missing = %v, want only the ordinary column [name] (email and unsubscribe_url are always supplied)", got)
	}
}

// ---- missing --------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestTemplateRenderer_MissingNeedsNoFallbackAndMeansAbsentOrBlank(t *testing.T) {
	p := prepareT(t, tplJob, Content{Subject: "{{b}} {{a|x}}", HTML: "<p>{{c}} {{d|}} {{b}}</p>", Text: "{{e}}"})
	for _, c := range []struct {
		name   string
		fields map[string]string
		want   []string
	}{
		{"nothing at all: the three without a fallback, sorted, once each", nil, []string{"b", "c", "e"}},
		{"a blank value is missing", map[string]string{"b": "  \t\n", "c": "1", "e": "1"}, []string{"b"}},
		{"a value makes it present", map[string]string{"b": "1", "c": "1", "e": "1"}, []string{}},
		{"a fallback never makes a row missing", map[string]string{"b": "1", "c": "1", "e": "1", "a": "", "d": ""}, []string{}},
	} {
		got := p.Missing(tplRow(1, c.fields))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Missing = %v, want %v", c.name, got, c.want)
		}
	}
	// a store with only a Renderer has nothing missing, whatever the content says (the Phase 4 behaviour)
	plain := plainCampaign{r: PlainRenderer{}, c: Content{Subject: "{{b}}"}}
	if got := plain.Missing(tplRow(1, nil)); len(got) != 0 {
		t.Errorf("a plain campaign says %v is missing", got)
	}
}

// decodeFields: a string as it is, a number or a boolean as its text, anything else absent. The control is the plain-string row, which every other writer produces.
func TestDecodeFields_CoercesScalarsAndDropsTheRest(t *testing.T) {
	got, err := decodeFields([]byte(`{"s":"text","n":12,"f":1.50,"t":true,"z":false,"nul":null,"arr":[1],"obj":{"a":1},"e":""}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"s": "text", "n": "12", "f": "1.50", "t": "true", "z": "false", "e": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeFields = %v, want %v", got, want)
	}
	if g, err := decodeFields([]byte(`{}`)); err != nil || len(g) != 0 || g == nil {
		t.Errorf("an empty object = %v, %v; want an empty non-nil map", g, err)
	}
	if g, err := decodeFields(nil); err != nil || g == nil {
		t.Errorf("no bytes = %v, %v; want an empty non-nil map", g, err)
	}
	if _, err := decodeFields([]byte(`[1]`)); err == nil {
		t.Error("an array was accepted as a row's merge data")
	}
}

func TestMissingReason_NamesFieldsAndIsBounded(t *testing.T) {
	if got := missingReason([]string{"a", "b"}); got != "a,b" {
		t.Errorf("missingReason = %q", got)
	}
	var names []string
	for i := 0; i < 60; i++ {
		names = append(names, fmt.Sprintf("a_rather_long_field_name_%03d_", i))
	}
	got := missingReason(names)
	if len(got) > maxReasonNameBytes+20 || !strings.HasSuffix(got, " more") || !strings.Contains(got, ",+") {
		t.Errorf("a long list of names gave %d bytes %q: want it cut with ,+N more", len(got), got)
	}
	if !strings.HasPrefix(got, names[0]+","+names[1]) {
		t.Errorf("the cut list does not start with the first names: %q", got)
	}
}

// ---- the footer ------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestTemplateRenderer_FooterIsPerPartAndOnceWhicheverWayTheTemplatesPlaceTheLink(t *testing.T) {
	count := func(s, sub string) int { return strings.Count(s, sub) }
	const words = "To stop receiving these messages"
	row := tplRow(1, map[string]string{"n": "Ann"})
	cases := []struct {
		name                         string
		c                            Content
		wantHTMLLinks, wantTextLinks int  // occurrences of the link in the html and in the text
		htmlFooter, textFooter       bool // the platform's own footer words are in that part
	}{
		{"neither places it, blank text: the html carries the footer once and the generated text derives it", Content{Subject: "S", HTML: "<p>Hi {{n}}</p>"}, 1, 1, true, true},
		{"neither places it, explicit text: both parts carry their own footer once", Content{Subject: "S", HTML: "<p>Hi</p>", Text: "Hi {{n}}"}, 1, 1, true, true},
		{"the html places it, blank text: no footer anywhere, the generated text has the link once because the html does", Content{Subject: "S", HTML: `<p><a href="{{unsubscribe_url}}">stop</a></p>`}, 1, 1, false, false},
		{"the html places it, the explicit text does not: the text gets the footer and the html no second link", Content{Subject: "S", HTML: `<p><a href="{{unsubscribe_url}}">stop</a></p>`, Text: "Hi {{n}}"}, 1, 1, false, true},
		{"the explicit text places it, the html does not: the html gets the footer and the text no second line", Content{Subject: "S", HTML: "<p>Hi</p>", Text: "stop: {{unsubscribe_url}}"}, 1, 1, true, false},
		{"both place it: nothing is added at all", Content{Subject: "S", HTML: `<p><a href="{{unsubscribe_url}}">stop</a></p>`, Text: "stop: {{unsubscribe_url}}"}, 1, 1, false, false},
		{"the subject places it and nothing else does: it decides nothing for the parts", Content{Subject: "S {{unsubscribe_url}}", HTML: "<p>Hi</p>", Text: "Hi"}, 1, 1, true, true},
		{"a text-only campaign gets the footer line and no html at all", Content{Subject: "S", HTML: "", Text: "Hi {{n}}"}, 0, 1, false, true},
	}
	for _, c := range cases {
		s, h, x, err := prepareT(t, tplJob, c.c).Render(row)
		if err != nil {
			t.Fatal(err)
		}
		if got := count(h, tplLink); got != c.wantHTMLLinks {
			t.Errorf("%s: the html holds the link %d times, want %d: %q", c.name, got, c.wantHTMLLinks, h)
		}
		if got := count(x, tplLink); got != c.wantTextLinks {
			t.Errorf("%s: the text holds the link %d times, want %d: %q", c.name, got, c.wantTextLinks, x)
		}
		if got := strings.Contains(h, words); got != c.htmlFooter {
			t.Errorf("%s: the platform's footer words in the html = %v, want %v: %q", c.name, got, c.htmlFooter, h)
		}
		if got := strings.Contains(x, words); got != c.textFooter {
			t.Errorf("%s: the platform's footer words in the text = %v, want %v: %q", c.name, got, c.textFooter, x)
		}
		if count(h, words) > 1 || count(x, words) > 1 {
			t.Errorf("%s: the footer is there twice: %q / %q", c.name, h, x)
		}
		if strings.Contains(s, words) {
			t.Errorf("%s: the subject has a footer: %q", c.name, s)
		}
		// the same row renders to the same bytes again, whatever the footer did
		if s2, h2, x2, _ := prepareT(t, tplJob, c.c).Render(row); s2 != s || h2 != h || x2 != x {
			t.Errorf("%s: a second render differs", c.name)
		}
	}
	// the exact wording (documented in Docs/templates.md): the html footer is one paragraph with the link on the word `unsubscribe`, the text footer one line after a blank line
	_, h, x, _ := prepareT(t, tplJob, Content{Subject: "S", HTML: "<p>Hi</p>", Text: "Hi"}).Render(row)
	if h != "<p>Hi</p>\n"+FooterHTMLLead+tplLink+FooterHTMLTrail || x != "Hi\n\n"+FooterTextLead+tplLink {
		t.Errorf("the footers are %q and %q", h, x)
	}
	// a row with no link (a store with no maker: no send job can be confirmed there) gets no footer and no crash
	r := row
	r.UnsubscribeURL = ""
	_, h, x, _ = prepareT(t, tplJob, Content{Subject: "S", HTML: "<p>Hi</p>", Text: "Hi"}).Render(r)
	if h != "<p>Hi</p>" || x != "Hi" {
		t.Errorf("with no link the message is %q / %q, want it untouched", h, x)
	}
}

// The footer goes before the last </body> in any case, and the search does not break on text whose lower-case form is longer (U+0130 lower-cases to three bytes).
func TestWithHTMLFooter_GoesBeforeTheLastBodyClose(t *testing.T) {
	link := "https://app.example.test/u/a&b"
	footer := FooterHTMLLead + "https://app.example.test/u/a&amp;b" + FooterHTMLTrail
	for _, c := range []struct{ name, in, want string }{
		{"no body tag: at the end", "<p>x</p>", "<p>x</p>\n" + footer},
		{"a body tag: inside it", "<html><body><p>x</p></body></html>", "<html><body><p>x</p>" + footer + "\n</body></html>"},
		{"upper case", "<BODY>x</BODY>", "<BODY>x" + footer + "\n</BODY>"},
		{"the LAST of two", "</body>a</body>b", "</body>a" + footer + "\n</body>b"},
		{"a character that changes length when lower-cased", "<body>\u0130\u0130\u0130</body>", "<body>\u0130\u0130\u0130" + footer + "\n</body>"},
	} {
		if got := withHTMLFooter(c.in, link); got != c.want {
			t.Errorf("%s: withHTMLFooter(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	if lastBodyClose("</bod") != -1 || lastBodyClose("") != -1 || lastBodyClose("</body") != -1 {
		t.Error("lastBodyClose found a tag that is not there")
	}
}

// The text part of a campaign with a blank text is made from the HTML, footer included, once.
func TestTemplateRenderer_BlankTextIsGeneratedFromTheRenderedHTML(t *testing.T) {
	p := prepareT(t, tplJob, Content{Subject: "S", HTML: `<p>Hello <b>{{n|there}}</b></p><p>See <a href="https://x.example.test/p">the page</a></p>`, Text: "  \n"})
	_, h, x := renderT(t, p, tplRow(1, map[string]string{"n": "Ann"}))
	if !strings.Contains(h, "Hello <b>Ann</b>") {
		t.Fatalf("html = %q", h)
	}
	for _, want := range []string{"Hello Ann", "the page (https://x.example.test/p)", "To stop receiving these messages"} {
		if !strings.Contains(x, want) {
			t.Errorf("the generated text %q lacks %q", x, want)
		}
	}
	if strings.Contains(x, "<") {
		t.Errorf("the generated text holds markup: %q", x)
	}
}

// ---- syntax at save ------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestTemplateRenderer_SyntaxErrorsNameThePartAndThePosition(t *testing.T) {
	for _, c := range []struct {
		name         string
		c            Content
		part         string
		line, column int
		contains     string
	}{
		{"an unclosed group in the subject", Content{Subject: "Hi {a|b", HTML: "<p>x</p>"}, TemplateSubject, 1, 4, "unclosed {"},
		{"a stray brace in the html, on its second line", Content{Subject: "S", HTML: "<style>\n  p } </style>"}, TemplateHTML, 2, 5, "unexpected }"},
		{"a one-option group in the text", Content{Subject: "S", HTML: "<p>x</p>", Text: "ok\n\n{only}"}, TemplateText, 3, 1, "at least two options"},
		{"an invalid field name in the html", Content{Subject: "S", HTML: "<p>{{first name}}</p>"}, TemplateHTML, 1, 11, "invalid character"},
		{"the subject is judged before the html", Content{Subject: "{x", HTML: "}"}, TemplateSubject, 1, 1, "unclosed {"},
	} {
		_, err := TemplateRenderer{}.Prepare(tplJob, c.c)
		var te *TemplateError
		if !errors.As(err, &te) || !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want a *TemplateError that is an ErrBadRequest", c.name, err)
			continue
		}
		if te.Part != c.part || te.Line != c.line || te.Column != c.column || !strings.Contains(te.Message, c.contains) {
			t.Errorf("%s: %+v, want part %s at %d:%d with %q in the message", c.name, te, c.part, c.line, c.column, c.contains)
		}
		if strings.Contains(err.Error(), "Hi {a|b") || strings.Contains(err.Error(), "<style>") {
			t.Errorf("%s: the error quotes the user's template: %v", c.name, err)
		}
	}
	// the controls: the same content with the mistake escaped prepares, and a blank text is not compiled
	ok := Content{Subject: "Hi \\{a|b", HTML: "<style>\n  p \\} </style>", Text: "ok\n\n\\{only\\}"}
	if _, err := (TemplateRenderer{}).Prepare(tplJob, ok); err != nil {
		t.Errorf("the escaped content was refused: %v", err)
	}
	if _, err := (TemplateRenderer{}).Prepare(tplJob, Content{Subject: "S", HTML: "<p>x</p>", Text: " \n"}); err != nil {
		t.Errorf("a blank text was refused: %v", err)
	}
}

// checkTemplates is the one check of every path that gives a send job its content: a store with a template renderer refuses a mistake, a store with only a Renderer reads no syntax (the Phase 4 behaviour,
// which the existing tests of the store rely on), and a verify job (no content) is not asked.
func TestStore_CheckTemplatesOnlyWithAPreparer(t *testing.T) {
	bad := &Content{Subject: "S", HTML: "<style>p { }</style>"}
	withT := &Store{prep: TemplateRenderer{}}
	plain := &Store{render: PlainRenderer{}}
	var te *TemplateError
	if err := withT.checkTemplates(bad); !errors.As(err, &te) || te.Part != TemplateHTML {
		t.Errorf("a store with templates: %v, want the html part refused", err)
	}
	if err := plain.checkTemplates(bad); err != nil {
		t.Errorf("a store with only a Renderer refused the content: %v", err)
	}
	if err := withT.checkTemplates(nil); err != nil {
		t.Errorf("a verify job (no content) was asked: %v", err)
	}
	if err := withT.checkTemplates(&Content{Subject: "S", HTML: "<style>p \\{ \\}</style>"}); err != nil {
		t.Errorf("the escaped content was refused: %v", err)
	}
	if !withT.TemplatesEnabled() || plain.TemplatesEnabled() {
		t.Error("TemplatesEnabled does not say which store has the renderer")
	}
	// WithRenderer after WithPreparer takes the renderer back (the last option wins), and the other way round the preparer is preferred
	s := &Store{render: PlainRenderer{}}
	WithPreparer(TemplateRenderer{})(s)
	if !s.TemplatesEnabled() {
		t.Error("WithPreparer did not set the preparer")
	}
	WithRenderer(PlainRenderer{})(s)
	if s.TemplatesEnabled() {
		t.Error("WithRenderer after WithPreparer left the preparer in place")
	}
	WithPreparer(nil)(s)
	if s.TemplatesEnabled() {
		t.Error("a nil preparer was set")
	}
}

// prepare returns the Preparer's campaign when there is one and the Renderer's otherwise: the second is what every store of the Phase 4 tests has.
func TestStore_PrepareFallsBackToTheRenderer(t *testing.T) {
	c := Content{Subject: "{{x}}", HTML: "<b>{x</b>"}
	s := &Store{render: PlainRenderer{}}
	p, err := s.prepare(tplJob, c)
	if err != nil {
		t.Fatal(err)
	}
	sub, h, _, _ := p.Render(tplRow(1, nil))
	if sub != "{{x}}" || h != "<b>{x</b>" {
		t.Errorf("the plain campaign rendered %q %q, want the content as written", sub, h)
	}
	if got := p.Missing(tplRow(1, nil)); len(got) != 0 {
		t.Errorf("a plain campaign reports %v missing", got)
	}
	s.prep = TemplateRenderer{}
	if _, err := s.prepare(tplJob, c); err == nil {
		t.Error("the template renderer accepted `<b>{x</b>`")
	}
}

// ---- the refusal words -----------------------------------------------------------------------------------------------------------------------------------------------------------------

// Every refusal of checkRendered carries a word, and the words are the ones documented (Docs/templates.md) and stored as a row's reason. The control is a message that passes.
func TestCheckRendered_EveryRefusalHasItsWord(t *testing.T) {
	big := strings.Repeat("a", MaxBodyBytes+1)
	for _, c := range []struct {
		name                string
		subject, html, text string
		word                string
		wantInTheSentence   string
	}{
		{"a NUL", "S", "a\x00b", "", WordInvalidText, "not valid UTF-8"},
		{"a blank subject", "  ", "<p>x</p>", "", WordSubjectBlank, "blank"},
		{"a subject over the limit", strings.Repeat("s", MaxSubjectBytes+1), "<p>x</p>", "", WordSubjectTooLong, "limit is 998"},
		{"a subject on two lines", "a\nb", "<p>x</p>", "", WordSubjectNotOneLine, "one line"},
		{"an html over the limit", "S", big, "", WordHTMLTooLarge, "rendered html"},
		{"a text over the limit", "S", "<p>x</p>", big, WordTextTooLarge, "rendered text"},
		{"no body", "S", " ", "\n", WordNoBody, "no body"},
	} {
		_, err := checkRendered(7, c.subject, c.html, c.text, "")
		var br *BadRequestError
		if !errors.As(err, &br) || !errors.Is(err, ErrBadRequest) || !strings.Contains(br.Reason, "row 7") || !strings.Contains(br.Reason, c.wantInTheSentence) {
			t.Errorf("%s: got %v, want an ErrBadRequest for row 7 saying %q", c.name, err, c.wantInTheSentence)
		}
		if got := refusalWordOf(err); got != c.word {
			t.Errorf("%s: the word is %q, want %q", c.name, got, c.word)
		}
	}
	if _, err := checkRendered(7, "S", "<p>x</p>", "", ""); err != nil {
		t.Errorf("a good message was refused: %v", err)
	}
	if refusalWordOf(errors.New("the link maker failed")) != "" || refusalWordOf(nil) != "" || refusalWordOf(badRequest("x")) != "" {
		t.Error("an error that is not a refusal of a message carries a word")
	}
}

// messageFor: the link first, then the missing check (a message with a hole is never made), then the shared table's checks; a failing link maker is the store's error, not a word.
func TestMessageFor_LinkThenMissingThenTheTable(t *testing.T) {
	s := &Store{unsub: func(id string) (string, error) { return "https://app.example.test/u/" + id, nil }}
	p := prepareT(t, tplJob, Content{Subject: "{{s|}}", HTML: "<p>{{n}}</p>"})
	row := EngineRow{ID: rowID(1), Ordinal: 1, EmailNorm: "a@example.test", Fields: map[string]string{"n": "Ann", "s": "Hi"}}
	got, m, err := s.messageFor(p, "", row)
	if err != nil || got.UnsubscribeURL != "https://app.example.test/u/"+rowID(1) || m.subject != "Hi" || !strings.Contains(m.html, got.UnsubscribeURL) {
		t.Fatalf("messageFor = %+v %+v %v", got, m, err)
	}
	_, _, err = s.messageFor(p, "", EngineRow{ID: rowID(2), Ordinal: 2, EmailNorm: "b@example.test", Fields: map[string]string{"s": "Hi"}})
	if w := refusalWordOf(err); w != WordMissingField || !strings.Contains(err.Error(), "row 2") || !strings.Contains(err.Error(), "n") {
		t.Errorf("a row that lacks n: %v (word %q), want the missing_field refusal for row 2", err, w)
	}
	_, _, err = s.messageFor(p, "", EngineRow{ID: rowID(3), Ordinal: 3, EmailNorm: "c@example.test", Fields: map[string]string{"n": "Ann"}}) // s has an empty fallback: not missing, the subject is blank
	if w := refusalWordOf(err); w != WordSubjectBlank {
		t.Errorf("a row whose subject renders blank: %v (word %q), want subject_blank", err, w)
	}
	bad := &Store{unsub: func(string) (string, error) { return "http://not-https", nil }}
	if _, _, err = bad.messageFor(p, "", row); err == nil || refusalWordOf(err) != "" {
		t.Errorf("a link the column would refuse: %v, want the store's own error with no word", err)
	}
}

// ---- the preview ---------------------------------------------------------------------------------------------------------------------------------------------------------------------

// pickSamples: the first, the middle and the last of the rows that would be sent, never one twice, fewer for a shorter list, none for none. The controls are the lists of one, two and three.
func TestPickSamples_FirstMiddleLastAndNeverTwice(t *testing.T) {
	seq := func(n int) []int32 {
		out := make([]int32, n)
		for i := range out {
			out[i] = int32(10 * (i + 1)) // ordinals with gaps: the rows left out are not in the list
		}
		return out
	}
	for _, c := range []struct {
		n    int
		want []int32
	}{
		{0, nil}, {1, []int32{10}}, {2, []int32{10, 20}}, {3, []int32{10, 20, 30}}, {4, []int32{10, 20, 40}}, {5, []int32{10, 30, 50}}, {10, []int32{10, 50, 100}}, {100000, []int32{10, 500000, 1000000}},
	} {
		if got := pickSamples(seq(c.n)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("pickSamples(%d rows) = %v, want %v", c.n, got, c.want)
		}
	}
	if len(pickSamples(seq(1000))) > SampleCount {
		t.Errorf("more than %d samples", SampleCount)
	}
}

// buildPreview with the pass over the templates: units are the rows that would be sent; the names are an array; a verify job is not touched even if the snapshot carries a result of the pass (which
// computePreview never gives it). The controls are the same snapshot without the pass.
func TestBuildPreview_UnitsSkipTheRowsTheTemplatesLeaveOut(t *testing.T) {
	mk := func(module string, tpl templateScan) *snapshot {
		return &snapshot{module: module, metered: true, rows: 10, pending: 9, malformed: 1, tpl: tpl, quote: money.RateQuote{Unit: "m2_accepted", UnitCost: 3}}
	}
	tpl := templateScan{missing: 2, names: []string{"first_name", "plan"}, unrenderable: 1}
	p := buildPreview(mk(ModuleSendManaged, tpl), "", 1, money.Balance{Available: 100}, time.Time{})
	if p.Pending != 9 || p.Suppressed != 1 || p.MissingFields != 2 || p.Unrenderable != 1 || p.Units != 9-1-2-1 || p.Credits != int64(p.Units)*3 {
		t.Errorf("a send job: pending %d suppressed %d missing %d unrenderable %d units %d credits %d", p.Pending, p.Suppressed, p.MissingFields, p.Unrenderable, p.Units, p.Credits)
	}
	if !reflect.DeepEqual(p.MissingFieldNames, []string{"first_name", "plan"}) || p.MissingFieldNamesTruncated {
		t.Errorf("names = %v truncated %v", p.MissingFieldNames, p.MissingFieldNamesTruncated)
	}
	none := buildPreview(mk(ModuleSendManaged, templateScan{}), "", 1, money.Balance{Available: 100}, time.Time{})
	if none.MissingFieldNames == nil || len(none.MissingFieldNames) != 0 || none.Units != 8 || none.MissingFields != 0 {
		t.Errorf("with nothing left out: names %v units %d missing %d, want [] 8 0", none.MissingFieldNames, none.Units, none.MissingFields)
	}
	b, _ := json.Marshal(none)
	if !strings.Contains(string(b), `"missing_field_names":[]`) || !strings.Contains(string(b), `"missing_fields":0`) || !strings.Contains(string(b), `"unrenderable":0`) {
		t.Errorf("the stored document of an empty pass: %s", b)
	}
	trunc := buildPreview(mk(ModuleSendBYO, templateScan{missing: 3, names: []string{"a"}, namesTruncated: true}), "", 0, money.Balance{}, time.Time{})
	if !trunc.MissingFieldNamesTruncated {
		t.Error("the truncation flag was lost")
	}
	// a send list that the templates leave empty is a valid preview: zero units, zero credits, no shortfall
	empty := buildPreview(mk(ModuleSendManaged, templateScan{missing: 9}), "", 0, money.Balance{}, time.Time{})
	if empty.Units != 0 || empty.Credits != 0 || empty.Shortfall != 0 {
		t.Errorf("an entirely skipped list: units %d credits %d shortfall %d", empty.Units, empty.Credits, empty.Shortfall)
	}
	// module 1 has no templates: nothing is left out of a verify job, whatever the snapshot says
	v := buildPreview(mk(ModuleVerify, tpl), "", 1, money.Balance{Available: 100}, time.Time{})
	if v.Units != 9 || !v.SuppressedStillVerified {
		t.Errorf("a verify job: units %d (want every pending row, 9), suppressed_still_verified %v", v.Units, v.SuppressedStillVerified)
	}
}

// The samples hold the recipients' data: Preview marshals without them, so what Preview writes into platform.jobs.preview never has them. The control is the same struct with every other member present.
func TestPreview_SamplesAreNeverInTheStoredDocument(t *testing.T) {
	p := Preview{Rows: 3, Pending: 3, Units: 3, Fingerprint: strings.Repeat("a", 64), Samples: []Sample{{Ordinal: 1, Subject: "Hi Ann SECRET-NAME", HTML: "<p>SECRET-NAME</p>", Text: "SECRET-NAME"}}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET-NAME") || strings.Contains(string(b), "samples") || strings.Contains(strings.ToLower(string(b)), "sample") {
		t.Errorf("the stored document holds the samples: %s", b)
	}
	if !strings.Contains(string(b), `"units":3`) || !strings.Contains(string(b), `"fingerprint"`) {
		t.Errorf("the document lost its other members: %s", b)
	}
	var back Preview
	if err := json.Unmarshal(b, &back); err != nil || len(back.Samples) != 0 {
		t.Errorf("read back: %+v %v", back, err)
	}
}

// ---- the fingerprint -----------------------------------------------------------------------------------------------------------------------------------------------------------------

// Merge data is in the fingerprint of a send job, as a digest, and only when there is some: a job with none keeps the fingerprint it had (the golden value of fingerprint_test.go), and a change of a
// value changes it. The golden below is sha256sum of the canonical text written out by hand.
func TestFingerprint_MergeDataIsNamedWhenThereIsSome(t *testing.T) {
	base := baseInput()
	with := baseInput()
	with.FieldsDigest = "f1"
	// sha256 of {"content":"c","fields":"f1","job":"1111...","module":"m2","prices":{"m2_accepted":1,"m2_bounced":1},"rows":"r","source":"","source_results":"","unknown_choice":"exclude"}
	const want = "96e959ea2f7071d53fe78c82551a085b8042ff2e948a5dd4f3b50acd5b53100d"
	if got := with.fingerprint(); got == base.fingerprint() {
		t.Error("merge data does not change the fingerprint: a value changed under a preview would not make it stale")
	}
	if got := with.fingerprint(); got != want {
		t.Errorf("fingerprint with merge data = %s, want the independently computed %s", got, want)
	}
	other := baseInput()
	other.FieldsDigest = "f2"
	if other.fingerprint() == with.fingerprint() {
		t.Error("two digests of merge data give one fingerprint")
	}
	// the document of a job without merge data has no `fields` member at all
	doc, _ := canonicalJSON(map[string]any{"job": goldJob})
	if strings.Contains(string(doc), "fields") {
		t.Fatal("test bug")
	}
	if baseInput().fingerprint() != "148a9234b6cf571a93802a668a464e1fa6228d290d18fcc64c3541f68f001704" {
		t.Error("a job without merge data does not keep its fingerprint")
	}
	// the two states a send job's confirm leaves rows in read as pending, like suppressed, so that a digest does not depend on a state that exists only after the confirm
	d := func(state string) string {
		x := newLineDigest()
		x.rowLine(1, state, "a@example.com", "")
		return x.sum()
	}
	if d(StateMissingField) != d(StatePending) || d(StateUnrenderable) != d(StatePending) {
		t.Error("a row left out by the templates digests differently from a pending one")
	}
	if d(StateMalformed) == d(StatePending) {
		t.Error("the control: a malformed row digests like a pending one")
	}
}
