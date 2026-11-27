// The link in an invitation or a password-reset email lands on the app with the session in the URL HASH:
//
//   /#access_token=...&refresh_token=...&expires_in=3600&token_type=bearer&type=invite      (type=recovery for a reset)
//   /#error=access_denied&error_code=otp_expired&error_description=...                       (a link that expired or was used)
//
// consumeAuthHash() reads it ONCE at start, before anything renders, and removes the hash from the address bar at once (history.replaceState) so the tokens
// are not left in the URL, the history entry, a screenshot or a copied link. The tokens are then kept in memory by the session.
import type { SetupKind } from './session.ts'
import type { TokenSet } from './gotrue.ts'

export type AuthLanding =
  | { kind: 'session'; setup: SetupKind; tokens: TokenSet }
  /** A link that did not give a usable session: expired, already used, or of a kind this app does not take (a sign-up or magic link). */
  | { kind: 'error' }

const MAX_TOKEN_LENGTH = 8 * 1024

/** Reads a hash; null when it is not an auth landing at all (an ordinary `#section` is left alone). */
export function parseAuthHash(hash: string): AuthLanding | null {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash
  if (raw === '') return null
  const params = new URLSearchParams(raw)

  if (params.has('error') || params.has('error_code')) return { kind: 'error' }
  if (!params.has('access_token')) return null

  const type = params.get('type')
  const access = params.get('access_token') ?? ''
  const refresh = params.get('refresh_token') ?? ''
  const expiresIn = Number(params.get('expires_in') ?? '')
  if ((type !== 'invite' && type !== 'recovery') || access === '' || refresh === '') return { kind: 'error' }
  if (access.length > MAX_TOKEN_LENGTH || refresh.length > MAX_TOKEN_LENGTH) return { kind: 'error' }
  return {
    kind: 'session',
    setup: type,
    tokens: { accessToken: access, refreshToken: refresh, expiresIn: Number.isFinite(expiresIn) && expiresIn > 0 ? expiresIn : 3600 },
  }
}

/** Parse the current URL's hash and take it out of the address bar. Returns what it held (null when it was not an auth hash, which is left in place). */
export function consumeAuthHash(win: Pick<Window, 'location' | 'history'> = window): AuthLanding | null {
  const landing = parseAuthHash(win.location.hash)
  if (landing !== null) {
    win.history.replaceState(win.history.state, '', win.location.pathname + win.location.search)
  }
  return landing
}
