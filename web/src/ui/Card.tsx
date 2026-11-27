import { useId, type ReactNode } from 'react'
import { cx } from './cx.ts'

export interface CardProps {
  /** A heading (h2) for the card; the card is then a labelled region. */
  title?: string
  /** Buttons or links at the right of the title. */
  actions?: ReactNode
  className?: string
  /** padding of the body (default true) */
  padded?: boolean
  children: ReactNode
}

export function Card({ title, actions, className, padded = true, children }: CardProps) {
  const id = useId()
  return (
    <section aria-labelledby={title === undefined ? undefined : id} className={cx('rounded-lg border border-border bg-raised shadow-sm', className)}>
      {(title !== undefined || actions !== undefined) && (
        <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-2.5">
          {title !== undefined && (
            <h2 id={id} className="text-lg font-semibold text-fg">
              {title}
            </h2>
          )}
          {actions !== undefined && <div className="flex items-center gap-2">{actions}</div>}
        </div>
      )}
      <div className={cx(padded && 'p-4')}>{children}</div>
    </section>
  )
}
