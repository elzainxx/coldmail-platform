// A11, "the sandboxed preview proves a <script> in a template does not run": a message whose HTML holds a script, an inline event handler, a javascript: link and a form is written on the new send job
// screen, previewed with its samples, saved as a draft and read back; in every place the user's HTML is shown:
//
//   - the frame has EXACTLY `sandbox=""` (the attribute present and empty: no scripts, no same origin, no forms, no popups), a `srcdoc` and no `src`
//   - the frame shows the user's visible text, and its own style (the inline style of the message is the message's own)
//   - nothing ran: `window.__pwned` is undefined in the app's page, the document title is the one it had, no dialog, no popup, no navigation of the page, no request the form of the template could make
//   - clicking the link and the button INSIDE the frame does nothing
//   - the app's own document never gets a `<script>` (or an element of the template) of the message: its scripts are the app's bundle and nothing else, before and after
// The proof is the app's own pair of walls (the empty sandbox, and a content security policy that forbids inline script, which a document in `srcdoc` inherits): this spec shows neither wall is missing, and the
// negative checks wait a quiet moment (nothing is awaited that could not happen), because "nothing happened" has no event to wait for.
import { expect, test, type Locator, type Page } from '@playwright/test'
import type { Job } from '../src/api/types.ts'
import { endJob } from './support/cross-api.ts'
import { checkScreen, fillSignIn, openScreen, runId, signIn, walkSendJob } from './support/cross-ui.ts'
import { csvFile, recipients, sendCsv } from './support/data.ts'
import { shot } from './support/shots.ts'
import { bearer, ready } from './support/users.ts'

const RUN = runId()

const VISIBLE = 'Quarterly update for'
const HTML =
  `<p style="color:#7a1f1f;font-weight:700">${VISIBLE} {{first_name|friend}}</p>` +
  `<script>window.top.__pwned=1;document.title='pwned'</script>` +
  `<img src=x onerror="window.top.__pwned=2">` +
  `<a href="javascript:window.top.__pwned=3">x</a>` +
  `<form action="/v1/jobs"><button>go</button></form>`
const SUBJECT = `Sandbox check ${RUN} {{first_name|friend}}`

const FRAME = '[data-testid=preview-frame]'
const AXE_WITHOUT_FRAMES = { exclude: [FRAME] }

/** What the app's own document is, and whether anything of the message got into it or ran. */
async function appDocument(page: Page) {
  return await page.evaluate(() => ({
    scripts: Array.from(document.scripts).map((s) => s.getAttribute('src') ?? '(inline)'),
    scriptHoldsMessage: Array.from(document.scripts).some((s) => (s.textContent ?? '').includes('__pwned')),
    elementsOfTheMessage: document.querySelectorAll('img[onerror], a[href^="javascript:"], form[action="/v1/jobs"], object, embed').length,
    title: document.title,
    pwned: Reflect.get(window, '__pwned') !== undefined,
  }))
}

/** What happened on the page since the observer was made. */
function observe(page: Page) {
  const seen = { dialogs: 0, popups: 0, navigations: 0, creates: 0 }
  page.on('dialog', (d) => {
    seen.dialogs++
    void d.dismiss()
  })
  page.on('popup', () => {
    seen.popups++
  })
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) seen.navigations++
  })
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === '/v1/jobs') seen.creates++
  })
  return {
    seen,
    reset: () => {
      seen.dialogs = seen.popups = seen.navigations = seen.creates = 0
    },
  }
}

/**
 * The user's visible text in a frame, in its own colour, and the inert link and button inside it clicked: nothing may follow. `required`: the link and the button must be there (the authoring frame shows the
 * markup as it was typed); a sample is what the platform rendered, so its link and button are clicked when they are there.
 */
