// A10, "module 3 account add with the From-gate result, test, send": user a, the SMTP accounts page, the stack's sink as the mail server, the fixture DNS of the From-gate.
//
//   1  a From address that is NOT aligned: the first press shows the warning in the user's words, the submit is blocked until the box is ticked (no request leaves), then the account is added and its row
//      says its state in words; the Test button says the connection and the From-gate state, and a warning has to be confirmed
//   2  an aligned From address: added with no warning; the same account again says "duplicate"; adding many: a batch with a line the platform cannot read is refused whole, a batch that is read shows what
//      became of each line
//   3  a send job through "Your own SMTP" from a CSV: the preview shows the entitlement and no credits, the job finishes, the sink holds one message per recipient with the unsubscribe headers; then the
//      account is removed through its confirmation
//
// At no point does a screen hold the SMTP password or a sentence of the module (the page's DOM is read for the password, and for the words of DNS and of the From-gate). The SMTP password of each
// account is made here and never printed. Recipients are unique to this run (`recipients`), so the sink can be read for them alone.
import { expect, test } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import { expectNoA11yViolations } from './support/axe.ts'
import { csvFile, recipients, sendCsv } from './support/data.ts'
import { checkScreen, confirmSend, escapeRe, expectJobStatus, h1, openScreen, pageHolds, runId, signIn, walkSendJob } from './support/cross-ui.ts'
import {
  ALIGNED_DOMAIN,
  RAW_MODULE_TEXT,
  UNALIGNED_DOMAIN,
  accountAt,
  accountRow,
  accountSecret,
  addForm,
  addReadyAccount,
  fillAddForm,
  listAccounts,
  openAddForm,
  removeAccountsStartingWith,
  submitAdd,
} from './support/m3-accounts.ts'
import { shot } from './support/shots.ts'
import { headerOf, messagesTo, rawMessage, waitForMessages } from './support/sink.ts'
import { ready } from './support/users.ts'
import { m3SinkHost } from './support/env.ts'

const RUN = runId()

const LOGIN_SENTENCE = new RegExp(
  `${escapeRe(t('accounts.verification.verified'))}|${escapeRe(t('accounts.verification.unverified'))}|${escapeRe(t('accounts.verification.failed', { why: 'X' }).split('X')[0] ?? '')}`,
)
const STATUS_WORDS = new RegExp([t('accounts.status.active'), t('accounts.status.paused'), t('accounts.status.quarantined'), t('accounts.status.disabled')].map(escapeRe).join('|'))

