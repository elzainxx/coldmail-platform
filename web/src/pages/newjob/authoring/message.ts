// The message the user writes, and what makes it fit to send. The platform judges the templates (POST /v1/templates/check, and again at the create); this only
// holds the three parts and the one thing the screen can say alone: a reply-to is one address or nothing.
import type { TemplateCheckError } from '../../../api/types.ts'

export type Part = 'subject' | 'html' | 'text'

export interface Message {
  subject: string
  html: string
  text: string
  /** one address, or empty for none */
  replyTo: string
}

export const EMPTY_MESSAGE: Message = { subject: '', html: '', text: '', replyTo: '' }

/** One address with something before the @ and a dotted domain after it: the same shape the platform asks for. A blank reply-to (none) is fine. */
export function replyToValid(value: string): boolean {
  const v = value.trim()
  return v === '' || /^[^\s@,;<>]+@[^\s@,;<>]+\.[^\s@,;<>]+$/.test(v)
}

/** A message with a subject and at least one body: what it takes before the platform has anything to check. */
export function messageComplete(m: Message): boolean {
  return m.subject.trim() !== '' && (m.html.trim() !== '' || m.text.trim() !== '')
}

/** The mistake in a part, from the live check's answer (or the platform's refusal of a create), or null. */
export function problemOf(errors: readonly TemplateCheckError[], part: Part): TemplateCheckError | null {
  return errors.find((e) => e.part === part) ?? null
}