async function readAndClickInside(frame: Locator, what: string, text: RegExp, required: boolean): Promise<void> {
  const inner = frame.contentFrame()
  await expect(inner.locator('body')).toContainText(text)
  await expect(inner.locator('p').first(), `${what}: the message's own style applies inside its frame`).toHaveCSS('color', 'rgb(122, 31, 31)')
  const link = inner.getByRole('link', { name: 'x', exact: true })
  const button = inner.getByRole('button', { name: 'go', exact: true })
  if (required || (await link.count()) > 0) await link.click()
  if (required || (await button.count()) > 0) await button.click()
}

/** `creates`: how many drafts the APP itself made since the observer was reset (the one it makes when the preview is asked for): any request beyond it came from somewhere else. */
async function nothingRan(page: Page, observer: ReturnType<typeof observe>, baseline: Awaited<ReturnType<typeof appDocument>>, what: string, creates: number): Promise<void> {
  await page.waitForTimeout(1500) // a negative has no event to wait for: a script that could run would have run within this
  const now = await appDocument(page)
  expect(now.pwned, `${what}: window.__pwned is undefined in the app's page`).toBe(false)
  expect(now.title, `${what}: the document title is the one it had`).toBe(baseline.title)
  expect(now.scripts, `${what}: the app's scripts are the same as before (no script was added, none is inline)`).toEqual(baseline.scripts)
  expect(now.scriptHoldsMessage, `${what}: no script of the document holds the message`).toBe(false)
  expect(now.elementsOfTheMessage, `${what}: no element of the message is in the app's own document`).toBe(0)
  expect(observer.seen.dialogs, `${what}: no dialog opened`).toBe(0)
  expect(observer.seen.popups, `${what}: no popup opened`).toBe(0)
  expect(observer.seen.navigations, `${what}: the page was not navigated`).toBe(0)
  expect(observer.seen.creates, `${what}: no request was sent by the form of the message (only the draft the app itself made)`).toBe(creates)
}

