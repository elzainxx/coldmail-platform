// The last step of a job: start now or at a time, and confirm. It sends the fingerprint of the preview the user looked at and an Idempotency-Key made once per
// intent (confirmAttempt.ts), and it answers each way the platform can say no, in words and with the next step:
//
//   preview_stale             looks again (a new preview), says what changed, and waits for the user to confirm the new figures
//   insufficient_credits      the shortfall in credits, and "check again" once credits were added
//   job_slot_taken            "you already have a job of this kind running: wait or cancel it", with a link to the jobs
//   nothing_to_do             nothing in the list can be processed: choose a different list
//   unknown_choice_required   the question about unknown addresses is still open
//
// No code of the platform and no sentence of its own is ever shown: errorText (copy/index.ts) is the only way an error becomes text.
import { useId, useRef, useState } from 'react'
import { jobsApi } from '../../api/endpoints.ts'
import type { ApiError } from '../../api/errors.ts'
import type { ConfirmRequest, ConfirmResult, Job, Preview, UnknownChoice } from '../../api/types.ts'
import { errorReference, errorText, t } from '../../copy/index.ts'
import { Link } from '../../router.tsx'
import { Button, Card, FormField, TextInput } from '../../ui/index.ts'
import { Choice, ChoiceGroup } from './Choice.tsx'
import { nextAttempt, type Attempt } from './confirmAttempt.ts'
import { isAbort, outcomeUnknown, toApiError } from './errors.ts'
import { describeChanges, previewChanges } from './previewDiff.ts'
import { fmt, formatWhen, localInputValue, MIN_LEAD_MINUTES, scheduleFromLocal, zoneInfo, type SchedulePlace } from './schedule.ts'
import { UnknownChoiceGroup } from './UnknownChoiceGroup.tsx'

const WHEN_KEYS = {
  empty: 'newjob.schedule.errorEmpty',
  invalid: 'newjob.schedule.errorInvalid',
  tooSoon: 'newjob.schedule.errorTooSoon',
  tooFar: 'newjob.schedule.errorTooFar',
} as const satisfies Record<SchedulePlace, string>

export interface ConfirmPanelProps {
  job: Job
  preview: Preview
  /** look at the list again (optionally under a new choice for unknown addresses); the new job, or null when that failed */
  rePreview: (choice?: UnknownChoice) => Promise<Job | null>
  onConfirmed: (result: ConfirmResult) => void
  /** back to choosing the list, letting this draft go */
  onRestart: () => void
  restartLabel: string
}

