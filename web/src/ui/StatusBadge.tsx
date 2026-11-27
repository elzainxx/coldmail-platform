import { t } from '../copy/index.ts'
import { cx } from './cx.ts'
import type { StatusToken } from './status.ts'

// Literal class names (not built from the token) so Tailwind sees every one of them.
const STYLES: Record<StatusToken, string> = {
  valid: 'bg-status-valid-bg text-status-valid',
  risky: 'bg-status-risky-bg text-status-risky',
  invalid: 'bg-status-invalid-bg text-status-invalid',
  unknown: 'bg-status-unknown-bg text-status-unknown',
  sent: 'bg-status-sent-bg text-status-sent',
  bounced: 'bg-status-bounced-bg text-status-bounced',
  failed: 'bg-status-failed-bg text-status-failed',
}

export interface StatusBadgeProps {
  /** The colour: one of the seven status tokens. */
  status: StatusToken
  /** The word. Defaults to the token's own word ("Valid"); pass the word of a job state or a result state (copy/words.ts) for those. A badge always has a word. */
  label?: string
  className?: string
}

/** A status as a word AND a colour: the colour never carries the meaning alone. */
export function StatusBadge({ status, label, className }: StatusBadgeProps) {
  return (
    <span className={cx('inline-flex h-5 items-center gap-1 rounded-full px-2 text-xs font-medium whitespace-nowrap', STYLES[status], className)} data-status={status}>
      <span aria-hidden="true" className="size-1.5 rounded-full bg-current" />
      {label ?? t(`common.status.${status}`)}
    </span>
  )
}
