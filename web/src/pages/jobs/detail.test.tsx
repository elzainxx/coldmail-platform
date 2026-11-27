import { act, fireEvent, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Job, JobStatus, ModuleId } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { formatDateTime, jobName } from './format.ts'
import { JOB_ID, jobId, makeEvent, makeJob, makePreview } from './fixtures.ts'

const ME = { 'GET /v1/me': { email: 'ann@example.test' } }
const EVENTS = (id: string) => `GET /v1/jobs/${id}/events`
const RESULTS = (id: string) => `GET /v1/jobs/${id}/results`
const JOB = (id: string) => `GET /v1/jobs/${id}`

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

type Routes = Record<string, unknown>

/** An error answer: a function, so that every request gets a new Response (a body can be read once). The server's own sentence is a marker the tests look for on screen. */
function failing(status: number, code: string, details?: Record<string, unknown>, headers: Record<string, string> = {}) {
  return () =>
    new Response(
      JSON.stringify({ error: { code, message: 'SERVER-SAID', request_id: 'req_0123456789abcdef0123456789abcdef', ...(details === undefined ? {} : { details }) } }),
      { status, headers },
    )
}

/** The three reads a job page makes, plus the user's address; `extra` adds or replaces routes. */
function detailApi(job: Job, extra: Routes = {}) {
  return mockApi({
    ...ME,
    [JOB(job.id)]: () => ({ job: current.get(job.id) ?? job }),
    [EVENTS(job.id)]: { events: [], next_after: null },
    [RESULTS(job.id)]: { results: [], next_cursor: null },
    ...extra,
  } as Parameters<typeof mockApi>[0])
}
/** What `GET /v1/jobs/{id}` answers now: a test changes it to move the job on. */
const current = new Map<string, Job>()
beforeEach(() => current.clear())

function posts(fetchMock: ReturnType<typeof mockApi>, path: string): unknown[] {
  return (fetchMock.mock.calls as Array<[string, RequestInit]>).filter(([url, init]) => init?.method === 'POST' && new URL(url, 'http://app.test').pathname === path)
}

async function open(job: Job, extra: Routes = {}) {
  const api = detailApi(job, extra)
  renderApp(`/jobs/${job.id}`)
  await screen.findByTestId(`job-status-${job.id}`)
  return api
}

