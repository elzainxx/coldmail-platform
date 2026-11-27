// A hand-written client for the five GoTrue (Supabase Auth) routes this app uses. The browser holds only the PUBLIC anon key (from /config.json).
//
//   POST <base>/token?grant_type=password         { email, password }          -> tokens
//   POST <base>/token?grant_type=refresh_token    { refresh_token }            -> tokens
//   POST <base>/recover                           { email }                    -> {}
//   PUT  <base>/user                              { password }  (Bearer)       -> the user
//   POST <base>/logout?scope=local                (Bearer)                     -> 204
//
// Every request carries `apikey: <anon key>`. GoTrue's own error text is NEVER kept: a failure becomes an AuthError whose `kind` the sign-in pages turn into
// a line of the copy catalogue (auth/errors.ts). Tokens are returned to the caller and written nowhere here.
import type { AppConfig } from '../api/types.ts'

export interface TokenSet {
  accessToken: string
  refreshToken: string
  /** seconds, as GoTrue says */
  expiresIn: number
}

export type AuthErrorKind =
  | 'invalid_credentials'
  | 'email_not_confirmed'
  | 'account_disabled'
  | 'rate_limited'
  | 'weak_password'
  | 'same_password'
  | 'session_ended'
  | 'unavailable'
  | 'network'
  | 'unknown'

export const AUTH_ERROR_KINDS: readonly AuthErrorKind[] = [
  'invalid_credentials',
  'email_not_confirmed',
  'account_disabled',
  'rate_limited',
  'weak_password',
  'same_password',
  'session_ended',
  'unavailable',
  'network',
  'unknown',
]

export class AuthError extends Error {
  readonly kind: AuthErrorKind
  readonly status: number

  constructor(kind: AuthErrorKind, status: number) {
    super(kind) // the kind, never GoTrue's text
    this.name = 'AuthError'
    this.kind = kind
    this.status = status
  }
}

export function isAuthError(e: unknown): e is AuthError {
  return e instanceof AuthError
}

export interface GoTrueClient {
  signInWithPassword(email: string, password: string): Promise<TokenSet>
  refresh(refreshToken: string): Promise<TokenSet>
  /** Resolves for every answer GoTrue gives (see below); rejects only when the request could not be made or the service failed. */
  recover(email: string): Promise<void>
  updatePassword(accessToken: string, password: string): Promise<void>
  signOut(accessToken: string): Promise<void>
}

/**
 * The base URL GoTrue is called at. The value of `supabase_url` is used as it is, except for a hosted Supabase project given as its bare origin
 * (`https://<ref>.supabase.co`), whose Auth lives under `/auth/v1`. A self-hosted or test GoTrue is given as the URL it answers at.
 */
export function gotrueBase(supabaseUrl: string): string {
  const url = new URL(supabaseUrl)
  const path = url.pathname.replace(/\/+$/, '')
  if (path === '' && url.hostname.endsWith('.supabase.co')) return `${url.origin}/auth/v1`
  return `${url.origin}${path}`
}

function asRecord(v: unknown): Record<string, unknown> {
  return typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {}
}

