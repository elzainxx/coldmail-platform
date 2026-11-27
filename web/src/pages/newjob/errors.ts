// What the screens of this area do with anything that is thrown: an ApiError is kept as it is, anything else (a bug, an odd browser) becomes one
// error with a code no wording exists for, so the user reads the one generic line and the console names it (copy/index.ts errorText).
import { ApiError, isAbortError, isApiError } from '../../api/errors.ts'

export function toApiError(e: unknown): ApiError {
  return isApiError(e) ? e : new ApiError({ status: 0, code: 'client_error' })
}

/** An aborted request is not a failure: the user went on to something else. */
export function isAbort(e: unknown): boolean {
  return isAbortError(e)
}

/** True when a confirm that failed may nevertheless have been done: no answer came, or the service failed. The same attempt (same key) is then repeated, never a new one. */
export function outcomeUnknown(err: ApiError): boolean {
  return err.status === 0 || err.status >= 500
}
