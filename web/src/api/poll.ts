// usePolling(fn, ms, { enabled }): call `fn` every `ms` milliseconds while the component is mounted, the tab is visible and `enabled` is true.
//
//   - The next call is scheduled when the previous one has FINISHED, so a slow answer never piles up requests.
//   - It stops when the tab is hidden and runs once at once when the tab is shown again.
//   - It stops when the component unmounts, and the AbortSignal given to `fn` is then aborted (so an answer in flight is dropped).
//   - The first call comes after `ms`, not at once: load the data in the usual way and let the polling keep it fresh (pass `immediate: true` to run at once).
//   - `fn` may change on every render without restarting the timer; the latest one is called.
import { useEffect, useRef } from 'react'

export interface PollingOptions {
  enabled?: boolean
  immediate?: boolean
}

export function usePolling(fn: (signal: AbortSignal) => unknown, ms: number, options: PollingOptions = {}): void {
  const { enabled = true, immediate = false } = options
  const latest = useRef(fn)
  latest.current = fn

  useEffect(() => {
    if (!enabled) return undefined
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | null = null
    let running = false
    let stopped = false

    const clear = () => {
      if (timer !== null) clearTimeout(timer)
      timer = null
    }
    const schedule = () => {
      clear()
      if (stopped || document.hidden) return
      timer = setTimeout(tick, ms)
    }
    const tick = async () => {
      timer = null
      if (stopped || running || document.hidden) return
      running = true
      try {
        await latest.current(controller.signal)
      } catch {
        // The caller shows its own error; a failed poll just waits for the next one.
      } finally {
        running = false
        schedule()
      }
    }
    const onVisibility = () => {
      if (document.hidden) {
        clear()
      } else if (!running) {
        void tick() // back on the tab: do not make the user wait out the interval
      }
    }

    document.addEventListener('visibilitychange', onVisibility)
    if (immediate && !document.hidden) void tick()
    else schedule()

    return () => {
      stopped = true
      clear()
      controller.abort()
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [enabled, immediate, ms])
}
