// A group of radio buttons the way the screens of this area use them: a fieldset with a legend (the question), and options that each have a label and,
// when it helps, a line of explanation read after the label. Native radio inputs, so the arrow keys, the focus and the screen reader all come with them.
import { useId, type InputHTMLAttributes, type ReactNode } from 'react'
import { cx } from '../../ui/index.ts'

export interface ChoiceGroupProps {
  /** the question: the group's accessible name */
  legend: string
  hint?: ReactNode
  /** a sentence that says the group must be answered; marks it invalid */
  error?: string | null
  children: ReactNode
  className?: string
}

export function ChoiceGroup({ legend, hint, error, children, className }: ChoiceGroupProps) {
  const id = useId()
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  const hasError = typeof error === 'string' && error !== ''
  const described = [hint === undefined ? '' : hintId, hasError ? errorId : ''].filter(Boolean).join(' ')
  return (
    <fieldset aria-describedby={described === '' ? undefined : described} className={cx('m-0 flex min-w-0 flex-col gap-2 border-0 p-0', className)}>
      <legend className="mb-1 p-0 text-sm font-medium text-fg">{legend}</legend>
      {hint !== undefined && (
        <p id={hintId} className="text-xs text-fg-muted">
          {hint}
        </p>
      )}
      {children}
      {hasError && (
        <p id={errorId} role="alert" className="text-xs font-medium text-danger">
          {error}
        </p>
      )}
    </fieldset>
  )
}

export interface ChoiceProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type' | 'children'> {
  label: ReactNode
  description?: ReactNode
}

export function Choice({ label, description, className, id, ...rest }: ChoiceProps) {
  const own = useId()
  const inputId = id ?? own
  const descId = `${own}-desc`
  const hasDescription = description !== undefined && description !== null && description !== false
  return (
    <div className={cx('flex items-start gap-2', className)}>
      <input
        {...rest}
        id={inputId}
        type="radio"
        aria-describedby={hasDescription ? descId : undefined}
        className="mt-0.5 size-4 shrink-0 cursor-pointer accent-accent disabled:cursor-not-allowed"
      />
      <div className="flex flex-col">
        <label htmlFor={inputId} className="cursor-pointer text-base text-fg">
          {label}
        </label>
        {hasDescription && (
          <span id={descId} className="text-xs text-fg-muted">
            {description}
          </span>
        )}
      </div>
    </div>
  )
}
