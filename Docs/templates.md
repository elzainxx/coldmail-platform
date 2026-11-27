# Templates: merge fields, spintax and the plain-text part

A message has three templates: the **subject**, the **HTML body** and the **text body** (leave the text body blank and it is made from the HTML). All three use the same small language, implemented by `internal/render` (ruling: [`phase-5-spec.md`](phase-5-spec.md), decisions 1 to 4). This page is the whole grammar.

## At a glance

| You write | You get |
|---|---|
| `Hi {{first_name}}` | the row's `first_name` |
| `Hi {{first_name\|there}}` | the row's `first_name`, or `there` when the row has none |
| `{Hello\|Hi\|Hey}` | one of the three, the same one every time for the same recipient |
| `{Hi\|Hello} {{first_name\|there}}` | both at once |
| `\{`, `\}`, `\|` and `\\` | a plain brace, bar or backslash, never part of the language |

## Merge fields

A field is `{{name}}`. The name is made of letters, digits and underscores, starts with a letter or an underscore, and is at most 64 characters (`first_name`, `_x`, `Company2`). Names are case sensitive. No spaces: `{{ first_name }}` is an error.

A **fallback** follows a bar: `{{first_name|there}}`. It is plain template text up to the closing `}}`. It may hold anything except a field or a group, and a brace in it must be escaped. It is written exactly as you typed it (it is not HTML-escaped: it is your text, not the recipient's data). Everything after the first bar is the fallback, so `{{x|a|b}}` has the fallback `a|b`. An empty fallback, `{{note|}}`, is allowed and means "nothing when the row has no note".

`{{a|b}}` is therefore a field named `a` with the fallback `b`. It is never a choice between `a` and `b`; `{{` always opens a field.

### What "missing" means

A field is **missing** for a row when the row has no such column, or the value is empty after trimming spaces and line breaks (a cell holding only spaces is missing). Then:

- a field **with** a fallback renders the fallback, and the row is sent;
- a field **without** a fallback makes the row **skipped, and free**: the preview counts such rows and lists the field names, and confirming the job leaves them out.

If a name appears several times and any use has no fallback, the field is required. A field inside one option of a group counts even when that option is not the one chosen for a given recipient (otherwise the same row would be sent or skipped depending on its draw).

### The two reserved fields

`{{email}}` (the recipient) and `{{unsubscribe_url}}` (that recipient's own unsubscribe link) are always available and never missing. Put `{{unsubscribe_url}}` where you want the link; when no template uses it, the platform adds a footer link itself.

## Spintax

A group is `{a|b|c}`: at least two options, at most 64, separated by bars. One option is picked for each recipient. Options may hold text, fields and other groups, to a depth of 8:

```
{Hi|Hello|Hey} {{first_name|there}}, {I {saw|read} your {post|note}|your {work|writing} stood out}.
```

An option may be empty: `{a|}` is either `a` or nothing. A group of one option, `{a}`, is an error, because it is almost always a mistake.

One limit follows from "`{{` always opens a field": an option cannot start with a field written right after the brace (`{{{x}}|b}`). Put text before it, or put the field in another option (`{b|{{x}}}`).

## Escapes, and why braces in CSS and scripts must be escaped

A backslash makes the next `{`, `}`, `|` or `\` a plain character: `\{` `\}` `\|` `\\`. Any other backslash is just a backslash (`\n` stays `\n`).

Braces are the language's own characters, so a stray one is an **error**, not text. This matters most in HTML: write CSS and script blocks with escaped braces.

```
<style>p \{ color: red \}</style>
```

A literal `{{x}}` in the output is written `\{\{x\}\}`. A bar outside any group or field is plain text (`Q&A | FAQ` needs nothing).

## Values are never interpreted

What a row holds is inserted **as data**: a value of `{{x}}`, `{a|b}` or `\{` appears exactly so, and is never read as a field or a group. In the HTML body the five characters `& < > " '` become character references, so a value can never add a tag or break out of an attribute. In the text body the value is inserted as it is. In the subject it is inserted as it is, then every control character (line breaks and tabs included) becomes a space and the result is trimmed, so a value cannot end the subject line or add a header.

## The same job and recipient always render the same

Choices are not random. The seed of one message is the first 8 bytes of SHA-256 of `job_id`, a colon, and `item_id`, fed to a fixed generator (SplitMix64) that draws once per group, in the order the groups appear, and only for groups that are reached (a group inside an option that was not picked draws nothing). Consequences:

- a retry, a restart or a second instance renders **byte for byte** the same message;
- the preview sample of a row is exactly what will be sent to it;
- the HTML and text bodies use the same stream; the subject has its own, so a subject group does not move with a body group;
- editing a template changes what it renders; nothing else does.

## The text part

When the text template is blank, the text body is made from the rendered HTML: paragraphs and headings are separated by a blank line, `<br>` is a line break, list items start with `- `, a link `<a href="https://x">Text</a>` becomes `Text (https://x)` (just the address when the text is the address; a relative or `javascript:` link keeps only its text), scripts, styles and comments are dropped, entities are decoded and runs of spaces and blank lines are collapsed. Preformatted text is not kept as it is and nested lists are not indented.

## Errors

A template with a mistake is refused where it is saved, with a position (line and column, counted in characters): an unclosed `{` or `{{`; a stray `}`; a group of fewer than two options or more than 64; nesting deeper than 8; an empty or invalid field name or one longer than 64; a field closed with one brace; a brace inside a fallback.

## In a job

This is what the platform does with the three templates of a send job (modules 2 and 3). A verify job (module 1) has no templates: nothing here applies to it, and its rows and counts are the same as before.

### When you save the job

The three templates are compiled when the job is created (`POST /v1/jobs`). A mistake is `400 validation_failed` and nothing is written: the job, its rows and its events do not exist. The answer names where the mistake is and what it is, and never repeats your text (the message may quote one character of it):

```json
{"error": {"code": "validation_failed", "message": "The request is not valid.", "request_id": "…",
  "details": {"reason": "the html template has a syntax error", "part": "html", "line": 2, "column": 5,
              "message": "unexpected }: write \\} for a literal brace"}}}
```

`part` is `subject`, `html` or `text` (the subject is judged first, then the HTML, then the text; a blank text template is not compiled). `line` and `column` start at 1 and count characters. A job's content is frozen once it is saved: there is no way to change it, so a draft that was saved before the templates existed and holds a stray brace answers the same 400 when it is previewed, and has to be created again.

### When you preview it

The preview shows what a confirm would do, row by row, without writing a row:

| Member | Meaning |
|---|---|
| `missing_fields` | how many pending rows lack a merge field the templates need and give no fallback for (the rows a confirm leaves out). The suppression list is applied first, so a row that is on it counts as suppressed and not here |
| `missing_field_names` | the names of those fields, sorted, never a value; at most 50. `missing_field_names_truncated` is `true` when there were more |
| `unrenderable` | how many of the other pending rows render to a message the platform cannot send (see below) |
| `units`, `credits` | the rows that would be sent: pending, minus suppressed, minus the two counts above. A list that is skipped entirely is a valid preview (`units` 0, `credits` 0), and confirming it is `422 nothing_to_do`, like a list that is entirely suppressed |
| `samples` | up to three real rows (the first, the middle and the last of the rows that would be sent, by position), each `{"ordinal", "subject", "html", "text"}`, rendered **exactly as they will be sent**: the same job and the same row give the same message, so the sample of a row is the message that row will get, link and footer included |

The samples hold your recipients' data. They are in the answer of the preview request and **nowhere else**: they are not stored with the job (a later `GET` of the job does not have them) and they are not part of the `fingerprint`. The fingerprint does name everything the counts and the samples depend on: the templates and every row's merge data. Change one value of one row and the fingerprint changes, so a confirm of the preview you saw is `409 preview_stale`.

### When you confirm it

The confirm leaves out, in the same transaction as the suppression list and before the credits are held, the rows its templates cannot be rendered for. They are **free**: not reserved, never given to a module, never sent, never charged. Each gets a result of its own, with its position and its state, in the results and in the CSV:

| State | Rule | Reason | When |
|---|---|---|---|
| `missing_field` | `P-09` | the names of the missing fields, separated by commas (`first_name,plan`), never a value | the row lacks a field its templates need and give no fallback for |
| `unrenderable` | `P-10` | one word: `subject_blank`, `subject_too_long`, `subject_not_one_line`, `html_too_large`, `text_too_large`, `no_body`, `invalid_text` or `renderer_refused` | the message the templates make for the row is one the platform refuses: a subject that renders blank or over 998 bytes, a part over 1 MiB, no body at all, text that is not valid UTF-8 |

A send **chained** to a verify job takes its rows from that job's results, and those rows have an address and no merge data: a template that needs a field without a fallback leaves every row out (and the confirm is `422 nothing_to_do`). Use fallbacks (`{{first_name|there}}`) or only `{{email}}` there.

The answer of the confirm says how many (`missing_fields`, `unrenderable`), next to `pending`, `suppressed` and `reserved`; a repeat of the same confirm (same `Idempotency-Key`) answers with the same numbers. A confirm whose every row is left out is `422 nothing_to_do` and writes nothing.

A row is judged by **rendering it once** during the confirm, exactly as it will be rendered when the job is sent. Without that, one row whose message the platform refuses would fail the whole job after its credits were held (free for you, but the job would be dead); with it only that row is left out. The work is the rendering of every row, done in pages inside the confirm: for a 100,000-row list, plan for a few seconds.

### The unsubscribe footer

Every part a recipient can read needs a way to stop receiving the messages. The rule is **per part**: for the HTML, and for the text when you wrote a text template, if **that** template does not place `{{unsubscribe_url}}`, the platform adds a footer to that part, always the same words:

- to the HTML, a paragraph before the closing `</body>` (or at the end when there is none): `To stop receiving these messages, <a href="LINK">unsubscribe</a>.`
- to an explicit text template, a blank line and `To stop receiving these messages, unsubscribe: LINK`;
- a text made from the HTML (a blank text template) comes from the **final** HTML, so it has the link exactly when the HTML does (your own link or the footer's), once and never twice.

So an HTML that places the link next to an explicit text that does not gets no second link in the HTML and a footer line in the text, and the reverse; when both place it nothing is added; when neither does, both get the footer. A campaign with no HTML part (text only) gets the text footer and no HTML. The subject never gets a footer, and a subject that uses `{{unsubscribe_url}}` is allowed and decides nothing for the other parts. The link is the recipient's own (it names the item, never the address) and is the one the platform hands the sending module for that item (`public.job_items.unsubscribe_url`, see [`unsubscribe.md`](unsubscribe.md)).

### What a recipient gets

For each recipient that is not left out: the subject, the HTML and the text of that row, with the row's own choices from every group and the row's own values; the reply-to of the job; and the footer when it applies. The sending modules put the same link in the `List-Unsubscribe` headers when they support them ([`unsubscribe.md`](unsubscribe.md)). A retry, a restart, a second instance of the platform and the preview all give the same bytes for the same recipient.

## For code

`render.Compile(src)` returns a `*render.SyntaxError{Offset, Line, Column, Message}` or a `*Template`; `Fields()` lists the fields (unique, in order of first use, with `HasFallback`); `Missing(values)` lists the required fields a row lacks; `Render(seed, part, values)` renders `PartHTML`, `PartText` or `PartSubject`; `SpinSeed(jobID, itemID)` is the seed; `HTMLToText` makes the text part. The job store renders through `jobs.TemplateRenderer` (a `jobs.Preparer`, installed with `jobs.WithPreparer`), which compiles a campaign once per job per call, adds the footer and checks the three templates when a job is created.
