import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Job } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { account, csvFile, job, preview, sendJob } from './testing/fixtures.ts'

beforeEach(async () => {
  resetForTest()
  await signInForTest()
})
afterEach(() => {
  vi.unstubAllGlobals()
  resetForTest()
})

type Routes = Record<string, unknown>

const LIST = 'first_name,work mail,plan\nAnn,a@x.test,pro\nBo,b@x.test,free\n'
const BALANCES = { modules: { m1: { available: 10, reserved: 0 }, m2: { available: 500, reserved: 0 } }, m3: { enabled: true, until: null, state: 'active' } }

function platform(over: Routes = {}): Routes {
  return {
    'GET /v1/me': { email: 'person@example.test' },
    'GET /v1/balances': BALANCES,
    'GET /v1/m3/accounts': { accounts: [account()], next_cursor: null },
    'GET /v1/jobs': { jobs: [], next_cursor: null },
    'POST /v1/templates/check': { ok: true, fields: [] },
    'POST /v1/jobs': [201, { job: sendJob({ status: 'draft', preview: null }) }],
    'POST /v1/jobs/job-send/preview': { job: sendJob() },
    ...over,
  }
}

const heading = (n: number, title: Parameters<typeof t>[0]) => t('newjob.step.heading', { n, total: 4, title: t(title) })
const next = () => fireEvent.click(screen.getByRole('button', { name: t('newjob.step.next') }))
const htmlBox = () => screen.getByLabelText(t('newjob.authoring.html')) as HTMLTextAreaElement
const previewButton = () => screen.getByRole('button', { name: t('newjob.message.next') })

function write(subject: string, html: string) {
  fireEvent.change(screen.getByLabelText(t('newjob.authoring.subject')), { target: { value: subject } })
  fireEvent.change(htmlBox(), { target: { value: html } })
}

async function open(routes: Routes) {
  mockApi(routes as Parameters<typeof mockApi>[0])
  renderApp('/new/send')
  await screen.findByRole('heading', { level: 1, name: t('newjob.send.title') })
  fireEvent.click(screen.getByRole('radio', { name: t('newjob.sender.managed') }))
  next()
  await screen.findByRole('heading', { level: 2, name: heading(2, 'newjob.step.list') })
}

/** From a file: choose it and go to the message. */
async function toMessageFromFile(routes: Routes) {
  await open(routes)
  fireEvent.change(screen.getByLabelText(t('newjob.csv.file')), { target: { files: [csvFile(LIST)] } })
  await screen.findByLabelText(t('newjob.columns.address'))
  next()
  await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
}

const finished = (id: string, fields?: string[]): Job =>
  job({ id, status: 'succeeded', finished_at: '2026-12-01T10:00:00Z', preview: preview({ rows: 50, units: 50 }), ...(fields === undefined ? {} : { merge_fields: fields }) })

const lists = (...jobs: Job[]): Routes => platform({ 'GET /v1/jobs': { jobs, next_cursor: null } })

async function atVerificationList(routes: Routes) {
  await open(routes)
  fireEvent.click(screen.getByRole('radio', { name: t('newjob.list.verification') }))
  await screen.findAllByRole('radio', { name: /^Finished / })
}

function choose(id: string) {
  const radio = screen.getAllByRole('radio', { name: /^Finished / }).find((r) => r.getAttribute('value') === id) as HTMLElement
  fireEvent.click(radio)
}

async function toMessageFrom(id: string) {
  choose(id)
  fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.include') }))
  next()
  await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
}

const insertButtons = () => within(screen.getByRole('list', { name: t('newjob.authoring.columnsFields') })).getAllByRole('button').map((b) => b.getAttribute('aria-label'))

