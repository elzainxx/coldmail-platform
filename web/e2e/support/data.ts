// Lists for the specs: the CSV files of web/e2e/fixtures/ (their contents and what the stack answers for each address are in web/e2e/README.md) and lists made on the spot.
//
// The users are SHARED by the whole run (the balances, the suppression list and the jobs of user a are the same for every spec of the run), so a spec that sends makes its own recipients with `recipients()`:
// an address no other spec uses cannot be suppressed by another spec's unsubscribe, and the sink can be read for it alone.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const FIXTURES = fileURLToPath(new URL('../fixtures/', import.meta.url))

/** The path of a fixture file (for `setInputFiles`). */
export const fixturePath = (name: string): string => `${FIXTURES}${name}`

/** The text of a fixture file. */
export const fixtureText = (name: string): string => readFileSync(fixturePath(name), 'utf8')

/** `count` distinct recipients the sink accepts, unique to `tag` (`<tag>-01@example.test`...): the lower case letters, digits and hyphens of a tag are all it takes. */
export function recipients(tag: string, count: number, domain = 'example.test'): string[] {
  if (!/^[a-z0-9][a-z0-9-]*$/.test(tag)) throw new Error(`the recipient tag '${tag}' must be lower case letters, digits and hyphens`)
  return Array.from({ length: count }, (_, i) => `${tag}-${String(i + 1).padStart(2, '0')}@${domain}`)
}

/** A CSV document (the platform takes `text/csv`): a header row and the rows, a field quoted when it holds a comma, a quote or a line break. */
export function csv(header: string[], rows: string[][]): string {
  const cell = (v: string): string => (/[",\r\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v)
  return [header, ...rows].map((r) => r.map(cell).join(',')).join('\r\n') + '\r\n'
}

/** The CSV of a send list: `email`, `first_name`, `topic` (the merge fields of the fixtures' campaigns) for the given recipients; a blank name or topic is a blank field (the template's fallback applies). */
export function sendCsv(addresses: string[], names: Array<[string, string]> = []): string {
  return csv(['email', 'first_name', 'topic'], addresses.map((a, i) => [a, names[i]?.[0] ?? '', names[i]?.[1] ?? '']))
}

/** A file for `page.setInputFiles`: a name, a type and the bytes. */
export function csvFile(name: string, text: string): { name: string; mimeType: string; buffer: Buffer } {
  return { name, mimeType: 'text/csv', buffer: Buffer.from(text, 'utf8') }
}
