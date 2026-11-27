import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { IDEMPOTENCY_KEY_PATTERN } from '../../api/idempotency.ts'
import type { Job, Preview } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { account, bodyOf, callsTo, confirmResult, csvFile, expectNoCodeWords, FAR_FUTURE_ENTITLEMENT, job, keyboardProblems, preview, samples, sendJob } from './testing/fixtures.ts'

beforeEach(async () => {
  resetForTest()
  await signInForTest()
})
afterEach(() => {
  vi.unstubAllGlobals()
  resetForTest()
})

const LIST = 'first_name,work mail,plan\nAnn,a@x.test,pro\nBo,b@x.test,free\n'

type Routes = Record<string, unknown>

const BALANCES = { modules: { m1: { available: 10, reserved: 0 }, m2: { available: 500, reserved: 0 } }, m3: { enabled: true, until: null, state: 'active' } }

/** The platform's side of a send: the balances, the accounts, the message check, and a draft that is made, previewed (with samples) and confirmed. */
function platform(over: Routes = {}): Routes {
  return {
    'GET /v1/me': { email: 'person@example.test' },
    'GET /v1/balances': BALANCES,
    'GET /v1/m3/accounts': { accounts: [account()], next_cursor: null },
    'GET /v1/jobs': { jobs: [], next_cursor: null },
    'POST /v1/templates/check': { ok: true, fields: [] },
    'POST /v1/jobs': [201, { job: sendJob({ status: 'draft', preview: null }) }],
    'POST /v1/jobs/job-send/preview': { job: sendJob({}, { samples: samples() }) },
    'POST /v1/jobs/job-send/confirm': () => confirmResult(sendJob(), { pending: 3 }),
    'POST /v1/jobs/job-send/discard': { job: sendJob({ status: 'cancelled' }) },
    ...over,
  }
}

async function open(routes: Routes = platform(), path = '/new/send') {
  const fetchFn = mockApi(routes as Parameters<typeof mockApi>[0])
  renderApp(path)
  await screen.findByRole('heading', { level: 1, name: t('newjob.send.title') })
  return fetchFn
}

const heading = (n: number, title: Parameters<typeof t>[0]) => t('newjob.step.heading', { n, total: 4, title: t(title) })
const next = () => fireEvent.click(screen.getByRole('button', { name: t('newjob.step.next') }))
const chooseSender = (which: 'newjob.sender.managed' | 'newjob.sender.own') => fireEvent.click(screen.getByRole('radio', { name: t(which) }))
const pick = (file: File) => fireEvent.change(screen.getByLabelText(t('newjob.csv.file')), { target: { files: [file] } })
const subjectBox = () => screen.getByLabelText(t('newjob.authoring.subject')) as HTMLInputElement
const htmlBox = () => screen.getByLabelText(t('newjob.authoring.html')) as HTMLTextAreaElement
const textBox = () => screen.getByLabelText(new RegExp(t('newjob.authoring.text'))) as HTMLTextAreaElement
const previewButton = () => screen.getByRole('button', { name: t('newjob.message.next') })

async function atList(sender: 'newjob.sender.managed' | 'newjob.sender.own' = 'newjob.sender.managed') {
  chooseSender(sender)
  next()
  await screen.findByRole('heading', { level: 2, name: heading(2, 'newjob.step.list') })
}

async function atMessage(file: File = csvFile(LIST)) {
  await atList()
  pick(file)
  await screen.findByLabelText(t('newjob.columns.address'))
  next()
  await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
}

function write(subject: string, html: string, text = '') {
  fireEvent.change(subjectBox(), { target: { value: subject } })
  fireEvent.change(htmlBox(), { target: { value: html } })
  if (text !== '') fireEvent.change(textBox(), { target: { value: text } })
}

/** Let the live check answer: the button that goes on is enabled only once it has. */
const checked = () => waitFor(() => expect(previewButton()).toBeEnabled(), { timeout: 3000 })

async function atPreview(routes: Routes = platform()) {
  const fetchFn = await open(routes)
  await atMessage()
  write('Hello {{first_name|there}}', '<p>Hi {{first_name|there}}</p>')
  await checked()
  fireEvent.click(previewButton())
  await screen.findByRole('heading', { name: t('newjob.preview.title') })
  return fetchFn
}

const figure = (label: string): string => screen.getByText(label, { selector: 'dt' }).nextElementSibling?.textContent ?? ''

describe('the send page: the steps', () => {
  it('is a protected page with one h1 and a list of four steps that says which one the user is on', async () => {
    await open()
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
    const steps = within(screen.getByRole('navigation', { name: t('newjob.step.label') })).getAllByRole('listitem')
    expect(steps.map((s) => s.textContent?.replace(/\d/, '').trim())).toEqual(['Sender', 'List', 'Message', 'Preview and confirm'])
    expect(steps.filter((s) => s.getAttribute('aria-current') === 'step')).toHaveLength(1)
    expect(steps[0]).toHaveAttribute('aria-current', 'step')
    expect(screen.getByRole('heading', { level: 2, name: heading(1, 'newjob.step.sender') })).toBeInTheDocument()
  })

  it('does not go on from the sender step until a way to send is chosen, and says so', async () => {
    await open()
    expect(screen.getByRole('button', { name: t('newjob.step.next') })).toBeDisabled()
    expect(screen.getByText(t('newjob.sender.needChoice'))).toBeInTheDocument()
    chooseSender('newjob.sender.managed') // control: chosen, it goes on
    expect(screen.getByRole('button', { name: t('newjob.step.next') })).toBeEnabled()
  })

  it('moves the focus to the heading of each step it opens, so a keyboard or screen reader user lands at the top of it, and Back keeps what was chosen', async () => {
    await open()
    chooseSender('newjob.sender.own')
    next()
    const list = await screen.findByRole('heading', { level: 2, name: heading(2, 'newjob.step.list') })
    expect(list).toHaveFocus()
    fireEvent.click(screen.getByRole('button', { name: t('common.back') }))
    const sender = await screen.findByRole('heading', { level: 2, name: heading(1, 'newjob.step.sender') })
    expect(sender).toHaveFocus()
    expect(screen.getByRole('radio', { name: t('newjob.sender.own') })).toBeChecked()
  })
})

