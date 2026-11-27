// /: where a signed-in user lands. What they have to spend (a balance per kind of job; their own SMTP accounts are a switch, not a balance), what is running, and what finished
// last. Each part loads on its own, so one that fails says so and the others still show. Everything is asked again every 10 seconds while the tab is visible.
import { balancesApi, jobsApi } from '../../api/endpoints.ts'
import { usePolling } from '../../api/poll.ts'
import type { Balances, Job, M3Entitlement, ModuleBalance } from '../../api/types.ts'
import { t, type CopyKey } from '../../copy/index.ts'
import { jobStatusText } from '../../copy/words.ts'
import { addressCountText, creditsText, formatDateTime, isFinal, isLive, jobName } from '../jobs/format.ts'
import { ErrorNotice } from '../jobs/ErrorNotice.tsx'
import { useRemote } from '../jobs/useRemote.ts'
import { Link, pathFor } from '../../router.tsx'
import { buttonClass, Card, EmptyState, PageHeader, Spinner, StatusBadge, jobStatusToken, type StatusToken } from '../../ui/index.ts'

export const DASHBOARD_POLL_MS = 10_000

/** How many finished jobs the "Recent results" part lists. */
export const RECENT_COUNT = 5
/** How many of the newest jobs are read to find the finished ones and the ones about to start or finish (the API has no filter for a finished job). */
const RECENT_WINDOW = 50

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// your own SMTP: what the entitlement says, in words
// ---------------------------------------------------------------------------------------------------------------------------------------------------

export interface M3Summary {
  state: 'on' | 'off' | 'suspended'
  token: StatusToken
  /** the sentences under the badge, in order */
  lines: string[]
}

/**
 * Your own SMTP accounts in plain words: switched off (never switched on, or its end has passed), suspended, or switched on until a date (or with no end).
 * Switched off wins over suspended (an entitlement that is not on has nothing to suspend); a state the platform has not observed is said, never guessed.
 */
export function m3Summary(m3: M3Entitlement, now: number = Date.now()): M3Summary {
  const untilTime = m3.until === null ? Number.NaN : new Date(m3.until).getTime()
  const until = Number.isNaN(untilTime) ? null : formatDateTime(m3.until)
  if (!m3.enabled) {
    const ended = until !== null && untilTime <= now
    return { state: 'off', token: 'unknown', lines: [ended ? t('dashboard.m3.ended', { date: until }) : t('dashboard.m3.disabled')] }
  }
  if (m3.state === 'suspended') return { state: 'suspended', token: 'failed', lines: [t('dashboard.m3.suspended')] }
  const lines = [until === null ? t('dashboard.m3.enabled') : t('dashboard.m3.enabledUntil', { date: until })]
  if (m3.state === 'unknown') lines.push(t('dashboard.m3.unconfirmed'))
  return { state: 'on', token: 'valid', lines }
}

const M3_STATE_WORD: Record<M3Summary['state'], CopyKey> = {
  on: 'dashboard.m3.state.on',
  off: 'dashboard.m3.state.off',
  suspended: 'dashboard.m3.state.suspended',
}

function CreditsBalance({ module, balance }: { module: 'm1' | 'm2'; balance: ModuleBalance }) {
  const title: CopyKey = module === 'm1' ? 'dashboard.balance.m1' : 'dashboard.balance.m2'
  return (
    <div data-testid={`balance-${module}`}>
      <Card title={t(title)} className="h-full">
        <dl className="flex flex-col gap-2">
          <div>
            <dt className="text-sm text-fg-muted">{t('dashboard.balance.available')}</dt>
            <dd className="text-xl font-semibold">{creditsText(balance.available)}</dd>
          </div>
          <div>
            <dt className="text-sm text-fg-muted">{t('dashboard.balance.reserved')}</dt>
            <dd>{creditsText(balance.reserved)}</dd>
          </div>
        </dl>
      </Card>
    </div>
  )
}

function SmtpBalance({ m3 }: { m3: M3Entitlement }) {
  const summary = m3Summary(m3)
  return (
    <div data-testid="balance-m3">
      <Card title={t('dashboard.balance.m3')} className="h-full">
        <div className="flex flex-col gap-2">
          <span className="self-start">
            <StatusBadge status={summary.token} label={t(M3_STATE_WORD[summary.state])} />
          </span>
          {summary.lines.map((line) => (
            <p key={line}>{line}</p>
          ))}
          <p className="text-sm text-fg-muted">{t('dashboard.m3.noCredits')}</p>
        </div>
      </Card>
    </div>
  )
}

