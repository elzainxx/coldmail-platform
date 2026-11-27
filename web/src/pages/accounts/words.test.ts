// The pure parts of the accounts screens: the words of the API's vocabularies, the form's reading and validation, the header of a bulk file, the wait of a rate limit.
// Each has a case that must come out the other way beside it.
import { describe, expect, it } from 'vitest'
import { ApiError } from '../../api/errors.ts'
import { t } from '../../copy/index.ts'
import { ACCOUNT_FIELDS, BULK_HEADER, isAccountField, parseAccountForm } from './accountForm.ts'
import { apiBlob, apiText, hasAccountLines, isScreenHeader, screenHeader } from './bulkFile.ts'
import {
  badLineText,
  bulkAdded,
  bulkDetail,
  bulkLabel,
  bulkToken,
  entitlementLine,
  failureOf,
  fromGateLabel,
  fromGateToken,
  fromGateWhy,
  stageText,
  statusLabel,
  statusReason,
  statusToken,
  verificationLine,
  verifyWhy,
  waitText,
} from './words.ts'

const apiError = (code: string, details?: Record<string, unknown>, extra: { retryAfterSeconds?: number } = {}) =>
  new ApiError({ status: 400, code, details, requestId: 'req_00000000000000000000000000000001', ...extra })

function form(values: Record<string, string>): FormData {
  const data = new FormData()
  for (const [k, v] of Object.entries(values)) data.set(k, v)
  return data
}

const VALID = { smtp_host: 'smtp.example.test', smtp_port: '', smtp_username: 'ann', smtp_secret: ' p w ', from_address: 'ann@example.test', from_name: '', daily_limit: '' }

const readText = (blob: Blob): Promise<string> =>
  new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsText(blob)
  })

describe('waitText', () => {
  it.each([
    [1, t('accounts.wait.second')],
    [2, t('accounts.wait.seconds', { count: 2 })],
    [59, t('accounts.wait.seconds', { count: 59 })],
    [60, t('accounts.wait.minute')],
    [89, t('accounts.wait.minute')],
    [90, t('accounts.wait.minutes', { count: 2 })],
    [300, t('accounts.wait.minutes', { count: 5 })],
    [3540, t('accounts.wait.minutes', { count: 59 })],
    [3599, t('accounts.wait.hour')],
    [3600, t('accounts.wait.hour')],
    [5400, t('accounts.wait.hours', { count: 2 })],
    [10800, t('accounts.wait.hours', { count: 3 })],
  ])('%s seconds is "%s"', (seconds, line) => {
    expect(waitText(seconds)).toBe(line)
  })

  it('invents no wait when the answer gave none', () => {
    expect(waitText(undefined)).toBeNull()
    expect(waitText(0)).toBeNull()
    expect(waitText(-5)).toBeNull()
    expect(waitText(Number.NaN)).toBeNull()
  })
})

