import { useRef, type ReactNode } from 'react'
import { t } from '../copy/index.ts'
import { Button } from './Button.tsx'
import { Dialog } from './Dialog.tsx'

export interface ConfirmDialogProps {
  open: boolean
  title: string
  /** What will happen, in a sentence the user can act on. */
  description: ReactNode
  /** The button that does it, named for what it does ("Cancel the job"), never just "OK". */
  confirmLabel: string
  /** The button that backs out (default: Cancel). Focus starts here, so Enter never confirms by accident. */
  cancelLabel?: string
  onConfirm: () => void
  onCancel: () => void
  /** a request is running: the confirm button shows it, and nothing can dismiss the dialog */
  busy?: boolean
  /** true for something that cannot be undone */
  destructive?: boolean
  children?: ReactNode
}

/** A question that needs an answer (role alertdialog). Focus starts on the safe button. */
export function ConfirmDialog({ open, title, description, confirmLabel, cancelLabel, onConfirm, onCancel, busy = false, destructive = false, children }: ConfirmDialogProps) {
  const cancelRef = useRef<HTMLButtonElement>(null)
  return (
    <Dialog
      open={open}
      role="alertdialog"
      title={title}
      description={description}
      onClose={onCancel}
      dismissible={!busy}
      initialFocusRef={cancelRef}
      size="sm"
      footer={
        <>
          <Button ref={cancelRef} onClick={onCancel} disabled={busy}>
            {cancelLabel ?? t('common.cancel')}
          </Button>
          <Button variant={destructive ? 'danger' : 'primary'} onClick={onConfirm} busy={busy}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      {children}
    </Dialog>
  )
}
