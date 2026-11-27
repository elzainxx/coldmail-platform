import type { ComponentProps, MouseEvent } from 'react'
import { cx } from './cx.ts'
import { Spinner } from './Spinner.tsx'

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
export type ButtonSize = 'md' | 'sm'

const BASE =
  'inline-flex shrink-0 cursor-pointer items-center justify-center gap-2 rounded-md font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-50 aria-disabled:cursor-wait aria-disabled:opacity-70'

const VARIANTS: Record<ButtonVariant, string> = {
  primary: 'bg-accent text-on-accent hover:bg-accent-hover',
  secondary: 'border border-border-strong bg-raised text-fg hover:bg-hover',
  ghost: 'bg-transparent text-fg hover:bg-hover',
  danger: 'bg-danger text-on-danger hover:bg-danger-hover',
}

const SIZES: Record<ButtonSize, string> = {
  md: 'h-8 px-3 text-sm',
  sm: 'h-7 px-2 text-xs',
}

/** The classes of a button, for a link that must look like one: <Link className={buttonClass('primary')}>. */
export function buttonClass(variant: ButtonVariant = 'secondary', size: ButtonSize = 'md'): string {
  return cx(BASE, VARIANTS[variant], SIZES[size], 'no-underline hover:no-underline')
}

export interface ButtonProps extends ComponentProps<'button'> {
  variant?: ButtonVariant
  size?: ButtonSize
  /** a request is running: the button shows a spinner, says it is busy and ignores clicks, but keeps keyboard focus (unlike `disabled`) */
  busy?: boolean
}

/** A button. `type` defaults to "button" (a button in a form never submits it by accident); pass type="submit" for the one that should. */
export function Button({ variant = 'secondary', size = 'md', busy = false, type = 'button', className, children, onClick, ...rest }: ButtonProps) {
  const handle = (e: MouseEvent<HTMLButtonElement>) => {
    if (busy) {
      e.preventDefault()
      return
    }
    onClick?.(e)
  }
  return (
    <button {...rest} type={type} className={cx(buttonClass(variant, size), className)} aria-disabled={busy || undefined} aria-busy={busy || undefined} onClick={handle}>
      {busy && <Spinner size={14} label="" />}
      {children}
    </button>
  )
}
