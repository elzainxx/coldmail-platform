import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { reportApiAnswered, reportNetworkFailure } from '../api/connectivity.ts'
import { catalogue, t } from '../copy/index.ts'
import { matchRoute } from '../router.tsx'
import { routes, routesByArea } from '../routes.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../test/helpers.tsx'
import { ErrorBoundary } from './ErrorBoundary.tsx'
import { NAV_ITEMS } from './navItems.ts'
import { OfflineBanner } from './OfflineBanner.tsx'
import { SkipLink } from './SkipLink.tsx'
import { applyTheme, chooseTheme, currentTheme, initTheme, storedTheme } from './theme.ts'

beforeEach(() => {
  resetForTest()
  localStorage.clear()
  applyTheme(null)
})
afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.clear()
  applyTheme(null)
  resetForTest()
})

describe('the navigation', () => {
  it('lists the seven areas, in order, by their words', async () => {
    await signInForTest()
    mockApi({ 'GET /v1/me': { email: 'ann@example.test' } })
    renderApp('/no/such/page')
    const nav = screen.getByRole('navigation', { name: 'Main navigation' })
    const links = within(nav).getAllByRole('link')
    expect(links.map((l) => l.textContent)).toEqual(['Dashboard', 'Jobs', 'New verify job', 'New send job', 'SMTP accounts', 'Suppressions', 'Settings'])
    expect(links.map((l) => l.getAttribute('href'))).toEqual(['/', '/jobs', '/new/verify', '/new/send', '/accounts', '/suppressions', '/settings'])
  })

  it('marks the current page with aria-current, and only that one', async () => {
    await signInForTest()
    mockApi({ 'GET /v1/me': { email: 'ann@example.test' } })
    renderApp('/jobs/42') // not found today: the area is not built, and the nav must still mark /jobs as the area
    const nav = screen.getByRole('navigation', { name: 'Main navigation' })
    expect(within(nav).getByRole('link', { name: 'Jobs' })).toHaveAttribute('aria-current', 'page')
    expect(within(nav).getAllByRole('link').filter((l) => l.getAttribute('aria-current') === 'page')).toHaveLength(1)
    expect(within(nav).getByRole('link', { name: 'Dashboard' })).not.toHaveAttribute('aria-current') // "/" is current only on "/"
  })

  it('a signed-out visitor sees no navigation', () => {
    renderApp('/sign-in')
    expect(screen.queryByRole('navigation', { name: 'Main navigation' })).not.toBeInTheDocument()
  })

  it('every entry is in the catalogue, and there is one for each of the six areas', () => {
    expect(NAV_ITEMS).toHaveLength(7)
    for (const item of NAV_ITEMS) expect(item.label in catalogue, item.id).toBe(true)
    expect(new Set(NAV_ITEMS.map((i) => i.area))).toEqual(new Set(['dashboard', 'jobs', 'newjob', 'accounts', 'suppressions', 'settings']))
    expect(new Set(NAV_ITEMS.map((i) => i.to)).size).toBe(7)
  })

  it('has a page behind each entry as soon as its area has any page (an area that is still empty is reported, not failed)', () => {
    const missing: string[] = []
    let checked = 0
    const empty: string[] = []
    for (const item of NAV_ITEMS) {
      if (routesByArea[item.area].length === 0) {
        empty.push(item.area)
        continue
      }
      checked++
      if (matchRoute(routes, item.to) === null) missing.push(`${item.id} -> ${item.to}`)
    }
    expect(missing, 'a navigation entry whose area has pages but none at the entry\'s path').toEqual([])
    // not a pass by omission: what was not checked is named
    expect(checked + empty.length).toBe(7)
  })

  it('paired control: the check does find an entry with no page', () => {
    const fake = [{ path: '/jobs', title: 'common.nav.jobs' as const, element: () => null, protected: true }]
    expect(matchRoute(fake, '/jobs')).not.toBeNull()
    expect(matchRoute(fake, '/new/verify')).toBeNull()
  })
})

