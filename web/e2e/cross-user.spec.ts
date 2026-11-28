// A11, "cross-user (UI and API)": user a owns a finished job, a suppression entry and a module 3 account; user b, signed in through the screen, meets NONE of it, and the API refuses b every way to touch it.
//
//   1  b's screens: the jobs table, the suppression list and the accounts list hold nothing of a's; a's job page, opened in b's browser, is the "we could not find that" notice and holds no data of a's
//   2  b's token on a's job and a's account: `404 not_found` for every read and every move (the same answer, but for the request id, as a job that does not exist), and a's things are as they were
//   3  b's token on a's suppression: it is not in b's list, removing it answers `removed:false` and leaves a's entry, adding it makes b's OWN entry, which can then be removed without touching a's
//   4  no token, and a malformed token: 401, the same body
//
// a's data is made ONCE per worker (the API, the users are shared by the run) with names unique to this run, and removed when the file's tests are over. No test prints a token or an address list: it compares
// statuses, codes and booleans.
import { expect, test, type APIRequestContext, type APIResponse } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import {
  addSuppression,
  envelopeWithoutRequestId,
  errorCodeOf,
  finishedVerifyJob,
  getJob,
  listJobs,
  removeSuppression,
  searchSuppressions,
} from './support/cross-api.ts'
import { checkScreen, h1, jobStatus, openScreen, pageHolds, runId, settled, signIn } from './support/cross-ui.ts'
import { accountAt, accountRow, addAccount, listAccounts, removeAccount } from './support/m3-accounts.ts'
import { appUrl } from './support/env.ts'
import { bearer, ready } from './support/users.ts'

const RUN = runId()

interface Seed {
  jobId: string
  /** a marker that is in the addresses of a's job and of nothing else */
  jobMarker: string
  suppressed: string
  accountId: string
  accountFrom: string
  accountUsername: string
}

let seeded: Promise<Seed> | undefined

/** a's data, made once: a finished verification job, a suppressed address and a module 3 account. */
function seedOnce(request: APIRequestContext): Promise<Seed> {
  seeded ??= (async () => {
    const sa = await ready(request, 'a')
    const jobMarker = `xu-${RUN}`
    const job = await finishedVerifyJob(request, sa, [`${jobMarker}-1@free.example.test`, `${jobMarker}-2@d1.example.test`])
    const suppressed = `${jobMarker}-suppressed@example.test`
    await addSuppression(request, sa, suppressed)
    const account = accountAt(`${jobMarker}-acct`)
    const added = await addAccount(request, sa, account)
    return { jobId: job.id, jobMarker, suppressed, accountId: added.id, accountFrom: account.from_address, accountUsername: account.smtp_username }
  })()
  return seeded
}

test.afterAll(async ({ playwright }) => {
  const seed = await seeded?.catch(() => undefined)
  if (seed === undefined) return
  const request = await playwright.request.newContext({ baseURL: appUrl(), ignoreHTTPSErrors: true })
  try {
    const sa = await ready(request, 'a')
    await removeSuppression(request, sa, seed.suppressed).catch(() => undefined)
    await removeAccount(request, sa, seed.accountId).catch(() => undefined)
  } finally {
    await request.dispose()
  }
})

const NOT_FOUND = 'not_found'

async function expectRefusal(res: APIResponse, status: number, code: string, what: string): Promise<void> {
  expect(res.status(), `${what}: the status`).toBe(status)
  expect(await errorCodeOf(res), `${what}: the error code`).toBe(code)
}

