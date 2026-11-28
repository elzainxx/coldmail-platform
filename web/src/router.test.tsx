import { act, fireEvent, render, screen } from '@testing-library/react'
import { lazy, useEffect, type ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Link, Router, matchPath, matchRoute, navigate, pathFor, useIsActive, useRoute, useSearchParams, type LayoutProps, type RouteDef } from './router.tsx'

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  document.title = ''
})

afterEach(() => {
  window.history.replaceState(null, '', '/')
})

// ---------- pure matching ----------

describe('matchPath', () => {
  it('reads parameters, and decodes them', () => {
    expect(matchPath('/jobs/:id', '/jobs/42')).toEqual({ id: '42' })
    expect(matchPath('/jobs/:id/results', '/jobs/a%20b/results')).toEqual({ id: 'a b' })
    expect(matchPath('/', '/')).toEqual({})
  })

  it('does not match a different shape: another static segment, a shorter or a longer path', () => {
    expect(matchPath('/jobs/:id', '/jobs')).toBeNull()
    expect(matchPath('/jobs/:id', '/jobs/1/results')).toBeNull()
    expect(matchPath('/jobs', '/job')).toBeNull()
    expect(matchPath('/jobs/:id', '/accounts/1')).toBeNull()
  })

  it('ignores a trailing slash and refuses a parameter that is not valid percent-encoding', () => {
    expect(matchPath('/jobs', '/jobs/')).toEqual({})
    expect(matchPath('/jobs/:id', '/jobs/%E0%A4%A')).toBeNull()
  })
})

describe('matchRoute', () => {
  const page = () => null
  const mk = (path: string): RouteDef => ({ path, title: 'common.nav.jobs', element: page, protected: true })

  it('the most specific pattern wins (/jobs/new before /jobs/:id), and the first on a tie', () => {
    const routes = [mk('/jobs/:id'), mk('/jobs/new'), mk('/:a/:b')]
    expect(matchRoute(routes, '/jobs/new')?.route.path).toBe('/jobs/new')
    expect(matchRoute(routes, '/jobs/7')?.route.path).toBe('/jobs/:id')
    expect(matchRoute(routes, '/x/y')?.route.path).toBe('/:a/:b')
    expect(matchRoute([mk('/a/:x'), mk('/a/:y')], '/a/1')?.route.path).toBe('/a/:x')
  })

  it('is null when nothing matches', () => {
    expect(matchRoute([mk('/jobs')], '/nope')).toBeNull()
    expect(matchRoute([], '/')).toBeNull()
  })
})

describe('pathFor', () => {
  it('fills and encodes the parameters and adds a query', () => {
    expect(pathFor('/jobs/:id', { id: 'a b/c' })).toBe('/jobs/a%20b%2Fc')
    expect(pathFor('/jobs', {}, { q: 'x y', cursor: undefined, empty: '' })).toBe('/jobs?q=x+y')
    expect(pathFor('/', {})).toBe('/')
  })

  it('a missing parameter is a programming error', () => {
    expect(() => pathFor('/jobs/:id', {})).toThrow(/id/)
  })
})

// ---------- the component ----------

function Page({ name }: { name: string }) {
  const { params, pathname } = useRoute()
  return (
    <>
      <h1 tabIndex={-1}>{name}</h1>
      <p data-testid="where">
        {pathname} {JSON.stringify(params)}
      </p>
    </>
  )
}

function makeRoutes(extra: Partial<Record<string, RouteDef>> = {}): RouteDef[] {
  return [
    { path: '/', title: 'common.nav.dashboard', element: () => <Page name="Dashboard page" />, protected: false },
    { path: '/jobs', title: 'common.nav.jobs', element: () => <Page name="Jobs page" />, protected: false },
    { path: '/jobs/:id', title: 'common.nav.jobs', element: () => <Page name="Job page" />, protected: false },
    ...Object.values(extra).filter((r): r is RouteDef => r !== undefined),
  ]
}

function NotFound() {
  return <h1 tabIndex={-1}>Nothing here</h1>
}

function Layout({ children }: LayoutProps) {
  return (
    <>
      <nav aria-label="test nav">
        <Link to="/jobs">Go to jobs</Link>
        <Link to="/jobs/7">Go to job 7</Link>
        <Link to="/jobs" target="_blank">
          New window
        </Link>
        <Link to="/" replace>
          Replace home
        </Link>
      </nav>
      <main id="main" tabIndex={-1}>
        {children}
      </main>
    </>
  )
}

