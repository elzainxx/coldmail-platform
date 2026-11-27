import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeEach, vi } from 'vitest'

// Vitest globals are off, so Testing Library cannot register its own auto-cleanup.
afterEach(() => {
  cleanup()
})

// jsdom does not implement scrolling (it prints "Not implemented: window.scrollTo" and carries on): the router scrolls to the top on every page change.
beforeEach(() => {
  window.scrollTo = vi.fn() as unknown as typeof window.scrollTo
})