describe('the job page: the header and what is happening now', () => {
  it('has its one heading at once, and then names the job by its kind and date, with its status, dates and size', async () => {
    const job = makeJob({ module: 'm2', status: 'succeeded', preview: makePreview({ rows: 12345 }) })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    expect(screen.getByRole('heading', { level: 1, name: t('jobs.detail.title') })).toBeInTheDocument()
    await screen.findByTestId(`job-status-${job.id}`)
    const main = screen.getByRole('main')
    expect(within(main).getByText(jobName(job))).toBeInTheDocument()
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Finished')
    expect(within(main).getByText(t('jobs.count.other', { count: '12,345' }))).toBeInTheDocument()
    expect(within(main).getByText(t('jobs.kind.m2'), { selector: 'dd' })).toBeInTheDocument()
    for (const [label, iso] of [
      [t('jobs.field.created'), job.created_at],
      [t('jobs.field.started'), job.started_at],
      [t('jobs.field.finished'), job.finished_at],
    ] as const) {
      const term = within(main).getByText(label, { selector: 'dt' })
      expect(term.nextElementSibling).toHaveTextContent(formatDateTime(iso))
    }
    expect(within(main).getByRole('link', { name: t('jobs.detail.back') })).toHaveAttribute('href', '/jobs')
  })

  it('never shows a kind slug, a status slug or the job id', async () => {
    const job = makeJob({ module: 'm3', status: 'cancelling', preview: makePreview({ metered: false, credits: 0 }) })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    const text = screen.getByRole('main').textContent ?? ''
    expect(text).toContain(t('jobs.kind.m3'))
    for (const raw of ['m3', 'cancelling', job.id, 'module', 'paused_from']) expect(text, raw).not.toContain(raw)
  })

  it.each<[JobStatus, string]>([
    ['draft', t('jobs.now.draft')],
    ['previewed', t('jobs.now.previewed')],
    ['confirmed', t('jobs.now.confirmed')],
    ['cancelling', t('jobs.now.cancelling')],
    ['completing', t('jobs.now.completing')],
    ['succeeded', t('jobs.now.succeeded')],
    ['cancelled', t('jobs.now.cancelled')],
    ['expired', t('jobs.now.expired')],
    ['failed', t('jobs.now.failed')],
  ])('says in words what a %s job is doing', async (status, sentence) => {
    const job = makeJob({ status })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    expect(screen.getByText(sentence)).toBeInTheDocument()
    expect(sentence).not.toBe(t('jobs.now.other'))
  })

  it('says it by kind and by where a pause began, and names the start of a scheduled job', async () => {
    const scheduled = makeJob({ status: 'scheduled', scheduled_start: '2026-12-05T10:00:00Z', started_at: null, finished_at: null })
    detailApi(scheduled)
    renderApp(`/jobs/${scheduled.id}`)
    await screen.findByTestId(`job-status-${scheduled.id}`)
    expect(screen.getByText(t('jobs.now.scheduled', { date: formatDateTime(scheduled.scheduled_start) }))).toBeInTheDocument()
  })

  it.each<[Partial<Job>, string]>([
    [{ status: 'running', module: 'm1' }, t('jobs.now.running.m1')],
    [{ status: 'running', module: 'm2' }, t('jobs.now.running.send')],
    [{ status: 'running', module: 'm3' }, t('jobs.now.running.send')],
    [{ status: 'paused', paused_from: 'scheduled' }, t('jobs.now.paused.scheduled')],
    [{ status: 'paused', paused_from: 'running' }, t('jobs.now.paused.running')],
  ])('says it for %j', async (over, sentence) => {
    const job = makeJob({ finished_at: null, ...over })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    expect(screen.getByText(sentence)).toBeInTheDocument()
  })

  it('shows the subject of a send job (as text, never as markup) and the list it was taken from', async () => {
    const job = makeJob({
      module: 'm2',
      source_job_id: jobId(9),
      content: { subject: 'Hello <b>there</b>', html: '<p>x</p>', text: 'x', reply_to: 'me@example.test' },
    })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    const main = screen.getByRole('main')
    expect(within(main).getByText('Hello <b>there</b>')).toBeInTheDocument()
    expect(main.querySelector('dd b')).toBeNull()
    expect(within(main).getByRole('link', { name: t('jobs.field.sourceLink') })).toHaveAttribute('href', `/jobs/${jobId(9)}`)
  })

  it('says why the template is gone, once, after the content was cleared, and shows no subject then', async () => {
    const job = makeJob({
      module: 'm2',
      finished_at: '2026-12-01T09:10:00Z',
      content_cleared_at: '2026-12-08T09:10:00Z',
      content: { subject: '[removed]', html: '[removed]', text: '', reply_to: 'me@example.test' },
    })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    const main = screen.getByRole('main')
    expect(within(main).getAllByText(t('jobs.cleared.send', { after: '7 days' }))).toHaveLength(1)
    expect(main.textContent).not.toContain('[removed]')
    expect(within(main).queryByText(t('jobs.field.subject'))).not.toBeInTheDocument()
    expect(within(main).getByText(t('jobs.csv.noteCleared.send'))).toBeInTheDocument()
    expect(within(main).queryByText(t('jobs.csv.note'))).not.toBeInTheDocument()
  })

  it('paired control: before the content is cleared there is no such sentence, the subject is shown and the file note is the normal one', async () => {
    const job = makeJob({ module: 'm2', content: { subject: 'A subject', html: '<p>x</p>', text: 'x', reply_to: 'me@example.test' } })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    const main = screen.getByRole('main')
    expect(main.textContent).not.toContain('were removed')
    expect(within(main).getByText('A subject')).toBeInTheDocument()
    expect(within(main).getByText(t('jobs.csv.note'))).toBeInTheDocument()
    expect(within(main).queryByText(t('jobs.csv.noteCleared.send'))).not.toBeInTheDocument()
  })

  it('words the cleared content of a verification job without a message, and counts the days it was kept', async () => {
    const job = makeJob({ module: 'm1', finished_at: '2026-12-01T09:10:00Z', content_cleared_at: '2026-12-02T09:10:00Z' })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    expect(screen.getByText(t('jobs.cleared.verify', { after: '1 day' }))).toBeInTheDocument()
    expect(screen.getByText(t('jobs.csv.noteCleared.verify'))).toBeInTheDocument()
    expect(screen.queryByText(/The message and/)).not.toBeInTheDocument() // a verification job never had one
  })

  it('a job of another user, or none, is the not found line, and not the server message or the code', async () => {
    mockApi({ ...ME, [JOB(JOB_ID)]: failing(404, 'not_found') })
    renderApp(`/jobs/${JOB_ID}`)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.not_found'))
    const text = document.body.textContent ?? ''
    expect(text).not.toContain('SERVER-SAID')
    expect(text).not.toContain('not_found')
    expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
  })
})

