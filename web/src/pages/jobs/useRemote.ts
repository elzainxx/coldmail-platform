// useRemote(load, deps): one request, kept as state, for a screen of this area (the dashboard, the jobs table, the job page).
//
//   const jobs = useRemote((signal) => jobsApi.list({}, { signal }), [])
//   jobs.data       what the last answer held; it STAYS on screen while the next load runs (a page that is changing does not flash)
//   jobs.loading    a load that shows itself is running (the first one, a change of `deps`, retry)
//   jobs.error      the ApiError of the last load that showed itself; the data is dropped with it, so a screen never shows the old page under a new error
//   jobs.refresh    a silent reload for usePolling: it keeps what is shown, forgets a failure, and drops its answer if a newer load or `replace` has begun
//   jobs.retry      a load that shows itself again (the Try again button)
//   jobs.replace    put an answer in without asking (the job that a pause, a resume or a cancel returned): whatever is in flight is dropped
//
// The request is aborted when `deps` change and when the screen goes away, so an answer for something the user has left never lands.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, isAbortError } from '../../api/errors.ts'

interface State<T> {
  data: T | undefined
  error: ApiError | null
  loading: boolean
}

/** How often a screen asks again while the platform is working on what it shows, and while nothing is. */
export const LIVE_POLL_MS = 3_000
export const IDLE_POLL_MS = 15_000

export interface Remote<T> {
  data: T | undefined
  error: ApiError | null
  loading: boolean
  refresh: (signal?: AbortSignal) => Promise<void>
  retry: () => void
  replace: (data: T) => void
}

/** What a thrown value is to a screen: an ApiError, or (a bug, an unreadable answer) the "could not read the reply" error. */
export function asApiError(e: unknown): ApiError {
  return e instanceof ApiError ? e : new ApiError({ status: 0, code: 'bad_response' })
}

export function useRemote<T>(load: (signal: AbortSignal) => Promise<T>, deps: readonly unknown[]): Remote<T> {
  const [state, setState] = useState<State<T>>({ data: undefined, error: null, loading: true })
  const [attempt, setAttempt] = useState(0)
  const loadRef = useRef(load)
  loadRef.current = load
  const generation = useRef(0)
  const lifetime = useRef<AbortController | null>(null)

  useEffect(() => {
    lifetime.current = new AbortController()
    const life = lifetime.current
    return () => life.abort()
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    const mine = ++generation.current
    setState((s) => ({ ...s, loading: true, error: null }))
    loadRef.current(controller.signal).then(
      (data) => {
        if (mine === generation.current && !controller.signal.aborted) setState({ data, error: null, loading: false })
      },
      (e: unknown) => {
        if (mine !== generation.current || controller.signal.aborted || isAbortError(e)) return
        setState({ data: undefined, error: asApiError(e), loading: false })
      },
    )
    return () => controller.abort()
    // `deps` is the caller's list of what the request depends on (its length never changes between two renders of one screen)
  }, [attempt, ...deps])

  const refresh = useCallback(async (signal?: AbortSignal) => {
    const mine = generation.current
    const useSignal = signal ?? lifetime.current?.signal
    try {
      const data = await loadRef.current(useSignal ?? new AbortController().signal)
      if (mine === generation.current && useSignal?.aborted !== true) setState({ data, error: null, loading: false })
    } catch {
      // a poll that fails waits for the next one: the screen keeps what it shows (connectivity is the offline banner's to say)
    }
  }, [])

  const retry = useCallback(() => setAttempt((n) => n + 1), [])

  const replace = useCallback((data: T) => {
    generation.current++
    setState({ data, error: null, loading: false })
  }, [])

  return useMemo(() => ({ ...state, refresh, retry, replace }), [state, refresh, retry, replace])
}
