// The steps of a send job: a list that says where the user is (the current one is marked with aria-current and in bold: colour is not the only signal), the
// frame of one step (its heading takes the focus when the user moves to it, so a keyboard or screen reader user lands at the top of the new step), and the
// Back and Next buttons at the bottom of it.
import { useEffect, useId, useRef, type ReactNode } from 'react'
import { t } from '../../copy/index.ts'
import { Button, cx } from '../../ui/index.ts'

export interface StepInfo {
  id: string
  label: string
}

export function Stepper({ steps, current }: { steps: readonly StepInfo[]; current: number }) {
  return (
    <nav aria-label={t('newjob.step.label')}>
      <ol className="m-0 flex list-none flex-wrap gap-2 p-0">
        {steps.map((step, i) => (
          <li
            key={step.id}
            aria-current={i === current ? 'step' : undefined}
            className={cx(
              'flex items-center gap-2 rounded-md border px-3 py-1 text-sm',
              i === current ? 'border-accent bg-raised font-semibold text-fg' : 'border-border text-fg-muted',
            )}
          >
            <span aria-hidden="true">{i + 1}</span>
            <span>{step.label}</span>
            {i < current && <span className="sr-only">{t('newjob.step.done')}</span>}
          </li>
        ))}
      </ol>
    </nav>
  )
}

export interface StepFrameProps {
  /** 1-based */
  index: number
  total: number
  title: string
  /** take the focus when it appears (every step after the first one the user sees) */
  focusOnShow: boolean
  children: ReactNode
}

export function StepFrame({ index, total, title, focusOnShow, children }: StepFrameProps) {
  const id = useId()
  const heading = useRef<HTMLHeadingElement>(null)
  useEffect(() => {
    if (focusOnShow) heading.current?.focus()
  }, [focusOnShow])
  return (
    <section aria-labelledby={id} className="flex flex-col gap-4 rounded-lg border border-border bg-raised p-4 shadow-sm">
      <h2 id={id} ref={heading} tabIndex={-1} className="text-lg font-semibold text-fg">
        {t('newjob.step.heading', { n: index, total, title })}
      </h2>
      {children}
    </section>
  )
}

export interface StepNavProps {
  onBack?: () => void
  onNext: () => void
  nextLabel: string
  nextDisabled?: boolean
  /** what stops the next step, in words (shown beside the button) */
  nextHint?: string | null
}

export function StepNav({ onBack, onNext, nextLabel, nextDisabled = false, nextHint }: StepNavProps) {
  return (
    <div className="flex flex-wrap items-center gap-2 border-t border-border pt-3">
      {onBack !== undefined && <Button onClick={onBack}>{t('common.back')}</Button>}
      <Button variant="primary" onClick={onNext} disabled={nextDisabled}>
        {nextLabel}
      </Button>
      {nextDisabled && nextHint !== undefined && nextHint !== null && <span className="text-sm text-fg-muted">{nextHint}</span>}
    </div>
  )
}