function renderAt(path: string, routes: RouteDef[] = makeRoutes(), guard: (route: RouteDef | null) => string | null = () => null, wrap?: (n: ReactNode) => ReactNode) {
  window.history.replaceState(null, '', path)
  const tree = <Router routes={routes} guard={guard} notFound={NotFound} layout={Layout} />
  return render(<>{wrap ? wrap(tree) : tree}</>)
}

describe('Router', () => {
  it('renders the page of the path, with its parameters', () => {
    renderAt('/jobs/42')
    expect(screen.getByRole('heading', { level: 1, name: 'Job page' })).toBeInTheDocument()
    expect(screen.getByTestId('where')).toHaveTextContent('/jobs/42 {"id":"42"}')
  })

  it('renders the not-found page for an unknown path, and the page for a known one', () => {
    const { unmount } = renderAt('/nowhere')
    expect(screen.getByRole('heading', { level: 1, name: 'Nothing here' })).toBeInTheDocument()
    unmount()
    renderAt('/jobs')
    expect(screen.getByRole('heading', { level: 1, name: 'Jobs page' })).toBeInTheDocument()
  })

  it('sets the document title from the route\'s copy key, and says so for a page that is not there', () => {
    const { unmount } = renderAt('/jobs')
    expect(document.title).toBe('Jobs - Coldmail')
    unmount()
    renderAt('/nowhere')
    expect(document.title).toBe('Page not found - Coldmail')
  })

  it('follows a Link without a reload, then the back button', () => {
    renderAt('/')
    fireEvent.click(screen.getByRole('link', { name: 'Go to job 7' }))
    expect(window.location.pathname).toBe('/jobs/7')
    expect(screen.getByRole('heading', { level: 1, name: 'Job page' })).toBeInTheDocument()
    window.history.replaceState(null, '', '/jobs') // what the back button does to the address ...
    act(() => {
      window.dispatchEvent(new PopStateEvent('popstate')) // ... and the event it fires
    })
    expect(screen.getByRole('heading', { level: 1, name: 'Jobs page' })).toBeInTheDocument()
  })

  it('leaves a modified click and a link to another window to the browser', () => {
    renderAt('/')
    const seen: boolean[] = []
    const spy = (e: Event) => {
      seen.push(e.defaultPrevented)
      e.preventDefault() // no navigation in jsdom
    }
    document.addEventListener('click', spy)
    fireEvent.click(screen.getByRole('link', { name: 'Go to jobs' }), { ctrlKey: true })
    fireEvent.click(screen.getByRole('link', { name: 'Go to jobs' }), { metaKey: true })
    fireEvent.click(screen.getByRole('link', { name: 'New window' }))
    fireEvent.click(screen.getByRole('link', { name: 'Go to jobs' }), { button: 1 })
    expect(seen).toEqual([false, false, false, false]) // the router did not take any of them
    expect(window.location.pathname).toBe('/')
    // paired control: a plain click IS taken by the router
    fireEvent.click(screen.getByRole('link', { name: 'Go to jobs' }))
    expect(seen[4]).toBe(true)
    expect(window.location.pathname).toBe('/jobs')
    document.removeEventListener('click', spy)
  })

  it('a Link can replace the history entry instead of adding one', () => {
    renderAt('/jobs')
    const before = window.history.length
    fireEvent.click(screen.getByRole('link', { name: 'Replace home' }))
    expect(window.location.pathname).toBe('/')
    expect(window.history.length).toBe(before)
  })

  it('a lazy page renders once it has loaded', async () => {
    const Lazy = lazy(async () => ({ default: () => <Page name="Lazy page" /> }))
    renderAt('/lazy', [...makeRoutes(), { path: '/lazy', title: 'common.nav.jobs', element: Lazy, protected: false }])
    expect(await screen.findByRole('heading', { level: 1, name: 'Lazy page' })).toBeInTheDocument()
  })
})

describe('navigate', () => {
  it('adds a history entry, or replaces the current one, and does nothing for the address it is already at', () => {
    window.history.replaceState(null, '', '/')
    const start = window.history.length
    navigate('/jobs')
    expect(window.history.length).toBe(start + 1)
    navigate('/jobs') // same address: no duplicate entry
    expect(window.history.length).toBe(start + 1)
    navigate('/accounts', { replace: true })
    expect(window.history.length).toBe(start + 1)
    expect(window.location.pathname).toBe('/accounts')
  })

  it('keeps the query and the hash it was given', () => {
    navigate('/jobs?status=active#top')
    expect(window.location.pathname + window.location.search + window.location.hash).toBe('/jobs?status=active#top')
  })

  it('refuses to leave the app', () => {
    expect(() => navigate('https://evil.example.test/x')).toThrow(/only paths of this app/)
    expect(() => navigate('//evil.example.test/x')).toThrow(/only paths of this app/)
    expect(window.location.hostname).not.toContain('evil')
  })
})

