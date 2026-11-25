package jobs

import "strings"

// Renderer turns a send job's frozen campaign and one recipient's row into the message that module 2 or 3 will send to that recipient: the subject, the HTML body and the plain-text part
// (public.job_items.rendered_subject, rendered_body and rendered_text). It is the hook of Phase 5 (merge fields from EngineRow.Fields, spintax, the generated plain-text part): Phase 4 ships
// PlainRenderer, the identity, so the store (WithRenderer) and the engine never learn what rendering is. A renderer is pure: the same content and row give the same message, because
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

// checkRendered applies what the shared table and the two modules need of a message, whatever the renderer was: valid text without NUL, a non-blank one-line subject of at most MaxSubjectBytes,
// bodies of at most MaxBodyBytes each (module 2 and 3 cap rendered_text at 1 MiB in their own tables), a message that has a body at all. A blank text becomes "" (NULL in the table). The reasons
// never quote the message or the address.
func checkRendered(ordinal int, subject, html, text, replyTo string) (renderedMessage, error) {
	for _, f := range []struct{ name, v string }{{"subject", subject}, {"html", html}, {"text", text}} {
		if !textOK(f.v) {
			return renderedMessage{}, badRequest("row %d: the rendered %s is not valid UTF-8 text without NUL", ordinal, f.name)
		}
	}
	subject = strings.TrimSpace(subject)
	switch {
	case subject == "":
		return renderedMessage{}, badRequest("row %d: the rendered subject is blank", ordinal)
	case len(subject) > MaxSubjectBytes:
		return renderedMessage{}, badRequest("row %d: the rendered subject is %d bytes, the limit is %d", ordinal, len(subject), MaxSubjectBytes)
	case strings.ContainsAny(subject, "\r\n"):
		return renderedMessage{}, badRequest("row %d: the rendered subject is not one line", ordinal)
	case len(html) > MaxBodyBytes:
		return renderedMessage{}, badRequest("row %d: the rendered html is %d bytes, the limit is %d", ordinal, len(html), MaxBodyBytes)
	case len(text) > MaxBodyBytes:
		return renderedMessage{}, badRequest("row %d: the rendered text is %d bytes, the limit is %d", ordinal, len(text), MaxBodyBytes)
	case strings.TrimSpace(html) == "" && strings.TrimSpace(text) == "":
		return renderedMessage{}, badRequest("row %d: the rendered message has no body", ordinal)
	}
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	return renderedMessage{subject: subject, html: html, text: text, replyTo: replyTo}, nil
}

// renderRow runs the renderer on one row and checks what it returned. A renderer that fails is a refusal naming the row's ordinal (never the error's own text, which may quote the row).
func renderRow(r Renderer, c Content, row EngineRow) (renderedMessage, error) {
	subject, html, text, err := r.Render(c, row)
	if err != nil {
		return renderedMessage{}, badRequest("row %d: the renderer refused the row", row.Ordinal)
	}
	return checkRendered(row.Ordinal, subject, html, text, c.ReplyTo)
}
