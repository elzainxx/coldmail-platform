// What the specs of chunks A10 and A11 do on the screens, written once: sign in THROUGH THE SCREEN, move by the navigation, walk the four steps of a send job, read the dashboard's balance, reach a
// control with the Tab key, and check a main screen (a picture, the internal-words check, axe) in one call. Every control is found by role and accessible name taken from the copy catalogue (`t()`),
// never by a class; `data-testid` only for the ones the spec of the phase lists (job-status-<id>, balance-<module>, sample-<n>, preview-frame, ...).
//
// THE SESSION IS IN MEMORY ONLY (web/README.md): a reload or `page.goto` signs the user out, and the app then asks for a sign in and brings the user back to where they were going (`?next=`). So
// `signIn(page, request, who, path)` opens the path, signs in on the page that appears, and lands on the path. Nothing here prints a password or a token.
import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import type { CopyKey } from '../../src/copy/index.ts'
import { t } from '../../src/copy/index.ts'
import { jobStatusText } from '../../src/copy/words.ts'
import { expectNoA11yViolations, type AxeOptions } from './axe.ts'
import { expectNoInternalText } from './cross-copy.ts'
import { shot } from './shots.ts'
import { ready, user, type UserTag } from './users.ts'

export type Who = Exclude<UserTag, 'c'>

/** A short id of this run for names that must not collide with another spec's (lower case letters and digits). */
export const runId = (): string => `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 5)}`

export const escapeRe = (text: string): string => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** The page's one `<h1>`. */
export const h1 = (page: Page, title: string): Locator => page.getByRole('heading', { level: 1, name: title, exact: true })

// ---- signing in ----

/** On the sign-in page that is showing: the address and password of the user, and the button. */
export async function fillSignIn(page: Page, who: Who): Promise<void> {
  const u = user(who)
  await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
  await page.getByLabel(t('auth.field.email'), { exact: true }).fill(u.email)
  await page.getByLabel(t('auth.field.password'), { exact: true }).fill(u.password)
  await page.getByRole('button', { name: t('auth.signIn.submit'), exact: true }).click()
  await expect(page.getByRole('navigation', { name: t('common.nav.label') })).toBeVisible()
}

/** Opens `path` (a protected page signs the visitor out to the sign-in page first), signs in as `who` through the screen and lands on `path`. */
export async function signIn(page: Page, request: APIRequestContext, who: Who, path = '/'): Promise<void> {
  await ready(request, who) // the password exists (the first call opens the invite without a browser)
  await page.goto(path)
  await fillSignIn(page, who)
  if (path !== '/') await expect(page).toHaveURL((url) => url.pathname + url.search === path)
}

// ---- moving about ----

export type Screen = 'dashboard' | 'jobs' | 'newVerify' | 'newSend' | 'accounts' | 'suppressions' | 'settings'

const SCREENS: Record<Screen, { nav: CopyKey; title: CopyKey }> = {
  dashboard: { nav: 'common.nav.dashboard', title: 'dashboard.title' },
  jobs: { nav: 'common.nav.jobs', title: 'jobs.title' },
  newVerify: { nav: 'common.nav.newVerify', title: 'newjob.verify.title' },
  newSend: { nav: 'common.nav.newSend', title: 'newjob.send.title' },
  accounts: { nav: 'common.nav.accounts', title: 'accounts.title' },
  suppressions: { nav: 'common.nav.suppressions', title: 'suppressions.title' },
  settings: { nav: 'common.nav.settings', title: 'settings.title' },
}

/** Follows the main navigation's link to a screen (no reload: the session stays) and waits for its heading. */
export async function openScreen(page: Page, screen: Screen): Promise<void> {
  const target = SCREENS[screen]
  await page.getByRole('navigation', { name: t('common.nav.label') }).getByRole('link', { name: t(target.nav), exact: true }).click()
  await expect(h1(page, t(target.title))).toBeVisible()
}

