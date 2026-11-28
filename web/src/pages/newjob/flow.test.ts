import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../api/errors.ts'
import { IDEMPOTENCY_KEY_PATTERN } from '../../api/idempotency.ts'
import { nextAttempt, intentSignature, type Intent } from './confirmAttempt.ts'
import { csvQuery, fitsInQuery, MAX_QUERY_CHARS, queryLength } from './createRequest.ts'
import { outcomeUnknown, toApiError } from './errors.ts'
import { fieldColumns, mergeFieldsOf } from './mergeFields.ts'
import { malformedLines } from './PreviewPanel.tsx'
import { describeChanges, previewChanges } from './previewDiff.ts'
import { formatWhen, localInputValue, MAX_LEAD_DAYS, MIN_LEAD_MINUTES, scheduleFromLocal, zoneInfo } from './schedule.ts'
import { account, preview } from './testing/fixtures.ts'
import { accountUsable } from './useSenderFacts.ts'
import { EMPTY_MESSAGE } from './authoring/message.ts'

describe('the confirm attempt', () => {
  const intent: Intent = { jobId: 'job-1', fingerprint: 'fp-1', scheduledStart: null, unknownChoice: null }

  it('makes a key for a new intent and gives the same attempt back for the same intent', () => {
    let n = 0
    const mint = vi.fn(() => `confirm-${++n}`)
    const first = nextAttempt(null, intent, mint)
    expect(first.key).toBe('confirm-1')
    expect(nextAttempt(first, { ...intent }, mint)).toBe(first) // the very same attempt: the same key
    expect(mint).toHaveBeenCalledTimes(1)
  })

  it('makes a new key when any part of the intent changes: the job, the preview, when it starts, the choice about unknown addresses', () => {
    let n = 0
    const mint = () => `confirm-${++n}`
    const first = nextAttempt(null, intent, mint)
    for (const changed of [
      { ...intent, jobId: 'job-2' },
      { ...intent, fingerprint: 'fp-2' },
      { ...intent, scheduledStart: '2030-01-01T00:00:00Z' },
      { ...intent, unknownChoice: 'include' },
    ]) {
      const next = nextAttempt(first, changed, mint)
      expect(next.key, JSON.stringify(changed)).not.toBe(first.key)
      expect(next.signature).toBe(intentSignature(changed))
    }
  })

  it('makes keys the platform accepts, one per attempt', () => {
    const a = nextAttempt(null, intent)
    const b = nextAttempt(null, intent)
    expect(a.key).toMatch(IDEMPOTENCY_KEY_PATTERN)
    expect(b.key).not.toBe(a.key)
  })

  it('treats a confirm as maybe-done only when no answer came or the service failed, never when the platform refused it', () => {
    expect(outcomeUnknown(new ApiError({ status: 0, code: 'network_error' }))).toBe(true)
    expect(outcomeUnknown(new ApiError({ status: 500, code: 'internal_error' }))).toBe(true)
    expect(outcomeUnknown(new ApiError({ status: 503, code: 'module_unavailable' }))).toBe(true)
    for (const status of [400, 402, 403, 404, 409, 422, 429]) expect(outcomeUnknown(new ApiError({ status, code: 'x' })), String(status)).toBe(false)
  })

  it('keeps an ApiError and makes anything else one the screen words generically', () => {
    const err = new ApiError({ status: 409, code: 'job_slot_taken' })
    expect(toApiError(err)).toBe(err)
    const other = toApiError(new TypeError('boom'))
    expect(other).toBeInstanceOf(ApiError)
    expect(other.message).not.toContain('boom')
  })
})

