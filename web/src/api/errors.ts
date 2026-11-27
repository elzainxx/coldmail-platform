// The platform's error envelope as the client sees it (Docs/jobs-api.md, "Errors"):
//
//   { "error": { "code", "message", "request_id", "details"? } }
//
// `message` is a sentence for developers: it is read nowhere in this app and is NOT kept on ApiError (so no screen can show it by accident).
// A screen turns an ApiError into words with errorText(err) from src/copy.

/** Every code of the envelope the platform sends today (Docs/jobs-api.md), kept in step with the catalogue by copy.test.ts. */
export const API_ERROR_CODES = [
  'unauthenticated',
  'not_found',
  'validation_failed',
  'bad_request',
  'payload_too_large',
  'too_many_rows',
  'unsupported_media_type',
  'nothing_to_do',
  'unknown_choice_required',
  'preview_stale',
  'job_slot_taken',
  'idempotency_key_reused',
  'not_previewed',
  'infeasible',
  'not_allowed',
  'insufficient_credits',
  'no_entitlement',
  'suspended',
  'account_duplicate',
  'account_action_unavailable',
  'host_forbidden',
  'from_gate_ack_required',
  'rate_limited',
  'module_unavailable',
  'account_outcome_unknown',
  'busy',
  'auth_unavailable',
  'internal_error',
  'method_not_allowed',
] as const

/**
 * The code of one line of `POST /v1/m3/accounts/bulk` that is not a success (`created` and `created_unverified` are the two that are): the same words as the
 * errors of the same name. `account_limit_reached` is not an error of its own (a single add reports it as account_action_unavailable) and has its line in
 * copy/errors.ts all the same.
 */
export const M3_BULK_ERROR_CODES = [
  'account_duplicate',
  'from_gate_ack_required',
  'host_forbidden',
  'account_limit_reached',
  'validation_failed',
  'account_action_unavailable',
  'not_found',
  'internal_error',
] as const

/** The `details.reason` words of `409 not_allowed`: a closed list of the API's own words. */
export const NOT_ALLOWED_REASONS = [
  'pause_not_supported',
  'cancel_not_supported',
  'not_a_draft',
  'already_a_draft',
  'not_confirmed',
  'not_paused',
  'start_deadline_passed',
  'submitted_job_ends_through_the_engine',
  'unsubscribe_not_configured',
  'opt_out_policy_unavailable',
  'job_status_has_no_preview',
] as const

/** The `details.reason` words of `409 preview_stale`. */
export const PREVIEW_STALE_REASONS = ['unknown_choice_changed'] as const

/** Codes whose `details.reason` is one of a closed list of words (every other code's reason is a sentence for developers and is never shown). */
export const CLOSED_REASON_CODES = ['not_allowed', 'preview_stale'] as const

/** Codes the app makes itself (status 0 or an unreadable reply): never sent by the platform. */
export const CLIENT_ERROR_CODES = ['network_error', 'bad_response'] as const

export type ApiErrorDetails = Record<string, unknown>

export interface ApiErrorInit {
  status: number
  code: string
  details?: ApiErrorDetails
  requestId?: string
  /** seconds, from a Retry-After header */
  retryAfterSeconds?: number
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  /** details.reason when it is a string: a word of the API's for the codes in CLOSED_REASON_CODES, a developer sentence for the others (never display it raw) */
  readonly reason: string | undefined
  readonly details: ApiErrorDetails | undefined
  readonly requestId: string | undefined
  readonly retryAfterSeconds: number | undefined

  constructor(init: ApiErrorInit) {
    // The Error message is the code, never the server's sentence.
    super(init.code)
    this.name = 'ApiError'
    this.status = init.status
    this.code = init.code
    this.details = init.details
    const reason = init.details?.['reason']
    this.reason = typeof reason === 'string' ? reason : undefined
    this.requestId = init.requestId
    this.retryAfterSeconds = init.retryAfterSeconds
  }
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError
}

export function isAbortError(e: unknown): boolean {
  return typeof e === 'object' && e !== null && (e as { name?: unknown }).name === 'AbortError'
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined
}

/**
 * Read an error response. `text` is the body (it may be empty, HTML from a proxy or anything else): whatever it is, the result is an ApiError, and an
 * unreadable body is `bad_response`, never an exception.
 */
export function apiErrorFromResponse(status: number, text: string, retryAfter: string | null): ApiError {
  const retryAfterSeconds = parseRetryAfter(retryAfter)
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    parsed = undefined
  }
  const envelope = asRecord(asRecord(parsed)?.['error'])
  const code = envelope?.['code']
  if (typeof code !== 'string' || code === '') {
    return new ApiError({ status, code: 'bad_response', retryAfterSeconds })
  }
  const requestId = envelope?.['request_id']
  // The doc puts `details` inside `error`; a sibling `details` is read too, in case the envelope is ever flattened.
  const details = asRecord(envelope?.['details']) ?? asRecord(asRecord(parsed)?.['details'])
  return new ApiError({
    status,
    code,
    details,
    requestId: typeof requestId === 'string' ? requestId : undefined,
    retryAfterSeconds,
  })
}

/** Retry-After as whole seconds (the platform sends 1 for busy and 30 for auth_unavailable); an HTTP date is not read. */
export function parseRetryAfter(value: string | null): number | undefined {
  if (value === null) return undefined
  const n = Number(value.trim())
  return Number.isInteger(n) && n >= 0 && n <= 3600 ? n : undefined
}
