// The seven status tokens: each is a colour in design/tokens.css (light and dark) AND a word in the copy catalogue.
// A badge shows both; colour is never the only signal (spec, "Conventions").
//
// The two helpers below choose the colour for the states the API names, so every screen colours the same state the same way. The WORD of a state is
// copy/words.ts (jobStatusText, resultStateText): <StatusBadge status={jobStatusToken(job.status)} label={jobStatusText(job.status)} />.

export const STATUS_TOKENS = ['valid', 'risky', 'invalid', 'unknown', 'sent', 'bounced', 'failed'] as const

export type StatusToken = (typeof STATUS_TOKENS)[number]

export function isStatusToken(value: string): value is StatusToken {
  return (STATUS_TOKENS as readonly string[]).includes(value)
}

const JOB_STATUS_TOKEN: Record<string, StatusToken> = {
  draft: 'unknown',
  previewed: 'unknown',
  confirmed: 'sent',
  scheduled: 'sent',
  running: 'sent',
  completing: 'sent',
  paused: 'risky',
  cancelling: 'risky',
  succeeded: 'valid',
  cancelled: 'unknown',
  expired: 'unknown',
  failed: 'failed',
}

/** The colour of a job's status: in progress is blue (sent), a hold is amber (risky), finished is green (valid), failed is red-violet, the rest is grey. */
export function jobStatusToken(status: string): StatusToken {
  return JOB_STATUS_TOKEN[status] ?? 'unknown'
}

const RESULT_STATE_TOKEN: Record<string, StatusToken> = {
  valid: 'valid',
  risky: 'risky',
  invalid: 'invalid',
  unknown: 'unknown',
  sent: 'sent',
  bounced: 'bounced',
  failed: 'failed',
  malformed: 'invalid',
  missing_field: 'risky',
  unrenderable: 'failed',
  infrastructure_failure: 'failed',
  pool_exhausted: 'failed',
  smtp_login_rejected: 'failed',
  duplicate: 'unknown',
  suppressed: 'unknown',
  excluded: 'unknown',
  cancelled: 'unknown',
  expired: 'unknown',
  quarantined: 'unknown',
}

/** The colour of a result's state (the module's own and the platform's). An unknown state is grey. */
export function resultStateToken(state: string): StatusToken {
  return RESULT_STATE_TOKEN[state] ?? 'unknown'
}
