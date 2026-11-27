import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { balancesApi, jobsApi, m3Api, meApi, settingsApi, suppressionsApi, templatesApi } from './endpoints.ts'
import { session } from '../auth/session.ts'

interface Call {
  method: string
  url: string
  headers: Record<string, string>
  body: unknown
}

let calls: Call[] = []

beforeEach(() => {
  calls = []
  session.clear()
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ method: init.method ?? 'GET', url, headers: init.headers as Record<string, string>, body: init.body })
      return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
    }),
  )
})
afterEach(() => {
  vi.unstubAllGlobals()
})

const file = new File(['email\na@b.test\n'], 'l.csv', { type: 'text/csv' })

// One row per wrapper: what it must send. A wrapper that is added without a row here makes the "covers every wrapper" test red.
const TABLE: Array<{ name: string; run: () => Promise<unknown>; method: string; url: string; json?: unknown; csv?: true; key?: string }> = [
  { name: 'jobsApi.list', run: () => jobsApi.list({ status: 'active', limit: 10, cursor: undefined }), method: 'GET', url: '/v1/jobs?status=active&limit=10' },
  { name: 'jobsApi.get', run: () => jobsApi.get('j 1'), method: 'GET', url: '/v1/jobs/j%201' },
  { name: 'jobsApi.createJson', run: () => jobsApi.createJson({ module: 'm1', rows: [{ email: 'a@b.test' }] }), method: 'POST', url: '/v1/jobs', json: { module: 'm1', rows: [{ email: 'a@b.test' }] } },
  { name: 'jobsApi.createCsv', run: () => jobsApi.createCsv(file, { module: 'm1', email_column: 'Work email' }), method: 'POST', url: '/v1/jobs?module=m1&email_column=Work+email', csv: true },
  { name: 'jobsApi.preview', run: () => jobsApi.preview('j1', { unknown_choice: 'include' }), method: 'POST', url: '/v1/jobs/j1/preview', json: { unknown_choice: 'include' } },
  { name: 'jobsApi.confirm', run: () => jobsApi.confirm('j1', { fingerprint: 'f' }, 'confirm-1'), method: 'POST', url: '/v1/jobs/j1/confirm', json: { fingerprint: 'f' }, key: 'confirm-1' },
  { name: 'jobsApi.discard', run: () => jobsApi.discard('j1'), method: 'POST', url: '/v1/jobs/j1/discard' },
  { name: 'jobsApi.reopen', run: () => jobsApi.reopen('j1'), method: 'POST', url: '/v1/jobs/j1/reopen' },
  { name: 'jobsApi.cancel', run: () => jobsApi.cancel('j1'), method: 'POST', url: '/v1/jobs/j1/cancel' },
  { name: 'jobsApi.pause', run: () => jobsApi.pause('j1'), method: 'POST', url: '/v1/jobs/j1/pause' },
  { name: 'jobsApi.resume', run: () => jobsApi.resume('j1'), method: 'POST', url: '/v1/jobs/j1/resume' },
  { name: 'jobsApi.events', run: () => jobsApi.events('j1', { after: 5, limit: 100 }), method: 'GET', url: '/v1/jobs/j1/events?after=5&limit=100' },
  { name: 'jobsApi.results', run: () => jobsApi.results('j1', { cursor: 'abc', limit: 50 }), method: 'GET', url: '/v1/jobs/j1/results?cursor=abc&limit=50' },
  { name: 'jobsApi.resultsCsv', run: () => jobsApi.resultsCsv('j1'), method: 'GET', url: '/v1/jobs/j1/results.csv' },
  { name: 'settingsApi.get', run: () => settingsApi.get(), method: 'GET', url: '/v1/settings' },
  { name: 'settingsApi.put', run: () => settingsApi.put({ vendor_fallback_opt_out: true }), method: 'PUT', url: '/v1/settings', json: { vendor_fallback_opt_out: true } },
  { name: 'meApi.get', run: () => meApi.get(), method: 'GET', url: '/v1/me' },
  { name: 'balancesApi.get', run: () => balancesApi.get(), method: 'GET', url: '/v1/balances' },
  { name: 'templatesApi.check', run: () => templatesApi.check({ subject: 's', html: 'h', text: 't' }), method: 'POST', url: '/v1/templates/check', json: { subject: 's', html: 'h', text: 't' } },
  { name: 'suppressionsApi.list', run: () => suppressionsApi.list({ q: 'ann', limit: 20 }), method: 'GET', url: '/v1/suppressions?q=ann&limit=20' },
  { name: 'suppressionsApi.add', run: () => suppressionsApi.add('a@b.test'), method: 'POST', url: '/v1/suppressions', json: { email: 'a@b.test' } },
  { name: 'suppressionsApi.remove', run: () => suppressionsApi.remove('a@b.test'), method: 'POST', url: '/v1/suppressions/remove', json: { email: 'a@b.test' } },
  { name: 'suppressionsApi.importCsv', run: () => suppressionsApi.importCsv(file), method: 'POST', url: '/v1/suppressions/import', csv: true },
  { name: 'm3Api.list', run: () => m3Api.list({ cursor: 'c' }), method: 'GET', url: '/v1/m3/accounts?cursor=c' },
  { name: 'm3Api.add', run: () => m3Api.add({ smtp_host: 'h', smtp_username: 'u', smtp_secret: 's', from_address: 'a@b.test' }), method: 'POST', url: '/v1/m3/accounts', json: { smtp_host: 'h', smtp_username: 'u', smtp_secret: 's', from_address: 'a@b.test' } },
  { name: 'm3Api.test', run: () => m3Api.test('a1', { acknowledge: true }), method: 'POST', url: '/v1/m3/accounts/a1/test', json: { acknowledge: true } },
  { name: 'm3Api.bulkJson', run: () => m3Api.bulkJson([]), method: 'POST', url: '/v1/m3/accounts/bulk', json: { accounts: [] } },
  { name: 'm3Api.bulkCsv', run: () => m3Api.bulkCsv(file), method: 'POST', url: '/v1/m3/accounts/bulk', csv: true },
  { name: 'm3Api.remove', run: () => m3Api.remove('a1'), method: 'POST', url: '/v1/m3/accounts/a1/remove' },
]