describe('the route registry', () => {
  it('has no path twice, every title a key of the catalogue, and every route one of the three kinds', () => {
    const seen = new Set<string>()
    for (const r of routes) {
      expect(seen.has(r.path), `${r.path} is registered twice`).toBe(false)
      seen.add(r.path)
      expect(r.title in catalogue, `${r.path}: title ${r.title}`).toBe(true)
      expect(typeof r.protected, r.path).toBe('boolean')
      expect(r.path.startsWith('/'), r.path).toBe(true)
      // a page is for the signed-in, or for the signed-out, or for a password link: never two of these
      const kinds = [r.protected, r.guestOnly === true, r.passwordSetup === true].filter(Boolean)
      expect(kinds.length, `${r.path} is more than one kind of page`).toBeLessThanOrEqual(1)
    }
    expect(routes.length).toBeGreaterThanOrEqual(4) // the sign-in pages at least
  })

  it('has the four sign-in pages, none of them protected, and the registry only joins the six area files and mine', () => {
    expect(routesByArea.auth.map((r) => r.path)).toEqual(['/sign-in', '/forgot-password', '/accept-invite', '/reset-password'])
    expect(routesByArea.auth.every((r) => !r.protected)).toBe(true)
    expect(Object.keys(routesByArea).sort()).toEqual(['accounts', 'auth', 'dashboard', 'jobs', 'newjob', 'settings', 'suppressions'])
  })

  it('every page that is not one of the sign-in pages is protected (an area agent cannot leave a page of user data open by omission)', () => {
    const open = Object.entries(routesByArea)
      .filter(([area]) => area !== 'auth')
      .flatMap(([, list]) => list)
      .filter((r) => !r.protected)
    expect(open.map((r) => r.path), 'a page of an area that is not protected').toEqual([])
  })
})

describe('the theme', () => {
  it('follows the system until the user chooses, then keeps the choice on the page and for the next visit', () => {
    expect(storedTheme()).toBeNull()
    expect(document.documentElement.getAttribute('data-theme')).toBeNull()
    chooseTheme('dark')
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
    expect(localStorage.getItem('coldmail-theme')).toBe('dark')
    expect(currentTheme()).toBe('dark')
    // the next visit
    applyTheme(null)
    initTheme()
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
  })

  it('ignores a stored value that is not a theme', () => {
    localStorage.setItem('coldmail-theme', 'purple')
    expect(storedTheme()).toBeNull()
    initTheme()
    expect(document.documentElement.getAttribute('data-theme')).toBeNull()
  })

  it('the toggle in the frame names what it will do, and switches', async () => {
    renderApp('/sign-in')
    const toLight = screen.queryByRole('button', { name: 'Switch to the light theme' })
    expect(toLight).not.toBeInTheDocument() // the page starts light
    fireEvent.click(screen.getByRole('button', { name: 'Switch to the dark theme' }))
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark')
    fireEvent.click(screen.getByRole('button', { name: 'Switch to the light theme' }))
    expect(document.documentElement).toHaveAttribute('data-theme', 'light')
    expect(localStorage.getItem('coldmail-theme')).toBe('light')
  })

  it('stores nothing but the theme', () => {
    renderApp('/sign-in')
    fireEvent.click(screen.getByRole('button', { name: 'Switch to the dark theme' }))
    expect(localStorage.length).toBe(1)
    expect(sessionStorage.length).toBe(0)
  })
})

