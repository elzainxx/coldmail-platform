import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Button, IconButton, CloseIcon } from './index.ts'

describe('Button', () => {
  it('is a button named by its text, and takes keyboard focus', () => {
    render(<Button>Save changes</Button>)
    const b = screen.getByRole('button', { name: 'Save changes' })
    b.focus()
    expect(b).toHaveFocus()
  })

  it('does not submit a form by accident: type defaults to button', () => {
    const onSubmit = vi.fn((e: { preventDefault: () => void }) => e.preventDefault())
    render(
      <form onSubmit={onSubmit}>
        <Button>Plain</Button>
      </form>,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Plain' }))
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('paired control: type="submit" does submit it', () => {
    const onSubmit = vi.fn((e: { preventDefault: () => void }) => e.preventDefault())
    render(
      <form onSubmit={onSubmit}>
        <Button type="submit">Send</Button>
      </form>,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(onSubmit).toHaveBeenCalledTimes(1)
  })

  it('busy: says so, ignores clicks, and keeps keyboard focus (it is not disabled)', () => {
    const onClick = vi.fn()
    render(
      <Button busy onClick={onClick}>
        Confirm
      </Button>,
    )
    const b = screen.getByRole('button', { name: 'Confirm' })
    expect(b).toHaveAttribute('aria-busy', 'true')
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).not.toBeDisabled()
    b.focus()
    expect(b).toHaveFocus()
    fireEvent.click(b)
    expect(onClick).not.toHaveBeenCalled()
  })

  it('paired control: not busy, a click is delivered', () => {
    const onClick = vi.fn()
    render(<Button onClick={onClick}>Confirm</Button>)
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))
    expect(onClick).toHaveBeenCalledTimes(1)
  })

  it('disabled is disabled', () => {
    render(<Button disabled>Nope</Button>)
    expect(screen.getByRole('button', { name: 'Nope' })).toBeDisabled()
  })

  it('has four variants that look different', () => {
    const { container } = render(
      <>
        <Button variant="primary">a</Button>
        <Button variant="secondary">b</Button>
        <Button variant="ghost">c</Button>
        <Button variant="danger">d</Button>
      </>,
    )
    const classes = [...container.querySelectorAll('button')].map((b) => b.className)
    expect(new Set(classes).size).toBe(4)
  })
})

describe('IconButton', () => {
  it('is named by its required label (an icon alone is not a name)', () => {
    render(
      <IconButton label="Close the dialog">
        <CloseIcon />
      </IconButton>,
    )
    const b = screen.getByRole('button', { name: 'Close the dialog' })
    expect(b).toHaveAttribute('title', 'Close the dialog')
    expect(b.querySelector('svg')).toHaveAttribute('aria-hidden', 'true')
  })

  it('paired control: a plain button holding only an icon has no accessible name', () => {
    render(
      <button type="button">
        <CloseIcon />
      </button>,
    )
    expect(screen.getByRole('button')).toHaveAccessibleName('')
  })

  it('does not accept a missing label (checked by tsc)', () => {
    // @ts-expect-error label is required: an icon button without a name must not compile
    render(<IconButton>x</IconButton>)
  })

  it('is operated like a button', () => {
    const onClick = vi.fn()
    render(
      <IconButton label="Copy" onClick={onClick}>
        <CloseIcon />
      </IconButton>,
    )
    const b = screen.getByRole('button', { name: 'Copy' })
    b.focus()
    expect(b).toHaveFocus()
    fireEvent.click(b)
    expect(onClick).toHaveBeenCalledTimes(1)
  })
})
