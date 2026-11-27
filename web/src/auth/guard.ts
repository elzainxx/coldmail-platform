// The guard of the router (router.tsx): where, if anywhere, a route sends the user, given who they are.
//
//   signed out         a protected page -> /sign-in?next=<where they were going>; the password pages -> /sign-in with the "link expired" notice
//   password required  (an invite or reset link was opened) every protected page and the sign-in page -> the page that sets the password
//   signed in          the sign-in pages (guestOnly) -> where they were going (`next`) or the dashboard; the password pages -> the dashboard
//
// It is a pure function of the session state and the route, so a test can walk every combination and prove it never redirects twice in a row.
import type { LocationInfo, RouteDef } from '../router.tsx'
import { ACCEPT_INVITE_PATH, AUTH_PATHS, HOME_PATH, RESET_PASSWORD_PATH, SIGN_IN_PATH } from './paths.ts'
import type { SessionSnapshot } from './session.ts'

/**
 * Where to go after signing in: the `next` the guard put in the URL, if it is a path of this app. Anything else (another site, a protocol-relative
 * `//host`, a backslash trick, a sign-in page itself) is the dashboard.
 */
export function safeNext(next: string | null | undefined): string {
  if (next === null || next === undefined || next === '' || next.length > 2000) return HOME_PATH
  if (!next.startsWith('/') || next.startsWith('//') || next.includes('\\') || /[\u0000-\u001f]/.test(next)) return HOME_PATH
  let url: URL
  try {
    url = new URL(next, 'http://app.invalid')
  } catch {
    return HOME_PATH
  }
  if (url.origin !== 'http://app.invalid' || AUTH_PATHS.includes(url.pathname)) return HOME_PATH
  return url.pathname + url.search + url.hash
}

export function guardRoute(snapshot: SessionSnapshot, route: RouteDef | null, location: LocationInfo): string | null {
  if (route === null) return null // the not-found page shows to anybody and holds nothing

  switch (snapshot.status) {
    case 'signed-out': {
      if (route.passwordSetup === true) return `${SIGN_IN_PATH}?notice=link_expired`
      if (!route.protected) return null
      const here = location.pathname + location.search
      return here === HOME_PATH ? SIGN_IN_PATH : `${SIGN_IN_PATH}?next=${encodeURIComponent(here)}`
    }
    case 'password-required': {
      if (route.passwordSetup === true) return null
      if (route.protected || route.guestOnly === true) return snapshot.setupKind === 'recovery' ? RESET_PASSWORD_PATH : ACCEPT_INVITE_PATH
      return null
    }
    case 'signed-in':
      // the sign-in page, once signed in, goes where the guard sent the user from (`next`); the password pages go to the dashboard
      if (route.guestOnly === true) return safeNext(new URLSearchParams(location.search).get('next'))
      return route.passwordSetup === true ? HOME_PATH : null
  }
}
