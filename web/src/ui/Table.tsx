import type { ReactNode } from 'react'
import { t } from '../copy/index.ts'
import { cx } from './cx.ts'
import { Spinner } from './Spinner.tsx'

export interface Column<T> {
  /** unique within the table */
  key: string
  header: ReactNode
  cell: (row: T) => ReactNode
  align?: 'left' | 'right'
  /** the cell that names the row (a screen reader reads it before the others): rendered as <th scope="row"> */
  rowHeader?: boolean
  className?: string
}

export interface TableProps<T> {
  /** What the table is: read by a screen reader before the rows. Required: a table without a caption is not accepted. */
  caption: string
  /** keep the caption for screen readers only */
  captionHidden?: boolean
  columns: readonly Column<T>[]
  rows: readonly T[]
  rowKey: (row: T) => string
  /** the data-testid of a row (the conventions list: job-row-<id>, results-row-<ordinal>, suppression-row-<n>, account-row-<id>) */
  rowTestId?: (row: T) => string | undefined
  /** shown in the table when there are no rows (an EmptyState) */
  empty?: ReactNode
  /** a request is running: the table says it is busy and shows a spinner when there are no rows yet */
  loading?: boolean
  /** a Tailwind max-height class for the scrolling area; the header stays in view while the rows scroll (default max-h-[70vh]) */
  maxHeightClass?: string
}

/** A data table with a caption, a sticky header, an empty state and a loading state. The scrolling area is a focusable region, so the keyboard can scroll it. */
export function Table<T>({ caption, captionHidden = false, columns, rows, rowKey, rowTestId, empty, loading = false, maxHeightClass = 'max-h-[70vh]' }: TableProps<T>) {
  const showEmpty = rows.length === 0 && !loading
  return (
    <div
      role="region"
      aria-label={caption}
      tabIndex={0}
      className={cx('overflow-auto rounded-lg border border-border bg-raised', maxHeightClass)}
    >
      <table className="w-full border-separate border-spacing-0 text-left text-base" aria-busy={loading || undefined}>
        <caption className={cx('px-3 py-2 text-left text-sm font-medium text-fg', captionHidden && 'sr-only')}>{caption}</caption>
        <thead>
          <tr>
            {columns.map((c) => (
              <th
                key={c.key}
                scope="col"
                className={cx(
                  'sticky top-0 z-10 border-b border-border bg-sunken px-3 py-1.5 text-xs font-medium whitespace-nowrap text-fg-muted',
                  c.align === 'right' && 'text-right',
                )}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={rowKey(row)} data-testid={rowTestId?.(row)} className="hover:bg-hover">
              {columns.map((c) =>
                c.rowHeader ? (
                  <th key={c.key} scope="row" className={cx('border-b border-border px-3 py-1.5 font-medium', c.align === 'right' && 'text-right', c.className)}>
                    {c.cell(row)}
                  </th>
                ) : (
                  <td key={c.key} className={cx('border-b border-border px-3 py-1.5', c.align === 'right' && 'text-right', c.className)}>
                    {c.cell(row)}
                  </td>
                ),
              )}
            </tr>
          ))}
          {loading && rows.length === 0 && (
            <tr>
              <td colSpan={columns.length} className="px-3 py-8 text-center">
                <Spinner label={t('common.loading')} />
              </td>
            </tr>
          )}
          {showEmpty && (
            <tr>
              <td colSpan={columns.length} className="px-3 py-8 text-center">
                {empty ?? <p className="text-fg-muted">{t('common.empty.title')}</p>}
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}