describe('focus and scroll on navigation', () => {
  it('does not take focus at first load', () => {
    renderAt('/jobs')
    expect(screen.getByRole('heading', { level: 1 })).not.toHaveFocus()
    expect(window.scrollTo).not.toHaveBeenCalled()
  })

  it('moves focus to the new page\'s h1 and scrolls to the top when the PATH changes', () => {
    renderAt('/')
    fireEvent.click(screen.getByRole('link', { name: 'Go to jobs' }))
    expect(screen.getByRole('heading', { level: 1, name: 'Jobs page' })).toHaveFocus()
    expect(window.scrollTo).toHaveBeenCalledWith(0, 0)
  })

  it('paired control: a change of the query alone moves neither focus nor scroll', () => {
    function Filter() {
      const [query, setSearch] = useSearchParams()
      return (
        <>
          <h1 tabIndex={-1}>Filter page</h1>
          <button type="button" onClick={() => setSearch({ q: 'abc' })}>
            Search
          </button>
          <output>{query.get('q') ?? 'none'}</output>
        </>
      )
    }
    renderAt('/f', [{ path: '/f', title: 'common.nav.jobs', element: Filter, protected: false }])
    const button = screen.getByRole('button', { name: 'Search' })
    button.focus()
    vi.mocked(window.scrollTo).mockClear()
    fireEvent.click(button)
    expect(screen.getByRole('status')).toHaveTextContent('abc')
    expect(button).toHaveFocus()
    expect(window.scrollTo).not.toHaveBeenCalled()
  })

  it('falls back to the main area when a page has no heading', () => {
    renderAt('/', [
      { path: '/', title: 'common.nav.dashboard', element: () => <p>home</p>, protected: false },
      { path: '/bare', title: 'common.nav.jobs', element: () => <p>bare</p>, protected: false },
    ])
    act(() => navigate('/bare'))
    expect(document.getElementById('main')).toHaveFocus()
  })
})

describe('useSearchParams', () => {
  function Box() {
    const [query, setSearch] = useSearchParams()
    return (
      <>
        <h1>Box</h1>
        <output>{query.toString()}</output>
        <button type="button" onClick={() => setSearch({ a: '1', b: null })}>
          set
        </button>
        <button type="button" onClick={() => setSearch({ page: '2' }, { push: true })}>
          push
        </button>
      </>
    )
  }
  const box: RouteDef[] = [{ path: '/box', title: 'common.nav.jobs', element: Box, protected: false }]

  it('sets, removes and keeps the other parameters, replacing the history entry', () => {
    renderAt('/box?b=old&keep=yes', box)
    const entries = window.history.length
    fireEvent.click(screen.getByRole('button', { name: 'set' }))
    expect(window.location.search).toBe('?keep=yes&a=1')
    expect(screen.getByRole('status')).toHaveTextContent('keep=yes&a=1')
    expect(window.history.length).toBe(entries)
  })

  it('paired control: push adds a history entry', () => {
    renderAt('/box', box)
    const entries = window.history.length
    fireEvent.click(screen.getByRole('button', { name: 'push' }))
    expect(window.history.length).toBe(entries + 1)
    expect(window.location.search).toBe('?page=2')
  })
})

describe('useIsActive', () => {
  function Probe({ to, end }: { to: string; end?: boolean }) {
    return <output aria-label={`${to}${end ? ' end' : ''}`}>{String(useIsActive(to, end))}</output>
  }
  function Probes() {
    return (
      <>
        <h1>probes</h1>
        <Probe to="/jobs" />
        <Probe to="/" />
        <Probe to="/" end />
        <Probe to="/accounts" />
      </>
    )
  }

  it('is true on the page and below it, the dashboard only on itself, false elsewhere', () => {
    renderAt('/jobs/7', [{ path: '/jobs/:id', title: 'common.nav.jobs', element: Probes, protected: false }])
    expect(screen.getByLabelText('/jobs')).toHaveTextContent('true')
    expect(screen.getByLabelText('/')).toHaveTextContent('false')
    expect(screen.getByLabelText('/ end')).toHaveTextContent('false')
    expect(screen.getByLabelText('/accounts')).toHaveTextContent('false')
  })
})

