// /jobs/:id: one job. The header, what is happening now, the controls its state allows, the results (with the CSV) and its history in words.
// The job is asked for again every 3 seconds while its items move, every 15 while it waits (api/poll.ts stops all of it with the tab), and a finished job is read once.
import type { ReactNode } from 'react'
import { jobsApi } from '../../api/endpoints.ts'
import { usePolling } from '../../api/poll.ts'
import type { Job } from '../../api/types.ts'
import { maybeT, t } from '../../copy/index.ts'
import { jobStatusText } from '../../copy/words.ts'
import { Link, pathFor, useRoute } from '../../router.tsx'
import { buttonClass, Card, PageHeader, Spinner, StatusBadge, jobStatusToken } from '../../ui/index.ts'
import { ErrorNotice } from './ErrorNotice.tsx'
import { addressCountText, continuePath, creditsText, daysBetween, daysText, formatDateTime, isLive, isNotStarted, isWorking, jobName, kindText } from './format.ts'
import { JobControlsBar } from './JobControlsBar.tsx'
import { JobHistory } from './JobHistory.tsx'
import { JobProgress } from './JobProgress.tsx'
import { ResultsSection } from './ResultsSection.tsx'
import { IDLE_POLL_MS, LIVE_POLL_MS, useRemote, type Remote } from './useRemote.ts'

/** The sentence of "what is happening now": by the job's status, its kind and where a pause began. */
export function nowText(job: Job): string {
  switch (job.status) {
    case 'scheduled': {
      const date = formatDateTime(job.scheduled_start)
      return date === '' ? t('jobs.now.scheduled.noDate') : t('jobs.now.scheduled', { date })
    }
    case 'running':
      return job.module === 'm1' ? t('jobs.now.running.m1') : t('jobs.now.running.send')
    case 'paused':
      return job.paused_from === 'scheduled' ? t('jobs.now.paused.scheduled') : t('jobs.now.paused.running')
    default:
      return maybeT(`jobs.now.${job.status}`) ?? t('jobs.now.other')
  }
}

/** The one calm sentence that says why the template and the extra columns are gone; null while they are still kept. */
export function clearedText(job: Job): string | null {
  if (job.content_cleared_at === null || job.content_cleared_at === undefined) return null
  const after = daysText(daysBetween(job.finished_at, job.content_cleared_at))
  return job.module === 'm1' ? t('jobs.cleared.verify', { after }) : t('jobs.cleared.send', { after })
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5 sm:flex-row sm:gap-4">
      <dt className="text-fg-muted sm:w-48 sm:shrink-0">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  )
}

function Dated({ iso }: { iso: string | null }) {
  return iso === null ? null : <time dateTime={iso}>{formatDateTime(iso)}</time>
}

function Summary({ job, remote }: { job: Job; remote: Remote<Job> }) {
  const cleared = clearedText(job)
  const subject = job.content?.subject
  const showCredits = isLive(job.status) && job.preview !== null && job.preview.metered && job.preview.credits > 0
  return (
    <Card title={t('jobs.detail.summary')}>
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-2">
          <span data-testid={`job-status-${job.id}`} className="self-start">
            <StatusBadge status={jobStatusToken(job.status)} label={jobStatusText(job.status)} />
          </span>
          <p aria-live="polite">{nowText(job)}</p>
          <JobProgress job={job} />
        </div>
        {isNotStarted(job.status) ? (
          <div className="flex flex-col items-start gap-2">
            <p className="font-medium">{t('jobs.continue.title')}</p>
            <p className="text-fg-muted">{t('jobs.continue.body')}</p>
            <Link to={continuePath(job)} className={buttonClass('primary')}>
              {t('jobs.continue.link')}
            </Link>
          </div>
        ) : (
          <JobControlsBar job={job} remote={remote} />
        )}
        <dl className="flex flex-col gap-1.5 text-base">
          <Field label={t('jobs.field.kind')}>{kindText(job.module)}</Field>
          <Field label={t('jobs.field.addresses')}>{addressCountText(job)}</Field>
          {showCredits && <Field label={t('jobs.field.credits')}>{creditsText(job.preview?.credits ?? 0)}</Field>}
          {typeof subject === 'string' && subject !== '' && cleared === null && <Field label={t('jobs.field.subject')}>{subject}</Field>}
          {job.source_job_id !== null && (
            <Field label={t('jobs.field.source')}>
              <Link to={pathFor('/jobs/:id', { id: job.source_job_id })}>{t('jobs.field.sourceLink')}</Link>
            </Field>
          )}
          <Field label={t('jobs.field.created')}>
            <Dated iso={job.created_at} />
          </Field>
          {job.scheduled_start !== null && job.started_at === null && (
            <Field label={t('jobs.field.scheduled')}>
              <Dated iso={job.scheduled_start} />
            </Field>
          )}
          {job.started_at !== null && (
            <Field label={t('jobs.field.started')}>
              <Dated iso={job.started_at} />
            </Field>
          )}
          {job.finished_at !== null && (
            <Field label={t('jobs.field.finished')}>
              <Dated iso={job.finished_at} />
            </Field>
          )}
        </dl>
        {cleared !== null && <p className="text-sm text-fg-muted">{cleared}</p>}
      </div>
    </Card>
  )
}

function JobBody({ job, remote }: { job: Job; remote: Remote<Job> }) {
  return (
    <>
      <Summary job={job} remote={remote} />
      {!isNotStarted(job.status) && <ResultsSection job={job} />}
      <JobHistory job={job} />
    </>
  )
}

function JobDetail({ id }: { id: string }) {
  const remote = useRemote(async (signal) => (await jobsApi.get(id, { signal })).job, [id])
  const job = remote.data
  // 3 seconds while its items move, 15 while it waits (for its start, or to be resumed), and not at all once it is over or was never started
  usePolling((signal) => remote.refresh(signal), job !== undefined && isWorking(job.status) ? LIVE_POLL_MS : IDLE_POLL_MS, { enabled: job !== undefined && isLive(job.status) })

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={t('jobs.detail.title')}
        description={job === undefined ? undefined : jobName(job)}
        actions={
          <Link to="/jobs" className={buttonClass('secondary')}>
            {t('jobs.detail.back')}
          </Link>
        }
      />
      {remote.error !== null && <ErrorNotice error={remote.error} onRetry={remote.retry} />}
      {job === undefined && remote.error === null && <Spinner />}
      {job !== undefined && <JobBody job={job} remote={remote} />}
    </div>
  )
}

export function JobDetailPage() {
  const { params } = useRoute()
  const id = params['id'] ?? ''
  // keyed by the id: another job's page starts from nothing, whatever the router reused
  return <JobDetail key={id} id={id} />
}
