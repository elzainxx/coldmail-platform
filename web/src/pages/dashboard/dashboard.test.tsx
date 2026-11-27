import { act, fireEvent, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Balances, Job, M3Entitlement } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { formatDateTime, jobName } from '../jobs/format.ts'
import { jobId, makeJob, makePreview } from '../jobs/fixtures.ts'
import { RECENT_COUNT } from './DashboardPage.tsx'

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

function balances(over: Partial<Balances> = {}): Balances {
  return {
    modules: { m1: { available: 1000, reserved: 25 }, m2: { available: 70, reserved: 5 } },
    m3: { enabled: true, until: '2026-12-31T12:00:00Z', state: 'active' },
    ...over,
  }
}

/** The routes of the dashboard: balances, the active jobs (`status=active`) and the newest jobs. */
function dashboardApi(opts: { balances?: Balances; active?: Job[]; newest?: Job[] } = {}, extra: Record<string, unknown> = {}) {
  const active = opts.active ?? []
  const newest = opts.newest ?? active
  return mockApi({
    ...ME,
    'GET /v1/balances': () => opts.balances ?? balances(),
    'GET /v1/jobs': (url: URL) => ({ jobs: url.searchParams.get('status') === 'active' ? active : newest, next_cursor: null }),
    ...extra,
  } as Parameters<typeof mockApi>[0])
}

const callsTo = (api: ReturnType<typeof mockApi>, path: string) =>
  (api.mock.calls as Array<[string, RequestInit]>).filter(([u]) => new URL(u, 'http://app.test').pathname === path)

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

