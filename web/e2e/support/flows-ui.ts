// The flows of the screens that more than one spec drives: sign in through the screen, move by the navigation, settle a screen (axe, the copy deny-list, a picture), read the figures of a preview and the
// rows of a results table, create a verify job from a CSV, wait for a job's status, download the results. Every control is found by ROLE and ACCESSIBLE NAME taken from the copy catalogue (`t()`), never by a
// CSS class; the only test ids are the ones the spec lists (job-row-<id>, job-status-<id>, balance-<module>, sample-<n>, preview-frame, results-row-<ordinal>, suppression-row-<n>).
//
// THE SESSION IS IN MEMORY (decision 2): `page.goto(...)` or a reload of a signed-in page signs the user out. After `signIn`, a flow MOVES BY THE NAVIGATION (`goTo`), which is a client-side route change.
//
// Nothing here prints a password, a token or an invite link; an assertion message states what is checked, never a value.
import { mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { t, type CopyKey } from '../../src/copy/index.ts'
import { expectNoA11yViolations } from './axe.ts'
import { expectNoInternalWords } from './copy.ts'
import { shotsDir } from './env.ts'
import { escapeRegExp, numberIn } from './flows-data.ts'
import { shot } from './shots.ts'
import { ready, user, type Session, type UserTag } from './users.ts'

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// the page, the heading, the navigation
// ---------------------------------------------------------------------------------------------------------------------------------------------------

/** The page's one `h1` (every page has exactly one). */
export function h1(page: Page, name: string): Locator {
  return page.getByRole('heading', { level: 1, name, exact: true })
}

/** An `h2` (a card or a step). */
export function h2(page: Page, name: string): Locator {
  return page.getByRole('heading', { level: 2, name, exact: true })
}

export type NavTarget = 'dashboard' | 'jobs' | 'newVerify' | 'newSend' | 'suppressions'

const NAV: Record<NavTarget, { link: CopyKey; title: CopyKey }> = {
  dashboard: { link: 'common.nav.dashboard', title: 'dashboard.title' },
  jobs: { link: 'common.nav.jobs', title: 'jobs.title' },
  newVerify: { link: 'common.nav.newVerify', title: 'newjob.verify.title' },
  newSend: { link: 'common.nav.newSend', title: 'newjob.send.title' },
  suppressions: { link: 'common.nav.suppressions', title: 'suppressions.title' },
}

/** The main navigation, followed by a click (a route change: the in-memory session survives, a `page.goto` would end it). */
export async function goTo(page: Page, target: NavTarget): Promise<void> {
  const { link, title } = NAV[target]
  await page.getByRole('navigation', { name: t('common.nav.label') }).getByRole('link', { name: t(link), exact: true }).click()
  await expect(h1(page, t(title))).toBeVisible()
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// signing in and out through the screens
// ---------------------------------------------------------------------------------------------------------------------------------------------------

/** Signs in on the sign-in page with an email and a password and waits for the dashboard. The user must already have a password (`ready` sets it for a, b and s). */
export async function signIn(page: Page, who: { email: string; password: string }): Promise<void> {
  await page.goto('/')
  await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
  await page.getByLabel(t('auth.field.email'), { exact: true }).fill(who.email)
  await page.getByLabel(t('auth.field.password'), { exact: true }).fill(who.password)
  await page.getByRole('button', { name: t('auth.signIn.submit'), exact: true }).click()
  await expect(h1(page, t('dashboard.title'))).toBeVisible()
}

/** The one call a spec about a screen starts with: the user has a password (no screen), the browser signs in through the sign-in page, and the session is returned for the spec's API calls. */
export async function openAs(page: Page, request: APIRequestContext, tag: Exclude<UserTag, 'c'>): Promise<Session> {
  const session = await ready(request, tag)
  await signIn(page, user(tag))
  return session
}

/** The Sign out button of the top bar; waits for the sign-in page. */
export async function signOut(page: Page): Promise<void> {
  await page.getByRole('button', { name: t('common.signOut'), exact: true }).click()
  await expect(h1(page, t('auth.signIn.title'))).toBeVisible()
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// settling a screen
// ---------------------------------------------------------------------------------------------------------------------------------------------------

export interface SettleOptions {
  /** more regions axe leaves out (the sandboxed preview of a user's own HTML is always left out: it is their content, not the app's) */
  exclude?: string[]
  /** a narrower deny-list than the whole one (a screen that must say one of the denied words on purpose; none does today) */
  words?: string[]
}

/**
 * The three things every main screen gets once it has settled (no spinner left): the readable text holds no internal word, axe finds no WCAG 2 A or AA violation (contrast included), and a picture is taken.
 * `name` is the picture's name (lower case, digits, hyphens): one per screen and state, unique across the specs.
 */
export async function settled(page: Page, name: string, options: SettleOptions = {}): Promise<void> {
  await expect(page.getByRole('status').filter({ hasText: t('common.loading') })).toHaveCount(0)
  await expectNoInternalWords(page, options.words)
  await expectNoA11yViolations(page, { exclude: ['[data-testid=preview-frame]', ...(options.exclude ?? [])] })
  await shot(page, name)
}

/**
 * A picture of ONE element (the gallery's full-page pictures show a sandboxed frame as an empty white box: a frame with no origin is painted by another process, which a full-page capture does not wait for;
 * a picture of the frame itself shows what is in it). Same naming rule and directory as `shot`.
 */
export async function shotFrame(frame: Locator, name: string): Promise<string> {
  if (!/^[a-z0-9][a-z0-9-]*$/.test(name)) throw new Error(`the screenshot name '${name}' must be lower case letters, digits and hyphens, starting with a letter or a digit`)
  mkdirSync(shotsDir(), { recursive: true })
  const path = join(shotsDir(), `${name}.png`)
  await frame.screenshot({ path, animations: 'disabled' })
  return path
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// the dashboard
// ---------------------------------------------------------------------------------------------------------------------------------------------------

export interface CreditBalance {
  available: number
  reserved: number
}

/** The two figures of a balance card of the dashboard ("Available", "Set aside for jobs"), read from the page now. */
export async function readBalance(page: Page, module: 'm1' | 'm2'): Promise<CreditBalance> {
  const amounts = page.getByTestId(`balance-${module}`).getByText(/^[\d,]+ credits?$/)
  await expect(amounts).toHaveCount(2)
  const [available, reserved] = await amounts.allInnerTexts()
  return { available: numberIn(available ?? ''), reserved: numberIn(reserved ?? '') }
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// the new-job screens
// ---------------------------------------------------------------------------------------------------------------------------------------------------

export type CsvPayload = string | { name: string; mimeType: string; buffer: Buffer }

/** Chooses the CSV file with the browser's own file chooser (the control is the native file input: its label is "CSV file"). */
export async function chooseCsv(page: Page, file: CsvPayload): Promise<void> {
  const input = page.getByLabel(t('newjob.csv.file'), { exact: true })
  const [chooser] = await Promise.all([page.waitForEvent('filechooser'), input.click()])
  await chooser.setFiles(file)
}

/**
 * One figure of a preview ("Rows in your list", "Messages that will be sent"): the value beside its label. The figures are a definition list, so the label is a `term` and the value its `definition`.
 * The extra line some figures carry under the value (the reasons, the field names) is the second definition: this is the first.
 */
export function stat(page: Page, label: string): Locator {
  return page
    .getByRole('term')
    .filter({ hasText: new RegExp(`^${escapeRegExp(label)}$`) })
    .locator('xpath=..')
    .getByRole('definition')
    .first()
}

/** The extra line under a figure (the second definition of its row), or an empty locator when it has none. */
export function statNote(page: Page, label: string): Locator {
  return page
    .getByRole('term')
    .filter({ hasText: new RegExp(`^${escapeRegExp(label)}$`) })
    .locator('xpath=..')
    .getByRole('definition')
    .nth(1)
}

/** A figure as a number. */
export async function statNumber(page: Page, label: string): Promise<number> {
  await expect(stat(page, label)).toBeVisible()
  return numberIn(await stat(page, label).innerText())
}

/** The price of one unit as the preview says it ("Credits per address checked: 2" / "Credits per message: 2"). */
export async function unitCost(page: Page, kind: 'check' | 'send'): Promise<number> {
  const sentence = t(kind === 'check' ? 'newjob.preview.unitCostCheck' : 'newjob.preview.unitCostSend', { cost: '@@' })
  const line = page.getByText(new RegExp(`^${escapeRegExp(sentence).replace('@@', '\\d[\\d,]*')}$`))
  await expect(line).toBeVisible()
  return numberIn(await line.innerText())
}

/** The job id in an address of the app (`/jobs/<id>` or `?job=<id>`). */
export function jobIdOf(href: string): string {
  const m = /([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/.exec(href)
  if (m?.[1] === undefined) throw new Error('no job id in the address')
  return m[1]
}

/**
 * Confirms the job being looked at ("Start now" is the default) and returns its id once the confirmation is on the screen. The confirmation page is NOT left: the caller reads its figures, then
 * `openConfirmedJob`.
 */
export async function confirmNow(page: Page): Promise<string> {
  await page.getByRole('button', { name: t('newjob.confirm.submitNow'), exact: true }).click()
  await expect(h2(page, t('newjob.done.title'))).toBeVisible()
  const href = await page.getByRole('link', { name: t('newjob.done.open'), exact: true }).getAttribute('href')
  return jobIdOf(href ?? '')
}

/** From the confirmation to the job's page. */
export async function openConfirmedJob(page: Page): Promise<void> {
  await page.getByRole('link', { name: t('newjob.done.open'), exact: true }).click()
  await expect(h1(page, t('jobs.detail.title'))).toBeVisible()
}

/**
 * A verify job from a CSV list, start to confirm: the navigation to the New verify job screen, the file chosen with the file chooser, "Check this list", "Confirm and start". Returns the job's id with
 * the confirmation still on the screen (call `openConfirmedJob`). For a spec that asserts the steps in between, drive them itself with `chooseCsv` and `confirmNow`.
 */
export async function createVerifyJobFromCsv(page: Page, file: CsvPayload): Promise<string> {
  await goTo(page, 'newVerify')
  await chooseCsv(page, file)
  await page.getByRole('button', { name: t('newjob.verify.check'), exact: true }).click()
  await expect(h2(page, t('newjob.preview.title'))).toBeVisible()
  return confirmNow(page)
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// a job's page
// ---------------------------------------------------------------------------------------------------------------------------------------------------

export type JobStatusKey = 'draft' | 'previewed' | 'confirmed' | 'scheduled' | 'running' | 'paused' | 'cancelling' | 'completing' | 'succeeded' | 'cancelled' | 'expired' | 'failed'

const ENDED: JobStatusKey[] = ['succeeded', 'failed', 'cancelled', 'expired']

/** The word the app uses for a job's status ("Finished", "Running"). */
export function statusWord(status: JobStatusKey): string {
  return t(`common.jobStatus.${status}`)
}

/**
 * Waits, on the job's page (or any page that shows `job-status-<id>`), until the job is in the wanted status. The page asks again every 3 seconds on its own (the app's rate); the wait ends at once if
 * the job ENDED in another status than the wanted one (a failed job does not make a spec wait out its timeout), and `timeoutMs` is generous because it covers the engine, the module and the sender.
 */
export async function waitForJobStatus(page: Page, jobId: string, want: JobStatusKey, timeoutMs = 180_000): Promise<void> {
  const badge = page.getByTestId(`job-status-${jobId}`)
  const wanted = statusWord(want)
  const others = ENDED.filter((s) => s !== want).map(statusWord)
  let last = ''
  await expect
    .poll(
      async () => {
        try {
          last = (await badge.first().innerText({ timeout: 2_000 })).trim()
        } catch {
          last = ''
        }
        return last === wanted || others.includes(last)
      },
      { message: `the job reaches '${wanted}' (or ends in another state)`, timeout: timeoutMs, intervals: [500, 1000, 2000] },
    )
    .toBe(true)
  expect(last, `the job ended in '${last}' and not in '${wanted}'`).toBe(wanted)
}

export interface ResultRow {
  ordinal: number
  address: string
  /** the badge's word ("Risky", "Left out") */
  outcome: string
  /** "Free", "Charged", "Charged (second lookup)", "Not charged" */
  cost: string
  details: string
}

/** The rows of the results table as they are on the page now (one page of it), in the order shown. */
export async function readResultRows(page: Page): Promise<ResultRow[]> {
  const rows = await page.getByTestId(/^results-row-\d+$/).evaluateAll((els) =>
    els.map((el) => ({
      id: el.getAttribute('data-testid') ?? '',
      cells: Array.from(el.children).map((c) => (c as HTMLElement).innerText.trim()),
    })),
  )
  return rows.map((r) => ({
    ordinal: Number(r.id.replace('results-row-', '')),
    address: r.cells[0] ?? '',
    outcome: r.cells[1] ?? '',
    cost: r.cells[2] ?? '',
    details: r.cells[3] ?? '',
  }))
}

/** One row of the results table by its ordinal. */
export function resultRow(page: Page, ordinal: number): Locator {
  return page.getByTestId(`results-row-${ordinal}`)
}

/** Presses "Download CSV" and returns the file the browser received: its suggested name and its text. */
export async function downloadResults(page: Page): Promise<{ filename: string; text: string }> {
  const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: t('jobs.csv.button'), exact: true }).click()])
  return { filename: download.suggestedFilename(), text: readFileSync(await download.path(), 'utf8') }
}

/** The name the app gives the results file of a job: `results-<kind>-<day>-<first 8 of the id>.csv`, as a regular expression. */
export function resultsFileNamePattern(kind: 'm1' | 'm2' | 'm3', jobId: string): RegExp {
  const id = jobId.replace(/[^a-z0-9]/gi, '').slice(0, 8)
  const template = t('jobs.csv.fileName', { kind: t(`jobs.csv.kind.${kind}`), date: '@DATE@', id })
  return new RegExp(`^${escapeRegExp(template).replace('@DATE@', '\\d{4}-\\d{2}-\\d{2}')}$`)
}

/** The word of a result state in the catalogue ("Risky", "Left out"), for comparing a table with a downloaded file. */
export function resultStateWord(state: string): string {
  return t(`common.resultState.${state}` as CopyKey)
}
