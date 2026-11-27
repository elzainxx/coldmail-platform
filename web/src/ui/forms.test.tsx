import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Checkbox, FormField, Select, TextArea, TextInput } from './index.ts'

describe('FormField with a TextInput', () => {
  it('names the control by its label', () => {
    render(
      <FormField label="Email address">
        <TextInput type="email" />
      </FormField>,
    )
    expect(screen.getByRole('textbox', { name: 'Email address' })).toBeInTheDocument()
    expect(screen.getByLabelText('Email address')).toBe(screen.getByRole('textbox'))
  })

  it('paired control: an input outside a FormField has no name', () => {
    render(<TextInput />)
    expect(screen.getByRole('textbox')).toHaveAccessibleName('')
  })

  it('reads the hint and the error as the control\'s description (aria-describedby)', () => {
    render(
      <FormField label="Password" hint="Use 12 characters or more." error="Too short.">
        <TextInput type="password" />
      </FormField>,
    )
    const input = screen.getByLabelText('Password')
    expect(input).toHaveAccessibleDescription('Use 12 characters or more. Too short.')
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByRole('alert')).toHaveTextContent('Too short.')
  })

  it('paired control: no error, no aria-invalid and no alert; no hint, no description', () => {
    render(
      <FormField label="Name">
        <TextInput />
      </FormField>,
    )
    const input = screen.getByLabelText('Name')
    expect(input).not.toHaveAttribute('aria-invalid')
    expect(input).not.toHaveAttribute('aria-describedby')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('marks a required field and shows (optional) on an optional one', () => {
    render(
      <>
        <FormField label="A" required>
          <TextInput />
        </FormField>
        <FormField label="B" optional>
          <TextInput />
        </FormField>
      </>,
    )
    expect(screen.getByLabelText('A')).toHaveAttribute('aria-required', 'true')
    expect(screen.getByLabelText(/^B/)).not.toHaveAttribute('aria-required')
    expect(screen.getByText('(optional)')).toBeInTheDocument()
  })

  it('keeps the description a caller gave the control, in front of its own', () => {
    render(
      <>
        <p id="extra">Extra</p>
        <FormField label="Name" hint="Hint">
          <TextInput aria-describedby="extra" />
        </FormField>
      </>,
    )
    expect(screen.getByLabelText('Name')).toHaveAccessibleDescription('Extra Hint')
  })

  it('two fields get two different ids', () => {
    render(
      <>
        <FormField label="One">
          <TextInput />
        </FormField>
        <FormField label="Two">
          <TextInput />
        </FormField>
      </>,
    )
    expect(screen.getByLabelText('One').id).not.toBe(screen.getByLabelText('Two').id)
  })

  it('takes typing', () => {
    const onChange = vi.fn()
    render(
      <FormField label="Name">
        <TextInput onChange={onChange} />
      </FormField>,
    )
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Ann' } })
    expect(onChange).toHaveBeenCalledTimes(1)
  })
})

describe('TextArea, Select and Checkbox', () => {
  it('a TextArea in a field is named by the label and described by the hint', () => {
    render(
      <FormField label="Message" hint="Plain text or HTML.">
        <TextArea mono />
      </FormField>,
    )
    const area = screen.getByRole('textbox', { name: 'Message' })
    expect(area.tagName).toBe('TEXTAREA')
    expect(area).toHaveAccessibleDescription('Plain text or HTML.')
  })

  it('a Select in a field is a combobox named by the label, and its options are chosen by the browser', () => {
    render(
      <FormField label="Column" error="Pick one.">
        <Select defaultValue="b">
          <option value="a">A</option>
          <option value="b">B</option>
        </Select>
      </FormField>,
    )
    const select = screen.getByRole('combobox', { name: 'Column' })
    expect(select).toHaveValue('b')
    expect(select).toHaveAttribute('aria-invalid', 'true')
    fireEvent.change(select, { target: { value: 'a' } })
    expect(select).toHaveValue('a')
  })

  it('a Checkbox is named by its label, described by its description, and toggles by its label too', () => {
    const onChange = vi.fn()
    render(<Checkbox label="Skip invalid rows" description="They are left out for free." onChange={onChange} />)
    const box = screen.getByRole('checkbox', { name: 'Skip invalid rows' })
    expect(box).toHaveAccessibleDescription('They are left out for free.')
    expect(box).not.toBeChecked()
    fireEvent.click(screen.getByText('Skip invalid rows'))
    expect(box).toBeChecked()
    expect(onChange).toHaveBeenCalledTimes(1)
    box.focus()
    expect(box).toHaveFocus()
  })

  it('paired control: a checkbox with no Checkbox wrapper and no label has no name', () => {
    render(<input type="checkbox" />)
    expect(screen.getByRole('checkbox')).toHaveAccessibleName('')
  })
})
