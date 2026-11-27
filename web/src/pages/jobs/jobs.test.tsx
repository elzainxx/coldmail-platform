import { act, fireEvent, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Job, JobStatus } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { jobStatusText } from '../../copy/words.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { STATUS_TOKENS } from '../../ui/index.ts'
import { formatDateTime, jobName } from './format.ts'
import { jobId, makeJob } from './fixtures.ts'

const ME = { 'GET /v1/me': { email: 'ann@example.test' } }

beforeEach(async () => {
  resetForTest()
  await signInForTest()
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => false })
  resetForTest()
})

function setHidden(hidden: boolean) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
  document.dispatchEvent(new Event('visibilitychange'))
}

/** The calls the app made to a route (method and path, no query). */
function callsTo(fetchMock: ReturnType<typeof mockApi>, method: string, path: string): Array<[string, RequestInit]> {
  return (fetchMock.mock.calls as Array<[string, RequestInit]>).filter(([url, init]) => (init?.method ?? 'GET') === method && new URL(url, 'http://app.test').pathname === path)
}

/** Let what is waiting (the answers of the mocked API, the effects they cause) happen, and `ms` of fake time pass. */
async function advance(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

describe('the jobs table', () => {
  it('lists the jobs with their kind, status word, address count and date, each with a link to its page', async () => {
    const running = makeJob({ id: jobId(1), module: 'm2', status: 'running', created_at: '2026-12-02T10:00:00Z' })
    const done = makeJob({ id: jobId(2), module: 'm1', status: 'succeeded' })
    mockApi({ ...ME, 'GET /v1/jobs': { jobs: [running, done], next_cursor: null } })
    renderApp('/jobs')

    expect(screen.getByRole('heading', { level: 1, name: t('jobs.title') })).toBeInTheDocument() // there at once, before any data
    const rowRunning = await screen.findByTestId(`job-row-${jobId(1)}`)
    const rowDone = screen.getByTestId(`job-row-${jobId(2)}`)

    expect(within(rowRunning).getByRole('link', { name: jobName(running) })).toHaveAttribute('href', `/jobs/${jobId(1)}`)
    expect(within(rowRunning).getByText(t('jobs.kind.m2'))).toBeInTheDocument()
    expect(screen.getByTestId(`job-status-${jobId(1)}`)).toHaveTextContent('Running')
    expect(screen.getByTestId(`job-status-${jobId(2)}`)).toHaveTextContent('Finished')
    expect(within(rowDone).getByText(t('jobs.count.other', { count: 5 }))).toBeInTheDocument()
    expect(within(rowDone).getByText(formatDateTime(done.created_at))).toBeInTheDocument()
    // control: the two rows are not the same row (the first one is not "Finished", the second not "Running")
    expect(screen.getByTestId(`job-status-${jobId(1)}`)).not.toHaveTextContent('Finished')
    expect(screen.getByTestId(`job-status-${jobId(2)}`)).not.toHaveTextContent('Running')
  })

  it('never shows a module name, a status slug or an id', async () => {
    mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1), module: 'm3', status: 'cancelling' })], next_cursor: null } })
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    const text = screen.getByRole('main').textContent ?? ''
    expect(text).toContain(t('jobs.kind.m3'))
    expect(text).toContain(jobStatusText('cancelling'))
    for (const raw of ['m3', 'm1', 'm2', 'cancelling', jobId(1), 'module']) expect(text, raw).not.toContain(raw)
  })

  it('says a job that has no preview yet is not counted, and an address count of one in the singular', async () => {
    mockApi({
      ...ME,
      'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1), status: 'draft', preview: null }), makeJob({ id: jobId(2), preview: { ...makeJob().preview!, rows: 1 } })], next_cursor: null },
    })
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(within(screen.getByTestId(`job-row-${jobId(1)}`)).getByText(t('jobs.count.none'))).toBeInTheDocument()
    expect(within(screen.getByTestId(`job-row-${jobId(2)}`)).getByText(t('jobs.count.one'))).toBeInTheDocument()
  })

  it('every status of a job has a word and a colour, and the table shows the word', async () => {
    const statuses: JobStatus[] = ['draft', 'previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing', 'succeeded', 'cancelled', 'expired', 'failed']
    const jobs = statuses.map((status, i) => makeJob({ id: jobId(i + 1), status }))
    mockApi({ ...ME, 'GET /v1/jobs': { jobs, next_cursor: null } })
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    const words = new Set<string>()
    statuses.forEach((status, i) => {
      const badge = screen.getByTestId(`job-status-${jobId(i + 1)}`)
      expect(badge, status).toHaveTextContent(jobStatusText(status))
      expect(jobStatusText(status), status).not.toBe(t('common.resultState.other')) // a status has a word of its own, not the generic one
      expect(badge.querySelector('[data-status]')?.getAttribute('data-status'), status).toSatisfy((token: string) => (STATUS_TOKENS as readonly string[]).includes(token))
      words.add(jobStatusText(status))
    })
    expect(words.size).toBe(statuses.length) // twelve statuses, twelve different words
  })

  it('paired control: a status the app does not know shows the generic word, never the status', async () => {
    mockApi({ ...ME, 'GET /v1/jobs': { jobs: [{ ...makeJob({ id: jobId(1) }), status: 'teleporting' } as unknown as Job], next_cursor: null } })
    renderApp('/jobs')
    const badge = await screen.findByTestId(`job-status-${jobId(1)}`)
    expect(badge).toHaveTextContent(t('common.resultState.other'))
    expect(badge).not.toHaveTextContent('teleporting')
  })

  it('has an empty state with a way to start the first job', async () => {
    mockApi({ ...ME, 'GET /v1/jobs': { jobs: [], next_cursor: null } })
    renderApp('/jobs')
    expect(await screen.findByText(t('jobs.table.empty.title'))).toBeInTheDocument()
    const main = screen.getByRole('main')
    // the page header and the empty state both offer it
    expect(within(main).getAllByRole('link', { name: t('common.nav.newVerify') }).map((a) => a.getAttribute('href'))).toEqual(['/new/verify', '/new/verify'])
  })

  it('paired control: a list with a job has no empty state', async () => {
    mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1) })], next_cursor: null } })
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(screen.queryByText(t('jobs.table.empty.title'))).not.toBeInTheDocument()
  })
})