describe('the top bar', () => {
  it('shows who is signed in once the service says so, with the sign out button', async () => {
    await signInForTest()
    mockApi({ 'GET /v1/me': { email: 'ann@example.test' } })
    renderApp('/no/such/page')
    expect(await screen.findByText('Signed in as ann@example.test')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument()
  })

  it('works without the route (a service that does not have /v1/me yet): no name, no error, no crash', async () => {
    await signInForTest()
    mockApi({})
    renderApp('/no/such/page')
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument()
    await act(async () => {
      await Promise.resolve()
    })
    expect(screen.queryByText(/Signed in as/)).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('has a menu button that opens the navigation on a small screen, and says it is open', async () => {
    await signInForTest()
    mockApi({})
    renderApp('/no/such/page')
    const menu = screen.getByRole('button', { name: 'Open the menu' })
    expect(menu).toHaveAttribute('aria-expanded', 'false')
    expect(menu).toHaveAttribute('aria-controls', 'site-nav')
    fireEvent.click(menu)
    expect(screen.getByRole('button', { name: 'Close the menu' })).toHaveAttribute('aria-expanded', 'true')
    expect(document.getElementById('site-nav')).toHaveClass('block')
  })

  it('signing out goes to sign in and tells the service', async () => {
    const gotrue = await signInForTest()
    mockApi({})
    renderApp('/no/such/page')
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument()
    expect(gotrue.signOut).toHaveBeenCalledTimes(1)
  })
})

describe('the offline banner', () => {
  it('has its live region in the page from the start, empty', () => {
    render(<OfflineBanner />)
    const region = screen.getByRole('status')
    expect(region).toHaveAttribute('aria-live', 'polite')
    expect(region).toBeEmptyDOMElement()
  })

  it('says offline, says unavailable, and goes away when the service answers', () => {
    render(<OfflineBanner />)
    act(() => reportNetworkFailure())
    expect(screen.getByRole('status')).toHaveTextContent(t('common.banner.offline'))
    act(() => reportApiAnswered(503))
    expect(screen.getByRole('status')).toHaveTextContent(t('common.banner.unavailable'))
    act(() => reportApiAnswered(200))
    expect(screen.getByRole('status')).toBeEmptyDOMElement()
  })
})

describe('the error boundary', () => {
  function Boom({ fail }: { fail: boolean }) {
    if (fail) throw new Error('INTERNAL DETAIL THAT MUST NOT BE SHOWN')
    return <p>fine</p>
  }

  it('shows a calm message, never the error, and a way out', () => {
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {})
    render(
      <ErrorBoundary>
        <Boom fail />
      </ErrorBoundary>,
    )
    expect(screen.getByRole('heading', { level: 1, name: 'This page ran into a problem' })).toBeInTheDocument()
    expect(screen.queryByText(/INTERNAL DETAIL/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Reload the page' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to the dashboard' })).toHaveAttribute('href', '/')
    expect(quiet).toHaveBeenCalled() // the developer's console has it
  })

  it('paired control: a page that does not throw is shown as it is', () => {
    render(
      <ErrorBoundary>
        <Boom fail={false} />
      </ErrorBoundary>,
    )
    expect(screen.getByText('fine')).toBeInTheDocument()
    expect(screen.queryByRole('heading')).not.toBeInTheDocument()
  })

  it('lets the next route try again (the key changes)', () => {
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {})
    const { rerender } = render(
      <ErrorBoundary resetKey="/a">
        <Boom fail />
      </ErrorBoundary>,
    )
    expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
    rerender(
      <ErrorBoundary resetKey="/b">
        <Boom fail={false} />
      </ErrorBoundary>,
    )
    expect(screen.getByText('fine')).toBeInTheDocument()
    quiet.mockRestore()
  })
})

describe('the skip link', () => {
  it('is a link to the main area that moves focus there without touching the address', () => {
    window.history.replaceState(null, '', '/x')
    render(
      <>
        <SkipLink />
        <main id="main" tabIndex={-1}>
          page
        </main>
      </>,
    )
    fireEvent.click(screen.getByRole('link', { name: 'Skip to the main content' }))
    expect(document.getElementById('main')).toHaveFocus()
    expect(window.location.pathname + window.location.hash).toBe('/x')
  })
})
