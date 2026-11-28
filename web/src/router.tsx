// A small router on the History API. No dependency.
//
//   <Router routes guard notFound layout />   the one place a page is chosen and rendered
//   <Link to="/jobs/42">                      a real <a href>, handled without a reload (a modified click or target=_blank is left to the browser)
//   navigate("/jobs", { replace })            from anywhere, also outside React
//   useRoute()                                { pathname, search, hash, params, query, route }
//   useSearchParams()                         [query, setSearch(patch)]: the query string as state (filters, cursors); it does not move focus or scroll
//   pathFor("/jobs/:id", { id })              builds a path from a route pattern
//
// On every change of PATH (not of the query) the router scrolls to the top, sets the document title from the route's copy key and focuses the page's <h1>
// (every page has exactly one, ui/PageHeader gives it tabIndex -1) so a keyboard or screen reader user lands at the top of the new page.
//
// THE GUARD. `guard(route, location)` returns a path to redirect to, or null. While it returns a path the router renders NOTHING for the route: a protected
// page's element is never created, so it cannot fetch anything or flash. The guard of the app is auth/guard.ts.
import {
  Suspense,
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useSyncExternalStore,
  type AnchorHTMLAttributes,
  type ComponentType,
  type LazyExoticComponent,
  type MouseEvent,
  type ReactNode,
} from 'react'
import { t, type CopyKey } from './copy/index.ts'

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// The route definition every area's routes.ts uses
// ---------------------------------------------------------------------------------------------------------------------------------------------------

export interface RouteDef {
  /** A path pattern: `/jobs`, `/jobs/:id` (a `:name` segment is a parameter, read with useRoute().params). Static segments win over parameters. */
  path: string
  /** The copy key of the page title (document title and the first thing a screen reader hears). Every title is a key of src/copy. */
  title: CopyKey
  /** The page component (or a React.lazy one): it takes no props and reads the route with useRoute(). */
  element: ComponentType | LazyExoticComponent<ComponentType>
  /** true: the page shows a user's data. It renders nothing and redirects to the sign-in page unless the user is signed in. */
  protected: boolean
  /** true: only for people who are not signed in (the sign-in page). A signed-in user is sent to the dashboard. */
  guestOnly?: boolean
  /** true: reached from an invitation or reset link, to choose a password. Anyone else is sent away. */
  passwordSetup?: boolean
}

export interface RouteMatch {
  route: RouteDef
  params: Record<string, string>
}

export interface LocationInfo {
  pathname: string
  search: string
  hash: string
}

export interface RouteState extends LocationInfo {
  params: Record<string, string>
  query: URLSearchParams
  route: RouteDef | null
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// Matching (pure)
// ---------------------------------------------------------------------------------------------------------------------------------------------------

const segments = (path: string): string[] => path.split('/').filter((s) => s !== '')

/** The parameters of `pathname` under `pattern`, or null when it does not match. A parameter that is not valid percent-encoding does not match. */
export function matchPath(pattern: string, pathname: string): Record<string, string> | null {
  const p = segments(pattern)
  const a = segments(pathname)
  if (p.length !== a.length) return null
  const params: Record<string, string> = {}
  for (let i = 0; i < p.length; i++) {
    const want = p[i] as string
    const got = a[i] as string
    if (want.startsWith(':')) {
      try {
        params[want.slice(1)] = decodeURIComponent(got)
      } catch {
        return null
      }
    } else if (want !== got) {
      return null
    }
  }
  return params
}

/** The route for a path: among the matching patterns the one with the most static segments wins (`/jobs/new` beats `/jobs/:id`), then the first listed. */
export function matchRoute(routes: readonly RouteDef[], pathname: string): RouteMatch | null {
  let best: { match: RouteMatch; score: number } | null = null
  for (const route of routes) {
    const params = matchPath(route.path, pathname)
    if (params === null) continue
    const score = segments(route.path).filter((s) => !s.startsWith(':')).length
    if (best === null || score > best.score) best = { match: { route, params }, score }
  }
  return best === null ? null : best.match
}

/** Fill the parameters of a pattern: pathFor('/jobs/:id', { id: 'a b' }) is '/jobs/a%20b'. A parameter that is missing is a programming error. */
export function pathFor(pattern: string, params: Record<string, string | number> = {}, query?: Record<string, string | number | undefined | null>): string {
  const path = '/' + segments(pattern).map((s) => {
    if (!s.startsWith(':')) return s
    const v = params[s.slice(1)]
    if (v === undefined) throw new Error(`pathFor: "${pattern}" needs the parameter "${s.slice(1)}"`)
    return encodeURIComponent(String(v))
  }).join('/')
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(query ?? {})) if (v !== undefined && v !== null && v !== '') qs.set(k, String(v))
  const s = qs.toString()
  return s === '' ? path : `${path}?${s}`
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------------------------------------------------------------------------------

const listeners = new Set<() => void>()
const notify = () => {
  for (const l of [...listeners]) l()
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  window.addEventListener('popstate', notify)
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0) window.removeEventListener('popstate', notify)
  }
}

const currentUrl = () => window.location.pathname + window.location.search + window.location.hash

