// A11, "the DOM holds no internal text": every main screen, in the three kinds of state a person meets, is read for the words of the deny-list (web/src/copy/internal-words.json, generated from the API's own
// codes, tables and statuses): what a person can read or hear on the page (its text, its title, its labels) holds none of them.
//
//   1  EMPTY    user s (no credits, no entitlement, no sending account, no module 3 account): the dashboard, the jobs, the new verify and send screens, the accounts, the suppressions and the settings
//   2  POPULATED user a with finished jobs of both kinds, an account, a suppression and a draft: the same screens with data in them (a verification and a send job's page, the previews with their samples)
//   3  REFUSED  the screens after an action the platform or the app REALLY refused: a wrong password, a page that does not exist, a job that is not found, an account the user may not add, an empty batch of
//               accounts, a malformed suppression, an import with no address column, a list that is not a CSV, two passwords that differ, a confirm of a job that was taken back to a draft
//   4  CONTROL  the check reads every place a person reads or hears a word (text, label, placeholder, title, the page title) and passes a page that says none
//
// Where a refusal has a wording, the spec also asserts the wording is on the page: an error state that shows nothing would pass the copy check by being empty.
import { expect, test, type Page } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import { createSendDraft, endJob, finishedVerifyJob, startJob, waitForStatus, addSuppression, removeSuppression } from './support/cross-api.ts'
import { expectNoInternalText, someInternalWord } from './support/cross-copy.ts'
import { checkCopy, escapeRe, h1, openJobFromTable, openScreen, runId, signIn, walkSendJob, type Screen } from './support/cross-ui.ts'
import { csvFile, fixturePath, recipients, sendCsv } from './support/data.ts'
import { accountAt, addReadyAccount, fillAddForm, openAddForm, removeAccountsStartingWith } from './support/m3-accounts.ts'
import { shot } from './support/shots.ts'
import { bearer, ready, user } from './support/users.ts'

const RUN = runId()

/** A screen, read for internal words with a picture of it. */
async function read(page: Page, name: string): Promise<void> {
  await checkCopy(page)
  await shot(page, name)
}

async function visit(page: Page, screen: Screen, name: string): Promise<void> {
  await openScreen(page, screen)
  await read(page, name)
}

const alertWith = (page: Page, text: string) => page.getByRole('alert').filter({ hasText: text })

