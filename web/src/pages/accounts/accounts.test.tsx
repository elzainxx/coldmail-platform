// The SMTP accounts screen: the list, the entitlement notice, Add (validation, the acknowledgement, every refusal in words), Test, Add many, Remove, and the password
// that must never go anywhere but the one request. Every property has a paired control: the same step that must fail when the property breaks.
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { FromGateState, M3Account, M3Entitlement } from '../../api/types.ts'
import { t, type CopyKey } from '../../copy/index.ts'
import { jsonResponse, mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { BULK_HEADER } from './accountForm.ts'

const ID = '3f1e2d4c-0000-4000-8000-000000000001'
const ID2 = '3f1e2d4c-0000-4000-8000-000000000002'
const CANARY = 'CANARY-pw-7Zq!x9'
const LEAK = 'LEAK-the-server-sentence'

function account(over: Partial<M3Account> = {}): M3Account {
  return {
    id: ID,
    smtp_host: 'smtp.example.test',
    smtp_port: 587,
    smtp_username: 'ann',
    from_address: 'ann@example.test',
    from_name: 'Ann',
    daily_limit: 200,
    has_credential: true,
    status: 'active',
    pause_reason: null,
    quarantine_reason: null,
    verification: 'verified',
    verify_code: null,
    from_gate: 'aligned',
    created_at: '2026-11-20T10:00:00Z',
    ...over,
  }
}

const ENABLED: { m3: M3Entitlement; modules: object } = { modules: { m1: { available: 1, reserved: 0 }, m2: { available: 1, reserved: 0 } }, m3: { enabled: true, until: null, state: 'active' } }

type Fetch = ReturnType<typeof mockApi>

function requests(fn: Fetch, method: string, path: string) {
  return fn.mock.calls
    .map(([input, init]) => ({ url: new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, window.location.origin), init: init ?? {} }))
    .filter((c) => (c.init.method ?? 'GET').toUpperCase() === method && c.url.pathname === path)
}

const bodyOf = (c: { init: RequestInit }): Record<string, unknown> => JSON.parse(String(c.init.body)) as Record<string, unknown>

function apiError(code: string, status: number, details?: Record<string, unknown>, headers: Record<string, string> = {}): Response {
  return jsonResponse({ error: { code, message: LEAK, request_id: 'req_00000000000000000000000000000001', ...(details === undefined ? {} : { details }) } }, status, headers)
}

async function openAccounts(routes: Record<string, unknown> = {}): Promise<Fetch> {
  await signInForTest()
  const fn = mockApi({
    'GET /v1/m3/accounts': { accounts: [], next_cursor: null },
    'GET /v1/balances': ENABLED,
    'GET /v1/me': { email: 'person@example.test' },
    ...routes,
  })
  renderApp('/accounts')
  await screen.findByRole('heading', { level: 1, name: t('accounts.title') })
  return fn
}

const main = () => screen.getByRole('main')
const field = (label: string) => screen.getByLabelText(new RegExp(`^${label}`))
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } })

function fillAccount(over: Partial<Record<'host' | 'port' | 'user' | 'secret' | 'from' | 'name' | 'daily', string>> = {}) {
  const v = { host: 'smtp.example.test', port: '', user: 'ann', secret: CANARY, from: 'ann@example.test', name: '', daily: '', ...over }
  type(field(t('accounts.add.host')), v.host)
  type(field(t('accounts.add.port')), v.port)
  type(field(t('accounts.add.username')), v.user)
  type(field(t('accounts.add.password')), v.secret)
  type(field(t('accounts.add.fromAddress')), v.from)
  type(field(t('accounts.add.fromName')), v.name)
  type(field(t('accounts.add.dailyLimit')), v.daily)
}

const openAddForm = async () => {
  fireEvent.click(within(main()).getByRole('button', { name: t('accounts.add.open') }))
  await screen.findByRole('heading', { level: 2, name: t('accounts.add.title') })
}
const submitAdd = (anyway = false) => fireEvent.click(screen.getByRole('button', { name: anyway ? t('accounts.add.submitAnyway') : t('accounts.add.submit') }))

/** What a screen must never show: a code, a snake_case word, the sentence the server sent. */
function expectNoInternalText() {
  const text = document.body.textContent ?? ''
  expect(text).not.toMatch(/\b[a-z]+_[a-z0-9_]+\b/)
  expect(text).not.toContain(LEAK)
  for (const word of ['unaligned', 'indeterminate', 'unverified', 'quarantined', 'unrecognised']) expect(text, word).not.toContain(word)
}

function watchConsole() {
  const seen: string[] = []
  const show = (a: unknown): string => {
    if (typeof a === 'string') return a
    if (a instanceof Error) return `${a.message}\n${a.stack ?? ''}`
    try {
      return JSON.stringify(a)
    } catch {
      return String(a)
    }
  }
  const spies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) =>
    vi.spyOn(console, m).mockImplementation((...args: unknown[]) => {
      seen.push(args.map(show).join(' '))
    }),
  )
  return { seen, restore: () => spies.forEach((s) => s.mockRestore()) }
}

function watchStorage() {
  const seen: string[] = []
  const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation((k: string, v: string) => {
    seen.push(`${k}=${v}`)
  })
  const cookie = vi.spyOn(document, 'cookie', 'set').mockImplementation((v: string) => {
    seen.push(`cookie=${v}`)
  })
  return { seen, restore: () => (setItem.mockRestore(), cookie.mockRestore()) }
}

