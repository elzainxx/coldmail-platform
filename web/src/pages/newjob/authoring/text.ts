// The plain-text work of the authoring screen: the snippets the palette and the toolbar insert, where they go in a textarea, how a mistake is
// shown under the field, and what the live preview makes of the grammar's escapes. All of it pure, so each piece has a test.
//
// The grammar is Docs/templates.md: {{name}} and {{name|fallback}} are merge fields, {a|b} is a choice, and a backslash makes the next { } | or \ a plain character.

/** The text of a fallback with the characters the grammar would read as its own written as plain ones (a brace or a backslash is escaped; a bar after the first one is plain). */
export function escapeFallback(text: string): string {
  return text.replace(/[\\{}]/g, (c) => '\\' + c)
}

/** `{{name}}`, or `{{name|fallback}}` when a fallback is given. The name must already be a valid field name (csv/columns.ts isFieldName). */
export function fieldSnippet(name: string, fallback: string): string {
  const fb = fallback.trim()
  return fb === '' ? `{{${name}}}` : `{{${name}|${escapeFallback(fb)}}}`
}

/** What to put in a text: `before` + the selection (or the placeholder when nothing is selected) + `after`; `replace` puts `before` in place of the selection instead. */
export interface Snippet {
  before: string
  after?: string
  placeholder?: string
  replace?: boolean
}

export interface Edit {
  value: string
  /** where the selection starts and ends after the edit */
  start: number
  end: number
}

export function applyInsert(value: string, selectionStart: number, selectionEnd: number, snippet: Snippet): Edit {
  const length = value.length
  const a = Math.max(0, Math.min(selectionStart, selectionEnd, length))
  const b = Math.max(0, Math.min(Math.max(selectionStart, selectionEnd), length))
  if (snippet.replace === true) {
    const caret = a + snippet.before.length
    return { value: value.slice(0, a) + snippet.before + value.slice(b), start: caret, end: caret }
  }
  const selected = value.slice(a, b)
  const inner = selected !== '' ? selected : (snippet.placeholder ?? '')
  const innerStart = a + snippet.before.length
  return {
    value: value.slice(0, a) + snippet.before + inner + (snippet.after ?? '') + value.slice(b),
    start: innerStart,
    end: innerStart + inner.length,
  }
}

/**
 * What the screen's own preview shows for the HTML: the text as typed, with the grammar's escapes read (a backslash before a brace, a bar or another backslash
 * goes, and the character stays), so a style block written with escaped braces looks the way it will. A {{field}} or a {a|b} choice stays as typed: the real
 * renders are the samples at the preview step.
 */
export function displayHtml(html: string): string {
  return html.replace(/\\([{}|\\])/g, '$1')
}

export interface Excerpt {
  /** the line of the template (or a window of it), tabs as spaces */
  text: string
  /** where the marker goes: a character index into `text` */
  caret: number
}

/** The line a mistake is on, cut to a window around its column, with the index of the column in it. Null when the template has no such line. Lines and columns count characters from 1. */
export function errorExcerpt(source: string, line: number, column: number, width = 72): Excerpt | null {
  const raw = source.split(/\r\n|\n/)[line - 1]
  if (raw === undefined) return null
  const chars = Array.from(raw.replace(/\t/g, ' '))
  const col = Math.max(0, Math.min(column - 1, chars.length))
  const from = chars.length <= width ? 0 : Math.max(0, Math.min(col - Math.floor(width / 2), chars.length - width))
  const window = chars.slice(from, from + width)
  const lead = from > 0 ? '…' : ''
  const tail = from + width < chars.length ? '…' : ''
  return { text: lead + window.join('') + tail, caret: lead.length + (col - from) }
}
