import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  API_ERROR_CODES,
  ApiError,
  CLIENT_ERROR_CODES,
  M3_BULK_ERROR_CODES,
  NOT_ALLOWED_REASONS,
  PREVIEW_STALE_REASONS,
  apiErrorFromResponse,
} from '../api/errors.ts'
import { accounts } from './accounts.ts'
import { auth } from './auth.ts'
import { common } from './common.ts'
import { dashboard } from './dashboard.ts'
import { errors } from './errors.ts'
import { catalogue, errorReference, errorText, fill, maybeT, t, templateProblem } from './index.ts'
import { jobs } from './jobs.ts'
import { newjob } from './newjob.ts'
import { settings } from './settings.ts'
import { suppressions } from './suppressions.ts'
import { addressReasonOf, fieldIssueText, fieldIssuesOf, jobStatusText, malformedReasonText, missingFieldNames, resultReasonText, resultStateText } from './words.ts'

afterEach(() => {
  vi.restoreAllMocks()
})

// ---------- the rules, as pure functions so a control can feed them a bad input ----------

const INTERNAL_WORDS = [
  'module',
  'engine',
  'slug',
  'preview_stale',
  'idempot',
  'fingerprint',
  'payload',
  'cursor',
  'jwt',
  'gotrue',
  'supabase',
  'http',
  'json',
  'request_id',
  'unauthenticated',
  'infeasible',
]

/** What is wrong with one catalogue string, as readable lines ([] when it is fine). */
function copyViolations(key: string, value: string, codeWords: readonly string[]): string[] {
  const bad: string[] = []
  const text = value.replace(/\{[A-Za-z][A-Za-z0-9]*\}/g, '') // a {name} is a placeholder, not text
  if (value.trim() === '') bad.push(`${key}: empty`)
  if (/[{}]/.test(text)) bad.push(`${key}: a stray brace`)
  if (/\b[A-Za-z0-9]+_[A-Za-z0-9_]+\b/.test(text)) bad.push(`${key}: a snake_case token`)
  if (/`/.test(text)) bad.push(`${key}: a backtick`)
  const lower = text.toLowerCase()
  for (const w of codeWords) {
    if (lower.includes(w)) bad.push(`${key}: the code word "${w}"`)
  }
  for (const w of INTERNAL_WORDS) {
    if (new RegExp(`\\b${w}`, 'i').test(text)) bad.push(`${key}: the internal word "${w}"`)
  }
  return bad
}

// Every API code and reason that has an underscore is banned as a substring (the snake_case rule above covers it too); the single-word ones that are
// plain English (busy, suspended) may appear in a sentence, so they are not in this list.
const codeWords = [...API_ERROR_CODES, ...NOT_ALLOWED_REASONS, ...PREVIEW_STALE_REASONS, ...CLIENT_ERROR_CODES].filter((w) => w.includes('_'))

/** The words of internal-words.json: an array of strings, or an object with a `words` array (the same reading as web/e2e/support/copy.ts). */
function denyList(json: string): string[] {
  const raw: unknown = JSON.parse(json)
  const list: unknown = Array.isArray(raw) ? raw : (raw as { words?: unknown } | null)?.words
  if (!Array.isArray(list) || list.length === 0 || !list.every((w): w is string => typeof w === 'string' && w.trim() !== '')) {
    throw new Error('internal-words.json must be a non-empty array of words, or an object with a non-empty "words" array of strings')
  }
  return [...new Set(list.map((w) => w.trim().toLowerCase()))]
}

function denyViolations(key: string, value: string, words: string[]): string[] {
  return words.filter((w) => new RegExp(`(?<![A-Za-z0-9_])${w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?![A-Za-z0-9_])`, 'i').test(value)).map((w) => `${key}: the internal word "${w}"`)
}

/** The catalogue keys a source text uses: any string literal that has the shape of a key. */
const KEY_SHAPE = /(['"`])((?:common|auth|error|dashboard|jobs|newjob|accounts|suppressions|settings)\.[A-Za-z0-9_.]+)\1/g
function usedKeys(source: string): string[] {
  // A key in a comment (an example in a doc line) is not a use.
  const code = source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1')
  return [...code.matchAll(KEY_SHAPE)].map((m) => m[2] as string)
}

