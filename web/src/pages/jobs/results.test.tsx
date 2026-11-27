import { act, fireEvent, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Job, JobResult } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { saveBlob } from '../../api/download.ts'
import { csvFileName } from './format.ts'
import { jobId, makeJob, makePreview, makeResult } from './fixtures.ts'
import { COUNT_PAGES_MAX } from './jobData.ts'

vi.mock('../../api/download.ts', () => ({ saveBlob: vi.fn() }))

const ME = { 'GET /v1/me': { email: 'ann@example.test' } }
const EVENTS = (id: string) => `GET /v1/jobs/${id}/events`
const RESULTS = (id: string) => `GET /v1/jobs/${id}/results`
const CSV = (id: string) => `GET /v1/jobs/${id}/results.csv`

beforeEach(async () => {
  resetForTest()
  vi.mocked(saveBlob).mockClear()
  await signInForTest()
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => false })
  resetForTest()
})

type Routes = Record<string, unknown>

function failing(status: number, code: string, details?: Record<string, unknown>) {
  return () =>
    new Response(JSON.stringify({ error: { code, message: 'SERVER-SAID-pq_failure', request_id: 'req_0123456789abcdef0123456789abcdef', ...(details === undefined ? {} : { details }) } }), { status })
}

function api(job: Job, results: Routes[string], extra: Routes = {}) {
  return mockApi({
    ...ME,
    [`GET /v1/jobs/${job.id}`]: { job },
    [EVENTS(job.id)]: { events: [], next_after: null },
    [RESULTS(job.id)]: results,
    ...extra,
  } as Parameters<typeof mockApi>[0])
}

async function openJob(job: Job) {
  renderApp(`/jobs/${job.id}`)
  await screen.findByTestId(`job-status-${job.id}`)
}

