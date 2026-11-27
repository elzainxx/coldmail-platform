// "No internal text reaches a screen" (chunk A11, decision 5): the text a person can read or hear on a page must hold none of the words of the deny-list, which is web/src/copy/internal-words.json (the words
// of the platform's own machinery: module names, table and column names, status names of the engine, error codes). `await expectNoInternalWords(page)` after a screen has settled.
//
// WHAT IS READ: the visible text of the page's own document (not its scripts or styles), its title, and the attributes a screen reader speaks (aria-label, title, alt, placeholder, the value of a button).
// The CONTENT of an iframe is not read: the sandboxed preview shows a user's own HTML, which may say anything. A word matches as a WHOLE word, case-insensitively (`ledger` is found in "the Ledger page" and not in "ledgers" or "smudgers"),
// so a deny-list word is never a substring trap.
//
// THE LIST: a JSON array of strings, or an object with a `words` array (the file may carry other members: a note, a generator's header). It is read when the helper runs, never at import. An empty list, or
// words that are not strings, fail (a deny-list that denies nothing is red).
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import type { Page } from '@playwright/test'

const WORDS_FILE = fileURLToPath(new URL('../../src/copy/internal-words.json', import.meta.url))

/** The deny-list, lower case, without repeats. */
export function internalWords(file: string = WORDS_FILE): string[] {
  const raw: unknown = JSON.parse(readFileSync(file, 'utf8'))
  const list: unknown = Array.isArray(raw) ? raw : (raw as { words?: unknown } | null)?.words
  if (!Array.isArray(list) || list.length === 0 || !list.every((w): w is string => typeof w === 'string' && w.trim() !== '')) {
    throw new Error(`${file} must be a non-empty array of words, or an object with a non-empty "words" array of strings: a deny-list that denies nothing proves nothing`)
  }
  return [...new Set(list.map((w) => w.trim().toLowerCase()))]
}

/** Every word of the list found in the text, with a little context (the text is the page's own: it is shown only to say where the word is). */
export function findInternalWords(text: string, words: string[]): Array<{ word: string; context: string }> {
  const found: Array<{ word: string; context: string }> = []
  for (const word of words) {
    const escaped = word.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
    const m = new RegExp(`(?<![A-Za-z0-9_])${escaped}(?![A-Za-z0-9_])`, 'i').exec(text)
    if (m) found.push({ word, context: text.slice(Math.max(0, m.index - 24), m.index + word.length + 24).replace(/\s+/g, ' ') })
  }
  return found
}

/** What a person reads or hears on the page: its visible text, its title and the speakable attributes. */
export async function readablePage(page: Page): Promise<string> {
  return page.evaluate(() => {
    const out: string[] = [document.title]
    const skip = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'IFRAME'])
    const walk = (node: Node): void => {
      if (node.nodeType === Node.TEXT_NODE) {
        const t = node.textContent?.trim()
        if (t) out.push(t)
        return
      }
      if (node.nodeType !== Node.ELEMENT_NODE) return
      const el = node as Element
      if (skip.has(el.tagName)) return
      for (const a of ['aria-label', 'title', 'alt', 'placeholder']) {
        const v = el.getAttribute(a)
        if (v) out.push(v)
      }
      if (el instanceof HTMLInputElement && (el.type === 'button' || el.type === 'submit') && el.value) out.push(el.value)
      for (const child of Array.from(el.childNodes)) walk(child)
    }
    walk(document.body)
    return out.join('\n')
  })
}

export async function expectNoInternalWords(page: Page, words: string[] = internalWords()): Promise<void> {
  const text = await readablePage(page)
  if (text.trim() === '') throw new Error('the page holds no readable text: the copy check proved nothing')
  const found = findInternalWords(text, words)
  if (found.length > 0) {
    throw new Error(`${found.length} internal word(s) reach the page:\n${found.map((f) => `  - '${f.word}' in "${f.context}"`).join('\n')}`)
  }
}