/** The codes of the error table of Docs/jobs-api.md and the words of `not_allowed`'s reason cell. */
function docErrorTable(md: string): { codes: string[]; notAllowedReasons: string[] } {
  const codes: string[] = []
  let notAllowedReasons: string[] = []
  for (const line of md.split('\n')) {
    const m = /^\| (\d{3}) \| `([a-z_]+)` \|/.exec(line)
    if (!m) continue
    codes.push(m[2] as string)
    if (m[2] === 'not_allowed') {
      const cells = line.split('|')
      const details = cells[cells.length - 2] ?? ''
      notAllowedReasons = [...details.matchAll(/`([a-z_]+)`/g)].map((r) => r[1] as string).filter((r) => r !== 'reason')
    }
  }
  return { codes, notAllowedReasons }
}

// ---------- the catalogue ----------

describe('the catalogue', () => {
  const files = { common, auth, errors, dashboard, jobs, newjob, accounts, suppressions, settings }

  it('merges every file without losing a key to a duplicate', () => {
    const total = Object.values(files).reduce<number>((n, f) => n + Object.keys(f).length, 0)
    expect(total).toBeGreaterThan(150)
    expect(Object.keys(catalogue).length).toBe(total)
  })

  it('keeps every key of a file under that file\'s own prefix', () => {
    const prefix: Record<string, string> = { errors: 'error' }
    for (const [name, file] of Object.entries(files)) {
      const want = `${prefix[name] ?? name}.`
      for (const key of Object.keys(file)) {
        expect(key.startsWith(want), `${key} in ${name}.ts`).toBe(true)
      }
    }
  })

  it('has no string with a code word, a snake_case token, an internal word, a backtick or a stray brace', () => {
    const found = Object.entries(catalogue).flatMap(([k, v]) => copyViolations(k, v, codeWords))
    expect(found).toEqual([])
  })

  it('holds no word of internal-words.json, the deny-list the e2e scope also checks the rendered pages against', () => {
    const words = denyList(readFileSync('src/copy/internal-words.json', 'utf8'))
    expect(words.length).toBeGreaterThanOrEqual(1) // a deny-list that denies nothing proves nothing
    const found = Object.entries(catalogue).flatMap(([k, v]) => denyViolations(k, v, words))
    expect(found).toEqual([])
  })

  it('paired control: the deny-list check does find a word, as a whole word only, and refuses an empty list', () => {
    expect(denyViolations('k', 'Your preview_stale job', ['preview_stale'])).toHaveLength(1)
    expect(denyViolations('k', 'Your PREVIEW_STALE job', ['preview_stale'])).toHaveLength(1)
    expect(denyViolations('k', 'No ledgers here', ['ledger'])).toEqual([])
    expect(denyList('{"words":["A","a"," b "]}')).toEqual(['a', 'b'])
    expect(denyList('["x"]')).toEqual(['x'])
    expect(() => denyList('{"words":[]}')).toThrow()
    expect(() => denyList('{"words":[1]}')).toThrow()
  })

  it('paired control: the rules do catch each kind of bad string', () => {
    expect(copyViolations('k', 'Your preview_stale job', codeWords)).not.toEqual([])
    expect(copyViolations('k', 'The module said no', codeWords)).not.toEqual([])
    expect(copyViolations('k', 'Run `npm test`', codeWords)).not.toEqual([])
    expect(copyViolations('k', 'Could not use {name', codeWords)).not.toEqual([])
    expect(copyViolations('k', 'Your reason was not_a_draft', [])).not.toEqual([]) // snake_case alone is enough
    expect(copyViolations('k', 'Needs {needed} credits', codeWords)).toEqual([])
  })
})

describe('t', () => {
  it('fills {name} from the values', () => {
    expect(t('common.signedInAs', { email: 'a@b.test' })).toBe('Signed in as a@b.test')
    expect(t('common.pagination.page', { n: 3 })).toBe('Page 3')
  })

  it('a key that is not in the catalogue is a compile error (this line is checked by tsc, not by vitest)', () => {
    // @ts-expect-error an unknown key must not type-check
    expect(t('no.such.key')).toBe('')
  })

  it('a value left out is logged and shown empty, never as {name}', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    expect(t('common.signedInAs')).toBe('Signed in as ')
    expect(warn).toHaveBeenCalled()
  })

  it('fill reports a missing value as null and maybeT never invents a line', () => {
    expect(fill('a {x} b', { x: 1 })).toBe('a 1 b')
    expect(fill('a {x} b', {})).toBeNull()
    expect(maybeT('common.nope.nothing')).toBeNull()
  })
})

