// Small pure helpers of the flows (flows-ui.ts, flows-api.ts, flows-mail.ts): a run id, a CSV reader, the app's number format. Nothing here touches a page, the network or a secret.
import { randomBytes } from 'node:crypto'

/**
 * A short random lower case hex id, new on every call. The users and the sink are shared by the whole run, so a spec that sends makes its recipients unique with it
 * (`recipients('cs' + runId(), 4)`): another spec's unsubscribe, or an earlier attempt of the same spec, cannot touch them.
 */
export function runId(): string {
  return randomBytes(3).toString('hex')
}

/** A number as the app writes it: `12,345` (the catalogue's `fmt` of the job screens, en-US). */
export function fmt(n: number): string {
  return new Intl.NumberFormat('en-US').format(n)
}

/** The digits of a text as a number: "99,980 credits" is 99980, "Credits per message: 2" is 2. A text with no digit is NaN (the caller's assertion then fails by name). */
export function numberIn(text: string): number {
  const digits = text.replace(/[^\d]/g, '')
  return digits === '' ? Number.NaN : Number(digits)
}

/** A string as a literal inside a regular expression. */
export function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/**
 * An RFC 4180 reader: a quoted cell may hold commas, quotes (doubled) and line breaks; a line ends at CRLF or LF; the last line may have no end; a leading byte order mark is ignored.
 * It returns the rows as the file holds them (the header too), never trimmed. The downloaded results are read with it (the platform writes them with Go's encoding/csv).
 */
export function parseCsv(text: string): string[][] {
  const src = text.replace(/^\uFEFF/, '')
  const rows: string[][] = []
  let row: string[] = []
  let cell = ''
  let quoted = false
  let i = 0
  while (i < src.length) {
    const c = src.charAt(i)
    if (quoted) {
      if (c === '"') {
        if (src.charAt(i + 1) === '"') {
          cell += '"'
          i += 2
          continue
        }
        quoted = false
        i += 1
        continue
      }
      cell += c
      i += 1
      continue
    }
    if (c === '"' && cell === '') {
      quoted = true
      i += 1
      continue
    }
    if (c === ',') {
      row.push(cell)
      cell = ''
      i += 1
      continue
    }
    if (c === '\r' || c === '\n') {
      if (c === '\r' && src.charAt(i + 1) === '\n') i += 1
      row.push(cell)
      rows.push(row)
      row = []
      cell = ''
      i += 1
      continue
    }
    cell += c
    i += 1
  }
  if (cell !== '' || row.length > 0) {
    row.push(cell)
    rows.push(row)
  }
  return rows
}

/** One word of a spreadsheet's protection: a leading apostrophe the platform put in front of a cell that starts with = + - @ (not needed for an address, but a reader of a cell must not be fooled by it). */
export function unprotectCell(cell: string): string {
  return /^'[=+\-@\t\r']/.test(cell) ? cell.slice(1) : cell
}

/** A value that a spec knows is there (a fixture's first row, the n-th recipient it just made): `noUncheckedIndexedAccess` makes an index possibly undefined, and this says what was expected when it is not. */
export function must<T>(value: T | undefined | null, what = 'a value the spec made a moment ago'): T {
  if (value === undefined || value === null) throw new Error(`${what} is missing`)
  return value
}
