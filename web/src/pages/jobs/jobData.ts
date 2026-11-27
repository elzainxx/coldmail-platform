// What a result row says in words (what it cost, the one detail that goes with it), and the reads that take more than one request (a job's whole history, a count of its results).
// The API's `rule`, `origin` and `reason` words are never shown: a reason is turned into a sentence (copy/words.ts) or left out.
import { jobsApi } from '../../api/endpoints.ts'
import type { JobEvent, JobResult } from '../../api/types.ts'
import { maybeT, t } from '../../copy/index.ts'
import { malformedReasonText, missingFieldNames, resultReasonText } from '../../copy/words.ts'

/** "Free", "Charged", "Charged (second lookup)", "Not charged": from the decision, and the unit of a charge. */
export function costText(r: Pick<JobResult, 'decision' | 'unit'>): string {
  switch (r.decision) {
    case 'free':
      return t('jobs.cost.free')
    case 'charge':
      return (r.unit === null ? null : maybeT(`jobs.cost.unit.${r.unit}`)) ?? t('jobs.cost.charged')
    case 'unmapped':
      return t('jobs.cost.notCharged')
    default:
      return t('jobs.cost.other')
  }
}

/** The lines that go with a result: why it came out so, the names of what a row lacked, and a note when the address is on the user's suppression list. */
export function detailLines(r: Pick<JobResult, 'state' | 'reason' | 'suppressed'>): string[] {
  const lines: string[] = []
  const reason = r.reason === null || r.reason === '' ? null : r.reason
  if (r.state === 'missing_field') {
    // the reason lists the user's own column names: data, not a word of ours
    const names = missingFieldNames(r.reason)
    if (names.length > 0) lines.push(t('jobs.results.missingFields', { fields: names.join(', ') }))
  } else if (r.state === 'malformed') {
    // the reason is a word of the address rule's closed list
    if (reason !== null) lines.push(malformedReasonText(reason))
  } else if (r.state === 'excluded') {
    // the reason is the verdict of the earlier check that left the address out ("invalid", "unknown")
    const verdict = reason === null ? null : maybeT(`common.resultState.${reason}`)
    if (verdict !== null) lines.push(t('jobs.results.excluded', { verdict: verdict.toLowerCase() }))
  } else {
    const sentence = resultReasonText(r.reason, r.state)
    if (sentence !== null) lines.push(sentence)
  }
  if (r.suppressed) lines.push(t('jobs.results.suppressed'))
  return lines
}

/** The distinct outcomes of the results on a page, in the order they first appear (the choices of the filter). */
export function outcomesOf(results: readonly Pick<JobResult, 'state'>[]): string[] {
  return [...new Set(results.map((r) => r.state))]
}

// ---- the whole history of a job ----

const EVENT_PAGE = 1000
const EVENT_PAGES_MAX = 5

/** Every event of a job, oldest first (a job has a handful; the cap keeps a runaway history from looping). */
export async function loadEvents(id: string, signal: AbortSignal): Promise<JobEvent[]> {
  const all: JobEvent[] = []
  let after: number | undefined
  for (let page = 0; page < EVENT_PAGES_MAX; page++) {
    const answer = await jobsApi.events(id, { after, limit: EVENT_PAGE }, { signal })
    all.push(...answer.events)
    if (answer.next_after === null) break
    after = answer.next_after
  }
  return all
}

// ---- how far a job has got ----

/** How many results a count looks at: the platform gives no total of a job's results, so a count walks the pages, and stops here. */
export const COUNT_PAGES_MAX = 10

export interface ResultCount {
  /** the results seen */
  count: number
  /** false: the walk stopped at the cap, the job has more */
  complete: boolean
}

/** Count a job's results by walking its pages (the platform's default page size), at most COUNT_PAGES_MAX of them. */
export async function countResults(id: string, signal: AbortSignal): Promise<ResultCount> {
  let count = 0
  let cursor: string | undefined
  for (let page = 0; page < COUNT_PAGES_MAX; page++) {
    const answer = await jobsApi.results(id, { cursor }, { signal })
    count += answer.results.length
    if (answer.next_cursor === null) return { count, complete: true }
    cursor = answer.next_cursor
  }
  return { count, complete: false }
}