// ---------- errors ----------

function apiError(code: string, details?: Record<string, unknown>, status = 409): ApiError {
  return new ApiError({ status, code, details, requestId: 'req_00000000000000000000000000000001' })
}

describe('errorText', () => {
  it('has a line for every API code, and none of them is the generic one', () => {
    for (const code of API_ERROR_CODES) {
      expect(maybeT(`error.${code}`) ?? maybeT(`error.${code}.plain`), code).not.toBeNull()
      const text = errorText(apiError(code))
      expect(text, code).not.toBe(t('error.unknown'))
    }
  })

  it('has a line for every reason of not_allowed and of preview_stale, and uses it', () => {
    for (const reason of NOT_ALLOWED_REASONS) {
      expect(maybeT(`error.not_allowed.${reason}`), reason).not.toBeNull()
      expect(errorText(apiError('not_allowed', { reason }))).toBe(maybeT(`error.not_allowed.${reason}`))
    }
    for (const reason of PREVIEW_STALE_REASONS) {
      expect(errorText(apiError('preview_stale', { reason }))).toBe(maybeT(`error.preview_stale.${reason}`))
    }
    // two reasons give two different lines (the lookup really reads the reason)
    expect(errorText(apiError('not_allowed', { reason: 'not_paused' }))).not.toBe(errorText(apiError('not_allowed', { reason: 'not_a_draft' })))
  })

  it('has a line for the errors the app makes itself', () => {
    for (const code of CLIENT_ERROR_CODES) {
      expect(errorText(apiError(code, undefined, 0)), code).not.toBe(t('error.unknown'))
    }
  })

  it('shows one generic line for a code it does not know, never the code, and logs the code', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const text = errorText(apiError('quota_gremlin_exceeded'))
    expect(text).toBe(t('error.unknown'))
    expect(text).not.toContain('quota')
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('quota_gremlin_exceeded'))
  })

  it('an unknown reason of a known code gets the code\'s line, and logs the reason', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    expect(errorText(apiError('not_allowed', { reason: 'brand_new_reason' }))).toBe(t('error.not_allowed'))
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('brand_new_reason'))
  })

  it('does not read a reason of a code whose reason is a developer sentence (validation_failed)', () => {
    const text = errorText(apiError('validation_failed', { reason: 'content.subject must not be blank' }, 400))
    expect(text).toBe(t('error.validation_failed'))
    expect(text).not.toContain('subject must')
  })

  it('never shows or keeps the server\'s message', () => {
    const body = JSON.stringify({ error: { code: 'busy', message: 'LEAK-the-server-sentence', request_id: 'req_1', details: { reason: 'LEAK-reason' } } })
    const err = apiErrorFromResponse(503, body, '1')
    expect(JSON.stringify({ ...err, message: err.message })).not.toContain('LEAK-the-server-sentence')
    expect(errorText(err)).not.toContain('LEAK')
    expect(err.message).toBe('busy')
    expect(err.retryAfterSeconds).toBe(1)
  })

  it('puts the numbers of an insufficient_credits error in the line, and uses the plain line when they are missing', () => {
    const withNumbers = errorText(apiError('insufficient_credits', { available: 1200, needed: 15000, shortfall: 13800 }, 402))
    expect(withNumbers).toContain('15,000')
    expect(withNumbers).toContain('1,200')
    const plain = errorText(apiError('insufficient_credits', undefined, 402))
    expect(plain).toBe(t('error.insufficient_credits.plain'))
    expect(plain).not.toMatch(/\{|\d/)
  })

  it('names the limit of a payload or a list in words', () => {
    expect(errorText(apiError('payload_too_large', { limit_bytes: 64 * 1024 * 1024 }, 413))).toContain('64 MB')
    expect(errorText(apiError('too_many_rows', { limit: 100000 }, 413))).toContain('100,000')
  })

  it('shows the position of a mistake in a template, and templateProblem carries the template package\'s own words', () => {
    const err = apiError('validation_failed', { reason: 'the html template has a syntax error', part: 'html', line: 2, column: 5, message: 'unexpected }: write \\} for a literal brace' }, 400)
    const text = errorText(err)
    expect(text).toContain('HTML body')
    expect(text).toContain('line 2')
    expect(text).toContain('column 5')
    expect(text).not.toContain('unexpected')
    expect(templateProblem(err)).toEqual({ part: 'html', line: 2, column: 5, message: 'unexpected }: write \\} for a literal brace' })
  })

  it('templateProblem is null for anything that is not a template mistake', () => {
    expect(templateProblem(apiError('validation_failed', { reason: 'x' }, 400))).toBeNull()
    expect(templateProblem(apiError('validation_failed', { part: 'cc', line: 1, column: 1 }, 400))).toBeNull()
    expect(templateProblem(apiError('busy', { part: 'html', line: 1, column: 1 }, 503))).toBeNull()
  })

  it('gives the reference of an error for a support request', () => {
    expect(errorReference(apiError('internal_error'))).toBe('Reference req_00000000000000000000000000000001')
    expect(errorReference(new ApiError({ status: 500, code: 'internal_error' }))).toBeNull()
  })
})

