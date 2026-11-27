import type { SelectHTMLAttributes } from 'react'
import { cx } from './cx.ts'
import { useControlA11y } from './FormField.tsx'
import { FIELD_CLASS } from './TextInput.tsx'

export type SelectProps = SelectHTMLAttributes<HTMLSelectElement>

/** A native select (keyboard and screen reader support come with the element). Children are <option>s. */
export function Select({ className, id, 'aria-describedby': describedBy, 'aria-invalid': invalid, children, ...rest }: SelectProps) {
  const a11y = useControlA11y({ id, 'aria-describedby': describedBy, 'aria-invalid': invalid })
  return (
    <select {...rest} {...a11y} className={cx(FIELD_CLASS, 'h-8', className)}>
      {children}
    </select>
  )
}
