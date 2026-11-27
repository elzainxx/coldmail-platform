// The words of the API's closed vocabularies (a job's status, a result's state, a reason), turned into customer wording.
// An unknown word is never shown as it is: the state falls back to 'Other', a reason to 'No further detail', a malformed reason to 'The address is not valid'.
import type { ApiError } from '../api/errors.ts'
import { maybeT, t } from './index.ts'

/** "Running", "Finished"... for the `status` of a job. */
export function jobStatusText(status: string): string {
  return maybeT(`common.jobStatus.${status}`) ?? t('common.resultState.other')
}

/** "Verify", "Send (our servers)"... for the `module` of a job (m1, m2, m3). */
export function moduleText(module: string): string {
  return maybeT(`common.module.${module}`) ?? t('common.resultState.other')
}

/** "Valid", "Suppressed", "Missing a value"... for the `state` of a result. */
export function resultStateText(state: string): string {
  return maybeT(`common.resultState.${state}`) ?? t('common.resultState.other')
}

/**
 * The sentence for the `reason` of a result, or null when the result has none. A `missing_field` result's reason is the list of the user's own field
 * names (see missingFieldNames), so it has no sentence of ours.
 */
export function resultReasonText(reason: string | null | undefined, state?: string): string | null {
  if (reason === null || reason === undefined || reason === '' || state === 'missing_field') return null
  return maybeT(`common.reason.${reason}`) ?? t('common.reason.other')
}

/** The field names a `missing_field` result lists in its reason (the user's own column names, comma separated: data, not copy). */
export function missingFieldNames(reason: string | null | undefined): string[] {
  if (reason === null || reason === undefined) return []
  return reason.split(',').map((s) => s.trim()).filter((s) => s !== '')
}

/** The sentence for a word of `preview.malformed_reasons` or an import's invalid reason. */
export function malformedReasonText(reason: string): string {
  return maybeT(`common.malformed.${reason}`) ?? t('common.malformed.other')
}

/** The one word of the closed list that says why an address was refused (`details.address_reason` of a 400 on the suppression routes), or null. */
export function addressReasonOf(err: ApiError): string | null {
  const v = err.details?.['address_reason']
  return typeof v === 'string' ? v : null
}

export interface FieldIssue {
  field: string
  /** one of required, out_of_range, malformed, not_found, conflict, forbidden_value (fieldIssueText shows an unknown word as "malformed") */
  issue: string
}

/** The fields a module 3 account was refused for (`details.fields`: a list of "field:issue" words), so a form can mark each one. */
export function fieldIssuesOf(err: ApiError): FieldIssue[] {
  const list = err.details?.['fields']
  if (!Array.isArray(list)) return []
  return list.flatMap((item): FieldIssue[] => {
    if (typeof item !== 'string') return []
    const [field, issue] = item.split(':')
    return field === undefined || field === '' ? [] : [{ field, issue: issue ?? 'malformed' }]
  })
}

/** The sentence for the issue of one field. */
export function fieldIssueText(issue: string): string {
  return maybeT(`error.field.${issue}`) ?? t('error.field.malformed')
}