/** Nothing on the page says it is loading (the table's and the page's spinners are gone). */
export async function settled(page: Page): Promise<void> {
  await expect(page.getByRole('status').filter({ hasText: /^Loading/ })).toHaveCount(0)
}

export interface ScreenCheck {
  /** regions axe leaves out: only the sandboxed frame of a user's own HTML, never the page */
  axe?: AxeOptions
}

/** A main screen, judged: settled, one picture, no internal word, no axe violation. Call it once the screen shows its data. */
export async function checkScreen(page: Page, name: string, options: ScreenCheck = {}): Promise<void> {
  await settled(page)
  await shot(page, name)
  await expectNoInternalText(page)
  await expectNoA11yViolations(page, options.axe)
}

/** The same without the picture and axe (a screen already judged, in another state). */
export async function checkCopy(page: Page): Promise<void> {
  await settled(page)
  await expectNoInternalText(page)
}

/**
 * Does the page's own document hold the text in its MARKUP (the DOM text and attributes)? With `fields` the value of every input, text area and select is read too (a typed value is not in the markup).
 * Used to prove a secret is on no screen: a boolean, never the text.
 */
export async function pageHolds(page: Page, needle: string, fields = false): Promise<boolean> {
  return await page.evaluate(
    ({ n, withFields }) => {
      if (document.documentElement.outerHTML.includes(n)) return true
      if (!withFields) return false
      return Array.from(document.querySelectorAll<HTMLInputElement>('input, textarea, select')).some((el) => el.value.includes(n))
    },
    { n: needle, withFields: fields },
  )
}

// ---- the keyboard ----

/** Presses Tab until `target` has the focus; returns how many presses it took. Throws when it is not reached in `max` presses (it is not keyboard reachable, or it is not on the page). */
export async function tabTo(page: Page, target: Locator, max = 40): Promise<number> {
  for (let presses = 0; presses <= max; presses++) {
    if (await target.evaluate((el) => el === document.activeElement)) return presses
    await page.keyboard.press('Tab')
  }
  throw new Error(`the control was not reached by ${max} presses of the Tab key`)
}

// ---- the dashboard's balance ----

export interface Credits {
  available: number
  reserved: number
}

/** The credits of a metered module as the dashboard says them (`balance-m1`, `balance-m2`). The page must be the dashboard. */
export async function readCredits(page: Page, module: 'm1' | 'm2'): Promise<Credits> {
  const card = page.getByTestId(`balance-${module}`)
  await expect(card).toContainText(t('dashboard.balance.available'))
  const text = await card.innerText()
  const figure = (label: string): number => {
    const found = new RegExp(`${escapeRe(label)}\\s+([\\d,]+) credit`).exec(text)?.[1]
    if (found === undefined) throw new Error(`the ${module} balance card does not say "${label}" with a number of credits`)
    return Number(found.replace(/,/g, ''))
  }
  return { available: figure(t('dashboard.balance.available')), reserved: figure(t('dashboard.balance.reserved')) }
}

// ---- jobs on the screen ----

/** The status badge of a job (on the jobs table, the dashboard and the job page). */
export const jobStatus = (page: Page, id: string): Locator => page.getByTestId(`job-status-${id}`)

export async function expectJobStatus(page: Page, id: string, status: string, timeout = 60_000): Promise<void> {
  await expect(jobStatus(page, id)).toHaveText(jobStatusText(status), { timeout })
}

/** The id in the address of a job page (`/jobs/<id>`). */
export function jobIdOfUrl(page: Page): string {
  const id = new URL(page.url()).pathname.split('/')[2]
  if (id === undefined || id === '') throw new Error('the page is not a job page')
  return id
}

