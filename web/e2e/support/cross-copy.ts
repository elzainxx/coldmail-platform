// "No internal text reaches a screen" for the specs of chunk A11: a thin layer over support/copy.ts (`expectNoInternalWords`), which owns the deny-list (web/src/copy/internal-words.json) and the reading of the
// page. Every screen is judged with the WHOLE list: there is no allowance (the merge-field name `unsubscribe_url`, which the message step of a send job must show, is not on the list: it is the user's own
// vocabulary, commit 10f243f).
import type { Page } from '@playwright/test'
import { expectNoInternalWords, internalWords } from './copy.ts'

export async function expectNoInternalText(page: Page): Promise<void> {
  await expectNoInternalWords(page)
}

/** An internal word of the list: what the paired controls put into a page to prove the check can fail. */
export function someInternalWord(): string {
  const word = internalWords()[0]
  if (word === undefined) throw new Error('the deny-list is empty')
  return word
}
