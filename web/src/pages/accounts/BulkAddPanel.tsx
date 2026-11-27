// Adding many accounts: pasted lines or a CSV file, one request, a table of what became of each line.
//
// THE PASSWORDS. Every line holds one. The lines are in an input that is not controlled (no state holds them), they go in the one request, and the input is emptied
// as soon as the API has answered with a result for each line. The result table shows the line number and a word, never a cell of the file. When the request as a
// whole is refused (a line that is not valid, the service unavailable) nothing was added and the text stays, so that a list of hundreds is not lost to a typing mistake.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { m3Api } from '../../api/endpoints.ts'
import { isAbortError, isApiError } from '../../api/errors.ts'
import type { M3BulkResponse } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Button, Card, FormField, StatusBadge, Table, TextArea, TextInput, type Column } from '../../ui/index.ts'
import { apiBlob, apiText, hasAccountLines, screenHeader } from './bulkFile.ts'
import { ErrorNote } from './ErrorNote.tsx'
import { badLineText, bulkAdded, bulkDetail, bulkLabel, bulkToken, failureOf, type Failure } from './words.ts'

type Line = M3BulkResponse['results'][number]

export interface BulkAddPanelProps {
  /** a request was answered (some accounts may exist now): read the list again */
  onChanged: () => void
  onCancel: () => void
}

function emptyLists(form: HTMLFormElement): void {
  form.reset() // the pasted lines go back to the header alone, the file is let go
}

export function BulkAddPanel({ onChanged, onCancel }: BulkAddPanelProps) {
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<Failure | string | null>(null)
  const [results, setResults] = useState<Line[] | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    if (busy) return
    const form = e.currentTarget
    const pasted = new FormData(form).get('lines')
    const fileInput = form.elements.namedItem('file')
    const file = fileInput instanceof HTMLInputElement ? fileInput.files?.[0] : undefined

    let body: Blob | null = null
    if (file !== undefined && file.size > 0) {
      body = await apiBlob(file)
    } else if (typeof pasted === 'string' && hasAccountLines(pasted)) {
      body = new Blob([apiText(pasted)], { type: 'text/csv' })
    }
    setResults(null)
    if (body === null) {
      setProblem(t('accounts.bulk.nothing'))
      return
    }
    setProblem(null)

    setBusy(true)
    try {
      const res = await m3Api.bulkCsv(body)
      emptyLists(form)
      if (!mounted.current) return
      setResults(res.results)
      onChanged()
    } catch (err) {
      if (isAbortError(err)) return
      // a batch the API could not read (a line with too few values, a column it does not know) or refused at a line: say which, in words
      const unreadable = isApiError(err) && err.code === 'validation_failed'
      const line = isApiError(err) ? badLineText(err) : null
      if (mounted.current) setProblem(line ?? (unreadable ? t('accounts.bulk.badFile') : failureOf(err)))
      if (isApiError(err) && err.code === 'account_outcome_unknown') onChanged()
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  return (
    <Card title={t('accounts.bulk.title')}>
      <form method="post" onSubmit={(e) => void submit(e)} noValidate autoComplete="off" className="flex max-w-3xl flex-col gap-3">
        <p className="text-base text-fg-muted">{t('accounts.bulk.intro')}</p>
        {typeof problem === 'string' && (
          <p role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
            {problem}
          </p>
        )}
        {problem !== null && typeof problem !== 'string' && <ErrorNote failure={problem} />}

        <FormField label={t('accounts.bulk.lines')} hint={t('accounts.bulk.columns')}>
          <TextArea name="lines" rows={8} mono defaultValue={`${screenHeader()}\n`} autoComplete="off" autoCapitalize="none" spellCheck={false} wrap="off" />
        </FormField>
        <FormField label={t('accounts.bulk.file')} hint={t('accounts.bulk.fileHint')}>
          <TextInput name="file" type="file" accept=".csv,text/csv" />
        </FormField>

        {busy && (
          <p role="status" className="text-sm text-fg-muted">
            {t('accounts.bulk.working')}
          </p>
        )}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" variant="primary" busy={busy}>
            {t('accounts.bulk.submit')}
          </Button>
          <Button onClick={onCancel} disabled={busy}>
            {t('common.close')}
          </Button>
        </div>
      </form>
      {results !== null && <BulkResults results={results} />}
    </Card>
  )
}

function BulkResults({ results }: { results: Line[] }) {
  const added = results.filter((r) => bulkAdded(r.code)).length
  const columns: Column<Line>[] = [
    { key: 'line', header: t('accounts.bulk.colLine'), rowHeader: true, cell: (r) => r.line },
    { key: 'result', header: t('accounts.bulk.colResult'), cell: (r) => <StatusBadge status={bulkToken(r.code)} label={bulkLabel(r.code)} /> },
    { key: 'details', header: t('accounts.bulk.colDetails'), cell: (r) => bulkDetail(r.code) ?? '' },
  ]
  return (
    <div className="mt-4 flex flex-col gap-2">
      <div role="status">
        <p className="text-base font-medium text-fg">{t('accounts.bulk.summary', { added, failed: results.length - added })}</p>
        <p className="text-sm text-fg-muted">{t('accounts.bulk.summaryCheck')}</p>
      </div>
      <Table caption={t('accounts.bulk.caption')} columns={columns} rows={results} rowKey={(r) => String(r.line)} maxHeightClass="max-h-96" />
    </div>
  )
}