describe('the send page: the keyboard', () => {
  it('can be operated by keyboard alone in every step: only native controls, in the tab order, each with a name', async () => {
    const problems = () => keyboardProblems(document.body)
    await open()
    expect(problems()).toEqual([]) // the sender
    chooseSender('newjob.sender.own')
    await screen.findByText(t('newjob.sender.accountsReady', { count: 1 }))
    expect(problems()).toEqual([])
    next()
    await screen.findByRole('heading', { level: 2, name: heading(2, 'newjob.step.list') })
    expect(problems()).toEqual([]) // the list
    pick(csvFile(LIST))
    await screen.findByLabelText(t('newjob.columns.address'))
    expect(problems()).toEqual([])
    next()
    await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
    expect(problems()).toEqual([]) // the authoring screen, with its toolbar, palette, text areas and help
    write('Hello', '<p>Hi</p>')
    await checked()
    expect(problems()).toEqual([])
    fireEvent.click(previewButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(problems()).toEqual([]) // the preview, the samples in their frames, and the confirm
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))
    await screen.findByRole('heading', { name: t('newjob.done.title') })
    expect(problems()).toEqual([])
  })

  it('keeps each group of choices a native radio group (one name for the group, so the arrow keys move within it)', async () => {
    await open()
    chooseSender('newjob.sender.managed')
    const names = (radios: HTMLElement[]) => new Set(radios.map((r) => (r as HTMLInputElement).name))
    expect(names(screen.getAllByRole('radio'))).toHaveProperty('size', 1)
    next()
    await screen.findByRole('heading', { level: 2, name: heading(2, 'newjob.step.list') })
    expect(names(screen.getAllByRole('radio'))).toHaveProperty('size', 1)
  })
})

describe('the send page: who sends', () => {
  it('says how many credits Managed sending has', async () => {
    await open()
    chooseSender('newjob.sender.managed')
    expect(await screen.findByText(t('newjob.sender.managedCredits', { credits: 500 }))).toBeInTheDocument()
  })

  it('Your own SMTP: says the entitlement and the sender accounts are fine, and has no warning to go to the accounts page', async () => {
    const fetchFn = await open()
    expect(callsTo(fetchFn, 'GET', '/v1/m3/accounts')).toHaveLength(0) // nothing is asked about accounts until "Your own SMTP" is chosen
    chooseSender('newjob.sender.own')
    expect(await screen.findByText(t('newjob.sender.accountsReady', { count: 1 }))).toBeInTheDocument()
    expect(screen.getByText(t('newjob.entitlement.on'))).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: t('newjob.sender.accountsLink') })).not.toBeInTheDocument()
  })

  it('Your own SMTP: says when it is not switched on for the user, with a link to the accounts page', async () => {
    await open(platform({ 'GET /v1/balances': { ...BALANCES, m3: { enabled: false, until: null, state: 'unknown' } } }))
    chooseSender('newjob.sender.own')
    expect(await screen.findByText(t('newjob.entitlement.off'))).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.sender.accountsLink') })).toHaveAttribute('href', '/accounts')
  })

  it('Your own SMTP: says when it is suspended', async () => {
    await open(platform({ 'GET /v1/balances': { ...BALANCES, m3: { enabled: true, until: FAR_FUTURE_ENTITLEMENT, state: 'suspended' } } }))
    chooseSender('newjob.sender.own')
    expect(await screen.findByText(t('newjob.entitlement.suspended'))).toBeInTheDocument()
  })

  it('Your own SMTP: says when no sender account is ready (none, or only ones that cannot send), with a link to the accounts page', async () => {
    const accounts = { accounts: [account({ status: 'quarantined' }), account({ id: 'b', verification: 'failed' }), account({ id: 'c', from_gate: 'blocked' })], next_cursor: null }
    await open(platform({ 'GET /v1/m3/accounts': accounts }))
    chooseSender('newjob.sender.own')
    expect(await screen.findByText(t('newjob.sender.noAccount'))).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.sender.accountsLink') })).toHaveAttribute('href', '/accounts')
  })

  it('says as much as it can when the facts could not be read, and still lets the user go on', async () => {
    await open(platform({ 'GET /v1/balances': [503, { error: { code: 'module_unavailable', message: 'x', request_id: 'r' } }], 'GET /v1/m3/accounts': [500, { error: { code: 'internal_error', message: 'x', request_id: 'r' } }] }))
    chooseSender('newjob.sender.own')
    expect(await screen.findByText(t('newjob.sender.ownPlain'), { exact: false })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('newjob.step.next') })).toBeEnabled()
  })
})

