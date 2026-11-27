// The part of both pages that comes after the list (and the message) is chosen: the draft being made and previewed, a failure of that in words with a way
// on, the preview (figures, samples), the confirm, and at the end the confirmation. A draft continued from the jobs page (`?job=`) comes in here too.
import { useEffect, useRef } from 'react'
import type { Balances, ConfirmResult, Job } from '../../api/types.ts'
import type { ApiError } from '../../api/errors.ts'
import { errorReference, errorText, t } from '../../copy/index.ts'
import { Link, pathFor } from '../../router.tsx'
import { Button, buttonClass, Card, Spinner } from '../../ui/index.ts'
import { ConfirmPanel } from './ConfirmPanel.tsx'
import { PreviewPanel } from './PreviewPanel.tsx'
import { fmt, formatWhen } from './schedule.ts'
import { Samples } from './Samples.tsx'
import type { Draft, DraftBusy } from './useDraft.ts'
import { VendorDisclosure } from './VendorDisclosure.tsx'

const BUSY_KEYS = {
  loading: 'newjob.working.loading',
  creating: 'newjob.working.creating',
  previewing: 'newjob.working.previewing',
} as const satisfies Record<DraftBusy, string>

function Working({ busy }: { busy: DraftBusy }) {
  return (
    <div className="flex items-center gap-2 text-base text-fg">
      <Spinner label="" />
      <p role="status">{t(BUSY_KEYS[busy])}</p>
    </div>
  )
}

function ReviewError({ error, hint, onRetry, onRestart, restartLabel }: { error: ApiError; hint?: string; onRetry: () => void; onRestart: () => void; restartLabel: string }) {
  return (
    <div role="alert" className="flex flex-col items-start gap-2 rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
      <p>{errorText(error)}</p>
      {hint !== undefined && error.code === 'validation_failed' && <p>{hint}</p>}
      {error.status >= 500 && errorReference(error) !== null && <p className="text-xs">{errorReference(error)}</p>}
      <div className="flex gap-2">
        <Button size="sm" onClick={onRetry}>
          {t('common.retry')}
        </Button>
        <Button size="sm" onClick={onRestart}>
          {restartLabel}
        </Button>
      </div>
    </div>
  )
}

function Done({ result }: { result: ConfirmResult }) {
  const heading = useRef<HTMLHeadingElement>(null)
  useEffect(() => heading.current?.focus(), [])
  const job = result.job
  const when = job.status === 'scheduled' && job.scheduled_start !== null ? formatWhen(job.scheduled_start) : null
  const leftOut = [
    result.suppressed > 0 && job.module !== 'm1' ? t('newjob.done.suppressed', { count: fmt(result.suppressed) }) : null,
    result.missing_fields > 0 ? t('newjob.done.missing', { count: fmt(result.missing_fields) }) : null,
    result.unrenderable > 0 ? t('newjob.done.unrenderable', { count: fmt(result.unrenderable) }) : null,
  ].filter((s): s is string => s !== null)
  return (
    <section aria-labelledby="done-title" className="rounded-lg border border-border bg-raised p-4 shadow-sm">
      <h2 id="done-title" ref={heading} tabIndex={-1} className="text-lg font-semibold text-fg">
        {when === null ? t('newjob.done.title') : t('newjob.done.titleScheduled')}
      </h2>
      <div role="status" className="mt-2 flex flex-col gap-1 text-base text-fg">
        <p>{when === null ? t('newjob.done.started') : t('newjob.done.scheduledFor', { when })}</p>
        <p>{t('newjob.done.pending', { count: fmt(result.pending) })}</p>
        {result.reserved > 0 && <p>{t('newjob.done.reserved', { credits: fmt(result.reserved) })}</p>}
        {leftOut.map((line) => (
          <p key={line} className="text-fg-muted">
            {line}
          </p>
        ))}
        {result.replayed && <p className="text-fg-muted">{t('newjob.done.replayed')}</p>}
      </div>
      <p className="mt-3">
        <Link to={pathFor('/jobs/:id', { id: job.id })} className={buttonClass('primary')}>
          {t('newjob.done.open')}
        </Link>
      </p>
    </section>
  )
}

export interface ReviewSectionProps {
  draft: Draft
  /** which page this is: a job of the other kind is not continued here */
  expect: 'verify' | 'send'
  balances: Balances | null
  /** the user's opt-out in force now (a verification's disclosure); null when it could not be read */
  settingsOptedOut: boolean | null
  onRestart: () => void
  restartLabel: string
  /** said under a refusal of the file ("check the rows") */
  fileHint?: string
}

function NotWaiting({ job }: { job: Job }) {
  return (
    <Card title={t('newjob.review.startedTitle')}>
      <p className="mb-3 text-base text-fg">{t('newjob.review.started')}</p>
      <Link to={pathFor('/jobs/:id', { id: job.id })} className={buttonClass('primary')}>
        {t('newjob.done.open')}
      </Link>
    </Card>
  )
}

function WrongPage({ job }: { job: Job }) {
  const verify = job.module === 'm1'
  return (
    <Card title={t('newjob.review.wrongTitle')}>
      <p className="mb-3 text-base text-fg">{verify ? t('newjob.review.isVerify') : t('newjob.review.isSend')}</p>
      <Link to={`${verify ? '/new/verify' : '/new/send'}?job=${encodeURIComponent(job.id)}`} className={buttonClass('primary')}>
        {verify ? t('newjob.review.goVerify') : t('newjob.review.goSend')}
      </Link>
    </Card>
  )
}

export function ReviewSection({ draft, expect, balances, settingsOptedOut, onRestart, restartLabel, fileHint }: ReviewSectionProps) {
  const { job, busy, error, result } = draft
  if (result !== null && job !== null) return <Done result={result} />

  if (job === null) {
    if (busy !== null) return <Working busy={busy} />
    if (error !== null) return <ReviewError error={error} hint={fileHint} onRetry={draft.retry} onRestart={onRestart} restartLabel={restartLabel} />
    return null
  }

  if ((job.module === 'm1') !== (expect === 'verify')) return <WrongPage job={job} />
  const preview = job.preview
  if (job.status !== 'previewed' && job.status !== 'draft') return <NotWaiting job={job} />
  if (job.status === 'draft' || preview === null) {
    return (
      <div className="flex flex-col items-start gap-2">
        <p className="text-base text-fg">{t('newjob.review.notPrepared')}</p>
        <Button onClick={() => void draft.rePreview()}>{t('newjob.review.prepare')}</Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4" aria-busy={busy !== null || undefined}>
      {busy !== null && <Working busy={busy} />}
      {error !== null && <ReviewError error={error} onRetry={() => void draft.rePreview()} onRestart={onRestart} restartLabel={restartLabel} />}
      <PreviewPanel
        job={job}
        preview={preview}
        balances={balances}
        disclosure={job.module === 'm1' ? <VendorDisclosure jobOptedOut={typeof preview.vendor_opt_out === 'boolean' ? preview.vendor_opt_out : (settingsOptedOut ?? false)} settingsOptedOut={settingsOptedOut} /> : undefined}
      />
      {job.module !== 'm1' && preview.samples !== undefined && <Samples samples={preview.samples} />}
      <ConfirmPanel job={job} preview={preview} rePreview={draft.rePreview} onConfirmed={draft.confirmed} onRestart={onRestart} restartLabel={restartLabel} />
    </div>
  )
}