describe('the guard', () => {
  function Counted({ onMount }: { onMount: () => void }) {
    useEffect(() => {
      onMount()
    }, [onMount])
    return <h1 tabIndex={-1}>Secret page</h1>
  }

  function secret(onMount: () => void): RouteDef[] {
    return [
      { path: '/secret', title: 'common.nav.jobs', element: () => <Counted onMount={onMount} />, protected: true },
      { path: '/sign-in', title: 'common.nav.jobs', element: () => <h1 tabIndex={-1}>Sign in page</h1>, protected: false },
    ]
  }

  it('renders NOTHING for a protected route it redirects: no page, no mount, no layout', () => {
    const onMount = vi.fn()
    const renders: string[] = []
    const { container } = renderAt('/secret', secret(onMount), (route) => {
      renders.push(route?.path ?? '-')
      return route?.protected ? '/sign-in' : null
    })
    // the redirect already happened (the effect ran inside render's act), and the page the user lands on is the sign-in page
    expect(window.location.pathname).toBe('/sign-in')
    expect(screen.queryByText('Secret page')).not.toBeInTheDocument()
    expect(onMount).not.toHaveBeenCalled()
    expect(screen.getByRole('heading', { level: 1, name: 'Sign in page' })).toBeInTheDocument()
    expect(container.textContent).not.toContain('Secret')
  })

  it('never even renders the element of a route it redirects (so nothing is fetched and nothing flashes)', () => {
    const rendered: string[] = []
    const routes: RouteDef[] = [
      {
        path: '/secret',
        title: 'common.nav.jobs',
        element: () => {
          rendered.push('secret')
          return <h1 tabIndex={-1}>Secret page</h1>
        },
        protected: true,
      },
      { path: '/sign-in', title: 'common.nav.jobs', element: () => <h1 tabIndex={-1}>Sign in page</h1>, protected: false },
    ]
    const { unmount } = renderAt('/secret', routes, (route) => (route?.protected ? '/sign-in' : null))
    expect(rendered).toEqual([])
    unmount()
    // paired control: the same route, let through, is rendered
    renderAt('/secret', routes, () => null)
    expect(rendered).toEqual(['secret'])
  })

  it('redirects by replacing the history entry, so the back button does not return to the protected page', () => {
    window.history.replaceState(null, '', '/')
    const start = window.history.length
    renderAt('/secret', secret(() => {}), (route) => (route?.protected ? '/sign-in' : null))
    expect(window.history.length).toBe(start)
  })

  it('paired control: when the guard lets the route through, the page is created and mounted once', () => {
    const onMount = vi.fn()
    renderAt('/secret', secret(onMount), () => null)
    expect(screen.getByRole('heading', { level: 1, name: 'Secret page' })).toBeInTheDocument()
    expect(onMount).toHaveBeenCalledTimes(1)
    expect(window.location.pathname).toBe('/secret')
  })

  it('follows the guard when its answer changes (a session that ends while a protected page is open)', () => {
    let signedIn = true
    const routes = secret(() => {})
    window.history.replaceState(null, '', '/secret')
    const guard = (route: RouteDef | null) => (route?.protected && !signedIn ? '/sign-in' : null)
    const { rerender } = render(<Router routes={routes} guard={guard} notFound={NotFound} layout={Layout} />)
    expect(screen.getByRole('heading', { level: 1, name: 'Secret page' })).toBeInTheDocument()
    signedIn = false
    rerender(<Router routes={routes} guard={(r) => guard(r)} notFound={NotFound} layout={Layout} />)
    expect(screen.queryByText('Secret page')).not.toBeInTheDocument()
    expect(window.location.pathname).toBe('/sign-in')
  })

  it('the guard sees the path, the query and the hash it must remember', () => {
    const seen: string[] = []
    window.history.replaceState(null, '', '/secret?x=1#h')
    render(
      <Router
        routes={secret(() => {})}
        guard={(route, loc) => {
          seen.push(`${loc.pathname}${loc.search}${loc.hash}`)
          return route?.protected ? '/sign-in' : null
        }}
        notFound={NotFound}
        layout={Layout}
      />,
    )
    expect(seen[0]).toBe('/secret?x=1#h')
  })
})

describe('Link and a new browsing context', () => {
  it('gives a link with a target the safe rel by default, keeps an explicit rel, and leaves a same-window link alone', () => {
    render(
      <>
        <Link to="/a" target="_blank">out</Link>
        <Link to="/b" target="_blank" rel="author">own</Link>
        <Link to="/c">same</Link>
        <Link to="/d" target="_self">self</Link>
      </>,
    )
    expect(screen.getByText('out').getAttribute('rel')).toBe('noopener noreferrer')
    expect(screen.getByText('own').getAttribute('rel')).toBe('author')
    expect(screen.getByText('same').getAttribute('rel')).toBeNull()
    expect(screen.getByText('self').getAttribute('rel')).toBeNull()
  })
})