describe('the send page: the list', () => {
  it('offers a CSV file (the same pickers as a verification) and the results of a finished verification', async () => {
    await open()
    await atList()
    expect(screen.getByRole('radio', { name: t('newjob.list.csv') })).toBeChecked()
    expect(screen.getByRole('radio', { name: t('newjob.list.verification') })).not.toBeChecked()
    expect(screen.getByLabelText(t('newjob.csv.file'))).toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('newjob.step.next') })).toBeDisabled()
    pick(csvFile(LIST))
    const select = (await screen.findByLabelText(t('newjob.columns.address'))) as HTMLSelectElement
    expect(select.value).toBe('1')
    const merge = screen.getByRole('list', { name: t('newjob.columns.mergeTitleSend') })
    expect(within(merge).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['first_name', 'plan'])
    expect(screen.getByRole('button', { name: t('newjob.step.next') })).toBeEnabled()
  })

  it('says which column names a message cannot use', async () => {
    await open()
    await atList()
    pick(csvFile('email,First Name,plan\na@x.test,Ann,pro\n'))
    const merge = await screen.findByRole('list', { name: t('newjob.columns.mergeTitleSend') })
    const items = within(merge).getAllByRole('listitem')
    expect(items[0]).toHaveTextContent(t('newjob.columns.unusable'))
    expect(items[1]).not.toHaveTextContent(t('newjob.columns.unusable')) // control: a usable name has no note
  })

  describe('from a finished verification', () => {
    const verification = (id: string, over: Partial<Job> = {}): Job => job({ id, status: 'succeeded', finished_at: '2026-12-01T10:00:00Z', preview: preview({ rows: 120, units: 100 }), ...over })
    const jobs = {
      jobs: [
        verification('v-1'),
        job({ id: 'running-1', status: 'running' }),
        sendJob({ id: 'send-1', status: 'succeeded' }),
        verification('v-2', { finished_at: '2026-11-30T10:00:00Z', preview: preview({ rows: 40, units: 40 }) }),
        job({ id: 'failed-1', status: 'failed' }),
        job({ id: 'cancelled-1', status: 'cancelled' }),
      ],
      next_cursor: null,
    }

    async function atVerification(routes: Routes = platform({ 'GET /v1/jobs': jobs })) {
      const fetchFn = await open(routes)
      await atList()
      fireEvent.click(screen.getByRole('radio', { name: t('newjob.list.verification') }))
      return fetchFn
    }

    it('lists only the user\'s finished verifications, newest first, with their counts', async () => {
      await atVerification()
      const group = await screen.findByRole('group', { name: t('newjob.verification.legend') })
      const options = within(group).getAllByRole('radio')
      expect(options).toHaveLength(2) // the running one, the send, the failed and the cancelled are not offered
      expect(group).toHaveTextContent('120 addresses, 100 checked')
      expect(group).toHaveTextContent('40 addresses, 40 checked')
      expect(options[0]).toHaveAttribute('value', 'v-1')
      expect(options[1]).toHaveAttribute('value', 'v-2')
    })

    it('says so, with a way to start one, when there is none', async () => {
      await atVerification(platform({ 'GET /v1/jobs': { jobs: [job({ status: 'running' })], next_cursor: null } }))
      expect(await screen.findByText(t('newjob.verification.emptyTitle'))).toBeInTheDocument()
      expect(screen.getByRole('link', { name: t('newjob.verification.emptyLink') })).toHaveAttribute('href', '/new/verify')
    })

    it('reads on through the pages until it has some, and offers Show more when the user has more jobs than it read', async () => {
      const seen: Array<string | null> = []
      const pages = platform({
        'GET /v1/jobs': (url: URL) => {
          const cursor = url.searchParams.get('cursor')
          seen.push(cursor)
          if (cursor === null) return { jobs: [job({ id: 'r1', status: 'running' })], next_cursor: 'c2' }
          return { jobs: [verification('v-9')], next_cursor: null }
        },
      })
      const fetchFn = await atVerification(pages)
      expect(await screen.findByRole('radio', { name: /Finished/ })).toHaveAttribute('value', 'v-9')
      expect(seen).toEqual([null, 'c2'])
      expect(callsTo(fetchFn, 'GET', '/v1/jobs')[0]?.url.searchParams.get('limit')).toBe('100')
      expect(screen.queryByRole('button', { name: t('newjob.verification.more') })).not.toBeInTheDocument()
    })

    it('shows the verdicts as fixed (valid and risky always in, invalid never), and the question about unknown addresses open, with no default', async () => {
      await atVerification()
      fireEvent.click((await screen.findAllByRole('radio', { name: /^Finished / }))[0] as HTMLElement)
      const valid = screen.getByRole('checkbox', { name: t('newjob.verdicts.valid') })
      const risky = screen.getByRole('checkbox', { name: t('newjob.verdicts.risky') })
      const invalid = screen.getByRole('checkbox', { name: t('newjob.verdicts.invalid') })
      for (const box of [valid, risky]) {
        expect(box).toBeChecked()
        expect(box).toBeDisabled()
      }
      expect(invalid).not.toBeChecked()
      expect(invalid).toBeDisabled()
      const include = screen.getByRole('radio', { name: t('newjob.unknown.include') })
      const exclude = screen.getByRole('radio', { name: t('newjob.unknown.exclude') })
      expect(include).not.toBeChecked()
      expect(exclude).not.toBeChecked()
    })

    it('will not go on until the question about unknown addresses is answered, and then goes on', async () => {
      await atVerification()
      const next = () => screen.getByRole('button', { name: t('newjob.step.next') })
      expect(next()).toBeDisabled()
      expect(screen.getByText(t('newjob.list.needVerification'))).toBeInTheDocument()
      fireEvent.click((await screen.findAllByRole('radio', { name: /^Finished / }))[0] as HTMLElement)
      expect(next()).toBeDisabled() // a verification is chosen, the question is not answered
      expect(screen.getByText(t('newjob.list.needChoice'))).toBeInTheDocument()
      fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.exclude') }))
      expect(next()).toBeEnabled()
    })

    it('sends no rows: it makes the draft from the finished verification, with the message and the answer about unknown addresses', async () => {
      const fetchFn = await atVerification()
      fireEvent.click((await screen.findAllByRole('radio', { name: /^Finished / }))[0] as HTMLElement)
      fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.include') }))
      next()
      await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
      fireEvent.change(screen.getByLabelText(new RegExp(t('newjob.authoring.replyTo'))), { target: { value: 'me@example.test' } })
      write('Hello', '<p>Hi {{email}}</p>', 'Hi')
      await checked()
      fireEvent.click(previewButton())
      await screen.findByRole('heading', { name: t('newjob.preview.title') })
      const [call, ...others] = callsTo(fetchFn, 'POST', '/v1/jobs')
      expect(others).toHaveLength(0)
      expect(call?.headers['Content-Type']).toBe('application/json')
      expect(bodyOf(call as { init: RequestInit })).toEqual({
        module: 'm2',
        source_job_id: 'v-1',
        content: { subject: 'Hello', html: '<p>Hi {{email}}</p>', text: 'Hi', reply_to: 'me@example.test' },
        settings: { unknown_choice: 'include' },
      })
    })

    it('control: the other answer is the other choice in the draft', async () => {
      const fetchFn = await atVerification()
      fireEvent.click((await screen.findAllByRole('radio', { name: /^Finished / }))[0] as HTMLElement)
      fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.exclude') }))
      next()
      await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
      write('Hello', '<p>Hi</p>')
      await checked()
      fireEvent.click(previewButton())
      await screen.findByRole('heading', { name: t('newjob.preview.title') })
      const body = bodyOf(callsTo(fetchFn, 'POST', '/v1/jobs')[0] as { init: RequestInit })
      expect(body['settings']).toEqual({ unknown_choice: 'exclude' })
      expect((body['content'] as Record<string, unknown>)['reply_to']).toBeUndefined() // no reply-to written, none sent
    })
  })
})

