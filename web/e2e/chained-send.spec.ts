// Sending through module 2, from the screens to the sink and back (chunk A10 "a chained send through module 2: messages at the sink with an unsubscribe link, click it, the next job excludes the address"),
// with user a. Every recipient is made for the run (`recipients('c...' + runId())`), so the sink is read for them alone and an unsubscribe cannot touch any other spec.
//
//   1  the message screen (no job is confirmed, nothing is sent): the palette offers the verified list's own column (`first_name`); the help opens; the HTML box says where a mistake is (line AND column) and
//      Continue stays closed until it is fixed; the live preview is a sandboxed frame; and the preview of a chained send counts the ONE row that lacks the field the message needs (no fallback) as missing,
//      names the field, and counts the others as sendable, with samples that carry their own first names
//   2  the whole chain: a send chained to a finished verification, confirmed; each recipient's message at the sink holds ITS subject and body (its first name, its address), exactly one List-Unsubscribe and
//      List-Unsubscribe-Post, the same https link in the body; the row without a first name and the invalid address got nothing; the recipient's own pages (a GET changes nothing, the button does; a one-click
//      POST of another recipient's link does too); the Suppressions screen lists both; a NEW send over the same verification counts them as suppressed and the sink gets nothing more for them while the others
//      still get one
//   3  a send from a file: the palette and the toolbar write the template; the row that lacks a value the template needs (no fallback) is counted, named, left out and NOT sent; the row whose value is blank
//      but has a fallback is sent with the fallback
//
// WHY EACH TEST MAKES ITS OWN VERIFICATION (and not the one of verify-job.spec.ts, nor a fixture file): the order of the files is not a contract, and a send needs recipients that no other spec uses (the
// sink is read for them alone and an unsubscribe must not suppress an address another spec mails), which a fixed file cannot give: `fixtures/verify-list.csv` is the verify spec's and stays as it is. The
// list is built on the spot (`chainedList`) with a `first_name` column, EMPTY for exactly one risky row and filled for the others, and the starting point is made through the API (`finishedVerification`:
// draft, preview, confirm, wait) because the verify screens are the other spec's subject; everything about the send is driven through the screens.
//
// WHAT A CHAINED SEND CARRIES (Docs/jobs-api.md "A chained send keeps the merge data of the list it was verified from", Docs/templates.md): each row that is or can become a recipient (valid, risky and unknown
// verdicts) keeps the `fields` of its row of the verification list, so `{{first_name}}` is that recipient's own; an invalid row is never sent and carries none; a field without a fallback leaves out the rows
// whose cell is empty ("an empty cell lacks it too"); the job lists the NAMES of the merge columns (`merge_fields`), which the authoring palette offers.
import { expect, test } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import { csvFile, recipients } from './support/data.ts'
import { appUrl } from './support/env.ts'
import { expectNoA11yViolations } from './support/axe.ts'
import { expectNoInternalWords } from './support/copy.ts'
import { balances, finishedVerification, suppressionsMatching } from './support/flows-api.ts'
import { escapeRegExp, fmt, must, runId } from './support/flows-data.ts'
import { readSinkFor, sinkCount } from './support/flows-mail.ts'
import {
  chooseListFile,
  chooseSender,
  chooseUnknown,
  chooseVerification,
  continueTo,
  sampleHeading,
  stepHeading,
  toPreview,
  UNSUBSCRIBE_PAGE,
  writeMessage,
} from './support/flows-send.ts'
import {
  confirmNow,
  goTo,
  h1,
  h2,
  openAs,
  openConfirmedJob,
  readResultRows,
  settled,
  shotFrame,
  stat,
  statNote,
  statNumber,
  unitCost,
  waitForJobStatus,
} from './support/flows-ui.ts'
import { shot } from './support/shots.ts'

/**
 * The verified list of the chained tests, made for one run: five risky recipients (`...@free.example.test`: module 1's fixture DNS gives them risky, and a risky row is a recipient) and one invalid address
 * (`...@d1.example.test`: no mail server). Each has a first name but ONE risky row (the third), whose cell is empty; the invalid row has a name too, which no message may ever carry.
 */
function chainedList(tag: string) {
  const risky = recipients(tag, 5, 'free.example.test').map((a) => must(a))
  const names = ['Pat', 'Quinn', '', 'Sam', 'Uma']
  const invalid = `${tag}-bad@d1.example.test`
  const nameOf = (address: string): string => names[risky.indexOf(address)] ?? ''
  return {
    risky,
    invalid,
    /** the risky row whose first_name is empty: the message below needs it (no fallback), so this row is never sent */
    blank: must(risky[2]),
    /** the rows that are sent: risky, with a first name */
    sendable: risky.filter((_, i) => names[i] !== ''),
    nameOf,
    rows: [
      { email: must(risky[0]), first_name: 'Pat' },
      { email: invalid, first_name: 'Ivy' },
      { email: must(risky[1]), first_name: 'Quinn' },
      { email: must(risky[2]), first_name: '' },
      { email: must(risky[3]), first_name: 'Sam' },
      { email: must(risky[4]), first_name: 'Uma' },
    ],
  }
}

