// A10, "pause, resume and cancel": user a, the job page, and the sink slowed down so that a job lasts long enough to be paused (the sink holds each message to the spec's own recipients for a few seconds
// and the sender works one lane at a time).
//
//   1  module 2, by KEYBOARD: Pause (status "Paused", nothing new reaches the sink for as long as it is paused), Resume ("Running" again, messages flow), Cancel through its confirmation (the dialog traps
//      the keyboard and Escape gives the focus back); the job ends "Cancelled", the sink holds exactly the messages of the results that say "Sent", the unsent results are free, and the balance of the dashboard
//      says the unsent credits came back
//   2  module 3, by mouse: the same three moves; a module 3 job holds no credits, so its proof is the sink and the results
//   3  module 1: a verification job shows NO Pause (module 1 cannot pause a running job) and says so, and it can be cancelled
//
// Everything the sink is told is for this spec's recipients (`recipients`, unique to the run) and is put back to the default at the end: the sink is shared by the whole run and is never reset.
import { expect, test, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import {
  allResults,
  createSendDraft,
  createVerifyDraft,
  endJob,
  getBalances,
  startJob,
  type SendModule,
} from './support/cross-api.ts'
import { countFor, recipientsReached, settledCount } from './support/cross-sink.ts'
import {
  checkScreen,
  escapeRe,
  expectJobStatus,
  openJobFromTable,
  openScreen,
  readCredits,
  runId,
  signIn,
  tabTo,
} from './support/cross-ui.ts'
import { recipients } from './support/data.ts'
import { addReadyAccount, removeAccountsStartingWith } from './support/m3-accounts.ts'
import { shot } from './support/shots.ts'
import { sinkMode } from './support/sink.ts'
import { ready, type Session } from './support/users.ts'

const RUN = runId()

/** How many rows the verification job of test 3 has: long enough that module 1 is still working when the page has been read (a guess: the first run says whether it needs to be larger). */
const VERIFY_ROWS = Number(process.env['E2E_HOLD_VERIFY_ROWS'] ?? '20000')

const controls = (page: Page) => ({
  group: page.getByRole('group', { name: t('jobs.controls.label') }),
  pause: page.getByRole('button', { name: t('jobs.control.pause'), exact: true }),
  resume: page.getByRole('button', { name: t('jobs.control.resume'), exact: true }),
  // NOTE: while the cancel question is open the dialog's own confirm button has the same name: use this only when no dialog is open
  cancel: page.getByRole('button', { name: t('jobs.control.cancel'), exact: true }),
})

/** Press a control: with the keyboard (Tab until it has the focus, then the key) or with the mouse. */
async function press(page: Page, target: Locator, keyboard: boolean, key: 'Enter' | 'Space' = 'Enter'): Promise<void> {
  if (!keyboard) {
    await target.click()
    return
  }
  await tabTo(page, target)
  await page.keyboard.press(key)
}

const toast = (page: Page, text: string): Locator => page.getByRole('region', { name: t('common.toast.region') }).getByText(text, { exact: true })

interface Exercise {
  id: string
  addresses: string[]
  module: SendModule
  keyboard: boolean
}

/** Pause, resume and cancel of a send job that is running, with everything the screen and the sink must say at each point. Returns what the results say at the end. */
async function exercisePauseResumeCancel(page: Page, request: APIRequestContext, session: Session, x: Exercise) {
  const c = controls(page)
  const shots = `ctl-${x.module}`

  await test.step('the job runs: Pause and Cancel are offered, Resume is not', async () => {
    await expectJobStatus(page, x.id, 'running', 120_000)
    await expect(c.pause).toBeVisible()
    await expect(c.cancel).toBeVisible()
    await expect(c.resume).toHaveCount(0)
    await checkScreen(page, `${shots}-job-running`)
  })

  let pausedCount = 0
  await test.step('Pause: the job reads "Paused", offers Resume and Cancel, and nothing new reaches the sink', async () => {
    await press(page, c.pause, x.keyboard)
    await expect(toast(page, t('jobs.toast.paused'))).toBeVisible()
    await expectJobStatus(page, x.id, 'paused', 15_000)
    await expect(c.pause).toHaveCount(0)
    await expect(c.resume).toBeVisible()
    await expect(c.cancel).toBeVisible()
    await expect(page.getByText(t('jobs.controls.pausedLimit'), { exact: true })).toBeVisible()
    await checkScreen(page, `${shots}-job-paused`)
    // what was in flight finishes (the sink holds a message for seconds); then NOTHING moves while the job is paused
    pausedCount = await settledCount(request, x.addresses, 7000)
    expect(pausedCount, 'the sink is quiet for 7 seconds while the job is paused, and the list is not all sent yet').toBeLessThan(x.addresses.length)
  })

  await test.step('Resume: the job reads "Running" again and messages flow again', async () => {
    await press(page, c.resume, x.keyboard, 'Space')
    await expect(toast(page, t('jobs.toast.resumedRunning'))).toBeVisible()
    await expectJobStatus(page, x.id, 'running', 15_000)
    await expect(c.pause).toBeVisible()
    await expect(c.resume).toHaveCount(0)
    await expect.poll(() => countFor(request, x.addresses), { message: 'a message reaches the sink after the resume', timeout: 60_000 }).toBeGreaterThan(pausedCount)
  })

  await test.step('Cancel asks first, in words, and traps the keyboard in the question', async () => {
    const dialog = page.getByRole('alertdialog', { name: t('jobs.cancel.title') })
    await press(page, c.cancel, x.keyboard)
    await expect(dialog).toBeVisible()
    await expect(dialog.getByText(t(`jobs.cancel.started.${x.module}`), { exact: true })).toBeVisible()
    await expect(dialog.getByText(t('jobs.cancel.stopping'), { exact: true })).toBeVisible()
    await shot(page, `${shots}-cancel-question`)
    const keep = dialog.getByRole('button', { name: t('jobs.cancel.keep'), exact: true })
    const confirm = dialog.getByRole('button', { name: t('jobs.cancel.confirm'), exact: true })

    if (x.keyboard) {
      // the safe button has the focus (Enter never cancels by accident), Tab stays inside the question, Escape closes it and the focus goes back to the control that opened it
      await expect(keep).toBeFocused()
      for (let i = 0; i < 6; i++) {
        await page.keyboard.press('Tab')
        await expect(dialog.locator(':focus')).toHaveCount(1)
      }
      await page.keyboard.press('Escape')
      await expect(dialog).toBeHidden()
      await expect(c.cancel).toBeFocused()
      await expectJobStatus(page, x.id, 'running', 5_000) // Escape cancelled nothing
      await page.keyboard.press('Enter')
      await expect(dialog).toBeVisible()
      await tabTo(page, confirm)
      await page.keyboard.press('Enter')
    } else {
      await confirm.click()
    }
    await expect(page.getByRole('region', { name: t('common.toast.region') }).getByText(new RegExp(`^(${escapeRe(t('jobs.toast.cancelling'))}|${escapeRe(t('jobs.toast.cancelled'))})$`))).toBeVisible()
  })

  await test.step('the job ends "Cancelled" and offers no control', async () => {
    await expectJobStatus(page, x.id, 'cancelled', 120_000)
    await expect(c.group).toHaveCount(0)
    await expect(page.getByText(t('jobs.now.cancelled'), { exact: true })).toBeVisible()
  })

  const results = await allResults(request, session, x.id)
  const sent = results.filter((r) => r.state === 'sent')
  const cancelled = results.filter((r) => r.state === 'cancelled')

  await test.step('the results: what was sent is "Sent", what was not is "Cancelled" and free', async () => {
    expect(sent.length + cancelled.length, `every row ends sent or cancelled (${results.length} results: ${sent.length} sent, ${cancelled.length} cancelled)`).toBe(x.addresses.length)
    expect(cancelled.length, 'the cancel came before the end of the list').toBeGreaterThan(0)
    expect(sent.length, 'something was sent before the cancel').toBeGreaterThan(0)
    expect(cancelled.every((r) => r.decision === 'free'), 'an unsent row is free').toBe(true)
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(x.addresses.length)
    const table = page.getByRole('table', { name: t('jobs.results.caption') })
    await expect(table).toContainText(t('common.resultState.sent'))
    await expect(table).toContainText(t('common.resultState.cancelled'))
    await checkScreen(page, `${shots}-job-cancelled`)
  })

  await test.step('the sink holds exactly the messages of the rows that say "Sent", and nothing more comes', async () => {
    await settledCount(request, x.addresses, 6000)
    const reached = await recipientsReached(request, x.addresses)
    expect(reached.size, 'the recipients the sink holds a message for, against the rows that say Sent').toBe(sent.length)
    expect(sent.every((r) => reached.has(r.email.toLowerCase())), 'every row that says Sent has a message at the sink').toBe(true)
    expect(cancelled.some((r) => reached.has(r.email.toLowerCase())), 'a row that says Cancelled has a message at the sink').toBe(false)
    expect(await countFor(request, x.addresses), 'one message for each row that says Sent').toBe(sent.length)
  })

  return { sent, cancelled }
}

test.describe('pause, resume and cancel', () => {
  test('module 2, by keyboard: pause, resume, cancel; the sink holds what was sent and the unsent credits come back', async ({ page, request }) => {
    test.setTimeout(360_000)
    const session = await ready(request, 'a')
    const addresses = recipients(`ctl2-${RUN}`, 10)
    let jobId: string | undefined
    try {
      await sinkMode(request, { data_delay_ms: 2500, delay_recipients: addresses })
      await signIn(page, request, 'a')
      const before = await readCredits(page, 'm2')

      const started = await startJob(request, session, await createSendDraft(request, session, { module: 'm2', addresses }))
      jobId = started.id
      const unitCost = started.preview.unit_cost
      expect(started.confirm.reserved, 'the confirm holds one unit for each message').toBe(addresses.length * unitCost)
      const held = await getBalances(request, session)
      expect(held.modules.m2.reserved - before.reserved, 'the reserve is held while the job runs').toBe(started.confirm.reserved)

      await openJobFromTable(page, jobId)
      const { sent } = await exercisePauseResumeCancel(page, request, session, { id: jobId, addresses, module: 'm2', keyboard: true })

      await test.step('the dashboard says the unsent credits came back: only what was sent is charged', async () => {
        await expect.poll(async () => (await getBalances(request, session)).modules.m2.reserved, { message: 'the reserve is released when the job ends', timeout: 60_000 }).toBe(before.reserved)
        await openScreen(page, 'dashboard')
        const after = await readCredits(page, 'm2')
        expect(after.reserved, 'nothing is set aside any more').toBe(before.reserved)
        expect(before.available - after.available, 'the balance fell by the credits of the messages that were sent, and by nothing else').toBe(sent.length * unitCost)
        await checkScreen(page, 'ctl-m2-dashboard-after')
      })
    } finally {
      await sinkMode(request, {})
      if (jobId !== undefined) await endJob(request, session, jobId)
    }
  })

  test('module 3, by mouse: pause, resume, cancel; the sink holds only what was sent', async ({ page, request }) => {
    test.setTimeout(360_000)
    const session = await ready(request, 'a')
    const addresses = recipients(`ctl3-${RUN}`, 8)
    const tag = `ctl3-${RUN}`
    let jobId: string | undefined
    try {
      await addReadyAccount(request, session, tag)
      await sinkMode(request, { data_delay_ms: 2000, delay_recipients: addresses })
      await signIn(page, request, 'a')

      const started = await startJob(request, session, await createSendDraft(request, session, { module: 'm3', addresses }))
      jobId = started.id
      expect(started.confirm.reserved, 'a module 3 job holds no credits').toBe(0)

      await openJobFromTable(page, jobId)
      await exercisePauseResumeCancel(page, request, session, { id: jobId, addresses, module: 'm3', keyboard: false })
    } finally {
      await sinkMode(request, {})
      if (jobId !== undefined) await endJob(request, session, jobId)
      await removeAccountsStartingWith(request, session, `${tag}@`)
    }
  })

  test('a verification job has no Pause (module 1 cannot pause a running job), says so, and can be cancelled', async ({ page, request }) => {
    test.setTimeout(360_000)
    const session = await ready(request, 'a')
    const addresses = Array.from({ length: VERIFY_ROWS }, (_, i) => `ctl1-${RUN}-${String(i + 1).padStart(6, '0')}@free.example.test`)
    let jobId: string | undefined
    try {
      await signIn(page, request, 'a') // signed in first: the job is started and looked at within a second or two, while module 1 is still at work
      const before = await getBalances(request, session)
      const started = await startJob(request, session, await createVerifyDraft(request, session, addresses))
      jobId = started.id
      await openJobFromTable(page, jobId)
      const c = controls(page)

      await test.step('no Pause on the job page, whatever it is doing; it says why when it is running', async () => {
        await expect(c.pause).toHaveCount(0)
        await expect(c.cancel).toBeVisible()
        await expectJobStatus(page, jobId as string, 'running', 90_000)
        await expect(c.pause).toHaveCount(0)
        await expect(page.getByText(t('jobs.controls.noPause.m1'), { exact: true })).toBeVisible()
        await checkScreen(page, 'ctl-m1-job-running')
      })

      await test.step('Cancel asks first, then the job ends "Cancelled"', async () => {
        await c.cancel.click()
        const dialog = page.getByRole('alertdialog', { name: t('jobs.cancel.title') })
        await expect(dialog).toBeVisible()
        await expect(dialog.getByText(t('jobs.cancel.started.m1'), { exact: true })).toBeVisible()
        await shot(page, 'ctl-m1-cancel-question')
        await dialog.getByRole('button', { name: t('jobs.cancel.confirm'), exact: true }).click()
        await expectJobStatus(page, jobId as string, 'cancelled', 180_000)
        await expect(c.group).toHaveCount(0)
        await checkScreen(page, 'ctl-m1-job-cancelled')
      })

      await test.step('the rows module 1 had not checked are "Cancelled" and free, and their credits are back', async () => {
        const results = await allResults(request, session, jobId as string)
        const cancelled = results.filter((r) => r.state === 'cancelled')
        expect(cancelled.length, `some rows were still waiting when the cancel came (${results.length} results of ${addresses.length}): the list may be too short for module 1 (E2E_HOLD_VERIFY_ROWS)`).toBeGreaterThan(0)
        expect(cancelled.every((r) => r.decision === 'free'), 'a row that was not checked is free').toBe(true)
        await expect.poll(async () => (await getBalances(request, session)).modules.m1.reserved, { message: 'the reserve is released when the job ends', timeout: 60_000 }).toBe(before.modules.m1.reserved)
        const after = await getBalances(request, session)
        expect(before.modules.m1.available - after.modules.m1.available, 'only the rows that were checked are charged: less than the whole reserve').toBeLessThan(started.confirm.reserved)
      })
    } finally {
      if (jobId !== undefined) await endJob(request, session, jobId)
    }
  })
})
