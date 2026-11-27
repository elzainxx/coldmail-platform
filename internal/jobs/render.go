package jobs

import (
	"errors"
	"fmt"
	"strings"
)

// Renderer turns a send job's frozen campaign and one recipient's row into the message that module 2 or 3 will send to that recipient: the subject, the HTML body and the plain-text part
// (public.job_items.rendered_subject, rendered_body and rendered_text). It is the Phase 4 hook, kept as it was: PlainRenderer, the identity, is what a store has unless it is given something else
// (WithRenderer), and a test's per-row renderer is a Renderer. Phase 5's merge fields, spintax and generated text part are the TemplateRenderer, a Preparer (template.go) that the store prefers to a
// Renderer when it has one (WithPreparer): a Renderer does not know the job, and the seed of a message is made of the job and the row. A renderer is pure: the same content and row give the same message, because
// EnsureModuleItems is idempotent and a replay must not change what an earlier step wrote. An error refuses the row, and with it the whole call: nothing is written.
type Renderer interface {
	Render(c Content, row EngineRow) (subject, html, text string, err error)
}

// PlainRenderer is the Phase 4 renderer: the campaign's subject, body and text exactly as the user wrote them, for every recipient.
type PlainRenderer struct{}

// Render returns the content as given.
func (PlainRenderer) Render(c Content, _ EngineRow) (string, string, string, error) {
	return c.Subject, c.HTML, c.Text, nil
}

// renderedMessage is one row's message after the renderer ran and the size rules of the shared table were applied.
type renderedMessage struct {
	subject string
	html    string
	text    string // "" when the text is blank: public.job_items.rendered_text is then NULL (a blank text means "no plain-text part" to both modules)
	replyTo string
}

// The words a refused message is named by (a row's reason when its state is unrenderable: platform_0019, rule P-10; also the reason a refusal is logged by). A word is never a value of the row.
const (
	WordRendererRefused   = "renderer_refused"            // the renderer returned an error for the row
	WordInvalidText       = "invalid_text"                // a rendered part is not valid UTF-8 text without NUL
	WordSubjectBlank      = "subject_blank"               // the rendered subject is empty after trimming
	WordSubjectTooLong    = "subject_too_long"            // more than MaxSubjectBytes
	WordSubjectNotOneLine = "subject_not_one_line"        // a line break in the subject
	WordHTMLTooLarge      = "html_too_large"              // more than MaxBodyBytes
	WordTextTooLarge      = "text_too_large"              // more than MaxBodyBytes
	WordNoBody            = "no_body"                     // the HTML and the text are both blank
	WordNoVisibleLink     = "no_visible_unsubscribe_link" // the message holds no link a recipient can see to unsubscribe by, even with the footer (an unclosed comment or tag swallowed it): never sent without one
)

// renderRefusal is the ErrBadRequest of a message the table refuses, with the word that names the refusal. It unwraps to the *BadRequestError that says it in a sentence, so errors.As and errors.Is see the
// same error they always did.
type renderRefusal struct {
	word string
	err  *BadRequestError
}

func (e *renderRefusal) Error() string { return e.err.Error() }
func (e *renderRefusal) Unwrap() error { return e.err }

func refuse(word, format string, args ...any) error {
	return &renderRefusal{word: word, err: &BadRequestError{Reason: fmt.Sprintf(format, args...)}}
}

// refusalWordOf is the word of a refusal of the renderer or of checkRendered ("" for any other error).
func refusalWordOf(err error) string {
	var r *renderRefusal
	if errors.As(err, &r) {
		return r.word
	}
	return ""
}

// checkRendered applies what the shared table and the two modules need of a message, whatever the renderer was: valid text without NUL, a non-blank one-line subject of at most MaxSubjectBytes,
// bodies of at most MaxBodyBytes each (module 2 and 3 cap rendered_text at 1 MiB in their own tables), a message that has a body at all. A blank text becomes "" (NULL in the table). The reasons
// never quote the message or the address. Every refusal carries its word (refusalWordOf).
func checkRendered(ordinal int, subject, html, text, replyTo string) (renderedMessage, error) {
	for _, f := range []struct{ name, v string }{{"subject", subject}, {"html", html}, {"text", text}} {
		if !textOK(f.v) {
			return renderedMessage{}, refuse(WordInvalidText, "row %d: the rendered %s is not valid UTF-8 text without NUL", ordinal, f.name)
		}
	}
	subject = strings.TrimSpace(subject)
	switch {
	case subject == "":
		return renderedMessage{}, refuse(WordSubjectBlank, "row %d: the rendered subject is blank", ordinal)
	case len(subject) > MaxSubjectBytes:
		return renderedMessage{}, refuse(WordSubjectTooLong, "row %d: the rendered subject is %d bytes, the limit is %d", ordinal, len(subject), MaxSubjectBytes)
	case strings.ContainsAny(subject, "\r\n"):
		return renderedMessage{}, refuse(WordSubjectNotOneLine, "row %d: the rendered subject is not one line", ordinal)
	case len(html) > MaxBodyBytes:
		return renderedMessage{}, refuse(WordHTMLTooLarge, "row %d: the rendered html is %d bytes, the limit is %d", ordinal, len(html), MaxBodyBytes)
	case len(text) > MaxBodyBytes:
		return renderedMessage{}, refuse(WordTextTooLarge, "row %d: the rendered text is %d bytes, the limit is %d", ordinal, len(text), MaxBodyBytes)
	case strings.TrimSpace(html) == "" && strings.TrimSpace(text) == "":
		return renderedMessage{}, refuse(WordNoBody, "row %d: the rendered message has no body", ordinal)
	}
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	return renderedMessage{subject: subject, html: html, text: text, replyTo: replyTo}, nil
}

// renderRow runs a Renderer on one row and checks what it returned. A renderer that fails is a refusal naming the row's ordinal (never the error's own text, which may quote the row).
func renderRow(r Renderer, c Content, row EngineRow) (renderedMessage, error) {
	return renderPrepared(plainCampaign{r: r, c: c}, c.ReplyTo, row)
}

// renderPrepared runs a prepared campaign on one row and checks what it returned (renderRow's rule, for either kind of renderer): the one place a row becomes a message, whether the message is being
// written (EnsureJobItems), shown (a preview's sample) or only tried (the confirm's pass over the rows).
func renderPrepared(p PreparedCampaign, replyTo string, row EngineRow) (renderedMessage, error) {
	subject, html, text, err := p.Render(row)
	if err != nil {
		var already *renderRefusal
		if errors.As(err, &already) { // a refusal that names its own word (template.go: no visible unsubscribe link) keeps it
			return renderedMessage{}, err
		}
		return renderedMessage{}, refuse(WordRendererRefused, "row %d: the renderer refused the row", row.Ordinal)
	}
	return checkRendered(row.Ordinal, subject, html, text, replyTo)
}
