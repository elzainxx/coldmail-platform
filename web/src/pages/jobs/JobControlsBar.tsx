// Pause, Resume and Cancel of one job. Only the controls the job's state allows are there (controls.ts); a refusal the API makes anyway for a reason this screen now knows
// (a verification job has no pause, a start that has passed cannot be resumed) takes the control away and says why in a calm sentence. Cancel asks first, in plain words
// about what happens to what was not done and to the credits. Whatever a request answers, the job it returns is what the page shows at once.
import { useRef, useState } from 'react'
import { jobsApi } from '../../api/endpoints.ts'
import type { ApiError } from '../../api/errors.ts'
import { isAbortError } from '../../api/errors.ts'
import type { Job } from '../../api/types.ts'
import { maybeT, t, type CopyKey } from '../../copy/index.ts'
import { Button, ConfirmDialog, useToast } from '../../ui/index.ts'
import { controlsFor, refusalOf, type CancelKind, type Refusal } from './controls.ts'
import { ErrorNotice } from './ErrorNotice.tsx'
import { asApiError, type Remote } from './useRemote.ts'

type Action = 'pause' | 'resume' | 'cancel'

function toastKey(action: Action, answered: Job): CopyKey {
  if (action === 'pause') return 'jobs.toast.paused'
  if (action === 'resume') return answered.status === 'running' ? 'jobs.toast.resumedRunning' : 'jobs.toast.resumedScheduled'
  return answered.status === 'cancelling' ? 'jobs.toast.cancelling' : 'jobs.toast.cancelled'
}

/** What the cancel question says will happen, for this kind of job and this stage. */
export function cancelSentences(module: string, kind: CancelKind): string[] {
  if (kind === 'notStarted') return [module === 'm3' ? t('jobs.cancel.notStarted.m3') : t('jobs.cancel.notStarted.credits')]
  return [maybeT(`jobs.cancel.started.${module}`) ?? t('jobs.cancel.started.other'), t('jobs.cancel.stopping')]
}

export function JobControlsBar({ job, remote }: { job: Job; remote: Remote<Job> }) {
  const toast = useToast()
  const [asking, setAsking] = useState(false)
  const [acting, setActing] = useState<Action | null>(null)
  const [error, setError] = useState<ApiError | null>(null)
  const inFlight = useRef(false)

  // The refusals met for this state of the job; they are forgotten when the job moves to another state.
  const stateKey = `${job.status}/${job.paused_from ?? ''}`
  const [met, setMet] = useState<{ key: string; refusals: Refusal[] }>({ key: stateKey, refusals: [] })
  const refused = new Set<Refusal>(met.key === stateKey ? met.refusals : [])
  const controls = controlsFor(job, Date.now(), refused)

  const run = async (action: Action) => {
    if (inFlight.current) return // one request at a time: a second click or Enter never posts twice
    inFlight.current = true
    setActing(action)
    setError(null)
    try {
      const answer = await jobsApi[action](job.id)
      remote.replace(answer.job)
      toast.notify(t(toastKey(action, answer.job)), { tone: 'success' })
    } catch (e) {
      if (isAbortError(e)) return
      const err = asApiError(e)
      setError(err)
      const refusal = err.code === 'not_allowed' ? refusalOf(err.reason) : null
      if (refusal !== null) setMet((prev) => ({ key: stateKey, refusals: [...new Set([...(prev.key === stateKey ? prev.refusals : []), refusal])] }))
      void remote.refresh() // the job may have moved on its own (it finished while the page was open): show where it really stands
    } finally {
      inFlight.current = false
      setActing(null)
      setAsking(false)
    }
  }

  const hasButtons = controls.pause || controls.resume || controls.cancel
  if (!hasButtons && controls.notes.length === 0 && error === null) return null

  return (
    <div className="flex flex-col gap-2">
      {hasButtons && (
        <div role="group" aria-label={t('jobs.controls.label')} className="flex flex-wrap items-center gap-2">
          {controls.pause && (
            <Button onClick={() => void run('pause')} busy={acting === 'pause'}>
              {t('jobs.control.pause')}
            </Button>
          )}
          {controls.resume && (
            <Button variant="primary" onClick={() => void run('resume')} busy={acting === 'resume'}>
              {t('jobs.control.resume')}
            </Button>
          )}
          {controls.cancel && (
            <Button variant="danger" onClick={() => setAsking(true)}>
              {t('jobs.control.cancel')}
            </Button>
          )}
        </div>
      )}
      {controls.notes.map((key) => (
        <p key={key} className="text-sm text-fg-muted">
          {t(key)}
        </p>
      ))}
      {error !== null && <ErrorNotice error={error} />}
      <ConfirmDialog
        open={asking}
        title={t('jobs.cancel.title')}
        description={
          <div className="flex flex-col gap-2">
            {cancelSentences(job.module, controls.cancelKind).map((sentence) => (
              <p key={sentence}>{sentence}</p>
            ))}
          </div>
        }
        confirmLabel={t('jobs.cancel.confirm')}
        cancelLabel={t('jobs.cancel.keep')}
        destructive
        busy={acting === 'cancel'}
        onConfirm={() => void run('cancel')}
        onCancel={() => setAsking(false)}
      />
    </div>
  )
}
