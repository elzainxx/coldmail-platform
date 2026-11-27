import { fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { Button, ConfirmDialog, Dialog } from './index.ts'

function Harness({ dismissible = true, onClose = () => {} }: { dismissible?: boolean; onClose?: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open it
      </button>
      <Dialog
        open={open}
        title="Edit the name"
        dismissible={dismissible}
        onClose={() => {
          onClose()
          setOpen(false)
        }}
        footer={<Button onClick={() => setOpen(false)}>Done</Button>}
      >
        <input aria-label="Name" />
        <input aria-label="Nickname" />
      </Dialog>
    </>
  )
}

function open() {
  const trigger = screen.getByRole('button', { name: 'Open it' })
  trigger.focus()
  fireEvent.click(trigger)
  return trigger
}

describe('Dialog', () => {
  it('is a modal dialog named by its title, and is not in the page until opened', () => {
    render(<Harness />)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    open()
    const dialog = screen.getByRole('dialog', { name: 'Edit the name' })
    expect(dialog).toHaveAttribute('aria-modal', 'true')
  })

  it('moves focus into it: the first field, not the close button', () => {
    render(<Harness />)
    open()
    expect(screen.getByRole('textbox', { name: 'Name' })).toHaveFocus()
  })

  it('Escape closes it', () => {
    const onClose = vi.fn()
    render(<Harness onClose={onClose} />)
    open()
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Name' }), { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('paired control: when it is not dismissible, Escape and the backdrop do nothing and there is no close button', () => {
    const onClose = vi.fn()
    render(<Harness dismissible={false} onClose={onClose} />)
    open()
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Name' }), { key: 'Escape' })
    fireEvent.mouseDown(screen.getByRole('dialog').parentElement as HTMLElement)
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Close the dialog' })).not.toBeInTheDocument()
  })

  it('traps Tab: from the last control to the first, and Shift+Tab from the first to the last', () => {
    render(<Harness />)
    open()
    const first = screen.getByRole('button', { name: 'Close the dialog' })
    const last = screen.getByRole('button', { name: 'Done' })

    last.focus()
    const forward = fireEvent.keyDown(last, { key: 'Tab' })
    expect(forward).toBe(false) // preventDefault was called: the browser does not move focus out
    expect(first).toHaveFocus()

    const back = fireEvent.keyDown(first, { key: 'Tab', shiftKey: true })
    expect(back).toBe(false)
    expect(last).toHaveFocus()
  })

  it('paired control: Tab from a control in the middle is left to the browser', () => {
    render(<Harness />)
    open()
    const middle = screen.getByRole('textbox', { name: 'Name' })
    middle.focus()
    expect(fireEvent.keyDown(middle, { key: 'Tab' })).toBe(true)
  })

  it('a click on the backdrop closes it; a click inside does not', () => {
    const onClose = vi.fn()
    render(<Harness onClose={onClose} />)
    open()
    fireEvent.mouseDown(screen.getByRole('textbox', { name: 'Name' }))
    expect(onClose).not.toHaveBeenCalled()
    fireEvent.mouseDown(screen.getByRole('dialog').parentElement as HTMLElement)
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('gives focus back to what had it, when it closes', () => {
    render(<Harness />)
    const trigger = open()
    fireEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })

  it('paired control: if that element is gone, focus goes to the main area, not to the page body', () => {
    function Gone() {
      const [open, setOpen] = useState(false)
      const [hasTrigger, setHasTrigger] = useState(true)
      return (
        <main id="main" tabIndex={-1}>
          {hasTrigger && (
            <button type="button" onClick={() => setOpen(true)}>
              Row action
            </button>
          )}
          <Dialog
            open={open}
            title="Sure?"
            onClose={() => {
              setHasTrigger(false)
              setOpen(false)
            }}
          >
            <input aria-label="Reason" />
          </Dialog>
        </main>
      )
    }
    render(<Gone />)
    const trigger = screen.getByRole('button', { name: 'Row action' })
    trigger.focus()
    fireEvent.click(trigger)
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Reason' }), { key: 'Escape' })
    expect(document.getElementById('main')).toHaveFocus()
  })

  it('stops the page behind it from scrolling while open, and only then', () => {
    render(<Harness />)
    expect(document.body).not.toHaveClass('overflow-hidden')
    open()
    expect(document.body).toHaveClass('overflow-hidden')
    fireEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(document.body).not.toHaveClass('overflow-hidden')
  })
})

describe('ConfirmDialog', () => {
  function Confirm({ busy = false, onConfirm = () => {}, onCancel = () => {} }: { busy?: boolean; onConfirm?: () => void; onCancel?: () => void }) {
    return (
      <ConfirmDialog
        open
        title="Cancel this job?"
        description="Rows already sent stay sent."
        confirmLabel="Cancel the job"
        cancelLabel="Keep it"
        destructive
        busy={busy}
        onConfirm={onConfirm}
        onCancel={onCancel}
      />
    )
  }

  it('is an alert dialog with a name and a description, and focus starts on the safe button', () => {
    render(<Confirm />)
    const dialog = screen.getByRole('alertdialog', { name: 'Cancel this job?' })
    expect(dialog).toHaveAccessibleDescription('Rows already sent stay sent.')
    expect(screen.getByRole('button', { name: 'Keep it' })).toHaveFocus()
  })

  it('confirming and cancelling call their own handler', () => {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(<Confirm onConfirm={onConfirm} onCancel={onCancel} />)
    fireEvent.click(screen.getByRole('button', { name: 'Cancel the job' }))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Keep it' }))
    expect(onCancel).toHaveBeenCalledTimes(1)
  })

  it('Escape cancels', () => {
    const onCancel = vi.fn()
    render(<Confirm onCancel={onCancel} />)
    fireEvent.keyDown(screen.getByRole('button', { name: 'Keep it' }), { key: 'Escape' })
    expect(onCancel).toHaveBeenCalledTimes(1)
  })

  it('while busy: the confirm button says so and ignores clicks, and nothing can dismiss the dialog', () => {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(<Confirm busy onConfirm={onConfirm} onCancel={onCancel} />)
    const confirm = screen.getByRole('button', { name: 'Cancel the job' })
    expect(confirm).toHaveAttribute('aria-busy', 'true')
    fireEvent.click(confirm)
    fireEvent.keyDown(confirm, { key: 'Escape' })
    expect(onConfirm).not.toHaveBeenCalled()
    expect(onCancel).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Keep it' })).toBeDisabled()
  })
})
