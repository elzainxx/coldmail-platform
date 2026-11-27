// How far a running job has got. The API has no total of a job's results, so the count is made by walking the results (jobData.countResults, at most COUNT_PAGES_MAX
// pages): exact for a job whose results fit, and for a larger one the screen says it cannot show the progress instead of showing a number that never moves.
import { usePolling } from '../../api/poll.ts'
import type { Job } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { ProgressBar } from '../../ui/index.ts'
import { formatNumber, isLive } from './format.ts'
import { countResults } from './jobData.ts'
import { useRemote } from './useRemote.ts'

export const PROGRESS_POLL_MS = 10_000

/** The statuses in which the items are being worked on: a paused or a not yet started job has nothing moving to show. */
const WORKING = new Set<string>(['running', 'cancelling', 'completing'])

export function JobProgress({ job }: { job: Job }) {
  const working = WORKING.has(job.status)
  const count = useRemote((signal) => (working ? countResults(job.id, signal) : Promise.resolve(null)), [job.id, job.status])
  const answer = count.data
  const capped = answer !== undefined && answer !== null && !answer.complete
  usePolling((signal) => count.refresh(signal), PROGRESS_POLL_MS, { enabled: working && isLive(job.status) && !capped })

  if (!working || answer === undefined || answer === null) return null
  if (!answer.complete) return <p className="text-sm text-fg-muted">{t('jobs.progress.large')}</p>

  const total = job.preview?.rows ?? 0
  if (total <= 0) return <p className="text-sm text-fg-muted">{t('jobs.progress.done', { done: formatNumber(answer.count) })}</p>
  return (
    <div className="flex flex-col gap-1">
      <ProgressBar value={answer.count} max={total} label={t('jobs.progress.label')} showValue />
      <p className="text-sm text-fg-muted">{t('jobs.progress.value', { done: formatNumber(answer.count), total: formatNumber(total) })}</p>
    </div>
  )
}