describe('the send page: the authoring screen', () => {
  it('has a subject, an HTML box in a monospace font, an optional text part that says it is made from the HTML, and a reply-to', async () => {
    await open()
    await atMessage()
    expect(subjectBox()).toBeInTheDocument()
    expect(htmlBox().tagName).toBe('TEXTAREA')
    expect(htmlBox().className).toContain('font-mono')
    expect(textBox()).toBeInTheDocument()
    expect(screen.getByText(t('newjob.authoring.textHint'))).toBeInTheDocument()
    expect(screen.getByLabelText(new RegExp(t('newjob.authoring.replyTo')))).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: t('newjob.authoring.toolbar') })).getAllByRole('button').length).toBeGreaterThanOrEqual(6)
  })

  it('has a palette fed by the file\'s columns and the two fields the platform fills in, and inserts {{name}} where the user is typing, keeping the focus there', async () => {
    await open()
    await atMessage()
    const box = htmlBox()
    fireEvent.change(box, { target: { value: 'Hi , bye' } })
    box.focus()
    box.setSelectionRange(3, 3)
    const fields = within(screen.getByRole('list', { name: t('newjob.authoring.columnsFields') })).getAllByRole('button')
    expect(fields.map((b) => b.getAttribute('aria-label'))).toEqual(['Insert first_name', 'Insert plan'])
    const reserved = within(screen.getByRole('list', { name: t('newjob.authoring.reservedFields') })).getAllByRole('button')
    expect(reserved.map((b) => b.textContent)).toEqual(['email', 'unsubscribe_url'])

    fireEvent.click(screen.getByRole('button', { name: 'Insert first_name' }))
    expect(box.value).toBe('Hi {{first_name}}, bye')
    expect(box).toHaveFocus()
    expect(box.selectionStart).toBe(3 + '{{first_name}}'.length) // the caret is after what was inserted: the user types on

    fireEvent.click(screen.getByRole('button', { name: 'Insert unsubscribe_url' }))
    expect(box.value).toBe('Hi {{first_name}}{{unsubscribe_url}}, bye')
  })

  it('inserts {{name|fallback}} when a fallback is written, and escapes a brace in it', async () => {
    await open()
    await atMessage()
    const box = htmlBox()
    box.focus()
    fireEvent.change(screen.getByLabelText(new RegExp(t('newjob.authoring.fallback'))), { target: { value: 'dear {friend}' } })
    fireEvent.click(screen.getByRole('button', { name: 'Insert first_name' }))
    expect(box.value).toBe('{{first_name|dear \\{friend\\}}}')
    // the two fields the platform fills in never take a fallback: they are never missing
    box.setSelectionRange(box.value.length, box.value.length)
    fireEvent.click(screen.getByRole('button', { name: 'Insert email' }))
    expect(box.value.endsWith('{{email}}')).toBe(true)
  })

  it('puts a field in the part the user was last in (the subject, or the text), and says which', async () => {
    await open()
    await atMessage()
    const subject = subjectBox()
    act(() => subject.focus())
    expect(screen.getByText(t('newjob.authoring.paletteHint', { target: t('common.part.subject') }))).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Insert plan' }))
    expect(subject.value).toBe('{{plan}}')
    expect(htmlBox().value).toBe('') // control: not in the HTML

    act(() => textBox().focus())
    expect(screen.getByText(t('newjob.authoring.paletteHint', { target: t('common.part.text') }))).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Insert plan' }))
    expect(textBox().value).toBe('{{plan}}')
  })

  it('wraps the selected text in the tag of the toolbar, and puts the caret inside a tag when nothing is selected', async () => {
    await open()
    await atMessage()
    const box = htmlBox()
    fireEvent.change(box, { target: { value: 'a big word' } })
    box.focus()
    box.setSelectionRange(2, 5)
    fireEvent.click(screen.getByRole('button', { name: t('newjob.authoring.tag.bold') }))
    expect(box.value).toBe('a <strong>big</strong> word')
    box.setSelectionRange(0, 0)
    fireEvent.click(screen.getByRole('button', { name: t('newjob.authoring.tag.paragraph') }))
    expect(box.value.startsWith('<p></p>a <strong>')).toBe(true)
    expect(box.selectionStart).toBe(3)
    fireEvent.click(screen.getByRole('button', { name: t('newjob.authoring.tag.unsubscribe') }))
    expect(box.value).toContain('<p><a href="{{unsubscribe_url}}">Unsubscribe</a></p>')
  })

  it('lists the columns a message cannot use, instead of offering them', async () => {
    await open()
    await atList()
    pick(csvFile('email,First Name,plan\na@x.test,Ann,pro\n'))
    await screen.findByLabelText(t('newjob.columns.address'))
    next()
    await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
    expect(screen.queryByRole('button', { name: 'Insert First Name' })).not.toBeInTheDocument()
    expect(screen.getByText(t('newjob.authoring.unusable'), { exact: false })).toHaveTextContent('First Name')
  })

  it('has a help panel for the grammar in plain words: fields with fallbacks, choices, escapes, and why braces in a style must be escaped', async () => {
    await open()
    await atMessage()
    const help = screen.getByText(t('newjob.help.summary')).closest('details') as HTMLElement
    for (const key of ['newjob.help.fieldsTitle', 'newjob.help.fallbackTitle', 'newjob.help.choicesTitle', 'newjob.help.escapesTitle', 'newjob.help.bracesTitle'] as const) {
      expect(within(help).getByRole('heading', { name: t(key) })).toBeInTheDocument()
    }
    expect(within(help).getByText(t('newjob.help.braces'), { exact: false })).toHaveTextContent('Curly brackets belong to this language')
    // the examples are what the user types, in a code font, exactly as the grammar reads them
    const written = within(help)
      .getAllByRole('cell')
      .filter((_, i) => i % 2 === 0)
      .map((c) => c.textContent)
    expect(written).toContain('Hi {{name|there}}')
    expect(written).toContain('{Hello|Hi|Hey}')
    expect(written).toContain('<style>p \\{ color: red \\}</style>')
  })

  it('says what the preview of the HTML is: your own text as typed, not the real messages', async () => {
    await open()
    await atMessage()
    expect(screen.getByText(t('newjob.authoring.previewNote'))).toBeInTheDocument()
    expect(screen.getByText(t('newjob.authoring.previewEmpty'))).toBeInTheDocument()
    write('Hello', '<p>Hi {{first_name}}</p>')
    const frame = await screen.findByTitle(t('newjob.authoring.previewFrame'), undefined, { timeout: 2000 })
    expect(frame.getAttribute('srcdoc')).toBe('<p>Hi {{first_name}}</p>') // a {{field}} stays literal there
  })

  it('shows the style block as it will look: an escaped brace is read as the brace', async () => {
    await open()
    await atMessage()
    write('Hello', '<style>p \\{ color: red \\}</style><p>x</p>')
    const frame = await screen.findByTitle(t('newjob.authoring.previewFrame'), undefined, { timeout: 2000 })
    expect(frame.getAttribute('srcdoc')).toBe('<style>p { color: red }</style><p>x</p>')
  })
})

