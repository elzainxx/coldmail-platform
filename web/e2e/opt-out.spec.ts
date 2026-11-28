// A10, "a vendor opt-out": user a, Settings and the preview of a new verify job.
//
//   - Settings: the opt-out box is off, its disclosure and the line about what a change applies to are on the page
//   - a new verify job's preview says some addresses may be checked by an outside service, and says what a credit per address is
//   - the box is turned on: the confirmation is shown, it is still on after a reload (the session is in memory, so the reload asks for the sign in again and comes back to Settings), and the API's own
//     answer (`GET /v1/settings`, read with the session of the helper that signs in as the user, NOT through the page) says `vendor_fallback_opt_out: true`
//   - a new verify job's preview now says none of the addresses will be sent to an outside service, and the job the platform made keeps that setting; it never costs more than before
//   - the box is turned off again (and put back by the test's last act whatever happened)
//
// What the vendor price is in this stack: the e2e README says the vendor fallback is ON and reachable, so an opted-in preview reserves 2 credits an address and an opted-out one 1. This spec asserts what
// holds in EITHER configuration (the opted-out price is 1, the opted-in price is never lower) and that the page says exactly the number the platform's preview answered.
import { expect, test, type Page } from '@playwright/test'
import type { Job } from '../src/api/types.ts'
import { t } from '../src/copy/index.ts'
import { endJob, getSettings, putSettings } from './support/cross-api.ts'
import { checkScreen, fillSignIn, h1, openScreen, signIn } from './support/cross-ui.ts'
import { fixturePath } from './support/data.ts'
import { ready } from './support/users.ts'

const optOutBox = (page: Page) => page.getByRole('checkbox', { name: t('settings.vendor.label'), exact: true })
const toast = (page: Page, text: string) => page.getByRole('region', { name: t('common.toast.region') }).getByText(text, { exact: true })

interface PreviewedVerify {
  id: string
  /** what the platform's preview answered */
  unitCost: number
  optedOut: boolean
}

