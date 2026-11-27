// The Idempotency-Key of a confirm: 1 to 128 characters of letters, digits and . _ : - (Docs/jobs-api.md). A fresh key per confirm INTENT: the screen makes one
// when the user first presses Confirm and sends the SAME key again for a retry, a double click or a lost answer, so the platform confirms the job once.

export function newIdempotencyKey(): string {
  const c = globalThis.crypto
  if (typeof c?.randomUUID === 'function') return `confirm-${c.randomUUID()}`
  const bytes = new Uint8Array(16)
  c.getRandomValues(bytes)
  return `confirm-${[...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')}`
}

export const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/
