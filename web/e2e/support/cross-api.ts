// API helpers for the specs of chunks A10 and A11 that start from a signed-in session WITHOUT a screen (the token comes from users.ts `ready`): jobs (create, preview, confirm, wait, results, end),
// suppressions, settings and balances. A spec uses them to make its data quickly and to read what the screens only show, and to put a job away when a test fails half way (a user can hold ONE active job
// per module, so a job left running would turn the next spec's confirm into a refusal).
//
// NOTHING HERE PRINTS A TOKEN, AN ADDRESS LIST OR A BODY: a refusal names the call and the status and, for the platform's own error envelope, its code (a word of the API, never a value). The run's users are
// shared by every spec (README, "The users"), so every helper works on what it is given and never assumes an empty account.
import { type APIRequestContext, type APIResponse } from '@playwright/test'
import type { Balances, ConfirmResult, Job, JobResult, Preview, SuppressionsPage, UserSettings } from '../../src/api/types.ts'
import { bearer, type Session } from './users.ts'

export type SendModule = 'm2' | 'm3'

export const FINAL_STATUSES: readonly string[] = ['succeeded', 'cancelled', 'expired', 'failed']

export const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

/** The platform's error envelope, as far as a spec reads it: the word and the message (a fixed sentence per code), never the details. */
export interface Envelope {
  error?: { code?: string; message?: string; request_id?: string; details?: Record<string, unknown> }
}

/** What went wrong with a response, for an error message: the status and, when the body is the platform's envelope, its code. */
async function describeFailure(res: APIResponse): Promise<string> {
  let code = ''
  try {
    code = ((await res.json()) as Envelope).error?.code ?? ''
  } catch {
    // not JSON: the status says enough
  }
  return `status ${res.status()}${code === '' ? '' : `, code ${code}`}`
}

async function okJson<T>(res: APIResponse, expected: number | readonly number[], what: string): Promise<T> {
  const want = typeof expected === 'number' ? [expected] : expected
  if (!want.includes(res.status())) throw new Error(`${what} answered ${await describeFailure(res)} (wanted ${want.join(' or ')})`)
  return (await res.json()) as T
}

/** The error code of an answer (undefined when the body is no envelope). */
export async function errorCodeOf(res: APIResponse): Promise<string | undefined> {
  try {
    return ((await res.json()) as Envelope).error?.code
  } catch {
    return undefined
  }
}

/** The envelope of an answer without its request id (which differs by request): two refusals that must be "the same answer" are equal as these strings. */
export async function envelopeWithoutRequestId(res: APIResponse): Promise<string> {
  const body = (await res.json()) as Envelope
  const error = { ...(body.error ?? {}) }
  delete error.request_id
  return JSON.stringify({ error })
}

// ---- jobs ----

export async function getJob(request: APIRequestContext, s: Session, id: string): Promise<Job> {
  return (await okJson<{ job: Job }>(await request.get(`/v1/jobs/${id}`, { headers: bearer(s) }), 200, 'GET a job')).job
}

export async function listJobs(request: APIRequestContext, s: Session, query: Record<string, string | number> = {}): Promise<Job[]> {
  return (await okJson<{ jobs: Job[] }>(await request.get('/v1/jobs', { headers: bearer(s), params: query }), 200, 'GET the jobs')).jobs
}

/** Every result of a job, page after page (the whole list, not what a screen shows). */
export async function allResults(request: APIRequestContext, s: Session, id: string): Promise<JobResult[]> {
  const out: JobResult[] = []
  let cursor: string | undefined
  for (let page = 0; page < 200; page++) {
    const params: Record<string, string | number> = { limit: 1000 }
    if (cursor !== undefined) params['cursor'] = cursor
    const body = await okJson<{ results: JobResult[]; next_cursor: string | null }>(
      await request.get(`/v1/jobs/${id}/results`, { headers: bearer(s), params }),
      200,
      'GET the results of a job',
    )
    out.push(...body.results)
    if (body.next_cursor === null) return out
    cursor = body.next_cursor
  }
  throw new Error('the results of the job did not end after 200 pages')
}

export interface SendJobSpec {
  module: SendModule
  /** the recipients; each row carries `first_name` so that the merge field of the default message has a value */
  addresses: string[]
  subject?: string
  html?: string
  text?: string
}

/** A draft of a send job from a JSON list. */
export async function createSendDraft(request: APIRequestContext, s: Session, spec: SendJobSpec): Promise<Job> {
  const body = {
    module: spec.module,
    rows: spec.addresses.map((email, i) => ({ email, fields: { first_name: `Person${i + 1}` } })),
    content: { subject: spec.subject ?? 'Hello {{first_name|there}}', html: spec.html ?? '<p>Hello {{first_name|there}}</p>', text: spec.text ?? '' },
  }
  const res = await request.post('/v1/jobs', { headers: bearer(s), data: body })
  return (await okJson<{ job: Job }>(res, 201, 'POST a send job')).job
}

/** A draft of a verification job from a list of addresses (one column, `email`). */
export async function createVerifyDraft(request: APIRequestContext, s: Session, addresses: string[]): Promise<Job> {
  const csv = `email\r\n${addresses.join('\r\n')}\r\n`
  const res = await request.post('/v1/jobs', { headers: { ...bearer(s), 'Content-Type': 'text/csv' }, params: { module: 'm1' }, data: csv })
  return (await okJson<{ job: Job }>(res, 201, 'POST a verification job')).job
}

