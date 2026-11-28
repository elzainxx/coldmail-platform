// The platform's API, called WITHOUT a screen, for what a spec needs as its starting point or as a second opinion: a finished verification to send to, the user's balances, a suppression search. A spec that is
// about a screen drives the screen; these are for the rest, and they say so by name (`finishedVerification`, not `createJob`).
//
// Every failure names the route and the status and the error's CODE (a word of the API's closed list), never a body: a body may hold an address, and nothing here ever prints a token.
import { expect, type APIRequestContext } from '@playwright/test'
import { csv } from './data.ts'
import { bearer, type Session } from './users.ts'

export interface ApiAnswer<T> {
  status: number
  body: T
}

async function call<T>(request: APIRequestContext, session: Session, method: 'GET' | 'POST', path: string, options: { json?: unknown; csv?: string; idempotencyKey?: string } = {}): Promise<ApiAnswer<T>> {
  const headers: Record<string, string> = { ...bearer(session) }
  if (options.idempotencyKey !== undefined) headers['Idempotency-Key'] = options.idempotencyKey
  let res
  if (method === 'GET') {
    res = await request.get(path, { headers })
  } else if (options.csv !== undefined) {
    res = await request.post(path, { headers: { ...headers, 'Content-Type': 'text/csv' }, data: options.csv })
  } else if (options.json !== undefined) {
    res = await request.post(path, { headers, data: options.json })
  } else {
    res = await request.post(path, { headers })
  }
  let body: unknown
  try {
    body = await res.json()
  } catch {
    body = undefined
  }
  return { status: res.status(), body: body as T }
}

/** The call, and a thrown error that names route, status and the error code when the answer is not a success. */
export async function apiCall<T>(request: APIRequestContext, session: Session, method: 'GET' | 'POST', path: string, options: Parameters<typeof call>[4] = {}): Promise<T> {
  const answer = await call<T>(request, session, method, path, options)
  if (answer.status >= 300) {
    const code = (answer.body as { error?: { code?: unknown } } | undefined)?.error?.code
    throw new Error(`${method} ${path.split('?')[0]} answered ${answer.status}${typeof code === 'string' ? ` (${code})` : ''}`)
  }
  return answer.body
}

interface JobBody {
  job: { id: string; status: string; preview: { fingerprint: string } | null }
}

const ENDED = ['succeeded', 'failed', 'cancelled', 'expired']

/** Waits until the job has ended and returns its status; a spec asserts it is `succeeded`. */
export async function waitForJobEnd(request: APIRequestContext, session: Session, jobId: string, timeoutMs = 180_000): Promise<string> {
  let status = ''
  await expect
    .poll(
      async () => {
        status = (await apiCall<JobBody>(request, session, 'GET', `/v1/jobs/${jobId}`)).job.status
        return ENDED.includes(status)
      },
      { message: 'the job ended', timeout: timeoutMs, intervals: [500, 1000, 2000] },
    )
    .toBe(true)
  return status
}

/**
 * A FINISHED verification of the given addresses, made through the API: draft, preview, confirm, wait. This is the starting point of a send chained to a verification. A spec makes its own rather than
 * depending on another spec's job for two reasons: the order of the files is not part of the contract, and the chain needs recipients that are UNIQUE to the spec (the sink is read for them alone and an
 * unsubscribe must not suppress an address another spec uses), which the fixture list cannot be.
 *
 * The addresses decide the verdicts through module 1's fixture DNS: `...@free.example.test` is risky (plausible, and free), `...@d1.example.test` is invalid (no mail server, charged).
 */
export async function finishedVerification(request: APIRequestContext, session: Session, rows: Array<{ email: string; first_name?: string }>, key: string): Promise<string> {
  const text = csv(['email', 'first_name'], rows.map((r) => [r.email, r.first_name ?? '']))
  const created = await apiCall<JobBody>(request, session, 'POST', '/v1/jobs?module=m1', { csv: text })
  const id = created.job.id
  const previewed = await apiCall<JobBody>(request, session, 'POST', `/v1/jobs/${id}/preview`)
  const fingerprint = previewed.job.preview?.fingerprint
  if (fingerprint === undefined) throw new Error('the preview of the verification has no fingerprint')
  await apiCall(request, session, 'POST', `/v1/jobs/${id}/confirm`, { json: { fingerprint }, idempotencyKey: key })
  const status = await waitForJobEnd(request, session, id)
  expect(status, 'the verification the spec made for itself finished').toBe('succeeded')
  return id
}

/** A draft verification of one address (it costs nothing and holds nothing): a job the jobs table can show. */
export async function draftVerification(request: APIRequestContext, session: Session, email: string): Promise<string> {
  const created = await apiCall<JobBody>(request, session, 'POST', '/v1/jobs?module=m1', { csv: csv(['email'], [[email]]) })
  return created.job.id
}

export interface Balances {
  m1: { available: number; reserved: number }
  m2: { available: number; reserved: number }
}

/** The user's credits at modules 1 and 2 as the API says them (the dashboard shows the same two numbers). */
export async function balances(request: APIRequestContext, session: Session): Promise<Balances> {
  const answer = await apiCall<{ modules: Balances }>(request, session, 'GET', '/v1/balances')
  return answer.modules
}

/** The user's suppression entries whose address contains `q`. */
export async function suppressionsMatching(request: APIRequestContext, session: Session, q: string): Promise<Array<{ email: string; source: string }>> {
  const answer = await apiCall<{ entries: Array<{ email: string; source: string }> }>(request, session, 'GET', `/v1/suppressions?q=${encodeURIComponent(q)}`)
  return answer.entries
}