describe('a job that was not started', () => {
  it.each<[ModuleId, string]>([
    ['m1', `/new/verify?job=${JOB_ID}`],
    ['m2', `/new/send?job=${JOB_ID}`],
    ['m3', `/new/send?job=${JOB_ID}`],
  ])('a draft of kind %s links back to its screen: %s', async (module, href) => {
    const job = makeJob({ module, status: 'draft', preview: null, started_at: null, finished_at: null })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    expect(screen.getByRole('link', { name: t('jobs.continue.link') })).toHaveAttribute('href', href)
    expect(screen.queryByRole('group', { name: t('jobs.controls.label') })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: t('jobs.csv.button') })).not.toBeInTheDocument() // nothing to download yet
  })

  it('a previewed job links back too; paired control: a running job does not', async () => {
    const previewed = makeJob({ status: 'previewed', started_at: null, finished_at: null })
    detailApi(previewed)
    const { unmount } = renderApp(`/jobs/${previewed.id}`)
    await screen.findByTestId(`job-status-${previewed.id}`)
    expect(screen.getByRole('link', { name: t('jobs.continue.link') })).toBeInTheDocument()
    unmount()
    resetForTest()
    await signInForTest()
    const running = makeJob({ id: jobId(2), status: 'running', finished_at: null })
    detailApi(running)
    renderApp(`/jobs/${running.id}`)
    await screen.findByTestId(`job-status-${running.id}`)
    expect(screen.queryByRole('link', { name: t('jobs.continue.link') })).not.toBeInTheDocument()
  })
})

describe('the controls a job allows', () => {
  type Row = { name: string; over: Partial<Job>; buttons: string[] }
  const P = t('jobs.control.pause')
  const R = t('jobs.control.resume')
  const C = t('jobs.control.cancel')
  const rows: Row[] = [
    { name: 'a confirmed job', over: { status: 'confirmed' }, buttons: [C] },
    { name: 'a scheduled verification job', over: { status: 'scheduled', module: 'm1' }, buttons: [P, C] },
    { name: 'a scheduled managed sending job', over: { status: 'scheduled', module: 'm2' }, buttons: [P, C] },
    { name: 'a running verification job (it has no pause)', over: { status: 'running', module: 'm1' }, buttons: [C] },
    { name: 'a running managed sending job', over: { status: 'running', module: 'm2' }, buttons: [P, C] },
    { name: 'a running own SMTP job', over: { status: 'running', module: 'm3' }, buttons: [P, C] },
    { name: 'a job paused before its start', over: { status: 'paused', paused_from: 'scheduled', start_deadline: '2999-01-01T00:00:00Z' }, buttons: [R, C] },
    { name: 'a running job that was paused', over: { status: 'paused', paused_from: 'running', module: 'm2' }, buttons: [R, C] },
    { name: 'a job that is being cancelled', over: { status: 'cancelling' }, buttons: [] },
    { name: 'a job that is finishing', over: { status: 'completing' }, buttons: [] },
    { name: 'a finished job', over: { status: 'succeeded' }, buttons: [] },
    { name: 'a cancelled job', over: { status: 'cancelled' }, buttons: [] },
    { name: 'an expired job', over: { status: 'expired' }, buttons: [] },
    { name: 'a failed job', over: { status: 'failed' }, buttons: [] },
  ]

  it.each(rows)('$name offers exactly $buttons', async ({ over, buttons }) => {
    const job = makeJob({ finished_at: null, ...over })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    const group = screen.queryByRole('group', { name: t('jobs.controls.label') })
    if (buttons.length === 0) {
      expect(group).not.toBeInTheDocument()
      return
    }
    expect(within(group as HTMLElement).getAllByRole('button').map((b) => b.textContent)).toEqual(buttons)
  })

  it('says calmly why a running verification job cannot be paused, and says nothing of the kind for a job that can', async () => {
    const m1 = makeJob({ status: 'running', module: 'm1', finished_at: null })
    detailApi(m1)
    renderApp(`/jobs/${m1.id}`)
    await screen.findByTestId(`job-status-${m1.id}`)
    expect(screen.getByText(t('jobs.controls.noPause.m1'))).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: t('jobs.control.pause') })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('jobs.control.cancel') })).toBeInTheDocument()
  })

  it('paired control: a running managed sending job has the pause and no such sentence', async () => {
    const m2 = makeJob({ status: 'running', module: 'm2', finished_at: null })
    detailApi(m2)
    renderApp(`/jobs/${m2.id}`)
    await screen.findByTestId(`job-status-${m2.id}`)
    expect(screen.queryByText(t('jobs.controls.noPause.m1'))).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('jobs.control.pause') })).toBeInTheDocument()
  })

  it('does not offer the resume of a job paused before its start when that start has passed, and says why (paired control: a start still ahead is offered)', async () => {
    const late = makeJob({ status: 'paused', paused_from: 'scheduled', start_deadline: '2020-01-01T00:00:00Z', finished_at: null })
    detailApi(late)
    const { unmount } = renderApp(`/jobs/${late.id}`)
    await screen.findByTestId(`job-status-${late.id}`)
    expect(screen.queryByRole('button', { name: t('jobs.control.resume') })).not.toBeInTheDocument()
    expect(screen.getByText(t('jobs.controls.noResume.deadline'))).toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('jobs.control.cancel') })).toBeInTheDocument()
    unmount()

    resetForTest()
    await signInForTest()
    const early = makeJob({ id: jobId(2), status: 'paused', paused_from: 'scheduled', start_deadline: '2999-01-01T00:00:00Z', finished_at: null })
    detailApi(early)
    renderApp(`/jobs/${early.id}`)
    await screen.findByTestId(`job-status-${early.id}`)
    expect(screen.getByRole('button', { name: t('jobs.control.resume') })).toBeInTheDocument()
    expect(screen.queryByText(t('jobs.controls.noResume.deadline'))).not.toBeInTheDocument()
  })

  it('tells a paused job it cannot stay paused for ever', async () => {
    const job = makeJob({ status: 'paused', paused_from: 'running', module: 'm3', finished_at: null })
    detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await screen.findByTestId(`job-status-${job.id}`)
    expect(screen.getByText(t('jobs.controls.pausedLimit'))).toBeInTheDocument()
  })
})

