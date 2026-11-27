import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { IDEMPOTENCY_KEY_PATTERN } from '../../api/idempotency.ts'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { localInputValue } from './schedule.ts'
import { bodyOf, callsTo, confirmResult, csvFile, expectNoCodeWords, job, keyboardProblems, preview, sendJob } from './testing/fixtures.ts'

beforeEach(async () => {
  resetForTest()
  await signInForTest()
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
  resetForTest()
})

const LIST = 'Name,work mail,City\nAnn,a@x.test,Paris\nBo,b@x.test,Rome\n'

type Routes = Record<string, unknown>

/** The platform's side of a verification: a draft is made, previewed and confirmed. Each route can be replaced. */
function platform(over: Routes = {}): Routes {
  const draft = job({ status: 'draft', preview: null })
  return {
    'GET /v1/me': { email: 'person@example.test' },
    'GET /v1/settings': { vendor_fallback_opt_out: false },
    'POST /v1/jobs': [201, { job: draft }],
    'POST /v1/jobs/job-1/preview': { job: job() },
    'POST /v1/jobs/job-1/confirm': () => confirmResult(job()),
    'POST /v1/jobs/job-1/discard': { job: job({ status: 'cancelled' }) },
    ...over,
  }
}

async function open(routes: Routes = platform(), path = '/new/verify') {
  const fetchFn = mockApi(routes as Parameters<typeof mockApi>[0])
  renderApp(path)
  await screen.findByRole('heading', { level: 1, name: t('newjob.verify.title') })
  return fetchFn
}

function pick(file: File) {
  fireEvent.change(screen.getByLabelText(t('newjob.csv.file')), { target: { files: [file] } })
}

const checkButton = () => screen.getByRole('button', { name: t('newjob.verify.check') })

/** Choose a file, accept its columns, ask for the preview and wait for it. */
async function toPreview(file: File = csvFile(LIST)) {
  pick(file)
  await screen.findByLabelText(t('newjob.columns.address'))
  fireEvent.click(checkButton())
  await screen.findByRole('heading', { name: t('newjob.preview.title') })
}

/** The figure beside a label of the preview (a definition list). */
function figure(label: string): string {
  const dt = screen.getByText(label, { selector: 'dt' })
  return dt.nextElementSibling?.textContent ?? ''
}

