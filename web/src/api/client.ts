// The one door to the platform API: `api.get / post / put / del`, typed by the caller (`api.get<Job>(...)`).
//
//   - Authorization: Bearer <access token> from the in-memory session. A route that needs none passes { auth: false }.
//   - A 401 gets ONE silent refresh and ONE retry of the same request. If the refresh is refused, or the retry is a 401 too, the session is ended (the router
//     guard then sends the user to the sign-in page, which says why) and the caller gets the 401 ApiError. A refresh that could not be made (no network) is a network error and signs nobody out.
//   - `503 busy` ("another operation on your credits did not finish; nothing was written") is retried up to twice after the Retry-After the platform sends.
//     Every other 503 is returned at once (auth_unavailable asks for 30 seconds: the screen says so).
//   - Bodies: JSON for objects and arrays; `text/csv` for a string with { contentType: 'text/csv' }; a File or Blob is sent as it is, so the browser streams
//     it from disk (the CSV upload) and never holds the whole list in memory.
//   - Every error is an ApiError; its `message` is the code, never the server's sentence. An aborted request throws the browser's AbortError.
//   - Signing out aborts every request in flight.
import { session as defaultSession, type RefreshOutcome } from '../auth/session.ts'
import { ApiError, apiErrorFromResponse, isAbortError } from './errors.ts'
import { reportApiAnswered, reportNetworkFailure } from './connectivity.ts'

export type QueryValue = string | number | boolean | null | undefined
export type Query = Record<string, QueryValue>

export interface RequestOptions {
  query?: Query
  signal?: AbortSignal
  headers?: Record<string, string>
  /** the Idempotency-Key header of a confirm (api/idempotency.ts) */
  idempotencyKey?: string
  /** the content type of a string or Blob body; a JSON body is always application/json */
  contentType?: string
  /** false: send no Authorization header and do not refresh on a 401 */
  auth?: boolean
}

export interface ApiClient {
  get<T>(path: string, options?: RequestOptions): Promise<T>
  post<T>(path: string, body?: unknown, options?: RequestOptions): Promise<T>
  put<T>(path: string, body?: unknown, options?: RequestOptions): Promise<T>
  del<T>(path: string, options?: RequestOptions): Promise<T>
  /** A text answer (the CSV of results). */
  getText(path: string, options?: RequestOptions): Promise<string>
  /** A binary answer to hand to saveBlob (api/download.ts). */
  getBlob(path: string, options?: RequestOptions): Promise<Blob>
  /** Abort every request in flight (done on sign out). */
  abortAll(): void
}

export interface ApiSession {
  getAccessToken(): string | null
  refresh(): Promise<RefreshOutcome>
  signOut(opts?: { because?: 'expired' }): Promise<void>
  onCleared(listener: () => void): () => void
}

/** What the client needs of fetch: a URL string and the init. The browser's fetch is one; a test passes a stub. */
export type FetchLike = (url: string, init: RequestInit) => Promise<Response>

export interface ApiDeps {
  session: ApiSession
  fetch?: FetchLike
  /** wait `ms` (abortable); tests pass a fake */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>
  /** where the API lives; default is the page's own origin (the platform serves the app and the API from one origin) */
  baseUrl?: string
}

const MAX_BUSY_RETRIES = 2
const DEFAULT_BUSY_WAIT_MS = 1000
const MAX_BUSY_WAIT_MS = 5000

export function buildUrl(baseUrl: string, path: string, query?: Query): string {
  if (!path.startsWith('/')) throw new Error(`api path must start with "/": ${path}`)
  const params = new URLSearchParams()
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v === undefined || v === null || v === '') continue // the API treats an empty value as an absent one
    params.append(k, String(v))
  }
  const qs = params.toString()
  return `${baseUrl}${path}${qs === '' ? '' : `?${qs}`}`
}

function defaultSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('aborted', 'AbortError'))
      return
    }
    const onAbort = () => {
      clearTimeout(timer)
      reject(new DOMException('aborted', 'AbortError'))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

interface Encoded {
  body: BodyInit | undefined
  contentType: string | undefined
}

function encodeBody(body: unknown, contentType: string | undefined): Encoded {
  if (body === undefined) return { body: undefined, contentType: undefined }
  // A File is a Blob: it is handed to fetch as it is, so the browser streams it from disk.
  if (typeof Blob !== 'undefined' && body instanceof Blob) return { body, contentType: contentType ?? (body.type || 'text/csv') }
  if (typeof body === 'string') return { body, contentType: contentType ?? 'text/csv' }
  return { body: JSON.stringify(body), contentType: 'application/json' }
}

export function createApi(deps: ApiDeps): ApiClient {
  const { session } = deps
  const sleep = deps.sleep ?? defaultSleep
  const baseUrl = deps.baseUrl ?? ''
  const inflight = new Set<AbortController>()
  session.onCleared(() => abortAll())

  function abortAll(): void {
    for (const c of [...inflight]) c.abort()
    inflight.clear()
  }

  async function send(method: string, path: string, body: unknown, options: RequestOptions, accept: string): Promise<Response> {
    const authed = options.auth !== false
    const url = buildUrl(baseUrl, path, options.query)
    const encoded = encodeBody(body, options.contentType)
    const controller = new AbortController()
    inflight.add(controller)
    const onCallerAbort = () => controller.abort()
    if (options.signal?.aborted) controller.abort()
    options.signal?.addEventListener('abort', onCallerAbort, { once: true })

    try {
      let refreshed = false
      let busyRetries = 0
      for (;;) {
        const headers: Record<string, string> = { Accept: accept, ...options.headers }
        if (encoded.contentType !== undefined) headers['Content-Type'] = encoded.contentType
        if (options.idempotencyKey !== undefined) headers['Idempotency-Key'] = options.idempotencyKey
        const token = authed ? session.getAccessToken() : null
        if (token !== null) headers['Authorization'] = `Bearer ${token}`

        let res: Response
        try {
          res = await (deps.fetch ?? ((u, i) => globalThis.fetch(u, i)))(url, {
            method,
            headers,
            body: encoded.body,
            signal: controller.signal,
            cache: 'no-store',
            credentials: 'same-origin',
          })
        } catch (e) {
          if (isAbortError(e) || controller.signal.aborted) throw e
          reportNetworkFailure()
          throw new ApiError({ status: 0, code: 'network_error' })
        }
        reportApiAnswered(res.status)
        if (res.ok) return res

        const err = apiErrorFromResponse(res.status, await readText(res), res.headers.get('Retry-After'))

        if (err.status === 401 && authed) {
          if (refreshed) {
            // a fresh token was refused too: this session is no good (unless the user signed in again while the request was out)
            if (session.getAccessToken() === token) await session.signOut({ because: 'expired' })
            throw err
          }
          const outcome = await session.refresh()
          if (outcome === 'refreshed') {
            refreshed = true
            continue
          }
          if (outcome === 'unreachable') {
            reportNetworkFailure()
            throw new ApiError({ status: 0, code: 'network_error' })
          }
          throw err // 'rejected': session.refresh() has ended the session, and the router guard sends the user to the sign-in page
        }

        if (err.code === 'busy' && busyRetries < MAX_BUSY_RETRIES) {
          busyRetries++
          const wait = err.retryAfterSeconds === undefined ? DEFAULT_BUSY_WAIT_MS : Math.min(err.retryAfterSeconds * 1000, MAX_BUSY_WAIT_MS)
          await sleep(wait, controller.signal)
          continue
        }
        throw err
      }
    } finally {
      options.signal?.removeEventListener('abort', onCallerAbort)
      inflight.delete(controller)
    }
  }

  async function readText(res: Response): Promise<string> {
    try {
      return await res.text()
    } catch {
      return ''
    }
  }

  async function json<T>(method: string, path: string, body: unknown, options: RequestOptions = {}): Promise<T> {
    const res = await send(method, path, body, options, 'application/json')
    if (res.status === 204) return undefined as T
    const text = await readText(res)
    if (text === '') return undefined as T
    try {
      return JSON.parse(text) as T
    } catch {
      throw new ApiError({ status: res.status, code: 'bad_response' })
    }
  }

  return {
    get: (path, options) => json('GET', path, undefined, options),
    post: (path, body, options) => json('POST', path, body, options),
    put: (path, body, options) => json('PUT', path, body, options),
    del: (path, options) => json('DELETE', path, undefined, options),
    async getText(path, options = {}) {
      const res = await send('GET', path, undefined, options, 'text/csv, text/plain')
      return readText(res)
    },
    async getBlob(path, options = {}) {
      const res = await send('GET', path, undefined, options, 'text/csv, */*')
      try {
        return await res.blob()
      } catch {
        throw new ApiError({ status: res.status, code: 'bad_response' })
      }
    },
    abortAll,
  }
}

/** The API client of the running app. */
export const api: ApiClient = createApi({ session: defaultSession })
