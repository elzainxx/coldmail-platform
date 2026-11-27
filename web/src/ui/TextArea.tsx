import type { TextareaHTMLAttributes } from 'react'
import { cx } from './cx.ts'
import { useControlA11y } from './FormField.tsx'
import { FIELD_CLASS } from './TextInput.tsx'

export interface TextAreaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  /** `mono` for a template or any text where the characters matter */
  mono?: boolean
}

export function TextArea({ mono = false, rows = 4, className, id, 'aria-describedby': describedBy, 'aria-invalid': invalid, ...rest }: TextAreaProps) {
  const a11y = useControlA11y({ id, 'aria-describedby': describedBy, 'aria-invalid': invalid })
  return <textarea {...rest} {...a11y} rows={rows} className={cx(FIELD_CLASS, 'py-1.5', mono && 'font-mono text-sm', className)} />
}