describe('the words of an account', () => {
  it('gives each state of the From address check its own word, line and colour, and an unknown state the "could not be determined" ones', () => {
    const states = ['unverified', 'aligned', 'unaligned_warned', 'blocked', 'indeterminate']
    expect(new Set(states.map(fromGateLabel)).size).toBe(5)
    expect(new Set(states.map(fromGateWhy)).size).toBe(5)
    expect(fromGateToken('aligned')).toBe('valid')
    expect(fromGateToken('blocked')).toBe('invalid')
    expect(fromGateLabel('something_new')).toBe(t('accounts.fromGate.indeterminate'))
    expect(fromGateWhy('something_new')).toBe(t('accounts.fromGate.indeterminateWhy'))
    for (const s of [...states, 'something_new']) {
      expect(fromGateLabel(s)).not.toMatch(/_/)
      expect(fromGateWhy(s)).not.toMatch(/_/)
    }
  })

  it('says each status in words, with a colour, and why an account is paused or on hold only when it is', () => {
    expect(statusLabel('active')).toBe('Active')
    expect(statusLabel('quarantined')).toBe('On hold')
    expect(statusLabel('on_fire')).toBe('Active') // an unknown status is not shown as its word
    expect(statusToken('active')).toBe('valid')
    expect(statusToken('on_fire')).toBe('unknown')
    expect(statusReason({ status: 'active', pause_reason: 'daily_limit', quarantine_reason: 'auth_failure' })).toBeNull() // control: an active account has no reason
    expect(statusReason({ status: 'paused', pause_reason: 'daily_limit', quarantine_reason: null })).toBe(t('accounts.pause.dailyLimit'))
    expect(statusReason({ status: 'quarantined', pause_reason: null, quarantine_reason: 'auth_failure' })).toBe(t('accounts.hold.authFailure'))
    expect(statusReason({ status: 'quarantined', pause_reason: null, quarantine_reason: 'unrecognised' })).toBeNull()
    expect(statusReason({ status: 'paused', pause_reason: null, quarantine_reason: null })).toBeNull()
  })

  it('says why a login check failed, and what the test got as far as', () => {
    expect(verifyWhy('verify_auth_failed')).toBe(t('accounts.why.authFailed'))
    expect(verifyWhy('verify_somethingnew')).toBe(t('accounts.why.error'))
    expect(verifyWhy(null)).toBe(t('accounts.why.error'))
    expect(verificationLine({ verification: 'verified', verify_code: null })).toBe(t('accounts.verification.verified'))
    expect(verificationLine({ verification: 'unverified', verify_code: null })).toBe(t('accounts.verification.unverified'))
    expect(verificationLine({ verification: 'failed', verify_code: 'verify_timeout' })).toBe(t('accounts.verification.failed', { why: t('accounts.why.timeout') }))
    const stages = ['done', 'connect', 'host', 'tls', 'login', 'protocol', 'unavailable']
    expect(new Set(stages.map(stageText)).size).toBe(7)
    expect(stageText('a_new_stage')).toBe(t('accounts.stage.unavailable'))
  })

  it('reads whether sending through the own accounts is switched on', () => {
    const now = new Date('2026-12-01T12:00:00Z')
    expect(entitlementLine({ enabled: false, until: null, state: 'unknown' }, now)).toBe(t('accounts.entitlement.none'))
    expect(entitlementLine({ enabled: false, until: '2026-06-15T12:00:00Z', state: 'active' }, now)).toBe(t('accounts.entitlement.ended', { date: 'June 15, 2026' }))
    expect(entitlementLine({ enabled: false, until: '2027-06-15T12:00:00Z', state: 'active' }, now)).toBe(t('accounts.entitlement.none')) // switched off before its end
    expect(entitlementLine({ enabled: true, until: null, state: 'suspended' }, now)).toBe(t('accounts.entitlement.suspended'))
    expect(entitlementLine({ enabled: true, until: '2027-06-15T12:00:00Z', state: 'active' }, now)).toBe(t('accounts.entitlement.until', { date: 'June 15, 2027' }))
    expect(entitlementLine({ enabled: true, until: null, state: 'active' }, now)).toBeNull()
    expect(entitlementLine({ enabled: true, until: null, state: 'unknown' }, now)).toBeNull()
  })
})

