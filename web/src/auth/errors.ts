// What a sign-in service failure means to the user: one line of the catalogue per kind. GoTrue's own text is never read (auth/gotrue.ts drops it).
import type { CopyKey } from '../copy/index.ts'
import { AuthError, type AuthErrorKind } from './gotrue.ts'

const KEYS: Record<AuthErrorKind, CopyKey> = {
  invalid_credentials: 'auth.error.invalid_credentials',
  email_not_confirmed: 'auth.error.email_not_confirmed',
  account_disabled: 'auth.error.account_disabled',
  rate_limited: 'auth.error.rate_limited',
  weak_password: 'auth.error.weak_password',
  same_password: 'auth.error.same_password',
  session_ended: 'auth.error.session_ended',
  unavailable: 'auth.error.unavailable',
  network: 'auth.error.network',
  unknown: 'auth.error.unknown',
}

export function authErrorKey(kind: AuthErrorKind): CopyKey {
  return KEYS[kind]
}

/** The line for any error thrown while signing in: an AuthError by its kind, anything else as unknown. */
export function authErrorKeyOf(e: unknown): CopyKey {
  return e instanceof AuthError ? KEYS[e.kind] : KEYS.unknown
}
