// The sign-in screens, end to end (chunks A10 "invite and sign in" and A11 "sign-out behaviour"), with user c, whose invite belongs to the browser and to this file alone:
//
//   the invitation   (serial: each test needs what the one before it left)
//     1  the invite link, opened in the browser, lands on the accept-invite page; the tokens leave the address bar; a password that is too short or does not match is explained and NOT sent;
//        a valid one is saved and the dashboard opens with the user's address in the top bar; nothing is in storage; a reload asks to sign in again (the session is in memory only)
//     2  the password signs in; sign out; the Back button shows the sign-in page and nothing of the user (not the address, not a job row), with a paired control that the data WAS on the page before
//     3  the same invite link, opened again, says it is no longer good, in the app's words and with none of GoTrue's
//   the rest (independent)
//     4  a wrong password gets the calm wording and none of GoTrue's text
//     5  "Forgot your password" gives the same confirmation for a known and for an unknown address
//
// Why the order matters in the first group: an invite is single use, and the password is set by test 1. If test 1 fails the others fail on purpose (they say so): they prove nothing without it.
// The session is in memory (decision 2): `page.goto` and a reload end it, so every step after a sign-in moves by the navigation.
// No assertion message here holds a token, a password, an invite link or an address: booleans and statuses only.
import { expect, test, type Page } from '@playwright/test'
import { PASSWORD_MAX_BYTES, PASSWORD_MIN_LENGTH } from '../src/auth/password.ts'
import { t } from '../src/copy/index.ts'
import { expectNoInternalWords } from './support/copy.ts'
import { draftVerification } from './support/flows-api.ts'
import { runId } from './support/flows-data.ts'
import { goTo, h1, settled, signIn, signOut } from './support/flows-ui.ts'
import { shot } from './support/shots.ts'
import { passwordSignIn, user } from './support/users.ts'

const rule = { min: PASSWORD_MIN_LENGTH, max: PASSWORD_MAX_BYTES }

/** Does storage or a cookie hold anything shaped like a token? (A JWT starts with `eyJ`: both of its first two segments are JSON objects.) */
async function holdsNoToken(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const dump = JSON.stringify([Object.entries(window.localStorage), Object.entries(window.sessionStorage), document.cookie])
    return !/eyJ[A-Za-z0-9_-]{10,}/.test(dump)
  })
}

/** The text of the page as the DOM holds it, for "is the address in it": a boolean is returned so that no failure message can quote the page. */
async function domHolds(page: Page, needle: string): Promise<boolean> {
  return (await page.content()).includes(needle)
}

/** Requests of a method to a path suffix, counted (the password is set with GoTrue's PUT /user): the count is the only thing kept, never an address or a body. */
function countRequests(page: Page, method: string, pathSuffix: string): () => number {
  let n = 0
  page.on('request', (r) => {
    if (r.method() === method && new URL(r.url()).pathname.endsWith(pathSuffix)) n += 1
  })
  return () => n
}