/** From the jobs table to one job's page, through its row (the table is read again when the page opens). */
export async function openJobFromTable(page: Page, id: string): Promise<void> {
  await openScreen(page, 'jobs')
  await page.getByTestId(`job-row-${id}`).getByRole('link').click()
  await expect(h1(page, t('jobs.detail.title'))).toBeVisible()
  await expect(jobStatus(page, id)).toBeVisible()
}

// ---- the four steps of a send job ----

export type SendStep = 'sender' | 'list' | 'message' | 'preview'

export interface SendWalk {
  sender: 'm2' | 'm3'
  file: { name: string; mimeType: string; buffer: Buffer }
  subject: string
  html: string
  /** run at each step once it is on the screen and has what it shows (a picture, a check) */
  at?: Partial<Record<SendStep, () => Promise<void>>>
  /** stop after this step (the default is the preview) */
  until?: SendStep
}

const stepHeading = (n: number, title: CopyKey): string => t('newjob.step.heading', { n, total: 4, title: t(title) })
const nextButton = (page: Page): Locator => page.getByRole('button', { name: t('newjob.step.next'), exact: true })

/** Sender, list, message and preview of a new send job, through the screen. The preview is the last thing it does (nothing is confirmed). */
export async function walkSendJob(page: Page, walk: SendWalk): Promise<void> {
  const until = walk.until ?? 'preview'

  await openScreen(page, 'newSend')
  await expect(page.getByRole('heading', { level: 2, name: stepHeading(1, 'newjob.step.sender') })).toBeVisible()
  await page.getByRole('radio', { name: t(walk.sender === 'm2' ? 'newjob.sender.managed' : 'newjob.sender.own'), exact: true }).check()
  await expect(page.getByText(walk.sender === 'm2' ? /credits for Managed sending/ : /Sender accounts ready to send: \d+|You have no sender account that is ready/)).toBeVisible()
  await walk.at?.sender?.()
  if (until === 'sender') return
  await nextButton(page).click()

  await expect(page.getByRole('heading', { level: 2, name: stepHeading(2, 'newjob.step.list') })).toBeVisible()
  await page.getByLabel(t('newjob.csv.file'), { exact: true }).setInputFiles(walk.file)
  await expect(page.getByLabel(t('newjob.columns.address'))).toBeVisible()
  await expect(nextButton(page)).toBeEnabled()
  await walk.at?.list?.()
  if (until === 'list') return
  await nextButton(page).click()

  await expect(page.getByRole('heading', { level: 2, name: stepHeading(3, 'newjob.step.message') })).toBeVisible()
  await page.getByLabel(t('newjob.authoring.subject'), { exact: true }).fill(walk.subject)
  await page.getByLabel(t('newjob.authoring.html'), { exact: true }).fill(walk.html)
  const toPreview = page.getByRole('button', { name: t('newjob.message.next'), exact: true })
  await expect(toPreview).toBeEnabled()
  await expect(page.getByText(t('newjob.authoring.ok'), { exact: true })).toBeVisible()
  await walk.at?.message?.()
  if (until === 'message') return
  await toPreview.click()

  await expect(page.getByRole('heading', { name: t('newjob.preview.title'), exact: true })).toBeVisible({ timeout: 30_000 })
  await expect(page.getByTestId('sample-1')).toBeVisible()
  await walk.at?.preview?.()
}

/** Presses "Confirm and start" on the preview and returns the id of the job the confirmation names (the page that says "Your job is confirmed"). */
export async function confirmSend(page: Page): Promise<string> {
  await page.getByRole('button', { name: t('newjob.confirm.submitNow'), exact: true }).click()
  await expect(page.getByRole('heading', { name: t('newjob.done.title'), exact: true })).toBeVisible({ timeout: 30_000 })
  const href = await page.getByRole('link', { name: t('newjob.done.open'), exact: true }).getAttribute('href')
  const id = /^\/jobs\/([0-9a-f-]{36})$/.exec(href ?? '')?.[1]
  if (id === undefined) throw new Error('the link of the confirmed job is not the address of a job page')
  return id
}