describe('starting later', () => {
  const now = new Date(2030, 0, 1, 12, 0, 0)
  const at = (minutes: number) => localInputValue(new Date(now.getTime() + minutes * 60_000))

  it('reads the user\'s own clock and gives the instant in UTC, without fractions of a second', () => {
    const r = scheduleFromLocal('2030-03-04T09:30', now)
    expect(r).toEqual({ ok: true, iso: new Date(2030, 2, 4, 9, 30).toISOString().replace(/\.\d{3}Z$/, 'Z') })
    expect(r.ok && r.iso).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/)
  })

  it('asks for at least two minutes ahead (and says nothing is wrong at exactly two), and at most a year', () => {
    expect(MIN_LEAD_MINUTES).toBe(2)
    expect(scheduleFromLocal(at(1), now)).toEqual({ ok: false, problem: 'tooSoon' })
    expect(scheduleFromLocal(at(0), now)).toEqual({ ok: false, problem: 'tooSoon' })
    expect(scheduleFromLocal(at(-60), now)).toEqual({ ok: false, problem: 'tooSoon' })
    expect(scheduleFromLocal(at(2), now).ok).toBe(true)
    expect(scheduleFromLocal(at(MAX_LEAD_DAYS * 24 * 60), now).ok).toBe(true)
    expect(scheduleFromLocal(at(MAX_LEAD_DAYS * 24 * 60 + 1), now)).toEqual({ ok: false, problem: 'tooFar' })
  })

  it('refuses nothing, something that is not a date, and a date that does not exist (the 31st of February is not the 3rd of March)', () => {
    expect(scheduleFromLocal('', now)).toEqual({ ok: false, problem: 'empty' })
    expect(scheduleFromLocal('  ', now)).toEqual({ ok: false, problem: 'empty' })
    expect(scheduleFromLocal('tomorrow', now)).toEqual({ ok: false, problem: 'invalid' })
    expect(scheduleFromLocal('2030-02-31T10:00', now)).toEqual({ ok: false, problem: 'invalid' })
    expect(scheduleFromLocal('2030-02-28T10:00', now).ok).toBe(true) // control: a date that exists
  })

  it('accepts a value with seconds, as some browsers give one', () => {
    expect(scheduleFromLocal('2030-03-04T09:30:15', now).ok).toBe(true)
  })

  it('says the zone as a name and an offset from UTC, for either side of it', () => {
    const fake = (offsetMinutes: number) => ({ getTimezoneOffset: () => offsetMinutes }) as Date
    expect(zoneInfo(fake(-90)).offset).toBe('UTC+01:30')
    expect(zoneInfo(fake(300)).offset).toBe('UTC-05:00')
    expect(zoneInfo(fake(0)).offset).toBe('UTC+00:00')
    expect(zoneInfo().name === null || typeof zoneInfo().name === 'string').toBe(true)
  })

  it('writes a moment the way a person reads it, and nothing for one it cannot read', () => {
    expect(formatWhen('2030-05-06T10:30:00Z')).toMatch(/2030/)
    expect(formatWhen('not a time')).toBe('')
  })
})

describe('what changed between two previews', () => {
  it('lists only the figures that moved, in the order of the preview, and nothing when nothing did', () => {
    const before = preview()
    expect(previewChanges(before, preview())).toEqual([])
    const after = preview({ units: 2, credits: 2, malformed: 2, available: 99 })
    expect(previewChanges(before, after)).toEqual([
      { field: 'units', from: 3, to: 2 },
      { field: 'credits', from: 3, to: 2 },
      { field: 'available', from: 100, to: 99 },
      { field: 'malformed', from: 1, to: 2 },
    ])
  })

  it('says each in a sentence with both numbers', () => {
    const lines = describeChanges(previewChanges(preview({ rows: 1000 }), preview({ rows: 1500, suppressed: 9, missing_fields: 4 })))
    expect(lines).toEqual([
      'The rows in your list changed from 1,000 to 1,500.',
      'The addresses on your suppression list changed from 1 to 9.',
      'The rows missing a value changed from 0 to 4.',
    ])
  })

  it('says a change of the choice about unknown addresses in words', () => {
    const lines = describeChanges(previewChanges(preview({ unknown_choice: null }), preview({ unknown_choice: 'exclude' })))
    expect(lines).toEqual(['What happens to unknown addresses changed from "not chosen" to "leave them out".'])
    const other = describeChanges(previewChanges(preview({ unknown_choice: 'include' }), preview({ unknown_choice: 'exclude' })))
    expect(other[0]).toContain('include them')
  })

  it('says the reasons rows were malformed most common first, a reason it does not know generically, and none for a reason with no rows', () => {
    const lines = malformedLines({ empty_domain: 2, no_at: 5, zzz_new: 1, multiple_at: 0 })
    expect(lines.map((l) => [l.reason, l.count])).toEqual([
      ['no_at', 5],
      ['empty_domain', 2],
      ['zzz_new', 1],
    ])
    expect(lines[2]?.text).toBe('The address is not valid.')
    expect(lines.every((l) => !l.text.includes('_'))).toBe(true)
    expect(malformedLines({})).toEqual([])
  })
})