beforeEach(() => {
  resetForTest()
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  resetForTest()
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('the list', () => {
  it('shows each account with its sending address, server, status and the check of the From address, in words', async () => {
    await openAccounts({ 'GET /v1/m3/accounts': { accounts: [account(), account({ id: ID2, from_address: 'bob@example.test', smtp_host: 'mail.example.test', smtp_port: 465, status: 'paused', pause_reason: 'daily_limit', from_gate: 'unaligned_warned' })], next_cursor: null } })
    const first = await screen.findByTestId(`account-row-${ID}`)
    expect(within(first).getByText('ann@example.test')).toBeInTheDocument()
    expect(within(first).getByText('smtp.example.test:587')).toBeInTheDocument()
    expect(within(first).getByText(t('accounts.status.active'))).toBeInTheDocument()
    expect(within(first).getByText(t('accounts.fromGate.aligned'))).toBeInTheDocument()
    expect(within(first).getByText(t('accounts.fromGate.alignedWhy'))).toBeInTheDocument()
    const second = screen.getByTestId(`account-row-${ID2}`)
    expect(within(second).getByText('mail.example.test:465')).toBeInTheDocument()
    expect(within(second).getByText(t('accounts.status.paused'))).toBeInTheDocument()
    expect(within(second).getByText(t('accounts.pause.dailyLimit'))).toBeInTheDocument()
    // control: the two rows do not read alike, and a list with accounts does not say it is empty
    expect(within(second).queryByText(t('accounts.fromGate.aligned'))).not.toBeInTheDocument()
    expect(screen.queryByText(t('accounts.empty.title'))).not.toBeInTheDocument()
    expect(within(first).queryByText(t('accounts.status.paused'))).not.toBeInTheDocument()
  })

  it.each<[FromGateState, CopyKey, CopyKey]>([
    ['aligned', 'accounts.fromGate.aligned', 'accounts.fromGate.alignedWhy'],
    ['unverified', 'accounts.fromGate.unverified', 'accounts.fromGate.unverifiedWhy'],
    ['unaligned_warned', 'accounts.fromGate.warned', 'accounts.fromGate.warnedWhy'],
    ['blocked', 'accounts.fromGate.blocked', 'accounts.fromGate.blockedWhy'],
    ['indeterminate', 'accounts.fromGate.indeterminate', 'accounts.fromGate.indeterminateWhy'],
  ])('says the From address check %s with a word and one line, and no code', async (state, label, why) => {
    await openAccounts({ 'GET /v1/m3/accounts': { accounts: [account({ from_gate: state })], next_cursor: null } })
    const row = await screen.findByTestId(`account-row-${ID}`)
    expect(within(row).getByText(t(label))).toBeInTheDocument()
    expect(within(row).getByText(t(why))).toBeInTheDocument()
    expectNoInternalText()
    expect(row.textContent ?? '').not.toContain(state) // the raw word is not on the screen
  })

  it('shows a state it does not know as "could not be determined", never the word, and never a sentence the module might have written', async () => {
    const odd = { ...account({ from_gate: 'brand_new_state' as FromGateState, status: 'on_fire' as never }), from_gate_message: LEAK, message: LEAK }
    await openAccounts({ 'GET /v1/m3/accounts': { accounts: [odd], next_cursor: null } })
    const row = await screen.findByTestId(`account-row-${ID}`)
    expect(within(row).getByText(t('accounts.fromGate.indeterminate'))).toBeInTheDocument()
    expectNoInternalText()
    expect(document.body.textContent).not.toContain('brand_new_state')
    expect(document.body.textContent).not.toContain('on_fire')
  })

  it('says why an account is on hold, and why a login check failed, in words', async () => {
    await openAccounts({
      'GET /v1/m3/accounts': {
        accounts: [account({ status: 'quarantined', quarantine_reason: 'auth_failure', verification: 'failed', verify_code: 'verify_auth_failed' })],
        next_cursor: null,
      },
    })
    const row = await screen.findByTestId(`account-row-${ID}`)
    expect(within(row).getByText(t('accounts.status.quarantined'))).toBeInTheDocument()
    expect(within(row).getByText(t('accounts.hold.authFailure'))).toBeInTheDocument()
    expect(within(row).getByText(t('accounts.verification.failed', { why: t('accounts.why.authFailed') }))).toBeInTheDocument()
    expectNoInternalText()
  })

  it('explains an empty list and offers the way to add one', async () => {
    await openAccounts()
    expect(await screen.findByText(t('accounts.empty.title'))).toBeInTheDocument()
    expect(screen.getByText(t('accounts.empty.body'))).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: t('accounts.add.open') })).toHaveLength(1) // one way to add, so a script or a reader is not offered two of the same name
  })

  it('pages with the cursor of the module: the next page is asked for with it, the first page without', async () => {
    const fn = await openAccounts({
      'GET /v1/m3/accounts': (url: URL) =>
        url.searchParams.get('cursor') === 'c2'
          ? { accounts: [account({ id: ID2, from_address: 'bob@example.test' })], next_cursor: null }
          : { accounts: [account()], next_cursor: 'c2' },
    })
    await screen.findByTestId(`account-row-${ID}`)
    expect(requests(fn, 'GET', '/v1/m3/accounts')[0]?.url.searchParams.has('cursor')).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByTestId(`account-row-${ID2}`)
    expect(requests(fn, 'GET', '/v1/m3/accounts').at(-1)?.url.searchParams.get('cursor')).toBe('c2')
    expect(screen.getByRole('button', { name: t('common.pagination.next') })).toBeDisabled() // control: the last page has no next
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.previous') }))
    await screen.findByTestId(`account-row-${ID}`)
  })

  it('says it could not load the list, in words, and tries again on request', async () => {
    let healthy = false
    await openAccounts({ 'GET /v1/m3/accounts': () => (healthy ? { accounts: [account()], next_cursor: null } : apiError('module_unavailable', 503)) })
    expect(await screen.findByText(t('accounts.loadError'))).toBeInTheDocument()
    expect(screen.getByText(t('error.module_unavailable'))).toBeInTheDocument()
    expectNoInternalText()
    healthy = true
    fireEvent.click(screen.getByRole('button', { name: t('common.retry') }))
    await screen.findByTestId(`account-row-${ID}`)
    expect(screen.queryByText(t('accounts.loadError'))).not.toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('whether sending through the user\'s own accounts is switched on', () => {
  const FUTURE = '2099-06-15T12:00:00Z'
  const PAST = '2020-06-15T12:00:00Z'
  it.each<[string, M3Entitlement, string | null]>([
    ['never switched on', { enabled: false, until: null, state: 'unknown' }, t('accounts.entitlement.none')],
    ['ended', { enabled: false, until: PAST, state: 'active' }, t('accounts.entitlement.ended', { date: 'June 15, 2020' })],
    ['switched off before its end', { enabled: false, until: FUTURE, state: 'active' }, t('accounts.entitlement.none')],
    ['suspended', { enabled: true, until: null, state: 'suspended' }, t('accounts.entitlement.suspended')],
    ['switched on until a date', { enabled: true, until: FUTURE, state: 'active' }, t('accounts.entitlement.until', { date: 'June 15, 2099' })],
    ['switched on with no end', { enabled: true, until: null, state: 'active' }, null],
  ])('%s', async (_name, m3, line) => {
    await openAccounts({ 'GET /v1/balances': { ...ENABLED, m3 } })
    if (line === null) {
      // control: a user who may use it is told nothing
      await screen.findByText(t('accounts.empty.title'))
      expect(main().textContent).not.toContain(t('accounts.entitlement.none'))
      expect(main().textContent).not.toMatch(/switched on|suspended|ended on/i)
    } else {
      expect(await screen.findByText(line)).toBeInTheDocument()
    }
    expectNoInternalText()
  })

  it('says nothing, and still shows the list, when the balances cannot be read', async () => {
    await openAccounts({ 'GET /v1/balances': () => apiError('internal_error', 500), 'GET /v1/m3/accounts': { accounts: [account()], next_cursor: null } })
    await screen.findByTestId(`account-row-${ID}`)
    expect(main().textContent).not.toContain(t('accounts.entitlement.none'))
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Add an account: validation', () => {
  it('names what is missing or wrong, in words, sends nothing, and keeps what was typed', async () => {
    const fn = await openAccounts()
    await openAddForm()
    submitAdd()
    expect(await screen.findAllByText(t('error.field.required'))).toHaveLength(4) // server, username, password, From address
    expect(field(t('accounts.add.host'))).toHaveAttribute('aria-invalid', 'true')
    expect(field(t('accounts.add.port'))).not.toHaveAttribute('aria-invalid')
    expect(requests(fn, 'POST', '/v1/m3/accounts')).toHaveLength(0)

    fillAccount({ host: 'smtp.example.test', port: '70000', user: 'ann', secret: CANARY, from: 'not-an-address', daily: 'many' })
    submitAdd()
    expect(await screen.findByText(t('error.field.out_of_range'))).toBeInTheDocument() // the port
    expect(screen.getAllByText(t('error.field.malformed'))).toHaveLength(2) // the From address and the daily limit
    expect(field(t('accounts.add.password'))).toHaveValue(CANARY) // nothing was sent, so nothing is lost
    expect(requests(fn, 'POST', '/v1/m3/accounts')).toHaveLength(0)
    expectNoInternalText()
  })

  it('control: the same form with valid values is sent, once, with the values as typed', async () => {
    const fn = await openAccounts({ 'POST /v1/m3/accounts': [201, { account: account() }] })
    await openAddForm()
    fillAccount({ host: ' smtp.example.test ', port: '2525', name: 'Ann Example', daily: '150' })
    submitAdd()
    await screen.findByRole('heading', { level: 2, name: t('accounts.added.title', { address: 'ann@example.test' }) })
    const sent = requests(fn, 'POST', '/v1/m3/accounts')
    expect(sent).toHaveLength(1)
    expect(bodyOf(sent[0]!)).toEqual({ smtp_host: 'smtp.example.test', smtp_port: 2525, smtp_username: 'ann', smtp_secret: CANARY, from_address: 'ann@example.test', from_name: 'Ann Example', daily_limit: 150 })
    expect((sent[0]!.init.headers as Record<string, string>)['Authorization']).toMatch(/^Bearer /)
  })

  it('has no field for a security setting (the API has none: the port decides) and the password field is a password field', async () => {
    await openAccounts()
    await openAddForm()
    expect(field(t('accounts.add.password'))).toHaveAttribute('type', 'password')
    expect(screen.queryByLabelText(/security|encryption|tls|ssl/i)).not.toBeInTheDocument()
    expect(screen.getByText(t('accounts.add.portHint'))).toBeInTheDocument()
  })
})

describe('Add an account: what it came to', () => {
  it.each<[string, M3Account, string, CopyKey, CopyKey]>([
    ['the login works and the address is aligned', account(), t('accounts.verification.verified'), 'accounts.fromGate.aligned', 'accounts.fromGate.alignedWhy'],
    [
      'the login failed',
      account({ verification: 'failed', verify_code: 'verify_tls_failed', from_gate: 'unverified' }),
      t('accounts.added.loginFailed', { why: t('accounts.why.tlsFailed') }),
      'accounts.fromGate.unverified',
      'accounts.fromGate.unverifiedWhy',
    ],
    ['the login was not checked', account({ verification: 'unverified', from_gate: 'indeterminate' }), t('accounts.added.loginUnchecked'), 'accounts.fromGate.indeterminate', 'accounts.fromGate.indeterminateWhy'],
    ['the address is blocked (the account is added, and says what to change)', account({ from_gate: 'blocked' }), t('accounts.verification.verified'), 'accounts.fromGate.blocked', 'accounts.fromGate.blockedWhy'],
  ])('%s', async (_name, added, loginLine, gateWord, gateWhy) => {
    await openAccounts({ 'POST /v1/m3/accounts': [201, { account: added }] })
    await openAddForm()
    fillAccount()
    submitAdd()
    const heading = await screen.findByRole('heading', { level: 2, name: t('accounts.added.title', { address: 'ann@example.test' }) })
    const card = heading.closest('section') as HTMLElement
    expect(within(card).getByText(loginLine)).toBeInTheDocument()
    expect(within(card).getByText(t(gateWord))).toBeInTheDocument()
    expect(within(card).getByText(t(gateWhy))).toBeInTheDocument()
    expect(screen.queryByRole('heading', { level: 2, name: t('accounts.add.title') })).not.toBeInTheDocument() // the form went away
    await waitFor(() => expect(card.parentElement).toHaveFocus()) // and the user is taken to the result, so it is read
    expectNoInternalText()
  })

  it('reads the list again after an add, so the new account is in it', async () => {
    let list: M3Account[] = []
    const fn = await openAccounts({
      'GET /v1/m3/accounts': () => ({ accounts: list, next_cursor: null }),
      'POST /v1/m3/accounts': () => {
        list = [account()]
        return [201, { account: account() }]
      },
    })
    expect(requests(fn, 'GET', '/v1/m3/accounts')).toHaveLength(1)
    await openAddForm()
    fillAccount()
    submitAdd()
    await screen.findByTestId(`account-row-${ID}`)
    expect(requests(fn, 'GET', '/v1/m3/accounts')).toHaveLength(2)
  })
})

describe('Add an account: the warning that has to be confirmed', () => {
  function warnedUntilConfirmed(): Record<string, unknown> {
    return {
      'POST /v1/m3/accounts': (_u: URL, init: RequestInit) => {
        const body = JSON.parse(String(init.body)) as Record<string, unknown>
        return body['acknowledge_from_gate'] === true
          ? [201, { account: account({ from_gate: 'unaligned_warned' }) }]
          : apiError('from_gate_ack_required', 422, { from_gate: 'unaligned_warned' })
      },
    }
  }

  it('asks for the box to be ticked, and sends the account again with the confirmation only after it is', async () => {
    const fn = await openAccounts(warnedUntilConfirmed())
    await openAddForm()
    fillAccount()
    submitAdd()
    const box = await screen.findByRole('checkbox', { name: t('accounts.ack.checkbox') })
    expect(screen.getByText(t('accounts.ack.body'))).toBeInTheDocument()
    let sent = requests(fn, 'POST', '/v1/m3/accounts')
    expect(sent).toHaveLength(1)
    expect(bodyOf(sent[0]!)['acknowledge_from_gate']).not.toBe(true) // the first try does not claim an acknowledgement
    expect(box).not.toBeChecked()
    await waitFor(() => expect(box).toHaveFocus())

    // control: pressing the button without the box ticked sends nothing
    submitAdd(true)
    expect(await screen.findByText(t('accounts.ack.needed'))).toBeInTheDocument()
    expect(requests(fn, 'POST', '/v1/m3/accounts')).toHaveLength(1)

    fireEvent.click(box)
    expect(box).toBeChecked()
    submitAdd(true)
    await screen.findByRole('heading', { level: 2, name: t('accounts.added.title', { address: 'ann@example.test' }) })
    sent = requests(fn, 'POST', '/v1/m3/accounts')
    expect(sent).toHaveLength(2)
    expect(bodyOf(sent[1]!)['acknowledge_from_gate']).toBe(true)
    expect(bodyOf(sent[1]!)['smtp_secret']).toBe(CANARY) // the same account, without typing the password twice
    expect(screen.getByText(t('accounts.fromGate.warned'))).toBeInTheDocument()
    expectNoInternalText()
  })

  it('does not carry a confirmation over to an address that was changed after the warning', async () => {
    const fn = await openAccounts(warnedUntilConfirmed())
    await openAddForm()
    fillAccount()
    submitAdd()
    const box = await screen.findByRole('checkbox', { name: t('accounts.ack.checkbox') })
    fireEvent.click(box)
    type(field(t('accounts.add.fromAddress')), 'other@example.test')
    expect(screen.queryByRole('checkbox', { name: t('accounts.ack.checkbox') })).not.toBeInTheDocument() // the box went away with the old address
    submitAdd()
    await screen.findByRole('checkbox', { name: t('accounts.ack.checkbox') }) // and the new address is warned about again
    const sent = requests(fn, 'POST', '/v1/m3/accounts')
    expect(sent).toHaveLength(2)
    expect(bodyOf(sent[1]!)['acknowledge_from_gate']).not.toBe(true)
  })
})

// The refusals, each shown as its line of the catalogue; the sentence the server sent and every code stay out of the page.
interface Refusal {
  name: string
  status: number
  code: string
  details?: Record<string, unknown>
  headers?: Record<string, string>
  line: string
  extra?: string
}

const REFUSALS: Refusal[] = [
  { name: 'a duplicate account', status: 409, code: 'account_duplicate', line: t('error.account_duplicate') },
  { name: 'an action that is not available', status: 409, code: 'account_action_unavailable', line: t('error.account_action_unavailable') },
  { name: 'sending through own accounts not switched on', status: 403, code: 'no_entitlement', line: t('error.no_entitlement') },
  { name: 'a suspended user', status: 403, code: 'suspended', line: t('error.suspended') },
  { name: 'a rate limit, with the wait', status: 429, code: 'rate_limited', headers: { 'Retry-After': '300' }, line: t('error.rate_limited'), extra: t('accounts.wait.minutes', { count: 5 }) },
  { name: 'a rate limit that asks for an hour', status: 429, code: 'rate_limited', headers: { 'Retry-After': '3600' }, line: t('error.rate_limited'), extra: t('accounts.wait.hour') },
  { name: 'a rate limit that gives no wait', status: 429, code: 'rate_limited', line: t('error.rate_limited') },
  { name: 'the service unavailable', status: 503, code: 'module_unavailable', line: t('error.module_unavailable') },
  { name: 'an outcome that is not known', status: 503, code: 'account_outcome_unknown', line: t('error.account_outcome_unknown') },
  { name: 'an error on our side', status: 500, code: 'internal_error', line: t('error.internal_error'), extra: 'Reference req_00000000000000000000000000000001' },
  { name: 'a code the app has no words for', status: 418, code: 'quota_gremlin_exceeded', line: t('error.unknown') },
]

describe('Add an account: every refusal is a line of words', () => {
  it.each(REFUSALS)('$name', async (r) => {
    vi.spyOn(console, 'warn').mockImplementation(() => {}) // the unknown code is logged for the developer, on purpose
    const fn = await openAccounts({ 'POST /v1/m3/accounts': () => apiError(r.code, r.status, r.details, r.headers) })
    await openAddForm()
    fillAccount()
    submitAdd()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(r.line)
    if (r.extra !== undefined) expect(alert).toHaveTextContent(r.extra)
    if (r.code === 'rate_limited' && r.headers === undefined) expect(alert.textContent).not.toMatch(/try again in/i) // no wait was given: none is invented
    expectNoInternalText()
    if (r.code.includes('_')) expect(document.body.textContent).not.toContain(r.code) // a code of one word may be a plain English word
    expect(requests(fn, 'POST', '/v1/m3/accounts')).toHaveLength(1)
    // the password is gone from the form after the answer
    expect(field(t('accounts.add.password'))).toHaveValue('')
  })

  it('control: a successful add shows none of these lines', async () => {
    await openAccounts({ 'POST /v1/m3/accounts': [201, { account: account() }] })
    await openAddForm()
    fillAccount()
    submitAdd()
    await screen.findByRole('heading', { level: 2, name: t('accounts.added.title', { address: 'ann@example.test' }) })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    for (const r of REFUSALS) expect(document.body.textContent).not.toContain(r.line)
  })

  it('a host that is not allowed is put on the host field, and the account is not added', async () => {
    await openAccounts({ 'POST /v1/m3/accounts': () => apiError('host_forbidden', 422) })
    await openAddForm()
    fillAccount()
    submitAdd()
    await waitFor(() => expect(field(t('accounts.add.host'))).toHaveAttribute('aria-invalid', 'true'))
    expect(field(t('accounts.add.host'))).toHaveAccessibleDescription(new RegExp(t('error.host_forbidden').slice(0, 30)))
    await waitFor(() => expect(field(t('accounts.add.host'))).toHaveFocus())
  })

  it('the fields the API refused are marked, each with its word', async () => {
    await openAccounts({
      'POST /v1/m3/accounts': () => apiError('validation_failed', 400, { reason: 'the account is not valid', fields: ['smtp_port:out_of_range', 'from_address:malformed', 'weird_field:required'] }),
    })
    await openAddForm()
    fillAccount()
    submitAdd()
    expect(await screen.findByText(t('error.validation_failed.account'))).toBeInTheDocument()
    expect(field(t('accounts.add.port'))).toHaveAccessibleDescription(new RegExp(t('error.field.out_of_range')))
    expect(field(t('accounts.add.fromAddress'))).toHaveAccessibleDescription(new RegExp(t('error.field.malformed')))
    expect(field(t('accounts.add.host'))).not.toHaveAttribute('aria-invalid')
    expectNoInternalText()
  })

  it('after an answer that was lost the list is read again, because the account may exist', async () => {
    const fn = await openAccounts({ 'POST /v1/m3/accounts': () => apiError('account_outcome_unknown', 503) })
    await openAddForm()
    fillAccount()
    expect(requests(fn, 'GET', '/v1/m3/accounts')).toHaveLength(1)
    submitAdd()
    await screen.findByRole('alert')
    await waitFor(() => expect(requests(fn, 'GET', '/v1/m3/accounts')).toHaveLength(2))
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('the password', () => {
  it('goes in the body of the one request and nowhere else: not in an address, a header, a log call, storage or the page', async () => {
    const logs = watchConsole()
    const storage = watchStorage()
    let refuse = false
    const fn = await openAccounts({ 'POST /v1/m3/accounts': () => (refuse ? apiError('account_duplicate', 409) : [201, { account: account() }]) })

    // an add that works, then an add that is refused
    await openAddForm()
    fillAccount()
    submitAdd()
    await screen.findByRole('heading', { level: 2, name: t('accounts.added.title', { address: 'ann@example.test' }) })
    refuse = true
    await openAddForm()
    fillAccount()
    submitAdd()
    await screen.findByRole('alert')

    const posts = requests(fn, 'POST', '/v1/m3/accounts')
    expect(posts).toHaveLength(2)
    for (const p of posts) expect(String(p.init.body)).toContain(CANARY) // control: the canary really was in the requests, so the checks below can see it
    for (const call of fn.mock.calls) {
      const [input, init] = call
      expect(String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)).not.toContain(encodeURIComponent(CANARY))
      expect(String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)).not.toContain(CANARY)
      expect(JSON.stringify(init?.headers ?? {})).not.toContain(CANARY)
    }
    expect(window.location.href).not.toContain(CANARY)
    expect(logs.seen.join('\n')).not.toContain(CANARY)
    expect(storage.seen.join('\n')).not.toContain(CANARY)
    expect(document.body.innerHTML).not.toContain(CANARY)
    expect(document.body.textContent).not.toContain(CANARY)
    logs.restore()
    storage.restore()
  })

  it('paired control: the watchers do see a password that is logged, stored or put in an address', () => {
    const logs = watchConsole()
    const storage = watchStorage()
    console.log(CANARY)
    console.error(new Error(`failed for ${CANARY}`))
    localStorage.setItem('k', CANARY)
    document.cookie = `k=${CANARY}`
    window.history.replaceState(null, '', `/accounts?pw=${CANARY}`)
    expect(logs.seen.join('\n')).toContain(CANARY)
    expect(storage.seen.join('\n')).toContain(CANARY)
    expect(window.location.href).toContain(CANARY)
    logs.restore()
    storage.restore()
  })

  it('is not kept by the page after the answer: an unrelated edit does not bring it back (nothing holds it in state)', async () => {
    await openAccounts({ 'POST /v1/m3/accounts': () => apiError('account_duplicate', 409) })
    await openAddForm()
    fillAccount()
    submitAdd()
    await screen.findByRole('alert')
    expect(field(t('accounts.add.password'))).toHaveValue('')
    type(field(t('accounts.add.fromName')), 'Another name')
    type(field(t('accounts.add.username')), 'someone')
    expect(field(t('accounts.add.password'))).toHaveValue('')
    expect(field(t('accounts.add.host'))).toHaveValue('smtp.example.test') // control: what is not a secret stays, so the clearing is deliberate
  })

  it('is emptied when the form is cancelled, and a new form starts empty', async () => {
    await openAccounts()
    await openAddForm()
    fillAccount()
    const input = field(t('accounts.add.password')) as HTMLInputElement
    fireEvent.click(screen.getByRole('button', { name: t('common.cancel') }))
    expect(input.value).toBe('')
    expect(screen.queryByRole('heading', { level: 2, name: t('accounts.add.title') })).not.toBeInTheDocument()
    await openAddForm()
    expect(field(t('accounts.add.password'))).toHaveValue('')
  })

  it('the form and the bulk panel close with Cancel and Close, and focus goes back to the button that opened them', async () => {
    await openAccounts()
    const addButton = within(main()).getByRole('button', { name: t('accounts.add.open') })
    const bulkButton = within(main()).getByRole('button', { name: t('accounts.bulk.open') })
    expect(addButton).toHaveAttribute('aria-expanded', 'false')
    addButton.focus()
    fireEvent.click(addButton)
    await screen.findByRole('heading', { level: 2, name: t('accounts.add.title') })
    expect(addButton).toHaveAttribute('aria-expanded', 'true')
    await waitFor(() => expect(field(t('accounts.add.host'))).toHaveFocus()) // the first field takes focus, so typing can begin
    fireEvent.click(screen.getByRole('button', { name: t('common.cancel') }))
    expect(screen.queryByRole('heading', { level: 2, name: t('accounts.add.title') })).not.toBeInTheDocument()
    await waitFor(() => expect(addButton).toHaveFocus())
    bulkButton.focus()
    fireEvent.click(bulkButton)
    await screen.findByRole('heading', { level: 2, name: t('accounts.bulk.title') })
    fireEvent.click(screen.getByRole('button', { name: t('common.close') }))
    expect(screen.queryByRole('heading', { level: 2, name: t('accounts.bulk.title') })).not.toBeInTheDocument()
    await waitFor(() => expect(bulkButton).toHaveFocus())
  })

  it('the form posts to nowhere if it is ever submitted without the handler: it is a POST form, never a GET with the password in the address', async () => {
    await openAccounts()
    await openAddForm()
    const form = field(t('accounts.add.password')).closest('form') as HTMLFormElement
    expect(form.getAttribute('method')).toBe('post')
    expect(form.getAttribute('action')).toBeNull()
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Test', () => {
  const testPath = `/v1/m3/accounts/${ID}/test`
  const withAccount = (extra: Record<string, unknown> = {}) => ({ 'GET /v1/m3/accounts': { accounts: [account()], next_cursor: null }, ...extra })
  const clickTest = async () => {
    const row = await screen.findByTestId(`account-row-${ID}`)
    const button = within(row).getByRole('button', { name: t('accounts.test.buttonFor', { address: 'ann@example.test' }) })
    button.focus()
    fireEvent.click(button)
    return button
  }

  it('shows that the connection works, and what the check of the From address says', async () => {
    const fn = await openAccounts(withAccount({ [`POST ${testPath}`]: { result: { ok: true, stage: 'done', from_gate: 'aligned' } } }))
    await clickTest()
    const dialog = await screen.findByRole('dialog', { name: t('accounts.test.title', { address: 'ann@example.test' }) })
    expect(await within(dialog).findByText(t('accounts.stage.done'))).toBeInTheDocument()
    expect(within(dialog).getByText(t('accounts.test.works'))).toBeInTheDocument()
    expect(within(dialog).getByText(t('accounts.fromGate.aligned'))).toBeInTheDocument()
    expect(within(dialog).queryByRole('checkbox')).not.toBeInTheDocument() // nothing to confirm
    const sent = requests(fn, 'POST', testPath)
    expect(sent).toHaveLength(1)
    expect(bodyOf(sent[0]!)['acknowledge']).not.toBe(true)
    expectNoInternalText()
  })

  it.each([
    ['connect', 'accounts.stage.connect'],
    ['host', 'accounts.stage.host'],
    ['tls', 'accounts.stage.tls'],
    ['login', 'accounts.stage.login'],
    ['protocol', 'accounts.stage.protocol'],
    ['unavailable', 'accounts.stage.unavailable'],
  ] as const)('says in words how far a connection got when it stopped at "%s"', async (stage, key) => {
    await openAccounts(withAccount({ [`POST ${testPath}`]: { result: { ok: false, stage, from_gate: 'unverified' } } }))
    await clickTest()
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText(t(key))).toBeInTheDocument()
    expect(within(dialog).getByText(t('accounts.test.fails'))).toBeInTheDocument()
    expect(within(dialog).queryByText(t('accounts.stage.done'))).not.toBeInTheDocument() // control: a failed stage is not read as success
    expectNoInternalText()
  })

  it('asks for a warning to be confirmed, and tests again with the confirmation only after the box is ticked', async () => {
    let calls = 0
    const fn = await openAccounts(
      withAccount({
        [`POST ${testPath}`]: () => {
          calls++
          return { result: { ok: true, stage: 'done', from_gate: 'unaligned_warned' } }
        },
      }),
    )
    await clickTest()
    const dialog = await screen.findByRole('dialog')
    const box = await within(dialog).findByRole('checkbox', { name: t('accounts.ack.checkboxUse') })
    const again = within(dialog).getByRole('button', { name: t('accounts.test.again') })
    expect(again).toBeDisabled() // control: no confirmation without the box
    fireEvent.click(again)
    expect(calls).toBe(1)
    fireEvent.click(box)
    expect(again).toBeEnabled()
    fireEvent.click(again)
    await waitFor(() => expect(calls).toBe(2))
    const sent = requests(fn, 'POST', testPath)
    expect(bodyOf(sent[0]!)['acknowledge']).not.toBe(true)
    expect(bodyOf(sent[1]!)['acknowledge']).toBe(true)
    await within(screen.getByRole('dialog')).findByText(t('accounts.test.confirmed'))
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument() // confirmed once, not asked again
  })

  it('shows a rate limit with the wait, and a failure with its line', async () => {
    let limited = true
    await openAccounts(withAccount({ [`POST ${testPath}`]: () => (limited ? apiError('rate_limited', 429, undefined, { 'Retry-After': '90' }) : apiError('module_unavailable', 503)) }))
    await clickTest()
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText(t('error.rate_limited'))).toBeInTheDocument()
    expect(within(dialog).getByText(t('accounts.wait.minutes', { count: 2 }))).toBeInTheDocument()
    expectNoInternalText()
    fireEvent.click(within(dialog).getByRole('button', { name: t('accounts.test.done') }))
    limited = false
    await clickTest()
    expect(await screen.findByText(t('error.module_unavailable'))).toBeInTheDocument()
    expect(screen.queryByText(t('error.rate_limited'))).not.toBeInTheDocument()
  })

  it('is closed with Escape, and focus goes back to the button that opened it', async () => {
    await openAccounts(withAccount({ [`POST ${testPath}`]: { result: { ok: true, stage: 'done', from_gate: 'aligned' } } }))
    const button = await clickTest()
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByText(t('accounts.stage.done'))
    fireEvent.keyDown(dialog, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await waitFor(() => expect(button).toHaveFocus())
  })

  it('reads the list again after a test, because the account may have changed', async () => {
    const fn = await openAccounts(withAccount({ [`POST ${testPath}`]: { result: { ok: true, stage: 'done', from_gate: 'aligned' } } }))
    await clickTest()
    await screen.findByText(t('accounts.stage.done'))
    await waitFor(() => expect(requests(fn, 'GET', '/v1/m3/accounts').length).toBeGreaterThanOrEqual(2))
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Add many', () => {
  const header = () => t('accounts.bulk.header')
  const lines = (...rows: string[]) => [header(), ...rows].join('\n')
  const openBulk = async () => {
    fireEvent.click(within(main()).getByRole('button', { name: t('accounts.bulk.open') }))
    await screen.findByRole('heading', { level: 2, name: t('accounts.bulk.title') })
  }
  const textarea = () => screen.getByLabelText(new RegExp(`^${t('accounts.bulk.lines')}`)) as HTMLTextAreaElement
  const submitBulk = () => fireEvent.click(screen.getByRole('button', { name: t('accounts.bulk.submit') }))
  const blobText = (init: RequestInit): Promise<string> =>
    new Promise((resolve, reject) => {
      const reader = new FileReader()
      reader.onload = () => resolve(String(reader.result))
      reader.onerror = () => reject(reader.error)
      reader.readAsText(init.body as Blob)
    })

  it('sends the pasted lines as a CSV and shows, for each line, its number and a word, with a summary and no password', async () => {
    const logs = watchConsole()
    const fn = await openAccounts({
      'POST /v1/m3/accounts/bulk': {
        results: [
          { line: 2, ok: true, code: 'created' },
          { line: 3, ok: false, code: 'account_duplicate' },
          { line: 4, ok: false, code: 'host_forbidden' },
          { line: 5, ok: false, code: 'validation_failed' },
          { line: 6, ok: true, code: 'created_unverified' },
          { line: 7, ok: false, code: 'from_gate_ack_required' },
          { line: 8, ok: false, code: 'a_code_from_the_future' },
        ],
      },
    })
    await openBulk()
    expect(textarea().value).toBe(`${header()}\n`) // the header is filled in, in the screen's own words
    type(textarea(), lines(`smtp.example.test,587,ann,${CANARY}-1,ann@example.test,Ann,`, `smtp.example.test,587,bob,${CANARY}-2,bob@example.test,,`))
    submitBulk()
    const table = await screen.findByRole('region', { name: t('accounts.bulk.caption') })

    const sent = requests(fn, 'POST', '/v1/m3/accounts/bulk')
    expect(sent).toHaveLength(1)
    expect((sent[0]!.init.headers as Record<string, string>)['Content-Type']).toBe('text/csv')
    const sentText = await blobText(sent[0]!.init)
    expect(sentText).toContain(`${CANARY}-1`) // control: the passwords did go to the API
    expect(sentText.split('\n')[0]).toBe(BULK_HEADER) // with the first line in the API's words ...
    expect(sentText.split('\n')).toHaveLength(3) // ... and every other line as the user wrote it, so the numbers of the lines are the user's
    expect(sentText).not.toContain(header())

    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(7)
    const word = (row: HTMLElement) => within(row).getAllByRole('cell')[0]?.textContent
    expect(rows.map((r) => within(r).getByRole('rowheader').textContent)).toEqual(['2', '3', '4', '5', '6', '7', '8'])
    expect(rows.map(word)).toEqual([
      t('accounts.bulk.created'),
      t('accounts.bulk.duplicate'),
      t('accounts.bulk.hostForbidden'),
      t('accounts.bulk.invalid'),
      t('accounts.bulk.createdUnverified'),
      t('accounts.bulk.ackRequired'),
      t('accounts.bulk.failed'), // a code of the future reads as not added, never as the code
    ])
    expect(rows[1]).toHaveTextContent(t('error.account_duplicate'))
    expect(rows[2]).toHaveTextContent(t('error.host_forbidden'))
    expect(screen.getByText(t('accounts.bulk.summary', { added: 2, failed: 5 }))).toBeInTheDocument()
    expect(screen.getByText(t('accounts.bulk.summaryCheck'))).toBeInTheDocument()

    // never a password in the page, a log, and the pasted lines are emptied
    expect(document.body.textContent).not.toContain(CANARY)
    expect(textarea().value).toBe(`${header()}\n`)
    expect(logs.seen.join('\n')).not.toContain(CANARY)
    expectNoInternalText()
    logs.restore()
  })

  it('sends a chosen CSV file with its first line in the words of the API and the rest as it is, and uses it instead of the pasted lines', async () => {
    const fn = await openAccounts({ 'POST /v1/m3/accounts/bulk': { results: [{ line: 2, ok: true, code: 'created' }] } })
    await openBulk()
    const file = new File([lines('smtp.example.test,587,ann,from-file,ann@example.test,,')], 'accounts.csv', { type: 'text/csv' })
    type(textarea(), lines('smtp.example.test,587,zed,from-text,zed@example.test,,'))
    fireEvent.change(screen.getByLabelText(new RegExp(`^${t('accounts.bulk.file')}`)), { target: { files: [file] } })
    submitBulk()
    await screen.findByText(t('accounts.bulk.summary', { added: 1, failed: 0 }))
    const text = await blobText(requests(fn, 'POST', '/v1/m3/accounts/bulk')[0]!.init)
    expect(text).toBe(`${BULK_HEADER}\nsmtp.example.test,587,ann,from-file,ann@example.test,,`)
    expect(text).not.toContain('from-text')
  })

  it('control: a file that already names its columns the way the API does is sent exactly as it is', async () => {
    const fn = await openAccounts({ 'POST /v1/m3/accounts/bulk': { results: [{ line: 2, ok: true, code: 'created' }] } })
    await openBulk()
    const own = `${BULK_HEADER}\r\nsmtp.example.test,587,ann,pw,ann@example.test,,\r\n`
    fireEvent.change(screen.getByLabelText(new RegExp(`^${t('accounts.bulk.file')}`)), { target: { files: [new File([own], 'own.csv', { type: 'text/csv' })] } })
    submitBulk()
    await screen.findByText(t('accounts.bulk.summary', { added: 1, failed: 0 }))
    expect(await blobText(requests(fn, 'POST', '/v1/m3/accounts/bulk')[0]!.init)).toBe(own)
  })

  it('refuses to send nothing: the header alone is not a list; control: a list with one account is sent', async () => {
    const fn = await openAccounts({ 'POST /v1/m3/accounts/bulk': { results: [{ line: 2, ok: true, code: 'created' }] } })
    await openBulk()
    submitBulk()
    expect(await screen.findByRole('alert')).toHaveTextContent(t('accounts.bulk.nothing'))
    expect(requests(fn, 'POST', '/v1/m3/accounts/bulk')).toHaveLength(0)
    type(textarea(), lines('smtp.example.test,587,ann,pw,ann@example.test,,'))
    submitBulk()
    await screen.findByText(t('accounts.bulk.summary', { added: 1, failed: 0 }))
    expect(requests(fn, 'POST', '/v1/m3/accounts/bulk')).toHaveLength(1)
  })

  it('names the line and the fields when the API refuses the whole batch, keeps the text, and never quotes a value', async () => {
    await openAccounts({
      'POST /v1/m3/accounts/bulk': () => apiError('validation_failed', 400, { reason: 'a row of the batch is not valid', line: 3, fields: ['smtp_secret:required', 'smtp_port:out_of_range'] }),
    })
    await openBulk()
    const text = lines(`smtp.example.test,587,ann,${CANARY},ann@example.test,,`, 'smtp.example.test,99999,bob,,bob@example.test,,')
    type(textarea(), text)
    submitBulk()
    expect(await screen.findByRole('alert')).toHaveTextContent(t('accounts.bulk.badLine', { line: 3, fields: `${t('accounts.field.password')}, ${t('accounts.field.port')}` }))
    expect(textarea().value).toBe(text) // the list is not lost to a typing mistake
    expect(screen.queryByRole('region', { name: t('accounts.bulk.caption') })).not.toBeInTheDocument()
    expectNoInternalText()
  })

  it('says in words that a batch could not be read when the API names no line (too few values on a line, a column it does not know), and keeps the text', async () => {
    await openAccounts({ 'POST /v1/m3/accounts/bulk': () => apiError('validation_failed', 400, { reason: 'line 2 is not valid CSV or has a different number of columns than the header' }) })
    await openBulk()
    const text = lines('smtp.example.test,587,ann')
    type(textarea(), text)
    submitBulk()
    expect(await screen.findByRole('alert')).toHaveTextContent(t('accounts.bulk.badFile'))
    expect(screen.getByRole('alert').textContent).not.toContain('columns than the header') // the developer's sentence stays out
    expect(textarea().value).toBe(text)
    expectNoInternalText()
  })

  it.each([
    ['rate_limited', 429, t('error.rate_limited')],
    ['module_unavailable', 503, t('error.module_unavailable')],
    ['no_entitlement', 403, t('error.no_entitlement')],
    ['account_outcome_unknown', 503, t('error.account_outcome_unknown')],
  ])('shows a refusal of the whole batch (%s) as its line', async (code, status, line) => {
    const fn = await openAccounts({ 'POST /v1/m3/accounts/bulk': () => apiError(code, status) })
    await openBulk()
    type(textarea(), lines('smtp.example.test,587,ann,pw,ann@example.test,,'))
    submitBulk()
    expect(await screen.findByRole('alert')).toHaveTextContent(line)
    expectNoInternalText()
    if (code === 'account_outcome_unknown') await waitFor(() => expect(requests(fn, 'GET', '/v1/m3/accounts').length).toBeGreaterThanOrEqual(2))
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Remove', () => {
  const removePath = `/v1/m3/accounts/${ID}/remove`
  const withAccount = (extra: Record<string, unknown> = {}) => ({ 'GET /v1/m3/accounts': { accounts: [account()], next_cursor: null }, ...extra })
  const clickRemove = async () => {
    const row = await screen.findByTestId(`account-row-${ID}`)
    const button = within(row).getByRole('button', { name: t('accounts.remove.buttonFor', { address: 'ann@example.test' }) })
    button.focus()
    fireEvent.click(button)
    return button
  }

  it('asks first and says plainly what happens, and does nothing until it is confirmed', async () => {
    const fn = await openAccounts(withAccount({ [`POST ${removePath}`]: { removed: true } }))
    await clickRemove()
    const dialog = await screen.findByRole('alertdialog', { name: t('accounts.remove.title', { address: 'ann@example.test' }) })
    expect(within(dialog).getByText(t('accounts.remove.body'))).toBeInTheDocument()
    expect(requests(fn, 'POST', removePath)).toHaveLength(0)
    await waitFor(() => expect(within(dialog).getByRole('button', { name: t('common.cancel') })).toHaveFocus()) // the safe button has focus: Enter does not remove by accident
    fireEvent.click(within(dialog).getByRole('button', { name: t('accounts.remove.confirm') }))
    await waitFor(() => expect(requests(fn, 'POST', removePath)).toHaveLength(1))
    expect(await screen.findByText(t('accounts.remove.done', { address: 'ann@example.test' }))).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    expect(requests(fn, 'GET', '/v1/m3/accounts').length).toBeGreaterThanOrEqual(2) // the list is read again
  })

  it('control: Cancel and Escape remove nothing, and focus goes back to the Remove button', async () => {
    const fn = await openAccounts(withAccount({ [`POST ${removePath}`]: { removed: true } }))
    let button = await clickRemove()
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('common.cancel') }))
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    await waitFor(() => expect(button).toHaveFocus())
    button = await clickRemove()
    fireEvent.keyDown(await screen.findByRole('alertdialog'), { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    await waitFor(() => expect(button).toHaveFocus())
    expect(requests(fn, 'POST', removePath)).toHaveLength(0)
  })

  it('says it in words when the account is already gone, and reads the list again', async () => {
    const fn = await openAccounts(withAccount({ [`POST ${removePath}`]: () => apiError('not_found', 404) }))
    await clickRemove()
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('accounts.remove.confirm') }))
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.not_found'))
    await waitFor(() => expect(requests(fn, 'GET', '/v1/m3/accounts').length).toBeGreaterThanOrEqual(2))
    expectNoInternalText()
  })

  it('says an outcome that is not known as the line that tells the user to look at the list', async () => {
    await openAccounts(withAccount({ [`POST ${removePath}`]: () => apiError('account_outcome_unknown', 503) }))
    await clickRemove()
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('accounts.remove.confirm') }))
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.account_outcome_unknown'))
  })
})
