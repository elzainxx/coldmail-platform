import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { App } from './App.tsx'

// The SAME queries are used by the real test and by the paired control below.
const findHeading = () => screen.queryByRole('heading', { level: 1 })
const findConstructionLine = () => screen.queryByText(/under construction/i)

// Deliberately broken: looks like a title but is a plain div, and has no under-construction line.
function BrokenPage() {
  return <div>Coldmail platform</div>
}

describe('App', () => {
  it('renders a level-1 heading and the under-construction line', () => {
    render(<App />)

    expect(findHeading()).toBeInTheDocument()
    expect(findConstructionLine()).toBeInTheDocument()
  })

  it('paired control: the same queries find nothing in a component with no heading', () => {
    render(<BrokenPage />)

    expect(findHeading()).not.toBeInTheDocument()
    expect(findConstructionLine()).not.toBeInTheDocument()
  })

  it('root element carries the Tailwind utility that scripts/check-css.mjs looks for in the built CSS', () => {
    // jsdom does not load the built stylesheet, so only the class half can be asserted here.
    // The "built CSS contains the selector" half is `npm run check:css`, run after `npm run build`.
    const { container } = render(<App />)

    expect(container.firstElementChild).toHaveClass('min-h-screen')
  })
})