function page(results: JobResult[], next: string | null = null) {
  return { results, next_cursor: next }
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

describe('the results table', () => {
  it('shows each result with its address, its outcome as a word and a colour, what it cost in words, and a sentence of detail', async () => {
    const job = makeJob()
    api(
      job,
      page([
        makeResult(1, { email: 'ann@example.test', state: 'valid', reason: 'accepted', decision: 'charge', unit: 'm1_own' }),
        makeResult(2, { email: 'bob@example.test', state: 'invalid', reason: 'no_mx', decision: 'charge', unit: 'm1_own' }),
        makeResult(3, { email: 'no-at-sign', origin: 'platform', state: 'malformed', reason: 'no_at', decision: 'free', unit: null, rule: 'P-08' }),
        makeResult(4, { email: 'dup@example.test', origin: 'platform', state: 'duplicate', reason: null, decision: 'free', unit: null, rule: 'P-07' }),
      ]),
    )
    await openJob(job)
    const row1 = await screen.findByTestId('results-row-1')
    expect(within(row1).getByText('ann@example.test')).toBeInTheDocument()
    expect(within(row1).getByText('Valid').closest('[data-status]')).toHaveAttribute('data-status', 'valid')
    expect(within(row1).getByText('Charged')).toBeInTheDocument()
    expect(within(row1).getByText(t('common.reason.accepted'))).toBeInTheDocument()

    const row2 = screen.getByTestId('results-row-2')
    expect(within(row2).getByText('Invalid').closest('[data-status]')).toHaveAttribute('data-status', 'invalid')
    expect(within(row2).getByText(t('common.reason.no_mx'))).toBeInTheDocument()

    const row3 = screen.getByTestId('results-row-3')
    expect(within(row3).getByText('Malformed address')).toBeInTheDocument()
    expect(within(row3).getByText('Free')).toBeInTheDocument() // free, in words
    expect(within(row3).getByText(t('common.malformed.no_at'))).toBeInTheDocument() // the reason of a malformed address is a sentence about the address
    expect(within(screen.getByTestId('results-row-4')).getByText('Repeated address')).toBeInTheDocument()
  })

  it('never shows a rule, an origin, a unit, a reason slug or the word module on the page', async () => {
    const job = makeJob()
    api(
      job,
      page([
        makeResult(1, { reason: 'accepted', unit: 'm1_own', rule: 'M1-03', origin: 'module' }),
        makeResult(2, { origin: 'platform', state: 'malformed', reason: 'no_at', decision: 'free', unit: null, rule: 'P-08' }),
        makeResult(3, { reason: 'a_reason_from_the_future', state: 'invalid' }),
      ]),
    )
    await openJob(job)
    await screen.findByTestId('results-row-3')
    const main = screen.getByRole('main')
    const text = main.textContent ?? ''
    for (const raw of ['M1-03', 'P-08', 'm1_own', 'no_at', 'a_reason_from_the_future', 'module']) expect(text, raw).not.toContain(raw)
    // a bare word of the API is never a cell or a label on its own (the sentences that stand for them may use the same English word)
    const alone = new Set([...main.querySelectorAll('td, th, span, p, dd, dt, option')].map((el) => el.textContent?.trim()))
    for (const raw of ['accepted', 'origin', 'platform', 'module', 'charge', 'decision', 'unit', 'rule']) expect(alone.has(raw), raw).toBe(false)
    // paired control: the sentences that stand for them are there
    expect(text).toContain(t('common.reason.accepted'))
    expect(text).toContain(t('common.reason.other')) // the reason it does not know is "no further detail"
  })

  it('says a charge at the second unit in its own words, a result it could not price as not charged, and a new decision as not shown', async () => {
    const job = makeJob()
    api(
      job,
      page([
        makeResult(1, { reason: 'external_verified', unit: 'm1_vendor' }),
        makeResult(2, { decision: 'unmapped', unit: null }),
        makeResult(3, { decision: 'refund' as never, unit: null }),
        makeResult(4, { origin: 'module', state: 'sent', reason: null, unit: 'm2_accepted' }),
      ]),
    )
    await openJob(job)
    await screen.findByTestId('results-row-1')
    expect(within(screen.getByTestId('results-row-1')).getByText(t('jobs.cost.unit.m1_vendor'))).toBeInTheDocument()
    expect(within(screen.getByTestId('results-row-2')).getByText(t('jobs.cost.notCharged'))).toBeInTheDocument()
    expect(within(screen.getByTestId('results-row-3')).getByText(t('jobs.cost.other'))).toBeInTheDocument()
    expect(within(screen.getByTestId('results-row-4')).getByText(t('jobs.cost.charged'))).toBeInTheDocument()
    expect(screen.getByRole('main').textContent).not.toContain('refund')
  })

  it('tells a suppressed address in a verification job, a row that lacked a value (with the user\'s own column names) and an address a chain left out', async () => {
    const job = makeJob({ module: 'm2' })
    api(
      job,
      page([
        makeResult(1, { suppressed: true, state: 'valid' }),
        makeResult(2, { origin: 'platform', state: 'missing_field', reason: 'first_name,plan', decision: 'free', unit: null }),
        makeResult(3, { origin: 'platform', state: 'excluded', reason: 'invalid', decision: 'free', unit: null }),
        makeResult(4, { origin: 'platform', state: 'excluded', reason: 'unclassified', decision: 'free', unit: null }),
      ]),
    )
    await openJob(job)
    await screen.findByTestId('results-row-1')
    expect(within(screen.getByTestId('results-row-1')).getByText(t('jobs.results.suppressed'))).toBeInTheDocument()
    expect(within(screen.getByTestId('results-row-2')).getByText(t('jobs.results.missingFields', { fields: 'first_name, plan' }))).toBeInTheDocument()
    expect(within(screen.getByTestId('results-row-3')).getByText(t('jobs.results.excluded', { verdict: 'invalid' }))).toBeInTheDocument()
    expect(screen.getByTestId('results-row-4').textContent).not.toContain('unclassified')
    // paired control: a row that is not suppressed says nothing of it
    expect(within(screen.getByTestId('results-row-3')).queryByText(t('jobs.results.suppressed'))).not.toBeInTheDocument()
  })

  it('shows an outcome it does not know as the generic word, never the word', async () => {
    const job = makeJob()
    api(job, page([makeResult(1, { state: 'vaporised' })]))
    await openJob(job)
    const row = await screen.findByTestId('results-row-1')
    expect(within(row).getByText(t('common.resultState.other'))).toBeInTheDocument()
    expect(row.textContent).not.toContain('vaporised')
  })

  it('has an empty state that says results come as they are ready while the job runs, and that there are none once it ended', async () => {
    const running = makeJob({ status: 'running', finished_at: null })
    api(running, page([]))
    await openJob(running)
    expect(await screen.findByText(t('jobs.results.empty.live'))).toBeInTheDocument()
    expect(screen.getByText(t('jobs.results.partial'))).toBeInTheDocument() // partial results are announced as partial

    resetForTest()
    await signInForTest()
    const done = makeJob({ id: jobId(2), status: 'succeeded' })
    api(done, page([]))
    await openJob(done)
    expect(await screen.findByText(t('jobs.results.empty.none'))).toBeInTheDocument()
    expect(screen.queryByText(t('jobs.results.partial'))).not.toBeInTheDocument()
  })

  it('shows the results that are final so far while a job runs', async () => {
    const job = makeJob({ status: 'running', finished_at: null, preview: makePreview({ rows: 10 }) })
    api(job, page([makeResult(1), makeResult(2)]))
    await openJob(job)
    expect(await screen.findByTestId('results-row-1')).toBeInTheDocument()
    expect(screen.getByTestId('results-row-2')).toBeInTheDocument()
    expect(screen.getByText(t('jobs.results.partial'))).toBeInTheDocument()
  })

  it('a failed page says it in the catalogue words, not with the server\'s sentence or its code', async () => {
    const job = makeJob()
    mockApi({
      ...ME,
      [`GET /v1/jobs/${job.id}`]: { job },
      [EVENTS(job.id)]: { events: [], next_after: null },
      [RESULTS(job.id)]: failing(500, 'internal_error'),
    })
    await openJob(job)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    const text = document.body.textContent ?? ''
    expect(text).not.toContain('SERVER-SAID')
    expect(text).not.toContain('pq_failure')
    expect(text).not.toContain('internal_error')
  })
})

describe('the pages of results', () => {
  function pagedResults() {
    const job = makeJob()
    const fetchMock = api(job, (url: URL) => {
      const cursor = url.searchParams.get('cursor')
      if (cursor === 'c3') return page([makeResult(5)], null)
      if (cursor === 'c2') return page([makeResult(3), makeResult(4)], 'c3')
      return page([makeResult(1), makeResult(2)], 'c2')
    })
    return { job, fetchMock }
  }

  it('walks forward with the cursor the API gave and back again, a page at a time', async () => {
    const { job, fetchMock } = pagedResults()
    await openJob(job)
    await screen.findByTestId('results-row-1')
    expect(screen.queryByTestId('results-row-3')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId('results-row-3')
    expect(screen.queryByTestId('results-row-1')).not.toBeInTheDocument()
    expect((fetchMock.mock.calls as Array<[string]>).some(([u]) => u.includes('/results?') && u.includes('cursor=c2'))).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId('results-row-5')
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.previous') }))
    await screen.findByTestId('results-row-3')
    expect(screen.queryByTestId('results-row-5')).not.toBeInTheDocument()
  })

  it('paired control: the next button is off on the last page and the previous one on the first', async () => {
    const { job } = pagedResults()
    await openJob(job)
    await screen.findByTestId('results-row-1')
    expect(screen.getByRole('button', { name: t('common.pagination.previous') })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId('results-row-3')
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId('results-row-5')
    expect(screen.getByRole('button', { name: t('common.pagination.next') })).toBeDisabled()
    expect(screen.getByRole('button', { name: t('common.pagination.previous') })).toBeEnabled()
  })
})

describe('the filter by outcome', () => {
  const mixed = () =>
    page([
      makeResult(1, { state: 'valid' }),
      makeResult(2, { state: 'invalid', reason: 'no_mx' }),
      makeResult(3, { state: 'valid' }),
      makeResult(4, { state: 'risky', reason: 'catch_all' }),
    ])

  it('offers the outcomes of the loaded page, narrows the rows to the one chosen, and All brings the rest back', async () => {
    const job = makeJob()
    api(job, mixed())
    await openJob(job)
    await screen.findByTestId('results-row-1')
    const select = screen.getByRole('combobox', { name: t('jobs.results.filter.label') })
    expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual([t('jobs.results.filter.all'), 'Valid', 'Invalid', 'Risky'])
    expect(screen.getByText(t('jobs.results.filter.hint'))).toBeInTheDocument() // it says it works on this page only

    fireEvent.change(select, { target: { value: 'valid' } })
    expect(screen.getByTestId('results-row-1')).toBeInTheDocument()
    expect(screen.getByTestId('results-row-3')).toBeInTheDocument()
    expect(screen.queryByTestId('results-row-2')).not.toBeInTheDocument()
    expect(screen.queryByTestId('results-row-4')).not.toBeInTheDocument()

    fireEvent.change(select, { target: { value: 'risky' } })
    expect(screen.getAllByTestId(/^results-row-/)).toHaveLength(1)
    expect(screen.getByTestId('results-row-4')).toBeInTheDocument()

    fireEvent.change(select, { target: { value: '' } })
    expect(screen.getAllByTestId(/^results-row-/)).toHaveLength(4)
  })

  it('is by keyboard: a native select, labelled, reachable', async () => {
    const job = makeJob()
    api(job, mixed())
    await openJob(job)
    await screen.findByTestId('results-row-1')
    const select = screen.getByRole('combobox', { name: t('jobs.results.filter.label') })
    expect(select.tagName).toBe('SELECT')
    select.focus()
    expect(select).toHaveFocus()
  })

  it('a choice that the next page has none of falls back to all outcomes, and the filter is not offered for a page with no results', async () => {
    const job = makeJob()
    api(job, (url: URL) => (url.searchParams.get('cursor') === 'c2' ? page([makeResult(3, { state: 'invalid', reason: 'no_mx' })]) : page([makeResult(1, { state: 'valid' }), makeResult(2, { state: 'invalid', reason: 'no_mx' })], 'c2')))
    await openJob(job)
    await screen.findByTestId('results-row-1')
    fireEvent.change(screen.getByRole('combobox', { name: t('jobs.results.filter.label') }), { target: { value: 'valid' } })
    expect(screen.queryByTestId('results-row-2')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId('results-row-3')
    expect(screen.getByRole('combobox', { name: t('jobs.results.filter.label') })).toHaveValue('')

    resetForTest()
    await signInForTest()
    const empty = makeJob({ id: jobId(2) })
    api(empty, page([]))
    await openJob(empty)
    await screen.findByText(t('jobs.results.empty.none'))
    expect(screen.queryByRole('combobox', { name: t('jobs.results.filter.label') })).not.toBeInTheDocument()
  })
})

describe('the CSV download', () => {
  const CSV_TEXT = 'ordinal,email,origin,state\n1,ann@example.test,module,valid\n'

  it('fetches the file through the API client with the Bearer header and hands the Blob to the browser as a download named from the job, and navigates nowhere', async () => {
    const job = makeJob({ module: 'm1', created_at: '2026-12-01T08:59:00Z' })
    const fetchMock = api(job, page([makeResult(1)]), { [CSV(job.id)]: () => new Response(CSV_TEXT, { headers: { 'Content-Type': 'text/csv' } }) })
    const open = vi.fn()
    vi.stubGlobal('open', open)
    await openJob(job)
    await screen.findByTestId('results-row-1')
    const csvCalls = () => (fetchMock.mock.calls as Array<[string, RequestInit]>).filter(([u]) => new URL(u, 'http://app.test').pathname === `/v1/jobs/${job.id}/results.csv`)
    expect(csvCalls()).toHaveLength(0) // nothing before the click
    expect(document.querySelector('a[href*="results.csv"]')).toBeNull() // and no link to the API that would need a header the browser cannot add

    fireEvent.click(screen.getByRole('button', { name: t('jobs.csv.button') }))
    await vi.waitFor(() => expect(saveBlob).toHaveBeenCalledTimes(1))

    const calls = csvCalls()
    expect(calls).toHaveLength(1)
    const headers = (calls[0]?.[1].headers ?? {}) as Record<string, string>
    expect(headers['Authorization']).toMatch(/^Bearer .+\..+\..+$/)
    const [blob, name] = vi.mocked(saveBlob).mock.calls[0] as [Blob, string]
    expect(await blob.text()).toBe(CSV_TEXT)
    expect(name).toBe(csvFileName(job))
    expect(name).toBe('results-verification-2026-12-01-0b000000.csv')
    expect(open).not.toHaveBeenCalled()
    expect(window.location.pathname).toBe(`/jobs/${job.id}`)
    expect(screen.getByText(t('jobs.csv.ready'))).toBeInTheDocument()
  })

  it('paired control: the other results requests of the page carry the header too (it is the client that adds it)', async () => {
    const job = makeJob()
    const fetchMock = api(job, page([makeResult(1)]))
    await openJob(job)
    await screen.findByTestId('results-row-1')
    const resultsCall = (fetchMock.mock.calls as Array<[string, RequestInit]>).find(([u]) => u.startsWith(`/v1/jobs/${job.id}/results`))
    expect((resultsCall?.[1].headers as Record<string, string>)['Authorization']).toMatch(/^Bearer /)
  })

  it('asks for the file once however often the button is pressed, and says so in the catalogue words when it fails', async () => {
    const job = makeJob()
    let fail = true
    const fetchMock = api(job, page([makeResult(1)]), { [CSV(job.id)]: () => (fail ? failing(500, 'internal_error')() : new Response(CSV_TEXT)) })
    await openJob(job)
    await screen.findByTestId('results-row-1')
    const button = screen.getByRole('button', { name: t('jobs.csv.button') })
    fireEvent.click(button)
    fireEvent.click(button)
    fireEvent.click(button)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    expect(saveBlob).not.toHaveBeenCalled()
    const asked = () => (fetchMock.mock.calls as Array<[string]>).filter(([u]) => u.endsWith('/results.csv')).length
    expect(asked()).toBe(1)
    expect(document.body.textContent).not.toContain('SERVER-SAID')

    fail = false // and the button works again
    fireEvent.click(screen.getByRole('button', { name: t('jobs.csv.button') }))
    await vi.waitFor(() => expect(saveBlob).toHaveBeenCalledTimes(1))
    expect(asked()).toBe(2)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument() // the old error is gone
  })

  it('is a button that can be reached and pressed by keyboard', async () => {
    const job = makeJob()
    api(job, page([makeResult(1)]))
    await openJob(job)
    const button = await screen.findByRole('button', { name: t('jobs.csv.button') })
    expect(button.tagName).toBe('BUTTON')
    button.focus()
    expect(button).toHaveFocus()
  })
})

describe('how far a running job has got', () => {
  const progressbar = () => screen.queryByRole('progressbar', { name: t('jobs.progress.label') })

  it('shows the results counted so far against the size of the list, in numbers and in a bar', async () => {
    const job = makeJob({ module: 'm1', status: 'running', finished_at: null, preview: makePreview({ rows: 10 }) })
    api(job, page([makeResult(1), makeResult(2), makeResult(3), makeResult(4)]))
    await openJob(job)
    const bar = await screen.findByRole('progressbar', { name: t('jobs.progress.label') })
    expect(bar).toHaveAttribute('aria-valuenow', '4')
    expect(bar).toHaveAttribute('aria-valuemax', '10')
    expect(screen.getByText(t('jobs.progress.value', { done: 4, total: 10 }))).toBeInTheDocument()
  })

  it('adds the pages up (a count is the sum of the pages, not the size of the first)', async () => {
    const job = makeJob({ status: 'running', finished_at: null, preview: makePreview({ rows: 500 }) })
    api(job, (url: URL) => (url.searchParams.get('cursor') === 'c2' ? page([makeResult(101), makeResult(102)]) : page(Array.from({ length: 100 }, (_, i) => makeResult(i + 1)), 'c2')))
    await openJob(job)
    const bar = await screen.findByRole('progressbar', { name: t('jobs.progress.label') })
    expect(bar).toHaveAttribute('aria-valuenow', '102')
  })

  it('does not pretend for a job too large to count: it says so, and shows no bar', async () => {
    const job = makeJob({ status: 'running', finished_at: null, preview: makePreview({ rows: 100000 }) })
    // every page has a next one: the count stops at the cap
    const fetchMock = api(job, page([makeResult(1)], 'more'))
    await openJob(job)
    expect(await screen.findByText(t('jobs.progress.large'))).toBeInTheDocument()
    expect(progressbar()).not.toBeInTheDocument()
    const walks = (fetchMock.mock.calls as Array<[string]>).filter(([u]) => u.includes('cursor=more')).length
    expect(walks).toBeLessThanOrEqual(COUNT_PAGES_MAX) // it stopped at the cap
  })

  it.each(['paused', 'scheduled', 'succeeded', 'cancelled', 'failed', 'confirmed'] as const)('shows no bar for a %s job', async (status) => {
    const job = makeJob({ status, paused_from: status === 'paused' ? 'running' : null, finished_at: null, preview: makePreview({ rows: 10 }) })
    api(job, page([makeResult(1)]))
    await openJob(job)
    await screen.findByTestId('results-row-1')
    expect(progressbar()).not.toBeInTheDocument()
  })

  it('counts again every 10 seconds while the job runs and the count is complete, and the bar follows', async () => {
    vi.useFakeTimers()
    const job = makeJob({ status: 'running', finished_at: null, preview: makePreview({ rows: 10 }) })
    let done = 2
    api(job, () => page(Array.from({ length: done }, (_, i) => makeResult(i + 1))))
    renderApp(`/jobs/${job.id}`)
    await advance(0)
    expect(screen.getByRole('progressbar', { name: t('jobs.progress.label') })).toHaveAttribute('aria-valuenow', '2')
    done = 7
    await advance(10_100)
    expect(screen.getByRole('progressbar', { name: t('jobs.progress.label') })).toHaveAttribute('aria-valuenow', '7')
    expect(screen.getByTestId('results-row-7')).toBeInTheDocument() // the table moved on by itself, too
  })
})
