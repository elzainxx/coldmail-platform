// The merge fields a job carries (`merge_fields` of the job JSON): sorted, unique, valid names, never `email` or `unsubscribe_url`. A send made from a finished
// verification carries the fields of that verification's rows, so they are what its message may name. An older answer has no such member: it reads as none.
import type { Job } from '../../api/types.ts'
import { isFieldName, RESERVED_FIELDS, type MergeColumn } from './csv/columns.ts'

export function mergeFieldsOf(job: Pick<Job, 'merge_fields'> | null | undefined): string[] {
  const fields = job?.merge_fields
  if (!Array.isArray(fields)) return []
  // The two reserved names are always there and never a field of a list: one that came back would be a second button for the same thing.
  return fields.filter((f): f is string => typeof f === 'string' && f !== '' && !(RESERVED_FIELDS as readonly string[]).includes(f))
}

/** The names as the columns the authoring screen's palette is made of. */
export function fieldColumns(names: readonly string[]): MergeColumn[] {
  return names.map((name, index) => ({ index, name, usable: isFieldName(name) }))
}
