// "From a finished verification": the user's finished verifications to choose one from (with their counts), which results of it are used, and the one answer the
// platform insists on, what to do with the addresses that came back unknown.
//
// What is used is the platform's rule, not a choice: valid and risky addresses are included, invalid ones never are (Docs/jobs-api.md, "Create a draft"). They are
// shown as the three checkboxes they would be, fixed, so the user sees what a send to a verification does without being offered a choice that does not exist.
// Only the unknown addresses are the user's to decide, and there is no default.
import type { UnknownChoice } from '../../api/types.ts'
import { errorText, t } from '../../copy/index.ts'
import { Link } from '../../router.tsx'
import { Button, Checkbox, EmptyState, Spinner } from '../../ui/index.ts'
import { Choice, ChoiceGroup } from './Choice.tsx'
import { fmt, formatWhen } from './schedule.ts'
import { UnknownChoiceGroup } from './UnknownChoiceGroup.tsx'
import { useFinishedVerifications } from './useFinishedVerifications.ts'

export interface VerificationSourceProps {
  selectedId: string | null
  onSelect: (id: string) => void
  unknownChoice: UnknownChoice | null
  onUnknownChoice: (choice: UnknownChoice) => void
}

export function VerificationSource({ selectedId, onSelect, unknownChoice, onUnknownChoice }: VerificationSourceProps) {
  const list = useFinishedVerifications(true)

  return (
    <div className="flex flex-col gap-4">
      {list.error !== null && (
        <div role="alert" className="flex flex-col items-start gap-2 rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
          <p>{errorText(list.error)}</p>
          <Button size="sm" onClick={list.retry}>
            {t('common.retry')}
          </Button>
        </div>
      )}

      {list.loading && list.jobs.length === 0 && <Spinner label={t('newjob.verification.loading')} />}

      {!list.loading && list.error === null && list.jobs.length === 0 && (
        <EmptyState
          title={t('newjob.verification.emptyTitle')}
          description={t('newjob.verification.emptyBody')}
          action={<Link to="/new/verify">{t('newjob.verification.emptyLink')}</Link>}
        />
      )}

      {list.jobs.length > 0 && (
        <ChoiceGroup legend={t('newjob.verification.legend')}>
          {list.jobs.map((job) => (
            <Choice
              key={job.id}
              name="verification"
              value={job.id}
              checked={selectedId === job.id}
              onChange={() => onSelect(job.id)}
              label={t('newjob.verification.finished', { when: formatWhen(job.finished_at ?? job.created_at) })}
              description={
                job.preview === null
                  ? undefined
                  : t('newjob.verification.counts', { rows: fmt(job.preview.rows), checked: fmt(job.preview.units) })
              }
            />
          ))}
        </ChoiceGroup>
      )}
      {list.hasMore && (
        <div>
          <Button size="sm" onClick={list.more} busy={list.loading}>
            {t('newjob.verification.more')}
          </Button>
        </div>
      )}

      {selectedId !== null && (
        <>
          <ChoiceGroup legend={t('newjob.verdicts.legend')} hint={t('newjob.verdicts.hint')}>
            <Checkbox checked disabled label={t('newjob.verdicts.valid')} description={t('newjob.verdicts.always')} onChange={() => undefined} />
            <Checkbox checked disabled label={t('newjob.verdicts.risky')} description={t('newjob.verdicts.always')} onChange={() => undefined} />
            <Checkbox checked={false} disabled label={t('newjob.verdicts.invalid')} description={t('newjob.verdicts.never')} onChange={() => undefined} />
          </ChoiceGroup>
          <UnknownChoiceGroup value={unknownChoice} onChange={onUnknownChoice} />
        </>
      )}
    </div>
  )
}