/** What a failed answer of GoTrue means, from its status and its body (GoTrue's text is read for a code only and then dropped). */
export function classifyAuthFailure(status: number, body: unknown): AuthErrorKind {
  const b = asRecord(body)
  // `error_code` is the current field; `error` is the older OAuth-style one. The words are looked at, not shown.
  const code = [b['error_code'], b['error']].find((v): v is string => typeof v === 'string') ?? ''
  const text = [b['msg'], b['message'], b['error_description']].find((v): v is string => typeof v === 'string')?.toLowerCase() ?? ''

  if (status >= 500) return 'unavailable'
  if (status === 429 || code === 'over_request_rate_limit' || code === 'over_email_send_rate_limit' || code === 'over_sms_send_rate_limit') return 'rate_limited'
  switch (code) {
    case 'invalid_credentials':
    case 'user_not_found':
    case 'validation_failed': // a malformed email or password in the body: the user sees the same line as for wrong ones
      return 'invalid_credentials'
    case 'email_not_confirmed':
      return 'email_not_confirmed'
    case 'user_banned':
      return 'account_disabled'
    case 'weak_password':
      return 'weak_password'
    case 'same_password':
      return 'same_password'
    case 'otp_expired':
    case 'otp_disabled':
    case 'flow_state_expired':
    case 'flow_state_not_found':
    case 'session_expired':
    case 'session_not_found':
    case 'refresh_token_not_found':
    case 'refresh_token_already_used':
    case 'bad_jwt':
    case 'no_authorization':
      return 'session_ended'
    case 'invalid_grant':
      return text.includes('invalid login credentials') ? 'invalid_credentials' : 'session_ended'
  }
  if (text.includes('invalid login credentials')) return 'invalid_credentials'
  if (text.includes('email not confirmed')) return 'email_not_confirmed'
  if (text.includes('password') && (text.includes('weak') || text.includes('at least'))) return 'weak_password'
  if (status === 401 || status === 403) return 'session_ended'
  return 'unknown'
}

function readTokens(body: unknown): TokenSet | null {
  const b = asRecord(body)
  const access = b['access_token']
  const refresh = b['refresh_token']
  const expiresIn = b['expires_in']
  if (typeof access !== 'string' || access === '' || typeof refresh !== 'string' || refresh === '') return null
  return { accessToken: access, refreshToken: refresh, expiresIn: typeof expiresIn === 'number' && expiresIn > 0 ? expiresIn : 3600 }
}

export function createGoTrue(config: Pick<AppConfig, 'supabase_url' | 'anon_key'>, fetchImpl?: typeof fetch): GoTrueClient {
  const base = gotrueBase(config.supabase_url)
  const doFetch = (input: string, init: RequestInit) => (fetchImpl ?? globalThis.fetch)(input, init)

  async function call(method: string, path: string, opts: { body?: unknown; bearer?: string } = {}): Promise<{ status: number; body: unknown }> {
    const headers: Record<string, string> = { apikey: config.anon_key, Accept: 'application/json' }
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
    if (opts.bearer !== undefined) headers['Authorization'] = `Bearer ${opts.bearer}`
    let res: Response
    try {
      res = await doFetch(`${base}${path}`, {
        method,
        headers,
        body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
        cache: 'no-store',
        credentials: 'omit',
        referrerPolicy: 'no-referrer',
      })
    } catch {
      throw new AuthError('network', 0)
    }
    let body: unknown
    try {
      const text = await res.text()
      body = text === '' ? undefined : JSON.parse(text)
    } catch {
      body = undefined
    }
    if (!res.ok) throw new AuthError(classifyAuthFailure(res.status, body), res.status)
    return { status: res.status, body }
  }

  async function tokenGrant(grant: 'password' | 'refresh_token', body: unknown): Promise<TokenSet> {
    const res = await call('POST', `/token?grant_type=${grant}`, { body })
    const tokens = readTokens(res.body)
    if (tokens === null) throw new AuthError('unknown', res.status)
    return tokens
  }

  return {
    signInWithPassword: (email, password) => tokenGrant('password', { email, password }),
    refresh: (refreshToken) => tokenGrant('refresh_token', { refresh_token: refreshToken }),
    async recover(email) {
      try {
        await call('POST', '/recover', { body: { email } })
      } catch (e) {
        // The page says the same thing whether or not the address has an account, so an answer that is about the address (a 4xx, a rate limit) is not told.
        // Only "could not ask" (no network) and "the service failed" (5xx) are real failures.
        if (isAuthError(e) && e.kind !== 'network' && e.kind !== 'unavailable') return
        throw e
      }
    },
    async updatePassword(accessToken, password) {
      await call('PUT', '/user', { body: { password }, bearer: accessToken })
    },
    async signOut(accessToken) {
      await call('POST', '/logout?scope=local', { bearer: accessToken })
    },
  }
}