describe('the verify page: the file and its columns', () => {
  it('is a protected page with one h1, registered at /new/verify with a title from the catalogue', async () => {
    const { routes } = await import('./routes.ts')
    expect(routes.map((r) => r.path)).toEqual(['/new/verify', '/new/send'])
    expect(routes.every((r) => r.protected)).toBe(true)
    await open()
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
    expect(document.title).toBe(`${t('newjob.verify.title')} - ${t('common.brand')}`)
  })

  it('reads the start of the file, pre-selects the column that says mail, and lists the other columns as the merge fields they become', async () => {
    await open()
    expect(checkButton()).toBeDisabled() // no file yet
    pick(csvFile(LIST))
    const select = (await screen.findByLabelText(t('newjob.columns.address'))) as HTMLSelectElement
    expect(select.value).toBe('1') // "work mail" contains mail
    expect(screen.getByText('Rows found: 2. Columns: 3.')).toBeInTheDocument()
    const merge = screen.getByRole('list', { name: t('newjob.columns.mergeTitleVerify') })
    expect(within(merge).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['Name', 'City'])
    // the first rows of the file are shown, so the user can see the column is right
    expect(within(screen.getByRole('table')).getByText('a@x.test')).toBeInTheDocument()
    expect(checkButton()).toBeEnabled()
  })

  it('control: a header that says nothing about mail pre-selects nothing and asks the user to choose; choosing a column makes the others the merge fields', async () => {
    await open()
    pick(csvFile('first,second,third\nx,y,z\n'))
    const select = (await screen.findByLabelText(t('newjob.columns.address'))) as HTMLSelectElement
    expect(select.value).toBe('')
    expect(screen.getByRole('option', { name: t('newjob.columns.choose') })).toBeInTheDocument()
    expect(checkButton()).toBeDisabled()
    expect(screen.getByText(t('newjob.verify.needColumn'))).toBeInTheDocument()

    fireEvent.change(select, { target: { value: '2' } })
    expect(select.value).toBe('2')
    const merge = screen.getByRole('list', { name: t('newjob.columns.mergeTitleVerify') })
    expect(within(merge).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['first', 'second'])
    expect(checkButton()).toBeEnabled()
  })

  it('takes a file dropped on the box as well as one chosen with the button', async () => {
    await open()
    const box = screen.getByText(t('newjob.csv.dropHint')).parentElement as HTMLElement
    fireEvent.dragOver(box)
    expect(screen.getByText(t('newjob.csv.dropActive'))).toBeInTheDocument()
    fireEvent.drop(box, { dataTransfer: { files: [csvFile(LIST)] } })
    expect(await screen.findByLabelText(t('newjob.columns.address'))).toBeInTheDocument()
    expect(screen.getByText(/list\.csv/)).toBeInTheDocument()
  })

  it('says in words what the platform would refuse in the file, and does not offer the check until it is fixed', async () => {
    await open()
    pick(csvFile('email,name\na@x.test,Ann\nb@x.test\n'))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('newjob.problem.title'))
    expect(alert).toHaveTextContent('the first at line 3')
    expect(checkButton()).toBeDisabled()
    expect(screen.getByText(t('newjob.verify.fixProblems'))).toBeInTheDocument()

    // another file, with nothing wrong, and the same page works
    fireEvent.click(screen.getByRole('button', { name: t('newjob.csv.change') }))
    pick(csvFile('email,name\na@x.test,Ann\n'))
    await screen.findByLabelText(t('newjob.columns.address'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(checkButton()).toBeEnabled()
  })

  it('says why a column named email beside another address column is refused, and lets the user make that column the address', async () => {
    await open()
    pick(csvFile('work mail,Email\na@x.test,b@x.test\n'))
    const select = (await screen.findByLabelText(t('newjob.columns.address'))) as HTMLSelectElement
    expect(select.value).toBe('1') // the column called email is pre-selected, so nothing clashes
    fireEvent.change(select, { target: { value: '0' } })
    expect(await screen.findByRole('alert')).toHaveTextContent('The column "Email" has a name we use ourselves')
    expect(checkButton()).toBeDisabled()
    fireEvent.change(select, { target: { value: '1' } })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})

describe('the verify page: the upload', () => {
  it('sends the File itself as the body, as text/csv, with the chosen column as email_column and nothing else about the list', async () => {
    const fetchFn = await open()
    const file = csvFile(LIST)
    pick(file)
    const select = (await screen.findByLabelText(t('newjob.columns.address'))) as HTMLSelectElement
    fireEvent.change(select, { target: { value: '1' } })
    fireEvent.click(checkButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })

    const [call, ...others] = callsTo(fetchFn, 'POST', '/v1/jobs')
    expect(others).toHaveLength(0)
    expect(call?.init.body).toBe(file) // the File itself: the browser streams it from disk
    expect(call?.headers['Content-Type']).toBe('text/csv')
    expect(call?.headers['Authorization']).toMatch(/^Bearer /)
    expect([...(call?.url.searchParams.keys() ?? [])].sort()).toEqual(['email_column', 'module'])
    expect(call?.url.searchParams.get('module')).toBe('m1')
    expect(call?.url.searchParams.get('email_column')).toBe('work mail')
    // the draft is then previewed, with no body
    const previews = callsTo(fetchFn, 'POST', '/v1/jobs/job-1/preview')
    expect(previews).toHaveLength(1)
    expect(previews[0]?.init.body).toBeUndefined()
  })

  it('control: another choice of column is another email_column', async () => {
    const fetchFn = await open()
    pick(csvFile('Name,Contact,City\nAnn,a@x.test,Paris\n'))
    const select = (await screen.findByLabelText(t('newjob.columns.address'))) as HTMLSelectElement
    fireEvent.change(select, { target: { value: '0' } })
    fireEvent.click(checkButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')[0]?.url.searchParams.get('email_column')).toBe('Name')
  })

  it('says what went wrong in words when the file is refused, and Try again sends it again', async () => {
    let tries = 0
    const fetchFn = await open(
      platform({
        'POST /v1/jobs': () => (++tries === 1 ? new Response(JSON.stringify({ error: { code: 'validation_failed', message: 'SERVER-ROW-3', request_id: 'req_1', details: { reason: 'line 3 has the wrong number of cells' } } }), { status: 400, headers: { 'Content-Type': 'application/json' } }) : [201, { job: job({ status: 'draft', preview: null }) }]),
      }),
    )
    pick(csvFile(LIST))
    await screen.findByLabelText(t('newjob.columns.address'))
    fireEvent.click(checkButton())
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.validation_failed'))
    expect(alert).toHaveTextContent(t('newjob.upload.checkFile'))
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-ROW-3', 'line 3 has'])

    fireEvent.click(within(alert).getByRole('button', { name: t('common.retry') }))
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(2)
  })

  it('does not make a second draft when only the preview failed: Try again previews the draft it has', async () => {
    let previews = 0
    const fetchFn = await open(
      platform({ 'POST /v1/jobs/job-1/preview': () => (++previews === 1 ? [503, { error: { code: 'module_unavailable', message: 'x', request_id: 'req_2' } }] : { job: job() }) }),
    )
    pick(csvFile(LIST))
    await screen.findByLabelText(t('newjob.columns.address'))
    fireEvent.click(checkButton())
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.module_unavailable'))
    fireEvent.click(within(alert).getByRole('button', { name: t('common.retry') }))
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(1)
    expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/preview')).toHaveLength(2)
  })

  it('names the limit in words when the list has too many rows', async () => {
    await open(platform({ 'POST /v1/jobs': [413, { error: { code: 'too_many_rows', message: 'x', request_id: 'req_3', details: { limit: 100000 } } }] }))
    pick(csvFile(LIST))
    await screen.findByLabelText(t('newjob.columns.address'))
    fireEvent.click(checkButton())
    expect(await screen.findByRole('alert')).toHaveTextContent('more than 100,000 addresses')
  })

  it('lets the draft go when the user chooses another file after the preview, and shows the file picker again', async () => {
    const fetchFn = await open()
    await toPreview()
    fireEvent.click(screen.getByRole('button', { name: t('newjob.csv.change') }))
    await waitFor(() => expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/discard')).toHaveLength(1))
    expect(await screen.findByLabelText(t('newjob.csv.file'))).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: t('newjob.preview.title') })).not.toBeInTheDocument()
  })
})

describe('the verify page: the preview', () => {
  const full = preview({
    rows: 1234,
    malformed: 7,
    malformed_reasons: { no_at: 4, empty_domain: 2, a_brand_new_reason: 1 },
    duplicates: 11,
    suppressed: 13,
    units: 1199,
    selected: 0,
    unit_cost: 2,
    credits: 2398,
    available: 5000,
    shortfall: 0,
  })

  it('shows every count, and each reason a row was found malformed as a sentence of the catalogue (an unknown reason as a generic one)', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/preview': { job: job({ preview: full }) } }))
    await toPreview()
    expect(figure(t('newjob.preview.rows'))).toBe('1,234')
    expect(figure(t('newjob.preview.malformed'))).toContain('7')
    expect(figure(t('newjob.preview.duplicates'))).toBe('11')
    expect(figure(t('newjob.preview.suppressedVerify'))).toContain('13')
    expect(figure(t('newjob.preview.unitsVerify'))).toBe('1,199')
    expect(figure(t('newjob.preview.credits'))).toContain('2,398') // the exact credits
    expect(figure(t('newjob.preview.available'))).toBe('5,000')
    expect(screen.queryByText(t('newjob.preview.shortfall'), { selector: 'dt' })).not.toBeInTheDocument() // control: no shortfall, none shown

    const reasons = within(screen.getByText(t('newjob.preview.malformed'), { selector: 'dt' }).parentElement as HTMLElement).getAllByRole('listitem')
    expect(reasons.map((li) => li.textContent)).toEqual([
      `4 ${t('common.malformed.no_at')}`,
      `2 ${t('common.malformed.empty_domain')}`,
      `1 ${t('common.malformed.other')}`,
    ])
    expect(document.body.textContent).not.toContain('a_brand_new_reason')
    expect(document.body.textContent).not.toContain('no_at')
  })

  it('says every reason the platform gives for a malformed address in its own sentence, one line each, and none of the words of the API', async () => {
    const closed = ['empty', 'no_at', 'multiple_at', 'empty_local', 'empty_domain', 'bad_idna', 'part_empty', 'local_too_long', 'domain_too_long', 'domain_no_dot', 'cannot_store', 'bad_line', 'invalid_address']
    const reasons = Object.fromEntries(closed.map((r, i) => [r, closed.length - i])) // 13, 12, ... 1: so the order is known
    await open(platform({ 'POST /v1/jobs/job-1/preview': { job: job({ preview: preview({ malformed: 91, malformed_reasons: reasons }) }) } }))
    await toPreview()
    const lines = within(screen.getByText(t('newjob.preview.malformed'), { selector: 'dt' }).parentElement as HTMLElement)
      .getAllByRole('listitem')
      .map((li) => li.textContent)
    expect(lines).toEqual(closed.map((r, i) => `${closed.length - i} ${t(`common.malformed.${r}` as 'common.malformed.no_at')}`))
    for (const line of lines) expect(line).not.toMatch(/[a-z]+_[a-z]+/) // no snake_case word of the API
    // control: a preview with no malformed rows lists no reason
    expect(figure(t('newjob.preview.malformed'))).toBe('91')
  })

  it('shows no reason at all when no row was malformed', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/preview': { job: job({ preview: preview({ malformed: 0, malformed_reasons: {} }) }) } }))
    await toPreview()
    expect(figure(t('newjob.preview.malformed'))).toBe('0')
    expect(within(screen.getByText(t('newjob.preview.malformed'), { selector: 'dt' }).parentElement as HTMLElement).queryAllByRole('listitem')).toEqual([])
  })

  it('says that suppressed addresses are still checked, and that nobody is emailed in a verification', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/preview': { job: job({ preview: full }) } }))
    await toPreview()
    expect(screen.getByText(t('newjob.preview.suppressedVerifyNote'))).toBeInTheDocument()
  })

  it('says the shortfall and the next step when the credits do not cover the list, and will not confirm it', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/preview': { job: job({ preview: preview({ credits: 3000, available: 1000, shortfall: 2000 }) }) } }))
    await toPreview()
    expect(figure(t('newjob.preview.shortfall'))).toContain('2,000')
    expect(screen.getAllByText(t('newjob.preview.shortfallNext')).length).toBeGreaterThan(0)
    expect(screen.getByRole('button', { name: t('newjob.confirm.submitNow') })).toBeDisabled()
    expect(screen.getByText(t('newjob.confirm.blockedCredits'))).toBeInTheDocument()
  })

  it('says when nothing in the list can be processed, and will not confirm it', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/preview': { job: job({ preview: preview({ units: 0, credits: 0 }) }) } }))
    await toPreview()
    expect(screen.getByText(t('newjob.preview.nothing'))).toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('newjob.confirm.submitNow') })).toBeDisabled()
  })

  it('has no figure of a send job (missing fields, messages) on a verification', async () => {
    await open()
    await toPreview()
    expect(screen.queryByText(t('newjob.preview.missing'), { selector: 'dt' })).not.toBeInTheDocument()
    expect(screen.queryByText(t('newjob.preview.unrenderable'), { selector: 'dt' })).not.toBeInTheDocument()
    expect(screen.queryByText(t('newjob.samples.title'))).not.toBeInTheDocument()
  })
})

