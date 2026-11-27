import { describe, expect, it } from 'vitest'
import { guardRoute, safeNext } from './guard.ts'
import { ACCEPT_INVITE_PATH, FORGOT_PASSWORD_PATH, RESET_PASSWORD_PATH, SIGN_IN_PATH } from './paths.ts'
import type { SessionSnapshot } from './session.ts'
import { matchRoute, type RouteDef } from '../router.tsx'

const page = () => null
const route = (path: string, flags: Partial<RouteDef> = {}): RouteDef => ({ path, title: 'common.nav.jobs', element: page, protected: false, ...flags })

const ROUTES: RouteDef[] = [
  route('/', { protected: true }),
  route('/jobs', { protected: true }),
  route('/jobs/:id', { protected: true }),
  route(SIGN_IN_PATH, { guestOnly: true }),
  route(FORGOT_PASSWORD_PATH, { guestOnly: true }),
  route(ACCEPT_INVITE_PATH, { passwordSetup: true }),
  route(RESET_PASSWORD_PATH, { passwordSetup: true }),
  route('/open'),
]

const OUT: SessionSnapshot = { status: 'signed-out', setupKind: null, endedBecause: null }
const IN: SessionSnapshot = { status: 'signed-in', setupKind: null, endedBecause: null }
const INVITED: SessionSnapshot = { status: 'password-required', setupKind: 'invite', endedBecause: null }
const RESET: SessionSnapshot = { status: 'password-required', setupKind: 'recovery', endedBecause: null }

const at = (pathname: string, search = '') => ({ pathname, search, hash: '' })
const guard = (s: SessionSnapshot, path: string, search = '') => guardRoute(s, matchRoute(ROUTES, path)?.route ?? null, at(path, search))

describe('guardRoute', () => {
  it('signed out: a protected page goes to sign in, remembering where the user was going', () => {
    expect(guard(OUT, '/jobs/42', '?tab=results')).toBe(`${SIGN_IN_PATH}?next=${encodeURIComponent('/jobs/42?tab=results')}`)
    expect(guard(OUT, '/')).toBe(SIGN_IN_PATH) // the dashboard is the default: nothing to remember
  })

  it('signed out: the public pages are open, the not-found page too', () => {
    for (const p of [SIGN_IN_PATH, FORGOT_PASSWORD_PATH, '/open', '/no/such/page']) expect(guard(OUT, p), p).toBeNull()
  })

  it('signed out: the password pages need a link, so they go to sign in with the "link expired" notice', () => {
    expect(guard(OUT, ACCEPT_INVITE_PATH)).toBe(`${SIGN_IN_PATH}?notice=link_expired`)
    expect(guard(OUT, RESET_PASSWORD_PATH)).toBe(`${SIGN_IN_PATH}?notice=link_expired`)
  })

  it('password required: everything that shows data, and the sign-in pages, go to the page that sets the password', () => {
    for (const p of ['/', '/jobs', '/jobs/1', SIGN_IN_PATH, FORGOT_PASSWORD_PATH]) {
      expect(guard(INVITED, p), p).toBe(ACCEPT_INVITE_PATH)
      expect(guard(RESET, p), p).toBe(RESET_PASSWORD_PATH)
    }
    expect(guard(INVITED, ACCEPT_INVITE_PATH)).toBeNull()
    expect(guard(RESET, RESET_PASSWORD_PATH)).toBeNull()
  })

  it('signed in: the pages are open; the sign-in pages go on to where the user was going, or the dashboard', () => {
    for (const p of ['/', '/jobs', '/jobs/1', '/open', '/no/such/page']) expect(guard(IN, p), p).toBeNull()
    expect(guard(IN, SIGN_IN_PATH)).toBe('/')
    expect(guard(IN, SIGN_IN_PATH, `?next=${encodeURIComponent('/jobs/9?tab=a')}`)).toBe('/jobs/9?tab=a')
    expect(guard(IN, FORGOT_PASSWORD_PATH)).toBe('/')
    expect(guard(IN, ACCEPT_INVITE_PATH)).toBe('/')
  })

  it('paired control: signed out is NOT let through to a protected page, signed in is (the guard does depend on the session)', () => {
    expect(guard(OUT, '/jobs')).not.toBeNull()
    expect(guard(IN, '/jobs')).toBeNull()
  })

  it('never sends anybody to a place that sends them on again (no redirect loop), in any state', () => {
    const paths = ['/', '/jobs', '/jobs/1', SIGN_IN_PATH, FORGOT_PASSWORD_PATH, ACCEPT_INVITE_PATH, RESET_PASSWORD_PATH, '/open', '/nowhere']
    let checked = 0
    for (const s of [OUT, IN, INVITED, RESET]) {
      for (const p of paths) {
        const target = guard(s, p)
        if (target === null) continue
        const url = new URL(target, 'http://app.invalid')
        expect(guard(s, url.pathname, url.search), `${s.status}: ${p} -> ${target}`).toBeNull()
        checked++
      }
    }
    expect(checked).toBeGreaterThan(15) // the walk looked at real redirects
  })
})

describe('safeNext', () => {
  it('keeps a path of this app, with its query and hash', () => {
    expect(safeNext('/jobs/42?tab=results#top')).toBe('/jobs/42?tab=results#top')
    expect(safeNext('/')).toBe('/')
  })

  it.each([
    ['another site', 'https://evil.example.test/x'],
    ['a protocol-relative URL', '//evil.example.test/x'],
    ['a backslash that browsers read as a slash', '/\\evil.example.test'],
    ['javascript:', 'javascript:alert(1)'],
    ['a relative path', 'jobs'],
    ['a control character', '/jobs\n/x'],
    ['empty', ''],
    ['null', null],
    ['undefined', undefined],
    ['the sign-in page itself', '/sign-in'],
    ['a password page', '/accept-invite?x=1'],
    ['something too long', `/${'a'.repeat(2500)}`],
  ])('sends %s to the dashboard', (_what, value) => {
    expect(safeNext(value as string | null | undefined)).toBe('/')
  })
})