function BalanceCards({ balances }: { balances: Balances }) {
  return (
    <>
      <div className="grid gap-3 md:grid-cols-3">
        <CreditsBalance module="m1" balance={balances.modules.m1} />
        <CreditsBalance module="m2" balance={balances.modules.m2} />
        <SmtpBalance m3={balances.m3} />
      </div>
      <p className="mt-2 text-sm text-fg-muted">{t('dashboard.balance.reservedHint')}</p>
    </>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// the jobs
// ---------------------------------------------------------------------------------------------------------------------------------------------------

/** The line under a job's name: when it starts, started or finished, else when it was created. */
function whenText(job: Job): string {
  if (job.status === 'scheduled' && job.scheduled_start !== null) return t('dashboard.job.starts', { date: formatDateTime(job.scheduled_start) })
  if (isFinal(job.status) && job.finished_at !== null) return t('dashboard.job.finished', { date: formatDateTime(job.finished_at) })
  if (job.started_at !== null) return t('dashboard.job.started', { date: formatDateTime(job.started_at) })
  return t('dashboard.job.created', { date: formatDateTime(job.created_at) })
}

function JobLine({ job }: { job: Job }) {
  return (
    <li data-testid={`job-row-${job.id}`} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 py-2">
      <div className="min-w-0">
        <Link to={pathFor('/jobs/:id', { id: job.id })} className="font-medium">
          {jobName(job)}
        </Link>
        <p className="text-sm text-fg-muted">
          {whenText(job)} · {addressCountText(job)}
        </p>
      </div>
      <span data-testid={`job-status-${job.id}`}>
        <StatusBadge status={jobStatusToken(job.status)} label={jobStatusText(job.status)} />
      </span>
    </li>
  )
}

function JobList({ jobs }: { jobs: readonly Job[] }) {
  return (
    <ul className="divide-y divide-border">
      {jobs.map((job) => (
        <JobLine key={job.id} job={job} />
      ))}
    </ul>
  )
}

function newVerifyLink() {
  return (
    <Link to="/new/verify" className={buttonClass('primary')}>
      {t('common.nav.newVerify')}
    </Link>
  )
}

/** The newest first of both lists, once each: the jobs the API calls active, and those it does not but that are in progress all the same (confirmed, finishing). */
export function inProgress(active: readonly Job[], newest: readonly Job[]): Job[] {
  const byId = new Map<string, Job>()
  for (const job of [...newest.filter((j) => isLive(j.status)), ...active]) byId.set(job.id, job)
  return [...byId.values()].sort((a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0))
}

export function DashboardPage() {
  const balances = useRemote((signal) => balancesApi.get({ signal }), [])
  const active = useRemote((signal) => jobsApi.list({ status: 'active' }, { signal }), [])
  const newest = useRemote((signal) => jobsApi.list({ limit: RECENT_WINDOW }, { signal }), [])

  usePolling(async (signal) => {
    await Promise.all([balances.refresh(signal), active.refresh(signal), newest.refresh(signal)])
  }, DASHBOARD_POLL_MS)

  const working = active.data === undefined ? [] : inProgress(active.data.jobs, newest.data?.jobs ?? [])
  const finished = (newest.data?.jobs ?? []).filter((j) => isFinal(j.status)).slice(0, RECENT_COUNT)

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('dashboard.title')} description={t('dashboard.intro')} actions={newVerifyLink()} />

      <section aria-labelledby="dashboard-balances" className="flex flex-col gap-2">
        <h2 id="dashboard-balances" className="text-lg font-semibold">
          {t('dashboard.balances.title')}
        </h2>
        {balances.error !== null ? (
          <ErrorNotice error={balances.error} onRetry={balances.retry} />
        ) : balances.data === undefined ? (
          <Spinner />
        ) : (
          <BalanceCards balances={balances.data} />
        )}
      </section>

      <Card title={t('dashboard.active.title')}>
        {active.error !== null ? (
          <ErrorNotice error={active.error} onRetry={active.retry} />
        ) : active.data === undefined ? (
          <Spinner />
        ) : working.length === 0 ? (
          <EmptyState title={t('dashboard.active.empty.title')} description={t('dashboard.active.empty.body')} action={newVerifyLink()} />
        ) : (
          <JobList jobs={working} />
        )}
      </Card>

      <Card
        title={t('dashboard.recent.title')}
        actions={
          <Link to="/jobs" className={buttonClass('secondary', 'sm')}>
            {t('dashboard.recent.all')}
          </Link>
        }
      >
        {newest.error !== null ? (
          <ErrorNotice error={newest.error} onRetry={newest.retry} />
        ) : newest.data === undefined ? (
          <Spinner />
        ) : finished.length === 0 ? (
          <EmptyState title={t('dashboard.recent.empty.title')} description={t('dashboard.recent.empty.body')} action={newVerifyLink()} />
        ) : (
          <JobList jobs={finished} />
        )}
      </Card>
    </div>
  )
}