test.describe('module 3 accounts through the screens', () => {
  test('a From address that is not aligned: the warning, the acknowledgement that unblocks the submit, the row in words, and the Test button', async ({ page, request }) => {
    test.setTimeout(240_000)
    const session = await ready(request, 'a')
    const tag = `warn-${RUN}`
    const account = accountAt(tag, UNALIGNED_DOMAIN)
    const posts: string[] = []
    page.on('request', (r) => {
      if (r.method() === 'POST' && new URL(r.url()).pathname === '/v1/m3/accounts') posts.push(r.url())
    })

    try {
      await signIn(page, request, 'a', '/accounts')

      await test.step('the accounts page', async () => {
        await expect(h1(page, t('accounts.title'))).toBeVisible()
        await checkScreen(page, 'm3a-accounts-before')
      })

      await test.step('the add form, filled in', async () => {
        await openAddForm(page)
        await fillAddForm(page, account)
        await checkScreen(page, 'm3a-add-form')
      })

      await test.step('the first press shows the From-gate warning in words, and holds the account back', async () => {
        await page.getByRole('button', { name: t('accounts.add.submit'), exact: true }).click()
        await expect(page.getByText(t('accounts.ack.title'), { exact: true })).toBeVisible()
        await expect(page.getByText(t('accounts.ack.body'), { exact: true })).toBeVisible()
        await expect(page.getByRole('checkbox', { name: t('accounts.ack.checkbox'), exact: true })).not.toBeChecked()
        await expect(page.getByRole('button', { name: t('accounts.add.submitAnyway'), exact: true })).toBeVisible()
        expect(posts.length, 'the form sent the account once and the platform asked for the warning to be confirmed').toBe(1)
        expect(await pageHolds(page, account.smtp_secret), 'the page holds the SMTP password in its markup').toBe(false)
        await checkScreen(page, 'm3a-from-gate-warning')
      })

      await test.step('the submit is blocked until the acknowledgement is ticked: no request leaves', async () => {
        await page.getByRole('button', { name: t('accounts.add.submitAnyway'), exact: true }).click()
        await expect(page.getByRole('alert').filter({ hasText: t('accounts.ack.needed') })).toBeVisible()
        expect(posts.length, 'a press with the box unticked sent nothing').toBe(1)
      })

      let created = ''
      await test.step('with the box ticked the account is added, and says what it came to', async () => {
        await page.getByRole('checkbox', { name: t('accounts.ack.checkbox'), exact: true }).check()
        const added = await submitAdd(page, 'accounts.add.submitAnyway')
        created = added.id
        expect(posts.length).toBe(2)
        const outcome = page.getByRole('region', { name: t('accounts.added.title', { address: account.from_address }) })
        await expect(outcome).toBeVisible()
        await expect(outcome.getByText(t('accounts.fromGate.warned'), { exact: true })).toBeVisible()
        await expect(outcome.getByText(t('accounts.fromGate.warnedWhy'), { exact: true })).toBeVisible()
        await expect(outcome).toContainText(LOGIN_SENTENCE)
        expect(await pageHolds(page, account.smtp_secret, true), 'the page holds the SMTP password after the add').toBe(false)
      })

      await test.step('its row shows the state in words, the server, the login and the From-gate check', async () => {
        const row = accountRow(page, created)
        await expect(row).toBeVisible()
        await expect(row).toContainText(account.from_address)
        await expect(row).toContainText(`${account.smtp_host}:${account.smtp_port}`)
        await expect(row).toContainText(t('accounts.row.login', { username: account.smtp_username }))
        await expect(row).toContainText(STATUS_WORDS)
        await expect(row).toContainText(t('accounts.fromGate.warned'))
        await expect(row).toContainText(t('accounts.fromGate.warnedWhy'))
        await expect(row).toContainText(LOGIN_SENTENCE)
        const text = await page.getByRole('main').innerText()
        expect(RAW_MODULE_TEXT.test(text), 'the page says a word of DNS or of the From-gate that is not the app\'s own wording').toBe(false)
        await checkScreen(page, 'm3a-account-warned-row')
      })

      await test.step('the Test button: the connection result and the From-gate state in words, and the warning is confirmed from the dialog', async () => {
        await page.getByRole('button', { name: t('accounts.test.buttonFor', { address: account.from_address }), exact: true }).click()
        const dialog = page.getByRole('dialog', { name: t('accounts.test.title', { address: account.from_address }) })
        await expect(dialog).toBeVisible()
        await expect(dialog.getByRole('heading', { name: t('accounts.test.connection'), exact: true })).toBeVisible({ timeout: 60_000 })
        await expect(dialog.getByText(t('accounts.test.works'), { exact: true })).toBeVisible()
        await expect(dialog.getByText(t('accounts.stage.done'), { exact: true })).toBeVisible()
        await expect(dialog.getByRole('heading', { name: t('accounts.test.fromCheck'), exact: true })).toBeVisible()
        await expect(dialog.getByText(t('accounts.fromGate.warned'), { exact: true })).toBeVisible()
        await expect(dialog.getByText(t('accounts.fromGate.warnedWhy'), { exact: true })).toBeVisible()

        const again = dialog.getByRole('button', { name: t('accounts.test.again'), exact: true })
        await expect(again).toBeDisabled()
        await dialog.getByRole('checkbox', { name: t('accounts.ack.checkboxUse'), exact: true }).check()
        await expect(again).toBeEnabled()
        await checkScreen(page, 'm3a-test-dialog')
        await again.click()
        await expect(dialog.getByText(t('accounts.test.confirmed'), { exact: true })).toBeVisible({ timeout: 60_000 })
        expect(await pageHolds(page, account.smtp_secret, true), 'the page holds the SMTP password after the test').toBe(false)
        await dialog.getByRole('button', { name: t('accounts.test.done'), exact: true }).click()
        await expect(dialog).toBeHidden()
      })
    } finally {
      await removeAccountsStartingWith(request, session, `${tag}@`)
    }
  })

  test('an aligned From address is added with no warning, a repeat says duplicate, and adding many shows what became of each line', async ({ page, request }) => {
    test.setTimeout(240_000)
    const session = await ready(request, 'a')
    const tag = `ok-${RUN}`
    const first = accountAt(tag)
    const bulkTag = `bulk-${RUN}`

    try {
      await signIn(page, request, 'a', '/accounts')

      let createdId = ''
      await test.step('an aligned account: added without a warning', async () => {
        await openAddForm(page)
        await fillAddForm(page, first)
        const added = await submitAdd(page, 'accounts.add.submit')
        createdId = added.id
        await expect(page.getByText(t('accounts.ack.title'), { exact: true })).toHaveCount(0)
        const outcome = page.getByRole('region', { name: t('accounts.added.title', { address: first.from_address }) })
        await expect(outcome).toBeVisible()
        await expect(outcome.getByText(t('accounts.fromGate.aligned'), { exact: true })).toBeVisible()
        await expect(outcome.getByText(t('accounts.fromGate.alignedWhy'), { exact: true })).toBeVisible()
        await expect(accountRow(page, createdId)).toContainText(t('accounts.fromGate.aligned'))
        expect(await pageHolds(page, first.smtp_secret, true), 'the page holds the SMTP password after the add').toBe(false)
        await checkScreen(page, 'm3a-account-aligned')
        await page.getByRole('button', { name: t('accounts.added.dismiss'), exact: true }).click()
      })

      await test.step('the Test button on it says the connection works and the address is aligned', async () => {
        await page.getByRole('button', { name: t('accounts.test.buttonFor', { address: first.from_address }), exact: true }).click()
        const dialog = page.getByRole('dialog', { name: t('accounts.test.title', { address: first.from_address }) })
        await expect(dialog.getByText(t('accounts.fromGate.aligned'), { exact: true })).toBeVisible({ timeout: 60_000 })
        await expect(dialog.getByText(t('accounts.stage.done'), { exact: true })).toBeVisible()
        await expect(dialog.getByText(t('accounts.ack.checkboxUse'))).toHaveCount(0)
        await dialog.getByRole('button', { name: t('accounts.test.done'), exact: true }).click()
        await expect(dialog).toBeHidden()
      })

      await test.step('adding the same account again says it is a duplicate, and adds nothing', async () => {
        await openAddForm(page)
        await fillAddForm(page, { ...first, from_address: `dup-${tag}@${ALIGNED_DOMAIN}`, smtp_secret: accountSecret() })
        await page.getByRole('button', { name: t('accounts.add.submit'), exact: true }).click()
        await expect(page.getByRole('alert').filter({ hasText: t('error.account_duplicate') })).toBeVisible({ timeout: 60_000 })
        await expect(addForm(page).password).toHaveValue('')
        await checkScreen(page, 'm3a-account-duplicate')
        const dups = (await listAccounts(request, session)).filter((a) => a.from_address.startsWith(`dup-${tag}@`))
        expect(dups.length, 'an account was added although it was a duplicate').toBe(0)
        await page.getByRole('button', { name: t('common.cancel'), exact: true }).click()
      })

      const host = m3SinkHost()
      const line = (username: string, from: string, limit = ''): string => [host, '1025', username, accountSecret(), from, 'Bulk', limit].join(',')

      await test.step('many accounts: a batch with a line the platform cannot read is refused whole, in words', async () => {
        await page.getByRole('button', { name: t('accounts.bulk.open'), exact: true }).click()
        await expect(page.getByRole('heading', { name: t('accounts.bulk.title'), exact: true })).toBeVisible()
        const lines = page.getByLabel(t('accounts.bulk.lines'), { exact: true })
        await lines.fill([t('accounts.bulk.header'), line(`e2e-${bulkTag}`, `${bulkTag}@${ALIGNED_DOMAIN}`), line(`e2e-${bulkTag}-bad`, `${bulkTag}-bad@${ALIGNED_DOMAIN}`, 'abc')].join('\n'))
        await page.getByRole('button', { name: t('accounts.bulk.submit'), exact: true }).click()
        await expect(page.getByRole('alert').filter({ hasText: /Nothing was added/ })).toBeVisible({ timeout: 60_000 })
        const none = (await listAccounts(request, session)).filter((a) => a.from_address.startsWith(`${bulkTag}`))
        expect(none.length, 'the good line of a refused batch was added').toBe(0)
      })

      await test.step('many accounts: a batch that is read shows a row for each line', async () => {
        const lines = page.getByLabel(t('accounts.bulk.lines'), { exact: true })
        await lines.fill([t('accounts.bulk.header'), line(`e2e-${bulkTag}`, `${bulkTag}@${ALIGNED_DOMAIN}`), line(first.smtp_username, `dup-${bulkTag}@${ALIGNED_DOMAIN}`)].join('\n'))
        await page.getByRole('button', { name: t('accounts.bulk.submit'), exact: true }).click()
        await expect(page.getByText(t('accounts.bulk.summary', { added: 1, failed: 1 }), { exact: true })).toBeVisible({ timeout: 120_000 })
        const table = page.getByRole('table', { name: t('accounts.bulk.caption') })
        await expect(table.getByRole('row')).toHaveCount(3) // the header and a row for each of the two lines
        await expect(table.getByRole('row').nth(1)).toContainText(/2\s*Added/)
        await expect(table.getByRole('row').nth(2)).toContainText(/3\s*Duplicate/)
        await expect(table.getByRole('row').nth(2)).toContainText(t('error.account_duplicate'))
        await expect(page.getByText(t('accounts.bulk.summaryCheck'), { exact: true })).toBeVisible()
        await checkScreen(page, 'm3a-bulk-result')
      })
    } finally {
      await removeAccountsStartingWith(request, session, `${tag}@`)
      await removeAccountsStartingWith(request, session, `${bulkTag}@`)
    }
  })

  test('a send job through "Your own SMTP": the preview names the entitlement and no credits, every recipient gets one message with the unsubscribe headers, and the account is removed through its confirmation', async ({ page, request }) => {
    test.setTimeout(300_000)
    const session = await ready(request, 'a')
    const tag = `send-${RUN}`
    const addresses = recipients(`m3s-${RUN}`, 4)
    const subject = `Own SMTP ${RUN}`
    let jobId = ''

    try {
      const account = await addReadyAccount(request, session, tag)
      await signIn(page, request, 'a', '/accounts')

      await test.step('the ready account is in the list: login works, From address aligned', async () => {
        const row = accountRow(page, account.id)
        await expect(row).toContainText(t('accounts.verification.verified'))
        await expect(row).toContainText(t('accounts.fromGate.aligned'))
        await checkScreen(page, 'm3a-account-ready')
      })

      await test.step('the four steps of a send through Your own SMTP, from a CSV', async () => {
        await walkSendJob(page, {
          sender: 'm3',
          file: csvFile('m3-recipients.csv', sendCsv(addresses, [['Ann', 'one'], ['Bo', 'two'], ['Cy', 'three'], ['Di', 'four']])),
          subject: `${subject} {{first_name|there}}`,
          html: '<p>Hello {{first_name|there}}, this is about {{topic|nothing}}.</p>',
          at: {
            sender: async () => {
              await expect(page.getByText(/Sending through your own SMTP is switched on/)).toBeVisible()
              await checkScreen(page, 'm3a-send-sender')
            },
            list: async () => {
              await checkScreen(page, 'm3a-send-list')
            },
            message: async () => {
              await checkScreen(page, 'm3a-send-message', { axe: { exclude: ['[data-testid=preview-frame]'] } })
            },
            preview: async () => {
              // the entitlement is shown and no credits are held
              await expect(page.getByText(t('newjob.preview.noCredits'), { exact: true })).toBeVisible()
              await expect(page.getByText(/Sending through your own SMTP is switched on/)).toBeVisible()
              await expect(page.getByText(t('newjob.preview.credits'), { exact: true })).toHaveCount(0)
              await expect(page.getByText(/^Credits per message/)).toHaveCount(0)
              await expect(page.getByText(t('newjob.preview.unitsSend'), { exact: true })).toBeVisible()
              await checkScreen(page, 'm3a-send-preview', { axe: { exclude: ['[data-testid=preview-frame]'] } })
            },
          },
        })
      })

      await test.step('confirm: the job is confirmed, nothing is held, and it finishes', async () => {
        jobId = await confirmSend(page)
        await expect(page.getByText(t('newjob.done.pending', { count: String(addresses.length) }), { exact: true })).toBeVisible()
        await expect(page.getByText(/^Credits held for this job:/)).toHaveCount(0)
        await page.getByRole('link', { name: t('newjob.done.open'), exact: true }).click()
        await expect(h1(page, t('jobs.detail.title'))).toBeVisible()
        await expectJobStatus(page, jobId, 'succeeded', 180_000)
        await expect(page.getByTestId(/^results-row-/)).toHaveCount(addresses.length)
        for (const row of await page.getByTestId(/^results-row-/).all()) await expect(row).toContainText(t('common.resultState.sent'))
        await checkScreen(page, 'm3a-job-finished', { axe: { exclude: ['[data-testid=preview-frame]'] } })
      })

      await test.step('the sink holds one message per recipient, each with the unsubscribe headers', async () => {
        for (const address of addresses) {
          const got = await waitForMessages(request, address, 1, 90_000)
          const first = got[0]
          if (first === undefined) throw new Error('a recipient has no message at the sink')
          expect(got.length, 'a recipient has more than one message at the sink').toBe(1)
          expect((await messagesTo(request, address)).length).toBe(1)
          const raw = await rawMessage(request, first.seq)
          expect(/<https:\/\/[^>\s]+>/.test(headerOf(raw, 'List-Unsubscribe') ?? ''), 'List-Unsubscribe holds an https address').toBe(true)
          expect(headerOf(raw, 'List-Unsubscribe-Post') !== undefined, 'the message has List-Unsubscribe-Post').toBe(true)
          expect(first.subject.includes(subject), 'the subject carries the job\'s subject').toBe(true)
        }
      })

      await test.step('removing the account asks first, says what removing means, and then the row is gone', async () => {
        await openScreen(page, 'accounts')
        const removeButton = page.getByRole('button', { name: t('accounts.remove.buttonFor', { address: account.from_address }), exact: true })
        await removeButton.click()
        const dialog = page.getByRole('alertdialog', { name: t('accounts.remove.title', { address: account.from_address }) })
        await expect(dialog).toBeVisible()
        await expect(dialog.getByText(t('accounts.remove.body'), { exact: true })).toBeVisible()
        await checkScreen(page, 'm3a-remove-dialog')
        await dialog.getByRole('button', { name: t('common.cancel'), exact: true }).click() // keep it first: the row stays
        await expect(dialog).toBeHidden()
        await expect(accountRow(page, account.id)).toBeVisible()
        await removeButton.click()
        await dialog.getByRole('button', { name: t('accounts.remove.confirm'), exact: true }).click()
        await expect(page.getByText(t('accounts.remove.done', { address: account.from_address }), { exact: true })).toBeVisible()
        await expect(accountRow(page, account.id)).toHaveCount(0)
        expect((await listAccounts(request, session)).some((a) => a.id === account.id), 'the removed account is still listed').toBe(false)
        await shot(page, 'm3a-account-removed')
        await expectNoA11yViolations(page)
      })
    } finally {
      await removeAccountsStartingWith(request, session, `${tag}@`)
    }
  })
})
