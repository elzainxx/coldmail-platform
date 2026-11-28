// The flows of the New send job screen (four steps: who sends, the list, the message, the preview with the confirm) and of the public unsubscribe pages, shared by the specs that send.
// Same rules as flows-ui.ts: role and accessible name from the copy catalogue; the session is in memory, so a flow moves by the navigation, never by `page.goto` (except for the recipient's own pages).
import { expect, type Page } from '@playwright/test'
import { t } from '../../src/copy/index.ts'
import { escapeRegExp } from './flows-data.ts'
import { goTo, h2, type CsvPayload, chooseCsv } from './flows-ui.ts'

export type Sender = 'm2' | 'm3'
export type UnknownChoice = 'include' | 'exclude'
export type StepNumber = 1 | 2 | 3 | 4

const STEP_TITLE = { 1: 'newjob.step.sender', 2: 'newjob.step.list', 3: 'newjob.step.message', 4: 'newjob.step.preview' } as const

/** The heading of a step ("Step 2 of 4: List"). */
export function stepHeading(n: StepNumber): string {
  return t('newjob.step.heading', { n, total: 4, title: t(STEP_TITLE[n]) })
}

/** Step 1: opens New send job by the navigation, picks who sends, and goes on to the list. */
export async function chooseSender(page: Page, sender: Sender): Promise<void> {
  await goTo(page, 'newSend')
  await expect(h2(page, stepHeading(1))).toBeVisible()
  await page.getByRole('group', { name: t('newjob.sender.legend') }).getByRole('radio', { name: t(sender === 'm2' ? 'newjob.sender.managed' : 'newjob.sender.own'), exact: true }).check()
  await page.getByRole('button', { name: t('newjob.step.next'), exact: true }).click()
  await expect(h2(page, stepHeading(2))).toBeVisible()
}

/**
 * Step 2, "From a finished verification": picks the verification BY ITS ID. The radio's accessible name is "Finished <the time>", which two verifications can share to the minute, so the one thing that
 * names one verification is the radio's value (the job id): this is the one selector of the flows that is not role plus name, and it is a role plus an attribute.
 */
export async function chooseVerification(page: Page, jobId: string): Promise<void> {
  await page.getByRole('radio', { name: t('newjob.list.verification'), exact: true }).check()
  await page.locator(`input[type="radio"][value="${jobId}"]`).check()
}

/** The answer to "what should happen to unknown addresses?" (there is no default: Continue stays closed until one is chosen). */
export async function chooseUnknown(page: Page, choice: UnknownChoice): Promise<void> {
  await page.getByRole('radio', { name: t(choice === 'include' ? 'newjob.unknown.include' : 'newjob.unknown.exclude'), exact: true }).check()
}

/** Step 2, "A file of addresses" (the default): the file chosen with the file chooser. */
export async function chooseListFile(page: Page, file: CsvPayload): Promise<void> {
  await page.getByRole('radio', { name: t('newjob.list.csv'), exact: true }).check()
  await chooseCsv(page, file)
}

/** Continue from a step to the next (the button is "Continue" on steps 1 and 2); waits for the next step's heading. */
export async function continueTo(page: Page, step: 3): Promise<void> {
  await page.getByRole('button', { name: t('newjob.step.next'), exact: true }).click()
  await expect(h2(page, stepHeading(step))).toBeVisible()
}

export interface Message {
  subject: string
  html: string
  text?: string
}

/** Step 3: the subject and the HTML (and the plain text when given), typed into their fields. A field marked "optional" has "(optional)" in its label, so the plain text field is found by its words alone. */
export async function writeMessage(page: Page, message: Message): Promise<void> {
  await page.getByLabel(t('newjob.authoring.subject'), { exact: true }).fill(message.subject)
  await page.getByLabel(t('newjob.authoring.html'), { exact: true }).fill(message.html)
  if (message.text !== undefined) await page.getByLabel(t('newjob.authoring.text')).fill(message.text)
}

/** Step 3 to step 4: "Check the list and preview"; waits until the draft is made and its numbers are on the screen. */
export async function toPreview(page: Page): Promise<void> {
  await page.getByRole('button', { name: t('newjob.message.next'), exact: true }).click()
  await expect(h2(page, stepHeading(4))).toBeVisible()
  await expect(h2(page, t('newjob.preview.title'))).toBeVisible()
}

/** The heading of one sample message of the preview as a pattern ("Sample 2 (row 3)"). */
export function sampleHeading(n: number): RegExp {
  return new RegExp(`^${escapeRegExp(t('newjob.samples.sample', { n, row: '@@' })).replace('@@', '\\d[\\d,]*')}$`)
}

/** What the recipient's own pages say (internal/unsubscribe/endpoint/endpoint.go: static HTML of the Go service, not the app's catalogue). */
export const UNSUBSCRIBE_PAGE = {
  confirmTitle: 'Unsubscribe',
  confirmButton: 'Unsubscribe',
  thanksTitle: 'You are unsubscribed',
  thanksText: 'Your request was recorded.',
  invalidTitle: 'This link is not valid',
} as const