describe('the error catalogue matches the API', () => {
  it('has no orphan: every error.* key belongs to a code, a reason or a documented suffix', () => {
    const codes = new Set<string>([...API_ERROR_CODES, ...CLIENT_ERROR_CODES, ...M3_BULK_ERROR_CODES, 'unknown', 'field'])
    const issues = ['required', 'out_of_range', 'malformed', 'not_found', 'conflict', 'forbidden_value']
    const reasons = new Set<string>([...NOT_ALLOWED_REASONS, ...PREVIEW_STALE_REASONS, ...issues, 'plain', 'template', 'account'])
    for (const key of Object.keys(errors)) {
      const [, code, suffix] = key.split('.')
      expect(codes.has(code ?? ''), `${key}: the code`).toBe(true)
      if (suffix !== undefined) expect(reasons.has(suffix), `${key}: the suffix`).toBe(true)
    }
  })

  it('reads the error table of Docs/jobs-api.md and finds a line for every code and every not_allowed reason in it', () => {
    const md = readFileSync('../Docs/jobs-api.md', 'utf8').replace(/\r/g, '')
    const { codes, notAllowedReasons } = docErrorTable(md)
    // a gate whose subject is empty is red: the instrument must have found the table
    expect(codes.length).toBeGreaterThanOrEqual(20)
    expect(notAllowedReasons.length).toBeGreaterThanOrEqual(8)
    const missingCodes = codes.filter((c) => maybeT(`error.${c}`) === null && maybeT(`error.${c}.plain`) === null)
    const missingReasons = notAllowedReasons.filter((r) => r !== 'not_allowed' && maybeT(`error.not_allowed.${r}`) === null)
    expect(missingCodes, 'a code of Docs/jobs-api.md with no wording in src/copy/errors.ts').toEqual([])
    expect(missingReasons, 'a not_allowed reason of Docs/jobs-api.md with no wording in src/copy/errors.ts').toEqual([])
    // and the list in api/errors.ts is the same list
    expect([...codes].sort()).toEqual([...API_ERROR_CODES].sort())
  })

  it('paired control: a doc with a code the catalogue lacks is reported', () => {
    const md = '| 409 | `brand_new` | when | none |\n| 409 | `not_allowed` | x | `reason`: `not_a_draft` or `shiny_new` |'
    const { codes, notAllowedReasons } = docErrorTable(md)
    expect(codes).toEqual(['brand_new', 'not_allowed'])
    expect(notAllowedReasons).toEqual(['not_a_draft', 'shiny_new'])
    expect(codes.filter((c) => maybeT(`error.${c}`) === null)).toEqual(['brand_new'])
    expect(notAllowedReasons.filter((r) => maybeT(`error.not_allowed.${r}`) === null)).toEqual(['shiny_new'])
  })
})

// ---------- the keys the source uses ----------

