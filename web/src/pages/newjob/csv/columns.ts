// What the column picker decides from a file's header: which column is the address, which others become merge fields, and what in the file the
// platform will refuse (so the user hears it before the upload, in words, instead of from a 400).
//
// The rules are Docs/jobs-api.md, "Create a draft": the chosen header must exist once, no column may be unnamed or repeated, every row has as many cells as
// the header, and a column called `email` or `unsubscribe_url` may not sit beside an address column that is not `email` (the platform supplies those two
// names itself). The platform stays the judge: this only says early what it will certainly say.
import { MAX_CELL_CHARS, type CsvHead } from './reader.ts'

/** The most rows one job holds (PLATFORM_JOB_MAX_ROWS is at most this) and the most bytes of one upload. */
export const MAX_ROWS = 100_000
export const MAX_UPLOAD_BYTES = 64 * 1024 * 1024

/** The two names the platform fills in itself for every row of a send. */
export const RESERVED_FIELDS = ['email', 'unsubscribe_url'] as const

const norm = (name: string) => name.trim().toLowerCase()

/** The column to pre-select as the address: the one named email, else the first whose name contains "mail"; null when there is none (the user chooses). */
export function suggestEmailColumn(header: readonly string[]): number | null {
  const names = header.map(norm)
  const exact = names.indexOf('email')
  if (exact !== -1) return exact
  const like = names.findIndex((n) => n.includes('mail'))
  return like === -1 ? null : like
}

/** A merge field a template can name: letters, digits and underscores, starting with a letter or an underscore, at most 64 (Docs/templates.md). */
export function isFieldName(name: string): boolean {
  return /^[A-Za-z_][A-Za-z0-9_]{0,63}$/.test(name)
}

export interface MergeColumn {
  index: number
  /** the header as it will be named */
  name: string
  /** false: a message cannot name it (a space, a dash, a leading digit...) */
  usable: boolean
}

/** Every column but the address: the merge fields they will become. */
export function mergeColumns(header: readonly string[], addressIndex: number | null): MergeColumn[] {
  const out: MergeColumn[] = []
  header.forEach((raw, index) => {
    if (index === addressIndex) return
    const name = raw.trim()
    out.push({ index, name, usable: isFieldName(name) })
  })
  return out
}

/** What is wrong with the file, as data (the words are copy: newjob.problem.*). */
export type CsvProblem =
  | { kind: 'empty' }
  | { kind: 'noRows' }
  | { kind: 'tooLarge' }
  | { kind: 'tooManyRows'; count: number }
  | { kind: 'unnamedColumn'; position: number }
  | { kind: 'repeatedColumn'; name: string }
  | { kind: 'headerTooLong' }
  | { kind: 'raggedRows'; count: number; line: number | null }
  | { kind: 'unclosedQuote' }
  | { kind: 'addressTwice'; name: string }
  | { kind: 'reservedColumn'; name: string }

/** Every reason this file cannot be uploaded as it is. `chosen` is the address column (null: not chosen yet, which is not a problem of the file). */
export function csvProblems(head: CsvHead, chosen: number | null, fileBytes: number): CsvProblem[] {
  const out: CsvProblem[] = []
  if (fileBytes > MAX_UPLOAD_BYTES) out.push({ kind: 'tooLarge' })
  if (head.header.length === 0) {
    out.push({ kind: 'empty' })
    return out
  }
  if (head.header.some((h) => h.length >= MAX_CELL_CHARS)) out.push({ kind: 'headerTooLong' })
  if (head.totalRows === 0) out.push({ kind: 'noRows' })
  if (head.totalRows > MAX_ROWS) out.push({ kind: 'tooManyRows', count: head.totalRows })

  const seen = new Set<string>()
  const repeated = new Set<string>()
  head.header.forEach((raw, i) => {
    const name = raw.trim()
    if (name === '') {
      out.push({ kind: 'unnamedColumn', position: i + 1 })
    } else if (seen.has(name)) {
      if (!repeated.has(name)) out.push({ kind: 'repeatedColumn', name })
      repeated.add(name)
    }
    seen.add(name)
  })

  if (head.raggedRows > 0) out.push({ kind: 'raggedRows', count: head.raggedRows, line: head.firstRaggedLine })
  if (head.unclosedQuote) out.push({ kind: 'unclosedQuote' })

  if (chosen !== null) {
    const name = norm(head.header[chosen] ?? '')
    if (name !== '') {
      const twice = head.header.some((h, i) => i !== chosen && norm(h) === name)
      if (twice) out.push({ kind: 'addressTwice', name: (head.header[chosen] ?? '').trim() })
      if (name !== 'email') {
        const clash = head.header.find((h, i) => i !== chosen && (RESERVED_FIELDS as readonly string[]).includes(norm(h)))
        if (clash !== undefined) out.push({ kind: 'reservedColumn', name: clash.trim() })
      }
    }
  }
  return out
}

/** The label of a column in the picker: its name, or "Column 3" for an unnamed one. */
export function columnLabel(raw: string, index: number, unnamed: (position: number) => string): string {
  const name = raw.trim()
  return name === '' ? unnamed(index + 1) : name
}