describe('pause and resume', () => {
  it('Pause posts once, shows the job as paused at once and offers Resume in its place', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const paused = makeJob({ module: 'm2', status: 'paused', paused_from: 'running', finished_at: null })
    const api = await open(job, { [`POST /v1/jobs/${job.id}/pause`]: { job: paused } })
    expect(posts(api, `/v1/jobs/${job.id}/pause`)).toHaveLength(0) // nothing is posted until the click
    const pause = screen.getByRole('button', { name: t('jobs.control.pause') })
    fireEvent.click(pause)
    fireEvent.click(pause) // a double click
    expect(await screen.findByRole('button', { name: t('jobs.control.resume') })).toBeInTheDocument()
    expect(posts(api, `/v1/jobs/${job.id}/pause`)).toHaveLength(1)
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Paused')
    expect(screen.queryByRole('button', { name: t('jobs.control.pause') })).not.toBeInTheDocument()
    expect(screen.getByText(t('jobs.toast.paused'))).toBeInTheDocument()
  })

  it('Resume posts once and the job runs again', async () => {
    const job = makeJob({ module: 'm3', status: 'paused', paused_from: 'running', finished_at: null })
    const running = makeJob({ module: 'm3', status: 'running', finished_at: null })
    const api = await open(job, { [`POST /v1/jobs/${job.id}/resume`]: { job: running } })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.resume') }))
    expect(await screen.findByRole('button', { name: t('jobs.control.pause') })).toBeInTheDocument()
    expect(posts(api, `/v1/jobs/${job.id}/resume`)).toHaveLength(1)
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Running')
    expect(screen.getByText(t('jobs.toast.resumedRunning'))).toBeInTheDocument()
  })

  it('when the platform refuses the pause for a reason the screen now knows, the control goes and the line is the calm one, never the reason', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const refusal = failing(409, 'not_allowed', { reason: 'pause_not_supported' })
    await open(job, { [`POST /v1/jobs/${job.id}/pause`]: refusal })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.pause') }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.not_allowed.pause_not_supported'))
    expect(screen.queryByRole('button', { name: t('jobs.control.pause') })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('jobs.control.cancel') })).toBeInTheDocument() // the others stay
    const text = document.body.textContent ?? ''
    for (const raw of ['SERVER-SAID', 'pause_not_supported', 'not_allowed']) expect(text, raw).not.toContain(raw)
  })

  it('paired control: a refusal for a reason that is not known (the job moved on) leaves the controls as they are and offers the page as it now stands', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const api = await open(job, {
      [`POST /v1/jobs/${job.id}/pause`]: failing(409, 'not_allowed', { reason: 'not_allowed' }),
    })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.pause') }))
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.not_allowed'))
    expect(screen.getByRole('button', { name: t('jobs.control.pause') })).toBeInTheDocument()
    await vi.waitFor(() => expect((api.mock.calls as Array<[string]>).filter(([u]) => u === `/v1/jobs/${job.id}`).length).toBeGreaterThanOrEqual(2)) // read again
  })

  it('a resume the platform refuses because the start has passed takes the Resume away', async () => {
    const job = makeJob({ status: 'paused', paused_from: 'scheduled', start_deadline: '2999-01-01T00:00:00Z', finished_at: null })
    await open(job, {
      [`POST /v1/jobs/${job.id}/resume`]: failing(409, 'not_allowed', { reason: 'start_deadline_passed' }),
    })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.resume') }))
    await screen.findByRole('alert')
    expect(screen.queryByRole('button', { name: t('jobs.control.resume') })).not.toBeInTheDocument()
    expect(screen.getAllByText(t('jobs.controls.noResume.deadline')).length).toBeGreaterThanOrEqual(1)
  })
})

