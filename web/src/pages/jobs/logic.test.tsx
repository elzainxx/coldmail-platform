// The pure rules of the jobs screens: which controls a state allows, how an event is told, how a job is named and dated, what a result costs, and the one hook.
import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../api/errors.ts'
import type { Job, JobEvent, JobStatus, ModuleId } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { mockApi, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { cancelSentences } from './JobControlsBar.tsx'
import { controlsFor, refusalOf, type Refusal } from './controls.ts'
import { eventText } from './eventText.ts'
import { addressCountText, continuePath, csvFileName, creditsText, daysBetween, daysText, formatDateTime, formatNumber, isFinal, isLive, isNotStarted, isWaiting, isWorking, jobName, kindText } from './format.ts'
import { makeEvent, makeJob, makePreview, makeResult } from './fixtures.ts'
import { COUNT_PAGES_MAX, costText, countResults, detailLines, loadEvents, outcomesOf } from './jobData.ts'
import { asApiError, useRemote, type Remote } from './useRemote.ts'
import { clearedText, nowText } from './JobDetailPage.tsx'
import { inProgress, m3Summary } from '../dashboard/DashboardPage.tsx'

const ALL_STATUSES: JobStatus[] = ['draft', 'previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing', 'succeeded', 'cancelled', 'expired', 'failed']
const MODULES: ModuleId[] = ['m1', 'm2', 'm3']

describe('controlsFor', () => {
  const FAR = '2999-01-01T00:00:00Z'
  const NOW = Date.parse('2026-12-01T12:00:00Z')
  const job = (over: Partial<Job>) => ({ module: 'm2' as ModuleId, status: 'running' as JobStatus, paused_from: null, start_deadline: null, ...over })
  const offered = (over: Partial<Job>, refused: Refusal[] = []) => {
    const c = controlsFor(job(over), NOW, new Set(refused))
    return { pause: c.pause, resume: c.resume, cancel: c.cancel }
  }

  it('is the API\'s table, for every status and every kind', () => {
    for (const module of MODULES) {
      expect(offered({ module, status: 'draft' }), `draft ${module}`).toEqual({ pause: false, resume: false, cancel: false })
      expect(offered({ module, status: 'previewed' })).toEqual({ pause: false, resume: false, cancel: false })
      expect(offered({ module, status: 'confirmed' })).toEqual({ pause: false, resume: false, cancel: true })
      expect(offered({ module, status: 'scheduled' })).toEqual({ pause: true, resume: false, cancel: true }) // any kind can defer a start
      expect(offered({ module, status: 'paused', paused_from: 'running' })).toEqual({ pause: false, resume: true, cancel: true })
      expect(offered({ module, status: 'paused', paused_from: 'scheduled', start_deadline: FAR })).toEqual({ pause: false, resume: true, cancel: true })
      for (const status of ['cancelling', 'completing', 'succeeded', 'cancelled', 'expired', 'failed'] as const) {
        expect(offered({ module, status }), `${status} ${module}`).toEqual({ pause: false, resume: false, cancel: false })
      }
    }
    expect(offered({ module: 'm1', status: 'running' })).toEqual({ pause: false, resume: false, cancel: true })
    expect(offered({ module: 'm2', status: 'running' })).toEqual({ pause: true, resume: false, cancel: true })
    expect(offered({ module: 'm3', status: 'running' })).toEqual({ pause: true, resume: false, cancel: true })
  })

  it('knows what a cancel will find: nothing started before the start, a started job after', () => {
    const kind = (over: Partial<Job>) => controlsFor(job(over), NOW).cancelKind
    expect(kind({ status: 'confirmed' })).toBe('notStarted')
    expect(kind({ status: 'scheduled' })).toBe('notStarted')
    expect(kind({ status: 'paused', paused_from: 'scheduled' })).toBe('notStarted')
    expect(kind({ status: 'running' })).toBe('started')
    expect(kind({ status: 'paused', paused_from: 'running' })).toBe('started')
    expect(kind({ status: 'paused', paused_from: null })).toBe('started') // when in doubt, the cautious sentence
  })

  it('does not offer the resume of a job paused before its start once that start has passed, and says why', () => {
    const late = controlsFor(job({ status: 'paused', paused_from: 'scheduled', start_deadline: '2026-12-01T11:59:59Z' }), NOW)
    expect(late.resume).toBe(false)
    expect(late.notes).toContain('jobs.controls.noResume.deadline')
    // paired controls: a start still ahead, a pause that began while running (no start to miss) and a job with no deadline are offered the resume
    expect(controlsFor(job({ status: 'paused', paused_from: 'scheduled', start_deadline: '2026-12-01T12:00:01Z' }), NOW).resume).toBe(true)
    expect(controlsFor(job({ status: 'paused', paused_from: 'running', start_deadline: '2020-01-01T00:00:00Z' }), NOW).resume).toBe(true)
    expect(controlsFor(job({ status: 'paused', paused_from: 'scheduled', start_deadline: null }), NOW).resume).toBe(true)
  })

  it('takes away what a met refusal forbids, and only that', () => {
    expect(offered({ status: 'running', module: 'm2' }, ['pause_not_supported'])).toEqual({ pause: false, resume: false, cancel: true })
    expect(offered({ status: 'running', module: 'm2' }, ['cancel_not_supported'])).toEqual({ pause: true, resume: false, cancel: false })
    expect(controlsFor(job({ status: 'running' }), NOW, new Set<Refusal>(['cancel_not_supported'])).notes).toContain('jobs.controls.noCancel')
    expect(offered({ status: 'paused', paused_from: 'scheduled', start_deadline: FAR }, ['start_deadline_passed'])).toEqual({ pause: false, resume: false, cancel: true })
    // paired control: a refusal that does not concern a control changes nothing
    expect(offered({ status: 'scheduled' }, ['start_deadline_passed'])).toEqual({ pause: true, resume: false, cancel: true })
  })

  it('explains the missing pause of a verification job and says nothing for the others', () => {
    expect(controlsFor(job({ module: 'm1', status: 'running' }), NOW).notes).toEqual(['jobs.controls.noPause.m1'])
    expect(controlsFor(job({ module: 'm2', status: 'running' }), NOW).notes).toEqual([])
    expect(controlsFor(job({ module: 'm1', status: 'scheduled' }), NOW).notes).toEqual([]) // a scheduled verification job can be paused
  })

  it('reads only the three refusals that remove a control', () => {
    expect(refusalOf('pause_not_supported')).toBe('pause_not_supported')
    expect(refusalOf('cancel_not_supported')).toBe('cancel_not_supported')
    expect(refusalOf('start_deadline_passed')).toBe('start_deadline_passed')
    for (const other of ['not_paused', 'not_allowed', 'not_a_draft', undefined, '']) expect(refusalOf(other), String(other)).toBeNull()
  })

  it('has a sentence for what a cancel does, by kind and by stage, and the generic one for a kind it does not know', () => {
    expect(cancelSentences('m1', 'started')).toEqual([t('jobs.cancel.started.m1'), t('jobs.cancel.stopping')])
    expect(cancelSentences('m2', 'started')[0]).toBe(t('jobs.cancel.started.m2'))
    expect(cancelSentences('m3', 'started')[0]).toBe(t('jobs.cancel.started.m3'))
    expect(cancelSentences('m9', 'started')[0]).toBe(t('jobs.cancel.started.other'))
    expect(cancelSentences('m1', 'notStarted')).toEqual([t('jobs.cancel.notStarted.credits')])
    expect(cancelSentences('m3', 'notStarted')).toEqual([t('jobs.cancel.notStarted.m3')])
    // the three kinds do not say the same thing (a test that every sentence is the same would pass this file otherwise)
    expect(new Set([t('jobs.cancel.started.m1'), t('jobs.cancel.started.m2'), t('jobs.cancel.started.m3')]).size).toBe(3)
    expect(t('jobs.cancel.started.m3')).not.toMatch(/credit/)
  })
})

describe('eventText', () => {
  const event = (over: Partial<JobEvent>) => makeEvent(1, { actor_kind: 'user', ...over })
  const move = (from: JobStatus, to: JobStatus, over: Partial<JobEvent> = {}) => eventText(event({ kind: 'transition', from_status: from, to_status: to, ...over }))

  it('has a line of its own for every status a job can move to', () => {
    const lines = new Map<JobStatus, string | null>([
      ['draft', move('previewed', 'draft')],
      ['previewed', move('draft', 'previewed')],
      ['confirmed', move('previewed', 'confirmed')],
      ['scheduled', move('previewed', 'scheduled')],
      ['running', move('confirmed', 'running')],
      ['paused', move('running', 'paused')],
      ['cancelling', move('running', 'cancelling')],
      ['completing', move('running', 'completing')],
      ['succeeded', move('completing', 'succeeded')],
      ['cancelled', move('scheduled', 'cancelled')],
      ['expired', move('scheduled', 'expired')],
      ['failed', move('running', 'failed')],
    ])
    expect([...lines.keys()].sort()).toEqual([...ALL_STATUSES].sort())
    for (const [status, line] of lines) {
      expect(line, status).not.toBeNull()
      expect(line, status).not.toBe(t('jobs.event.transition.other'))
    }
    expect(new Set(lines.values()).size).toBe(12) // twelve moves, twelve different lines
  })

  it('tells a resume, a pause and a cancel by where they came from', () => {
    expect(move('paused', 'running')).toBe(t('jobs.event.resumedRunning'))
    expect(move('confirmed', 'running')).toBe(t('jobs.event.started'))
    expect(move('paused', 'scheduled')).toBe(t('jobs.event.resumedScheduled'))
    expect(move('previewed', 'scheduled')).toBe(t('jobs.event.scheduled'))
    expect(move('scheduled', 'paused')).toBe(t('jobs.event.pausedBeforeStart'))
    expect(move('running', 'paused')).toBe(t('jobs.event.pausedRunning'))
    expect(move('draft', 'cancelled')).toBe(t('jobs.event.discarded'))
    expect(move('previewed', 'cancelled')).toBe(t('jobs.event.discarded'))
    expect(move('confirmed', 'cancelled')).toBe(t('jobs.event.cancelledBeforeStart'))
    expect(move('completing', 'cancelled')).toBe(t('jobs.event.cancelledAfterStart'))
  })

  it('tells the pause limit by the word the platform begins its reason with, and no other reason changes the line', () => {
    const limit = 'pause_limit: the job was paused for longer than the limit of 7 days and the engine cancelled it'
    expect(move('paused', 'cancelled', { actor_kind: 'engine', reason: limit })).toBe(t('jobs.event.pauseLimitCancelled'))
    expect(move('paused', 'cancelling', { actor_kind: 'engine', reason: limit })).toBe(t('jobs.event.pauseLimitCancelling'))
    expect(eventText(event({ kind: 'note', from_status: null, to_status: null, reason: limit }))).toBeNull()
    // paired controls: the same move without the word, a word that only starts the same, and a word in the middle
    expect(move('paused', 'cancelled', { reason: 'cancelled by the user' })).toBe(t('jobs.event.cancelledBeforeStart'))
    expect(move('paused', 'cancelled', { reason: 'pause_limited: something else' })).toBe(t('jobs.event.cancelledBeforeStart'))
    expect(move('paused', 'cancelled', { reason: 'the pause_limit' })).toBe(t('jobs.event.cancelledBeforeStart'))
  })

  it('tells the notes the platform writes on purpose, and any other note as an update', () => {
    const note = (reason: string) => eventText(event({ kind: 'note', from_status: null, to_status: null, reason }))
    expect(note('module_paused: the module reports the job\'s campaign held')).toBe(t('jobs.event.note.paused'))
    expect(note('module_resumed: the module reports the job\'s campaign released')).toBe(t('jobs.event.note.resumed'))
    expect(note('something else entirely')).toBe(t('jobs.event.note.other'))
    expect(note('module_pausedish')).toBe(t('jobs.event.note.other'))
  })

  it('tells the other kinds, and a kind it does not know as one generic line', () => {
    expect(eventText(event({ kind: 'created', from_status: null, to_status: null }))).toBe(t('jobs.event.created'))
    expect(eventText(event({ kind: 'stale', from_status: null, to_status: null }))).toBe(t('jobs.event.stale'))
    expect(eventText(event({ kind: 'flag', from_status: null, to_status: null }))).toBe(t('jobs.event.flag'))
    expect(eventText(event({ kind: 'mystery' as never }))).toBe(t('jobs.event.other'))
    expect(move('running', 'on_fire' as never)).toBe(t('jobs.event.transition.other'))
    expect(eventText(event({ kind: 'transition', from_status: null, to_status: null }))).toBe(t('jobs.event.transition.other'))
  })

  it('names a move an administrator made as the team\'s, with the status in words', () => {
    expect(move('running', 'failed', { actor_kind: 'admin' })).toBe(t('jobs.event.byTeam', { status: 'Failed' }))
    // paired control: the same move by the engine is the plain line
    expect(move('running', 'failed', { actor_kind: 'engine' })).toBe(t('jobs.event.failed'))
  })

  it('never lets the platform\'s reason through, whatever the event', () => {
    const kinds = ['created', 'transition', 'stale', 'flag', 'note', 'unheard-of'] as const
    for (const kind of kinds) {
      for (const to of [...ALL_STATUSES, null]) {
        for (const actor of ['user', 'admin', 'engine'] as const) {
          const line = eventText(event({ kind: kind as never, from_status: to === null ? null : 'running', to_status: to, actor_kind: actor, reason: 'LEAK-reason pause_limit-ish m1_own' }))
          if (line !== null) expect(line, `${kind} ${String(to)} ${actor}`).not.toMatch(/LEAK|m1_own/)
        }
      }
    }
  })
})

describe('format', () => {
  it('writes a date for the user, in the zone asked for, and nothing for a time that is missing or unreadable', () => {
    expect(formatDateTime('2026-12-01T09:30:00Z', 'UTC')).toMatch(/Dec 1, 2026.*9:30/)
    expect(formatDateTime('2026-12-01T09:30:00Z', 'Asia/Tokyo')).toMatch(/Dec 1, 2026.*6:30/) // the same instant on a clock nine hours ahead: 6:30 PM
    expect(formatDateTime(null)).toBe('')
    expect(formatDateTime(undefined)).toBe('')
    expect(formatDateTime('not a date')).toBe('')
  })

  it('names a job by its kind in the customer\'s words and its date, and never by a module name', () => {
    const named = jobName({ module: 'm1', created_at: '2026-12-01T09:30:00Z' }, 'UTC')
    expect(named).toMatch(/^Verification from Dec 1, 2026/)
    expect(kindText('m2')).toBe('Managed sending')
    expect(kindText('m3')).toBe('Your own SMTP')
    expect(kindText('m9')).toBe(t('jobs.kind.other'))
    expect(jobName({ module: 'm9' as ModuleId, created_at: 'nonsense' })).toBe('Job from ')
  })

  it('counts addresses and credits in the singular and the plural, with thousands separated', () => {
    expect(addressCountText({ preview: makePreview({ rows: 1 }) })).toBe('1 address')
    expect(addressCountText({ preview: makePreview({ rows: 0 }) })).toBe('0 addresses')
    expect(addressCountText({ preview: makePreview({ rows: 12345 }) })).toBe('12,345 addresses')
    expect(addressCountText({ preview: null })).toBe(t('jobs.count.none'))
    expect(creditsText(1)).toBe('1 credit')
    expect(creditsText(1200)).toBe('1,200 credits')
    expect(formatNumber(1234567)).toBe('1,234,567')
  })

  it('puts every status in exactly one of: not started, in progress, over', () => {
    for (const status of ALL_STATUSES) {
      const places = [isNotStarted(status), isLive(status), isFinal(status)].filter(Boolean)
      expect(places, status).toHaveLength(1)
    }
    expect(ALL_STATUSES.filter(isLive)).toEqual(['confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing'])
    expect(ALL_STATUSES.filter(isFinal)).toEqual(['succeeded', 'cancelled', 'expired', 'failed'])
    // a job in progress either waits (for its start, for the user) or its items move
    expect(ALL_STATUSES.filter(isWaiting)).toEqual(['scheduled', 'paused'])
    expect(ALL_STATUSES.filter(isWorking)).toEqual(['confirmed', 'running', 'cancelling', 'completing'])
    expect(isWorking('draft')).toBe(false)
    expect(isWorking('succeeded')).toBe(false)
    expect(isLive('teleporting')).toBe(true) // a status it does not know is treated as in progress (it keeps asking, which is the safe side)
  })

  it('counts the days a job\'s content was kept, and says "a few days" when it cannot', () => {
    expect(daysBetween('2026-12-01T09:00:00Z', '2026-12-08T09:00:00Z')).toBe(7)
    expect(daysBetween('2026-12-01T09:00:00Z', '2026-12-01T10:00:00Z')).toBe(1) // at least one
    expect(daysBetween(null, '2026-12-08T09:00:00Z')).toBeNull()
    expect(daysBetween('2026-12-08T09:00:00Z', '2026-12-01T09:00:00Z')).toBeNull()
    expect(daysBetween('x', 'y')).toBeNull()
    expect(daysText(7)).toBe('7 days')
    expect(daysText(1)).toBe('1 day')
    expect(daysText(null)).toBe(t('jobs.days.some'))
  })

  it('names the file by kind, day and the start of the id, and never by anything that is not a letter or a digit', () => {
    expect(csvFileName({ id: '0b000000-0000-4000-8000-000000000001', module: 'm1', created_at: '2026-12-01T08:59:00Z' })).toBe('results-verification-2026-12-01-0b000000.csv')
    expect(csvFileName({ id: 'a/b\\c:d', module: 'm3', created_at: '2026-12-01T08:59:00Z' })).toBe('results-own-smtp-2026-12-01-abcd.csv')
    expect(csvFileName({ id: 'x', module: 'm9' as ModuleId, created_at: 'garbage' })).toBe('results-job-undated-x.csv')
  })

  it('continues a draft on the screen of its kind with the job in the query', () => {
    expect(continuePath({ id: 'abc', module: 'm1' })).toBe('/new/verify?job=abc')
    expect(continuePath({ id: 'abc', module: 'm2' })).toBe('/new/send?job=abc')
    expect(continuePath({ id: 'a b&c', module: 'm3' })).toBe('/new/send?job=a%20b%26c')
  })
})

describe('what the job page says', () => {
  it('says something of its own for every status, and keeps "we cannot tell" for a status it does not know', () => {
    const lines = ALL_STATUSES.map((status) => nowText(makeJob({ status, paused_from: status === 'paused' ? 'running' : null })))
    expect(new Set(lines).size).toBe(ALL_STATUSES.length)
    expect(lines).not.toContain(t('jobs.now.other'))
    expect(nowText({ ...makeJob(), status: 'teleporting' } as unknown as Job)).toBe(t('jobs.now.other'))
    expect(nowText(makeJob({ status: 'scheduled', scheduled_start: null }))).toBe(t('jobs.now.scheduled.noDate'))
  })

  it('says the content was cleared once, by kind, and not at all while it is kept', () => {
    expect(clearedText(makeJob({ module: 'm2', finished_at: '2026-12-01T09:00:00Z', content_cleared_at: '2026-12-08T09:00:00Z' }))).toBe(t('jobs.cleared.send', { after: '7 days' }))
    expect(clearedText(makeJob({ module: 'm1', finished_at: '2026-12-01T09:00:00Z', content_cleared_at: '2026-12-08T09:00:00Z' }))).toBe(t('jobs.cleared.verify', { after: '7 days' }))
    expect(clearedText(makeJob({ content_cleared_at: null }))).toBeNull()
    expect(clearedText({ ...makeJob(), content_cleared_at: undefined })).toBeNull() // an older service does not send the member
    expect(clearedText(makeJob({ finished_at: null, content_cleared_at: '2026-12-08T09:00:00Z' }))).toContain(t('jobs.days.some'))
  })
})

describe('results in words', () => {
  it('says what a result cost from its decision and unit', () => {
    expect(costText({ decision: 'free', unit: null })).toBe('Free')
    expect(costText({ decision: 'charge', unit: 'm1_own' })).toBe('Charged')
    expect(costText({ decision: 'charge', unit: null })).toBe('Charged')
    expect(costText({ decision: 'charge', unit: 'm1_vendor' })).toBe(t('jobs.cost.unit.m1_vendor'))
    expect(costText({ decision: 'charge', unit: 'a_unit_from_the_future' })).toBe('Charged')
    expect(costText({ decision: 'unmapped', unit: null })).toBe('Not charged')
    expect(costText({ decision: 'surprise' as never, unit: null })).toBe(t('jobs.cost.other'))
  })

  it('gives each result the lines that go with it, and none for a result with nothing to add', () => {
    expect(detailLines(makeResult(1, { reason: 'accepted' }))).toEqual([t('common.reason.accepted')])
    expect(detailLines(makeResult(1, { reason: null }))).toEqual([])
    expect(detailLines(makeResult(1, { reason: '' }))).toEqual([])
    expect(detailLines(makeResult(1, { state: 'malformed', reason: 'multiple_at' }))).toEqual([t('common.malformed.multiple_at')])
    expect(detailLines(makeResult(1, { state: 'malformed', reason: 'too_long' }))).toEqual([t('common.malformed.other')])
    expect(detailLines(makeResult(1, { state: 'missing_field', reason: 'a, b' }))).toEqual([t('jobs.results.missingFields', { fields: 'a, b' })])
    expect(detailLines(makeResult(1, { state: 'excluded', reason: 'unknown' }))).toEqual([t('jobs.results.excluded', { verdict: 'unknown' })])
    expect(detailLines(makeResult(1, { state: 'excluded', reason: 'unclassified' }))).toEqual([])
    expect(detailLines(makeResult(1, { reason: 'accepted', suppressed: true }))).toEqual([t('common.reason.accepted'), t('jobs.results.suppressed')])
    expect(detailLines(makeResult(1, { reason: 'zzz_unknown' }))).toEqual([t('common.reason.other')])
  })

  it('lists the outcomes of a page once each, in the order they appear', () => {
    expect(outcomesOf([{ state: 'valid' }, { state: 'invalid' }, { state: 'valid' }, { state: 'risky' }])).toEqual(['valid', 'invalid', 'risky'])
    expect(outcomesOf([])).toEqual([])
  })
})

describe('the reads that take more than one request', () => {
  beforeEach(async () => {
    resetForTest()
    await signInForTest()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    resetForTest()
  })
  const signal = () => new AbortController().signal

  it('counts the results of every page it walks, and says when the walk stopped at its cap', async () => {
    mockApi({ 'GET /v1/jobs/j/results': (url: URL) => ({ results: [makeResult(1), makeResult(2)], next_cursor: url.searchParams.get('cursor') === 'last' ? null : url.searchParams.get('cursor') === 'mid' ? 'last' : 'mid' }) })
    expect(await countResults('j', signal())).toEqual({ count: 6, complete: true })

    const endless = mockApi({ 'GET /v1/jobs/j/results': { results: [makeResult(1), makeResult(2), makeResult(3)], next_cursor: 'again' } })
    expect(await countResults('j', signal())).toEqual({ count: 3 * COUNT_PAGES_MAX, complete: false })
    expect(endless).toHaveBeenCalledTimes(COUNT_PAGES_MAX)
  })

  it('counts a job with no results as complete and zero', async () => {
    mockApi({ 'GET /v1/jobs/j/results': { results: [], next_cursor: null } })
    expect(await countResults('j', signal())).toEqual({ count: 0, complete: true })
  })

  it('reads every page of a job\'s events, in order', async () => {
    mockApi({
      'GET /v1/jobs/j/events': (url: URL) =>
        url.searchParams.get('after') === '2' ? { events: [makeEvent(3)], next_after: null } : { events: [makeEvent(1), makeEvent(2)], next_after: 2 },
    })
    expect((await loadEvents('j', signal())).map((e) => e.id)).toEqual([1, 2, 3])
  })
})

describe('the dashboard\'s rules', () => {
  const NOW = Date.parse('2026-12-01T12:00:00Z')

  it('says your own SMTP in plain words for each of its states', () => {
    const until = '2026-12-31T12:00:00Z'
    const on = m3Summary({ enabled: true, until, state: 'active' }, NOW)
    expect(on).toMatchObject({ state: 'on', token: 'valid', lines: [t('dashboard.m3.enabledUntil', { date: formatDateTime(until) })] })
    expect(m3Summary({ enabled: true, until: null, state: 'active' }, NOW).lines).toEqual([t('dashboard.m3.enabled')])
    expect(m3Summary({ enabled: true, until, state: 'unknown' }, NOW).lines).toEqual([t('dashboard.m3.enabledUntil', { date: formatDateTime(until) }), t('dashboard.m3.unconfirmed')])
    expect(m3Summary({ enabled: true, until, state: 'suspended' }, NOW)).toMatchObject({ state: 'suspended', token: 'failed', lines: [t('dashboard.m3.suspended')] })
    expect(m3Summary({ enabled: false, until: null, state: 'unknown' }, NOW)).toMatchObject({ state: 'off', token: 'unknown', lines: [t('dashboard.m3.disabled')] })
    expect(m3Summary({ enabled: false, until: '2026-11-01T00:00:00Z', state: 'active' }, NOW).lines).toEqual([t('dashboard.m3.ended', { date: formatDateTime('2026-11-01T00:00:00Z') })])
    // switched off wins over suspended; an end that is still ahead of an entitlement that is off is not "ended"
    expect(m3Summary({ enabled: false, until: null, state: 'suspended' }, NOW).state).toBe('off')
    expect(m3Summary({ enabled: false, until, state: 'active' }, NOW).lines).toEqual([t('dashboard.m3.disabled')])
    // a date that cannot be read is no date
    expect(m3Summary({ enabled: true, until: 'garbage', state: 'active' }, NOW).lines).toEqual([t('dashboard.m3.enabled')])
  })

  it('joins the jobs the API calls active with the ones in progress it does not (confirmed, finishing), once each, newest first', () => {
    const a = makeJob({ id: 'a', status: 'running', created_at: '2026-12-01T10:00:00Z' })
    const b = makeJob({ id: 'b', status: 'confirmed', created_at: '2026-12-01T11:00:00Z' })
    const c = makeJob({ id: 'c', status: 'succeeded', created_at: '2026-12-01T12:00:00Z' })
    const d = makeJob({ id: 'd', status: 'draft', created_at: '2026-12-01T13:00:00Z' })
    expect(inProgress([a], [d, c, b, a]).map((j) => j.id)).toEqual(['b', 'a'])
    expect(inProgress([], [d, c]).map((j) => j.id)).toEqual([]) // a finished job and a draft are not in progress
  })
})

describe('useRemote', () => {
  function deferred<T>() {
    let resolve!: (v: T) => void
    let reject!: (e: unknown) => void
    const promise = new Promise<T>((res, rej) => {
      resolve = res
      reject = rej
    })
    return { promise, resolve, reject }
  }

  function setup<T>(load: (s: AbortSignal) => Promise<T>) {
    const handle: { current: Remote<T> | null } = { current: null }
    function Probe() {
      const remote = useRemote(load, [])
      handle.current = remote
      return <p>{remote.error !== null ? `error ${remote.error.code}` : remote.data === undefined ? 'nothing' : String(remote.data)}</p>
    }
    render(<Probe />)
    return handle as { current: Remote<T> }
  }

  it('loads once, shows the answer, and keeps it when a silent refresh fails', async () => {
    let n = 0
    const remote = setup(async () => {
      n++
      if (n === 2) throw new ApiError({ status: 500, code: 'internal_error' })
      return `answer ${n}`
    })
    await screen.findByText('answer 1')
    await act(async () => remote.current.refresh())
    expect(screen.getByText('answer 1')).toBeInTheDocument() // the failed refresh is forgotten, the answer stays
    await act(async () => remote.current.refresh())
    expect(screen.getByText('answer 3')).toBeInTheDocument()
  })

  it('drops an answer that arrives after replace() put a newer one in (a poll that began before a pause cannot undo it)', async () => {
    const slow = deferred<string>()
    let first = true
    const remote = setup((): Promise<string> => {
      if (first) {
        first = false
        return Promise.resolve('loaded')
      }
      return slow.promise
    })
    await screen.findByText('loaded')
    let refreshing!: Promise<void>
    act(() => {
      refreshing = remote.current.refresh()
    })
    act(() => remote.current.replace('answer of the action'))
    await act(async () => {
      slow.resolve('stale poll')
      await refreshing
    })
    expect(screen.getByText('answer of the action')).toBeInTheDocument()
    expect(screen.queryByText('stale poll')).not.toBeInTheDocument()
  })

  it('paired control: without replace() the refresh\'s answer lands', async () => {
    const slow = deferred<string>()
    let first = true
    const remote = setup((): Promise<string> => {
      if (first) {
        first = false
        return Promise.resolve('loaded')
      }
      return slow.promise
    })
    await screen.findByText('loaded')
    let refreshing!: Promise<void>
    act(() => {
      refreshing = remote.current.refresh()
    })
    await act(async () => {
      slow.resolve('fresh poll')
      await refreshing
    })
    expect(screen.getByText('fresh poll')).toBeInTheDocument()
  })

  it('a failed load shows the error and no data, and retry loads again', async () => {
    let fail = true
    const remote = setup(async () => {
      if (fail) throw new ApiError({ status: 404, code: 'not_found' })
      return 'found'
    })
    await screen.findByText('error not_found')
    fail = false
    act(() => remote.current.retry())
    await screen.findByText('found')
  })

  it('turns anything that is not an ApiError into the "could not read the reply" error, and never shows its message', async () => {
    expect(asApiError(new TypeError('LEAK internal detail')).code).toBe('bad_response')
    expect(asApiError(new ApiError({ status: 400, code: 'bad_request' })).code).toBe('bad_request')
    const remote = setup(async () => {
      throw new TypeError('LEAK internal detail')
    })
    await screen.findByText('error bad_response')
    expect(remote.current.error?.message).not.toContain('LEAK')
  })
})