test.describe('user b meets nothing of user a', () => {
  test.describe.configure({ timeout: 240_000 })

  test('b\'s screens hold none of a\'s job, suppression or account, and a\'s job page is a notice that it cannot be found', async ({ page, request }) => {
    const seed = await seedOnce(request)
    const sb = await ready(request, 'b')
    const answers: number[] = []
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === `/v1/jobs/${seed.jobId}`) answers.push(r.status())
    })

    await signIn(page, request, 'b')

    await test.step('the jobs table does not list a\'s job', async () => {
      await openScreen(page, 'jobs')
      await settled(page)
      await expect(page.getByTestId(`job-row-${seed.jobId}`)).toHaveCount(0)
      expect(await pageHolds(page, seed.jobId), 'b\'s jobs page holds the id of a\'s job').toBe(false)
      expect((await listJobs(request, sb, { limit: 100 })).some((j) => j.id === seed.jobId), 'GET /v1/jobs as b lists a\'s job').toBe(false)
      await checkScreen(page, 'xu-b-jobs')
    })

    await test.step('the suppression list does not hold a\'s address, searched or not', async () => {
      await openScreen(page, 'suppressions')
      await settled(page)
      expect(await pageHolds(page, seed.suppressed), 'b\'s list holds a\'s suppressed address').toBe(false)
      await page.getByLabel(t('suppressions.search.label'), { exact: true }).fill(seed.suppressed)
      await expect(page.getByText(t('suppressions.empty.search', { query: seed.suppressed }), { exact: true })).toBeVisible()
      await expect(page.getByTestId(/^suppression-row-/)).toHaveCount(0)
      await checkScreen(page, 'xu-b-suppressions')
    })

    await test.step('the accounts list does not hold a\'s account', async () => {
      await openScreen(page, 'accounts')
      await settled(page)
      await expect(accountRow(page, seed.accountId)).toHaveCount(0)
      expect(await pageHolds(page, seed.accountFrom), 'b\'s accounts page holds a\'s From address').toBe(false)
      expect(await pageHolds(page, seed.accountUsername), 'b\'s accounts page holds a\'s SMTP username').toBe(false)
      await checkScreen(page, 'xu-b-accounts')
    })

    await test.step('a\'s job page, opened in b\'s browser, says it cannot be found and shows nothing of the job', async () => {
      await signIn(page, request, 'b', `/jobs/${seed.jobId}`)
      await expect(h1(page, t('jobs.detail.title'))).toBeVisible()
      await expect(page.getByRole('alert').filter({ hasText: t('error.not_found') })).toBeVisible()
      await expect(jobStatus(page, seed.jobId)).toHaveCount(0)
      await expect(page.getByRole('heading', { name: t('jobs.detail.summary'), exact: true })).toHaveCount(0)
      await expect(page.getByRole('heading', { name: t('jobs.results.title'), exact: true })).toHaveCount(0)
      await expect(page.getByRole('heading', { name: t('jobs.history.title'), exact: true })).toHaveCount(0)
      await expect(page.getByRole('link', { name: t('jobs.field.sourceLink') })).toHaveCount(0)
      expect(await pageHolds(page, seed.jobMarker), 'the page holds an address of a\'s job').toBe(false)
      expect(answers.length > 0 && answers.every((s) => s === 404), 'every answer to b\'s request for a\'s job was a 404').toBe(true)
      await checkScreen(page, 'xu-b-job-not-found')
    })
  })

  test('b\'s token on a\'s job and a\'s account: 404 not_found for every read and every move, and nothing of a\'s changes', async ({ request }) => {
    const seed = await seedOnce(request)
    const sa = await ready(request, 'a')
    const sb = await ready(request, 'b')
    const h = bearer(sb)
    const job = `/v1/jobs/${seed.jobId}`
    const account = `/v1/m3/accounts/${seed.accountId}`

    const calls: Array<[string, () => Promise<APIResponse>]> = [
      ['GET the job', () => request.get(job, { headers: h })],
      ['GET its results', () => request.get(`${job}/results`, { headers: h })],
      ['GET its results.csv', () => request.get(`${job}/results.csv`, { headers: h })],
      ['GET its events', () => request.get(`${job}/events`, { headers: h })],
      ['POST its preview', () => request.post(`${job}/preview`, { headers: h })],
      ['POST its pause', () => request.post(`${job}/pause`, { headers: h })],
      ['POST its resume', () => request.post(`${job}/resume`, { headers: h })],
      ['POST its cancel', () => request.post(`${job}/cancel`, { headers: h })],
      ['POST its discard', () => request.post(`${job}/discard`, { headers: h })],
      ['POST its reopen', () => request.post(`${job}/reopen`, { headers: h })],
      ['POST its confirm', () => request.post(`${job}/confirm`, { headers: { ...h, 'Idempotency-Key': `e2e-xu-${RUN}` }, data: { fingerprint: '0'.repeat(64) } })],
      ['POST the account\'s test', () => request.post(`${account}/test`, { headers: h, data: {} })],
      ['POST the account\'s removal', () => request.post(`${account}/remove`, { headers: h })],
    ]
    for (const [what, call] of calls) await expectRefusal(await call(), 404, NOT_FOUND, `b's token: ${what}`)

    await test.step('a job that does not exist gets the same answer, but for the request id', async () => {
      const missing = '0b000000-0000-4000-8000-0000000e2e00'
      const real = await envelopeWithoutRequestId(await request.get(job, { headers: h }))
      const none = await envelopeWithoutRequestId(await request.get(`/v1/jobs/${missing}`, { headers: h }))
      expect(real === none, 'a job of another user is answered exactly as a job that does not exist').toBe(true)
    })

    await test.step('everything of a\'s is as it was', async () => {
      expect((await getJob(request, sa, seed.jobId)).status, 'a\'s job after b\'s moves').toBe('succeeded')
      expect((await listAccounts(request, sa)).some((x) => x.id === seed.accountId), 'a\'s account after b\'s removal').toBe(true)
      expect((await listJobs(request, sb, { limit: 100 })).some((j) => j.id === seed.jobId), 'b\'s list of jobs').toBe(false)
    })
  })

  test('a\'s suppression: not in b\'s list, b\'s removal says nothing was removed and leaves it, and b\'s own entry for the address is b\'s alone', async ({ request }) => {
    const seed = await seedOnce(request)
    const sa = await ready(request, 'a')
    const sb = await ready(request, 'b')

    expect((await searchSuppressions(request, sb, seed.suppressed)).length, 'b\'s list holds a\'s address').toBe(0)
    expect((await searchSuppressions(request, sa, seed.suppressed)).length, 'a\'s list holds a\'s address').toBe(1)

    expect(await removeSuppression(request, sb, seed.suppressed), 'b\'s removal of an address that is only on a\'s list').toBe(false)
    expect((await searchSuppressions(request, sa, seed.suppressed)).length, 'a\'s entry after b\'s removal').toBe(1)

    // b suppresses the same address: it is a NEW entry for b (a's entry is not b's), and b removing it leaves a's
    expect((await addSuppression(request, sb, seed.suppressed)).already, 'b\'s add says the address was already on b\'s list').toBe(false)
    expect((await searchSuppressions(request, sb, seed.suppressed)).length, 'b\'s list after b\'s add').toBe(1)
    expect(await removeSuppression(request, sb, seed.suppressed), 'b\'s removal of b\'s own entry').toBe(true)
    expect((await searchSuppressions(request, sb, seed.suppressed)).length, 'b\'s list after b\'s removal').toBe(0)
    expect((await searchSuppressions(request, sa, seed.suppressed)).length, 'a\'s entry after b\'s add and removal').toBe(1)
  })

  test('with no token, and with a malformed token, the API answers 401 with the same body', async ({ request }) => {
    const seed = await seedOnce(request)
    const routes = [`/v1/jobs/${seed.jobId}`, '/v1/jobs', '/v1/suppressions', '/v1/m3/accounts', '/v1/settings', '/v1/balances']
    const attempts: Array<[string, Record<string, string>]> = [
      ['no Authorization header', {}],
      ['a Bearer token that is not a token', { Authorization: 'Bearer not-a-token' }],
      ['a scheme that is not Bearer', { Authorization: 'Basic bm90OmF0b2tlbg==' }],
    ]
    let first: string | undefined
    for (const route of routes) {
      for (const [what, headers] of attempts) {
        const res = await request.get(route, { headers })
        await expectRefusal(res, 401, 'unauthenticated', `${what} on ${route.startsWith('/v1/jobs/') ? 'a job' : route}`)
        expect((res.headers()['www-authenticate'] ?? '').toLowerCase().startsWith('bearer'), `${what}: the refusal says which scheme to use`).toBe(true)
        const body = await envelopeWithoutRequestId(res)
        first ??= body
        expect(body === first, `${what} on ${route.startsWith('/v1/jobs/') ? 'a job' : route}: the body of the refusal is the same as every other`).toBe(true)
      }
    }
  })
})