describe('endpoints', () => {
  it.each(TABLE.map((r) => [r.name, r] as const))('%s sends the right request', async (_name, row) => {
    await row.run()
    expect(calls).toHaveLength(1)
    const c = calls[0] as Call
    expect(c.method).toBe(row.method)
    expect(c.url).toBe(row.url)
    if (row.json !== undefined) expect(JSON.parse(c.body as string)).toEqual(row.json)
    if (row.json === undefined && !row.csv) expect(c.body).toBeUndefined()
    if (row.csv) {
      expect(c.body).toBe(file)
      expect(c.headers['Content-Type']).toBe('text/csv')
    }
    if (row.key !== undefined) expect(c.headers['Idempotency-Key']).toBe(row.key)
  })

  it('has a row for every wrapper (a new wrapper without a test is red)', () => {
    const all = { jobsApi, settingsApi, meApi, balancesApi, templatesApi, suppressionsApi, m3Api }
    const wrappers = Object.entries(all).flatMap(([group, fns]) => Object.keys(fns).map((k) => `${group}.${k}`))
    expect(wrappers.length).toBeGreaterThanOrEqual(28)
    expect(TABLE.map((r) => r.name).sort()).toEqual(wrappers.sort())
  })

  it('paired control: a wrong expectation is caught (the table can fail)', async () => {
    await jobsApi.pause('j1')
    expect((calls[0] as Call).url).not.toBe('/v1/jobs/j1/resume')
    expect((calls[0] as Call).method).not.toBe('GET')
  })

  it('the removal routes are POSTs, never a path with an address in it (decision 8)', () => {
    for (const r of TABLE.filter((x) => x.name.endsWith('.remove'))) {
      expect(r.method, r.name).toBe('POST')
      expect(r.url, r.name).not.toContain('@')
    }
  })
})
