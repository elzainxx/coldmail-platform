// "What changed" when a confirm is answered with a stale preview and the screen has looked again: the figures that moved, each as a sentence the user can read.
// Nothing here knows the platform's word for the refusal; it only compares two previews.
import type { Preview, UnknownChoice } from '../../api/types.ts'
import { t, type CopyKey } from '../../copy/index.ts'
import { fmt } from './schedule.ts'

export type NumberField =
  | 'rows'
  | 'units'
  | 'credits'
  | 'unitCost'
  | 'available'
  | 'malformed'
  | 'duplicates'
  | 'excluded'
  | 'suppressed'
  | 'missingFields'
  | 'unrenderable'

export type Change =
  | { field: NumberField; from: number; to: number }
  | { field: 'unknownChoice'; from: UnknownChoice | null; to: UnknownChoice | null }

const NUMBER_FIELDS: ReadonlyArray<readonly [NumberField, (p: Preview) => number]> = [
  ['rows', (p) => p.rows],
  ['units', (p) => p.units],
  ['credits', (p) => p.credits],
  ['unitCost', (p) => p.unit_cost],
  ['available', (p) => p.available],
  ['malformed', (p) => p.malformed],
  ['duplicates', (p) => p.duplicates],
  ['excluded', (p) => p.excluded],
  ['suppressed', (p) => p.suppressed],
  ['missingFields', (p) => p.missing_fields],
  ['unrenderable', (p) => p.unrenderable],
]

/** The figures of the two previews that are not the same. An empty list means the difference is not one a screen can show (a price rule, a row's value). */
export function previewChanges(before: Preview, after: Preview): Change[] {
  const out: Change[] = []
  for (const [field, read] of NUMBER_FIELDS) {
    const from = read(before)
    const to = read(after)
    if (from !== to) out.push({ field, from, to })
  }
  if (before.unknown_choice !== after.unknown_choice) out.push({ field: 'unknownChoice', from: before.unknown_choice, to: after.unknown_choice })
  return out
}

const NUMBER_KEYS: Record<NumberField, CopyKey> = {
  rows: 'newjob.changed.rows',
  units: 'newjob.changed.units',
  credits: 'newjob.changed.credits',
  unitCost: 'newjob.changed.unitCost',
  available: 'newjob.changed.available',
  malformed: 'newjob.changed.malformed',
  duplicates: 'newjob.changed.duplicates',
  excluded: 'newjob.changed.excluded',
  suppressed: 'newjob.changed.suppressed',
  missingFields: 'newjob.changed.missingFields',
  unrenderable: 'newjob.changed.unrenderable',
}

function choiceWord(c: UnknownChoice | null): string {
  return c === 'include' ? t('newjob.changed.choiceInclude') : c === 'exclude' ? t('newjob.changed.choiceExclude') : t('newjob.changed.choiceNone')
}

/** One sentence per change, in the order of the preview. */
export function describeChanges(changes: readonly Change[]): string[] {
  return changes.map((c) =>
    c.field === 'unknownChoice'
      ? t('newjob.changed.unknownChoice', { from: choiceWord(c.from), to: choiceWord(c.to) })
      : t(NUMBER_KEYS[c.field], { from: fmt(c.from), to: fmt(c.to) }),
  )
}