/**
 * The subject has a fallback for the first name (it would show "friend"), the body has NONE: a row without a first name is never sent, so "friend" must appear in no message. The body also holds the
 * recipient's own address and the unsubscribe link.
 */
const SUBJECT = '{Hello|Hi|Hey} {{first_name|friend}}: news for {{email}}'
const HTML = ['<p>{Hi|Hello|Hey} {{first_name}},</p>', '<p>This message is for {{email}}.</p>', '<p><a href="{{unsubscribe_url}}">Unsubscribe</a></p>'].join('\n')

const subjectOf = (name: string, address: string): RegExp => new RegExp(`^(Hello|Hi|Hey) ${escapeRegExp(name)}: news for ${escapeRegExp(address)}$`)

test('the message screen: the palette offers the list\'s own column, a mistake is shown at its line and column and blocks Continue, the preview counts the row without a first name', async ({ page, request }) => {
  test.setTimeout(300_000)
  const run = runId()
  const tag = `ca${run}`
  const list = chainedList(tag)
  const session = await openAs(page, request, 'a')
  const verification = await finishedVerification(request, session, list.rows, `e2e-${run}-a`)
  const m2Before = (await balances(request, session)).m2

  await chooseSender(page, 'm2')
  await chooseVerification(page, verification)
  await chooseUnknown(page, 'exclude')
  await continueTo(page, 3)

  const subject = page.getByLabel(t('newjob.authoring.subject'), { exact: true })
  const html = page.getByLabel(t('newjob.authoring.html'), { exact: true })
  const next = page.getByRole('button', { name: t('newjob.message.next'), exact: true })

  await test.step('nothing is written yet: the screen says what it needs, and offers the verified list\'s column and the two fields every message has', async () => {
    await expect(next).toBeDisabled()
    await expect(page.getByText(t('newjob.message.needSubjectAndBody'))).toBeVisible()
    const toolbar = page.getByRole('group', { name: t('newjob.authoring.toolbar') })
    await expect(toolbar.getByRole('button')).toHaveText(
      ['paragraph', 'heading', 'bold', 'italic', 'link', 'lineBreak', 'list', 'unsubscribe'].map((k) => t(`newjob.authoring.tag.${k}` as 'newjob.authoring.tag.bold')),
    )
    // the column of the verified list (the job's merge_fields), not typed by hand
    const fromList = page.getByRole('list', { name: t('newjob.authoring.columnsFields') })
    await expect(fromList.getByRole('button')).toHaveCount(1)
    await expect(fromList.getByRole('button', { name: t('newjob.authoring.insertField', { name: 'first_name' }), exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: t('newjob.authoring.insertField', { name: 'email' }), exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: t('newjob.authoring.insertField', { name: 'unsubscribe_url' }), exact: true })).toBeVisible()
  })

  const line2 = '<p>Your address is {{email}} and a stray } is here.</p>'
  const column = line2.indexOf('stray }') + 'stray '.length + 1 // the brace itself, counting characters from 1
  await test.step('a stray brace is shown where it is, with its line and its column, and Continue stays closed', async () => {
    await subject.fill('{Hello|Hi} {{first_name|friend}}, news for {{email}}')
    await html.fill(['<p>{Hi|Hello} {{first_name}},</p>', line2, '<p><a href="{{unsubscribe_url}}">Unsubscribe</a></p>'].join('\n'))
    const where = t('newjob.authoring.errorAt', { line: 2, column, message: '' }).trim()
    await expect(page.getByRole('alert').filter({ hasText: where })).toBeVisible()
    await expect(html).toHaveAttribute('aria-invalid', 'true')
    await expect(html).toHaveAccessibleDescription(new RegExp(escapeRegExp(where)))
    await expect(subject).not.toHaveAttribute('aria-invalid', 'true')
    await expect(next).toBeDisabled()
    await expect(page.getByText(t('newjob.message.fixMistakes'))).toBeVisible()
    await settled(page, 'send-step-message-error')
  })

  await test.step('the mistake fixed: the message reads correctly, names the fields it uses, and has no warning about the list (it has the column)', async () => {
    await html.fill(['<p>{Hi|Hello} {{first_name}},</p>', '<p>Your address is {{email}}.</p>', '<p><a href="{{unsubscribe_url}}">Unsubscribe</a></p>'].join('\n'))
    const live = page.getByRole('status').filter({ hasText: t('newjob.authoring.ok') })
    await expect(live).toBeVisible()
    await expect(live).toContainText(`first_name (${t('newjob.authoring.noFallback')})`)
    await expect(live).not.toContainText(t('newjob.authoring.unmetNoColumns'))
    await expect(live).not.toContainText(t('newjob.authoring.unmetColumn'))
    await expect(html).not.toHaveAttribute('aria-invalid', 'true')
    await expect(next).toBeEnabled()
  })

  await test.step('the live preview of the HTML is a sandboxed frame', async () => {
    const frame = page.getByTestId('preview-frame')
    await expect(frame).toHaveCount(1)
    await expect(frame).toHaveAttribute('sandbox', '')
    await expect(frame).toHaveAttribute('title', t('newjob.authoring.previewFrame'))
  })

  await test.step('the help for merge fields, choices and escapes opens', async () => {
    await page.getByText(t('newjob.help.summary'), { exact: true }).click()
    await expect(page.getByRole('heading', { level: 4, name: t('newjob.help.fallbackTitle'), exact: true })).toBeVisible()
    await expect(page.getByRole('cell', { name: 'Hi {{name|there}}', exact: true })).toBeVisible()
    await expect(page.getByRole('cell', { name: '{Hello|Hi|Hey}', exact: true })).toBeVisible()
    await settled(page, 'send-step-message')
  })

  await test.step('the preview counts the one row without a first name as missing, names the field, and counts the others as sendable, with their own names in the samples', async () => {
    await toPreview(page)
    expect(await statNumber(page, t('newjob.preview.rows')), 'rows: one for each result of the verification').toBe(6)
    await expect(statNote(page, t('newjob.preview.fromVerification'))).toContainText(`${t('common.resultState.risky')}: 5`)
    await expect(statNote(page, t('newjob.preview.fromVerification'))).toContainText(`${t('common.resultState.invalid')}: 1`)
    expect(await statNumber(page, t('newjob.preview.excluded')), 'the invalid address is left out by the verdicts').toBe(1)
    expect(await statNumber(page, t('newjob.preview.selected')), 'chosen from the verification: the risky ones').toBe(5)
    expect(await statNumber(page, t('newjob.preview.missing')), 'only the row with no first name lacks the field').toBe(1)
    await expect(statNote(page, t('newjob.preview.missing'))).toContainText(`${t('newjob.preview.missingNames')} first_name`)
    expect(await statNumber(page, t('newjob.preview.unitsSend')), 'messages that would be sent').toBe(list.sendable.length)
    const unit = await unitCost(page, 'send')
    expect(await statNumber(page, t('newjob.preview.credits')), 'credits held = messages x the price').toBe(list.sendable.length * unit)
    expect(await statNumber(page, t('newjob.preview.available')), 'the credits the preview says the user has are the account\'s').toBe(m2Before.available)
    await expect(page.getByText(t('newjob.preview.nothing'))).toHaveCount(0)
    await expect(page.getByRole('button', { name: t('newjob.confirm.submitNow'), exact: true })).toBeEnabled()

    await expect(page.getByTestId(/^sample-/)).toHaveCount(3) // the first, the middle and the last of the four rows that would be sent
    for (const n of [1, 2, 3]) {
      const sample = page.getByTestId(`sample-${n}`)
      const subjectText = await sample.getByRole('definition').first().innerText()
      const who = /news for (\S+)$/.exec(subjectText)?.[1] ?? ''
      expect(list.sendable.includes(who), `sample ${n}: its subject names a recipient that will be sent to (never the row without a first name)`).toBe(true)
      expect(/^(Hello|Hi) /.test(subjectText) && subjectText.includes(`${list.nameOf(who)}, news for`), `sample ${n}: its subject carries that recipient's own first name`).toBe(true)
      expect(subjectText.includes('friend'), `sample ${n}: the fallback is not used`).toBe(false)
      const srcdoc = (await sample.getByTestId('preview-frame').getAttribute('srcdoc')) ?? ''
      expect(srcdoc.includes(`${list.nameOf(who)},</p>`), `sample ${n}: its HTML greets that recipient by first name`).toBe(true)
    }
    await settled(page, 'send-step-preview-gap')
  })

  await test.step('going back to the message lets the draft go and keeps what was written', async () => {
    await page.getByRole('button', { name: t('newjob.send.editMessage'), exact: true }).click()
    await expect(h2(page, stepHeading(3))).toBeVisible()
    await expect(html).toHaveValue(/\{\{first_name\}\}/)
    await expect(subject).toHaveValue(/\{\{first_name\|friend\}\}/)
  })
})

