import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { EmptyState, ProgressBar, Spinner, STATUS_TOKENS, StatusBadge, ToastProvider, jobStatusToken, resultStateToken, useToast } from './index.ts'

afterEach(() => {
  vi.useRealTimers()
})

describe('StatusBadge', () => {
  it('shows a WORD and a colour for each of the seven statuses', () => {
    const words: Record<string, string> = { valid: 'Valid', risky: 'Risky', invalid: 'Invalid', unknown: 'Unknown', sent: 'Sent', bounced: 'Bounced', failed: 'Failed' }
    expect(STATUS_TOKENS.length).toBe(7)
    for (const s of STATUS_TOKENS) {
      const { unmount, container } = render(<StatusBadge status={s} />)
      const badge = screen.getByText(words[s] as string)
      expect(badge, s).toBeInTheDocument()
      expect(badge.className, s).toContain(`text-status-${s}`)
      expect(badge.className, s).toContain(`bg-status-${s}-bg`)
      expect(container.textContent?.trim(), s).toBe(words[s])
      unmount()
    }
  })

  it('paired control: the seven look different from one another (colour classes are distinct)', () => {
    const classes = STATUS_TOKENS.map((s) => {
      const { container, unmount } = render(<StatusBadge status={s} />)
      const c = (container.firstElementChild as HTMLElement).className
      unmount()
      return c
    })
    expect(new Set(classes).size).toBe(7)
  })

  it('takes the word of a job or result state, but never drops the word', () => {
    render(<StatusBadge status={jobStatusToken('paused')} label="Paused" />)
    expect(screen.getByText('Paused')).toHaveAttribute('data-status', 'risky')
  })

  it('colours the states the API names the same way everywhere, and an unknown one grey', () => {
    expect(jobStatusToken('succeeded')).toBe('valid')
    expect(jobStatusToken('running')).toBe('sent')
    expect(jobStatusToken('failed')).toBe('failed')
    expect(jobStatusToken('something_new')).toBe('unknown')
    expect(resultStateToken('malformed')).toBe('invalid')
    expect(resultStateToken('bounced')).toBe('bounced')
    expect(resultStateToken('something_new')).toBe('unknown')
  })
})

describe('Spinner', () => {
  it('is a status that says Loading, and the words can be changed', () => {
    const { unmount } = render(<Spinner />)
    expect(screen.getByRole('status')).toHaveTextContent('Loading')
    unmount()
    render(<Spinner label="Checking the list" />)
    expect(screen.getByRole('status')).toHaveTextContent('Checking the list')
  })

  it('paired control: an empty label makes it silent (for a control that already says it is busy)', () => {
    render(<Spinner label="" />)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })
})

describe('EmptyState', () => {
  it('says what is missing and offers the way forward', () => {
    render(<EmptyState title="No jobs yet" description="Start with a list of addresses." action={<button type="button">New verify job</button>} />)
    expect(screen.getByText('No jobs yet')).toBeInTheDocument()
    expect(screen.getByText('Start with a list of addresses.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New verify job' })).toBeInTheDocument()
  })
})

describe('ProgressBar', () => {
  it('is a progressbar with a name and the numbers a screen reader needs', () => {
    render(<ProgressBar label="Rows checked" value={30} max={120} showValue />)
    const bar = screen.getByRole('progressbar', { name: 'Rows checked' })
    expect(bar).toHaveAttribute('aria-valuenow', '30')
    expect(bar).toHaveAttribute('aria-valuemin', '0')
    expect(bar).toHaveAttribute('aria-valuemax', '120')
    expect(screen.getByText('25%')).toBeInTheDocument()
  })

  it('keeps a value out of range and a max of zero inside the bar', () => {
    const { unmount } = render(<ProgressBar label="A" value={500} max={100} />)
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '100')
    unmount()
    render(<ProgressBar label="B" value={3} max={0} />)
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuemax', '1')
  })
})

function Notifier({ message = 'Job paused', tone }: { message?: string; tone?: 'info' | 'success' | 'error' }) {
  const toast = useToast()
  return (
    <button type="button" onClick={() => toast.notify(message, tone === undefined ? undefined : { tone })}>
      Notify
    </button>
  )
}

describe('Toast', () => {
  it('has its polite live region in the page from the start, named Notifications, empty', () => {
    render(
      <ToastProvider>
        <p>page</p>
      </ToastProvider>,
    )
    const region = screen.getByRole('region', { name: 'Notifications' })
    expect(region).toHaveAttribute('aria-live', 'polite')
    expect(region).toBeEmptyDOMElement()
  })

  it('puts a message in the region, can be dismissed with a named button, and goes by itself', () => {
    vi.useFakeTimers()
    render(
      <ToastProvider>
        <Notifier />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Notify' }))
    const region = screen.getByRole('region', { name: 'Notifications' })
    expect(region).toHaveTextContent('Job paused')
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss the notification' }))
    expect(region).not.toHaveTextContent('Job paused')

    fireEvent.click(screen.getByRole('button', { name: 'Notify' }))
    expect(region).toHaveTextContent('Job paused')
    act(() => {
      vi.advanceTimersByTime(4_000)
    })
    expect(region).toHaveTextContent('Job paused') // paired control: not yet
    act(() => {
      vi.advanceTimersByTime(1_500)
    })
    expect(region).not.toHaveTextContent('Job paused')
  })

  it('an error is an alert (announced at once) and stays longer', () => {
    vi.useFakeTimers()
    render(
      <ToastProvider>
        <Notifier message="Could not pause" tone="error" />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Notify' }))
    expect(screen.getByRole('alert')).toHaveTextContent('Could not pause')
    act(() => {
      vi.advanceTimersByTime(6_000)
    })
    expect(screen.getByRole('alert')).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(5_000)
    })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows at most four at once, newest kept', () => {
    render(
      <ToastProvider>
        <Notifier />
      </ToastProvider>,
    )
    for (let i = 0; i < 6; i++) fireEvent.click(screen.getByRole('button', { name: 'Notify' }))
    expect(screen.getAllByRole('button', { name: 'Dismiss the notification' })).toHaveLength(4)
  })

  it('useToast outside a provider is a programming error, not a silent no-op', () => {
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => render(<Notifier />)).toThrow(/ToastProvider/)
    quiet.mockRestore()
  })
})
