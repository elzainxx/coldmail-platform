import { t } from '../copy/index.ts'
import { cx } from './cx.ts'

export interface SpinnerProps {
  /** px */
  size?: number
  /** the words a screen reader hears; '' for a spinner inside a control that already says so */
  label?: string
  className?: string
}

export function Spinner({ size = 20, label, className }: SpinnerProps) {
  const text = label ?? t('common.loading')
  return (
    <span role={text === '' ? undefined : 'status'} className={cx('inline-flex items-center', className)}>
      <svg
        className="animate-spin text-fg-muted"
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill="none"
        aria-hidden="true"
        focusable="false"
      >
        <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="3" opacity="0.25" />
        <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
      </svg>
      {text !== '' && <span className="sr-only">{text}</span>}
    </span>
  )
}