describe('the lines of a bulk add', () => {
  it('has a word and a colour for every code, and a code of the future reads as not added', () => {
    const codes = ['created', 'created_unverified', 'account_duplicate', 'from_gate_ack_required', 'host_forbidden', 'account_limit_reached', 'validation_failed', 'account_action_unavailable', 'not_found', 'internal_error']
    for (const code of codes) {
      expect(bulkLabel(code), code).not.toBe('')
      expect(bulkLabel(code)).not.toMatch(/_/)
    }
    expect(bulkLabel('created')).toBe(t('accounts.bulk.created'))
    expect(bulkLabel('a_code_from_the_future')).toBe(t('accounts.bulk.failed'))
    expect(bulkToken('created')).toBe('valid')
    expect(bulkToken('host_forbidden')).toBe('invalid')
    expect(bulkToken('a_code_from_the_future')).toBe('failed')
  })

  it('tells what to do about a line that was not added, and nothing about one that was', () => {
    expect(bulkDetail('created')).toBeNull()
    expect(bulkDetail('account_duplicate')).toBe(t('error.account_duplicate'))
    expect(bulkDetail('host_forbidden')).toBe(t('error.host_forbidden'))
    expect(bulkDetail('account_limit_reached')).toBe(t('error.account_limit_reached'))
    expect(bulkDetail('account_action_unavailable')).toBe(t('error.account_action_unavailable'))
    expect(bulkDetail('created_unverified')).toBe(t('accounts.bulk.createdUnverifiedWhy'))
    expect(bulkDetail('from_gate_ack_required')).toBe(t('accounts.bulk.ackRequiredWhy'))
    expect(bulkDetail('validation_failed')).toBe(t('accounts.bulk.invalidWhy'))
    expect(bulkDetail('a_code_from_the_future')).toBe(t('accounts.bulk.failedWhy'))
    expect(bulkAdded('created')).toBe(true)
    expect(bulkAdded('created_unverified')).toBe(true)
    expect(['account_duplicate', 'host_forbidden', 'internal_error', 'a_code_from_the_future'].some(bulkAdded)).toBe(false)
  })

  it('names the line of a refused batch and its fields in words, never a value', () => {
    expect(badLineText(apiError('validation_failed', { line: 3, fields: ['smtp_secret:required', 'smtp_port:out_of_range', 'smtp_secret:malformed'] }))).toBe(
      t('accounts.bulk.badLine', { line: 3, fields: `${t('accounts.field.password')}, ${t('accounts.field.port')}` }),
    )
    expect(badLineText(apiError('validation_failed', { line: 4 }))).toBe(t('accounts.bulk.badLinePlain', { line: 4 }))
    expect(badLineText(apiError('validation_failed', { line: 5, fields: ['weird_field:required'] }))).toBe(t('accounts.bulk.badLine', { line: 5, fields: t('accounts.field.other') }))
    // control: an error that names no line is not one
    expect(badLineText(apiError('validation_failed', { reason: 'x' }))).toBeNull()
    expect(badLineText(apiError('validation_failed', { line: 0 }))).toBeNull()
    expect(badLineText(apiError('validation_failed', { line: '3' }))).toBeNull()
  })

  it('turns a failed call into a line, the wait of a rate limit and the reference; anything else is the generic line', () => {
    const limited = failureOf(apiError('rate_limited', undefined, { retryAfterSeconds: 120 }))
    expect(limited).toEqual({ text: t('error.rate_limited'), wait: t('accounts.wait.minutes', { count: 2 }), reference: 'Reference req_00000000000000000000000000000001' })
    expect(failureOf(apiError('busy')).wait).toBeNull() // the wait is for a rate limit only
    expect(failureOf(new Error('boom'))).toEqual({ text: t('error.unknown'), wait: null, reference: null })
  })
})

describe('the add form', () => {
  it('reads a valid form: the values trimmed, the password exactly as typed, the optional ones only when given', () => {
    const parsed = parseAccountForm(form({ ...VALID, smtp_host: '  smtp.example.test ', smtp_username: ' ann ', from_address: ' ann@example.test ' }), false)
    expect(parsed.errors).toEqual({})
    expect(parsed.account).toEqual({ smtp_host: 'smtp.example.test', smtp_username: 'ann', smtp_secret: ' p w ', from_address: 'ann@example.test' })
    const full = parseAccountForm(form({ ...VALID, smtp_port: '465', from_name: ' Ann ', daily_limit: '300' }), true)
    expect(full.account).toEqual({ smtp_host: 'smtp.example.test', smtp_username: 'ann', smtp_secret: ' p w ', from_address: 'ann@example.test', smtp_port: 465, from_name: 'Ann', daily_limit: 300, acknowledge_from_gate: true })
  })

  it('carries the acknowledgement only when it was given', () => {
    expect(parseAccountForm(form(VALID), false).account).not.toHaveProperty('acknowledge_from_gate')
    expect(parseAccountForm(form(VALID), true).account?.acknowledge_from_gate).toBe(true)
  })

  it.each([
    ['smtp_host', '', 'required'],
    ['smtp_host', '   ', 'required'],
    ['smtp_host', 'two words', 'malformed'],
    ['smtp_host', 'h'.repeat(1025), 'malformed'],
    ['smtp_username', '', 'required'],
    ['smtp_secret', '', 'required'],
    ['from_address', '', 'required'],
    ['from_address', 'no-at-sign', 'malformed'],
    ['from_address', 'a@b@c', 'malformed'],
    ['from_address', 'a b@c.test', 'malformed'],
    ['smtp_port', '0', 'out_of_range'],
    ['smtp_port', '65536', 'out_of_range'],
    ['smtp_port', '58a', 'malformed'],
    ['smtp_port', '-1', 'malformed'],
    ['smtp_port', '1.5', 'malformed'],
    ['daily_limit', '0', 'out_of_range'],
    ['daily_limit', 'lots', 'malformed'],
    ['from_name', 'n'.repeat(1025), 'malformed'],
  ])('refuses %s = %j as %s, by field, and builds no account', (field, value, issue) => {
    const parsed = parseAccountForm(form({ ...VALID, [field]: value }), false)
    expect(parsed.account).toBeNull()
    expect(Object.keys(parsed.errors)).toEqual([field])
    expect(parsed.errors[field as keyof typeof parsed.errors]).toBe(t(`error.field.${issue}` as 'error.field.required'))
  })

  it.each([
    ['smtp_port', '1'],
    ['smtp_port', '65535'],
    ['smtp_port', ' 587 '],
    ['daily_limit', '1'],
    ['from_name', 'n'.repeat(1024)],
  ])('control: %s = %j is accepted', (field, value) => {
    const parsed = parseAccountForm(form({ ...VALID, [field]: value }), false)
    expect(parsed.errors).toEqual({})
    expect(parsed.account).not.toBeNull()
  })

  it('lists every field it can name, and only those', () => {
    expect([...ACCOUNT_FIELDS]).toEqual(['smtp_host', 'smtp_port', 'smtp_username', 'smtp_secret', 'from_address', 'from_name', 'daily_limit'])
    expect(isAccountField('smtp_host')).toBe(true)
    expect(isAccountField('user_id')).toBe(false)
  })
})

