// What a bulk add sends. The first line of the box (and of a file made from it) names the columns in the screen's own words ("Mail server,Port,..."); the API names
// them by the members of an account, so that line is replaced on the way out, and nothing else about the text changes: every other line, and the number of every line,
// is the user's. A first line that is not the screen's header (a file that already has the API's names, a different order) is sent as it is, and the API judges it.
import { t } from '../../copy/index.ts'
import { BULK_HEADER } from './accountForm.ts'

const normal = (line: string): string =>
  line
    .replace(/^\uFEFF/, '')
    .split(',')
    .map((cell) => cell.trim().toLowerCase())
    .join(',')

/** The header line the box starts with: the words of the form's own fields, in the order the columns go. */
export const screenHeader = (): string => t('accounts.bulk.header')

/** Does this line name the columns the way the screen does? */
export const isScreenHeader = (line: string): boolean => normal(line) === normal(screenHeader())

/** Is there an account in this text, or only a header (or nothing)? */
export function hasAccountLines(text: string): boolean {
  const lines = text.split(/\r?\n/).filter((l) => l.trim() !== '')
  if (lines.length === 0) return false
  return lines.length > 1 || !(isScreenHeader(lines[0] ?? '') || normal(lines[0] ?? '') === BULK_HEADER)
}

/** The pasted text with the screen's header line replaced by the API's. */
export function apiText(text: string): string {
  const end = text.search(/\r?\n/)
  const first = end === -1 ? text : text.slice(0, end)
  if (!isScreenHeader(first)) return text
  return BULK_HEADER + (end === -1 ? '' : text.slice(end))
}

/** A chosen file with its first line replaced the same way. Only the first kilobyte is read; the rest of the file is handed on as it is. */
export async function apiBlob(file: Blob): Promise<Blob> {
  const head = new TextDecoder('utf-8', { ignoreBOM: true }).decode(await file.slice(0, 1024).arrayBuffer())
  const end = head.search(/\r?\n/)
  const first = end === -1 ? head : head.slice(0, end)
  if (!isScreenHeader(first)) return file
  return new Blob([BULK_HEADER, file.slice(new TextEncoder().encode(first).length)], { type: 'text/csv' })
}
