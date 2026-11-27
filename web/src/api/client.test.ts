import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { RefreshOutcome } from '../auth/session.ts'
import { getConnectivity, resetConnectivity } from './connectivity.ts'
import { buildUrl, createApi, type ApiSession, type FetchLike } from './client.ts'
import { ApiError, isAbortError } from './errors.ts'

function envelope(code: string, details?: Record<string, unknown>) {
  return { error: { code, message: 'SERVER SENTENCE NOT FOR USERS', request_id: 'req_abc', ...(details ? { details } : {}) } }
}

function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })
}

interface FakeSession extends ApiSession {
  token: string | null
  refresh: ReturnType<typeof vi.fn<() => Promise<RefreshOutcome>>>
  signOut: ReturnType<typeof vi.fn<(opts?: { because?: 'expired' }) => Promise<void>>>
  clearedListeners: Array<() => void>
}

function fakeSession(token: string | null = 'token-1', outcome: RefreshOutcome = 'refreshed', nextToken = 'token-2'): FakeSession {
  const s: FakeSession = {
    token,
    clearedListeners: [],
    getAccessToken: () => s.token,
    refresh: vi.fn(async () => {
      if (outcome === 'refreshed') s.token = nextToken
      return outcome
    }),
    signOut: vi.fn(async () => {
      s.token = null
    }),
    onCleared: (l) => {
      s.clearedListeners.push(l)
      return () => {}
    },
  }
  return s
}

function headerOf(init: RequestInit | undefined, name: string): string | undefined {
  return (init?.headers as Record<string, string> | undefined)?.[name]
}

beforeEach(() => {
  resetConnectivity()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('buildUrl', () => {
  it('encodes the query and leaves out what is empty (the API reads an empty value as an absent one)', () => {
    expect(buildUrl('', '/v1/jobs', { status: 'active', cursor: undefined, q: '', limit: 25, a: null, flag: false })).toBe('/v1/jobs?status=active&limit=25&flag=false')
    expect(buildUrl('', '/v1/x', { q: 'a b&c' })).toBe('/v1/x?q=a+b%26c')
    expect(buildUrl('', '/v1/x')).toBe('/v1/x')
  })

  it('refuses a path that does not start with a slash (a full URL would send the token elsewhere)', () => {
    expect(() => buildUrl('', 'https://evil.example.test/x')).toThrow()
    expect(() => buildUrl('', 'v1/jobs')).toThrow()
  })
})

describe('requests', () => {
  it('sends the bearer token and asks for JSON', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({ ok: 1 }))
    const api = createApi({ session: fakeSession('abc'), fetch })
    await api.get('/v1/me')
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe('/v1/me')
    expect(init.method).toBe('GET')
    expect(headerOf(init, 'Authorization')).toBe('Bearer abc')
    expect(headerOf(init, 'Accept')).toBe('application/json')
    expect(init.credentials).toBe('same-origin')
    expect(init.cache).toBe('no-store')
  })

  it('paired control: with no session, or with auth: false, no Authorization header is sent', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({}))
    await createApi({ session: fakeSession(null), fetch }).get('/v1/me')
    await createApi({ session: fakeSession('abc'), fetch }).get('/v1/me', { auth: false })
    for (const call of fetch.mock.calls) expect(headerOf(call[1] as unknown as RequestInit, 'Authorization')).toBeUndefined()
  })

  it('sends JSON for an object, with the content type', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({ job: {} }, 201))
    const api = createApi({ session: fakeSession(), fetch })
    await api.post('/v1/jobs', { module: 'm1', rows: [{ email: 'a@b.test' }] })
    const init = fetch.mock.calls[0]?.[1] as unknown as RequestInit
    expect(init.method).toBe('POST')
    expect(headerOf(init, 'Content-Type')).toBe('application/json')
    expect(JSON.parse(init.body as string)).toEqual({ module: 'm1', rows: [{ email: 'a@b.test' }] })
  })

  it('sends a string as text/csv, and a File as the File itself (the browser streams it from disk)', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({}, 201))
    const api = createApi({ session: fakeSession(), fetch })
    await api.post('/v1/jobs', 'email\na@b.test\n', { query: { module: 'm1' } })
    const text = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(text[0]).toBe('/v1/jobs?module=m1')
    expect(headerOf(text[1], 'Content-Type')).toBe('text/csv')
    expect(text[1].body).toBe('email\na@b.test\n')

    const file = new File(['email\na@b.test\n'], 'list.csv', { type: 'text/csv' })
    await api.post('/v1/jobs', file, { query: { module: 'm1', email_column: 'Work email' } })
    const sent = fetch.mock.calls[1] as unknown as [string, RequestInit]
    expect(sent[1].body).toBe(file) // the very same File object, not a copy and not a string
    expect(headerOf(sent[1], 'Content-Type')).toBe('text/csv')
    expect(sent[0]).toBe('/v1/jobs?module=m1&email_column=Work+email')
  })

  it('paired control: a JSON object is NOT sent as the object itself', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({}))
    const api = createApi({ session: fakeSession(), fetch })
    const body = { a: 1 }
    await api.post('/v1/x', body)
    expect((fetch.mock.calls[0]?.[1] as unknown as RequestInit).body).not.toBe(body)
  })

  it('a File with another type is sent under the content type the caller names', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({}))
    const api = createApi({ session: fakeSession(), fetch })
    await api.post('/v1/suppressions/import', new File(['x'], 'a.txt', { type: 'text/plain' }), { contentType: 'text/csv' })
    expect(headerOf(fetch.mock.calls[0]?.[1] as unknown as RequestInit, 'Content-Type')).toBe('text/csv')
  })

  it('puts the Idempotency-Key on a confirm', async () => {
    const fetch = vi.fn<FetchLike>(async () => json({}))
    await createApi({ session: fakeSession(), fetch }).post('/v1/jobs/x/confirm', { fingerprint: 'f' }, { idempotencyKey: 'confirm-1' })
    expect(headerOf(fetch.mock.calls[0]?.[1] as unknown as RequestInit, 'Idempotency-Key')).toBe('confirm-1')
  })

  it('put and del use their verbs; a 204 is undefined', async () => {
    const fetch = vi.fn<FetchLike>(async () => new Response(null, { status: 204 }))
    const api = createApi({ session: fakeSession(), fetch })
    expect(await api.put('/v1/settings', { vendor_fallback_opt_out: true })).toBeUndefined()
    expect(await api.del('/v1/x')).toBeUndefined()
    expect(fetch.mock.calls.map((c) => (c[1] as unknown as RequestInit).method)).toEqual(['PUT', 'DELETE'])
  })

  it('getText and getBlob return the body', async () => {
    const fetch = vi.fn<FetchLike>(async () => new Response('a,b\n1,2\n', { status: 200, headers: { 'Content-Type': 'text/csv' } }))
    const api = createApi({ session: fakeSession(), fetch })
    expect(await api.getText('/v1/jobs/x/results.csv')).toBe('a,b\n1,2\n')
    const blob = await api.getBlob('/v1/jobs/x/results.csv')
    expect(blob.size).toBe(8)
  })

  it('an answer that is not JSON where JSON was expected is bad_response, not an exception from JSON.parse', async () => {
    const api = createApi({ session: fakeSession(), fetch: vi.fn<FetchLike>(async () => new Response('<html>', { status: 200 })) })
    await expect(api.get('/v1/me')).rejects.toMatchObject({ code: 'bad_response', status: 200 })
  })
})

