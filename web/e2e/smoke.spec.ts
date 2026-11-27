// The smoke spec: it proves the PLUMBING of the e2e scope without any screen. If this is red, no other spec means anything; if the screens change, this does not.
//   1-3  the platform serves the app from one origin: /config.json, the headers of /, the fallback of the router, the 404 of a missing file
//   4    the API refuses a call with no token
//   5    the chain that every real spec stands on: an invited user sets a password at GoTrue, the BROWSER signs in at GoTrue from the platform's origin (TLS, CORS and the Content-Security-Policy all
//        let it), and the API answers 200 to the token GoTrue issued (the platform's JWT secret and issuer are GoTrue's)
//   6-9  the two helpers the screens' specs will call (axe, the copy deny-list), each with its PAIRED CONTROL: a helper that cannot fail proves nothing
//   10   the screenshot helper writes a picture where the stage reads it
// No assertion message here carries a token, a key or a password (the stage refuses a run whose output holds one): statuses and booleans only.
import { statSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { expectNoA11yViolations } from './support/axe.ts'
import { expectNoInternalWords, internalWords } from './support/copy.ts'
import { expectedCsp, parseCsp } from './support/csp.ts'
import { anonKey, appUrl, authOrigin, authUrl } from './support/env.ts'
import { shot } from './support/shots.ts'
import { bearer, ready, user } from './support/users.ts'

const PLAIN_PAGE = '<!doctype html><html lang="en"><head><title>Plumbing</title></head><body><main><h1>Plumbing</h1><p>Zebra quokka pangolin.</p></main></body></html>'

test.describe('the platform serves the app from one origin', () => {
  test('/config.json answers with exactly the public Supabase address and the anon key of the run', async ({ request }) => {
    const res = await request.get('/config.json')
    expect(res.status()).toBe(200)
    expect(res.headers()['content-type']).toContain('application/json')
    expect(res.headers()['cache-control']).toContain('no-store')
    const body = (await res.json()) as { supabase_url?: unknown; anon_key?: unknown }
    expect(body.supabase_url === authUrl(), 'supabase_url is the address GoTrue is reached at').toBe(true)
    expect(body.anon_key === anonKey(), 'anon_key is the anon key of this run').toBe(true)
    expect(Object.keys(body).sort(), 'config.json holds the two public values and nothing else').toEqual(['anon_key', 'supabase_url'])
  })

  test('/ carries the strict Content-Security-Policy and is never cached', async ({ request }) => {
    const res = await request.get('/')
    expect(res.status()).toBe(200)
    expect(res.headers()['content-type']).toContain('text/html')
    expect(res.headers()['cache-control']).toContain('no-store')
    expect(parseCsp(res.headers()['content-security-policy'])).toEqual(parseCsp(expectedCsp(authOrigin())))
  })

  test('an app route that is no file falls back to the app, and a missing file is a 404', async ({ request }) => {
    const route = await request.get('/jobs/a-route-the-router-owns')
    expect(route.status()).toBe(200)
    expect(route.headers()['content-type']).toContain('text/html')
    expect(route.headers()['cache-control']).toContain('no-store')
    expect(parseCsp(route.headers()['content-security-policy'])).toEqual(parseCsp(expectedCsp(authOrigin())))
    expect((await request.get('/assets/no-such-file.js')).status()).toBe(404)
  })

  test('the API refuses a call with no token', async ({ request }) => {
    const res = await request.get('/v1/jobs')
    expect(res.status()).toBe(401)
    const body = (await res.json()) as { error?: { code?: unknown } }
    expect(typeof body.error?.code, 'the refusal is the API\'s own error envelope').toBe('string')
  })
})

test.describe('the sign-in chain', () => {
  test('an invited user signs in at GoTrue from the browser and the API accepts the token', async ({ page, request }) => {
    // the invite is accepted WITHOUT a screen and the password set with the invite's own tokens (users.ts); then everything below is the BROWSER's
    const a = user('a')
    const session = await ready(request, 'a')
    await page.goto('/')
    const seen = await page.evaluate(
      async ({ auth, anon, email, password }) => {
        // a cross-origin fetch from the platform's origin to GoTrue's: the TLS edge, GoTrue's CORS answer (apikey is an allowed header) and the page's connect-src all have to let it through
        const grant = await fetch(`${auth}/token?grant_type=password`, {
          method: 'POST',
          headers: { apikey: anon, 'Content-Type': 'application/json' },
          body: JSON.stringify({ email, password }),
        })
        const body = (await grant.json()) as { access_token?: string; token_type?: string }
        // and the platform's own API from the same page, with the token GoTrue just issued (same origin: connect-src 'self')
        const api = body.access_token === undefined ? undefined : await fetch('/v1/jobs', { headers: { Authorization: `Bearer ${body.access_token}` } })
        return { grantStatus: grant.status, hasToken: typeof body.access_token === 'string' && body.access_token !== '', tokenType: body.token_type, apiStatus: api?.status }
      },
      { auth: authUrl(), anon: anonKey(), email: a.email, password: a.password },
    )
    expect(seen.grantStatus, 'the browser\'s password sign in at GoTrue').toBe(200)
    expect(seen.hasToken).toBe(true)
    expect(seen.tokenType).toBe('bearer')
    expect(seen.apiStatus, 'the browser\'s call of the platform\'s API with that token').toBe(200)
    // and the token a spec gets without a screen is accepted the same way
    const res = await request.get('/v1/jobs', { headers: bearer(session) })
    expect(res.status(), 'GET /v1/jobs with the token GoTrue issued').toBe(200)
  })
})

test.describe('the helpers of the screens\' specs', () => {
  test('axe: a plain page has no violation', async ({ page }) => {
    await page.setContent(PLAIN_PAGE)
    await expectNoA11yViolations(page)
  })

  test('axe CONTROL: a page with an image that has no text alternative is reported, by rule', async ({ page }) => {
    await page.setContent(PLAIN_PAGE.replace('<p>', '<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw="><p>'))
    await expect(expectNoA11yViolations(page)).rejects.toThrow(/image-alt/)
  })

  test('copy: a page with no internal word passes', async ({ page }) => {
    await page.setContent(PLAIN_PAGE)
    await expectNoInternalWords(page)
  })

  test('copy CONTROL: a page that says an internal word is reported, naming it', async ({ page }) => {
    const [word] = internalWords()
    expect(word, 'the deny-list has a first word').toBeTruthy()
    await page.setContent(PLAIN_PAGE.replace('Zebra quokka pangolin.', `Zebra ${word} pangolin.`))
    await expect(expectNoInternalWords(page)).rejects.toThrow(new RegExp(`'${(word ?? '').replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}'`))
  })
})

test.describe('the artifacts', () => {
  test('a screenshot is written where the stage reads it', async ({ page }) => {
    await page.goto(appUrl())
    const path = await shot(page, 'smoke-root')
    expect(statSync(path).size, 'the picture has bytes').toBeGreaterThan(500)
  })
})
