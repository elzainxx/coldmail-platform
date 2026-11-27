import type { ComponentProps, ReactNode } from 'react'
import { cx } from './cx.ts'

export interface IconButtonProps extends Omit<ComponentProps<'button'>, 'aria-label' | 'children'> {
  /** What the button does, in words: it is the accessible name (and the tooltip). Required: an icon alone is not a name. */
  label: string
  /** The icon: decorative (aria-hidden). */
  children: ReactNode
  variant?: 'ghost' | 'secondary'
}

export function IconButton({ label, children, variant = 'ghost', type = 'button', className, ...rest }: IconButtonProps) {
  return (
    <button
      {...rest}
      type={type}
      aria-label={label}
      title={label}
      className={cx(
        'inline-flex size-8 shrink-0 cursor-pointer items-center justify-center rounded-md text-fg transition-colors disabled:cursor-not-allowed disabled:opacity-50',
        variant === 'secondary' ? 'border border-border-strong bg-raised hover:bg-hover' : 'bg-transparent hover:bg-hover',
        className,
      )}
    >
      {children}
    </button>
  )
}
