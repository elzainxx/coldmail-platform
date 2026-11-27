import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, mockApi, resetForTest, signInForTest } from '../../../test/helpers.tsx'
import { CHECK_DELAY_MS, useTemplateCheck } from './useTemplateCheck.ts'

beforeEach(async () => {
  resetForTest()
  await signInForTest()
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  resetForTest()
})

type Parts = { subject: string; html: string; text: string }
const parts = (over: Partial<Parts> = {}): Parts => ({ subject: 'Hi', html: '<p>Hello</p>', text: '', ...over })

const advance = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)))

/** A fetch whose answers the test hands out one at a time, and which says when a request is aborted. */
function hangingFetch() {
  const calls: Array<{ body: unknown; signal: AbortSignal; resolve: (body: unknown) => void }> = []
  const fn = vi.fn(
    (_url: unknown, init: RequestInit = {}) =>
      new Promise<Response>((resolve, reject) => {
        const signal = init.signal as AbortSignal
        calls.push({ body: JSON.parse(String(init.body)), signal, resolve: (b) => resolve(jsonResponse(b)) })
        signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
      }),
  )
  vi.stubGlobal('fetch', fn)
  return { fn, calls }
}

describe('the live check of a message', () => {
  it('waits 400 ms after the last keystroke, then sends the three parts, once', async () => {
    const fetchFn = mockApi({ 'POST /v1/templates/check': { ok: true, fields: [{ name: 'name', has_fallback: false }] } })
    const { result, rerender } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts() })
    expect(CHECK_DELAY_MS).toBe(400)
    expect(result.current.pending).toBe(true)

    await advance(399)
    expect(fetchFn).not.toHaveBeenCalled() // control: not a moment before the delay

    await advance(1)
    expect(fetchFn).toHaveBeenCalledTimes(1)
    const init = fetchFn.mock.calls[0]?.[1] as RequestInit
    expect(JSON.parse(String(init.body))).toEqual({ subject: 'Hi', html: '<p>Hello</p>', text: '' })
    expect(result.current).toEqual({ pending: false, result: { kind: 'ok', fields: [{ name: 'name', has_fallback: false }] } })

    // control: a later change is a new check, so the count above was the count of one change, not a ceiling
    rerender(parts({ subject: 'Hi {{name}}' }))
    await advance(400)
    expect(fetchFn).toHaveBeenCalledTimes(2)
  })

  it('asks once for a burst of keystrokes: each one starts the wait again', async () => {
    const fetchFn = mockApi({ 'POST /v1/templates/check': { ok: true, fields: [] } })
    const { rerender } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts({ subject: 'H' }) })
    await advance(300)
    rerender(parts({ subject: 'Hi' }))
    await advance(300)
    rerender(parts({ subject: 'Hi!' }))
    await advance(399)
    expect(fetchFn).not.toHaveBeenCalled() // 999 ms after the first keystroke, 399 after the last
    await advance(1)
    expect(fetchFn).toHaveBeenCalledTimes(1)
    expect(JSON.parse(String((fetchFn.mock.calls[0]?.[1] as RequestInit).body)).subject).toBe('Hi!')
  })

  it('aborts a request that a newer keystroke has made old, and never shows its answer', async () => {
    const { calls } = hangingFetch()
    const { result, rerender } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts({ subject: 'one' }) })
    await advance(400)
    expect(calls).toHaveLength(1)
    expect(calls[0]?.signal.aborted).toBe(false) // it is out, and nothing has superseded it yet

    rerender(parts({ subject: 'two' }))
    expect(calls[0]?.signal.aborted).toBe(true) // superseded at the keystroke, before the next request is even sent
    expect(result.current.pending).toBe(true)
    await advance(400)
    expect(calls).toHaveLength(2)
    expect(calls[1]?.signal.aborted).toBe(false)

    // the old request's answer arrives late: it is not shown
    calls[0]?.resolve({ ok: false, errors: [{ part: 'subject', line: 1, column: 1, message: 'OLD' }] })
    await advance(0)
    expect(result.current.result).toBeNull()

    calls[1]?.resolve({ ok: true, fields: [] })
    await advance(0)
    expect(result.current).toEqual({ pending: false, result: { kind: 'ok', fields: [] } })
  })

  it('aborts the request in flight when the screen goes away', async () => {
    const { calls } = hangingFetch()
    const { unmount } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts() })
    await advance(400)
    expect(calls[0]?.signal.aborted).toBe(false)
    unmount()
    expect(calls[0]?.signal.aborted).toBe(true)
  })

  it('gives the position of a mistake as the platform says it: the part, the line and the column, and the grammar\'s sentence', async () => {
    const errors = [
      { part: 'subject', line: 1, column: 4, message: 'unclosed {{: write }} to end the field' },
      { part: 'html', line: 2, column: 9, message: 'unexpected }: write \\} for a literal brace' },
    ]
    mockApi({ 'POST /v1/templates/check': { ok: false, errors } })
    const { result } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts({ subject: 'Hi {{name', html: '<p>\n<b>x}</b>' }) })
    await advance(400)
    expect(result.current).toEqual({ pending: false, result: { kind: 'errors', errors } })
  })

  it('keeps the last answer while the next check is on its way, so the marks do not flicker', async () => {
    const { calls } = hangingFetch()
    const { result, rerender } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts() })
    await advance(400)
    calls[0]?.resolve({ ok: false, errors: [{ part: 'html', line: 1, column: 1, message: 'm' }] })
    await advance(0)
    expect(result.current.result?.kind).toBe('errors')
    rerender(parts({ html: '<p>Hello!</p>' }))
    expect(result.current.pending).toBe(true)
    expect(result.current.result?.kind).toBe('errors') // still shown
  })

  it('does not ask at all while the message is empty, and says nothing then', async () => {
    const fetchFn = mockApi({ 'POST /v1/templates/check': { ok: true, fields: [] } })
    const { result } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: { subject: '', html: '  ', text: '' } })
    await advance(2000)
    expect(fetchFn).not.toHaveBeenCalled()
    expect(result.current).toEqual({ pending: false, result: null })
    // control: the same hook does ask once there is something to check
    const asked = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts() })
    await advance(400)
    expect(fetchFn).toHaveBeenCalledTimes(1)
    expect(asked.result.current.pending).toBe(false)
  })

  it('says the check could not be made when it fails, without a code word or the server\'s message', async () => {
    mockApi({ 'POST /v1/templates/check': [503, { error: { code: 'module_unavailable', message: 'SERVER-SENTENCE', request_id: 'req_1' } }] })
    const { result } = renderHook((p: Parts) => useTemplateCheck(p), { initialProps: parts() })
    await advance(400)
    const r = result.current.result
    expect(r?.kind).toBe('failed')
    expect(JSON.stringify(r)).not.toContain('SERVER-SENTENCE')
  })
})