test('a send chained to a verification: each message at the sink is that recipient\'s own, with its first name and its unsubscribe link; the link works; the next job leaves the address out', async ({ page, request, context }) => {
  test.setTimeout(480_000)
  const run = runId()
  const tag = `cc${run}`
  const list = chainedList(tag)
  const { risky, invalid, blank, sendable } = list
  const session = await openAs(page, request, 'a')
  const verification = await finishedVerification(request, session, list.rows, `e2e-${run}-c`)
  const m2Before = (await balances(request, session)).m2

  await test.step('step 1: managed sending, and what the user has', async () => {
    await goTo(page, 'newSend')
    await expect(h2(page, stepHeading(1))).toBeVisible()
    await expect(page.getByRole('button', { name: t('newjob.step.next'), exact: true })).toBeDisabled()
    await expect(page.getByText(t('newjob.sender.needChoice'))).toBeVisible()
    await page.getByRole('group', { name: t('newjob.sender.legend') }).getByRole('radio', { name: t('newjob.sender.managed'), exact: true }).check()
    await expect(page.getByText(t('newjob.sender.managedCredits', { credits: fmt(m2Before.available) }))).toBeVisible()
    await settled(page, 'send-step-sender')
    await page.getByRole('button', { name: t('newjob.step.next'), exact: true }).click()
    await expect(h2(page, stepHeading(2))).toBeVisible()
  })

  await test.step('step 2: from the finished verification; the valid and risky are in, the invalid never; unknown needs an explicit answer', async () => {
    await chooseVerification(page, verification)
    const verdicts = page.getByRole('group', { name: t('newjob.verdicts.legend') })
    for (const [name, on] of [[t('newjob.verdicts.valid'), true], [t('newjob.verdicts.risky'), true], [t('newjob.verdicts.invalid'), false]] as const) {
      const box = verdicts.getByRole('checkbox', { name, exact: true })
      await expect(box).toBeDisabled() // fixed by the platform's rule: the user does not choose these
      if (on) await expect(box).toBeChecked()
      else await expect(box).not.toBeChecked()
    }
    await expect(page.getByRole('button', { name: t('newjob.step.next'), exact: true })).toBeDisabled()
    await expect(page.getByText(t('newjob.list.needChoice'))).toBeVisible()
    await chooseUnknown(page, 'exclude')
    await expect(page.getByRole('button', { name: t('newjob.step.next'), exact: true })).toBeEnabled()
    await settled(page, 'send-step-list')
    await continueTo(page, 3)
  })

  await test.step('step 3: the message, with the list\'s column offered by the palette', async () => {
    await expect(page.getByRole('list', { name: t('newjob.authoring.columnsFields') }).getByRole('button', { name: t('newjob.authoring.insertField', { name: 'first_name' }), exact: true })).toBeVisible()
    await writeMessage(page, { subject: SUBJECT, html: HTML })
    const live = page.getByRole('status').filter({ hasText: t('newjob.authoring.ok') })
    await expect(live).toBeVisible()
    await expect(live).toContainText(`first_name (${t('newjob.authoring.noFallback')})`) // one use has no fallback: the field is required
    await expect(page.getByRole('button', { name: t('newjob.message.next'), exact: true })).toBeEnabled()
    await settled(page, 'send-step-message-chained')
  })

  const unit = await test.step('step 4: the preview: the row without a first name is missing, the others are sendable; the samples; the exact credits', async () => {
    await toPreview(page)
    expect(await statNumber(page, t('newjob.preview.rows')), 'rows: one for each result of the verification').toBe(6)
    await expect(statNote(page, t('newjob.preview.fromVerification'))).toContainText(`${t('common.resultState.risky')}: 5`)
    await expect(statNote(page, t('newjob.preview.fromVerification'))).toContainText(`${t('common.resultState.invalid')}: 1`)
    expect(await statNumber(page, t('newjob.preview.excluded')), 'the invalid address is left out by the verdicts').toBe(1)
    expect(await statNumber(page, t('newjob.preview.selected')), 'chosen from the verification').toBe(5)
    expect(await statNumber(page, t('newjob.preview.suppressedSend')), 'none of these addresses is on the suppression list').toBe(0)
    expect(await statNumber(page, t('newjob.preview.missing')), 'the row with no first name').toBe(1)
    await expect(statNote(page, t('newjob.preview.missing'))).toContainText(`${t('newjob.preview.missingNames')} first_name`)
    expect(await statNumber(page, t('newjob.preview.unitsSend')), 'messages that will be sent').toBe(sendable.length)
    const price = await unitCost(page, 'send')
    expect(await statNumber(page, t('newjob.preview.credits')), 'credits held = messages x the price').toBe(sendable.length * price)
    expect(await statNumber(page, t('newjob.preview.available')), 'the credits the preview says the user has are the account\'s').toBe(m2Before.available)
    await expect(page.getByText(t('newjob.preview.unsubscribe'))).toBeVisible()

    // the samples: real rows of the list with their own data, rendered exactly as they will be sent, in a frame that allows nothing
    await expect(page.getByTestId(/^sample-/)).toHaveCount(3)
    const sampled = new Set<string>()
    for (const n of [1, 2, 3]) {
      const sample = page.getByTestId(`sample-${n}`)
      await expect(sample.getByRole('heading', { level: 3 })).toHaveText(sampleHeading(n))
      const subject = await sample.getByRole('definition').first().innerText()
      const who = /news for (\S+)$/.exec(subject)?.[1] ?? ''
      expect(sendable.includes(who), `sample ${n}: its subject names a recipient that will be sent to (never the row without a first name)`).toBe(true)
      expect(subjectOf(list.nameOf(who), who).test(subject), `sample ${n}: its subject is a rendered one: a choice made, that recipient's own first name, no brace left`).toBe(true)
      sampled.add(who)
      const frame = sample.getByTestId('preview-frame')
      await expect(frame).toHaveAttribute('sandbox', '')
      if (n === 1) await shotFrame(frame, 'send-sample-frame') // the full-page pictures show a sandboxed frame empty: this one shows what is in it
      const srcdoc = (await frame.getAttribute('srcdoc')) ?? ''
      expect(srcdoc.includes(`This message is for ${who}.`) && srcdoc.includes(`${list.nameOf(who)},</p>`), `sample ${n}: its HTML is for the same recipient and greets them by first name`).toBe(true)
      expect(/https:\/\/[^"\s]+\/u\/\S+"/.test(srcdoc), `sample ${n}: its HTML carries the recipient's own unsubscribe link`).toBe(true)
      const text = await sample.getByRole('region', { name: t('newjob.samples.textOf', { n }) }).innerText()
      expect(text.includes(`This message is for ${who}.`) && text.includes(list.nameOf(who)) && text.includes('/u/'), `sample ${n}: its plain text is for the same recipient, with the link`).toBe(true)
    }
    expect(sampled.size, 'three different recipients').toBe(3)
    expect(sampled.has(blank), 'the row without a first name is never a sample').toBe(false)
    await settled(page, 'send-step-preview')
    return price
  })

  const jobId = await test.step('confirm now: the confirmation says one row was left out; the job page runs to "Finished"', async () => {
    const id = await confirmNow(page)
    await expect(page.getByText(t('newjob.done.pending', { count: sendable.length }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.missing', { count: 1 }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.reserved', { credits: fmt(sendable.length * unit) }))).toBeVisible()
    await openConfirmedJob(page)
    await waitForJobStatus(page, id, 'succeeded')
    return id
  })

  await test.step('the results: four sent, the row without a first name "Missing a value", the invalid address left out by the verdict', async () => {
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(6)
    const rows = await readResultRows(page)
    for (const address of sendable) {
      const row = must(rows.find((r) => r.address.toLowerCase() === address), 'the result of a recipient')
      expect(row.outcome, `${address}: the outcome`).toBe(t('common.resultState.sent'))
      expect(row.cost, `${address}: the cost`).toBe(t('jobs.cost.charged'))
    }
    const gap = must(rows.find((r) => r.address.toLowerCase() === blank), 'the result of the row without a first name')
    expect(gap.outcome, 'the row without a first name is not sent').toBe(t('common.resultState.missing_field'))
    expect(gap.details, 'the field is named').toContain(t('jobs.results.missingFields', { fields: 'first_name' }))
    expect(gap.cost, 'it costs nothing').toBe(t('jobs.cost.free'))
    const left = must(rows.find((r) => r.address.toLowerCase() === invalid), 'the result of the invalid address')
    expect(left.outcome, 'the invalid address is left out').toBe(t('common.resultState.excluded'))
    expect(left.cost).toBe(t('jobs.cost.free'))
    await expect(page.getByTestId(`results-row-${left.ordinal}`)).toContainText(t('jobs.results.excluded', { verdict: t('common.resultState.invalid').toLowerCase() }))
    await settled(page, 'send-job-finished')
  })

  const links: string[] = []
  await test.step('at the sink: each recipient\'s message is its own (its first name, its address), with exactly one List-Unsubscribe and List-Unsubscribe-Post and the same link in the body', async () => {
    const names = sendable.map((a) => list.nameOf(a))
    for (const address of sendable) {
      const name = list.nameOf(address)
      const [m, ...others] = await readSinkFor(request, address)
      expect(others.length, `${address}: exactly one message`).toBe(0)
      const message = must(m, 'the message of a recipient')
      expect(subjectOf(name, address).test(message.subject), `${address}: the subject is the rendered one: this recipient's first name and address`).toBe(true)
      expect(message.from.includes('@example.test'), `${address}: sent from the user's sending account`).toBe(true)
      for (const part of [message.html, message.text]) {
        expect(part.includes(`This message is for ${address}.`), `${address}: the body is for this recipient`).toBe(true)
        expect(new RegExp(`\\b${escapeRegExp(name)},`).test(part), `${address}: the body greets this recipient by first name`).toBe(true)
        expect(/[{}]/.test(part), `${address}: no brace of the template is left`).toBe(false)
        expect(risky.filter((o) => o !== address).some((o) => part.includes(o)), `${address}: the body names no other recipient`).toBe(false)
        expect(names.filter((n) => n !== name).some((n) => new RegExp(`\\b${escapeRegExp(n)}\\b`).test(part)), `${address}: the body names no other recipient's first name`).toBe(false)
        expect(/friend|Ivy/.test(part), `${address}: neither the fallback nor the invalid row's name is in the body`).toBe(false)
      }
      expect(/friend|Ivy/.test(message.subject), `${address}: neither the fallback nor the invalid row's name is in the subject`).toBe(false)

      expect(message.listUnsubscribeHeaders, `${address}: one List-Unsubscribe header`).toBe(1)
      expect(message.listUnsubscribePostHeaders, `${address}: one List-Unsubscribe-Post header`).toBe(1)
      expect(message.listUnsubscribePost, `${address}: the one-click form`).toBe('List-Unsubscribe=One-Click')
      const link = message.unsubscribeUrl()
      expect(link.startsWith(`${appUrl()}/u/`) && link.startsWith('https://'), `${address}: the link is an https address under the platform's own`).toBe(true)
      expect(message.html.includes(`href="${link}"`), `${address}: the HTML carries the same link`).toBe(true)
      expect(message.text.includes(link), `${address}: the plain text carries the same link`).toBe(true)
      links.push(link)
    }
    expect(new Set(links).size, 'a link of its own for each recipient').toBe(sendable.length)
    // every message that was going to be sent is in: the row without a first name and the invalid address got nothing
    expect(await sinkCount(request, blank), 'the row without a first name received nothing').toBe(0)
    expect(await sinkCount(request, invalid), 'the invalid address received nothing').toBe(0)
  })

  await test.step('the dashboard\'s credits: four sent, four charged, the credits set aside came back', async () => {
    await expect
      .poll(
        async () => {
          const now = (await balances(request, session)).m2
          return `charged ${m2Before.available - now.available}, set aside changed by ${now.reserved - m2Before.reserved}`
        },
        { message: 'the balance after the job', timeout: 60_000, intervals: [2_000] },
      )
      .toBe(`charged ${sendable.length * unit}, set aside changed by 0`)
  })

  const mails = sendable.map((address, i) => ({ address, link: must(links[i]) }))
  const unsubscribed = [must(mails[0]), must(mails[1])]
  const remaining = [must(mails[2]).address, must(mails[3]).address]

  const suppressionListed = async (address: string): Promise<boolean> => (await suppressionsMatching(request, session, address)).some((e) => e.email === address && e.source === 'unsubscribe')

  await test.step('the recipient opens the link in a browser: the page asks, and opening it changes nothing', async () => {
    const recipient = await context.newPage()
    await recipient.goto(unsubscribed[0]!.link)
    await expect(h1(recipient, UNSUBSCRIBE_PAGE.confirmTitle)).toBeVisible()
    await expect(recipient.getByRole('button', { name: UNSUBSCRIBE_PAGE.confirmButton, exact: true })).toBeVisible()
    await settled(recipient, 'send-unsubscribe-confirm')
    // as the user: the address is not on the suppression list (the Suppressions screen, and the API as the second opinion)
    await goTo(page, 'dashboard')
    await goTo(page, 'suppressions')
    await page.getByLabel(t('suppressions.search.label'), { exact: true }).fill(unsubscribed[0]!.address)
    await expect(page.getByText(t('suppressions.empty.search', { query: unsubscribed[0]!.address }))).toBeVisible()
    expect(await suppressionListed(unsubscribed[0]!.address), 'the API does not list it either').toBe(false)

    await test.step('pressing the button unsubscribes: the thank-you page', async () => {
      await recipient.getByRole('button', { name: UNSUBSCRIBE_PAGE.confirmButton, exact: true }).click()
      await expect(h1(recipient, UNSUBSCRIBE_PAGE.thanksTitle)).toBeVisible()
      await expect(recipient.getByText(UNSUBSCRIBE_PAGE.thanksText)).toBeVisible()
      await settled(recipient, 'send-unsubscribe-thanks')
    })
    await recipient.close()
  })

  await test.step('the one-click POST a mail client makes, for another recipient, lands on the list too (and a repeat adds no second entry)', async () => {
    for (let attempt = 1; attempt <= 2; attempt++) {
      const res = await request.post(unsubscribed[1]!.link, { form: { 'List-Unsubscribe': 'One-Click' } })
      expect(res.status(), `one-click POST ${attempt}`).toBe(200)
      expect((await res.text()).includes(UNSUBSCRIBE_PAGE.thanksTitle), `one-click POST ${attempt}: the thank-you page`).toBe(true)
    }
    expect((await suppressionsMatching(request, session, unsubscribed[1]!.address)).length, 'one entry, not two').toBe(1)
  })

  await test.step('as the user: the Suppressions screen lists both, as "They unsubscribed"', async () => {
    for (const { address } of unsubscribed) {
      await goTo(page, 'dashboard')
      await goTo(page, 'suppressions')
      await page.getByLabel(t('suppressions.search.label'), { exact: true }).fill(address)
      const row = page.getByTestId(/^suppression-row-/).filter({ hasText: address })
      await expect(row).toHaveCount(1)
      await expect(row).toContainText(t('suppressions.source.unsubscribe'))
    }
    await settled(page, 'send-suppressions-unsubscribed')
    expect(await suppressionListed(unsubscribed[0]!.address), 'the API lists the first').toBe(true)
    expect(await suppressionListed(unsubscribed[1]!.address), 'the API lists the second').toBe(true)
  })

  const secondJob = await test.step('a NEW send job over the same verification: the preview counts both as suppressed, the blank row is still missing', async () => {
    await chooseSender(page, 'm2')
    await chooseVerification(page, verification)
    await chooseUnknown(page, 'include') // the other explicit answer: this verification has no unknown address, so it changes nothing
    await continueTo(page, 3)
    await writeMessage(page, { subject: SUBJECT, html: HTML })
    await toPreview(page)
    expect(await statNumber(page, t('newjob.preview.suppressedSend')), 'the two unsubscribed addresses are left out').toBe(2)
    expect(await statNumber(page, t('newjob.preview.missing')), 'the row without a first name is still missing (it was never suppressed)').toBe(1)
    expect(await statNumber(page, t('newjob.preview.unitsSend')), 'messages that will be sent').toBe(remaining.length)
    expect(await statNumber(page, t('newjob.preview.credits')), 'credits held').toBe(remaining.length * unit)
    await settled(page, 'send-preview-suppressed')
    const id = await confirmNow(page)
    await expect(page.getByText(t('newjob.done.pending', { count: remaining.length }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.suppressed', { count: 2 }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.missing', { count: 1 }))).toBeVisible()
    await openConfirmedJob(page)
    await waitForJobStatus(page, id, 'succeeded')
    return id
  })

  await test.step('the results of the second job: the two are "Suppressed", the other two "Sent", the blank row "Missing a value"', async () => {
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(6)
    const rows = await readResultRows(page)
    for (const { address } of unsubscribed) expect(must(rows.find((r) => r.address.toLowerCase() === address)).outcome, `${address}: left out`).toBe(t('common.resultState.suppressed'))
    for (const address of remaining) expect(must(rows.find((r) => r.address.toLowerCase() === address)).outcome, `${address}: sent`).toBe(t('common.resultState.sent'))
    expect(must(rows.find((r) => r.address.toLowerCase() === blank)).outcome, 'the row without a first name').toBe(t('common.resultState.missing_field'))
    expect(secondJob === jobId, 'it is a new job').toBe(false)
  })

  await test.step('at the sink: the others get a second message (their own name again), the two who unsubscribed and the blank row get nothing more', async () => {
    for (const address of remaining) {
      const got = await readSinkFor(request, address, { count: 2 })
      expect(got.length, `${address}: a second message`).toBe(2)
      const latest = must(got[got.length - 1])
      expect(subjectOf(list.nameOf(address), address).test(latest.subject), `${address}: the second message is theirs too`).toBe(true)
    }
    // every message of the second job has arrived (the other two are in): the two who unsubscribed still have the one of the first job
    for (const { address } of unsubscribed) expect(await sinkCount(request, address), `${address}: no new message after unsubscribing`).toBe(1)
    expect(await sinkCount(request, blank), 'the row without a first name still has nothing').toBe(0)
    expect(await sinkCount(request, invalid), 'the invalid address still has nothing').toBe(0)
  })
})

test('a send from a file: the palette and the toolbar write the template; the row that lacks a value is counted, named and not sent; a blank value with a fallback is sent with it', async ({ page, request }) => {
  test.setTimeout(420_000)
  const run = runId()
  const addresses = recipients(`cf${run}`, 5)
  const at = (i: number): string => must(addresses[i])
  const [alice, bob, carol, dave, erin] = [at(0), at(1), at(2), at(3), at(4)]
  // the third row has no first_name (and the subject has no fallback for it): it is left out. The second has no topic (the body has a fallback for it): it is sent with the fallback.
  const list = [
    [alice, 'Alice', 'payments'],
    [bob, 'Bob', ''],
    [carol, '', 'logistics'],
    [dave, 'Dave', 'onboarding'],
    [erin, 'Erin', 'analytics'],
  ] as const
  const sent = [alice, bob, dave, erin]
  const text = ['email,first_name,topic', ...list.map((r) => r.join(','))].join('\n') + '\n'
  const session = await openAs(page, request, 'a')
  const m2Before = (await balances(request, session)).m2

  await chooseSender(page, 'm2')
  await chooseListFile(page, csvFile(`send-${run}.csv`, text))

  await test.step('the list: the address column is chosen, the other columns are the merge fields', async () => {
    await expect(page.getByText(t('newjob.csv.found', { rows: 5, columns: 3 }))).toBeVisible()
    await expect(page.getByLabel(t('newjob.columns.address'), { exact: true })).toHaveValue('0')
    await expect(page.getByRole('list', { name: t('newjob.columns.mergeTitleSend') }).getByRole('listitem')).toHaveText(['first_name', 'topic'])
    await settled(page, 'send-step-list-file')
    await continueTo(page, 3)
  })

  const html = page.getByLabel(t('newjob.authoring.html'), { exact: true })
  await test.step('the message: typed subject, a field from the palette (with a fallback), the unsubscribe link from the toolbar', async () => {
    await page.getByLabel(t('newjob.authoring.subject'), { exact: true }).fill('Hi {{first_name}}, a note about {{topic|your team}}')
    await html.fill('<p>Hello {{first_name}},</p>\n<p>About: </p>')
    // the caret goes before the last </p>; the palette puts the field where the user is typing, the toolbar adds the link at the end
    await html.evaluate((el: HTMLTextAreaElement) => {
      const at = el.value.lastIndexOf('</p>')
      el.setSelectionRange(at, at)
    })
    await page.getByLabel(t('newjob.authoring.fallback')).fill('your team')
    await page.getByRole('list', { name: t('newjob.authoring.columnsFields') }).getByRole('button', { name: t('newjob.authoring.insertField', { name: 'topic' }), exact: true }).click()
    await expect(html).toHaveValue(/<p>About: \{\{topic\|your team\}\}<\/p>/)
    await html.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(el.value.length, el.value.length))
    await page.getByRole('group', { name: t('newjob.authoring.toolbar') }).getByRole('button', { name: t('newjob.authoring.tag.unsubscribe'), exact: true }).click()
    await expect(html).toHaveValue(/<a href="\{\{unsubscribe_url\}\}">Unsubscribe<\/a>$/)

    const live = page.getByRole('status').filter({ hasText: t('newjob.authoring.ok') })
    await expect(live).toBeVisible()
    await expect(live).toContainText(`first_name (${t('newjob.authoring.noFallback')})`)
    await expect(live).toContainText(`topic (${t('newjob.authoring.hasFallback')})`)
    await expect(page.getByRole('button', { name: t('newjob.message.next'), exact: true })).toBeEnabled()
    await settled(page, 'send-step-message-file')
  })

  const unit = await test.step('the preview: one row lacks a value and is named; four messages; the samples are rows that will be sent', async () => {
    await toPreview(page)
    expect(await statNumber(page, t('newjob.preview.rows')), 'rows in the file').toBe(5)
    expect(await statNumber(page, t('newjob.preview.missing')), 'rows missing a value the message needs').toBe(1)
    await expect(statNote(page, t('newjob.preview.missing'))).toContainText(`${t('newjob.preview.missingNames')} first_name`)
    expect(await statNumber(page, t('newjob.preview.unitsSend')), 'messages that will be sent').toBe(4)
    const price = await unitCost(page, 'send')
    expect(await statNumber(page, t('newjob.preview.credits')), 'credits held').toBe(4 * price)
    await expect(stat(page, t('newjob.preview.suppressedSend'))).toBeVisible()
    await expect(page.getByTestId(/^sample-/)).toHaveCount(3)
    for (const n of [1, 2, 3]) {
      const subject = await page.getByTestId(`sample-${n}`).getByRole('definition').first().innerText()
      expect(/^Hi \S+, a note about \S/.test(subject), `sample ${n}: a rendered subject with a first name (the row without one is never a sample)`).toBe(true)
    }
    await settled(page, 'send-step-preview-file')
    return price
  })

  const jobId = await test.step('confirm: the confirmation says one row was left out; the job runs to "Finished"', async () => {
    const id = await confirmNow(page)
    await expect(page.getByText(t('newjob.done.pending', { count: 4 }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.missing', { count: 1 }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.reserved', { credits: fmt(4 * unit) }))).toBeVisible()
    await openConfirmedJob(page)
    await waitForJobStatus(page, id, 'succeeded')
    return id
  })

  await test.step('the results: four sent, the row without a first name "Missing a value", with the field named', async () => {
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(5)
    const rows = await readResultRows(page)
    for (const address of sent) expect(must(rows.find((r) => r.address.toLowerCase() === address), 'a result').outcome, `${address}: sent`).toBe(t('common.resultState.sent'))
    const left = must(rows.find((r) => r.address.toLowerCase() === carol), 'the result of the row without a first name')
    expect(left.outcome, 'the row without a first name').toBe(t('common.resultState.missing_field'))
    expect(left.details, 'the field is named').toContain(t('jobs.results.missingFields', { fields: 'first_name' }))
    expect(left.cost, 'it costs nothing').toBe(t('jobs.cost.free'))
    await settled(page, 'send-job-finished-file')
    expect(jobId.length > 0).toBe(true)
  })

  await test.step('at the sink: each of the four has its own first name and topic; the blank topic got the fallback; the row without a first name got nothing', async () => {
    const byAddress = new Map<string, { first: string; topic: string }>(list.map((r): [string, { first: string; topic: string }] => [r[0], { first: r[1], topic: r[2] }]))
    for (const address of sent) {
      const [m, ...others] = await readSinkFor(request, address)
      expect(others.length, `${address}: exactly one message`).toBe(0)
      const message = must(m, 'the message of a recipient')
      const row = must(byAddress.get(address), 'the row of a recipient')
      const topic = row.topic === '' ? 'your team' : row.topic
      expect(message.subject, `${address}: the subject has this row's first name and topic`).toBe(`Hi ${row.first}, a note about ${topic}`)
      expect(message.html.includes(`Hello ${row.first},`) && message.html.includes(`About: ${topic}`), `${address}: the HTML has this row's first name and topic`).toBe(true)
      expect(message.text.includes(row.first) && message.text.includes(topic), `${address}: the plain text, made from the HTML, has them too`).toBe(true)
      expect(/[{}]/.test(message.html + message.text + message.subject), `${address}: no brace of the template is left`).toBe(false)
      expect(message.listUnsubscribeHeaders, `${address}: one List-Unsubscribe header`).toBe(1)
      expect(message.listUnsubscribePostHeaders, `${address}: one List-Unsubscribe-Post header`).toBe(1)
      expect(message.html.includes(`href="${message.unsubscribeUrl()}"`), `${address}: the HTML carries the link of the header`).toBe(true)
    }
    // all four have arrived: the job is over at the sink, and the fifth row never reached it
    expect(await sinkCount(request, carol), 'the row that lacked a value received nothing').toBe(0)
  })

  await test.step('the credits: four sent, four charged', async () => {
    await expect
      .poll(
        async () => {
          const now = (await balances(request, session)).m2
          return `charged ${m2Before.available - now.available}, set aside changed by ${now.reserved - m2Before.reserved}`
        },
        { message: 'the balance after the job', timeout: 60_000, intervals: [2_000] },
      )
      .toBe(`charged ${4 * unit}, set aside changed by 0`)
    // the screens of the user's own were checked above; the recipients' pages are the next test's subject
    await expectNoInternalWords(page)
    await expectNoA11yViolations(page, { exclude: ['[data-testid=preview-frame]'] })
    await shot(page, 'send-job-finished-file-after')
  })
})
