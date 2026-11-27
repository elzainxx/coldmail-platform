// The words of the account screens, from the closed vocabularies of the API (types.ts). Each function turns a word into a line of the copy catalogue and an
// unknown word into a safe one, never into the word itself: the platform may grow a state, and the screen must still read.
import { isApiError, type ApiError } from '../../api/errors.ts'
import type { FromGateState, M3Account, M3BulkCode, M3PauseReason, M3QuarantineReason, M3TestStage, M3VerifyCode } from '../../api/types.ts'
import { errorReference, errorText, t, type CopyKey } from '../../copy/index.ts'
import { fieldIssuesOf } from '../../copy/words.ts'
import type { StatusToken } from '../../ui/index.ts'

// ---- the check of the From address ----

interface GateWords {
  label: CopyKey
  why: CopyKey
  token: StatusToken
}

const FROM_GATE: Record<FromGateState, GateWords> = {
  aligned: { label: 'accounts.fromGate.aligned', why: 'accounts.fromGate.alignedWhy', token: 'valid' },
  unverified: { label: 'accounts.fromGate.unverified', why: 'accounts.fromGate.unverifiedWhy', token: 'unknown' },
  unaligned_warned: { label: 'accounts.fromGate.warned', why: 'accounts.fromGate.warnedWhy', token: 'risky' },
  blocked: { label: 'accounts.fromGate.blocked', why: 'accounts.fromGate.blockedWhy', token: 'invalid' },
  indeterminate: { label: 'accounts.fromGate.indeterminate', why: 'accounts.fromGate.indeterminateWhy', token: 'unknown' },
}

function gateWords(state: string): GateWords {
  return FROM_GATE[state as FromGateState] ?? FROM_GATE.indeterminate
}

export const fromGateLabel = (state: string): string => t(gateWords(state).label)
export const fromGateWhy = (state: string): string => t(gateWords(state).why)
export const fromGateToken = (state: string): StatusToken => gateWords(state).token

// ---- the state of an account ----

const STATUS_WORD: Record<string, { label: CopyKey; token: StatusToken }> = {
  active: { label: 'accounts.status.active', token: 'valid' },
  paused: { label: 'accounts.status.paused', token: 'risky' },
  quarantined: { label: 'accounts.status.quarantined', token: 'invalid' },
  disabled: { label: 'accounts.status.disabled', token: 'unknown' },
}

export function statusLabel(status: string): string {
  return t((STATUS_WORD[status] ?? STATUS_WORD.active!).label)
}

export function statusToken(status: string): StatusToken {
  return (STATUS_WORD[status] ?? { token: 'unknown' as StatusToken }).token
}

const PAUSE_REASON: Partial<Record<M3PauseReason, CopyKey>> = {
  daily_limit: 'accounts.pause.dailyLimit',
  throttle: 'accounts.pause.throttle',
  manual: 'accounts.pause.manual',
}

const QUARANTINE_REASON: Partial<Record<M3QuarantineReason, CopyKey>> = {
  auth_failure: 'accounts.hold.authFailure',
  consecutive_failures: 'accounts.hold.consecutiveFailures',
  failure_rate: 'accounts.hold.failureRate',
  manual: 'accounts.hold.manual',
}

/** The sentence that says why an account is paused or on hold, or null when it is neither or the reason is not one we have words for. */
export function statusReason(account: Pick<M3Account, 'status' | 'pause_reason' | 'quarantine_reason'>): string | null {
  const key =
    account.status === 'quarantined' && account.quarantine_reason !== null
      ? QUARANTINE_REASON[account.quarantine_reason]
      : account.status === 'paused' && account.pause_reason !== null
        ? PAUSE_REASON[account.pause_reason]
        : undefined
  return key === undefined ? null : t(key)
}

// ---- the login check ----