describe('the first line of a bulk add', () => {
  it('knows its own header however it is written, and no other', () => {
    expect(isScreenHeader(screenHeader())).toBe(true)
    expect(isScreenHeader(` ${screenHeader().toUpperCase()} `)).toBe(true)
    expect(isScreenHeader(`\uFEFF${screenHeader()}`)).toBe(true) // a byte order mark in front
    expect(isScreenHeader(screenHeader().replace('Port', 'Number'))).toBe(false)
    expect(isScreenHeader(BULK_HEADER)).toBe(false)
    expect(isScreenHeader('')).toBe(false)
  })

  it('is a list only when there is more than the header', () => {
    expect(hasAccountLines('')).toBe(false)
    expect(hasAccountLines('\n\n')).toBe(false)
    expect(hasAccountLines(`${screenHeader()}\n`)).toBe(false)
    expect(hasAccountLines(`${BULK_HEADER}\r\n`)).toBe(false)
    expect(hasAccountLines(`${screenHeader()}\nsmtp.example.test,587,ann,pw,ann@example.test,,`)).toBe(true)
    expect(hasAccountLines('smtp.example.test,587,ann,pw,ann@example.test,,')).toBe(true) // one line that is not the header: the API says what is wrong with it
  })

  it('replaces the header and nothing else of the pasted text, whatever the line ends are', () => {
    const rows = 'smtp.example.test,587,ann,"p,w",ann@example.test,,\r\nsmtp.example.test,587,bob,pw2,bob@example.test,Bob,10'
    expect(apiText(`${screenHeader()}\r\n${rows}`)).toBe(`${BULK_HEADER}\r\n${rows}`)
    expect(apiText(`${screenHeader()}\n${rows}`)).toBe(`${BULK_HEADER}\n${rows}`)
    expect(apiText(screenHeader())).toBe(BULK_HEADER)
    // control: text that does not begin with the screen's header is not touched
    expect(apiText(`${BULK_HEADER}\n${rows}`)).toBe(`${BULK_HEADER}\n${rows}`)
    expect(apiText(rows)).toBe(rows)
    expect(apiText(`${rows}\n${screenHeader()}`)).toBe(`${rows}\n${screenHeader()}`) // the header is only the first line
  })

  it('does the same for a file, handing on every byte after the first line (multi-byte characters included)', async () => {
    const rows = 'smtp.example.test,587,ann,pässwörd-日本,ann@example.test,Ánn,\n'
    const out = await apiBlob(new Blob([`${screenHeader()}\n${rows}`], { type: 'text/csv' }))
    expect(await readText(out)).toBe(`${BULK_HEADER}\n${rows}`)
    expect(out.type).toBe('text/csv')
    const withBom = await apiBlob(new Blob([`\uFEFF${screenHeader()}\r\n${rows}`]))
    expect(await readText(withBom)).toBe(`${BULK_HEADER}\r\n${rows}`) // the mark goes with the line it was on
  })

  it('control: a file whose first line is not the screen\'s header is the very same file', async () => {
    const own = new Blob([`${BULK_HEADER}\nsmtp.example.test,587,ann,pw,ann@example.test,,\n`])
    expect(await apiBlob(own)).toBe(own)
    const other = new Blob(['email,name\na@b.test,A\n'])
    expect(await apiBlob(other)).toBe(other)
  })
})