/** Go to a path of this app (never another site). `replace` swaps the current history entry. */
export function navigate(to: string, options: { replace?: boolean } = {}): void {
  const url = new URL(to, window.location.origin)
  if (url.origin !== window.location.origin) throw new Error('navigate: only paths of this app')
  const target = url.pathname + url.search + url.hash
  if (target === currentUrl() && !options.replace) return
  if (options.replace) window.history.replaceState(null, '', target)
  else window.history.pushState(null, '', target)
  notify()
}

function parseLocation(url: string): LocationInfo {
  const u = new URL(url, 'http://app.invalid')
  return { pathname: u.pathname, search: u.search, hash: u.hash }
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// React
// ---------------------------------------------------------------------------------------------------------------------------------------------------

const RouteContext = createContext<RouteState | null>(null)

export function useRoute(): RouteState {
  const ctx = useContext(RouteContext)
  if (ctx === null) throw new Error('useRoute must be used inside <Router>')
  return ctx
}

/** The query string as state. `setSearch({ q: 'a', cursor: null })` sets q, removes cursor, keeps the rest; it replaces the history entry unless `push` is true. */
export function useSearchParams(): [URLSearchParams, (patch: Record<string, string | null | undefined>, options?: { push?: boolean }) => void] {
  const { query } = useRoute()
  const setSearch = (patch: Record<string, string | null | undefined>, options: { push?: boolean } = {}) => {
    const next = new URLSearchParams(window.location.search)
    for (const [k, v] of Object.entries(patch)) {
      if (v === null || v === undefined || v === '') next.delete(k)
      else next.set(k, v)
    }
    const qs = next.toString()
    navigate(window.location.pathname + (qs === '' ? '' : `?${qs}`) + window.location.hash, { replace: !options.push })
  }
  return [query, setSearch]
}

/** Is `to` the current page (or, without `end`, a parent of it)? For aria-current in a menu. */
export function useIsActive(to: string, end = false): boolean {
  const { pathname } = useRoute()
  const here = '/' + segments(pathname).join('/')
  const there = '/' + segments(to.split(/[?#]/)[0] ?? '').join('/')
  return end || there === '/' ? here === there : here === there || here.startsWith(`${there}/`)
}

export interface LinkProps extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, 'href'> {
  to: string
  replace?: boolean
}

export function Link({ to, replace, onClick, target, ...rest }: LinkProps) {
  const handle = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e)
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    if ((target !== undefined && target !== '_self') || rest.download !== undefined) return
    e.preventDefault()
    navigate(to, { replace })
  }
  // a link that opens another browsing context must not hand it window.opener or the referrer: the default is the safe one, an explicit rel still wins
  const rel = rest.rel ?? (target !== undefined && target !== '_self' ? 'noopener noreferrer' : undefined)
  return <a {...rest} href={to} target={target} rel={rel} onClick={handle} />
}

export interface LayoutProps {
  route: RouteDef | null
  pathname: string
  children: ReactNode
}

export interface RouterProps {
  routes: readonly RouteDef[]
  /** a path to redirect to, or null to show the route */
  guard: (route: RouteDef | null, location: LocationInfo) => string | null
  notFound: ComponentType
  layout: ComponentType<LayoutProps>
  /** what to show while a lazy page loads */
  fallback?: ReactNode
  /** the part of the document title after the page's own title */
  titleSuffix?: string
}

export function Router({ routes, guard, notFound: NotFound, layout: Layout, fallback = null, titleSuffix }: RouterProps) {
  const url = useSyncExternalStore(subscribe, currentUrl)
  const location = useMemo(() => parseLocation(url), [url])
  const match = useMemo(() => matchRoute(routes, location.pathname), [routes, location.pathname])
  const route = match?.route ?? null
  const redirect = guard(route, location)

  useEffect(() => {
    if (redirect !== null) navigate(redirect, { replace: true })
  }, [redirect])

  const previousPath = useRef<string | null>(null)
  const suffix = titleSuffix ?? t('common.brand')
  useEffect(() => {
    if (redirect !== null) return
    const title = route === null ? t('common.notFound.title') : t(route.title)
    document.title = `${title} - ${suffix}`
    if (previousPath.current !== null && previousPath.current !== location.pathname) {
      window.scrollTo(0, 0)
      const heading = document.querySelector<HTMLElement>('main h1') ?? document.querySelector<HTMLElement>('h1')
      const target = heading ?? document.getElementById('main')
      target?.focus({ preventScroll: true })
    }
    previousPath.current = location.pathname
  }, [redirect, route, location.pathname, suffix])

  const state = useMemo<RouteState>(
    () => ({ ...location, params: match?.params ?? {}, query: new URLSearchParams(location.search), route }),
    [location, match, route],
  )

  // A route that is redirecting renders NOTHING: its element is never created.
  if (redirect !== null) return null

  const Page = route === null ? NotFound : route.element
  return (
    <RouteContext value={state}>
      <Layout route={route} pathname={location.pathname}>
        <Suspense fallback={fallback}>
          <Page key={location.pathname} />
        </Suspense>
      </Layout>
    </RouteContext>
  )
}
