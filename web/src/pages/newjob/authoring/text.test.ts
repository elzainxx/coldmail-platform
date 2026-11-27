import { describe, expect, it } from 'vitest'
import { replyToValid } from './message.ts'
import { applyInsert, displayHtml, errorExcerpt, escapeFallback, fieldSnippet } from './text.ts'

describe('the snippets the palette inserts', () => {
  it('writes a field, with a fallback when there is one', () => {
    expect(fieldSnippet('first_name', '')).toBe('{{first_name}}')
    expect(fieldSnippet('first_name', 'there')).toBe('{{first_name|there}}')
    expect(fieldSnippet('first_name', '   ')).toBe('{{first_name}}') // a fallback of spaces is no fallback
    expect(fieldSnippet('first_name', ' dear friend ')).toBe('{{first_name|dear friend}}')
  })

  it('escapes what the grammar would read as its own in a fallback, and leaves a bar alone (everything after the first bar is the fallback)', () => {
    expect(escapeFallback('a{b}c')).toBe('a\\{b\\}c')
    expect(escapeFallback('back\\slash')).toBe('back\\\\slash')
    expect(escapeFallback('Q&A | FAQ')).toBe('Q&A | FAQ')
    expect(fieldSnippet('x', 'a{b')).toBe('{{x|a\\{b}}')
    // control: a plain fallback is untouched
    expect(escapeFallback('hello there')).toBe('hello there')
  })
})

describe('where an insert goes in the text', () => {
  it('replaces the selection with a field, or inserts it at the caret, and leaves the caret after it', () => {
    expect(applyInsert('Hi NAME, hello', 3, 7, { before: '{{name}}', replace: true })).toEqual({ value: 'Hi {{name}}, hello', start: 11, end: 11 })
    expect(applyInsert('Hi ', 3, 3, { before: '{{name}}', replace: true })).toEqual({ value: 'Hi {{name}}', start: 11, end: 11 })
  })

  it('wraps the selected text in a tag and keeps it selected; with nothing selected it leaves the caret inside the tag', () => {
    expect(applyInsert('a big word', 2, 5, { before: '<strong>', after: '</strong>' })).toEqual({ value: 'a <strong>big</strong> word', start: 10, end: 13 })
    expect(applyInsert('ab', 1, 1, { before: '<em>', after: '</em>' })).toEqual({ value: 'a<em></em>b', start: 5, end: 5 })
  })

  it('puts a placeholder where there is no selection, selected so that typing replaces it, and does not use it when there is a selection', () => {
    const empty = applyInsert('', 0, 0, { before: '<a href="https://">', after: '</a>', placeholder: 'link text' })
    expect(empty.value).toBe('<a href="https://">link text</a>')
    expect(empty.value.slice(empty.start, empty.end)).toBe('link text')
    const picked = applyInsert('see here now', 4, 8, { before: '<a href="https://">', after: '</a>', placeholder: 'link text' })
    expect(picked.value).toBe('see <a href="https://">here</a> now')
  })

  it('takes a backwards selection and one past the end of the text as they are', () => {
    expect(applyInsert('abcdef', 4, 2, { before: '<b>', after: '</b>' }).value).toBe('ab<b>cd</b>ef')
    expect(applyInsert('abc', 99, 99, { before: '!', replace: true }).value).toBe('abc!')
  })
})

describe('what the screen\'s own preview makes of the escapes', () => {
  it('reads an escaped brace, bar or backslash as the plain character, so a style block written for the grammar looks as it will', () => {
    expect(displayHtml('<style>p \\{ color: red \\}</style>')).toBe('<style>p { color: red }</style>')
    expect(displayHtml('a \\| b \\\\ c')).toBe('a | b \\ c')
  })

  it('leaves a merge field, a choice and any other backslash as typed', () => {
    expect(displayHtml('Hi {{name|there}}, {Hello|Hi} \\n')).toBe('Hi {{name|there}}, {Hello|Hi} \\n')
  })
})

describe('where a mistake is shown', () => {
  it('shows the line the mistake is on and the index of its column in it (1-based line and column, in characters)', () => {
    expect(errorExcerpt('first line\nHi {{name', 2, 4)).toEqual({ text: 'Hi {{name', caret: 3 })
    expect(errorExcerpt('Hi {{name', 1, 4)).toEqual({ text: 'Hi {{name', caret: 3 })
    expect(errorExcerpt('a\r\nb {', 2, 3)).toEqual({ text: 'b {', caret: 2 }) // a CRLF ends a line
  })

  it('counts characters, not UTF-16 units, so the marker stays under the right place after an emoji', () => {
    const e = errorExcerpt('😀😀 {{x', 1, 4)
    expect(e).toEqual({ text: '😀😀 {{x', caret: 3 })
  })

  it('cuts a long line to a window around the column and says so with an ellipsis at the cut end', () => {
    const line = 'x'.repeat(200) + '{{oops' + 'y'.repeat(200)
    const e = errorExcerpt(line, 1, 201, 40)
    expect(e).not.toBeNull()
    expect(e?.text.length).toBeLessThanOrEqual(42)
    expect(e?.text.startsWith('…')).toBe(true)
    expect(e?.text.endsWith('…')).toBe(true)
    // the marker is under the first brace
    expect(e?.text[e.caret]).toBe('{')
    // control: a short line is not cut
    expect(errorExcerpt('short {{', 1, 7, 40)?.text).toBe('short {{')
  })

  it('shows tabs as spaces so the marker lines up, and has nothing for a line the template does not have', () => {
    expect(errorExcerpt('\t{{x', 1, 2)).toEqual({ text: ' {{x', caret: 1 })
    expect(errorExcerpt('one line', 5, 1)).toBeNull()
  })
})

describe('the reply-to address', () => {
  it('is one address, or nothing', () => {
    expect(replyToValid('')).toBe(true)
    expect(replyToValid('  ')).toBe(true)
    expect(replyToValid('me@example.test')).toBe(true)
    expect(replyToValid('me@example')).toBe(false)
    expect(replyToValid('me@@example.test')).toBe(false)
    expect(replyToValid('a@x.test, b@x.test')).toBe(false)
    expect(replyToValid('Me <me@example.test>')).toBe(false)
  })
})