describe('the send page: the merge fields of a finished verification', () => {
  it('lists the fields of the chosen verification, as chips like the columns of a file, with the note that the values go after 7 days', async () => {
    await atVerificationList(lists(finished('v-1', ['first_name', 'plan']), finished('v-2', ['city'])))
    expect(screen.queryByText(t('newjob.verification.fieldsTitle'))).not.toBeInTheDocument() // nothing chosen yet
    choose('v-1')
    const chips = screen.getByRole('list', { name: t('newjob.verification.fieldsTitle') })
    expect(within(chips).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['first_name', 'plan'])
    expect(screen.getByText(t('newjob.palette.verifiedNote'))).toBeInTheDocument()
    expect(t('newjob.palette.verifiedNote')).toBe('Fields come from the list you verified; after 7 days the values of a finished job are removed, so use a fallback.')
    // choosing the other verification is choosing its fields
    choose('v-2')
    expect(within(screen.getByRole('list', { name: t('newjob.verification.fieldsTitle') })).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['city'])
  })

  it('makes the palette of the authoring screen from the merge_fields of the verification the user chose, and inserts them', async () => {
    await atVerificationList(lists(finished('v-1', ['first_name', 'plan']), finished('v-2', ['city'])))
    await toMessageFrom('v-1')
    expect(insertButtons()).toEqual(['Insert first_name', 'Insert plan'])
    const reserved = within(screen.getByRole('list', { name: t('newjob.authoring.reservedFields') })).getAllByRole('button')
    expect(reserved.map((b) => b.textContent)).toEqual(['email', 'unsubscribe_url']) // always there, as on the other path
    expect(screen.getByText(t('newjob.palette.verifiedNote'))).toBeInTheDocument()

    const box = htmlBox()
    box.focus()
    fireEvent.click(screen.getByRole('button', { name: 'Insert plan' }))
    expect(box.value).toBe('{{plan}}')

    // back to the list and another verification: the palette follows it
    fireEvent.click(screen.getByRole('button', { name: t('common.back') }))
    await screen.findByRole('heading', { level: 2, name: heading(2, 'newjob.step.list') })
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.list.verification') }))
    await screen.findAllByRole('radio', { name: /^Finished / })
    choose('v-2')
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.include') }))
    next()
    await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
    expect(insertButtons()).toEqual(['Insert city'])
  })

  it('control: the palette of a file is its own columns and does not carry the note about a verified list', async () => {
    await toMessageFromFile(platform())
    expect(insertButtons()).toEqual(['Insert first_name', 'Insert plan'])
    expect(screen.queryByText(t('newjob.palette.verifiedNote'))).not.toBeInTheDocument()
  })

  it('reads an answer without merge_fields (an older service) as no fields: only the two fields the platform fills in, and no note', async () => {
    await atVerificationList(lists(finished('v-1')))
    choose('v-1')
    expect(screen.getByText(t('newjob.verification.fieldsNone'))).toBeInTheDocument()
    expect(screen.queryByRole('list', { name: t('newjob.verification.fieldsTitle') })).not.toBeInTheDocument()
    expect(screen.queryByText(t('newjob.palette.verifiedNote'))).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.include') }))
    next()
    await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
    expect(screen.queryByRole('list', { name: t('newjob.authoring.columnsFields') })).not.toBeInTheDocument()
    expect(within(screen.getByRole('list', { name: t('newjob.authoring.reservedFields') })).getAllByRole('button')).toHaveLength(2)
    expect(screen.queryByText(t('newjob.palette.verifiedNote'))).not.toBeInTheDocument()
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1) // and the page is whole
  })

  it('control: a verification that says it has no fields is the same as one that says nothing', async () => {
    await atVerificationList(lists(finished('v-1', [])))
    choose('v-1')
    expect(screen.getByText(t('newjob.verification.fieldsNone'))).toBeInTheDocument()
  })

  it('never offers email or unsubscribe_url twice, even if the answer lists them', async () => {
    await atVerificationList(lists(finished('v-1', ['email', 'plan', 'unsubscribe_url'])))
    await toMessageFrom('v-1')
    expect(insertButtons()).toEqual(['Insert plan'])
  })
})

describe('the send page: the note about a field with no fallback', () => {
  const answer = (fields: Array<{ name: string; has_fallback: boolean }>) => ({ 'POST /v1/templates/check': { ok: true, fields } })

  it('says, politely and without stopping the user, that rows with no value for a field of the list will be skipped when the message gives it no fallback (a file)', async () => {
    await toMessageFromFile(platform(answer([{ name: 'first_name', has_fallback: false }, { name: 'plan', has_fallback: true }])))
    write('Hello', '<p>{{first_name}} {{plan|free}}</p>')
    const note = await screen.findByText(t('newjob.authoring.skipNote'), { exact: false }, { timeout: 2000 })
    expect(note).toHaveTextContent('first_name')
    expect(note).not.toHaveTextContent('plan') // it has a fallback
    expect(note.closest('[role="status"]')).not.toBeNull() // polite, not an alert
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await waitFor(() => expect(previewButton()).toBeEnabled()) // it does not stop the user
  })

  it('control: with a fallback on every field of the list there is no such note', async () => {
    await toMessageFromFile(platform(answer([{ name: 'first_name', has_fallback: true }])))
    write('Hello', '<p>{{first_name|there}}</p>')
    await screen.findByText(t('newjob.authoring.ok'), undefined, { timeout: 2000 })
    expect(screen.queryByText(t('newjob.authoring.skipNote'), { exact: false })).not.toBeInTheDocument()
  })

  it('does not call a field that is not a column of the list a skip: that is the other, stronger warning', async () => {
    await toMessageFromFile(platform(answer([{ name: 'company', has_fallback: false }])))
    write('Hello', '<p>{{company}}</p>')
    expect(await screen.findByText(t('newjob.authoring.unmetColumn'), { exact: false }, { timeout: 2000 })).toBeInTheDocument()
    expect(screen.queryByText(t('newjob.authoring.skipNote'), { exact: false })).not.toBeInTheDocument()
  })

  it('says it too for a field of a finished verification, and calls a field it does not carry the stronger warning', async () => {
    await atVerificationList(
      platform({ 'GET /v1/jobs': { jobs: [finished('v-1', ['plan'])], next_cursor: null }, ...answer([{ name: 'plan', has_fallback: false }, { name: 'company', has_fallback: false }]) }),
    )
    await toMessageFrom('v-1')
    write('Hello', '<p>{{plan}} {{company}}</p>')
    const skip = await screen.findByText(t('newjob.authoring.skipNote'), { exact: false }, { timeout: 2000 })
    expect(skip).toHaveTextContent('plan')
    expect(skip).not.toHaveTextContent('company')
    const unmet = screen.getByText(t('newjob.authoring.unmetColumn'), { exact: false })
    expect(unmet).toHaveTextContent('company')
    await waitFor(() => expect(previewButton()).toBeEnabled())
  })
})