describe('how a send job is made', () => {
  const message = { ...EMPTY_MESSAGE, subject: 'Hello', html: '<p>Hi</p>', text: '', replyTo: ' me@example.test ' }

  it('puts the message in the query, with the reply-to trimmed, and measures it as it will be written', () => {
    const q = csvQuery('m2', 'work mail', message)
    expect(q).toEqual({ module: 'm2', email_column: 'work mail', subject: 'Hello', html: '<p>Hi</p>', text: '', reply_to: 'me@example.test' })
    expect(queryLength(q)).toBe(new URLSearchParams({ module: 'm2', email_column: 'work mail', subject: 'Hello', html: '<p>Hi</p>', reply_to: 'me@example.test' }).toString().length)
    expect(fitsInQuery(q)).toBe(true)
  })

  it('knows a message is too long for a query before it is sent: the characters a query must write as three count as three', () => {
    const plain = csvQuery('m2', 'email', { ...message, html: 'a'.repeat(60_000) })
    const escaped = csvQuery('m2', 'email', { ...message, html: '<'.repeat(30_000) })
    expect(queryLength(plain)).toBeLessThan(MAX_QUERY_CHARS)
    expect(fitsInQuery(plain)).toBe(true)
    expect(queryLength(escaped)).toBeGreaterThan(MAX_QUERY_CHARS) // 30,000 characters, over 90,000 written
    expect(fitsInQuery(escaped)).toBe(false)
  })
})

describe('which sender accounts can send', () => {
  it('counts an active account whose login was not refused and whose From address is not blocked, warned or not yet checked included', () => {
    expect(accountUsable(account())).toBe(true)
    expect(accountUsable(account({ from_gate: 'unaligned_warned' }))).toBe(true)
    expect(accountUsable(account({ verification: 'unverified', from_gate: 'unverified' }))).toBe(true)
  })

  it('does not count one that is switched off, held, refused by its provider, or blocked', () => {
    for (const status of ['paused', 'quarantined', 'disabled'] as const) expect(accountUsable(account({ status })), status).toBe(false)
    expect(accountUsable(account({ verification: 'failed' }))).toBe(false)
    expect(accountUsable(account({ from_gate: 'blocked' }))).toBe(false)
  })
})

describe('the merge fields of a job', () => {
  it('reads merge_fields as they are, and an answer without them (an older service) as none', () => {
    expect(mergeFieldsOf({ merge_fields: ['first_name', 'plan'] })).toEqual(['first_name', 'plan'])
    expect(mergeFieldsOf({})).toEqual([])
    expect(mergeFieldsOf({ merge_fields: undefined })).toEqual([])
    expect(mergeFieldsOf(null)).toEqual([])
    expect(mergeFieldsOf(undefined)).toEqual([])
    expect(mergeFieldsOf({ merge_fields: null as unknown as string[] })).toEqual([])
  })

  it('leaves out the two names the platform fills in, and anything that is not a name', () => {
    expect(mergeFieldsOf({ merge_fields: ['email', 'plan', 'unsubscribe_url', '', 7 as unknown as string] })).toEqual(['plan'])
  })

  it('makes the palette columns of them, marking a name a message cannot use', () => {
    expect(fieldColumns(['first_name', 'bad name'])).toEqual([
      { index: 0, name: 'first_name', usable: true },
      { index: 1, name: 'bad name', usable: false },
    ])
    expect(fieldColumns([])).toEqual([])
  })
})
