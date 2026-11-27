// The app as a user meets it: boot, the guard, sign in, sign out, a session that ends, an invitation link, a reset link. The real router, guard, session,
// API client and pages; only the sign-in service and fetch are stubs.
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useEffect } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App.tsx'
import { api } from './api/client.ts'
import { AuthError } from './auth/gotrue.ts'
import { session } from './auth/session.ts'
import { t } from './copy/index.ts'
import { PageHeader } from './ui/index.ts'
import { routes } from './routes.ts'
import type { RouteDef } from './router.tsx'
import { fakeGoTrue, fakeTokens, mockApi, renderApp, resetForTest, signInForTest } from './test/helpers.tsx'

// Two pages that exist only here: a protected one that counts its mounts, and a protected one that calls the API when it opens.
let secretMounts = 0
function SecretPage() {
  useEffect(() => {
    secretMounts++
  }, [])
  return <PageHeader title="Secret page" />
}
function ProbePage() {
  useEffect(() => {
    api.get('/v1/probe').catch(() => {})
  }, [])
  return <PageHeader title="Probe page" />
}
const TEST_ROUTES: RouteDef[] = [
  { path: '/__test/secret', title: 'common.nav.jobs', element: SecretPage, protected: true },
  { path: '/__test/probe', title: 'common.nav.jobs', element: ProbePage, protected: true },
]

beforeEach(() => {
  routes.push(...TEST_ROUTES)
  secretMounts = 0
  resetForTest()
})
afterEach(() => {
  for (const r of TEST_ROUTES) routes.splice(routes.indexOf(r), 1)
  vi.unstubAllGlobals()
  resetForTest()
})

const signInForm = () => ({
  email: screen.getByLabelText('Email address'),
  password: screen.getByLabelText('Password'),
  submit: screen.getByRole('button', { name: 'Sign in' }),
})

function fill(el: HTMLElement, value: string) {
  fireEvent.change(el, { target: { value } })
}

