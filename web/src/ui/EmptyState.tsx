import type { ReactNode } from 'react'
import { cx } from './cx.ts'

export interface EmptyStateProps {
  title: string
  description?: ReactNode
  /** The thing to do about it: a Button or a link. */
  action?: ReactNode
  className?: string
}

/** What a list or a table says when there is nothing in it: what is missing, and what to do. */
export function EmptyState({ title, description, action, className }: EmptyStateProps) {
  return (
    <div className={cx('flex flex-col items-center gap-2 px-4 py-6 text-center', className)}>
      <p className="text-base font-medium text-fg">{title}</p>
      {description !== undefined && <p className="max-w-prose text-sm text-fg-muted">{description}</p>}
      {action !== undefined && <div className="mt-1">{action}</div>}
    </div>
  )
}
