import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthError } from './gotrue.ts'
import { createSession, jwtExpiryMs, type Session } from './session.ts'
import { fakeGoTrue, fakeJwt, fakeTokens } from '../test/helpers.tsx'

const START = new Date('2026-12-01T10:00:00Z')
const nowSeconds = () => Math.floor(Date.now() / 1000)

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(START)
})
afterEach(() => {
  vi.useRealTimers()
})

function tokensExpiringIn(seconds: number, refreshToken = 'refresh-1') {
  return { accessToken: fakeJwt(nowSeconds() + seconds), refreshToken, expiresIn: seconds }
}

async function signedIn(expiresInSeconds = 600) {
  const gotrue = fakeGoTrue({ signInWithPassword: vi.fn(async () => tokensExpiringIn(expiresInSeconds)) })
  const session = createSession()
  session.configure(gotrue)
  await session.signIn('a@b.test', 'pw')
  return { session, gotrue }
}

describe('jwtExpiryMs', () => {
  it('reads exp (seconds) as milliseconds, and is null for anything that is not a readable JWT', () => {
    expect(jwtExpiryMs(fakeJwt(1_800_000_000))).toBe(1_800_000_000_000)
    expect(jwtExpiryMs('not-a-jwt')).toBeNull()
    expect(jwtExpiryMs('a.b.c')).toBeNull()
    expect(jwtExpiryMs(`x.${btoa('{"exp":"soon"}')}.y`)).toBeNull()
  })
})

describe('signing in', () => {
  it('holds the tokens in memory and says signed in; the access token is readable, nothing else', async () => {
    const { session, gotrue } = await signedIn()
    expect(session.getSnapshot()).toEqual({ status: 'signed-in', setupKind: null, endedBecause: null })
    expect(session.getAccessToken()).toMatch(/^ey/)
    expect(gotrue.signInWithPassword).toHaveBeenCalledWith('a@b.test', 'pw')
    expect(Object.keys(session).sort()).not.toContain('getRefreshToken') // the refresh token has no reader
  })

  it('a refused sign in leaves nobody signed in and rethrows the error', async () => {
    const session = createSession()
    session.configure(fakeGoTrue({ signInWithPassword: vi.fn(async () => Promise.reject(new AuthError('invalid_credentials', 400))) }))
    await expect(session.signIn('a@b.test', 'bad')).rejects.toMatchObject({ kind: 'invalid_credentials' })
    expect(session.getSnapshot().status).toBe('signed-out')
    expect(session.getAccessToken()).toBeNull()
  })

  it('before the sign-in service is configured, signing in is "unavailable", not a crash', async () => {
    await expect(createSession().signIn('a@b.test', 'x')).rejects.toMatchObject({ kind: 'unavailable' })
  })

  it('tells its subscribers about each change, and stops telling those who left', async () => {
    const session = createSession()
    session.configure(fakeGoTrue())
    const l = vi.fn()
    const off = session.subscribe(l)
    await session.signIn('a@b.test', 'pw')
    expect(l).toHaveBeenCalledTimes(1)
    off()
    session.clear()
    expect(l).toHaveBeenCalledTimes(1)
  })
})