test.describe('the sandboxed preview', () => {
  test('a script, an inline handler, a javascript: link and a form in a template do not run: the authoring frame and every sample frame are sandbox=""', async ({ page, request }) => {
    test.setTimeout(240_000)
    const session = await ready(request, 'a')
    const addresses = recipients(`sbx-${RUN}`, 3)
    const names: Array<[string, string]> = [['Zed', 'a'], ['Yan', 'b'], ['Xi', 'c']]
    let draftId: string | undefined
    page.on('response', async (r) => {
      if (new URL(r.url()).pathname === '/v1/jobs' && r.request().method() === 'POST' && r.status() === 201) draftId = ((await r.json()) as { job: Job }).job.id
    })

    try {
      await signIn(page, request, 'a')
      const observer = observe(page)
      const baseline = await appDocument(page)

      await walkSendJob(page, {
        sender: 'm2',
        file: csvFile('sandbox.csv', sendCsv(addresses, names)),
        subject: SUBJECT,
        html: HTML,
        at: {
          sender: async () => {
            observer.reset()
            baseline.title = await page.title() // the title of the screen the message is written on (the same on all four steps)
          },
          message: async () => {
            const frame = page.getByTestId('preview-frame')
            await test.step('the authoring frame: sandbox="" exactly, the visible text, the style', async () => {
              await expect(frame).toHaveCount(1)
              await expect(frame).toHaveAttribute('srcdoc', /pwned/)
              expect(await frame.getAttribute('sandbox'), 'the authoring frame\'s sandbox is present and empty').toBe('')
              expect(await frame.getAttribute('src'), 'the authoring frame has no src').toBeNull()
              await readAndClickInside(frame, 'the authoring frame', new RegExp(`${VISIBLE} \\{\\{first_name\\|friend\\}\\}`), true)
            })
            await test.step('nothing ran in the app\'s page', async () => {
              await nothingRan(page, observer, baseline, 'the authoring screen', 0)
              await checkScreen(page, 'sbx-authoring', { axe: AXE_WITHOUT_FRAMES })
            })
          },
          preview: async () => {
            await test.step('every sample frame: sandbox="" exactly, the message rendered for a recipient, nothing ran', async () => {
              const samples = page.getByTestId(/^sample-\d+$/)
              const count = await samples.count()
              expect(count, 'the preview shows up to three samples').toBeGreaterThan(0)
              for (let i = 0; i < count; i++) {
                const frame = samples.nth(i).getByTestId('preview-frame')
                await expect(frame).toHaveCount(1)
                expect(await frame.getAttribute('sandbox'), `sample ${i + 1}: sandbox is present and empty`).toBe('')
                expect(await frame.getAttribute('src'), `sample ${i + 1}: no src`).toBeNull()
                await readAndClickInside(frame, `sample ${i + 1}`, new RegExp(`${VISIBLE} (Zed|Yan|Xi)`), false)
              }
              await nothingRan(page, observer, baseline, 'the preview', 1)
              await checkScreen(page, 'sbx-preview', { axe: AXE_WITHOUT_FRAMES })
            })
          },
        },
      })
      expect(draftId, 'the draft the preview was made from').toBeDefined()
    } finally {
      if (draftId !== undefined) await endJob(request, session, draftId)
    }
  })

  test('the same template, saved as a send job and opened again, shows in sandboxed frames and never makes a script in the app\'s document', async ({ page, request }) => {
    test.setTimeout(240_000)
    const session = await ready(request, 'a')
    const addresses = recipients(`sbx2-${RUN}`, 3)
    let draftId: string | undefined
    page.on('response', async (r) => {
      if (new URL(r.url()).pathname === '/v1/jobs' && r.request().method() === 'POST' && r.status() === 201) draftId = ((await r.json()) as { job: Job }).job.id
    })

    try {
      // save it: the preview step makes the draft (the platform freezes the message when it is saved)
      await signIn(page, request, 'a')
      await walkSendJob(page, {
        sender: 'm2',
        file: csvFile('sandbox2.csv', sendCsv(addresses, [['Zed', 'a'], ['Yan', 'b'], ['Xi', 'c']])),
        subject: SUBJECT,
        html: HTML,
      })
      if (draftId === undefined) throw new Error('the draft was not made')
      const saved = await request.get(`/v1/jobs/${draftId}`, { headers: bearer(session) })
      const savedJob = ((await saved.json()) as { job: Job }).job
      expect(savedJob.content?.html.includes('<script>'), 'the platform saved the template as it was written').toBe(true)

      // open the saved job again, in a page that has not seen the message: the scripts of the document are counted before the job is shown
      await page.goto(`/new/send?job=${draftId}`)
      const baseline = await appDocument(page)
      const observer = observe(page)
      await fillSignIn(page, 'a')
      await expect(page.getByTestId('sample-1')).toBeVisible({ timeout: 60_000 })
      observer.reset()
      const titleBefore = await page.title()
      expect(titleBefore.includes('pwned'), 'the title of the page before anything is clicked').toBe(false)
      const samples = page.getByTestId(/^sample-\d+$/)
      const count = await samples.count()
      expect(count).toBeGreaterThan(0)
      for (let i = 0; i < count; i++) {
        const frame = samples.nth(i).getByTestId('preview-frame')
        expect(await frame.getAttribute('sandbox'), `saved job, sample ${i + 1}: sandbox is present and empty`).toBe('')
        await readAndClickInside(frame, `saved job, sample ${i + 1}`, new RegExp(`${VISIBLE} (Zed|Yan|Xi)`), false)
      }
      await nothingRan(page, observer, { ...baseline, title: titleBefore }, 'the saved job opened again', 0)
      await checkScreen(page, 'sbx-saved-job', { axe: AXE_WITHOUT_FRAMES })

      // its page in the jobs list shows its subject as text and nothing of its HTML
      await openScreen(page, 'jobs')
      await shot(page, 'sbx-jobs-with-saved-draft')
      const after = await appDocument(page)
      expect(after.pwned, 'window.__pwned is undefined on the jobs page').toBe(false)
      expect(after.scripts, 'the jobs page has the app\'s scripts only').toEqual(baseline.scripts)
      expect(after.elementsOfTheMessage).toBe(0)
    } finally {
      if (draftId !== undefined) await endJob(request, session, draftId)
    }
  })
})
