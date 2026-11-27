// A job's history in the customer's words. The API gives an event's kind, the two statuses of a move, who acted and a `reason`:
// the reason is a sentence the platform wrote for its own log (some begin with a word like `pause_limit`), so it is NEVER shown. The line comes from the kind, the
// statuses and, for the three words the platform uses on purpose, the start of the reason. An event the screen cannot read shows one generic line.
import type { JobEvent } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { jobStatusText } from '../../copy/words.ts'

/** Does the platform's reason begin with this word (`pause_limit`, `module_paused: ...`)? Only the word is read; the rest of the sentence is not used. */
function reasonBegins(reason: unknown, word: string): boolean {
  if (typeof reason !== 'string' || !reason.startsWith(word)) return false
  const next = reason.charAt(word.length)
  return next === '' || !/[A-Za-z0-9_]/.test(next)
}

function transitionText(e: JobEvent): string {
  const to = e.to_status
  const from = e.from_status
  if (to === null || from === null) return t('jobs.event.transition.other')
  if (e.actor_kind === 'admin') return t('jobs.event.byTeam', { status: jobStatusText(to) })
  const pauseLimit = reasonBegins(e.reason, 'pause_limit')
  switch (to) {
    case 'draft':
      return t('jobs.event.backToDraft')
    case 'previewed':
      return t('jobs.event.previewed')
    case 'confirmed':
      return t('jobs.event.confirmed')
    case 'scheduled':
      return from === 'paused' ? t('jobs.event.resumedScheduled') : t('jobs.event.scheduled')
    case 'running':
      return from === 'paused' ? t('jobs.event.resumedRunning') : t('jobs.event.started')
    case 'paused':
      return from === 'running' ? t('jobs.event.pausedRunning') : t('jobs.event.pausedBeforeStart')
    case 'cancelling':
      return pauseLimit ? t('jobs.event.pauseLimitCancelling') : t('jobs.event.cancelling')
    case 'completing':
      return t('jobs.event.completing')
    case 'succeeded':
      return t('jobs.event.succeeded')
    case 'cancelled':
      if (pauseLimit) return t('jobs.event.pauseLimitCancelled')
      if (from === 'draft' || from === 'previewed') return t('jobs.event.discarded')
      return from === 'completing' ? t('jobs.event.cancelledAfterStart') : t('jobs.event.cancelledBeforeStart')
    case 'expired':
      return t('jobs.event.expired')
    case 'failed':
      return t('jobs.event.failed')
    default:
      return t('jobs.event.transition.other')
  }
}

/** The line for one event, or null when the event is only for the operator and the customer's history leaves it out. */
export function eventText(e: JobEvent): string | null {
  switch (e.kind) {
    case 'created':
      return t('jobs.event.created')
    case 'transition':
      return transitionText(e)
    case 'stale':
      return t('jobs.event.stale')
    case 'flag':
      return t('jobs.event.flag')
    case 'note':
      // The note beside a pause-limit move carries the limit for the operator; the move above it already says what happened.
      if (reasonBegins(e.reason, 'pause_limit')) return null
      if (reasonBegins(e.reason, 'module_paused')) return t('jobs.event.note.paused')
      if (reasonBegins(e.reason, 'module_resumed')) return t('jobs.event.note.resumed')
      return t('jobs.event.note.other')
    default:
      return t('jobs.event.other')
  }
}