describe('cancel', () => {
  it('asks first, in plain words about the items not done and the credits, and posts nothing until the user confirms', async () => {
    const job = makeJob({ module: 'm1', status: 'running', finished_at: null })
    const api = await open(job, { [`POST /v1/jobs/${job.id}/cancel`]: { job: makeJob({ status: 'cancelling', finished_at: null }) } })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    const dialog = screen.getByRole('alertdialog', { name: t('jobs.cancel.title') })
    expect(within(dialog).getByText(t('jobs.cancel.started.m1'))).toBeInTheDocument()
    expect(within(dialog).getByText(t('jobs.cancel.stopping'))).toBeInTheDocument()
    expect(dialog.textContent).toMatch(/credits/)
    expect(dialog.textContent).toMatch(/cannot be undone/)
    expect(posts(api, `/v1/jobs/${job.id}/cancel`)).toHaveLength(0)
  })

  it('Keep the job closes the question and posts nothing (paired control: the confirm button does post)', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const api = await open(job, { [`POST /v1/jobs/${job.id}/cancel`]: { job: makeJob({ module: 'm2', status: 'cancelling', finished_at: null }) } })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: t('jobs.cancel.keep') }))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(posts(api, `/v1/jobs/${job.id}/cancel`)).toHaveLength(0)
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Running')

    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: t('jobs.cancel.confirm') }))
    await vi.waitFor(() => expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Cancelling'))
    expect(posts(api, `/v1/jobs/${job.id}/cancel`)).toHaveLength(1)
  })

  it('posts once however many times the confirm is pressed, then shows the job cancelling, closes the question and says what was asked', async () => {
    const job = makeJob({ module: 'm3', status: 'running', finished_at: null })
    const api = await open(job, { [`POST /v1/jobs/${job.id}/cancel`]: { job: makeJob({ module: 'm3', status: 'cancelling', finished_at: null }) } })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    const confirm = within(screen.getByRole('alertdialog')).getByRole('button', { name: t('jobs.cancel.confirm') })
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    await vi.waitFor(() => expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Cancelling'))
    expect(posts(api, `/v1/jobs/${job.id}/cancel`)).toHaveLength(1)
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.queryByRole('group', { name: t('jobs.controls.label') })).not.toBeInTheDocument() // nothing more to press
    expect(screen.getByText(t('jobs.toast.cancelling'))).toBeInTheDocument()
  })

  it('a job that had not started says nothing was processed and the credits come back; one of your own SMTP says nothing was sent', async () => {
    const scheduled = makeJob({ module: 'm2', status: 'scheduled', scheduled_start: '2999-01-01T00:00:00Z', finished_at: null, started_at: null })
    detailApi(scheduled)
    const { unmount } = renderApp(`/jobs/${scheduled.id}`)
    await screen.findByTestId(`job-status-${scheduled.id}`)
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    const dialog = screen.getByRole('alertdialog')
    expect(within(dialog).getByText(t('jobs.cancel.notStarted.credits'))).toBeInTheDocument()
    expect(within(dialog).queryByText(t('jobs.cancel.stopping'))).not.toBeInTheDocument()
    unmount()

    resetForTest()
    await signInForTest()
    const own = makeJob({ id: jobId(2), module: 'm3', status: 'scheduled', scheduled_start: '2999-01-01T00:00:00Z', finished_at: null, started_at: null })
    detailApi(own)
    renderApp(`/jobs/${own.id}`)
    await screen.findByTestId(`job-status-${own.id}`)
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    const other = screen.getByRole('alertdialog')
    expect(within(other).getByText(t('jobs.cancel.notStarted.m3'))).toBeInTheDocument()
    expect(other.textContent).not.toMatch(/credits/)
  })

  it('a job paused before its start is cancelled as one that had not started; one paused while running as one that had', async () => {
    const before = makeJob({ module: 'm2', status: 'paused', paused_from: 'scheduled', start_deadline: '2999-01-01T00:00:00Z', finished_at: null })
    detailApi(before)
    const { unmount } = renderApp(`/jobs/${before.id}`)
    await screen.findByTestId(`job-status-${before.id}`)
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    expect(within(screen.getByRole('alertdialog')).getByText(t('jobs.cancel.notStarted.credits'))).toBeInTheDocument()
    unmount()

    resetForTest()
    await signInForTest()
    const during = makeJob({ id: jobId(2), module: 'm2', status: 'paused', paused_from: 'running', finished_at: null })
    detailApi(during)
    renderApp(`/jobs/${during.id}`)
    await screen.findByTestId(`job-status-${during.id}`)
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    expect(within(screen.getByRole('alertdialog')).getByText(t('jobs.cancel.started.m2'))).toBeInTheDocument()
  })

  it('a cancel the platform cannot do takes the Cancel away and says so calmly', async () => {
    const job = makeJob({ module: 'm1', status: 'running', finished_at: null })
    await open(job, {
      [`POST /v1/jobs/${job.id}/cancel`]: failing(409, 'not_allowed', { reason: 'cancel_not_supported' }),
    })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: t('jobs.cancel.confirm') }))
    await vi.waitFor(() => expect(screen.queryByRole('button', { name: t('jobs.control.cancel') })).not.toBeInTheDocument())
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent(t('error.not_allowed.cancel_not_supported'))
    expect(screen.getAllByText(t('jobs.controls.noCancel')).length).toBeGreaterThanOrEqual(1)
    expect(document.body.textContent).not.toContain('cancel_not_supported')
  })

  it('any other failure closes the question, says it in the catalogue words and leaves the controls', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    await open(job, { [`POST /v1/jobs/${job.id}/cancel`]: failing(503, 'busy', undefined, { 'Retry-After': '0' }) })
    fireEvent.click(screen.getByRole('button', { name: t('jobs.control.cancel') }))
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: t('jobs.cancel.confirm') }))
    expect(await screen.findByRole('alert', {}, { timeout: 5_000 })).toHaveTextContent(t('error.busy'))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('jobs.control.cancel') })).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('SERVER-SAID')
  }, 15_000)
})

