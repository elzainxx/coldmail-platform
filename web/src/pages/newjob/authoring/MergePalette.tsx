// The merge-field palette: one button for each column of the list the message can name (and the two fields the platform always supplies), each inserting
// {{name}} or {{name|fallback}} where the user is typing. A column whose name a message cannot use (a space, a dash) is listed but cannot be inserted, and says why.
import { useState } from 'react'
import { t } from '../../../copy/index.ts'
import { Button, FormField, TextInput } from '../../../ui/index.ts'
import { RESERVED_FIELDS, type MergeColumn } from '../csv/columns.ts'
import { fieldSnippet } from './text.ts'

export interface MergePaletteProps {
  /** the columns of the list other than the address (none for a send to a finished verification) */
  columns: readonly MergeColumn[]
  /** where a field goes right now, in words ("HTML body") */
  target: string
  /** insert this text where the user is typing */
  onInsert: (snippet: string) => void
  /** one line about where the fields come from (a send to a finished verification) */
  note?: string
}

export function MergePalette({ columns, target, onInsert, note }: MergePaletteProps) {
  const [fallback, setFallback] = useState('')
  const usable = columns.filter((c) => c.usable)
  const unusable = columns.filter((c) => !c.usable && c.name !== '')

  return (
    <div className="flex flex-col gap-3 rounded-md border border-border bg-sunken p-3">
      <div>
        <h3 className="text-base font-semibold text-fg">{t('newjob.authoring.paletteTitle')}</h3>
        <p className="text-sm text-fg-muted">{t('newjob.authoring.paletteHint', { target })}</p>
        {note !== undefined && <p className="text-sm text-fg-muted">{note}</p>}
      </div>

      <FormField label={t('newjob.authoring.fallback')} hint={t('newjob.authoring.fallbackHint')} optional>
        <TextInput value={fallback} onChange={(e) => setFallback(e.target.value)} autoComplete="off" />
      </FormField>

      {usable.length > 0 && (
        <ul className="m-0 flex list-none flex-wrap gap-2 p-0" aria-label={t('newjob.authoring.columnsFields')}>
          {usable.map((c) => (
            <li key={c.index}>
              <Button size="sm" aria-label={t('newjob.authoring.insertField', { name: c.name })} onClick={() => onInsert(fieldSnippet(c.name, fallback))}>
                <span className="font-mono">{c.name}</span>
              </Button>
            </li>
          ))}
        </ul>
      )}

      <ul className="m-0 flex list-none flex-wrap gap-2 p-0" aria-label={t('newjob.authoring.reservedFields')}>
        {RESERVED_FIELDS.map((name) => (
          <li key={name}>
            <Button size="sm" aria-label={t('newjob.authoring.insertField', { name })} onClick={() => onInsert(fieldSnippet(name, ''))}>
              <span className="font-mono">{name}</span>
            </Button>
          </li>
        ))}
      </ul>
      <p className="text-xs text-fg-muted">{t('newjob.authoring.reservedHint')}</p>

      {unusable.length > 0 && (
        <p className="text-xs text-fg-muted">
          {t('newjob.authoring.unusable')} <span className="font-mono">{unusable.map((c) => c.name).join(', ')}</span>
        </p>
      )}
    </div>
  )
}
