// A11, "keyboard reachable, labelled inputs, contrast (automated)": user a, every main screen.
//
//   1 and 2  axe (WCAG 2 A and AA, contrast included) has no violation on any main screen, in the LIGHT theme and then in the DARK one (switched by the theme button of the page, not by a setting): sign in, forgot
//            password, dashboard, jobs, a job's page, new verify (empty, with a file, previewed), the four steps of a new send job, SMTP accounts (list and add form), suppressions, settings; every visible input,
//            select and text area has an accessible name. A screen that fails does not stop the tour: every failure is reported at the end, by screen, rule and node count.
//   3        the sign-in page by keyboard: Tab from the top reaches the skip link first, then every control in order, each with a visible focus ring (`:focus-visible`, an outline of 2px or more), and the skip
//            link moves to the main content
//   4        the app's screens by keyboard: the skip link is the first thing a Tab reaches, and Tab walks a run of controls, each with a ring
//   5        a dialog traps the focus and gives it back on Escape (the question that removes a suppressed address)
//   6        the paired controls of the checks of this file: a control with no name, a text with no contrast, a control with no ring are each REPORTED by the helper that is meant to report them
// The sandboxed frame of a user's own HTML is left out of axe (its content is the user's, not the app's); the page never is.
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import { expectNoA11yViolations } from './support/axe.ts'
import { expectEveryControlNamed, focusStop, hasVisibleRing, tabStops } from './support/cross-a11y.ts'
import { addSuppression, finishedVerifyJob, removeSuppression } from './support/cross-api.ts'
import { fillSignIn, openJobFromTable, openScreen, runId, settled, signIn, walkSendJob } from './support/cross-ui.ts'
import { csvFile, fixturePath, recipients, sendCsv } from './support/data.ts'
import { shot } from './support/shots.ts'
import { ready } from './support/users.ts'

const RUN = runId()
const WITHOUT_FRAMES = { exclude: ['[data-testid=preview-frame]'] }

type Theme = 'light' | 'dark'

let finished: Promise<string> | undefined
/** A finished verification job of user a (its page is the "job detail" screen): made once per worker. */
function finishedJob(request: APIRequestContext): Promise<string> {
  finished ??= (async () => (await finishedVerifyJob(request, await ready(request, 'a'), [`a11y-${RUN}-1@free.example.test`, `a11y-${RUN}-2@d1.example.test`])).id)()
  return finished
}

const themeButton = (page: Page, to: Theme) => page.getByRole('button', { name: t(to === 'dark' ? 'common.theme.toDark' : 'common.theme.toLight'), exact: true })

/** One screen: a picture, axe, and the names of its controls. A failure is recorded and the tour goes on. */
async function audit(page: Page, failures: string[], theme: Theme, screen: string, withFrames = false): Promise<void> {
  const name = `a11y-${theme}-${screen}`
  await test.step(name, async () => {
    try {
      await settled(page)
      const onPage = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
      expect(onPage === 'dark', `the page is in the ${theme} theme`).toBe(theme === 'dark')
      await shot(page, name)
      await expectNoA11yViolations(page, withFrames ? WITHOUT_FRAMES : {})
      await expectEveryControlNamed(page)
    } catch (e) {
      failures.push(`${name}: ${e instanceof Error ? e.message : 'failed'}`)
    }
  })
}