const sources = import.meta.glob(['/src/**/*.{ts,tsx}', '!/src/**/*.test.{ts,tsx}', '!/src/copy/**', '!/src/**/*.d.ts'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

describe('the keys src/** uses', () => {
  it('are all in the catalogue', () => {
    const files = Object.keys(sources)
    const used = new Set(Object.values(sources).flatMap(usedKeys))
    // a gate whose subject is empty is red
    expect(files.length).toBeGreaterThanOrEqual(25)
    expect(used.size).toBeGreaterThanOrEqual(25)
    const missing = [...used].filter((k) => !(k in catalogue))
    expect(missing, 'a key used in src/** that no src/copy file defines').toEqual([])
  })

  it('does not count a key that is only written in a comment', () => {
    expect(usedKeys("// t('jobs.example')\nconst a = 1 /* t('jobs.other') */\nconst b = t('common.brand')")).toEqual(['common.brand'])
  })

  it('paired control: the scan does find a key that is not in the catalogue', () => {
    const fixture = `const a = t('common.brand'); const b = t("jobs.noSuchKey"); const c = { title: 'settings.alsoMissing' }`
    const found = usedKeys(fixture)
    expect(found).toEqual(['common.brand', 'jobs.noSuchKey', 'settings.alsoMissing'])
    expect(found.filter((k) => !(k in catalogue))).toEqual(['jobs.noSuchKey', 'settings.alsoMissing'])
  })
})

// ---------- the words of the API's vocabularies ----------

describe('words', () => {
  it('turns a closed-vocabulary word into a sentence, and an unknown word into a generic one, never the word', () => {
    expect(jobStatusText('succeeded')).toBe('Finished')
    expect(jobStatusText('teleporting')).not.toContain('teleporting')
    expect(resultStateText('missing_field')).toBe('Missing a value')
    expect(resultStateText('something_new')).toBe(t('common.resultState.other'))
    expect(malformedReasonText('no_at')).toBe(t('common.malformed.no_at'))
    expect(malformedReasonText('a_new_reason')).toBe(t('common.malformed.other'))
    expect(resultReasonText('no_mx')).toBe(t('common.reason.no_mx'))
    expect(resultReasonText('a_new_reason')).toBe(t('common.reason.other'))
  })

  it('reads the address reason and the field issues of the new routes, and never shows a word it does not know', () => {
    const address = apiError('validation_failed', { reason: 'the address is not valid', address_reason: 'no_at' }, 400)
    expect(addressReasonOf(address)).toBe('no_at')
    expect(malformedReasonText(addressReasonOf(address) ?? '')).toBe('There is no @ sign.')
    expect(addressReasonOf(apiError('validation_failed', {}, 400))).toBeNull()
    const account = apiError('validation_failed', { reason: 'the account is not valid', fields: ['smtp_host:required', 'daily_limit:out_of_range', 'weird', 7] }, 400)
    expect(fieldIssuesOf(account)).toEqual([
      { field: 'smtp_host', issue: 'required' },
      { field: 'daily_limit', issue: 'out_of_range' },
      { field: 'weird', issue: 'malformed' },
    ])
    expect(errorText(account)).toBe(t('error.validation_failed.account'))
    expect(fieldIssueText('required')).toBe('This is required.')
    expect(fieldIssueText('a_new_issue')).toBe(t('error.field.malformed'))
    expect(fieldIssuesOf(apiError('busy', undefined, 503))).toEqual([])
  })

  it('has a line for every refusal one line of a bulk add can carry, and the module 3 codes read as they should', () => {
    for (const code of M3_BULK_ERROR_CODES) expect(maybeT(`error.${code}`), code).not.toBeNull()
    expect(errorText(apiError('rate_limited', undefined, 429))).toBe(t('error.rate_limited'))
    expect(errorText(apiError('from_gate_ack_required', { from_gate: 'unaligned_warned' }, 422))).toBe(t('error.from_gate_ack_required'))
    expect(errorText(apiError('account_outcome_unknown', undefined, 503))).toContain('list of accounts')
  })

  it('has no sentence for a missing_field reason (it lists the user\'s own field names) and none for an empty reason', () => {
    expect(resultReasonText('first_name,plan', 'missing_field')).toBeNull()
    expect(resultReasonText(null)).toBeNull()
    expect(resultReasonText('')).toBeNull()
    expect(missingFieldNames('first_name, plan')).toEqual(['first_name', 'plan'])
    expect(missingFieldNames(null)).toEqual([])
  })
})
