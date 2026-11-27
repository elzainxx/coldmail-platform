import { act, fireEvent, render, renderHook, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Card, CodeBlock, EmptyState, PageHeader, Pagination, Table, Tabs, useCursorPager, type Column } from './index.ts'

describe('PageHeader', () => {
  it('is the one h1 of the page, focusable by script only (the router focuses it)', () => {
    render(<PageHeader title="Jobs" description="Everything you have started." actions={<button type="button">New</button>} />)
    const h1 = screen.getByRole('heading', { level: 1, name: 'Jobs' })
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
    expect(h1).toHaveAttribute('tabindex', '-1')
    h1.focus()
    expect(h1).toHaveFocus()
    expect(screen.getByRole('button', { name: 'New' })).toBeInTheDocument()
  })
})

describe('Card', () => {
  it('is a region named by its heading when it has a title', () => {
    render(
      <Card title="Balance">
        <p>100 credits</p>
      </Card>,
    )
    expect(screen.getByRole('region', { name: 'Balance' })).toHaveTextContent('100 credits')
    expect(screen.getByRole('heading', { level: 2, name: 'Balance' })).toBeInTheDocument()
  })

  it('paired control: without a title it is not announced as a region', () => {
    render(
      <Card>
        <p>x</p>
      </Card>,
    )
    expect(screen.queryByRole('region')).not.toBeInTheDocument()
  })
})