describe('by keyboard', () => {
  it('every control is a real button in the tab order, and the cancel question takes focus on its safe button', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    await open(job)
    const group = screen.getByRole('group', { name: t('jobs.controls.label') })
    for (const button of within(group).getAllByRole('button')) {
      expect(button.tagName).toBe('BUTTON')
      expect(button.getAttribute('tabindex')).not.toBe('-1')
      button.focus()
      expect(button).toHaveFocus()
    }
    const cancel = within(group).getByRole('button', { name: t('jobs.control.cancel') })
    cancel.focus()
    fireEvent.click(cancel) // Enter and Space on a button are this click in a browser
    expect(within(screen.getByRole('alertdialog')).getByRole('button', { name: t('jobs.cancel.keep') })).toHaveFocus() // Enter never confirms by accident
  })

  it('Escape closes the question without cancelling and gives focus back to the Cancel button; Tab stays inside the question', async () => {
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const api = await open(job, { [`POST /v1/jobs/${job.id}/cancel`]: { job: makeJob({ status: 'cancelling' }) } })
    const cancel = screen.getByRole('button', { name: t('jobs.control.cancel') })
    cancel.focus()
    fireEvent.click(cancel)
    const dialog = screen.getByRole('alertdialog')

    // Tab from the last control goes to the first, Shift+Tab from the first to the last
    const confirm = within(dialog).getByRole('button', { name: t('jobs.cancel.confirm') })
    confirm.focus()
    expect(fireEvent.keyDown(confirm, { key: 'Tab' })).toBe(false)
    const first = within(dialog).getByRole('button', { name: t('common.dialog.close') })
    expect(first).toHaveFocus()
    expect(fireEvent.keyDown(first, { key: 'Tab', shiftKey: true })).toBe(false)
    expect(confirm).toHaveFocus()

    fireEvent.keyDown(confirm, { key: 'Escape' })
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(cancel).toHaveFocus()
    expect(posts(api, `/v1/jobs/${job.id}/cancel`)).toHaveLength(0)
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Running')
  })
})

