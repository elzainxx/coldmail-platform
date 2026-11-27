//go:build harness

package main

import (
	"strings"
)

// THE CAMPAIGN AND THE LIST OF A SEND JOB IN THE SCOPE sendjobs (Phase 5, chunk S9; Docs/templates.md is the grammar). The send job of the crash matrix is a TEMPLATED job: its subject, its HTML and
// (in the default mode) its text are templates with spintax groups of FOUR options and merge fields that carry a fallback, and every recipient's row carries merge data. The rows, the malformed lines, the
// repeats and the bounces are the ones the matrix always had, so every other invariant keeps its meaning; what changes is that the message each recipient gets depends on the job id, the item id and the row's
// data, which is what the invariant rendered_bytes_equal_the_pure_render (send_verify.go, send_render.go) holds against the message the sink received.
//
// WHAT THE TEMPLATES EXERCISE, on purpose: groups with four options at depth one and two; a group whose options hold other groups and a merge field; a field with a fallback used by the subject, the HTML and the
// text; the same field twice in one template; a link in the HTML (which the text conversion turns into `text (url)`); the footer (the HTML never places {{unsubscribe_url}}, so the platform adds its footer to it;
// the explicit text places the link itself, so the platform adds nothing to it: the footer rule is PER PART); a text-part made from the HTML (generated mode). Braces appear only as the language's own, so no
// escape is needed.
const (
	sjSubjectTemplate = "{Quick|Short|Brief|Fresh} {question|note|thought|idea} for {{first_name|there}}"
	sjGreeting        = "{Hi|Hello|Hey|Greetings} {{first_name|there}},"
	sjOpening         = "{I {saw|read|noticed|liked} your {post|note|work|talk} on {{topic|your field}}|Your {work|writing|talk|team} on {{topic|your field}} {stood out|caught my eye|impressed me|made me stop}|Lately {{topic|your field}} has been on my mind, and your {post|note|work|talk} is the reason|A {quick|short|brief|fresh} thought on {{topic|your field}} after your {post|note|work|talk}}."
	sjClosing         = "{Best|Cheers|Regards|Thanks},"

	sjHTMLTemplate = "<html><body>\n<p>" + sjGreeting + "</p>\n<p>" + sjOpening + "</p>\n<p>See <a href=\"https://example.test/offer\">our offer</a>.</p>\n<p>" + sjClosing + "<br>The harness</p>\n</body></html>"
	sjTextTemplate = sjGreeting + "\n\n" + sjOpening + "\n\nSee our offer: https://example.test/offer\n\n" + sjClosing + "\nThe harness\n\nTo stop receiving these messages: {{unsubscribe_url}}"
)

// how the campaign's text part is made
const (
	textExplicit  = "explicit"  // the campaign has a text template of its own (sjTextTemplate)
	textGenerated = "generated" // the text template is blank: the platform makes the text from the final HTML
)

// sendContent is the campaign of a send job of the scope: the tag names the scenario in the subject (the tag is plain text: it holds none of the language's characters).
func sendContent(tag, textMode string) (map[string]string, error) {
	if strings.ContainsAny(tag, "{}|\\\r\n") {
		return nil, failf(exitUsage, "--tag %q holds a character of the template language ({ } | \\) or a line break: the tag is part of the subject template", tag)
	}
	c := map[string]string{"subject": "[sendjobs " + tag + "] " + sjSubjectTemplate, "html": sjHTMLTemplate}
	switch textMode {
	case "", textExplicit:
		c["text"] = sjTextTemplate
	case textGenerated:
		// no text template: the platform makes the text part from the final HTML (a blank text means exactly that)
	default:
		return nil, failf(exitUsage, "--text-mode is %s or %s (got %q)", textExplicit, textGenerated, textMode)
	}
	return c, nil
}

// sjMergeTable is the merge data of the good recipients of a list, by their position among the list's good addresses (k modulo its length). The recipients the matrix bounces are k = 1 and k = 5 for the
// default list (nine good addresses, two bounced), so the interesting rows are the ones that are SENT and whose message the sink holds: a row with no first_name at all (the fallback), a blank first_name (the
// fallback: a value of only spaces is missing), a blank topic, a topic with non-ASCII text, a topic that is template syntax and a first_name that is markup (all of them are data: the renderer never reads a
// value as a template, and escapes it in the HTML).
var sjMergeTable = []map[string]string{
	{"first_name": "Ann", "topic": "pricing"},
	{"first_name": "Bea", "topic": "onboarding"},
	{"first_name": "Bob"},
	{"topic": "naïve café ✓"},
	{"first_name": " \t ", "topic": "{a|b} & {{email}}"},
	{"first_name": "Eve", "topic": "billing"},
	{"first_name": "O'Neil & <Co> \"x\"", "topic": "pricing"},
	{"first_name": "Zed", "topic": "  "},
	{"first_name": "Hana", "topic": "<b>bold</b>"},
}

// orDefault is s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// sendMergeData is the merge data of the k-th good address of a list (a copy: the caller may keep it).
func sendMergeData(k int) map[string]string {
	src := sjMergeTable[k%len(sjMergeTable)]
	out := make(map[string]string, len(src))
	for name, v := range src {
		out[name] = v
	}
	return out
}
