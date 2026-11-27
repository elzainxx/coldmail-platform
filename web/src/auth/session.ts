// The session: the access and refresh tokens of the signed-in user, IN MEMORY ONLY.
//
//   - Nothing is ever written to localStorage, sessionStorage, a cookie or IndexedDB (session.test.ts spies on every one of them, and a source scan in
//     auth.test.ts allows storage in exactly one file: shell/theme.ts, which stores the theme and nothing else). A reload therefore signs the user out.
//   - The access token is refreshed silently a minute before it expires (the `exp` claim), and once more when the API answers 401 (api/client.ts).
//   - A refresh that GoTrue refuses signs the user out; one that could not be made (no network) keeps the session and tries again.
//   - Signing out clears everything first and tells GoTrue second (best effort).
//   - An invite or recovery link gives a session that may do one thing, choose a password (status 'password-required'); the app shows nothing else until it is done.
import { AuthError, isAuthError, type GoTrueClient, type TokenSet } from './gotrue.ts'

export type SessionStatus = 'signed-out' | 'password-required' | 'signed-in'
export type SetupKind = 'invite' | 'recovery'
export type RefreshOutcome = 'refreshed' | 'rejected' | 'unreachable'

export interface SessionSnapshot {
  readonly status: SessionStatus
  /** which link the password is being chosen from, while status is 'password-required' */
  readonly setupKind: SetupKind | null
  /** 'expired' when the session was ended because GoTrue refused it; the sign-in page says so */
  readonly endedBecause: 'expired' | null
}

export interface SessionOptions {
  now?: () => number
  /** how long before `exp` the silent refresh runs (default one minute) */
  leadMs?: number
  /** how long to wait before trying a refresh again after one that could not be made (default 15 seconds) */
  retryMs?: number
}

export interface Session {
  getSnapshot(): SessionSnapshot
  subscribe(listener: () => void): () => void
  /** Called every time the tokens are wiped (sign out, a refused refresh, bfcache): the API client aborts what is in flight. */
  onCleared(listener: () => void): () => void
  /** Give the session its sign-in client (once /config.json is loaded). null takes it away (tests). */
  configure(gotrue: GoTrueClient | null): void
  isConfigured(): boolean
  getAccessToken(): string | null
  signIn(email: string, password: string): Promise<void>
  /** Keep the tokens of an invite or recovery link (parsed from the URL hash by auth/hash.ts) and require a new password. */
  adopt(tokens: TokenSet, kind: SetupKind): void
  /** Ask for a password-reset email. Resolves for every answer about the address; rejects only when the request could not be made or the service failed. */
  recover(email: string): Promise<void>
  /** Set the password of the signed-in or password-required session; the session is then signed in. */
  setPassword(password: string): Promise<void>
  refresh(): Promise<RefreshOutcome>
  signOut(opts?: { because?: 'expired' }): Promise<void>
  /** Wipe everything now, without telling anybody (the page came back from the back-forward cache). */
  clear(): void
}

interface Held {
  access: string
  refresh: string
  expMs: number
}

const SIGNED_OUT: SessionSnapshot = { status: 'signed-out', setupKind: null, endedBecause: null }
const MAX_TIMER_MS = 2 ** 31 - 1

/** The `exp` claim of a JWT in milliseconds, or null when the token does not carry a readable one. Used only to schedule the refresh, never to trust the token. */
export function jwtExpiryMs(token: string): number | null {
  const part = token.split('.')[1]
  if (part === undefined) return null
  try {
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
    const json = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, '='))
    const exp = (JSON.parse(json) as { exp?: unknown }).exp
    return typeof exp === 'number' && Number.isFinite(exp) ? exp * 1000 : null
  } catch {
    return null
  }
}

