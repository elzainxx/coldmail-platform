// The settings screen: the vendor opt-out with its disclosure (the round trip and its error path), the address of the account, changing the password (the same rule
// as the sign-in pages, every refusal of the sign-in service in words, the password nowhere but in the one call) and signing out. Every property has a paired control.
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthError, type AuthErrorKind, type GoTrueClient } from '../../auth/gotrue.ts'
import { PASSWORD_MAX_BYTES, PASSWORD_MIN_LENGTH } from '../../auth/password.ts'
import { session } from '../../auth/session.ts'
import { t } from '../../copy/index.ts'
import { fakeGoTrue, jsonResponse, mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'

const LEAK = 'LEAK-the-server-sentence'
const CANARY = 'CANARY-pw-7Zq!x9-long'
const EMAIL = 'person@example.test'

type Fetch = ReturnType<typeof mockApi>

function requests(fn: Fetch, method: string, path: string) {
  return fn.mock.calls
    .map(([input, init]) => ({ url: new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, window.location.origin), init: init ?? {} }))
    .filter((c) => (c.init.method ?? 'GET').toUpperCase() === method && c.url.pathname === path)
}

function apiError(code: string, status: number): Response {
  return jsonResponse({ error: { code, message: LEAK, request_id: 'req_00000000000000000000000000000001' } }, status)
}

interface Opened {
  fn: Fetch
  gotrue: GoTrueClient
}

async function openSettings(routes: Record<string, unknown> = {}, gotrueOverrides: Partial<GoTrueClient> = {}): Promise<Opened> {
  const gotrue = await signInForTest(fakeGoTrue(gotrueOverrides))
  const fn = mockApi({
    'GET /v1/settings': { vendor_fallback_opt_out: false },
    'PUT /v1/settings': (_u: URL, init: RequestInit) => JSON.parse(String(init.body)),
    'GET /v1/me': { email: EMAIL },
    ...routes,
  })
  renderApp('/settings')
  await screen.findByRole('heading', { level: 1, name: t('settings.title') })
  return { fn, gotrue }
}

const main = () => screen.getByRole('main')
const toggle = () => screen.findByRole<HTMLInputElement>('checkbox', { name: t('settings.vendor.label') })
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } })
const newPassword = () => screen.getByLabelText(new RegExp(`^${t('auth.password.new')}`)) as HTMLInputElement
const confirmPassword = () => screen.getByLabelText(new RegExp(`^${t('auth.password.confirm')}`)) as HTMLInputElement
const submitPassword = () => fireEvent.click(screen.getByRole('button', { name: t('settings.password.submit') }))
const fillPassword = (password: string, confirmation = password) => {
  type(newPassword(), password)
  type(confirmPassword(), confirmation)
}