function number(details: Record<string, unknown> | undefined, name: string): number | null {
  const v = details?.[name]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

/** The credits refusal: how many are needed, how many there are, how many short, and the next step. */
function CreditsNotice({ error }: { error: ApiError }) {
  const needed = number(error.details, 'needed')
  const available = number(error.details, 'available')
  const shortfall = number(error.details, 'shortfall')
  return (
    <>
      <p className="font-medium">{t('newjob.confirm.creditsTitle')}</p>
      {needed !== null && available !== null && shortfall !== null ? (
        <p>{t('newjob.confirm.creditsNumbers', { needed: fmt(needed), available: fmt(available), shortfall: fmt(shortfall) })}</p>
      ) : (
        <p>{errorText(error)}</p>
      )}
      <p>{t('newjob.confirm.creditsNext')}</p>
    </>
  )
}

export function ConfirmPanel({ job, preview, rePreview, onConfirmed, onRestart, restartLabel }: ConfirmPanelProps) {
  const name = useId()
  const [mode, setMode] = useState<'now' | 'later'>('now')
  const [when, setWhen] = useState('')
  const [whenError, setWhenError] = useState<SchedulePlace | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  const [changes, setChanges] = useState<string[] | null>(null)
  const attempt = useRef<Attempt | null>(null)

  const chained = job.source_job_id !== null
  const choiceOpen = chained && preview.unknown_choice_required && preview.unknown_choice === null
  const outOfCredits = preview.metered && preview.shortfall > 0
  const nothing = preview.units === 0
  const blocked = nothing || outOfCredits || choiceOpen

  const zone = zoneInfo()
  const picked = mode === 'later' ? scheduleFromLocal(when) : null

  const changeChoice = async (choice: UnknownChoice) => {
    if (busy) return
    setBusy(true)
    setError(null)
    setChanges(null)
    try {
      await rePreview(choice)
    } finally {
      setBusy(false)
    }
  }

  const checkAgain = async () => {
    if (busy) return
    setBusy(true)
    setError(null)
    setChanges(null)
    try {
      await rePreview()
    } finally {
      setBusy(false)
    }
  }

  const confirm = async () => {
    if (busy || blocked) return
    let scheduledStart: string | null = null
    if (mode === 'later') {
      const check = scheduleFromLocal(when)
      if (!check.ok) {
        setWhenError(check.problem)
        return
      }
      scheduledStart = check.iso
    }
    setWhenError(null)

    const body: ConfirmRequest = {
      fingerprint: preview.fingerprint,
      ...(scheduledStart === null ? {} : { scheduled_start: scheduledStart }),
      ...(chained && preview.unknown_choice !== null ? { unknown_choice: preview.unknown_choice } : {}),
    }
    const mine = nextAttempt(attempt.current, { jobId: job.id, fingerprint: body.fingerprint, scheduledStart, unknownChoice: body.unknown_choice ?? null })
    attempt.current = mine

    setBusy(true)
    setError(null)
    setChanges(null)
    try {
      const result = await jobsApi.confirm(job.id, body, mine.key)
      attempt.current = null
      onConfirmed(result)
    } catch (e) {
      if (isAbort(e)) return
      const err = toApiError(e)
      // A refusal ends the attempt (the next press is a new one); a confirm whose outcome is not known is repeated with the same key.
      if (!outcomeUnknown(err)) attempt.current = null
      if (err.code === 'preview_stale') {
        const next = await rePreview()
        setChanges(next?.preview ? describeChanges(previewChanges(preview, next.preview)) : [])
      } else {
        setError(err)
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card title={t('newjob.confirm.title')}>
      <div className="flex flex-col gap-4">
        {chained && (
          <UnknownChoiceGroup
            value={preview.unknown_choice}
            onChange={(c) => void changeChoice(c)}
            disabled={busy}
            error={choiceOpen || error?.code === 'unknown_choice_required' ? t('newjob.unknown.open') : null}
          />
        )}

        <ChoiceGroup legend={t('newjob.confirm.when')}>
          <Choice name={name} value="now" checked={mode === 'now'} onChange={() => setMode('now')} disabled={busy} label={t('newjob.confirm.now')} />
          <Choice name={name} value="later" checked={mode === 'later'} onChange={() => setMode('later')} disabled={busy} label={t('newjob.confirm.later')} />
        </ChoiceGroup>

        {mode === 'later' && (
          <div className="flex flex-col gap-1">
            <FormField
              label={t('newjob.schedule.label')}
              required
              hint={t('newjob.schedule.hint', { minutes: MIN_LEAD_MINUTES })}
              error={whenError === null ? null : t(WHEN_KEYS[whenError])}
            >
              <TextInput
                type="datetime-local"
                value={when}
                min={localInputValue(new Date(Date.now() + MIN_LEAD_MINUTES * 60_000))}
                onChange={(e) => {
                  setWhen(e.target.value)
                  setWhenError(null)
                }}
                disabled={busy}
              />
            </FormField>
            <p className="text-sm text-fg-muted">
              {zone.name === null ? t('newjob.schedule.zoneLocal', { offset: zone.offset }) : t('newjob.schedule.zone', { zone: zone.name, offset: zone.offset })}
            </p>
            {picked?.ok === true && <p className="text-sm text-fg">{t('newjob.schedule.resolved', { when: formatWhen(picked.iso) })}</p>}
          </div>
        )}

        {nothing && <p className="text-sm text-fg">{t('newjob.confirm.blockedNothing')}</p>}
        {outOfCredits && <p className="text-sm text-fg">{t('newjob.confirm.blockedCredits')}</p>}
        {choiceOpen && <p className="text-sm text-fg">{t('newjob.confirm.blockedChoice')}</p>}

        {changes !== null && (
          <div role="status" className="rounded-md bg-status-risky-bg px-3 py-2 text-sm text-status-risky">
            <p className="font-medium">{t('newjob.stale.title')}</p>
            {changes.length > 0 ? (
              <ul className="mt-1 list-disc pl-5">
                {changes.map((line) => (
                  <li key={line}>{line}</li>
                ))}
              </ul>
            ) : (
              <p>{t('newjob.stale.nothingVisible')}</p>
            )}
            <p className="mt-1">{t('newjob.stale.next')}</p>
          </div>
        )}

        {error !== null && (
          <div role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
            {error.code === 'insufficient_credits' ? <CreditsNotice error={error} /> : <p>{errorText(error)}</p>}
            {error.code === 'job_slot_taken' && (
              <p className="mt-1">
                <Link to="/jobs">{t('newjob.confirm.seeJobs')}</Link>
              </p>
            )}
            {error.status >= 500 && errorReference(error) !== null && <p className="mt-1 text-xs">{errorReference(error)}</p>}
          </div>
        )}

        <div className="flex flex-wrap items-center gap-2">
          <Button variant="primary" busy={busy} disabled={blocked} onClick={() => void confirm()}>
            {mode === 'later' ? t('newjob.confirm.submitLater') : t('newjob.confirm.submitNow')}
          </Button>
          {(outOfCredits || error?.code === 'insufficient_credits') && (
            <Button onClick={() => void checkAgain()} disabled={busy}>
              {t('newjob.confirm.checkAgain')}
            </Button>
          )}
          <Button onClick={onRestart} disabled={busy}>
            {restartLabel}
          </Button>
        </div>
      </div>
    </Card>
  )
}
