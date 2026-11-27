// How a job is named, counted and dated on every screen of this area (the dashboard shares it). Every sentence is in copy/jobs.ts.
import type { Job } from '../../api/types.ts'
import { FINAL_JOB_STATUSES } from '../../api/types.ts'
import { maybeT, t } from '../../copy/index.ts'

const numbers = new Intl.NumberFormat('en-US')

/** 12345 as "12,345". */
export function formatNumber(n: number): string {
  return numbers.format(n)
}

/** An RFC 3339 time as the user's local date and time ("Dec 1, 2026, 9:30 AM"); '' for a time that is missing or unreadable (never "Invalid Date"). `timeZone` is for tests. */
export function formatDateTime(iso: string | null | undefined, timeZone?: string): string {
  if (iso === null || iso === undefined) return ''
  const when = new Date(iso)
  if (Number.isNaN(when.getTime())) return ''
  return new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short', ...(timeZone === undefined ? {} : { timeZone }) }).format(when)
}

/** "Verification", "Managed sending", "Your own SMTP"; an unknown kind is "Job". */
export function kindText(module: string): string {
  return maybeT(`jobs.kind.${module}`) ?? t('jobs.kind.other')
}

/** "Verification from Dec 1, 2026, 9:30 AM": a job has no name of its own, so its kind and its date name it. */
export function jobName(job: Pick<Job, 'module' | 'created_at'>, timeZone?: string): string {
  return t('jobs.name', { kind: kindText(job.module), date: formatDateTime(job.created_at, timeZone) })
}

/** "1 address", "12,345 addresses", or "Not counted yet" for a job that has no preview. */
export function addressCountText(job: Pick<Job, 'preview'>): string {
  if (job.preview === null) return t('jobs.count.none')
  return job.preview.rows === 1 ? t('jobs.count.one') : t('jobs.count.other', { count: formatNumber(job.preview.rows) })
}

/** "1 credit", "1,200 credits". */
export function creditsText(n: number): string {
  return n === 1 ? t('jobs.credits.one') : t('jobs.credits.other', { count: formatNumber(n) })
}

const FINAL = new Set<string>(FINAL_JOB_STATUSES)
const NOT_STARTED = new Set<string>(['draft', 'previewed'])

const WAITING = new Set<string>(['scheduled', 'paused'])

/** A job that is waiting (for its start, or for the user to resume it): it changes by the user's hand or by the clock, not by itself from one second to the next. */
export function isWaiting(status: string): boolean {
  return WAITING.has(status)
}

/** A job whose items are moving (confirmed to completing, but not waiting): its page asks again every few seconds. */
export function isWorking(status: string): boolean {
  return isLive(status) && !WAITING.has(status)
}

/** A job that nothing moves any more. */
export function isFinal(status: string): boolean {
  return FINAL.has(status)
}

/** A job the platform is working on or waiting on (confirmed to completing): its page keeps itself up to date. A draft and a finished job do not change by themselves. */
export function isLive(status: string): boolean {
  return !FINAL.has(status) && !NOT_STARTED.has(status)
}

/** A job the user has not confirmed yet (it is continued on the New job screen). */
export function isNotStarted(status: string): boolean {
  return NOT_STARTED.has(status)
}

/** Whole days between two times, at least 1, or null when either is missing: how long a job's content was kept (the sentence about cleared content). */
export function daysBetween(fromIso: string | null | undefined, toIso: string | null | undefined): number | null {
  if (fromIso === null || fromIso === undefined || toIso === null || toIso === undefined) return null
  const from = new Date(fromIso).getTime()
  const to = new Date(toIso).getTime()
  if (Number.isNaN(from) || Number.isNaN(to) || to < from) return null
  return Math.max(1, Math.round((to - from) / 86_400_000))
}

/** "1 day", "7 days", or "a few days" when it is not known. */
export function daysText(days: number | null): string {
  if (days === null) return t('jobs.days.some')
  return days === 1 ? t('jobs.days.one', { n: 1 }) : t('jobs.days.other', { n: formatNumber(days) })
}

/** The name of the file a job's results are saved as: kind, day and the first characters of its id ("results-verification-2026-12-01-0b000000.csv"). */
export function csvFileName(job: Pick<Job, 'id' | 'module' | 'created_at'>): string {
  const kind = maybeT(`jobs.csv.kind.${job.module}`) ?? t('jobs.csv.kind.other')
  const day = /^\d{4}-\d{2}-\d{2}/.exec(job.created_at)?.[0] ?? 'undated'
  const id = job.id.replace(/[^a-z0-9]/gi, '').slice(0, 8)
  return t('jobs.csv.fileName', { kind, date: day, id })
}

/** The page a job that was not started is continued on (the New job screens read `?job=`). */
export function continuePath(job: Pick<Job, 'id' | 'module'>): string {
  const base = job.module === 'm1' ? '/new/verify' : '/new/send'
  return `${base}?job=${encodeURIComponent(job.id)}`
}
