package jobs

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/render"
)

// CheckCampaign (Phase 6 A3, template_check.go) is the save's own judgement of a campaign that is still being written. These tests hold it to the save: the same functions, so the same words and the same
// positions, and a table in which the save and the check are asked the same questions.

func TestCheckCampaign_ACampaignThatCompilesListsItsFieldsInOrderWithTheirFallbacks(t *testing.T) {
	got, err := CheckCampaign(Content{
		Subject: "Hi {{first_name|there}}",
		HTML:    "<p>{{first_name}} at {{company|your company}} {{email}} {{unsubscribe_url|nowhere}} {a|{{city|here}}}</p>",
		Text:    "{{company}} {{zip}} {{city|here}}",
	})
	if err != nil || len(got.Errors) != 0 {
		t.Fatalf("CheckCampaign = %+v, %v; want a campaign that compiles", got, err)
	}
	want := []CampaignField{
		{"first_name", false}, // a fallback in the subject, none in the html: one use without a fallback makes it required
		{"company", false},    // a fallback in the html, none in the text
		{"city", true},        // a fallback at every use
		{"zip", false},        // no fallback at all
	}
	if !reflect.DeepEqual(got.Fields, want) {
		t.Fatalf("fields = %+v, want %+v (in the order of first use across subject, html, text; the reserved names left out)", got.Fields, want)
	}
	// the twins: the reserved names ARE used (so leaving them out is a choice), and a field with a fallback everywhere is not required
	for _, r := range []string{"email", "unsubscribe_url"} {
		if !render.IsReserved(r) {
			t.Fatalf("control failed: %s is not reserved", r)
		}
	}
	only, _ := CheckCampaign(Content{Subject: "{{a|x}}", HTML: "{{a|y}} {{b|z}}"})
	if !reflect.DeepEqual(only.Fields, []CampaignField{{"a", true}, {"b", true}}) {
		t.Fatalf("fields with a fallback at every use = %+v", only.Fields)
	}
}

func TestCheckCampaign_AnEmptyPartIsAllowedAndFieldsAreNeverNil(t *testing.T) {
	for name, c := range map[string]Content{
		"nothing written yet":  {},
		"only a subject":       {Subject: "Hello"},
		"only a body":          {HTML: "<p>hi</p>"},
		"only a text":          {Text: "hi {{name}}"},
		"a blank subject":      {Subject: " \t ", HTML: "<p>hi</p>"},
		"a blank body":         {Subject: "Hello", HTML: "  \n ", Text: " "},
		"a subject with edges": {Subject: "\n Hello \n", HTML: "<p>hi</p>"}, // trimmed, as the save trims it
	} {
		got, err := CheckCampaign(c)
		if err != nil || len(got.Errors) != 0 || got.Fields == nil {
			t.Errorf("%s: CheckCampaign = %+v, %v; want no error and a non-nil list of fields", name, got, err)
		}
	}
	// the twin: the SAVE refuses every one of the campaigns that has no subject or no body, so the allowance is the check's own and not a looseness of the rules
	for name, c := range map[string]Content{"nothing written yet": {}, "a blank subject": {Subject: " ", HTML: "<p>hi</p>"}, "a blank body": {Subject: "Hello", HTML: " ", Text: " "}} {
		if _, err := validateContent(&c); !errors.Is(err, ErrBadRequest) {
			t.Errorf("control: the save accepted %s: %v", name, err)
		}
	}
	// a field is found in a campaign that has only a text
	if got, _ := CheckCampaign(Content{Text: "hi {{name}}"}); !reflect.DeepEqual(got.Fields, []CampaignField{{"name", false}}) {
		t.Errorf("fields of a text-only campaign = %+v", got.Fields)
	}
}

