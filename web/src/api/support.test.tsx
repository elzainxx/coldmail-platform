// Errors, connectivity, the idempotency key, downloads and polling.
import { act, render } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { getConnectivity, reportApiAnswered, reportNetworkFailure, resetConnectivity, subscribeConnectivity, watchBrowserConnectivity } from './connectivity.ts'
import { saveBlob } from './download.ts'
import { ApiError, apiErrorFromResponse, parseRetryAfter } from './errors.ts'
import { IDEMPOTENCY_KEY_PATTERN, newIdempotencyKey } from './idempotency.ts'
import { usePolling } from './poll.ts'

describe('apiErrorFromResponse', () => {
  it('reads code, request id and details from the envelope', () => {
    const e = apiErrorFromResponse(409, JSON.stringify({ error: { code: 'job_slot_taken', message: 'm', request_id: 'req_1' } }), null)
    expect(e).toMatchObject({ status: 409, code: 'job_slot_taken', requestId: 'req_1' })
    expect(e.details).toBeUndefined()
    expect(e.reason).toBeUndefined()
  })

  it('also reads details when they sit beside the error, and a reason that is not a string is no reason', () => {
    expect(apiErrorFromResponse(400, JSON.stringify({ error: { code: 'validation_failed' }, details: { reason: 'x' } }), null).reason).toBe('x')
    expect(apiErrorFromResponse(400, JSON.stringify({ error: { code: 'validation_failed', details: { reason: 5 } } }), null).reason).toBeUndefined()
  })

  it('treats anything that is not the envelope as bad_response', () => {
    for (const body of ['', 'not json', '[]', '{}', '{"error":"x"}', '{"error":{"code":""}}', '{"error":{"code":7}}']) {
      expect(apiErrorFromResponse(500, body, null).code, body).toBe('bad_response')
    }
  })

  it('is an Error whose message is the code', () => {
    const e = new ApiError({ status: 500, code: 'internal_error' })
    expect(e).toBeInstanceOf(Error)
    expect(e.message).toBe('internal_error')
  })

  it('reads Retry-After as whole seconds only', () => {
    expect(parseRetryAfter('30')).toBe(30)
    expect(parseRetryAfter(' 1 ')).toBe(1)
    expect(parseRetryAfter(null)).toBeUndefined()
    expect(parseRetryAfter('Wed, 21 Oct 2026 07:28:00 GMT')).toBeUndefined()
    expect(parseRetryAfter('-5')).toBeUndefined()
    expect(parseRetryAfter('99999')).toBeUndefined()
  })
})

describe('connectivity', () => {
  beforeEach(() => resetConnectivity())

  it('goes offline on a network failure, unavailable on a 503, and back to ok on any other answer', () => {
    expect(getConnectivity()).toBe('ok')
    reportNetworkFailure()
    expect(getConnectivity()).toBe('offline')
    reportApiAnswered(503)
    expect(getConnectivity()).toBe('unavailable')
    reportApiAnswered(404) // the service answered: it is there
    expect(getConnectivity()).toBe('ok')
  })

  it('tells its subscribers once per change, and stops when they unsubscribe', () => {
    const l = vi.fn()
    const off = subscribeConnectivity(l)
    reportNetworkFailure()
    reportNetworkFailure()
    expect(l).toHaveBeenCalledTimes(1)
    off()
    reportApiAnswered(200)
    expect(l).toHaveBeenCalledTimes(1)
  })

  it('follows the browser\'s online and offline events until stopped', () => {
    const stop = watchBrowserConnectivity()
    window.dispatchEvent(new Event('offline'))
    expect(getConnectivity()).toBe('offline')
    window.dispatchEvent(new Event('online'))
    expect(getConnectivity()).toBe('ok')
    stop()
    window.dispatchEvent(new Event('offline'))
    expect(getConnectivity()).toBe('ok')
  })
})

describe('newIdempotencyKey', () => {
  it('fits the platform\'s rule (1 to 128 characters of letters, digits and . _ : -) and is new every time', () => {
    const keys = new Set(Array.from({ length: 50 }, () => newIdempotencyKey()))
    expect(keys.size).toBe(50)
    for (const k of keys) expect(k).toMatch(IDEMPOTENCY_KEY_PATTERN)
  })

  it('paired control: the pattern does refuse a bad key', () => {
    expect(IDEMPOTENCY_KEY_PATTERN.test('has space')).toBe(false)
    expect(IDEMPOTENCY_KEY_PATTERN.test('x'.repeat(129))).toBe(false)
    expect(IDEMPOTENCY_KEY_PATTERN.test('')).toBe(false)
  })

  it('still works where randomUUID is missing', () => {
    const real = globalThis.crypto
    vi.stubGlobal('crypto', { getRandomValues: real.getRandomValues.bind(real) })
    try {
      expect(newIdempotencyKey()).toMatch(IDEMPOTENCY_KEY_PATTERN)
    } finally {
      vi.unstubAllGlobals()
    }
  })
})

