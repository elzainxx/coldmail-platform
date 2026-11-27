// Helpers for tests of pages and of the foundation. Test files only: no app code imports this.
//
//   await signInForTest()                    the session becomes a signed-in one (a fake sign-in service, a token that expires in an hour)
//   resetForTest()                           signed out, nothing stored, connectivity ok, URL "/"
//   mockApi({ 'GET /v1/jobs': {...} })       stubs fetch: each key is "METHOD /path" (no query), each value a JSON body, a number (status with an empty body),
//                                            a [status, body] pair, or a function (url, init) => Response | body. Returns the vi.fn to assert on.
//   renderApp('/jobs')                       the whole app (router, layout, guard, toasts) at that path, ready at once
import { render } from '@testing-library/react'
import { vi } from 'vitest'
import { resetConnectivity } from '../api/connectivity.ts'
import { App } from '../App.tsx'
import type { GoTrueClient, TokenSet } from '../auth/gotrue.ts'
import { session } from '../auth/session.ts'
import { resetConfigCache } from '../auth/config.ts'

function b64url(value: unknown): string {
  return btoa(JSON.stringify(value)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

/** A token that looks like a JWT and expires at `expSeconds` (seconds since 1970). It proves nothing to anybody: the tests use it to schedule the refresh. */
export function fakeJwt(expSeconds: number, extra: Record<string, unknown> = {}): string {
  return `${b64url({ alg: 'none', typ: 'JWT' })}.${b64url({ sub: 'user-1', exp: expSeconds, ...extra })}.sig`
}

let tokenSequence = 0

/** Tokens that are different every time (a counter in the payload), so a test can tell the one before a refresh from the one after. */
export function fakeTokens(overrides: Partial<TokenSet> = {}, expiresInSeconds = 3600): TokenSet {
  return {
    accessToken: fakeJwt(Math.floor(Date.now() / 1000) + expiresInSeconds, { n: ++tokenSequence }),
    refreshToken: 'refresh-1',
    expiresIn: expiresInSeconds,
    ...overrides,
  }
}

export function fakeGoTrue(overrides: Partial<GoTrueClient> = {}): GoTrueClient {
  return {
    signInWithPassword: vi.fn(async () => fakeTokens()),
    refresh: vi.fn(async () => fakeTokens({ refreshToken: 'refresh-2' })),
    recover: vi.fn(async () => {}),
    updatePassword: vi.fn(async () => {}),
    signOut: vi.fn(async () => {}),
    ...overrides,
  }
}

export async function signInForTest(gotrue: GoTrueClient = fakeGoTrue()): Promise<GoTrueClient> {
  session.configure(gotrue)
  await session.signIn('person@example.test', 'a long enough password')
  return gotrue
}

export function resetForTest(): void {
  session.clear()
  session.configure(null) // not booted: the next App loads its config again
  resetConnectivity()
  resetConfigCache()
  window.history.replaceState(null, '', '/')
  document.title = ''
}

export function jsonResponse(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })
}

type Handler = unknown | number | [number, unknown] | ((url: URL, init: RequestInit) => Response | unknown)

export function mockApi(routes: Record<string, Handler>) {
  const fn = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, window.location.origin)
    const key = `${(init.method ?? 'GET').toUpperCase()} ${url.pathname}`
    if (!(key in routes)) return jsonResponse({ error: { code: 'not_found', message: 'x', request_id: 'req_test' } }, 404)
    let h = routes[key] as Handler
    if (typeof h === 'function') h = (h as (u: URL, i: RequestInit) => Response | unknown)(url, init)
    if (h instanceof Response) return h
    if (typeof h === 'number') return new Response(null, { status: h })
    if (Array.isArray(h) && typeof h[0] === 'number') return jsonResponse(h[1], h[0])
    return jsonResponse(h)
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

/** The whole app at `path`, ready at once (the session is already configured). */
export function renderApp(path = '/') {
  window.history.replaceState(null, '', path)
  if (!session.isConfigured()) session.configure(fakeGoTrue())
  return render(<App />)
}