describe('the send page: the platform\'s judgement of the message, as it is written', () => {
  const mistake = { part: 'html', line: 2, column: 5, message: 'unexpected }: write \\} for a literal brace' }

  it('shows where the mistake is under the field it is in: the line, the column, the grammar\'s sentence, and a marker under the place', async () => {
    const fetchFn = await open(platform({ 'POST /v1/templates/check': { ok: false, errors: [mistake] } }))
    await atMessage()
    write('Hello', '<p>\nabc} x</p>')
    const message = await screen.findByText('Line 2, column 5: unexpected }: write \\} for a literal brace', undefined, { timeout: 2000 })
    expect(message).toHaveAttribute('role', 'alert')
    expect(htmlBox()).toHaveAttribute('aria-invalid', 'true')
    expect(htmlBox().getAttribute('aria-describedby')).toContain(message.id)
    expect(subjectBox()).not.toHaveAttribute('aria-invalid') // the other parts are fine
    // the marker: the line of the template and a caret under the column
    const marker = [...document.querySelectorAll<HTMLElement>('[aria-hidden="true"]')].find((e) => e.textContent?.includes('^')) as HTMLElement
    expect(marker.textContent).toContain('abc} x</p>')
    expect(marker.textContent).toContain('    ^')
    // and it does not let the user on with it
    expect(previewButton()).toBeDisabled()
    expect(screen.getByText(t('newjob.message.fixMistakes'))).toBeInTheDocument()
    expect(JSON.parse(String(callsTo(fetchFn, 'POST', '/v1/templates/check')[0]?.init.body))).toEqual({ subject: 'Hello', html: '<p>\nabc} x</p>', text: '' })
  })

  it('shows a mistake of the subject and one of the text each under its own field', async () => {
    await open(
      platform({
        'POST /v1/templates/check': {
          ok: false,
          errors: [
            { part: 'subject', line: 1, column: 4, message: 'unclosed' },
            { part: 'text', line: 3, column: 1, message: 'stray' },
          ],
        },
      }),
    )
    await atMessage()
    write('Hi {{x', '<p>ok</p>', 'a\nb\n}')
    await screen.findByText('Line 1, column 4: unclosed', undefined, { timeout: 2000 })
    expect(subjectBox()).toHaveAttribute('aria-invalid', 'true')
    expect(textBox()).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText('Line 3, column 1: stray')).toBeInTheDocument()
    expect(htmlBox()).not.toHaveAttribute('aria-invalid')
  })

  it('control: a message with no mistake says it reads correctly, lists the fields it uses, and lets the user on', async () => {
    await open(platform({ 'POST /v1/templates/check': { ok: true, fields: [{ name: 'first_name', has_fallback: true }, { name: 'plan', has_fallback: false }] } }))
    await atMessage()
    write('Hello', '<p>{{first_name|there}} {{plan}}</p>')
    expect(await screen.findByText(t('newjob.authoring.ok'), undefined, { timeout: 2000 })).toBeInTheDocument()
    const used = screen.getByText(t('newjob.authoring.fieldsUsed'), { exact: false })
    expect(used).toHaveTextContent('first_name (has a fallback), plan (no fallback)')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(previewButton()).toBeEnabled()
  })

  it('says when a field the message needs (and gives no fallback) is not a column of the list', async () => {
    await open(platform({ 'POST /v1/templates/check': { ok: true, fields: [{ name: 'company', has_fallback: false }, { name: 'plan', has_fallback: false }, { name: 'city', has_fallback: true }] } }))
    await atMessage()
    write('Hello', '<p>{{company}} {{plan}} {{city|here}}</p>')
    const warning = await screen.findByText(t('newjob.authoring.unmetColumn'), { exact: false }, { timeout: 2000 })
    expect(warning).toHaveTextContent('company') // not a column
    expect(warning).not.toHaveTextContent('plan') // a column of the list
    expect(warning).not.toHaveTextContent('city') // has a fallback
  })

  it('does not go on while a check is still on its way, nor while the subject or the body is empty', async () => {
    await open()
    await atMessage()
    expect(previewButton()).toBeDisabled()
    expect(screen.getByText(t('newjob.message.needSubjectAndBody'))).toBeInTheDocument()
    fireEvent.change(subjectBox(), { target: { value: 'Hello' } })
    expect(previewButton()).toBeDisabled() // no body yet
    fireEvent.change(htmlBox(), { target: { value: '<p>Hi</p>' } })
    expect(previewButton()).toBeDisabled() // the check has not answered
    expect(screen.getByText(t('newjob.authoring.checking'), { selector: 'p' })).toBeInTheDocument()
    await checked()
  })

  it('lets the user go on when the check could not be made (the platform judges the message again when the draft is made)', async () => {
    await open(platform({ 'POST /v1/templates/check': [503, { error: { code: 'module_unavailable', message: 'SERVER-SENTENCE', request_id: 'req_6' } }] }))
    await atMessage()
    write('Hello', '<p>Hi</p>')
    expect(await screen.findByText(t('newjob.authoring.checkFailed'), { exact: false }, { timeout: 2000 })).toBeInTheDocument()
    expect(previewButton()).toBeEnabled()
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })

  it('refuses a reply-to that is not an address, and says so once the user has left the box', async () => {
    await open()
    await atMessage()
    write('Hello', '<p>Hi</p>')
    const reply = screen.getByLabelText(new RegExp(t('newjob.authoring.replyTo')))
    fireEvent.change(reply, { target: { value: 'not an address' } })
    expect(screen.queryByText(t('newjob.authoring.replyToInvalid'))).not.toBeInTheDocument() // not while typing
    fireEvent.blur(reply)
    expect(screen.getByText(t('newjob.authoring.replyToInvalid'))).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText(t('newjob.authoring.checking'), { selector: 'p' })).not.toBeInTheDocument(), { timeout: 3000 })
    expect(previewButton()).toBeDisabled()
    fireEvent.change(reply, { target: { value: 'me@example.test' } })
    await checked()
  })
})

describe('the send page: the draft is made when the user asks for the preview', () => {
  it('sends the file itself with the chosen column and the message in the query, and no row of its own', async () => {
    const file = csvFile(LIST)
    const fetchFn = await open()
    await atMessage(file)
    fireEvent.change(screen.getByLabelText(new RegExp(t('newjob.authoring.replyTo'))), { target: { value: ' me@example.test ' } })
    write('Hello {{first_name|there}}', '<p>Hi & welcome</p>', 'Hi')
    await checked()
    fireEvent.click(previewButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    const [call, ...others] = callsTo(fetchFn, 'POST', '/v1/jobs')
    expect(others).toHaveLength(0)
    expect(call?.init.body).toBe(file)
    expect(call?.headers['Content-Type']).toBe('text/csv')
    const q = call?.url.searchParams
    expect(Object.fromEntries(q ?? [])).toEqual({
      module: 'm2',
      email_column: 'work mail',
      subject: 'Hello {{first_name|there}}',
      html: '<p>Hi & welcome</p>',
      text: 'Hi',
      reply_to: 'me@example.test',
    })
  })

  it('does not send a part that was left blank (the text is then made from the HTML)', async () => {
    const fetchFn = await open()
    await atMessage()
    write('Hello', '<p>Hi</p>')
    await checked()
    fireEvent.click(previewButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    const q = callsTo(fetchFn, 'POST', '/v1/jobs')[0]?.url.searchParams
    expect(q?.has('text')).toBe(false)
    expect(q?.has('reply_to')).toBe(false)
  })

  it('says in words that a message is too long to travel with a file, before anything is sent', async () => {
    const fetchFn = await open()
    await atMessage()
    write('Hello', '<'.repeat(30_000)) // 30,000 characters that are each three in a query: far over 64 KB
    await checked()
    fireEvent.click(previewButton())
    expect(await screen.findByText(t('newjob.authoring.tooLong'), { selector: 'p' })).toBeInTheDocument()
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(0)
    expect(screen.getByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })).toBeInTheDocument() // still on the message
    expect(previewButton()).toBeDisabled()
    // control: shortened, it goes
    fireEvent.change(htmlBox(), { target: { value: '<p>short</p>' } })
    await checked()
    fireEvent.click(previewButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(1)
  })

  it('takes the user back to the message, with the mistake at its field, when the platform finds one the live check did not', async () => {
    await open(
      platform({
        'POST /v1/jobs': [400, { error: { code: 'validation_failed', message: 'SERVER-SENTENCE', request_id: 'req_7', details: { reason: 'the html template has a syntax error', part: 'html', line: 1, column: 3, message: 'unexpected }' } } }],
      }),
    )
    await atMessage()
    write('Hello', '<p>}</p>')
    await checked()
    fireEvent.click(previewButton())
    expect(await screen.findByText('Line 1, column 3: unexpected }')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })).toBeInTheDocument()
    expect(htmlBox().value).toBe('<p>}</p>') // what was written is kept
    expect(previewButton()).toBeDisabled()
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
    // writing again clears it
    fireEvent.change(htmlBox(), { target: { value: '<p>fine</p>' } })
    expect(screen.queryByText('Line 1, column 3: unexpected }')).not.toBeInTheDocument()
  })
})