describe('the dashboard: balances', () => {
  it('has its heading at once, and a place for each kind of job once the balances are in', async () => {
    dashboardApi()
    renderApp('/')
    expect(screen.getByRole('heading', { level: 1, name: t('dashboard.title') })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('common.nav.dashboard') })).toHaveAttribute('aria-current', 'page')
    expect(await screen.findByTestId('balance-m1')).toBeInTheDocument()
    expect(screen.getByTestId('balance-m2')).toBeInTheDocument()
    expect(screen.getByTestId('balance-m3')).toBeInTheDocument()
  })

  it('shows what is available and what is set aside for verification and for managed sending, each in its own place', async () => {
    dashboardApi({ balances: balances({ modules: { m1: { available: 12345, reserved: 25 }, m2: { available: 70, reserved: 1 } } }) })
    renderApp('/')
    const m1 = await screen.findByTestId('balance-m1')
    const m2 = screen.getByTestId('balance-m2')
    expect(within(m1).getByRole('heading', { name: t('dashboard.balance.m1') })).toBeInTheDocument()
    expect(within(m1).getByText('12,345 credits')).toBeInTheDocument()
    expect(within(m1).getByText('25 credits')).toBeInTheDocument()
    expect(within(m2).getByRole('heading', { name: t('dashboard.balance.m2') })).toBeInTheDocument()
    expect(within(m2).getByText('70 credits')).toBeInTheDocument()
    expect(within(m2).getByText('1 credit')).toBeInTheDocument() // the singular
    // paired control: the numbers of one are not in the other
    expect(m1.textContent).not.toContain('70 credits')
    expect(m2.textContent).not.toContain('12,345')
  })

  it('says zero credits as zero (a user who never had any)', async () => {
    dashboardApi({ balances: balances({ modules: { m1: { available: 0, reserved: 0 }, m2: { available: 0, reserved: 0 } } }) })
    renderApp('/')
    const m1 = await screen.findByTestId('balance-m1')
    expect(within(m1).getAllByText('0 credits')).toHaveLength(2)
  })

  const SMTP: Array<[string, M3Entitlement, string, string]> = [
    ['switched on until a date', { enabled: true, until: '2026-12-31T12:00:00Z', state: 'active' }, t('dashboard.m3.enabledUntil', { date: formatDateTime('2026-12-31T12:00:00Z') }), 'On'],
    ['switched on with no end', { enabled: true, until: null, state: 'active' }, t('dashboard.m3.enabled'), 'On'],
    ['switched on, its state not yet seen', { enabled: true, until: '2026-12-31T12:00:00Z', state: 'unknown' }, t('dashboard.m3.unconfirmed'), 'On'],
    ['suspended', { enabled: true, until: '2026-12-31T12:00:00Z', state: 'suspended' }, t('dashboard.m3.suspended'), 'Suspended'],
    ['not switched on', { enabled: false, until: null, state: 'unknown' }, t('dashboard.m3.disabled'), 'Off'],
    ['ended', { enabled: false, until: '2026-01-01T00:00:00Z', state: 'active' }, t('dashboard.m3.ended', { date: formatDateTime('2026-01-01T00:00:00Z') }), 'Off'],
  ]

  it.each(SMTP)('says in words that your own SMTP is %s, with its colour, and says nothing of the other states', async (_name, m3, sentence, badge) => {
    dashboardApi({ balances: balances({ m3 }) })
    renderApp('/')
    const card = await screen.findByTestId('balance-m3')
    expect(within(card).getByRole('heading', { name: t('dashboard.balance.m3') })).toBeInTheDocument()
    expect(within(card).getByText(sentence)).toBeInTheDocument()
    expect(within(card).getByText(badge).closest('[data-status]')).not.toBeNull()
    expect(within(card).getByText(t('dashboard.m3.noCredits'))).toBeInTheDocument() // not a credit balance
    // paired control: none of the other states' sentences is there
    const others = [t('dashboard.m3.suspended'), t('dashboard.m3.disabled'), t('dashboard.m3.enabled')].filter((s) => s !== sentence)
    for (const other of others) expect(card.textContent, other).not.toContain(other)
    expect(card.textContent).not.toMatch(/m3|active|enabled"|unknown/) // no state word of the API
  })

  it('colours the three states differently', async () => {
    const seen: string[] = []
    for (const m3 of [SMTP[0]![1], SMTP[3]![1], SMTP[4]![1]]) {
      resetForTest()
      await signInForTest()
      dashboardApi({ balances: balances({ m3 }) })
      const { unmount } = renderApp('/')
      const card = await screen.findByTestId('balance-m3')
      seen.push(card.querySelector('[data-status]')?.getAttribute('data-status') ?? '')
      unmount()
    }
    expect(seen).toEqual(['valid', 'failed', 'unknown'])
  })

  it('a failed balance read says so in the catalogue words and lets the rest of the page load; Try again reads it again', async () => {
    let fail = true
    dashboardApi({ active: [makeJob({ id: jobId(1), status: 'running' })] }, {
      'GET /v1/balances': () =>
        fail
          ? new Response(JSON.stringify({ error: { code: 'internal_error', message: 'SERVER-SAID-pq_down', request_id: 'req_0123456789abcdef0123456789abcdef' } }), { status: 500 })
          : balances(),
    })
    renderApp('/')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    expect(alert).toHaveTextContent('Reference req_0123456789abcdef0123456789abcdef')
    expect(await screen.findByTestId(`job-row-${jobId(1)}`)).toBeInTheDocument() // the jobs did not wait for the balances
    const text = document.body.textContent ?? ''
    for (const raw of ['SERVER-SAID', 'pq_down', 'internal_error']) expect(text, raw).not.toContain(raw)
    expect(screen.queryByTestId('balance-m1')).not.toBeInTheDocument()

    fail = false
    fireEvent.click(within(alert).getByRole('button', { name: t('common.retry') }))
    expect(await screen.findByTestId('balance-m1')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})

describe('the dashboard: jobs in progress', () => {
  it('lists each active job with its status word, when it started and how big it is, each with a link to its page', async () => {
    const running = makeJob({ id: jobId(1), module: 'm2', status: 'running', started_at: '2026-12-01T09:02:00Z', finished_at: null, preview: makePreview({ rows: 1200 }) })
    const scheduled = makeJob({ id: jobId(2), module: 'm1', status: 'scheduled', scheduled_start: '2026-12-05T10:00:00Z', started_at: null, finished_at: null, created_at: '2026-11-30T08:00:00Z' })
    const paused = makeJob({ id: jobId(3), module: 'm3', status: 'paused', paused_from: 'running', started_at: '2026-12-01T09:02:00Z', finished_at: null, created_at: '2026-11-29T08:00:00Z' })
    dashboardApi({ active: [running, scheduled, paused], newest: [running, scheduled, paused] })
    renderApp('/')
    const section = (await screen.findByRole('heading', { name: t('dashboard.active.title') })).closest('section') as HTMLElement
    await within(section).findByTestId(`job-row-${jobId(1)}`)
    const rowRunning = within(section).getByTestId(`job-row-${jobId(1)}`)
    expect(within(rowRunning).getByRole('link', { name: jobName(running) })).toHaveAttribute('href', `/jobs/${jobId(1)}`)
    expect(within(section).getByTestId(`job-status-${jobId(1)}`)).toHaveTextContent('Running')
    expect(rowRunning).toHaveTextContent(t('dashboard.job.started', { date: formatDateTime(running.started_at) }))
    expect(rowRunning).toHaveTextContent('1,200 addresses')
    expect(within(section).getByTestId(`job-row-${jobId(2)}`)).toHaveTextContent(t('dashboard.job.starts', { date: formatDateTime(scheduled.scheduled_start) }))
    expect(within(section).getByTestId(`job-status-${jobId(2)}`)).toHaveTextContent('Scheduled')
    expect(within(section).getByTestId(`job-status-${jobId(3)}`)).toHaveTextContent('Paused')
    // newest first
    expect(within(section).getAllByRole('listitem').map((li) => li.getAttribute('data-testid'))).toEqual([`job-row-${jobId(1)}`, `job-row-${jobId(2)}`, `job-row-${jobId(3)}`])
    expect(within(section).queryByText(t('dashboard.active.empty.title'))).not.toBeInTheDocument()
  })

  it('also lists a confirmed job that is about to start, which the API does not call active (and not a draft or a finished one)', async () => {
    const confirmed = makeJob({ id: jobId(1), status: 'confirmed', started_at: null, finished_at: null })
    const draft = makeJob({ id: jobId(2), status: 'draft', preview: null })
    const done = makeJob({ id: jobId(3), status: 'succeeded' })
    dashboardApi({ active: [], newest: [confirmed, draft, done] })
    renderApp('/')
    const section = (await screen.findByRole('heading', { name: t('dashboard.active.title') })).closest('section') as HTMLElement
    await within(section).findByTestId(`job-row-${jobId(1)}`)
    expect(within(section).queryByTestId(`job-row-${jobId(2)}`)).not.toBeInTheDocument()
    expect(within(section).queryByTestId(`job-row-${jobId(3)}`)).not.toBeInTheDocument()
  })

  it('asks the API for the active jobs with the filter, and for the others without it', async () => {
    const api = dashboardApi()
    renderApp('/')
    await screen.findByTestId('balance-m1')
    const queries = callsTo(api, '/v1/jobs').map(([u]) => new URL(u, 'http://app.test').searchParams.get('status'))
    expect(queries).toContain('active')
    expect(queries).toContain(null)
  })

  it('has an empty state with a way to start a job', async () => {
    dashboardApi({ active: [], newest: [] })
    renderApp('/')
    const section = (await screen.findByRole('heading', { name: t('dashboard.active.title') })).closest('section') as HTMLElement
    await within(section).findByText(t('dashboard.active.empty.title'))
    expect(within(section).getByRole('link', { name: t('common.nav.newVerify') })).toHaveAttribute('href', '/new/verify')
  })
})

describe('the dashboard: recent results', () => {
  it('lists the latest finished jobs with their size and a link, newest first, at most five, and no job still running', async () => {
    const finished = Array.from({ length: 7 }, (_, i) => makeJob({ id: jobId(10 + i), status: 'succeeded', finished_at: `2026-12-0${i + 1}T10:00:00Z`, created_at: `2026-12-0${7 - i}T08:00:00Z`, preview: makePreview({ rows: 100 + i }) }))
    const running = makeJob({ id: jobId(1), status: 'running', finished_at: null, created_at: '2026-12-09T08:00:00Z' })
    dashboardApi({ active: [running], newest: [running, ...finished] })
    renderApp('/')
    const section = (await screen.findByRole('heading', { name: t('dashboard.recent.title') })).closest('section') as HTMLElement
    await within(section).findByTestId(`job-row-${jobId(10)}`)
    const items = within(section).getAllByRole('listitem')
    expect(items).toHaveLength(RECENT_COUNT)
    expect(items.map((li) => li.getAttribute('data-testid'))).toEqual(finished.slice(0, RECENT_COUNT).map((j) => `job-row-${j.id}`))
    expect(within(items[0]!).getByRole('link', { name: jobName(finished[0]!) })).toHaveAttribute('href', `/jobs/${jobId(10)}`)
    expect(items[0]).toHaveTextContent('100 addresses')
    expect(items[0]).toHaveTextContent(t('dashboard.job.finished', { date: formatDateTime(finished[0]!.finished_at) }))
    expect(within(section).queryByTestId(`job-row-${jobId(1)}`)).not.toBeInTheDocument() // the running job is in progress, not a result
    expect(within(section).queryByTestId(`job-row-${jobId(15)}`)).not.toBeInTheDocument() // the sixth is not listed
    expect(within(section).getByRole('link', { name: t('dashboard.recent.all') })).toHaveAttribute('href', '/jobs')
  })

  it('lists a cancelled or failed job too, with its own word', async () => {
    dashboardApi({ newest: [makeJob({ id: jobId(1), status: 'cancelled' }), makeJob({ id: jobId(2), status: 'failed' })], active: [] })
    renderApp('/')
    await screen.findByTestId(`job-status-${jobId(1)}`)
    expect(screen.getByTestId(`job-status-${jobId(1)}`)).toHaveTextContent('Cancelled')
    expect(screen.getByTestId(`job-status-${jobId(2)}`)).toHaveTextContent('Failed')
  })

  it('has an empty state with a way to start a job, and paired control: a finished job removes it', async () => {
    dashboardApi({ active: [], newest: [makeJob({ id: jobId(1), status: 'running', finished_at: null })] })
    const { unmount } = renderApp('/')
    const section = (await screen.findByRole('heading', { name: t('dashboard.recent.title') })).closest('section') as HTMLElement
    await within(section).findByText(t('dashboard.recent.empty.title'))
    expect(within(section).getByRole('link', { name: t('common.nav.newVerify') })).toHaveAttribute('href', '/new/verify')
    unmount()

    resetForTest()
    await signInForTest()
    dashboardApi({ active: [], newest: [makeJob({ id: jobId(1), status: 'succeeded' })] })
    renderApp('/')
    const again = (await screen.findByRole('heading', { name: t('dashboard.recent.title') })).closest('section') as HTMLElement
    await within(again).findByTestId(`job-row-${jobId(1)}`)
    expect(within(again).queryByText(t('dashboard.recent.empty.title'))).not.toBeInTheDocument()
  })

  it('shows no module name, status slug or id anywhere on the page', async () => {
    dashboardApi({ active: [makeJob({ id: jobId(1), module: 'm3', status: 'cancelling', finished_at: null })], newest: [makeJob({ id: jobId(2), module: 'm2', status: 'failed' })] })
    renderApp('/')
    await screen.findByTestId(`job-row-${jobId(1)}`)
    const text = screen.getByRole('main').textContent ?? ''
    expect(text).toContain('Cancelling')
    for (const raw of ['m1', 'm2', 'm3', 'cancelling', 'failed', jobId(1), 'module', 'reserved', 'available"']) expect(text, raw).not.toContain(raw)
  })

  it('a failed read of the jobs says so and the balances stay', async () => {
    dashboardApi({}, { 'GET /v1/jobs': () => new Response(JSON.stringify({ error: { code: 'busy', message: 'SERVER-SAID', request_id: 'req_1' } }), { status: 503, headers: { 'Retry-After': '0' } }) })
    renderApp('/')
    expect(await screen.findByTestId('balance-m1', {}, { timeout: 5_000 })).toBeInTheDocument()
    const alerts = await screen.findAllByRole('alert', {}, { timeout: 5_000 })
    expect(alerts.length).toBeGreaterThanOrEqual(1)
    for (const alert of alerts) expect(alert).toHaveTextContent(t('error.busy'))
    expect(document.body.textContent).not.toContain('SERVER-SAID')
  }, 15_000)
})

describe('the dashboard polls', () => {
  it('reads everything again every 10 seconds, not before, and the page shows what changed', async () => {
    vi.useFakeTimers()
    let credits = 1000
    const api = dashboardApi({}, { 'GET /v1/balances': () => balances({ modules: { m1: { available: credits, reserved: 0 }, m2: { available: 70, reserved: 5 } } }) })
    renderApp('/')
    await advance(0)
    expect(callsTo(api, '/v1/balances')).toHaveLength(1)
    expect(within(screen.getByTestId('balance-m1')).getByText('1,000 credits')).toBeInTheDocument()
    credits = 800
    await advance(9_000)
    expect(callsTo(api, '/v1/balances')).toHaveLength(1) // not yet
    await advance(1_100)
    expect(callsTo(api, '/v1/balances')).toHaveLength(2)
    expect(callsTo(api, '/v1/jobs').length).toBeGreaterThanOrEqual(4) // both lists, twice
    expect(within(screen.getByTestId('balance-m1')).getByText('800 credits')).toBeInTheDocument()
  })

  it('stops while the tab is hidden and reads at once when it is shown again', async () => {
    vi.useFakeTimers()
    const api = dashboardApi()
    renderApp('/')
    await advance(0)
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => true })
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    const before = callsTo(api, '/v1/balances').length
    await advance(120_000)
    expect(callsTo(api, '/v1/balances')).toHaveLength(before)
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => false })
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await advance(0)
    expect(callsTo(api, '/v1/balances')).toHaveLength(before + 1)
  })

  it('paired control: while the tab is shown the same two minutes make many reads', async () => {
    vi.useFakeTimers()
    const api = dashboardApi()
    renderApp('/')
    await advance(0)
    await advance(120_000)
    expect(callsTo(api, '/v1/balances').length).toBeGreaterThanOrEqual(10)
  })
})
