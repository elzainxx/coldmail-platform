// The draft a screen of this area works on, from creating it to the confirm: create (from the list the user chose), preview, preview again (after a stale
// answer, a changed choice, or "check again"), continue a draft by id, discard. One request at a time: starting another stops the one in flight.
//
// A draft is created ONCE: if the preview that follows fails, "try again" only previews it again (no second draft is made).
import { useCallback, useEffect, useRef, useState } from 'react'
import { jobsApi } from '../../api/endpoints.ts'
import type { ApiError } from '../../api/errors.ts'
import type { ConfirmResult, Job, JobEnvelope, UnknownChoice } from '../../api/types.ts'
import { isAbort, toApiError } from './errors.ts'

export type DraftBusy = 'loading' | 'creating' | 'previewing'

export interface Draft {
  /** the draft as the last answer left it (previewed, once all went well) */
  job: Job | null
  busy: DraftBusy | null
  error: ApiError | null
  /** set once the confirm was accepted */
  result: ConfirmResult | null
  /** create the draft with `make` and preview it */
  start: (make: (signal: AbortSignal) => Promise<JobEnvelope>) => Promise<void>
  /** continue a draft the user left: load it and preview it again (unless `accept` says it is not a job of this page: it is then only loaded) */
  load: (id: string, accept?: (job: Job) => boolean) => Promise<void>
  /** preview again, optionally with the choice for unknown addresses; the new job, or null when it failed (`error` says why) */
  rePreview: (choice?: UnknownChoice) => Promise<Job | null>
  /** do the last start or load again */
  retry: () => void
  /** let the draft go (it holds nothing) and forget it */
  discard: () => Promise<void>
  confirmed: (result: ConfirmResult) => void
  /** forget the draft without telling the platform */
  reset: () => void
}

export function useDraft(): Draft {
  const [job, setJob] = useState<Job | null>(null)
  const [busy, setBusy] = useState<DraftBusy | null>(null)
  const [error, setError] = useState<ApiError | null>(null)
  const [result, setResult] = useState<ConfirmResult | null>(null)
  const idRef = useRef<string | null>(null)
  const lastRef = useRef<(() => Promise<void>) | null>(null)
  const controller = useRef<AbortController | null>(null)

  useEffect(() => () => controller.current?.abort(), [])

  const run = useCallback(async (task: (signal: AbortSignal) => Promise<void>): Promise<void> => {
    controller.current?.abort()
    const c = new AbortController()
    controller.current = c
    setError(null)
    try {
      await task(c.signal)
    } catch (e) {
      if (c.signal.aborted || isAbort(e)) return
      setError(toApiError(e))
    } finally {
      if (controller.current === c) setBusy(null)
    }
  }, [])

  const start = useCallback(
    (make: (signal: AbortSignal) => Promise<JobEnvelope>) => {
      const task = async (signal: AbortSignal) => {
        if (idRef.current === null) {
          setBusy('creating')
          idRef.current = (await make(signal)).job.id
        }
        setBusy('previewing')
        setJob((await jobsApi.preview(idRef.current, undefined, { signal })).job)
      }
      const again = () => run(task)
      lastRef.current = again
      return again()
    },
    [run],
  )

  const load = useCallback(
    (id: string, accept?: (job: Job) => boolean) => {
      const task = async (signal: AbortSignal) => {
        setBusy('loading')
        const got = (await jobsApi.get(id, { signal })).job
        idRef.current = got.id
        if ((got.status === 'draft' || got.status === 'previewed') && (accept === undefined || accept(got))) {
          setBusy('previewing')
          setJob((await jobsApi.preview(id, undefined, { signal })).job)
        } else {
          setJob(got)
        }
      }
      const again = () => run(task)
      lastRef.current = again
      return again()
    },
    [run],
  )

  const rePreview = useCallback(
    async (choice?: UnknownChoice): Promise<Job | null> => {
      const id = idRef.current
      if (id === null) return null
      let out = null as Job | null
      await run(async (signal) => {
        setBusy('previewing')
        const answer = await jobsApi.preview(id, choice === undefined ? undefined : { unknown_choice: choice }, { signal })
        out = answer.job
        setJob(answer.job)
      })
      return out
    },
    [run],
  )

  const reset = useCallback(() => {
    controller.current?.abort()
    controller.current = null
    idRef.current = null
    lastRef.current = null
    setJob(null)
    setBusy(null)
    setError(null)
    setResult(null)
  }, [])

  const discard = useCallback(async () => {
    const id = idRef.current
    reset()
    if (id === null) return
    try {
      await jobsApi.discard(id)
    } catch {
      // a draft holds nothing: if it could not be let go it stays a draft the user can find in the jobs list
    }
  }, [reset])

  const retry = useCallback(() => {
    void lastRef.current?.()
  }, [])

  const confirmed = useCallback((r: ConfirmResult) => {
    setResult(r)
    setJob(r.job)
  }, [])

  return { job, busy, error, result, start, load, rePreview, retry, discard, confirmed, reset }
}