func TestCheckCampaign_ASyntaxErrorIsAnAnswerWithItsPartAndPositionAndEachPartHasItsOwn(t *testing.T) {
	// each part alone
	for _, c := range []struct {
		name string
		in   Content
		part string
		src  string
	}{
		{"subject", Content{Subject: "Hi {{name", HTML: "<p>ok</p>"}, TemplateSubject, "Hi {{name"},
		{"html", Content{Subject: "Hi", HTML: "<p>ok</p>\n<p>{only}</p>"}, TemplateHTML, "<p>ok</p>\n<p>{only}</p>"},
		{"text", Content{Subject: "Hi", HTML: "<p>ok</p>", Text: "café }"}, TemplateText, "café }"},
	} {
		got, err := CheckCampaign(c.in)
		if err != nil || len(got.Errors) != 1 || got.Errors[0].Part != c.part {
			t.Errorf("%s: CheckCampaign = %+v, %v; want one error in the %s", c.name, got, err, c.part)
			continue
		}
		var se *render.SyntaxError
		_, cerr := render.Compile(c.src)
		if !errors.As(cerr, &se) {
			t.Fatalf("%s: control failed: the source compiles", c.name)
		}
		if e := got.Errors[0]; e.Line != se.Line || e.Column != se.Column || e.Message != se.Message || e.Line < 1 || e.Column < 1 {
			t.Errorf("%s: the error is %+v, the template package's own is line %d column %d %q", c.name, e, se.Line, se.Column, se.Message)
		}
	}
	// all three at once: one error each, in the order of the parts, and the part that compiles is not mentioned
	got, err := CheckCampaign(Content{Subject: "{{a", HTML: "{b}", Text: "}"})
	if err != nil || len(got.Errors) != 3 || got.Errors[0].Part != TemplateSubject || got.Errors[1].Part != TemplateHTML || got.Errors[2].Part != TemplateText {
		t.Fatalf("three broken parts: %+v, %v", got, err)
	}
	two, _ := CheckCampaign(Content{Subject: "{{a", HTML: "<p>{{fine}}</p>", Text: "}"})
	if len(two.Errors) != 2 || two.Errors[0].Part != TemplateSubject || two.Errors[1].Part != TemplateText {
		t.Fatalf("two broken parts: %+v", two.Errors)
	}
	if !reflect.DeepEqual(two.Fields, []CampaignField{{"fine", false}}) {
		t.Fatalf("the part that compiles still lists its fields: %+v", two.Fields)
	}
	// a part stops at its FIRST mistake
	one, _ := CheckCampaign(Content{Subject: "Hi", HTML: "{a} {b} }"})
	if len(one.Errors) != 1 {
		t.Fatalf("a part with three mistakes gave %d errors, want its first", len(one.Errors))
	}
}

// {{unsubscribe_url}} inside a spintax group is the rule the template package cannot see (it is the store's, compilePart): the check must say it exactly as the save does, in every part.
func TestCheckCampaign_TheUnsubscribeLinkInsideAGroupIsRefusedLikeTheSave(t *testing.T) {
	for _, c := range []struct {
		name string
		in   Content
		part string
		line int
		col  int
	}{
		{"html", Content{Subject: "Hi", HTML: "{a|{{unsubscribe_url}}}"}, TemplateHTML, 1, 4},
		{"html with a fallback", Content{Subject: "Hi", HTML: "<p>\n  {a|{{unsubscribe_url|none}}}</p>"}, TemplateHTML, 2, 6},
		{"text", Content{Subject: "Hi", HTML: "<p>ok</p>", Text: "x {a|b {{unsubscribe_url}}}"}, TemplateText, 1, 8},
		{"subject", Content{Subject: "{a|{{unsubscribe_url}}}", HTML: "<p>ok</p>"}, TemplateSubject, 1, 4},
	} {
		got, err := CheckCampaign(c.in)
		if err != nil || len(got.Errors) != 1 {
			t.Errorf("%s: %+v, %v", c.name, got, err)
			continue
		}
		e := got.Errors[0]
		if e.Part != c.part || e.Line != c.line || e.Column != c.col || !strings.Contains(e.Message, "{{unsubscribe_url}}") {
			t.Errorf("%s: the error is %+v, want the %s at line %d column %d naming {{unsubscribe_url}}", c.name, e, c.part, c.line, c.col)
		}
	}
	// the twins: outside a group, with or without a fallback, in every part, it is fine
	ok, err := CheckCampaign(Content{Subject: "Hi", HTML: "<p>{a|b} <a href=\"{{unsubscribe_url}}\">stop</a> {{unsubscribe_url|x}}</p>", Text: "{c|d} {{unsubscribe_url}}"})
	if err != nil || len(ok.Errors) != 0 {
		t.Fatalf("an unsubscribe link outside every group was refused: %+v, %v", ok, err)
	}
}

