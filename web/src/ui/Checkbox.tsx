import { useId, type InputHTMLAttributes, type ReactNode } from 'react'
import { cx } from './cx.ts'
import { useControlA11y } from './FormField.tsx'

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type' | 'children'> {
  /** The words next to the box: they are the accessible name. */
  label: ReactNode
  /** Extra explanation under the label, read after it. */
  description?: ReactNode
}

export function Checkbox({ label, description, className, id, 'aria-describedby': describedBy, 'aria-invalid': invalid, ...rest }: CheckboxProps) {
  const own = useId()
  const descId = `${own}-desc`
  const hasDescription = description !== undefined && description !== null && description !== false
  const a11y = useControlA11y({
    id: id ?? own,
    'aria-describedby': [describedBy, hasDescription ? descId : ''].filter(Boolean).join(' ') || undefined,
    'aria-invalid': invalid,
  })
  return (
    <div className={cx('flex items-start gap-2', className)}>
      <input {...rest} {...a11y} type="checkbox" className="mt-0.5 size-4 shrink-0 cursor-pointer accent-accent disabled:cursor-not-allowed" />
      <div className="flex flex-col">
        <label htmlFor={a11y.id} className="cursor-pointer text-base text-fg">
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
