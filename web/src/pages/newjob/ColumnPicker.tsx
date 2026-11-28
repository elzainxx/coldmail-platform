// Which column of the file is the address, and what the other columns will be: the columns of the file as the platform will read them. The most likely column is
// already chosen (csv/columns.ts suggestEmailColumn); the user can choose another. Every other column is listed as the merge field it becomes.
import { useId } from 'react'
import { t } from '../../copy/index.ts'
import { FormField, Select, Table, type Column } from '../../ui/index.ts'
import { columnLabel, mergeColumns } from './csv/columns.ts'
import type { CsvHead } from './csv/reader.ts'
import { FieldChips } from './FieldChips.tsx'

export interface ColumnPickerProps {
  head: CsvHead
  chosen: number | null
  onChoose: (index: number | null) => void
  /** a send job names the merge fields in its message; a verification only keeps them */
  purpose: 'verify' | 'send'
}

const SAMPLE_SHOWN = 5

export function ColumnPicker({ head, chosen, onChoose, purpose }: ColumnPickerProps) {
  const listId = useId()
  const unnamed = (position: number) => t('newjob.columns.unnamed', { position })
  const merge = mergeColumns(head.header, chosen)

  const sample = head.rows.slice(0, SAMPLE_SHOWN).map((cells, i) => ({ i, cells }))
  const columns: Column<{ i: number; cells: string[] }>[] = head.header.map((raw, index) => ({
    key: String(index),
    header: index === chosen ? `${columnLabel(raw, index, unnamed)} (${t('newjob.columns.addressMark')})` : columnLabel(raw, index, unnamed),
    cell: (row) => row.cells[index] ?? '',
  }))

  return (
    <div className="flex flex-col gap-4">
      <FormField label={t('newjob.columns.address')} hint={t('newjob.columns.addressHint')} required>
        <Select value={chosen === null ? '' : String(chosen)} onChange={(e) => onChoose(e.target.value === '' ? null : Number(e.target.value))}>
          {chosen === null && <option value="">{t('newjob.columns.choose')}</option>}
          {head.header.map((raw, index) => (
            <option key={index} value={String(index)}>
              {columnLabel(raw, index, unnamed)}
            </option>
          ))}
        </Select>
      </FormField>

      <section aria-labelledby={`${listId}-title`}>
        <h3 id={`${listId}-title`} className="text-base font-semibold text-fg">
          {purpose === 'send' ? t('newjob.columns.mergeTitleSend') : t('newjob.columns.mergeTitleVerify')}
        </h3>
        <p className="text-sm text-fg-muted">{purpose === 'send' ? t('newjob.columns.mergeHintSend') : t('newjob.columns.mergeHintVerify')}</p>
        {chosen === null ? null : merge.length === 0 ? (
          <p className="mt-1 text-sm text-fg-muted">{t('newjob.columns.none')}</p>
        ) : (
          <FieldChips
            columns={merge}
            label={purpose === 'send' ? t('newjob.columns.mergeTitleSend') : t('newjob.columns.mergeTitleVerify')}
            unnamed={unnamed}
            flagUnusable={purpose === 'send'}
          />
        )}
      </section>

      {sample.length > 0 && <Table caption={t('newjob.csv.sampleCaption')} columns={columns} rows={sample} rowKey={(r) => String(r.i)} maxHeightClass="max-h-64" />}
    </div>
  )
}