describe('errors', () => {
  it('maps the envelope to an ApiError and never keeps the server sentence', async () => {
    const fetch = vi.fn<FetchLike>(async () => json(envelope('insufficient_credits', { available: 5, needed: 9, shortfall: 4 }), 402))
    const err = await createApi({ session: fakeSession(), fetch }).get('/v1/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    const e = err as ApiError
    expect(e.status).toBe(402)
    expect(e.code).toBe('insufficient_credits')
    expect(e.details).toEqual({ available: 5, needed: 9, shortfall: 4 })
    expect(e.requestId).toBe('req_abc')
    expect(JSON.stringify({ ...e, message: e.message })).not.toContain('SERVER SENTENCE')
  })

  it('reads the reason of a not_allowed answer', async () => {
    const fetch = vi.fn<FetchLike>(async () => json(envelope('not_allowed', { reason: 'pause_not_supported' }), 409))
    const e = (await createApi({ session: fakeSession(), fetch }).post('/v1/jobs/x/pause').catch((x: unknown) => x)) as ApiError
    expect(e.reason).toBe('pause_not_supported')
  })

  it('an error body that is not the envelope is bad_response with the real status', async () => {
    const fetch = vi.fn<FetchLike>(async () => new Response('<h1>Bad gateway</h1>', { status: 502 }))
    await expect(createApi({ session: fakeSession(), fetch }).get('/v1/x')).rejects.toMatchObject({ code: 'bad_response', status: 502 })
  })

  it('a request that cannot be made is network_error (status 0) and the banner says offline; the next answer clears it', async () => {
    const fetch = vi.fn<FetchLike>()
    fetch.mockRejectedValueOnce(new TypeError('Failed to fetch'))
    const api = createApi({ session: fakeSession(), fetch })
    await expect(api.get('/v1/x')).rejects.toMatchObject({ code: 'network_error', status: 0 })
    expect(getConnectivity()).toBe('offline')
    fetch.mockResolvedValueOnce(json({}))
    await api.get('/v1/x')
    expect(getConnectivity()).toBe('ok')
  })

  it('a 503 puts the banner on "unavailable"', async () => {
    const fetch = vi.fn<FetchLike>(async () => json(envelope('module_unavailable'), 503))
    await expect(createApi({ session: fakeSession(), fetch }).get('/v1/x')).rejects.toMatchObject({ code: 'module_unavailable' })
    expect(getConnectivity()).toBe('unavailable')
  })
})

describe('a 401', () => {
  it('refreshes ONCE and retries the same request with the new token', async () => {
    const session = fakeSession('old', 'refreshed', 'new')
    const fetch = vi.fn<FetchLike>()
    fetch.mockResolvedValueOnce(json(envelope('unauthenticated'), 401))
    fetch.mockResolvedValueOnce(json({ email: 'a@b.test' }))
    const result = await createApi({ session, fetch }).get<{ email: string }>('/v1/me')
    expect(result.email).toBe('a@b.test')
    expect(session.refresh).toHaveBeenCalledTimes(1)
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(headerOf(fetch.mock.calls[0]?.[1] as unknown as RequestInit, 'Authorization')).toBe('Bearer old')
    expect(headerOf(fetch.mock.calls[1]?.[1] as unknown as RequestInit, 'Authorization')).toBe('Bearer new')
    expect(session.signOut).not.toHaveBeenCalled()
  })

  it('paired control: a request that is not refused never refreshes', async () => {
    const session = fakeSession()
    await createApi({ session, fetch: vi.fn<FetchLike>(async () => json({})) }).get('/v1/me')
    expect(session.refresh).not.toHaveBeenCalled()
  })

  it('keeps the same body for the retry (a File can be read again)', async () => {
    const session = fakeSession('old', 'refreshed', 'new')
    const file = new File(['email\na@b.test\n'], 'l.csv', { type: 'text/csv' })
    const fetch = vi.fn<FetchLike>()
    fetch.mockResolvedValueOnce(json(envelope('unauthenticated'), 401))
    fetch.mockResolvedValueOnce(json({}, 201))
    await createApi({ session, fetch }).post('/v1/jobs', file)
    expect((fetch.mock.calls[0]?.[1] as unknown as RequestInit).body).toBe(file)
    expect((fetch.mock.calls[1]?.[1] as unknown as RequestInit).body).toBe(file)
  })

  it('when the refresh is refused: the 401 is thrown, nothing is retried (the session has ended itself)', async () => {
    const session = fakeSession('old', 'rejected')
    const fetch = vi.fn<FetchLike>(async () => json(envelope('unauthenticated'), 401))
    await expect(createApi({ session, fetch }).get('/v1/me')).rejects.toMatchObject({ status: 401, code: 'unauthenticated' })
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(session.refresh).toHaveBeenCalledTimes(1)
  })

  it('when the retry is a 401 too: one refresh only, the session is ended, the 401 is thrown', async () => {
    const session = fakeSession('old', 'refreshed', 'new')
    const fetch = vi.fn<FetchLike>(async () => json(envelope('unauthenticated'), 401))
    await expect(createApi({ session, fetch }).get('/v1/me')).rejects.toMatchObject({ status: 401 })
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(session.refresh).toHaveBeenCalledTimes(1)
    expect(session.signOut).toHaveBeenCalledWith({ because: 'expired' })
  })

  it('when the refresh could not be made (offline): a network error, and nobody is signed out', async () => {
    const session = fakeSession('old', 'unreachable')
    const fetch = vi.fn<FetchLike>(async () => json(envelope('unauthenticated'), 401))
    await expect(createApi({ session, fetch }).get('/v1/me')).rejects.toMatchObject({ code: 'network_error' })
    expect(session.signOut).not.toHaveBeenCalled()
  })

  it('a route called with auth: false does not refresh on a 401', async () => {
    const session = fakeSession()
    const fetch = vi.fn<FetchLike>(async () => json(envelope('unauthenticated'), 401))
    await expect(createApi({ session, fetch }).get('/v1/x', { auth: false })).rejects.toMatchObject({ status: 401 })
    expect(session.refresh).not.toHaveBeenCalled()
  })
})

describe('a 503 busy', () => {
  const busy = () => json(envelope('busy'), 503, { 'Retry-After': '1' })

  it('waits for the Retry-After the platform sent and tries again', async () => {
    const sleep = vi.fn(async (_ms: number) => {})
    const fetch = vi.fn<FetchLike>()
    fetch.mockResolvedValueOnce(busy())
    fetch.mockResolvedValueOnce(json({ job: {} }))
    await createApi({ session: fakeSession(), fetch, sleep }).post('/v1/jobs/x/confirm', { fingerprint: 'f' }, { idempotencyKey: 'k' })
    expect(sleep).toHaveBeenCalledTimes(1)
    expect(sleep.mock.calls[0]?.[0]).toBe(1000)
    expect(fetch).toHaveBeenCalledTimes(2)
    // the retry is the same request, key included
    expect(headerOf(fetch.mock.calls[1]?.[1] as unknown as RequestInit, 'Idempotency-Key')).toBe('k')
  })

  it('gives up after two retries and throws the busy error', async () => {
    const sleep = vi.fn(async (_ms: number) => {})
    const fetch = vi.fn<FetchLike>(async () => busy())
    await expect(createApi({ session: fakeSession(), fetch, sleep }).post('/v1/jobs/x/confirm', {})).rejects.toMatchObject({ code: 'busy', status: 503 })
    expect(fetch).toHaveBeenCalledTimes(3)
    expect(sleep).toHaveBeenCalledTimes(2)
  })

  it('never waits longer than five seconds, whatever Retry-After says', async () => {
    const sleep = vi.fn(async (_ms: number) => {})
    const fetch = vi.fn<FetchLike>()
    fetch.mockResolvedValueOnce(json(envelope('busy'), 503, { 'Retry-After': '120' }))
    fetch.mockResolvedValueOnce(json({}))
    await createApi({ session: fakeSession(), fetch, sleep }).get('/v1/x')
    expect(sleep.mock.calls[0]?.[0]).toBe(5000)
  })

  it('paired control: any other 503 is returned at once, with the Retry-After on the error', async () => {
    const sleep = vi.fn(async (_ms: number) => {})
    const fetch = vi.fn<FetchLike>(async () => json(envelope('auth_unavailable'), 503, { 'Retry-After': '30' }))
    const e = (await createApi({ session: fakeSession(), fetch, sleep }).get('/v1/x').catch((x: unknown) => x)) as ApiError
    expect(e.code).toBe('auth_unavailable')
    expect(e.retryAfterSeconds).toBe(30)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(sleep).not.toHaveBeenCalled()
  })
})

describe('aborting', () => {
  function hangingFetch() {
    // like the real fetch: an aborted signal rejects it, at once if it is already aborted
    return vi.fn<FetchLike>(
      (_url, init) =>
        new Promise<Response>((_resolve, reject) => {
          const abort = () => reject(new DOMException('aborted', 'AbortError'))
          if (init.signal?.aborted) abort()
          else init.signal?.addEventListener('abort', abort)
        }),
    )
  }

  it('the caller\'s signal aborts the request, and the error is the browser\'s AbortError, not an ApiError', async () => {
    const controller = new AbortController()
    const api = createApi({ session: fakeSession(), fetch: hangingFetch() })
    const pending = api.get('/v1/x', { signal: controller.signal }).catch((e: unknown) => e)
    controller.abort()
    const e = await pending
    expect(isAbortError(e)).toBe(true)
    expect(e).not.toBeInstanceOf(ApiError)
    expect(getConnectivity()).toBe('ok') // an abort is not an outage
  })

  it('a signal that is already aborted ends the request at once, as an AbortError', async () => {
    const controller = new AbortController()
    controller.abort()
    const fetch = hangingFetch()
    const e = await createApi({ session: fakeSession(), fetch }).get('/v1/x', { signal: controller.signal }).catch((x: unknown) => x)
    expect(isAbortError(e)).toBe(true)
  })

  it('signing out (the session was cleared) aborts every request in flight', async () => {
    const session = fakeSession()
    const api = createApi({ session, fetch: hangingFetch() })
    const a = api.get('/v1/a').catch((e: unknown) => e)
    const b = api.get('/v1/b').catch((e: unknown) => e)
    expect(session.clearedListeners).toHaveLength(1)
    session.clearedListeners[0]?.()
    expect(isAbortError(await a)).toBe(true)
    expect(isAbortError(await b)).toBe(true)
  })

  it('paired control: without that call the request is still waiting', async () => {
    const fetch = hangingFetch()
    const api = createApi({ session: fakeSession(), fetch })
    let settled = false
    void api
      .get('/v1/a')
      .catch(() => {})
      .finally(() => {
        settled = true
      })
    await Promise.resolve()
    await Promise.resolve()
    expect(settled).toBe(false)
    api.abortAll() // clean up
  })
})
