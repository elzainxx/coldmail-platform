// /jobs: the user's jobs, newest first, as a live table. It asks again every 3 seconds while any job on the page is in progress (so a running job moves on
// its own), every 15 seconds otherwise (a job started in another window turns up), and not at all while the tab is hidden (api/poll.ts).
import { jobsApi } from '../../api/endpoints.ts'
import { usePolling } from '../../api/poll.ts'
import type { Job } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { jobStatusText } from '../../copy/words.ts'
import { Link, pathFor } from '../../router.tsx'
import { buttonClass, EmptyState, PageHeader, Pagination, StatusBadge, Table, jobStatusToken, useCursorPager, type Column } from '../../ui/index.ts'
import { ErrorNotice } from './ErrorNotice.tsx'
import { addressCountText, formatDateTime, isLive, jobName, kindText } from './format.ts'
import { IDLE_POLL_MS, LIVE_POLL_MS, useRemote } from './useRemote.ts'

const columns: readonly Column<Job>[] = [
  {
    key: 'job',
    header: t('jobs.table.job'),
    rowHeader: true,
    cell: (job) => (
      <Link to={pathFor('/jobs/:id', { id: job.id })} aria-label={jobName(job)}>
        {kindText(job.module)}
      </Link>
    ),
  },
  {
    key: 'status',
    header: t('jobs.table.status'),
    cell: (job) => (
      <span data-testid={`job-status-${job.id}`}>
        <StatusBadge status={jobStatusToken(job.status)} label={jobStatusText(job.status)} />
      </span>
    ),
  },
  { key: 'addresses', header: t('jobs.table.addresses'), align: 'right', cell: (job) => addressCountText(job) },
  { key: 'created', header: t('jobs.table.created'), cell: (job) => <time dateTime={job.created_at}>{formatDateTime(job.created_at)}</time> },
]

export function JobsPage() {
  const pager = useCursorPager()
  const list = useRemote((signal) => jobsApi.list({ cursor: pager.cursor }, { signal }), [pager.cursor])
  const rows = list.data?.jobs ?? []
  const hasNext = list.data?.next_cursor != null
  const anyInProgress = rows.some((job) => isLive(job.status))
  usePolling((signal) => list.refresh(signal), anyInProgress ? LIVE_POLL_MS : IDLE_POLL_MS)

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={t('jobs.title')}
        description={t('jobs.intro')}
        actions={
          <Link to="/new/verify" className={buttonClass('primary')}>
            {t('common.nav.newVerify')}
          </Link>
        }
      />
      {list.error !== null ? (
        <ErrorNotice error={list.error} onRetry={list.retry} />
      ) : (
        <>
          <Table
            caption={t('jobs.table.caption')}
            captionHidden
            columns={columns}
            rows={rows}
            rowKey={(job) => job.id}
            rowTestId={(job) => `job-row-${job.id}`}
            loading={list.loading}
            empty={
              <EmptyState
                title={t('jobs.table.empty.title')}
                description={t('jobs.table.empty.body')}
                action={
                  <Link to="/new/verify" className={buttonClass('primary')}>
                    {t('common.nav.newVerify')}
                  </Link>
                }
              />
            }
          />
          {(pager.hasPrevious || hasNext) && (
            <Pagination
              page={pager.page}
              hasPrevious={pager.hasPrevious}
              hasNext={hasNext}
              busy={list.loading}
              onPrevious={pager.goPrevious}
              onNext={() => pager.goNext(list.data?.next_cursor ?? null)}
            />
          )}
        </>
      )}
    </div>
  )
}