function expectNoInternalText() {
  const text = document.body.textContent ?? ''
  expect(text).not.toMatch(/\b[a-z]+_[a-z0-9_]+\b/)
  expect(text).not.toContain(LEAK)
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

describe('the vendor opt-out', () => {
  it.each([false, true])('shows what the service holds (%s), with its label and the disclosure that says plainly what it changes', async (held) => {
    await openSettings({ 'GET /v1/settings': { vendor_fallback_opt_out: held } })
    const box = await toggle()
    expect(box.checked).toBe(held)
    // the disclosure is read with the label, and says what the setting changes in both directions
    expect(box).toHaveAccessibleDescription(t('settings.disclosure.vendor'))
    const text = t('settings.disclosure.vendor')
    expect(text).toMatch(/With this on, we never send your addresses to an outside verification service/)
    expect(text).toMatch(/more addresses undetermined/)
    expect(text).toMatch(/costs less per address/)
    expect(text).toMatch(/With it off, some addresses may be checked by an outside service, and those cost more/)
    expect(screen.getByText(t('settings.vendor.scope'))).toBeInTheDocument()
    expectNoInternalText()
  })

  it('round trip: ticking it asks the service, says so once the service has answered, and shows the new state; unticking does the same the other way', async () => {
    const { fn } = await openSettings()
    const box = await toggle()
    expect(box).not.toBeChecked()
    fireEvent.click(box)
    expect(await screen.findByText(t('settings.vendor.savedOn'))).toBeInTheDocument()
    await waitFor(() => expect(box).toBeChecked())
    let puts = requests(fn, 'PUT', '/v1/settings')
    expect(puts).toHaveLength(1)
    expect(JSON.parse(String(puts[0]!.init.body))).toEqual({ vendor_fallback_opt_out: true })

    fireEvent.click(box)
    expect(await screen.findByText(t('settings.vendor.savedOff'))).toBeInTheDocument()
    await waitFor(() => expect(box).not.toBeChecked())
    puts = requests(fn, 'PUT', '/v1/settings')
    expect(puts).toHaveLength(2)
    expect(JSON.parse(String(puts[1]!.init.body))).toEqual({ vendor_fallback_opt_out: false })
    // control: reading it once is all the page does besides the saves
    expect(requests(fn, 'GET', '/v1/settings')).toHaveLength(1)
  })

  it('shows what the service answered, not what was asked: the box follows the answer', async () => {
    await openSettings({ 'PUT /v1/settings': { vendor_fallback_opt_out: false } }) // the service says the setting is still off
    const box = await toggle()
    fireEvent.click(box)
    expect(await screen.findByText(t('settings.vendor.savedOff'))).toBeInTheDocument()
    expect(box).not.toBeChecked()
  })

  it('does not change the box before the service has answered, and ignores a second click meanwhile', async () => {
    const { fn } = await openSettings()
    const inner = fn.getMockImplementation()!
    let release: () => void = () => {}
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    fn.mockImplementation(async (input, init) => {
      if ((init?.method ?? 'GET').toUpperCase() === 'PUT') await held
      return inner(input, init)
    })
    const box = await toggle()
    fireEvent.click(box)
    expect(await screen.findByText(t('settings.vendor.saving'))).toBeInTheDocument()
    expect(box).not.toBeChecked() // asked, not yet answered
    expect(box).toHaveAttribute('aria-busy', 'true')
    fireEvent.click(box)
    expect(requests(fn, 'PUT', '/v1/settings')).toHaveLength(1)
    release()
    await waitFor(() => expect(box).toBeChecked())
    expect(screen.queryByText(t('settings.vendor.saving'))).not.toBeInTheDocument()
  })

  it('error path: a refusal leaves the box where it was, says so in words with the reference, and a later try works', async () => {
    let failing = true
    const { fn } = await openSettings({ 'PUT /v1/settings': (_u: URL, init: RequestInit) => (failing ? apiError('internal_error', 500) : JSON.parse(String(init.body))) })
    const box = await toggle()
    fireEvent.click(box)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    expect(alert).toHaveTextContent('Reference req_00000000000000000000000000000001')
    expect(screen.getByText(t('settings.vendor.failed'))).toBeInTheDocument()
    expect(box).not.toBeChecked()
    expect(screen.queryByText(t('settings.vendor.savedOn'))).not.toBeInTheDocument() // control: no word of success
    expectNoInternalText()
    failing = false
    fireEvent.click(box)
    expect(await screen.findByText(t('settings.vendor.savedOn'))).toBeInTheDocument()
    await waitFor(() => expect(box).toBeChecked())
    expect(screen.queryByText(t('settings.vendor.failed'))).not.toBeInTheDocument()
    expect(requests(fn, 'PUT', '/v1/settings')).toHaveLength(2)
  })

  it('says it could not read the setting, offers no box, and tries again on request', async () => {
    let healthy = false
    await openSettings({ 'GET /v1/settings': () => (healthy ? { vendor_fallback_opt_out: true } : apiError('module_unavailable', 503)) })
    expect(await screen.findByText(t('error.module_unavailable'))).toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: t('settings.vendor.label') })).not.toBeInTheDocument()
    expectNoInternalText()
    healthy = true
    fireEvent.click(screen.getByRole('button', { name: t('common.retry') }))
    expect(await toggle()).toBeChecked()
  })

  it('is operated by keyboard: the box takes focus and a press on it asks the service', async () => {
    const { fn } = await openSettings()
    const box = await toggle()
    box.focus()
    expect(box).toHaveFocus()
    fireEvent.click(box) // the space bar on a focused checkbox is a click
    await screen.findByText(t('settings.vendor.savedOn'))
    expect(requests(fn, 'PUT', '/v1/settings')).toHaveLength(1)
    await waitFor(() => expect(box).toHaveFocus()) // it was never disabled, so a keyboard user is not thrown out of it
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('the account', () => {
  it('shows the address of the account', async () => {
    await openSettings()
    const card = (await screen.findByRole('heading', { level: 2, name: t('settings.account.title') })).closest('section') as HTMLElement
    expect(await within(card).findByText(EMAIL)).toBeInTheDocument()
    expect(within(card).getByText(t('settings.account.email'))).toBeInTheDocument()
  })

  it('says it could not load the address, in words, and the page still works', async () => {
    await openSettings({ 'GET /v1/me': () => apiError('internal_error', 500) })
    expect(await screen.findByText(t('settings.account.emailUnknown'))).toBeInTheDocument()
    expect(await toggle()).toBeInTheDocument()
    expect(screen.getByRole('button', { name: t('settings.password.submit') })).toBeInTheDocument()
  })

  it('has one h1 and a heading for each part', async () => {
    await openSettings()
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
    expect(screen.getByRole('heading', { level: 1 })).toHaveAttribute('tabindex', '-1') // so the router can put the user at the top of the page
    const names = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)
    expect(names).toEqual([t('settings.vendor.title'), t('settings.account.title'), t('settings.signOut.title')])
  })
})

describe('changing the password', () => {
  const longEnough = 'a-phrase-that-is-long-enough'

  it('says the rule above the field, the same rule as the sign-in pages', async () => {
    await openSettings()
    expect(newPassword()).toHaveAccessibleDescription(t('auth.password.rule', { min: PASSWORD_MIN_LENGTH, max: PASSWORD_MAX_BYTES }))
    expect(newPassword()).toHaveAttribute('type', 'password')
    expect(confirmPassword()).toHaveAttribute('type', 'password')
    expect(newPassword()).toHaveAttribute('autocomplete', 'new-password')
  })

  it.each([
    ['one character short', 'x'.repeat(PASSWORD_MIN_LENGTH - 1), undefined, 'auth.password.tooShort'],
    ['one byte too long', 'x'.repeat(PASSWORD_MAX_BYTES + 1), undefined, 'auth.password.tooLong'],
    ['too long in bytes though not in characters', 'é'.repeat(PASSWORD_MAX_BYTES / 2 + 1), undefined, 'auth.password.tooLong'],
    ['not the same twice', longEnough, `${longEnough}!`, 'auth.password.mismatch'],
    ['the address of the account', EMAIL, undefined, 'auth.password.sameAsEmail'],
  ] as const)('refuses a password that is %s, in words, and calls nobody', async (_name, password, confirmation, key) => {
    const { gotrue } = await openSettings()
    await screen.findByText(EMAIL)
    fillPassword(password, confirmation ?? password)
    submitPassword()
    const line = t(key, { min: PASSWORD_MIN_LENGTH, max: PASSWORD_MAX_BYTES })
    expect(await screen.findByText(line)).toBeInTheDocument()
    expect(gotrue.updatePassword).not.toHaveBeenCalled()
    expect(newPassword().value).toBe(password) // nothing was sent, so what was typed is kept
    expectNoInternalText()
  })

  it.each([
    ['the shortest allowed', 'x'.repeat(PASSWORD_MIN_LENGTH)],
    ['the longest allowed', 'x'.repeat(PASSWORD_MAX_BYTES)],
    ['the longest allowed in multi-byte characters', 'é'.repeat(PASSWORD_MAX_BYTES / 2)],
  ])('control: %s is accepted: the sign-in client is asked once with it, the fields are emptied and the user is told', async (_name, password) => {
    const { gotrue } = await openSettings()
    await screen.findByText(EMAIL)
    fillPassword(password)
    submitPassword()
    expect(await screen.findByText(t('settings.password.done'))).toBeInTheDocument()
    expect(gotrue.updatePassword).toHaveBeenCalledTimes(1)
    expect(gotrue.updatePassword).toHaveBeenCalledWith(expect.any(String), password)
    expect(newPassword().value).toBe('')
    expect(confirmPassword().value).toBe('')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it.each<[AuthErrorKind, string]>([
    ['weak_password', t('auth.error.weak_password')],
    ['same_password', t('auth.error.same_password')],
    ['rate_limited', t('auth.error.rate_limited')],
    ['unavailable', t('auth.error.unavailable')],
    ['network', t('auth.error.network')],
    ['unknown', t('settings.password.failed')],
    ['invalid_credentials', t('settings.password.failed')],
  ])('shows a refusal of the sign-in service (%s) in words, and empties the fields', async (kind, line) => {
    const { gotrue } = await openSettings({}, { updatePassword: vi.fn(async () => Promise.reject(new AuthError(kind, 400))) })
    await screen.findByText(EMAIL)
    fillPassword(longEnough)
    submitPassword()
    expect(await screen.findByRole('alert')).toHaveTextContent(line)
    expect(gotrue.updatePassword).toHaveBeenCalledTimes(1)
    expect(newPassword().value).toBe('')
    expect(confirmPassword().value).toBe('')
    expect(screen.queryByText(t('settings.password.done'))).not.toBeInTheDocument()
    expectNoInternalText()
    expect(document.body.textContent).not.toContain(kind === 'unknown' ? 'unknown_' : kind)
  })

  it('a session that has ended sends the user to sign in again (and says why there)', async () => {
    await openSettings({}, { updatePassword: vi.fn(async () => Promise.reject(new AuthError('session_ended', 401))) })
    await screen.findByText(EMAIL)
    fillPassword(longEnough)
    submitPassword()
    await waitFor(() => expect(window.location.pathname).toBe('/sign-in'))
    expect(session.getAccessToken()).toBeNull()
  })

  it('the password is in the one call and nowhere else: no address, no log call, no storage, nothing left in the page', async () => {
    const logs = watchConsole()
    const storage = watchStorage()
    const { fn, gotrue } = await openSettings({}, { updatePassword: vi.fn(async (_token: string, password: string) => (password === `${CANARY}-refused` ? Promise.reject(new AuthError('weak_password', 422)) : undefined)) })
    await screen.findByText(EMAIL)
    fillPassword(CANARY)
    submitPassword()
    await screen.findByText(t('settings.password.done'))
    fillPassword(`${CANARY}-refused`)
    submitPassword()
    await screen.findByRole('alert')

    expect(gotrue.updatePassword).toHaveBeenCalledWith(expect.any(String), CANARY) // control: the canary really was in the call, so the checks below can see it
    for (const [input, init] of fn.mock.calls) {
      expect(String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)).not.toContain(CANARY)
      expect(JSON.stringify(init ?? {})).not.toContain(CANARY)
    }
    expect(window.location.href).not.toContain(CANARY)
    expect(logs.seen.join('\n')).not.toContain(CANARY)
    expect(storage.seen.join('\n')).not.toContain(CANARY)
    expect(document.body.innerHTML).not.toContain(CANARY)
    expect(document.body.textContent).not.toContain(CANARY)
    expect(newPassword().value).toBe('')
    logs.restore()
    storage.restore()
  })

  it('paired control: the watchers do see a password that is logged, stored or put in an address', () => {
    const logs = watchConsole()
    const storage = watchStorage()
    console.warn(`the password was ${CANARY}`)
    localStorage.setItem('k', CANARY)
    window.history.replaceState(null, '', `/settings?pw=${CANARY}`)
    expect(logs.seen.join('\n')).toContain(CANARY)
    expect(storage.seen.join('\n')).toContain(CANARY)
    expect(window.location.href).toContain(CANARY)
    logs.restore()
    storage.restore()
  })

  it('is a POST form that is never submitted to an address, and the fields are password fields', async () => {
    await openSettings()
    const form = newPassword().closest('form') as HTMLFormElement
    expect(form.getAttribute('method')).toBe('post')
    expect(form.getAttribute('action')).toBeNull()
  })

  it('works by keyboard: the form is submitted as the Enter key submits it', async () => {
    const { gotrue } = await openSettings()
    await screen.findByText(EMAIL)
    fillPassword(longEnough)
    ;(newPassword().closest('form') as HTMLFormElement).requestSubmit()
    expect(await screen.findByText(t('settings.password.done'))).toBeInTheDocument()
    expect(gotrue.updatePassword).toHaveBeenCalledTimes(1)
  })
})

describe('Sign out', () => {
  it('ends the session at once, tells the sign-in service, and shows the sign-in page', async () => {
    const { gotrue } = await openSettings()
    expect(session.getAccessToken()).not.toBeNull()
    fireEvent.click(within(main()).getByRole('button', { name: t('common.signOut') }))
    await waitFor(() => expect(window.location.pathname).toBe('/sign-in'))
    expect(session.getAccessToken()).toBeNull()
    await waitFor(() => expect(gotrue.signOut).toHaveBeenCalledTimes(1))
    expect(await screen.findByRole('heading', { level: 1, name: t('auth.signIn.title') })).toBeInTheDocument()
    // control: nothing of the user's is left on the page
    expect(screen.queryByText(EMAIL)).not.toBeInTheDocument()
  })

  it('control: opening the page does not sign anybody out', async () => {
    const { gotrue } = await openSettings()
    expect(window.location.pathname).toBe('/settings')
    expect(gotrue.signOut).not.toHaveBeenCalled()
    expect(session.getAccessToken()).not.toBeNull()
  })
})