describe('the send page: the preview', () => {
  const full = {
    rows: 10,
    malformed: 3,
    malformed_reasons: { no_at: 2, domain_no_dot: 1 },
    duplicates: 2,
    suppressed: 4,
    missing_fields: 2,
    missing_field_names: ['first_name', 'plan'],
    missing_field_names_truncated: true,
    unrenderable: 1,
    units: 7,
    credits: 7,
    available: 500,
    samples: samples(),
  }

  it('shows every count of a send: rows, will be sent, missing a field (with the field names), suppressed, unrenderable, malformed (with reasons) and repeats', async () => {
    await atPreview(platform({ 'POST /v1/jobs/job-send/preview': { job: sendJob({}, full) } }))
    expect(figure(t('newjob.preview.rows'))).toBe('10')
    expect(figure(t('newjob.preview.unitsSend'))).toBe('7')
    expect(figure(t('newjob.preview.missing'))).toContain('2')
    const missing = screen.getByText(t('newjob.preview.missing'), { selector: 'dt' }).parentElement as HTMLElement
    expect(missing).toHaveTextContent('Missing: first_name, plan and more')
    expect(figure(t('newjob.preview.suppressedSend'))).toContain('4')
    expect(figure(t('newjob.preview.unrenderable'))).toContain('1')
    expect(screen.getByText(t('newjob.preview.unrenderableNote'))).toBeInTheDocument()
    expect(figure(t('newjob.preview.malformed'))).toContain('3')
    expect(figure(t('newjob.preview.duplicates'))).toBe('2')
    const reasons = within(screen.getByText(t('newjob.preview.malformed'), { selector: 'dt' }).parentElement as HTMLElement).getAllByRole('listitem')
    expect(reasons.map((r) => r.textContent)).toEqual([`2 ${t('common.malformed.no_at')}`, `1 ${t('common.malformed.domain_no_dot')}`])
    // a send is not a verification: no outside-service line, and nothing about suppressed addresses being checked
    expect(screen.queryByText(t('newjob.disclosure.vendor'), { exact: false })).not.toBeInTheDocument()
    expect(screen.queryByText(t('newjob.preview.suppressedVerifyNote'))).not.toBeInTheDocument()
  })

  it('control: a list with nothing missing says no field names and has no note', async () => {
    await atPreview()
    expect(figure(t('newjob.preview.missing'))).toBe('0')
    expect(screen.queryByText(t('newjob.preview.missingNames'), { exact: false })).not.toBeInTheDocument()
    expect(screen.queryByText(t('newjob.preview.unrenderableNote'))).not.toBeInTheDocument()
  })

  it('shows the credits Managed sending holds, the credits the user has, and the shortfall with the next step', async () => {
    await atPreview(platform({ 'POST /v1/jobs/job-send/preview': { job: sendJob({}, { credits: 900, available: 500, shortfall: 400, units: 900, samples: samples() }) } }))
    expect(figure(t('newjob.preview.credits'))).toBe('900')
    expect(figure(t('newjob.preview.available'))).toBe('500')
    expect(figure(t('newjob.preview.shortfall'))).toBe('400')
    expect(screen.getByRole('button', { name: t('newjob.confirm.submitNow') })).toBeDisabled()
  })

  it('shows the entitlement, and no credits, for Your own SMTP', async () => {
    await atPreview(
      platform({
        'GET /v1/balances': { ...BALANCES, m3: { enabled: true, until: '2099-01-01T00:00:00Z', state: 'active' } },
        'POST /v1/jobs/job-send/preview': { job: sendJob({ module: 'm3' }, { metered: false, credits: 0, unit_cost: 0, samples: samples() }) },
      }),
    )
    expect(screen.queryByText(t('newjob.preview.credits'), { selector: 'dt' })).not.toBeInTheDocument()
    expect(figure(t('newjob.preview.noCredits'))).toBe(t('common.none'))
    expect(screen.getByText(/Sending through your own SMTP is switched on until/)).toBeInTheDocument()
  })

  it('says every message carries an unsubscribe link, in one sentence', async () => {
    await atPreview()
    expect(screen.getByText(t('newjob.preview.unsubscribe'))).toBeInTheDocument()
  })

  it('shows the several sample messages as cards, each with its subject, its text and its HTML in a sandboxed frame', async () => {
    await atPreview()
    const cards = [1, 2, 3].map((n) => screen.getByTestId(`sample-${n}`))
    expect(screen.queryByTestId('sample-4')).not.toBeInTheDocument()
    expect(within(cards[0] as HTMLElement).getByText('Hello Ann')).toBeInTheDocument()
    expect(within(cards[1] as HTMLElement).getByText('Hello Bo')).toBeInTheDocument()
    expect(within(cards[2] as HTMLElement).getByText('Hello Cy')).toBeInTheDocument()
    for (const [i, card] of cards.entries()) {
      const frame = within(card).getByTestId('preview-frame')
      expect(frame.getAttribute('srcdoc')).toBe(samples()[i]?.html)
      expect(frame).toHaveAttribute('title', t('newjob.samples.htmlOf', { n: i + 1 }))
      expect(within(card).getByRole('region', { name: t('newjob.samples.textOf', { n: i + 1 }) })).toHaveTextContent(samples()[i]?.text ?? '')
    }
    expect(within(cards[0] as HTMLElement).getByRole('heading', { name: 'Sample 1 (row 1)' })).toBeInTheDocument()
    expect(within(cards[2] as HTMLElement).getByRole('heading', { name: 'Sample 3 (row 5)' })).toBeInTheDocument()
  })

  it('says so when a sample has no HTML or no text, instead of showing an empty frame', async () => {
    await atPreview(platform({ 'POST /v1/jobs/job-send/preview': { job: sendJob({}, { samples: [{ ordinal: 1, subject: 'S', html: '', text: '' }] }) } }))
    const card = screen.getByTestId('sample-1')
    expect(within(card).queryByTestId('preview-frame')).not.toBeInTheDocument()
    expect(within(card).getByText(t('newjob.samples.noHtml'))).toBeInTheDocument()
    expect(within(card).getByText(t('newjob.samples.noText'))).toBeInTheDocument()
  })

  it('goes back to the message, lets the draft go, and keeps what the user wrote', async () => {
    const fetchFn = await atPreview()
    fireEvent.click(screen.getByRole('button', { name: t('newjob.send.editMessage') }))
    await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
    await waitFor(() => expect(callsTo(fetchFn, 'POST', '/v1/jobs/job-send/discard')).toHaveLength(1))
    expect(subjectBox().value).toBe('Hello {{first_name|there}}')
    expect(htmlBox().value).toBe('<p>Hi {{first_name|there}}</p>')
    // a new draft is made on the way forward (a draft's message cannot be changed)
    await checked()
    fireEvent.click(previewButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(2)
  })
})

describe('the send page: confirming', () => {
  const confirmButton = () => screen.getByRole('button', { name: t('newjob.confirm.submitNow') })

  it('sends the fingerprint and a key, and says how many will be sent and how many were left out', async () => {
    const fetchFn = await atPreview(platform({ 'POST /v1/jobs/job-send/confirm': () => confirmResult(sendJob(), { pending: 7, reserved: 7, suppressed: 4, missing_fields: 2, unrenderable: 1 }) }))
    fireEvent.click(confirmButton())
    expect(await screen.findByRole('heading', { name: t('newjob.done.title') })).toBeInTheDocument()
    const [call] = callsTo(fetchFn, 'POST', '/v1/jobs/job-send/confirm')
    expect(bodyOf(call as { init: RequestInit })).toEqual({ fingerprint: 'fp-1' })
    expect(call?.headers['Idempotency-Key']).toMatch(IDEMPOTENCY_KEY_PATTERN)
    expect(screen.getByText(t('newjob.done.pending', { count: 7 }))).toBeInTheDocument()
    expect(screen.getByText(t('newjob.done.suppressed', { count: 4 }))).toBeInTheDocument()
    expect(screen.getByText(t('newjob.done.missing', { count: 2 }))).toBeInTheDocument()
    expect(screen.getByText(t('newjob.done.unrenderable', { count: 1 }))).toBeInTheDocument()
    expect(screen.getByRole('link', { name: t('newjob.done.open') })).toHaveAttribute('href', '/jobs/job-send')
  })

  it('says what changed when the preview went stale, and shows the new samples', async () => {
    let previews = 0
    let confirms = 0
    await atPreview(
      platform({
        'POST /v1/jobs/job-send/preview': () => (++previews === 1 ? { job: sendJob({}, { samples: samples() }) } : { job: sendJob({}, { fingerprint: 'fp-2', missing_fields: 1, units: 2, credits: 2, samples: [{ ordinal: 9, subject: 'Fresh subject', html: '<p>fresh</p>', text: 'fresh' }] }) }),
        'POST /v1/jobs/job-send/confirm': () => (++confirms === 1 ? [409, { error: { code: 'preview_stale', message: 'SERVER-SENTENCE', request_id: 'r' } }] : confirmResult(sendJob())),
      }),
    )
    fireEvent.click(confirmButton())
    const notice = (await screen.findByText(t('newjob.stale.title'))).parentElement as HTMLElement
    expect(notice).toHaveTextContent('The rows missing a value changed from 0 to 1.')
    expect(notice).toHaveTextContent('The addresses that will be processed changed from 3 to 2.')
    expect(screen.getByText('Fresh subject')).toBeInTheDocument()
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })

  it('says Your own SMTP is not switched on, in words, when the platform refuses the confirm for it', async () => {
    await atPreview(platform({ 'POST /v1/jobs/job-send/confirm': [403, { error: { code: 'no_entitlement', message: 'SERVER-SENTENCE', request_id: 'r' } }] }))
    fireEvent.click(confirmButton())
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.no_entitlement'))
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })

  it('says nothing was charged and what to do when sending cannot start (a campaign that cannot run)', async () => {
    await atPreview(platform({ 'POST /v1/jobs/job-send/confirm': [409, { error: { code: 'infeasible', message: 'SERVER-SENTENCE', request_id: 'r', details: { causes: ['no_account'] } } }] }))
    fireEvent.click(confirmButton())
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.infeasible'))
    expect(alert).not.toHaveTextContent('no_account')
  })

  it.each([
    [409, 'job_slot_taken', 'You already have a job of this kind in progress', undefined],
    [422, 'nothing_to_do', 'None of the addresses in this list can be processed', undefined],
    [402, 'insufficient_credits', 'you are 150 short', { available: 50, needed: 200, shortfall: 150 }],
    [503, 'module_unavailable', 'We cannot reach the service that runs this right now', undefined],
  ] as Array<[number, string, string, Record<string, unknown> | undefined]>)('answers a refusal (%s %s) with its own words and no code', async (status, code, words, details) => {
    await atPreview(platform({ 'POST /v1/jobs/job-send/confirm': [status, { error: { code, message: 'SERVER-SENTENCE', request_id: 'r', details } }] }))
    fireEvent.click(confirmButton())
    expect((await screen.findByRole('alert')).textContent).toContain(words)
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE'])
  })
})

describe('the send page: unknown addresses of a chained send', () => {
  const chained = (over: Partial<Preview> = {}) =>
    sendJob(
      { source_job_id: 'v-1', settings: { unknown_choice: null } },
      { verdicts: { valid: 50, risky: 20, invalid: 10, unknown: 5 }, selected: 75, unknown_count: 5, unknown_choice_required: true, unknown_choice: null, rows: 85, units: 75, credits: 75, samples: samples(), ...over },
    )
  const verifications = { jobs: [job({ id: 'v-1', status: 'succeeded', finished_at: '2026-12-01T10:00:00Z', preview: preview({ rows: 85, units: 85 }) })], next_cursor: null }

  async function atChainedPreview(routes: Routes) {
    const fetchFn = await open(platform({ 'GET /v1/jobs': verifications, ...routes }))
    await atList()
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.list.verification') }))
    fireEvent.click((await screen.findAllByRole('radio', { name: /^Finished / }))[0] as HTMLElement)
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.include') }))
    next()
    await screen.findByRole('heading', { level: 2, name: heading(3, 'newjob.step.message') })
    write('Hello', '<p>Hi</p>')
    await checked()
    fireEvent.click(previewButton())
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    return fetchFn
  }

  it('shows what the verification found, by verdict, and what was chosen from it', async () => {
    await atChainedPreview({ 'POST /v1/jobs/job-send/preview': { job: chained({ unknown_choice: 'include' }) } })
    const row = screen.getByText(t('newjob.preview.fromVerification'), { selector: 'dt' }).parentElement as HTMLElement
    expect(row).toHaveTextContent('Valid: 50')
    expect(row).toHaveTextContent('Risky: 20')
    expect(row).toHaveTextContent('Invalid: 10')
    expect(row).toHaveTextContent('Unknown: 5')
    expect(figure(t('newjob.preview.selected'))).toBe('75')
  })

  it('confirms with the choice the preview was made under, and makes it again when the user changes it', async () => {
    let previews = 0
    const fetchFn = await atChainedPreview({
      'POST /v1/jobs/job-send/preview': (_url: URL, init: RequestInit) => {
        previews++
        const body = init.body === undefined ? null : (JSON.parse(String(init.body)) as { unknown_choice?: 'include' | 'exclude' })
        const choice = body?.unknown_choice ?? 'include'
        return { job: chained({ unknown_choice: choice, fingerprint: `fp-${choice}`, units: choice === 'include' ? 75 : 70 }) }
      },
    })
    expect(screen.getByRole('radio', { name: t('newjob.unknown.include') })).toBeChecked()
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.exclude') }))
    await waitFor(() => expect(screen.getByRole('radio', { name: t('newjob.unknown.exclude') })).toBeChecked())
    const previewCalls = callsTo(fetchFn, 'POST', '/v1/jobs/job-send/preview')
    expect(previews).toBe(2)
    expect(JSON.parse(String(previewCalls[1]?.init.body))).toEqual({ unknown_choice: 'exclude' })
    expect(figure(t('newjob.preview.unitsSend'))).toBe('70')

    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))
    await screen.findByRole('heading', { name: t('newjob.done.title') })
    expect(bodyOf(callsTo(fetchFn, 'POST', '/v1/jobs/job-send/confirm')[0] as { init: RequestInit })).toEqual({ fingerprint: 'fp-exclude', unknown_choice: 'exclude' })
  })

  it('will not confirm while the question is open (a draft continued without an answer), and answering it makes the preview again with the answer', async () => {
    const draft = sendJob({ status: 'draft', source_job_id: 'v-1', preview: null })
    const fetchFn = await open(
      platform({
        'GET /v1/jobs/job-send': { job: draft },
        'POST /v1/jobs/job-send/preview': (_u: URL, init: RequestInit) => {
          const choice = init.body === undefined ? null : (JSON.parse(String(init.body)) as { unknown_choice: 'include' }).unknown_choice
          return { job: chained({ unknown_choice: choice, fingerprint: choice === null ? 'fp-open' : 'fp-answered' }) }
        },
      }),
      '/new/send?job=job-send',
    )
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    const confirm = screen.getByRole('button', { name: t('newjob.confirm.submitNow') })
    expect(confirm).toBeDisabled()
    expect(screen.getByText(t('newjob.confirm.blockedChoice'))).toBeInTheDocument()
    expect(screen.getByRole('radio', { name: t('newjob.unknown.include') })).not.toBeChecked()
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.unknown.include') }))
    await waitFor(() => expect(screen.getByRole('button', { name: t('newjob.confirm.submitNow') })).toBeEnabled())
    expect(JSON.parse(String(callsTo(fetchFn, 'POST', '/v1/jobs/job-send/preview')[1]?.init.body))).toEqual({ unknown_choice: 'include' })
    expect(screen.queryByText(t('newjob.confirm.blockedChoice'))).not.toBeInTheDocument()
  })

  it('unknown_choice_required: when the platform says the question is still open, the screen says so at the question and does not show the code', async () => {
    await atChainedPreview({
      'POST /v1/jobs/job-send/preview': { job: chained({ unknown_choice: 'include', unknown_choice_required: false }) },
      'POST /v1/jobs/job-send/confirm': [422, { error: { code: 'unknown_choice_required', message: 'SERVER-SENTENCE', request_id: 'r', details: { unknown_rows: 5, allowed: ['include', 'exclude'] } } }],
    })
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))
    const alert = await screen.findByText(t('error.unknown_choice_required'), { selector: 'p' })
    expect(alert).toBeInTheDocument()
    expect(screen.getByText(t('newjob.unknown.open'))).toBeInTheDocument()
    expectNoCodeWords(document.body.textContent ?? '', ['SERVER-SENTENCE', 'unknown_rows'])
  })
})

