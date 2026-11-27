// One confirm "attempt": the Idempotency-Key a confirm is sent with, made ONCE per intent and sent again when the same intent is repeated (a retry after a
// lost answer, a second click), so the platform confirms the job once however many times the request goes out (Docs/jobs-api.md, "Confirm").
//
// The intent is the job, the preview the user looked at (its fingerprint), when it starts and the choice for unknown addresses. Any of them changing is a new
// intent and gets a new key; a confirm the platform REFUSED (a 4xx) is over, and the next press is a new intent too. Only a confirm whose outcome is not
// known (no answer, a server fault) is repeated with the key it had.
import { newIdempotencyKey } from '../../api/idempotency.ts'

export interface Attempt {
  key: string
  signature: string
}

export interface Intent {
  jobId: string
  fingerprint: string
  /** RFC 3339, or null for "now" */
  scheduledStart: string | null
  unknownChoice: string | null
}

export function intentSignature(i: Intent): string {
  return [i.jobId, i.fingerprint, i.scheduledStart ?? 'now', i.unknownChoice ?? ''].join('|')
}

/** The attempt for this intent: the previous one when it is the same intent, else a new one with a fresh key. */
export function nextAttempt(previous: Attempt | null, intent: Intent, mint: () => string = newIdempotencyKey): Attempt {
  const signature = intentSignature(intent)
  return previous !== null && previous.signature === signature ? previous : { key: mint(), signature }
}