describe('App', () => {
  it('the root element carries min-h-screen (scripts/check-css.mjs looks for this utility in the built CSS)', () => {
    const { container } = renderApp('/sign-in')
    expect(container.firstElementChild).toHaveClass('min-h-screen')
  })

  it('paired control: that root class is not on every element, so the check above is about the root', () => {
    const { container } = renderApp('/sign-in')
    expect(container.querySelectorAll('.min-h-screen')).toHaveLength(1)
  })

  it('shows a spinner while /config.json loads, and nothing of the app', () => {
    window.history.replaceState(null, '', '/sign-in')
    expect(session.isConfigured()).toBe(false)
    render(<App load={() => new Promise<never>(() => {})} />)
    expect(screen.getByRole('status')).toHaveTextContent('Loading')
    expect(screen.queryByRole('heading')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument()
  })

  it('once the config has loaded the app is ready, and the sign-in client is made from it', async () => {
    window.history.replaceState(null, '', '/sign-in')
    render(<App load={async () => ({ supabase_url: 'https://auth.example.test/auth/v1', anon_key: 'anon' })} />)
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(session.isConfigured()).toBe(true)
  })

  it('a config that cannot be loaded gives a calm message and a Try again button that loads it again', async () => {
    const config = { supabase_url: 'https://auth.example.test/auth/v1', anon_key: 'anon' }
    const load = vi.fn<() => Promise<typeof config>>()
    load.mockRejectedValueOnce(new Error('network down'))
    load.mockResolvedValueOnce(config)
    window.history.replaceState(null, '', '/sign-in')
    render(<App load={load} />)
    expect(await screen.findByRole('heading', { level: 1, name: 'The app could not start' })).toBeInTheDocument()
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(load).toHaveBeenCalledTimes(2)
  })
})

describe('the guard, in the whole app', () => {
  it('signed out, a protected address shows the sign-in page, remembers where the user was going, and never creates the protected page', () => {
    renderApp('/__test/secret?tab=a')
    expect(window.location.pathname).toBe('/sign-in')
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/__test/secret?tab=a')
    expect(screen.getByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(screen.queryByText('Secret page')).not.toBeInTheDocument()
    expect(secretMounts).toBe(0)
  })

  it('paired control: signed in, the same address shows the page', async () => {
    await signInForTest()
    renderApp('/__test/secret')
    expect(screen.getByRole('heading', { level: 1, name: 'Secret page' })).toBeInTheDocument()
    expect(secretMounts).toBe(1)
  })

  it('the not-found page is shown to anybody and holds nothing', () => {
    renderApp('/no/such/page')
    expect(screen.getByRole('heading', { level: 1, name: 'Page not found' })).toBeInTheDocument()
  })
})

describe('signing in', () => {
  it('signs in, then goes where the user was going', async () => {
    const gotrue = fakeGoTrue()
    session.configure(gotrue)
    renderApp('/__test/secret')
    const f = signInForm()
    fill(f.email, ' person@example.test ')
    fill(f.password, 'correct horse battery staple')
    fireEvent.click(f.submit)
    expect(await screen.findByRole('heading', { level: 1, name: 'Secret page' })).toBeInTheDocument()
    expect(gotrue.signInWithPassword).toHaveBeenCalledWith('person@example.test', 'correct horse battery staple')
    expect(window.location.pathname).toBe('/__test/secret')
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument()
  })

  it('a next that points to another site is not followed', async () => {
    session.configure(fakeGoTrue())
    renderApp(`/sign-in?next=${encodeURIComponent('https://evil.example.test/x')}`)
    const f = signInForm()
    fill(f.email, 'person@example.test')
    fill(f.password, 'pw')
    fireEvent.click(f.submit)
    await waitFor(() => expect(session.getSnapshot().status).toBe('signed-in'))
    await waitFor(() => expect(window.location.pathname).toBe('/'))
    expect(window.location.hostname).not.toContain('evil')
  })

  it('a wrong password shows OUR line, never the service\'s, keeps the address and clears the password', async () => {
    session.configure(fakeGoTrue({ signInWithPassword: vi.fn(async () => Promise.reject(new AuthError('invalid_credentials', 400))) }))
    renderApp('/sign-in')
    const f = signInForm()
    fill(f.email, 'person@example.test')
    fill(f.password, 'wrong')
    fireEvent.click(f.submit)
    expect(await screen.findByRole('alert')).toHaveTextContent(t('auth.error.invalid_credentials'))
    expect(screen.queryByText(/invalid login credentials/i)).not.toBeInTheDocument()
    expect(f.email).toHaveValue('person@example.test')
    expect(f.password).toHaveValue('')
    expect(window.location.pathname).toBe('/sign-in')
    expect(session.getSnapshot().status).toBe('signed-out')
  })

  it('every other failure also gets one of our lines (no service, rate limit)', async () => {
    const signIn = vi.fn<(e: string, p: string) => Promise<never>>()
    signIn.mockRejectedValueOnce(new AuthError('network', 0))
    signIn.mockRejectedValueOnce(new AuthError('rate_limited', 429))
    session.configure(fakeGoTrue({ signInWithPassword: signIn }))
    renderApp('/sign-in')
    const f = signInForm()
    fill(f.email, 'a@b.test')
    fill(f.password, 'x')
    fireEvent.click(f.submit)
    expect(await screen.findByRole('alert')).toHaveTextContent(t('auth.error.network'))
    fill(screen.getByLabelText('Password'), 'y')
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(t('auth.error.rate_limited')))
  })

  it('asks for both fields before it asks the service', () => {
    const gotrue = fakeGoTrue()
    session.configure(gotrue)
    renderApp('/sign-in')
    fireEvent.click(signInForm().submit)
    expect(screen.getByRole('alert')).toHaveTextContent('Enter your email address and your password.')
    expect(gotrue.signInWithPassword).not.toHaveBeenCalled()
  })

  it('has no sign-up page and no link to one', () => {
    renderApp('/sign-in')
    for (const link of screen.queryAllByRole('link')) {
      expect(link.textContent ?? '').not.toMatch(/sign.?up|register|create (an )?account|get started/i)
    }
    expect(routes.every((r) => !/sign-?up|register|join/i.test(r.path))).toBe(true)
    expect(screen.getByRole('link', { name: 'Forgot your password?' })).toHaveAttribute('href', '/forgot-password')
  })
})

describe('signing out', () => {
  it('goes to the sign-in page with nothing of the user left, and the back button cannot bring the page back', async () => {
    const gotrue = await signInForTest()
    renderApp('/__test/secret')
    expect(secretMounts).toBe(1)
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/sign-in')
    expect(session.getAccessToken()).toBeNull()
    expect(screen.queryByText('Secret page')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Sign out' })).not.toBeInTheDocument()
    expect(gotrue.signOut).toHaveBeenCalledTimes(1)

    // the back button: the address of the protected page comes back, the guard answers again
    act(() => {
      window.history.pushState(null, '', '/__test/secret')
      window.dispatchEvent(new PopStateEvent('popstate'))
    })
    expect(window.location.pathname).toBe('/sign-in')
    expect(secretMounts).toBe(1) // not mounted a second time
    expect(screen.queryByText('Secret page')).not.toBeInTheDocument()
  })

  it('a page restored from the back-forward cache shows the sign-in page and nothing else', async () => {
    await signInForTest()
    renderApp('/__test/secret')
    expect(screen.getByRole('heading', { level: 1, name: 'Secret page' })).toBeInTheDocument()
    const e = new Event('pageshow')
    Object.defineProperty(e, 'persisted', { value: true })
    act(() => {
      window.dispatchEvent(e)
    })
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(screen.queryByText('Secret page')).not.toBeInTheDocument()
    expect(session.getAccessToken()).toBeNull()
  })
})

describe('a session that ends while the user works', () => {
  it('a 401 whose refresh is refused sends the user to the sign-in page, which says why', async () => {
    const gotrue = await signInForTest(fakeGoTrue({ refresh: vi.fn(async () => Promise.reject(new AuthError('session_ended', 400))) }))
    mockApi({ 'GET /v1/probe': [401, { error: { code: 'unauthenticated', message: 'x', request_id: 'req_1' } }] })
    renderApp('/__test/probe')
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText('Your session ended. Sign in again to continue.')).toBeInTheDocument()
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
    expect(session.getAccessToken()).toBeNull()
    expect(screen.queryByText('Probe page')).not.toBeInTheDocument()
  })

  it('paired control: when the refresh works, the request is made again with the new token and the user stays', async () => {
    const gotrue = await signInForTest()
    const auths: Array<string | undefined> = []
    mockApi({
      'GET /v1/probe': (_url: URL, init: RequestInit) => {
        auths.push((init.headers as Record<string, string>)['Authorization'])
        return auths.length === 1 ? new Response(JSON.stringify({ error: { code: 'unauthenticated', message: 'x', request_id: 'r' } }), { status: 401 }) : { ok: true }
      },
    })
    const before = session.getAccessToken()
    renderApp('/__test/probe')
    await waitFor(() => expect(auths).toHaveLength(2))
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
    expect(auths[0]).toBe(`Bearer ${before}`)
    expect(auths[1]).toBe(`Bearer ${session.getAccessToken()}`)
    expect(screen.getByRole('heading', { level: 1, name: 'Probe page' })).toBeInTheDocument()
    expect(session.getSnapshot().status).toBe('signed-in')
  })
})

describe('an invitation link', () => {
  const landing = () => ({ kind: 'session' as const, setup: 'invite' as const, tokens: fakeTokens({ refreshToken: 'invite-refresh' }) })

  function open(kind: 'invite' | 'recovery' = 'invite') {
    const gotrue = fakeGoTrue()
    session.configure(gotrue)
    window.history.replaceState(null, '', '/')
    const l = landing()
    render(<App landing={{ ...l, setup: kind }} />)
    return { gotrue, tokens: l.tokens }
  }

  it('lands on the page that chooses the password, with the rule stated, and the rest of the app closed', async () => {
    open()
    expect(await screen.findByRole('heading', { level: 1, name: 'Welcome. Choose your password' })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/accept-invite')
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/Use 12 to 72 characters/)
    expect(session.getSnapshot().status).toBe('password-required')
    // every protected address leads back here
    act(() => navigateTo('/__test/secret'))
    expect(window.location.pathname).toBe('/accept-invite')
    expect(secretMounts).toBe(0)
  })

  it('a reset link says "Choose a new password" instead', async () => {
    open('recovery')
    expect(await screen.findByRole('heading', { level: 1, name: 'Choose a new password' })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/reset-password')
  })

  it('refuses a short password and a mismatch without asking the service, with the rule in the message', async () => {
    const { gotrue } = open()
    await screen.findByRole('heading', { level: 1, name: 'Welcome. Choose your password' })
    fill(screen.getByLabelText('New password'), 'short')
    fill(screen.getByLabelText('Confirm the new password'), 'short')
    fireEvent.click(screen.getByRole('button', { name: 'Save the password and continue' }))
    expect(screen.getByRole('alert')).toHaveTextContent('The password needs at least 12 characters.')
    fill(screen.getByLabelText('New password'), 'a long enough passphrase')
    fill(screen.getByLabelText('Confirm the new password'), 'a different passphrase')
    fireEvent.click(screen.getByRole('button', { name: 'Save the password and continue' }))
    expect(screen.getByRole('alert')).toHaveTextContent('The two passwords do not match.')
    expect(gotrue.updatePassword).not.toHaveBeenCalled()
  })

  it('sets the password with the session of the link, then opens the app', async () => {
    const { gotrue, tokens } = open()
    await screen.findByRole('heading', { level: 1, name: 'Welcome. Choose your password' })
    fill(screen.getByLabelText('New password'), 'a long enough passphrase')
    fill(screen.getByLabelText('Confirm the new password'), 'a long enough passphrase')
    fireEvent.click(screen.getByRole('button', { name: 'Save the password and continue' }))
    await waitFor(() => expect(session.getSnapshot().status).toBe('signed-in'))
    expect(gotrue.updatePassword).toHaveBeenCalledWith(tokens.accessToken, 'a long enough passphrase')
    await waitFor(() => expect(window.location.pathname).toBe('/'))
  })

  it('a refusal by the service (too weak) is shown as our line, and the user can try again', async () => {
    const { gotrue } = open()
    vi.mocked(gotrue.updatePassword).mockRejectedValueOnce(new AuthError('weak_password', 422))
    await screen.findByRole('heading', { level: 1, name: 'Welcome. Choose your password' })
    fill(screen.getByLabelText('New password'), 'aaaaaaaaaaaaaaaa')
    fill(screen.getByLabelText('Confirm the new password'), 'aaaaaaaaaaaaaaaa')
    fireEvent.click(screen.getByRole('button', { name: 'Save the password and continue' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(t('auth.error.weak_password'))
    expect(session.getSnapshot().status).toBe('password-required')
    expect(screen.getByRole('button', { name: 'Save the password and continue' })).not.toHaveAttribute('aria-busy')
  })

  it('a link that did not give a session (expired, used) goes to sign in with the notice', async () => {
    session.configure(fakeGoTrue())
    window.history.replaceState(null, '', '/')
    render(<App landing={{ kind: 'error' }} />)
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText(/That link has expired or was already used./)).toBeInTheDocument()
  })

  it('paired control: opening the invite page with no link at all (a reload) goes to sign in with the same notice', () => {
    renderApp('/accept-invite')
    expect(window.location.pathname).toBe('/sign-in')
    expect(screen.getByText(/That link has expired or was already used./)).toBeInTheDocument()
  })
})

function navigateTo(path: string) {
  window.history.pushState(null, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
}

describe('forgot password', () => {
  it('says the same thing for any address, and asks the service', async () => {
    const gotrue = fakeGoTrue()
    session.configure(gotrue)
    const said: string[] = []
    for (const address of ['real@example.test', 'nobody@example.test']) {
      const { unmount } = renderApp('/forgot-password')
      fill(screen.getByLabelText('Email address'), address)
      fireEvent.click(screen.getByRole('button', { name: 'Send the reset link' }))
      const sent = await screen.findByText(t('auth.forgot.sent'))
      said.push(sent.textContent ?? '')
      expect(screen.queryByLabelText('Email address')).not.toBeInTheDocument() // the form is replaced by the confirmation
      unmount()
    }
    expect(said[0]).toBe(said[1])
    expect(said[0]).not.toContain('@')
    expect(gotrue.recover).toHaveBeenCalledWith('real@example.test')
    expect(gotrue.recover).toHaveBeenCalledWith('nobody@example.test')
  })

  it('paired control: a request that could not be made is reported, and is NOT the confirmation', async () => {
    session.configure(fakeGoTrue({ recover: vi.fn(async () => Promise.reject(new AuthError('network', 0))) }))
    renderApp('/forgot-password')
    fill(screen.getByLabelText('Email address'), 'a@b.test')
    fireEvent.click(screen.getByRole('button', { name: 'Send the reset link' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(t('auth.error.network'))
    expect(screen.queryByText(t('auth.forgot.sent'))).not.toBeInTheDocument()
  })

  it('asks for an address first', () => {
    const gotrue = fakeGoTrue()
    session.configure(gotrue)
    renderApp('/forgot-password')
    fireEvent.click(screen.getByRole('button', { name: 'Send the reset link' }))
    expect(screen.getByRole('alert')).toHaveTextContent('Enter the email address of your account.')
    expect(gotrue.recover).not.toHaveBeenCalled()
  })
})

describe('the frame around the pages', () => {
  it('the first stop for the keyboard is the skip link, and it moves focus to the page', () => {
    renderApp('/sign-in')
    const skip = screen.getByRole('link', { name: 'Skip to the main content' })
    expect(skip).toHaveAttribute('href', '#main')
    fireEvent.click(skip)
    expect(document.getElementById('main')).toHaveFocus()
    expect(window.location.hash).toBe('')
  })

  it('every page has exactly one h1', async () => {
    for (const path of ['/sign-in', '/forgot-password', '/no/such/page']) {
      const { unmount } = renderApp(path)
      expect(screen.getAllByRole('heading', { level: 1 }), path).toHaveLength(1)
      unmount()
    }
  })
})