describe('the jobs table pages', () => {
  function pagedApi() {
    return mockApi({
      ...ME,
      'GET /v1/jobs': (url: URL) => {
        const cursor = url.searchParams.get('cursor')
        if (cursor === 'page-3') return { jobs: [makeJob({ id: jobId(3) })], next_cursor: null }
        if (cursor === 'page-2') return { jobs: [makeJob({ id: jobId(2) })], next_cursor: 'page-3' }
        return { jobs: [makeJob({ id: jobId(1) })], next_cursor: 'page-2' }
      },
    })
  }

  it('goes to the next page with the cursor the API gave, and back to the one before', async () => {
    const api = pagedApi()
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(callsTo(api, 'GET', '/v1/jobs')[0]?.[0]).not.toContain('cursor') // the first page is asked for with no cursor

    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId(`job-row-${jobId(2)}`)
    expect(screen.queryByTestId(`job-row-${jobId(1)}`)).not.toBeInTheDocument()
    expect(callsTo(api, 'GET', '/v1/jobs').at(-1)?.[0]).toContain('cursor=page-2')
    expect(screen.getByText(t('common.pagination.page', { n: 2 }))).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.previous') }))
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(screen.queryByTestId(`job-row-${jobId(2)}`)).not.toBeInTheDocument()
  })

  it('paired control: the previous button is off on the first page and the next button is off on the last', async () => {
    pagedApi()
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(screen.getByRole('button', { name: t('common.pagination.previous') })).toBeDisabled()
    expect(screen.getByRole('button', { name: t('common.pagination.next') })).toBeEnabled()
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId(`job-row-${jobId(2)}`)
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId(`job-row-${jobId(3)}`)
    expect(screen.getByRole('button', { name: t('common.pagination.next') })).toBeDisabled()
    expect(screen.getByRole('button', { name: t('common.pagination.previous') })).toBeEnabled()
  })

  it('shows no page buttons when everything fits on one page', async () => {
    mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1) })], next_cursor: null } })
    renderApp('/jobs')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(screen.queryByRole('navigation', { name: t('common.pagination.label') })).not.toBeInTheDocument()
  })
})

