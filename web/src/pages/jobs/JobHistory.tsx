// The job's history in words: one line per event, oldest first. Nothing of an event's own `reason` is shown (eventText.ts).
import type { Job } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Card, Spinner } from '../../ui/index.ts'
import { ErrorNotice } from './ErrorNotice.tsx'
import { eventText } from './eventText.ts'
import { formatDateTime } from './format.ts'
import { loadEvents } from './jobData.ts'
import { useRemote } from './useRemote.ts'

export function JobHistory({ job }: { job: Job }) {
  // The history is read again whenever the job moves (its status or its last change), so it needs no polling of its own.
  const events = useRemote((signal) => loadEvents(job.id, signal), [job.id, job.status, job.updated_at])
  const lines = (events.data ?? []).flatMap((e) => {
    const text = eventText(e)
    return text === null ? [] : [{ id: e.id, at: e.created_at, text }]
  })

  return (
    <Card title={t('jobs.history.title')}>
      {events.error !== null ? (
        <ErrorNotice error={events.error} onRetry={events.retry} />
      ) : events.data === undefined ? (
        <Spinner />
      ) : lines.length === 0 ? (
        <p className="text-fg-muted">{t('jobs.history.empty')}</p>
      ) : (
        <ol className="flex flex-col gap-2">
          {lines.map((line) => (
            <li key={line.id} className="grid gap-x-4 sm:grid-cols-[12rem_1fr]">
              <time dateTime={line.at} className="text-sm text-fg-muted">
                {formatDateTime(line.at)}
              </time>
              <span>{line.text}</span>
            </li>
          ))}
        </ol>
      )}
    </Card>
  )
}