describe('the history', () => {
  it('tells every kind of event in words, oldest first, and never the platform\'s own reason', async () => {
    const job = makeJob()
    const events = [
      makeEvent(1, { kind: 'created', from_status: null, to_status: null, actor_kind: 'user', reason: 'LEAK-created-reason' }),
      makeEvent(2, { from_status: 'draft', to_status: 'previewed', actor_kind: 'user', reason: 'LEAK-previewed' }),
      makeEvent(3, { from_status: 'previewed', to_status: 'confirmed', actor_kind: 'user', reason: 'confirmed by the user' }),
      makeEvent(4, { from_status: 'confirmed', to_status: 'running', reason: 'LEAK-engine-submitted' }),
      makeEvent(5, { kind: 'stale', from_status: null, to_status: null, reason: 'LEAK-stale: no progress' }),
      makeEvent(6, { kind: 'flag', from_status: null, to_status: null, reason: 'LEAK-flag the vendor answered for an unreachable unit' }),
      makeEvent(7, { kind: 'note', from_status: null, to_status: null, reason: 'module_paused: the module reports the job\'s campaign held; LEAK-note' }),
      makeEvent(8, { kind: 'note', from_status: null, to_status: null, reason: 'module_resumed: LEAK-resumed' }),
      makeEvent(9, { kind: 'note', from_status: null, to_status: null, reason: 'pause_limit: the job was paused for longer than the limit of 7 days LEAK-limit' }),
      makeEvent(10, { kind: 'note', from_status: null, to_status: null, reason: 'LEAK-an-unknown-note' }),
      makeEvent(11, { from_status: 'running', to_status: 'completing', reason: 'LEAK-completing' }),
      makeEvent(12, { from_status: 'completing', to_status: 'succeeded', reason: 'every result is recorded: the job succeeded' }),
    ]
    detailApi(job, { [EVENTS(job.id)]: { events, next_after: null } })
    renderApp(`/jobs/${job.id}`)
    const history = (await screen.findByRole('heading', { name: t('jobs.history.title') })).closest('section') as HTMLElement
    await vi.waitFor(() => expect(within(history).getAllByRole('listitem').length).toBeGreaterThan(0))
    const lines = within(history).getAllByRole('listitem').map((li) => li.textContent ?? '')
    const expected = [
      t('jobs.event.created'),
      t('jobs.event.previewed'),
      t('jobs.event.confirmed'),
      t('jobs.event.started'),
      t('jobs.event.stale'),
      t('jobs.event.flag'),
      t('jobs.event.note.paused'),
      t('jobs.event.note.resumed'),
      // the note beside a pause limit is for the operator and is left out
      t('jobs.event.note.other'),
      t('jobs.event.completing'),
      t('jobs.event.succeeded'),
    ]
    expect(lines).toHaveLength(expected.length)
    expected.forEach((sentence, i) => expect(lines[i], sentence).toContain(sentence))
    const text = history.textContent ?? ''
    expect(text).not.toContain('LEAK')
    expect(text).not.toContain('module_paused')
    expect(text).not.toContain('pause_limit')
    expect(text).not.toContain('confirmed by the user')
  })

  it('an event of a kind it does not know is one generic line; an empty history says so', async () => {
    const job = makeJob()
    detailApi(job, { [EVENTS(job.id)]: { events: [makeEvent(1, { kind: 'teleport' as never, from_status: null, to_status: null, reason: 'LEAK-teleport' })], next_after: null } })
    const { unmount } = renderApp(`/jobs/${job.id}`)
    const history = (await screen.findByRole('heading', { name: t('jobs.history.title') })).closest('section') as HTMLElement
    await vi.waitFor(() => expect(within(history).getByText(t('jobs.event.other'))).toBeInTheDocument())
    expect(history.textContent).not.toContain('teleport')
    expect(history.textContent).not.toContain('LEAK')
    unmount()

    resetForTest()
    await signInForTest()
    detailApi(job) // no events
    renderApp(`/jobs/${job.id}`)
    const empty = (await screen.findByRole('heading', { name: t('jobs.history.title') })).closest('section') as HTMLElement
    await vi.waitFor(() => expect(within(empty).getByText(t('jobs.history.empty'))).toBeInTheDocument())
  })

  it('reads the next page of events when the first was full', async () => {
    const job = makeJob()
    const api = detailApi(job, {
      [EVENTS(job.id)]: (url: URL) =>
        url.searchParams.get('after') === '1'
          ? { events: [makeEvent(2, { from_status: 'running', to_status: 'completing' })], next_after: null }
          : { events: [makeEvent(1, { from_status: 'confirmed', to_status: 'running' })], next_after: 1 },
    })
    renderApp(`/jobs/${job.id}`)
    const history = (await screen.findByRole('heading', { name: t('jobs.history.title') })).closest('section') as HTMLElement
    await vi.waitFor(() => expect(within(history).getAllByRole('listitem')).toHaveLength(2))
    expect(within(history).getByText(t('jobs.event.completing'))).toBeInTheDocument()
    expect((api.mock.calls as Array<[string]>).some(([u]) => u.includes('after=1'))).toBe(true)
  })
})