export async function previewJob(request: APIRequestContext, s: Session, id: string): Promise<Job> {
  return (await okJson<{ job: Job }>(await request.post(`/v1/jobs/${id}/preview`, { headers: bearer(s) }), 200, 'POST a preview')).job
}

export async function confirmJob(request: APIRequestContext, s: Session, job: Job): Promise<ConfirmResult> {
  const fingerprint = job.preview?.fingerprint
  if (fingerprint === undefined) throw new Error('the job has no preview to confirm')
  const res = await request.post(`/v1/jobs/${job.id}/confirm`, {
    headers: { ...bearer(s), 'Idempotency-Key': `e2e-${job.id}` },
    data: { fingerprint },
  })
  return await okJson<ConfirmResult>(res, 200, 'POST a confirm')
}

export interface StartedJob {
  id: string
  preview: Preview
  confirm: ConfirmResult
}

/** A draft, its preview and its confirm: the job is `confirmed` (the engine starts it within a tick). */
export async function startJob(request: APIRequestContext, s: Session, draft: Job): Promise<StartedJob> {
  const previewed = await previewJob(request, s, draft.id)
  if (previewed.preview === null) throw new Error('the previewed job carries no preview')
  const confirm = await confirmJob(request, s, previewed)
  return { id: draft.id, preview: previewed.preview, confirm }
}

/** Polls the job until its status is one of `statuses`; returns the job. A job that ends in another final status is a failure that says so. */
export async function waitForStatus(request: APIRequestContext, s: Session, id: string, statuses: readonly string[], timeoutMs = 90_000): Promise<Job> {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const job = await getJob(request, s, id)
    if (statuses.includes(job.status)) return job
    if (FINAL_STATUSES.includes(job.status)) throw new Error(`the job ended ${job.status} while waiting for ${statuses.join(' or ')}`)
    if (Date.now() > deadline) throw new Error(`the job is still ${job.status} after ${timeoutMs} ms, waiting for ${statuses.join(' or ')}`)
    await sleep(500)
  }
}

/** A finished verification job of `addresses` (made through the API, waited for): what a spec needs when it only wants a job that finished. */
export async function finishedVerifyJob(request: APIRequestContext, s: Session, addresses: string[]): Promise<Job> {
  const started = await startJob(request, s, await createVerifyDraft(request, s, addresses))
  return await waitForStatus(request, s, started.id, ['succeeded'], 120_000)
}

/** Puts a job away whatever state it is in (a test that failed half way): resume is not needed, a paused job can be cancelled; the call waits for the end and swallows what it cannot do. */
export async function endJob(request: APIRequestContext, s: Session, id: string): Promise<void> {
  try {
    const job = await getJob(request, s, id)
    if (FINAL_STATUSES.includes(job.status)) return
    if (job.status === 'draft' || job.status === 'previewed') {
      await request.post(`/v1/jobs/${id}/discard`, { headers: bearer(s) })
      return
    }
    await request.post(`/v1/jobs/${id}/cancel`, { headers: bearer(s) })
    const deadline = Date.now() + 90_000
    while (Date.now() < deadline) {
      if (FINAL_STATUSES.includes((await getJob(request, s, id)).status)) return
      await sleep(1000)
    }
  } catch {
    // best effort: the stack is thrown away when the run ends
  }
}

// ---- balances, settings, suppressions ----

export async function getBalances(request: APIRequestContext, s: Session): Promise<Balances> {
  return await okJson<Balances>(await request.get('/v1/balances', { headers: bearer(s) }), 200, 'GET the balances')
}

export async function getSettings(request: APIRequestContext, s: Session): Promise<UserSettings> {
  return await okJson<UserSettings>(await request.get('/v1/settings', { headers: bearer(s) }), 200, 'GET the settings')
}

export async function putSettings(request: APIRequestContext, s: Session, optOut: boolean): Promise<UserSettings> {
  return await okJson<UserSettings>(await request.put('/v1/settings', { headers: bearer(s), data: { vendor_fallback_opt_out: optOut } }), 200, 'PUT the settings')
}

/** Suppresses an address for the session's user (201 new, 200 already there). */
export async function addSuppression(request: APIRequestContext, s: Session, email: string): Promise<{ already: boolean }> {
  const res = await request.post('/v1/suppressions', { headers: bearer(s), data: { email } })
  const body = await okJson<{ already?: boolean }>(res, [200, 201], 'POST a suppression')
  return { already: body.already === true }
}

/** Removes the user's own entry; resolves to the answer's `removed`. */
export async function removeSuppression(request: APIRequestContext, s: Session, email: string): Promise<boolean> {
  const res = await request.post('/v1/suppressions/remove', { headers: bearer(s), data: { email } })
  return (await okJson<{ removed: boolean }>(res, 200, 'POST a suppression removal')).removed
}

/** The entries of the user's list that contain `q` (a search, not a pattern). */
export async function searchSuppressions(request: APIRequestContext, s: Session, q: string): Promise<SuppressionsPage['entries']> {
  const res = await request.get('/v1/suppressions', { headers: bearer(s), params: { q, limit: 100 } })
  return (await okJson<SuppressionsPage>(res, 200, 'GET the suppressions')).entries
}