describe('the verify page: the line about the outside service', () => {
  it('says some addresses may be checked with a trusted outside service, with a link to Settings, when the user has not opted out', async () => {
    await open()
    await toPreview()
    expect(screen.getByText(t('newjob.disclosure.vendor'), { exact: false })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.disclosure.settingsLink') })).toHaveAttribute('href', '/settings')
    expect(screen.queryByText(t('newjob.disclosure.vendorOptedOut'), { exact: false })).not.toBeInTheDocument()
  })

  it('says they will not when the user has opted out (the job was made under the opt-out)', async () => {
    await open(
      platform({
        'GET /v1/settings': { vendor_fallback_opt_out: true },
        'POST /v1/jobs/job-1/preview': { job: job({ preview: preview({ vendor_opt_out: true }), settings: { unknown_choice: null, vendor_opt_out: true } }) },
      }),
    )
    await toPreview()
    expect(screen.getByText(t('newjob.disclosure.vendorOptedOut'), { exact: false })).toBeInTheDocument()
    expect(screen.queryByText(t('newjob.disclosure.vendor'), { exact: false })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.disclosure.settingsLink') })).toBeInTheDocument()
    expect(screen.queryByText(t('newjob.disclosure.changedSince'))).not.toBeInTheDocument() // the setting and the job agree
  })

  it('says the new setting applies to the next job when the user changed it after this draft was made', async () => {
    await open(platform({ 'GET /v1/settings': { vendor_fallback_opt_out: true } })) // the job below was made opted in
    await toPreview()
    expect(screen.getByText(t('newjob.disclosure.vendor'), { exact: false })).toBeInTheDocument() // this job: still opted in
    expect(await screen.findByText(t('newjob.disclosure.changedSince'))).toBeInTheDocument()
  })

  it('is one sentence kept under newjob.disclosure.vendor (the key P8 replaces)', () => {
    expect(t('newjob.disclosure.vendor')).not.toBe('')
    expect(t('newjob.disclosure.vendorOptedOut')).not.toBe(t('newjob.disclosure.vendor'))
  })
})

describe('the verify page: confirming', () => {
  async function confirmNow() {
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))
  }

  it('sends the fingerprint of the preview the user saw and a key for this attempt, and then says the job is confirmed, with a link to it', async () => {
    const fetchFn = await open()
    await toPreview()
    await confirmNow()
    expect(await screen.findByRole('heading', { name: t('newjob.done.title') })).toBeInTheDocument()
    const [call, ...others] = callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm')
    expect(others).toHaveLength(0)
    expect(bodyOf(call as { init: RequestInit })).toEqual({ fingerprint: 'fp-1' })
    expect(call?.headers['Idempotency-Key']).toMatch(IDEMPOTENCY_KEY_PATTERN)
    expect(screen.getByRole('link', { name: t('newjob.done.open') })).toHaveAttribute('href', '/jobs/job-1')
    expect(screen.getByText(t('newjob.done.pending', { count: 3 }))).toBeInTheDocument()
    expect(screen.getByText(t('newjob.done.reserved', { credits: 3 }))).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: t('newjob.done.title') })).toHaveFocus() // the keyboard user lands on the news
  })

  it('repeats the SAME key when the same attempt is repeated after an answer was lost, and the same fingerprint', async () => {
    let n = 0
    const fetchFn = await open(
      platform({
        'POST /v1/jobs/job-1/confirm': () => {
          if (++n === 1) throw new TypeError('Failed to fetch')
          return confirmResult(job())
        },
      }),
    )
    await toPreview()
    await confirmNow()
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.network_error'))
    await confirmNow()
    await screen.findByRole('heading', { name: t('newjob.done.title') })
    const [first, second] = callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm')
    expect(first?.headers['Idempotency-Key']).toMatch(IDEMPOTENCY_KEY_PATTERN)
    expect(second?.headers['Idempotency-Key']).toBe(first?.headers['Idempotency-Key'])
    expect(bodyOf(second as { init: RequestInit })).toEqual(bodyOf(first as { init: RequestInit }))
  })

  it('makes a FRESH key for a new attempt: after the platform refused one, and when the time it starts changes', async () => {
    let n = 0
    const fetchFn = await open(
      platform({
        'POST /v1/jobs/job-1/confirm': () => {
          n++
          if (n === 1) return [409, { error: { code: 'job_slot_taken', message: 'x', request_id: 'req_4' } }]
          if (n === 2) throw new TypeError('Failed to fetch')
          return confirmResult(job())
        },
      }),
    )
    await toPreview()
    await confirmNow() // 1: refused
    await screen.findByRole('alert')
    await confirmNow() // 2: a new attempt (the refusal ended the first), answer lost
    await waitFor(() => expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm')).toHaveLength(2))
    await screen.findByText(t('error.network_error'))
    // 3: the same intent as 2 would reuse its key; a change of the start time is a new intent
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.confirm.later') }))
    fireEvent.change(screen.getByLabelText(new RegExp(t('newjob.schedule.label'))), { target: { value: localInputValue(new Date(Date.now() + 2 * 86_400_000)) } })
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitLater') }))
    await screen.findByRole('heading', { name: t('newjob.done.title') })

    const keys = callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm').map((c) => c.headers['Idempotency-Key'])
    expect(keys).toHaveLength(3)
    expect(new Set(keys).size).toBe(3)
    for (const k of keys) expect(k).toMatch(IDEMPOTENCY_KEY_PATTERN)
  })

  it('sends a later start as an instant in UTC, shows the zone it is read in, and refuses a time that is too soon or missing without asking the platform', async () => {
    const fetchFn = await open()
    await toPreview()
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.confirm.later') }))
    const input = screen.getByLabelText(new RegExp(t('newjob.schedule.label')))
    expect(input).toHaveAttribute('type', 'datetime-local')
    expect(screen.getByText(/^Times are in your/)).toHaveTextContent(/UTC[+-]\d\d:\d\d/)
    expect(screen.getByRole('button', { name: t('newjob.confirm.submitLater') })).toBeInTheDocument()

    // nothing chosen
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitLater') }))
    expect(await screen.findByText(t('newjob.schedule.errorEmpty'))).toBeInTheDocument()
    // a minute from now is too soon
    fireEvent.change(input, { target: { value: localInputValue(new Date(Date.now() + 60_000)) } })
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitLater') }))
    expect(await screen.findByText(t('newjob.schedule.errorTooSoon'))).toBeInTheDocument()
    // two years from now is too far
    fireEvent.change(input, { target: { value: localInputValue(new Date(Date.now() + 2 * 366 * 86_400_000)) } })
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitLater') }))
    expect(await screen.findByText(t('newjob.schedule.errorTooFar'))).toBeInTheDocument()
    expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm')).toHaveLength(0)

    // control: a good time is sent, as the instant it is on the user's clock
    const value = localInputValue(new Date(Date.now() + 3 * 86_400_000))
    fireEvent.change(input, { target: { value } })
    expect(screen.getByText(/^It will start on/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitLater') }))
    await screen.findByRole('heading', { name: t('newjob.done.title') })
    const sent = bodyOf(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm')[0] as { init: RequestInit })
    expect(sent['scheduled_start']).toBe(new Date(value).toISOString().replace(/\.\d{3}Z$/, 'Z'))
    expect(sent['scheduled_start']).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/)
    expect(sent['fingerprint']).toBe('fp-1')
  })

  it('says the job is scheduled, and when, once a scheduled confirm is accepted', async () => {
    await open(
      platform({
        'POST /v1/jobs/job-1/confirm': () => {
          const j = job({ status: 'scheduled', scheduled_start: '2030-05-06T10:30:00Z' })
          return { ...confirmResult(j), job: j }
        },
      }),
    )
    await toPreview()
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.confirm.later') }))
    fireEvent.change(screen.getByLabelText(new RegExp(t('newjob.schedule.label'))), { target: { value: localInputValue(new Date(Date.now() + 86_400_000)) } })
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitLater') }))
    expect(await screen.findByRole('heading', { name: t('newjob.done.titleScheduled') })).toBeInTheDocument()
    expect(screen.getByText(/^It will start on .*2030/)).toBeInTheDocument()
  })

  it('says a repeated confirm that the platform recognised was not done twice', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/confirm': () => confirmResult(job(), { replayed: true }) }))
    await toPreview()
    await confirmNow()
    expect(await screen.findByText(t('newjob.done.replayed'))).toBeInTheDocument()
  })
})