describe('the job page polls', () => {
  async function advance(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }
  const jobCalls = (api: ReturnType<typeof mockApi>, id: string) => (api.mock.calls as Array<[string, RequestInit]>).filter(([u]) => new URL(u, 'http://app.test').pathname === `/v1/jobs/${id}`).length

  it('asks for a running job every 3 seconds and shows what changed', async () => {
    vi.useFakeTimers()
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const api = detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await advance(0)
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Running')
    const before = jobCalls(api, job.id)
    current.set(job.id, makeJob({ module: 'm2', status: 'succeeded' }))
    await advance(3_100)
    expect(jobCalls(api, job.id)).toBe(before + 1)
    expect(screen.getByTestId(`job-status-${job.id}`)).toHaveTextContent('Finished')
    expect(screen.queryByRole('group', { name: t('jobs.controls.label') })).not.toBeInTheDocument() // the controls went with the state
  })

  it('asks every 15 seconds for a job that is waiting (scheduled or paused), and not for its results at all (paired control: a running one every 3)', async () => {
    vi.useFakeTimers()
    const waiting = makeJob({ status: 'scheduled', scheduled_start: '2999-01-01T00:00:00Z', started_at: null, finished_at: null })
    const api = detailApi(waiting)
    const first = renderApp(`/jobs/${waiting.id}`)
    await advance(0)
    const results = () => (api.mock.calls as Array<[string, RequestInit]>).filter(([u]) => new URL(u, 'http://app.test').pathname === `/v1/jobs/${waiting.id}/results`).length
    const before = jobCalls(api, waiting.id)
    const resultsBefore = results()
    await advance(14_000)
    expect(jobCalls(api, waiting.id)).toBe(before) // five rounds of three seconds: nothing
    await advance(1_100)
    expect(jobCalls(api, waiting.id)).toBe(before + 1)
    await advance(60_000)
    expect(jobCalls(api, waiting.id)).toBe(before + 5) // one every fifteen seconds
    expect(results()).toBe(resultsBefore) // nothing is moving, so nothing is asked of the results
    first.unmount()

    const paused = makeJob({ id: jobId(2), status: 'paused', paused_from: 'running', finished_at: null })
    const api2 = detailApi(paused)
    const second = renderApp(`/jobs/${paused.id}`)
    await advance(0)
    const pausedBefore = jobCalls(api2, paused.id)
    await advance(15_100)
    expect(jobCalls(api2, paused.id)).toBe(pausedBefore + 1)
    second.unmount()

    const running = makeJob({ id: jobId(3), module: 'm2', status: 'running', finished_at: null })
    const api3 = detailApi(running)
    renderApp(`/jobs/${running.id}`)
    await advance(0)
    const runningBefore = jobCalls(api3, running.id)
    await advance(15_100)
    expect(jobCalls(api3, running.id)).toBe(runningBefore + 5)
  })

  it('does not ask again for a finished job (paired control: the same minute for a running one does)', async () => {
    vi.useFakeTimers()
    const done = makeJob({ status: 'succeeded' })
    const api = detailApi(done)
    renderApp(`/jobs/${done.id}`)
    await advance(0)
    const before = jobCalls(api, done.id)
    await advance(60_000)
    expect(jobCalls(api, done.id)).toBe(before)
  })

  it('a running job is asked for again, and nothing is asked while the tab is hidden', async () => {
    vi.useFakeTimers()
    const job = makeJob({ module: 'm2', status: 'running', finished_at: null })
    const api = detailApi(job)
    renderApp(`/jobs/${job.id}`)
    await advance(0)
    const before = jobCalls(api, job.id)
    await advance(30_000)
    expect(jobCalls(api, job.id)).toBeGreaterThan(before + 5)
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => true })
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    const hiddenAt = jobCalls(api, job.id)
    await advance(60_000)
    expect(jobCalls(api, job.id)).toBe(hiddenAt)
  })
})