describe('the send page: a draft continued with ?job=', () => {
  it('goes straight to the preview, with the samples, shows the message it was written with, and confirms', async () => {
    const draft = sendJob({ status: 'draft', preview: null, content: { subject: 'The subject', html: '<p>x</p>', text: '', reply_to: 'me@example.test' } })
    const fetchFn = await open(
      platform({
        'GET /v1/jobs/job-send': { job: draft },
        'POST /v1/jobs/job-send/preview': { job: sendJob({ content: draft.content }, { samples: samples() }) },
      }),
      '/new/send?job=job-send',
    )
    await screen.findByRole('heading', { name: t('newjob.preview.title') })
    expect(screen.queryByRole('navigation', { name: t('newjob.step.label') })).not.toBeInTheDocument() // no steps to walk
    expect(screen.getByRole('heading', { name: t('newjob.send.yourMessage') })).toBeInTheDocument()
    expect(screen.getByText('The subject')).toBeInTheDocument()
    expect(screen.getByText('me@example.test')).toBeInTheDocument()
    expect(screen.getByTestId('sample-1')).toBeInTheDocument()
    expect(callsTo(fetchFn, 'POST', '/v1/jobs')).toHaveLength(0)
    fireEvent.click(screen.getByRole('button', { name: t('newjob.confirm.submitNow') }))
    await screen.findByRole('heading', { name: t('newjob.done.title') })
  })

  it('points a verification draft to the verify page', async () => {
    await open(platform({ 'GET /v1/jobs/job-1': { job: job({ status: 'draft', preview: null }) } }), '/new/send?job=job-1')
    expect(await screen.findByRole('link', { name: t('newjob.review.goVerify') })).toHaveAttribute('href', '/new/verify?job=job-1')
    expect(screen.queryByRole('heading', { name: t('newjob.preview.title') })).not.toBeInTheDocument()
  })
})
