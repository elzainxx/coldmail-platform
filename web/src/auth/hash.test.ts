import { afterEach, describe, expect, it } from 'vitest'
import { consumeAuthHash, parseAuthHash } from './hash.ts'

afterEach(() => {
  window.history.replaceState(null, '', '/')
})

const INVITE = '#access_token=AAA.BBB.CCC&expires_at=1800000000&expires_in=3600&refresh_token=REFRESH-1&token_type=bearer&type=invite'

describe('parseAuthHash', () => {
  it('reads an invitation link', () => {
    expect(parseAuthHash(INVITE)).toEqual({
      kind: 'session',
      setup: 'invite',
      tokens: { accessToken: 'AAA.BBB.CCC', refreshToken: 'REFRESH-1', expiresIn: 3600 },
    })
  })

  it('reads a reset link', () => {
    const landing = parseAuthHash(INVITE.replace('type=invite', 'type=recovery'))
    expect(landing).toMatchObject({ kind: 'session', setup: 'recovery' })
  })

  it('reads a link that has expired or been used as an error, without keeping GoTrue\'s words', () => {
    const landing = parseAuthHash('#error=access_denied&error_code=otp_expired&error_description=Email+link+is+invalid+or+has+expired')
    expect(landing).toEqual({ kind: 'error' })
  })

  it('refuses a session of a kind this app does not take (sign-up, magic link, email change)', () => {
    for (const type of ['signup', 'magiclink', 'email_change', 'invite2', '']) {
      expect(parseAuthHash(INVITE.replace('type=invite', `type=${type}`)), type).toEqual({ kind: 'error' })
    }
  })

  it('refuses a link with a token missing, or one that is absurdly long', () => {
    expect(parseAuthHash('#access_token=AAA&type=invite')).toEqual({ kind: 'error' })
    expect(parseAuthHash('#refresh_token=R&access_token=&type=invite')).toEqual({ kind: 'error' })
    expect(parseAuthHash(`#access_token=${'x'.repeat(9000)}&refresh_token=R&type=invite`)).toEqual({ kind: 'error' })
  })

  it('is null for a hash that is not an auth landing at all', () => {
    for (const h of ['', '#', '#main', '#section=2', '#/jobs']) expect(parseAuthHash(h), h).toBeNull()
  })

  it('a missing or silly expires_in is an hour (it only plans the refresh)', () => {
    const landing = parseAuthHash('#access_token=A&refresh_token=R&type=invite&expires_in=banana')
    expect(landing).toMatchObject({ tokens: { expiresIn: 3600 } })
  })
})

describe('consumeAuthHash', () => {
  it('returns the session AND takes the hash out of the address bar at once, keeping the path and the query', () => {
    window.history.replaceState(null, '', `/accept?x=1${INVITE}`)
    const entries = window.history.length
    const landing = consumeAuthHash()
    expect(landing).toMatchObject({ kind: 'session', setup: 'invite' })
    expect(window.location.hash).toBe('')
    expect(window.location.pathname + window.location.search).toBe('/accept?x=1')
    expect(window.location.href).not.toContain('access_token')
    expect(window.location.href).not.toContain('REFRESH-1')
    expect(window.history.length).toBe(entries) // replaced, not pushed: the tokens are not in a history entry either
  })

  it('removes the hash of an expired-link error too', () => {
    window.history.replaceState(null, '', '/#error=access_denied&error_code=otp_expired')
    expect(consumeAuthHash()).toEqual({ kind: 'error' })
    expect(window.location.hash).toBe('')
  })

  it('paired control: a hash that is not an auth landing is left where it is', () => {
    window.history.replaceState(null, '', '/page#section-2')
    expect(consumeAuthHash()).toBeNull()
    expect(window.location.hash).toBe('#section-2')
  })

  it('a second call finds nothing: the tokens are only ever read once', () => {
    window.history.replaceState(null, '', `/${INVITE}`)
    expect(consumeAuthHash()).not.toBeNull()
    expect(consumeAuthHash()).toBeNull()
  })
})
