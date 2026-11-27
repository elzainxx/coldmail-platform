// Import a CSV of addresses into the suppression list. The file is handed to the request as it is (the browser streams it from disk); the answer is counts and, for the
// first 20 lines that were not valid, a line number and a word. No address is ever shown back.
import { useState, type FormEvent } from 'react'
import { suppressionsApi } from '../../api/endpoints.ts'
import { isAbortError, isApiError } from '../../api/errors.ts'
import type { SuppressionImportInvalid, SuppressionImportResult } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { malformedReasonText } from '../../copy/words.ts'
import { Button, Card, FormField, Table, TextInput, type Column } from '../../ui/index.ts'
import { FailureNote, failureOf, type Failure } from './Failure.tsx'

export interface ImportPanelProps {
  /** the file was imported (some addresses may be new): read the list again */
  onImported: () => void
}

export function ImportPanel({ onImported }: ImportPanelProps) {
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<string | Failure | null>(null)
  const [result, setResult] = useState<SuppressionImportResult | null>(null)

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    if (busy) return
    const form = e.currentTarget
    const fileInput = form.elements.namedItem('file')
    const file = fileInput instanceof HTMLInputElement ? fileInput.files?.[0] : undefined
    setResult(null)
    if (file === undefined || file.size === 0) {
      setProblem(t('suppressions.import.nothing'))
      return
    }
    setProblem(null)
    setBusy(true)
    try {
      const res = await suppressionsApi.importCsv(file)
      form.reset()
      setResult(res)
      onImported()
    } catch (err) {
      if (isAbortError(err)) return
      // the one 400 of an import is a header that cannot be used (no email column, or two)
      setProblem(isApiError(err) && err.code === 'validation_failed' ? t('suppressions.import.badFile') : failureOf(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card title={t('suppressions.import.title')}>
      <form method="post" onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-3">
        {typeof problem === 'string' && (
          <p role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
            {problem}
          </p>
        )}
        {problem !== null && typeof problem !== 'string' && <FailureNote failure={problem} />}
        <FormField label={t('suppressions.import.label')} hint={t('suppressions.import.hint')}>
          <TextInput name="file" type="file" accept=".csv,text/csv" />
        </FormField>
        <div>
          <Button type="submit" variant="primary" busy={busy}>
            {t('suppressions.import.submit')}
          </Button>
        </div>
      </form>
      {result !== null && <ImportResult result={result} />}
    </Card>
  )
}

function ImportResult({ result }: { result: SuppressionImportResult }) {
  const columns: Column<SuppressionImportInvalid>[] = [
    { key: 'line', header: t('suppressions.import.sampleLine'), rowHeader: true, cell: (r) => r.line },
    { key: 'why', header: t('suppressions.import.sampleWhy'), cell: (r) => malformedReasonText(r.reason) },
  ]
  const shown = result.invalid_sample.length
  return (
    <div className="mt-4 flex flex-col gap-2">
      <div role="status">
        <p className="text-base font-medium text-fg">{t('suppressions.import.summary', { added: result.added, already: result.already_there, invalid: result.invalid })}</p>
        {result.invalid > 0 && (
          <p className="text-sm text-fg-muted">{shown < result.invalid ? t('suppressions.import.sampleNote', { shown, invalid: result.invalid }) : t('suppressions.import.sampleAll')}</p>
        )}
      </div>
      {shown > 0 && <Table caption={t('suppressions.import.sampleCaption')} columns={columns} rows={result.invalid_sample} rowKey={(r) => String(r.line)} maxHeightClass="max-h-72" />}
    </div>
  )
}