/** Every main screen in one theme; the failures of all of them are collected and reported together. */
async function tour(page: Page, request: APIRequestContext, theme: Theme): Promise<void> {
  const failures: string[] = []
  const session = await ready(request, 'a')
  const jobId = await finishedJob(request)
  const suppressed = `a11y-${RUN}-${theme}@example.test`
  await addSuppression(request, session, suppressed)

  try {
    await page.goto('/sign-in')
    await expect(themeButton(page, 'dark')).toBeVisible() // the page starts in the light theme
    if (theme === 'dark') {
      await themeButton(page, 'dark').click()
      await expect(themeButton(page, 'light')).toBeVisible()
    }
    await audit(page, failures, theme, 'sign-in')
    await page.getByRole('link', { name: t('auth.signIn.forgot'), exact: true }).click()
    await expect(page.getByRole('heading', { level: 1, name: t('auth.forgot.title'), exact: true })).toBeVisible()
    await audit(page, failures, theme, 'forgot-password')
    await page.getByRole('link', { name: t('auth.forgot.back'), exact: true }).click()
    await fillSignIn(page, 'a')

    await audit(page, failures, theme, 'dashboard')
    await openScreen(page, 'jobs')
    await audit(page, failures, theme, 'jobs')
    await openJobFromTable(page, jobId)
    await audit(page, failures, theme, 'job-detail', true)

    await openScreen(page, 'newVerify')
    await audit(page, failures, theme, 'verify-empty')
    await page.getByLabel(t('newjob.csv.file'), { exact: true }).setInputFiles(fixturePath('verify-list.csv'))
    await expect(page.getByLabel(t('newjob.columns.address'))).toBeVisible()
    await audit(page, failures, theme, 'verify-file')
    await page.getByRole('button', { name: t('newjob.verify.check'), exact: true }).click()
    await expect(page.getByRole('heading', { name: t('newjob.preview.title'), exact: true })).toBeVisible()
    await audit(page, failures, theme, 'verify-preview')
    await page.getByRole('button', { name: t('newjob.csv.change'), exact: true }).click() // lets the draft go

    await walkSendJob(page, {
      sender: 'm2',
      file: csvFile('a11y.csv', sendCsv(recipients(`a11y-${RUN}`, 3), [['Ann', 'a'], ['Bo', 'b'], ['Cy', 'c']])),
      subject: 'Accessibility {{first_name|there}}',
      html: '<p>Hello {{first_name|there}}</p>',
      at: {
        sender: () => audit(page, failures, theme, 'send-sender'),
        list: () => audit(page, failures, theme, 'send-list'),
        message: () => audit(page, failures, theme, 'send-message', true),
        preview: () => audit(page, failures, theme, 'send-preview', true),
      },
    })
    await page.getByRole('button', { name: t('newjob.send.editMessage'), exact: true }).click() // lets the draft go

    await openScreen(page, 'accounts')
    await audit(page, failures, theme, 'accounts')
    await page.getByRole('button', { name: t('accounts.add.open'), exact: true }).click()
    await expect(page.getByRole('heading', { name: t('accounts.add.title'), exact: true })).toBeVisible()
    await audit(page, failures, theme, 'accounts-add-form')
    await page.getByRole('button', { name: t('common.cancel'), exact: true }).click()

    await openScreen(page, 'suppressions')
    await expect(page.getByText(suppressed, { exact: true })).toBeVisible()
    await audit(page, failures, theme, 'suppressions')

    await openScreen(page, 'settings')
    await expect(page.getByRole('checkbox', { name: t('settings.vendor.label'), exact: true })).toBeVisible()
    await audit(page, failures, theme, 'settings')
  } finally {
    await removeSuppression(request, session, suppressed).catch(() => undefined)
  }

  expect(failures.length, `${failures.length} screen(s) of the ${theme} theme fail:\n${failures.join('\n')}`).toBe(0)
}

test.describe('accessibility of the main screens', () => {
  test('light theme: no axe violation on any main screen, and every control has a name', async ({ page, request }) => {
    test.setTimeout(420_000)
    await ready(request, 'a')
    await tour(page, request, 'light')
  })

  test('dark theme (switched by the theme button): no axe violation on any main screen, and every control has a name', async ({ page, request }) => {
    test.setTimeout(420_000)
    await ready(request, 'a')
    await tour(page, request, 'dark')
  })
})