// The check and the save are asked the same questions. For every campaign the save would accept as to its size and text, the save's Prepare and the check agree on whether it compiles and, when it does
// not, on the part, the line, the column and the message of the first mistake. The table has both kinds (the loop counts them), so a check that always said "fine" or always said "broken" fails it.
func TestCheckCampaign_TheFirstErrorIsExactlyTheOneTheSavesPrepareReturns(t *testing.T) {
	campaigns := []Content{
		{Subject: "Hello", HTML: "<p>Hi</p>"},
		{Subject: "Hello {{first_name|there}}", HTML: "<p>{{first_name}}</p>", Text: "{a|b|c}"},
		{Subject: "{{", HTML: "<p>x</p>"},
		{Subject: "Hello", HTML: "<p>{{1bad}}</p>"},
		{Subject: "Hello", HTML: "<p>{only}</p>"},
		{Subject: "Hello", HTML: "<p>x</p>", Text: "{a|b"},
		{Subject: "Hello", HTML: "<p>x</p>", Text: "}"},
		{Subject: "Hello", HTML: "line one\nline two {{open"},
		{Subject: "Hello", HTML: "café über {{open"},
		{Subject: "Hello", HTML: "{a|{{unsubscribe_url}}}"},
		{Subject: "Hello", HTML: "{a|b}", Text: "{a|{b|{{unsubscribe_url|x}}}}"},
		{Subject: "{a|{{unsubscribe_url}}}", HTML: "<p>x</p>"},
		{Subject: "Hello", HTML: strings.Repeat("{a|", 9) + "x" + strings.Repeat("}", 9)}, // nested deeper than the grammar allows
		{Subject: "Hello", HTML: "{" + strings.Repeat("a|", 65) + "b}"},                   // more options than the grammar allows
		{Subject: "Hello", HTML: "<p>\\{ \\} \\| \\\\ {{ok}}</p>"},                        // escapes
		{Subject: "Hello", HTML: "<p>x</p>", Text: "   "},                                 // a blank text: the save does not compile it
	}
	broken, fine := 0, 0
	for i, c := range campaigns {
		saved, verr := validateContent(&c)
		if verr != nil {
			t.Fatalf("campaign %d: the save refuses it for its size or text, so it is not a question for this table: %v", i, verr)
		}
		_, perr := TemplateRenderer{}.Prepare("", *saved)
		got, err := CheckCampaign(c)
		if err != nil {
			t.Fatalf("campaign %d: CheckCampaign: %v", i, err)
		}
		var te *TemplateError
		switch {
		case perr == nil:
			fine++
			if len(got.Errors) != 0 {
				t.Errorf("campaign %d: the save compiles it and the check says %+v", i, got.Errors)
			}
		case errors.As(perr, &te):
			broken++
			if len(got.Errors) == 0 || got.Errors[0] != *te {
				t.Errorf("campaign %d: the save's first error is %+v, the check's are %+v", i, *te, got.Errors)
			}
		default:
			t.Fatalf("campaign %d: Prepare returned %v, which is not a template error", i, perr)
		}
	}
	if broken < 8 || fine < 4 {
		t.Fatalf("the table has %d broken and %d good campaigns: the control matched too little", broken, fine)
	}
}

func TestCheckCampaign_TheSizeAndTextRulesAreTheSavesOwnWordsForWord(t *testing.T) {
	for name, c := range map[string]Content{
		"a subject of 999 bytes":    {Subject: strings.Repeat("a", MaxSubjectBytes+1), HTML: "<p>x</p>"},
		"a subject on two lines":    {Subject: "one\ntwo", HTML: "<p>x</p>"},
		"a subject with a CR":       {Subject: "one\rtwo", HTML: "<p>x</p>"},
		"an html over the limit":    {Subject: "Hello", HTML: strings.Repeat("a", MaxBodyBytes+1)},
		"a text over the limit":     {Subject: "Hello", HTML: "<p>x</p>", Text: strings.Repeat("a", MaxBodyBytes+1)},
		"a NUL in the html":         {Subject: "Hello", HTML: "<p>\x00</p>"},
		"bytes that are not text":   {Subject: "Hello", HTML: "<p>\xff\xfe</p>"},
		"a NUL in the subject":      {Subject: "Hel\x00lo", HTML: "<p>x</p>"},
		"invalid UTF-8 in the text": {Subject: "Hello", HTML: "<p>x</p>", Text: "\xc3\x28"},
	} {
		_, serr := validateContent(&c)
		got, cerr := CheckCampaign(c)
		var bad, cbad *BadRequestError
		if !errors.As(serr, &bad) || !errors.As(cerr, &cbad) || bad.Reason != cbad.Reason || len(got.Errors) != 0 || len(got.Fields) != 0 {
			t.Errorf("%s: the save says %v, the check says %v (%+v)", name, serr, cerr, got)
			continue
		}
		if strings.Contains(cbad.Reason, "aaaa") || strings.Contains(cbad.Reason, "\xff") {
			t.Errorf("%s: the reason quotes the content: %q", name, cbad.Reason)
		}
	}
	// the twins: the edge just inside each limit, and a reply-to that the save would refuse is not the check's business
	edge := Content{Subject: strings.Repeat("s", MaxSubjectBytes), HTML: strings.Repeat("h", MaxBodyBytes), Text: strings.Repeat("t", MaxBodyBytes), ReplyTo: "not an address"}
	if _, err := CheckCampaign(edge); err != nil {
		t.Errorf("the limits themselves were refused: %v", err)
	}
	if _, err := validateContent(&edge); err == nil {
		t.Error("control failed: the save accepted a reply-to that is not an address")
	}
}