test.describe.serial('the invitation of user c', () => {
  test('the invite link opens the accept-invite page without its tokens in the address bar; a password is chosen on the screen; a reload asks to sign in again', async ({ page }) => {
    test.setTimeout(120_000)
    const c = user('c')
    const passwordPuts = countRequests(page, 'PUT', '/user')
    // The first thing the app's own script reads, before it runs: did the link arrive with a session in its hash? (A flag only, never the tokens.) It is the paired control of "the hash is gone".
    await page.addInitScript(() => {
      Object.assign(window, { e2eLinkCarriedSession: /access_token=/.test(window.location.hash) })
    })

    await test.step('the invite link, opened in the browser, lands on the accept-invite page', async () => {
      await page.goto(c.invite_link)
      await expect(page).toHaveURL(/\/accept-invite$/)
      await expect(h1(page, t('auth.invite.title'))).toBeVisible()
      await expect(page.getByText(t('auth.invite.intro'))).toBeVisible()
      await expect(page.getByText(t('auth.password.rule', rule))).toBeVisible()
    })

    await test.step('the tokens are gone from the address bar (and the link did carry them)', async () => {
      expect(await page.evaluate(() => window.location.hash === ''), 'the address bar holds no hash').toBe(true)
      expect(await page.evaluate(() => (window as unknown as { e2eLinkCarriedSession?: boolean }).e2eLinkCarriedSession === true), 'the link arrived with a session in its hash').toBe(true)
      expect(await holdsNoToken(page), 'storage and cookies hold no token').toBe(true)
      await settled(page, 'login-invite-accept')
    })

    await test.step('a password that is too short is explained and is not sent', async () => {
      await page.getByLabel(t('auth.password.new'), { exact: true }).fill('short')
      await page.getByLabel(t('auth.password.confirm'), { exact: true }).fill('short')
      await page.getByRole('button', { name: t('auth.password.submit'), exact: true }).click()
      await expect(page.getByText(t('auth.password.tooShort', rule))).toBeVisible()
      await expect(page.getByLabel(t('auth.password.new'), { exact: true })).toHaveAttribute('aria-invalid', 'true')
      expect(passwordPuts(), 'nothing was sent to the sign-in service').toBe(0)
      await expect(page).toHaveURL(/\/accept-invite$/)
      await settled(page, 'login-invite-too-short')
    })

    await test.step('two passwords that differ are explained and are not sent', async () => {
      await page.getByLabel(t('auth.password.new'), { exact: true }).fill(c.password)
      await page.getByLabel(t('auth.password.confirm'), { exact: true }).fill(`${c.password}-different`)
      await page.getByRole('button', { name: t('auth.password.submit'), exact: true }).click()
      await expect(page.getByText(t('auth.password.mismatch'))).toBeVisible()
      expect(passwordPuts(), 'nothing was sent to the sign-in service').toBe(0)
    })

    await test.step('a valid password is saved and the dashboard opens, with the user in the top bar', async () => {
      await page.getByLabel(t('auth.password.new'), { exact: true }).fill(c.password)
      await page.getByLabel(t('auth.password.confirm'), { exact: true }).fill(c.password)
      await page.getByRole('button', { name: t('auth.password.submit'), exact: true }).click()
      await expect(h1(page, t('dashboard.title'))).toBeVisible()
      await expect(page).toHaveURL(/\/$/)
      expect(passwordPuts(), 'the password was sent to the sign-in service exactly once').toBe(1)
      await expect(page.getByText(t('common.signedInAs', { email: c.email }))).toBeVisible()
      await expect(page.getByTestId('balance-m1')).toBeVisible()
      expect(await holdsNoToken(page), 'storage and cookies hold no token after signing in').toBe(true)
      await settled(page, 'login-dashboard-first')
    })

    await test.step('a reload shows the sign-in page: the session lived in memory only', async () => {
      await page.reload()
      await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
      await expect(page.getByText(t('common.signedInAs', { email: c.email }))).toHaveCount(0)
      expect(await domHolds(page, c.email), 'the sign-in page holds no address of the user').toBe(false)
      await settled(page, 'login-signin-after-reload')
    })
  })

  test('the password signs in; sign out, then the Back button, show the sign-in page and nothing of the user', async ({ page, request }) => {
    test.setTimeout(120_000)
    const c = user('c')
    const session = await passwordSignIn(request, c)
    expect(session !== null, 'the password set by the first test of this file signs user c in (that test must pass first)').toBe(true)
    // a job of the user's own, so that "no job row" has something to be true about (a draft holds nothing and costs nothing)
    const draftId = await draftVerification(request, session!, `back-${runId()}@free.example.test`)

    await test.step('signing in with the password opens the dashboard', async () => {
      await signIn(page, c)
      await expect(page.getByText(t('common.signedInAs', { email: c.email }))).toBeVisible()
      expect(await holdsNoToken(page), 'storage and cookies hold no token').toBe(true)
    })

    await test.step('the page holds the user\'s data (the control of what follows)', async () => {
      await goTo(page, 'jobs')
      await expect(page.getByTestId(`job-row-${draftId}`)).toBeVisible()
      expect(await domHolds(page, c.email), 'the page holds the address of the user while signed in').toBe(true)
    })

    await test.step('sign out shows the sign-in page', async () => {
      await signOut(page)
      await expect(page).toHaveURL(/\/sign-in$/)
    })

    await test.step('the Back button shows the sign-in page and no data of the user', async () => {
      await page.goBack()
      await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
      await expect(page).toHaveURL(/\/sign-in$/)
      await expect(page.getByTestId(/^job-row-/)).toHaveCount(0)
      await expect(page.getByText(t('common.signedInAs', { email: c.email }))).toHaveCount(0)
      expect(await domHolds(page, c.email), 'the DOM holds no address of the user').toBe(false)
      expect(await domHolds(page, draftId), 'the DOM holds no job of the user').toBe(false)
      await expectNoInternalWords(page)
      await shot(page, 'login-signin-after-back')
    })

    await test.step('the protected pages stay closed: the address of one goes to sign in', async () => {
      // a typed address is a new page load (the session is gone anyway): it must not show the jobs
      await page.goto('/jobs')
      await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
      await expect(page.getByTestId(/^job-row-/)).toHaveCount(0)
    })
  })

  test('the same invite link, opened again, says it is no longer good, in the app\'s words', async ({ page }) => {
    const c = user('c')
    await page.goto(c.invite_link)
    await expect(page).toHaveURL(/\/sign-in\?notice=link_expired$/)
    await expect(page.getByText(t('auth.notice.linkExpired'))).toBeVisible()
    expect(await page.evaluate(() => window.location.hash === ''), 'the address bar holds no hash').toBe(true)
    // none of GoTrue's own words or codes reaches the page
    const text = await page.locator('body').innerText()
    expect(/otp_expired|access_denied|invalid or has expired|error_description/i.test(text), 'the page shows nothing of the sign-in service\'s own error').toBe(false)
    await settled(page, 'login-invite-used')
  })
})

