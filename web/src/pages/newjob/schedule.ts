// "Start later": the time the user picks is on their own clock (a datetime-local input), and the platform wants an instant in UTC between one minute and 366 days
// ahead. The screen asks for at least two minutes (the answer takes time) and at most 365 days, and always shows the zone the time is read in.

export const MIN_LEAD_MINUTES = 2
export const MAX_LEAD_DAYS = 365

export type SchedulePlace = 'empty' | 'invalid' | 'tooSoon' | 'tooFar'
export type ScheduleCheck = { ok: true; iso: string } | { ok: false; problem: SchedulePlace }

const pad = (n: number) => String(n).padStart(2, '0')

/** A value for a datetime-local input: the user's own clock, to the minute ("2026-12-02T09:30"). */
export function localInputValue(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

/** The value of a datetime-local input (the user's clock) as the UTC instant to send, or why it will not do. */
export function scheduleFromLocal(value: string, now: Date = new Date()): ScheduleCheck {
  if (value.trim() === '') return { ok: false, problem: 'empty' }
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(value)
  if (m === null) return { ok: false, problem: 'invalid' }
  const [y, mo, d, h, mi, s] = [m[1], m[2], m[3], m[4], m[5], m[6] ?? '0'].map(Number) as [number, number, number, number, number, number]
  const at = new Date(y, mo - 1, d, h, mi, s)
  // a date that does not exist (the 31st of February) rolls over into the next month: that is not what was typed
  if (Number.isNaN(at.getTime()) || at.getMonth() !== mo - 1 || at.getDate() !== d) return { ok: false, problem: 'invalid' }
  const lead = at.getTime() - now.getTime()
  if (lead < MIN_LEAD_MINUTES * 60_000) return { ok: false, problem: 'tooSoon' }
  if (lead > MAX_LEAD_DAYS * 86_400_000) return { ok: false, problem: 'tooFar' }
  return { ok: true, iso: at.toISOString().replace(/\.\d{3}Z$/, 'Z') }
}

export interface ZoneInfo {
  /** the zone's name when the browser knows it ("Europe/Berlin") */
  name: string | null
  /** "UTC+01:00" at the given moment */
  offset: string
}

export function zoneInfo(at: Date = new Date()): ZoneInfo {
  let name: string | null = null
  try {
    name = Intl.DateTimeFormat().resolvedOptions().timeZone || null
  } catch {
    name = null
  }
  const minutes = -at.getTimezoneOffset()
  const sign = minutes < 0 ? '-' : '+'
  const abs = Math.abs(minutes)
  return { name, offset: `UTC${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}` }
}

const whenFormat = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' })

/** An instant (RFC 3339) as the user reads it: "Dec 2, 2026, 9:30 AM", on their clock. */
export function formatWhen(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : whenFormat.format(d)
}

const numberFormat = new Intl.NumberFormat('en-US')
export const fmt = (n: number): string => numberFormat.format(n)
