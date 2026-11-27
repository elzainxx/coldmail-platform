// A labelled field: the label, an optional hint, an optional error, and the control, tied together with ids so a screen reader reads
// "Email address, edit text, required: <hint>. <error>". The control is any of TextInput, TextArea, Select, Checkbox placed inside: they read the ids
// from this component (no prop to forward), and a control outside a FormField must carry its own aria-label.
import { createContext, useContext, useId, type AriaAttributes, type ReactNode } from 'react'
import { t } from '../copy/index.ts'
import { cx } from './cx.ts'

interface FieldContextValue {
  id: string
  describedBy: string | undefined
  invalid: boolean
  required: boolean
}

const FieldContext = createContext<FieldContextValue | null>(null)

export interface ControlA11y {
  id: string | undefined
  'aria-describedby': string | undefined
  'aria-invalid': true | undefined
  'aria-required': true | undefined
}

/** What a control must put on its element to be part of the field it sits in (nothing when it sits in none). */
export function useControlA11y(own: {
  id?: string | undefined
  'aria-describedby'?: string | undefined
  'aria-invalid'?: AriaAttributes['aria-invalid']
}): ControlA11y {
  const f = useContext(FieldContext)
  const describedBy = [own['aria-describedby'], f?.describedBy].filter((s): s is string => typeof s === 'string' && s !== '').join(' ')
  const ownInvalid = own['aria-invalid'] !== undefined && own['aria-invalid'] !== false && own['aria-invalid'] !== 'false'
  return {
    id: own.id ?? f?.id,
    'aria-describedby': describedBy === '' ? undefined : describedBy,
    'aria-invalid': ownInvalid || f?.invalid ? true : undefined,
    'aria-required': f?.required ? true : undefined,
  }
}

export interface FormFieldProps {
  label: string
  hint?: ReactNode
  /** The error text; its presence marks the control invalid. Already a sentence of the catalogue. */
  error?: string | null
  required?: boolean
  /** shows "(optional)" after the label */
  optional?: boolean
  className?: string
  children: ReactNode
}

export function FormField({ label, hint, error, required = false, optional = false, className, children }: FormFieldProps) {
  const id = useId()
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  const hasError = typeof error === 'string' && error !== ''
  const hasHint = hint !== undefined && hint !== null && hint !== false && hint !== ''
  const describedBy = [hasHint ? hintId : '', hasError ? errorId : ''].filter(Boolean).join(' ')
  return (
    <FieldContext value={{ id, describedBy: describedBy === '' ? undefined : describedBy, invalid: hasError, required }}>
      <div className={cx('flex flex-col gap-1', className)}>
        <label htmlFor={id} className="text-sm font-medium text-fg">
          {label}
          {optional && <span className="ml-1 font-normal text-fg-muted">({t('common.optional')})</span>}
        </label>
        {children}
        {hasHint && (
          <p id={hintId} className="text-xs text-fg-muted">
            {hint}
          </p>
        )}
        {hasError && (
          <p id={errorId} role="alert" className="text-xs font-medium text-danger">
            {error}
          </p>
        )}
      </div>
    </FieldContext>
  )
}
