package jobs

import (
	"errors"
	"fmt"

	"github.com/coldmail-platform/platform/internal/render"
)

// THE TEMPLATE CHECK (Phase 6 A3: POST /v1/templates/check). The authoring screen asks, while the user writes, whether the subject, the HTML and the text are templates the platform will accept, and which
// merge fields they use. It must never say "fine" about a campaign the save then refuses, so it is not a second implementation: it runs the save's own functions. The sizes, the text and the one-line
// rule are validateContentAs (the function the save runs, with the one switch that lets a part be blank while the user is still writing), and each part is compiled by compilePart, which is what
// TemplateRenderer.Prepare calls, including the rule that {{unsubscribe_url}} cannot stand inside a spintax group. A test holds the two together: for every campaign of a table, the first error the check
// finds is the error the save's Prepare returns.
//
// IT TOUCHES NOTHING. No database, no store, no clock, no row and no recipient: the three strings in, an answer out. Which is also why it takes no user.

// CampaignField is a merge field a campaign uses.
type CampaignField struct {
	Name        string
	HasFallback bool // true only when EVERY use of the name, in every part, carries a fallback ({{name|fallback}}); one use without makes the field required
}

// CampaignCheck is what CheckCampaign says about a campaign that follows the save's size and text rules.
type CampaignCheck struct {
	// Errors holds one syntax error for each part that has one, in the order subject, html, text (a part stops at its first mistake). Empty when the campaign compiles.
	Errors []TemplateError
	// Fields are the merge fields the campaign uses, in the order of their first appearance in the subject, then the HTML, then the text; never nil. The two reserved names (render.IsReserved: `email` and
	// `unsubscribe_url`) are left out: the platform supplies them for every row, so they never need a column. It is the answer for a campaign that compiles; with Errors it holds what the parts that do compile use.
	Fields []CampaignField
}

// CheckCampaign judges a campaign that is being written. A part may be blank (a subject not typed yet, a campaign with no body yet); every other rule is the save's. A campaign that breaks a size or
// text rule (a subject over 998 bytes or over more than one line, a body over 1 MiB, text that is not valid UTF-8 or holds a NUL) is the same *BadRequestError the save returns, with the save's words, and
// nothing is judged; one that is only not valid TEMPLATES is an answer, not an error (Errors). The content's reply-to is ignored here.
func CheckCampaign(c Content) (CampaignCheck, error) {
	c.ReplyTo = ""
	v, err := validateContentAs(&c, false)
	if err != nil {
		return CampaignCheck{}, err
	}
	out := CampaignCheck{Fields: []CampaignField{}}
	index := map[string]int{}
	for _, p := range []struct{ part, src string }{{TemplateSubject, v.Subject}, {TemplateHTML, v.HTML}, {TemplateText, v.Text}} {
		t, err := compilePart(p.part, p.src)
		if err != nil {
			var te *TemplateError
			if !errors.As(err, &te) { // compilePart returns nothing else for a source that is text (validateContentAs saw to that): a bug, never a user's mistake
				return CampaignCheck{}, fmt.Errorf("the %s template could not be checked: %w", p.part, err)
			}
			out.Errors = append(out.Errors, *te)
			continue
		}
		for _, f := range t.Fields() {
			if render.IsReserved(f.Name) {
				continue
			}
			if i, seen := index[f.Name]; seen {
				out.Fields[i].HasFallback = out.Fields[i].HasFallback && f.HasFallback
				continue
			}
			index[f.Name] = len(out.Fields)
			out.Fields = append(out.Fields, CampaignField{Name: f.Name, HasFallback: f.HasFallback})
		}
	}
	return out, nil
}
