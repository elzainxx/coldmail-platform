import { describe, expect, it, vi } from 'vitest'
import { AUTH_ERROR_KINDS, AuthError, classifyAuthFailure, createGoTrue, gotrueBase, type AuthErrorKind } from './gotrue.ts'
import { authErrorKey, authErrorKeyOf } from './errors.ts'
import { catalogue } from '../copy/index.ts'
import { fakeJwt } from '../test/helpers.tsx'

const CONFIG = { supabase_url: 'https://auth.example.test/auth/v1', anon_key: 'public-anon-key' }

function reply(body: unknown, status = 200): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), { status })
}

function setup(response: Response | (() => Promise<Response>)) {
  const fetch = vi.fn(async () => (typeof response === 'function' ? response() : response)) as unknown as typeof globalThis.fetch & ReturnType<typeof vi.fn>
  return { client: createGoTrue(CONFIG, fetch), fetch: fetch as unknown as ReturnType<typeof vi.fn> }
}

function call(fetch: ReturnType<typeof vi.fn>, n = 0) {
  const [url, init] = fetch.mock.calls[n] as [string, RequestInit]
  return { url, init, headers: init.headers as Record<string, string>, body: init.body === undefined ? undefined : JSON.parse(init.body as string) }
}

describe('gotrueBase', () => {
  it('uses the URL as given, without a trailing slash', () => {
    expect(gotrueBase('https://auth.example.test/auth/v1')).toBe('https://auth.example.test/auth/v1')
    expect(gotrueBase('https://auth.example.test/auth/v1/')).toBe('https://auth.example.test/auth/v1')
    expect(gotrueBase('http://gotrue:9999')).toBe('http://gotrue:9999')
    expect(gotrueBase('http://gotrue:9999/')).toBe('http://gotrue:9999')
  })

  it('adds /auth/v1 to the bare origin of a hosted Supabase project, and only to that', () => {
    expect(gotrueBase('https://abcdefgh.supabase.co')).toBe('https://abcdefgh.supabase.co/auth/v1')
    expect(gotrueBase('https://abcdefgh.supabase.co/')).toBe('https://abcdefgh.supabase.co/auth/v1')
    expect(gotrueBase('https://abcdefgh.supabase.co/auth/v1')).toBe('https://abcdefgh.supabase.co/auth/v1')
    expect(gotrueBase('https://login.example.test')).toBe('https://login.example.test') // a self-hosted or test GoTrue answers at the root
  })
})

describe('the five routes', () => {
  it('sign in with a password: POST /token?grant_type=password with the anon key and no bearer', async () => {
    const access = fakeJwt(2_000_000_000)
    const { client, fetch } = setup(reply({ access_token: access, refresh_token: 'r1', expires_in: 3600, token_type: 'bearer', user: { id: 'u' } }))
    const tokens = await client.signInWithPassword('a@b.test', 'secret')
    const c = call(fetch)
    expect(c.url).toBe('https://auth.example.test/auth/v1/token?grant_type=password')
    expect(c.init.method).toBe('POST')
    expect(c.headers['apikey']).toBe('public-anon-key')
    expect(c.headers['Authorization']).toBeUndefined()
    expect(c.body).toEqual({ email: 'a@b.test', password: 'secret' })
    expect(tokens).toEqual({ accessToken: access, refreshToken: 'r1', expiresIn: 3600 })
    // nothing of the browser's ambient state goes along
    expect(c.init.credentials).toBe('omit')
    expect(c.init.cache).toBe('no-store')
    expect(c.init.referrerPolicy).toBe('no-referrer')
  })

  it('refresh: POST /token?grant_type=refresh_token with the refresh token', async () => {
    const { client, fetch } = setup(reply({ access_token: 'a2', refresh_token: 'r2', expires_in: 1800 }))
    const tokens = await client.refresh('r1')
    const c = call(fetch)
    expect(c.url).toBe('https://auth.example.test/auth/v1/token?grant_type=refresh_token')
    expect(c.body).toEqual({ refresh_token: 'r1' })
    expect(tokens).toEqual({ accessToken: 'a2', refreshToken: 'r2', expiresIn: 1800 })
  })

  it('recover: POST /recover with the address', async () => {
    const { client, fetch } = setup(reply({}))
    await client.recover('a@b.test')
    const c = call(fetch)
    expect(c.url).toBe('https://auth.example.test/auth/v1/recover')
    expect(c.body).toEqual({ email: 'a@b.test' })
    expect(c.headers['apikey']).toBe('public-anon-key')
  })

  it('set a password: PUT /user with the access token as bearer', async () => {
    const { client, fetch } = setup(reply({ id: 'u' }))
    await client.updatePassword('the-access-token', 'a new long password')
    const c = call(fetch)
    expect(c.url).toBe('https://auth.example.test/auth/v1/user')
    expect(c.init.method).toBe('PUT')
    expect(c.headers['Authorization']).toBe('Bearer the-access-token')
    expect(c.headers['apikey']).toBe('public-anon-key')
    expect(c.body).toEqual({ password: 'a new long password' })
  })

  it('sign out: POST /logout for this session only, with the access token as bearer; a 204 is fine', async () => {
    const { client, fetch } = setup(new Response(null, { status: 204 }))
    await client.signOut('the-access-token')
    const c = call(fetch)
    expect(c.url).toBe('https://auth.example.test/auth/v1/logout?scope=local')
    expect(c.init.method).toBe('POST')
    expect(c.headers['Authorization']).toBe('Bearer the-access-token')
  })

  it('an answer without tokens is an error, not a session', async () => {
    const { client } = setup(reply({ user: {} }))
    await expect(client.signInWithPassword('a@b.test', 'x')).rejects.toMatchObject({ kind: 'unknown' })
  })

  it('a missing expires_in means an hour (it is only used to plan the refresh)', async () => {
    const { client } = setup(reply({ access_token: 'a', refresh_token: 'r' }))
    expect((await client.refresh('r')).expiresIn).toBe(3600)
  })
})

