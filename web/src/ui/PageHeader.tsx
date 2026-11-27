import type { ReactNode } from 'react'
import { cx } from './cx.ts'

export interface PageHeaderProps {
  /** The page title: the page's one <h1>. The router focuses it when the user arrives at the page. */
  title: string
  description?: ReactNode
  /** Buttons or links at the right of the title (the primary action of the page). */
  actions?: ReactNode
  className?: string
}

/** The top of every page. Render it at once, before any data has loaded, so the router has a heading to focus. */
export function PageHeader({ title, description, actions, className }: PageHeaderProps) {
  return (
    <header className={cx('mb-4 flex flex-wrap items-start justify-between gap-3', className)}>
      <div className="min-w-0">
        <h1 tabIndex={-1} className="text-2xl font-semibold text-fg">
          {title}
        </h1>
        {description !== undefined && <p className="mt-1 text-base text-fg-muted">{description}</p>}
      </div>
      {actions !== undefined && <div className="flex items-center gap-2">{actions}</div>}
    </header>
  )
}