describe('the silent refresh', () => {
  it('runs a minute before the token expires, and not before', async () => {
    const { session, gotrue } = await signedIn(600)
    await vi.advanceTimersByTimeAsync(538_000) // 9 minutes 58 seconds: 2 s short of "a minute before"
    expect(gotrue.refresh).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(2_000)
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
    expect(gotrue.refresh).toHaveBeenCalledWith('refresh-1')
    expect(session.getSnapshot().status).toBe('signed-in')
  })

  it('takes the new tokens and plans the next refresh from them', async () => {
    const gotrue = fakeGoTrue({
      signInWithPassword: vi.fn(async () => tokensExpiringIn(600)),
      refresh: vi.fn(async () => tokensExpiringIn(600, 'refresh-2')),
    })
    const session = createSession()
    session.configure(gotrue)
    await session.signIn('a@b.test', 'pw')
    await vi.advanceTimersByTimeAsync(540_000)
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(540_000)
    expect(gotrue.refresh).toHaveBeenCalledTimes(2)
    expect(gotrue.refresh).toHaveBeenLastCalledWith('refresh-2') // the rotated token, not the first
  })

  it('uses expires_in when the access token carries no readable exp', async () => {
    const gotrue = fakeGoTrue({ signInWithPassword: vi.fn(async () => ({ accessToken: 'opaque', refreshToken: 'r', expiresIn: 300 })) })
    const session = createSession()
    session.configure(gotrue)
    await session.signIn('a@b.test', 'pw')
    await vi.advanceTimersByTimeAsync(239_000)
    expect(gotrue.refresh).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1_000)
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
  })

  it('a token that is already inside the last minute is refreshed at once', async () => {
    const gotrue = fakeGoTrue({ signInWithPassword: vi.fn(async () => tokensExpiringIn(30)) })
    const session = createSession()
    session.configure(gotrue)
    await session.signIn('a@b.test', 'pw')
    await vi.advanceTimersByTimeAsync(0)
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
  })

  it('two refreshes at once are one request (the service rotates the refresh token: a second use of the old one would end the session)', async () => {
    const { session, gotrue } = await signedIn()
    const [a, b] = await Promise.all([session.refresh(), session.refresh()])
    expect(a).toBe('refreshed')
    expect(b).toBe('refreshed')
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
  })

  it('a refresh the service refuses signs the user out, says why, and tells the API client to drop what is in flight', async () => {
    const { session, gotrue } = await signedIn()
    vi.mocked(gotrue.refresh).mockRejectedValueOnce(new AuthError('session_ended', 400))
    const cleared = vi.fn()
    session.onCleared(cleared)
    expect(await session.refresh()).toBe('rejected')
    expect(session.getSnapshot()).toEqual({ status: 'signed-out', setupKind: null, endedBecause: 'expired' })
    expect(session.getAccessToken()).toBeNull()
    expect(cleared).toHaveBeenCalledTimes(1)
  })

  it('paired control: a refresh that could not be MADE keeps the session and tries again in a moment', async () => {
    const { session, gotrue } = await signedIn(600)
    vi.mocked(gotrue.refresh).mockRejectedValueOnce(new AuthError('network', 0))
    expect(await session.refresh()).toBe('unreachable')
    expect(session.getSnapshot().status).toBe('signed-in')
    expect(session.getAccessToken()).not.toBeNull()
    await vi.advanceTimersByTimeAsync(15_000)
    expect(gotrue.refresh).toHaveBeenCalledTimes(2) // the retry
    expect(session.getSnapshot().status).toBe('signed-in')
  })

  it('a refresh with nobody signed in is "rejected" and calls nothing', async () => {
    const gotrue = fakeGoTrue()
    const session = createSession()
    session.configure(gotrue)
    expect(await session.refresh()).toBe('rejected')
    expect(gotrue.refresh).not.toHaveBeenCalled()
  })

  it('an answer that arrives after the user signed out belongs to nobody', async () => {
    let release: (t: ReturnType<typeof tokensExpiringIn>) => void = () => {}
    const gotrue = fakeGoTrue({
      signInWithPassword: vi.fn(async () => tokensExpiringIn(600)),
      refresh: vi.fn(() => new Promise<ReturnType<typeof tokensExpiringIn>>((resolve) => (release = resolve))),
    })
    const session = createSession()
    session.configure(gotrue)
    await session.signIn('a@b.test', 'pw')
    const pending = session.refresh()
    await session.signOut()
    release(tokensExpiringIn(600, 'late'))
    expect(await pending).toBe('rejected')
    expect(session.getAccessToken()).toBeNull()
    expect(session.getSnapshot().status).toBe('signed-out')
  })
})

