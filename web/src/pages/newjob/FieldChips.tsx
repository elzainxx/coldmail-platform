// The merge fields of a list as chips: the other columns of a CSV file, or the fields of a finished verification. The same look on both paths.
import { t } from '../../copy/index.ts'
import type { MergeColumn } from './csv/columns.ts'

export interface FieldChipsProps {
  columns: readonly MergeColumn[]
  /** what the list is: its accessible name */
  label: string
  /** the word for a field with no name (a CSV column): given only where that can happen */
  unnamed?: (position: number) => string
  /** say of a field a message cannot name that it cannot be used (a send) */
  flagUnusable: boolean
}

export function FieldChips({ columns, label, unnamed, flagUnusable }: FieldChipsProps) {
  return (
    <ul className="mt-1 flex flex-wrap gap-2" aria-label={label}>
      {columns.map((c) => (
        <li key={c.index} className="rounded-md border border-border bg-sunken px-2 py-0.5 text-sm text-fg">
          <span className="font-mono">{c.name === '' && unnamed !== undefined ? unnamed(c.index + 1) : c.name}</span>
          {flagUnusable && !c.usable && c.name !== '' && <span className="ml-2 text-xs text-fg-muted">{t('newjob.columns.unusable')}</span>}
        </li>
      ))}
    </ul>
  )
}