describe('saveBlob', () => {
  it('hands the blob to the browser as a download with the given name, then lets the URL go', () => {
    vi.useFakeTimers()
    const create = vi.fn(() => 'blob:fake-1')
    const revoke = vi.fn()
    URL.createObjectURL = create
    URL.revokeObjectURL = revoke
    const clicked: Array<{ download: string; href: string }> = []
    const onClick = (e: Event) => {
      const a = e.target as HTMLAnchorElement
      clicked.push({ download: a.download, href: a.href })
      e.preventDefault() // jsdom cannot navigate
    }
    document.addEventListener('click', onClick)
    saveBlob(new Blob(['a,b\n']), 'job-1.csv')
    document.removeEventListener('click', onClick)
    expect(clicked).toEqual([{ download: 'job-1.csv', href: 'blob:fake-1' }])
    expect(document.querySelector('a[download]')).toBeNull() // the helper link is not left in the page
    expect(revoke).not.toHaveBeenCalled()
    vi.advanceTimersByTime(10_000)
    expect(revoke).toHaveBeenCalledWith('blob:fake-1')
    vi.useRealTimers()
  })
})

// ---------- polling ----------

function Poller({ fn, ms = 1000, enabled = true, immediate = false }: { fn: (s: AbortSignal) => unknown; ms?: number; enabled?: boolean; immediate?: boolean }) {
  usePolling(fn, ms, { enabled, immediate })
  return null
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
  document.dispatchEvent(new Event('visibilitychange'))
}

describe('usePolling', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    setHidden(false)
    vi.useRealTimers()
  })

  const advance = async (ms: number) => {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }

  it('calls the function every interval, the first time after one interval', async () => {
    const fn = vi.fn()
    render(<Poller fn={fn} />)
    expect(fn).not.toHaveBeenCalled()
    await advance(999)
    expect(fn).not.toHaveBeenCalled()
    await advance(1)
    expect(fn).toHaveBeenCalledTimes(1)
    await advance(3000)
    expect(fn).toHaveBeenCalledTimes(4)
  })

  it('immediate runs it at once', async () => {
    const fn = vi.fn()
    render(<Poller fn={fn} immediate />)
    await advance(0)
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('stops when the tab is hidden, and runs once at once when it is shown again', async () => {
    const fn = vi.fn()
    render(<Poller fn={fn} />)
    await advance(1000)
    expect(fn).toHaveBeenCalledTimes(1)
    act(() => setHidden(true))
    await advance(10_000)
    expect(fn).toHaveBeenCalledTimes(1) // nothing while hidden
    act(() => setHidden(false))
    await advance(0)
    expect(fn).toHaveBeenCalledTimes(2) // at once on return
    await advance(1000)
    expect(fn).toHaveBeenCalledTimes(3) // and the rhythm is back
  })

  it('a call that is still running when the tab is hidden does not start the next round', async () => {
    const fn = vi.fn(async () => {
      await new Promise((r) => setTimeout(r, 500))
    })
    render(<Poller fn={fn} />)
    await advance(1000) // the call starts
    act(() => setHidden(true)) // the user leaves the tab while it runs
    await advance(500) // it finishes
    await advance(20_000)
    expect(fn).toHaveBeenCalledTimes(1)
    act(() => setHidden(false))
    await advance(0)
    expect(fn).toHaveBeenCalledTimes(2) // paired control: it was waiting for the tab, not dead
  })

  it('stops when the component unmounts, and aborts the signal it gave', async () => {
    let signal: AbortSignal | undefined
    const fn = vi.fn((s: AbortSignal) => {
      signal = s
    })
    const { unmount } = render(<Poller fn={fn} />)
    await advance(1000)
    expect(signal?.aborted).toBe(false)
    unmount()
    expect(signal?.aborted).toBe(true)
    await advance(10_000)
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('does not poll while disabled, and starts when enabled', async () => {
    const fn = vi.fn()
    const { rerender } = render(<Poller fn={fn} enabled={false} />)
    await advance(5000)
    expect(fn).not.toHaveBeenCalled()
    rerender(<Poller fn={fn} enabled />)
    await advance(1000)
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('never overlaps: the next call waits for the previous one to finish', async () => {
    let running = 0
    let maxRunning = 0
    const fn = vi.fn(async () => {
      running++
      maxRunning = Math.max(maxRunning, running)
      await new Promise((r) => setTimeout(r, 2500))
      running--
    })
    render(<Poller fn={fn} />)
    await advance(10_000)
    expect(maxRunning).toBe(1)
    expect(fn.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(fn.mock.calls.length).toBeLessThanOrEqual(3) // 2.5 s of work + 1 s wait: a call every 3.5 s, not every 1 s
  })

  it('keeps going after a call throws, and uses the latest function without restarting', async () => {
    const first = vi.fn(() => {
      throw new Error('boom')
    })
    const second = vi.fn()
    const { rerender } = render(<Poller fn={first} />)
    await advance(1000)
    expect(first).toHaveBeenCalledTimes(1)
    rerender(<Poller fn={second} />)
    await advance(1000)
    expect(second).toHaveBeenCalledTimes(1)
    expect(first).toHaveBeenCalledTimes(1)
  })
})