describe('signing out', () => {
  it('clears everything FIRST, and only then tells the service, with the token it had', async () => {
    const { session, gotrue } = await signedIn()
    const token = session.getAccessToken()
    let tokenWhenServiceWasTold: string | null | undefined
    vi.mocked(gotrue.signOut).mockImplementation(async () => {
      tokenWhenServiceWasTold = session.getAccessToken()
    })
    const done = session.signOut()
    expect(session.getSnapshot().status).toBe('signed-out') // before the service has answered
    expect(session.getAccessToken()).toBeNull()
    await done
    expect(gotrue.signOut).toHaveBeenCalledWith(token)
    expect(tokenWhenServiceWasTold).toBeNull()
  })

  it('stops the refresh timer (no request after a sign out)', async () => {
    const { session, gotrue } = await signedIn(600)
    await session.signOut()
    await vi.advanceTimersByTimeAsync(3_600_000)
    expect(gotrue.refresh).not.toHaveBeenCalled()
  })

  it('works even if the service cannot be told', async () => {
    const { session, gotrue } = await signedIn()
    vi.mocked(gotrue.signOut).mockRejectedValueOnce(new AuthError('network', 0))
    await expect(session.signOut()).resolves.toBeUndefined()
    expect(session.getSnapshot().status).toBe('signed-out')
  })

  it('remembers why when the cause is an expired session, and forgets it on a plain sign out', async () => {
    const a = await signedIn()
    await a.session.signOut({ because: 'expired' })
    expect(a.session.getSnapshot().endedBecause).toBe('expired')
    const b = await signedIn()
    await b.session.signOut()
    expect(b.session.getSnapshot().endedBecause).toBeNull()
  })

  it('clear() wipes without telling the service (the page came back from the back-forward cache)', async () => {
    const { session, gotrue } = await signedIn()
    session.clear()
    expect(session.getAccessToken()).toBeNull()
    expect(session.getSnapshot().status).toBe('signed-out')
    expect(gotrue.signOut).not.toHaveBeenCalled()
  })
})

describe('the session of an invitation or reset link', () => {
  function adopted(kind: 'invite' | 'recovery' = 'invite'): { session: Session; gotrue: ReturnType<typeof fakeGoTrue> } {
    const gotrue = fakeGoTrue()
    const session = createSession()
    session.configure(gotrue)
    session.adopt(fakeTokens(), kind)
    return { session, gotrue }
  }

  it('is "password required", not signed in, until a password is set', () => {
    const { session } = adopted('recovery')
    expect(session.getSnapshot()).toEqual({ status: 'password-required', setupKind: 'recovery', endedBecause: null })
    expect(session.getAccessToken()).not.toBeNull()
  })

  it('setting the password calls the service with the access token and the new password, then the user is signed in', async () => {
    const { session, gotrue } = adopted()
    const token = session.getAccessToken()
    await session.setPassword('a brand new long password')
    expect(gotrue.updatePassword).toHaveBeenCalledWith(token, 'a brand new long password')
    expect(session.getSnapshot()).toEqual({ status: 'signed-in', setupKind: null, endedBecause: null })
  })

  it('paired control: a refused password (too weak) leaves the session waiting for another try', async () => {
    const { session, gotrue } = adopted()
    vi.mocked(gotrue.updatePassword).mockRejectedValueOnce(new AuthError('weak_password', 422))
    await expect(session.setPassword('short')).rejects.toMatchObject({ kind: 'weak_password' })
    expect(session.getSnapshot().status).toBe('password-required')
    expect(session.getAccessToken()).not.toBeNull()
  })

  it('a link that has died ends the session', async () => {
    const { session, gotrue } = adopted()
    vi.mocked(gotrue.updatePassword).mockRejectedValueOnce(new AuthError('session_ended', 401))
    await expect(session.setPassword('a brand new long password')).rejects.toMatchObject({ kind: 'session_ended' })
    expect(session.getSnapshot()).toMatchObject({ status: 'signed-out', endedBecause: 'expired' })
  })

  it('without a session there is nothing to set a password on', async () => {
    const session = createSession()
    session.configure(fakeGoTrue())
    await expect(session.setPassword('a brand new long password')).rejects.toMatchObject({ kind: 'session_ended' })
  })

  it('is refreshed like any session', async () => {
    const gotrue = fakeGoTrue()
    const session = createSession()
    session.configure(gotrue)
    session.adopt(tokensExpiringIn(300), 'invite')
    await vi.advanceTimersByTimeAsync(240_000)
    expect(gotrue.refresh).toHaveBeenCalledTimes(1)
  })

  it('can ask for a reset link, and says "unavailable" before it is configured', async () => {
    const { session, gotrue } = adopted()
    await session.recover('a@b.test')
    expect(gotrue.recover).toHaveBeenCalledWith('a@b.test')
    await expect(createSession().recover('a@b.test')).rejects.toMatchObject({ kind: 'unavailable' })
  })
})