test.describe('the keyboard', () => {
  test('the sign-in page: Tab from the top reaches the skip link first, then every control in order, each with a visible ring; the skip link works', async ({ page }) => {
    await page.goto('/sign-in')
    await expect(page.getByRole('heading', { level: 1, name: t('auth.signIn.title'), exact: true })).toBeVisible()

    const stops = await tabStops(page, 6)
    expect(stops.map((s) => s.tag), 'the order of the controls the keyboard reaches').toEqual(['a', 'button', 'input', 'input', 'button', 'a'])
    expect(stops[0]?.name, 'the first stop is the skip link').toBe(t('common.skipToContent'))
    expect(stops[1]?.name, 'the second stop is the theme button').toBe(t('common.theme.toDark'))
    expect(stops[4]?.name, 'the fifth stop is the sign in button').toBe(t('auth.signIn.submit'))
    expect(stops[5]?.name, 'the sixth stop is the forgot password link').toBe(t('auth.signIn.forgot'))
    const noRing = stops.filter((s) => !hasVisibleRing(s)).map((s) => `${s.tag} "${s.name}" (outline ${s.outlineStyle} ${s.outlineWidthPx}px, focus-visible ${s.focusVisible})`)
    expect(noRing.length, `these controls have no visible focus ring: ${noRing.join('; ')}`).toBe(0)
    await shot(page, 'a11y-keyboard-sign-in')

    // the skip link, activated from the keyboard, puts the focus on the main content
    await page.reload()
    await page.keyboard.press('Tab')
    expect((await focusStop(page)).name).toBe(t('common.skipToContent'))
    await page.keyboard.press('Enter')
    await expect(page.getByRole('main')).toBeFocused()
  })

  test('the app\'s screens: the skip link comes first in the tab order, and a run of controls is walked, each with a visible ring', async ({ page, request }) => {
    test.setTimeout(240_000)
    await signIn(page, request, 'a')
    const screens = ['dashboard', 'jobs', 'newVerify', 'accounts', 'suppressions', 'settings'] as const
    for (const screen of screens) {
      if (screen !== 'dashboard') await openScreen(page, screen)
      await settled(page)
      const first = await page.evaluate(() => {
        const el = document.querySelector('a[href], button:not([disabled]), input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')
        return el?.textContent?.trim() ?? ''
      })
      expect(first, `${screen}: the first control of the page in the tab order is the skip link`).toBe(t('common.skipToContent'))

      const stops = (await tabStops(page, 14)).filter((s) => s.tag !== 'body')
      expect(stops.length, `${screen}: a run of controls is reached by the Tab key`).toBeGreaterThanOrEqual(8)
      expect(new Set(stops.map((s) => `${s.tag}:${s.name}`)).size, `${screen}: the stops are different controls`).toBeGreaterThanOrEqual(6)
      const noRing = stops.filter((s) => !hasVisibleRing(s)).map((s) => `${s.tag} "${s.name}" (outline ${s.outlineStyle} ${s.outlineWidthPx}px)`)
      expect(noRing.length, `${screen}: these controls have no visible focus ring: ${noRing.join('; ')}`).toBe(0)
    }
  })

  test('a dialog traps the focus and gives it back on Escape (the question that removes a suppressed address)', async ({ page, request }) => {
    test.setTimeout(180_000)
    const session = await ready(request, 'a')
    const address = `a11y-dialog-${RUN}@example.test`
    await addSuppression(request, session, address)
    try {
      await signIn(page, request, 'a', '/suppressions')
      const opener = page.getByRole('button', { name: t('suppressions.remove.buttonFor', { email: address }), exact: true })
      await expect(opener).toBeVisible()
      await opener.focus()
      await page.keyboard.press('Enter')

      const dialog = page.getByRole('alertdialog', { name: t('suppressions.remove.title', { email: address }) })
      await expect(dialog).toBeVisible()
      await expect(dialog).toHaveAttribute('aria-modal', 'true')
      await expect(dialog.getByRole('button', { name: t('common.cancel'), exact: true })).toBeFocused() // the safe button has the focus

      for (let i = 0; i < 6; i++) {
        await page.keyboard.press('Tab')
        await expect(dialog.locator(':focus'), `Tab ${i + 1}: the focus stays in the dialog`).toHaveCount(1)
      }
      for (let i = 0; i < 6; i++) {
        await page.keyboard.press('Shift+Tab')
        await expect(dialog.locator(':focus'), `Shift+Tab ${i + 1}: the focus stays in the dialog`).toHaveCount(1)
      }
      await shot(page, 'a11y-dialog-open')

      await page.keyboard.press('Escape')
      await expect(dialog).toBeHidden()
      await expect(opener).toBeFocused() // the focus is back where it was
      await expect(page.getByText(address, { exact: true })).toBeVisible() // and Escape removed nothing
    } finally {
      await removeSuppression(request, session, address).catch(() => undefined)
    }
  })
})

test.describe('the checks of this file can fail', () => {
  const PAGE = (body: string): string => `<!doctype html><html lang="en"><head><title>Control</title></head><body><main><h1>Control</h1>${body}</main></body></html>`

  test('CONTROL: a control with no name is reported, a labelled one is not', async ({ page }) => {
    await page.setContent(PAGE('<label for="n">Name</label><input id="n" type="text"><select aria-label="Choice"><option>a</option></select>'))
    await expectEveryControlNamed(page)
    await page.setContent(PAGE('<input type="text" placeholder="no label, only a placeholder">'))
    await expect(expectEveryControlNamed(page)).rejects.toThrow(/no accessible name/)
    await page.setContent(PAGE('<input type="checkbox">'))
    await expect(expectEveryControlNamed(page)).rejects.toThrow(/no accessible name/)
  })

  test('CONTROL: text with too little contrast is reported by axe, by rule', async ({ page }) => {
    await page.setContent(PAGE('<p style="color:#c8c8c8;background:#ffffff">Words in a light grey on white.</p>'))
    await expect(expectNoA11yViolations(page)).rejects.toThrow(/color-contrast/)
  })

  test('CONTROL: a control with no focus ring is reported, one with a 2px ring is not', async ({ page }) => {
    await page.setContent(PAGE('<button id="bare" style="outline:none">bare</button><button id="ringed" style="outline-offset:2px" class="r">ringed</button><style>.r:focus-visible{outline:2px solid #0050a0}</style>'))
    const [bare, ringed] = await tabStops(page, 2)
    expect(bare !== undefined && hasVisibleRing(bare), 'a button with outline:none has a ring').toBe(false)
    expect(ringed !== undefined && hasVisibleRing(ringed), 'a button with a 2px focus-visible outline has a ring').toBe(true)
  })
})