describe('Tabs', () => {
  const tabs = [
    { id: 'a', label: 'Summary', panel: <p>summary panel</p> },
    { id: 'b', label: 'Results', panel: <p>results panel</p> },
    { id: 'c', label: 'History', panel: <p>history panel</p> },
  ]

  it('has a named tablist, tabs, one selected, and a panel named by its tab', () => {
    render(<Tabs label="Job" tabs={tabs} />)
    expect(screen.getByRole('tablist', { name: 'Job' })).toBeInTheDocument()
    expect(screen.getAllByRole('tab')).toHaveLength(3)
    expect(screen.getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel', { name: 'Summary' })).toHaveTextContent('summary panel')
    expect(screen.queryByText('results panel')).not.toBeInTheDocument()
  })

  it('has one tab stop: only the selected tab is in the tab order', () => {
    render(<Tabs label="Job" tabs={tabs} />)
    expect(screen.getByRole('tab', { name: 'Summary' })).toHaveAttribute('tabindex', '0')
    expect(screen.getByRole('tab', { name: 'Results' })).toHaveAttribute('tabindex', '-1')
  })

  it('arrow keys, Home and End move focus and select, wrapping at the ends', () => {
    render(<Tabs label="Job" tabs={tabs} />)
    const summary = screen.getByRole('tab', { name: 'Summary' })
    summary.focus()
    fireEvent.keyDown(summary, { key: 'ArrowRight' })
    expect(screen.getByRole('tab', { name: 'Results' })).toHaveFocus()
    expect(screen.getByRole('tab', { name: 'Results' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel')).toHaveTextContent('results panel')
    fireEvent.keyDown(screen.getByRole('tab', { name: 'Results' }), { key: 'End' })
    expect(screen.getByRole('tab', { name: 'History' })).toHaveFocus()
    fireEvent.keyDown(screen.getByRole('tab', { name: 'History' }), { key: 'ArrowRight' })
    expect(screen.getByRole('tab', { name: 'Summary' })).toHaveFocus() // wrapped
    fireEvent.keyDown(screen.getByRole('tab', { name: 'Summary' }), { key: 'ArrowLeft' })
    expect(screen.getByRole('tab', { name: 'History' })).toHaveFocus() // wrapped back
    fireEvent.keyDown(screen.getByRole('tab', { name: 'History' }), { key: 'Home' })
    expect(screen.getByRole('tab', { name: 'Summary' })).toHaveFocus()
  })

  it('paired control: other keys change nothing', () => {
    render(<Tabs label="Job" tabs={tabs} />)
    const summary = screen.getByRole('tab', { name: 'Summary' })
    summary.focus()
    fireEvent.keyDown(summary, { key: 'a' })
    expect(summary).toHaveAttribute('aria-selected', 'true')
  })

  it('a click selects, and a controlled Tabs reports instead of changing itself', () => {
    const onChange = vi.fn()
    render(<Tabs label="Job" tabs={tabs} value="a" onChange={onChange} />)
    fireEvent.click(screen.getByRole('tab', { name: 'History' }))
    expect(onChange).toHaveBeenCalledWith('c')
    expect(screen.getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true')
  })
})

describe('CodeBlock', () => {
  it('is a focusable region with a name, so the keyboard can scroll it, and shows the text as text', () => {
    render(<CodeBlock label="The message">{'<script>alert(1)</script> Hello'}</CodeBlock>)
    const block = screen.getByRole('region', { name: 'The message' })
    expect(block).toHaveAttribute('tabindex', '0')
    expect(block).toHaveTextContent('<script>alert(1)</script> Hello')
    expect(block.querySelector('script')).toBeNull()
  })

  it('copies on request and says so; without copyable there is no Copy button', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const { unmount } = render(<CodeBlock copyable>some text</CodeBlock>)
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Copy' }))
    })
    expect(writeText).toHaveBeenCalledWith('some text')
    expect(screen.getByRole('status')).toHaveTextContent('Copied')
    unmount()
    render(<CodeBlock>x</CodeBlock>)
    expect(screen.queryByRole('button', { name: 'Copy' })).not.toBeInTheDocument()
  })
})

interface Row {
  id: string
  name: string
  n: number
}

const COLUMNS: Column<Row>[] = [
  { key: 'name', header: 'Name', cell: (r) => r.name, rowHeader: true },
  { key: 'n', header: 'Rows', cell: (r) => r.n, align: 'right' },
]
const ROWS: Row[] = [
  { id: 'a1', name: 'March list', n: 120 },
  { id: 'b2', name: 'April list', n: 40 },
]

describe('Table', () => {
  it('is a table named by its caption, with column headers, row headers and the data', () => {
    render(<Table caption="Your jobs" columns={COLUMNS} rows={ROWS} rowKey={(r) => r.id} rowTestId={(r) => `job-row-${r.id}`} />)
    const table = screen.getByRole('table', { name: 'Your jobs' })
    expect(within(table).getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Name', 'Rows'])
    expect(within(table).getByRole('rowheader', { name: 'March list' })).toBeInTheDocument()
    expect(screen.getByTestId('job-row-b2')).toHaveTextContent('April list')
    expect(screen.getByTestId('job-row-b2')).toHaveTextContent('40')
  })

  it('has a sticky header and a scrolling area the keyboard can reach', () => {
    render(<Table caption="Your jobs" columns={COLUMNS} rows={ROWS} rowKey={(r) => r.id} />)
    for (const h of screen.getAllByRole('columnheader')) expect(h.className).toContain('sticky')
    expect(screen.getByRole('region', { name: 'Your jobs' })).toHaveAttribute('tabindex', '0')
  })

  it('shows the empty state in the table when there are no rows', () => {
    render(<Table caption="Your jobs" columns={COLUMNS} rows={[]} rowKey={(r) => r.id} empty={<EmptyState title="No jobs yet" />} />)
    expect(within(screen.getByRole('table')).getByText('No jobs yet')).toBeInTheDocument()
  })

  it('paired control: with rows there is no empty state', () => {
    render(<Table caption="Your jobs" columns={COLUMNS} rows={ROWS} rowKey={(r) => r.id} empty={<EmptyState title="No jobs yet" />} />)
    expect(screen.queryByText('No jobs yet')).not.toBeInTheDocument()
  })

  it('says it is busy while loading, and shows a spinner instead of the empty state', () => {
    render(<Table caption="Your jobs" columns={COLUMNS} rows={[]} loading rowKey={(r) => r.id} empty={<EmptyState title="No jobs yet" />} />)
    expect(screen.getByRole('table')).toHaveAttribute('aria-busy', 'true')
    expect(screen.getByRole('status')).toHaveTextContent('Loading')
    expect(screen.queryByText('No jobs yet')).not.toBeInTheDocument()
  })

  it('can keep the caption for screen readers only', () => {
    render(<Table caption="Your jobs" captionHidden columns={COLUMNS} rows={ROWS} rowKey={(r) => r.id} />)
    expect(screen.getByRole('table', { name: 'Your jobs' })).toBeInTheDocument()
    expect(screen.getByText('Your jobs', { selector: 'caption' })).toHaveClass('sr-only')
  })
})

describe('Pagination', () => {
  it('is a navigation named Pages with Previous and Next buttons that follow the cursors', () => {
    const onPrevious = vi.fn()
    const onNext = vi.fn()
    render(<Pagination hasPrevious hasNext onPrevious={onPrevious} onNext={onNext} page={2} />)
    const nav = screen.getByRole('navigation', { name: 'Pages' })
    expect(within(nav).getByText('Page 2')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Next page' }))
    fireEvent.click(screen.getByRole('button', { name: 'Previous page' }))
    expect(onNext).toHaveBeenCalledTimes(1)
    expect(onPrevious).toHaveBeenCalledTimes(1)
  })

  it('paired control: at either end the button is disabled and does nothing', () => {
    const onPrevious = vi.fn()
    const onNext = vi.fn()
    render(<Pagination hasPrevious={false} hasNext={false} onPrevious={onPrevious} onNext={onNext} />)
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Next page' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: 'Next page' }))
    expect(onNext).not.toHaveBeenCalled()
  })

  it('both buttons wait while a page is loading', () => {
    render(<Pagination hasPrevious hasNext busy onPrevious={() => {}} onNext={() => {}} />)
    expect(screen.getByRole('button', { name: 'Next page' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled()
  })
})

describe('useCursorPager', () => {
  it('keeps the cursors already visited, so previous works with a one-way API', () => {
    const { result } = renderHook(() => useCursorPager())
    expect(result.current.cursor).toBeUndefined()
    expect(result.current.page).toBe(1)
    expect(result.current.hasPrevious).toBe(false)

    act(() => result.current.goNext('c2'))
    act(() => result.current.goNext('c3'))
    expect(result.current.cursor).toBe('c3')
    expect(result.current.page).toBe(3)
    expect(result.current.hasPrevious).toBe(true)

    act(() => result.current.goPrevious())
    expect(result.current.cursor).toBe('c2')
    act(() => result.current.goPrevious())
    expect(result.current.cursor).toBeUndefined()
    act(() => result.current.goPrevious()) // paired control: nothing before the first page
    expect(result.current.page).toBe(1)
  })

  it('does not move on a null next cursor (the last page), and reset goes back to the first page', () => {
    const { result } = renderHook(() => useCursorPager())
    act(() => result.current.goNext(null))
    expect(result.current.page).toBe(1)
    act(() => result.current.goNext('x'))
    act(() => result.current.reset())
    expect(result.current.page).toBe(1)
    expect(result.current.cursor).toBeUndefined()
  })
})
