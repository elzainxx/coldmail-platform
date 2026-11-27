// A modal dialog, by hand: focus moves into it, Tab and Shift+Tab stay inside it, Escape closes it, and focus goes back to what had it before.
// It is rendered in a portal on <body> with role="dialog" (or "alertdialog" for a question that needs an answer) and aria-modal.
import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode, type RefObject } from 'react'
import { createPortal } from 'react-dom'
import { t } from '../copy/index.ts'
import { cx } from './cx.ts'
import { CloseIcon } from './icons.tsx'
import { IconButton } from './IconButton.tsx'

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

function focusables(root: HTMLElement): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => !el.hasAttribute('hidden') && el.getAttribute('aria-hidden') !== 'true')
}

// While any dialog is open the page behind it does not scroll.
let openCount = 0

export interface DialogProps {
  open: boolean
  /** Called for Escape, the close button and a click on the backdrop. The dialog does not close itself: the owner sets `open` to false. */
  onClose: () => void
  /** The dialog's heading and accessible name. */
  title: string
  description?: ReactNode
  children?: ReactNode
  /** Buttons at the bottom. */
  footer?: ReactNode
  /** 'alertdialog' for a question the user must answer (ConfirmDialog). */
  role?: 'dialog' | 'alertdialog'
  /** the element that takes focus when it opens (default: the first focusable one in the body or the footer) */
  initialFocusRef?: RefObject<HTMLElement | null>
  /** false: Escape, the backdrop and the close button do nothing (while a request is running) */
  dismissible?: boolean
  size?: 'sm' | 'md' | 'lg'
}

export function Dialog({ open, onClose, title, description, children, footer, role = 'dialog', initialFocusRef, dismissible = true, size = 'md' }: DialogProps) {
  const panelRef = useRef<HTMLDivElement>(null)
  const headerRef = useRef<HTMLDivElement>(null)
  const titleId = useId()
  const descId = useId()

  useEffect(() => {
    if (!open) return undefined
    const returnTo = document.activeElement instanceof HTMLElement ? document.activeElement : null
    openCount++
    document.body.classList.add('overflow-hidden')
    const panel = panelRef.current
    if (panel) {
      // the element asked for, else the first thing in the body or the footer (the close button is last choice), else the dialog itself
      const items = focusables(panel)
      const first = items.find((el) => !headerRef.current?.contains(el)) ?? items[0]
      ;(initialFocusRef?.current ?? first ?? panel).focus()
    }
    return () => {
      openCount--
      if (openCount === 0) document.body.classList.remove('overflow-hidden')
      // back to what had focus; if that is gone (the row it was in was removed), to the main area
      if (returnTo !== null && document.contains(returnTo)) returnTo.focus()
      else document.getElementById('main')?.focus()
    }
  }, [open]) // initialFocusRef is read once, when the dialog opens

  if (!open) return null

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') {
      e.stopPropagation()
      if (dismissible) onClose()
      return
    }
    if (e.key !== 'Tab') return
    const panel = panelRef.current
    if (!panel) return
    const items = focusables(panel)
    if (items.length === 0) {
      e.preventDefault()
      panel.focus()
      return
    }
    const first = items[0] as HTMLElement
    const last = items[items.length - 1] as HTMLElement
    const active = document.activeElement
    if (e.shiftKey && (active === first || active === panel || !panel.contains(active))) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && (active === last || !panel.contains(active))) {
      e.preventDefault()
      first.focus()
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-scrim/50 p-4"
      onKeyDown={onKeyDown}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && dismissible) onClose()
      }}
    >
      <div
        ref={panelRef}
        role={role}
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description === undefined ? undefined : descId}
        tabIndex={-1}
        className={cx(
          'flex max-h-full w-full flex-col rounded-lg border border-border bg-raised shadow-lg',
          size === 'sm' && 'max-w-sm',
          size === 'md' && 'max-w-lg',
          size === 'lg' && 'max-w-3xl',
        )}
      >
        <div ref={headerRef} className="flex items-start justify-between gap-3 px-4 pt-4">
          <h2 id={titleId} className="text-lg font-semibold text-fg">
            {title}
          </h2>
          {dismissible && (
            <IconButton label={t('common.dialog.close')} onClick={onClose}>
              <CloseIcon />
            </IconButton>
          )}
        </div>
        {description !== undefined && (
          <div id={descId} className="px-4 pt-2 text-base text-fg-muted">
            {description}
          </div>
        )}
        {children !== undefined && <div className="overflow-auto px-4 py-3">{children}</div>}
        {footer !== undefined && <div className="flex justify-end gap-2 border-t border-border px-4 py-3">{footer}</div>}
      </div>
    </div>,
    document.body,
  )
}