export function createSession(options: SessionOptions = {}): Session {
  const now = options.now ?? (() => Date.now())
  const leadMs = options.leadMs ?? 60_000
  const retryMs = options.retryMs ?? 15_000

  let gotrue: GoTrueClient | null = null
  let held: Held | null = null
  let snapshot: SessionSnapshot = SIGNED_OUT
  let timer: ReturnType<typeof setTimeout> | null = null
  let refreshing: Promise<RefreshOutcome> | null = null
  const listeners = new Set<() => void>()
  const clearedListeners = new Set<() => void>()

  function setSnapshot(next: SessionSnapshot): void {
    if (next.status === snapshot.status && next.setupKind === snapshot.setupKind && next.endedBecause === snapshot.endedBecause) return
    snapshot = next
    for (const l of [...listeners]) l()
  }

  function stopTimer(): void {
    if (timer !== null) clearTimeout(timer)
    timer = null
  }

  function scheduleAt(delayMs: number): void {
    stopTimer()
    timer = setTimeout(() => {
      timer = null
      void refresh()
    }, Math.min(Math.max(0, delayMs), MAX_TIMER_MS))
  }

  function scheduleRefresh(): void {
    if (held === null) return
    scheduleAt(held.expMs - leadMs - now())
  }

  function hold(tokens: TokenSet): void {
    held = {
      access: tokens.accessToken,
      refresh: tokens.refreshToken,
      expMs: jwtExpiryMs(tokens.accessToken) ?? now() + tokens.expiresIn * 1000,
    }
    scheduleRefresh()
  }

  function wipe(because: 'expired' | null): void {
    held = null
    stopTimer()
    setSnapshot(because === null ? SIGNED_OUT : { status: 'signed-out', setupKind: null, endedBecause: because })
    for (const l of [...clearedListeners]) l()
  }

  function refresh(): Promise<RefreshOutcome> {
    if (refreshing !== null) return refreshing // one refresh at a time: GoTrue rotates the refresh token, so a second use of the old one would revoke the session
    const current = held
    if (current === null || gotrue === null) return Promise.resolve('rejected')
    const client = gotrue
    const run = (async (): Promise<RefreshOutcome> => {
      try {
        const next = await client.refresh(current.refresh)
        if (held !== current) return 'rejected' // signed out, or signed in again, while the request was out: this answer belongs to nobody
        hold(next)
        return 'refreshed'
      } catch (e) {
        if (held !== current) return 'rejected'
        if (isAuthError(e) && (e.kind === 'network' || e.kind === 'unavailable' || e.kind === 'rate_limited')) {
          scheduleAt(retryMs) // could not ask: keep the session, ask again
          return 'unreachable'
        }
        wipe('expired')
        return 'rejected'
      }
    })()
    refreshing = run
    void run.finally(() => {
      if (refreshing === run) refreshing = null
    })
    return run
  }

  return {
    getSnapshot: () => snapshot,
    subscribe(listener) {
      listeners.add(listener)
      return () => {
        listeners.delete(listener)
      }
    },
    onCleared(listener) {
      clearedListeners.add(listener)
      return () => {
        clearedListeners.delete(listener)
      }
    },
    configure(client) {
      gotrue = client
    },
    isConfigured: () => gotrue !== null,
    getAccessToken: () => held?.access ?? null,

    async signIn(email, password) {
      if (gotrue === null) throw new AuthError('unavailable', 0)
      const tokens = await gotrue.signInWithPassword(email, password)
      hold(tokens)
      setSnapshot({ status: 'signed-in', setupKind: null, endedBecause: null })
    },

    adopt(tokens, kind) {
      hold(tokens)
      setSnapshot({ status: 'password-required', setupKind: kind, endedBecause: null })
    },

    async setPassword(password) {
      const current = held
      if (current === null || gotrue === null) throw new AuthError('session_ended', 401)
      try {
        await gotrue.updatePassword(current.access, password)
      } catch (e) {
        if (isAuthError(e) && e.kind === 'session_ended') wipe('expired')
        throw e
      }
      if (held === current) setSnapshot({ status: 'signed-in', setupKind: null, endedBecause: null })
    },

    async recover(email) {
      if (gotrue === null) throw new AuthError('unavailable', 0)
      await gotrue.recover(email)
    },

    refresh,

    async signOut(opts) {
      const access = held?.access
      wipe(opts?.because ?? null)
      if (access !== undefined && gotrue !== null) {
        try {
          await gotrue.signOut(access)
        } catch {
          // The tokens are already gone from this page; if GoTrue could not be told, the refresh token simply expires there.
        }
      }
    },

    clear() {
      wipe(null)
    },
  }
}

/** The one session of the running app. */
export const session: Session = createSession()
