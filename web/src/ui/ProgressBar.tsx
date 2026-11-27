import { cx } from './cx.ts'

export interface ProgressBarProps {
  value: number
  max: number
  /** What is progressing ("Rows checked"): the accessible name. Required. */
  label: string
  /** show "value of max" and the percentage beside the bar */
  showValue?: boolean
  className?: string
}

export function ProgressBar({ value, max, label, showValue = false, className }: ProgressBarProps) {
  const safeMax = max > 0 ? max : 1
  const clamped = Math.min(Math.max(value, 0), safeMax)
  const percent = Math.round((clamped / safeMax) * 100)
  return (
    <div className={cx('flex items-center gap-3', className)}>
      <div
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={safeMax}
        aria-valuenow={clamped}
        className="h-2 flex-1 overflow-hidden rounded-full bg-sunken"
      >
        {/* the width is the one value that has to be dynamic */}
        <div className="h-full rounded-full bg-accent" style={{ width: `${percent}%` }} />
      </div>
      {showValue && (
        <span className="text-xs whitespace-nowrap text-fg-muted" aria-hidden="true">
          {percent}%
        </span>
      )}
    </div>
  )
}