const WHY: Record<M3VerifyCode, CopyKey> = {
  verify_connect_failed: 'accounts.why.connectFailed',
  verify_timeout: 'accounts.why.timeout',
  verify_tls_unavailable: 'accounts.why.tlsUnavailable',
  verify_tls_failed: 'accounts.why.tlsFailed',
  verify_auth_failed: 'accounts.why.authFailed',
  verify_protocol_error: 'accounts.why.protocolError',
  verify_connection_lost: 'accounts.why.connectionLost',
  verify_host_not_allowed: 'accounts.why.hostNotAllowed',
  verify_error: 'accounts.why.error',
  verification_unavailable: 'accounts.why.unavailable',
}

/** Why a login check failed, as a phrase to put after "the last login check failed:" (an unknown or missing code is "something went wrong"). */
export function verifyWhy(code: string | null): string {
  return t(WHY[code as M3VerifyCode] ?? 'accounts.why.error')
}

/** The line about the login of an account: it works, it was not checked, or why it failed. */
export function verificationLine(account: Pick<M3Account, 'verification' | 'verify_code'>): string {
  if (account.verification === 'verified') return t('accounts.verification.verified')
  if (account.verification === 'failed') return t('accounts.verification.failed', { why: verifyWhy(account.verify_code) })
  return t('accounts.verification.unverified')
}

// ---- the test button ----

const STAGE: Record<M3TestStage, CopyKey> = {
  done: 'accounts.stage.done',
  connect: 'accounts.stage.connect',
  host: 'accounts.stage.host',
  tls: 'accounts.stage.tls',
  login: 'accounts.stage.login',
  protocol: 'accounts.stage.protocol',
  unavailable: 'accounts.stage.unavailable',
}

export const stageText = (stage: string): string => t(STAGE[stage as M3TestStage] ?? STAGE.unavailable)

// ---- one line of a bulk add ----

interface BulkWords {
  label: CopyKey
  why: CopyKey | null
  token: StatusToken
}

const BULK: Record<M3BulkCode, BulkWords> = {
  created: { label: 'accounts.bulk.created', why: null, token: 'valid' },
  created_unverified: { label: 'accounts.bulk.createdUnverified', why: 'accounts.bulk.createdUnverifiedWhy', token: 'risky' },
  account_duplicate: { label: 'accounts.bulk.duplicate', why: null, token: 'unknown' },
  from_gate_ack_required: { label: 'accounts.bulk.ackRequired', why: 'accounts.bulk.ackRequiredWhy', token: 'risky' },
  host_forbidden: { label: 'accounts.bulk.hostForbidden', why: null, token: 'invalid' },
  account_limit_reached: { label: 'accounts.bulk.limitReached', why: null, token: 'invalid' },
  validation_failed: { label: 'accounts.bulk.invalid', why: 'accounts.bulk.invalidWhy', token: 'invalid' },
  account_action_unavailable: { label: 'accounts.bulk.unavailable', why: null, token: 'invalid' },
  not_found: { label: 'accounts.bulk.failed', why: 'accounts.bulk.failedWhy', token: 'failed' },
  internal_error: { label: 'accounts.bulk.failed', why: 'accounts.bulk.failedWhy', token: 'failed' },
}

const BULK_OTHER: BulkWords = { label: 'accounts.bulk.failed', why: 'accounts.bulk.failedWhy', token: 'failed' }

export const bulkLabel = (code: string): string => t((BULK[code as M3BulkCode] ?? BULK_OTHER).label)
export const bulkToken = (code: string): StatusToken => (BULK[code as M3BulkCode] ?? BULK_OTHER).token

/**
 * What to do about a line that was not added: its own sentence, or the line the same refusal has as an error (a duplicate, a host that is not allowed, the limit,
 * "not available right now"). Null for a line that was added without a remark.
 */