test('a wrong password gets the calm wording, and nothing of the sign-in service\'s own', async ({ page }) => {
  // user a exists whatever the order of the files; the attempt changes nothing
  await page.goto('/')
  await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
  await settled(page, 'login-signin')

  await test.step('nothing typed: the form says what is missing and sends nothing', async () => {
    await page.getByRole('button', { name: t('auth.signIn.submit'), exact: true }).click()
    await expect(page.getByText(t('auth.signIn.missing'))).toBeVisible()
  })

  await test.step('a wrong password: the one calm line, the password field emptied', async () => {
    await page.getByLabel(t('auth.field.email'), { exact: true }).fill(user('a').email)
    await page.getByLabel(t('auth.field.password'), { exact: true }).fill(`not-the-password-${runId()}`)
    await page.getByRole('button', { name: t('auth.signIn.submit'), exact: true }).click()
    const alert = page.getByRole('alert').filter({ hasText: t('auth.error.invalid_credentials') })
    await expect(alert).toHaveText(t('auth.error.invalid_credentials'))
    await expect(page.getByLabel(t('auth.field.password'), { exact: true })).toHaveValue('')
    await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
    const text = await page.locator('body').innerText()
    expect(/invalid login credentials|invalid_grant|invalid_credentials|authapierror|supabase|gotrue|error_code|\b400\b/i.test(text), 'the page shows nothing of the sign-in service\'s own error').toBe(false)
    await settled(page, 'login-signin-wrong-password')
  })
})

test('"Forgot your password" gives the same confirmation for an address that has an account and for one that has not', async ({ page }) => {
  const known = user('c').email
  const unknown = `nobody-${runId()}@example.test`

  const ask = async (address: string): Promise<string> => {
    await page.goto('/')
    await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
    await page.getByRole('link', { name: t('auth.signIn.forgot'), exact: true }).click()
    await expect(h1(page, t('auth.forgot.title'))).toBeVisible()
    await page.getByLabel(t('auth.field.email'), { exact: true }).fill(address)
    await page.getByRole('button', { name: t('auth.forgot.submit'), exact: true }).click()
    const confirmation = page.getByRole('status').filter({ hasText: t('auth.forgot.sent') })
    await expect(confirmation).toBeVisible()
    return (await confirmation.innerText()).trim()
  }

  const forKnown = await ask(known)
  await settled(page, 'login-forgot-sent')
  const forUnknown = await ask(unknown)

  expect(forKnown, 'the confirmation for an address that has an account is the catalogue\'s line').toBe(t('auth.forgot.sent'))
  expect(forUnknown, 'the confirmation for an address that has none is the same line').toBe(forKnown)
  // neither answer names the address or says whether it has an account
  expect(forKnown.includes(known) || forUnknown.includes(unknown), 'the confirmation repeats no address').toBe(false)
  await page.getByRole('link', { name: t('auth.forgot.back'), exact: true }).click()
  await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
})
