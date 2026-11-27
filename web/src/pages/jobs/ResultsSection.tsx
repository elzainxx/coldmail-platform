// The results of a job, a page at a time, and the CSV of all of them. While the job runs the page asks again every 3 seconds (partial results); the filter narrows the
// page that is loaded, the way a spreadsheet filter would (the API has no search of a job's results).
import { useRef, useState } from 'react'
import { jobsApi } from '../../api/endpoints.ts'
import { saveBlob } from '../../api/download.ts'
import type { ApiError } from '../../api/errors.ts'
import { isAbortError } from '../../api/errors.ts'
import { usePolling } from '../../api/poll.ts'
import type { Job, JobResult } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { resultStateText } from '../../copy/words.ts'
import { Button, Card, EmptyState, FormField, Pagination, Select, StatusBadge, Table, resultStateToken, useCursorPager, useToast, type Column } from '../../ui/index.ts'
import { ErrorNotice } from './ErrorNotice.tsx'
import { csvFileName, isLive, isWorking } from './format.ts'
import { costText, detailLines, outcomesOf } from './jobData.ts'
import { LIVE_POLL_MS, asApiError, useRemote } from './useRemote.ts'

/** What the file holds: after the content was cleared, the address and the outcome only (and what is gone is named for the kind of job). */
export function csvNote(job: Pick<Job, 'module' | 'content_cleared_at'>): string {
  if (job.content_cleared_at === null || job.content_cleared_at === undefined) return t('jobs.csv.note')
  return job.module === 'm1' ? t('jobs.csv.noteCleared.verify') : t('jobs.csv.noteCleared.send')
}

const columns: readonly Column<JobResult>[] = [
  { key: 'address', header: t('jobs.results.address'), rowHeader: true, cell: (r) => <span className="break-all">{r.email}</span> },
  { key: 'outcome', header: t('jobs.results.outcome'), cell: (r) => <StatusBadge status={resultStateToken(r.state)} label={resultStateText(r.state)} /> },
  { key: 'cost', header: t('jobs.results.cost'), cell: (r) => costText(r) },
  {
    key: 'details',
    header: t('jobs.results.details'),
    cell: (r) => {
      const lines = detailLines(r)
      return lines.length === 0 ? null : (
        <span className="flex flex-col gap-0.5 text-fg-muted">
          {lines.map((line) => (
            <span key={line}>{line}</span>
          ))}
        </span>
      )
    },
  },
]

export function ResultsSection({ job }: { job: Job }) {
  const toast = useToast()
  const pager = useCursorPager()
  const live = isLive(job.status)
  // Read again when the job's status changes (the last results land as it finishes) and, while its items move, every few seconds.
  const page = useRemote((signal) => jobsApi.results(job.id, { cursor: pager.cursor }, { signal }), [job.id, pager.cursor, job.status])
  usePolling((signal) => page.refresh(signal), LIVE_POLL_MS, { enabled: isWorking(job.status) })

  const [filter, setFilter] = useState('')
  const [downloading, setDownloading] = useState(false)
  const [downloadError, setDownloadError] = useState<ApiError | null>(null)
  const downloadRunning = useRef(false)

  const results = page.data?.results ?? []
  const outcomes = outcomesOf(results)
  const chosen = outcomes.includes(filter) ? filter : '' // a choice that is not on this page is "all"
  const shown = chosen === '' ? results : results.filter((r) => r.state === chosen)
  const hasNext = page.data?.next_cursor != null

  const download = async () => {
    if (downloadRunning.current) return
    downloadRunning.current = true
    setDownloading(true)
    setDownloadError(null)
    try {
      // Through the API client, so the request carries the Authorization header; the file is then handed to the browser as a download.
      saveBlob(await jobsApi.resultsCsv(job.id), csvFileName(job))
      toast.notify(t('jobs.csv.ready'), { tone: 'success' })
    } catch (e) {
      if (!isAbortError(e)) setDownloadError(asApiError(e))
    } finally {
      downloadRunning.current = false
      setDownloading(false)
    }
  }

  const emptyBody =
    results.length > 0 && shown.length === 0 ? (
      <EmptyState title={t('jobs.results.empty.title')} description={t('jobs.results.filter.empty')} />
    ) : (
      <EmptyState title={t('jobs.results.empty.title')} description={live ? t('jobs.results.empty.live') : t('jobs.results.empty.none')} />
    )

  return (
    <Card
      title={t('jobs.results.title')}
      actions={
        <Button onClick={() => void download()} busy={downloading}>
          {t('jobs.csv.button')}
        </Button>
      }
    >
      <div className="flex flex-col gap-3">
        <p className="text-sm text-fg-muted">{csvNote(job)}</p>
        {downloadError !== null && <ErrorNotice error={downloadError} />}
        {live && <p className="text-sm text-fg-muted">{t('jobs.results.partial')}</p>}
        {page.error !== null ? (
          <ErrorNotice error={page.error} onRetry={page.retry} />
        ) : (
          <>
            {results.length > 0 && (
              <FormField label={t('jobs.results.filter.label')} hint={t('jobs.results.filter.hint')} className="max-w-xs">
                <Select value={chosen} onChange={(e) => setFilter(e.target.value)}>
                  <option value="">{t('jobs.results.filter.all')}</option>
                  {outcomes.map((state) => (
                    <option key={state} value={state}>
                      {resultStateText(state)}
                    </option>
                  ))}
                </Select>
              </FormField>
            )}
            <Table
              caption={t('jobs.results.caption')}
              captionHidden
              columns={columns}
              rows={shown}
              rowKey={(r) => String(r.ordinal)}
              rowTestId={(r) => `results-row-${r.ordinal}`}
              loading={page.loading && page.data === undefined}
              empty={emptyBody}
              maxHeightClass="max-h-[60vh]"
            />
            {(pager.hasPrevious || hasNext) && (
              <Pagination
                page={pager.page}
                hasPrevious={pager.hasPrevious}
                hasNext={hasNext}
                busy={page.loading}
                onPrevious={() => {
                  setFilter('')
                  pager.goPrevious()
                }}
                onNext={() => {
                  setFilter('')
                  pager.goNext(page.data?.next_cursor ?? null)
                }}
              />
            )}
          </>
        )}
      </div>
    </Card>
  )
}
