import type { InputHTMLAttributes } from 'react'
import { cx } from './cx.ts'
import { useControlA11y } from './FormField.tsx'

export const FIELD_CLASS =
  'block w-full rounded-md border border-border-strong bg-raised px-2 text-base text-fg placeholder:text-fg-muted disabled:cursor-not-allowed disabled:opacity-60 aria-[invalid=true]:border-2 aria-[invalid=true]:border-danger'

export type TextInputProps = InputHTMLAttributes<HTMLInputElement>

/** A single-line input (text, email, password, search, number, url...). Put it inside a FormField: the label, hint and error come from there. */
export function TextInput({ type = 'text', className, id, 'aria-describedby': describedBy, 'aria-invalid': invalid, ...rest }: TextInputProps) {
  const a11y = useControlA11y({ id, 'aria-describedby': describedBy, 'aria-invalid': invalid })
  return <input {...rest} {...a11y} type={type} className={cx(FIELD_CLASS, 'h-8', className)} />
}
