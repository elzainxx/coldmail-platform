// Cursor pagination. The API gives only the cursor of the NEXT page, so "previous" is a stack of the cursors already visited:
//
//   const pager = useCursorPager()
//   const page = await jobsApi.list({ cursor: pager.cursor })          // cursor is undefined on the first page
//   <Pagination page={pager.page} hasPrevious={pager.hasPrevious} hasNext={page.next_cursor !== null}
//               onPrevious={pager.goPrevious} onNext={() => pager.goNext(page.next_cursor)} />
import { useCallback, useState } from 'react'
import { t } from '../copy/index.ts'
import { Button } from './Button.tsx'
import { cx } from './cx.ts'

export interface CursorPager {
  /** the cursor to ask the current page with (undefined for the first) */
  cursor: string | undefined
  /** 1 for the first page */
  page: number
  hasPrevious: boolean
  /** go to the next page, given the cursor the current page's answer gave */
  goNext: (nextCursor: string | null) => void
  goPrevious: () => void
  /** back to the first page (after a filter or a search changed) */
  reset: () => void
}

export function useCursorPager(): CursorPager {
  const [stack, setStack] = useState<Array<string | undefined>>([undefined])
  const goNext = useCallback((next: string | null) => {
    if (next !== null) setStack((s) => [...s, next])
  }, [])
  const goPrevious = useCallback(() => setStack((s) => (s.length > 1 ? s.slice(0, -1) : s)), [])
  const reset = useCallback(() => setStack([undefined]), [])
  return { cursor: stack[stack.length - 1], page: stack.length, hasPrevious: stack.length > 1, goNext, goPrevious, reset }
}

export interface PaginationProps {
  hasPrevious: boolean
  hasNext: boolean
  onPrevious: () => void
  onNext: () => void
  /** shows "Page N" */
  page?: number
  /** a page is loading */
  busy?: boolean
  className?: string
}

export function Pagination({ hasPrevious, hasNext, onPrevious, onNext, page, busy = false, className }: PaginationProps) {
  return (
    <nav aria-label={t('common.pagination.label')} className={cx('flex items-center justify-end gap-2', className)}>
      {page !== undefined && (
        <span className="mr-2 text-sm text-fg-muted" aria-live="polite">
          {t('common.pagination.page', { n: page })}
        </span>
      )}
      <Button size="sm" disabled={!hasPrevious || busy} onClick={onPrevious}>
        {t('common.pagination.previous')}
      </Button>
      <Button size="sm" disabled={!hasNext || busy} onClick={onNext}>
        {t('common.pagination.next')}
      </Button>
    </nav>
  )
}