export function bulkDetail(code: string): string | null {
  const words = BULK[code as M3BulkCode] ?? BULK_OTHER
  if (words.why !== null) return t(words.why)
  switch (code) {
    case 'account_duplicate':
      return t('error.account_duplicate')
    case 'host_forbidden':
      return t('error.host_forbidden')
    case 'account_limit_reached':
      return t('error.account_limit_reached')
    case 'account_action_unavailable':
      return t('error.account_action_unavailable')
    default:
      return null
  }
}

/** Is this line of a bulk add an account that now exists? */
export const bulkAdded = (code: string): boolean => code === 'created' || code === 'created_unverified'

// ---- the fields of an account, for a message that points at one ----

const FIELD_WORD: Record<string, CopyKey> = {
  smtp_host: 'accounts.field.server',
  smtp_port: 'accounts.field.port',
  smtp_username: 'accounts.field.username',
  smtp_secret: 'accounts.field.password',
  from_address: 'accounts.field.fromAddress',
  from_name: 'accounts.field.fromName',
  daily_limit: 'accounts.field.dailyLimit',
}

export const fieldWord = (field: string): string => t(FIELD_WORD[field] ?? 'accounts.field.other')

/** The line of a batch the API refused whole (`details.line` and `details.fields`), as a sentence naming the fields in words; null when the error names no line. */
export function badLineText(err: ApiError): string | null {
  const line = err.details?.['line']
  if (typeof line !== 'number' || !Number.isInteger(line) || line < 1) return null
  const names = [...new Set(fieldIssuesOf(err).map((i) => fieldWord(i.field)))]
  if (names.length === 0) return t('accounts.bulk.badLinePlain', { line })
  return t('accounts.bulk.badLine', { line, fields: names.join(', ') })
}

// ---- how long to wait ----

/** "You can try again in about 5 minutes", from the Retry-After of a rate limit; null when the answer gave no wait. */
export function waitText(seconds: number | undefined): string | null {
  if (seconds === undefined || !Number.isFinite(seconds) || seconds < 1) return null
  if (seconds < 2) return t('accounts.wait.second')
  if (seconds < 60) return t('accounts.wait.seconds', { count: Math.round(seconds) })
  const minutes = Math.round(seconds / 60)
  if (minutes <= 1) return t('accounts.wait.minute')
  if (minutes < 60) return t('accounts.wait.minutes', { count: minutes })
  const hours = Math.round(seconds / 3600)
  return hours <= 1 ? t('accounts.wait.hour') : t('accounts.wait.hours', { count: hours })
}

/** A failed call as a page shows it: the one line, the wait when a rate limit gave one, the reference to quote to support. Never the server's message, never a code. */
export interface Failure {
  text: string
  wait: string | null
  reference: string | null
}

export function failureOf(err: unknown): Failure {
  // A thrown value that is not an ApiError is a mistake of the app's own: the user gets the generic line (it was not the user's doing).
  if (!isApiError(err)) return { text: t('error.unknown'), wait: null, reference: null }
  return { text: errorText(err), wait: err.code === 'rate_limited' ? waitText(err.retryAfterSeconds) : null, reference: errorReference(err) }
}

// ---- dates ----

const DATE = new Intl.DateTimeFormat('en-US', { dateStyle: 'long' })

/** "December 1, 2026" for a time of the API ("" for one that is not a time: a line with a hole in it is better than the word "Invalid Date"). */
export function formatDate(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : DATE.format(d)
}

/** The sentence about whether sending through the user's own accounts is switched on (what the balances say), or null when there is nothing to say. */
export function entitlementLine(m3: { enabled: boolean; until: string | null; state: string }, now: Date = new Date()): string | null {
  const ended = m3.until !== null && new Date(m3.until).getTime() <= now.getTime()
  if (!m3.enabled) return ended ? t('accounts.entitlement.ended', { date: formatDate(m3.until ?? '') }) : t('accounts.entitlement.none')
  if (m3.state === 'suspended') return t('accounts.entitlement.suspended')
  return m3.until === null ? null : t('accounts.entitlement.until', { date: formatDate(m3.until) })
}