/** Chooses the fixture's verify list on the new verify page, presses "Check this list" and returns what the platform's preview answered; the screen is left on the preview. */
async function previewVerify(page: Page): Promise<PreviewedVerify> {
  await openScreen(page, 'newVerify')
  await page.getByLabel(t('newjob.csv.file'), { exact: true }).setInputFiles(fixturePath('verify-list.csv'))
  await expect(page.getByLabel(t('newjob.columns.address'))).toBeVisible()
  const previewed = page.waitForResponse((r) => /^\/v1\/jobs\/[0-9a-f-]+\/preview$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST' && r.status() === 200)
  await page.getByRole('button', { name: t('newjob.verify.check'), exact: true }).click()
  const job = ((await (await previewed).json()) as { job: Job }).job
  await expect(page.getByRole('heading', { name: t('newjob.preview.title'), exact: true })).toBeVisible()
  if (job.preview === null) throw new Error('the preview answered no preview')
  return { id: job.id, unitCost: job.preview.unit_cost, optedOut: job.settings.vendor_opt_out === true }
}

/** The preview's line "Credits per address checked: N" as the page says it. */
async function unitCostOnPage(page: Page): Promise<number> {
  const line = page.getByText(/^Credits per address checked: \d+$/)
  await expect(line).toBeVisible()
  return Number((await line.innerText()).replace(/\D/g, ''))
}

/** Lets the draft go (the button that turns away from the preview) and waits for the file picker to be back. */
async function leavePreview(page: Page): Promise<void> {
  await page.getByRole('button', { name: t('newjob.csv.change'), exact: true }).click()
  await expect(page.getByLabel(t('newjob.csv.file'), { exact: true })).toBeVisible()
}

test.describe('the vendor opt-out', () => {
  test('the box, its disclosure, the line on the verify preview, the saved setting that survives a reload, and the API says the same', async ({ page, request }) => {
    test.setTimeout(240_000)
    const session = await ready(request, 'a')
    const drafts: string[] = []
    await putSettings(request, session, false) // a known start: the users are shared by the run's specs

    try {
      await signIn(page, request, 'a', '/settings')

      await test.step('Settings: the opt-out is off, its disclosure and its scope are on the page', async () => {
        await expect(h1(page, t('settings.title'))).toBeVisible()
        await expect(page.getByRole('heading', { name: t('settings.vendor.title'), exact: true })).toBeVisible()
        await expect(optOutBox(page)).toBeVisible()
        await expect(optOutBox(page)).not.toBeChecked()
        await expect(page.getByText(t('settings.disclosure.vendor'), { exact: true })).toBeVisible()
        await expect(page.getByText(t('settings.vendor.scope'), { exact: true })).toBeVisible()
        await checkScreen(page, 'opt-settings-off')
      })

      let unitCostIn = 0
      await test.step('a new verify job: the preview says some addresses may be checked by an outside service', async () => {
        const made = await previewVerify(page)
        drafts.push(made.id)
        expect(made.optedOut, 'the job was made under the setting that was off').toBe(false)
        await expect(page.getByText(t('newjob.disclosure.vendor'))).toBeVisible()
        await expect(page.getByText(t('newjob.disclosure.vendorOptedOut'))).toHaveCount(0)
        await expect(page.getByRole('link', { name: t('newjob.disclosure.settingsLink'), exact: true })).toBeVisible()
        unitCostIn = await unitCostOnPage(page)
        expect(unitCostIn, 'the page says the number of credits per address that the platform\'s preview answered').toBe(made.unitCost)
        await checkScreen(page, 'opt-verify-preview-in')
        await leavePreview(page)
      })

      await test.step('turn the opt-out on: the confirmation is shown and the box is on', async () => {
        await openScreen(page, 'settings')
        // a click, not check(): the box follows the platform's answer, not the click, so it is not on the instant after it
        await optOutBox(page).click()
        await expect(toast(page, t('settings.vendor.savedOn'))).toBeVisible()
        await expect(optOutBox(page)).toBeChecked()
        await checkScreen(page, 'opt-settings-on')
      })

      await test.step('after a reload (and the sign in it asks for) the box is still on', async () => {
        await page.reload()
        await fillSignIn(page, 'a')
        await expect(h1(page, t('settings.title'))).toBeVisible()
        await expect(optOutBox(page)).toBeChecked()
      })

      await test.step('GET /v1/settings, read with the session of the API helper (not through the page), says vendor_fallback_opt_out: true', async () => {
        const settings = await getSettings(request, session)
        expect(settings.vendor_fallback_opt_out, 'GET /v1/settings after the box was turned on').toBe(true)
      })

      await test.step('a new verify job now says no address will be sent to an outside service, and keeps that setting', async () => {
        const made = await previewVerify(page)
        drafts.push(made.id)
        expect(made.optedOut, 'the job was made under the setting that is on').toBe(true)
        await expect(page.getByText(t('newjob.disclosure.vendorOptedOut'))).toBeVisible()
        await expect(page.getByText(t('newjob.disclosure.vendor'))).toHaveCount(0)
        await expect(page.getByText(t('newjob.disclosure.changedSince'))).toHaveCount(0)
        const unitCostOut = await unitCostOnPage(page)
        expect(unitCostOut, 'the page says the number of credits per address that the platform\'s preview answered').toBe(made.unitCost)
        expect(unitCostOut, 'an opted-out address costs one credit').toBe(1)
        expect(unitCostOut <= unitCostIn, 'opting out never costs more per address').toBe(true)
        await checkScreen(page, 'opt-verify-preview-out')
        await leavePreview(page)
      })

      await test.step('turn it off again: the confirmation is shown, the box is off, and the API says false', async () => {
        await openScreen(page, 'settings')
        await expect(optOutBox(page)).toBeChecked()
        await optOutBox(page).click()
        await expect(toast(page, t('settings.vendor.savedOff'))).toBeVisible()
        await expect(optOutBox(page)).not.toBeChecked()
        expect((await getSettings(request, session)).vendor_fallback_opt_out, 'GET /v1/settings after the box was turned off').toBe(false)
      })
    } finally {
      await putSettings(request, session, false)
      for (const id of drafts) await endJob(request, session, id)
    }
  })
})