describe('the jobs table polls', () => {
  it('asks again every 3 seconds while a job is in progress, and not before', async () => {
    vi.useFakeTimers()
    const api = mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1), status: 'running' })], next_cursor: null } })
    renderApp('/jobs')
    await advance()
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(1)
    await advance(2_900)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(1) // not yet
    await advance(200)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(2)
    await advance(3_000)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(3)
  })

  it('asks every 15 seconds when no job is in progress (paired control: it is not 3 seconds then)', async () => {
    vi.useFakeTimers()
    const api = mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1), status: 'succeeded' }), makeJob({ id: jobId(2), status: 'draft' })], next_cursor: null } })
    renderApp('/jobs')
    await advance()
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(1)
    await advance(14_000)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(1) // three seconds went by five times: nothing was asked
    await advance(1_100)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(2)
  })

  it('speeds up when a job starts and slows down when the last one ends, and the table shows the change', async () => {
    vi.useFakeTimers()
    let status: JobStatus = 'running'
    const api = mockApi({ ...ME, 'GET /v1/jobs': () => ({ jobs: [makeJob({ id: jobId(1), status })], next_cursor: null }) })
    renderApp('/jobs')
    await advance()
    expect(screen.getByTestId(`job-status-${jobId(1)}`)).toHaveTextContent('Running')
    status = 'succeeded'
    await advance(3_000)
    expect(screen.getByTestId(`job-status-${jobId(1)}`)).toHaveTextContent('Finished') // the row moved on by itself
    const asked = callsTo(api, 'GET', '/v1/jobs').length
    await advance(3_000)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(asked) // finished: back to the slow rhythm
    await advance(12_100)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(asked + 1)
  })

  it('stops while the tab is hidden, and asks at once when it is shown again', async () => {
    vi.useFakeTimers()
    const api = mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1), status: 'running' })], next_cursor: null } })
    renderApp('/jobs')
    await advance()
    const before = callsTo(api, 'GET', '/v1/jobs').length
    act(() => setHidden(true))
    await advance(60_000)
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(before) // a minute hidden, not one request
    act(() => setHidden(false))
    await advance()
    expect(callsTo(api, 'GET', '/v1/jobs')).toHaveLength(before + 1)
  })

  it('paired control: while the tab is shown the same minute makes many requests', async () => {
    vi.useFakeTimers()
    const api = mockApi({ ...ME, 'GET /v1/jobs': { jobs: [makeJob({ id: jobId(1), status: 'running' })], next_cursor: null } })
    renderApp('/jobs')
    await advance()
    await advance(60_000)
    expect(callsTo(api, 'GET', '/v1/jobs').length).toBeGreaterThanOrEqual(15)
  })
})

describe('a failed table', () => {
  it('shows the catalogue wording for the code, the reference, and never the server message or the code; Try again asks again', async () => {
    let fail = true
    mockApi({
      ...ME,
      'GET /v1/jobs': () =>
        fail
          ? new Response(JSON.stringify({ error: { code: 'internal_error', message: 'SERVER-SAID-pq_connection_refused', request_id: 'req_0123456789abcdef0123456789abcdef' } }), { status: 500 })
          : { jobs: [makeJob({ id: jobId(1) })], next_cursor: null },
    })
    renderApp('/jobs')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    expect(alert).toHaveTextContent('Reference req_0123456789abcdef0123456789abcdef')
    const page = document.body.textContent ?? ''
    expect(page).not.toContain('SERVER-SAID')
    expect(page).not.toContain('pq_connection_refused')
    expect(page).not.toContain('internal_error')

    fail = false
    fireEvent.click(within(alert).getByRole('button', { name: t('common.retry') }))
    await screen.findByTestId(`job-row-${jobId(1)}`)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('a poll that fails keeps the table on screen (paired control: the failure of the first load does not)', async () => {
    vi.useFakeTimers()
    let calls = 0
    mockApi({
      ...ME,
      'GET /v1/jobs': () => {
        calls++
        return calls === 1 ? { jobs: [makeJob({ id: jobId(1), status: 'running' })], next_cursor: null } : new Response('', { status: 500 })
      },
    })
    renderApp('/jobs')
    await advance()
    await advance(3_000)
    expect(calls).toBeGreaterThanOrEqual(2)
    expect(screen.getByTestId(`job-row-${jobId(1)}`)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
