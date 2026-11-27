// The user's finished verifications (GET /v1/jobs, a verification that succeeded), newest first, for "from a finished verification". The platform filters a
// list by `status=active` only, so the list is read in pages of 100 and filtered here: it reads on until it has enough to show, or has read five pages (500 jobs),
// and "Show more" reads on from there.
import { useCallback, useEffect, useRef, useState } from 'react'
import { jobsApi } from '../../api/endpoints.ts'
import type { ApiError } from '../../api/errors.ts'
import type { Job } from '../../api/types.ts'
import { isAbort, toApiError } from './errors.ts'

const PAGE = 100
const WANT = 20
const MAX_PAGES = 5

export function isFinishedVerification(job: Job): boolean {
  return job.module === 'm1' && job.status === 'succeeded'
}

export interface Verifications {
  jobs: Job[]
  loading: boolean
  error: ApiError | null
  /** the user has more jobs than were read */
  hasMore: boolean
  more: () => void
  retry: () => void
}

export function useFinishedVerifications(enabled: boolean): Verifications {
  const [jobs, setJobs] = useState<Job[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  const [hasMore, setHasMore] = useState(false)
  const cursor = useRef<string | null>(null)
  const loadedOnce = useRef(false)
  const controller = useRef<AbortController | null>(null)

  const read = useCallback(async () => {
    controller.current?.abort()
    const c = new AbortController()
    controller.current = c
    setLoading(true)
    setError(null)
    try {
      let found = 0
      let pages = 0
      let next = cursor.current
      let more = true
      const collected: Job[] = []
      while (more && found < WANT && pages < MAX_PAGES) {
        const page = await jobsApi.list(next === null ? { limit: PAGE } : { limit: PAGE, cursor: next }, { signal: c.signal })
        pages++
        for (const j of page.jobs) {
          if (isFinishedVerification(j)) {
            collected.push(j)
            found++
          }
        }
        next = page.next_cursor
        more = next !== null
      }
      if (c.signal.aborted) return
      cursor.current = next
      loadedOnce.current = true
      setJobs((prev) => [...prev, ...collected])
      setHasMore(more)
    } catch (e) {
      if (c.signal.aborted || isAbort(e)) return
      setError(toApiError(e))
    } finally {
      if (controller.current === c) setLoading(false)
    }
  }, [])

  // Read when the list is first needed. A read stopped by the effect's cleanup (leaving the step; React running the effect twice in development) starts over.
  useEffect(() => {
    if (!enabled || loadedOnce.current) return undefined
    void read()
    return () => controller.current?.abort()
  }, [enabled, read])

  return { jobs, loading, error, hasMore, more: () => void read(), retry: () => void read() }
}