describe('failures become kinds, and the service\'s own words are dropped', () => {
  const cases: Array<[string, number, unknown, AuthErrorKind]> = [
    ['wrong password (error_code)', 400, { code: 400, error_code: 'invalid_credentials', msg: 'Invalid login credentials' }, 'invalid_credentials'],
    ['wrong password (older shape)', 400, { error: 'invalid_grant', error_description: 'Invalid login credentials' }, 'invalid_credentials'],
    ['unknown user', 400, { error_code: 'user_not_found', msg: 'User not found' }, 'invalid_credentials'],
    ['not confirmed', 400, { error_code: 'email_not_confirmed', msg: 'Email not confirmed' }, 'email_not_confirmed'],
    ['banned', 403, { error_code: 'user_banned', msg: 'User is banned' }, 'account_disabled'],
    ['rate limit by status', 429, { msg: 'slow down' }, 'rate_limited'],
    ['rate limit by code', 400, { error_code: 'over_request_rate_limit' }, 'rate_limited'],
    ['email rate limit', 429, { error_code: 'over_email_send_rate_limit' }, 'rate_limited'],
    ['weak password', 422, { error_code: 'weak_password', msg: 'Password should be at least 12 characters' }, 'weak_password'],
    ['weak password (text only)', 422, { msg: 'Password should be at least 6 characters.' }, 'weak_password'],
    ['same password', 422, { error_code: 'same_password', msg: 'New password should be different' }, 'same_password'],
    ['expired link', 403, { error_code: 'otp_expired' }, 'session_ended'],
    ['used refresh token', 400, { error_code: 'refresh_token_already_used' }, 'session_ended'],
    ['refresh token not found', 400, { error_code: 'refresh_token_not_found' }, 'session_ended'],
    ['bad jwt', 401, { error_code: 'bad_jwt' }, 'session_ended'],
    ['invalid_grant that is not about credentials', 400, { error: 'invalid_grant', error_description: 'Refresh Token Not Found' }, 'session_ended'],
    ['a bare 401', 401, undefined, 'session_ended'],
    ['a server error', 500, { msg: 'boom' }, 'unavailable'],
    ['a gateway error with no body', 502, undefined, 'unavailable'],
    ['something never seen', 418, { error_code: 'teapot' }, 'unknown'],
  ]

  it.each(cases)('%s -> the right kind', async (_name, status, body, kind) => {
    expect(classifyAuthFailure(status, body)).toBe(kind)
    const { client } = setup(reply(body, status))
    const err = (await client.signInWithPassword('a@b.test', 'x').catch((e: unknown) => e)) as AuthError
    expect(err).toBeInstanceOf(AuthError)
    expect(err.kind).toBe(kind)
    expect(err.status).toBe(status)
  })

  it('never carries the service\'s text', async () => {
    const { client } = setup(reply({ error_code: 'invalid_credentials', msg: 'Invalid login credentials SECRET-TEXT' }, 400))
    const err = (await client.signInWithPassword('a@b.test', 'x').catch((e: unknown) => e)) as AuthError
    expect(err.message).toBe('invalid_credentials')
    expect(JSON.stringify({ ...err, message: err.message })).not.toContain('SECRET-TEXT')
  })

  it('a request that cannot be made is network', async () => {
    const { client } = setup(() => Promise.reject(new TypeError('Failed to fetch')))
    await expect(client.signInWithPassword('a@b.test', 'x')).rejects.toMatchObject({ kind: 'network', status: 0 })
  })

  it('every kind has a line in the catalogue, and no line is shared with another kind', () => {
    expect(AUTH_ERROR_KINDS.length).toBe(10)
    const lines = AUTH_ERROR_KINDS.map((k) => authErrorKey(k))
    for (const key of lines) expect(catalogue[key], key).toBeTruthy()
    expect(new Set(lines).size).toBe(lines.length)
    expect(authErrorKeyOf(new Error('anything else'))).toBe('auth.error.unknown')
    expect(authErrorKeyOf(new AuthError('rate_limited', 429))).toBe('auth.error.rate_limited')
  })
})

describe('recover tells nothing about the address', () => {
  it.each([400, 404, 422, 429])('swallows a %i answer (it would say something about the address)', async (status) => {
    const { client } = setup(reply({ error_code: 'over_email_send_rate_limit', msg: 'x' }, status))
    await expect(client.recover('nobody@example.test')).resolves.toBeUndefined()
  })

  it('but reports a service failure and a request that could not be made', async () => {
    await expect(setup(reply({}, 500)).client.recover('a@b.test')).rejects.toMatchObject({ kind: 'unavailable' })
    await expect(setup(() => Promise.reject(new TypeError('x'))).client.recover('a@b.test')).rejects.toMatchObject({ kind: 'network' })
  })

  it('paired control: sign in does NOT swallow the same 400', async () => {
    const { client } = setup(reply({ error_code: 'invalid_credentials' }, 400))
    await expect(client.signInWithPassword('a@b.test', 'x')).rejects.toBeInstanceOf(AuthError)
  })
})
