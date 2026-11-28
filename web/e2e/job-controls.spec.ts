// A10, "pause, resume and cancel": user a, the job page, and the sink slowed down so that a job lasts long enough to be paused (the sink holds each message to the spec's own recipients for a few seconds
// and the sender works one lane at a time).
//
//   1  module 2, by KEYBOARD: Pause (status "Paused", nothing new reaches the sink for as long as it is paused), Resume ("Running" again, messages flow), Cancel through its confirmation (the dialog traps
//      the keyboard and Escape gives the focus back); the job ends "Cancelled", the sink holds exactly the messages of the results that say "Sent", the unsent results are free, and the balance of the dashboard
//      says the unsent credits came back
//   2  module 3, by mouse: the same three moves; a module 3 job holds no credits, so its proof is the sink and the results
//   3  module 1: a verification job shows NO Pause (module 1 cannot pause a running job) and says so, and it can be cancelled. THE SIZE OF ITS LIST IS ADAPTIVE (a ladder, the way the platform's own live test
//      tests/harness/jobs_live_m1cancel_test.go does it): how fast module 1 settles a list on a runner is not known, and a list it finishes before the cancel reaches it cannot show a cancel in the middle
//      of a job. Such a try is a good, finished job (what is true of it is asserted: it ended, no control is left, the balance is exactly what was charged) and the next, longer list is tried; the test fails,
//      saying so, only when the longest list also finished first. Only the try in which rows were cancelled gets the cancellation assertions and the pictures.
//
// Everything the sink is told is for this spec's recipients (`recipients`, unique to the run) and is put back to the default at the end: the sink is shared by the whole run and is never reset.
import { expect, test, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import { jobStatusText } from '../src/copy/words.ts'
import {
  allResults,
  createSendDraft,
  createVerifyDraft,
  endJob,
  FINAL_STATUSES,
  getBalances,
  getJob,
  startJob,
  waitForStatus,
  type SendModule,
} from './support/cross-api.ts'
import { countFor, recipientsReached, settledCount } from './support/cross-sink.ts'
import {
  checkScreen,
  escapeRe,
  expectJobStatus,
  holdShot,
  jobStatus,
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

/**
 * The lists the verification test tries, shortest first. 100,000 is the platform's row limit; a try is also capped by what the user can pay for (the reserve is held for every row: the stage grants 100,000
 * credits at module 1 and a row reserves 1 or 2), so the longest try on a stage that grants no more is what the balance covers after the first.
 */
const VERIFY_LADDER = [20_000, 100_000]

/** The price of one charged answer of module 1 (internal/charge, the seeded prices: m1_own and m1_cache are 1; the vendor's is the reserve rate, read from the preview). */
const OWN_PRICE = 1

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
    const addresses = recipients(`ctl2-${RUN}`, 16) // 16 x 2.5 s of sending: a slow runner (axe, the keyboard) is still in the middle of the list when the cancel lands
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
    const addresses = recipients(`ctl3-${RUN}`, 12) // 12 x (2 s and up to 1 s between two sends): the same reason
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
    test.setTimeout(720_000)
    const session = await ready(request, 'a')
    const c = controls(page)
    const made: string[] = []
    const tried: string[] = []
    let conclusive = false
    let unitCost = 2 // the reserve of a row until a preview says otherwise (the vendor's rate, the dearer)
    let lastSize = 0

    try {
      await signIn(page, request, 'a') // signed in first: the job is started and looked at within a second or two, while module 1 is still at work

      for (const planned of VERIFY_LADDER) {
        const before = await getBalances(request, session)
        const size = Math.min(planned, Math.floor(before.modules.m1.available / unitCost))
        if (size <= lastSize) {
          tried.push(`${planned}: not tried, the balance (${before.modules.m1.available} credits at ${unitCost} a row) covers ${size} rows, no more than the last try`)
          break
        }
        lastSize = size

        await test.step(`a list of ${size} addresses`, async () => {
          const addresses = Array.from({ length: size }, (_, i) => `ctl1-${RUN}-${size}-${String(i + 1).padStart(6, '0')}@free.example.test`)
          const run = await startJob(request, session, await createVerifyDraft(request, session, addresses))
          made.push(run.id)
          unitCost = run.preview.unit_cost
          await openJobFromTable(page, run.id)

          // ---- what the page says at once: no Pause in any state; the sentence that says why while the job runs. Nothing slow before the cancel: module 1 is at work ----
          const running = jobStatusText('running')
          let status = (await jobStatus(page, run.id).innerText()).trim()
          if (status === jobStatusText('confirmed')) {
            await expect(jobStatus(page, run.id)).toHaveText(running, { timeout: 8_000 }).catch(() => undefined) // the page asks again every 3 seconds; it may also have gone past "running" meanwhile
            status = (await jobStatus(page, run.id).innerText()).trim()
          }
          await expect(c.pause).toHaveCount(0)
          const sawRunning = status === running
          if (sawRunning) await expect(page.getByText(t('jobs.controls.noPause.m1'), { exact: true })).toBeVisible()
          const keepPage = await holdShot(page)

          // ---- Cancel asks first, then the job ends; if the job ended under the page's feet there is nothing to cancel and the try is a finished job ----
          let keepQuestion: ((name: string) => string) | undefined
          try {
            if ((await c.cancel.count()) > 0) {
              await c.cancel.click({ timeout: 5_000 })
              const dialog = page.getByRole('alertdialog', { name: t('jobs.cancel.title') })
              await expect(dialog).toBeVisible({ timeout: 5_000 })
              if (sawRunning) await expect(dialog.getByText(t('jobs.cancel.started.m1'), { exact: true })).toBeVisible({ timeout: 2_000 })
              keepQuestion = await holdShot(page)
              await dialog.getByRole('button', { name: t('jobs.cancel.confirm'), exact: true }).click({ timeout: 5_000 })
            }
          } catch (e) {
            if (!FINAL_STATUSES.includes((await getJob(request, session, run.id)).status)) throw e
            keepQuestion = undefined
          }

          const ended = await waitForStatus(request, session, run.id, ['cancelled', 'succeeded'], 300_000)
          await expectJobStatus(page, run.id, ended.status, 120_000)

          // ---- true of every try: no control is left, one result for every row, the books are exactly what was charged and the reserve is back ----
          await expect(c.group).toHaveCount(0)
          await expect(c.pause).toHaveCount(0)
          const results = await allResults(request, session, run.id)
          expect(results.length, 'one result for every row of the list').toBe(size)
          expect(new Set(results.map((r) => r.ordinal)).size, 'no row has two results').toBe(size)
          const cancelled = results.filter((r) => r.state === 'cancelled')
          expect(cancelled.some((r) => r.decision !== 'free' || r.unit !== null), 'a row that is cancelled is also charged').toBe(false)
          const charged = results.filter((r) => r.decision === 'charge').reduce((sum, r) => sum + (r.unit === 'm1_vendor' ? run.preview.unit_cost : OWN_PRICE), 0)
          await expect.poll(async () => (await getBalances(request, session)).modules.m1.reserved, { message: 'the reserve is released when the job ends', timeout: 120_000 }).toBe(before.modules.m1.reserved)
          const after = await getBalances(request, session)
          expect(before.modules.m1.available - after.modules.m1.available, 'the balance fell by what the charged rows cost, and by nothing else').toBe(charged)
          expect(run.confirm.reserved - charged >= cancelled.length * run.preview.unit_cost, 'the credits that came back cover every cancelled row').toBe(true)

          if (cancelled.length === 0 || !sawRunning || ended.status !== 'cancelled') {
            tried.push(`${size}: ended ${ended.status} with ${cancelled.length} row(s) cancelled, seen running: ${sawRunning}`)
            return // module 1 had settled the whole list before the cancel reached it: a good, finished job; the next list is longer
          }

          // ---- the try in which the cancel landed in the middle of the job: it is the one that is kept ----
          conclusive = true
          expect(cancelled.length, 'some rows were cancelled and some were checked').toBeLessThan(size)
          await expect(page.getByText(t('jobs.now.cancelled'), { exact: true })).toBeVisible()
          keepPage('ctl-m1-job-running')
          keepQuestion?.('ctl-m1-cancel-question')
          await checkScreen(page, 'ctl-m1-job-cancelled')
        })
        if (conclusive) break
      }

      expect(
        conclusive,
        `module 1 settled every row before the cancel reached it, at every size tried (${tried.join('; ')}): a cancel in the middle of a verification job was never shown; add a longer list to VERIFY_LADDER, or more credits at module 1 for the user`,
      ).toBe(true)
    } finally {
      for (const id of made) await endJob(request, session, id)
    }
  })
})
