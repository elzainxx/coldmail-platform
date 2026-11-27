// The list as a CSV file, from choosing it to being ready: the file picker, then what reading the start of the file found (rows, columns), the column
// picker, and everything about the file the platform would refuse, in words. The same for a verification and for a send.
import { t } from '../../copy/index.ts'
import { Button, Spinner } from '../../ui/index.ts'
import { ColumnPicker } from './ColumnPicker.tsx'
import type { CsvProblem } from './csv/columns.ts'
import { FilePicker } from './FilePicker.tsx'
import { fmt } from './schedule.ts'
import type { CsvSource } from './useCsvSource.ts'

export function problemText(p: CsvProblem): string {
  switch (p.kind) {
    case 'empty':
      return t('newjob.problem.empty')
    case 'noRows':
      return t('newjob.problem.noRows')
    case 'tooLarge':
      return t('newjob.problem.tooLarge')
    case 'tooManyRows':
      return t('newjob.problem.tooManyRows', { count: fmt(p.count) })
    case 'unnamedColumn':
      return t('newjob.problem.unnamedColumn', { position: p.position })
    case 'repeatedColumn':
      return t('newjob.problem.repeatedColumn', { name: p.name })
    case 'headerTooLong':
      return t('newjob.problem.headerTooLong')
    case 'raggedRows':
      return p.line === null
        ? t('newjob.problem.raggedRowsAnywhere', { count: fmt(p.count) })
        : t('newjob.problem.raggedRows', { count: fmt(p.count), line: fmt(p.line) })
    case 'unclosedQuote':
      return t('newjob.problem.unclosedQuote')
    case 'addressTwice':
      return t('newjob.problem.addressTwice', { name: p.name })
    case 'reservedColumn':
      return t('newjob.problem.reservedColumn', { name: p.name })
  }
}

export function formatSize(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`
  return `${bytes} bytes`
}

export interface CsvStepProps {
  source: CsvSource
  purpose: 'verify' | 'send'
  /** the list is with the platform now (a draft was made from it): it is shown, not changed; the page offers its own way back */
  locked?: boolean
}

export function CsvStep({ source, purpose, locked = false }: CsvStepProps) {
  const { file, head } = source
  if (file === null) return <FilePicker onFile={source.pick} />
  if (source.reading) return <Spinner label={t('newjob.csv.reading')} />

  const change = locked ? null : (
    <Button size="sm" onClick={source.clear}>
      {t('newjob.csv.change')}
    </Button>
  )

  if (source.readFailed || head === null) {
    return (
      <div className="flex flex-col items-start gap-3">
        <p role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
          {t('newjob.csv.readFailed')}
        </p>
        {change}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border bg-sunken px-3 py-2">
        <div className="min-w-0">
          <p className="truncate text-base font-medium text-fg">{t('newjob.csv.chosen', { name: file.name, size: formatSize(file.size) })}</p>
          <p className="text-sm text-fg-muted">{t('newjob.csv.found', { rows: fmt(head.totalRows), columns: fmt(head.header.length) })}</p>
        </div>
        {change}
      </div>

      {source.problems.length > 0 && (
        <div role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
          <p className="font-medium">{t('newjob.problem.title')}</p>
          <ul className="mt-1 list-disc pl-5">
            {source.problems.map((p, i) => (
              <li key={`${p.kind}-${i}`}>{problemText(p)}</li>
            ))}
          </ul>
        </div>
      )}

      {head.header.length > 0 && <ColumnPicker head={head} chosen={source.chosen} onChoose={source.choose} purpose={purpose} />}
    </div>
  )
}