describe('the verify page: when the platform says no to a confirm', () => {
  const refuse = (status: number, code: string, details?: Record<string, unknown>) => () => [status, { error: { code, message: 'SERVER-SENTENCE', request_id: 'req_9', details } }]
  const confirmNow = () => fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))

  it('preview_stale: looks again, says what changed in words, and confirms the new figures with a new key when the user says so', async () => {
    let previews = 0
    let confirms = 0
    const fetchFn = await open(
      platform({
        'POST /v1/jobs/job-1/preview': () => (++previews === 1 ? { job: job() } : { job: job({ preview: preview({ units: 2, credits: 2, rows: 4, fingerprint: 'fp-2' }) }) }),
        'POST /v1/jobs/job-1/confirm': () => (++confirms === 1 ? refuse(409, 'preview_stale')() : confirmResult(job())),
      }),
    )
    await toPreview()
    confirmNow()
    const notice = await screen.findByText(t('newjob.stale.title'))
    const box = notice.parentElement as HTMLElement
    expect(box).toHaveTextContent('The rows in your list changed from 5 to 4.')
    expect(box).toHaveTextContent('The addresses that will be processed changed from 3 to 2.')
    expect(box).toHaveTextContent('The credits held for this job changed from 3 to 2.')
    expect(box).not.toHaveTextContent('The credits you have changed') // control: what did not change is not said to have
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
    expect(figure(t('newjob.preview.unitsVerify'))).toBe('2') // the new figures are on the screen

    confirmNow()
    await screen.findByRole('heading', { name: t('newjob.done.title') })
    const [first, second] = callsTo(fetchFn, 'POST', '/v1/jobs/job-1/confirm')
    expect(bodyOf(first as { init: RequestInit })['fingerprint']).toBe('fp-1')
    expect(bodyOf(second as { init: RequestInit })['fingerprint']).toBe('fp-2')
    expect(second?.headers['Idempotency-Key']).not.toBe(first?.headers['Idempotency-Key'])
  })

  it('preview_stale with nothing visible to say: says so instead of inventing a change', async () => {
    let previews = 0
    await open(
      platform({
        'POST /v1/jobs/job-1/preview': () => (++previews === 1 ? { job: job() } : { job: job({ preview: preview({ fingerprint: 'fp-2' }) }) }),
        'POST /v1/jobs/job-1/confirm': refuse(409, 'preview_stale'),
      }),
    )
    await toPreview()
    confirmNow()
    expect(await screen.findByText(t('newjob.stale.nothingVisible'))).toBeInTheDocument()
  })

  it('insufficient_credits: how many are needed, how many there are, how many short, and the next step; Check again looks at the balance again', async () => {
    let previews = 0
    const fetchFn = await open(
      platform({
        'POST /v1/jobs/job-1/preview': () => (++previews === 1 ? { job: job() } : { job: job({ preview: preview({ available: 20000, fingerprint: 'fp-3' }) }) }),
        'POST /v1/jobs/job-1/confirm': refuse(402, 'insufficient_credits', { available: 1200, needed: 15000, shortfall: 13800 }),
      }),
    )
    await toPreview()
    confirmNow()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('This list needs 15,000 credits. You have 1,200, so you are 13,800 short.')
    expect(alert).toHaveTextContent(t('newjob.confirm.creditsNext'))
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.checkAgain') }))
    await waitFor(() => expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/preview')).toHaveLength(2))
    await waitFor(() => expect(figure(t('newjob.preview.available'))).toBe('20,000'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('job_slot_taken: says the user has a job of this kind running and to wait or cancel it, and links to the jobs', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/confirm': refuse(409, 'job_slot_taken') }))
    await toPreview()
    confirmNow()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('You already have a job of this kind in progress. Wait for it to finish, or cancel it, before you start another.')
    expect(within(alert).getByRole('link', { name: t('newjob.confirm.seeJobs') })).toHaveAttribute('href', '/jobs')
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE', 'job_slot'])
  })

  it('nothing_to_do: says no address in the list can be processed, and offers a different list', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/confirm': refuse(422, 'nothing_to_do') }))
    await toPreview()
    confirmNow()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.nothing_to_do'))
    expect(screen.getByRole('button', { name: t('newjob.csv.change') })).toBeInTheDocument()
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })

  it('a failure on the platform\'s side says so, gives the reference for support, and never the platform\'s own sentence', async () => {
    await open(platform({ 'POST /v1/jobs/job-1/confirm': refuse(500, 'internal_error') }))
    await toPreview()
    confirmNow()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    expect(alert).toHaveTextContent('Reference req_9')
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })
})

describe('the verify page: a draft continued with ?job=', () => {
  it('loads the draft, previews it and offers the confirm, with no file to choose', async () => {
    const draft = job({ status: 'draft', preview: null })
    const fetchFn = await open(platform({ 'GET /v1/jobs/job-1': { job: draft } }), '/new/verify?job=job-1')
    expect(await screen.findByRole('heading', { name: t('newjob.preview.title') })).toBeInTheDocument()
    expect(screen.queryByLabelText(t('newjob.csv.file'))).not.toBeInTheDocument()
    expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/preview')).toHaveLength(1)
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(0)
    expect(screen.getByRole('button', { name: t('newjob.confirm.submitNow') })).toBeEnabled()
  })

  it('says a job that was already confirmed is under way, with a link to it, and previews nothing', async () => {
    const fetchFn = await open(platform({ 'GET /v1/jobs/job-1': { job: job({ status: 'running' }) } }), '/new/verify?job=job-1')
    expect(await screen.findByText(t('newjob.review.started'))).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.done.open') })).toHaveAttribute('href', '/jobs/job-1')
    expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-1/preview')).toHaveLength(0)
  })

  it('points a send draft to the send page', async () => {
    await open(platform({ 'GET /v1/jobs/job-1': { job: sendJob({ id: 'job-send', status: 'draft', preview: null }) } }), '/new/verify?job=job-1')
    expect(await screen.findByText(t('newjob.review.isSend'))).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.review.goSend') })).toHaveAttribute('href', '/new/send?job=job-send')
  })

  it('says in words that it cannot find a draft that is not there (nor another user\'s), and never why', async () => {
    await open(platform({ 'GET /v1/jobs/job-1': [404, { error: { code: 'not_found', message: 'SERVER-SENTENCE', request_id: 'req_5' } }] }), '/new/verify?job=job-1')
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.not_found'))
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })
})

describe('the verify page: the keyboard', () => {
  it('can be operated by keyboard alone at every stage: only native controls, in the tab order, each with a name', async () => {
    await open()
    expect(keyboardProblems(document.body)).toEqual([]) // nothing chosen yet
    pick(csvFile(LIST))
    await screen.findByLabelText(t('newjob.columns.address'))
    expect(keyboardProblems(document.body)).toEqual([]) // the columns
    fireEvent.click(checkButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(keyboardProblems(document.body)).toEqual([]) // the preview and the confirm
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.confirm.later') }))
    expect(keyboardProblems(document.body)).toEqual([]) // the date and time
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.confirm.now') }))
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))
    await screen.findByRole('heading', { name: t('newjob.done.title') })
    expect(keyboardProblems(document.body)).toEqual([]) // the confirmation
  })

  it('control: the check does find a control that a keyboard cannot use', () => {
    const page = document.createElement('div')
    page.innerHTML = '<div role="button" tabindex="0">Go</div><button tabindex="3">Ahead</button><button tabindex="-1">Out</button><input><div tabindex="0">box</div><button aria-hidden="true">Hidden</button>'
    const found = keyboardProblems(page).join(' | ')
    expect(found).toContain('a button that is not a native control')
    expect(found).toContain('a positive tabindex')
    expect(found).toContain('tabindex -1 takes it out of order')
    expect(found).toContain('no accessible name')
    expect(found).toContain('not a control or a named scrolling region')
    expect(found).toContain('hidden from assistive technology')
    const clean = document.createElement('div')
    clean.innerHTML = '<button>Go</button><label>Name <input></label><a href="/x">Link</a><div role="region" aria-label="Rows" tabindex="0">r</div>'
    expect(keyboardProblems(clean)).toEqual([])
  })

  it('offers the choices as native radios of one group (the arrow keys move within it) and the confirm before the way back', async () => {
    await open()
    await toPreview()
    const radios = screen.getAllByRole('radio')
    expect(radios.map((r) => (r as HTMLInputElement).name).filter((n, i, all) => all.indexOf(n) === i)).toHaveLength(1)
    expect(radios.every((r) => r.tagName === 'INPUT')).toBe(true)
    const buttons = screen.getAllByRole('button')
    expect(buttons[buttons.length - 2]?.textContent).toBe(t('newjob.confirm.submitNow')) // then "Choose a different file" and nothing after
  })
})