test.describe('no internal word reaches a screen', () => {
  test('EMPTY: the screens of a user with nothing', async ({ page, request }) => {
    test.setTimeout(240_000)
    await signIn(page, request, 's')

    await test.step('the dashboard, the jobs, the new verify job, the accounts, the suppressions, the settings', async () => {
      await read(page, 'copy-empty-dashboard')
      await expect(page.getByText(t('dashboard.m3.disabled'), { exact: true })).toBeVisible()
      await visit(page, 'jobs', 'copy-empty-jobs')
      await visit(page, 'newVerify', 'copy-empty-verify')
      await visit(page, 'accounts', 'copy-empty-accounts')
      await expect(page.getByText(t('accounts.entitlement.none'), { exact: true })).toBeVisible()
      await visit(page, 'suppressions', 'copy-empty-suppressions')
      await visit(page, 'settings', 'copy-empty-settings')
    })

    await test.step('a new send job: Your own SMTP says it is not switched on, with a way to the accounts', async () => {
      await walkSendJob(page, {
        sender: 'm3',
        file: csvFile('empty.csv', sendCsv(['copy-empty@example.test'])),
        subject: 'Hello',
        html: '<p>Hello</p>',
        until: 'sender',
        at: {
          sender: async () => {
            await expect(page.getByText(t('newjob.entitlement.off'), { exact: true })).toBeVisible()
            await expect(page.getByRole('link', { name: t('newjob.sender.accountsLink'), exact: true })).toBeVisible()
            await read(page, 'copy-empty-send-sender-own')
          },
        },
      })
    })

    await test.step('a new send job with no credits: the message step, and the preview that says how short the user is', async () => {
      await openScreen(page, 'newVerify') // away and back: a new send job starts from its first step
      let draftId: string | undefined
      page.on('response', async (r) => {
        if (new URL(r.url()).pathname === '/v1/jobs' && r.request().method() === 'POST' && r.status() === 201) draftId = ((await r.json()) as { job: { id: string } }).job.id
      })
      try {
        await walkSendJob(page, {
          sender: 'm2',
          file: csvFile('short.csv', sendCsv(recipients(`copy-short-${RUN}`, 2), [['Ann', 'a'], ['Bo', 'b']])),
          subject: 'Hello {{first_name|there}}',
          html: '<p>Hello {{first_name|there}}</p>',
          at: {
            sender: () => read(page, 'copy-empty-send-sender-managed'),
            list: () => read(page, 'copy-empty-send-list'),
            message: () => read(page, 'copy-empty-send-message'),
            preview: async () => {
              await expect(page.getByText(t('newjob.confirm.blockedCredits'), { exact: true })).toBeVisible()
              await expect(page.getByRole('button', { name: t('newjob.confirm.submitNow'), exact: true })).toBeDisabled()
              await read(page, 'copy-empty-send-preview-short')
            },
          },
        })
        await page.getByRole('button', { name: t('newjob.send.editMessage'), exact: true }).click() // lets the draft go
      } finally {
        if (draftId !== undefined) await endJob(request, await ready(request, 's'), draftId)
      }
    })
  })

  test('POPULATED: the screens of a user with finished jobs, an account, a suppression and a preview', async ({ page, request }) => {
    test.setTimeout(420_000)
    const session = await ready(request, 'a')
    const tag = `copy-${RUN}`
    const suppressed = `copy-supp-${RUN}@example.test`
    let draftId: string | undefined
    try {
      const verified = await finishedVerifyJob(request, session, [`copy-${RUN}-1@free.example.test`, `copy-${RUN}-2@d1.example.test`, `copy-${RUN}-3@d2.example.test`])
      const send = await startJob(request, session, await createSendDraft(request, session, { module: 'm2', addresses: recipients(`copy-send-${RUN}`, 2) }))
      await waitForStatus(request, session, send.id, ['succeeded'], 120_000)
      await addSuppression(request, session, suppressed)
      await addReadyAccount(request, session, tag)

      await signIn(page, request, 'a')
      await test.step('the dashboard, the jobs, and the page of each kind of job', async () => {
        await read(page, 'copy-full-dashboard')
        await visit(page, 'jobs', 'copy-full-jobs')
        await openJobFromTable(page, verified.id)
        await read(page, 'copy-full-job-verify')
        await openJobFromTable(page, send.id)
        await expect(page.getByTestId(/^results-row-/)).toHaveCount(2)
        await read(page, 'copy-full-job-send')
      })

      await test.step('the accounts and the suppressions with their rows', async () => {
        await visit(page, 'accounts', 'copy-full-accounts')
        await visit(page, 'suppressions', 'copy-full-suppressions')
        await expect(page.getByText(suppressed, { exact: true })).toBeVisible()
      })

      await test.step('a verify job previewed: the counts of what is left out and why', async () => {
        await openScreen(page, 'newVerify')
        await page.getByLabel(t('newjob.csv.file'), { exact: true }).setInputFiles(fixturePath('verify-list.csv'))
        await expect(page.getByLabel(t('newjob.columns.address'))).toBeVisible()
        await read(page, 'copy-full-verify-file')
        await page.getByRole('button', { name: t('newjob.verify.check'), exact: true }).click()
        await expect(page.getByRole('heading', { name: t('newjob.preview.title'), exact: true })).toBeVisible()
        await expect(page.getByText(t('common.malformed.no_at'))).toBeVisible()
        await read(page, 'copy-full-verify-preview')
        await page.getByRole('button', { name: t('newjob.csv.change'), exact: true }).click()
      })

      await test.step('a send job: the four steps, with their samples', async () => {
        page.on('response', async (r) => {
          if (new URL(r.url()).pathname === '/v1/jobs' && r.request().method() === 'POST' && r.status() === 201) draftId = ((await r.json()) as { job: { id: string } }).job.id
        })
        await walkSendJob(page, {
          sender: 'm3',
          file: csvFile('full.csv', sendCsv(recipients(`copy-full-${RUN}`, 3), [['Ann', 'a'], ['Bo', 'b'], ['Cy', 'c']])),
          subject: 'Hello {{first_name|there}}',
          html: '<p>Hello {{first_name|there}}, {{topic|about nothing}}</p>',
          at: {
            sender: () => read(page, 'copy-full-send-sender'),
            list: () => read(page, 'copy-full-send-list'),
            message: () => read(page, 'copy-full-send-message'),
            preview: () => read(page, 'copy-full-send-preview'),
          },
        })
        await page.getByRole('button', { name: t('newjob.send.editMessage'), exact: true }).click()
      })

      await visit(page, 'settings', 'copy-full-settings')
    } finally {
      if (draftId !== undefined) await endJob(request, session, draftId)
      await removeSuppression(request, session, suppressed).catch(() => undefined)
      await removeAccountsStartingWith(request, session, `${tag}@`)
    }
  })

  test('REFUSED: the screens after an action that was really refused', async ({ page, request }) => {
    test.setTimeout(420_000)
    const sa = await ready(request, 'a')
    await ready(request, 's')
    let draftId: string | undefined

    await test.step('a page that does not exist, and a wrong password', async () => {
      await page.goto('/no-such-page')
      await expect(h1(page, t('common.notFound.title'))).toBeVisible()
      await read(page, 'copy-refused-page-not-found')
      await page.goto('/sign-in')
      await page.getByLabel(t('auth.field.email'), { exact: true }).fill(user('s').email)
      await page.getByLabel(t('auth.field.password'), { exact: true }).fill(`not-the-password-${RUN}`)
      await page.getByRole('button', { name: t('auth.signIn.submit'), exact: true }).click()
      await expect(alertWith(page, t('auth.error.invalid_credentials'))).toBeVisible()
      await read(page, 'copy-refused-wrong-password')
    })

    await test.step('signed in as s: a job that is not found', async () => {
      await signIn(page, request, 's', '/jobs/0b000000-0000-4000-8000-000000000e2e')
      await expect(alertWith(page, t('error.not_found'))).toBeVisible()
      await read(page, 'copy-refused-job-not-found')
    })

    await test.step('an account s may not add (module 3 is not switched on for s), and an empty batch of accounts', async () => {
      await openScreen(page, 'accounts')
      await openAddForm(page)
      await fillAddForm(page, accountAt(`copy-s-${RUN}`))
      await page.getByRole('button', { name: t('accounts.add.submit'), exact: true }).click()
      await expect(alertWith(page, t('error.no_entitlement'))).toBeVisible({ timeout: 30_000 })
      await read(page, 'copy-refused-account-no-entitlement')
      await page.getByRole('button', { name: t('common.cancel'), exact: true }).click()
      await page.getByRole('button', { name: t('accounts.bulk.open'), exact: true }).click()
      await page.getByRole('button', { name: t('accounts.bulk.submit'), exact: true }).click()
      await expect(alertWith(page, t('accounts.bulk.nothing'))).toBeVisible()
      await read(page, 'copy-refused-bulk-nothing')
    })

    await test.step('suppressions: no address, a malformed address, a file with no address column', async () => {
      await openScreen(page, 'suppressions')
      await page.getByRole('button', { name: t('suppressions.add.submit'), exact: true }).click()
      await expect(page.getByText(t('suppressions.add.empty'), { exact: true })).toBeVisible()
      await page.getByLabel(t('suppressions.add.label'), { exact: true }).fill('not-an-address')
      await page.getByRole('button', { name: t('suppressions.add.submit'), exact: true }).click()
      await expect(page.getByText(t('common.malformed.no_at'), { exact: true })).toBeVisible()
      await read(page, 'copy-refused-suppression-malformed')
      await page.getByLabel(t('suppressions.import.label'), { exact: true }).setInputFiles(csvFile('no-address.csv', 'name\nAnn\n'))
      await page.getByRole('button', { name: t('suppressions.import.submit'), exact: true }).click()
      await expect(alertWith(page, t('suppressions.import.badFile'))).toBeVisible()
      await read(page, 'copy-refused-suppression-import')
    })

    await test.step('a list that cannot be used, and two passwords that differ', async () => {
      await openScreen(page, 'newVerify')
      await page.getByLabel(t('newjob.csv.file'), { exact: true }).setInputFiles(csvFile('ragged.csv', 'email,name\nann@example.test\nbo@example.test,Bo,extra\n'))
      await expect(page.getByText(t('newjob.problem.title'), { exact: true })).toBeVisible()
      await read(page, 'copy-refused-file-unusable')
      await openScreen(page, 'settings')
      await page.getByLabel(t('auth.password.new'), { exact: true }).fill('a-long-enough-phrase-1')
      await page.getByLabel(t('auth.password.confirm'), { exact: true }).fill('a-long-enough-phrase-2')
      await page.getByRole('button', { name: t('settings.password.submit'), exact: true }).click()
      await expect(page.getByText(t('auth.password.mismatch'), { exact: true })).toBeVisible()
      await read(page, 'copy-refused-password-mismatch')
    })

    await test.step('as a: a confirm of a job that was taken back to a draft', async () => {
      page.on('response', async (r) => {
        if (new URL(r.url()).pathname === '/v1/jobs' && r.request().method() === 'POST' && r.status() === 201) draftId = ((await r.json()) as { job: { id: string } }).job.id
      })
      try {
        await signIn(page, request, 'a')
        await walkSendJob(page, {
          sender: 'm2',
          file: csvFile('taken-back.csv', sendCsv(recipients(`copy-back-${RUN}`, 2), [['Ann', 'a'], ['Bo', 'b']])),
          subject: 'Hello {{first_name|there}}',
          html: '<p>Hello {{first_name|there}}</p>',
        })
        if (draftId === undefined) throw new Error('the draft was not made')
        const reopened = await request.post(`/v1/jobs/${draftId}/reopen`, { headers: bearer(sa) })
        expect(reopened.status(), 'the previewed job went back to a draft').toBe(200)
        await page.getByRole('button', { name: t('newjob.confirm.submitNow'), exact: true }).click()
        // the platform says the job is not previewed any more (a stale preview, a refused move and a draft have each their own line: any of the three is a wording, none is a code)
        await expect(page.getByText(new RegExp([t('error.not_previewed'), t('error.not_allowed'), t('newjob.stale.title')].map(escapeRe).join('|')))).toBeVisible()
        await read(page, 'copy-refused-confirm-not-previewed')
      } finally {
        if (draftId !== undefined) await endJob(request, sa, draftId)
      }
    })
  })

  test('CONTROL: an internal word is found in every place a person reads or hears it (the text, a label, a placeholder, a title, the page title), and a page without one passes', async ({ page }) => {
    const word = someInternalWord()
    const doc = (title: string, body: string): string => `<!doctype html><html lang="en"><head><title>${title}</title></head><body><main><h1>Control</h1>${body}</main></body></html>`
    const found = new RegExp(`'${escapeRe(word)}'`)

    await page.setContent(doc('Control', '<p>Nothing internal is said here.</p>'))
    await expectNoInternalText(page)
    for (const [where, html, title] of [
      ['the text', `<p>It says ${word} in a sentence.</p>`, 'Control'],
      ['an aria-label', `<button aria-label="Do ${word}">Go</button>`, 'Control'],
      ['a placeholder', `<label>Name <input placeholder="${word}"></label>`, 'Control'],
      ['a title', `<a href="#x" title="${word}">link</a>`, 'Control'],
      ['the page title', '<p>Plain.</p>', `About ${word}`],
    ] as const) {
      await page.setContent(doc(title, html))
      await expect(expectNoInternalText(page), `the check reads ${where}`).rejects.toThrow(found)
    }
  })
})

