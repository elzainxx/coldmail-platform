// The users of the specs, made by the harness stage (tests/harness/e2e/stage.sh, e2e_users) and described in the fixtures file it wrote (E2E_FIXTURES, mode 600, mounted read-only):
//
//   a  funded     credits at modules 1 and 2, module 3 enabled, a sending account at module 2, an invite not yet opened
//   b  funded     the same: the OTHER user of the cross-user specs (A must never see B's jobs, accounts, suppressions or results, in the UI and in the API)
//   c  funded     the same: reserved for the one spec that opens an invite link IN THE BROWSER and sets a password through the screen (nothing in these helpers ever opens c's invite)
//   s  unfunded   a platform user with no credits, no entitlement and no sending account: the user the error-state specs sign in as (a low balance, module 3 not enabled, no sending account)
//
// Every user has an INVITE (GoTrue's admin route minted it: `invite_link` opens at GoTrue, which verifies it and sends the browser to the app with the session in the URL fragment: what the invite screen
// consumes) and a PASSWORD minted for the run. `ready(request, user)` is how a spec that is not about the invite gets a signed-in user without a screen: it signs in with the password, and when the user has
// none yet (the first time) it opens the invite link WITHOUT a browser (the redirect is read, not followed), sets the password with the invite's own tokens, and signs in. It is idempotent and keeps no state
// between calls (Playwright restarts a worker after a failure, and module state would be lost), so a spec may call it at the top of every test.
//
// NO VALUE IS EVER PUT IN AN ASSERTION MESSAGE. Access tokens, refresh tokens, invite links and passwords are secrets of a throw-away stack, and the stage refuses a run whose output holds one (a JWT-shaped
// string is red): compare booleans and statuses, never `expect(token).toBe(...)`.
import { readFileSync } from 'node:fs'
import type { APIRequestContext } from '@playwright/test'
import { anonKey, authUrl, fixturesPath } from './env.ts'

export interface FixtureUser {
  tag: string
  id: string
  email: string
  password: string
  invite_link: string
  funded: boolean
  /** The user's sending account at module 2 (a dedicated pool, naming the sink); null for a user without one. */
  m2_account_id: string | null
}

export interface Fixtures {
  version: 1
  urls: { app: string; auth: string; issuer: string; sink: string; m3_sink_host: string; sink_smtp_port: number }
  users: Record<string, FixtureUser>
}

let cached: Fixtures | undefined

/** The fixtures file, read once per worker. */
export function fixtures(): Fixtures {
  if (cached === undefined) {
    const f = JSON.parse(readFileSync(fixturesPath(), 'utf8')) as Fixtures
    if (f.version !== 1 || typeof f.users !== 'object') throw new Error('the fixtures file is not version 1: the stage and the helpers are out of step')
    cached = f
  }
  return cached
}

export type UserTag = 'a' | 'b' | 'c' | 's'

export function user(tag: UserTag): FixtureUser {
  const u = fixtures().users[tag]
  if (u === undefined) throw new Error(`the fixtures file has no user '${tag}'`)
  return u
}

export interface Session {
  accessToken: string
  refreshToken: string
}

/** The header of an authenticated call to the platform's API. */
export function bearer(session: Session): Record<string, string> {
  return { Authorization: `Bearer ${session.accessToken}` }
}

const gotrueHeaders = (): Record<string, string> => ({ apikey: anonKey(), 'Content-Type': 'application/json' })

function tokensOf(body: unknown): Session | null {
  const b = body as { access_token?: unknown; refresh_token?: unknown } | null
  if (typeof b?.access_token === 'string' && b.access_token !== '' && typeof b.refresh_token === 'string' && b.refresh_token !== '') {
    return { accessToken: b.access_token, refreshToken: b.refresh_token }
  }
  return null
}

/** Password sign in against GoTrue; null when GoTrue refuses the credentials (no password set yet, or a wrong one). */
export async function passwordSignIn(request: APIRequestContext, u: Pick<FixtureUser, 'email' | 'password'>): Promise<Session | null> {
  const res = await request.post(`${authUrl()}/token?grant_type=password`, { headers: gotrueHeaders(), data: { email: u.email, password: u.password } })
  if (res.status() === 200) return tokensOf(await res.json())
  if (res.status() === 400 || res.status() === 401 || res.status() === 422) return null
  throw new Error(`GoTrue answered ${res.status()} to a password sign in (not a refusal of the credentials: the service is not what the stage made)`)
}

/** Opens an invite link without a browser: GoTrue verifies it and answers a redirect to the app whose URL fragment holds the session. Returns that session. */
export async function acceptInvite(request: APIRequestContext, u: Pick<FixtureUser, 'tag' | 'invite_link'>): Promise<Session> {
  const res = await request.get(u.invite_link, { maxRedirects: 0 })
  const location = res.headers()['location']
  if (res.status() < 300 || res.status() > 399 || location === undefined) {
    throw new Error(`the invite link of the user '${u.tag}' answered ${res.status()} and no redirect: it was already used, or it has expired`)
  }
  const params = new URLSearchParams(new URL(location).hash.replace(/^#/, ''))
  const failure = params.get('error_code') ?? params.get('error')
  if (failure !== null) throw new Error(`the invite link of the user '${u.tag}' was refused by GoTrue (${failure}): it was already used, or it has expired`)
  const session = tokensOf({ access_token: params.get('access_token'), refresh_token: params.get('refresh_token') })
  if (session === null) throw new Error(`the redirect of the invite link of the user '${u.tag}' carries no session in its fragment`)
  return session
}

/** Sets the user's password with the tokens an invite (or a sign in) gave: GoTrue's `PUT /user`, as the app's own set-password screen does. */
export async function setPassword(request: APIRequestContext, session: Session, password: string): Promise<void> {
  const res = await request.put(`${authUrl()}/user`, { headers: { ...gotrueHeaders(), ...bearer(session) }, data: { password } })
  if (res.status() !== 200) throw new Error(`GoTrue answered ${res.status()} to setting the password`)
}

/** A signed-in session for the user, WITHOUT a screen (see the header). Never for user c, whose invite belongs to the browser spec. */
export async function ready(request: APIRequestContext, tag: Exclude<UserTag, 'c'>): Promise<Session> {
  const u = user(tag)
  const existing = await passwordSignIn(request, u)
  if (existing !== null) return existing
  await setPassword(request, await acceptInvite(request, u), u.password)
  const session = await passwordSignIn(request, u)
  if (session === null) throw new Error(`the user '${tag}' still cannot sign in after the invite was accepted and the password set`)
  return session
}
